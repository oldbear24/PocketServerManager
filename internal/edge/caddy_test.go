package edge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/version"
)

// TestGoldenConfigsLoadInCaddy checks the golden configs with the pinned edge
// image itself: `caddy validate` provisions every module, so an unknown field,
// a handler Caddy does not have or a certificate it cannot load fails here. It
// needs Docker and runs when POCKETCLOUD_CADDY_TEST is set, as in CI.
func TestGoldenConfigsLoadInCaddy(t *testing.T) {
	if os.Getenv("POCKETCLOUD_CADDY_TEST") == "" {
		t.Skip("set POCKETCLOUD_CADDY_TEST=1 to validate the golden configs with the edge image in Docker")
	}

	certs := t.TempDir()
	for _, config := range golden {
		for _, cert := range config.Certificates {
			writeSelfSigned(t, certs, cert.ID)
		}
	}

	// the golden configs as the edge loads them: with the secrets they refer to
	testdata := t.TempDir()
	for name, config := range golden {
		built, err := Build(config)
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := ResolveSecrets(string(built), Secrets(config))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(testdata, name+".json"), []byte(resolved), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for name := range golden {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("docker", "run", "--rm", "--network", "none",
				"-v", certs+":"+CertDir+":ro",
				"-v", testdata+":/golden:ro",
				version.EdgeRef(),
				"caddy", "validate", "--config", "/golden/"+name+".json")
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("caddy validate: %v\n%s", err, output)
			}
		})
	}
}

// writeSelfSigned writes <dir>/<id>.crt and .key, readable by the container.
func writeSelfSigned(t *testing.T, dir, id string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: id + ".example.com"},
		DNSNames:     []string{id + ".example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(filepath.Join(dir, id+".crt"), certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".key"), keyPEM, 0o644); err != nil {
		t.Fatal(err)
	}
}
