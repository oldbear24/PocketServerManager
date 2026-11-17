package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/recoverykit"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
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

// TestThePanelsRecoveryCommandsReachTheAgent (#32): the binary runs
// panel-recovery with the kit's passphrase from the environment, as docker
// exec -e gives it, asks no terminal it does not have, and runs repoint;
// each refusal is said and ends with exit code 1.
func TestThePanelsRecoveryCommandsReachTheAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the agent")
	}
	binary := filepath.Join(t.TempDir(), "pocketcloud-agent")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	master := bytes.Repeat([]byte{7}, secrets.KeySize)
	header := recoverykit.Header{Kit: "KIT-7Q2M-9XHC", Created: time.Date(2026, 11, 17, 10, 2, 0, 0, time.UTC), Panel: "http://192.0.2.2:8090",
		KeyID: secrets.KeyID(master), CAPin: "sha256:" + strings.Repeat("5d", 32), PanelVersion: "0.32.0", AgentImage: "agent"}
	text, err := recoverykit.Seal(header, recoverykit.Payload{
		CreatedBy: "admin@example.com", MasterKey: hex.EncodeToString(master), KeyID: secrets.KeyID(master), Env: map[string]string{},
		AgentsDial: []recoverykit.Dial{}, Job: &recoverykit.Job{ID: "p4n3lj0b0000001", Name: "panel (pb_data)"},
		Targets: []recoverykit.Target{{ID: "t1", Name: "panel-rest", Kind: "rest", Repository: recoverykit.Repository{URL: "rest:http://192.0.2.2:8000/panel32/", Password: "rest-repo-password"}}},
	}, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	kit := filepath.Join(t.TempDir(), "kit.pckit")
	if err := os.WriteFile(kit, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	run := func(env []string, args ...string) (string, string, int) {
		t.Helper()
		command := exec.Command(binary, args...)
		command.Env = append(os.Environ(), append([]string{"STATE_DIR=" + state}, env...)...)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatal(err)
		}
		return stdout.String(), stderr.String(), command.ProcessState.ExitCode()
	}
	stdout, stderr, code := run([]string{"POCKETCLOUD_KIT_PASSPHRASE=correct horse battery staple"}, "panel-recovery", "targets", "--kit", kit)
	if code != 0 || stdout != "The panel's backup job panel (pb_data) (p4n3lj0b0000001) backs up to:\n* panel-rest (rest): rest:http://192.0.2.2:8000/panel32/\n" ||
		!strings.HasPrefix(stderr, "Kit KIT-7Q2M-9XHC of http://192.0.2.2:8090 (key ") {
		t.Errorf("targets: %d %q %q", code, stdout, stderr)
	}
	if _, stderr, code := run(nil, "panel-recovery", "targets", "--kit", kit); code != 1 ||
		stderr != "error: the kit's passphrase comes from --passphrase-file, POCKETCLOUD_KIT_PASSPHRASE or a terminal, and none is there\n" {
		t.Errorf("no passphrase: %d %q", code, stderr)
	}
	if _, stderr, code := run([]string{"POCKETCLOUD_KIT_PASSPHRASE=another passphrase"}, "panel-recovery", "targets", "--kit", kit); code != 1 ||
		stderr != "error: the passphrase does not open this kit, or the kit was changed\n" {
		t.Errorf("a wrong passphrase: %d %q", code, stderr)
	}
	if _, stderr, code := run(nil, "repoint", "--panel", "panel.example.com:9443"); code != 1 || !strings.Contains(stderr, "has not enrolled") {
		t.Errorf("repoint: %d %q", code, stderr)
	}
	if entries, _ := os.ReadDir(state); len(entries) != 0 {
		t.Errorf("the state directory holds %d entries", len(entries))
	}
}
