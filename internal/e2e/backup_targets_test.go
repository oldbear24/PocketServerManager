package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"golang.org/x/crypto/ssh"

	"github.com/oldbear24/PocketServerManager/internal/agent"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

// backupHarness is a panel and an agent whose engine runs backup jobs with the
// backup tool and a fake restic, as the job container would.
type backupHarness struct {
	*harness
	admin   string
	adminID string
	host    *core.Record
	agent   *runningAgent
	restic  string

	mu   sync.Mutex
	jobs []fakedocker.Job
	// finished holds the checks whose finishers ran
	finished sync.Map
}

func newBackupHarness(t *testing.T) *backupHarness {
	t.Helper()
	b := &backupHarness{harness: newHarness(t)}
	b.restic = fakerestic.Install(t)
	b.docker.AddImage("sha256:"+strings.Repeat("a", 64), nil)
	b.docker.AddContainer("pocketcloud-agent", "ghcr.io/oldbear24/pocketcloud-agent:dev", map[string]string{"pocketcloud.role": "agent"})
	fakerestic.RunJobs(b.docker, agent.BackupTool)
	run := b.docker.OnJob
	b.docker.OnJob = func(job fakedocker.Job) fakedocker.JobResult {
		b.mu.Lock()
		b.jobs = append(b.jobs, job)
		b.mu.Unlock()
		return run(job)
	}
	// after the panel's own finisher, which keeps what a check found
	b.panel.Commands.OnFinish("backup.init", func(command *core.Record) { b.finished.Store(command.Id, true) })
	b.agent, b.host = b.enrolled("backup-node")
	var admin *core.Record
	admin, b.admin = testutil.NewUser(t, b.app, "admin@example.com", schema.RoleAdmin)
	b.adminID = admin.Id
	return b
}

func (b *backupHarness) send(method, url, token string, body any) (int, map[string]any) {
	b.t.Helper()
	raw := ""
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			b.t.Fatal(err)
		}
		raw = string(encoded)
	}
	status, out := testutil.Do(b.t, b.handler, testutil.Request{Method: method, URL: url, Token: token, Body: raw})
	answer := map[string]any{}
	_ = json.Unmarshal([]byte(out), &answer)
	answer["raw"] = out
	return status, answer
}

// checksDone waits until every check of a target has ended and its finisher
// kept what it found: the measures that follow a check run without anyone
// waiting, and a command is final a moment before its finisher runs.
func (b *backupHarness) checksDone() {
	b.t.Helper()
	b.waitFor("the checks to end", func() bool {
		checks, err := b.app.FindRecordsByFilter(schema.Commands, "type='backup.init'", "", 0, 0)
		if err != nil {
			return false
		}
		for _, check := range checks {
			if _, kept := b.finished.Load(check.Id); !kept {
				return false
			}
		}
		return true
	})
}

// gateJobs holds the jobs that match until release is called, and tells on
// entered when one waits.
func (b *backupHarness) gateJobs(match func(fakedocker.Job) bool, result func(fakedocker.JobResult) fakedocker.JobResult) (entered <-chan struct{}, release func()) {
	b.t.Helper()
	in := make(chan struct{}, 8)
	gate := make(chan struct{})
	var once sync.Once
	run := b.docker.OnJob
	b.docker.OnJob = func(job fakedocker.Job) fakedocker.JobResult {
		if !match(job) {
			return run(job)
		}
		in <- struct{}{}
		<-gate
		got := run(job)
		if result != nil {
			got = result(got)
		}
		return got
	}
	release = func() { once.Do(func() { close(gate) }) }
	b.t.Cleanup(release)
	return in, release
}

func (b *backupHarness) ranJobs() []fakedocker.Job {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.jobs)
}

func (b *backupHarness) targets() []*core.Record {
	b.t.Helper()
	targets, err := b.app.FindAllRecords(schema.BackupTargets)
	if err != nil {
		b.t.Fatal(err)
	}
	return targets
}

func s3Target(name, secret string) map[string]any {
	return map[string]any{
		"name": name, "kind": "s3",
		"config":      map[string]any{"endpoint": "http://minio:9000", "bucket": "backups", "prefix": name, "path_style": true},
		"credentials": map[string]any{"access_key_id": "minio-access-" + name, "secret_access_key": secret},
	}
}

// sftpKeys makes an SFTP user's private key and a server's host key.
func sftpKeys(t *testing.T) (private, hostKey string) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	server, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, err := ssh.NewPublicKey(server)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block)), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public)))
}

// TestAddingABackupTargetChecksItWithRestic (#28): a target is saved only once
// the agent of its server opened its repository, or created it; storage that
// refuses the credentials is refused with restic's words, and nothing is kept.
func TestAddingABackupTargetChecksItWithRestic(t *testing.T) {
	b := newBackupHarness(t)
	body := s3Target("b2", "minio-secret-b2")
	body["host"] = b.host.Id
	body["immutable_days"] = 30
	body["note"] = "eu-central"
	status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, body)
	if status != http.StatusCreated {
		t.Fatalf("add: %d %v", status, out)
	}
	if out["status"] != schema.TargetOK || len(out["repo_id"].(string)) != 64 || out["location"] != "s3:backups/b2 @ minio:9000" ||
		out["immutable_days"] != float64(30) || out["credentials"] != nil || out["repo_password"] != nil {
		t.Errorf("the target: %v", out)
	}
	if fakerestic.Repositories(t, b.restic) != 1 || strings.Count(fakerestic.Calls(t, b.restic), " init --json\n") != 1 {
		t.Errorf("restic ran:\n%s", fakerestic.Calls(t, b.restic))
	}
	stored := b.targets()[0]
	if !secrets.IsSealed(stored.GetString("credentials")) || !secrets.IsSealed(stored.GetString("repo_password")) {
		t.Error("the secrets are not sealed")
	}

	// credentials the storage refuses
	wrong := "minio-" + fakerestic.Wrong + "-secret"
	refused := s3Target("refused", wrong)
	refused["host"] = b.host.Id
	status, out = b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, refused)
	message, _ := out["message"].(string)
	if status != http.StatusBadRequest || !strings.Contains(message, "Access Denied for key ***") || strings.Contains(out["raw"].(string), wrong) {
		t.Errorf("refused credentials: %d %v", status, out)
	}
	if len(b.targets()) != 1 || fakerestic.Repositories(t, b.restic) != 1 {
		t.Errorf("a refused target was kept: %d targets", len(b.targets()))
	}

	// a name or a place taken, settings that are not a target's
	again := s3Target("b2", "x-secret")
	again["host"] = b.host.Id
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, again); status != http.StatusBadRequest || !strings.Contains(out["message"].(string), "exists already") {
		t.Errorf("the same name: %d %v", status, out)
	}
	again["name"] = "b2-again"
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, again); status != http.StatusBadRequest || !strings.Contains(out["message"].(string), "is there already") {
		t.Errorf("the same place: %d %v", status, out)
	}
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, map[string]any{
		"name": "etc", "kind": "local", "host": b.host.Id, "config": map[string]any{"path": "/etc/backups"},
	}); status != http.StatusBadRequest || !strings.Contains(out["message"].(string), "/etc is the system's") {
		t.Errorf("a system directory: %d %v", status, out)
	}

	// a local target: a directory the Engine makes, which says its room
	dir := filepath.Join(t.TempDir(), "srv", "restic")
	status, local := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, map[string]any{
		"name": "disk", "kind": "local", "host": b.host.Id, "config": map[string]any{"path": dir},
	})
	if status != http.StatusCreated || local["capacity_bytes"].(float64) <= 0 || local["location"] != dir {
		t.Fatalf("a local target: %d %v", status, local)
	}
	if _, err := os.Stat(filepath.Join(dir, "config")); err != nil {
		t.Errorf("the local repository: %v", err)
	}

	// an existing repository is added with its password
	existing := s3Target("old", "old-secret")
	existing["host"] = b.host.Id
	existing["password"] = "the-old-password"
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, existing); status != http.StatusCreated {
		t.Fatalf("a repository with its password: %d %v", status, out)
	}
	if status, out := b.send(http.MethodDelete, "/api/pc/backup-targets/"+b.targetNamed("old").Id, b.admin, nil); status != http.StatusNoContent {
		t.Fatalf("remove: %d %v", status, out)
	}
	delete(existing, "password")
	status, out = b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, existing)
	if status != http.StatusBadRequest || !strings.Contains(out["message"].(string), "the password is not its own") {
		t.Errorf("an existing repository without its password: %d %v", status, out)
	}
	existing["password"] = "the-old-password"
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, existing); status != http.StatusCreated || out["repo_id"] == "" {
		t.Errorf("the repository again, with its password: %d %v", status, out)
	}

	// viewers read the targets, without their secrets, and change none
	_, viewer := testutil.NewUser(t, b.app, "viewer@example.com", schema.RoleViewer)
	_, operator := testutil.NewUser(t, b.app, "operator@example.com", schema.RoleOperator)
	status, listed := b.send(http.MethodGet, "/api/collections/backup_targets/records", viewer, nil)
	if status != http.StatusOK || listed["totalItems"] != float64(3) || strings.Contains(listed["raw"].(string), "repo_password") ||
		strings.Contains(listed["raw"].(string), "credentials") {
		t.Errorf("a viewer's list: %d %v", status, listed)
	}
	for _, token := range []string{viewer, operator} {
		if status, _ := b.send(http.MethodPost, "/api/pc/backup-targets", token, body); status != http.StatusForbidden {
			t.Errorf("a non-admin adds a target: %d", status)
		}
		if status, _ := b.send(http.MethodPost, "/api/pc/backup-targets/"+stored.Id+"/refresh", token, nil); status != http.StatusForbidden {
			t.Errorf("a non-admin refreshes a target: %d", status)
		}
	}
}

func (b *backupHarness) targetNamed(name string) *core.Record {
	b.t.Helper()
	target, err := b.app.FindFirstRecordByData(schema.BackupTargets, "name", name)
	if err != nil {
		b.t.Fatalf("the target %s: %v", name, err)
	}
	return target
}

// TestABackupTargetsSecretsLeaveThePanelOnlyInItsCommand (#28): the commands
// the panel stores, which viewers read, the API's answers and the audit log
// hold no secret of a target; the job of the agent that runs restic gets them.
func TestABackupTargetsSecretsLeaveThePanelOnlyInItsCommand(t *testing.T) {
	b := newBackupHarness(t)
	private, hostKey := sftpKeys(t)
	added := map[string]map[string]any{
		"b2": s3Target("b2", "minio-secret-key-b2"),
		"node": {"name": "node", "kind": "sftp", "config": map[string]any{"address": "backup-node:2222", "user": "backup", "path": "/srv/restic", "host_key": hostKey},
			"credentials": map[string]any{"private_key": private}},
		"rest": {"name": "rest", "kind": "rest", "config": map[string]any{"url": "https://backup-node:8000/homelab/"},
			"credentials": map[string]any{"username": "homelab", "password": "rest-server-password"}},
	}
	var answers []string
	for _, name := range []string{"b2", "node", "rest"} {
		body := added[name]
		body["host"] = b.host.Id
		status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, body)
		if status != http.StatusCreated {
			t.Fatalf("add %s: %d %v", name, status, out)
		}
		answers = append(answers, out["raw"].(string))
	}
	// a refresh and new credentials go the same way
	b2 := b.targetNamed("b2")
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets/"+b2.Id+"/refresh", b.admin, nil); status != http.StatusOK || out["status"] != schema.TargetOK {
		t.Fatalf("refresh: %d %v", status, out)
	}
	status, out := b.send(http.MethodPatch, "/api/pc/backup-targets/"+b2.Id, b.admin, map[string]any{
		"credentials": map[string]any{"access_key_id": "minio-access-b2", "secret_access_key": "minio-secret-key-b2-rotated"},
	})
	if status != http.StatusOK {
		t.Fatalf("new credentials: %d %v", status, out)
	}
	answers = append(answers, out["raw"].(string))

	var passwords []string
	for _, target := range b.targets() {
		password, err := b.panel.Box.OpenString(target.GetString("repo_password"))
		if err != nil || len(password) < 32 {
			t.Fatalf("the password of %s: %v", target.GetString("name"), err)
		}
		passwords = append(passwords, password)
	}
	keyLine := strings.Split(private, "\n")[1]
	forbidden := append([]string{"minio-secret-key-b2", "rest-server-password", keyLine, "PRIVATE KEY"}, passwords...)

	// the commands, as the panel keeps them and viewers read them: three
	// adds, a check and its measure, a change
	b.checksDone()
	inits, err := b.app.FindRecordsByFilter(schema.Commands, "type='backup.init'", "created", 0, 0)
	if err != nil || len(inits) != 6 {
		t.Fatalf("%d checks: %v", len(inits), err)
	}
	_, viewer := testutil.NewUser(t, b.app, "viewer@example.com", schema.RoleViewer)
	_, listed := b.send(http.MethodGet, "/api/collections/commands/records?perPage=100", viewer, nil)
	audit, err := b.app.FindAllRecords("audit_log")
	if err != nil {
		t.Fatal(err)
	}
	kept := []string{listed["raw"].(string)}
	kept = append(kept, answers...)
	for _, command := range inits {
		raw, _ := json.Marshal(command.FieldsData())
		kept = append(kept, string(raw))
		payload, _ := json.Marshal(command.Get("payload"))
		if !strings.Contains(string(payload), `"target_id"`) || !strings.Contains(string(payload), `"url"`) {
			t.Errorf("the stored payload names no target: %s", payload)
		}
	}
	for _, entry := range audit {
		raw, _ := json.Marshal(entry.FieldsData())
		kept = append(kept, string(raw))
	}
	for _, text := range kept {
		for _, secret := range forbidden {
			if strings.Contains(text, secret) {
				t.Errorf("a secret is kept: %.20s… in %.300s", secret, text)
			}
		}
	}

	// the agent's jobs got them
	jobs := b.ranJobs()
	if len(jobs) != 6 {
		t.Fatalf("%d jobs ran", len(jobs))
	}
	var env, files string
	for _, job := range jobs {
		env += strings.Join(job.Env, "\n") + "\n"
		for name, content := range job.Files {
			files += name + "=" + string(content) + "\n"
		}
	}
	for _, want := range []string{"AWS_SECRET_ACCESS_KEY=minio-secret-key-b2\n", "AWS_SECRET_ACCESS_KEY=minio-secret-key-b2-rotated\n",
		"RESTIC_REST_PASSWORD=rest-server-password\n", "RESTIC_REPOSITORY=s3:http://minio:9000/backups/b2\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("the jobs' environment lacks %s", want)
		}
	}
	for _, password := range passwords {
		if !strings.Contains(env, "RESTIC_PASSWORD="+password+"\n") {
			t.Error("a job lacks its repository's password")
		}
	}
	if !strings.Contains(files, "/run/pocketcloud/ssh-key="+private) || !strings.Contains(files, "/run/pocketcloud/known_hosts=[backup-node]:2222 "+hostKey) {
		t.Errorf("the SFTP job's files:\n%s", files)
	}
	// none of the jobs is left
	for _, name := range b.docker.Names() {
		if name != "pocketcloud-agent" {
			t.Errorf("a container is left: %s", name)
		}
	}
}

// TestNewCredentialsThatFailKeepTheOldOnes (#28): a change is checked with
// restic before it is kept; the target stays where it is.
func TestNewCredentialsThatFailKeepTheOldOnes(t *testing.T) {
	b := newBackupHarness(t)
	body := s3Target("b2", "good-secret")
	body["host"] = b.host.Id
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, body); status != http.StatusCreated {
		t.Fatalf("add: %d %v", status, out)
	}
	target := b.targetNamed("b2")
	wrong := "bad-" + fakerestic.Wrong + "-secret"
	status, out := b.send(http.MethodPatch, "/api/pc/backup-targets/"+target.Id, b.admin, map[string]any{
		"name": "renamed", "credentials": map[string]any{"access_key_id": "a", "secret_access_key": wrong},
	})
	if status != http.StatusBadRequest || strings.Contains(out["raw"].(string), wrong) {
		t.Errorf("credentials that fail: %d %v", status, out)
	}
	after := b.targetNamed("b2")
	if after.GetString("credentials") != target.GetString("credentials") {
		t.Error("credentials that failed were kept")
	}
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets/"+target.Id+"/refresh", b.admin, nil); status != http.StatusOK || out["status"] != schema.TargetOK {
		t.Errorf("the old credentials: %d %v", status, out)
	}

	// another place is another target
	if status, out := b.send(http.MethodPatch, "/api/pc/backup-targets/"+target.Id, b.admin, map[string]any{
		"config": map[string]any{"endpoint": "http://minio:9000", "bucket": "elsewhere"},
	}); status != http.StatusBadRequest || !strings.Contains(out["message"].(string), "stays where it is") {
		t.Errorf("another bucket: %d %v", status, out)
	}
	// what does not move it may change
	status, out = b.send(http.MethodPatch, "/api/pc/backup-targets/"+target.Id, b.admin, map[string]any{
		"name": "b2-eu", "note": "Object Lock on", "immutable_days": 7,
		"config": map[string]any{"endpoint": "http://minio:9000", "bucket": "backups", "prefix": "b2", "path_style": true, "region": "eu-central-003"},
	})
	if status != http.StatusOK || out["name"] != "b2-eu" || out["immutable_days"] != float64(7) {
		t.Errorf("a change: %d %v", status, out)
	}
	if !strings.Contains(fakerestic.Calls(t, b.restic), "AWS_DEFAULT_REGION=eu-central-003") {
		t.Error("the new region did not reach restic")
	}
	if status, _ := b.send(http.MethodPatch, "/api/pc/backup-targets/"+target.Id, b.admin, map[string]any{"name": "x", "kind": "sftp"}); status != http.StatusBadRequest {
		t.Errorf("another kind: %d", status)
	}
}

// TestTheHourlyCheckKeepsWhatATargetHolds (#28): the panel asks each target
// what it holds; a repository that is gone is the target's failure, not a
// reason to make a new one, and a check that works again clears it.
func TestTheHourlyCheckKeepsWhatATargetHolds(t *testing.T) {
	b := newBackupHarness(t)
	dir := filepath.Join(t.TempDir(), "restic")
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, map[string]any{
		"name": "disk", "kind": "local", "host": b.host.Id, "config": map[string]any{"path": dir},
	}); status != http.StatusCreated {
		t.Fatalf("add: %d %v", status, out)
	}
	t.Setenv("FAKE_RESTIC_SIZE", "123456")
	b.panel.CheckBackupTargets()
	b.waitFor("the target's usage", func() bool { return b.targetNamed("disk").GetInt("used_bytes") == 123456 })

	moved := dir + ".moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b.panel.CheckBackupTargets()
	b.waitFor("the target to fail", func() bool { return b.targetNamed("disk").GetString("status") == schema.TargetFailed })
	if target := b.targetNamed("disk"); !strings.Contains(target.GetString("last_error"), "The repository is gone, with its snapshots") {
		t.Errorf("the error: %s", target.GetString("last_error"))
	}
	if _, err := os.Stat(filepath.Join(dir, "config")); err == nil {
		t.Error("a new repository took the place of the one that is gone")
	}

	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, dir); err != nil {
		t.Fatal(err)
	}
	target := b.targetNamed("disk")
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets/"+target.Id+"/refresh", b.admin, nil); status != http.StatusOK ||
		out["status"] != schema.TargetOK || out["last_error"] != "" {
		t.Errorf("back: %d %v", status, out)
	}
}

// TestABackupTargetNeedsItsServerOnline (#28): the agent checks a target
// before it is saved; with its server offline, nothing is saved or queued.
func TestABackupTargetNeedsItsServerOnline(t *testing.T) {
	b := newBackupHarness(t)
	body := s3Target("b2", "a-secret")
	body["host"] = b.host.Id
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, body); status != http.StatusCreated {
		t.Fatalf("add: %d %v", status, out)
	}
	target := b.targetNamed("b2")
	if err := b.agent.stop(); err != nil {
		t.Fatal(err)
	}
	b.waitFor("the agent to be offline", func() bool { return !b.panel.Commands.Connected(b.host.Id) })
	before := b.commandCount(b.host.Id, "backup.init")

	other := s3Target("other", "a-secret")
	other["host"] = b.host.Id
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, other); status != http.StatusConflict || !strings.Contains(out["message"].(string), "offline") {
		t.Errorf("add offline: %d %v", status, out)
	}
	if status, _ := b.send(http.MethodPost, "/api/pc/backup-targets/"+target.Id+"/refresh", b.admin, nil); status != http.StatusConflict {
		t.Errorf("refresh offline: %d", status)
	}
	if len(b.targets()) != 1 || b.commandCount(b.host.Id, "backup.init") != before {
		t.Errorf("%d targets, %d checks queued", len(b.targets()), b.commandCount(b.host.Id, "backup.init")-before)
	}
	// the hourly check passes a server that is offline by
	b.panel.CheckBackupTargets()
	if b.commandCount(b.host.Id, "backup.init") != before {
		t.Error("the hourly check queued a check for an offline server")
	}
}

// TestATargetThatDidNotAnswerInTimeIsKept (review of #28): a target is kept
// with its password before restic runs, so a check that the panel stopped
// waiting for, which may have created the repository all the same, leaves a
// target to check again rather than a repository whose password is lost.
func TestATargetThatDidNotAnswerInTimeIsKept(t *testing.T) {
	b := newBackupHarness(t)
	b.panel.BackupCheckWait = 300 * time.Millisecond
	entered, release := b.gateJobs(func(fakedocker.Job) bool { return true }, nil)
	body := s3Target("slow", "slow-secret")
	body["host"] = b.host.Id
	status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, body)
	if status != http.StatusGatewayTimeout || !strings.Contains(out["message"].(string), "The target is kept, waiting") {
		t.Fatalf("a check that did not end in time: %d %v", status, out)
	}
	<-entered
	target := b.targetNamed("slow")
	if target.GetString("status") != schema.TargetPending || !strings.Contains(target.GetString("last_error"), "check it again") ||
		!secrets.IsSealed(target.GetString("repo_password")) {
		t.Errorf("the kept target: %v", target.PublicExport())
	}
	// the job went on and made the repository with the target's password
	release()
	b.waitFor("the repository", func() bool { return fakerestic.Repositories(t, b.restic) == 1 })
	b.checksDone()
	status, out = b.send(http.MethodPost, "/api/pc/backup-targets/"+target.Id+"/refresh", b.admin, nil)
	if status != http.StatusOK || out["status"] != schema.TargetOK || len(out["repo_id"].(string)) != 64 || out["last_error"] != "" {
		t.Errorf("checked again: %d %v", status, out)
	}
	if fakerestic.Repositories(t, b.restic) != 1 {
		t.Error("checking again made another repository")
	}
}

// TestABackupTargetIsNotBroughtBackByARevert (review of #28): one brought
// back would have neither its credentials nor its repository's password,
// and read as answering; one whose secrets cannot be opened reads as failed.
func TestABackupTargetIsNotBroughtBackByARevert(t *testing.T) {
	b := newBackupHarness(t)
	body := s3Target("b2", "a-secret")
	body["host"] = b.host.Id
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, body); status != http.StatusCreated {
		t.Fatalf("add: %d %v", status, out)
	}
	target := b.targetNamed("b2")
	if status, _ := b.send(http.MethodDelete, "/api/pc/backup-targets/"+target.Id, b.admin, nil); status != http.StatusNoContent {
		t.Fatalf("remove: %d", status)
	}
	entry, err := b.app.FindFirstRecordByFilter(schema.AuditLog, "collection={:c} && action='delete'", dbx.Params{"c": schema.BackupTargets})
	if err != nil {
		t.Fatal(err)
	}
	if status, out := b.send(http.MethodPost, "/api/pc/audit/"+entry.Id+"/revert", b.admin, nil); status != http.StatusBadRequest ||
		!strings.Contains(out["message"].(string), "cannot be reverted") {
		t.Errorf("revert: %d %v", status, out)
	}
	if len(b.targets()) != 0 {
		t.Error("the target was brought back")
	}

	// a target as a revert would have made it: stored without its secrets
	collection, err := b.app.FindCollectionByNameOrId(schema.BackupTargets)
	if err != nil {
		t.Fatal(err)
	}
	ghost := core.NewRecord(collection)
	ghost.Load(map[string]any{"name": "ghost", "kind": "local", "host": b.host.Id, "config": map[string]any{"path": "/srv/ghost"},
		"location": "/srv/ghost", "status": schema.TargetOK, "repo_id": strings.Repeat("c", 64)})
	if err := b.app.Save(ghost); err != nil {
		t.Fatal(err)
	}
	status, out := b.send(http.MethodPost, "/api/pc/backup-targets/"+ghost.Id+"/refresh", b.admin, nil)
	if status != http.StatusOK || out["status"] != schema.TargetFailed || !strings.Contains(out["last_error"].(string), "remove it and add it again") {
		t.Errorf("a target without its secrets: %d %v", status, out)
	}
}

// TestAServerThatReachesTargetsIsNotRemoved (review of #28): its targets
// would lose their only way to their repositories; the answer names them.
func TestAServerThatReachesTargetsIsNotRemoved(t *testing.T) {
	b := newBackupHarness(t)
	body := s3Target("b2", "a-secret")
	body["host"] = b.host.Id
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, body); status != http.StatusCreated {
		t.Fatalf("add: %d %v", status, out)
	}
	status, out := b.send(http.MethodDelete, "/api/collections/hosts/records/"+b.host.Id, b.admin, nil)
	if status != http.StatusBadRequest || !strings.Contains(out["message"].(string), "The server backup-node reaches the backup targets b2: move them to another server first") {
		t.Errorf("remove the server: %d %v", status, out)
	}
	if _, err := b.app.FindRecordById(schema.Hosts, b.host.Id); err != nil {
		t.Error("the server is gone")
	}
	if status, _ := b.send(http.MethodDelete, "/api/pc/backup-targets/"+b.targetNamed("b2").Id, b.admin, nil); status != http.StatusNoContent {
		t.Fatalf("remove the target: %d", status)
	}
	if status, out := b.send(http.MethodDelete, "/api/collections/hosts/records/"+b.host.Id, b.admin, nil); status != http.StatusNoContent {
		t.Errorf("remove the server without targets: %d %v", status, out)
	}
}

// TestTargetChangesAreAuditedAsTheAdmin (review of #28): who added a target
// with credentials, changed them or removed it, with its password.
func TestTargetChangesAreAuditedAsTheAdmin(t *testing.T) {
	b := newBackupHarness(t)
	body := s3Target("b2", "a-secret")
	body["host"] = b.host.Id
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, body); status != http.StatusCreated {
		t.Fatalf("add: %d %v", status, out)
	}
	id := b.targetNamed("b2").Id
	if status, out := b.send(http.MethodPatch, "/api/pc/backup-targets/"+id, b.admin, map[string]any{
		"note": "rotated", "credentials": map[string]any{"access_key_id": "minio-access-b2", "secret_access_key": "another-secret"},
	}); status != http.StatusOK {
		t.Fatalf("change: %d %v", status, out)
	}
	if status, _ := b.send(http.MethodDelete, "/api/pc/backup-targets/"+id, b.admin, nil); status != http.StatusNoContent {
		t.Fatalf("remove: %d", status)
	}
	entries, err := b.app.FindRecordsByFilter(schema.AuditLog, "collection={:c}", "seq", 0, 0, dbx.Params{"c": schema.BackupTargets})
	if err != nil || len(entries) < 3 {
		t.Fatalf("%d entries: %v", len(entries), err)
	}
	actions := map[string]bool{}
	for _, entry := range entries {
		actions[entry.GetString("action")] = true
		if entry.GetString("actor") != b.adminID || entry.GetString("actor_kind") != "user" {
			t.Errorf("%s by %s %s", entry.GetString("action"), entry.GetString("actor_kind"), entry.GetString("actor_label"))
		}
	}
	if !actions["create"] || !actions["update"] || !actions["delete"] {
		t.Errorf("actions: %v", actions)
	}
}

// TestAChangeMadeDuringACheckIsKept (review of #28): a change waits up to
// two minutes for its check; what another change stored meanwhile stays.
func TestAChangeMadeDuringACheckIsKept(t *testing.T) {
	b := newBackupHarness(t)
	body := s3Target("b2", "a-secret")
	body["host"] = b.host.Id
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, body); status != http.StatusCreated {
		t.Fatalf("add: %d %v", status, out)
	}
	b.checksDone()
	id := b.targetNamed("b2").Id
	entered, release := b.gateJobs(func(fakedocker.Job) bool { return true }, nil)
	done := make(chan int, 1)
	go func() {
		status, _ := b.send(http.MethodPatch, "/api/pc/backup-targets/"+id, b.admin, map[string]any{
			"note":        "A's note",
			"credentials": map[string]any{"access_key_id": "minio-access-b2", "secret_access_key": "rotated-secret"},
		})
		done <- status
	}()
	<-entered
	if status, out := b.send(http.MethodPatch, "/api/pc/backup-targets/"+id, b.admin, map[string]any{"name": "renamed"}); status != http.StatusOK {
		t.Fatalf("the other change: %d %v", status, out)
	}
	release()
	if status := <-done; status != http.StatusOK {
		t.Fatalf("the change with a check: %d", status)
	}
	target, err := b.app.FindRecordById(schema.BackupTargets, id)
	if err != nil {
		t.Fatal(err)
	}
	if target.GetString("name") != "renamed" || target.GetString("note") != "A's note" {
		t.Errorf("the other change was lost: %v", target.PublicExport())
	}
	secrets, err := b.panel.Box.OpenString(target.GetString("credentials"))
	if err != nil || !strings.Contains(secrets, "rotated-secret") {
		t.Errorf("the new credentials were not kept: %v", err)
	}
}

// TestAMeasureMadeWithOldCredentialsDoesNotUndoAChange (review of #28): a
// measure runs beside other checks; one completed with the credentials a
// change has since replaced says nothing of the target as it is now.
func TestAMeasureMadeWithOldCredentialsDoesNotUndoAChange(t *testing.T) {
	b := newBackupHarness(t)
	body := s3Target("b2", "old-secret")
	body["host"] = b.host.Id
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, body); status != http.StatusCreated {
		t.Fatalf("add: %d %v", status, out)
	}
	b.checksDone()
	id := b.targetNamed("b2").Id
	// the measure with the old credentials, which the storage refuses by then
	entered, release := b.gateJobs(func(job fakedocker.Job) bool { return slices.Contains(job.Cmd, "--usage") },
		func(fakedocker.JobResult) fakedocker.JobResult {
			return fakedocker.JobResult{Code: 1, Stderr: "error: Fatal: unable to open config file: Access Denied"}
		})
	b.panel.CheckBackupTargets()
	<-entered
	if status, out := b.send(http.MethodPatch, "/api/pc/backup-targets/"+id, b.admin, map[string]any{
		"credentials": map[string]any{"access_key_id": "minio-access-b2", "secret_access_key": "new-secret"},
	}); status != http.StatusOK || out["status"] != schema.TargetOK {
		t.Fatalf("new credentials: %d %v", status, out)
	}
	release()
	b.checksDone()
	if target, _ := b.app.FindRecordById(schema.BackupTargets, id); target.GetString("status") != schema.TargetOK {
		t.Errorf("the old measure undid the change: %s %s", target.GetString("status"), target.GetString("last_error"))
	}
}

// TestWhatATargetHoldsIsMeasuredAfterItOpens (review of #28): adding an
// existing repository only opens it; restic reads its snapshots afterwards,
// without anyone waiting, and the target says when.
func TestWhatATargetHoldsIsMeasuredAfterItOpens(t *testing.T) {
	b := newBackupHarness(t)
	body := s3Target("b2", "a-secret")
	body["host"] = b.host.Id
	body["password"] = "a-password-of-my-own"
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, body); status != http.StatusCreated || out["usage_at"] == "" {
		t.Fatalf("add: %d %v", status, out)
	}
	if status, _ := b.send(http.MethodDelete, "/api/pc/backup-targets/"+b.targetNamed("b2").Id, b.admin, nil); status != http.StatusNoContent {
		t.Fatalf("remove: %d", status)
	}
	b.checksDone()
	jobs := len(b.ranJobs())
	t.Setenv("FAKE_RESTIC_SIZE", "777")
	status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, body)
	if status != http.StatusCreated || out["status"] != schema.TargetOK {
		t.Fatalf("add it again: %d %v", status, out)
	}
	b.waitFor("the measure", func() bool {
		target := b.targetNamed("b2")
		return target.GetInt("used_bytes") == 777 && !target.GetDateTime("usage_at").IsZero()
	})
	ran := b.ranJobs()[jobs:]
	if len(ran) != 2 || slices.Contains(ran[0].Cmd, "--usage") || !slices.Contains(ran[1].Cmd, "--usage") {
		t.Errorf("the jobs: %v", ran)
	}
	// the measure opens the repository the check found, on a resource of its
	// own, so that it holds up no check
	target := b.targetNamed("b2")
	measures, err := b.app.FindRecordsByFilter(schema.Commands, "type='backup.init' && resource={:r}", "", 0, 0, dbx.Params{"r": "backup-usage:" + target.Id})
	if err != nil || len(measures) != 1 || !strings.Contains(string(mustJSON(measures[0].Get("payload"))), `"repo_id":"`+target.GetString("repo_id")+`"`) ||
		!slices.Contains(ran[1].Cmd, "--expect") {
		t.Errorf("the measure: %v %v", measures, err)
	}
}

// TestNotesAndNamesAreCountedInCharacters (review of #28): as the collection
// and the form count them, not in bytes, which Czech letters take two of.
func TestNotesAndNamesAreCountedInCharacters(t *testing.T) {
	b := newBackupHarness(t)
	body := s3Target("b2", "a-secret")
	body["host"] = b.host.Id
	body["name"] = strings.Repeat("ž", 90)
	body["note"] = strings.Repeat("ř", 450)
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, body); status != http.StatusCreated {
		t.Errorf("Czech: %d %v", status, out)
	}
	body["name"] = "other"
	body["note"] = strings.Repeat("ř", 501)
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, body); status != http.StatusBadRequest || !strings.Contains(out["message"].(string), "at most 500") {
		t.Errorf("too long: %d %v", status, out)
	}
}

// TestTwoChangesAtOnceKeepNeitherOver (second review of #28): a change is
// made from the target as it was read; one that another change replaced
// meanwhile is refused, not stored over it.
func TestTwoChangesAtOnceKeepNeitherOver(t *testing.T) {
	b := newBackupHarness(t)
	body := s3Target("b2", "old-secret")
	body["host"] = b.host.Id
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, body); status != http.StatusCreated {
		t.Fatalf("add: %d %v", status, out)
	}
	b.checksDone()
	id := b.targetNamed("b2").Id
	entered, release := b.gateJobs(func(job fakedocker.Job) bool { return !slices.Contains(job.Cmd, "--usage") }, nil)
	rotated := make(chan int, 1)
	go func() {
		status, _ := b.send(http.MethodPatch, "/api/pc/backup-targets/"+id, b.admin, map[string]any{
			"credentials": map[string]any{"access_key_id": "minio-access-b2", "secret_access_key": "rotated-secret"},
		})
		rotated <- status
	}()
	<-entered
	region := make(chan map[string]any, 1)
	go func() {
		status, out := b.send(http.MethodPatch, "/api/pc/backup-targets/"+id, b.admin, map[string]any{
			"config": map[string]any{"endpoint": "http://minio:9000", "bucket": "backups", "prefix": "b2", "path_style": true, "region": "eu-1"},
		})
		out["status_code"] = status
		region <- out
	}()
	b.waitFor("the second change's check to queue", func() bool {
		queued, _ := b.app.FindRecordsByFilter(schema.Commands, "type='backup.init' && status='queued'", "", 0, 0)
		return len(queued) == 1
	})
	release()
	if status := <-rotated; status != http.StatusOK {
		t.Fatalf("the rotation: %d", status)
	}
	if out := <-region; out["status_code"] == http.StatusOK || !strings.Contains(out["message"].(string), "changed meanwhile") {
		t.Errorf("the change made from the old key: %v", out)
	}
	target := b.targetNamed("b2")
	creds, _ := b.panel.Box.OpenString(target.GetString("credentials"))
	if !strings.Contains(creds, "rotated-secret") || strings.Contains(target.GetString("config"), "eu-1") {
		t.Errorf("the rotation was undone: %s %s", creds, target.GetString("config"))
	}
}

// TestAClientThatLeavesEndsNothing (second review of #28): the check goes on
// without the client, and so do the writes that follow it.
func TestAClientThatLeavesEndsNothing(t *testing.T) {
	b := newBackupHarness(t)
	entered, release := b.gateJobs(func(fakedocker.Job) bool { return true }, nil)
	body := s3Target("refused", "a-"+fakerestic.Wrong+"-secret")
	body["host"] = b.host.Id
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/api/pc/backup-targets", strings.NewReader(string(raw))).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", b.admin)
	done := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		b.handler.ServeHTTP(recorder, request)
		done <- recorder.Code
	}()
	<-entered
	cancel()
	release()
	<-done
	b.checksDone()
	b.waitFor("the refused target to go", func() bool { return len(b.targets()) == 0 })
}

// TestACheckSentAgainAfterAReconnectKeepsNoStaleResult (second review of
// #28): the agent goes on with what it was first sent; the panel does not
// know what that was once a change came between.
func TestACheckSentAgainAfterAReconnectKeepsNoStaleResult(t *testing.T) {
	b := newBackupHarness(t)
	body := s3Target("b2", "old-secret")
	body["host"] = b.host.Id
	if status, out := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, body); status != http.StatusCreated {
		t.Fatalf("add: %d %v", status, out)
	}
	b.checksDone()
	id := b.targetNamed("b2").Id
	entered, release := b.gateJobs(func(job fakedocker.Job) bool { return slices.Contains(job.Cmd, "--usage") },
		func(fakedocker.JobResult) fakedocker.JobResult {
			return fakedocker.JobResult{Code: 1, Stderr: "error: Fatal: unable to open config file: Access Denied"}
		})
	b.panel.CheckBackupTargets()
	<-entered
	if status, out := b.send(http.MethodPatch, "/api/pc/backup-targets/"+id, b.admin, map[string]any{
		"credentials": map[string]any{"access_key_id": "minio-access-b2", "secret_access_key": "new-secret"},
	}); status != http.StatusOK {
		t.Fatalf("new credentials: %d %v", status, out)
	}
	measure, err := b.app.FindFirstRecordByFilter(schema.Commands, "type='backup.init' && status='running'")
	if err != nil {
		t.Fatal(err)
	}
	b.panel.Commands.Drop(b.host.Id, errors.New("a test drops the stream"))
	b.waitFor("the measure to be sent again", func() bool { return b.command(measure.Id).GetInt("attempts") >= 2 })
	release()
	b.checksDone()
	if target := b.targetNamed("b2"); target.GetString("status") != schema.TargetOK {
		t.Errorf("a stale measure failed the target: %s", target.GetString("last_error"))
	}
}

// TestATargetBeingAddedIsNotChanged (second review of #28): its first check
// may yet remove it, with its password; a change's check would make its
// repository all the same.
func TestATargetBeingAddedIsNotChanged(t *testing.T) {
	b := newBackupHarness(t)
	entered, release := b.gateJobs(func(fakedocker.Job) bool { return true }, nil)
	body := s3Target("b2", "a-"+fakerestic.Wrong+"-secret")
	body["host"] = b.host.Id
	added := make(chan int, 1)
	go func() {
		status, _ := b.send(http.MethodPost, "/api/pc/backup-targets", b.admin, body)
		added <- status
	}()
	<-entered
	id := b.targetNamed("b2").Id
	if status, out := b.send(http.MethodPatch, "/api/pc/backup-targets/"+id, b.admin, map[string]any{
		"credentials": map[string]any{"access_key_id": "minio-access-b2", "secret_access_key": "good-secret"},
	}); status != http.StatusConflict || !strings.Contains(out["message"].(string), "still being added") {
		t.Errorf("a change during the first check: %d %v", status, out)
	}
	release()
	if status := <-added; status != http.StatusBadRequest {
		t.Errorf("the refused add: %d", status)
	}
	b.checksDone()
	if len(b.targets()) != 0 || fakerestic.Repositories(t, b.restic) != 0 {
		t.Errorf("%d targets, %d repositories", len(b.targets()), fakerestic.Repositories(t, b.restic))
	}
}
