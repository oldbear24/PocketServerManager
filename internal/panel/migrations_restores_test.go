package panel

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// restoresMigration is the file of the migration that adds backup_restores.
const restoresMigration = "1770000000_backup_restores.go"

// bootMigrationPanel starts a panel on an empty data dir, as `panel serve`
// does, before any migration ran: the panel's hooks see each migration.
func bootMigrationPanel(t *testing.T) *core.BaseApp {
	t.Helper()
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: t.TempDir(), EncryptionEnv: "pb_test_env"})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.ClearBootstrap(); err != nil {
			t.Error(err)
		}
	})
	p := New(app, &Config{MasterKey: testutil.MasterKey, GRPCAddr: "127.0.0.1:0"}, testUI)
	p.Register()
	// what the hooks would do later, such as the removal of a site's files
	// that a migration reverted, ends before the app is torn down, as the
	// panel's stop does it (cleanups run last first)
	t.Cleanup(p.later.stop)
	// as the panel's start does, for the targets' sealed fields
	if err := p.loadMasterKey(); err != nil {
		t.Fatal(err)
	}
	return app
}

// migrationsFrom lists the files of the panel's migrations from file on,
// newest first: those that reverting down to file reverts, in their order.
func migrationsFrom(t *testing.T, file string) []string {
	t.Helper()
	var files []string
	for _, migration := range core.AppMigrations.Items() {
		if migration.File >= file {
			files = append(files, migration.File)
		}
	}
	slices.Sort(files)
	slices.Reverse(files)
	if len(files) == 0 || files[len(files)-1] != file {
		t.Fatalf("no migration %s", file)
	}
	return files
}

// migrationsBefore lists PocketBase's migrations and the panel's, without
// the panel's from file on: those of a panel of before.
func migrationsBefore(t *testing.T, file string) core.MigrationsList {
	t.Helper()
	list := core.MigrationsList{}
	list.Copy(core.SystemMigrations)
	found := false
	for _, migration := range core.AppMigrations.Items() {
		if migration.File >= file {
			found = found || migration.File == file
			continue
		}
		list.Register(migration.Up, migration.Down, migration.File)
	}
	if !found {
		t.Fatalf("no migration %s", file)
	}
	return list
}

// checkRestoresSchema checks backup_restores and the fields the migration
// adds to commands, as the panel's parts of #30 rely on them.
func checkRestoresSchema(t *testing.T, app core.App) {
	t.Helper()
	restores, err := app.FindCollectionByNameOrId(schema.BackupRestores)
	if err != nil {
		t.Fatalf("no backup_restores: %v", err)
	}

	// operators read it; nothing writes it through the API
	if restores.ListRule == nil || *restores.ListRule != schema.RuleOperator || restores.ViewRule == nil || *restores.ViewRule != schema.RuleOperator {
		t.Errorf("read rules: %v, %v", restores.ListRule, restores.ViewRule)
	}
	if restores.CreateRule != nil || restores.UpdateRule != nil || restores.DeleteRule != nil {
		t.Error("the API may write backup_restores")
	}

	want := map[string]string{
		"job": core.FieldTypeRelation, "target": core.FieldTypeRelation, "host": core.FieldTypeRelation,
		"label": core.FieldTypeText, "snapshot_id": core.FieldTypeText, "snapshot_time": core.FieldTypeDate,
		"mode": core.FieldTypeSelect, "roots": core.FieldTypeJSON, "path": core.FieldTypeText, "source": core.FieldTypeText,
		"include": core.FieldTypeJSON, "replace": core.FieldTypeBool, "overwrite": core.FieldTypeBool,
		"stack_name": core.FieldTypeText, "status": core.FieldTypeSelect, "preview": core.FieldTypeJSON,
		"preview_at": core.FieldTypeDate, "started_at": core.FieldTypeDate, "finished_at": core.FieldTypeDate, "expires_at": core.FieldTypeDate,
		"preview_command": core.FieldTypeRelation, "command": core.FieldTypeRelation,
		"requested_by": core.FieldTypeRelation, "confirmed_by": core.FieldTypeRelation,
		"requested_label": core.FieldTypeText, "confirmed_label": core.FieldTypeText, "stop_requested_label": core.FieldTypeText,
		"files_restored": core.FieldTypeNumber, "bytes_restored": core.FieldTypeNumber, "files_deleted": core.FieldTypeNumber,
		"errors": core.FieldTypeNumber, "bytes": core.FieldTypeNumber, "partial": core.FieldTypeBool,
		"app_stopped": core.FieldTypeJSON, "app_problem": core.FieldTypeText, "error": core.FieldTypeText, "log": core.FieldTypeText,
		"download_name": core.FieldTypeText, "download_kind": core.FieldTypeSelect,
	}
	for name, kind := range want {
		field := restores.Fields.GetByName(name)
		if field == nil {
			t.Errorf("backup_restores has no %s", name)
			continue
		}
		if field.Type() != kind {
			t.Errorf("%s is a %s, not a %s", name, field.Type(), kind)
		}
	}
	for _, field := range restores.Fields {
		if _, ok := want[field.GetName()]; !ok && !slices.Contains([]string{"id", "created", "updated"}, field.GetName()) &&
			!slices.Contains(laterFields[schema.BackupRestores], field.GetName()) {
			t.Errorf("backup_restores has a field %s no part expects", field.GetName())
		}
	}

	// the job, the target and the server take their restores with them
	for _, name := range []string{"job", "target", "host"} {
		if relation, ok := restores.Fields.GetByName(name).(*core.RelationField); !ok || !relation.CascadeDelete || !relation.Required {
			t.Errorf("%s is not a required relation removed with its record", name)
		}
	}
	// #30's values first; #53's migration adds its own after them
	for name, values := range map[string][]string{
		"status":        schema.RestoreStates,
		"mode":          {schema.RestoreOriginal, schema.RestorePath, schema.RestoreDownload},
		"download_kind": {schema.DownloadDir, schema.DownloadFile},
	} {
		if field, ok := restores.Fields.GetByName(name).(*core.SelectField); !ok || len(field.Values) < len(values) || !slices.Equal(field.Values[:len(values)], values) {
			t.Errorf("%s takes %v", name, restores.Fields.GetByName(name))
		}
	}
	for _, index := range []string{"(host, status)", "(job, created)", "(command)", "(preview_command)"} {
		if !slices.ContainsFunc(restores.Indexes, func(each string) bool { return strings.Contains(strings.ReplaceAll(each, "`", ""), index) }) {
			t.Errorf("no index on %s: %v", index, restores.Indexes)
		}
	}

	commands, err := app.FindCollectionByNameOrId(schema.Commands)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := commands.Fields.GetByName("cancel_requested_at").(*core.DateField); !ok {
		t.Error("commands has no date cancel_requested_at")
	}
	if field, ok := commands.Fields.GetByName("cancel_reason").(*core.TextField); !ok || field.Max != 255 {
		t.Error("commands has no text cancel_reason of 255 characters")
	}
}

// restoreFixture is what a restore refers to.
type restoreFixture struct {
	host, target, job, command, user *core.Record
}

func seedRestoreFixture(t *testing.T, app core.App, suffix string) restoreFixture {
	t.Helper()
	host := testutil.NewHost(t, app, "node-"+suffix)
	target := saveRecord(t, app, schema.BackupTargets, map[string]any{"name": "b2-" + suffix, "kind": "local", "host": host.Id,
		"config": map[string]any{"path": "/srv/backups"}, "credentials": "sealed", "repo_password": "sealed", "status": schema.TargetOK})
	job := saveRecord(t, app, schema.BackupJobs, map[string]any{"name": "vaultwarden-" + suffix, "host": host.Id, "source_kind": schema.SourceVolume,
		"source": "vw_data", "targets": []string{target.Id}, "schedule": "@daily", "enabled": true, "origin": schema.JobManual})
	saveRecord(t, app, schema.BackupRuns, map[string]any{"job": job.Id, "target": target.Id, "host": host.Id, "trigger": schema.RunManual,
		"status": schema.RunSucceeded, "snapshot_id": strings.Repeat("3f", 32)})
	command := saveRecord(t, app, schema.Commands, map[string]any{"type": "backup.run", "host": host.Id, "status": schema.CommandRunning,
		"payload": map[string]any{"run_id": "r1"}, "progress": map[string]any{"step": "backup", "percent": 40}})
	user, _ := testutil.NewUser(t, app, "operator-"+suffix+"@example.com", schema.RoleOperator)
	return restoreFixture{host: host, target: target, job: job, command: command, user: user}
}

// saveFullRestore saves a restore with every field set, as the panel's
// routes and finishers write them.
func saveFullRestore(t *testing.T, app core.App, f restoreFixture) *core.Record {
	t.Helper()
	now := types.NowDateTime()
	return saveRecord(t, app, schema.BackupRestores, map[string]any{
		"job": f.job.Id, "target": f.target.Id, "host": f.host.Id,
		"label":       "vaultwarden ← b2 · 3f3f3f3f → original",
		"snapshot_id": strings.Repeat("3f", 32), "snapshot_time": now,
		"mode":  schema.RestoreOriginal,
		"roots": []map[string]any{{"source": "/volumes/vw_data", "volume": "vw_data"}},
		"path":  "", "source": "", "include": []string{"/volumes/vw_data/db.sqlite3"},
		"replace": false, "overwrite": true, "stack_name": "vaultwarden", "status": schema.RestoreSucceeded,
		"preview":    map[string]any{"dry_run": true, "counts": map[string]any{"new": 1, "updated": 2, "unchanged": 3, "deleted": 0}},
		"preview_at": now, "started_at": now, "finished_at": now, "expires_at": now.Add(30 * time.Minute),
		"preview_command": "", "command": f.command.Id,
		"requested_by": f.user.Id, "confirmed_by": f.user.Id,
		"requested_label": f.user.Email(), "confirmed_label": f.user.Email(), "stop_requested_label": "",
		"files_restored": 3, "bytes_restored": 1 << 40, "files_deleted": 0, "errors": 0, "bytes": 0, "partial": false,
		"app_stopped": []map[string]any{{"id": "c1", "name": "vaultwarden-app-1"}},
		"app_problem": "", "error": "", "log": strings.Repeat("x", 32<<10),
		"download_name": "vaultwarden-3f3f3f3f-20261110-1200.tar", "download_kind": schema.DownloadDir,
	})
}

// TestTheRestoresMigrationAppliesToAFreshDatabase: a new panel gets
// backup_restores with the fields that #30's agent, panel and web parts code
// against, and commands get their cancel fields.
func TestTheRestoresMigrationAppliesToAFreshDatabase(t *testing.T) {
	app := bootMigrationPanel(t)
	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("the migrations of a fresh install fail: %v", err)
	}
	checkRestoresSchema(t, app)

	f := seedRestoreFixture(t, app, "fresh")
	restore := saveFullRestore(t, app, f)
	if restore.GetInt("bytes_restored") != 1<<40 || len(restore.GetString("log")) != 32<<10 {
		t.Errorf("the record does not keep what it was given: %v", restore)
	}

	f.command.Set("cancel_requested_at", types.NowDateTime())
	f.command.Set("cancel_reason", "operator-fresh@example.com")
	if err := app.Save(f.command); err != nil {
		t.Fatalf("a command does not take its cancel fields: %v", err)
	}
}

// TestTheRestoresMigrationAppliesOverBackupJobs: a panel migrated through
// #29's migration, with its jobs, runs and commands, takes the migration and
// keeps its records; reverting it removes the collection and the fields and
// keeps the rest; it applies again after that.
func TestTheRestoresMigrationAppliesOverBackupJobs(t *testing.T) {
	app := bootMigrationPanel(t)
	if _, err := core.NewMigrationsRunner(app, migrationsBefore(t, restoresMigration)).Up(); err != nil {
		t.Fatalf("the migrations through 1769000000 fail: %v", err)
	}
	if _, err := app.FindCollectionByNameOrId(schema.BackupJobs); err != nil {
		t.Fatalf("#29's migration did not run: %v", err)
	}
	if _, err := app.FindCollectionByNameOrId(schema.BackupRestores); err == nil {
		t.Fatal("backup_restores is there before its migration")
	}
	f := seedRestoreFixture(t, app, "old")

	// the panel's next start applies only the new migration
	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("the migration fails over #29's: %v", err)
	}
	checkRestoresSchema(t, app)
	command, err := app.FindRecordById(schema.Commands, f.command.Id)
	if err != nil {
		t.Fatal(err)
	}
	if command.GetString("status") != f.command.GetString("status") || command.GetString("payload") != f.command.GetString("payload") ||
		command.GetString("progress") != f.command.GetString("progress") ||
		!command.GetDateTime("cancel_requested_at").IsZero() || command.GetString("cancel_reason") != "" {
		t.Errorf("a command of before changed: %v", command)
	}
	if _, err := app.FindRecordById(schema.BackupJobs, f.job.Id); err != nil {
		t.Errorf("the job is gone: %v", err)
	}

	// a restore of the job of before, and one of each that goes with its record
	saveFullRestore(t, app, f)
	for _, gone := range []string{"job", "target", "host"} {
		other := seedRestoreFixture(t, app, "gone-"+gone)
		if gone == "host" {
			// a restore to a folder of another server, which is removed
			other.host = testutil.NewHost(t, app, "destination")
		}
		restore := saveFullRestore(t, app, other)
		record := map[string]*core.Record{"job": other.job, "target": other.target, "host": other.host}[gone]
		if err := app.Delete(record); err != nil {
			t.Fatalf("removing the %s: %v", gone, err)
		}
		if _, err := app.FindRecordById(schema.BackupRestores, restore.Id); err == nil {
			t.Errorf("a restore stays when its %s is removed", gone)
		}
	}
	// a restore outlives its command, which the panel removes after a preview
	if err := app.Delete(command); err != nil {
		t.Fatal(err)
	}
	if restores, err := app.FindRecordsByFilter(schema.BackupRestores, "job = {:job}", "", 0, 0, map[string]any{"job": f.job.Id}); err != nil || len(restores) != 1 || restores[0].GetString("command") != "" {
		t.Errorf("the restore of a removed command: %v, %v", restores, err)
	}

	// the migrations after it go first
	later := migrationsFrom(t, restoresMigration)
	reverted, err := core.NewMigrationsRunner(app, core.AppMigrations).Down(len(later))
	if err != nil || !slices.Equal(reverted, later) {
		t.Fatalf("reverting: %v, %v", reverted, err)
	}
	if _, err := app.FindCollectionByNameOrId(schema.BackupRestores); err == nil {
		t.Error("backup_restores stays after the revert")
	}
	commands, err := app.FindCollectionByNameOrId(schema.Commands)
	if err != nil {
		t.Fatal(err)
	}
	if commands.Fields.GetByName("cancel_requested_at") != nil || commands.Fields.GetByName("cancel_reason") != nil {
		t.Error("the commands' cancel fields stay after the revert")
	}
	if _, err := app.FindRecordById(schema.BackupJobs, f.job.Id); err != nil {
		t.Errorf("the revert removed the job: %v", err)
	}

	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("the migration does not apply again: %v", err)
	}
	checkRestoresSchema(t, app)
}
