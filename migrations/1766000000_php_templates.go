package migrations

import (
	"errors"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upPHPTemplates, downPHPTemplates)
}

// upPHPTemplates adds what PHP application templates need (#52): a site's
// worker, health check, what its backups leave out and a fresh install under
// way, in site_php; an update of an application as a trigger of a
// deployment, with the release it starts from, what runs before its release
// hook and how the site answered after it.
func upPHPTemplates(app core.App) error {
	settings, err := app.FindCollectionByNameOrId(schema.SitePHP)
	if err != nil {
		return err
	}
	settings.Fields.Add(
		// a worker container of the template's, such as Laravel's queue worker
		&core.BoolField{Name: "queue_worker"},
		// the path the panel asks after each release, such as /up
		&core.TextField{Name: "health_path", Max: 255},
		// paths of the site that its backups leave out, such as temp
		&core.JSONField{Name: "backup_exclude", MaxSize: 16 << 10},
		// a fresh install's title, administrator and language until it is done;
		// its password is a secret of the site meanwhile
		&core.JSONField{Name: "install", MaxSize: 4 << 10, Hidden: true},
	)
	if err := app.Save(settings); err != nil {
		return err
	}

	deployments, err := app.FindCollectionByNameOrId(schema.Deployments)
	if err != nil {
		return err
	}
	trigger, ok := deployments.Fields.GetByName("trigger").(*core.SelectField)
	if !ok {
		return errors.New("deployments has no select field trigger")
	}
	trigger.Values = []string{schema.TriggerCreate, schema.TriggerManual, schema.TriggerPush, schema.TriggerUpload, schema.TriggerUpdate}
	deployments.Fields.Add(
		// the release whose copy was built, as an update's or a redeploy's
		&core.NumberField{Name: "base_release", OnlyInt: true},
		// commands run before the release hook, as an update's
		&core.TextField{Name: "pre_hook", Max: 8192},
		// how the site answered at its health check after the release went live
		&core.JSONField{Name: "health", MaxSize: 4 << 10},
	)
	return app.Save(deployments)
}

func downPHPTemplates(app core.App) error {
	settings, err := app.FindCollectionByNameOrId(schema.SitePHP)
	if err != nil {
		return err
	}
	for _, name := range []string{"queue_worker", "health_path", "backup_exclude", "install"} {
		settings.Fields.RemoveByName(name)
	}
	if err := app.Save(settings); err != nil {
		return err
	}
	deployments, err := app.FindCollectionByNameOrId(schema.Deployments)
	if err != nil {
		return err
	}
	for _, name := range []string{"base_release", "pre_hook", "health"} {
		deployments.Fields.RemoveByName(name)
	}
	if trigger, ok := deployments.Fields.GetByName("trigger").(*core.SelectField); ok {
		trigger.Values = []string{schema.TriggerCreate, schema.TriggerManual, schema.TriggerPush, schema.TriggerUpload}
	}
	return app.Save(deployments)
}
