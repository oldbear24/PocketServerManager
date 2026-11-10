package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

// The backup tool's restore ops (#30), with the fake restic, whose lines
// are shaped after testdata/restic-0.17.3.

// runTool runs an op of the backup tool and returns what it printed.
func runTool(t *testing.T, env map[string]string, stdin io.Reader, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := BackupTool(context.Background(), args, env, stdin, &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

// decodeLast decodes the last line the tool printed.
func decodeLast(t *testing.T, out string, into any) {
	t.Helper()
	if err := json.Unmarshal(lastLine([]byte(out)), into); err != nil {
		t.Fatalf("the result %q: %v", out, err)
	}
}

// snapshotOf backs up dir as the job's path p (a volume's /volumes/<name>)
// and returns the snapshot's id.
func snapshotOf(t *testing.T, env map[string]string, p, dir string, tags ...string) string {
	t.Helper()
	if len(tags) == 0 {
		tags = []string{"pocketcloud", "job:aaaaaaaaaaaaaaa"}
	}
	runEnv := map[string]string{"FAKE_RESTIC_MOUNTS": p + "=" + dir}
	for name, value := range env {
		runEnv[name] = value
	}
	args := []string{"--host", "nas", "--path", p}
	for _, tag := range tags {
		args = append(args, "--tag", tag)
	}
	result, log, err := runBackup(t, runEnv, nil, args...)
	if err != nil {
		t.Fatalf("the backup: %v\n%s", err, log)
	}
	return result.SnapshotID
}

// readTree reads a directory's files, by their path in it.
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(file string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := os.ReadFile(file) //nolint:gosec // the test's own files
		rel, _ := filepath.Rel(dir, file)
		files[filepath.ToSlash(rel)] = string(content)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// restoreCalls are the restic restore commands the fake restic ran.
func restoreCalls(t *testing.T, root string) []string {
	t.Helper()
	var calls []string
	for line := range strings.SplitSeq(fakerestic.Calls(t, root), "\n") {
		if strings.HasPrefix(line, "restic ") && strings.Contains(line, " restore ") {
			calls = append(calls, line)
		}
	}
	return calls
}

// TestResticLinesAreReadAsRestic0173PrintsThem (#30): what restic 0.17.3
// printed is what the tool reads: a snapshot's summary, ls's nodes and
// modes, restore's actions, status and errors, and the include form.
func TestResticLinesAreReadAsRestic0173PrintsThem(t *testing.T) {
	fixture := func(name string) []byte {
		data, err := os.ReadFile(filepath.Join("testdata", "restic-0.17.3", name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var snapshots []resticSnapshot
	if err := json.Unmarshal(fixture("snapshots.json"), &snapshots); err != nil || len(snapshots) != 1 {
		t.Fatalf("%v %v", snapshots, err)
	}
	if s := snapshots[0]; s.Summary == nil || s.Summary.TotalBytesProcessed != 9 || s.Summary.DataAdded != 5700 || s.ShortID != s.ID[:8] ||
		!slices.Equal(s.Paths, []string{"/volumes/vx", "/volumes/vy"}) || s.ProgramVersion != "restic 0.17.3" {
		t.Errorf("the snapshot: %+v", s)
	}
	nodes := map[string]resticNode{}
	for line := range bytes.SplitSeq(fixture("ls.json"), []byte("\n")) {
		var node resticNode
		if json.Unmarshal(line, &node) == nil && node.node() {
			nodes[node.Path] = node
		}
	}
	if len(nodes) != 4 || unixMode(nodes["/volumes/vx/sub"].Mode) != 0o2770 || unixMode(nodes["/volumes/vx"].Mode) != 0o750 ||
		nodes["/volumes/vx"].UID != 1000 || nodes["/volumes/vx/a.txt"].Size != 3 || !nodes["/volumes/vx"].MTime.Equal(time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("the nodes: %+v", nodes)
	}
	actions := map[string]int{}
	for line := range bytes.SplitSeq(fixture("restore-dry-run.json"), []byte("\n")) {
		var parsed resticRestoreLine
		if json.Unmarshal(line, &parsed) == nil {
			actions[parsed.MessageType+":"+parsed.Action]++
		}
	}
	if actions["verbose_status:restored"] != 4 || actions["verbose_status:updated"] != 1 || actions["verbose_status:unchanged"] != 1 ||
		actions["verbose_status:deleted"] != 1 || actions["summary:"] != 1 {
		t.Errorf("the actions: %v", actions)
	}
	var status resticRestoreLine
	if err := json.Unmarshal(bytes.SplitN(fixture("restore-status.json"), []byte("\n"), 2)[0], &status); err != nil ||
		status.MessageType != "status" || status.TotalBytes != 60500000 || status.BytesRestored == 0 || status.PercentDone <= 0 {
		t.Errorf("the status: %+v %v", status, err)
	}
	errorLine := strings.SplitN(string(fixture("restore-errors.txt")), "\n", 2)[0]
	if readable(errorLine) != "remove /restore/0/f1.bin: directory not empty" {
		t.Errorf("the error: %s", readable(errorLine))
	}
	// the patterns that picked something
	for line := range strings.SplitSeq(string(fixture("restore-include.txt")), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pattern, picked, _ := strings.Cut(line, "\t")
		if picked == "nothing" {
			continue
		}
		if pattern == "/sub" && includePattern("/volumes/vx", "/volumes/vx/sub") != pattern {
			t.Errorf("a pick in a folder: %s", includePattern("/volumes/vx", "/volumes/vx/sub"))
		}
		if pattern == "/volumes/vx/sub" && includePattern("/", "/volumes/vx/sub") != pattern {
			t.Errorf("a pick in all of the snapshot: %s", includePattern("/", "/volumes/vx/sub"))
		}
	}
}

// TestSnapshotsKeepNewestAndReadSummary (#30): a job's snapshots are listed
// newest first, only those with every tag, at most the limit, with the
// total and what each saved.
func TestSnapshotsKeepNewestAndReadSummary(t *testing.T) {
	env, root := backupRepository(t)
	dir := t.TempDir()
	var ids []string
	for i := range 3 {
		writeFiles(t, dir, map[string]string{"a.txt": strings.Repeat("x", i+1)})
		ids = append(ids, snapshotOf(t, env, "/volumes/app", dir))
	}
	snapshotOf(t, env, "/volumes/app", dir, "pocketcloud", "job:bbbbbbbbbbbbbbb")
	out, _, err := runTool(t, env, nil, "snapshots", "--tag", "pocketcloud", "--tag", "job:aaaaaaaaaaaaaaa", "--limit", "2")
	if err != nil {
		t.Fatal(err)
	}
	var result snapshotsResult
	decodeLast(t, out, &result)
	if result.Total != 3 || len(result.Snapshots) != 2 || result.Snapshots[0].ID != ids[2] || result.Snapshots[1].ID != ids[1] {
		t.Fatalf("the list: %+v", result)
	}
	newest := result.Snapshots[0]
	if newest.ShortID != ids[2][:8] || newest.Hostname != "nas" || !slices.Equal(newest.Paths, []string{"/volumes/app"}) ||
		*newest.SizeBytes != 3 || *newest.Files != 1 || newest.AddedBytes == nil || newest.ProgramVersion != "restic 0.17.3" ||
		!strings.HasSuffix(newest.Time, "Z") || !slices.Contains(newest.Tags, "job:aaaaaaaaaaaaaaa") {
		t.Errorf("the newest: %+v", newest)
	}
	if !strings.Contains(fakerestic.Calls(t, root), " --no-lock snapshots --json --tag pocketcloud,job:aaaaaaaaaaaaaaa\n") {
		t.Errorf("restic ran:\n%s", fakerestic.Calls(t, root))
	}
	if _, _, err := runTool(t, env, nil, "snapshots", "--tag", "a,b"); err == nil {
		t.Error("a tag with a comma was taken")
	}
	if _, _, err := runTool(t, env, nil, "snapshots", "--limit", "1001"); err == nil {
		t.Error("a limit past 1000 was taken")
	}
}

// browseSnapshot is a snapshot of a folder with folders and files.
func browseSnapshot(t *testing.T) (map[string]string, string) {
	t.Helper()
	env, _ := backupRepository(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"b.txt": "bb", "a.txt": "a", "Zeta.log": "z", "docs/readme": "r", "conf/app.ini": "x=1", "conf/deep/x": "x"})
	return env, snapshotOf(t, env, "/volumes/app", dir)
}

// TestLsListsOnlyTheFoldersOwnEntries (#30): a folder lists what it holds,
// neither itself nor deeper, with each entry's type, size, mode and time.
func TestLsListsOnlyTheFoldersOwnEntries(t *testing.T) {
	env, id := browseSnapshot(t)
	out, _, err := runTool(t, env, nil, "ls", "--snapshot", id, "--dir", "/volumes/app/conf")
	if err != nil {
		t.Fatal(err)
	}
	var result lsResult
	decodeLast(t, out, &result)
	if result.Path != "/volumes/app/conf" || result.Total != 2 || len(result.Entries) != 2 ||
		result.Entries[0].Name != "deep" || result.Entries[0].Type != "dir" || result.Entries[1].Name != "app.ini" ||
		result.Entries[1].Size != 3 || result.Entries[1].Mode != 0o644 || result.Entries[1].MTime == "" {
		t.Errorf("the folder: %+v", result)
	}
	// the snapshot's top
	out, _, err = runTool(t, env, nil, "ls", "--snapshot", id, "--dir", "/")
	if err != nil {
		t.Fatal(err)
	}
	decodeLast(t, out, &result)
	if result.Total != 1 || result.Entries[0].Name != "volumes" {
		t.Errorf("the top: %+v", result)
	}
	if _, _, err := runTool(t, env, nil, "ls", "--snapshot", id, "--dir", "/volumes/gone"); err == nil || !strings.Contains(err.Error(), "/volumes/gone is not in the snapshot") {
		t.Errorf("a folder that is not there: %v", err)
	}
	if _, _, err := runTool(t, env, nil, "ls", "--snapshot", id, "--dir", "/volumes/app/a.txt"); err == nil || !strings.Contains(err.Error(), "is a file of the snapshot") {
		t.Errorf("a file: %v", err)
	}
	for _, bad := range [][]string{{"--snapshot", id[:8], "--dir", "/"}, {"--snapshot", id, "--dir", "volumes"}, {"--snapshot", id, "--dir", "/volumes/../etc"}} {
		if _, _, err := runTool(t, env, nil, append([]string{"ls"}, bad...)...); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}

// TestLsPagesDirectoriesFirstAndCounts (#30): folders come first, then
// files, each by name; a page starts at its offset, and the counts are of
// the whole folder; a folder past the limit is refused.
func TestLsPagesDirectoriesFirstAndCounts(t *testing.T) {
	env, id := browseSnapshot(t)
	names := func(offset, limit string) ([]string, lsResult) {
		out, _, err := runTool(t, env, nil, "ls", "--snapshot", id, "--dir", "/volumes/app", "--offset", offset, "--limit", limit)
		if err != nil {
			t.Fatal(err)
		}
		var result lsResult
		decodeLast(t, out, &result)
		var got []string
		for _, entry := range result.Entries {
			got = append(got, entry.Name)
		}
		return got, result
	}
	first, result := names("0", "3")
	if !slices.Equal(first, []string{"conf", "docs", "Zeta.log"}) || result.Total != 5 || result.Dirs != 2 || result.Files != 3 || result.Offset != 0 {
		t.Errorf("the first page: %q %+v", first, result)
	}
	if rest, result := names("3", "3"); !slices.Equal(rest, []string{"a.txt", "b.txt"}) || result.Offset != 3 {
		t.Errorf("the second page: %q", rest)
	}
	if none, result := names("9", "3"); len(none) != 0 || result.Total != 5 {
		t.Errorf("past the end: %q", none)
	}
	defer func(old int) { maxListed = old }(maxListed)
	maxListed = 4
	if _, _, err := runTool(t, env, nil, "ls", "--snapshot", id, "--dir", "/volumes/app"); !errors.Is(err, errTooManyEntries) {
		t.Errorf("a folder past the limit: %v", err)
	}
}

// TestLsFiltersByMatch (#30): a match keeps the names that hold it, in any
// case, and the counts are of what it kept.
func TestLsFiltersByMatch(t *testing.T) {
	env, id := browseSnapshot(t)
	out, _, err := runTool(t, env, nil, "ls", "--snapshot", id, "--dir", "/volumes/app", "--match", "T")
	if err != nil {
		t.Fatal(err)
	}
	var result lsResult
	decodeLast(t, out, &result)
	var got []string
	for _, entry := range result.Entries {
		got = append(got, entry.Name)
	}
	if !slices.Equal(got, []string{"Zeta.log", "a.txt", "b.txt"}) || result.Total != 3 || result.Dirs != 0 {
		t.Errorf("the match: %q %+v", got, result)
	}
}

// restoreArgsOf are the restore op's flags for one root.
func restoreArgsOf(id, source, target string, more ...string) []string {
	return append([]string{"restore", "--snapshot", id, "--root", source + "=" + target, "--dest", "app_data", "--expect-tag", "job:aaaaaaaaaaaaaaa"}, more...)
}

// TestRestoreRefusesAFilledDestinationUnconfirmed (#30, AC3): a destination
// that holds files is not written without --overwrite: the tool says so,
// and refuses before it says it is ready, so nothing would stop.
func TestRestoreRefusesAFilledDestinationUnconfirmed(t *testing.T) {
	env, root := backupRepository(t)
	source := t.TempDir()
	writeFiles(t, source, map[string]string{"a.txt": "v1"})
	id := snapshotOf(t, env, "/volumes/app", source)
	dest := t.TempDir()
	writeFiles(t, dest, map[string]string{"mine.txt": "keep", "sub/x": "keep"})
	out, log, err := runTool(t, env, strings.NewReader("go\n"), restoreArgsOf(id, "/volumes/app", dest, "--wait")...)
	if err == nil || err.Error() != "app_data holds 3 items and the restore was not confirmed to overwrite them: preview again" {
		t.Errorf("the restore: %v", err)
	}
	var result restoreResult
	decodeLast(t, out, &result)
	if result.Refused != "not_empty" || result.Roots[0].Entries != 3 || result.FilesRestored != 0 {
		t.Errorf("the result: %+v", result)
	}
	if strings.Contains(log, restoreReady) || len(restoreCalls(t, root)) != 0 {
		t.Errorf("the restore went on: %q %q", log, restoreCalls(t, root))
	}
	if files := readTree(t, dest); len(files) != 2 || files["mine.txt"] != "keep" {
		t.Errorf("the destination: %v", files)
	}
	// confirmed, it is written
	if _, log, err := runTool(t, env, nil, restoreArgsOf(id, "/volumes/app", dest, "--overwrite")...); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if files := readTree(t, dest); files["a.txt"] != "v1" || files["mine.txt"] != "keep" {
		t.Errorf("the destination: %v", files)
	}
}

// TestARefusalCountsOneItemInTheSingular (#30): a destination that holds one
// item says so as a sentence does, "1 item", and "it"; and restic's errors
// are counted the same way.
func TestARefusalCountsOneItemInTheSingular(t *testing.T) {
	env, _ := backupRepository(t)
	source := t.TempDir()
	writeFiles(t, source, map[string]string{"a.txt": "v1"})
	id := snapshotOf(t, env, "/volumes/app", source)
	dest := t.TempDir()
	writeFiles(t, dest, map[string]string{"mine.txt": "keep"})
	_, _, err := runTool(t, env, strings.NewReader("go\n"), restoreArgsOf(id, "/volumes/app", dest, "--wait")...)
	if err == nil || err.Error() != "app_data holds 1 item and the restore was not confirmed to overwrite it: preview again" {
		t.Errorf("the restore: %v", err)
	}
	if got := couldNotRestore(&restoreResult{Errors: 1}).Error(); got != "restic could not restore 1 item, so the destination holds part of the snapshot" {
		t.Errorf("one error: %s", got)
	}
	if got := couldNotRestore(&restoreResult{Errors: 2, ErrorSample: []string{"x: denied"}}).Error(); got != "restic could not restore 2 items, so the destination holds part of the snapshot; the first: x: denied" {
		t.Errorf("two errors: %s", got)
	}
}

// TestRestoreRefusesASnapshotWithoutTheJobTag (#30, SAFE): a snapshot of
// another job is never restored, nor dumped, under this job's name.
func TestRestoreRefusesASnapshotWithoutTheJobTag(t *testing.T) {
	env, root := backupRepository(t)
	source := t.TempDir()
	writeFiles(t, source, map[string]string{"a.txt": "v1"})
	id := snapshotOf(t, env, "/volumes/app", source, "pocketcloud", "job:bbbbbbbbbbbbbbb")
	_, _, err := runTool(t, env, nil, restoreArgsOf(id, "/volumes/app", t.TempDir())...)
	if err == nil || !strings.Contains(err.Error(), "is not one of this job's: it has no tag job:aaaaaaaaaaaaaaa") {
		t.Errorf("the restore: %v", err)
	}
	_, _, err = runTool(t, env, nil, "dump", "--snapshot", id, "--path", "/volumes/app", "--expect-tag", "job:aaaaaaaaaaaaaaa")
	if err == nil || !strings.Contains(err.Error(), "is not one of this job's") {
		t.Errorf("the dump: %v", err)
	}
	if strings.Contains(fakerestic.Calls(t, root), " dump ") || len(restoreCalls(t, root)) != 0 {
		t.Errorf("restic ran:\n%s", fakerestic.Calls(t, root))
	}
	gone := strings.Repeat("e", 64)
	if _, _, err := runTool(t, env, nil, restoreArgsOf(gone, "/volumes/app", t.TempDir())...); err == nil || !strings.Contains(err.Error(), "is not in the repository") {
		t.Errorf("a snapshot that is gone: %v", err)
	}
}

// syncBuffer is a buffer written and read at once.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(data)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestRestoreWaitsForGoBeforeRestic (#30): once it has checked everything,
// the tool says it is ready and restores nothing until the agent says go;
// anything else calls the restore off.
func TestRestoreWaitsForGoBeforeRestic(t *testing.T) {
	env, root := backupRepository(t)
	source := t.TempDir()
	writeFiles(t, source, map[string]string{"a.txt": "v1"})
	id := snapshotOf(t, env, "/volumes/app", source)
	for _, answer := range []string{"go", "abort"} {
		dest := t.TempDir()
		stdin, feed := io.Pipe()
		var stderr syncBuffer
		done := make(chan error, 1)
		go func() {
			done <- BackupTool(context.Background(), restoreArgsOf(id, "/volumes/app", dest, "--wait"), env, stdin, io.Discard, &stderr)
		}()
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(stderr.String(), restoreReady+"\n") {
			if time.Now().After(deadline) {
				t.Fatalf("the tool never said it was ready:\n%s", stderr.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		before := len(restoreCalls(t, root))
		time.Sleep(50 * time.Millisecond)
		if len(restoreCalls(t, root)) != before || len(readTree(t, dest)) != 0 {
			t.Fatal("restic ran before the go")
		}
		if _, err := io.WriteString(feed, answer+"\n"); err != nil {
			t.Fatal(err)
		}
		err := <-done
		switch answer {
		case "go":
			if err != nil || readTree(t, dest)["a.txt"] != "v1" || len(restoreCalls(t, root)) != before+1 {
				t.Errorf("after go: %v %v", err, readTree(t, dest))
			}
		default:
			if err == nil || !strings.Contains(err.Error(), "called off") || len(restoreCalls(t, root)) != before || len(readTree(t, dest)) != 0 {
				t.Errorf("after %s: %v", answer, err)
			}
		}
	}
}

// TestRestoreReplaceDeletesExtraFiles (#30, AC1): restored with replace, a
// destination is back to the snapshot: a changed file is reverted, a file
// added later is deleted, and a deleted one comes back.
func TestRestoreReplaceDeletesExtraFiles(t *testing.T) {
	env, root := backupRepository(t)
	source := t.TempDir()
	writeFiles(t, source, map[string]string{"a.txt": "v1", "c.txt": "c", "sub/d.txt": "d"})
	id := snapshotOf(t, env, "/volumes/app", source)
	dest := t.TempDir()
	writeFiles(t, dest, map[string]string{"a.txt": "v2", "b.txt": "added later", "sub/d.txt": "d", "sub/new/e": "e"})
	out, log, err := runTool(t, env, nil, restoreArgsOf(id, "/volumes/app", dest, "--overwrite", "--replace")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if files := readTree(t, dest); len(files) != 3 || files["a.txt"] != "v1" || files["c.txt"] != "c" || files["sub/d.txt"] != "d" {
		t.Errorf("the destination: %v", files)
	}
	var result restoreResult
	decodeLast(t, out, &result)
	if result.DryRun || len(result.Roots) != 1 || !result.Roots[0].Done || result.Roots[0].Destination != "app_data" ||
		result.FilesDeleted != 3 || result.FilesRestored == 0 || result.Partial || result.Errors != 0 {
		t.Errorf("the result: %+v", result)
	}
	calls := restoreCalls(t, root)
	if len(calls) != 1 || !strings.Contains(calls[0], "--no-cache --retry-lock 5m restore "+id+":/volumes/app --target "+dest+" --json -vv --overwrite always --delete") {
		t.Errorf("restic ran %q", calls)
	}
	if !strings.Contains(log, "Restored ") || !strings.Contains(log, "into app_data, and deleted 3") {
		t.Errorf("the log:\n%s", log)
	}

	// without --overwrite, replace is refused, even into an empty
	// destination, and a pick with it too
	if _, _, err := runTool(t, env, nil, restoreArgsOf(id, "/volumes/app", t.TempDir(), "--replace")...); err == nil || !strings.Contains(err.Error(), "it needs --overwrite") {
		t.Errorf("replace without overwrite: %v", err)
	}
	if _, _, err := runTool(t, env, nil, restoreArgsOf(id, "/volumes/app", dest, "--overwrite", "--replace", "--include", "/volumes/app/sub")...); err == nil {
		t.Error("replace with a pick was taken")
	}
}

// TestRestoreAppliesTheSnapshotRootsOwnerAndMode (#30, AC1): the folder a
// root restores into gets the snapshot folder's owner, mode and time, as a
// database's data directory needs them, which restic leaves as they were.
func TestRestoreAppliesTheSnapshotRootsOwnerAndMode(t *testing.T) {
	env, _ := backupRepository(t)
	source := t.TempDir()
	writeFiles(t, source, map[string]string{"PG_VERSION": "16"})
	then := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chmod(source, 0o2750|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(source, then, then); err != nil {
		t.Fatal(err)
	}
	id := snapshotOf(t, env, "/volumes/pgdata", source)
	dest := t.TempDir()
	if err := os.Chmod(dest, 0o777); err != nil { //nolint:gosec // a destination as Docker makes one
		t.Fatal(err)
	}
	if _, log, err := runTool(t, env, nil, restoreArgsOf(id, "/volumes/pgdata", dest)...); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o750 || info.Mode()&os.ModeSetgid == 0 || !info.ModTime().Equal(then) {
		t.Errorf("the destination: %v %v", info.Mode(), info.ModTime())
	}
	if readTree(t, dest)["PG_VERSION"] != "16" {
		t.Error("the files were not restored")
	}
}

// TestDryRunCountsAndCapsSamples (#30): a preview says what a restore would
// add, overwrite, keep and delete, with a few paths of each and how many it
// left out, and what the destination holds; it writes nothing.
func TestDryRunCountsAndCapsSamples(t *testing.T) {
	env, root := backupRepository(t)
	source := t.TempDir()
	writeFiles(t, source, map[string]string{"n1": "1", "n2": "22", "n3": "333", "same": "s", "changed": "new"})
	id := snapshotOf(t, env, "/volumes/app", source)
	dest := t.TempDir()
	writeFiles(t, dest, map[string]string{"same": "s", "changed": "old", "extra": "x"})
	before := readTree(t, dest)
	out, log, err := runTool(t, env, nil, restoreArgsOf(id, "/volumes/app", dest, "--dry-run", "--replace", "--sample", "2")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	var result previewResult
	decodeLast(t, out, &result)
	if !result.DryRun || result.Snapshot.ID != id || !slices.Equal(result.Snapshot.Paths, []string{"/volumes/app"}) {
		t.Errorf("the snapshot: %+v", result.Snapshot)
	}
	if result.Counts != (actionCounts{New: 3, Updated: 1, Unchanged: 1, Deleted: 1}) || result.Bytes.New != 6 || result.Bytes.Updated != 3 {
		t.Errorf("counts %+v bytes %+v", result.Counts, result.Bytes)
	}
	if len(result.Samples.New) != 2 || result.Truncated.New != 1 || !slices.Equal(result.Samples.Updated, []string{"changed"}) ||
		!slices.Equal(result.Samples.Deleted, []string{"extra"}) {
		t.Errorf("samples %+v truncated %+v", result.Samples, result.Truncated)
	}
	if len(result.Roots) != 1 || result.Roots[0].Entries != 3 || !result.Roots[0].InSnapshot || result.Roots[0].Destination != "app_data" ||
		result.Roots[0].UID == nil || result.TotalFiles != 5 {
		t.Errorf("the roots: %+v, %d files", result.Roots, result.TotalFiles)
	}
	if after := readTree(t, dest); len(after) != len(before) || after["changed"] != "old" || after["extra"] != "x" {
		t.Errorf("the preview changed the destination: %v", after)
	}
	calls := restoreCalls(t, root)
	if len(calls) != 1 || !strings.Contains(calls[0], "--no-lock restore "+id+":/volumes/app --target "+dest+" --dry-run -vv --json --overwrite always --delete") ||
		!strings.Contains(fakerestic.Calls(t, root), "RESTIC_PROGRESS_FPS=0.2") {
		t.Errorf("restic ran %q", calls)
	}

	// a root the snapshot lacks is said, not refused, and a missing
	// destination holds nothing
	out, _, err = runTool(t, env, nil, "restore", "--snapshot", id, "--root", "/volumes/app="+t.TempDir(), "--root", "/volumes/gone="+filepath.Join(t.TempDir(), "none"),
		"--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	decodeLast(t, out, &result)
	if len(result.Roots) != 2 || result.Roots[1].InSnapshot || result.Roots[1].Entries != 0 || len(result.Notes) != 1 ||
		!strings.Contains(result.Notes[0], "/volumes/gone is not in the snapshot") || result.Samples.New[0] != "changed" {
		t.Errorf("a root the snapshot lacks: %+v %q %q", result.Roots, result.Notes, result.Samples.New)
	}
	// the real restore refuses it
	if _, _, err := runTool(t, env, nil, "restore", "--snapshot", id, "--root", "/volumes/gone="+t.TempDir()); err == nil || !strings.Contains(err.Error(), "is not in the snapshot") {
		t.Errorf("a real restore of a root the snapshot lacks: %v", err)
	}
	// with several roots, a sample says its root; a long one is cut
	plan := restorePlan{restoreRoot: restoreRoot{source: "/volumes/app"}}
	if sample := samplePath(plan, "/sub/x", true); sample != "app/sub/x" {
		t.Errorf("a sample of several roots: %s", sample)
	}
	if sample := samplePath(plan, "/"+strings.Repeat("é", 400), false); utf8.RuneCountInString(sample) != maxSamplePath || !strings.HasSuffix(sample, "…") {
		t.Errorf("a long sample: %d characters", utf8.RuneCountInString(sample))
	}
}

// TestRestorePicksPathsUnderTheirRoots (#30): a pick restores only what it
// names, relative to its root, as restic takes it; a root none of the picks
// is in is left alone, and a pick in no root is refused.
func TestRestorePicksPathsUnderTheirRoots(t *testing.T) {
	env, root := backupRepository(t)
	source := t.TempDir()
	writeFiles(t, source, map[string]string{"keep/a": "a", "skip/b": "b"})
	id := snapshotOf(t, env, "/volumes/app", source)
	dest := t.TempDir()
	other := t.TempDir()
	if _, log, err := runTool(t, env, nil, "restore", "--snapshot", id, "--root", "/volumes/app="+dest, "--root", "/="+other,
		"--include", "/volumes/app/keep"); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if files := readTree(t, dest); len(files) != 1 || files["keep/a"] != "a" {
		t.Errorf("the destination: %v", files)
	}
	// all of the snapshot takes the same pick by its full path
	if files := readTree(t, other); len(files) != 1 || files["volumes/app/keep/a"] != "a" {
		t.Errorf("all of the snapshot: %v", files)
	}
	calls := restoreCalls(t, root)
	if len(calls) != 2 || !strings.HasSuffix(calls[0], " --include /keep") || !strings.HasSuffix(calls[1], " --include /volumes/app/keep") ||
		!strings.Contains(calls[1], " restore "+id+" --target ") {
		t.Errorf("restic ran %q", calls)
	}
	if _, _, err := runTool(t, env, nil, "restore", "--snapshot", id, "--root", "/volumes/app="+t.TempDir(), "--include", "/srv/x"); err == nil ||
		!strings.Contains(err.Error(), "/srv/x is in none of the folders") {
		t.Errorf("a pick in no root: %v", err)
	}
}

// globNames are files whose names restic's globs give a meaning, and those
// the globs would match instead (see testdata/restic-0.17.3).
var globNames = map[string]string{"file[1].txt": "1", "file1.txt": "2", "data*": "3", "datax": "4", "a[b": "5", "q?": "6", "qa": "7",
	"d[1]/in/c.txt": "8", "d1/e.txt": "9", "b]": "10", "b": "11"}

// TestAPickWithGlobCharactersRestoresOnlyItself (#30, AC2): restic takes a
// pick as globs; the tool hands it over with each character that means
// something to a glob standing for itself, so a picked file[1].txt
// restores that file and not file1.txt, data* not datax, and a[b, which is
// no glob, restores rather than fails.
func TestAPickWithGlobCharactersRestoresOnlyItself(t *testing.T) {
	env, _ := backupRepository(t)
	source := t.TempDir()
	writeFiles(t, source, globNames)
	id := snapshotOf(t, env, "/volumes/app", source)
	for pick, want := range map[string]map[string]string{
		"file[1].txt": {"file[1].txt": "1"},
		"data*":       {"data*": "3"},
		"a[b":         {"a[b": "5"},
		"q?":          {"q?": "6"},
		"d[1]":        {"d[1]/in/c.txt": "8"},
		"b]":          {"b]": "10"},
	} {
		dest := t.TempDir()
		if _, log, err := runTool(t, env, nil, restoreArgsOf(id, "/volumes/app", dest, "--include", "/volumes/app/"+pick)...); err != nil {
			t.Errorf("%s: %v\n%s", pick, err, log)
			continue
		}
		if files := readTree(t, dest); !maps.Equal(files, want) {
			t.Errorf("%s restored %v", pick, files)
		}
	}
	// a backslash too, and all of the snapshot's paths
	if pattern := includePattern("/volumes/app", `/volumes/app/x\y*`); pattern != `/x\\y\*` {
		t.Errorf("a backslash: %s", pattern)
	}
	if pattern := includePattern("/", "/volumes/app/d[1]/q?"); pattern != `/volumes/app/d\[1]/q\?` {
		t.Errorf("all of the snapshot: %s", pattern)
	}
}

// TestTheFakeResticPicksAsRestic0173Did: the fake restic's --include picks
// what restic 0.17.3 picked for the same patterns, globs and their escapes,
// as testdata/restic-0.17.3/restore-include.txt holds it, so that a test of
// a pick on the fake says what the real one does.
func TestTheFakeResticPicksAsRestic0173Did(t *testing.T) {
	env, _ := backupRepository(t)
	source := t.TempDir()
	files := maps.Clone(globNames)
	files[`x\y`], files["xy"] = "12", "13"
	writeFiles(t, source, files)
	id := snapshotOf(t, env, "/volumes/vx", source)
	captured, err := os.ReadFile(filepath.Join("testdata", "restic-0.17.3", "restore-include.txt"))
	if err != nil {
		t.Fatal(err)
	}
	_, globs, _ := strings.Cut(string(captured), "# globs:")
	checked := 0
	for line := range strings.SplitSeq(globs, "\n") {
		pattern, want, ok := strings.Cut(line, "\t")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		checked++
		target := filepath.Join(t.TempDir(), "t")
		command := exec.Command("restic", "restore", id+":/volumes/vx", "--target", target, "--include", pattern)
		command.Env = os.Environ()
		for name, value := range env {
			command.Env = append(command.Env, name+"="+value)
		}
		out, err := command.CombinedOutput()
		got := "nothing"
		switch {
		case err != nil:
			_, fatal, _ := strings.Cut(string(out), "Fatal: ")
			fatal, _, _ = strings.Cut(fatal, "\n")
			got = "error: Fatal: " + fatal
		default:
			var picked []string
			_ = filepath.WalkDir(target, func(file string, _ os.DirEntry, err error) error {
				if err == nil && file != target {
					rel, _ := filepath.Rel(target, file)
					picked = append(picked, filepath.ToSlash(rel))
				}
				return nil
			})
			slices.Sort(picked)
			if len(picked) > 0 {
				got = strings.Join(picked, " ")
			}
		}
		if got != want {
			t.Errorf("%s: the fake picked %q, restic %q", pattern, got, want)
		}
	}
	if checked != 14 {
		t.Errorf("%d patterns were checked", checked)
	}
}

// TestRestoreProgressMarksAreThrottled (#30): restic's status lines become
// the tool's progress lines, at most one per interval, with the bytes done
// of all the roots so far, and they stay out of what the log reads.
func TestRestoreProgressMarksAreThrottled(t *testing.T) {
	env, _ := backupRepository(t)
	source := t.TempDir()
	files := map[string]string{}
	for i := range 20 {
		files["f"+itoa(i)] = strings.Repeat("x", 100)
	}
	writeFiles(t, source, files)
	id := snapshotOf(t, env, "/volumes/app", source)
	defer func(old time.Duration) { restoreProgressEvery = old }(restoreProgressEvery)
	restoreProgressEvery = 150 * time.Millisecond
	t.Setenv("FAKE_RESTIC_SLOW_MS", "40")
	var marks []time.Time
	var progress []map[string]any
	var log bytes.Buffer
	stderr := &toolMarks{w: &log, progress: func(data []byte) {
		marks = append(marks, time.Now())
		var parsed map[string]any
		_ = json.Unmarshal(data, &parsed)
		progress = append(progress, parsed)
	}}
	if err := BackupTool(context.Background(), restoreArgsOf(id, "/volumes/app", t.TempDir()), env, nil, io.Discard, stderr); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if len(marks) < 2 || len(marks) > 8 {
		t.Fatalf("%d marks", len(marks))
	}
	for i := 1; i < len(marks); i++ {
		if gap := marks[i].Sub(marks[i-1]); gap < 140*time.Millisecond {
			t.Errorf("two marks %v apart", gap)
		}
	}
	last := progress[len(progress)-1]
	if last["unit"] != "bytes" || last["total"] != float64(2000) || last["done"].(float64) <= 0 || last["roots"] != float64(1) || last["root"] != float64(0) {
		t.Errorf("the progress: %v", progress)
	}
	if strings.Contains(log.String(), progressPrefix) || strings.Contains(log.String(), "status") {
		t.Errorf("the log: %q", log.String())
	}
}

// TestRestoreUnlocksStaleLocksAndRetriesOnce (#30): a restore that finds
// the repository locked before it restored anything runs restic unlock and
// tries once more, as a backup does; a lock that stays fails it.
func TestRestoreUnlocksStaleLocksAndRetriesOnce(t *testing.T) {
	env, root := backupRepository(t)
	source := t.TempDir()
	writeFiles(t, source, map[string]string{"a": "1"})
	id := snapshotOf(t, env, "/volumes/app", source)
	t.Setenv("FAKE_RESTIC_LOCKED_RESTORE", "1")
	dest := t.TempDir()
	if _, log, err := runTool(t, env, nil, restoreArgsOf(id, "/volumes/app", dest)...); err != nil || readTree(t, dest)["a"] != "1" {
		t.Fatalf("%v\n%s", err, log)
	}
	if commands := resticCommands(t, root); !slices.Equal(commands[len(commands)-6:], []string{"version", "snapshots", "ls", "restore", "unlock", "restore"}) ||
		fakerestic.Unlocks(t, root) != 1 {
		t.Errorf("restic ran %q", commands)
	}
	t.Setenv("FAKE_RESTIC_LOCKED_RESTORE", "3")
	if _, _, err := runTool(t, env, nil, restoreArgsOf(id, "/volumes/app", t.TempDir())...); err == nil || err.Error() != lockedLine[:strings.Index(lockedLine, "exclusively")]+"by PID 1 on other-server by root (UID 0, GID 0)" {
		t.Errorf("a lock that stays: %v", err)
	}
	if fakerestic.Unlocks(t, root) != 2 {
		t.Errorf("%d unlocks", fakerestic.Unlocks(t, root))
	}
}

// TestRestoreRefusesResticBefore017 (#30): restore, ls and dump need the
// restic whose output the tool reads; an older one is refused before it
// runs any of them.
func TestRestoreRefusesResticBefore017(t *testing.T) {
	env, root := backupRepository(t)
	source := t.TempDir()
	writeFiles(t, source, map[string]string{"a": "1"})
	id := snapshotOf(t, env, "/volumes/app", source)
	t.Setenv("FAKE_RESTIC_VERSION", "0.16.4")
	for _, args := range [][]string{
		restoreArgsOf(id, "/volumes/app", t.TempDir()),
		{"ls", "--snapshot", id, "--dir", "/"},
		{"dump", "--snapshot", id, "--path", "/volumes/app"},
	} {
		if _, _, err := runTool(t, env, nil, args...); err == nil || !strings.Contains(err.Error(), "restic 0.16.4 is too old to restore: the agent's image needs restic 0.17 or later") {
			t.Errorf("%s: %v", args[0], err)
		}
	}
	for _, command := range resticCommands(t, root) {
		if command == "restore" || command == "ls" || command == "dump" {
			t.Errorf("restic ran %s", command)
		}
	}
}

// timedWriter notes when its first and last writes came.
type timedWriter struct {
	buf         bytes.Buffer
	first, last time.Time
	writes      int
}

func (w *timedWriter) Write(data []byte) (int, error) {
	if w.first.IsZero() {
		w.first = time.Now()
	}
	w.last = time.Now()
	w.writes++
	return w.buf.Write(data)
}

// TestDumpStreamsStdoutUnbuffered (#30): a dump reaches the tool's output as
// restic writes it, not once restic ends: a file as it is, a folder as a tar
// of what it holds named by their snapshot paths.
func TestDumpStreamsStdoutUnbuffered(t *testing.T) {
	env, root := backupRepository(t)
	source := t.TempDir()
	big := bytes.Repeat([]byte("0123456789abcdef"), 20<<10)
	writeFiles(t, source, map[string]string{"big.bin": string(big), "sub/small.txt": "small"})
	id := snapshotOf(t, env, "/volumes/app", source)
	t.Setenv("FAKE_RESTIC_SLOW_MS", "60")
	out := &timedWriter{}
	var stderr bytes.Buffer
	started := time.Now()
	if err := BackupTool(context.Background(), []string{"dump", "--snapshot", id, "--path", "/volumes/app/big.bin", "--expect-tag", "job:aaaaaaaaaaaaaaa"}, env, nil, out, &stderr); err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	if !bytes.Equal(out.buf.Bytes(), big) {
		t.Errorf("the file: %d bytes", out.buf.Len())
	}
	if out.writes < 2 || out.last.Sub(out.first) < 150*time.Millisecond || out.first.Sub(started) > out.last.Sub(started)/2 {
		t.Errorf("%d writes, the first after %v, the last after %v", out.writes, out.first.Sub(started), out.last.Sub(started))
	}
	if !strings.Contains(fakerestic.Calls(t, root), " --retry-lock 5m dump --archive tar "+id+" /volumes/app/big.bin\n") {
		t.Errorf("restic ran:\n%s", fakerestic.Calls(t, root))
	}

	// a folder, as a tar
	t.Setenv("FAKE_RESTIC_SLOW_MS", "")
	out = &timedWriter{}
	if err := BackupTool(context.Background(), []string{"dump", "--snapshot", id, "--path", "/volumes/app"}, env, nil, out, &stderr); err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(&out.buf)
	entries := map[string]string{}
	for {
		header, err := archive.Next()
		if err != nil {
			break
		}
		content, _ := io.ReadAll(archive)
		entries[header.Name] = string(content)
	}
	if len(entries) != 3 || entries["volumes/app/sub/small.txt"] != "small" || entries["volumes/app/big.bin"] != string(big) {
		names := []string{}
		for name := range entries {
			names = append(names, name)
		}
		t.Errorf("the tar: %q", names)
	}
	if _, _, err := runTool(t, env, nil, "dump", "--snapshot", id, "--path", "/volumes/app/gone"); err == nil || !strings.Contains(err.Error(), "not found in snapshot") {
		t.Errorf("a path that is not there: %v", err)
	}
}

// TestTheRestoreOpChecksItsFlags (#30): the restore names its snapshot in
// full, each root once, at most 32 of them and 100 picks, each clean.
func TestTheRestoreOpChecksItsFlags(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, bad := range [][]string{
		{"--snapshot", id[:8], "--root", "/v=/restore/0"},
		{"--snapshot", id},
		{"--snapshot", id, "--root", "/v"},
		{"--snapshot", id, "--root", "/v=restore/0"},
		{"--snapshot", id, "--root", "v=/restore/0"},
		{"--snapshot", id, "--root", "/v=/restore/0", "--root", "/w=/restore/0/x"},
		{"--snapshot", id, "--root", "/v=/restore/0", "--dest", "a", "--dest", "b"},
		{"--snapshot", id, "--root", "/v=/restore/0", "--include", "rel"},
		{"--snapshot", id, "--root", "/v=/restore/0", "--expect-tag", "job:x"},
		{"--snapshot", id, "--root", "/v=/restore/0", "--sample", "-1"},
	} {
		if _, err := restoreRequestOf(flagValue(bad, "--snapshot"), flagValues(bad, "--root"), flagValues(bad, "--dest"), flagValues(bad, "--include"),
			false, false, false, false, sampleOf(bad), flagValue(bad, "--expect-tag")); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	var roots, includes []string
	for i := range maxRestoreRoots + 1 {
		roots = append(roots, "/v"+itoa(i)+"=/restore/"+itoa(i))
	}
	if _, err := restoreRequestOf(id, roots, nil, nil, false, false, false, false, defaultSample, ""); err == nil {
		t.Error("33 roots were taken")
	}
	for range maxRestoreIncludes + 1 {
		includes = append(includes, "/v/x")
	}
	if _, err := restoreRequestOf(id, roots[:1], nil, includes, false, false, false, false, defaultSample, ""); err == nil {
		t.Error("101 picks were taken")
	}
	if request, err := restoreRequestOf(id, []string{"=/restore/0", "/=/restore/1"}, []string{"vw_data", "/srv/x"}, nil, true, true, false, true, defaultSample, "job:aaaaaaaaaaaaaaa"); err != nil ||
		request.roots[0].source != "" || request.roots[1].label != "/srv/x" {
		t.Errorf("a good restore: %+v %v", request, err)
	}
}

// flagValues are the values of a repeated flag.
func flagValues(args []string, name string) []string {
	var values []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			values = append(values, args[i+1])
		}
	}
	return values
}

func flagValue(args []string, name string) string {
	values := flagValues(args, name)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// sampleOf is the --sample of a restore's flags.
func sampleOf(args []string) int {
	if value := flagValue(args, "--sample"); value != "" {
		n, _ := strconv.Atoi(value)
		return n
	}
	return defaultSample
}
