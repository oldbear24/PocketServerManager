//go:build windows

package panel

import "os"

// tryLock does not lock on Windows, where the panel is not released: the
// serve lock guards a panel's pb_data on Linux.
func tryLock(*os.File) (bool, error) { return true, nil }
