package panel

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"

	"github.com/oldbear24/PocketServerManager/internal/ca"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// The panel's own HTTPS (#63).

func TestTheHTTPSOfThePanelNeedsANameToBeCertifiedFor(t *testing.T) {
	for _, test := range []struct {
		addr, url, challenge, name, err string
	}{
		{addr: "", url: "http://10.0.0.5:8090"},
		{addr: ":8443", url: "https://Panel.Example.com:8443/x", name: "panel.example.com"},
		{addr: ":8443", url: "https://panel.example.com", challenge: "dns-01", name: "panel.example.com"},
		{addr: ":8443", url: "http://panel.example.com", err: "https://"},
		{addr: ":8443", url: "", err: "https://"},
		{addr: ":8443", url: "https://203.0.113.7", err: "a name, not"},
		{addr: ":8443", url: "https://[2001:db8::1]", err: "a name, not"},
		{addr: ":8443", url: "https://*.example.com", err: "a name, not"},
		{addr: ":8443", url: "https://panel", err: "a name, not"},
		{addr: ":8443", url: "https://panel.example.com", challenge: "tls-alpn-01", err: "auto, http-01 or dns-01"},
	} {
		config := Config{HTTPSAddr: test.addr, PublicURL: test.url, HTTPSChallenge: test.challenge}
		name, err := config.CheckHTTPS()
		if test.err != "" {
			if err == nil || !strings.Contains(err.Error(), test.err) {
				t.Errorf("%s %s %s: %q %v", test.addr, test.url, test.challenge, name, err)
			}
			continue
		}
		if err != nil || name != test.name {
			t.Errorf("%s %s: %q %v", test.addr, test.url, name, err)
		}
	}
	if got := (&Config{}).PanelChallenge(); got != schema.ChallengeAuto {
		t.Errorf("no challenge given: %q", got)
	}
}

func TestPocketBasesOwnHTTPSIsRefusedBesidesThePanels(t *testing.T) {
	serve := func() *cobra.Command {
		cmd := &cobra.Command{Use: "serve"}
		cmd.Flags().String("https", "", "")
		return cmd
	}
	config := Config{HTTPSAddr: ":8443"}
	withHTTPS := serve()
	_ = withHTTPS.Flags().Set("https", ":443")
	if err := config.CheckServe(withHTTPS, nil); err == nil || !strings.Contains(err.Error(), "--https-addr alone") {
		t.Errorf("--https: %v", err)
	}
	if err := config.CheckServe(serve(), []string{"panel.example.com"}); err == nil {
		t.Error("domain arguments were taken")
	}
	if err := config.CheckServe(serve(), nil); err != nil {
		t.Errorf("--https-addr alone: %v", err)
	}
	// without --https-addr PocketBase's own stays as it was
	if err := (&Config{}).CheckServe(withHTTPS, []string{"panel.example.com"}); err != nil {
		t.Errorf("PocketBase's own HTTPS: %v", err)
	}
	if err := config.CheckServe(&cobra.Command{Use: "migrate"}, []string{"up"}); err != nil {
		t.Errorf("another command: %v", err)
	}
}

func TestThePlainPortAnswersTheCAAndSendsTheRestToHTTPS(t *testing.T) {
	p, _, _ := newTestPanel(t)
	p.Config.HTTPSAddr = ":8443"
	p.Config.PublicURL = "https://panel.example.com:8443/ui"
	handler := p.HTTPHandler()
	answers := &panelAnswers{p: p}
	if err := answers.Present("panel.example.com", "tok-1", "tok-1.thumb"); err != nil {
		t.Fatal(err)
	}

	ask := func(method, host, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://"+host+path, nil)
		r.Host = host
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := ask(http.MethodGet, "Panel.Example.com:80", "/.well-known/acme-challenge/tok-1"); w.Code != http.StatusOK ||
		w.Body.String() != "tok-1.thumb" || w.Header().Get("Content-Type") != "text/plain" {
		t.Errorf("the answer: %d %q %q", w.Code, w.Body, w.Header().Get("Content-Type"))
	}
	for _, wrong := range []struct{ host, path string }{
		{"other.example.com", "/.well-known/acme-challenge/tok-1"},
		{"panel.example.com", "/.well-known/acme-challenge/tok-2"},
	} {
		if w := ask(http.MethodGet, wrong.host, wrong.path); w.Code != http.StatusNotFound {
			t.Errorf("%s%s: %d", wrong.host, wrong.path, w.Code)
		}
	}
	_ = answers.CleanUp("panel.example.com", "tok-1", "tok-1.thumb")
	if w := ask(http.MethodGet, "panel.example.com", "/.well-known/acme-challenge/tok-1"); w.Code != http.StatusNotFound {
		t.Errorf("after the clean-up: %d", w.Code)
	}

	// everything else goes to the public URL's origin, not to the host asked
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		w := ask(method, "evil.test", "/x?y=1")
		if w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != "https://panel.example.com:8443/x?y=1" {
			t.Errorf("%s: %d %q", method, w.Code, w.Header().Get("Location"))
		}
	}

	// a request target that is no path cannot lead elsewhere
	server := httptest.NewServer(handler)
	defer server.Close()
	for target, want := range map[string]string{
		"http:@evil.com/steal": "https://panel.example.com:8443/",
		"http:evil.com":        "https://panel.example.com:8443/",
		"*":                    "https://panel.example.com:8443/",
		"http://evil.com/abs":  "https://panel.example.com:8443/abs",
		"//evil.com/":          "https://panel.example.com:8443//evil.com/",
	} {
		conn, err := net.Dial("tcp", server.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(conn, "GET "+target+" HTTP/1.1\r\nHost: panel.example.com\r\nConnection: close\r\n\r\n")
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		_ = conn.Close()
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		_ = response.Body.Close()
		if location := response.Header.Get("Location"); location != want {
			t.Errorf("GET %s: %d %q, want %q", target, response.StatusCode, location, want)
		}
	}
}

// openAuthority gives a test panel its master key and internal CA, as its
// start does.
func openAuthority(t *testing.T, p *Panel, app core.App) *ca.Authority {
	t.Helper()
	box, err := secrets.New(testutil.MasterKey)
	if err != nil {
		t.Fatal(err)
	}
	p.Box = box
	authority, err := ca.Open(app, box, []string{"panel.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	p.Authority = authority
	p.authority.Store(authority)
	return authority
}

// panelLeaf is a certificate for the panel's name as an ACME CA issues it,
// with the fields the ACME manager stores.
func panelLeaf(t *testing.T, p *Panel, name, fingerprint string) (map[string]any, *issued) {
	t.Helper()
	root := issue(t, "Test ACME root", nil, true, time.Now().Add(90*24*time.Hour))
	leaf := issue(t, name, root, false, time.Now().Add(60*24*time.Hour))
	leaf.cert.ExtKeyUsage = nil
	sealed, err := p.Box.SealString(keyPEM(t, leaf.key))
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"chain_pem": leaf.pem + root.pem, "key_pem": sealed, "fingerprint": fingerprint,
		"not_before": leaf.cert.NotBefore, "not_after": leaf.cert.NotAfter,
	}, leaf
}

// handshake is the certificate a client asking for a name gets.
func handshake(t *testing.T, p *Panel, serverName string) *x509.Certificate {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go func() { _ = tls.Server(server, p.HTTPSConfig()).Handshake() }()
	//nolint:gosec // the test looks at the certificate the panel offers, whoever issued it
	conn := tls.Client(client, &tls.Config{ServerName: serverName, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if err := conn.Handshake(); err != nil {
		t.Fatalf("%q: %v", serverName, err)
	}
	return conn.ConnectionState().PeerCertificates[0]
}

func isInternal(cert *x509.Certificate) bool {
	return len(cert.URIs) == 1 && cert.URIs[0].String() == "pocketcloud://panel"
}

func TestThePanelServesItsCertificateForItsNameAndTheInternalOneOtherwise(t *testing.T) {
	p, app, _ := newTestPanel(t)
	p.Config.HTTPSAddr = ":8443"
	p.Config.PublicURL = "https://panel.example.com"
	openAuthority(t, p, app)

	// before its first certificate the panel serves the internal CA's
	if !isInternal(handshake(t, p, "panel.example.com")) {
		t.Error("before the first certificate: not the internal one")
	}

	// ordered, then issued: served from the next handshake on
	record := saveRecord(t, app, schema.Certificates, map[string]any{
		"names": []string{"panel.example.com"}, "issuer": schema.IssuerACME, "challenge": schema.ChallengeAuto,
		"status": schema.CertPending, "auto": true, "panel": true,
	})
	fields, leaf := panelLeaf(t, p, "panel.example.com", "f-1")
	p.setFields(record, fields)
	if !p.servesPanelCertificate(record.Id) {
		t.Fatal("the issued record is not served")
	}
	if got := handshake(t, p, "Panel.Example.com"); !got.Equal(leaf.cert) {
		t.Errorf("by its name: %v", got.Subject)
	}
	// another name, an address or none: the internal one
	for _, other := range []string{"", "other.example.com"} {
		if !isInternal(handshake(t, p, other)) {
			t.Errorf("%q: not the internal certificate", other)
		}
	}

	// a renewal is served without a restart
	fields, renewed := panelLeaf(t, p, "panel.example.com", "f-2")
	p.setFields(record, fields)
	if got := handshake(t, p, "panel.example.com"); !got.Equal(renewed.cert) {
		t.Error("the renewed certificate is not served")
	}
	// a key that does not open keeps the one served
	p.setFields(record, map[string]any{"fingerprint": "f-3", "key_pem": "broken"})
	if got := handshake(t, p, "panel.example.com"); !got.Equal(renewed.cert) {
		t.Error("a broken renewal replaced the served certificate")
	}

	// a removed one is not served any more
	if err := app.Delete(record); err != nil {
		t.Fatal(err)
	}
	if p.servesPanelCertificate(record.Id) || !isInternal(handshake(t, p, "panel.example.com")) {
		t.Error("a removed certificate is still served")
	}
}

func TestThePanelsCertificateIsNoEdgesAndFollowsItsName(t *testing.T) {
	p, app, _ := newTestPanel(t)
	host := saveRecord(t, app, schema.Hosts, map[string]any{
		"name": "vps", "status": "online", "roles": []string{"docker", "edge"}, "public_ip4": "203.0.114.7",
	})
	domain := saveRecord(t, app, schema.Domains, map[string]any{
		"hostname": "panel.example.com", "host": host.Id, "target_kind": schema.TargetContainer, "container": "x", "port": 80, "tls": schema.TLSAuto,
	})
	own := saveRecord(t, app, schema.Certificates, map[string]any{
		"names": []string{"panel.example.com"}, "issuer": schema.IssuerACME, "status": schema.CertActive, "auto": true, "panel": true, "fingerprint": "f-1",
	})
	if !neededBy(own, []*core.Record{domain}, "panel.example.com") {
		t.Error("the panel's own is not needed by the panel")
	}
	for _, name := range []string{"", "other.example.com"} {
		if neededBy(own, []*core.Record{domain}, name) {
			t.Errorf("needed with the panel's name %q, though a domain has the name", name)
		}
	}
	if suits(own, domain) {
		t.Error("the panel's own suits a domain")
	}
	certs := []certificateRecord{{record: own, names: []string{"panel.example.com"}, notAfter: time.Now().Add(time.Hour)}}
	if found := coveringCertificate(certs, domain); found != nil {
		t.Error("an edge would serve the panel's key")
	}

	// a request for the same names is not taken for a duplicate of the panel's
	p.Config.HTTPSAddr = ":8443"
	p.Config.PublicURL = "https://panel.example.com"
	if p.panelRecord() == nil || p.panelRecord().Id != own.Id {
		t.Error("the panel's record is not found by its name")
	}
	p.Config.PublicURL = "https://admin.example.com"
	if p.panelRecord() != nil {
		t.Error("the record of a name the panel no longer has is its own")
	}
}

func TestTheCAChecksThePanelsNameOverItsOwnPlainPort(t *testing.T) {
	p, app, _ := newTestPanel(t)
	p.Config.HTTPSAddr = ":8443"
	p.Config.PublicURL = "https://panel.example.com"

	plan, err := p.planChallenge([]string{"panel.example.com"}, schema.ChallengeAuto, true)
	if err != nil || plan.kind != schema.ChallengeHTTP01 || !plan.panel || len(plan.hosts) != 0 {
		t.Fatalf("auto: %+v %v", plan, err)
	}
	if hint := plan.hint("panel.example.com: Timeout during connect (connection)"); !strings.Contains(hint, "reaches the panel's --http address") {
		t.Errorf("hint: %q", hint)
	}

	// PocketBase's default --http, 127.0.0.1:8090, is out of the CA's reach
	loopback := "127.0.0.1:8090"
	p.https.httpAddr.Store(&loopback)
	if _, err := p.planChallenge([]string{"panel.example.com"}, schema.ChallengeAuto, true); err == nil ||
		!strings.Contains(err.Error(), "listens for HTTP on 127.0.0.1:8090 only") {
		t.Errorf("loopback without a zone: %v", err)
	}
	// asked for HTTP-01, the panel tries it all the same: a port forward may
	// reach it, and the CA's answer says if not
	if plan, err := p.planChallenge([]string{"panel.example.com"}, schema.ChallengeHTTP01, true); err != nil || plan.kind != schema.ChallengeHTTP01 {
		t.Errorf("loopback, HTTP-01 asked for: %q %v", plan.kind, err)
	}
	// with the zone managed, DNS-01 then
	provider := saveRecord(t, app, schema.DNSProviders, map[string]any{"name": "Cloudflare", "kind": "cloudflare", "status": "ok"})
	saveRecord(t, app, schema.DNSZones, map[string]any{"name": "example.com", "provider": provider.Id, "managed": true})
	if plan, _ := p.planChallenge([]string{"panel.example.com"}, schema.ChallengeAuto, true); plan.kind != schema.ChallengeDNS01 {
		t.Errorf("loopback with a managed zone: %q", plan.kind)
	}

	// an edge that serves the name has its port 80
	public := ":80"
	p.https.httpAddr.Store(&public)
	host := saveRecord(t, app, schema.Hosts, map[string]any{
		"name": "vps", "status": "online", "roles": []string{"docker", "edge"}, "public_ip4": "203.0.114.7",
	})
	saveRecord(t, app, schema.Domains, map[string]any{
		"hostname": "panel.example.com", "host": host.Id, "target_kind": schema.TargetContainer, "container": "x", "port": 80, "tls": schema.TLSAuto,
	})
	if _, err := p.planChallenge([]string{"panel.example.com"}, schema.ChallengeHTTP01, true); err == nil ||
		!strings.Contains(err.Error(), "reaches the edge of vps") {
		t.Errorf("an edge's domain: %v", err)
	}
}

func TestTheHTTPSListenerIsWrappedInTLS(t *testing.T) {
	p, app, _ := newTestPanel(t)
	p.Config.HTTPSAddr = "127.0.0.1:0"
	p.Config.PublicURL = "https://panel.example.com"
	authority := openAuthority(t, p, app)

	e := &core.ServeEvent{Server: &http.Server{Addr: "127.0.0.1:0"}}
	if err := p.startHTTPS(e); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stopHTTPS)
	if e.Listener == nil || e.Server.TLSConfig == nil || e.Server.TLSConfig.NextProtos[0] != "h2" {
		t.Fatalf("listener %v, TLS %+v", e.Listener, e.Server.TLSConfig)
	}
	if e.Server.Addr != "127.0.0.1:0" {
		t.Errorf("the server's address changed to %s", e.Server.Addr)
	}

	// HTTPS with the internal CA's certificate, before the first from ACME
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "panel") }), ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(e.Listener) }()
	t.Cleanup(func() { _ = server.Close() })
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(authority.RootPEM())
	conn, err := tls.Dial("tcp", e.Listener.Addr().String(), &tls.Config{ServerName: "panel.example.com", RootCAs: roots, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("the handshake: %v", err)
	}
	_ = conn.Close()

	// the plain port redirects
	address := *p.https.httpAddr.Load()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 5 * time.Second}
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+address+"/servers", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusPermanentRedirect || response.Header.Get("Location") != "https://panel.example.com/servers" {
		t.Errorf("the plain port: %d %q", response.StatusCode, response.Header.Get("Location"))
	}

	// a panel without an https:// public URL does not start
	p2, app2, _ := newTestPanel(t)
	p2.Config.HTTPSAddr = "127.0.0.1:0"
	p2.Config.PublicURL = "http://panel.example.com"
	openAuthority(t, p2, app2)
	if err := p2.startHTTPS(&core.ServeEvent{Server: &http.Server{Addr: "127.0.0.1:0"}}); err == nil {
		t.Error("an http:// public URL was taken")
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		t.Error(err)
	}
}
