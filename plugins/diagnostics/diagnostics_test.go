package diagnostics

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

const secret = "TOP-SECRET-VALUE"

// sensitiveMessages holds real messages with every kind of sensitive field.
var sensitiveMessages = map[string]proto.Message{
	"enroll token":   &agentv1.EnrollRequest{Token: secret},
	"terminal input": &agentv1.ExecResponse{Input: []byte(secret)},
	"relay data":     &agentv1.RelayRequest{Data: []byte(secret)},
	"stack secrets": &agentv1.Command{Payload: &agentv1.Command_StackDeploy{StackDeploy: &agentv1.StackDeploy{
		Name:       "demo",
		Secrets:    map[string]string{"DB_PASSWORD": secret},
		Registries: []*agentv1.RegistryAuth{{Server: "ghcr.io", Username: "me", Password: secret}},
	}}},
	"database passwords": &agentv1.Command{Payload: &agentv1.Command_DbDatabaseCreate{DbDatabaseCreate: &agentv1.DbDatabaseCreate{
		Connection: &agentv1.DbConnection{AdminUser: "root", AdminPassword: secret},
		Database:   "shop",
		Password:   secret,
	}}},
	"certificate key": &agentv1.Command{Payload: &agentv1.Command_CertInstall{CertInstall: &agentv1.CertInstall{KeyPem: []byte(secret)}}},
	"backup repository": &agentv1.Command{Payload: &agentv1.Command_BackupRun{BackupRun: &agentv1.BackupRun{
		Repository: &agentv1.BackupRepository{
			Url:      "s3:bucket",
			Password: secret,
			Env:      map[string]string{"AWS_SECRET_ACCESS_KEY": secret},
		},
		Env: map[string]string{"PGPASSWORD": secret},
	}}},
	"inside a stream message": &agentv1.SubscribeCommandsResponse{Event: &agentv1.SubscribeCommandsResponse_Command{Command: &agentv1.Command{
		Payload: &agentv1.Command_ImagePull{ImagePull: &agentv1.ImagePull{Image: "x", Auth: &agentv1.RegistryAuth{Password: secret}}},
	}}},
}

func TestSensitiveFieldsAreMasked(t *testing.T) {
	for name, message := range sensitiveMessages {
		t.Run(name, func(t *testing.T) {
			original := proto.Clone(message)

			masked := MaskedJSON(message)
			if masked == "" {
				t.Fatal("no output")
			}
			if strings.Contains(masked, secret) {
				t.Fatalf("the secret is in the output: %s", masked)
			}

			// the bytes form of a secret must not get through either
			if strings.Contains(masked, "VE9QLVNFQ1JFVC1WQUxVRQ") {
				t.Fatalf("the base64 of the secret is in the output: %s", masked)
			}

			// the message itself keeps working: it is about to be sent
			if !proto.Equal(original, message) {
				t.Fatal("masking changed the original message")
			}
		})
	}
}

func TestMaskKeepsWhatIsNotSensitive(t *testing.T) {
	masked := MaskedJSON(&agentv1.Command{Payload: &agentv1.Command_StackDeploy{StackDeploy: &agentv1.StackDeploy{
		Name:    "demo",
		Secrets: map[string]string{"DB_PASSWORD": secret},
	}}})

	for _, want := range []string{"demo", "DB_PASSWORD", maskedValue} {
		if !strings.Contains(masked, want) {
			t.Errorf("%q is missing from %s", want, masked)
		}
	}
}

func TestDisconnectReasons(t *testing.T) {
	for _, tc := range []struct {
		name     string
		message  string
		silence  time.Duration
		shutdown bool
		want     string
	}{
		{"clean close", "context canceled", 0, false, ReasonEOF},
		{"network cut", "context canceled", 15 * time.Second, false, ReasonKeepaliveTimeout},
		{"keepalive named", "keepalive ping not acked within timeout", 0, false, ReasonKeepaliveTimeout},
		{"revoked", "the agent certificate has been revoked: enroll this server again", 0, false, ReasonRevoked},
		{"expired", "the agent certificate has expired: enroll this server again", 0, false, ReasonExpired},
		{"tls", "tls: bad certificate", 0, false, ReasonTLSError},
		{"handshake", "connection error: authentication handshake failed", 0, false, ReasonTLSError},
		{"panel stopping", "context canceled", 0, true, ReasonShutdown},
		{"replaced", "replaced by a newer connection of this agent", 0, false, ReasonReplaced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyDisconnect(tc.message, tc.silence, tc.shutdown); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// newPlugin returns a plugin that identifies agents by a fixed value in the context.
func newPlugin(t testing.TB, config Config) (*Plugin, http.Handler) {
	t.Helper()

	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)

	config.AgentOf = func(context.Context) string { return "agent-1" }

	plugin := MustRegister(app, config)

	return plugin, testutil.Handler(t, app)
}

type streamStub struct {
	grpc.ServerStream
	ctx      context.Context
	incoming []proto.Message
}

func (s *streamStub) Context() context.Context { return s.ctx }
func (s *streamStub) SendMsg(any) error        { return nil }
func (s *streamStub) RecvMsg(message any) error {
	proto.Merge(message.(proto.Message), s.incoming[0])
	s.incoming = s.incoming[1:]
	return nil
}

func TestCaptureNeverStoresASecret(t *testing.T) {
	plugin, _ := newPlugin(t, Config{Enabled: true})

	plugin.StartCapture("agent-1")

	interceptor := plugin.UnaryInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/pocketcloud.agent.v1.AgentService/Enroll"}

	for _, message := range sensitiveMessages {
		_, err := interceptor(context.Background(), message, info, func(_ context.Context, req any) (any, error) {
			return req, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	detail, ok := plugin.Agent("agent-1")
	if !ok {
		t.Fatal("the agent is unknown")
	}

	captured := 0
	for _, entry := range detail.Trace {
		if entry.Payload != "" {
			captured++
		}
		if strings.Contains(entry.Payload, secret) {
			t.Fatalf("a secret reached the trace: %s", entry.Payload)
		}
	}
	if captured == 0 {
		t.Fatal("the capture stored nothing")
	}
}

func TestNoPayloadsWithoutACapture(t *testing.T) {
	plugin, _ := newPlugin(t, Config{Enabled: true})

	interceptor := plugin.UnaryInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/x/Heartbeat"}

	_, err := interceptor(context.Background(), &agentv1.HeartbeatRequest{}, info, func(_ context.Context, req any) (any, error) {
		return req, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	detail, _ := plugin.Agent("agent-1")
	if len(detail.Trace) == 0 {
		t.Fatal("no trace entries")
	}
	for _, entry := range detail.Trace {
		if entry.Payload != "" {
			t.Fatalf("a payload was stored without a capture: %+v", entry)
		}
	}
}

func TestTraceIsARingBuffer(t *testing.T) {
	plugin, _ := newPlugin(t, Config{Enabled: true, TraceEntries: 10})

	interceptor := plugin.UnaryInterceptor()

	for range 50 {
		_, err := interceptor(context.Background(), &agentv1.HeartbeatRequest{}, &grpc.UnaryServerInfo{FullMethod: "/x/Heartbeat"}, func(context.Context, any) (any, error) {
			return nil, status.Error(codes.Internal, "boom")
		})
		if err == nil {
			t.Fatal("the error must come back")
		}
	}

	detail, _ := plugin.Agent("agent-1")
	if len(detail.Trace) != 10 {
		t.Fatalf("%d entries, want 10", len(detail.Trace))
	}
	if detail.LastError != "boom" {
		t.Errorf("last error: %q", detail.LastError)
	}
	if detail.Trace[len(detail.Trace)-1].Code != codes.Internal.String() {
		t.Errorf("code: %+v", detail.Trace[len(detail.Trace)-1])
	}
}

func TestStreamMessagesAreTraced(t *testing.T) {
	plugin, _ := newPlugin(t, Config{
		Enabled: true,
		Describe: func(message proto.Message) (string, string) {
			if _, ok := message.(*agentv1.HeartbeatRequest); ok {
				return "heartbeat", "described"
			}
			return "", ""
		},
	})

	stub := &streamStub{ctx: context.Background(), incoming: []proto.Message{&agentv1.HeartbeatRequest{}}}

	err := plugin.StreamInterceptor()(nil, stub, &grpc.StreamServerInfo{FullMethod: "/x/Stream"}, func(_ any, stream grpc.ServerStream) error {
		if err := stream.RecvMsg(&agentv1.HeartbeatRequest{}); err != nil {
			return err
		}
		return stream.SendMsg(&agentv1.HeartbeatResponse{})
	})
	if err != nil {
		t.Fatal(err)
	}

	detail, _ := plugin.Agent("agent-1")
	if len(detail.Trace) != 2 || detail.Trace[0].Kind != "heartbeat" || detail.Trace[0].Subject != "described" || detail.Trace[1].Direction != directionOut {
		t.Fatalf("%+v", detail.Trace)
	}
}

// statsEvents walks a connection through the stats handler the way gRPC does.
func connect(t *testing.T, plugin *Plugin) (context.Context, stats.Handler) {
	t.Helper()

	handler := plugin.StatsHandler()

	remote := &net.TCPAddr{IP: net.IPv4(203, 0, 113, 24), Port: 4242}

	ctx := handler.TagConn(context.Background(), &stats.ConnTagInfo{RemoteAddr: remote})
	handler.HandleConn(ctx, &stats.ConnBegin{})
	ctx = peer.NewContext(ctx, &peer.Peer{Addr: remote})
	ctx = handler.TagRPC(ctx, &stats.RPCTagInfo{FullMethodName: "/x/Heartbeat"})

	return ctx, handler
}

func TestConnectionHistory(t *testing.T) {
	seen := make(chan ConnectionEvent, 8)

	plugin, _ := newPlugin(t, Config{
		Enabled:      true,
		OnConnection: func(_ string, event ConnectionEvent) { seen <- event },
	})

	ctx, handler := connect(t, plugin)
	handler.HandleRPC(ctx, &stats.InPayload{WireLength: 100})
	handler.HandleRPC(ctx, &stats.OutPayload{WireLength: 40})

	summary, ok := plugin.Agent("agent-1")
	if !ok || !summary.Connected || summary.Reconnects != 0 {
		t.Fatalf("%+v", summary)
	}
	if summary.MessagesIn != 1 || summary.BytesIn != 100 || summary.MessagesOut != 1 || summary.BytesOut != 40 {
		t.Errorf("%+v", summary)
	}
	if summary.Remote != "203.0.113.24:4242" {
		t.Errorf("remote: %s", summary.Remote)
	}

	if event := <-seen; event.Kind != "connected" {
		t.Errorf("%+v", event)
	}

	message := "the agent certificate has been revoked: enroll this server again"
	state, _ := ctx.Value(connKey{}).(*connState)
	state.lastError.Store(&message)

	handler.HandleConn(ctx, &stats.ConnEnd{})

	if event := <-seen; event.Kind != "disconnected" || event.Reason != ReasonRevoked {
		t.Errorf("%+v", event)
	}

	summary, _ = plugin.Agent("agent-1")
	if summary.Connected || summary.LastReason != ReasonRevoked {
		t.Errorf("%+v", summary)
	}

	// it comes back
	ctx, handler = connect(t, plugin)
	handler.HandleRPC(ctx, &stats.InPayload{WireLength: 1})

	summary, _ = plugin.Agent("agent-1")
	if !summary.Connected || summary.Reconnects != 1 {
		t.Errorf("%+v", summary)
	}
	if len(summary.Events) != 3 {
		t.Errorf("events: %+v", summary.Events)
	}
}

func TestRTTPercentiles(t *testing.T) {
	plugin, _ := newPlugin(t, Config{Enabled: true})

	for i := 1; i <= 100; i++ {
		plugin.ObserveHeartbeat("agent-1", float64(i), 1.5)
	}

	detail, _ := plugin.Agent("agent-1")
	if detail.RTTp50MS != 50 || detail.RTTp95MS != 95 {
		t.Errorf("p50 %f, p95 %f", detail.RTTp50MS, detail.RTTp95MS)
	}
	if detail.ClockSkewMS != 1.5 {
		t.Errorf("skew: %f", detail.ClockSkewMS)
	}
}

func TestDisabledCollectsNothingAndHasNoRoutes(t *testing.T) {
	plugin, handler := newPlugin(t, Config{Enabled: false, MetricsToken: "t"})

	plugin.ObserveHeartbeat("agent-1", 5, 1)
	plugin.ObserveQueueEvent(QueueTimeout)
	plugin.ObserveCommandLatency("x", time.Second, time.Second)
	plugin.RecordError("boom", "")

	if agents := plugin.Agents(); len(agents) != 0 {
		t.Errorf("a disabled plugin collected %d agents", len(agents))
	}
	if errors := plugin.recentErrors(); len(errors) != 0 {
		t.Errorf("a disabled plugin collected errors: %v", errors)
	}

	_, superuserToken := testutil.NewSuperuser(t, plugin.app)

	for _, url := range []string{"/api/diag/overview", "/api/diag/agents", "/metrics"} {
		if status, _ := testutil.Do(t, handler, testutil.Request{Method: "GET", URL: url, Token: superuserToken}); status != http.StatusNotFound {
			t.Errorf("%s: status %d", url, status)
		}
	}
}

func TestAPIAccess(t *testing.T) {
	plugin, handler := newPlugin(t, Config{Enabled: true})

	_, viewerToken := testutil.NewUser(t, plugin.app, "viewer@example.com", schema.RoleViewer)
	_, superuserToken := testutil.NewSuperuser(t, plugin.app)

	plugin.ObserveHeartbeat("agent-1", 5, 1)

	if status, _ := testutil.Do(t, handler, testutil.Request{Method: "GET", URL: "/api/diag/overview"}); status != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", status)
	}
	if status, _ := testutil.Do(t, handler, testutil.Request{Method: "GET", URL: "/api/diag/overview", Token: viewerToken}); status != http.StatusForbidden {
		t.Errorf("a viewer: %d", status)
	}

	for url, want := range map[string]string{
		"/api/diag/overview":            "process",
		"/api/diag/agents":              "agent-1",
		"/api/diag/agents/agent-1":      "rtt_p50_ms",
		"/api/diag/overview?window=24h": "window_s",
	} {
		status, body := testutil.Do(t, handler, testutil.Request{Method: "GET", URL: url, Token: superuserToken})
		if status != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s: %d %s", url, status, body)
		}
	}

	if status, _ := testutil.Do(t, handler, testutil.Request{Method: "GET", URL: "/api/diag/agents/nobody", Token: superuserToken}); status != http.StatusNotFound {
		t.Errorf("unknown agent: %d", status)
	}

	status, body := testutil.Do(t, handler, testutil.Request{Method: "POST", URL: "/api/diag/agents/agent-1/capture", Token: superuserToken})
	if status != http.StatusOK || !strings.Contains(body, "until") {
		t.Errorf("capture: %d %s", status, body)
	}
}

func TestPrometheusEndpoint(t *testing.T) {
	plugin, handler := newPlugin(t, Config{
		Enabled:      true,
		MetricsToken: "scrape-token",
		QueueSnapshot: func() QueueSnapshot {
			return QueueSnapshot{Depth: map[string]int{"queued": 3}, OldestQueued: 4 * time.Second}
		},
	})

	plugin.ObserveQueueEvent(QueueReplay)

	if status, _ := testutil.Do(t, handler, testutil.Request{Method: "GET", URL: "/metrics"}); status != http.StatusUnauthorized {
		t.Errorf("no token: %d", status)
	}
	if status, _ := testutil.Do(t, handler, testutil.Request{Method: "GET", URL: "/metrics", Token: "Bearer wrong"}); status != http.StatusUnauthorized {
		t.Errorf("a wrong token: %d", status)
	}

	status, body := testutil.Do(t, handler, testutil.Request{Method: "GET", URL: "/metrics", Token: "Bearer scrape-token"})
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}

	for _, want := range []string{
		"# TYPE pocketcloud_agents_connected gauge",
		`pocketcloud_queue_depth{state="queued"} 3`,
		"pocketcloud_queue_oldest_seconds 4",
		"pocketcloud_queue_replays_total 1",
		"pocketcloud_process_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%q is missing from:\n%s", want, body)
		}
	}
}

func TestNoMetricsEndpointWithoutAToken(t *testing.T) {
	_, handler := newPlugin(t, Config{Enabled: true})

	if status, _ := testutil.Do(t, handler, testutil.Request{Method: "GET", URL: "/metrics"}); status != http.StatusNotFound {
		t.Errorf("status: %d", status)
	}
}

func TestBundleHoldsNoSecrets(t *testing.T) {
	plugin, handler := newPlugin(t, Config{
		Enabled:    true,
		ConfigDump: func() map[string]any { return map[string]any{"public_url": "https://panel.example.com"} },
	})

	_, token := testutil.NewSuperuser(t, plugin.app)

	plugin.RecordError("something broke", "details")

	status, body := testutil.Do(t, handler, testutil.Request{Method: "GET", URL: "/api/diag/bundle", Token: token})
	if status != http.StatusOK || !strings.HasPrefix(body, "PK") {
		t.Fatalf("%d %.100q", status, body)
	}
	if strings.Contains(body, secret) || strings.Contains(body, string(testutil.MasterKey)) {
		t.Fatal("a secret is in the bundle")
	}
}

func TestLiveTraceFilters(t *testing.T) {
	plugin, _ := newPlugin(t, Config{Enabled: true})

	all, stopAll := plugin.subscribe("")
	defer stopAll()
	other, stopOther := plugin.subscribe("someone-else")
	defer stopOther()

	_, err := plugin.UnaryInterceptor()(context.Background(), &agentv1.HeartbeatRequest{}, &grpc.UnaryServerInfo{FullMethod: "/x/Heartbeat"}, func(_ context.Context, req any) (any, error) {
		return req, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case entry := <-all:
		if entry.Agent != "agent-1" || entry.Method != "Heartbeat" {
			t.Errorf("%+v", entry)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing was published")
	}

	select {
	case entry := <-other:
		t.Errorf("the filter let through %+v", entry)
	default:
	}
}

func TestSlowTraceViewerDoesNotBlock(t *testing.T) {
	plugin, _ := newPlugin(t, Config{Enabled: true})

	_, stop := plugin.subscribe("")
	defer stop()

	done := make(chan struct{})
	go func() {
		for range 1000 {
			plugin.publish(TraceEntry{Agent: "agent-1"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a viewer that does not read blocked publishing")
	}
}

func TestRateCounter(t *testing.T) {
	var counter rateCounter

	now := time.Now()
	for range 120 {
		counter.add(now)
	}

	if rate := counter.perMinute(now, time.Minute); rate != 120 {
		t.Errorf("per minute: %f", rate)
	}
	if rate := counter.perMinute(now, 15*time.Minute); rate != 8 {
		t.Errorf("per minute over 15 minutes: %f", rate)
	}
}

// BenchmarkInterceptor is the per-message cost documented in docs/architecture.md.
func BenchmarkInterceptor(b *testing.B) {
	run := func(b *testing.B, enabled bool) {
		app := testutil.NewApp(b)
		b.Cleanup(app.Cleanup)

		plugin := MustRegister(app, Config{Enabled: enabled, AgentOf: func(context.Context) string { return "agent-1" }})

		interceptor := plugin.UnaryInterceptor()
		info := &grpc.UnaryServerInfo{FullMethod: "/pocketcloud.agent.v1.AgentService/Heartbeat"}
		message := &agentv1.HeartbeatRequest{Facts: &agentv1.HostFacts{Hostname: "bench", AgentVersion: "1.0"}}
		handler := func(_ context.Context, req any) (any, error) { return req, nil }

		for b.Loop() {
			if _, err := interceptor(context.Background(), message, info, handler); err != nil {
				b.Fatal(err)
			}
		}
	}

	b.Run("enabled", func(b *testing.B) { run(b, true) })
}
