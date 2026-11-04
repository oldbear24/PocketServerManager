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
