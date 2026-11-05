package acme_test

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/oldbear24/PocketServerManager/internal/acme"
	"github.com/oldbear24/PocketServerManager/internal/acme/acmetest"
)

// answers serves HTTP-01 answers, as an edge does on port 80.
type answers struct {
	mu     sync.Mutex
	tokens map[string]string
	port   int
}

func serveAnswers(t *testing.T) *answers {
	t.Helper()
	a := &answers{tokens: map[string]string{}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a.port = listener.Addr().(*net.TCPAddr).Port
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.URL.Path, "/.well-known/acme-challenge/")
		a.mu.Lock()
		answer, ok := a.tokens[token]
		a.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(answer))
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return a
}

// Present and CleanUp make answers an HTTP-01 provider.
func (a *answers) Present(_, token, keyAuth string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tokens[token] = keyAuth
	return nil
}

func (a *answers) CleanUp(_, token, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.tokens, token)
	return nil
}

// silent answers nothing: the CA finds no answer.
type silent struct{}

func (silent) Present(string, string, string) error { return nil }
func (silent) CleanUp(string, string, string) error { return nil }

// txt puts DNS-01 answers in the test DNS server.
type txt struct {
	ca *acmetest.CA
	mu sync.Mutex
	// values by name, as several names can share one, like a name and its
	// wildcard
	values  map[string][]string
	cleaned []string
}

func (d *txt) Present(domain, _, keyAuth string) error {
	info := acme.ChallengeInfo(domain, keyAuth)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.values[info.FQDN] = append(d.values[info.FQDN], info.Value)
	d.ca.DNS.SetTXT(info.FQDN, d.values[info.FQDN]...)
	return nil
}

func (d *txt) CleanUp(domain, _, keyAuth string) error {
	info := acme.ChallengeInfo(domain, keyAuth)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.values[info.FQDN] = slices.DeleteFunc(d.values[info.FQDN], func(v string) bool { return v == info.Value })
	d.ca.DNS.SetTXT(info.FQDN, d.values[info.FQDN]...)
	d.cleaned = append(d.cleaned, info.FQDN)
	return nil
}

func (d *txt) Timeout() (time.Duration, time.Duration) {
	return 10 * time.Second, 50 * time.Millisecond
}

func account(t *testing.T, ca *acmetest.CA) *acme.Account {
	t.Helper()
	key, err := acme.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return &acme.Account{DirectoryURL: ca.DirectoryURL, Email: "admin@example.com", Key: key}
}

func TestHTTP01GetsATrustedCertificate(t *testing.T) {
	served := serveAnswers(t)
	ca := acmetest.Start(t, acmetest.Options{HTTPPort: served.port})
	acct := account(t, ca)

	uri, err := acme.Register(acct, ca.Client)
	if err != nil || uri == "" {
		t.Fatalf("register: %q %v", uri, err)
	}
	acct.URI = uri

	cert, err := acme.Obtain(acme.Request{Account: acct, Names: []string{"shop.example.test"}, HTTP01: served, HTTPClient: ca.Client})
	if err != nil {
		t.Fatal(acme.Explain(err))
	}
	if err := ca.Verify(cert.ChainPEM, "shop.example.test"); err != nil {
		t.Errorf("the chain is not trusted: %v", err)
	}
	if _, err := tls.X509KeyPair(cert.ChainPEM, cert.KeyPEM); err != nil {
		t.Errorf("the key does not belong to the certificate: %v", err)
	}
	if !cert.NotAfter.After(time.Now().Add(80*24*time.Hour)) || cert.NotBefore.After(time.Now().Add(time.Minute)) {
		t.Errorf("validity %v – %v", cert.NotBefore, cert.NotAfter)
	}
	if cert.AccountURI != uri {
		t.Errorf("account %q, want %q", cert.AccountURI, uri)
	}
	if len(served.tokens) != 0 {
		t.Errorf("answers left behind: %v", served.tokens)
	}
}

func TestDNS01CoversAWildcard(t *testing.T) {
	ca := acmetest.Start(t, acmetest.Options{Validity: 48 * time.Hour})
	provider := &txt{ca: ca, values: map[string][]string{}}
	asked := 0
	propagated := func(domain, fqdn, value string) (bool, error) {
		asked++
		if !strings.HasPrefix(fqdn, "_acme-challenge.") {
			return false, fmt.Errorf("lego asked about %s", fqdn)
		}
		return slices.Contains(ca.DNS.Query(fqdn, dns.TypeTXT), value), nil
	}

	cert, err := acme.Obtain(acme.Request{
		Account: account(t, ca), Names: []string{"example.test", "*.example.test"},
		DNS01: provider, Propagated: propagated, HTTPClient: ca.Client,
	})
	if err != nil {
		t.Fatal(acme.Explain(err))
	}
	for _, name := range []string{"example.test", "app.example.test"} {
		if err := ca.Verify(cert.ChainPEM, name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := cert.NotAfter.Sub(cert.NotBefore); got > 49*time.Hour || got < 47*time.Hour {
		t.Errorf("lifetime %v", got)
	}
	if asked == 0 {
		t.Error("the propagation check was never asked")
	}
	if len(provider.cleaned) != 2 || len(ca.DNS.Query("_acme-challenge.example.test", dns.TypeTXT)) != 0 {
		t.Errorf("cleaned %v, left %v", provider.cleaned, ca.DNS.Query("_acme-challenge.example.test", dns.TypeTXT))
	}
}

func TestAFailedChallengeSaysWhatTheCASaw(t *testing.T) {
	served := serveAnswers(t)
	ca := acmetest.Start(t, acmetest.Options{HTTPPort: served.port})

	_, err := acme.Obtain(acme.Request{Account: account(t, ca), Names: []string{"shop.example.test"}, HTTP01: silent{}, HTTPClient: ca.Client})
	if err == nil {
		t.Fatal("a certificate without an answer")
	}
	explained := acme.Explain(err)
	if !strings.HasPrefix(explained, "shop.example.test: ") || !strings.Contains(explained, "404") || strings.Contains(explained, "one or more domains") {
		t.Errorf("explained as %q (from %v)", explained, err)
	}
}

func TestAnAccountTheCAForgotIsRegisteredAgain(t *testing.T) {
	served := serveAnswers(t)
	ca := acmetest.Start(t, acmetest.Options{HTTPPort: served.port})
	acct := account(t, ca)
	acct.URI = strings.TrimSuffix(ca.DirectoryURL, "/dir") + "/my-account/424242"

	cert, err := acme.Obtain(acme.Request{Account: acct, Names: []string{"shop.example.test"}, HTTP01: served, HTTPClient: ca.Client})
	if err != nil {
		t.Fatal(acme.Explain(err))
	}
	if cert.AccountURI == "" || strings.HasSuffix(cert.AccountURI, "/424242") {
		t.Errorf("account %q", cert.AccountURI)
	}
}

func TestExternalAccountBinding(t *testing.T) {
	served := serveAnswers(t)
	ca := acmetest.Start(t, acmetest.Options{HTTPPort: served.port, EABKeys: map[string]string{
		"kid-1": "zWNDZM6eQGHWpSRTPal5eIUYFTu7EajVIoguysqZ9wG44nMEtx3MUAsUDkMTQ12W",
	}})

	_, err := acme.Register(account(t, ca), ca.Client)
	if err == nil || !strings.Contains(acme.Explain(err), "externalAccountRequired") {
		t.Errorf("without a binding: %v", acme.Explain(err))
	}

	bound := account(t, ca)
	bound.EABKid = "kid-1"
	bound.EABHMAC = "zWNDZM6eQGHWpSRTPal5eIUYFTu7EajVIoguysqZ9wG44nMEtx3MUAsUDkMTQ12W"
	if uri, err := acme.Register(bound, ca.Client); err != nil || uri == "" {
		t.Errorf("with a binding: %q %v", uri, acme.Explain(err))
	}
}

func TestKeysRoundTrip(t *testing.T) {
	key, err := acme.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	text, err := acme.EncodeKey(key)
	if err != nil {
		t.Fatal(err)
	}
	back, err := acme.DecodeKey(text)
	if err != nil || !key.Equal(back) {
		t.Errorf("decoded %v %v", back, err)
	}
	if _, err := acme.DecodeKey("not a key"); err == nil {
		t.Error("a bad key was read")
	}
}

// TestExplainKeepsTheNamesApart: the errors as lego 4.35 builds them, the
// resolver's map of names and the certifier's nested joins, come out as one
// line per name without lego's framing.
func TestExplainKeepsTheNamesApart(t *testing.T) {
	// the certifier: errors.Join(errors.Join(nil, "a: e"), "b: e")
	var certifier error
	for _, name := range []string{"a.example.test", "b.example.test", "c.example.test"} {
		certifier = errors.Join(certifier, fmt.Errorf("%s: %w", name, errors.New("acme: error: the CSR is bad")))
	}
	certifier = fmt.Errorf("error: one or more domains had a problem:\n%w", certifier)
	want := "a.example.test: the CSR is bad\nb.example.test: the CSR is bad\nc.example.test: the CSR is bad"
	if got := acme.Explain(certifier); got != want {
		t.Errorf("the certifier's joins: got %q\nwant %q", got, want)
	}

	// the resolver: a map by name, its errors framed "[name] acme: ..."
	resolver := mapError{
		"b.example.test": fmt.Errorf("[b.example.test] acme: error presenting token: %w", errors.New("the edge of web could not put the answer in place: offline")),
		"a.example.test": fmt.Errorf("[a.example.test] acme: error cleaning up: %w", errors.New("gone")),
	}
	want = "a.example.test: gone\nb.example.test: the edge of web could not put the answer in place: offline"
	if got := acme.Explain(resolver); got != want {
		t.Errorf("the resolver's map: got %q\nwant %q", got, want)
	}
	if acme.Explain(nil) != "" {
		t.Error("nil explained")
	}
}

// mapError is shaped like lego's resolver.obtainError.
type mapError map[string]error

func (e mapError) Error() string { return "error: one or more domains had a problem" }

// TestTheCallersClientIsLeftAlone: lego changes the transport of the client
// it gets; the panel shares one between issuances that run at once.
func TestTheCallersClientIsLeftAlone(t *testing.T) {
	served := serveAnswers(t)
	ca := acmetest.Start(t, acmetest.Options{HTTPPort: served.port})
	transport := ca.Client.Transport
	if _, err := acme.Register(account(t, ca), ca.Client); err != nil {
		t.Fatal(acme.Explain(err))
	}
	if ca.Client.Transport != transport {
		t.Error("the caller's client was changed")
	}
}
