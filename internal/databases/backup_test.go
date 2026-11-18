package databases

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// The scripts' pieces, as the tests expect them, written out once more.
const (
	wantClient  = "if command -v mariadb >/dev/null 2>&1; then client=mariadb; dump=mariadb-dump; else client=mysql; dump=mysqldump; fi\n"
	wantAddress = `addr="$(hostname -i | cut -d' ' -f1)"` + "\n" +
		`[ -n "$addr" ] || { echo "the container has no address to sign in on" >&2; exit 1; }` + "\n"
	wantSandbox = `sb=; "$client" --help 2>/dev/null | grep -q -- '--sandbox' && sb=--sandbox` + "\n"

	scratch  = "pc_rs_aaaaaaaaaaaaaaa"
	previous = "pc_old_aaaaaaaaaaaaaaa"
	testCopy = "pc_rt_bbbbbbbbbbbbbbb"
)

// TestTheDumpOfOneDatabase pins the dump commands of a database job's run:
// no password, only the database named, never --databases or
// --all-databases, and the administrator and the database as positional
// arguments, never in the environment (AC1, AC3).
func TestTheDumpOfOneDatabase(t *testing.T) {
	for _, tc := range []struct {
		engine Engine
		admin  string
		want   string
	}{
		{Postgres, "postgres", `PGPASSWORD="${POSTGRES_PASSWORD:-}" exec pg_dump --format=custom --compress=0 --lock-wait-timeout=10min --username="$1" --dbname="$2"`},
		{MariaDB, "root", `if command -v mariadb-dump >/dev/null 2>&1; then d=mariadb-dump; else d=mysqldump; fi; ` +
			`MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-$MYSQL_ROOT_PASSWORD}" exec "$d" --user="$1" --single-transaction --routines --triggers ` +
			`--events --hex-blob --skip-extended-insert --no-autocommit --order-by-primary --skip-dump-date --default-character-set=utf8mb4 "$2"`},
	} {
		command, err := DumpCommand(tc.engine, tc.admin, "shop")
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"sh", "-c", tc.want, "sh", tc.admin, "shop"}; !slices.Equal(command, want) {
			t.Errorf("%s: %q\nwant %q", tc.engine.Name, command, want)
		}
		script := command[2]
		for _, never := range []string{"--databases", "--all-databases", "shop", tc.admin, "PC_"} {
			if strings.Contains(script, never) {
				t.Errorf("%s: the script holds %q", tc.engine.Name, never)
			}
		}
		for _, name := range []string{"", "Shop", "1shop", "--all-databases", "shop;drop", "host=evil dbname=shop", "shop db", tc.engine.ReservedNames[0]} {
			if _, err := DumpCommand(tc.engine, tc.admin, name); err == nil {
				t.Errorf("%s: the dump of %q", tc.engine.Name, name)
			}
		}
		for _, admin := range []string{"", "--all-databases", "root;id", "Root"} {
			if _, err := DumpCommand(tc.engine, admin, "shop"); err == nil {
				t.Errorf("%s: the dump as %q", tc.engine.Name, admin)
			}
		}
	}
	if BackupFile(Postgres, "shop") != "shop.pgdump" || BackupFile(MariaDB, "shop") != "shop.sql" {
		t.Error("the dumps' file names")
	}
}

// shims is a directory of fake database clients for running the scripts
// with a real shell: the tools the scripts use are linked in, and nothing
// else is on PATH, so a client installed on the machine is never run.
type shims struct {
	t        *testing.T
	bin, out string
}

func newShims(t *testing.T, fakes map[string]string) shims {
	t.Helper()
	s := shims{t: t, bin: t.TempDir(), out: t.TempDir()}
	for _, tool := range []string{"sed", "grep", "tail", "cut", "awk", "cat", "df"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("the scripts need %s: %v", tool, err)
		}
		if err := os.Symlink(path, filepath.Join(s.bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range fakes {
		if err := os.WriteFile(filepath.Join(s.bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// run runs a command as docker exec would, with its environment and the
// shims', and gives its output and exit code.
func (s shims) run(cmd []string, env ...string) (string, string, int) {
	s.t.Helper()
	shell, err := exec.LookPath(cmd[0])
	if err != nil {
		s.t.Fatal(err)
	}
	command := exec.Command(shell, cmd[1:]...)
	command.Env = append(env, "PATH="+s.bin, "OUT="+s.out)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return stdout.String(), stderr.String(), exit.ExitCode()
	case err != nil:
		s.t.Fatal(err)
	}
	return stdout.String(), stderr.String(), 0
}

func (s shims) output(name string) string {
	s.t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.out, name))
	if err != nil {
		s.t.Fatalf("%s: %v", name, err)
	}
	return string(raw)
}

const printArgs = `for a in "$@"; do printf 'arg:%s\n' "$a"; done
`

// TestTheDumpRunsWithTheContainersPassword runs the dump commands with
// fake dumps: the password comes from the container's environment, the
// names arrive as one argument each, and a name is never run as a command.
func TestTheDumpRunsWithTheContainersPassword(t *testing.T) {
	s := newShims(t, map[string]string{
		"pg_dump":      printArgs + `printf 'PGPASSWORD=%s\n' "$PGPASSWORD"`,
		"mariadb-dump": `echo mariadb-dump; ` + printArgs + `printf 'MYSQL_PWD=%s\n' "$MYSQL_PWD"`,
		"mysqldump":    `echo mysqldump; ` + printArgs + `printf 'MYSQL_PWD=%s\n' "$MYSQL_PWD"`,
	})
	postgres, _ := DumpCommand(Postgres, "postgres", "shop")
	out, errOut, code := s.run(postgres, "POSTGRES_PASSWORD=container-secret")
	if want := "arg:--format=custom\narg:--compress=0\narg:--lock-wait-timeout=10min\narg:--username=postgres\narg:--dbname=shop\nPGPASSWORD=container-secret\n"; code != 0 || out != want {
		t.Errorf("pg_dump: %d %q %q", code, out, errOut)
	}

	maria, _ := DumpCommand(MariaDB, "root", "shop")
	out, _, code = s.run(maria, "MARIADB_ROOT_PASSWORD=maria-secret")
	if !strings.HasPrefix(out, "mariadb-dump\narg:--user=root\narg:--single-transaction\n") || !strings.HasSuffix(out, "arg:--default-character-set=utf8mb4\narg:shop\nMYSQL_PWD=maria-secret\n") || code != 0 {
		t.Errorf("mariadb-dump: %d %q", code, out)
	}
	if strings.Count(out, "arg:") != 12 {
		t.Errorf("mariadb-dump takes %d arguments: %q", strings.Count(out, "arg:"), out)
	}
	// a MySQL image: mysqldump, and MYSQL_ROOT_PASSWORD
	if err := os.Remove(filepath.Join(s.bin, "mariadb-dump")); err != nil {
		t.Fatal(err)
	}
	out, _, _ = s.run(maria, "MYSQL_ROOT_PASSWORD=mysql-secret")
	if !strings.HasPrefix(out, "mysqldump\n") || !strings.HasSuffix(out, "MYSQL_PWD=mysql-secret\n") {
		t.Errorf("mysqldump: %q", out)
	}

	// what the panel checked arrives as it is: a name the checks would refuse
	// is one argument, and runs nothing
	hostile := slices.Clone(postgres)
	marker := filepath.Join(s.out, "ran")
	hostile[len(hostile)-1] = `shop" $(touch ` + marker + `) "x`
	out, _, _ = s.run(hostile)
	if _, err := os.Stat(marker); err == nil || !strings.Contains(out, "arg:--dbname="+hostile[len(hostile)-1]+"\n") {
		t.Errorf("a hostile name: %q", out)
	}
}

// TestTheAdminStatementsOfARestore pins the SQL the administrator runs for
// restores and restore tests.
func TestTheAdminStatementsOfARestore(t *testing.T) {
	latin := Charset{Set: "latin1", Collation: "latin1_swedish_ci"}
	for _, tc := range []struct {
		name      string
		sql       func() (string, error)
		postgres  string
		mariadb   string
		mariadbFn func() (string, error)
	}{
		{
			name: "scratch",
			sql:  func() (string, error) { return ScratchSQL(Postgres, scratch, "shop", latin) },
			postgres: `CREATE DATABASE "pc_rs_aaaaaaaaaaaaaaa" OWNER "shop";` + "\n" +
				`REVOKE ALL ON DATABASE "pc_rs_aaaaaaaaaaaaaaa" FROM PUBLIC;`,
			mariadbFn: func() (string, error) { return ScratchSQL(MariaDB, scratch, "shop", latin) },
			mariadb: "CREATE DATABASE `pc_rs_aaaaaaaaaaaaaaa` CHARACTER SET latin1 COLLATE latin1_swedish_ci;\n" +
				"GRANT ALL PRIVILEGES ON `pc_rs_aaaaaaaaaaaaaaa`.* TO 'shop'@'%';",
		},
		{
			name: "swap",
			sql:  func() (string, error) { return SwapSQL("shop", scratch, previous) },
			postgres: `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname IN ('shop', 'pc_rs_aaaaaaaaaaaaaaa') AND pid <> pg_backend_pid();` + "\n" +
				`SET lock_timeout = '10s';` + "\n" +
				"BEGIN;\n" +
				`ALTER DATABASE "shop" RENAME TO "pc_old_aaaaaaaaaaaaaaa";` + "\n" +
				`ALTER DATABASE "pc_rs_aaaaaaaaaaaaaaa" RENAME TO "shop";` + "\n" +
				"COMMIT;\n" +
				`ALTER DATABASE "pc_old_aaaaaaaaaaaaaaa" WITH ALLOW_CONNECTIONS false;`,
			mariadbFn: func() (string, error) { return RecreateSQL("shop", Charset{}) },
			mariadb:   "DROP DATABASE IF EXISTS `shop`;\nCREATE DATABASE `shop` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;",
		},
	} {
		got, err := tc.sql()
		if err != nil || got != tc.postgres {
			t.Errorf("%s, postgres: %v\n%s\nwant\n%s", tc.name, err, got, tc.postgres)
		}
		got, err = tc.mariadbFn()
		if err != nil || got != tc.mariadb {
			t.Errorf("%s, mariadb: %v\n%s\nwant\n%s", tc.name, err, got, tc.mariadb)
		}
	}

	// a restore test's copy, and how it closes
	create, closing, err := TestDatabaseSQL(Postgres, testCopy, "shop", Charset{})
	if err != nil || create != `CREATE DATABASE "pc_rt_bbbbbbbbbbbbbbb" OWNER "shop";`+"\n"+`REVOKE ALL ON DATABASE "pc_rt_bbbbbbbbbbbbbbb" FROM PUBLIC;` ||
		closing != `ALTER DATABASE "pc_rt_bbbbbbbbbbbbbbb" WITH ALLOW_CONNECTIONS false;` {
		t.Errorf("postgres test: %v\n%s\n%s", err, create, closing)
	}
	create, closing, err = TestDatabaseSQL(MariaDB, testCopy, "shop", Charset{Set: "utf8mb4", Collation: "utf8mb4_uca1400_ai_ci"})
	if err != nil || create != "CREATE DATABASE `pc_rt_bbbbbbbbbbbbbbb` CHARACTER SET utf8mb4 COLLATE utf8mb4_uca1400_ai_ci;\n"+
		"GRANT ALL PRIVILEGES ON `pc_rt_bbbbbbbbbbbbbbb`.* TO 'shop'@'%';" ||
		closing != "REVOKE ALL PRIVILEGES ON `pc_rt_bbbbbbbbbbbbbbb`.* FROM 'shop'@'%';" {
		t.Errorf("mariadb test: %v\n%s\n%s", err, create, closing)
	}

	// the drops of temporary databases
	revokes, drop, err := DiscardSQL(Postgres, []string{testCopy, previous}, "shop")
	if err != nil || revokes != nil ||
		drop != `DROP DATABASE IF EXISTS "pc_rt_bbbbbbbbbbbbbbb" WITH (FORCE);`+"\n"+`DROP DATABASE IF EXISTS "pc_old_aaaaaaaaaaaaaaa" WITH (FORCE);` {
		t.Errorf("postgres discard: %v %q\n%s", err, revokes, drop)
	}
	revokes, drop, err = DiscardSQL(MariaDB, []string{testCopy, scratch}, "shop")
	if err != nil || !slices.Equal(revokes, []string{
		"REVOKE ALL PRIVILEGES ON `pc_rt_bbbbbbbbbbbbbbb`.* FROM 'shop'@'%';",
		"REVOKE ALL PRIVILEGES ON `pc_rs_aaaaaaaaaaaaaaa`.* FROM 'shop'@'%';",
	}) || drop != "DROP DATABASE IF EXISTS `pc_rt_bbbbbbbbbbbbbbb`;\nDROP DATABASE IF EXISTS `pc_rs_aaaaaaaaaaaaaaa`;" {
		t.Errorf("mariadb discard: %v %q\n%s", err, revokes, drop)
	}
	if revokes, _, err := DiscardSQL(MariaDB, []string{testCopy}, ""); err != nil || revokes != nil {
		t.Errorf("a discard of no known user: %v %q", err, revokes)
	}

	if got := CharsetSQL("shop"); got != "SELECT default_character_set_name, default_collation_name FROM information_schema.schemata WHERE schema_name = 'shop';" {
		t.Errorf("charset: %s", got)
	}
	if got := SettingsSQL(Postgres, "shop"); got != "SELECT unnest(s.setconfig) FROM pg_db_role_setting s JOIN pg_database d ON d.oid = s.setdatabase WHERE d.datname = 'shop';" {
		t.Errorf("settings: %s", got)
	}
	if SettingsSQL(MariaDB, "shop") != "" {
		t.Error("MariaDB has settings of a database")
	}
	if got := SessionsSQL(Postgres, "shop"); got != "SELECT COALESCE(host(client_addr), 'local') FROM pg_stat_activity WHERE datname = 'shop' AND pid <> pg_backend_pid();" {
		t.Errorf("sessions: %s", got)
	}
	if got := SessionsSQL(MariaDB, "shop"); got != "SELECT SUBSTRING_INDEX(host, ':', 1) FROM information_schema.processlist WHERE db = 'shop' AND id <> CONNECTION_ID();" {
		t.Errorf("sessions: %s", got)
	}
}

// TestOnlyTemporaryDatabasesAreDiscarded: what drops, renames or creates a
// temporary database takes names of the namespace only, of the right kind,
// and a user's plain name; the in-place restore's database is never one of
// them, nor one the engine keeps.
func TestOnlyTemporaryDatabasesAreDiscarded(t *testing.T) {
	refused := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s passed", what)
		}
	}
	for _, name := range []string{"shop", "pc_rt_short", "PC_RT_AAAAAAAAAAAAAAA", "pc_rt_aaaaaaaaaaaaaaa; DROP DATABASE shop", "pc_rx_aaaaaaaaaaaaaaa",
		"pc_rt_aaaaaaaaaaaaaaaa", "pc_rt_aaaaaaaaaaaaaa`", "pc_old_aaaaaaaaaaaaaa\"", " pc_rt_aaaaaaaaaaaaaaa", "pc_rt_aaaaaaaaaaaaaaa\n"} {
		for _, engine := range []Engine{Postgres, MariaDB} {
			_, _, err := DiscardSQL(engine, []string{testCopy, name}, "shop")
			refused("a discard of "+name, err)
		}
		_, err := ScratchSQL(Postgres, name, "shop", Charset{})
		refused("a scratch copy "+name, err)
	}
	_, _, err := DiscardSQL(Postgres, nil, "")
	refused("a discard of nothing", err)
	_, _, err = DiscardSQL(MariaDB, []string{testCopy}, "shop'@'%'; DROP USER root; --")
	refused("a revoke from a hostile user", err)

	// the kinds
	_, err = ScratchSQL(Postgres, previous, "shop", Charset{})
	refused("a scratch copy named as a previous copy", err)
	_, _, err = TestDatabaseSQL(MariaDB, scratch, "shop", Charset{})
	refused("a test copy named as a scratch copy", err)
	_, err = ScratchSQL(MariaDB, testCopy, "shop'@'%", Charset{})
	refused("a hostile user", err)
	_, err = SwapSQL("shop", testCopy, previous)
	refused("a swap of a test copy", err)
	_, err = SwapSQL("shop", scratch, "pc_old_ccccccccccccccc")
	refused("a swap of two restores' copies", err)
	_, err = SwapSQL("shop", previous, scratch)
	refused("a swap the wrong way round", err)
	_, err = SwapSQL(testCopy, scratch, previous)
	refused("a swap into a temporary database", err)
	_, err = SwapSQL("postgres", scratch, previous)
	refused("a swap of PostgreSQL's own database", err)
	_, err = SwapSQL(`shop"; DROP DATABASE "blog`, scratch, previous)
	refused("a swap of a hostile name", err)
	for _, name := range []string{"shop", scratch, testCopy, `pc_old_aaaaaaaaaaaaaaa"; DROP DATABASE "blog`} {
		_, err = ClosePreviousSQL(name)
		refused("a close of "+name, err)
	}
	if got, err := ClosePreviousSQL(previous); err != nil || got != `ALTER DATABASE "pc_old_aaaaaaaaaaaaaaa" WITH ALLOW_CONNECTIONS false;` {
		t.Errorf("the close of the data before a restore: %v %s", err, got)
	}
	for _, name := range []string{"mysql", "information_schema", scratch, "Shop", "shop`; DROP DATABASE `blog"} {
		_, err = RecreateSQL(name, Charset{})
		refused("a recreation of "+name, err)
	}
}

// TestHostileNamesStayQuoted: what reaches SQL text as a literal or an
// identifier cannot end it, and a character set that is not a plain name
// never reaches the SQL.
func TestHostileNamesStayQuoted(t *testing.T) {
	hostile := `x'); DROP DATABASE shop; -- \`
	if got := EndSessions(Postgres, "postgres", "pw", hostile).Env[2]; got !=
		`PC_SQL=SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE datname = 'x''); DROP DATABASE shop; -- \' AND pid <> pg_backend_pid();` {
		t.Errorf("postgres: %s", got)
	}
	if got := EndSessions(MariaDB, "root", "pw", hostile).Env[2]; got !=
		`PC_SQL=SELECT id FROM information_schema.processlist WHERE db = 'x\'); DROP DATABASE shop; -- \\' AND id <> CONNECTION_ID();` {
		t.Errorf("mariadb: %s", got)
	}
	if got := CharsetSQL(hostile); got != `SELECT default_character_set_name, default_collation_name FROM information_schema.schemata WHERE schema_name = 'x\'); DROP DATABASE shop; -- \\';` {
		t.Errorf("charset: %s", got)
	}
	if got := SettingsSQL(Postgres, hostile); !strings.HasSuffix(got, `d.datname = 'x''); DROP DATABASE shop; -- \';`) {
		t.Errorf("settings: %s", got)
	}
	if got := SessionsSQL(Postgres, hostile); !strings.Contains(got, `datname = 'x''); DROP DATABASE shop; -- \' AND`) {
		t.Errorf("sessions: %s", got)
	}
	if got := SessionsSQL(MariaDB, hostile); !strings.Contains(got, `db = 'x\'); DROP DATABASE shop; -- \\' AND`) {
		t.Errorf("sessions: %s", got)
	}

	for _, charset := range []Charset{
		{Set: "utf8mb4; DROP DATABASE shop", Collation: "utf8mb4_bin"},
		{Set: "utf8mb4", Collation: "utf8mb4_bin COLLATE x"},
		{Set: "", Collation: ""},
		{Set: "UTF8", Collation: "utf8mb4_bin"},
	} {
		sql, err := ScratchSQL(MariaDB, scratch, "shop", charset)
		if err != nil || !strings.Contains(sql, " CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;") || strings.Contains(sql, "DROP") {
			t.Errorf("%+v: %v %s", charset, err, sql)
		}
	}

	if got, err := ParseCharset("latin1\tlatin1_swedish_ci\n"); err != nil || got != (Charset{Set: "latin1", Collation: "latin1_swedish_ci"}) {
		t.Errorf("a character set: %+v, %v", got, err)
	}
	for _, output := range []string{"", "\n", "utf8mb4; DROP\tx\n", "utf8mb4\t\n"} {
		if got, err := ParseCharset(output); err == nil {
			t.Errorf("%q gave %+v", output, got)
		}
	}
}

// TestTheScriptsOfARestore pins the scripts a restore and a restore test
// run in the server's container, and their environments: the user's
// password and the names only there (AC1).
func TestTheScriptsOfARestore(t *testing.T) {
	restoreAs := func(engine Engine, definer string) Script {
		t.Helper()
		script, err := RestoreAsUser(engine, "shop", "user-password", "shop", "/tmp/pocketcloud-restore-c1.x", definer)
		if err != nil {
			t.Fatal(err)
		}
		return script
	}
	removed, err := RemoveStaged("/tmp/pocketcloud-restore-c1.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		script Script
		cmd    string
		env    []string
	}{
		{
			"postgres tables", CountBaseTables(Postgres, "postgres", "admin-password", "shop"),
			`psql -X -q -A -t --username="$PC_ADMIN_USER" --dbname="$PC_DATABASE" -c "SELECT count(*) FROM information_schema.tables WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema')"`,
			[]string{"PC_ADMIN_USER=postgres", "PGPASSWORD=admin-password", "PC_DATABASE=shop"},
		},
		{
			"mariadb tables", CountBaseTables(MariaDB, "root", "admin-password", "shop"),
			wantClient + `"$client" --user="$PC_ADMIN_USER" --batch --skip-column-names -e "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type IN ('BASE TABLE', 'SYSTEM VERSIONED')" "$PC_DATABASE"`,
			[]string{"PC_ADMIN_USER=root", "MYSQL_PWD=admin-password", "PC_DATABASE=shop"},
		},
		{
			"postgres dump's tables", CountDump(Postgres, "/tmp/pocketcloud-restore-c1.pgdump"),
			`list=$(pg_restore --list "$PC_FILE") || { echo "pg_restore cannot read the dump's list" >&2; exit 1; }` + "\n" +
				`printf '%s\n' "$list" | awk '$4 == "TABLE" && $5 != "DATA" && $5 != "ATTACH" {n++} END {print n+0}'`,
			[]string{"PC_FILE=/tmp/pocketcloud-restore-c1.pgdump"},
		},
		{
			"mariadb dump's tables", CountDump(MariaDB, "/tmp/pocketcloud-restore-c1.sql"),
			`tail -c 4096 "$PC_FILE" | grep -q '^-- Dump completed' || { echo "the dump is incomplete: it does not end as a whole dump does" >&2; exit 1; }` + "\n" +
				`sed '/^DELIMITER ;;/,/^DELIMITER ;$/d' "$PC_FILE" | grep -c '^CREATE TABLE ' || true`,
			[]string{"PC_FILE=/tmp/pocketcloud-restore-c1.sql"},
		},
		{
			"postgres restore", restoreAs(Postgres, "ignored"),
			wantAddress + `exec pg_restore --no-password --host="$addr" --username="$PC_USER" --dbname="$PC_DATABASE" --single-transaction --exit-on-error --no-owner --no-privileges --no-tablespaces --no-publications --no-subscriptions --no-security-labels "$PC_FILE"`,
			[]string{"PC_USER=shop", "PGPASSWORD=user-password", "PC_DATABASE=shop", "PC_FILE=/tmp/pocketcloud-restore-c1.x", "PGCONNECT_TIMEOUT=10"},
		},
		{
			"mariadb restore", restoreAs(MariaDB, ""),
			wantClient + wantSandbox + wantAddress + `exec "$client" $sb --host="$addr" --user="$PC_USER" --one-database --connect-timeout=10 "$PC_DATABASE" < "$PC_FILE"`,
			[]string{"PC_USER=shop", "MYSQL_PWD=user-password", "PC_DATABASE=shop", "PC_FILE=/tmp/pocketcloud-restore-c1.x"},
		},
		{
			"mariadb restore into a new database", restoreAs(MariaDB, "shop"),
			wantClient + wantSandbox + wantAddress +
				"sed -E '/^INSERT INTO /!s/DEFINER=`[^`]+`@`[^`]+`/DEFINER=`'\"$PC_DEFINER\"'`@`%`/g' \"$PC_FILE\" | " +
				`"$client" $sb --host="$addr" --user="$PC_USER" --one-database --connect-timeout=10 "$PC_DATABASE"`,
			[]string{"PC_USER=shop", "MYSQL_PWD=user-password", "PC_DATABASE=shop", "PC_FILE=/tmp/pocketcloud-restore-c1.x", "PC_DEFINER=shop"},
		},
		{
			"postgres sessions", EndSessions(Postgres, "postgres", "admin-password", "shop"),
			`printf '%s\n' "$PC_SQL" | psql -X -q -v ON_ERROR_STOP=1 -A -t -F "$(printf '\t')" --username="$PC_ADMIN_USER" --dbname=postgres`,
			[]string{"PC_ADMIN_USER=postgres", "PGPASSWORD=admin-password",
				"PC_SQL=SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE datname = 'shop' AND pid <> pg_backend_pid();"},
		},
		{
			"mariadb sessions", EndSessions(MariaDB, "root", "admin-password", "shop"),
			wantClient + `ids=$(printf '%s\n' "$PC_SQL" | "$client" --user="$PC_ADMIN_USER" --batch --skip-column-names) || exit 1` + "\n" +
				"n=0\n" +
				"for id in $ids; do\n" +
				`  case "$id" in ''|*[!0-9]*) continue ;; esac` + "\n" +
				`  "$client" --user="$PC_ADMIN_USER" --batch -e "KILL CONNECTION $id" 2>/dev/null && n=$((n + 1))` + "\n" +
				"done\n" +
				`echo "$n"`,
			[]string{"PC_ADMIN_USER=root", "MYSQL_PWD=admin-password",
				"PC_SQL=SELECT id FROM information_schema.processlist WHERE db = 'shop' AND id <> CONNECTION_ID();"},
		},
		{"remove", removed, `rm -f "$PC_FILE"`, []string{"PC_FILE=/tmp/pocketcloud-restore-c1.sql"}},
		{"sweep", SweepStaged(), `find /tmp -maxdepth 1 -type f -name 'pocketcloud-restore-*' -mmin +1440 -exec rm -f {} +`, nil},
		{"list", ListStaged(), `exec find /tmp -maxdepth 1 -type f -name 'pocketcloud-restore-*'`, nil},
		{"free space", FreeSpace("/var/lib/postgresql/data"), `exec df -Pk -- "$PC_DATA_DIR" /tmp`, []string{"PC_DATA_DIR=/var/lib/postgresql/data"}},
		{"sandbox", Sandbox(), wantClient + `if "$client" --help 2>/dev/null | grep -q -- '--sandbox'; then echo yes; else echo no; fi`, nil},
	} {
		if want := []string{"sh", "-c", tc.cmd}; !slices.Equal(tc.script.Cmd, want) {
			t.Errorf("%s: %q\nwant %q", tc.name, tc.script.Cmd, want)
		}
		if !slices.Equal(tc.script.Env, tc.env) {
			t.Errorf("%s: the environment is %q, not %q", tc.name, tc.script.Env, tc.env)
		}
		line := strings.Join(tc.script.Cmd, " ")
		for _, never := range []string{"admin-password", "user-password", "shop", "pocketcloud-restore-c1", "--databases", "--all-databases"} {
			if strings.Contains(line, never) {
				t.Errorf("%s: the command line holds %q", tc.name, never)
			}
		}
	}

	for _, bad := range []struct{ user, database, definer string }{
		{"shop'", "shop", ""}, {"shop", "shop db", ""}, {"shop", "shop", "x/`@`%`/;s/^/DROP DATABASE blog;/;#"}, {"", "shop", ""}, {"shop", "", ""},
	} {
		if _, err := RestoreAsUser(MariaDB, bad.user, "pw", bad.database, "/tmp/f.sql", bad.definer); err == nil {
			t.Errorf("a restore as %+v", bad)
		}
	}
	for _, file := range []string{"/var/lib/postgresql/data/base/1", "/tmp/pocketcloud-restore-c1.sql/..", "/tmp/../etc/passwd",
		"/tmp/pocketcloud-restore-.sql", "/tmp/pocketcloud-restore-c1.tar", "/tmp/pocketcloud-restore-c 1.sql", "tmp/pocketcloud-restore-c1.sql", ""} {
		if _, err := RemoveStaged(file); err == nil {
			t.Errorf("%q is removed as a staged dump", file)
		}
	}
	if StagedName(Postgres, "c1") != "pocketcloud-restore-c1.pgdump" || StagedName(MariaDB, "c1") != "pocketcloud-restore-c1.sql" || StagedDir != "/tmp" {
		t.Error("the staged dumps' names")
	}
	if _, err := RemoveStaged(StagedDir + "/" + StagedName(Postgres, "k3m9x2p7q1w8e4r")); err != nil {
		t.Errorf("a staged dump is not removed: %v", err)
	}
}

// TestStagedFilesAreStagedDumpsOnly (#53, D4): of what ListStaged prints,
// the lines that are whole a path a restore or a restore test stages its
// dump as are staged dumps, and nothing else is: another name, another
// folder, a line cut from a name with a newline in it.
func TestStagedFilesAreStagedDumpsOnly(t *testing.T) {
	output := strings.Join([]string{
		"/tmp/pocketcloud-restore-k3m9x2p7q1w8e4r.pgdump",
		"/tmp/pocketcloud-restore-c-1_a.sql",
		"/tmp/pocketcloud-restore-notes.txt",
		"/tmp/pocketcloud-restore-.sql",
		"/tmp/pocketcloud-restore-c1.sql.part",
		"/tmp/pocketcloud-restore-c 1.sql",
		"/tmp/pocketcloud-restore-" + strings.Repeat("a", 65) + ".sql",
		"/tmp/pocketcloud-move-c1-0.sql",
		"/tmp/sub/pocketcloud-restore-c1.sql",
		"/var/tmp/pocketcloud-restore-c1.sql",
		" /tmp/pocketcloud-restore-c2.sql",
		"/tmp/pocketcloud-restore-c3.sql ",
		"",
	}, "\n")
	want := []string{"/tmp/pocketcloud-restore-k3m9x2p7q1w8e4r.pgdump", "/tmp/pocketcloud-restore-c-1_a.sql"}
	if files := StagedFiles(output); !slices.Equal(files, want) {
		t.Errorf("the staged dumps of %q: %q, want %q", output, files, want)
	}
	if files := StagedFiles(""); files == nil || len(files) != 0 {
		t.Errorf("the staged dumps of nothing: %#v", files)
	}
	for _, file := range StagedFiles(output) {
		if _, err := RemoveStaged(file); err != nil {
			t.Errorf("%s is not removed: %v", file, err)
		}
	}
}

// TestTheRestoreRunsAsTheUserOverTheAddress runs the restores with fake
// clients: they sign in as the user over the container's address, the
// MariaDB client takes --sandbox where it knows it and --one-database, and
// into a new database the definers become the new user, but the rows of
// INSERT lines stay as they are (AC1).
func TestTheRestoreRunsAsTheUserOverTheAddress(t *testing.T) {
	client := `if [ "$1" = --help ]; then [ -n "$FAKE_SANDBOX" ] && echo "  --sandbox  Disallow commands that access the file system"; echo "  --one-database"; exit 0; fi
echo "$0" > "$OUT/client"
for a in "$@"; do printf '%s\n' "$a"; done > "$OUT/args"
printf '%s\n' "$MYSQL_PWD" > "$OUT/password"
cat > "$OUT/stdin"
`
	s := newShims(t, map[string]string{
		"hostname":   `[ "$1" = -i ] && echo "${FAKE_ADDRESS-10.1.2.3 172.17.0.1}"`,
		"mariadb":    client,
		"mysql":      client,
		"pg_restore": `for a in "$@"; do printf '%s\n' "$a"; done > "$OUT/args"; printf '%s %s\n' "$PGPASSWORD" "$PGCONNECT_TIMEOUT" > "$OUT/env"`,
	})
	dump := filepath.Join(s.out, "pocketcloud-restore-c1.sql")
	content := "/*!50003 CREATE*/ /*!50017 DEFINER=`root`@`localhost`*/ /*!50003 TRIGGER `t_ai` AFTER INSERT ON `t` FOR EACH ROW SET @x = 1 */;;\n" +
		"INSERT INTO `t` VALUES (1,'DEFINER=`root`@`localhost`');\n" +
		"CREATE DEFINER=`old`@`%` PROCEDURE `p`()\n" +
		"/*!50001 CREATE ALGORITHM=UNDEFINED */ /*!50013 DEFINER=`app`@`10.0.%` SQL SECURITY DEFINER */ /*!50001 VIEW `v` AS select 1 */;\n"
	if err := os.WriteFile(dump, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	asNew, _ := RestoreAsUser(MariaDB, "shop_new", "user-password", "shop_new", dump, "shop_new")
	if _, errOut, code := s.run(asNew.Cmd, append(asNew.Env, "FAKE_SANDBOX=1")...); code != 0 {
		t.Fatalf("the restore failed: %d %s", code, errOut)
	}
	want := "/*!50003 CREATE*/ /*!50017 DEFINER=`shop_new`@`%`*/ /*!50003 TRIGGER `t_ai` AFTER INSERT ON `t` FOR EACH ROW SET @x = 1 */;;\n" +
		"INSERT INTO `t` VALUES (1,'DEFINER=`root`@`localhost`');\n" +
		"CREATE DEFINER=`shop_new`@`%` PROCEDURE `p`()\n" +
		"/*!50001 CREATE ALGORITHM=UNDEFINED */ /*!50013 DEFINER=`shop_new`@`%` SQL SECURITY DEFINER */ /*!50001 VIEW `v` AS select 1 */;\n"
	if got := s.output("stdin"); got != want {
		t.Errorf("the dump the client read:\n%s\nwant\n%s", got, want)
	}
	if got := s.output("args"); got != "--sandbox\n--host=10.1.2.3\n--user=shop_new\n--one-database\n--connect-timeout=10\nshop_new\n" {
		t.Errorf("the client's arguments: %q", got)
	}
	if s.output("password") != "user-password\n" || !strings.HasSuffix(strings.TrimSpace(s.output("client")), "/mariadb") {
		t.Errorf("the client: %q %q", s.output("client"), s.output("password"))
	}

	// in place, the dump as it is; a client without --sandbox; a MySQL image
	if err := os.Remove(filepath.Join(s.bin, "mariadb")); err != nil {
		t.Fatal(err)
	}
	inPlace, _ := RestoreAsUser(MariaDB, "shop", "user-password", "shop", dump, "")
	if _, errOut, code := s.run(inPlace.Cmd, inPlace.Env...); code != 0 {
		t.Fatalf("the restore failed: %d %s", code, errOut)
	}
	if s.output("stdin") != content || s.output("args") != "--host=10.1.2.3\n--user=shop\n--one-database\n--connect-timeout=10\nshop\n" ||
		!strings.HasSuffix(strings.TrimSpace(s.output("client")), "/mysql") {
		t.Errorf("in place: %q %q %q", s.output("client"), s.output("args"), s.output("stdin"))
	}

	pg, _ := RestoreAsUser(Postgres, "shop", "user-password", "pc_rt_bbbbbbbbbbbbbbb", "/tmp/pocketcloud-restore-c1.pgdump", "")
	if _, errOut, code := s.run(pg.Cmd, pg.Env...); code != 0 {
		t.Fatalf("pg_restore failed: %d %s", code, errOut)
	}
	if got := s.output("args"); got != "--no-password\n--host=10.1.2.3\n--username=shop\n--dbname=pc_rt_bbbbbbbbbbbbbbb\n--single-transaction\n"+
		"--exit-on-error\n--no-owner\n--no-privileges\n--no-tablespaces\n--no-publications\n--no-subscriptions\n--no-security-labels\n"+
		"/tmp/pocketcloud-restore-c1.pgdump\n" || s.output("env") != "user-password 10\n" {
		t.Errorf("pg_restore: %q %q", got, s.output("env"))
	}

	// without an address the server would not check the password: nothing runs
	if err := os.Remove(filepath.Join(s.out, "args")); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := s.run(pg.Cmd, append(pg.Env, "FAKE_ADDRESS=")...); code == 0 || !strings.Contains(errOut, "no address") {
		t.Errorf("without an address: %d %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(s.out, "args")); err == nil {
		t.Error("pg_restore ran without an address")
	}
}

// TestTheDumpsTablesAreCounted runs CountDump: a PostgreSQL list's tables,
// without their data and partitions' attach entries; a MariaDB dump's
// CREATE TABLE lines; and the refusal of a dump that is not whole.
func TestTheDumpsTablesAreCounted(t *testing.T) {
	s := newShims(t, map[string]string{
		"pg_restore": `[ "$1" = --list ] || exit 9; [ -n "$FAKE_BROKEN" ] && { echo "pg_restore: error: input file does not appear to be a valid archive" >&2; exit 1; }; cat "$FAKE_TOC"`,
	})
	toc := filepath.Join(s.out, "toc")
	if err := os.WriteFile(toc, []byte(`;
; Archive created at 2026-11-18 01:30:00 UTC
;     dbname: shop
;
215; 1259 16386 TABLE public orders shop
216; 1259 16390 TABLE public orders_2026 shop
217; 1259 16394 TABLE public customers shop
3301; 0 0 TABLE ATTACH public orders_2026 shop
218; 1259 16398 VIEW public recent shop
219; 1259 16384 SEQUENCE public orders_id_seq shop
3340; 0 16386 TABLE DATA public orders shop
3341; 0 16394 TABLE DATA public customers shop
220; 1259 16400 MATERIALIZED VIEW public stats shop
`), 0o600); err != nil {
		t.Fatal(err)
	}
	pg := CountDump(Postgres, "/tmp/pocketcloud-restore-c1.pgdump")
	out, errOut, code := s.run(pg.Cmd, append(pg.Env, "FAKE_TOC="+toc)...)
	if count, err := ParseCount(out); code != 0 || err != nil || count != 3 {
		t.Errorf("a PostgreSQL dump: %d %q %q", code, out, errOut)
	}
	out, errOut, code = s.run(pg.Cmd, append(pg.Env, "FAKE_TOC="+toc, "FAKE_BROKEN=1")...)
	if code == 0 || !strings.Contains(errOut, "cannot read the dump's list") {
		t.Errorf("a damaged PostgreSQL dump: %d %q %q", code, out, errOut)
	}

	whole := "-- MariaDB dump 10.19\n" +
		"CREATE TABLE `orders` (\n  `id` int NOT NULL\n);\n" +
		"/*!50001 CREATE VIEW `recent` AS SELECT 1 AS `id`*/;\n" +
		"INSERT INTO `orders` VALUES (1);\n" +
		"CREATE TABLE `customers` (\n  `id` int NOT NULL\n);\n" +
		"-- Dump completed\n"
	// a trigger, an event and a procedure, as mariadb-dump writes them:
	// their bodies as they are, between DELIMITER lines (an event's ;;; when
	// its body holds ;;)
	routines := strings.Replace(whole, "-- Dump completed\n", "DELIMITER ;;\n"+
		"/*!50003 CREATE*/ /*!50017 DEFINER=`shop`@`%`*/ /*!50003 TRIGGER `orders_touch` BEFORE INSERT ON `orders` FOR EACH ROW SET NEW.id = NEW.id */;;\n"+
		"DELIMITER ;\n"+
		"DELIMITER ;;;\n"+
		"/*!50106 CREATE*/ /*!50117 DEFINER=`shop`@`%`*/ /*!50106 EVENT `rollup` ON SCHEDULE EVERY 1 DAY STARTS '2026-11-18 00:00:00' ON COMPLETION NOT PRESERVE ENABLE DO BEGIN\n"+
		"CREATE TABLE IF NOT EXISTS `rollup` (`id` int);\n"+
		"SELECT ';;';\n"+
		"END */ ;;;\n"+
		"DELIMITER ;\n"+
		"/*!50003 DROP PROCEDURE IF EXISTS `archive` */;\n"+
		"DELIMITER ;;\n"+
		"CREATE DEFINER=`shop`@`%` PROCEDURE `archive`()\nBEGIN\n"+
		"CREATE TABLE IF NOT EXISTS `orders_old` LIKE `orders`;\n"+
		"CREATE TABLE `orders_copy` (`id` int);\n"+
		"END ;;\n"+
		"DELIMITER ;\n"+
		"-- Dump completed\n", 1)
	for name, tc := range map[string]struct {
		content string
		ok      bool
		count   int64
	}{
		"whole":           {whole, true, 2},
		"with routines":   {routines, true, 2},
		"cut short":       {strings.TrimSuffix(whole, "-- Dump completed\n"), false, 0},
		"without a table": {"-- MariaDB dump 10.19\n-- Dump completed\n", true, 0},
		"dated":           {strings.Replace(whole, "-- Dump completed\n", "-- Dump completed on 2026-11-18  1:30:00\n", 1), true, 2},
	} {
		file := filepath.Join(s.out, "dump.sql")
		if err := os.WriteFile(file, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		maria := CountDump(MariaDB, file)
		out, errOut, code := s.run(maria.Cmd, maria.Env...)
		count, err := ParseCount(out)
		switch {
		case tc.ok && (code != 0 || err != nil || count != tc.count):
			t.Errorf("%s: %d %q %q", name, code, out, errOut)
		case !tc.ok && (code == 0 || !strings.Contains(errOut, "the dump is incomplete")):
			t.Errorf("%s passed: %d %q %q", name, code, out, errOut)
		}
	}
}

// TestTheSessionsOfADatabaseEnd runs MariaDB's EndSessions: it kills each
// session the server lists, counts those it ended, and fails when the list
// does.
func TestTheSessionsOfADatabaseEnd(t *testing.T) {
	s := newShims(t, map[string]string{
		"mariadb": `case "$*" in
*"KILL CONNECTION 13"*) exit 1 ;;
*"KILL CONNECTION"*) echo "$*" >> "$OUT/kills"; exit 0 ;;
esac
cat > "$OUT/sql"; [ -n "$FAKE_FAIL" ] && exit 1
printf '12\n13\nabc;id\n14\n'`,
	})
	end := EndSessions(MariaDB, "root", "admin-password", "pc_rs_aaaaaaaaaaaaaaa")
	out, errOut, code := s.run(end.Cmd, end.Env...)
	if count, err := ParseCount(out); code != 0 || err != nil || count != 2 {
		t.Errorf("the sessions: %d %q %q", code, out, errOut)
	}
	if got := s.output("kills"); got != "--user=root --batch -e KILL CONNECTION 12\n--user=root --batch -e KILL CONNECTION 14\n" {
		t.Errorf("the kills: %q", got)
	}
	if got := s.output("sql"); got != "SELECT id FROM information_schema.processlist WHERE db = 'pc_rs_aaaaaaaaaaaaaaa' AND id <> CONNECTION_ID();\n" {
		t.Errorf("the list: %q", got)
	}
	if _, _, code := s.run(end.Cmd, append(end.Env, "FAKE_FAIL=1")...); code == 0 {
		t.Error("a list that failed ended no session, and passed")
	}
}

// TestTheRoomOnTheServer reads df's lines, as FreeSpace prints them.
func TestTheRoomOnTheServer(t *testing.T) {
	output := "Filesystem     1024-blocks      Used Available Capacity Mounted on\n" +
		"/dev/sda1        102687672  51234567  46200000      53% /var/lib/postgresql/data\n" +
		"my tmp fs           524288       100    524188       1% /tmp\n"
	data, tmp, err := ParseFreeSpace(output)
	if err != nil || data != 46200000*1024 || tmp != 524188*1024 {
		t.Errorf("%d %d %v", data, tmp, err)
	}
	for _, bad := range []string{"", "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 1 1 x 5% /\n/dev/sda2 1 1 1 5% /tmp\n",
		"Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 10 5 5 50% /\n"} {
		if data, tmp, err := ParseFreeSpace(bad); err == nil {
			t.Errorf("%q gave %d %d", bad, data, tmp)
		}
	}
	// and as a real df prints them
	s := newShims(t, nil)
	space := FreeSpace(t.TempDir())
	out, errOut, code := s.run(space.Cmd, space.Env...)
	if data, tmp, err := ParseFreeSpace(out); code != 0 || err != nil || data <= 0 || tmp <= 0 {
		t.Errorf("df: %d %q %q: %d %d %v", code, out, errOut, data, tmp, err)
	}

	for output, want := range map[string]int64{"3\n": 3, " 12 \n": 12, "t\nt\n0\n": 0, "1\n2\n": 2} {
		if got, err := ParseCount(output); err != nil || got != want {
			t.Errorf("ParseCount(%q) = %d, %v", output, got, err)
		}
	}
	for _, output := range []string{"", "\n", "x\n", "-1\n", "3 tables\n"} {
		if got, err := ParseCount(output); err == nil {
			t.Errorf("ParseCount(%q) = %d", output, got)
		}
	}
}

// TestTheTemporaryNamespace: a temporary database's name is the prefix of
// its kind and its record's id, and nothing else; a new database may not
// start with a prefix of the namespace.
func TestTheTemporaryNamespace(t *testing.T) {
	if schema.TemporaryDatabase.String() != `^pc_(rt|rs|old)_[a-z0-9]{15}$` {
		t.Errorf("the namespace is %s", schema.TemporaryDatabase)
	}
	for _, name := range []string{testCopy, scratch, previous, "pc_rt_012345678901234", TestDatabaseName("k3m9x2p7q1w8e4r")} {
		if err := ValidTemporaryName(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if err := ValidDatabaseName(Postgres, name); err != nil {
			t.Errorf("%s is not a database's name: %v", name, err)
		}
	}
	for _, name := range []string{"", "shop", "pc_rt_", "pc_rt_aaaaaaaaaaaaaa", "pc_rt_aaaaaaaaaaaaaaaa", "pc_rt_AAAAAAAAAAAAAAA", "pc_xx_aaaaaaaaaaaaaaa",
		"pc_rt_aaaaaaaaaaaaaa-", "xpc_rt_aaaaaaaaaaaaaaa", "pc_rt_aaaaaaaaaaaaaaa\n", "pc_old_aaaaaaaaaaaaaa"} {
		if ValidTemporaryName(name) == nil {
			t.Errorf("%q passed as a temporary name", name)
		}
	}
	if ScratchName("r1") != "pc_rs_r1" || PreviousName("r1") != "pc_old_r1" || TestDatabaseName("c1") != "pc_rt_c1" {
		t.Error("the temporary names")
	}
	for name, want := range map[string][2]string{testCopy: {"pc_rt_", "bbbbbbbbbbbbbbb"}, scratch: {"pc_rs_", "aaaaaaaaaaaaaaa"}, previous: {"pc_old_", "aaaaaaaaaaaaaaa"}} {
		if prefix, id, ok := TemporaryOwner(name); !ok || prefix != want[0] || id != want[1] {
			t.Errorf("TemporaryOwner(%s) = %s %s %v", name, prefix, id, ok)
		}
	}
	if ReservedPrefixProblem != "Names that start with pc_rt_, pc_rs_ or pc_old_ are PocketCloud's own temporary databases." {
		t.Errorf("the answer is %q", ReservedPrefixProblem)
	}
	if _, _, ok := TemporaryOwner("pc_rt_short"); ok {
		t.Error("a name outside the namespace has an owner")
	}

	for _, name := range []string{"pc_rt_shop", "pc_rs_x", "pc_old_", testCopy} {
		if err := ReservedPrefix(name); err == nil || err.Error() != "names that start with pc_rt_, pc_rs_ or pc_old_ are PocketCloud's own temporary databases" {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, name := range []string{"shop", "pc_shop", "pc_r_x", "pcrt_x", "apc_rt_x", "pc_oldx"} {
		if err := ReservedPrefix(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	for engine, files := range map[string]map[string]string{
		"postgres": {"shop.pgdump": "shop", "shop_staging.pgdump": "shop_staging", "shop.sql": "", "Shop.pgdump": "", ".pgdump": "", "a/b.pgdump": "", "shop.pgdump.tmp": ""},
		"mariadb":  {"shop.sql": "shop", "shop.pgdump": "", "shop": "", "x y.sql": ""},
	} {
		e, _ := EngineOf(engine)
		for file, want := range files {
			got, err := FileDatabase(e, file)
			if got != want || (err == nil) != (want != "") {
				t.Errorf("FileDatabase(%s, %q) = %q, %v", engine, file, got, err)
			}
		}
	}
}
