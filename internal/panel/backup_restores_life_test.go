package panel

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// previewed is a draft whose preview is ready.
func (f *jobFixture) previewed(job *core.Record, preview map[string]any, fields map[string]any) *core.Record {
	f.t.Helper()
	now := time.Now().UTC()
	all := map[string]any{"status": schema.RestorePreviewed, "preview": preview, "preview_at": now, "expires_at": now.Add(30 * time.Minute),
		"requested_label": "superuser root@example.com", "label": "data ← b2 · 3f9a1c2e → original location"}
	for key, value := range fields {
		all[key] = value
	}
	return f.restore(job, all)
}

func (f *jobFixture) start(restore *core.Record, token string, confirm bool) (int, map[string]any) {
	f.t.Helper()
	return f.send(http.MethodPost, "/api/pc/backup-restores/"+restore.Id+"/start", token, map[string]any{"confirm_overwrite": confirm})
}

// TestOverwritingNeedsExplicitConfirmation (#30, AC3): a restore to the
// original location, or into a folder that holds files, starts only once
// someone confirmed the overwrite, and its command says so; a restore into
// an empty folder needs no confirmation and says no overwrite, so the agent
// refuses the folder if it filled meanwhile. The command stops exactly the
// containers the preview showed, at the roots the preview found.
func TestOverwritingNeedsExplicitConfirmation(t *testing.T) {
	f := newJobFixture(t)
	operator, token := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	other := testutil.NewHost(t, f.app, "node-2")
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	stream := f.agent(f.host)
	elsewhere := f.agent(other)

	original := f.previewed(job, previewResult(0, "app-web-1", "app-db-1"), map[string]any{"replace": true, "stack_name": "app"})
	status, body := f.start(original, token, false)
	if status != http.StatusConflict || body["message"] != "The destination already holds files: confirm that the restore overwrites them." {
		t.Errorf("unconfirmed: %d %v", status, body)
	}
	if !f.quiet(stream) {
		t.Error("an unconfirmed restore went out")
	}
	status, body = f.start(original, token, true)
	if status != http.StatusOK || body["status"] != schema.RestoreQueued || body["overwrite"] != true || body["confirmed_by"] != operator.Id ||
		body["confirmed_label"] != "operator@example.com" {
		t.Fatalf("confirmed: %d %v", status, body)
	}
	command := f.received(stream)
	restore := command.GetBackupRestore()
	if command.GetId() != body["command"] || restore.GetDryRun() || !restore.GetOverwrite() || !restore.GetReplace() || restore.GetStack() != "app" ||
		strings.Join(restore.GetConfirmedStops(), ",") != "app-web-1,app-db-1" || len(restore.GetOriginal()) != 1 ||
		restore.GetOriginal()[0].GetSource() != "/volumes/app_data" || restore.GetRepository().GetPassword() != "repo-password-b2" {
		t.Errorf("the restore: %v", command)
	}

	// a folder that is empty needs no confirmation, and the command says so:
	// it replaces nothing, as nothing is there to delete, and the agent
	// replaces only what it was told it overwrites
	empty := previewResult(0)
	empty["roots"] = []any{map[string]any{"index": 0, "source": "/volumes/app_data", "destination": "/srv/restored", "in_snapshot": true, "exists": false, "entries": 0}}
	folder := f.previewed(job, empty, map[string]any{"mode": schema.RestorePath, "host": other.Id, "roots": nil, "path": "/srv/restored", "source": "/volumes/app_data", "replace": true})
	status, body = f.start(folder, f.admin, false)
	if status != http.StatusOK || body["overwrite"] != false || body["replace"] != false {
		t.Fatalf("an empty folder: %d %v", status, body)
	}
	if command := f.received(elsewhere); command.GetBackupRestore().GetOverwrite() || command.GetBackupRestore().GetReplace() ||
		command.GetBackupRestore().GetTargetPath() != "/srv/restored" || command.GetBackupRestore().GetSource() != "/volumes/app_data" {
		t.Errorf("into an empty folder: %v", command)
	}

	// a folder that holds files, or files the restore would change, needs it
	for name, change := range map[string]func(map[string]any){
		"entries": func(preview map[string]any) {
			preview["roots"] = []any{map[string]any{"index": 0, "in_snapshot": true, "exists": true, "entries": 2}}
		},
		"updated": func(preview map[string]any) {
			preview["counts"] = map[string]any{"new": 0, "updated": 1, "unchanged": 0, "deleted": 0}
		},
		"deleted": func(preview map[string]any) {
			preview["counts"] = map[string]any{"new": 0, "updated": 0, "unchanged": 0, "deleted": 1}
		},
	} {
		preview := previewResult(0)
		preview["roots"] = empty["roots"]
		change(preview)
		filled := f.previewed(job, preview, map[string]any{"mode": schema.RestorePath, "host": other.Id, "roots": nil, "path": "/srv/" + name, "source": "/"})
		if status, body := f.start(filled, f.admin, false); status != http.StatusConflict {
			t.Errorf("a folder with %s: %d %v", name, status, body)
		}
	}
}

// TestARestoreCannotStartWithoutAFreshPreview (#30, AC3): a restore starts
// from a preview that is ready and at most 30 minutes old, once, by the
// role of its mode, and only while nothing else holds its data; what is not
// in the snapshot is not restored; starting it is a security event.
func TestARestoreCannotStartWithoutAFreshPreview(t *testing.T) {
	f := newJobFixture(t)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	f.agent(f.host)
	expect := func(name string, restore *core.Record, token string, status int, message string) {
		t.Helper()
		if got, body := f.start(restore, token, true); got != status || (message != "" && body["message"] != message) {
			t.Errorf("%s: %d %v", name, got, body)
		}
	}

	expect("previewing", f.restore(job, nil), operator, http.StatusConflict, "The preview is not ready yet.")
	old := f.previewed(job, previewResult(0), map[string]any{"expires_at": time.Now().UTC().Add(-time.Second)})
	expect("an old preview", old, operator, http.StatusConflict, "The preview is older than 30 minutes: preview again.")
	if stored := f.stored(schema.BackupRestores, old.Id); stored.GetString("status") != schema.RestoreExpired {
		t.Errorf("an old preview is %s", stored.GetString("status"))
	}
	expect("expired", old, operator, http.StatusConflict, "The preview is older than 30 minutes: preview again.")
	expect("a failed preview", f.restore(job, map[string]any{"status": schema.RestoreFailed, "error": "Fatal: wrong password ***."}), operator,
		http.StatusConflict, "Fatal: wrong password ***.")
	expect("started", f.restore(job, map[string]any{"status": schema.RestoreQueued}), operator, http.StatusConflict, "The restore was started already.")
	expect("ended", f.restore(job, map[string]any{"status": schema.RestoreSucceeded}), operator, http.StatusConflict, "The restore has ended: preview it again.")
	expect("an operator's folder", f.previewed(job, previewResult(0), map[string]any{"mode": schema.RestorePath, "roots": nil, "path": "/srv/x", "source": "/"}),
		operator, http.StatusForbidden, "You are not allowed to do this.")
	expect("a download", f.restore(job, map[string]any{"mode": schema.RestoreDownload, "status": schema.RestoreQueued, "roots": nil, "path": "/"}),
		f.admin, http.StatusBadRequest, "A download starts from its link.")
	missing := previewResult(0)
	missing["roots"] = []any{map[string]any{"index": 0, "source": "/volumes/app_data", "in_snapshot": false}}
	expect("nothing in the snapshot", f.previewed(job, missing, nil), operator, http.StatusConflict, "None of what the job backs up is in this snapshot.")
	expect("a folder not in the snapshot", f.previewed(job, missing, map[string]any{"mode": schema.RestorePath, "roots": nil, "path": "/srv/x", "source": "/volumes/app_data"}),
		f.admin, http.StatusConflict, "/volumes/app_data is not in this snapshot.")

	// another restore of the same data started meanwhile
	ready := f.previewed(job, previewResult(0), nil)
	f.restore(job, map[string]any{"status": schema.RestoreRunning})
	expect("while another restore runs", ready, operator, http.StatusConflict, "A restore into app_data is already under way.")

	// once, as the operator who started it
	fresh := f.previewed(job, previewResult(0), map[string]any{"roots": []restoreRoot{{Source: "/volumes/other", Volume: "other"}}})
	if status, body := f.start(fresh, operator, true); status != http.StatusOK {
		t.Fatalf("started: %d %v", status, body)
	}
	expect("again", fresh, operator, http.StatusConflict, "The restore was started already.")
	entries, err := f.app.FindRecordsByFilter(schema.AuditLog, "action={:action}", "", 0, 0, map[string]any{"action": audit.ActionBackupRestore})
	if err != nil || len(entries) != 1 || entries[0].GetString("actor_label") != "operator@example.com" ||
		!strings.Contains(entries[0].GetString("snapshot"), `"other"`) {
		t.Errorf("audited: %v %v", entries, err)
	}
}

// TestARestoreWaitsForAMoveOfItsStack (#30, SAFE): while the databases of a
// stack move, the move stops the application and deploys it once it ends;
// a restore queued behind it would hold that deploy, and the application
// would stay down. A restore of the stack's data is refused as it is made,
// and one whose preview stops a container of the stack, whatever its mode,
// is refused at its start; once the move ended, it starts.
func TestARestoreWaitsForAMoveOfItsStack(t *testing.T) {
	f := newJobFixture(t)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	notes := f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "notes"})
	f.save(schema.Volumes, map[string]any{"host": f.host.Id, "name": "notes_data", "labels": map[string]string{"com.docker.compose.project": "notes"}})
	job := f.job("notes", map[string]any{"source_kind": schema.SourceVolume, "source": "notes_data"})
	stream := f.agent(f.host)
	move := f.save(schema.DBMoves, map[string]any{"host": f.host.Id, "stack": notes.Id, "target_name": "pool", "status": schema.MoveRunning})
	moving := "Databases of notes are moving to pool: restore once the move ends."

	status, body := f.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/restores", operator,
		map[string]any{"target": f.target.Id, "snapshot": snapshotID, "mode": "original"})
	if status != http.StatusConflict || body["message"] != moving {
		t.Errorf("a restore made during the move: %d %v", status, body)
	}
	if !f.quiet(stream) {
		t.Error("a preview went out during the move")
	}

	stops := previewResult(0, "notes-db-1")
	stops["would_stop"] = []any{map[string]any{"name": "notes-db-1", "project": "notes"}}
	original := f.previewed(job, stops, map[string]any{"roots": []restoreRoot{{Source: "/volumes/notes_data", Volume: "notes_data"}}, "stack_name": "notes"})
	empty := previewResult(0)
	empty["roots"] = []any{map[string]any{"index": 0, "source": "/", "destination": "/srv/notes", "in_snapshot": true, "exists": false, "entries": 0}}
	empty["would_stop"] = stops["would_stop"]
	// a restore into a folder that the stack mounts names no stack
	folder := f.previewed(job, empty, map[string]any{"mode": schema.RestorePath, "roots": nil, "path": "/srv/notes", "source": "/"})
	for name, test := range map[string]struct {
		restore *core.Record
		token   string
	}{"to the original location": {original, operator}, "into a folder the stack mounts": {folder, f.admin}} {
		if status, body := f.start(test.restore, test.token, true); status != http.StatusConflict || body["message"] != moving {
			t.Errorf("a restore %s started during the move: %d %v", name, status, body)
		}
		if stored := f.stored(schema.BackupRestores, test.restore.Id); stored.GetString("status") != schema.RestorePreviewed {
			t.Errorf("a restore %s refused during the move is %s", name, stored.GetString("status"))
		}
	}
	if !f.quiet(stream) {
		t.Error("a restore went out during the move")
	}

	// once the move ended
	move.Set("status", schema.MoveDone)
	if err := f.app.Save(move); err != nil {
		t.Fatal(err)
	}
	if status, body := f.start(original, operator, true); status != http.StatusOK || body["status"] != schema.RestoreQueued {
		t.Errorf("a restore started once the move ended: %d %v", status, body)
	}
}

// TestARestoreStartsOnceThoughStartedTwiceAtOnce (#30, SAFE): a draft
// started by several requests at once becomes one restore with one
// command.
func TestARestoreStartsOnceThoughStartedTwiceAtOnce(t *testing.T) {
	f := newJobFixture(t)
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	f.agent(f.host)
	draft := f.previewed(job, previewResult(0), nil)
	const tries = 16
	answers := make(chan reply, tries)
	start := make(chan struct{})
	for range tries {
		go func() {
			<-start
			status, body := f.send(http.MethodPost, "/api/pc/backup-restores/"+draft.Id+"/start", f.admin, map[string]any{"confirm_overwrite": true})
			answers <- reply{status, body}
		}()
	}
	close(start)
	started := 0
	for range tries {
		got := f.wait(answers)
		if got.status == http.StatusOK {
			started++
		}
	}
	restores, err := f.app.FindRecordsByFilter(schema.Commands, "type='backup.restore'", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	real := 0
	for _, command := range restores {
		if !restoreCommandPayloadOf(command).DryRun {
			real++
		}
	}
	if started != 1 || real != 1 {
		t.Errorf("%d starts, %d restores", started, real)
	}
}

// TestARestoreRunsAndEndsAsItsAgentSays (#30, S): a restore runs once its
// agent starts it, and ends as its agent said: what it restored, the
// containers it stopped, and its output, with the target's secrets hidden.
// A failure, or an application that did not stay up, raises an alert of
// the data, which the next restore of it that goes well resolves; one that
// waited its whole time never started.
func TestARestoreRunsAndEndsAsItsAgentSays(t *testing.T) {
	f := newJobFixture(t)
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	stream := f.agent(f.host)
	run := func(answer agentv1.CommandState, result map[string]any, message string) *core.Record {
		t.Helper()
		draft := f.previewed(job, previewResult(0, "app-web-1"), nil)
		if status, body := f.start(draft, f.admin, true); status != http.StatusOK {
			t.Fatalf("started: %d %v", status, body)
		}
		command := f.received(stream)
		f.report(f.host, command.GetId(), 1, agentv1.CommandState_COMMAND_STATE_RUNNING,
			&agentv1.CommandProgress{Step: "restore", Percent: 40, Done: 400, Total: 1000, Unit: "bytes"}, nil, "")
		if running := f.stored(schema.BackupRestores, draft.Id); running.GetString("status") != schema.RestoreRunning || running.GetDateTime("started_at").IsZero() {
			t.Errorf("running: %v", running.FieldsData())
		}
		f.report(f.host, command.GetId(), 2, answer, nil, result, message)
		return f.stored(schema.BackupRestores, draft.Id)
	}
	alerts := func() []*core.Record {
		t.Helper()
		open, err := f.app.FindRecordsByFilter(schema.Alerts, "rule='backup_restore' && status!='resolved'", "", 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		return open
	}
	key := f.host.Id + ":volume:app_data"

	done := run(agentv1.CommandState_COMMAND_STATE_SUCCEEDED, map[string]any{"dry_run": false, "files_restored": 10, "bytes_restored": 1000,
		"files_deleted": 2, "errors": 0, "app_stopped": []any{map[string]any{"id": "c0ffee", "name": "app-web-1"}},
		"app_started": []any{"app-web-1"}, "app_not_running": []any{}, "partial": false, "log": "restic said the-secret-key"}, "")
	if done.GetString("status") != schema.RestoreSucceeded || done.GetInt("files_restored") != 10 || done.GetInt("bytes_restored") != 1000 ||
		done.GetInt("files_deleted") != 2 || !strings.Contains(done.GetString("app_stopped"), "app-web-1") || done.GetString("log") != "restic said ***" ||
		done.GetDateTime("finished_at").IsZero() || done.GetString("error") != "" {
		t.Errorf("succeeded: %v", done.FieldsData())
	}
	// settled again, as self-healing may: once
	f.p.restoreFinished(f.stored(schema.Commands, done.GetString("command")))
	if restored, _ := f.app.FindRecordsByFilter(schema.Events, "kind='backup.restored'", "", 0, 0); len(restored) != 1 {
		t.Errorf("%d restored events", len(restored))
	}

	failed := run(agentv1.CommandState_COMMAND_STATE_FAILED, map[string]any{"partial": true, "files_restored": 3, "app_started": []any{"app-web-1"}},
		"restic failed: wrong key the-secret-key")
	if failed.GetString("status") != schema.RestoreFailed || failed.GetString("error") != "Restic failed: wrong key ***." || !failed.GetBool("partial") {
		t.Errorf("failed: %v", failed.FieldsData())
	}
	open := alerts()
	if len(open) != 1 || open[0].GetString("target") != key || open[0].GetString("severity") != schema.SeverityCritical ||
		open[0].GetString("message") != "The restore of data into app_data failed: Restic failed: wrong key ***. The application was started again." {
		t.Errorf("the alert of a failure: %v", open)
	}

	down := run(agentv1.CommandState_COMMAND_STATE_SUCCEEDED, map[string]any{"files_restored": 1, "app_not_running": []any{"app-web-1"}}, "")
	if down.GetString("app_problem") != "app-web-1 did not stay up after the restore." {
		t.Errorf("an application down: %v", down.FieldsData())
	}
	if open := alerts(); len(open) != 1 || !strings.Contains(open[0].GetString("message"), "app-web-1 did not stay up") {
		t.Errorf("the alert of an application down: %v", open)
	}
	if downEvents, _ := f.app.FindRecordsByFilter(schema.Events, "kind='backup.restore_app_down'", "", 0, 0); len(downEvents) != 1 {
		t.Errorf("%d events of an application down", len(downEvents))
	}
	run(agentv1.CommandState_COMMAND_STATE_SUCCEEDED, map[string]any{"files_restored": 1}, "")
	if open := alerts(); len(open) != 0 {
		t.Errorf("a restore that went well left %v", open)
	}

	// one that waited its whole time behind another restore of its stack
	waiting := f.queuedRestore(job, stream)
	command := f.stored(schema.Commands, waiting.GetString("command"))
	if left := time.Until(command.GetDateTime("expires_at").Time()); left > 30*time.Minute || left < 29*time.Minute {
		t.Errorf("it may wait %s in the queue", left)
	}
	f.host.Set("status", schema.HostOnline)
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	command.Set("expires_at", time.Now().UTC().Add(-time.Second))
	if err := f.app.Save(command); err != nil {
		t.Fatal(err)
	}
	f.p.Commands.Reap()
	if never := f.stored(schema.BackupRestores, waiting.Id); never.GetString("status") != schema.RestoreFailed ||
		never.GetString("error") != "It waited 30 minutes while the server node-1 was busy, and never started: nothing was stopped or changed." {
		t.Errorf("never started: %v", never.FieldsData())
	}
}

// queuedRestore starts a restore of a stack that waits in the queue
// behind another restore of the same stack, which its agent got.
func (f *jobFixture) queuedRestore(job *core.Record, stream *commands.Stream) *core.Record {
	f.t.Helper()
	first := f.previewed(job, previewResult(0), map[string]any{"stack_name": "app", "roots": []restoreRoot{{Source: "/volumes/app_one", Volume: "app_one"}}})
	if status, body := f.start(first, f.admin, true); status != http.StatusOK {
		f.t.Fatalf("started the first: %d %v", status, body)
	}
	f.received(stream)
	second := f.previewed(job, previewResult(0), map[string]any{"stack_name": "app", "roots": []restoreRoot{{Source: "/volumes/app_two", Volume: "app_two"}}})
	if status, body := f.start(second, f.admin, true); status != http.StatusOK {
		f.t.Fatalf("started the second: %d %v", status, body)
	}
	if !f.quiet(stream) {
		f.t.Fatal("a second restore of the stack went out")
	}
	return f.stored(schema.BackupRestores, second.Id)
}

// TestAStoppedRestoreReportsTheContainersItStarted (#30, SAFE): a restore
// stopped in the queue is cancelled at once, having changed nothing; one
// that runs is stopping until its agent says it started the containers
// again, which the record then says, with what was restored so far.
func TestAStoppedRestoreReportsTheContainersItStarted(t *testing.T) {
	f := newJobFixture(t)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	stream := f.agent(f.host)

	queued := f.queuedRestore(job, stream)
	status, body := f.send(http.MethodPost, "/api/pc/backup-restores/"+queued.Id+"/cancel", operator, nil)
	if status != http.StatusOK || body["status"] != schema.RestoreCancelled || body["error"] != "Cancelled before it started: nothing was stopped or changed." {
		t.Errorf("stopped in the queue: %d %v", status, body)
	}

	running := f.previewed(job, previewResult(0, "app-web-1"), nil)
	if status, body := f.start(running, f.admin, true); status != http.StatusOK {
		t.Fatalf("started: %d %v", status, body)
	}
	command := f.received(stream)
	f.report(f.host, command.GetId(), 1, agentv1.CommandState_COMMAND_STATE_RUNNING, nil, nil, "")
	status, body = f.send(http.MethodPost, "/api/pc/backup-restores/"+running.Id+"/cancel", operator, nil)
	if status != http.StatusAccepted || body["stopping"] != true {
		t.Fatalf("stopped while running: %d %v", status, body)
	}
	select {
	case message := <-stream.Out():
		if message.GetCancel().GetCommandId() != command.GetId() {
			t.Errorf("the agent got %v", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the agent was not told to stop")
	}
	if stopping := f.stored(schema.BackupRestores, running.Id); stopping.GetString("status") != schema.RestoreRunning ||
		stopping.GetString("stop_requested_label") != "operator@example.com" {
		t.Errorf("stopping: %v", stopping.FieldsData())
	}
	if cancel := f.report(f.host, command.GetId(), 2, agentv1.CommandState_COMMAND_STATE_RUNNING, nil, nil, ""); !cancel {
		t.Error("an update while stopping was not told to stop")
	}
	f.report(f.host, command.GetId(), 3, agentv1.CommandState_COMMAND_STATE_CANCELLED,
		nil, map[string]any{"partial": true, "files_restored": 3, "app_stopped": []any{map[string]any{"id": "c0ffee", "name": "app-web-1"}},
			"app_started": []any{"app-web-1"}}, "The restore was stopped before it was complete.")
	stopped := f.stored(schema.BackupRestores, running.Id)
	if stopped.GetString("status") != schema.RestoreCancelled || !stopped.GetBool("partial") || stopped.GetInt("files_restored") != 3 ||
		stopped.GetString("error") != "Stopped by operator@example.com before it was complete: the destination holds part of the snapshot. The application was started again." {
		t.Errorf("stopped: %v", stopped.FieldsData())
	}
	if events, _ := f.app.FindRecordsByFilter(schema.Events, "kind='backup.restore_stopped'", "", 0, 0); len(events) != 2 {
		t.Errorf("%d stopped events", len(events))
	}

	if status, body := f.send(http.MethodPost, "/api/pc/backup-restores/"+running.Id+"/cancel", operator, nil); status != http.StatusBadRequest ||
		body["message"] != "The restore has ended." {
		t.Errorf("stopped again: %d %v", status, body)
	}
	if status, body := f.send(http.MethodPost, "/api/pc/backup-restores/"+f.restore(job, nil).Id+"/cancel", operator, nil); status != http.StatusBadRequest ||
		body["message"] != "This is a draft: discard it instead." {
		t.Errorf("a draft: %d %v", status, body)
	}
}
