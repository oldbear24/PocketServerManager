package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakedb"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

// Database restores and restore tests (#53), on the fake engine, the fake
// restic and the fake database servers.

const (
	testRestoreID = "rrrrrrrrrrrrrrr"
	testCheckID   = "ccccccccccccccc"
	adminPassword = "admin-pw-1"
	shopPassword  = "shop-pw-1"
)

// dbHarness is a restore harness with a PostgreSQL and a MariaDB shared
// server, whose processes it notes among what happened.
type dbHarness struct {
	*restoreHarness
	db *fakedb.Server
	// containers by engine, and the dumps staged, as the archive PUT
	// copied them in, by their file
	servers map[string]string
	smu     sync.Mutex
	staged  map[string]string
	// inject, when set, answers a process instead of the fake servers
	inject func(call *fakedocker.ExecCall) (fakedocker.ExecResult, bool)
	// tt is the test, for setups that need it
	tt *testing.T
}

func newDBHarness(t *testing.T) *dbHarness {
	t.Helper()
	h := newRestoreHarness(t)
	d := &dbHarness{restoreHarness: h, db: fakedb.New(h.engine), servers: map[string]string{}, staged: map[string]string{}, tt: t}
	h.engine.OnExec = func(call *fakedocker.ExecCall, stdin []byte) fakedocker.ExecResult {
		d.smu.Lock()
		inject := d.inject
		d.smu.Unlock()
		if inject != nil {
			if result, ok := inject(call); ok {
				h.note("exec:injected")
				return result
			}
		}
		result := d.db.Answer(call, stdin)
		execs := d.db.Execs()
		last := execs[len(execs)-1]
		if last.Kind == fakedb.KindCountDump {
			file := last.Env["PC_FILE"]
			d.smu.Lock()
			d.staged[file] = string(h.engine.File(call.Container, file))
			d.smu.Unlock()
		}
		h.note("exec:" + execLabel(last))
		return result
	}
	d.servers[databases.Postgres.Name] = d.db.AddServer("pcdb-main-db-1", databases.Postgres.Name)
	d.servers[databases.MariaDB.Name] = d.db.AddServer("pcdb-maria-db-1", databases.MariaDB.Name)
	wait := swapWait
	swapWait = time.Millisecond
	t.Cleanup(func() { swapWait = wait })
	return d
}

// setInject sets what answers processes instead of the fake servers.
func (d *dbHarness) setInject(inject func(call *fakedocker.ExecCall) (fakedocker.ExecResult, bool)) {
	d.smu.Lock()
	defer d.smu.Unlock()
	d.inject = inject
}

// execLabel says what a process did, shortly, as what happened notes it.
func execLabel(e fakedb.Exec) string {
	sql := e.Env["PC_SQL"]
	switch {
	case e.Kind != fakedb.KindStatements:
		return e.Kind + ":" + strings.Join(e.Databases, ",")
	case strings.Contains(sql, " RENAME TO "):
		return "swap"
	case strings.HasPrefix(sql, "CREATE DATABASE") && strings.Contains(sql, databases.TemporaryScratch):
		return "scratch"
	case strings.HasPrefix(sql, "CREATE DATABASE") && strings.Contains(sql, databases.TemporaryTest):
		return "test-database"
	case strings.HasPrefix(sql, "DROP DATABASE IF EXISTS `") && strings.Contains(sql, "CREATE DATABASE"):
		return "recreate"
	case strings.HasPrefix(sql, "DROP DATABASE"):
		return "drop:" + strings.Join(e.Databases, ",")
	case strings.HasPrefix(sql, "REVOKE ALL PRIVILEGES"):
		return "revoke"
	case strings.HasPrefix(sql, "ALTER DATABASE") && strings.Contains(sql, "ALLOW_CONNECTIONS false"):
		return "close"
	case strings.HasPrefix(sql, "DO $$"), strings.HasPrefix(sql, "CREATE DATABASE IF NOT EXISTS"):
		return "create"
	}
	return "sql"
}

// interesting are what happened, without the processes that only read.
func (d *dbHarness) interesting() []string {
	var events []string
	for _, event := range d.happened() {
		if event == "exec:sql" || strings.HasPrefix(event, "exec:"+fakedb.KindSweepStaged) || strings.HasPrefix(event, "exec:"+fakedb.KindFreeSpace) ||
			strings.HasPrefix(event, "exec:"+fakedb.KindRemoveStaged) || strings.HasPrefix(event, "exec:"+fakedb.KindDump) {
			continue
		}
		events = append(events, event)
	}
	return events
}

// connection is a server's, as the panel completes it.
func connection(engine databases.Engine) *agentv1.DbConnection {
	container := "pcdb-main-db-1"
	if engine.Name == databases.MariaDB.Name {
		container = "pcdb-maria-db-1"
	}
	return &agentv1.DbConnection{ContainerId: container, Engine: engine.Name, AdminUser: engine.AdminUser, AdminPassword: adminPassword}
}

// backupDatabase backs a database of a server up as the job testJobTag, as
// a database job's run does, and says the snapshot and the dump.
func (d *dbHarness) backupDatabase(t *testing.T, engine databases.Engine, database string) (string, string) {
	t.Helper()
	command, err := databases.DumpCommand(engine, engine.AdminUser, database)
	if err != nil {
		t.Fatal(err)
	}
	file := databases.BackupFile(engine, database)
	result, err := d.handleBackupRun(context.Background(), runCommand(&agentv1.BackupRun{
		Repository: d.repo, StdinContainer: connection(engine).GetContainerId(), StdinFileName: file, StdinCommand: command,
		Tags: []string{"pocketcloud", testJobTag}, ExtraTags: backup.DatabaseTags(database, "main"), HostName: "nas",
	}), &Reporter{})
	if err != nil {
		t.Fatalf("the backup of %s: %v %v", database, err, result)
	}
	snapshots := fakerestic.Snapshots(t, d.root, d.repo.GetUrl())
	return result["snapshot_id"].(string), snapshots[len(snapshots)-1].Files["/"+file]
}

// replace is a restore of a database in place, as the panel sends it, with
// what stops confirmed.
func (d *dbHarness) replace(engine databases.Engine, snapshot, database string, confirmed ...string) *agentv1.BackupRestore {
	previous := ""
	if engine.Name == databases.Postgres.Name {
		previous = databases.PreviousName(testRestoreID)
	}
	return &agentv1.BackupRestore{
		Repository: d.repo, SnapshotId: snapshot, JobTag: testJobTag, RestoreId: testRestoreID, Overwrite: true, Stack: "shop",
		ConfirmedStops: confirmed,
		Database: &agentv1.BackupDbRestore{Connection: connection(engine), Database: database, File: databases.BackupFile(engine, database),
			Mode: backup.DbRestoreReplace, User: database, Password: shopPassword, Scratch: databases.ScratchName(testRestoreID), Previous: previous},
	}
}

// app adds a stack's running container.
func (d *dbHarness) app(name, project string) string {
	return d.container(name, map[string]string{composeProjectLabel: project, composeServiceLabel: "web"}, "unless-stopped", 0)
}

// shop gives a server the database shop with its user and tables.
func (d *dbHarness) shop(engine databases.Engine) {
	d.db.AddDatabase(d.servers[engine.Name], "shop", "shop", shopPassword, "items", "orders")
	d.db.AddDatabase(d.servers[engine.Name], "blog", "blog", "blog-pw-1", "posts")
}

// TestAPostgresRestoreInPlaceSwapsInARestoredCopy (#53, AC1): the dump goes
// into a scratch copy, as the database's user, while the application runs;
// only once the copy has the dump's tables does the application stop, and
// the copy takes the database's place in one transaction, tried again when
// a session came back; the data before it stays as pc_old_<restore id>,
// closed; the application starts again. The archive PUT copies exactly the
// snapshot's file, which goes once the restore ends. The other databases of
// the server, and the server's container, are left alone.
func TestAPostgresRestoreInPlaceSwapsInARestoredCopy(t *testing.T) {
	d := newDBHarness(t)
	pg := databases.Postgres
	d.shop(pg)
	snapshot, dump := d.backupDatabase(t, pg, "shop")
	web := d.app("shop-web-1", "shop")
	d.app("blog-web-1", "blog")
	d.db.SetSessions(d.servers[pg.Name], "shop", "172.17.0.2", "10.0.0.9")
	d.db.SetSettings(d.servers[pg.Name], "shop", "search_path=app")

	preview, err := d.restore(context.Background(), "p1", func() *agentv1.BackupRestore {
		p := d.replace(pg, snapshot, "shop")
		p.DryRun, p.Overwrite = true, false
		return p
	}())
	if err != nil {
		t.Fatal(err)
	}
	var seen backup.DbRestorePreview
	raw, _ := json.Marshal(preview)
	if err := json.Unmarshal(raw, &seen); err != nil {
		t.Fatal(err)
	}
	if !seen.DryRun || seen.Snapshot.ID != snapshot || !slices.Equal(seen.Snapshot.Paths, []string{"/shop.pgdump"}) || seen.TotalBytes != int64(len(dump)) ||
		len(seen.Roots) != 1 || !seen.Roots[0].InSnapshot || !seen.Roots[0].Exists || seen.Roots[0].Entries != 2 || seen.Roots[0].Destination != "database shop" {
		t.Errorf("the preview: %+v", seen)
	}
	if !seen.Database.Exists || !seen.Database.UserExists || seen.Database.Tables != 2 || !seen.Database.SignIn || seen.Database.ConnectionsOther != 1 ||
		!slices.Equal(seen.Database.Settings, []string{"search_path=app"}) || seen.Database.SizeBytes == 0 {
		t.Errorf("what the preview found: %+v", seen.Database)
	}
	if len(seen.WouldStop) != 1 || seen.WouldStop[0] != (backup.WouldStop{Name: "shop-web-1", Project: "shop", Service: "web"}) {
		t.Errorf("would stop %+v", seen.WouldStop)
	}
	if seen.Free.Data != 50<<30 || seen.Needed.Tmp != int64(len(dump))+stageReserve || seen.Needed.Data != seen.Database.SizeBytes*6/5+dataReserve ||
		len(seen.Notes) != 2 || !strings.Contains(seen.Notes[0], "search_path=app") {
		t.Errorf("room and notes: %+v %+v %q", seen.Free, seen.Needed, seen.Notes)
	}
	if d.status("shop-web-1") != "running" || len(d.db.DatabaseNames(d.servers[pg.Name])) != 2 {
		t.Error("the preview changed something")
	}

	d.events = nil
	d.db.SetBusyRenames(1)
	result, err := d.restore(context.Background(), "c1", d.replace(pg, snapshot, "shop", "shop-web-1"))
	if err != nil {
		t.Fatalf("the restore: %v\n%v", err, result["log"])
	}
	want := []string{"exec:count-dump:shop", "exec:scratch", "exec:restore:" + databases.ScratchName(testRestoreID) + ",shop",
		"exec:count-tables:" + databases.ScratchName(testRestoreID), "stop:shop-web-1", "exec:end-sessions:shop", "swap", "swap", "start:shop-web-1"}
	got := d.interesting()
	for i, event := range got {
		got[i] = strings.TrimPrefix(event, "exec:")
		if event == "exec:swap" {
			got[i] = "swap"
		}
	}
	for i := range want {
		want[i] = strings.TrimPrefix(want[i], "exec:")
	}
	if !slices.Equal(got, want) {
		t.Errorf("what happened:\n%q\nwant\n%q", got, want)
	}
	if result["tables"] != float64(2) || result["tables_expected"] != float64(2) || result["previous"] != databases.PreviousName(testRestoreID) ||
		result["files_restored"] != float64(1) || result["bytes_restored"] != float64(len(dump)) || result["connections_ended"] != float64(2) ||
		result["partial"] != false || result["dropped"] != false {
		t.Errorf("the result: %v", result)
	}
	if stopped, _ := result["app_stopped"].([]any); len(stopped) != 1 || stopped[0].(map[string]any)["id"] != web {
		t.Errorf("stopped %v", result["app_stopped"])
	}
	shop, _ := d.db.Database(d.servers[pg.Name], "shop")
	previous, _ := d.db.Database(d.servers[pg.Name], databases.PreviousName(testRestoreID))
	blog, _ := d.db.Database(d.servers[pg.Name], "blog")
	if !slices.Equal(shop.Tables, []string{"items", "orders"}) || shop.From != "shop" || shop.Owner != "shop" || !previous.Closed ||
		!slices.Equal(blog.Tables, []string{"posts"}) || blog.From != "" {
		t.Errorf("after: shop %+v, previous %+v, blog %+v", shop, previous, blog)
	}
	staged := databases.StagedDir + "/" + databases.StagedName(pg, "c1")
	d.smu.Lock()
	copied := d.staged[staged]
	d.smu.Unlock()
	if copied != dump || len(d.engine.Files(d.servers[pg.Name])) != 0 {
		t.Errorf("staged %q, want the snapshot's %q; left %q", copied, dump, d.engine.Files(d.servers[pg.Name]))
	}
	if d.status("pcdb-main-db-1") != "running" || d.engine.Called("POST /containers/"+d.servers[pg.Name]+"/stop") || d.status("blog-web-1") != "running" {
		t.Error("the server's container or another stack stopped")
	}
	for _, exec := range d.db.Execs() {
		if exec.Kind == fakedb.KindRestore && (exec.Env["PC_USER"] != "shop" || exec.Env["PGPASSWORD"] != shopPassword) {
			t.Errorf("a restore ran as %q", exec.Env["PC_USER"])
		}
		for _, name := range exec.Databases {
			if name != "shop" && !strings.HasSuffix(name, testRestoreID) {
				t.Errorf("a process named %s: %v", name, exec.Cmd)
			}
		}
	}
	if _, err := os.Stat(d.markerPath("c1")); !os.IsNotExist(err) {
		t.Errorf("the marker is left: %v", err)
	}
}

// TestAPostgresRestoreThatFailsBeforeTheSwapChangesNothing (#53, AC1): a
// copy whose tables are not the dump's, or a restore that fails, drops the
// copy, stops nothing, and says nothing changed; a swap that never gets the
// database starts the application again.
func TestAPostgresRestoreThatFailsBeforeTheSwapChangesNothing(t *testing.T) {
	d := newDBHarness(t)
	pg := databases.Postgres
	d.db.AddDatabase(d.servers[pg.Name], "short_shop", "short_shop", shopPassword, "items", "orders")
	snapshot, _ := d.backupDatabase(t, pg, "short_shop")
	d.app("shop-web-1", "shop")
	_, err := d.restore(context.Background(), "c1", d.replace(pg, snapshot, "short_shop", "shop-web-1"))
	if err == nil || err.Error() != "Nothing changed in short_shop: "+databases.ScratchName(testRestoreID)+" has 1 tables after the restore, and the dump 2." {
		t.Errorf("a copy that is short: %v", err)
	}
	if slices.Contains(d.happened(), "stop:shop-web-1") || slices.Contains(d.db.DatabaseNames(d.servers[pg.Name]), databases.ScratchName(testRestoreID)) {
		t.Errorf("after: %q, databases %q", d.happened(), d.db.DatabaseNames(d.servers[pg.Name]))
	}

	d.db.SetBusyRenames(swapTries)
	d.db.AddDatabase(d.servers[pg.Name], "shop", "shop", shopPassword, "items")
	snapshot, _ = d.backupDatabase(t, pg, "shop")
	_, err = d.restore(context.Background(), "c2", d.replace(pg, snapshot, "shop", "shop-web-1"))
	if err == nil || !strings.HasPrefix(err.Error(), "Nothing changed in shop: ") || !strings.Contains(err.Error(), "is being accessed by other users") {
		t.Errorf("a swap that never got the database: %v", err)
	}
	if d.status("shop-web-1") != "running" || d.policy(t, "shop-web-1").Name != "unless-stopped" ||
		!slices.Equal(d.db.DatabaseNames(d.servers[pg.Name]), []string{"shop", "short_shop"}) {
		t.Errorf("after: %s %v %q", d.status("shop-web-1"), d.policy(t, "shop-web-1"), d.db.DatabaseNames(d.servers[pg.Name]))
	}
}

// TestAMariaDBRestoreInPlaceRehearsesFirst (#53): the dump goes into a
// scratch copy first, as the database's user, which is counted and dropped
// with the user's privileges on it; then the application stops, the
// database is dropped and created again with its character set, keeping
// its grants, and the dump goes into it. A failure once it was emptied
// leaves the application stopped, with its restart policies back, and says
// so.
func TestAMariaDBRestoreInPlaceRehearsesFirst(t *testing.T) {
	d := newDBHarness(t)
	maria := databases.MariaDB
	d.shop(maria)
	server := d.servers[maria.Name]
	d.db.SetCharset(server, "shop", databases.Charset{Set: "latin1", Collation: "latin1_swedish_ci"})
	snapshot, _ := d.backupDatabase(t, maria, "shop")
	d.app("shop-web-1", "shop")
	result, err := d.restore(context.Background(), "c1", d.replace(maria, snapshot, "shop", "shop-web-1"))
	if err != nil {
		t.Fatalf("the restore: %v\n%v", err, result["log"])
	}
	scratch := databases.ScratchName(testRestoreID)
	want := []string{"count-dump:shop", "scratch", "restore:" + scratch + ",shop", "count-tables:" + scratch, "end-sessions:" + scratch,
		"revoke", "drop:" + scratch, "stop:shop-web-1", "end-sessions:shop", "recreate", "restore:shop", "count-tables:shop", "start:shop-web-1"}
	var got []string
	for _, event := range d.interesting() {
		got = append(got, strings.TrimPrefix(event, "exec:"))
	}
	if !slices.Equal(got, want) {
		t.Errorf("what happened:\n%q\nwant\n%q", got, want)
	}
	shop, _ := d.db.Database(server, "shop")
	if !slices.Equal(shop.Tables, []string{"items", "orders"}) || !slices.Equal(shop.Granted, []string{"shop"}) || shop.Charset.Set != "latin1" {
		t.Errorf("shop after: %+v", shop)
	}
	if copied, there := d.db.Database(server, scratch); there || len(copied.Granted) != 0 || result["previous"] != "" {
		t.Errorf("the rehearsal's copy is left: %+v %v", copied, result["previous"])
	}

	// a failure once the database was emptied
	d.setInject(func(call *fakedocker.ExecCall) (fakedocker.ExecResult, bool) {
		if strings.Contains(strings.Join(call.Cmd, " "), "--one-database") && slices.Contains(call.Env, "PC_DATABASE=shop") {
			return fakedocker.ExecResult{Code: 1, Stderr: "ERROR 2013 (HY000) at line 40: Lost connection to server during query\n"}, true
		}
		return fakedocker.ExecResult{}, false
	})
	result, err = d.restore(context.Background(), "c2", d.replace(maria, snapshot, "shop", "shop-web-1"))
	if err == nil || err.Error() != "The restore failed after shop was emptied: it holds part of the backup, and shop-web-1 stay stopped. Restore again, then start them on their pages." {
		t.Errorf("a failure after the drop: %v", err)
	}
	if result["partial"] != true || !slices.Equal(anyStrings(result["app_left_stopped"]), []string{"shop-web-1"}) || len(anyStrings(result["app_started"])) != 0 {
		t.Errorf("the result: %v", result)
	}
	if d.status("shop-web-1") == "running" || d.policy(t, "shop-web-1").Name != "unless-stopped" {
		t.Errorf("the application after: %s %v", d.status("shop-web-1"), d.policy(t, "shop-web-1"))
	}
	if _, err := os.Stat(d.markerPath("c2")); !os.IsNotExist(err) {
		t.Errorf("the marker is left: %v", err)
	}
	if held := d.restoring.holds("shop-web-1"); held {
		t.Error("the restore still holds the container")
	}
}

// TestADatabaseRestoreReportsItsStepsInTheWebsOrder (#53): the steps a
// database restore reports come in the order the web shows them: PostgreSQL
// in place restores its copy before the stop and swaps it in after it;
// MariaDB in place rehearses before the stop and restores after it; a new
// database stops nothing.
func TestADatabaseRestoreReportsItsStepsInTheWebsOrder(t *testing.T) {
	inPlace := map[string][]string{
		databases.Postgres.Name: {"check", "fetch", "dbrestore", "stop", "swap", "start", "verify"},
		databases.MariaDB.Name:  {"check", "fetch", "rehearse", "stop", "dbrestore", "start", "verify"},
	}
	for _, engine := range []databases.Engine{databases.Postgres, databases.MariaDB} {
		for _, mode := range []string{backup.DbRestoreReplace, backup.DbRestoreNew} {
			t.Run(engine.Name+"/"+mode, func(t *testing.T) {
				d := newDBHarness(t)
				d.shop(engine)
				snapshot, _ := d.backupDatabase(t, engine, "shop")
				d.app("shop-web-1", "shop")
				payload, want := d.replace(engine, snapshot, "shop", "shop-web-1"), inPlace[engine.Name]
				if mode == backup.DbRestoreNew {
					payload = &agentv1.BackupRestore{Repository: d.repo, SnapshotId: snapshot, JobTag: testJobTag, RestoreId: testRestoreID,
						Database: &agentv1.BackupDbRestore{Connection: connection(engine), Database: "shop_copy", File: databases.BackupFile(engine, "shop"),
							Mode: backup.DbRestoreNew, User: "shop_copy", Password: "copy-pw-1"}}
					want = []string{"check", "fetch", "dbrestore"}
				}
				panel := &fakePanel{}
				if result, err := d.handleBackupRestore(context.Background(), restoreCommand("c1", payload), reporterFor(t, panel, "c1")); err != nil {
					t.Fatalf("%v\n%v", err, result["log"])
				}
				var got []string
				for _, progress := range panel.progress("c1") {
					if len(got) == 0 || got[len(got)-1] != progress.GetStep() {
						got = append(got, progress.GetStep())
					}
				}
				if !slices.Equal(got, want) {
					t.Errorf("the steps %q, want %q", got, want)
				}
			})
		}
	}
}

// TestARestoreIntoANewDatabase (#53): the database and its user are made
// with the delivered password, the dump goes in as that user (on MariaDB
// with the user as its definers), and nothing stops; a failure drops both
// again; a name that is taken is refused before anything is made.
func TestARestoreIntoANewDatabase(t *testing.T) {
	for _, engine := range []databases.Engine{databases.Postgres, databases.MariaDB} {
		t.Run(engine.Name, func(t *testing.T) {
			d := newDBHarness(t)
			d.shop(engine)
			server := d.servers[engine.Name]
			snapshot, _ := d.backupDatabase(t, engine, "shop")
			d.app("shop-web-1", "shop")
			payload := func() *agentv1.BackupRestore {
				return &agentv1.BackupRestore{Repository: d.repo, SnapshotId: snapshot, JobTag: testJobTag, RestoreId: testRestoreID,
					Database: &agentv1.BackupDbRestore{Connection: connection(engine), Database: "shop_copy", File: databases.BackupFile(engine, "shop"),
						Mode: backup.DbRestoreNew, User: "shop_copy", Password: "copy-pw-1"}}
			}
			result, err := d.restore(context.Background(), "c1", payload())
			if err != nil {
				t.Fatalf("%v\n%v", err, result["log"])
			}
			copied, _ := d.db.Database(server, "shop_copy")
			password, _ := d.db.Password(server, "shop_copy")
			if !slices.Equal(copied.Tables, []string{"items", "orders"}) || password != "copy-pw-1" || result["tables"] != float64(2) || result["dropped"] != false {
				t.Errorf("the new database: %+v %q %v", copied, password, result)
			}
			if slices.ContainsFunc(d.happened(), func(event string) bool { return strings.HasPrefix(event, "stop:") }) {
				t.Errorf("something stopped: %q", d.happened())
			}
			for _, exec := range d.db.Execs() {
				if exec.Kind == fakedb.KindRestore && engine.Name == databases.MariaDB.Name && !slices.Contains(exec.Cmd, "sh") {
					t.Errorf("a restore: %v", exec.Cmd)
				}
				if exec.Kind == fakedb.KindRestore && engine.Name == databases.MariaDB.Name && exec.Env["PC_DEFINER"] != "shop_copy" {
					t.Errorf("the definer: %v", exec.Env)
				}
			}

			// taken
			if _, err := d.restore(context.Background(), "c2", payload()); err == nil || !strings.Contains(err.Error(), "A database or user named shop_copy exists on") {
				t.Errorf("a name that is taken: %v", err)
			}

			// a failure drops what was made
			failing := payload()
			failing.Database.Database, failing.Database.User = "fail_copy", "fail_copy"
			result, err = d.restore(context.Background(), "c3", failing)
			if err == nil || !strings.HasPrefix(err.Error(), "The restore into the new database fail_copy failed, and it was dropped again: ") || result["dropped"] != true {
				t.Errorf("a failure: %v %v", err, result["dropped"])
			}
			if _, there := d.db.Database(server, "fail_copy"); there {
				t.Error("the database is left")
			}
			if _, there := d.db.Password(server, "fail_copy"); there {
				t.Error("the user is left")
			}
		})
	}
}

// TestADatabaseRestoreGoesIntoASharedServersContainerOnly (#53, AC1): a
// container that is not a running shared server's is refused, before
// anything is done.
func TestADatabaseRestoreGoesIntoASharedServersContainerOnly(t *testing.T) {
	d := newDBHarness(t)
	pg := databases.Postgres
	d.shop(pg)
	snapshot, _ := d.backupDatabase(t, pg, "shop")
	d.container("umami-db-1", map[string]string{composeProjectLabel: "umami"}, "always", 0)
	jobs := len(d.restoreJobs())
	for _, container := range []string{"umami-db-1", "gone"} {
		payload := d.replace(pg, snapshot, "shop")
		payload.Database.Connection.ContainerId = container
		if _, err := d.restore(context.Background(), "c1", payload); err == nil || err.Error() != "Database restores go into a shared server's container only." {
			t.Errorf("%s: %v", container, err)
		}
	}
	d.engine.SetStatus(d.servers[pg.Name], "exited")
	if _, err := d.restore(context.Background(), "c1", d.replace(pg, snapshot, "shop")); err == nil || err.Error() != "Database restores go into a shared server's container only." {
		t.Errorf("a stopped server: %v", err)
	}
	if len(d.restoreJobs()) != jobs {
		t.Error("a job ran")
	}
}

// TestWhatARestoreInPlaceStops (#53, AC1): the containers of the
// database's stack and of the stacks that name it, and a site's <site>-php
// and <site>-worker by name, though they are PocketCloud's; never the
// server's container, though it is of the stack, nor another PocketCloud
// container; a container the preview did not show refuses the restore
// before anything is done.
func TestWhatARestoreInPlaceStops(t *testing.T) {
	d := newDBHarness(t)
	pg := databases.Postgres
	d.shop(pg)
	snapshot, _ := d.backupDatabase(t, pg, "shop")
	d.app("shop-web-1", "shop")
	d.app("reports-1", "reports")
	d.container("shop-db-1", map[string]string{composeProjectLabel: "shop", "pocketcloud.role": "db"}, "always", 0)
	d.container("wp-php", map[string]string{"pocketcloud.role": "site"}, "unless-stopped", 0)
	d.container("wp-worker", map[string]string{"pocketcloud.role": "site"}, "unless-stopped", 0)
	d.container("wp-edge", map[string]string{"pocketcloud.role": "edge"}, "always", 0)
	payload := d.replace(pg, snapshot, "shop")
	payload.Database.StopProjects = []string{"reports"}
	payload.Database.StopContainers = []string{"wp-php", "wp-worker"}
	payload.DryRun, payload.Overwrite = true, false
	preview, err := d.restore(context.Background(), "p1", payload)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, raw := range preview["would_stop"].([]any) {
		names = append(names, raw.(map[string]any)["name"].(string))
	}
	slices.Sort(names)
	if want := []string{"reports-1", "shop-web-1", "wp-php", "wp-worker"}; !slices.Equal(names, want) {
		t.Errorf("would stop %q, want %q", names, want)
	}
	// a container started since the preview
	payload.DryRun, payload.Overwrite = false, true
	payload.ConfirmedStops = []string{"reports-1", "shop-web-1", "wp-php"}
	if _, err := d.restore(context.Background(), "c1", payload); err == nil || err.Error() != "wp-worker started using the destination after the preview: preview again." {
		t.Errorf("an unconfirmed stop: %v", err)
	}
	if len(d.db.DatabaseNames(d.servers[pg.Name])) != 2 || slices.ContainsFunc(d.happened(), func(e string) bool { return strings.HasPrefix(e, "stop:") }) {
		t.Errorf("something changed: %q", d.happened())
	}
	payload.ConfirmedStops = names
	if result, err := d.restore(context.Background(), "c2", payload); err != nil {
		t.Fatalf("%v\n%v", err, result["log"])
	}
	for name, stopped := range map[string]bool{"shop-web-1": true, "reports-1": true, "wp-php": true, "wp-worker": true, "shop-db-1": false, "wp-edge": false} {
		if called := d.engine.Called("POST /containers/" + d.engine.Find(name).ID + "/stop"); called != stopped {
			t.Errorf("%s stopped: %v", name, called)
		}
	}
	if d.engine.Called("POST /containers/" + d.servers[pg.Name] + "/stop") {
		t.Error("the server's container stopped")
	}
}

// TestARestoreWaitsForTheDumpOfItsDatabase (#53): a dump of the database
// that runs holds it for reading; its restore is refused meanwhile.
func TestARestoreWaitsForTheDumpOfItsDatabase(t *testing.T) {
	d := newDBHarness(t)
	pg := databases.Postgres
	d.shop(pg)
	snapshot, _ := d.backupDatabase(t, pg, "shop")
	release, err := d.claims.claim(false, []string{databaseClaim(d.servers[pg.Name], "shop")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.restore(context.Background(), "c1", d.replace(pg, snapshot, "shop")); err == nil || err.Error() != "A backup of shop is running: restore when it ends." {
		t.Errorf("a restore during a dump: %v", err)
	}
	release()
	if _, err := d.restore(context.Background(), "c2", d.replace(pg, snapshot, "shop")); err != nil {
		t.Errorf("after the dump: %v", err)
	}
}

// TestTooLittleRoomRefusesARestore (#53): a restore whose dump would not fit
// on the server, staged and as a copy, is refused before a byte is copied.
func TestTooLittleRoomRefusesARestore(t *testing.T) {
	d := newDBHarness(t)
	pg := databases.Postgres
	d.shop(pg)
	snapshot, _ := d.backupDatabase(t, pg, "shop")
	d.app("shop-web-1", "shop")
	d.db.SetFree(100<<20, 100<<20)
	_, err := d.restore(context.Background(), "c1", d.replace(pg, snapshot, "shop", "shop-web-1"))
	if err == nil || !strings.HasPrefix(err.Error(), "Nothing changed in shop: pcdb-main-db-1 has 100.0 MiB free for its data and 100.0 MiB in /tmp, and the restore needs about") {
		t.Errorf("too little room: %v", err)
	}
	if slices.ContainsFunc(d.engine.Calls(), func(call string) bool { return strings.Contains(call, "/archive") }) || d.status("shop-web-1") != "running" {
		t.Error("something was copied or stopped")
	}
}

// TestARestoresPasswordsAreMaskedButNotItsDatabase (#53): what the client
// says of a failed restore loses the administrator's and the user's
// passwords, and keeps the database's name, which is no secret.
func TestARestoresPasswordsAreMaskedButNotItsDatabase(t *testing.T) {
	d := newDBHarness(t)
	pg := databases.Postgres
	d.shop(pg)
	snapshot, _ := d.backupDatabase(t, pg, "shop")
	d.setInject(func(call *fakedocker.ExecCall) (fakedocker.ExecResult, bool) {
		if strings.Contains(strings.Join(call.Cmd, " "), "pg_restore --no-password") {
			return fakedocker.ExecResult{Code: 1, Stderr: "pg_restore: error: shop failed: PGPASSWORD=" + shopPassword + " admin " + adminPassword + "\n"}, true
		}
		return fakedocker.ExecResult{}, false
	})
	result, err := d.restore(context.Background(), "c1", d.replace(pg, snapshot, "shop"))
	if err == nil || !strings.Contains(err.Error(), "pg_restore: error: shop failed: PGPASSWORD=*** admin ***") {
		t.Errorf("the error: %v", err)
	}
	if log, _ := result["log"].(string); strings.Contains(log, shopPassword) || strings.Contains(log, adminPassword) || !strings.Contains(log, "into "+databases.ScratchName(testRestoreID)+" as shop") {
		t.Errorf("the log:\n%s", log)
	}
}

// TestADatabaseRestoreIsChecked (#53): what a database restore asks is
// checked before the server is looked at.
func TestADatabaseRestoreIsChecked(t *testing.T) {
	d := newDBHarness(t)
	snapshot := strings.Repeat("a", 64)
	for name, tc := range map[string]struct {
		change func(*agentv1.BackupRestore)
		want   string
	}{
		"both":          {func(p *agentv1.BackupRestore) { p.TargetPath = "/srv/x" }, "exactly one of them"},
		"picked":        {func(p *agentv1.BackupRestore) { p.Include = []string{"/shop.pgdump"} }, "picks no paths"},
		"not overwrite": {func(p *agentv1.BackupRestore) { p.Overwrite = false }, "that was not confirmed"},
		"mode":          {func(p *agentv1.BackupRestore) { p.Database.Mode = "test" }, `not "test"`},
		"file":          {func(p *agentv1.BackupRestore) { p.Database.File = "shop.sql" }, "is not the dump of a PostgreSQL database"},
		"user":          {func(p *agentv1.BackupRestore) { p.Database.User = "Shop;" }, "not a valid database name"},
		"temporary":     {func(p *agentv1.BackupRestore) { p.Database.Database = "pc_old_aaaaaaaaaaaaaaa" }, "one of PocketCloud's temporary databases"},
		"scratch":       {func(p *agentv1.BackupRestore) { p.Database.Scratch = "pc_rs_bbbbbbbbbbbbbbb" }, "the scratch copy of a restore is pc_rs_"},
		"previous":      {func(p *agentv1.BackupRestore) { p.Database.Previous = "" }, "the copy of the data before a restore"},
		"engine":        {func(p *agentv1.BackupRestore) { p.Database.Connection.Engine = "sqlite" }, "unknown database engine"},
		"site":          {func(p *agentv1.BackupRestore) { p.Database.StopContainers = []string{"pocketcloud-edge"} }, "is not a site's container"},
		"project":       {func(p *agentv1.BackupRestore) { p.Database.StopProjects = []string{"../x"} }, "is not a stack's name"},
		"password":      {func(p *agentv1.BackupRestore) { p.Database.Password = "" }, "no database password"},
		"new stops": {func(p *agentv1.BackupRestore) {
			p.Database.Mode, p.Overwrite, p.Database.Scratch, p.Database.Previous = backup.DbRestoreNew, false, "", ""
		}, "stops nothing"},
	} {
		payload := d.replace(databases.Postgres, snapshot, "shop")
		tc.change(payload)
		if _, err := d.restore(context.Background(), "c1", payload); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	mariadb := d.replace(databases.MariaDB, snapshot, "shop")
	mariadb.Database.Previous = "pc_old_" + testRestoreID
	if _, err := d.restore(context.Background(), "c1", mariadb); err == nil || !strings.Contains(err.Error(), "the copy of the data before a restore is \"\"") {
		t.Errorf("a previous copy on MariaDB: %v", err)
	}
	if len(d.db.Execs()) != 0 {
		t.Errorf("the server was asked: %+v", d.db.Execs())
	}
}

// TestAnInterruptedDatabaseRestoreFinishesByItsPhase (#53): an agent that
// restarted reads where a database restore in place was before it starts
// anything: before the stop, nothing changed and nothing starts; stopped,
// the application starts again and nothing changed; after the swap or once
// MariaDB's database holds the dump, it starts again and the restore
// succeeded; with MariaDB's database emptied, the application stays
// stopped with its restart policies back.
func TestAnInterruptedDatabaseRestoreFinishesByItsPhase(t *testing.T) {
	d := newDBHarness(t)
	for _, tc := range []struct {
		phase, engine  string
		state, message string
		running        bool
	}{
		{backup.PhaseScratch, databases.Postgres.Name, stateFailed, "Nothing changed in shop: the agent restarted during the restore.", false},
		{backup.PhaseRehearsal, databases.MariaDB.Name, stateFailed, "Nothing changed in shop: the agent restarted during the restore.", false},
		{backup.PhaseStopped, databases.Postgres.Name, stateFailed, "Nothing changed in shop: the agent restarted during the restore. It started the application again (1 of 1 containers).", true},
		{backup.PhaseSwapped, databases.Postgres.Name, stateSucceeded, "", true},
		{backup.PhaseRestored, databases.MariaDB.Name, stateSucceeded, "", true},
		{backup.PhaseEmptied, databases.MariaDB.Name, stateFailed, "The agent restarted during the restore. The restore failed after shop was emptied: it holds part of the backup, and app-" +
			backup.PhaseEmptied + " stay stopped. Restore again, then start them on their pages.", false},
	} {
		name := "app-" + tc.phase
		id := d.container(name, nil, "no", 0)
		var containers []markedContainer
		if tc.phase != backup.PhaseScratch && tc.phase != backup.PhaseRehearsal {
			d.engine.SetStatus(id, "exited")
			containers = []markedContainer{{ID: id, Name: name, RestartPolicy: restartPolicy{Name: "unless-stopped"}}}
		}
		command := "c-" + tc.phase
		marker := restoreMarker{CommandID: command, RestoreID: testRestoreID, Containers: containers, Database: &databaseMarker{
			Container: d.servers[tc.engine], Engine: tc.engine, Name: "shop", Scratch: databases.ScratchName(testRestoreID), Phase: tc.phase}}
		if tc.engine == databases.Postgres.Name {
			marker.Database.Previous = databases.PreviousName(testRestoreID)
		}
		raw, _ := json.Marshal(marker)
		if err := os.MkdirAll(filepath.Join(d.Config.StateDir, restoreMarkers), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(d.markerPath(command), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		d.finishInterruptedRestores(context.Background())
		entry, ok := d.store.get(command)
		if !ok || entry.State != tc.state || entry.Error != tc.message || entry.Result["interrupted"] != true {
			t.Errorf("%s: %+v", tc.phase, entry)
		}
		if running := d.status(name) == "running"; running != (tc.running || tc.phase == backup.PhaseScratch || tc.phase == backup.PhaseRehearsal) {
			t.Errorf("%s: %s is %s", tc.phase, name, d.status(name))
		}
		if len(containers) > 0 && d.policy(t, name).Name != "unless-stopped" {
			t.Errorf("%s: the restart policy %v", tc.phase, d.policy(t, name))
		}
		if tc.phase == backup.PhaseEmptied && (entry.Result["partial"] != true || !slices.Equal(anyStrings(entry.Result["app_left_stopped"]), []string{name})) {
			t.Errorf("%s: %v", tc.phase, entry.Result)
		}
		if tc.phase == backup.PhaseSwapped && entry.Result["previous"] != databases.PreviousName(testRestoreID) {
			t.Errorf("%s: %v", tc.phase, entry.Result)
		}
		if _, err := os.Stat(d.markerPath(command)); !os.IsNotExist(err) {
			t.Errorf("%s: the marker is left", tc.phase)
		}
	}
}

// TestAnAgentThatRestartedDuringTheSwapAsksTheServer (#53, A1): the swap's
// process runs on in the server's container when the agent restarts, and
// may commit: once it can have ended, the server says whether the copy took
// the database's place. Done, the restore succeeded, and the data before it
// stays, closed; not done, nothing changed; when the server does not say,
// the restore says neither, and keeps the data before it. What stopped
// starts again in each case.
func TestAnAgentThatRestartedDuringTheSwapAsksTheServer(t *testing.T) {
	d := newDBHarness(t)
	pg := databases.Postgres
	server := d.servers[pg.Name]
	started := time.Now().UTC().Add(-time.Second).Truncate(time.Second)
	for _, tc := range []struct {
		name, id string
		// on is what the server holds of shop's restore when the agent
		// starts; inFlight commits the swap while the agent waits
		on       []string
		inFlight bool
		silent   bool
		state    string
		message  string
	}{
		{name: "done", id: "ddddddddddddddd", on: []string{"shop", "pc_old_"}, state: stateSucceeded},
		{name: "in flight", id: "fffffffffffffff", on: []string{"shop", "pc_rs_"}, inFlight: true, state: stateSucceeded},
		{name: "not done", id: "nnnnnnnnnnnnnnn", on: []string{"shop", "pc_rs_"}, state: stateFailed,
			message: "Nothing changed in shop: the agent restarted during the restore. It started the application again (1 of 1 containers)."},
		{name: "silent", id: "sssssssssssssss", on: []string{"shop", "pc_rs_"}, silent: true, state: stateFailed,
			message: "The restore of shop may have gone through: the agent restarted while it put the copy in place, and the server did not say whether the copy took its place " +
				"(the database server's client failed with exit code 2: psql: error: the database system is starting up). If it did, the data before it stays a day as pc_old_%s. " +
				"It started the application again (1 of 1 containers)."},
	} {
		id := tc.id
		scratch, previous := databases.ScratchName(id), databases.PreviousName(id)
		for _, name := range tc.on {
			switch name {
			case "shop":
				d.db.AddDatabase(server, "shop", "shop", shopPassword, "items")
			case "pc_old_":
				d.db.AddDatabase(server, previous, "shop", shopPassword, "items")
			case "pc_rs_":
				d.db.AddDatabase(server, scratch, "shop", shopPassword, "items", "orders")
			}
		}
		waited := settleSwapsWith(t, func(time.Time) {
			if tc.inFlight {
				swap, _ := databases.SwapSQL("shop", scratch, previous)
				script := databases.Statements(pg, pg.AdminUser, "", swap)
				d.db.Answer(&fakedocker.ExecCall{Container: server, Cmd: script.Cmd, Env: script.Env}, nil)
			}
		})
		d.setInject(func(call *fakedocker.ExecCall) (fakedocker.ExecResult, bool) {
			if tc.silent {
				return fakedocker.ExecResult{Code: 2, Stderr: "psql: error: the database system is starting up\n"}, true
			}
			return fakedocker.ExecResult{}, false
		})
		name := "app-" + strings.ReplaceAll(tc.name, " ", "-")
		app := d.container(name, nil, "no", 0)
		d.engine.SetStatus(app, "exited")
		command := "c-" + id
		marker := restoreMarker{CommandID: command, RestoreID: id,
			Containers: []markedContainer{{ID: app, Name: name, RestartPolicy: restartPolicy{Name: "unless-stopped"}}},
			Database: &databaseMarker{Container: server, Engine: pg.Name, Name: "shop", Scratch: scratch, Previous: previous,
				Phase: backup.PhaseSwapping, AdminUser: pg.AdminUser, SwapStarted: started}}
		raw, _ := json.Marshal(marker)
		if err := os.MkdirAll(filepath.Join(d.Config.StateDir, restoreMarkers), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(d.markerPath(command), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		d.finishInterruptedRestores(context.Background())
		d.setInject(nil)

		entry, _ := d.store.get(command)
		message := tc.message
		if strings.Contains(message, "%s") {
			message = fmt.Sprintf(message, id)
		}
		if entry.State != tc.state || entry.Error != message || entry.Result["interrupted"] != true {
			t.Errorf("%s: %s %q\nwant %s %q", tc.name, entry.State, entry.Error, tc.state, message)
		}
		if keeps := tc.state == stateSucceeded || tc.silent; keeps != (entry.Result["previous"] == previous) {
			t.Errorf("%s: previous %v", tc.name, entry.Result["previous"])
		}
		if len(*waited) != 1 || !(*waited)[0].Equal(started) {
			t.Errorf("%s: waited for the swap begun at %v, not %v", tc.name, *waited, started)
		}
		if d.status(name) != "running" || d.policy(t, name).Name != "unless-stopped" {
			t.Errorf("%s: the application is %s, %v", tc.name, d.status(name), d.policy(t, name))
		}
		before, there := d.db.Database(server, previous)
		if tc.state == stateSucceeded && (!there || !before.Closed) {
			t.Errorf("%s: the data before the restore: %+v %v", tc.name, before, there)
		}
		if _, err := os.Stat(d.markerPath(command)); !os.IsNotExist(err) {
			t.Errorf("%s: the marker is left", tc.name)
		}
	}
}

// settleSwapsWith makes the agent's wait for a swap it lost sight of call
// settle instead, and note the time it was given.
func settleSwapsWith(t *testing.T, settle func(started time.Time)) *[]time.Time {
	var mu sync.Mutex
	waited := &[]time.Time{}
	keep := swapSettled
	swapSettled = func(_ context.Context, started time.Time) {
		mu.Lock()
		*waited = append(*waited, started)
		mu.Unlock()
		if settle != nil {
			settle(started)
		}
	}
	t.Cleanup(func() { swapSettled = keep })
	return waited
}

// isSwap says whether a process is the swap of PostgreSQL's copy.
func isSwap(call *fakedocker.ExecCall) bool {
	return slices.ContainsFunc(call.Env, func(item string) bool {
		return strings.HasPrefix(item, "PC_SQL=") && strings.Contains(item, " RENAME TO ")
	})
}

// committed is a swap's process cut after its COMMIT, as when its last
// statement failed.
func committed(call *fakedocker.ExecCall) *fakedocker.ExecCall {
	cut := &fakedocker.ExecCall{ID: call.ID, Container: call.Container, Cmd: call.Cmd}
	for _, item := range call.Env {
		if sql, ok := strings.CutPrefix(item, "PC_SQL="); ok {
			before, _, _ := strings.Cut(sql, "COMMIT;")
			item = "PC_SQL=" + before + "COMMIT;"
		}
		cut.Env = append(cut.Env, item)
	}
	return cut
}

// TestASwapThatTookPlaceDespiteAnErrorSucceeds (#53, A1): a swap whose
// process fails after its COMMIT, as when its last statement fails, put the
// copy in place: the server says so, and the restore succeeded, keeps the
// data before it, closed, and starts the application again. A process whose
// stream broke may still run: the agent waits for it before it asks. When
// the server does not say, the restore says neither that it went through
// nor that nothing changed, and keeps the data before it.
func TestASwapThatTookPlaceDespiteAnErrorSucceeds(t *testing.T) {
	d := newDBHarness(t)
	pg := databases.Postgres
	d.shop(pg)
	server := d.servers[pg.Name]
	snapshot, _ := d.backupDatabase(t, pg, "shop")
	d.app("shop-web-1", "shop")
	previous := databases.PreviousName(testRestoreID)
	waited := settleSwapsWith(t, nil)

	// the swap's last statement fails after the COMMIT; the marker said
	// it was under way, as the administrator, from when
	var during databaseMarker
	d.setInject(func(call *fakedocker.ExecCall) (fakedocker.ExecResult, bool) {
		if !isSwap(call) {
			return fakedocker.ExecResult{}, false
		}
		var marker restoreMarker
		if raw, err := os.ReadFile(d.markerPath("c1")); err == nil && json.Unmarshal(raw, &marker) == nil && marker.Database != nil {
			during = *marker.Database
		}
		d.db.Answer(committed(call), nil)
		return fakedocker.ExecResult{Code: 3, Stderr: "ERROR:  canceling statement due to lock timeout\n"}, true
	})
	result, err := d.restore(context.Background(), "c1", d.replace(pg, snapshot, "shop", "shop-web-1"))
	if err != nil || result["previous"] != previous || result["files_restored"] != float64(1) {
		t.Fatalf("a swap whose last statement failed: %v %v", err, result)
	}
	shop, _ := d.db.Database(server, "shop")
	before, there := d.db.Database(server, previous)
	if shop.From != "shop" || !there || !before.Closed || slices.Contains(d.db.DatabaseNames(server), databases.ScratchName(testRestoreID)) {
		t.Errorf("after: shop %+v, previous %+v, databases %q", shop, before, d.db.DatabaseNames(server))
	}
	if d.status("shop-web-1") != "running" || len(*waited) != 0 {
		t.Errorf("the application is %s; waited for the swap %v", d.status("shop-web-1"), *waited)
	}
	if during.Phase != backup.PhaseSwapping || during.AdminUser != pg.AdminUser || time.Since(during.SwapStarted) > time.Minute {
		t.Errorf("the marker during the swap: %+v", during)
	}
	if log, _ := result["log"].(string); !strings.Contains(log, "Putting the copy in place failed after it took place") {
		t.Errorf("the log:\n%s", log)
	}

	// the swap's stream breaks while its process runs on, and commits
	d.db.AddDatabase(server, "shop", "shop", shopPassword, "items", "orders")
	other := func(id string) *agentv1.BackupRestore {
		payload := d.replace(pg, snapshot, "shop", "shop-web-1")
		payload.RestoreId, payload.Database.Scratch, payload.Database.Previous = id, databases.ScratchName(id), databases.PreviousName(id)
		return payload
	}
	var swap *fakedocker.ExecCall
	d.setInject(func(call *fakedocker.ExecCall) (fakedocker.ExecResult, bool) {
		if !isSwap(call) {
			return fakedocker.ExecResult{}, false
		}
		swap = call
		d.engine.Fail("/exec/"+call.ID+"/json", "connection reset by peer")
		return fakedocker.ExecResult{}, true
	})
	waited = settleSwapsWith(t, func(time.Time) {
		d.engine.Fail("/exec/"+swap.ID+"/json", "")
		d.db.Answer(swap, nil)
	})
	result, err = d.restore(context.Background(), "c2", other("sssssssssssssss"))
	if err != nil || result["previous"] != databases.PreviousName("sssssssssssssss") || len(*waited) != 1 || (*waited)[0].IsZero() {
		t.Fatalf("a swap whose stream broke: %v %v, waited %v", err, result, *waited)
	}
	if shop, _ := d.db.Database(server, "shop"); shop.From != "shop" || d.status("shop-web-1") != "running" {
		t.Errorf("after: shop %+v, the application %s", shop, d.status("shop-web-1"))
	}

	// the swap fails, and the server does not say whether it took place
	d.db.AddDatabase(server, "shop", "shop", shopPassword, "items", "orders")
	d.setInject(func(call *fakedocker.ExecCall) (fakedocker.ExecResult, bool) {
		switch {
		case isSwap(call):
			return fakedocker.ExecResult{Code: 2, Stderr: "psql: error: server closed the connection unexpectedly\n"}, true
		case slices.ContainsFunc(call.Env, func(item string) bool { return strings.Contains(item, "FROM pg_database WHERE datname") }):
			return fakedocker.ExecResult{Code: 2, Stderr: "psql: error: the database system is starting up\n"}, true
		}
		return fakedocker.ExecResult{}, false
	})
	result, err = d.restore(context.Background(), "c3", other("uuuuuuuuuuuuuuu"))
	want := "The restore of shop may have gone through: putting the copy in place failed (the server's client failed with exit code 2: " +
		"psql: error: server closed the connection unexpectedly), and the server did not say whether the copy took its place " +
		"(the database server's client failed with exit code 2: psql: error: the database system is starting up). " +
		"If it did, the data before it stays a day as pc_old_uuuuuuuuuuuuuuu."
	if err == nil || err.Error() != want || result["previous"] != databases.PreviousName("uuuuuuuuuuuuuuu") {
		t.Errorf("a swap the server says nothing of:\n%v\nwant\n%s\n%v", err, want, result["previous"])
	}
	if d.status("shop-web-1") != "running" {
		t.Errorf("the application is %s", d.status("shop-web-1"))
	}
}
func (d *dbHarness) testCommand(engine databases.Engine, snapshot, database string, dropBefore ...string) *agentv1.Command {
	return &agentv1.Command{Id: "t1", Type: "backup.restore_test", Payload: &agentv1.Command_BackupRestoreTest{BackupRestoreTest: &agentv1.BackupRestoreTest{
		Repository: d.repo, SnapshotId: snapshot, JobTag: testJobTag, CheckId: testCheckID, DropBefore: dropBefore,
		Database: &agentv1.BackupDbRestore{Connection: connection(engine), Database: databases.TestDatabaseName(testCheckID),
			File: databases.BackupFile(engine, database), Mode: backup.DbRestoreTest, User: database, Password: shopPassword},
	}}}
}

// TestARestoreTestRestoresIntoATemporaryDatabase (#53, AC2): the earlier
// tests' databases go first; the newest dump goes into pc_rt_<check id> as
// the database's user, its tables are counted against the dump's, and the
// copy is closed, and stays for the panel to drop.
func TestARestoreTestRestoresIntoATemporaryDatabase(t *testing.T) {
	for _, engine := range []databases.Engine{databases.Postgres, databases.MariaDB} {
		t.Run(engine.Name, func(t *testing.T) {
			d := newDBHarness(t)
			d.shop(engine)
			server := d.servers[engine.Name]
			earlier := "pc_rt_eeeeeeeeeeeeeee"
			d.db.AddDatabase(server, earlier, "shop", shopPassword, "items")
			snapshot, dump := d.backupDatabase(t, engine, "shop")
			result, err := d.handleBackupRestoreTest(context.Background(), d.testCommand(engine, snapshot, "shop", earlier), &Reporter{})
			if err != nil {
				t.Fatalf("%v\n%v", err, result["log"])
			}
			temporary := databases.TestDatabaseName(testCheckID)
			if result["status"] != backup.RestoreTestOK || result["failure"] != "" || result["tables"] != int64(2) && result["tables"] != float64(2) {
				t.Errorf("the answer: %v", result)
			}
			var answer backup.RestoreTestResult
			raw, _ := json.Marshal(result)
			if err := json.Unmarshal(raw, &answer); err != nil {
				t.Fatal(err)
			}
			if answer.Tables != 2 || answer.TablesExpected != 2 || answer.Bytes != int64(len(dump)) || !answer.Created || !slices.Equal(answer.Dropped, []string{earlier}) {
				t.Errorf("the answer: %+v", answer)
			}
			copied, there := d.db.Database(server, temporary)
			if !there || !slices.Equal(copied.Tables, []string{"items", "orders"}) || engine.Name == databases.Postgres.Name && !copied.Closed ||
				engine.Name == databases.MariaDB.Name && len(copied.Granted) != 0 {
				t.Errorf("the copy: %+v", copied)
			}
			if _, there := d.db.Database(server, earlier); there {
				t.Error("the earlier test's copy is left")
			}
			if names := d.db.DatabaseNames(server); !slices.Equal(names, []string{"blog", temporary, "shop"}) {
				t.Errorf("the server has %q", names)
			}
			if len(d.engine.Files(server)) != 0 {
				t.Errorf("files left: %q", d.engine.Files(server))
			}
		})
	}
}

// TestARestoreTestSaysWhyItFailed (#53, AC2): a failed test says its class,
// which tells a backup that could not be restored (damaged, incomplete,
// restore, tables) from a test that could not tell; and whether it created
// its copy, for the panel to drop; a copy that exists is never reused.
func TestARestoreTestSaysWhyItFailed(t *testing.T) {
	type setup func(d *dbHarness, server string)
	for name, tc := range map[string]struct {
		engine   databases.Engine
		database string
		before   setup
		failure  string
		created  bool
	}{
		"damaged": {databases.Postgres, "shop", func(d *dbHarness, _ string) { fakerestic.Damage(d.tt, d.root, d.repo.GetUrl()) }, backup.TestDamaged, true},
		"incomplete": {databases.MariaDB, "shop", func(d *dbHarness, server string) {
			d.db.SetDumpContent(server, "shop", "CREATE TABLE `items` (`id` int);\n")
		}, backup.TestIncomplete, true},
		"restore":   {databases.MariaDB, "fail_shop", nil, backup.TestRestore, true},
		"tables":    {databases.Postgres, "short_shop", nil, backup.TestTables, true},
		"space":     {databases.Postgres, "shop", func(d *dbHarness, _ string) { d.db.SetFree(1<<20, 1<<20) }, backup.TestSpace, true},
		"locked":    {databases.Postgres, "shop", func(d *dbHarness, _ string) { d.tt.Setenv("FAKE_RESTIC_LOCKED_DUMP", "5") }, backup.TestLocked, true},
		"container": {databases.Postgres, "shop", func(d *dbHarness, server string) { d.engine.SetStatus(server, "exited") }, backup.TestContainer, false},
		"reused": {databases.Postgres, "shop", func(d *dbHarness, server string) {
			d.db.AddDatabase(server, databases.TestDatabaseName(testCheckID), "shop", shopPassword)
		}, backup.TestContainer, false},
	} {
		t.Run(name, func(t *testing.T) {
			d := newDBHarness(t)
			d.tt = t
			server := d.servers[tc.engine.Name]
			d.db.AddDatabase(server, tc.database, tc.database, shopPassword, "items", "orders")
			snapshot, _ := d.backupDatabase(t, tc.engine, tc.database)
			if tc.before != nil {
				tc.before(d, server)
			}
			if name == "incomplete" {
				snapshot, _ = d.backupDatabase(t, tc.engine, tc.database)
			}
			command := d.testCommand(tc.engine, snapshot, tc.database)
			command.GetBackupRestoreTest().Database.Password = shopPassword
			result, err := d.handleBackupRestoreTest(context.Background(), command, &Reporter{})
			if err == nil || result["failure"] != tc.failure || result["status"] != "" || result["created"] != tc.created {
				t.Errorf("%v %v", err, result)
			}
			if backup.TestFailed(tc.failure) != slices.Contains([]string{"damaged", "incomplete", "restore", "tables"}, name) {
				t.Errorf("%s fails the test: %v", name, backup.TestFailed(tc.failure))
			}
			if name == "reused" && !strings.Contains(err.Error(), "never reuses a database") {
				t.Errorf("reused: %v", err)
			}
		})
	}

	// a snapshot the retention forgot, and a repository that does not
	// answer, say nothing of the backup
	d := newDBHarness(t)
	d.shop(databases.Postgres)
	d.backupDatabase(t, databases.Postgres, "shop")
	if result, err := d.handleBackupRestoreTest(context.Background(), d.testCommand(databases.Postgres, strings.Repeat("f", 64), "shop"), &Reporter{}); err == nil ||
		result["failure"] != backup.TestRepository {
		t.Errorf("a forgotten snapshot: %v %v", err, result["failure"])
	}
	d = newDBHarness(t)
	d.shop(databases.Postgres)
	snapshot, _ := d.backupDatabase(t, databases.Postgres, "shop")
	command := d.testCommand(databases.Postgres, snapshot, "shop")
	command.GetBackupRestoreTest().Repository = &agentv1.BackupRepository{TargetId: "t1", Url: d.repo.GetUrl(), Password: d.repo.GetPassword(),
		Env: map[string]string{"AWS_ACCESS_KEY_ID": "minio-key", "AWS_SECRET_ACCESS_KEY": "wrong"}}
	if result, err := d.handleBackupRestoreTest(context.Background(), command, &Reporter{}); err == nil || result["failure"] != backup.TestUnreachable ||
		strings.Contains(err.Error(), "wrong") {
		t.Errorf("a repository that does not answer: %v %v", err, result["failure"])
	}

	// a password that does not sign in
	d = newDBHarness(t)
	d.shop(databases.MariaDB)
	snapshot, _ = d.backupDatabase(t, databases.MariaDB, "shop")
	command = d.testCommand(databases.MariaDB, snapshot, "shop")
	command.GetBackupRestoreTest().Database.Password = "stale-pw"
	if result, err := d.handleBackupRestoreTest(context.Background(), command, &Reporter{}); err == nil || result["failure"] != backup.TestSignIn || result["created"] != true {
		t.Errorf("a stale password: %v %v", err, result)
	}
}

// TestARestoreTestIsChecked (#53): what a restore test asks is checked
// before the server is looked at: its copy is its check's pc_rt_, and it
// drops earlier tests' copies only.
func TestARestoreTestIsChecked(t *testing.T) {
	d := newDBHarness(t)
	snapshot := strings.Repeat("a", 64)
	for name, tc := range map[string]struct {
		change func(*agentv1.BackupRestoreTest)
		want   string
	}{
		"mode":        {func(p *agentv1.BackupRestoreTest) { p.Database.Mode = backup.DbRestoreReplace }, "mode \"test\""},
		"other check": {func(p *agentv1.BackupRestoreTest) { p.Database.Database = "pc_rt_ddddddddddddddd" }, "restores into pc_rt_ and its check's id"},
		"database":    {func(p *agentv1.BackupRestoreTest) { p.Database.Database = "shop" }, "restores into pc_rt_ and its check's id"},
		"check":       {func(p *agentv1.BackupRestoreTest) { p.CheckId = "short" }, "names the panel's record"},
		"drop shop":   {func(p *agentv1.BackupRestoreTest) { p.DropBefore = []string{"shop"} }, "drops earlier tests' databases only"},
		"drop old":    {func(p *agentv1.BackupRestoreTest) { p.DropBefore = []string{"pc_old_aaaaaaaaaaaaaaa"} }, "drops earlier tests' databases only"},
		"drop itself": {func(p *agentv1.BackupRestoreTest) { p.DropBefore = []string{databases.TestDatabaseName(testCheckID)} }, "drops earlier tests' databases only"},
		"stops":       {func(p *agentv1.BackupRestoreTest) { p.Database.StopProjects = []string{"shop"} }, "stops nothing"},
		"password":    {func(p *agentv1.BackupRestoreTest) { p.Database.Password = "" }, "no database password"},
		"tag":         {func(p *agentv1.BackupRestoreTest) { p.JobTag = "pocketcloud" }, "names its job's tag"},
	} {
		command := d.testCommand(databases.Postgres, snapshot, "shop")
		tc.change(command.GetBackupRestoreTest())
		if _, err := d.handleBackupRestoreTest(context.Background(), command, &Reporter{}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	if len(d.db.Execs()) != 0 {
		t.Errorf("the server was asked: %+v", d.db.Execs())
	}
}

// TestRestoreTestsRunOneAtATime (#53): a restore test has a slot of its
// own, which the longest prefix of its type, backup.restore, would not give
// it.
func TestRestoreTestsRunOneAtATime(t *testing.T) {
	if perTypeLimits["backup.restore_test"] != 1 {
		t.Errorf("the restore tests' slots: %v", perTypeLimits)
	}
}

// TestADumpIsStagedFromItsJobsSnapshotOnly (#53): what is staged is the one
// file of a database job's snapshot, named as the restore names it: a
// snapshot of two files, one whose file is another database's, or one of
// another job, is refused before anything changes, and nothing is left in
// the server's container.
func TestADumpIsStagedFromItsJobsSnapshotOnly(t *testing.T) {
	d := newDBHarness(t)
	pg := databases.Postgres
	d.shop(pg)
	shopSnapshot, _ := d.backupDatabase(t, pg, "shop")
	d.volume(t, "two_files", map[string]string{"shop.pgdump": "PGDMP", "zz-other": "x"})
	volumes := d.backup(t, "two_files")
	for name, tc := range map[string]struct {
		snapshot, database, want string
	}{
		"two files":      {volumes, "shop", "the snapshot holds more than one file: it is no database's dump"},
		"another's dump": {shopSnapshot, "blog", "the snapshot holds shop.pgdump, not the dump blog.pgdump"},
		"another job's":  {shopSnapshot, "shop", "is not one of this job's"},
	} {
		payload := d.replace(pg, tc.snapshot, tc.database)
		if name == "another job's" {
			payload.JobTag = "job:bbbbbbbbbbbbbbb"
		}
		if _, err := d.restore(context.Background(), "c1", payload); err == nil || !strings.HasPrefix(err.Error(), "Nothing changed in "+tc.database+": ") ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if files := d.engine.Files(d.servers[pg.Name]); len(files) != 0 {
		t.Errorf("left in the server: %q", files)
	}
	if shop, _ := d.db.Database(d.servers[pg.Name], "shop"); !slices.Equal(shop.Tables, []string{"items", "orders"}) || shop.From != "" ||
		len(d.db.DatabaseNames(d.servers[pg.Name])) != 2 {
		t.Errorf("shop changed: %+v", shop)
	}
	// a preview of another job's snapshot
	payload := d.replace(pg, shopSnapshot, "shop")
	payload.DryRun, payload.Overwrite, payload.JobTag = true, false, "job:bbbbbbbbbbbbbbb"
	if _, err := d.restore(context.Background(), "p1", payload); err == nil || err.Error() != "the snapshot "+shopSnapshot[:8]+" is not one of this job's" {
		t.Errorf("a preview of another job's snapshot: %v", err)
	}
}

// TestOnlyASitesContainersStopByName (#53, AC1): a container a database
// restore names stops only when it is a site's <site>-php or <site>-worker,
// even one of PocketCloud's role site; a named container of another role,
// such as a database server's, never stops, nor one with another name.
func TestOnlyASitesContainersStopByName(t *testing.T) {
	d := newDBHarness(t)
	d.container("wp-php", map[string]string{"pocketcloud.role": "site"}, "unless-stopped", 0)
	d.container("wp-worker", nil, "unless-stopped", 0)
	d.container("adminer", nil, "unless-stopped", 0)
	d.container("evil-php", map[string]string{"pocketcloud.role": "db"}, "always", 0)
	users, err := d.affected(context.Background(), "c1", nil, nil, []string{"wp-php", "wp-worker", "adminer", "evil-php"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, user := range users {
		names = append(names, user.name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"wp-php", "wp-worker"}) {
		t.Errorf("would stop %q", names)
	}
}

// TestAContainerThatStartsDuringARestoreRefusesTheStop (#53): what stops is
// read again, live, before the stop: a container of the stack that started
// while the copy was restored, which the preview did not show, refuses the
// restore, which drops its copy, stops nothing and says nothing changed.
func TestAContainerThatStartsDuringARestoreRefusesTheStop(t *testing.T) {
	d := newDBHarness(t)
	pg := databases.Postgres
	d.shop(pg)
	snapshot, _ := d.backupDatabase(t, pg, "shop")
	d.app("shop-web-1", "shop")
	var once sync.Once
	d.setInject(func(call *fakedocker.ExecCall) (fakedocker.ExecResult, bool) {
		if strings.Contains(strings.Join(call.Cmd, " "), "pg_restore --no-password") {
			once.Do(func() { d.app("shop-late-1", "shop") })
		}
		return fakedocker.ExecResult{}, false
	})
	_, err := d.restore(context.Background(), "c1", d.replace(pg, snapshot, "shop", "shop-web-1"))
	if err == nil || err.Error() != "Nothing changed in shop: shop-late-1 started using the destination after the preview: preview again." {
		t.Errorf("a container that started meanwhile: %v", err)
	}
	if d.status("shop-web-1") != "running" || d.status("shop-late-1") != "running" ||
		slices.ContainsFunc(d.happened(), func(e string) bool { return strings.HasPrefix(e, "stop:") }) {
		t.Errorf("something stopped: %q", d.happened())
	}
	if names := d.db.DatabaseNames(d.servers[pg.Name]); !slices.Equal(names, []string{"blog", "shop"}) {
		t.Errorf("the server has %q", names)
	}
	if d.restoring.holds("shop-web-1") || d.restoring.holds("shop-late-1") {
		t.Error("the restore still holds a container")
	}
}

// TestADatabaseRestoreIsStoppedUntilTheSwapOnly (#53): a stop while the
// copy is restored ends the restore, drops the copy and changes nothing,
// and the application never stopped; once the application stopped for the
// swap, the restore finishes, as a stop then would leave it stopped for
// nothing.
func TestADatabaseRestoreIsStoppedUntilTheSwapOnly(t *testing.T) {
	d := newDBHarness(t)
	pg := databases.Postgres
	d.shop(pg)
	snapshot, _ := d.backupDatabase(t, pg, "shop")
	d.app("shop-web-1", "shop")
	stopAt := func(match func(call *fakedocker.ExecCall) bool) context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		d.setInject(func(call *fakedocker.ExecCall) (fakedocker.ExecResult, bool) {
			if match(call) {
				cancel()
			}
			return fakedocker.ExecResult{}, false
		})
		return ctx
	}
	ctx := stopAt(func(call *fakedocker.ExecCall) bool {
		return strings.Contains(strings.Join(call.Cmd, " "), "pg_restore --no-password")
	})
	if _, err := d.restore(ctx, "c1", d.replace(pg, snapshot, "shop", "shop-web-1")); err == nil || err.Error() != "Nothing changed in shop: the restore was stopped." {
		t.Errorf("a stop during the copy: %v", err)
	}
	if slices.ContainsFunc(d.happened(), func(e string) bool { return strings.HasPrefix(e, "stop:") }) ||
		!slices.Equal(d.db.DatabaseNames(d.servers[pg.Name]), []string{"blog", "shop"}) {
		t.Errorf("after a stop during the copy: %q, %q", d.happened(), d.db.DatabaseNames(d.servers[pg.Name]))
	}

	// stopped as the application stops
	d.setInject(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	noted := d.engine.OnStop
	d.engine.OnStop = func(c *fakedocker.Container) {
		noted(c)
		if c.Name == "shop-web-1" {
			cancel()
		}
	}
	result, err := d.restore(ctx, "c2", d.replace(pg, snapshot, "shop", "shop-web-1"))
	if err != nil || result["previous"] != databases.PreviousName(testRestoreID) {
		t.Errorf("a stop during the swap: %v %v", err, result)
	}
	if log, _ := result["log"].(string); !strings.Contains(log, "The restore is putting the copy in place and finishes.") {
		t.Errorf("the log:\n%s", log)
	}
	if shop, _ := d.db.Database(d.servers[pg.Name], "shop"); shop.From != "shop" || d.status("shop-web-1") != "running" {
		t.Errorf("after a stop during the swap: %+v, %s", shop, d.status("shop-web-1"))
	}
}
