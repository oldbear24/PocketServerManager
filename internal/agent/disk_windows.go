//go:build windows

package agent

import "golang.org/x/sys/windows"

// diskUsage returns the size of the volume at path, the space in use and the
// space available to this user. The agent runs on Linux; this build only exists
// for developing on a Windows machine.
func diskUsage(path string) (total, used, available uint64, err error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, 0, err
	}

	var free, size, allFree uint64
	if err := windows.GetDiskFreeSpaceEx(name, &free, &size, &allFree); err != nil {
		return 0, 0, 0, err
	}

	return size, size - allFree, free, nil
}
