package e2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

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
	admin  string
	host   *core.Record
	agent  *runningAgent
	restic string

	mu   sync.Mutex
	jobs []fakedocker.Job
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
	b.agent, b.host = b.enrolled("backup-node")
	_, b.admin = testutil.NewUser(t, b.app, "admin@example.com", schema.RoleAdmin)
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
	if fakerestic.Repositories(t, b.restic) != 1 || strings.Count(fakerestic.Calls(t, b.restic), " init\n") != 1 {
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

	// the commands, as the panel keeps them and viewers read them
	inits, err := b.app.FindRecordsByFilter(schema.Commands, "type='backup.init'", "created", 0, 0)
	if err != nil || len(inits) != 5 {
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
	if len(jobs) != 5 {
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
