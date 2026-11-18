package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakefetch"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

// runAgent is an agent that runs backup jobs, and the jobs it ran.
type runAgent struct {
	*Agent
	engine *fakedocker.Engine
	root   string
	mu     sync.Mutex
	jobs   []fakedocker.Job
}

func newRunAgent(t *testing.T) *runAgent {
	t.Helper()
	a, engine, root := backupAgent(t)
	engine.VolumeRoot = t.TempDir()
	a.Config.JobImageRoot = jobImage(t)
	r := &runAgent{Agent: a, engine: engine, root: root}
	run := engine.OnJob
	engine.OnJob = func(job fakedocker.Job) fakedocker.JobResult {
		r.mu.Lock()
		r.jobs = append(r.jobs, job)
		r.mu.Unlock()
		return run(job)
	}
	return r
}

// jobImage writes the directories of the backup jobs' image, the agent's own
// (docker:27-cli, on Alpine), that hold its symbolic links, and the links,
// which the agent checks the directories it backs up against.
func jobImage(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"/run/lock", "/var/mail", "/var/spool/cron", "/etc/crontabs", "/etc/ssl/certs", "/etc/ssl1.1", "/srv", "/tmp"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		"/var/run":                 "../run",
		"/var/lock":                "../run/lock",
		"/var/spool/mail":          "../mail",
		"/var/spool/cron/crontabs": "../../../etc/crontabs",
		"/etc/ssl1.1/certs":        "/etc/ssl/certs",
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// jobCount is how many jobs ran.
func (r *runAgent) jobCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.jobs)
}

// lastJob is the job that ran last.
func (r *runAgent) lastJob(t *testing.T) fakedocker.Job {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.jobs) == 0 {
		t.Fatal("no job ran")
	}
	return r.jobs[len(r.jobs)-1]
}

// remote is a target's repository the panel completed, created.
func (r *runAgent) remote(t *testing.T) *agentv1.BackupRepository {
	t.Helper()
	repo := &agentv1.BackupRepository{
		TargetId: "t1", Url: "s3:http://minio:9000/backups", Password: "repo-password-1",
		Env: map[string]string{"AWS_ACCESS_KEY_ID": "minio-key", "AWS_SECRET_ACCESS_KEY": "minio-secret"},
	}
	if _, err := r.handleBackupInit(context.Background(), initCommand(repo), &Reporter{}); err != nil {
		t.Fatal(err)
	}
	return repo
}

// volume makes a volume of the engine with files.
func (r *runAgent) volume(t *testing.T, name string, files map[string]string) {
	t.Helper()
	r.engine.AddVolume(name)
	writeFiles(t, r.engine.VolumeDir(name), files)
}

func runCommand(run *agentv1.BackupRun) *agentv1.Command {
	return &agentv1.Command{Type: "backup.run", Payload: &agentv1.Command_BackupRun{BackupRun: run}}
}

// mountsOf are a job's mounts by their target.
func mountsOf(job fakedocker.Job) map[string]map[string]any {
	mounts := map[string]map[string]any{}
	list, _ := job.Host["Mounts"].([]any)
	for _, raw := range list {
		mount := raw.(map[string]any)
		mounts[mount["Target"].(string)] = mount
	}
	return mounts
}

// TestABackupRunMountsItsVolumesReadOnly (#29): the job mounts each volume at
// /volumes/<name> read-only, besides restic's cache, has no capability but
// DAC_OVERRIDE, to read what it backs up (never DAC_READ_SEARCH, with which
// open_by_handle_at reaches past its mounts), and restic saves one snapshot
// with the run's host and tags; the result is restic's summary, the
// retention's count and the log, without a secret.
func TestABackupRunMountsItsVolumesReadOnly(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	r.volume(t, "umami_data", map[string]string{"events.db": "events", "conf/app.ini": "x=1"})
	r.volume(t, "umami_cache", map[string]string{"c": "cached"})
	run := &agentv1.BackupRun{
		RunId: "r1", Repository: repo, Volumes: []string{"umami_data", "umami_cache"},
		Tags: []string{"pocketcloud", "job:j1"}, HostName: "nas", Retention: &agentv1.BackupRetention{Last: 3, Daily: 7},
	}
	result, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
	if err != nil {
		t.Fatalf("%v: %v", err, result)
	}
	snapshots := fakerestic.Snapshots(t, r.root, repo.GetUrl())
	if len(snapshots) != 1 {
		t.Fatalf("snapshots: %+v", snapshots)
	}
	snapshot := snapshots[0]
	if snapshot.Hostname != "nas" || !slices.Equal(snapshot.Tags, []string{"pocketcloud", "job:j1"}) ||
		!slices.Equal(snapshot.Paths, []string{"/volumes/umami_data", "/volumes/umami_cache"}) ||
		snapshot.Files["/volumes/umami_data/conf/app.ini"] != "x=1" || snapshot.Files["/volumes/umami_cache/c"] != "cached" {
		t.Errorf("the snapshot: %+v", snapshot)
	}
	log, _ := result["log"].(string)
	if result["snapshot_id"] != snapshot.ID || result["files_new"] != int64(3) || result["files_changed"] != int64(0) ||
		result["bytes_added"] != int64(15) || result["total_bytes"] != int64(15) || result["forgotten"] != int64(0) ||
		result["refused"] != nil || result["retention_error"] != nil || result["bytes_stored"] != int64(15) || len(result) != 8 {
		t.Errorf("the result: %v", result)
	}
	if !strings.Contains(log, "Saved the snapshot "+snapshot.ID[:8]) || !strings.Contains(log, "Applying the retention: keep last 3, daily 7") ||
		strings.Contains(log, "repo-password-1") || strings.Contains(log, "minio-secret") {
		t.Errorf("the log:\n%s", log)
	}

	job := r.lastJob(t)
	if !slices.Equal(job.Cmd, []string{"backup-tool", "backup", "--host", "nas", "--tag", "pocketcloud", "--tag", "job:j1",
		"--keep-last", "3", "--keep-daily", "7", "--path", "/volumes/umami_data", "--path", "/volumes/umami_cache", "--cache", "/cache"}) {
		t.Errorf("the job's command: %q", job.Cmd)
	}
	mounts := mountsOf(job)
	if len(mounts) != 4 || mounts["/cache"]["Source"] != resticCache || !coversImageVolume(job.Host["Mounts"].([]any)) {
		t.Errorf("mounts: %v", mounts)
	}
	for _, name := range []string{"umami_data", "umami_cache"} {
		if mount := mounts["/volumes/"+name]; mount["Type"] != "volume" || mount["Source"] != name || mount["ReadOnly"] != true {
			t.Errorf("the volume %s: %v", name, mount)
		}
	}
	if job.Host["NetworkMode"] != "bridge" || !slices.Equal(anyStrings(job.Host["CapDrop"]), []string{"ALL"}) ||
		!slices.Equal(anyStrings(job.Host["CapAdd"]), []string{"DAC_OVERRIDE"}) ||
		!slices.Equal(anyStrings(job.Host["SecurityOpt"]), []string{"no-new-privileges"}) || job.Stdin != nil {
		t.Errorf("host config: %v", job.Host)
	}
	if job.Labels["pocketcloud.role"] != "job" || job.Labels[backupTargetLabel] != "t1" {
		t.Errorf("labels: %v", job.Labels)
	}
	if calls := fakerestic.Calls(t, r.root); !strings.Contains(calls, " forget --json --prune --retry-lock 5m --group-by tags --tag pocketcloud,job:j1 --keep-last 3 --keep-daily 7\n") {
		t.Errorf("restic ran:\n%s", calls)
	}
	for _, name := range r.engine.Names() {
		if name != "pocketcloud-agent" {
			t.Errorf("a container is left: %s", name)
		}
	}

	// the retention counts what went
	for range 3 {
		if _, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{}); err != nil {
			t.Fatal(err)
		}
	}
	run.Retention = &agentv1.BackupRetention{Last: 2}
	result, err = r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
	if err != nil || result["forgotten"] != int64(3) || result["files_new"] != int64(0) || result["bytes_added"] != int64(0) {
		t.Errorf("the last run: %v %v", result, err)
	}
	if snapshots := fakerestic.Snapshots(t, r.root, repo.GetUrl()); len(snapshots) != 2 {
		t.Errorf("%d snapshots", len(snapshots))
	}
}

// TestABackupRunOfDirectoriesToALocalTarget (#29): a directory of the server
// is bind-mounted read-only at its own path, which the snapshot names; a
// local target's job has no network, and may read and write its directory.
func TestABackupRunOfDirectoriesToALocalTarget(t *testing.T) {
	r := newRunAgent(t)
	repoDir := filepath.Join(t.TempDir(), "restic")
	repo := &agentv1.BackupRepository{TargetId: "t2", Url: backup.LocalMount, Password: "repo-password-2", Mount: repoDir}
	if _, err := r.handleBackupInit(context.Background(), initCommand(repo), &Reporter{}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "srv", "photos")
	writeFiles(t, dir, map[string]string{"cat.jpg": "meow"})
	result, err := r.handleBackupRun(context.Background(), runCommand(&agentv1.BackupRun{
		RunId: "r2", Repository: repo, Paths: []string{dir}, Tags: []string{"pocketcloud", "job:j2"}, HostName: "nas",
	}), &Reporter{})
	if err != nil {
		t.Fatalf("%v: %v", err, result)
	}
	job := r.lastJob(t)
	mount := mountsOf(job)[dir]
	if options, _ := mount["BindOptions"].(map[string]any); mount["Type"] != "bind" || mount["Source"] != dir || mount["ReadOnly"] != true ||
		options["ReadOnlyForceRecursive"] != true {
		t.Errorf("the directory's mount: %v", mount)
	}
	if job.Host["NetworkMode"] != "none" || !slices.Equal(anyStrings(job.Host["CapAdd"]), []string{"DAC_OVERRIDE", "FOWNER"}) {
		t.Errorf("host config: %v", job.Host)
	}
	if slices.Contains(job.Cmd, "--keep-last") || !slices.Contains(job.Cmd, "--local") {
		t.Errorf("the job's command: %q", job.Cmd)
	}
	snapshots := fakerestic.Snapshots(t, r.root, repoDir)
	if len(snapshots) != 1 || snapshots[0].Files[dir+"/cat.jpg"] != "meow" || result["snapshot_id"] != snapshots[0].ID {
		t.Errorf("snapshots: %+v, result %v", snapshots, result)
	}
	if calls := fakerestic.Calls(t, r.root); strings.Contains(calls, " forget ") {
		t.Errorf("a run without retention forgot:\n%s", calls)
	}
}

// TestWhatABackupRunCannotReadIsRefused (#29): a volume that is not there is
// refused before any job, so that Docker never makes an empty one; so are
// directories that are not the server's to back up, and runs the panel did
// not complete.
func TestWhatABackupRunCannotReadIsRefused(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	jobs := r.jobCount()
	r.volume(t, "here", nil)
	ok := func() *agentv1.BackupRun {
		return &agentv1.BackupRun{Repository: repo, Volumes: []string{"here"}, Tags: []string{"pocketcloud", "job:j"}, HostName: "nas"}
	}
	for _, tc := range []struct {
		change func(*agentv1.BackupRun)
		want   string
	}{
		{func(b *agentv1.BackupRun) { b.Volumes = []string{"here", "ghost"} }, "the volume ghost does not exist on this server"},
		{func(b *agentv1.BackupRun) { b.Volumes = []string{"../etc"} }, `"../etc" is not a volume's name`},
		{func(b *agentv1.BackupRun) { b.Volumes = []string{"here", "here"} }, "there twice"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/"} }, "the whole of /"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"srv/app"} }, "absolute and clean"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/srv/../etc"} }, "absolute and clean"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/srv/"} }, "absolute and clean"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/proc/1"} }, "the kernel's"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/sys"} }, "the kernel's"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/dev/sda"} }, "the kernel's"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/usr"} }, "would hide the backup job's own /usr/bin"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/var"} }, "would hide the backup job's own /var/lib/pocketcloud"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/tmp"} }, "/tmp"},
		{func(b *agentv1.BackupRun) { b.Volumes, b.Paths = nil, []string{"/repo/x"} }, "in the backup job's own /repo"},
		{func(b *agentv1.BackupRun) { b.Paths = []string{"/srv"} }, "exactly one of them"},
		{func(b *agentv1.BackupRun) { b.Volumes = nil }, "exactly one of them"},
		{func(b *agentv1.BackupRun) { b.Tags = nil }, "needs its job's tags"},
		{func(b *agentv1.BackupRun) { b.HostName = "" }, "needs the server's name"},
		{func(b *agentv1.BackupRun) { b.Retention = &agentv1.BackupRetention{Weekly: -1} }, "not -1"},
		{func(b *agentv1.BackupRun) { b.Repository = &agentv1.BackupRepository{Url: repo.GetUrl()} }, "no password"},
	} {
		run := ok()
		tc.change(run)
		if _, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", run, err, tc.want)
		}
	}
	if r.jobCount() != jobs || r.engine.Called("/volumes/create") {
		t.Errorf("%d jobs ran", r.jobCount()-jobs)
	}
	// a directory that is not there: the Engine says so, as the agent cannot see it
	r.engine.Fail("POST /containers/create", `invalid mount config for type "bind": bind source path does not exist: /srv/no where`)
	run := ok()
	run.Volumes, run.Paths = nil, []string{"/srv/no where"}
	if _, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{}); err == nil || err.Error() != "the directory /srv/no where does not exist on this server" {
		t.Errorf("a directory that is not there: %v", err)
	}
	r.engine.Fail("POST /containers/create", "")
	// a directory of the system that the job has no use for is the server's to back up
	if err := checkBackupPath("/etc"); err != nil {
		t.Errorf("/etc: %v", err)
	}
	if err := checkBackupPath("/var/lib/app"); err != nil {
		t.Errorf("/var/lib/app: %v", err)
	}
}

// TestADirectoryIsFollowedOnTheServerBeforeItIsMounted (review of #29): the
// Engine would follow a symbolic link of the server past the checks, such as
// /var/run to /run, so the agent follows it first, through the server's /
// as it sees it, and checks and mounts where it leads; an absolute link
// leads from the server's /, and none out of it.
func TestADirectoryIsFollowedOnTheServerBeforeItIsMounted(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	root := t.TempDir()
	r.Config.HostRoot = root
	// what the server holds: a disk mounted elsewhere, and links
	disk := filepath.Join(t.TempDir(), "disk", "data")
	writeFiles(t, disk, map[string]string{"photo.jpg": "cheese"})
	plain := t.TempDir()
	for _, dir := range []string{disk, plain, "/run/pocketcloud", "/proc/1"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFiles(t, filepath.Join(root, "srv"), map[string]string{"notes.txt": "a file"})
	for link, target := range map[string]string{
		"/var/run":     "../run",
		"/srv/run":     "/run",
		"/srv/varrun":  "../var/run/",
		"/srv/data":    disk,
		"/srv/mnt":     "data",
		"/srv/kernel":  "/proc/1",
		"/srv/escape":  "../../../../../../.." + disk,
		"/srv/outside": "../../../../../../../etc",
		"/srv/loop":    "loop",
		"/srv/notes":   "notes.txt",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, link)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	run := func(dir string) (map[string]any, error) {
		return r.handleBackupRun(context.Background(), runCommand(&agentv1.BackupRun{
			Repository: repo, Paths: []string{dir}, Tags: []string{"pocketcloud", "job:j9"}, HostName: "nas",
		}), &Reporter{})
	}

	jobs := r.jobCount()
	for dir, want := range map[string]string{
		"/srv/run":            "/run would hide the backup job's own /run/pocketcloud",
		"/srv/varrun":         "/run would hide the backup job's own /run/pocketcloud",
		"/var/run":            "/run would hide the backup job's own /run/pocketcloud",
		"/srv/kernel":         "/proc/1 is the kernel's",
		"/srv/outside":        "the directory /srv/outside does not exist on this server",
		"/srv/loop":           "the directory /srv/loop leads through more than 40 symbolic links on this server",
		"/srv/notes":          "/srv/notes is not a directory on this server",
		"/srv/notes/x":        "the directory /srv/notes/x does not exist on this server",
		"/srv/none":           "the directory /srv/none does not exist on this server",
		"/var/spool/mail/bob": "the directory /var/spool/mail/bob does not exist on this server",
	} {
		if _, err := run(dir); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: %v, want %q", dir, err, want)
		}
	}
	if r.jobCount() != jobs {
		t.Errorf("%d jobs ran", r.jobCount()-jobs)
	}

	// a link, and one that would climb out of /, lead to the disk, which is
	// mounted where it is, and backed up as such
	for _, dir := range []string{"/srv/mnt", "/srv/escape"} {
		result, err := run(dir)
		if err != nil {
			t.Fatalf("%s: %v: %v", dir, err, result)
		}
		job := r.lastJob(t)
		if mount := mountsOf(job)[disk]; mount["Type"] != "bind" || mount["Source"] != disk || mount["ReadOnly"] != true || len(mountsOf(job)) != 3 {
			t.Errorf("%s: the mounts: %v", dir, mountsOf(job))
		}
		if i := slices.Index(job.Cmd, "--path"); i < 0 || job.Cmd[i+1] != disk {
			t.Errorf("%s: the job's command: %q", dir, job.Cmd)
		}
		if log, _ := result["log"].(string); !strings.HasPrefix(log, "The directory "+dir+" leads to "+disk+" on this server: backing up "+disk+"\n") {
			t.Errorf("%s: the log:\n%s", dir, log)
		}
	}
	snapshots := fakerestic.Snapshots(t, r.root, repo.GetUrl())
	if len(snapshots) != 2 || !slices.Equal(snapshots[1].Paths, []string{disk}) || snapshots[1].Files[disk+"/photo.jpg"] != "cheese" {
		t.Errorf("the snapshots: %+v", snapshots)
	}
	// two that lead to one are one
	if _, err := r.handleBackupRun(context.Background(), runCommand(&agentv1.BackupRun{
		Repository: repo, Paths: []string{"/srv/data", "/srv/mnt"}, Tags: []string{"pocketcloud", "job:j9"}, HostName: "nas",
	}), &Reporter{}); err == nil || err.Error() != "the directory "+disk+" is there twice" {
		t.Errorf("two links to one directory: %v", err)
	}
	// a directory without links is mounted as it is, and the log says nothing of it
	result, err := run(plain)
	if mount := mountsOf(r.lastJob(t))[plain]; mount["Source"] != plain || err != nil || strings.Contains(result["log"].(string), "leads to") {
		t.Errorf("a plain directory: %v %v %v", mount, err, result)
	}
	// an agent that does not see the server's / checks nothing, and mounts nothing
	r.Config.HostRoot = filepath.Join(root, "gone")
	if _, err := run(plain); err == nil || !strings.HasPrefix(err.Error(), "the agent cannot see the server's directories at "+r.Config.HostRoot) {
		t.Errorf("without the server's /: %v", err)
	}
}

// TestADirectoryIsCheckedAgainstTheJobsImageWhereItLeads (review of #29): a
// directory mounted at a symbolic link of the job's image, or in one, would
// land where the link leads in the job, and restic would save only the link,
// so the agent refuses it, and says what holds it; it checks every such link
// of the image, which is its own, in its own files. But it checks only where
// the directory leads on the server, which is what the job mounts: on
// Debian, where /var/spool/mail leads to /var/mail, it is backed up as
// /var/mail, and where it is a directory of its own, it is refused.
func TestADirectoryIsCheckedAgainstTheJobsImageWhereItLeads(t *testing.T) {
	r := newRunAgent(t)
	// a Debian server, with its users' crontabs in /var/spool/cron/crontabs
	// and the certificates of an old OpenSSL; and an older one, where
	// /var/spool/mail and /var/run are directories of their own
	debian, older := t.TempDir(), t.TempDir()
	for _, dir := range []string{debian + "/var/spool/cron/crontabs", debian + "/var/mail", debian + "/srv/app", debian + "/etc/ssl1.1/certs",
		older + "/var/spool/mail/bob", older + "/var/run/app"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFiles(t, debian+"/var/spool/cron/crontabs", map[string]string{"bob": "@daily /home/bob/sync"})
	if err := os.Symlink("../mail", debian+"/var/spool/mail"); err != nil {
		t.Fatal(err)
	}
	prepare := func(server, dir string) (backupJobSpec, error) {
		r.Config.HostRoot = server
		return r.backupRunJob(context.Background(), &agentv1.BackupRun{
			Paths: []string{dir}, Tags: []string{"pocketcloud", "job:j11"}, HostName: "nas",
		})
	}

	for _, tc := range []struct {
		server, dir string
		// mounted is where the job mounts the directory; refused, what the
		// agent says when it does not
		mounted, refused string
	}{
		{debian, "/var/spool/mail", "/var/mail", ""},
		{debian, "/srv/app", "/srv/app", ""},
		{debian, "/var/spool/cron", "/var/spool/cron", ""},
		{debian, "/var/spool/cron/crontabs", "", "/var/spool/cron/crontabs is a symbolic link in the backup job's image, so it cannot be mounted there: back up /var/spool/cron, which holds it"},
		{debian, "/etc/ssl1.1/certs", "", "/etc/ssl1.1/certs is a symbolic link in the backup job's image, so it cannot be mounted there: back up /etc/ssl1.1, which holds it"},
		{older, "/var/spool/mail", "", "/var/spool/mail is a symbolic link in the backup job's image, so it cannot be mounted there: back up /var/spool, which holds it"},
		{older, "/var/spool/mail/bob", "", "/var/spool/mail is a symbolic link in the backup job's image, so /var/spool/mail/bob cannot be mounted there: back up /var/spool, which holds it"},
		// /var would hide the job's own /var/lib/pocketcloud
		{older, "/var/run/app", "", "/var/run is a symbolic link in the backup job's image, so /var/run/app cannot be mounted there, and no directory that holds it can be backed up instead"},
	} {
		job, err := prepare(tc.server, tc.dir)
		if tc.refused != "" {
			if err == nil || err.Error() != tc.refused {
				t.Errorf("%s: %v, want %q", tc.dir, err, tc.refused)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.dir, err)
			continue
		}
		if len(job.mounts) != 1 || job.mounts[0]["Source"] != tc.mounted || job.mounts[0]["Target"] != tc.mounted {
			t.Errorf("%s: the mounts: %v", tc.dir, job.mounts)
		}
		if i := slices.Index(job.args, "--path"); i < 0 || job.args[i+1] != tc.mounted {
			t.Errorf("%s: the job's command: %q", tc.dir, job.args)
		}
		leads := "The directory " + tc.dir + " leads to " + tc.mounted + " on this server: backing up " + tc.mounted
		if slices.Contains(job.notes, leads) != (tc.dir != tc.mounted) {
			t.Errorf("%s: the notes: %q", tc.dir, job.notes)
		}
	}

	// an agent that is told nothing checks its own files, the image's
	r.Config.JobImageRoot = ""
	own := t.TempDir()
	if err := os.Symlink("/etc", filepath.Join(own, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(debian, own, "link"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := own + "/link is a symbolic link in the backup job's image, so it cannot be mounted there: back up " + own + ", which holds it"
	if _, err := prepare(debian, own+"/link"); err == nil || err.Error() != want {
		t.Errorf("a link in the agent's own files: %v, want %q", err, want)
	}
}

// TestADirectoryIsReadOnlyWithTheMountsUnderIt (review of #29): a disk
// mounted in a directory to back up is read-only in the job too, where the
// Engine (API 1.44) and the kernel (5.12) can make it so; elsewhere the run's
// log says that only the directory's top is.
func TestADirectoryIsReadOnlyWithTheMountsUnderIt(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.txt": "a"})
	top := dir + " is mounted read-only at its top only: this Docker Engine or kernel cannot make the mounts under it read-only.\n"
	for _, tc := range []struct {
		api, kernel string
		recursive   bool
	}{
		{"1.47", "6.1.0-18-amd64", true},
		{"1.44", "5.12.0", true},
		{"1.51", "6.18.44-fc-v310", true},
		{"1.43", "6.1.0-18-amd64", false},
		{"1.47", "5.10.0-28-amd64", false},
		{"1.41", "4.19.0-26-amd64", false},
	} {
		r.engine.APIVersion, r.engine.KernelVersion = tc.api, tc.kernel
		result, err := r.handleBackupRun(context.Background(), runCommand(&agentv1.BackupRun{
			Repository: repo, Paths: []string{dir}, Tags: []string{"pocketcloud", "job:j10"}, HostName: "nas",
		}), &Reporter{})
		if err != nil {
			t.Fatalf("%v: %v %v", tc, err, result)
		}
		mount := mountsOf(r.lastJob(t))[dir]
		options, _ := mount["BindOptions"].(map[string]any)
		log, _ := result["log"].(string)
		if tc.recursive && (options["ReadOnlyForceRecursive"] != true || len(options) != 1 || strings.Contains(log, "at its top only")) ||
			!tc.recursive && (mount["BindOptions"] != nil || !strings.HasPrefix(log, top)) || mount["ReadOnly"] != true {
			t.Errorf("Engine API %s, kernel %s: the mount %v, the log:\n%s", tc.api, tc.kernel, mount, log)
		}
	}
	for release, want := range map[string]bool{"5.12": true, "5.11.22": false, "10.0.1": true, "4.20.0": false, "6.10.14-linuxkit": true,
		"5.15.167.4-microsoft-standard-WSL2": true, "": false, "x.y": false, "5": false} {
		if kernelAtLeast(release, 5, 12) != want {
			t.Errorf("kernel %q: %v", release, !want)
		}
	}
}

// TestADumpIsFedThroughTheJobsInput (#29): the job is attached before it
// starts; the dump runs in its database's container with docker exec, and
// what it prints reaches restic through the job's input and the relay as
// the snapshot's one file. The job reads no file of the server, and has no
// capability (#53).
func TestADumpIsFedThroughTheJobsInput(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	db := r.engine.AddContainer("umami-db-1", "postgres:16", map[string]string{"com.docker.compose.service": "db"})
	dump := strings.Repeat("COPY public.website (id, name) FROM stdin;\n", 3000)
	command := []string{"sh", "-c", `exec pg_dump --format=custom --username="${POSTGRES_USER:-postgres}" "${POSTGRES_DB:-postgres}"`}
	var calls []*fakedocker.ExecCall
	var mu sync.Mutex
	answer := fakedocker.ExecResult{Stdout: dump, Stderr: "pg_dump: warning: a note\n"}
	r.engine.OnExec = func(call *fakedocker.ExecCall, _ []byte) fakedocker.ExecResult {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, call)
		return answer
	}
	answers := func(result fakedocker.ExecResult) {
		mu.Lock()
		defer mu.Unlock()
		answer = result
	}
	run := &agentv1.BackupRun{
		RunId: "r3", Repository: repo, StdinCommand: command, StdinContainer: db, StdinFileName: "umami-db.pgdump",
		Tags: []string{"pocketcloud", "job:j3"}, HostName: "nas",
	}
	result, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
	if err != nil {
		t.Fatalf("%v: %v", err, result)
	}
	snapshots := fakerestic.Snapshots(t, r.root, repo.GetUrl())
	if len(snapshots) != 1 || snapshots[0].Files["/umami-db.pgdump"] != dump || result["total_bytes"] != int64(len(dump)) {
		t.Fatalf("snapshots: %d, result %v", len(snapshots), result)
	}
	mu.Lock()
	if len(calls) != 1 || calls[0].Container != db || !slices.Equal(calls[0].Cmd, command) || calls[0].TTY || calls[0].Stdin {
		t.Errorf("the dump ran: %+v", calls)
	}
	mu.Unlock()
	if log, _ := result["log"].(string); !strings.Contains(log, "pg_dump: warning: a note") || strings.Contains(log, relayReady) {
		t.Errorf("the log:\n%s", log)
	}
	job := r.lastJob(t)
	if !slices.Equal(job.Cmd, []string{"backup-tool", "backup", "--host", "nas", "--tag", "pocketcloud", "--tag", "job:j3",
		"--stdin-name", "umami-db.pgdump", "--relay", dumpSocket, "--cache", "/cache"}) {
		t.Errorf("the job's command: %q", job.Cmd)
	}
	if len(mountsOf(job)) != 2 || job.Host["CapAdd"] != nil || job.Stdin == nil {
		t.Errorf("the job: %v", job.Host)
	}
	// attached before it started
	attached, started := -1, -1
	for i, call := range r.engine.Calls() {
		if strings.Contains(call, "/attach?") {
			attached = i
		}
		if strings.HasPrefix(call, "POST /containers/") && strings.Contains(call, "/start?") && attached >= 0 && started < 0 {
			started = i
		}
	}
	if attached < 0 || started < attached {
		t.Errorf("attached at %d, started at %d: %v", attached, started, r.engine.Calls())
	}

	// a dump that fails saves nothing, and says why
	answers(fakedocker.ExecResult{Stdout: "PGDMP", Stderr: "pg_dump: error: connection to server on socket failed\nFATAL: role \"postgres\" does not exist\n", Code: 1})
	result, err = r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
	if err == nil || err.Error() != `the dump failed with exit code 1, and restic saved no snapshot: pg_dump: error: connection to server on socket failed FATAL: role "postgres" does not exist` ||
		result["refused"] != true || result["snapshot_id"] != nil {
		t.Errorf("a dump that failed: %v %v", result, err)
	}
	// a command the container does not have
	answers(fakedocker.ExecResult{Stdout: "OCI runtime exec failed: exec failed: unable to start container process: exec: \"sqlite3\": executable file not found in $PATH\r\n", Code: 127})
	if _, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{}); err == nil ||
		!strings.Contains(err.Error(), `exit code 127, and restic saved no snapshot: OCI runtime exec failed: exec failed: unable to start container process: exec: "sqlite3": executable file not found in $PATH`) {
		t.Errorf("a missing command: %v", err)
	}
	if snapshots := fakerestic.Snapshots(t, r.root, repo.GetUrl()); len(snapshots) != 1 {
		t.Errorf("%d snapshots", len(snapshots))
	}

	// a database that is not running is said before any job
	jobs := r.jobCount()
	r.engine.SetStatus(db, "exited")
	if _, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{}); err == nil || err.Error() != "the container umami-db-1 is not running, and the dump runs in it" {
		t.Errorf("a stopped database: %v", err)
	}
	run.StdinContainer = strings.Repeat("f", 64)
	if _, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{}); err == nil || !strings.Contains(err.Error(), "does not exist on this server") {
		t.Errorf("a database that is gone: %v", err)
	}
	if r.jobCount() != jobs {
		t.Error("a job ran for a database that is not running")
	}
}

// TestThePanelsCopyIsBackedUpWithNothingMounted (#32): a run of the panel's
// backup job mounts nothing of the server, adds no capability, and backs up
// the panel's copy, fetched on a connection of its own, through the relay
// as /pocketcloud-panel.pcpb of the host panel with the job's three tags;
// the job is the hardened one of every backup, named after its command, and
// a local target's is bound with no network.
func TestThePanelsCopyIsBackedUpWithNothingMounted(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	server, dial := fakefetch.NewServer(t)
	r.LiveUpload = dial
	data := panelCopy(5<<20/2 + 1)
	server.Offer(testExport, fakefetch.Artifact{Data: data})
	run := panelRun(repo)
	result, err := r.handleBackupRun(context.Background(), commandOf("k3j9x2m4q8w1z7v", run), &Reporter{})
	if err != nil {
		t.Fatalf("%v: %v", err, result)
	}
	snapshots := fakerestic.Snapshots(t, r.root, repo.GetUrl())
	if len(snapshots) != 1 || snapshots[0].Files["/"+backup.PanelFileName] != string(data) || snapshots[0].Hostname != "panel" ||
		!slices.Equal(snapshots[0].Tags, []string{"pocketcloud", "job:p4n3lj0b0000001", "pocketcloud-panel"}) ||
		!slices.Equal(snapshots[0].Paths, []string{"/pocketcloud-panel.pcpb"}) {
		t.Fatalf("snapshots: %+v", snapshots)
	}
	if result["snapshot_id"] != snapshots[0].ID || result["total_bytes"] != int64(len(data)) {
		t.Errorf("the result: %v", result)
	}
	job := r.lastJob(t)
	if !slices.Equal(job.Cmd, []string{"backup-tool", "backup", "--host", "panel", "--tag", "pocketcloud", "--tag", "job:p4n3lj0b0000001",
		"--tag", "pocketcloud-panel", "--keep-daily", "14", "--keep-weekly", "8", "--keep-monthly", "12",
		"--stdin-name", "pocketcloud-panel.pcpb", "--relay", dumpSocket, "--cache", "/cache"}) {
		t.Errorf("the job's command: %q", job.Cmd)
	}
	mounts := mountsOf(job)
	if len(mounts) != 2 || mounts[resticCacheDir] == nil || mounts[imageVolume]["Type"] != "tmpfs" || mounts[imageVolume]["ReadOnly"] != true {
		t.Errorf("the job mounts %v", mounts)
	}
	if len(anyStrings(job.Host["CapAdd"])) != 0 || !slices.Equal(anyStrings(job.Host["CapDrop"]), []string{"ALL"}) ||
		!slices.Equal(anyStrings(job.Host["SecurityOpt"]), []string{"no-new-privileges"}) || job.Host["NetworkMode"] != "bridge" ||
		job.Hostname != "pc-k3j9x2m4q8w1z7v" || job.Stdin == nil {
		t.Errorf("the job: %v, hostname %q", job.Host, job.Hostname)
	}
	requests := server.Requests()
	if len(requests) != 1 || requests[0].GetArtifactId() != testExport || requests[0].GetOffset() != 0 || server.Dials() != 1 {
		t.Errorf("fetched: %v, %d dials", requests, server.Dials())
	}
	if log, _ := result["log"].(string); strings.Contains(log, relayReady) || !strings.Contains(log, "Backing up the panel's copy as pocketcloud-panel.pcpb") {
		t.Errorf("the log:\n%s", log)
	}

	// to a local target
	repoDir := filepath.Join(t.TempDir(), "restic")
	local := &agentv1.BackupRepository{TargetId: "t2", Url: backup.LocalMount, Password: "repo-password-2", Mount: repoDir}
	if _, err := r.handleBackupInit(context.Background(), initCommand(local), &Reporter{}); err != nil {
		t.Fatal(err)
	}
	run.Repository = local
	if result, err := r.handleBackupRun(context.Background(), commandOf("k3j9x2m4q8w1z7w", run), &Reporter{}); err != nil {
		t.Fatalf("%v: %v", err, result)
	}
	job = r.lastJob(t)
	if repoMount := mountsOf(job)[backup.LocalMount]; repoMount["Source"] != repoDir || len(mountsOf(job)) != 3 ||
		job.Host["NetworkMode"] != "none" || !slices.Equal(anyStrings(job.Host["CapAdd"]), []string{"DAC_OVERRIDE", "FOWNER"}) {
		t.Errorf("the local target's job: %v", job.Host)
	}
	if snapshots := fakerestic.Snapshots(t, r.root, repoDir); len(snapshots) != 1 || snapshots[0].Files["/"+backup.PanelFileName] != string(data) {
		t.Errorf("the local target's snapshots: %d", len(snapshots))
	}
}

// TestARunOfThePanelIsRefusedWhenItIsNotOne (#32): the panel's copy is one
// source of four, named by its run's id, saved as pocketcloud-panel.pcpb,
// with no container to run in; anything else is refused before a job runs
// or the panel is asked for anything.
func TestARunOfThePanelIsRefusedWhenItIsNotOne(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	server, dial := fakefetch.NewServer(t)
	r.LiveUpload = dial
	jobs := r.jobCount()
	r.volume(t, "here", nil)
	for _, tc := range []struct {
		change func(*agentv1.BackupRun)
		want   string
	}{
		{func(b *agentv1.BackupRun) { b.Volumes = []string{"here"} }, "a backup run backs up volumes, directories, a dump, the panel or a site's files: exactly one of them"},
		{func(b *agentv1.BackupRun) { b.Paths = []string{"/srv"} }, "exactly one of them"},
		{func(b *agentv1.BackupRun) { b.StdinCommand = []string{"pg_dump"} }, "exactly one of them"},
		{func(b *agentv1.BackupRun) { b.PanelExport = "panel-export:short" }, `"panel-export:short" names no copy of the panel`},
		{func(b *agentv1.BackupRun) { b.PanelExport = "panel-export:K3J9X2M4Q8W1Z7V" }, "names no copy of the panel"},
		{func(b *agentv1.BackupRun) { b.PanelExport = "site-upload:k3j9x2m4q8w1z7v" }, "names no copy of the panel"},
		{func(b *agentv1.BackupRun) { b.PanelExport = testExport + "/../x" }, "names no copy of the panel"},
		{func(b *agentv1.BackupRun) { b.StdinFileName = "panel.db" }, `the panel's copy is saved as pocketcloud-panel.pcpb, not as "panel.db"`},
		{func(b *agentv1.BackupRun) { b.StdinFileName = "" }, "saved as pocketcloud-panel.pcpb"},
		{func(b *agentv1.BackupRun) { b.StdinFileName = "../" + backup.PanelFileName }, "saved as pocketcloud-panel.pcpb"},
		{func(b *agentv1.BackupRun) { b.StdinContainer = "umami-db-1" }, "a backup of the panel runs nothing in a container"},
		{func(b *agentv1.BackupRun) { b.Env = map[string]string{"PGPASSWORD": "x"} }, "runs nothing in a container"},
		{func(b *agentv1.BackupRun) { b.Tags = nil }, "needs its job's tags"},
		{func(b *agentv1.BackupRun) { b.HostName = "" }, "needs the server's name"},
	} {
		run := panelRun(repo)
		tc.change(run)
		if _, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", run, err, tc.want)
		}
	}
	if r.jobCount() != jobs || len(server.Requests()) != 0 || server.Dials() != 0 {
		t.Errorf("%d jobs ran, %d fetches", r.jobCount()-jobs, len(server.Requests()))
	}
}

// TestADumpStartsOnceResticReadsIt (review of #29): restic may wait for the
// repository's lock, or fail, before it runs the relay, and a dump started
// with the job would hold the database's locks all that time, or run for
// nothing; the agent starts it once the backup tool says restic reads it,
// and never when the job ends first. The tool's line stays out of the log.
func TestADumpStartsOnceResticReadsIt(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	db := r.engine.AddContainer("app-db-1", "postgres:16", nil)
	var mu sync.Mutex
	var execs []string
	r.engine.OnExec = func(*fakedocker.ExecCall, []byte) fakedocker.ExecResult {
		mu.Lock()
		defer mu.Unlock()
		// what restic had done when the dump started
		execs = append(execs, strings.Join(resticCommands(t, r.root), " "))
		return fakedocker.ExecResult{Stdout: "CREATE TABLE t;\n"}
	}
	dumps := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(execs)
	}
	run := func(repo *agentv1.BackupRepository) (map[string]any, error) {
		return r.handleBackupRun(context.Background(), runCommand(&agentv1.BackupRun{
			Repository: repo, StdinCommand: []string{"pg_dump"}, StdinContainer: db, StdinFileName: "app-db.sql",
			Tags: []string{"pocketcloud", "job:j12"}, HostName: "nas",
		}), &Reporter{})
	}

	// restic that fails before it runs the relay: no dump
	wrong := &agentv1.BackupRepository{TargetId: repo.GetTargetId(), Url: repo.GetUrl(), Password: "another-password", Env: repo.GetEnv()}
	if _, err := run(wrong); err == nil || !strings.Contains(err.Error(), "the password is not its own") {
		t.Errorf("a wrong password: %v", err)
	}
	t.Setenv("FAKE_RESTIC_LOCKED_BACKUP", "2")
	if _, err := run(repo); err == nil || !strings.Contains(err.Error(), "is already locked exclusively") {
		t.Errorf("a repository that stays locked: %v", err)
	}
	if len(dumps()) != 0 {
		t.Errorf("dumps ran while restic did not read them: %q", dumps())
	}

	// one that waited for the lock reads the dump once it has it
	t.Setenv("FAKE_RESTIC_LOCKED_BACKUP", "3")
	result, err := run(repo)
	if err != nil {
		t.Fatalf("%v: %v", err, result)
	}
	if ran := dumps(); len(ran) != 1 || !strings.HasSuffix(ran[0], " backup unlock backup") {
		t.Errorf("the dump started after restic ran %q", ran)
	}
	snapshots := fakerestic.Snapshots(t, r.root, repo.GetUrl())
	if len(snapshots) != 1 || snapshots[0].Files["/app-db.sql"] != "CREATE TABLE t;\n" {
		t.Errorf("the snapshots: %+v", snapshots)
	}
	if log, _ := result["log"].(string); strings.Contains(log, relayReady) || !strings.Contains(log, "Backing up the dump as app-db.sql") {
		t.Errorf("the log:\n%s", log)
	}

	// a job killed once restic reads the dump is no error of the tool's line
	r.engine.OnJob = func(job fakedocker.Job) fakedocker.JobResult {
		fmt.Fprintln(job.Stderr, relayReady)
		return fakedocker.JobResult{Code: 137}
	}
	if _, err := run(repo); err == nil || err.Error() != "exit code 137" {
		t.Errorf("a job killed while it reads the dump: %v", err)
	}
}

// TestABackupRunThatCouldNotReadEverythingFails (#29): the run fails with
// restic's words, its result says the snapshot; a retention that fails
// leaves a run that saved its snapshot a success, and says why.
func TestABackupRunThatCouldNotReadEverythingFails(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	r.volume(t, "data", map[string]string{"open": "1", "locked": "2"})
	run := &agentv1.BackupRun{Repository: repo, Volumes: []string{"data"}, Tags: []string{"pocketcloud", "job:j4"}, HostName: "nas", Retention: &agentv1.BackupRetention{Last: 1}}

	t.Setenv("FAKE_RESTIC_UNREADABLE", "locked")
	result, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
	id, _ := result["snapshot_id"].(string)
	if err == nil || len(id) != 64 || result["refused"] != true ||
		err.Error() != "restic saved the snapshot "+id[:8]+", but could not read everything: open /volumes/data/locked: permission denied Warning: at least one source file could not be read" {
		t.Errorf("a partial backup: %v %v", result, err)
	}

	t.Setenv("FAKE_RESTIC_UNREADABLE", "")
	t.Setenv("FAKE_RESTIC_FORGET_FAIL", "unable to open repository: a bad key")
	result, err = r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
	if err != nil || result["snapshot_id"] == nil || result["retention_error"] != "Fatal: unable to open repository: a bad key" || result["retention_deferred"] != nil {
		t.Errorf("a failed retention: %v %v", result, err)
	}

	// one that another restic's lock kept out waits for the next run, and
	// says who held it (review of #29)
	t.Setenv("FAKE_RESTIC_FORGET_FAIL", "")
	t.Setenv("FAKE_RESTIC_LOCKED_FORGET", "2")
	result, err = r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
	if err != nil || result["snapshot_id"] == nil || result["retention_error"] != nil || len(result) != 8 ||
		result["retention_deferred"] != "unable to create lock in backend: repository is already locked by PID 1 on other-server by root (UID 0, GID 0)" {
		t.Errorf("a retention locked out: %v %v", result, err)
	}
}

// TestACancelledBackupJobIsStoppedBeforeItIsRemoved (review of #29): a run
// that is cancelled, or times out, gives its job 30 seconds to stop, as
// restic then removes its locks, before it removes the job; other jobs, a
// target's check, which takes no lock, among them, are removed at once, as
// before.
func TestACancelledBackupJobIsStoppedBeforeItIsRemoved(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	r.volume(t, "data", map[string]string{"a": "1"})
	entered := make(chan string, 2)
	stopped := make(chan string, 2)
	// the jobs removed by force, which the fake leaves running, end
	removed := make(chan struct{})
	t.Cleanup(func() { close(removed) })
	run := r.engine.OnJob
	r.engine.OnJob = func(job fakedocker.Job) fakedocker.JobResult {
		if len(job.Cmd) < 2 || job.Cmd[1] != "backup" && job.Cmd[0] != "site-tool" && !slices.Contains(job.Cmd, "--expect") {
			return run(job)
		}
		entered <- job.Name
		select {
		case <-job.Stop:
			stopped <- job.Name
			return fakedocker.JobResult{Code: 130, Stderr: "error: signal: terminated"}
		case <-removed:
			return fakedocker.JobResult{Code: 137}
		}
	}
	// the stop and the removal of a job's container, in order: the fake
	// names it after its id
	callsOf := func(name string) []string {
		prefix := "/containers/" + strings.TrimPrefix(name, "generated-")
		var calls []string
		for _, call := range r.engine.Calls() {
			if strings.Contains(call, prefix) && (strings.Contains(call, "/stop?") || strings.HasPrefix(call, "DELETE ")) {
				calls = append(calls, call)
			}
		}
		return calls
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.handleBackupRun(ctx, runCommand(&agentv1.BackupRun{
			Repository: repo, Volumes: []string{"data"}, Tags: []string{"pocketcloud", "job:j11"}, HostName: "nas",
		}), &Reporter{})
		done <- err
	}()
	name := <-entered
	cancel()
	if err := <-done; err == nil {
		t.Error("a cancelled run succeeded")
	}
	select {
	case got := <-stopped:
		if got != name {
			t.Errorf("%s stopped, not %s", got, name)
		}
	default:
		t.Error("the job was not stopped")
	}
	if calls := callsOf(name); len(calls) != 2 || !strings.HasSuffix(calls[0], "/stop?t=30") || !strings.HasPrefix(calls[1], "DELETE ") {
		t.Errorf("the job's container: %q", calls)
	}

	// a target's check, and a site's job, are removed at once, as before
	for _, start := range []func(ctx context.Context) error{
		func(ctx context.Context) error {
			command := initCommand(repo)
			command.GetBackupInit().RepoId = strings.Repeat("a", 64)
			_, err := r.handleBackupInit(ctx, command, &Reporter{})
			return err
		},
		func(ctx context.Context) error {
			return dockerSites{a: r.Agent}.run(ctx, map[string]any{"Image": agentImage, "Cmd": []string{"site-tool", "sweep"},
				"Labels": map[string]string{"pocketcloud.role": "job"}}, nil, io.Discard, io.Discard)
		},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { done <- start(ctx) }()
		name := <-entered
		cancel()
		if err := <-done; err == nil {
			t.Error("a cancelled job succeeded")
		}
		if calls := callsOf(name); len(calls) != 1 || !strings.HasPrefix(calls[0], "DELETE ") {
			t.Errorf("another job: %q", calls)
		}
	}
	if len(stopped) != 0 {
		t.Errorf("%d other jobs were stopped", len(stopped))
	}
}

// TestBackupRunsHaveSlotsOfTheirOwn (#29): two run at a time on a server,
// whatever else of the backups runs.
func TestBackupRunsHaveSlotsOfTheirOwn(t *testing.T) {
	x := newExecutor(nil, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	for range 2 {
		release, err := x.acquire(ctx, "backup.run")
		if err != nil {
			t.Fatalf("a run waited: %v", err)
		}
		defer release()
	}
	if _, err := x.acquire(ctx, "backup.run"); err == nil {
		t.Error("a third run ran beside two")
	}
	if release, err := x.acquire(context.Background(), "backup.other"); err != nil {
		t.Errorf("another backup command waited behind runs: %v", err)
	} else {
		release()
	}
	if perTypeLimits["backup.run"] != 2 {
		t.Errorf("limits: %v", perTypeLimits)
	}
}

// TestARunSaysWhyItFailed (#31): when restic saved nothing, a run's answer
// carries the failure's class, restic's exit code and the lock that kept it
// out beside its log, and crosses the stream as it is; the retry of a run
// that found the repository locked unlocks first; a directory the Engine
// would not mount, as it is gone, is the source's failure.
func TestARunSaysWhyItFailed(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	r.volume(t, "data", map[string]string{"a": "1"})
	run := &agentv1.BackupRun{Repository: repo, Volumes: []string{"data"}, Tags: []string{"pocketcloud", "job:j31"}, HostName: "nas"}

	t.Setenv("FAKE_RESTIC_LOCKED_BACKUP", "2")
	t.Setenv("FAKE_RESTIC_LOCK_HOST", "pc-k3j9x2m4q8w1z7v")
	result, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
	lock, _ := result["lock"].(map[string]any)
	if err == nil || !strings.Contains(err.Error(), "is already locked exclusively by PID 1 on pc-k3j9x2m4q8w1z7v") || result["refused"] != true ||
		result["failure"] != backup.FailureLocked || result["restic_code"] != int64(11) || result["snapshot_id"] != nil {
		t.Errorf("a repository that stayed locked: %v %v", result, err)
	}
	if lock["host"] != "pc-k3j9x2m4q8w1z7v" || lock["pid"] != float64(1) || lock["exclusive"] != true || lock["age_s"] != float64(300) || lock["created"] == nil {
		t.Errorf("the lock: %v", lock)
	}
	if log, _ := result["log"].(string); !strings.Contains(log, "lock was created at") {
		t.Errorf("the log:\n%s", log)
	}
	if _, err := structpb.NewStruct(result); err != nil {
		t.Errorf("the answer cannot cross the stream: %v", err)
	}
	if slices.Contains(r.lastJob(t).Cmd, "--unlock") {
		t.Errorf("a first run unlocks first: %q", r.lastJob(t).Cmd)
	}

	// its retry runs restic unlock before the backup
	t.Setenv("FAKE_RESTIC_LOCKED_BACKUP", "0")
	run.Unlock = true
	result, err = r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
	if err != nil || result["snapshot_id"] == nil || result["failure"] != nil {
		t.Fatalf("the retry: %v %v", result, err)
	}
	if commands := resticCommands(t, r.root); !slices.Equal(commands[len(commands)-2:], []string{"unlock", "backup"}) || !slices.Contains(r.lastJob(t).Cmd, "--unlock") {
		t.Errorf("restic ran %q for %q", commands, r.lastJob(t).Cmd)
	}

	// another failure, and none of restic's
	t.Setenv("FAKE_RESTIC_BACKUP_FAIL", "unable to save snapshot: connection refused")
	result, err = r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
	if err == nil || result["failure"] != backup.FailureRestic || result["restic_code"] != int64(1) || result["lock"] != nil {
		t.Errorf("restic failed: %v %v", result, err)
	}
	t.Setenv("FAKE_RESTIC_BACKUP_FAIL", "")
	dir := t.TempDir()
	r.engine.Fail("POST /containers/create", `invalid mount config for type "bind": bind source path does not exist: `+dir)
	result, err = r.handleBackupRun(context.Background(), runCommand(&agentv1.BackupRun{Repository: repo, Paths: []string{dir},
		Tags: []string{"pocketcloud", "job:j31"}, HostName: "nas"}), &Reporter{})
	r.engine.Fail("POST /containers/create", "")
	if err == nil || err.Error() != "the directory "+dir+" does not exist on this server" || result["failure"] != backup.FailureSource || result["restic_code"] != nil {
		t.Errorf("a directory that is gone: %v %v", result, err)
	}
	r.engine.Fail("POST /containers/create", "the Engine is gone")
	result, err = r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
	r.engine.Fail("POST /containers/create", "")
	if err == nil || result["failure"] != nil {
		t.Errorf("a job that did not start: %v %v", result, err)
	}
}

// databaseRun is a backup run of a database job's dump (#53), as the panel
// sends it: the dump runs in the shared server's container, with no
// environment, and the snapshot's extra tags name the database and its
// server.
func databaseRun(repo *agentv1.BackupRepository, container string) *agentv1.BackupRun {
	return &agentv1.BackupRun{
		RunId: "r53", Repository: repo, StdinContainer: container, StdinFileName: "shop.pgdump",
		StdinCommand: []string{"sh", "-c", `exec pg_dump --format=custom --username="$1" --dbname="$2"`, "sh", "postgres", "shop"},
		Tags:         []string{"pocketcloud", "job:j53"}, ExtraTags: backup.DatabaseTags("shop", "db-main"), HostName: "nas",
		Retention: &agentv1.BackupRetention{Daily: 7},
	}
}

// TestADatabasesDumpIsTaggedAndKeptAsOneGroup (#53): a database job's dump
// runs in the server's container, its snapshot has the job's tags and the
// extra tags db:<name> and dbserver:<server>, and its retention selects the
// job's snapshots by the job's tags alone, as one group, so that a move to
// another server leaves no group that is never thinned out. The job has no
// capability on a remote target, and a local target's own on a local one;
// the answer says what the repository grew by, packed.
func TestADatabasesDumpIsTaggedAndKeptAsOneGroup(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	db := r.engine.AddContainer("pcdb-main-db-1", "postgres:17", map[string]string{"pocketcloud.role": "db"})
	r.engine.OnExec = func(*fakedocker.ExecCall, []byte) fakedocker.ExecResult {
		return fakedocker.ExecResult{Stdout: "PGDMP of shop"}
	}
	run := databaseRun(repo, "pcdb-main-db-1")
	result, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
	if err != nil {
		t.Fatalf("%v: %v", err, result)
	}
	job := r.lastJob(t)
	if !slices.Equal(job.Cmd, []string{"backup-tool", "backup", "--host", "nas", "--tag", "pocketcloud", "--tag", "job:j53",
		"--extra-tag", "db:shop", "--extra-tag", "dbserver:db-main", "--keep-daily", "7", "--stdin-name", "shop.pgdump", "--relay", dumpSocket, "--cache", "/cache"}) {
		t.Errorf("the job's command: %q", job.Cmd)
	}
	if job.Host["CapAdd"] != nil || job.Host["NetworkMode"] != "bridge" || len(mountsOf(job)) != 2 {
		t.Errorf("a dump to a remote target: %v", job.Host)
	}
	snapshots := fakerestic.Snapshots(t, r.root, repo.GetUrl())
	if len(snapshots) != 1 || !slices.Equal(snapshots[0].Tags, []string{"pocketcloud", "job:j53", "db:shop", "dbserver:db-main"}) ||
		snapshots[0].Files["/shop.pgdump"] != "PGDMP of shop" {
		t.Fatalf("snapshots: %+v", snapshots)
	}
	if result["bytes_stored"] != int64(len("PGDMP of shop")) {
		t.Errorf("what the repository grew by: %v", result)
	}
	calls := fakerestic.Calls(t, r.root)
	if !strings.Contains(calls, " backup --json --quiet --retry-lock 30m --host nas --tag pocketcloud,job:j53,db:shop,dbserver:db-main --stdin-from-command") ||
		!strings.Contains(calls, " forget --json --prune --retry-lock 5m --group-by  --tag pocketcloud,job:j53 --keep-daily 7\n") {
		t.Errorf("restic ran:\n%s", calls)
	}
	// after a move, the server's tag differs, and the job keeps one group
	run.ExtraTags = backup.DatabaseTags("shop", "db-new")
	run.Retention = &agentv1.BackupRetention{Last: 1}
	if result, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{}); err != nil || result["forgotten"] != int64(1) {
		t.Fatalf("after a move: %v %v", result, err)
	}
	if snapshots := fakerestic.Snapshots(t, r.root, repo.GetUrl()); len(snapshots) != 1 || !slices.Contains(snapshots[0].Tags, "dbserver:db-new") {
		t.Errorf("one snapshot of the job is left: %+v", snapshots)
	}

	// a local target adds its own, with no network
	local := &agentv1.BackupRepository{TargetId: "t2", Url: backup.LocalMount, Password: "repo-password-2", Mount: filepath.Join(t.TempDir(), "restic")}
	if _, err := r.handleBackupInit(context.Background(), initCommand(local), &Reporter{}); err != nil {
		t.Fatal(err)
	}
	if result, err := r.handleBackupRun(context.Background(), runCommand(databaseRun(local, db)), &Reporter{}); err != nil {
		t.Fatalf("to a local target: %v %v", err, result)
	}
	if job := r.lastJob(t); !slices.Equal(anyStrings(job.Host["CapAdd"]), []string{"DAC_OVERRIDE", "FOWNER"}) || job.Host["NetworkMode"] != "none" {
		t.Errorf("a dump to a local target: %v", job.Host)
	}

	// without extra tags, #29's retention is as it was
	plain := databaseRun(repo, db)
	plain.ExtraTags = nil
	if _, err := r.handleBackupRun(context.Background(), runCommand(plain), &Reporter{}); err != nil {
		t.Fatal(err)
	}
	if calls := fakerestic.Calls(t, r.root); !strings.Contains(calls, " forget --json --prune --retry-lock 5m --group-by tags --tag pocketcloud,job:j53 --keep-daily 7\n") {
		t.Errorf("restic ran:\n%s", calls)
	}
}

// TestADatabaseRunHoldsItsDatabaseForReading (#53): a database's dump holds
// the database of its server's container for reading, which a restore of it
// writes: one does not run while the other does; another database of the
// server is not held.
func TestADatabaseRunHoldsItsDatabaseForReading(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	db := r.engine.AddContainer("pcdb-main-db-1", "postgres:17", map[string]string{"pocketcloud.role": "db"})
	r.engine.OnExec = func(*fakedocker.ExecCall, []byte) fakedocker.ExecResult {
		return fakedocker.ExecResult{Stdout: "PGDMP of shop"}
	}
	release, err := r.claims.claim(true, []string{databaseClaim(db, "shop")})
	if err != nil {
		t.Fatal(err)
	}
	jobs := r.jobCount()
	if _, err := r.handleBackupRun(context.Background(), runCommand(databaseRun(repo, "pcdb-main-db-1")), &Reporter{}); err == nil ||
		err.Error() != "A restore into shop is under way: back it up once it ends." {
		t.Errorf("a dump during a restore of its database: %v", err)
	}
	if r.jobCount() != jobs {
		t.Error("a job ran")
	}
	release()
	release, err = r.claims.claim(true, []string{databaseClaim(db, "blog")})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := r.handleBackupRun(context.Background(), runCommand(databaseRun(repo, "pcdb-main-db-1")), &Reporter{}); err != nil {
		t.Errorf("a dump during a restore of another database: %v", err)
	}
}

// TestASitesFilesAreBackedUpReadOnly (#53): a PHP site's job mounts its
// directory of the sites volume read-only at /srv/sites/<dir>: through the
// volume's subpath from Engine API 1.45, and a bind of the volume's own
// directory before; DAC_OVERRIDE stays, as www-data owns the files; what
// the site's template excludes is left out; the tag site:<name> is the
// snapshot's.
func TestASitesFilesAreBackedUpReadOnly(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	r.volume(t, "pocketcloud-sites", map[string]string{"wp/shared/wp-content/uploads/a.png": "png", "wp/shared/wp-content/cache/c": "cached",
		"wp/releases/d1/index.php": "<?php"})
	run := &agentv1.BackupRun{RunId: "r54", Repository: repo, SiteDir: "wp/shared", Tags: []string{"pocketcloud", "job:j54"},
		ExtraTags: backup.SiteTags("wp"), Excludes: []string{"/srv/sites/wp/shared/wp-content/cache"}, HostName: "nas"}
	for _, api := range []string{"1.45", "1.44"} {
		r.engine.APIVersion = api
		result, err := r.handleBackupRun(context.Background(), runCommand(run), &Reporter{})
		if err != nil {
			t.Fatalf("API %s: %v: %v", api, err, result)
		}
		job := r.lastJob(t)
		mount := mountsOf(job)["/srv/sites/wp/shared"]
		options, _ := mount["VolumeOptions"].(map[string]any)
		switch api {
		case "1.45":
			if mount["Type"] != "volume" || mount["Source"] != "pocketcloud-sites" || mount["ReadOnly"] != true || options["Subpath"] != "wp/shared" {
				t.Errorf("API %s: the mount %v", api, mount)
			}
		default:
			if mount["Type"] != "bind" || mount["Source"] != filepath.Join(r.engine.VolumeDir("pocketcloud-sites"), "wp/shared") || mount["ReadOnly"] != true {
				t.Errorf("API %s: the mount %v", api, mount)
			}
		}
		if !slices.Equal(anyStrings(job.Host["CapAdd"]), []string{"DAC_OVERRIDE"}) {
			t.Errorf("API %s: capabilities %v", api, job.Host["CapAdd"])
		}
		if !slices.Equal(job.Cmd[:12], []string{"backup-tool", "backup", "--host", "nas", "--tag", "pocketcloud", "--tag", "job:j54",
			"--extra-tag", "site:wp", "--path", "/srv/sites/wp/shared"}) || !slices.Contains(job.Cmd, "--exclude") {
			t.Errorf("API %s: the job's command %q", api, job.Cmd)
		}
		snapshots := fakerestic.Snapshots(t, r.root, repo.GetUrl())
		snapshot := snapshots[len(snapshots)-1]
		if snapshot.Files["/srv/sites/wp/shared/wp-content/uploads/a.png"] != "png" || len(snapshot.Files) != 1 ||
			!slices.Equal(snapshot.Tags, []string{"pocketcloud", "job:j54", "site:wp"}) {
			t.Errorf("API %s: the snapshot %+v", api, snapshot)
		}
	}

	// a classic site's whole directory, its build left out
	r.engine.APIVersion = ""
	classic := &agentv1.BackupRun{RunId: "r55", Repository: repo, SiteDir: "wp", Tags: []string{"pocketcloud", "job:j55"},
		ExtraTags: backup.SiteTags("wp"), Excludes: []string{"/srv/sites/wp/releases/*/wp-content/cache", "/srv/sites/wp/.build"}, HostName: "nas"}
	if _, err := r.handleBackupRun(context.Background(), runCommand(classic), &Reporter{}); err != nil {
		t.Fatal(err)
	}
	if mount := mountsOf(r.lastJob(t))["/srv/sites/wp"]; mount["ReadOnly"] != true || mount["VolumeOptions"].(map[string]any)["Subpath"] != "wp" {
		t.Errorf("a classic site's mount: %v", mount)
	}

	jobs := r.jobCount()
	for _, tc := range []struct {
		change func(*agentv1.BackupRun)
		want   string
	}{
		{func(p *agentv1.BackupRun) { p.SiteDir = "../etc" }, "no directory of a site"},
		{func(p *agentv1.BackupRun) { p.SiteDir = "wp/shared/../../etc" }, "no directory of a site"},
		{func(p *agentv1.BackupRun) { p.SiteDir = "wp/releases" }, "no directory of a site"},
		{func(p *agentv1.BackupRun) { p.SiteDir = "WP" }, "no directory of a site"},
		{func(p *agentv1.BackupRun) { p.Volumes = []string{"data"} }, "exactly one of them"},
		{func(p *agentv1.BackupRun) { p.Excludes = []string{"/srv/sites/blog/shared/cache"} }, "under no directory the backup saves"},
		{func(p *agentv1.BackupRun) { p.Excludes = []string{"/srv/sites/wp/shared"} }, "under no directory the backup saves"},
		{func(p *agentv1.BackupRun) { p.Excludes = []string{"/srv/sites/wp/shared/../../../etc"} }, "absolute and clean"},
		{func(p *agentv1.BackupRun) { p.Excludes = []string{"cache"} }, "absolute and clean"},
		{func(p *agentv1.BackupRun) {
			p.Excludes = slices.Repeat([]string{"/srv/sites/wp/shared/x"}, backup.MaxExcludes+1)
		}, "at most 100"},
	} {
		changed := &agentv1.BackupRun{RunId: "r56", Repository: repo, SiteDir: "wp/shared", Tags: []string{"pocketcloud", "job:j54"}, HostName: "nas"}
		tc.change(changed)
		if _, err := r.handleBackupRun(context.Background(), runCommand(changed), &Reporter{}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v", changed, err)
		}
	}
	// a dump has no path to leave a part of out
	dump := databaseRun(repo, r.engine.AddContainer("pcdb-main-db-1", "postgres:17", map[string]string{"pocketcloud.role": "db"}))
	dump.Excludes = []string{"/shop.pgdump"}
	if _, err := r.handleBackupRun(context.Background(), runCommand(dump), &Reporter{}); err == nil || !strings.Contains(err.Error(), "under no directory the backup saves") {
		t.Errorf("a dump with an exclude: %v", err)
	}
	if r.jobCount() != jobs {
		t.Error("a job ran for a refused run")
	}
}
