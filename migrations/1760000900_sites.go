package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/sites"
)

func init() {
	m.Register(upSites, downSites)
}

// maxUpload is the largest ZIP a site can be deployed from.
const maxUpload = 256 << 20

// upSites adds static sites and their deployments (#21), and domains that
// serve a site.
func upSites(app core.App) error {
	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return err
	}
	commands, err := app.FindCollectionByNameOrId(schema.Commands)
	if err != nil {
		return err
	}

	siteCollection := newCollection(schema.Sites, access{
		list: schema.RuleViewer, view: schema.RuleViewer,
		create: schema.RuleOperator, update: schema.RuleOperator, del: schema.RuleOperator,
	})
	siteCollection.Fields.Add(
		&core.TextField{Name: "name", Required: true, Max: 40, Pattern: `^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`},
		// the edge server that builds and serves it
		&core.RelationField{Name: "host", Required: true, CollectionId: hosts.Id, MaxSelect: 1, CascadeDelete: true},
		&core.SelectField{Name: "type", MaxSelect: 1, Values: []string{schema.SiteStatic}},
		&core.SelectField{Name: "source", Required: true, MaxSelect: 1, Values: []string{sites.SourceGit, sites.SourceUpload, sites.SourceEmpty}},
		&core.TextField{Name: "repo_url", Max: 2048},
		&core.TextField{Name: "branch", Max: 255},
		// for a private repository, sealed
		&core.TextField{Name: "git_token", Max: maxSealed, Hidden: true},
		&core.SelectField{Name: "generator", MaxSelect: 1, Values: []string{
			sites.GeneratorAuto, sites.GeneratorNone, sites.GeneratorHugo, sites.GeneratorMkDocs,
			sites.GeneratorAstro, sites.GeneratorNPM, sites.GeneratorCustom,
		}},
		&core.TextField{Name: "build_image", Max: 512},
		&core.TextField{Name: "build_cmd", Max: 4096},
		&core.TextField{Name: "output_dir", Max: 255},
		&core.NumberField{Name: "build_cpus"},
		&core.NumberField{Name: "build_memory_mb", OnlyInt: true},
		&core.NumberField{Name: "keep_releases", OnlyInt: true},
		// how the edge serves it
		&core.BoolField{Name: "https_redirect"},
		&core.BoolField{Name: "hsts"},
		&core.BoolField{Name: "compress"},
		&core.BoolField{Name: "spa"},
		&core.TextField{Name: "not_found", Max: 255},
		&core.JSONField{Name: "headers", MaxSize: 64 << 10},
		&core.JSONField{Name: "cache_rules", MaxSize: 64 << 10},
		// user:bcrypt-hash lines, sealed; the names alone are shown
		&core.TextField{Name: "basic_auth", Max: maxSealed, Hidden: true},
		&core.JSONField{Name: "basic_auth_users", MaxSize: 16 << 10},
		// signs the pushes of the Git server, sealed
		&core.TextField{Name: "webhook_secret", Max: maxSealed, Hidden: true},
		// whether there is a git_token, which is never shown
		&core.BoolField{Name: "git_token_set"},
	)
	siteCollection.AddIndex("idx_sites_host_name", true, "host, name", "")
	if err := app.Save(siteCollection); err != nil {
		return err
	}

	deployments := newCollection(schema.Deployments, access{list: schema.RuleViewer, view: schema.RuleViewer})
	deployments.Fields.Add(
		&core.RelationField{Name: "site", Required: true, CollectionId: siteCollection.Id, MaxSelect: 1, CascadeDelete: true},
		// the release's directory on the server
		&core.NumberField{Name: "number", OnlyInt: true},
		&core.SelectField{Name: "status", MaxSelect: 1, Values: []string{
			schema.DeploymentQueued, schema.DeploymentBuilding, schema.DeploymentSucceeded, schema.DeploymentFailed,
		}},
		&core.SelectField{Name: "trigger", MaxSelect: 1, Values: []string{
			schema.TriggerCreate, schema.TriggerManual, schema.TriggerPush, schema.TriggerUpload,
		}},
		&core.SelectField{Name: "source", MaxSelect: 1, Values: []string{sites.SourceGit, sites.SourceUpload, sites.SourceEmpty}},
		&core.TextField{Name: "branch", Max: 255},
		&core.TextField{Name: "commit_sha", Max: 64},
		&core.TextField{Name: "message", Max: 1000},
		&core.TextField{Name: "requested_by", Max: 255},
		&core.RelationField{Name: "command", CollectionId: commands.Id, MaxSelect: 1},
		// how it was built
		&core.TextField{Name: "generator", Max: 32},
		&core.TextField{Name: "image", Max: 512},
		&core.TextField{Name: "build_cmd", Max: 4096},
		&core.DateField{Name: "started_at"},
		&core.DateField{Name: "finished_at"},
		&core.NumberField{Name: "build_ms", OnlyInt: true},
		&core.NumberField{Name: "size_bytes", OnlyInt: true},
		&core.NumberField{Name: "files", OnlyInt: true},
		&core.TextField{Name: "error", Max: 4096},
		// its release is still on the server, so it can be switched back to
		&core.BoolField{Name: "on_server"},
		&core.FileField{Name: "upload", MaxSelect: 1, MaxSize: maxUpload, Protected: true},
		&core.TextField{Name: "upload_sha256", Max: 64},
	)
	deployments.AddIndex("idx_deployments_site_number", true, "site, number", "")
	if err := app.Save(deployments); err != nil {
		return err
	}

	siteCollection.Fields.Add(&core.RelationField{Name: "active_deployment", CollectionId: deployments.Id, MaxSelect: 1})
	if err := app.Save(siteCollection); err != nil {
		return err
	}

	domains, err := app.FindCollectionByNameOrId(schema.Domains)
	if err != nil {
		return err
	}
	target, _ := domains.Fields.GetByName("target_kind").(*core.SelectField)
	target.Values = []string{schema.TargetContainer, schema.TargetRedirect, schema.TargetSite}
	// the site it serves; removing the site removes its domains
	domains.Fields.Add(&core.RelationField{Name: "site", CollectionId: siteCollection.Id, MaxSelect: 1, CascadeDelete: true})
	domains.AddIndex("idx_domains_site", false, "site", "site != ''")
	return app.Save(domains)
}

func downSites(app core.App) error {
	domains, err := app.FindCollectionByNameOrId(schema.Domains)
	if err != nil {
		return err
	}
	domains.RemoveIndex("idx_domains_site")
	domains.Fields.RemoveByName("site")
	target, _ := domains.Fields.GetByName("target_kind").(*core.SelectField)
	target.Values = []string{schema.TargetContainer, schema.TargetRedirect}
	if err := app.Save(domains); err != nil {
		return err
	}

	siteCollection, err := app.FindCollectionByNameOrId(schema.Sites)
	if err != nil {
		return err
	}
	siteCollection.Fields.RemoveByName("active_deployment")
	if err := app.Save(siteCollection); err != nil {
		return err
	}
	deployments, err := app.FindCollectionByNameOrId(schema.Deployments)
	if err != nil {
		return err
	}
	if err := app.Delete(deployments); err != nil {
		return err
	}
	return app.Delete(siteCollection)
}
