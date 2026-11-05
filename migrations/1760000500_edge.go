package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upEdge, downEdge)
}

// hostEdgeFields are the edge proxy's state on a host: what the panel wants it
// to serve and what the agent says it serves.
var hostEdgeFields = []core.Field{
	// the digest and revision of the config the panel generated last
	&core.TextField{Name: "edge_digest", Max: 64},
	&core.NumberField{Name: "edge_revision", OnlyInt: true},
	// what the agent reports: the config that loaded, and how the edge is
	&core.TextField{Name: "edge_applied_digest", Max: 64},
	&core.NumberField{Name: "edge_applied_revision", OnlyInt: true},
	&core.BoolField{Name: "edge_running"},
	&core.TextField{Name: "edge_error", Max: 2048},
	&core.NumberField{Name: "edge_requests_per_s"},
	&core.TextField{Name: "edge_caddy_version", Max: 32},
}

// upEdge adds the edge proxy of #20: the domains an edge serves and the
// certificates it serves them with. Later issues add to both: #21 a site as a
// target, #22 the zone and DNS state, #23 certificates from ACME.
func upEdge(app core.App) error {
	hosts, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		return err
	}
	hosts.Fields.Add(hostEdgeFields...)
	if err := app.Save(hosts); err != nil {
		return err
	}

	// certificates are uploaded and removed through the panel's routes, which
	// check them and put them on the edges; the key is sealed and hidden
	certificates := newCollection(schema.Certificates, access{list: schema.RuleViewer, view: schema.RuleViewer})
	certificates.Fields.Add(
		&core.JSONField{Name: "names", Required: true},
		&core.SelectField{Name: "issuer", MaxSelect: 1, Values: []string{schema.IssuerManual, schema.IssuerACME, schema.IssuerInternal}},
		&core.DateField{Name: "not_before"},
		&core.DateField{Name: "not_after"},
		&core.SelectField{Name: "status", MaxSelect: 1, Values: []string{schema.CertActive, schema.CertExpired}},
		&core.TextField{Name: "chain_pem", Max: 1 << 18},
		&core.TextField{Name: "key_pem", Max: maxSealed, Hidden: true},
		// the start of the leaf's SHA-256 fingerprint
		&core.TextField{Name: "fingerprint", Max: 64},
		&core.TextField{Name: "subject", Max: 512},
		&core.TextField{Name: "issuer_name", Max: 512},
		// the hosts it is installed on, with the file it got there: {host: file}
		&core.JSONField{Name: "installed"},
		&core.TextField{Name: "note", Max: 512},
		&core.TextField{Name: "last_error", Max: 2048},
	)
	certificates.AddIndex("idx_certificates_fingerprint", true, "fingerprint", "fingerprint != ''")
	if err := app.Save(certificates); err != nil {
		return err
	}

	domains := newCollection(schema.Domains, access{
		list: schema.RuleViewer, view: schema.RuleViewer,
		create: schema.RuleOperator, update: schema.RuleOperator, del: schema.RuleOperator,
	})
	domains.Fields.Add(
		&core.TextField{Name: "hostname", Required: true, Max: 253},
		// the edge that serves it
		&core.RelationField{Name: "host", Required: true, CollectionId: hosts.Id, MaxSelect: 1, CascadeDelete: true},
		&core.SelectField{Name: "target_kind", Required: true, MaxSelect: 1, Values: []string{schema.TargetContainer, schema.TargetRedirect}},
		// a container on the same host, by name, and its port
		&core.TextField{Name: "container", Max: 255},
		&core.NumberField{Name: "port", OnlyInt: true},
		&core.TextField{Name: "redirect_to", Max: 2048},
		&core.BoolField{Name: "redirect_permanent"},
		// auto: HTTPS with a certificate that covers the name, if there is one
		&core.SelectField{Name: "tls", MaxSelect: 1, Values: []string{schema.TLSAuto, schema.TLSOff}},
		&core.BoolField{Name: "https_redirect"},
		&core.BoolField{Name: "hsts"},
		&core.BoolField{Name: "compress"},
	)
	domains.AddIndex("idx_domains_hostname", true, "hostname", "")
	domains.AddIndex("idx_domains_host", false, "host", "")
	return app.Save(domains)
}

func downEdge(app core.App) error {
	for _, name := range []string{schema.Domains, schema.Certificates} {
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
	for _, field := range hostEdgeFields {
		hosts.Fields.RemoveByName(field.GetName())
	}
	return app.Save(hosts)
}
