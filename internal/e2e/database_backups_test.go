package e2e

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakedb"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

// Per-database backups and restore tests (#53) end to end: shared database
// servers made through the panel's routes, whose containers' processes
// fakedb answers as the servers' own clients would, with the privileges of
// the user a process signs in as; a real agent backs their databases up
// through the relay into the fake restic, and restores them, as each
// database's own user, into the database, a scratch copy of it or a new
// one.

// dbBackupsHarness is the restores harness whose database servers are
// fakedb's, with the containers that stopped and started, in order.
type dbBackupsHarness struct {
	*restoresHarness
	db *fakedb.Server

	lifeMu sync.Mutex
	life   []string

	// held, when set, keeps the restores of dumps in a server's container
	// waiting until it is closed; reached hears of each that waits
	held    atomic.Pointer[chan struct{}]
	reached chan string
}

// newDBBackupsHarness starts the panel and an agent whose engine's database
// servers are fakedb's. Restore tests, and the jobs the panel makes, never
// come due by themselves: the tests start them, or pass the clock.
func newDBBackupsHarness(t *testing.T, prepare ...func(engine *fakedocker.Engine)) *dbBackupsHarness {
	t.Helper()
	d := &dbBackupsHarness{reached: make(chan string, 16)}
	d.restoresHarness = newRestoresHarness(t, func(engine *fakedocker.Engine) {
		d.db = fakedb.New(engine)
		engine.OnExec = func(call *fakedocker.ExecCall, stdin []byte) fakedocker.ExecResult {
			script := strings.Join(call.Cmd, " ")
			if held := d.held.Load(); held != nil && (strings.Contains(script, "pg_restore --no-password") || strings.Contains(script, "--one-database")) {
				select {
				case d.reached <- envOf(call, "PC_DATABASE"):
				default:
				}
				<-*held
			}
			return d.db.Answer(call, stdin)
		}
		stop, start := engine.OnStop, engine.OnStart
		engine.OnStop = func(c *fakedocker.Container) {
			d.noteLife("stop:" + c.Name)
			stop(c)
		}
		engine.OnStart = func(c *fakedocker.Container) (string, string) {
			d.noteLife("start:" + c.Name)
			return start(c)
		}
		for _, change := range prepare {
			change(engine)
		}
	})
	settings, err := d.app.FindFirstRecordByFilter(schema.Settings, "")
	if err != nil {
		t.Fatal(err)
	}
	settings.Set("restore_test_schedule", neverDue)
	if err := d.app.Save(settings); err != nil {
		t.Fatal(err)
	}
	d.defaults(map[string]any{"schedule": neverDue, "targets": []string{d.disk.Id}})
	return d
}

// envOf is a variable of a process.
func envOf(call *fakedocker.ExecCall, name string) string {
	for _, item := range call.Env {
		if key, value, _ := strings.Cut(item, "="); key == name {
			return value
		}
	}
	return ""
}

// holdRestores keeps the restores of dumps waiting until release is called,
// and says the database of the first that waits.
func (d *dbBackupsHarness) holdRestores() (release func(), first func() string) {
	held := make(chan struct{})
	d.held.Store(&held)
	var once sync.Once
	release = func() {
		once.Do(func() {
			d.held.Store(nil)
			close(held)
		})
	}
	d.t.Cleanup(release)
	first = func() string {
		d.t.Helper()
		select {
		case name := <-d.reached:
			return name
		case <-time.After(waitLimit):
			d.t.Fatal("no restore came to wait")
			return ""
		}
	}
	return release, first
}

// noteLife notes a container that stopped or started; the engine calls it
// locked, and it locks nothing of the engine.
func (d *dbBackupsHarness) noteLife(event string) {
	d.lifeMu.Lock()
	defer d.lifeMu.Unlock()
	d.life = append(d.life, event)
}

// lifeSince are the stops and starts since a count of them.
func (d *dbBackupsHarness) lifeSince(from int) []string {
	d.lifeMu.Lock()
	defer d.lifeMu.Unlock()
	return slices.Clone(d.life[from:])
}

// lives counts the stops and starts so far.
func (d *dbBackupsHarness) lives() int {
	d.lifeMu.Lock()
	defer d.lifeMu.Unlock()
	return len(d.life)
}

// defaults sets the automatic backups of the agent's server as the admin.
func (d *dbBackupsHarness) defaults(body map[string]any) map[string]any {
	d.t.Helper()
	status, out := d.send(http.MethodPut, "/api/pc/hosts/"+d.host.Id+"/auto-backups", d.admin, body)
	if status != http.StatusOK {
		d.t.Fatalf("the server's backup settings: %d %v", status, out)
	}
	return out
}

// sharedServer makes a shared database server through the panel and waits
// until it is ready.
func (d *dbBackupsHarness) sharedServer(name, engine string) *core.Record {
	d.t.Helper()
	status, out := d.send(http.MethodPost, "/api/pc/db-servers", d.operator, map[string]any{"host": d.host.Id, "name": name, "engine": engine})
	if status != http.StatusOK {
		d.t.Fatalf("the server %s: %d %v", name, status, out)
	}
	id := out["server"].(map[string]any)["id"].(string)
	d.waitForStatus(out["command"].(map[string]any)["id"].(string), schema.CommandSucceeded)
	var server *core.Record
	d.waitFor("the server "+name+" to be ready", func() bool {
		found, err := d.app.FindRecordById(schema.DBServers, id)
		server = found
		return err == nil && found.GetString("status") == "ready"
	})
	return server
}

// application makes a stack with one service and deploys it, as an
// operator.
func (d *dbBackupsHarness) application(name string) *core.Record {
	d.t.Helper()
	status, out := d.send(http.MethodPost, "/api/collections/stacks/records", d.operator, map[string]any{
		"host": d.host.Id, "name": name, "compose_yaml": "services:\n  web:\n    image: nginx:1\n"})
	if status != http.StatusOK {
		d.t.Fatalf("the stack %s: %d %v", name, status, out)
	}
	id := out["id"].(string)
	status, out = d.send(http.MethodPost, "/api/pc/stacks/"+id+"/deploy", d.operator, map[string]any{})
	if status != http.StatusOK {
		d.t.Fatalf("the deploy of %s: %d %v", name, status, out)
	}
	d.waitForStatus(out["command"].(map[string]any)["id"].(string), schema.CommandSucceeded)
	return d.fresh(schema.Stacks, id)
}

// fresh reads a record again.
func (d *dbBackupsHarness) fresh(collection, id string) *core.Record {
	d.t.Helper()
	record, err := d.app.FindRecordById(collection, id)
	if err != nil {
		d.t.Fatalf("the record %s of %s: %v", id, collection, err)
	}
	return record
}

// database makes a database of a server through the panel, for a stack when
// one is given, waits until it is ready (and its stack deployed with it),
// and gives it tables.
func (d *dbBackupsHarness) database(server *core.Record, name string, stack *core.Record, tables ...string) *core.Record {
	d.t.Helper()
	body := map[string]any{"server": server.Id, "name": name}
	if stack != nil {
		body["stack"] = stack.Id
	}
	status, out := d.send(http.MethodPost, "/api/pc/databases", d.operator, body)
	if status != http.StatusOK {
		d.t.Fatalf("the database %s: %d %v", name, status, out)
	}
	id := out["database"].(map[string]any)["id"].(string)
	d.waitForStatus(out["command"].(map[string]any)["id"].(string), schema.CommandSucceeded)
	d.waitFor("the database "+name+" to be ready", func() bool {
		found, err := d.app.FindRecordById(schema.Databases, id)
		return err == nil && found.GetString("status") == "ready"
	})
	if stack != nil {
		// the stack is connected: its network attached and its new revision
		// deployed
		revision := stack.GetInt("current_revision")
		d.waitFor("the stack "+stack.GetString("name")+" to be connected", func() bool {
			fresh := d.fresh(schema.Stacks, stack.Id)
			return fresh.GetInt("current_revision") > revision && fresh.GetInt("deployed_revision") == fresh.GetInt("current_revision")
		})
	}
	d.settled()
	d.db.SetTables(server.GetString("container_ref"), name, tables...)
	return d.fresh(schema.Databases, id)
}

// settled waits until no command is open, so that what a test does next
// meets no deploy or attach still under way.
func (d *dbBackupsHarness) settled() {
	d.t.Helper()
	d.waitFor("the commands to end", func() bool {
		open, err := d.app.FindRecordsByFilter(schema.Commands,
			"status!='succeeded' && status!='failed' && status!='cancelled' && status!='timed_out'", "", 0, 0)
		return err == nil && len(open) == 0
	})
}

// jobOf waits for the job the panel makes of a database, or of a site.
func (d *dbBackupsHarness) jobOf(field, id string) *core.Record {
	d.t.Helper()
	var job *core.Record
	d.waitFor("the job of "+id, func() bool {
		found, err := d.app.FindFirstRecordByData(schema.BackupJobs, field, id)
		job = found
		return err == nil
	})
	return job
}

// backedUp runs a job now and says its run, which succeeded.
func (d *dbBackupsHarness) backedUp(job *core.Record) *core.Record {
	d.t.Helper()
	runs := d.runNow(job.Id)
	if len(runs) != 1 || runs[0].GetString("status") != schema.RunSucceeded || runs[0].GetString("snapshot_id") == "" {
		d.t.Fatalf("the run of %s: %v", job.GetString("name"), runs[0].FieldsData())
	}
	return runs[0]
}

// snapshots are a job's snapshots in the local target, whose directory is
// the server's, under the agent's HostRoot.
func (d *dbBackupsHarness) snapshots(jobID string) []fakerestic.Snapshot {
	d.t.Helper()
	found := []fakerestic.Snapshot{}
	for _, snapshot := range fakerestic.Snapshots(d.t, d.restic, filepath.Join(d.hostRoot, d.diskDir)) {
		if slices.Contains(snapshot.Tags, "job:"+jobID) {
			found = append(found, snapshot)
		}
	}
	return found
}

// tables are a database's tables on a server's container, and the database
// whose dump it holds last.
func (d *dbBackupsHarness) tables(server *core.Record, name string) ([]string, string) {
	found, ok := d.db.Database(server.GetString("container_ref"), name)
	if !ok {
		return nil, "gone"
	}
	return found.Tables, found.From
}

// draft makes a restore's draft of a job as the operator and waits for its
// preview.
func (d *dbBackupsHarness) draft(job *core.Record, body map[string]any) *core.Record {
	d.t.Helper()
	all := map[string]any{"target": d.disk.Id}
	for key, value := range body {
		all[key] = value
	}
	return d.preview(job.Id, d.operator, all)
}

// startNamed starts a restore of a database in place with its name typed.
func (d *dbBackupsHarness) startNamed(restore *core.Record, name string) (int, map[string]any) {
	d.t.Helper()
	return d.send(http.MethodPost, "/api/pc/backup-restores/"+restore.Id+"/start", d.operator,
		map[string]any{"confirm_overwrite": true, "confirm_name": name})
}

// execsSince are the processes of a server's container from a count of
// every server's processes on.
func (d *dbBackupsHarness) execsSince(server *core.Record, from int) []fakedb.Exec {
	container := d.docker.Find(server.GetString("container_ref")).ID
	found := []fakedb.Exec{}
	for _, exec := range d.db.Execs()[from:] {
		if exec.Container == container || exec.Container == server.GetString("container_ref") {
			found = append(found, exec)
		}
	}
	return found
}

// TestEveryDatabaseGetsAJob (#53): each database of a shared server, of
// either engine, gets a backup job of its own, made by the panel, which
// waits for a target until its server's defaults name one; a change of the
// defaults reaches every job but what a job set for itself.
func TestEveryDatabaseGetsAJob(t *testing.T) {
	d := newDBBackupsHarness(t)
	d.defaults(map[string]any{"schedule": neverDue, "targets": []string{}})
	pg := d.sharedServer("pg", "postgres")
	maria := d.sharedServer("maria", "mariadb")
	jobs := map[string]*core.Record{}
	for _, server := range []*core.Record{pg, maria} {
		// a database's name is its server's host's own (its secret db/<name>)
		prefix := ""
		if server == maria {
			prefix = "m"
		}
		for _, name := range []string{prefix + "shop", prefix + "blog", prefix + "wiki"} {
			database := d.database(server, name, nil)
			job := d.jobOf("database", database.Id)
			engine := schema.DumpPostgres
			if server == maria {
				engine = schema.DumpMySQL
			}
			if job.GetString("name") != server.GetString("name")+"-"+name || job.GetString("origin") != schema.JobDatabase ||
				job.GetString("source_kind") != schema.SourceDatabase || job.GetString("source") != name || job.GetString("engine") != engine ||
				job.GetString("db_server_name") != server.GetString("name") || job.GetString("host") != d.host.Id ||
				job.GetString("problem") != "Choose where it goes: no target." || len(job.GetStringSlice("targets")) != 0 || !job.GetBool("enabled") {
				t.Errorf("the job of %s/%s: %v", server.GetString("name"), name, job.FieldsData())
			}
			jobs[job.GetString("name")] = job
		}
	}
	if all, err := d.app.FindRecordsByFilter(schema.BackupJobs, "origin={:origin}", "", 0, 0, dbx.Params{"origin": schema.JobDatabase}); err != nil || len(all) != 6 {
		t.Fatalf("%d automatic jobs: %v", len(all), err)
	}

	// one job has a schedule of its own
	own := "15 4 * * *"
	if status, out := d.send(http.MethodPatch, "/api/pc/backup-jobs/"+jobs["maria-mblog"].Id, d.admin, map[string]any{"schedule": own}); status != http.StatusOK {
		t.Fatalf("the job's own schedule: %d %v", status, out)
	}
	out := d.defaults(map[string]any{"schedule": "45 1 * * *", "targets": []string{d.disk.Id}})
	if out["jobs_changed"] != float64(6) {
		t.Errorf("the defaults changed %v jobs", out["jobs_changed"])
	}
	for name, job := range jobs {
		job = d.job(job.Id)
		schedule := "45 1 * * *"
		if name == "maria-mblog" {
			schedule = own
		}
		if job.GetString("schedule") != schedule || !slices.Equal(job.GetStringSlice("targets"), []string{d.disk.Id}) || job.GetString("problem") != "" {
			t.Errorf("the job %s after the defaults: %v", name, job.FieldsData())
		}
	}
	// the API makes no such job
	if status, out := d.send(http.MethodPost, "/api/pc/backup-jobs", d.admin, map[string]any{"name": "pg-other", "host": d.host.Id,
		"source_kind": "database", "source": "other", "targets": []string{d.disk.Id}, "schedule": neverDue}); status != http.StatusBadRequest ||
		out["message"] != "Database and site jobs come with their database or site." {
		t.Errorf("a database's job by hand: %d %v", status, out)
	}
}

// TestADatabaseRunDumpsOnlyItsDatabase (#53, AC1, AC3): a database's run
// dumps that one database in its shared server's container, uncompressed
// (PostgreSQL's custom format, MariaDB's one row an INSERT), with the
// server's own administrator from the container's own environment: the
// command the panel keeps holds no password and no environment, and names no
// other database. The snapshot holds the one dump, tagged with the database
// and its server, and the retention keeps the job's snapshots as one group.
func TestADatabaseRunDumpsOnlyItsDatabase(t *testing.T) {
	d := newDBBackupsHarness(t)
	for _, engine := range []string{"postgres", "mariadb"} {
		server := d.sharedServer(engine+"-main", engine)
		// a database's name is its server's host's own (its secret db/<name>)
		shopName, blogName := "shop", "blog"
		if engine == "mariadb" {
			shopName, blogName = "mshop", "mblog"
		}
		shop := d.database(server, shopName, nil, "items", "orders")
		d.database(server, blogName, nil, "posts")
		job := d.jobOf("database", shop.Id)
		if status, out := d.send(http.MethodPatch, "/api/pc/backup-jobs/"+job.Id, d.admin, map[string]any{"retention": map[string]any{"last": 2}}); status != http.StatusOK {
			t.Fatalf("the job's retention: %d %v", status, out)
		}
		from := len(d.db.Execs())
		run := d.backedUp(job)
		// restic's data_added_packed, which the agent answers as bytes_stored
		if run.GetInt("bytes_stored") <= 0 || run.GetInt("bytes_added") <= 0 {
			t.Errorf("the %s run's bytes: %v", engine, run.FieldsData())
		}

		command := d.command(run.GetString("command"))
		payload := map[string]any{}
		if err := command.UnmarshalJSONField("payload", &payload); err != nil {
			t.Fatal(err)
		}
		dump, _ := payload["stdin_command"].([]any)
		text := strings.Join(names(payload["stdin_command"]), " ")
		file := shopName + ".pgdump"
		want := "pg_dump --format=custom --compress=0"
		if engine == "mariadb" {
			file, want = shopName+".sql", "--skip-extended-insert"
		}
		env, _ := payload["env"].(map[string]any)
		if payload["stdin_container"] != server.GetString("container_ref") || payload["stdin_file_name"] != file || len(env) != 0 ||
			len(dump) != 6 || dump[4] != server.GetString("admin_user") || dump[5] != shopName || !strings.Contains(dump[2].(string), want) ||
			strings.Contains(text, blogName) || strings.Contains(text, "--all-databases") || strings.Contains(text, "--databases") {
			t.Errorf("the %s run's command: %v", engine, payload)
		}
		if raw := command.GetString("payload"); strings.Contains(raw, d.password(server)) {
			t.Errorf("the %s run's command holds the administrator's password", engine)
		}
		if tags := names(payload["extra_tags"]); !slices.Equal(tags, []string{"db:" + shopName, "dbserver:" + server.GetString("name")}) {
			t.Errorf("the %s run's extra tags: %v", engine, tags)
		}
		// the dump ran once, in the server's container, of shop only
		dumps := 0
		for _, exec := range d.execsSince(server, from) {
			if exec.Kind == fakedb.KindDump {
				dumps++
				if !slices.Equal(exec.Databases, []string{shopName}) {
					t.Errorf("the %s dump: %+v", engine, exec)
				}
			}
		}
		if dumps != 1 {
			t.Errorf("%d %s dumps", dumps, engine)
		}
		snapshots := d.snapshots(job.Id)
		if len(snapshots) != 1 || !slices.Equal(snapshots[0].Paths, []string{"/" + file}) || !slices.Contains(snapshots[0].Tags, "db:"+shopName) ||
			!slices.Contains(snapshots[0].Tags, "dbserver:"+server.GetString("name")) || !slices.Contains(snapshots[0].Tags, "job:"+job.Id) {
			t.Fatalf("the %s snapshots: %+v", engine, snapshots)
		}
		content := snapshots[0].Files["/"+file]
		if engine == "postgres" && (!strings.HasPrefix(content, "PGDMP") || !strings.Contains(content, "dbname: "+shopName)) ||
			engine == "mariadb" && (!strings.Contains(content, "Database: "+shopName) || strings.Contains(content, "CREATE DATABASE") || strings.Contains(content, "USE ")) {
			t.Errorf("the %s dump saved: %q", engine, content)
		}
		// the retention selects the job's snapshots by its own tags, as one group
		forgets := 0
		for _, line := range strings.Split(fakerestic.Calls(t, d.restic), "\n") {
			if strings.Contains(line, " forget ") && strings.Contains(line, "job:"+job.Id) {
				forgets++
				if !strings.Contains(line, "--group-by  ") && !strings.HasSuffix(line, "--group-by ") && !strings.Contains(line, "--group-by ''") ||
					strings.Contains(line, "db:"+shopName) || strings.Contains(line, "dbserver:") {
					t.Errorf("the %s forget: %q", engine, line)
				}
			}
		}
		if forgets != 1 {
			t.Errorf("%d forgets of the %s job", forgets, engine)
		}
	}
}

// password is a server's administrator's password, as the panel keeps it.
func (d *dbBackupsHarness) password(server *core.Record) string {
	d.t.Helper()
	sealed := d.fresh(schema.DBServers, server.Id).GetString("admin_password")
	plain, err := d.panel.Box.OpenString(sealed)
	if err != nil {
		d.t.Fatal(err)
	}
	return plain
}

// restoreInPlaceLeavesTheOthers restores the database a of a server of an
// engine back into itself, where the databases b and c of the stacks b and
// c are, and checks that nothing but a and its copies was touched (AC1).
func restoreInPlaceLeavesTheOthers(t *testing.T, engine string) {
	d := newDBBackupsHarness(t)
	server := d.sharedServer("main", engine)
	stacks := map[string]*core.Record{}
	for _, name := range []string{"a", "b", "c"} {
		stacks[name] = d.application(name)
		d.database(server, name, stacks[name], name+"_items", name+"_orders")
	}
	a := d.jobOf("database", d.databaseNamed(server, "a").Id)
	run := d.backedUp(a)
	// what a, b and c hold after the backup
	container := server.GetString("container_ref")
	d.db.SetTables(container, "a", "a_items", "a_orders", "a_extra")
	before := map[string][]string{}
	for _, name := range []string{"b", "c"} {
		before[name], _ = d.tables(server, name)
	}
	if d.docker.Find("a-web-1") == nil || d.docker.Find("b-web-1") == nil {
		t.Fatalf("the applications: %v", d.docker.Names())
	}

	from, lives := len(d.db.Execs()), d.lives()
	restore := d.draft(a, map[string]any{"snapshot": run.GetString("snapshot_id"), "mode": "database"})
	if restore.GetString("status") != schema.RestorePreviewed {
		t.Fatalf("the preview: %v", restore.FieldsData())
	}
	preview := previewOf(t, restore)
	if stops := names(preview["would_stop"]); !slices.Equal(stops, []string{"a-web-1"}) {
		t.Errorf("the preview stops %v", stops)
	}
	if status, out := d.send(http.MethodPost, "/api/pc/backup-restores/"+restore.Id+"/start", d.operator,
		map[string]any{"confirm_overwrite": true, "confirm_name": "b"}); status != http.StatusConflict ||
		out["message"] != "Type the database's name to confirm that its data is replaced." {
		t.Errorf("another database's name typed: %d %v", status, out)
	}
	if status, out := d.startNamed(restore, "a"); status != http.StatusOK {
		t.Fatalf("the start: %d %v", status, out)
	}
	ended := d.ended(restore.Id)
	if ended.GetString("status") != schema.RestoreSucceeded || ended.GetInt("tables") != 2 {
		t.Fatalf("the restore: %v", ended.FieldsData())
	}
	scratch, previous := databases.ScratchName(restore.Id), ""
	if engine == "postgres" {
		previous = databases.PreviousName(restore.Id)
		if ended.GetString("previous") != previous {
			t.Errorf("the data before the restore: %q", ended.GetString("previous"))
		}
	}

	// a holds the snapshot, as its own user restored it
	if tables, from := d.tables(server, "a"); !slices.Equal(tables, []string{"a_items", "a_orders"}) || from != "a" {
		t.Errorf("a after the restore: %v from %s", tables, from)
	}
	allowed := []string{"a", scratch}
	if previous != "" {
		allowed = append(allowed, previous)
	}
	restores := 0
	for _, exec := range d.execsSince(server, from) {
		for _, name := range exec.Databases {
			if !slices.Contains(allowed, name) {
				t.Errorf("a process of the restore named %s: %s %v", name, exec.Kind, exec.Cmd)
			}
		}
		if exec.Kind == fakedb.KindRestore {
			restores++
			if exec.Env["PC_USER"] != "a" || exec.Failed {
				t.Errorf("a restore ran as %q: %+v", exec.Env["PC_USER"], exec)
			}
		}
	}
	if restores == 0 {
		t.Error("nothing was restored")
	}
	// only a's application stopped and started again; the others and the
	// server were left alone
	life := d.lifeSince(lives)
	for _, event := range life {
		if !strings.HasSuffix(event, ":a-web-1") {
			t.Errorf("the restore touched another container: %s (%v)", event, life)
		}
	}
	if !slices.Equal(life, []string{"stop:a-web-1", "start:a-web-1"}) {
		t.Errorf("what stopped and started: %v", life)
	}
	for _, name := range []string{"b", "c"} {
		if tables, from := d.tables(server, name); !slices.Equal(tables, before[name]) || from != "" {
			t.Errorf("%s after the restore of a: %v from %q", name, tables, from)
		}
	}
	if status := d.docker.Find(container).Status; status != "running" {
		t.Errorf("the server's container is %s", status)
	}
	if entry, err := d.app.FindFirstRecordByFilter(schema.AuditLog, "action='backup_restore'"); err != nil ||
		entry.GetString("actor_label") != "operator@example.com" || !strings.Contains(entry.GetString("snapshot"), `"database":"a"`) {
		t.Errorf("the restore's audit: %v %v", entry, err)
	}
}

// databaseNamed is a server's database by its name.
func (d *dbBackupsHarness) databaseNamed(server *core.Record, name string) *core.Record {
	d.t.Helper()
	database, err := d.app.FindFirstRecordByFilter(schema.Databases, "server={:server} && name={:name}", dbx.Params{"server": server.Id, "name": name})
	if err != nil {
		d.t.Fatalf("the database %s: %v", name, err)
	}
	return database
}

// TestRestoringOneDatabaseLeavesTheOthers (#53, AC1): a restore of one
// database of a shared server back into itself stops only its own
// application, restores as its own user into it and its copies only, and
// leaves the server's other databases, their applications and the server's
// container alone; on PostgreSQL and on MariaDB.
func TestRestoringOneDatabaseLeavesTheOthers(t *testing.T) {
	for _, engine := range []string{"postgres", "mariadb"} {
		t.Run(engine, func(t *testing.T) { restoreInPlaceLeavesTheOthers(t, engine) })
	}
}

// TestRestoreCraftedDumpCannotTouchOthers (#53, AC1): a snapshot whose dump
// was crafted to write another database of the server fails as the
// database's own user has no privilege there: the rehearsal fails, nothing
// changed in the database, its application never stopped, and the other
// database is as it was. A dump that switches to the other database with
// USE restores only its own: the client, with --one-database, skips what
// follows, and the other database is as it was.
func TestRestoreCraftedDumpCannotTouchOthers(t *testing.T) {
	d := newDBBackupsHarness(t)
	server := d.sharedServer("maria", "mariadb")
	shop := d.application("shop")
	d.database(server, "shop", shop, "items", "orders")
	d.database(server, "blog", nil, "posts", "comments")
	container := server.GetString("container_ref")
	header := "-- MariaDB dump 10.19  Distrib 11.8.2-MariaDB\n--\n-- Host: localhost    Database: shop\n" +
		"CREATE TABLE `items` (`id` int);\nINSERT INTO `items` VALUES (1);\nCREATE TABLE `orders` (`id` int);\n"
	d.db.SetDumpContent(container, "shop", header+"DROP TABLE `blog`.`posts`;\n-- Dump completed\n")
	job := d.jobOf("database", d.databaseNamed(server, "shop").Id)
	run := d.backedUp(job)

	from, lives := len(d.db.Execs()), d.lives()
	restore := d.draft(job, map[string]any{"snapshot": run.GetString("snapshot_id"), "mode": "database"})
	if status, out := d.startNamed(restore, "shop"); status != http.StatusOK {
		t.Fatalf("the start: %d %v", status, out)
	}
	ended := d.ended(restore.Id)
	if ended.GetString("status") != schema.RestoreFailed || !strings.HasPrefix(ended.GetString("error"), "Nothing changed in shop: ") ||
		!strings.Contains(ended.GetString("error"), "DROP command denied to user 'shop'@") || !strings.Contains(ended.GetString("error"), "for table `blog`.`posts`") {
		t.Errorf("the restore of the crafted dump: %v", ended.FieldsData())
	}
	if tables, _ := d.tables(server, "blog"); !slices.Equal(tables, []string{"posts", "comments"}) {
		t.Errorf("blog after the crafted dump: %v", tables)
	}
	if tables, from := d.tables(server, "shop"); !slices.Equal(tables, []string{"items", "orders"}) || from != "" {
		t.Errorf("shop after the crafted dump: %v from %q", tables, from)
	}
	for _, exec := range d.execsSince(server, from) {
		if slices.Contains(exec.Databases, "blog") && (exec.Kind != fakedb.KindRestore || !exec.Failed || exec.Env["PC_USER"] != "shop") {
			t.Errorf("a process named blog: %+v", exec)
		}
	}
	if life := d.lifeSince(lives); len(life) != 0 {
		t.Errorf("the failed rehearsal stopped or started %v", life)
	}
	if alert := d.openAlerts("backup_restore")[d.host.Id+":database:"+d.databaseNamed(server, "shop").Id]; alert == nil || alert.GetString("severity") != schema.SeverityCritical {
		t.Errorf("the restore's alert: %v", alert)
	}
	if event := d.event("backup.restore_failed", "data.restore={:id}", dbx.Params{"id": restore.Id}); event.GetString("severity") != schema.SeverityCritical {
		t.Errorf("the restore's event: %v", event.FieldsData())
	}

	// a dump that switches to blog: only shop is restored
	d.db.SetDumpContent(container, "shop", header+"USE `blog`;\nDROP TABLE `posts`;\nINSERT INTO `posts` VALUES (9);\n-- Dump completed\n")
	run = d.backedUp(job)
	from, lives = len(d.db.Execs()), d.lives()
	restore = d.draft(job, map[string]any{"snapshot": run.GetString("snapshot_id"), "mode": "database"})
	if status, out := d.startNamed(restore, "shop"); status != http.StatusOK {
		t.Fatalf("the start: %d %v", status, out)
	}
	if ended := d.ended(restore.Id); ended.GetString("status") != schema.RestoreSucceeded || ended.GetInt("tables") != 2 {
		t.Errorf("the restore of the dump that uses blog: %v", ended.FieldsData())
	}
	if tables, from := d.tables(server, "shop"); !slices.Equal(tables, []string{"items", "orders"}) || from != "shop" {
		t.Errorf("shop after the dump that uses blog: %v from %q", tables, from)
	}
	if tables, from := d.tables(server, "blog"); !slices.Equal(tables, []string{"posts", "comments"}) || from != "" {
		t.Errorf("blog after the dump that uses it: %v from %q", tables, from)
	}
	for _, exec := range d.execsSince(server, from) {
		if slices.Contains(exec.Databases, "blog") {
			t.Errorf("a process named blog: %+v", exec)
		}
		if exec.Kind == fakedb.KindRestore && (exec.Env["PC_USER"] != "shop" || exec.Failed) {
			t.Errorf("a restore: %+v", exec)
		}
	}
	if life := d.lifeSince(lives); !slices.Equal(life, []string{"stop:shop-web-1", "start:shop-web-1"}) {
		t.Errorf("what stopped and started: %v", life)
	}
}

// phpSite saves a PHP site of the agent's server, as deployed, with its
// PHP-FPM and worker containers running.
func (d *dbBackupsHarness) phpSite(name string, phpSettings map[string]any) *core.Record {
	d.t.Helper()
	collection, err := d.app.FindCollectionByNameOrId(schema.Sites)
	if err != nil {
		d.t.Fatal(err)
	}
	site := core.NewRecord(collection)
	site.Load(map[string]any{"name": name, "host": d.host.Id, "type": schema.SitePHPApp, "source": "upload"})
	if err := d.app.SaveNoValidate(site); err != nil {
		d.t.Fatal(err)
	}
	// the panel gives a PHP site its settings
	var php *core.Record
	d.waitFor("the site's PHP settings", func() bool {
		php, err = d.app.FindFirstRecordByData(schema.SitePHP, "site", site.Id)
		return err == nil
	})
	php.Load(phpSettings)
	if err := d.app.SaveNoValidate(php); err != nil {
		d.t.Fatal(err)
	}
	for _, role := range []string{"php", "worker"} {
		d.docker.AddContainer(name+"-"+role, "php:8.3-fpm", map[string]string{"pocketcloud.role": "site", "pocketcloud.site": name})
	}
	return site
}

// TestSiteDatabaseRestoreStopsTheSite (#53): a restore of a PHP site's
// database back into itself stops the site's PHP-FPM and worker containers,
// which are known by their names, and no other; while it is under way the
// site is not deployed.
func TestSiteDatabaseRestoreStopsTheSite(t *testing.T) {
	d := newDBBackupsHarness(t)
	server := d.sharedServer("main", "postgres")
	site := d.phpSite("shop", nil)
	status, out := d.send(http.MethodPost, "/api/pc/databases", d.operator, map[string]any{"server": server.Id, "name": "shop", "site": site.Id})
	if status != http.StatusOK {
		t.Fatalf("the site's database: %d %v", status, out)
	}
	d.waitForStatus(out["command"].(map[string]any)["id"].(string), schema.CommandSucceeded)
	d.waitFor("the database to be ready", func() bool { return d.databaseNamed(server, "shop").GetString("status") == "ready" })
	d.db.SetTables(server.GetString("container_ref"), "shop", "wp_posts", "wp_options")
	d.database(server, "blog", d.application("blog"), "posts")
	job := d.jobOf("database", d.databaseNamed(server, "shop").Id)
	run := d.backedUp(job)

	restore := d.draft(job, map[string]any{"snapshot": run.GetString("snapshot_id"), "mode": "database"})
	if stops := names(previewOf(t, restore)["would_stop"]); !slices.Equal(stops, []string{"shop-php", "shop-worker"}) {
		t.Errorf("the preview stops %v", stops)
	}
	release, first := d.holdRestores()
	lives := d.lives()
	if status, out := d.startNamed(restore, "shop"); status != http.StatusOK {
		t.Fatalf("the start: %d %v", status, out)
	}
	if name := first(); name != databases.ScratchName(restore.Id) {
		t.Errorf("the first restore went into %s", name)
	}
	if status, out := d.send(http.MethodPost, "/api/pc/sites/"+site.Id+"/deploy", d.operator, nil); status != http.StatusConflict ||
		out["message"] != "The site shop is being restored from a backup: this waits until that is done." {
		t.Errorf("a deploy of the site meanwhile: %d %v", status, out)
	}
	release()
	ended := d.ended(restore.Id)
	if ended.GetString("status") != schema.RestoreSucceeded {
		t.Fatalf("the restore: %v", ended.FieldsData())
	}
	life := d.lifeSince(lives)
	slices.Sort(life)
	if !slices.Equal(life, []string{"start:shop-php", "start:shop-worker", "stop:shop-php", "stop:shop-worker"}) {
		t.Errorf("what stopped and started: %v", life)
	}
	if status := d.docker.Find("shop-php").Status; status != "running" {
		t.Errorf("the site's PHP-FPM is %s", status)
	}
}

// TestRestoreIntoANewDatabase (#53): a restore into a new database makes its
// record and its user's, with a password and the secret db/<name> of their
// own, restores as that user, and leaves the new database ready; one that
// fails drops what it made, and the panel forgets it.
func TestRestoreIntoANewDatabase(t *testing.T) {
	d := newDBBackupsHarness(t)
	server := d.sharedServer("main", "mariadb")
	shop := d.database(server, "shop", d.application("shop"), "items", "orders")
	job := d.jobOf("database", shop.Id)
	run := d.backedUp(job)
	lives := d.lives()

	restore := d.draft(job, map[string]any{"snapshot": run.GetString("snapshot_id"), "mode": "new_database", "name": "shop_copy"})
	if stops := names(previewOf(t, restore)["would_stop"]); len(stops) != 0 {
		t.Errorf("the preview stops %v", stops)
	}
	if status, out := d.send(http.MethodPost, "/api/pc/backup-restores/"+restore.Id+"/start", d.operator, map[string]any{}); status != http.StatusOK {
		t.Fatalf("the start: %d %v", status, out)
	}
	ended := d.ended(restore.Id)
	if ended.GetString("status") != schema.RestoreSucceeded || ended.GetInt("tables") != 2 {
		t.Fatalf("the restore: %v", ended.FieldsData())
	}
	copied := d.databaseNamed(server, "shop_copy")
	account := d.user(server, "shop_copy")
	if copied.GetString("status") != "ready" || copied.GetString("owner_kind") != "none" || ended.GetString("database") != copied.Id ||
		account.GetString("secret_name") != "db/shop_copy" || !slices.Contains(account.GetStringSlice("databases"), copied.Id) {
		t.Errorf("the new database: %v %v", copied.FieldsData(), account.FieldsData())
	}
	password, ok := d.db.Password(server.GetString("container_ref"), "shop_copy")
	if sealed, err := d.panel.Box.OpenString(account.GetString("password")); err != nil || !ok || password != sealed || password == "" {
		t.Errorf("the new user's password on the server: %v %v", ok, err)
	}
	if tables, from := d.tables(server, "shop_copy"); !slices.Equal(tables, []string{"items", "orders"}) || from != "shop" {
		t.Errorf("the new database holds %v from %q", tables, from)
	}
	if tables, _ := d.tables(server, "shop"); !slices.Equal(tables, []string{"items", "orders"}) {
		t.Errorf("shop after: %v", tables)
	}
	if life := d.lifeSince(lives); len(life) != 0 {
		t.Errorf("a restore into a new database stopped or started %v", life)
	}

	// one that fails is dropped again, its records with it
	restore = d.draft(job, map[string]any{"snapshot": run.GetString("snapshot_id"), "mode": "new_database", "name": "shop_fail"})
	if status, out := d.send(http.MethodPost, "/api/pc/backup-restores/"+restore.Id+"/start", d.operator, map[string]any{}); status != http.StatusOK {
		t.Fatalf("the start: %d %v", status, out)
	}
	ended = d.ended(restore.Id)
	if ended.GetString("status") != schema.RestoreFailed || !strings.HasPrefix(ended.GetString("error"), "The restore into the new database shop_fail failed, and it was dropped again: ") {
		t.Errorf("the failed restore: %v", ended.FieldsData())
	}
	if _, err := d.app.FindFirstRecordByFilter(schema.Databases, "name='shop_fail'"); err == nil {
		t.Error("the failed new database's record stays")
	}
	if _, err := d.app.FindFirstRecordByFilter(schema.DBUsers, "name='shop_fail'"); err == nil {
		t.Error("the failed new database's user stays")
	}
	if _, there := d.db.Database(server.GetString("container_ref"), "shop_fail"); there {
		t.Error("the failed new database stays on the server")
	}
	if _, there := d.db.Password(server.GetString("container_ref"), "shop_fail"); there {
		t.Error("the failed new database's user stays on the server")
	}
}

// user is a server's user by its name.
func (d *dbBackupsHarness) user(server *core.Record, name string) *core.Record {
	d.t.Helper()
	account, err := d.app.FindFirstRecordByFilter(schema.DBUsers, "server={:server} && name={:name}", dbx.Params{"server": server.Id, "name": name})
	if err != nil {
		d.t.Fatalf("the user %s: %v", name, err)
	}
	return account
}

// sqlDownload downloads a database job's snapshot as SQL, as token: the
// answer's status, its Content-Type, and what came.
func (d *dbBackupsHarness) sqlDownload(job, run *core.Record, token string) (int, string, string) {
	d.t.Helper()
	status, out := d.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/downloads", token,
		map[string]any{"target": d.disk.Id, "snapshot": run.GetString("snapshot_id"), "kind": "sql"})
	if status != http.StatusCreated {
		return status, "", fmt.Sprint(out["message"])
	}
	server := httptest.NewServer(d.handler)
	defer server.Close()
	response, err := http.Get(server.URL + out["url"].(string)) //nolint:noctx // a browser's navigation
	if err != nil {
		d.t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		d.t.Fatal(err)
	}
	return response.StatusCode, response.Header.Get("Content-Type"), string(data)
}

// TestSqlDownloads (#53): an admin downloads a database job's snapshot as
// plain SQL: PostgreSQL's custom archive through a conversion container of
// its server's own image, which has no network and no capability;
// MariaDB's dump as it is stored. An operator does not download.
func TestSqlDownloads(t *testing.T) {
	d := newDBBackupsHarness(t)
	pg := d.sharedServer("pg", "postgres")
	maria := d.sharedServer("maria", "mariadb")
	pgJob := d.jobOf("database", d.database(pg, "shop", nil, "items").Id)
	mariaJob := d.jobOf("database", d.database(maria, "blog", nil, "posts").Id)
	pgRun, mariaRun := d.backedUp(pgJob), d.backedUp(mariaJob)

	if status, _, message := d.sqlDownload(pgJob, pgRun, d.operator); status != http.StatusForbidden {
		t.Errorf("an operator's download: %d %s", status, message)
	}
	status, kind, body := d.sqlDownload(pgJob, pgRun, d.admin)
	if status != http.StatusOK || kind != "application/sql; charset=utf-8" || !strings.Contains(body, "\n-- PostgreSQL database dump\n") ||
		!strings.Contains(body, "PGDMP") || !strings.Contains(body, "dbname: shop") {
		t.Errorf("PostgreSQL's SQL: %d %s %q", status, kind, body)
	}
	converters := 0
	for _, job := range d.ranJobs() {
		if len(job.Entrypoint) == 0 || job.Entrypoint[0] != "pg_restore" {
			continue
		}
		converters++
		if job.User != "postgres" || job.Host["NetworkMode"] != "none" || job.Host["ReadonlyRootfs"] != true ||
			capabilities(job) != "[]" || !slices.Contains(job.Cmd, "--no-owner") {
			t.Errorf("the conversion container: %+v %v", job, job.Host)
		}
	}
	if converters != 1 {
		t.Errorf("%d conversion containers", converters)
	}

	status, kind, body = d.sqlDownload(mariaJob, mariaRun, d.admin)
	if status != http.StatusOK || kind != "application/sql; charset=utf-8" || !strings.HasPrefix(body, "-- MariaDB dump") ||
		!strings.Contains(body, "Database: blog") || !strings.HasSuffix(body, "-- Dump completed\n") {
		t.Errorf("MariaDB's SQL: %d %s %q", status, kind, body)
	}
	if count := 0; true {
		for _, job := range d.ranJobs() {
			if len(job.Entrypoint) > 0 && job.Entrypoint[0] == "pg_restore" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("MariaDB's download was converted: %d conversions", count)
		}
	}
}

// restoreTests are a job's restore tests, oldest first.
func (d *dbBackupsHarness) restoreTests(job *core.Record) []*core.Record {
	d.t.Helper()
	rows, err := d.app.FindRecordsByFilter(schema.BackupChecks, "kind={:kind} && job={:job}", "created,id", 0, 0,
		dbx.Params{"kind": schema.CheckRestoreTest, "job": job.Id})
	if err != nil {
		d.t.Fatal(err)
	}
	return rows
}

// testEnded waits until a restore test ended, and its finisher, which alerts
// and notes it in the events after it keeps how it went, is done.
func (d *dbBackupsHarness) testEnded(id string) *core.Record {
	d.t.Helper()
	var row *core.Record
	d.waitFor("the restore test "+id+" to end", func() bool {
		found, err := d.app.FindRecordById(schema.BackupChecks, id)
		if err != nil {
			return false
		}
		row = found
		switch found.GetString("status") {
		case schema.CheckQueued, schema.CheckRunning:
			return false
		}
		events, err := d.app.FindRecordsByFilter(schema.Events, "kind~'backup.restore_test_' && data.check={:id}", "", 0, 0, dbx.Params{"id": id})
		return err == nil && len(events) > 0
	})
	return row
}

// testNow starts a restore test of a job by hand, as the operator, and waits
// until it ended.
func (d *dbBackupsHarness) testNow(job *core.Record) *core.Record {
	d.t.Helper()
	status, out := d.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/restore-test", d.operator, nil)
	if status != http.StatusAccepted {
		d.t.Fatalf("the restore test of %s: %d %v", job.GetString("name"), status, out)
	}
	return d.testEnded(out["check"].(string))
}

// TestRestoreTestsRunOnScheduleAndAlert (#53, AC2): at the slot of their
// schedule every database job's newest backup is restored into a temporary
// database of its server, one test of a server at a time, and passes when
// the tables are all there; a test of a damaged repository fails with the
// critical alert and its event, which the next test that passes resolves;
// the temporary databases go two hours after their test; a job whose tests
// have not passed for long raises the warning.
func TestRestoreTestsRunOnScheduleAndAlert(t *testing.T) {
	d := newDBBackupsHarness(t)
	server := d.sharedServer("main", "postgres")
	jobs := []*core.Record{}
	for _, name := range []string{"shop", "blog"} {
		job := d.jobOf("database", d.database(server, name, nil, name+"_a", name+"_b", name+"_c").Id)
		d.backedUp(job)
		jobs = append(jobs, job)
	}
	schedule, slot := farSchedule(time.Now())
	if status, out := d.send(http.MethodPut, "/api/pc/settings/backup-checks", d.admin, map[string]any{"restore_test_schedule": schedule}); status != http.StatusOK ||
		out["next_restore_test_at"] != slot.Format(time.RFC3339) {
		t.Fatalf("the restore tests' schedule: %d %v", status, out)
	}

	// nothing before the slot; at it, a test of each job, one at a time on
	// their server
	release, first := d.holdRestores()
	d.panel.RunDueRestoreTests(slot.Add(-time.Minute))
	for _, job := range jobs {
		if rows := d.restoreTests(job); len(rows) != 0 {
			t.Fatalf("a test before its slot: %v", rows[0].FieldsData())
		}
	}
	d.panel.RunDueRestoreTests(slot)
	rows := map[string]*core.Record{}
	for _, job := range jobs {
		found := d.restoreTests(job)
		if len(found) != 1 {
			t.Fatalf("%d tests of %s at the slot", len(found), job.GetString("name"))
		}
		row := found[0]
		temporary := databases.TestDatabaseName(row.Id)
		if row.GetString("trigger") != schema.RunSchedule || !row.GetDateTime("scheduled_at").Time().Equal(slot) || row.GetString("temporary") != temporary ||
			row.GetString("target") != d.disk.Id || row.GetString("host") != d.host.Id || row.GetString("database_name") != job.GetString("source") {
			t.Errorf("the test of %s: %v", job.GetString("name"), row.FieldsData())
		}
		copied := d.databaseNamed(server, temporary)
		if copied.GetDateTime("temporary_until").IsZero() || copied.GetString("owner_kind") != "none" {
			t.Errorf("the temporary database %s: %v", temporary, copied.FieldsData())
		}
		rows[temporary] = row
	}
	waiting := first()
	if rows[waiting] == nil {
		t.Fatalf("a restore into %s waits", waiting)
	}
	select {
	case other := <-d.reached:
		t.Errorf("a second test of the server restores into %s meanwhile", other)
	case <-time.After(500 * time.Millisecond):
	}
	release()
	ended := []*core.Record{}
	for _, row := range rows {
		row = d.testEnded(row.Id)
		if row.GetString("status") != schema.CheckOK || row.GetInt("tables_expected") != 3 || row.GetInt("tables_restored") != 3 || row.GetString("failure") != "" {
			t.Errorf("the test %s: %v", row.Id, row.FieldsData())
		}
		ended = append(ended, row)
	}
	slices.SortFunc(ended, func(a, b *core.Record) int {
		return a.GetDateTime("started_at").Time().Compare(b.GetDateTime("started_at").Time())
	})
	if ended[1].GetDateTime("started_at").Time().Before(ended[0].GetDateTime("finished_at").Time()) {
		t.Errorf("two tests of a server ran at once: %v - %v and %v - %v", ended[0].GetDateTime("started_at"), ended[0].GetDateTime("finished_at"),
			ended[1].GetDateTime("started_at"), ended[1].GetDateTime("finished_at"))
	}
	for temporary := range rows {
		if copied, ok := d.db.Database(server.GetString("container_ref"), temporary); !ok || copied.Owner == "" || !copied.Closed || copied.From == "" {
			t.Errorf("the test's database %s on the server: %+v", temporary, copied)
		}
	}

	// a damaged repository fails the test, with the critical alert
	repository := filepath.Join(d.hostRoot, d.diskDir)
	fakerestic.Damage(t, d.restic, repository)
	shop := jobs[0]
	failed := d.testNow(shop)
	if failed.GetString("status") != schema.CheckFailed || failed.GetString("failure") != backup.TestDamaged || failed.GetString("trigger") != schema.RunManual {
		t.Errorf("the test of a damaged repository: %v", failed.FieldsData())
	}
	alert := d.openAlerts(backup.AlertRestoreTestFailed)[shop.Id]
	if alert == nil || alert.GetString("severity") != schema.SeverityCritical || alert.GetString("host") != d.host.Id ||
		!strings.HasPrefix(alert.GetString("message"), "The restore test of shop on main failed: ") {
		t.Errorf("the failed test's alert: %v", alert)
	}
	if event := d.event(backup.EventRestoreTestFailed, "data.check={:id}", dbx.Params{"id": failed.Id}); event.GetString("severity") != schema.SeverityCritical {
		t.Errorf("the failed test's event: %v", event.FieldsData())
	}
	// the next test that passes resolves it
	if err := os.Remove(filepath.Join(fakerestic.Repository(d.restic, repository), "damaged")); err != nil {
		t.Fatal(err)
	}
	if passed := d.testNow(shop); passed.GetString("status") != schema.CheckOK {
		t.Errorf("the test after the repair: %v", passed.FieldsData())
	}
	if alert := d.openAlerts(backup.AlertRestoreTestFailed)[shop.Id]; alert != nil {
		t.Errorf("the alert stays after a test passed: %v", alert.FieldsData())
	}

	// two hours after their tests the temporary databases go
	d.panel.SweepTemporaries(time.Now().Add(2*time.Hour + time.Minute))
	d.waitFor("the temporary databases to go", func() bool {
		left, err := d.app.FindRecordsByFilter(schema.Databases, "temporary_until!=''", "", 0, 0)
		return err == nil && len(left) == 0
	})
	for _, name := range d.db.DatabaseNames(server.GetString("container_ref")) {
		if strings.HasPrefix(name, "pc_") {
			t.Errorf("the temporary database %s stays on the server", name)
		}
	}

	// tests that do not pass for long are a warning
	d.panel.WatchRestoreTests(time.Now().AddDate(3, 0, 0))
	if alert := d.openAlerts(backup.AlertRestoreTestUnverified)[jobs[1].Id]; alert == nil || alert.GetString("severity") != schema.SeverityWarning {
		t.Errorf("the watchdog's alert: %v", alert)
	}
}

// TestTemporaryOrphans (#53): a database of the temporary namespace that a
// server reports and the panel has no record of, as a panel restored from an
// older backup may not, becomes a temporary record, which is dropped at its
// time, and gets no job; the API makes no database of that namespace.
func TestTemporaryOrphans(t *testing.T) {
	d := newDBBackupsHarness(t)
	server := d.sharedServer("main", "postgres")
	orphan := "pc_rt_aaaaaaaaaaaaaaa"
	d.db.AddDatabase(server.GetString("container_ref"), orphan, "", "", "items")
	status, out := d.send(http.MethodPost, "/api/pc/db-servers/"+server.Id+"/stats", d.operator, nil)
	if status != http.StatusOK && status != http.StatusAccepted {
		t.Fatalf("the server's numbers: %d %v", status, out)
	}
	var record *core.Record
	d.waitFor("the orphan's record", func() bool {
		found, err := d.app.FindFirstRecordByFilter(schema.Databases, "name={:name}", dbx.Params{"name": orphan})
		record = found
		return err == nil
	})
	until := record.GetDateTime("temporary_until").Time()
	if record.GetString("server") != server.Id || record.GetString("owner_kind") != "none" || until.Before(time.Now().Add(time.Hour)) || until.After(time.Now().Add(3*time.Hour)) {
		t.Errorf("the orphan's record: %v", record.FieldsData())
	}
	d.event(backup.EventTemporaryFound, "", nil)
	if status, out := d.send(http.MethodPost, "/api/pc/databases", d.operator, map[string]any{"server": server.Id, "name": "pc_rt_bbbbbbbbbbbbbbb"}); status != http.StatusBadRequest ||
		out["message"] != databases.ReservedPrefixProblem {
		t.Errorf("a database of the namespace by hand: %d %v", status, out)
	}

	d.panel.SweepTemporaries(time.Now().Add(3 * time.Hour))
	d.waitFor("the orphan to go", func() bool {
		_, err := d.app.FindRecordById(schema.Databases, record.Id)
		return err != nil
	})
	if _, there := d.db.Database(server.GetString("container_ref"), orphan); there {
		t.Error("the orphan stays on the server")
	}
	d.event(backup.EventTemporaryDropped, "", nil)
	if jobs, err := d.app.FindRecordsByFilter(schema.BackupJobs, "origin={:origin}", "", 0, 0, dbx.Params{"origin": schema.JobDatabase}); err != nil || len(jobs) != 0 {
		t.Errorf("a temporary database got a job: %v %v", jobs, err)
	}
}

// event waits for an event of a kind, which a finisher logs after it kept
// what it says, and returns it.
func (d *dbBackupsHarness) event(kind, filter string, params dbx.Params) *core.Record {
	d.t.Helper()
	all := dbx.Params{"kind": kind}
	for key, value := range params {
		all[key] = value
	}
	where := "kind={:kind}"
	if filter != "" {
		where += " && " + filter
	}
	var found *core.Record
	d.waitFor("the event "+kind, func() bool {
		record, err := d.app.FindFirstRecordByFilter(schema.Events, where, all)
		found = record
		return err == nil
	})
	return found
}

// drop drops a database through the panel and waits until it is gone.
func (d *dbBackupsHarness) drop(database *core.Record) {
	d.t.Helper()
	status, out := d.send(http.MethodDelete, "/api/pc/databases/"+database.Id, d.operator, nil)
	if status != http.StatusOK && status != http.StatusAccepted {
		d.t.Fatalf("the drop of %s: %d %v", database.GetString("name"), status, out)
	}
	d.waitFor("the database "+database.GetString("name")+" to go", func() bool {
		_, err := d.app.FindRecordById(schema.Databases, database.Id)
		return err != nil
	})
}

// TestDroppedDatabaseKeepsItsSnapshots (#53): a dropped database's job stays,
// disabled, with its snapshots, which no longer go back into a database but
// into a new one; one under the old name works, and gets a job of its own.
// The drop resolves the alerts of the database's failed restore test and
// failed restore, which nothing would resolve later.
func TestDroppedDatabaseKeepsItsSnapshots(t *testing.T) {
	d := newDBBackupsHarness(t)
	server := d.sharedServer("main", "mariadb")
	shop := d.database(server, "shop", nil, "items", "orders")
	job := d.jobOf("database", shop.Id)
	run := d.backedUp(job)

	// a restore test and a restore of a damaged repository fail, with alerts
	repository := filepath.Join(d.hostRoot, d.diskDir)
	fakerestic.Damage(t, d.restic, repository)
	if failed := d.testNow(job); failed.GetString("status") != schema.CheckFailed {
		t.Fatalf("the test of a damaged repository: %v", failed.FieldsData())
	}
	restore := d.draft(job, map[string]any{"snapshot": run.GetString("snapshot_id"), "mode": "database"})
	if status, out := d.startNamed(restore, "shop"); status != http.StatusOK {
		t.Fatalf("the start: %d %v", status, out)
	}
	if ended := d.ended(restore.Id); ended.GetString("status") != schema.RestoreFailed {
		t.Fatalf("the restore from a damaged repository: %v", ended.FieldsData())
	}
	restoreKey := d.host.Id + ":database:" + shop.Id
	if d.openAlerts(backup.AlertRestoreTestFailed)[job.Id] == nil || d.openAlerts("backup_restore")[restoreKey] == nil {
		t.Fatalf("the alerts before the drop: %v %v", d.openAlerts(backup.AlertRestoreTestFailed), d.openAlerts("backup_restore"))
	}
	if err := os.Remove(filepath.Join(fakerestic.Repository(d.restic, repository), "damaged")); err != nil {
		t.Fatal(err)
	}
	d.settled()

	d.drop(shop)

	d.waitFor("the job to be disabled", func() bool { return !d.job(job.Id).GetBool("enabled") })
	kept := d.job(job.Id)
	if kept.GetString("database") != "" || kept.GetString("problem") != "Its database was dropped." || kept.GetString("name") != "main-shop" ||
		kept.GetString("db_server_name") != "main" || kept.GetString("next_run_at") != "" {
		t.Errorf("the dropped database's job: %v", kept.FieldsData())
	}
	if snapshots := d.snapshots(job.Id); len(snapshots) != 1 {
		t.Errorf("the dropped database's snapshots: %v", snapshots)
	}
	d.event(backup.EventJobDisabled, "data.job={:job}", dbx.Params{"job": job.Id})
	d.waitFor("the restore test's alert to be resolved", func() bool { return d.openAlerts(backup.AlertRestoreTestFailed)[job.Id] == nil })
	if alert := d.openAlerts("backup_restore")[restoreKey]; alert != nil {
		t.Errorf("the dropped database's restore alert stays: %v", alert.FieldsData())
	}
	if status, out := d.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/run", d.operator, nil); status != http.StatusConflict ||
		out["message"] != "Its database was dropped: restore its snapshots into a new database." {
		t.Errorf("a run of the dropped database: %d %v", status, out)
	}
	if status, out := d.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/restores", d.operator, map[string]any{"target": d.disk.Id,
		"snapshot": run.GetString("snapshot_id"), "mode": "database"}); status != http.StatusConflict || out["message"] != "The database was dropped: restore it into a new database." {
		t.Errorf("a restore back into the dropped database: %d %v", status, out)
	}

	restore = d.draft(job, map[string]any{"snapshot": run.GetString("snapshot_id"), "mode": "new_database", "name": "shop", "server": server.Id})
	if status, out := d.send(http.MethodPost, "/api/pc/backup-restores/"+restore.Id+"/start", d.operator, map[string]any{}); status != http.StatusOK {
		t.Fatalf("the start: %d %v", status, out)
	}
	if ended := d.ended(restore.Id); ended.GetString("status") != schema.RestoreSucceeded {
		t.Fatalf("the restore into a new shop: %v", ended.FieldsData())
	}
	again := d.databaseNamed(server, "shop")
	if again.GetString("status") != "ready" {
		t.Errorf("the new shop: %v", again.FieldsData())
	}
	if tables, from := d.tables(server, "shop"); !slices.Equal(tables, []string{"items", "orders"}) || from != "shop" {
		t.Errorf("the new shop holds %v from %q", tables, from)
	}
	if next := d.jobOf("database", again.Id); next.Id == job.Id || next.GetString("name") != "main-shop-2" || !next.GetBool("enabled") {
		t.Errorf("the new shop's job: %v", next.FieldsData())
	}
	if kept := d.job(job.Id); kept.GetBool("enabled") || kept.GetString("database") != "" {
		t.Errorf("the old job after: %v", kept.FieldsData())
	}
}

// TestAMovedDatabaseKeepsOneRetentionGroup (#53, AC3): a database that moves
// to another shared server keeps its job, whose next dump is of the new
// server and tagged with its name; the retention selects the job's
// snapshots by its own tags and keeps them as one group, so the snapshots
// of both servers are forgotten as one.
func TestAMovedDatabaseKeepsOneRetentionGroup(t *testing.T) {
	d := newDBBackupsHarness(t)
	old := d.sharedServer("pg", "postgres")
	next := d.sharedServer("pg-next", "postgres")
	shop := d.database(old, "shop", d.application("shop"), "items", "orders")
	job := d.jobOf("database", shop.Id)
	if status, out := d.send(http.MethodPatch, "/api/pc/backup-jobs/"+job.Id, d.admin, map[string]any{"retention": map[string]any{"last": 1}}); status != http.StatusOK {
		t.Fatalf("the job's retention: %d %v", status, out)
	}
	d.backedUp(job)

	status, out := d.send(http.MethodPost, "/api/pc/db-servers/"+old.Id+"/move", d.operator, map[string]any{"target": next.Id, "database": "shop"})
	if status != http.StatusOK {
		t.Fatalf("the move: %d %v", status, out)
	}
	move := out["move"].(map[string]any)["id"].(string)
	d.waitForStatus(out["command"].(map[string]any)["id"].(string), schema.CommandSucceeded)
	d.waitFor("the move to be done", func() bool {
		found, err := d.app.FindRecordById(schema.DBMoves, move)
		return err == nil && found.GetString("status") == schema.MoveDone
	})
	d.waitFor("the job to follow its database", func() bool { return d.job(job.Id).GetString("db_server_name") == "pg-next" })
	if moved := d.jobOf("database", shop.Id); moved.Id != job.Id {
		t.Fatalf("the moved database got another job: %v", moved.FieldsData())
	}
	run := d.backedUp(job)
	command := d.command(run.GetString("command"))
	payload := map[string]any{}
	if err := command.UnmarshalJSONField("payload", &payload); err != nil {
		t.Fatal(err)
	}
	if payload["stdin_container"] != next.GetString("container_ref") || !slices.Equal(names(payload["extra_tags"]), []string{"db:shop", "dbserver:pg-next"}) {
		t.Errorf("the run after the move: %v", payload)
	}
	snapshots := d.snapshots(job.Id)
	if len(snapshots) != 1 || !slices.Contains(snapshots[0].Tags, "dbserver:pg-next") || snapshots[0].ID != run.GetString("snapshot_id") {
		t.Errorf("the snapshots after the move: %+v", snapshots)
	}
	for _, line := range strings.Split(fakerestic.Calls(t, d.restic), "\n") {
		if strings.Contains(line, " forget ") && (strings.Contains(line, "dbserver:") || strings.Contains(line, "--group-by tags")) {
			t.Errorf("a forget by the server's tag: %q", line)
		}
	}
}

// TestDatabaseJobsNeedAnUpdatedAgent (#53): an agent that does not say it
// backs up and restores single databases gets no run, restore, conversion
// or restore test of a database's job: each is refused, saying to update
// it, and a scheduled test is skipped.
func TestDatabaseJobsNeedAnUpdatedAgent(t *testing.T) {
	d := newDBBackupsHarness(t)
	pg := d.sharedServer("main", "postgres")
	job := d.jobOf("database", d.database(pg, "shop", nil, "items").Id)
	run := d.backedUp(job)

	// as an agent before #53 says what it can do
	host := d.fresh(schema.Hosts, d.host.Id)
	host.Set("capabilities", slices.DeleteFunc(host.GetStringSlice("capabilities"), func(capability string) bool {
		return capability == backup.CapabilityDatabase || capability == "backup.restore_test" || capability == "db.database.discard"
	}))
	if err := d.app.Save(host); err != nil {
		t.Fatal(err)
	}
	if status, out := d.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/run", d.operator, nil); status != http.StatusConflict ||
		out["message"] != "The agent of backup-node cannot back up databases one by one yet: update it." {
		t.Errorf("a run: %d %v", status, out)
	}
	if status, out := d.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/restores", d.operator, map[string]any{"target": d.disk.Id,
		"snapshot": run.GetString("snapshot_id"), "mode": "database"}); status != http.StatusConflict || out["message"] != "The agent of backup-node cannot restore databases yet: update it." {
		t.Errorf("a restore: %d %v", status, out)
	}
	if status, _, message := d.sqlDownload(job, run, d.admin); status != http.StatusConflict || message != "The agent of backup-node cannot convert dumps to SQL yet: update it." {
		t.Errorf("a download as SQL: %d %s", status, message)
	}
	if status, out := d.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/restore-test", d.operator, nil); status != http.StatusConflict ||
		out["message"] != "The agent of backup-node cannot test restores yet: update it." {
		t.Errorf("a restore test: %d %v", status, out)
	}
	schedule, slot := farSchedule(time.Now())
	if status, out := d.send(http.MethodPut, "/api/pc/settings/backup-checks", d.admin, map[string]any{"restore_test_schedule": schedule}); status != http.StatusOK {
		t.Fatalf("the restore tests' schedule: %d %v", status, out)
	}
	d.panel.RunDueRestoreTests(slot)
	rows := d.restoreTests(job)
	if len(rows) != 1 || rows[0].GetString("status") != schema.CheckSkipped || rows[0].GetString("command") != "" ||
		rows[0].GetString("error") != "The agent of backup-node cannot test restores yet: update it." {
		t.Errorf("the scheduled test: %v", rows)
	}
	for _, kind := range []string{"backup.restore", "backup.restore_test", "backup.dump"} {
		if commands, err := d.app.FindRecordsByFilter(schema.Commands, "type={:type}", "", 0, 0, dbx.Params{"type": kind}); err != nil || len(commands) != 0 {
			t.Errorf("the old agent was sent %s: %v %v", kind, commands, err)
		}
	}
}

// TestSiteFilesJob (#53): a PHP site gets the job <site>-files once it was
// deployed, which backs up its shared/ read-only, without what its template
// leaves out, tagged with the site; a site removed leaves its job disabled,
// with its snapshots.
func TestSiteFilesJob(t *testing.T) {
	d := newDBBackupsHarness(t, func(engine *fakedocker.Engine) {
		engine.AddVolume("pocketcloud-sites")
		writeTree(t, engine.VolumeDir("pocketcloud-sites"), map[string]string{
			"wp/shared/wp-content/uploads/a.png": "png", "wp/shared/wp-content/cache/page.html": "cached",
			"wp/releases/1/index.php": "<?php", "wp/.build/log": "built"})
	})
	site := d.phpSite("wp", map[string]any{"backup_exclude": []string{"wp-content/cache"}})
	collection, err := d.app.FindCollectionByNameOrId(schema.Deployments)
	if err != nil {
		t.Fatal(err)
	}
	deployment := core.NewRecord(collection)
	deployment.Load(map[string]any{"site": site.Id, "number": 1, "status": schema.DeploymentSucceeded})
	if err := d.app.SaveNoValidate(deployment); err != nil {
		t.Fatal(err)
	}
	site.Set("active_deployment", deployment.Id)
	if err := d.app.SaveNoValidate(site); err != nil {
		t.Fatal(err)
	}
	job := d.jobOf("site", site.Id)
	if job.GetString("name") != "wp-files" || job.GetString("source_kind") != schema.SourceSite || job.GetString("source") != "wp/shared" ||
		job.GetString("origin") != schema.JobSite || !slices.Equal(job.GetStringSlice("targets"), []string{d.disk.Id}) {
		t.Errorf("the site's job: %v", job.FieldsData())
	}
	run := d.backedUp(job)
	jobs := d.backupJobs()
	backupJob := jobs[len(jobs)-1]
	for target, mount := range mounts(backupJob) {
		if target == "/srv/sites/wp/shared" && (mount["ReadOnly"] != true || mount["Source"] != "pocketcloud-sites") ||
			target != "/srv/sites/wp/shared" && mount["Source"] == "pocketcloud-sites" {
			t.Errorf("the job's mount %s: %v", target, mount)
		}
	}
	if mounts(backupJob)["/srv/sites/wp/shared"] == nil {
		t.Errorf("the job's mounts: %v", mounts(backupJob))
	}
	if !slices.Contains(backupJob.Cmd, "/srv/sites/wp/shared/wp-content/cache") || !slices.Contains(backupJob.Cmd, "--exclude") {
		t.Errorf("the job's command: %q", backupJob.Cmd)
	}
	snapshots := d.snapshots(job.Id)
	if len(snapshots) != 1 || snapshots[0].ID != run.GetString("snapshot_id") || !slices.Equal(snapshots[0].Paths, []string{"/srv/sites/wp/shared"}) ||
		!slices.Contains(snapshots[0].Tags, "site:wp") || len(snapshots[0].Files) != 1 || snapshots[0].Files["/srv/sites/wp/shared/wp-content/uploads/a.png"] != "png" {
		t.Errorf("the site's snapshot: %+v", snapshots)
	}

	if err := d.app.Delete(site); err != nil {
		t.Fatal(err)
	}
	d.waitFor("the job to be disabled", func() bool { return !d.job(job.Id).GetBool("enabled") })
	if kept := d.job(job.Id); kept.GetString("problem") != "Its site was removed." || kept.GetString("site") != "" || len(d.snapshots(job.Id)) != 1 {
		t.Errorf("the removed site's job: %v", kept.FieldsData())
	}
}
