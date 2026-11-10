//go:build !linux && !windows

package agent

import "golang.org/x/sys/unix"

// lookupOnly opens a folder to look names up in it and to say its inode;
// these systems have no O_PATH, so it is read. The agent runs on Linux;
// this build only exists for developing on another machine.
const lookupOnly = unix.O_RDONLY
