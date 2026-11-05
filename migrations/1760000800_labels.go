package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upLabels, downLabels)
}

// upLabels adds domains that the labels of a stack's services ask for (#25).
func upLabels(app core.App) error {
	stacks, err := app.FindCollectionByNameOrId(schema.Stacks)
	if err != nil {
		return err
	}
	// what was wrong with the labels at the last deploy
	stacks.Fields.Add(
		&core.JSONField{Name: "label_problems", MaxSize: 64 << 10},
		// when the labels of the deployed revision were last read
		&core.DateField{Name: "labels_at"},
	)
	if err := app.Save(stacks); err != nil {
		return err
	}

	domains, err := app.FindCollectionByNameOrId(schema.Domains)
	if err != nil {
		return err
	}
	domains.Fields.Add(
		// label: a pocketcloud.domain label asks for it; empty: added by hand
		&core.SelectField{Name: "origin", MaxSelect: 1, Values: []string{schema.OriginLabel}},
		// the stack and the service whose label it is; a removed stack leaves it empty
		&core.RelationField{Name: "stack", CollectionId: stacks.Id, MaxSelect: 1},
		&core.TextField{Name: "service", Max: 255},
		// the label is gone: the domain stays until someone confirms its removal
		&core.BoolField{Name: "label_removed"},
		// user:bcrypt-hash lines, sealed
		&core.TextField{Name: "basic_auth", Max: maxSealed, Hidden: true},
	)
	domains.AddIndex("idx_domains_stack", false, "stack", "stack != ''")
	return app.Save(domains)
}

func downLabels(app core.App) error {
	domains, err := app.FindCollectionByNameOrId(schema.Domains)
	if err != nil {
		return err
	}
	domains.RemoveIndex("idx_domains_stack")
	for _, name := range []string{"origin", "stack", "service", "label_removed", "basic_auth"} {
		domains.Fields.RemoveByName(name)
	}
	if err := app.Save(domains); err != nil {
		return err
	}

	stacks, err := app.FindCollectionByNameOrId(schema.Stacks)
	if err != nil {
		return err
	}
	stacks.Fields.RemoveByName("label_problems")
	stacks.Fields.RemoveByName("labels_at")
	return app.Save(stacks)
}
