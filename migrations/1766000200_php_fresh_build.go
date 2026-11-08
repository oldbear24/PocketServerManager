package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upPHPFreshBuild, downPHPFreshBuild)
}

// upPHPFreshBuild keeps a fresh install's build apart from the site's own
// (#52): WP-CLI's download builds the install's release only, not an upload
// or a Git source of the site's later.
func upPHPFreshBuild(app core.App) error {
	settings, err := app.FindCollectionByNameOrId(schema.SitePHP)
	if err != nil {
		return err
	}
	settings.Fields.Add(&core.TextField{Name: "fresh_build", Max: 1024})
	return app.Save(settings)
}

func downPHPFreshBuild(app core.App) error {
	settings, err := app.FindCollectionByNameOrId(schema.SitePHP)
	if err != nil {
		return err
	}
	settings.Fields.RemoveByName("fresh_build")
	return app.Save(settings)
}
