package panel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// dbPreview is an agent's preview of a database restore: the dump in the
// snapshot, the database as found, and the room on the server.
func dbPreview(t *testing.T, file string, signIn bool, free, needed int64) map[string]any {
	t.Helper()
	preview, err := backup.ResultMap(backup.DbRestorePreview{
		DryRun:     true,
		Snapshot:   backup.PreviewSnapshot{ID: snapshotID, Time: "2026-11-18T01:30:00Z", Paths: []string{"/" + file}},
		Roots:      []backup.PreviewRoot{{Source: "/" + file, Destination: "database shop", InSnapshot: true, Exists: true, Entries: 12}},
		WouldStop:  []backup.WouldStop{{Name: "shop-web-1", Project: "shop", Service: "web"}},
		TotalFiles: 1, TotalBytes: 1 << 20,
		Database: backup.DbPreview{Exists: true, UserExists: true, SizeBytes: 8 << 20, Tables: 12, SignIn: signIn},
		Free:     backup.Space{Data: free, Tmp: free},
		Needed:   backup.Space{Data: needed, Tmp: needed},
		Notes:    []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return preview
}

// draft asks for a database restore's draft as an operator, answers its
// preview as the agent, and is the draft as kept.
func (f *dbFixture) draft(stream *commands.Stream, token string, body map[string]any, preview map[string]any) *core.Record {
	f.t.Helper()
	all := map[string]any{"target": f.target.Id, "snapshot": snapshotID, "mode": schema.RestoreDatabase}
	for key, value := range body {
		all[key] = value
	}
	status, answer := f.send(http.MethodPost, "/api/pc/backup-jobs/"+f.job.Id+"/restores", token, all)
	if status != http.StatusAccepted {
		f.t.Fatalf("the draft: %d %v", status, answer)
	}
	command := f.receivedOf(stream, "backup.restore")
	f.answer(f.host, command.GetId(), agentv1.CommandState_COMMAND_STATE_SUCCEEDED, preview, "")
	restore := f.stored(schema.BackupRestores, answer["id"].(string))
	if restore.GetString("status") != schema.RestorePreviewed {
		f.t.Fatalf("the preview: %v", restore.FieldsData())
	}
	return restore
}

// receivedOf is the next command of a type an agent got, past those of other
// types, such as the numbers of a server a restore asks for.
func (f *jobFixture) receivedOf(stream *commands.Stream, commandType string) *agentv1.Command {
	f.t.Helper()
	for range 10 {
		command := f.received(stream)
		if commands.TypeOf(command) == commandType {
			return command
		}
	}
	f.t.Fatalf("the agent got no %s", commandType)
	return nil
}

// startDatabase starts a draft with what someone confirmed.
func (f *jobFixture) startDatabase(restore *core.Record, token string, confirm bool, name string) (int, map[string]any) {
	f.t.Helper()
	return f.send(http.MethodPost, "/api/pc/backup-restores/"+restore.Id+"/start", token, map[string]any{"confirm_overwrite": confirm, "confirm_name": name})
}

// TestADatabaseRestoreIsPlannedFromItsDatabase (#53, P5, AC1): back into a
// database, a restore goes to the database's server, restores as its own
// user, stops its stack, a site's PHP containers or the other stacks whose
// env names its secret, and makes a scratch copy and, on PostgreSQL, keeps
// the data before it; its preview gets the passwords as it is delivered, and
// stores none. Into a new database, a name of the rules, on the database's
// server or, for a dropped database, a chosen one.
func TestADatabaseRestoreIsPlannedFromItsDatabase(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "reports", "env": "DB_PASSWORD=${secret:db/shop}\n"})
	f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "blog", "env": "DB_PASSWORD=${secret:db/blog}\n"})
	stream := f.dbAgent()

	status, body := f.send(http.MethodPost, "/api/pc/backup-jobs/"+f.job.Id+"/restores", operator,
		map[string]any{"target": f.target.Id, "snapshot": snapshotID, "mode": schema.RestoreDatabase})
	if status != http.StatusAccepted {
		t.Fatalf("the draft: %d %v", status, body)
	}
	restore := f.stored(schema.BackupRestores, body["id"].(string))
	want := []restoreRoot{{Source: "/shop.pgdump", Container: "db-main-db-1", Engine: "postgres", AdminUser: "postgres", Database: "shop",
		User: "shop", Scratch: "pc_rs_" + restore.Id, Previous: "pc_old_" + restore.Id, StopProjects: []string{"reports"}}}
	if !reflect.DeepEqual(rootsOf(restore), want) || restore.GetString("database") != f.database.Id || restore.GetString("database_name") != "shop" ||
		restore.GetString("stack_name") != "shop" || restore.GetString("label") != "db-main-shop ← b2 · 3f9a1c2e → database shop" {
		t.Errorf("the draft: %v", restore.FieldsData())
	}
	preview := f.received(stream)
	database := preview.GetBackupRestore().GetDatabase()
	if !preview.GetBackupRestore().GetDryRun() || preview.GetBackupRestore().GetStack() != "shop" || database.GetMode() != backup.DbRestoreReplace ||
		database.GetFile() != "shop.pgdump" || database.GetConnection().GetAdminPassword() != adminSecretPG || database.GetPassword() != userSecretShop ||
		preview.GetBackupRestore().GetRepository().GetPassword() != "repo-password-b2" {
		t.Errorf("the preview delivered: %v", preview)
	}
	stored := f.stored(schema.Commands, preview.GetId())
	if raw := mustString(t, stored.Get("payload")); strings.Contains(raw, adminSecretPG) || strings.Contains(raw, userSecretShop) || stored.GetString("resource") != "" {
		t.Errorf("the preview stored: %s %q", raw, stored.GetString("resource"))
	}

	// a site's database stops the site's PHP containers by name
	site := f.phpSite("wp")
	wpDatabase, _ := f.addDatabase(f.server, "wp", "user-secret-wp", map[string]any{"owner_kind": "site", "site": site.Id})
	wpJob := f.dbJob(wpDatabase)
	status, body = f.send(http.MethodPost, "/api/pc/backup-jobs/"+wpJob.Id+"/restores", operator,
		map[string]any{"target": f.target.Id, "snapshot": snapshotID, "mode": schema.RestoreDatabase})
	if status != http.StatusAccepted {
		t.Fatalf("the site's draft: %d %v", status, body)
	}
	wp := f.stored(schema.BackupRestores, body["id"].(string))
	if roots := rootsOf(wp); len(roots) != 1 || roots[0].Site != "wp" || !reflect.DeepEqual(roots[0].StopContainers, []string{"wp-php", "wp-worker"}) ||
		wp.GetString("stack_name") != "" || roots[0].StopProjects != nil {
		t.Errorf("the site's draft: %v", wp.FieldsData())
	}
	f.received(stream)

	// into a new database
	create := func(job *core.Record, body map[string]any) (int, map[string]any) {
		t.Helper()
		all := map[string]any{"target": f.target.Id, "snapshot": snapshotID, "mode": schema.RestoreNewDatabase}
		for key, value := range body {
			all[key] = value
		}
		return f.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/restores", operator, all)
	}
	for name, message := range map[string]string{
		"pc_rt_aaaaaaaaaaaaaaa": databases.ReservedPrefixProblem,
		"Shop":                  `"Shop" is not a valid database name: start with a letter and use lower case letters, digits and underscores, at most 63.`,
	} {
		if status, body := create(f.job, map[string]any{"name": name}); status != http.StatusBadRequest || body["message"] != message {
			t.Errorf("a new database %s: %d %v", name, status, body["message"])
		}
	}
	if status, body := create(f.job, map[string]any{"name": "wp"}); status != http.StatusConflict || body["message"] != "A database named wp exists on db-main already." {
		t.Errorf("a name taken: %d %v", status, body["message"])
	}
	status, body = create(f.job, map[string]any{"name": "shop_copy"})
	if status != http.StatusAccepted {
		t.Fatalf("a new database: %d %v", status, body)
	}
	copied := f.stored(schema.BackupRestores, body["id"].(string))
	if roots := rootsOf(copied); !reflect.DeepEqual(roots, []restoreRoot{{Source: "/shop.pgdump", Container: "db-main-db-1", Engine: "postgres",
		AdminUser: "postgres", Database: "shop_copy", User: "shop_copy"}}) || copied.GetString("database") != "" || copied.GetString("stack_name") != "" {
		t.Errorf("a new database's draft: %v", copied.FieldsData())
	}
	preview = f.received(stream)
	if database := preview.GetBackupRestore().GetDatabase(); database.GetMode() != backup.DbRestoreNew || database.GetPassword() != "" ||
		database.GetConnection().GetAdminPassword() != adminSecretPG {
		t.Errorf("a new database's preview: %v", preview)
	}
	if status, body := create(f.job, map[string]any{"name": "shop_copy"}); status != http.StatusAccepted {
		t.Errorf("a second draft of a name: %d %v", status, body["message"])
	} else {
		f.received(stream)
	}

	// a dropped database goes into a new one, on a chosen server of its
	// engine
	f.job.Set("database", "")
	if err := f.app.Save(f.job); err != nil {
		t.Fatal(err)
	}
	if status, body := create(f.job, map[string]any{"mode": schema.RestoreDatabase}); status != http.StatusConflict ||
		body["message"] != "The database was dropped: restore it into a new database." {
		t.Errorf("back into a dropped database: %d %v", status, body["message"])
	}
	if status, body := create(f.job, map[string]any{"name": "shop"}); status != http.StatusBadRequest || body["message"] != "Choose the database server the new database goes on." {
		t.Errorf("no server: %d %v", status, body["message"])
	}
	maria := f.sharedServer("maria", databases.MariaDB, "mariadb:11.8", adminSecretMaria)
	if status, body := create(f.job, map[string]any{"name": "shop", "server": maria.Id}); status != http.StatusBadRequest ||
		body["message"] != "The server maria runs another engine than the job's dumps are of." {
		t.Errorf("another engine: %d %v", status, body["message"])
	}
	f.server.Set("version", "17.2")
	if err := f.app.Save(f.server); err != nil {
		t.Fatal(err)
	}
	old := f.sharedServer("pg16", databases.Postgres, "postgres:16", "admin-secret-16")
	if status, body := create(f.job, map[string]any{"name": "shop_restored", "server": old.Id}); status != http.StatusBadRequest ||
		body["message"] != "The server pg16 runs PostgreSQL 16, older than the 17 the backup was made with: restore into a server of the same or a newer version." {
		t.Errorf("an older server: %d %v", status, body["message"])
	}
	newer := f.sharedServer("pg18", databases.Postgres, "postgres:18", "admin-secret-18")
	// a database's name is its host's only, as its secret is found by it
	if status, body := create(f.job, map[string]any{"name": "shop", "server": newer.Id}); status != http.StatusConflict ||
		body["message"] != "A database named shop exists on db-main already." {
		t.Errorf("a name of another server of the host: %d %v", status, body["message"])
	}
	if status, body := create(f.job, map[string]any{"name": "shop_restored", "server": newer.Id}); status != http.StatusAccepted {
		t.Errorf("a dropped database into a new one: %d %v", status, body["message"])
	} else if restored := f.stored(schema.BackupRestores, body["id"].(string)); rootsOf(restored)[0].Container != "pg18-db-1" ||
		rootsOf(restored)[0].Source != "/shop.pgdump" {
		t.Errorf("its plan: %v", restored.FieldsData())
	}
}

// TestAMariaDBRestoreKeepsNoCopy (#53, D11): MariaDB's restore in place
// rehearses in a scratch copy and keeps no copy of the data before it; and a
// database whose user carries no secret of it, as one that came with a
// stack's own server, stops no other stack that names the secret, which is
// then not its.
func TestAMariaDBRestoreKeepsNoCopy(t *testing.T) {
	f := newDBFixture(t, databases.MariaDB.Name)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	f.dbAgent()
	f.setTest(f.user, map[string]any{"secret_name": ""})
	f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "reports", "env": "DB_PASSWORD=${secret:db/shop}\n"})
	status, body := f.send(http.MethodPost, "/api/pc/backup-jobs/"+f.job.Id+"/restores", operator,
		map[string]any{"target": f.target.Id, "snapshot": snapshotID, "mode": schema.RestoreDatabase})
	if status != http.StatusAccepted {
		t.Fatalf("the draft: %d %v", status, body)
	}
	restore := f.stored(schema.BackupRestores, body["id"].(string))
	want := []restoreRoot{{Source: "/shop.sql", Container: "maria-db-1", Engine: "mariadb", AdminUser: "root", Database: "shop", User: "shop",
		Scratch: "pc_rs_" + restore.Id}}
	if !reflect.DeepEqual(rootsOf(restore), want) {
		t.Errorf("the plan: %v", rootsOf(restore))
	}
}

// TestADatabaseRestoreIsRefusedAsItIsMade (#53, P5, P9): a database restore
// is refused with a sentence for a job of anything else, a database that is
// not ready, a rotation of its user's password, a restore or a restore test
// of it under way, a backup of the job running, a stack held by another
// restore, and an agent that cannot restore databases.
func TestADatabaseRestoreIsRefusedAsItIsMade(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	_, viewer := testutil.NewUser(t, f.app, "viewer@example.com", schema.RoleViewer)
	f.dbAgent()
	create := func(job *core.Record, token string) (int, string) {
		t.Helper()
		status, body := f.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/restores", token,
			map[string]any{"target": f.target.Id, "snapshot": snapshotID, "mode": schema.RestoreDatabase})
		message, _ := body["message"].(string)
		return status, message
	}
	expect := func(name string, wantStatus int, want string) {
		t.Helper()
		if status, message := create(f.job, operator); status != wantStatus || message != want {
			t.Errorf("%s: %d %q, not %d %q", name, status, message, wantStatus, want)
		}
	}
	if status, _ := create(f.job, viewer); status != http.StatusForbidden {
		t.Errorf("a viewer's restore: %d", status)
	}
	volume := f.jobFixture.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	if status, message := create(volume, operator); status != http.StatusBadRequest || message != "Only a database's job restores into a database." {
		t.Errorf("a volume's job: %d %q", status, message)
	}

	f.setTest(f.database, map[string]any{"status": dbCreating})
	expect("a database not ready", http.StatusConflict, "The database shop is not ready.")
	f.setTest(f.database, map[string]any{"status": dbReady})

	f.setTest(f.user, map[string]any{"pending_password": "the-next-one"})
	expect("a rotation", http.StatusConflict, "A rotation of shop's password is under way.")
	f.setTest(f.user, map[string]any{"pending_password": ""})

	open := f.restore(f.job, map[string]any{"mode": schema.RestoreNewDatabase, "status": schema.RestoreQueued,
		"roots": []restoreRoot{{Source: "/shop.pgdump", Container: "db-main-db-1", Database: "shop", User: "shop"}}})
	expect("a restore under way", http.StatusConflict, "A restore of shop is under way.")
	f.setTest(open, map[string]any{"status": schema.RestoreFailed})

	test := f.testRow(f.job, f.target, map[string]any{"status": schema.CheckRunning})
	expect("a test under way", http.StatusConflict, "A restore test of shop is under way.")
	f.setTest(test, map[string]any{"status": schema.CheckOK})

	run := f.save(schema.BackupRuns, map[string]any{"job": f.job.Id, "target": f.target.Id, "host": f.host.Id, "trigger": schema.RunManual, "status": schema.RunRunning})
	// the run's dump comes from the database's server, as a database run's
	// does: the delivery refuses one of another container
	runCommand := f.save(schema.Commands, map[string]any{"host": f.host.Id, "type": "backup.run",
		"payload": map[string]any{"run_id": run.Id, "repository": map[string]any{"target_id": f.target.Id, "url": "s3:x"},
			"stdin_container": f.server.GetString("container_ref")}})
	f.setTest(run, map[string]any{"command": runCommand.Id})
	expect("a backup running", http.StatusConflict, "A backup of db-main-shop is running: restore when it ends.")
	f.closeCommand(runCommand, schema.CommandFailed)
	f.setTest(run, map[string]any{"status": schema.RunFailed})

	f.restore(f.job, map[string]any{"mode": schema.RestoreOriginal, "status": schema.RestoreRunning, "stack_name": "shop",
		"roots": []restoreRoot{{Source: "/volumes/shop_data", Volume: "shop_data"}}})
	expect("a stack held", http.StatusConflict, restoreHoldMessage("shop"))
}

// TestADatabaseRestoreNeedsAnUpdatedAgent (#53, D17): an agent that does not
// say backup.database restores no database.
func TestADatabaseRestoreNeedsAnUpdatedAgent(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	f.dbAgent()
	f.carry(f.host, "backup.run", "backup.restore", "backup.dump")
	status, body := f.send(http.MethodPost, "/api/pc/backup-jobs/"+f.job.Id+"/restores", operator,
		map[string]any{"target": f.target.Id, "snapshot": snapshotID, "mode": schema.RestoreDatabase})
	if status != http.StatusConflict || body["message"] != "The agent of node-1 cannot restore databases yet: update it." {
		t.Errorf("an old agent: %d %v", status, body["message"])
	}
}

// TestADatabaseRestoreStartsWithItsNameTyped (#53, P5, AC1): a restore back
// into a database starts only with its name typed, from a preview that found
// the dump, signed in as its user and found room; it records its temporary
// copies with its command, which holds no password and gets them as it is
// delivered, orders with its stack, and is audited with the database and
// its server. A plan that changed since the preview is previewed again.
func TestADatabaseRestoreStartsWithItsNameTyped(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	stream := f.dbAgent()
	restore := f.draft(stream, operator, nil, dbPreview(t, "shop.pgdump", true, 1<<30, 1<<20))

	for _, attempt := range []struct {
		confirm bool
		name    string
	}{{false, ""}, {true, ""}, {true, "blog"}, {false, "shop"}} {
		if status, body := f.startDatabase(restore, operator, attempt.confirm, attempt.name); status != http.StatusConflict || body["message"] != confirmNameMessage {
			t.Errorf("%v: %d %v", attempt, status, body["message"])
		}
	}
	for name, preview := range map[string]map[string]any{
		"The panel's password of shop no longer signs in on db-main: restore into a new database.": dbPreview(t, "shop.pgdump", false, 1<<30, 1<<20),
		"The server db-main has 1.0 KiB free, and the restore needs about 2.0 KiB.":                dbPreview(t, "shop.pgdump", true, 1024, 2048),
	} {
		short := f.draft(stream, operator, nil, preview)
		if status, body := f.startDatabase(short, operator, true, "shop"); status != http.StatusConflict || body["message"] != name {
			t.Errorf("%s: %d %v", name, status, body["message"])
		}
	}
	missing := dbPreview(t, "shop.pgdump", true, 1<<30, 1<<20)
	missing["roots"].([]any)[0].(map[string]any)["in_snapshot"] = false
	if status, body := f.startDatabase(f.draft(stream, operator, nil, missing), operator, true, "shop"); status != http.StatusConflict ||
		body["message"] != "The dump is not in this snapshot." {
		t.Errorf("a dump that is not there: %d %v", status, body["message"])
	}

	// a stack that names the database's secret since the preview
	changed := f.draft(stream, operator, nil, dbPreview(t, "shop.pgdump", true, 1<<30, 1<<20))
	reports := f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "reports", "env": "DB_PASSWORD=${secret:db/shop}\n"})
	if status, body := f.startDatabase(changed, operator, true, "shop"); status != http.StatusConflict ||
		body["message"] != "What the restore writes or stops changed since its preview: preview it again." {
		t.Errorf("a plan that changed: %d %v", status, body["message"])
	}
	if err := f.app.Delete(reports); err != nil {
		t.Fatal(err)
	}

	status, body := f.startDatabase(restore, operator, true, "shop")
	if status != http.StatusOK || body["status"] != schema.RestoreQueued || body["overwrite"] != true {
		t.Fatalf("started: %d %v", status, body)
	}
	for _, name := range []string{"pc_rs_" + restore.Id, "pc_old_" + restore.Id} {
		copied, err := f.p.databaseOf(f.server, name)
		if err != nil || copied.GetString("status") != dbCreating || copied.GetString("owner_kind") != "none" {
			t.Errorf("the copy %s: %v %v", name, copied, err)
			continue
		}
		if left := time.Until(copied.GetDateTime("temporary_until").Time()); left < restoreCopyLife-time.Minute || left > restoreCopyLife {
			t.Errorf("the copy %s goes in %s", name, left)
		}
	}
	started := f.stored(schema.BackupRestores, restore.Id)
	command := f.stored(schema.Commands, started.GetString("command"))
	if raw := mustString(t, command.Get("payload")); strings.Contains(raw, adminSecretPG) || strings.Contains(raw, userSecretShop) ||
		command.GetString("resource") != "stack:shop" {
		t.Errorf("the restore stored: %s %q", raw, command.GetString("resource"))
	}
	delivered := f.receivedOf(stream, "backup.restore")
	database := delivered.GetBackupRestore().GetDatabase()
	if delivered.GetId() != command.Id || delivered.GetBackupRestore().GetDryRun() || !delivered.GetBackupRestore().GetOverwrite() ||
		!reflect.DeepEqual(delivered.GetBackupRestore().GetConfirmedStops(), []string{"shop-web-1"}) || database.GetScratch() != "pc_rs_"+restore.Id ||
		database.GetPrevious() != "pc_old_"+restore.Id || database.GetPassword() != userSecretShop || database.GetConnection().GetAdminPassword() != adminSecretPG {
		t.Errorf("the restore delivered: %v", delivered)
	}
	entries, err := f.app.FindRecordsByFilter(schema.AuditLog, "action={:action}", "", 0, 0, map[string]any{"action": audit.ActionBackupRestore})
	if err != nil || len(entries) != 1 || !strings.Contains(entries[0].GetString("snapshot"), `"database":"shop"`) ||
		!strings.Contains(entries[0].GetString("snapshot"), `"server":"db-main"`) {
		t.Errorf("audited: %v %v", entries, err)
	}
}

// TestARestoreIntoANewDatabaseMakesItsRecords (#53, P5): a restore into a
// new database records the database, creating, and its user, with a new
// password and its secret, in the transaction that queues it; the restore
// restores as that user, and the database is ready once it succeeded, or
// goes with its user when the agent dropped it again.
func TestARestoreIntoANewDatabaseMakesItsRecords(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	stream := f.dbAgent()
	fresh := dbPreview(t, "shop.pgdump", false, 1<<30, 1<<20)
	fresh["database"].(map[string]any)["exists"] = false
	fresh["database"].(map[string]any)["user_exists"] = false

	exists := f.draft(stream, operator, map[string]any{"mode": schema.RestoreNewDatabase, "name": "shop_copy"}, dbPreview(t, "shop.pgdump", false, 1<<30, 1<<20))
	if status, body := f.startDatabase(exists, operator, false, ""); status != http.StatusConflict || body["message"] != "A database named shop_copy exists on db-main already." {
		t.Errorf("a name the server has: %d %v", status, body["message"])
	}

	restore := f.draft(stream, operator, map[string]any{"mode": schema.RestoreNewDatabase, "name": "shop_copy"}, fresh)
	status, body := f.startDatabase(restore, operator, false, "")
	if status != http.StatusOK || body["overwrite"] != false {
		t.Fatalf("started: %d %v", status, body)
	}
	database, err := f.p.databaseOf(f.server, "shop_copy")
	if err != nil || database.GetString("status") != dbCreating || database.GetString("owner_kind") != "none" || !database.GetDateTime("temporary_until").IsZero() {
		t.Fatalf("the new database: %v %v", database, err)
	}
	user, err := f.p.userOf(f.server, "shop_copy")
	if err != nil || user.GetString("secret_name") != "db/shop_copy" || !reflect.DeepEqual(user.GetStringSlice("databases"), []string{database.Id}) {
		t.Fatalf("its user: %v %v", user, err)
	}
	password, _ := f.p.sealed(user, "password")
	started := f.stored(schema.BackupRestores, restore.Id)
	if started.GetString("database") != database.Id {
		t.Errorf("the restore: %v", started.FieldsData())
	}
	delivered := f.receivedOf(stream, "backup.restore")
	if db := delivered.GetBackupRestore().GetDatabase(); db.GetMode() != backup.DbRestoreNew || db.GetUser() != "shop_copy" || password == "" ||
		db.GetPassword() != password || delivered.GetBackupRestore().GetOverwrite() {
		t.Errorf("delivered: %v", delivered)
	}
	if command := f.stored(schema.Commands, delivered.GetId()); command.GetString("resource") != "db:db-main-db-1/shop_copy" {
		t.Errorf("its ordering key: %q", command.GetString("resource"))
	}
	f.answer(f.host, delivered.GetId(), agentv1.CommandState_COMMAND_STATE_SUCCEEDED,
		map[string]any{"files_restored": 1, "bytes_restored": 1 << 20, "tables": 12, "tables_expected": 12}, "")
	if ready := f.fresh(database); ready.GetString("status") != dbReady {
		t.Errorf("after the restore: %v", ready.FieldsData())
	}
	if kept := f.fresh(restore); kept.GetString("status") != schema.RestoreSucceeded || kept.GetInt("tables") != 12 {
		t.Errorf("the restore kept: %v", kept.FieldsData())
	}

	// one that failed and was dropped again leaves no record
	again := f.draft(stream, operator, map[string]any{"mode": schema.RestoreNewDatabase, "name": "shop_two"}, fresh)
	if status, body := f.startDatabase(again, operator, false, ""); status != http.StatusOK {
		t.Fatalf("started again: %d %v", status, body)
	}
	delivered = f.receivedOf(stream, "backup.restore")
	secret := delivered.GetBackupRestore().GetDatabase().GetPassword()
	f.answer(f.host, delivered.GetId(), agentv1.CommandState_COMMAND_STATE_FAILED, map[string]any{"dropped": true},
		"pg_restore: error: could not execute query, with "+secret+" and "+adminSecretPG)
	if _, err := f.p.databaseOf(f.server, "shop_two"); err == nil {
		t.Error("a dropped new database kept its record")
	}
	if _, err := f.p.userOf(f.server, "shop_two"); err == nil {
		t.Error("a dropped new database kept its user")
	}
	if kept := f.fresh(again); kept.GetString("status") != schema.RestoreFailed || secret == "" || strings.Contains(kept.GetString("error"), secret) ||
		strings.Contains(kept.GetString("error"), adminSecretPG) {
		t.Errorf("the failed restore: %v", kept.FieldsData())
	}
}

// databaseRestore saves a started restore of the fixture's database, with its
// copies, and links its command.
func (f *dbFixture) databaseRestore(mode string, previous bool) (*core.Record, *core.Record) {
	f.t.Helper()
	restore := f.restore(f.job, map[string]any{"mode": mode, "status": schema.RestoreRunning, "overwrite": true,
		"confirmed_label": "superuser root@example.com", "database": f.database.Id, "database_name": "shop", "stack_name": "shop"})
	root := restoreRoot{Source: "/shop.pgdump", Container: f.server.GetString("container_ref"), Engine: f.server.GetString("engine"),
		AdminUser: f.server.GetString("admin_user"), Database: "shop", User: "shop", Scratch: databases.ScratchName(restore.Id)}
	if previous {
		root.Previous = databases.PreviousName(restore.Id)
	}
	restore.Set("roots", []restoreRoot{root})
	if err := f.app.Save(restore); err != nil {
		f.t.Fatal(err)
	}
	until := time.Now().UTC().Add(restoreCopyLife)
	f.temporary(root.Scratch, until, map[string]any{"status": dbCreating})
	if previous {
		f.temporary(root.Previous, until, map[string]any{"status": dbCreating})
	}
	return restore, f.link(restore, false)
}

// TestADatabaseRestoreEndsWithItsCopies (#53, P5): after a restore in place
// succeeded, its scratch copy's record goes and the data before it stays a
// day; after one that failed before the swap, the scratch copy is swept and
// the copy that was never made goes; after a MariaDB restore that failed
// with the database emptied, the database says so, and the alert of its data
// is critical; one that never started leaves nothing; what is kept hides the
// database's passwords.
func TestADatabaseRestoreEndsWithItsCopies(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	finish := func(command *core.Record, status, message string, result map[string]any) {
		t.Helper()
		f.ended(command, status, message, result)
		ended := f.fresh(command)
		if result != nil {
			ended.Set("started_at", ended.GetDateTime("finished_at").Time().Add(-time.Minute))
		}
		f.p.restoreFinished(ended)
	}
	copyOf := func(name string) *core.Record {
		t.Helper()
		copied, err := f.p.databaseOf(f.server, name)
		if err != nil {
			return nil
		}
		return copied
	}

	restore, command := f.databaseRestore(schema.RestoreDatabase, true)
	finish(command, schema.CommandSucceeded, "", map[string]any{"files_restored": 1, "bytes_restored": 4096, "tables": 12, "tables_expected": 12,
		"previous": "pc_old_" + restore.Id, "log": "restored as shop with " + userSecretShop + " and " + adminSecretPG})
	kept := f.fresh(restore)
	if kept.GetString("status") != schema.RestoreSucceeded || kept.GetInt("tables") != 12 || kept.GetString("previous") != "pc_old_"+restore.Id ||
		strings.Contains(kept.GetString("log"), userSecretShop) || strings.Contains(kept.GetString("log"), adminSecretPG) {
		t.Errorf("the restore: %v", kept.FieldsData())
	}
	if copyOf("pc_rs_"+restore.Id) != nil {
		t.Error("the scratch copy's record stayed")
	}
	previous := copyOf("pc_old_" + restore.Id)
	if previous == nil || previous.GetString("status") != dbReady ||
		previous.GetDateTime("temporary_until").Time().Sub(f.fresh(command).GetDateTime("finished_at").Time()) != previousLife {
		t.Errorf("the data before the restore: %v", previous)
	}
	if events := f.eventsOf("backup.restored"); len(events) != 1 || !strings.HasSuffix(events[0].GetString("message"), "restored 12 tables, 4096 bytes.") {
		t.Errorf("the event: %v", events)
	}

	failed, command := f.databaseRestore(schema.RestoreDatabase, true)
	finish(command, schema.CommandFailed, "Nothing changed in shop: pg_restore: error: password "+userSecretShop, map[string]any{"files_restored": 0})
	if kept := f.fresh(failed); kept.GetString("status") != schema.RestoreFailed || strings.Contains(kept.GetString("error"), userSecretShop) {
		t.Errorf("the failed restore: %v", kept.FieldsData())
	}
	if scratch := copyOf("pc_rs_" + failed.Id); scratch == nil || scratch.GetString("status") != dbReady || time.Until(scratch.GetDateTime("temporary_until").Time()) > 0 {
		t.Errorf("the scratch copy of a failed restore: %v", scratch)
	}
	if copyOf("pc_old_"+failed.Id) != nil {
		t.Error("the copy a failed restore never made kept its record")
	}
	key := f.host.Id + ":database:" + f.database.Id
	if alert := f.openAlert(alertBackupRestore, key); alert == nil || alert.GetString("severity") != schema.SeverityCritical ||
		strings.Contains(alert.GetString("message"), userSecretShop) || !strings.Contains(alert.GetString("message"), "into db-main/shop failed") {
		t.Errorf("the alert: %v", alert)
	}

	never, command := f.databaseRestore(schema.RestoreDatabase, true)
	finish(command, schema.CommandCancelled, "Cancelled by someone", nil)
	if copyOf("pc_rs_"+never.Id) != nil || copyOf("pc_old_"+never.Id) != nil {
		t.Errorf("a restore that never started kept its copies: %v %v", f.fresh(never).FieldsData(), f.fresh(command).FieldsData())
	}
}

// TestAPartialRestoreSaysSoOnItsDatabase (#53, P5): a MariaDB restore in
// place that failed after the database was emptied leaves the database's
// error, and a critical alert.
func TestAPartialRestoreSaysSoOnItsDatabase(t *testing.T) {
	f := newDBFixture(t, databases.MariaDB.Name)
	restore, command := f.databaseRestore(schema.RestoreDatabase, false)
	message := "The restore failed after shop was emptied: it holds part of the backup, and shop-web-1 stay stopped. Restore again, then start them on their pages."
	f.ended(command, schema.CommandFailed, message, map[string]any{"partial": true, "app_left_stopped": []any{"shop-web-1"}})
	ended := f.fresh(command)
	ended.Set("started_at", time.Now().UTC().Add(-time.Minute))
	f.p.restoreFinished(ended)
	if kept := f.fresh(restore); kept.GetString("status") != schema.RestoreFailed || !kept.GetBool("partial") {
		t.Errorf("the restore: %v", kept.FieldsData())
	}
	if database := f.fresh(f.database); database.GetString("last_error") != message || database.GetString("status") != dbReady {
		t.Errorf("the database: %v", database.FieldsData())
	}
	if alert := f.openAlert(alertBackupRestore, f.host.Id+":database:"+f.database.Id); alert == nil || alert.GetString("severity") != schema.SeverityCritical {
		t.Errorf("the alert: %v", alert)
	}
}

// TestADroppedDatabaseTakesTheAlertsOfItsRestores (#53): the alert of a
// database's failed restore is keyed by the database's record, which its
// drop clears in the restore: the drop resolves it, as nothing would later,
// and leaves another database's alone. A restore into a new database that
// failed and dropped what it made has the key of the name on its server,
// which a restore into a new database of that name that went well resolves.
func TestADroppedDatabaseTakesTheAlertsOfItsRestores(t *testing.T) {
	f := newDBFixture(t, databases.MariaDB.Name)
	container := f.server.GetString("container_ref")
	root := func(name string) []restoreRoot {
		return []restoreRoot{{Source: "/shop.sql", Container: container, Engine: databases.MariaDB.Name,
			AdminUser: f.server.GetString("admin_user"), Database: name, User: name}}
	}
	failedNew := f.restore(f.job, map[string]any{"mode": schema.RestoreNewDatabase, "status": schema.RestoreFailed,
		"database_name": "shop_copy", "roots": root("shop_copy"),
		"error": "The restore into the new database shop_copy failed, and it was dropped again: no room."})
	f.p.noteRestore(failedNew, restoreResult{})
	byName := f.host.Id + ":database:" + container + "/shop_copy"
	if f.openAlert(alertBackupRestore, byName) == nil {
		t.Fatal("no alert of the failed restore into a new database")
	}
	copied, _ := f.addDatabase(f.server, "shop_copy", "copy-secret", nil)
	done := f.restore(f.job, map[string]any{"mode": schema.RestoreNewDatabase, "status": schema.RestoreSucceeded,
		"database": copied.Id, "database_name": "shop_copy", "roots": root("shop_copy")})
	f.p.noteRestore(done, restoreResult{})
	if alert := f.openAlert(alertBackupRestore, byName); alert != nil {
		t.Errorf("a restore into shop_copy went well, and the alert stays: %v", alert.FieldsData())
	}

	restore, command := f.databaseRestore(schema.RestoreDatabase, false)
	message := "The restore failed after shop was emptied: it holds part of the backup, and shop-web-1 stay stopped. Restore again, then start them on their pages."
	f.ended(command, schema.CommandFailed, message, map[string]any{"partial": true, "app_left_stopped": []any{"shop-web-1"}})
	ended := f.fresh(command)
	ended.Set("started_at", time.Now().UTC().Add(-time.Minute))
	f.p.restoreFinished(ended)
	key := f.host.Id + ":database:" + f.database.Id
	if f.openAlert(alertBackupRestore, key) == nil {
		t.Fatal("no alert of the failed restore")
	}
	otherKey := f.host.Id + ":database:" + copied.Id
	if err := events.RaiseAlert(f.app, alertBackupRestore, otherKey, f.host.Id, schema.SeverityCritical, "The restore of shop_copy failed."); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Delete(f.database); err != nil {
		t.Fatal(err)
	}
	if kept := f.fresh(restore); kept.GetString("database") != "" || restoreAlertKey(kept) == key {
		t.Errorf("the drop left the restore its database: %v", kept.FieldsData())
	}
	if alert := f.openAlert(alertBackupRestore, key); alert != nil {
		t.Errorf("shop was dropped, and the alert of its restore stays: %v", alert.FieldsData())
	}
	if f.openAlert(alertBackupRestore, otherKey) == nil {
		t.Error("the drop of shop resolved the alert of shop_copy's restore")
	}
}

// TestADatabaseRestoreIsCheckedAsItIsDelivered (#53, P4, AC1): a database
// restore gets no secret once its plan changed, nor once its database is
// elsewhere or has another user than the plan restores as.
func TestADatabaseRestoreIsCheckedAsItIsDelivered(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	restore, command := f.databaseRestore(schema.RestoreDatabase, true)
	f.setTest(restore, map[string]any{"status": schema.RestoreQueued})
	if delivered, err := f.deliver(command); err != nil || delivered.GetBackupRestore().GetDatabase().GetPassword() != userSecretShop {
		t.Fatalf("delivered: %v %v", delivered, err)
	}
	refused := func(name, reason string) {
		t.Helper()
		delivered, err := f.deliver(command)
		database := delivered.GetBackupRestore().GetDatabase()
		if err == nil || err.Error() != reason || database.GetPassword() != "" || database.GetConnection().GetAdminPassword() != "" ||
			delivered.GetBackupRestore().GetRepository().GetPassword() != "" {
			t.Errorf("%s: %v, not %q", name, err, reason)
		}
	}

	// the plan changed: another user
	roots := rootsOf(restore)
	roots[0].User = "blog"
	f.setTest(restore, map[string]any{"roots": roots})
	refused("another user", "the command differs from the restore that was confirmed")
	roots[0].User = "shop"
	f.setTest(restore, map[string]any{"roots": roots})

	// the database moved to another server's container
	f.setTest(f.server, map[string]any{"container_ref": "db-new-db-1"})
	refused("a moved database", "the database's server is gone")
	f.setTest(f.server, map[string]any{"container_ref": "db-main-db-1"})

	// another user owns the database now
	other := f.save(schema.DBUsers, map[string]any{"server": f.server.Id, "name": "owner", "password": "x", "databases": []string{f.database.Id}})
	if err := f.app.Delete(f.user); err != nil {
		t.Fatal(err)
	}
	refused("another owner", "the database's user is not the one the restore was planned with")
	_ = other
}

// TestARestoreOfADatabaseHoldsWhatUsesIt (#53, P5): while a restore back
// into a database is under way, a scheduled backup of its job is skipped,
// the deploys of its stack wait, and a site whose database it is neither
// deploys nor goes back to another release.
func TestARestoreOfADatabaseHoldsWhatUsesIt(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	restore, _ := f.databaseRestore(schema.RestoreDatabase, true)
	if writing := f.p.restoreWriting(f.job); writing == nil || writing.Id != restore.Id {
		t.Errorf("the restore that writes the job's data: %v", writing)
	}
	f.p.startJobRuns(f.job, schema.RunSchedule, time.Now().UTC().Truncate(time.Minute))
	runs := f.runsOf(f.job)
	if newest := runs[len(runs)-1]; newest.GetString("status") != schema.RunSkipped || !strings.HasPrefix(newest.GetString("error"), "A restore into its data is under way") {
		t.Errorf("a scheduled backup during the restore: %v", newest.FieldsData())
	}
	if holding := f.p.restoreHolding(f.stack); holding == nil || holding.Id != restore.Id {
		t.Errorf("the stack's hold: %v", holding)
	}
	other, _ := f.addDatabase(f.server, "blog", "user-secret-blog", nil)
	if writing := f.p.restoreWriting(f.dbJob(other)); writing != nil {
		t.Errorf("another database's job is held by %v", writing)
	}

	// a site's database
	site := f.phpSite("wp")
	f.restore(f.job, map[string]any{"mode": schema.RestoreDatabase, "status": schema.RestoreQueued,
		"roots": []restoreRoot{{Source: "/wp.pgdump", Container: "db-main-db-1", Database: "wp", User: "wp", Site: "wp",
			StopContainers: []string{"wp-php", "wp-worker"}}}})
	for _, path := range []string{"/deploy", "/rollback"} {
		status, body := f.send(http.MethodPost, "/api/pc/sites/"+site.Id+path, operator, map[string]any{"deployment": "x"})
		if status != http.StatusConflict || body["message"] != "The site wp is being restored from a backup: this waits until that is done." {
			t.Errorf("%s of the site: %d %v", path, status, body["message"])
		}
	}
	_, superuser := testutil.NewSuperuser(t, f.app)
	status, body := f.send(http.MethodPost, "/api/collections/commands/records", superuser, map[string]any{
		"host": f.host.Id, "type": "site.remove", "payload": map[string]any{"site": "wp"}})
	if status != http.StatusConflict || body["message"] != "The site wp is being restored from a backup: this waits until that is done." {
		t.Errorf("a site's command through the API: %d %v", status, body["message"])
	}
}

// TestAServerWaitsForARestoreInIt (#53, P5): while a restore of one of a
// shared server's databases, or a restore test, runs in the server's
// container, an update of the server, a deploy of its own stack, its stack's
// commands and its containers' commands are refused with the sentence of
// the server's removal, and nothing is queued that would recreate or stop
// the container; once they ended, the update goes ahead.
func TestAServerWaitsForARestoreInIt(t *testing.T) {
	f := newDBFixture(t, databases.MariaDB.Name)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	_, superuser := testutil.NewSuperuser(t, f.app)
	own := f.stored(schema.Stacks, f.server.GetString("stack"))
	f.setTest(own, map[string]any{"compose_yaml": "services:\n  db:\n    image: mariadb:11.8\n"})
	f.save(schema.Containers, map[string]any{"host": f.host.Id, "docker_id": "c-maria", "name": "maria-db-1", "project": "maria"})
	deploys := func() int {
		t.Helper()
		count, err := f.app.CountRecords(schema.Commands, dbx.HashExp{"type": "stack.deploy", "host": f.host.Id})
		if err != nil {
			t.Fatal(err)
		}
		return int(count)
	}
	update := "/api/pc/db-servers/" + f.server.Id + "/update"
	held := "A restore or a restore test on maria is under way: this waits until it ends."
	refused := func(step string) {
		t.Helper()
		before := deploys()
		if status, body := f.send(http.MethodPost, update, operator, nil); status != http.StatusConflict ||
			body["message"] != "A restore or a restore test on maria is under way: update the server once it ends." {
			t.Errorf("%s: the update: %d %v", step, status, body["message"])
		}
		if status, body := f.send(http.MethodPost, "/api/pc/stacks/"+own.Id+"/deploy", operator, map[string]any{}); status != http.StatusUnprocessableEntity || body["message"] != held {
			t.Errorf("%s: a deploy of its stack: %d %v", step, status, body["message"])
		}
		for _, command := range []map[string]any{
			{"type": "stack.restart", "payload": map[string]any{"name": "maria"}},
			{"type": "stack.down", "payload": map[string]any{"name": "maria"}},
			{"type": "container.restart", "payload": map[string]any{"docker_id": "c-maria"}},
			{"type": "container.stop", "payload": map[string]any{"docker_id": "c-maria"}},
		} {
			command["host"] = f.host.Id
			if status, body := f.send(http.MethodPost, "/api/collections/commands/records", superuser, command); status != http.StatusConflict || body["message"] != held {
				t.Errorf("%s: %s: %d %v", step, command["type"], status, body["message"])
			}
		}
		if deploys() != before {
			t.Errorf("%s: a deploy of the server was queued", step)
		}
	}

	// a restore back into shop
	restore, _ := f.databaseRestore(schema.RestoreDatabase, false)
	refused("a restore of shop")
	f.setTest(restore, map[string]any{"status": schema.RestoreSucceeded})

	// a restore test of shop, its copy on the server
	row := f.testRow(f.job, f.target, map[string]any{"status": schema.CheckRunning})
	f.setTest(row, map[string]any{"temporary": databases.TestDatabaseName(row.Id)})
	f.temporary(databases.TestDatabaseName(row.Id), time.Now().UTC().Add(time.Hour), map[string]any{"status": dbCreating})
	refused("a restore test of shop")
	f.setTest(row, map[string]any{"status": schema.CheckOK})

	// the update needs the server's agent, which the refusals do not ask for
	f.dbAgent()
	before := deploys()
	if status, body := f.send(http.MethodPost, update, operator, nil); status != http.StatusOK || deploys() != before+1 {
		t.Errorf("the update once they ended: %d %v", status, body["message"])
	}
}

// sqlDownloads is a download fixture of a database fixture: a served panel
// and the agent of the fixture's server.
func (f *dbFixture) sqlDownloads() *downloadFixture {
	f.t.Helper()
	server := httptest.NewServer(f.handler)
	f.t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	return &downloadFixture{jobFixture: f.jobFixture, server: server, stream: f.dbAgent(), job: f.job}
}

// TestADatabaseDumpDownloadsAsSQL (#53, P6): an admin downloads a database
// job's dump as SQL: PostgreSQL's through a conversion container of its
// server's image, on the server's host, MariaDB's as it is; under the
// database's name and the snapshot's time, as application/sql. Only a
// database's job does, only with an agent that converts, and only while the
// server is there; an operator does not download.
func TestADatabaseDumpDownloadsAsSQL(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	d := f.sqlDownloads()
	id, url := d.link(map[string]any{"kind": schema.DownloadSQL, "path": ""})
	restore := f.stored(schema.BackupRestores, id)
	name := restore.GetString("download_name")
	if restore.GetString("download_kind") != schema.DownloadSQL || restore.GetString("path") != "/shop.pgdump" ||
		!strings.HasPrefix(name, "shop-") || !strings.HasSuffix(name, ".sql") || len(name) != len("shop-20261118-0130.sql") ||
		len(rootsOf(restore)) != 1 || rootsOf(restore)[0].Container != "db-main-db-1" {
		t.Fatalf("the download: %v", restore.FieldsData())
	}
	answer := d.browse(context.Background(), url)
	command, up := d.started()
	if dump := command.GetBackupDump(); dump.GetSqlFrom() != "db-main-db-1" || dump.GetPath() != "/shop.pgdump" || dump.GetFileName() != name {
		t.Errorf("the dump: %v", command)
	}
	go func() {
		_, _ = up.writer.Write([]byte("-- PostgreSQL database dump\n"))
		_ = up.writer.Close()
	}()
	response := d.response(answer)
	got, _ := io.ReadAll(response.Body)
	if response.Header.Get("Content-Type") != "application/sql; charset=utf-8" || string(got) != "-- PostgreSQL database dump\n" ||
		!strings.Contains(response.Header.Get("Content-Disposition"), `filename="`+name+`"`) {
		t.Errorf("the SQL: %v %q", response.Header, got)
	}
	if _, err := up.result(t); err != nil {
		t.Errorf("the upload: %v", err)
	}

	request := func(token string, job *core.Record) (int, string) {
		t.Helper()
		status, body := f.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/downloads", token,
			map[string]any{"target": f.target.Id, "snapshot": snapshotID, "kind": schema.DownloadSQL})
		message, _ := body["message"].(string)
		return status, message
	}
	if status, _ := request(operator, f.job); status != http.StatusForbidden {
		t.Errorf("an operator's download: %d", status)
	}
	volume := f.jobFixture.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	if status, message := request(f.admin, volume); status != http.StatusBadRequest || message != "Only a database's job downloads as SQL." {
		t.Errorf("a volume's job: %d %q", status, message)
	}
	f.carry(f.host, "backup.run", "backup.dump")
	if status, message := request(f.admin, f.job); status != http.StatusConflict || message != "The agent of node-1 cannot convert dumps to SQL yet: update it." {
		t.Errorf("an old agent: %d %q", status, message)
	}
	f.carry(f.host, "backup.run", "backup.dump", backup.CapabilityDatabase)
	// a dropped database's server is found by its name, while it is there
	f.setTest(f.job, map[string]any{"database": ""})
	if status, message := request(f.admin, f.job); status != http.StatusCreated {
		t.Errorf("a dropped database's dump: %d %q", status, message)
	}
	f.setTest(f.job, map[string]any{"db_server_name": "gone"})
	if status, message := request(f.admin, f.job); status != http.StatusConflict || message != "The database's server is gone: download the dump as stored." {
		t.Errorf("a server that is gone: %d %q", status, message)
	}
}

// TestAMariaDBDumpDownloadsAsItIs (#53, P6): MariaDB's dump is SQL already,
// and downloads as it is, without a conversion.
func TestAMariaDBDumpDownloadsAsItIs(t *testing.T) {
	f := newDBFixture(t, databases.MariaDB.Name)
	d := f.sqlDownloads()
	id, url := d.link(map[string]any{"kind": schema.DownloadSQL, "path": ""})
	restore := f.stored(schema.BackupRestores, id)
	if restore.GetString("path") != "/shop.sql" || len(rootsOf(restore)) != 0 {
		t.Fatalf("the download: %v", restore.FieldsData())
	}
	answer := d.browse(context.Background(), url)
	command, up := d.started()
	if command.GetBackupDump().GetSqlFrom() != "" || command.GetBackupDump().GetPath() != "/shop.sql" {
		t.Errorf("the dump: %v", command)
	}
	_ = up.writer.Close()
	response := d.response(answer)
	if response.Header.Get("Content-Type") != "application/sql; charset=utf-8" {
		t.Errorf("the answer: %v", response.Header)
	}
}
