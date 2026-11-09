package agent

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

const agentImage = "localhost:5000/pocketcloud-agent:dev"

// backupAgent is an agent on a fake engine that runs backup jobs with the
// backup tool and the fake restic.
func backupAgent(t *testing.T) (*Agent, *fakedocker.Engine, string) {
	t.Helper()
	root := fakerestic.Install(t)
	engine := fakedocker.New(t)
	engine.AddImage(agentImage, nil)
	// a container's image, as the Engine names it
	engine.AddImage("sha256:"+strings.Repeat("a", 64), nil)
	engine.AddContainer("pocketcloud-agent", agentImage, map[string]string{"pocketcloud.role": "agent"})
	fakerestic.RunJobs(engine, BackupTool)
	client, err := dockerapi.New(engine.Host())
	if err != nil {
		t.Fatal(err)
	}
	return &Agent{Docker: client, Log: slog.New(slog.DiscardHandler), Config: Config{ContainerName: "pocketcloud-agent"}}, engine, root
}

func initCommand(repo *agentv1.BackupRepository) *agentv1.Command {
	return &agentv1.Command{Payload: &agentv1.Command_BackupInit{BackupInit: &agentv1.BackupInit{Repository: repo}}}
}

// jobCreated checks that a job container was created.
func jobCreated(t *testing.T, engine *fakedocker.Engine) {
	t.Helper()
	if !engine.Called("POST /containers/create") {
		t.Fatal("no job container was created")
	}
}

// TestABackupTargetIsCreatedOnceInAJobOfItsOwn (#28): restic runs in a job
// container of the agent's image, with the repository's password and the
// storage's credentials in its environment, without privileges; the first
// init creates the repository and the next one opens it.
func TestABackupTargetIsCreatedOnceInAJobOfItsOwn(t *testing.T) {
	a, engine, root := backupAgent(t)
	var seen []fakedocker.Job
	run := engine.OnJob
	engine.OnJob = func(job fakedocker.Job) fakedocker.JobResult {
		seen = append(seen, job)
		return run(job)
	}
	repo := &agentv1.BackupRepository{
		TargetId: "t1", Url: "s3:http://minio:9000/backups", Password: "repo-password-1",
		Env:     map[string]string{"AWS_ACCESS_KEY_ID": "minio-key", "AWS_SECRET_ACCESS_KEY": "minio-secret", "AWS_DEFAULT_REGION": "us-east-1"},
		Options: map[string]string{"s3.bucket-lookup": "path"},
	}
	result, err := a.handleBackupInit(context.Background(), initCommand(repo), &Reporter{})
	if err != nil {
		t.Fatal(err)
	}
	if result["created"] != true || len(result["id"].(string)) != 64 || result["used_bytes"] != int64(0) || result["free_bytes"] != nil {
		t.Errorf("the first init: %v", result)
	}
	again, err := a.handleBackupInit(context.Background(), initCommand(repo), &Reporter{})
	if err != nil || again["created"] != false || again["id"] != result["id"] {
		t.Errorf("the second init: %v %v", again, err)
	}
	jobCreated(t, engine)
	if len(seen) != 2 {
		t.Fatalf("%d jobs ran", len(seen))
	}
	job := seen[0]
	if !slices.Equal(job.Cmd, []string{"backup-tool", "init", "--cache", "/cache", "-o", "s3.bucket-lookup=path"}) {
		t.Errorf("the job's command: %q", job.Cmd)
	}
	for _, want := range []string{"RESTIC_REPOSITORY=s3:http://minio:9000/backups", "RESTIC_PASSWORD=repo-password-1",
		"AWS_ACCESS_KEY_ID=minio-key", "AWS_SECRET_ACCESS_KEY=minio-secret", "AWS_DEFAULT_REGION=us-east-1"} {
		if !slices.Contains(job.Env, want) {
			t.Errorf("the job's environment lacks %s: %q", want, job.Env)
		}
	}
	if job.Labels["pocketcloud.role"] != "job" || job.Labels[backupTargetLabel] != "t1" {
		t.Errorf("labels: %v", job.Labels)
	}
	if job.Host["NetworkMode"] != "bridge" || !slices.Equal(anyStrings(job.Host["CapDrop"]), []string{"ALL"}) ||
		!slices.Equal(anyStrings(job.Host["SecurityOpt"]), []string{"no-new-privileges"}) {
		t.Errorf("host config: %v", job.Host)
	}
	// restic's cache, and nothing else of the server
	if mounts, _ := job.Host["Mounts"].([]any); len(mounts) != 1 || mounts[0].(map[string]any)["Source"] != resticCache ||
		mounts[0].(map[string]any)["Type"] != "volume" || job.Host["CapAdd"] != nil {
		t.Errorf("a remote target's job mounts %v, may %v", mounts, job.Host["CapAdd"])
	}
	calls := fakerestic.Calls(t, root)
	if !strings.Contains(calls, "restic --cache-dir /cache --cleanup-cache -o s3.bucket-lookup=path --no-lock cat config") ||
		!strings.Contains(calls, "restic --cache-dir /cache --cleanup-cache -o s3.bucket-lookup=path init --json") || strings.Count(calls, " init --json\n") != 1 ||
		strings.Contains(calls, " stats ") {
		t.Errorf("restic ran:\n%s", calls)
	}
	if fakerestic.Repositories(t, root) != 1 {
		t.Error("the repository was not created")
	}
	// the job is gone
	for _, name := range engine.Names() {
		if name != "pocketcloud-agent" {
			t.Errorf("a container is left: %s", name)
		}
	}
}

// TestALocalTargetIsTheOnlyThingItsJobMounts (#28): a local target's directory
// is bind-mounted at /repo, made when it is missing, without a network; the
// repository says how much room the file system leaves it.
func TestALocalTargetIsTheOnlyThingItsJobMounts(t *testing.T) {
	a, engine, _ := backupAgent(t)
	var job fakedocker.Job
	run := engine.OnJob
	engine.OnJob = func(j fakedocker.Job) fakedocker.JobResult {
		job = j
		return run(j)
	}
	dir := filepath.Join(t.TempDir(), "restic")
	t.Setenv("FAKE_RESTIC_SIZE", "4096")
	result, err := a.handleBackupInit(context.Background(), initCommand(&agentv1.BackupRepository{
		TargetId: "t2", Url: backup.LocalMount, Password: "repo-password-2", Mount: dir,
	}), &Reporter{})
	if err != nil {
		t.Fatal(err)
	}
	if result["created"] != true || result["used_bytes"] != int64(0) || result["free_bytes"].(int64) <= 0 {
		t.Errorf("result: %v", result)
	}
	if !slices.Equal(job.Cmd, []string{"backup-tool", "init", "--cache", "/cache", "--local", "/repo"}) || job.Host["NetworkMode"] != "none" ||
		!slices.Equal(anyStrings(job.Host["CapAdd"]), []string{"DAC_OVERRIDE", "FOWNER"}) {
		t.Errorf("job: %q %v %v", job.Cmd, job.Host["NetworkMode"], job.Host["CapAdd"])
	}
	// how much it holds, when the panel asks
	command := initCommand(&agentv1.BackupRepository{TargetId: "t2", Url: backup.LocalMount, Password: "repo-password-2", Mount: dir})
	command.GetBackupInit().RepoId = result["id"].(string)
	command.GetBackupInit().Usage = true
	if measured, err := a.handleBackupInit(context.Background(), command, &Reporter{}); err != nil || measured["used_bytes"] != int64(4096) ||
		!slices.Contains(job.Cmd, "--usage") || !slices.Contains(job.Cmd, "--expect") {
		t.Errorf("usage: %v %v %q", measured, err, job.Cmd)
	}
	mounts, _ := job.Host["Mounts"].([]any)
	if len(mounts) != 2 {
		t.Fatalf("mounts: %v", mounts)
	}
	mount := mounts[1].(map[string]any)
	options, _ := mount["BindOptions"].(map[string]any)
	if mount["Type"] != "bind" || mount["Source"] != dir || mount["Target"] != "/repo" || options["CreateMountpoint"] != true {
		t.Errorf("mount: %v", mount)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the new repository's directory: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config")); err != nil {
		t.Errorf("the repository is not in the directory: %v", err)
	}

	// a system directory is not mounted, whatever the command says
	if _, err := a.handleBackupInit(context.Background(), initCommand(&agentv1.BackupRepository{
		TargetId: "t3", Url: backup.LocalMount, Password: "p", Mount: "/etc/pocketcloud",
	}), &Reporter{}); err == nil || !strings.Contains(err.Error(), "/etc is the system's") {
		t.Errorf("a system directory: %v", err)
	}
}

// TestAnSFTPTargetsKeysAreCopiedIntoItsJob (#28): ssh reads the key and the
// server's pinned host key from files in the job, which reach it through the
// archive endpoint before it starts and never through its environment.
func TestAnSFTPTargetsKeysAreCopiedIntoItsJob(t *testing.T) {
	a, engine, _ := backupAgent(t)
	var job fakedocker.Job
	run := engine.OnJob
	engine.OnJob = func(j fakedocker.Job) fakedocker.JobResult {
		job = j
		return run(j)
	}
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n-----END OPENSSH PRIVATE KEY-----\n"
	known := "[backup-node]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHostKey\n"
	if _, err := a.handleBackupInit(context.Background(), initCommand(&agentv1.BackupRepository{
		TargetId: "t4", Url: "sftp:backup@backup-node:/srv/restic", Password: "repo-password-4",
		SshKey: key, KnownHosts: known, Options: map[string]string{"sftp.command": "ssh -i /run/pocketcloud/ssh-key backup@backup-node -s sftp"},
	}), &Reporter{}); err != nil {
		t.Fatal(err)
	}
	if string(job.Files[backup.SSHKeyFile]) != key || string(job.Files[backup.KnownHostsFile]) != known {
		t.Errorf("files: %v", job.Files)
	}
	for _, entry := range job.Env {
		if strings.Contains(entry, "PRIVATE KEY") || strings.Contains(entry, "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQ") {
			t.Errorf("the key is in the environment: %s", entry)
		}
	}
}

// TestWhatResticSaysKeepsNoSecret (#28): a storage that refuses the
// credentials fails the command with restic's own words, and neither they nor
// the output name a credential, even where restic repeats it.
func TestWhatResticSaysKeepsNoSecret(t *testing.T) {
	a, _, _ := backupAgent(t)
	secret := "a-" + fakerestic.Wrong + "-secret-key"
	var out bytes.Buffer
	_, err := a.backupJob(context.Background(), &agentv1.BackupRepository{
		TargetId: "t5", Url: "s3:http://minio:9000/backups", Password: "repo-password-5",
		Env: map[string]string{"AWS_ACCESS_KEY_ID": "minio-key", "AWS_SECRET_ACCESS_KEY": secret},
	}, backupJobSpec{args: []string{"init"}}, &out)
	if err == nil || !strings.Contains(err.Error(), "Access Denied") || strings.Contains(err.Error(), secret) {
		t.Errorf("the error: %v", err)
	}
	if strings.Contains(out.String(), secret) || !strings.Contains(out.String(), "Access Denied for key ***") {
		t.Errorf("the output:\n%s", out.String())
	}

	// the job ran and said no: the panel may forget a new target
	result, err := a.handleBackupInit(context.Background(), initCommand(&agentv1.BackupRepository{
		TargetId: "t5", Url: "s3:http://minio:9000/backups", Password: "repo-password-5",
		Env: map[string]string{"AWS_ACCESS_KEY_ID": "minio-key", "AWS_SECRET_ACCESS_KEY": secret},
	}), &Reporter{})
	if err == nil || result["refused"] != true {
		t.Errorf("a refusal: %v %v", result, err)
	}

	// a command the panel did not complete
	if _, err := a.handleBackupInit(context.Background(), initCommand(&agentv1.BackupRepository{TargetId: "t6", Url: "rest:http://x/"}), &Reporter{}); err == nil ||
		!strings.Contains(err.Error(), "no password") {
		t.Errorf("without a password: %v", err)
	}
}

// TestBackupJobsLeftByARestartAreRemoved (#28): the commands of jobs the
// agent was running when it stopped ended with it.
func TestBackupJobsLeftByARestartAreRemoved(t *testing.T) {
	a, engine, _ := backupAgent(t)
	engine.AddContainer("backup-job", agentImage, map[string]string{"pocketcloud.role": "job", backupTargetLabel: "t1"})
	engine.AddContainer("someones-job", "busybox", map[string]string{"pocketcloud.role": "job"})
	a.cleanupBackupJobs(context.Background())
	names := engine.Names()
	if slices.Contains(names, "backup-job") || !slices.Contains(names, "someones-job") {
		t.Errorf("containers: %v", names)
	}
}

func anyStrings(value any) []string {
	var out []string
	if list, ok := value.([]any); ok {
		for _, item := range list {
			text, _ := item.(string)
			out = append(out, text)
		}
	}
	return out
}

// TestABackupTargetsMeasureNeverTakesTheSlotsOfItsCheck (#28): measures may
// read a large repository for an hour; a check someone waits for has slots
// of its own.
func TestABackupTargetsMeasureNeverTakesTheSlotsOfItsCheck(t *testing.T) {
	measure := &agentv1.Command{Type: "backup.init", Payload: &agentv1.Command_BackupInit{BackupInit: &agentv1.BackupInit{Usage: true}}}
	check := &agentv1.Command{Type: "backup.init", Payload: &agentv1.Command_BackupInit{BackupInit: &agentv1.BackupInit{}}}
	run := &agentv1.Command{Type: "backup.run", Payload: &agentv1.Command_BackupRun{BackupRun: &agentv1.BackupRun{}}}
	if limitKey(measure) != "backup-usage" || limitKey(check) != "backup-check" || limitKey(run) != "backup.run" {
		t.Errorf("keys: %s %s %s", limitKey(measure), limitKey(check), limitKey(run))
	}
	x := newExecutor(nil, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// a measure holds its slot; a second waits, while checks still run
	release, err := x.acquire(ctx, limitKey(measure))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for range 2 {
		got, err := x.acquire(ctx, limitKey(check))
		if err != nil {
			t.Fatalf("a check waited behind a measure: %v", err)
		}
		defer got()
	}
	if _, err := x.acquire(ctx, limitKey(measure)); err == nil {
		t.Error("a second measure ran beside the first")
	}
}
