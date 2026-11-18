package panel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// settleRestoreTests heals the restore tests the panel lost track of, as its
// scheduler passes: one whose command ended without its finisher, as when the
// panel stopped in between, is kept from its command; one whose command is
// gone did not complete.
func (p *Panel) settleRestoreTests() {
	open, err := p.App.FindRecordsByFilter(schema.BackupChecks, "kind={:kind} && (status={:queued} || status={:running})", "created,id", 0, 0,
		dbx.Params{"kind": schema.CheckRestoreTest, "queued": schema.CheckQueued, "running": schema.CheckRunning})
	if err != nil {
		p.Log.Error("failed to read the restore tests", "error", err)
		return
	}
	for _, row := range open {
		command, err := p.App.FindRecordById(schema.Commands, row.GetString("command"))
		switch {
		case err != nil:
			p.lostRestoreTest(row)
		case commandFinal(command):
			p.restoreTestFinished(command)
		}
	}
}

// lostRestoreTest ends a restore test whose command is gone: it did not
// complete, and is tried again as such a test is. Its copy may have been
// made, and is dropped in two hours.
func (p *Panel) lostRestoreTest(row *core.Record) {
	schedule := p.restoreTestSchedule()
	now := time.Now().UTC()
	kept := false
	err := p.update(context.Background(), row, func(fresh *core.Record) error {
		if s := fresh.GetString("status"); s != schema.CheckQueued && s != schema.CheckRunning {
			return errUnchanged
		}
		fresh.Set("status", schema.CheckError)
		fresh.Set("error", "The restore test ended without the panel recording how.")
		fresh.Set("finished_at", now)
		if retry := checkRetryAt(fresh, now, schedule); !retry.IsZero() {
			fresh.Set("retry_at", retry)
		}
		kept = true
		return nil
	})
	if err != nil {
		p.Log.Error("failed to keep a restore test", "check", row.Id, "error", err)
	}
	if kept {
		p.testCopiesAfter(row, backup.RestoreTestResult{}, false, true, now)
		p.restoreTestEnded(row, true)
	}
}

// retryDueRestoreTests starts the retries of restore tests that are due: of
// a job that still has a test to start, on a server that is not offline,
// with no test of the job under way.
func (p *Panel) retryDueRestoreTests(now time.Time) {
	at, err := types.ParseDateTime(now.UTC())
	if err != nil {
		return
	}
	due, err := p.App.FindRecordsByFilter(schema.BackupChecks, "kind={:kind} && retry_at!='' && retry_at<={:now}", "retry_at,id", checkRetryBatch, 0,
		dbx.Params{"kind": schema.CheckRestoreTest, "now": at.String()})
	if err != nil {
		p.Log.Error("failed to read the retries of restore tests", "error", err)
		return
	}
	for _, row := range due {
		job, err := p.App.FindRecordById(schema.BackupJobs, row.GetString("job"))
		if err != nil || !job.GetBool("enabled") {
			p.setFields(row, map[string]any{"retry_at": ""})
			continue
		}
		plan, problem := p.testPlanOf(job)
		switch {
		case problem != nil:
			p.setFields(row, map[string]any{"retry_at": ""})
			continue
		case p.offline(plan.host):
			p.setFields(row, map[string]any{"retry_at": now.UTC().Add(time.Hour)})
			continue
		case p.databaseTestOpen(plan.server, plan.database.GetString("name")) != nil:
			// the test under way says how the backup is
			p.setFields(row, map[string]any{"retry_at": ""})
			continue
		}
		if _, err := p.startRestoreTest(job, testStart{trigger: schema.RunRetry, prev: row, requester: "schedule", now: now}); err != nil &&
			!errors.Is(err, errCheckTaken) && !errors.Is(err, errNotTestable) {
			p.Log.Error("failed to try a restore test again", "check", row.Id, "error", err)
		}
	}
}

// testSnapshotText names the snapshot a test restored, as people read it:
// its short id, its time and its target, as "3f2a9c1b of 18 Nov 01:30 from
// b2".
func (p *Panel) testSnapshotText(row *core.Record) string {
	short := row.GetString("snapshot_id")
	if len(short) > 8 {
		short = short[:8]
	}
	text := short
	if runs, err := p.App.FindRecordsByFilter(schema.BackupRuns, "job={:job} && snapshot_id={:snapshot}", "-created", 1, 0,
		dbx.Params{"job": row.GetString("job"), "snapshot": row.GetString("snapshot_id")}); err == nil && len(runs) > 0 {
		at := runs[0].GetDateTime("started_at")
		if at.IsZero() {
			at = runs[0].GetDateTime("created")
		}
		text += " of " + at.Time().UTC().Format("2 Jan 15:04")
	}
	if target, err := p.App.FindRecordById(schema.BackupTargets, row.GetString("target")); err == nil {
		text += " from " + target.GetString("name")
	}
	return text
}

// restoreTestEnded says how a restore test ended in the events, and alerts:
// a test that found the backup could not be restored raises
// backup_restore_test_failed; one that could not tell, once it is not tried
// again (retried says such a test is), backup_restore_test_unverified; a test
// that passed resolves the second, and the first once no target of the job
// is still broken (settleFailedTestAlert).
func (p *Panel) restoreTestEnded(before *core.Record, retried bool) {
	row, err := p.App.FindRecordById(schema.BackupChecks, before.Id)
	if err != nil {
		return
	}
	job, err := p.App.FindRecordById(schema.BackupJobs, row.GetString("job"))
	if err != nil {
		return
	}
	what := fmt.Sprintf("The restore test of %s on %s", row.GetString("database_name"), job.GetString("db_server_name"))
	host := job.GetString("host")
	data := map[string]any{"job": job.Id, "check": row.Id, "target": row.GetString("target"), "command": row.GetString("command")}
	retry := ""
	if at := row.GetDateTime("retry_at"); !at.IsZero() {
		retry = fmt.Sprintf(" PocketCloud tests again at %s UTC.", at.Time().UTC().Format("15:04"))
	}
	reason := sentenceText(row.GetString("error"))
	switch row.GetString("status") {
	case schema.CheckOK:
		p.resolveTestAlerts(p.App, job.Id, backup.AlertRestoreTestUnverified)
		p.settleFailedTestAlert(p.App, job)
		message := cutText(fmt.Sprintf("%s passed: %d tables restored, as many as the dump has.", what, row.GetInt("tables_restored")), 2048)
		events.Log(p.App, host, backup.EventRestoreTestOK, schema.SeverityInfo, message, data)
	case schema.CheckFailed:
		message := p.failedTestMessage(job, row)
		p.raiseTestAlert(job, backup.AlertRestoreTestFailed, schema.SeverityCritical, message)
		p.resolveTestAlerts(p.App, job.Id, backup.AlertRestoreTestUnverified)
		events.Log(p.App, host, backup.EventRestoreTestFailed, schema.SeverityCritical, message, data)
	case schema.CheckError:
		message := cutText(fmt.Sprintf("%s did not complete: %s", what, reason), 2048-len(retry)) + retry
		if retry == "" {
			p.raiseTestAlert(job, backup.AlertRestoreTestUnverified, schema.SeverityWarning, message)
		}
		events.Log(p.App, host, backup.EventRestoreTestError, schema.SeverityWarning, message, data)
	case schema.CheckSkipped:
		message := cutText(fmt.Sprintf("%s was skipped: %s", what, lowerFirst(reason)), 2048-len(retry)) + retry
		if retried && retry == "" {
			p.raiseTestAlert(job, backup.AlertRestoreTestUnverified, schema.SeverityWarning, message)
		}
		events.Log(p.App, host, backup.EventRestoreTestSkipped, schema.SeverityInfo, message, data)
	}
}

// failedTestMessage says that a restore test found its backup could not be
// restored, naming the snapshot and its target.
func (p *Panel) failedTestMessage(job, row *core.Record) string {
	what := fmt.Sprintf("The restore test of %s on %s", row.GetString("database_name"), job.GetString("db_server_name"))
	return cutText(fmt.Sprintf("%s failed: %s The snapshot %s could not be restored.", what, sentenceText(row.GetString("error")), p.testSnapshotText(row)), 2048)
}

// brokenTarget is the newest failed restore test of a job on a target that
// is still broken, if any: one of the job's targets whose newest test that
// told, ok or failed, failed. A test of one target says nothing of another.
func brokenTarget(app core.App, job *core.Record) *core.Record {
	var newest *core.Record
	for _, target := range job.GetStringSlice("targets") {
		tests, err := app.FindRecordsByFilter(schema.BackupChecks,
			"kind={:kind} && job={:job} && target={:target} && (status={:ok} || status={:failed})", "-created,-id", 1, 0,
			dbx.Params{"kind": schema.CheckRestoreTest, "job": job.Id, "target": target, "ok": schema.CheckOK, "failed": schema.CheckFailed})
		if err != nil || len(tests) == 0 || tests[0].GetString("status") != schema.CheckFailed {
			continue
		}
		if newest == nil || tests[0].GetDateTime("created").After(newest.GetDateTime("created")) {
			newest = tests[0]
		}
	}
	return newest
}

// settleFailedTestAlert resolves a job's open backup_restore_test_failed
// once no target of the job is still broken, as after a test of the broken
// target passed, or after that target left the job. While one is, the alert
// stays, about that target's newest failed test: a test that passed on
// another target does not resolve it.
func (p *Panel) settleFailedTestAlert(app core.App, job *core.Record) {
	open, err := app.FindRecordsByFilter(schema.Alerts, "rule={:rule} && target={:job} && status!={:resolved}", "", 1, 0,
		dbx.Params{"rule": backup.AlertRestoreTestFailed, "job": job.Id, "resolved": schema.AlertResolved})
	if err != nil || len(open) == 0 {
		return
	}
	broken := brokenTarget(app, job)
	if broken == nil {
		if err := events.ResolveAlert(app, backup.AlertRestoreTestFailed, job.Id); err != nil {
			p.Log.Error("failed to resolve an alert", "rule", backup.AlertRestoreTestFailed, "error", err)
		}
		return
	}
	if message := p.failedTestMessage(job, broken); open[0].GetString("message") != message {
		open[0].Set("message", message)
		if err := app.Save(open[0]); err != nil {
			p.Log.Error("failed to keep an alert", "rule", backup.AlertRestoreTestFailed, "error", err)
		}
	}
}

// raiseTestAlert raises an alert of a job's restore tests, keyed by the job,
// on the job's server. It reads the job in the transaction that raises the
// alert: a job removed meanwhile alerts about nothing, and an open critical
// alert of the rule stays critical.
func (p *Panel) raiseTestAlert(job *core.Record, rule, severity, message string) {
	err := p.App.RunInTransaction(func(tx core.App) error {
		fresh, err := tx.FindRecordById(schema.BackupJobs, job.Id)
		if err != nil {
			return nil //nolint:nilerr // nothing to raise
		}
		return events.RaiseAlert(tx, rule, fresh.Id, fresh.GetString("host"), keptSeverity(tx, rule, fresh.Id, severity), message)
	})
	if err != nil {
		p.Log.Error("failed to raise an alert", "rule", rule, "error", err)
	}
}

// WatchRestoreTests raises backup_restore_test_unverified for every enabled
// database job with snapshots whose restore tests have not passed for twice
// the longest time between two slots of the schedule and a day: since its
// newest test that passed, or since its first snapshot, or since the panel
// began to test. The panel calls it hourly; it does nothing while the tests
// are off, and a job whose newest test failed has its alert already.
func (p *Panel) WatchRestoreTests(now time.Time) {
	_, most := checkGaps(parseCronRule(p.restoreTestSchedule()), now)
	if most == 0 {
		return
	}
	limit := 2*most + 24*time.Hour
	jobs, err := p.App.FindRecordsByFilter(schema.BackupJobs, "origin={:origin} && enabled=true && database!=''", "created,id", 0, 0,
		dbx.Params{"origin": schema.JobDatabase})
	if err != nil {
		p.Log.Error("failed to read the database jobs", "error", err)
		return
	}
	began := migrationApplied(p.App, databasesMigration)
	for _, job := range jobs {
		failed, err := p.App.FindRecordsByFilter(schema.Alerts, "rule={:rule} && target={:job} && status!={:resolved}", "", 1, 0,
			dbx.Params{"rule": backup.AlertRestoreTestFailed, "job": job.Id, "resolved": schema.AlertResolved})
		if err != nil || len(failed) > 0 {
			continue
		}
		first, err := p.App.FindRecordsByFilter(schema.BackupRuns, "job={:job} && status={:succeeded} && snapshot_id!=''", "created,id", 1, 0,
			dbx.Params{"job": job.Id, "succeeded": schema.RunSucceeded})
		if err != nil || len(first) == 0 {
			continue
		}
		since := time.Time{}
		if passed, err := p.App.FindRecordsByFilter(schema.BackupChecks, "kind={:kind} && job={:job} && status={:ok}", "-finished_at", 1, 0,
			dbx.Params{"kind": schema.CheckRestoreTest, "job": job.Id, "ok": schema.CheckOK}); err == nil && len(passed) > 0 {
			since = passed[0].GetDateTime("finished_at").Time()
		}
		if since.IsZero() {
			since = maxTime(first[0].GetDateTime("created").Time(), began)
		}
		if now.Sub(since) <= limit {
			continue
		}
		name := job.GetString("source")
		if database, err := p.App.FindRecordById(schema.Databases, job.GetString("database")); err == nil {
			name = database.GetString("name")
		}
		p.raiseTestAlert(job, backup.AlertRestoreTestUnverified, schema.SeverityWarning,
			fmt.Sprintf("No restore test of %s on %s has passed since %s UTC.", name, job.GetString("db_server_name"), since.UTC().Format("2006-01-02 15:04")))
	}
}

// restoreTestsTurnedOff resolves what only restore tests would resolve: the
// alerts that a job's backups are not verified, and the retries of tests.
func (p *Panel) restoreTestsTurnedOff() {
	p.resolveTestAlerts(p.App, "", backup.AlertRestoreTestUnverified)
	planned, err := p.App.FindRecordsByFilter(schema.BackupChecks, "kind={:kind} && retry_at!=''", "", 0, 0,
		dbx.Params{"kind": schema.CheckRestoreTest})
	if err != nil {
		p.Log.Error("failed to read the restore tests", "error", err)
		return
	}
	for _, row := range planned {
		p.setFields(row, map[string]any{"retry_at": ""})
	}
}
