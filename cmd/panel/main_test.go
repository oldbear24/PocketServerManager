package main

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain runs the panel's main when a test runs this binary as the panel.
func TestMain(m *testing.M) {
	if os.Getenv("POCKETCLOUD_TEST_RUN_PANEL") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runPanel runs this binary as `panel` with args, and returns its exit code and
// what it wrote.
func runPanel(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...) //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), "POCKETCLOUD_TEST_RUN_PANEL=1", "POCKETCLOUD_KIT_PASSPHRASE=", "POCKETCLOUD_DATA_DIR=", "POCKETCLOUD_MASTER_KEY_FILE=")
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}

// TestRecoverRunsBeforePocketBase (#32): `panel recover` is caught before
// PocketBase, whose every command bootstraps pb_data and would make a
// database where the backup is to go. A refusal exits 1 and writes nothing;
// the command given after PocketBase's flags is unknown to PocketBase, which
// bootstraps nothing for it either.
func TestRecoverRunsBeforePocketBase(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "pb_data")
	passphrase := filepath.Join(work, "pass")
	if err := os.WriteFile(passphrase, []byte("a passphrase of the kit\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out := runPanel(t, "recover", "--kit", filepath.Join(work, "missing.pckit"), "--from", filepath.Join(work, "missing.pcpb"),
		"--dir", dir, "--master-key-file", filepath.Join(work, "master.key"), "--passphrase-file", passphrase)
	if code != 1 || !strings.Contains(out, "error: the recovery kit: open ") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("pb_data was made: %v", err)
	}
	if left, _ := os.ReadDir(work); len(left) != 1 {
		t.Fatalf("recover wrote %d files", len(left)-1)
	}

	code, out = runPanel(t, "recover", "--help")
	if code != 0 || !strings.Contains(out, "panel recover --kit FILE --from FILE.pcpb [flags]") || !strings.Contains(out, "--replace") {
		t.Fatalf("the help: exit %d: %s", code, out)
	}

	code, out = runPanel(t, "--dir", dir, "recover")
	if code != 1 || !strings.Contains(out, `unknown command "recover"`) {
		t.Fatalf("recover after a flag: exit %d: %s", code, out)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("PocketBase made pb_data: %v", err)
	}
}
