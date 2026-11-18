package panel

import (
	"errors"

	"github.com/pocketbase/pocketbase/core"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// Restore tests (#53) restore a database job's newest dump into a temporary
// database of the job's server, pc_rt_<check id>, as the database's own user,
// and compare the tables restored with the dump's: a backup_checks row of the
// kind restore_test, its temporary database's record and the command
// backup.restore_test are made in one transaction. The record is the
// temporary database's schedule: the panel drops it two hours after the test,
// or the next test of the server does. See "Per-database backups and restore
// tests (#53)" in docs/architecture.md.

// testSubject is what a database job's restore test restores: the job's
// database, its server and the user it restores as.
func (p *Panel) testSubject(job *core.Record) (database, server, user *core.Record, err error) {
	database, err = p.App.FindRecordById(schema.Databases, job.GetString("database"))
	if err != nil || job.GetString("database") == "" {
		return nil, nil, nil, errors.New("the database was dropped")
	}
	server, err = p.App.FindRecordById(schema.DBServers, database.GetString("server"))
	if err != nil {
		return nil, nil, nil, errors.New("the database's server is gone")
	}
	user, err = p.ownerOf(server, database)
	if err != nil {
		return nil, nil, nil, errors.New("the database has no user the panel knows")
	}
	return database, server, user, nil
}

// restoreTestPayload is the one builder of a restore test's
// backup.restore_test, without its repository and drop_before: the panel
// adds those as it queues it, and the enricher compares what it delivers with
// what this builds from the row, the database's server as it is then, and
// the database's user.
func restoreTestPayload(row, server *core.Record, user string) *agentv1.BackupRestoreTest {
	engine, _ := databases.EngineOf(server.GetString("engine"))
	return &agentv1.BackupRestoreTest{
		SnapshotId: row.GetString("snapshot_id"),
		JobTag:     "job:" + row.GetString("job"),
		CheckId:    row.Id,
		Database: &agentv1.BackupDbRestore{
			Connection: connectionOf(server),
			Database:   row.GetString("temporary"),
			File:       databases.BackupFile(engine, row.GetString("database_name")),
			Mode:       backup.DbRestoreTest,
			User:       user,
		},
	}
}

// checkTestDelivery checks a backup.restore_test as it is delivered, before
// it gets the target's and the database's secrets: its row is still there,
// a restore test that links this very command; the row names the command's
// target and server; the target is not being added, and a local target is
// read on its own server only; the command is what the row builds, into the
// container of the database's server as it is now and as the database's
// user, and drops earlier test copies only. On its first delivery the row
// waits for it, and whoever started a test by hand is still an operator. A
// command sent again after its agent reconnected keeps its secrets, as a
// restore does (see checkRestoreDelivery).
func (p *Panel) checkTestDelivery(record *core.Record, test *agentv1.BackupRestoreTest, target *core.Record) error {
	row, err := p.App.FindRecordById(schema.BackupChecks, test.GetCheckId())
	if err != nil {
		return errors.New("the restore test is gone")
	}
	if row.GetString("kind") != schema.CheckRestoreTest || row.GetString("command") == "" || row.GetString("command") != record.Id {
		// a command someone made by hand, as a superuser may
		return errors.New("the command is not the restore test's")
	}
	switch {
	case row.GetString("target") != target.Id:
		return errors.New("the restore test is from another target")
	case row.GetString("host") != record.GetString("host"):
		return errors.New("the restore test runs on another server")
	case target.GetString("status") == schema.TargetPending:
		return errors.New("the target is still being added")
	case target.GetString("kind") == backup.KindLocal && target.GetString("host") != record.GetString("host"):
		return errors.New("a local target is a directory of its own server: only that server restores from it")
	}
	job, err := p.App.FindRecordById(schema.BackupJobs, row.GetString("job"))
	if err != nil {
		return errors.New("the restore test's job is gone")
	}
	_, server, user, err := p.testSubject(job)
	if err != nil {
		return err
	}
	got, ok := proto.Clone(test).(*agentv1.BackupRestoreTest)
	if !ok {
		return errors.New("the command differs from the restore test")
	}
	got.Repository, got.DropBefore = nil, nil
	if !proto.Equal(got, restoreTestPayload(row, server, user.GetString("name"))) {
		return errors.New("the command differs from the restore test")
	}
	for _, name := range test.GetDropBefore() {
		if prefix, _, ok := databases.TemporaryOwner(name); !ok || prefix != databases.TemporaryTest || name == row.GetString("temporary") {
			return errors.New("the command drops more than earlier test copies")
		}
	}
	if record.GetString("status") != schema.CommandQueued {
		return nil
	}
	if row.GetString("status") != schema.CheckQueued {
		return errors.New("the restore test is not waiting for this command")
	}
	if row.GetString("trigger") == schema.RunManual &&
		!p.stillHasRole(record.GetString("requested_by"), record.GetString("requested_label"), schema.RoleOperator) {
		return errors.New("only an operator's restore test gets the target's secrets")
	}
	return nil
}
