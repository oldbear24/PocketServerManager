package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
)

const (
	selfCheckTimeout = 10 * time.Second
	maxClockSkew     = 5 * time.Second
)

// selfChecks are what the agent verifies about itself at start and every few
// minutes. They explain a half-broken agent, so each one must fail on its own.
type selfChecks struct {
	agent *Agent

	mu     sync.Mutex
	latest []*agentv1.SelfCheck
}

func newSelfChecks(agent *Agent) *selfChecks {
	return &selfChecks{agent: agent}
}

func (s *selfChecks) run(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, selfCheckTimeout)
	defer cancel()

	results := []*agentv1.SelfCheck{
		s.docker(ctx),
		s.hostMount(),
		s.edge(ctx),
		s.restic(ctx),
		s.clock(),
	}

	s.mu.Lock()
	s.latest = results
	s.mu.Unlock()
}

func (s *selfChecks) results() []*agentv1.SelfCheck {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.latest)
}

func check(name string, err error, detail string) *agentv1.SelfCheck {
	result := &agentv1.SelfCheck{Name: name, Ok: err == nil, Detail: detail, CheckedAt: timestamppb.Now()}
	if err != nil {
		result.Detail = err.Error()
	}
	return result
}

func skipped(name, detail string) *agentv1.SelfCheck {
	return &agentv1.SelfCheck{Name: name, Ok: true, Skipped: true, Detail: detail, CheckedAt: timestamppb.Now()}
}

func (s *selfChecks) docker(ctx context.Context) *agentv1.SelfCheck {
	version, err := s.agent.Docker.Version(ctx)
	if err != nil {
		return check("docker", fmt.Errorf("the Docker socket does not answer: %w", err), "")
	}
	return check("docker", nil, "Docker "+version.Version)
}

func (s *selfChecks) hostMount() *agentv1.SelfCheck {
	root := s.agent.Config.HostRoot

	if _, err := os.Stat(filepath.Join(root, "proc", "stat")); err != nil {
		return check("host_mount", fmt.Errorf("the host is not mounted at %s: metrics and disk usage show the container's view", root), "")
	}

	// a writable host mount means the install command was changed: the agent
	// promises that it can only read the host
	probe, err := os.CreateTemp(root, ".pocketcloud-probe-*")
	if err == nil {
		name := probe.Name()
		probe.Close()
		_ = os.Remove(name)
		return check("host_mount", fmt.Errorf("the host mount at %s is writable: it must be mounted read-only", root), "")
	}

	return check("host_mount", nil, root+" is mounted read-only")
}

// edge checks the edge proxy of a server that runs one: the agent knows from
// its own state, as the roles an admin edits later never reach the agent.
func (s *selfChecks) edge(ctx context.Context) *agentv1.SelfCheck {
	state, err := s.agent.loadEdgeState()
	if err != nil {
		return check("edge", err, "")
	}
	if state == nil {
		return skipped("edge", "this server runs no edge proxy")
	}
	if state.Error != "" {
		return check("edge", errors.New(state.Error), "")
	}
	if len(state.Config) == 0 {
		return skipped("edge", "the edge proxy has no config yet")
	}

	info, err := s.agent.Docker.Inspect(ctx, edgeName)
	if err != nil {
		return check("edge", fmt.Errorf("the edge container is gone: %w", err), "")
	}
	if !info.State.Running {
		return check("edge", fmt.Errorf("the edge container is %s", info.State.Status), "")
	}
	if _, status, err := s.agent.edgeRequest(ctx, "GET", "/config/", nil, edgeProbeLimit); err != nil || status != 200 {
		if err == nil {
			err = fmt.Errorf("status %d", status)
		}
		return check("edge", fmt.Errorf("the edge's admin API does not answer: %w", err), "")
	}

	detail := fmt.Sprintf("the edge serves revision %d", state.Revision)
	if state.CaddyVersion != "" {
		detail = fmt.Sprintf("Caddy %s serves revision %d", state.CaddyVersion, state.Revision)
	}
	return check("edge", nil, detail)
}

// runEdge runs the edge's check again after the edge changed, so the next
// heartbeat says how it went.
func (s *selfChecks) runEdge(parent context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), selfCheckTimeout)
	defer cancel()

	result := s.edge(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.latest {
		if existing.GetName() == "edge" {
			s.latest[i] = result
			return
		}
	}
	s.latest = append(s.latest, result)
}

func (s *selfChecks) restic(ctx context.Context) *agentv1.SelfCheck {
	output, err := exec.CommandContext(ctx, "restic", "version").Output()
	if err != nil {
		return check("restic", fmt.Errorf("restic does not run: %w", err), "")
	}
	return check("restic", nil, strings.TrimSpace(string(output)))
}

func (s *selfChecks) clock() *agentv1.SelfCheck {
	skew := time.Duration(s.agent.lastSkew.Load()) * time.Microsecond

	if time.Duration(math.Abs(float64(skew))) > maxClockSkew {
		return check("time", fmt.Errorf("the clock differs from the panel's by %s: certificates and schedules depend on it", skew.Round(time.Millisecond)), "")
	}

	return check("time", nil, "the clock differs from the panel's by "+skew.Round(time.Millisecond).String())
}
