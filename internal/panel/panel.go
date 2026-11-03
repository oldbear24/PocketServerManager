// Package panel wires the panel's components into a PocketBase app: secrets, the
// change audit, the command queue, the gRPC gateway for agents and the web UI.
package panel

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/url"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/ca"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/gateway"
	"github.com/oldbear24/PocketServerManager/internal/inventory"
	"github.com/oldbear24/PocketServerManager/internal/metrics"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/streams"
	"github.com/oldbear24/PocketServerManager/plugins/diagnostics"
)

const eventRetention = 30 * 24 * time.Hour

// Panel holds the components of a running panel.
type Panel struct {
	App    core.App
	Config *Config
	UI     fs.FS
	Log    *slog.Logger

	Box       *secrets.Box
	Audit     *audit.Writer
	Authority *ca.Authority
	Commands  *commands.Manager
	Gateway   *gateway.Server
	Updates   *updateBatches
	Metrics   *metrics.Store
	Streams   *streams.Broker

	// Diagnostics is the diagnostics plugin, if it was enabled.
	Diagnostics *diagnostics.Plugin

	// TerminalIdle closes a terminal without input or output; zero is the default.
	TerminalIdle    time.Duration
	terminalTickets terminalTickets

	// setupToken allows creating the first admin account; empty once one exists.
	setupToken string

	// Observers are told about the gateway before it starts, for plugins that
	// instrument it, such as diagnostics.
	Observers []func(*gateway.Server)
}

// New creates the panel for a PocketBase app. Call Register before starting it.
func New(app core.App, config *Config, ui fs.FS) *Panel {
	p := &Panel{
		App:      app,
		Config:   config,
		UI:       ui,
		Log:      slog.Default().With("component", "panel"),
		Audit:    audit.New(app, schema.Audited),
		Commands: commands.NewManager(app),
	}
	p.Updates = newUpdateBatches(p)
	p.Streams = streams.New(p.Commands)
	return p
}

// Register binds the panel's hooks. The components start from OnServe, so
// commands like `panel migrate` and `panel superuser` need no master key.
func (p *Panel) Register() {
	p.Audit.Register()
	p.bindEncryption()
	p.bindAuditRoutes()
	p.Commands.Register()
	p.Updates.register()
	p.registerMetrics()
	p.Streams.Register()
	p.registerStacks()

	p.App.OnServe().BindFunc(func(e *core.ServeEvent) error {
		// PocketBase's default installer opens the machine's default browser with a
		// link that holds a superuser token. A panel usually runs on a server and
		// must never start programs by itself, so this stays off; the first account
		// comes from the setup link, and a superuser from the command line.
		e.InstallerFunc = nil

		if err := p.loadMasterKey(); err != nil {
			return err
		}

		if err := p.startGateway(); err != nil {
			return err
		}

		if err := p.prepareFirstRun(); err != nil {
			return err
		}

		group := e.Router.Group("/api/pc")
		group.Bind(apis.RequireAuth())
		p.bindRoutes(group)
		p.bindMetricsRoutes(group)
		p.bindStreamRoutes(group)
		p.bindStackRoutes(group)
		p.bindTerminalRoutes(group)
		p.Commands.BindRoutes(group)
		p.bindPublicRoutes(e)

		if p.UI != nil {
			bindUI(e, p.UI)
		}

		return e.Next()
	})

	p.App.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		p.Streams.Close()
		if p.Gateway != nil {
			p.Gateway.Stop()
		}
		if p.Metrics != nil {
			if err := p.Metrics.Close(); err != nil {
				p.Log.Error("failed to close the metrics database", "error", err)
			}
		}
		return e.Next()
	})

	p.App.Cron().MustAdd("pocketcloud_events_retention", "50 3 * * *", func() {
		if err := events.Prune(p.App, eventRetention); err != nil {
			p.Log.Error("event retention failed", "error", err)
		}
	})
}

// startGateway opens the internal CA and starts the gRPC gateway for agents.
func (p *Panel) startGateway() error {
	authority, err := ca.Open(p.App, p.Box, p.serverNames())
	if err != nil {
		return fmt.Errorf("the internal CA: %w", err)
	}
	p.Authority = authority

	if err := p.openMetrics(); err != nil {
		return err
	}

	p.Gateway = &gateway.Server{
		App:       p.App,
		Authority: authority,
		Commands:  p.Commands,
		Audit:     p.Audit,
		Inventory: &inventory.Store{App: p.App},
		Metrics:   p.Metrics,
		Streams:   p.Streams,
		Artifacts: p.artifactHandlers(),
	}
	p.Gateway.BindHostRemoval()

	for _, observe := range p.Observers {
		observe(p.Gateway)
	}

	if p.Config.GRPCAddr == "" {
		return nil
	}

	return p.Gateway.Start(p.Config.GRPCAddr)
}

// serverNames are the names in the gateway's certificate. Agents verify the
// panel by its pinned CA and not by name, but other tools may want them.
func (p *Panel) serverNames() []string {
	names := []string{}

	if u, err := url.Parse(p.Config.PublicURL); err == nil && u.Hostname() != "" {
		names = append(names, u.Hostname())
	}
	if host, _, err := net.SplitHostPort(p.Config.AgentEndpoint()); err == nil {
		names = append(names, host)
	}

	return names
}

// loadMasterKey refuses to start without a master key: every secret stored by
// the panel depends on it.
func (p *Panel) loadMasterKey() error {
	if p.Box != nil {
		return nil
	}

	key := p.Config.MasterKey
	if key == nil {
		var err error
		key, err = secrets.LoadKeyFile(p.Config.MasterKeyFile)
		if err != nil {
			if errors.Is(err, secrets.ErrKeyMissing) {
				return fmt.Errorf("%w\n\nThe panel does not start without a master key, because it encrypts every stored secret.\nCreate one with `panel keygen --out %s` and keep a copy outside this machine", err, p.Config.MasterKeyFile)
			}
			return err
		}
	}

	box, err := secrets.New(key)
	if err != nil {
		return err
	}
	p.Box = box

	return nil
}
