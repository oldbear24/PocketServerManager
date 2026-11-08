package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upScheduledTasks, downScheduledTasks)
}

// upScheduledTasks adds scheduled tasks (#51): commands that the panel runs on
// a cron schedule in a PHP site's container or a stack's service, and the SMTP
// relay through which PHP sites send their mail.
func upScheduledTasks(app core.App) error {
	sites, err := app.FindCollectionByNameOrId(schema.Sites)
	if err != nil {
		return err
	}
	stacks, err := app.FindCollectionByNameOrId(schema.Stacks)
	if err != nil {
		return err
	}
	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return err
	}
	commands, err := app.FindCollectionByNameOrId(schema.Commands)
	if err != nil {
		return err
	}

	tasks := newCollection(schema.ScheduledTasks, access{
		list: schema.RuleViewer, view: schema.RuleViewer,
		create: schema.RuleOperator, update: schema.RuleOperator, del: schema.RuleOperator,
	})
	tasks.Fields.Add(
		&core.SelectField{Name: "target_kind", Required: true, MaxSelect: 1, Values: []string{schema.TaskTargetSite, schema.TaskTargetStack}},
		&core.RelationField{Name: "site", CollectionId: sites.Id, MaxSelect: 1, CascadeDelete: true},
		&core.RelationField{Name: "stack", CollectionId: stacks.Id, MaxSelect: 1, CascadeDelete: true},
		// the stack's service whose container runs it
		&core.TextField{Name: "service", Max: 255},
		// the server of the site or stack, set by the panel
		&core.RelationField{Name: "host", CollectionId: hosts.Id, MaxSelect: 1, CascadeDelete: true},
		// five fields, or @hourly, @daily and the like, in UTC
		&core.TextField{Name: "schedule", Required: true, Max: 255},
		&core.TextField{Name: "command", Required: true, Max: 4096},
		// a stack's container may run it as another user; a PHP site's runs it as www-data
		&core.TextField{Name: "user", Max: 64},
		&core.NumberField{Name: "timeout_s", OnlyInt: true},
		&core.BoolField{Name: "enabled"},
		// the last run, as the panel keeps it
		&core.DateField{Name: "last_run_at"},
		&core.SelectField{Name: "last_status", MaxSelect: 1, Values: []string{
			schema.TaskRunning, schema.TaskSucceeded, schema.TaskFailed, schema.TaskTimedOut, schema.TaskSkipped, schema.TaskCancelled,
		}},
		&core.NumberField{Name: "last_exit_code", OnlyInt: true},
		&core.NumberField{Name: "last_duration_ms", OnlyInt: true},
		// the end of its output
		&core.TextField{Name: "last_output", Max: 8192},
		&core.RelationField{Name: "last_command", CollectionId: commands.Id, MaxSelect: 1},
		// failed runs in a row, which alert at a few
		&core.NumberField{Name: "failures", OnlyInt: true},
	)
	tasks.AddIndex("idx_scheduled_tasks_site", false, "site", "")
	tasks.AddIndex("idx_scheduled_tasks_stack", false, "stack", "")
	if err := app.Save(tasks); err != nil {
		return err
	}

	settings, err := app.FindCollectionByNameOrId(schema.Settings)
	if err != nil {
		return err
	}
	settings.Fields.Add(
		// the SMTP relay of PHP sites' mail(); its password is set through
		// /api/pc/settings/smtp and never shown
		&core.TextField{Name: "smtp_host", Max: 255},
		&core.NumberField{Name: "smtp_port", OnlyInt: true},
		&core.SelectField{Name: "smtp_tls", MaxSelect: 1, Values: []string{"starttls", "tls", "off"}},
		&core.TextField{Name: "smtp_user", Max: 255},
		&core.TextField{Name: "smtp_password", Max: maxSealed, Hidden: true},
		&core.BoolField{Name: "smtp_password_set"},
		&core.TextField{Name: "smtp_from", Max: 255},
	)
	return app.Save(settings)
}

func downScheduledTasks(app core.App) error {
	if tasks, err := app.FindCollectionByNameOrId(schema.ScheduledTasks); err == nil {
		if err := app.Delete(tasks); err != nil {
			return err
		}
	}
	settings, err := app.FindCollectionByNameOrId(schema.Settings)
	if err != nil {
		return err
	}
	for _, field := range []string{"smtp_host", "smtp_port", "smtp_tls", "smtp_user", "smtp_password", "smtp_password_set", "smtp_from"} {
		settings.Fields.RemoveByName(field)
	}
	return app.Save(settings)
}
