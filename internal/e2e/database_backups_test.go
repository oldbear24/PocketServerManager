package e2e

import (
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

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
}

// newDBBackupsHarness starts the panel and an agent whose engine's database
// servers are fakedb's. Restore tests, and the jobs the panel makes, never
// come due by themselves: the tests start them, or pass the clock.
func newDBBackupsHarness(t *testing.T, prepare ...func(engine *fakedocker.Engine)) *dbBackupsHarness {
	t.Helper()
	d := &dbBackupsHarness{}
	d.restoresHarness = newRestoresHarness(t, func(engine *fakedocker.Engine) {
		d.db = fakedb.Install(engine)
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
		revision := stack.GetInt("current_revision")
		d.waitFor("the stack "+stack.GetString("name")+" to be connected", func() bool {
			return d.fresh(schema.Stacks, stack.Id).GetInt("current_revision") > revision && !d.deploying(stack.Id)
		})
	}
	d.db.SetTables(server.GetString("container_ref"), name, tables...)
	return d.fresh(schema.Databases, id)
}

// deploying tells whether a deploy of a stack is still open.
func (d *dbBackupsHarness) deploying(stackID string) bool {
	open, err := d.app.FindRecordsByFilter(schema.Commands, "type='stack.deploy' && status!='succeeded' && status!='failed' && status!='cancelled' && status!='timed_out' && payload.stack_id={:stack}",
		"", 0, 0, dbx.Params{"stack": stackID})
	return err != nil || len(open) > 0
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
// database is as it was.
func TestRestoreCraftedDumpCannotTouchOthers(t *testing.T) {
	d := newDBBackupsHarness(t)
	server := d.sharedServer("maria", "mariadb")
	shop := d.application("shop")
	d.database(server, "shop", shop, "items", "orders")
	d.database(server, "blog", nil, "posts", "comments")
	container := server.GetString("container_ref")
	d.db.SetDumpContent(container, "shop", "-- MariaDB dump 10.19  Distrib 11.8.2-MariaDB\n--\n-- Host: localhost    Database: shop\n"+
		"CREATE TABLE `items` (`id` int);\nINSERT INTO `items` VALUES (1);\n"+
		"CREATE TABLE `orders` (`id` int);\nUSE `blog`;\nDROP TABLE `posts`;\n-- Dump completed\n")
	job := d.jobOf("database", d.databaseNamed(server, "shop").Id)
	run := d.backedUp(job)

	from, lives := len(d.db.Execs()), d.lives()
	restore := d.draft(job, map[string]any{"snapshot": run.GetString("snapshot_id"), "mode": "database"})
	if status, out := d.startNamed(restore, "shop"); status != http.StatusOK {
		t.Fatalf("the start: %d %v", status, out)
	}
	ended := d.ended(restore.Id)
	if ended.GetString("status") != schema.RestoreFailed || !strings.HasPrefix(ended.GetString("error"), "Nothing changed in shop: ") ||
		!strings.Contains(ended.GetString("error"), "Access denied for user 'shop'@'%' to database 'blog'") {
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
	if _, err := d.app.FindFirstRecordByFilter(schema.Events, "kind='backup.restore_failed' && data.restore={:id}", dbx.Params{"id": restore.Id}); err != nil {
		t.Errorf("the restore's event: %v", err)
	}
}
