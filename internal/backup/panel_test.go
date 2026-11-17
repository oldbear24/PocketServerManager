package backup

import (
	"strings"
	"testing"
)

func TestAPanelExportIDNamesOneRun(t *testing.T) {
	const run = "abc123def456ghi"
	id := PanelExportID(run)
	if id != "panel-export:abc123def456ghi" {
		t.Fatalf("got %q", id)
	}
	got, ok := PanelExportRun(id)
	if !ok || got != run {
		t.Fatalf("PanelExportRun(%q) = %q, %v", id, got, ok)
	}

	for _, bad := range []string{
		"",
		"panel-export:",
		"abc123def456ghi",
		"site-upload:abc123def456ghi",
		"panel-export:abc123def456gh",    // 14
		"panel-export:abc123def456ghij",  // 16
		"panel-export:ABC123DEF456GHI",   // not a record id
		"panel-export:abc123def456gh_",   // ditto
		"panel-export:abc123def456ghi\n", // a newline after it
		"panel-export:../../data.db....",
		" panel-export:abc123def456ghi",
		"PANEL-EXPORT:abc123def456ghi",
	} {
		if run, ok := PanelExportRun(bad); ok || run != "" {
			t.Errorf("PanelExportRun(%q) = %q, %v: want a refusal", bad, run, ok)
		}
	}
}

// The agent's backup tool takes the copy's file name as a dump's, and its
// tags and host as any run's: none of them may hold what it refuses.
func TestThePanelSnapshotsNamesPassTheBackupTool(t *testing.T) {
	for _, s := range []string{PanelFileName, PanelTag, PanelHostName} {
		if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/, \n\r\x00") {
			t.Errorf("%q would be refused", s)
		}
	}
	if !strings.HasPrefix(PanelExportPrefix, "panel-export") || strings.ContainsAny(PanelExportPrefix, "/ \n") {
		t.Errorf("the prefix %q", PanelExportPrefix)
	}
	if RelayRetry != 75 {
		t.Errorf("RelayRetry is %d, not EX_TEMPFAIL", RelayRetry)
	}
}

// The web's alert links and texts, and the docs, name the three rules.
func TestThePanelsAlertsKeepTheirNames(t *testing.T) {
	if AlertRecoveryKit != "recovery_kit" || AlertPanelBackup != "panel_backup" || AlertPanelRestored != "panel_restored" || PanelAlertTarget != "panel" {
		t.Error("an alert of the panel's backup changed its name")
	}
}
