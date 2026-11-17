package migrations

import (
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upPanelRecovery, downPanelRecovery)
}

// sourceKinds are what a backup job backs up, before #32 and with it.
var (
	sourceKindsBefore = []string{schema.SourceVolume, schema.SourcePath, schema.SourceStack, schema.SourceDump}
	sourceKindsPanel  = []string{schema.SourceVolume, schema.SourcePath, schema.SourceStack, schema.SourceDump, schema.SourcePanel}
)

// runTriggers are how a backup run starts, before #32 and with it.
var (
	runTriggersBefore = []string{schema.RunSchedule, schema.RunManual, schema.RunRetry}
	runTriggersPanel  = []string{schema.RunSchedule, schema.RunManual, schema.RunRetry, schema.RunChange}
)

const createPanelTable = `CREATE TABLE IF NOT EXISTS {{` + schema.PanelTable + `}} (
	[[id]] INTEGER PRIMARY KEY CHECK ([[id]] = 1),
	[[key_id]] TEXT NOT NULL DEFAULT '',
	[[ca_pin]] TEXT NOT NULL DEFAULT '',
	[[restored_at]] TEXT NOT NULL DEFAULT '',
	[[restored_from]] TEXT NOT NULL DEFAULT '{}'
)`

// upPanelRecovery lets the panel back itself up and come back from that
// backup on a new machine (#32). One backup job may back up the panel: its
// server is the carrier, whose agent uploads the panel's encrypted copy. A
// run of it may start after a change of the servers. Each server says which
// address its agent dials. The recovery kits the panel sealed are listed,
// never kept; and a plain table holds the id of the master key that sealed
// the database, the pin of its CA, and the restore it came from.
func upPanelRecovery(app core.App) error {
	jobs, err := app.FindCollectionByNameOrId(schema.BackupJobs)
	if err != nil {
		return err
	}
	jobs.Fields.GetByName("source_kind").(*core.SelectField).Values = sourceKindsPanel
	// the panel has one backup job at most
	jobs.AddIndex("idx_backup_jobs_panel", true, "source_kind", "source_kind = '"+schema.SourcePanel+"'")
	if err := app.Save(jobs); err != nil {
		return err
	}

	runs, err := app.FindCollectionByNameOrId(schema.BackupRuns)
	if err != nil {
		return err
	}
	runs.Fields.GetByName("trigger").(*core.SelectField).Values = runTriggersPanel
	if err := app.Save(runs); err != nil {
		return err
	}

	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return err
	}
	// the gRPC :authority of the agent's stream: the address it dials,
	// host:port, as its identity holds it
	hosts.Fields.Add(&core.TextField{Name: "panel_address", Max: 300})
	if err := app.Save(hosts); err != nil {
		return err
	}

	// admins read the kits; only the panel's routes write them
	kits := newCollection(schema.RecoveryKits, access{list: schema.RuleAdmin, view: schema.RuleAdmin})
	kits.Fields.Add(
		// KIT-XXXX-XXXX, as the kit's header says it
		&core.TextField{Name: "kit_id", Required: true, Max: 20},
		// secrets.KeyID of the master key the kit holds
		&core.TextField{Name: "key_id", Max: 32},
		// the digest of each section of what the kit holds: key, address,
		// job, targets; a kit is current while they equal the panel's
		&core.JSONField{Name: "digests", MaxSize: 4 << 10},
		&core.TextField{Name: "created_by", Max: 255},
		// when a check opened the kit with its passphrase, and who checked it
		&core.DateField{Name: "verified_at"},
		&core.TextField{Name: "verified_by", Max: 255},
	)
	kits.AddIndex("idx_recovery_kits_kit_id", true, "kit_id", "")
	if err := app.Save(kits); err != nil {
		return err
	}

	if _, err := app.DB().NewQuery(createPanelTable).Execute(); err != nil {
		return err
	}
	_, err = app.DB().NewQuery("INSERT OR IGNORE INTO {{" + schema.PanelTable + "}} ([[id]]) VALUES (1)").Execute()
	return err
}

func downPanelRecovery(app core.App) error {
	// the panel's job, and with it its runs and restores
	panelJobs, err := app.FindAllRecords(schema.BackupJobs, dbx.HashExp{"source_kind": schema.SourcePanel})
	if err != nil {
		return err
	}
	for _, job := range panelJobs {
		if err := app.Delete(job); err != nil {
			return err
		}
	}
	jobs, err := app.FindCollectionByNameOrId(schema.BackupJobs)
	if err != nil {
		return err
	}
	jobs.RemoveIndex("idx_backup_jobs_panel")
	jobs.Fields.GetByName("source_kind").(*core.SelectField).Values = sourceKindsBefore
	if err := app.Save(jobs); err != nil {
		return err
	}

	runs, err := app.FindCollectionByNameOrId(schema.BackupRuns)
	if err != nil {
		return err
	}
	if _, err := app.DB().NewQuery("UPDATE {{" + schema.BackupRuns + "}} SET [[trigger]] = {:manual} WHERE [[trigger]] = {:change}").
		Bind(map[string]any{"manual": schema.RunManual, "change": schema.RunChange}).Execute(); err != nil {
		return err
	}
	runs.Fields.GetByName("trigger").(*core.SelectField).Values = runTriggersBefore
	if err := app.Save(runs); err != nil {
		return err
	}

	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return err
	}
	hosts.Fields.RemoveByName("panel_address")
	if err := app.Save(hosts); err != nil {
		return err
	}

	kits, err := app.FindCollectionByNameOrId(schema.RecoveryKits)
	if err != nil {
		return err
	}
	if err := app.Delete(kits); err != nil {
		return err
	}

	_, err = app.DB().NewQuery("DROP TABLE IF EXISTS {{" + schema.PanelTable + "}}").Execute()
	return err
}
