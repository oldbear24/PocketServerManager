package agent

import (
	"testing"

	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
)

func statsOf(cpuTotal, systemCPU, rx uint64) *dockerapi.Stats {
	var stats dockerapi.Stats
	stats.CPUStats.CPUUsage.TotalUsage = cpuTotal
	stats.CPUStats.SystemCPUUsage = systemCPU
	stats.MemoryStats.Usage = 300 << 20
	stats.MemoryStats.Limit = 1 << 30
	stats.Networks = map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	}{"eth0": {RxBytes: rx}}
	return &stats
}

// TestTheFirstContainerSampleIsOnlyABase: CPU and traffic are deltas, and a first
// sample without a base would be a zero that never happened, for every container
// after each restart of the agent.
func TestTheFirstContainerSampleIsOnlyABase(t *testing.T) {
	m := newMetricsCollector(nil)

	if sample, ok := m.containerSample("web", statsOf(1000, 10000, 500)); ok || sample != nil {
		t.Fatalf("the first sample was sent: %+v", sample)
	}

	sample, ok := m.containerSample("web", statsOf(1250, 12000, 800))
	if !ok {
		t.Fatal("the second sample was not sent")
	}
	if sample.GetCpuPercent() != 12.5 || sample.GetNetInBytes() != 300 || sample.GetMemBytes() != 300<<20 {
		t.Errorf("second sample: %+v", sample)
	}

	// a container that stopped starts over
	m.forgetGone(map[string]bool{})
	if _, ok := m.containerSample("web", statsOf(2000, 20000, 900)); ok {
		t.Error("a container seen again after it was gone sent a sample without a base")
	}
}

// TestContainerCPUIsOnTheScaleOfDockerStats: 100 % is one core, as in docker stats
// and top, so one busy core of four reads 100 % and not 25 %.
func TestContainerCPUIsOnTheScaleOfDockerStats(t *testing.T) {
	m := newMetricsCollector(nil)

	first := statsOf(1000, 10000, 0)
	first.CPUStats.OnlineCPUs = 4
	m.containerSample("web", first)

	// the host's counter covers all four cores, the container used a quarter of it
	second := statsOf(1250, 11000, 0)
	second.CPUStats.OnlineCPUs = 4
	sample, ok := m.containerSample("web", second)
	if !ok {
		t.Fatal("no sample")
	}
	if sample.GetCpuPercent() != 100 {
		t.Errorf("one busy core of four reads %v %%, want 100 %%", sample.GetCpuPercent())
	}

	// an engine that reports no online CPUs counts the per-CPU values, as docker stats does
	third := statsOf(1500, 12000, 0)
	third.CPUStats.CPUUsage.PercpuUsage = []uint64{1, 2}
	sample, _ = m.containerSample("web", third)
	if sample.GetCpuPercent() != 50 {
		t.Errorf("with two per-CPU values a quarter of the host reads %v %%, want 50 %%", sample.GetCpuPercent())
	}
}

// TestRequestsCountOncePerRequest: the pinned Caddy counts a request once,
// under the first handler of its route, whatever that handler is; this is
// what Caddy 2.11.7 reported for a site with compression, a proxy with HSTS
// and a redirect to HTTPS.
func TestRequestsCountOncePerRequest(t *testing.T) {
	metrics := []byte(`# HELP caddy_http_requests_total Counter of HTTP(S) requests made.
# TYPE caddy_http_requests_total counter
caddy_http_requests_total{handler="encode",host="hugo.example.test",server="http"} 5
caddy_http_requests_total{handler="headers",host="app.example.test",server="https"} 7
caddy_http_requests_total{handler="static_response",host="_other",server="http"} 3
caddy_http_request_duration_seconds_count{code="200",handler="encode",host="hugo.example.test",method="GET",server="http"} 5
`)
	total, byHost := countRequests(metrics)
	if total != 15 {
		t.Errorf("counted %v requests, want 15", total)
	}
	if byHost["hugo.example.test"] != 5 || byHost["app.example.test"] != 7 || len(byHost) != 2 {
		t.Errorf("by host: %v", byHost)
	}
}

// TestRequestsAreCountedPerHostname: with per_host metrics each hostname has
// its own counters, and a heartbeat reports what each had since the last one.
func TestRequestsAreCountedPerHostname(t *testing.T) {
	metrics := []byte(`caddy_http_requests_total{handler="subroute",host="blog.example.com",server="https"} 40
caddy_http_requests_total{handler="file_server",host="Docs.example.com",server="http"} 5
caddy_http_requests_total{handler="reverse_proxy",host="app.example.com",server="https"} 9
`)
	total, byHost := countRequests(metrics)
	if total != 54 || byHost["blog.example.com"] != 40 || byHost["docs.example.com"] != 5 || byHost["app.example.com"] != 9 {
		t.Errorf("total %v, by host %v", total, byHost)
	}

	since := requestsSince(map[string]float64{"blog.example.com": 30, "app.example.com": 20, "docs.example.com": 5}, byHost)
	// app's counter went down: the edge restarted, and counted 9 since
	if since["blog.example.com"] != 10 || since["app.example.com"] != 9 || len(since) != 2 {
		t.Errorf("since the last count: %v", since)
	}
}
