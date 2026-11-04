package databases

import (
	"bufio"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Script is something the agent runs inside a database server's container, with
// the server's own client. The command is fixed; whatever varies, and every
// password, comes from the environment of the process, never from its command
// line, which anyone in the container can read.
type Script struct {
	Cmd []string
	Env []string
}

// selectClient picks the client of a MySQL-family server: MariaDB images have
// mariadb, MySQL images still call it mysql.
const selectClient = `if command -v mariadb >/dev/null 2>&1; then client=mariadb; dump=mariadb-dump; else client=mysql; dump=mysqldump; fi
`

func credentials(engine Engine, user, password string) []string {
	if engine.Name == MariaDB.Name {
		return []string{"PC_ADMIN_USER=" + user, "MYSQL_PWD=" + password}
	}
	return []string{"PC_ADMIN_USER=" + user, "PGPASSWORD=" + password}
}

// Statements runs SQL as the administrator. The output is tab-separated rows
// without headers.
func Statements(engine Engine, user, password, sql string) Script {
	var script string
	if engine.Name == MariaDB.Name {
		script = selectClient + `printf '%s\n' "$PC_SQL" | "$client" --user="$PC_ADMIN_USER" --batch --skip-column-names`
	} else {
		script = `printf '%s\n' "$PC_SQL" | psql -X -q -v ON_ERROR_STOP=1 -A -t -F "$(printf '\t')" --username="$PC_ADMIN_USER" --dbname=postgres`
	}
	return Script{
		Cmd: []string{"sh", "-c", script},
		Env: append(credentials(engine, user, password), "PC_SQL="+sql),
	}
}

// Dump writes a database as SQL to the output. A MariaDB dump is consistent
// through one transaction; a PostgreSQL dump is consistent by itself. Owners and
// privileges are left out, so that the dump can be restored as any user.
func Dump(engine Engine, user, password, database string) Script {
	var script string
	if engine.Name == MariaDB.Name {
		script = selectClient + `exec "$dump" --user="$PC_ADMIN_USER" --single-transaction --routines --triggers --events --hex-blob "$PC_DATABASE"`
	} else {
		script = `exec pg_dump --username="$PC_ADMIN_USER" --no-owner --no-privileges "$PC_DATABASE"`
	}
	return Script{
		Cmd: []string{"sh", "-c", script},
		Env: append(credentials(engine, user, password), "PC_DATABASE="+database),
	}
}

// Clone copies a database into another one on the same server, which must exist,
// in one process: the dump goes straight into the restore. A failed dump fails
// the clone too, although only the restore's status is the pipe's own.
func Clone(engine Engine, user, password, source, target, owner string) Script {
	var pipe string
	if engine.Name == MariaDB.Name {
		pipe = selectClient + `{ "$dump" --user="$PC_ADMIN_USER" --single-transaction --routines --triggers --events --hex-blob "$PC_SOURCE"; echo $? > "$status"; } | "$client" --user="$PC_ADMIN_USER" "$PC_TARGET"`
	} else {
		// what the restore creates belongs to the target's owner
		pipe = `{ printf 'SET ROLE "%s";\n' "$PC_OWNER"; pg_dump --username="$PC_ADMIN_USER" --no-owner --no-privileges "$PC_SOURCE"; echo $? > "$status"; } | psql -X -q -v ON_ERROR_STOP=1 --username="$PC_ADMIN_USER" --dbname="$PC_TARGET"`
	}

	script := `status=$(mktemp) || exit 1
` + pipe + `
restored=$?
dumped=$(cat "$status")
rm -f "$status"
if [ "$dumped" != 0 ]; then echo "the dump of $PC_SOURCE failed" >&2; exit 1; fi
exit $restored`

	return Script{
		Cmd: []string{"sh", "-c", script},
		Env: append(credentials(engine, user, password), "PC_SOURCE="+source, "PC_TARGET="+target, "PC_OWNER="+owner),
	}
}

func mariadbString(value string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(value) + "'"
}

func postgresString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func postgresIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func mariadbIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// CreateSQL creates a database and a user with every privilege on it and none
// elsewhere. It can run again: what exists is kept, and the password is set.
func CreateSQL(engine Engine, database, user, password string) string {
	if engine.Name == MariaDB.Name {
		account := mariadbString(user) + "@'%'"
		return strings.Join([]string{
			"CREATE DATABASE IF NOT EXISTS " + mariadbIdent(database) + " CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;",
			"CREATE USER IF NOT EXISTS " + account + " IDENTIFIED BY " + mariadbString(password) + ";",
			"ALTER USER " + account + " IDENTIFIED BY " + mariadbString(password) + ";",
			"GRANT ALL PRIVILEGES ON " + mariadbIdent(database) + ".* TO " + account + ";",
		}, "\n")
	}

	// CREATE DATABASE cannot run in a function or a transaction: psql's \gexec
	// runs it only when the database is missing
	return strings.Join([]string{
		"DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = " + postgresString(user) + ") THEN CREATE ROLE " + postgresIdent(user) + " LOGIN; END IF; END $$;",
		"ALTER ROLE " + postgresIdent(user) + " WITH LOGIN PASSWORD " + postgresString(password) + ";",
		"SELECT " + postgresString("CREATE DATABASE "+postgresIdent(database)+" OWNER "+postgresIdent(user)) +
			" WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = " + postgresString(database) + ")\\gexec",
		// everyone may connect to a new database unless told otherwise
		"REVOKE ALL ON DATABASE " + postgresIdent(database) + " FROM PUBLIC;",
	}, "\n")
}

// RotateSQL gives a user a new password.
func RotateSQL(engine Engine, user, password string) string {
	if engine.Name == MariaDB.Name {
		return "ALTER USER " + mariadbString(user) + "@'%' IDENTIFIED BY " + mariadbString(password) + ";"
	}
	return "ALTER ROLE " + postgresIdent(user) + " WITH PASSWORD " + postgresString(password) + ";"
}

// DropSQL removes a database and its user. Connections to the database end.
func DropSQL(engine Engine, database, user string) string {
	if engine.Name == MariaDB.Name {
		return "DROP DATABASE IF EXISTS " + mariadbIdent(database) + ";\nDROP USER IF EXISTS " + mariadbString(user) + "@'%';"
	}
	return "DROP DATABASE IF EXISTS " + postgresIdent(database) + " WITH (FORCE);\nDROP ROLE IF EXISTS " + postgresIdent(user) + ";"
}

// DropManySQL removes databases, then their users, as a user may own several of
// them and PostgreSQL keeps a role that still owns a database.
func DropManySQL(engine Engine, names, users []string) string {
	lines := []string{}
	for _, name := range names {
		if engine.Name == MariaDB.Name {
			lines = append(lines, "DROP DATABASE IF EXISTS "+mariadbIdent(name)+";")
		} else {
			lines = append(lines, "DROP DATABASE IF EXISTS "+postgresIdent(name)+" WITH (FORCE);")
		}
	}
	seen := map[string]bool{}
	for _, user := range users {
		if seen[user] {
			continue
		}
		seen[user] = true
		if engine.Name == MariaDB.Name {
			lines = append(lines, "DROP USER IF EXISTS "+mariadbString(user)+"@'%';")
		} else {
			lines = append(lines, "DROP ROLE IF EXISTS "+postgresIdent(user)+";")
		}
	}
	return strings.Join(lines, "\n")
}

// StatsSQL reads the size and the connections of every database, and the
// server's limit and version, as rows of kind, name and value.
func StatsSQL(engine Engine) string {
	if engine.Name == MariaDB.Name {
		return strings.Join([]string{
			"SELECT 'size', table_schema, COALESCE(SUM(data_length + index_length), 0) FROM information_schema.tables GROUP BY table_schema;",
			"SELECT 'size', schema_name, 0 FROM information_schema.schemata;",
			"SELECT 'conn', COALESCE(db, ''), COUNT(*) FROM information_schema.processlist WHERE command <> 'Daemon' GROUP BY db;",
			"SELECT 'max', '', @@max_connections;",
			"SELECT 'version', VERSION(), 0;",
		}, "\n")
	}
	return strings.Join([]string{
		"SELECT 'size', datname, pg_database_size(datname) FROM pg_database WHERE NOT datistemplate;",
		"SELECT 'conn', COALESCE(datname, ''), count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' GROUP BY datname;",
		"SELECT 'max', '', setting FROM pg_settings WHERE name = 'max_connections';",
		"SELECT 'version', current_setting('server_version'), 0;",
	}, "\n")
}

// Stats are what a server says about its databases.
type Stats struct {
	Version        string                   `json:"version"`
	MaxConnections int                      `json:"max_connections"`
	Connections    int                      `json:"connections"`
	SizeBytes      int64                    `json:"size_bytes"`
	Databases      map[string]DatabaseStats `json:"databases"`
}

// DatabaseStats are the size and the open connections of one database.
type DatabaseStats struct {
	SizeBytes   int64 `json:"size_bytes"`
	Connections int   `json:"connections"`
}

// ParseStats reads the output of StatsSQL. The engine's own databases count for
// the totals but are not listed.
func ParseStats(engine Engine, output string) (Stats, error) {
	stats := Stats{Databases: map[string]DatabaseStats{}}

	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) != 3 {
			continue
		}
		kind, name, value := fields[0], fields[1], strings.TrimSpace(fields[2])

		switch kind {
		case "version":
			stats.Version = name
		case "max":
			maximum, err := strconv.Atoi(value)
			if err != nil {
				return Stats{}, fmt.Errorf("max_connections %q: %w", value, err)
			}
			stats.MaxConnections = maximum
		case "size", "conn":
			number, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return Stats{}, fmt.Errorf("%s of %s %q: %w", kind, name, value, err)
			}
			if kind == "size" {
				stats.SizeBytes += number
			} else {
				stats.Connections += int(number)
			}
			if name == "" || slices.Contains(engine.ReservedNames, name) {
				continue
			}
			database := stats.Databases[name]
			if kind == "size" {
				database.SizeBytes += number
			} else {
				database.Connections += int(number)
			}
			stats.Databases[name] = database
		}
	}

	if stats.Version == "" {
		return Stats{}, fmt.Errorf("the server did not say its version: %q", output)
	}

	return stats, scanner.Err()
}

// RestoreFile restores a dump that lies in the server's container into an existing
// database, and removes the file whatever happens. A PostgreSQL restore runs as
// the database's owner, so that what it creates belongs to that user.
func RestoreFile(engine Engine, user, password, database, owner, file string) Script {
	var restore string
	if engine.Name == MariaDB.Name {
		restore = selectClient + `"$client" --user="$PC_ADMIN_USER" "$PC_DATABASE" < "$PC_FILE"`
	} else {
		restore = `{ printf 'SET ROLE "%s";\n' "$PC_OWNER"; cat "$PC_FILE"; } | psql -X -q -v ON_ERROR_STOP=1 --username="$PC_ADMIN_USER" --dbname="$PC_DATABASE"`
	}

	return Script{
		Cmd: []string{"sh", "-c", restore + "\nstatus=$?\nrm -f \"$PC_FILE\"\nexit $status"},
		Env: append(credentials(engine, user, password), "PC_DATABASE="+database, "PC_OWNER="+owner, "PC_FILE="+file),
	}
}

// ExistsSQL tells whether a database and a user of these names exist, as rows of
// kind and count.
func ExistsSQL(engine Engine, database, user string) string {
	if engine.Name == MariaDB.Name {
		return "SELECT 'database', COUNT(*) FROM information_schema.schemata WHERE schema_name = " + mariadbString(database) + ";\n" +
			"SELECT 'user', COUNT(*) FROM mysql.user WHERE user = " + mariadbString(user) + ";"
	}
	return "SELECT 'database', count(*) FROM pg_database WHERE datname = " + postgresString(database) + ";\n" +
		"SELECT 'user', count(*) FROM pg_roles WHERE rolname = " + postgresString(user) + ";"
}

// SignIn signs in as a user over the container's own address, where the server
// checks the password, unlike on its local socket, and runs nothing. It tells
// whether the password is the one the server knows.
func SignIn(engine Engine, user, password, database string) Script {
	address := `"$(hostname -i | cut -d' ' -f1)"`
	var script string
	if engine.Name == MariaDB.Name {
		script = selectClient + `"$client" --host=` + address + ` --user="$PC_ADMIN_USER" --batch --skip-column-names -e "SELECT 1" "$PC_DATABASE" >/dev/null`
	} else {
		script = `psql -X -q -A -t --host=` + address + ` --username="$PC_ADMIN_USER" --dbname="$PC_DATABASE" -c "SELECT 1" >/dev/null`
	}
	return Script{
		Cmd: []string{"sh", "-c", script},
		Env: append(credentials(engine, user, password), "PC_DATABASE="+database),
	}
}

// DatabasesSQL lists a server's databases, one name per line, without those the
// engine keeps for itself.
func DatabasesSQL(engine Engine) string {
	if engine.Name == MariaDB.Name {
		return "SELECT schema_name FROM information_schema.schemata WHERE schema_name NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys');"
	}
	return "SELECT datname FROM pg_database WHERE NOT datistemplate AND datname <> 'postgres';"
}

// ParseNames reads a list of names, one per line.
func ParseNames(output string) []string {
	names := []string{}
	for _, line := range strings.Split(output, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// ParseExists reads the output of ExistsSQL.
func ParseExists(output string) (database, user bool) {
	for _, line := range strings.Split(output, "\n") {
		kind, count, found := strings.Cut(strings.TrimSpace(line), "\t")
		if !found || strings.TrimSpace(count) == "0" {
			continue
		}
		switch kind {
		case "database":
			database = true
		case "user":
			user = true
		}
	}
	return database, user
}

// CountTables prints how many tables and views a database has, for comparing a
// restored database with its original.
func CountTables(engine Engine, user, password, database string) Script {
	var script string
	if engine.Name == MariaDB.Name {
		script = selectClient + `"$client" --user="$PC_ADMIN_USER" --batch --skip-column-names -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE()" "$PC_DATABASE"`
	} else {
		script = `psql -X -q -A -t --username="$PC_ADMIN_USER" --dbname="$PC_DATABASE" -c "SELECT count(*) FROM information_schema.tables WHERE table_schema NOT IN ('pg_catalog', 'information_schema')"`
	}
	return Script{
		Cmd: []string{"sh", "-c", script},
		Env: append(credentials(engine, user, password), "PC_DATABASE="+database),
	}
}
