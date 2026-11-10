// Package resticcapture reads what restic 0.17.3 printed, as captured from the
// binary into internal/agent/testdata/restic-0.17.3 for #31: the checks, the
// locks and unlock. The parsers of the agent and the panel are held to these
// lines, and the fake restic prints lines of the same shape.
//
// A capture file starts with lines of description, each starting with "# ".
// Then come one or more runs, each as
//
//	$ restic ARGS
//	--- stdout
//	...
//	--- stderr
//	...
//	--- exit N
package resticcapture

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Run is one run of restic in a capture.
type Run struct {
	// Args are restic's arguments, as one line.
	Args string
	// Stdout and Stderr are what restic printed, each line ending in a
	// newline; empty when it printed nothing.
	Stdout string
	Stderr string
	// Exit is restic's exit code.
	Exit int
}

// Path is the path of the capture file name.
func Path(name string) string {
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "..", "..", "agent", "testdata", "restic-0.17.3", name)
}

// Read reads the runs of the capture file name.
func Read(name string) ([]Run, error) {
	raw, err := os.ReadFile(Path(name))
	if err != nil {
		return nil, err
	}
	runs, err := Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return runs, nil
}

// Parse reads the runs of a capture.
func Parse(text string) ([]Run, error) {
	var runs []Run
	var run *Run
	var into *strings.Builder
	var stdout, stderr strings.Builder
	end := func() {
		run.Stdout, run.Stderr = stdout.String(), stderr.String()
		runs = append(runs, *run)
		run, into = nil, nil
		stdout.Reset()
		stderr.Reset()
	}
	for n, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		switch {
		case run == nil && strings.HasPrefix(line, "# ") && len(runs) == 0:
		case strings.HasPrefix(line, "$ restic"):
			if run != nil {
				return nil, fmt.Errorf("line %d: a run without its exit code", n+1)
			}
			run = &Run{Args: strings.TrimSpace(strings.TrimPrefix(line, "$ restic"))}
		case run == nil:
			return nil, fmt.Errorf("line %d: %q is not in a run", n+1, line)
		case line == "--- stdout" && into == nil:
			into = &stdout
		case line == "--- stderr" && into == &stdout:
			into = &stderr
		case strings.HasPrefix(line, "--- exit ") && into == &stderr:
			code, err := strconv.Atoi(strings.TrimPrefix(line, "--- exit "))
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", n+1, err)
			}
			run.Exit = code
			end()
		case into == nil:
			return nil, fmt.Errorf("line %d: %q comes before --- stdout", n+1, line)
		default:
			into.WriteString(line + "\n")
		}
	}
	if run != nil {
		return nil, fmt.Errorf("the last run has no exit code")
	}
	if len(runs) == 0 {
		return nil, fmt.Errorf("no run")
	}
	return runs, nil
}
