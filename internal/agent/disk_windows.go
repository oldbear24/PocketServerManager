//go:build windows

package agent

import "golang.org/x/sys/windows"

// diskUsage returns the size and the free space of the volume at path. The agent
// runs on Linux; this build only exists for developing on a Windows machine.
func diskUsage(path string) (total, free uint64, err error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}

	var available, size, allFree uint64
	if err := windows.GetDiskFreeSpaceEx(name, &available, &size, &allFree); err != nil {
		return 0, 0, err
	}

	return size, available, nil
}
