package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upMoves, downMoves)
}

// upMoves adds what moving databases between servers needs: the names a server
// answers to on an application's network, the copies kept of moved databases, and
// a record of each move. Moves are written by the panel only.
func upMoves(app core.App) error {
	networks, err := app.FindCollectionByNameOrId(schema.DBNetworks)
	if err != nil {
		return err
	}
	// the names of the application's own database service whose data moved here
	networks.Fields.Add(&core.JSONField{Name: "aliases"})
	if err := app.Save(networks); err != nil {
		return err
	}

	users, err := app.FindCollectionByNameOrId(schema.DBUsers)
	if err != nil {
		return err
	}
	// the application keeps this password in its own settings: it came with a
	// database from the application's own server, and the panel does not change it
	users.Fields.Add(&core.BoolField{Name: "app_password"})
	if err := app.Save(users); err != nil {
		return err
	}

	dumps, err := app.FindCollectionByNameOrId(schema.DBDumps)
	if err != nil {
		return err
	}
	if kind, ok := dumps.Fields.GetByName("kind").(*core.SelectField); ok {
		kind.Values = []string{schema.DumpExport, schema.DumpFinal, schema.DumpMove}
	}
	if err := app.Save(dumps); err != nil {
		return err
	}

	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return err
	}
	stacks, err := app.FindCollectionByNameOrId(schema.Stacks)
	if err != nil {
		return err
	}
	servers, err := app.FindCollectionByNameOrId(schema.DBServers)
	if err != nil {
		return err
	}
	commands, err := app.FindCollectionByNameOrId(schema.Commands)
	if err != nil {
		return err
	}
	people, err := app.FindCollectionByNameOrId(schema.Users)
	if err != nil {
		return err
	}

	moves := newCollection(schema.DBMoves, access{list: schema.RuleViewer, view: schema.RuleViewer})
	moves.Fields.Add(
		&core.RelationField{Name: "host", Required: true, CollectionId: hosts.Id, MaxSelect: 1, CascadeDelete: true},
		// the application whose databases moved
		&core.RelationField{Name: "stack", CollectionId: stacks.Id, MaxSelect: 1, CascadeDelete: true},
		&core.RelationField{Name: "target", CollectionId: servers.Id, MaxSelect: 1},
		&core.TextField{Name: "target_name", Max: 48},
		// the server the databases left, as it was
		&core.TextField{Name: "source_name", Max: 255},
		&core.TextField{Name: "source_kind", Max: 16},
		&core.TextField{Name: "source_container", Max: 255},
		&core.TextField{Name: "source_service", Max: 128},
		&core.TextField{Name: "source_volume", Max: 255},
		&core.TextField{Name: "source_version", Max: 64},
		&core.JSONField{Name: "databases"},
		// the passwords the moved databases keep, sealed until the command is delivered
		&core.TextField{Name: "secrets", Max: maxSealed, Hidden: true},
		&core.SelectField{Name: "status", MaxSelect: 1, Values: []string{schema.MoveRunning, schema.MoveDone, schema.MoveFailed}},
		&core.RelationField{Name: "command", CollectionId: commands.Id, MaxSelect: 1},
		&core.RelationField{Name: "requested_by", CollectionId: people.Id, MaxSelect: 1},
		&core.TextField{Name: "error", Max: 2048},
		&core.DateField{Name: "moved_at"},
		// a stack's old database container stays stopped until then, for going back
		&core.DateField{Name: "remove_after"},
		&core.DateField{Name: "removed_at"},
	)
	return app.Save(moves)
}

func downMoves(app core.App) error {
	moves, err := app.FindCollectionByNameOrId(schema.DBMoves)
	if err != nil {
		return err
	}
	if err := app.Delete(moves); err != nil {
		return err
	}

	networks, err := app.FindCollectionByNameOrId(schema.DBNetworks)
	if err != nil {
		return err
	}
	networks.Fields.RemoveByName("aliases")
	if err := app.Save(networks); err != nil {
		return err
	}

	users, err := app.FindCollectionByNameOrId(schema.DBUsers)
	if err != nil {
		return err
	}
	users.Fields.RemoveByName("app_password")
	if err := app.Save(users); err != nil {
		return err
	}

	dumps, err := app.FindCollectionByNameOrId(schema.DBDumps)
	if err != nil {
		return err
	}
	if kind, ok := dumps.Fields.GetByName("kind").(*core.SelectField); ok {
		kind.Values = []string{schema.DumpExport, schema.DumpFinal}
	}
	return app.Save(dumps)
}
