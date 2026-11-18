package panel

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// testsOf are a job's restore tests, oldest first.
func (f *jobFixture) testsOf(job *core.Record) []*core.Record {
	f.t.Helper()
	rows, err := f.app.FindRecordsByFilter(schema.BackupChecks, "kind={:kind} && job={:job}", "created,id", 0, 0,
		map[string]any{"kind": schema.CheckRestoreTest, "job": job.Id})
	if err != nil {
		f.t.Fatal(err)
	}
	return rows
}

// restoreTestSettings stores when the restore tests run.
func (f *jobFixture) restoreTestSettings(schedule string) {
	f.t.Helper()
	settings, err := f.p.settingsRecord()
	if err != nil {
		f.t.Fatal(err)
	}
	settings.Set("restore_test_schedule", schedule)
	if err := f.app.Save(settings); err != nil {
		f.t.Fatal(err)
	}
}

// startedTest starts a restore test of a job, and is its row and command.
func (f *jobFixture) startedTest(job *core.Record, trigger string) (*core.Record, *core.Record) {
	f.t.Helper()
	row, err := f.p.startRestoreTest(job, testStart{trigger: trigger, requester: "test"})
	if err != nil {
		f.t.Fatal(err)
	}
	command, err := f.app.FindRecordById(schema.Commands, row.GetString("command"))
	if err != nil {
		f.t.Fatalf("the test %v has no command: %v", row.FieldsData(), err)
	}
	return row, command
}

// endTest ends a test's command as the fields say, as the agent and the
// queue would, and is the test as kept.
func (f *jobFixture) endTest(row, command *core.Record, fields map[string]any) *core.Record {
	f.t.Helper()
	status, _ := fields["status"].(string)
	f.closeCommand(command, status)
	ended := f.fresh(command)
	ended.Load(map[string]any{"finished_at": types.NowDateTime()})
	ended.Load(fields)
	f.p.restoreTestFinished(ended)
	return f.fresh(row)
}

// testAnswer is an agent's answer of a restore test.
func testAnswer(t *testing.T, result backup.RestoreTestResult) map[string]any {
	t.Helper()
	answer, err := backup.ResultMap(result)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

// TestRestoreTestsStartInTheirSlot (#53, P7, AC2): at the slot of the
// restore tests' schedule, every enabled database job with a snapshot gets
// one test, queued, with its temporary database's record and its command,
// which holds no secret, waits 12 hours, drops the server's earlier copies
// whose tests ended, and orders with the server's other tests; a second pass
// of the slot starts none; a slot missed while the panel was down less than
// a day ago is caught up once.
func TestRestoreTestsStartInTheirSlot(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	f.host.Set("status", schema.HostOnline)
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	blog, _ := f.addDatabase(f.server, "blog", "user-secret-blog", nil)
	blogJob := f.dbJob(blog)
	f.snapshotRun(blogJob, f.target, otherSnapshot, 2048)
	maria := f.sharedServer("maria", databases.MariaDB, "mariadb:11.8", adminSecretMaria)
	mshop, _ := f.addDatabase(maria, "mshop", "user-secret-mshop", nil)
	mshopJob := f.dbJob(mshop)
	f.snapshotRun(mshopJob, f.target, snapshotID, 1024)
	off, _ := f.addDatabase(f.server, "old", "user-secret-old", nil)
	offJob := f.dbJob(off)
	f.snapshotRun(offJob, f.target, snapshotID, 1024)
	f.setTest(offJob, map[string]any{"enabled": false})
	fresh, _ := f.addDatabase(f.server, "fresh", "user-secret-fresh", nil)
	freshJob := f.dbJob(fresh)

	// an earlier test's copy that ended, one whose test runs, one dropping
	ended := f.testRow(f.job, f.target, map[string]any{"status": schema.CheckOK})
	earlier := f.temporary(databases.TestDatabaseName(ended.Id), time.Now().UTC().Add(time.Hour), nil)
	running := f.testRow(mshopJob, f.target, map[string]any{"status": schema.CheckFailed})
	f.temporary(databases.TestDatabaseName(running.Id), time.Now().UTC().Add(time.Hour), map[string]any{"status": dbDropping})

	rule := parseCronRule(farSchedule())
	slot := rule.next(time.Now())
	f.p.RunDueRestoreTests(slot.Add(-time.Minute))
	if rows := f.testsOf(f.job); len(rows) != 1 {
		t.Fatalf("%d tests before the slot", len(rows))
	}
	f.p.RunDueRestoreTests(slot.Add(20 * time.Second))
	for job, want := range map[*core.Record]int{f.job: 2, blogJob: 1, mshopJob: 2, offJob: 0, freshJob: 0} {
		if rows := f.testsOf(job); len(rows) != want {
			t.Errorf("%s: %d tests, not %d", job.GetString("name"), len(rows), want)
		}
	}
	rows := f.testsOf(f.job)
	row := rows[len(rows)-1]
	if row.GetString("status") != schema.CheckQueued || row.GetString("trigger") != schema.RunSchedule || !row.GetDateTime("scheduled_at").Time().Equal(slot) ||
		row.GetString("database_name") != "shop" || row.GetString("snapshot_id") != snapshotID || row.GetString("temporary") != "pc_rt_"+row.Id ||
		row.GetInt("dump_bytes") != 4096 || row.GetString("host") != f.host.Id || row.GetString("target") != f.target.Id || row.GetInt("attempt") != 1 {
		t.Errorf("the test: %v", row.FieldsData())
	}
	copied, err := f.p.databaseOf(f.server, "pc_rt_"+row.Id)
	if err != nil || copied.GetString("status") != dbCreating || copied.GetString("owner_kind") != "none" {
		t.Fatalf("its copy: %v %v", copied, err)
	}
	if left := time.Until(copied.GetDateTime("temporary_until").Time()); left < testCopyLife-time.Minute || left > testCopyLife {
		t.Errorf("its copy goes in %s", left)
	}
	command := f.stored(schema.Commands, row.GetString("command"))
	payload := f.payloadOfTest(command)
	raw := mustString(t, command.Get("payload"))
	if command.GetString("type") != "backup.restore_test" || command.GetString("resource") != "db-test:db-main-db-1" ||
		!slices.Equal(payload.GetDropBefore(), []string{earlier.GetString("name")}) || payload.GetDatabase().GetUser() != "shop" ||
		strings.Contains(raw, adminSecretPG) || strings.Contains(raw, userSecretShop) || strings.Contains(raw, "repo-password") ||
		command.GetString("label") != "Test the restore of shop on db-main" || command.GetString("requested_label") != "schedule" {
		t.Errorf("the command: %v %s", command.FieldsData(), raw)
	}
	if expires := command.GetDateTime("expires_at").Time(); expires.Before(time.Now().Add(restoreTestQueue-time.Minute)) || expires.After(time.Now().Add(restoreTestQueue+time.Minute)) {
		t.Errorf("the test waits until %s", expires)
	}
	mshopRows := f.testsOf(mshopJob)
	blogRows := f.testsOf(blogJob)
	if blogPayload := f.payloadOfTest(f.stored(schema.Commands, blogRows[0].GetString("command"))); !slices.Equal(blogPayload.GetDropBefore(), []string{earlier.GetString("name")}) {
		t.Errorf("blog's test drops %v", blogPayload.GetDropBefore())
	}
	if mshopCommand := f.stored(schema.Commands, mshopRows[len(mshopRows)-1].GetString("command")); mshopCommand.GetString("resource") != "db-test:maria-db-1" {
		t.Errorf("MariaDB's test orders with %q", mshopCommand.GetString("resource"))
	}

	// the tests of a server run one at a time
	stream := f.dbAgent()
	got := []string{f.receivedOf(stream, "backup.restore_test").GetBackupRestoreTest().GetDatabase().GetConnection().GetContainerId(),
		f.receivedOf(stream, "backup.restore_test").GetBackupRestoreTest().GetDatabase().GetConnection().GetContainerId()}
	slices.Sort(got)
	if !slices.Equal(got, []string{"db-main-db-1", "maria-db-1"}) || !f.quiet(stream) {
		t.Errorf("delivered at once: %v", got)
	}

	// once
	f.p.RunDueRestoreTests(slot.Add(40 * time.Second))
	if rows := f.testsOf(blogJob); len(rows) != 1 {
		t.Errorf("%d tests of one slot", len(rows))
	}
}

// TestAMissedSlotOfTheRestoreTestsIsCaughtUp (#53, P7): a slot that came
// while the panel was down, less than a day ago and after the panel began
// to test, is caught up once at the panel's next pass, for that slot; one
// that came while the panel ran, or longer ago, is not.
func TestAMissedSlotOfTheRestoreTestsIsCaughtUp(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	now := time.Now().UTC().Truncate(time.Minute)
	missed := now.Add(-3 * time.Hour)
	f.restoreTestSettings(fmt.Sprintf("%d %d * * *", missed.Minute(), missed.Hour()))
	if _, err := f.app.DB().NewQuery("UPDATE _migrations SET applied={:at} WHERE file={:file}").
		Bind(map[string]any{"at": now.Add(-7 * 24 * time.Hour).UnixMicro(), "file": databasesMigration}).Execute(); err != nil {
		t.Fatal(err)
	}

	f.p.SetStarted(missed.Add(-time.Hour))
	f.p.RunDueRestoreTests(now)
	if rows := f.testsOf(f.job); len(rows) != 0 {
		t.Fatalf("a slot that came while the panel ran was caught up: %d", len(rows))
	}
	f.p.SetStarted(now.Add(-time.Minute))
	f.p.RunDueRestoreTests(now)
	rows := f.testsOf(f.job)
	if len(rows) != 1 || !rows[0].GetDateTime("scheduled_at").Time().Equal(missed) {
		t.Fatalf("the missed slot: %v", rows)
	}
	f.p.RunDueRestoreTests(now.Add(time.Minute))
	if rows := f.testsOf(f.job); len(rows) != 1 {
		t.Errorf("%d tests after a second pass", len(rows))
	}

	// a slot more than a day ago is too late
	weekly := now.Add(-50 * time.Hour)
	f.restoreTestSettings(fmt.Sprintf("%d %d * * %d", weekly.Minute(), weekly.Hour(), int(weekly.Weekday())))
	f.p.RunDueRestoreTests(now.Add(90 * time.Second))
	if rows := f.testsOf(f.job); len(rows) != 1 {
		t.Errorf("a slot two days old was caught up: %d", len(rows))
	}
	f.restoreTestSettings(fmt.Sprintf("%d %d * * *", missed.Minute(), missed.Hour()))

	// before the panel began to test, nothing is caught up
	f.setTest(rows[0], map[string]any{"scheduled_at": ""})
	if _, err := f.app.DB().NewQuery("UPDATE _migrations SET applied={:at} WHERE file={:file}").
		Bind(map[string]any{"at": now.Add(-time.Hour).UnixMicro(), "file": databasesMigration}).Execute(); err != nil {
		t.Fatal(err)
	}
	f.p.RunDueRestoreTests(now.Add(2 * time.Minute))
	if rows := f.testsOf(f.job); len(rows) != 1 {
		t.Errorf("a slot before the tests began was caught up: %d", len(rows))
	}
}

// TestRestoreTestsGoRoundTheTargets (#53, P7): a job's test reads the
// target whose newest test of the job is oldest, never tested first, ties in
// the job's order, and restores that target's newest snapshot.
func TestRestoreTestsGoRoundTheTargets(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	nas := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	f.setTest(f.job, map[string]any{"targets": []string{f.target.Id, nas.Id}})
	f.snapshotRun(f.job, nas, otherSnapshot, 10)
	newest := strings.Repeat("9a8b7c6d", 8)
	f.snapshotRun(f.job, f.target, newest, 20)
	for i, want := range []struct {
		target   *core.Record
		snapshot string
	}{{f.target, newest}, {nas, otherSnapshot}, {f.target, newest}, {nas, otherSnapshot}} {
		row, command := f.startedTest(f.job, schema.RunManual)
		if row.GetString("target") != want.target.Id || row.GetString("snapshot_id") != want.snapshot {
			t.Errorf("test %d: %s %s", i, row.GetString("target"), row.GetString("snapshot_id"))
		}
		f.endTest(row, command, map[string]any{"status": schema.CommandCancelled})
		time.Sleep(5 * time.Millisecond)
	}

	// a slot gets one test of the job, whichever target the next pass
	// would read
	slot := parseCronRule(farSchedule()).next(time.Now())
	f.p.RunDueRestoreTests(slot.Add(10 * time.Second))
	f.p.RunDueRestoreTests(slot.Add(30 * time.Second))
	slotted, err := f.app.FindRecordsByFilter(schema.BackupChecks, "kind={:kind} && job={:job} && scheduled_at!=''", "", 0, 0,
		map[string]any{"kind": schema.CheckRestoreTest, "job": f.job.Id})
	if err != nil || len(slotted) != 1 {
		t.Errorf("%d tests of one slot (%v)", len(slotted), err)
	}
}

// TestARestoreTestThatCannotStartIsSkipped (#53, P7): a job whose server is
// offline at the slot, whose agent cannot test, or whose user's password is
// being rotated gets a skipped test with its reason and an event, tried
// again as a check is, never past the next slot; after its last attempt, the
// job is unverified.
func TestARestoreTestThatCannotStartIsSkipped(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	f.setTest(f.host, map[string]any{"status": schema.HostOnline})
	f.p.SetStarted(time.Now().Add(-3 * time.Minute))
	slot := parseCronRule(farSchedule()).next(time.Now())
	for name, setUp := range map[string]func() func(){
		"The server node-1 was offline at " + slot.Format("15:04") + " UTC.": func() func() {
			f.setTest(f.host, map[string]any{"status": schema.HostOffline})
			return func() { f.setTest(f.host, map[string]any{"status": schema.HostOnline}) }
		},
		"The agent of node-1 cannot test restores yet: update it.": func() func() {
			f.carry(f.host, "backup.run", backup.CapabilityDatabase)
			return func() { f.carry(f.host, "backup.run", "backup.restore_test", backup.CapabilityDatabase) }
		},
		"A rotation of shop's password is under way.": func() func() {
			f.setTest(f.user, map[string]any{"pending_password": "next"})
			return func() { f.setTest(f.user, map[string]any{"pending_password": ""}) }
		},
	} {
		undo := setUp()
		row, err := f.p.startRestoreTest(f.job, testStart{trigger: schema.RunSchedule, scheduledAt: slot, requester: "schedule", now: slot})
		undo()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if row.GetString("status") != schema.CheckSkipped || row.GetString("error") != name || row.GetString("command") != "" ||
			!row.GetDateTime("retry_at").Time().Equal(slot.Add(time.Hour)) {
			t.Errorf("%s: %v", name, row.FieldsData())
		}
		if _, err := f.p.databaseOf(f.server, "pc_rt_"+row.Id); err == nil {
			t.Errorf("%s: a skipped test has a copy", name)
		}
		if err := f.app.Delete(row); err != nil {
			t.Fatal(err)
		}
	}
	if events := f.eventsOf(backup.EventRestoreTestSkipped); len(events) != 3 {
		t.Errorf("%d skipped events", len(events))
	}
	if alert := f.openAlert(backup.AlertRestoreTestUnverified, f.job.Id); alert != nil {
		t.Errorf("an alert while it is tried again: %v", alert.FieldsData())
	}

	// a retry is due: it starts as a test of its own, once
	f.setTest(f.host, map[string]any{"status": schema.HostOffline})
	first, _ := f.p.startRestoreTest(f.job, testStart{trigger: schema.RunSchedule, scheduledAt: slot, requester: "schedule", now: slot})
	f.setTest(f.host, map[string]any{"status": schema.HostOnline})
	f.p.retryDueRestoreTests(slot.Add(time.Hour))
	rows := f.testsOf(f.job)
	retry := rows[len(rows)-1]
	if len(rows) != 2 || retry.GetString("trigger") != schema.RunRetry || retry.GetString("retry_of") != first.Id || retry.GetInt("attempt") != 2 ||
		retry.GetString("status") != schema.CheckQueued || !retry.GetDateTime("scheduled_at").IsZero() || !f.fresh(first).GetDateTime("retry_at").IsZero() {
		t.Errorf("the retry: %v", retry.FieldsData())
	}
	f.p.retryDueRestoreTests(slot.Add(time.Hour))
	if rows := f.testsOf(f.job); len(rows) != 2 {
		t.Errorf("%d tests after a second pass", len(rows))
	}

	// the last attempt leaves the job unverified
	f.setTest(f.host, map[string]any{"status": schema.HostOffline})
	last, err := f.p.startRestoreTest(f.job, testStart{trigger: schema.RunRetry, prev: f.testRow(f.job, f.target, map[string]any{
		"status": schema.CheckSkipped, "attempt": maxCheckAttempts - 1, "retry_at": slot}), requester: "schedule", now: slot})
	if err != nil || last.GetInt("attempt") != maxCheckAttempts || !last.GetDateTime("retry_at").IsZero() {
		t.Fatalf("the last attempt: %v %v", last, err)
	}
	if alert := f.openAlert(backup.AlertRestoreTestUnverified, f.job.Id); alert == nil || alert.GetString("severity") != schema.SeverityWarning ||
		alert.GetString("host") != f.host.Id || !strings.HasPrefix(alert.GetString("message"), "The restore test of shop on db-main was skipped: the server node-1 was offline") {
		t.Errorf("the alert after the last attempt: %v", alert)
	}
}

// TestTheEndOfARestoreTest (#53, P7, AC2): a test that passed is ok, keeps
// its copy two hours, removes the records of the copies it dropped and
// resolves the job's alerts; one whose backup could not be restored failed,
// with the critical alert and event that name the snapshot; one that could
// not tell is in error and tried again, and unverified once it is not; one
// that never started is skipped; one someone stopped is cancelled; the agent
// that made no copy leaves no record of it; nothing kept holds a password.
func TestTheEndOfARestoreTest(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	f.restoreTestSettings("0 4 * * *")
	oldRow := f.testRow(f.job, f.target, map[string]any{"status": schema.CheckOK})
	old := f.temporary(databases.TestDatabaseName(oldRow.Id), time.Now().UTC().Add(time.Hour), nil)

	// failed: the backup could not be restored
	row, command := f.startedTest(f.job, schema.RunSchedule)
	secret := " with " + adminSecretPG + " and " + userSecretShop + " and the-secret-key"
	failed := f.endTest(row, command, map[string]any{"status": schema.CommandFailed, "error": "restic: ciphertext verification failed" + secret,
		"started_at": types.NowDateTime().Add(-time.Minute), "result": testAnswer(t, backup.RestoreTestResult{Failure: backup.TestDamaged,
			Created: true, Dropped: []string{old.GetString("name")}, Log: "the log" + secret})})
	if failed.GetString("status") != schema.CheckFailed || failed.GetString("failure") != backup.TestDamaged || !failed.GetDateTime("retry_at").IsZero() ||
		strings.Contains(failed.GetString("error")+failed.GetString("log"), adminSecretPG) || strings.Contains(failed.GetString("error")+failed.GetString("log"), userSecretShop) ||
		strings.Contains(failed.GetString("error")+failed.GetString("log"), "the-secret-key") {
		t.Errorf("a failed test: %v", failed.FieldsData())
	}
	if _, err := f.app.FindRecordById(schema.Databases, old.Id); err == nil {
		t.Error("a copy the test dropped kept its record")
	}
	if copied, err := f.p.databaseOf(f.server, "pc_rt_"+row.Id); err != nil || copied.GetString("status") != dbReady ||
		time.Until(copied.GetDateTime("temporary_until").Time()) < testCopyLife-time.Minute {
		t.Errorf("a failed test's copy: %v %v", copied, err)
	}
	alert := f.openAlert(backup.AlertRestoreTestFailed, f.job.Id)
	want := "The restore test of shop on db-main failed: Restic: ciphertext verification failed with *** and *** and ***. The snapshot 3f9a1c2e of " +
		f.run.GetDateTime("started_at").Time().UTC().Format("2 Jan 15:04") + " from b2 could not be restored."
	if alert == nil || alert.GetString("severity") != schema.SeverityCritical || alert.GetString("host") != f.host.Id || alert.GetString("message") != want {
		t.Errorf("the alert: %v, not %q", alert, want)
	}
	if events := f.eventsOf(backup.EventRestoreTestFailed); len(events) != 1 || events[0].GetString("severity") != schema.SeverityCritical {
		t.Errorf("the events: %v", events)
	}

	// error: tried again; the failed alert stays critical
	row, command = f.startedTest(f.job, schema.RunSchedule)
	inError := f.endTest(row, command, map[string]any{"status": schema.CommandFailed, "error": "the repository could not be reached",
		"started_at": types.NowDateTime().Add(-time.Minute), "result": testAnswer(t, backup.RestoreTestResult{Failure: backup.TestUnreachable})})
	if inError.GetString("status") != schema.CheckError || inError.GetDateTime("retry_at").IsZero() {
		t.Errorf("a test in error: %v", inError.FieldsData())
	}
	if _, err := f.p.databaseOf(f.server, "pc_rt_"+row.Id); err == nil {
		t.Error("a copy the agent never made kept its record")
	}
	if alert := f.openAlert(backup.AlertRestoreTestFailed, f.job.Id); alert == nil || alert.GetString("severity") != schema.SeverityCritical {
		t.Errorf("the failed alert after an error: %v", alert)
	}
	if f.openAlert(backup.AlertRestoreTestUnverified, f.job.Id) != nil {
		t.Error("an unverified alert while the test is tried again")
	}

	// a test by hand is never tried again: unverified at once
	stream := f.dbAgent()
	status, body := f.send(http.MethodPost, "/api/pc/backup-jobs/"+f.job.Id+"/restore-test", operator, nil)
	if status != http.StatusAccepted {
		t.Fatalf("a test by hand: %d %v", status, body)
	}
	row = f.stored(schema.BackupChecks, body["check"].(string))
	command = f.stored(schema.Commands, row.GetString("command"))
	f.p.Commands.Disconnect(stream)
	manual := f.endTest(row, command, map[string]any{"status": schema.CommandFailed, "error": "the agent restarted",
		"started_at": types.NowDateTime().Add(-time.Minute), "result": testAnswer(t, backup.RestoreTestResult{Failure: backup.TestInterrupted, Created: true})})
	if manual.GetString("status") != schema.CheckError || !manual.GetDateTime("retry_at").IsZero() || manual.GetString("trigger") != schema.RunManual {
		t.Errorf("a test by hand in error: %v", manual.FieldsData())
	}
	if alert := f.openAlert(backup.AlertRestoreTestUnverified, f.job.Id); alert == nil || alert.GetString("severity") != schema.SeverityWarning {
		t.Errorf("the unverified alert: %v", alert)
	}

	// never started, stopped
	row, command = f.startedTest(f.job, schema.RunSchedule)
	closed := f.fresh(command)
	closed.Set("expires_at", types.NowDateTime().Add(-time.Minute))
	if err := f.app.Save(closed); err != nil {
		t.Fatal(err)
	}
	if skipped := f.endTest(row, command, map[string]any{"status": schema.CommandTimedOut}); skipped.GetString("status") != schema.CheckSkipped ||
		!strings.HasPrefix(skipped.GetString("error"), "It waited 12 hours while the server node-1 ") || skipped.GetDateTime("retry_at").IsZero() {
		t.Errorf("a test that never started: %v", skipped.FieldsData())
	}
	if _, err := f.p.databaseOf(f.server, "pc_rt_"+row.Id); err == nil {
		t.Error("a test that never started kept its copy's record")
	}
	row, command = f.startedTest(f.job, schema.RunManual)
	if cancelled := f.endTest(row, command, map[string]any{"status": schema.CommandCancelled, "started_at": types.NowDateTime().Add(-time.Minute)}); cancelled.GetString("status") != schema.CheckCancelled {
		t.Errorf("a stopped test: %v", cancelled.FieldsData())
	}

	// passed: the alerts go
	row, command = f.startedTest(f.job, schema.RunSchedule)
	passed := f.endTest(row, command, map[string]any{"status": schema.CommandSucceeded, "started_at": types.NowDateTime().Add(-time.Minute),
		"result": testAnswer(t, backup.RestoreTestResult{Status: backup.RestoreTestOK, Tables: 12, TablesExpected: 12, Bytes: 5000, Created: true})})
	if passed.GetString("status") != schema.CheckOK || passed.GetInt("tables_restored") != 12 || passed.GetInt("tables_expected") != 12 ||
		passed.GetInt("dump_bytes") != 5000 || passed.GetInt("duration_ms") < 59000 || passed.GetString("error") != "" {
		t.Errorf("a passed test: %v", passed.FieldsData())
	}
	if f.openAlert(backup.AlertRestoreTestFailed, f.job.Id) != nil || f.openAlert(backup.AlertRestoreTestUnverified, f.job.Id) != nil {
		t.Error("a passed test left an alert open")
	}
	if events := f.eventsOf(backup.EventRestoreTestOK); len(events) != 1 ||
		events[0].GetString("message") != "The restore test of shop on db-main passed: 12 tables restored, as many as the dump has." {
		t.Errorf("the events: %v", events)
	}
	// once: the command settled again
	f.p.restoreTestFinished(f.fresh(command))
	if events := f.eventsOf(backup.EventRestoreTestOK); len(events) != 1 {
		t.Errorf("%d ok events", len(events))
	}
}

// TestRetriesOfRestoreTestsComeBeforeTheNextSlot (#53, P7): a test is not
// tried again past the next slot of the schedule, which tests it anyway.
func TestRetriesOfRestoreTestsComeBeforeTheNextSlot(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	next := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Minute)
	f.restoreTestSettings(fmt.Sprintf("%d %d * * *", next.Minute(), next.Hour()))
	row, command := f.startedTest(f.job, schema.RunSchedule)
	if kept := f.endTest(row, command, map[string]any{"status": schema.CommandFailed, "error": "unreachable", "started_at": types.NowDateTime().Add(-time.Minute),
		"result": testAnswer(t, backup.RestoreTestResult{Failure: backup.TestUnreachable})}); !kept.GetDateTime("retry_at").IsZero() {
		t.Errorf("a retry past the next slot: %v", kept.FieldsData())
	}
}

// TestRestoreTestAlertsGoWithTheirJob (#53, P7): a job's test alerts are
// resolved when the job is removed, turned off or its database dropped, and
// the unverified ones when the tests are turned off, with their retries.
func TestRestoreTestAlertsGoWithTheirJob(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	raise := func(job *core.Record) {
		t.Helper()
		for _, rule := range []string{backup.AlertRestoreTestFailed, backup.AlertRestoreTestUnverified} {
			if err := f.p.App.RunInTransaction(func(tx core.App) error {
				return eventsRaise(tx, rule, job)
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	open := func(job *core.Record) int {
		t.Helper()
		count := 0
		for _, rule := range []string{backup.AlertRestoreTestFailed, backup.AlertRestoreTestUnverified} {
			if f.openAlert(rule, job.Id) != nil {
				count++
			}
		}
		return count
	}
	raise(f.job)
	f.setTest(f.job, map[string]any{"name": "renamed"})
	if open(f.job) != 2 {
		t.Error("a change of a job's name resolved its alerts")
	}
	f.setTest(f.job, map[string]any{"enabled": false})
	if open(f.job) != 0 {
		t.Error("a job turned off kept its alerts")
	}
	f.setTest(f.job, map[string]any{"enabled": true})
	raise(f.job)
	f.setTest(f.job, map[string]any{"database": ""})
	if open(f.job) != 0 {
		t.Error("a job whose database was dropped kept its alerts")
	}
	raise(f.job)
	if err := f.app.Delete(f.job); err != nil {
		t.Fatal(err)
	}
	if open(f.job) != 0 {
		t.Error("a removed job kept its alerts")
	}

	// tests turned off: the unverified alerts and the retries
	blog, _ := f.addDatabase(f.server, "blog", "user-secret-blog", nil)
	blogJob := f.dbJob(blog)
	raise(blogJob)
	retry := f.testRow(blogJob, f.target, map[string]any{"status": schema.CheckError, "retry_at": time.Now().UTC().Add(time.Hour)})
	status, body := f.send(http.MethodPut, "/api/pc/settings/backup-checks", f.admin, map[string]any{"restore_test_schedule": ""})
	if status != http.StatusOK || body["restore_test_schedule"] != "" || body["next_restore_test_at"] != "" {
		t.Fatalf("tests off: %d %v", status, body)
	}
	if f.openAlert(backup.AlertRestoreTestUnverified, blogJob.Id) != nil || f.openAlert(backup.AlertRestoreTestFailed, blogJob.Id) == nil {
		t.Error("turning the tests off resolved the wrong alerts")
	}
	if !f.fresh(retry).GetDateTime("retry_at").IsZero() {
		t.Error("turning the tests off kept a retry")
	}
}

// eventsRaise raises an alert of a job's tests, as a test would.
func eventsRaise(tx core.App, rule string, job *core.Record) error {
	return events.RaiseAlert(tx, rule, job.Id, job.GetString("host"), schema.SeverityCritical, "raised by the test")
}

// TestTheRestoreTestWatchdog (#53, P7): a job with snapshots whose tests
// have not passed for twice the longest gap of the schedule and a day is
// unverified; not one whose test passed lately, one whose test failed, or
// any while the tests are off.
func TestTheRestoreTestWatchdog(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	f.restoreTestSettings("20 5 * * 3")
	if _, err := f.app.DB().NewQuery("UPDATE _migrations SET applied={:at} WHERE file={:file}").
		Bind(map[string]any{"at": time.Now().Add(-90 * 24 * time.Hour).UnixMicro(), "file": databasesMigration}).Execute(); err != nil {
		t.Fatal(err)
	}
	aged := func(record *core.Record, collection string, age time.Duration) {
		t.Helper()
		if _, err := f.app.DB().NewQuery("UPDATE " + collection + " SET created={:at} WHERE id={:id}").
			Bind(map[string]any{"at": types.NowDateTime().Add(-age).String(), "id": record.Id}).Execute(); err != nil {
			t.Fatal(err)
		}
	}
	aged(f.run, schema.BackupRuns, 30*24*time.Hour)
	f.p.WatchRestoreTests(time.Now())
	alert := f.openAlert(backup.AlertRestoreTestUnverified, f.job.Id)
	if alert == nil || alert.GetString("severity") != schema.SeverityWarning ||
		!strings.HasPrefix(alert.GetString("message"), "No restore test of shop on db-main has passed since ") {
		t.Fatalf("the watchdog's alert: %v", alert)
	}
	if err := events.ResolveAlert(f.app, backup.AlertRestoreTestUnverified, f.job.Id); err != nil {
		t.Fatal(err)
	}

	// a test that passed lately
	f.testRow(f.job, f.target, map[string]any{"status": schema.CheckOK, "finished_at": time.Now().UTC().Add(-24 * time.Hour)})
	f.p.WatchRestoreTests(time.Now())
	if alert := f.openAlert(backup.AlertRestoreTestUnverified, f.job.Id); alert != nil {
		t.Errorf("an alert after a test passed lately: %v", alert.FieldsData())
	}

	// a job whose last test failed has its alert already
	blog, _ := f.addDatabase(f.server, "blog", "user-secret-blog", nil)
	blogJob := f.dbJob(blog)
	aged(f.snapshotRun(blogJob, f.target, snapshotID, 1), schema.BackupRuns, 30*24*time.Hour)
	if err := eventsRaise(f.app, backup.AlertRestoreTestFailed, blogJob); err != nil {
		t.Fatal(err)
	}
	f.p.WatchRestoreTests(time.Now())
	if alert := f.openAlert(backup.AlertRestoreTestUnverified, blogJob.Id); alert != nil {
		t.Errorf("an unverified alert beside a failed one: %v", alert.FieldsData())
	}

	// a job without snapshots
	fresh, _ := f.addDatabase(f.server, "fresh", "user-secret-fresh", nil)
	freshJob := f.dbJob(fresh)
	f.p.WatchRestoreTests(time.Now())
	if alert := f.openAlert(backup.AlertRestoreTestUnverified, freshJob.Id); alert != nil {
		t.Errorf("an alert of a job without snapshots: %v", alert.FieldsData())
	}

	// the tests off
	if err := events.ResolveAlert(f.app, backup.AlertRestoreTestFailed, blogJob.Id); err != nil {
		t.Fatal(err)
	}
	f.restoreTestSettings("")
	f.p.WatchRestoreTests(time.Now())
	if alert := f.openAlert(backup.AlertRestoreTestUnverified, blogJob.Id); alert != nil {
		t.Errorf("an alert while the tests are off: %v", alert.FieldsData())
	}
}

// TestTestingARestoreNow (#53, P9): an operator tests a database job's
// restore now, 202 with the test, which is never tried again; it is refused
// with a sentence for a job of anything else, a dropped database, a job
// without a backup, a test or a restore under way, an offline server, an
// agent that cannot test, and a rotation of the user's password.
func TestTestingARestoreNow(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	operator, token := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	_, viewer := testutil.NewUser(t, f.app, "viewer@example.com", schema.RoleViewer)
	stream := f.dbAgent()
	test := func(job *core.Record, token string) (int, map[string]any) {
		t.Helper()
		return f.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/restore-test", token, nil)
	}
	expect := func(name string, job *core.Record, wantStatus int, want string) {
		t.Helper()
		if status, body := test(job, token); status != wantStatus || body["message"] != want {
			t.Errorf("%s: %d %v, not %d %q", name, status, body["message"], wantStatus, want)
		}
	}
	if status, _ := test(f.job, viewer); status != http.StatusForbidden {
		t.Errorf("a viewer's test: %d", status)
	}
	volume := f.jobFixture.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	expect("a volume's job", volume, http.StatusBadRequest, "Only a database's job has restore tests.")
	fresh, _ := f.addDatabase(f.server, "fresh", "user-secret-fresh", nil)
	expect("no backup", f.dbJob(fresh), http.StatusConflict, "No backup of fresh has succeeded yet.")
	f.setTest(f.database, map[string]any{"status": dbCreating})
	expect("a database not ready", f.job, http.StatusConflict, "The database shop is not ready.")
	f.setTest(f.database, map[string]any{"status": dbReady})
	f.setTest(f.server, map[string]any{"status": dbCreating})
	expect("a server not ready", f.job, http.StatusConflict, "The database server db-main is not ready.")
	f.setTest(f.server, map[string]any{"status": dbReady})

	f.setTest(f.user, map[string]any{"pending_password": "next"})
	expect("a rotation", f.job, http.StatusConflict, "A rotation of shop's password is under way.")
	f.setTest(f.user, map[string]any{"pending_password": ""})
	f.carry(f.host, "backup.run", backup.CapabilityDatabase)
	expect("an old agent", f.job, http.StatusConflict, "The agent of node-1 cannot test restores yet: update it.")
	f.carry(f.host, "backup.run", "backup.restore_test", backup.CapabilityDatabase)
	restore := f.restore(f.job, map[string]any{"mode": schema.RestoreDatabase, "status": schema.RestoreRunning,
		"roots": []restoreRoot{{Source: "/shop.pgdump", Container: "db-main-db-1", Database: "shop", User: "shop"}}})
	expect("a restore", f.job, http.StatusConflict, "A restore of shop is under way.")
	f.setTest(restore, map[string]any{"status": schema.RestoreSucceeded})

	status, body := test(f.job, token)
	if status != http.StatusAccepted {
		t.Fatalf("a test by hand: %d %v", status, body)
	}
	row := f.stored(schema.BackupChecks, body["check"].(string))
	command := f.stored(schema.Commands, row.GetString("command"))
	if row.GetString("trigger") != schema.RunManual || !row.GetDateTime("scheduled_at").IsZero() || body["command"] != command.Id ||
		command.GetString("requested_by") != operator.Id {
		t.Errorf("the test: %v %v", row.FieldsData(), command.FieldsData())
	}
	if delivered := f.receivedOf(stream, "backup.restore_test"); delivered.GetId() != command.Id ||
		delivered.GetBackupRestoreTest().GetDatabase().GetPassword() != userSecretShop {
		t.Errorf("delivered: %v", delivered)
	}
	expect("a test under way", f.job, http.StatusConflict, "A restore test of shop is under way.")
	f.report(f.host, command.Id, 1, agentv1.CommandState_COMMAND_STATE_RUNNING, nil, nil, "")
	if running := f.fresh(row); running.GetString("status") != schema.CheckRunning || running.GetDateTime("started_at").IsZero() {
		t.Errorf("a test its agent started: %v", running.FieldsData())
	}

	f.p.Commands.Disconnect(stream)
	f.setTest(row, map[string]any{"status": schema.CheckOK})
	expect("an offline server", f.job, http.StatusConflict, "The server node-1 is offline: its agent runs the test.")
	f.setTest(f.job, map[string]any{"database": ""})
	expect("a dropped database", f.job, http.StatusConflict, "The database of this job was dropped.")
}

// TestTheRestoreTestScheduleIsHeldToItsRules (#53, P7, P9): admins set when
// the restore tests run, through the checks' route or the record API, which
// hold it to the checks' rules: cron in UTC, at most once a day.
func TestTheRestoreTestScheduleIsHeldToItsRules(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	if status, _ := f.send(http.MethodPut, "/api/pc/settings/backup-checks", operator, map[string]any{"restore_test_schedule": "0 3 * * *"}); status != http.StatusForbidden {
		t.Errorf("an operator set the tests: %d", status)
	}
	for schedule, want := range map[string]string{"*/30 * * * *": checkOftenProblem, "every day": checkScheduleProblem, "0 0 30 2 *": checkNeverProblem} {
		if status, body := f.send(http.MethodPut, "/api/pc/settings/backup-checks", f.admin, map[string]any{"restore_test_schedule": schedule}); status != http.StatusBadRequest ||
			body["message"] != want {
			t.Errorf("%q: %d %v", schedule, status, body["message"])
		}
	}
	status, body := f.send(http.MethodPut, "/api/pc/settings/backup-checks", f.admin, map[string]any{"restore_test_schedule": "20  5 * * 3"})
	next := parseCronRule("20 5 * * 3").next(time.Now()).Format(time.RFC3339)
	if status != http.StatusOK || body["restore_test_schedule"] != "20 5 * * 3" || body["next_restore_test_at"] != next {
		t.Errorf("set: %d %v", status, body)
	}
	settings, err := f.p.settingsRecord()
	if err != nil {
		t.Fatal(err)
	}
	status, body = f.send(http.MethodPatch, "/api/collections/settings/records/"+settings.Id, f.admin, map[string]any{"restore_test_schedule": "0 * * * *"})
	if status != http.StatusBadRequest || body["message"] != checkOftenProblem {
		t.Errorf("through the record API: %d %v", status, body["message"])
	}
}

// TestARestoreTestThePanelMissedIsSettled (#53, P7): a test whose command
// ended without its finisher, as when the panel stopped in between, is kept
// from its command at the scheduler's next pass; one whose command is gone
// did not complete, is tried again, and keeps its copy two hours.
func TestARestoreTestThePanelMissedIsSettled(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	row, command := f.startedTest(f.job, schema.RunSchedule)
	f.ended(command, schema.CommandSucceeded, "", testAnswer(t, backup.RestoreTestResult{Status: backup.RestoreTestOK, Tables: 3, TablesExpected: 3, Created: true}))
	blog, _ := f.addDatabase(f.server, "blog", "user-secret-blog", nil)
	blogJob := f.dbJob(blog)
	f.snapshotRun(blogJob, f.target, snapshotID, 1)
	lostRow, lostCommand := f.startedTest(blogJob, schema.RunManual)
	if err := f.app.Delete(f.fresh(lostCommand)); err != nil {
		t.Fatal(err)
	}
	f.p.RunDueRestoreTests(time.Now())
	if kept := f.fresh(row); kept.GetString("status") != schema.CheckOK || kept.GetInt("tables_restored") != 3 {
		t.Errorf("a test whose command ended: %v", kept.FieldsData())
	}
	lost := f.fresh(lostRow)
	if lost.GetString("status") != schema.CheckError || lost.GetString("error") != "The restore test ended without the panel recording how." ||
		lost.GetDateTime("finished_at").IsZero() {
		t.Errorf("a test whose command is gone: %v", lost.FieldsData())
	}
	if copied, err := f.p.databaseOf(f.server, "pc_rt_"+lostRow.Id); err != nil || copied.GetString("status") != dbReady {
		t.Errorf("its copy: %v %v", copied, err)
	}
	if events := f.eventsOf(backup.EventRestoreTestError); len(events) != 1 {
		t.Errorf("%d error events", len(events))
	}
	f.p.RunDueRestoreTests(time.Now())
	if events := f.eventsOf(backup.EventRestoreTestError); len(events) != 1 {
		t.Errorf("%d error events after a second pass", len(events))
	}
}

// TestRetriesOfRestoreTestsWait (#53, P7): a due retry waits an hour while
// its server is offline, and goes when the job is off, its database dropped
// or a test of it is under way.
func TestRetriesOfRestoreTestsWait(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	now := time.Now().UTC().Truncate(time.Minute)
	due := func() *core.Record {
		t.Helper()
		return f.testRow(f.job, f.target, map[string]any{"status": schema.CheckError, "attempt": 1, "retry_at": now.Add(-time.Minute)})
	}
	f.setTest(f.host, map[string]any{"status": schema.HostOffline})
	f.p.SetStarted(now.Add(-time.Hour))
	offline := due()
	f.p.retryDueRestoreTests(now)
	if kept := f.fresh(offline); !kept.GetDateTime("retry_at").Time().Equal(now.Add(time.Hour)) {
		t.Errorf("a retry of an offline server: %v", kept.FieldsData())
	}
	f.setTest(offline, map[string]any{"retry_at": ""})
	f.setTest(f.host, map[string]any{"status": schema.HostOnline})

	open := f.testRow(f.job, f.target, map[string]any{"status": schema.CheckRunning})
	busy := due()
	f.p.retryDueRestoreTests(now)
	if kept := f.fresh(busy); !kept.GetDateTime("retry_at").IsZero() {
		t.Errorf("a retry while a test runs: %v", kept.FieldsData())
	}
	f.setTest(open, map[string]any{"status": schema.CheckOK})

	f.setTest(f.job, map[string]any{"enabled": false})
	disabled := due()
	f.p.retryDueRestoreTests(now)
	if kept := f.fresh(disabled); !kept.GetDateTime("retry_at").IsZero() {
		t.Errorf("a retry of a job turned off: %v", kept.FieldsData())
	}
	f.setTest(f.job, map[string]any{"enabled": true, "database": ""})
	dropped := due()
	f.p.retryDueRestoreTests(now)
	if kept := f.fresh(dropped); !kept.GetDateTime("retry_at").IsZero() {
		t.Errorf("a retry of a dropped database: %v", kept.FieldsData())
	}
	if rows := f.testsOf(f.job); len(rows) != 5 {
		t.Errorf("%d tests: a retry started", len(rows))
	}
}
