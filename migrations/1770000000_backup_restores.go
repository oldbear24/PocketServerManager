package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upBackupRestores, downBackupRestores)
}

// upBackupRestores adds restores and downloads of a job's snapshots (#30). A
// record is the plan of a restore, its preview, what lets the panel give the
// target's secrets to the restore's server, and its history. Only the
// panel's routes write it, and operators read it: it names folders of the
// servers. Commands that an agent settles after a cancel, such as a restore
// that starts its application again, keep when and why they were asked to
// stop until the agent reports.
func upBackupRestores(app core.App) error {
	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return err
	}
	users, err := app.FindCollectionByNameOrId(schema.Users)
	if err != nil {
		return err
	}
	targets, err := app.FindCollectionByNameOrId(schema.BackupTargets)
	if err != nil {
		return err
	}
	jobs, err := app.FindCollectionByNameOrId(schema.BackupJobs)
	if err != nil {
		return err
	}
	commands, err := app.FindCollectionByNameOrId(schema.Commands)
	if err != nil {
		return err
	}

	commands.Fields.Add(
		// when someone stopped a command that its agent settles, and who:
		// it stays open until the agent says how it ended
		&core.DateField{Name: "cancel_requested_at"},
		&core.TextField{Name: "cancel_reason", Max: 255},
	)
	if err := app.Save(commands); err != nil {
		return err
	}

	restores := newCollection(schema.BackupRestores, access{list: schema.RuleOperator, view: schema.RuleOperator})
	restores.Fields.Add(
		&core.RelationField{Name: "job", Required: true, CollectionId: jobs.Id, MaxSelect: 1, CascadeDelete: true},
		&core.RelationField{Name: "target", Required: true, CollectionId: targets.Id, MaxSelect: 1, CascadeDelete: true},
		// the server the restore writes on, or whose agent sends a download
		&core.RelationField{Name: "host", Required: true, CollectionId: hosts.Id, MaxSelect: 1, CascadeDelete: true},
		// what people read, such as "vaultwarden ← B2 · 3f9a1c2e → original"
		&core.TextField{Name: "label", Max: 255},
		&core.TextField{Name: "snapshot_id", Required: true, Max: 64},
		&core.DateField{Name: "snapshot_time"},
		&core.SelectField{Name: "mode", Required: true, MaxSelect: 1, Values: []string{
			schema.RestoreOriginal, schema.RestorePath, schema.RestoreDownload,
		}},
		// the original location's roots, each a snapshot path and the volume
		// or directory it goes back into; resolved from the preview at start
		&core.JSONField{Name: "roots", MaxSize: 8 << 10},
		// the new folder a restore writes into, or the snapshot path a
		// download sends
		&core.TextField{Name: "path", Max: 1000},
		// the snapshot's folder a restore to a new folder restores
		&core.TextField{Name: "source", Max: 1000},
		// the snapshot paths restored, if not all of it
		&core.JSONField{Name: "include", MaxSize: 64 << 10},
		// replace deletes what the destination holds that the snapshot does
		// not; overwrite is set when someone confirmed that the restore
		// overwrites what the destination holds
		&core.BoolField{Name: "replace"},
		&core.BoolField{Name: "overwrite"},
		// the Compose project whose containers stop during the restore
		&core.TextField{Name: "stack_name", Max: 255},
		&core.SelectField{Name: "status", Required: true, MaxSelect: 1, Values: []string{
			schema.RestorePreviewing, schema.RestorePreviewed, schema.RestoreQueued, schema.RestoreRunning,
			schema.RestoreSucceeded, schema.RestoreFailed, schema.RestoreCancelled, schema.RestoreExpired,
		}},
		// the agent's dry run: counts, samples, the destination's contents
		// and the containers that would stop
		&core.JSONField{Name: "preview", MaxSize: 128 << 10},
		&core.DateField{Name: "preview_at"},
		&core.DateField{Name: "started_at"},
		&core.DateField{Name: "finished_at"},
		// when a draft's preview stops being good enough to start from
		&core.DateField{Name: "expires_at"},
		&core.RelationField{Name: "preview_command", CollectionId: commands.Id, MaxSelect: 1},
		&core.RelationField{Name: "command", CollectionId: commands.Id, MaxSelect: 1},
		&core.RelationField{Name: "requested_by", CollectionId: users.Id, MaxSelect: 1},
		&core.RelationField{Name: "confirmed_by", CollectionId: users.Id, MaxSelect: 1},
		&core.TextField{Name: "requested_label", Max: 255},
		&core.TextField{Name: "confirmed_label", Max: 255},
		&core.TextField{Name: "stop_requested_label", Max: 255},
		&core.NumberField{Name: "files_restored", OnlyInt: true},
		&core.NumberField{Name: "bytes_restored", OnlyInt: true},
		&core.NumberField{Name: "files_deleted", OnlyInt: true},
		&core.NumberField{Name: "errors", OnlyInt: true},
		// what a download sent
		&core.NumberField{Name: "bytes", OnlyInt: true},
		// a restore that ended before it was complete, with part of the
		// snapshot in the destination
		&core.BoolField{Name: "partial"},
		// the containers the restore stopped, and what was wrong when it
		// started them again
		&core.JSONField{Name: "app_stopped", MaxSize: 8 << 10},
		&core.TextField{Name: "app_problem", Max: 1000},
		&core.TextField{Name: "error", Max: 4096},
		// the end of the job's output, with the target's secrets hidden
		&core.TextField{Name: "log", Max: 32 << 10},
		&core.TextField{Name: "download_name", Max: 255},
		&core.SelectField{Name: "download_kind", MaxSelect: 1, Values: []string{schema.DownloadDir, schema.DownloadFile}},
	)
	restores.AddIndex("idx_backup_restores_host_status", false, "host, status", "")
	restores.AddIndex("idx_backup_restores_job", false, "job, created", "")
	restores.AddIndex("idx_backup_restores_command", false, "command", "")
	restores.AddIndex("idx_backup_restores_preview_command", false, "preview_command", "")
	return app.Save(restores)
}

func downBackupRestores(app core.App) error {
	restores, err := app.FindCollectionByNameOrId(schema.BackupRestores)
	if err != nil {
		return err
	}
	if err := app.Delete(restores); err != nil {
		return err
	}
	commands, err := app.FindCollectionByNameOrId(schema.Commands)
	if err != nil {
		return err
	}
	commands.Fields.RemoveByName("cancel_requested_at")
	commands.Fields.RemoveByName("cancel_reason")
	return app.Save(commands)
}
