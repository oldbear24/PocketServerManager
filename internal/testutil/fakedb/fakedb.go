// Package fakedb answers what the agent runs in the containers of database
// servers on a fake Engine (internal/dockerapi/fakedocker), as the servers'
// own clients would, for the tests of databases and of their backups and
// restores (#53), and for cmd/fakedocker. It keeps, for each server's
// container, the databases and users that statements create, rename, close
// and drop, with each database's tables, and MariaDB's grants, which outlive
// a dropped database as MariaDB's do.
//
// What it models, by the scripts of internal/databases:
//
//   - Statements: the SQL of the panel's commands, statement by statement;
//     PostgreSQL's psql stops at the first error (ON_ERROR_STOP, exit code 3)
//     and rolls back an open BEGIN, MariaDB's client stops at the first
//     error (exit code 1) and keeps what ran before it. A PostgreSQL role
//     that owns a database cannot be dropped. SetBusyRenames makes the first
//     renames find the database "being accessed by other users".
//   - The dump of one database (DumpCommand): a dump whose header names the
//     database, PostgreSQL's as a fake custom archive (PGDMP, then
//     pg_restore's list of it: a TABLE and a TABLE DATA entry per table) and
//     MariaDB's as SQL with a CREATE TABLE and an INSERT per table, a
//     DEFINER of the database's user, and "-- Dump completed" last.
//     SetDumpContent stands in for one database's next dumps.
//   - CountDump: pg_restore --list of a staged dump (it fails on what is no
//     archive) or MariaDB's footer check, and the tables counted.
//   - RestoreAsUser, with the privilege model of a restore that runs as the
//     database's own user: the user signs in with its password, may write
//     only a database it owns (PostgreSQL) or has a grant on (MariaDB), and
//     a statement of the dump that names another database (USE b, `b`.`t`,
//     CREATE or DROP DATABASE b, \connect b, a DATABASE entry) fails on
//     privileges: "Access denied for user 'a'@'%' to database 'b'". A
//     DEFINER of another user fails unless PC_DEFINER rewrites it. A
//     restore whose database, or the dump's database, has "fail" in its
//     name fails. PostgreSQL's restore is one transaction and changes
//     nothing when it fails; MariaDB's keeps the tables created before the
//     statement that failed.
//   - CountBaseTables: the tables of a database, one fewer when its name, or
//     the name of the database whose dump it holds, has "short" in it.
//   - df (FreeSpace, SetFree), the client's --sandbox (Sandbox,
//     SetSandbox), SignIn, EndSessions (the sessions SetSessions gave a
//     database), RemoveStaged (the file the archive PUT copied in goes),
//     SweepStaged, and #19's Dump, Clone, RestoreFile and CountTables.
//
// Every process is noted (Execs) with its kind and the databases it named.
package fakedb

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
)

// The kinds of the processes a Server notes.
const (
	KindStatements   = "statements"
	KindDump         = "dump"
	KindCountDump    = "count-dump"
	KindRestore      = "restore"
	KindCountTables  = "count-tables"
	KindEndSessions  = "end-sessions"
	KindSignIn       = "sign-in"
	KindFreeSpace    = "free-space"
	KindSandbox      = "sandbox"
	KindRemoveStaged = "remove-staged"
	KindSweepStaged  = "sweep-staged"
	KindClone        = "clone"
	KindExportDump   = "export-dump"
	KindRestoreFile  = "restore-file"
	KindUnknown      = "unknown"
)

// defaultFree is what df says is available, in bytes, unless SetFree says
// otherwise.
const defaultFree = 50 << 30

// Exec is a process the agent ran in a server's container.
type Exec struct {
	// Container is the container's id.
	Container string
	Kind      string
	Cmd       []string
	Env       map[string]string
	// Databases are the databases it named: in its SQL, its PC_DATABASE,
	// its dump's argument, or the dump it restored.
	Databases []string
	// Failed says it ended with an exit code other than 0.
	Failed bool
}

// Database is what a server holds of a database.
type Database struct {
	Name  string
	Owner string
	// Tables are its tables, in the order they were created.
	Tables []string
	// Closed says it takes no connections (ALLOW_CONNECTIONS false).
	Closed bool
	// From is the database whose dump was restored into it last.
	From string
	// Granted are the MariaDB users with privileges on it.
	Granted []string
	Charset databases.Charset
}

// Server is the database servers of a fake Engine.
type Server struct {
	engine *fakedocker.Engine

	mu       sync.Mutex
	servers  map[string]*server
	execs    []Exec
	busy     int
	freeData int64
	freeTmp  int64
	sandbox  bool
	dumps    map[string]string
}

// server is one server's container.
type server struct {
	pg        bool
	databases map[string]*database
	users     map[string]string
	// grants are MariaDB's, by database: they stay when it is dropped
	grants map[string]map[string]bool
}

type database struct {
	owner    string
	tables   []string
	closed   bool
	from     string
	charset  databases.Charset
	settings []string
	sessions []string
	size     int64
}

// New is a Server of an Engine; Install makes it answer the Engine's
// processes.
func New(engine *fakedocker.Engine) *Server {
	return &Server{engine: engine, servers: map[string]*server{}, sandbox: true, freeData: defaultFree, freeTmp: defaultFree, dumps: map[string]string{}}
}

// Install makes a new Server answer every process of the Engine without a
// terminal, and returns it.
func Install(engine *fakedocker.Engine) *Server {
	s := New(engine)
	engine.OnExec = s.Answer
	return s
}

// AddServer adds a running shared server's container, labelled
// pocketcloud.role=db, of an engine (postgres or mariadb) with its default
// image, and returns its id.
func (s *Server) AddServer(name, engine string) string {
	image := databases.Postgres.Image
	if engine == databases.MariaDB.Name {
		image = databases.MariaDB.Image
	}
	id := s.engine.AddContainer(name, image, map[string]string{"pocketcloud.role": "db"})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.servers[id] = newServer(engine != databases.MariaDB.Name)
	return id
}

func newServer(pg bool) *server {
	return &server{pg: pg, databases: map[string]*database{}, users: map[string]string{}, grants: map[string]map[string]bool{}}
}

// AddDatabase adds a database of a server's container (its name or id) and
// its user with a password, who owns it, or has every privilege on it, and
// its tables.
func (s *Server) AddDatabase(container, name, user, password string, tables ...string) {
	id := s.idOf(container)
	sv := s.server(id, "", nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	if user != "" {
		sv.users[user] = password
		sv.grant(name, user)
	}
	sv.databases[name] = &database{owner: user, tables: slices.Clone(tables), charset: databases.DefaultCharset}
}

// SetSessions gives a database sessions from these addresses, such as a
// container's.
func (s *Server) SetSessions(container, name string, addresses ...string) {
	s.edit(container, name, func(db *database) { db.sessions = slices.Clone(addresses) })
}

// SetSettings gives a PostgreSQL database settings, name=value each.
func (s *Server) SetSettings(container, name string, settings ...string) {
	s.edit(container, name, func(db *database) { db.settings = slices.Clone(settings) })
}

// SetCharset gives a MariaDB database a character set and collation.
func (s *Server) SetCharset(container, name string, charset databases.Charset) {
	s.edit(container, name, func(db *database) { db.charset = charset })
}

// SetSize gives a database a size in bytes.
func (s *Server) SetSize(container, name string, size int64) {
	s.edit(container, name, func(db *database) { db.size = size })
}

// SetTables replaces a database's tables.
func (s *Server) SetTables(container, name string, tables ...string) {
	s.edit(container, name, func(db *database) { db.tables = slices.Clone(tables) })
}

func (s *Server) edit(container, name string, change func(*database)) {
	id := s.idOf(container)
	sv := s.server(id, "", nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	if db := sv.databases[name]; db != nil {
		change(db)
	}
}

// SetFree is what df says is available in the data directory and /tmp of
// every server, in bytes.
func (s *Server) SetFree(data, tmp int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.freeData, s.freeTmp = data, tmp
}

// SetBusyRenames makes the next n renames of a database find it being
// accessed by other users, as a session that came back does.
func (s *Server) SetBusyRenames(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy = n
}

// SetSandbox says whether the MariaDB client takes --sandbox; it does
// unless told otherwise.
func (s *Server) SetSandbox(sandbox bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sandbox = sandbox
}

// SetDumpContent makes every dump of a database of a server's container
// print content, as a crafted backup would hold it; "" goes back to the
// database's own dump.
func (s *Server) SetDumpContent(container, name, content string) {
	id := s.idOf(container)
	s.mu.Lock()
	defer s.mu.Unlock()
	if content == "" {
		delete(s.dumps, id+"/"+name)
		return
	}
	s.dumps[id+"/"+name] = content
}

// Database is a database of a server's container (its name or id), and
// whether it exists.
func (s *Server) Database(container, name string) (Database, bool) {
	id := s.idOf(container)
	s.mu.Lock()
	defer s.mu.Unlock()
	sv := s.servers[id]
	if sv == nil || sv.databases[name] == nil {
		return Database{}, false
	}
	db := sv.databases[name]
	return Database{Name: name, Owner: db.owner, Tables: slices.Clone(db.tables), Closed: db.closed, From: db.from,
		Granted: slices.Sorted(maps.Keys(sv.grants[name])), Charset: db.charset}, true
}

// DatabaseNames are the databases of a server's container, sorted.
func (s *Server) DatabaseNames(container string) []string {
	id := s.idOf(container)
	s.mu.Lock()
	defer s.mu.Unlock()
	sv := s.servers[id]
	if sv == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(sv.databases))
}

// Password is a user's password on a server's container, and whether the
// user exists.
func (s *Server) Password(container, user string) (string, bool) {
	id := s.idOf(container)
	s.mu.Lock()
	defer s.mu.Unlock()
	sv := s.servers[id]
	if sv == nil {
		return "", false
	}
	password, ok := sv.users[user]
	return password, ok
}

// Execs are the processes run so far, oldest first.
func (s *Server) Execs() []Exec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.execs)
}

// idOf is the id of a container named by its name or id.
func (s *Server) idOf(container string) string {
	if found := s.engine.Find(container); found != nil {
		return found.ID
	}
	return container
}

// server is the state of a container, made on its first process: its engine
// from its image, else from what the process says, and the databases its
// image creates from its environment.
func (s *Server) server(id, script string, env map[string]string) *server {
	s.mu.Lock()
	if sv := s.servers[id]; sv != nil {
		s.mu.Unlock()
		return sv
	}
	s.mu.Unlock()
	var image string
	seeded := map[string]bool{}
	for _, name := range s.engine.Names() {
		container := s.engine.Find(name)
		if container == nil || container.ID != id {
			continue
		}
		image = container.Image
		values := map[string]string{}
		list, _ := container.Config["Env"].([]any)
		for _, item := range list {
			key, value, _ := strings.Cut(fmt.Sprint(item), "=")
			values[key] = value
		}
		for _, key := range []string{"POSTGRES_DB", "MARIADB_DATABASE", "MYSQL_DATABASE"} {
			if values[key] != "" {
				seeded[values[key]] = true
			}
		}
		if values["POSTGRES_DB"] == "" && values["POSTGRES_USER"] != "" {
			seeded[values["POSTGRES_USER"]] = true
		}
	}
	pg := true
	switch {
	case strings.Contains(image, "mariadb"), strings.Contains(image, "mysql"):
		pg = false
	case strings.Contains(image, "postgres"):
	case env["MYSQL_PWD"] != "", strings.Contains(script, `"$client"`), strings.Contains(script, "mariadb-dump"):
		pg = false
	}
	sv := newServer(pg)
	for name := range seeded {
		sv.databases[name] = &database{charset: databases.DefaultCharset}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.servers[id]; existing != nil {
		return existing
	}
	s.servers[id] = sv
	return sv
}

// Answer answers a process of a container, as fakedocker's OnExec.
func (s *Server) Answer(call *fakedocker.ExecCall, _ []byte) fakedocker.ExecResult {
	env := map[string]string{}
	for _, item := range call.Env {
		key, value, _ := strings.Cut(item, "=")
		env[key] = value
	}
	script := ""
	if len(call.Cmd) >= 3 && call.Cmd[0] == "sh" && call.Cmd[1] == "-c" {
		script = call.Cmd[2]
	}
	sv := s.server(call.Container, script, env)
	p := &process{s: s, sv: sv, container: call.Container, cmd: call.Cmd, env: env, script: script}
	result := p.run()
	s.mu.Lock()
	s.execs = append(s.execs, Exec{Container: call.Container, Kind: p.kind, Cmd: slices.Clone(call.Cmd), Env: env,
		Databases: p.names, Failed: result.Code != 0})
	s.mu.Unlock()
	return result
}

// process is one process the Server answers.
type process struct {
	s         *Server
	sv        *server
	container string
	cmd       []string
	env       map[string]string
	script    string
	kind      string
	names     []string
}

// named notes a database the process named.
func (p *process) named(names ...string) {
	for _, name := range names {
		if name != "" && !slices.Contains(p.names, name) {
			p.names = append(p.names, name)
		}
	}
}

func (p *process) run() fakedocker.ExecResult {
	script, env := p.script, p.env
	switch {
	case strings.Contains(script, "pg_dump --format=custom") || strings.Contains(script, "--skip-extended-insert"):
		p.kind = KindDump
		return p.dump()
	case strings.Contains(script, "KILL CONNECTION"):
		p.kind = KindEndSessions
		return p.endMariaDBSessions()
	case strings.Contains(script, "pg_restore --list") || strings.Contains(script, "-- Dump completed"):
		p.kind = KindCountDump
		return p.countDump()
	case strings.Contains(script, "pg_restore --no-password") || strings.Contains(script, "--one-database"):
		p.kind = KindRestore
		return p.restore()
	case strings.Contains(script, "BASE TABLE"):
		p.kind = KindCountTables
		return p.countTables(true)
	case strings.Contains(script, "information_schema.tables"):
		p.kind = KindCountTables
		return p.countTables(false)
	case script == `rm -f "$PC_FILE"`:
		p.kind = KindRemoveStaged
		p.s.engine.RemoveFile(p.container, env["PC_FILE"])
		return fakedocker.ExecResult{}
	case strings.Contains(script, "find /tmp -maxdepth 1"):
		p.kind = KindSweepStaged
		return fakedocker.ExecResult{}
	case strings.Contains(script, "df -Pk"):
		p.kind = KindFreeSpace
		return p.freeSpace()
	case strings.Contains(script, "--sandbox") && strings.Contains(script, "echo yes"):
		p.kind = KindSandbox
		p.s.mu.Lock()
		defer p.s.mu.Unlock()
		if p.s.sandbox {
			return fakedocker.ExecResult{Stdout: "yes\n"}
		}
		return fakedocker.ExecResult{Stdout: "no\n"}
	case strings.Contains(script, `"SELECT 1"`):
		p.kind = KindSignIn
		return p.signIn()
	case strings.HasPrefix(env["PC_SQL"], "SELECT count(pg_terminate_backend(pid))"):
		// EndSessions on PostgreSQL, one statement
		p.kind = KindEndSessions
		return p.statements(env["PC_SQL"])
	case env["PC_SQL"] != "":
		p.kind = KindStatements
		return p.statements(env["PC_SQL"])
	case env["PC_SOURCE"] != "":
		p.kind = KindClone
		return p.clone()
	case env["PC_FILE"] != "" && env["PC_OWNER"] != "":
		p.kind = KindRestoreFile
		return p.restoreFile()
	case env["PC_DATABASE"] != "":
		p.kind = KindExportDump
		return p.exportDump()
	}
	p.kind = KindUnknown
	return fakedocker.ExecResult{Code: 127, Stderr: "fakedb: no such program\n"}
}

// password is the password a process signs in with.
func (p *process) password() string {
	if p.sv.pg {
		return p.env["PGPASSWORD"]
	}
	return p.env["MYSQL_PWD"]
}

// signedIn says why a user cannot sign in to a database over the
// container's address, if it cannot.
func (p *process) signedIn(user, password, name string) string {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	known, ok := p.sv.users[user]
	if !ok || known != password {
		if p.sv.pg {
			return fmt.Sprintf("FATAL:  password authentication failed for user %q", user)
		}
		return fmt.Sprintf("ERROR 1045 (28000): Access denied for user '%s'@'172.17.0.2' (using password: YES)", user)
	}
	db := p.sv.databases[name]
	switch {
	case db == nil && p.sv.pg:
		return fmt.Sprintf("FATAL:  database %q does not exist", name)
	case db == nil:
		return fmt.Sprintf("ERROR 1049 (42000): Unknown database '%s'", name)
	case db.closed:
		return fmt.Sprintf("FATAL:  database %q is not currently accepting connections", name)
	case !p.sv.mayWrite(name, user):
		if p.sv.pg {
			return fmt.Sprintf("FATAL:  permission denied for database %q", name)
		}
		return fmt.Sprintf("ERROR 1044 (42000): Access denied for user '%s'@'%%' to database '%s'", user, name)
	}
	return ""
}

// mayWrite says whether a user may write a database: it owns it, or has a
// grant on it.
func (sv *server) mayWrite(name, user string) bool {
	if sv.pg {
		db := sv.databases[name]
		return db != nil && db.owner == user
	}
	return sv.grants[name][user]
}

func (sv *server) grant(name, user string) {
	if sv.pg {
		return
	}
	if sv.grants[name] == nil {
		sv.grants[name] = map[string]bool{}
	}
	sv.grants[name][user] = true
}

func (p *process) signIn() fakedocker.ExecResult {
	name := p.env["PC_DATABASE"]
	p.named(name)
	if why := p.signedIn(p.env["PC_ADMIN_USER"], p.password(), name); why != "" {
		return fakedocker.ExecResult{Code: 1, Stderr: why + "\n"}
	}
	return fakedocker.ExecResult{}
}

func (p *process) freeSpace() fakedocker.ExecResult {
	p.s.mu.Lock()
	data, tmp := p.s.freeData, p.s.freeTmp
	p.s.mu.Unlock()
	line := func(filesystem string, available int64, mounted string) string {
		kib := available / 1024
		return fmt.Sprintf("%-14s %12d %12d %12d %8s %s\n", filesystem, 3*kib, 2*kib, kib, "34%", mounted)
	}
	return fakedocker.ExecResult{Stdout: "Filesystem     1024-blocks         Used    Available Capacity Mounted on\n" +
		line("/dev/sda1", data, p.env["PC_DATA_DIR"]) + line("overlay", tmp, "/")}
}

// postgresArchive starts a fake custom archive.
const postgresArchive = "PGDMP"

// dump prints the dump of one database: DumpCommand's arguments are the
// administrator and the database.
func (p *process) dump() fakedocker.ExecResult {
	if len(p.cmd) < 6 {
		return fakedocker.ExecResult{Code: 2, Stderr: "fakedb: a dump names its administrator and its database\n"}
	}
	name := p.cmd[5]
	p.named(name)
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if content, ok := p.s.dumps[p.container+"/"+name]; ok {
		return fakedocker.ExecResult{Stdout: content}
	}
	db := p.sv.databases[name]
	if db == nil {
		if p.sv.pg {
			return fakedocker.ExecResult{Code: 1, Stderr: fmt.Sprintf("pg_dump: error: connection to server on socket \"/var/run/postgresql/.s.PGSQL.5432\" failed: FATAL:  database %q does not exist\n", name)}
		}
		return fakedocker.ExecResult{Code: 2, Stderr: fmt.Sprintf("mariadb-dump: Got error: 1049: \"Unknown database '%s'\" when selecting the database\n", name)}
	}
	if p.sv.pg {
		return fakedocker.ExecResult{Stdout: PostgresDump(name, db.owner, db.tables...)}
	}
	definer := ""
	if granted := slices.Sorted(maps.Keys(p.sv.grants[name])); len(granted) > 0 {
		definer = granted[0]
	}
	return fakedocker.ExecResult{Stdout: MariaDBDump(name, definer, db.tables...)}
}

// PostgresDump is a fake custom archive of a PostgreSQL database with
// tables, owned by owner: its list, as pg_restore --list prints it.
func PostgresDump(name, owner string, tables ...string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "%s fake custom-format archive\n;\n; Archive created at 2026-11-18 01:30:00 UTC\n;     dbname: %s\n;     TOC Entries: %d\n;\n", postgresArchive, name, 2*len(tables))
	for i, table := range tables {
		fmt.Fprintf(&out, "%d; 1259 %d TABLE public %s %s\n", 215+i, 16385+i, table, owner)
	}
	for i, table := range tables {
		fmt.Fprintf(&out, "%d; 0 %d TABLE DATA public %s %s\n", 3301+i, 16385+i, table, owner)
	}
	return out.String()
}

// MariaDBDump is a fake SQL dump of a MariaDB database with tables, as
// DumpCommand writes one: a table and a row each, and a trigger whose
// definer is definer, if one is given.
func MariaDBDump(name, definer string, tables ...string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "-- MariaDB dump 10.19  Distrib 11.8.2-MariaDB, for debian-linux-gnu (x86_64)\n--\n-- Host: localhost    Database: %s\n", name)
	out.WriteString("-- ------------------------------------------------------\n-- Server version\t11.8.2-MariaDB\n\n")
	for _, table := range tables {
		fmt.Fprintf(&out, "DROP TABLE IF EXISTS `%s`;\nCREATE TABLE `%s` (\n  `id` int(11) NOT NULL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;\n", table, table)
		fmt.Fprintf(&out, "set autocommit=0;\nINSERT INTO `%s` VALUES (1);\ncommit;\n", table)
	}
	if definer != "" && len(tables) > 0 {
		fmt.Fprintf(&out, "/*!50003 CREATE*/ /*!50017 DEFINER=`%s`@`%%`*/ /*!50003 TRIGGER `%s_touch` BEFORE INSERT ON `%s` FOR EACH ROW SET NEW.id = NEW.id */;;\n", definer, tables[0], tables[0])
	}
	out.WriteString("-- Dump completed\n")
	return out.String()
}

// staged is the file a process reads, as the archive PUT copied it in.
func (p *process) staged() ([]byte, string) {
	file := p.env["PC_FILE"]
	content := p.s.engine.File(p.container, file)
	if content == nil {
		return nil, file + ": No such file or directory"
	}
	return content, ""
}

// tocTable reads a TABLE entry of a fake archive's list: its kind, the
// table, and whether it is the table itself.
var tocEntry = regexp.MustCompile(`^\d+; \d+ \d+ (\S+) (\S+) (\S+)(?: (\S+))?(?: (\S+))?$`)

// archive reads a fake custom archive: the database it is of, its tables,
// and the databases its entries name besides.
func archive(content string) (source string, tables, others []string, ok bool) {
	if !strings.HasPrefix(content, postgresArchive) {
		return "", nil, nil, false
	}
	for _, line := range strings.Split(content, "\n") {
		if rest, found := strings.CutPrefix(line, ";     dbname: "); found {
			source = strings.TrimSpace(rest)
			continue
		}
		if rest, found := strings.CutPrefix(line, `\connect `); found {
			others = append(others, strings.TrimSpace(rest))
			continue
		}
		match := tocEntry.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		switch {
		case match[1] == "TABLE" && match[2] != "DATA" && match[2] != "ATTACH":
			tables = append(tables, match[3])
		case match[1] == "DATABASE":
			others = append(others, match[2])
		}
	}
	return source, tables, others, true
}

// mariadbDumpOf reads the database a MariaDB dump is of.
func mariadbDumpOf(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if _, rest, found := strings.Cut(line, "Database: "); found && strings.HasPrefix(line, "-- Host:") {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

var (
	createTable = regexp.MustCompile("^CREATE TABLE `?([A-Za-z0-9_]+)`?")
	useDatabase = regexp.MustCompile("^USE `?([A-Za-z0-9_]+)`?\\s*;")
	qualified   = regexp.MustCompile("`([A-Za-z0-9_]+)`\\.`")
	dropCreate  = regexp.MustCompile("(?i)^(?:CREATE|DROP|ALTER) DATABASE (?:IF (?:NOT )?EXISTS )?`?([A-Za-z0-9_]+)`?")
	definerOf   = regexp.MustCompile("DEFINER=`([^`]+)`@")
)

func (p *process) countDump() fakedocker.ExecResult {
	content, missing := p.staged()
	if p.sv.pg {
		if content == nil {
			return fakedocker.ExecResult{Code: 1, Stderr: "pg_restore: error: could not open input file \"" + missing + "\"\npg_restore cannot read the dump's list\n"}
		}
		source, tables, _, ok := archive(string(content))
		p.named(source)
		if !ok {
			return fakedocker.ExecResult{Code: 1, Stderr: "pg_restore: error: input file does not appear to be a valid archive\npg_restore cannot read the dump's list\n"}
		}
		return fakedocker.ExecResult{Stdout: strconv.Itoa(len(tables)) + "\n"}
	}
	if content == nil {
		return fakedocker.ExecResult{Code: 1, Stderr: "tail: cannot open '" + missing + "'\nthe dump is incomplete: it does not end as a whole dump does\n"}
	}
	p.named(mariadbDumpOf(string(content)))
	tail := content[max(0, len(content)-4096):]
	complete := slices.ContainsFunc(strings.Split(string(tail), "\n"), func(line string) bool { return strings.HasPrefix(line, "-- Dump completed") })
	if !complete {
		return fakedocker.ExecResult{Code: 1, Stderr: "the dump is incomplete: it does not end as a whole dump does\n"}
	}
	count := 0
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "CREATE TABLE ") {
			count++
		}
	}
	return fakedocker.ExecResult{Stdout: strconv.Itoa(count) + "\n"}
}

// restore restores a staged dump as a database's own user (RestoreAsUser).
func (p *process) restore() fakedocker.ExecResult {
	user, target, definer := p.env["PC_USER"], p.env["PC_DATABASE"], p.env["PC_DEFINER"]
	p.named(target)
	content, missing := p.staged()
	if content == nil {
		if p.sv.pg {
			return fakedocker.ExecResult{Code: 1, Stderr: "pg_restore: error: could not open input file \"" + missing + "\"\n"}
		}
		return fakedocker.ExecResult{Code: 2, Stderr: "sh: 1: cannot open " + missing + "\n"}
	}
	if why := p.signedIn(user, p.password(), target); why != "" {
		if p.sv.pg {
			why = "pg_restore: error: connection to server at \"172.17.0.2\", port 5432 failed: " + why
		}
		return fakedocker.ExecResult{Code: 1, Stderr: why + "\n"}
	}
	if p.sv.pg {
		return p.restorePostgres(user, target, string(content))
	}
	return p.restoreMariaDB(user, target, definer, string(content))
}

func (p *process) restorePostgres(user, target, content string) fakedocker.ExecResult {
	source, tables, others, ok := archive(content)
	p.named(source)
	if !ok {
		return fakedocker.ExecResult{Code: 1, Stderr: "pg_restore: error: input file does not appear to be a valid archive\n"}
	}
	for _, other := range others {
		if other != target {
			p.named(other)
			return fakedocker.ExecResult{Code: 1, Stderr: fmt.Sprintf("pg_restore: error: could not execute query: ERROR:  permission denied for database %s\n", other)}
		}
	}
	if strings.Contains(target, "fail") || strings.Contains(source, "fail") {
		return fakedocker.ExecResult{Code: 1, Stderr: "pg_restore: error: could not execute query: ERROR:  syntax error at or near \"fail\"\n"}
	}
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	db := p.sv.databases[target]
	for _, table := range tables {
		if slices.Contains(db.tables, table) {
			return fakedocker.ExecResult{Code: 1, Stderr: fmt.Sprintf("pg_restore: error: could not execute query: ERROR:  relation \"public.%s\" already exists\n", table)}
		}
	}
	db.tables = append(db.tables, tables...)
	db.from = source
	return fakedocker.ExecResult{}
}

func (p *process) restoreMariaDB(user, target, definer, content string) fakedocker.ExecResult {
	source := mariadbDumpOf(content)
	p.named(source)
	failing := strings.Contains(target, "fail") || strings.Contains(source, "fail")
	lines := strings.Split(content, "\n")
	last := -1
	for i, line := range lines {
		if createTable.MatchString(line) {
			last = i
		}
	}
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	db := p.sv.databases[target]
	denied := func(at int, other string) fakedocker.ExecResult {
		p.named(other)
		return fakedocker.ExecResult{Code: 1, Stderr: fmt.Sprintf("ERROR 1044 (42000) at line %d: Access denied for user '%s'@'%%' to database '%s'\n", at+1, user, other)}
	}
	for i, line := range lines {
		if match := useDatabase.FindStringSubmatch(line); match != nil && match[1] != target {
			return denied(i, match[1])
		}
		if match := dropCreate.FindStringSubmatch(line); match != nil && match[1] != target {
			return denied(i, match[1])
		}
		for _, match := range qualified.FindAllStringSubmatch(line, -1) {
			if match[1] != target {
				return denied(i, match[1])
			}
		}
		if match := definerOf.FindStringSubmatch(line); match != nil {
			who := match[1]
			if definer != "" && !strings.HasPrefix(line, "INSERT INTO ") {
				who = definer
			}
			if who != user {
				return fakedocker.ExecResult{Code: 1, Stderr: fmt.Sprintf("ERROR 1227 (42000) at line %d: Access denied; you need (at least one of) the SUPER, SET USER privilege(s) for this operation\n", i+1)}
			}
		}
		if match := createTable.FindStringSubmatch(line); match != nil {
			if failing && i == last {
				return fakedocker.ExecResult{Code: 1, Stderr: fmt.Sprintf("ERROR 1064 (42000) at line %d: You have an error in your SQL syntax; check the manual that corresponds to your MariaDB server version for the right syntax to use near 'fail' at line 1\n", i+1)}
			}
			if !slices.Contains(db.tables, match[1]) {
				db.tables = append(db.tables, match[1])
			}
		}
	}
	db.from = source
	return fakedocker.ExecResult{}
}

// countTables prints a database's tables; base, as CountBaseTables counts
// them, one fewer for a "short" database.
func (p *process) countTables(base bool) fakedocker.ExecResult {
	name := p.env["PC_DATABASE"]
	p.named(name)
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	db := p.sv.databases[name]
	if db == nil {
		if p.sv.pg {
			return fakedocker.ExecResult{Code: 2, Stderr: fmt.Sprintf("psql: error: connection to server on socket \"/var/run/postgresql/.s.PGSQL.5432\" failed: FATAL:  database %q does not exist\n", name)}
		}
		return fakedocker.ExecResult{Code: 1, Stderr: fmt.Sprintf("ERROR 1049 (42000): Unknown database '%s'\n", name)}
	}
	count := len(db.tables)
	if base && (strings.Contains(name, "short") || strings.Contains(db.from, "short")) {
		count = max(0, count-1)
	}
	return fakedocker.ExecResult{Stdout: strconv.Itoa(count) + "\n"}
}

// endMariaDBSessions is EndSessions on MariaDB: it kills the database's
// sessions and prints how many.
func (p *process) endMariaDBSessions() fakedocker.ExecResult {
	match := regexp.MustCompile(`WHERE db = '([^']*)'`).FindStringSubmatch(p.env["PC_SQL"])
	if match == nil {
		return fakedocker.ExecResult{Code: 1, Stderr: "fakedb: no database to end the sessions of\n"}
	}
	p.named(match[1])
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	ended := 0
	if db := p.sv.databases[match[1]]; db != nil {
		ended = len(db.sessions)
		db.sessions = nil
	}
	return fakedocker.ExecResult{Stdout: strconv.Itoa(ended) + "\n"}
}

func (p *process) clone() fakedocker.ExecResult {
	source, target := p.env["PC_SOURCE"], p.env["PC_TARGET"]
	p.named(source, target)
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	from := p.sv.databases[source]
	to := p.sv.databases[target]
	if to == nil {
		to = &database{owner: p.env["PC_OWNER"], charset: databases.DefaultCharset}
		p.sv.databases[target] = to
	}
	if from != nil {
		to.tables = slices.Clone(from.tables)
	}
	return fakedocker.ExecResult{}
}

// exportDump is #19's plain dump of a database, for an export, a drop's
// final dump and a move: a CREATE TABLE line per table.
func (p *process) exportDump() fakedocker.ExecResult {
	name := p.env["PC_DATABASE"]
	p.named(name)
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	tables := []string{"orders"}
	if db := p.sv.databases[name]; db != nil && len(db.tables) > 0 {
		tables = db.tables
	}
	var out strings.Builder
	fmt.Fprintf(&out, "-- a made-up dump of %s\n", name)
	for _, table := range tables {
		fmt.Fprintf(&out, "CREATE TABLE %s (id int);\n", table)
	}
	return fakedocker.ExecResult{Stdout: out.String()}
}

// restoreFile is #19's restore of a plain dump as the administrator, which
// removes the file.
func (p *process) restoreFile() fakedocker.ExecResult {
	name := p.env["PC_DATABASE"]
	p.named(name)
	content, _ := p.staged()
	p.s.engine.RemoveFile(p.container, p.env["PC_FILE"])
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	db := p.sv.databases[name]
	if db == nil {
		return fakedocker.ExecResult{Code: 1, Stderr: fmt.Sprintf("database %q does not exist\n", name)}
	}
	for _, line := range strings.Split(string(content), "\n") {
		if match := createTable.FindStringSubmatch(line); match != nil && !slices.Contains(db.tables, match[1]) {
			db.tables = append(db.tables, match[1])
		}
	}
	return fakedocker.ExecResult{}
}
