package fakedb

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
)

// The fake runs the scripts of internal/databases as the agent runs them:
// through the Engine API of a fake Engine.

type harness struct {
	t      *testing.T
	engine *fakedocker.Engine
	client *dockerapi.Client
	db     *Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	engine := fakedocker.New(t)
	client, err := dockerapi.New(engine.Host())
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, engine: engine, client: client, db: Install(engine)}
}

// run runs a script in a container, and says what it printed and its exit
// code.
func (h *harness) run(container string, script databases.Script) (string, string, int) {
	h.t.Helper()
	var stdout bytes.Buffer
	result, err := h.client.Run(context.Background(), container, dockerapi.ExecOptions{Cmd: script.Cmd, Env: script.Env}, &stdout)
	if err != nil {
		h.t.Fatal(err)
	}
	return stdout.String(), result.Stderr, result.ExitCode
}

// must runs a script, which must succeed, and says what it printed.
func (h *harness) must(container string, script databases.Script) string {
	h.t.Helper()
	stdout, stderr, code := h.run(container, script)
	if code != 0 {
		h.t.Fatalf("%q: exit %d: %s", script.Cmd, code, stderr)
	}
	return stdout
}

// sql runs statements as the administrator.
func (h *harness) sql(container string, engine databases.Engine, sql string) (string, string, int) {
	h.t.Helper()
	return h.run(container, databases.Statements(engine, engine.AdminUser, "admin-pw", sql))
}

// stage copies a dump into the container, as a restore stages it.
func (h *harness) stage(container, name, content string) string {
	h.t.Helper()
	if err := h.client.CopyFileFrom(context.Background(), container, databases.StagedDir, name, strings.NewReader(content), int64(len(content)), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return databases.StagedDir + "/" + name
}

func engines() []databases.Engine { return []databases.Engine{databases.Postgres, databases.MariaDB} }

// TestStatementsMakeRenameAndDropDatabases: what the panel's statements
// create, rename, close and drop is there, and errors are the client's: a
// name that exists, a PostgreSQL role that still owns a database, a revoke
// of no grant; PostgreSQL rolls back an open transaction that failed.
func TestStatementsMakeRenameAndDropDatabases(t *testing.T) {
	h := newHarness(t)
	for _, engine := range engines() {
		c := h.db.AddServer("pcdb-"+engine.Name, engine.Name)
		if _, stderr, code := h.sql(c, engine, databases.CreateSQL(engine, "shop", "shop", "shop-pw")); code != 0 {
			t.Fatalf("%s: create: %s", engine.Name, stderr)
		}
		if password, ok := h.db.Password(c, "shop"); !ok || password != "shop-pw" {
			t.Errorf("%s: the user: %q %v", engine.Name, password, ok)
		}
		out, _, _ := h.sql(c, engine, databases.ExistsSQL(engine, "shop", "shop"))
		if database, user := databases.ParseExists(out); !database || !user {
			t.Errorf("%s: exists: %q", engine.Name, out)
		}
		scratch, err := databases.ScratchSQL(engine, "pc_rs_aaaaaaaaaaaaaaa", "shop", databases.DefaultCharset)
		if err != nil {
			t.Fatal(err)
		}
		if _, stderr, code := h.sql(c, engine, scratch); code != 0 {
			t.Fatalf("%s: scratch: %s", engine.Name, stderr)
		}
		if _, stderr, code := h.sql(c, engine, scratch); code == 0 || !strings.Contains(stderr, "exists") {
			t.Errorf("%s: a scratch copy is never reused: %d %s", engine.Name, code, stderr)
		}
		if db, _ := h.db.Database(c, "pc_rs_aaaaaaaaaaaaaaa"); engine.Name == databases.Postgres.Name && db.Owner != "shop" ||
			engine.Name == databases.MariaDB.Name && !slices.Equal(db.Granted, []string{"shop"}) {
			t.Errorf("%s: the scratch copy: %+v", engine.Name, db)
		}
		revokes, drop, err := databases.DiscardSQL(engine, []string{"pc_rs_aaaaaaaaaaaaaaa"}, "shop")
		if err != nil {
			t.Fatal(err)
		}
		for _, revoke := range revokes {
			h.sql(c, engine, revoke)
			if _, stderr, code := h.sql(c, engine, revoke); code != 1 || !strings.Contains(stderr, "no such grant") {
				t.Errorf("%s: a second revoke: %d %s", engine.Name, code, stderr)
			}
		}
		if _, stderr, code := h.sql(c, engine, drop); code != 0 {
			t.Errorf("%s: discard: %s", engine.Name, stderr)
		}
		if names := h.db.DatabaseNames(c); !slices.Equal(names, []string{"shop"}) {
			t.Errorf("%s: databases %q", engine.Name, names)
		}
	}

	// PostgreSQL: the swap is one transaction, retried while a session
	// holds the database; the previous copy keeps the role from going
	pg := h.db.AddServer("pcdb-swap", "postgres")
	h.db.AddDatabase(pg, "shop", "shop", "pw", "items")
	h.db.AddDatabase(pg, "pc_rs_aaaaaaaaaaaaaaa", "shop", "pw", "items", "orders")
	swap, err := databases.SwapSQL("shop", "pc_rs_aaaaaaaaaaaaaaa", "pc_old_aaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	h.db.SetBusyRenames(1)
	if _, stderr, code := h.sql(pg, databases.Postgres, swap); code != 3 || !strings.Contains(stderr, `database "shop" is being accessed by other users`) {
		t.Errorf("a busy swap: %d %s", code, stderr)
	}
	if names := h.db.DatabaseNames(pg); !slices.Equal(names, []string{"pc_rs_aaaaaaaaaaaaaaa", "shop"}) {
		t.Errorf("a failed swap changed %q", names)
	}
	if _, stderr, code := h.sql(pg, databases.Postgres, swap); code != 0 {
		t.Fatalf("the swap: %s", stderr)
	}
	shop, _ := h.db.Database(pg, "shop")
	previous, _ := h.db.Database(pg, "pc_old_aaaaaaaaaaaaaaa")
	if !slices.Equal(shop.Tables, []string{"items", "orders"}) || shop.Closed || !previous.Closed || !slices.Equal(previous.Tables, []string{"items"}) {
		t.Errorf("after the swap: %+v %+v", shop, previous)
	}
	if _, stderr, code := h.sql(pg, databases.Postgres, databases.DropManySQL(databases.Postgres, []string{"shop"}, []string{"shop"})); code != 3 ||
		!strings.Contains(stderr, `role "shop" cannot be dropped because some objects depend on it`) {
		t.Errorf("a role that owns the previous copy: %d %s", code, stderr)
	}

	// MariaDB keeps the grants of a dropped database
	maria := h.db.AddServer("pcdb-grants", "mariadb")
	h.db.AddDatabase(maria, "shop", "shop", "pw", "items")
	recreate, err := databases.RecreateSQL("shop", databases.Charset{Set: "latin1", Collation: "latin1_swedish_ci"})
	if err != nil {
		t.Fatal(err)
	}
	h.must(maria, databases.Statements(databases.MariaDB, "root", "pw", recreate))
	if db, _ := h.db.Database(maria, "shop"); len(db.Tables) != 0 || !slices.Equal(db.Granted, []string{"shop"}) || db.Charset.Set != "latin1" {
		t.Errorf("recreated: %+v", db)
	}
	if out := h.must(maria, databases.Statements(databases.MariaDB, "root", "pw", databases.CharsetSQL("shop"))); out != "latin1\tlatin1_swedish_ci\n" {
		t.Errorf("charset: %q", out)
	}
}

// TestDumpsNameTheirDatabase: a database's dump says which database it is,
// with its tables; CountDump counts them in the staged file, and refuses a
// PostgreSQL file that is no archive and a MariaDB dump cut short.
func TestDumpsNameTheirDatabase(t *testing.T) {
	h := newHarness(t)
	for _, engine := range engines() {
		c := h.db.AddServer("pcdb-"+engine.Name, engine.Name)
		h.db.AddDatabase(c, "shop", "shop", "pw", "items", "orders")
		h.db.AddDatabase(c, "blog", "blog", "pw", "posts")
		command, err := databases.DumpCommand(engine, engine.AdminUser, "shop")
		if err != nil {
			t.Fatal(err)
		}
		dump := h.must(c, databases.Script{Cmd: command})
		switch engine.Name {
		case databases.Postgres.Name:
			if !strings.HasPrefix(dump, "PGDMP") || !strings.Contains(dump, "dbname: shop\n") || !strings.Contains(dump, " TABLE public orders shop\n") ||
				strings.Contains(dump, "posts") {
				t.Errorf("postgres dump:\n%s", dump)
			}
		default:
			if !strings.Contains(dump, "Database: shop\n") || !strings.Contains(dump, "CREATE TABLE `orders`") || !strings.HasSuffix(dump, "-- Dump completed\n") ||
				!strings.Contains(dump, "DEFINER=`shop`@`%`") || strings.Contains(dump, "posts") {
				t.Errorf("mariadb dump:\n%s", dump)
			}
		}
		file := h.stage(c, databases.StagedName(engine, "c1"), dump)
		if out := h.must(c, databases.CountDump(engine, file)); out != "2\n" {
			t.Errorf("%s: count %q", engine.Name, out)
		}
		cut := h.stage(c, databases.StagedName(engine, "c2"), dump[:len(dump)-20])
		if engine.Name == databases.Postgres.Name {
			cut = h.stage(c, databases.StagedName(engine, "c2"), "-- plain SQL\n")
		}
		if _, stderr, code := h.run(c, databases.CountDump(engine, cut)); code != 1 || stderr == "" {
			t.Errorf("%s: a dump that is not whole: %d %s", engine.Name, code, stderr)
		}
		remove, err := databases.RemoveStaged(file)
		if err != nil {
			t.Fatal(err)
		}
		h.must(c, remove)
		if h.engine.File(c, file) != nil || !slices.Contains(h.engine.Files(c), cut) {
			t.Errorf("%s: files left %q", engine.Name, h.engine.Files(c))
		}
		if _, _, code := h.run(c, databases.Script{Cmd: mustDump(t, engine, "gone")}); code == 0 {
			t.Errorf("%s: a dump of a database that is not there", engine.Name)
		}
	}
}

func mustDump(t *testing.T, engine databases.Engine, name string) []string {
	t.Helper()
	command, err := databases.DumpCommand(engine, engine.AdminUser, name)
	if err != nil {
		t.Fatal(err)
	}
	return command
}

// TestARestoreRunsAsTheDatabasesOwnUser (AC1): a restore signs in as the
// database's user with its password and writes only its database; a dump
// that names another database fails on privileges and leaves that one as
// it was; names with "fail" fail, and "short" counts one table fewer.
func TestARestoreRunsAsTheDatabasesOwnUser(t *testing.T) {
	h := newHarness(t)
	for _, engine := range engines() {
		c := h.db.AddServer("pcdb-"+engine.Name, engine.Name)
		h.db.AddDatabase(c, "shop", "shop", "shop-pw", "items", "orders")
		h.db.AddDatabase(c, "blog", "blog", "blog-pw", "posts")
		h.db.AddDatabase(c, "pc_rs_aaaaaaaaaaaaaaa", "shop", "shop-pw")
		dump := h.must(c, databases.Script{Cmd: mustDump(t, engine, "shop")})
		file := h.stage(c, databases.StagedName(engine, "r1"), dump)
		restore := func(user, password, target, definer string) (string, int) {
			script, err := databases.RestoreAsUser(engine, user, password, target, file, definer)
			if err != nil {
				t.Fatal(err)
			}
			_, stderr, code := h.run(c, script)
			return stderr, code
		}
		if stderr, code := restore("shop", "wrong", "pc_rs_aaaaaaaaaaaaaaa", ""); code == 0 || !strings.Contains(stderr, "shop") {
			t.Errorf("%s: a wrong password: %d %s", engine.Name, code, stderr)
		}
		if stderr, code := restore("shop", "shop-pw", "blog", ""); code == 0 || !strings.Contains(stderr, "blog") {
			t.Errorf("%s: another user's database: %d %s", engine.Name, code, stderr)
		}
		if blog, _ := h.db.Database(c, "blog"); !slices.Equal(blog.Tables, []string{"posts"}) {
			t.Errorf("%s: blog: %+v", engine.Name, blog)
		}
		if stderr, code := restore("shop", "shop-pw", "pc_rs_aaaaaaaaaaaaaaa", ""); code != 0 {
			t.Fatalf("%s: the restore: %s", engine.Name, stderr)
		}
		scratch, _ := h.db.Database(c, "pc_rs_aaaaaaaaaaaaaaa")
		if !slices.Equal(scratch.Tables, []string{"items", "orders"}) || scratch.From != "shop" {
			t.Errorf("%s: restored %+v", engine.Name, scratch)
		}
		count := databases.CountBaseTables(engine, engine.AdminUser, "pw", "pc_rs_aaaaaaaaaaaaaaa")
		if out := h.must(c, count); out != "2\n" {
			t.Errorf("%s: counted %q", engine.Name, out)
		}

		// a crafted dump that names another database
		crafted := "-- Host: localhost    Database: shop\nCREATE TABLE `extra` (`id` int);\nUSE blog;\nDROP TABLE posts;\n-- Dump completed\n"
		if engine.Name == databases.Postgres.Name {
			crafted = PostgresDump("shop", "shop", "extra") + "\\connect blog\n"
		}
		file = h.stage(c, databases.StagedName(engine, "r2"), crafted)
		h.db.AddDatabase(c, "pc_rs_bbbbbbbbbbbbbbb", "shop", "shop-pw")
		if stderr, code := restore("shop", "shop-pw", "pc_rs_bbbbbbbbbbbbbbb", ""); code != 1 ||
			engine.Name == databases.MariaDB.Name && !strings.Contains(stderr, "Access denied for user 'shop'@'%' to database 'blog'") ||
			engine.Name == databases.Postgres.Name && !strings.Contains(stderr, "permission denied for database blog") {
			t.Errorf("%s: a crafted dump: %d %s", engine.Name, code, stderr)
		}
		if blog, _ := h.db.Database(c, "blog"); !slices.Equal(blog.Tables, []string{"posts"}) {
			t.Errorf("%s: blog after a crafted dump: %+v", engine.Name, blog)
		}

		// fail and short
		h.db.AddDatabase(c, "fail_shop", "shop", "shop-pw")
		file = h.stage(c, databases.StagedName(engine, "r3"), dump)
		if _, code := restore("shop", "shop-pw", "fail_shop", ""); code != 1 {
			t.Errorf("%s: a failing restore: %d", engine.Name, code)
		}
		h.db.AddDatabase(c, "short_shop", "shop", "shop-pw")
		if _, code := restore("shop", "shop-pw", "short_shop", ""); code != 0 {
			t.Errorf("%s: a short restore failed", engine.Name)
		}
		if out := h.must(c, databases.CountBaseTables(engine, engine.AdminUser, "pw", "short_shop")); out != "1\n" {
			t.Errorf("%s: a short count %q", engine.Name, out)
		}
	}

	// MariaDB: a definer of another user, as in a new database, needs the
	// rewrite
	c := h.db.AddServer("pcdb-definer", "mariadb")
	h.db.AddDatabase(c, "shop", "shop", "pw", "items")
	h.db.AddDatabase(c, "copy", "copy", "copy-pw")
	dump := h.must(c, databases.Script{Cmd: mustDump(t, databases.MariaDB, "shop")})
	file := h.stage(c, databases.StagedName(databases.MariaDB, "d1"), dump)
	plain, _ := databases.RestoreAsUser(databases.MariaDB, "copy", "copy-pw", "copy", file, "")
	if _, stderr, code := h.run(c, plain); code != 1 || !strings.Contains(stderr, "SUPER") {
		t.Errorf("a definer of another user: %d %s", code, stderr)
	}
	rewritten, _ := databases.RestoreAsUser(databases.MariaDB, "copy", "copy-pw", "copy", file, "copy")
	if _, stderr, code := h.run(c, rewritten); code != 0 {
		t.Errorf("the definer rewritten: %s", stderr)
	}
}

// TestTheRoomSessionsAndSandbox: df's numbers, the sessions of a database,
// which EndSessions ends, the sandbox of the MariaDB client, and the
// processes noted with the databases they named.
func TestTheRoomSessionsAndSandbox(t *testing.T) {
	h := newHarness(t)
	for _, engine := range engines() {
		c := h.db.AddServer("pcdb-"+engine.Name, engine.Name)
		h.db.AddDatabase(c, "shop", "shop", "pw", "items")
		h.db.SetFree(8<<30, 2<<30)
		data, tmp, err := databases.ParseFreeSpace(h.must(c, databases.FreeSpace(engine.DataDir)))
		if err != nil || data != 8<<30 || tmp != 2<<30 {
			t.Errorf("%s: room %d %d %v", engine.Name, data, tmp, err)
		}
		h.db.SetSessions(c, "shop", "172.18.0.5", "172.18.0.9")
		if out := h.must(c, databases.Statements(engine, engine.AdminUser, "pw", databases.SessionsSQL(engine, "shop"))); out != "172.18.0.5\n172.18.0.9\n" {
			t.Errorf("%s: sessions %q", engine.Name, out)
		}
		if out := h.must(c, databases.EndSessions(engine, engine.AdminUser, "pw", "shop")); out != "2\n" {
			t.Errorf("%s: ended %q", engine.Name, out)
		}
		if out := h.must(c, databases.EndSessions(engine, engine.AdminUser, "pw", "shop")); out != "0\n" {
			t.Errorf("%s: ended again %q", engine.Name, out)
		}
		if _, _, code := h.run(c, databases.SignIn(engine, "shop", "pw", "shop")); code != 0 {
			t.Errorf("%s: sign in", engine.Name)
		}
		if _, _, code := h.run(c, databases.SignIn(engine, "shop", "stale", "shop")); code == 0 {
			t.Errorf("%s: a stale password signs in", engine.Name)
		}
	}
	c := h.db.AddServer("pcdb-sandbox", "mariadb")
	if out := h.must(c, databases.Sandbox()); out != "yes\n" {
		t.Errorf("sandbox %q", out)
	}
	h.db.SetSandbox(false)
	if out := h.must(c, databases.Sandbox()); out != "no\n" {
		t.Errorf("no sandbox %q", out)
	}
	execs := h.db.Execs()
	last := execs[len(execs)-1]
	if last.Kind != KindSandbox || last.Container != c {
		t.Errorf("the last process: %+v", last)
	}
	var ended []Exec
	for _, exec := range execs {
		if exec.Kind == KindEndSessions {
			ended = append(ended, exec)
		}
	}
	if len(ended) != 4 || !slices.Equal(ended[0].Databases, []string{"shop"}) {
		t.Errorf("sessions ended: %+v", ended)
	}
}

// TestADumpCanBeCrafted: a test can make a database's dumps hold what a
// crafted backup would.
func TestADumpCanBeCrafted(t *testing.T) {
	h := newHarness(t)
	c := h.db.AddServer("pcdb-maria", "mariadb")
	h.db.AddDatabase(c, "shop", "shop", "pw", "items")
	h.db.SetDumpContent("pcdb-maria", "shop", "USE blog;\n-- Dump completed\n")
	if out := h.must(c, databases.Script{Cmd: mustDump(t, databases.MariaDB, "shop")}); out != "USE blog;\n-- Dump completed\n" {
		t.Errorf("crafted %q", out)
	}
	h.db.SetDumpContent(c, "shop", "")
	if out := h.must(c, databases.Script{Cmd: mustDump(t, databases.MariaDB, "shop")}); !strings.Contains(out, "Database: shop") {
		t.Errorf("own dump %q", out)
	}
}
