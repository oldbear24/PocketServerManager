package panel

import (
	"strings"
	"testing"

	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// TestRestoreAlertsGoWithTheirRestores (#30): the alert of a restore's data
// goes once no restore of that data is left that failed, as when its job or
// its target is removed with their restores; one that another failed
// restore of the same data still stands for stays. A key that would not
// fit an alert names the data by its hash.
func TestRestoreAlertsGoWithTheirRestores(t *testing.T) {
	f := newJobFixture(t)
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	other := f.job("other", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	first := f.restore(job, map[string]any{"status": schema.RestoreFailed, "command": ""})
	second := f.restore(other, map[string]any{"status": schema.RestoreFailed})
	key := restoreAlertKey(first)
	if key != f.host.Id+":volume:app_data" || restoreAlertKey(second) != key {
		t.Fatalf("the keys: %s %s", key, restoreAlertKey(second))
	}
	if err := events.RaiseAlert(f.app, alertBackupRestore, key, f.host.Id, schema.SeverityCritical, "it failed"); err != nil {
		t.Fatal(err)
	}
	open := func() int {
		t.Helper()
		alerts, err := f.app.FindRecordsByFilter(schema.Alerts, "rule='backup_restore' && status!='resolved'", "", 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(alerts)
	}
	if err := f.app.Delete(job); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.FindRecordById(schema.BackupRestores, first.Id); err == nil {
		t.Fatal("the job's restore stayed")
	}
	if open() != 1 {
		t.Error("the alert went while another failed restore of the data is left")
	}
	if err := f.app.Delete(f.target); err != nil {
		t.Fatal(err)
	}
	if open() != 0 {
		t.Error("the alert stayed once its restores went with the target")
	}
}

// TestALongRestoreAlertKeyIsHashed (#30): a folder too long for an alert's
// key is named by its hash.
func TestALongRestoreAlertKeyIsHashed(t *testing.T) {
	f := newJobFixture(t)
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	long := f.restore(job, map[string]any{"mode": schema.RestorePath, "roots": nil, "path": "/srv/" + strings.Repeat("x", 300)})
	if key := restoreAlertKey(long); len(key) > 255 || !strings.HasPrefix(key, f.host.Id+":sha256:") {
		t.Errorf("a long key: %s", key)
	}
}
