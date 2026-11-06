package panel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/ca"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// TestTheCheckOfALANSiteTrustsTheInternalRoot: the availability check of a
// new site on a LAN name verifies the edge's certificate against the internal
// CA's root, and a site checked against the system's roots fails with it.
func TestTheCheckOfALANSiteTrustsTheInternalRoot(t *testing.T) {
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)
	box, err := secrets.New(testutil.MasterKey)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := ca.Open(app, box, nil)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := authority.IssueLANCert([]string{"wiki.lan"})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(issued.ChainPEM, issued.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("wiki"))
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(authority.RootPEM())
	answer, err := askSite(ctx, "https://wiki.lan:"+port+"/", "127.0.0.1", roots)
	if err != nil || answer.Status != http.StatusOK || answer.TLS == "" {
		t.Fatalf("with the internal root: %+v %v", answer, err)
	}
	if _, err := askSite(ctx, "https://wiki.lan:"+port+"/", "127.0.0.1", nil); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("with the system's roots: %v", err)
	}
}
