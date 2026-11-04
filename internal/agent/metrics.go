package agent

import (
	"context"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
)

const (
	statsConcurrency = 8
	statsTimeout     = 8 * time.Second
)

// metricsCollector samples the host and its containers. CPU usage and network
// traffic are deltas, so it remembers the previous sample of each container. The
// first sample of a container is only that base: sent, it would be a zero that
// never happened, for every container after each restart of the agent.
type metricsCollector struct {
	agent *Agent

	mu       sync.Mutex
	previous map[string]containerSample
}

type containerSample struct {
	cpuTotal  uint64
	systemCPU uint64
	netIn     uint64
	netOut    uint64
}

func newMetricsCollector(agent *Agent) *metricsCollector {
	return &metricsCollector{agent: agent, previous: make(map[string]containerSample)}
}

func (m *metricsCollector) loop(ctx context.Context, client agentv1.AgentServiceClient) error {
	stream, err := client.PushMetrics(ctx)
	if err != nil {
		return err
	}

	ticker := time.NewTicker(m.agent.Config.MetricsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			samples := m.collect(ctx)
			if err := stream.Send(&agentv1.PushMetricsRequest{Samples: samples}); err != nil {
				return err
			}
		}
	}
}

func (m *metricsCollector) collect(ctx context.Context) []*agentv1.MetricSample {
	now := timestamppb.Now()

	vitals := m.agent.host.vitals()
	netIn, netOut := m.agent.host.netDelta()

	samples := []*agentv1.MetricSample{{
		Ts:             now,
		CpuPercent:     vitals.GetCpuPercent(),
		MemBytes:       vitals.GetMemUsedBytes(),
		MemTotalBytes:  vitals.GetMemTotalBytes(),
		DiskUsedBytes:  vitals.GetDiskUsedBytes(),
		DiskTotalBytes: vitals.GetDiskTotalBytes(),
		NetInBytes:     netIn,
		NetOutBytes:    netOut,
		Load1:          vitals.GetLoad1(),
		Mounts:         m.agent.host.mounts(),
	}}

	containers, err := m.agent.Docker.Containers(ctx)
	if err != nil {
		return samples
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		limiter = make(chan struct{}, statsConcurrency)
		running = make(map[string]bool, len(containers))
	)

	for _, container := range containers {
		if container.State != "running" {
			continue
		}
		running[container.ID] = true

		wg.Add(1)
		limiter <- struct{}{}

		go func(id string) {
			defer wg.Done()
			defer func() { <-limiter }()

			statsCtx, cancel := context.WithTimeout(ctx, statsTimeout)
			defer cancel()

			stats, err := m.agent.Docker.ContainerStats(statsCtx, id)
			if err != nil {
				return
			}

			sample, ok := m.containerSample(id, stats)
			if !ok {
				return
			}
			sample.Ts = now

			mu.Lock()
			samples = append(samples, sample)
			mu.Unlock()
		}(container.ID)
	}
	wg.Wait()

	m.forgetGone(running)

	return samples
}

// containerSample computes a container's sample from its statistics and the
// previous ones; without previous ones there is none yet.
func (m *metricsCollector) containerSample(id string, stats *dockerapi.Stats) (*agentv1.MetricSample, bool) {
	current := containerSample{
		cpuTotal:  stats.CPUStats.CPUUsage.TotalUsage,
		systemCPU: stats.CPUStats.SystemCPUUsage,
	}
	for _, network := range stats.Networks {
		current.netIn += network.RxBytes
		current.netOut += network.TxBytes
	}

	m.mu.Lock()
	previous, known := m.previous[id]
	m.previous[id] = current
	m.mu.Unlock()

	if !known {
		return nil, false
	}

	sample := &agentv1.MetricSample{
		ContainerId:   id,
		MemBytes:      containerMemory(stats),
		MemTotalBytes: stats.MemoryStats.Limit,
		CpuPercent:    containerCPUPercent(previous, current),
	}
	if current.netIn >= previous.netIn {
		sample.NetInBytes = current.netIn - previous.netIn
	}
	if current.netOut >= previous.netOut {
		sample.NetOutBytes = current.netOut - previous.netOut
	}

	return sample, true
}

// containerCPUPercent is relative to the whole host, like the host's own value.
func containerCPUPercent(previous, current containerSample) float64 {
	if current.systemCPU <= previous.systemCPU || current.cpuTotal < previous.cpuTotal {
		return 0
	}
	return float64(current.cpuTotal-previous.cpuTotal) / float64(current.systemCPU-previous.systemCPU) * 100
}

// containerMemory subtracts the page cache, like `docker stats` does.
func containerMemory(stats *dockerapi.Stats) uint64 {
	usage := stats.MemoryStats.Usage

	cache := stats.MemoryStats.Stats["inactive_file"] // cgroup v2
	if cache == 0 {
		cache = stats.MemoryStats.Stats["total_inactive_file"] // cgroup v1
	}

	if cache < usage {
		return usage - cache
	}

	return usage
}

func (m *metricsCollector) forgetGone(running map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id := range m.previous {
		if !running[id] {
			delete(m.previous, id)
		}
	}
}
