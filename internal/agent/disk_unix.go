//go:build !windows

package agent

import "syscall"

// diskUsage returns the size and the free space of the file system at path.
func diskUsage(path string) (total, free uint64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}

	size := uint64(stat.Bsize)                                         //nolint:unconvert,gosec // the type differs per platform
	return uint64(stat.Blocks) * size, uint64(stat.Bavail) * size, nil //nolint:unconvert // the types differ per platform
}
