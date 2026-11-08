package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
