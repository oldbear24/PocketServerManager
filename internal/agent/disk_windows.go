//go:build windows

package agent

import "errors"

// diskUsage is unavailable on Windows: the agent runs on Linux, and this build
// only exists for developing the panel on a Windows machine.
func diskUsage(string) (total, free uint64, err error) {
	return 0, 0, errors.New("not supported on Windows")
}
