package databases

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"
)

// Candidate is a database container found inside an application's stack: a
// dedicated server.
type Candidate struct {
	Engine Engine
	// Product names what runs when it is not the engine itself, such as MySQL.
	Product string
	// Version is the major version, or major.minor, from the image tag.
	Version string
	// Extensions are what a shared server does not offer, such as PostGIS. A
	// server with any of them stays dedicated.
	Extensions []string
}

type knownImage struct {
	repository *regexp.Regexp
	engine     Engine
	product    string
	extensions []string
}

// knownImages are matched against the repository of an image, without a registry
// prefix of Docker Hub's own images.
var knownImages = []knownImage{
	{regexp.MustCompile(`^(library/)?mariadb$|^bitnami/mariadb$|^linuxserver/mariadb$`), MariaDB, "", nil},
	{regexp.MustCompile(`^(library/)?mysql$|^bitnami/mysql$|^mysql/mysql-server$`), MariaDB, "MySQL", nil},
	{regexp.MustCompile(`^(library/)?postgres$|^bitnami/postgresql$`), Postgres, "", nil},
	{regexp.MustCompile(`^postgis/postgis$`), Postgres, "", []string{"PostGIS"}},
	{regexp.MustCompile(`^timescale/timescaledb(-ha)?$`), Postgres, "", []string{"TimescaleDB"}},
	{regexp.MustCompile(`^pgvector/pgvector$`), Postgres, "", []string{"pgvector"}},
	{regexp.MustCompile(`^tensorchord/pgvecto-rs$`), Postgres, "", []string{"pgvecto.rs"}},
	{regexp.MustCompile(`^tensorchord/vchord-postgres$|^ghcr\.io/immich-app/postgres$`), Postgres, "", []string{"VectorChord"}},
	{regexp.MustCompile(`^supabase/postgres$`), Postgres, "", []string{"Supabase"}},
}

var leadingVersion = regexp.MustCompile(`^(?:pg)?(\d+(?:\.\d+)?)`)

// Recognize tells whether an image is a database server, and which.
func Recognize(image string) (Candidate, bool) {
	repository, tag := splitReference(image)
	repository = strings.TrimPrefix(repository, "docker.io/")

	for _, known := range knownImages {
		if !known.repository.MatchString(repository) {
			continue
		}

		candidate := Candidate{Engine: known.engine, Product: known.product, Extensions: known.extensions}
		if match := leadingVersion.FindStringSubmatch(tag); match != nil {
			candidate.Version = match[1]
		}
		return candidate, true
	}

	return Candidate{}, false
}

// Label is how the server is called on its card: "PostgreSQL 16 + VectorChord".
func (c Candidate) Label() string {
	label := c.Engine.Label
	if c.Product != "" {
		label = c.Product
	}
	if c.Version != "" {
		label += " " + c.Version
	}
	if len(c.Extensions) > 0 {
		label += " + " + strings.Join(c.Extensions, ", ")
	}
	return label
}

func splitReference(image string) (repository, tag string) {
	image, _, _ = strings.Cut(image, "@")
	// a colon after the last slash starts the tag; before it, a registry's port
	if slash := strings.LastIndex(image, "/"); strings.LastIndex(image, ":") > slash {
		colon := strings.LastIndex(image, ":")
		return image[:colon], image[colon+1:]
	}
	return image, ""
}

// MajorVersion is the first number of a version, or 0.
func MajorVersion(version string) int {
	major, _, _ := strings.Cut(version, ".")
	number, err := strconv.Atoi(strings.TrimSpace(major))
	if err != nil {
		return 0
	}
	return number
}

// CanMoveTo tells whether a dedicated server's databases fit on a shared server:
// the same engine, nothing the shared server lacks, and a version that is not
// older, because a dump does not restore into an older server.
func CanMoveTo(dedicated Candidate, sharedEngine, sharedVersion string) bool {
	if dedicated.Engine.Name != sharedEngine || dedicated.Product != "" || len(dedicated.Extensions) > 0 {
		return false
	}
	have, want := MajorVersion(sharedVersion), MajorVersion(dedicated.Version)
	return have > 0 && want > 0 && have >= want
}

// Credentials are what a stack's Compose file says about the administrator of a
// database container: the user, and the variable that holds the password.
type Credentials struct {
	User        string `json:"admin_user"`
	PasswordVar string `json:"password_key"`
}

var variableReference = regexp.MustCompile(`^\$\{?([A-Za-z_][A-Za-z0-9_]*)(?:[:?-][^}]*)?\}?$`)

// SuggestCredentials reads the environment of a database service in a Compose
// file. A password written into the file itself is not a variable, and the
// person linking the server types it instead.
func SuggestCredentials(compose, service string, engine Engine) Credentials {
	var file struct {
		Services map[string]struct {
			Environment any `yaml:"environment"`
		} `yaml:"services"`
	}
	if yaml.Unmarshal([]byte(compose), &file) != nil {
		return Credentials{User: engine.AdminUser}
	}

	env := map[string]string{}
	switch values := file.Services[service].Environment.(type) {
	case map[string]any:
		for key, value := range values {
			env[key] = fmt.Sprint(value)
		}
	case []any:
		for _, item := range values {
			if key, value, found := strings.Cut(fmt.Sprint(item), "="); found {
				env[key] = value
			}
		}
	}

	first := func(keys ...string) string {
		for _, key := range keys {
			if env[key] != "" {
				return env[key]
			}
		}
		return ""
	}
	variable := func(value string) string {
		if match := variableReference.FindStringSubmatch(strings.TrimSpace(value)); match != nil {
			return match[1]
		}
		return ""
	}

	if engine.Name == Postgres.Name {
		user := first("POSTGRES_USER", "POSTGRESQL_USERNAME")
		if user == "" {
			user = engine.AdminUser
		}
		return Credentials{User: user, PasswordVar: variable(first("POSTGRES_PASSWORD", "POSTGRESQL_PASSWORD"))}
	}

	if root := first("MARIADB_ROOT_PASSWORD", "MYSQL_ROOT_PASSWORD"); root != "" {
		return Credentials{User: engine.AdminUser, PasswordVar: variable(root)}
	}
	// without a root password, the application's own user is all there is
	if user := first("MARIADB_USER", "MYSQL_USER"); user != "" {
		return Credentials{User: user, PasswordVar: variable(first("MARIADB_PASSWORD", "MYSQL_PASSWORD"))}
	}
	return Credentials{User: engine.AdminUser}
}
