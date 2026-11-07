// Package gateway is the gRPC server agents dial. It authenticates them with
// their client certificates and connects them to the panel's components.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/ca"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/inventory"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

const (
	gracefulStopTimeout = 5 * time.Second
	enrollPerMinute     = 10
	rejectEventInterval = time.Minute
	maxMessageSize      = 8 << 20
)

// Server implements the AgentService.
type Server struct {
	agentv1.UnimplementedAgentServiceServer

	App       core.App
	Authority *ca.Authority
	Commands  *commands.Manager
	Audit     *audit.Writer
	Inventory *inventory.Store
	Log       *slog.Logger

	// Metrics stores the batches of PushMetrics. Nil drops them.
	Metrics MetricsSink
	// Streams relays logs and terminal sessions to the browsers. Nil refuses them.
	Streams StreamBroker
	// Artifacts takes uploads by their kind; diagnostics bundles are built in.
	Artifacts map[string]ArtifactHandler
	// Fetch opens a file an agent downloads with FetchArtifact. Nil refuses
	// every download.
	Fetch FetchHandler

	// ExtraOptions adds server options, such as the diagnostics plugin's stats
	// handler. UnaryInterceptors and StreamInterceptors run before authentication.
	ExtraOptions       []grpc.ServerOption
	UnaryInterceptors  []grpc.UnaryServerInterceptor
	StreamInterceptors []grpc.StreamServerInterceptor

	// OfflineAfter is how long a host may stay silent; 3 missed heartbeats by default.
	OfflineAfter time.Duration
	// MonitorInterval is how often silent hosts are looked for.
	MonitorInterval time.Duration

	// OnHeartbeat is told the round trip and clock skew an agent measured.
	OnHeartbeat func(hostID string, rttMS, skewMS float64)
	// OnEdgeStatus is told that a heartbeat stored the state of a host's edge.
	OnEdgeStatus func(hostID string)
	// OnRequests is told how many requests each hostname of a host's edge had
	// since its previous heartbeat.
	OnRequests func(hostID string, byHost map[string]uint64)
	// OnLatency is told how long each hostname's requests took, and how many
	// failed, since the previous heartbeat (#50).
	OnLatency func(hostID string, byHost map[string]*agentv1.HostLatency)
	// OnStopping is called when the gateway begins to shut down.
	OnStopping func()

	// ConnectionEventsElsewhere is set when an observer records connects and
	// disconnects, with more detail than the gateway knows.
	ConnectionEventsElsewhere bool

	limiter *rateLimiter

	server   *grpc.Server
	listener net.Listener
	stopping chan struct{}
	stopOnce sync.Once
	cancel   context.CancelFunc

	hostLocks sync.Map

	rejectMu   sync.Mutex
	rejectLast map[string]time.Time
}

// ArtifactHandler stores a file an agent uploaded for a command. It receives the
// path of the file, which is removed afterwards, and returns the id of what it made.
type ArtifactHandler func(command *core.Record, fileName, path string, size uint64) (string, error)

// FetchHandler opens an artifact for the agent of hostID, which must have a
// command that needs it; ErrNoArtifact when it has none.
type FetchHandler func(hostID, artifactID string) (*Artifact, error)

// Artifact is a file an agent downloads.
type Artifact struct {
	Reader io.ReadSeekCloser
	Size   uint64
	// SHA256 is the hex digest of the whole file.
	SHA256 string
}

// ErrNoArtifact is a download the agent may not have.
var ErrNoArtifact = errors.New("no such artifact for this server")

// MetricsSink stores metric batches.
type MetricsSink interface {
	Write(hostID string, samples []*agentv1.MetricSample) error
}

// StreamBroker connects agent streams to browser sessions. Sessions belong to a
// host: an agent cannot reach the session of another one.
type StreamBroker interface {
	// Logs receives the lines of an active log session. False means that nobody is
	// watching that session, so the agent should stop sending.
	Logs(hostID, sessionID string, lines []*agentv1.LogLine) bool
	// LogsClosed tells that the agent's log stream of a session ended.
	LogsClosed(hostID, sessionID string)
	// Terminal attaches the agent's Exec stream to a session and blocks until it ends.
	Terminal(ctx context.Context, hostID, sessionID string, stream agentv1.AgentService_ExecServer) error
	// Relay attaches the agent's stream to a tool container to its session and
	// blocks until it ends.
	Relay(ctx context.Context, hostID, sessionID string, stream agentv1.AgentService_RelayServer) error
}

// Start listens on addr and serves in the background.
func (s *Server) Start(addr string) error {
	if s.Log == nil {
		s.Log = slog.Default().With("component", "gateway")
	}
	if s.OfflineAfter == 0 {
		s.OfflineAfter = 30 * time.Second
	}
	if s.MonitorInterval == 0 {
		s.MonitorInterval = 5 * time.Second
	}
	s.limiter = newRateLimiter(enrollPerMinute, time.Minute)
	s.stopping = make(chan struct{})
	s.rejectLast = make(map[string]time.Time)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("gRPC gateway: %w", err)
	}
	s.listener = listener

	unary := append(append([]grpc.UnaryServerInterceptor{}, s.UnaryInterceptors...), s.authUnary)
	stream := append(append([]grpc.StreamServerInterceptor{}, s.StreamInterceptors...), s.authStream)

	options := []grpc.ServerOption{
		grpc.Creds(credentials.NewTLS(s.Authority.ServerTLSConfig())),
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(stream...),
		grpc.MaxRecvMsgSize(maxMessageSize),
		grpc.MaxSendMsgSize(maxMessageSize),
		// NAT gateways drop silent connections: agents ping every 30 s, and may do
		// that without an active stream. The panel's own pings are what notices a
		// cut network within seconds; an agent's heartbeats usually make them unnecessary.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             15 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    10 * time.Second,
			Timeout: 5 * time.Second,
		}),
	}
	options = append(options, s.ExtraOptions...)

	s.server = grpc.NewServer(options...)
	agentv1.RegisterAgentServiceServer(s.server, s)

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	go s.monitor(ctx)
	go s.Commands.Run(ctx)
	go func() {
		if err := s.server.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			s.Log.Error("the gRPC gateway stopped", "error", err)
		}
	}()

	s.Log.Info("gRPC gateway started", "addr", listener.Addr().String())

	return nil
}

// Addr returns the address the gateway listens on.
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// Stop ends the long-lived streams and shuts the gateway down.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		if s.server == nil {
			return // never started
		}

		if s.OnStopping != nil {
			s.OnStopping()
		}

		close(s.stopping)
		s.cancel()

		done := make(chan struct{})
		go func() {
			s.server.GracefulStop()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(gracefulStopTimeout):
			s.server.Stop()
		}
	})
}

type identityKey struct{}

// identity is who is calling, as proven by the client certificate.
type identity struct {
	HostID string
	Serial string
}

func hostIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(identityKey{}).(identity)
	return id.HostID
}

const enrollMethod = agentv1.AgentService_Enroll_FullMethodName

func (s *Server) authUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if info.FullMethod == enrollMethod {
		return handler(ctx, req)
	}

	ctx, err := s.authenticate(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}

	return handler(ctx, req)
}

type authedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authedStream) Context() context.Context {
	return s.ctx
}

func (s *Server) authStream(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx, err := s.authenticate(stream.Context(), info.FullMethod)
	if err != nil {
		return err
	}

	return handler(srv, &authedStream{ServerStream: stream, ctx: ctx})
}

// authenticate checks the client certificate on every call, so that a revoked
// certificate stops working at once, even on a connection that is already open.
func (s *Server) authenticate(ctx context.Context, method string) (context.Context, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer information")
	}

	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return nil, status.Error(codes.Unauthenticated, "a client certificate is required; only Enroll works without one")
	}

	hostID, serial, err := s.Authority.VerifyAgent(info.State.PeerCertificates)
	if err != nil {
		s.rejected(serial, hostID, p.Addr.String(), method, err.Error())
		if errors.Is(err, ca.ErrExpired) {
			return nil, status.Error(codes.Unauthenticated, "the agent certificate has expired: enroll this server again")
		}
		return nil, status.Errorf(codes.Unauthenticated, "invalid agent certificate: %v", err)
	}

	if s.isRevoked(serial) {
		s.rejected(serial, hostID, p.Addr.String(), method, "revoked")
		return nil, status.Error(codes.PermissionDenied, "the agent certificate has been revoked: enroll this server again")
	}

	host, err := s.App.FindRecordById(schema.Hosts, hostID)
	if err != nil {
		s.rejected(serial, hostID, p.Addr.String(), method, "unknown host")
		return nil, status.Error(codes.PermissionDenied, "this server was removed from the panel: enroll it again")
	}

	// the first call with a renewed certificate retires the previous one
	if host.GetString("cert_serial") == serial && host.GetString("cert_prev_serial") != "" {
		s.retirePreviousCert(hostID)
	}

	return context.WithValue(ctx, identityKey{}, identity{HostID: hostID, Serial: serial}), nil
}

func (s *Server) isRevoked(serial string) bool {
	_, err := s.App.FindFirstRecordByData(schema.RevokedCerts, "serial", serial)
	return err == nil
}

// rejected records a refused connection as an event and a security event, at
// most once per minute for each certificate: an agent retries all the time.
func (s *Server) rejected(serial, hostID, addr, method, reason string) {
	key := serial + "|" + reason

	s.rejectMu.Lock()
	last := s.rejectLast[key]
	due := time.Since(last) >= rejectEventInterval
	if due {
		s.rejectLast[key] = time.Now()
	}
	s.rejectMu.Unlock()

	if !due {
		return
	}

	s.Log.Warn("rejected an agent connection", "serial", serial, "reason", reason, "addr", addr, "method", method)

	// the event's host is empty if the record no longer exists
	relatedHost := ""
	if hostID != "" {
		if _, err := s.App.FindRecordById(schema.Hosts, hostID); err == nil {
			relatedHost = hostID
		}
	}

	events.Log(s.App, relatedHost, "agent.rejected", schema.SeverityWarning,
		"An agent connection was rejected: "+reason, map[string]any{"serial": serial, "addr": addr})

	actor := audit.System("gRPC gateway", audit.SourceAgent)
	actor.IP = peerHost(addr)
	s.Audit.Security(audit.ActionCertRejected, actor, serial, map[string]any{"reason": reason})
}

func peerHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// rateLimiter is a fixed-window limiter per key.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	counts map[string]int
	start  time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, counts: make(map[string]int), start: time.Now()}
}

func (l *rateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if time.Since(l.start) >= l.window {
		l.counts = make(map[string]int)
		l.start = time.Now()
	}

	if l.counts[key] >= l.limit {
		return false
	}
	l.counts[key]++

	return true
}
