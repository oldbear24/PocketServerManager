package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
