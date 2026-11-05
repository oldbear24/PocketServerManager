package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/acme/acmetest"
	pcdns "github.com/oldbear24/PocketServerManager/internal/dns"
	"github.com/oldbear24/PocketServerManager/internal/edge"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// acmeHarness is the DNS harness with a test CA: Pebble, whose validation
// fetches HTTP-01 answers from the files the edge agent writes, as Caddy
// serves them on port 80, and asks a DNS server that answers TXT records from
// the fake Cloudflare.
type acmeHarness struct {
	*dnsHarness
	ca *acmetest.CA
}

type acmeOptions struct {
	validity time.Duration
	// unreachable: nothing answers where the CA fetches HTTP-01 answers
	unreachable bool
}

func newACMEHarness(t *testing.T, opts acmeOptions, zones ...string) *acmeHarness {
	d := newDNSHarness(t, zones...)
	ca := startCA(t, d, opts)

	d.panel.SetACMEHTTPClient(ca.Client)
	d.panel.SetACMEPropagation(20*time.Second, 50*time.Millisecond)
	// the CA's DNS server is the zones' name server: asked over the network,
	// as the panel asks real ones
	checker := d.resolvers.checker()
	faked, real := checker.Lookup, pcdns.NewChecker().Lookup
	checker.NameServers = func(context.Context, string) ([]string, error) { return []string{ca.DNS.Addr}, nil }
	checker.Lookup = func(ctx context.Context, server, name, kind string) ([]string, error) {
		if kind == "TXT" {
			return real(ctx, ca.DNS.Addr, name, kind)
		}
		return faked(ctx, server, name, kind)
	}
	d.panel.SetDNSChecker(checker)
	return &acmeHarness{dnsHarness: d, ca: ca}
}

// startCA starts a CA for the harness's edge.
func startCA(t *testing.T, d *dnsHarness, opts acmeOptions) *acmetest.CA {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if opts.unreachable {
		_ = listener.Close()
	} else {
		// what the edge's Caddy does on port 80 for /.well-known/acme-challenge/
		server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.FileServer(http.Dir(filepath.Join(d.edge.config.StateDir, "edge", "acme")))}
		go func() { _ = server.Serve(listener) }()
		t.Cleanup(func() { _ = server.Close() })
	}

	ca := acmetest.Start(t, acmetest.Options{HTTPPort: port, Validity: opts.validity})
	ca.DNS.Lookup(func(name string, qtype uint16) []string {
		if qtype != dns.TypeTXT {
			return nil
		}
		values := []string{}
		for _, zone := range []string{"example.test", "example.com"} {
			for _, record := range d.api.Records(zone) {
				if record.Type == "TXT" && strings.EqualFold(record.Name+".", name) {
					values = append(values, strings.Trim(record.Content, `"`))
				}
			}
		}
		return values
	})
	return ca
}

// setAccount sets the primary ACME account.
func (a *acmeHarness) setAccount(role, directory string) (int, string) {
	a.t.Helper()
	body := fmt.Sprintf(`{"directory_url":%q,"email":"admin@example.com"}`, directory)
	return testutil.Do(a.t, a.handler, testutil.Request{Method: "PUT", URL: "/api/pc/acme-accounts/" + role, Token: a.admin, Body: body})
}

func (a *acmeHarness) mustSetAccount() {
	a.t.Helper()
	if status, raw := a.setAccount(schema.ACMEPrimary, a.ca.DirectoryURL); status != http.StatusOK {
		a.t.Fatalf("the account: %d %s", status, raw)
	}
}

// certificateFor waits for the ACME certificate of exactly these names.
func (a *acmeHarness) certificateFor(names ...string) *core.Record {
	a.t.Helper()
	var found *core.Record
	a.waitFor("a certificate for "+strings.Join(names, ", "), func() bool {
		records, _ := a.app.FindRecordsByFilter(schema.Certificates, "issuer='acme'", "", 0, 0)
		for _, record := range records {
			var got []string
			_ = json.Unmarshal([]byte(record.GetString("names")), &got)
			if strings.Join(got, ",") == strings.Join(names, ",") {
				found = record
				return true
			}
		}
		return false
	})
	return found
}

// waitCertificate waits until a certificate is in a state, and returns it.
func (a *acmeHarness) waitCertificate(id, what string, condition func(*core.Record) bool) *core.Record {
	a.t.Helper()
	var record *core.Record
	deadline := time.Now().Add(60 * time.Second)
	for {
		found, err := a.app.FindRecordById(schema.Certificates, id)
		if err == nil && condition(found) {
			return found
		}
		if err == nil {
			record = found
		}
		if time.Now().After(deadline) {
			if record != nil {
				a.t.Fatalf("timed out waiting for the certificate %s: %s %q", what, record.GetString("status"), record.GetString("last_error"))
			}
			a.t.Fatalf("timed out waiting for the certificate %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// setCertField changes one field of a certificate in the database, and
// nothing else the panel may be writing at the same time.
func (a *acmeHarness) setCertField(id, field string, value any) {
	a.t.Helper()
	if _, err := a.app.DB().Update(schema.Certificates, dbx.Params{field: value}, dbx.HashExp{"id": id}).Execute(); err != nil {
		a.t.Fatal(err)
	}
}

func issued(record *core.Record) bool {
	return record.GetString("status") == schema.CertActive && record.GetString("chain_pem") != ""
}

func failedOnce(record *core.Record) bool { return record.GetInt("attempts") >= 1 }

func historyOf(record *core.Record) []map[string]any {
	var history []map[string]any
	_ = json.Unmarshal([]byte(record.GetString("history")), &history)
	return history
}

func lastAttempt(t *testing.T, record *core.Record) map[string]any {
	t.Helper()
	var history []map[string]any
	_ = json.Unmarshal([]byte(record.GetString("history")), &history)
	if len(history) == 0 {
		t.Fatal("the certificate has no history")
	}
	return history[len(history)-1]
}

// edgeRoute is the route of a hostname as the server page shows it.
func (a *acmeHarness) edgeRoute(hostname string) map[string]any {
	a.t.Helper()
	status, raw := testutil.Do(a.t, a.handler, testutil.Request{URL: "/api/pc/hosts/" + a.host.Id + "/edge", Token: a.admin})
	if status != http.StatusOK {
		a.t.Fatalf("%d %s", status, raw)
	}
	var state struct {
		Routes []map[string]any `json:"routes"`
	}
	_ = json.Unmarshal([]byte(raw), &state)
	for _, route := range state.Routes {
		if route["hostname"] == hostname {
			return route
		}
	}
	a.t.Fatalf("no route for %s in %s", hostname, raw)
	return nil
}

// served waits until the edge serves a hostname with the current version
// of a certificate, and returns the certificate as it is then.
func (a *acmeHarness) served(hostname string, cert *core.Record) *core.Record {
	a.t.Helper()
	var current *core.Record
	a.waitFor(hostname+" to be served with its certificate", func() bool {
		status, raw := testutil.Do(a.t, a.handler, testutil.Request{URL: "/api/pc/hosts/" + a.host.Id + "/edge", Token: a.admin})
		if status != http.StatusOK || !strings.Contains(raw, cert.Id) {
			return false
		}
		host := a.harness.host(a.host.Id)
		fresh, err := a.app.FindRecordById(schema.Certificates, cert.Id)
		if err != nil {
			return false
		}
		file, err := edge.CertFileID(fresh.Id, []byte(fresh.GetString("chain_pem")))
		if err != nil {
			return false
		}
		var installed map[string]string
		_ = json.Unmarshal([]byte(fresh.GetString("installed")), &installed)
		current = fresh
		return installed[host.Id] == file && host.GetString("edge_digest") == host.GetString("edge_applied_digest")
	})
	return current
}

// TestANewDomainGetsATrustedCertificate: a domain with automatic TLS gets a
// certificate without anyone asking. The edge serves the HTTP-01 answer, the
// CA checks it and the certificate goes to the edge that serves the domain.
func TestANewDomainGetsATrustedCertificate(t *testing.T) {
	a := newACMEHarness(t, acmeOptions{})
	a.mustSetAccount()

	a.domain("shop.example.test")
	cert := a.certificateFor("shop.example.test")
	if !cert.GetBool("auto") || cert.GetString("challenge") != schema.ChallengeAuto {
		t.Errorf("ordered as auto %v, challenge %q", cert.GetBool("auto"), cert.GetString("challenge"))
	}
	cert = a.waitCertificate(cert.Id, "to be issued", issued)

	if err := a.ca.Verify([]byte(cert.GetString("chain_pem")), "shop.example.test"); err != nil {
		t.Errorf("the chain is not trusted: %v", err)
	}
	if left := time.Until(cert.GetDateTime("not_after").Time()); left < 80*24*time.Hour {
		t.Errorf("valid for %v", left)
	}
	if before := cert.GetDateTime("not_after").Time().Sub(cert.GetDateTime("renew_after").Time()); before < 30*24*time.Hour-time.Second || before > 30*24*time.Hour {
		t.Errorf("renews %v before it expires", before)
	}
	if attempt := lastAttempt(t, cert); attempt["ok"] != true || attempt["challenge"] != schema.ChallengeHTTP01 || attempt["account"] != schema.ACMEPrimary {
		t.Errorf("history: %v", attempt)
	}

	a.served("shop.example.test", cert)
	if route := a.edgeRoute("shop.example.test"); route["tls"] != "valid" {
		t.Errorf("the route's TLS: %v", route)
	}
	file := filepath.Join(a.edge.config.StateDir, "edge", "certs")
	if entries, _ := os.ReadDir(file); len(entries) != 2 {
		t.Errorf("certificate files on the edge: %v", entries)
	}

	// the answer was a file on the edge, and is gone
	answers := filepath.Join(a.edge.config.StateDir, "edge", "acme", ".well-known", "acme-challenge")
	a.waitFor("the answer to be removed", func() bool {
		entries, _ := os.ReadDir(answers)
		return len(entries) == 0
	})
	presented, _ := a.app.FindRecordsByFilter(schema.Commands, "type='acme.challenge.present' && host={:h}", "", 0, 0, dbx.Params{"h": a.host.Id})
	if len(presented) != 1 || presented[0].GetString("status") != schema.CommandSucceeded {
		t.Errorf("challenge commands: %d", len(presented))
	}
	if events, _ := a.app.FindRecordsByFilter(schema.Events, "kind='certificate.issued'", "", 0, 0); len(events) != 1 {
		t.Errorf("%d issued events", len(events))
	}

	// one that is fine is not issued again on request: each time would use
	// up the CA's rate limits
	if status, raw := testutil.Do(t, a.handler, testutil.Request{Method: "POST", URL: "/api/pc/certificates/" + cert.Id + "/retry", Token: a.admin}); status != http.StatusConflict {
		t.Errorf("retrying a fine certificate: %d %s", status, raw)
	}
}

// presented counts the HTTP-01 answers the panel asked the edge for.
func (a *acmeHarness) presented() int {
	a.t.Helper()
	count, err := a.app.CountRecords(schema.Commands, dbx.HashExp{"type": "acme.challenge.present", "host": a.host.Id})
	if err != nil {
		a.t.Fatal(err)
	}
	return int(count)
}

// TestAWildcardComesThroughDNS01: an admin asks for a wildcard, whose names
// can only be checked in DNS: the panel puts the answers in the zone at the
// provider, waits until the zone's name servers answer them, and removes
// them after.
func TestAWildcardComesThroughDNS01(t *testing.T) {
	a := newACMEHarness(t, acmeOptions{}, "example.test")
	if status, out := a.connect("cf-token"); status != http.StatusOK {
		t.Fatalf("connect: %d %v", status, out)
	}
	a.mustSetAccount()

	status, raw := testutil.Do(t, a.handler, testutil.Request{Method: "POST", URL: "/api/pc/certificates/acme", Token: a.admin,
		Body: `{"names":["example.test","*.example.test"]}`})
	if status != http.StatusCreated {
		t.Fatalf("%d %s", status, raw)
	}
	cert := a.certificateFor("example.test", "*.example.test")
	cert = a.waitCertificate(cert.Id, "to be issued", issued)
	for _, name := range []string{"example.test", "app.example.test"} {
		if err := a.ca.Verify([]byte(cert.GetString("chain_pem")), name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if attempt := lastAttempt(t, cert); attempt["challenge"] != schema.ChallengeDNS01 {
		t.Errorf("history: %v", attempt)
	}
	a.waitFor("the TXT records to be removed", func() bool {
		for _, record := range a.api.Records("example.test") {
			if record.Type == "TXT" {
				return false
			}
		}
		return true
	})

	// the same names twice are refused; a wildcard cannot be checked over HTTP
	if status, _ := testutil.Do(t, a.handler, testutil.Request{Method: "POST", URL: "/api/pc/certificates/acme", Token: a.admin,
		Body: `{"names":["*.example.test","example.test"]}`}); status != http.StatusConflict {
		t.Errorf("the same names again: %d", status)
	}
	if status, _ := testutil.Do(t, a.handler, testutil.Request{Method: "POST", URL: "/api/pc/certificates/acme", Token: a.admin,
		Body: `{"names":["*.other.test"],"challenge":"http-01"}`}); status != http.StatusBadRequest {
		t.Errorf("a wildcard over HTTP-01: %d", status)
	}

	// a domain the wildcard covers needs no certificate of its own
	a.domain("app.example.test")
	a.panel.RunACME()
	if records, _ := a.app.FindRecordsByFilter(schema.Certificates, "issuer='acme'", "", 0, 0); len(records) != 1 {
		t.Errorf("%d ACME certificates, want the wildcard only", len(records))
	}
	a.served("app.example.test", cert)
}

// TestRenewalReplacesTheCertificateOnTheEdge: a certificate renews once a
// third of its lifetime is left, and the edge serves the new one.
func TestRenewalReplacesTheCertificateOnTheEdge(t *testing.T) {
	a := newACMEHarness(t, acmeOptions{validity: 30 * time.Second})
	a.mustSetAccount()

	a.domain("shop.example.test")
	cert := a.waitCertificate(a.certificateFor("shop.example.test").Id, "to be issued", issued)
	a.served("shop.example.test", cert)
	first := cert.GetString("fingerprint")
	renew := cert.GetDateTime("renew_after").Time()
	if left := cert.GetDateTime("not_after").Time().Sub(renew); left < 9*time.Second || left > 11*time.Second {
		t.Fatalf("renews %v before it expires", left)
	}

	// nothing is due before renew_after: no attempt starts
	a.panel.RunACME()
	time.Sleep(500 * time.Millisecond)
	fresh, _ := a.app.FindRecordById(schema.Certificates, cert.Id)
	if a.presented() != 1 || len(historyOf(fresh)) != 1 || !fresh.GetDateTime("attempt_started").IsZero() || fresh.GetString("fingerprint") != first {
		t.Fatalf("an attempt before it was due: %d answers, history %s", a.presented(), fresh.GetString("history"))
	}

	time.Sleep(time.Until(renew))
	deadline := time.Now().Add(15 * time.Second)
	for {
		a.panel.RunACME()
		fresh, _ := a.app.FindRecordById(schema.Certificates, cert.Id)
		if fresh.GetString("fingerprint") != first && issued(fresh) {
			// no third one while the test looks: a 30-second certificate
			// is due again 20 seconds on
			a.setCertField(cert.Id, "renew_after", types.NowDateTime().Add(time.Hour).String())
			cert = fresh
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not renewed: %q", fresh.GetString("last_error"))
		}
		time.Sleep(200 * time.Millisecond)
	}

	if attempt := lastAttempt(t, cert); attempt["ok"] != true || attempt["renewal"] != true {
		t.Errorf("history: %v", attempt)
	}
	if err := a.ca.Verify([]byte(cert.GetString("chain_pem")), "shop.example.test"); err != nil {
		t.Errorf("the renewed chain: %v", err)
	}
	cert = a.served("shop.example.test", cert)
	// the edge has the new files only
	entries, _ := os.ReadDir(filepath.Join(a.edge.config.StateDir, "edge", "certs"))
	var installed map[string]string
	_ = json.Unmarshal([]byte(cert.GetString("installed")), &installed)
	if len(entries) != 2 || !strings.HasPrefix(entries[0].Name(), installed[a.host.Id]) {
		t.Errorf("files %v, installed %v", entries, installed)
	}
	if events, _ := a.app.FindRecordsByFilter(schema.Events, "kind='certificate.renewed'", "", 0, 0); len(events) == 0 {
		t.Error("no renewal event")
	}
}

// TestFailuresShowWhatTheCASaid: when the CA cannot fetch the answer, the
// certificate says what the CA said and what to check, waits before the next
// attempt, raises an alert, and stops for the day after a few attempts.
func TestFailuresShowWhatTheCASaid(t *testing.T) {
	a := newACMEHarness(t, acmeOptions{unreachable: true})

	// without an account, an order waits and says why
	domain := a.domain("shop.example.test")
	cert := a.certificateFor("shop.example.test")
	cert = a.waitCertificate(cert.Id, "to wait for an account", func(r *core.Record) bool {
		return strings.Contains(r.GetString("last_error"), "ACME account")
	})
	if cert.GetInt("attempts") != 0 || cert.GetString("status") != schema.CertPending {
		t.Errorf("waiting is an attempt: %d %s", cert.GetInt("attempts"), cert.GetString("status"))
	}

	// the order of a domain removed before its certificate came goes too
	other := a.domain("blog.example.test")
	ordered := a.certificateFor("blog.example.test")
	if err := a.app.Delete(other); err != nil {
		t.Fatal(err)
	}
	a.panel.RunACME()
	a.waitFor("the order of a removed domain to go", func() bool {
		_, err := a.app.FindRecordById(schema.Certificates, ordered.Id)
		return err != nil
	})

	a.mustSetAccount()
	cert = a.waitCertificate(cert.Id, "to fail", failedOnce)
	message := cert.GetString("last_error")
	if cert.GetString("status") != schema.CertFailed || !strings.HasPrefix(message, "shop.example.test: ") ||
		!strings.Contains(message, "(connection)") || !strings.Contains(message, "port 80") {
		t.Errorf("%s: %q", cert.GetString("status"), message)
	}
	if wait := time.Until(cert.GetDateTime("next_attempt").Time()); wait < 4*time.Minute || wait > 5*time.Minute {
		t.Errorf("the next attempt in %v", wait)
	}
	if attempt := lastAttempt(t, cert); attempt["ok"] != false || attempt["error"] != message {
		t.Errorf("history: %v", attempt)
	}
	var alerts []*core.Record
	a.waitFor("an alert", func() bool {
		alerts, _ = a.app.FindRecordsByFilter(schema.Alerts, "rule='certificate_failed' && target={:id} && status='open'", "", 0, 0, dbx.Params{"id": cert.Id})
		return len(alerts) > 0
	})
	if len(alerts) != 1 || !strings.Contains(alerts[0].GetString("message"), "(connection)") {
		t.Errorf("alerts: %v", alerts)
	}

	// the page reads it: a viewer sees the CA's answer, never the key
	_, viewer := testutil.NewUser(t, a.app, "viewer@example.com", schema.RoleViewer)
	status, raw := testutil.Do(t, a.handler, testutil.Request{URL: "/api/collections/certificates/records/" + cert.Id, Token: viewer})
	if status != http.StatusOK || !strings.Contains(raw, "(connection)") || strings.Contains(raw, "key_pem") {
		t.Errorf("as a viewer: %d %s", status, raw)
	}

	// Retry starts again now; the backoff grows
	if status, raw := testutil.Do(t, a.handler, testutil.Request{Method: "POST", URL: "/api/pc/certificates/" + cert.Id + "/retry", Token: viewer}); status != http.StatusForbidden {
		t.Errorf("a viewer retried: %d %s", status, raw)
	}
	if status, raw := testutil.Do(t, a.handler, testutil.Request{Method: "POST", URL: "/api/pc/certificates/" + cert.Id + "/retry", Token: a.admin}); status != http.StatusAccepted {
		t.Fatalf("retry: %d %s", status, raw)
	}
	cert = a.waitCertificate(cert.Id, "to fail again", func(r *core.Record) bool { return r.GetInt("attempts") == 2 })
	if wait := time.Until(cert.GetDateTime("next_attempt").Time()); wait < 14*time.Minute || wait > 15*time.Minute {
		t.Errorf("the next attempt in %v", wait)
	}

	// five failures in a day are the limit, for Retry too
	history := []map[string]any{}
	for i := 0; i < 5; i++ {
		history = append(history, map[string]any{"at": time.Now().Add(-time.Duration(i) * time.Hour).UTC(), "ok": false, "error": "x"})
	}
	encoded, _ := json.Marshal(history)
	a.setCertField(cert.Id, "history", string(encoded))
	if status, raw := testutil.Do(t, a.handler, testutil.Request{Method: "POST", URL: "/api/pc/certificates/" + cert.Id + "/retry", Token: a.admin}); status != http.StatusTooManyRequests {
		t.Errorf("past the daily limit: %d %s", status, raw)
	}

	// a domain served without a certificate goes on over plain HTTP
	if route := a.edgeRoute(domain.GetString("hostname")); route["tls"] != "none" {
		t.Errorf("the route: %v", route)
	}

	// a certificate removed has nothing left to alert about
	if status, raw := testutil.Do(t, a.handler, testutil.Request{Method: "DELETE", URL: "/api/pc/certificates/" + cert.Id, Token: a.admin}); status != http.StatusNoContent {
		t.Fatalf("remove: %d %s", status, raw)
	}
	if open, _ := a.app.CountRecords(schema.Alerts, dbx.HashExp{"target": cert.Id, "status": schema.AlertOpen}); open != 0 {
		t.Errorf("%d alerts left open", open)
	}
}

// TestAFallbackCAIssuesWhenThePrimaryOneFails: a second account at another
// CA gets the certificate when the first one fails, and the error says which
// CA said what.
func TestAFallbackCAIssuesWhenThePrimaryOneFails(t *testing.T) {
	a := newACMEHarness(t, acmeOptions{})
	// test servers share one TLS certificate, so the client trusts both CAs
	broken := startCA(t, a.dnsHarness, acmeOptions{unreachable: true})

	if status, raw := a.setAccount(schema.ACMEPrimary, broken.DirectoryURL); status != http.StatusOK {
		t.Fatalf("primary: %d %s", status, raw)
	}
	if status, raw := a.setAccount(schema.ACMEFallback, a.ca.DirectoryURL); status != http.StatusOK {
		t.Fatalf("fallback: %d %s", status, raw)
	}

	a.domain("shop.example.test")
	cert := a.waitCertificate(a.certificateFor("shop.example.test").Id, "to be issued", issued)
	if attempt := lastAttempt(t, cert); attempt["account"] != schema.ACMEFallback {
		t.Errorf("history: %v", attempt)
	}
	if err := a.ca.Verify([]byte(cert.GetString("chain_pem")), "shop.example.test"); err != nil {
		t.Error(err)
	}

	// both failing, at two CAs: the error names each
	other := startCA(t, a.dnsHarness, acmeOptions{unreachable: true})
	if status, raw := a.setAccount(schema.ACMEFallback, other.DirectoryURL); status != http.StatusOK {
		t.Fatalf("fallback: %d %s", status, raw)
	}
	a.setCertField(cert.Id, "renew_after", types.NowDateTime().String())
	a.panel.RunACME()
	cert = a.waitCertificate(cert.Id, "to fail", failedOnce)
	message := cert.GetString("last_error")
	for _, ca := range []*acmetest.CA{broken, other} {
		parsed, _ := url.Parse(ca.DirectoryURL)
		if !strings.Contains("\n"+message, "\n"+parsed.Host+": shop.example.test: ") {
			t.Errorf("no line of %s in %q", parsed.Host, message)
		}
	}
	if cert.GetString("status") != schema.CertActive {
		t.Errorf("a failed renewal changed the state to %s", cert.GetString("status"))
	}
}

// TestAnAccountTheCARefusesIsNotKept: the account's checks, and the CA's
// answer when it says no.
func TestAnAccountTheCARefusesIsNotKept(t *testing.T) {
	a := newACMEHarness(t, acmeOptions{})
	for _, body := range []string{
		`{"directory_url":"http://acme.example.com/dir","email":"admin@example.com"}`,
		`{"directory_url":"https://acme.example.com/dir","email":"not an email"}`,
		`{"directory_url":"https://acme.zerossl.com/v2/DV90","email":"admin@example.com"}`,
		`{"directory_url":"https://acme.example.com/dir","email":"admin@example.com","eab_kid":"kid"}`,
	} {
		if status, raw := testutil.Do(t, a.handler, testutil.Request{Method: "PUT", URL: "/api/pc/acme-accounts/primary", Token: a.admin, Body: body}); status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, status, raw)
		}
	}
	if status, _ := testutil.Do(t, a.handler, testutil.Request{Method: "PUT", URL: "/api/pc/acme-accounts/third", Token: a.admin, Body: `{}`}); status != http.StatusNotFound {
		t.Errorf("a third account: %d", status)
	}

	// a URL that is no ACME directory
	status, raw := a.setAccount(schema.ACMEPrimary, strings.TrimSuffix(a.ca.DirectoryURL, "/dir")+"/nothing")
	if status != http.StatusBadRequest || !strings.Contains(raw, "The CA did not take the account") {
		t.Errorf("%d %s", status, raw)
	}
	if count, _ := a.app.CountRecords(schema.ACMEAccounts); count != 0 {
		t.Errorf("%d accounts kept", count)
	}

	// a good one is kept with its key sealed; the same again is no new account
	a.mustSetAccount()
	record, err := a.app.FindFirstRecordByData(schema.ACMEAccounts, "role", schema.ACMEPrimary)
	if err != nil || !secrets.IsSealed(record.GetString("key")) || record.GetString("uri") == "" {
		t.Fatalf("stored %v %v", record, err)
	}
	a.mustSetAccount()
	again, _ := a.app.FindFirstRecordByData(schema.ACMEAccounts, "role", schema.ACMEPrimary)
	if again.GetString("uri") != record.GetString("uri") {
		t.Error("the same account was registered again")
	}
	// operators cannot change it
	_, operator := testutil.NewUser(t, a.app, "op@example.com", schema.RoleOperator)
	if status, _ := testutil.Do(t, a.handler, testutil.Request{Method: "PUT", URL: "/api/pc/acme-accounts/primary", Token: operator, Body: `{}`}); status != http.StatusForbidden {
		t.Errorf("an operator: %d", status)
	}
}

// TestAnUploadedCertificateIsUsedAndWatched: a domain an uploaded
// certificate covers gets none from ACME, and one that expires within two
// weeks raises an alert.
func TestAnUploadedCertificateIsUsedAndWatched(t *testing.T) {
	a := newACMEHarness(t, acmeOptions{})
	a.mustSetAccount()

	chain, key := selfSigned(t, "app.example.com")
	uploaded := a.storeCertificate(chain, key)
	uploaded.Set("not_after", time.Now().Add(23*time.Hour).UTC())
	if err := a.app.Save(uploaded); err != nil {
		t.Fatal(err)
	}
	a.domain("app.example.com")
	a.panel.RunACME()
	if count, _ := a.app.CountRecords(schema.Certificates, dbx.HashExp{"issuer": schema.IssuerACME}); count != 0 {
		t.Errorf("%d ACME certificates for a covered domain", count)
	}

	expiring := func() []*core.Record {
		alerts, _ := a.app.FindRecordsByFilter(schema.Alerts, "rule='certificate_expiring' && target={:id} && status='open'", "", 0, 0, dbx.Params{"id": uploaded.Id})
		return alerts
	}
	a.setCertField(uploaded.Id, "not_after", types.NowDateTime().Add(10*24*time.Hour).String())
	a.panel.MarkExpiredCertificates()
	alerts := expiring()
	if len(alerts) != 1 || alerts[0].GetString("severity") != schema.SeverityWarning {
		t.Fatalf("10 days left: %v", alerts)
	}
	// it grows critical within a week, as the same alert
	a.setCertField(uploaded.Id, "not_after", types.NowDateTime().Add(3*24*time.Hour).String())
	a.panel.MarkExpiredCertificates()
	if again := expiring(); len(again) != 1 || again[0].Id != alerts[0].Id || again[0].GetString("severity") != schema.SeverityCritical {
		t.Errorf("3 days left: %v", again)
	}

	// once a newer one covers the name, the edge serves that, and the old
	// one is no worry
	newer := a.storeCertificate(selfSigned(t, "app.example.com"))
	a.setCertField(newer.Id, "not_after", types.NowDateTime().Add(60*24*time.Hour).String())
	a.panel.MarkExpiredCertificates()
	if left := expiring(); len(left) != 0 {
		t.Errorf("an alert for a certificate the edge no longer serves: %v", left)
	}
}
