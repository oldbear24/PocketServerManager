package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

// TestTheSiteToolBinaryGetsWhatTheJobIsGiven: the agent runs the site tool in
// a job container of its own image, and passes it what a deployment needs in
// the job's environment: a template's files and a release's .env (#52), and a
// Git token. The binary must hand all of it to the tool, as tests that run
// the tool in-process cannot tell.
func TestTheSiteToolBinaryGetsWhatTheJobIsGiven(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the agent")
	}
	binary := filepath.Join(t.TempDir(), "pocketcloud-agent")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	root := t.TempDir()
	run := func(env []string, args ...string) {
		t.Helper()
		command := exec.Command(binary, append(append([]string{"site-tool"}, args...), "--root", root)...)
		command.Env = append(os.Environ(), env...)
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("site-tool %v: %v: %s", args, err, out)
		}
	}

	run([]string{`POCKETCLOUD_FILES={"wp-config.php":"<?php // template"}`},
		"source", "--site", "blog", "--n", "1", "--kind", "empty", "--php", "--image", "php", "--command", "true")
	if got, _ := os.ReadFile(filepath.Join(root, "blog", ".build", "1", "src", "wp-config.php")); string(got) != "<?php // template" {
		t.Errorf("the template's file: %q", got)
	}
	run([]string{"POCKETCLOUD_ENV_FILE=APP_KEY=\"base64:x\"\n"},
		"release", "--site", "blog", "--n", "1", "--php", "--no-switch", "--env-file", ".env")
	if got, _ := os.ReadFile(filepath.Join(root, "blog", "releases", "1", ".env")); string(got) != "APP_KEY=\"base64:x\"\n" {
		t.Errorf(".env: %q", got)
	}
}

// TestTheBackupToolBinaryGetsWhatTheJobIsGiven (#28): the panel's secrets
// reach restic through the job's environment, which the binary hands to the
// backup tool, and its result is the last line of its output.
func TestTheBackupToolBinaryGetsWhatTheJobIsGiven(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the agent")
	}
	root := fakerestic.Install(t)
	binary := filepath.Join(t.TempDir(), "pocketcloud-agent")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	command := exec.Command(binary, "backup-tool", "init", "-o", "s3.bucket-lookup=path")
	command.Env = append(os.Environ(), "RESTIC_REPOSITORY=s3:http://minio:9000/b", "RESTIC_PASSWORD=pw-binary",
		"AWS_ACCESS_KEY_ID=id", "AWS_SECRET_ACCESS_KEY=secret-binary")
	out, err := command.Output()
	if err != nil {
		t.Fatalf("backup-tool: %v: %s", err, out)
	}
	if !strings.HasPrefix(string(out), `{"created":true,"id":"`) {
		t.Errorf("the result: %s", out)
	}
	if calls := fakerestic.Calls(t, root); !strings.Contains(calls, "AWS_SECRET_ACCESS_KEY=secret-binary") ||
		!strings.Contains(calls, "RESTIC_PASSWORD=pw-binary") || !strings.Contains(calls, "-o s3.bucket-lookup=path") {
		t.Errorf("restic ran:\n%s", calls)
	}
}

// TestADumpGoesThroughTheBinarysRelay (#29): the backup tool names its own
// binary for restic's --stdin-from-command, and the binary's `backup-tool
// relay` hands restic the dump from the job's input and ends with the dump's
// own exit code.
func TestADumpGoesThroughTheBinarysRelay(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the agent")
	}
	root := fakerestic.Install(t)
	binary := filepath.Join(t.TempDir(), "pocketcloud-agent")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	repo := filepath.Join(t.TempDir(), "repo")
	env := append(os.Environ(), "RESTIC_REPOSITORY="+repo, "RESTIC_PASSWORD=pw-dump")
	frames := func(data string, code int, message string) *bytes.Buffer {
		var stream bytes.Buffer
		writer := backup.NewFrameWriter(&stream)
		_, _ = writer.Write([]byte(data))
		_ = writer.End(code, message)
		return &stream
	}
	run := func(stdin io.Reader, args ...string) (string, string, int) {
		t.Helper()
		command := exec.Command(binary, append([]string{"backup-tool"}, args...)...)
		command.Env = env
		command.Stdin = stdin
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatal(err)
		}
		return stdout.String(), stderr.String(), command.ProcessState.ExitCode()
	}
	if _, stderr, code := run(nil, "init"); code != 0 {
		t.Fatalf("init: %s", stderr)
	}
	socket := filepath.Join(t.TempDir(), "dump.sock")
	backupArgs := []string{"backup", "--host", "nas", "--tag", "pocketcloud", "--tag", "job:db", "--stdin-name", "db.sql", "--relay", socket}
	stdout, stderr, code := run(frames("CREATE TABLE posts;\n", 0, ""), backupArgs...)
	if code != 0 || !strings.Contains(stdout, `"snapshot_id":"`) {
		t.Fatalf("the backup: %d %s\n%s", code, stdout, stderr)
	}
	if snapshots := fakerestic.Snapshots(t, root, repo); len(snapshots) != 1 || snapshots[0].Files["/db.sql"] != "CREATE TABLE posts;\n" {
		t.Errorf("snapshots: %+v", snapshots)
	}
	if calls := fakerestic.Calls(t, root); !strings.Contains(calls, "-- "+binary+" backup-tool relay --socket "+socket+"\n") {
		t.Errorf("restic ran:\n%s", calls)
	}
	_, stderr, code = run(frames("CREATE", 2, "mysqldump: Got error: 1045: Access denied"), backupArgs...)
	if code != 1 || !strings.Contains(stderr, "error: the dump failed with exit code 2, and restic saved no snapshot: mysqldump: Got error: 1045: Access denied\n") {
		t.Errorf("a dump that failed: %d\n%s", code, stderr)
	}

	// the relay alone
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
		_, _ = io.Copy(conn, frames("PGDMP", 3, "pg_dump: error: query failed"))
	}()
	stdout, stderr, code = run(nil, "relay", "--socket", socket)
	if code != 3 || stdout != "PGDMP" || stderr != "pg_dump: error: query failed\n" {
		t.Errorf("the relay: %d %q %q", code, stdout, stderr)
	}
}
