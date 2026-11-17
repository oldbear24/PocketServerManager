package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// newRetryFixture is a fixture of jobs whose server is online and connected,
// and a job of a volume that never runs by itself.
func newRetryFixture(t *testing.T) (*jobFixture, *core.Record, *commands.Stream) {
	t.Helper()
	f := newJobFixture(t)
	f.host.Set("status", schema.HostOnline)
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	stream := f.p.Commands.Connect(f.host.Id, []string{"backup.run", "backup.check", "backup.init"}, nil)
	t.Cleanup(func() { f.p.Commands.Disconnect(stream) })
	return f, f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"}), stream
}

// fresh reads a record again.
func (f *jobFixture) fresh(record *core.Record) *core.Record {
	f.t.Helper()
	stored, err := f.app.FindRecordById(record.Collection().Name, record.Id)
	if err != nil {
		f.t.Fatalf("%s %s: %v", record.Collection().Name, record.Id, err)
	}
	return stored
}

// openAlert is the open alert of a rule and key, if any.
func (f *jobFixture) openAlert(rule, key string) *core.Record {
	f.t.Helper()
	alerts, err := f.app.FindRecordsByFilter(schema.Alerts, "rule={:rule} && target={:key} && status!='resolved'", "", 0, 0,
		map[string]any{"rule": rule, "key": key})
	if err != nil || len(alerts) > 1 {
		f.t.Fatalf("%d alerts %s %s: %v", len(alerts), rule, key, err)
	}
	if len(alerts) == 0 {
		return nil
	}
	return alerts[0]
}

// eventsOf are the events of a kind, oldest first.
func (f *jobFixture) eventsOf(kind string) []*core.Record {
	f.t.Helper()
	found, err := f.app.FindRecordsByFilter(schema.Events, "kind={:kind}", "created", 0, 0, map[string]any{"kind": kind})
	if err != nil {
		f.t.Fatal(err)
	}
	return found
}

// closeCommand moves a stored command to a final state through the queue's
// states.
func (f *jobFixture) closeCommand(command *core.Record, status string) {
	f.t.Helper()
	stored := f.fresh(command)
	steps := []string{status}
	if status == schema.CommandSucceeded {
		steps = []string{schema.CommandDispatched, schema.CommandRunning, status}
	}
	for _, step := range steps {
		stored.Set("status", step)
		if err := f.app.Save(stored); err != nil {
			f.t.Fatalf("command to %s: %v", step, err)
		}
	}
}

// endRun ends a run's command as the fields say, as the agent and the queue
// would, and is the run as kept. A run without a command gets one.
func (f *jobFixture) endRun(run *core.Record, fields map[string]any) *core.Record {
	f.t.Helper()
	var command *core.Record
	if id := run.GetString("command"); id != "" {
		stored, err := f.app.FindRecordById(schema.Commands, id)
		if err != nil {
			f.t.Fatal(err)
		}
		command = stored
	} else {
		command = f.save(schema.Commands, map[string]any{"host": f.host.Id, "type": "backup.run",
			"payload": map[string]any{"run_id": run.Id, "repository": map[string]any{"target_id": run.GetString("target"), "url": "s3:x"}}})
		run.Set("command", command.Id)
		if err := f.app.Save(run); err != nil {
			f.t.Fatal(err)
		}
	}
	status, _ := fields["status"].(string)
	if status != "" && !commandFinal(command) {
		f.closeCommand(command, status)
	}
	command.Load(map[string]any{"finished_at": types.NowDateTime()})
	command.Load(fields)
	f.p.backupRunFinished(command)
	return f.fresh(run)
}

// failRun makes a run of a job that started and failed with the agent's
// answer.
func (f *jobFixture) failRun(job *core.Record, message string, result map[string]any) *core.Record {
	f.t.Helper()
	return f.endRun(f.run(job), map[string]any{"status": schema.CommandFailed, "error": message, "result": result,
		"started_at": types.NowDateTime().Add(-time.Minute)})
}

// lockAnswer is a new agent's answer of a run that a lock kept out.
func lockAnswer(lock backup.Lock) map[string]any {
	raw, _ := json.Marshal(backup.RunFailure{Failure: backup.FailureLocked, ResticCode: 11, Lock: &lock})
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	out["log"] = "restic said so"
	return out
}

// runsOf are a job's runs to the fixture's target, oldest first.
func (f *jobFixture) runsOf(job *core.Record) []*core.Record {
	f.t.Helper()
	runs, err := f.app.FindRecordsByFilter(schema.BackupRuns, "job={:job}", "created,id", 0, 0, map[string]any{"job": job.Id})
	if err != nil {
		f.t.Fatal(err)
	}
	return runs
}

// payloadOf is a command's payload.
func (f *jobFixture) payloadOf(commandID string) map[string]any {
	f.t.Helper()
	command, err := f.app.FindRecordById(schema.Commands, commandID)
	if err != nil {
		f.t.Fatal(err)
	}
	payload := map[string]any{}
	raw, _ := json.Marshal(command.Get("payload"))
	_ = json.Unmarshal(raw, &payload)
	return payload
}

// TestWhichFailuresAreTriedAgain (#31, AC2): a run that started and failed in
// a way that may pass by itself (a lock, restic, an agent that restarted) is
// tried again 5 minutes later, a lock no sooner than restic counts it as
// stale; the others are not, nor is a run that never started, was skipped or
// cancelled. A failure is classed by the agent's answer, and an older agent's
// by its error.
func TestWhichFailuresAreTriedAgain(t *testing.T) {
	f, job, _ := newRetryFixture(t)
	now := time.Now().UTC()
	young := now.Add(-2 * time.Minute)
	for _, test := range []struct {
		name, message string
		result        map[string]any
		fields        map[string]any
		failure       string
		status        string
		// wait is how long after its end the run is tried again; 0 for not
		wait  time.Duration
		after time.Time
		lock  string
	}{
		{name: "locked", message: "unable to create lock in backend: repository is already locked by PID 7 on nas by root (UID 0, GID 0)",
			result:  lockAnswer(backup.Lock{PID: 7, Host: "nas", User: "root", Created: now.Add(-40 * time.Minute), AgeS: 2400, StorageID: "957548b2"}),
			failure: backup.FailureLocked, wait: 5 * time.Minute, lock: `"storage_id":"957548b2"`},
		{name: "locked by a young lock", message: "unable to create lock in backend: repository is already locked by PID 7 on nas by root (UID 0, GID 0)",
			result:  lockAnswer(backup.Lock{PID: 7, Host: "nas", User: "root", Created: young, AgeS: 120}),
			failure: backup.FailureLocked, after: young.Add(32 * time.Minute)},
		{name: "restic", message: "Fatal: unable to save snapshot: connection reset", result: map[string]any{"failure": "restic", "restic_code": 1},
			failure: backup.FailureRestic, wait: 5 * time.Minute},
		{name: "the agent restarted", message: "The agent restarted while running this command, so it may be incomplete.",
			failure: backup.FailureInterrupted, wait: 5 * time.Minute},
		{name: "an unreadable lock", message: "Fatal: invalid lock file", result: map[string]any{"failure": "lock_invalid", "restic_code": 1},
			failure: backup.FailureLockInvalid},
		{name: "no repository", message: "the repository is gone, with its snapshots: Fatal: …", result: map[string]any{"failure": "repository", "restic_code": 10},
			failure: backup.FailureRepository},
		{name: "a wrong password", message: "a repository is there, and the password is not its own", result: map[string]any{"failure": "password", "restic_code": 12},
			failure: backup.FailurePassword},
		{name: "the source", message: "the directory /srv/x does not exist on this server", result: map[string]any{"failure": "source"},
			failure: backup.FailureSource},
		{name: "some files not read", message: "restic could not read 2 files", result: map[string]any{"snapshot_id": "4f2a", "failure": "restic"},
			failure: backup.FailurePartial},
		{name: "out of time", fields: map[string]any{"status": schema.CommandTimedOut, "started_at": types.NowDateTime().Add(-time.Minute), "error": "The agent did not finish the command in time."},
			failure: backup.FailureTimeout},
		{name: "never started", fields: map[string]any{"status": schema.CommandFailed, "error": "This agent does not support backup.run. Update the agent."}},
		{name: "never delivered", fields: map[string]any{"status": schema.CommandTimedOut, "error": "The server stayed offline, so the command was never delivered."},
			status: schema.RunSkipped},
		{name: "cancelled", fields: map[string]any{"status": schema.CommandCancelled, "started_at": types.NowDateTime().Add(-time.Minute), "error": "Cancelled by admin@example.com"},
			status: schema.RunCancelled},
		// older agents
		{name: "an older agent's lock", message: "unable to create lock in backend: repository is already locked exclusively by PID 7 on nas by root (UID 0, GID 0)",
			failure: backup.FailureLocked, wait: 5 * time.Minute, lock: `"exclusive":true`},
		{name: "an older agent's unreadable lock", message: "Fatal: invalid lock file", failure: backup.FailureLockInvalid},
		{name: "an older agent's gone repository", message: "the repository is gone, with its snapshots: Fatal: …", failure: backup.FailureRepository},
		{name: "an older agent's password", message: "a repository is there, and the password is not its own: …", failure: backup.FailurePassword},
		{name: "an older agent's directory", message: "the directory /srv/x does not exist on this server", failure: backup.FailureSource},
		{name: "an older agent's dump", message: "the dump failed with exit code 1, and restic saved no snapshot: …", failure: backup.FailureSource},
		{name: "an older agent's restic", message: "Fatal: unable to save snapshot", failure: backup.FailureRestic, wait: 5 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			var run *core.Record
			if test.fields != nil {
				run = f.endRun(f.run(job), test.fields)
			} else {
				run = f.failRun(job, test.message, test.result)
			}
			status := test.status
			if status == "" {
				status = schema.RunFailed
			}
			finished := run.GetDateTime("finished_at").Time()
			retry := run.GetDateTime("retry_at").Time()
			want := time.Time{}
			switch {
			case test.wait > 0:
				want = finished.Add(test.wait)
			case !test.after.IsZero():
				want = test.after
			}
			if run.GetString("status") != status || run.GetString("failure") != test.failure || retry.IsZero() != want.IsZero() || !near(retry, want) ||
				!strings.Contains(run.GetString("lock"), test.lock) {
				t.Errorf("%v: retry %s, not %s", run.FieldsData(), retry, want)
			}
		})
	}

	// a run whose command could not be made: no class, no retry
	stack := f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "shop"})
	empty := f.job("shop", map[string]any{"source_kind": schema.SourceStack, "stack": stack.Id})
	run, err := f.p.startRun(t.Context(), empty, f.target, f.host, schema.RunManual, time.Time{}, commands.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if run = f.fresh(run); run.GetString("status") != schema.RunFailed || run.GetString("failure") != "" || !run.GetDateTime("retry_at").IsZero() ||
		run.GetString("command") != "" || run.GetString("error") != "The stack shop has no volumes to back up." {
		t.Errorf("a run whose command could not be made: %v", run.FieldsData())
	}
	if a := f.openAlert(alertBackupFailed, empty.Id+":"+f.target.Id); a == nil || a.GetString("severity") != schema.SeverityCritical {
		t.Errorf("its alert: %v", a)
	}
}

// TestARunIsTriedAgainThreeTimes (#31, AC2): a failed run is tried again after
// 5 minutes, 30 minutes and 2 hours, by runs of their own (trigger retry, the
// next attempt, retry_of), each claimed with its command; the fourth attempt
// is the last. The alert is a warning that says when PocketCloud tries again
// while it does, critical once it does not, and a success resolves it.
func TestARunIsTriedAgainThreeTimes(t *testing.T) {
	f, job, _ := newRetryFixture(t)
	key := job.Id + ":" + f.target.Id
	run := f.failRun(job, "Fatal: unable to save snapshot: connection reset", map[string]any{"failure": "restic", "restic_code": 1})
	for attempt, wait := range []time.Duration{5 * time.Minute, 30 * time.Minute, 2 * time.Hour} {
		retryAt := run.GetDateTime("retry_at").Time()
		if want := run.GetDateTime("finished_at").Time().Add(wait); !near(retryAt, want) {
			t.Fatalf("attempt %d is tried again at %s, not %s", attempt+1, retryAt, want)
		}
		a := f.openAlert(alertBackupFailed, key)
		if a == nil || a.GetString("severity") != schema.SeverityWarning || !strings.HasSuffix(a.GetString("message"),
			fmt.Sprintf(" PocketCloud tries again at %s UTC (attempt %d of 4).", retryAt.UTC().Format("15:04"), attempt+2)) {
			t.Fatalf("the alert of attempt %d: %v", attempt+1, a)
		}
		// not yet
		f.p.RunDueBackups(retryAt.Add(-time.Minute))
		if runs := f.runsOf(job); len(runs) != attempt+1 {
			t.Fatalf("%d runs before the retry is due", len(runs))
		}
		f.p.RunDueBackups(retryAt.Add(time.Minute))
		runs := f.runsOf(job)
		if len(runs) != attempt+2 {
			t.Fatalf("%d runs once the retry of attempt %d is due", len(runs), attempt+1)
		}
		retry := runs[attempt+1]
		if retry.GetString("trigger") != schema.RunRetry || retry.GetInt("attempt") != attempt+2 || retry.GetString("retry_of") != run.Id ||
			retry.GetString("status") != schema.RunQueued || retry.GetString("command") == "" || retry.GetString("host") != f.host.Id {
			t.Fatalf("the retry: %v", retry.FieldsData())
		}
		if payload := f.payloadOf(retry.GetString("command")); payload["run_id"] != retry.Id || payload["unlock"] == true {
			t.Errorf("the retry's command: %v", payload)
		}
		if prev := f.fresh(run); !prev.GetDateTime("retry_at").IsZero() {
			t.Errorf("the run tried again still has its retry: %v", prev.FieldsData())
		}
		run = f.endRun(retry, map[string]any{"status": schema.CommandFailed, "error": "Fatal: unable to save snapshot: connection reset",
			"started_at": types.NowDateTime(), "result": map[string]any{"failure": "restic", "restic_code": 1}})
	}
	if run.GetInt("attempt") != 4 || !run.GetDateTime("retry_at").IsZero() {
		t.Errorf("the last attempt: %v", run.FieldsData())
	}
	if a := f.openAlert(alertBackupFailed, key); a == nil || a.GetString("severity") != schema.SeverityCritical ||
		a.GetString("message") != "Backup data → b2 failed: Fatal: unable to save snapshot: connection reset." {
		t.Errorf("the alert once no retry is left: %v", a)
	}
	// the next failure that is tried again leaves the alert critical
	f.failRun(job, "Fatal: unable to save snapshot", map[string]any{"failure": "restic"})
	if a := f.openAlert(alertBackupFailed, key); a == nil || a.GetString("severity") != schema.SeverityCritical ||
		!strings.Contains(a.GetString("message"), "PocketCloud tries again at") {
		t.Errorf("a critical alert went down: %v", a)
	}
	if found := f.eventsOf("backup.failed"); len(found) != 5 || found[0].GetString("severity") != schema.SeverityWarning ||
		found[3].GetString("severity") != schema.SeverityCritical || found[4].GetString("severity") != schema.SeverityWarning {
		t.Errorf("%d events", len(found))
	}
	f.endRun(f.run(job), map[string]any{"status": schema.CommandSucceeded, "started_at": types.NowDateTime(), "result": map[string]any{"snapshot_id": "4f2a"}})
	if f.openAlert(alertBackupFailed, key) != nil {
		t.Error("a success did not resolve the alert")
	}
}

// TestTheNextRunTakesARetrysPlace (#31): a retry that would come no more than
// 10 minutes before the job's next scheduled run is not planned, and a planned
// one whose job's next run comes as close is given up, saying so.
func TestTheNextRunTakesARetrysPlace(t *testing.T) {
	f, _, _ := newRetryFixture(t)
	soon := time.Now().UTC().Add(12 * time.Minute)
	job := f.job("soon", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data", "schedule": fmt.Sprintf("%d %d * * *", soon.Minute(), soon.Hour())})
	run := f.failRun(job, "Fatal: boom", map[string]any{"failure": "restic"})
	if !run.GetDateTime("retry_at").IsZero() || run.GetString("retry_note") != "The next scheduled run at "+soon.Format("15:04")+" UTC takes its place." {
		t.Errorf("a retry just before the next run: %v", run.FieldsData())
	}
	if a := f.openAlert(alertBackupFailed, job.Id+":"+f.target.Id); a == nil || a.GetString("severity") != schema.SeverityCritical {
		t.Errorf("its alert: %v", a)
	}

	// planned, and then the next run comes close
	later := f.job("later", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	run = f.failRun(later, "Fatal: boom", map[string]any{"failure": "restic"})
	retryAt := run.GetDateTime("retry_at").Time()
	near := retryAt.Add(8 * time.Minute)
	later.Set("schedule", fmt.Sprintf("%d %d * * *", near.Minute(), near.Hour()))
	if err := f.app.Save(later); err != nil {
		t.Fatal(err)
	}
	f.p.RunDueBackups(retryAt)
	if run = f.fresh(run); !run.GetDateTime("retry_at").IsZero() || run.GetString("retry_note") != "The next scheduled run at "+near.Format("15:04")+" UTC takes its place." {
		t.Errorf("a planned retry the next run comes close to: %v", run.FieldsData())
	}
	if runs := f.runsOf(later); len(runs) != 1 {
		t.Errorf("%d runs", len(runs))
	}
}

// TestARetryIsClaimedOnce (#31): two passes of the scheduler at once, or the
// route and a pass, make one retry of a run.
func TestARetryIsClaimedOnce(t *testing.T) {
	f, job, _ := newRetryFixture(t)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	for round := range 3 {
		run := f.failRun(job, "Fatal: boom", map[string]any{"failure": "restic"})
		due := run.GetDateTime("retry_at").Time().Add(time.Minute)
		var group sync.WaitGroup
		statuses := make(chan int, 1)
		for i := range 4 {
			group.Add(1)
			go func() {
				defer group.Done()
				if round > 0 && i == 0 {
					status, _ := f.send(http.MethodPost, "/api/pc/backup-runs/"+run.Id+"/retry", operator, nil)
					statuses <- status
					return
				}
				f.p.RunDueBackups(due)
			}()
		}
		group.Wait()
		children, err := f.app.FindRecordsByFilter(schema.BackupRuns, "retry_of={:run}", "", 0, 0, map[string]any{"run": run.Id})
		if err != nil || len(children) != 1 {
			t.Fatalf("round %d: %d retries: %v", round, len(children), err)
		}
		if round > 0 {
			if status := <-statuses; status != http.StatusOK && status != http.StatusConflict {
				t.Errorf("round %d: the route answered %d", round, status)
			}
		}
		// the retry ends, so that the next round's run is the newest
		f.endRun(children[0], map[string]any{"status": schema.CommandSucceeded, "started_at": types.NowDateTime(), "result": map[string]any{"snapshot_id": "4f2a"}})
	}
}

// TestARetryFollowsAHolderThatRuns (#31, AC2): a run a lock kept out, whose
// holder is a PocketCloud command that still runs, names it, and its retry
// waits for it: every 30 minutes, without counting an attempt, for 12 hours,
// then it is given up with a critical alert. Once the holder ended, the retry
// goes, unlocking first.
func TestARetryFollowsAHolderThatRuns(t *testing.T) {
	f, job, _ := newRetryFixture(t)
	nas := testutil.NewHost(t, f.app, "nas")
	holder := f.command(nas.Id, checkCommand(f.target.Id))
	holder.Set("label", "Check the integrity of b2")
	if err := f.app.Save(holder); err != nil {
		t.Fatal(err)
	}
	lock := backup.Lock{PID: 1, Host: backup.JobHost(holder.Id), User: "root", Exclusive: true, Created: time.Now().Add(-40 * time.Minute).UTC(), AgeS: 2400}
	run := f.failRun(job, "unable to create lock in backend: repository is already locked exclusively by PID 1 on pc-"+holder.Id+" by root (UID 0, GID 0)", lockAnswer(lock))
	finished := run.GetDateTime("finished_at").Time()
	stored := runLock(run)
	if stored == nil || stored.Command != holder.Id || stored.CommandType != "backup.check" || stored.CommandLabel != "Check the integrity of b2" ||
		stored.CommandHost != nas.Id || stored.CommandHostName != "nas" || !near(run.GetDateTime("retry_at").Time(), finished.Add(5*time.Minute)) {
		t.Fatalf("the run: %v", run.FieldsData())
	}
	if state, command := f.p.lockHolder(stored); state != holderAlive || command.Id != holder.Id {
		t.Errorf("the holder: %v", state)
	}

	f.p.RunDueBackups(finished.Add(6 * time.Minute))
	run = f.fresh(run)
	if runs := f.runsOf(job); len(runs) != 1 {
		t.Fatalf("%d runs while the holder runs", len(runs))
	}
	if retryAt := run.GetDateTime("retry_at").Time(); !near(retryAt, finished.Add(36*time.Minute)) || run.GetString("retry_note") != "Waits for Check the integrity of b2." {
		t.Errorf("a retry that waits for the holder: %v", run.FieldsData())
	}
	// 12 hours later it gives up, critical
	f.p.RunDueBackups(finished.Add(13 * time.Hour))
	run = f.fresh(run)
	if !run.GetDateTime("retry_at").IsZero() || run.GetString("retry_note") != "The repository stayed locked by Check the integrity of b2 for 12 hours." {
		t.Errorf("a retry given up: %v", run.FieldsData())
	}
	if a := f.openAlert(alertBackupFailed, job.Id+":"+f.target.Id); a == nil || a.GetString("severity") != schema.SeverityCritical ||
		!strings.HasSuffix(a.GetString("message"), "; the repository stayed locked by Check the integrity of b2 for 12 hours.") {
		t.Errorf("the alert: %v", a)
	}

	// the route refuses while the holder runs, and unlocks once it ended
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	url := "/api/pc/backup-runs/" + run.Id + "/retry"
	if status, out := f.send(http.MethodPost, url, operator, nil); status != http.StatusConflict ||
		out["message"] != "The repository is locked by Check the integrity of b2 on nas, which is still running: PocketCloud retries once it ends." {
		t.Errorf("while the holder runs: %d %v", status, out)
	}
	if err := f.p.Commands.Cancel(holder.Id, "Cancelled by admin@example.com"); err != nil {
		t.Fatal(err)
	}
	status, out := f.send(http.MethodPost, url, operator, nil)
	if status != http.StatusOK {
		t.Fatalf("once the holder ended: %d %v", status, out)
	}
	runs := f.runsOf(job)
	if len(runs) != 2 || runs[1].Id != out["run"] || runs[1].GetString("retry_of") != run.Id || runs[1].GetInt("attempt") != 2 {
		t.Fatalf("runs: %v", runs)
	}
	if payload := f.payloadOf(runs[1].GetString("command")); payload["unlock"] != true {
		t.Errorf("the retry does not unlock: %v", payload)
	}
	if command, _ := f.app.FindRecordById(schema.Commands, runs[1].GetString("command")); command.GetString("requested_label") != "operator@example.com" {
		t.Errorf("the retry's requester: %s", command.GetString("requested_label"))
	}
}

// checkCommand is a check of a target, as a command.
func checkCommand(targetID string) *agentv1.Command {
	return &agentv1.Command{Payload: &agentv1.Command_BackupCheck{BackupCheck: &agentv1.BackupCheck{
		Repository: &agentv1.BackupRepository{TargetId: targetID, Url: "s3:x"}, ReadDataSubset: "1/20",
	}}}
}

// queuedRun is a run of a job whose command is queued.
func (f *jobFixture) queuedRun(job *core.Record) *core.Record {
	f.t.Helper()
	run := f.run(job)
	command := f.save(schema.Commands, map[string]any{"host": f.host.Id, "type": "backup.run",
		"payload": map[string]any{"run_id": run.Id, "repository": map[string]any{"target_id": run.GetString("target"), "url": "s3:x"}}})
	run.Set("command", command.Id)
	if err := f.app.Save(run); err != nil {
		f.t.Fatal(err)
	}
	return run
}

// near tells whether two moments are the same to the millisecond the records
// keep.
func near(a, b time.Time) bool {
	return a.Sub(b).Abs() < time.Millisecond
}

// TestWhetherALocksHolderRuns (#31): the holder of a lock is a PocketCloud
// command that runs while it is queued, delivered or running, or timed out
// while its server is not connected; one that ended, or is gone, is not;
// another host's restic is unknown. A lock is stale 32 minutes after its
// last refresh, and an older agent's has no time.
func TestWhetherALocksHolderRuns(t *testing.T) {
	f, _, stream := newRetryFixture(t)
	// its commands stay queued until they are ended here
	f.p.Commands.Disconnect(stream)
	other := testutil.NewHost(t, f.app, "nas")
	created := time.Now().Add(-10 * time.Minute).UTC()
	lockOf := func(command *core.Record) *backup.Lock {
		return &backup.Lock{PID: 1, Host: backup.JobHost(command.Id), Created: created}
	}
	command := func(host *core.Record, status string) *core.Record {
		t.Helper()
		record := f.command(host.Id, checkCommand(f.target.Id))
		if status != schema.CommandQueued {
			f.closeCommand(record, status)
		}
		return f.fresh(record)
	}
	tests := []struct {
		name string
		lock *backup.Lock
		want holderState
	}{
		{"queued", lockOf(command(other, schema.CommandQueued)), holderAlive},
		{"failed", lockOf(command(other, schema.CommandFailed)), holderGone},
		{"succeeded", lockOf(command(f.host, schema.CommandSucceeded)), holderGone},
		// nas has no connection: its agent may still run it
		{"timed out on a server not connected", lockOf(command(other, schema.CommandTimedOut)), holderAlive},
		{"timed out on a connected server", lockOf(command(f.host, schema.CommandTimedOut)), holderGone},
		{"gone", &backup.Lock{Host: "pc-" + strings.Repeat("z", 15)}, holderGone},
		{"another host's restic", &backup.Lock{Host: "nas", Created: created}, holderUnknown},
		{"none", nil, holderUnknown},
	}
	connected := f.p.Commands.Connect(f.host.Id, []string{"backup.run"}, nil)
	defer f.p.Commands.Disconnect(connected)
	for _, test := range tests {
		if state, _ := f.p.lockHolder(test.lock); state != test.want {
			t.Errorf("%s: %v, not %v", test.name, state, test.want)
		}
	}
	if stale := (&backup.Lock{Host: "nas", Created: created}).StaleAt(); !stale.Equal(created.Add(32 * time.Minute)) {
		t.Errorf("stale at %s", stale)
	}
	if older := backup.ParseLockLine("repository is already locked by PID 7 on nas by root (UID 0, GID 0)"); older == nil || !older.StaleAt().IsZero() {
		t.Errorf("an older agent's lock: %v", older)
	}
}

// TestTheRetryRoute (#31, AC2): operators try a failed run again; the route
// refuses what cannot be tried again, in its order, and for a lock: a server
// whose clock is off, a holder that runs, and one restic does not count as
// stale yet, which it plans for when restic does. Viewers cannot.
func TestTheRetryRoute(t *testing.T) {
	f, job, stream := newRetryFixture(t)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	_, viewer := testutil.NewUser(t, f.app, "viewer@example.com", schema.RoleViewer)
	retry := func(run *core.Record, token string) (int, map[string]any) {
		t.Helper()
		return f.send(http.MethodPost, "/api/pc/backup-runs/"+run.Id+"/retry", token, nil)
	}
	refused := func(name string, run *core.Record, want string) {
		t.Helper()
		if status, out := retry(run, operator); status != http.StatusConflict || out["message"] != want {
			t.Errorf("%s: %d %v", name, status, out["message"])
		}
	}
	if status, out := f.send(http.MethodPost, "/api/pc/backup-runs/nope/retry", operator, nil); status != http.StatusNotFound || out["message"] != "The run does not exist." {
		t.Errorf("a run that does not exist: %d %v", status, out)
	}
	ok := f.endRun(f.run(job), map[string]any{"status": schema.CommandSucceeded, "started_at": types.NowDateTime(), "result": map[string]any{"snapshot_id": "4f2a"}})
	refused("a run that succeeded", ok, "Only a failed run is tried again.")

	failed := f.failRun(job, "Fatal: boom", map[string]any{"failure": "restic"})
	if status, _ := retry(failed, viewer); status != http.StatusForbidden {
		t.Errorf("a viewer: %d", status)
	}
	newer := f.run(job)
	refused("a newer run", failed, "A newer run of this job to this target exists.")
	f.endRun(newer, map[string]any{"status": schema.CommandCancelled, "error": "Cancelled"})

	failed = f.failRun(job, "Fatal: boom", map[string]any{"failure": "restic"})
	child := f.save(schema.BackupRuns, map[string]any{"job": job.Id, "target": f.target.Id, "host": f.host.Id, "trigger": schema.RunRetry,
		"status": schema.RunFailed, "retry_of": failed.Id})
	// as made before it, as by a pass that claimed it meanwhile
	if _, err := f.app.DB().NewQuery("UPDATE backup_runs SET created={:at} WHERE id={:id}").
		Bind(map[string]any{"at": failed.GetDateTime("created").Add(-time.Second).String(), "id": child.Id}).Execute(); err != nil {
		t.Fatal(err)
	}
	refused("a run tried again", failed, "It is being tried again already.")

	stack := f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "shop"})
	label := f.job("shop-db", map[string]any{"origin": schema.JobLabel, "stack": stack.Id, "service": "db", "source_kind": schema.SourceDump, "engine": schema.DumpPostgres})
	labelRun := f.failRun(label, "Fatal: boom", map[string]any{"failure": "restic"})
	label.Set("label_removed", true)
	if err := f.app.Save(label); err != nil {
		t.Fatal(err)
	}
	refused("a job whose label is gone", labelRun, labelGoneMessage)

	nas := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	two := f.job("two", map[string]any{"source_kind": schema.SourceVolume, "source": "b", "targets": []string{f.target.Id, nas.Id}})
	out := f.failRun(two, "Fatal: boom", map[string]any{"failure": "restic"})
	two.Set("targets", []string{nas.Id})
	if err := f.app.Save(two); err != nil {
		t.Fatal(err)
	}
	refused("a target taken out", out, "The target is no longer one of the job's targets.")

	other := f.job("other", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	f.queuedRun(other)
	behind := f.failRun(other, "Fatal: boom", map[string]any{"failure": "restic"})
	refused("a run before it still going", behind, "Its previous run is still going.")

	restoring := f.job("restoring", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	restoringRun := f.failRun(restoring, "Fatal: boom", map[string]any{"failure": "restic"})
	f.restore(restoring, map[string]any{"status": schema.RestoreQueued})
	refused("a restore under way", restoringRun, "A restore into its data is under way: back it up once it ends.")

	// a lock: a clock that is off, then a young lock of another host
	locked := f.job("locked", map[string]any{"source_kind": schema.SourceVolume, "source": "c"})
	created := time.Now().Add(-2 * time.Minute).UTC()
	lockedRun := f.failRun(locked, "unable to create lock in backend: repository is already locked by PID 9 on nas by root (UID 0, GID 0)",
		lockAnswer(backup.Lock{PID: 9, Host: "nas", User: "root", Created: created, AgeS: 120}))
	f.host.Set("clock_skew_ms", 600000)
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	refused("a clock that is off", lockedRun, "The clock of node-1 is off by 10 minutes: restic would take a live lock for a stale one. Set its clock first.")
	f.host.Set("clock_skew_ms", -1200)
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	status, answer := retry(lockedRun, operator)
	staleAt := created.Add(32 * time.Minute)
	if status != http.StatusAccepted || answer["retry_at"] != staleAt.Format(time.RFC3339) {
		t.Errorf("a young lock: %d %v", status, answer)
	}
	if planned := f.fresh(lockedRun); !near(planned.GetDateTime("retry_at").Time(), staleAt) ||
		planned.GetString("retry_note") != "Waits until restic counts the lock as stale." {
		t.Errorf("the planned retry: %v", planned.FieldsData())
	}
	if runs := f.runsOf(locked); len(runs) != 1 {
		t.Errorf("%d runs of a young lock", len(runs))
	}
	// at that time the pass unlocks and tries again
	f.p.RunDueBackups(staleAt.Add(time.Minute))
	runs := f.runsOf(locked)
	if len(runs) != 2 || f.payloadOf(runs[1].GetString("command"))["unlock"] != true {
		t.Errorf("the retry of a young lock: %v", runs)
	}

	// offline last: every refusal above was a connected server's
	f.p.Commands.Disconnect(stream)
	lone := f.job("lone", map[string]any{"source_kind": schema.SourceVolume, "source": "d"})
	loneRun := f.failRun(lone, "Fatal: boom", map[string]any{"failure": "restic"})
	refused("an offline server", loneRun, "The server node-1 is offline: its agent runs the backup.")
	again := f.p.Commands.Connect(f.host.Id, []string{"backup.run"}, nil)
	defer f.p.Commands.Disconnect(again)
	status, answer = retry(loneRun, operator)
	if status != http.StatusOK {
		t.Fatalf("a retry: %d %v", status, answer)
	}
	if runs := f.runsOf(lone); len(runs) != 2 || runs[1].Id != answer["run"] || runs[1].GetString("trigger") != schema.RunRetry ||
		f.payloadOf(runs[1].GetString("command"))["unlock"] == true {
		t.Errorf("the retry: %v", runs)
	}
	refused("a second retry", loneRun, "A newer run of this job to this target exists.")
}

// TestARetryWaitsOrIsGivenUp (#31): a due retry waits 10 minutes while its
// server is offline, the run before it still goes, or a restore writes its
// data; it is given up, saying why, when its job was disabled, its target
// taken out of it, a newer run took its place, or it was missed for a day
// while the panel was down, which alerts critical.
func TestARetryWaitsOrIsGivenUp(t *testing.T) {
	f, _, stream := newRetryFixture(t)
	f.p.Commands.Disconnect(stream)
	planned := func(name string) (*core.Record, *core.Record) {
		t.Helper()
		job := f.job(name, map[string]any{"source_kind": schema.SourceVolume, "source": name})
		return job, f.failRun(job, "Fatal: boom", map[string]any{"failure": "restic"})
	}

	// offline: 10 minutes more
	f.host.Set("status", schema.HostOffline)
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	f.p.SetStarted(time.Now().Add(-3 * time.Minute))
	job, run := planned("offline")
	due := run.GetDateTime("retry_at").Time()
	f.p.RunDueBackups(due.Add(time.Minute))
	if moved := f.fresh(run).GetDateTime("retry_at").Time(); !near(moved, due.Add(11*time.Minute)) {
		t.Errorf("an offline server's retry moved to %s", moved)
	}
	if runs := f.runsOf(job); len(runs) != 1 {
		t.Errorf("%d runs of an offline server", len(runs))
	}
	f.host.Set("status", schema.HostOnline)
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}

	// a restore writes its data
	job, run = planned("restoring")
	restore := f.restore(job, map[string]any{"status": schema.RestoreQueued})
	due = run.GetDateTime("retry_at").Time()
	f.p.RunDueBackups(due.Add(time.Minute))
	if moved := f.fresh(run).GetDateTime("retry_at").Time(); !near(moved, due.Add(11*time.Minute)) {
		t.Errorf("a retry during a restore moved to %s", moved)
	}
	restore.Set("status", schema.RestoreSucceeded)
	if err := f.app.Save(restore); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name  string
		after func(job, run *core.Record)
		note  string
	}{
		{"disabled", func(job, _ *core.Record) { job.Set("enabled", false); _ = f.app.Save(job) }, "The job is disabled, so it is not tried again."},
		{"taken out", func(job, _ *core.Record) { job.Set("targets", []string{}); _ = f.app.Save(job) }, "The target is no longer one of the job's targets."},
		{"newer", func(job, _ *core.Record) {
			f.endRun(f.run(job), map[string]any{"status": schema.CommandSucceeded, "started_at": types.NowDateTime(), "result": map[string]any{"snapshot_id": "4f2a"}})
		}, "A newer run of the job to the target took its place."},
	} {
		job, run := planned(test.name)
		test.after(job, run)
		f.p.RunDueBackups(run.GetDateTime("retry_at").Time().Add(time.Minute))
		if given := f.fresh(run); !given.GetDateTime("retry_at").IsZero() || given.GetString("retry_note") != test.note {
			t.Errorf("%s: %v", test.name, given.FieldsData())
		}
		if runs := f.runsOf(job); len(runs) > 2 || len(runs) == 2 && runs[1].GetString("trigger") == schema.RunRetry {
			t.Errorf("%s: %d runs", test.name, len(runs))
		}
	}

	// missed for a day
	job, run = planned("missed")
	f.p.RunDueBackups(run.GetDateTime("retry_at").Time().Add(25 * time.Hour))
	if given := f.fresh(run); !given.GetDateTime("retry_at").IsZero() || given.GetString("retry_note") != "Its retry was missed while the panel was down." {
		t.Errorf("a missed retry: %v", given.FieldsData())
	}
	if a := f.openAlert(alertBackupFailed, job.Id+":"+f.target.Id); a == nil || a.GetString("severity") != schema.SeverityCritical ||
		a.GetString("message") != "Backup missed → b2 failed: Fatal: boom; its retry was missed while the panel was down." {
		t.Errorf("its alert: %v", a)
	}
}

// TestARunByHandTakesTheRetrysPlace (#31): running a job by hand gives up the
// planned retries of its runs to the targets it runs to.
func TestARunByHandTakesTheRetrysPlace(t *testing.T) {
	f, job, _ := newRetryFixture(t)
	run := f.failRun(job, "Fatal: boom", map[string]any{"failure": "restic"})
	if run.GetDateTime("retry_at").IsZero() {
		t.Fatal("no retry")
	}
	if status, out := f.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/run", f.admin, nil); status != http.StatusOK {
		t.Fatalf("run now: %d %v", status, out)
	}
	if given := f.fresh(run); !given.GetDateTime("retry_at").IsZero() || given.GetString("retry_note") != "A run started by hand took its place." {
		t.Errorf("the retry: %v", given.FieldsData())
	}
}

// TestARetryWhoseCommandCannotBeMadeFails (#31): a retry whose command cannot
// be made, as a stack's job whose stack has no volume left, is a failed run of
// its own, with a critical alert, and gives up the retry of the run before it.
func TestARetryWhoseCommandCannotBeMadeFails(t *testing.T) {
	f, _, _ := newRetryFixture(t)
	stack := f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "shop"})
	volume := f.save(schema.Volumes, map[string]any{"host": f.host.Id, "name": "shop_db", "labels": map[string]string{"com.docker.compose.project": "shop"}})
	job := f.job("shop", map[string]any{"source_kind": schema.SourceStack, "stack": stack.Id})
	run := f.failRun(job, "Fatal: boom", map[string]any{"failure": "restic"})
	if err := f.app.Delete(volume); err != nil {
		t.Fatal(err)
	}
	f.p.RunDueBackups(run.GetDateTime("retry_at").Time().Add(time.Minute))
	runs := f.runsOf(job)
	if len(runs) != 2 || runs[1].GetString("status") != schema.RunFailed || runs[1].GetString("retry_of") != run.Id ||
		runs[1].GetString("error") != "The stack shop has no volumes to back up." || runs[1].GetString("command") != "" {
		t.Fatalf("runs: %v", runs)
	}
	if !f.fresh(run).GetDateTime("retry_at").IsZero() {
		t.Error("the run before it is still to be tried again")
	}
	if a := f.openAlert(alertBackupFailed, job.Id+":"+f.target.Id); a == nil || a.GetString("severity") != schema.SeverityCritical ||
		a.GetString("message") != "Backup shop → b2 failed: The stack shop has no volumes to back up." {
		t.Errorf("its alert: %v", a)
	}
}

// TestWhatDidNotFailIsNotTriedAgain (#31, with #29 and #30 as they were): a
// run and its command, made in one transaction now, are delivered with the
// run's secrets as before, without an unlock; a run skipped as its server was
// offline, or as a restore wrote its data, a run whose retention failed after
// its backup did not, and a run retried by hand, keep their #29 and #30
// records and alerts, and no retry is planned or made for any of them.
func TestWhatDidNotFailIsNotTriedAgain(t *testing.T) {
	f, job, _ := newRetryFixture(t)
	minute := time.Now().UTC().Truncate(time.Minute)

	// queued: its command names its run, which the enricher finds
	f.p.runOnSchedule(job, minute)
	runs := f.runsOf(job)
	if len(runs) != 1 || runs[0].GetString("status") != schema.RunQueued || runs[0].GetString("command") == "" {
		t.Fatalf("a scheduled run: %v", runs)
	}
	queued := runs[0]
	command, err := f.app.FindRecordById(schema.Commands, queued.GetString("command"))
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := f.deliver(command)
	if err != nil || delivered.GetBackupRun().GetRunId() != queued.Id || delivered.GetBackupRun().GetRepository().GetPassword() != "repo-password-b2" ||
		delivered.GetBackupRun().GetUnlock() {
		t.Errorf("its delivery: %v %v", delivered, err)
	}
	// its backup worked and its retention did not: an alert of the retention
	// alone, and no retry
	retained := f.endRun(queued, map[string]any{"status": schema.CommandSucceeded, "started_at": types.NowDateTime(),
		"result": map[string]any{"snapshot_id": "4f2a", "retention_error": "Fatal: unable to create lock"}})
	if retained.GetString("status") != schema.RunSucceeded || retained.GetString("failure") != "" || !retained.GetDateTime("retry_at").IsZero() {
		t.Errorf("a run whose retention failed: %v", retained.FieldsData())
	}
	if a := f.openAlert(alertBackupRetention, job.Id+":"+f.target.Id); a == nil || a.GetString("severity") != schema.SeverityWarning {
		t.Errorf("the retention's alert: %v", a)
	}
	if f.openAlert(alertBackupFailed, job.Id+":"+f.target.Id) != nil {
		t.Error("a run whose backup worked failed")
	}

	// a restore writes its data: skipped, without an alert
	restoring := f.job("restoring", map[string]any{"source_kind": schema.SourceVolume, "source": "r_data"})
	f.restore(restoring, map[string]any{"status": schema.RestoreRunning, "roots": []restoreRoot{{Source: "/volumes/r_data", Volume: "r_data"}}})
	f.p.runOnSchedule(restoring, minute)
	if runs := f.runsOf(restoring); len(runs) != 1 || runs[0].GetString("status") != schema.RunSkipped ||
		runs[0].GetString("error") != "A restore into its data is under way on node-1." || runs[0].GetString("failure") != "" ||
		!runs[0].GetDateTime("retry_at").IsZero() {
		t.Errorf("a run of data being restored: %v", runs)
	}
	if f.openAlert(alertBackupSkipped, restoring.Id+":"+f.target.Id) != nil {
		t.Error("a run of data being restored alerted")
	}

	// the server is offline at the minute: skipped, with the warning of #29
	f.host.Set("status", schema.HostOffline)
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	f.p.SetStarted(time.Now().Add(-3 * time.Minute))
	offline := f.job("offline", map[string]any{"source_kind": schema.SourceVolume, "source": "b"})
	f.p.runOnSchedule(offline, minute)
	runs = f.runsOf(offline)
	if len(runs) != 1 || runs[0].GetString("status") != schema.RunSkipped || runs[0].GetString("failure") != "" || !runs[0].GetDateTime("retry_at").IsZero() {
		t.Fatalf("a run of an offline server: %v", runs)
	}
	if a := f.openAlert(alertBackupSkipped, offline.Id+":"+f.target.Id); a == nil || a.GetString("severity") != schema.SeverityWarning ||
		a.GetString("message") != "Backup offline → b2 was skipped: the server node-1 was offline at "+minute.Format("15:04")+" UTC." {
		t.Errorf("its alert: %v", a)
	}

	// hours later, the scheduler tries none of them again
	f.host.Set("status", schema.HostOnline)
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	f.p.RunDueBackups(time.Now().Add(3 * time.Hour))
	for _, of := range []*core.Record{job, restoring, offline} {
		for _, run := range f.runsOf(of) {
			if run.GetString("trigger") == schema.RunRetry {
				t.Errorf("%s was tried again: %v", of.GetString("name"), run.FieldsData())
			}
		}
	}

	// a run retried by hand that fails on its last attempt is critical, as a
	// failure that is not tried again was before
	failed := f.failRun(job, "Fatal: boom", map[string]any{"failure": "restic"})
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	for range maxRunAttempts {
		status, out := f.send(http.MethodPost, "/api/pc/backup-runs/"+failed.Id+"/retry", operator, nil)
		if status != http.StatusOK {
			t.Fatalf("retry by hand: %d %v", status, out)
		}
		retry, err := f.app.FindRecordById(schema.BackupRuns, out["run"].(string))
		if err != nil {
			t.Fatal(err)
		}
		failed = f.endRun(retry, map[string]any{"status": schema.CommandFailed, "error": "Fatal: boom", "started_at": types.NowDateTime(),
			"result": map[string]any{"failure": "restic"}})
	}
	if failed.GetInt("attempt") != maxRunAttempts+1 || !failed.GetDateTime("retry_at").IsZero() {
		t.Errorf("a retry by hand past the last attempt: %v", failed.FieldsData())
	}
	if a := f.openAlert(alertBackupFailed, job.Id+":"+f.target.Id); a == nil || a.GetString("severity") != schema.SeverityCritical ||
		a.GetString("message") != "Backup data → b2 failed: Fatal: boom." {
		t.Errorf("its alert: %v", a)
	}
}
