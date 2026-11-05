package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
	"github.com/oldbear24/PocketServerManager/internal/updater"
	"github.com/oldbear24/PocketServerManager/internal/version"
)

const (
	healthFile        = "health.json"
	shutdownTimeout   = 10 * time.Second
	logRingCapacity   = 2000
	heartbeatTimeout  = 8 * time.Second
	inventoryTimeout  = 2 * time.Minute
	dockerPingTimeout = 3 * time.Second
)

var errReconnect = errors.New("reconnecting with the renewed certificate")

// Agent is the running program.
type Agent struct {
	Config   Config
	Identity *Identity
	Docker   *dockerapi.Client
	Executor *Executor
	Log      *slog.Logger

	level *slog.LevelVar
	ring  *logRing
	host  *hostInfo
	store *commandStore

	checks *selfChecks

	levelMu    sync.Mutex
	levelTimer *time.Timer

	reconnect chan struct{}

	client atomic.Pointer[agentv1.AgentServiceClient]

	logSessions     sessionSet
	execSessions    sessionSet
	adminerSessions sessionSet
	metrics         *metricsCollector

	// DialTool opens a connection to a tool container such as Adminer. Tests,
	// which have no containers to reach, replace it.
	DialTool func(ctx context.Context, address string) (net.Conn, error)

	composeMu      sync.Mutex
	composeRelease string
	composeAt      time.Time

	// edgeMu keeps the edge's operations apart: applies, certificates, and
	// bringing it back at start
	edgeMu        sync.Mutex
	edgeMetricsMu sync.Mutex
	edgeCount     float64
	edgeCountAt   time.Time
	edgeByHost    map[string]float64

	lastRTT  atomic.Int64
	lastSkew atomic.Int64
}

// New prepares an agent. The state directory is created if needed.
func New(config Config, output io.Writer) (*Agent, error) {
	if err := os.MkdirAll(config.StateDir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot use the state directory %s: %w", config.StateDir, err)
	}

	level := &slog.LevelVar{}
	ring := newLogRing(logRingCapacity)
	log := slog.New(slog.NewTextHandler(io.MultiWriter(output, ring), &slog.HandlerOptions{Level: level}))

	docker, err := dockerapi.New(config.DockerHost)
	if err != nil {
		return nil, err
	}

	store, err := openCommandStore(config.StateDir)
	if err != nil {
		return nil, err
	}

	a := &Agent{
		Config:    config,
		Docker:    docker,
		Log:       log,
		level:     level,
		ring:      ring,
		host:      newHostInfo(config.HostRoot),
		store:     store,
		reconnect: make(chan struct{}, 1),
	}
	a.checks = newSelfChecks(a)
	a.metrics = newMetricsCollector(a)
	a.Executor = newExecutor(store, log)
	a.registerHandlers()

	return a, nil
}

// Run enrolls if needed and stays connected until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.ensureIdentity(ctx); err != nil {
		return err
	}

	a.Log.Info("the agent is starting", "version", version.Version, "host", a.Identity.Name, "panel", a.Identity.Panel)

	a.restoreCleanEnvFiles()
	a.removeStaleAdminers(ctx)

	// the edge keeps serving without the panel; one that is gone comes back
	go a.restoreEdge(ctx)
	go a.watchUpstreams(ctx)

	a.setHealth(false)
	defer a.setHealth(false)

	a.checks.run(ctx)

	backoff := a.Config.BackoffMin

	for {
		started := time.Now()

		err := a.session(ctx)
		a.setHealth(false)

		if ctx.Err() != nil {
			a.Log.Info("the agent is stopping")
			a.Executor.Shutdown(shutdownTimeout)
			return nil
		}

		if errors.Is(err, errReconnect) {
			backoff = a.Config.BackoffMin
			continue
		}

		// a session that lasted a while was a success: start over with short waits
		if time.Since(started) > a.Config.BackoffMax {
			backoff = a.Config.BackoffMin
		}

		a.logSessionEnd(err)

		// jitter keeps a fleet from reconnecting at the same moment after a panel restart
		wait := backoff/2 + rand.N(backoff/2+1) //nolint:gosec // jitter needs no strong randomness

		select {
		case <-ctx.Done():
			a.Executor.Shutdown(shutdownTimeout)
			return nil
		case <-time.After(wait):
		}

		backoff = min(backoff*2, a.Config.BackoffMax)
	}
}

func (a *Agent) logSessionEnd(err error) {
	switch status.Code(err) {
	case codes.Unauthenticated, codes.PermissionDenied:
		a.Log.Error("the panel refuses this agent", "reason", status.Convert(err).Message())
	default:
		a.Log.Warn("the connection to the panel was lost", "error", err)
	}
}

func (a *Agent) ensureIdentity(ctx context.Context) error {
	identity, err := LoadIdentity(a.Config.StateDir)
	if err == nil {
		a.Identity = identity
		return nil
	}
	if !errors.Is(err, ErrNotEnrolled) {
		return err
	}

	backoff := a.Config.BackoffMin

	for {
		identity, err := Enroll(ctx, a.Config, a.facts(ctx))
		if err == nil {
			a.Identity = identity
			a.Log.Info("the agent enrolled", "host", identity.Name, "id", identity.HostID)
			return nil
		}

		// a bad token will not get better by retrying, but the panel may be unreachable for a while
		switch status.Code(err) {
		case codes.Unauthenticated, codes.AlreadyExists, codes.InvalidArgument:
			return fmt.Errorf("enrollment failed: %s", status.Convert(err).Message())
		}
		if a.Config.Panel == "" || a.Config.CAPin == "" || a.Config.Token == "" {
			return err
		}

		a.Log.Warn("enrollment failed, trying again", "error", err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff = min(backoff*2, a.Config.BackoffMax)
	}
}

// dial opens the connection to the panel, with keepalive pings tuned to survive
// NAT timeouts on idle connections.
func (a *Agent) dial() (*grpc.ClientConn, error) {
	return grpc.NewClient(
		a.Identity.Panel,
		grpc.WithTransportCredentials(credentials.NewTLS(a.Identity.TLSConfig())),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                a.Config.KeepaliveTime,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
}

// session is one connection to the panel. It returns when the connection ends.
func (a *Agent) session(parent context.Context) error {
	conn, err := a.dial()
	if err != nil {
		return err
	}
	defer conn.Close()

	client := agentv1.NewAgentServiceClient(conn)

	group, ctx := errgroup.WithContext(parent)

	self := a.ownContainerID(ctx)

	capabilities := a.Executor.Capabilities()
	if a.Config.HostShell {
		capabilities = append(capabilities, capabilityHostShell)
	}

	stream, err := client.SubscribeCommands(ctx, &agentv1.SubscribeCommandsRequest{
		AgentVersion:     version.Version,
		Capabilities:     capabilities,
		RecentlyFinished: a.store.recentlyFinished(),
		ImageDigest:      a.ownImageDigest(ctx),
		ContainerId:      self,
	})
	if err != nil {
		return err
	}

	if _, err := stream.Header(); err != nil {
		return err
	}

	a.client.Store(&client)
	a.Executor.SetClient(client)
	defer func() {
		a.client.Store(nil)
		a.Executor.SetClient(nil)
		a.logSessions.closeAll()
		a.execSessions.closeAll()
	}()

	a.Log.Info("connected to the panel", "panel", a.Identity.Panel)

	// a first heartbeat right away, so the panel has the host's facts at once and
	// the container turns healthy
	if err := a.heartbeat(ctx, client); err != nil {
		return err
	}
	a.setHealth(true)

	a.reportOutcomes()

	group.Go(func() error { return a.receiveLoop(stream) })
	group.Go(func() error { return a.heartbeatLoop(ctx, client) })
	group.Go(func() error { return a.inventoryLoop(ctx, client) })
	group.Go(func() error { return a.eventsLoop(ctx, client) })
	if a.Config.MetricsInterval > 0 {
		group.Go(func() error { return a.metrics.loop(ctx, client) })
	}
	group.Go(func() error { return a.renewalLoop(ctx, client) })
	group.Go(func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-a.reconnect:
			return errReconnect
		}
	})

	err = group.Wait()

	// the connection is still open here, so running commands can report that they
	// were cancelled
	if parent.Err() != nil {
		a.Executor.Shutdown(shutdownTimeout)
	}

	return err
}

func (a *Agent) receiveLoop(stream agentv1.AgentService_SubscribeCommandsClient) error {
	for {
		message, err := stream.Recv()
		if err != nil {
			return err
		}

		switch event := message.GetEvent().(type) {
		case *agentv1.SubscribeCommandsResponse_Command:
			a.Executor.Submit(event.Command)
		case *agentv1.SubscribeCommandsResponse_Cancel:
			a.Executor.Cancel(event.Cancel.GetCommandId())
		}
	}
}

func (a *Agent) heartbeatLoop(ctx context.Context, client agentv1.AgentServiceClient) error {
	ticker := time.NewTicker(a.Config.HeartbeatInterval)
	defer ticker.Stop()

	checks := time.NewTicker(a.Config.SelfCheckInterval)
	defer checks.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-checks.C:
			a.checks.run(ctx)
		case <-ticker.C:
			if err := a.heartbeat(ctx, client); err != nil {
				return err
			}
			a.setHealth(true)
			a.reportOutcomes()
		}
	}
}

func (a *Agent) heartbeat(parent context.Context, client agentv1.AgentServiceClient) error {
	ctx, cancel := context.WithTimeout(parent, heartbeatTimeout)
	defer cancel()

	request := &agentv1.HeartbeatRequest{
		SentAt:      timestamppb.Now(),
		Facts:       a.facts(ctx),
		Vitals:      a.host.vitals(),
		Runtime:     a.runtimeStats(ctx),
		LastRttMs:   float64(a.lastRTT.Load()) / 1000,
		ClockSkewMs: float64(a.lastSkew.Load()) / 1000,
		SelfChecks:  a.checks.results(),
		Edge:        a.edgeStatus(ctx),
	}

	sent := time.Now()

	response, err := client.Heartbeat(ctx, request)
	if err != nil {
		return err
	}

	rtt := time.Since(sent)
	a.lastRTT.Store(rtt.Microseconds())

	// the panel's clock at the midpoint of the round trip against our own
	skew := response.GetServerTime().AsTime().Sub(sent.Add(rtt / 2))
	a.lastSkew.Store(skew.Microseconds())

	return nil
}

func (a *Agent) renewalLoop(ctx context.Context, client agentv1.AgentServiceClient) error {
	ticker := time.NewTicker(a.renewalCheckInterval())
	defer ticker.Stop()

	for {
		if a.Identity.NeedsRenewal() {
			if err := a.renewCertificate(ctx, client); err != nil {
				a.Log.Warn("failed to renew the certificate", "error", err)
			} else {
				return errReconnect
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (a *Agent) renewalCheckInterval() time.Duration {
	leaf := a.Identity.Certificate()
	if leaf != nil {
		// certificates are normally valid for months; tests use seconds
		if life := certificateLife(leaf); life < 12*time.Hour {
			return max(life/20, 100*time.Millisecond)
		}
	}
	return 6 * time.Hour
}

func (a *Agent) renewCertificate(ctx context.Context, client agentv1.AgentServiceClient) error {
	if err := a.Identity.Renew(ctx, client); err != nil {
		return err
	}
	a.Log.Info("the certificate was renewed", "expires", a.Identity.Certificate().NotAfter)
	return nil
}

// facts combines what the host mount and the Docker Engine say about the host.
func (a *Agent) facts(ctx context.Context) *agentv1.HostFacts {
	facts := a.host.facts()
	facts.AgentVersion = version.Version

	dockerCtx, cancel := context.WithTimeout(ctx, dockerPingTimeout)
	defer cancel()

	if info, err := a.Docker.Info(dockerCtx); err == nil {
		facts.DockerVersion = info.ServerVersion
		if facts.Os == runtime.GOOS || facts.Os == "" {
			facts.Os = info.OperatingSystem
		}
		if facts.Kernel == "" {
			facts.Kernel = info.KernelVersion
		}
		if facts.Hostname == "" {
			facts.Hostname = info.Name
		}
		if info.NCPU > 0 {
			facts.CpuCores = info.NCPU
		}
		if facts.MemBytes == 0 {
			facts.MemBytes = info.MemTotal
		}
	}

	facts.AgentContainerId = a.ownContainerID(ctx)
	facts.AgentImageDigest = a.ownImageDigest(ctx)
	facts.ComposeVersion = a.composeVersion(ctx)

	return facts
}

func (a *Agent) runtimeStats(ctx context.Context) *agentv1.AgentRuntime {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)

	stats := &agentv1.AgentRuntime{
		Goroutines:  int32(runtime.NumGoroutine()), //nolint:gosec // a small number
		HeapBytes:   memory.HeapAlloc,
		WorkersBusy: a.Executor.busy.Load(),
		WorkersMax:  maxWorkers,
		LogLevel:    strings.ToLower(a.level.Level().String()),
	}

	pingCtx, cancel := context.WithTimeout(ctx, dockerPingTimeout)
	defer cancel()

	start := time.Now()
	if err := a.Docker.Ping(pingCtx); err == nil {
		stats.DockerApiLatencyMs = float64(time.Since(start).Microseconds()) / 1000
	}

	if _, _, available, err := diskUsage(a.Config.StateDir); err == nil {
		stats.VolumeFreeBytes = available
	}

	return stats
}

// ownContainerID finds the agent's own container by its configured name.
func (a *Agent) ownContainerID(ctx context.Context) string {
	info, err := a.ownContainer(ctx)
	if err != nil {
		return ""
	}
	return info.ID
}

func (a *Agent) ownContainer(ctx context.Context) (*dockerapi.ContainerInspect, error) {
	inspectCtx, cancel := context.WithTimeout(ctx, dockerPingTimeout)
	defer cancel()

	// Docker sets the hostname to the short container id unless it was overridden
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		if info, err := a.Docker.Inspect(inspectCtx, hostname); err == nil {
			return info, nil
		}
	}

	return a.Docker.Inspect(inspectCtx, a.Config.ContainerName)
}

func (a *Agent) ownImageDigest(ctx context.Context) string {
	info, err := a.ownContainer(ctx)
	if err != nil {
		return ""
	}

	image, _ := info.Config["Image"].(string)
	if at := strings.Index(image, "@"); at >= 0 {
		return image[at+1:]
	}

	// started by a tag: the digest the registry gave for the image, so that the
	// value means the same as for an agent started by digest
	repository := image
	if colon := strings.LastIndex(image, ":"); colon > strings.LastIndex(image, "/") {
		repository = image[:colon]
	}
	if found, err := a.Docker.InspectImage(ctx, info.Image); err == nil {
		for _, digest := range found.RepoDigests {
			if name, value, ok := strings.Cut(digest, "@"); ok && name == repository {
				return value
			}
		}
	}

	// an image that never came from a registry has only its local id
	return info.Image
}

// reportOutcomes reports what the updater left in the volume, if anything.
func (a *Agent) reportOutcomes() {
	outcomes, err := updater.ReadOutcomes(a.Config.StateDir)
	if err != nil {
		a.Log.Warn("failed to read update outcomes", "error", err)
		return
	}

	for _, outcome := range outcomes {
		state := stateSucceeded
		protoState := agentv1.CommandState_COMMAND_STATE_SUCCEEDED
		message := ""
		if !outcome.Succeeded {
			state = stateFailed
			protoState = agentv1.CommandState_COMMAND_STATE_FAILED
			message = outcome.Error
			if outcome.RolledBack {
				message = "The update was rolled back: " + outcome.Error
			}
		}

		result := map[string]any{
			"old_image":   outcome.OldImage,
			"new_image":   outcome.NewImage,
			"version":     outcome.Version,
			"step":        outcome.Step,
			"rolled_back": outcome.RolledBack,
		}

		if err := a.store.setState(outcome.CommandID, state, message, result); err != nil {
			a.Log.Error("failed to store an update outcome", "command", outcome.CommandID, "error", err)
			continue
		}

		if err := a.sendFinal(outcome.CommandID, protoState, message, result); err != nil {
			// it is reported again with the next heartbeat
			a.Log.Debug("failed to report an update outcome", "command", outcome.CommandID, "error", err)
			continue
		}

		if err := updater.RemoveOutcome(a.Config.StateDir, outcome.CommandID); err != nil {
			a.Log.Warn("failed to remove an update outcome", "command", outcome.CommandID, "error", err)
		}
	}
}

func (a *Agent) sendFinal(id string, state agentv1.CommandState, message string, result map[string]any) error {
	update := &agentv1.ReportCommandUpdateRequest{
		CommandId: id,
		Sequence:  uint64(time.Now().UnixNano()), //nolint:gosec // a positive clock value
		State:     state,
		Error:     message,
		Duplicate: true,
	}
	if encoded, err := structpb.NewStruct(result); err == nil {
		update.Result = encoded
	}
	return a.Executor.send(update)
}

type healthDocument struct {
	Connected     bool      `json:"connected"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

func (a *Agent) setHealth(connected bool) {
	document := healthDocument{Connected: connected, LastHeartbeat: time.Now().UTC()}

	raw, err := json.Marshal(document)
	if err != nil {
		return
	}

	if err := writeAtomic(filepath.Join(a.Config.StateDir, healthFile), raw, 0o644); err != nil {
		a.Log.Warn("failed to write the health state", "error", err)
	}
}

// Healthy is the image's health check: it passes only while the agent is
// connected to the panel. The updater relies on this to judge a new agent.
func Healthy(config Config) error {
	raw, err := os.ReadFile(filepath.Join(config.StateDir, healthFile))
	if err != nil {
		return fmt.Errorf("the agent has not connected yet: %w", err)
	}

	var document healthDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return err
	}

	if !document.Connected {
		return errors.New("the agent is not connected to the panel")
	}

	limit := 3*config.HeartbeatInterval + 5*time.Second
	if age := time.Since(document.LastHeartbeat); age > limit {
		return fmt.Errorf("the last heartbeat was %s ago", age.Round(time.Second))
	}

	return nil
}

// setLogLevel changes the log level, for a limited time if duration is positive.
func (a *Agent) setLogLevel(level string, duration time.Duration) error {
	var parsed slog.Level
	if err := parsed.UnmarshalText([]byte(level)); err != nil {
		return fmt.Errorf("unknown log level %q", level)
	}

	a.levelMu.Lock()
	defer a.levelMu.Unlock()

	if a.levelTimer != nil {
		a.levelTimer.Stop()
		a.levelTimer = nil
	}

	a.level.Set(parsed)

	if duration > 0 && parsed != slog.LevelInfo {
		a.levelTimer = time.AfterFunc(duration, func() {
			a.level.Set(slog.LevelInfo)
			a.Log.Info("the log level fell back to info")
		})
	}

	return nil
}

// logRing keeps the most recent log lines for debug bundles.
type logRing struct {
	mu    sync.Mutex
	lines []string
	next  int
	full  bool
}

func newLogRing(capacity int) *logRing {
	return &logRing{lines: make([]string, capacity)}
}

func (r *logRing) Write(data []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.lines[r.next] = string(data)
	r.next = (r.next + 1) % len(r.lines)
	if r.next == 0 {
		r.full = true
	}

	return len(data), nil
}

func (r *logRing) last(count int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	size := r.next
	if r.full {
		size = len(r.lines)
	}
	count = min(count, size)

	result := make([]string, 0, count)
	for i := size - count; i < size; i++ {
		index := i
		if r.full {
			index = (r.next + i) % len(r.lines)
		}
		result = append(result, r.lines[index])
	}

	return result
}

// sessionSet tracks long-lived streams opened by commands.
type sessionSet struct {
	mu       sync.Mutex
	sessions map[string]context.CancelFunc
}

func (s *sessionSet) add(id string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = make(map[string]context.CancelFunc)
	}
	s.sessions[id] = cancel
}

func (s *sessionSet) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

func (s *sessionSet) stop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel := s.sessions[id]; cancel != nil {
		cancel()
		delete(s.sessions, id)
	}
}

func (s *sessionSet) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, cancel := range s.sessions {
		cancel()
		delete(s.sessions, id)
	}
}
