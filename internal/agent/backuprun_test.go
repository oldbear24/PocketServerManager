package agent

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

// runAgent is an agent that runs backup jobs, and the jobs it ran.
type runAgent struct {
	*Agent
	engine *fakedocker.Engine
	root   string
	mu     sync.Mutex
	jobs   []fakedocker.Job
}

func newRunAgent(t *testing.T) *runAgent {
	t.Helper()
	a, engine, root := backupAgent(t)
	engine.VolumeRoot = t.TempDir()
	r := &runAgent{Agent: a, engine: engine, root: root}
	run := engine.OnJob
	engine.OnJob = func(job fakedocker.Job) fakedocker.JobResult {
		r.mu.Lock()
		r.jobs = append(r.jobs, job)
		r.mu.Unlock()
		return run(job)
	}
	return r
}

// jobCount is how many jobs ran.
func (r *runAgent) jobCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.jobs)
}

// lastJob is the job that ran last.
func (r *runAgent) lastJob(t *testing.T) fakedocker.Job {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.jobs) == 0 {
		t.Fatal("no job ran")
	}
	return r.jobs[len(r.jobs)-1]
}

// remote is a target's repository the panel completed, created.
func (r *runAgent) remote(t *testing.T) *agentv1.BackupRepository {
	t.Helper()
	repo := &agentv1.BackupRepository{
		TargetId: "t1", Url: "s3:http://minio:9000/backups", Password: "repo-password-1",
		Env: map[string]string{"AWS_ACCESS_KEY_ID": "minio-key", "AWS_SECRET_ACCESS_KEY": "minio-secret"},
	}
	if _, err := r.handleBackupInit(context.Background(), initCommand(repo), &Reporter{}); err != nil {
		t.Fatal(err)
	}
	return repo
}

// volume makes a volume of the engine with files.
func (r *runAgent) volume(t *testing.T, name string, files map[string]string) {
	t.Helper()
	r.engine.AddVolume(name)
	writeFiles(t, r.engine.VolumeDir(name), files)
}

func runCommand(run *agentv1.BackupRun) *agentv1.Command {
	return &agentv1.Command{Type: "backup.run", Payload: &agentv1.Command_BackupRun{BackupRun: run}}
}

// mountsOf are a job's mounts by their target.
func mountsOf(job fakedocker.Job) map[string]map[string]any {
	mounts := map[string]map[string]any{}
	list, _ := job.Host["Mounts"].([]any)
	for _, raw := range list {
		mount := raw.(map[string]any)
		mounts[mount["Target"].(string)] = mount
	}
	return mounts
}

// TestABackupRunMountsItsVolumesReadOnly (#29): the job mounts each volume at
// /volumes/<name> read-only, besides restic's cache, has no capability but
// DAC_OVERRIDE, to read what it backs up (never DAC_READ_SEARCH, with which
// open_by_handle_at reaches past its mounts), and restic saves one snapshot
// with the run's host and tags; the result is restic's summary, the
// retention's count and the log, without a secret.
func TestABackupRunMountsItsVolumesReadOnly(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	r.volume(t, "umami_data", map[string]string{"events.db": "events", "conf/app.ini": "x=1"})
	r.volume(t, "umami_cache", map[string]string{"c": "cached"})
	run := &agentv1.BackupRun{
		RunId: "r1", Repository: repo, Volumes: []string{"umami_data", "umami_cache"},
		Tags: []string{"pocketcloud", "job:j1"}, HostName: "nas", Retention: &agentv1.BackupRetention{Last: 3, Daily: 7},
	}
	result, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
	if err != nil {
		t.Fatalf("%v: %v", err, result)
	}
	snapshots := fakerestic.Snapshots(t, r.root, repo.GetUrl())
	if len(snapshots) != 1 {
		t.Fatalf("snapshots: %+v", snapshots)
	}
	snapshot := snapshots[0]
	if snapshot.Hostname != "nas" || !slices.Equal(snapshot.Tags, []string{"pocketcloud", "job:j1"}) ||
		!slices.Equal(snapshot.Paths, []string{"/volumes/umami_data", "/volumes/umami_cache"}) ||
		snapshot.Files["/volumes/umami_data/conf/app.ini"] != "x=1" || snapshot.Files["/volumes/umami_cache/c"] != "cached" {
		t.Errorf("the snapshot: %+v", snapshot)
	}
	log, _ := result["log"].(string)
	if result["snapshot_id"] != snapshot.ID || result["files_new"] != int64(3) || result["files_changed"] != int64(0) ||
		result["bytes_added"] != int64(15) || result["total_bytes"] != int64(15) || result["forgotten"] != int64(0) ||
		result["refused"] != nil || result["retention_error"] != nil || len(result) != 7 {
		t.Errorf("the result: %v", result)
	}
	if !strings.Contains(log, "Saved the snapshot "+snapshot.ID[:8]) || !strings.Contains(log, "Applying the retention: keep last 3, daily 7") ||
		strings.Contains(log, "repo-password-1") || strings.Contains(log, "minio-secret") {
		t.Errorf("the log:\n%s", log)
	}

	job := r.lastJob(t)
	if !slices.Equal(job.Cmd, []string{"backup-tool", "backup", "--host", "nas", "--tag", "pocketcloud", "--tag", "job:j1",
		"--keep-last", "3", "--keep-daily", "7", "--path", "/volumes/umami_data", "--path", "/volumes/umami_cache", "--cache", "/cache"}) {
		t.Errorf("the job's command: %q", job.Cmd)
	}
	mounts := mountsOf(job)
	if len(mounts) != 3 || mounts["/cache"]["Source"] != resticCache {
		t.Errorf("mounts: %v", mounts)
	}
	for _, name := range []string{"umami_data", "umami_cache"} {
		if mount := mounts["/volumes/"+name]; mount["Type"] != "volume" || mount["Source"] != name || mount["ReadOnly"] != true {
			t.Errorf("the volume %s: %v", name, mount)
		}
	}
	if job.Host["NetworkMode"] != "bridge" || !slices.Equal(anyStrings(job.Host["CapDrop"]), []string{"ALL"}) ||
		!slices.Equal(anyStrings(job.Host["CapAdd"]), []string{"DAC_OVERRIDE"}) ||
		!slices.Equal(anyStrings(job.Host["SecurityOpt"]), []string{"no-new-privileges"}) || job.Stdin != nil {
		t.Errorf("host config: %v", job.Host)
	}
	if job.Labels["pocketcloud.role"] != "job" || job.Labels[backupTargetLabel] != "t1" {
		t.Errorf("labels: %v", job.Labels)
	}
	if calls := fakerestic.Calls(t, r.root); !strings.Contains(calls, " forget --json --prune --retry-lock 30m --group-by tags --tag pocketcloud,job:j1 --keep-last 3 --keep-daily 7\n") {
		t.Errorf("restic ran:\n%s", calls)
	}
	for _, name := range r.engine.Names() {
		if name != "pocketcloud-agent" {
			t.Errorf("a container is left: %s", name)
		}
	}

	// the retention counts what went
	for range 3 {
		if _, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{}); err != nil {
			t.Fatal(err)
		}
	}
	run.Retention = &agentv1.BackupRetention{Last: 2}
	result, err = r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
	if err != nil || result["forgotten"] != int64(3) || result["files_new"] != int64(0) || result["bytes_added"] != int64(0) {
		t.Errorf("the last run: %v %v", result, err)
	}
	if snapshots := fakerestic.Snapshots(t, r.root, repo.GetUrl()); len(snapshots) != 2 {
		t.Errorf("%d snapshots", len(snapshots))
	}
}

// TestABackupRunOfDirectoriesToALocalTarget (#29): a directory of the server
// is bind-mounted read-only at its own path, which the snapshot names; a
// local target's job has no network, and may read and write its directory.
func TestABackupRunOfDirectoriesToALocalTarget(t *testing.T) {
	r := newRunAgent(t)
	repoDir := filepath.Join(t.TempDir(), "restic")
	repo := &agentv1.BackupRepository{TargetId: "t2", Url: backup.LocalMount, Password: "repo-password-2", Mount: repoDir}
	if _, err := r.handleBackupInit(context.Background(), initCommand(repo), &Reporter{}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "srv", "photos")
	writeFiles(t, dir, map[string]string{"cat.jpg": "meow"})
	result, err := r.handleBackupRun(context.Background(), runCommand(&agentv1.BackupRun{
		RunId: "r2", Repository: repo, Paths: []string{dir}, Tags: []string{"pocketcloud", "job:j2"}, HostName: "nas",
	}), &Reporter{})
	if err != nil {
		t.Fatalf("%v: %v", err, result)
	}
	job := r.lastJob(t)
	mount := mountsOf(job)[dir]
	if mount["Type"] != "bind" || mount["Source"] != dir || mount["ReadOnly"] != true {
		t.Errorf("the directory's mount: %v", mount)
	}
	if job.Host["NetworkMode"] != "none" || !slices.Equal(anyStrings(job.Host["CapAdd"]), []string{"DAC_OVERRIDE", "FOWNER"}) {
		t.Errorf("host config: %v", job.Host)
	}
	if slices.Contains(job.Cmd, "--keep-last") || !slices.Contains(job.Cmd, "--local") {
		t.Errorf("the job's command: %q", job.Cmd)
	}
	snapshots := fakerestic.Snapshots(t, r.root, repoDir)
	if len(snapshots) != 1 || snapshots[0].Files[dir+"/cat.jpg"] != "meow" || result["snapshot_id"] != snapshots[0].ID {
		t.Errorf("snapshots: %+v, result %v", snapshots, result)
	}
	if calls := fakerestic.Calls(t, r.root); strings.Contains(calls, " forget ") {
		t.Errorf("a run without retention forgot:\n%s", calls)
	}
}

// TestWhatABackupRunCannotReadIsRefused (#29): a volume that is not there is
// refused before any job, so that Docker never makes an empty one; so are
// directories that are not the server's to back up, and runs the panel did
// not complete.
func TestWhatABackupRunCannotReadIsRefused(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	jobs := r.jobCount()
	r.volume(t, "here", nil)
	ok := func() *agentv1.BackupRun {
		return &agentv1.BackupRun{Repository: repo, Volumes: []string{"here"}, Tags: []string{"pocketcloud", "job:j"}, HostName: "nas"}
	}
	for _, tc := range []struct {
		change func(*agentv1.BackupRun)
		want   string
	}{
		{func(b *agentv1.BackupRun) { b.Volumes = []string{"here", "ghost"} }, "the volume ghost does not exist on this server"},
		{func(b *agentv1.BackupRun) { b.Volumes = []string{"../etc"} }, `"../etc" is not a volume's name`},
		{func(b *agentv1.BackupRun) { b.Volumes = []string{"here", "here"} }, "there twice"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/"} }, "the whole of /"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"srv/app"} }, "absolute and clean"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/srv/../etc"} }, "absolute and clean"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/srv/"} }, "absolute and clean"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/proc/1"} }, "the kernel's"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/sys"} }, "the kernel's"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/dev/sda"} }, "the kernel's"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/usr"} }, "would hide the backup job's own /usr/bin"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/var"} }, "would hide the backup job's own /var/lib/pocketcloud"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/tmp"} }, "/tmp"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/repo/x"} }, "in the backup job's own /repo"},
		{func(b *agentv1.BackupRun) { b.Paths = []string{"/srv"} }, "exactly one of them"},
		{func(b *agentv1.BackupRun) { b.Volumes = nil }, "exactly one of them"},
		{func(b *agentv1.BackupRun) { b.Tags = nil }, "needs its job's tags"},
		{func(b *agentv1.BackupRun) { b.HostName = "" }, "needs the server's name"},
		{func(b *agentv1.BackupRun) { b.Retention = &agentv1.BackupRetention{Weekly: -1} }, "not -1"},
		{func(b *agentv1.BackupRun) { b.Repository = &agentv1.BackupRepository{Url: repo.GetUrl()} }, "no password"},
	} {
		run := ok()
		tc.change(run)
		if _, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", run, err, tc.want)
		}
	}
	if r.jobCount() != jobs || r.engine.Called("/volumes/create") {
		t.Errorf("%d jobs ran", r.jobCount()-jobs)
	}
	// a directory that is not there: the Engine says so, as the agent cannot see it
	r.engine.Fail("POST /containers/create", `invalid mount config for type "bind": bind source path does not exist: /srv/no where`)
	run := ok()
	run.Volumes, run.Paths = nil, []string{"/srv/no where"}
	if _, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{}); err == nil || err.Error() != "the directory /srv/no where does not exist on this server" {
		t.Errorf("a directory that is not there: %v", err)
	}
	r.engine.Fail("POST /containers/create", "")
	// a directory of the system that the job has no use for is the server's to back up
	if err := checkBackupPath("/etc"); err != nil {
		t.Errorf("/etc: %v", err)
	}
	if err := checkBackupPath("/var/lib/app"); err != nil {
		t.Errorf("/var/lib/app: %v", err)
	}
}

// TestADumpIsFedThroughTheJobsInput (#29): the job is attached before it
// starts; the dump runs in its database's container with docker exec, and
// what it prints reaches restic through the job's input and the relay as
// the snapshot's one file.
func TestADumpIsFedThroughTheJobsInput(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	db := r.engine.AddContainer("umami-db-1", "postgres:16", map[string]string{"com.docker.compose.service": "db"})
	dump := strings.Repeat("COPY public.website (id, name) FROM stdin;\n", 3000)
	command := []string{"sh", "-c", `exec pg_dump --format=custom --username="${POSTGRES_USER:-postgres}" "${POSTGRES_DB:-postgres}"`}
	var calls []*fakedocker.ExecCall
	var mu sync.Mutex
	answer := fakedocker.ExecResult{Stdout: dump, Stderr: "pg_dump: warning: a note\n"}
	r.engine.OnExec = func(call *fakedocker.ExecCall, _ []byte) fakedocker.ExecResult {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, call)
		return answer
	}
	answers := func(result fakedocker.ExecResult) {
		mu.Lock()
		defer mu.Unlock()
		answer = result
	}
	run := &agentv1.BackupRun{
		RunId: "r3", Repository: repo, StdinCommand: command, StdinContainer: db, StdinFileName: "umami-db.pgdump",
		Tags: []string{"pocketcloud", "job:j3"}, HostName: "nas",
	}
	result, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
	if err != nil {
		t.Fatalf("%v: %v", err, result)
	}
	snapshots := fakerestic.Snapshots(t, r.root, repo.GetUrl())
	if len(snapshots) != 1 || snapshots[0].Files["/umami-db.pgdump"] != dump || result["total_bytes"] != int64(len(dump)) {
		t.Fatalf("snapshots: %d, result %v", len(snapshots), result)
	}
	mu.Lock()
	if len(calls) != 1 || calls[0].Container != db || !slices.Equal(calls[0].Cmd, command) || calls[0].TTY || calls[0].Stdin {
		t.Errorf("the dump ran: %+v", calls)
	}
	mu.Unlock()
	if log, _ := result["log"].(string); !strings.Contains(log, "pg_dump: warning: a note") {
		t.Errorf("the log:\n%s", log)
	}
	job := r.lastJob(t)
	if !slices.Equal(job.Cmd, []string{"backup-tool", "backup", "--host", "nas", "--tag", "pocketcloud", "--tag", "job:j3",
		"--stdin-name", "umami-db.pgdump", "--relay", dumpSocket, "--cache", "/cache"}) {
		t.Errorf("the job's command: %q", job.Cmd)
	}
	if len(mountsOf(job)) != 1 || !slices.Equal(anyStrings(job.Host["CapAdd"]), []string{"DAC_OVERRIDE"}) || job.Stdin == nil {
		t.Errorf("the job: %v", job.Host)
	}
	// attached before it started
	attached, started := -1, -1
	for i, call := range r.engine.Calls() {
		if strings.Contains(call, "/attach?") {
			attached = i
		}
		if strings.HasPrefix(call, "POST /containers/") && strings.Contains(call, "/start?") && attached >= 0 && started < 0 {
			started = i
		}
	}
	if attached < 0 || started < attached {
		t.Errorf("attached at %d, started at %d: %v", attached, started, r.engine.Calls())
	}

	// a dump that fails saves nothing, and says why
	answers(fakedocker.ExecResult{Stdout: "PGDMP", Stderr: "pg_dump: error: connection to server on socket failed\nFATAL: role \"postgres\" does not exist\n", Code: 1})
	result, err = r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
	if err == nil || err.Error() != `the dump failed with exit code 1, and restic saved no snapshot: pg_dump: error: connection to server on socket failed FATAL: role "postgres" does not exist` ||
		result["refused"] != true || result["snapshot_id"] != nil {
		t.Errorf("a dump that failed: %v %v", result, err)
	}
	// a command the container does not have
	answers(fakedocker.ExecResult{Stdout: "OCI runtime exec failed: exec failed: unable to start container process: exec: \"sqlite3\": executable file not found in $PATH\r\n", Code: 127})
	if _, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{}); err == nil ||
		!strings.Contains(err.Error(), `exit code 127, and restic saved no snapshot: OCI runtime exec failed: exec failed: unable to start container process: exec: "sqlite3": executable file not found in $PATH`) {
		t.Errorf("a missing command: %v", err)
	}
	if snapshots := fakerestic.Snapshots(t, r.root, repo.GetUrl()); len(snapshots) != 1 {
		t.Errorf("%d snapshots", len(snapshots))
	}

	// a database that is not running is said before any job
	jobs := r.jobCount()
	r.engine.SetStatus(db, "exited")
	if _, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{}); err == nil || err.Error() != "the container umami-db-1 is not running, and the dump runs in it" {
		t.Errorf("a stopped database: %v", err)
	}
	run.StdinContainer = strings.Repeat("f", 64)
	if _, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{}); err == nil || !strings.Contains(err.Error(), "does not exist on this server") {
		t.Errorf("a database that is gone: %v", err)
	}
	if r.jobCount() != jobs {
		t.Error("a job ran for a database that is not running")
	}
}

// TestABackupRunThatCouldNotReadEverythingFails (#29): the run fails with
// restic's words, its result says the snapshot; a retention that fails
// leaves a run that saved its snapshot a success, and says why.
func TestABackupRunThatCouldNotReadEverythingFails(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	r.volume(t, "data", map[string]string{"open": "1", "locked": "2"})
	run := &agentv1.BackupRun{Repository: repo, Volumes: []string{"data"}, Tags: []string{"pocketcloud", "job:j4"}, HostName: "nas", Retention: &agentv1.BackupRetention{Last: 1}}

	t.Setenv("FAKE_RESTIC_UNREADABLE", "locked")
	result, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
	id, _ := result["snapshot_id"].(string)
	if err == nil || len(id) != 64 || result["refused"] != true ||
		err.Error() != "restic saved the snapshot "+id[:8]+", but could not read everything: open /volumes/data/locked: permission denied Warning: at least one source file could not be read" {
		t.Errorf("a partial backup: %v %v", result, err)
	}

	t.Setenv("FAKE_RESTIC_UNREADABLE", "")
	t.Setenv("FAKE_RESTIC_FORGET_FAIL", "unable to create lock in backend: repository is already locked by PID 9 on other-server")
	result, err = r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
	if err != nil || result["snapshot_id"] == nil || result["retention_error"] != "Fatal: unable to create lock in backend: repository is already locked by PID 9 on other-server" {
		t.Errorf("a failed retention: %v %v", result, err)
	}
}

// TestBackupRunsHaveSlotsOfTheirOwn (#29): two run at a time on a server,
// whatever else of the backups runs.
func TestBackupRunsHaveSlotsOfTheirOwn(t *testing.T) {
	x := newExecutor(nil, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	for range 2 {
		release, err := x.acquire(ctx, "backup.run")
		if err != nil {
			t.Fatalf("a run waited: %v", err)
		}
		defer release()
	}
	if _, err := x.acquire(ctx, "backup.run"); err == nil {
		t.Error("a third run ran beside two")
	}
	if release, err := x.acquire(context.Background(), "backup.other"); err != nil {
		t.Errorf("another backup command waited behind runs: %v", err)
	} else {
		release()
	}
	if perTypeLimits["backup.run"] != 2 {
		t.Errorf("limits: %v", perTypeLimits)
	}
}
