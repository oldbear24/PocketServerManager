package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/agent"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/gateway"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

// Backup jobs (#29) end to end: the panel's routes, its scheduler and its
// finisher with a real agent, whose engine runs backup jobs with the backup
// tool and a fake restic. fakerestic.Snapshots lists what a repository holds;
// a volume's files are under the engine's VolumeDir; fakedocker's OnExec
// answers a dump's exec in its service's container.

// jobsHarness is the backup harness whose agent also deploys stacks, with the
// stand-in for docker compose, and a local target to back up to.
type jobsHarness struct {
	*backupHarness
	// disk is the local target and its directory, its repository
	disk    *core.Record
	diskDir string
	// fail makes the backups fail as restic would, with its words
	fail atomic.Pointer[string]
}

// newJobsHarness starts the panel and the agent. prepare readies the engine
// before the agent starts, so that its first inventory reports what it adds;
// the options change the gateway before it starts.
func newJobsHarness(t *testing.T, prepare func(engine *fakedocker.Engine), options ...func(*gateway.Server)) *jobsHarness {
	t.Helper()
	b := &backupHarness{harness: newHarness(t, options...)}
	j := &jobsHarness{backupHarness: b}
	b.restic = fakerestic.Install(t)
	b.docker.AddImage("sha256:"+strings.Repeat("a", 64), nil)
	b.docker.AddContainer("pocketcloud-agent", "ghcr.io/oldbear24/pocketcloud-agent:dev", map[string]string{"pocketcloud.role": "agent"})
	b.docker.VolumeRoot = t.TempDir()
	if prepare != nil {
		prepare(b.docker)
	}
	fakerestic.RunJobs(b.docker, agent.BackupTool)
	run := b.docker.OnJob
	b.docker.OnJob = func(job fakedocker.Job) fakedocker.JobResult {
		b.mu.Lock()
		b.jobs = append(b.jobs, job)
		b.mu.Unlock()
		if words := j.fail.Load(); words != nil && isBackupRun(job) {
			return fakedocker.JobResult{Code: 1, Stderr: *words}
		}
		return run(job)
	}
	// after the panel's own finishers, which keep what a check found and how
	// a run went
	b.panel.Commands.OnFinish("backup.init", func(command *core.Record) { b.finished.Store(command.Id, true) })
	b.panel.Commands.OnFinish("backup.run", func(command *core.Record) { b.finished.Store(command.Id, true) })

	t.Setenv("FAKE_COMPOSE_LOG", filepath.Join(t.TempDir(), "compose.log"))
	config := b.agentConfig("backup-node", b.token("backup-node", schema.HostRoleDocker))
	config.DockerCLI = fakeCLI(t)
	config.HostRoot = fakeHostRoot(t)
	b.agent = b.start(config)
	b.waitFor("the agent to connect", func() bool {
		found, err := b.app.FindFirstRecordByData(schema.Hosts, "name", "backup-node")
		if err != nil || !found.GetBool("stream_open") {
			return false
		}
		b.host = found
		return true
	})
	admin, token := testutil.NewUser(t, b.app, "admin@example.com", schema.RoleAdmin)
	b.admin, b.adminID = token, admin.Id

	j.diskDir = filepath.Join(t.TempDir(), "restic")
	status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, map[string]any{
		"name": "disk", "kind": "local", "host": b.host.Id, "config": map[string]any{"path": j.diskDir},
	})
	if status != http.StatusCreated {
		t.Fatalf("the local target: %d %v", status, out)
	}
	j.disk = b.targetNamed("disk")
	b.checksDone()
	return j
}

// addVolume adds a volume to the engine with a file in it.
func addVolume(t *testing.T, engine *fakedocker.Engine, name string, files ...string) {
	t.Helper()
	engine.AddVolume(name)
	dir := engine.VolumeDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		files = []string{"data.txt"}
	}
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("what "+name+" holds"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// isBackupRun tells a backup.run's job from the checks of targets.
func isBackupRun(job fakedocker.Job) bool {
	return len(job.Cmd) > 1 && job.Cmd[0] == "backup-tool" && job.Cmd[1] == "backup"
}

// addS3 adds an S3 target, whose secret key is secret.
func (j *jobsHarness) addS3(name, secret string) *core.Record {
	j.t.Helper()
	body := s3Target(name, secret)
	body["host"] = j.host.Id
	if status, out := j.send(http.MethodPost, "/api/pc/backup-targets", j.admin, body); status != http.StatusCreated {
		j.t.Fatalf("the target %s: %d %v", name, status, out)
	}
	j.checksDone()
	return j.targetNamed(name)
}

// neverDue is a schedule that is never due, as the 30th of February: the
// panel's own scheduler, which runs in the tests too, starts no run of a job
// that a test starts itself.
const neverDue = "0 0 30 2 *"

// addJob adds a job as the admin, with what a job needs by default.
func (j *jobsHarness) addJob(fields map[string]any) map[string]any {
	j.t.Helper()
	body := map[string]any{"host": j.host.Id, "targets": []string{j.disk.Id}, "schedule": neverDue}
	for key, value := range fields {
		body[key] = value
	}
	status, out := j.send(http.MethodPost, "/api/pc/backup-jobs", j.admin, body)
	if status != http.StatusCreated {
		j.t.Fatalf("add a job: %d %v", status, out)
	}
	return out
}

func (j *jobsHarness) job(id string) *core.Record {
	j.t.Helper()
	job, err := j.app.FindRecordById(schema.BackupJobs, id)
	if err != nil {
		j.t.Fatal(err)
	}
	return job
}

func (j *jobsHarness) jobNamed(name string) *core.Record {
	j.t.Helper()
	job, err := j.app.FindFirstRecordByData(schema.BackupJobs, "name", name)
	if err != nil {
		j.t.Fatalf("the job %s: %v", name, err)
	}
	return job
}

// runs are a job's runs, oldest first.
func (j *jobsHarness) runs(jobID string) []*core.Record {
	j.t.Helper()
	runs, err := j.app.FindRecordsByFilter(schema.BackupRuns, "job={:job}", "created,id", 0, 0, dbx.Params{"job": jobID})
	if err != nil {
		j.t.Fatal(err)
	}
	return runs
}

// runNow starts a job's runs and waits until they end.
func (j *jobsHarness) runNow(jobID string) []*core.Record {
	j.t.Helper()
	status, out := j.send(http.MethodPost, "/api/pc/backup-jobs/"+jobID+"/run", j.admin, nil)
	if status != http.StatusOK {
		j.t.Fatalf("run now: %d %v", status, out)
	}
	ended := []*core.Record{}
	for _, id := range out["runs"].([]any) {
		ended = append(ended, j.runEnded(id.(string)))
	}
	return ended
}

// runEnded waits until a run has ended, and its command's finisher, which
// alerts and notes the run on its job after it keeps how the run went, is
// done.
func (j *jobsHarness) runEnded(id string) *core.Record {
	j.t.Helper()
	var run *core.Record
	j.waitFor("the run "+id+" to end", func() bool {
		found, err := j.app.FindRecordById(schema.BackupRuns, id)
		if err != nil {
			return false
		}
		run = found
		switch found.GetString("status") {
		case schema.RunSucceeded, schema.RunFailed, schema.RunCancelled, schema.RunSkipped:
		default:
			return false
		}
		if command := found.GetString("command"); command != "" {
			_, done := j.finished.Load(command)
			return done
		}
		return true
	})
	return run
}

// openAlerts are the alerts of a rule that are still open, by their target.
func (j *jobsHarness) openAlerts(rule string) map[string]*core.Record {
	j.t.Helper()
	alerts, err := j.app.FindRecordsByFilter(schema.Alerts, "rule={:rule} && status!={:resolved}", "", 0, 0,
		dbx.Params{"rule": rule, "resolved": schema.AlertResolved})
	if err != nil {
		j.t.Fatal(err)
	}
	byTarget := map[string]*core.Record{}
	for _, alert := range alerts {
		byTarget[alert.GetString("target")] = alert
	}
	return byTarget
}

// mounts are the mounts of a job container, by where they are mounted.
func mounts(job fakedocker.Job) map[string]map[string]any {
	byTarget := map[string]map[string]any{}
	list, _ := job.Host["Mounts"].([]any)
	for _, raw := range list {
		if mount, ok := raw.(map[string]any); ok {
			target, _ := mount["Target"].(string)
			byTarget[target] = mount
		}
	}
	return byTarget
}

func (j *jobsHarness) backupJobs() []fakedocker.Job {
	jobs := []fakedocker.Job{}
	for _, job := range j.ranJobs() {
		if isBackupRun(job) {
			jobs = append(jobs, job)
		}
	}
	return jobs
}

// snapshotsOf are the snapshots of a job in the local target's repository.
func (j *jobsHarness) snapshotsOf(jobID string) []fakerestic.Snapshot {
	j.t.Helper()
	snapshots := []fakerestic.Snapshot{}
	for _, snapshot := range fakerestic.Snapshots(j.t, j.restic, j.diskDir) {
		if slices.Contains(snapshot.Tags, "job:"+jobID) {
			snapshots = append(snapshots, snapshot)
		}
	}
	return snapshots
}

// dueNow is a schedule due at a minute.
func dueNow(minute time.Time) string {
	return fmt.Sprintf("%d %d * * *", minute.Minute(), minute.Hour())
}

// TestAScheduledBackupStartsInItsMinute (#29): the scheduler starts a job in
// the minute its schedule names: its run's command is dispatched at once, and
// the backup starts within the minute. The job container mounts only the
// volume, read-only, and the snapshot is tagged with the job and named after
// the server.
func TestAScheduledBackupStartsInItsMinute(t *testing.T) {
	j := newJobsHarness(t, func(engine *fakedocker.Engine) { addVolume(t, engine, "app_data") })
	// the scheduler runs at the start of a minute: so does this test
	if now := time.Now(); now.Second() > 40 {
		time.Sleep(now.Truncate(time.Minute).Add(time.Minute + time.Second).Sub(now))
	}
	now := time.Now().UTC()
	job := j.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data", "schedule": dueNow(now)})
	id := job["id"].(string)

	j.panel.RunDueBackups(now)
	runs := j.runs(id)
	if len(runs) != 1 || runs[0].GetString("trigger") != schema.RunSchedule || runs[0].GetString("command") == "" {
		t.Fatalf("the runs: %v", runs)
	}
	command := j.command(runs[0].GetString("command"))
	if command.GetString("type") != "backup.run" || command.GetString("requested_label") != "schedule" || command.GetString("label") != "Backup app → disk" {
		t.Errorf("the command: %v", command.FieldsData())
	}
	j.waitFor("the command to be dispatched", func() bool { return !j.command(command.Id).GetDateTime("dispatched_at").IsZero() })
	run := j.runEnded(runs[0].Id)
	if run.GetString("status") != schema.RunSucceeded || run.GetString("snapshot_id") == "" || run.GetString("error") != "" {
		t.Fatalf("the run: %v", run.FieldsData())
	}
	scheduled, started := run.GetDateTime("scheduled_at").Time(), run.GetDateTime("started_at").Time()
	if !scheduled.Equal(now.Truncate(time.Minute)) || started.IsZero() || started.Sub(scheduled) >= time.Minute || started.Before(scheduled) {
		t.Errorf("scheduled at %s, started at %s", scheduled, started)
	}
	if run.GetInt("duration_ms") < 0 || run.GetDateTime("finished_at").IsZero() {
		t.Errorf("the run's times: %v", run.FieldsData())
	}

	stored := j.job(id)
	if stored.GetString("last_status") != schema.RunSucceeded || stored.GetDateTime("last_run_at").IsZero() ||
		!stored.GetDateTime("next_run_at").Time().Equal(now.Truncate(time.Minute).Add(24*time.Hour)) {
		t.Errorf("the job: %v", stored.FieldsData())
	}

	snapshots := j.snapshotsOf(id)
	if len(snapshots) != 1 || !strings.HasPrefix(snapshots[0].ID, run.GetString("snapshot_id")) || snapshots[0].Hostname != "backup-node" ||
		strings.Join(snapshots[0].Tags, ",") != "pocketcloud,job:"+id || strings.Join(snapshots[0].Paths, ",") != "/volumes/app_data" {
		t.Errorf("the snapshots: %+v", snapshots)
	}
	backups := j.backupJobs()
	if len(backups) != 1 {
		t.Fatalf("%d backup jobs ran", len(backups))
	}
	volume := mounts(backups[0])["/volumes/app_data"]
	if volume["Type"] != "volume" || volume["Source"] != "app_data" || volume["ReadOnly"] != true {
		t.Errorf("the volume's mount: %v", backups[0].Host["Mounts"])
	}

	// the same minute again starts nothing new while nothing is due
	j.panel.RunDueBackups(now.Add(time.Minute))
	if len(j.runs(id)) != 1 {
		t.Error("a run started at a minute that is not due")
	}
}

// TestRunNowBacksUpToEachTarget (#29): an operator backs a job up now, to
// each of its targets, each with its own secrets; a target whose run before
// is still going is left out, and a server that is offline runs nothing.
func TestRunNowBacksUpToEachTarget(t *testing.T) {
	j := newJobsHarness(t, func(engine *fakedocker.Engine) { addVolume(t, engine, "app_data") })
	b2 := j.addS3("b2", "b2-secret-key")
	job := j.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data", "targets": []string{j.disk.Id, b2.Id}})
	id := job["id"].(string)
	_, operator := testutil.NewUser(t, j.app, "operator@example.com", schema.RoleOperator)
	_, viewer := testutil.NewUser(t, j.app, "viewer@example.com", schema.RoleViewer)

	if status, _ := j.send(http.MethodPost, "/api/pc/backup-jobs/"+id+"/run", viewer, nil); status != http.StatusForbidden {
		t.Errorf("a viewer's run: %d", status)
	}
	status, out := j.send(http.MethodPost, "/api/pc/backup-jobs/"+id+"/run", operator, nil)
	if status != http.StatusOK || len(out["runs"].([]any)) != 2 {
		t.Fatalf("run now: %d %v", status, out)
	}
	for _, runID := range out["runs"].([]any) {
		run := j.runEnded(runID.(string))
		command := j.command(run.GetString("command"))
		if run.GetString("status") != schema.RunSucceeded || run.GetString("trigger") != schema.RunManual || command.GetString("requested_label") != "operator@example.com" {
			t.Errorf("a run: %v", run.FieldsData())
		}
	}
	env := ""
	for _, backup := range j.backupJobs() {
		env += strings.Join(backup.Env, "\n") + "\n"
	}
	if !strings.Contains(env, "AWS_SECRET_ACCESS_KEY=b2-secret-key\n") || !strings.Contains(env, "RESTIC_REPOSITORY=s3:http://minio:9000/backups/b2\n") {
		t.Errorf("the jobs' environment:\n%s", env)
	}

	// a run still going keeps the next one of its target out
	entered, release := j.gateJobs(isBackupRun, nil)
	status, out = j.send(http.MethodPost, "/api/pc/backup-jobs/"+id+"/run", operator, nil)
	if status != http.StatusOK || len(out["runs"].([]any)) != 2 {
		t.Fatalf("run now: %d %v", status, out)
	}
	select {
	case <-entered:
	case <-time.After(waitLimit):
		t.Fatal("no backup started")
	}
	j.waitFor("a run to be running", func() bool {
		return slices.ContainsFunc(j.runs(id), func(run *core.Record) bool { return run.GetString("status") == schema.RunRunning })
	})
	if status, out := j.send(http.MethodPost, "/api/pc/backup-jobs/"+id+"/run", operator, nil); status != http.StatusConflict ||
		out["message"] != "Its previous runs are still going." {
		t.Errorf("run again: %d %v", status, out)
	}
	release()
	for _, runID := range out["runs"].([]any) {
		j.runEnded(runID.(string))
	}

	before := len(j.runs(id))
	if err := j.agent.stop(); err != nil {
		t.Fatal(err)
	}
	j.waitFor("the agent to be offline", func() bool { return !j.panel.Commands.Connected(j.host.Id) })
	if status, out := j.send(http.MethodPost, "/api/pc/backup-jobs/"+id+"/run", operator, nil); status != http.StatusConflict ||
		out["message"] != "The server backup-node is offline: its agent runs the backup." {
		t.Errorf("run offline: %d %v", status, out)
	}
	if len(j.runs(id)) != before {
		t.Error("a run was kept for an offline server")
	}
}

// TestRetentionKeepsWhatThePolicySays (#29): after each backup restic
// forgets the job's snapshots that its retention does not keep, and prunes;
// the run counts them. Another job's snapshots in the same repository are not
// the job's to forget.
func TestRetentionKeepsWhatThePolicySays(t *testing.T) {
	j := newJobsHarness(t, func(engine *fakedocker.Engine) {
		addVolume(t, engine, "app_data")
		addVolume(t, engine, "other_data")
	})
	app := j.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data", "retention": map[string]any{"last": 2}})["id"].(string)
	other := j.addJob(map[string]any{"name": "other", "source_kind": "volume", "source": "other_data", "retention": map[string]any{"last": 1}})["id"].(string)

	j.runNow(other)
	forgotten := []int{}
	for range 3 {
		run := j.runNow(app)[0]
		if run.GetString("status") != schema.RunSucceeded {
			t.Fatalf("a run: %v", run.FieldsData())
		}
		forgotten = append(forgotten, run.GetInt("forgotten"))
	}
	if !slices.Equal(forgotten, []int{0, 0, 1}) {
		t.Errorf("forgotten: %v", forgotten)
	}
	if snapshots := j.snapshotsOf(app); len(snapshots) != 2 {
		t.Errorf("the job keeps %d snapshots: %+v", len(snapshots), snapshots)
	}
	j.runNow(other)
	if len(j.snapshotsOf(app)) != 2 || len(j.snapshotsOf(other)) != 1 {
		t.Errorf("after the other job: %d and %d snapshots", len(j.snapshotsOf(app)), len(j.snapshotsOf(other)))
	}
	calls := fakerestic.Calls(t, j.restic)
	if !strings.Contains(calls, "--tag pocketcloud,job:"+app+" --keep-last 2") || !strings.Contains(calls, "--group-by tags") || !strings.Contains(calls, "--prune") {
		t.Errorf("restic ran:\n%s", calls)
	}

	// a retention that fails leaves the backup, and alerts
	t.Setenv("FAKE_RESTIC_FORGET_FAIL", "Fatal: unable to create exclusive lock: repository is already locked by PID 4242")
	run := j.runNow(app)[0]
	if run.GetString("status") != schema.RunSucceeded || run.GetString("snapshot_id") == "" {
		t.Errorf("the run whose retention failed: %v", run.FieldsData())
	}
	alert := j.openAlerts("backup_retention")[app+":"+j.disk.Id]
	if alert == nil || alert.GetString("severity") != schema.SeverityWarning || !strings.Contains(alert.GetString("message"), "repository is already locked") {
		t.Errorf("the retention's alert: %v", alert)
	}
}

// TestAPartialBackupIsAFailureThatKeepsItsSnapshot (#29): a backup that
// saved its snapshot but could not read every file fails, with restic's
// words, and keeps the snapshot's id; it forgets nothing.
func TestAPartialBackupIsAFailureThatKeepsItsSnapshot(t *testing.T) {
	j := newJobsHarness(t, func(engine *fakedocker.Engine) { addVolume(t, engine, "app_data", "data.txt", "locked.db") })
	id := j.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data", "retention": map[string]any{"last": 1}})["id"].(string)
	t.Setenv("FAKE_RESTIC_UNREADABLE", "locked.db")
	run := j.runNow(id)[0]
	if run.GetString("status") != schema.RunFailed || run.GetString("snapshot_id") == "" ||
		!strings.HasPrefix(run.GetString("error"), "Restic saved the snapshot ") || !strings.Contains(run.GetString("error"), "could not read everything") {
		t.Errorf("the run: %v", run.FieldsData())
	}
	if alert := j.openAlerts("backup_failed")[id+":"+j.disk.Id]; alert == nil || !strings.Contains(alert.GetString("message"), "could not read everything") {
		t.Errorf("the alert: %v", alert)
	}
	if strings.Contains(fakerestic.Calls(t, j.restic), " forget ") {
		t.Error("a partial backup applied the retention")
	}
}

// TestADirectoryIsBackedUpAtItsOwnPath (#29): a directory of the server is
// mounted read-only at its own path, so its snapshot names the real path.
func TestADirectoryIsBackedUpAtItsOwnPath(t *testing.T) {
	j := newJobsHarness(t, nil)
	dir := filepath.Join(t.TempDir(), "www")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>hi</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := j.addJob(map[string]any{"name": "www", "source_kind": "path", "source": dir})["id"].(string)
	if run := j.runNow(id)[0]; run.GetString("status") != schema.RunSucceeded {
		t.Fatalf("the run: %v", run.FieldsData())
	}
	backups := j.backupJobs()
	if len(backups) != 1 {
		t.Fatalf("%d backups", len(backups))
	}
	if mount := mounts(backups[0])[dir]; mount["Type"] != "bind" || mount["Source"] != dir || mount["ReadOnly"] != true {
		t.Errorf("the directory's mount: %v", backups[0].Host["Mounts"])
	}
	if snapshots := j.snapshotsOf(id); len(snapshots) != 1 || strings.Join(snapshots[0].Paths, ",") != dir {
		t.Errorf("the snapshots: %+v", snapshots)
	}

	// what the job container needs for itself is refused as it is saved
	if status, out := j.send(http.MethodPost, "/api/pc/backup-jobs", j.admin, map[string]any{"name": "usr", "host": j.host.Id,
		"source_kind": "path", "source": "/usr", "targets": []string{j.disk.Id}, "schedule": neverDue}); status != http.StatusBadRequest ||
		!strings.Contains(out["message"].(string), "/usr holds /usr/bin") {
		t.Errorf("a directory the job needs: %d %v", status, out)
	}
}

// TestAFailedBackupAlertsWithResticsError (#29): a run that fails raises an
// alert of its job and target with restic's error; the next run that
// succeeds resolves it.
func TestAFailedBackupAlertsWithResticsError(t *testing.T) {
	j := newJobsHarness(t, func(engine *fakedocker.Engine) { addVolume(t, engine, "app_data") })
	id := j.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data"})["id"].(string)
	key := id + ":" + j.disk.Id

	words := "Fatal: unable to save snapshot: write /repo/data/4f: no space left on device\n"
	j.fail.Store(&words)
	run := j.runNow(id)[0]
	if run.GetString("status") != schema.RunFailed || !strings.Contains(run.GetString("error"), "no space left on device") {
		t.Errorf("the run: %v", run.FieldsData())
	}
	alert := j.openAlerts("backup_failed")[key]
	if alert == nil || alert.GetString("severity") != schema.SeverityCritical || alert.GetString("host") != j.host.Id ||
		!strings.HasPrefix(alert.GetString("message"), "Backup app → disk failed: ") || !strings.Contains(alert.GetString("message"), "no space left on device") {
		t.Fatalf("the alert: %v", alert)
	}
	if j.job(id).GetString("last_status") != schema.RunFailed {
		t.Errorf("the job's last status: %s", j.job(id).GetString("last_status"))
	}
	if events, err := j.app.FindRecordsByFilter(schema.Events, "kind='backup.failed'", "", 0, 0); err != nil || len(events) != 1 {
		t.Errorf("%d events: %v", len(events), err)
	}

	j.fail.Store(nil)
	if run := j.runNow(id)[0]; run.GetString("status") != schema.RunSucceeded {
		t.Fatalf("the run after: %v", run.FieldsData())
	}
	if alert := j.openAlerts("backup_failed")[key]; alert != nil {
		t.Errorf("the alert is still open: %v", alert.FieldsData())
	}
}

// TestAnOfflineServerSkipsItsBackups (#29): a job whose server is marked
// offline at its minute gets a skipped run per target, and an alert for each;
// nothing is queued. Just after the panel started, while the agents may still
// be reconnecting, the runs are queued instead.
func TestAnOfflineServerSkipsItsBackups(t *testing.T) {
	j := newJobsHarness(t, nil, func(server *gateway.Server) {
		server.OfflineAfter = 500 * time.Millisecond
		server.MonitorInterval = 100 * time.Millisecond
	})
	b2 := j.addS3("b2", "b2-secret-key")
	now := time.Now().UTC()
	id := j.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data", "targets": []string{j.disk.Id, b2.Id},
		"schedule": dueNow(now)})["id"].(string)
	if err := j.agent.stop(); err != nil {
		t.Fatal(err)
	}
	j.waitFor("the server to be marked offline", func() bool {
		host, err := j.app.FindRecordById(schema.Hosts, j.host.Id)
		return err == nil && host.GetString("status") == schema.HostOffline
	})

	// the panel has just started: the runs wait for the server
	j.panel.RunDueBackups(now)
	runs := j.runs(id)
	if len(runs) != 2 {
		t.Fatalf("%d runs", len(runs))
	}
	for _, run := range runs {
		if run.GetString("status") != schema.RunQueued || run.GetString("command") == "" {
			t.Errorf("a run just after the start: %v", run.FieldsData())
		}
		if err := j.panel.Commands.Cancel(run.GetString("command"), "Cancelled by admin@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	if alerts := j.openAlerts("backup_skipped"); len(alerts) != 0 {
		t.Errorf("alerts just after the start: %v", alerts)
	}

	j.panel.SetStarted(time.Now().Add(-3 * time.Minute))
	before := j.commandCount(j.host.Id, "backup.run")
	j.panel.RunDueBackups(now)
	runs = slices.DeleteFunc(j.runs(id), func(run *core.Record) bool { return run.GetString("status") == schema.RunCancelled })
	want := fmt.Sprintf("The server backup-node was offline at %s UTC.", now.Format("15:04"))
	if len(runs) != 2 {
		t.Fatalf("%d runs", len(runs))
	}
	for _, run := range runs {
		if run.GetString("status") != schema.RunSkipped || run.GetString("error") != want {
			t.Errorf("a run: %v", run.FieldsData())
		}
	}
	alerts := j.openAlerts("backup_skipped")
	for _, target := range []string{j.disk.Id, b2.Id} {
		if alert := alerts[id+":"+target]; alert == nil || alert.GetString("severity") != schema.SeverityWarning || !strings.Contains(alert.GetString("message"), "offline") {
			t.Errorf("the alert of %s: %v", target, alert)
		}
	}
	if j.commandCount(j.host.Id, "backup.run") != before {
		t.Error("a backup was queued for an offline server")
	}
	if j.job(id).GetString("last_status") != schema.RunSkipped {
		t.Errorf("the job's last status: %s", j.job(id).GetString("last_status"))
	}
}

// TestAStacksVolumesAreBackedUpTogether (#29): a stack's job backs up every
// named volume of its Compose project, each mounted read-only, and nothing
// else; a stack without volumes fails at once, with an alert.
func TestAStacksVolumesAreBackedUpTogether(t *testing.T) {
	j := newJobsHarness(t, func(engine *fakedocker.Engine) {
		for _, name := range []string{"shop_db", "shop_uploads", "other_data"} {
			addVolume(t, engine, name)
		}
	})
	j.waitFor("the volumes in the inventory", func() bool {
		volumes, err := j.app.FindRecordsByFilter(schema.Volumes, "host={:host}", "", 0, 0, dbx.Params{"host": j.host.Id})
		return err == nil && len(volumes) == 3
	})
	// as Compose labels the volumes it makes
	for name, project := range map[string]string{"shop_db": "shop", "shop_uploads": "shop", "other_data": "other"} {
		volume, err := j.app.FindFirstRecordByFilter(schema.Volumes, "host={:host} && name={:name}", dbx.Params{"host": j.host.Id, "name": name})
		if err != nil {
			t.Fatal(err)
		}
		volume.Set("labels", map[string]string{"com.docker.compose.project": project, "com.docker.compose.volume": strings.TrimPrefix(name, project+"_")})
		if err := j.app.Save(volume); err != nil {
			t.Fatal(err)
		}
	}
	shop := j.saveStack(j.host.Id, "shop", "services: {}\n")
	empty := j.saveStack(j.host.Id, "empty", "services: {}\n")

	job := j.addJob(map[string]any{"name": "shop", "source_kind": "stack", "stack": shop})
	if job["host"] != j.host.Id {
		t.Errorf("the job's server: %v", job["host"])
	}
	id := job["id"].(string)
	if run := j.runNow(id)[0]; run.GetString("status") != schema.RunSucceeded {
		t.Fatalf("the run: %v", run.FieldsData())
	}
	backups := j.backupJobs()
	if len(backups) != 1 {
		t.Fatalf("%d backups", len(backups))
	}
	mounted := mounts(backups[0])
	for _, name := range []string{"shop_db", "shop_uploads"} {
		if mount := mounted["/volumes/"+name]; mount["Source"] != name || mount["ReadOnly"] != true {
			t.Errorf("the mount of %s: %v", name, mount)
		}
	}
	if _, ok := mounted["/volumes/other_data"]; ok {
		t.Error("another stack's volume was mounted")
	}
	if snapshots := j.snapshotsOf(id); len(snapshots) != 1 || strings.Join(snapshots[0].Paths, ",") != "/volumes/shop_db,/volumes/shop_uploads" {
		t.Errorf("the snapshots: %+v", snapshots)
	}

	none := j.addJob(map[string]any{"name": "empty", "source_kind": "stack", "stack": empty})["id"].(string)
	run := j.runNow(none)[0]
	if run.GetString("status") != schema.RunFailed || run.GetString("error") != "The stack empty has no volumes to back up." || run.GetString("command") != "" {
		t.Errorf("a stack without volumes: %v", run.FieldsData())
	}
	if j.openAlerts("backup_failed")[none+":"+j.disk.Id] == nil {
		t.Error("no alert")
	}
}

// TestADatabaseDumpGoesThroughTheRelay (#29): a dump runs with Docker exec in
// the running container of its service, with the engine's own command, and
// reaches restic as one file; a dump that fails is the run's failure, with
// what the dump said, and saves nothing.
func TestADatabaseDumpGoesThroughTheRelay(t *testing.T) {
	var db string
	dump := atomic.Pointer[fakedocker.ExecResult]{}
	dump.Store(&fakedocker.ExecResult{Stdout: "PGDMP the shop's database"})
	j := newJobsHarness(t, func(engine *fakedocker.Engine) {
		db = engine.AddContainer("shop-db-1", "postgres:17", map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.service": "db"})
		engine.OnExec = func(call *fakedocker.ExecCall, _ []byte) fakedocker.ExecResult {
			if call.Container == db && strings.Contains(strings.Join(call.Cmd, " "), "pg_dump") {
				return *dump.Load()
			}
			return fakedocker.ExecResult{Code: 127, Stderr: "sh: unexpected command"}
		}
	})
	j.waitForInventory(j.host.Id, db)
	shop := j.saveStack(j.host.Id, "shop", "services: {}\n")
	id := j.addJob(map[string]any{"name": "shop-db", "source_kind": "db_dump", "stack": shop, "service": "db", "engine": "postgres"})["id"].(string)

	run := j.runNow(id)[0]
	if run.GetString("status") != schema.RunSucceeded {
		t.Fatalf("the run: %v", run.FieldsData())
	}
	call := j.docker.LastExec("shop-db-1")
	if call == nil || len(call.Cmd) != 3 || call.Cmd[0] != "sh" || !strings.Contains(call.Cmd[2], "pg_dump --format=custom --compress=0") {
		t.Errorf("the dump: %+v", call)
	}
	snapshots := j.snapshotsOf(id)
	if len(snapshots) != 1 || !strings.HasSuffix(strings.Join(snapshots[0].Paths, ","), "shop-db.pgdump") {
		t.Errorf("the snapshots: %+v", snapshots)
	}
	for _, backup := range j.backupJobs() {
		for target := range mounts(backup) {
			if strings.HasPrefix(target, "/volumes/") {
				t.Errorf("a dump's job mounts %s", target)
			}
		}
	}

	dump.Store(&fakedocker.ExecResult{Code: 1, Stderr: "pg_dump: error: connection to server on socket \"/var/run/postgresql/.s.PGSQL.5432\" failed\n"})
	run = j.runNow(id)[0]
	if run.GetString("status") != schema.RunFailed || !strings.HasPrefix(run.GetString("error"), "The dump failed with exit code 1, and restic saved no snapshot: ") ||
		!strings.Contains(run.GetString("error"), "pg_dump: error: connection to server") {
		t.Errorf("a dump that failed: %v", run.FieldsData())
	}
	if len(j.snapshotsOf(id)) != 1 {
		t.Error("a dump that failed was saved")
	}
}

// TestLabelsMakeBackupJobs (#29): a deploy makes a job of each service its
// pocketcloud.backup label backs up, with what the labels say; what they do
// not say an admin may change, and a deploy keeps it. A label that is gone,
// or a removed stack, leaves the job disabled until someone removes it.
func TestLabelsMakeBackupJobs(t *testing.T) {
	j := newJobsHarness(t, nil)
	_, operator := testutil.NewUser(t, j.app, "operator@example.com", schema.RoleOperator)
	status, out := j.send(http.MethodPost, "/api/collections/stacks/records", operator, map[string]any{"host": j.host.Id, "name": "shop", "compose_yaml": "services: {}\n"})
	if status != http.StatusOK {
		t.Fatalf("a stack: %d %v", status, out)
	}
	stackID := out["id"].(string)
	deploy := func(compose string) *core.Record {
		t.Helper()
		stack, err := j.app.FindRecordById(schema.Stacks, stackID)
		if err != nil {
			t.Fatal(err)
		}
		before := stack.GetDateTime("labels_at")
		status, out := j.send(http.MethodPost, "/api/pc/stacks/"+stackID+"/deploy", operator, map[string]any{"compose_yaml": compose})
		if status != http.StatusOK {
			t.Fatalf("deploy: %d %v", status, out)
		}
		command, _ := out["command"].(map[string]any)
		if done := j.waitForStatus(command["id"].(string), schema.CommandSucceeded, schema.CommandFailed); done.GetString("status") != schema.CommandSucceeded {
			t.Fatalf("the deploy failed: %s", done.GetString("error"))
		}
		j.waitFor("the labels to be read", func() bool {
			stack, err = j.app.FindRecordById(schema.Stacks, stackID)
			return err == nil && stack.GetDateTime("labels_at").After(before)
		})
		return stack
	}

	stack := deploy(`services:
  app:
    image: app:1
    volumes: [uploads:/srv/uploads]
    labels:
      pocketcloud.backup: volumes
      pocketcloud.backup.target: disk, nowhere
  db:
    image: postgres:17
    labels:
      pocketcloud.backup: postgres
      pocketcloud.backup.schedule: "15 2 * * *"
      pocketcloud.backup.retention: 7d 4w
volumes:
  uploads:
`)
	if problems := problemsOf(stack); len(problems) != 1 || problems[0] != "app: pocketcloud.backup.target names nowhere, which is no backup target." {
		t.Errorf("the stack's problems: %v", problems)
	}
	app, db := j.jobNamed("shop-app"), j.jobNamed("shop-db")
	if app.GetString("origin") != schema.JobLabel || app.GetString("source_kind") != schema.SourceStack || app.GetString("source") != "shop_uploads" ||
		strings.Join(app.GetStringSlice("targets"), ",") != j.disk.Id || !strings.Contains(app.GetString("problem"), "nowhere") ||
		app.GetString("schedule") != "0 3 * * *" || app.GetString("host") != j.host.Id || app.GetDateTime("next_run_at").IsZero() {
		t.Errorf("shop-app: %v", app.FieldsData())
	}
	var retention map[string]int
	_ = db.UnmarshalJSONField("retention", &retention)
	if db.GetString("source_kind") != schema.SourceDump || db.GetString("engine") != schema.DumpPostgres || db.GetString("schedule") != "15 2 * * *" ||
		retention["daily"] != 7 || retention["weekly"] != 4 || retention["monthly"] != 0 || db.GetString("problem") != "Choose where it goes: no target." ||
		!db.GetDateTime("next_run_at").IsZero() {
		t.Errorf("shop-db: %v", db.FieldsData())
	}

	// what the labels do not set, an admin sets; what they set stays theirs
	if status, out := j.send(http.MethodPatch, "/api/pc/backup-jobs/"+db.Id, j.admin, map[string]any{"targets": []string{j.disk.Id}}); status != http.StatusOK ||
		out["problem"] != "" || out["next_run_at"] == "" {
		t.Errorf("give shop-db a target: %d %v", status, out)
	}
	if status, out := j.send(http.MethodPatch, "/api/pc/backup-jobs/"+db.Id, j.admin, map[string]any{"schedule": "0 4 * * *"}); status != http.StatusBadRequest ||
		!strings.Contains(out["message"].(string), "pocketcloud.backup.schedule label of shop (db)") {
		t.Errorf("change shop-db's schedule: %d %v", status, out)
	}
	if status, out := j.send(http.MethodDelete, "/api/pc/backup-jobs/"+db.Id, j.admin, nil); status != http.StatusConflict ||
		out["message"] != "It comes back at the stack's next deploy: remove its pocketcloud.backup label instead." {
		t.Errorf("remove shop-db: %d %v", status, out)
	}

	stack = deploy(`services:
  app:
    image: app:1
    volumes: [uploads:/srv/uploads]
    labels:
      pocketcloud.backup: volumes
      pocketcloud.backup.target: disk
      pocketcloud.backup.schedule: "@hourly"
  db:
    image: postgres:17
volumes:
  uploads:
`)
	if problems := problemsOf(stack); len(problems) != 0 {
		t.Errorf("the stack's problems: %v", problems)
	}
	app, db = j.jobNamed("shop-app"), j.jobNamed("shop-db")
	if app.GetString("problem") != "" || app.GetString("schedule") != "@hourly" || !app.GetBool("enabled") {
		t.Errorf("shop-app: %v", app.FieldsData())
	}
	if !db.GetBool("label_removed") || db.GetBool("enabled") || len(db.GetStringSlice("targets")) != 1 || !db.GetDateTime("next_run_at").IsZero() {
		t.Errorf("shop-db without its label: %v", db.FieldsData())
	}
	if status, _ := j.send(http.MethodDelete, "/api/pc/backup-jobs/"+db.Id, j.admin, nil); status != http.StatusNoContent {
		t.Errorf("remove shop-db: %d", status)
	}

	// a removed stack leaves its jobs marked
	if status, out := j.send(http.MethodDelete, "/api/collections/stacks/records/"+stackID, operator, nil); status != http.StatusNoContent {
		t.Fatalf("remove the stack: %d %v", status, out)
	}
	j.waitFor("the job of the removed stack to be marked", func() bool {
		app := j.jobNamed("shop-app")
		return app.GetBool("label_removed") && !app.GetBool("enabled")
	})
}

// TestBackupJobsKeepTheirSecretsToThemselves (#29): the stored backup.run
// commands, which viewers read, the runs, the jobs, the API's answers and the
// audit log hold no secret of a target; the job containers get them.
func TestBackupJobsKeepTheirSecretsToThemselves(t *testing.T) {
	j := newJobsHarness(t, func(engine *fakedocker.Engine) { addVolume(t, engine, "app_data") })
	b2 := j.addS3("b2", "b2-secret-key-2931")
	job := j.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data", "targets": []string{j.disk.Id, b2.Id}})
	id := job["id"].(string)
	j.runNow(id)

	forbidden := []string{"b2-secret-key-2931"}
	for _, target := range j.targets() {
		password, err := j.panel.Box.OpenString(target.GetString("repo_password"))
		if err != nil {
			t.Fatal(err)
		}
		forbidden = append(forbidden, password)
	}
	_, viewer := testutil.NewUser(t, j.app, "viewer@example.com", schema.RoleViewer)
	kept := []string{job["raw"].(string)}
	for _, url := range []string{"/api/collections/commands/records?perPage=100", "/api/collections/backup_runs/records?perPage=100", "/api/collections/backup_jobs/records"} {
		status, out := j.send(http.MethodGet, url, viewer, nil)
		if status != http.StatusOK {
			t.Fatalf("%s: %d", url, status)
		}
		kept = append(kept, out["raw"].(string))
	}
	commands, err := j.app.FindRecordsByFilter(schema.Commands, "type='backup.run'", "", 0, 0)
	if err != nil || len(commands) != 2 {
		t.Fatalf("%d commands: %v", len(commands), err)
	}
	for _, command := range commands {
		raw, _ := json.Marshal(command.FieldsData())
		kept = append(kept, string(raw))
	}
	audit, err := j.app.FindAllRecords(schema.AuditLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range audit {
		raw, _ := json.Marshal(entry.FieldsData())
		kept = append(kept, string(raw))
	}
	for _, text := range kept {
		for _, secret := range forbidden {
			if strings.Contains(text, secret) {
				t.Errorf("a secret is kept: %.12s… in %.300s", secret, text)
			}
		}
	}
	env := ""
	for _, backup := range j.backupJobs() {
		env += strings.Join(backup.Env, "\n") + "\n"
	}
	for _, secret := range forbidden {
		if !strings.Contains(env, secret) {
			t.Errorf("no job got %.12s…", secret)
		}
	}
}

// TestBackupJobsAreTheAdminsToChange (#29): only admins add, change and
// remove jobs, audited as themselves; a target that jobs back up to stays
// until they no longer do.
func TestBackupJobsAreTheAdminsToChange(t *testing.T) {
	j := newJobsHarness(t, nil)
	_, operator := testutil.NewUser(t, j.app, "operator@example.com", schema.RoleOperator)
	_, viewer := testutil.NewUser(t, j.app, "viewer@example.com", schema.RoleViewer)
	body := map[string]any{"name": "app", "host": j.host.Id, "source_kind": "path", "source": "/srv/app", "targets": []string{j.disk.Id}, "schedule": neverDue}
	for _, token := range []string{operator, viewer} {
		if status, _ := j.send(http.MethodPost, "/api/pc/backup-jobs", token, body); status != http.StatusForbidden {
			t.Errorf("add as another role: %d", status)
		}
		if status, _ := j.send(http.MethodPost, "/api/collections/backup_jobs/records", token, body); status == http.StatusOK {
			t.Error("a job was added through the records API")
		}
	}
	id := j.addJob(body)["id"].(string)
	for _, token := range []string{operator, viewer} {
		if status, _ := j.send(http.MethodPatch, "/api/pc/backup-jobs/"+id, token, map[string]any{"enabled": false}); status != http.StatusForbidden {
			t.Errorf("change as another role: %d", status)
		}
		if status, _ := j.send(http.MethodDelete, "/api/pc/backup-jobs/"+id, token, nil); status != http.StatusForbidden {
			t.Errorf("remove as another role: %d", status)
		}
	}
	if status, out := j.send(http.MethodPatch, "/api/pc/backup-jobs/"+id, j.admin, map[string]any{"schedule": "30 1 * * 1-5"}); status != http.StatusOK ||
		out["schedule"] != "30 1 * * 1-5" {
		t.Errorf("change: %d %v", status, out)
	}
	entries, err := j.app.FindRecordsByFilter(schema.AuditLog, "collection={:c} && record_id={:id}", "seq", 0, 0, dbx.Params{"c": schema.BackupJobs, "id": id})
	if err != nil || len(entries) != 2 {
		t.Fatalf("%d audit entries: %v", len(entries), err)
	}
	for _, entry := range entries {
		if entry.GetString("actor") != j.adminID || entry.GetString("actor_kind") != "user" {
			t.Errorf("%s by %s %s", entry.GetString("action"), entry.GetString("actor_kind"), entry.GetString("actor_label"))
		}
	}

	if status, out := j.send(http.MethodDelete, "/api/pc/backup-targets/"+j.disk.Id, j.admin, nil); status != http.StatusConflict ||
		out["message"] != "The job app backs up to this target: take it out of them first." {
		t.Errorf("remove a target in use: %d %v", status, out)
	}
	if status, out := j.send(http.MethodPatch, "/api/pc/backup-jobs/"+id, j.admin, map[string]any{"targets": []string{}}); status != http.StatusOK {
		t.Fatalf("take the target out: %d %v", status, out)
	}
	if status, out := j.send(http.MethodDelete, "/api/pc/backup-targets/"+j.disk.Id, j.admin, nil); status != http.StatusNoContent {
		t.Errorf("remove the target: %d %v", status, out)
	}
	if status, _ := j.send(http.MethodDelete, "/api/pc/backup-jobs/"+id, j.admin, nil); status != http.StatusNoContent {
		t.Errorf("remove the job: %d", status)
	}
}

// TestABackupRunToATargetTakenOutOfItsJobGetsNoSecrets (#29): a run queued
// behind another one to the same target, whose target is taken out of its
// job meanwhile, is not delivered: the server gets none of the target's
// secrets, and the run is cancelled, with no alert.
func TestABackupRunToATargetTakenOutOfItsJobGetsNoSecrets(t *testing.T) {
	j := newJobsHarness(t, func(engine *fakedocker.Engine) {
		addVolume(t, engine, "app_data")
		addVolume(t, engine, "other_data")
	})
	b2 := j.addS3("b2", "b2-secret-key-taken-out")
	first := j.addJob(map[string]any{"name": "first", "source_kind": "volume", "source": "app_data", "targets": []string{b2.Id}})["id"].(string)
	second := j.addJob(map[string]any{"name": "second", "source_kind": "volume", "source": "other_data", "targets": []string{b2.Id}})["id"].(string)

	entered, release := j.gateJobs(isBackupRun, nil)
	status, out := j.send(http.MethodPost, "/api/pc/backup-jobs/"+first+"/run", j.admin, nil)
	if status != http.StatusOK {
		t.Fatalf("run first: %d %v", status, out)
	}
	firstRun := out["runs"].([]any)[0].(string)
	select {
	case <-entered:
	case <-time.After(waitLimit):
		t.Fatal("the first backup did not start")
	}
	// the second waits for the first, as runs to one target from one server do
	status, out = j.send(http.MethodPost, "/api/pc/backup-jobs/"+second+"/run", j.admin, nil)
	if status != http.StatusOK {
		t.Fatalf("run second: %d %v", status, out)
	}
	secondRun := out["runs"].([]any)[0].(string)
	queued := j.runs(second)[0]
	if command := j.command(queued.GetString("command")); command.GetString("status") != schema.CommandQueued {
		t.Fatalf("the second run's command: %s", command.GetString("status"))
	}
	if status, out := j.send(http.MethodPatch, "/api/pc/backup-jobs/"+second, j.admin, map[string]any{"targets": []string{j.disk.Id}}); status != http.StatusOK {
		t.Fatalf("take b2 out: %d %v", status, out)
	}
	release()

	if run := j.runEnded(firstRun); run.GetString("status") != schema.RunSucceeded {
		t.Errorf("the first run: %v", run.FieldsData())
	}
	run := j.runEnded(secondRun)
	if run.GetString("status") != schema.RunCancelled || !strings.HasSuffix(run.GetString("error"), "the target is no longer one of the job's targets") {
		t.Errorf("the second run: %v", run.FieldsData())
	}
	if len(j.openAlerts("backup_failed")) != 0 {
		t.Errorf("alerts: %v", j.openAlerts("backup_failed"))
	}
	given := 0
	for _, job := range j.backupJobs() {
		if slices.Contains(job.Env, "AWS_SECRET_ACCESS_KEY=b2-secret-key-taken-out") {
			given++
		}
	}
	if given != 1 {
		t.Errorf("%d backup jobs got b2's secrets", given)
	}
}

// TestBackupAlertsGoWithTheirJobAndTarget (#29): a job's alerts to a target
// are resolved when the target is taken out of the job, and when the job is
// removed: no run would resolve them any more.
func TestBackupAlertsGoWithTheirJobAndTarget(t *testing.T) {
	j := newJobsHarness(t, func(engine *fakedocker.Engine) { addVolume(t, engine, "app_data") })
	b2 := j.addS3("b2", "b2-secret-key")
	id := j.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data", "targets": []string{j.disk.Id, b2.Id}})["id"].(string)
	words := "Fatal: unable to save snapshot: no space left on device\n"
	j.fail.Store(&words)
	j.runNow(id)
	if alerts := j.openAlerts("backup_failed"); alerts[id+":"+j.disk.Id] == nil || alerts[id+":"+b2.Id] == nil {
		t.Fatalf("the alerts: %v", alerts)
	}

	if status, out := j.send(http.MethodPatch, "/api/pc/backup-jobs/"+id, j.admin, map[string]any{"targets": []string{j.disk.Id}}); status != http.StatusOK {
		t.Fatalf("take b2 out: %d %v", status, out)
	}
	if alerts := j.openAlerts("backup_failed"); len(alerts) != 1 || alerts[id+":"+j.disk.Id] == nil {
		t.Errorf("after b2 was taken out: %v", alerts)
	}
	if status, _ := j.send(http.MethodDelete, "/api/pc/backup-jobs/"+id, j.admin, nil); status != http.StatusNoContent {
		t.Fatalf("remove the job: %d", status)
	}
	if alerts := j.openAlerts("backup_failed"); len(alerts) != 0 {
		t.Errorf("after the job was removed: %v", alerts)
	}
}

// TestABackupJobOfARemovedStackStopsUntilItIsChanged (#29): a job an admin
// made of a stack stops when the stack is removed, instead of failing at
// each of its runs, and says why; pointed at another stack, it runs again
// without the problem.
func TestABackupJobOfARemovedStackStopsUntilItIsChanged(t *testing.T) {
	j := newJobsHarness(t, nil)
	_, operator := testutil.NewUser(t, j.app, "operator@example.com", schema.RoleOperator)
	shop := j.saveStack(j.host.Id, "shop", "services: {}\n")
	other := j.saveStack(j.host.Id, "other", "services: {}\n")
	now := time.Now().UTC()
	id := j.addJob(map[string]any{"name": "shop", "source_kind": "stack", "stack": shop, "schedule": dueNow(now)})["id"].(string)

	if status, out := j.send(http.MethodDelete, "/api/collections/stacks/records/"+shop, operator, nil); status != http.StatusNoContent {
		t.Fatalf("remove the stack: %d %v", status, out)
	}
	j.waitFor("the job to stop", func() bool { return !j.job(id).GetBool("enabled") })
	job := j.job(id)
	if job.GetString("problem") != "Its stack was removed." || !job.GetDateTime("next_run_at").IsZero() {
		t.Errorf("the job: %v", job.FieldsData())
	}
	// the event follows the job's save
	j.waitFor("the event of the stopped job", func() bool {
		events, err := j.app.FindRecordsByFilter(schema.Events, "kind='backup.job_disabled'", "", 0, 0)
		return err == nil && len(events) == 1
	})
	j.panel.RunDueBackups(now)
	if runs := j.runs(id); len(runs) != 0 {
		t.Errorf("it ran: %v", runs[0].FieldsData())
	}
	if alerts := j.openAlerts("backup_failed"); len(alerts) != 0 {
		t.Errorf("alerts: %v", alerts)
	}

	status, out := j.send(http.MethodPatch, "/api/pc/backup-jobs/"+id, j.admin, map[string]any{"stack": other, "enabled": true})
	if status != http.StatusOK || out["problem"] != "" || out["enabled"] != true || out["next_run_at"] == "" {
		t.Errorf("point it at another stack: %d %v", status, out)
	}
}

// TestABackupWaitsForItsServerToReconnect (#29): a server that has no
// connection at a job's minute but is not marked offline, as between two
// connections, gets its run queued: the run is delivered once the agent is
// back, and is no skip.
func TestABackupWaitsForItsServerToReconnect(t *testing.T) {
	j := newJobsHarness(t, func(engine *fakedocker.Engine) { addVolume(t, engine, "app_data") })
	j.panel.SetStarted(time.Now().Add(-3 * time.Minute))
	now := time.Now().UTC()
	id := j.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data", "schedule": dueNow(now)})["id"].(string)
	if err := j.agent.stop(); err != nil {
		t.Fatal(err)
	}
	j.waitFor("the agent to be gone", func() bool { return !j.panel.Commands.Connected(j.host.Id) })

	j.panel.RunDueBackups(now)
	runs := j.runs(id)
	if len(runs) != 1 || runs[0].GetString("status") != schema.RunQueued || runs[0].GetString("command") == "" {
		t.Fatalf("the runs: %v", runs)
	}
	j.agent = j.start(j.agent.config)
	run := j.runEnded(runs[0].Id)
	if run.GetString("status") != schema.RunSucceeded || run.GetString("trigger") != schema.RunSchedule ||
		!run.GetDateTime("scheduled_at").Time().Equal(now.Truncate(time.Minute)) || len(j.snapshotsOf(id)) != 1 {
		t.Errorf("the run: %v", run.FieldsData())
	}
	if alerts := j.openAlerts("backup_skipped"); len(alerts) != 0 {
		t.Errorf("alerts: %v", alerts)
	}
}

// TestABackupThePanelMissedIsCaughtUp (#29): a job whose minute passed while
// the panel was down, less than a day ago, runs once for that minute when the
// panel is back.
func TestABackupThePanelMissedIsCaughtUp(t *testing.T) {
	j := newJobsHarness(t, func(engine *fakedocker.Engine) { addVolume(t, engine, "app_data") })
	now := time.Now().UTC()
	missed := now.Truncate(time.Minute).Add(-2 * time.Hour)
	id := j.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data", "schedule": dueNow(missed)})["id"].(string)
	// as the panel stopped before the minute
	job := j.job(id)
	job.Set("next_run_at", missed)
	if err := j.app.Save(job); err != nil {
		t.Fatal(err)
	}

	j.panel.CatchUpBackups(now)
	runs := j.runs(id)
	if len(runs) != 1 || runs[0].GetString("trigger") != schema.RunSchedule || !runs[0].GetDateTime("scheduled_at").Time().Equal(missed) {
		t.Fatalf("the runs: %v", runs)
	}
	if run := j.runEnded(runs[0].Id); run.GetString("status") != schema.RunSucceeded {
		t.Errorf("the run: %v", run.FieldsData())
	}
	if next := j.job(id).GetDateTime("next_run_at").Time(); !next.Equal(missed.Add(24 * time.Hour)) {
		t.Errorf("the next run: %s", next)
	}
	j.panel.CatchUpBackups(now)
	if len(j.runs(id)) != 1 {
		t.Error("the minute was caught up twice")
	}
}

// TestABackupThatNeverStartsIsSkippedWithAnAlert (#29): a run that waits
// its whole time behind the runs before it of the same target, and never
// starts, is skipped with an alert that says why; one that someone cancels
// stays cancelled, without one.
func TestABackupThatNeverStartsIsSkippedWithAnAlert(t *testing.T) {
	j := newJobsHarness(t, func(engine *fakedocker.Engine) {
		addVolume(t, engine, "app_data")
		addVolume(t, engine, "other_data")
	})
	first := j.addJob(map[string]any{"name": "first", "source_kind": "volume", "source": "app_data"})["id"].(string)
	second := j.addJob(map[string]any{"name": "second", "source_kind": "volume", "source": "other_data"})["id"].(string)
	start := func(id string) string {
		t.Helper()
		status, out := j.send(http.MethodPost, "/api/pc/backup-jobs/"+id+"/run", j.admin, nil)
		if status != http.StatusOK {
			t.Fatalf("run %s: %d %v", id, status, out)
		}
		return out["runs"].([]any)[0].(string)
	}

	entered, release := j.gateJobs(isBackupRun, nil)
	firstRun := start(first)
	select {
	case <-entered:
	case <-time.After(waitLimit):
		t.Fatal("the first backup did not start")
	}
	waiting := start(second)
	// its time is up while it waits for the first
	command := j.command(j.runs(second)[0].GetString("command"))
	command.Set("expires_at", time.Now().Add(-time.Second))
	if err := j.app.Save(command); err != nil {
		t.Fatal(err)
	}
	j.panel.Commands.Reap()
	run := j.runEnded(waiting)
	if run.GetString("status") != schema.RunSkipped || run.GetString("error") != "It waited 12 hours while the server backup-node was busy with other backups, and never started." {
		t.Errorf("the run that never started: %v", run.FieldsData())
	}
	alert := j.openAlerts("backup_skipped")[second+":"+j.disk.Id]
	if alert == nil || alert.GetString("severity") != schema.SeverityWarning || !strings.Contains(alert.GetString("message"), "never started") {
		t.Errorf("the alert: %v", alert)
	}

	cancelled := start(second)
	queued := j.runs(second)[1]
	if status, out := j.send(http.MethodPost, "/api/pc/commands/"+queued.GetString("command")+"/cancel", j.admin, nil); status != http.StatusNoContent {
		t.Fatalf("cancel: %d %v", status, out)
	}
	if run := j.runEnded(cancelled); run.GetString("status") != schema.RunCancelled || run.GetString("error") != "Cancelled by admin@example.com" {
		t.Errorf("the run someone cancelled: %v", run.FieldsData())
	}
	release()
	if run := j.runEnded(firstRun); run.GetString("status") != schema.RunSucceeded {
		t.Errorf("the first run: %v", run.FieldsData())
	}
}
