package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

func runBackupTool(t *testing.T, env map[string]string, args ...string) (backupInitResult, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := BackupTool(context.Background(), args, env, &stdout, &stderr)
	var result backupInitResult
	if err == nil {
		if decodeErr := json.Unmarshal(lastLine(stdout.Bytes()), &result); decodeErr != nil {
			t.Fatalf("the result %q: %v", stdout.String(), decodeErr)
		}
	}
	return result, stderr.String(), err
}

// TestTheBackupToolOpensWhatExistsAndCreatesWhatDoesNot (#28): restic's exit
// codes tell a missing repository, which init creates, from a wrong password,
// which it never papers over with a second repository.
func TestTheBackupToolOpensWhatExistsAndCreatesWhatDoesNot(t *testing.T) {
	root := fakerestic.Install(t)
	env := map[string]string{"RESTIC_REPOSITORY": "rest:http://backup-node:8000/homelab/", "RESTIC_PASSWORD": "first-password",
		"RESTIC_REST_USERNAME": "homelab", "RESTIC_REST_PASSWORD": "rest-password"}
	created, _, err := runBackupTool(t, env, "init")
	if err != nil || !created.Created || created.ID == "" {
		t.Fatalf("the first init: %+v %v", created, err)
	}
	opened, _, err := runBackupTool(t, env, "init")
	if err != nil || opened.Created || opened.ID != created.ID {
		t.Fatalf("the second init: %+v %v", opened, err)
	}

	// a target that has a repository only opens it
	if again, _, err := runBackupTool(t, env, "init", "--expect", created.ID); err != nil || again.Created {
		t.Errorf("an expected repository: %+v %v", again, err)
	}
	if _, _, err := runBackupTool(t, env, "init", "--expect", strings.Repeat("f", 64)); err == nil || !strings.Contains(err.Error(), "another repository (") {
		t.Errorf("another repository: %v", err)
	}
	gone := map[string]string{"RESTIC_REPOSITORY": "rest:http://backup-node:8000/gone/", "RESTIC_PASSWORD": "first-password"}
	if _, _, err := runBackupTool(t, gone, "init", "--expect", created.ID); err == nil || !strings.Contains(err.Error(), "the repository is gone") {
		t.Errorf("a repository that is gone: %v", err)
	}
	if fakerestic.Repositories(t, root) != 1 {
		t.Error("a repository that was gone was made again")
	}

	env["RESTIC_PASSWORD"] = "another-password"
	_, _, err = runBackupTool(t, env, "init")
	if err == nil || !strings.Contains(err.Error(), "the password is not its own") || strings.Contains(err.Error(), "another-password") {
		t.Errorf("a wrong password: %v", err)
	}
	if calls := fakerestic.Calls(t, root); strings.Count(calls, " init\n") != 1 || !strings.Contains(calls, "RESTIC_REST_PASSWORD=rest-password") {
		t.Errorf("restic ran:\n%s", calls)
	}

	if _, _, err := runBackupTool(t, map[string]string{"RESTIC_REPOSITORY": "/x"}, "init"); err == nil || !strings.Contains(err.Error(), "no password") {
		t.Errorf("without a password: %v", err)
	}
	if _, _, err := runBackupTool(t, env, "frobnicate"); err == nil || !strings.Contains(err.Error(), "unknown op") {
		t.Errorf("an unknown op: %v", err)
	}
	if _, _, err := runBackupTool(t, env, "init", "-o", "novalue"); err == nil {
		t.Error("an option without a value was taken")
	}
}

// TestTheBackupToolTellsResticsErrorWithoutSecrets (#28): the error is
// restic's from its Fatal line on, on one line, and neither it nor the log
// holds a credential restic repeated.
func TestTheBackupToolTellsResticsErrorWithoutSecrets(t *testing.T) {
	fakerestic.Install(t)
	secret := "key-" + fakerestic.Wrong + "-0123456789"
	_, log, err := runBackupTool(t, map[string]string{"RESTIC_REPOSITORY": "s3:https://s3.example.com/bucket", "RESTIC_PASSWORD": "pw-12345",
		"AWS_ACCESS_KEY_ID": "id-12345", "AWS_SECRET_ACCESS_KEY": secret}, "init")
	if err == nil {
		t.Fatal("refused credentials were taken")
	}
	want := "Fatal: unable to open config file: Stat: Access Denied for key *** Is there a repository at the following location? s3:https://s3.example.com/bucket"
	if err.Error() != want {
		t.Errorf("the error:\n%s\nwant\n%s", err, want)
	}
	if strings.Contains(log, secret) || !strings.Contains(log, "Access Denied for key ***\n") {
		t.Errorf("the log:\n%s", log)
	}
}

// TestALocalRepositoryGoesIntoAnEmptyDirectoryOnly (#28): a directory that
// holds other files and no repository is refused, as restic would scatter its
// own among them; a new one is made the owner's alone.
func TestALocalRepositoryGoesIntoAnEmptyDirectoryOnly(t *testing.T) {
	fakerestic.Install(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "photo.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"RESTIC_REPOSITORY": dir, "RESTIC_PASSWORD": "pw-local"}
	if _, _, err := runBackupTool(t, env, "init", "--local", dir); err == nil || !strings.Contains(err.Error(), "holds other files") {
		t.Errorf("a directory with files: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config")); err == nil {
		t.Error("a repository was made among the files")
	}

	empty := filepath.Join(t.TempDir(), "restic")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_RESTIC_SIZE", "1000")
	env["RESTIC_REPOSITORY"] = empty
	result, _, err := runBackupTool(t, env, "init", "--local", empty)
	if err != nil || !result.Created || result.Used != 1000 || result.Capacity <= 1000 {
		t.Fatalf("an empty directory: %+v %v", result, err)
	}
	if info, err := os.Stat(empty); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the directory: %v %v", info.Mode(), err)
	}
	// its own repository is no other file
	if again, _, err := runBackupTool(t, env, "init", "--local", empty); err != nil || again.Created {
		t.Errorf("again: %+v %v", again, err)
	}
}

func TestBackupToolEnvTakesResticsVariablesOnly(t *testing.T) {
	env := BackupToolEnv([]string{"PATH=/bin", "RESTIC_PASSWORD=a=b", "AWS_SECRET_ACCESS_KEY=s", "HOME=/root", "GIT_TOKEN=x"})
	if len(env) != 2 || env["RESTIC_PASSWORD"] != "a=b" || env["AWS_SECRET_ACCESS_KEY"] != "s" {
		t.Errorf("env: %v", env)
	}
}
