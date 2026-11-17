package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/stacks"
)

// Integrity checks (#31) read every target's repository with restic check, on
// one schedule for the whole panel that admins set in the settings: a
// backup.check for the agent of the target's own server, which reads one
// group of the repository's data packs without a lock, and reads it again
// under restic's exclusive lock only when it found errors (see
// docs/architecture.md). Each check is a record of backup_checks; the target
// keeps a summary of its checks, and the group its next check reads, which
// moves on only after a check found nothing.
//
// A check that did not complete, or did not start because its server was
// offline, is tried again after 1, 3, 6, 12 and 24 hours, unless the next
// scheduled check comes first. Errors that the second pass confirmed raise
// the critical alert backup_check_failed; a check that could not tell, or did
// not complete, raises the warning backup_check_unverified, as does a target
// that no check has verified for twice the time between two checks and a
// day. A check that found nothing resolves both.

const (
	// checkQueue is how long a check may wait for its server, and for the
	// check of the target before it.
	checkQueue = 6 * time.Hour
	// keepBackupChecks is how long checks stay as a target's history; a
	// target's newest check, and its newest that completed, stay whatever
	// their age.
	keepBackupChecks = 180 * 24 * time.Hour
	// maxCheckAttempts is the attempt after which a check is not tried
	// again.
	maxCheckAttempts = 6
	// minCheckGap is the least time between two checks of the schedule:
	// each reads a part of every repository, which storage that bills
	// downloads bills.
	minCheckGap = 23 * time.Hour
	// checkGapSlots is how many slots of a schedule are looked at for the
	// least time between two of them.
	checkGapSlots = 14
	// checkRetryBatch is how many due retries of checks a pass starts.
	checkRetryBatch = 50
)

// checkBackoff is how long after a check that did not complete it is tried
// again, by attempt.
var checkBackoff = []time.Duration{time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour}

// Rules of the alerts of integrity checks, each keyed by the target's id.
const (
	alertCheckFailed     = "backup_check_failed"
	alertCheckUnverified = "backup_check_unverified"
)

// The most the JSON fields of a check hold (see the migration).
const (
	maxCheckErrorsJSON = 16 << 10
	maxCheckSmallJSON  = 4 << 10
)

// The answers about the schedule of the checks.
const (
	checkScheduleProblem = "The schedule is cron in UTC, such as 0 5 * * 0."
	checkNeverProblem    = "The schedule never comes within a year: name a minute that comes, such as 0 5 * * 0."
	checkOftenProblem    = "A check runs at most once a day."
	checkGroupsProblem   = "Groups is a whole number from 0 to 256."
)

var (
	// errCheckTaken says the slot of the schedule, or the retry, has its
	// check already.
	errCheckTaken = errors.New("the check has started already")
	// errCheckOpen says a check of the target is queued or running.
	errCheckOpen = errors.New("a check of the target is under way")
)

func (p *Panel) registerBackupChecks() {
	p.Commands.OnStart("backup.check", p.backupCheckStarted)
	p.Commands.OnFinish("backup.check", p.backupCheckFinished)
	p.App.Cron().MustAdd("pocketcloud_backup_checks", "* * * * *", func() { p.RunDueChecks(time.Now()) })
	p.App.Cron().MustAdd("pocketcloud_backup_checks_watch", "41 * * * *", func() { p.WatchChecks(time.Now()) })
	p.App.Cron().MustAdd("pocketcloud_backup_checks_retention", "45 4 * * *", p.pruneBackupChecks)

	// nothing would resolve the alerts of a target that is gone
	p.App.OnRecordAfterDeleteSuccess(schema.BackupTargets).BindFunc(func(e *core.RecordEvent) error {
		p.resolveCheckAlerts(e.App, e.Record.Id, alertCheckFailed, alertCheckUnverified)
		return e.Next()
	})
	// the record API holds the schedule and the groups to the rules of the
	// route, which admins may bypass through it
	p.App.OnRecordCreateRequest(schema.Settings).BindFunc(p.checkSettingsRequest)
	p.App.OnRecordUpdateRequest(schema.Settings).BindFunc(p.checkSettingsRequest)
}

func (p *Panel) bindBackupCheckRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	admins := requireRole(schema.RoleAdmin)
	group.POST("/backup-targets/{id}/check", p.handleCheckTarget).BindFunc(admins)
	group.PUT("/settings/backup-checks", p.handleCheckSettings).BindFunc(admins)
}

// checkSettings reads when the checks run, "" when they are off or the
// settings say nothing that runs, and in how many groups a check reads a
// repository's data packs, 0 for the structure only.
func (p *Panel) checkSettings() (schedule string, groups int) {
	settings, err := p.settingsRecord()
	if err != nil {
		return "", 0
	}
	schedule = stacks.NormalSchedule(settings.GetString("backup_check_schedule"))
	if parseCronRule(schedule) == nil {
		schedule = ""
	}
	return schedule, min(max(settings.GetInt("backup_check_groups"), 0), backup.MaxGroups)
}

// checkScheduleText checks the schedule of the checks: cron in UTC that comes
// within a year, at most once a day; "" turns the checks off. It returns what
// is wrong, as a sentence.
func checkScheduleText(schedule string, now time.Time) string {
	if schedule == "" {
		return ""
	}
	rule := parseCronRule(schedule)
	if !stacks.ValidSchedule(schedule) || rule == nil {
		return checkScheduleProblem
	}
	at := rule.next(now)
	if at.IsZero() {
		return checkNeverProblem
	}
	// not only the next two: 0 0,23 * * * is 23 hours, then one
	for range checkGapSlots {
		next := rule.next(at)
		if next.IsZero() {
			break
		}
		if next.Sub(at) < minCheckGap {
			return checkOftenProblem
		}
		at = next
	}
	return ""
}

// checkGroupsText checks the groups of the checks: a whole number from 0 to
// backup.MaxGroups.
func checkGroupsText(groups float64) string {
	if groups != math.Trunc(groups) || groups < 0 || groups > backup.MaxGroups {
		return checkGroupsProblem
	}
	return ""
}

// checkSettingsRequest holds a change of the settings through the record API
// to the rules of the checks' route.
func (p *Panel) checkSettingsRequest(e *core.RecordRequestEvent) error {
	record := e.Record
	before := record.Original()
	schedule := stacks.NormalSchedule(record.GetString("backup_check_schedule"))
	if record.IsNew() || schedule != stacks.NormalSchedule(before.GetString("backup_check_schedule")) {
		if problem := checkScheduleText(schedule, time.Now()); problem != "" {
			return e.BadRequestError(problem, validation.Errors{"backup_check_schedule": validation.NewError("validation_invalid_schedule", problem)})
		}
		record.Set("backup_check_schedule", schedule)
	}
	if record.IsNew() || record.GetFloat("backup_check_groups") != before.GetFloat("backup_check_groups") {
		if problem := checkGroupsText(record.GetFloat("backup_check_groups")); problem != "" {
			return e.BadRequestError(problem, validation.Errors{"backup_check_groups": validation.NewError("validation_invalid_groups", problem)})
		}
	}
	if err := e.Next(); err != nil {
		return err
	}
	p.checkSettingsChanged(before, record)
	return nil
}

// checkSettingsChanged follows a change of the checks' settings that a
// request saved: each target's next check, and when the checks were turned
// off, what only checks would resolve. A migration's save of the settings
// changes nothing else.
func (p *Panel) checkSettingsChanged(before, after *core.Record) {
	schedule := stacks.NormalSchedule(after.GetString("backup_check_schedule"))
	was := stacks.NormalSchedule(before.GetString("backup_check_schedule"))
	if schedule != was || after.GetInt("backup_check_groups") != before.GetInt("backup_check_groups") {
		p.refreshNextChecks(time.Now())
	}
	if schedule == "" && was != "" {
		p.checksTurnedOff()
	}
}

// handleCheckSettings sets when the checks run and how much of a repository
// one reads: a field left out stays, and an empty schedule turns the checks
// off, which resolves the alerts that say a target is not verified.
func (p *Panel) handleCheckSettings(e *core.RequestEvent) error {
	var body struct {
		Schedule *string  `json:"schedule"`
		Groups   *float64 `json:"groups"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}
	settings, err := p.settingsRecord()
	if err != nil {
		return e.InternalServerError("", err)
	}
	before := settings.Fresh()
	now := time.Now()
	if body.Schedule != nil {
		schedule := stacks.NormalSchedule(*body.Schedule)
		if problem := checkScheduleText(schedule, now); problem != "" {
			return e.BadRequestError(problem, nil)
		}
		settings.Set("backup_check_schedule", schedule)
	}
	if body.Groups != nil {
		if problem := checkGroupsText(*body.Groups); problem != "" {
			return e.BadRequestError(problem, nil)
		}
		settings.Set("backup_check_groups", int(*body.Groups))
	}
	if err := e.App.SaveWithContext(audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e)), settings); err != nil {
		return e.BadRequestError("The checks could not be saved: "+saveErrorText(err), nil)
	}
	p.checkSettingsChanged(before, settings)
	schedule, groups := p.checkSettings()
	next := ""
	if at := parseCronRule(schedule).next(now); !at.IsZero() {
		next = at.UTC().Format(time.RFC3339)
	}
	return e.JSON(http.StatusOK, map[string]any{"schedule": schedule, "groups": groups, "next_check_at": next})
}

// checksTurnedOff resolves what only checks would resolve: the alerts that a
// target is not verified, and the retries of checks.
func (p *Panel) checksTurnedOff() {
	p.resolveCheckAlerts(p.App, "", alertCheckUnverified)
	planned, err := p.App.FindRecordsByFilter(schema.BackupChecks, "retry_at!=''", "", 0, 0)
	if err != nil {
		p.Log.Error("failed to read the integrity checks", "error", err)
		return
	}
	for _, check := range planned {
		p.setFields(check, map[string]any{"retry_at": ""})
	}
}

// resolveCheckAlerts resolves the open alerts of a target's checks with the
// rules, or every target's when target is "".
func (p *Panel) resolveCheckAlerts(app core.App, target string, rules ...string) {
	alerts := []*core.Record{}
	query := app.RecordQuery(schema.Alerts).
		AndWhere(dbx.In("rule", toAny(rules)...)).
		AndWhere(dbx.Not(dbx.HashExp{"status": schema.AlertResolved}))
	if target != "" {
		query = query.AndWhere(dbx.HashExp{"target": target})
	}
	if err := query.All(&alerts); err != nil {
		p.Log.Error("failed to read the alerts of integrity checks", "error", err)
		return
	}
	for _, alert := range alerts {
		if err := events.ResolveAlert(app, alert.GetString("rule"), alert.GetString("target")); err != nil {
			p.Log.Error("failed to resolve an alert", "rule", alert.GetString("rule"), "error", err)
		}
	}
}

// toAny makes a list of strings the values of dbx.In.
func toAny(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

// RunDueChecks starts the checks of the targets whose slot of the schedule has
// come, in UTC: the panel calls it every minute. A slot the panel missed while
// it was down, less than a day ago, is caught up once. Then the retries that
// are due start, and each target's next check is set.
func (p *Panel) RunDueChecks(now time.Time) {
	p.checkPass.Lock()
	defer p.checkPass.Unlock()
	minute := now.UTC().Truncate(time.Minute)
	schedule, _ := p.checkSettings()
	if schedule == "" {
		p.refreshNextChecks(now)
		return
	}
	targets, err := p.App.FindAllRecords(schema.BackupTargets)
	if err != nil {
		p.Log.Error("failed to read the backup targets", "error", err)
		return
	}
	since := minute.Add(-catchUpWithin)
	for _, target := range targets {
		slot := target.GetDateTime("next_check_at").Time()
		if !checkable(target) || slot.IsZero() || slot.After(minute) || !slot.After(since) {
			continue
		}
		_, err := p.startCheck(target, checkStart{trigger: schema.RunSchedule, scheduledAt: slot, requester: "schedule", now: now})
		if err != nil && !errors.Is(err, errCheckTaken) {
			p.Log.Error("failed to start an integrity check", "target", target.Id, "error", err)
		}
	}
	p.retryDueChecks(minute)
	p.refreshNextChecks(now)
}

// checkable tells whether a target has a repository to check: one still being
// added has none yet.
func checkable(target *core.Record) bool {
	return target.GetString("status") != schema.TargetPending && target.GetString("repo_id") != ""
}

// refreshNextChecks sets each target's next check, as it is after now; none
// while the checks are off.
func (p *Panel) refreshNextChecks(now time.Time) {
	schedule, _ := p.checkSettings()
	var want any = ""
	if at := parseCronRule(schedule).next(now); !at.IsZero() {
		want = at
	}
	targets, err := p.App.FindAllRecords(schema.BackupTargets)
	if err != nil {
		return
	}
	for _, target := range targets {
		if sameDate(target.GetDateTime("next_check_at"), want) {
			continue
		}
		err := p.update(context.Background(), target, func(fresh *core.Record) error {
			if sameDate(fresh.GetDateTime("next_check_at"), want) {
				return errUnchanged
			}
			fresh.Set("next_check_at", want)
			return nil
		})
		if err != nil {
			p.Log.Error("failed to store the next integrity check of a target", "target", target.Id, "error", err)
		}
	}
}

// checkStart says how a check starts.
type checkStart struct {
	trigger string
	// scheduledAt is the slot of the schedule, for the first attempt of a
	// scheduled check
	scheduledAt time.Time
	// prev is the check this one tries again
	prev *core.Record
	// requestedBy and requester are who asked for it
	requestedBy, requester string
	// refuse answers what keeps the check from starting as a *targetProblem,
	// for the route, instead of keeping a skipped check
	refuse bool
	// now is the scheduler's pass; zero is now
	now time.Time
}

// checkBlock is what keeps a check from starting.
type checkBlock struct {
	status string
	// reason is the check's error; refusal is the route's answer
	reason, refusal string
	// alert raises backup_check_unverified, as for an agent too old to
	// check; retry tries the check again with the backoff, as for a server
	// that was offline
	alert, retry bool
}

// checkBlocked says what keeps a check of a target from starting now on its
// server, if anything: the server is offline, or its agent cannot check.
func (p *Panel) checkBlocked(host *core.Record, at time.Time, refuse bool) *checkBlock {
	name := host.GetString("name")
	if (refuse && !p.Commands.Connected(host.Id)) || (!refuse && p.offline(host)) {
		return &checkBlock{status: schema.CheckSkipped, retry: true,
			reason:  fmt.Sprintf("The server %s was offline at %s UTC.", name, at.UTC().Format("15:04")),
			refusal: fmt.Sprintf("The server %s is offline: its agent checks the target.", name)}
	}
	if capabilities := host.GetStringSlice("capabilities"); len(capabilities) > 0 && !slices.Contains(capabilities, "backup.check") {
		text := fmt.Sprintf("The agent of %s cannot check repositories yet: update it.", name)
		return &checkBlock{status: schema.CheckSkipped, alert: true, reason: text, refusal: text}
	}
	return nil
}

// checkOpenIn tells whether a check of a target is queued or running, as the
// app (a transaction's) sees it.
func checkOpenIn(app core.App, targetID string) bool {
	open, err := app.FindRecordsByFilter(schema.Commands,
		"type='backup.check' && resource={:resource} && (status={:queued} || status={:dispatched} || status={:running})", "", 1, 0,
		dbx.Params{"resource": checkResource(targetID), "queued": schema.CommandQueued, "dispatched": schema.CommandDispatched, "running": schema.CommandRunning})
	return err != nil || len(open) > 0
}

// checkResource is the ordering key of a target's checks.
func checkResource(targetID string) string {
	return commands.Specs["backup.check"].Resource(&agentv1.Command{Payload: &agentv1.Command_BackupCheck{BackupCheck: &agentv1.BackupCheck{
		Repository: &agentv1.BackupRepository{TargetId: targetID},
	}}})
}

// startCheck starts a check of a target: a queued record and its command, in
// one transaction, or a skipped record when its server is offline, its agent
// cannot check, or the check before it is still going. A retry clears the
// retry of the check it tries again in the same transaction, and a slot of the
// schedule gets one check: a second start of either is errCheckTaken.
func (p *Panel) startCheck(target *core.Record, start checkStart) (*core.Record, error) {
	// as it is stored now: its group moves on as its checks end
	target, err := p.App.FindRecordById(schema.BackupTargets, target.Id)
	if err != nil {
		return nil, err
	}
	host, err := p.App.FindRecordById(schema.Hosts, target.GetString("host"))
	if err != nil {
		return nil, fmt.Errorf("the server of the target is gone: %w", err)
	}
	now := start.now.UTC()
	if start.now.IsZero() {
		now = time.Now().UTC()
	}
	at := now
	if !start.scheduledAt.IsZero() {
		at = start.scheduledAt
	}
	schedule, groups := p.checkSettings()
	block := p.checkBlocked(host, at, start.refuse)
	if block != nil && start.refuse {
		return nil, &targetProblem{http.StatusConflict, block.refusal}
	}

	collection, err := p.App.FindCachedCollectionByNameOrId(schema.BackupChecks)
	if err != nil {
		return nil, err
	}
	attempt := 1
	if start.prev != nil {
		attempt = max(start.prev.GetInt("attempt"), 1) + 1
	}
	subset := backup.Subset(target.GetInt("integrity_group"), groups)
	var options commands.Options
	if block == nil {
		secrets, err := p.targetSecretsOf(target)
		if err != nil {
			p.secretsMissing(target, err)
			block = &checkBlock{status: schema.CheckError, alert: true, reason: secretsMissingMessage, refusal: secretsMissingMessage}
		} else if repo, err := backup.Locate(secrets.kind, secrets.config, secrets.creds); err != nil {
			text := sentence(err)
			block = &checkBlock{status: schema.CheckError, alert: true, reason: text, refusal: text}
		} else {
			options = commands.Options{
				ID:   core.GenerateDefaultRandomId(),
				Host: host.Id,
				// the place, never a secret: the enricher adds those
				Command: &agentv1.Command{Payload: &agentv1.Command_BackupCheck{BackupCheck: &agentv1.BackupCheck{
					Repository:     &agentv1.BackupRepository{TargetId: target.Id, Url: repo.URL},
					ReadDataSubset: subset,
				}}},
				Label:        cutText("Check the integrity of "+target.GetString("name"), 255),
				QueueTimeout: checkQueue,
				RequestedBy:  start.requestedBy,
				Requester:    start.requester,
			}
		}
		if block != nil && start.refuse {
			return nil, &targetProblem{http.StatusConflict, block.refusal}
		}
	}

	var row *core.Record
	var ended *checkBlock
	err = p.App.RunInTransaction(func(tx core.App) error {
		row = core.NewRecord(collection)
		row.Load(map[string]any{
			"target": target.Id, "host": host.Id, "kind": schema.CheckIntegrity, "trigger": start.trigger,
			"status": schema.CheckQueued, "subset": subset, "attempt": attempt,
		})
		if !start.scheduledAt.IsZero() {
			row.Set("scheduled_at", start.scheduledAt)
		}
		if start.prev != nil {
			prev, err := tx.FindRecordById(schema.BackupChecks, start.prev.Id)
			if err != nil || prev.GetDateTime("retry_at").IsZero() {
				return errCheckTaken
			}
			prev.Set("retry_at", "")
			if err := tx.Save(prev); err != nil {
				return err
			}
			row.Set("retry_of", prev.Id)
		}
		ended = block
		if ended == nil && checkOpenIn(tx, target.Id) {
			if start.refuse {
				return errCheckOpen
			}
			ended = &checkBlock{status: schema.CheckSkipped, reason: "The check before it was still going."}
		}
		if ended != nil {
			row.Set("status", ended.status)
			row.Set("error", ended.reason)
			row.Set("finished_at", now)
			if ended.retry {
				if retry := checkRetryAt(row, now, schedule); !retry.IsZero() {
					row.Set("retry_at", retry)
				}
			}
			return tx.Save(row)
		}
		command, err := p.Commands.CreateIn(context.Background(), tx, options)
		if err != nil {
			return err
		}
		row.Set("command", command.Id)
		return tx.Save(row)
	})
	switch {
	case errors.Is(err, errCheckOpen):
		return nil, &targetProblem{http.StatusConflict, fmt.Sprintf("An integrity check of %s is under way.", target.GetString("name"))}
	case uniqueViolation(err):
		return nil, errCheckTaken
	case err != nil:
		return nil, err
	}
	if ended != nil {
		p.checkEnded(target, row, ended.alert)
	} else {
		p.noteTargetCheck(target.Id, nil)
	}
	return row, nil
}

// checkRetryAt is when a check that did not complete, or did not start as its
// server was offline, is tried again: after the backoff of its attempt,
// unless the next scheduled check comes first; zero when it is not tried
// again, as a check someone started by hand.
func checkRetryAt(row *core.Record, finished time.Time, schedule string) time.Time {
	attempt := max(row.GetInt("attempt"), 1)
	if row.GetString("trigger") == schema.RunManual || attempt >= maxCheckAttempts || schedule == "" {
		return time.Time{}
	}
	at := finished.UTC().Add(checkBackoff[attempt-1])
	if next := parseCronRule(schedule).next(finished); !next.IsZero() && !next.After(at) {
		return time.Time{}
	}
	return at
}

// handleCheckTarget checks a target's repository now. The check reads a group
// of its data packs through the agent of its server, without a lock, and
// answers at once: its record and command say how it goes.
func (p *Panel) handleCheckTarget(e *core.RequestEvent) error {
	target, err := e.App.FindRecordById(schema.BackupTargets, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The target does not exist.", err)
	}
	if !checkable(target) {
		return e.Error(http.StatusConflict, fmt.Sprintf("The target %s is not checked yet: it has no repository.", target.GetString("name")), nil)
	}
	requestedBy, requester := requester(e)
	row, err := p.startCheck(target, checkStart{trigger: schema.RunManual, requestedBy: requestedBy, requester: requester, refuse: true})
	var problem *targetProblem
	if errors.As(err, &problem) {
		return e.Error(problem.status, problem.message, nil)
	}
	if err != nil {
		return e.BadRequestError("The check could not be started: "+saveErrorText(err), nil)
	}
	events.Log(p.App, target.GetString("host"), "backup.check_started", schema.SeverityInfo,
		cutText(fmt.Sprintf("%s started the integrity check of %s.", requester, target.GetString("name")), 2048),
		map[string]any{"target": target.Id, "check": row.Id, "command": row.GetString("command")})
	return e.JSON(http.StatusAccepted, map[string]any{"check": row.Id, "command": row.GetString("command")})
}

// checkOfCommand is the check whose command a backup.check is.
func (p *Panel) checkOfCommand(commandID string) (*core.Record, error) {
	return p.App.FindFirstRecordByFilter(schema.BackupChecks, "command={:command}", dbx.Params{"command": commandID})
}

// backupCheckStarted marks a check as running once its agent started it.
func (p *Panel) backupCheckStarted(command *core.Record) {
	row, err := p.checkOfCommand(command.Id)
	if err != nil {
		return
	}
	err = p.update(context.Background(), row, func(fresh *core.Record) error {
		if fresh.GetString("status") != schema.CheckQueued {
			return errUnchanged
		}
		fresh.Set("status", schema.CheckRunning)
		fresh.Set("started_at", command.GetDateTime("started_at"))
		return nil
	})
	if err != nil {
		p.Log.Error("failed to mark an integrity check as running", "check", row.Id, "error", err)
	}
	p.noteTargetCheck(row.GetString("target"), nil)
}

// backupCheckFinished keeps how a check went, from its command and the agent's
// answer, plans its retry, and alerts.
func (p *Panel) backupCheckFinished(command *core.Record) {
	row, err := p.checkOfCommand(command.Id)
	if err != nil {
		return
	}
	if s := row.GetString("status"); s != schema.CheckQueued && s != schema.CheckRunning {
		return
	}
	target, err := p.App.FindRecordById(schema.BackupTargets, row.GetString("target"))
	if err != nil {
		return
	}
	var result backup.CheckResult
	raw, _ := json.Marshal(command.Get("result"))
	_ = json.Unmarshal(raw, &result)

	started := command.GetDateTime("started_at")
	finished := command.GetDateTime("finished_at")
	if finished.IsZero() {
		finished = types.NowDateTime()
	}
	message := command.GetString("error")
	offline := false
	var status string
	switch commandStatus := command.GetString("status"); {
	case commandStatus == schema.CommandSucceeded:
		status, message = schema.CheckOK, ""
	case commandStatus == schema.CommandFailed && result.Status == backup.CheckStatusFailed:
		status, message = schema.CheckFailed, ""
	case commandStatus == schema.CommandFailed && result.Status == backup.CheckStatusSuspect:
		status = schema.CheckSuspect
		if result.Error != "" {
			message = result.Error
		}
	case !started.IsZero() && commandStatus == schema.CommandCancelled:
		status = schema.CheckCancelled
	case started.IsZero() && (commandStatus == schema.CommandTimedOut || commandStatus == schema.CommandCancelled && cancelledByItsTime(command, finished)):
		// it waited its whole time and never started: no check happened
		status = schema.CheckSkipped
		message, offline = p.checkNeverStarted(command)
	case commandStatus == schema.CommandCancelled:
		status = schema.CheckCancelled
	case commandStatus == schema.CommandTimedOut:
		status, message = schema.CheckError, fmt.Sprintf("The check did not finish in %d hours.", int(commands.Specs["backup.check"].Timeout.Hours()))
	default:
		status = schema.CheckError
		if message == "" {
			message = "The check failed."
		}
	}

	// the agent took the secrets out already; whatever it missed
	hide := p.targetSecretValues(target)
	fields := map[string]any{
		"status": status, "finished_at": finished, "error": cutText(capitalize(backup.Redact(message, hide...)), 4096),
		"log": cutTail(backup.Redact(result.Log, hide...), maxRunLog),
	}
	if !started.IsZero() {
		fields["started_at"] = started
		fields["duration_ms"] = finished.Time().Sub(started.Time()).Milliseconds()
	}
	if result.Status != "" {
		checkResultFields(fields, result, hide)
		fields["lock"] = p.describeLock(result.Lock)
	}
	schedule, _ := p.checkSettings()
	retries := status == schema.CheckError || status == schema.CheckSuspect || status == schema.CheckSkipped && offline

	// once: the check's command may be settled from two places
	kept := false
	err = p.update(context.Background(), row, func(fresh *core.Record) error {
		if s := fresh.GetString("status"); s != schema.CheckQueued && s != schema.CheckRunning {
			return errUnchanged
		}
		for name, value := range fields {
			fresh.Set(name, value)
		}
		if retry := checkRetryAt(fresh, finished.Time(), schedule); retries && !retry.IsZero() {
			fresh.Set("retry_at", retry)
		}
		kept = true
		return nil
	})
	if err != nil {
		p.Log.Error("failed to keep an integrity check", "check", row.Id, "error", err)
	}
	if !kept {
		return
	}
	p.checkEnded(target, row, false)
}

// checkResultFields sets what the agent found on a check's fields, redacted,
// and fitted to what the record holds: errors that JSON makes too long, as it
// writes < and > in six bytes each, are counted in errors_more instead.
func checkResultFields(fields map[string]any, result backup.CheckResult, hide []string) {
	found := make([]backup.CheckError, 0, len(result.Errors))
	for _, item := range result.Errors {
		item.Text = cutText(backup.Redact(item.Text, hide...), backup.MaxCheckErrorText)
		found = append(found, item)
	}
	more := result.ErrorsMore
	for len(found) > 0 && jsonSize(found) > maxCheckErrorsJSON {
		found = found[:len(found)-1]
		more++
	}
	notes := make([]string, 0, len(result.Notes))
	for _, note := range result.Notes {
		notes = append(notes, cutText(backup.Redact(note, hide...), backup.MaxCheckErrorText))
	}
	for len(notes) > 0 && jsonSize(notes) > maxCheckSmallJSON {
		notes = notes[:len(notes)-1]
	}
	repair := result.Repair
	if repair.Packs == nil {
		repair.Packs = []string{}
	}
	for len(repair.Packs) > 0 && jsonSize(repair) > maxCheckSmallJSON {
		repair.Packs = repair.Packs[:len(repair.Packs)-1]
	}
	if result.Subset != "" {
		fields["subset"] = cutText(result.Subset, 16)
	}
	fields["packs_read"] = result.PacksRead
	fields["packs_total"] = result.PacksTotal
	fields["confirmed"] = result.Confirmed
	fields["unconfirmed"] = result.Unconfirmed
	fields["errors"] = found
	fields["errors_more"] = more
	fields["repair"] = repair
	fields["notes"] = notes
}

// jsonSize is how long a value is as JSON.
func jsonSize(value any) int {
	raw, err := json.Marshal(value)
	if err != nil {
		return math.MaxInt
	}
	return len(raw)
}

// checkNeverStarted says why a check whose command waited its whole time
// never started, as neverStarted does for a run, and whether its server was
// offline.
func (p *Panel) checkNeverStarted(command *core.Record) (string, bool) {
	offline := !p.Commands.Connected(command.GetString("host")) || p.markedOffline(command.GetString("host"))
	why := "was busy with other checks"
	if offline {
		why = "stayed offline"
	}
	return fmt.Sprintf("It waited %d hours while the server %s %s, and never started.", int(checkQueue.Hours()), command.GetString("host_name"), why), offline
}

// noteTargetCheck keeps the summary of a target's checks on the target: its
// newest check, and from a check that completed (ok or failed) the target's
// integrity, and the group its next check reads, which moves on only after
// ok. The checks are read in the transaction that writes the target.
func (p *Panel) noteTargetCheck(targetID string, completed *core.Record) {
	err := p.App.RunInTransaction(func(tx core.App) error {
		target, err := tx.FindRecordById(schema.BackupTargets, targetID)
		if err != nil {
			// a target removed meanwhile has nothing to keep
			return nil //nolint:nilerr // nothing to do
		}
		changed := false
		set := func(name string, value any) {
			if fmt.Sprint(target.Get(name)) != fmt.Sprint(value) {
				target.Set(name, value)
				changed = true
			}
		}
		rows, err := tx.FindRecordsByFilter(schema.BackupChecks, "target={:target}", "-created,-id", 1, 0, dbx.Params{"target": targetID})
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			newest := rows[0]
			set("last_check", newest.Id)
			set("last_check_status", newest.GetString("status"))
			reason := ""
			switch newest.GetString("status") {
			case schema.CheckSuspect, schema.CheckError, schema.CheckSkipped, schema.CheckCancelled:
				reason = cutText(newest.GetString("error"), 1000)
			}
			set("last_check_error", reason)
		}
		if completed != nil {
			fresh, err := tx.FindRecordById(schema.BackupChecks, completed.Id)
			if err != nil {
				return err
			}
			status := fresh.GetString("status")
			at := fresh.GetDateTime("finished_at")
			if (status == schema.CheckOK || status == schema.CheckFailed) && !at.Time().Before(target.GetDateTime("integrity_at").Time()) {
				set("integrity_status", status)
				target.Set("integrity_at", at)
				errorsText := ""
				if status == schema.CheckFailed {
					errorsText = cutText(checkErrorsText(fresh), 4096)
				}
				set("integrity_error", errorsText)
				if n, t, err := backup.ParseSubset(fresh.GetString("subset")); status == schema.CheckOK && err == nil && t > 0 {
					set("integrity_group", n%t+1)
				}
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return tx.Save(target)
	})
	if err != nil {
		p.Log.Error("failed to keep the integrity checks of a target", "target", targetID, "error", err)
	}
}

// checkErrors reads the errors a check found.
func checkErrors(row *core.Record) []backup.CheckError {
	found := []backup.CheckError{}
	_ = row.UnmarshalJSONField("errors", &found)
	return found
}

// checkErrorsText is the errors a check found, a line each.
func checkErrorsText(row *core.Record) string {
	lines := []string{}
	for _, item := range checkErrors(row) {
		lines = append(lines, item.Text)
	}
	if len(lines) == 0 {
		return row.GetString("error")
	}
	return strings.Join(lines, "\n")
}

// checkEnded keeps the summary of a check that ended on its target, and
// alerts: confirmed errors raise backup_check_failed, a check that could not
// tell or did not complete backup_check_unverified, and one that found
// nothing resolves both. A skipped check is an event only, but for an agent
// too old to check (alertSkip).
func (p *Panel) checkEnded(target, row *core.Record, alertSkip bool) {
	p.noteTargetCheck(target.Id, row)
	name := target.GetString("name")
	host := target.GetString("host")
	data := map[string]any{"target": target.Id, "check": row.Id, "command": row.GetString("command")}
	where := "in its structure"
	if subset := row.GetString("subset"); subset != "" {
		where = "in group " + subset
	}
	retry := ""
	if at := row.GetDateTime("retry_at"); !at.IsZero() {
		retry = fmt.Sprintf(" PocketCloud checks again at %s UTC.", at.Time().UTC().Format("15:04"))
	}
	reason := sentenceText(row.GetString("error"))
	switch row.GetString("status") {
	case schema.CheckFailed:
		first := "restic found errors"
		if found := checkErrors(row); len(found) > 0 {
			first = found[0].Text
		}
		message := cutText(fmt.Sprintf("The integrity check of %s found errors %s: %s", name, where, sentenceText(first)), 2048)
		p.raiseCheckAlert(target, alertCheckFailed, schema.SeverityCritical, message)
		p.resolveCheckAlerts(p.App, target.Id, alertCheckUnverified)
		events.Log(p.App, host, "backup.check_failed", schema.SeverityCritical, message, data)
	case schema.CheckOK:
		p.resolveCheckAlerts(p.App, target.Id, alertCheckFailed, alertCheckUnverified)
		message := fmt.Sprintf("The integrity check of %s found no errors %s.", name, where)
		if row.GetBool("unconfirmed") {
			message = fmt.Sprintf("The integrity check of %s found no errors %s: what a first reading without a lock saw was not there with the repository locked, as a prune running meanwhile can cause.", name, where)
		}
		events.Log(p.App, host, "backup.check_ok", schema.SeverityInfo, message, data)
	case schema.CheckSuspect:
		message := cutText(fmt.Sprintf("The integrity check of %s found errors %s that it could not confirm with the repository locked: %s", name, where, reason), 2048-len(retry)) + retry
		p.raiseCheckAlert(target, alertCheckUnverified, schema.SeverityWarning, message)
		events.Log(p.App, host, "backup.check_unverified", schema.SeverityWarning, message, data)
	case schema.CheckError:
		message := cutText(fmt.Sprintf("The integrity check of %s did not complete: %s", name, reason), 2048-len(retry)) + retry
		p.raiseCheckAlert(target, alertCheckUnverified, schema.SeverityWarning, message)
		events.Log(p.App, host, "backup.check_unverified", schema.SeverityWarning, message, data)
	case schema.CheckSkipped:
		message := cutText(fmt.Sprintf("The integrity check of %s was skipped: %s", name, lowerFirst(reason)), 2048-len(retry)) + retry
		if alertSkip {
			p.raiseCheckAlert(target, alertCheckUnverified, schema.SeverityWarning, message)
		}
		events.Log(p.App, host, "backup.check_skipped", schema.SeverityInfo, message, data)
	}
}

// raiseCheckAlert raises an alert of a target's checks, keyed by the target,
// on the target's server. It reads the target in the transaction that raises
// the alert: a target removed meanwhile alerts about nothing.
func (p *Panel) raiseCheckAlert(target *core.Record, rule, severity, message string) {
	err := p.App.RunInTransaction(func(tx core.App) error {
		fresh, err := tx.FindRecordById(schema.BackupTargets, target.Id)
		if err != nil {
			return nil //nolint:nilerr // nothing to raise
		}
		return events.RaiseAlert(tx, rule, fresh.Id, fresh.GetString("host"), severity, message)
	})
	if err != nil {
		p.Log.Error("failed to raise an alert", "rule", rule, "error", err)
	}
}

// retryDueChecks starts the retries of checks that are due: of a target that
// can still be checked, on a server that is not offline, whose check is not
// already under way.
func (p *Panel) retryDueChecks(now time.Time) {
	at, err := types.ParseDateTime(now.UTC())
	if err != nil {
		return
	}
	due, err := p.App.FindRecordsByFilter(schema.BackupChecks, "retry_at!='' && retry_at<={:now}", "retry_at,id", checkRetryBatch, 0,
		dbx.Params{"now": at.String()})
	if err != nil {
		p.Log.Error("failed to read the retries of integrity checks", "error", err)
		return
	}
	for _, row := range due {
		target, err := p.App.FindRecordById(schema.BackupTargets, row.GetString("target"))
		if err != nil {
			continue
		}
		host, err := p.App.FindRecordById(schema.Hosts, target.GetString("host"))
		switch {
		case !checkable(target) || err != nil:
			p.setFields(row, map[string]any{"retry_at": ""})
			continue
		case p.offline(host):
			p.setFields(row, map[string]any{"retry_at": now.UTC().Add(time.Hour)})
			continue
		case checkOpenIn(p.App, target.Id):
			// the check under way says how the target is
			p.setFields(row, map[string]any{"retry_at": ""})
			continue
		}
		if _, err := p.startCheck(target, checkStart{trigger: schema.RunRetry, prev: row, requester: "schedule", now: now}); err != nil && !errors.Is(err, errCheckTaken) {
			p.Log.Error("failed to try an integrity check again", "check", row.Id, "error", err)
		}
	}
}

// checksMigration is the migration that brought the integrity checks.
const checksMigration = "1771000000_backup_checks.go"

// checksBegan is when this panel began to check repositories: when the
// migration that brought the checks was applied; zero when that is not
// known.
func checksBegan(app core.App) time.Time {
	var applied int64
	err := app.DB().Select("applied").From(core.DefaultMigrationsTable).
		Where(dbx.HashExp{"file": checksMigration}).Row(&applied)
	if err != nil || applied <= 0 {
		return time.Time{}
	}
	return time.UnixMicro(applied).UTC()
}

// WatchChecks raises backup_check_unverified for every target that no check
// has verified for twice the time between two slots of the schedule and a
// day, as when its checks never complete: since its newest check that
// completed, or since it was added, or since the panel began to check, for a
// target it had before. The panel calls it hourly; it does nothing while the
// checks are off, and a target whose errors were confirmed has its alert
// already.
func (p *Panel) WatchChecks(now time.Time) {
	schedule, _ := p.checkSettings()
	rule := parseCronRule(schedule)
	first := rule.next(now)
	second := rule.next(first)
	if first.IsZero() || second.IsZero() {
		return
	}
	limit := 2*second.Sub(first) + 24*time.Hour
	targets, err := p.App.FindAllRecords(schema.BackupTargets)
	if err != nil {
		p.Log.Error("failed to read the backup targets", "error", err)
		return
	}
	// a target added before the panel could check it was not unverified
	// for all that time
	began := checksBegan(p.App)
	for _, target := range targets {
		if !checkable(target) || target.GetString("integrity_status") == schema.CheckFailed {
			continue
		}
		since := target.GetDateTime("integrity_at").Time()
		if since.IsZero() {
			since = maxTime(target.GetDateTime("created").Time(), began)
		}
		if now.Sub(since) <= limit {
			continue
		}
		p.raiseCheckAlert(target, alertCheckUnverified, schema.SeverityWarning,
			fmt.Sprintf("No integrity check of %s has completed since %s UTC.", target.GetString("name"), since.UTC().Format("2006-01-02 15:04")))
	}
}

// pruneBackupChecks removes the checks older than their history keeps, but
// each target's newest one, and its newest that completed.
func (p *Panel) pruneBackupChecks() {
	cutoff := types.NowDateTime().Add(-keepBackupChecks)
	table := schema.BackupChecks
	newer := func(extra string) string {
		return "SELECT 1 FROM " + table + " n WHERE n.target = " + table + ".target" + extra +
			" AND (n.created > " + table + ".created OR (n.created = " + table + ".created AND n.id > " + table + ".id))"
	}
	_, err := p.App.DB().NewQuery(
		"DELETE FROM " + table + " WHERE created < {:cutoff} AND status NOT IN ('queued', 'running')" +
			" AND EXISTS (" + newer("") + ")" +
			" AND (status NOT IN ('ok', 'failed') OR EXISTS (" + newer(" AND n.status IN ('ok', 'failed')") + "))",
	).Bind(dbx.Params{"cutoff": cutoff.String()}).Execute()
	if err != nil {
		p.Log.Error("failed to remove old integrity checks", "error", err)
	}
}
