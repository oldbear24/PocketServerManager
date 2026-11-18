package panel

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// linkedTest saves a queued restore test of the fixture's job with its
// command, as the panel queues one, and is the row and the command.
func (f *dbFixture) linkedTest(fields map[string]any) (*core.Record, *core.Record) {
	f.t.Helper()
	row := f.testRow(f.job, f.target, fields)
	row.Set("temporary", databases.TestDatabaseName(row.Id))
	row.Set("database_name", f.database.GetString("name"))
	if err := f.app.Save(row); err != nil {
		f.t.Fatal(err)
	}
	payload := restoreTestPayload(row, f.server, f.user.GetString("name"))
	payload.Repository = &agentv1.BackupRepository{TargetId: f.target.Id, Url: "s3:https://s3.example.com/backups/node"}
	command := f.command(f.host.Id, &agentv1.Command{Payload: &agentv1.Command_BackupRestoreTest{BackupRestoreTest: payload}})
	row.Set("command", command.Id)
	if err := f.app.Save(row); err != nil {
		f.t.Fatal(err)
	}
	return row, command
}

// TestARestoreTestGetsItsSecretsAtDeliveryOnly (#53, P4, AC1): a restore
// test's command is stored without a secret, and gets the target's, the
// server administrator's and the database user's passwords as it is
// delivered to the job's server, when it is what its row builds: into the
// container of the database's server as it is now, as the database's own
// user, and stored with no copy to drop, as what it drops is worked out as
// it is delivered: the earlier test copies of the server whose tests ended,
// never its own nor any other database. It gets none when it is forged,
// changed, from a target being added, for a row that does not wait for it,
// started by someone who is no operator any more, or while the user's
// password is being rotated.
func TestARestoreTestGetsItsSecretsAtDeliveryOnly(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	ended := f.testRow(f.job, f.target, map[string]any{"status": schema.CheckOK})
	earlier := f.temporary(databases.TestDatabaseName(ended.Id), time.Now().UTC().Add(time.Hour), map[string]any{"status": dbReady})
	f.temporary(databases.ScratchName("sssssssssssssss"), time.Now().UTC().Add(time.Hour), nil)
	row, command := f.linkedTest(nil)
	f.temporary(row.GetString("temporary"), time.Now().UTC().Add(time.Hour), map[string]any{"status": dbCreating})
	stored := f.stored(schema.Commands, command.Id)
	raw := mustJSON(t, stored.Get("payload"))
	for _, secret := range []string{adminSecretPG, userSecretShop, "repo-password-b2", "the-secret-key"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the stored command holds %s: %s", secret, raw)
		}
	}
	if stored.GetString("resource") != "db-test:db-main-db-1" {
		t.Errorf("the ordering key: %q", stored.GetString("resource"))
	}
	delivered, err := f.deliver(command)
	test := delivered.GetBackupRestoreTest()
	if err != nil || test.GetRepository().GetPassword() != "repo-password-b2" || test.GetDatabase().GetConnection().GetAdminPassword() != adminSecretPG ||
		test.GetDatabase().GetPassword() != userSecretShop || test.GetDatabase().GetUser() != "shop" || test.GetDatabase().GetFile() != "shop.pgdump" ||
		test.GetDatabase().GetMode() != backup.DbRestoreTest || !slices.Equal(test.GetDropBefore(), []string{earlier.GetString("name")}) {
		t.Fatalf("delivered: %v %v", delivered, err)
	}
	f.setTest(row, map[string]any{"status": schema.CheckOK})

	refused := func(name string, command *core.Record, reason string) {
		t.Helper()
		delivered, err := f.deliver(command)
		test := delivered.GetBackupRestoreTest()
		if err == nil || err.Error() != reason || test.GetRepository().GetPassword() != "" ||
			test.GetDatabase().GetConnection().GetAdminPassword() != "" || test.GetDatabase().GetPassword() != "" {
			t.Errorf("%s: %v, not %q (%v)", name, err, reason, delivered)
		}
	}

	// a command made by hand, for a row that links another
	row, linked := f.linkedTest(nil)
	forged := f.command(f.host.Id, &agentv1.Command{Payload: &agentv1.Command_BackupRestoreTest{BackupRestoreTest: f.payloadOfTest(linked)}})
	refused("a forged command", forged, "the command is not the restore test's")

	// a row that is gone, or names another target or server
	gone, orphan := f.linkedTest(nil)
	if err := f.app.Delete(gone); err != nil {
		t.Fatal(err)
	}
	refused("a row that is gone", orphan, "the restore test is gone")
	nas := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	elsewhere, fromNas := f.linkedTest(nil)
	f.setTest(elsewhere, map[string]any{"target": nas.Id})
	refused("another target", fromNas, "the restore test is from another target")
	other := testutil.NewHost(t, f.app, "node-2")
	_, away := f.linkedTest(nil)
	sent := f.stored(schema.Commands, away.Id)
	sent.Set("host", other.Id)
	if err := f.app.Save(sent); err != nil {
		t.Fatal(err)
	}
	refused("another server", sent, "the restore test runs on another server")
	disk := f.addTarget("disk", backup.KindLocal, other, backup.Config{Path: "/srv/restic"}, backup.Credentials{})
	local, fromDisk := f.linkedTest(map[string]any{"target": disk.Id})
	payload := f.payloadOfTest(fromDisk)
	payload.Repository.TargetId = disk.Id
	f.setPayload(fromDisk, &agentv1.Command{Payload: &agentv1.Command_BackupRestoreTest{BackupRestoreTest: payload}})
	refused("another server's local target", fromDisk, "a local target is a directory of its own server: only that server restores from it")
	_ = local

	// changed: another user, another container; any copy to drop, which
	// only the delivery adds
	for name, change := range map[string]func(*agentv1.BackupRestoreTest){
		"another user":         func(p *agentv1.BackupRestoreTest) { p.Database.User = "blog" },
		"another database":     func(p *agentv1.BackupRestoreTest) { p.Database.Database = "shop" },
		"another snapshot":     func(p *agentv1.BackupRestoreTest) { p.SnapshotId = otherSnapshot },
		"another container":    func(p *agentv1.BackupRestoreTest) { p.Database.Connection.ContainerId = "blog-db-1" },
		"another file":         func(p *agentv1.BackupRestoreTest) { p.Database.File = "blog.pgdump" },
		"another mode":         func(p *agentv1.BackupRestoreTest) { p.Database.Mode = backup.DbRestoreReplace },
		"a test copy to drop":  func(p *agentv1.BackupRestoreTest) { p.DropBefore = []string{earlier.GetString("name")} },
		"a scratch to drop":    func(p *agentv1.BackupRestoreTest) { p.DropBefore = []string{"pc_rs_aaaaaaaaaaaaaaa"} },
		"a database to drop":   func(p *agentv1.BackupRestoreTest) { p.DropBefore = []string{"blog"} },
		"its own copy to drop": func(p *agentv1.BackupRestoreTest) { p.DropBefore = []string{p.Database.Database} },
	} {
		row, linked := f.linkedTest(nil)
		payload := f.payloadOfTest(linked)
		change(payload)
		f.setPayload(linked, &agentv1.Command{Payload: &agentv1.Command_BackupRestoreTest{BackupRestoreTest: payload}})
		refused(name, linked, "the command differs from the restore test")
		_ = row
	}

	// the database moved to another container since the test was queued
	_, moved := f.linkedTest(nil)
	f.server.Set("container_ref", "db-new-db-1")
	if err := f.app.Save(f.server); err != nil {
		t.Fatal(err)
	}
	refused("a moved database", moved, "the command differs from the restore test")
	f.server.Set("container_ref", "db-main-db-1")
	if err := f.app.Save(f.server); err != nil {
		t.Fatal(err)
	}

	// a row that does not wait for its command
	f.setTest(row, map[string]any{"status": schema.CheckCancelled})
	refused("a cancelled test", linked, "the restore test is not waiting for this command")

	// the target being added
	_, pending := f.linkedTest(nil)
	f.target.Set("status", schema.TargetPending)
	if err := f.app.Save(f.target); err != nil {
		t.Fatal(err)
	}
	refused("a target being added", pending, "the target is still being added")
	f.target.Set("status", schema.TargetOK)
	if err := f.app.Save(f.target); err != nil {
		t.Fatal(err)
	}

	// a test by hand of someone who is no operator any more
	viewer, _ := testutil.NewUser(t, f.app, "was-operator@example.com", schema.RoleViewer)
	_, manual := f.linkedTest(map[string]any{"trigger": schema.RunManual})
	byHand := f.stored(schema.Commands, manual.Id)
	byHand.Set("requested_by", viewer.Id)
	if err := f.app.Save(byHand); err != nil {
		t.Fatal(err)
	}
	refused("a viewer's test", manual, "only an operator's restore test gets the target's secrets")

	// a rotation of the user's password under way
	_, rotating := f.linkedTest(nil)
	f.user.Set("pending_password", "the-next-password")
	if err := f.app.Save(f.user); err != nil {
		t.Fatal(err)
	}
	refused("a rotation", rotating, "a rotation of shop's password is under way")
	f.user.Set("pending_password", "")
	if err := f.app.Save(f.user); err != nil {
		t.Fatal(err)
	}

	// the database dropped
	_, dropped := f.linkedTest(nil)
	f.job.Set("database", "")
	if err := f.app.Save(f.job); err != nil {
		t.Fatal(err)
	}
	refused("a dropped database", dropped, "the database was dropped")
}

// payloadOfTest is a stored restore test's payload.
func (f *jobFixture) payloadOfTest(command *core.Record) *agentv1.BackupRestoreTest {
	f.t.Helper()
	stored := f.stored(schema.Commands, command.Id)
	built, err := commands.Build("backup.restore_test", mustJSON(f.t, stored.Get("payload")))
	if err != nil {
		f.t.Fatal(err)
	}
	return built.GetBackupRestoreTest()
}

// setPayload stores another payload on a command.
func (f *jobFixture) setPayload(command *core.Record, payload *agentv1.Command) {
	f.t.Helper()
	raw, err := commands.PayloadJSON(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	stored := f.stored(schema.Commands, command.Id)
	stored.Set("payload", string(raw))
	if err := f.app.Save(stored); err != nil {
		f.t.Fatal(err)
	}
}

// TestADiscardGetsTheAdministratorOnly (#53, P4): db.database.discard gets
// its server administrator's password at delivery, and no user's.
func TestADiscardGetsTheAdministratorOnly(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	command := f.command(f.host.Id, &agentv1.Command{Payload: &agentv1.Command_DbDatabaseDiscard{DbDatabaseDiscard: &agentv1.DbDatabaseDiscard{
		Connection: connectionOf(f.server), Names: []string{"pc_rt_aaaaaaaaaaaaaaa"}, RevokeFrom: "shop",
	}}})
	stored := f.stored(schema.Commands, command.Id)
	if raw := mustJSON(t, stored.Get("payload")); strings.Contains(string(raw), adminSecretPG) {
		t.Errorf("the stored discard holds the password: %s", raw)
	}
	built, err := commands.Build("db.database.discard", mustJSON(t, stored.Get("payload")))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.p.enrichDatabaseCommand(stored, built); err != nil || built.GetDbDatabaseDiscard().GetConnection().GetAdminPassword() != adminSecretPG {
		t.Errorf("delivered: %v %v", built, err)
	}
}
