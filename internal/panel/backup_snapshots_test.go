package panel

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"google.golang.org/protobuf/types/known/structpb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// agent connects the agent of a server that lists, restores and sends
// downloads.
func (f *jobFixture) agent(host *core.Record) *commands.Stream {
	f.t.Helper()
	stream := f.p.Commands.Connect(host.Id, []string{"backup.snapshots", "backup.browse", "backup.restore", "backup.dump", "backup.run"}, nil)
	f.t.Cleanup(func() { f.p.Commands.Disconnect(stream) })
	return stream
}

// received is the next command an agent got.
func (f *jobFixture) received(stream *commands.Stream) *agentv1.Command {
	f.t.Helper()
	select {
	case message := <-stream.Out():
		if message.GetCommand() == nil {
			f.t.Fatalf("the agent got %v", message)
		}
		return message.GetCommand()
	case <-time.After(5 * time.Second):
		f.t.Fatal("the agent got no command")
		return nil
	}
}

// quiet says whether an agent gets nothing for a moment.
func (f *jobFixture) quiet(stream *commands.Stream) bool {
	select {
	case message := <-stream.Out():
		f.t.Logf("the agent got %v", message)
		return false
	case <-time.After(200 * time.Millisecond):
		return true
	}
}

// answer reports the end of a command, as its agent would.
func (f *jobFixture) answer(host *core.Record, id string, state agentv1.CommandState, result map[string]any, message string) {
	f.t.Helper()
	f.report(host, id, 100, state, nil, result, message)
}

// report sends an update of a command, as its agent would.
func (f *jobFixture) report(host *core.Record, id string, sequence uint64, state agentv1.CommandState, progress *agentv1.CommandProgress, result map[string]any, message string) bool {
	f.t.Helper()
	update := &agentv1.ReportCommandUpdateRequest{CommandId: id, Sequence: sequence, State: state, Progress: progress, Error: message}
	if result != nil {
		structured, err := structpb.NewStruct(result)
		if err != nil {
			f.t.Fatal(err)
		}
		update.Result = structured
	}
	cancel, err := f.p.Commands.Update(host.Id, update)
	if err != nil {
		f.t.Fatal(err)
	}
	return cancel
}

// reply is the answer of a request sent with sendLater.
type reply struct {
	status int
	body   map[string]any
}

// sendLater sends a request while the test answers for the agent.
func (f *jobFixture) sendLater(method, url, token string, body any) <-chan reply {
	answer := make(chan reply, 1)
	go func() {
		status, out := f.send(method, url, token, body)
		answer <- reply{status, out}
	}()
	return answer
}

func (f *jobFixture) wait(answer <-chan reply) reply {
	f.t.Helper()
	select {
	case got := <-answer:
		return got
	case <-time.After(10 * time.Second):
		f.t.Fatal("the route did not answer")
		return reply{}
	}
}

// eventually waits for a condition, as for what the panel does later.
func (f *jobFixture) eventually(what string, done func() bool) {
	f.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if done() {
			return
		}
	}
	f.t.Errorf("%s did not happen", what)
}

func (f *jobFixture) gone(id string) bool {
	_, err := f.app.FindRecordById(schema.Commands, id)
	return err != nil
}

func snapshotsURL(job, target *core.Record, query string) string {
	return "/api/pc/backup-jobs/" + job.Id + "/snapshots?target=" + target.Id + query
}

// TestSnapshotsAreListedPerJobAndTargetAndCached (#30, S): a job's
// snapshots in a target are listed by the agent of the target's server with
// the job's tag, joined with the runs that made them, whose sizes stand in;
// they are kept for the next request, read again on refresh or once a
// backup ends, and the listing's command does not stay.
func TestSnapshotsAreListedPerJobAndTargetAndCached(t *testing.T) {
	f := newJobFixture(t)
	_, viewer := testutil.NewUser(t, f.app, "viewer@example.com", schema.RoleViewer)
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	run := f.save(schema.BackupRuns, map[string]any{"job": job.Id, "target": f.target.Id, "host": f.host.Id, "trigger": schema.RunSchedule,
		"status": schema.RunSucceeded, "snapshot_id": snapshotID, "total_bytes": 4096, "bytes_added": 512})
	stream := f.agent(f.host)
	older := strings.Repeat("0a", 32)
	result := map[string]any{"total": 2, "snapshots": []any{
		map[string]any{"id": snapshotID, "short_id": "3f9a1c2e", "time": "2026-11-10T12:00:00Z", "hostname": "node-1",
			"paths": []any{"/volumes/app_data"}, "size_bytes": 0, "added_bytes": 0, "files": 12},
		map[string]any{"id": older, "short_id": "0a0a0a0a", "time": "2026-11-09T12:00:00Z", "hostname": "node-1",
			"paths": []any{"/volumes/app_data"}, "size_bytes": 100, "added_bytes": 10, "files": 11},
	}}

	pending := f.sendLater(http.MethodGet, snapshotsURL(job, f.target, ""), viewer, nil)
	command := f.received(stream)
	listing := command.GetBackupSnapshots()
	if strings.Join(listing.GetTags(), ",") != "pocketcloud,job:"+job.Id || listing.GetLimit() != 500 ||
		listing.GetRepository().GetPassword() != "repo-password-b2" {
		t.Errorf("the listing: %v", command)
	}
	f.answer(f.host, command.GetId(), agentv1.CommandState_COMMAND_STATE_SUCCEEDED, result, "")
	got := f.wait(pending)
	snapshots, _ := got.body["snapshots"].([]any)
	if got.status != http.StatusOK || got.body["cached"] != false || got.body["total"] != float64(2) || got.body["truncated"] != false ||
		got.body["target"] != f.target.Id || got.body["host"] != f.host.Id || len(snapshots) != 2 {
		t.Fatalf("listed: %d %v", got.status, got.body)
	}
	newest, _ := snapshots[0].(map[string]any)
	made, _ := newest["run"].(map[string]any)
	if newest["id"] != snapshotID || newest["size_bytes"] != float64(4096) || newest["added_bytes"] != float64(512) ||
		made["id"] != run.Id || made["trigger"] != schema.RunSchedule {
		t.Errorf("the newest: %v", newest)
	}
	if oldest, _ := snapshots[1].(map[string]any); oldest["run"] != nil || oldest["size_bytes"] != float64(100) {
		t.Errorf("one no run made: %v", oldest)
	}
	f.eventually("the listing's command going", func() bool { return f.gone(command.GetId()) })
	// which a restore of it reads
	if kept, ok := f.p.cachedSnapshot(job.Id, f.target.Id, snapshotID); !ok || kept.ShortID != "3f9a1c2e" ||
		strings.Join(kept.Paths, ",") != "/volumes/app_data" || kept.Time != "2026-11-10T12:00:00Z" {
		t.Errorf("the snapshot kept: %v %v", kept, ok)
	}

	// kept
	if status, body := f.send(http.MethodGet, snapshotsURL(job, f.target, ""), viewer, nil); status != http.StatusOK || body["cached"] != true {
		t.Errorf("again: %d %v", status, body)
	}
	if !f.quiet(stream) {
		t.Error("listed again within ten minutes")
	}
	// read again on refresh
	pending = f.sendLater(http.MethodGet, snapshotsURL(job, f.target, "&refresh=1"), viewer, nil)
	command = f.received(stream)
	f.answer(f.host, command.GetId(), agentv1.CommandState_COMMAND_STATE_SUCCEEDED, result, "")
	if got := f.wait(pending); got.status != http.StatusOK || got.body["cached"] != false {
		t.Errorf("refreshed: %d %v", got.status, got.body)
	}
	// and once a backup of the job to the target ended
	backupRun := f.command(f.host.Id, &agentv1.Command{Payload: &agentv1.Command_BackupRun{BackupRun: &agentv1.BackupRun{
		RunId: run.Id, Repository: &agentv1.BackupRepository{TargetId: f.target.Id}, Tags: []string{"pocketcloud", "job:" + job.Id},
	}}})
	f.received(stream)
	f.answer(f.host, backupRun.Id, agentv1.CommandState_COMMAND_STATE_SUCCEEDED, map[string]any{"snapshot_id": strings.Repeat("ff", 32)}, "")
	pending = f.sendLater(http.MethodGet, snapshotsURL(job, f.target, ""), viewer, nil)
	command = f.received(stream)
	f.answer(f.host, command.GetId(), agentv1.CommandState_COMMAND_STATE_SUCCEEDED, result, "")
	if got := f.wait(pending); got.status != http.StatusOK || got.body["cached"] != false {
		t.Errorf("after a backup: %d %v", got.status, got.body)
	}

	// another target of the job is another listing
	nas := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	f.save(schema.BackupRuns, map[string]any{"job": job.Id, "target": nas.Id, "host": f.host.Id, "trigger": schema.RunManual, "status": schema.RunFailed})
	pending = f.sendLater(http.MethodGet, snapshotsURL(job, nas, ""), viewer, nil)
	command = f.received(stream)
	if command.GetBackupSnapshots().GetRepository().GetTargetId() != nas.Id {
		t.Errorf("the other target's listing: %v", command)
	}
	f.answer(f.host, command.GetId(), agentv1.CommandState_COMMAND_STATE_SUCCEEDED, map[string]any{"snapshots": []any{}, "total": 0}, "")
	if got := f.wait(pending); got.status != http.StatusOK || got.body["total"] != float64(0) {
		t.Errorf("a target the job once backed up to: %d %v", got.status, got.body)
	}
}

// TestABackupThatEndsDropsTheListingUnderWay (#30): a listing that started
// before a backup of the job to the target ended may have read the
// repository before the backup saved its snapshot. Its requests get its
// answer, but it is not kept: a refresh asked for after the backup starts
// another listing rather than join it, and the next request is answered by
// that one, with the new snapshot.
func TestABackupThatEndsDropsTheListingUnderWay(t *testing.T) {
	f := newJobFixture(t)
	f.p.ListingWait = 5 * time.Second
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	run := f.save(schema.BackupRuns, map[string]any{"job": job.Id, "target": f.target.Id, "host": f.host.Id, "trigger": schema.RunSchedule, "status": schema.RunRunning})
	stream := f.agent(f.host)
	snapshot := func(id, short, at string) map[string]any {
		return map[string]any{"id": id, "short_id": short, "time": at, "hostname": "node-1", "paths": []any{"/volumes/app_data"}, "files": 1}
	}
	older := snapshot(strings.Repeat("0a", 32), "0a0a0a0a", "2026-11-09T12:00:00Z")
	newest := snapshot(snapshotID, "3f9a1c2e", "2026-11-10T12:00:00Z")
	before := map[string]any{"total": 1, "snapshots": []any{older}}
	after := map[string]any{"total": 2, "snapshots": []any{newest, older}}

	backupRun := f.command(f.host.Id, &agentv1.Command{Payload: &agentv1.Command_BackupRun{BackupRun: &agentv1.BackupRun{
		RunId: run.Id, Repository: &agentv1.BackupRepository{TargetId: f.target.Id}, Tags: []string{"pocketcloud", "job:" + job.Id},
	}}})
	f.received(stream)
	first := f.sendLater(http.MethodGet, snapshotsURL(job, f.target, ""), f.admin, nil)
	stale := f.received(stream)
	if stale.GetBackupSnapshots() == nil {
		t.Fatalf("the listing: %v", stale)
	}
	// the backup saves its snapshot and ends while the listing reads
	f.answer(f.host, backupRun.Id, agentv1.CommandState_COMMAND_STATE_SUCCEEDED, map[string]any{"snapshot_id": snapshotID}, "")
	refreshed := f.sendLater(http.MethodGet, snapshotsURL(job, f.target, "&refresh=1"), f.admin, nil)
	fresh := f.received(stream)
	if fresh.GetBackupSnapshots() == nil || fresh.GetId() == stale.GetId() {
		t.Fatalf("the listing after the backup: %v", fresh)
	}

	f.answer(f.host, stale.GetId(), agentv1.CommandState_COMMAND_STATE_SUCCEEDED, before, "")
	if got := f.wait(first); got.status != http.StatusOK || got.body["total"] != float64(1) {
		t.Errorf("the request of the listing that started before: %d %v", got.status, got.body)
	}
	if _, ok := f.p.cachedSnapshot(job.Id, f.target.Id, strings.Repeat("0a", 32)); ok {
		t.Error("the listing that started before the backup ended was kept")
	}
	later := f.sendLater(http.MethodGet, snapshotsURL(job, f.target, ""), f.admin, nil)
	if !f.quiet(stream) {
		t.Error("a third listing went out")
	}
	f.answer(f.host, fresh.GetId(), agentv1.CommandState_COMMAND_STATE_SUCCEEDED, after, "")
	for name, pending := range map[string]<-chan reply{"the refresh": refreshed, "a request meanwhile": later} {
		if got := f.wait(pending); got.status != http.StatusOK || got.body["total"] != float64(2) {
			t.Errorf("%s: %d %v", name, got.status, got.body)
		}
	}
	if status, body := f.send(http.MethodGet, snapshotsURL(job, f.target, ""), f.admin, nil); status != http.StatusOK || body["cached"] != true || body["total"] != float64(2) {
		t.Errorf("kept: %d %v", status, body)
	}
	if kept, ok := f.p.cachedSnapshot(job.Id, f.target.Id, snapshotID); !ok || kept.Time != "2026-11-10T12:00:00Z" {
		t.Errorf("the new snapshot kept: %v %v", kept, ok)
	}
}

// TestSnapshotsJoinTheRequestInFlightAndAnswerPending (#30, S): a request
// that waited long enough answers that the listing is pending; the next
// joins the same listing rather than queueing another; a failure, with the
// target's secrets hidden, is told to the requests that come soon after,
// and an agent that does not answer in time is said to.
func TestSnapshotsJoinTheRequestInFlightAndAnswerPending(t *testing.T) {
	f := newJobFixture(t)
	f.p.ListingWait = 100 * time.Millisecond
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	stream := f.agent(f.host)

	status, body := f.send(http.MethodGet, snapshotsURL(job, f.target, ""), f.admin, nil)
	if status != http.StatusAccepted || body["pending"] != true {
		t.Fatalf("waited: %d %v", status, body)
	}
	command := f.received(stream)
	if status, body := f.send(http.MethodGet, snapshotsURL(job, f.target, "&refresh=1"), f.admin, nil); status != http.StatusAccepted || body["pending"] != true {
		t.Errorf("again: %d %v", status, body)
	}
	if !f.quiet(stream) {
		t.Error("a second listing went out")
	}
	f.answer(f.host, command.GetId(), agentv1.CommandState_COMMAND_STATE_SUCCEEDED, map[string]any{"snapshots": []any{}, "total": 0}, "")
	if status, body := f.send(http.MethodGet, snapshotsURL(job, f.target, ""), f.admin, nil); status != http.StatusOK || body["total"] != float64(0) {
		t.Errorf("once listed: %d %v", status, body)
	}

	// a failure, with the secret restic said hidden
	f.p.ListingWait = 5 * time.Second
	pending := f.sendLater(http.MethodGet, snapshotsURL(job, f.target, "&refresh=1"), f.admin, nil)
	command = f.received(stream)
	f.answer(f.host, command.GetId(), agentv1.CommandState_COMMAND_STATE_FAILED, nil, "failed to prepare the command: Fatal: wrong key the-secret-key")
	got := f.wait(pending)
	if got.status != http.StatusBadGateway || strings.Contains(got.raw(), "the-secret-key") || got.body["message"] != "Fatal: wrong key ***." {
		t.Errorf("a failure: %d %v", got.status, got.body)
	}
	if status, body := f.send(http.MethodGet, snapshotsURL(job, f.target, ""), f.admin, nil); status != http.StatusBadGateway {
		t.Errorf("right after a failure: %d %v", status, body)
	}
	if !f.quiet(stream) {
		t.Error("listed again right after a failure")
	}

	// an agent that does not answer in time
	pending = f.sendLater(http.MethodGet, snapshotsURL(job, f.target, "&refresh=1"), f.admin, nil)
	command = f.received(stream)
	stored, err := f.app.FindRecordById(schema.Commands, command.GetId())
	if err != nil {
		t.Fatal(err)
	}
	stored.Set("expires_at", time.Now().UTC().Add(-time.Second))
	if err := f.app.Save(stored); err != nil {
		t.Fatal(err)
	}
	f.p.Commands.Reap()
	got = f.wait(pending)
	if got.status != http.StatusGatewayTimeout || got.body["message"] != "The agent of node-1 did not answer in time: restic may still be reading the repository. Try again." {
		t.Errorf("timed out: %d %v", got.status, got.body)
	}
}

func (r reply) raw() string {
	raw, _ := r.body["raw"].(string)
	return raw
}

// TestSnapshotsReadFromTheJobsHostWhenTheTargetsHostIsOffline (#30): a
// remote target whose server is offline is listed by the job's server; a
// local target only by its own; with neither online the answer says so; a
// job or a target that is not there, or a target the job never used, is
// refused.
func TestSnapshotsReadFromTheJobsHostWhenTheTargetsHostIsOffline(t *testing.T) {
	f := newJobFixture(t)
	jobHost := testutil.NewHost(t, f.app, "node-2")
	job := f.job("data", map[string]any{"host": jobHost.Id, "source_kind": schema.SourceVolume, "source": "app_data"})

	if status, body := f.send(http.MethodGet, snapshotsURL(job, f.target, ""), f.admin, nil); status != http.StatusConflict ||
		body["message"] != "Neither node-1 nor node-2 is online: one of their agents lists the snapshots." {
		t.Errorf("neither online: %d %v", status, body)
	}
	stream := f.agent(jobHost)
	pending := f.sendLater(http.MethodGet, snapshotsURL(job, f.target, ""), f.admin, nil)
	command := f.received(stream)
	if command.GetBackupSnapshots().GetRepository().GetPassword() != "repo-password-b2" {
		t.Errorf("on the job's server: %v", command)
	}
	f.answer(jobHost, command.GetId(), agentv1.CommandState_COMMAND_STATE_SUCCEEDED, map[string]any{"snapshots": []any{}, "total": 0}, "")
	if got := f.wait(pending); got.status != http.StatusOK || got.body["host"] != jobHost.Id {
		t.Errorf("listed on the job's server: %d %v", got.status, got.body)
	}

	disk := f.addTarget("disk", backup.KindLocal, f.host, backup.Config{Path: "/srv/restic"}, backup.Credentials{})
	local := f.job("local", map[string]any{"source_kind": schema.SourceVolume, "source": "x", "targets": []string{disk.Id}})
	if status, body := f.send(http.MethodGet, snapshotsURL(local, disk, ""), f.admin, nil); status != http.StatusConflict ||
		body["message"] != "The server node-1 is offline: its agent lists the snapshots." {
		t.Errorf("a local target offline: %d %v", status, body)
	}
	// not by the server of a job that names it, though connected
	elsewhere := f.job("elsewhere", map[string]any{"host": jobHost.Id, "source_kind": schema.SourceVolume, "source": "x", "targets": []string{disk.Id}})
	if status, body := f.send(http.MethodGet, snapshotsURL(elsewhere, disk, ""), f.admin, nil); status != http.StatusConflict ||
		body["message"] != "The server node-1 is offline: its agent lists the snapshots." {
		t.Errorf("a local target on a job's server: %d %v", status, body)
	}
	if !f.quiet(stream) {
		t.Error("a local target was listed on a job's server")
	}

	if status, body := f.send(http.MethodGet, "/api/pc/backup-jobs/nosuchjob000000/snapshots?target="+f.target.Id, f.admin, nil); status != http.StatusNotFound ||
		body["message"] != "The job does not exist." {
		t.Errorf("no job: %d %v", status, body)
	}
	if status, body := f.send(http.MethodGet, "/api/pc/backup-jobs/"+job.Id+"/snapshots?target=nosuchtarget000", f.admin, nil); status != http.StatusNotFound ||
		body["message"] != "The target does not exist." {
		t.Errorf("no target: %d %v", status, body)
	}
	if status, body := f.send(http.MethodGet, snapshotsURL(job, disk, ""), f.admin, nil); status != http.StatusBadRequest ||
		body["message"] != "The job never backed up to that target." {
		t.Errorf("a target the job never used: %d %v", status, body)
	}
}

// TestBrowsingPagesAFolderAndDeletesItsCommand (#30, S/SAFE): an operator
// lists a page of a snapshot's folder through the agent, with the job's
// tag for the delivery check; the page is kept, its command does not stay,
// and a viewer may not browse.
func TestBrowsingPagesAFolderAndDeletesItsCommand(t *testing.T) {
	f := newJobFixture(t)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	_, viewer := testutil.NewUser(t, f.app, "viewer@example.com", schema.RoleViewer)
	job := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})
	stream := f.agent(f.host)
	files := func(snapshot, query string) string {
		return "/api/pc/backup-jobs/" + job.Id + "/snapshots/" + snapshot + "/files?target=" + f.target.Id + query
	}
	page := map[string]any{"path": "/volumes/app_data", "total": 450, "dirs": 50, "files": 400, "offset": 200,
		"entries": []any{map[string]any{"name": "logs", "type": "dir", "size": 0, "mtime": "2026-11-10T12:00:00Z", "mode": 493, "bad_name": false}}}

	query := "&path=" + url.QueryEscape("/volumes/app_data") + "&offset=200&limit=200&match=Log"
	pending := f.sendLater(http.MethodGet, files(snapshotID, query), operator, nil)
	command := f.received(stream)
	browse := command.GetBackupBrowse()
	if browse.GetSnapshotId() != snapshotID || browse.GetPath() != "/volumes/app_data" || browse.GetOffset() != 200 || browse.GetLimit() != 200 ||
		browse.GetMatch() != "Log" || browse.GetJobTag() != "job:"+job.Id || browse.GetRepository().GetPassword() != "repo-password-b2" {
		t.Errorf("the listing: %v", command)
	}
	f.answer(f.host, command.GetId(), agentv1.CommandState_COMMAND_STATE_SUCCEEDED, page, "")
	got := f.wait(pending)
	entries, _ := got.body["entries"].([]any)
	if got.status != http.StatusOK || got.body["total"] != float64(450) || got.body["offset"] != float64(200) || len(entries) != 1 {
		t.Errorf("the page: %d %v", got.status, got.body)
	}
	f.eventually("the listing's command going", func() bool { return f.gone(command.GetId()) })
	if status, body := f.send(http.MethodGet, files(snapshotID, query), operator, nil); status != http.StatusOK || body["total"] != float64(450) {
		t.Errorf("again: %d %v", status, body)
	}
	if !f.quiet(stream) {
		t.Error("the same page was listed again")
	}
	// the root of the snapshot, by default
	pending = f.sendLater(http.MethodGet, files(snapshotID, ""), operator, nil)
	command = f.received(stream)
	if command.GetBackupBrowse().GetPath() != "/" || command.GetBackupBrowse().GetMatch() != "" {
		t.Errorf("the root: %v", command)
	}
	f.answer(f.host, command.GetId(), agentv1.CommandState_COMMAND_STATE_SUCCEEDED, map[string]any{"path": "/", "total": 1, "entries": []any{}}, "")
	f.wait(pending)

	for query, want := range map[string]string{
		"&path=volumes":         "Give an absolute path in the snapshot.",
		"&path=%2Fa%2F..%2Fb":   "Give an absolute path in the snapshot.",
		"&path=%2Fa%2F":         "Give an absolute path in the snapshot.",
		"&limit=5000":           "A page lists at most 1000 entries.",
		"&offset=-1":            "The offset is a count of entries.",
		"&match=" + longMatch(): "Filter by a part of a name.",
	} {
		if status, body := f.send(http.MethodGet, files(snapshotID, query), operator, nil); status != http.StatusBadRequest || body["message"] != want {
			t.Errorf("%s: %d %v", query, status, body)
		}
	}
	if status, body := f.send(http.MethodGet, files("3f9a1c2e", ""), operator, nil); status != http.StatusBadRequest || body["message"] != "Choose a snapshot." {
		t.Errorf("a short id: %d %v", status, body)
	}
	if status, _ := f.send(http.MethodGet, files(snapshotID, ""), viewer, nil); status != http.StatusForbidden {
		t.Errorf("a viewer browsed: %d", status)
	}
}

func longMatch() string { return strings.Repeat("x", 256) }

// TestListingsKeptStayBounded (#30): what the listings gave is kept within
// about 32 MiB, the least recently used going first, and nothing is kept
// past its time.
func TestListingsKeptStayBounded(t *testing.T) {
	cache := listingCache{fresh: time.Hour}
	big := listingCacheBytes/2 - 1
	cache.finish("c1", "first", listing{value: map[string]any{"n": 1}, size: big})
	cache.finish("c2", "second", listing{value: map[string]any{"n": 2}, size: big})
	if _, ok := cache.peek("first"); !ok {
		t.Fatal("the first was not kept")
	}
	// the first was used last: the second goes
	cache.finish("c3", "third", listing{value: map[string]any{"n": 3}, size: big})
	if _, ok := cache.peek("second"); ok {
		t.Error("the least recently used stayed")
	}
	if _, ok := cache.peek("first"); !ok {
		t.Error("the one used last went")
	}
	if cache.bytes > listingCacheBytes {
		t.Errorf("%d bytes kept", cache.bytes)
	}
	cache.finish("c4", "huge", listing{value: map[string]any{}, size: listingCacheBytes + 1})
	if _, ok := cache.peek("huge"); ok {
		t.Error("a listing larger than the cache was kept")
	}

	short := listingCache{fresh: time.Millisecond}
	short.finish("c5", "old", listing{value: map[string]any{}})
	time.Sleep(5 * time.Millisecond)
	if _, ok := short.peek("old"); ok || short.bytes != 0 {
		t.Errorf("kept past its time: %d bytes", short.bytes)
	}
}
