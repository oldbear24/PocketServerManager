//go:build !windows

package agent

import "syscall"

// diskUsage returns the size of the file system at path, the space in use and
// the space still available to ordinary users: df's Size, Used and Avail. The
// blocks reserved for root are neither used nor available to others; Docker,
// which runs as root, can still fill them, so they do not count as used.
func diskUsage(path string) (total, used, available uint64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, 0, err
	}

	size := uint64(stat.Bsize)                                                    //nolint:unconvert,gosec // the type differs per platform
	blocks, free := uint64(stat.Blocks), uint64(stat.Bfree)                       //nolint:unconvert // the types differ per platform
	return blocks * size, (blocks - free) * size, uint64(stat.Bavail) * size, nil //nolint:unconvert // the type differs per platform
}
