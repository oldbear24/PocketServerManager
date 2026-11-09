// Package backup knows where backups go (#28): it checks a backup target's
// settings and credentials, and turns them into what restic needs to open the
// target's repository, its URL, environment and options. The panel keeps the
// targets and their secrets; agents run restic.
package backup

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// The kinds of target.
const (
	// KindS3 is an S3-compatible bucket: Backblaze B2, MinIO, AWS.
	KindS3 = "s3"
	// KindSFTP is a directory on a server reached over SFTP.
	KindSFTP = "sftp"
	// KindREST is restic's rest-server.
	KindREST = "rest"
	// KindLocal is a directory on the target's server.
	KindLocal = "local"
)

// Kinds lists the kinds of target.
var Kinds = []string{KindS3, KindSFTP, KindREST, KindLocal}

// Paths inside the job container that runs restic.
const (
	// LocalMount is where a local target's directory is mounted.
	LocalMount = "/repo"
	// SSHKeyFile and KnownHostsFile hold an SFTP target's key and the
	// server's host key.
	SSHKeyFile     = "/run/pocketcloud/ssh-key"
	KnownHostsFile = "/run/pocketcloud/known_hosts"
)

// Config is a target's settings, none of them secret.
type Config struct {
	// S3
	Endpoint  string `json:"endpoint,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	Region    string `json:"region,omitempty"`
	PathStyle bool   `json:"path_style,omitempty"`
	// SFTP: the server's address (host or host:port), the user and its host
	// key, pinned as an authorized_keys line
	Address string `json:"address,omitempty"`
	User    string `json:"user,omitempty"`
	HostKey string `json:"host_key,omitempty"`
	// SFTP and local: the directory
	Path string `json:"path,omitempty"`
	// rest-server: its URL, without credentials
	URL string `json:"url,omitempty"`
}

// Credentials are a target's secrets.
type Credentials struct {
	// S3
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	// SFTP: an unencrypted private key
	PrivateKey string `json:"private_key,omitempty"`
	// rest-server: its HTTP basic auth, if any
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// Repository is how restic opens a target's repository.
type Repository struct {
	// URL is restic's repository: the place, never a secret.
	URL string
	// Env is restic's environment for the storage's credentials.
	Env map[string]string
	// Options are restic's -o options.
	Options map[string]string
	// SSHKey and KnownHosts go to SSHKeyFile and KnownHostsFile.
	SSHKey     string
	KnownHosts string
	// Mount is the server's directory a local target mounts at LocalMount.
	Mount string
}

var (
	bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	userName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)
	regionName = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
)

// forbidden are the server's directories a local target may not be in or be:
// they belong to the system, the kernel or Docker.
var forbidden = []string{"/proc", "/sys", "/dev", "/run", "/var/run", "/var/lib/docker", "/boot", "/etc", "/bin", "/sbin", "/usr", "/lib", "/lib64"}

// Check checks a target's settings and credentials and returns them tidied:
// spaces trimmed, a bucket's prefix and a path cleaned. Credentials that the
// kind does not use are dropped.
func Check(kind string, config Config, creds Credentials) (Config, Credentials, error) {
	trim := func(s *string) { *s = strings.TrimSpace(*s) }
	for _, s := range []*string{&config.Endpoint, &config.Bucket, &config.Prefix, &config.Region, &config.Address, &config.User, &config.HostKey, &config.Path, &config.URL, &creds.AccessKeyID, &creds.Username} {
		trim(s)
	}
	switch kind {
	case KindS3:
		endpoint, err := endpointURL(config.Endpoint)
		if err != nil {
			return config, creds, err
		}
		config.Endpoint = endpoint
		if !bucketName.MatchString(config.Bucket) || strings.Contains(config.Bucket, "..") {
			return config, creds, errors.New("the bucket's name is 3 to 63 lowercase letters, digits, dots and hyphens")
		}
		prefix, err := relative(config.Prefix, "the prefix")
		if err != nil {
			return config, creds, err
		}
		config.Prefix = prefix
		if config.Region != "" && !regionName.MatchString(config.Region) {
			return config, creds, errors.New("the region is a name such as eu-central-003 or us-east-1")
		}
		if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
			return config, creds, errors.New("give the access key ID and the secret access key")
		}
		config = Config{Endpoint: config.Endpoint, Bucket: config.Bucket, Prefix: config.Prefix, Region: config.Region, PathStyle: config.PathStyle}
		creds = Credentials{AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey}
	case KindSFTP:
		host, port, err := address(config.Address)
		if err != nil {
			return config, creds, err
		}
		config.Address = net.JoinHostPort(host, strconv.Itoa(port))
		if port == 22 {
			config.Address = host
		}
		if !userName.MatchString(config.User) {
			return config, creds, errors.New("give the SFTP user, such as backup")
		}
		if config.Path == "" || strings.ContainsAny(config.Path, "\n\r\x00") {
			return config, creds, errors.New("give the directory on the server, such as /srv/restic")
		}
		config.Path = path.Clean(config.Path)
		if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(config.HostKey)); err != nil {
			return config, creds, errors.New("give the server's host key, a line such as ssh-ed25519 AAAA… (ssh-keyscan prints it), so that no other server can pose as it")
		}
		fields := strings.Fields(config.HostKey)
		config.HostKey = fields[0] + " " + fields[1]
		if _, err := ssh.ParsePrivateKey([]byte(creds.PrivateKey)); err != nil {
			var missing *ssh.PassphraseMissingError
			if errors.As(err, &missing) {
				return config, creds, errors.New("the private key has a passphrase: give one without, as restic runs unattended")
			}
			return config, creds, errors.New("give the private key, in OpenSSH or PEM format")
		}
		config = Config{Address: config.Address, User: config.User, Path: config.Path, HostKey: config.HostKey}
		creds = Credentials{PrivateKey: strings.TrimSpace(creds.PrivateKey) + "\n"}
	case KindREST:
		u, err := url.Parse(config.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return config, creds, errors.New("give the rest-server's URL, such as https://backup-node:8000/homelab/")
		}
		if u.User != nil {
			return config, creds, errors.New("give the user and password in their own fields, not in the URL")
		}
		if !strings.HasSuffix(u.Path, "/") {
			u.Path += "/"
		}
		config = Config{URL: u.String()}
		if (creds.Username == "") != (creds.Password == "") {
			return config, creds, errors.New("give both the user and the password, or neither")
		}
		creds = Credentials{Username: creds.Username, Password: creds.Password}
	case KindLocal:
		clean, err := LocalPath(config.Path)
		if err != nil {
			return config, creds, err
		}
		config = Config{Path: clean}
		creds = Credentials{}
	default:
		return config, creds, fmt.Errorf("a target is %s, not %q", strings.Join(Kinds, ", "), kind)
	}
	return config, creds, nil
}

// LocalPath checks a local target's directory on its server and returns it
// cleaned: absolute, not / and outside the system's directories. The agent
// checks it again before it mounts it.
func LocalPath(dir string) (string, error) {
	if !path.IsAbs(dir) || strings.ContainsAny(dir, "\n\r\x00") {
		return "", errors.New("give an absolute directory on the server, such as /srv/backups/restic")
	}
	clean := path.Clean(dir)
	if clean == "/" {
		return "", errors.New("the repository needs a directory of its own, not /")
	}
	for _, system := range forbidden {
		if clean == system || strings.HasPrefix(clean, system+"/") {
			return "", fmt.Errorf("%s is the system's: choose a directory elsewhere, such as /srv/backups", system)
		}
	}
	return clean, nil
}

// Locate is where restic finds a checked target's repository.
func Locate(kind string, config Config, creds Credentials) (Repository, error) {
	repo := Repository{Env: map[string]string{}, Options: map[string]string{}}
	switch kind {
	case KindS3:
		repo.URL = "s3:" + config.Endpoint + "/" + config.Bucket
		if config.Prefix != "" {
			repo.URL += "/" + config.Prefix
		}
		repo.Env["AWS_ACCESS_KEY_ID"] = creds.AccessKeyID
		repo.Env["AWS_SECRET_ACCESS_KEY"] = creds.SecretAccessKey
		if config.Region != "" {
			repo.Env["AWS_DEFAULT_REGION"] = config.Region
		}
		if config.PathStyle {
			repo.Options["s3.bucket-lookup"] = "path"
		}
	case KindSFTP:
		host, port, err := address(config.Address)
		if err != nil {
			return repo, err
		}
		// restic runs ssh itself; the server's key is pinned and nothing
		// else is trusted
		repo.URL = "sftp:" + config.User + "@" + host + ":" + config.Path
		repo.Options["sftp.command"] = fmt.Sprintf("ssh -i %s -o UserKnownHostsFile=%s -o StrictHostKeyChecking=yes -o BatchMode=yes -o IdentitiesOnly=yes -p %d %s@%s -s sftp",
			SSHKeyFile, KnownHostsFile, port, config.User, host)
		known := host
		if port != 22 {
			known = "[" + host + "]:" + strconv.Itoa(port)
		}
		repo.KnownHosts = known + " " + config.HostKey + "\n"
		repo.SSHKey = creds.PrivateKey
	case KindREST:
		repo.URL = "rest:" + config.URL
		if creds.Username != "" {
			repo.Env["RESTIC_REST_USERNAME"] = creds.Username
			repo.Env["RESTIC_REST_PASSWORD"] = creds.Password
		}
	case KindLocal:
		repo.URL = LocalMount
		repo.Mount = config.Path
	default:
		return repo, fmt.Errorf("a target is %s, not %q", strings.Join(Kinds, ", "), kind)
	}
	return repo, nil
}

// Where is a target's place as people read it: the bucket, the server's
// directory or the URL, never a secret.
func Where(kind string, config Config) string {
	switch kind {
	case KindS3:
		u, _ := url.Parse(config.Endpoint)
		where := "s3:" + config.Bucket
		if config.Prefix != "" {
			where += "/" + config.Prefix
		}
		if u != nil {
			where += " @ " + u.Host
		}
		return where
	case KindSFTP:
		return "sftp:" + config.User + "@" + config.Address + ":" + config.Path
	case KindREST:
		return "rest:" + config.URL
	default:
		return config.Path
	}
}

// NewPassword makes a repository's password: 32 random bytes.
func NewPassword() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Secrets are the values to keep out of what restic says about a
// repository: its password, the storage's credentials and the lines of an
// SFTP key.
func Secrets(password string, env map[string]string, sshKey string) []string {
	secrets := []string{password}
	for name, value := range env {
		// a region is no secret, and restic's errors name it
		if !strings.HasSuffix(name, "_REGION") {
			secrets = append(secrets, value)
		}
	}
	for _, line := range strings.Split(sshKey, "\n") {
		if line = strings.TrimSpace(line); len(line) >= 16 && !strings.HasPrefix(line, "-----") {
			secrets = append(secrets, line)
		}
	}
	// the longest first, so that one inside another goes whole
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	return secrets
}

// Redact replaces the secrets in what restic said, as its errors may repeat a
// URL or a value it was given.
func Redact(text string, secrets ...string) string {
	for _, secret := range secrets {
		if len(secret) >= 4 {
			text = strings.ReplaceAll(text, secret, "***")
		}
	}
	return text
}

// endpointURL checks an S3 endpoint: http(s)://host[:port], nothing more.
func endpointURL(raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" {
		return "", errors.New("give the S3 endpoint, such as https://s3.eu-central-003.backblazeb2.com (the bucket has its own field)")
	}
	return u.Scheme + "://" + u.Host, nil
}

// relative checks a path under a bucket or a directory: no leading slash, no
// way up.
func relative(p, what string) (string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", nil
	}
	if strings.ContainsAny(p, "\n\r\x00\\") {
		return "", fmt.Errorf("%s holds a character it cannot", what)
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%s is a path such as backups/homelab", what)
		}
	}
	return p, nil
}

// address splits host[:port], with port 22 by default.
func address(raw string) (string, int, error) {
	host, portText, err := net.SplitHostPort(raw)
	if err != nil {
		host, portText = raw, "22"
	}
	host = strings.Trim(host, "[]")
	port, err := strconv.Atoi(portText)
	if host == "" || strings.ContainsAny(host, " /@\n") || err != nil || port < 1 || port > 65535 {
		return "", 0, errors.New("give the SFTP server's address, such as backup-node or backup-node:2222")
	}
	return host, port, nil
}
