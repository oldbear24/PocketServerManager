//go:build !windows

package agent

import (
	"syscall"
	"testing"
)

// TestDiskUsageMatchesDf counts what df counts: used is what is not free, so the
// blocks reserved for root are not in it.
func TestDiskUsageMatchesDf(t *testing.T) {
	dir := t.TempDir()

	total, used, available, err := diskUsage(dir)
	if err != nil {
		t.Fatal(err)
	}

	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		t.Fatal(err)
	}
	block := uint64(stat.Bsize) //nolint:unconvert,gosec // the type differs per platform

	// the file system may change between the two calls; a few blocks do not matter
	near := func(got, want uint64) bool {
		diff := int64(got) - int64(want)                         //nolint:gosec // disk sizes fit
		return diff < int64(64*block) && diff > -int64(64*block) //nolint:gosec // as above
	}

	if total != uint64(stat.Blocks)*block { //nolint:unconvert // the type differs per platform
		t.Errorf("total %d, want %d", total, uint64(stat.Blocks)*block) //nolint:unconvert // as above
	}
	if want := (uint64(stat.Blocks) - uint64(stat.Bfree)) * block; !near(used, want) { //nolint:unconvert // as above
		t.Errorf("used %d, want about %d", used, want)
	}
	if used+available > total {
		t.Errorf("used %d and available %d are more than the size %d", used, available, total)
	}
}
