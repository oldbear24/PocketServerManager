//go:build windows

package agent

import "errors"

// The agent runs on Linux; this build only exists for developing on a
// Windows machine, where a restore's folders cannot be told apart by inode.
var errNoInodes = errors.New("folders have no inodes on this system")

func dirID(string) (fileID, error) { return fileID{}, errNoInodes }

func dirIDNoLinks(string, string) (fileID, error) { return fileID{}, errNoInodes }
