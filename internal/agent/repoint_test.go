package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/pki"
)

// The repoint of an agent to a panel at another address (#32), against
// gateways that check the agent's certificate as the panel's does.

// repointPanel is a gateway that holds a CA, checks an agent's certificate
// under it, and knows some hosts.
type repointPanel struct {
	agentv1.UnimplementedAgentServiceServer
	roots *x509.CertPool
	known map[string]bool
	mu    sync.Mutex
	beats []string
	kept  int
}

// Heartbeat answers a known host, as the panel's authentication lets it
// through, and refuses others as it does. It counts the calls that were not
// probes, which a panel would keep as the server's return.
func (p *repointPanel) Heartbeat(ctx context.Context, req *agentv1.HeartbeatRequest) (*agentv1.HeartbeatResponse, error) {
	from, _ := peer.FromContext(ctx)
	info, ok := from.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return nil, status.Error(codes.Unauthenticated, "a client certificate is required; only Enroll works without one")
	}
	leaf := info.State.PeerCertificates[0]
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: p.roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid agent certificate: %v", err)
	}
	host := pki.HostIDOf(leaf)
	p.mu.Lock()
	p.beats = append(p.beats, host)
	if !req.GetProbe() {
		p.kept++
	}
	p.mu.Unlock()
	if !p.known[host] {
		return nil, status.Error(codes.PermissionDenied, "this server was removed from the panel: enroll it again")
	}
	return &agentv1.HeartbeatResponse{}, nil
}

func (p *repointPanel) heartbeats() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.beats...)
}

// pin is the root's CA_PIN.
func (c *testCA) pin() string { return pki.PinOf(c.root.Raw) }

// panelCert is a gateway's certificate under the root.
func (c *testCA) panelCert(t *testing.T) tls.Certificate {
	t.Helper()
	certPEM, keyPEM := c.issue(t, pki.PanelURI, x509.ExtKeyUsageServerAuth)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// serveRepoint starts a gateway of ca on a free port, which knows the hosts.
func serveRepoint(t *testing.T, ca *testCA, known ...string) (*repointPanel, string) {
	t.Helper()
	panel := &repointPanel{roots: x509.NewCertPool(), known: map[string]bool{}}
	panel.roots.AddCert(ca.root)
	for _, host := range known {
		panel.known[host] = true
	}
	cert := ca.panelCert(t)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequestClientCert,
		Certificates: []tls.Certificate{cert}})))
	agentv1.RegisterAgentServiceServer(server, panel)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return panel, listener.Addr().String()
}

// enrolledState is an agent's state directory with the identity of host h1,
// named vm, under ca, enrolled at old; identity.json holds a field of a
// later agent too, which a repoint keeps.
func enrolledState(t *testing.T, ca *testCA, old string) string {
	t.Helper()
	state := t.TempDir()
	dir := filepath.Join(state, identityDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := ca.issue(t, pki.HostURIPrefix+"h1", x509.ExtKeyUsageClientAuth)
	identity := &Identity{Panel: old, HostID: "h1", Name: "vm", Roles: []string{"docker"}, dir: dir}
	if err := identity.save(keyPEM, certPEM, ca.rootPEM()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, identityMeta))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["later"] = map[string]any{"kept": true}
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, identityMeta), raw, 0o644); err != nil { //nolint:gosec // as enrollment writes it
		t.Fatal(err)
	}
	return state
}

// identityFiles are the files of an identity, by name.
func identityFiles(t *testing.T, state string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, name := range []string{keyFile, certFile, caFile, identityMeta} {
		raw, err := os.ReadFile(filepath.Join(state, identityDir, name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = string(raw)
	}
	return files
}

// TestRepointRewritesOnlyThePanel (#32): a panel that holds this server's CA
// and knows it gets one heartbeat, a probe that it keeps nothing of; then
// identity.json names the new address and keeps every other field, and the
// key and the certificates stay as they are. --check changes nothing.
func TestRepointRewritesOnlyThePanel(t *testing.T) {
	ca := newTestCA(t, "PocketCloud panel root")
	panel, address := serveRepoint(t, ca, "h1")
	state := enrolledState(t, ca, "192.0.2.2:9443")
	before := identityFiles(t, state)

	var out bytes.Buffer
	if err := Repoint(context.Background(), Config{StateDir: state}, []string{"--panel", address, "--check"}, &out); err != nil {
		t.Fatal(err)
	}
	found := "The panel at " + address + " holds this server's CA (" + ca.pin() + ") and knows it as vm."
	if out.String() != found+" Nothing was changed (--check).\n" {
		t.Errorf("--check: %q", out.String())
	}
	if after := identityFiles(t, state); after[identityMeta] != before[identityMeta] {
		t.Errorf("--check changed the identity: %s", after[identityMeta])
	}

	out.Reset()
	if err := Repoint(context.Background(), Config{StateDir: state}, []string{"--panel", address}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != found+" This server now dials "+address+": restart the agent (docker restart pocketcloud-agent).\n" {
		t.Errorf("the repoint: %q", out.String())
	}
	after := identityFiles(t, state)
	for _, name := range []string{keyFile, certFile, caFile} {
		if after[name] != before[name] {
			t.Errorf("%s changed", name)
		}
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(after[identityMeta]), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["panel"] != address || fields["host_id"] != "h1" || fields["name"] != "vm" || fields["later"] == nil || len(fields) != 5 {
		t.Errorf("identity.json: %v", fields)
	}
	if info, _ := os.Stat(filepath.Join(state, identityDir, identityMeta)); info.Mode().Perm() != 0o644 {
		t.Errorf("identity.json's mode: %v", info.Mode())
	}
	identity, err := LoadIdentity(state)
	if err != nil || identity.Panel != address {
		t.Errorf("the identity: %+v %v", identity, err)
	}
	if beats := panel.heartbeats(); len(beats) != 2 || beats[0] != "h1" {
		t.Errorf("heartbeats: %v", beats)
	}
	panel.mu.Lock()
	defer panel.mu.Unlock()
	if panel.kept != 0 {
		t.Errorf("%d heartbeats were no probes", panel.kept)
	}
}

// TestRepointRefusesAPanelOfAnotherCA (#32): a panel whose certificate is
// not under the CA this server pinned, such as one that made a CA anew, is
// refused before any call, and nothing is changed; so is a panel that does
// not know this server, and one that cannot be reached.
func TestRepointRefusesAPanelOfAnotherCA(t *testing.T) {
	ca := newTestCA(t, "PocketCloud panel root")
	state := enrolledState(t, ca, "192.0.2.2:9443")
	before := identityFiles(t, state)

	other, otherAddress := serveRepoint(t, newTestCA(t, "PocketCloud panel root"), "h1")
	err := Repoint(context.Background(), Config{StateDir: state}, []string{"--panel", otherAddress}, &bytes.Buffer{})
	if err == nil || err.Error() != "The panel at "+otherAddress+" does not hold this server's CA: nothing was changed." {
		t.Errorf("another CA: %v", err)
	}
	if beats := other.heartbeats(); len(beats) != 0 {
		t.Errorf("the other panel got %v", beats)
	}

	stranger, strangerAddress := serveRepoint(t, ca)
	err = Repoint(context.Background(), Config{StateDir: state}, []string{"--panel", strangerAddress}, &bytes.Buffer{})
	if err == nil || err.Error() != "The panel at "+strangerAddress+" does not know this server: nothing was changed. The panel said: this server was removed from the panel: enroll it again" {
		t.Errorf("a panel that does not know the server: %v", err)
	}
	if beats := stranger.heartbeats(); len(beats) != 1 {
		t.Errorf("heartbeats: %v", beats)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := listener.Addr().String()
	_ = listener.Close()
	if err := Repoint(context.Background(), Config{StateDir: state}, []string{"--panel", closed}, &bytes.Buffer{}); err == nil ||
		!strings.HasPrefix(err.Error(), "The panel at "+closed+" cannot be reached: ") || !strings.HasSuffix(err.Error(), ". Nothing was changed.") {
		t.Errorf("no panel: %v", err)
	}
	if after := identityFiles(t, state); after[identityMeta] != before[identityMeta] {
		t.Errorf("the identity changed: %s", after[identityMeta])
	}

	for _, bad := range [][]string{{}, {"--panel", "panel.example.com"}, {"--panel", ":9443"}, {"--panel", "panel:0"}, {"--panel", "panel:https"},
		{"--panel", "panel:70000"}} {
		if err := Repoint(context.Background(), Config{StateDir: state}, bad, &bytes.Buffer{}); err == nil ||
			!strings.HasPrefix(err.Error(), "--panel is the panel's gateway as HOST:PORT, such as panel.example.com:9443, not ") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if err := Repoint(context.Background(), Config{StateDir: state}, []string{"--panel", "panel:9443", "again"}, &bytes.Buffer{}); err == nil ||
		err.Error() != `repoint takes flags only, not "again"` {
		t.Errorf("an argument: %v", err)
	}
	if err := Repoint(context.Background(), Config{StateDir: t.TempDir()}, []string{"--panel", "panel:9443"}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "has not enrolled") {
		t.Errorf("an agent that has not enrolled: %v", err)
	}
}
