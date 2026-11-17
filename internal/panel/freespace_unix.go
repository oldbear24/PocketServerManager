//go:build !windows

package panel

import "syscall"

// freeSpace is the space at path that the panel, which does not run as root
// in its image, may still fill: df's Avail.
func freeSpace(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil //nolint:unconvert,gosec // the types differ per platform
}
