package e2e

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/dns/rewrite/rewritetest"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// internalCertificate waits for the certificate of the internal CA with
// these names to be issued.
func (a *acmeHarness) internalCertificate(names ...string) *core.Record {
	a.t.Helper()
	var found *core.Record
	a.waitFor("a certificate of the internal CA for "+strings.Join(names, ", "), func() bool {
		records, _ := a.app.FindRecordsByFilter(schema.Certificates, "issuer='internal'", "", 0, 0)
		for _, record := range records {
			var got []string
			_ = json.Unmarshal([]byte(record.GetString("names")), &got)
			if strings.Join(got, ",") == strings.Join(names, ",") && issued(record) {
				found = record
				return true
			}
		}
		return false
	})
	return found
}

// root is the root certificate as the panel gives it out, without an
// account: PEM and DER, which must be the same certificate.
func (a *acmeHarness) root() *x509.Certificate {
	a.t.Helper()
	status, pemBody := testutil.Do(a.t, a.handler, testutil.Request{URL: "/api/pc/ca.pem"})
	if status != http.StatusOK {
		a.t.Fatalf("ca.pem: %d %s", status, pemBody)
	}
	status, der := testutil.Do(a.t, a.handler, testutil.Request{URL: "/api/pc/ca.crt"})
	if status != http.StatusOK {
		a.t.Fatalf("ca.crt: %d %s", status, der)
	}
	block, _ := pem.Decode([]byte(pemBody))
	if block == nil || !bytes.Equal(block.Bytes, []byte(der)) {
		a.t.Fatal("ca.pem and ca.crt are not the same certificate")
	}
	root, err := x509.ParseCertificate([]byte(der))
	if err != nil {
		a.t.Fatal(err)
	}
	return root
}

// verifyOnEdge checks the chain the edge has for a certificate against a
// root, for a name.
func (a *acmeHarness) verifyOnEdge(cert *core.Record, root *x509.Certificate, name string) {
	a.t.Helper()
	files, _ := filepath.Glob(filepath.Join(a.edge.config.StateDir, "edge", "certs", cert.Id+"*.crt"))
	if len(files) != 1 {
		a.t.Fatalf("the edge has %d files of the certificate", len(files))
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		a.t.Fatal(err)
	}
	var chain []*x509.Certificate
	for block, rest := pem.Decode(raw); block != nil; block, rest = pem.Decode(rest) {
		parsed, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			a.t.Fatal(err)
		}
		chain = append(chain, parsed)
	}
	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(root)
	for _, c := range chain[1:] {
		intermediates.AddCert(c)
	}
	if _, err := chain[0].Verify(x509.VerifyOptions{
		DNSName: name, Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		a.t.Fatalf("the edge's chain for %s does not verify against the root: %v", name, err)
	}
}

// TestALANNameIsServedWithACertificateOfTheInternalCA: a domain with internal
// TLS gets a certificate of the internal CA at once, without an ACME account,
// and the edge serves it. Its chain verifies against the root that the panel
// gives out without an account, so a browser that has the root installed
// trusts it.
func TestALANNameIsServedWithACertificateOfTheInternalCA(t *testing.T) {
	a := &acmeHarness{dnsHarness: newDNSHarness(t)}
	a.docker.AddContainer("wiki", "wiki:1", nil)
	_, operator := testutil.NewUser(t, a.app, "op@example.com", schema.RoleOperator)
	_, viewer := testutil.NewUser(t, a.app, "viewer@example.com", schema.RoleViewer)

	status, raw := addDomain(t, a.harness, operator, fmt.Sprintf(
		`{"hostname":"wiki.lan","host":%q,"target_kind":"container","container":"wiki","port":3000,"tls":"internal","https_redirect":true}`, a.host.Id))
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, raw)
	}
	var domain struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(raw), &domain)

	cert := a.internalCertificate("wiki.lan")
	cert = a.served("wiki.lan", cert)
	if route := a.edgeRoute("wiki.lan"); route["tls"] != "valid" {
		t.Errorf("the route: %v", route)
	}
	root := a.root()
	a.verifyOnEdge(cert, root, "wiki.lan")
	if attempt := lastAttempt(t, cert); attempt["ok"] != true || attempt["account"] != "internal" {
		t.Errorf("the history: %v", attempt)
	}
	// no ACME account was needed, and nothing was asked of ACME
	if count, _ := a.app.CountRecords(schema.Certificates, dbx.HashExp{"issuer": schema.IssuerACME}); count != 0 {
		t.Errorf("%d certificates of ACME were ordered", count)
	}

	// the card of the internal CA
	status, raw = testutil.Do(t, a.handler, testutil.Request{URL: "/api/pc/ca", Token: viewer})
	var card struct {
		Root struct {
			Subject string `json:"subject"`
			SHA256  string `json:"sha256"`
		} `json:"root"`
		Agents int `json:"agents"`
		LAN    int `json:"lan_certificates"`
	}
	_ = json.Unmarshal([]byte(raw), &card)
	if status != http.StatusOK || card.Root.Subject != "PocketCloud Root CA" || card.Agents < 1 || card.LAN != 1 || len(card.Root.SHA256) != 95 {
		t.Errorf("the CA: %d %s", status, raw)
	}

	// an admin asks for a wildcard of the internal CA; an operator cannot
	ask := func(token, names string) (int, string) {
		return testutil.Do(t, a.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/certificates/acme", Token: token,
			Body: `{"names":` + names + `,"issuer":"internal"}`})
	}
	if status, _ := ask(operator, `["*.home.arpa"]`); status != http.StatusForbidden {
		t.Errorf("an operator asked for a certificate: %d", status)
	}
	// Chrome would refuse it
	if status, raw := ask(a.admin, `["*.lan"]`); status != http.StatusBadRequest || !strings.Contains(raw, "name each host, such as wiki.lan") {
		t.Errorf("a wildcard under a top-level name: %d %s", status, raw)
	}
	if status, raw := ask(a.admin, `["*.home.arpa"]`); status != http.StatusCreated {
		t.Fatalf("%d %s", status, raw)
	}
	wildcard := a.internalCertificate("*.home.arpa")
	if status, _ := ask(a.admin, `["*.home.arpa"]`); status != http.StatusConflict {
		t.Errorf("the same names twice: %d", status)
	}
	// a domain it covers needs no certificate of its own
	if status, raw := addDomain(t, a.harness, operator, fmt.Sprintf(
		`{"hostname":"nas.home.arpa","host":%q,"target_kind":"container","container":"wiki","port":3000,"tls":"internal"}`, a.host.Id)); status != http.StatusOK {
		t.Fatalf("%d %s", status, raw)
	}
	a.verifyOnEdge(a.served("nas.home.arpa", wildcard), root, "nas.home.arpa")
	if count, _ := a.app.CountRecords(schema.Certificates, dbx.HashExp{"issuer": schema.IssuerInternal}); count != 2 {
		t.Errorf("%d certificates of the internal CA, not those of wiki.lan and *.home.arpa", count)
	}

	// back to automatic TLS, the internal CA's certificates no longer serve it
	status, raw = testutil.Do(t, a.handler, testutil.Request{Method: http.MethodPatch, URL: "/api/collections/domains/records/" + domain.ID, Token: operator, Body: `{"tls":"auto"}`})
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, raw)
	}
	a.waitFor("the route to lose the internal certificate", func() bool {
		route := a.edgeRoute("wiki.lan")
		return route["tls"] == "none" && route["certificate"] == nil
	})
	a.certificateFor("wiki.lan")
}

func (a *acmeHarness) record(collection, id string) *core.Record {
	a.t.Helper()
	record, err := a.app.FindRecordById(collection, id)
	if err != nil {
		a.t.Fatal(err)
	}
	return record
}

// lanDNS connects a DNS server of a LAN, reached through the edge's agent.
func (a *acmeHarness) lanDNS(name, kind string, credentials map[string]string, zones ...string) (int, map[string]any) {
	a.t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name, "kind": kind, "credentials": credentials, "zones": zones, "host": a.host.Id})
	status, raw := testutil.Do(a.t, a.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/dns-providers", Token: a.admin, Body: string(body)})
	var out map[string]any
	_ = json.Unmarshal([]byte(raw), &out)
	return status, out
}

func providerStatus(out map[string]any) (string, string) {
	provider, _ := out["provider"].(map[string]any)
	status, _ := provider["status"].(string)
	message, _ := provider["last_error"].(string)
	return status, message
}

// TestALANNameResolvesThroughTheDNSServerOfItsNetwork: AdGuard Home and
// Pi-hole on the server's network get the records of LAN names, through the
// server's agent, pointing at the server's address on that network as the
// agent reports it. They
// answer at once, so the domain's DNS is ok without waiting for the world,
// and their credentials never reach the command history.
func TestALANNameResolvesThroughTheDNSServerOfItsNetwork(t *testing.T) {
	a := &acmeHarness{dnsHarness: newDNSHarness(t)}
	a.docker.AddContainer("wiki", "wiki:1", nil)
	// the address on its network is the one its agent reports
	var addr string
	a.waitFor("the agent to report its address", func() bool {
		addr = a.harness.host(a.host.Id).GetString("ip4")
		return addr != ""
	})
	_, viewer := testutil.NewUser(t, a.app, "viewer@example.com", schema.RoleViewer)
	adguard := rewritetest.NewAdGuard(t, "admin", "adguard-secret", rewritetest.Rewrite{Domain: "router.lan", Answer: "192.168.1.1"})
	pihole := rewritetest.NewPiHole(t, "pihole-secret")

	// a DNS server of a LAN needs a server to reach it through
	body := fmt.Sprintf(`{"name":"Home","kind":"adguard","credentials":{"url":%q,"username":"admin","password":"adguard-secret"},"zones":["lan"]}`, adguard.URL)
	if status, raw := testutil.Do(t, a.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/dns-providers", Token: a.admin, Body: body}); status != http.StatusBadRequest || !strings.Contains(raw, "Choose the server") {
		t.Errorf("without a server: %d %s", status, raw)
	}
	if status, out := a.lanDNS("Home", "adguard", map[string]string{"url": "192.168.1.2:3000"}, "lan"); status != http.StatusBadRequest {
		t.Errorf("an address that is not a URL: %d %v", status, out)
	}
	status, out := a.lanDNS("Home", "adguard", map[string]string{"url": adguard.URL, "username": "admin", "password": "adguard-secret"}, "lan")
	if state, message := providerStatus(out); status != http.StatusOK || state != schema.DNSOK {
		t.Fatalf("connecting AdGuard Home: %d %s %s", status, state, message)
	}
	if adguard.Calls("/control/rewrite/list") == 0 {
		t.Fatal("AdGuard Home was not asked")
	}
	status, out = a.lanDNS("Pi", "pihole", map[string]string{"url": pihole.URL + "/admin", "password": "pihole-secret"}, "home.arpa")
	if state, message := providerStatus(out); status != http.StatusOK || state != schema.DNSOK {
		t.Fatalf("connecting Pi-hole: %d %s %s", status, state, message)
	}

	// a LAN name gets its record at once, and is ok
	wiki := a.domain("wiki.lan")
	a.waitFor("wiki.lan to resolve", func() bool {
		return a.record(schema.Domains, wiki.Id).GetString("dns_status") == schema.DNSOK
	})
	if got := adguard.Rewrites(); len(got) != 2 || got[1].Domain != "wiki.lan" || got[1].Answer != addr {
		t.Fatalf("the rewrites: %+v", got)
	}
	nas := a.domain("nas.home.arpa")
	a.waitFor("nas.home.arpa to resolve", func() bool {
		return a.record(schema.Domains, nas.Id).GetString("dns_status") == schema.DNSOK
	})
	if got := strings.Join(pihole.Hosts(), "; "); got != addr+" nas.home.arpa" {
		t.Fatalf("Pi-hole's dns.hosts: %s", got)
	}
	if pihole.Open() != 0 {
		t.Errorf("%d sessions of Pi-hole are still open", pihole.Open())
	}

	// the domain's page says so, without asking the world
	status, raw := testutil.Do(t, a.handler, testutil.Request{URL: "/api/pc/domains/" + wiki.Id + "/dns?check=1", Token: viewer})
	if status != http.StatusOK || !strings.Contains(raw, `"lan":true`) || strings.Contains(raw, `"propagation"`) || !strings.Contains(raw, `"value":"`+addr+`"`) {
		t.Errorf("the domain's DNS: %d %s", status, raw)
	}

	// every read and change was a command of the agent, without the credentials
	rewrites, err := a.app.FindRecordsByFilter(schema.Commands, "type='dns.rewrite'", "", 0, 0)
	if err != nil || len(rewrites) == 0 {
		t.Fatalf("%d commands, %v", len(rewrites), err)
	}
	for _, command := range rewrites {
		stored, _ := json.Marshal(command.FieldsData())
		if strings.Contains(string(stored), "secret") || command.GetString("host") != a.host.Id {
			t.Fatalf("a command: %s", stored)
		}
	}

	// a removed domain takes its record
	if status, raw := testutil.Do(t, a.handler, testutil.Request{Method: http.MethodDelete, URL: "/api/collections/domains/records/" + wiki.Id, Token: a.admin}); status != http.StatusNoContent {
		t.Fatalf("%d %s", status, raw)
	}
	a.waitFor("the rewrite to go", func() bool { return len(adguard.Rewrites()) == 1 })
	if got := adguard.Rewrites()[0]; got.Domain != "router.lan" {
		t.Errorf("a rewrite PocketCloud did not make went: %+v", got)
	}

	// a wrong password says so
	status, raw = testutil.Do(t, a.handler, testutil.Request{Method: http.MethodPatch, URL: "/api/pc/dns-providers/" + out["provider"].(map[string]any)["id"].(string), Token: a.admin,
		Body: fmt.Sprintf(`{"credentials":{"url":%q,"password":"wrong"}}`, pihole.URL)})
	_ = json.Unmarshal([]byte(raw), &out)
	if state, message := providerStatus(out); status != http.StatusOK || state != schema.DNSError || !strings.Contains(message, "It refused the password of Pi-hole's web interface") {
		t.Errorf("a wrong password: %d %s %q", status, state, message)
	}
}
