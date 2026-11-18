package panel

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// databaseBackupsMigration is the file of the migration of per-database
// backups and restore tests (#53).
const databaseBackupsMigration = "1773000000_database_backups.go"

// laterFields are the fields later migrations add to a collection whose
// earlier migration's test checks its fields exactly: the later migration's
// own test checks them.
var laterFields = map[string][]string{
	schema.BackupChecks:   {"job", "database_name", "snapshot_id", "temporary", "tables_expected", "tables_restored", "dump_bytes", "failure"},
	schema.BackupRestores: {"database", "database_name", "previous", "tables"},
}

// checkDatabaseBackupsSchema checks what the migration adds and changes, as
// #53's agent, panel and web parts code against it.
func checkDatabaseBackupsSchema(t *testing.T, app core.App) {
	t.Helper()
	if schema.SourceDatabase != "database" || schema.SourceSite != "site" || schema.JobDatabase != "database" || schema.JobSite != "site" ||
		schema.RestoreDatabase != "database" || schema.RestoreNewDatabase != "new_database" || schema.DownloadSQL != "sql" ||
		schema.DefaultRestoreTestSchedule != "20 5 * * 3" || schema.OwnSchedule != "schedule" || schema.OwnTargets != "targets" ||
		schema.OwnRetention != "retention" || schema.TemporaryDatabase.String() != `^pc_(rt|rs|old)_[a-z0-9]{15}$` {
		t.Error("a name of the contract changed")
	}

	databases, err := app.FindCollectionByNameOrId(schema.Databases)
	if err != nil {
		t.Fatal(err)
	}
	sites, err := app.FindCollectionByNameOrId(schema.Sites)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := app.FindCollectionByNameOrId(schema.BackupTargets)
	if err != nil {
		t.Fatal(err)
	}
	relation := func(c *core.Collection, name, to string, maxSelect int, cascade bool) {
		t.Helper()
		field, ok := c.Fields.GetByName(name).(*core.RelationField)
		if !ok || field.CollectionId != to || field.MaxSelect != maxSelect || field.CascadeDelete != cascade || field.Required {
			t.Errorf("%s.%s is %+v", c.Name, name, c.Fields.GetByName(name))
		}
	}
	selectValues := func(c *core.Collection, name string, values []string) {
		t.Helper()
		if field, ok := c.Fields.GetByName(name).(*core.SelectField); !ok || !slices.Equal(field.Values, values) || field.MaxSelect != 1 {
			t.Errorf("%s.%s takes %v", c.Name, name, c.Fields.GetByName(name))
		}
	}
	texts := func(c *core.Collection, sizes map[string]int) {
		t.Helper()
		for name, size := range sizes {
			if field := textField(t, c, name); field.Max != size || field.Required || field.Hidden {
				t.Errorf("%s.%s is %+v, not of %d characters", c.Name, name, field, size)
			}
		}
	}
	numbers := func(c *core.Collection, names ...string) {
		t.Helper()
		for _, name := range names {
			if field, ok := c.Fields.GetByName(name).(*core.NumberField); !ok || !field.OnlyInt || field.Required {
				t.Errorf("%s.%s is not a whole number", c.Name, name)
			}
		}
	}

	jobs, err := app.FindCollectionByNameOrId(schema.BackupJobs)
	if err != nil {
		t.Fatal(err)
	}
	selectValues(jobs, "source_kind", []string{
		schema.SourceVolume, schema.SourcePath, schema.SourceStack, schema.SourceDump, schema.SourcePanel, schema.SourceDatabase, schema.SourceSite,
	})
	selectValues(jobs, "origin", []string{schema.JobManual, schema.JobLabel, schema.JobDatabase, schema.JobSite})
	checkFieldTypes(t, jobs, map[string]string{
		"database": core.FieldTypeRelation, "site": core.FieldTypeRelation, "db_server_name": core.FieldTypeText,
		"own_settings": core.FieldTypeJSON, "last_success_at": core.FieldTypeDate,
	}, false)
	// a database's or a site's going clears the relation: the job stays
	relation(jobs, "database", databases.Id, 1, false)
	relation(jobs, "site", sites.Id, 1, false)
	texts(jobs, map[string]int{"db_server_name": 48})
	if field := jobs.Fields.GetByName("own_settings").(*core.JSONField); field.MaxSize != 1<<10 {
		t.Errorf("own_settings holds %d bytes", field.MaxSize)
	}
	checkIndexes(t, app, schema.BackupJobs,
		"UNIQUE INDEX idx_backup_jobs_database ON backup_jobs (database) WHERE database != ''",
		"UNIQUE INDEX idx_backup_jobs_site ON backup_jobs (site) WHERE site != ''",
		// #32's index stays
		"UNIQUE INDEX idx_backup_jobs_panel ON backup_jobs (source_kind) WHERE source_kind = 'panel'",
	)
	for _, audited := range schema.Audited {
		switch audited.Name {
		case schema.BackupJobs:
			if !slices.Contains(audited.Ignore, "last_success_at") {
				t.Error("the audit records a job's last success")
			}
			for _, name := range []string{"database", "site", "db_server_name", "own_settings"} {
				if slices.Contains(audited.Ignore, name) {
					t.Errorf("the audit leaves a job's %s out", name)
				}
			}
		case schema.Databases:
			if !slices.Contains(audited.Ignore, "temporary_until") {
				t.Error("the audit records a temporary database's time")
			}
		case schema.Hosts:
			for _, name := range []string{"auto_backup_schedule", "auto_backup_targets", "auto_backup_retention"} {
				if slices.Contains(audited.Ignore, name) {
					t.Errorf("the audit leaves a server's %s out", name)
				}
			}
		}
	}

	runs, err := app.FindCollectionByNameOrId(schema.BackupRuns)
	if err != nil {
		t.Fatal(err)
	}
	numbers(runs, "bytes_stored")

	restores, err := app.FindCollectionByNameOrId(schema.BackupRestores)
	if err != nil {
		t.Fatal(err)
	}
	selectValues(restores, "mode", []string{
		schema.RestoreOriginal, schema.RestorePath, schema.RestoreDownload, schema.RestoreDatabase, schema.RestoreNewDatabase,
	})
	selectValues(restores, "download_kind", []string{schema.DownloadDir, schema.DownloadFile, schema.DownloadSQL})
	checkFieldTypes(t, restores, map[string]string{
		"database": core.FieldTypeRelation, "database_name": core.FieldTypeText, "previous": core.FieldTypeText, "tables": core.FieldTypeNumber,
	}, false)
	relation(restores, "database", databases.Id, 1, false)
	texts(restores, map[string]int{"database_name": 63, "previous": 63})
	numbers(restores, "tables")
	// the plan rides in roots
	if field := restores.Fields.GetByName("roots").(*core.JSONField); field.MaxSize != 8<<10 {
		t.Errorf("roots holds %d bytes", field.MaxSize)
	}

	checks, err := app.FindCollectionByNameOrId(schema.BackupChecks)
	if err != nil {
		t.Fatal(err)
	}
	checkFieldTypes(t, checks, map[string]string{
		"job": core.FieldTypeRelation, "database_name": core.FieldTypeText, "snapshot_id": core.FieldTypeText,
		"temporary": core.FieldTypeText, "tables_expected": core.FieldTypeNumber, "tables_restored": core.FieldTypeNumber,
		"dump_bytes": core.FieldTypeNumber, "failure": core.FieldTypeText,
	}, false)
	if !slices.Equal(laterFields[schema.BackupChecks], []string{"job", "database_name", "snapshot_id", "temporary", "tables_expected", "tables_restored", "dump_bytes", "failure"}) ||
		!slices.Equal(laterFields[schema.BackupRestores], []string{"database", "database_name", "previous", "tables"}) {
		t.Error("the earlier migrations' tests let other fields pass")
	}
	// a test goes with its job
	relation(checks, "job", jobs.Id, 1, true)
	texts(checks, map[string]int{"database_name": 63, "snapshot_id": 64, "temporary": 63, "failure": 32})
	numbers(checks, "tables_expected", "tables_restored", "dump_bytes")
	if field := checks.Fields.GetByName("kind").(*core.SelectField); !slices.Equal(field.Values, []string{schema.CheckIntegrity, schema.CheckRestoreTest}) {
		t.Errorf("a check's kind takes %v", field.Values)
	}
	for _, failure := range backup.TestFailures {
		if len(failure) > 32 {
			t.Errorf("failure holds 32 characters, not %q", failure)
		}
	}
	checkIndexes(t, app, schema.BackupChecks,
		"UNIQUE INDEX idx_backup_checks_slot ON backup_checks (kind, target, job, scheduled_at) WHERE scheduled_at != ''",
		"INDEX idx_backup_checks_job ON backup_checks (job, created)",
	)
	for _, index := range checks.Indexes {
		if strings.Contains(index, "(`target`, `scheduled_at`)") || strings.Contains(index, "(target, scheduled_at)") {
			t.Errorf("the slot of before stays: %s", index)
		}
	}

	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		t.Fatal(err)
	}
	checkFieldTypes(t, hosts, map[string]string{
		"auto_backup_schedule": core.FieldTypeText, "auto_backup_targets": core.FieldTypeRelation, "auto_backup_retention": core.FieldTypeJSON,
	}, false)
	texts(hosts, map[string]int{"auto_backup_schedule": 255})
	relation(hosts, "auto_backup_targets", targets.Id, 10, false)
	if field := hosts.Fields.GetByName("auto_backup_retention").(*core.JSONField); field.MaxSize != 1<<10 {
		t.Errorf("auto_backup_retention holds %d bytes", field.MaxSize)
	}

	settings, err := app.FindCollectionByNameOrId(schema.Settings)
	if err != nil {
		t.Fatal(err)
	}
	texts(settings, map[string]int{"restore_test_schedule": 100})

	// a temporary database is a databases record: unchanged
	if _, ok := databases.Fields.GetByName("temporary_until").(*core.DateField); !ok {
		t.Error("databases has no date temporary_until")
	}
	if _, err := app.FindCollectionByNameOrId("db_temporaries"); err == nil {
		t.Error("a collection of temporary databases exists")
	}
}

// checkRestoreTestDefault checks that every settings record runs the
// restore tests on the default schedule.
func checkRestoreTestDefault(t *testing.T, app core.App) {
	t.Helper()
	records, err := app.FindAllRecords(schema.Settings)
	if err != nil || len(records) == 0 {
		t.Fatalf("settings: %v, %v", records, err)
	}
	for _, record := range records {
		if got := record.GetString("restore_test_schedule"); got != schema.DefaultRestoreTestSchedule {
			t.Errorf("the restore tests run at %q", got)
		}
	}
}

// databaseFixture is a shared server with a database, a PHP site, and the
// check fixture's server, target and job.
type databaseFixture struct {
	checkFixture
	server, database, site *core.Record
}

func seedDatabaseFixture(t *testing.T, app core.App, suffix string) databaseFixture {
	t.Helper()
	f := databaseFixture{checkFixture: seedCheckFixture(t, app, suffix)}
	f.server = saveRecord(t, app, schema.DBServers, map[string]any{"host": f.host.Id, "name": "db-" + suffix, "engine": schema.EnginePostgres,
		"kind": schema.DBServerShared, "container_ref": "pcdb-db-" + suffix + "-db-1", "admin_user": "postgres", "status": "ready"})
	// still being created, so that the panel's hook makes no job of it: the
	// tests save the jobs themselves
	f.database = saveRecord(t, app, schema.Databases, map[string]any{"server": f.server.Id, "name": "shop", "owner_kind": "none", "status": "creating"})
	// a site's record as it is stored, without the checks of a new site,
	// which want an edge on its server
	sites, err := app.FindCollectionByNameOrId(schema.Sites)
	if err != nil {
		t.Fatal(err)
	}
	f.site = core.NewRecord(sites)
	f.site.Load(map[string]any{"name": "wp-" + suffix, "host": f.host.Id, "type": schema.SitePHPApp, "source": "upload"})
	if err := app.SaveNoValidate(f.site); err != nil {
		t.Fatal(err)
	}
	return f
}

// databaseJob is the automatic job of f's database.
func databaseJob(f databaseFixture) map[string]any {
	return map[string]any{"name": "db-shop-" + f.host.GetString("name"), "host": f.host.Id, "source_kind": schema.SourceDatabase,
		"source": "shop", "engine": schema.DumpPostgres, "database": f.database.Id, "db_server_name": f.server.GetString("name"),
		"origin": schema.JobDatabase, "enabled": true, "targets": []string{f.target.Id}, "schedule": "45 2 * * *",
		"retention": map[string]int{"daily": 7, "weekly": 4, "monthly": 6}, "own_settings": []string{schema.OwnSchedule},
		"last_success_at": types.NowDateTime()}
}

// siteJob is the automatic job of f's site's files.
func siteJob(f databaseFixture) map[string]any {
	return map[string]any{"name": f.site.GetString("name") + "-files", "host": f.host.Id, "source_kind": schema.SourceSite,
		"source": f.site.GetString("name"), "site": f.site.Id, "origin": schema.JobSite, "enabled": true, "schedule": "45 2 * * *",
		"own_settings": []string{}}
}

// restoreTest is a restore test of a job from f's target, in a slot.
func restoreTest(f databaseFixture, job *core.Record, slot types.DateTime) map[string]any {
	return map[string]any{"target": f.target.Id, "host": f.host.Id, "kind": schema.CheckRestoreTest, "trigger": schema.RunSchedule,
		"status": schema.CheckFailed, "scheduled_at": slot, "job": job.Id, "database_name": "shop",
		"snapshot_id": strings.Repeat("3f", 32), "temporary": "pc_rt_" + strings.Repeat("a", 15), "tables_expected": 12,
		"tables_restored": 11, "dump_bytes": int64(1) << 34, "failure": backup.TestTables, "attempt": 1}
}

// integrityCheck is an integrity check of f's target in a slot.
func integrityCheck(f databaseFixture, slot types.DateTime) map[string]any {
	return map[string]any{"target": f.target.Id, "host": f.host.Id, "kind": schema.CheckIntegrity, "trigger": schema.RunSchedule,
		"status": schema.CheckOK, "scheduled_at": slot, "subset": "3/20"}
}

// databaseRestore is a restore of f's database into itself.
func databaseRestore(f databaseFixture, job *core.Record) map[string]any {
	return map[string]any{"job": job.Id, "target": f.target.Id, "host": f.host.Id, "snapshot_id": strings.Repeat("3f", 32),
		"mode": schema.RestoreDatabase, "status": schema.RestoreSucceeded, "database": f.database.Id, "database_name": "shop",
		"previous": "pc_old_" + strings.Repeat("b", 15), "tables": 12, "stack_name": "shop",
		"roots": []map[string]any{{"source": "/shop.pgdump", "container": f.server.GetString("container_ref"), "engine": "postgres",
			"admin_user": "postgres", "database": "shop", "user": "shop", "scratch": "pc_rs_" + strings.Repeat("b", 15),
			"previous": "pc_old_" + strings.Repeat("b", 15)}}}
}

// TestTheDatabaseBackupsMigrationAppliesToAFreshDatabase: a new panel gets
// the automatic jobs' fields, the database restores, the restore tests, the
// servers' defaults and the restore tests' schedule; one job per database
// and per site; an integrity check and restore tests share a slot.
func TestTheDatabaseBackupsMigrationAppliesToAFreshDatabase(t *testing.T) {
	app := bootMigrationPanel(t)
	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("the migrations of a fresh install fail: %v", err)
	}
	checkDatabaseBackupsSchema(t, app)
	checkRestoreTestDefault(t, app)

	f := seedDatabaseFixture(t, app, "fresh")
	job := saveRecord(t, app, schema.BackupJobs, databaseJob(f))
	if job.GetString("database") != f.database.Id || !slices.Equal(job.GetStringSlice("own_settings"), []string{"schedule"}) ||
		job.GetDateTime("last_success_at").IsZero() || job.GetString("origin") != schema.JobDatabase {
		t.Errorf("the job does not keep what it was given: %v", job)
	}
	again := databaseJob(f)
	again["name"] = "db-shop-again"
	if _, err := trySave(app, schema.BackupJobs, again); err == nil {
		t.Error("a second job backs up the database")
	}
	files := saveRecord(t, app, schema.BackupJobs, siteJob(f))
	againSite := siteJob(f)
	againSite["name"] = "wp-files-again"
	if _, err := trySave(app, schema.BackupJobs, againSite); err == nil {
		t.Error("a second job backs up the site")
	}
	// jobs of no database and no site are many
	other := seedDatabaseFixture(t, app, "other")
	if other.job.GetString("database") != "" || other.job.GetString("site") != "" {
		t.Fatal(other.job)
	}
	long := databaseJob(other)
	long["db_server_name"] = strings.Repeat("d", 49)
	if _, err := trySave(app, schema.BackupJobs, long); err == nil {
		t.Error("a job kept a server name of 49 characters")
	}
	big := databaseJob(other)
	big["own_settings"] = []string{strings.Repeat("s", 1<<10)}
	if _, err := trySave(app, schema.BackupJobs, big); err == nil {
		t.Error("a job's own_settings took more than 1 KiB")
	}
	run := saveRecord(t, app, schema.BackupRuns, map[string]any{"job": job.Id, "target": f.target.Id, "host": f.host.Id,
		"trigger": schema.RunSchedule, "status": schema.RunSucceeded, "snapshot_id": strings.Repeat("3f", 32), "bytes_stored": int64(1) << 33})
	if run.GetInt("bytes_stored") != 1<<33 {
		t.Errorf("a run's bytes_stored: %v", run)
	}

	// an integrity check and the restore tests of two jobs share a target's
	// slot; a job has one test per slot
	slot := types.NowDateTime().Add(-time.Hour)
	saveRecord(t, app, schema.BackupChecks, integrityCheck(f, slot))
	test := saveRecord(t, app, schema.BackupChecks, restoreTest(f, job, slot))
	saveRecord(t, app, schema.BackupChecks, restoreTest(f, files, slot))
	if _, err := trySave(app, schema.BackupChecks, restoreTest(f, job, slot)); err == nil {
		t.Error("a job has two restore tests in one slot")
	}
	if _, err := trySave(app, schema.BackupChecks, integrityCheck(f, slot)); err == nil {
		t.Error("a target has two integrity checks in one slot")
	}
	if test.GetInt("tables_expected") != 12 || test.GetInt("dump_bytes") != 1<<34 || test.GetString("failure") != backup.TestTables {
		t.Errorf("the test does not keep what it was given: %v", test)
	}
	for field, value := range map[string]any{"failure": strings.Repeat("f", 33), "temporary": strings.Repeat("t", 64), "snapshot_id": strings.Repeat("3", 65)} {
		bad := restoreTest(f, job, types.DateTime{})
		bad[field] = value
		if _, err := trySave(app, schema.BackupChecks, bad); err == nil {
			t.Errorf("a test took a %s of %d characters", field, len(value.(string)))
		}
	}

	restore := saveRecord(t, app, schema.BackupRestores, databaseRestore(f, job))
	download := databaseRestore(f, job)
	download["mode"], download["download_kind"], download["download_name"] = schema.RestoreDownload, schema.DownloadSQL, "shop-20261118-0130.sql"
	saveRecord(t, app, schema.BackupRestores, download)
	newDatabase := databaseRestore(f, job)
	newDatabase["mode"], newDatabase["database_name"], newDatabase["previous"] = schema.RestoreNewDatabase, "shop_restored", ""
	saveRecord(t, app, schema.BackupRestores, newDatabase)
	var roots []restoreRoot
	if err := restore.UnmarshalJSONField("roots", &roots); err != nil || len(roots) != 1 || roots[0].Source != "/shop.pgdump" {
		t.Errorf("the restore's roots: %+v, %v", roots, err)
	}

	host := f.host
	host.Set("auto_backup_schedule", "15 1 * * *")
	host.Set("auto_backup_targets", []string{f.target.Id, other.target.Id})
	host.Set("auto_backup_retention", map[string]int{"daily": 14})
	if err := app.Save(host); err != nil {
		t.Fatalf("a server does not keep its automatic backups: %v", err)
	}
	ids := []string{}
	for i := range 11 {
		ids = append(ids, seedCheckFixture(t, app, "many"+string(rune('a'+i))).target.Id)
	}
	host.Set("auto_backup_targets", ids)
	if err := app.Save(host); err == nil {
		t.Error("a server backs up to 11 targets by default")
	}

	// the database goes: its job, its restores and its tests stay, without it
	if err := app.Delete(f.database); err != nil {
		t.Fatal(err)
	}
	if kept, err := app.FindRecordById(schema.BackupJobs, job.Id); err != nil || kept.GetString("database") != "" || kept.GetString("db_server_name") != f.server.GetString("name") {
		t.Errorf("the job of a dropped database: %v, %v", kept, err)
	}
	if kept, err := app.FindRecordById(schema.BackupRestores, restore.Id); err != nil || kept.GetString("database") != "" || kept.GetString("database_name") != "shop" {
		t.Errorf("the restore of a dropped database: %v, %v", kept, err)
	}
	if err := app.Delete(f.site); err != nil {
		t.Fatal(err)
	}
	if kept, err := app.FindRecordById(schema.BackupJobs, files.Id); err != nil || kept.GetString("site") != "" {
		t.Errorf("the job of a removed site: %v, %v", kept, err)
	}
	// the job goes, and its tests with it; the integrity check stays
	if err := app.Delete(job); err != nil {
		t.Fatal(err)
	}
	if _, err := app.FindRecordById(schema.BackupChecks, test.Id); err == nil {
		t.Error("a restore test outlives its job")
	}
	if checks, err := app.FindRecordsByFilter(schema.BackupChecks, "kind = 'integrity'", "", 0, 0); err != nil || len(checks) != 1 {
		t.Errorf("the integrity checks: %v, %v", checks, err)
	}
}

// TestTheDatabaseBackupsMigrationAppliesOverRecovery: a panel migrated
// through #32's migration, with its jobs, checks and settings, takes the
// migration and keeps its records; reverting it removes the automatic jobs
// with their runs, restores and tests, the restores into databases and the
// SQL downloads, every restore test, the fields and the indexes, puts the
// slot of before back, and keeps the rest; it applies again after that.
func TestTheDatabaseBackupsMigrationAppliesOverRecovery(t *testing.T) {
	app := bootMigrationPanel(t)
	if _, err := core.NewMigrationsRunner(app, migrationsBefore(t, databaseBackupsMigration)).Up(); err != nil {
		t.Fatalf("the migrations through 1772000000 fail: %v", err)
	}
	if _, err := app.FindCollectionByNameOrId(schema.RecoveryKits); err != nil {
		t.Fatalf("#32's migration did not run: %v", err)
	}
	f := seedDatabaseFixture(t, app, "old")
	if _, err := trySave(app, schema.BackupJobs, databaseJob(f)); err == nil {
		t.Fatal("a job backs up a database before the migration")
	}
	slot := types.NowDateTime().Add(-time.Hour)
	oldCheck := saveRecord(t, app, schema.BackupChecks, integrityCheck(f, slot))
	settings, err := app.FindAllRecords(schema.Settings)
	if err != nil || len(settings) != 1 {
		t.Fatalf("settings: %v, %v", settings, err)
	}
	checkSchedule := settings[0].GetString("backup_check_schedule")

	// the panel's next start applies only the new migration
	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("the migration fails over #32's: %v", err)
	}
	checkDatabaseBackupsSchema(t, app)
	checkRecoverySchema(t, app)
	checkChecksSchema(t, app)
	checkRestoresSchema(t, app)
	checkRestoreTestDefault(t, app)
	if after, err := app.FindAllRecords(schema.Settings); err != nil || after[0].GetString("backup_check_schedule") != checkSchedule {
		t.Errorf("the checks' settings changed: %v, %v", after, err)
	}
	job, err := app.FindRecordById(schema.BackupJobs, f.job.Id)
	if err != nil || job.GetString("origin") != schema.JobManual || job.GetString("database") != "" || len(job.GetStringSlice("own_settings")) != 0 ||
		!job.GetDateTime("last_success_at").IsZero() {
		t.Errorf("a job of before: %v, %v", job, err)
	}
	if check, err := app.FindRecordById(schema.BackupChecks, oldCheck.Id); err != nil || check.GetString("job") != "" || check.GetString("kind") != schema.CheckIntegrity {
		t.Errorf("a check of before: %v, %v", check, err)
	}
	host, err := app.FindRecordById(schema.Hosts, f.host.Id)
	if err != nil || host.GetString("auto_backup_schedule") != "" || len(host.GetStringSlice("auto_backup_targets")) != 0 {
		t.Errorf("a server of before: %v, %v", host, err)
	}

	// an automatic job of the database with a run, a restore and a test that
	// shares the integrity check's slot; a restore test of the job of before;
	// a site's job; an SQL download and a restore of the job of before
	auto := saveRecord(t, app, schema.BackupJobs, databaseJob(f))
	autoRun := saveRecord(t, app, schema.BackupRuns, map[string]any{"job": auto.Id, "target": f.target.Id, "host": f.host.Id,
		"trigger": schema.RunManual, "status": schema.RunSucceeded, "snapshot_id": strings.Repeat("3f", 32), "bytes_stored": 100})
	autoRestore := saveRecord(t, app, schema.BackupRestores, databaseRestore(f, auto))
	autoTest := saveRecord(t, app, schema.BackupChecks, restoreTest(f, auto, slot))
	manualTest := saveRecord(t, app, schema.BackupChecks, restoreTest(f, f.job, slot))
	files := saveRecord(t, app, schema.BackupJobs, siteJob(f))
	download := databaseRestore(f, f.job)
	download["mode"], download["download_kind"] = schema.RestoreDownload, schema.DownloadSQL
	sqlDownload := saveRecord(t, app, schema.BackupRestores, download)
	tarDownload := databaseRestore(f, f.job)
	tarDownload["mode"], tarDownload["download_kind"], tarDownload["database"] = schema.RestoreDownload, schema.DownloadDir, ""
	keptDownload := saveRecord(t, app, schema.BackupRestores, tarDownload)
	host.Set("auto_backup_schedule", "15 1 * * *")
	host.Set("auto_backup_targets", []string{f.target.Id})
	if err := app.Save(host); err != nil {
		t.Fatal(err)
	}

	later := migrationsFrom(t, databaseBackupsMigration)
	reverted, err := core.NewMigrationsRunner(app, core.AppMigrations).Down(len(later))
	if err != nil || !slices.Equal(reverted, later) {
		t.Fatalf("reverting: %v, %v", reverted, err)
	}
	for collection, gone := range map[string][]*core.Record{
		schema.BackupJobs:     {auto, files},
		schema.BackupRuns:     {autoRun},
		schema.BackupRestores: {autoRestore, sqlDownload},
		schema.BackupChecks:   {autoTest, manualTest},
	} {
		for _, record := range gone {
			if _, err := app.FindRecordById(collection, record.Id); err == nil {
				t.Errorf("%s %s stays after the revert", collection, record.Id)
			}
		}
	}
	for collection, kept := range map[string][]*core.Record{
		schema.BackupJobs:     {f.job},
		schema.BackupRuns:     {f.run},
		schema.BackupRestores: {keptDownload},
		schema.BackupChecks:   {oldCheck},
		schema.Databases:      {f.database},
		schema.Sites:          {f.site},
	} {
		for _, record := range kept {
			if _, err := app.FindRecordById(collection, record.Id); err != nil {
				t.Errorf("the revert removed %s %s: %v", collection, record.Id, err)
			}
		}
	}
	for collection, fields := range map[string][]string{
		schema.BackupJobs:     {"database", "site", "db_server_name", "own_settings", "last_success_at"},
		schema.BackupRuns:     {"bytes_stored"},
		schema.BackupRestores: laterFields[schema.BackupRestores],
		schema.BackupChecks:   laterFields[schema.BackupChecks],
		schema.Hosts:          {"auto_backup_schedule", "auto_backup_targets", "auto_backup_retention"},
		schema.Settings:       {"restore_test_schedule"},
	} {
		c, err := app.FindCollectionByNameOrId(collection)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range fields {
			if c.Fields.GetByName(name) != nil {
				t.Errorf("%s.%s stays after the revert", collection, name)
			}
		}
		for _, index := range c.Indexes {
			if strings.Contains(index, "idx_backup_jobs_database") || strings.Contains(index, "idx_backup_jobs_site") || strings.Contains(index, "idx_backup_checks_job") {
				t.Errorf("the index %s stays after the revert", index)
			}
		}
	}
	// the collections are as #32's migration left them, with the slot of #31
	checkRecoverySchema(t, app)
	checkChecksSchema(t, app)
	checkRestoresSchema(t, app)
	checkIndexes(t, app, schema.BackupChecks, "UNIQUE INDEX idx_backup_checks_slot ON backup_checks (target, scheduled_at) WHERE scheduled_at != ''")
	jobs, _ := app.FindCollectionByNameOrId(schema.BackupJobs)
	if field := jobs.Fields.GetByName("origin").(*core.SelectField); !slices.Equal(field.Values, []string{schema.JobManual, schema.JobLabel}) {
		t.Errorf("a job's origin takes %v after the revert", field.Values)
	}
	if field := jobs.Fields.GetByName("source_kind").(*core.SelectField); !slices.Equal(field.Values, []string{schema.SourceVolume, schema.SourcePath, schema.SourceStack, schema.SourceDump, schema.SourcePanel}) {
		t.Errorf("a job's source_kind takes %v after the revert", field.Values)
	}
	restores, _ := app.FindCollectionByNameOrId(schema.BackupRestores)
	for name, values := range map[string][]string{
		"mode":          {schema.RestoreOriginal, schema.RestorePath, schema.RestoreDownload},
		"download_kind": {schema.DownloadDir, schema.DownloadFile},
	} {
		if field := restores.Fields.GetByName(name).(*core.SelectField); !slices.Equal(field.Values, values) {
			t.Errorf("a restore's %s takes %v after the revert", name, field.Values)
		}
	}
	// the slot of before holds
	if _, err := trySave(app, schema.BackupChecks, integrityCheck(f, slot)); err == nil {
		t.Error("a target has two checks in one slot after the revert")
	}

	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("the migration does not apply again: %v", err)
	}
	checkDatabaseBackupsSchema(t, app)
	checkRestoreTestDefault(t, app)
	saveRecord(t, app, schema.BackupJobs, databaseJob(f))
	if host, err := app.FindRecordById(schema.Hosts, f.host.Id); err != nil || host.GetString("auto_backup_schedule") != "" {
		t.Errorf("a server after it applied again: %v, %v", host, err)
	}
}
