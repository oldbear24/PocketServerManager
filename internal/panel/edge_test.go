package panel

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

type issued struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func issue(t *testing.T, name string, parent *issued, ca bool, notAfter time.Time) *issued {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: notAfter,
		IsCA: ca, BasicConstraintsValid: true,
	}
	if !ca {
		template.DNSNames = []string{name}
	}
	signer, signerKey := template, key
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &issued{cert: cert, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

func keyPEM(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestUploadedChainsAreChecked(t *testing.T) {
	year := time.Now().Add(365 * 24 * time.Hour)
	root := issue(t, "Root", nil, true, year)
	intermediate := issue(t, "Intermediate", root, true, year)
	leaf := issue(t, "app.example.com", intermediate, false, year)
	expiredIntermediate := issue(t, "Old intermediate", root, true, time.Now().Add(-time.Hour))
	leafOfExpired := issue(t, "app.example.com", expiredIntermediate, false, year)
	other := issue(t, "Other", nil, true, year)

	if parsed, err := parseCertificate(leaf.pem+intermediate.pem, keyPEM(t, leaf.key)); err != nil || len(parsed.names) != 1 {
		t.Fatalf("a good chain: %v", err)
	}

	cases := map[string]struct{ chain, key, want string }{
		// the first certificate must fit the key, so this fails before the chain
		"links in the wrong order":     {intermediate.pem + leaf.pem, keyPEM(t, leaf.key), "do not fit"},
		"an unrelated intermediate":    {leaf.pem + other.pem, keyPEM(t, leaf.key), "did not sign"},
		"an expired intermediate":      {leafOfExpired.pem + expiredIntermediate.pem, keyPEM(t, leafOfExpired.key), "expired"},
		"a key with a password":        {leaf.pem, "-----BEGIN ENCRYPTED PRIVATE KEY-----\nAA==\n-----END ENCRYPTED PRIVATE KEY-----\n", "password"},
		"a key of another certificate": {leaf.pem, keyPEM(t, other.key), "do not fit"},
	}
	for name, c := range cases {
		_, err := parseCertificate(c.chain, c.key)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
