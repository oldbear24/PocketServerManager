package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/pki"
)

const (
	identityDir  = "identity"
	keyFile      = "key.pem"
	certFile     = "cert.pem"
	caFile       = "ca.pem"
	identityMeta = "identity.json"

	renewBefore        = 30 * 24 * time.Hour
	notBeforeAllowance = 5 * time.Minute
)

// ErrNotEnrolled means that the volume holds no identity yet.
var ErrNotEnrolled = errors.New("this agent has not been enrolled yet")

// Identity is who this agent is: its key, certificate and the panel it belongs
// to. It lives in the agent volume, so it survives recreating the container.
type Identity struct {
	Panel  string   `json:"panel"`
	HostID string   `json:"host_id"`
	Name   string   `json:"name"`
	Roles  []string `json:"roles"`

	dir   string
	roots *x509.CertPool
	cert  atomic.Pointer[tls.Certificate]
}

// LoadIdentity reads an identity from the state directory.
func LoadIdentity(stateDir string) (*Identity, error) {
	dir := filepath.Join(stateDir, identityDir)

	raw, err := os.ReadFile(filepath.Join(dir, identityMeta))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotEnrolled
		}
		return nil, err
	}

	identity := &Identity{dir: dir}
	if err := json.Unmarshal(raw, identity); err != nil {
		return nil, fmt.Errorf("malformed identity: %w", err)
	}

	caPEM, err := os.ReadFile(filepath.Join(dir, caFile))
	if err != nil {
		return nil, err
	}
	identity.roots = x509.NewCertPool()
	if !identity.roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("malformed CA certificate in the identity")
	}

	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, certFile), filepath.Join(dir, keyFile))
	if err != nil {
		return nil, err
	}
	identity.cert.Store(&cert)

	return identity, nil
}

// Certificate returns the parsed client certificate.
func (i *Identity) Certificate() *x509.Certificate {
	cert := i.cert.Load()
	if cert == nil || len(cert.Certificate) == 0 {
		return nil
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil
	}
	return leaf
}

// NeedsRenewal tells whether the certificate is close to its expiry.
func (i *Identity) NeedsRenewal() bool {
	leaf := i.Certificate()
	if leaf == nil {
		return false
	}
	// short-lived certificates renew after two thirds of their life
	return time.Until(leaf.NotAfter) < min(renewBefore, certificateLife(leaf)/3)
}

// certificateLife is the validity of a certificate, without the panel's allowance
// for clock differences at its start.
func certificateLife(leaf *x509.Certificate) time.Duration {
	life := leaf.NotAfter.Sub(leaf.NotBefore)
	if life > notBeforeAllowance {
		return life - notBeforeAllowance
	}
	return life
}

// TLSConfig returns the client configuration for the panel's gateway. The panel
// is identified by its name under the pinned root, not by a host name; see pki.
func (i *Identity) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, //nolint:gosec // VerifyConnection does the verification
		VerifyConnection: func(state tls.ConnectionState) error {
			return pki.VerifyPanel(state, i.roots)
		},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return i.cert.Load(), nil
		},
	}
}

// Renew rotates the key and the certificate. The new pair is on disk before it
// is used, so a crash at any point leaves a working identity behind.
func (i *Identity) Renew(ctx context.Context, client agentv1.AgentServiceClient) error {
	keyPEM, csrPEM, err := pki.NewKeyAndCSR(i.HostID)
	if err != nil {
		return err
	}

	response, err := client.RenewCertificate(ctx, &agentv1.RenewCertificateRequest{CsrPem: csrPEM})
	if err != nil {
		return err
	}

	cert, err := tls.X509KeyPair(response.GetCertificatePem(), keyPEM)
	if err != nil {
		return fmt.Errorf("the panel returned an invalid certificate: %w", err)
	}

	if err := writeKeyPair(i.dir, keyPEM, response.GetCertificatePem()); err != nil {
		return err
	}

	i.cert.Store(&cert)

	return nil
}

// Enroll exchanges the token for an identity. The panel's certificate is checked
// against CA_PIN, so the first connection cannot be intercepted.
func Enroll(ctx context.Context, config Config, facts *agentv1.HostFacts) (*Identity, error) {
	if config.Panel == "" || config.CAPin == "" || config.Token == "" {
		return nil, errors.New("enrolling needs PANEL, CA_PIN and TOKEN")
	}

	keyPEM, csrPEM, err := pki.NewKeyAndCSR(config.Name)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, //nolint:gosec // VerifyConnection does the verification
		VerifyConnection: func(state tls.ConnectionState) error {
			root, err := pki.RootFromPin(state, config.CAPin)
			if err != nil {
				return err
			}
			roots := x509.NewCertPool()
			roots.AddCert(root)
			return pki.VerifyPanel(state, roots)
		},
	}

	conn, err := grpc.NewClient(config.Panel, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	response, err := agentv1.NewAgentServiceClient(conn).Enroll(ctx, &agentv1.EnrollRequest{
		Token:  config.Token,
		CsrPem: csrPEM,
		Name:   config.Name,
		Roles:  config.Roles,
		Facts:  facts,
	})
	if err != nil {
		return nil, err
	}

	identity := &Identity{
		Panel:  config.Panel,
		HostID: response.GetHostId(),
		Name:   response.GetName(),
		Roles:  response.GetRoles(),
		dir:    filepath.Join(config.StateDir, identityDir),
	}

	if err := identity.save(keyPEM, response.GetCertificatePem(), response.GetCaPem()); err != nil {
		return nil, fmt.Errorf("failed to store the identity: %w", err)
	}

	// load it back, so that everything is checked the way a later start would
	return LoadIdentity(config.StateDir)
}

func (i *Identity) save(keyPEM, certPEM, caPEM []byte) error {
	if err := os.MkdirAll(i.dir, 0o700); err != nil {
		return err
	}

	if block, _ := pem.Decode(caPEM); block == nil {
		return errors.New("the panel returned an invalid CA certificate")
	}

	if err := writeAtomic(filepath.Join(i.dir, caFile), caPEM, 0o644); err != nil {
		return err
	}
	if err := writeKeyPair(i.dir, keyPEM, certPEM); err != nil {
		return err
	}

	raw, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return err
	}

	// written last: an identity without this file does not exist
	return writeAtomic(filepath.Join(i.dir, identityMeta), raw, 0o644)
}

func writeKeyPair(dir string, keyPEM, certPEM []byte) error {
	if err := writeAtomic(filepath.Join(dir, keyFile), keyPEM, 0o600); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, certFile), certPEM, 0o644)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, mode); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
