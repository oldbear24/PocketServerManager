package panel

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// The passwords of the database fixture, which nothing the panel keeps may
// hold.
const (
	adminSecretPG    = "admin-secret-pg"
	adminSecretMaria = "admin-secret-maria"
	userSecretShop   = "user-secret-shop"
)

// dbFixture is a job fixture whose server holds a shared database server
// with the database shop of the stack shop, shop's user, and shop's job
// (origin database) with a succeeded run to the fixture's target, as the
// panel's jobs part makes them (#53).
type dbFixture struct {
	*jobFixture
	server, stack, database, user, job, run *core.Record
}

// newDBFixture is a dbFixture on a PostgreSQL server, or a MariaDB one, whose
// agent restores databases.
func newDBFixture(t *testing.T, engine string) *dbFixture {
	t.Helper()
	f := &dbFixture{jobFixture: newJobFixture(t)}
	// the panel's own schedulers, which pass at the start of each minute,
	// would settle what the tests leave open and start what is due: the
	// tests make every pass themselves (rather than wait for a minute's
	// start, which a test under the race detector may outlast anyway)
	f.app.Cron().Stop()
	// restore tests whose slots the panel's own scheduler never meets
	settings, err := f.p.settingsRecord()
	if err != nil {
		t.Fatal(err)
	}
	settings.Set("restore_test_schedule", farSchedule())
	if err := f.app.Save(settings); err != nil {
		t.Fatal(err)
	}
	f.carry(f.host, "backup.run", "backup.restore", "backup.dump", "backup.snapshots", "backup.browse", "backup.restore_test",
		"db.database.discard", "db.database.drop", "db.stats", backup.CapabilityDatabase)
	if engine == databases.MariaDB.Name {
		f.server = f.sharedServer("maria", databases.MariaDB, "mariadb:11.8", adminSecretMaria)
	} else {
		f.server = f.sharedServer("db-main", databases.Postgres, "postgres:17", adminSecretPG)
	}
	f.stack = f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "shop"})
	f.database, f.user = f.addDatabase(f.server, "shop", userSecretShop, map[string]any{"owner_kind": "stack", "stack": f.stack.Id})
	f.job = f.dbJob(f.database)
	f.run = f.snapshotRun(f.job, f.target, snapshotID, 4096)
	return f
}

// sharedServer saves a ready shared server of the fixture's host.
func (f *jobFixture) sharedServer(name string, engine databases.Engine, image, admin string) *core.Record {
	f.t.Helper()
	stack := f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": name, "source": stackSourceDBServer})
	return f.save(schema.DBServers, map[string]any{
		"host": f.host.Id, "name": name, "engine": engine.Name, "kind": schema.DBServerShared, "image": image,
		"container_ref": serverContainer(name), "service": databases.ServerService, "stack": stack.Id, "port": engine.Port,
		"admin_user": engine.AdminUser, "admin_password": admin, "status": dbReady,
	})
}

// addDatabase saves a ready database of a server and its user, named after
// it, with the secret db/<name>.
func (f *jobFixture) addDatabase(server *core.Record, name, password string, fields map[string]any) (*core.Record, *core.Record) {
	f.t.Helper()
	all := map[string]any{"server": server.Id, "name": name, "status": dbReady, "owner_kind": "none"}
	for key, value := range fields {
		all[key] = value
	}
	database := f.save(schema.Databases, all)
	user := f.save(schema.DBUsers, map[string]any{"server": server.Id, "name": name, "password": password,
		"databases": []string{database.Id}, "secret_name": databases.SecretName(name)})
	return database, user
}

// dbJob saves a database's job, as the jobs part makes it, never due by
// itself and backing up to the fixture's target. The panel may have made the
// job already, as its hooks do for a ready database: then that job takes these
// fields.
func (f *jobFixture) dbJob(database *core.Record) *core.Record {
	f.t.Helper()
	server := f.stored(schema.DBServers, database.GetString("server"))
	engine := schema.DumpPostgres
	if server.GetString("engine") == databases.MariaDB.Name {
		engine = schema.DumpMySQL
	}
	fields := map[string]any{
		"name": server.GetString("name") + "-" + database.GetString("name"), "host": f.host.Id, "targets": []string{f.target.Id},
		"schedule": "0 0 30 2 *", "enabled": true, "origin": schema.JobDatabase, "source_kind": schema.SourceDatabase,
		"source": database.GetString("name"), "engine": engine, "database": database.Id, "db_server_name": server.GetString("name"),
		"own_settings": []string{},
	}
	if existing, err := f.app.FindFirstRecordByFilter(schema.BackupJobs, "database={:database}", map[string]any{"database": database.Id}); err == nil {
		existing.Load(fields)
		if err := f.app.Save(existing); err != nil {
			f.t.Fatal(err)
		}
		return existing
	}
	collection, err := f.app.FindCollectionByNameOrId(schema.BackupJobs)
	if err != nil {
		f.t.Fatal(err)
	}
	job := core.NewRecord(collection)
	job.Load(fields)
	if err := f.app.Save(job); err != nil {
		existing, findErr := f.app.FindFirstRecordByFilter(schema.BackupJobs, "database={:database}", map[string]any{"database": database.Id})
		if findErr != nil {
			f.t.Fatalf("the job of %s: %v", database.GetString("name"), err)
		}
		existing.Load(fields)
		if err := f.app.Save(existing); err != nil {
			f.t.Fatal(err)
		}
		return existing
	}
	return job
}

// snapshotRun saves a run of a job to a target that succeeded with a
// snapshot.
func (f *jobFixture) snapshotRun(job, target *core.Record, snapshot string, bytes int64) *core.Record {
	f.t.Helper()
	now := time.Now().UTC()
	return f.save(schema.BackupRuns, map[string]any{"job": job.Id, "target": target.Id, "host": job.GetString("host"),
		"trigger": schema.RunSchedule, "status": schema.RunSucceeded, "snapshot_id": snapshot, "total_bytes": bytes,
		"started_at": now.Add(-time.Minute), "finished_at": now})
}

// phpSite saves a PHP site of the fixture's server, which gets the edge
// role a site needs.
func (f *jobFixture) phpSite(name string) *core.Record {
	f.t.Helper()
	host := f.stored(schema.Hosts, f.host.Id)
	if !slices.Contains(host.GetStringSlice("roles"), schema.HostRoleEdge) {
		host.Set("roles", append(host.GetStringSlice("roles"), schema.HostRoleEdge))
		if err := f.app.Save(host); err != nil {
			f.t.Fatal(err)
		}
		f.host = host
	}
	return f.save(schema.Sites, map[string]any{"host": f.host.Id, "name": name, "type": schema.SitePHPApp, "source": "upload"})
}

// otherSnapshot is another snapshot's full id.
var otherSnapshot = strings.Repeat("7c4d2b9e", 8)
