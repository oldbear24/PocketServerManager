package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

const maxDump = 1 << 30 // SQL exports and final dumps, as large as an artifact may be

func init() {
	m.Register(upDatabases, downDatabases)
}

// upDatabases adds the shared database servers and the databases of applications.
// Everything is written by the panel through its own routes, which generate the
// passwords and queue the commands: the REST API only reads.
func upDatabases(app core.App) error {
	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return err
	}
	stacks, err := app.FindCollectionByNameOrId(schema.Stacks)
	if err != nil {
		return err
	}
	commands, err := app.FindCollectionByNameOrId(schema.Commands)
	if err != nil {
		return err
	}
	users, err := app.FindCollectionByNameOrId(schema.Users)
	if err != nil {
		return err
	}

	servers := newCollection(schema.DBServers, access{list: schema.RuleViewer, view: schema.RuleViewer})
	servers.Fields.Add(
		&core.RelationField{Name: "host", Required: true, CollectionId: hosts.Id, MaxSelect: 1, CascadeDelete: true},
		&core.TextField{Name: "name", Required: true, Max: 48, Pattern: `^[a-z0-9][a-z0-9_-]*$`},
		&core.SelectField{Name: "engine", Required: true, MaxSelect: 1, Values: []string{schema.EngineMariaDB, schema.EnginePostgres}},
		&core.TextField{Name: "version", Max: 64},
		&core.SelectField{Name: "kind", Required: true, MaxSelect: 1, Values: []string{schema.DBServerShared, schema.DBServerDedicated}},
		&core.TextField{Name: "image", Max: 512},
		&core.TextField{Name: "container_ref", Max: 255},
		&core.TextField{Name: "service", Max: 128},
		&core.RelationField{Name: "stack", CollectionId: stacks.Id, MaxSelect: 1, CascadeDelete: true},
		&core.TextField{Name: "volume", Max: 255},
		&core.NumberField{Name: "port", OnlyInt: true},
		&core.TextField{Name: "admin_user", Max: 128},
		&core.TextField{Name: "admin_password", Max: maxSealed, Hidden: true},
		&core.NumberField{Name: "max_connections", OnlyInt: true},
		&core.NumberField{Name: "connections", OnlyInt: true},
		&core.JSONField{Name: "extensions"},
		&core.TextField{Name: "status", Max: 32},
		&core.NumberField{Name: "size_bytes", OnlyInt: true},
		&core.DateField{Name: "stats_at"},
		&core.TextField{Name: "last_error", Max: 2048},
	)
	servers.AddIndex("idx_db_servers_host_name", true, "host, name", "")
	if err := app.Save(servers); err != nil {
		return err
	}

	databases := newCollection(schema.Databases, access{list: schema.RuleViewer, view: schema.RuleViewer})
	databases.Fields.Add(
		&core.RelationField{Name: "server", Required: true, CollectionId: servers.Id, MaxSelect: 1, CascadeDelete: true},
		&core.TextField{Name: "name", Required: true, Max: 63, Pattern: `^[a-z][a-z0-9_]*$`},
		&core.SelectField{Name: "owner_kind", MaxSelect: 1, Values: []string{"none", "stack", "site"}},
		&core.RelationField{Name: "stack", CollectionId: stacks.Id, MaxSelect: 1},
		&core.NumberField{Name: "size_bytes", OnlyInt: true},
		&core.NumberField{Name: "connections", OnlyInt: true},
		&core.DateField{Name: "temporary_until"},
		&core.TextField{Name: "status", Max: 32},
		&core.TextField{Name: "last_error", Max: 2048},
	)
	databases.AddIndex("idx_databases_server_name", true, "server, name", "")
	if err := app.Save(databases); err != nil {
		return err
	}

	dbUsers := newCollection(schema.DBUsers, access{list: schema.RuleViewer, view: schema.RuleViewer})
	dbUsers.Fields.Add(
		&core.RelationField{Name: "server", Required: true, CollectionId: servers.Id, MaxSelect: 1, CascadeDelete: true},
		&core.TextField{Name: "name", Required: true, Max: 63, Pattern: `^[a-z][a-z0-9_]*$`},
		&core.TextField{Name: "password", Max: maxSealed, Hidden: true},
		// a new password waits here until the server has taken it
		&core.TextField{Name: "pending_password", Max: maxSealed, Hidden: true},
		&core.RelationField{Name: "databases", CollectionId: databases.Id, MaxSelect: 100},
		&core.TextField{Name: "secret_name", Max: 128},
		&core.DateField{Name: "rotated_at"},
	)
	dbUsers.AddIndex("idx_db_users_server_name", true, "server, name", "")
	if err := app.Save(dbUsers); err != nil {
		return err
	}

	networks := newCollection(schema.DBNetworks, access{list: schema.RuleViewer, view: schema.RuleViewer})
	networks.Fields.Add(
		&core.RelationField{Name: "server", Required: true, CollectionId: servers.Id, MaxSelect: 1, CascadeDelete: true},
		&core.TextField{Name: "name", Required: true, Max: 128},
		&core.RelationField{Name: "stack", CollectionId: stacks.Id, MaxSelect: 1, CascadeDelete: true},
	)
	networks.AddIndex("idx_db_networks_server_stack", true, "server, stack", "")
	if err := app.Save(networks); err != nil {
		return err
	}

	// dumps hold every row of a database: only admins see them
	dumps := newCollection(schema.DBDumps, access{list: schema.RuleAdmin, view: schema.RuleAdmin, del: schema.RuleAdmin})
	dumps.Fields.Add(
		&core.RelationField{Name: "server", Required: true, CollectionId: servers.Id, MaxSelect: 1, CascadeDelete: true},
		&core.TextField{Name: "database_name", Max: 63},
		&core.SelectField{Name: "kind", MaxSelect: 1, Values: []string{schema.DumpExport, schema.DumpFinal}},
		&core.RelationField{Name: "command", CollectionId: commands.Id, MaxSelect: 1},
		&core.RelationField{Name: "requested_by", CollectionId: users.Id, MaxSelect: 1},
		&core.FileField{Name: "file", MaxSelect: 1, MaxSize: maxDump, Protected: true},
		&core.NumberField{Name: "size_bytes", OnlyInt: true},
		&core.DateField{Name: "expires_at"},
	)
	return app.Save(dumps)
}

func downDatabases(app core.App) error {
	for _, name := range []string{schema.DBDumps, schema.DBNetworks, schema.DBUsers, schema.Databases, schema.DBServers} {
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
