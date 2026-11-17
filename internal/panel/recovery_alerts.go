package panel

import (
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/recoverykit"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// The alerts of the panel's backup and recovery (#32): recovery_kit while no
// checked kit is current, panel_backup while the panel is not backed up,
// and panel_restored after a restore (recover.go). Each has the target
// "panel" and no host, as the audit log's integrity alert. Nothing is raised
// before the first server: a panel without servers loses little with its
// machine.

// recoveryDebounce is how long after a change the alerts are looked at
// again; a test shortens it.
var recoveryDebounce = 5 * time.Second

const (
	// kitCriticalAfter is how long recovery_kit stays a warning.
	kitCriticalAfter = 7 * 24 * time.Hour
	// panelBackupMaxAge is how old the panel job's last success may be.
	panelBackupMaxAge = 48 * time.Hour
)

// registerRecovery binds what keeps the recovery's alerts current.
func (p *Panel) registerRecovery() {
	p.watchRestore()

	changed := func(e *core.RecordEvent) error {
		p.recoveryChanged()
		return e.Next()
	}
	for _, name := range []string{schema.BackupJobs, schema.BackupTargets, schema.RecoveryKits} {
		p.App.OnRecordAfterCreateSuccess(name).BindFunc(changed)
		p.App.OnRecordAfterUpdateSuccess(name).BindFunc(changed)
		p.App.OnRecordAfterDeleteSuccess(name).BindFunc(changed)
	}
	p.App.OnRecordAfterCreateSuccess(schema.Hosts).BindFunc(changed)
	p.App.OnRecordAfterDeleteSuccess(schema.Hosts).BindFunc(changed)
	p.App.OnRecordAfterUpdateSuccess(schema.Hosts).BindFunc(func(e *core.RecordEvent) error {
		if original := e.Record.Original(); original != nil && original.GetString("panel_address") != e.Record.GetString("panel_address") {
			p.recoveryChanged()
		}
		return e.Next()
	})

	p.App.Cron().MustAdd("pocketcloud_recovery", "19 * * * *", p.evaluateRecoveryAlerts)
}

// recoveryChanged looks at the alerts again a moment after a change, once
// for changes that come together.
func (p *Panel) recoveryChanged() {
	// before the start, which looks at them anyway, there is nothing to do
	if p.authority.Load() == nil {
		return
	}
	if p.recoveryPending.CompareAndSwap(false, true) {
		p.later.after(recoveryDebounce, func() {
			p.recoveryPending.Store(false)
			p.evaluateRecoveryAlerts()
		})
	}
}

// evaluateRecoveryAlerts raises or resolves the recovery's alerts.
func (p *Panel) evaluateRecoveryAlerts() {
	if p.authority.Load() == nil {
		// not started: nothing to compare a kit with
		return
	}
	p.recoveryEval.Lock()
	defer p.recoveryEval.Unlock()

	hosts, err := p.App.CountRecords(schema.Hosts)
	if err != nil {
		p.Log.Error("failed to count the servers", "error", err)
		return
	}
	if hosts == 0 {
		p.resolvePanelAlert(backup.AlertRecoveryKit)
		p.resolvePanelAlert(backup.AlertPanelBackup)
	} else {
		p.evaluateKitAlert()
		p.evaluatePanelBackupAlert()
	}
	p.evaluatePanelRestored(false)
}

func (p *Panel) resolvePanelAlert(rule string) {
	if err := events.ResolveAlert(p.App, rule, backup.PanelAlertTarget); err != nil {
		p.Log.Error("failed to resolve an alert", "rule", rule, "error", err)
	}
}

// raisePanelAlert raises an alert of the panel, a warning that becomes
// critical once it was open for criticalAfter, when that is not zero.
func (p *Panel) raisePanelAlert(rule, message string, criticalAfter time.Duration) {
	severity := schema.SeverityWarning
	if criticalAfter > 0 {
		open, err := p.App.FindFirstRecordByFilter(schema.Alerts, "rule = {:rule} && target = {:target} && status != {:resolved}",
			dbx.Params{"rule": rule, "target": backup.PanelAlertTarget, "resolved": schema.AlertResolved})
		if err == nil && time.Since(open.GetDateTime("first_seen").Time()) >= criticalAfter {
			severity = schema.SeverityCritical
		}
	}
	if err := events.RaiseAlert(p.App, rule, backup.PanelAlertTarget, "", severity, message); err != nil {
		p.Log.Error("failed to raise an alert", "rule", rule, "error", err)
	}
}

// kitReasons says in words what changed since a kit was made.
var kitReasons = map[string]string{
	recoverykit.SectionKey:     "the master key",
	recoverykit.SectionAddress: "the panel's addresses",
	recoverykit.SectionJob:     "the panel's backup job",
	recoverykit.SectionTargets: "a target of the panel's backup",
}

// evaluateKitAlert keeps recovery_kit open while no checked kit is current.
func (p *Panel) evaluateKitAlert() {
	current, err := p.currentDigests()
	if err != nil {
		p.Log.Error("failed to read the panel's state for its recovery kit", "error", err)
		return
	}
	status, err := p.kitStatus(current)
	if err != nil {
		p.Log.Error("failed to read the recovery kits", "error", err)
		return
	}
	var message string
	switch status.Status {
	case kitSaved:
		p.resolvePanelAlert(backup.AlertRecoveryKit)
		return
	case kitNever:
		message = "No recovery kit was downloaded: without its master key the panel's backups open nowhere. Download one in Settings › Recovery kit."
	case kitUnverified:
		message = fmt.Sprintf("The recovery kit %s was downloaded but not checked: check it in Settings › Recovery kit.", status.KitID)
	default:
		words := make([]string, 0, len(status.Reasons))
		for _, reason := range status.Reasons {
			words = append(words, kitReasons[reason])
		}
		message = fmt.Sprintf("The recovery kit is out of date, as %s changed: download a new one and check it.", strings.Join(words, ", "))
	}
	p.raisePanelAlert(backup.AlertRecoveryKit, message, kitCriticalAfter)
}

// evaluatePanelBackupAlert keeps panel_backup open while the panel is not
// backed up: no job, a job turned off or without targets, or no success in
// 48 hours of a job older than that.
func (p *Panel) evaluatePanelBackupAlert() {
	job, err := p.panelJob()
	if err != nil {
		p.Log.Error("failed to read the panel's backup job", "error", err)
		return
	}
	message := ""
	switch {
	case job == nil:
		message = "The panel itself is not backed up: losing its machine would mean enrolling every server again. Back it up to a target off that machine."
	case !job.GetBool("enabled"):
		message = fmt.Sprintf("The panel's backup job %s is turned off.", job.GetString("name"))
	case len(job.GetStringSlice("targets")) == 0:
		message = fmt.Sprintf("The panel's backup job %s has no target.", job.GetString("name"))
	case time.Since(job.GetDateTime("created").Time()) > panelBackupMaxAge:
		last, err := p.lastPanelSuccess(job)
		if err != nil {
			p.Log.Error("failed to read the panel's backup runs", "error", err)
			return
		}
		if last.IsZero() {
			message = "The panel's backup has not succeeded once in 48 hours."
		} else if time.Since(last.Time()) > panelBackupMaxAge {
			message = fmt.Sprintf("The panel's backup has not succeeded for more than 48 hours: the last success was %s.", utcMinute(last.Time()))
		}
	}
	if message == "" {
		p.resolvePanelAlert(backup.AlertPanelBackup)
		return
	}
	p.raisePanelAlert(backup.AlertPanelBackup, message, 0)
}
