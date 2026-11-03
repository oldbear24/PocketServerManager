package panel

import (
	"net"
	"net/url"
	"os"

	"github.com/spf13/cobra"
)

// Environment variables that configure the panel; flags take precedence.
const (
	EnvPublicURL     = "POCKETCLOUD_PUBLIC_URL"
	EnvGRPCAddr      = "POCKETCLOUD_GRPC_ADDR"
	EnvGRPCPublic    = "POCKETCLOUD_GRPC_PUBLIC_ADDR"
	EnvMasterKeyFile = "POCKETCLOUD_MASTER_KEY_FILE"
	EnvDataDir       = "POCKETCLOUD_DATA_DIR"

	defaultGRPCAddr      = ":9443"
	defaultMasterKeyFile = "master.key"
)

// Config holds the panel's own configuration, next to PocketBase's flags.
type Config struct {
	// PublicURL is the URL people use to open the panel.
	PublicURL string
	// GRPCAddr is the address the gRPC gateway listens on.
	GRPCAddr string
	// GRPCPublicAddr is the host:port agents dial; derived from PublicURL if empty.
	GRPCPublicAddr string
	// MasterKeyFile is the file with the master key, kept outside the database.
	MasterKeyFile string
	// MasterKey is used instead of MasterKeyFile in tests.
	MasterKey []byte
}

// ConfigFromEnv returns the defaults, overridden by the environment.
func ConfigFromEnv() Config {
	return Config{
		PublicURL:      os.Getenv(EnvPublicURL),
		GRPCAddr:       envOr(EnvGRPCAddr, defaultGRPCAddr),
		GRPCPublicAddr: os.Getenv(EnvGRPCPublic),
		MasterKeyFile:  envOr(EnvMasterKeyFile, defaultMasterKeyFile),
	}
}

// BindFlags adds the panel's flags to the root command.
func (c *Config) BindFlags(cmd *cobra.Command) {
	flags := cmd.PersistentFlags()
	flags.StringVar(&c.PublicURL, "public-url", c.PublicURL, "public URL of the panel, e.g. https://panel.example.com")
	flags.StringVar(&c.GRPCAddr, "grpc-addr", c.GRPCAddr, "address the gRPC gateway for agents listens on")
	flags.StringVar(&c.GRPCPublicAddr, "grpc-public-addr", c.GRPCPublicAddr, "host:port agents dial (default: the public URL's host with the gRPC port)")
	flags.StringVar(&c.MasterKeyFile, "master-key-file", c.MasterKeyFile, "file with the master key; create one with: panel keygen")
}

// AgentEndpoint returns the host:port that agents dial.
func (c *Config) AgentEndpoint() string {
	return c.AgentEndpointFor("localhost")
}

// AgentEndpointFor is AgentEndpoint for a panel that was not told its public
// address: the address a person reached it with is the best guess there is.
func (c *Config) AgentEndpointFor(requestHost string) string {
	if c.GRPCPublicAddr != "" {
		return c.GRPCPublicAddr
	}

	_, port, err := net.SplitHostPort(c.GRPCAddr)
	if err != nil || port == "" || port == "0" {
		port = "9443"
	}

	host := requestHost
	if h, _, err := net.SplitHostPort(requestHost); err == nil {
		host = h
	}
	if u, err := url.Parse(c.PublicURL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}

	return net.JoinHostPort(host, port)
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
