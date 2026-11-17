package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/pki"
)

// testCA is an internal CA as the panel's, for tests that dial a panel: its
// root signs the gateway's certificate, which names pocketcloud://panel,
// and the agents' ones, which name their host.
type testCA struct {
	root *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T, name string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{root: root, key: key}
}

// rootPEM is the root, as an agent's ca.pem holds it.
func (c *testCA) rootPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.root.Raw})
}

// issue signs a certificate naming uri for usage, and returns it and its
// key, PEM encoded.
func (c *testCA) issue(t *testing.T, uri string, usage x509.ExtKeyUsage) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	name, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: uri},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(12 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, URIs: []*url.URL{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, c.root, &key.PublicKey, c.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// testIdentityPEM is an agent's key, certificate and CA, as enrollment
// leaves them, of a CA of its own.
func testIdentityPEM(t *testing.T) (keyPEM, certPEM, caPEM []byte) {
	t.Helper()
	ca := newTestCA(t, "PocketCloud test root")
	certPEM, keyPEM = ca.issue(t, pki.HostURIPrefix+"h1", x509.ExtKeyUsageClientAuth)
	return keyPEM, certPEM, ca.rootPEM()
}
