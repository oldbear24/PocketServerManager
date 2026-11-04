package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
	"github.com/pocketbase/pocketbase/tools/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/ca"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

const (
	ruleAgentOffline = "agent_offline"
	// capabilityHostShell is how an agent says that it allows host shells.
	capabilityHostShell = "host_shell"
	maxArtifactBytes    = 1 << 30
	bundleTTL           = 7 * 24 * time.Hour
)

func (s *Server) hostLock(hostID string) *sync.Mutex {
	lock, _ := s.hostLocks.LoadOrStore(hostID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// updateHost changes a host in one transaction. A save writes every field, so
// reading and saving apart from each other could undo a change made in between:
// a heartbeat would silently bring back the roles an admin has just edited.
func (s *Server) updateHost(hostID string, change func(host *core.Record)) error {
	lock := s.hostLock(hostID)
	lock.Lock()
	defer lock.Unlock()

	return s.App.RunInTransaction(func(tx core.App) error {
		host, err := tx.FindRecordById(schema.Hosts, hostID)
		if err != nil {
			return err
		}

		change(host)

		return tx.Save(host)
	})
}

// SubscribeCommands delivers commands until the connection ends.
func (s *Server) SubscribeCommands(req *agentv1.SubscribeCommandsRequest, stream agentv1.AgentService_SubscribeCommandsServer) error {
	ctx := stream.Context()
	hostID := hostIDFrom(ctx)

	// The stream is registered before the host is marked as connected: whoever sees
	// stream_open can then rely on Commands.Connected, as the terminal and the logs do.
	delivery := s.Commands.Connect(hostID, req.GetCapabilities(), req.GetRecentlyFinished())
	defer func() {
		s.Commands.Disconnect(delivery)
		s.streamClosed(hostID)
	}()

	cameBack := false
	err := s.updateHost(hostID, func(host *core.Record) {
		// the first connection of a new server is not a recovery
		cameBack = host.GetString("status") == schema.HostOffline && !host.GetDateTime("connected_since").IsZero()
		host.Set("stream_open", true)
		host.Set("connected_since", types.NowDateTime())
		host.Set("last_seen", types.NowDateTime())
		host.Set("status", schema.HostOnline)
		host.Set("capabilities", req.GetCapabilities())
		host.Set("host_shell_enabled", slices.Contains(req.GetCapabilities(), capabilityHostShell))
		if v := req.GetAgentVersion(); v != "" {
			host.Set("agent_version", strings.TrimPrefix(v, "v"))
		}
		if v := req.GetImageDigest(); v != "" {
			host.Set("agent_image_digest", v)
		}
		if v := req.GetContainerId(); v != "" {
			host.Set("agent_container_id", v)
		}
	})
	if err != nil {
		return status.Errorf(codes.Internal, "failed to update the host: %v", err)
	}

	if cameBack {
		s.hostCameBack(hostID)
	}
	if !s.ConnectionEventsElsewhere {
		events.Log(s.App, hostID, "connection.connected", schema.SeverityInfo, "The agent connected", nil)
	}

	if err := stream.SendHeader(nil); err != nil {
		return err
	}

	for {
		message, err := delivery.Next(ctx, s.stopping)
		switch {
		case errors.Is(err, commands.ErrReplaced):
			return status.Error(codes.Aborted, err.Error())
		case errors.Is(err, commands.ErrStopping):
			return status.Error(codes.Unavailable, err.Error())
		case err != nil:
			return err
		}
		if err := stream.Send(message); err != nil {
			return err
		}
	}
}

func (s *Server) streamClosed(hostID string) {
	// a newer stream may already have taken over
	if s.Commands.Connected(hostID) {
		return
	}

	closed := false
	err := s.updateHost(hostID, func(host *core.Record) {
		// Asked again under the host's lock: a newer stream connects before it takes
		// that lock to mark the host, so it cannot be marked closed after that.
		if s.Commands.Connected(hostID) {
			return
		}
		closed = true
		host.Set("stream_open", false)
		if host.GetString("status") == schema.HostOnline {
			host.Set("status", schema.HostDegraded)
		}
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		s.Log.Debug("failed to update a disconnected host", "host", hostID, "error", err)
		return
	}
	if !closed {
		return
	}

	if !s.ConnectionEventsElsewhere {
		events.Log(s.App, hostID, "connection.disconnected", schema.SeverityInfo, "The command stream of the agent closed", nil)
	}
}

// ReportCommandUpdate stores progress and results.
func (s *Server) ReportCommandUpdate(ctx context.Context, req *agentv1.ReportCommandUpdateRequest) (*agentv1.ReportCommandUpdateResponse, error) {
	cancel, err := s.Commands.Update(hostIDFrom(ctx), req)
	if errors.Is(err, commands.ErrWrongHost) {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to store the update: %v", err)
	}

	return &agentv1.ReportCommandUpdateResponse{Cancel: cancel}, nil
}

// Heartbeat updates the host's live state.
func (s *Server) Heartbeat(ctx context.Context, req *agentv1.HeartbeatRequest) (*agentv1.HeartbeatResponse, error) {
	hostID := hostIDFrom(ctx)
	now := time.Now().UTC()

	var (
		cameBack  bool
		failed    []selfCheckChange
		recovered []selfCheckChange
		renew     bool
	)

	err := s.updateHost(hostID, func(host *core.Record) {
		cameBack = host.GetString("status") == schema.HostOffline && !host.GetDateTime("connected_since").IsZero()

		applyFacts(host, req.GetFacts())

		if vitals := req.GetVitals(); vitals != nil {
			host.Set("uptime_s", int64(vitals.GetUptime().AsDuration().Seconds()))
			host.Set("load1", vitals.GetLoad1())
			host.Set("load5", vitals.GetLoad5())
			host.Set("load15", vitals.GetLoad15())
			host.Set("cpu_percent", vitals.GetCpuPercent())
			host.Set("mem_used_bytes", vitals.GetMemUsedBytes())
			host.Set("mem_bytes", vitals.GetMemTotalBytes())
			host.Set("disk_used_bytes", vitals.GetDiskUsedBytes())
			host.Set("disk_total_bytes", vitals.GetDiskTotalBytes())
		}

		host.Set("last_seen", types.NowDateTime())
		host.Set("rtt_ms", req.GetLastRttMs())
		host.Set("clock_skew_ms", req.GetClockSkewMs())

		if runtime := req.GetRuntime(); runtime != nil {
			host.Set("runtime", map[string]any{
				"goroutines":            runtime.GetGoroutines(),
				"heap_bytes":            runtime.GetHeapBytes(),
				"workers_busy":          runtime.GetWorkersBusy(),
				"workers_max":           runtime.GetWorkersMax(),
				"docker_api_latency_ms": runtime.GetDockerApiLatencyMs(),
				"volume_free_bytes":     runtime.GetVolumeFreeBytes(),
				"log_level":             runtime.GetLogLevel(),
			})
		}

		applyEdgeStatus(host, req.GetEdge())

		failed, recovered = s.applySelfChecks(host, req.GetSelfChecks())

		host.Set("status", hostStatus(host, req.GetSelfChecks()))

		renew = time.Until(host.GetDateTime("cert_expires_at").Time()) < ca.RenewBefore
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to update the host: %v", err)
	}

	if cameBack {
		s.hostCameBack(hostID)
	}
	if s.OnHeartbeat != nil {
		s.OnHeartbeat(hostID, req.GetLastRttMs(), req.GetClockSkewMs())
	}
	if s.OnEdgeStatus != nil {
		s.OnEdgeStatus(hostID)
	}
	for _, check := range failed {
		events.Log(s.App, hostID, "agent.self_check_failed", schema.SeverityWarning,
			fmt.Sprintf("Self-check %s failed: %s", check.name, check.detail),
			map[string]any{"check": check.name, "detail": check.detail})
	}
	for _, check := range recovered {
		events.Log(s.App, hostID, "agent.self_check_recovered", schema.SeverityInfo,
			fmt.Sprintf("Self-check %s passes again", check.name),
			map[string]any{"check": check.name})
	}

	return &agentv1.HeartbeatResponse{
		ServerTime:       timestamppb.New(now),
		RenewCertificate: renew,
	}, nil
}

// applyEdgeStatus stores what the agent says of its edge proxy; a heartbeat
// without one comes from a server that runs none.
func applyEdgeStatus(host *core.Record, status *agentv1.EdgeStatus) {
	host.Set("edge_applied_digest", status.GetDigest())
	host.Set("edge_applied_revision", status.GetRevision())
	host.Set("edge_running", status.GetRunning())
	host.Set("edge_error", status.GetError())
	host.Set("edge_requests_per_s", status.GetRequestsPerS())
	if version := status.GetCaddyVersion(); version != "" || status == nil {
		host.Set("edge_caddy_version", version)
	}
}

// hostStatus is online when everything works, degraded when the agent is alive
// but something is wrong, and it never returns offline: that is only the
// monitor's call, after heartbeats stop.
func hostStatus(host *core.Record, checks []*agentv1.SelfCheck) string {
	if !host.GetBool("stream_open") {
		return schema.HostDegraded
	}
	for _, check := range checks {
		if !check.GetOk() && !check.GetSkipped() {
			return schema.HostDegraded
		}
	}
	return schema.HostOnline
}

type selfCheckChange struct {
	name   string
	detail string
}

// applySelfChecks stores the results and returns the checks that changed state,
// so that the timeline holds transitions and not every heartbeat.
func (s *Server) applySelfChecks(host *core.Record, checks []*agentv1.SelfCheck) (failed, recovered []selfCheckChange) {
	if len(checks) == 0 {
		return nil, nil
	}

	previous := map[string]bool{}
	stored := []struct {
		Name string `json:"name"`
		OK   bool   `json:"ok"`
	}{}
	if err := json.Unmarshal([]byte(host.GetString("self_checks")), &stored); err == nil {
		for _, check := range stored {
			previous[check.Name] = check.OK
		}
	}

	results := make([]map[string]any, 0, len(checks))
	for _, check := range checks {
		results = append(results, map[string]any{
			"name":       check.GetName(),
			"ok":         check.GetOk(),
			"skipped":    check.GetSkipped(),
			"detail":     check.GetDetail(),
			"checked_at": check.GetCheckedAt().AsTime().UTC().Format(time.RFC3339),
		})

		wasOK, known := previous[check.GetName()]
		switch {
		case check.GetSkipped():
		case !check.GetOk() && (!known || wasOK):
			failed = append(failed, selfCheckChange{name: check.GetName(), detail: check.GetDetail()})
		case check.GetOk() && known && !wasOK:
			recovered = append(recovered, selfCheckChange{name: check.GetName()})
		}
	}
	host.Set("self_checks", results)

	return failed, recovered
}

func (s *Server) hostCameBack(hostID string) {
	if err := events.ResolveAlert(s.App, ruleAgentOffline, hostID); err != nil {
		s.Log.Error("failed to resolve an alert", "host", hostID, "error", err)
	}
	events.Log(s.App, hostID, "agent.online", schema.SeverityInfo, "The server is back online", nil)
}

// monitor marks hosts offline after 3 missed heartbeats.
func (s *Server) monitor(ctx context.Context) {
	ticker := time.NewTicker(s.MonitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.CheckOffline()
		}
	}
}

// CheckOffline marks hosts that stopped sending heartbeats as offline. Every
// transition produces an event and an alert.
func (s *Server) CheckOffline() {
	cutoff, err := types.ParseDateTime(time.Now().UTC().Add(-s.OfflineAfter))
	if err != nil {
		return
	}

	hosts := []*core.Record{}
	err = s.App.RecordQuery(schema.Hosts).
		AndWhere(dbx.NewExp("[[status]] != {:offline} AND [[last_seen]] != '' AND [[last_seen]] < {:cutoff}", dbx.Params{
			"offline": schema.HostOffline,
			"cutoff":  cutoff.String(),
		})).
		All(&hosts)
	if err != nil {
		s.Log.Error("failed to look for silent hosts", "error", err)
		return
	}

	for _, host := range hosts {
		name := host.GetString("name")

		err := s.updateHost(host.Id, func(fresh *core.Record) {
			fresh.Set("status", schema.HostOffline)
			fresh.Set("stream_open", false)
		})
		if err != nil {
			s.Log.Error("failed to mark a host offline", "host", host.Id, "error", err)
			continue
		}

		message := fmt.Sprintf("%s: the agent stopped responding", name)
		events.Log(s.App, host.Id, "agent.offline", schema.SeverityCritical, message, nil)
		if err := events.RaiseAlert(s.App, ruleAgentOffline, host.Id, host.Id, schema.SeverityCritical, message); err != nil {
			s.Log.Error("failed to raise an alert", "host", host.Id, "error", err)
		}
	}
}

// ReportInventory applies a complete inventory snapshot.
func (s *Server) ReportInventory(ctx context.Context, req *agentv1.ReportInventoryRequest) (*agentv1.ReportInventoryResponse, error) {
	hostID := hostIDFrom(ctx)

	if err := s.Inventory.Sync(hostID, req); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to store the inventory: %v", err)
	}

	err := s.updateHost(hostID, func(host *core.Record) {
		host.Set("inventory_at", types.NowDateTime())
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to update the host: %v", err)
	}

	return &agentv1.ReportInventoryResponse{}, nil
}

// ReportEvents applies incremental inventory changes.
func (s *Server) ReportEvents(stream agentv1.AgentService_ReportEventsServer) error {
	hostID := hostIDFrom(stream.Context())

	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&agentv1.ReportEventsResponse{})
		}
		if err != nil {
			return err
		}

		if err := s.Inventory.Apply(hostID, event); err != nil {
			s.Log.Error("failed to apply an inventory change", "host", hostID, "error", err)
		}
	}
}

// PushMetrics stores metric batches.
func (s *Server) PushMetrics(stream agentv1.AgentService_PushMetricsServer) error {
	hostID := hostIDFrom(stream.Context())

	for {
		batch, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&agentv1.PushMetricsResponse{})
		}
		if err != nil {
			return err
		}

		if s.Metrics == nil || reflect.ValueOf(s.Metrics).IsNil() {
			continue
		}
		if err := s.Metrics.Write(hostID, batch.GetSamples()); err != nil {
			s.Log.Error("failed to store metrics", "host", hostID, "error", err)
		}
	}
}

// UploadArtifact stores a file sent by an agent. Diagnostics bundles end up in
// diagnostic_bundles, with an expiry.
func (s *Server) UploadArtifact(stream agentv1.AgentService_UploadArtifactServer) error {
	hostID := hostIDFrom(stream.Context())

	first, err := stream.Recv()
	if err != nil {
		return err
	}

	header := first.GetHeader()
	if header == nil {
		return status.Error(codes.InvalidArgument, "the first message must be the header")
	}

	command, err := s.App.FindRecordById(schema.Commands, header.GetCommandId())
	if err != nil || command.GetString("host") != hostID {
		return status.Error(codes.PermissionDenied, "the artifact does not belong to a command of this host")
	}

	handler, known := s.Artifacts[header.GetKind()]
	if !known && header.GetKind() != "diagnostics" {
		return status.Errorf(codes.InvalidArgument, "unknown artifact kind %q", header.GetKind())
	}

	// a directory per upload, so the file can keep the name it should download with
	base := filepath.Join(s.App.DataDir(), core.LocalTempDirName)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return status.Errorf(codes.Internal, "cannot store the artifact: %v", err)
	}

	dir, err := os.MkdirTemp(base, "artifact-*")
	if err != nil {
		return status.Errorf(codes.Internal, "cannot store the artifact: %v", err)
	}
	defer os.RemoveAll(dir)

	name := filepath.Base(filepath.Clean("/" + strings.ReplaceAll(header.GetFileName(), "\\", "/")))
	if name == "" || name == "/" || name == "." {
		name = "artifact.bin"
	}

	spool, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		return status.Errorf(codes.Internal, "cannot store the artifact: %v", err)
	}
	defer spool.Close()

	var size uint64
	for {
		part, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		size += uint64(len(part.GetChunk()))
		if size > maxArtifactBytes {
			return status.Error(codes.ResourceExhausted, "the artifact is too large")
		}
		if _, err := spool.Write(part.GetChunk()); err != nil {
			return status.Errorf(codes.Internal, "cannot store the artifact: %v", err)
		}
	}

	if err := spool.Close(); err != nil {
		return status.Errorf(codes.Internal, "cannot store the artifact: %v", err)
	}

	if handler != nil {
		id, err := handler(command, name, spool.Name(), size)
		if err != nil {
			return status.Errorf(codes.Internal, "cannot store the artifact: %v", err)
		}
		return stream.SendAndClose(&agentv1.UploadArtifactResponse{ArtifactId: id, SizeBytes: size})
	}

	file, err := filesystem.NewFileFromPath(spool.Name())
	if err != nil {
		return status.Errorf(codes.Internal, "cannot store the artifact: %v", err)
	}

	collection, err := s.App.FindCachedCollectionByNameOrId(schema.DiagnosticBundles)
	if err != nil {
		return status.Errorf(codes.Internal, "cannot store the artifact: %v", err)
	}

	expires, err := types.ParseDateTime(time.Now().UTC().Add(bundleTTL))
	if err != nil {
		return status.Errorf(codes.Internal, "cannot store the artifact: %v", err)
	}

	bundle := core.NewRecord(collection)
	bundle.Set("host", hostID)
	bundle.Set("command", command.Id)
	bundle.Set("requested_by", command.GetString("requested_by"))
	bundle.Set("file", file)
	bundle.Set("size_bytes", size)
	bundle.Set("expires_at", expires)

	if err := s.App.Save(bundle); err != nil {
		return status.Errorf(codes.Internal, "cannot store the artifact: %v", err)
	}

	return stream.SendAndClose(&agentv1.UploadArtifactResponse{ArtifactId: bundle.Id, SizeBytes: size})
}

// StreamLogs relays log chunks to the browsers watching a session.
func (s *Server) StreamLogs(stream agentv1.AgentService_StreamLogsServer) error {
	if s.Streams == nil {
		return status.Error(codes.Unimplemented, "log streaming is not available")
	}

	hostID := hostIDFrom(stream.Context())

	sessionID := ""
	defer func() {
		if sessionID != "" {
			s.Streams.LogsClosed(hostID, sessionID)
		}
	}()

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stream.SendAndClose(&agentv1.StreamLogsResponse{})
		}
		if err != nil {
			return err
		}

		sessionID = chunk.GetSessionId()
		if !s.Streams.Logs(hostID, sessionID, chunk.GetLines()) {
			return status.Error(codes.NotFound, "nobody is watching this log session")
		}
	}
}

// Exec attaches a terminal session opened by an agent to its browser session.
func (s *Server) Exec(stream agentv1.AgentService_ExecServer) error {
	if s.Streams == nil {
		return status.Error(codes.Unimplemented, "terminal sessions are not available")
	}

	first, err := stream.Recv()
	if err != nil {
		return err
	}

	return s.Streams.Terminal(stream.Context(), hostIDFrom(stream.Context()), first.GetSessionId(), &firstExecMessage{AgentService_ExecServer: stream, first: first})
}

// Relay attaches an agent's stream to a tool container, such as Adminer, to the
// session it was opened for. The first message only names the session.
func (s *Server) Relay(stream agentv1.AgentService_RelayServer) error {
	if s.Streams == nil {
		return status.Error(codes.Unimplemented, "relays are not available")
	}

	first, err := stream.Recv()
	if err != nil {
		return err
	}

	return s.Streams.Relay(stream.Context(), hostIDFrom(stream.Context()), first.GetSessionId(), stream)
}

// firstExecMessage hands the session id message back to the reader of the stream.
type firstExecMessage struct {
	agentv1.AgentService_ExecServer
	first *agentv1.ExecRequest
}

func (f *firstExecMessage) Recv() (*agentv1.ExecRequest, error) {
	if f.first != nil {
		first := f.first
		f.first = nil
		return first, nil
	}
	return f.AgentService_ExecServer.Recv()
}
