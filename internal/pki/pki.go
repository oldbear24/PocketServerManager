// Package pki holds what the panel and the agents share about certificates. It
// has no dependency on PocketBase, so that it can be linked into the agent.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

const (
	// PanelURI is the identity of the gateway's server certificate. Agents verify
	// this name under the pinned root instead of a host name, so a panel behind a
	// changed DNS name, an IP address or a port forward keeps working.
	PanelURI = "pocketcloud://panel"

	// HostURIPrefix is followed by the host id in an agent certificate's SAN.
	HostURIPrefix = "pocketcloud://host/"
)

// PinOf returns the CA_PIN of a DER-encoded certificate.
func PinOf(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// HostIDOf returns the host id from a certificate's SAN, or "".
func HostIDOf(cert *x509.Certificate) string {
	for _, uri := range cert.URIs {
		if id, ok := strings.CutPrefix(uri.String(), HostURIPrefix); ok {
			return id
		}
	}
	return ""
}

// SerialString returns the serial as stored in hosts and revoked_certs.
func SerialString(cert *x509.Certificate) string {
	return hex.EncodeToString(cert.SerialNumber.Bytes())
}

// VerifyPanel is what an agent runs against the gateway's certificates. roots is
// the trusted root: built from the pin on the first connection, and from the
// stored certificate later on.
func VerifyPanel(state tls.ConnectionState, roots *x509.CertPool) error {
	if len(state.PeerCertificates) == 0 {
		return errors.New("the panel sent no certificate")
	}

	leaf := state.PeerCertificates[0]

	intermediates := x509.NewCertPool()
	for _, cert := range state.PeerCertificates[1:] {
		intermediates.AddCert(cert)
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return fmt.Errorf("the panel's certificate is not trusted: %w", err)
	}

	for _, uri := range leaf.URIs {
		if uri.String() == PanelURI {
			return nil
		}
	}

	return errors.New("the certificate does not identify a PocketCloud panel")
}

// RootFromPin finds the certificate matching a CA_PIN among those the panel sent.
func RootFromPin(state tls.ConnectionState, pin string) (*x509.Certificate, error) {
	for _, cert := range state.PeerCertificates {
		if PinOf(cert.Raw) == strings.ToLower(strings.TrimSpace(pin)) {
			return cert, nil
		}
	}
	return nil, errors.New("the panel's certificate does not match CA_PIN: check the value, or the connection may be intercepted")
}

// NewKeyAndCSR generates a P-256 key and a CSR for it, both PEM encoded.
func NewKeyAndCSR(commonName string) (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}

	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: commonName},
	}, key)
	if err != nil {
		return nil, nil, err
	}

	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	csrPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	return keyPEM, csrPEM, nil
}
