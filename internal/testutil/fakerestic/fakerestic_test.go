package fakerestic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
)

// The fake restic's own behaviour that per-database backups lean on (#53).

// resticRun runs the fake restic with a repository and its password, and
// says what it printed and its exit code.
func resticRun(t *testing.T, repository string, args ...string) (string, string, int) {
	t.Helper()
	command := exec.Command("restic", args...)
	command.Env = append(os.Environ(), "RESTIC_REPOSITORY="+repository, "RESTIC_PASSWORD=pw")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return stdout.String(), stderr.String(), exit.ExitCode()
	case err != nil:
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), 0
}

// mustRestic runs the fake restic, which must succeed.
func mustRestic(t *testing.T, repository string, args ...string) string {
	t.Helper()
	stdout, stderr, code := resticRun(t, repository, args...)
	if code != 0 {
		t.Fatalf("restic %q: exit %d: %s", args, code, stderr)
	}
	return stdout
}

// withFiles is a directory with files.
func withFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		file := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestForgetTakesEverySnapshotAsOneGroupWithAnEmptyGroupBy (#53): a
// database's job keeps its snapshots under one retention whatever its extra
// tags, as restic's forget with an empty --group-by does: one group, whose tags are
// null; by tags, each server's tag would be a group of its own; any other
// grouping is refused.
func TestForgetTakesEverySnapshotAsOneGroupWithAnEmptyGroupBy(t *testing.T) {
	root := Install(t)
	repo := "s3:http://minio:9000/groups"
	mustRestic(t, repo, "init", "--json")
	dir := withFiles(t, map[string]string{"shop.pgdump": "PGDMP"})
	for _, server := range []string{"main", "other", "other"} {
		mustRestic(t, repo, "backup", "--json", "--host", "vm", "--tag", "pocketcloud,job:j1,db:shop,dbserver:"+server, "--", dir)
	}
	mustRestic(t, repo, "backup", "--json", "--host", "vm", "--tag", "pocketcloud,job:j2", "--", dir)

	byTags := mustRestic(t, repo, "forget", "--json", "--prune", "--group-by", "tags", "--tag", "pocketcloud,job:j1", "--keep-last", "1")
	var groups []struct {
		Tags   []string          `json:"tags"`
		Keep   []json.RawMessage `json:"keep"`
		Remove []json.RawMessage `json:"remove"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(byTags, "\n", 2)[0]), &groups); err != nil || len(groups) != 2 {
		t.Fatalf("by tags, one group per server: %v %s", err, byTags)
	}
	if left := len(Snapshots(t, root, repo)); left != 3 {
		t.Fatalf("by tags, each server keeps one: %d snapshots left", left)
	}

	mustRestic(t, repo, "backup", "--json", "--host", "vm", "--tag", "pocketcloud,job:j1,db:shop,dbserver:main", "--", dir)
	whole := mustRestic(t, repo, "forget", "--json", "--prune", "--group-by", "", "--tag", "pocketcloud,job:j1", "--keep-last", "1")
	line := strings.SplitN(whole, "\n", 2)[0]
	if !strings.HasPrefix(line, `[{"tags":null,`) {
		t.Errorf("one group without tags, as restic says it: %s", line)
	}
	groups = nil
	if err := json.Unmarshal([]byte(line), &groups); err != nil || len(groups) != 1 || len(groups[0].Keep) != 1 || len(groups[0].Remove) != 2 {
		t.Fatalf("one group: %v %s", err, line)
	}
	left := Snapshots(t, root, repo)
	if len(left) != 2 {
		t.Fatalf("the job keeps its newest, and the other job its own: %d left", len(left))
	}
	if !slices.Contains(left[1].Tags, "dbserver:main") || !slices.Contains(left[0].Tags, "job:j2") {
		t.Errorf("what is left: %v", left)
	}

	if _, stderr, code := resticRun(t, repo, "forget", "--json", "--group-by", "host", "--tag", "pocketcloud", "--keep-last", "1"); code != 1 ||
		!strings.Contains(stderr, "not host") {
		t.Errorf("another grouping: %d %s", code, stderr)
	}
}

// TestBackupLeavesOutWhatAnExcludeNames (#53): a PHP site's backup leaves
// out the cache its template names, with all it holds, and keeps the rest.
func TestBackupLeavesOutWhatAnExcludeNames(t *testing.T) {
	root := Install(t)
	repo := "s3:http://minio:9000/excludes"
	mustRestic(t, repo, "init", "--json")
	dir := withFiles(t, map[string]string{
		"wp-content/uploads/a.png": "png", "wp-content/cache/page.html": "cached", "wp-content/cache/deep/x": "x",
		"wp-content/upgrade/u": "u", "storage/framework/views/v.php": "v", "storage/app/keep": "keep",
	})
	mustRestic(t, repo, "backup", "--json", "--host", "vm", "--tag", "pocketcloud,job:j1",
		"--exclude", dir+"/wp-content/cache", "--exclude", dir+"/wp-content/upgrade", "--exclude", dir+"/storage/*/views", "--", dir)
	snapshots := Snapshots(t, root, repo)
	if len(snapshots) != 1 {
		t.Fatalf("%d snapshots", len(snapshots))
	}
	var saved []string
	for name := range snapshots[0].Files {
		saved = append(saved, strings.TrimPrefix(name, dir))
	}
	slices.Sort(saved)
	if want := []string{"/storage/app/keep", "/wp-content/uploads/a.png"}; !slices.Equal(saved, want) {
		t.Errorf("saved %q, want %q", saved, want)
	}
}

// TestADamagedRepositoryFailsItsDumps (#53): a dump of a repository whose
// pack changed fails as restic 0.17.3's does, and writes nothing; another
// repository dumps on.
func TestADamagedRepositoryFailsItsDumps(t *testing.T) {
	root := Install(t)
	repo, other := "s3:http://minio:9000/broken", "s3:http://minio:9000/fine"
	dir := withFiles(t, map[string]string{"shop.sql": "-- Dump completed\n"})
	ids := map[string]string{}
	for _, r := range []string{repo, other} {
		mustRestic(t, r, "init", "--json")
		mustRestic(t, r, "backup", "--json", "--host", "vm", "--tag", "pocketcloud,job:j1", "--", dir)
		ids[r] = Snapshots(t, root, r)[0].ID
	}
	Damage(t, root, repo)
	stdout, stderr, code := resticRun(t, repo, "dump", "--archive", "tar", ids[repo], "/")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "ciphertext verification failed") ||
		!strings.HasPrefix(stderr, `Fatal: loading tree for snapshot "`+ids[repo]+`" failed: decrypting blob <tree/`) {
		t.Errorf("a damaged dump: %d %q %s", code, stdout, stderr)
	}
	if out := mustRestic(t, other, "dump", "--archive", "tar", ids[other], dir+"/shop.sql"); out != "-- Dump completed\n" {
		t.Errorf("the other repository dumps %q", out)
	}
}

// TestRunJobsReadsASitesFilesThroughTheirSubpath (#53): a site's job mounts
// a directory of the sites volume read-only at /srv/sites/<dir>, which the
// fake restic reads from the volume's directory; a writable one is refused.
func TestRunJobsReadsASitesFilesThroughTheirSubpath(t *testing.T) {
	engine := fakedocker.New(t)
	engine.VolumeRoot = t.TempDir()
	engine.AddVolume("pocketcloud-sites")
	var mounts string
	RunJobs(engine, func(_ context.Context, _ []string, env map[string]string, _ io.Reader, _, _ io.Writer) error {
		mounts = env["FAKE_RESTIC_MOUNTS"]
		return nil
	})
	mount := map[string]any{"Type": "volume", "Source": "pocketcloud-sites", "Target": "/srv/sites/wp/shared", "ReadOnly": true,
		"VolumeOptions": map[string]any{"Subpath": "wp/shared", "NoCopy": true}}
	job := fakedocker.Job{Cmd: []string{"backup-tool", "backup", "--path", "/srv/sites/wp/shared"}, Host: map[string]any{"Mounts": []any{mount}}}
	if result := engine.OnJob(job); result.Code != 0 {
		t.Fatalf("the job: %+v", result)
	}
	if want := "/srv/sites/wp/shared=" + filepath.Join(engine.VolumeDir("pocketcloud-sites"), "wp", "shared"); mounts != want {
		t.Errorf("the fake reads %q, want %q", mounts, want)
	}
	mount["ReadOnly"] = false
	if result := engine.OnJob(job); result.Code != 125 || !strings.Contains(result.Stderr, "writable") {
		t.Errorf("a writable mount: %+v", result)
	}
}

// TestTheConversionContainerEchoesTheDump (#53): the conversion of a
// PostgreSQL dump to SQL prints pg_restore's header and the dump as it
// comes, and refuses what is no custom archive or another command.
func TestTheConversionContainerEchoesTheDump(t *testing.T) {
	engine := fakedocker.New(t)
	RunJobs(engine, nil)
	convert := func(input string, cmd ...string) (fakedocker.JobResult, string) {
		var out bytes.Buffer
		job := fakedocker.Job{Entrypoint: []string{"pg_restore"}, Cmd: cmd, Stdin: strings.NewReader(input), Stdout: &out}
		result := engine.OnJob(job)
		return result, out.String()
	}
	result, out := convert("PGDMP\x01\x0e of shop", "--file=-", "--no-owner", "--no-privileges")
	if result.Code != 0 || out != "--\n-- PostgreSQL database dump\n--\n\nPGDMP\x01\x0e of shop" {
		t.Errorf("the conversion: %+v %q", result, out)
	}
	if result, out := convert("-- plain SQL", "--file=-"); result.Code != 1 || out != "" || !strings.Contains(result.Stderr, "not appear to be a valid archive") {
		t.Errorf("no archive: %+v %q", result, out)
	}
	if result, _ := convert("PGDMP", "--list"); result.Code != 1 {
		t.Errorf("another command: %+v", result)
	}
}
