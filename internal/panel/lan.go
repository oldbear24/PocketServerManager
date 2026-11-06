package panel

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	"golang.org/x/net/publicsuffix"

	"github.com/oldbear24/PocketServerManager/internal/acme"
	"github.com/oldbear24/PocketServerManager/internal/dns"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// LAN names (#26) are served with certificates of the internal CA: a domain
// with internal TLS gets one at once, and it renews like one from ACME.
// Browsers trust them once the root certificate is installed, which the
// panel offers for download without an account, as the gateway sends it to
// anyone anyway.

// lanSuffixes are the names meant for local networks: home.arpa (RFC 8375),
// local (RFC 6762), internal (reserved by ICANN), and what home routers use.
var lanSuffixes = []string{"home.arpa", "internal", "local", "lan", "home", "localdomain"}

// checkInternalName tells whether the internal CA may sign a name: one under
// a suffix meant for local networks, under a top-level name the world does
// not know, or in a zone of a DNS server of the LAN, as for split horizon.
// Any other name has an owner on the internet, and every device that
// installed the root would trust a certificate for it: the CA checks nothing,
// and operators and labels can ask for internal TLS.
func (p *Panel) checkInternalName(name string) error {
	name = strings.TrimPrefix(dns.NormalizeZone(name), "*.")
	for _, suffix := range lanSuffixes {
		if name == suffix || strings.HasSuffix(name, "."+suffix) {
			return nil
		}
	}
	if suffix, icann := publicsuffix.PublicSuffix(name); !icann && !strings.Contains(suffix, ".") {
		return nil
	}
	zones, err := p.App.FindRecordsByFilter(schema.DNSZones, "", "", 0, 0)
	if err == nil {
		names := []string{}
		for _, zone := range zones {
			if provider, err := p.App.FindRecordById(schema.DNSProviders, zone.GetString("provider")); err == nil && lanDriver(provider) {
				names = append(names, zone.GetString("name"))
			}
		}
		if _, ok := dns.ZoneFor(name, names); ok {
			return nil
		}
	}
	return fmt.Errorf("%s is a name of the internet: the internal CA signs only names of the LAN, such as wiki.lan or nas.home.arpa, and names in a zone of AdGuard Home or Pi-hole", name)
}

// issueInternal signs a certificate of the internal CA and installs it.
func (p *Panel) issueInternal(record *core.Record) {
	authority := p.authority.Load()
	if authority == nil {
		p.acmeFailed(record, "", "The internal CA is not open yet.", false)
		return
	}
	for _, name := range certificateNames(record) {
		if err := p.checkInternalName(name); err != nil {
			p.acmeFailed(record, "", capitalize(err.Error())+".", false)
			return
		}
	}
	issued, err := authority.IssueLANCert(certificateNames(record))
	if err != nil {
		p.acmeFailed(record, "", "The internal CA could not sign it: "+err.Error(), false)
		return
	}
	p.acmeIssued(record, &acme.Certificate{
		ChainPEM: issued.ChainPEM, KeyPEM: issued.KeyPEM, NotBefore: issued.NotBefore, NotAfter: issued.NotAfter,
	}, "", schema.IssuerInternal)
}

func (p *Panel) bindLANRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	group.GET("/ca", p.handleCA).BindFunc(requireRole(schema.RoleViewer, schema.RoleOperator, schema.RoleAdmin))
}

// handleCA describes the internal CA for its card on the Domains page.
func (p *Panel) handleCA(e *core.RequestEvent) error {
	authority := p.authority.Load()
	if authority == nil {
		return e.Error(http.StatusServiceUnavailable, "The internal CA is not open yet.", nil)
	}
	agents, err := e.App.CountRecords(schema.Hosts, dbx.NewExp("cert_serial != ''"))
	if err != nil {
		return e.InternalServerError("", err)
	}
	lan, err := e.App.CountRecords(schema.Certificates, dbx.HashExp{"issuer": schema.IssuerInternal})
	if err != nil {
		return e.InternalServerError("", err)
	}
	return e.JSON(http.StatusOK, map[string]any{
		"root":             authority.RootInfo(),
		"agents":           agents,
		"lan_certificates": lan,
	})
}

// handleRootDER serves the root certificate in DER, the form Windows,
// Android, iOS and macOS open by themselves.
func (p *Panel) handleRootDER(e *core.RequestEvent) error {
	authority := p.authority.Load()
	if authority == nil {
		return e.Error(http.StatusServiceUnavailable, "The internal CA is not open yet.", nil)
	}
	e.Response.Header().Set("Content-Type", "application/x-x509-ca-cert")
	e.Response.Header().Set("Content-Disposition", `attachment; filename="pocketcloud-root-ca.crt"`)
	_, err := e.Response.Write(authority.RootDER())
	return err
}
