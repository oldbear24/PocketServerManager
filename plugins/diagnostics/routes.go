package diagnostics

import (
	"archive/zip"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"slices"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

func (p *Plugin) bindRoutes(se *core.ServeEvent) {
	group := se.Router.Group("/api/diag")
	group.Bind(apis.RequireAuth())
	group.BindFunc(p.authorize)

	group.GET("/overview", func(e *core.RequestEvent) error {
		return e.JSON(http.StatusOK, p.Overview(windowFrom(e)))
	})
	group.GET("/agents", func(e *core.RequestEvent) error {
		return e.JSON(http.StatusOK, p.Agents())
	})
	group.GET("/agents/{id}", func(e *core.RequestEvent) error {
		detail, ok := p.Agent(e.Request.PathValue("id"))
		if !ok {
			return e.NotFoundError("This agent has not connected since the panel started.", nil)
		}
		return e.JSON(http.StatusOK, detail)
	})
	group.POST("/agents/{id}/capture", func(e *core.RequestEvent) error {
		return e.JSON(http.StatusOK, map[string]any{"until": p.StartCapture(e.Request.PathValue("id"))})
	})
	group.GET("/trace", p.handleTrace)
	group.GET("/bundle", p.handleBundle)

	if p.config.MetricsToken != "" {
		se.Router.GET("/metrics", p.handleMetrics)
	}
}

func (p *Plugin) authorize(e *core.RequestEvent) error {
	if p.config.Authorize != nil {
		if err := p.config.Authorize(e); err != nil {
			return err
		}
		return e.Next()
	}
	if !e.HasSuperuserAuth() {
		return e.ForbiddenError("Only superusers can read diagnostics.", nil)
	}
	return e.Next()
}

func windowFrom(e *core.RequestEvent) time.Duration {
	switch e.Request.URL.Query().Get("window") {
	case "1h":
		return time.Hour
	case "24h":
		return 24 * time.Hour
	}
	return 15 * time.Minute
}

// Overview is the summary of the whole panel.
type Overview struct {
	Gateway  GatewayStats  `json:"gateway"`
	Messages MessageStats  `json:"messages"`
	Queue    QueueOverview `json:"queue"`
	Process  ProcessStats  `json:"process"`
	Errors   []PanelError  `json:"errors"`
	Cron     []CronJob     `json:"cron"`
}

// GatewayStats describes the connected agents.
type GatewayStats struct {
	AgentsConnected int `json:"agents_connected"`
	AgentsKnown     int `json:"agents_known"`
}

// MessageStats describes the message rate over the chosen window.
type MessageStats struct {
	PerMinute float64 `json:"per_minute"`
	WindowS   int     `json:"window_s"`
}

// QueueOverview describes the command queue.
type QueueOverview struct {
	Depth          map[string]int          `json:"depth"`
	OldestQueuedMS int64                   `json:"oldest_queued_ms"`
	Retries        uint64                  `json:"retries"`
	Replays        uint64                  `json:"replays"`
	Timeouts       uint64                  `json:"timeouts"`
	Latency        map[string]*latencyPair `json:"latency"`
}

// ProcessStats describes the panel process.
type ProcessStats struct {
	UptimeS         int64  `json:"uptime_s"`
	Goroutines      int    `json:"goroutines"`
	HeapBytes       uint64 `json:"heap_bytes"`
	TotalMemBytes   uint64 `json:"total_mem_bytes"`
	GCCycles        uint64 `json:"gc_cycles"`
	RealtimeClients int    `json:"realtime_clients"`
	Streams         int    `json:"streams"`
	SQLiteBytes     int64  `json:"sqlite_bytes"`
	WALBytes        int64  `json:"wal_bytes"`
}

// CronJob is one scheduled job of the panel.
type CronJob struct {
	ID         string `json:"id"`
	Expression string `json:"expression"`
}

// Overview gathers the summary for a time window.
func (p *Plugin) Overview(window time.Duration) Overview {
	agents := p.Agents()

	connected := 0
	for _, agent := range agents {
		if agent.Connected {
			connected++
		}
	}

	overview := Overview{
		Gateway:  GatewayStats{AgentsConnected: connected, AgentsKnown: len(agents)},
		Messages: MessageStats{PerMinute: p.rate.perMinute(time.Now(), window), WindowS: int(window.Seconds())},
		Process:  p.processStats(),
		Errors:   p.recentErrors(),
		Queue:    p.queueOverview(),
	}

	for _, job := range p.app.Cron().Jobs() {
		overview.Cron = append(overview.Cron, CronJob{ID: job.Id(), Expression: job.Expression()})
	}
	slices.SortFunc(overview.Cron, func(a, b CronJob) int { return strings.Compare(a.ID, b.ID) })

	return overview
}

func (p *Plugin) queueOverview() QueueOverview {
	queue := QueueOverview{Depth: map[string]int{}}

	if p.config.QueueSnapshot != nil {
		snapshot := p.config.QueueSnapshot()
		queue.Depth = snapshot.Depth
		queue.OldestQueuedMS = snapshot.OldestQueued.Milliseconds()
	}

	p.queue.mu.Lock()
	defer p.queue.mu.Unlock()

	queue.Retries = p.queue.retries
	queue.Replays = p.queue.replays
	queue.Timeouts = p.queue.timeouts
	queue.Latency = make(map[string]*latencyPair, len(p.queue.perType))
	for commandType, pair := range p.queue.perType {
		copied := *pair
		queue.Latency[commandType] = &copied
	}

	return queue
}

var processMetricNames = []string{
	"/memory/classes/heap/objects:bytes",
	"/memory/classes/total:bytes",
	"/gc/cycles/total:gc-cycles",
	"/sched/goroutines:goroutines",
}

func (p *Plugin) processStats() ProcessStats {
	samples := make([]metrics.Sample, len(processMetricNames))
	for i, name := range processMetricNames {
		samples[i].Name = name
	}
	metrics.Read(samples)

	value := func(index int) uint64 {
		if samples[index].Value.Kind() == metrics.KindUint64 {
			return samples[index].Value.Uint64()
		}
		return 0
	}

	stats := ProcessStats{
		UptimeS:         int64(time.Since(p.started).Seconds()),
		Goroutines:      int(value(3)), //nolint:gosec // a small number
		HeapBytes:       value(0),
		TotalMemBytes:   value(1),
		GCCycles:        value(2),
		RealtimeClients: p.app.SubscriptionsBroker().TotalClients(),
	}

	if p.config.Streams != nil {
		stats.Streams = p.config.Streams()
	}

	stats.SQLiteBytes = fileSize(filepath.Join(p.app.DataDir(), "data.db"))
	stats.WALBytes = fileSize(filepath.Join(p.app.DataDir(), "data.db-wal"))

	return stats
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// handleTrace streams trace entries over SSE until the client leaves.
func (p *Plugin) handleTrace(e *core.RequestEvent) error {
	entries, stop := p.subscribe(e.Request.URL.Query().Get("agent"))
	defer stop()

	e.Response.Header().Set("Content-Type", "text/event-stream")
	e.Response.Header().Set("Cache-Control", "no-cache")
	e.Response.Header().Set("X-Accel-Buffering", "no")
	e.Response.WriteHeader(http.StatusOK)

	flush := func() {
		if err := e.Flush(); err != nil {
			p.app.Logger().Debug("diagnostics: flushing the trace failed", "error", err)
		}
	}
	flush()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-e.Request.Context().Done():
			return nil
		case <-keepalive.C:
			if _, err := fmt.Fprint(e.Response, ": keepalive\n\n"); err != nil {
				return nil //nolint:nilerr // the client left
			}
			flush()
		case entry := <-entries:
			raw, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(e.Response, "data: %s\n\n", raw); err != nil {
				return nil //nolint:nilerr // the client left
			}
			flush()
		}
	}
}

// handleBundle sends the panel's debug bundle: configuration without secrets,
// versions, recent errors and a goroutine dump.
func (p *Plugin) handleBundle(e *core.RequestEvent) error {
	e.Response.Header().Set("Content-Type", "application/zip")
	e.Response.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="panel-diagnostics-%s.zip"`, time.Now().UTC().Format("20060102-150405")))
	e.Response.WriteHeader(http.StatusOK)

	archive := zip.NewWriter(e.Response)
	defer archive.Close()

	addJSON := func(name string, value any) error {
		writer, err := archive.Create(name)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	}

	version := map[string]any{"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH}
	config := map[string]any{}
	if p.config.ConfigDump != nil {
		config = p.config.ConfigDump()
	}

	for name, value := range map[string]any{
		"versions.json": version,
		"config.json":   config,
		"overview.json": p.Overview(24 * time.Hour),
		"agents.json":   p.Agents(),
		"errors.json":   p.recentErrors(),
	} {
		if err := addJSON(name, value); err != nil {
			return err
		}
	}

	writer, err := archive.Create("goroutines.txt")
	if err != nil {
		return err
	}

	return pprof.Lookup("goroutine").WriteTo(writer, 1)
}

// handleMetrics serves the Prometheus text format.
func (p *Plugin) handleMetrics(e *core.RequestEvent) error {
	token := strings.TrimPrefix(e.Request.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(token), []byte(p.config.MetricsToken)) != 1 {
		return e.UnauthorizedError("A valid metrics token is required.", nil)
	}

	var out strings.Builder

	write := func(name, kind, help string, value float64, labels string) {
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s %s\n%s%s %g\n", name, help, name, kind, name, labels, value)
	}

	overview := p.Overview(15 * time.Minute)

	write("pocketcloud_agents_connected", "gauge", "Agents with an open connection.", float64(overview.Gateway.AgentsConnected), "")
	write("pocketcloud_agents_known", "gauge", "Agents seen since the panel started.", float64(overview.Gateway.AgentsKnown), "")
	write("pocketcloud_messages_per_minute", "gauge", "gRPC messages per minute over 15 minutes.", overview.Messages.PerMinute, "")
	write("pocketcloud_queue_oldest_seconds", "gauge", "Age of the oldest queued command.", float64(overview.Queue.OldestQueuedMS)/1000, "")
	write("pocketcloud_queue_retries_total", "counter", "Command retries.", float64(overview.Queue.Retries), "")
	write("pocketcloud_queue_replays_total", "counter", "Commands replayed after a reconnect.", float64(overview.Queue.Replays), "")
	write("pocketcloud_queue_timeouts_total", "counter", "Commands that timed out.", float64(overview.Queue.Timeouts), "")
	write("pocketcloud_process_goroutines", "gauge", "Goroutines of the panel.", float64(overview.Process.Goroutines), "")
	write("pocketcloud_process_heap_bytes", "gauge", "Live heap of the panel.", float64(overview.Process.HeapBytes), "")
	write("pocketcloud_process_uptime_seconds", "counter", "Uptime of the panel.", float64(overview.Process.UptimeS), "")
	write("pocketcloud_realtime_clients", "gauge", "PocketBase realtime clients.", float64(overview.Process.RealtimeClients), "")
	write("pocketcloud_relay_streams", "gauge", "Open log and terminal relays.", float64(overview.Process.Streams), "")
	write("pocketcloud_sqlite_bytes", "gauge", "Size of the main database.", float64(overview.Process.SQLiteBytes), "")
	write("pocketcloud_sqlite_wal_bytes", "gauge", "Size of the write-ahead log.", float64(overview.Process.WALBytes), "")

	states := make([]string, 0, len(overview.Queue.Depth))
	for state := range overview.Queue.Depth {
		states = append(states, state)
	}
	slices.Sort(states)

	out.WriteString("# HELP pocketcloud_queue_depth Commands per state.\n# TYPE pocketcloud_queue_depth gauge\n")
	for _, state := range states {
		fmt.Fprintf(&out, "pocketcloud_queue_depth{state=%q} %d\n", state, overview.Queue.Depth[state])
	}

	out.WriteString("# HELP pocketcloud_agent_up Whether an agent is connected.\n# TYPE pocketcloud_agent_up gauge\n")
	for _, agent := range p.Agents() {
		up := 0
		if agent.Connected {
			up = 1
		}
		fmt.Fprintf(&out, "pocketcloud_agent_up{agent=%q} %d\n", agent.ID, up)
	}

	e.Response.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, err := e.Response.Write([]byte(out.String()))

	return err
}
