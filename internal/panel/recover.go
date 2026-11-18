package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/security"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// The first start of a panel that `panel recover` restored (#32). The
// restored database is as the backup left it: commands queued then, servers
// online then, the sessions of then. Before the gateway listens the start
// cancels what was queued, marks every server offline without an alert, so
// that each reconnect counts as a return, ends every session from before,
// and writes a sealed `panel_restored` entry where the restored log
// resumes. Dispatched and running commands are left to the agents' reports
// of what they finished, the replay and the reaper: the run that made the
// snapshot ends as it did.

// restoreMarkerName is the file `panel recover` leaves in the data
// directory for the first start.
const restoreMarkerName = ".pc_restored.json"

// restoreMarker is what `panel recover` restored, as its marker says.
type restoreMarker struct {
	ExportID           string `json:"export_id"`
	ExportCreated      string `json:"export_created"`
	PanelVersionExport string `json:"panel_version_export"`
	Migration          string `json:"migration"`
	AuditSeq           int64  `json:"audit_seq"`
	AuditHash          string `json:"audit_hash"`
	Snapshot           string `json:"snapshot"`
	SnapshotTime       string `json:"snapshot_time"`
	Target             string `json:"target"`
	KitID              string `json:"kit_id"`
	RecoveredAt        string `json:"recovered_at"`
	ToolVersion        string `json:"tool_version"`
}

// made is when the backup was made, as people read it.
func (m restoreMarker) made() string {
	created, err := time.Parse(time.RFC3339, m.ExportCreated)
	if err != nil {
		return m.ExportCreated
	}
	return utcMinute(created)
}

// utcMinute is a time to the minute in UTC, as the recovery's sentences say
// it: 2026-11-17 04:00 UTC.
func utcMinute(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04") + " UTC"
}

// details is the marker as the audit entry's details.
func (m restoreMarker) details() map[string]any {
	raw, _ := json.Marshal(m)
	details := map[string]any{}
	_ = json.Unmarshal(raw, &details)
	return details
}

// unknownMigrations lists, in order, the migrations a database applied that
// this panel does not know: those of a newer panel, which PocketBase would
// skip without a word.
func unknownMigrations(db dbx.Builder) ([]string, error) {
	var applied []struct {
		File string `db:"file"`
	}
	if err := db.NewQuery("SELECT [[file]] FROM {{" + core.DefaultMigrationsTable + "}} ORDER BY [[file]]").All(&applied); err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, list := range []core.MigrationsList{core.SystemMigrations, core.AppMigrations} {
		for _, migration := range list.Items() {
			known[migration.File] = true
		}
	}
	var unknown []string
	for _, row := range applied {
		if !known[row.File] {
			unknown = append(unknown, row.File)
		}
	}
	return unknown, nil
}

// restoredReason is why the first start after a restore cancels what was
// queued.
func restoredReason(made string) string {
	return fmt.Sprintf("The panel was restored from a backup made %s: what was queued then is not sent. Queue it again if it is still wanted.", made)
}

// finishRestore is the first start's part of a restore, when `panel
// recover` left its marker. A start after it, from the same export, only
// removes the marker. A database of a newer panel keeps the marker and
// stops the start.
func (p *Panel) finishRestore() error {
	path := filepath.Join(p.App.DataDir(), restoreMarkerName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("the restore's marker: %w", err)
	}
	var marker restoreMarker
	if err := json.Unmarshal(raw, &marker); err != nil {
		return fmt.Errorf("the restore's marker %s cannot be read: %w", path, err)
	}
	if marker.ExportID == "" {
		return fmt.Errorf("the restore's marker %s names no export", path)
	}

	state, err := readPanelState(p.App.DB())
	if err != nil {
		return fmt.Errorf("reading %s: %w", schema.PanelTable, err)
	}
	var done restoreMarker
	_ = json.Unmarshal([]byte(state.RestoredFrom), &done)
	if done.ExportID == marker.ExportID {
		return removeMarker(path)
	}

	unknown, err := unknownMigrations(p.App.DB())
	if err != nil {
		return err
	}
	if len(unknown) > 0 {
		newer := marker.PanelVersionExport
		if newer == "" {
			newer = unknown[len(unknown)-1]
		}
		return refusalf("The data comes from a newer panel (%s): start that version or a newer one.", newer)
	}

	made := marker.made()
	cancelled, err := p.cancelQueued(restoredReason(made))
	if err != nil {
		return err
	}
	// offline without an event or an alert: a reconnect is a return
	if _, err := p.App.DB().Update(schema.Hosts, dbx.Params{"status": schema.HostOffline, "stream_open": false}, nil).Execute(); err != nil {
		return fmt.Errorf("marking the servers offline: %w", err)
	}
	if err := p.endSessions(); err != nil {
		return err
	}

	details := marker.details()
	details["entries_after_lost"] = true
	details["sessions_ended"] = true
	details["queued_cancelled"] = cancelled
	// written before the restore is recorded as finished: a start that
	// stops in between writes it again rather than never
	p.Audit.Security(audit.ActionPanelRestored, audit.System("panel recover", audit.SourceCommand), made, details)

	from, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	if err := setPanelState(p.App.DB(), dbx.Params{"restored_at": types.NowDateTime().String(), "restored_from": string(from)}); err != nil {
		return fmt.Errorf("recording the restore: %w", err)
	}

	p.evaluatePanelRestored(true)
	p.Log.Warn("the panel was restored from a backup", "made", made, "export", marker.ExportID, "kit", marker.KitID,
		"audit_seq", marker.AuditSeq, "queued_cancelled", cancelled)
	return removeMarker(path)
}

func removeMarker(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the restore's marker: %w", err)
	}
	return nil
}

// cancelQueued cancels every queued command, and says how many.
func (p *Panel) cancelQueued(reason string) (int, error) {
	queued, err := p.App.FindAllRecords(schema.Commands, dbx.HashExp{"status": schema.CommandQueued})
	if err != nil {
		return 0, err
	}
	cancelled := 0
	for _, command := range queued {
		if err := p.Commands.Cancel(command.Id, reason); err != nil {
			p.Log.Error("a queued command of before the restore was not cancelled", "command", command.Id, "error", err)
			continue
		}
		cancelled++
	}
	return cancelled, nil
}

// endSessions gives the users' and superusers' tokens new secrets: every
// session, and every link to reset a password, change an email, verify an
// account or read a file, from before the disaster stops working.
func (p *Panel) endSessions() error {
	ctx := audit.WithActor(context.Background(), audit.System("panel recover", audit.SourceCommand))
	for _, name := range []string{schema.Users, schema.Superusers} {
		collection, err := p.App.FindCollectionByNameOrId(name)
		if err != nil {
			return err
		}
		for _, token := range []*core.TokenConfig{
			&collection.AuthToken, &collection.PasswordResetToken, &collection.EmailChangeToken,
			&collection.VerificationToken, &collection.FileToken,
		} {
			token.Secret = security.RandomString(50)
		}
		if err := p.App.SaveWithContext(ctx, collection); err != nil {
			return fmt.Errorf("ending the sessions of %s: %w", name, err)
		}
	}
	return nil
}

// serversBack counts the servers connected since a time, and all of them.
func (p *Panel) serversBack(since string) (back, total int, err error) {
	all, err := p.App.CountRecords(schema.Hosts)
	if err != nil {
		return 0, 0, err
	}
	connected, err := p.App.CountRecords(schema.Hosts, dbx.NewExp("[[connected_since]] > {:since}", dbx.Params{"since": since}))
	return int(connected), int(all), err
}

// evaluatePanelRestored keeps the panel_restored alert: raised by the first
// start after a restore, refreshed with the servers back, and resolved once
// every server connected since or was removed.
func (p *Panel) evaluatePanelRestored(raise bool) {
	state, err := readPanelState(p.App.DB())
	if err != nil || state.RestoredAt == "" {
		return
	}
	back, total, err := p.serversBack(state.RestoredAt)
	if err != nil {
		p.Log.Error("failed to count the servers back after the restore", "error", err)
		return
	}
	if back >= total {
		p.restoreWatch.Store(false)
		if err := events.ResolveAlert(p.App, backup.AlertPanelRestored, backup.PanelAlertTarget); err != nil {
			p.Log.Error("failed to resolve an alert", "rule", backup.AlertPanelRestored, "error", err)
		}
		return
	}
	if !raise && !alertOpen(p.App, backup.AlertPanelRestored, backup.PanelAlertTarget) {
		return
	}
	// the servers are watched as they come back while the alert is open,
	// also after a restart: the watch is kept in memory, the alert is not
	p.restoreWatch.Store(true)
	var from restoreMarker
	_ = json.Unmarshal([]byte(state.RestoredFrom), &from)
	message := fmt.Sprintf("The panel was restored from a backup made %s: %d of %d servers are back.", from.made(), back, total)
	if err := events.RaiseAlert(p.App, backup.AlertPanelRestored, backup.PanelAlertTarget, "", schema.SeverityWarning, message); err != nil {
		p.Log.Error("failed to raise an alert", "rule", backup.AlertPanelRestored, "error", err)
	}
}

// watchRestore follows the servers coming back after a restore: a change of
// a server, or its removal, counts them again a moment later.
func (p *Panel) watchRestore() {
	changed := func(e *core.RecordEvent) error {
		if p.restoreWatch.Load() && p.restoreRecount.CompareAndSwap(false, true) {
			p.later.after(time.Second, func() {
				p.restoreRecount.Store(false)
				p.evaluatePanelRestored(false)
			})
		}
		return e.Next()
	}
	p.App.OnRecordAfterUpdateSuccess(schema.Hosts).BindFunc(changed)
	p.App.OnRecordAfterDeleteSuccess(schema.Hosts).BindFunc(changed)
}

// alertOpen says whether an alert of a rule and target is open.
func alertOpen(app core.App, rule, target string) bool {
	open, err := app.CountRecords(schema.Alerts, dbx.HashExp{"rule": rule, "target": target},
		dbx.Not(dbx.HashExp{"status": schema.AlertResolved}))
	return err == nil && open > 0
}
