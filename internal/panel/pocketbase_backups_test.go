package panel

import (
	"archive/zip"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// TestPocketBasesRestoreIsRefused (#32): PocketBase's own restore is refused
// by its route, before PocketBase looks at the backup, and by its hook,
// saying how the panel is restored; its other backup routes answer.
func TestPocketBasesRestoreIsRefused(t *testing.T) {
	_, app, handler := newTestPanel(t)
	_, superuser := testutil.NewSuperuser(t, app)
	if err := app.CreateBackup(context.Background(), "pb_backup_test.zip"); err != nil {
		t.Fatal(err)
	}
	// a backup that is not there first: PocketBase itself would answer 400,
	// and restore nothing; one that is there, PocketBase would restore in
	// the background, and restart
	for _, key := range []string{"nosuch.zip", "pb_backup_test.zip"} {
		status, out := testutil.Do(t, handler, testutil.Request{Method: http.MethodPost, URL: "/api/backups/" + key + "/restore", Token: superuser})
		answer := map[string]any{}
		_ = json.Unmarshal([]byte(out), &answer)
		if status != http.StatusForbidden || answer["message"] != "Restore the panel with `panel recover`, which checks the master key first: see docs/disaster-recovery.md." {
			t.Fatalf("the restore of %s: %d %s", key, status, out)
		}
	}
	// the hook PocketBase's restore triggers, without what it would do next
	event := &core.BackupEvent{Name: "pb_backup_test.zip"}
	event.App, event.Context = app, context.Background()
	err := app.OnBackupRestore().Trigger(event, func(*core.BackupEvent) error {
		t.Error("PocketBase's restore went on")
		return nil
	})
	if err == nil || err.Error() != pocketBaseRestoreRefused {
		t.Errorf("a restore by its hook: %v", err)
	}
	status, out := testutil.Do(t, handler, testutil.Request{Method: http.MethodGet, URL: "/api/backups", Token: superuser})
	if status != http.StatusOK || !strings.Contains(out, "pb_backup_test.zip") {
		t.Errorf("the list of backups: %d %s", status, out)
	}
}

// TestPocketBasesBackupLeavesOutThePanelsFiles (#32): a backup PocketBase
// makes goes on, with the database, but without the metrics, the spool of
// the panel's copies, its serve lock and its restore marker.
func TestPocketBasesBackupLeavesOutThePanelsFiles(t *testing.T) {
	_, app, _ := newTestPanel(t)
	dir := app.DataDir()
	if err := os.MkdirAll(filepath.Join(dir, exportSpool), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{exportSpool + "/x.pcpb", ".pc_serve.lock", ".pc_restored.json", "metrics.db-wal", "metrics.db-shm", "kept.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "metrics.db")); err != nil {
		t.Fatalf("the panel has no metrics: %v", err)
	}
	if err := app.CreateBackup(context.Background(), "pb_backup_test.zip"); err != nil {
		t.Fatal(err)
	}
	archive, err := zip.OpenReader(filepath.Join(dir, "backups", "pb_backup_test.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	names := []string{}
	for _, file := range archive.File {
		names = append(names, file.Name)
	}
	if !slices.Contains(names, "data.db") || !slices.Contains(names, "kept.txt") {
		t.Errorf("the backup holds %v", names)
	}
	for _, name := range names {
		if strings.HasPrefix(name, "metrics.db") || strings.HasPrefix(name, exportSpool) || strings.HasPrefix(name, ".pc_") {
			t.Errorf("the backup holds %s", name)
		}
	}
}
