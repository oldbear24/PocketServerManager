package panel

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/search"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
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

// runsReads are the reads of a window of runs that the index of created
// serves: the Overview's (O12, as the timing test of the page sends it) and
// the Backups page's 15 days (web/src/pages/Backups.tsx), each sorted by
// created. A read's fields change nothing of its SQL: PocketBase picks them
// from whole rows.
func runsReads(now time.Time) []overviewRead {
	reads := []overviewRead{}
	for _, read := range overviewReads(now, nil) {
		if read.collection == schema.BackupRuns {
			reads = append(reads, read)
		}
	}
	return append(reads, fullListRead("backups runs", schema.BackupRuns, map[string]string{
		"filter": "created >= " + pbFilterTime(now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -15)),
		"sort":   "created",
	}))
}

// viewerListQuery is the SQL, and its parameters, of the first page of a read
// as a viewer makes it: built from the collection's list rule, the viewer and
// the read's query by PocketBase's own field resolver and search provider, as
// its list route builds it (apis/record_crud.go, recordsList). A rule that
// names @request.auth, as the runs' does, joins the users and makes the read
// DISTINCT (core/record_field_resolver.go), and SQLite then reads the runs by
// created through its index only when the read is sorted by created: else
// it goes through every run by id, as DISTINCT costs nothing that way.
func viewerListQuery(t *testing.T, app core.App, read overviewRead) (string, dbx.Params) {
	t.Helper()
	collection, err := app.FindCollectionByNameOrId(read.collection)
	if err != nil {
		t.Fatal(err)
	}
	if collection.ListRule == nil || *collection.ListRule == "" {
		t.Fatalf("%s has no list rule", read.collection)
	}
	viewer, err := app.FindAuthRecordByEmail(schema.Users, "viewer@example.com")
	if err != nil {
		viewer, _ = testutil.NewUser(t, app, "viewer@example.com", schema.RoleViewer)
	}
	resolver := core.NewRecordFieldResolver(app, collection,
		&core.RequestInfo{Method: http.MethodGet, Auth: viewer, Context: core.RequestInfoContextDefault}, true)
	query := app.RecordQuery(collection)
	rule, err := search.FilterData(*collection.ListRule).BuildExpr(resolver)
	if err != nil {
		t.Fatal(err)
	}
	query.AndWhere(rule)
	resolver.SetAllowHiddenFields(false)
	// in place of the hook that makes records of the rows: only the SQL is wanted
	var built *dbx.Query
	query.WithBuildHook(func(q *dbx.Query) { built = q })

	params := url.Values{}
	for key, values := range read.query {
		params[key] = values
	}
	params.Set("page", "1")
	params.Set("perPage", strconv.Itoa(read.perPage))
	rows := []dbx.NullStringMap{}
	if _, err := search.NewProvider(resolver).Query(query).CountCol("_rowid_").ParseAndExec(params.Encode(), &rows); err != nil {
		t.Fatalf("%s: %v", read.name, err)
	}
	if built == nil {
		t.Fatalf("%s built no query", read.name)
	}
	return built.SQL(), built.Params()
}

// runsReadPlan is how SQLite runs a viewer's read of a window of runs.
func runsReadPlan(t *testing.T, app core.App, read overviewRead) string {
	t.Helper()
	sql, params := viewerListQuery(t, app, read)
	rows := []dbx.NullStringMap{}
	if err := app.DB().NewQuery("EXPLAIN QUERY PLAN " + sql).Bind(params).All(&rows); err != nil {
		t.Fatal(err)
	}
	details := []string{}
	for _, row := range rows {
		details = append(details, row["detail"].String)
	}
	return strings.Join(details, "; ")
}

// checkRunsByTime checks that the runs have the index and that each read of
// a window of them goes through it to the window's runs only: as SQLite
// first plans it, and once it has measured the tables, as PocketBase's
// nightly PRAGMA optimize has it do for a table that grew. Without the
// read's sort SQLite would go by id through every run kept: from the first,
// and after the measure still for the Backups page's 15 days.
func checkRunsByTime(t *testing.T, app core.App) {
	t.Helper()
	checkIndexes(t, app, schema.BackupRuns, runsCreatedIndex,
		// the indexes of before stay
		"INDEX idx_backup_runs_job ON backup_runs (job, created)",
		"INDEX idx_backup_runs_target ON backup_runs (target)",
		"INDEX idx_backup_runs_command ON backup_runs (command)",
	)
	for _, measured := range []bool{false, true} {
		if measured {
			// the test's connection made none of the queries PRAGMA
			// optimize goes by
			if _, err := app.NonconcurrentDB().NewQuery("ANALYZE").Execute(); err != nil {
				t.Fatal(err)
			}
		}
		for _, read := range runsReads(time.Now()) {
			if plan := runsReadPlan(t, app, read); !strings.Contains(plan, "SEARCH backup_runs USING INDEX idx_backup_runs_created (created>?)") {
				t.Errorf("%s is read %q (measured: %v)", read.name, plan, measured)
			}
		}
	}
}

// checkRunsWithoutIndex checks that no read of a window of runs uses the
// index, as before the migration and after its revert.
func checkRunsWithoutIndex(t *testing.T, app core.App, when string) {
	t.Helper()
	for _, read := range runsReads(time.Now()) {
		if plan := runsReadPlan(t, app, read); strings.Contains(plan, "idx_backup_runs_created") {
			t.Errorf("%s is read %q %s", read.name, plan, when)
		}
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
	// without it SQLite goes through every run
	checkRunsWithoutIndex(t, app, "before the migration")

	// the panel's next start applies only the new migration
	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("the migration fails over #53's: %v", err)
	}
	checkRunsByTime(t, app)
	if run, err := app.FindRecordById(schema.BackupRuns, f.run.Id); err != nil || run.GetString("status") != schema.RunFailed ||
		run.GetString("error") != f.run.GetString("error") || run.GetDateTime("created").String() != f.run.GetDateTime("created").String() {
		t.Errorf("a run of before changed: %v, %v", run, err)
	}
	// the window holds the runs of its 49 hours: those of the history, every
	// 6 hours, and f's
	window, err := app.FindRecordsByFilter(schema.BackupRuns, "created >= {:since}", "", 0, 0,
		dbx.Params{"since": types.NowDateTime().Add(-49 * time.Hour)})
	if err != nil || len(window) != 9+1 {
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
	checkRunsWithoutIndex(t, app, "after the revert")
	if count, err := app.CountRecords(schema.BackupRuns); err != nil || count != 60*4+1 {
		t.Errorf("the revert left %d runs: %v", count, err)
	}

	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("the migration does not apply again: %v", err)
	}
	checkRunsByTime(t, app)
}
