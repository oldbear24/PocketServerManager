package panel

import (
	"net/http"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// ended moves a command to a final state the way a panel that stopped in
// between would leave it: without its finishers.
func (f *jobFixture) ended(command *core.Record, status, message string, result map[string]any) {
	f.t.Helper()
	command = f.stored(schema.Commands, command.Id)
	command.Set("status", schema.CommandDispatched)
	if err := f.app.Save(command); err != nil {
		f.t.Fatal(err)
	}
	command.Load(map[string]any{"status": status, "error": message, "result": result, "finished_at": time.Now().UTC()})
	if err := f.app.Save(command); err != nil {
		f.t.Fatal(err)
	}
}

// TestARestoreThePanelMissedIsSettled (#30, SAFE): a restore or a preview
// whose command ended while the panel was not there to finish it is
// settled from the command; one whose command is gone fails, saying where
// to look; a stopped restore tells its agent again; a preview too old
// expires.
func TestARestoreThePanelMissedIsSettled(t *testing.T) {
	f := newJobFixture(t)
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})

	done := f.restore(job, map[string]any{"status": schema.RestoreRunning, "overwrite": true, "confirmed_label": "superuser root@example.com",
		"roots": []restoreRoot{{Source: "/volumes/one", Volume: "one"}}})
	f.ended(f.link(done, false), schema.CommandSucceeded, "", map[string]any{"files_restored": 4})
	gone := f.restore(job, map[string]any{"status": schema.RestoreRunning, "roots": []restoreRoot{{Source: "/volumes/two", Volume: "two"}}})
	lost := f.link(gone, false)
	previewed := f.restore(job, map[string]any{"roots": []restoreRoot{{Source: "/volumes/three", Volume: "three"}}})
	f.ended(f.link(previewed, true), schema.CommandFailed, "Fatal: no such snapshot", nil)
	orphan := f.restore(job, map[string]any{"roots": []restoreRoot{{Source: "/volumes/four", Volume: "four"}}})
	stale := f.restore(job, map[string]any{"status": schema.RestorePreviewed, "expires_at": time.Now().UTC().Add(-time.Minute)})
	if err := f.app.Delete(lost); err != nil {
		t.Fatal(err)
	}

	f.p.settleRestores()
	for _, test := range []struct {
		record        *core.Record
		status, error string
	}{
		{done, schema.RestoreSucceeded, ""},
		{gone, schema.RestoreFailed, "The restore ended without the panel recording how: check the application on node-1."},
		{previewed, schema.RestoreFailed, "Fatal: no such snapshot."},
		{orphan, schema.RestoreFailed, "The preview ended without the panel recording it: preview again."},
		{stale, schema.RestoreExpired, ""},
	} {
		stored := f.stored(schema.BackupRestores, test.record.Id)
		if stored.GetString("status") != test.status || stored.GetString("error") != test.error {
			t.Errorf("%s: %s %q", test.record.Id, stored.GetString("status"), stored.GetString("error"))
		}
	}
	if stored := f.stored(schema.BackupRestores, done.Id); stored.GetInt("files_restored") != 4 {
		t.Errorf("settled from its command: %v", stored.FieldsData())
	}

	// a stop that the agent may have missed is sent again
	stream := f.agent(f.host)
	running := f.previewed(job, previewResult(0), map[string]any{"roots": []restoreRoot{{Source: "/volumes/five", Volume: "five"}}})
	if status, body := f.start(running, f.admin, true); status != http.StatusOK {
		t.Fatalf("started: %d %v", status, body)
	}
	command := f.received(stream)
	f.report(f.host, command.GetId(), 1, agentv1.CommandState_COMMAND_STATE_RUNNING, nil, nil, "")
	if status, _ := f.send(http.MethodPost, "/api/pc/backup-restores/"+running.Id+"/cancel", f.admin, nil); status != http.StatusAccepted {
		t.Fatalf("stopped: %d", status)
	}
	<-stream.Out()
	f.p.settleRestores()
	select {
	case message := <-stream.Out():
		if message.GetCancel().GetCommandId() != command.GetId() || message.GetCancel().GetReason() != "Cancelled by admin@example.com" {
			t.Errorf("told again: %v", message)
		}
	case <-time.After(5 * time.Second):
		t.Error("the stop was not sent again")
	}
}

// TestRestoresThePanelMissedAreSettledAtItsStart (#30, SAFE): the restores
// a panel that was down missed are settled as it starts, before any
// request.
func TestRestoresThePanelMissedAreSettledAtItsStart(t *testing.T) {
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)
	p := New(app, &Config{MasterKey: testutil.MasterKey, GRPCAddr: "127.0.0.1:0"}, testUI)
	p.Register()
	t.Cleanup(p.later.stop)
	if err := p.loadMasterKey(); err != nil {
		t.Fatal(err)
	}
	// the panel that was down had its CA (#32)
	caOfAnEarlierStart(t, app)
	f := &jobFixture{t: t, p: p, app: app, host: testutil.NewHost(t, app, "node-1")}
	f.target = f.addTarget("b2", backup.KindS3, f.host, backup.Config{Endpoint: "https://s3.example.com", Bucket: "backups"},
		backup.Credentials{AccessKeyID: "key-id", SecretAccessKey: "the-secret-key"})
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	lost := f.restore(job, map[string]any{"status": schema.RestoreRunning})

	testutil.Handler(t, app)
	if stored := f.stored(schema.BackupRestores, lost.Id); stored.GetString("status") != schema.RestoreFailed {
		t.Errorf("after the start: %v", stored.FieldsData())
	}
}

// pull is a command of the fixture's server, for a record to name.
func (f *jobFixture) pull() *core.Record {
	f.t.Helper()
	return f.command(f.host.Id, &agentv1.Command{Payload: &agentv1.Command_ImagePull{ImagePull: &agentv1.ImagePull{Image: "nginx:1"}}})
}

// TestOldRestoresGo (#30): drafts nobody started go after a day, and
// restores and downloads older than 90 days go, but each job's newest
// restore that was started, however it ended: a newer draft or download
// does not count, so a job keeps its restore history.
func TestOldRestoresGo(t *testing.T) {
	f := newJobFixture(t)
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	other := f.job("other", map[string]any{"source_kind": schema.SourceVolume, "source": "other"})
	age := func(record *core.Record, ago time.Duration) {
		t.Helper()
		_, err := f.app.DB().NewQuery("UPDATE backup_restores SET created={:at} WHERE id={:id}").
			Bind(dbx.Params{"at": time.Now().UTC().Add(-ago).Format("2006-01-02 15:04:05.000Z"), "id": record.Id}).Execute()
		if err != nil {
			t.Fatal(err)
		}
	}
	oldDraft := f.restore(job, map[string]any{"status": schema.RestorePreviewed})
	age(oldDraft, 25*time.Hour)
	oldFailedPreview := f.restore(job, map[string]any{"status": schema.RestoreFailed})
	age(oldFailedPreview, 25*time.Hour)
	newDraft := f.restore(job, map[string]any{"status": schema.RestoreExpired})
	age(newDraft, time.Hour)
	ancient := f.restore(job, map[string]any{"status": schema.RestoreSucceeded, "command": f.pull().Id})
	age(ancient, 100*24*time.Hour)
	// the job's newest restore, cancelled in the queue, stays though a
	// draft is newer
	newer := f.restore(job, map[string]any{"status": schema.RestoreCancelled, "command": f.pull().Id})
	age(newer, 95*24*time.Hour)
	onlyOne := f.restore(other, map[string]any{"status": schema.RestoreFailed, "command": f.pull().Id})
	age(onlyOne, 200*24*time.Hour)
	running := f.restore(job, map[string]any{"status": schema.RestoreRunning})
	age(running, 200*24*time.Hour)
	// a job whose restore to a new folder is followed by downloads and a
	// failed preview: the restore stays, the old download goes
	third := f.job("third", map[string]any{"source_kind": schema.SourceVolume, "source": "third"})
	folder := f.restore(third, map[string]any{"status": schema.RestoreSucceeded, "mode": schema.RestorePath, "roots": nil, "path": "/srv/x", "command": f.pull().Id})
	age(folder, 120*24*time.Hour)
	oldDownload := f.restore(third, map[string]any{"status": schema.RestoreSucceeded, "mode": schema.RestoreDownload, "roots": nil, "path": "/", "command": f.pull().Id})
	age(oldDownload, 100*24*time.Hour)
	newDownload := f.restore(third, map[string]any{"status": schema.RestoreSucceeded, "mode": schema.RestoreDownload, "roots": nil, "path": "/", "command": f.pull().Id})
	age(newDownload, time.Hour)
	failedPreview := f.restore(third, map[string]any{"status": schema.RestoreFailed})
	age(failedPreview, 2*time.Hour)

	f.p.pruneRestores()
	for record, kept := range map[*core.Record]bool{oldDraft: false, oldFailedPreview: false, newDraft: true, ancient: false,
		newer: true, onlyOne: true, running: true, folder: true, oldDownload: false, newDownload: true, failedPreview: true} {
		if _, err := f.app.FindRecordById(schema.BackupRestores, record.Id); (err == nil) != kept {
			t.Errorf("%s (%s): kept %v", record.Id, record.GetString("status"), err == nil)
		}
	}
}
