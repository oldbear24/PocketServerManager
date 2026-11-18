package migrations

import (
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upDatabaseBackups, downDatabaseBackups)
}

// What a backup job backs up, how it came to be, where a restore goes and
// what a download sends, before #53 and with it.
var (
	sourceKindsDatabases = []string{
		schema.SourceVolume, schema.SourcePath, schema.SourceStack, schema.SourceDump, schema.SourcePanel,
		schema.SourceDatabase, schema.SourceSite,
	}
	jobOriginsBefore    = []string{schema.JobManual, schema.JobLabel}
	jobOriginsDatabases = []string{schema.JobManual, schema.JobLabel, schema.JobDatabase, schema.JobSite}

	restoreModesBefore    = []string{schema.RestoreOriginal, schema.RestorePath, schema.RestoreDownload}
	restoreModesDatabases = []string{
		schema.RestoreOriginal, schema.RestorePath, schema.RestoreDownload, schema.RestoreDatabase, schema.RestoreNewDatabase,
	}
	downloadKindsBefore    = []string{schema.DownloadDir, schema.DownloadFile}
	downloadKindsDatabases = []string{schema.DownloadDir, schema.DownloadFile, schema.DownloadSQL}
)

// The fields #53 adds, by collection.
var (
	jobDatabaseFields     = []string{"database", "site", "db_server_name", "own_settings", "last_success_at"}
	restoreDatabaseFields = []string{"database", "database_name", "previous", "tables"}
	checkRestoreFields    = []string{
		"job", "database_name", "snapshot_id", "temporary", "tables_expected", "tables_restored", "dump_bytes", "failure",
	}
	hostAutoBackupFields = []string{"auto_backup_schedule", "auto_backup_targets", "auto_backup_retention"}
)

// The slot index of backup_checks: one check per target and slot (#31), then
// one per kind, target, job and slot (#53), so that a target's integrity check
// and the restore tests of its jobs share a slot.
const (
	checkSlotIndex = "idx_backup_checks_slot"
	checkSlotWhere = "scheduled_at != ''"
)

// upDatabaseBackups backs up every database of a shared server and every PHP
// site's files with a job of its own, restores a database into itself or a
// new database, and tests the restores (#53). A job of a database or a site
// names it by a relation that is cleared, not cascaded, when it goes, so the
// job and its snapshots stay; the server's name stays for display. A job
// follows its server's automatic-backup settings, but for those it set for
// itself. A restore of a database names it and the copy it kept; a restore
// test is a check of the job, with the database, the snapshot, the temporary
// database and the tables it compared. The restore tests have a schedule of
// their own in the settings.
func upDatabaseBackups(app core.App) error {
	databases, err := app.FindCollectionByNameOrId(schema.Databases)
	if err != nil {
		return err
	}
	sites, err := app.FindCollectionByNameOrId(schema.Sites)
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
	jobs.Fields.GetByName("source_kind").(*core.SelectField).Values = sourceKindsDatabases
	jobs.Fields.GetByName("origin").(*core.SelectField).Values = jobOriginsDatabases
	jobs.Fields.Add(
		// the database or the site an automatic job backs up; cleared when it
		// goes, so the job keeps its snapshots
		&core.RelationField{Name: "database", CollectionId: databases.Id, MaxSelect: 1},
		&core.RelationField{Name: "site", CollectionId: sites.Id, MaxSelect: 1},
		// the database's server, as people read it after the database went
		&core.TextField{Name: "db_server_name", Max: 48},
		// what of schedule, targets and retention the job set for itself; the
		// others follow its server's auto_backup_* settings
		&core.JSONField{Name: "own_settings", MaxSize: 1 << 10},
		// when a run of the job last succeeded
		&core.DateField{Name: "last_success_at"},
	)
	// one job per database and per site
	jobs.AddIndex("idx_backup_jobs_database", true, "database", "database != ''")
	jobs.AddIndex("idx_backup_jobs_site", true, "site", "site != ''")
	if err := app.Save(jobs); err != nil {
		return err
	}

	runs, err := app.FindCollectionByNameOrId(schema.BackupRuns)
	if err != nil {
		return err
	}
	// what the run added to the repository once compressed, restic's
	// data_added_packed
	runs.Fields.Add(&core.NumberField{Name: "bytes_stored", OnlyInt: true})
	if err := app.Save(runs); err != nil {
		return err
	}

	restores, err := app.FindCollectionByNameOrId(schema.BackupRestores)
	if err != nil {
		return err
	}
	restores.Fields.GetByName("mode").(*core.SelectField).Values = restoreModesDatabases
	restores.Fields.GetByName("download_kind").(*core.SelectField).Values = downloadKindsDatabases
	restores.Fields.Add(
		// the database a restore replaces, or the new one it makes
		&core.RelationField{Name: "database", CollectionId: databases.Id, MaxSelect: 1},
		&core.TextField{Name: "database_name", Max: 63},
		// the temporary database that keeps the data before an in-place
		// restore of PostgreSQL, pc_old_<restore id>
		&core.TextField{Name: "previous", Max: 63},
		// the tables restored
		&core.NumberField{Name: "tables", OnlyInt: true},
	)
	if err := app.Save(restores); err != nil {
		return err
	}

	checks, err := app.FindCollectionByNameOrId(schema.BackupChecks)
	if err != nil {
		return err
	}
	checks.Fields.Add(
		// a restore test's job, its database, the snapshot it restored, the
		// temporary database it restored into, and the tables of the dump and
		// of the restore; the dump's size and why the test failed
		&core.RelationField{Name: "job", CollectionId: jobs.Id, MaxSelect: 1, CascadeDelete: true},
		&core.TextField{Name: "database_name", Max: 63},
		&core.TextField{Name: "snapshot_id", Max: 64},
		&core.TextField{Name: "temporary", Max: 63},
		&core.NumberField{Name: "tables_expected", OnlyInt: true},
		&core.NumberField{Name: "tables_restored", OnlyInt: true},
		&core.NumberField{Name: "dump_bytes", OnlyInt: true},
		&core.TextField{Name: "failure", Max: 32},
	)
	checks.RemoveIndex(checkSlotIndex)
	checks.AddIndex(checkSlotIndex, true, "kind, target, job, scheduled_at", checkSlotWhere)
	checks.AddIndex("idx_backup_checks_job", false, "job, created", "")
	if err := app.Save(checks); err != nil {
		return err
	}

	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return err
	}
	hosts.Fields.Add(
		// the automatic jobs' defaults on the server: cron in UTC, empty for
		// the staggered daily default; the targets, none until an admin picks
		// them; the retention, null for 7d 4w 6m
		&core.TextField{Name: "auto_backup_schedule", Max: 255},
		&core.RelationField{Name: "auto_backup_targets", CollectionId: targets.Id, MaxSelect: 10},
		&core.JSONField{Name: "auto_backup_retention", MaxSize: 1 << 10},
	)
	if err := app.Save(hosts); err != nil {
		return err
	}

	settings, err := app.FindCollectionByNameOrId(schema.Settings)
	if err != nil {
		return err
	}
	// cron in UTC; empty turns the restore tests off
	settings.Fields.Add(&core.TextField{Name: "restore_test_schedule", Max: 100})
	if err := app.Save(settings); err != nil {
		return err
	}
	records, err := app.FindAllRecords(schema.Settings)
	if err != nil {
		return err
	}
	for _, record := range records {
		record.Set("restore_test_schedule", schema.DefaultRestoreTestSchedule)
		if err := app.Save(record); err != nil {
			return err
		}
	}
	return nil
}

func downDatabaseBackups(app core.App) error {
	// the automatic jobs, and with them their runs, restores and tests
	automatic, err := app.FindAllRecords(schema.BackupJobs, dbx.Or(
		dbx.In("origin", schema.JobDatabase, schema.JobSite),
		dbx.In("source_kind", schema.SourceDatabase, schema.SourceSite),
	))
	if err != nil {
		return err
	}
	for _, job := range automatic {
		if err := app.Delete(job); err != nil {
			return err
		}
	}
	// restores into databases, and SQL downloads, of any job left
	restoresOfDatabases, err := app.FindAllRecords(schema.BackupRestores, dbx.Or(
		dbx.In("mode", schema.RestoreDatabase, schema.RestoreNewDatabase),
		dbx.HashExp{"download_kind": schema.DownloadSQL},
	))
	if err != nil {
		return err
	}
	for _, restore := range restoresOfDatabases {
		if err := app.Delete(restore); err != nil {
			return err
		}
	}
	// every restore test: only #53 makes them, and the slot index of before
	// takes one check per target and slot
	tests, err := app.FindAllRecords(schema.BackupChecks, dbx.HashExp{"kind": schema.CheckRestoreTest})
	if err != nil {
		return err
	}
	for _, test := range tests {
		if err := app.Delete(test); err != nil {
			return err
		}
	}

	checks, err := app.FindCollectionByNameOrId(schema.BackupChecks)
	if err != nil {
		return err
	}
	checks.RemoveIndex("idx_backup_checks_job")
	checks.RemoveIndex(checkSlotIndex)
	checks.AddIndex(checkSlotIndex, true, "target, scheduled_at", checkSlotWhere)
	for _, name := range checkRestoreFields {
		checks.Fields.RemoveByName(name)
	}
	if err := app.Save(checks); err != nil {
		return err
	}

	restores, err := app.FindCollectionByNameOrId(schema.BackupRestores)
	if err != nil {
		return err
	}
	for _, name := range restoreDatabaseFields {
		restores.Fields.RemoveByName(name)
	}
	restores.Fields.GetByName("mode").(*core.SelectField).Values = restoreModesBefore
	restores.Fields.GetByName("download_kind").(*core.SelectField).Values = downloadKindsBefore
	if err := app.Save(restores); err != nil {
		return err
	}

	runs, err := app.FindCollectionByNameOrId(schema.BackupRuns)
	if err != nil {
		return err
	}
	runs.Fields.RemoveByName("bytes_stored")
	if err := app.Save(runs); err != nil {
		return err
	}

	jobs, err := app.FindCollectionByNameOrId(schema.BackupJobs)
	if err != nil {
		return err
	}
	jobs.RemoveIndex("idx_backup_jobs_database")
	jobs.RemoveIndex("idx_backup_jobs_site")
	for _, name := range jobDatabaseFields {
		jobs.Fields.RemoveByName(name)
	}
	jobs.Fields.GetByName("source_kind").(*core.SelectField).Values = sourceKindsPanel
	jobs.Fields.GetByName("origin").(*core.SelectField).Values = jobOriginsBefore
	if err := app.Save(jobs); err != nil {
		return err
	}

	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return err
	}
	for _, name := range hostAutoBackupFields {
		hosts.Fields.RemoveByName(name)
	}
	if err := app.Save(hosts); err != nil {
		return err
	}

	settings, err := app.FindCollectionByNameOrId(schema.Settings)
	if err != nil {
		return err
	}
	settings.Fields.RemoveByName("restore_test_schedule")
	return app.Save(settings)
}
