// Package databases holds what the panel and the agent know about the database
// engines PocketCloud runs: names, images, the Compose files of shared servers and
// the database networks of applications, and the SQL the agent runs with each
// engine's own client. See "Database servers" in docs/architecture.md.
package databases

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Engine describes one database engine.
type Engine struct {
	// Name is the engine's name in the panel: mariadb or postgres.
	Name string
	// Label is how people call it.
	Label string
	// Image is the image of new shared servers: the LTS series of MariaDB 11 and
	// PostgreSQL 17. Pulling it again brings the newest minor version.
	Image string
	// Port is where the server listens on its networks.
	Port int
	// AdminUser is the administrator the official image creates.
	AdminUser string
	// DataDir is where the image keeps its data.
	DataDir string
	// MaxConnections is what a new shared server allows.
	MaxConnections int
	// ReservedNames cannot be used for databases or users: the engine has them.
	ReservedNames []string
}

// Engines PocketCloud runs as shared servers.
var (
	MariaDB = Engine{
		Name:           "mariadb",
		Label:          "MariaDB",
		Image:          "mariadb:11.8",
		Port:           3306,
		AdminUser:      "root",
		DataDir:        "/var/lib/mysql",
		MaxConnections: 200,
		ReservedNames:  []string{"mysql", "information_schema", "performance_schema", "sys", "root", "mariadb.sys"},
	}
	Postgres = Engine{
		Name:           "postgres",
		Label:          "PostgreSQL",
		Image:          "postgres:17",
		Port:           5432,
		AdminUser:      "postgres",
		DataDir:        "/var/lib/postgresql/data",
		MaxConnections: 100,
		ReservedNames:  []string{"postgres", "template0", "template1", "public"},
	}
)

// EngineOf returns an engine by its name.
func EngineOf(name string) (Engine, bool) {
	switch name {
	case MariaDB.Name:
		return MariaDB, true
	case Postgres.Name:
		return Postgres, true
	}
	return Engine{}, false
}

var (
	// serverNamePattern is also a stack name, a network alias and a host name on
	// the database networks, so it has neither upper case nor underscores.
	serverNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
	// databaseNamePattern needs no quoting in either engine, and is a user name too.
	databaseNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
)

// ValidServerName says what is wrong with the name of a new server, if anything.
func ValidServerName(name string) error {
	if !serverNamePattern.MatchString(name) {
		return fmt.Errorf("%q is not a valid server name: use lower case letters, digits and dashes", name)
	}
	return nil
}

// ValidDatabaseName says what is wrong with the name of a new database, if anything.
// It is also the name of its user, and of the secret with its password.
func ValidDatabaseName(engine Engine, name string) error {
	if !databaseNamePattern.MatchString(name) {
		return fmt.Errorf("%q is not a valid database name: start with a letter and use lower case letters, digits and underscores, at most 63", name)
	}
	if slices.Contains(engine.ReservedNames, name) {
		return fmt.Errorf("%s keeps the name %q for itself", engine.Label, name)
	}
	return nil
}

// NetworkName is the private network between an application and its database servers.
func NetworkName(app string) string {
	return "pcdb-" + app
}

// SecretName is the name under which an application's env file refers to the
// password of a database: ${secret:db/<name>}.
func SecretName(database string) string {
	return "db/" + database
}

// AdminSecretName is the reference a shared server's own env file uses for its
// administrator's password: ${secret:db-admin/<server>}.
func AdminSecretName(server string) string {
	return "db-admin/" + server
}

const passwordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// NewPassword returns a random password of 32 letters and digits, about 185 bits.
// It needs no quoting in SQL, URLs or env files.
func NewPassword() string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	for i, value := range raw {
		// 256 is not a multiple of the alphabet, which skews the choice a little;
		// at 32 characters that costs a few bits of more than 180
		raw[i] = passwordAlphabet[int(value)%len(passwordAlphabet)]
	}
	return string(raw)
}

const mib = 1 << 20

// BufferSize is the memory a shared server keeps for its cache
// (innodb_buffer_pool_size or shared_buffers): an eighth of the host's memory,
// because other applications run there too, from 128 MiB to 2 GiB, in whole MiB.
func BufferSize(hostMemory uint64) uint64 {
	size := hostMemory / 8
	size = max(size, 128*mib)
	size = min(size, 2048*mib)
	return size / mib * mib
}

func megabytes(bytes uint64) string {
	return strconv.FormatUint(bytes/mib, 10) + "M"
}

// Connection describes the database of an application, for its env file.
type Connection struct {
	Engine   Engine
	Server   string
	Database string
	User     string
}

// EnvLines are the variables an application gets for its database. The password
// is a reference that the panel resolves when a deploy is delivered.
func (c Connection) EnvLines() []string {
	password := "${secret:" + SecretName(c.Database) + "}"
	lines := []string{
		"DB_HOST=" + c.Server,
		"DB_PORT=" + strconv.Itoa(c.Engine.Port),
		"DB_NAME=" + c.Database,
		"DB_USER=" + c.User,
		"DB_PASSWORD=" + password,
	}
	// PostgreSQL applications usually take a URL; MariaDB ones do not agree on a scheme
	if c.Engine.Name == Postgres.Name {
		lines = append(lines, fmt.Sprintf("DATABASE_URL=postgres://%s:%s@%s:%d/%s", c.User, password, c.Server, c.Engine.Port, c.Database))
	}
	return lines
}

// AddEnv adds the variables of a connection to an env file. Variables the file
// already sets keep their values, so a second database does not overwrite the first.
func AddEnv(env string, lines []string) (string, bool) {
	keys := map[string]bool{}
	for _, line := range strings.Split(env, "\n") {
		if key, _, found := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "export "), "="); found {
			keys[strings.TrimSpace(key)] = true
		}
	}

	added := []string{}
	for _, line := range lines {
		key, _, _ := strings.Cut(line, "=")
		if !keys[key] {
			added = append(added, line)
		}
	}
	if len(added) == 0 {
		return env, false
	}

	if env != "" && !strings.HasSuffix(env, "\n") {
		env += "\n"
	}
	return env + strings.Join(added, "\n") + "\n", true
}
