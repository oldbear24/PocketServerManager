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
	if !strings.Contains(calls, "restic --no-cache forget --json --prune --retry-lock 30m --group-by tags --tag pocketcloud,job:j1 --keep-last 2 --keep-daily 1\n") {
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
