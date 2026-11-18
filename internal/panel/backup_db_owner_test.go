package panel

import (
	"testing"

	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// ownerScenario is the review's scenario of #53 on the fixture's
// PostgreSQL server db-main: the stack a's own server moved in with its
// database app and the application's user app_user, and the stack b's
// database blog has a user named app.
type ownerScenario struct {
	a, b, app, appUser, blog, bUser, job *core.Record
}

func (f *dbFixture) ownerScenario() ownerScenario {
	f.t.Helper()
	var s ownerScenario
	s.a = f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "a"})
	s.app = f.save(schema.Databases, map[string]any{"server": f.server.Id, "name": "app", "status": dbReady, "owner_kind": "stack", "stack": s.a.Id})
	s.appUser = f.save(schema.DBUsers, map[string]any{"server": f.server.Id, "name": "app_user", "password": "secret-of-a",
		"databases": []string{s.app.Id}, "app_password": true})
	s.b = f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "b"})
	s.blog = f.save(schema.Databases, map[string]any{"server": f.server.Id, "name": "blog", "status": dbReady, "owner_kind": "stack", "stack": s.b.Id})
	s.bUser = f.save(schema.DBUsers, map[string]any{"server": f.server.Id, "name": "app", "password": "secret-of-b",
		"databases": []string{s.blog.Id}, "secret_name": databases.SecretName("blog")})
	s.job = f.dbJob(s.app)
	f.snapshotRun(s.job, f.target, snapshotID, 1024)
	return s
}

// TestARestoreRunsAsTheUserThatHoldsItsDatabase (#53, ISO-1, AC1): a
// restore test of a database, a restore back into it, what either is
// delivered with, and whom a test's copy is revoked from are the user
// whose databases hold the database, never another application's user that
// is only named like it: b's user app never signs in to a copy of a's
// database app, nor owns one.
func TestARestoreRunsAsTheUserThatHoldsItsDatabase(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	f.host.Set("status", schema.HostOnline)
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	s := f.ownerScenario()
	if owner, err := f.p.ownerOf(f.server, s.app); err != nil || owner.Id != s.appUser.Id {
		t.Errorf("the owner of app: %v %v", owner.GetString("name"), err)
	}
	if owner, err := f.p.ownerOf(f.server, s.blog); err != nil || owner.Id != s.bUser.Id {
		t.Errorf("the owner of blog: %v %v", owner.GetString("name"), err)
	}

	// the weekly test of app's job
	row, command := f.startedTest(s.job, schema.RunSchedule)
	if user := f.payloadOfTest(command).GetDatabase().GetUser(); user != "app_user" {
		t.Errorf("the restore test of app restores as %q", user)
	}
	delivered, err := f.deliver(command)
	if database := delivered.GetBackupRestoreTest().GetDatabase(); err != nil || database.GetUser() != "app_user" || database.GetPassword() != "secret-of-a" {
		t.Errorf("the restore test is delivered as %q with %q: %v", database.GetUser(), database.GetPassword(), err)
	}
	copied, err := f.p.databaseOf(f.server, row.GetString("temporary"))
	if err != nil {
		t.Fatal(err)
	}
	if from := f.p.revokeFrom(copied, f.server); from != "app_user" {
		t.Errorf("the test's copy is revoked from %q", from)
	}
	f.setTest(row, map[string]any{"status": schema.CheckOK})

	// a restore back into app
	plan, problem := f.p.databasePlan(s.job, f.target, restoreBody{Mode: schema.RestoreDatabase}, "rrrrrrrrrrrrrrr")
	if problem != nil || len(plan.roots) != 1 || plan.roots[0].User != "app_user" {
		t.Fatalf("the plan of a restore of app: %+v %v", plan.roots, problem)
	}
	restore := f.restore(s.job, map[string]any{"mode": schema.RestoreDatabase, "status": schema.RestoreQueued, "overwrite": true,
		"confirmed_label": "superuser root@example.com", "database": s.app.Id, "database_name": "app", "stack_name": "a", "roots": plan.roots})
	restored, err := f.deliver(f.link(restore, false))
	if database := restored.GetBackupRestore().GetDatabase(); err != nil || database.GetUser() != "app_user" || database.GetPassword() != "secret-of-a" {
		t.Errorf("the restore is delivered as %q with %q: %v", database.GetUser(), database.GetPassword(), err)
	}
	// a plan made as b's user is not delivered
	roots := rootsOf(restore)
	roots[0].User = "app"
	f.setTest(restore, map[string]any{"roots": roots})
	if _, err := f.deliver(f.link(restore, false)); err == nil || err.Error() != "the database's user is not the one the restore was planned with" {
		t.Errorf("a restore of app as b's user: %v", err)
	}
}
