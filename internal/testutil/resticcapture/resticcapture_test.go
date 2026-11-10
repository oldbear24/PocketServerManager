package resticcapture

import (
	"path/filepath"
	"strings"
	"testing"
)

// Captures lists the capture files of #31, which are in this package's format.
var captures = []string{
	"backup-locked.txt", "check-empty.txt", "check-index.txt",
	"check-invalid-lock.txt", "check-locked.txt", "check-missing.txt",
	"check-nolock-concurrent-prune.txt", "check-ok.txt", "check-packs.txt",
	"check-progress.txt", "check-readonly.txt", "check-repository.txt",
	"check-tree.txt", "invalid-lock.txt", "locks.txt", "unlock.txt",
}

func TestEveryCaptureReads(t *testing.T) {
	found, err := filepath.Glob(Path("check-*.txt"))
	if err != nil || len(found) == 0 {
		t.Fatalf("no check capture: %v", err)
	}
	for _, path := range found {
		name := filepath.Base(path)
		if !strings.Contains(strings.Join(captures, " "), name) {
			t.Errorf("%s is not listed", name)
		}
	}
	for _, name := range captures {
		runs, err := Read(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		for _, run := range runs {
			if run.Args == "" {
				t.Errorf("%s: a run without its arguments", name)
			}
			for _, text := range []string{run.Stdout, run.Stderr} {
				if strings.Contains(text, "scratchpad") || strings.Contains(text, "/tmp/claude") {
					t.Errorf("%s: a scratch path was left: %q", name, text)
				}
			}
		}
	}
}

func TestACaptureKeepsWhatEachStreamSaid(t *testing.T) {
	runs, err := Read("check-packs.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want both passes", len(runs))
	}
	free, confirm := runs[0], runs[1]
	if free.Exit != 1 || !strings.HasSuffix(free.Stderr, "Fatal: repository contains errors\n") ||
		!strings.Contains(free.Stderr, "\n\nThe repository contains damaged pack files.") {
		t.Errorf("pass 1 = %+v", free)
	}
	if !strings.HasPrefix(free.Args, "--cache-dir /tmp/pocketcloud-check-cache --cleanup-cache --no-lock check") ||
		strings.Contains(free.Stdout, "create exclusive lock") {
		t.Errorf("pass 1 = %+v", free)
	}
	if !strings.Contains(confirm.Stdout, "create exclusive lock for repository\n") || confirm.Exit != 1 {
		t.Errorf("pass 2 = %+v", confirm)
	}
	runs, err = Read("unlock.txt")
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].Stdout != "" || runs[0].Stderr != "" || runs[2].Stdout != "successfully removed 1 locks\n" {
		t.Errorf("unlock = %+v", runs)
	}
}

func TestAMalformedCaptureIsRefused(t *testing.T) {
	for name, text := range map[string]string{
		"no run":            "# only words\n",
		"no exit code":      "$ restic check\n--- stdout\nok\n--- stderr\n",
		"output before":     "$ restic check\nok\n--- stdout\n--- stderr\n--- exit 0\n",
		"words between":     "$ restic check\n--- stdout\n--- stderr\n--- exit 0\n# late\n",
		"exit not a number": "$ restic check\n--- stdout\n--- stderr\n--- exit one\n",
		"stderr first":      "$ restic check\n--- stderr\n--- stdout\n--- exit 0\n",
		"two runs at once":  "$ restic check\n--- stdout\n$ restic unlock\n--- stdout\n--- stderr\n--- exit 0\n",
	} {
		if _, err := Parse(text); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
