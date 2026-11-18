package panel

import (
	"fmt"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

const (
	// keepRestoreDrafts is how long a draft stays once it is of no use.
	keepRestoreDrafts = 24 * time.Hour
	// keepRestores is how long restores stay as a job's history; a job's
	// newest restore that was started stays whatever its age.
	keepRestores = 90 * 24 * time.Hour
)

// settleRestores heals the restores the panel lost track of, at its start
// and every minute: one whose command ended without its finisher, as when
// the panel stopped in between, is settled from the command; one whose
// command is gone fails; one someone stopped tells its agent again, as a
// cancel can be lost; a draft whose preview is too old expires.
func (p *Panel) settleRestores() {
	records, err := p.App.FindRecordsByFilter(schema.BackupRestores,
		"status={:previewing} || status={:previewed} || status={:queued} || status={:running}", "created", 0, 0,
		dbx.Params{"previewing": schema.RestorePreviewing, "previewed": schema.RestorePreviewed,
			"queued": schema.RestoreQueued, "running": schema.RestoreRunning})
	if err != nil {
		p.Log.Error("failed to read the restores", "error", err)
		return
	}
	for _, restore := range records {
		switch restore.GetString("status") {
		case schema.RestorePreviewed:
			if expires := restore.GetDateTime("expires_at"); !expires.IsZero() && time.Now().After(expires.Time()) {
				p.expireDraft(restore)
			}
		case schema.RestorePreviewing:
			command, err := p.App.FindRecordById(schema.Commands, restore.GetString("preview_command"))
			switch {
			case err != nil:
				p.settleRestore(restore, schema.RestoreFailed, "The preview ended without the panel recording it: preview again.")
			case commandFinal(command):
				p.restoreFinished(command)
			}
		default:
			p.settleStarted(restore)
		}
	}
}

// settleStarted heals a restore that was started.
func (p *Panel) settleStarted(restore *core.Record) {
	if restore.GetString("mode") == schema.RestoreDownload {
		p.settleDownload(restore)
		return
	}
	command, err := p.App.FindRecordById(schema.Commands, restore.GetString("command"))
	switch {
	case err != nil:
		p.settleRestore(restore, schema.RestoreFailed,
			fmt.Sprintf("The restore ended without the panel recording how: check the application on %s.", p.hostName(restore.GetString("host"))))
		// what a database restore made, as far as the panel can tell (#53)
		p.databaseRestored(restore, nil)
	case commandFinal(command):
		p.restoreFinished(command)
	case restore.GetString("stop_requested_label") != "":
		_ = p.Commands.Cancel(command.Id, "Cancelled by "+restore.GetString("stop_requested_label"))
	}
}

// settleDownload heals a download: one whose link nobody used in time
// fails; one whose command ended is settled from it; one whose browser's
// request is gone, as a panel restart ends it, stops and fails.
func (p *Panel) settleDownload(restore *core.Record) {
	id := restore.GetString("command")
	if id == "" {
		if time.Since(restore.GetDateTime("created").Time()) > downloadTicketLife+time.Minute {
			p.settleRestore(restore, schema.RestoreFailed, "The download link was not used in time: start the download again.")
		}
		return
	}
	command, err := p.App.FindRecordById(schema.Commands, id)
	switch {
	case err != nil:
		p.settleRestore(restore, schema.RestoreFailed, "The panel restarted during the download.")
	case commandFinal(command):
		p.downloadFinished(command)
	case p.downloads.get(id) == nil:
		_ = p.Commands.Cancel(id, "The panel restarted during the download.")
		p.settleRestore(restore, schema.RestoreFailed, "The panel restarted during the download.")
	}
}

// hostName is a server's name, or its id once it is gone.
func (p *Panel) hostName(id string) string {
	if host, err := p.App.FindRecordById(schema.Hosts, id); err == nil {
		return host.GetString("name")
	}
	return id
}

// pruneRestores removes the drafts nobody started after a day, and the
// restores and downloads older than their history keeps, but each job's
// newest restore that was started.
func (p *Panel) pruneRestores() {
	day := types.NowDateTime().Add(-keepRestoreDrafts)
	drafts, err := p.App.FindRecordsByFilter(schema.BackupRestores,
		"created<{:day} && (status={:previewing} || status={:previewed} || status={:expired} || (status={:failed} && command='' && mode!={:download}))", "", 0, 0,
		dbx.Params{"day": day.String(), "previewing": schema.RestorePreviewing, "previewed": schema.RestorePreviewed,
			"expired": schema.RestoreExpired, "failed": schema.RestoreFailed, "download": schema.RestoreDownload})
	if err != nil {
		p.Log.Error("failed to read the restores' drafts", "error", err)
	}
	for _, draft := range drafts {
		if draft.GetString("status") == schema.RestorePreviewing && draft.GetString("preview_command") != "" {
			_ = p.Commands.Cancel(draft.GetString("preview_command"), "The draft was too old.")
		}
		p.removeRestore(draft)
	}

	cutoff := types.NowDateTime().Add(-keepRestores)
	old, err := p.App.FindRecordsByFilter(schema.BackupRestores,
		"created<{:cutoff} && (status={:succeeded} || status={:failed} || status={:cancelled})", "", 0, 0,
		dbx.Params{"cutoff": cutoff.String(), "succeeded": schema.RestoreSucceeded, "failed": schema.RestoreFailed, "cancelled": schema.RestoreCancelled})
	if err != nil {
		p.Log.Error("failed to read the old restores", "error", err)
	}
	for _, restore := range old {
		// a job keeps its newest restore that was started, whatever its
		// age and however it ended: a draft or a download does not count
		if startedRestore(restore) {
			newer, err := p.App.CountRecords(schema.BackupRestores, dbx.NewExp(
				"job={:job} AND mode!={:download} AND command!='' AND status IN ({:succeeded}, {:failed}, {:cancelled}) AND "+
					"(created>{:created} OR (created={:created} AND id>{:id}))",
				dbx.Params{"job": restore.GetString("job"), "download": schema.RestoreDownload, "succeeded": schema.RestoreSucceeded,
					"failed": schema.RestoreFailed, "cancelled": schema.RestoreCancelled,
					"created": restore.GetDateTime("created").String(), "id": restore.Id}))
			if err != nil || newer == 0 {
				continue
			}
		}
		p.removeRestore(restore)
	}
}

// startedRestore tells whether a record is a restore someone started, to
// the original location or to a new folder, rather than a draft or a
// download.
func startedRestore(restore *core.Record) bool {
	return restore.GetString("mode") != schema.RestoreDownload && restore.GetString("command") != ""
}

// removeRestore removes a restore record, with its hooks: the alert of its
// data goes with the last restore of it that went wrong.
func (p *Panel) removeRestore(restore *core.Record) {
	if err := p.App.Delete(restore); err != nil {
		p.Log.Error("failed to remove a restore", "restore", restore.Id, "error", err)
	}
}
