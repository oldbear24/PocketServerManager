package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/pki"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

func newAuthority(t *testing.T) (*Authority, *secrets.Box) {
	t.Helper()

	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)

	box, err := secrets.New(testutil.MasterKey)
	if err != nil {
		t.Fatal(err)
	}

	authority, err := Open(app, box, []string{"panel.example.com"})
	if err != nil {
		t.Fatal(err)
	}

	return authority, box
}

func newCSR(t *testing.T) []byte {
	t.Helper()

	_, csr, err := pki.NewKeyAndCSR("someone")
	if err != nil {
		t.Fatal(err)
	}

	return csr
}

func chainOf(t *testing.T, chainPEM []byte) []*x509.Certificate {
	t.Helper()

	var chain []*x509.Certificate
	for block, rest := pem.Decode(chainPEM); block != nil; block, rest = pem.Decode(rest) {
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		chain = append(chain, cert)
	}

	return chain
}

// TestKeysAreNeverStoredInPlaintext is an acceptance criterion of #7. It looks at
// the actual files of the database, not only at what the API returns.
func TestKeysAreNeverStoredInPlaintext(t *testing.T) {
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)

	box, err := secrets.New(testutil.MasterKey)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Open(app, box, nil); err != nil {
		t.Fatal(err)
	}

	records, err := app.FindRecordsByFilter(schema.InternalCA, "", "", 0, 0)
	if err != nil || len(records) != 1 {
		t.Fatalf("%d records, %v", len(records), err)
	}

	for _, field := range schema.Encrypted[schema.InternalCA] {
		value := records[0].GetString(field)
		if !secrets.IsSealed(value) {
			t.Errorf("%s is not sealed: %.40s", field, value)
		}

		opened, err := box.Open(value)
		if err != nil {
			t.Errorf("%s cannot be opened: %v", field, err)
		}
		if !strings.Contains(string(opened), "PRIVATE KEY") {
			t.Errorf("%s does not hold a key", field)
		}
	}

	// flush the write-ahead log, then look at every file of the data directory
	if _, err := app.DB().NewQuery("PRAGMA wal_checkpoint(TRUNCATE)").Execute(); err != nil {
		t.Fatal(err)
	}

	err = filepath.Walk(app.DataDir(), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), "PRIVATE KEY") {
			t.Errorf("a private key is in plain text in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestReopeningKeepsTheCA(t *testing.T) {
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)

	box, _ := secrets.New(testutil.MasterKey)

	first, err := Open(app, box, []string{"panel.example.com"})
	if err != nil {
		t.Fatal(err)
	}

	second, err := Open(app, box, []string{"panel.example.com"})
	if err != nil {
		t.Fatal(err)
	}

	if first.Pin() != second.Pin() {
		t.Fatal("the pin changed: every agent would have to enroll again")
	}

	// a certificate of the first instance is valid for the second
	issued, err := first.IssueAgentCert(newCSR(t), "host1")
	if err != nil {
		t.Fatal(err)
	}
	if id, _, err := second.VerifyAgent(chainOf(t, issued.ChainPEM)); err != nil || id != "host1" {
		t.Fatalf("%s, %v", id, err)
	}

	// the wrong master key must not start with an unreadable CA
	other, _ := secrets.New([]byte("another-key-another-key-another-"))
	if _, err := Open(app, other, nil); err == nil {
		t.Fatal("opening the CA with another master key must fail")
	}
}

func TestServerCertificate(t *testing.T) {
	authority, _ := newAuthority(t)

	certificate, err := authority.ServerTLSConfig().GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}

	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(authority.RootPEM())

	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	if err := pki.VerifyPanel(state, roots); err != nil {
		t.Fatalf("an agent must accept the panel: %v", err)
	}

	// the root is in the chain, so that an agent can find it by its pin
	if len(certificate.Certificate) != 2 {
		t.Fatalf("the chain has %d certificates", len(certificate.Certificate))
	}
	root, err := x509.ParseCertificate(certificate.Certificate[1])
	if err != nil {
		t.Fatal(err)
	}
	if pki.PinOf(root.Raw) != authority.Pin() {
		t.Error("the certificate after the leaf is not the pinned root")
	}

	if err := leaf.VerifyHostname("panel.example.com"); err != nil {
		t.Errorf("host name: %v", err)
	}
}

func TestServerCertificateFollowsTheNames(t *testing.T) {
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)

	box, _ := secrets.New(testutil.MasterKey)

	serverCert := func(names ...string) *x509.Certificate {
		authority, err := Open(app, box, names)
		if err != nil {
			t.Fatal(err)
		}
		certificate, err := authority.ServerTLSConfig().GetCertificate(nil)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(certificate.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		return leaf
	}

	first := serverCert("a.example.com")
	same := serverCert("a.example.com")
	if first.SerialNumber.Cmp(same.SerialNumber) != 0 {
		t.Error("the certificate must be reused while the names stay the same")
	}

	changed := serverCert("b.example.com")
	if first.SerialNumber.Cmp(changed.SerialNumber) == 0 {
		t.Error("new names need a new certificate")
	}
	if err := changed.VerifyHostname("b.example.com"); err != nil {
		t.Error(err)
	}
}

func TestAgentCertificate(t *testing.T) {
	authority, _ := newAuthority(t)

	issued, err := authority.IssueAgentCert(newCSR(t), "host-id-1")
	if err != nil {
		t.Fatal(err)
	}

	chain := chainOf(t, issued.ChainPEM)
	leaf := chain[0]

	// the identity comes from the panel, never from the CSR
	if pki.HostIDOf(leaf) != "host-id-1" {
		t.Errorf("host id: %q", pki.HostIDOf(leaf))
	}
	if strings.Contains(leaf.Subject.CommonName, "someone") {
		t.Error("a name from the CSR reached the certificate")
	}

	if days := time.Until(leaf.NotAfter).Hours() / 24; days < 179 || days > 181 {
		t.Errorf("valid for %.1f days", days)
	}

	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("an agent certificate must only be good for client auth: %v", leaf.ExtKeyUsage)
	}
	if leaf.IsCA {
		t.Error("an agent certificate must not be a CA")
	}

	id, serial, err := authority.VerifyAgent(chain)
	if err != nil || id != "host-id-1" || serial != issued.Serial {
		t.Fatalf("%s %s %v", id, serial, err)
	}

	// an agent's certificate cannot pass as the panel
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(authority.RootPEM())
	state := tls.ConnectionState{PeerCertificates: chain}
	if err := pki.VerifyPanel(state, roots); err == nil {
		t.Error("an agent certificate must not be accepted as the panel")
	}
}

func TestBadCSRs(t *testing.T) {
	authority, _ := newAuthority(t)

	for name, csr := range map[string][]byte{
		"empty":   nil,
		"not PEM": []byte("hello"),
		"wrong":   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")}),
		"garbage": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: []byte("x")}),
	} {
		if _, err := authority.IssueAgentCert(csr, "host"); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestExpiredCertificate(t *testing.T) {
	authority, _ := newAuthority(t)
	authority.AgentTTL = -time.Minute

	issued, err := authority.IssueAgentCert(newCSR(t), "host")
	if err != nil {
		t.Fatal(err)
	}

	_, serial, err := authority.VerifyAgent(chainOf(t, issued.ChainPEM))
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("%v", err)
	}
	if serial != issued.Serial {
		t.Errorf("the serial is still needed for the event: %q", serial)
	}
}

// TestForgedCertificates: a certificate that names a host, from anyone else's CA.
func TestForgedCertificates(t *testing.T) {
	authority, _ := newAuthority(t)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	identity, err := url.Parse(pki.HostURIPrefix + "victim")
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "victim"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{identity},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}

	forged, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := authority.VerifyAgent([]*x509.Certificate{forged}); err == nil {
		t.Fatal("a self-signed certificate was accepted")
	}

	if _, _, err := authority.VerifyAgent(nil); err == nil {
		t.Fatal("no certificate at all was accepted")
	}

	// the gateway's own certificate is from the right root, but is no client
	certificate, err := authority.ServerTLSConfig().GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	server, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := authority.VerifyAgent([]*x509.Certificate{server}); err == nil {
		t.Fatal("the server certificate was accepted as an agent")
	}
}

func TestPins(t *testing.T) {
	authority, _ := newAuthority(t)

	certificate, err := authority.ServerTLSConfig().GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}

	var chain []*x509.Certificate
	for _, der := range certificate.Certificate {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		chain = append(chain, cert)
	}

	state := tls.ConnectionState{PeerCertificates: chain}

	if _, err := pki.RootFromPin(state, authority.Pin()); err != nil {
		t.Errorf("the right pin: %v", err)
	}
	if _, err := pki.RootFromPin(state, strings.ToUpper(authority.Pin())); err != nil {
		t.Errorf("a pin in capitals: %v", err)
	}
	if _, err := pki.RootFromPin(state, "sha256:0000"); err == nil {
		t.Error("a wrong pin must be refused")
	}
}

func TestLANCertificatesChainToTheRoot(t *testing.T) {
	authority, _ := newAuthority(t)

	issued, err := authority.IssueLANCert([]string{"Wiki.lan", "*.home.arpa", "192.168.1.10"})
	if err != nil {
		t.Fatal(err)
	}
	chain := chainOf(t, issued.ChainPEM)
	if len(chain) != 2 {
		t.Fatalf("the chain has %d certificates: the leaf and the LAN intermediate, without the root", len(chain))
	}
	leaf := chain[0]

	roots := x509.NewCertPool()
	block, _ := pem.Decode(authority.RootPEM())
	root, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(root)
	intermediates := x509.NewCertPool()
	intermediates.AddCert(chain[1])
	for _, name := range []string{"wiki.lan", "nas.home.arpa", "192.168.1.10"} {
		if _, err := leaf.Verify(x509.VerifyOptions{
			DNSName: name, Roots: roots, Intermediates: intermediates,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			t.Errorf("%s does not verify against the root: %v", name, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "other.lan", Roots: roots, Intermediates: intermediates}); err == nil {
		t.Error("the certificate verifies for a name it does not have")
	}
	// it is no agent certificate
	if _, _, err := authority.VerifyAgent(chain); err == nil {
		t.Error("a LAN certificate passes as an agent's")
	}

	if lifetime := issued.NotAfter.Sub(issued.NotBefore); lifetime > 398*24*time.Hour {
		t.Errorf("valid for %v, more than browsers allow", lifetime)
	}
	if !leaf.NotAfter.Equal(issued.NotAfter) {
		t.Errorf("not after %v, the certificate says %v", issued.NotAfter, leaf.NotAfter)
	}
	if chain[1].Subject.CommonName != "PocketCloud LAN" || !chain[1].IsCA {
		t.Errorf("the intermediate is %q", chain[1].Subject)
	}

	// the key belongs to the certificate
	if _, err := tls.X509KeyPair(issued.ChainPEM, issued.KeyPEM); err != nil {
		t.Fatal(err)
	}

	if _, err := authority.IssueLANCert(nil); err == nil {
		t.Error("a certificate without a name was issued")
	}
}

func TestTheLANIntermediateIsKeptAndAddedToAnOlderCA(t *testing.T) {
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)
	box, _ := secrets.New(testutil.MasterKey)

	first, err := Open(app, box, nil)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := first.IssueLANCert([]string{"wiki.lan"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(app, box, nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := second.IssueLANCert([]string{"wiki.lan"})
	if err != nil {
		t.Fatal(err)
	}
	if a, b := chainOf(t, issued.ChainPEM)[1], chainOf(t, again.ChainPEM)[1]; !a.Equal(b) {
		t.Fatal("reopening the CA made another LAN intermediate")
	}

	// a CA made before LAN certificates gets its intermediate, with the same root
	records, err := app.FindRecordsByFilter(schema.InternalCA, "", "", 0, 0)
	if err != nil || len(records) != 1 {
		t.Fatalf("%d records, %v", len(records), err)
	}
	records[0].Set("lan_intermediate_cert", "")
	records[0].Set("lan_intermediate_key", "")
	if err := app.Save(records[0]); err != nil {
		t.Fatal(err)
	}
	third, err := Open(app, box, nil)
	if err != nil {
		t.Fatal(err)
	}
	if third.Pin() != first.Pin() {
		t.Fatal("the root changed")
	}
	if _, err := third.IssueLANCert([]string{"wiki.lan"}); err != nil {
		t.Fatal(err)
	}
	stored, _ := app.FindRecordById(schema.InternalCA, records[0].Id)
	if !secrets.IsSealed(stored.GetString("lan_intermediate_key")) || stored.GetString("lan_intermediate_cert") == "" {
		t.Fatal("the new intermediate was not stored, sealed")
	}

	info := third.RootInfo()
	if info.Subject != "PocketCloud Root CA" || len(info.SHA256) != 95 || time.Until(info.NotAfter) < 19*365*24*time.Hour {
		t.Fatalf("%+v", info)
	}
}
