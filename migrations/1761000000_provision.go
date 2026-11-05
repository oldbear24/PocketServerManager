package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upProvision, downProvision)
}

// upProvision lets a domain be an alias of another (#24): instead of A and
// AAAA records, its zone gets a CNAME to the other name, as a www name has
// next to the site it redirects to.
func upProvision(app core.App) error {
	domains, err := app.FindCollectionByNameOrId(schema.Domains)
	if err != nil {
		return err
	}
	domains.Fields.Add(&core.TextField{Name: "dns_alias", Max: 253})
	return app.Save(domains)
}

func downProvision(app core.App) error {
	domains, err := app.FindCollectionByNameOrId(schema.Domains)
	if err != nil {
		return err
	}
	domains.Fields.RemoveByName("dns_alias")
	return app.Save(domains)
}
