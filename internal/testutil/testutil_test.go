package testutil

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOnlyOldMigratedDataDirsAreRemoved(t *testing.T) {
	tmp := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	create := func(name string, dir bool, at time.Time) string {
		path := filepath.Join(tmp, name)
		if dir {
			if err := os.MkdirAll(filepath.Join(path, "pb_data"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "pb_data", "data.db"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}
	gone := create(migratedPrefix+"1", true, old)
	young := create(migratedPrefix+"2", true, time.Now())
	other := create("pocketcloud-other-3", true, old)
	file := create(migratedPrefix+"4", false, old)

	removeOldMigrated(tmp, time.Now().Add(-migratedKept))

	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Errorf("an old migrated data dir stayed: %v", err)
	}
	for _, kept := range []string{young, other, file} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s was removed: %v", filepath.Base(kept), err)
		}
	}
}
