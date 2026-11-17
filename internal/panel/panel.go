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
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/ca"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/dns"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/gateway"
	"github.com/oldbear24/PocketServerManager/internal/inventory"
	"github.com/oldbear24/PocketServerManager/internal/metrics"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/streams"
	"github.com/oldbear24/PocketServerManager/internal/version"
	"github.com/oldbear24/PocketServerManager/plugins/diagnostics"
)

const eventRetention = 30 * 24 * time.Hour

// offlineGrace is how long after its start the gateway marks no server
// offline: agents back off for up to a minute, with jitter, while the panel
// is down (#32).
const offlineGrace = 90 * time.Second

// Panel holds the components of a running panel.
type Panel struct {
	App    core.App
	Config *Config
	UI     fs.FS
	Log    *slog.Logger

	// edgeImage is the edge proxy image the panel gives its edges: the one
	// pinned in this release, another in tests (SetEdgeImage).
	edgeImage atomic.Pointer[string]

	// https is the panel's own HTTPS (#63)
	https httpsState

	Box *secrets.Box
	// masterKey is the master key Box was made from: the keys of the
	// panel's copy of itself derive from it, and the recovery kit holds it
	// (#32).
	masterKey []byte
	// serveLock is the data directory's serve lock, held while the panel
	// serves (#32).
	serveLock *os.File

	Audit     *audit.Writer
	Authority *ca.Authority
	// authority is the same CA, for the work that may start before the
	// gateway: certificates for LAN names
	authority atomic.Pointer[ca.Authority]
	Commands  *commands.Manager
	Gateway   *gateway.Server
	Updates   *updateBatches
	Metrics   *metrics.Store
	Streams   *streams.Broker

	// Diagnostics is the diagnostics plugin, if it was enabled.
	Diagnostics *diagnostics.Plugin

	// TerminalIdle closes a terminal without input or output; zero is the default.
	TerminalIdle time.Duration
	// AdminerTTL is how long an Adminer session lasts; zero is AdminerLifetime.
	AdminerTTL time.Duration
	// BackupCheckWait is how long adding or checking a backup target waits
	// for the agent; zero is two minutes.
	BackupCheckWait time.Duration
	// ListingWait is how long a request for snapshots or a snapshot's folder
	// waits for the agent before it answers that the listing is pending;
	// zero is 25 seconds.
	ListingWait time.Duration
	// DownloadFirstByte is how long a download of a snapshot waits for the
	// agent's first bytes; zero is 120 seconds.
	DownloadFirstByte time.Duration

	terminalTickets terminalTickets
	adminers        adminerSessions
	// siteConnects connects a site to a database one call at a time: a new
	// site's database end and its setup's next step may both connect it
	siteConnects keyedLocks
	later        laterWork

	// snapshotCache and browseCache keep what listings of snapshots, and of
	// a snapshot's folders, gave (#30)
	snapshotCache listingCache
	browseCache   listingCache
	// downloadTickets and downloads are the downloads of snapshots: their
	// links, and those under way (#30)
	downloadTickets downloadTickets
	downloads       downloadSessions
	// exports are the panel's encrypted copies of itself that its backup
	// job's carrier fetches (#32)
	exports panelExports
	// changes is the panel's backup after a change of its servers (#32)
	changes changeBackups

	auditKeyOnce sync.Once
	auditKey     []byte

	edgeLocks edgeLocks
	// dnsLocks serialise the DNS work of each domain.
	dnsLocks edgeLocks
	// DNSChecker asks DNS servers whether records have propagated; tests set it.
	DNSChecker atomic.Pointer[dns.Checker]
	// acme is the ACME work: certificates from a CA (#23).
	acme acmeState
	// provision runs the setups of new sites (#24)
	provision provisionState
	// ManualEdges stops the panel from sending edges their configs by itself,
	// for tests that send edge.apply themselves.
	ManualEdges atomic.Bool

	// started is when the panel started, in Unix nanoseconds (see
	// SetStarted).
	started atomic.Int64
	// backupCatchUp is set until the backup scheduler's first pass, which
	// catches up the runs the panel missed while it was down.
	backupCatchUp atomic.Bool
	// backupPass holds a pass of the backup scheduler, retries included
	// (#31): PocketBase's cron starts a pass every minute whether the one
	// before has ended or not.
	backupPass sync.Mutex
	// checkPass holds a pass of the integrity checks' scheduler (#31).
	checkPass sync.Mutex

	// restoreWatch is set from a restored panel's first start until every
	// server is back (#32); restoreRecount while a count of them is due.
	restoreWatch   atomic.Bool
	restoreRecount atomic.Bool
	// recoveryPending is set while an evaluation of the recovery's alerts
	// is due; recoveryEval holds one (#32).
	recoveryPending atomic.Bool
	recoveryEval    sync.Mutex
	// kitAttempts limits the attempts at the recovery kit's routes, and
	// kitSealing lets one argon2 run at a time (#32).
	kitAttempts kitAttempts
	kitSealing  chan struct{}

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

		kitSealing: make(chan struct{}, 1),
	}
	p.SetEdgeImage(version.EdgeRef())
	p.SetStarted(time.Now())
	p.acme.init()
	p.Updates = newUpdateBatches(p)
	p.Streams = streams.New(p.Commands)
	p.Audit.SealKey = p.AuditKey
	return p
}

// SetEdgeImage changes the edge proxy image, as a release that pins another
// Caddy does. Edges get it with their next config (see RefreshEdges).
func (p *Panel) SetEdgeImage(ref string) { p.edgeImage.Store(&ref) }

// EdgeImage is the edge proxy image the panel gives its edges.
func (p *Panel) EdgeImage() string { return *p.edgeImage.Load() }

// AuditKey returns the key that seals the head of the audit chain, derived from
// the master key. Commands that do not start the panel, such as migrations, load
// the key too, so that their entries are sealed as well; without a readable key
// it is nil and the head waits for the next start of the panel.
func (p *Panel) AuditKey() []byte {
	p.auditKeyOnce.Do(func() {
		master := p.Config.MasterKey
		if master == nil && p.Config.MasterKeyFile != "" {
			loaded, err := secrets.LoadKeyFile(p.Config.MasterKeyFile)
			if err != nil {
				return
			}
			master = loaded
		}
		if master == nil {
			return
		}
		key, err := secrets.DeriveKey(master, audit.SealPurpose)
		if err != nil {
			p.Log.Error("cannot derive the key of the audit seal", "error", err)
			return
		}
		p.auditKey = key
	})
	return p.auditKey
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
	p.registerDatabases()
	p.registerMoves()
	p.registerEdge()
	p.registerDNS()
	p.registerACME()
	p.registerHTTPS()
	p.registerLabels()
	p.registerSites()
	p.registerPHPSites()
	p.registerTasks()
	p.registerProvision()
	p.registerBackups()
	p.registerBackupJobs()
	p.registerBackupRestores()
	p.registerBackupChecks()
	// the panel's backup of itself and its recovery (#32)
	p.registerPanelExports()
	p.registerChangeBackups()
	p.registerPocketBaseBackups()
	p.registerRecovery()

	p.App.OnServe().BindFunc(func(e *core.ServeEvent) error {
		// PocketBase's default installer opens the machine's default browser with a
		// link that holds a superuser token. A panel usually runs on a server and
		// must never start programs by itself, so this stays off; the first account
		// comes from the setup link, and a superuser from the command line.
		e.InstallerFunc = nil

		// one panel per pb_data (#32)
		if err := p.takeServeLock(); err != nil {
			return err
		}
		if err := p.loadMasterKey(); err != nil {
			return err
		}
		// the key that sealed the database, before anything is written
		if err := p.checkKeyID(); err != nil {
			return err
		}

		// entries written by commands that ran without the master key
		if err := p.Audit.SealHead(); err != nil {
			return fmt.Errorf("sealing the audit log: %w", err)
		}

		// what a panel that stopped left in the spool of its copies, before
		// the gateway serves one (#32)
		p.sweepPanelExports(time.Now())

		if err := p.startGateway(); err != nil {
			return err
		}
		// with the internal CA open, whose certificate it serves first (#63)
		if err := p.startHTTPS(e); err != nil {
			return err
		}
		// a new release may give the edges another config
		p.later.after(edgeSettle, p.RefreshEdges)

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
		p.bindDatabaseRoutes(group)
		p.bindEdgeRoutes(group)
		p.bindDNSRoutes(group)
		p.bindACMERoutes(group)
		p.bindLabelRoutes(group)
		p.bindSiteRoutes(group)
		p.bindTaskRoutes(group)
		p.bindMailRoutes(group)
		p.bindProvisionRoutes(group)
		p.bindLANRoutes(group)
		p.bindBackupRoutes(group)
		p.bindBackupJobRoutes(group)
		p.bindBackupRestoreRoutes(group)
		p.bindBackupCheckRoutes(group)
		p.bindRecoveryRoutes(group)
		// setups of new sites that a restart interrupted go on
		p.later.after(edgeSettle, p.resumeProvisions)
		// restores whose end the panel missed while it was down are settled,
		// before any request: no download's browser is left after a restart
		p.settleRestores()
		p.Commands.BindRoutes(group)
		p.bindPublicRoutes(e)

		if p.UI != nil {
			bindUI(e, p.UI)
		}

		// the recovery's alerts, with the gateway started (#32)
		p.evaluateRecoveryAlerts()

		return e.Next()
	})

	p.App.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		// issuances under way stop waiting for answers
		p.stopACME()
		p.stopHTTPS()
		p.later.stop()
		p.Streams.Close()
		if p.Gateway != nil {
			p.Gateway.Stop()
		}
		// after the gateway, whose handlers deliver commands too
		p.Commands.Close()
		if p.Metrics != nil {
			if err := p.Metrics.Close(); err != nil {
				p.Log.Error("failed to close the metrics database", "error", err)
			}
		}
		p.releaseServeLock()
		return e.Next()
	})

	// bundles are kept for a week (see the gateway's bundleTTL)
	p.App.Cron().MustAdd("pocketcloud_diagnostic_bundles_retention", "55 3 * * *", p.PruneBundles)

	p.App.Cron().MustAdd("pocketcloud_events_retention", "50 3 * * *", func() {
		if err := events.Prune(p.App, eventRetention); err != nil {
			p.Log.Error("event retention failed", "error", err)
		}
	})
}

// startGateway opens the internal CA and starts the gRPC gateway for agents.
// The first start after `panel recover` finishes the restore before the
// gateway listens, so nothing stale reaches a server (#32).
func (p *Panel) startGateway() error {
	authority, err := p.openAuthority()
	if err != nil {
		// a refusal says itself what is wrong
		if refused := ca.Refusal(""); errors.As(err, &refused) {
			return err
		}
		return fmt.Errorf("the internal CA: %w", err)
	}
	p.Authority = authority
	p.authority.Store(authority)

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
	p.Gateway.OnEdgeStatus = p.onEdgeStatus
	p.Gateway.OnRequests = p.onRequests
	p.Gateway.OnLatency = p.onLatency
	p.Gateway.Fetch = p.fetchArtifact
	// a download of a snapshot goes on to the browser as it arrives (#30)
	p.Gateway.LiveArtifacts = map[string]gateway.LiveArtifactHandler{artifactBackupDownload: p.serveDownload}
	// agents still backing off are not offline yet (#32)
	p.Gateway.StartGrace = offlineGrace

	for _, observe := range p.Observers {
		observe(p.Gateway)
	}

	// the first start after `panel recover`, before anything reaches a
	// server (#32)
	if err := p.finishRestore(); err != nil {
		return err
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
	if p.Box != nil && p.masterKey != nil {
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
	p.masterKey = key

	return nil
}
