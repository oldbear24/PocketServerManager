package panel

import (
	"slices"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// checkFieldTypes checks that a collection has exactly the fields want, of
// their types, besides id, created and updated, or, with only, that it has
// these among others.
func checkFieldTypes(t *testing.T, collection *core.Collection, want map[string]string, exact bool) {
	t.Helper()
	for name, kind := range want {
		field := collection.Fields.GetByName(name)
		if field == nil {
			t.Errorf("%s has no %s", collection.Name, name)
			continue
		}
		if field.Type() != kind {
			t.Errorf("%s.%s is a %s, not a %s", collection.Name, name, field.Type(), kind)
		}
	}
	if !exact {
		return
	}
	for _, field := range collection.Fields {
		if _, ok := want[field.GetName()]; !ok && !slices.Contains([]string{"id", "created", "updated"}, field.GetName()) {
			t.Errorf("%s has a field %s no part expects", collection.Name, field.GetName())
		}
	}
}

// checkIndexes checks that each index, as SQLite has it, is there.
func checkIndexes(t *testing.T, app core.App, table string, want ...string) {
	t.Helper()
	var sqls []string
	if err := app.DB().NewQuery("SELECT sql FROM sqlite_master WHERE type = 'index' AND tbl_name = {:table} AND sql IS NOT NULL").
		Bind(map[string]any{"table": table}).Column(&sqls); err != nil {
		t.Fatal(err)
	}
	for i, sql := range sqls {
		sql = strings.Join(strings.Fields(strings.ReplaceAll(sql, "`", "")), " ")
		sqls[i] = strings.ReplaceAll(strings.ReplaceAll(sql, "( ", "("), " )", ")")
	}
	for _, index := range want {
		if !slices.ContainsFunc(sqls, func(sql string) bool { return strings.Contains(sql, index) }) {
			t.Errorf("%s has no index %s: %v", table, index, sqls)
		}
	}
}

func textField(t *testing.T, collection *core.Collection, name string) *core.TextField {
	t.Helper()
	field, ok := collection.Fields.GetByName(name).(*core.TextField)
	if !ok {
		t.Fatalf("%s.%s is not text", collection.Name, name)
	}
	return field
}

// checkChecksSchema checks backup_checks and the fields the migration adds
// to settings, backup_targets and backup_runs, as #31's agent, panel and web
// parts code against them.
func checkChecksSchema(t *testing.T, app core.App) {
	t.Helper()
	checks, err := app.FindCollectionByNameOrId(schema.BackupChecks)
	if err != nil {
		t.Fatalf("no backup_checks: %v", err)
	}
	// viewers read the checks; nothing writes them through the API
	if checks.ListRule == nil || *checks.ListRule != schema.RuleViewer || checks.ViewRule == nil || *checks.ViewRule != schema.RuleViewer {
		t.Errorf("read rules: %v, %v", checks.ListRule, checks.ViewRule)
	}
	if checks.CreateRule != nil || checks.UpdateRule != nil || checks.DeleteRule != nil {
		t.Error("the API may write backup_checks")
	}
	checkFieldTypes(t, checks, map[string]string{
		"target": core.FieldTypeRelation, "host": core.FieldTypeRelation, "kind": core.FieldTypeSelect,
		"trigger": core.FieldTypeSelect, "status": core.FieldTypeSelect, "scheduled_at": core.FieldTypeDate,
		"started_at": core.FieldTypeDate, "finished_at": core.FieldTypeDate, "duration_ms": core.FieldTypeNumber,
		"subset": core.FieldTypeText, "packs_read": core.FieldTypeNumber, "packs_total": core.FieldTypeNumber,
		"confirmed": core.FieldTypeBool, "unconfirmed": core.FieldTypeBool, "errors": core.FieldTypeJSON,
		"errors_more": core.FieldTypeNumber, "repair": core.FieldTypeJSON, "notes": core.FieldTypeJSON,
		"lock": core.FieldTypeJSON, "error": core.FieldTypeText, "log": core.FieldTypeText,
		"command": core.FieldTypeRelation, "attempt": core.FieldTypeNumber, "retry_of": core.FieldTypeText,
		"retry_at": core.FieldTypeDate,
	}, true)
	if relation, ok := checks.Fields.GetByName("target").(*core.RelationField); !ok || !relation.Required || !relation.CascadeDelete {
		t.Error("target is not a required relation removed with its target")
	}
	if relation, ok := checks.Fields.GetByName("host").(*core.RelationField); !ok || !relation.CascadeDelete {
		t.Error("host is not removed with its server")
	}
	if relation, ok := checks.Fields.GetByName("command").(*core.RelationField); !ok || relation.CascadeDelete || relation.Required {
		t.Error("a check does not outlive its command")
	}
	for name, values := range map[string][]string{
		"kind":    {schema.CheckIntegrity, schema.CheckRestoreTest},
		"trigger": {schema.RunSchedule, schema.RunManual, schema.RunRetry},
		"status":  schema.CheckStates,
	} {
		if field, ok := checks.Fields.GetByName(name).(*core.SelectField); !ok || !field.Required || !slices.Equal(field.Values, values) {
			t.Errorf("%s takes %v", name, checks.Fields.GetByName(name))
		}
	}
	for name, size := range map[string]int64{"errors": 16 << 10, "repair": 4 << 10, "notes": 4 << 10, "lock": 2 << 10} {
		if field := checks.Fields.GetByName(name).(*core.JSONField); field.MaxSize != size {
			t.Errorf("%s holds %d bytes, not %d", name, field.MaxSize, size)
		}
	}
	for name, size := range map[string]int{"subset": 16, "error": 4096, "log": 32 << 10, "retry_of": 15} {
		if field := textField(t, checks, name); field.Max != size {
			t.Errorf("%s holds %d characters, not %d", name, field.Max, size)
		}
	}
	if textField(t, checks, "retry_of").Pattern != "^[a-z0-9]*$" {
		t.Error("retry_of takes more than an id")
	}
	checkIndexes(t, app, schema.BackupChecks,
		"INDEX idx_backup_checks_target ON backup_checks (target, created)",
		"INDEX idx_backup_checks_command ON backup_checks (command)",
		"UNIQUE INDEX idx_backup_checks_slot ON backup_checks (target, scheduled_at) WHERE scheduled_at != ''",
		"UNIQUE INDEX idx_backup_checks_retry_of ON backup_checks (retry_of) WHERE retry_of != ''",
		"INDEX idx_backup_checks_retry_at ON backup_checks (status, retry_at)",
	)

	targets, err := app.FindCollectionByNameOrId(schema.BackupTargets)
	if err != nil {
		t.Fatal(err)
	}
	checkFieldTypes(t, targets, map[string]string{
		"integrity_status": core.FieldTypeSelect, "integrity_at": core.FieldTypeDate, "integrity_error": core.FieldTypeText,
		"last_check": core.FieldTypeText, "last_check_status": core.FieldTypeSelect, "last_check_error": core.FieldTypeText,
		"next_check_at": core.FieldTypeDate, "integrity_group": core.FieldTypeNumber,
	}, false)
	if field := targets.Fields.GetByName("integrity_status").(*core.SelectField); !slices.Equal(field.Values, []string{schema.CheckOK, schema.CheckFailed}) || field.Required {
		t.Errorf("integrity_status takes %v", field.Values)
	}
	if field := targets.Fields.GetByName("last_check_status").(*core.SelectField); !slices.Equal(field.Values, schema.CheckStates) || field.Required {
		t.Errorf("last_check_status takes %v", field.Values)
	}
	if field := textField(t, targets, "last_check"); field.Max != 15 || field.Pattern != "^[a-z0-9]*$" {
		t.Errorf("last_check is %+v", field)
	}
	if textField(t, targets, "integrity_error").Max != 4096 || textField(t, targets, "last_check_error").Max != 1000 {
		t.Error("the targets' check errors are not 4096 and 1000 characters")
	}
	for _, audited := range schema.Audited {
		if audited.Name != schema.BackupTargets {
			continue
		}
		for _, name := range []string{"integrity_status", "integrity_at", "integrity_error", "last_check", "last_check_status", "last_check_error", "next_check_at", "integrity_group"} {
			if !slices.Contains(audited.Ignore, name) {
				t.Errorf("the audit records a target's %s", name)
			}
		}
	}

	runs, err := app.FindCollectionByNameOrId(schema.BackupRuns)
	if err != nil {
		t.Fatal(err)
	}
	checkFieldTypes(t, runs, map[string]string{
		"attempt": core.FieldTypeNumber, "retry_of": core.FieldTypeText, "retry_at": core.FieldTypeDate, "retry_end": core.FieldTypeSelect,
		"retry_note": core.FieldTypeText, "failure": core.FieldTypeSelect, "lock": core.FieldTypeJSON,
	}, false)
	if field := runs.Fields.GetByName("retry_end").(*core.SelectField); !slices.Equal(field.Values, []string{schema.RetryNextRun, schema.RetryGivenUp}) ||
		field.Required || field.MaxSelect != 1 || schema.RetryNextRun != "next_run" || schema.RetryGivenUp != "given_up" {
		t.Errorf("a run's retry_end takes %v", field.Values)
	}
	if field := runs.Fields.GetByName("trigger").(*core.SelectField); !slices.Equal(field.Values, []string{schema.RunSchedule, schema.RunManual, schema.RunRetry}) {
		t.Errorf("a run's trigger takes %v", field.Values)
	}
	if field := runs.Fields.GetByName("failure").(*core.SelectField); !slices.Equal(field.Values, backup.Failures) || field.Required {
		t.Errorf("a run's failure takes %v", field.Values)
	}
	if field := textField(t, runs, "retry_of"); field.Max != 15 || field.Pattern != "^[a-z0-9]*$" {
		t.Errorf("retry_of is %+v", field)
	}
	if textField(t, runs, "retry_note").Max != 500 || runs.Fields.GetByName("lock").(*core.JSONField).MaxSize != 2<<10 {
		t.Error("retry_note or lock is not of its size")
	}
	checkIndexes(t, app, schema.BackupRuns,
		"UNIQUE INDEX idx_backup_runs_retry_of ON backup_runs (retry_of) WHERE retry_of != ''",
		"INDEX idx_backup_runs_retry_at ON backup_runs (status, retry_at)",
	)

	settings, err := app.FindCollectionByNameOrId(schema.Settings)
	if err != nil {
		t.Fatal(err)
	}
	if field, ok := settings.Fields.GetByName("backup_check_schedule").(*core.TextField); !ok || field.Max != 100 {
		t.Error("settings has no backup_check_schedule of 100 characters")
	}
	if field, ok := settings.Fields.GetByName("backup_check_groups").(*core.NumberField); !ok || !field.OnlyInt {
		t.Error("settings has no whole number backup_check_groups")
	}
}

// checkFixture is what a check and a retried run refer to.
type checkFixture struct {
	host, target, job, run, command *core.Record
}

func seedCheckFixture(t *testing.T, app core.App, suffix string) checkFixture {
	t.Helper()
	host := testutil.NewHost(t, app, "node-"+suffix)
	target := saveRecord(t, app, schema.BackupTargets, map[string]any{"name": "minio-" + suffix, "kind": "local", "host": host.Id,
		"config": map[string]any{"path": "/srv/backups"}, "credentials": "sealed", "repo_password": "sealed", "status": schema.TargetOK})
	job := saveRecord(t, app, schema.BackupJobs, map[string]any{"name": "photos-" + suffix, "host": host.Id, "source_kind": schema.SourceVolume,
		"source": "photos", "targets": []string{target.Id}, "schedule": "0 3 * * *", "enabled": true, "origin": schema.JobManual})
	run := saveRecord(t, app, schema.BackupRuns, map[string]any{"job": job.Id, "target": target.Id, "host": host.Id, "trigger": schema.RunSchedule,
		"status": schema.RunFailed, "error": "unable to create lock in backend: repository is already locked"})
	command := saveRecord(t, app, schema.Commands, map[string]any{"type": "backup.check", "host": host.Id, "status": schema.CommandRunning,
		"payload": map[string]any{"repository": map[string]any{"target_id": target.Id}, "read_data_subset": "3/20"}})
	return checkFixture{host: host, target: target, job: job, run: run, command: command}
}

// fullCheck is a check with every field set, as the panel's finisher writes
// it from the agent's answer.
func fullCheck(f checkFixture) map[string]any {
	now := types.NowDateTime()
	lock := backup.Lock{PID: 1, Host: "pc-k3j9x2m4q8w1z7v", User: "root", Exclusive: true, StorageID: "957548b2",
		Command: "k3j9x2m4q8w1z7v", CommandType: "backup.run", CommandLabel: "Back up photos to minio", CommandHost: f.host.Id, CommandHostName: f.host.GetString("name")}
	return map[string]any{
		"target": f.target.Id, "host": f.host.Id, "kind": schema.CheckIntegrity, "trigger": schema.RunSchedule,
		"status": schema.CheckFailed, "scheduled_at": now, "started_at": now, "finished_at": now, "duration_ms": 4 * 3600 * 1000,
		"subset": "256/256", "packs_read": 118, "packs_total": 2361, "confirmed": true, "unconfirmed": false,
		"errors":      []backup.CheckError{{Kind: backup.CheckErrorPack, ID: strings.Repeat("b6", 32), Text: strings.Repeat("e", backup.MaxCheckErrorText)}},
		"errors_more": 3,
		"repair":      backup.CheckRepair{Packs: []string{strings.Repeat("b6", 32)}, Snapshots: true},
		"notes":       []string{"1 additional files were found in the repo, which likely contain duplicate data."},
		"lock":        lock, "error": strings.Repeat("x", 4096), "log": strings.Repeat("l", 32<<10),
		"command": f.command.Id, "attempt": 1, "retry_of": "", "retry_at": now,
	}
}

// retriedRun is a run that tries f's failed run again, with every field the
// migration adds set.
func retriedRun(f checkFixture) map[string]any {
	return map[string]any{"job": f.job.Id, "target": f.target.Id, "host": f.host.Id, "trigger": schema.RunRetry,
		"status": schema.RunFailed, "attempt": 2, "retry_of": f.run.Id, "retry_at": types.NowDateTime(),
		"retry_note": strings.Repeat("n", 500), "failure": backup.FailureLocked, "retry_end": schema.RetryGivenUp,
		"lock": backup.Lock{PID: 1, Host: "nas", User: "root", Exclusive: true}}
}

func checkSettingsDefaults(t *testing.T, app core.App) {
	t.Helper()
	records, err := app.FindAllRecords(schema.Settings)
	if err != nil || len(records) == 0 {
		t.Fatalf("settings: %v, %v", records, err)
	}
	for _, record := range records {
		if record.GetString("backup_check_schedule") != schema.DefaultCheckSchedule || record.GetInt("backup_check_groups") != schema.DefaultCheckGroups {
			t.Errorf("the checks' settings are %q and %d", record.GetString("backup_check_schedule"), record.GetInt("backup_check_groups"))
		}
	}
	if schema.DefaultCheckSchedule != "0 5 * * 0" || schema.DefaultCheckGroups != 20 {
		t.Error("the checks no longer run on Sundays at 05:00, one of 20 groups each")
	}
}

// TestTheChecksMigrationAppliesToAFreshDatabase: a new panel gets
// backup_checks, the targets' summary fields, the runs' retry fields and the
// checks' settings, with their defaults; each index keeps one check per slot
// and one retry per failed run or check.
func TestTheChecksMigrationAppliesToAFreshDatabase(t *testing.T) {
	app := bootMigrationPanel(t)
	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("the migrations of a fresh install fail: %v", err)
	}
	checkChecksSchema(t, app)
	checkSettingsDefaults(t, app)

	f := seedCheckFixture(t, app, "fresh")
	check := saveRecord(t, app, schema.BackupChecks, fullCheck(f))
	var lock backup.Lock
	if err := check.UnmarshalJSONField("lock", &lock); err != nil || lock.Command != "k3j9x2m4q8w1z7v" || !lock.Exclusive {
		t.Errorf("the check's lock reads back as %+v, %v", lock, err)
	}
	if check.GetInt("duration_ms") != 4*3600*1000 || len(check.GetString("log")) != 32<<10 {
		t.Errorf("the check does not keep what it was given: %v", check)
	}

	// one check per target and slot; checks with no slot are many
	slot := fullCheck(f)
	slot["scheduled_at"] = check.GetDateTime("scheduled_at")
	if _, err := trySave(app, schema.BackupChecks, slot); err == nil {
		t.Error("a second check of the same slot was saved")
	}
	for range 2 {
		manual := fullCheck(f)
		manual["trigger"], manual["scheduled_at"] = schema.RunManual, ""
		saveRecord(t, app, schema.BackupChecks, manual)
	}
	// one retry per check
	retry := fullCheck(f)
	retry["trigger"], retry["scheduled_at"], retry["retry_of"], retry["attempt"] = schema.RunRetry, "", check.Id, 2
	saveRecord(t, app, schema.BackupChecks, retry)
	if _, err := trySave(app, schema.BackupChecks, retry); err == nil {
		t.Error("a second retry of the same check was saved")
	}

	// a run tried again once; runs that try nothing again are many
	saveRecord(t, app, schema.BackupRuns, retriedRun(f))
	if _, err := trySave(app, schema.BackupRuns, retriedRun(f)); err == nil {
		t.Error("a second retry of the same run was saved")
	}
	for _, value := range []string{"retry_of", "failure", "retry_end"} {
		bad := retriedRun(f)
		bad["retry_of"] = ""
		bad[value] = "Not An Id!"
		if _, err := trySave(app, schema.BackupRuns, bad); err == nil {
			t.Errorf("a run took %s %q", value, bad[value])
		}
	}

	// the target keeps its summary
	f.target.Set("integrity_status", schema.CheckFailed)
	f.target.Set("integrity_at", types.NowDateTime())
	f.target.Set("integrity_error", "pack b659… contains 2 errors")
	f.target.Set("last_check", check.Id)
	f.target.Set("last_check_status", schema.CheckSuspect)
	f.target.Set("last_check_error", "the repository is locked by pc-k3j9x2m4q8w1z7v")
	f.target.Set("next_check_at", types.NowDateTime())
	f.target.Set("integrity_group", 3)
	if err := app.Save(f.target); err != nil {
		t.Fatalf("a target does not take its checks' summary: %v", err)
	}
	f.target.Set("integrity_status", schema.CheckSuspect)
	if err := app.Save(f.target); err == nil {
		t.Error("a target's integrity took suspect")
	}
}

// trySave saves a record and says the error.
func trySave(app core.App, name string, fields map[string]any) (*core.Record, error) {
	collection, err := app.FindCollectionByNameOrId(name)
	if err != nil {
		return nil, err
	}
	record := core.NewRecord(collection)
	for key, value := range fields {
		record.Set(key, value)
	}
	return record, app.Save(record)
}

// TestTheChecksMigrationAppliesOverRestores: a panel migrated through #30's
// migration, with its targets, runs and settings, takes the migration and
// keeps its records; reverting it turns retries into manual runs and
// removes the collection and the fields, and keeps the rest; it applies
// again after that.
func TestTheChecksMigrationAppliesOverRestores(t *testing.T) {
	app := bootMigrationPanel(t)
	if _, err := core.NewMigrationsRunner(app, migrationsBefore(t, checksMigration)).Up(); err != nil {
		t.Fatalf("the migrations through 1770000000 fail: %v", err)
	}
	if _, err := app.FindCollectionByNameOrId(schema.BackupRestores); err != nil {
		t.Fatalf("#30's migration did not run: %v", err)
	}
	if _, err := app.FindCollectionByNameOrId(schema.BackupChecks); err == nil {
		t.Fatal("backup_checks is there before its migration")
	}
	f := seedCheckFixture(t, app, "old")
	settings, err := app.FindAllRecords(schema.Settings)
	if err != nil || len(settings) != 1 {
		t.Fatalf("settings: %v, %v", settings, err)
	}
	retention := settings[0].GetInt("audit_retention_days")

	// the panel's next start applies only the new migration
	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("the migration fails over #30's: %v", err)
	}
	checkChecksSchema(t, app)
	checkSettingsDefaults(t, app)
	run, err := app.FindRecordById(schema.BackupRuns, f.run.Id)
	if err != nil {
		t.Fatal(err)
	}
	if run.GetString("trigger") != schema.RunSchedule || run.GetString("status") != schema.RunFailed || run.GetString("error") != f.run.GetString("error") ||
		run.GetInt("attempt") != 0 || run.GetString("retry_of") != "" || !run.GetDateTime("retry_at").IsZero() || run.GetString("failure") != "" {
		t.Errorf("a run of before changed: %v", run)
	}
	target, err := app.FindRecordById(schema.BackupTargets, f.target.Id)
	if err != nil {
		t.Fatal(err)
	}
	if target.GetString("status") != schema.TargetOK || target.GetString("integrity_status") != "" || target.GetString("last_check_status") != "" ||
		!target.GetDateTime("next_check_at").IsZero() || target.GetInt("integrity_group") != 0 {
		t.Errorf("a target of before: %v", target)
	}
	if after, err := app.FindAllRecords(schema.Settings); err != nil || after[0].GetInt("audit_retention_days") != retention {
		t.Errorf("the settings changed: %v, %v", after, err)
	}

	// a retry of the run of before; a check of the target of before, and one
	// that goes with its target, one with its server
	retry := saveRecord(t, app, schema.BackupRuns, retriedRun(f))
	check := saveRecord(t, app, schema.BackupChecks, fullCheck(f))
	for _, gone := range []string{"target", "host"} {
		other := seedCheckFixture(t, app, "gone-"+gone)
		if gone == "host" {
			// a server cannot go while a target of it stays: the check's
			// server is another one here
			other.host = testutil.NewHost(t, app, "checker")
		}
		otherCheck := saveRecord(t, app, schema.BackupChecks, fullCheck(other))
		if err := app.Delete(map[string]*core.Record{"target": other.target, "host": other.host}[gone]); err != nil {
			t.Fatalf("removing the %s: %v", gone, err)
		}
		if _, err := app.FindRecordById(schema.BackupChecks, otherCheck.Id); err == nil {
			t.Errorf("a check stays when its %s is removed", gone)
		}
	}
	// a check outlives its command
	if err := app.Delete(f.command); err != nil {
		t.Fatal(err)
	}
	if kept, err := app.FindRecordById(schema.BackupChecks, check.Id); err != nil || kept.GetString("command") != "" {
		t.Errorf("the check of a removed command: %v, %v", kept, err)
	}

	reverted, err := core.NewMigrationsRunner(app, core.AppMigrations).Down(1)
	if err != nil || !slices.Equal(reverted, []string{checksMigration}) {
		t.Fatalf("reverting: %v, %v", reverted, err)
	}
	if _, err := app.FindCollectionByNameOrId(schema.BackupChecks); err == nil {
		t.Error("backup_checks stays after the revert")
	}
	retry, err = app.FindRecordById(schema.BackupRuns, retry.Id)
	if err != nil || retry.GetString("trigger") != schema.RunManual || retry.GetString("status") != schema.RunFailed {
		t.Errorf("the retry after the revert: %v, %v", retry, err)
	}
	for collection, fields := range map[string][]string{
		schema.BackupRuns:    {"attempt", "retry_of", "retry_at", "retry_end", "retry_note", "failure", "lock"},
		schema.BackupTargets: {"integrity_status", "integrity_at", "integrity_error", "last_check", "last_check_status", "last_check_error", "next_check_at", "integrity_group"},
		schema.Settings:      {"backup_check_schedule", "backup_check_groups"},
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
		if collection == schema.BackupRuns {
			if field := c.Fields.GetByName("trigger").(*core.SelectField); !slices.Equal(field.Values, []string{schema.RunSchedule, schema.RunManual}) {
				t.Errorf("a run's trigger takes %v after the revert", field.Values)
			}
			for _, index := range c.Indexes {
				if strings.Contains(index, "retry") {
					t.Errorf("the index %s stays after the revert", index)
				}
			}
		}
	}
	if _, err := app.FindRecordById(schema.BackupTargets, f.target.Id); err != nil {
		t.Errorf("the revert removed the target: %v", err)
	}

	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("the migration does not apply again: %v", err)
	}
	checkChecksSchema(t, app)
	checkSettingsDefaults(t, app)
}
