package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upBackupTargets, downBackupTargets)
}

// upBackupTargets adds where backups go (#28). Targets are added, changed and
// removed through the panel's routes, which check them with restic first and
// seal their credentials and the repository's password; viewers read the rest.
func upBackupTargets(app core.App) error {
	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return err
	}
	targets := newCollection(schema.BackupTargets, access{list: schema.RuleViewer, view: schema.RuleViewer})
	targets.Fields.Add(
		&core.TextField{Name: "name", Required: true, Max: 100},
		&core.SelectField{Name: "kind", Required: true, MaxSelect: 1, Values: backup.Kinds},
		// the server whose agent reaches the target, and holds a local one
		&core.RelationField{Name: "host", Required: true, CollectionId: hosts.Id, MaxSelect: 1},
		&core.JSONField{Name: "config", MaxSize: 16 << 10},
		&core.TextField{Name: "credentials", Max: maxSealed, Hidden: true},
		&core.TextField{Name: "repo_password", Max: maxSealed, Hidden: true},
		// restic's id of the repository, from its config
		&core.TextField{Name: "repo_id", Max: 100},
		&core.SelectField{Name: "status", MaxSelect: 1, Values: []string{schema.TargetPending, schema.TargetOK, schema.TargetFailed}},
		&core.TextField{Name: "last_error", Max: 4096},
		&core.DateField{Name: "checked_at"},
		&core.NumberField{Name: "used_bytes", OnlyInt: true},
		// 0: unknown, as for a bucket without a quota
		&core.NumberField{Name: "capacity_bytes", OnlyInt: true},
		// the days a bucket's Object Lock keeps objects, as the admin states
		// it: a note, which PocketCloud does not enforce
		&core.NumberField{Name: "immutable_days", OnlyInt: true},
		&core.TextField{Name: "note", Max: 500},
	)
	targets.AddIndex("idx_backup_targets_name", true, "name", "")
	return app.Save(targets)
}

func downBackupTargets(app core.App) error {
	targets, err := app.FindCollectionByNameOrId(schema.BackupTargets)
	if err != nil {
		return nil
	}
	return app.Delete(targets)
}
