package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upOverview, downOverview)
}

// runsCreatedIndex orders the backup runs by when they were made.
const runsCreatedIndex = "idx_backup_runs_created"

// upOverview indexes the backup runs by when they were made (#33). The
// Overview reads the runs of the last day and a half, and the Backups page
// those of two weeks; no index of the runs started with created, so each read
// went through every run the panel keeps, 60 days of them, each with a log of
// up to 32 KB.
func upOverview(app core.App) error {
	runs, err := app.FindCollectionByNameOrId(schema.BackupRuns)
	if err != nil {
		return err
	}
	runs.AddIndex(runsCreatedIndex, false, "created", "")
	return app.Save(runs)
}

func downOverview(app core.App) error {
	runs, err := app.FindCollectionByNameOrId(schema.BackupRuns)
	if err != nil {
		return err
	}
	runs.RemoveIndex(runsCreatedIndex)
	return app.Save(runs)
}
