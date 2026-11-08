package panel

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/oldbear24/PocketServerManager/internal/edge"
	"github.com/oldbear24/PocketServerManager/internal/php"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// Environment variables that configure the panel; flags take precedence.
const (
	EnvPublicURL     = "POCKETCLOUD_PUBLIC_URL"
	EnvGRPCAddr      = "POCKETCLOUD_GRPC_ADDR"
	EnvGRPCPublic    = "POCKETCLOUD_GRPC_PUBLIC_ADDR"
	EnvMasterKeyFile = "POCKETCLOUD_MASTER_KEY_FILE"
	EnvPHPImage      = "POCKETCLOUD_PHP_IMAGE"
	EnvDataDir       = "POCKETCLOUD_DATA_DIR"
	// the panel's own HTTPS (#63)
	EnvHTTPSAddr      = "POCKETCLOUD_HTTPS_ADDR"
	EnvHTTPSChallenge = "POCKETCLOUD_HTTPS_CHALLENGE"

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
	// PHPImage is the repository of the PHP runtime images (#50), such as
	// a registry's copy of PocketCloud's own.
	PHPImage string
	// HTTPSAddr is where the panel serves its UI and API over HTTPS with its
	// own certificate from ACME (#63). Empty: plain HTTP on PocketBase's
	// --http, as behind a reverse proxy. Set: --http only answers the CA's
	// HTTP-01 challenges and redirects to HTTPS.
	HTTPSAddr string
	// HTTPSChallenge is how the CA checks the panel's name: auto, http-01 or
	// dns-01.
	HTTPSChallenge string
}

// ConfigFromEnv returns the defaults, overridden by the environment.
func ConfigFromEnv() Config {
	return Config{
		PublicURL:      os.Getenv(EnvPublicURL),
		GRPCAddr:       envOr(EnvGRPCAddr, defaultGRPCAddr),
		GRPCPublicAddr: os.Getenv(EnvGRPCPublic),
		MasterKeyFile:  envOr(EnvMasterKeyFile, defaultMasterKeyFile),
		PHPImage:       envOr(EnvPHPImage, php.Image),
		HTTPSAddr:      os.Getenv(EnvHTTPSAddr),
		HTTPSChallenge: envOr(EnvHTTPSChallenge, schema.ChallengeAuto),
	}
}

// BindFlags adds the panel's flags to the root command.
func (c *Config) BindFlags(cmd *cobra.Command) {
	flags := cmd.PersistentFlags()
	flags.StringVar(&c.PublicURL, "public-url", c.PublicURL, "public URL of the panel, e.g. https://panel.example.com")
	flags.StringVar(&c.GRPCAddr, "grpc-addr", c.GRPCAddr, "address the gRPC gateway for agents listens on")
	flags.StringVar(&c.GRPCPublicAddr, "grpc-public-addr", c.GRPCPublicAddr, "host:port agents dial (default: the public URL's host with the gRPC port)")
	flags.StringVar(&c.MasterKeyFile, "master-key-file", c.MasterKeyFile, "file with the master key; create one with: panel keygen")
	flags.StringVar(&c.PHPImage, "php-image", c.PHPImage, "repository of the PHP runtime images, tagged <version>-fpm")
	flags.StringVar(&c.HTTPSAddr, "https-addr", c.HTTPSAddr,
		"address the panel serves HTTPS on with its own certificate from ACME; --http then only answers ACME HTTP-01 and redirects")
	flags.StringVar(&c.HTTPSChallenge, "https-challenge", c.HTTPSChallenge, "how the CA checks the panel's name: auto, http-01 or dns-01")
}

// CheckHTTPS checks the panel's own HTTPS (#63) and returns the name it gets
// a certificate for, its public URL's host; "" when it serves no HTTPS.
func (c *Config) CheckHTTPS() (string, error) {
	if c.HTTPSAddr == "" {
		return "", nil
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", errors.New("--https-addr needs --public-url with the panel's https:// address, such as https://panel.example.com")
	}
	name := strings.ToLower(u.Hostname())
	if net.ParseIP(name) != nil || strings.HasPrefix(name, "*.") || edge.ValidHostname(name) != nil {
		return "", fmt.Errorf("the panel's certificate is for a name, not %q: give --public-url a host name", name)
	}
	if challenge := c.PanelChallenge(); challenge != schema.ChallengeAuto && challenge != schema.ChallengeHTTP01 && challenge != schema.ChallengeDNS01 {
		return "", fmt.Errorf("--https-challenge is auto, http-01 or dns-01, not %q", challenge)
	}
	return name, nil
}

// PanelChallenge is how the CA checks the panel's name; auto when unset.
func (c *Config) PanelChallenge() string {
	if c.HTTPSChallenge == "" {
		return schema.ChallengeAuto
	}
	return c.HTTPSChallenge
}

// CheckServe refuses a start that would serve HTTPS twice: PocketBase's own
// --https and domain arguments, with autocert, besides --https-addr.
func (c *Config) CheckServe(cmd *cobra.Command, args []string) error {
	if cmd.Name() != "serve" || c.HTTPSAddr == "" {
		return nil
	}
	if flag := cmd.Flags().Lookup("https"); (flag != nil && flag.Changed) || len(args) > 0 {
		return errors.New("--https-addr and PocketBase's --https or domain arguments both serve HTTPS: use --https-addr alone")
	}
	return nil
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
