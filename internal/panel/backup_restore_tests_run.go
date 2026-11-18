package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/cron"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/stacks"
)

// Restore tests run on a schedule of their own, restore_test_schedule in
// the settings (empty turns them off): at each slot, and at a slot missed
// while the panel was down less than a day ago, once, every enabled database
// job whose database and server are ready and that has a snapshot gets a
// test, from the target whose newest test of the job is oldest. One whose
// server is offline, whose agent cannot test, or whose user's password is
// being rotated gets a skipped test, tried again as a check is. Tests of one
// server run one at a time, as their ordering key says. A test that found
// the backup could not be restored raises backup_restore_test_failed
// (critical); one that could not tell, after its last attempt, and a job
// whose tests have not passed for long, backup_restore_test_unverified.

const (
	// testCopyLife is how long a restore test's copy stays after the test,
	// and at most while it waits, unless its test is open.
	testCopyLife = 2 * time.Hour
	// restoreTestQueue is how long a test waits for its server, and for the
	// tests of the server before it.
	restoreTestQueue = 12 * time.Hour
	// databasesMigration is the migration that brought the restore tests.
	databasesMigration = "1773000000_database_backups.go"
)

// errNotTestable says a job has no restore test to start now: it is not
// eligible, as a job whose database was dropped.
var errNotTestable = errors.New("the job has no restore test to start")

func (p *Panel) registerRestoreTests() {
	p.Commands.OnStart("backup.restore_test", p.restoreTestStarted)
	p.Commands.OnFinish("backup.restore_test", p.restoreTestFinished)
	p.App.Cron().MustAdd("pocketcloud_restore_tests", "* * * * *", func() { p.RunDueRestoreTests(time.Now()) })
	p.App.Cron().MustAdd("pocketcloud_restore_tests_watch", "43 * * * *", func() { p.WatchRestoreTests(time.Now()) })

	// nothing would resolve the alerts of a job's tests once the job is
	// gone, turned off or its database dropped
	p.App.OnRecordAfterDeleteSuccess(schema.BackupJobs).BindFunc(func(e *core.RecordEvent) error {
		p.resolveTestAlerts(e.App, e.Record.Id, backup.AlertRestoreTestFailed, backup.AlertRestoreTestUnverified)
		return e.Next()
	})
	// whatever the job was before: a record the panel made in memory has
	// no original to compare with. A broken target that left the job, as
	// one taken out of it or removed, keeps its failed alert no longer.
	p.App.OnRecordAfterUpdateSuccess(schema.BackupJobs).BindFunc(func(e *core.RecordEvent) error {
		job := e.Record
		gone := job.GetString("origin") == schema.JobDatabase && job.GetString("database") == ""
		left := slices.ContainsFunc(job.Original().GetStringSlice("targets"), func(target string) bool {
			return !slices.Contains(job.GetStringSlice("targets"), target)
		})
		switch {
		case !job.GetBool("enabled") || gone:
			p.resolveTestAlerts(e.App, job.Id, backup.AlertRestoreTestFailed, backup.AlertRestoreTestUnverified)
		case left:
			p.settleFailedTestAlert(e.App, job)
		}
		return e.Next()
	})
}

// resolveTestAlerts resolves the open alerts of a job's restore tests with
// the rules, or every job's when job is "".
func (p *Panel) resolveTestAlerts(app core.App, job string, rules ...string) {
	alerts := []*core.Record{}
	query := app.RecordQuery(schema.Alerts).
		AndWhere(dbx.In("rule", toAny(rules)...)).
		AndWhere(dbx.Not(dbx.HashExp{"status": schema.AlertResolved}))
	if job != "" {
		query = query.AndWhere(dbx.HashExp{"target": job})
	}
	if err := query.All(&alerts); err != nil {
		p.Log.Error("failed to read the alerts of restore tests", "error", err)
		return
	}
	for _, alert := range alerts {
		if err := events.ResolveAlert(app, alert.GetString("rule"), alert.GetString("target")); err != nil {
			p.Log.Error("failed to resolve an alert", "rule", alert.GetString("rule"), "error", err)
		}
	}
}

// restoreTestSchedule is when the restore tests run, "" while they are off.
func (p *Panel) restoreTestSchedule() string {
	return restoreTestScheduleIn(p.App)
}

func restoreTestScheduleIn(app core.App) string {
	records, err := app.FindRecordsByFilter(schema.Settings, "", "-created", 1, 0)
	if err != nil || len(records) == 0 {
		return ""
	}
	schedule := stacks.NormalSchedule(records[0].GetString("restore_test_schedule"))
	if parseCronRule(schedule) == nil {
		return ""
	}
	return schedule
}

// migrationApplied is when a migration was applied; zero when that is not
// known.
func migrationApplied(app core.App, file string) time.Time {
	var applied int64
	err := app.DB().Select("applied").From(core.DefaultMigrationsTable).Where(dbx.HashExp{"file": file}).Row(&applied)
	if err != nil || applied <= 0 {
		return time.Time{}
	}
	return time.UnixMicro(applied).UTC()
}

// restoreTestSlot is the slot of the schedule a pass at a minute starts the
// tests of: that minute when it is one, or the newest slot of the last day
// that came while the panel was down, after the panel began to test; zero
// when there is none.
func (p *Panel) restoreTestSlot(rule cronRule, minute time.Time) time.Time {
	if rule.due(cron.NewMoment(minute)) {
		return minute
	}
	last := time.Time{}
	for at, i := rule.next(minute.Add(-catchUpWithin)), 0; !at.IsZero() && at.Before(minute) && i < 2*24*60; at, i = rule.next(at), i+1 {
		last = at
	}
	started := time.Unix(0, p.started.Load()).UTC()
	if last.IsZero() || !last.Before(started) || !last.After(migrationApplied(p.App, databasesMigration)) {
		return time.Time{}
	}
	return last
}

// RunDueRestoreTests starts the restore tests of a slot of the schedule, in
// UTC, and those that are due again: the panel calls it every minute. The
// tests whose command ended without the panel are settled first.
func (p *Panel) RunDueRestoreTests(now time.Time) {
	p.checkPass.Lock()
	defer p.checkPass.Unlock()
	p.settleRestoreTests()
	schedule := p.restoreTestSchedule()
	if schedule == "" {
		return
	}
	minute := now.UTC().Truncate(time.Minute)
	if slot := p.restoreTestSlot(parseCronRule(schedule), minute); !slot.IsZero() {
		jobs, err := p.App.FindRecordsByFilter(schema.BackupJobs, "origin={:origin} && enabled=true && database!=''", "created,id", 0, 0,
			dbx.Params{"origin": schema.JobDatabase})
		if err != nil {
			p.Log.Error("failed to read the database jobs", "error", err)
			return
		}
		at, err := types.ParseDateTime(slot)
		if err != nil {
			return
		}
		for _, job := range jobs {
			// once per slot, whichever target its test took
			if done, err := p.App.FindRecordsByFilter(schema.BackupChecks, "kind={:kind} && job={:job} && scheduled_at={:slot}", "", 1, 0,
				dbx.Params{"kind": schema.CheckRestoreTest, "job": job.Id, "slot": at.String()}); err != nil || len(done) > 0 {
				continue
			}
			_, err := p.startRestoreTest(job, testStart{trigger: schema.RunSchedule, scheduledAt: slot, requester: "schedule", now: now})
			if err != nil && !errors.Is(err, errCheckTaken) && !errors.Is(err, errNotTestable) {
				p.Log.Error("failed to start a restore test", "job", job.Id, "error", err)
			}
		}
	}
	p.retryDueRestoreTests(minute)
}

// testStart says how a restore test starts.
type testStart struct {
	trigger string
	// scheduledAt is the slot of the schedule, for the first attempt of a
	// scheduled test
	scheduledAt time.Time
	// prev is the test this one tries again
	prev *core.Record
	// requestedBy and requester are who asked for it
	requestedBy, requester string
	// refuse answers what keeps the test from starting as a *targetProblem,
	// for the route, instead of keeping a skipped test
	refuse bool
	// now is the scheduler's pass; zero is now
	now time.Time
}

// testPlan is what a restore test restores, from where, onto which server.
type testPlan struct {
	job, database, server, user, host, target, run *core.Record
}

// testPlanOf works out a job's restore test: its database ready on a ready
// shared server, and the snapshot of the target whose newest test of the job
// is oldest, never tested first, in the job's order.
func (p *Panel) testPlanOf(job *core.Record) (testPlan, *targetProblem) {
	conflict := func(format string, args ...any) (testPlan, *targetProblem) {
		return testPlan{}, &targetProblem{http.StatusConflict, fmt.Sprintf(format, args...)}
	}
	if job.GetString("origin") != schema.JobDatabase || job.GetString("source_kind") != schema.SourceDatabase {
		return testPlan{}, &targetProblem{http.StatusBadRequest, "Only a database's job has restore tests."}
	}
	database, server, user, err := p.testSubject(job)
	if err != nil {
		return conflict("The database of this job was dropped.")
	}
	name := database.GetString("name")
	if database.GetString("status") != dbReady || isTemporary(database) {
		return conflict("The database %s is not ready.", name)
	}
	if server.GetString("kind") != schema.DBServerShared || server.GetString("status") != dbReady {
		return conflict("The database server %s is not ready.", server.GetString("name"))
	}
	host, err := p.App.FindRecordById(schema.Hosts, job.GetString("host"))
	if err != nil {
		return conflict("The server of the job is gone.")
	}
	target, run := p.testTarget(job)
	if target == nil {
		return conflict("No backup of %s has succeeded yet.", name)
	}
	return testPlan{job: job, database: database, server: server, user: user, host: host, target: target, run: run}, nil
}

// testTarget is the target a job's next restore test reads, and the run of
// the snapshot it restores: among the job's targets with a succeeded run,
// the one whose newest test of the job is oldest, never tested first, ties in
// the job's order; its newest succeeded run.
func (p *Panel) testTarget(job *core.Record) (*core.Record, *core.Record) {
	var best, bestRun *core.Record
	var bestTested time.Time
	for _, target := range p.jobTargets(job) {
		if target.GetString("status") == schema.TargetPending {
			continue
		}
		runs, err := p.App.FindRecordsByFilter(schema.BackupRuns, "job={:job} && target={:target} && status={:succeeded} && snapshot_id!=''",
			"-created,-id", 1, 0, dbx.Params{"job": job.Id, "target": target.Id, "succeeded": schema.RunSucceeded})
		if err != nil || len(runs) == 0 {
			continue
		}
		tested := time.Time{}
		if tests, err := p.App.FindRecordsByFilter(schema.BackupChecks, "kind={:kind} && job={:job} && target={:target}", "-created,-id", 1, 0,
			dbx.Params{"kind": schema.CheckRestoreTest, "job": job.Id, "target": target.Id}); err == nil && len(tests) > 0 {
			tested = tests[0].GetDateTime("created").Time()
			if tested.IsZero() {
				tested = time.Unix(1, 0)
			}
		}
		if best == nil || tested.Before(bestTested) {
			best, bestRun, bestTested = target, runs[0], tested
		}
	}
	return best, bestRun
}

// testBlocked says what keeps a restore test from starting now, if
// anything: its server is offline, its agent cannot test, its user's
// password is being rotated; a test of the job, or a restore of its
// database, is under way.
func (p *Panel) testBlocked(plan testPlan, at time.Time, refuse bool) *checkBlock {
	name := plan.database.GetString("name")
	hostName := plan.host.GetString("name")
	if (refuse && !p.Commands.Connected(plan.host.Id)) || (!refuse && p.offline(plan.host)) {
		return &checkBlock{status: schema.CheckSkipped, retry: true,
			reason:  fmt.Sprintf("The server %s was offline at %s UTC.", hostName, at.UTC().Format("15:04")),
			refusal: fmt.Sprintf("The server %s is offline: its agent runs the test.", hostName)}
	}
	if !slices.Contains(plan.host.GetStringSlice("capabilities"), "backup.restore_test") {
		text := fmt.Sprintf("The agent of %s cannot test restores yet: update it.", hostName)
		return &checkBlock{status: schema.CheckSkipped, retry: true, reason: text, refusal: text}
	}
	if plan.user.GetString("pending_password") != "" {
		text := fmt.Sprintf("A rotation of %s's password is under way.", plan.user.GetString("name"))
		return &checkBlock{status: schema.CheckSkipped, retry: true, reason: text, refusal: text}
	}
	if p.databaseTestOpen(plan.server, name) != nil {
		return &checkBlock{status: schema.CheckSkipped, reason: "The test before it was still going.",
			refusal: fmt.Sprintf("A restore test of %s is under way.", name)}
	}
	if p.databaseRestoreOpen(plan.server, name) != nil {
		return &checkBlock{status: schema.CheckSkipped, retry: true,
			reason:  fmt.Sprintf("A restore of %s was under way.", name),
			refusal: fmt.Sprintf("A restore of %s is under way.", name)}
	}
	return nil
}

// startRestoreTest starts a job's restore test: in one transaction, a
// queued row, the record of its temporary database and its command, which
// drops the server's earlier test copies whose tests ended; or a skipped row
// when something keeps it from starting. A retry clears the retry of the test
// it tries again in the same transaction, and a slot of the schedule gets one
// test per job and target: a second start of either is errCheckTaken. A job
// that has no test to start is errNotTestable, or its refusal for the route.
func (p *Panel) startRestoreTest(job *core.Record, start testStart) (*core.Record, error) {
	job, err := p.App.FindRecordById(schema.BackupJobs, job.Id)
	if err != nil {
		return nil, err
	}
	plan, problem := p.testPlanOf(job)
	if problem != nil {
		if start.refuse {
			return nil, problem
		}
		return nil, errNotTestable
	}
	now := start.now.UTC()
	if start.now.IsZero() {
		now = time.Now().UTC()
	}
	at := now
	if !start.scheduledAt.IsZero() {
		at = start.scheduledAt
	}
	schedule := p.restoreTestSchedule()
	block := p.testBlocked(plan, at, start.refuse)
	if block != nil && start.refuse {
		return nil, &targetProblem{http.StatusConflict, block.refusal}
	}
	var repository *agentv1.BackupRepository
	if block == nil {
		repository, err = p.repositoryOf(plan.target)
		if err != nil {
			text := sentence(err)
			if start.refuse {
				return nil, &targetProblem{http.StatusConflict, text}
			}
			block = &checkBlock{status: schema.CheckError, reason: text}
		}
	}
	checks, err := p.App.FindCachedCollectionByNameOrId(schema.BackupChecks)
	if err != nil {
		return nil, err
	}
	attempt := 1
	if start.prev != nil {
		attempt = max(start.prev.GetInt("attempt"), 1) + 1
	}
	name := plan.database.GetString("name")
	var row *core.Record
	var ended *checkBlock
	err = p.App.RunInTransaction(func(tx core.App) error {
		row = core.NewRecord(checks)
		row.Id = core.GenerateDefaultRandomId()
		row.Load(map[string]any{
			"kind": schema.CheckRestoreTest, "job": job.Id, "target": plan.target.Id, "host": plan.host.Id, "trigger": start.trigger,
			"status": schema.CheckQueued, "attempt": attempt, "database_name": name, "snapshot_id": plan.run.GetString("snapshot_id"),
			"temporary": databases.TestDatabaseName(row.Id), "dump_bytes": plan.run.GetInt("total_bytes"),
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
		// read again in the transaction: two starts at once start one test
		if open, err := tx.FindRecordsByFilter(schema.BackupChecks,
			"kind={:kind} && host={:host} && database_name={:name} && (status={:queued} || status={:running})", "", 1, 0,
			dbx.Params{"kind": schema.CheckRestoreTest, "host": plan.host.Id, "name": name,
				"queued": schema.CheckQueued, "running": schema.CheckRunning}); err != nil || len(open) > 0 {
			return errCheckOpen
		}
		// the copy's record is its schedule, from the start
		collection, err := tx.FindCachedCollectionByNameOrId(schema.Databases)
		if err != nil {
			return err
		}
		copied := core.NewRecord(collection)
		copied.Load(map[string]any{"server": plan.server.Id, "name": row.GetString("temporary"), "status": dbCreating, "owner_kind": "none",
			"temporary_until": time.Now().UTC().Add(testCopyLife)})
		if err := tx.SaveWithContext(audit.WithActor(context.Background(), dbActor), copied); err != nil {
			return err
		}
		// the earlier copies it drops are worked out as it is delivered
		payload := restoreTestPayload(row, plan.server, plan.user.GetString("name"))
		payload.Repository = repository
		// delivered once the transaction committed, with its row
		command, err := p.Commands.CreateIn(context.Background(), tx, commands.Options{
			Host: plan.host.Id, Label: cutText(fmt.Sprintf("Test the restore of %s on %s", name, plan.server.GetString("name")), 255),
			Command:     &agentv1.Command{Payload: &agentv1.Command_BackupRestoreTest{BackupRestoreTest: payload}},
			RequestedBy: start.requestedBy, Requester: start.requester, QueueTimeout: restoreTestQueue,
		})
		if err != nil {
			return err
		}
		row.Set("command", command.Id)
		return tx.Save(row)
	})
	switch {
	case errors.Is(err, errCheckOpen) && start.refuse:
		return nil, &targetProblem{http.StatusConflict, fmt.Sprintf("A restore test of %s is under way.", name)}
	case uniqueViolation(err), errors.Is(err, errCheckOpen):
		return nil, errCheckTaken
	case err != nil:
		return nil, err
	}
	if ended != nil {
		p.restoreTestEnded(row, ended.retry)
	}
	return row, nil
}

// earlierTestCopies are the copies of a server's earlier restore tests that
// ended, which a test drops before it restores. They are worked out as the
// test is delivered, not as it was queued: its ordering key has let every
// test of the server queued before it end by then, those of its own slot
// too, so a server keeps its newest test copy only. A test ended once its
// row or its command did, as its finisher may still be keeping it. self is
// the test's own copy.
func earlierTestCopies(app core.App, server *core.Record, self string) []string {
	records, err := app.FindRecordsByFilter(schema.Databases, "server={:server} && name!={:self} && status!={:dropping}", "name", 0, 0,
		dbx.Params{"server": server.Id, "self": self, "dropping": dbDropping})
	if err != nil {
		return nil
	}
	names := []string{}
	for _, record := range records {
		prefix, id, ok := databases.TemporaryOwner(record.GetString("name"))
		if !ok || prefix != databases.TemporaryTest || !isTemporary(record) {
			continue
		}
		if row, err := app.FindRecordById(schema.BackupChecks, id); err == nil && testRunning(app, row) {
			continue
		}
		names = append(names, record.GetString("name"))
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// testRunning tells whether a restore test may still restore into its copy:
// its row is open, and its command is there and not final.
func testRunning(app core.App, row *core.Record) bool {
	if s := row.GetString("status"); s != schema.CheckQueued && s != schema.CheckRunning {
		return false
	}
	command, err := app.FindRecordById(schema.Commands, row.GetString("command"))
	return err == nil && !commandFinal(command)
}

// handleRestoreTest tests a database job's restore now, as an operator asks:
// its row and command answer at once, and say how it goes. A test by hand
// is never tried again.
func (p *Panel) handleRestoreTest(e *core.RequestEvent) error {
	job, err := e.App.FindRecordById(schema.BackupJobs, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The job does not exist.", err)
	}
	requestedBy, requester := requester(e)
	row, err := p.startRestoreTest(job, testStart{trigger: schema.RunManual, requestedBy: requestedBy, requester: requester, refuse: true})
	var problem *targetProblem
	if errors.As(err, &problem) {
		return e.Error(problem.status, problem.message, nil)
	}
	if errors.Is(err, errCheckTaken) {
		return e.Error(http.StatusConflict, fmt.Sprintf("A restore test of %s is under way.", job.GetString("source")), nil)
	}
	if err != nil {
		return e.BadRequestError("The restore test could not be started: "+saveErrorText(err), nil)
	}
	return e.JSON(http.StatusAccepted, map[string]any{"check": row.Id, "command": row.GetString("command")})
}

// restoreTestOf is the restore test whose command a backup.restore_test is.
func (p *Panel) restoreTestOf(commandID string) (*core.Record, error) {
	return p.App.FindFirstRecordByFilter(schema.BackupChecks, "kind={:kind} && command={:command}",
		dbx.Params{"kind": schema.CheckRestoreTest, "command": commandID})
}

// restoreTestStarted marks a restore test as running once its agent
// started it.
func (p *Panel) restoreTestStarted(command *core.Record) {
	row, err := p.restoreTestOf(command.Id)
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
		p.Log.Error("failed to mark a restore test as running", "check", row.Id, "error", err)
	}
}

// testSecrets are what a restore test's row and log may not hold: the
// target's secrets, and the passwords of the database's server and user.
func (p *Panel) testSecrets(row *core.Record) []string {
	hide := []string{}
	if target, err := p.App.FindRecordById(schema.BackupTargets, row.GetString("target")); err == nil {
		hide = p.targetSecretValues(target)
	}
	if job, err := p.App.FindRecordById(schema.BackupJobs, row.GetString("job")); err == nil {
		if _, server, user, err := p.testSubject(job); err == nil {
			hide = append(hide, p.serverSecrets(server, user.GetString("name"))...)
		}
	}
	return hide
}

// restoreTestFinished keeps how a restore test went, from its command and
// the agent's answer, plans its retry, keeps or drops its copy's record and
// those of the copies it dropped, and alerts.
func (p *Panel) restoreTestFinished(command *core.Record) {
	row, err := p.restoreTestOf(command.Id)
	if err != nil {
		return
	}
	if s := row.GetString("status"); s != schema.CheckQueued && s != schema.CheckRunning {
		return
	}
	var result backup.RestoreTestResult
	raw, _ := json.Marshal(command.Get("result"))
	_ = json.Unmarshal(raw, &result)
	stored := strings.TrimSpace(command.GetString("result"))
	hasResult := stored != "" && stored != "null" && stored != "{}"

	started := command.GetDateTime("started_at")
	finished := command.GetDateTime("finished_at")
	if finished.IsZero() {
		finished = types.NowDateTime()
	}
	message := command.GetString("error")
	retried := false
	var status string
	switch commandStatus := command.GetString("status"); {
	case commandStatus == schema.CommandSucceeded:
		status, message = schema.CheckOK, ""
	case commandStatus == schema.CommandFailed && backup.TestFailed(result.Failure):
		status = schema.CheckFailed
	case started.IsZero() && (commandStatus == schema.CommandTimedOut || commandStatus == schema.CommandCancelled && cancelledByItsTime(command, finished)):
		// it waited its whole time and never started: no test happened
		status, retried = schema.CheckSkipped, true
		message, _ = p.testNeverStarted(command)
	case commandStatus == schema.CommandCancelled:
		status = schema.CheckCancelled
	case commandStatus == schema.CommandTimedOut:
		status, retried = schema.CheckError, true
		message = fmt.Sprintf("The restore test did not finish in %d hours.", int(commands.Specs["backup.restore_test"].Timeout.Hours()))
	default:
		status, retried = schema.CheckError, true
		if message == "" {
			message = "The restore test failed."
		}
	}
	message = strings.TrimPrefix(message, "failed to prepare the command: ")

	hide := p.testSecrets(row)
	fields := map[string]any{
		"status": status, "finished_at": finished, "error": cutText(sentenceText(backup.Redact(message, hide...)), 4096),
		"log": cutTail(backup.Redact(result.Log, hide...), maxRunLog), "failure": cutText(result.Failure, 32),
		"tables_expected": result.TablesExpected, "tables_restored": result.Tables,
	}
	if result.Bytes > 0 {
		fields["dump_bytes"] = result.Bytes
	}
	if !started.IsZero() {
		fields["started_at"] = started
		fields["duration_ms"] = finished.Time().Sub(started.Time()).Milliseconds()
	}
	schedule := p.restoreTestSchedule()

	// once: the test's command may be settled from two places
	kept := false
	err = p.update(context.Background(), row, func(fresh *core.Record) error {
		if s := fresh.GetString("status"); s != schema.CheckQueued && s != schema.CheckRunning {
			return errUnchanged
		}
		for name, value := range fields {
			fresh.Set(name, value)
		}
		if retry := checkRetryAt(fresh, finished.Time(), schedule); retried && !retry.IsZero() {
			fresh.Set("retry_at", retry)
		}
		kept = true
		return nil
	})
	if err != nil {
		p.Log.Error("failed to keep a restore test", "check", row.Id, "error", err)
	}
	if !kept {
		return
	}
	p.testCopiesAfter(row, result, hasResult, !started.IsZero(), finished.Time())
	p.restoreTestEnded(row, retried)
}

// testNeverStarted says why a test whose command waited its whole time
// never started, and whether its server was offline.
func (p *Panel) testNeverStarted(command *core.Record) (string, bool) {
	offline := !p.Commands.Connected(command.GetString("host")) || p.markedOffline(command.GetString("host"))
	why := "was busy with other tests"
	if offline {
		why = "stayed offline"
	}
	return fmt.Sprintf("It waited %d hours while the server %s %s, and never started.", int(restoreTestQueue.Hours()), command.GetString("host_name"), why), offline
}

// testCopiesAfter follows a test's copies once it ended: its own copy stays
// two hours when the agent made it, or may have, as when it answered nothing
// after it started; it goes when the agent says it made none, or the test
// never started. The earlier copies it dropped go.
func (p *Panel) testCopiesAfter(row *core.Record, result backup.RestoreTestResult, answered, started bool, finished time.Time) {
	ctx := audit.WithActor(context.Background(), dbActor)
	copied, err := p.App.FindFirstRecordByFilter(schema.Databases, "name={:name}", dbx.Params{"name": row.GetString("temporary")})
	server := ""
	if err == nil && isTemporary(copied) {
		server = copied.GetString("server")
		if made := answered && result.Created || !answered && started; made {
			p.setFields(copied, map[string]any{"status": dbReady, "temporary_until": finished.UTC().Add(testCopyLife)})
		} else if err := p.App.DeleteWithContext(ctx, copied); err != nil {
			p.Log.Error("failed to remove a temporary database", "database", copied.Id, "error", err)
		}
	}
	for _, name := range result.Dropped {
		if prefix, _, ok := databases.TemporaryOwner(name); !ok || prefix != databases.TemporaryTest || server == "" {
			continue
		}
		dropped, err := p.App.FindFirstRecordByFilter(schema.Databases, "server={:server} && name={:name}", dbx.Params{"server": server, "name": name})
		if err == nil && isTemporary(dropped) {
			if err := p.App.DeleteWithContext(ctx, dropped); err != nil {
				p.Log.Error("failed to remove a temporary database", "database", dropped.Id, "error", err)
			}
		}
	}
}
