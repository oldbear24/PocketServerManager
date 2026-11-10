//go:build !windows

package agent

import (
	"strings"

	"golang.org/x/sys/unix"
)

// dirID is the device and inode of a folder, as the backup tool sees what
// its job mounts (#30).
func dirID(dir string) (fileID, error) {
	var stat unix.Stat_t
	if err := unix.Stat(dir, &stat); err != nil {
		return fileID{}, err
	}
	return fileID{Dev: uint64(stat.Dev), Ino: uint64(stat.Ino)}, nil //nolint:unconvert,gosec // the types differ per platform
}

// dirIDNoLinks is the device and inode of dir, a folder of the server with
// no symbolic link in it, as the agent sees it through root, where the
// server's / is: each name is opened in the folder before it without
// following a link, so that a link put in the place of any of them is an
// error rather than a way elsewhere, however the folders change meanwhile.
// A folder is only looked up (lookupOnly), not read.
func dirIDNoLinks(root, dir string) (fileID, error) {
	fd, err := unix.Open(root, lookupOnly|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fileID{}, err
	}
	for name := range strings.SplitSeq(strings.TrimPrefix(dir, "/"), "/") {
		if name == "" {
			continue
		}
		next, err := unix.Openat(fd, name, lookupOnly|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			return fileID{}, err
		}
		fd = next
	}
	defer unix.Close(fd) //nolint:errcheck // a folder only looked up
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fileID{}, err
	}
	return fileID{Dev: uint64(stat.Dev), Ino: uint64(stat.Ino)}, nil //nolint:unconvert,gosec // the types differ per platform
}
