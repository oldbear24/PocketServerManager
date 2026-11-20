package panel

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// overviewMigration is the file of the migration that indexes the backup
// runs by their time (#33).
const overviewMigration = "1774000000_overview.go"

// runsCreatedIndex is the index as SQLite has it.
const runsCreatedIndex = "INDEX idx_backup_runs_created ON backup_runs (created)"

// seedRunHistory gives f's job runs, one every 6 hours over the 60 days the
// panel keeps them, as a panel that backs up has them.
func seedRunHistory(t *testing.T, app core.App, f checkFixture) {
	t.Helper()
	now := time.Now().UTC()
	err := app.RunInTransaction(func(tx core.App) error {
		collection, err := tx.FindCollectionByNameOrId(schema.BackupRuns)
		if err != nil {
			return err
		}
		for i := range 60 * 4 {
			run := core.NewRecord(collection)
			run.Load(map[string]any{"job": f.job.Id, "target": f.target.Id, "host": f.host.Id, "trigger": schema.RunSchedule,
				"status": schema.RunSucceeded, "log": strings.Repeat("restic backup\n", 200)})
			created, err := types.ParseDateTime(now.Add(-time.Duration(i) * 6 * time.Hour))
			if err != nil {
				return err
			}
			run.SetRaw("created", created)
			if err := tx.Save(run); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// runsSincePlan is how SQLite reads the runs of the Overview's window, after
// it measured the tables as PocketBase has it do each night.
func runsSincePlan(t *testing.T, app core.App) string {
	t.Helper()
	if _, err := app.NonconcurrentDB().NewQuery("PRAGMA optimize").Execute(); err != nil {
		t.Fatal(err)
	}
	rows := []dbx.NullStringMap{}
	if err := app.DB().NewQuery("EXPLAIN QUERY PLAN SELECT id FROM backup_runs WHERE created >= {:since}").
		Bind(dbx.Params{"since": types.NowDateTime().Add(-37 * time.Hour).String()}).All(&rows); err != nil {
		t.Fatal(err)
	}
	details := []string{}
	for _, row := range rows {
		details = append(details, row["detail"].String)
	}
	return strings.Join(details, "; ")
}

// checkRunsByTime checks that the runs have the index and that the window of
// them is read through it.
func checkRunsByTime(t *testing.T, app core.App) {
	t.Helper()
	checkIndexes(t, app, schema.BackupRuns, runsCreatedIndex,
		// the indexes of before stay
		"INDEX idx_backup_runs_job ON backup_runs (job, created)",
		"INDEX idx_backup_runs_target ON backup_runs (target)",
		"INDEX idx_backup_runs_command ON backup_runs (command)",
	)
	if plan := runsSincePlan(t, app); !strings.Contains(plan, "USING INDEX idx_backup_runs_created") {
		t.Errorf("the runs since a moment are read %q", plan)
	}
}

// TestTheOverviewMigrationAppliesToAFreshDatabase: a new panel reads the
// runs of a window through their index of time, once it has their history.
func TestTheOverviewMigrationAppliesToAFreshDatabase(t *testing.T) {
	// it migrates an app of its own from the start, the slowest work of
	// the package under the race detector: beside the other such tests
	t.Parallel()
	app := bootMigrationPanel(t)
	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("the migrations of a fresh install fail: %v", err)
	}
	checkRunsByTime(t, app)
	seedRunHistory(t, app, seedCheckFixture(t, app, "fresh"))
	checkRunsByTime(t, app)
}

// TestTheOverviewMigrationAppliesOverDatabaseBackups: a panel migrated
// through #53's migration, with 60 days of runs, takes the index and keeps
// the runs; reverting it removes the index, so the window is read through
// every run again, and keeps the runs; it applies again after that.
func TestTheOverviewMigrationAppliesOverDatabaseBackups(t *testing.T) {
	// it migrates an app of its own from the start, the slowest work of
	// the package under the race detector: beside the other such tests
	t.Parallel()
	app := bootMigrationPanel(t)
	if _, err := core.NewMigrationsRunner(app, migrationsBefore(t, overviewMigration)).Up(); err != nil {
		t.Fatalf("the migrations through 1773000000 fail: %v", err)
	}
	runs, err := app.FindCollectionByNameOrId(schema.BackupRuns)
	if err != nil {
		t.Fatal(err)
	}
	if runs.Fields.GetByName("bytes_stored") == nil {
		t.Fatal("#53's migration did not run")
	}
	f := seedCheckFixture(t, app, "old")
	seedRunHistory(t, app, f)
	// without it SQLite goes through every run, or skips through those of
	// each job when the jobs are few
	if plan := runsSincePlan(t, app); strings.Contains(plan, "idx_backup_runs_created") {
		t.Fatalf("the runs are read %q before the migration", plan)
	}

	// the panel's next start applies only the new migration
	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("the migration fails over #53's: %v", err)
	}
	checkRunsByTime(t, app)
	if run, err := app.FindRecordById(schema.BackupRuns, f.run.Id); err != nil || run.GetString("status") != schema.RunFailed ||
		run.GetString("error") != f.run.GetString("error") || run.GetDateTime("created").String() != f.run.GetDateTime("created").String() {
		t.Errorf("a run of before changed: %v, %v", run, err)
	}
	// the window holds the runs of its 37 hours: those of the history, every
	// 6 hours, and f's
	window, err := app.FindRecordsByFilter(schema.BackupRuns, "created >= {:since}", "", 0, 0,
		dbx.Params{"since": types.NowDateTime().Add(-37 * time.Hour)})
	if err != nil || len(window) != 7+1 {
		t.Errorf("the runs of the window: %d, %v", len(window), err)
	}

	later := migrationsFrom(t, overviewMigration)
	reverted, err := core.NewMigrationsRunner(app, core.AppMigrations).Down(len(later))
	if err != nil || !slices.Equal(reverted, later) {
		t.Fatalf("reverting: %v, %v", reverted, err)
	}
	runs, err = app.FindCollectionByNameOrId(schema.BackupRuns)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range runs.Indexes {
		if strings.Contains(index, "idx_backup_runs_created") {
			t.Errorf("the index %s stays after the revert", index)
		}
	}
	if plan := runsSincePlan(t, app); strings.Contains(plan, "idx_backup_runs_created") {
		t.Errorf("the runs are read %q after the revert", plan)
	}
	if count, err := app.CountRecords(schema.BackupRuns); err != nil || count != 60*4+1 {
		t.Errorf("the revert left %d runs: %v", count, err)
	}

	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("the migration does not apply again: %v", err)
	}
	checkRunsByTime(t, app)
}
