// Package acmetest runs a test certificate authority in the test's process:
// Pebble, the ACME server Let's Encrypt publishes for testing clients, with a
// DNS server of the test's own that its validation asks. Only tests import it.
package acmetest

import (
	"crypto/x509"
	"encoding/pem"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/letsencrypt/pebble/v2/ca"
	"github.com/letsencrypt/pebble/v2/db"
	"github.com/letsencrypt/pebble/v2/va"
	"github.com/letsencrypt/pebble/v2/wfe"
	"github.com/miekg/dns"
)

// Options configure a test CA.
type Options struct {
	// HTTPPort is where the CA fetches HTTP-01 answers, on the address the
	// DNS server gives a name.
	HTTPPort int
	// Validity is how long certificates last; Pebble's default is 90 days.
	Validity time.Duration
	// EABKeys, when set, are the only external accounts the CA accepts: key
	// ID to base64url HMAC key.
	EABKeys map[string]string
}

// CA is a running test CA.
type CA struct {
	// DirectoryURL is the ACME directory.
	DirectoryURL string
	// Client trusts the CA's own TLS certificate.
	Client *http.Client
	// DNS answers the CA's lookups.
	DNS *DNSServer

	server *httptest.Server
	ca     *ca.CAImpl
}

// Start runs a CA until the test ends.
func Start(t testing.TB, opts Options) *CA {
	t.Helper()
	// no random waits before each validation
	t.Setenv("PEBBLE_VA_NOSLEEP", "1")

	logger := log.New(io.Discard, "", 0)
	store := db.NewMemoryStore()
	// zero is Pebble's default
	var validity uint64
	if seconds := int64(opts.Validity / time.Second); seconds > 0 {
		validity = uint64(seconds)
	}
	profiles := map[string]ca.Profile{"default": {Description: "The default profile", ValidityPeriod: validity}}
	authority := ca.New(logger, store, "", "ecdsa", 0, 1, profiles)

	server := StartDNS(t)
	port := opts.HTTPPort
	if port == 0 {
		port = 5002
	}
	validator := va.New(logger, port, 5001, false, server.Addr, store)
	for kid, key := range opts.EABKeys {
		if err := store.AddExternalAccountKeyByID(kid, key); err != nil {
			t.Fatal(err)
		}
	}
	frontEnd := wfe.New(logger, store, validator, authority, []string{"pebble.letsencrypt.org"}, false, len(opts.EABKeys) > 0, 3, 5)

	https := httptest.NewTLSServer(frontEnd.Handler())
	t.Cleanup(https.Close)
	return &CA{DirectoryURL: https.URL + "/dir", Client: https.Client(), DNS: server, server: https, ca: authority}
}

// Roots are the roots the CA's certificates chain up to.
func (c *CA) Roots() *x509.CertPool {
	pool := x509.NewCertPool()
	if root := c.ca.GetRootCert(0); root != nil {
		pool.AddCert(root.Cert)
	}
	return pool
}

// Verify checks that a chain the CA issued is trusted for a name.
func (c *CA) Verify(chainPEM []byte, name string) error {
	var certs []*x509.Certificate
	rest := chainPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return err
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return x509.CertificateInvalidError{Reason: x509.NotAuthorizedToSign, Detail: "no certificate"}
	}
	intermediates := x509.NewCertPool()
	for _, cert := range certs[1:] {
		intermediates.AddCert(cert)
	}
	_, err := certs[0].Verify(x509.VerifyOptions{DNSName: name, Roots: c.Roots(), Intermediates: intermediates})
	return err
}

// DNSServer answers A and TXT queries over UDP and TCP.
type DNSServer struct {
	Addr string

	mu  sync.Mutex
	a   map[string]string
	txt map[string][]string
	// lookup, when set, answers before the fixed records.
	lookup func(name string, qtype uint16) []string
}

// listenBoth listens on one port for UDP and TCP. A free UDP port may be taken
// for TCP, by a listener or a connection, so it tries a few.
func listenBoth() (net.PacketConn, net.Listener, error) {
	var last error
	for range 20 {
		packet, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			return nil, nil, err
		}
		port := packet.LocalAddr().(*net.UDPAddr).Port
		listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil {
			return packet, listener, nil
		}
		_ = packet.Close()
		last = err
	}
	return nil, nil, last
}

// StartDNS runs a DNS server until the test ends. Every name answers A with
// 127.0.0.1 unless the test gives it another address.
func StartDNS(t testing.TB) *DNSServer {
	t.Helper()
	s := &DNSServer{a: map[string]string{}, txt: map[string][]string{}}

	packet, listener, err := listenBoth()
	if err != nil {
		t.Fatal(err)
	}
	s.Addr = listener.Addr().String()

	udp := &dns.Server{PacketConn: packet, Handler: s}
	tcp := &dns.Server{Listener: listener, Handler: s}
	go func() { _ = udp.ActivateAndServe() }()
	go func() { _ = tcp.ActivateAndServe() }()
	t.Cleanup(func() {
		_ = udp.Shutdown()
		_ = tcp.Shutdown()
	})
	return s
}

func fqdn(name string) string { return dns.Fqdn(strings.ToLower(name)) }

// SetA gives a name an address.
func (s *DNSServer) SetA(name, ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.a[fqdn(name)] = ip
}

// SetTXT gives a name its TXT values; none removes them.
func (s *DNSServer) SetTXT(name string, values ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.txt[fqdn(name)] = values
}

// Lookup answers queries from the test's own records first, such as those
// a fake DNS provider holds; qtype is dns.TypeA or dns.TypeTXT. A nil answer
// falls back to the fixed records.
func (s *DNSServer) Lookup(lookup func(name string, qtype uint16) []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookup = lookup
}

// ServeDNS implements dns.Handler.
func (s *DNSServer) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	reply := new(dns.Msg)
	reply.SetReply(r)
	reply.Authoritative = true
	for _, q := range r.Question {
		name := strings.ToLower(q.Name)
		for _, value := range s.answer(name, q.Qtype) {
			header := dns.RR_Header{Name: q.Name, Rrtype: q.Qtype, Class: dns.ClassINET, Ttl: 0}
			switch q.Qtype {
			case dns.TypeA:
				reply.Answer = append(reply.Answer, &dns.A{Hdr: header, A: net.ParseIP(value).To4()})
			case dns.TypeTXT:
				reply.Answer = append(reply.Answer, &dns.TXT{Hdr: header, Txt: []string{value}})
			}
		}
	}
	_ = w.WriteMsg(reply)
}

func (s *DNSServer) answer(name string, qtype uint16) []string {
	s.mu.Lock()
	lookup := s.lookup
	a, hasA := s.a[name]
	txt := append([]string(nil), s.txt[name]...)
	s.mu.Unlock()

	if lookup != nil {
		if values := lookup(name, qtype); values != nil {
			return values
		}
	}
	switch qtype {
	case dns.TypeA:
		if !hasA {
			a = "127.0.0.1"
		}
		return []string{a}
	case dns.TypeTXT:
		return txt
	}
	return nil
}

// Query asks the server for the values of a name, as the panel's propagation
// check does: it is a dns.Checker lookup.
func (s *DNSServer) Query(name string, qtype uint16) []string {
	return s.answer(fqdn(name), qtype)
}
