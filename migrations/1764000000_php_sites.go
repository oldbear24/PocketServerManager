package migrations

import (
	"errors"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upPHPSites, downPHPSites)
}

// upPHPSites adds PHP sites (#50): a site of type php has its PHP settings in
// site_php, which the panel's routes change, as each change is applied to the
// site's PHP-FPM container. A deployment keeps the PHP version it was built
// for, and a database or its network can belong to a site.
func upPHPSites(app core.App) error {
	sites, err := app.FindCollectionByNameOrId(schema.Sites)
	if err != nil {
		return err
	}
	kind, ok := sites.Fields.GetByName("type").(*core.SelectField)
	if !ok {
		return errors.New("sites has no select field type")
	}
	kind.Values = []string{schema.SiteStatic, schema.SitePHPApp}
	if err := app.Save(sites); err != nil {
		return err
	}

	databases, err := app.FindCollectionByNameOrId(schema.Databases)
	if err != nil {
		return err
	}

	settings := newCollection(schema.SitePHP, access{list: schema.RuleViewer, view: schema.RuleViewer})
	settings.Fields.Add(
		&core.RelationField{Name: "site", Required: true, CollectionId: sites.Id, MaxSelect: 1, CascadeDelete: true},
		// such as 8.4, of the manifest of the runtime images
		&core.TextField{Name: "php_version", Required: true, Max: 16},
		&core.SelectField{Name: "template", MaxSelect: 1, Values: []string{schema.PHPCustom, schema.PHPWordPress, schema.PHPLaravel, schema.PHPNette}},
		// the directory the edge serves, in the release: public, www, or the release itself
		&core.TextField{Name: "docroot", Max: 255},
		&core.SelectField{Name: "mode", MaxSelect: 1, Values: []string{schema.PHPReleases, schema.PHPClassic}},
		// pm, max_children, max_requests, request_terminate_timeout
		&core.JSONField{Name: "fpm", MaxSize: 4 << 10},
		// php.ini lines of its own, such as memory_limit = 256M
		&core.TextField{Name: "php_ini", Max: 16 << 10},
		// NAME=value lines, which may refer to ${secret:name}
		&core.TextField{Name: "env", Max: 64 << 10},
		// the site's own secrets, sealed: a JSON object of name → value
		&core.TextField{Name: "secrets", Max: maxSealed, Hidden: true},
		&core.NumberField{Name: "cpu_limit"},
		&core.NumberField{Name: "mem_limit_mb", OnlyInt: true},
		// paths of the release kept in shared/ across releases, such as storage
		&core.JSONField{Name: "shared_paths", MaxSize: 16 << 10},
		// run in the new release before it goes live, such as php artisan migrate --force
		&core.TextField{Name: "release_hook", Max: 4096},
		&core.RelationField{Name: "database", CollectionId: databases.Id, MaxSelect: 1},
		// the PHP-FPM container, as its last apply left it
		&core.SelectField{Name: "state", MaxSelect: 1, Values: []string{schema.PHPPending, schema.PHPRunning, schema.PHPFailed}},
		&core.TextField{Name: "image", Max: 512},
		&core.DateField{Name: "applied_at"},
		&core.TextField{Name: "last_error", Max: 4096},
	)
	settings.AddIndex("idx_site_php_site", true, "site", "")
	if err := app.Save(settings); err != nil {
		return err
	}

	deployments, err := app.FindCollectionByNameOrId(schema.Deployments)
	if err != nil {
		return err
	}
	deployments.Fields.Add(&core.TextField{Name: "php_version", Max: 16})
	if err := app.Save(deployments); err != nil {
		return err
	}

	// a database of a site; removing the site keeps its data
	databases.Fields.Add(&core.RelationField{Name: "site", CollectionId: sites.Id, MaxSelect: 1})
	if err := app.Save(databases); err != nil {
		return err
	}
	networks, err := app.FindCollectionByNameOrId(schema.DBNetworks)
	if err != nil {
		return err
	}
	networks.Fields.Add(&core.RelationField{Name: "site", CollectionId: sites.Id, MaxSelect: 1, CascadeDelete: true})
	return app.Save(networks)
}

func downPHPSites(app core.App) error {
	for name, field := range map[string]string{schema.Deployments: "php_version", schema.Databases: "site", schema.DBNetworks: "site"} {
		collection, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			return err
		}
		collection.Fields.RemoveByName(field)
		if err := app.Save(collection); err != nil {
			return err
		}
	}
	if settings, err := app.FindCollectionByNameOrId(schema.SitePHP); err == nil {
		if err := app.Delete(settings); err != nil {
			return err
		}
	}
	sites, err := app.FindCollectionByNameOrId(schema.Sites)
	if err != nil {
		return err
	}
	if kind, ok := sites.Fields.GetByName("type").(*core.SelectField); ok {
		kind.Values = []string{schema.SiteStatic}
	}
	return app.Save(sites)
}
