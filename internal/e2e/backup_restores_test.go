package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/agent"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

// Restores (#30) end to end: the panel's routes, its delivery checks and
// finishers with real agents, whose engines run the restore's jobs with the
// backup tool and the fake restic. A volume's files are under its engine's
// VolumeDir; a server's folders are under its agent's HostRoot, where the
// fake job containers read and write them too.

// quickRestores shortens how long a restore waits before it looks whether
// the containers it started again stayed up.
func quickRestores(a *agent.Agent) { a.RestoreVerifyWait = 50 * time.Millisecond }

// restoresHarness is the jobs harness with users of each role, and what
// happened on its engine, in order: the containers that stopped and
// started, and the restore jobs that ran.
type restoresHarness struct {
	*jobsHarness
	operator string
	viewer   string

	eventsMu sync.Mutex
	events   []string
	// policies are the restart policies of the app's containers as each
	// real restore's job got its go, and restic restored
	policies []map[string]string
}

// goReader is a restore job's standard input, which notes the go the agent
// writes once it stopped the containers, as the backup tool reads it.
type goReader struct {
	in   io.Reader
	read strings.Builder
	once sync.Once
	said func()
}

func (g *goReader) Read(p []byte) (int, error) {
	n, err := g.in.Read(p)
	g.read.Write(p[:n])
	if strings.HasPrefix(g.read.String(), "go\n") {
		g.once.Do(g.said)
	}
	return n, err
}

// newRestoresHarness starts the panel and an agent whose server's folders
// are under its HostRoot. prepare readies the engine before the agent
// starts.
func newRestoresHarness(t *testing.T, prepare func(engine *fakedocker.Engine)) *restoresHarness {
	t.Helper()
	r := &restoresHarness{}
	r.jobsHarness = startJobsHarness(t, func(engine *fakedocker.Engine) {
		engine.OnStop = func(c *fakedocker.Container) {
			if c.Labels["pocketcloud.role"] == "" {
				r.note("stop:" + c.Name)
			}
		}
		engine.OnStart = func(c *fakedocker.Container) (string, string) {
			r.note("start:" + c.Name)
			return "running", c.Health
		}
		if prepare != nil {
			prepare(engine)
		}
	}, true)
	run := r.docker.OnJob
	r.docker.OnJob = func(job fakedocker.Job) fakedocker.JobResult {
		if !isRealRestore(job) || job.Stdin == nil {
			return run(job)
		}
		r.note("job:restore")
		job.Stdin = &goReader{in: job.Stdin, said: func() {
			policies := map[string]string{}
			for _, name := range r.docker.Names() {
				if container := r.docker.Find(name); container != nil && container.Labels["pocketcloud.role"] == "" {
					policy, _ := container.Host["RestartPolicy"].(map[string]any)
					policies[name], _ = policy["Name"].(string)
				}
			}
			r.eventsMu.Lock()
			r.policies = append(r.policies, policies)
			r.eventsMu.Unlock()
			r.note("go")
		}}
		result := run(job)
		r.note("job:end")
		return result
	}
	_, r.operator = testutil.NewUser(t, r.app, "operator@example.com", schema.RoleOperator)
	_, r.viewer = testutil.NewUser(t, r.app, "viewer@example.com", schema.RoleViewer)
	return r
}

// note adds to what happened; the engine's hooks note with the engine
// locked, and nothing here locks the engine.
func (r *restoresHarness) note(event string) {
	r.eventsMu.Lock()
	defer r.eventsMu.Unlock()
	r.events = append(r.events, event)
}

// happened is what happened so far, and forgets it.
func (r *restoresHarness) happened() []string {
	r.eventsMu.Lock()
	defer r.eventsMu.Unlock()
	events := r.events
	r.events = nil
	return events
}

// isRestore tells a restore's job, its preview's or its own.
func isRestore(job fakedocker.Job) bool {
	return len(job.Cmd) > 1 && job.Cmd[0] == "backup-tool" && job.Cmd[1] == "restore"
}

// isRealRestore tells a restore's own job from its preview's.
func isRealRestore(job fakedocker.Job) bool {
	return isRestore(job) && !slices.Contains(job.Cmd, "--dry-run")
}

// appContainer adds a running container of a Compose project with mounts
// and the restart policy unless-stopped.
func appContainer(engine *fakedocker.Engine, name, project string, labels map[string]string, mounts ...map[string]any) string {
	all := map[string]string{}
	if project != "" {
		all["com.docker.compose.project"] = project
		all["com.docker.compose.service"] = strings.TrimSuffix(strings.TrimPrefix(name, project+"-"), "-1")
	}
	maps.Copy(all, labels)
	id := engine.AddContainer(name, "app:1", all)
	list := []any{}
	for _, mount := range mounts {
		list = append(list, mount)
	}
	engine.SetHostConfig(name, map[string]any{"NetworkMode": "bridge", "Mounts": list,
		"RestartPolicy": map[string]any{"Name": "unless-stopped", "MaximumRetryCount": float64(0)}})
	return id
}

func volumeMount(name, target string, readOnly bool) map[string]any {
	return map[string]any{"Type": "volume", "Source": name, "Target": target, "ReadOnly": readOnly}
}

func bindMount(source, target string, readOnly bool) map[string]any {
	return map[string]any{"Type": "bind", "Source": source, "Target": target, "ReadOnly": readOnly}
}

// writeTree writes files under a directory: each path relative to it, and
// its content.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// readTree reads the files under a directory: each path relative to it,
// and its content.
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := os.ReadFile(path) //nolint:gosec // the test's own files
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		files[filepath.ToSlash(rel)] = string(content)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// listing asks a route for a listing as token, again while the panel says
// it is pending.
func (r *restoresHarness) listing(url, token string) (int, map[string]any) {
	r.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		status, out := r.send(http.MethodGet, url, token, nil)
		if status != http.StatusAccepted {
			return status, out
		}
		if out["pending"] != true || time.Now().After(deadline) {
			r.t.Fatalf("the listing %s: %d %v", url, status, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// snapshots lists a job's snapshots in a target as token.
func (r *restoresHarness) snapshots(jobID, targetID, token, query string) (int, map[string]any) {
	r.t.Helper()
	return r.listing("/api/pc/backup-jobs/"+jobID+"/snapshots?target="+targetID+query, token)
}

// newestSnapshot is the full id of a job's newest snapshot in a target.
func (r *restoresHarness) newestSnapshot(jobID, targetID string) string {
	r.t.Helper()
	status, out := r.snapshots(jobID, targetID, r.admin, "&refresh=1")
	snapshots, _ := out["snapshots"].([]any)
	if status != http.StatusOK || len(snapshots) == 0 {
		r.t.Fatalf("the snapshots: %d %v", status, out)
	}
	return snapshots[0].(map[string]any)["id"].(string)
}

func (r *restoresHarness) restore(id string) *core.Record {
	r.t.Helper()
	restore, err := r.app.FindRecordById(schema.BackupRestores, id)
	if err != nil {
		r.t.Fatal(err)
	}
	return restore
}

// waitRestore waits until a restore reaches one of the statuses.
func (r *restoresHarness) waitRestore(id string, statuses ...string) *core.Record {
	r.t.Helper()
	var restore *core.Record
	r.waitFor("the restore "+id+" to be "+strings.Join(statuses, " or "), func() bool {
		found, err := r.app.FindRecordById(schema.BackupRestores, id)
		if err != nil {
			return false
		}
		restore = found
		return slices.Contains(statuses, found.GetString("status"))
	})
	return restore
}

// ended waits until a restore that was started ended, and its finisher,
// which notes it in the events and alerts after it saves it, is done.
func (r *restoresHarness) ended(id string) *core.Record {
	r.t.Helper()
	restore := r.waitRestore(id, schema.RestoreSucceeded, schema.RestoreFailed, schema.RestoreCancelled)
	r.waitFor("the restore's event", func() bool {
		found, err := r.app.FindRecordsByFilter(schema.Events, "data.restore={:id}", "", 0, 0, dbx.Params{"id": id})
		return err == nil && len(found) > 0
	})
	return restore
}

// preview makes a restore's draft as token and waits for its preview.
func (r *restoresHarness) preview(jobID, token string, body map[string]any) *core.Record {
	r.t.Helper()
	status, out := r.send(http.MethodPost, "/api/pc/backup-jobs/"+jobID+"/restores", token, body)
	if status != http.StatusAccepted || out["status"] != schema.RestorePreviewing {
		r.t.Fatalf("the restore's draft: %d %v", status, out)
	}
	return r.waitRestore(out["id"].(string), schema.RestorePreviewed, schema.RestoreFailed)
}

// start starts a restore from its preview as token.
func (r *restoresHarness) start(id, token string, confirm bool) (int, map[string]any) {
	r.t.Helper()
	return r.send(http.MethodPost, "/api/pc/backup-restores/"+id+"/start", token, map[string]any{"confirm_overwrite": confirm})
}

// previewOf is a restore's preview.
func previewOf(t *testing.T, restore *core.Record) map[string]any {
	t.Helper()
	preview := map[string]any{}
	if err := restore.UnmarshalJSONField("preview", &preview); err != nil {
		t.Fatalf("the preview: %v", err)
	}
	return preview
}

// resultOf is a command's result.
func resultOf(t *testing.T, command *core.Record) map[string]any {
	t.Helper()
	result := map[string]any{}
	if err := command.UnmarshalJSONField("result", &result); err != nil {
		t.Fatalf("the result: %v", err)
	}
	return result
}

// names are the names a list of a result holds, sorted: of strings, or of
// objects with a name.
func names(list any) []string {
	found := []string{}
	items, _ := list.([]any)
	for _, item := range items {
		switch item := item.(type) {
		case string:
			found = append(found, item)
		case map[string]any:
			found = append(found, fmt.Sprint(item["name"]))
		}
	}
	sort.Strings(found)
	return found
}

// policyOf is a container's restart policy.
func (r *restoresHarness) policyOf(engine *fakedocker.Engine, name string) string {
	r.t.Helper()
	container := engine.Find(name)
	if container == nil {
		r.t.Fatalf("no container %s", name)
	}
	policy, _ := container.Host["RestartPolicy"].(map[string]any)
	name, _ = policy["Name"].(string)
	return name
}

// gone waits until a command was removed.
func (r *restoresHarness) gone(id string) {
	r.t.Helper()
	r.waitFor("the command "+id+" to be removed", func() bool {
		_, err := r.app.FindRecordById(schema.Commands, id)
		return err != nil
	})
}

// before tells whether each event happened before the next.
func before(events []string, order ...string) bool {
	at := -1
	for _, event := range order {
		i := slices.Index(events, event)
		if i <= at {
			return false
		}
		at = i
	}
	return true
}

// secondServer enrolls another server with an engine and a HostRoot of its
// own, whose jobs run with the backup tool and the fake restic too.
type secondServer struct {
	agent  *runningAgent
	host   *core.Record
	engine *fakedocker.Engine
	root   string

	mu   sync.Mutex
	jobs []fakedocker.Job
}

func (s *secondServer) ranJobs() []fakedocker.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.jobs)
}

// enrolledOn starts an agent of a server named name on an engine of its
// own, with its own HostRoot, and waits until it is connected.
func (h *harness) enrolledOn(name string, prepare func(engine *fakedocker.Engine)) *secondServer {
	h.t.Helper()
	s := &secondServer{engine: fakedocker.New(h.t), root: fakeHostRoot(h.t)}
	s.engine.AddImage("sha256:"+strings.Repeat("a", 64), nil)
	s.engine.AddContainer("pocketcloud-agent", "ghcr.io/oldbear24/pocketcloud-agent:dev", map[string]string{"pocketcloud.role": "agent"})
	s.engine.VolumeRoot = h.t.TempDir()
	if prepare != nil {
		prepare(s.engine)
	}
	fakerestic.RunJobsWith(s.engine, agent.BackupTool, fakerestic.Options{ServerRoot: s.root})
	run := s.engine.OnJob
	s.engine.OnJob = func(job fakedocker.Job) fakedocker.JobResult {
		s.mu.Lock()
		s.jobs = append(s.jobs, job)
		s.mu.Unlock()
		return run(job)
	}
	config := h.agentConfig(name, h.token(name, schema.HostRoleDocker))
	config.DockerHost = s.engine.Host()
	config.HostRoot = s.root
	s.agent = h.start(config, quickRestores)
	h.waitFor("the agent of "+name+" to connect", func() bool {
		found, err := h.app.FindFirstRecordByData(schema.Hosts, "name", name)
		if err != nil || !found.GetBool("stream_open") {
			return false
		}
		s.host = found
		return true
	})
	return s
}

// TestRestoringAVolumeBringsBackTheSnapshot (#30, AC1): an operator lists a
// volume job's snapshots, previews a restore to the original location with
// replace, which changes nothing and says what would change and which
// containers would stop, confirms the overwrite and restores: the volume
// holds what the snapshot holds, a file changed since is back, one added
// since is gone, one removed since is back. The container that writes the
// volume stopped before the restore's job, with the restart policy no, and
// started after it with its own policy again; one that reads a parent
// folder read-only kept running. The job mounted only the volume writable.
func TestRestoringAVolumeBringsBackTheSnapshot(t *testing.T) {
	r := newRestoresHarness(t, func(engine *fakedocker.Engine) {
		engine.AddVolume("app_data")
		writeTree(t, engine.VolumeDir("app_data"), map[string]string{"a.txt": "v1", "c.txt": "kept", "docs/readme.md": "# app"})
		appContainer(engine, "app-web-1", "app", nil, volumeMount("app_data", "/data", false))
		appContainer(engine, "node-exporter", "", nil, bindMount("/", "/host", true))
	})
	id := r.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data"})["id"].(string)
	if run := r.runNow(id)[0]; run.GetString("status") != schema.RunSucceeded {
		t.Fatalf("the backup: %v", run.FieldsData())
	}
	volume := r.docker.VolumeDir("app_data")
	want := readTree(t, volume)
	// what changed since the snapshot
	writeTree(t, volume, map[string]string{"a.txt": "v2", "b.txt": "added later"})
	if err := os.Remove(filepath.Join(volume, "c.txt")); err != nil {
		t.Fatal(err)
	}
	changed := readTree(t, volume)
	snapshot := r.newestSnapshot(id, r.disk.Id)

	draft := r.preview(id, r.operator, map[string]any{"target": r.disk.Id, "snapshot": snapshot, "mode": "original", "replace": true})
	if draft.GetString("status") != schema.RestorePreviewed {
		t.Fatalf("the preview: %v", draft.FieldsData())
	}
	preview := previewOf(t, draft)
	counts, _ := preview["counts"].(map[string]any)
	if counts["new"].(float64) < 1 || counts["updated"].(float64) < 1 || counts["deleted"].(float64) < 1 {
		t.Errorf("the preview's counts: %v", preview)
	}
	if stops := names(preview["would_stop"]); !slices.Equal(stops, []string{"app-web-1"}) {
		t.Errorf("the preview stops %v", stops)
	}
	if got := readTree(t, volume); !maps.Equal(got, changed) {
		t.Errorf("the preview changed the volume: %v", got)
	}
	r.gone(draft.GetString("preview_command"))
	if events := r.happened(); slices.ContainsFunc(events, func(event string) bool { return strings.HasPrefix(event, "stop:") }) {
		t.Errorf("the preview stopped containers: %v", events)
	}

	// the original location is overwritten: only once someone confirms it
	if status, out := r.start(draft.Id, r.operator, false); status != http.StatusConflict ||
		out["message"] != "The destination already holds files: confirm that the restore overwrites them." {
		t.Fatalf("a start without the confirmation: %d %v", status, out)
	}
	if r.restore(draft.Id).GetString("status") != schema.RestorePreviewed {
		t.Error("a start without the confirmation changed the restore")
	}
	status, out := r.start(draft.Id, r.operator, true)
	if status != http.StatusOK || out["status"] != schema.RestoreQueued || out["overwrite"] != true || out["confirmed_label"] != "operator@example.com" {
		t.Fatalf("the start: %d %v", status, out)
	}
	restore := r.ended(draft.Id)
	if restore.GetString("status") != schema.RestoreSucceeded || restore.GetString("error") != "" || restore.GetString("app_problem") != "" ||
		restore.GetInt("files_restored") < 1 || restore.GetDateTime("finished_at").IsZero() {
		t.Fatalf("the restore: %v", restore.FieldsData())
	}
	if got := readTree(t, volume); !maps.Equal(got, want) {
		t.Errorf("the volume holds %v, not the snapshot's %v", got, want)
	}

	events := r.happened()
	if !before(events, "job:restore", "stop:app-web-1", "go", "job:end", "start:app-web-1") || slices.Contains(events, "stop:node-exporter") {
		t.Errorf("what happened: %v", events)
	}
	if len(r.policies) != 1 || r.policies[0]["app-web-1"] != "no" || r.policies[0]["node-exporter"] != "unless-stopped" {
		t.Errorf("the restart policies during the restore: %v", r.policies)
	}
	if policy := r.policyOf(r.docker, "app-web-1"); policy != "unless-stopped" {
		t.Errorf("the restart policy after the restore: %s", policy)
	}
	if r.docker.Find("app-web-1").Status != "running" {
		t.Error("the app does not run")
	}
	stopped := []map[string]any{}
	if err := restore.UnmarshalJSONField("app_stopped", &stopped); err != nil || len(stopped) != 1 || stopped[0]["name"] != "app-web-1" {
		t.Errorf("the restore's stopped containers: %v %v", stopped, err)
	}
	result := resultOf(t, r.command(restore.GetString("command")))
	if !slices.Equal(names(result["app_started"]), []string{"app-web-1"}) || result["partial"] != false {
		t.Errorf("the restore's result: %v", result)
	}

	// the job could write the volume and nothing else of the server
	var jobs []fakedocker.Job
	for _, job := range r.ranJobs() {
		if isRealRestore(job) {
			jobs = append(jobs, job)
		}
	}
	if len(jobs) != 1 {
		t.Fatalf("%d restore jobs", len(jobs))
	}
	mounted := mounts(jobs[0])
	if mount := mounted["/restore/0"]; mount["Type"] != "volume" || mount["Source"] != "app_data" || mount["ReadOnly"] == true {
		t.Errorf("the volume's mount: %v", mount)
	}
	for target, mount := range mounted {
		switch target {
		case "/restore/0":
		case "/repo":
			// the local target's repository, where restic keeps its lock
			if mount["Source"] != r.diskDir {
				t.Errorf("the repository's mount: %v", mount)
			}
		default:
			t.Errorf("the job also mounts %s: %v", target, mount)
		}
	}
	caps := capabilities(jobs[0])
	if caps != "[CHOWN DAC_OVERRIDE FOWNER FSETID]" || fmt.Sprint(jobs[0].Host["CapDrop"]) != "[ALL]" ||
		!strings.Contains(fmt.Sprint(jobs[0].Host["SecurityOpt"]), "no-new-privileges") {
		t.Errorf("the job's privileges: %v %v %v", caps, jobs[0].Host["CapDrop"], jobs[0].Host["SecurityOpt"])
	}

	entry, err := r.app.FindFirstRecordByFilter(schema.AuditLog, "action={:action}", dbx.Params{"action": "backup_restore"})
	if err != nil || entry.GetString("actor_label") != "operator@example.com" {
		t.Errorf("the audit of the restore: %v %v", entry, err)
	}
	if found, err := r.app.FindRecordsByFilter(schema.Events, "kind='backup.restored'", "", 0, 0); err != nil || len(found) != 1 {
		t.Errorf("the restore's events: %v %v", found, err)
	}
}

// encode is a query's value.
func encode(value string) string { return url.QueryEscape(value) }

// capabilities are a job's added capabilities, sorted.
func capabilities(job fakedocker.Job) string {
	caps := []string{}
	list, _ := job.Host["CapAdd"].([]any)
	for _, capability := range list {
		caps = append(caps, fmt.Sprint(capability))
	}
	sort.Strings(caps)
	return fmt.Sprint(caps)
}

// progressOf is a command's progress.
func progressOf(command *core.Record) map[string]any {
	progress := map[string]any{}
	_ = command.UnmarshalJSONField("progress", &progress)
	return progress
}

// realRestores are the restore jobs that ran on an engine, not previews.
func realRestores(jobs []fakedocker.Job) []fakedocker.Job {
	var found []fakedocker.Job
	for _, job := range jobs {
		if isRealRestore(job) {
			found = append(found, job)
		}
	}
	return found
}

// TestTheAppStartsAgainWhenARestoreFailsOrIsStopped (#30, AC1/SAFE): the
// containers a restore stopped start again, with their own restart policy,
// when restic fails part of the way, which leaves part of the snapshot and
// raises the alert of the data, and when someone stops the restore while
// restic restores, which ends the restore only once the agent says it
// started them again.
func TestTheAppStartsAgainWhenARestoreFailsOrIsStopped(t *testing.T) {
	r := newRestoresHarness(t, func(engine *fakedocker.Engine) {
		engine.AddVolume("app_data")
		files := map[string]string{}
		for i := range 8 {
			files[fmt.Sprintf("f%d.txt", i)] = fmt.Sprintf("v1 of %d", i)
		}
		writeTree(t, engine.VolumeDir("app_data"), files)
		appContainer(engine, "app-web-1", "app", nil, volumeMount("app_data", "/data", false))
	})
	id := r.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data"})["id"].(string)
	r.runNow(id)
	volume := r.docker.VolumeDir("app_data")
	changed := map[string]string{}
	for i := range 8 {
		changed[fmt.Sprintf("f%d.txt", i)] = fmt.Sprintf("v2 of %d", i)
	}
	writeTree(t, volume, changed)
	snapshot := r.newestSnapshot(id, r.disk.Id)
	body := map[string]any{"target": r.disk.Id, "snapshot": snapshot, "mode": "original", "replace": true}

	// restic fails after the first file
	draft := r.preview(id, r.operator, body)
	r.happened()
	t.Setenv("FAKE_RESTIC_RESTORE_FAIL_AFTER", "1")
	if status, out := r.start(draft.Id, r.operator, true); status != http.StatusOK {
		t.Fatalf("the start: %d %v", status, out)
	}
	restore := r.ended(draft.Id)
	if restore.GetString("status") != schema.RestoreFailed || !restore.GetBool("partial") || restore.GetString("error") == "" {
		t.Fatalf("the failed restore: %v", restore.FieldsData())
	}
	if events := r.happened(); !before(events, "job:restore", "stop:app-web-1", "go", "job:end", "start:app-web-1") {
		t.Errorf("what happened as it failed: %v", events)
	}
	if r.docker.Find("app-web-1").Status != "running" || r.policyOf(r.docker, "app-web-1") != "unless-stopped" {
		t.Errorf("the app after the failure: %v", r.docker.Find("app-web-1"))
	}
	if result := resultOf(t, r.command(restore.GetString("command"))); !slices.Equal(names(result["app_started"]), []string{"app-web-1"}) {
		t.Errorf("the failed restore's result: %v", result)
	}
	alert := r.openAlerts("backup_restore")[r.host.Id+":volume:app_data"]
	if alert == nil || alert.GetString("severity") != schema.SeverityCritical || !strings.Contains(alert.GetString("message"), "The application was started again.") {
		t.Errorf("the alert: %v", alert)
	}

	// someone stops a restore while restic restores
	t.Setenv("FAKE_RESTIC_RESTORE_FAIL_AFTER", "")
	draft = r.preview(id, r.operator, body)
	r.happened()
	t.Setenv("FAKE_RESTIC_SLOW_MS", "400")
	if status, out := r.start(draft.Id, r.operator, true); status != http.StatusOK {
		t.Fatalf("the start: %d %v", status, out)
	}
	command := r.waitRestore(draft.Id, schema.RestoreRunning).GetString("command")
	r.waitFor("restic to restore", func() bool { return progressOf(r.command(command))["step"] == "restore" })
	if progress := progressOf(r.command(command)); progress["percent"].(float64) < 10 || progress["message"] == "" {
		t.Errorf("the restore's progress: %v", progress)
	}
	status, out := r.send(http.MethodPost, "/api/pc/backup-restores/"+draft.Id+"/cancel", r.operator, nil)
	if status != http.StatusAccepted || out["stopping"] != true {
		t.Fatalf("the stop: %d %v", status, out)
	}
	if stopping := r.command(command); stopping.GetDateTime("cancel_requested_at").IsZero() {
		t.Errorf("the command is not stopping: %v", stopping.FieldsData())
	}
	restore = r.ended(draft.Id)
	if restore.GetString("status") != schema.RestoreCancelled || restore.GetString("stop_requested_label") != "operator@example.com" ||
		restore.GetString("error") != "Stopped by operator@example.com before it was complete: the destination holds part of the snapshot. The application was started again." {
		t.Fatalf("the stopped restore: %v", restore.FieldsData())
	}
	if events := r.happened(); !before(events, "job:restore", "stop:app-web-1", "go", "job:end", "start:app-web-1") {
		t.Errorf("what happened as it stopped: %v", events)
	}
	if r.docker.Find("app-web-1").Status != "running" || r.policyOf(r.docker, "app-web-1") != "unless-stopped" {
		t.Errorf("the app after the stop: %v", r.docker.Find("app-web-1"))
	}
	stopped := r.command(command)
	if stopped.GetString("status") != schema.CommandCancelled || !slices.Equal(names(resultOf(t, stopped)["app_started"]), []string{"app-web-1"}) {
		t.Errorf("the stopped command: %v", stopped.FieldsData())
	}
	if status, out := r.send(http.MethodPost, "/api/pc/backup-restores/"+draft.Id+"/cancel", r.operator, nil); status != http.StatusBadRequest ||
		out["message"] != "The restore has ended." {
		t.Errorf("a stop of a restore that ended: %d %v", status, out)
	}
}

// TestRestoringAStacksVolumesRestoresEachIntoItsOwn (#30, AC1): a stack's
// restore puts each of its volumes back as its snapshot holds it, stopping
// the stack's containers, the one that depends on another first, and
// starting them in the other order. While it is under way the stack is
// held: the API starts none of its containers, and its job is not backed
// up by hand.
func TestRestoringAStacksVolumesRestoresEachIntoItsOwn(t *testing.T) {
	r := newRestoresHarness(t, func(engine *fakedocker.Engine) {
		for name, content := range map[string]string{"shop_db": "rows v1", "shop_uploads": "logo v1"} {
			engine.AddVolume(name)
			writeTree(t, engine.VolumeDir(name), map[string]string{"data": content})
		}
		appContainer(engine, "shop-db-1", "shop", nil, volumeMount("shop_db", "/var/lib/postgresql/data", false))
		appContainer(engine, "shop-web-1", "shop", map[string]string{"com.docker.compose.depends_on": "db:service_started:false"},
			volumeMount("shop_uploads", "/uploads", false))
	})
	r.waitFor("the volumes in the inventory", func() bool {
		volumes, err := r.app.FindRecordsByFilter(schema.Volumes, "host={:host}", "", 0, 0, dbx.Params{"host": r.host.Id})
		return err == nil && len(volumes) == 2
	})
	for _, name := range []string{"shop_db", "shop_uploads"} {
		volume, err := r.app.FindFirstRecordByFilter(schema.Volumes, "host={:host} && name={:name}", dbx.Params{"host": r.host.Id, "name": name})
		if err != nil {
			t.Fatal(err)
		}
		volume.Set("labels", map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.volume": strings.TrimPrefix(name, "shop_")})
		if err := r.app.Save(volume); err != nil {
			t.Fatal(err)
		}
	}
	shop := r.saveStack(r.host.Id, "shop", "services: {}\n")
	id := r.addJob(map[string]any{"name": "shop", "source_kind": "stack", "stack": shop})["id"].(string)
	if run := r.runNow(id)[0]; run.GetString("status") != schema.RunSucceeded {
		t.Fatalf("the backup: %v", run.FieldsData())
	}
	writeTree(t, r.docker.VolumeDir("shop_db"), map[string]string{"data": "rows v2", "wal": "later"})
	writeTree(t, r.docker.VolumeDir("shop_uploads"), map[string]string{"data": "logo v2"})
	snapshot := r.newestSnapshot(id, r.disk.Id)

	draft := r.preview(id, r.operator, map[string]any{"target": r.disk.Id, "snapshot": snapshot, "mode": "original", "replace": true})
	preview := previewOf(t, draft)
	if stops := names(preview["would_stop"]); !slices.Equal(stops, []string{"shop-db-1", "shop-web-1"}) {
		t.Errorf("the preview stops %v", stops)
	}
	if roots, _ := preview["roots"].([]any); len(roots) != 2 || draft.GetString("stack_name") != "shop" {
		t.Errorf("the preview's roots: %v, the stack %q", preview["roots"], draft.GetString("stack_name"))
	}
	r.happened()

	entered, release := r.gateJobs(isRealRestore, nil)
	if status, out := r.start(draft.Id, r.operator, true); status != http.StatusOK {
		t.Fatalf("the start: %d %v", status, out)
	}
	select {
	case <-entered:
	case <-time.After(waitLimit):
		t.Fatal("the restore did not start")
	}
	r.waitRestore(draft.Id, schema.RestoreRunning)
	db := r.docker.Find("shop-db-1")
	r.waitForInventory(r.host.Id, db.ID)
	held := "The stack shop is being restored from a backup: this waits until that is done."
	if status, out := r.send(http.MethodPost, "/api/collections/commands/records", r.operator, map[string]any{
		"host": r.host.Id, "type": "container.start", "payload": map[string]any{"docker_id": db.ID},
	}); status != http.StatusConflict || out["message"] != held {
		t.Errorf("a start of the stack's container: %d %v", status, out)
	}
	if status, out := r.send(http.MethodPost, "/api/pc/backup-jobs/"+id+"/run", r.operator, nil); status != http.StatusConflict ||
		out["message"] != "A restore into its data is under way: back it up once it ends." {
		t.Errorf("a backup by hand: %d %v", status, out)
	}
	release()

	restore := r.ended(draft.Id)
	if restore.GetString("status") != schema.RestoreSucceeded {
		t.Fatalf("the restore: %v", restore.FieldsData())
	}
	if got := readTree(t, r.docker.VolumeDir("shop_db")); !maps.Equal(got, map[string]string{"data": "rows v1"}) {
		t.Errorf("shop_db holds %v", got)
	}
	if got := readTree(t, r.docker.VolumeDir("shop_uploads")); !maps.Equal(got, map[string]string{"data": "logo v1"}) {
		t.Errorf("shop_uploads holds %v", got)
	}
	events := r.happened()
	if !before(events, "job:restore", "stop:shop-web-1", "stop:shop-db-1", "go", "job:end", "start:shop-db-1", "start:shop-web-1") {
		t.Errorf("what happened: %v", events)
	}
	jobs := realRestores(r.ranJobs())
	if len(jobs) != 1 {
		t.Fatalf("%d restore jobs", len(jobs))
	}
	writable := []string{}
	for target, mount := range mounts(jobs[0]) {
		if mount["ReadOnly"] != true && target != "/repo" {
			writable = append(writable, fmt.Sprint(mount["Source"]))
		}
	}
	sort.Strings(writable)
	if !slices.Equal(writable, []string{"shop_db", "shop_uploads"}) {
		t.Errorf("the job writes %v", writable)
	}
}

// TestRestoringToAnotherServerSendsTheSecretsOnlyThere (#30, AC2): an admin
// restores a remote target's snapshot into a new folder of another server,
// which its agent creates: the files are there, the server that backed up
// ran nothing, the other server's agent got the target's secrets in the
// preview and the restore only, and the panel keeps them in neither. An
// operator restores to the original location only.
func TestRestoringToAnotherServerSendsTheSecretsOnlyThere(t *testing.T) {
	r := newRestoresHarness(t, func(engine *fakedocker.Engine) {
		engine.AddVolume("app_data")
		writeTree(t, engine.VolumeDir("app_data"), map[string]string{"a.txt": "v1", "docs/readme.md": "# app"})
	})
	b2 := r.addS3("b2", "b2-secret-key")
	id := r.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data", "targets": []string{b2.Id}})["id"].(string)
	if run := r.runNow(id)[0]; run.GetString("status") != schema.RunSucceeded {
		t.Fatalf("the backup: %v", run.FieldsData())
	}
	want := readTree(t, r.docker.VolumeDir("app_data"))
	nodeB := r.enrolledOn("node-b", nil)
	snapshot := r.newestSnapshot(id, b2.Id)
	body := map[string]any{"target": b2.Id, "snapshot": snapshot, "mode": "path", "host": nodeB.host.Id, "path": "/srv/restored/app"}

	if status, out := r.send(http.MethodPost, "/api/pc/backup-jobs/"+id+"/restores", r.operator, body); status != http.StatusForbidden ||
		out["message"] != "Only admins restore to a new folder." {
		t.Errorf("an operator's restore to a new folder: %d %v", status, out)
	}
	ranBefore := len(r.ranJobs())
	draft := r.preview(id, r.admin, body)
	if draft.GetString("status") != schema.RestorePreviewed || draft.GetString("source") != "/volumes/app_data" {
		t.Fatalf("the preview: %v", draft.FieldsData())
	}
	roots, _ := previewOf(t, draft)["roots"].([]any)
	if len(roots) != 1 || roots[0].(map[string]any)["exists"] != false || roots[0].(map[string]any)["in_snapshot"] != true {
		t.Errorf("the preview's roots: %v", roots)
	}
	// nothing is there to overwrite
	status, out := r.start(draft.Id, r.admin, false)
	if status != http.StatusOK || out["overwrite"] == true {
		t.Fatalf("the start: %d %v", status, out)
	}
	restore := r.ended(draft.Id)
	if restore.GetString("status") != schema.RestoreSucceeded {
		t.Fatalf("the restore: %v", restore.FieldsData())
	}
	if got := readTree(t, filepath.Join(nodeB.root, "srv", "restored", "app")); !maps.Equal(got, want) {
		t.Errorf("node-b's folder holds %v, not %v", got, want)
	}
	if _, err := os.Stat(filepath.Join(r.hostRoot, "srv", "restored")); !os.IsNotExist(err) {
		t.Errorf("backup-node has the folder too: %v", err)
	}
	for _, job := range r.ranJobs()[ranBefore:] {
		t.Errorf("backup-node ran a job: %v", job.Cmd)
	}
	jobs := nodeB.ranJobs()
	if len(jobs) != 2 || !isRestore(jobs[0]) || !isRealRestore(jobs[1]) {
		t.Fatalf("node-b ran %d jobs", len(jobs))
	}
	for _, job := range jobs {
		if !slices.Contains(job.Env, "AWS_SECRET_ACCESS_KEY=b2-secret-key") {
			t.Errorf("a job of node-b has no secret: %v", job.Cmd)
		}
	}
	if mount := mounts(jobs[1])["/restore/0"]; mount["Type"] != "bind" || mount["Source"] != "/srv/restored/app" || mount["ReadOnly"] == true {
		t.Errorf("the folder's mount: %v", mount)
	}

	password, err := r.panel.Box.OpenString(r.targetNamed("b2").GetString("repo_password"))
	if err != nil {
		t.Fatal(err)
	}
	command := r.command(restore.GetString("command"))
	payload := map[string]any{}
	if err := command.UnmarshalJSONField("payload", &payload); err != nil {
		t.Fatal(err)
	}
	if repository, _ := payload["repository"].(map[string]any); len(repository) != 2 || repository["target_id"] != b2.Id || repository["url"] == nil {
		t.Errorf("the stored repository: %v", payload["repository"])
	}
	kept := []string{}
	for _, collection := range []string{schema.Commands, schema.BackupRestores, schema.Events, schema.AuditLog} {
		records, err := r.app.FindAllRecords(collection)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range records {
			raw, _ := json.Marshal(record.FieldsData())
			kept = append(kept, string(raw))
		}
	}
	state, err := os.ReadFile(filepath.Join(nodeB.agent.config.StateDir, "commands.json"))
	if err != nil {
		t.Fatal(err)
	}
	kept = append(kept, string(state))
	for _, text := range kept {
		for _, secret := range []string{"b2-secret-key", password} {
			if strings.Contains(text, secret) {
				t.Errorf("a secret is kept: %.6s… in %.300s", secret, text)
			}
		}
	}
}

// TestALocalTargetRestoresOnlyOnItsOwnServer (#30, AC2/SAFE): a local
// target is a folder of its server, which alone restores from it: the
// route refuses another server, and a record made by hand that sends its
// preview there gets no secret, as its command fails as it is delivered.
func TestALocalTargetRestoresOnlyOnItsOwnServer(t *testing.T) {
	r := newRestoresHarness(t, func(engine *fakedocker.Engine) {
		engine.AddVolume("app_data")
		writeTree(t, engine.VolumeDir("app_data"), map[string]string{"a.txt": "v1"})
	})
	id := r.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data"})["id"].(string)
	run := r.runNow(id)[0]
	nodeB := r.enrolledOn("node-b", nil)
	snapshot := r.newestSnapshot(id, r.disk.Id)
	if status, out := r.send(http.MethodPost, "/api/pc/backup-jobs/"+id+"/restores", r.admin, map[string]any{
		"target": r.disk.Id, "snapshot": snapshot, "mode": "path", "host": nodeB.host.Id, "path": "/srv/restored/app",
	}); status != http.StatusBadRequest || out["message"] != "The target disk is a folder of backup-node: only that server restores from it." {
		t.Errorf("a restore of a local target on another server: %d %v", status, out)
	}

	// as the route would have saved it, and its preview
	stored := map[string]any{}
	if err := r.command(run.GetString("command")).UnmarshalJSONField("payload", &stored); err != nil {
		t.Fatal(err)
	}
	repository, _ := stored["repository"].(map[string]any)
	collection, err := r.app.FindCollectionByNameOrId(schema.BackupRestores)
	if err != nil {
		t.Fatal(err)
	}
	forged := core.NewRecord(collection)
	forged.Id = core.GenerateDefaultRandomId()
	previewID := core.GenerateDefaultRandomId()
	forged.Load(map[string]any{"job": id, "target": r.disk.Id, "host": nodeB.host.Id, "snapshot_id": snapshot, "mode": schema.RestorePath,
		"path": "/srv/restored/app", "source": "/volumes/app_data", "include": []string{}, "status": schema.RestorePreviewing,
		"requested_by": r.adminID, "requested_label": "admin@example.com", "preview_command": previewID})
	err = r.app.RunInTransaction(func(tx core.App) error {
		_, err := r.panel.Commands.CreateIn(context.Background(), tx, commands.Options{ID: previewID, Host: nodeB.host.Id,
			Command: &agentv1.Command{Payload: &agentv1.Command_BackupRestore{BackupRestore: &agentv1.BackupRestore{
				Repository: &agentv1.BackupRepository{TargetId: r.disk.Id, Url: fmt.Sprint(repository["url"])},
				SnapshotId: snapshot, RestoreId: forged.Id, JobTag: "job:" + id, TargetPath: "/srv/restored/app", Source: "/volumes/app_data", DryRun: true,
			}}}})
		if err != nil {
			return err
		}
		return tx.Save(forged)
	})
	if err != nil {
		t.Fatal(err)
	}
	// the preview's command fails as it is delivered, and goes once its
	// draft kept why
	restore := r.waitRestore(forged.Id, schema.RestoreFailed)
	if restore.GetString("error") != "A local target is a directory of its own server: only that server restores from it." {
		t.Errorf("the forged restore: %v", restore.FieldsData())
	}
	r.gone(previewID)
	if jobs := nodeB.ranJobs(); len(jobs) != 0 {
		t.Errorf("node-b ran %d jobs", len(jobs))
	}
}

// TestOverwritingNeedsExplicitConfirmation (#30, AC3): a restore into a
// folder that was empty as its preview saw it, and filled since, is refused
// by the agent without the confirmation, and leaves the folder as it is,
// byte for byte; once a new preview sees the files, the restore needs the
// confirmation, and with it restores. A restore command made by hand gets
// no secret and stops nothing.
func TestOverwritingNeedsExplicitConfirmation(t *testing.T) {
	r := newRestoresHarness(t, func(engine *fakedocker.Engine) {
		engine.AddVolume("app_data")
		writeTree(t, engine.VolumeDir("app_data"), map[string]string{"a.txt": "v1"})
		appContainer(engine, "app-web-1", "app", nil, volumeMount("app_data", "/data", false))
	})
	id := r.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data"})["id"].(string)
	r.runNow(id)
	snapshot := r.newestSnapshot(id, r.disk.Id)
	folder := filepath.Join(r.hostRoot, "srv", "x")
	body := map[string]any{"target": r.disk.Id, "snapshot": snapshot, "mode": "path", "host": r.host.Id, "path": "/srv/x"}

	draft := r.preview(id, r.admin, body)
	roots, _ := previewOf(t, draft)["roots"].([]any)
	if len(roots) != 1 || roots[0].(map[string]any)["exists"] != false {
		t.Fatalf("the preview's roots: %v", roots)
	}
	writeTree(t, folder, map[string]string{"mine.txt": "keep me"})
	if status, out := r.start(draft.Id, r.admin, false); status != http.StatusOK || out["overwrite"] == true {
		t.Fatalf("the start: %d %v", status, out)
	}
	restore := r.ended(draft.Id)
	if restore.GetString("status") != schema.RestoreFailed ||
		!strings.Contains(restore.GetString("error"), "holds 1 items and the restore was not confirmed to overwrite them: preview again") {
		t.Fatalf("the restore into a folder filled after its preview: %v", restore.FieldsData())
	}
	if got := readTree(t, folder); !maps.Equal(got, map[string]string{"mine.txt": "keep me"}) {
		t.Errorf("the folder holds %v", got)
	}

	draft = r.preview(id, r.admin, body)
	roots, _ = previewOf(t, draft)["roots"].([]any)
	if len(roots) != 1 || roots[0].(map[string]any)["entries"] != float64(1) {
		t.Fatalf("the new preview's roots: %v", roots)
	}
	if status, out := r.start(draft.Id, r.admin, false); status != http.StatusConflict ||
		out["message"] != "The destination already holds files: confirm that the restore overwrites them." {
		t.Fatalf("a start without the confirmation: %d %v", status, out)
	}
	if status, out := r.start(draft.Id, r.admin, true); status != http.StatusOK || out["overwrite"] != true {
		t.Fatalf("the confirmed start: %d %v", status, out)
	}
	if restore := r.ended(draft.Id); restore.GetString("status") != schema.RestoreSucceeded {
		t.Fatalf("the confirmed restore: %v", restore.FieldsData())
	}
	if got := readTree(t, folder); !maps.Equal(got, map[string]string{"mine.txt": "keep me", "a.txt": "v1"}) {
		t.Errorf("the folder holds %v", got)
	}

	// replace into a new folder that is empty deletes nothing, and needs no
	// confirmation
	body["path"], body["replace"] = "/srv/y", true
	draft = r.preview(id, r.admin, body)
	if status, out := r.start(draft.Id, r.admin, false); status != http.StatusOK {
		t.Fatalf("the start of a replace into a new folder: %d %v", status, out)
	}
	if restore := r.ended(draft.Id); restore.GetString("status") != schema.RestoreSucceeded {
		t.Fatalf("a replace into a new folder: %v", restore.FieldsData())
	}
	if got := readTree(t, filepath.Join(r.hostRoot, "srv", "y")); !maps.Equal(got, map[string]string{"a.txt": "v1"}) {
		t.Errorf("the new folder holds %v", got)
	}

	// a restore command a superuser makes by hand, of a restore previewed
	writeTree(t, r.docker.VolumeDir("app_data"), map[string]string{"a.txt": "v2"})
	draft = r.preview(id, r.operator, map[string]any{"target": r.disk.Id, "snapshot": snapshot, "mode": "original", "replace": true})
	r.happened()
	jobsBefore := len(r.ranJobs())
	stored := map[string]any{}
	if err := r.command(r.runs(id)[0].GetString("command")).UnmarshalJSONField("payload", &stored); err != nil {
		t.Fatal(err)
	}
	repository, _ := stored["repository"].(map[string]any)
	_, root := testutil.NewSuperuser(t, r.app)
	status, out := r.send(http.MethodPost, "/api/collections/commands/records", root, map[string]any{
		"host": r.host.Id, "type": "backup.restore", "payload": map[string]any{
			"repository":  map[string]any{"target_id": r.disk.Id, "url": repository["url"]},
			"snapshot_id": snapshot, "restore_id": draft.Id, "job_tag": "job:" + id,
			"original":  []any{map[string]any{"source": "/volumes/app_data", "volume": "app_data"}},
			"overwrite": true, "replace": true, "confirmed_stops": []string{"app-web-1"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("the command made by hand: %d %v", status, out)
	}
	command := r.waitForStatus(out["id"].(string), schema.CommandFailed)
	if command.GetString("error") != "failed to prepare the command: the command is not the restore's" {
		t.Errorf("the command made by hand: %v", command.FieldsData())
	}
	if events := r.happened(); len(events) != 0 || len(r.ranJobs()) != jobsBefore {
		t.Errorf("the command made by hand ran: %v", events)
	}
	if got := readTree(t, r.docker.VolumeDir("app_data")); got["a.txt"] != "v2" {
		t.Errorf("the volume holds %v", got)
	}
}

// TestARestoreCannotStartWithoutAFreshPreview (#30, AC3): a restore starts
// from a preview that is ready, and at most 30 minutes old; one older
// expires, and its draft is discarded.
func TestARestoreCannotStartWithoutAFreshPreview(t *testing.T) {
	r := newRestoresHarness(t, func(engine *fakedocker.Engine) {
		engine.AddVolume("app_data")
		writeTree(t, engine.VolumeDir("app_data"), map[string]string{"a.txt": "v1"})
	})
	id := r.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data"})["id"].(string)
	r.runNow(id)
	snapshot := r.newestSnapshot(id, r.disk.Id)

	entered, release := r.gateJobs(isRestore, nil)
	status, out := r.send(http.MethodPost, "/api/pc/backup-jobs/"+id+"/restores", r.operator,
		map[string]any{"target": r.disk.Id, "snapshot": snapshot, "mode": "original", "replace": true})
	if status != http.StatusAccepted {
		t.Fatalf("the draft: %d %v", status, out)
	}
	draft := out["id"].(string)
	select {
	case <-entered:
	case <-time.After(waitLimit):
		t.Fatal("the preview did not start")
	}
	if status, out := r.start(draft, r.operator, true); status != http.StatusConflict || out["message"] != "The preview is not ready yet." {
		t.Errorf("a start during the preview: %d %v", status, out)
	}
	release()
	restore := r.waitRestore(draft, schema.RestorePreviewed)

	// half an hour later
	restore.Set("preview_at", time.Now().UTC().Add(-31*time.Minute))
	restore.Set("expires_at", time.Now().UTC().Add(-time.Minute))
	if err := r.app.Save(restore); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if status, out := r.start(draft, r.operator, true); status != http.StatusConflict || out["message"] != "The preview is older than 30 minutes: preview again." {
			t.Errorf("a start from an old preview: %d %v", status, out)
		}
	}
	if status := r.restore(draft).GetString("status"); status != schema.RestoreExpired {
		t.Errorf("the old draft is %s", status)
	}
	if realRestores(r.ranJobs()) != nil {
		t.Error("a restore ran")
	}
	if status, out := r.send(http.MethodDelete, "/api/pc/backup-restores/"+draft, r.operator, nil); status != http.StatusNoContent {
		t.Errorf("the draft's discard: %d %v", status, out)
	}
	if _, err := r.app.FindRecordById(schema.BackupRestores, draft); err == nil {
		t.Error("the draft is still there")
	}
}

// listings are the listing jobs of a kind, snapshots or ls, that ran.
func listings(jobs []fakedocker.Job, kind string) int {
	count := 0
	for _, job := range jobs {
		if len(job.Cmd) > 1 && job.Cmd[0] == "backup-tool" && job.Cmd[1] == kind {
			count++
		}
	}
	return count
}

// commandsOf are the commands of a type the panel keeps.
func (r *restoresHarness) commandsOf(commandType string) []*core.Record {
	r.t.Helper()
	found, err := r.app.FindRecordsByFilter(schema.Commands, "type={:type}", "", 0, 0, dbx.Params{"type": commandType})
	if err != nil {
		r.t.Fatal(err)
	}
	return found
}

// TestSnapshotsAreListedPerJobAndTargetAndCached (#30, S): everyone lists a
// job's snapshots in each of its targets, newest first, with the run that
// made each; the list is kept, read again on refresh and after a backup of
// the job, and its command goes once it is answered. Another job's
// snapshots in the same repository are not listed. Browsing, restoring and
// downloading take an operator, and an admin for a new folder or a
// download.
func TestSnapshotsAreListedPerJobAndTargetAndCached(t *testing.T) {
	r := newRestoresHarness(t, func(engine *fakedocker.Engine) {
		for _, name := range []string{"app_data", "other_data"} {
			engine.AddVolume(name)
			writeTree(t, engine.VolumeDir(name), map[string]string{"a.txt": name})
		}
	})
	b2 := r.addS3("b2", "b2-secret-key")
	id := r.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data", "targets": []string{r.disk.Id, b2.Id}})["id"].(string)
	other := r.addJob(map[string]any{"name": "other", "source_kind": "volume", "source": "other_data"})["id"].(string)
	first := r.runNow(id)
	writeTree(t, r.docker.VolumeDir("app_data"), map[string]string{"b.txt": "later"})
	second := r.runNow(id)
	r.runNow(other)
	runOf := map[string]string{}
	for _, run := range append(first, second...) {
		runOf[run.GetString("target")+"|"+run.GetString("snapshot_id")] = run.Id
	}

	status, out := r.snapshots(id, r.disk.Id, r.viewer, "")
	snapshots, _ := out["snapshots"].([]any)
	if status != http.StatusOK || out["total"] != float64(2) || len(snapshots) != 2 || out["cached"] != false || out["host"] != r.host.Id ||
		out["target"] != r.disk.Id || out["fetched_at"] == nil {
		t.Fatalf("the snapshots: %d %v", status, out)
	}
	ids := []string{}
	for i, raw := range snapshots {
		snapshot := raw.(map[string]any)
		snapshotID, _ := snapshot["id"].(string)
		ids = append(ids, snapshotID)
		run, _ := snapshot["run"].(map[string]any)
		if len(snapshotID) != 64 || snapshot["short_id"] != snapshotID[:8] || snapshot["hostname"] != "backup-node" ||
			fmt.Sprint(snapshot["paths"]) != "[/volumes/app_data]" || run["trigger"] != schema.RunManual ||
			run["id"] != runOf[r.disk.Id+"|"+snapshotID[:8]] && run["id"] != runOf[r.disk.Id+"|"+snapshotID] {
			t.Errorf("snapshot %d: %v (runs %v)", i, snapshot, runOf)
		}
	}
	if newer, older := snapshots[0].(map[string]any)["time"].(string), snapshots[1].(map[string]any)["time"].(string); newer < older {
		t.Errorf("not newest first: %s, then %s", newer, older)
	}
	r.waitFor("the listing's command to go", func() bool { return len(r.commandsOf("backup.snapshots")) == 0 })

	// kept, and read again on refresh
	listed := listings(r.ranJobs(), "snapshots")
	if status, out := r.snapshots(id, r.disk.Id, r.viewer, ""); status != http.StatusOK || out["cached"] != true || out["total"] != float64(2) {
		t.Errorf("the kept snapshots: %d %v", status, out)
	}
	if listings(r.ranJobs(), "snapshots") != listed {
		t.Error("a kept list was listed again")
	}
	if status, out := r.snapshots(id, r.disk.Id, r.viewer, "&refresh=1"); status != http.StatusOK || out["cached"] != false {
		t.Errorf("a refresh: %d %v", status, out)
	}
	if listings(r.ranJobs(), "snapshots") != listed+1 {
		t.Error("a refresh did not list again")
	}

	// each target lists its own
	status, out = r.snapshots(id, b2.Id, r.viewer, "")
	inB2, _ := out["snapshots"].([]any)
	if status != http.StatusOK || len(inB2) != 2 || slices.Contains(ids, inB2[0].(map[string]any)["id"].(string)) {
		t.Errorf("the snapshots in b2: %d %v", status, out)
	}

	// a backup of the job drops the list
	r.runNow(id)
	if status, out := r.snapshots(id, r.disk.Id, r.viewer, ""); status != http.StatusOK || out["cached"] != false || out["total"] != float64(3) {
		t.Errorf("after a backup: %d %v", status, out)
	}
	if status, out := r.snapshots(other, r.disk.Id, r.viewer, ""); status != http.StatusOK || out["total"] != float64(1) {
		t.Errorf("the other job's snapshots: %d %v", status, out)
	}
	if status, out := r.send(http.MethodGet, "/api/pc/backup-jobs/"+other+"/snapshots?target="+b2.Id, r.viewer, nil); status != http.StatusBadRequest ||
		out["message"] != "The job never backed up to that target." {
		t.Errorf("a target the job never used: %d %v", status, out)
	}

	// who may do what
	snapshot := ids[0]
	files := "/api/pc/backup-jobs/" + id + "/snapshots/" + snapshot + "/files?target=" + r.disk.Id + "&path=" + encode("/volumes/app_data")
	if status, _ := r.send(http.MethodGet, files, r.viewer, nil); status != http.StatusForbidden {
		t.Errorf("a viewer browses: %d", status)
	}
	if status, _ := r.listing(files, r.operator); status != http.StatusOK {
		t.Errorf("an operator browses: %d", status)
	}
	original := map[string]any{"target": r.disk.Id, "snapshot": snapshot, "mode": "original"}
	if status, _ := r.send(http.MethodPost, "/api/pc/backup-jobs/"+id+"/restores", r.viewer, original); status != http.StatusForbidden {
		t.Errorf("a viewer restores: %d", status)
	}
	download := map[string]any{"target": r.disk.Id, "snapshot": snapshot, "path": "/volumes/app_data", "kind": "dir"}
	if status, _ := r.send(http.MethodPost, "/api/pc/backup-jobs/"+id+"/downloads", r.operator, download); status != http.StatusForbidden {
		t.Errorf("an operator downloads: %d", status)
	}
	if status, out := r.send(http.MethodPost, "/api/pc/backup-jobs/"+id+"/downloads", r.admin, download); status != http.StatusCreated {
		t.Errorf("an admin downloads: %d %v", status, out)
	}
}

// TestBrowsingPagesAFolderAndDeletesItsCommand (#30, S/SAFE): an operator
// lists a folder of a snapshot a page at a time, its folders first, then by
// name, filtered by a part of a name; a page once listed is kept, and the
// listing's command, which viewers could read, goes once it is answered.
func TestBrowsingPagesAFolderAndDeletesItsCommand(t *testing.T) {
	r := newRestoresHarness(t, func(engine *fakedocker.Engine) {
		engine.AddVolume("app_data")
		writeTree(t, engine.VolumeDir("app_data"), map[string]string{
			"c.txt": "c", "a.txt": "a", "B.txt": "bb", "docs/readme.md": "# app", "zdir/x": "x",
		})
	})
	id := r.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data"})["id"].(string)
	r.runNow(id)
	snapshot := r.newestSnapshot(id, r.disk.Id)
	page := func(path, query string) (int, map[string]any) {
		return r.listing("/api/pc/backup-jobs/"+id+"/snapshots/"+snapshot+"/files?target="+r.disk.Id+"&path="+encode(path)+query, r.operator)
	}
	entries := func(out map[string]any) []string {
		found := []string{}
		list, _ := out["entries"].([]any)
		for _, entry := range list {
			found = append(found, fmt.Sprint(entry.(map[string]any)["name"]))
		}
		return found
	}

	status, out := page("/volumes/app_data", "&limit=2")
	if status != http.StatusOK || out["total"] != float64(5) || out["dirs"] != float64(2) || out["files"] != float64(3) || out["offset"] != float64(0) ||
		out["path"] != "/volumes/app_data" || !slices.Equal(entries(out), []string{"docs", "zdir"}) {
		t.Fatalf("the first page: %d %v", status, out)
	}
	if first := out["entries"].([]any)[0].(map[string]any); first["type"] != "dir" || first["bad_name"] == true {
		t.Errorf("a folder: %v", first)
	}
	status, out = page("/volumes/app_data", "&limit=2&offset=2")
	if status != http.StatusOK || !slices.Equal(entries(out), []string{"B.txt", "a.txt"}) && !slices.Equal(entries(out), []string{"a.txt", "B.txt"}) {
		t.Errorf("the second page: %d %v", status, out)
	}
	if status, out := page("/volumes/app_data", "&match=b"); status != http.StatusOK || !slices.Equal(entries(out), []string{"B.txt"}) || out["total"] != float64(1) {
		t.Errorf("a filter: %d %v", status, out)
	}
	status, out = page("/volumes/app_data/docs", "")
	if status != http.StatusOK || !slices.Equal(entries(out), []string{"readme.md"}) {
		t.Fatalf("a folder's page: %d %v", status, out)
	}
	if readme := out["entries"].([]any)[0].(map[string]any); readme["type"] != "file" || readme["size"] != float64(5) {
		t.Errorf("a file: %v", readme)
	}
	r.waitFor("the listings' commands to go", func() bool { return len(r.commandsOf("backup.browse")) == 0 })

	listed := listings(r.ranJobs(), "ls")
	if status, out := page("/volumes/app_data/docs", ""); status != http.StatusOK || !slices.Equal(entries(out), []string{"readme.md"}) {
		t.Errorf("a kept page: %d %v", status, out)
	}
	if listings(r.ranJobs(), "ls") != listed {
		t.Error("a kept page was listed again")
	}
	if status, out := r.send(http.MethodGet, "/api/pc/backup-jobs/"+id+"/snapshots/"+snapshot+"/files?target="+r.disk.Id+"&path=docs", r.operator, nil); status != http.StatusBadRequest ||
		out["message"] != "Give an absolute path in the snapshot." {
		t.Errorf("a relative path: %d %v", status, out)
	}
	if status, out := r.send(http.MethodGet, "/api/pc/backup-jobs/"+id+"/snapshots/abc/files?target="+r.disk.Id+"&path=/", r.operator, nil); status != http.StatusBadRequest ||
		out["message"] != "Choose a snapshot." {
		t.Errorf("a snapshot that is not one: %d %v", status, out)
	}
}

// download starts a download as the admin and says its record and link.
func (r *restoresHarness) download(jobID string, body map[string]any) (string, string) {
	r.t.Helper()
	status, out := r.send(http.MethodPost, "/api/pc/backup-jobs/"+jobID+"/downloads", r.admin, body)
	restore, _ := out["restore"].(map[string]any)
	link, _ := out["url"].(string)
	if status != http.StatusCreated || restore["status"] != schema.RestoreQueued || !strings.HasPrefix(link, "/api/pc/backup-downloads/") {
		r.t.Fatalf("the download: %d %v", status, out)
	}
	return restore["id"].(string), link
}

// untar reads the files of a tar: each name and its content.
func untar(t *testing.T, data []byte) map[string]string {
	t.Helper()
	files := map[string]string{}
	reader := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatalf("the tar: %v", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = string(content)
	}
}

// TestADownloadStreamsTheTarToTheTicketsHolder (#30, S/SAFE): an admin's
// download link gives the browser a snapshot's folder as a tar, as restic
// dumps it, or a file as it is; a link works once.
func TestADownloadStreamsTheTarToTheTicketsHolder(t *testing.T) {
	r := newRestoresHarness(t, func(engine *fakedocker.Engine) {
		engine.AddVolume("app_data")
		writeTree(t, engine.VolumeDir("app_data"), map[string]string{"a.txt": "v1", "docs/readme.md": "# app"})
	})
	id := r.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data"})["id"].(string)
	r.runNow(id)
	snapshot := r.newestSnapshot(id, r.disk.Id)
	server := httptest.NewServer(r.handler)
	t.Cleanup(server.Close)

	restoreID, link := r.download(id, map[string]any{"target": r.disk.Id, "snapshot": snapshot, "path": "/volumes/app_data", "kind": "dir"})
	response, err := http.Get(server.URL + link) //nolint:noctx // a browser's navigation
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/x-tar" ||
		!strings.HasPrefix(response.Header.Get("Content-Disposition"), `attachment; filename="app-`+snapshot[:8]+"-") ||
		response.Header.Get("Cache-Control") != "no-store" || response.ContentLength != -1 {
		t.Fatalf("the download: %v %d %v", err, response.StatusCode, response.Header)
	}
	if files := untar(t, data); !maps.Equal(files, map[string]string{"volumes/app_data/a.txt": "v1", "volumes/app_data/docs/readme.md": "# app"}) {
		t.Errorf("the tar holds %v", files)
	}
	restore := r.waitRestore(restoreID, schema.RestoreSucceeded, schema.RestoreFailed, schema.RestoreCancelled)
	if restore.GetString("status") != schema.RestoreSucceeded || restore.GetInt("bytes") != len(data) {
		t.Errorf("the download's record: %v (%d bytes)", restore.FieldsData(), len(data))
	}
	if entry, err := r.app.FindFirstRecordByFilter(schema.AuditLog, "action='backup_download'"); err != nil || entry.GetString("actor_label") != "admin@example.com" {
		t.Errorf("the download's audit: %v %v", entry, err)
	}

	// a link works once
	response, err = http.Get(server.URL + link) //nolint:noctx // a browser's navigation
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "This download link has been used or has expired: start the download again.") {
		t.Errorf("the link used again: %d %s", response.StatusCode, body)
	}
	if len(r.commandsOf("backup.dump")) != 1 {
		t.Error("the link used again dumped again")
	}

	// a file comes as it is
	_, link = r.download(id, map[string]any{"target": r.disk.Id, "snapshot": snapshot, "path": "/volumes/app_data/docs/readme.md", "kind": "file"})
	response, err = http.Get(server.URL + link) //nolint:noctx // a browser's navigation
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "# app" || response.Header.Get("Content-Type") != "application/octet-stream" ||
		!strings.Contains(response.Header.Get("Content-Disposition"), `filename="readme.md"`) {
		t.Errorf("a file's download: %v %d %v %q", err, response.StatusCode, response.Header, body)
	}
}

// slowDownload is a job with a volume large enough that a slow restic takes
// a while to dump it, and its snapshot.
func slowDownload(t *testing.T) (*restoresHarness, string, string) {
	t.Helper()
	r := newRestoresHarness(t, func(engine *fakedocker.Engine) {
		engine.AddVolume("app_data")
		writeTree(t, engine.VolumeDir("app_data"), map[string]string{"big.bin": strings.Repeat("0123456789abcdef", 64<<10)})
	})
	id := r.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data"})["id"].(string)
	r.runNow(id)
	return r, id, r.newestSnapshot(id, r.disk.Id)
}

// TestADownloadStopsWhenTheBrowserLeaves (#30, SAFE): a browser that leaves
// a download stops the agent's job, and the download is cancelled.
func TestADownloadStopsWhenTheBrowserLeaves(t *testing.T) {
	r, id, snapshot := slowDownload(t)
	server := httptest.NewServer(r.handler)
	t.Cleanup(server.Close)
	t.Setenv("FAKE_RESTIC_SLOW_MS", "300")
	restoreID, link := r.download(id, map[string]any{"target": r.disk.Id, "snapshot": snapshot, "path": "/volumes/app_data", "kind": "dir"})

	ctx, leave := context.WithCancel(context.Background())
	defer leave()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+link, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(response.Body, make([]byte, 1024)); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("the download's first bytes: %v %d", err, response.StatusCode)
	}
	command := r.restore(restoreID).GetString("command")
	leave()
	_ = response.Body.Close()

	restore := r.waitRestore(restoreID, schema.RestoreSucceeded, schema.RestoreFailed, schema.RestoreCancelled)
	if restore.GetString("status") != schema.RestoreCancelled || restore.GetString("error") != "The browser stopped the download." {
		t.Errorf("the download the browser left: %v", restore.FieldsData())
	}
	r.waitForStatus(command, schema.CommandCancelled, schema.CommandFailed)
	r.waitFor("the dump's job to go", func() bool {
		for _, name := range r.docker.Names() {
			if container := r.docker.Find(name); container != nil && container.Labels["pocketcloud.command"] == command {
				return false
			}
		}
		return true
	})
}

// TestAFailedDownloadAbortsTheResponse (#30, SAFE): a dump that fails after
// its first bytes cuts the browser's download, rather than end it as a tar
// that looks whole, and the download fails.
func TestAFailedDownloadAbortsTheResponse(t *testing.T) {
	r, id, snapshot := slowDownload(t)
	server := httptest.NewServer(r.handler)
	t.Cleanup(server.Close)
	t.Setenv("FAKE_RESTIC_DUMP_FAIL", "100000")
	restoreID, link := r.download(id, map[string]any{"target": r.disk.Id, "snapshot": snapshot, "path": "/volumes/app_data", "kind": "dir"})

	response, err := http.Get(server.URL + link) //nolint:noctx // a browser's navigation
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !errors.Is(err, io.ErrUnexpectedEOF) || len(data) == 0 || len(data) > 100000 {
		t.Errorf("the failed download: %d, %d bytes, %v", response.StatusCode, len(data), err)
	}
	if restore := r.waitRestore(restoreID, schema.RestoreSucceeded, schema.RestoreFailed, schema.RestoreCancelled); restore.GetString("status") != schema.RestoreFailed {
		t.Errorf("the failed download's record: %v", restore.FieldsData())
	}
}

// TestSnapshotsReadFromTheJobsHostWhenTheTargetsHostIsOffline (#30): a
// remote target's snapshots are listed by its own server, or, while that
// one is offline, by the server of the job, which holds the target's
// secrets already as it backs up to it; with both offline, by none.
func TestSnapshotsReadFromTheJobsHostWhenTheTargetsHostIsOffline(t *testing.T) {
	r := newRestoresHarness(t, func(engine *fakedocker.Engine) {
		engine.AddVolume("app_data")
		writeTree(t, engine.VolumeDir("app_data"), map[string]string{"a.txt": "v1"})
	})
	nodeB := r.enrolledOn("node-b", nil)
	body := s3Target("b2", "b2-secret-key")
	body["host"] = nodeB.host.Id
	if status, out := r.send(http.MethodPost, "/api/pc/backup-targets", r.admin, body); status != http.StatusCreated {
		t.Fatalf("the target on node-b: %d %v", status, out)
	}
	r.checksDone()
	b2 := r.targetNamed("b2")
	id := r.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data", "targets": []string{b2.Id}})["id"].(string)
	if run := r.runNow(id)[0]; run.GetString("status") != schema.RunSucceeded {
		t.Fatalf("the backup: %v", run.FieldsData())
	}

	if status, out := r.snapshots(id, b2.Id, r.viewer, ""); status != http.StatusOK || out["host"] != nodeB.host.Id || out["total"] != float64(1) {
		t.Errorf("the snapshots from node-b: %d %v", status, out)
	}
	if err := nodeB.agent.stop(); err != nil {
		t.Fatal(err)
	}
	r.waitFor("node-b to be offline", func() bool { return !r.panel.Commands.Connected(nodeB.host.Id) })
	listed := listings(r.ranJobs(), "snapshots")
	if status, out := r.snapshots(id, b2.Id, r.viewer, "&refresh=1"); status != http.StatusOK || out["host"] != r.host.Id || out["total"] != float64(1) {
		t.Errorf("the snapshots from the job's server: %d %v", status, out)
	}
	if listings(r.ranJobs(), "snapshots") != listed+1 {
		t.Error("the job's server listed nothing")
	}
	if err := r.agent.stop(); err != nil {
		t.Fatal(err)
	}
	r.waitFor("backup-node to be offline", func() bool { return !r.panel.Commands.Connected(r.host.Id) })
	if status, out := r.send(http.MethodGet, "/api/pc/backup-jobs/"+id+"/snapshots?target="+b2.Id+"&refresh=1", r.viewer, nil); status != http.StatusConflict ||
		out["message"] != "Neither node-b nor backup-node is online: one of their agents lists the snapshots." {
		t.Errorf("both offline: %d %v", status, out)
	}
}

// TestARestoreWritesOnlyIntoFoldersItMay (#30, SAFE): the panel refuses a
// new folder that is the system's, holds the system's, or is a local
// target's repository; a folder that passes, but leads through a link the
// panel cannot see into the system's, is refused by the agent once it
// followed the link, and nothing is written there.
func TestARestoreWritesOnlyIntoFoldersItMay(t *testing.T) {
	r := newRestoresHarness(t, func(engine *fakedocker.Engine) {
		engine.AddVolume("app_data")
		writeTree(t, engine.VolumeDir("app_data"), map[string]string{"a.txt": "v1"})
	})
	id := r.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data"})["id"].(string)
	r.runNow(id)
	snapshot := r.newestSnapshot(id, r.disk.Id)
	body := func(path string) map[string]any {
		return map[string]any{"target": r.disk.Id, "snapshot": snapshot, "mode": "path", "host": r.host.Id, "path": path}
	}
	for _, path := range []string{"/", "/etc", "/etc/nginx", "/var", "/var/lib", "/var/lib/docker/volumes/x", "/srv/../etc", "srv/x", "/repo/x"} {
		if status, out := r.send(http.MethodPost, "/api/pc/backup-jobs/"+id+"/restores", r.admin, body(path)); status != http.StatusBadRequest || out["message"] == "" {
			t.Errorf("a restore into %s: %d %v", path, status, out)
		}
	}
	if status, out := r.send(http.MethodPost, "/api/pc/backup-jobs/"+id+"/restores", r.admin, body(r.diskDir+"/x")); status != http.StatusBadRequest ||
		out["message"] != r.diskDir+" is the repository of the target disk: restore elsewhere." {
		t.Errorf("a restore into the local target: %d %v", status, out)
	}
	if restores, err := r.app.FindAllRecords(schema.BackupRestores); err != nil || len(restores) != 0 {
		t.Errorf("refused restores were kept: %v %v", restores, err)
	}

	// /srv/evil leads to /etc on the server
	if err := os.MkdirAll(filepath.Join(r.hostRoot, "srv"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(r.hostRoot, "srv", "evil")); err != nil {
		t.Fatal(err)
	}
	before := readTree(t, filepath.Join(r.hostRoot, "etc"))
	draft := r.preview(id, r.admin, body("/srv/evil"))
	if draft.GetString("status") != schema.RestoreFailed || !strings.HasPrefix(draft.GetString("error"), "/srv/evil leads to /etc on this server: ") {
		t.Errorf("a restore into a link to /etc: %v", draft.FieldsData())
	}
	if after := readTree(t, filepath.Join(r.hostRoot, "etc")); !maps.Equal(after, before) {
		t.Errorf("/etc holds %v", after)
	}
	if jobs := r.ranJobs(); slices.ContainsFunc(jobs, isRestore) {
		t.Error("a restore's job ran")
	}
}
