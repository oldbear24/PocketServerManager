//go:build !windows

package agent

import (
	"os"
	"syscall"
)

// gracefulStop asks a process to stop, and to clean up first.
var gracefulStop os.Signal = syscall.SIGTERM
