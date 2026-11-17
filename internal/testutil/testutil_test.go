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

// TestAnAppFromADataDirStartsOnACopyOfIt (#32): a restored pb_data, the
// hidden files next to its database included, is copied, and the original
// is left as it was.
func TestAnAppFromADataDirStartsOnACopyOfIt(t *testing.T) {
	first := NewApp(t)
	defer first.Cleanup()
	if _, err := first.DB().NewQuery("CREATE TABLE kept (word TEXT); INSERT INTO kept VALUES ('restored')").Execute(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := first.DB().NewQuery("VACUUM INTO {:path}").Bind(map[string]any{"path": filepath.Join(dir, "data.db")}).Execute(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".pc_restored.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	app := NewAppFrom(t, dir)
	defer app.Cleanup()
	if app.DataDir() == dir {
		t.Fatal("the app runs on the original")
	}
	var word string
	if err := app.DB().NewQuery("SELECT word FROM kept").Row(&word); err != nil || word != "restored" {
		t.Fatalf("the copy holds %q: %v", word, err)
	}
	if _, err := os.Stat(filepath.Join(app.DataDir(), ".pc_restored.json")); err != nil {
		t.Errorf("the hidden file was not copied: %v", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 2 {
		t.Errorf("the original changed: %v, %v", entries, err)
	}
}
