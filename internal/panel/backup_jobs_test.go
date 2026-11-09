package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/cron"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/stacks"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// TestTheNextRunIsTheNextDueMinute (#29): a job's next run is the first
// minute after now that its schedule is due, in UTC, with cron's rule that a
// day of the month or a weekday is enough when both are named; a schedule
// that never comes within a year has none.
func TestTheNextRunIsTheNextDueMinute(t *testing.T) {
	at := func(text string) time.Time {
		moment, err := time.Parse(time.DateTime, text)
		if err != nil {
			t.Fatal(err)
		}
		return moment
	}
	for _, test := range []struct {
		schedule, after, want string
	}{
		{"*/15 * * * *", "2026-11-07 12:07:30", "2026-11-07 12:15:00"},
		{"0 3 * * *", "2026-11-07 02:59:59", "2026-11-07 03:00:00"},
		// strictly after: the minute that just ran is not the next
		{"0 3 * * *", "2026-11-07 03:00:00", "2026-11-08 03:00:00"},
		{"@monthly", "2026-11-09 10:00:00", "2026-12-01 00:00:00"},
		{"@yearly", "2026-11-09 10:00:00", "2027-01-01 00:00:00"},
		{"30 2 * * 1-5", "2026-11-06 03:00:00", "2026-11-09 02:30:00"},
		// the 1st and the 15th, and every Friday: 2026-11-13 is a Friday,
		// the 15th a Sunday
		{"0 4 1,15 * 5", "2026-11-06 04:00:00", "2026-11-13 04:00:00"},
		{"0 4 1,15 * 5", "2026-11-13 04:00:00", "2026-11-15 04:00:00"},
		{"0 0 31 * *", "2026-11-09 00:00:00", "2026-12-31 00:00:00"},
		{"0 0 30 2 *", "2026-11-09 00:00:00", ""},
		{"not a schedule", "2026-11-09 00:00:00", ""},
	} {
		got := parseCronRule(test.schedule).next(at(test.after))
		want := time.Time{}
		if test.want != "" {
			want = at(test.want)
		}
		if !got.Equal(want) {
			t.Errorf("%q after %s: %s, not %s", test.schedule, test.after, got, want)
		}
	}

	// the minutes it passes by are none that the scheduler would run
	start := at("2026-11-09 17:41:00")
	for _, schedule := range []string{"*/7 */5 * * *", "13 1 2,20 * 0,3", "0 12 * 2,6 *", "5 4 * * 0", "59 23 31 12 *"} {
		rule := parseCronRule(schedule)
		next := rule.next(start)
		for minute := start.Add(time.Minute); minute.Before(next) && minute.Before(start.Add(400*24*time.Hour)); minute = minute.Add(time.Minute) {
			if taskDue(schedule, cron.NewMoment(minute)) {
				t.Errorf("%q: due at %s, before %s", schedule, minute, next)
				break
			}
		}
		if next.IsZero() || !taskDue(schedule, cron.NewMoment(next)) {
			t.Errorf("%q: the next run %s is not due", schedule, next)
		}
	}
}

// jobFixture is a panel with a server, a target and the helpers of the tests
// of jobs.
type jobFixture struct {
	t       *testing.T
	p       *Panel
	app     *tests.TestApp
	handler http.Handler
	host    *core.Record
	target  *core.Record
	admin   string
}

func newJobFixture(t *testing.T) *jobFixture {
	t.Helper()
	p, app, handler := newTestPanel(t)
	f := &jobFixture{t: t, p: p, app: app, handler: handler, host: testutil.NewHost(t, app, "node-1")}
	f.target = f.addTarget("b2", backup.KindS3, f.host, backup.Config{Endpoint: "https://s3.example.com", Bucket: "backups", Prefix: "node"},
		backup.Credentials{AccessKeyID: "key-id", SecretAccessKey: "the-secret-key"})
	_, f.admin = testutil.NewUser(t, app, "admin@example.com", schema.RoleAdmin)
	return f
}

func (f *jobFixture) addTarget(name, kind string, host *core.Record, config backup.Config, creds backup.Credentials) *core.Record {
	f.t.Helper()
	collection, err := f.app.FindCollectionByNameOrId(schema.BackupTargets)
	if err != nil {
		f.t.Fatal(err)
	}
	target := core.NewRecord(collection)
	target.Load(map[string]any{"name": name, "kind": kind, "host": host.Id, "config": config,
		"location": backup.Where(kind, config), "status": schema.TargetOK, "repo_id": strings.Repeat("a", 64)})
	if err := f.p.sealTarget(target, creds, "repo-password-"+name); err != nil {
		f.t.Fatal(err)
	}
	if err := f.app.Save(target); err != nil {
		f.t.Fatal(err)
	}
	return target
}

func (f *jobFixture) save(collectionName string, fields map[string]any) *core.Record {
	f.t.Helper()
	collection, err := f.app.FindCollectionByNameOrId(collectionName)
	if err != nil {
		f.t.Fatal(err)
	}
	record := core.NewRecord(collection)
	record.Load(fields)
	if err := f.app.Save(record); err != nil {
		f.t.Fatalf("%s: %v", collectionName, err)
	}
	return record
}

func (f *jobFixture) job(name string, fields map[string]any) *core.Record {
	f.t.Helper()
	// never due by itself: the panel's own scheduler runs in the tests too
	all := map[string]any{"name": name, "host": f.host.Id, "targets": []string{f.target.Id}, "schedule": "0 0 30 2 *",
		"enabled": true, "origin": schema.JobManual}
	for key, value := range fields {
		all[key] = value
	}
	return f.save(schema.BackupJobs, all)
}

func (f *jobFixture) send(method, url, token string, body any) (int, map[string]any) {
	f.t.Helper()
	raw := ""
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		raw = string(encoded)
	}
	status, out := testutil.Do(f.t, f.handler, testutil.Request{Method: method, URL: url, Token: token, Body: raw})
	answer := map[string]any{}
	_ = json.Unmarshal([]byte(out), &answer)
	answer["raw"] = out
	return status, answer
}

func (f *jobFixture) run(job *core.Record) *core.Record {
	f.t.Helper()
	return f.save(schema.BackupRuns, map[string]any{"job": job.Id, "target": f.target.Id, "host": f.host.Id, "trigger": schema.RunManual, "status": schema.RunQueued})
}

// TestABackupRunNamesWhatItBacksUp (#29): the command of a run names the
// target, never its secrets, and what its job backs up: a volume, a
// directory, a stack's volumes from the inventory, a dump made in the running
// container of a service with the engine's own command.
func TestABackupRunNamesWhatItBacksUp(t *testing.T) {
	f := newJobFixture(t)
	stack := f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "shop"})
	empty := f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "empty"})
	for name, project := range map[string]string{"shop_db": "shop", "shop_uploads": "shop", "other_data": "other"} {
		f.save(schema.Volumes, map[string]any{"host": f.host.Id, "name": name, "labels": map[string]string{"com.docker.compose.project": project}})
	}
	f.save(schema.Containers, map[string]any{"host": f.host.Id, "stack": stack.Id, "project": "shop", "service": "db", "docker_id": "c0ffee", "name": "shop-db-1", "state": "running"})
	f.save(schema.Containers, map[string]any{"host": f.host.Id, "stack": stack.Id, "project": "shop", "service": "old", "docker_id": "dead", "name": "shop-old-1", "state": "exited"})

	build := func(job *core.Record) (*agentv1.BackupRun, error) {
		t.Helper()
		return f.p.backupRun(job, f.target, f.host, f.run(job))
	}

	volume := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data",
		"retention": stacks.Retention{Daily: 7, Last: 3}})
	run, err := build(volume)
	if err != nil {
		t.Fatal(err)
	}
	if run.GetRepository().GetTargetId() != f.target.Id || run.GetRepository().GetUrl() != "s3:https://s3.example.com/backups/node" ||
		run.GetRepository().GetPassword() != "" || strings.Join(run.GetTags(), ",") != "pocketcloud,job:"+volume.Id ||
		run.GetHostName() != "node-1" || strings.Join(run.GetVolumes(), ",") != "app_data" || len(run.GetPaths()) != 0 ||
		run.GetRetention().GetDaily() != 7 || run.GetRetention().GetLast() != 3 {
		t.Errorf("a volume: %v", run)
	}
	stored, err := commands.PayloadJSON(&agentv1.Command{Payload: &agentv1.Command_BackupRun{BackupRun: run}})
	if err != nil || strings.Contains(string(stored), "the-secret-key") || strings.Contains(string(stored), "repo-password") {
		t.Errorf("the stored payload: %s %v", stored, err)
	}

	if run, err := build(f.job("srv", map[string]any{"source_kind": schema.SourcePath, "source": "/srv/www"})); err != nil ||
		strings.Join(run.GetPaths(), ",") != "/srv/www" || run.GetRetention() != nil {
		t.Errorf("a directory: %v %v", run, err)
	}
	if run, err := build(f.job("shop", map[string]any{"source_kind": schema.SourceStack, "stack": stack.Id})); err != nil ||
		strings.Join(run.GetVolumes(), ",") != "shop_db,shop_uploads" {
		t.Errorf("a stack: %v %v", run, err)
	}
	if run, err := build(f.job("shop-app", map[string]any{"source_kind": schema.SourceStack, "stack": stack.Id, "service": "app",
		"source": "shop_uploads,shared-cache", "origin": schema.JobLabel})); err != nil || strings.Join(run.GetVolumes(), ",") != "shop_uploads,shared-cache" {
		t.Errorf("a service's volumes from its label: %v %v", run, err)
	}
	if _, err := build(f.job("empty", map[string]any{"source_kind": schema.SourceStack, "stack": empty.Id})); err == nil ||
		err.Error() != "the stack empty has no volumes to back up" {
		t.Errorf("a stack without volumes: %v", err)
	}

	run, err = build(f.job("pg", map[string]any{"source_kind": schema.SourceDump, "stack": stack.Id, "service": "db", "engine": schema.DumpPostgres}))
	if err != nil || run.GetStdinContainer() != "c0ffee" || run.GetStdinFileName() != "shop-db.pgdump" || len(run.GetVolumes()) != 0 ||
		strings.Join(run.GetStdinCommand()[:2], " ") != "sh -c" || !strings.Contains(run.GetStdinCommand()[2], "pg_dump --format=custom --compress=0") {
		t.Errorf("a PostgreSQL dump: %v %v", run, err)
	}
	run, err = build(f.job("maria", map[string]any{"source_kind": schema.SourceDump, "stack": stack.Id, "service": "db", "engine": schema.DumpMySQL}))
	if err != nil || run.GetStdinFileName() != "shop-db.sql" || !strings.Contains(run.GetStdinCommand()[2], "--single-transaction") ||
		!strings.Contains(run.GetStdinCommand()[2], `MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-$MYSQL_ROOT_PASSWORD}"`) {
		t.Errorf("a MySQL dump: %v %v", run, err)
	}
	// sqlite3 exits 0 on a database it cannot read, and makes one that is
	// not there: the dump opens it read-only, waits for a writer's lock, and
	// fails on any error sqlite3 says; the file is the shell's argument
	run, err = build(f.job("notes", map[string]any{"source_kind": schema.SourceDump, "stack": stack.Id, "service": "db", "engine": schema.DumpSQLite, "source": "/data/my notes.db"}))
	want := []string{"sh", "-c", `exec 3>&1; e=$(sqlite3 -readonly -cmd '.timeout 60000' "$1" .dump 2>&1 >&3); s=$?; ` +
		`if [ -n "$e" ]; then printf '%s\n' "$e" >&2; exit 1; fi; exit $s`, "sh", "/data/my notes.db"}
	if err != nil || !slices.Equal(run.GetStdinCommand(), want) || run.GetStdinFileName() != "shop-db.sql" {
		t.Errorf("a SQLite dump: %q %v", run.GetStdinCommand(), err)
	}
	if _, err := build(f.job("stopped", map[string]any{"source_kind": schema.SourceDump, "stack": stack.Id, "service": "old", "engine": schema.DumpPostgres})); err == nil ||
		!strings.Contains(err.Error(), "the service old of shop has no running container") {
		t.Errorf("a stopped service: %v", err)
	}
}

// TestARunIsDeliveredToItsJobsServer (#29): a run goes to its job's server,
// which need not be the one that checks the target, and gets the target's
// secrets there; a local target is only its own server's, and a target still
// being added takes no run.
func TestARunIsDeliveredToItsJobsServer(t *testing.T) {
	f := newJobFixture(t)
	other := testutil.NewHost(t, f.app, "node-2")
	job := f.job("data", map[string]any{"host": other.Id, "source_kind": schema.SourceVolume, "source": "app_data"})
	run := f.run(job)
	deliver := func(host string, target *core.Record) (*agentv1.Command, error) {
		t.Helper()
		record := core.NewRecord(core.NewBaseCollection(schema.Commands))
		record.Id = "deliveredrun001"
		record.Set("host", host)
		record.Set("type", "backup.run")
		command := &agentv1.Command{Payload: &agentv1.Command_BackupRun{BackupRun: &agentv1.BackupRun{
			RunId: run.Id, Repository: &agentv1.BackupRepository{TargetId: target.Id, Url: "s3:https://s3.example.com/backups/node"},
		}}}
		return command, f.p.enrichBackup(record, command)
	}
	command, err := deliver(other.Id, f.target)
	if err != nil || command.GetBackupRun().GetRepository().GetPassword() != "repo-password-b2" ||
		command.GetBackupRun().GetRepository().GetEnv()["AWS_SECRET_ACCESS_KEY"] != "the-secret-key" {
		t.Errorf("delivered: %v %v", command, err)
	}
	if _, err := deliver(f.host.Id, f.target); err == nil || !strings.Contains(err.Error(), "runs on another server") {
		t.Errorf("to the target's server: %v", err)
	}
	disk := f.addTarget("disk", backup.KindLocal, f.host, backup.Config{Path: "/srv/restic"}, backup.Credentials{})
	run.Set("target", disk.Id)
	if err := f.app.Save(run); err != nil {
		t.Fatal(err)
	}
	// as a target changed since it was chosen
	job.Set("targets", []string{f.target.Id, disk.Id})
	if err := f.app.Save(job); err != nil {
		t.Fatal(err)
	}
	if _, err := deliver(other.Id, disk); err == nil || !strings.Contains(err.Error(), "only that server's jobs back up to it") {
		t.Errorf("another server's local target: %v", err)
	}
	run.Set("target", f.target.Id)
	f.target.Set("status", schema.TargetPending)
	if err := f.app.Save(run); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Save(f.target); err != nil {
		t.Fatal(err)
	}
	if _, err := deliver(other.Id, f.target); err == nil || !strings.Contains(err.Error(), "still being added") {
		t.Errorf("a target being added: %v", err)
	}

	// a run queued before its target was taken out of the job, or before
	// its label went, gets none of the target's secrets
	f.target.Set("status", schema.TargetOK)
	if err := f.app.Save(f.target); err != nil {
		t.Fatal(err)
	}
	second := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	job.Set("targets", []string{second.Id})
	if err := f.app.Save(job); err != nil {
		t.Fatal(err)
	}
	command, err = deliver(other.Id, f.target)
	if err == nil || err.Error() != "the target is no longer one of the job's targets" || command.GetBackupRun().GetRepository().GetPassword() != "" {
		t.Errorf("a target taken out of the job: %v", err)
	}
	stack := f.save(schema.Stacks, map[string]any{"host": other.Id, "name": "shop"})
	job.Load(map[string]any{"targets": []string{f.target.Id}, "origin": schema.JobLabel, "stack": stack.Id, "service": "db", "label_removed": true})
	if err := f.app.Save(job); err != nil {
		t.Fatal(err)
	}
	if _, err := deliver(other.Id, f.target); err == nil || err.Error() != "the pocketcloud.backup label of the job is gone" {
		t.Errorf("a label that is gone: %v", err)
	}

	// such a run did not start: it is cancelled, and alerts nobody
	job.Load(map[string]any{"targets": []string{second.Id}, "origin": schema.JobManual, "label_removed": false})
	if err := f.app.Save(job); err != nil {
		t.Fatal(err)
	}
	refused := f.save(schema.Commands, map[string]any{"host": other.Id, "type": "backup.run",
		"payload": map[string]any{"run_id": run.Id, "repository": map[string]any{"target_id": f.target.Id, "url": "s3:x"}}})
	refused.Load(map[string]any{"status": schema.CommandFailed, "error": "the target is no longer one of the job's targets", "finished_at": types.NowDateTime()})
	f.p.backupRunFinished(refused)
	if stored, _ := f.app.FindRecordById(schema.BackupRuns, run.Id); stored.GetString("status") != schema.RunCancelled ||
		stored.GetString("error") != "The target is no longer one of the job's targets" {
		t.Errorf("the refused run: %v", stored.FieldsData())
	}
	if alerts, _ := f.app.FindRecordsByFilter(schema.Alerts, "rule='backup_failed'", "", 0, 0); len(alerts) != 0 {
		t.Errorf("%d alerts", len(alerts))
	}
}

// TestARunsEndIsKeptAndAlerted (#29): the finisher keeps what restic said on
// the run, with the secrets it missed hidden; a failure raises an alert with
// restic's error, which a success resolves, and a retention that failed after
// a backup raises one of its own.
func TestARunsEndIsKeptAndAlerted(t *testing.T) {
	f := newJobFixture(t)
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	key := job.Id + ":" + f.target.Id
	// finishWith ends a run's command as the fields say
	finishWith := func(fields map[string]any) *core.Record {
		t.Helper()
		run := f.run(job)
		command := f.save(schema.Commands, map[string]any{"host": f.host.Id, "type": "backup.run",
			"payload": map[string]any{"run_id": run.Id, "repository": map[string]any{"target_id": f.target.Id, "url": "s3:x"}}})
		command.Load(map[string]any{"finished_at": types.NowDateTime()})
		command.Load(fields)
		f.p.backupRunFinished(command)
		stored, err := f.app.FindRecordById(schema.BackupRuns, run.Id)
		if err != nil {
			t.Fatal(err)
		}
		return stored
	}
	finish := func(status, message string, result map[string]any) *core.Record {
		t.Helper()
		fields := map[string]any{"status": status, "error": message, "result": result}
		// a command that timed out here never started
		if status != schema.CommandTimedOut {
			fields["started_at"] = types.NowDateTime().Add(-90 * time.Second)
		}
		return finishWith(fields)
	}
	alert := func(rule string) *core.Record {
		t.Helper()
		alerts, err := f.app.FindRecordsByFilter(schema.Alerts, "rule={:rule} && target={:key} && status!='resolved'", "", 0, 0,
			map[string]any{"rule": rule, "key": key})
		if err != nil || len(alerts) > 1 {
			t.Fatalf("%d alerts: %v", len(alerts), err)
		}
		if len(alerts) == 0 {
			return nil
		}
		return alerts[0]
	}

	failed := finish(schema.CommandFailed, "Fatal: unable to open config file: Access Denied for the-secret-key", map[string]any{"refused": true, "log": "restic said repo-password-b2"})
	if failed.GetString("status") != schema.RunFailed || failed.GetString("error") != "Fatal: unable to open config file: Access Denied for ***" ||
		strings.Contains(failed.GetString("log"), "repo-password") || failed.GetInt("duration_ms") < 89000 {
		t.Errorf("a failed run: %v", failed.FieldsData())
	}
	if a := alert(alertBackupFailed); a == nil || a.GetString("message") != "Backup data → b2 failed: Fatal: unable to open config file: Access Denied for ***." ||
		a.GetString("severity") != schema.SeverityCritical || a.GetString("host") != f.host.Id {
		t.Errorf("the alert: %v", a)
	}
	if stored, _ := f.app.FindRecordById(schema.BackupJobs, job.Id); stored.GetString("last_status") != schema.RunFailed {
		t.Errorf("the job's last status: %s", stored.GetString("last_status"))
	}

	ok := finish(schema.CommandSucceeded, "", map[string]any{"snapshot_id": "4f2a", "bytes_added": 1234567, "total_bytes": 9876543210.0,
		"files_new": 3, "files_changed": 2, "forgotten": 5, "log": "saved", "retention_error": "Fatal: unable to create lock"})
	if ok.GetString("status") != schema.RunSucceeded || ok.GetString("snapshot_id") != "4f2a" || ok.GetInt("bytes_added") != 1234567 ||
		ok.GetInt("total_bytes") != 9876543210 || ok.GetInt("forgotten") != 5 || ok.GetString("error") != "" || ok.GetString("log") != "saved" {
		t.Errorf("a run that succeeded: %v", ok.FieldsData())
	}
	if alert(alertBackupFailed) != nil {
		t.Error("the success did not resolve the failure")
	}
	if a := alert(alertBackupRetention); a == nil || !strings.Contains(a.GetString("message"), "Fatal: unable to create lock") {
		t.Errorf("the retention's alert: %v", a)
	}
	finish(schema.CommandSucceeded, "", map[string]any{"snapshot_id": "5b3c"})
	if alert(alertBackupRetention) != nil {
		t.Error("a retention that worked again did not resolve its alert")
	}

	// a run that waited its whole time and never started did not happen: it
	// is skipped, with an alert, whether it never reached its server...
	never := finish(schema.CommandTimedOut, "The server stayed offline, so the command was never delivered.", nil)
	if never.GetString("status") != schema.RunSkipped || never.GetString("error") != "It waited 12 hours while the server node-1 stayed offline, and never started." {
		t.Errorf("never delivered: %v", never.FieldsData())
	}
	if a := alert(alertBackupSkipped); a == nil || a.GetString("severity") != schema.SeverityWarning ||
		a.GetString("message") != "Backup data → b2 was skipped: it waited 12 hours while the server node-1 stayed offline, and never started." {
		t.Errorf("the alert of a run that was never delivered: %v", a)
	}
	if alert(alertBackupFailed) != nil {
		t.Error("a run that never started failed")
	}
	// ...or the agent cancelled it as its time was up while it waited for a
	// free worker
	now := types.NowDateTime()
	busy := finishWith(map[string]any{"status": schema.CommandCancelled, "error": "The command was cancelled while waiting for a free worker.",
		"dispatched_at": now.Add(-12 * time.Hour), "expires_at": now.Add(-time.Second), "finished_at": now})
	if busy.GetString("status") != schema.RunSkipped || busy.GetString("error") != "It waited 12 hours while the server node-1 was busy with other backups, and never started." {
		t.Errorf("never started on a busy server: %v", busy.FieldsData())
	}
	if a := alert(alertBackupSkipped); a == nil || !strings.Contains(a.GetString("message"), "was busy with other backups") {
		t.Errorf("the alert of a run that waited for a free worker: %v", a)
	}

	// one someone cancelled is cancelled, and alerts nobody
	finish(schema.CommandSucceeded, "", map[string]any{"snapshot_id": "6c4d"})
	if alert(alertBackupSkipped) != nil {
		t.Error("a success did not resolve the skip")
	}
	cancelled := finishWith(map[string]any{"status": schema.CommandCancelled, "error": "Cancelled by admin@example.com"})
	if cancelled.GetString("status") != schema.RunCancelled || cancelled.GetString("error") != "Cancelled by admin@example.com" {
		t.Errorf("cancelled: %v", cancelled.FieldsData())
	}
	if alert(alertBackupSkipped) != nil || alert(alertBackupFailed) != nil {
		t.Error("a run someone cancelled alerted")
	}
}

// TestJobsAreCheckedAsTheyAreSaved (#29): the routes check what a job backs
// up, its targets, its schedule and its retention; a label's job keeps what
// its labels set, and stays while its label is there.
func TestJobsAreCheckedAsTheyAreSaved(t *testing.T) {
	f := newJobFixture(t)
	other := testutil.NewHost(t, f.app, "node-2")
	disk := f.addTarget("disk", backup.KindLocal, other, backup.Config{Path: "/srv/restic"}, backup.Credentials{})
	good := map[string]any{"name": "data", "host": f.host.Id, "source_kind": "volume", "source": "app_data", "targets": []string{f.target.Id},
		"schedule": "0  3 * * *", "retention": map[string]any{"daily": 7}}
	with := func(changes map[string]any) map[string]any {
		body := map[string]any{}
		for key, value := range good {
			body[key] = value
		}
		for key, value := range changes {
			body[key] = value
		}
		return body
	}
	for _, test := range []struct {
		changes map[string]any
		want    string
	}{
		{map[string]any{"name": " "}, "Give the job a name."},
		{map[string]any{"source": "-bad"}, "Name the volume as Docker does"},
		{map[string]any{"source_kind": "path", "source": "/proc/1"}, "Nothing under /proc is backed up"},
		{map[string]any{"source_kind": "path", "source": "/var/lib/pocketcloud/x"}, "Nothing under /var/lib/pocketcloud is backed up"},
		// a directory that holds one the job container needs would hide it
		{map[string]any{"source_kind": "path", "source": "/usr"}, "/usr holds /usr/bin, which the backup's job needs"},
		{map[string]any{"source_kind": "path", "source": "/var"}, "/var holds /var/lib/pocketcloud"},
		{map[string]any{"source_kind": "path", "source": "/tmp"}, "The backup's job needs /tmp for itself"},
		{map[string]any{"source_kind": "path", "source": "/srv/../etc"}, "absolute path"},
		{map[string]any{"source_kind": "path", "source": "/"}, "not the whole server"},
		{map[string]any{"source_kind": "stack"}, "Choose the stack."},
		{map[string]any{"targets": []string{disk.Id}}, "only that server's jobs back up to it"},
		{map[string]any{"targets": []string{"nosuchtarget123"}}, "One of the targets does not exist."},
		{map[string]any{"schedule": "every day"}, "A schedule has five fields"},
		{map[string]any{"retention": map[string]any{"daily": 10001}}, "between 0 and 10000"},
		{map[string]any{"host": "nosuchhost12345"}, "Choose the server"},
	} {
		if status, out := f.send(http.MethodPost, "/api/pc/backup-jobs", f.admin, with(test.changes)); status != http.StatusBadRequest ||
			!strings.Contains(out["message"].(string), test.want) {
			t.Errorf("%v: %d %v", test.changes, status, out)
		}
	}
	status, out := f.send(http.MethodPost, "/api/pc/backup-jobs", f.admin, good)
	if status != http.StatusCreated || out["schedule"] != "0 3 * * *" || out["enabled"] != true || out["origin"] != schema.JobManual || out["next_run_at"] == "" {
		t.Fatalf("add: %d %v", status, out)
	}
	if status, out := f.send(http.MethodPost, "/api/pc/backup-jobs", f.admin, good); status != http.StatusBadRequest || !strings.Contains(out["message"].(string), "exists already") {
		t.Errorf("the same name: %d %v", status, out)
	}
	id := out["id"].(string)
	// the target a job backs up to stays
	if status, out := f.send(http.MethodDelete, "/api/pc/backup-targets/"+f.target.Id, f.admin, nil); status != http.StatusConflict ||
		out["message"] != "The job data backs up to this target: take it out of them first." {
		t.Errorf("remove a target in use: %d %v", status, out)
	}
	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+id, f.admin, map[string]any{"enabled": false, "targets": []string{}}); status != http.StatusOK ||
		out["enabled"] != false || out["next_run_at"] != "" || out["problem"] != noTarget || out["source"] != "app_data" {
		t.Errorf("a change: %d %v", status, out)
	}

	// a dump's server is its stack's
	stack := f.save(schema.Stacks, map[string]any{"host": other.Id, "name": "shop"})
	status, out = f.send(http.MethodPost, "/api/pc/backup-jobs", f.admin, map[string]any{"name": "pg", "source_kind": "db_dump", "stack": stack.Id,
		"service": "db", "engine": "postgres", "source": "ignored", "schedule": "@daily"})
	if status != http.StatusCreated || out["host"] != other.Id || out["source"] != "" || out["problem"] != noTarget {
		t.Errorf("a dump: %d %v", status, out)
	}

	for _, dir := range []string{"/srv/app", "/home/me/photos", "/tmp/exports", "/var/www"} {
		if status, out := f.send(http.MethodPost, "/api/pc/backup-jobs", f.admin, with(map[string]any{"name": dir, "source_kind": "path", "source": dir})); status != http.StatusCreated {
			t.Errorf("the directory %s: %d %v", dir, status, out)
		}
	}

	// a label's job: what its labels set stays theirs
	label := f.job("shop-db", map[string]any{"origin": schema.JobLabel, "stack": stack.Id, "host": other.Id, "service": "db",
		"source_kind": schema.SourceDump, "engine": schema.DumpPostgres, "targets": []string{}, "label_sets": []string{"schedule", "retention"}})
	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+label.Id, f.admin, map[string]any{"schedule": "@hourly"}); status != http.StatusBadRequest ||
		out["message"] != "The schedule of this job comes from the pocketcloud.backup.schedule label of shop (db): change it there, and deploy." {
		t.Errorf("change a label's schedule: %d %v", status, out)
	}
	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+label.Id, f.admin, map[string]any{"retention": map[string]any{"last": 3}}); status != http.StatusBadRequest ||
		!strings.Contains(out["message"].(string), "pocketcloud.backup.retention label") {
		t.Errorf("change a label's retention: %d %v", status, out)
	}
	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+label.Id, f.admin, map[string]any{"targets": []string{f.target.Id}, "schedule": "0 0 30 2 *"}); status != http.StatusOK ||
		out["problem"] != "" {
		t.Errorf("give a label's job a target: %d %v", status, out)
	}
	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+label.Id, f.admin, map[string]any{"engine": "mysql"}); status != http.StatusBadRequest ||
		!strings.Contains(out["message"].(string), "comes from the pocketcloud.backup label of shop (db)") {
		t.Errorf("change a label's source: %d %v", status, out)
	}
	if status, out := f.send(http.MethodDelete, "/api/pc/backup-jobs/"+label.Id, f.admin, nil); status != http.StatusConflict ||
		!strings.Contains(out["message"].(string), "remove its pocketcloud.backup label instead") {
		t.Errorf("remove a label's job: %d %v", status, out)
	}
	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+label.Id, f.admin, map[string]any{"name": "shop database", "engine": "postgres"}); status != http.StatusOK ||
		out["name"] != "shop database" {
		t.Errorf("rename a label's job: %d %v", status, out)
	}

	// runs need the server online, and operators may start them
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	_, viewer := testutil.NewUser(t, f.app, "viewer@example.com", schema.RoleViewer)
	if status, out := f.send(http.MethodPost, "/api/pc/backup-jobs/"+id+"/run", operator, nil); status != http.StatusBadRequest ||
		out["message"] != "Choose where it goes: the job has no target." {
		t.Errorf("run without a target: %d %v", status, out)
	}
	label.Set("label_removed", true)
	if err := f.app.Save(label); err != nil {
		t.Fatal(err)
	}
	if status, out := f.send(http.MethodPost, "/api/pc/backup-jobs/"+label.Id+"/run", operator, nil); status != http.StatusConflict ||
		out["message"] != "The pocketcloud.backup label of this job is gone: remove the job." {
		t.Errorf("run a job whose label is gone: %d %v", status, out)
	}
	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+id, f.admin, map[string]any{"targets": []string{f.target.Id}}); status != http.StatusOK {
		t.Fatalf("give it its target back: %d %v", status, out)
	}
	if status, out := f.send(http.MethodPost, "/api/pc/backup-jobs/"+id+"/run", operator, nil); status != http.StatusConflict ||
		out["message"] != "The server node-1 is offline: its agent runs the backup." {
		t.Errorf("run offline: %d %v", status, out)
	}
	if runs, _ := f.app.FindRecordsByFilter(schema.BackupRuns, "trigger='manual'", "", 0, 0); len(runs) != 0 {
		t.Errorf("%d runs were kept", len(runs))
	}
	for _, request := range []struct{ method, url string }{
		{http.MethodPost, "/api/pc/backup-jobs"}, {http.MethodPatch, "/api/pc/backup-jobs/" + id}, {http.MethodDelete, "/api/pc/backup-jobs/" + id},
	} {
		if status, _ := f.send(request.method, request.url, operator, good); status != http.StatusForbidden {
			t.Errorf("an operator's %s %s: %d", request.method, request.url, status)
		}
	}
	if status, _ := f.send(http.MethodPost, "/api/pc/backup-jobs/"+id+"/run", viewer, nil); status != http.StatusForbidden {
		t.Errorf("a viewer's run: %d", status)
	}
	if status, _ := f.send(http.MethodDelete, "/api/pc/backup-jobs/"+id, f.admin, nil); status != http.StatusNoContent {
		t.Errorf("remove: %d", status)
	}
}

// TestAnOfflineServerSkipsItsRuns (#29): at a job's minute, a server that is
// offline gets a skipped run per target and an alert, and the next run is
// set after the pass. A run whose command ended without its finisher is
// settled before the next one starts.
func TestAnOfflineServerSkipsItsRuns(t *testing.T) {
	f := newJobFixture(t)
	// a month that is not this one: the panel's own scheduler, which runs in
	// the tests too, does not start the job by itself
	month := time.Now().UTC().Month()%12 + 1
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data", "schedule": fmt.Sprintf("15 3 * %d *", month)})
	now := time.Date(2030, month, 9, 3, 15, 20, 0, time.UTC)
	// the server is marked offline, long after the panel started
	f.p.SetStarted(time.Now().Add(-3 * time.Minute))
	f.p.RunDueBackups(now.Add(-time.Minute))
	if runs, _ := f.app.FindAllRecords(schema.BackupRuns); len(runs) != 0 {
		t.Fatalf("%d runs before the minute", len(runs))
	}
	f.p.RunDueBackups(now)
	runs, err := f.app.FindAllRecords(schema.BackupRuns)
	if err != nil || len(runs) != 1 {
		t.Fatalf("%d runs: %v", len(runs), err)
	}
	if runs[0].GetString("status") != schema.RunSkipped || runs[0].GetString("error") != "The server node-1 was offline at 03:15 UTC." ||
		!runs[0].GetDateTime("scheduled_at").Time().Equal(now.Truncate(time.Minute)) || runs[0].GetString("trigger") != schema.RunSchedule {
		t.Errorf("the run: %v", runs[0].FieldsData())
	}
	alerts, err := f.app.FindRecordsByFilter(schema.Alerts, "rule='backup_skipped'", "", 0, 0)
	if err != nil || len(alerts) != 1 || alerts[0].GetString("target") != job.Id+":"+f.target.Id ||
		alerts[0].GetString("message") != "Backup data → b2 was skipped: the server node-1 was offline at 03:15 UTC." {
		t.Errorf("alerts: %v %v", alerts, err)
	}
	stored, err := f.app.FindRecordById(schema.BackupJobs, job.Id)
	if err != nil || stored.GetString("last_status") != schema.RunSkipped ||
		!stored.GetDateTime("next_run_at").Time().Equal(time.Date(2030, month, 10, 3, 15, 0, 0, time.UTC)) {
		t.Errorf("the job: %v %v", stored.FieldsData(), err)
	}

	// a run still going: its command is not final
	going := f.run(job)
	command := f.save(schema.Commands, map[string]any{"host": f.host.Id, "type": "backup.run",
		"payload": map[string]any{"run_id": going.Id, "repository": map[string]any{"target_id": f.target.Id, "url": "s3:x"}}})
	going.Set("command", command.Id)
	if err := f.app.Save(going); err != nil {
		t.Fatal(err)
	}
	if !f.p.runGoing(job, f.target) {
		t.Error("a run whose command is queued is not going")
	}
	// one whose command ended without its finisher, as when the panel
	// stopped in between, is settled from its command
	command.Set("status", schema.CommandCancelled)
	if err := f.app.Save(command); err != nil {
		t.Fatal(err)
	}
	if f.p.runGoing(job, f.target) {
		t.Error("a run whose command ended is still going")
	}
	if settled, _ := f.app.FindRecordById(schema.BackupRuns, going.Id); settled.GetString("status") != schema.RunCancelled {
		t.Errorf("settled: %s", settled.GetString("status"))
	}
}

// TestBackupAlertsGoWithWhatTheyAreAbout (#29): the alerts of a job to a
// target are resolved when the target is taken out of the job, when the
// job's label goes, when the job is removed and when the target is: no run
// would resolve them any more.
func TestBackupAlertsGoWithWhatTheyAreAbout(t *testing.T) {
	f := newJobFixture(t)
	nas := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	raise := func(job, target *core.Record, rule string) {
		t.Helper()
		if err := events.RaiseAlert(f.app, rule, job.Id+":"+target.Id, f.host.Id, schema.SeverityWarning, "it went wrong"); err != nil {
			t.Fatal(err)
		}
	}
	open := func() []string {
		t.Helper()
		alerts, err := f.app.FindRecordsByFilter(schema.Alerts, "status!='resolved'", "", 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		keys := []string{}
		for _, alert := range alerts {
			keys = append(keys, alert.GetString("rule")+" "+alert.GetString("target"))
		}
		slices.Sort(keys)
		return keys
	}
	rules := []string{alertBackupFailed, alertBackupRetention, alertBackupSkipped}
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data", "targets": []string{f.target.Id, nas.Id}})
	other := f.job("other", map[string]any{"source_kind": schema.SourceVolume, "source": "b"})
	for _, rule := range rules {
		raise(job, f.target, rule)
		raise(job, nas, rule)
	}
	raise(other, f.target, alertBackupFailed)

	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+job.Id, f.admin, map[string]any{"targets": []string{f.target.Id}}); status != http.StatusOK {
		t.Fatalf("take nas out: %d %v", status, out)
	}
	want := []string{}
	for _, rule := range rules {
		want = append(want, rule+" "+job.Id+":"+f.target.Id)
	}
	want = append(want, alertBackupFailed+" "+other.Id+":"+f.target.Id)
	slices.Sort(want)
	if got := open(); !slices.Equal(got, want) {
		t.Errorf("after nas was taken out:\n%v, not\n%v", got, want)
	}

	// a label's job whose label went no longer runs
	stack := f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "shop"})
	label := f.job("shop-db", map[string]any{"origin": schema.JobLabel, "stack": stack.Id, "service": "db", "source_kind": schema.SourceDump, "engine": schema.DumpPostgres})
	raise(label, f.target, alertBackupFailed)
	f.p.syncBackupLabels(stack, nil)
	if got := open(); !slices.Equal(got, want) {
		t.Errorf("after the label went:\n%v, not\n%v", got, want)
	}

	if status, _ := f.send(http.MethodDelete, "/api/pc/backup-jobs/"+job.Id, f.admin, nil); status != http.StatusNoContent {
		t.Fatalf("remove the job: %d", status)
	}
	want = []string{alertBackupFailed + " " + other.Id + ":" + f.target.Id}
	if got := open(); !slices.Equal(got, want) {
		t.Errorf("after the job was removed: %v", got)
	}

	// one that nothing resolved, as from before the target was the job's
	raise(other, nas, alertBackupSkipped)
	if status, out := f.send(http.MethodDelete, "/api/pc/backup-targets/"+nas.Id, f.admin, nil); status != http.StatusNoContent {
		t.Fatalf("remove nas: %d %v", status, out)
	}
	if got := open(); !slices.Equal(got, want) {
		t.Errorf("after nas was removed: %v", got)
	}
}

// TestARunIsQueuedUnlessItsServerIsOffline (#29): at a job's minute, a
// server without a connection that is not marked offline, being between two
// connections, gets its run queued, to be delivered once its agent is back;
// so does one marked offline while its agent may still be reconnecting after
// the panel started. Only a server marked offline after that gets skipped
// runs and an alert.
func TestARunIsQueuedUnlessItsServerIsOffline(t *testing.T) {
	f := newJobFixture(t)
	between := testutil.NewHost(t, f.app, "node-2")
	between.Set("status", schema.HostDegraded)
	if err := f.app.Save(between); err != nil {
		t.Fatal(err)
	}
	month := time.Now().UTC().Month()%12 + 1
	schedule := fmt.Sprintf("15 3 * %d *", month)
	offline := f.job("offline", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data", "schedule": schedule})
	degraded := f.job("degraded", map[string]any{"host": between.Id, "source_kind": schema.SourceVolume, "source": "app_data", "schedule": schedule})
	now := time.Date(2030, month, 9, 3, 15, 20, 0, time.UTC)
	runOf := func(job *core.Record) *core.Record {
		t.Helper()
		runs, err := f.app.FindRecordsByFilter(schema.BackupRuns, "job={:job}", "-created,-id", 1, 0, map[string]any{"job": job.Id})
		if err != nil || len(runs) != 1 {
			t.Fatalf("the runs of %s: %v %v", job.GetString("name"), runs, err)
		}
		return runs[0]
	}
	queued := func(run *core.Record) bool {
		t.Helper()
		command, err := f.app.FindRecordById(schema.Commands, run.GetString("command"))
		return err == nil && run.GetString("status") == schema.RunQueued && command.GetString("status") == schema.CommandQueued &&
			command.GetString("host") == run.GetString("host")
	}

	// the panel has just started: the agents may still be reconnecting
	f.p.RunDueBackups(now)
	for _, job := range []*core.Record{offline, degraded} {
		if run := runOf(job); !queued(run) {
			t.Errorf("the run of %s just after the start: %v", job.GetString("name"), run.FieldsData())
		}
	}
	if alerts, _ := f.app.FindRecordsByFilter(schema.Alerts, "rule='backup_skipped'", "", 0, 0); len(alerts) != 0 {
		t.Errorf("%d alerts", len(alerts))
	}
	for _, job := range []*core.Record{offline, degraded} {
		if err := f.p.Commands.Cancel(runOf(job).GetString("command"), "Cancelled by admin@example.com"); err != nil {
			t.Fatal(err)
		}
		if run := runOf(job); run.GetString("status") != schema.RunCancelled {
			t.Errorf("the run someone cancelled: %v", run.FieldsData())
		}
	}

	// later
	f.p.SetStarted(time.Now().Add(-3 * time.Minute))
	f.p.RunDueBackups(now.Add(24 * time.Hour))
	if run := runOf(degraded); !queued(run) {
		t.Errorf("the run of a server between two connections: %v", run.FieldsData())
	}
	if run := runOf(offline); run.GetString("status") != schema.RunSkipped || run.GetString("error") != "The server node-1 was offline at 03:15 UTC." {
		t.Errorf("the run of an offline server: %v", run.FieldsData())
	}
	if alerts, _ := f.app.FindRecordsByFilter(schema.Alerts, "rule='backup_skipped'", "", 0, 0); len(alerts) != 1 || alerts[0].GetString("target") != offline.Id+":"+f.target.Id {
		t.Errorf("alerts: %v", alerts)
	}
}

// TestAMinuteThePanelMissedIsCaughtUp (#29): a job whose next run passed
// while the panel was down, less than a day ago, runs once for that minute
// when the panel is back, in the scheduler's first pass; one due at that
// very minute runs for it instead, and a disabled one, or one that was due
// longer ago, does not.
func TestAMinuteThePanelMissedIsCaughtUp(t *testing.T) {
	f := newJobFixture(t)
	month := time.Now().UTC().Month()%12 + 1
	now := time.Date(2030, month, 9, 5, 10, 20, 0, time.UTC)
	at := func(day, hour, minute int) time.Time {
		return time.Date(2030, month, day, hour, minute, 0, 0, time.UTC)
	}
	daily := fmt.Sprintf("0 3 * %d *", month)
	missed := f.job("missed", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data", "schedule": daily, "next_run_at": at(9, 3, 0)})
	long := f.job("long", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data", "schedule": daily, "next_run_at": at(8, 5, 0)})
	off := f.job("off", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data", "schedule": daily, "next_run_at": at(9, 3, 0), "enabled": false})
	due := f.job("due", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data", "schedule": fmt.Sprintf("10 3,5 * %d *", month), "next_run_at": at(9, 3, 10)})
	runs := func(job *core.Record) []*core.Record {
		t.Helper()
		runs, err := f.app.FindRecordsByFilter(schema.BackupRuns, "job={:job}", "created", 0, 0, map[string]any{"job": job.Id})
		if err != nil {
			t.Fatal(err)
		}
		return runs
	}

	f.p.backupCatchUp.Store(true)
	f.p.RunDueBackups(now)
	caught := runs(missed)
	if len(caught) != 1 || caught[0].GetString("trigger") != schema.RunSchedule || !caught[0].GetDateTime("scheduled_at").Time().Equal(at(9, 3, 0)) ||
		caught[0].GetString("status") != schema.RunQueued || caught[0].GetString("command") == "" {
		t.Fatalf("the missed minute's run: %v", caught)
	}
	if stored, _ := f.app.FindRecordById(schema.BackupJobs, missed.Id); !stored.GetDateTime("next_run_at").Time().Equal(at(10, 3, 0)) {
		t.Errorf("the next run: %s", stored.GetDateTime("next_run_at"))
	}
	for _, job := range []*core.Record{long, off} {
		if len(runs(job)) != 0 {
			t.Errorf("%s was caught up", job.GetString("name"))
		}
	}
	if due := runs(due); len(due) != 1 || !due[0].GetDateTime("scheduled_at").Time().Equal(at(9, 5, 10)) {
		t.Errorf("the job due now: %v", due)
	}

	// once: the passes after the first catch nothing up, nor does a second
	// catching up of the same minute
	f.p.RunDueBackups(now.Add(time.Minute))
	missed.Set("next_run_at", at(9, 3, 0))
	if err := f.app.Save(missed); err != nil {
		t.Fatal(err)
	}
	f.p.CatchUpBackups(now)
	if len(runs(missed)) != 1 {
		t.Errorf("%d runs of the missed minute", len(runs(missed)))
	}
}

// TestOldRunsGo (#29): runs older than 60 days go, but each job's newest,
// also while a job has no run at all, as a new one or one without a target.
func TestOldRunsGo(t *testing.T) {
	f := newJobFixture(t)
	busy := f.job("busy", map[string]any{"source_kind": schema.SourceVolume, "source": "a"})
	quiet := f.job("quiet", map[string]any{"source_kind": schema.SourceVolume, "source": "b"})
	stale := f.job("stale", map[string]any{"source_kind": schema.SourceVolume, "source": "c"})
	f.job("never", map[string]any{"source_kind": schema.SourceVolume, "source": "d", "targets": []string{}})
	old := types.NowDateTime().Add(-61 * 24 * time.Hour)
	age := func(run *core.Record, at types.DateTime) {
		t.Helper()
		if _, err := f.app.DB().NewQuery("UPDATE backup_runs SET created={:at}, status='succeeded' WHERE id={:id}").
			Bind(map[string]any{"at": at.String(), "id": run.Id}).Execute(); err != nil {
			t.Fatal(err)
		}
	}
	oldBusy, newBusy, oldQuiet := f.run(busy), f.run(busy), f.run(quiet)
	age(oldBusy, old)
	age(oldQuiet, old)
	// two old runs of a job: the newer of them stays
	older, newer := f.run(stale), f.run(stale)
	age(older, old.Add(-time.Hour))
	age(newer, old)
	f.p.pruneBackupRuns()
	for run, kept := range map[*core.Record]bool{oldBusy: false, newBusy: true, oldQuiet: true, older: false, newer: true} {
		if _, err := f.app.FindRecordById(schema.BackupRuns, run.Id); (err == nil) != kept {
			t.Errorf("run %s kept: %v", run.Id, err == nil)
		}
	}
}
