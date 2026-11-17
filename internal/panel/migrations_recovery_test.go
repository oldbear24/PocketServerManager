package panel

import (
	"slices"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// recoveryMigration is the file of the migration that lets the panel back
// itself up (#32).
const recoveryMigration = "1772000000_panel_recovery.go"

// panelRow is the row of schema.PanelTable.
type panelRow struct {
	ID           int    `db:"id"`
	KeyID        string `db:"key_id"`
	CAPin        string `db:"ca_pin"`
	RestoredAt   string `db:"restored_at"`
	RestoredFrom string `db:"restored_from"`
}

// panelRows reads every row of schema.PanelTable, or says that it is not there.
func panelRows(app core.App) ([]panelRow, error) {
	var rows []panelRow
	err := app.DB().NewQuery("SELECT [[id]], [[key_id]], [[ca_pin]], [[restored_at]], [[restored_from]] FROM {{" + schema.PanelTable + "}}").All(&rows)
	return rows, err
}

// checkRecoverySchema checks what the migration adds and changes, as #32's
// agent, panel and web parts code against it.
func checkRecoverySchema(t *testing.T, app core.App) {
	t.Helper()
	jobs, err := app.FindCollectionByNameOrId(schema.BackupJobs)
	if err != nil {
		t.Fatal(err)
	}
	if field := jobs.Fields.GetByName("source_kind").(*core.SelectField); !field.Required ||
		!slices.Equal(field.Values, []string{schema.SourceVolume, schema.SourcePath, schema.SourceStack, schema.SourceDump, schema.SourcePanel}) {
		t.Errorf("a job's source_kind takes %v", field.Values)
	}
	if schema.SourcePanel != "panel" || schema.RunChange != "change" || schema.RecoveryKits != "recovery_kits" || schema.PanelTable != "_pc_panel" {
		t.Error("a name of the contract changed")
	}
	checkIndexes(t, app, schema.BackupJobs, "UNIQUE INDEX idx_backup_jobs_panel ON backup_jobs (source_kind) WHERE source_kind = 'panel'")

	runs, err := app.FindCollectionByNameOrId(schema.BackupRuns)
	if err != nil {
		t.Fatal(err)
	}
	if field := runs.Fields.GetByName("trigger").(*core.SelectField); !field.Required ||
		!slices.Equal(field.Values, []string{schema.RunSchedule, schema.RunManual, schema.RunRetry, schema.RunChange}) {
		t.Errorf("a run's trigger takes %v", field.Values)
	}
	checks, err := app.FindCollectionByNameOrId(schema.BackupChecks)
	if err != nil {
		t.Fatal(err)
	}
	if field := checks.Fields.GetByName("trigger").(*core.SelectField); !slices.Equal(field.Values, []string{schema.RunSchedule, schema.RunManual, schema.RunRetry}) {
		t.Errorf("a check's trigger takes %v: a check never starts after a change", field.Values)
	}

	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		t.Fatal(err)
	}
	if field := textField(t, hosts, "panel_address"); field.Max != 300 || field.Required {
		t.Errorf("panel_address is %+v", field)
	}
	for _, audited := range schema.Audited {
		if audited.Name == schema.Hosts && !slices.Contains(audited.Ignore, "panel_address") {
			t.Error("the audit records the address a server dials")
		}
	}

	kits, err := app.FindCollectionByNameOrId(schema.RecoveryKits)
	if err != nil {
		t.Fatalf("no recovery_kits: %v", err)
	}
	// admins read the kits; nothing writes them through the API
	if kits.ListRule == nil || *kits.ListRule != schema.RuleAdmin || kits.ViewRule == nil || *kits.ViewRule != schema.RuleAdmin {
		t.Errorf("read rules: %v, %v", kits.ListRule, kits.ViewRule)
	}
	if kits.CreateRule != nil || kits.UpdateRule != nil || kits.DeleteRule != nil {
		t.Error("the API may write recovery_kits")
	}
	checkFieldTypes(t, kits, map[string]string{
		"kit_id": core.FieldTypeText, "key_id": core.FieldTypeText, "digests": core.FieldTypeJSON, "created_by": core.FieldTypeText,
		"verified_at": core.FieldTypeDate, "verified_by": core.FieldTypeText, "created": core.FieldTypeAutodate, "updated": core.FieldTypeAutodate,
	}, true)
	if field := textField(t, kits, "kit_id"); !field.Required || field.Max != 20 {
		t.Errorf("kit_id is %+v", field)
	}
	for name, size := range map[string]int{"key_id": 32, "created_by": 255, "verified_by": 255} {
		if field := textField(t, kits, name); field.Max != size || field.Required {
			t.Errorf("%s holds %d characters, not %d", name, field.Max, size)
		}
	}
	if field := kits.Fields.GetByName("digests").(*core.JSONField); field.MaxSize != 4<<10 {
		t.Errorf("digests holds %d bytes", field.MaxSize)
	}
	if schema.IsEncrypted(schema.RecoveryKits, "digests") || len(schema.Encrypted[schema.RecoveryKits]) != 0 {
		t.Error("a kit's row holds a secret")
	}
	checkIndexes(t, app, schema.RecoveryKits, "UNIQUE INDEX idx_recovery_kits_kit_id ON recovery_kits (kit_id)")

	rows, err := panelRows(app)
	if err != nil {
		t.Fatalf("no %s: %v", schema.PanelTable, err)
	}
	if len(rows) != 1 || rows[0].ID != 1 {
		t.Fatalf("%s holds %+v", schema.PanelTable, rows)
	}
	// a plain table, which the API and the dashboard do not see
	if _, err := app.FindCollectionByNameOrId(schema.PanelTable); err == nil {
		t.Errorf("%s is a collection", schema.PanelTable)
	}
	if _, err := app.DB().NewQuery("INSERT INTO {{" + schema.PanelTable + "}} ([[id]]) VALUES (2)").Execute(); err == nil {
		t.Errorf("%s took a second row", schema.PanelTable)
	}
}

// panelJob is the panel's backup job, carried by f's server to f's target.
func panelJob(f checkFixture, name string) map[string]any {
	return map[string]any{"name": name, "host": f.host.Id, "source_kind": schema.SourcePanel,
		"targets": []string{f.target.Id}, "schedule": "0 3 * * *", "enabled": true, "origin": schema.JobManual,
		"retention": map[string]int{"daily": 14, "weekly": 8, "monthly": 12}}
}

// changeRun is a run that a change of the servers started.
func changeRun(job *core.Record, f checkFixture) map[string]any {
	return map[string]any{"job": job.Id, "target": f.target.Id, "host": f.host.Id, "trigger": schema.RunChange,
		"status": schema.RunSucceeded, "snapshot_id": strings.Repeat("4f", 32)}
}

// recoveryKit is a row of a kit the panel sealed and someone checked.
func recoveryKit(id string) map[string]any {
	return map[string]any{"kit_id": id, "key_id": strings.Repeat("9f", 16),
		"digests":    map[string]string{"key": strings.Repeat("a", 32), "address": strings.Repeat("b", 32), "job": strings.Repeat("c", 32), "targets": strings.Repeat("d", 32)},
		"created_by": "admin@example.com", "verified_at": types.NowDateTime(), "verified_by": "admin@example.com"}
}

// TestThePanelRecoveryMigrationAppliesToAFreshDatabase: a new panel gets
// the panel source, the change trigger, the servers' dial address, the
// recovery kits and the panel's own table; one job backs up the panel, and
// one row names each kit.
func TestThePanelRecoveryMigrationAppliesToAFreshDatabase(t *testing.T) {
	app := bootMigrationPanel(t)
	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("the migrations of a fresh install fail: %v", err)
	}
	checkRecoverySchema(t, app)
	rows, _ := panelRows(app)
	if rows[0] != (panelRow{ID: 1, RestoredFrom: "{}"}) {
		t.Errorf("a fresh panel's row is %+v", rows[0])
	}

	f := seedCheckFixture(t, app, "fresh")
	job := saveRecord(t, app, schema.BackupJobs, panelJob(f, "panel (pb_data)"))
	if _, err := trySave(app, schema.BackupJobs, panelJob(f, "panel again")); err == nil {
		t.Error("a second job backs up the panel")
	}
	// jobs of the other sources are many
	other := seedCheckFixture(t, app, "other")
	if other.job.GetString("source_kind") != schema.SourceVolume {
		t.Fatal(other.job)
	}
	run := saveRecord(t, app, schema.BackupRuns, changeRun(job, f))
	if run.GetString("trigger") != schema.RunChange {
		t.Errorf("the run's trigger is %q", run.GetString("trigger"))
	}
	bad := changeRun(job, f)
	bad["trigger"] = "enrollment"
	if _, err := trySave(app, schema.BackupRuns, bad); err == nil {
		t.Error("a run took the trigger enrollment")
	}

	host := testutil.NewHost(t, app, "dials")
	host.Set("panel_address", strings.Repeat("a", 295)+":9443")
	if err := app.Save(host); err != nil {
		t.Errorf("a server does not keep its address: %v", err)
	}
	host.Set("panel_address", strings.Repeat("a", 296)+":9443")
	if err := app.Save(host); err == nil {
		t.Error("a server keeps an address of 301 characters")
	}

	saveRecord(t, app, schema.RecoveryKits, recoveryKit("KIT-7Q2M-9XHC"))
	if _, err := trySave(app, schema.RecoveryKits, recoveryKit("KIT-7Q2M-9XHC")); err == nil {
		t.Error("two rows name one kit")
	}
	for _, id := range []string{"", strings.Repeat("K", 21)} {
		if _, err := trySave(app, schema.RecoveryKits, recoveryKit(id)); err == nil {
			t.Errorf("a kit took the id %q", id)
		}
	}
	big := recoveryKit("KIT-0000-0001")
	big["digests"] = map[string]string{"key": strings.Repeat("a", 4<<10)}
	if _, err := trySave(app, schema.RecoveryKits, big); err == nil {
		t.Error("a kit's digests took more than 4 KiB")
	}

	if _, err := app.DB().NewQuery("UPDATE {{" + schema.PanelTable + "}} SET [[key_id]] = {:key}").
		Bind(map[string]any{"key": strings.Repeat("9f", 16)}).Execute(); err != nil {
		t.Fatal(err)
	}
	if rows, err := panelRows(app); err != nil || len(rows) != 1 || rows[0].KeyID != strings.Repeat("9f", 16) {
		t.Errorf("the panel's row: %+v, %v", rows, err)
	}
}

// TestThePanelRecoveryMigrationAppliesOverChecks: a panel migrated through
// #31's migration, with its jobs, runs and servers, takes the migration and
// keeps its records; reverting it removes the panel's job with its runs,
// turns the other change runs into manual ones, and removes the field, the
// collection and the table, and keeps the rest; it applies again after that.
func TestThePanelRecoveryMigrationAppliesOverChecks(t *testing.T) {
	app := bootMigrationPanel(t)
	if _, err := core.NewMigrationsRunner(app, migrationsBefore(t, recoveryMigration)).Up(); err != nil {
		t.Fatalf("the migrations through 1771000000 fail: %v", err)
	}
	if _, err := app.FindCollectionByNameOrId(schema.BackupChecks); err != nil {
		t.Fatalf("#31's migration did not run: %v", err)
	}
	if _, err := app.FindCollectionByNameOrId(schema.RecoveryKits); err == nil {
		t.Fatal("recovery_kits is there before its migration")
	}
	if _, err := panelRows(app); err == nil {
		t.Fatalf("%s is there before its migration", schema.PanelTable)
	}
	f := seedCheckFixture(t, app, "old")
	if _, err := trySave(app, schema.BackupJobs, panelJob(f, "panel (pb_data)")); err == nil {
		t.Fatal("a job backs up the panel before the migration")
	}

	// the panel's next start applies only the new migration
	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("the migration fails over #31's: %v", err)
	}
	checkRecoverySchema(t, app)
	checkChecksSchema(t, app)
	host, err := app.FindRecordById(schema.Hosts, f.host.Id)
	if err != nil || host.GetString("panel_address") != "" || host.GetString("name") != f.host.GetString("name") {
		t.Errorf("a server of before: %v, %v", host, err)
	}
	run, err := app.FindRecordById(schema.BackupRuns, f.run.Id)
	if err != nil || run.GetString("trigger") != schema.RunSchedule || run.GetString("status") != schema.RunFailed {
		t.Errorf("a run of before: %v, %v", run, err)
	}

	// the panel's job and a change run of it; a change run of another job,
	// which only the revert's update reaches; a kit; the key id
	job := saveRecord(t, app, schema.BackupJobs, panelJob(f, "panel (pb_data)"))
	panelRun := saveRecord(t, app, schema.BackupRuns, changeRun(job, f))
	otherRun := saveRecord(t, app, schema.BackupRuns, changeRun(f.job, f))
	saveRecord(t, app, schema.RecoveryKits, recoveryKit("KIT-7Q2M-9XHC"))
	host.Set("panel_address", "192.0.2.2:9443")
	if err := app.Save(host); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB().NewQuery("UPDATE {{" + schema.PanelTable + "}} SET [[key_id]] = 'x', [[ca_pin]] = 'y'").Execute(); err != nil {
		t.Fatal(err)
	}

	later := migrationsFrom(t, recoveryMigration)
	reverted, err := core.NewMigrationsRunner(app, core.AppMigrations).Down(len(later))
	if err != nil || !slices.Equal(reverted, later) {
		t.Fatalf("reverting: %v, %v", reverted, err)
	}
	if _, err := app.FindRecordById(schema.BackupJobs, job.Id); err == nil {
		t.Error("the panel's job stays after the revert")
	}
	if _, err := app.FindRecordById(schema.BackupRuns, panelRun.Id); err == nil {
		t.Error("a run of the panel's job stays after the revert")
	}
	if kept, err := app.FindRecordById(schema.BackupRuns, otherRun.Id); err != nil || kept.GetString("trigger") != schema.RunManual {
		t.Errorf("a change run after the revert: %v, %v", kept, err)
	}
	if _, err := app.FindRecordById(schema.BackupJobs, f.job.Id); err != nil {
		t.Errorf("the revert removed another job: %v", err)
	}
	if _, err := app.FindRecordById(schema.BackupRuns, f.run.Id); err != nil {
		t.Errorf("the revert removed another run: %v", err)
	}
	if _, err := app.FindCollectionByNameOrId(schema.RecoveryKits); err == nil {
		t.Error("recovery_kits stays after the revert")
	}
	if _, err := panelRows(app); err == nil {
		t.Errorf("%s stays after the revert", schema.PanelTable)
	}
	for collection, check := range map[string]func(*core.Collection){
		schema.BackupJobs: func(c *core.Collection) {
			if field := c.Fields.GetByName("source_kind").(*core.SelectField); !slices.Equal(field.Values, []string{schema.SourceVolume, schema.SourcePath, schema.SourceStack, schema.SourceDump}) {
				t.Errorf("a job's source_kind takes %v after the revert", field.Values)
			}
			for _, index := range c.Indexes {
				if strings.Contains(index, "idx_backup_jobs_panel") {
					t.Errorf("the index %s stays after the revert", index)
				}
			}
		},
		schema.BackupRuns: func(c *core.Collection) {
			if field := c.Fields.GetByName("trigger").(*core.SelectField); !slices.Equal(field.Values, []string{schema.RunSchedule, schema.RunManual, schema.RunRetry}) {
				t.Errorf("a run's trigger takes %v after the revert", field.Values)
			}
		},
		schema.Hosts: func(c *core.Collection) {
			if c.Fields.GetByName("panel_address") != nil {
				t.Error("hosts.panel_address stays after the revert")
			}
		},
	} {
		c, err := app.FindCollectionByNameOrId(collection)
		if err != nil {
			t.Fatal(err)
		}
		check(c)
	}

	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("the migration does not apply again: %v", err)
	}
	checkRecoverySchema(t, app)
	// the table comes back empty: the revert took the key id with it
	if rows, err := panelRows(app); err != nil || rows[0] != (panelRow{ID: 1, RestoredFrom: "{}"}) {
		t.Errorf("the panel's row after it applied again: %+v, %v", rows, err)
	}
}
