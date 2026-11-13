package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

// TestToolMarksKeepTheToolsLinesOutOfTheLog (#30): the backup tool's marks
// open what waits for them and its progress lines reach the agent, however
// the output is cut into writes; neither reaches the log, and every other
// line does, as it is.
func TestToolMarksKeepTheToolsLinesOutOfTheLog(t *testing.T) {
	var log bytes.Buffer
	ready, relay := make(chan struct{}), make(chan struct{})
	var progress []string
	marks := &toolMarks{w: &log, marks: map[string]chan struct{}{restoreReady: ready, relayReady: relay},
		progress: func(data []byte) { progress = append(progress, string(data)) }}
	output := "restic opens the repository\n" + restoreReady + "\r\n" + progressLine([]byte(`{"percent":40}`)) + "\n" +
		restoreReady + "\n--- pocketcloud: progress is a word ---\nrestored 3 files\n"
	for i := 0; i < len(output); i += 7 {
		if _, err := marks.Write([]byte(output[i:min(i+7, len(output))])); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-ready:
	default:
		t.Error("the mark did not open its channel")
	}
	select {
	case <-relay:
		t.Error("a mark the tool never said opened its channel")
	default:
	}
	if !slices.Equal(progress, []string{`{"percent":40}`}) {
		t.Errorf("progress: %q", progress)
	}
	if log.String() != "restic opens the repository\n--- pocketcloud: progress is a word ---\nrestored 3 files\n" {
		t.Errorf("the log: %q", log.String())
	}
	if !toolLine(restoreReady) || !toolLine(progressLine([]byte("{}"))) || toolLine("restic failed") {
		t.Error("toolLine")
	}
}

// TestARestoreJobKeepsNoCacheAndSaysItsCommand (#30): a job without the
// cache neither mounts restic's cache nor names it, its log is limited, and
// it carries the command's label besides those cleanupBackupJobs goes by,
// which the job's own cannot change.
func TestARestoreJobKeepsNoCacheAndSaysItsCommand(t *testing.T) {
	a, engine, _ := backupAgent(t)
	var job fakedocker.Job
	engine.OnJob = func(j fakedocker.Job) fakedocker.JobResult {
		job = j
		return fakedocker.JobResult{Stdout: "{\"ok\":true}\n"}
	}
	logConfig := map[string]any{"Type": "json-file", "Config": map[string]any{"max-size": "10m", "max-file": "1"}}
	// a job that is no job to the engine would never end
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := a.backupJob(ctx, &agentv1.BackupRepository{TargetId: "t1", Url: "s3:http://minio:9000/b", Password: "p"},
		backupJobSpec{args: []string{"snapshots"}, noCache: true, logConfig: logConfig,
			labels: map[string]string{"pocketcloud.command": "c1", "pocketcloud.role": "app"}}, io.Discard)
	if err != nil || string(out) != `{"ok":true}` {
		t.Fatalf("%s %v", out, err)
	}
	if slices.Contains(job.Cmd, "--cache") || len(mountsOf(job)) != 1 || !coversImageVolume(job.Host["Mounts"].([]any)) {
		t.Errorf("the job uses the cache: %q %v", job.Cmd, mountsOf(job))
	}
	if job.Labels["pocketcloud.command"] != "c1" || job.Labels["pocketcloud.role"] != "job" || job.Labels[backupTargetLabel] != "t1" {
		t.Errorf("labels: %v", job.Labels)
	}
	config, _ := job.Host["LogConfig"].(map[string]any)
	options, _ := config["Config"].(map[string]any)
	if config["Type"] != "json-file" || options["max-size"] != "10m" {
		t.Errorf("the log: %v", job.Host["LogConfig"])
	}
}

// failingWriter takes n bytes, then fails, as an upload whose browser left.
type failingWriter struct {
	n       int
	written bytes.Buffer
}

func (f *failingWriter) Write(data []byte) (int, error) {
	if f.written.Len()+len(data) > f.n {
		return 0, errors.New("the browser left")
	}
	return f.written.Write(data)
}

// TestAnAttachedJobStreamsItsOutputAndKeepsNoLog (#30): a download's job
// keeps no log: its output reaches the agent through the attached output,
// byte for byte, while its error output still goes to the log; a writer that
// fails stops the job with its grace, as restic then removes its lock.
func TestAnAttachedJobStreamsItsOutputAndKeepsNoLog(t *testing.T) {
	a, engine, _ := backupAgent(t)
	data := append(bytes.Repeat([]byte{0, 0xff, '\n'}, 300<<10), "end"...)
	var job fakedocker.Job
	engine.OnJob = func(j fakedocker.Job) fakedocker.JobResult {
		job = j
		for i := 0; i < len(data); i += 64 << 10 {
			if _, err := j.Stdout.Write(data[i:min(i+64<<10, len(data))]); err != nil {
				return fakedocker.JobResult{Code: 1}
			}
		}
		_, _ = io.WriteString(j.Stderr, "dumped\n")
		if slices.Contains(j.Cmd, "--wait") {
			<-j.Stop
			return fakedocker.JobResult{Code: 130, Stderr: "error: signal: terminated"}
		}
		return fakedocker.JobResult{}
	}
	repo := &agentv1.BackupRepository{TargetId: "t1", Url: "s3:http://minio:9000/b", Password: "p"}
	var got, log bytes.Buffer
	if _, err := a.backupJob(context.Background(), repo, backupJobSpec{args: []string{"dump"}, attachOutput: &got, stopGrace: backupStopGrace}, &log); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), data) || log.String() != "dumped\n" {
		t.Errorf("%d bytes, log %q", got.Len(), log.String())
	}
	if config, _ := job.Host["LogConfig"].(map[string]any); config["Type"] != "none" {
		t.Errorf("the job keeps a log: %v", job.Host["LogConfig"])
	}
	for _, call := range engine.Calls() {
		if strings.Contains(call, "/logs") {
			t.Errorf("the job's log was read: %s", call)
		}
	}

	// the browser leaves
	writer := &failingWriter{n: 128 << 10}
	done := make(chan error, 1)
	go func() {
		_, err := a.backupJob(context.Background(), repo, backupJobSpec{args: []string{"dump", "--wait"}, attachOutput: writer, stopGrace: backupStopGrace}, io.Discard)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "the browser left") {
			t.Errorf("the job: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the job went on after its output failed")
	}
	stopped := false
	for _, call := range engine.Calls() {
		stopped = stopped || strings.HasSuffix(call, "/stop?t=30")
	}
	if !stopped {
		t.Errorf("the job was not stopped with its grace: %q", engine.Calls())
	}
}

// commandOf is a command of id with payload, of the type the payload is.
func commandOf(id string, payload any) *agentv1.Command {
	c := &agentv1.Command{Id: id}
	switch p := payload.(type) {
	case *agentv1.BackupInit:
		c.Type, c.Payload = "backup.init", &agentv1.Command_BackupInit{BackupInit: p}
	case *agentv1.BackupRun:
		c.Type, c.Payload = "backup.run", &agentv1.Command_BackupRun{BackupRun: p}
	case *agentv1.BackupSnapshots:
		c.Type, c.Payload = "backup.snapshots", &agentv1.Command_BackupSnapshots{BackupSnapshots: p}
	case *agentv1.BackupBrowse:
		c.Type, c.Payload = "backup.browse", &agentv1.Command_BackupBrowse{BackupBrowse: p}
	case *agentv1.BackupRestore:
		c.Type, c.Payload = "backup.restore", &agentv1.Command_BackupRestore{BackupRestore: p}
	case *agentv1.BackupDump:
		c.Type, c.Payload = "backup.dump", &agentv1.Command_BackupDump{BackupDump: p}
	case *agentv1.BackupCheck:
		c.Type, c.Payload = "backup.check", &agentv1.Command_BackupCheck{BackupCheck: p}
	}
	return c
}

// TestEveryBackupJobIsNamedAfterItsCommand (#31): restic names a lock's
// holder by its hostname, so every backup job's is pc-<its command's id>,
// beside the command's label, and a lock says which command holds it; a job
// that runs for no command keeps the Engine's own hostname.
func TestEveryBackupJobIsNamedAfterItsCommand(t *testing.T) {
	h := newRestoreHarness(t)
	livePanel(t, h.Agent)
	h.volume(t, "app_data", map[string]string{"a.txt": "a"})
	ctx := context.Background()
	if _, err := h.handleBackupInit(ctx, commandOf("kinit0000000001", &agentv1.BackupInit{Repository: h.repo}), &Reporter{}); err != nil {
		t.Fatal(err)
	}
	result, err := h.handleBackupRun(ctx, commandOf("krun00000000001", &agentv1.BackupRun{
		Repository: h.repo, Volumes: []string{"app_data"}, Tags: []string{"pocketcloud", testJobTag}, HostName: "nas"}), &Reporter{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := result["snapshot_id"].(string)
	if _, err := h.handleBackupSnapshots(ctx, commandOf("klist0000000001", &agentv1.BackupSnapshots{Repository: h.repo}), &Reporter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.handleBackupBrowse(ctx, commandOf("kbrowse00000001", &agentv1.BackupBrowse{Repository: h.repo, SnapshotId: snapshot, Path: "/"}), &Reporter{}); err != nil {
		t.Fatal(err)
	}
	preview := h.payload(snapshot, "app_data")
	preview.DryRun, preview.Overwrite, preview.Replace = true, false, false
	if _, err := h.restore(ctx, "kpreview0000001", preview); err != nil {
		t.Fatal(err)
	}
	if _, err := h.restore(ctx, "krestore0000001", h.payload(snapshot, "app_data")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.handleBackupDump(ctx, commandOf("kdump0000000001", &agentv1.BackupDump{Repository: h.repo, SnapshotId: snapshot,
		Path: "/volumes/app_data/a.txt", RestoreId: "r1", FileName: "a.txt", JobTag: testJobTag}), &Reporter{}); err != nil {
		t.Fatal(err)
	}

	h.mu.Lock()
	jobs := slices.Clone(h.jobs)
	h.mu.Unlock()
	var named []string
	for _, job := range jobs {
		id := job.Labels["pocketcloud.command"]
		switch {
		case id == "" && job.Hostname != "":
			t.Errorf("a job for no command has the hostname %q", job.Hostname)
		case id != "" && job.Hostname != "pc-"+id:
			t.Errorf("the job of %s (%q) has the hostname %q", id, job.Cmd, job.Hostname)
		case id != "":
			named = append(named, id)
		}
	}
	if want := []string{"kinit0000000001", "krun00000000001", "klist0000000001", "kbrowse00000001", "kpreview0000001", "krestore0000001", "kdump0000000001"}; !slices.Equal(named, want) {
		t.Errorf("jobs named after their commands: %q, want %q", named, want)
	}
}

// TestAServerWhoseClockIsOffUnlocksNothing (#31): while the server's clock
// is more than 5 minutes off the panel's, ahead or behind, every backup tool
// call says by how many minutes, and the tool's unlocks remove nothing; a
// run that finds the repository locked tries once more all the same.
func TestAServerWhoseClockIsOffUnlocksNothing(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	r.volume(t, "data", map[string]string{"a": "1"})
	for i, c := range []struct {
		skew time.Duration
		flag string
	}{
		{6*time.Minute + 20*time.Second, "--no-unlock=6"},
		{-(7*time.Minute + 40*time.Second), "--no-unlock=8"},
		{5 * time.Minute, ""},
		{-5 * time.Minute, ""},
		{0, ""},
	} {
		r.lastSkew.Store(c.skew.Microseconds())
		// the first try of each run finds the repository locked
		t.Setenv("FAKE_RESTIC_LOCKED_BACKUP", strconv.Itoa(i+1))
		result, err := r.handleBackupRun(context.Background(), runCommand(&agentv1.BackupRun{
			Repository: repo, Volumes: []string{"data"}, Tags: []string{"pocketcloud", "job:j1"}, HostName: "nas",
		}), &Reporter{})
		job := r.lastJob(t)
		var flags []string
		for _, arg := range job.Cmd {
			if strings.HasPrefix(arg, "--no-unlock") {
				flags = append(flags, arg)
			}
		}
		log, _ := result["log"].(string)
		switch {
		case c.flag == "" && len(flags) > 0, c.flag != "" && !slices.Equal(flags, []string{c.flag}):
			t.Errorf("a clock off by %v: %q", c.skew, job.Cmd)
		case c.flag != "" && !strings.Contains(log, "so no lock is removed\n"):
			t.Errorf("a clock off by %v: the log\n%s", c.skew, log)
		case c.flag == "" && strings.Contains(log, "so no lock is removed"):
			t.Errorf("a clock off by %v removed no lock:\n%s", c.skew, log)
		case err != nil:
			t.Errorf("a clock off by %v: %v", c.skew, err)
		}
	}
	if unlocks := fakerestic.Unlocks(t, r.root); unlocks != 3 {
		t.Errorf("restic unlock ran %d times, for the 3 runs whose server's clock was right", unlocks)
	}
}

// TestARepositoryBoundReadOnlyIsReadOnly (#31): a job that only reads a
// local target's repository, as the first pass of an integrity check,
// binds it read-only, without making the directory where it is gone, and
// with no capability but DAC_OVERRIDE, which reads it whoever owns it; any
// other binds it writable, with FOWNER besides, as restic writes its locks
// there.
func TestARepositoryBoundReadOnlyIsReadOnly(t *testing.T) {
	a, engine, _ := backupAgent(t)
	var job fakedocker.Job
	engine.OnJob = func(j fakedocker.Job) fakedocker.JobResult {
		job = j
		return fakedocker.JobResult{Stdout: "{}\n"}
	}
	repo := &agentv1.BackupRepository{TargetId: "t1", Url: "/repo", Password: "p", Mount: "/srv/backups"}
	for _, readOnly := range []bool{true, false} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := a.backupJob(ctx, repo, backupJobSpec{args: []string{"check"}, noCache: true, repoReadOnly: readOnly}, io.Discard)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		bind := mountsOf(job)["/repo"]
		capabilities := anyStrings(job.Host["CapAdd"])
		switch {
		case readOnly && (bind["ReadOnly"] != true || bind["BindOptions"] != nil || !slices.Equal(capabilities, []string{"DAC_OVERRIDE"})):
			t.Errorf("read-only: %v, may %q", bind, capabilities)
		case !readOnly && (bind["ReadOnly"] == true || bind["BindOptions"] == nil || !slices.Equal(capabilities, []string{"DAC_OVERRIDE", "FOWNER"})):
			t.Errorf("writable: %v, may %q", bind, capabilities)
		}
		if bind["Source"] != "/srv/backups" || job.Host["NetworkMode"] != "none" {
			t.Errorf("the repository's mount: %v, network %v", bind, job.Host["NetworkMode"])
		}
	}

	// a remote repository has no mount and no capability either way
	remote := &agentv1.BackupRepository{TargetId: "t2", Url: "s3:http://minio:9000/b", Password: "p"}
	if _, err := a.backupJob(context.Background(), remote, backupJobSpec{args: []string{"check"}, noCache: true, repoReadOnly: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if mountsOf(job)["/repo"] != nil || job.Host["CapAdd"] != nil || job.Host["NetworkMode"] != "bridge" {
		t.Errorf("a remote repository's job: %v", job.Host)
	}
}
