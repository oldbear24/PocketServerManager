package panel

import (
	"context"
	"crypto/x509"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/ca"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/gateway"
	"github.com/oldbear24/PocketServerManager/internal/pki"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/version"
	"github.com/oldbear24/PocketServerManager/plugins/diagnostics"
)

// Environment variables of the diagnostics plugin.
const (
	EnvDiagnostics  = "POCKETCLOUD_DIAGNOSTICS"
	EnvMetricsToken = "POCKETCLOUD_METRICS_TOKEN"
)

// EnableDiagnostics registers the diagnostics plugin and connects it to the
// gateway and the command queue. Call it before Register. It stays off when
// POCKETCLOUD_DIAGNOSTICS=0, and then adds no overhead.
func (p *Panel) EnableDiagnostics() *diagnostics.Plugin {
	enabled := os.Getenv(EnvDiagnostics) != "0"

	plugin := diagnostics.MustRegister(p.App, diagnostics.Config{
		Enabled:       enabled,
		MetricsToken:  os.Getenv(EnvMetricsToken),
		AgentOf:       p.agentOf,
		CertExpiry:    certExpiryOf,
		Authorize:     authorizeDiagnostics,
		OnConnection:  p.onAgentConnection,
		Describe:      describeMessage,
		ConfigDump:    p.configDump,
		QueueSnapshot: p.queueSnapshot,
	})

	p.Diagnostics = plugin

	if !enabled {
		return plugin
	}

	// errors of every component also reach the "recent errors" list
	slog.SetDefault(slog.New(plugin.ErrorHandler(slog.NewTextHandler(os.Stderr, nil))))
	p.Log = slog.Default().With("component", "panel")
	p.Commands.Log = slog.Default().With("component", "commands")

	p.Observers = append(p.Observers, func(server *gateway.Server) {
		server.ExtraOptions = append(server.ExtraOptions, grpc.StatsHandler(plugin.StatsHandler()))
		server.UnaryInterceptors = append(server.UnaryInterceptors, plugin.UnaryInterceptor())
		server.StreamInterceptors = append(server.StreamInterceptors, plugin.StreamInterceptor())
		server.OnHeartbeat = plugin.ObserveHeartbeat
		server.OnStopping = plugin.MarkShutdown
		server.ConnectionEventsElsewhere = true
	})

	p.Commands.OnReplay = func() { plugin.ObserveQueueEvent(diagnostics.QueueReplay) }

	p.App.OnRecordAfterUpdateSuccess(schema.Commands).BindFunc(func(e *core.RecordEvent) error {
		p.observeCommand(plugin, e.Record)
		return e.Next()
	})

	return plugin
}

// observeCommand feeds the latencies of a finished command to the plugin.
func (p *Panel) observeCommand(plugin *diagnostics.Plugin, record *core.Record) {
	finished := record.GetDateTime("finished_at")
	if finished.IsZero() {
		return
	}

	if record.GetString("status") == schema.CommandTimedOut {
		plugin.ObserveQueueEvent(diagnostics.QueueTimeout)
	}

	dispatched := record.GetDateTime("dispatched_at")
	started := record.GetDateTime("started_at")

	var dispatchToAck, ackToDone time.Duration
	if !dispatched.IsZero() && !started.IsZero() {
		dispatchToAck = started.Time().Sub(dispatched.Time())
	}
	if !started.IsZero() {
		ackToDone = finished.Time().Sub(started.Time())
	}

	plugin.ObserveCommandLatency(record.GetString("type"), dispatchToAck, ackToDone)
}

// agentOf identifies the host behind a gRPC call. Client certificates are only
// checked by the gateway's interceptor, so a certificate this panel did not issue
// must not reach anyone's statistics. Expired ones still count for their host: a
// rejected agent is exactly what diagnostics should be able to explain.
func (p *Panel) agentOf(ctx context.Context) string {
	cert := peerChain(ctx)
	if len(cert) == 0 {
		return ""
	}

	id, _, err := p.Authority.VerifyAgent(cert)
	if errors.Is(err, ca.ErrExpired) {
		return pki.HostIDOf(cert[0])
	}
	if err != nil {
		return ""
	}

	return id
}

func certExpiryOf(ctx context.Context) time.Time {
	cert := peerChain(ctx)
	if len(cert) == 0 {
		return time.Time{}
	}
	return cert[0].NotAfter
}

func peerChain(ctx context.Context) []*x509.Certificate {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil
	}

	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil
	}

	return info.State.PeerCertificates
}

// authorizeDiagnostics is the plugin's access rule: admins only.
func authorizeDiagnostics(e *core.RequestEvent) error {
	if e.HasSuperuserAuth() || (e.Auth != nil && e.Auth.GetString("role") == schema.RoleAdmin) {
		return nil
	}
	return e.ForbiddenError("Diagnostics are for administrators.", nil)
}

// onAgentConnection stores connection events, so that they survive restarts.
func (p *Panel) onAgentConnection(hostID string, event diagnostics.ConnectionEvent) {
	if _, err := p.App.FindRecordById(schema.Hosts, hostID); err != nil {
		return
	}

	severity := schema.SeverityInfo
	message := "Connected from " + event.Remote
	if event.Kind == "disconnected" {
		message = "Disconnected: " + event.Reason
		if event.Reason != diagnostics.ReasonEOF && event.Reason != diagnostics.ReasonShutdown && event.Reason != diagnostics.ReasonReplaced {
			severity = schema.SeverityWarning
		}
	}

	events.Log(p.App, hostID, "connection."+event.Kind, severity, message, map[string]any{
		"reason": event.Reason,
		"remote": event.Remote,
	})
}

// describeMessage classifies messages for the trace filters.
func describeMessage(message proto.Message) (kind, subject string) {
	switch m := message.(type) {
	case *agentv1.HeartbeatRequest, *agentv1.HeartbeatResponse:
		return "heartbeat", ""
	case *agentv1.SubscribeCommandsResponse:
		if command := m.GetCommand(); command != nil {
			return "command", command.GetType() + " " + command.GetId()
		}
		if cancel := m.GetCancel(); cancel != nil {
			return "command", "cancel " + cancel.GetCommandId()
		}
	case *agentv1.ReportCommandUpdateRequest:
		return "command", strings.TrimPrefix(m.GetState().String(), "COMMAND_STATE_") + " " + m.GetCommandId()
	case *agentv1.SubscribeCommandsRequest:
		return "command", "subscribe"
	}
	return "", ""
}

// configDump is what a debug bundle says about the panel: nothing secret.
func (p *Panel) configDump() map[string]any {
	return map[string]any{
		"version":          version.Version,
		"commit":           version.Commit,
		"public_url":       p.Config.PublicURL,
		"grpc_addr":        p.Config.GRPCAddr,
		"grpc_public_addr": p.Config.AgentEndpoint(),
		"agent_image":      version.AgentRef(),
		"updater_image":    version.UpdaterRef(),
		"data_dir":         p.App.DataDir(),
		"metrics_endpoint": os.Getenv(EnvMetricsToken) != "",
	}
}

func (p *Panel) queueSnapshot() diagnostics.QueueSnapshot {
	snapshot := diagnostics.QueueSnapshot{Depth: map[string]int{}}

	rows := []struct {
		Status string `db:"status"`
		Count  int    `db:"count"`
	}{}
	err := p.App.DB().
		Select("status", "count(*) as count").
		From(schema.Commands).
		GroupBy("status").
		All(&rows)
	if err != nil {
		p.Log.Error("failed to read the queue depth", "error", err)
		return snapshot
	}

	for _, row := range rows {
		snapshot.Depth[row.Status] = row.Count
	}

	oldest := struct {
		Created string `db:"created"`
	}{}
	err = p.App.DB().
		Select("created").
		From(schema.Commands).
		Where(dbx.HashExp{"status": schema.CommandQueued}).
		OrderBy("created ASC").
		Limit(1).
		One(&oldest)
	if err == nil {
		if created, err := time.Parse("2006-01-02 15:04:05.000Z", oldest.Created); err == nil {
			snapshot.OldestQueued = time.Since(created)
		}
	}

	return snapshot
}
