package panel

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/sites"
	"github.com/oldbear24/PocketServerManager/internal/stacks"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// Schedules that the panel's own scheduler, which runs in the tests too,
// never starts while a test runs: a job with targets would run at its
// minute.
const (
	neverDue   = "0 0 30 2 *"
	newYear    = "0 0 1 1 *"
	newYearToo = "30 0 1 1 *"
)

// sharedServer is a shared database server of a host, ready, with its
// administrator's password.
func (f *jobFixture) sharedServer(host *core.Record, name, engine string) *core.Record {
	f.t.Helper()
	admin := "postgres"
	if engine == schema.EngineMariaDB {
		admin = "root"
	}
	return f.save(schema.DBServers, map[string]any{"host": host.Id, "name": name, "engine": engine, "kind": schema.DBServerShared,
		"container_ref": "pcdb-" + name + "-db-1", "admin_user": admin, "admin_password": "admin-secret-of-" + name, "status": dbReady})
}

// database is a database of a server, in a state, with its user.
func (f *jobFixture) database(server *core.Record, name, status string) *core.Record {
	f.t.Helper()
	database := f.save(schema.Databases, map[string]any{"server": server.Id, "name": name, "owner_kind": "none", "status": status})
	if _, err := f.p.userOf(server, name); err != nil {
		f.save(schema.DBUsers, map[string]any{"server": server.Id, "name": name, "password": "user-secret-of-" + name,
			"databases": []string{database.Id}, "secret_name": "db/" + name})
	}
	return database
}

// databaseJobs are the jobs of a database, which the panel makes later.
func (f *jobFixture) databaseJobs(database *core.Record) []*core.Record {
	f.t.Helper()
	jobs, err := f.app.FindRecordsByFilter(schema.BackupJobs, "database={:database}", "", 0, 0, dbx.Params{"database": database.Id})
	if err != nil {
		f.t.Fatal(err)
	}
	return jobs
}

// jobOfDatabase waits for the job of a database.
func (f *jobFixture) jobOfDatabase(database *core.Record) *core.Record {
	f.t.Helper()
	f.eventually("the job of "+database.GetString("name"), func() bool { return len(f.databaseJobs(database)) == 1 })
	jobs := f.databaseJobs(database)
	if len(jobs) != 1 {
		f.t.Fatalf("%d jobs of %s", len(jobs), database.GetString("name"))
	}
	return jobs[0]
}

// noJobOf says that a database gets no job: what its hooks do, done now,
// makes none, and neither did they.
func (f *jobFixture) noJobOf(database *core.Record) {
	f.t.Helper()
	if err := f.p.ensureDatabaseJob(database.Id); err != nil {
		f.t.Errorf("%s: %v", database.GetString("name"), err)
	}
	f.settle()
	if jobs := f.databaseJobs(database); len(jobs) != 0 {
		f.t.Errorf("%s got a job: %v", database.GetString("name"), jobs[0])
	}
}

// settle waits for what the hooks of the saves before do later: a marker
// scheduled after them has run, and what started before it had a moment to
// end.
func (f *jobFixture) settle() {
	f.t.Helper()
	done := make(chan struct{})
	f.p.later.after(0, func() { close(done) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		f.t.Fatal("the panel's later work did not settle")
	}
	// one that started just before may still be saving
	time.Sleep(50 * time.Millisecond)
}

// TestEveryDatabaseOfASharedServerGetsAJob (#53): a ready database of a
// shared server gets a job of its own, made by the panel, named after its
// server and itself, with its server's defaults: the staggered daily
// schedule, no target yet, 7 daily, 4 weekly and 6 monthly snapshots. A
// database that is not ready gets one once it is; a temporary database, a
// name of the temporary namespace, and a database of a dedicated server get
// none.
func TestEveryDatabaseOfASharedServerGetsAJob(t *testing.T) {
	f := newJobFixture(t)
	main := f.sharedServer(f.host, "main", schema.EnginePostgres)
	maria := f.sharedServer(f.host, "maria", schema.EngineMariaDB)

	shop := f.database(main, "shop", dbReady)
	job := f.jobOfDatabase(shop)
	if job.GetString("name") != "main-shop" || job.GetString("host") != f.host.Id || job.GetString("source_kind") != schema.SourceDatabase ||
		job.GetString("source") != "shop" || job.GetString("engine") != schema.DumpPostgres || job.GetString("db_server_name") != "main" ||
		job.GetString("origin") != schema.JobDatabase || !job.GetBool("enabled") || len(job.GetStringSlice("own_settings")) != 0 ||
		job.GetString("schedule") != backup.AutoSchedule(f.host.Id) || len(job.GetStringSlice("targets")) != 0 ||
		retentionOf(job) != (stacks.Retention{Daily: 7, Weekly: 4, Monthly: 6}) || job.GetString("problem") != noTarget ||
		!job.GetDateTime("next_run_at").IsZero() || job.GetString("stack") != "" || job.GetString("site") != "" {
		t.Errorf("the job of shop: %v", job.FieldsData())
	}
	if raw := job.GetString("own_settings"); raw != "[]" {
		t.Errorf("own_settings: %s", raw)
	}
	// the panel made it, and the audit says so
	entries, err := f.app.FindRecordsByFilter(schema.AuditLog, "collection={:c} && record_id={:id}", "seq", 0, 0,
		dbx.Params{"c": schema.BackupJobs, "id": job.Id})
	if err != nil || len(entries) != 1 || entries[0].GetString("actor_kind") != audit.KindSystem || entries[0].GetString("actor_label") != "backups" {
		t.Errorf("the audit of the job: %v %v", entries, err)
	}

	if job := f.jobOfDatabase(f.database(maria, "blog", dbReady)); job.GetString("engine") != schema.DumpMySQL || job.GetString("name") != "maria-blog" {
		t.Errorf("a MariaDB database's job: %v", job.FieldsData())
	}

	// one that is being made gets its job once it is ready
	coming := f.database(main, "coming", dbCreating)
	f.noJobOf(coming)
	coming.Set("status", dbReady)
	if err := f.app.Save(coming); err != nil {
		t.Fatal(err)
	}
	f.jobOfDatabase(coming)

	// temporary databases, names of their namespace, and a dedicated
	// server's databases are not backed up by a job of their own
	temporary := f.save(schema.Databases, map[string]any{"server": main.Id, "name": "pc_rt_" + strings.Repeat("a", 15), "owner_kind": "none",
		"status": dbReady, "temporary_until": types.NowDateTime().Add(2 * time.Hour)})
	looksTemporary := f.save(schema.Databases, map[string]any{"server": main.Id, "name": "pc_rs_mine", "owner_kind": "none", "status": dbReady})
	stillTemporary := f.save(schema.Databases, map[string]any{"server": main.Id, "name": "kept", "owner_kind": "none", "status": dbReady,
		"temporary_until": types.NowDateTime().Add(time.Hour)})
	dedicated := f.save(schema.DBServers, map[string]any{"host": f.host.Id, "name": "umami-db", "engine": schema.EnginePostgres,
		"kind": schema.DBServerDedicated, "container_ref": "umami-db-1", "status": dbReady})
	own := f.save(schema.Databases, map[string]any{"server": dedicated.Id, "name": "umami", "owner_kind": "stack", "status": dbReady})
	for _, database := range []*core.Record{temporary, looksTemporary, stillTemporary, own} {
		f.noJobOf(database)
	}
	// nor does a pass of the panel make one
	f.p.reconcileAutoJobs()
	for _, database := range []*core.Record{temporary, looksTemporary, stillTemporary, own} {
		if jobs := f.databaseJobs(database); len(jobs) != 0 {
			t.Errorf("%s got a job from the pass", database.GetString("name"))
		}
	}
}

// TestADatabaseGetsOneJobThoughAskedAtOnce (#53): the hooks and the passes
// that make a database's job may run at once; the unique index keeps one
// job, and none of them fails.
func TestADatabaseGetsOneJobThoughAskedAtOnce(t *testing.T) {
	f := newJobFixture(t)
	main := f.sharedServer(f.host, "main", schema.EnginePostgres)
	shop := f.database(main, "shop", dbReady)
	const callers = 8
	start := make(chan struct{})
	errs := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			errs <- f.p.ensureDatabaseJob(shop.Id)
		}()
	}
	close(start)
	for range callers {
		if err := <-errs; err != nil {
			t.Errorf("a maker: %v", err)
		}
	}
	f.settle()
	if jobs := f.databaseJobs(shop); len(jobs) != 1 || jobs[0].GetString("name") != "main-shop" {
		t.Errorf("%d jobs", len(jobs))
	}
	if all, _ := f.app.FindRecordsByFilter(schema.BackupJobs, "origin='database'", "", 0, 0); len(all) != 1 {
		t.Errorf("%d database jobs", len(all))
	}
}

// TestADatabaseJobTakesAFreeName (#53): a job named as a database's job
// would be leaves the database's job the name with a number after it.
func TestADatabaseJobTakesAFreeName(t *testing.T) {
	f := newJobFixture(t)
	f.job("main-shop", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	main := f.sharedServer(f.host, "main", schema.EnginePostgres)
	if job := f.jobOfDatabase(f.database(main, "shop", dbReady)); job.GetString("name") != "main-shop-2" {
		t.Errorf("the name: %s", job.GetString("name"))
	}
}

// TestAutomaticJobsFollowTheirServersSettings (#53): an admin sets a
// server's schedule, targets and retention, and each automatic job of the
// server takes those it has not set for itself, with its problem and next
// run again, in one go; a job's own schedule stays through a change of the
// server's, until it follows the server's again. An empty schedule is the
// staggered default, a null retention 7d 4w 6m. The settings change only
// through their route.
func TestAutomaticJobsFollowTheirServersSettings(t *testing.T) {
	f := newJobFixture(t)
	main := f.sharedServer(f.host, "main", schema.EnginePostgres)
	shop, blog := f.jobOfDatabase(f.database(main, "shop", dbReady)), f.jobOfDatabase(f.database(main, "blog", dbReady))
	other := testutil.NewHost(t, f.app, "node-2")
	elsewhere := f.addTarget("elsewhere", backup.KindLocal, other, backup.Config{Path: "/srv/restic"}, backup.Credentials{})
	disk := f.addTarget("disk", backup.KindLocal, f.host, backup.Config{Path: "/srv/restic"}, backup.Credentials{})
	url := "/api/pc/hosts/" + f.host.Id + "/auto-backups"

	status, out := f.send(http.MethodPut, url, f.admin, map[string]any{"schedule": " 0  0 1 1 * ", "targets": []string{f.target.Id, disk.Id, f.target.Id},
		"retention": map[string]int{"daily": 3}})
	if status != http.StatusOK || out["jobs_changed"] != float64(2) {
		t.Fatalf("set: %d %v", status, out)
	}
	if host, _ := out["host"].(map[string]any); host == nil || host["auto_backup_schedule"] != newYear {
		t.Errorf("the server answered: %v", out["host"])
	}
	for _, job := range []*core.Record{shop, blog} {
		job = f.fresh(job)
		if job.GetString("schedule") != newYear || strings.Join(job.GetStringSlice("targets"), ",") != f.target.Id+","+disk.Id ||
			retentionOf(job) != (stacks.Retention{Daily: 3}) || job.GetString("problem") != "" || job.GetDateTime("next_run_at").IsZero() {
			t.Errorf("%s after the server's settings: %v", job.GetString("name"), job.FieldsData())
		}
	}
	// the audit names the admin
	entries, err := f.app.FindRecordsByFilter(schema.AuditLog, "collection={:c} && record_id={:id}", "-seq", 1, 0,
		dbx.Params{"c": schema.Hosts, "id": f.host.Id})
	if err != nil || len(entries) != 1 || entries[0].GetString("actor_kind") != audit.KindUser {
		t.Errorf("the audit of the server: %v %v", entries, err)
	}

	// shop's schedule is its own from now on
	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+shop.Id, f.admin, map[string]any{"schedule": newYearToo}); status != http.StatusOK ||
		out["schedule"] != newYearToo {
		t.Fatalf("shop's own schedule: %d %v", status, out)
	}
	if own := f.fresh(shop).GetStringSlice("own_settings"); !slices.Equal(own, []string{schema.OwnSchedule}) {
		t.Errorf("shop's own settings: %v", own)
	}
	status, out = f.send(http.MethodPut, url, f.admin, map[string]any{"schedule": neverDue, "targets": []string{f.target.Id}})
	if status != http.StatusOK || out["jobs_changed"] != float64(2) {
		t.Fatalf("set again: %d %v", status, out)
	}
	if job := f.fresh(shop); job.GetString("schedule") != newYearToo || strings.Join(job.GetStringSlice("targets"), ",") != f.target.Id ||
		retentionOf(job) != (stacks.Retention{Daily: 7, Weekly: 4, Monthly: 6}) {
		t.Errorf("shop keeps its own schedule only: %v", job.FieldsData())
	}
	if job := f.fresh(blog); job.GetString("schedule") != neverDue || !job.GetDateTime("next_run_at").IsZero() {
		t.Errorf("blog follows: %v", job.FieldsData())
	}
	// nothing changed: no job changed
	if status, out := f.send(http.MethodPut, url, f.admin, map[string]any{"schedule": neverDue, "targets": []string{f.target.Id}}); status != http.StatusOK ||
		out["jobs_changed"] != float64(0) {
		t.Errorf("the same again: %d %v", status, out)
	}

	// shop follows the server's schedule again, and sets its retention
	status, out = f.send(http.MethodPatch, "/api/pc/backup-jobs/"+shop.Id, f.admin, map[string]any{"follow": []string{"schedule"},
		"schedule": newYear, "retention": map[string]int{"weekly": 2}})
	if status != http.StatusOK || out["schedule"] != neverDue {
		t.Fatalf("follow: %d %v", status, out)
	}
	if own := f.fresh(shop).GetStringSlice("own_settings"); !slices.Equal(own, []string{schema.OwnRetention}) {
		t.Errorf("shop's own settings after follow: %v", own)
	}
	status, out = f.send(http.MethodPatch, "/api/pc/backup-jobs/"+shop.Id, f.admin, map[string]any{"follow": []string{"retention", "targets"}})
	if status != http.StatusOK || len(f.fresh(shop).GetStringSlice("own_settings")) != 0 || retentionOf(f.fresh(shop)) != (stacks.Retention{Daily: 7, Weekly: 4, Monthly: 6}) {
		t.Errorf("follow everything: %d %v", status, out)
	}

	// an empty schedule is the server's staggered default
	if status, out := f.send(http.MethodPut, url, f.admin, map[string]any{"schedule": "", "targets": []string{}}); status != http.StatusOK {
		t.Fatalf("defaults: %d %v", status, out)
	}
	for _, job := range []*core.Record{shop, blog} {
		if job := f.fresh(job); job.GetString("schedule") != backup.AutoSchedule(f.host.Id) || job.GetString("problem") != noTarget {
			t.Errorf("%s on the defaults: %v", job.GetString("name"), job.FieldsData())
		}
	}
	// a new database's job starts with the server's settings
	if status, out := f.send(http.MethodPut, url, f.admin, map[string]any{"schedule": neverDue, "targets": []string{disk.Id},
		"retention": map[string]int{"last": 1}}); status != http.StatusOK {
		t.Fatalf("set for the next: %d %v", status, out)
	}
	if job := f.jobOfDatabase(f.database(main, "next", dbReady)); job.GetString("schedule") != neverDue ||
		strings.Join(job.GetStringSlice("targets"), ",") != disk.Id || retentionOf(job) != (stacks.Retention{Last: 1}) || job.GetString("problem") != "" {
		t.Errorf("a new job: %v", job.FieldsData())
	}

	// what the route refuses
	for _, test := range []struct {
		body    map[string]any
		message string
	}{
		{map[string]any{"schedule": "every day"}, stacks.ScheduleProblem},
		{map[string]any{"targets": []string{elsewhere.Id}}, "The target elsewhere is a directory of its own server: only that server's jobs back up to it."},
		{map[string]any{"targets": []string{"nosuchtarget123"}}, "One of the targets does not exist."},
		{map[string]any{"retention": map[string]int{"daily": -1}}, "Each count of the retention is between 0 and 10000."},
		{map[string]any{"retention": map[string]int{"monthly": 10001}}, "Each count of the retention is between 0 and 10000."},
	} {
		if status, out := f.send(http.MethodPut, url, f.admin, test.body); status != http.StatusBadRequest || out["message"] != test.message {
			t.Errorf("%v: %d %v", test.body, status, out)
		}
	}
	if status, out := f.send(http.MethodPut, "/api/pc/hosts/nosuchserver123/auto-backups", f.admin, map[string]any{}); status != http.StatusNotFound ||
		out["message"] != "The server does not exist." {
		t.Errorf("an unknown server: %d %v", status, out)
	}
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	if status, _ := f.send(http.MethodPut, url, operator, map[string]any{}); status != http.StatusForbidden {
		t.Errorf("an operator: %d", status)
	}
	// a refused change left the server as it was
	if host := f.fresh(f.host); host.GetString("auto_backup_schedule") != neverDue {
		t.Errorf("the server after refusals: %v", host.FieldsData())
	}

	// the records API leaves them as they are
	if status, out := f.send(http.MethodPatch, "/api/collections/hosts/records/"+f.host.Id, f.admin, map[string]any{
		"auto_backup_schedule": "* * * * *", "auto_backup_targets": []string{elsewhere.Id}, "auto_backup_retention": nil, "name": "node-one"}); status != http.StatusOK {
		t.Fatalf("the records API: %d %v", status, out)
	}
	if host := f.fresh(f.host); host.GetString("name") != "node-one" || host.GetString("auto_backup_schedule") != neverDue ||
		strings.Join(host.GetStringSlice("auto_backup_targets"), ",") != disk.Id || hostRetention(host) != (stacks.Retention{Last: 1}) {
		t.Errorf("the server after the records API: %v", host.FieldsData())
	}

	// a local target of another server that the server's settings name
	// somehow is no job's
	if _, err := f.app.DB().Update(schema.Hosts, dbx.Params{"auto_backup_targets": `["` + elsewhere.Id + `","` + f.target.Id + `"]`},
		dbx.HashExp{"id": f.host.Id}).Execute(); err != nil {
		t.Fatal(err)
	}
	if settings := autoSettingsOf(f.app, f.fresh(f.host)); !slices.Equal(settings.targets, []string{f.target.Id}) {
		t.Errorf("the settings' targets: %v", settings.targets)
	}
}

// TestAutomaticJobsAreMadeAtTheStart (#53): the databases and the deployed
// PHP sites that a panel had before its automatic jobs, or that came while
// it was down, get their jobs once it serves.
func TestAutomaticJobsAreMadeAtTheStart(t *testing.T) {
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)
	caOfAnEarlierStart(t, app)
	host := testutil.NewHost(t, app, "node-1")
	save := func(collection string, fields map[string]any) *core.Record {
		t.Helper()
		return saveRecord(t, app, collection, fields)
	}
	server := save(schema.DBServers, map[string]any{"host": host.Id, "name": "main", "engine": schema.EnginePostgres,
		"kind": schema.DBServerShared, "container_ref": "pcdb-main-db-1", "admin_user": "postgres", "status": dbReady})
	shop := save(schema.Databases, map[string]any{"server": server.Id, "name": "shop", "owner_kind": "none", "status": dbReady})
	site := siteRecord(t, app, host, "wp", schema.SitePHPApp)
	deploy(t, app, site)

	// the panel starts, and serves
	p := New(app, &Config{MasterKey: testutil.MasterKey, GRPCAddr: "127.0.0.1:0"}, testUI)
	p.Register()
	testutil.Handler(t, app)
	f := &jobFixture{t: t, p: p, app: app, host: host}
	f.eventually("the jobs at the start", func() bool {
		jobs, err := app.FindRecordsByFilter(schema.BackupJobs, "database={:database} || site={:site}", "", 0, 0,
			dbx.Params{"database": shop.Id, "site": site.Id})
		return err == nil && len(jobs) == 2
	})
	if job, err := app.FindFirstRecordByData(schema.BackupJobs, "site", site.Id); err != nil || job.GetString("name") != "wp-files" {
		t.Errorf("the site's job: %v %v", job, err)
	}
}

// siteRecord is a site of a type as it is stored, without the checks of a
// new site, which want an edge on its server, and of an upload, which is not
// deployed by itself. A PHP site has its settings, as the panel makes them.
func siteRecord(t *testing.T, app core.App, host *core.Record, name, kind string) *core.Record {
	t.Helper()
	collection, err := app.FindCollectionByNameOrId(schema.Sites)
	if err != nil {
		t.Fatal(err)
	}
	site := core.NewRecord(collection)
	site.Load(map[string]any{"name": name, "host": host.Id, "type": kind, "source": sites.SourceUpload})
	if err := app.SaveNoValidate(site); err != nil {
		t.Fatal(err)
	}
	if _, err := app.FindFirstRecordByData(schema.SitePHP, "site", site.Id); err != nil && kind == schema.SitePHPApp {
		saveRecord(t, app, schema.SitePHP, map[string]any{"site": site.Id, "php_version": "8.3", "mode": schema.PHPReleases})
	}
	return site
}

// deploy gives a site a deployment that is live.
func deploy(t *testing.T, app core.App, site *core.Record) {
	t.Helper()
	deployment := saveRecord(t, app, schema.Deployments, map[string]any{"site": site.Id, "number": 1, "status": schema.DeploymentSucceeded})
	site.Set("active_deployment", deployment.Id)
	if err := app.SaveNoValidate(site); err != nil {
		t.Fatal(err)
	}
}

// TestADroppedDatabaseLeavesItsJobDisabled (#53): a database that is dropped
// leaves its job, disabled, with its problem, an event, and its backup
// alerts resolved; the job keeps its server's name and its snapshots. It
// does not run, by hand or as its schedule says, nor is it turned on
// again; it may be removed now. A new database of the name gets a job of
// its own. A server removed takes its databases, and so their jobs' sources;
// a host removed takes its jobs.
func TestADroppedDatabaseLeavesItsJobDisabled(t *testing.T) {
	f := newJobFixture(t)
	f.carry(f.host, "backup.run", backup.CapabilityDatabase)
	main := f.sharedServer(f.host, "main", schema.EnginePostgres)
	shop := f.database(main, "shop", dbReady)
	job := f.jobOfDatabase(shop)
	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+job.Id, f.admin, map[string]any{"targets": []string{f.target.Id}, "schedule": neverDue}); status != http.StatusOK {
		t.Fatalf("give it a target: %d %v", status, out)
	}
	key := alertKey(job.Id, f.target.Id)
	if err := events.RaiseAlert(f.app, alertBackupFailed, key, f.host.Id, schema.SeverityCritical, "it failed"); err != nil {
		t.Fatal(err)
	}

	if err := f.app.Delete(shop); err != nil {
		t.Fatal(err)
	}
	f.eventually("the job disabled", func() bool { return !f.fresh(job).GetBool("enabled") })
	job = f.fresh(job)
	if job.GetString("database") != "" || job.GetString("problem") != "Its database was dropped." || !job.GetDateTime("next_run_at").IsZero() ||
		job.GetString("db_server_name") != "main" || job.GetString("source") != "shop" {
		t.Errorf("the job of a dropped database: %v", job.FieldsData())
	}
	f.eventually("the event", func() bool { return len(f.eventsOf(backup.EventJobDisabled)) == 1 })
	if found := f.eventsOf(backup.EventJobDisabled); len(found) != 1 || found[0].GetString("severity") != schema.SeverityWarning ||
		found[0].GetString("message") != "The backup job main-shop was disabled: its database was dropped. Its snapshots stay." {
		t.Errorf("the events: %v", found)
	}
	if f.openAlert(alertBackupFailed, key) != nil {
		t.Error("the job's alert stays open")
	}
	// once is enough
	f.p.markGoneAutoJobs()
	if found := f.eventsOf(backup.EventJobDisabled); len(found) != 1 {
		t.Errorf("%d events", len(found))
	}

	// it does not run, nor come back
	if status, out := f.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/run", f.admin, nil); status != http.StatusConflict ||
		out["message"] != "Its database was dropped: restore its snapshots into a new database." {
		t.Errorf("run it: %d %v", status, out)
	}
	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+job.Id, f.admin, map[string]any{"enabled": true}); status != http.StatusBadRequest ||
		out["message"] != "Its database was dropped: the job cannot be turned on again." {
		t.Errorf("turn it on: %d %v", status, out)
	}
	// its schedule, as if it were still enabled, starts nothing
	f.p.startJobRuns(job, schema.RunSchedule, time.Now().UTC().Truncate(time.Minute))
	if runs := f.runsOf(job); len(runs) != 0 {
		t.Errorf("%d runs of a dropped database's job", len(runs))
	}

	// a new database of the name gets a job of its own
	again := f.database(main, "shop", dbReady)
	if fresh := f.jobOfDatabase(again); fresh.Id == job.Id || fresh.GetString("name") != "main-shop-2" {
		t.Errorf("the new database's job: %v", fresh.FieldsData())
	}
	// the old one may go now
	if status, out := f.send(http.MethodDelete, "/api/pc/backup-jobs/"+job.Id, f.admin, nil); status != http.StatusNoContent {
		t.Errorf("remove it: %d %v", status, out)
	}

	// a server removed takes its databases' sources
	blog := f.jobOfDatabase(f.database(main, "blog", dbReady))
	if err := f.app.Delete(main); err != nil {
		t.Fatal(err)
	}
	f.eventually("the server's jobs disabled", func() bool {
		return !f.fresh(blog).GetBool("enabled") && f.fresh(blog).GetString("problem") == databaseDropped
	})
	// a host removed takes its jobs
	other := testutil.NewHost(t, f.app, "node-2")
	server := f.sharedServer(other, "pool", schema.EngineMariaDB)
	pooled := f.jobOfDatabase(f.database(server, "notes", dbReady))
	if err := f.app.Delete(other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.FindRecordById(schema.BackupJobs, pooled.Id); err == nil {
		t.Error("the job of a removed host stays")
	}
	f.settle()
}

// TestAMovedDatabaseKeepsItsJob (#53): a database that moves to another
// shared server keeps its record, and so its job, which takes the server's
// name; its next run names the new server's container.
func TestAMovedDatabaseKeepsItsJob(t *testing.T) {
	f := newJobFixture(t)
	f.carry(f.host, "backup.run", backup.CapabilityDatabase)
	main := f.sharedServer(f.host, "main", schema.EnginePostgres)
	next := f.sharedServer(f.host, "next", schema.EnginePostgres)
	shop := f.database(main, "shop", dbReady)
	job := f.jobOfDatabase(shop)

	shop.Set("server", next.Id)
	if err := f.app.Save(shop); err != nil {
		t.Fatal(err)
	}
	f.eventually("the job on the new server", func() bool { return f.fresh(job).GetString("db_server_name") == "next" })
	if jobs := f.databaseJobs(shop); len(jobs) != 1 || jobs[0].Id != job.Id {
		t.Errorf("the jobs after the move: %v", jobs)
	}
	run, err := f.p.backupRun(f.fresh(job), f.target, f.host, f.run(job))
	if err != nil || run.GetStdinContainer() != "pcdb-next-db-1" || !slices.Equal(run.GetExtraTags(), []string{"db:shop", "dbserver:next"}) {
		t.Errorf("the next run: %v %v", run, err)
	}
	// a pass keeps a name that a hook missed
	if _, err := f.app.DB().Update(schema.BackupJobs, dbx.Params{"db_server_name": "stale"}, dbx.HashExp{"id": job.Id}).Execute(); err != nil {
		t.Fatal(err)
	}
	f.p.reconcileAutoJobs()
	if got := f.fresh(job).GetString("db_server_name"); got != "next" {
		t.Errorf("after the pass: %s", got)
	}
}

// TestOrphanedLabelJobsLeaveDatabaseJobsAlone (#53): the pass that marks
// the jobs of removed stacks leaves database jobs, which have no stack, as
// they are.
func TestOrphanedLabelJobsLeaveDatabaseJobsAlone(t *testing.T) {
	f := newJobFixture(t)
	main := f.sharedServer(f.host, "main", schema.EnginePostgres)
	job := f.jobOfDatabase(f.database(main, "shop", dbReady))
	f.p.markOrphanedLabelJobs()
	if job := f.fresh(job); !job.GetBool("enabled") || job.GetString("problem") != noTarget || job.GetBool("label_removed") {
		t.Errorf("the database's job: %v", job.FieldsData())
	}
}

// TestADeployedPHPSiteGetsAFilesJob (#53): a PHP site gets the job
// <site>-files at its first deployment, not before; a static site gets
// none; a site removed leaves its job disabled.
func TestADeployedPHPSiteGetsAFilesJob(t *testing.T) {
	f := newJobFixture(t)
	wp := siteRecord(t, f.app, f.host, "wp", schema.SitePHPApp)
	static := siteRecord(t, f.app, f.host, "docs", schema.SiteStatic)
	siteJobs := func(site *core.Record) []*core.Record {
		t.Helper()
		jobs, err := f.app.FindRecordsByFilter(schema.BackupJobs, "site={:site}", "", 0, 0, dbx.Params{"site": site.Id})
		if err != nil {
			t.Fatal(err)
		}
		return jobs
	}
	f.p.reconcileAutoJobs()
	if len(siteJobs(wp)) != 0 {
		t.Error("a site that was never deployed got a job")
	}
	for _, site := range []*core.Record{wp, static} {
		deploy(t, f.app, site)
	}
	f.eventually("the site's job", func() bool { return len(siteJobs(wp)) == 1 })
	job := siteJobs(wp)[0]
	if job.GetString("name") != "wp-files" || job.GetString("source_kind") != schema.SourceSite || job.GetString("source") != "wp" ||
		job.GetString("origin") != schema.JobSite || job.GetString("host") != f.host.Id || job.GetString("problem") != noTarget ||
		job.GetString("schedule") != backup.AutoSchedule(f.host.Id) || job.GetString("database") != "" {
		t.Errorf("the site's job: %v", job.FieldsData())
	}
	f.settle()
	if len(siteJobs(static)) != 0 {
		t.Error("a static site got a job")
	}

	if status, out := f.send(http.MethodDelete, "/api/pc/backup-jobs/"+job.Id, f.admin, nil); status != http.StatusConflict ||
		out["message"] != "It would come back while its site exists: turn it off instead." {
		t.Errorf("remove it: %d %v", status, out)
	}
	if err := f.app.Delete(wp); err != nil {
		t.Fatal(err)
	}
	f.eventually("the job disabled", func() bool { return !f.fresh(job).GetBool("enabled") })
	if job := f.fresh(job); job.GetString("problem") != "Its site was removed." || job.GetString("site") != "" {
		t.Errorf("the job of a removed site: %v", job.FieldsData())
	}
	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+job.Id, f.admin, map[string]any{"enabled": true}); status != http.StatusBadRequest ||
		out["message"] != "Its site was removed: the job cannot be turned on again." {
		t.Errorf("turn it on: %d %v", status, out)
	}
	if status, out := f.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/run", f.admin, nil); status != http.StatusConflict ||
		out["message"] != siteGoneMessage {
		t.Errorf("run it: %d %v", status, out)
	}
	f.eventually("the event", func() bool {
		found := f.eventsOf(backup.EventJobDisabled)
		return len(found) == 1 && strings.Contains(found[0].GetString("message"), "its site was removed")
	})
}

// TestAutomaticJobsKeepWhatTheyBackUp (#53): the API makes no database or
// site job; a change of what an automatic job backs up, or of its server,
// is refused; follow is only for automatic jobs, and only of their
// settings; an automatic job is not removed while its database exists.
func TestAutomaticJobsKeepWhatTheyBackUp(t *testing.T) {
	f := newJobFixture(t)
	other := testutil.NewHost(t, f.app, "node-2")
	main := f.sharedServer(f.host, "main", schema.EnginePostgres)
	job := f.jobOfDatabase(f.database(main, "shop", dbReady))
	for _, kind := range []string{schema.SourceDatabase, schema.SourceSite} {
		if status, out := f.send(http.MethodPost, "/api/pc/backup-jobs", f.admin, map[string]any{"name": "mine", "host": f.host.Id,
			"source_kind": kind, "source": "shop", "targets": []string{f.target.Id}, "schedule": neverDue}); status != http.StatusBadRequest ||
			out["message"] != "Database and site jobs come with their database or site." {
			t.Errorf("add a %s job: %d %v", kind, status, out)
		}
	}
	const backsUpItsDatabase = "A database's job always backs up its database: change when, where or how long its backups are kept."
	for _, body := range []map[string]any{
		{"source_kind": schema.SourceVolume}, {"source": "blog"}, {"host": other.Id}, {"stack": "somestack"},
		{"service": "db"}, {"engine": schema.DumpMySQL},
	} {
		if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+job.Id, f.admin, body); status != http.StatusBadRequest ||
			out["message"] != backsUpItsDatabase {
			t.Errorf("change %v: %d %v", body, status, out)
		}
	}
	// the same values are no change
	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+job.Id, f.admin, map[string]any{"source_kind": schema.SourceDatabase,
		"source": "shop", "host": f.host.Id, "engine": schema.DumpPostgres, "name": "shop's dump", "enabled": false}); status != http.StatusOK ||
		out["name"] != "shop's dump" || out["enabled"] != false {
		t.Errorf("a change of its name: %d %v", status, out)
	}
	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+job.Id, f.admin, map[string]any{"follow": []string{"name"}}); status != http.StatusBadRequest ||
		out["message"] != "A job follows its server's schedule, targets or retention." {
		t.Errorf("follow a name: %d %v", status, out)
	}
	manual := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+manual.Id, f.admin, map[string]any{"follow": []string{"schedule"}}); status != http.StatusBadRequest ||
		out["message"] != "Only a database's or a site's job follows its server's settings." {
		t.Errorf("follow on a manual job: %d %v", status, out)
	}
	if status, out := f.send(http.MethodDelete, "/api/pc/backup-jobs/"+job.Id, f.admin, nil); status != http.StatusConflict ||
		out["message"] != "It would come back while its database exists: turn it off instead." {
		t.Errorf("remove it: %d %v", status, out)
	}

	// a site's job says so of its site
	site := siteRecord(t, f.app, f.host, "wp", schema.SitePHPApp)
	files := f.job("wp-files", map[string]any{"source_kind": schema.SourceSite, "source": "wp", "site": site.Id, "origin": schema.JobSite})
	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+files.Id, f.admin, map[string]any{"source": "blog"}); status != http.StatusBadRequest ||
		out["message"] != "A site's job always backs up its site: change when, where or how long its backups are kept." {
		t.Errorf("change a site's job: %d %v", status, out)
	}
}
