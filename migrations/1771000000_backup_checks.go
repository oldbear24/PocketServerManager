package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upBackupChecks, downBackupChecks)
}

// checkStates are the states of a check, and of a target's last one.
var checkStates = []string{
	schema.CheckQueued, schema.CheckRunning, schema.CheckOK, schema.CheckFailed,
	schema.CheckSuspect, schema.CheckError, schema.CheckSkipped, schema.CheckCancelled,
}

// targetCheckFields are the fields a target gains: how its checks go.
var targetCheckFields = []string{
	"integrity_status", "integrity_at", "integrity_error", "last_check", "last_check_status",
	"last_check_error", "next_check_at", "integrity_group",
}

// runRetryFields are the fields a run gains: its failure, and its retry.
var runRetryFields = []string{"attempt", "retry_of", "retry_at", "retry_end", "retry_note", "failure", "lock"}

// upBackupChecks adds the integrity checks of the targets' repositories, and
// what a failed run needs to be tried again (#31). Admins set when the
// checks run, and how much of each repository one reads, in the settings;
// every target keeps a summary of its checks, and each check is a record
// that viewers read. A run keeps why it failed, the lock that kept it out,
// and the retry the panel plans.
func upBackupChecks(app core.App) error {
	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return err
	}
	commands, err := app.FindCollectionByNameOrId(schema.Commands)
	if err != nil {
		return err
	}

	settings, err := app.FindCollectionByNameOrId(schema.Settings)
	if err != nil {
		return err
	}
	settings.Fields.Add(
		// cron in UTC; empty turns the checks off
		&core.TextField{Name: "backup_check_schedule", Max: 100},
		// each check reads group n of this many groups of a repository's
		// data packs, the next one each time; 0 checks the structure only
		&core.NumberField{Name: "backup_check_groups", OnlyInt: true},
	)
	if err := app.Save(settings); err != nil {
		return err
	}
	records, err := app.FindAllRecords(schema.Settings)
	if err != nil {
		return err
	}
	for _, record := range records {
		record.Set("backup_check_schedule", schema.DefaultCheckSchedule)
		record.Set("backup_check_groups", schema.DefaultCheckGroups)
		if err := app.Save(record); err != nil {
			return err
		}
	}

	targets, err := app.FindCollectionByNameOrId(schema.BackupTargets)
	if err != nil {
		return err
	}
	targets.Fields.Add(
		// the newest check that completed, ok or failed, when it finished and
		// the first errors it found
		&core.SelectField{Name: "integrity_status", MaxSelect: 1, Values: []string{schema.CheckOK, schema.CheckFailed}},
		&core.DateField{Name: "integrity_at"},
		&core.TextField{Name: "integrity_error", Max: 4096},
		// the newest check, as text: the prune of old checks is raw SQL
		&core.TextField{Name: "last_check", Max: 15, Pattern: "^[a-z0-9]*$"},
		&core.SelectField{Name: "last_check_status", MaxSelect: 1, Values: checkStates},
		// why the newest check did not complete
		&core.TextField{Name: "last_check_error", Max: 1000},
		&core.DateField{Name: "next_check_at"},
		// the group the next check reads, from 1; 0 is read as 1
		&core.NumberField{Name: "integrity_group", OnlyInt: true},
	)
	if err := app.Save(targets); err != nil {
		return err
	}

	checks := newCollection(schema.BackupChecks, access{list: schema.RuleViewer, view: schema.RuleViewer})
	checks.Fields.Add(
		&core.RelationField{Name: "target", Required: true, CollectionId: targets.Id, MaxSelect: 1, CascadeDelete: true},
		// the server whose agent checks it: the target's
		&core.RelationField{Name: "host", CollectionId: hosts.Id, MaxSelect: 1, CascadeDelete: true},
		&core.SelectField{Name: "kind", Required: true, MaxSelect: 1, Values: []string{schema.CheckIntegrity, schema.CheckRestoreTest}},
		&core.SelectField{Name: "trigger", Required: true, MaxSelect: 1, Values: []string{schema.RunSchedule, schema.RunManual, schema.RunRetry}},
		&core.SelectField{Name: "status", Required: true, MaxSelect: 1, Values: checkStates},
		// the slot of the schedule, on the first attempt of a scheduled check
		// only: one check per target and slot
		&core.DateField{Name: "scheduled_at"},
		&core.DateField{Name: "started_at"},
		&core.DateField{Name: "finished_at"},
		&core.NumberField{Name: "duration_ms", OnlyInt: true},
		// restic's --read-data-subset, n/t; empty for the structure only
		&core.TextField{Name: "subset", Max: 16},
		&core.NumberField{Name: "packs_read", OnlyInt: true},
		&core.NumberField{Name: "packs_total", OnlyInt: true},
		// the second pass, under the exclusive lock, found the first pass's
		// errors too, or did not
		&core.BoolField{Name: "confirmed"},
		&core.BoolField{Name: "unconfirmed"},
		// what restic found and advised, as the agent says it
		&core.JSONField{Name: "errors", MaxSize: 16 << 10},
		&core.NumberField{Name: "errors_more", OnlyInt: true},
		&core.JSONField{Name: "repair", MaxSize: 4 << 10},
		&core.JSONField{Name: "notes", MaxSize: 4 << 10},
		// the lock that kept the second pass out
		&core.JSONField{Name: "lock", MaxSize: 2 << 10},
		// why the check did not complete, or did not start
		&core.TextField{Name: "error", Max: 4096},
		// the end of the job's output, with the target's secrets hidden
		&core.TextField{Name: "log", Max: 32 << 10},
		&core.RelationField{Name: "command", CollectionId: commands.Id, MaxSelect: 1},
		// the retries of a check that did not complete: 1 is the first
		// attempt (0 is read as 1), retry_of the check before it, as text
		&core.NumberField{Name: "attempt", OnlyInt: true},
		&core.TextField{Name: "retry_of", Max: 15, Pattern: "^[a-z0-9]*$"},
		&core.DateField{Name: "retry_at"},
	)
	checks.AddIndex("idx_backup_checks_target", false, "target, created", "")
	checks.AddIndex("idx_backup_checks_command", false, "command", "")
	checks.AddIndex("idx_backup_checks_slot", true, "target, scheduled_at", "scheduled_at != ''")
	checks.AddIndex("idx_backup_checks_retry_of", true, "retry_of", "retry_of != ''")
	checks.AddIndex("idx_backup_checks_retry_at", false, "status, retry_at", "")
	if err := app.Save(checks); err != nil {
		return err
	}

	runs, err := app.FindCollectionByNameOrId(schema.BackupRuns)
	if err != nil {
		return err
	}
	runs.Fields.GetByName("trigger").(*core.SelectField).Values = []string{schema.RunSchedule, schema.RunManual, schema.RunRetry}
	runs.Fields.Add(
		// 1 is the first attempt of a run; 0 is read as 1
		&core.NumberField{Name: "attempt", OnlyInt: true},
		// the failed run this one tries again, as text: the prune of old
		// runs is raw SQL
		&core.TextField{Name: "retry_of", Max: 15, Pattern: "^[a-z0-9]*$"},
		// when the panel tries a failed run again; empty when it does not
		&core.DateField{Name: "retry_at"},
		// how the panel ended a planned retry without making it: the job's
		// next scheduled run takes its place, or it gave the retry up;
		// empty while one is planned, once it was made, or when none was
		&core.SelectField{Name: "retry_end", MaxSelect: 1, Values: []string{schema.RetryNextRun, schema.RetryGivenUp}},
		// why it waits, or why it is not tried again
		&core.TextField{Name: "retry_note", Max: 500},
		&core.SelectField{Name: "failure", MaxSelect: 1, Values: []string{
			backup.FailureLocked, backup.FailureLockInvalid, backup.FailureRepository, backup.FailurePassword,
			backup.FailurePartial, backup.FailureSource, backup.FailureRestic, backup.FailureInterrupted, backup.FailureTimeout,
		}},
		// the lock that kept the run out, and the command that held it
		&core.JSONField{Name: "lock", MaxSize: 2 << 10},
	)
	// a failed run is tried again once
	runs.AddIndex("idx_backup_runs_retry_of", true, "retry_of", "retry_of != ''")
	runs.AddIndex("idx_backup_runs_retry_at", false, "status, retry_at", "")
	return app.Save(runs)
}

func downBackupChecks(app core.App) error {
	runs, err := app.FindCollectionByNameOrId(schema.BackupRuns)
	if err != nil {
		return err
	}
	if _, err := app.DB().NewQuery("UPDATE {{" + schema.BackupRuns + "}} SET [[trigger]] = {:manual} WHERE [[trigger]] = {:retry}").
		Bind(map[string]any{"manual": schema.RunManual, "retry": schema.RunRetry}).Execute(); err != nil {
		return err
	}
	runs.RemoveIndex("idx_backup_runs_retry_of")
	runs.RemoveIndex("idx_backup_runs_retry_at")
	for _, name := range runRetryFields {
		runs.Fields.RemoveByName(name)
	}
	runs.Fields.GetByName("trigger").(*core.SelectField).Values = []string{schema.RunSchedule, schema.RunManual}
	if err := app.Save(runs); err != nil {
		return err
	}

	checks, err := app.FindCollectionByNameOrId(schema.BackupChecks)
	if err != nil {
		return err
	}
	if err := app.Delete(checks); err != nil {
		return err
	}

	targets, err := app.FindCollectionByNameOrId(schema.BackupTargets)
	if err != nil {
		return err
	}
	for _, name := range targetCheckFields {
		targets.Fields.RemoveByName(name)
	}
	if err := app.Save(targets); err != nil {
		return err
	}

	settings, err := app.FindCollectionByNameOrId(schema.Settings)
	if err != nil {
		return err
	}
	settings.Fields.RemoveByName("backup_check_schedule")
	settings.Fields.RemoveByName("backup_check_groups")
	return app.Save(settings)
}
