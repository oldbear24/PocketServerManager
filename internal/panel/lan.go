package panel

import (
	"net/http"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"

	"github.com/oldbear24/PocketServerManager/internal/acme"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// LAN names (#26) are served with certificates of the internal CA: a domain
// with internal TLS gets one at once, and it renews like one from ACME.
// Browsers trust them once the root certificate is installed, which the
// panel offers for download without an account, as the gateway sends it to
// anyone anyway.

// issueInternal signs a certificate of the internal CA and installs it.
func (p *Panel) issueInternal(record *core.Record) {
	authority := p.authority.Load()
	if authority == nil {
		p.acmeFailed(record, "", "The internal CA is not open yet.", false)
		return
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
