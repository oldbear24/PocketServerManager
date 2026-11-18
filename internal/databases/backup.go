package databases

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// Per-database backups, restores and restore tests (#53). Both the panel and
// the agent use what is here: the panel puts a database's dump command into a
// backup run, and the agent runs the rest in the shared server's container,
// with the server's own client. As with Script, the command lines are fixed:
// whatever varies, and every password, travels in the environment, but for
// the dump command, which has no password at all and takes the
// administrator's name and the database's as positional arguments. Every name
// that goes into SQL text is checked first, and quoted as well. See "Per-
// database backups and restore tests (#53)" in docs/architecture.md.

// The extensions of a database job's one file in its snapshots: PostgreSQL's
// custom-format dump, and MariaDB's SQL.
const (
	PostgresDumpExtension = ".pgdump"
	MariaDBDumpExtension  = ".sql"
)

// BackupFile is the name of a database's dump in its job's snapshots:
// <database>.pgdump or <database>.sql. The snapshot's one path is
// "/"+BackupFile.
func BackupFile(engine Engine, database string) string {
	if engine.Name == MariaDB.Name {
		return database + MariaDBDumpExtension
	}
	return database + PostgresDumpExtension
}

// FileDatabase is the database whose dump a snapshot's file is, as
// BackupFile names it: the extension must be the engine's, and the rest a
// database's name.
func FileDatabase(engine Engine, file string) (string, error) {
	extension := PostgresDumpExtension
	if engine.Name == MariaDB.Name {
		extension = MariaDBDumpExtension
	}
	database, ok := strings.CutSuffix(file, extension)
	if !ok || !databaseNamePattern.MatchString(database) {
		return "", fmt.Errorf("%q is not the dump of a %s database: <database>%s", file, engine.Label, extension)
	}
	return database, nil
}

// The dump commands of a database job's run (BackupRun.stdin_command). They
// run in the shared server's container, with the password of its own
// environment, which the server's Compose file sets from the panel's secret:
// the run carries none. The administrator's name ($1) and the database's
// ($2) are positional arguments: never --databases or --all-databases, so
// the dump has no CREATE DATABASE and no USE, and restores into any name.
// PostgreSQL's is custom format and uncompressed, so that restic's chunks
// find what did not change; MariaDB's is one row per INSERT, in the primary
// key's order, without the date, and one transaction per table on restore.
const (
	postgresDump = `PGPASSWORD="${POSTGRES_PASSWORD:-}" exec pg_dump --format=custom --compress=0 --lock-wait-timeout=10min --username="$1" --dbname="$2"`
	mariadbDump  = `if command -v mariadb-dump >/dev/null 2>&1; then d=mariadb-dump; else d=mysqldump; fi; ` +
		`MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-$MYSQL_ROOT_PASSWORD}" exec "$d" --user="$1" --single-transaction --routines --triggers --events ` +
		`--hex-blob --skip-extended-insert --no-autocommit --order-by-primary --skip-dump-date --default-character-set=utf8mb4 "$2"`
)

// DumpCommand dumps one database of a shared server to the standard output,
// for restic's --stdin-from-command through #29's relay. The database's
// name must be a valid one, and the administrator's a plain name, as they
// are positional arguments of the dump.
func DumpCommand(engine Engine, adminUser, database string) ([]string, error) {
	if !databaseNamePattern.MatchString(adminUser) {
		return nil, fmt.Errorf("%q is not a valid administrator's name", adminUser)
	}
	if err := ValidDatabaseName(engine, database); err != nil {
		return nil, err
	}
	script := postgresDump
	if engine.Name == MariaDB.Name {
		script = mariadbDump
	}
	return []string{"sh", "-c", script, "sh", adminUser, database}, nil
}

// The prefixes of PocketCloud's temporary databases, which
// schema.TemporaryDatabase completes with a record's id: a restore test's
// copy (pc_rt_<check id>), an in-place restore's scratch copy (pc_rs_<restore
// id>), and the data an in-place restore of PostgreSQL replaced
// (pc_old_<restore id>).
const (
	TemporaryTest     = "pc_rt_"
	TemporaryScratch  = "pc_rs_"
	TemporaryPrevious = "pc_old_"
)

// TemporaryPrefixes are the prefixes a new database's name may not start
// with.
var TemporaryPrefixes = []string{TemporaryTest, TemporaryScratch, TemporaryPrevious}

// ReservedPrefixProblem is why a new database may not take a name of the
// temporary namespace.
const ReservedPrefixProblem = "Names that start with pc_rt_, pc_rs_ or pc_old_ are PocketCloud's own temporary databases."

// ReservedPrefix says why a name is not for a new database, a restore into
// a new database or a database PocketCloud keeps: it starts with a prefix of
// the temporary namespace. ValidDatabaseName lets such names pass, as the
// temporary databases themselves take them. The error is
// ReservedPrefixProblem as an error's text, which the panel's sentence()
// gives back.
func ReservedPrefix(name string) error {
	for _, prefix := range TemporaryPrefixes {
		if strings.HasPrefix(name, prefix) {
			return errors.New(reservedPrefixError)
		}
	}
	return nil
}

const reservedPrefixError = "names that start with pc_rt_, pc_rs_ or pc_old_ are PocketCloud's own temporary databases"

// ValidTemporaryName says what is wrong with the name of a temporary
// database, if anything: it is ^pc_(rt|rs|old)_[a-z0-9]{15}$ and nothing
// else. Nothing drops a database outside the namespace as temporary.
func ValidTemporaryName(name string) error {
	if !schema.TemporaryDatabase.MatchString(name) {
		return fmt.Errorf("%q is not one of PocketCloud's temporary databases: pc_rt_, pc_rs_ or pc_old_ and 15 lower case letters or digits", name)
	}
	return nil
}

// TestDatabaseName is the temporary database of a restore test.
func TestDatabaseName(checkID string) string { return TemporaryTest + checkID }

// ScratchName is the scratch copy of an in-place restore.
func ScratchName(restoreID string) string { return TemporaryScratch + restoreID }

// PreviousName is the copy of the data an in-place restore of PostgreSQL
// replaced.
func PreviousName(restoreID string) string { return TemporaryPrevious + restoreID }

// TemporaryOwner splits a temporary database's name into its prefix and the
// id of the record it belongs to: the restore test's check, or the restore.
func TemporaryOwner(name string) (prefix, id string, ok bool) {
	if ValidTemporaryName(name) != nil {
		return "", "", false
	}
	for _, prefix := range TemporaryPrefixes {
		if id, found := strings.CutPrefix(name, prefix); found {
			return prefix, id, true
		}
	}
	return "", "", false
}

// temporaryOf checks a temporary name of one of the prefixes.
func temporaryOf(name string, prefixes ...string) error {
	if err := ValidTemporaryName(name); err != nil {
		return err
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return nil
		}
	}
	return fmt.Errorf("%q is not a temporary database of this kind: %s", name, strings.Join(prefixes, " or "))
}

// validUser checks the name of a database's user, which is a database's
// name too.
func validUser(user string) error {
	if !databaseNamePattern.MatchString(user) {
		return fmt.Errorf("%q is not a valid user name", user)
	}
	return nil
}

// Charset is a MariaDB database's character set and collation, which its
// scratch copy, its restore test's copy and its recreation take. A name that
// is not a plain lower case name never reaches the SQL: the default stands in
// for it.
type Charset struct {
	Set       string
	Collation string
}

// DefaultCharset is what CreateSQL gives a new MariaDB database.
var DefaultCharset = Charset{Set: "utf8mb4", Collation: "utf8mb4_unicode_ci"}

var charsetNamePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

func (c Charset) valid() bool {
	return charsetNamePattern.MatchString(c.Set) && charsetNamePattern.MatchString(c.Collation)
}

func (c Charset) clause() string {
	if !c.valid() {
		c = DefaultCharset
	}
	return "CHARACTER SET " + c.Set + " COLLATE " + c.Collation
}

// CharsetSQL reads a MariaDB database's character set and collation, as a
// row of the two.
func CharsetSQL(database string) string {
	return "SELECT default_character_set_name, default_collation_name FROM information_schema.schemata WHERE schema_name = " +
		mariadbString(database) + ";"
}

// ParseCharset reads the output of CharsetSQL.
func ParseCharset(output string) (Charset, error) {
	for _, line := range strings.Split(output, "\n") {
		set, collation, found := strings.Cut(strings.TrimSpace(line), "\t")
		if !found {
			continue
		}
		charset := Charset{Set: strings.TrimSpace(set), Collation: strings.TrimSpace(collation)}
		if !charset.valid() {
			return Charset{}, fmt.Errorf("the database's character set is not a plain name: %q", line)
		}
		return charset, nil
	}
	return Charset{}, fmt.Errorf("the server did not say the database's character set: %q", output)
}

// ScratchSQL creates an empty temporary database that a user restores into
// and owns: an in-place restore's scratch copy, or a restore test's copy.
// PostgreSQL's is created as CreateSQL creates a database, so that its locale
// and owner are the original's; MariaDB's takes the original's character set
// and collation, and grants the user every privilege on it. It fails when the
// database exists: a temporary database is never reused.
func ScratchSQL(engine Engine, name, user string, charset Charset) (string, error) {
	if err := temporaryOf(name, TemporaryScratch, TemporaryTest); err != nil {
		return "", err
	}
	if err := validUser(user); err != nil {
		return "", err
	}
	if engine.Name == MariaDB.Name {
		return strings.Join([]string{
			"CREATE DATABASE " + mariadbIdent(name) + " " + charset.clause() + ";",
			"GRANT ALL PRIVILEGES ON " + mariadbIdent(name) + ".* TO " + mariadbString(user) + "@'%';",
		}, "\n"), nil
	}
	return strings.Join([]string{
		"CREATE DATABASE " + postgresIdent(name) + " OWNER " + postgresIdent(user) + ";",
		"REVOKE ALL ON DATABASE " + postgresIdent(name) + " FROM PUBLIC;",
	}, "\n"), nil
}

// SwapSQL puts PostgreSQL's restored scratch copy in place of the database,
// on the postgres database, so that neither is the current one: it ends the
// sessions of both, then renames the database to the previous copy and the
// scratch copy to the database in one transaction, and closes the previous
// copy to connections. The agent runs it again, a few times, when a session
// came back between the end and the rename ("is being accessed by other
// users"); ON_ERROR_STOP rolls a failed rename back.
func SwapSQL(database, scratch, previous string) (string, error) {
	if err := ValidDatabaseName(Postgres, database); err != nil {
		return "", err
	}
	if err := ReservedPrefix(database); err != nil {
		return "", fmt.Errorf("%s is a temporary database: %w", database, err)
	}
	if err := temporaryOf(scratch, TemporaryScratch); err != nil {
		return "", err
	}
	if err := temporaryOf(previous, TemporaryPrevious); err != nil {
		return "", err
	}
	if strings.TrimPrefix(scratch, TemporaryScratch) != strings.TrimPrefix(previous, TemporaryPrevious) {
		return "", fmt.Errorf("%s and %s are not of one restore", scratch, previous)
	}
	closing, err := ClosePreviousSQL(previous)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname IN (" + postgresString(database) + ", " +
			postgresString(scratch) + ") AND pid <> pg_backend_pid();",
		"SET lock_timeout = '10s';",
		"BEGIN;",
		"ALTER DATABASE " + postgresIdent(database) + " RENAME TO " + postgresIdent(previous) + ";",
		"ALTER DATABASE " + postgresIdent(scratch) + " RENAME TO " + postgresIdent(database) + ";",
		"COMMIT;",
		closing,
	}, "\n"), nil
}

// ClosePreviousSQL closes the data before a PostgreSQL restore in place to
// connections, as SwapSQL does once the swap committed. The agent runs it on
// its own when the swap's process failed after the commit.
func ClosePreviousSQL(previous string) (string, error) {
	if err := temporaryOf(previous, TemporaryPrevious); err != nil {
		return "", err
	}
	return "ALTER DATABASE " + postgresIdent(previous) + " WITH ALLOW_CONNECTIONS false;", nil
}

// RecreateSQL empties a MariaDB database for an in-place restore: it drops
// it and creates it again with its character set and collation. The grants
// on it survive, as MariaDB keeps them after a drop.
func RecreateSQL(database string, charset Charset) (string, error) {
	if err := ValidDatabaseName(MariaDB, database); err != nil {
		return "", err
	}
	if err := ReservedPrefix(database); err != nil {
		return "", fmt.Errorf("%s is a temporary database: %w", database, err)
	}
	return strings.Join([]string{
		"DROP DATABASE IF EXISTS " + mariadbIdent(database) + ";",
		"CREATE DATABASE " + mariadbIdent(database) + " " + charset.clause() + ";",
	}, "\n"), nil
}

// TestDatabaseSQL creates a restore test's copy, as ScratchSQL does, and
// closes it once the test counted its tables: PostgreSQL takes connections no
// more, MariaDB takes the user's privileges on it away. The copy stays for
// the panel to drop.
func TestDatabaseSQL(engine Engine, name, user string, charset Charset) (create, closing string, err error) {
	if err := temporaryOf(name, TemporaryTest); err != nil {
		return "", "", err
	}
	if create, err = ScratchSQL(engine, name, user, charset); err != nil {
		return "", "", err
	}
	if engine.Name == MariaDB.Name {
		return create, "REVOKE ALL PRIVILEGES ON " + mariadbIdent(name) + ".* FROM " + mariadbString(user) + "@'%';", nil
	}
	return create, "ALTER DATABASE " + postgresIdent(name) + " WITH ALLOW_CONNECTIONS false;", nil
}

// DiscardSQL drops temporary databases, and only those: every name must be
// of the namespace. On MariaDB the privileges of revokeFrom on each, if
// known, go first, as MariaDB keeps them after a drop: each revoke runs on
// its own, and "no such grant" is no failure. PostgreSQL ends their sessions
// with the drop.
func DiscardSQL(engine Engine, names []string, revokeFrom string) (revokes []string, drop string, err error) {
	if len(names) == 0 {
		return nil, "", errors.New("no temporary database to drop")
	}
	for _, name := range names {
		if err := ValidTemporaryName(name); err != nil {
			return nil, "", err
		}
	}
	if revokeFrom != "" {
		if err := validUser(revokeFrom); err != nil {
			return nil, "", err
		}
	}
	lines := []string{}
	for _, name := range names {
		if engine.Name == MariaDB.Name {
			if revokeFrom != "" {
				revokes = append(revokes, "REVOKE ALL PRIVILEGES ON "+mariadbIdent(name)+".* FROM "+mariadbString(revokeFrom)+"@'%';")
			}
			lines = append(lines, "DROP DATABASE IF EXISTS "+mariadbIdent(name)+";")
		} else {
			lines = append(lines, "DROP DATABASE IF EXISTS "+postgresIdent(name)+" WITH (FORCE);")
		}
	}
	return revokes, strings.Join(lines, "\n"), nil
}

// SettingsSQL lists PostgreSQL's settings of a database (ALTER DATABASE …
// SET, and those of a role in it), one name=value per line, which a restore
// does not carry: a preview names them. MariaDB has none.
func SettingsSQL(engine Engine, database string) string {
	if engine.Name == MariaDB.Name {
		return ""
	}
	return "SELECT unnest(s.setconfig) FROM pg_db_role_setting s JOIN pg_database d ON d.oid = s.setdatabase WHERE d.datname = " +
		postgresString(database) + ";"
}

// SessionsSQL lists where the sessions of a database come from, one address
// per line, but the statement's own: an in-place restore's preview counts
// those from outside the containers it stops. A session on the server's
// socket says local (PostgreSQL) or localhost (MariaDB).
func SessionsSQL(engine Engine, database string) string {
	if engine.Name == MariaDB.Name {
		return "SELECT SUBSTRING_INDEX(host, ':', 1) FROM information_schema.processlist WHERE db = " + mariadbString(database) +
			" AND id <> CONNECTION_ID();"
	}
	return "SELECT COALESCE(host(client_addr), 'local') FROM pg_stat_activity WHERE datname = " + postgresString(database) +
		" AND pid <> pg_backend_pid();"
}

// EndSessions ends the sessions of a database but its own, and prints how
// many it ended. PostgreSQL terminates them in one statement. MariaDB has
// none that ends the sessions of a database, so the script lists their ids
// and kills each, leaving out those that ended meanwhile.
func EndSessions(engine Engine, adminUser, adminPassword, name string) Script {
	if engine.Name == MariaDB.Name {
		script := selectClient + `ids=$(printf '%s\n' "$PC_SQL" | "$client" --user="$PC_ADMIN_USER" --batch --skip-column-names) || exit 1
n=0
for id in $ids; do
  case "$id" in ''|*[!0-9]*) continue ;; esac
  "$client" --user="$PC_ADMIN_USER" --batch -e "KILL CONNECTION $id" 2>/dev/null && n=$((n + 1))
done
echo "$n"`
		return Script{
			Cmd: []string{"sh", "-c", script},
			Env: append(credentials(engine, adminUser, adminPassword),
				"PC_SQL=SELECT id FROM information_schema.processlist WHERE db = "+mariadbString(name)+" AND id <> CONNECTION_ID();"),
		}
	}
	return Statements(engine, adminUser, adminPassword,
		"SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE datname = "+postgresString(name)+" AND pid <> pg_backend_pid();")
}

// CountBaseTables prints how many tables a database has, as the
// administrator: base tables only, as a dump's count has them, where
// CountTables, which moves compare, counts views too. MariaDB's
// system-versioned tables are tables too.
func CountBaseTables(engine Engine, adminUser, adminPassword, database string) Script {
	var script string
	if engine.Name == MariaDB.Name {
		script = selectClient + `"$client" --user="$PC_ADMIN_USER" --batch --skip-column-names -e "SELECT COUNT(*) FROM information_schema.tables ` +
			`WHERE table_schema = DATABASE() AND table_type IN ('BASE TABLE', 'SYSTEM VERSIONED')" "$PC_DATABASE"`
	} else {
		script = `psql -X -q -A -t --username="$PC_ADMIN_USER" --dbname="$PC_DATABASE" -c "SELECT count(*) FROM information_schema.tables ` +
			`WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema')"`
	}
	return Script{
		Cmd: []string{"sh", "-c", script},
		Env: append(credentials(engine, adminUser, adminPassword), "PC_DATABASE="+database),
	}
}

// ParseCount reads the number a count prints, on its last line.
func ParseCount(output string) (int64, error) {
	lines := ParseNames(output)
	if len(lines) == 0 {
		return 0, fmt.Errorf("no count in %q", output)
	}
	count, err := strconv.ParseInt(lines[len(lines)-1], 10, 64)
	if err != nil || count < 0 {
		return 0, fmt.Errorf("no count in %q", output)
	}
	return count, nil
}

// StagedDir is where a restore stages a dump in the server's container.
const StagedDir = "/tmp"

// stagedPrefix starts the name of a staged dump.
const stagedPrefix = "pocketcloud-restore-"

var stagedPattern = regexp.MustCompile(`^/tmp/pocketcloud-restore-[A-Za-z0-9_-]{1,64}\.(pgdump|sql)$`)

// StagedName is the name of the file a restore or a restore test stages its
// dump as in StagedDir, by its command's id.
func StagedName(engine Engine, commandID string) string {
	return BackupFile(engine, stagedPrefix+commandID)
}

// CountDump prints how many tables a staged dump holds, and fails when it is
// not whole. PostgreSQL's list has a TABLE entry for each, besides its data
// and a partition's attach; pg_restore that cannot read the list fails it. A
// MariaDB dump that does not end with "-- Dump completed" was cut short; it
// has a CREATE TABLE line for each table, outside the DELIMITER ;; …
// DELIMITER ; blocks it writes its triggers, routines and events in, whose
// bodies it writes as they are, and which may hold CREATE TABLE lines that
// make no table on restore.
func CountDump(engine Engine, file string) Script {
	script := `list=$(pg_restore --list "$PC_FILE") || { echo "pg_restore cannot read the dump's list" >&2; exit 1; }
printf '%s\n' "$list" | awk '$4 == "TABLE" && $5 != "DATA" && $5 != "ATTACH" {n++} END {print n+0}'`
	if engine.Name == MariaDB.Name {
		script = `tail -c 4096 "$PC_FILE" | grep -q '^-- Dump completed' || { echo "the dump is incomplete: it does not end as a whole dump does" >&2; exit 1; }
sed '/^DELIMITER ;;/,/^DELIMITER ;$/d' "$PC_FILE" | grep -c '^CREATE TABLE ' || true`
	}
	return Script{Cmd: []string{"sh", "-c", script}, Env: []string{"PC_FILE=" + file}}
}

// containerAddress is the container's own address, where the server checks
// a user's password, unlike on its local socket: SignIn's.
const containerAddress = `addr="$(hostname -i | cut -d' ' -f1)"
[ -n "$addr" ] || { echo "the container has no address to sign in on" >&2; exit 1; }
`

// The restores of a staged dump as the database's own user, whose
// privileges reach that database only: a dump that names another database
// fails on them. PostgreSQL's pg_restore runs in one transaction, stops at
// the first error, and gives the user what it creates. MariaDB's client
// ignores what a dump runs in another database (--one-database), and runs no
// command of its own (--sandbox) where the client knows the option. Into a
// new database, MariaDB's definers become the new user, whom the dump does
// not know, but for the rows of INSERT lines.
const (
	postgresRestore = containerAddress + `exec pg_restore --no-password --host="$addr" --username="$PC_USER" --dbname="$PC_DATABASE" ` +
		`--single-transaction --exit-on-error --no-owner --no-privileges --no-tablespaces --no-publications --no-subscriptions ` +
		`--no-security-labels "$PC_FILE"`
	mariadbSandbox = `sb=; "$client" --help 2>/dev/null | grep -q -- '--sandbox' && sb=--sandbox
`
	mariadbRestoreClient = `"$client" $sb --host="$addr" --user="$PC_USER" --one-database --connect-timeout=10 "$PC_DATABASE"`
	mariadbRestore       = selectClient + mariadbSandbox + containerAddress + `exec ` + mariadbRestoreClient + ` < "$PC_FILE"`
	mariadbRestoreAs     = selectClient + mariadbSandbox + containerAddress +
		"sed -E '/^INSERT INTO /!s/DEFINER=`[^`]+`@`[^`]+`/DEFINER=`'\"$PC_DEFINER\"'`@`%`/g' \"$PC_FILE\" | " + mariadbRestoreClient
)

// RestoreAsUser restores a staged dump into a database as its own user,
// with that user's password, over the container's address. definer is, for
// MariaDB into a new database, the new user, who becomes the definer of the
// dump's views, routines, triggers and events; PostgreSQL leaves it out. The
// file stays: MariaDB's rehearsal reads it twice, and RemoveStaged removes
// it.
func RestoreAsUser(engine Engine, user, password, database, file, definer string) (Script, error) {
	if err := validUser(user); err != nil {
		return Script{}, err
	}
	if !databaseNamePattern.MatchString(database) {
		return Script{}, fmt.Errorf("%q is not a valid database name", database)
	}
	if engine.Name != MariaDB.Name {
		return Script{
			Cmd: []string{"sh", "-c", postgresRestore},
			Env: []string{"PC_USER=" + user, "PGPASSWORD=" + password, "PC_DATABASE=" + database, "PC_FILE=" + file, "PGCONNECT_TIMEOUT=10"},
		}, nil
	}
	env := []string{"PC_USER=" + user, "MYSQL_PWD=" + password, "PC_DATABASE=" + database, "PC_FILE=" + file}
	if definer == "" {
		return Script{Cmd: []string{"sh", "-c", mariadbRestore}, Env: env}, nil
	}
	// the definer goes into sed's replacement: a plain name only
	if err := validUser(definer); err != nil {
		return Script{}, err
	}
	return Script{Cmd: []string{"sh", "-c", mariadbRestoreAs}, Env: append(env, "PC_DEFINER="+definer)}, nil
}

// RemoveStaged removes a staged dump, whatever happened: it runs deferred.
// Only a file a restore stages is removed.
func RemoveStaged(file string) (Script, error) {
	if !stagedPattern.MatchString(file) {
		return Script{}, fmt.Errorf("%q is not a dump a restore staged", file)
	}
	return Script{Cmd: []string{"sh", "-c", `rm -f "$PC_FILE"`}, Env: []string{"PC_FILE=" + file}}, nil
}

// SweepStaged removes the dumps of restores that ended without removing
// theirs, such as when the agent restarted: those staged more than a day
// ago. Every restore and restore test runs it first.
func SweepStaged() Script {
	return Script{Cmd: []string{"sh", "-c", `find /tmp -maxdepth 1 -type f -name 'pocketcloud-restore-*' -mmin +1440 -exec rm -f {} +`}}
}

// FreeSpace prints the room left in the server's data directory
// (Engine.DataDirOf of its image) and in its container's /tmp, where a dump
// is staged, as df's POSIX lines in KiB.
func FreeSpace(dataDir string) Script {
	return Script{Cmd: []string{"sh", "-c", `exec df -Pk -- "$PC_DATA_DIR" /tmp`}, Env: []string{"PC_DATA_DIR=" + dataDir}}
}

// ParseFreeSpace reads the output of FreeSpace: the bytes available in the
// data directory and in /tmp.
func ParseFreeSpace(output string) (data, tmp int64, err error) {
	available := []int64{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		// the capacity ends in %, after the available KiB; a name with
		// spaces only shifts what comes before
		for i := len(fields) - 1; i > 0; i-- {
			if !strings.HasSuffix(fields[i], "%") {
				continue
			}
			if kib, err := strconv.ParseInt(fields[i-1], 10, 64); err == nil && kib >= 0 {
				available = append(available, kib*1024)
			}
			break
		}
	}
	if len(available) != 2 {
		return 0, 0, fmt.Errorf("df did not say the room in the data directory and /tmp: %q", output)
	}
	return available[0], available[1], nil
}

// Sandbox prints yes when the server's MariaDB client runs without commands
// of its own (--sandbox), which a restore then uses, and no otherwise.
func Sandbox() Script {
	return Script{Cmd: []string{"sh", "-c", selectClient + `if "$client" --help 2>/dev/null | grep -q -- '--sandbox'; then echo yes; else echo no; fi`}}
}
