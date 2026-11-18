package fakedb

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
)

// The SQL of the panel's commands, as internal/databases writes it, one
// statement a line. Names are plain in what the agent sends; an identifier
// is "x" or `x`, a string 'x'.
const (
	ident  = `"([^"]+)"`
	mident = "`([^`]+)`"
	str    = `'([^']*)'`
)

// statement is a statement the fake knows: its pattern and what it does,
// which may fail with an error, as the client says it.
type statement struct {
	pattern *regexp.Regexp
	do      func(x *execution, match []string) error
}

func pattern(text string) *regexp.Regexp { return regexp.MustCompile("^" + text + "$") }

var postgresStatements = []statement{
	{pattern(`SELECT 'database', count\(\*\) FROM pg_database WHERE datname = ` + str + `;`), func(x *execution, m []string) error {
		x.named(m[1])
		x.printf("database\t%d\n", boolCount(x.sv.databases[m[1]] != nil))
		return nil
	}},
	{pattern(`SELECT 'user', count\(\*\) FROM pg_roles WHERE rolname = ` + str + `;`), func(x *execution, m []string) error {
		_, ok := x.sv.users[m[1]]
		x.printf("user\t%d\n", boolCount(ok))
		return nil
	}},
	{pattern(`DO \$\$ BEGIN IF NOT EXISTS \(SELECT FROM pg_roles WHERE rolname = ` + str + `\) THEN CREATE ROLE ` + ident + ` LOGIN; END IF; END \$\$;`),
		func(x *execution, m []string) error {
			if _, ok := x.sv.users[m[2]]; !ok {
				x.sv.users[m[2]] = ""
			}
			return nil
		}},
	{pattern(`ALTER ROLE ` + ident + ` WITH (?:LOGIN )?PASSWORD ` + str + `;`), func(x *execution, m []string) error {
		if _, ok := x.sv.users[m[1]]; !ok {
			return fmt.Errorf("ERROR:  role %q does not exist", m[1])
		}
		x.sv.users[m[1]] = m[2]
		return nil
	}},
	{pattern(`SELECT 'CREATE DATABASE ` + ident + ` OWNER ` + ident + `' WHERE NOT EXISTS \(SELECT FROM pg_database WHERE datname = ` + str + `\)\\gexec`),
		func(x *execution, m []string) error {
			x.named(m[1])
			if x.sv.databases[m[1]] == nil {
				return x.create(m[1], m[2], databases.DefaultCharset)
			}
			return nil
		}},
	{pattern(`CREATE DATABASE ` + ident + ` OWNER ` + ident + `;`), func(x *execution, m []string) error {
		x.named(m[1])
		if x.sv.databases[m[1]] != nil {
			return fmt.Errorf("ERROR:  database %q already exists", m[1])
		}
		return x.create(m[1], m[2], databases.DefaultCharset)
	}},
	{pattern(`REVOKE ALL ON DATABASE ` + ident + ` FROM PUBLIC;`), func(x *execution, m []string) error {
		return x.exists(m[1])
	}},
	{pattern(`DROP DATABASE IF EXISTS ` + ident + ` WITH \(FORCE\);`), func(x *execution, m []string) error {
		x.named(m[1])
		delete(x.sv.databases, m[1])
		return nil
	}},
	{pattern(`DROP ROLE IF EXISTS ` + ident + `;`), func(x *execution, m []string) error {
		for _, name := range slices.Sorted(maps.Keys(x.sv.databases)) {
			if x.sv.databases[name].owner == m[1] {
				return fmt.Errorf("ERROR:  role %q cannot be dropped because some objects depend on it\nDETAIL:  owner of database %s", m[1], name)
			}
		}
		delete(x.sv.users, m[1])
		return nil
	}},
	{pattern(`SELECT pg_terminate_backend\(pid\) FROM pg_stat_activity WHERE datname IN \(` + str + `, ` + str + `\) AND pid <> pg_backend_pid\(\);`),
		func(x *execution, m []string) error {
			for _, name := range m[1:] {
				x.named(name)
				if db := x.sv.databases[name]; db != nil {
					for range db.sessions {
						x.printf("t\n")
					}
					db.sessions = nil
				}
			}
			return nil
		}},
	{pattern(`SET lock_timeout = '[^']*';`), func(*execution, []string) error { return nil }},
	{pattern(`BEGIN;`), func(x *execution, _ []string) error {
		x.begun = x.sv.clone()
		return nil
	}},
	{pattern(`COMMIT;`), func(x *execution, _ []string) error {
		x.begun = nil
		return nil
	}},
	{pattern(`ALTER DATABASE ` + ident + ` RENAME TO ` + ident + `;`), func(x *execution, m []string) error {
		x.named(m[1], m[2])
		if err := x.exists(m[1]); err != nil {
			return err
		}
		if x.s.busy > 0 {
			x.s.busy--
			return fmt.Errorf("ERROR:  database %q is being accessed by other users\nDETAIL:  There is 1 other session using the database", m[1])
		}
		if x.sv.databases[m[2]] != nil {
			return fmt.Errorf("ERROR:  database %q already exists", m[2])
		}
		x.sv.databases[m[2]] = x.sv.databases[m[1]]
		delete(x.sv.databases, m[1])
		return nil
	}},
	{pattern(`ALTER DATABASE ` + ident + ` WITH ALLOW_CONNECTIONS false;`), func(x *execution, m []string) error {
		if err := x.exists(m[1]); err != nil {
			return err
		}
		x.sv.databases[m[1]].closed = true
		return nil
	}},
	{pattern(`SELECT count\(pg_terminate_backend\(pid\)\) FROM pg_stat_activity WHERE datname = ` + str + ` AND pid <> pg_backend_pid\(\);`),
		func(x *execution, m []string) error {
			x.named(m[1])
			ended := 0
			if db := x.sv.databases[m[1]]; db != nil {
				ended = len(db.sessions)
				db.sessions = nil
			}
			x.printf("%d\n", ended)
			return nil
		}},
	{pattern(`SELECT unnest\(s.setconfig\) FROM pg_db_role_setting s JOIN pg_database d ON d.oid = s.setdatabase WHERE d.datname = ` + str + `;`),
		func(x *execution, m []string) error {
			x.named(m[1])
			if db := x.sv.databases[m[1]]; db != nil {
				for _, setting := range db.settings {
					x.printf("%s\n", setting)
				}
			}
			return nil
		}},
	{pattern(`SELECT COALESCE\(host\(client_addr\), 'local'\) FROM pg_stat_activity WHERE datname = ` + str + ` AND pid <> pg_backend_pid\(\);`),
		func(x *execution, m []string) error {
			x.sessions(m[1])
			return nil
		}},
	{pattern(`SELECT datname FROM pg_database WHERE NOT datistemplate AND datname <> 'postgres';`), func(x *execution, _ []string) error {
		x.names()
		return nil
	}},
}

var mariadbStatements = []statement{
	{pattern(`SELECT 'database', COUNT\(\*\) FROM information_schema.schemata WHERE schema_name = ` + str + `;`), func(x *execution, m []string) error {
		x.named(m[1])
		x.printf("database\t%d\n", boolCount(x.sv.databases[m[1]] != nil))
		return nil
	}},
	{pattern(`SELECT 'user', COUNT\(\*\) FROM mysql.user WHERE user = ` + str + `;`), func(x *execution, m []string) error {
		_, ok := x.sv.users[m[1]]
		x.printf("user\t%d\n", boolCount(ok))
		return nil
	}},
	{pattern(`CREATE DATABASE IF NOT EXISTS ` + mident + ` CHARACTER SET (\S+) COLLATE (\S+);`), func(x *execution, m []string) error {
		x.named(m[1])
		if x.sv.databases[m[1]] == nil {
			return x.create(m[1], "", databases.Charset{Set: m[2], Collation: m[3]})
		}
		return nil
	}},
	{pattern(`CREATE DATABASE ` + mident + ` CHARACTER SET (\S+) COLLATE (\S+);`), func(x *execution, m []string) error {
		x.named(m[1])
		if x.sv.databases[m[1]] != nil {
			return fmt.Errorf("ERROR 1007 (HY000) at line %d: Can't create database '%s'; database exists", x.line, m[1])
		}
		return x.create(m[1], "", databases.Charset{Set: m[2], Collation: m[3]})
	}},
	{pattern(`CREATE USER IF NOT EXISTS ` + str + `@'%' IDENTIFIED BY ` + str + `;`), func(x *execution, m []string) error {
		if _, ok := x.sv.users[m[1]]; !ok {
			x.sv.users[m[1]] = m[2]
		}
		return nil
	}},
	{pattern(`ALTER USER ` + str + `@'%' IDENTIFIED BY ` + str + `;`), func(x *execution, m []string) error {
		if _, ok := x.sv.users[m[1]]; !ok {
			return fmt.Errorf("ERROR 1396 (HY000) at line %d: Operation ALTER USER failed for '%s'@'%%'", x.line, m[1])
		}
		x.sv.users[m[1]] = m[2]
		return nil
	}},
	{pattern(`GRANT ALL PRIVILEGES ON ` + mident + `\.\* TO ` + str + `@'%';`), func(x *execution, m []string) error {
		x.named(m[1])
		if _, ok := x.sv.users[m[2]]; !ok {
			return fmt.Errorf("ERROR 1133 (28000) at line %d: Can't find any matching row in the user table", x.line)
		}
		x.sv.grant(m[1], m[2])
		if db := x.sv.databases[m[1]]; db != nil && db.owner == "" {
			db.owner = m[2]
		}
		return nil
	}},
	{pattern(`REVOKE ALL PRIVILEGES ON ` + mident + `\.\* FROM ` + str + `@'%';`), func(x *execution, m []string) error {
		x.named(m[1])
		if !x.sv.grants[m[1]][m[2]] {
			return fmt.Errorf("ERROR 1141 (42000) at line %d: There is no such grant defined for user '%s' on host '%%'", x.line, m[2])
		}
		delete(x.sv.grants[m[1]], m[2])
		return nil
	}},
	{pattern(`DROP DATABASE IF EXISTS ` + mident + `;`), func(x *execution, m []string) error {
		// the grants on it stay, as MariaDB's do
		x.named(m[1])
		delete(x.sv.databases, m[1])
		return nil
	}},
	{pattern(`DROP USER IF EXISTS ` + str + `@'%';`), func(x *execution, m []string) error {
		delete(x.sv.users, m[1])
		for _, users := range x.sv.grants {
			delete(users, m[1])
		}
		return nil
	}},
	{pattern(`SELECT default_character_set_name, default_collation_name FROM information_schema.schemata WHERE schema_name = ` + str + `;`),
		func(x *execution, m []string) error {
			x.named(m[1])
			if db := x.sv.databases[m[1]]; db != nil {
				x.printf("%s\t%s\n", db.charset.Set, db.charset.Collation)
			}
			return nil
		}},
	{pattern(`SELECT SUBSTRING_INDEX\(host, ':', 1\) FROM information_schema.processlist WHERE db = ` + str + ` AND id <> CONNECTION_ID\(\);`),
		func(x *execution, m []string) error {
			x.sessions(m[1])
			return nil
		}},
	{pattern(`SELECT schema_name FROM information_schema.schemata WHERE schema_name NOT IN \(.*\);`), func(x *execution, _ []string) error {
		x.names()
		return nil
	}},
}

// execution is the statements of one process.
type execution struct {
	*process
	out   strings.Builder
	line  int
	begun *server
}

func (x *execution) printf(format string, args ...any) { fmt.Fprintf(&x.out, format, args...) }

func (x *execution) exists(name string) error {
	x.named(name)
	if x.sv.databases[name] == nil {
		return fmt.Errorf("ERROR:  database %q does not exist", name)
	}
	return nil
}

func (x *execution) create(name, owner string, charset databases.Charset) error {
	if owner != "" {
		if _, ok := x.sv.users[owner]; !ok {
			return fmt.Errorf("ERROR:  role %q does not exist", owner)
		}
	}
	x.sv.databases[name] = &database{owner: owner, charset: charset}
	return nil
}

func (x *execution) sessions(name string) {
	x.named(name)
	if db := x.sv.databases[name]; db != nil {
		for _, address := range db.sessions {
			x.printf("%s\n", address)
		}
	}
}

func (x *execution) names() {
	for _, name := range slices.Sorted(maps.Keys(x.sv.databases)) {
		x.printf("%s\n", name)
	}
}

func boolCount(ok bool) int {
	if ok {
		return 1
	}
	return 0
}

// clone is a copy of a server's state, which a failed transaction goes back
// to.
func (sv *server) clone() *server {
	copied := newServer(sv.pg)
	maps.Copy(copied.users, sv.users)
	for name, db := range sv.databases {
		dup := *db
		dup.tables, dup.settings, dup.sessions = slices.Clone(db.tables), slices.Clone(db.settings), slices.Clone(db.sessions)
		copied.databases[name] = &dup
	}
	for name, users := range sv.grants {
		copied.grants[name] = maps.Clone(users)
	}
	return copied
}

// statements runs SQL as the administrator (Statements).
func (p *process) statements(sql string) fakedocker.ExecResult {
	if strings.Contains(sql, "'version'") {
		return p.stats()
	}
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	x := &execution{process: p}
	known := mariadbStatements
	if p.sv.pg {
		known = postgresStatements
	}
	for _, line := range strings.Split(sql, "\n") {
		x.line++
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, candidate := range known {
			match := candidate.pattern.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			if err := candidate.do(x, match); err != nil {
				if x.begun != nil {
					// psql's ON_ERROR_STOP leaves the transaction, which
					// rolls back
					*p.sv = *x.begun
				}
				code := 1
				if p.sv.pg {
					code = 3
				}
				return fakedocker.ExecResult{Stdout: x.out.String(), Stderr: err.Error() + "\n", Code: code}
			}
			break
		}
	}
	return fakedocker.ExecResult{Stdout: x.out.String()}
}

// stats answers StatsSQL: each database's size and sessions, the limit and
// the version.
func (p *process) stats() fakedocker.ExecResult {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	var out strings.Builder
	for index, name := range slices.Sorted(maps.Keys(p.sv.databases)) {
		db := p.sv.databases[name]
		size := db.size
		if size == 0 {
			size = int64(index+3) * 37 << 20
		}
		fmt.Fprintf(&out, "size\t%s\t%d\n", name, size)
		if len(db.sessions) > 0 {
			fmt.Fprintf(&out, "conn\t%s\t%d\n", name, len(db.sessions))
		}
	}
	version, limit := "11.8.2-MariaDB", 200
	if p.sv.pg {
		version, limit = "17.2", 100
	}
	fmt.Fprintf(&out, "max\t\t%s\nversion\t%s\t0\n", strconv.Itoa(limit), version)
	return fakedocker.ExecResult{Stdout: out.String()}
}
