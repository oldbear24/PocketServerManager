package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upLAN, downLAN)
}

// upLAN adds what LAN names need (#26):
//   - the internal CA's intermediate for their certificates;
//   - the internal TLS mode of a domain;
//   - the server whose agent reaches a DNS provider on the LAN, such as
//     AdGuard Home or Pi-hole.
func upLAN(app core.App) error {
	internalCA, err := app.FindCollectionByNameOrId(schema.InternalCA)
	if err != nil {
		return err
	}
	internalCA.Fields.Add(
		&core.TextField{Name: "lan_intermediate_cert", Max: maxLongText},
		&core.TextField{Name: "lan_intermediate_key", Max: maxSealed, Hidden: true},
	)
	if err := app.Save(internalCA); err != nil {
		return err
	}

	domains, err := app.FindCollectionByNameOrId(schema.Domains)
	if err != nil {
		return err
	}
	if field, ok := domains.Fields.GetByName("tls").(*core.SelectField); ok {
		field.Values = []string{schema.TLSAuto, schema.TLSOff, schema.TLSInternal}
	}
	if err := app.Save(domains); err != nil {
		return err
	}

	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return err
	}
	providers, err := app.FindCollectionByNameOrId(schema.DNSProviders)
	if err != nil {
		return err
	}
	providers.Fields.Add(&core.RelationField{Name: "host", CollectionId: hosts.Id, MaxSelect: 1})
	return app.Save(providers)
}

func downLAN(app core.App) error {
	providers, err := app.FindCollectionByNameOrId(schema.DNSProviders)
	if err != nil {
		return err
	}
	providers.Fields.RemoveByName("host")
	if err := app.Save(providers); err != nil {
		return err
	}
	domains, err := app.FindCollectionByNameOrId(schema.Domains)
	if err != nil {
		return err
	}
	if field, ok := domains.Fields.GetByName("tls").(*core.SelectField); ok {
		field.Values = []string{schema.TLSAuto, schema.TLSOff}
	}
	if err := app.Save(domains); err != nil {
		return err
	}
	internalCA, err := app.FindCollectionByNameOrId(schema.InternalCA)
	if err != nil {
		return err
	}
	internalCA.Fields.RemoveByName("lan_intermediate_cert")
	internalCA.Fields.RemoveByName("lan_intermediate_key")
	return app.Save(internalCA)
}
