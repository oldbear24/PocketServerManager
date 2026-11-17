package backup

import (
	"regexp"
	"strings"
)

// The panel's backup of itself (#32). A backup job whose source is the panel
// has a carrier as its server: that server's agent fetches the panel's
// encrypted copy of itself (internal/panelbackup's PCPB1) with FetchArtifact,
// on a connection of its own, and backs it up through the relay as a dump's
// file. Neither the carrier nor the repository ever sees the copy
// unencrypted. See docs/architecture.md, "Panel backup and disaster recovery
// (#32)".
const (
	// CapabilityPanel is the pseudo-capability of an agent that can carry the
	// panel's backup, as host_shell is of one that offers a host shell:
	// capabilities gate command types only, and an older agent would refuse
	// BackupRun.panel_export as a second source.
	CapabilityPanel = "backup.run.panel"
	// PanelExportPrefix starts the artifact id of a panel run's copy, which
	// the backup_runs id completes (PanelExportID).
	PanelExportPrefix = "panel-export:"
	// PanelFileName is the file name of the copy in a panel snapshot: the
	// snapshot's one file is /pocketcloud-panel.pcpb.
	PanelFileName = "pocketcloud-panel.pcpb"
	// PanelTag is the restic tag of every panel snapshot, next to
	// pocketcloud and job:<id>.
	PanelTag = "pocketcloud-panel"
	// PanelHostName is restic's --host of every panel snapshot, whatever
	// server carried it, so that a change of carrier does not split the
	// listings.
	PanelHostName = "panel"
	// RelayRetry is the exit code of a relay that ended in a way worth
	// another try, as sysexits' EX_TEMPFAIL: the panel's copy broke off or
	// came damaged. The agent classes such a run FailureInterrupted, which
	// the panel retries.
	RelayRetry = 75
)

// The alerts of the panel's backup and recovery (#32). Each names the target
// PanelAlertTarget and no host, as the audit log's integrity alert does.
const (
	// AlertRecoveryKit is open while a server exists and no checked
	// recovery kit is current: warning, then critical once open 7 days.
	AlertRecoveryKit = "recovery_kit"
	// AlertPanelBackup is open while a server exists and the panel has no
	// enabled backup job with targets, or its last success is older than 48
	// hours.
	AlertPanelBackup = "panel_backup"
	// AlertPanelRestored is open after a restored panel's first start until
	// every server came back or was removed.
	AlertPanelRestored = "panel_restored"
	// PanelAlertTarget is the target of the three.
	PanelAlertTarget = "panel"
)

var panelExportForm = regexp.MustCompile(`^panel-export:[a-z0-9]{15}$`)

// PanelExportID is the artifact id under which the panel offers the copy of
// a panel backup run to that run's carrier.
func PanelExportID(runID string) string {
	return PanelExportPrefix + runID
}

// PanelExportRun reads the backup_runs id out of a panel copy's artifact id,
// and says whether it is one: the prefix and a record id of 15 characters
// [a-z0-9], nothing more.
func PanelExportRun(id string) (string, bool) {
	if !panelExportForm.MatchString(id) {
		return "", false
	}
	return strings.TrimPrefix(id, PanelExportPrefix), true
}
