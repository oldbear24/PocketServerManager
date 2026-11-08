// Package ca is the panel's internal certificate authority. Agents authenticate
// with client certificates it issues; no passwords or API tokens live on servers.
//
// The hierarchy is a root (ECDSA P-256), an intermediate for agent identities, a
// server certificate for the gRPC gateway, and an intermediate that signs the
// certificates of LAN names (#26), which browsers trust once the root is
// installed. Private keys are stored only sealed with the master key.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/pki"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
)

const (
	// AgentCertTTL is the validity of an agent certificate.
	AgentCertTTL = 180 * 24 * time.Hour
	// RenewBefore is how long before expiry an agent renews its certificate.
	RenewBefore = 30 * 24 * time.Hour

	// LANCertTTL is the validity of a certificate for LAN names: under the
	// 398 days that browsers allow public CAs, and well under the 825 days
	// that Apple's platforms accept from a root a user installed.
	LANCertTTL = 397 * 24 * time.Hour

	rootTTL         = 20 * 365 * 24 * time.Hour
	intermediateTTL = 10 * 365 * 24 * time.Hour
	serverCertTTL   = 365 * 24 * time.Hour
	serverRenewal   = 30 * 24 * time.Hour
	// a LAN intermediate is replaced while it still outlives every leaf it
	// could sign
	lanIntermediateRenewal = LANCertTTL + 30*24*time.Hour
)

// Authority issues and verifies certificates.
type Authority struct {
	app core.App
	box *secrets.Box

	root            *x509.Certificate
	rootPEM         []byte
	rootKey         *ecdsa.PrivateKey
	intermediate    *x509.Certificate
	intermediatePEM []byte
	intermediateKey *ecdsa.PrivateKey

	lan atomic.Pointer[lanIssuer]

	roots *x509.CertPool

	server atomic.Pointer[tls.Certificate]

	// AgentTTL overrides the validity of agent certificates in tests.
	AgentTTL time.Duration
}

// Open loads the authority from the internal_ca record, creating it on first
// start, and makes sure there is a valid server certificate for serverNames.
func Open(app core.App, box *secrets.Box, serverNames []string) (*Authority, error) {
	a := &Authority{app: app, box: box, AgentTTL: AgentCertTTL}

	records, err := app.FindRecordsByFilter(schema.InternalCA, "", "-created", 1, 0)
	if err != nil {
		return nil, err
	}

	var record *core.Record
	if len(records) > 0 {
		record = records[0]
		if err := a.load(record); err != nil {
			return nil, err
		}
	} else {
		record, err = a.create()
		if err != nil {
			return nil, err
		}
	}

	if err := a.ensureServerCert(record, serverNames); err != nil {
		return nil, err
	}
	if err := a.ensureLANIntermediate(record); err != nil {
		return nil, fmt.Errorf("LAN intermediate: %w", err)
	}

	return a, nil
}

func (a *Authority) create() (*core.Record, error) {
	now := time.Now()

	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	rootTemplate := &x509.Certificate{
		SerialNumber:          newSerial(),
		Subject:               pkix.Name{CommonName: "PocketCloud Root CA", Organization: []string{"PocketCloud"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(rootTTL),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, rootKey.Public(), rootKey)
	if err != nil {
		return nil, err
	}

	intermediateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		return nil, err
	}

	intermediateTemplate := &x509.Certificate{
		SerialNumber:          newSerial(),
		Subject:               pkix.Name{CommonName: "PocketCloud Agents", Organization: []string{"PocketCloud"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(intermediateTTL),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	intermediateDER, err := x509.CreateCertificate(rand.Reader, intermediateTemplate, root, intermediateKey.Public(), rootKey)
	if err != nil {
		return nil, err
	}

	intermediate, err := x509.ParseCertificate(intermediateDER)
	if err != nil {
		return nil, err
	}

	a.setMaterial(root, rootKey, intermediate, intermediateKey)

	sealedRoot, err := a.sealKey(rootKey)
	if err != nil {
		return nil, err
	}
	sealedIntermediate, err := a.sealKey(intermediateKey)
	if err != nil {
		return nil, err
	}

	collection, err := a.app.FindCollectionByNameOrId(schema.InternalCA)
	if err != nil {
		return nil, err
	}

	record := core.NewRecord(collection)
	record.Set("root_cert", string(a.rootPEM))
	record.Set("root_key", sealedRoot)
	record.Set("agent_intermediate_cert", string(a.intermediatePEM))
	record.Set("agent_intermediate_key", sealedIntermediate)

	if err := a.app.Save(record); err != nil {
		return nil, err
	}

	return record, nil
}

func (a *Authority) load(record *core.Record) error {
	root, err := parseCertPEM(record.GetString("root_cert"))
	if err != nil {
		return fmt.Errorf("root certificate: %w", err)
	}
	rootKey, err := a.openKey(record.GetString("root_key"))
	if err != nil {
		return fmt.Errorf("root key: %w", err)
	}
	intermediate, err := parseCertPEM(record.GetString("agent_intermediate_cert"))
	if err != nil {
		return fmt.Errorf("agent intermediate certificate: %w", err)
	}
	intermediateKey, err := a.openKey(record.GetString("agent_intermediate_key"))
	if err != nil {
		return fmt.Errorf("agent intermediate key: %w", err)
	}

	a.setMaterial(root, rootKey, intermediate, intermediateKey)

	return nil
}

func (a *Authority) setMaterial(root *x509.Certificate, rootKey *ecdsa.PrivateKey, intermediate *x509.Certificate, intermediateKey *ecdsa.PrivateKey) {
	a.root = root
	a.rootKey = rootKey
	a.rootPEM = encodeCertPEM(root)
	a.intermediate = intermediate
	a.intermediateKey = intermediateKey
	a.intermediatePEM = encodeCertPEM(intermediate)
	a.roots = x509.NewCertPool()
	a.roots.AddCert(root)
}

// ensureServerCert (re)issues the gateway's certificate when it is missing, about
// to expire or does not cover the configured names.
func (a *Authority) ensureServerCert(record *core.Record, names []string) error {
	names = normalizeNames(names)

	certPEM := record.GetString("server_cert")
	sealedKey := record.GetString("server_key")
	expiresAt := record.GetDateTime("server_expires_at").Time()

	stored := []string{}
	_ = json.Unmarshal([]byte(record.GetString("server_names")), &stored)

	if certPEM != "" && sealedKey != "" && slices.Equal(stored, names) && time.Until(expiresAt) > serverRenewal {
		keyPEM, err := a.box.Open(sealedKey)
		if err != nil {
			return fmt.Errorf("server key: %w", err)
		}
		cert, err := tls.X509KeyPair([]byte(certPEM), keyPEM)
		if err != nil {
			return fmt.Errorf("server certificate: %w", err)
		}
		a.server.Store(&cert)
		return nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	now := time.Now()
	panelURI, err := url.Parse(pki.PanelURI)
	if err != nil {
		return err
	}

	template := &x509.Certificate{
		SerialNumber: newSerial(),
		Subject:      pkix.Name{CommonName: "PocketCloud gateway", Organization: []string{"PocketCloud"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(serverCertTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		URIs:         []*url.URL{panelURI},
	}
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, name)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, template, a.root, key.Public(), a.rootKey)
	if err != nil {
		return err
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}

	// the chain includes the root, so that an agent can check it against its pin
	chainPEM := append(encodeCertPEM(leaf), a.rootPEM...)

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	sealed, err := a.box.Seal(keyPEM)
	if err != nil {
		return err
	}

	record.Set("server_cert", string(chainPEM))
	record.Set("server_key", sealed)
	record.Set("server_names", names)
	expires, err := types.ParseDateTime(leaf.NotAfter)
	if err != nil {
		return err
	}
	record.Set("server_expires_at", expires)
	if err := a.app.Save(record); err != nil {
		return err
	}

	cert, err := tls.X509KeyPair(chainPEM, keyPEM)
	if err != nil {
		return err
	}
	a.server.Store(&cert)

	return nil
}

// lanIssuer is the intermediate that signs the certificates of LAN names.
type lanIssuer struct {
	cert *x509.Certificate
	pem  []byte
	key  *ecdsa.PrivateKey
}

// ensureLANIntermediate loads the LAN intermediate, or makes it when the CA
// has none yet, as one made before #26, or when it would not outlive a new
// leaf.
func (a *Authority) ensureLANIntermediate(record *core.Record) error {
	if certPEM, sealed := record.GetString("lan_intermediate_cert"), record.GetString("lan_intermediate_key"); certPEM != "" && sealed != "" {
		cert, err := parseCertPEM(certPEM)
		if err != nil {
			return err
		}
		key, err := a.openKey(sealed)
		if err != nil {
			return err
		}
		if time.Until(cert.NotAfter) > lanIntermediateRenewal {
			a.lan.Store(&lanIssuer{cert: cert, pem: encodeCertPEM(cert), key: key})
			return nil
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          newSerial(),
		Subject:               pkix.Name{CommonName: "PocketCloud LAN", Organization: []string{"PocketCloud"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              earliest(now.Add(intermediateTTL), a.root.NotAfter),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.root, key.Public(), a.rootKey)
	if err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	sealed, err := a.sealKey(key)
	if err != nil {
		return err
	}
	record.Set("lan_intermediate_cert", string(encodeCertPEM(cert)))
	record.Set("lan_intermediate_key", sealed)
	if err := a.app.Save(record); err != nil {
		return err
	}
	a.lan.Store(&lanIssuer{cert: cert, pem: encodeCertPEM(cert), key: key})
	return nil
}

// LANCertificate is a certificate for LAN names and its key.
type LANCertificate struct {
	// ChainPEM holds the certificate followed by the LAN intermediate; the
	// root is left out, as clients must have it installed.
	ChainPEM  []byte
	KeyPEM    []byte
	NotBefore time.Time
	NotAfter  time.Time
}

// IssueLANCert signs a server certificate for names, such as wiki.lan or
// *.home.arpa, and IP addresses. Which names may get one is the panel's
// choice: the root signs for any name, so a client that installed it trusts
// every certificate of this CA.
func (a *Authority) IssueLANCert(names []string) (*LANCertificate, error) {
	issuer := a.lan.Load()
	if issuer == nil {
		return nil, errors.New("the CA has no LAN intermediate")
	}
	if len(names) == 0 {
		return nil, errors.New("a certificate needs a name")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: newSerial(),
		Subject:      pkix.Name{CommonName: names[0], Organization: []string{"PocketCloud"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     earliest(now.Add(LANCertTTL), issuer.cert.NotAfter),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, name := range names {
		name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
		if name == "" {
			return nil, errors.New("a certificate name is empty")
		}
		if ip := net.ParseIP(name); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
			continue
		}
		template.DNSNames = append(template.DNSNames, name)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer.cert, key.Public(), issuer.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return &LANCertificate{
		ChainPEM:  append(encodeCertPEM(leaf), issuer.pem...),
		KeyPEM:    pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		NotBefore: leaf.NotBefore,
		NotAfter:  leaf.NotAfter,
	}, nil
}

// Root describes the root certificate for people who install it.
type Root struct {
	Subject  string    `json:"subject"`
	NotAfter time.Time `json:"not_after"`
	// SHA256 is the fingerprint as browsers show it, in pairs of hex digits.
	SHA256 string `json:"sha256"`
}

// RootInfo describes the root certificate.
func (a *Authority) RootInfo() Root {
	sum := sha256.Sum256(a.root.Raw)
	pairs := make([]string, len(sum))
	for i, b := range sum {
		pairs[i] = fmt.Sprintf("%02X", b)
	}
	return Root{Subject: a.root.Subject.CommonName, NotAfter: a.root.NotAfter, SHA256: strings.Join(pairs, ":")}
}

// RootDER is the root certificate in DER, as some devices import it.
func (a *Authority) RootDER() []byte {
	return a.root.Raw
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// RootPEM returns the root certificate, safe to publish.
func (a *Authority) RootPEM() []byte {
	return a.rootPEM
}

// Pin returns the CA_PIN agents use for their first connection.
func (a *Authority) Pin() string {
	return pki.PinOf(a.root.Raw)
}

// ServerCertificate is the gateway's server certificate, which names the
// panel's public host and the agents' endpoint: the panel's HTTPS serves it
// too until it has one from ACME (#63).
func (a *Authority) ServerCertificate() *tls.Certificate { return a.server.Load() }

// ServerTLSConfig returns the gateway's TLS configuration. Client certificates
// are requested but not verified here: the gateway's interceptor does that, so a
// rejected agent gets a clear gRPC error instead of a broken handshake, and only
// Enroll may be called without one.
func (a *Authority) ServerTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequestClientCert,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return a.server.Load(), nil
		},
	}
}

// Issued is a newly signed agent certificate.
type Issued struct {
	// ChainPEM holds the client certificate followed by the agent intermediate.
	ChainPEM  []byte
	Serial    string
	NotBefore time.Time
	NotAfter  time.Time
}

// IssueAgentCert signs a CSR for a host. The agent proves that it holds the key;
// every name in the CSR is ignored, the identity comes only from hostID.
func (a *Authority) IssueAgentCert(csrPEM []byte, hostID string) (*Issued, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("the CSR is not PEM encoded")
	}

	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("invalid CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("invalid CSR signature: %w", err)
	}

	identity, err := url.Parse(pki.HostURIPrefix + hostID)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	serial := newSerial()

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostID, Organization: []string{"PocketCloud agents"}},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(a.AgentTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{identity},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, a.intermediate, csr.PublicKey, a.intermediateKey)
	if err != nil {
		return nil, err
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}

	return &Issued{
		ChainPEM:  append(encodeCertPEM(leaf), a.intermediatePEM...),
		Serial:    pki.SerialString(leaf),
		NotBefore: leaf.NotBefore,
		NotAfter:  leaf.NotAfter,
	}, nil
}

// ErrExpired is returned for an expired agent certificate.
var ErrExpired = errors.New("the agent certificate has expired")

// VerifyAgent checks a presented chain and returns the host id and the serial.
func (a *Authority) VerifyAgent(chain []*x509.Certificate) (hostID, serial string, err error) {
	if len(chain) == 0 {
		return "", "", errors.New("no client certificate")
	}

	leaf := chain[0]

	intermediates := x509.NewCertPool()
	intermediates.AddCert(a.intermediate)
	for _, cert := range chain[1:] {
		intermediates.AddCert(cert)
	}

	_, err = leaf.Verify(x509.VerifyOptions{
		Roots:         a.roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		var invalid x509.CertificateInvalidError
		if errors.As(err, &invalid) && invalid.Reason == x509.Expired {
			return "", pki.SerialString(leaf), ErrExpired
		}
		return "", pki.SerialString(leaf), fmt.Errorf("the certificate was not issued by this panel: %w", err)
	}

	hostID = pki.HostIDOf(leaf)
	if hostID == "" {
		return "", pki.SerialString(leaf), errors.New("the certificate does not name a host")
	}

	return hostID, pki.SerialString(leaf), nil
}

func (a *Authority) sealKey(key *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", err
	}
	return a.box.Seal(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

func (a *Authority) openKey(sealed string) (*ecdsa.PrivateKey, error) {
	keyPEM, err := a.box.Open(sealed)
	if err != nil {
		return nil, err
	}

	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("malformed key")
	}

	return x509.ParseECPrivateKey(block.Bytes)
}

func parseCertPEM(value string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(value))
	if block == nil {
		return nil, errors.New("malformed certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func encodeCertPEM(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

func newSerial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return serial
}

func normalizeNames(names []string) []string {
	result := []string{"localhost", "127.0.0.1"}
	for _, name := range names {
		if name != "" && !slices.Contains(result, name) {
			result = append(result, name)
		}
	}
	slices.Sort(result)
	return result
}
