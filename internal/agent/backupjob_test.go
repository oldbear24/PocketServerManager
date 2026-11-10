package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
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
	if slices.Contains(job.Cmd, "--cache") || len(mountsOf(job)) != 0 {
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
