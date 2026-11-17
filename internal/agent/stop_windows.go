package agent

import "os"

// gracefulStop asks a process to stop, and to clean up first, where it has
// no SIGTERM.
var gracefulStop = os.Interrupt
