package panel

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// dbAgent connects the agent of the fixture's server, which runs every
// command of databases, backups and restores.
func (f *dbFixture) dbAgent() *commands.Stream {
	f.t.Helper()
	stream := f.p.Commands.Connect(f.host.Id, []string{
		"backup.snapshots", "backup.browse", "backup.restore", "backup.dump", "backup.run", "backup.restore_test",
		"db.database.discard", "db.database.drop", "db.database.create", "db.stats", "db.database.rotate",
	}, nil)
	f.t.Cleanup(func() { f.p.Commands.Disconnect(stream) })
	return stream
}

// temporary saves a temporary database of the fixture's server.
func (f *dbFixture) temporary(name string, until time.Time, fields map[string]any) *core.Record {
	f.t.Helper()
	all := map[string]any{"server": f.server.Id, "name": name, "status": dbReady, "owner_kind": "none", "temporary_until": until}
	for key, value := range fields {
		all[key] = value
	}
	return f.save(schema.Databases, all)
}

// discards are the queued discards of temporary databases, oldest first.
func (f *jobFixture) discards() []*core.Record {
	f.t.Helper()
	found, err := f.app.FindRecordsByFilter(schema.Commands, "type='db.database.discard'", "created,id", 0, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return found
}

// endDiscard ends a discard as its agent and the queue would.
func (f *jobFixture) endDiscard(command *core.Record, status, message string) {
	f.t.Helper()
	f.closeCommand(command, status)
	stored := f.fresh(command)
	stored.Load(map[string]any{"error": message, "finished_at": time.Now().UTC()})
	f.p.discardFinished(stored)
}

// TestTemporaryDatabasesGoAtTheirTime (#53, P8): once its time has come, a
// temporary database whose restore test or restore ended, or that has none,
// is marked dropping and discarded, with the user granted on it, by the
// panel, in one transaction; never one of a test or a restore still under
// way, one whose time has not come, one on a server that is offline, one
// being dropped, nor a database outside the namespace whatever its time.
func TestTemporaryDatabasesGoAtTheirTime(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	past, future := time.Now().UTC().Add(-time.Minute), time.Now().UTC().Add(time.Hour)
	ended := f.testRow(f.job, f.target, map[string]any{"status": schema.CheckOK})
	open := f.testRow(f.job, f.target, map[string]any{"status": schema.CheckRunning})
	running := f.restore(f.job, map[string]any{"mode": schema.RestoreDatabase, "status": schema.RestoreRunning,
		"roots": []restoreRoot{{Source: "/shop.pgdump", Container: "db-main-db-1", Database: "shop", User: "shop"}}})
	due := f.temporary(databases.TestDatabaseName(ended.Id), past, nil)
	testing := f.temporary(databases.TestDatabaseName(open.Id), past, nil)
	restoring := f.temporary(databases.ScratchName(running.Id), past, nil)
	later := f.temporary("pc_rt_bbbbbbbbbbbbbbb", future, nil)
	orphan := f.temporary("pc_old_ccccccccccccccc", past, nil)
	dropping := f.temporary("pc_rt_ddddddddddddddd", past, map[string]any{"status": dbDropping})
	outside := f.temporary("shop_old", past, nil)

	// nothing while the server is offline
	f.p.SweepTemporaries(time.Now())
	if found := f.discards(); len(found) != 0 {
		t.Fatalf("%d discards while the server is offline", len(found))
	}

	f.dbAgent()
	f.p.SweepTemporaries(time.Now())
	found := f.discards()
	if len(found) != 2 {
		t.Fatalf("%d discards, not 2", len(found))
	}
	byName := map[string]*core.Record{}
	for _, command := range found {
		payload := f.payloadOf(command.Id)
		names, _ := payload["names"].([]any)
		if len(names) != 1 || command.GetString("requested_label") != panelRequester || command.GetString("resource") != "db-test:db-main-db-1" ||
			strings.Contains(mustString(t, payload), adminSecretPG) {
			t.Errorf("a discard: %v %v", command.FieldsData(), payload)
			continue
		}
		byName[names[0].(string)] = command
	}
	if command := byName[due.GetString("name")]; command == nil || f.payloadOf(command.Id)["revoke_from"] != "shop" {
		t.Errorf("the ended test's copy: %v", command)
	}
	if command := byName[orphan.GetString("name")]; command == nil || f.payloadOf(command.Id)["revoke_from"] != nil {
		t.Errorf("a copy of nothing: %v", command)
	}
	for record, want := range map[*core.Record]string{due: dbDropping, orphan: dbDropping, testing: dbReady, restoring: dbReady,
		later: dbReady, dropping: dbDropping, outside: dbReady} {
		if got := f.fresh(record).GetString("status"); got != want {
			t.Errorf("%s: %s, not %s", record.GetString("name"), got, want)
		}
	}
	// once
	f.p.SweepTemporaries(time.Now())
	if again := f.discards(); len(again) != 2 {
		t.Errorf("%d discards after a second pass", len(again))
	}

	// the discard dropped it: the record goes, with an event
	f.endDiscard(byName[due.GetString("name")], schema.CommandSucceeded, "")
	if _, err := f.app.FindRecordById(schema.Databases, due.Id); err == nil {
		t.Error("a dropped temporary database kept its record")
	}
	if events := f.eventsOf(backup.EventTemporaryDropped); len(events) != 1 || events[0].GetString("message") != "The temporary database "+due.GetString("name")+" on db-main was dropped." {
		t.Errorf("the events: %v", events)
	}
}

// mustString is a value as JSON text.
func mustString(t *testing.T, value any) string {
	t.Helper()
	return string(mustJSON(t, value))
}

// TestADiscardThatFailsIsTriedAgain (#53, P8): a discard that failed leaves
// the temporary database ready, with the reason, to be tried again in ten
// minutes; a second failure raises db_temporary_left, which goes with the
// record once a discard dropped it.
func TestADiscardThatFailsIsTriedAgain(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	f.dbAgent()
	copied := f.temporary("pc_rt_aaaaaaaaaaaaaaa", time.Now().UTC().Add(-time.Minute), nil)
	f.p.SweepTemporaries(time.Now())
	first := f.discards()
	if len(first) != 1 {
		t.Fatalf("%d discards", len(first))
	}
	f.endDiscard(first[0], schema.CommandFailed, "ERROR: database is being accessed by other users")
	kept := f.fresh(copied)
	if kept.GetString("status") != dbReady || kept.GetString("last_error") != "The temporary database was not dropped: ERROR: database is being accessed by other users." ||
		time.Until(kept.GetDateTime("temporary_until").Time()) < 9*time.Minute || time.Until(kept.GetDateTime("temporary_until").Time()) > discardAgain {
		t.Errorf("after a failure: %v", kept.FieldsData())
	}
	if alert := f.openAlert(backup.AlertTemporaryLeft, copied.Id); alert != nil {
		t.Errorf("an alert after one failure: %v", alert.FieldsData())
	}

	f.p.SweepTemporaries(time.Now().Add(11 * time.Minute))
	second := f.discards()
	if len(second) != 2 {
		t.Fatalf("%d discards after ten minutes", len(second))
	}
	f.endDiscard(second[1], schema.CommandTimedOut, "")
	alert := f.openAlert(backup.AlertTemporaryLeft, copied.Id)
	if alert == nil || alert.GetString("severity") != schema.SeverityWarning || alert.GetString("host") != f.host.Id ||
		alert.GetString("message") != "The temporary database pc_rt_aaaaaaaaaaaaaaa on db-main could not be dropped: The drop did not finish." {
		t.Fatalf("the alert after a second failure: %v", alert)
	}

	f.p.SweepTemporaries(time.Now().Add(22 * time.Minute))
	third := f.discards()
	if len(third) != 3 {
		t.Fatalf("%d discards", len(third))
	}
	f.endDiscard(third[2], schema.CommandSucceeded, "")
	if alert := f.openAlert(backup.AlertTemporaryLeft, copied.Id); alert != nil {
		t.Errorf("the alert stayed after the drop: %v", alert.FieldsData())
	}

	// a discard that names a database outside the namespace, as one made by
	// hand, removes no record of it
	forged := f.save(schema.Commands, map[string]any{"host": f.host.Id, "type": "db.database.discard",
		"payload": map[string]any{"connection": map[string]any{"container_id": "db-main-db-1"}, "names": []string{"shop"}}})
	f.endDiscard(forged, schema.CommandSucceeded, "")
	if _, err := f.app.FindRecordById(schema.Databases, f.database.Id); err != nil {
		t.Errorf("a discard removed the record of shop: %v", err)
	}
}

// TestTemporaryOrphansFromStats (#53, P8): a database of the namespace that
// a shared server reports and the panel has no record of becomes one, ready,
// owned by nothing, to be dropped in two hours, with an event; a dedicated
// server's names of the namespace are never listed as its stack's.
func TestTemporaryOrphansFromStats(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	stats := func(server *core.Record, names ...string) {
		t.Helper()
		reported := map[string]any{}
		for _, name := range names {
			reported[name] = map[string]any{"size_bytes": 1024, "connections": 0}
		}
		command := f.save(schema.Commands, map[string]any{"host": f.host.Id, "type": "db.stats",
			"payload": map[string]any{"connection": map[string]any{"container_id": server.GetString("container_ref")}},
			"result":  map[string]any{"version": "17.2", "databases": reported}})
		f.closeCommand(command, schema.CommandSucceeded)
		f.p.databaseStatsFinished(f.fresh(command))
	}
	stats(f.server, "shop", "pc_rt_aaaaaaaaaaaaaaa", "stray", "pc_rt_short")
	found, err := f.p.databaseOf(f.server, "pc_rt_aaaaaaaaaaaaaaa")
	if err != nil || found.GetString("status") != dbReady || found.GetString("owner_kind") != "none" || found.GetInt("size_bytes") != 1024 {
		t.Fatalf("the orphan: %v %v", found, err)
	}
	if left := time.Until(found.GetDateTime("temporary_until").Time()); left < 119*time.Minute || left > orphanLife {
		t.Errorf("dropped in %s", left)
	}
	for _, name := range []string{"stray", "pc_rt_short"} {
		if _, err := f.p.databaseOf(f.server, name); err == nil {
			t.Errorf("%s got a record", name)
		}
	}
	if events := f.eventsOf(backup.EventTemporaryFound); len(events) != 1 || !strings.HasPrefix(events[0].GetString("message"),
		"The server db-main holds the temporary database pc_rt_aaaaaaaaaaaaaaa, which the panel had no record of: it is dropped at ") {
		t.Errorf("the events: %v", events)
	}
	// once
	stats(f.server, "shop", "pc_rt_aaaaaaaaaaaaaaa")
	if events := f.eventsOf(backup.EventTemporaryFound); len(events) != 1 {
		t.Errorf("%d events after a second report", len(events))
	}

	dedicated := f.save(schema.DBServers, map[string]any{"host": f.host.Id, "name": "app-db-1", "engine": databases.Postgres.Name,
		"kind": schema.DBServerDedicated, "image": "postgres:17", "container_ref": "app-db-1", "admin_user": "postgres",
		"admin_password": "dedicated-secret", "status": dbReady, "stack": f.stack.Id})
	stats(dedicated, "app", "pc_old_aaaaaaaaaaaaaaa")
	if _, err := f.p.databaseOf(dedicated, "app"); err != nil {
		t.Errorf("the dedicated server's database: %v", err)
	}
	if _, err := f.p.databaseOf(dedicated, "pc_old_aaaaaaaaaaaaaaa"); err == nil {
		t.Error("a dedicated server listed a temporary name as its stack's")
	}
}

// TestNewDatabasesKeepOutOfTheNamespace (#53, P8): a new database may not
// take a name that starts with a prefix of the temporary namespace.
func TestNewDatabasesKeepOutOfTheNamespace(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	for _, name := range []string{"pc_rt_aaaaaaaaaaaaaaa", "pc_rs_x", "pc_old_shop"} {
		status, body := f.send(http.MethodPost, "/api/pc/databases", operator, map[string]any{"server": f.server.Id, "name": name})
		if status != http.StatusBadRequest || body["message"] != databases.ReservedPrefixProblem {
			t.Errorf("%s: %d %v", name, status, body["message"])
		}
	}
	status, body := f.send(http.MethodPost, "/api/pc/databases", operator, map[string]any{"server": f.server.Id, "name": "pc_shop"})
	if status != http.StatusOK {
		t.Errorf("a name near the namespace: %d %v", status, body["message"])
	}
}

// TestTemporaryDatabasesAreDroppedOnly (#53, P8): an operator drops a
// temporary database at once, 202 and its discard, unless its restore test
// or restore still runs; nothing else is done with it; and a server whose
// databases are all temporary is removed, but not while a restore or a
// restore test on it is under way.
func TestTemporaryDatabasesAreDroppedOnly(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	ended := f.testRow(f.job, f.target, map[string]any{"status": schema.CheckFailed})
	open := f.testRow(f.job, f.target, map[string]any{"status": schema.CheckQueued})
	running := f.restore(f.job, map[string]any{"mode": schema.RestoreDatabase, "status": schema.RestoreQueued,
		"roots": []restoreRoot{{Source: "/shop.pgdump", Container: "db-main-db-1", Database: "shop", User: "shop"}}})
	later := time.Now().UTC().Add(time.Hour)
	copied := f.temporary(databases.TestDatabaseName(ended.Id), later, nil)
	testing := f.temporary(databases.TestDatabaseName(open.Id), later, nil)
	scratch := f.temporary(databases.ScratchName(running.Id), later, nil)

	for record, want := range map[*core.Record]string{testing: "Its restore test is still running.", scratch: "Its restore is still running."} {
		status, body := f.send(http.MethodDelete, "/api/pc/databases/"+record.Id, operator, nil)
		if status != http.StatusConflict || body["message"] != want {
			t.Errorf("%s: %d %v", record.GetString("name"), status, body["message"])
		}
	}
	for _, path := range []string{"/connect", "/rotate", "/clone"} {
		status, body := f.send(http.MethodPost, "/api/pc/databases/"+copied.Id+path, operator, map[string]any{"stack": f.stack.Id, "name": "copy"})
		if status != http.StatusConflict || body["message"] != temporaryMessage {
			t.Errorf("%s of a temporary database: %d %v", path, status, body["message"])
		}
	}
	for _, path := range []string{"/export", "/adminer"} {
		status, body := f.send(http.MethodPost, "/api/pc/databases/"+copied.Id+path, f.admin, nil)
		if status != http.StatusConflict || body["message"] != temporaryMessage {
			t.Errorf("%s of a temporary database: %d %v", path, status, body["message"])
		}
	}
	status, body := f.send(http.MethodDelete, "/api/pc/databases/"+copied.Id, operator, nil)
	if status != http.StatusAccepted || f.fresh(copied).GetString("status") != dbDropping {
		t.Fatalf("the drop of a temporary database: %d %v", status, body)
	}
	command, _ := body["command"].(map[string]any)
	if found := f.discards(); len(found) != 1 || command == nil || found[0].Id != command["id"] || found[0].GetString("requested_label") != "operator@example.com" {
		t.Errorf("its discard: %v %v", found, body)
	}
	status, body = f.send(http.MethodDelete, "/api/pc/databases/"+copied.Id, operator, nil)
	if status != http.StatusBadRequest || body["message"] != "The database is being dropped." {
		t.Errorf("dropped again: %d %v", status, body["message"])
	}

	// the server holds only temporary databases once shop goes, but its
	// restore and its test are under way
	if err := f.app.Delete(f.user); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Delete(f.database); err != nil {
		t.Fatal(err)
	}
	status, body = f.send(http.MethodDelete, "/api/pc/db-servers/"+f.server.Id, operator, nil)
	if status != http.StatusConflict || body["message"] != "A restore or a restore test on db-main is under way: remove the server once it ends." {
		t.Errorf("the server's removal while a restore runs: %d %v", status, body["message"])
	}
	f.setTest(open, map[string]any{"status": schema.CheckCancelled})
	f.setTest(running, map[string]any{"status": schema.RestoreCancelled})
	status, body = f.send(http.MethodDelete, "/api/pc/db-servers/"+f.server.Id, operator, nil)
	if status != http.StatusNoContent {
		t.Errorf("the removal of a server of temporary databases only: %d %v", status, body["message"])
	}
}

// copiesOfShop saves temporary copies of the fixture's database shop: the
// data a restore in place replaced, and an ended test's copy; and of another
// database, and one of nothing.
func (f *dbFixture) copiesOfShop() (previous, test, others []*core.Record) {
	f.t.Helper()
	restored := f.restore(f.job, map[string]any{"mode": schema.RestoreDatabase, "status": schema.RestoreSucceeded,
		"roots": []restoreRoot{{Source: "/shop.pgdump", Container: "db-main-db-1", Database: "shop", User: "shop"}}})
	ended := f.testRow(f.job, f.target, map[string]any{"status": schema.CheckOK})
	blog, _ := f.addDatabase(f.server, "blog", "user-secret-blog", nil)
	blogJob := f.dbJob(blog)
	blogTest := f.testRow(blogJob, f.target, map[string]any{"status": schema.CheckOK, "database_name": "blog"})
	later := time.Now().UTC().Add(20 * time.Hour)
	previous = []*core.Record{f.temporary(databases.PreviousName(restored.Id), later, nil)}
	test = []*core.Record{f.temporary(databases.TestDatabaseName(ended.Id), later.Add(-time.Hour), nil)}
	others = []*core.Record{
		f.temporary(databases.TestDatabaseName(blogTest.Id), later, nil),
		f.temporary("pc_rt_eeeeeeeeeeeeeee", later, nil),
	}
	return previous, test, others
}

// TestADropTakesItsTemporaryCopies (#53, P5): a database's drop drops its
// temporary copies first, as its user owns them, marking them dropping; they
// go once the drop succeeded, and are back to be swept when it failed. The
// copies of another database, or of nothing, stay.
func TestADropTakesItsTemporaryCopies(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	previous, test, others := f.copiesOfShop()
	copies := append(append([]*core.Record{}, previous...), test...)
	drop := func() *core.Record {
		t.Helper()
		status, body := f.send(http.MethodDelete, "/api/pc/databases/"+f.database.Id, operator, nil)
		if status != http.StatusOK {
			t.Fatalf("the drop: %d %v", status, body)
		}
		command := f.stored(schema.Commands, body["id"].(string))
		payload := f.payloadOf(command.Id)
		temporaries, _ := payload["temporaries"].([]any)
		if len(temporaries) != 2 || temporaries[0] != previous[0].GetString("name") || temporaries[1] != test[0].GetString("name") {
			t.Errorf("the drop's temporaries: %v", payload)
		}
		for _, copied := range copies {
			if f.fresh(copied).GetString("status") != dbDropping {
				t.Errorf("%s is not dropping", copied.GetString("name"))
			}
		}
		return command
	}

	failed := drop()
	f.closeCommand(failed, schema.CommandFailed)
	f.p.databaseDropFinished(f.fresh(failed))
	for _, copied := range append(copies, others...) {
		if f.fresh(copied).GetString("status") != dbReady {
			t.Errorf("%s after a failed drop: %v", copied.GetString("name"), f.fresh(copied).FieldsData())
		}
	}

	done := drop()
	f.closeCommand(done, schema.CommandSucceeded)
	f.p.databaseDropFinished(f.fresh(done))
	for _, copied := range copies {
		if _, err := f.app.FindRecordById(schema.Databases, copied.Id); err == nil {
			t.Errorf("%s stayed after the drop", copied.GetString("name"))
		}
	}
	for _, copied := range others {
		if f.fresh(copied).GetString("status") != dbReady {
			t.Errorf("%s of another database: %v", copied.GetString("name"), f.fresh(copied).FieldsData())
		}
	}
}

// TestAMoveWaitsForTemporaryCopies (#53, P5): a database with temporary
// copies does not move: its user, who owns them, would.
func TestAMoveWaitsForTemporaryCopies(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	target := f.sharedServer("db-new", databases.Postgres, "postgres:17", "admin-secret-new")
	previous, _, _ := f.copiesOfShop()
	_, err := f.p.planMove(f.server, target.Id, "shop")
	want := "drop the temporary copies of shop first, or wait until " + latestUntil(previous).Format("2006-01-02 15:04") + " UTC"
	if err == nil || err.Error() != want {
		t.Errorf("a move of a database with copies: %v, not %q", err, want)
	}
}

// TestADatabaseWaitsForItsRestoreAndTest (#53, P5): nothing changes a
// database while a restore or a restore test of it is under way.
func TestADatabaseWaitsForItsRestoreAndTest(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	restore := f.restore(f.job, map[string]any{"mode": schema.RestoreDatabase, "status": schema.RestoreRunning,
		"roots": []restoreRoot{{Source: "/shop.pgdump", Container: "db-main-db-1", Database: "shop", User: "shop"}}})
	try := func(want string) {
		t.Helper()
		for _, path := range []string{"/rotate", "/clone", "/connect"} {
			status, body := f.send(http.MethodPost, "/api/pc/databases/"+f.database.Id+path, operator, map[string]any{"name": "copy", "stack": f.stack.Id})
			if status != http.StatusConflict || body["message"] != want {
				t.Errorf("%s: %d %v", path, status, body["message"])
			}
		}
		for _, path := range []string{"/export", "/adminer"} {
			status, body := f.send(http.MethodPost, "/api/pc/databases/"+f.database.Id+path, f.admin, nil)
			if status != http.StatusConflict || body["message"] != want {
				t.Errorf("%s: %d %v", path, status, body["message"])
			}
		}
		status, body := f.send(http.MethodDelete, "/api/pc/databases/"+f.database.Id, operator, nil)
		if status != http.StatusConflict || body["message"] != want {
			t.Errorf("the drop: %d %v", status, body["message"])
		}
	}
	try("A restore of shop is under way.")
	f.setTest(restore, map[string]any{"status": schema.RestoreSucceeded})
	test := f.testRow(f.job, f.target, map[string]any{"status": schema.CheckRunning})
	try("A restore test of shop is under way.")
	f.setTest(test, map[string]any{"status": schema.CheckOK})
	status, body := f.send(http.MethodPost, "/api/pc/databases/"+f.database.Id+"/export", f.admin, nil)
	if status != http.StatusOK {
		t.Errorf("an export once both ended: %d %v", status, body["message"])
	}
}
