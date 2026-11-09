package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upBackupJobs, downBackupJobs)
}

// upBackupJobs adds backup jobs and their runs (#29). A job says what is
// backed up, from which server, to which targets, when and how long it is
// kept; each run backs it up to one target. The panel's routes and its
// scheduler write both: viewers read them.
func upBackupJobs(app core.App) error {
	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return err
	}
	stacks, err := app.FindCollectionByNameOrId(schema.Stacks)
	if err != nil {
		return err
	}
	targets, err := app.FindCollectionByNameOrId(schema.BackupTargets)
	if err != nil {
		return err
	}
	commands, err := app.FindCollectionByNameOrId(schema.Commands)
	if err != nil {
		return err
	}

	jobs := newCollection(schema.BackupJobs, access{list: schema.RuleViewer, view: schema.RuleViewer})
	jobs.Fields.Add(
		&core.TextField{Name: "name", Required: true, Max: 100},
		// the server whose data the job backs up, and whose agent runs it
		&core.RelationField{Name: "host", Required: true, CollectionId: hosts.Id, MaxSelect: 1, CascadeDelete: true},
		&core.SelectField{Name: "source_kind", Required: true, MaxSelect: 1, Values: []string{
			schema.SourceVolume, schema.SourcePath, schema.SourceStack, schema.SourceDump,
		}},
		// a volume's name, a directory of the server, or a SQLite file in the
		// service's container
		&core.TextField{Name: "source", Max: 1000},
		// the stack of a stack's volumes or of a dump; a label's stack
		&core.RelationField{Name: "stack", CollectionId: stacks.Id, MaxSelect: 1},
		// the service whose container makes a dump; a label's service
		&core.TextField{Name: "service", Max: 255},
		&core.SelectField{Name: "engine", MaxSelect: 1, Values: []string{schema.DumpPostgres, schema.DumpMySQL, schema.DumpSQLite}},
		&core.RelationField{Name: "targets", CollectionId: targets.Id, MaxSelect: 10},
		// five fields, or @hourly, @daily and the like, in UTC
		&core.TextField{Name: "schedule", Required: true, Max: 255},
		// how many snapshots restic keeps: last, hourly, daily, weekly,
		// monthly, yearly; nothing set keeps every one
		&core.JSONField{Name: "retention", MaxSize: 1 << 10},
		&core.BoolField{Name: "enabled"},
		&core.SelectField{Name: "origin", Required: true, MaxSelect: 1, Values: []string{schema.JobManual, schema.JobLabel}},
		// a label's job whose label is gone, kept until someone removes it
		&core.BoolField{Name: "label_removed"},
		// what keeps the job from running, such as a label naming no target
		&core.TextField{Name: "problem", Max: 1000},
		&core.DateField{Name: "next_run_at"},
		&core.DateField{Name: "last_run_at"},
		&core.SelectField{Name: "last_status", MaxSelect: 1, Values: []string{
			schema.RunQueued, schema.RunRunning, schema.RunSucceeded, schema.RunFailed, schema.RunSkipped, schema.RunCancelled,
		}},
	)
	jobs.AddIndex("idx_backup_jobs_name", true, "name", "")
	jobs.AddIndex("idx_backup_jobs_stack", false, "stack", "")
	if err := app.Save(jobs); err != nil {
		return err
	}

	runs := newCollection(schema.BackupRuns, access{list: schema.RuleViewer, view: schema.RuleViewer})
	runs.Fields.Add(
		&core.RelationField{Name: "job", Required: true, CollectionId: jobs.Id, MaxSelect: 1, CascadeDelete: true},
		&core.RelationField{Name: "target", Required: true, CollectionId: targets.Id, MaxSelect: 1, CascadeDelete: true},
		&core.RelationField{Name: "host", CollectionId: hosts.Id, MaxSelect: 1, CascadeDelete: true},
		&core.SelectField{Name: "trigger", Required: true, MaxSelect: 1, Values: []string{schema.RunSchedule, schema.RunManual}},
		&core.SelectField{Name: "status", Required: true, MaxSelect: 1, Values: []string{
			schema.RunQueued, schema.RunRunning, schema.RunSucceeded, schema.RunFailed, schema.RunSkipped, schema.RunCancelled,
		}},
		// the minute the schedule named, for a scheduled run
		&core.DateField{Name: "scheduled_at"},
		// when the agent started the job, and when it ended
		&core.DateField{Name: "started_at"},
		&core.DateField{Name: "finished_at"},
		&core.NumberField{Name: "duration_ms", OnlyInt: true},
		&core.TextField{Name: "snapshot_id", Max: 100},
		// what restic added to the repository, and what it read
		&core.NumberField{Name: "bytes_added", OnlyInt: true},
		&core.NumberField{Name: "total_bytes", OnlyInt: true},
		&core.NumberField{Name: "files_new", OnlyInt: true},
		&core.NumberField{Name: "files_changed", OnlyInt: true},
		// the snapshots the retention removed after the backup
		&core.NumberField{Name: "forgotten", OnlyInt: true},
		// restic's error, or why the run did not start
		&core.TextField{Name: "error", Max: 4096},
		// the end of the job's output, with its secrets hidden
		&core.TextField{Name: "log", Max: 32 << 10},
		&core.RelationField{Name: "command", CollectionId: commands.Id, MaxSelect: 1},
	)
	runs.AddIndex("idx_backup_runs_job", false, "job, created", "")
	runs.AddIndex("idx_backup_runs_target", false, "target", "")
	runs.AddIndex("idx_backup_runs_command", false, "command", "")
	return app.Save(runs)
}

func downBackupJobs(app core.App) error {
	for _, name := range []string{schema.BackupRuns, schema.BackupJobs} {
		collection, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			return err
		}
		if err := app.Delete(collection); err != nil {
			return err
		}
	}
	return nil
}
