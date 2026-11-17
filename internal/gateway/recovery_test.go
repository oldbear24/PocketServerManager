package gateway

import (
	"context"
	"crypto/tls"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/ca"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/inventory"
	"github.com/oldbear24/PocketServerManager/internal/pki"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// startedGateway is a gateway of a test app that listens on a free port, with
// its CA and a server that may connect.
type startedGateway struct {
	app       *tests.TestApp
	server    *Server
	authority *ca.Authority
	host      *core.Record
}

func startGateway(t *testing.T, prepare func(*Server)) *startedGateway {
	t.Helper()
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
	writer := audit.New(app, schema.Audited)
	writer.Register()
	host := testutil.NewHost(t, app, "vm")

	server := &Server{App: app, Authority: authority, Commands: commands.NewManager(app), Audit: writer, Inventory: &inventory.Store{App: app}}
	if prepare != nil {
		prepare(server)
	}
	if err := server.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Stop)
	return &startedGateway{app: app, server: server, authority: authority, host: host}
}

// subscribe connects as the gateway's server, as its agent does: a client
// certificate, the panel verified by its CA and not by a name, and the
// address the agent dials as the gRPC target. It returns once the panel
// answered the stream's headers, when the host is marked connected.
func (g *startedGateway) subscribe(t *testing.T, options ...grpc.DialOption) {
	t.Helper()
	keyPEM, csrPEM, err := pki.NewKeyAndCSR(g.host.Id)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := g.authority.IssueAgentCert(csrPEM, g.host.Id)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(issued.ChainPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	config := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, //nolint:gosec // a test of the panel's side, as the agent verifies by the pinned CA
		Certificates:       []tls.Certificate{cert},
	}
	conn, err := grpc.NewClient(g.server.Addr(), append([]grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(config))}, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := agentv1.NewAgentServiceClient(conn).SubscribeCommands(ctx, &agentv1.SubscribeCommandsRequest{AgentVersion: "0.32.0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Header(); err != nil {
		t.Fatal(err)
	}
}

func (g *startedGateway) panelAddress(t *testing.T) string {
	t.Helper()
	host, err := g.app.FindRecordById(schema.Hosts, g.host.Id)
	if err != nil {
		t.Fatal(err)
	}
	return host.GetString("panel_address")
}

// TestThePanelAddressComesFromTheAuthority (#32): the address a server's
// agent dials is what its stream's :authority says, the host:port of its
// identity, whatever the agent's version; it is no change anyone made, so it
// is not audited.
func TestThePanelAddressComesFromTheAuthority(t *testing.T) {
	g := startGateway(t, nil)

	g.subscribe(t)
	if got := g.panelAddress(t); got != g.server.Addr() {
		t.Fatalf("the panel address is %q, the agent dialled %q", got, g.server.Addr())
	}

	// an agent that dials the panel by a name says that name
	g.subscribe(t, grpc.WithAuthority("panel.example.com:9443"))
	if got := g.panelAddress(t); got != "panel.example.com:9443" {
		t.Fatalf("the panel address is %q, the agent dialled panel.example.com:9443", got)
	}

	entries, err := g.app.FindRecordsByFilter(schema.AuditLog, "collection = 'hosts'", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.GetString("changes"), "panel_address") || strings.Contains(entry.GetString("snapshot"), "panel_address") {
			t.Fatalf("the panel address was audited: %s", entry.GetString("changes"))
		}
	}
}

// TestAnAuthorityThatIsNoAddressIsLeftOut: the panel stores only what may be
// an address.
func TestAnAuthorityThatIsNoAddressIsLeftOut(t *testing.T) {
	for _, value := range []string{"", "a b:9443", "panel\x00:9443", strings.Repeat("a", 301)} {
		if got := panelAddress(withAuthority(value)); got != "" {
			t.Errorf("%q gave %q", value, got)
		}
	}
	if got := panelAddress(context.Background()); got != "" {
		t.Errorf("no metadata gave %q", got)
	}
	if got := panelAddress(withAuthority("[2001:db8::1]:9443")); got != "[2001:db8::1]:9443" {
		t.Errorf("an IPv6 address gave %q", got)
	}
}

func withAuthority(value string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.MD{":authority": []string{value}})
}

// TestOfflineAlertsWaitForTheStartGrace (#32): right after the gateway
// started, a host whose last heartbeat is old is one whose agent is still
// backing off: it is marked offline, with its alert, only once the grace
// passed. Without a grace it is marked at once.
func TestOfflineAlertsWaitForTheStartGrace(t *testing.T) {
	const grace = 400 * time.Millisecond
	g := startGateway(t, func(s *Server) {
		s.OfflineAfter = time.Millisecond
		s.MonitorInterval = time.Hour
		s.StartGrace = grace
	})
	g.host.Set("status", schema.HostOnline)
	g.host.Set("last_seen", types.NowDateTime().Add(-time.Hour))
	if err := g.app.Save(g.host); err != nil {
		t.Fatal(err)
	}

	alerts := func() int {
		open, err := g.app.FindRecordsByFilter(schema.Alerts, "rule = 'agent_offline' && target = {:host}", "", 0, 0, dbx.Params{"host": g.host.Id})
		if err != nil {
			t.Fatal(err)
		}
		return len(open)
	}
	status := func() string {
		host, err := g.app.FindRecordById(schema.Hosts, g.host.Id)
		if err != nil {
			t.Fatal(err)
		}
		return host.GetString("status")
	}

	g.server.CheckOffline()
	if status() != schema.HostOnline || alerts() != 0 {
		t.Fatalf("within the grace the host is %s with %d alerts", status(), alerts())
	}

	time.Sleep(time.Until(g.server.started.Add(grace + 50*time.Millisecond)))
	g.server.CheckOffline()
	if status() != schema.HostOffline || alerts() != 1 {
		t.Fatalf("after the grace the host is %s with %d alerts", status(), alerts())
	}

	// no grace: at once
	h := startGateway(t, func(s *Server) { s.OfflineAfter = time.Millisecond; s.MonitorInterval = time.Hour })
	h.host.Set("status", schema.HostOnline)
	h.host.Set("last_seen", types.NowDateTime().Add(-time.Hour))
	if err := h.app.Save(h.host); err != nil {
		t.Fatal(err)
	}
	h.server.CheckOffline()
	if host, _ := h.app.FindRecordById(schema.Hosts, h.host.Id); host.GetString("status") != schema.HostOffline {
		t.Fatal("without a grace the silent host stayed online")
	}
}
