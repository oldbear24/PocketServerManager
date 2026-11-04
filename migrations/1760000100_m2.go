package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upM2, downM2)
}

// upM2 adds what the second milestone keeps on existing collections.
func upM2(app core.App) error {
	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return err
	}
	hosts.Fields.Add(
		&core.NumberField{Name: "load5"},
		&core.NumberField{Name: "load15"},
		&core.TextField{Name: "compose_version", Max: 64},
	)
	if err := app.Save(hosts); err != nil {
		return err
	}

	stacks, err := app.FindCollectionByNameOrId(schema.Stacks)
	if err != nil {
		return err
	}
	stacks.Fields.Add(
		&core.NumberField{Name: "deployed_revision", OnlyInt: true},
		&core.DateField{Name: "deployed_at"},
		&core.TextField{Name: "source", Max: 32},
	)

	return app.Save(stacks)
}

func downM2(app core.App) error {
	stacks, err := app.FindCollectionByNameOrId(schema.Stacks)
	if err != nil {
		return err
	}
	stacks.Fields.RemoveByName("deployed_revision")
	stacks.Fields.RemoveByName("deployed_at")
	stacks.Fields.RemoveByName("source")
	if err := app.Save(stacks); err != nil {
		return err
	}

	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return err
	}
	hosts.Fields.RemoveByName("load5")
	hosts.Fields.RemoveByName("load15")
	hosts.Fields.RemoveByName("compose_version")
	return app.Save(hosts)
}
