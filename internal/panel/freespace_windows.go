//go:build windows

package panel

import "golang.org/x/sys/windows"

// freeSpace is the space at path available to this user. The panel runs on
// Linux; this build only exists for developing on a Windows machine.
func freeSpace(path string) (uint64, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free, size, allFree uint64
	if err := windows.GetDiskFreeSpaceEx(name, &free, &size, &allFree); err != nil {
		return 0, err
	}
	return free, nil
}
