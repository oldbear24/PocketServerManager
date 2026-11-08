package panel

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// The panel's own HTTPS (#63). With --https-addr the panel serves its UI and
// API over HTTPS on that address, with a certificate for its public name that
// its ACME manager obtains and renews like an edge's: a certificates record
// with panel set, which no edge ever gets. PocketBase's own --https and
// autocert are not used: the listener PocketBase serves on is wrapped in TLS,
// and PocketBase's --http address becomes the panel's plain port, which
// answers the CA's HTTP-01 challenges and sends everything else to HTTPS.
// Until the first certificate, and for a handshake that names no other name,
// the panel serves the internal CA's gateway certificate. Without
// --https-addr nothing changes: plain HTTP, as behind a reverse proxy. The
// gRPC gateway keeps the internal CA (docs/adr/0007).

// httpsState is the panel's own HTTPS.
type httpsState struct {
	// served is the ACME certificate the panel serves; nil serves the
	// internal CA's
	served  atomic.Pointer[servedCert]
	answers sync.Map // token → panelAnswer

	once   sync.Once
	config *tls.Config

	// httpAddr is where the plain port listens, once it does
	httpAddr atomic.Pointer[string]
	plain    *http.Server
}

type servedCert struct {
	id, name, fingerprint string
	cert                  *tls.Certificate
}

type panelAnswer struct{ domain, keyAuth string }

// panelName is the name the panel gets its certificate for: its public URL's
// host, when it serves HTTPS itself.
func (p *Panel) panelName() string {
	if p.Config == nil {
		return ""
	}
	name, err := p.Config.CheckHTTPS()
	if err != nil {
		return ""
	}
	return name
}

// startHTTPS serves the UI and API over HTTPS on --https-addr, and the plain
// port on --http.
func (p *Panel) startHTTPS(e *core.ServeEvent) error {
	if p.Config == nil || p.Config.HTTPSAddr == "" || e.Server == nil {
		return nil
	}
	name, err := p.Config.CheckHTTPS()
	if err != nil {
		return err
	}
	p.loadPanelCertificate()
	secure, err := net.Listen("tcp", p.Config.HTTPSAddr)
	if err != nil {
		return err
	}
	plainAddr := e.Server.Addr
	if plainAddr == "" {
		plainAddr = ":http"
	}
	plain, err := net.Listen("tcp", plainAddr)
	if err != nil {
		_ = secure.Close()
		return err
	}
	address := plain.Addr().String()
	p.https.httpAddr.Store(&address)

	config := p.HTTPSConfig()
	// the same object, to which the server adds HTTP/2; its Addr stays the
	// plain port's, which PocketBase's start banner shows
	e.Server.TLSConfig = config
	e.Listener = tls.NewListener(secure, config)

	p.https.plain = &http.Server{
		Handler:           p.HTTPHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		if err := p.https.plain.Serve(plain); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.Log.Error("the panel's plain HTTP port stopped", "address", address, "error", err)
		}
	}()
	p.Log.Info("serving HTTPS with the panel's own certificate", "name", name, "https", secure.Addr().String(), "http", address)
	p.later.after(acmeSettle, p.RunACME)
	return nil
}

// stopHTTPS closes the plain port; PocketBase closes the HTTPS one.
func (p *Panel) stopHTTPS() {
	if p.https.plain == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = p.https.plain.Shutdown(ctx)
}

// HTTPSConfig is the TLS of the panel's HTTPS: its certificate by the name
// asked for. It sets no HSTS, so that turning HTTPS off later locks no
// browser out.
func (p *Panel) HTTPSConfig() *tls.Config {
	p.https.once.Do(func() {
		p.https.config = &tls.Config{
			MinVersion:     tls.VersionTLS12,
			NextProtos:     []string{"h2", "http/1.1"},
			GetCertificate: p.panelCertificate,
		}
	})
	return p.https.config
}

// panelCertificate picks what a handshake gets: the ACME certificate for the
// panel's name, the internal CA's gateway certificate otherwise, as before
// the first issuance. An expired ACME certificate is still served, as an
// edge's is: its expiry was alerted before.
func (p *Panel) panelCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if served := p.https.served.Load(); served != nil && strings.EqualFold(hello.ServerName, served.name) &&
		hello.SupportsCertificate(served.cert) == nil {
		return served.cert, nil
	}
	authority := p.authority.Load()
	if authority == nil {
		return nil, errors.New("the internal CA is not open yet")
	}
	return authority.ServerCertificate(), nil
}

// HTTPHandler is the panel's plain port: the CA's HTTP-01 challenges for the
// panel's name are answered, and everything else goes to the public URL over
// HTTPS, never to the host a request names.
func (p *Panel) HTTPHandler() http.Handler {
	origin := strings.TrimSuffix(p.Config.PublicURL, "/")
	if u, err := urlOrigin(p.Config.PublicURL); err == nil {
		origin = u
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token, ok := strings.CutPrefix(r.URL.Path, "/.well-known/acme-challenge/"); ok && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			value, found := p.https.answers.Load(token)
			answer, _ := value.(panelAnswer)
			if !found || !strings.EqualFold(hostOnly(r.Host), answer.domain) {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(answer.keyAuth))
			return
		}
		// the path and query alone, from the origin's root: a request target
		// that is not a path (http:@evil.com, *) cannot change where it leads
		target := r.URL.EscapedPath()
		if r.URL.Opaque != "" || !strings.HasPrefix(target, "/") {
			target = "/"
		}
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, origin+target, http.StatusPermanentRedirect) //nolint:gosec // the origin is the public URL's; the request gives the path alone
	})
}

// panelAnswers answers HTTP-01 for the panel's own name on its plain port,
// as lego's HTTP-01 server would on port 80, which the panel's port is.
type panelAnswers struct{ p *Panel }

func (a *panelAnswers) Present(domain, token, keyAuth string) error {
	a.p.https.answers.Store(token, panelAnswer{domain: domain, keyAuth: keyAuth})
	return nil
}

func (a *panelAnswers) CleanUp(_, token, _ string) error {
	a.p.https.answers.Delete(token)
	return nil
}

// panelRecord is the certificate record of the panel's name, nil if none.
func (p *Panel) panelRecord() *core.Record {
	name := p.panelName()
	if name == "" {
		return nil
	}
	records, err := p.App.FindRecordsByFilter(schema.Certificates, "panel=true", "-created", 0, 0)
	if err != nil {
		return nil
	}
	for _, record := range records {
		if slices.Equal(certificateNames(record), []string{name}) {
			return record
		}
	}
	return nil
}

// panelCertificateID is a record's id, "" without one.
func panelCertificateID(record *core.Record) string {
	if record == nil {
		return ""
	}
	return record.Id
}

// loadPanelCertificate serves the panel's record's certificate from the next
// handshake on, as it was issued or renewed: no restart.
func (p *Panel) loadPanelCertificate() {
	record := p.panelRecord()
	if record == nil || record.GetString("key_pem") == "" || p.Box == nil {
		p.https.served.Store(nil)
		return
	}
	if served := p.https.served.Load(); served != nil && served.id == record.Id && served.fingerprint == record.GetString("fingerprint") {
		return
	}
	key, err := p.Box.OpenString(record.GetString("key_pem"))
	if err == nil {
		var cert tls.Certificate
		if cert, err = tls.X509KeyPair([]byte(record.GetString("chain_pem")), []byte(key)); err == nil {
			p.https.served.Store(&servedCert{id: record.Id, name: p.panelName(), fingerprint: record.GetString("fingerprint"), cert: &cert})
			return
		}
	}
	p.Log.Error("the panel's certificate cannot be served: it keeps the one it had", "certificate", record.Id, "error", err)
}

// servesPanelCertificate says whether a record is what the panel serves.
func (p *Panel) servesPanelCertificate(id string) bool {
	served := p.https.served.Load()
	return served != nil && served.id == id
}

// registerHTTPS swaps the served certificate when its record changes.
func (p *Panel) registerHTTPS() {
	reload := func(e *core.RecordEvent) error {
		if e.Record.GetBool("panel") {
			p.loadPanelCertificate()
		}
		return e.Next()
	}
	p.App.OnRecordAfterUpdateSuccess(schema.Certificates).BindFunc(reload)
	p.App.OnRecordAfterDeleteSuccess(schema.Certificates).BindFunc(reload)
}

// orderPanelCertificate orders the certificate the panel serves its UI and
// API with, under the ACME manager's planning lock, or follows a change of the
// challenge it was asked for.
func (p *Panel) orderPanelCertificate() {
	name := p.panelName()
	if name == "" {
		return
	}
	if record := p.panelRecord(); record != nil {
		if record.GetString("challenge") != p.Config.PanelChallenge() {
			p.setFields(record, map[string]any{"challenge": p.Config.PanelChallenge(), "next_attempt": nil})
		}
		return
	}
	collection, err := p.App.FindCachedCollectionByNameOrId(schema.Certificates)
	if err != nil {
		return
	}
	record := core.NewRecord(collection)
	record.Set("names", []string{name})
	record.Set("issuer", schema.IssuerACME)
	record.Set("challenge", p.Config.PanelChallenge())
	record.Set("status", schema.CertPending)
	record.Set("auto", true)
	record.Set("panel", true)
	if err := p.App.Save(record); err != nil {
		p.Log.Error("failed to order the panel's certificate", "name", name, "error", err)
	}
}

// panelHTTPBlocked says why the CA cannot reach the panel for HTTP-01 on its
// name, or "": its name is an edge's domain, whose port 80 the edge has, or,
// when the challenge is chosen for it, the panel listens for HTTP on a
// loopback address alone. Asked for HTTP-01, the panel tries it there too: a
// port forward or a proxy may reach it, and the CA's answer says if not.
func (p *Panel) panelHTTPBlocked(name string, domains []*core.Record, wanted string) string {
	for _, domain := range domains {
		if !strings.EqualFold(domain.GetString("hostname"), name) {
			continue
		}
		if host, err := p.App.FindRecordById(schema.Hosts, domain.GetString("host")); err == nil && hasEdgeRole(host) {
			return "port 80 of " + name + " reaches the edge of " + host.GetString("name") + ", which serves it as a domain"
		}
	}
	if wanted == schema.ChallengeHTTP01 {
		return ""
	}
	if address := p.https.httpAddr.Load(); address != nil {
		if host, _, err := net.SplitHostPort(*address); err == nil {
			if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
				return "the panel listens for HTTP on " + *address + " only, which the CA cannot reach"
			}
		}
	}
	return ""
}

// hostOnly is a request's host without its port.
func hostOnly(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return hostport
}

// urlOrigin is a URL's scheme and host, its port included.
func urlOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("not an absolute URL")
	}
	return u.Scheme + "://" + u.Host, nil
}
