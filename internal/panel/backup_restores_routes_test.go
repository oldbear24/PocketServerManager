package panel

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// previewResult is what an agent's preview of a restore answers.
func previewResult(entries int, wouldStop ...string) map[string]any {
	stops := []any{}
	for _, name := range wouldStop {
		stops = append(stops, map[string]any{"name": name, "project": "app"})
	}
	return map[string]any{"dry_run": true,
		"snapshot":   map[string]any{"id": snapshotID, "time": "2026-11-10T12:00:00Z", "paths": []any{"/volumes/app_data"}},
		"roots":      []any{map[string]any{"index": 0, "source": "/volumes/app_data", "destination": "app_data", "in_snapshot": true, "exists": true, "entries": entries}},
		"not_in_job": []any{}, "counts": map[string]any{"new": 3, "updated": 0, "unchanged": 1, "deleted": 0},
		"bytes": map[string]any{"new": 300, "updated": 0, "deleted": 0}, "would_stop": stops, "total_files": 4, "total_bytes": 400, "notes": []any{}}
}

// stored reads a record as it is now.
func (f *jobFixture) stored(collection, id string) *core.Record {
	f.t.Helper()
	record, err := f.app.FindRecordById(collection, id)
	if err != nil {
		f.t.Fatalf("%s %s: %v", collection, id, err)
	}
	return record
}

// TestARestoreStartsWithAPreview (#30, S): an operator's restore to the
// original location is a draft whose preview goes to the job's server: the
// job's volume, its Compose project, replace; the preview is kept, with the
// target's secrets hidden, for 30 minutes, and its command does not stay.
func TestARestoreStartsWithAPreview(t *testing.T) {
	f := newJobFixture(t)
	operator, token := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	f.save(schema.Volumes, map[string]any{"host": f.host.Id, "name": "app_data", "labels": map[string]string{"com.docker.compose.project": "app"}})
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	stream := f.agent(f.host)

	status, body := f.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/restores", token,
		map[string]any{"target": f.target.Id, "snapshot": snapshotID, "mode": "original", "replace": true})
	if status != http.StatusAccepted || body["status"] != schema.RestorePreviewing {
		t.Fatalf("created: %d %v", status, body)
	}
	restore := f.stored(schema.BackupRestores, body["id"].(string))
	roots := rootsOf(restore)
	if len(roots) != 1 || roots[0] != (restoreRoot{Source: "/volumes/app_data", Volume: "app_data"}) || restore.GetString("stack_name") != "app" ||
		restore.GetString("host") != f.host.Id || restore.GetString("requested_by") != operator.Id || !restore.GetBool("replace") ||
		restore.GetString("label") != "data ← b2 · 3f9a1c2e → original location" {
		t.Errorf("the draft: %v", restore.FieldsData())
	}
	if left := time.Until(restore.GetDateTime("expires_at").Time()); left < 29*time.Minute || left > 30*time.Minute {
		t.Errorf("good for %s", left)
	}
	command := f.received(stream)
	preview := command.GetBackupRestore()
	if command.GetId() != restore.GetString("preview_command") || !preview.GetDryRun() || preview.GetRestoreId() != restore.Id ||
		preview.GetJobTag() != "job:"+job.Id || preview.GetStack() != "app" || !preview.GetReplace() || preview.GetOverwrite() ||
		len(preview.GetOriginal()) != 1 || preview.GetOriginal()[0].GetVolume() != "app_data" || preview.GetRepository().GetPassword() != "repo-password-b2" {
		t.Errorf("the preview: %v", command)
	}

	result := previewResult(3, "app-web-1")
	result["notes"] = []any{"restic said the-secret-key"}
	f.answer(f.host, command.GetId(), agentv1.CommandState_COMMAND_STATE_SUCCEEDED, result, "")
	restore = f.stored(schema.BackupRestores, restore.Id)
	kept, ok := previewOf(restore)
	if restore.GetString("status") != schema.RestorePreviewed || !ok || len(kept.WouldStop) != 1 || kept.Roots[0].Entries != 3 ||
		strings.Contains(restore.GetString("preview"), "the-secret-key") || restore.GetDateTime("preview_at").IsZero() ||
		!restore.GetDateTime("snapshot_time").Time().Equal(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("previewed: %v", restore.FieldsData())
	}
	f.eventually("the preview's command going", func() bool { return f.gone(command.GetId()) })

	// a preview that fails says why, without the target's secrets
	status, body = f.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/restores", token,
		map[string]any{"target": f.target.Id, "snapshot": snapshotID, "mode": "original"})
	if status != http.StatusAccepted {
		t.Fatalf("created again: %d %v", status, body)
	}
	command = f.received(stream)
	f.answer(f.host, command.GetId(), agentv1.CommandState_COMMAND_STATE_FAILED, nil, "Fatal: wrong password the-secret-key")
	failed := f.stored(schema.BackupRestores, body["id"].(string))
	if failed.GetString("status") != schema.RestoreFailed || failed.GetString("error") != "Fatal: wrong password ***." {
		t.Errorf("a preview that failed: %v", failed.FieldsData())
	}
}

// TestARestoreIsCheckedAsItIsMade (#30, AC2/SAFE): a restore is refused,
// with a sentence, when what it names is not there or not the job's, when
// it picks too much or replaces a selection, when the original location is
// not where it goes or a dump is restored there, when a folder is the
// system's or a local target's repository, when a local target's snapshot
// would go to another server, and while its server is offline, a backup of
// the job runs, or another restore writes the same data.
func TestARestoreIsCheckedAsItIsMade(t *testing.T) {
	f := newJobFixture(t)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	other := testutil.NewHost(t, f.app, "node-2")
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	create := func(token string, job *core.Record, body map[string]any) (int, string) {
		t.Helper()
		all := map[string]any{"target": f.target.Id, "snapshot": snapshotID, "mode": "original"}
		for key, value := range body {
			all[key] = value
		}
		status, answer := f.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/restores", token, all)
		message, _ := answer["message"].(string)
		return status, message
	}
	expect := func(name string, status int, message string, gotStatus int, got string) {
		t.Helper()
		if gotStatus != status || got != message {
			t.Errorf("%s: %d %q, not %d %q", name, gotStatus, got, status, message)
		}
	}
	f.agent(f.host)
	f.agent(other)

	s, m := create(operator, job, map[string]any{"mode": "path", "host": other.Id, "path": "/srv/restored"})
	expect("an operator's folder", http.StatusForbidden, "Only admins restore to a new folder.", s, m)
	s, m = create(operator, job, map[string]any{"mode": "volume"})
	expect("a mode", http.StatusBadRequest, "Choose where the snapshot goes: its original location or a new folder.", s, m)
	s, m = create(operator, job, map[string]any{"snapshot": "3f9a1c2e"})
	expect("a short id", http.StatusBadRequest, "Choose a snapshot.", s, m)
	s, m = create(operator, job, map[string]any{"target": "nosuchtarget000"})
	expect("no target", http.StatusNotFound, "The target does not exist.", s, m)
	nas := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	s, m = create(operator, job, map[string]any{"target": nas.Id})
	expect("another target", http.StatusBadRequest, "The job never backed up to that target.", s, m)
	f.target.Set("status", schema.TargetPending)
	if err := f.app.Save(f.target); err != nil {
		t.Fatal(err)
	}
	s, m = create(operator, job, nil)
	expect("a target being added", http.StatusConflict, "The target is still being added: restore from it once its check ends.", s, m)
	f.target.Set("status", schema.TargetOK)
	if err := f.app.Save(f.target); err != nil {
		t.Fatal(err)
	}
	many := make([]string, 101)
	for i := range many {
		many[i] = fmt.Sprintf("/volumes/app_data/%d", i)
	}
	s, m = create(operator, job, map[string]any{"include": many})
	expect("too many", http.StatusBadRequest, "Pick at most 100 items, or a whole folder.", s, m)
	s, m = create(operator, job, map[string]any{"include": []string{"volumes/app_data/x"}})
	expect("a relative item", http.StatusBadRequest, "Pick each item by its absolute path in the snapshot.", s, m)
	s, m = create(operator, job, map[string]any{"include": []string{"/volumes/app_data/x"}, "replace": true})
	expect("replace a selection", http.StatusBadRequest, "Replace works for the whole snapshot only.", s, m)
	s, m = create(operator, job, map[string]any{"include": []string{"/volumes/other/x"}})
	expect("an item of other data", http.StatusBadRequest, "Pick only items of what the job backs up.", s, m)
	s, m = create(operator, job, map[string]any{"host": other.Id})
	expect("the original location elsewhere", http.StatusBadRequest, "A restore to the original location goes to the job's server.", s, m)

	stack := f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "shop"})
	dump := f.job("dump", map[string]any{"source_kind": schema.SourceDump, "stack": stack.Id, "service": "db", "engine": schema.DumpPostgres})
	s, m = create(operator, dump, nil)
	expect("a dump", http.StatusBadRequest, "A database dump goes back into its database with the database's own restore: restore it to a folder, or download it.", s, m)
	system := f.job("system", map[string]any{"source_kind": schema.SourcePath, "source": "/etc/nginx"})
	s, m = create(operator, system, nil)
	expect("a system directory", http.StatusBadRequest, "/etc is the system's: restore elsewhere, such as /srv/restored.", s, m)

	disk := f.addTarget("disk", backup.KindLocal, f.host, backup.Config{Path: "/srv/restic"}, backup.Credentials{})
	away := f.job("away", map[string]any{"host": other.Id, "source_kind": schema.SourceVolume, "source": "x", "targets": []string{disk.Id}})
	s, m = create(operator, away, map[string]any{"target": disk.Id})
	expect("a local target's to another server", http.StatusBadRequest, "The target disk is a folder of node-1: only that server restores from it.", s, m)

	// to a new folder
	s, m = create(f.admin, job, map[string]any{"mode": "path", "host": "nosuchhost00000", "path": "/srv/restored"})
	expect("no server", http.StatusBadRequest, "Choose the server the snapshot is restored on.", s, m)
	for dir, want := range map[string]string{
		"/":                         "Restore into a folder of its own, not into /.",
		"/var":                      "/var holds /var/run, which is the system's: restore elsewhere, such as /srv/restored.",
		"/var/lib/docker/volumes/x": "/var/lib/docker is the system's: restore elsewhere, such as /srv/restored.",
		"/srv/../etc":               "Give the folder by its absolute path, such as /srv/restored.",
		"/srv/restic/x":             "/srv/restic is the repository of the target disk: restore elsewhere.",
		"/srv":                      "/srv/restic is the repository of the target disk: restore elsewhere.",
	} {
		s, m = create(f.admin, job, map[string]any{"mode": "path", "host": f.host.Id, "path": dir})
		expect(dir, http.StatusBadRequest, want, s, m)
	}
	f.save(schema.BackupRuns, map[string]any{"job": job.Id, "target": disk.Id, "host": f.host.Id, "trigger": schema.RunManual, "status": schema.RunSucceeded})
	s, m = create(f.admin, job, map[string]any{"mode": "path", "host": other.Id, "path": "/srv/restored", "target": disk.Id})
	expect("a local target's to a folder elsewhere", http.StatusBadRequest, "The target disk is a folder of node-1: only that server restores from it.", s, m)
	s, m = create(f.admin, job, map[string]any{"mode": "path", "host": other.Id, "path": "/srv/restored", "source": "/volumes/app_data", "include": []string{"/volumes/other/x"}})
	expect("an item outside the folder", http.StatusBadRequest, "Pick only items in the folder of the snapshot that is restored.", s, m)
	s, m = create(f.admin, job, map[string]any{"mode": "path", "host": other.Id, "path": "/srv/restored", "source": "volumes"})
	expect("a relative folder of the snapshot", http.StatusBadRequest, "Give the folder of the snapshot by its absolute path.", s, m)

	// the source is the snapshot's one path, or all of it
	status, body := f.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/restores", f.admin,
		map[string]any{"target": f.target.Id, "snapshot": snapshotID, "mode": "path", "host": other.Id, "path": "/srv/restored"})
	if status != http.StatusAccepted || body["source"] != "/" || body["host"] != other.Id || body["path"] != "/srv/restored" {
		t.Errorf("a folder: %d %v", status, body)
	}

	// offline
	lonely := testutil.NewHost(t, f.app, "node-3")
	s, m = create(f.admin, job, map[string]any{"mode": "path", "host": lonely.Id, "path": "/srv/restored"})
	expect("offline", http.StatusConflict, "The server node-3 is offline: its agent restores the snapshot.", s, m)
	// a backup of the job is running
	running := f.save(schema.BackupRuns, map[string]any{"job": job.Id, "target": f.target.Id, "host": f.host.Id, "trigger": schema.RunManual, "status": schema.RunRunning})
	runCommand := f.command(f.host.Id, &agentv1.Command{Payload: &agentv1.Command_BackupRun{BackupRun: &agentv1.BackupRun{
		RunId: running.Id, Repository: &agentv1.BackupRepository{TargetId: f.target.Id}}}})
	running.Set("command", runCommand.Id)
	if err := f.app.Save(running); err != nil {
		t.Fatal(err)
	}
	s, m = create(operator, job, nil)
	expect("a backup running", http.StatusConflict, "A backup of data is running: restore when it ends.", s, m)
	if err := f.app.Delete(running); err != nil {
		t.Fatal(err)
	}
	// another restore into the same data
	f.restore(job, map[string]any{"status": schema.RestoreRunning, "requested_label": "superuser root@example.com"})
	s, m = create(operator, job, nil)
	expect("another restore", http.StatusConflict, "A restore into app_data is already under way.", s, m)
	f.restore(job, map[string]any{"status": schema.RestoreQueued, "mode": schema.RestorePath, "host": other.Id, "roots": nil, "path": "/srv/restored/app"})
	s, m = create(f.admin, job, map[string]any{"mode": "path", "host": other.Id, "path": "/srv/restored"})
	expect("another restore into a folder under it", http.StatusConflict, "A restore into /srv/restored is already under way.", s, m)
	f.restore(job, map[string]any{"status": schema.RestoreQueued, "mode": schema.RestorePath, "host": other.Id, "roots": nil, "path": "/srv/app"})
	s, m = create(f.admin, job, map[string]any{"mode": "path", "host": other.Id, "path": "/srv/app/data"})
	expect("another restore into a folder above it", http.StatusConflict, "A restore into /srv/app/data is already under way.", s, m)
	s, m = create(f.admin, job, map[string]any{"mode": "path", "host": other.Id, "path": "/srv/app2"})
	expect("a folder that only starts the same", http.StatusAccepted, "", s, m)
}

// TestARestoreIsCheckedInTheDocumentedOrder (#30): a restore that fails two
// checks gets the answer of the one docs/architecture.md lists first: the
// mode and the role, the snapshot, the target, the job's use of it, a
// pending target, the includes, replace, then what the mode needs.
func TestARestoreIsCheckedInTheDocumentedOrder(t *testing.T) {
	f := newJobFixture(t)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	stack := f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "shop"})
	dump := f.job("dump", map[string]any{"source_kind": schema.SourceDump, "stack": stack.Id, "service": "db", "engine": schema.DumpPostgres})
	unused := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	f.agent(f.host)
	many := make([]string, 101)
	for i := range many {
		many[i] = fmt.Sprintf("/volumes/app_data/%d", i)
	}
	setStatus := func(status string, targets ...*core.Record) {
		t.Helper()
		for _, target := range targets {
			target.Set("status", status)
			if err := f.app.Save(target); err != nil {
				t.Fatal(err)
			}
		}
	}
	type check struct {
		name    string
		job     *core.Record
		body    map[string]any
		status  int
		message string
	}
	expect := func(checks ...check) {
		t.Helper()
		for _, c := range checks {
			all := map[string]any{"target": f.target.Id, "snapshot": snapshotID, "mode": "original"}
			for key, value := range c.body {
				all[key] = value
			}
			status, answer := f.send(http.MethodPost, "/api/pc/backup-jobs/"+c.job.Id+"/restores", operator, all)
			if message, _ := answer["message"].(string); status != c.status || message != c.message {
				t.Errorf("%s: %d %q, not %d %q", c.name, status, message, c.status, c.message)
			}
		}
	}

	setStatus(schema.TargetPending, unused, f.target)
	expect(
		check{"an operator's folder of a short id", job, map[string]any{"mode": "path", "snapshot": "3f9a1c2e", "target": "nosuchtarget000"},
			http.StatusForbidden, "Only admins restore to a new folder."},
		check{"a mode of a short id", job, map[string]any{"mode": "volume", "snapshot": "3f9a1c2e", "target": "nosuchtarget000"},
			http.StatusBadRequest, "Choose where the snapshot goes: its original location or a new folder."},
		check{"a short id from no target", job, map[string]any{"snapshot": "3f9a1c2e", "target": "nosuchtarget000"},
			http.StatusBadRequest, "Choose a snapshot."},
		check{"no target, and too many items", job, map[string]any{"target": "nosuchtarget000", "include": many},
			http.StatusNotFound, "The target does not exist."},
		check{"a pending target the job never used", job, map[string]any{"target": unused.Id},
			http.StatusBadRequest, "The job never backed up to that target."},
		check{"a pending target, and too many items", job, map[string]any{"include": many},
			http.StatusConflict, "The target is still being added: restore from it once its check ends."},
	)
	setStatus(schema.TargetOK, f.target)
	expect(
		check{"too many items to replace", job, map[string]any{"include": many, "replace": true},
			http.StatusBadRequest, "Pick at most 100 items, or a whole folder."},
		check{"a dump's items to replace", dump, map[string]any{"include": []string{"/dump/db.sql"}, "replace": true},
			http.StatusBadRequest, "Replace works for the whole snapshot only."},
		check{"another server, and items of other data", job, map[string]any{"host": "nosuchhost00000", "include": []string{"/volumes/other/x"}},
			http.StatusBadRequest, "A restore to the original location goes to the job's server."},
	)
}

// TestADraftIsDiscarded (#30): a draft goes when someone discards it, its
// preview stopped if it runs; one that was started does not, and only an
// admin discards a draft to a folder.
func TestADraftIsDiscarded(t *testing.T) {
	f := newJobFixture(t)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	stream := f.agent(f.host)

	status, body := f.send(http.MethodPost, "/api/pc/backup-jobs/"+job.Id+"/restores", operator,
		map[string]any{"target": f.target.Id, "snapshot": snapshotID, "mode": "original"})
	if status != http.StatusAccepted {
		t.Fatalf("created: %d %v", status, body)
	}
	preview := f.received(stream)
	if status, _ := f.send(http.MethodDelete, "/api/pc/backup-restores/"+body["id"].(string), operator, nil); status != http.StatusNoContent {
		t.Errorf("discarded: %d", status)
	}
	select {
	case message := <-stream.Out():
		if message.GetCancel().GetCommandId() != preview.GetId() {
			t.Errorf("the agent got %v", message)
		}
	case <-time.After(5 * time.Second):
		t.Error("the preview was not stopped")
	}
	f.answer(f.host, preview.GetId(), agentv1.CommandState_COMMAND_STATE_CANCELLED, nil, "cancelled")
	f.eventually("the preview's command going", func() bool { return f.gone(preview.GetId()) })

	for _, status := range []string{schema.RestorePreviewed, schema.RestoreExpired} {
		draft := f.restore(job, map[string]any{"status": status})
		if got, _ := f.send(http.MethodDelete, "/api/pc/backup-restores/"+draft.Id, operator, nil); got != http.StatusNoContent {
			t.Errorf("a draft %s: %d", status, got)
		}
	}
	folder := f.restore(job, map[string]any{"status": schema.RestorePreviewed, "mode": schema.RestorePath, "roots": nil, "path": "/srv/x"})
	if got, _ := f.send(http.MethodDelete, "/api/pc/backup-restores/"+folder.Id, operator, nil); got != http.StatusForbidden {
		t.Errorf("an operator discarded a folder's draft: %d", got)
	}
	started := f.restore(job, map[string]any{"status": schema.RestoreQueued})
	if got, answer := f.send(http.MethodDelete, "/api/pc/backup-restores/"+started.Id, f.admin, nil); got != http.StatusConflict ||
		answer["message"] != "Only a draft is discarded: this restore was started." {
		t.Errorf("a restore that was started: %d %v", got, answer)
	}
}
