package panel

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// snapshotID is a snapshot's full id, as the tests of restores name it.
var snapshotID = strings.Repeat("3f9a1c2e", 8)

// restore saves a restore record of a job to the fixture's target.
func (f *jobFixture) restore(job *core.Record, fields map[string]any) *core.Record {
	f.t.Helper()
	all := map[string]any{"job": job.Id, "target": f.target.Id, "host": job.GetString("host"), "snapshot_id": snapshotID,
		"mode": schema.RestoreOriginal, "status": schema.RestorePreviewing,
		"roots": []restoreRoot{{Source: "/volumes/app_data", Volume: "app_data"}}}
	for key, value := range fields {
		all[key] = value
	}
	return f.save(schema.BackupRestores, all)
}

// link queues a restore's preview, or the restore, or a download's dump, as
// the routes build it, and links it to the record.
func (f *jobFixture) link(restore *core.Record, dryRun bool) *core.Record {
	f.t.Helper()
	repository := &agentv1.BackupRepository{TargetId: restore.GetString("target"), Url: "s3:https://s3.example.com/backups/node"}
	command := &agentv1.Command{}
	if restore.GetString("mode") == schema.RestoreDownload {
		payload := dumpPayload(restore)
		payload.Repository = repository
		command.Payload = &agentv1.Command_BackupDump{BackupDump: payload}
	} else {
		payload := restorePayload(restore, dryRun)
		payload.Repository = repository
		command.Payload = &agentv1.Command_BackupRestore{BackupRestore: payload}
	}
	record := f.command(restore.GetString("host"), command)
	field := "command"
	if dryRun {
		field = "preview_command"
	}
	restore.Set(field, record.Id)
	if err := f.app.Save(restore); err != nil {
		f.t.Fatal(err)
	}
	return record
}

// command queues a command for a server.
func (f *jobFixture) command(host string, command *agentv1.Command) *core.Record {
	f.t.Helper()
	raw, err := commands.PayloadJSON(command)
	if err != nil {
		f.t.Fatal(err)
	}
	payload := map[string]any{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		f.t.Fatal(err)
	}
	return f.save(schema.Commands, map[string]any{"host": host, "type": commands.TypeOf(command), "payload": payload})
}

// deliver completes a queued command as the queue does when it sends it.
func (f *jobFixture) deliver(command *core.Record) (*agentv1.Command, error) {
	f.t.Helper()
	stored, err := f.app.FindRecordById(schema.Commands, command.Id)
	if err != nil {
		f.t.Fatal(err)
	}
	raw, err := json.Marshal(stored.Get("payload"))
	if err != nil {
		f.t.Fatal(err)
	}
	built, err := commands.Build(stored.GetString("type"), raw)
	if err != nil {
		f.t.Fatal(err)
	}
	built.Id = stored.Id
	return built, f.p.enrichBackup(stored, built)
}

// password is the repository password a delivered command carries.
func password(command *agentv1.Command) string {
	if dump := command.GetBackupDump(); dump != nil {
		return dump.GetRepository().GetPassword()
	}
	if listing := command.GetBackupSnapshots(); listing != nil {
		return listing.GetRepository().GetPassword()
	}
	if browse := command.GetBackupBrowse(); browse != nil {
		return browse.GetRepository().GetPassword()
	}
	return command.GetBackupRestore().GetRepository().GetPassword()
}

// refusedWith says whether a delivery was refused with a reason, without a
// secret.
func refusedWith(command *agentv1.Command, err error, reason string) bool {
	return err != nil && err.Error() == reason && password(command) == ""
}

// TestRestoreSecretsGoOnlyToTheRestoresServer (#30, AC2/SAFE): a restore's
// preview and the restore get the target's secrets on the server its record
// names, whichever server reaches the target, and on no other; a local
// target's only on its own server; a target being added gives none.
func TestRestoreSecretsGoOnlyToTheRestoresServer(t *testing.T) {
	f := newJobFixture(t)
	boss, _ := testutil.NewUser(t, f.app, "boss@example.com", schema.RoleAdmin)
	other := testutil.NewHost(t, f.app, "node-2")
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})

	// a restore to a new folder of another server
	restore := f.restore(job, map[string]any{"mode": schema.RestorePath, "host": other.Id, "roots": nil,
		"path": "/srv/restored", "source": "/volumes/app_data", "requested_by": boss.Id, "requested_label": "boss@example.com"})
	preview := f.link(restore, true)
	command, err := f.deliver(preview)
	if err != nil || password(command) != "repo-password-b2" || command.GetBackupRestore().GetRepository().GetEnv()["AWS_SECRET_ACCESS_KEY"] != "the-secret-key" {
		t.Fatalf("the preview on its server: %v %v", command, err)
	}

	// the same command to the target's own server, as a forged record
	forged, err := f.app.FindRecordById(schema.Commands, preview.Id)
	if err != nil {
		t.Fatal(err)
	}
	forged.Set("host", f.host.Id)
	built, _ := commands.Build("backup.restore", mustJSON(t, forged.Get("payload")))
	if err := f.p.enrichBackup(forged, built); err == nil || err.Error() != "the restore goes to another server" || password(built) != "" {
		t.Errorf("to the target's server: %v", err)
	}

	// another target's restore
	nas := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	built, _ = commands.Build("backup.restore", mustJSON(t, forged.Get("payload")))
	built.GetBackupRestore().Repository.TargetId = nas.Id
	forged.Set("host", other.Id)
	if err := f.p.enrichBackup(forged, built); !refusedWith(built, err, "the restore is from another target") {
		t.Errorf("from another target: %v", err)
	}

	// a target still being added
	f.target.Set("status", schema.TargetPending)
	if err := f.app.Save(f.target); err != nil {
		t.Fatal(err)
	}
	if command, err := f.deliver(preview); !refusedWith(command, err, "the target is still being added") {
		t.Errorf("a target being added: %v", err)
	}
	f.target.Set("status", schema.TargetOK)
	if err := f.app.Save(f.target); err != nil {
		t.Fatal(err)
	}

	// a local target is read on its own server only
	disk := f.addTarget("disk", backup.KindLocal, f.host, backup.Config{Path: "/srv/restic"}, backup.Credentials{})
	elsewhere := f.restore(job, map[string]any{"mode": schema.RestorePath, "target": disk.Id, "host": other.Id, "roots": nil,
		"path": "/srv/restored", "source": "/", "requested_by": boss.Id, "requested_label": "boss@example.com"})
	if command, err := f.deliver(f.link(elsewhere, true)); !refusedWith(command, err, "a local target is a directory of its own server: only that server restores from it") {
		t.Errorf("a local target elsewhere: %v", err)
	}
	home := f.restore(job, map[string]any{"mode": schema.RestorePath, "target": disk.Id, "host": f.host.Id, "roots": nil,
		"path": "/srv/restored", "source": "/", "requested_by": boss.Id, "requested_label": "boss@example.com"})
	if command, err := f.deliver(f.link(home, true)); err != nil || command.GetBackupRestore().GetRepository().GetMount() != "/srv/restic" ||
		password(command) != "repo-password-disk" {
		t.Errorf("a local target on its server: %v %v", command, err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestAHandMadeRestoreCommandGetsNoSecrets (#30, SAFE): a backup.restore or
// backup.dump that its record does not link, as one a superuser made through
// the API for a record of their choice, gets no secret; nor does one whose
// record is gone, or that is not of the record's kind.
func TestAHandMadeRestoreCommandGetsNoSecrets(t *testing.T) {
	f := newJobFixture(t)
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	restore := f.restore(job, map[string]any{"requested_label": "superuser root@example.com"})
	f.link(restore, true)

	payload := restorePayload(restore, true)
	payload.Repository = &agentv1.BackupRepository{TargetId: f.target.Id}
	handMade := f.command(f.host.Id, &agentv1.Command{Payload: &agentv1.Command_BackupRestore{BackupRestore: payload}})
	if command, err := f.deliver(handMade); !refusedWith(command, err, "the command is not the restore's") {
		t.Errorf("a preview made by hand: %v", err)
	}
	// as the restore itself, which the record does not link yet
	real := restorePayload(restore, false)
	real.Repository = &agentv1.BackupRepository{TargetId: f.target.Id}
	handMade = f.command(f.host.Id, &agentv1.Command{Payload: &agentv1.Command_BackupRestore{BackupRestore: real}})
	if command, err := f.deliver(handMade); !refusedWith(command, err, "the command is not the restore's") {
		t.Errorf("a restore made by hand: %v", err)
	}
	// a dump for a restore that is no download
	dump := dumpPayload(restore)
	dump.Repository = &agentv1.BackupRepository{TargetId: f.target.Id}
	handMade = f.command(f.host.Id, &agentv1.Command{Payload: &agentv1.Command_BackupDump{BackupDump: dump}})
	restore.Set("command", handMade.Id)
	if err := f.app.Save(restore); err != nil {
		t.Fatal(err)
	}
	if command, err := f.deliver(handMade); !refusedWith(command, err, "the command is not the restore's") {
		t.Errorf("a dump of a restore: %v", err)
	}
	// a record that is gone
	gone := f.restore(job, map[string]any{"requested_label": "superuser root@example.com"})
	preview := f.link(gone, true)
	if err := f.app.Delete(gone); err != nil {
		t.Fatal(err)
	}
	if command, err := f.deliver(preview); !refusedWith(command, err, "the restore is gone") {
		t.Errorf("a restore that is gone: %v", err)
	}
}

// TestARestoreWhosePayloadDiffersGetsNoSecrets (#30, AC3/SAFE): a restore
// gets the secrets only as it was confirmed: any other snapshot, folder,
// selection, replace, overwrite or container to stop than its record says
// refuses it, and an overwrite nobody confirmed does too.
func TestARestoreWhosePayloadDiffersGetsNoSecrets(t *testing.T) {
	f := newJobFixture(t)
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	confirmed := map[string]any{"status": schema.RestoreQueued, "overwrite": true, "replace": true,
		"requested_label": "superuser root@example.com", "confirmed_label": "superuser root@example.com",
		"preview": map[string]any{"would_stop": []any{map[string]any{"name": "app-web-1", "project": "app"}}}}
	restore := f.restore(job, confirmed)
	command := f.link(restore, false)
	delivered, err := f.deliver(command)
	if err != nil || password(delivered) == "" || !delivered.GetBackupRestore().GetOverwrite() ||
		strings.Join(delivered.GetBackupRestore().GetConfirmedStops(), ",") != "app-web-1" {
		t.Fatalf("as confirmed: %v %v", delivered, err)
	}

	for name, change := range map[string]func(*agentv1.BackupRestore){
		"another snapshot": func(r *agentv1.BackupRestore) { r.SnapshotId = strings.Repeat("ab", 32) },
		"another volume":   func(r *agentv1.BackupRestore) { r.Original[0].Volume = "other_data" },
		"another folder":   func(r *agentv1.BackupRestore) { r.TargetPath = "/srv/elsewhere" },
		"a selection":      func(r *agentv1.BackupRestore) { r.Include = []string{"/volumes/app_data/x"} },
		"no replace":       func(r *agentv1.BackupRestore) { r.Replace = false },
		"another stack":    func(r *agentv1.BackupRestore) { r.Stack = "other" },
		"another stop":     func(r *agentv1.BackupRestore) { r.ConfirmedStops = append(r.ConfirmedStops, "db-1") },
		"no stop":          func(r *agentv1.BackupRestore) { r.ConfirmedStops = nil },
		"a preview":        func(r *agentv1.BackupRestore) { r.DryRun = true },
		"another job":      func(r *agentv1.BackupRestore) { r.JobTag = "job:otherjob0000000" },
	} {
		stored, err := f.app.FindRecordById(schema.Commands, command.Id)
		if err != nil {
			t.Fatal(err)
		}
		built, err := commands.Build("backup.restore", mustJSON(t, stored.Get("payload")))
		if err != nil {
			t.Fatal(err)
		}
		change(built.GetBackupRestore())
		want := "the command differs from the restore that was confirmed"
		if built.GetBackupRestore().GetDryRun() {
			// a preview is the record's preview_command
			want = "the command is not the restore's"
		}
		if err := f.p.enrichBackup(stored, built); !refusedWith(built, err, want) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// an overwrite that nobody confirmed
	unconfirmed := f.restore(job, map[string]any{"status": schema.RestoreQueued, "overwrite": true, "requested_label": "superuser root@example.com"})
	if command, err := f.deliver(f.link(unconfirmed, false)); !refusedWith(command, err, "the overwrite was not confirmed") {
		t.Errorf("unconfirmed: %v", err)
	}

	// a download as its record says, and changed
	download := f.restore(job, map[string]any{"mode": schema.RestoreDownload, "status": schema.RestoreQueued, "roots": nil,
		"path": "/volumes/app_data", "download_name": "data-3f9a1c2e.tar", "requested_label": "superuser root@example.com"})
	dump := f.link(download, false)
	if command, err := f.deliver(dump); err != nil || password(command) == "" {
		t.Errorf("a download: %v", err)
	}
	stored, _ := f.app.FindRecordById(schema.Commands, dump.Id)
	built, _ := commands.Build("backup.dump", mustJSON(t, stored.Get("payload")))
	built.GetBackupDump().Path = "/etc"
	if err := f.p.enrichBackup(stored, built); !refusedWith(built, err, "the command differs from the restore that was confirmed") {
		t.Errorf("another path: %v", err)
	}
}

// TestADemotedRequesterGetsNoSecretsOnFirstDelivery (#30, SAFE): whoever
// asked for a preview or a download, or started a restore, must still have
// the role its mode needs when the command first goes out, and the record
// must be waiting for it.
func TestADemotedRequesterGetsNoSecretsOnFirstDelivery(t *testing.T) {
	f := newJobFixture(t)
	operator, _ := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	admin, _ := testutil.NewUser(t, f.app, "boss@example.com", schema.RoleAdmin)
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})

	original := f.restore(job, map[string]any{"requested_by": operator.Id, "requested_label": "operator@example.com"})
	preview := f.link(original, true)
	if command, err := f.deliver(preview); err != nil || password(command) == "" {
		t.Fatalf("an operator's preview: %v", err)
	}
	operator.Set("role", schema.RoleViewer)
	if err := f.app.Save(operator); err != nil {
		t.Fatal(err)
	}
	if command, err := f.deliver(preview); !refusedWith(command, err, "only an operator's restore gets the target's secrets") {
		t.Errorf("a viewer's preview: %v", err)
	}
	operator.Set("role", schema.RoleOperator)
	if err := f.app.Save(operator); err != nil {
		t.Fatal(err)
	}

	// a restore to a new folder is an admin's
	folder := f.restore(job, map[string]any{"mode": schema.RestorePath, "roots": nil, "path": "/srv/restored", "source": "/",
		"requested_by": operator.Id, "requested_label": "operator@example.com"})
	if command, err := f.deliver(f.link(folder, true)); !refusedWith(command, err, "only an admin's restore gets the target's secrets") {
		t.Errorf("an operator's restore to a folder: %v", err)
	}
	// the restore goes by who started it
	started := f.restore(job, map[string]any{"mode": schema.RestorePath, "roots": nil, "path": "/srv/restored", "source": "/",
		"status": schema.RestoreQueued, "requested_by": operator.Id, "requested_label": "operator@example.com",
		"confirmed_by": admin.Id, "confirmed_label": "boss@example.com"})
	restore := f.link(started, false)
	if command, err := f.deliver(restore); err != nil || password(command) == "" {
		t.Errorf("started by an admin: %v", err)
	}
	admin.Set("role", schema.RoleOperator)
	if err := f.app.Save(admin); err != nil {
		t.Fatal(err)
	}
	if command, err := f.deliver(restore); !refusedWith(command, err, "only an admin's restore gets the target's secrets") {
		t.Errorf("started by a demoted admin: %v", err)
	}
	// a download, by who asked for it
	download := f.restore(job, map[string]any{"mode": schema.RestoreDownload, "status": schema.RestoreQueued, "roots": nil,
		"path": "/volumes/app_data", "download_name": "data.tar", "requested_by": admin.Id, "requested_label": "boss@example.com"})
	if command, err := f.deliver(f.link(download, false)); !refusedWith(command, err, "only an admin's download gets the target's secrets") {
		t.Errorf("a demoted admin's download: %v", err)
	}
	// a user that is gone
	if err := f.app.Delete(admin); err != nil {
		t.Fatal(err)
	}
	if command, err := f.deliver(restore); err == nil || password(command) != "" {
		t.Errorf("started by a user who is gone: %v", err)
	}
	// a superuser's
	superuser := f.restore(job, map[string]any{"mode": schema.RestorePath, "roots": nil, "path": "/srv/restored", "source": "/",
		"requested_label": "superuser root@example.com"})
	if command, err := f.deliver(f.link(superuser, true)); err != nil || password(command) == "" {
		t.Errorf("a superuser's preview: %v", err)
	}

	// a record that waits for something else
	waiting := f.restore(job, map[string]any{"requested_by": operator.Id, "requested_label": "operator@example.com"})
	early := f.link(waiting, true)
	waiting.Set("status", schema.RestorePreviewed)
	if err := f.app.Save(waiting); err != nil {
		t.Fatal(err)
	}
	if command, err := f.deliver(early); !refusedWith(command, err, "the restore is not waiting for this command") {
		t.Errorf("a preview of a record that has one: %v", err)
	}
}

// TestAReplayedRestoreKeepsItsSecrets (#30): a restore sent again after its
// agent reconnected reached the agent with the secrets already, and may be
// running: it keeps them, though its starter lost the role or the record
// moved on; the checks of what it is and where it goes still hold.
func TestAReplayedRestoreKeepsItsSecrets(t *testing.T) {
	f := newJobFixture(t)
	operator, _ := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	restore := f.restore(job, map[string]any{"status": schema.RestoreQueued, "overwrite": true,
		"requested_by": operator.Id, "requested_label": "operator@example.com",
		"confirmed_by": operator.Id, "confirmed_label": "operator@example.com"})
	command := f.link(restore, false)
	operator.Set("role", schema.RoleViewer)
	if err := f.app.Save(operator); err != nil {
		t.Fatal(err)
	}
	restore.Set("status", schema.RestoreRunning)
	if err := f.app.Save(restore); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{schema.CommandDispatched, schema.CommandRunning} {
		stored, err := f.app.FindRecordById(schema.Commands, command.Id)
		if err != nil {
			t.Fatal(err)
		}
		stored.Set("status", status)
		if err := f.app.Save(stored); err != nil {
			t.Fatal(err)
		}
		if delivered, err := f.deliver(command); err != nil || password(delivered) != "repo-password-b2" {
			t.Errorf("sent again while %s: %v", status, err)
		}
	}
	// still not to another server
	stored, _ := f.app.FindRecordById(schema.Commands, command.Id)
	stored.Set("host", testutil.NewHost(t, f.app, "node-2").Id)
	built, _ := commands.Build("backup.restore", mustJSON(t, stored.Get("payload")))
	if err := f.p.enrichBackup(stored, built); !refusedWith(built, err, "the restore goes to another server") {
		t.Errorf("sent again elsewhere: %v", err)
	}
}

// TestListingsGoOnlyToServersThatHaveTheSecrets (#30, SAFE): a listing of a
// target's snapshots, or of a snapshot's folder, goes to the target's
// server, or to the server of the job it lists when that job backs up to the
// remote target; never to another server, nor a local target's elsewhere.
func TestListingsGoOnlyToServersThatHaveTheSecrets(t *testing.T) {
	f := newJobFixture(t)
	jobHost := testutil.NewHost(t, f.app, "node-2")
	stranger := testutil.NewHost(t, f.app, "node-3")
	job := f.job("data", map[string]any{"host": jobHost.Id, "source_kind": schema.SourceVolume, "source": "app_data"})
	snapshots := func(host string, target *core.Record, job string) (*agentv1.Command, error) {
		return f.deliver(f.command(host, &agentv1.Command{Payload: &agentv1.Command_BackupSnapshots{BackupSnapshots: &agentv1.BackupSnapshots{
			Repository: &agentv1.BackupRepository{TargetId: target.Id}, Tags: []string{"pocketcloud", "job:" + job}, Limit: 500,
		}}}))
	}
	browse := func(host string, target *core.Record, job string) (*agentv1.Command, error) {
		return f.deliver(f.command(host, &agentv1.Command{Payload: &agentv1.Command_BackupBrowse{BackupBrowse: &agentv1.BackupBrowse{
			Repository: &agentv1.BackupRepository{TargetId: target.Id}, SnapshotId: snapshotID, Path: "/", JobTag: "job:" + job,
		}}}))
	}
	refused := "the backup target is reached from another server"
	for name, list := range map[string]func(string, *core.Record, string) (*agentv1.Command, error){"snapshots": snapshots, "browse": browse} {
		if command, err := list(f.host.Id, f.target, job.Id); err != nil || password(command) != "repo-password-b2" {
			t.Errorf("%s on the target's server: %v", name, err)
		}
		if command, err := list(jobHost.Id, f.target, job.Id); err != nil || password(command) != "repo-password-b2" {
			t.Errorf("%s on the job's server: %v", name, err)
		}
		if command, err := list(stranger.Id, f.target, job.Id); !refusedWith(command, err, refused) {
			t.Errorf("%s on another server: %v", name, err)
		}
		other := f.job(name+"-other", map[string]any{"host": stranger.Id, "source_kind": schema.SourceVolume, "source": "x", "targets": []string{}})
		if command, err := list(stranger.Id, f.target, other.Id); !refusedWith(command, err, refused) {
			t.Errorf("%s for a job that does not back up to the target: %v", name, err)
		}
		if command, err := list(stranger.Id, f.target, "nosuchjob000000"); !refusedWith(command, err, refused) {
			t.Errorf("%s for a job that is gone: %v", name, err)
		}
	}
	// a local target is read on its own server only, whatever the job
	disk := f.addTarget("disk", backup.KindLocal, f.host, backup.Config{Path: "/srv/restic"}, backup.Credentials{})
	local := f.job("local", map[string]any{"host": jobHost.Id, "source_kind": schema.SourceVolume, "source": "x", "targets": []string{disk.Id}})
	if command, err := snapshots(jobHost.Id, disk, local.Id); !refusedWith(command, err, refused) {
		t.Errorf("a local target on a job's server: %v", err)
	}
	if command, err := snapshots(f.host.Id, disk, local.Id); err != nil || password(command) != "repo-password-disk" {
		t.Errorf("a local target on its server: %v", err)
	}
	// a job whose label is gone no longer reads the target
	stack := f.save(schema.Stacks, map[string]any{"host": jobHost.Id, "name": "shop"})
	job.Load(map[string]any{"origin": schema.JobLabel, "stack": stack.Id, "service": "db", "label_removed": true})
	if err := f.app.Save(job); err != nil {
		t.Fatal(err)
	}
	if command, err := snapshots(jobHost.Id, f.target, job.Id); !refusedWith(command, err, refused) {
		t.Errorf("a job whose label is gone: %v", err)
	}
}
