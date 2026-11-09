package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

// runBackup runs the backup op, with stdin as the job's input, and returns
// its result, which a snapshot that lacks files has too, and its log.
func runBackup(t *testing.T, env map[string]string, stdin io.Reader, args ...string) (*backupRunResult, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := BackupTool(context.Background(), append([]string{"backup"}, args...), env, stdin, &stdout, &stderr)
	if stdout.Len() == 0 {
		return nil, stderr.String(), err
	}
	var result backupRunResult
	if decodeErr := json.Unmarshal(lastLine(stdout.Bytes()), &result); decodeErr != nil {
		t.Fatalf("the result %q: %v", stdout.String(), decodeErr)
	}
	return &result, stderr.String(), err
}

// backupRepository is a repository of the fake restic, created.
func backupRepository(t *testing.T) (map[string]string, string) {
	t.Helper()
	root := fakerestic.Install(t)
	env := map[string]string{"RESTIC_REPOSITORY": "rest:http://backup-node:8000/jobs/", "RESTIC_PASSWORD": "pw-jobs",
		"RESTIC_REST_USERNAME": "homelab", "RESTIC_REST_PASSWORD": "rest-secret"}
	if _, _, err := runBackupTool(t, env, "init"); err != nil {
		t.Fatal(err)
	}
	return env, root
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTheBackupToolSavesDirectoriesWithTheRunsHostAndTags (#29): restic
// backs up the directories with the server's name and every tag of the run,
// quietly and waiting for another's lock; the result is restic's summary.
func TestTheBackupToolSavesDirectoriesWithTheRunsHostAndTags(t *testing.T) {
	env, root := backupRepository(t)
	dir := filepath.Join(t.TempDir(), "srv", "app")
	writeFiles(t, dir, map[string]string{"a.txt": "hello", "sub/b.txt": "world!"})
	args := []string{"--cache", "/cache", "--host", "nas", "--tag", "pocketcloud", "--tag", "job:j1", "--path", dir}

	first, log, err := runBackup(t, env, nil, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if len(first.SnapshotID) != 64 || first.FilesNew != 2 || first.FilesChanged != 0 || first.BytesAdded != 11 || first.TotalBytes != 11 || first.Forgotten != 0 {
		t.Errorf("the first backup: %+v", first)
	}
	if !strings.Contains(log, "Backing up "+dir) || !strings.Contains(log, "Saved the snapshot "+first.SnapshotID[:8]+": 2 new and 0 changed files, 11 B added, 11 B in all") {
		t.Errorf("the log:\n%s", log)
	}
	calls := fakerestic.Calls(t, root)
	if !strings.Contains(calls, "restic --cache-dir /cache --cleanup-cache backup --json --quiet --retry-lock 30m --host nas --tag pocketcloud,job:j1 -- "+dir+"\n") ||
		strings.Contains(calls, " forget ") {
		t.Errorf("restic ran:\n%s", calls)
	}

	writeFiles(t, dir, map[string]string{"sub/b.txt": "world, again"})
	second, _, err := runBackup(t, env, nil, args...)
	if err != nil || second.FilesNew != 0 || second.FilesChanged != 1 || second.BytesAdded != 12 || second.TotalBytes != 17 || second.SnapshotID == first.SnapshotID {
		t.Errorf("the second backup: %+v %v", second, err)
	}
	snapshots := fakerestic.Snapshots(t, root, env["RESTIC_REPOSITORY"])
	if len(snapshots) != 2 {
		t.Fatalf("snapshots: %+v", snapshots)
	}
	saved := snapshots[1]
	if saved.ID != second.SnapshotID || saved.Hostname != "nas" || !slices.Equal(saved.Tags, []string{"pocketcloud", "job:j1"}) ||
		!slices.Equal(saved.Paths, []string{dir}) || saved.Files[dir+"/sub/b.txt"] != "world, again" || saved.Files[dir+"/a.txt"] != "hello" {
		t.Errorf("the snapshot: %+v", saved)
	}

	// a repository that went, and one that another password opens
	gone := map[string]string{"RESTIC_REPOSITORY": "rest:http://backup-node:8000/gone/", "RESTIC_PASSWORD": "pw-jobs"}
	if _, _, err := runBackup(t, gone, nil, args...); err == nil || !strings.Contains(err.Error(), "the repository is gone, with its snapshots") {
		t.Errorf("a repository that is gone: %v", err)
	}
	env["RESTIC_PASSWORD"] = "another"
	if _, _, err := runBackup(t, env, nil, args...); err == nil || !strings.Contains(err.Error(), "the password is not its own") {
		t.Errorf("a wrong password: %v", err)
	}
}

// TestABackupThatCouldNotReadEverythingFails (#29): restic's exit code 3 saves
// the snapshot without the files it could not read; the run fails with what
// restic said, readable, and still says the snapshot; no retention follows.
func TestABackupThatCouldNotReadEverythingFails(t *testing.T) {
	env, root := backupRepository(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"open.txt": "x", "locked": "secret"})
	t.Setenv("FAKE_RESTIC_UNREADABLE", "locked")
	result, log, err := runBackup(t, env, nil, "--host", "nas", "--tag", "pocketcloud", "--tag", "job:j1", "--keep-last", "1", "--path", dir)
	if result == nil || len(result.SnapshotID) != 64 || result.FilesNew != 1 {
		t.Fatalf("the result: %+v", result)
	}
	want := "restic saved the snapshot " + result.SnapshotID[:8] + ", but could not read everything: open " + dir + "/locked: permission denied Warning: at least one source file could not be read"
	if err == nil || err.Error() != want {
		t.Errorf("the error:\n%v\nwant\n%s", err, want)
	}
	if strings.Contains(log, "message_type") || !strings.Contains(log, "open "+dir+"/locked: permission denied\n") {
		t.Errorf("the log:\n%s", log)
	}
	if strings.Contains(fakerestic.Calls(t, root), " forget ") {
		t.Error("the retention ran after a snapshot that lacks files")
	}
}

// TestTheRetentionThinsOutTheJobsSnapshotsOnly (#29): forget takes the
// snapshots with every tag of the run, grouped by tags, with the job's
// policy, and prunes; the result counts those that went. A retention that
// fails leaves the backup a success, and says why.
func TestTheRetentionThinsOutTheJobsSnapshotsOnly(t *testing.T) {
	env, root := backupRepository(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a": "1"})
	backupOf := func(job string, keep ...string) *backupRunResult {
		t.Helper()
		result, log, err := runBackup(t, env, nil, append([]string{"--host", "nas", "--tag", "pocketcloud", "--tag", "job:" + job, "--path", dir}, keep...)...)
		if err != nil {
			t.Fatalf("%v\n%s", err, log)
		}
		return result
	}
	for range 3 {
		backupOf("j1")
	}
	other := backupOf("j2")
	kept := backupOf("j1", "--keep-last", "2", "--keep-daily", "1", "--keep-yearly", "0")
	if kept.Forgotten != 2 || kept.RetentionError != "" {
		t.Errorf("the retention: %+v", kept)
	}
	calls := fakerestic.Calls(t, root)
	if !strings.Contains(calls, "restic --no-cache forget --json --prune --retry-lock 5m --group-by tags --tag pocketcloud,job:j1 --keep-last 2 --keep-daily 1\n") {
		t.Errorf("restic ran:\n%s", calls)
	}
	var ids []string
	for _, snapshot := range fakerestic.Snapshots(t, root, env["RESTIC_REPOSITORY"]) {
		ids = append(ids, snapshot.ID)
	}
	if len(ids) != 3 || ids[1] != other.SnapshotID || ids[2] != kept.SnapshotID {
		t.Errorf("snapshots: %v, the other job's %s, the last %s", ids, other.SnapshotID, kept.SnapshotID)
	}
	// nothing more to forget
	if again := backupOf("j1", "--keep-last", "2"); again.Forgotten != 1 {
		t.Errorf("again: %+v", again)
	}

	t.Setenv("FAKE_RESTIC_FORGET_FAIL", "unable to create lock in backend: repository is already locked exclusively by PID 7 on other-server by root (UID 0, GID 0)")
	failed, log, err := runBackup(t, env, nil, "--host", "nas", "--tag", "pocketcloud", "--tag", "job:j1", "--keep-last", "2", "--path", dir)
	if err != nil || failed.SnapshotID == "" || failed.RetentionError != "Fatal: unable to create lock in backend: repository is already locked exclusively by PID 7 on other-server by root (UID 0, GID 0)" {
		t.Errorf("a retention that failed: %+v %v", failed, err)
	}
	if !strings.Contains(log, "The retention failed: Fatal: unable to create lock") {
		t.Errorf("the log:\n%s", log)
	}
}

// resticCommands are the commands restic ran, in order, without the global
// options.
func resticCommands(t *testing.T, root string) []string {
	t.Helper()
	var commands []string
	for line := range strings.SplitSeq(fakerestic.Calls(t, root), "\n") {
		if args, ok := strings.CutPrefix(line, "restic "); ok {
			fields := strings.Fields(args)
			for len(fields) > 0 && strings.HasPrefix(fields[0], "-") {
				if fields[0] == "-o" || fields[0] == "--cache-dir" {
					fields = fields[1:]
				}
				fields = fields[1:]
			}
			if len(fields) > 0 {
				commands = append(commands, fields[0])
			}
		}
	}
	return commands
}

// lockedLine is what the fake restic says of the lock that keeps a backup
// out, as restic 0.17 does.
const lockedLine = "unable to create lock in backend: repository is already locked exclusively by PID 1 on other-server by root (UID 0, GID 0)"

// TestALockedRepositoryIsUnlockedAndTriedOnceMore (review of #29): a lock
// that no restic refreshes any more, as a job that was killed leaves, keeps
// every backup out until restic unlock removes it; a backup that finds the
// repository locked runs it and tries once more, also with a dump, whose
// relay restic never ran.
func TestALockedRepositoryIsUnlockedAndTriedOnceMore(t *testing.T) {
	env, root := backupRepository(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a": "1"})
	args := []string{"--host", "nas", "--tag", "pocketcloud", "--tag", "job:j1", "--path", dir}
	t.Setenv("FAKE_RESTIC_LOCKED_BACKUP", "1")
	result, log, err := runBackup(t, env, nil, args...)
	if err != nil || len(result.SnapshotID) != 64 {
		t.Fatalf("%+v %v\n%s", result, err, log)
	}
	if commands := resticCommands(t, root); !slices.Equal(commands, []string{"cat", "init", "backup", "unlock", "backup"}) || fakerestic.Unlocks(t, root) != 1 {
		t.Errorf("restic ran %q", commands)
	}
	if !strings.Contains(log, lockedLine+"\n") || !strings.Contains(log, "The repository is locked: removing the locks no restic holds any more, and trying once more\n") {
		t.Errorf("the log:\n%s", log)
	}

	// a lock that stays fails the backup once it was tried twice, with
	// restic's words of who holds it
	t.Setenv("FAKE_RESTIC_LOCKED_BACKUP", "3")
	if _, _, err := runBackup(t, env, nil, args...); err == nil || err.Error() != lockedLine {
		t.Errorf("a lock that stays: %v", err)
	}
	if commands := resticCommands(t, root); len(commands) != 8 || !slices.Equal(commands[5:], []string{"backup", "unlock", "backup"}) {
		t.Errorf("restic ran %q", commands)
	}

	// a dump is read once restic got the lock
	t.Setenv("FAKE_RESTIC_LOCKED_BACKUP", "4")
	socket := filepath.Join(t.TempDir(), "dump.sock")
	result, log, err = runBackup(t, env, dumpFrames(t, "CREATE TABLE t;\n", &backup.DumpEnd{}),
		"--host", "nas", "--tag", "pocketcloud", "--tag", "job:db", "--stdin-name", "db.sql", "--relay", socket)
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	snapshots := fakerestic.Snapshots(t, root, env["RESTIC_REPOSITORY"])
	if last := snapshots[len(snapshots)-1]; last.ID != result.SnapshotID || last.Files["/db.sql"] != "CREATE TABLE t;\n" || fakerestic.Unlocks(t, root) != 3 {
		t.Errorf("the dump's snapshot: %+v", last)
	}
}

// TestARetentionLockedOutWaitsForTheNextRun (review of #29): forget needs
// the repository's exclusive lock, which any backup to it from another
// server keeps out; it waits 5 minutes, runs restic unlock and tries once
// more, and then leaves the snapshots to the next run, saying who held the
// lock, as a retention deferred and not failed.
func TestARetentionLockedOutWaitsForTheNextRun(t *testing.T) {
	env, root := backupRepository(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a": "1"})
	args := []string{"--host", "nas", "--tag", "pocketcloud", "--tag", "job:j1", "--path", dir, "--keep-last", "1"}
	if _, _, err := runBackup(t, env, nil, args...); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FAKE_RESTIC_LOCKED_FORGET", "1")
	result, log, err := runBackup(t, env, nil, args...)
	if err != nil || result.Forgotten != 1 || result.RetentionDeferred != "" || result.RetentionError != "" {
		t.Errorf("a lock that went: %+v %v\n%s", result, err, log)
	}
	if commands := resticCommands(t, root); !slices.Equal(commands[len(commands)-4:], []string{"backup", "forget", "unlock", "forget"}) {
		t.Errorf("restic ran %q", commands)
	}

	t.Setenv("FAKE_RESTIC_LOCKED_FORGET", "3")
	result, log, err = runBackup(t, env, nil, args...)
	locked := "unable to create lock in backend: repository is already locked by PID 1 on other-server by root (UID 0, GID 0)"
	if err != nil || result.SnapshotID == "" || result.Forgotten != 0 || result.RetentionDeferred != locked || result.RetentionError != "" {
		t.Errorf("a retention locked out: %+v %v", result, err)
	}
	if !strings.Contains(log, "The retention waits for the next run, as another restic uses the repository: "+locked+"\n") || strings.Contains(log, "The retention failed") {
		t.Errorf("the log:\n%s", log)
	}
	if commands := resticCommands(t, root); !slices.Equal(commands[len(commands)-4:], []string{"backup", "forget", "unlock", "forget"}) || fakerestic.Unlocks(t, root) != 2 {
		t.Errorf("restic ran %q", commands)
	}
	if snapshots := fakerestic.Snapshots(t, root, env["RESTIC_REPOSITORY"]); len(snapshots) != 2 {
		t.Errorf("%d snapshots", len(snapshots))
	}
	// the backup itself waits as long as before
	if calls := fakerestic.Calls(t, root); !strings.Contains(calls, " backup --json --quiet --retry-lock 30m ") || !strings.Contains(calls, " forget --json --prune --retry-lock 5m ") {
		t.Errorf("restic ran:\n%s", calls)
	}
}

// stubRestic puts a restic on PATH that runs until it is stopped: on SIGTERM
// it writes "terminated" to the file signal of dir and ends, as restic
// removes its locks; with ignore, it notes the signal and runs on.
func stubRestic(t *testing.T, ignore bool) string {
	t.Helper()
	dir := t.TempDir()
	onTerm := `echo terminated > "$STUB_DIR/signal"; exit 130`
	if ignore {
		onTerm = `echo ignored > "$STUB_DIR/signal"`
	}
	script := "#!/bin/sh\ntrap '" + onTerm + "' TERM\necho running > \"$STUB_DIR/running\"\nwhile :; do sleep 0.02; done\n"
	if err := os.WriteFile(filepath.Join(dir, "restic"), []byte(script), 0o755); err != nil { //nolint:gosec // a script
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STUB_DIR", dir)
	return dir
}

// stopRestic runs the stub restic, stops it once it runs, and says what it
// got and how long it took to end.
func stopRestic(t *testing.T, dir string) (string, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan error, 1)
	go func() {
		_, _, _, err := restic{log: io.Discard}.run(ctx, "backup")
		ended <- err
	}()
	for start := time.Now(); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "running")); err == nil {
			break
		}
		if time.Since(start) > 10*time.Second {
			t.Fatal("the stub restic did not start")
		}
	}
	stopped := time.Now()
	cancel()
	select {
	case err := <-ended:
		if err == nil {
			t.Error("a stopped restic said nothing")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stopped restic did not end")
	}
	took := time.Since(stopped)
	signal, _ := os.ReadFile(filepath.Join(dir, "signal")) //nolint:gosec // the test's own
	return strings.TrimSpace(string(signal)), took
}

// TestAStoppedResticRemovesItsLocks (review of #29): a job that is stopped
// stops restic with SIGTERM, on which restic removes its locks, not with
// SIGKILL, which leaves them to keep every other restic of the repository
// waiting; a restic that does not end in time is killed.
func TestAStoppedResticRemovesItsLocks(t *testing.T) {
	if signal, _ := stopRestic(t, stubRestic(t, false)); signal != "terminated" {
		t.Errorf("restic got %q", signal)
	}

	wait := resticStopWait
	t.Cleanup(func() { resticStopWait = wait })
	resticStopWait = 300 * time.Millisecond
	if signal, took := stopRestic(t, stubRestic(t, true)); signal != "ignored" || took < resticStopWait || took > 5*time.Second {
		t.Errorf("a restic that runs on: %q, ended after %v", signal, took)
	}
}

// TestTheBackupOpChecksItsFlags (#29).
func TestTheBackupOpChecksItsFlags(t *testing.T) {
	env, _ := backupRepository(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--tag", "a", "--path", "/srv"}, "needs --host"},
		{[]string{"--host", "nas"}, "of the directories of --path or of a dump"},
		{[]string{"--host", "nas", "--path", "/srv", "--stdin-name", "db.sql", "--relay", "/tmp/s"}, "and not both"},
		{[]string{"--host", "nas", "--path", "srv"}, "absolute and clean"},
		{[]string{"--host", "nas", "--path", "/srv/../etc"}, "absolute and clean"},
		{[]string{"--host", "nas", "--tag", "a,b", "--path", "/srv"}, `a tag cannot be "a,b"`},
		{[]string{"--host", "nas", "--stdin-name", "db.sql"}, "needs the relay's socket"},
		{[]string{"--host", "nas", "--stdin-name", "../db.sql", "--relay", "/tmp/s"}, "file name cannot be"},
		{[]string{"--host", "nas", "--path", "/srv", "--keep-last", "3"}, "without --tag it would take every other's"},
		{[]string{"--host", "nas", "--tag", "a", "--path", "/srv", "--keep-daily", "-1"}, "not -1"},
	} {
		if _, _, err := runBackup(t, env, nil, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v", tc.args, err)
		}
	}
}

// dumpFrames is a dump's stream as the agent sends it.
func dumpFrames(t *testing.T, data string, end *backup.DumpEnd) *bytes.Buffer {
	t.Helper()
	var stream bytes.Buffer
	frames := backup.NewFrameWriter(&stream)
	if _, err := frames.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	if end != nil {
		if err := frames.End(end.Code, end.Message); err != nil {
			t.Fatal(err)
		}
	}
	return &stream
}

// TestADumpReachesResticThroughTheRelay (#29): restic runs the relay, this
// binary, which reads the frames the tool serves from its own input on the
// socket; a dump that failed, or broke off, saves no snapshot and says why.
func TestADumpReachesResticThroughTheRelay(t *testing.T) {
	env, root := backupRepository(t)
	socket := filepath.Join(t.TempDir(), "dump.sock")
	args := []string{"--host", "nas", "--tag", "pocketcloud", "--tag", "job:db", "--stdin-name", "app-db.pgdump", "--relay", socket}
	dump := strings.Repeat("COPY public.posts FROM stdin;\n", 5000)

	result, log, err := runBackup(t, env, dumpFrames(t, dump, &backup.DumpEnd{Message: "pg_dump: warning: a note\n"}), args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if result.FilesNew != 1 || result.TotalBytes != int64(len(dump)) {
		t.Errorf("the result: %+v", result)
	}
	snapshots := fakerestic.Snapshots(t, root, env["RESTIC_REPOSITORY"])
	if len(snapshots) != 1 || snapshots[0].Files["/app-db.pgdump"] != dump || !slices.Equal(snapshots[0].Paths, []string{"/app-db.pgdump"}) {
		t.Fatalf("snapshots: %d", len(snapshots))
	}
	self, _ := os.Executable()
	calls := fakerestic.Calls(t, root)
	if !strings.Contains(calls, "backup --json --quiet --retry-lock 30m --host nas --tag pocketcloud,job:db --stdin-from-command --stdin-filename app-db.pgdump -- "+self+" backup-tool relay --socket "+socket+"\n") {
		t.Errorf("restic ran:\n%s", calls)
	}
	// what the dump said reaches the log, as restic passes it on
	if !strings.Contains(log, "subprocess "+self+": pg_dump: warning: a note") {
		t.Errorf("the log:\n%s", log)
	}
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket is left: %v", err)
	}

	_, _, err = runBackup(t, env, dumpFrames(t, "COPY", &backup.DumpEnd{Code: 1, Message: "pg_dump: error: connection to server failed\nFATAL: password authentication failed for user \"pw-jobs\"\n"}), args...)
	if err == nil || err.Error() != `the dump failed with exit code 1, and restic saved no snapshot: pg_dump: error: connection to server failed FATAL: password authentication failed for user "***"` {
		t.Errorf("a dump that failed: %v", err)
	}
	_, _, err = runBackup(t, env, dumpFrames(t, "COPY", nil), args...)
	if err == nil || !strings.HasPrefix(err.Error(), "the dump broke off before it said how it went, and restic saved no snapshot") {
		t.Errorf("a dump that broke off: %v", err)
	}
	if snapshots := fakerestic.Snapshots(t, root, env["RESTIC_REPOSITORY"]); len(snapshots) != 1 {
		t.Errorf("%d snapshots", len(snapshots))
	}

	// restic that never runs the relay says why itself
	env["RESTIC_PASSWORD"] = "another"
	if _, _, err := runBackup(t, env, dumpFrames(t, "COPY", &backup.DumpEnd{}), args...); err == nil || !strings.Contains(err.Error(), "the password is not its own") {
		t.Errorf("a wrong password: %v", err)
	}
}

// TestTheRelayOpEndsWithTheDumpsCode (#29): the op restic runs writes the
// dump to its output and ends with the dump's exit code.
func TestTheRelayOpEndsWithTheDumpsCode(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "dump.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, dumpFrames(t, "-- SQLite dump\n", &backup.DumpEnd{Code: 127, Message: `exec: "sqlite3": executable file not found in $PATH`}))
	}()
	var stdout, stderr bytes.Buffer
	err = BackupTool(context.Background(), []string{"relay", "--socket", socket}, nil, nil, &stdout, &stderr)
	var exit *ToolExit
	if !errors.As(err, &exit) || exit.Code != 127 || stdout.String() != "-- SQLite dump\n" || !strings.Contains(stderr.String(), `"sqlite3": executable file not found`) {
		t.Errorf("%v %q %q", err, stdout.String(), stderr.String())
	}
	if err := BackupTool(context.Background(), []string{"relay"}, nil, nil, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "--socket") {
		t.Errorf("without a socket: %v", err)
	}
}

func TestSizesReadAsPeopleReadThem(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 5 << 30: "5.0 GiB"} {
		if got := sizeText(n); got != want {
			t.Errorf("%d: %s, want %s", n, got, want)
		}
	}
}
