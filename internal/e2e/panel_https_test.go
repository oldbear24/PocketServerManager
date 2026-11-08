package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/agent"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// The panel's own HTTPS (#63): a certificate for its public name from its ACME
// accounts, which it serves its UI and API with and never sends to an edge;
// the gRPC gateway keeps the internal CA (docs/adr/0007).

// panelCertificate waits for the panel's own certificate record.
func (a *acmeHarness) panelCertificate() *core.Record {
	a.t.Helper()
	var found *core.Record
	a.waitFor("the panel's own certificate", func() bool {
		a.panel.RunACME()
		records, _ := a.app.FindRecordsByFilter(schema.Certificates, "panel=true", "", 0, 0)
		if len(records) == 1 {
			found = records[0]
		}
		return found != nil
	})
	return found
}

// panelLeaf is the certificate a browser asking for a name gets from the
// panel's HTTPS.
func (a *acmeHarness) panelLeaf(serverName string) *x509.Certificate {
	a.t.Helper()
	return a.panelChain(serverName)[0]
}

// panelChain is the chain the panel's HTTPS sends for a name.
func (a *acmeHarness) panelChain(serverName string) []*x509.Certificate {
	a.t.Helper()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go func() { _ = tls.Server(server, a.panel.HTTPSConfig()).Handshake() }()
	//nolint:gosec // the test looks at what the panel offers, and checks it itself
	conn := tls.Client(client, &tls.Config{ServerName: serverName, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if err := conn.Handshake(); err != nil {
		a.t.Fatalf("the panel's HTTPS for %q: %v", serverName, err)
	}
	return conn.ConnectionState().PeerCertificates
}

// internalRoots is the internal CA's root, as /api/pc/ca.pem hands it out.
func (a *acmeHarness) internalRoots() *x509.CertPool {
	a.t.Helper()
	status, raw := testutil.Do(a.t, a.handler, testutil.Request{URL: "/api/pc/ca.pem"})
	if status != http.StatusOK {
		a.t.Fatalf("ca.pem: %d %s", status, raw)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(raw)) {
		a.t.Fatalf("ca.pem: %s", raw)
	}
	return roots
}

// isInternal says whether a certificate is the internal CA's for the panel.
func (a *acmeHarness) isInternal(leaf *x509.Certificate) bool {
	a.t.Helper()
	_, err := leaf.Verify(x509.VerifyOptions{Roots: a.internalRoots(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err == nil && len(leaf.URIs) == 1 && leaf.URIs[0].String() == "pocketcloud://panel"
}

// firstOfChain is the leaf of a stored chain.
func firstOfChain(t *testing.T, chain string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(chain))
	if block == nil {
		t.Fatalf("no certificate in %q", chain)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

// TestThePanelGetsItsOwnCertificate: a panel that serves HTTPS itself orders
// a certificate for its name, answers the CA's HTTP-01 on its plain port and
// serves the certificate from the next handshake on; until then, and for any
// other name, the internal CA's. No edge ever gets it.
func TestThePanelGetsItsOwnCertificate(t *testing.T) {
	a := newACMEHarness(t, acmeOptions{panelHTTPS: true})

	// without an account it waits and says why, and the panel serves HTTPS
	// with the internal CA's certificate
	cert := a.panelCertificate()
	if !cert.GetBool("auto") || cert.GetString("challenge") != schema.ChallengeAuto || certificateNamesOf(cert) != panelHost {
		t.Errorf("ordered: auto %v, challenge %q, names %s", cert.GetBool("auto"), cert.GetString("challenge"), cert.GetString("names"))
	}
	cert = a.waitCertificate(cert.Id, "to wait for an account", func(r *core.Record) bool {
		return strings.Contains(r.GetString("last_error"), "ACME account")
	})
	if !a.isInternal(a.panelLeaf(panelHost)) {
		t.Error("before the first certificate: not the internal CA's")
	}

	// an edge domain next to it gets its own
	a.domain("www.example.test")
	a.mustSetAccount()
	cert = a.waitCertificate(cert.Id, "to be issued", issued)
	if err := a.ca.Verify([]byte(cert.GetString("chain_pem")), panelHost); err != nil {
		t.Errorf("the chain is not trusted: %v", err)
	}
	if attempt := lastAttempt(t, cert); attempt["ok"] != true || attempt["challenge"] != schema.ChallengeHTTP01 || attempt["account"] != schema.ACMEPrimary {
		t.Errorf("history: %v", attempt)
	}
	chain := a.panelChain(panelHost)
	if !chain[0].Equal(firstOfChain(t, cert.GetString("chain_pem"))) {
		t.Errorf("the panel serves %v, not its new certificate", chain[0].Issuer)
	}
	// a browser that trusts the CA's root trusts what the panel sends
	intermediates := x509.NewCertPool()
	for _, sent := range chain[1:] {
		intermediates.AddCert(sent)
	}
	if _, err := chain[0].Verify(x509.VerifyOptions{DNSName: panelHost, Roots: a.ca.Roots(), Intermediates: intermediates}); err != nil {
		t.Errorf("a browser trusting the CA: %v", err)
	}
	// a handshake that names no other name gets the internal certificate
	for _, other := range []string{"", "www.example.test", "127.0.0.1"} {
		if !a.isInternal(a.panelLeaf(other)) {
			t.Errorf("%q: not the internal certificate", other)
		}
	}
	if events, _ := a.app.FindRecordsByFilter(schema.Events, "kind='certificate.issued'", "", 0, 0); len(events) < 1 {
		t.Error("no issued event")
	}
	if status, raw := testutil.Do(t, a.handler, testutil.Request{Method: "POST", URL: "/api/pc/certificates/" + cert.Id + "/retry", Token: a.admin}); status != http.StatusConflict {
		t.Errorf("retrying a fine certificate: %d %s", status, raw)
	}

	// the panel says it serves HTTPS, with which record
	status, raw := testutil.Do(t, a.handler, testutil.Request{URL: "/api/pc/info", Token: a.admin})
	var info struct {
		HTTPS            bool   `json:"https"`
		PanelCertificate string `json:"panel_certificate"`
	}
	_ = json.Unmarshal([]byte(raw), &info)
	if status != http.StatusOK || !info.HTTPS || info.PanelCertificate != cert.Id {
		t.Errorf("info: %d %s", status, raw)
	}

	// the edge serves the other domain with its own, and never the panel's
	www := a.waitCertificate(a.certificateFor("www.example.test").Id, "to be issued", issued)
	a.served("www.example.test", www)
	cert, _ = a.app.FindRecordById(schema.Certificates, cert.Id)
	var installed map[string]string
	_ = json.Unmarshal([]byte(cert.GetString("installed")), &installed)
	if len(installed) != 0 {
		t.Errorf("the panel's certificate is installed on %v", installed)
	}
	entries, _ := os.ReadDir(filepath.Join(a.edge.config.StateDir, "edge", "certs"))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), cert.Id) {
			t.Errorf("the edge has the panel's certificate: %s", entry.Name())
		}
	}
	commands, _ := a.app.FindRecordsByFilter(schema.Commands, "payload ~ {:id}", "", 0, 0, dbx.Params{"id": cert.Id})
	for _, command := range commands {
		t.Errorf("a command carries the panel's certificate: %s", command.GetString("type"))
	}
	if route := a.edgeRoute("www.example.test"); route["tls"] != "valid" {
		t.Errorf("the other domain's route: %v", route)
	}
	// and an admin can still ask for one of the panel's name for an edge
	if status, raw := testutil.Do(t, a.handler, testutil.Request{Method: "POST", URL: "/api/pc/certificates/acme", Token: a.admin,
		Body: `{"names":["` + panelHost + `"]}`}); status != http.StatusCreated {
		t.Errorf("the panel's name for an edge: %d %s", status, raw)
	}
}

func certificateNamesOf(record *core.Record) string {
	var names []string
	_ = json.Unmarshal([]byte(record.GetString("names")), &names)
	return strings.Join(names, ",")
}

// TestThePanelRenewsWithoutARestart: the panel's certificate renews as an
// edge's does, and the next handshake presents the new one.
func TestThePanelRenewsWithoutARestart(t *testing.T) {
	a := newACMEHarness(t, acmeOptions{panelHTTPS: true, validity: 30 * time.Second})
	a.mustSetAccount()
	cert := a.waitCertificate(a.panelCertificate().Id, "to be issued", issued)
	first := cert.GetString("fingerprint")
	if !a.panelLeaf(panelHost).Equal(firstOfChain(t, cert.GetString("chain_pem"))) {
		t.Fatal("the first certificate is not served")
	}

	time.Sleep(time.Until(cert.GetDateTime("renew_after").Time()))
	deadline := time.Now().Add(15 * time.Second)
	for {
		a.panel.RunACME()
		fresh, _ := a.app.FindRecordById(schema.Certificates, cert.Id)
		if fresh.GetString("fingerprint") != first && issued(fresh) {
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
	if !a.panelLeaf(panelHost).Equal(firstOfChain(t, cert.GetString("chain_pem"))) {
		t.Error("the renewed certificate is not served")
	}
	if events, _ := a.app.FindRecordsByFilter(schema.Events, "kind='certificate.renewed'", "", 0, 0); len(events) == 0 {
		t.Error("no renewal event")
	}
}

// TestThePanelCertificateFailureShowsWhatTheCASaid: when the CA cannot reach
// the panel's port 80, the certificate says what the CA said and what to
// check, waits, raises an alert, and the panel goes on with the internal CA's.
func TestThePanelCertificateFailureShowsWhatTheCASaid(t *testing.T) {
	a := newACMEHarness(t, acmeOptions{panelHTTPS: true, unreachable: true})
	a.mustSetAccount()
	cert := a.waitCertificate(a.panelCertificate().Id, "to fail", failedOnce)
	message := cert.GetString("last_error")
	if cert.GetString("status") != schema.CertFailed || !strings.HasPrefix(message, panelHost+": ") ||
		!strings.Contains(message, "(connection)") || !strings.Contains(message, "port 80 reaches the panel's --http address") {
		t.Errorf("%s: %q", cert.GetString("status"), message)
	}
	if wait := time.Until(cert.GetDateTime("next_attempt").Time()); wait < 4*time.Minute || wait > 5*time.Minute {
		t.Errorf("the next attempt in %v", wait)
	}
	a.waitFor("an alert", func() bool {
		count, _ := a.app.CountRecords(schema.Alerts, dbx.HashExp{"rule": "certificate_failed", "target": cert.Id, "status": schema.AlertOpen})
		return count == 1
	})
	if !a.isInternal(a.panelLeaf(panelHost)) {
		t.Error("the panel stopped serving the internal certificate")
	}

	_, viewer := testutil.NewUser(t, a.app, "viewer@example.com", schema.RoleViewer)
	status, raw := testutil.Do(t, a.handler, testutil.Request{URL: "/api/collections/certificates/records/" + cert.Id, Token: viewer})
	if status != http.StatusOK || !strings.Contains(raw, "(connection)") || !strings.Contains(raw, `"panel":true`) || strings.Contains(raw, "key_pem") {
		t.Errorf("as a viewer: %d %s", status, raw)
	}
	if status, raw := testutil.Do(t, a.handler, testutil.Request{Method: "POST", URL: "/api/pc/certificates/" + cert.Id + "/retry", Token: viewer}); status != http.StatusForbidden {
		t.Errorf("a viewer retried: %d %s", status, raw)
	}
	if status, raw := testutil.Do(t, a.handler, testutil.Request{Method: "POST", URL: "/api/pc/certificates/" + cert.Id + "/retry", Token: a.admin}); status != http.StatusAccepted {
		t.Fatalf("retry: %d %s", status, raw)
	}
	cert = a.waitCertificate(cert.Id, "to fail again", func(r *core.Record) bool { return r.GetInt("attempts") == 2 })
	history := []map[string]any{}
	for i := 0; i < 5; i++ {
		history = append(history, map[string]any{"at": time.Now().Add(-time.Duration(i) * time.Hour).UTC(), "ok": false, "error": "x"})
	}
	encoded, _ := json.Marshal(history)
	a.setCertField(cert.Id, "history", string(encoded))
	if status, raw := testutil.Do(t, a.handler, testutil.Request{Method: "POST", URL: "/api/pc/certificates/" + cert.Id + "/retry", Token: a.admin}); status != http.StatusTooManyRequests {
		t.Errorf("past the daily limit: %d %s", status, raw)
	}

	// removed, it is ordered again, and its alert goes
	if status, raw := testutil.Do(t, a.handler, testutil.Request{Method: "DELETE", URL: "/api/pc/certificates/" + cert.Id, Token: a.admin}); status != http.StatusNoContent {
		t.Fatalf("remove: %d %s", status, raw)
	}
	if open, _ := a.app.CountRecords(schema.Alerts, dbx.HashExp{"target": cert.Id, "status": schema.AlertOpen}); open != 0 {
		t.Errorf("%d alerts left open", open)
	}
	if again := a.panelCertificate(); again.Id == cert.Id {
		t.Error("not ordered again")
	}
}

// TestThePanelCertificateThroughDNS01: a panel whose port 80 the CA cannot
// reach gets its certificate through the DNS provider of its zone.
func TestThePanelCertificateThroughDNS01(t *testing.T) {
	a := newACMEHarness(t, acmeOptions{panelHTTPS: true, panelChallenge: schema.ChallengeDNS01, unreachable: true}, "example.test")
	if status, out := a.connect("cf-token"); status != http.StatusOK {
		t.Fatalf("connect: %d %v", status, out)
	}
	a.mustSetAccount()
	cert := a.panelCertificate()
	if cert.GetString("challenge") != schema.ChallengeDNS01 {
		t.Errorf("challenge %q", cert.GetString("challenge"))
	}
	cert = a.waitCertificate(cert.Id, "to be issued", issued)
	if attempt := lastAttempt(t, cert); attempt["challenge"] != schema.ChallengeDNS01 {
		t.Errorf("history: %v", attempt)
	}
	if !a.panelLeaf(panelHost).Equal(firstOfChain(t, cert.GetString("chain_pem"))) {
		t.Error("the certificate is not served")
	}
	a.waitFor("the TXT records to be removed", func() bool {
		for _, record := range a.api.Records("example.test") {
			if record.Type == "TXT" {
				return false
			}
		}
		return true
	})
}

// TestThePanelCertificateExpiryAlert: the panel's certificate that expires
// soon raises the alert an edge's does, as it is in use.
func TestThePanelCertificateExpiryAlert(t *testing.T) {
	a := newACMEHarness(t, acmeOptions{panelHTTPS: true})
	a.mustSetAccount()
	cert := a.waitCertificate(a.panelCertificate().Id, "to be issued", issued)
	a.setCertField(cert.Id, "not_after", types.NowDateTime().Add(5*24*time.Hour).String())
	a.panel.MarkExpiredCertificates()
	alerts, _ := a.app.FindRecordsByFilter(schema.Alerts, "rule='certificate_expiring' && target={:id} && status='open'", "", 0, 0, dbx.Params{"id": cert.Id})
	if len(alerts) != 1 || alerts[0].GetString("severity") != "critical" {
		t.Fatalf("alerts: %v", alerts)
	}
}

// TestAgentsKeepTheInternalCA: with the panel's certificate from ACME, the
// gateway still serves the internal CA's, agents connected before stay, new
// ones enroll with the pin, and a wrong pin is refused (ADR 0007).
func TestAgentsKeepTheInternalCA(t *testing.T) {
	a := newACMEHarness(t, acmeOptions{panelHTTPS: true})
	a.mustSetAccount()
	a.waitCertificate(a.panelCertificate().Id, "to be issued", issued)

	if !a.harness.host(a.host.Id).GetBool("stream_open") {
		t.Error("the edge agent connected before is not connected")
	}
	_, host := a.enrolled("after-acme")
	if host.GetString("status") == "" {
		t.Errorf("the new agent: %v", host)
	}

	config := a.agentConfig("pinned", a.token("pinned"))
	config.CAPin = "sha256:" + strings.Repeat("00", 32)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := agent.Enroll(ctx, config, nil); err == nil || !strings.Contains(err.Error(), "CA_PIN") {
		t.Errorf("a wrong pin: %v", err)
	}

	// the gateway's certificate is the internal CA's, not the CA's of ACME
	dialer := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}}} //nolint:gosec // looked at below
	conn, err := dialer.DialContext(ctx, "tcp", a.panel.Gateway.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	leaf := conn.(*tls.Conn).ConnectionState().PeerCertificates[0]
	if !a.isInternal(leaf) {
		t.Errorf("the gateway serves %v", leaf.Issuer)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: a.ca.Roots()}); err == nil {
		t.Error("the gateway's certificate is the ACME CA's")
	}
}
