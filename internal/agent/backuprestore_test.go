package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

// The restore handlers (#30), on the fake engine and the fake restic: the
// server's folders live under the agent's HostRoot, where the fake job
// container reads and writes them too.

const testJobTag = "job:aaaaaaaaaaaaaaa"

// restoreHarness is an agent that restores, the engine it runs on, and what
// happened on the engine, in order.
type restoreHarness struct {
	*Agent
	engine *fakedocker.Engine
	root   string
	host   string
	repo   *agentv1.BackupRepository
	mu     sync.Mutex
	events []string
	jobs   []fakedocker.Job
	// during, when set, runs as a restore's job starts, with the job
	during func(job fakedocker.Job)
}

func newRestoreHarness(t *testing.T) *restoreHarness {
	t.Helper()
	root := fakerestic.Install(t)
	engine := fakedocker.New(t)
	engine.AddImage(agentImage, nil)
	engine.AddImage("sha256:"+strings.Repeat("a", 64), nil)
	engine.AddContainer("pocketcloud-agent", agentImage, map[string]string{"pocketcloud.role": "agent"})
	engine.VolumeRoot = t.TempDir()
	host := t.TempDir()
	fakerestic.RunJobsWith(engine, BackupTool, fakerestic.Options{ServerRoot: host})
	client, err := dockerapi.New(engine.Host())
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	store, err := openCommandStore(state)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	a := &Agent{Docker: client, Log: log, store: store, Executor: newExecutor(store, log),
		Config: Config{ContainerName: "pocketcloud-agent", StateDir: state, HostRoot: host, JobImageRoot: jobImage(t)}}
	h := &restoreHarness{Agent: a, engine: engine, root: root, host: host}
	run := engine.OnJob
	engine.OnJob = func(job fakedocker.Job) fakedocker.JobResult {
		h.mu.Lock()
		h.jobs = append(h.jobs, job)
		during := h.during
		h.mu.Unlock()
		restoring := len(job.Cmd) > 1 && job.Cmd[1] == "restore"
		if restoring {
			h.note("job:" + job.Cmd[1])
			if during != nil {
				during(job)
			}
		}
		result := run(job)
		if restoring {
			h.note("job:end")
		}
		return result
	}
	engine.OnStop = func(c *fakedocker.Container) {
		if c.Labels["pocketcloud.role"] == "" {
			h.note("stop:" + c.Name)
		}
	}
	engine.OnStart = func(c *fakedocker.Container) (string, string) {
		h.note("start:" + c.Name)
		return "running", c.Health
	}
	h.repo = &agentv1.BackupRepository{TargetId: "t1", Url: "s3:http://minio:9000/backups", Password: "repo-password-1",
		Env: map[string]string{"AWS_ACCESS_KEY_ID": "minio-key", "AWS_SECRET_ACCESS_KEY": "minio-secret"}}
	if _, err := h.handleBackupInit(context.Background(), initCommand(h.repo), &Reporter{}); err != nil {
		t.Fatal(err)
	}
	quick(t)
	return h
}

// quick shortens what a restore waits for.
func quick(t *testing.T) {
	t.Helper()
	verify, forward, every := restoreVerifyWait, restoreProgressForward, restoreProgressEvery
	restoreVerifyWait, restoreProgressForward, restoreProgressEvery = 20*time.Millisecond, 0, 0
	t.Cleanup(func() { restoreVerifyWait, restoreProgressForward, restoreProgressEvery = verify, forward, every })
}

// note adds to what happened; OnStop and OnStart note with the engine
// locked, and nothing here locks the engine.
func (h *restoreHarness) note(event string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event)
}

// happened is what happened so far.
func (h *restoreHarness) happened() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.events)
}

// restoreJobs are the restore jobs that ran.
func (h *restoreHarness) restoreJobs() []fakedocker.Job {
	h.mu.Lock()
	defer h.mu.Unlock()
	var jobs []fakedocker.Job
	for _, job := range h.jobs {
		if len(job.Cmd) > 1 && job.Cmd[1] == "restore" {
			jobs = append(jobs, job)
		}
	}
	return jobs
}

// volume makes a volume with files.
func (h *restoreHarness) volume(t *testing.T, name string, files map[string]string) {
	t.Helper()
	h.engine.AddVolume(name)
	writeFiles(t, h.engine.VolumeDir(name), files)
}

// folder makes a folder of the server with files.
func (h *restoreHarness) folder(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(h.host, dir), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, filepath.Join(h.host, dir), files)
}

// backup snapshots volumes as the job testJobTag, and says the snapshot.
func (h *restoreHarness) backup(t *testing.T, volumes ...string) string {
	t.Helper()
	result, err := h.handleBackupRun(context.Background(), runCommand(&agentv1.BackupRun{
		Repository: h.repo, Volumes: volumes, Tags: []string{"pocketcloud", testJobTag}, HostName: "nas",
	}), &Reporter{})
	if err != nil {
		t.Fatalf("the backup: %v %v", err, result)
	}
	return result["snapshot_id"].(string)
}

// container adds a running container with labels, mounts and a restart
// policy.
func (h *restoreHarness) container(name string, labels map[string]string, policy string, retries int, mounts ...map[string]any) string {
	id := h.engine.AddContainer(name, "app:1", labels)
	list := []any{}
	for _, mount := range mounts {
		list = append(list, mount)
	}
	h.engine.SetHostConfig(name, map[string]any{"NetworkMode": "bridge", "Mounts": list,
		"RestartPolicy": map[string]any{"Name": policy, "MaximumRetryCount": float64(retries)}})
	return id
}

func volumeMount(name, target string, readOnly bool) map[string]any {
	return map[string]any{"Type": "volume", "Source": name, "Target": target, "ReadOnly": readOnly}
}

func bindMount(source, target string, readOnly bool) map[string]any {
	return map[string]any{"Type": "bind", "Source": source, "Target": target, "ReadOnly": readOnly}
}

// payload is a real restore of snapshot to the original location of its
// volumes, with overwrite confirmed.
func (h *restoreHarness) payload(snapshot string, volumes ...string) *agentv1.BackupRestore {
	payload := &agentv1.BackupRestore{Repository: h.repo, SnapshotId: snapshot, RestoreId: "r1", JobTag: testJobTag, Overwrite: true, Replace: true}
	for _, volume := range volumes {
		payload.Original = append(payload.Original, &agentv1.BackupRestoreRoot{Source: "/volumes/" + volume, Volume: volume})
	}
	return payload
}

func restoreCommand(id string, payload *agentv1.BackupRestore) *agentv1.Command {
	return &agentv1.Command{Id: id, Type: "backup.restore", Payload: &agentv1.Command_BackupRestore{BackupRestore: payload}}
}

// restore runs a restore command.
func (h *restoreHarness) restore(ctx context.Context, id string, payload *agentv1.BackupRestore) (map[string]any, error) {
	return h.handleBackupRestore(ctx, restoreCommand(id, payload), &Reporter{})
}

// policy is a container's restart policy now.
func (h *restoreHarness) policy(t *testing.T, name string) restartPolicy {
	t.Helper()
	info, err := h.Docker.Inspect(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return policyOf(info)
}

// status is a container's status now.
func (h *restoreHarness) status(name string) string {
	if container := h.engine.Find(name); container != nil {
		return container.Status
	}
	return "gone"
}

// TestARestoreMountsOnlyTheDestinationWritable (#30, S/SAFE): a real restore's
// job mounts its destination at /restore/0 read-write and nothing else of
// the server, keeps no cache, may do no more than write files of any owner
// and give them their owner and mode back, and keeps a limited log.
func TestARestoreMountsOnlyTheDestinationWritable(t *testing.T) {
	h := newRestoreHarness(t)
	h.volume(t, "app_data", map[string]string{"a.txt": "v1"})
	snapshot := h.backup(t, "app_data")
	if _, err := h.restore(context.Background(), "c1", h.payload(snapshot, "app_data")); err != nil {
		t.Fatal(err)
	}
	jobs := h.restoreJobs()
	if len(jobs) != 1 {
		t.Fatalf("%d restore jobs", len(jobs))
	}
	job := jobs[0]
	mounts := mountsOf(job)
	if len(mounts) != 1 || mounts["/restore/0"]["Type"] != "volume" || mounts["/restore/0"]["Source"] != "app_data" || mounts["/restore/0"]["ReadOnly"] != nil {
		t.Errorf("mounts: %v", mounts)
	}
	if !slices.Equal(anyStrings(job.Host["CapAdd"]), []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID"}) ||
		!slices.Equal(anyStrings(job.Host["CapDrop"]), []string{"ALL"}) || !slices.Equal(anyStrings(job.Host["SecurityOpt"]), []string{"no-new-privileges"}) ||
		job.Host["NetworkMode"] != "bridge" || job.Stdin == nil {
		t.Errorf("host config: %v", job.Host)
	}
	if slices.Contains(job.Cmd, "--cache") || !slices.Contains(job.Cmd, "--wait") || !slices.Contains(job.Cmd, "--overwrite") || !slices.Contains(job.Cmd, "--replace") ||
		!strings.Contains(strings.Join(job.Cmd, " "), "--root /volumes/app_data=/restore/0 --dest app_data") {
		t.Errorf("the job's command: %q", job.Cmd)
	}
	logConfig, _ := job.Host["LogConfig"].(map[string]any)
	options, _ := logConfig["Config"].(map[string]any)
	if logConfig["Type"] != "json-file" || options["max-size"] != "10m" || job.Labels["pocketcloud.command"] != "c1" || job.Labels["pocketcloud.role"] != "job" {
		t.Errorf("log %v, labels %v", job.Host["LogConfig"], job.Labels)
	}
	if !strings.Contains(fakerestic.Calls(t, h.root), "restic --no-cache --retry-lock 5m restore "+snapshot+":/volumes/app_data") {
		t.Errorf("restic ran:\n%s", fakerestic.Calls(t, h.root))
	}

	// a new folder is made by the Engine as it mounts it
	payload := &agentv1.BackupRestore{Repository: h.repo, SnapshotId: snapshot, RestoreId: "r2", JobTag: testJobTag, TargetPath: "/srv/restored/app", Source: "/volumes/app_data"}
	if _, err := h.restore(context.Background(), "c2", payload); err != nil {
		t.Fatal(err)
	}
	mount := mountsOf(h.restoreJobs()[1])["/restore/0"]
	options, _ = mount["BindOptions"].(map[string]any)
	if mount["Type"] != "bind" || mount["Source"] != "/srv/restored/app" || mount["ReadOnly"] != nil || options["CreateMountpoint"] != true {
		t.Errorf("a new folder: %v", mount)
	}
	if content, err := os.ReadFile(filepath.Join(h.host, "srv/restored/app/a.txt")); err != nil || string(content) != "v1" {
		t.Errorf("the new folder: %q %v", content, err)
	}

	// a local target's directory stays writable for restic's lock, with no
	// capability more than the four
	disk := filepath.Join(t.TempDir(), "restic")
	local := &agentv1.BackupRepository{TargetId: "t2", Url: "/repo", Password: "repo-password-2", Mount: disk}
	if _, err := h.handleBackupInit(context.Background(), initCommand(local), &Reporter{}); err != nil {
		t.Fatal(err)
	}
	h.repo = local
	snapshot = h.backup(t, "app_data")
	if _, err := h.restore(context.Background(), "c3", h.payload(snapshot, "app_data")); err != nil {
		t.Fatal(err)
	}
	job = h.restoreJobs()[2]
	mounts = mountsOf(job)
	if len(mounts) != 2 || mounts["/repo"]["ReadOnly"] != nil || mounts["/repo"]["Source"] != disk || job.Host["NetworkMode"] != "none" ||
		!slices.Equal(anyStrings(job.Host["CapAdd"]), []string{"DAC_OVERRIDE", "FOWNER", "CHOWN", "FSETID"}) {
		t.Errorf("a local target's restore: %v %v", mounts, job.Host["CapAdd"])
	}
}

// TestADryRunMountsReadOnlyAndCreatesNothing (#30, S): a preview mounts its
// destinations read-only and a folder that is not there not at all, so that
// nothing is made; it says what would stop, whether the folder is there,
// the room it has, and what the snapshot holds that the job no longer backs
// up.
func TestADryRunMountsReadOnlyAndCreatesNothing(t *testing.T) {
	h := newRestoreHarness(t)
	h.volume(t, "app_data", map[string]string{"a.txt": "v1"})
	h.volume(t, "app_cache", map[string]string{"c": "c"})
	snapshot := h.backup(t, "app_data", "app_cache")
	writeFiles(t, h.engine.VolumeDir("app_data"), map[string]string{"a.txt": "v2", "extra": "x"})
	h.container("app-1", map[string]string{composeProjectLabel: "app"}, "always", 0, volumeMount("app_data", "/data", false))
	payload := h.payload(snapshot, "app_data")
	payload.DryRun, payload.Overwrite, payload.Stack = true, false, "app"
	result, err := h.restore(context.Background(), "p1", payload)
	if err != nil {
		t.Fatal(err)
	}
	job := h.restoreJobs()[0]
	if mount := mountsOf(job)["/restore/0"]; mount["ReadOnly"] != true || mount["Source"] != "app_data" ||
		!slices.Equal(anyStrings(job.Host["CapAdd"]), []string{"DAC_OVERRIDE"}) || !slices.Contains(job.Cmd, "--dry-run") || job.Stdin != nil {
		t.Errorf("the preview's job: %v %v %q", mountsOf(job), job.Host["CapAdd"], job.Cmd)
	}
	if content, _ := os.ReadFile(filepath.Join(h.engine.VolumeDir("app_data"), "a.txt")); string(content) != "v2" || h.status("app-1") != "running" {
		t.Error("the preview changed something")
	}
	stops, _ := result["would_stop"].([]any)
	roots, _ := result["roots"].([]any)
	root, _ := roots[0].(map[string]any)
	notInJob, _ := result["not_in_job"].([]any)
	counts, _ := result["counts"].(map[string]any)
	if len(stops) != 1 || stops[0].(map[string]any)["name"] != "app-1" || stops[0].(map[string]any)["project"] != "app" ||
		root["exists"] != true || root["free_bytes"] == nil || root["entries"] != float64(2) ||
		len(notInJob) != 1 || notInJob[0] != "/volumes/app_cache" || counts["updated"] != float64(1) || counts["deleted"] != float64(1) || result["dry_run"] != true {
		t.Errorf("the preview: %v", result)
	}

	// a new folder that is not there is not mounted, nor made
	payload = &agentv1.BackupRestore{Repository: h.repo, SnapshotId: snapshot, RestoreId: "r2", JobTag: testJobTag, TargetPath: "/srv/new/app",
		Source: "/volumes/app_data", DryRun: true}
	result, err = h.restore(context.Background(), "p2", payload)
	if err != nil {
		t.Fatal(err)
	}
	if mounts := mountsOf(h.restoreJobs()[1]); len(mounts) != 1 || mounts["/cache"] == nil {
		t.Errorf("the preview of a new folder mounts %v", mounts)
	}
	if _, err := os.Stat(filepath.Join(h.host, "srv")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the preview made the folder: %v", err)
	}
	roots, _ = result["roots"].([]any)
	root, _ = roots[0].(map[string]any)
	counts, _ = result["counts"].(map[string]any)
	if root["exists"] != false || root["free_bytes"] == nil || root["entries"] != float64(0) || counts["new"] != float64(1) || len(result["not_in_job"].([]any)) != 0 {
		t.Errorf("the preview of a new folder: %v", result)
	}
}

// TestRestoreStopsTheProjectAndTheVolumesUsersAndStartsThemAgain (#30, AC1):
// a restore stops the stack's containers and the other users of its volumes
// before its job writes, dependents first, restores each volume back to the
// snapshot, and starts exactly those again afterwards, in the reverse
// order; a container that was not running stays as it was.
func TestRestoreStopsTheProjectAndTheVolumesUsersAndStartsThemAgain(t *testing.T) {
	h := newRestoreHarness(t)
	h.volume(t, "notes_pgdata", map[string]string{"PG_VERSION": "16", "base/1": "rows-v1"})
	h.volume(t, "notes_appdata", map[string]string{"a.txt": "v1", "c.txt": "c"})
	snapshot := h.backup(t, "notes_pgdata", "notes_appdata")
	writeFiles(t, h.engine.VolumeDir("notes_pgdata"), map[string]string{"base/1": "rows-v2"})
	writeFiles(t, h.engine.VolumeDir("notes_appdata"), map[string]string{"a.txt": "v2", "b.txt": "added"})
	if err := os.Remove(filepath.Join(h.engine.VolumeDir("notes_appdata"), "c.txt")); err != nil {
		t.Fatal(err)
	}
	stack := map[string]string{composeProjectLabel: "notes"}
	h.container("notes-db-1", map[string]string{composeProjectLabel: "notes", composeServiceLabel: "db"}, "unless-stopped", 0, volumeMount("notes_pgdata", "/var/lib/postgresql/data", false))
	h.container("notes-app-1", map[string]string{composeProjectLabel: "notes", composeServiceLabel: "app", composeDependsLabel: "db:service_started:false"},
		"on-failure", 5, volumeMount("notes_appdata", "/data", false))
	h.container("notes-worker-1", stack, "always", 0)
	h.container("reader", nil, "always", 0, volumeMount("notes_appdata", "/in", true))
	h.container("unrelated", nil, "always", 0, volumeMount("other", "/x", false))
	stopped := h.container("notes-old-1", stack, "no", 0)
	h.engine.SetStatus(stopped, "exited")

	payload := h.payload(snapshot, "notes_pgdata", "notes_appdata")
	payload.Stack = "notes"
	payload.ConfirmedStops = []string{"notes-app-1", "notes-db-1", "notes-worker-1", "reader"}
	result, err := h.restore(context.Background(), "c1", payload)
	if err != nil {
		t.Fatalf("%v %v", err, result)
	}
	if files := readTree(t, h.engine.VolumeDir("notes_appdata")); len(files) != 2 || files["a.txt"] != "v1" || files["c.txt"] != "c" {
		t.Errorf("notes_appdata: %v", files)
	}
	if files := readTree(t, h.engine.VolumeDir("notes_pgdata")); files["base/1"] != "rows-v1" {
		t.Errorf("notes_pgdata: %v", files)
	}
	// the job starts and checks; the stops come at its ready mark, before it
	// restores, and the starts once it ended
	events := h.happened()
	job := slices.Index(events, "job:restore")
	end := slices.Index(events, "job:end")
	if job < 0 || end < job {
		t.Fatalf("events: %q", events)
	}
	stops, starts := []string{}, []string{}
	for i, event := range events {
		if name, ok := strings.CutPrefix(event, "stop:"); ok && i > job && i < end {
			stops = append(stops, name)
		}
		if name, ok := strings.CutPrefix(event, "start:"); ok && i > end {
			starts = append(starts, name)
		}
	}
	if slices.ContainsFunc(events[end:], func(event string) bool { return strings.HasPrefix(event, "stop:") }) {
		t.Errorf("a stop after the job: %q", events)
	}
	if !slices.Equal(stops, []string{"notes-app-1", "notes-db-1", "notes-worker-1", "reader"}) ||
		!slices.Equal(starts, []string{"reader", "notes-worker-1", "notes-db-1", "notes-app-1"}) {
		t.Errorf("stopped %q, then started %q: %q", stops, starts, events)
	}
	for _, name := range []string{"notes-db-1", "notes-app-1", "notes-worker-1", "reader", "unrelated"} {
		if h.status(name) != "running" {
			t.Errorf("%s is %s", name, h.status(name))
		}
	}
	if h.status("notes-old-1") != "exited" || h.policy(t, "notes-app-1") != (restartPolicy{Name: "on-failure", MaxRetry: 5}) ||
		h.policy(t, "notes-db-1").Name != "unless-stopped" {
		t.Errorf("after: %s %v", h.status("notes-old-1"), h.policy(t, "notes-app-1"))
	}
	appStopped, _ := result["app_stopped"].([]any)
	appStarted, _ := result["app_started"].([]any)
	if len(appStopped) != 4 || appStopped[0].(map[string]any)["name"] != "notes-app-1" || len(appStarted) != 4 ||
		len(result["app_not_running"].([]any)) != 0 || len(result["start_errors"].([]any)) != 0 || result["partial"] != false ||
		result["files_deleted"] != float64(1) || result["dry_run"] != false || result["refused"] != "" ||
		!strings.Contains(result["log"].(string), "Started 4 containers again.") {
		t.Errorf("the result: %v", result)
	}
	if _, err := os.Stat(h.markerPath("c1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the marker is left: %v", err)
	}
}

// TestAReadOnlyParentBindKeepsRunning (#30): a container that mounts a
// folder above the destination read-only, such as a node exporter that
// reads /, runs on; one that writes a folder above it, or in it, stops.
func TestAReadOnlyParentBindKeepsRunning(t *testing.T) {
	h := newRestoreHarness(t)
	h.volume(t, "app_data", map[string]string{"a.txt": "v1"})
	snapshot := h.backup(t, "app_data")
	h.folder(t, "/srv/app", map[string]string{"old": "x"})
	h.container("node-exporter", nil, "always", 0, bindMount("/", "/host", true))
	h.container("writer-above", nil, "always", 0, bindMount("/srv", "/srv", false))
	h.container("writer-in", nil, "always", 0, bindMount("/srv/app/uploads", "/uploads", false))
	h.container("reader-in", nil, "always", 0, bindMount("/srv/app", "/app", true))
	h.container("next-door", nil, "always", 0, bindMount("/srv/app2", "/app", false))
	payload := &agentv1.BackupRestore{Repository: h.repo, SnapshotId: snapshot, RestoreId: "r1", JobTag: testJobTag, TargetPath: "/srv/app",
		Source: "/volumes/app_data", Overwrite: true, ConfirmedStops: []string{"writer-above", "writer-in"}}
	result, err := h.restore(context.Background(), "c1", payload)
	if err != nil {
		t.Fatalf("%v %v", err, result)
	}
	stopped := []string{}
	for _, event := range h.happened() {
		if name, ok := strings.CutPrefix(event, "stop:"); ok {
			stopped = append(stopped, name)
		}
	}
	slices.Sort(stopped)
	if !slices.Equal(stopped, []string{"writer-above", "writer-in"}) {
		t.Errorf("stopped %q", stopped)
	}
	if files := readTree(t, filepath.Join(h.host, "srv/app")); files["a.txt"] != "v1" || files["old"] != "x" {
		t.Errorf("the folder: %v", files)
	}
}

// TestContainersStartAgainWhenResticFails (#30, AC1/SAFE): a restore that
// fails once it stopped the application starts it again, with its restart
// policies, and says what it started; one whose files restic could not all
// write is partial.
func TestContainersStartAgainWhenResticFails(t *testing.T) {
	h := newRestoreHarness(t)
	h.volume(t, "app_data", map[string]string{"a.txt": "v1", "b.txt": "b"})
	snapshot := h.backup(t, "app_data")
	h.container("app-1", nil, "always", 0, volumeMount("app_data", "/data", false))
	payload := h.payload(snapshot, "app_data")
	payload.ConfirmedStops = []string{"app-1"}
	// a lock that stays, once the application is stopped
	t.Setenv("FAKE_RESTIC_LOCKED_RESTORE", "9")
	result, err := h.restore(context.Background(), "c1", payload)
	if err == nil || !strings.Contains(err.Error(), "already locked") {
		t.Fatalf("the restore: %v", err)
	}
	if h.status("app-1") != "running" || h.policy(t, "app-1").Name != "always" || result["partial"] != true ||
		len(result["app_started"].([]any)) != 1 || len(result["app_stopped"].([]any)) != 1 {
		t.Errorf("after a failure: %s %v %v", h.status("app-1"), h.policy(t, "app-1"), result)
	}
	if _, err := os.Stat(h.markerPath("c1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the marker is left: %v", err)
	}

	// restic restores what it can, and says what it could not
	t.Setenv("FAKE_RESTIC_LOCKED_RESTORE", "")
	t.Setenv("FAKE_RESTIC_RESTORE_FAIL_AFTER", "1")
	writeFiles(t, h.engine.VolumeDir("app_data"), map[string]string{"a.txt": "v2", "b.txt": "b2"})
	result, err = h.restore(context.Background(), "c2", payload)
	if err != nil {
		t.Fatal(err)
	}
	samples, _ := result["error_sample"].([]any)
	if result["partial"] != true || result["errors"] != float64(1) || len(samples) != 1 || !strings.Contains(samples[0].(string), "app_data/b.txt: permission denied") ||
		h.status("app-1") != "running" {
		t.Errorf("a partial restore: %v", result)
	}
}

// slowRestore is a restore whose restic takes a while, and a channel that
// says when restic began restoring.
func slowRestore(t *testing.T, h *restoreHarness) (*agentv1.BackupRestore, chan struct{}) {
	t.Helper()
	files := map[string]string{}
	for i := range 10 {
		files["f"+itoa(i)] = "x"
	}
	h.volume(t, "app_data", files)
	snapshot := h.backup(t, "app_data")
	h.container("app-1", nil, "unless-stopped", 0, volumeMount("app_data", "/data", false))
	payload := h.payload(snapshot, "app_data")
	payload.ConfirmedStops = []string{"app-1"}
	t.Setenv("FAKE_RESTIC_SLOW_MS", "400")
	began := make(chan struct{})
	go func() {
		defer close(began)
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if strings.Contains(fakerestic.Calls(t, h.root), " restore "+snapshot) {
				return
			}
		}
	}()
	return payload, began
}

// TestContainersStartAgainWhenCancelledBeforeTheReport (#30, SAFE): a restore
// stopped part way stops its job, which stops restic, starts the
// application again, and says so with what it started.
func TestContainersStartAgainWhenCancelledBeforeTheReport(t *testing.T) {
	h := newRestoreHarness(t)
	payload, began := slowRestore(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var result map[string]any
	var err error
	go func() {
		defer close(done)
		result, err = h.restore(ctx, "c1", payload)
	}()
	<-began
	if h.status("app-1") != "exited" || h.policy(t, "app-1").Name != "no" {
		t.Errorf("while restoring: %s %v", h.status("app-1"), h.policy(t, "app-1"))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(40 * time.Second):
		t.Fatal("the restore did not stop")
	}
	if err == nil || err.Error() != "The restore was stopped before it was complete: the destination holds part of the snapshot. The application was started again." {
		t.Errorf("the restore: %v", err)
	}
	if h.status("app-1") != "running" || h.policy(t, "app-1").Name != "unless-stopped" || result["partial"] != true || len(result["app_started"].([]any)) != 1 {
		t.Errorf("after the stop: %s %v %v", h.status("app-1"), h.policy(t, "app-1"), result)
	}
	stoppedJob := false
	for _, call := range h.engine.Calls() {
		stoppedJob = stoppedJob || strings.HasSuffix(call, "/stop?t=30") && !strings.Contains(call, "app-1")
	}
	if !stoppedJob {
		t.Error("the job was not stopped with its grace")
	}

	// stopped before the go, nothing was stopped
	h2 := newRestoreHarness(t)
	h2.volume(t, "app_data", map[string]string{"a": "1"})
	snapshot := h2.backup(t, "app_data")
	h2.container("app-1", nil, "always", 0, volumeMount("app_data", "/data", false))
	ctx, cancel = context.WithCancel(context.Background())
	h2.during = func(fakedocker.Job) { cancel() }
	payload = h2.payload(snapshot, "app_data")
	payload.ConfirmedStops = []string{"app-1"}
	if _, err := h2.restore(ctx, "c2", payload); err == nil || err.Error() != "The restore was stopped before anything changed." {
		t.Errorf("stopped before the go: %v", err)
	}
	if slices.ContainsFunc(h2.happened(), func(event string) bool { return event == "stop:app-1" }) {
		t.Error("a restore stopped before the go stopped the application")
	}
}

// TestRestoreStopsBeforeItsDeadlineToStartTheApp (#30, SAFE): a restore's job
// ends before the command's deadline, keeping time to start the application
// again and to say so before the panel gives up on it.
func TestRestoreStopsBeforeItsDeadlineToStartTheApp(t *testing.T) {
	h := newRestoreHarness(t)
	payload, _ := slowRestore(t, h)
	deadline := time.Now().Add(3 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	result, err := h.restore(ctx, "c1", payload)
	if err == nil || err.Error() != "The restore ran out of time: the destination holds part of the snapshot. The application was started again." {
		t.Errorf("the restore: %v", err)
	}
	if !time.Now().Before(deadline) {
		t.Error("the restore ended after its deadline")
	}
	if h.status("app-1") != "running" || len(result["app_started"].([]any)) != 1 {
		t.Errorf("after the deadline: %s %v", h.status("app-1"), result)
	}
}

// TestRestartPolicyIsNoWhileStoppedAndRestoredAfter (#30, SAFE): while its
// job runs, every container a restore stopped has the restart policy no,
// which neither the Engine nor a reboot overrides, and gets its own back,
// retries included, afterwards; the marker names each with its policy.
func TestRestartPolicyIsNoWhileStoppedAndRestoredAfter(t *testing.T) {
	h := newRestoreHarness(t)
	h.volume(t, "app_data", map[string]string{"a": "1"})
	snapshot := h.backup(t, "app_data")
	h.container("app-1", nil, "on-failure", 3, volumeMount("app_data", "/data", false))
	h.container("app-2", nil, "always", 0, volumeMount("app_data", "/data", false))
	var during []restartPolicy
	var marker restoreMarker
	var looked sync.WaitGroup
	h.during = func(fakedocker.Job) {
		// the job checks first: the stops come at its ready mark
		looked.Go(func() {
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
				if h.status("app-1") == "exited" && h.status("app-2") == "exited" {
					during = []restartPolicy{h.policy(t, "app-1"), h.policy(t, "app-2")}
					raw, _ := os.ReadFile(h.markerPath("c1"))
					_ = json.Unmarshal(raw, &marker)
					return
				}
			}
		})
	}
	t.Setenv("FAKE_RESTIC_SLOW_MS", "300")
	payload := h.payload(snapshot, "app_data")
	payload.ConfirmedStops = []string{"app-1", "app-2"}
	if _, err := h.restore(context.Background(), "c1", payload); err != nil {
		t.Fatal(err)
	}
	looked.Wait()
	if len(during) != 2 || during[0].Name != "no" || during[1].Name != "no" {
		t.Errorf("while stopped: %v", during)
	}
	if len(marker.Containers) != 2 || marker.CommandID != "c1" || marker.RestoreID != "r1" ||
		marker.Containers[0].RestartPolicy != (restartPolicy{Name: "on-failure", MaxRetry: 3}) || marker.Containers[1].RestartPolicy.Name != "always" {
		t.Errorf("the marker: %+v", marker)
	}
	if h.policy(t, "app-1") != (restartPolicy{Name: "on-failure", MaxRetry: 3}) || h.policy(t, "app-2").Name != "always" {
		t.Errorf("after: %v %v", h.policy(t, "app-1"), h.policy(t, "app-2"))
	}
}

// TestAContainerNotInThePreviewRefusesTheRestore (#30, AC3): a restore that
// would stop a container its preview did not show is refused before
// anything stops, also when the container came while the job checked.
func TestAContainerNotInThePreviewRefusesTheRestore(t *testing.T) {
	h := newRestoreHarness(t)
	h.volume(t, "app_data", map[string]string{"a.txt": "v1"})
	snapshot := h.backup(t, "app_data")
	writeFiles(t, h.engine.VolumeDir("app_data"), map[string]string{"a.txt": "v2"})
	h.container("app-1", nil, "always", 0, volumeMount("app_data", "/data", false))
	h.container("app-2", nil, "always", 0, volumeMount("app_data", "/data", false))
	payload := h.payload(snapshot, "app_data")
	payload.ConfirmedStops = []string{"app-1"}
	if _, err := h.restore(context.Background(), "c1", payload); err == nil || err.Error() != "app-2 started using the destination after the preview: preview again." {
		t.Errorf("the restore: %v", err)
	}
	if len(h.restoreJobs()) != 0 || h.status("app-1") != "running" {
		t.Error("the restore went on")
	}

	// one that comes while the job checks
	payload.ConfirmedStops = []string{"app-1", "app-2"}
	h.during = func(fakedocker.Job) {
		h.container("vaultwarden-app-2", nil, "always", 0, volumeMount("app_data", "/data", false))
	}
	result, err := h.restore(context.Background(), "c2", payload)
	if err == nil || err.Error() != "vaultwarden-app-2 started using the destination after the preview: preview again." {
		t.Errorf("the restore: %v", err)
	}
	if content, _ := os.ReadFile(filepath.Join(h.engine.VolumeDir("app_data"), "a.txt")); string(content) != "v2" {
		t.Errorf("the destination was written: %q", content)
	}
	if slices.ContainsFunc(h.happened(), func(event string) bool { return strings.HasPrefix(event, "stop:") }) || len(result["app_stopped"].([]any)) != 0 {
		t.Errorf("something stopped: %q", h.happened())
	}
}

// TestAFailingStopStartsTheOthersAndRestoresNothing (#30): when a container
// does not stop, those already stopped start again with their policies,
// the marker goes, and nothing is restored.
func TestAFailingStopStartsTheOthersAndRestoresNothing(t *testing.T) {
	h := newRestoreHarness(t)
	h.volume(t, "app_data", map[string]string{"a.txt": "v1"})
	snapshot := h.backup(t, "app_data")
	writeFiles(t, h.engine.VolumeDir("app_data"), map[string]string{"a.txt": "v2"})
	h.container("app-1", nil, "always", 0, volumeMount("app_data", "/data", false))
	stubborn := h.container("app-2", nil, "unless-stopped", 0, volumeMount("app_data", "/data", false))
	h.engine.Fail("POST /containers/"+stubborn+"/stop", "the container ignores SIGTERM")
	payload := h.payload(snapshot, "app_data")
	payload.ConfirmedStops = []string{"app-1", "app-2"}
	result, err := h.restore(context.Background(), "c1", payload)
	if err == nil || !strings.Contains(err.Error(), "app-2 did not stop") {
		t.Errorf("the restore: %v", err)
	}
	if content, _ := os.ReadFile(filepath.Join(h.engine.VolumeDir("app_data"), "a.txt")); string(content) != "v2" {
		t.Errorf("the destination was written: %q", content)
	}
	if h.status("app-1") != "running" || h.policy(t, "app-1").Name != "always" || h.policy(t, "app-2").Name != "unless-stopped" {
		t.Errorf("after: %s %v %v", h.status("app-1"), h.policy(t, "app-1"), h.policy(t, "app-2"))
	}
	if _, err := os.Stat(h.markerPath("c1")); !errors.Is(err, os.ErrNotExist) || result["partial"] != false {
		t.Errorf("the marker: %v, %v", err, result)
	}
}

// TestAnAgentRestartStartsTheAppAndSettlesTheCommand (#30, SAFE): what a
// restore stopped when the agent stopped is started again at its next
// start, with its policies back; the command is settled with what happened,
// which the panel's replay reads rather than running it again.
func TestAnAgentRestartStartsTheAppAndSettlesTheCommand(t *testing.T) {
	h := newRestoreHarness(t)
	h.volume(t, "app_data", map[string]string{"a": "1"})
	h.container("app-1", nil, "on-failure", 2, volumeMount("app_data", "/data", false))
	h.container("app-2", nil, "always", 0, volumeMount("app_data", "/data", false))
	if err := h.store.start("c1", "backup.restore"); err != nil {
		t.Fatal(err)
	}
	// the restore stopped them, and the agent went
	session := &restoreSession{a: h.Agent, marker: restoreMarker{CommandID: "c1", RestoreID: "r1"}}
	users, err := h.affected(context.Background(), "c1", "", []restoreDest{{volume: "app_data"}})
	if err != nil {
		t.Fatal(err)
	}
	session.users = users
	if err := session.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.status("app-1") != "exited" || h.policy(t, "app-1").Name != "no" {
		t.Fatalf("stopped: %s %v", h.status("app-1"), h.policy(t, "app-1"))
	}
	if err := os.WriteFile(filepath.Join(h.Config.StateDir, restoreMarkers, "c9.json.tmp"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	// the next agent, on the same volume
	store, err := openCommandStore(h.Config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	next := &Agent{Docker: h.Docker, Log: h.Log, store: store, Executor: newExecutor(store, h.Log), Config: h.Config}
	next.cleanupBackupJobs(context.Background())
	next.finishInterruptedRestores(context.Background())
	if h.status("app-1") != "running" || h.status("app-2") != "running" || h.policy(t, "app-1") != (restartPolicy{Name: "on-failure", MaxRetry: 2}) ||
		h.policy(t, "app-2").Name != "always" {
		t.Errorf("after the restart: %s %s %v %v", h.status("app-1"), h.status("app-2"), h.policy(t, "app-1"), h.policy(t, "app-2"))
	}
	entries, _ := os.ReadDir(filepath.Join(h.Config.StateDir, restoreMarkers))
	if len(entries) != 0 {
		t.Errorf("left in the markers' folder: %v", entries)
	}
	entry, _ := store.get("c1")
	message := "The agent restarted during the restore, so the destination may hold only part of the snapshot. It started the application again (2 of 2 containers)."
	if entry.State != stateFailed || entry.Error != message || entry.Result["interrupted"] != true || len(entry.Result["app_started"].([]any)) != 2 {
		t.Errorf("the command: %+v", entry)
	}

	// the panel sends it again: it is answered, never run
	panel := &fakePanel{}
	next.Executor.SetClient(panel)
	ran := false
	next.Executor.Handle("backup.restore", func(context.Context, *agentv1.Command, *Reporter) (map[string]any, error) {
		ran = true
		return nil, nil
	})
	next.Executor.Submit(restoreCommand("c1", h.payload(strings.Repeat("a", 64), "app_data")))
	final := panel.final(t, "c1")
	if ran || !final.GetDuplicate() || final.GetError() != message || final.GetState() != agentv1.CommandState_COMMAND_STATE_FAILED ||
		!final.GetResult().GetFields()["interrupted"].GetBoolValue() {
		t.Errorf("the replay: %v, ran %v", final, ran)
	}
}

// TestContainerStartIsRefusedDuringARestore (#30, SAFE): a container a
// restore holds stopped is not started or restarted from the panel, by its
// id, a part of it or its name, until the restore started it again.
func TestContainerStartIsRefusedDuringARestore(t *testing.T) {
	h := newRestoreHarness(t)
	h.volume(t, "app_data", map[string]string{"a": "1"})
	snapshot := h.backup(t, "app_data")
	id := h.container("app-1", nil, "always", 0, volumeMount("app_data", "/data", false))
	var refused []error
	var tried sync.WaitGroup
	h.during = func(fakedocker.Job) {
		tried.Go(func() {
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
				if h.status("app-1") != "exited" {
					continue
				}
				for _, ref := range []string{id, id[:12], "app-1", "/app-1"} {
					_, err := h.handleContainerStart(context.Background(), &agentv1.Command{Payload: &agentv1.Command_ContainerStart{ContainerStart: &agentv1.ContainerStart{DockerId: ref}}}, nil)
					refused = append(refused, err)
				}
				_, err := h.handleContainerRestart(context.Background(), &agentv1.Command{Payload: &agentv1.Command_ContainerRestart{ContainerRestart: &agentv1.ContainerRestart{DockerId: id}}}, nil)
				refused = append(refused, err)
				return
			}
		})
	}
	t.Setenv("FAKE_RESTIC_SLOW_MS", "300")
	payload := h.payload(snapshot, "app_data")
	payload.ConfirmedStops = []string{"app-1"}
	if _, err := h.restore(context.Background(), "c1", payload); err != nil {
		t.Fatal(err)
	}
	tried.Wait()
	if len(refused) != 5 {
		t.Fatalf("%d starts were tried", len(refused))
	}
	for _, err := range refused {
		if !errors.Is(err, errRestoring) {
			t.Errorf("a start during the restore: %v", err)
		}
	}
	// once the restore ends, it starts as before
	h.engine.SetStatus(id, "exited")
	if _, err := h.handleContainerStart(context.Background(), &agentv1.Command{Payload: &agentv1.Command_ContainerStart{ContainerStart: &agentv1.ContainerStart{DockerId: id}}}, nil); err != nil || h.status("app-1") != "running" {
		t.Errorf("after the restore: %v", err)
	}
}

// TestRestoreAndBackupOfTheSameDataExclude (#30, SAFE): a restore does not
// write what a backup reads, nor a backup read what a restore writes, on
// this server; folders count when one holds the other.
func TestRestoreAndBackupOfTheSameDataExclude(t *testing.T) {
	h := newRestoreHarness(t)
	h.volume(t, "app_data", map[string]string{"a": "1"})
	snapshot := h.backup(t, "app_data")
	release, err := h.claims.claim(false, []string{"volume:app_data"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.restore(context.Background(), "c1", h.payload(snapshot, "app_data")); err == nil || err.Error() != "A backup of app_data is running: restore when it ends." {
		t.Errorf("a restore beside a backup: %v", err)
	}
	// reads go together
	if _, err := h.handleBackupRun(context.Background(), runCommand(&agentv1.BackupRun{Repository: h.repo, Volumes: []string{"app_data"},
		Tags: []string{"pocketcloud", testJobTag}, HostName: "nas"}), &Reporter{}); err != nil {
		t.Errorf("two backups: %v", err)
	}
	release()
	release, err = h.claims.claim(true, []string{"path:/srv/app"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	h.folder(t, "/srv/app/www", map[string]string{"index.html": "hi"})
	_, err = h.handleBackupRun(context.Background(), runCommand(&agentv1.BackupRun{Repository: h.repo, Paths: []string{"/srv/app/www"},
		Tags: []string{"pocketcloud", testJobTag}, HostName: "nas"}), &Reporter{})
	if err == nil || err.Error() != "A restore into /srv/app is under way: back it up once it ends." {
		t.Errorf("a backup of what a restore writes: %v", err)
	}
	payload := &agentv1.BackupRestore{Repository: h.repo, SnapshotId: snapshot, RestoreId: "r2", JobTag: testJobTag, TargetPath: "/srv", Source: "/volumes/app_data"}
	if _, err := h.restore(context.Background(), "c2", payload); err == nil || !strings.Contains(err.Error(), "A restore into /srv/app is under way") {
		t.Errorf("a restore into what holds another's: %v", err)
	}
	// a preview takes no claim
	payload.DryRun = true
	if _, err := h.restore(context.Background(), "p3", payload); err != nil {
		t.Errorf("a preview beside a restore: %v", err)
	}
	if claimsOverlap("path:/srv/app", "path:/srv/app2") || claimsOverlap("volume:a", "path:a") || !claimsOverlap("volume:a", "volume:a") {
		t.Error("claimsOverlap")
	}
}

// TestARestoreChecksWhatItIsAsked (#30): the agent refuses a restore that
// does not name its record, its snapshot in full and its job's tag, that
// names both or neither of the original location and a new folder, too
// many roots or picks, a pick in no root, a replace with picks or without
// overwrite, or the original location without overwrite; before it looks
// at anything of the server.
func TestARestoreChecksWhatItIsAsked(t *testing.T) {
	h := newRestoreHarness(t)
	h.volume(t, "app_data", map[string]string{"a": "1"})
	good := func() *agentv1.BackupRestore { return h.payload(strings.Repeat("a", 64), "app_data") }
	for name, change := range map[string]func(*agentv1.BackupRestore){
		"no record":            func(p *agentv1.BackupRestore) { p.RestoreId = "" },
		"a short snapshot id":  func(p *agentv1.BackupRestore) { p.SnapshotId = "3f9a1c2e" },
		"no job tag":           func(p *agentv1.BackupRestore) { p.JobTag = "pocketcloud" },
		"a target volume":      func(p *agentv1.BackupRestore) { p.TargetVolume = "app_data" },
		"both":                 func(p *agentv1.BackupRestore) { p.TargetPath = "/srv/x" },
		"neither":              func(p *agentv1.BackupRestore) { p.Original = nil },
		"a pick in no root":    func(p *agentv1.BackupRestore) { p.Replace, p.Include = false, []string{"/volumes/other/x"} },
		"a relative pick":      func(p *agentv1.BackupRestore) { p.Replace, p.Include = false, []string{"volumes/app_data"} },
		"replace with a pick":  func(p *agentv1.BackupRestore) { p.Include = []string{"/volumes/app_data/x"} },
		"replace unconfirmed":  func(p *agentv1.BackupRestore) { p.Overwrite = false },
		"original unconfirmed": func(p *agentv1.BackupRestore) { p.Replace, p.Overwrite = false, false },
		"too many roots": func(p *agentv1.BackupRestore) {
			for range maxRestoreRoots {
				p.Original = append(p.Original, &agentv1.BackupRestoreRoot{Source: "/volumes/app_data", Volume: "app_data"})
			}
		},
		"too many picks": func(p *agentv1.BackupRestore) {
			p.Replace = false
			for range maxRestoreIncludes + 1 {
				p.Include = append(p.Include, "/volumes/app_data/x")
			}
		},
		"no repository password": func(p *agentv1.BackupRestore) {
			p.Repository = &agentv1.BackupRepository{TargetId: "t1", Url: h.repo.GetUrl()}
		},
	} {
		payload := good()
		change(payload)
		if _, err := h.restore(context.Background(), "c1", payload); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
	if len(h.restoreJobs()) != 0 {
		t.Error("a job ran")
	}
	// a preview needs no confirmation
	payload := good()
	payload.Overwrite, payload.DryRun = false, true
	if err := checkRestore(restoreCommand("p1", payload)); err != nil {
		t.Errorf("a preview: %v", err)
	}
}

// TestAMissingVolumeIsNotRestored (#30): a volume is restored only when it
// exists, and never restic's cache, a volume of PocketCloud's or the
// agent's own.
func TestAMissingVolumeIsNotRestored(t *testing.T) {
	h := newRestoreHarness(t)
	h.volume(t, "app_data", map[string]string{"a": "1"})
	snapshot := h.backup(t, "app_data")
	h.engine.AddVolume(resticCache)
	h.engine.AddVolume("pocketcloud-agent-state")
	agent := h.engine.Find("pocketcloud-agent")
	h.engine.SetHostConfig("pocketcloud-agent", map[string]any{"Mounts": []any{volumeMount("pocketcloud-agent-state", "/var/lib/pocketcloud", false)}})
	if agent == nil {
		t.Fatal("no agent container")
	}
	h.Config.ContainerName = "pocketcloud-agent"
	for volume, want := range map[string]string{
		"gone_data":               "the volume gone_data does not exist on this server",
		resticCache:               "is restic's cache",
		"pocketcloud-agent-state": "is the agent's own volume",
		"../etc":                  "is not a volume's name",
	} {
		_, err := h.restore(context.Background(), "c1", h.payload(snapshot, volume))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", volume, err)
		}
	}
	if len(h.restoreJobs()) != 0 {
		t.Error("a job ran")
	}
}

// TestRestoreProgressReachesThePanel (#30, S): a restore reports its steps
// as it goes, the bytes restored of all, and a step's progress at most
// every so often.
func TestRestoreProgressReachesThePanel(t *testing.T) {
	h := newRestoreHarness(t)
	files := map[string]string{}
	for i := range 8 {
		files["f"+itoa(i)] = strings.Repeat("x", 100)
	}
	h.volume(t, "app_data", files)
	snapshot := h.backup(t, "app_data")
	h.container("app-1", nil, "always", 0, volumeMount("app_data", "/data", false))
	t.Setenv("FAKE_RESTIC_SLOW_MS", "60")
	restoreProgressForward = time.Hour
	panel := &fakePanel{}
	payload := h.payload(snapshot, "app_data")
	payload.ConfirmedStops = []string{"app-1"}
	command := restoreCommand("c1", payload)
	reporter := reporterFor(t, panel, "c1")
	if _, err := h.handleBackupRestore(context.Background(), command, reporter); err != nil {
		t.Fatal(err)
	}
	var steps []string
	var restore *agentv1.CommandProgress
	for _, progress := range panel.progress("c1") {
		steps = append(steps, progress.GetStep())
		if progress.GetStep() == "restore" {
			restore = progress
		}
	}
	if !slices.Equal(steps, []string{"check", "stop", "restore", "start", "verify"}) {
		t.Errorf("steps: %q", steps)
	}
	if restore == nil || restore.GetPercent() != 10 {
		t.Errorf("the restore step: %v", restore)
	}

	// each restic status line reaches the panel when nothing holds it back,
	// with the bytes restic writes of all it restores
	restoreProgressForward = 0
	panel = &fakePanel{}
	payload.Replace = false
	for name := range files {
		files[name] = strings.Repeat("y", 100)
	}
	writeFiles(t, h.engine.VolumeDir("app_data"), files)
	if _, err := h.handleBackupRestore(context.Background(), restoreCommand("c2", payload), reporterFor(t, panel, "c2")); err != nil {
		t.Fatal(err)
	}
	var counted *agentv1.CommandProgress
	for _, progress := range panel.progress("c2") {
		if progress.GetStep() == "restore" && progress.GetTotal() > 0 {
			counted = progress
		}
	}
	if counted == nil || counted.GetUnit() != "bytes" || counted.GetTotal() != 800 || counted.GetDone() <= 0 || counted.GetPercent() <= 10 || counted.GetPercent() > 95 {
		t.Errorf("the counted progress: %v", counted)
	}
}

// TestRestoreFolderResolvesLinksAndAMissingTail (#30, SAFE): a folder is
// followed through its links on the server, an absolute one from the
// server's /, and checked where it leads; the part that is not there yet is
// made as named; a file in the way is refused, and so is a link that leads
// into the system's folders.
func TestRestoreFolderResolvesLinksAndAMissingTail(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"data/app", "etc", "srv"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/data", filepath.Join(root, "srv/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../etc", filepath.Join(root, "srv/evil")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "srv/file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop", filepath.Join(root, "srv/loop")); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]struct {
		real   string
		exists bool
	}{
		"/srv/link/app":         {"/data/app", true},
		"/srv/link/new/sub":     {"/data/new/sub", false},
		"/srv/restored":         {"/srv/restored", false},
		"/srv/evil/pocketcloud": {"/etc/pocketcloud", false},
	} {
		real, exists, err := resolveServerTarget(root, dir)
		if err != nil || real != want.real || exists != want.exists {
			t.Errorf("%s: %s %v %v", dir, real, exists, err)
		}
	}
	if _, _, err := resolveServerTarget(root, "/srv/file/x"); err == nil || !strings.Contains(err.Error(), "/srv/file is a file on this server") {
		t.Errorf("a file in the way: %v", err)
	}
	if _, _, err := resolveServerTarget(root, "/srv/loop"); err == nil || !strings.Contains(err.Error(), "more than 40 symbolic links") {
		t.Errorf("a loop: %v", err)
	}

	h := newRestoreHarness(t)
	h.volume(t, "app_data", map[string]string{"a": "1"})
	snapshot := h.backup(t, "app_data")
	h.folder(t, "/etc", nil)
	h.folder(t, "/srv", nil)
	if err := os.Symlink("/etc", filepath.Join(h.host, "srv/evil")); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{
		"/srv/evil": "/srv/evil leads to /etc on this server: /etc is the system's",
		"/etc/x":    "/etc is the system's",
		"/var":      "/var holds /var/",
		"/srv/../x": "give the folder by its absolute path",
	} {
		payload := &agentv1.BackupRestore{Repository: h.repo, SnapshotId: snapshot, RestoreId: "r1", JobTag: testJobTag, TargetPath: dir, Source: "/volumes/app_data", DryRun: true}
		if _, err := h.restore(context.Background(), "p1", payload); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", dir, err)
		}
	}
	// a folder that leads elsewhere is restored where it leads, and said so
	h.folder(t, "/data/app", nil)
	if err := os.Symlink("/data", filepath.Join(h.host, "srv/link")); err != nil {
		t.Fatal(err)
	}
	payload := &agentv1.BackupRestore{Repository: h.repo, SnapshotId: snapshot, RestoreId: "r1", JobTag: testJobTag, TargetPath: "/srv/link/app", Source: "/volumes/app_data", DryRun: true}
	result, err := h.restore(context.Background(), "p2", payload)
	if err != nil {
		t.Fatal(err)
	}
	notes, _ := result["notes"].([]any)
	if mount := mountsOf(h.restoreJobs()[0])["/restore/0"]; mount["Source"] != "/data/app" || len(notes) == 0 || notes[0] != "The folder /srv/link/app leads to /data/app on this server: restoring into /data/app" {
		t.Errorf("a folder through a link: %v %v", mountsOf(h.restoreJobs()[0]), notes)
	}
	// never into a local target's repository
	disk := filepath.Join(t.TempDir(), "restic")
	local := &agentv1.BackupRepository{TargetId: "t2", Url: "/repo", Password: "p", Mount: disk}
	payload = &agentv1.BackupRestore{Repository: local, SnapshotId: snapshot, RestoreId: "r1", JobTag: testJobTag, TargetPath: disk + "/x", Source: "/", DryRun: true}
	if _, err := h.restore(context.Background(), "p3", payload); err == nil || !strings.Contains(err.Error(), "is the repository of the target this restore reads") {
		t.Errorf("into the repository: %v", err)
	}
}
