// Package agent is the program on every managed server. It dials out to the
// panel, executes commands and reports what happens on the host.
package agent

import (
	"os"
	"strings"
	"time"
)

const (
	defaultStateDir = "/var/lib/pocketcloud"
	defaultHostRoot = "/host"

	defaultDockerCLI = "docker"
)

// Config comes from the container's environment.
type Config struct {
	// Panel is the gateway's host:port.
	Panel string
	// CAPin is the fingerprint of the panel's root certificate.
	CAPin string
	// Token is the one-time enrollment token; useless once it has been used.
	Token string
	// Name and Roles override what the token says, if set.
	Name  string
	Roles []string

	// StateDir is the agent volume.
	StateDir string
	// HostRoot is where the host's root is mounted read-only.
	HostRoot string
	// JobImageRoot is where the agent sees the files of the image its
	// backup jobs run from, which is its own: its own / unless set. Only
	// tests set it.
	JobImageRoot string
	// DockerHost is the Docker Engine to use.
	DockerHost string
	// DockerCLI is the Docker command line tool, which brings the Compose plugin.
	DockerCLI string

	// ContainerName is the name of the agent's own container.
	ContainerName string

	// LocalSites, when set, is a directory that stands in for the sites
	// volume: site deployments run there without Docker. Only tests set it.
	LocalSites string

	// HostShell allows terminals on the host itself, which need a privileged helper
	// container. It is off unless HOST_SHELL_ENABLED=1 is set on this agent.
	HostShell bool

	// HeartbeatInterval is the time between heartbeats.
	HeartbeatInterval time.Duration
	// InventoryInterval is the time between complete inventory reports.
	InventoryInterval time.Duration
	// MetricsInterval is the time between metric samples. Zero turns them off.
	MetricsInterval time.Duration
	// SelfCheckInterval is the time between self-checks.
	SelfCheckInterval time.Duration
	// BackoffMin and BackoffMax bound the wait between connection attempts.
	BackoffMin time.Duration
	BackoffMax time.Duration
	// KeepaliveTime is the time between gRPC pings, tuned for NAT timeouts.
	KeepaliveTime time.Duration
}

// ConfigFromEnv reads the configuration from the environment.
func ConfigFromEnv() Config {
	config := Config{
		Panel:             os.Getenv("PANEL"),
		CAPin:             os.Getenv("CA_PIN"),
		Token:             os.Getenv("TOKEN"),
		Name:              os.Getenv("NAME"),
		Roles:             splitList(os.Getenv("ROLES")),
		StateDir:          envOr("STATE_DIR", defaultStateDir),
		HostRoot:          envOr("HOST_ROOT", defaultHostRoot),
		DockerHost:        os.Getenv("DOCKER_HOST"),
		DockerCLI:         envOr("DOCKER_CLI", defaultDockerCLI),
		ContainerName:     envOr("AGENT_CONTAINER", "pocketcloud-agent"),
		HostShell:         os.Getenv(hostShellEnv) == "1",
		HeartbeatInterval: 10 * time.Second,
		InventoryInterval: 5 * time.Minute,
		MetricsInterval:   10 * time.Second,
		SelfCheckInterval: 5 * time.Minute,
		BackoffMin:        time.Second,
		BackoffMax:        time.Minute,
		KeepaliveTime:     30 * time.Second,
	}
	return config
}

func (c Config) dockerCLI() string {
	if c.DockerCLI != "" {
		return c.DockerCLI
	}
	return defaultDockerCLI
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func splitList(value string) []string {
	result := []string{}
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}
