package panel

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/audit"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

const holdMessage = "The stack app is being restored from a backup: this waits until that is done."

// TestARestoreHoldsItsStack (#30, SAFE): while a restore to the original
// location of a stack is queued or running, the stack is not changed,
// deployed or removed, the API's commands of the stack and starts of its
// containers are refused, as is the start of any container a restore stops;
// once it ends, they go again.
func TestARestoreHoldsItsStack(t *testing.T) {
	f := newJobFixture(t)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	_, superuser := testutil.NewSuperuser(t, f.app)
	stack := f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "app", "compose_yaml": "services:\n  web:\n    image: nginx\n"})
	other := f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "other"})
	f.save(schema.Containers, map[string]any{"host": f.host.Id, "stack": stack.Id, "project": "app", "service": "web", "docker_id": "c0ffee", "name": "app-web-1", "state": "exited"})
	f.save(schema.Containers, map[string]any{"host": f.host.Id, "stack": other.Id, "project": "other", "service": "db", "docker_id": "beef", "name": "other-db-1", "state": "exited"})
	f.save(schema.Containers, map[string]any{"host": f.host.Id, "project": "", "docker_id": "free", "name": "free-1", "state": "exited"})
	job := f.job("data", map[string]any{"source_kind": schema.SourceStack, "stack": stack.Id, "source": "app_data"})
	restore := f.restore(job, map[string]any{"status": schema.RestoreQueued, "stack_name": "app"})
	// a restore to a folder that a container of another stack uses
	f.restore(job, map[string]any{"status": schema.RestoreRunning, "mode": schema.RestorePath, "roots": nil, "path": "/srv/other",
		"preview": map[string]any{"would_stop": []any{map[string]any{"name": "other-db-1", "project": "other"}}}})

	command := func(commandType string, payload map[string]any) (int, string) {
		t.Helper()
		status, body := f.send(http.MethodPost, "/api/collections/commands/records", operator,
			map[string]any{"type": commandType, "host": f.host.Id, "payload": payload})
		message, _ := body["message"].(string)
		return status, message
	}
	for name, test := range map[string]struct {
		commandType string
		payload     map[string]any
		message     string
	}{
		"a start":           {"container.start", map[string]any{"docker_id": "c0ffee"}, holdMessage},
		"a restart":         {"container.restart", map[string]any{"docker_id": "c0ffee"}, holdMessage},
		"a removal":         {"container.remove", map[string]any{"docker_id": "c0ffee"}, holdMessage},
		"a stack's restart": {"stack.restart", map[string]any{"name": "app"}, holdMessage},
		"a stack's stop":    {"stack.stop", map[string]any{"name": "app"}, holdMessage},
		"a stopped one":     {"container.start", map[string]any{"docker_id": "beef"}, "A restore is writing this container's data: it starts again when the restore ends."},
		"another's restart": {"container.restart", map[string]any{"docker_id": "beef"}, "A restore is writing this container's data: it starts again when the restore ends."},
	} {
		if status, message := command(test.commandType, test.payload); status != http.StatusConflict || message != test.message {
			t.Errorf("%s: %d %q", name, status, message)
		}
	}
	if status, message := command("container.start", map[string]any{"docker_id": "free"}); status != http.StatusOK {
		t.Errorf("a container no restore holds: %d %q", status, message)
	}
	if status, message := command("stack.restart", map[string]any{"name": "other"}); status != http.StatusOK {
		t.Errorf("another stack: %d %q", status, message)
	}

	// changes, deploys and the stack's removal
	if status, body := f.send(http.MethodPatch, "/api/collections/stacks/records/"+stack.Id, superuser, map[string]any{"note": "x"}); status != http.StatusConflict ||
		body["message"] != holdMessage {
		t.Errorf("a change: %d %v", status, body)
	}
	if status, body := f.send(http.MethodPost, "/api/pc/stacks/"+stack.Id+"/deploy", operator, map[string]any{}); status < 400 || body["message"] != holdMessage {
		t.Errorf("a deploy: %d %v", status, body)
	}
	if _, err := f.p.deployRevision(context.Background(), stack, core.NewRecord(core.NewBaseCollection(schema.StackRevisions)), false, "", "", ""); err == nil || err.Error() != holdMessage {
		t.Errorf("a deploy queued by the panel: %v", err)
	}
	if err := f.p.saveStackEdit(context.Background(), nil, stack, stackEdit{}, audit.Actor{Label: "test"}); err == nil {
		t.Error("an edit went through")
	}
	if status, body := f.send(http.MethodDelete, "/api/collections/stacks/records/"+stack.Id, superuser, nil); status != http.StatusConflict ||
		body["message"] != holdMessage {
		t.Errorf("a removal: %d %v", status, body)
	}
	// a database of the stack, or connected to it
	shared := f.save(schema.DBServers, map[string]any{"host": f.host.Id, "name": "pool", "engine": schema.EnginePostgres, "kind": schema.DBServerShared})
	owned := f.save(schema.Databases, map[string]any{"server": shared.Id, "name": "owned", "owner_kind": "stack", "stack": stack.Id})
	loose := f.save(schema.Databases, map[string]any{"server": shared.Id, "name": "loose"})
	if status, body := f.send(http.MethodPost, "/api/pc/databases/"+owned.Id+"/connect", operator, map[string]any{"stack": other.Id}); status != http.StatusConflict ||
		body["message"] != holdMessage {
		t.Errorf("a change of the stack's database: %d %v", status, body)
	}
	if status, body := f.send(http.MethodPost, "/api/pc/databases/"+loose.Id+"/connect", operator, map[string]any{"stack": stack.Id}); status != http.StatusConflict ||
		body["message"] != holdMessage {
		t.Errorf("a database connected to the stack: %d %v", status, body)
	}
	if reason := f.p.claimedByMove(f.save(schema.DBServers, map[string]any{"host": f.host.Id, "name": "shared", "engine": schema.EnginePostgres, "kind": schema.DBServerShared}), "notes", stack.Id); reason == "" {
		t.Error("a database was added to a stack being restored")
	}

	// once the restore ended
	restore.Set("status", schema.RestoreSucceeded)
	if err := f.app.Save(restore); err != nil {
		t.Fatal(err)
	}
	if status, message := command("container.start", map[string]any{"docker_id": "c0ffee"}); status != http.StatusOK {
		t.Errorf("after the restore: %d %q", status, message)
	}
	if f.p.restoreHolding(stack) != nil {
		t.Error("a restore that ended holds its stack")
	}
}

// TestAScheduledBackupOfDataBeingRestoredIsSkipped (#30, SAFE): a scheduled
// backup of data a restore writes is skipped, without an alert, and one by
// hand is refused: the job's own restore, another into its volume, its
// stack's, or a folder that is, holds or lies in its directory. A folder that
// only starts the same does not count.
func TestAScheduledBackupOfDataBeingRestoredIsSkipped(t *testing.T) {
	f := newJobFixture(t)
	f.agent(f.host)
	minute := time.Now().UTC().Truncate(time.Minute)
	stack := f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "shop"})
	volume := f.job("volume", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	dir := f.job("dir", map[string]any{"source_kind": schema.SourcePath, "source": "/srv/app"})
	shop := f.job("shop", map[string]any{"source_kind": schema.SourceStack, "stack": stack.Id, "source": "shop_db"})
	dump := f.job("dump", map[string]any{"source_kind": schema.SourceDump, "stack": stack.Id, "service": "db", "engine": schema.DumpPostgres})
	free := f.job("free", map[string]any{"source_kind": schema.SourcePath, "source": "/srv/app2"})

	other := f.job("other", map[string]any{"source_kind": schema.SourceVolume, "source": "elsewhere"})
	f.restore(other, map[string]any{"status": schema.RestoreRunning, "roots": []restoreRoot{{Source: "/volumes/app_data", Volume: "app_data"}}})
	f.restore(other, map[string]any{"status": schema.RestoreQueued, "mode": schema.RestorePath, "roots": nil, "path": "/srv/app/data"})
	f.restore(other, map[string]any{"status": schema.RestoreRunning, "stack_name": "shop", "roots": []restoreRoot{{Source: "/volumes/shop_x", Volume: "shop_x"}}})

	for _, job := range []*core.Record{volume, dir, shop, dump, free} {
		f.p.runOnSchedule(job, minute)
	}
	skipped := func(job *core.Record) bool {
		t.Helper()
		runs, err := f.app.FindRecordsByFilter(schema.BackupRuns, "job={:job}", "", 0, 0, map[string]any{"job": job.Id})
		if err != nil || len(runs) != 1 {
			t.Fatalf("%s: %d runs, %v", job.GetString("name"), len(runs), err)
		}
		return runs[0].GetString("status") == schema.RunSkipped && runs[0].GetString("error") == "A restore into its data is under way on node-1."
	}
	for _, job := range []*core.Record{volume, dir, shop, dump} {
		if !skipped(job) {
			t.Errorf("%s was not skipped", job.GetString("name"))
		}
	}
	if skipped(free) {
		t.Error("a folder that only starts the same was skipped")
	}
	if alerts, _ := f.app.FindRecordsByFilter(schema.Alerts, "rule='backup_skipped'", "", 0, 0); len(alerts) != 0 {
		t.Errorf("%d alerts", len(alerts))
	}

	// its own restore, and by hand
	own := f.job("own", map[string]any{"source_kind": schema.SourceVolume, "source": "own_data"})
	// its data as it was when the restore started, before the job changed
	f.restore(own, map[string]any{"status": schema.RestoreQueued, "roots": []restoreRoot{{Source: "/volumes/before_data", Volume: "before_data"}}})
	if status, body := f.send(http.MethodPost, "/api/pc/backup-jobs/"+own.Id+"/run", f.admin, nil); status != http.StatusConflict ||
		body["message"] != "A restore into its data is under way: back it up once it ends." {
		t.Errorf("by hand: %d %v", status, body)
	}
}

// TestRemovalWaitsForARestore (#30, SAFE): a job, a target or a server is
// not removed while a restore of theirs, or a download, is open.
func TestRemovalWaitsForARestore(t *testing.T) {
	f := newJobFixture(t)
	_, superuser := testutil.NewSuperuser(t, f.app)
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data", "targets": []string{}})
	restore := f.restore(job, map[string]any{"status": schema.RestoreRunning})
	if status, body := f.send(http.MethodDelete, "/api/pc/backup-jobs/"+job.Id, f.admin, nil); status != http.StatusConflict || body["message"] != restoreUnderWayMessage {
		t.Errorf("the job: %d %v", status, body)
	}
	if status, body := f.send(http.MethodDelete, "/api/pc/backup-targets/"+f.target.Id, f.admin, nil); status != http.StatusConflict || body["message"] != restoreUnderWayMessage {
		t.Errorf("the target: %d %v", status, body)
	}
	if status, body := f.send(http.MethodDelete, "/api/collections/hosts/records/"+f.host.Id, superuser, nil); status != http.StatusConflict || body["message"] != restoreUnderWayMessage {
		t.Errorf("the server: %d %v", status, body)
	}
	// a download, on another server that reads the target
	other := testutil.NewHost(t, f.app, "node-2")
	download := f.restore(job, map[string]any{"status": schema.RestoreQueued, "mode": schema.RestoreDownload, "host": other.Id, "roots": nil, "path": "/"})
	restore.Set("status", schema.RestoreSucceeded)
	if err := f.app.Save(restore); err != nil {
		t.Fatal(err)
	}
	if status, body := f.send(http.MethodDelete, "/api/collections/hosts/records/"+other.Id, superuser, nil); status != http.StatusConflict || body["message"] != restoreUnderWayMessage {
		t.Errorf("a download's server: %d %v", status, body)
	}
	download.Set("status", schema.RestoreFailed)
	if err := f.app.Save(download); err != nil {
		t.Fatal(err)
	}
	if status, body := f.send(http.MethodDelete, "/api/pc/backup-jobs/"+job.Id, f.admin, nil); status != http.StatusNoContent {
		t.Errorf("the job once its restores ended: %d %v", status, body)
	}
}
