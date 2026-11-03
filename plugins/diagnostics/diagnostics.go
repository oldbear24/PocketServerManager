// Package diagnostics is a PocketBase plugin with detailed diagnostics of agents
// connected over gRPC: connection health, a message trace and panel internals. It
// keeps everything in memory, and knows nothing about the UI that shows it, so it
// can be used by other PocketBase + gRPC projects.
//
// Register it like PocketBase's own plugins:
//
//	plugin := diagnostics.MustRegister(app, diagnostics.Config{Enabled: true})
//	server := grpc.NewServer(
//		grpc.StatsHandler(plugin.StatsHandler()),
//		grpc.ChainUnaryInterceptor(plugin.UnaryInterceptor()),
//		grpc.ChainStreamInterceptor(plugin.StreamInterceptor()),
//	)
//
// A disabled plugin collects nothing and adds no routes.
package diagnostics

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"google.golang.org/protobuf/proto"
)

const (
	defaultTraceEntries = 500
	defaultWindow       = 24 * time.Hour
	captureDuration     = 10 * time.Minute
	rttSamples          = 360
	connectionEvents    = 200
	recentErrors        = 100
)

// Config configures the plugin.
type Config struct {
	// Enabled turns the plugin on. A disabled plugin has no overhead.
	Enabled bool

	// MetricsToken protects the Prometheus endpoint /metrics, which only exists
	// when this is set.
	MetricsToken string

	// TraceEntries is the size of each agent's message trace; 500 by default.
	TraceEntries int

	// Window is how long connection events and trace entries are kept; 24 h by default.
	Window time.Duration

	// AgentOf identifies the agent behind a call. It defaults to the remote address.
	AgentOf func(ctx context.Context) string

	// CertExpiry returns when the client certificate of a call expires.
	CertExpiry func(ctx context.Context) time.Time

	// Authorize decides who may use the API. Nil means superusers only.
	Authorize func(e *core.RequestEvent) error

	// OnConnection is told about connects and disconnects, to store them durably.
	OnConnection func(agentID string, event ConnectionEvent)

	// Describe classifies a message for the trace: "command", "heartbeat" or
	// anything else, and what it is about.
	Describe func(message proto.Message) (kind, subject string)

	// ConfigDump returns the configuration for a bundle, without secrets.
	ConfigDump func() map[string]any

	// QueueSnapshot returns the current state of a work queue, if there is one.
	QueueSnapshot func() QueueSnapshot

	// Streams counts open log and terminal relays.
	Streams func() int
}

// QueueSnapshot describes a work queue at one moment.
type QueueSnapshot struct {
	Depth        map[string]int `json:"depth"`
	OldestQueued time.Duration  `json:"oldest_queued_ns"`
}

// Plugin is a registered diagnostics plugin.
type Plugin struct {
	app    core.App
	config Config

	started time.Time

	mu     sync.RWMutex
	agents map[string]*agentState

	rate rateCounter

	queue queueStats

	subscribersMu sync.Mutex
	subscribers   map[chan TraceEntry]string

	errorsMu sync.Mutex
	errors   []PanelError

	shuttingDown atomic.Bool
}

// MustRegister registers the plugin and panics if that fails.
func MustRegister(app core.App, config Config) *Plugin {
	plugin, err := Register(app, config)
	if err != nil {
		panic(err)
	}
	return plugin
}

// Register registers the plugin with the app.
func Register(app core.App, config Config) (*Plugin, error) {
	if config.TraceEntries <= 0 {
		config.TraceEntries = defaultTraceEntries
	}
	if config.Window <= 0 {
		config.Window = defaultWindow
	}
	if config.AgentOf == nil {
		config.AgentOf = remoteAddressOf
	}

	plugin := &Plugin{
		app:         app,
		config:      config,
		started:     time.Now(),
		agents:      make(map[string]*agentState),
		subscribers: make(map[chan TraceEntry]string),
		queue:       queueStats{perType: make(map[string]*latencyPair)},
	}

	if !config.Enabled {
		return plugin, nil
	}

	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		plugin.bindRoutes(e)
		return e.Next()
	})

	return plugin, nil
}

// Enabled reports whether the plugin collects anything.
func (p *Plugin) Enabled() bool {
	return p.config.Enabled
}

// ErrorHandler wraps a log handler so that errors also reach the "recent errors"
// list of the overview and of bundles.
func (p *Plugin) ErrorHandler(next slog.Handler) slog.Handler {
	if !p.config.Enabled {
		return next
	}
	return &errorHandler{Handler: next, plugin: p}
}

// PanelError is one error logged by the panel.
type PanelError struct {
	At      time.Time `json:"at"`
	Message string    `json:"message"`
	Detail  string    `json:"detail,omitempty"`
}

// RecordError adds an error to the "recent errors" list.
func (p *Plugin) RecordError(message, detail string) {
	if !p.config.Enabled {
		return
	}

	p.errorsMu.Lock()
	defer p.errorsMu.Unlock()

	p.errors = append(p.errors, PanelError{At: time.Now().UTC(), Message: message, Detail: detail})
	if len(p.errors) > recentErrors {
		p.errors = p.errors[len(p.errors)-recentErrors:]
	}
}

func (p *Plugin) recentErrors() []PanelError {
	p.errorsMu.Lock()
	defer p.errorsMu.Unlock()
	return append([]PanelError{}, p.errors...)
}

type errorHandler struct {
	slog.Handler
	plugin *Plugin
}

func (h *errorHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Level >= slog.LevelError {
		detail := ""
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "error" {
				detail = attr.Value.String()
				return false
			}
			return true
		})
		h.plugin.RecordError(record.Message, detail)
	}
	return h.Handler.Handle(ctx, record)
}

func (h *errorHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &errorHandler{Handler: h.Handler.WithAttrs(attrs), plugin: h.plugin}
}

func (h *errorHandler) WithGroup(name string) slog.Handler {
	return &errorHandler{Handler: h.Handler.WithGroup(name), plugin: h.plugin}
}
