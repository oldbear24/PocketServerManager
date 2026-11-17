package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

// checkAgent is an agent with a local target whose repository holds a few
// snapshots, and a remote one, for integrity checks.
type checkAgent struct {
	*runAgent
	local, remote *agentv1.BackupRepository
}

func newCheckAgent(t *testing.T) *checkAgent {
	t.Helper()
	r := newRunAgent(t)
	c := &checkAgent{runAgent: r, remote: r.remote(t)}
	c.local = &agentv1.BackupRepository{TargetId: "t2", Url: backup.LocalMount, Password: "repo-password-2", Mount: filepath.Join(t.TempDir(), "restic")}
	if _, err := r.handleBackupInit(context.Background(), initCommand(c.local), &Reporter{}); err != nil {
		t.Fatal(err)
	}
	r.volume(t, "data", map[string]string{"a": "1"})
	for _, repo := range []*agentv1.BackupRepository{c.local, c.remote} {
		for range 3 {
			if _, err := r.handleBackupRun(context.Background(), runCommand(&agentv1.BackupRun{
				Repository: repo, Volumes: []string{"data"}, Tags: []string{"pocketcloud", "job:j31"}, HostName: "nas",
			}), &Reporter{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	checkProgressForward = 0
	t.Cleanup(func() { checkProgressForward = 5 * time.Second })
	return c
}

// check runs a check of repo's subset as the command id, and returns its
// answer read back as the panel reads it, the jobs it ran, the progress it
// reported and its error.
func (c *checkAgent) check(t *testing.T, id string, repo *agentv1.BackupRepository, subset string) (backup.CheckResult, []fakedocker.Job, []*agentv1.CommandProgress, error) {
	t.Helper()
	before := c.jobCount()
	panel := &fakePanel{}
	answer, err := c.handleBackupCheck(context.Background(), commandOf(id, &agentv1.BackupCheck{Repository: repo, ReadDataSubset: subset}), reporterFor(t, panel, id))
	// the answer crosses the stream, and the panel reads it back
	message, structErr := structpb.NewStruct(answer)
	if structErr != nil {
		t.Fatalf("the answer cannot cross the stream: %v", structErr)
	}
	raw, _ := json.Marshal(message.AsMap())
	var result backup.CheckResult
	if jsonErr := json.Unmarshal(raw, &result); jsonErr != nil {
		t.Fatal(jsonErr)
	}
	for _, key := range []string{"status", "subset", "packs_read", "packs_total", "errors", "errors_more", "repair", "notes", "unconfirmed", "confirmed", "lock", "restic_code", "error", "log"} {
		if _, ok := answer[key]; !ok {
			t.Errorf("the answer has no %s: %v", key, answer)
		}
	}
	c.mu.Lock()
	jobs := slices.Clone(c.jobs[before:])
	c.mu.Unlock()
	return result, jobs, panel.progress(id), err
}

// TestAnIntegrityCheckReadsWithoutALockFirst (#31): pass 1 runs restic
// check of the group without a lock, in a job that binds a local target's
// directory read-only with no capability but DAC_OVERRIDE and no network
// (a remote target's has the network and no capability), mounts no cache,
// keeps a small log, is named after its command, and says how far it got;
// when it finds nothing, it is the only pass, and the check is ok.
func TestAnIntegrityCheckReadsWithoutALockFirst(t *testing.T) {
	c := newCheckAgent(t)
	unlocks := fakerestic.Unlocks(t, c.root)
	result, jobs, progress, err := c.check(t, "kcheck000000001", c.local, "2/4")
	if err != nil || result.Status != backup.CheckStatusOK || result.Subset != "2/4" || result.PacksRead != 2 || result.PacksTotal != 6 ||
		result.Confirmed || result.Unconfirmed || len(result.Errors) != 0 || result.Lock != nil || result.Error != "" || result.ResticCode != 0 {
		t.Fatalf("%+v %v", result, err)
	}
	if !strings.Contains(result.Log, "no errors were found\n") || strings.Contains(result.Log, "repo-password-2") || strings.Contains(result.Log, "load indexes") {
		t.Errorf("the log:\n%s", result.Log)
	}
	if len(jobs) != 1 {
		t.Fatalf("%d jobs ran", len(jobs))
	}
	job := jobs[0]
	bind := mountsOf(job)[backup.LocalMount]
	if bind["ReadOnly"] != true || bind["BindOptions"] != nil || !slices.Equal(anyStrings(job.Host["CapAdd"]), []string{"DAC_OVERRIDE"}) ||
		job.Host["NetworkMode"] != "none" || !slices.Equal(anyStrings(job.Host["CapDrop"]), []string{"ALL"}) {
		t.Errorf("pass 1's job: %v", job.Host)
	}
	if mountsOf(job)[resticCacheDir] != nil || slices.Contains(job.Cmd, "--cache") || len(mountsOf(job)) != 2 || !coversImageVolume(job.Host["Mounts"].([]any)) {
		t.Errorf("pass 1 uses the cache: %q %v", job.Cmd, mountsOf(job))
	}
	if !slices.Equal(job.Cmd[:6], []string{"backup-tool", "check", "--subset", "2/4", "--pass", "free"}) {
		t.Errorf("pass 1 runs %q", job.Cmd)
	}
	logConfig, _ := job.Host["LogConfig"].(map[string]any)
	options, _ := logConfig["Config"].(map[string]any)
	if job.Hostname != "pc-kcheck000000001" || job.Labels["pocketcloud.command"] != "kcheck000000001" || job.Labels[backupTargetLabel] != "t2" ||
		logConfig["Type"] != "json-file" || options["max-size"] != "10m" || options["max-file"] != "1" {
		t.Errorf("pass 1's job: %q %v %v", job.Hostname, job.Labels, job.Host["LogConfig"])
	}
	var steps []string
	for _, p := range progress {
		steps = append(steps, p.GetStep())
		if p.GetStep() == "read" && (p.GetMessage() != "Reading group 2/4 of the data packs" || p.GetUnit() != "packs" || p.GetTotal() != 2) {
			t.Errorf("a read's progress: %v", p)
		}
	}
	if !slices.Equal(slices.Compact(steps), []string{"index", "packs", "snapshots", "read"}) || progress[len(progress)-1].GetPercent() != 100 {
		t.Errorf("progress: %v", progress)
	}
	if fakerestic.Unlocks(t, c.root) != unlocks {
		t.Error("pass 1 unlocked")
	}

	// a remote target's job has the network, and no capability
	result, jobs, _, err = c.check(t, "kcheck000000002", c.remote, "")
	if err != nil || result.Status != backup.CheckStatusOK || result.Subset != "" || result.PacksRead != 0 || len(jobs) != 1 {
		t.Fatalf("a remote target: %+v %v", result, err)
	}
	if job := jobs[0]; job.Host["NetworkMode"] != "bridge" || len(anyStrings(job.Host["CapAdd"])) != 0 || mountsOf(job)[backup.LocalMount] != nil ||
		mountsOf(job)[resticCacheDir] != nil || !slices.Equal(job.Cmd[:6], []string{"backup-tool", "check", "--subset", "", "--pass", "free"}) {
		t.Errorf("a remote target's job: %q %v", job.Cmd, job.Host)
	}

	// a check's result is kept, as a run's
	if ephemeral(commandOf("kcheck000000003", &agentv1.BackupCheck{Repository: c.local})) {
		t.Error("a check's result is not kept")
	}
	x := newExecutor(nil, slog.New(slog.DiscardHandler))
	(&Agent{Executor: x}).registerBackupHandlers()
	if !slices.Contains(x.Capabilities(), "backup.check") {
		t.Errorf("the agent says it runs %q", x.Capabilities())
	}
}

// TestASecondPassConfirmsWhatTheFirstFound (#31): only errors of pass 1 make
// pass 2 read the same group again, under restic's exclusive lock, from the
// directory bound writable with DAC_OVERRIDE and FOWNER, as restic writes its
// lock there; errors it confirms fail the check, errors it does not see were
// a prune's, and a pass 2 that cannot lock the repository, or fails, leaves
// them suspect.
func TestASecondPassConfirmsWhatTheFirstFound(t *testing.T) {
	c := newCheckAgent(t)

	t.Setenv("FAKE_RESTIC_CHECK", "errors")
	result, jobs, progress, err := c.check(t, "kcheck000000011", c.local, "2/4")
	if err == nil || err.Error() != "restic found 1 errors in group 2/4" || result.Status != backup.CheckStatusFailed || !result.Confirmed || result.Unconfirmed ||
		len(result.Errors) != 1 || result.Errors[0].Kind != "pack" || !slices.Equal(result.Repair.Packs, []string{result.Errors[0].ID}) || !result.Repair.Snapshots ||
		result.ResticCode != 1 || result.Error != "" || result.PacksRead != 2 {
		t.Errorf("confirmed errors: %+v %v", result, err)
	}
	if len(jobs) != 2 {
		t.Fatalf("%d jobs ran", len(jobs))
	}
	first, second := jobs[0], jobs[1]
	bind := mountsOf(second)[backup.LocalMount]
	if bind["ReadOnly"] == true || !slices.Equal(anyStrings(second.Host["CapAdd"]), []string{"DAC_OVERRIDE", "FOWNER"}) || second.Host["NetworkMode"] != "none" ||
		mountsOf(second)[resticCacheDir] != nil || !slices.Equal(second.Cmd[:6], []string{"backup-tool", "check", "--subset", "2/4", "--pass", "confirm"}) {
		t.Errorf("pass 2's job: %q %v", second.Cmd, second.Host)
	}
	if mountsOf(first)[backup.LocalMount]["ReadOnly"] != true || first.Hostname != "pc-kcheck000000011" || second.Hostname != "pc-kcheck000000011" ||
		second.Labels["pocketcloud.command"] != "kcheck000000011" {
		t.Errorf("the passes' jobs: %q %q %v", first.Hostname, second.Hostname, second.Labels)
	}
	if !strings.Contains(result.Log, "restic found errors without a lock, as a prune running meanwhile can cause: checking group 2/4 again with the repository locked\n") ||
		strings.Count(result.Log, "Fatal: repository contains errors") != 2 {
		t.Errorf("the log:\n%s", result.Log)
	}
	confirm := slices.IndexFunc(progress, func(p *agentv1.CommandProgress) bool { return p.GetStep() == "confirm" })
	if confirm < 0 || !strings.HasPrefix(progress[confirm].GetMessage(), "Checking group 2/4 again with the repository locked: ") ||
		slices.ContainsFunc(progress[confirm:], func(p *agentv1.CommandProgress) bool { return p.GetStep() != "confirm" }) {
		t.Errorf("progress: %v", progress)
	}

	// what a prune that ran meanwhile made pass 1 see, pass 2 does not
	t.Setenv("FAKE_RESTIC_CHECK", "errors_free")
	result, jobs, _, err = c.check(t, "kcheck000000012", c.remote, "2/4")
	if err != nil || result.Status != backup.CheckStatusOK || !result.Unconfirmed || result.Confirmed || len(result.Errors) != 1 ||
		result.Errors[0].Kind != "other" || !result.Repair.Other || len(jobs) != 2 || result.ResticCode != 0 {
		t.Errorf("unconfirmed errors: %+v %v (%d jobs)", result, err, len(jobs))
	}

	// a pass 2 that another restic keeps out, after an unlock and another
	// try, leaves them suspect, with the lock
	t.Setenv("FAKE_RESTIC_LOCKED_CHECK", "2")
	t.Setenv("FAKE_RESTIC_LOCK_HOST", "pc-k3j9x2m4q8w1z7v")
	unlocks := fakerestic.Unlocks(t, c.root)
	result, _, _, err = c.check(t, "kcheck000000013", c.local, "2/4")
	if err == nil || result.Status != backup.CheckStatusSuspect || result.Confirmed || result.Unconfirmed || len(result.Errors) != 1 ||
		result.Lock == nil || result.Lock.Host != "pc-k3j9x2m4q8w1z7v" || !result.Lock.Exclusive || result.Lock.Created.IsZero() ||
		result.ResticCode != 11 || !strings.Contains(result.Error, "is already locked exclusively by PID 1 on pc-k3j9x2m4q8w1z7v") || err.Error() != result.Error {
		t.Errorf("a locked pass 2: %+v %v", result, err)
	}
	if fakerestic.Unlocks(t, c.root) != unlocks+1 {
		t.Errorf("pass 2 unlocked %d times", fakerestic.Unlocks(t, c.root)-unlocks)
	}

	// with the server's clock off, pass 2 removes no lock
	c.lastSkew.Store((10 * time.Minute).Microseconds())
	t.Setenv("FAKE_RESTIC_LOCKED_CHECK", "4")
	unlocks = fakerestic.Unlocks(t, c.root)
	result, jobs, _, _ = c.check(t, "kcheck000000014", c.local, "2/4")
	if result.Status != backup.CheckStatusSuspect || fakerestic.Unlocks(t, c.root) != unlocks || !strings.Contains(result.Log, "The server's clock is off by 10 min") ||
		!slices.Contains(jobs[1].Cmd, "--no-unlock=10") {
		t.Errorf("a clock off: %+v %q", result, jobs[1].Cmd)
	}
	c.lastSkew.Store(0)
	t.Setenv("FAKE_RESTIC_LOCKED_CHECK", "")

	// a pass 2 that did not run leaves them suspect too
	c.engine.OnJob = func(run func(fakedocker.Job) fakedocker.JobResult) func(fakedocker.Job) fakedocker.JobResult {
		return func(job fakedocker.Job) fakedocker.JobResult {
			c.engine.Fail("POST /containers/create", "the Engine is gone")
			return run(job)
		}
	}(c.engine.OnJob)
	result, _, _, err = c.check(t, "kcheck000000015", c.local, "2/4")
	if err == nil || result.Status != backup.CheckStatusSuspect || !strings.Contains(result.Error, "the Engine is gone") || len(result.Errors) != 1 {
		t.Errorf("pass 2 did not run: %+v %v", result, err)
	}
}

// TestACheckThatDidNotCompleteSaysWhy (#31): a pass 1 that restic or the
// Engine failed ends the check with its error, and no pass 2; a structure
// that restic finds damaged fails it, said as the structure's errors.
func TestACheckThatDidNotCompleteSaysWhy(t *testing.T) {
	c := newCheckAgent(t)
	wrong := &agentv1.BackupRepository{TargetId: c.remote.GetTargetId(), Url: c.remote.GetUrl(), Password: "not-the-password", Env: c.remote.GetEnv()}
	result, jobs, _, err := c.check(t, "kcheck000000021", wrong, "1/20")
	if err == nil || result.Status != backup.CheckStatusError || result.ResticCode != 12 || len(jobs) != 1 ||
		result.Error != "a repository is there, and the password is not its own: Fatal: wrong password or no key found" || err.Error() != result.Error {
		t.Errorf("a wrong password: %+v %v", result, err)
	}

	dir := filepath.Join(t.TempDir(), "gone")
	c.engine.Fail("POST /containers/create", `invalid mount config for type "bind": bind source path does not exist: `+dir)
	result, _, _, err = c.check(t, "kcheck000000022", c.local, "1/20")
	c.engine.Fail("POST /containers/create", "")
	if err == nil || result.Status != backup.CheckStatusError || result.Error != "the directory "+dir+" does not exist on this server" {
		t.Errorf("a directory that is gone: %+v %v", result, err)
	}

	t.Setenv("FAKE_RESTIC_CHECK", "tree")
	result, jobs, _, err = c.check(t, "kcheck000000023", c.local, "")
	if err == nil || err.Error() != "restic found 1 errors in the repository's structure" || result.Status != backup.CheckStatusFailed || len(jobs) != 2 ||
		result.Errors[0].Kind != "tree" || !result.Repair.Other {
		t.Errorf("a damaged tree: %+v %v", result, err)
	}

	// what the panel did not complete, or a group that is no group, runs
	// no job
	before := c.jobCount()
	for _, payload := range []*agentv1.BackupCheck{
		{Repository: &agentv1.BackupRepository{TargetId: "t1", Url: "s3:http://minio:9000/backups"}, ReadDataSubset: "1/20"},
		{Repository: c.remote, ReadDataSubset: "5%"},
		{Repository: c.remote, ReadDataSubset: "21/20"},
	} {
		if answer, err := c.handleBackupCheck(context.Background(), commandOf("kcheck000000024", payload), &Reporter{}); err == nil || answer != nil {
			t.Errorf("%v: %v %v", payload, answer, err)
		}
	}
	if c.jobCount() != before {
		t.Errorf("%d jobs ran", c.jobCount()-before)
	}
}

// TestAStoppedCheckGivesResticItsGrace (#31): a check that is stopped stops
// its job with 30 seconds' grace, in either pass, as restic then removes its
// lock: a pass 2 that is killed leaves its exclusive lock for 30 minutes.
func TestAStoppedCheckGivesResticItsGrace(t *testing.T) {
	c := newCheckAgent(t)
	t.Setenv("FAKE_RESTIC_CHECK", "errors")
	for _, pass := range []string{backup.PassFree, backup.PassConfirm} {
		entered := make(chan string, 1)
		run := c.engine.OnJob
		c.engine.OnJob = func(job fakedocker.Job) fakedocker.JobResult {
			if !slices.Contains(job.Cmd, pass) {
				return run(job)
			}
			entered <- job.Name
			<-job.Stop
			return fakedocker.JobResult{Code: 130, Stderr: "error: signal: terminated"}
		}
		ctx, cancel := context.WithCancel(context.Background())
		type ended struct {
			answer map[string]any
			err    error
		}
		done := make(chan ended, 1)
		go func() {
			answer, err := c.handleBackupCheck(ctx, commandOf("kcheck000000031", &agentv1.BackupCheck{Repository: c.local, ReadDataSubset: "1/4"}), &Reporter{})
			done <- ended{answer, err}
		}()
		name := <-entered
		cancel()
		got := <-done
		c.engine.OnJob = run
		if got.err == nil || got.err.Error() != checkStopped || got.answer["error"] != checkStopped {
			t.Errorf("%s: %v %v", pass, got.answer, got.err)
		}
		var calls []string
		prefix := "/containers/" + strings.TrimPrefix(name, "generated-")
		for _, call := range c.engine.Calls() {
			if strings.Contains(call, prefix) && (strings.Contains(call, "/stop?") || strings.HasPrefix(call, "DELETE ")) {
				calls = append(calls, call)
			}
		}
		if len(calls) != 2 || !strings.HasSuffix(calls[0], "/stop?t=30") || !strings.HasPrefix(calls[1], "DELETE ") {
			t.Errorf("%s's job: %q", pass, calls)
		}
	}
}
