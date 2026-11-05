package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upDNS, downDNS)
}

// hostDNSFields are the public addresses of a host that the A and AAAA
// records of its domains point at. The addresses the agent reports (ip4,
// ip6) are those of its interfaces, which may be private behind NAT.
var hostDNSFields = []core.Field{
	&core.TextField{Name: "public_ip4", Max: 45},
	&core.TextField{Name: "public_ip6", Max: 45},
}

var dnsStates = []string{schema.DNSUnknown, schema.DNSOK, schema.DNSError, schema.DNSPending, schema.DNSDrift, schema.DNSManual, schema.DNSNone}

// upDNS adds the DNS providers of #22: connected accounts, the zones found
// there, the records PocketCloud created in them, and the DNS state of a
// domain.
func upDNS(app core.App) error {
	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return err
	}
	hosts.Fields.Add(hostDNSFields...)
	if err := app.Save(hosts); err != nil {
		return err
	}

	// providers are connected, changed and removed through the panel's routes,
	// which seal their credentials; the kind is checked there too, so tests can
	// connect fakes
	providers := newCollection(schema.DNSProviders, access{list: schema.RuleViewer, view: schema.RuleViewer})
	providers.Fields.Add(
		&core.TextField{Name: "name", Required: true, Max: 100},
		&core.TextField{Name: "kind", Required: true, Max: 32},
		&core.TextField{Name: "credentials", Max: maxSealed, Hidden: true},
		&core.SelectField{Name: "status", MaxSelect: 1, Values: dnsStates},
		&core.DateField{Name: "last_check"},
		&core.TextField{Name: "last_error", Max: 2048},
	)
	providers.AddIndex("idx_dns_providers_name", true, "name", "")
	if err := app.Save(providers); err != nil {
		return err
	}

	zones := newCollection(schema.DNSZones, access{list: schema.RuleViewer, view: schema.RuleViewer})
	zones.Fields.Add(
		&core.RelationField{Name: "provider", Required: true, CollectionId: providers.Id, MaxSelect: 1, CascadeDelete: true},
		&core.TextField{Name: "name", Required: true, Max: 253},
		// managed zones get the records of their domains; others are only known
		&core.BoolField{Name: "managed"},
		&core.SelectField{Name: "status", MaxSelect: 1, Values: dnsStates},
		&core.DateField{Name: "last_check"},
		&core.TextField{Name: "last_error", Max: 2048},
	)
	zones.AddIndex("idx_dns_zones_name", true, "name", "")
	if err := app.Save(zones); err != nil {
		return err
	}

	domains, err := app.FindCollectionByNameOrId(schema.Domains)
	if err != nil {
		return err
	}
	domains.Fields.Add(
		// the zone that holds the name, found by the longest suffix
		&core.RelationField{Name: "zone", CollectionId: zones.Id, MaxSelect: 1},
		&core.SelectField{Name: "dns_status", MaxSelect: 1, Values: dnsStates},
		&core.TextField{Name: "dns_error", Max: 2048},
	)
	if err := app.Save(domains); err != nil {
		return err
	}

	// one row per record PocketCloud created, which is how it knows its own:
	// libdns does not expose the providers' ids
	records := newCollection(schema.DNSRecords, access{list: schema.RuleViewer, view: schema.RuleViewer})
	records.Fields.Add(
		&core.RelationField{Name: "zone", Required: true, CollectionId: zones.Id, MaxSelect: 1, CascadeDelete: true},
		&core.RelationField{Name: "domain", CollectionId: domains.Id, MaxSelect: 1},
		&core.TextField{Name: "name", Required: true, Max: 253},
		&core.TextField{Name: "type", Required: true, Max: 16},
		&core.TextField{Name: "value", Required: true, Max: 4096},
		&core.NumberField{Name: "ttl", OnlyInt: true},
		&core.SelectField{Name: "status", MaxSelect: 1, Values: dnsStates},
		&core.TextField{Name: "last_error", Max: 2048},
		&core.DateField{Name: "last_check"},
		// what the zone holds instead, once it drifted
		&core.JSONField{Name: "drift"},
	)
	records.AddIndex("idx_dns_records_key", true, "zone, name, type, value", "")
	records.AddIndex("idx_dns_records_domain", false, "domain", "")
	return app.Save(records)
}

func downDNS(app core.App) error {
	domains, err := app.FindCollectionByNameOrId(schema.Domains)
	if err != nil {
		return err
	}
	for _, name := range []string{"zone", "dns_status", "dns_error"} {
		domains.Fields.RemoveByName(name)
	}
	if err := app.Save(domains); err != nil {
		return err
	}

	for _, name := range []string{schema.DNSRecords, schema.DNSZones, schema.DNSProviders} {
		collection, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			return err
		}
		if err := app.Delete(collection); err != nil {
			return err
		}
	}

	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return err
	}
	for _, field := range hostDNSFields {
		hosts.Fields.RemoveByName(field.GetName())
	}
	return app.Save(hosts)
}
