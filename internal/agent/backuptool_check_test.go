package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
	"github.com/oldbear24/PocketServerManager/internal/testutil/resticcapture"
)

// replayScript is a restic that says it is REPLAY_VERSION, 0.17.3 by
// default, and answers each other call with the next run kept in REPLAY_DIR,
// as restic printed it; it notes each call's arguments in its calls file.
const replayScript = `#!/bin/sh
dir="${REPLAY_DIR:?}"
echo "$*" >> "$dir/calls"
case " $* " in
*" version "*)
	echo "restic ${REPLAY_VERSION:-0.17.3} compiled with go1.23.9 on linux/amd64"
	exit 0;;
esac
n=$(cat "$dir/next" 2>/dev/null || echo 0)
echo $((n + 1)) > "$dir/next"
if [ ! -f "$dir/$n.exit" ]; then
	echo "Fatal: the replay has no run $n" >&2
	exit 1
fi
cat "$dir/$n.stdout"
cat "$dir/$n.stderr" >&2
exit "$(cat "$dir/$n.exit")"
`

// replayRestic puts on PATH a restic that answers with runs, one per call
// but version, and returns its directory.
func replayRestic(t *testing.T, runs ...resticcapture.Run) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "restic"), []byte(replayScript), 0o755); err != nil { //nolint:gosec // a script
		t.Fatal(err)
	}
	for i, run := range runs {
		for name, content := range map[string]string{"stdout": run.Stdout, "stderr": run.Stderr, "exit": strconv.Itoa(run.Exit)} {
			if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(i)+"."+name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REPLAY_DIR", dir)
	return dir
}

// replayed are the calls the replayed restic got.
func replayed(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "calls")) //nolint:gosec // the test's own
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// capturedRun is run i of a capture file.
func capturedRun(t *testing.T, file string, i int) resticcapture.Run {
	t.Helper()
	runs, err := resticcapture.Read(file)
	if err != nil {
		t.Fatal(err)
	}
	return runs[i]
}

// checkEnv is a repository's environment for the check op, with a storage's
// secret.
var checkEnv = map[string]string{"RESTIC_REPOSITORY": "s3:http://minio:9000/backups", "RESTIC_PASSWORD": "pw-check-0123",
	"AWS_ACCESS_KEY_ID": "id-check", "AWS_SECRET_ACCESS_KEY": "secret-check-0123"}

// toolCheck runs the check op and returns the pass it printed last, the
// progress marks it said, its log without them, and its error. The last line
// must be the pass, whatever the error.
func toolCheck(t *testing.T, env map[string]string, args ...string) (backup.CheckPass, []toolProgress, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := BackupTool(context.Background(), append([]string{"check"}, args...), env, nil, &stdout, &stderr)
	var pass backup.CheckPass
	if decodeErr := json.Unmarshal(lastLine(stdout.Bytes()), &pass); decodeErr != nil {
		t.Fatalf("the last line %q: %v (the error: %v)", stdout.String(), decodeErr, err)
	}
	var marks []toolProgress
	var log strings.Builder
	for line := range strings.SplitSeq(stderr.String(), "\n") {
		if data, ok := progressOf(line); ok {
			var mark toolProgress
			if json.Unmarshal(data, &mark) != nil {
				t.Fatalf("a progress line %q", line)
			}
			marks = append(marks, mark)
		} else if line != "" {
			log.WriteString(line + "\n")
		}
	}
	return pass, marks, log.String(), err
}

// TestTheCheckOpRunsResticAsItShould (#31): pass 1 runs restic --no-lock
// check of its group, pass 2 the same under the exclusive lock, waiting 30
// minutes for it; a structure-only check reads no data; restic keeps its
// temporary cache in the job's /tmp, never in the shared cache, says its
// counters every 5 seconds, and never runs --quiet or --json, whose text the
// tool reads.
func TestTheCheckOpRunsResticAsItShould(t *testing.T) {
	root := fakerestic.Install(t)
	env := map[string]string{"RESTIC_REPOSITORY": "rest:http://backup-node:8000/check/", "RESTIC_PASSWORD": "pw-check"}
	if _, _, err := runBackupTool(t, env, "init"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--subset", "3/20", "--pass", "free", "--cache", "/cache"},
		{"--subset", "3/20", "--pass", "confirm"},
		{"--subset", "", "--pass", "free"},
	} {
		if pass, _, log, err := toolCheck(t, env, args...); err != nil || pass.Outcome != backup.OutcomeOK {
			t.Fatalf("%q: %+v %v\n%s", args, pass, err, log)
		}
	}
	var checks []string
	for line := range strings.SplitSeq(fakerestic.Calls(t, root), "\n") {
		if strings.HasPrefix(line, "restic ") && strings.Contains(line, " check") {
			checks = append(checks, line)
		}
	}
	want := []string{
		"restic --cache-dir /tmp/pocketcloud-check-cache --cleanup-cache --no-lock check --read-data-subset=3/20",
		"restic --cache-dir /tmp/pocketcloud-check-cache --cleanup-cache --retry-lock 30m check --read-data-subset=3/20",
		"restic --cache-dir /tmp/pocketcloud-check-cache --cleanup-cache --no-lock check",
	}
	if !slices.Equal(checks, want) {
		t.Errorf("restic ran\n%s\nwant\n%s", strings.Join(checks, "\n"), strings.Join(want, "\n"))
	}
	calls := fakerestic.Calls(t, root)
	if strings.Count(calls, "RESTIC_PROGRESS_FPS=0.2\n") != 3 || strings.Contains(calls, "--quiet") || strings.Contains(calls, "--json check") || strings.Contains(calls, "/cache ") {
		t.Errorf("restic ran:\n%s", calls)
	}
}

// TestTheCheckOpReadsWhatRestic0173Printed (#31): each pass's outcome, its
// errors, the repairs restic advised, its notes and its packs, from what
// restic 0.17.3 printed, and the last line says it whatever the outcome.
func TestTheCheckOpReadsWhatRestic0173Printed(t *testing.T) {
	packID := "b6597323646d9a1a0b87300c57c625cffe338cb24e7cc9a47a680d320c992870"
	missing := "b538140e7a2b97fce36af1fa9e4e8db9918a68abe0e7a9debd752d60bca79a33"
	note := "1 additional files were found in the repo, which likely contain duplicate data. This is non-critical, you can run `restic prune` to correct this."
	for _, c := range []struct {
		file             string
		run              int
		args             []string
		outcome          string
		code             int
		read, total      int64
		kinds            []string
		ids              []string
		more             int
		repair           backup.CheckRepair
		notes            []string
		err, passErr     string
		textHas, textNot string
	}{
		{file: "check-ok.txt", args: []string{"--subset", "1/20", "--pass", "free"}, outcome: backup.OutcomeOK, read: 4, total: 80},
		{file: "check-packs.txt", args: []string{"--subset", "3/20", "--pass", "free"}, outcome: backup.OutcomeErrors, code: 1, read: 1, total: 80,
			kinds: []string{"pack"}, ids: []string{packID}, repair: backup.CheckRepair{Packs: []string{packID}, Snapshots: true},
			err: "restic found 1 errors", textHas: "pack " + packID + " contains 2 errors: [blob 21331c4e"},
		{file: "check-packs.txt", run: 1, args: []string{"--subset", "3/20", "--pass", "confirm"}, outcome: backup.OutcomeErrors, code: 1, read: 1, total: 80,
			kinds: []string{"pack"}, ids: []string{packID}, repair: backup.CheckRepair{Packs: []string{packID}, Snapshots: true}, err: "restic found 1 errors"},
		{file: "check-index.txt", args: []string{"--subset", "1/20", "--pass", "free"}, outcome: backup.OutcomeErrors, code: 1,
			kinds: []string{"other"}, ids: []string{""}, repair: backup.CheckRepair{Index: true}, err: "restic found 1 errors",
			textHas: "error: error loading index 1010d92b"},
		{file: "check-tree.txt", args: []string{"--subset", "", "--pass", "free"}, outcome: backup.OutcomeErrors, code: 1,
			kinds: []string{"tree"}, ids: []string{"07055eee"}, repair: backup.CheckRepair{Other: true}, err: "restic found 1 errors",
			textHas: "error for tree 07055eee: decrypting blob <tree/07055eee> from 2840f0de failed: ciphertext verification failed"},
		{file: "check-missing.txt", args: []string{"--subset", "2/20", "--pass", "free"}, outcome: backup.OutcomeErrors, code: 1, read: 5, total: 80,
			kinds: []string{"pack", "other"}, ids: []string{missing, ""}, repair: backup.CheckRepair{Other: true}, err: "restic found 2 errors",
			textNot: "Load(<data/"},
		{file: "check-nolock-concurrent-prune.txt", args: []string{"--subset", "1/2", "--pass", "free"}, outcome: backup.OutcomeErrors, code: 1, read: 45, total: 80,
			kinds: slices.Repeat([]string{"other"}, 20), ids: slices.Repeat([]string{""}, 20), more: 2, repair: backup.CheckRepair{Other: true},
			notes: []string{note}, err: "restic found 22 errors", textHas: "download error: open /repo/data/", textNot: "Load(<data/"},
		{file: "check-invalid-lock.txt", args: []string{"--subset", "1/20", "--pass", "confirm"}, outcome: backup.OutcomeLockInvalid, code: 1,
			passErr: "Fatal: invalid lock file: LoadRaw(<lock/5a09675764>): invalid data returned"},
		{file: "check-invalid-lock.txt", run: 1, args: []string{"--subset", "1/20", "--pass", "free"}, outcome: backup.OutcomeOK, read: 4, total: 80, notes: []string{note}},
		{file: "check-readonly.txt", run: 1, args: []string{"--subset", "1/20", "--pass", "confirm"}, outcome: backup.OutcomeError, code: 1,
			passErr: "unable to create lock in backend: open /repo/locks/7551026be9fc82d56fb4bda95690828a7235c844d750152b160722afed5600ae-tmp-3116456346: read-only file system"},
		{file: "check-repository.txt", args: []string{"--subset", "", "--pass", "free"}, outcome: backup.OutcomeError, code: 12,
			passErr: "a repository is there, and the password is not its own: Fatal: wrong password or no key found"},
		{file: "check-repository.txt", run: 1, args: []string{"--subset", "", "--pass", "free"}, outcome: backup.OutcomeError, code: 10,
			passErr: "the repository is gone, with its snapshots: Fatal: repository does not exist"},
		{file: "check-empty.txt", args: []string{"--subset", "1/20", "--pass", "free"}, outcome: backup.OutcomeOK},
	} {
		name := c.file + "#" + strconv.Itoa(c.run)
		replayRestic(t, capturedRun(t, c.file, c.run))
		pass, _, log, err := toolCheck(t, checkEnv, c.args...)
		var kinds, ids []string
		for _, entry := range pass.Errors {
			kinds, ids = append(kinds, entry.Kind), append(ids, entry.ID)
		}
		wantRepair := c.repair
		if wantRepair.Packs == nil {
			wantRepair.Packs = []string{}
		}
		wantNotes := c.notes
		if wantNotes == nil {
			wantNotes = []string{}
		}
		switch {
		case pass.Outcome != c.outcome || pass.ResticCode != c.code || pass.PacksRead != c.read || pass.PacksTotal != c.total || pass.Subset != c.args[1] || pass.Pass != c.args[3]:
			t.Errorf("%s: %+v", name, pass)
		case !slices.Equal(kinds, c.kinds) || !slices.Equal(ids, c.ids) || pass.ErrorsMore != c.more:
			t.Errorf("%s: errors %q %q, %d more", name, kinds, ids, pass.ErrorsMore)
		case pass.Repair.Index != wantRepair.Index || !slices.Equal(pass.Repair.Packs, wantRepair.Packs) || pass.Repair.Snapshots != wantRepair.Snapshots || pass.Repair.Other != wantRepair.Other:
			t.Errorf("%s: repair %+v, want %+v", name, pass.Repair, wantRepair)
		case !slices.Equal(pass.Notes, wantNotes):
			t.Errorf("%s: notes %q", name, pass.Notes)
		case c.err == "" && c.passErr == "" && (err != nil || pass.Error != ""):
			t.Errorf("%s: %v %q", name, err, pass.Error)
		case c.err != "" && (err == nil || err.Error() != c.err || pass.Error != ""):
			t.Errorf("%s: the error %v, the pass's %q", name, err, pass.Error)
		case c.passErr != "" && (err == nil || !strings.HasPrefix(pass.Error, c.passErr) || err.Error() != pass.Error):
			t.Errorf("%s: the error %v, the pass's %q", name, err, pass.Error)
		}
		texts := ""
		for _, entry := range pass.Errors {
			texts += entry.Text + "\n"
		}
		if c.textHas != "" && !strings.Contains(texts, c.textHas) || c.textNot != "" && strings.Contains(texts, c.textNot) {
			t.Errorf("%s: the errors say\n%s", name, texts)
		}
		// what restic said is in the log, but its phases and counters
		for _, said := range []string{"load indexes", "check all packs", "check snapshots", "read group", "[0:0", "create exclusive lock"} {
			if strings.Contains(log, said) {
				t.Errorf("%s: the log says %q:\n%s", name, said, log)
			}
		}
		if c.outcome == backup.OutcomeErrors && !strings.Contains(log, "Fatal: repository contains errors\n") {
			t.Errorf("%s: the log:\n%s", name, log)
		}
	}
}

// TestACheckThatFindsTheRepositoryLockedSaysTheLock (#31): pass 2 that finds
// the repository locked, after its wait, runs restic unlock and tries once
// more, as a backup does; still locked, it says the lock restic named last,
// dated from now; with the server's clock off, it removes nothing.
func TestACheckThatFindsTheRepositoryLockedSaysTheLock(t *testing.T) {
	locked := capturedRun(t, "check-locked.txt", 0)
	shared := capturedRun(t, "check-locked.txt", 1)
	unlocked := resticcapture.Run{}
	dir := replayRestic(t, locked, unlocked, shared)
	pass, marks, log, err := toolCheck(t, checkEnv, "--subset", "1/20", "--pass", "confirm")
	held := "unable to create lock in backend: repository is already locked by PID 23062 on nas by root (UID 0, GID 0)"
	if err == nil || err.Error() != held || pass.Outcome != backup.OutcomeLocked || pass.ResticCode != 11 || pass.Error != held || len(pass.Errors) != 0 {
		t.Errorf("%+v %v", pass, err)
	}
	if lock := pass.Lock; lock == nil || lock.Host != "nas" || lock.PID != 23062 || lock.Exclusive || lock.StorageID != "4e44bf94" || lock.AgeS != 21 || lock.Created.IsZero() {
		t.Errorf("the lock: %+v", pass.Lock)
	}
	calls := replayed(t, dir)
	if len(calls) != 4 || !strings.HasSuffix(calls[1], " check --read-data-subset=1/20") || calls[2] != "--no-cache unlock" || calls[3] != calls[1] {
		t.Errorf("restic ran %q", calls)
	}
	if !strings.Contains(log, lockedRetry+"\n") {
		t.Errorf("the log:\n%s", log)
	}
	if len(marks) == 0 || marks[0].Step != "lock" {
		t.Errorf("the marks: %+v", marks)
	}

	// with the server's clock off, no lock is removed, and pass 2 tries
	// once more all the same
	dir = replayRestic(t, locked, locked)
	pass, _, log, _ = toolCheck(t, checkEnv, "--subset", "1/20", "--pass", "confirm", "--no-unlock=11")
	if calls := replayed(t, dir); len(calls) != 3 || slices.ContainsFunc(calls, func(call string) bool { return strings.Contains(call, "unlock") }) ||
		!strings.Contains(log, "The server's clock is off by 11 min") || pass.Lock == nil || pass.Lock.Host != "pc-k3j9x2m4q8w1z7v" {
		t.Errorf("restic ran %q: %+v\n%s", calls, pass, log)
	}

	// pass 1 takes no lock, and never unlocks
	dir = replayRestic(t, locked)
	if pass, _, _, _ := toolCheck(t, checkEnv, "--subset", "1/20", "--pass", "free"); pass.Outcome != backup.OutcomeLocked || len(replayed(t, dir)) != 2 {
		t.Errorf("pass 1: %+v %q", pass, replayed(t, dir))
	}
}

// TestACheckSaysHowFarItGot (#31): restic's phases and counters become the
// tool's progress marks, the step's share of the pass with what of it is
// done, and never lines of the log, which keeps what else restic said.
func TestACheckSaysHowFarItGot(t *testing.T) {
	replayRestic(t, capturedRun(t, "check-progress.txt", 0))
	pass, marks, log, err := toolCheck(t, checkEnv, "--subset", "2/2", "--pass", "free")
	if err != nil || pass.PacksRead != 35 || pass.PacksTotal != 80 {
		t.Fatalf("%+v %v", pass, err)
	}
	want := []toolProgress{
		{Step: "index"}, {Step: "packs", Percent: 5}, {Step: "snapshots", Percent: 15},
		{Step: "snapshots", Percent: 45, Done: 40, Total: 40, Unit: "snapshots"},
		{Step: "read", Percent: 45, Total: 35, Unit: "packs"},
		{Step: "read", Percent: 87, Done: 27, Total: 35, Unit: "packs"},
		{Step: "read", Percent: 98, Done: 34, Total: 35, Unit: "packs"},
		{Step: "read", Percent: 100, Done: 35, Total: 35, Unit: "packs"},
	}
	if !slices.EqualFunc(marks, want, func(a, b toolProgress) bool {
		return a.Step == b.Step && a.Percent == b.Percent && a.Done == b.Done && a.Total == b.Total && a.Unit == b.Unit
	}) {
		t.Errorf("marks %+v\nwant %+v", marks, want)
	}
	if log != "using temporary cache in /tmp/pocketcloud-check-cache/restic-check-cache-2028128584\nno errors were found\n" {
		t.Errorf("the log: %q", log)
	}

	// the structure alone takes the pass to its end; a counter with nothing
	// to count ends its step
	replayRestic(t, capturedRun(t, "check-tree.txt", 0))
	if _, marks, _, _ := toolCheck(t, checkEnv, "--subset", "", "--pass", "free"); len(marks) != 4 || marks[3].Percent != 100 || marks[3].Step != "snapshots" {
		t.Errorf("the structure's marks: %+v", marks)
	}
	replayRestic(t, capturedRun(t, "check-empty.txt", 0))
	if _, marks, _, _ := toolCheck(t, checkEnv, "--subset", "1/20", "--pass", "free"); len(marks) != 6 || marks[3].Percent != 45 || marks[3].Done != 0 ||
		marks[5].Percent != 100 || marks[5].Unit != "packs" {
		t.Errorf("an empty repository's marks: %+v", marks)
	}
}

// TestACheckKeepsTwentyErrorsWithoutSecrets (#31): a pass keeps at most 20
// errors of at most 500 characters, counts the rest, and takes the
// repository's password and the storage's credentials out of them, of its
// error and of the log.
func TestACheckKeepsTwentyErrorsWithoutSecrets(t *testing.T) {
	var stderr strings.Builder
	for i := range 25 {
		id := strings.Repeat(strconv.FormatInt(int64(i%16), 16), 64)
		stderr.WriteString("pack " + id + ": does not exist at s3:http://minio:9000/backups?key=" + checkEnv["AWS_SECRET_ACCESS_KEY"] + "\n")
	}
	stderr.WriteString("error for tree 0011aabb:\n  " + strings.Repeat("é", 600) + "\n")
	stderr.WriteString("\nThe repository is damaged and must be repaired. Please follow the troubleshooting guide.\n\nFatal: repository contains errors\n")
	replayRestic(t, resticcapture.Run{Stdout: "load indexes\n", Stderr: stderr.String(), Exit: 1})
	pass, _, log, err := toolCheck(t, checkEnv, "--subset", "1/20", "--pass", "free")
	if err == nil || err.Error() != "restic found 26 errors" || len(pass.Errors) != 20 || pass.ErrorsMore != 6 || !pass.Repair.Other {
		t.Fatalf("%v: %d errors, %d more", err, len(pass.Errors), pass.ErrorsMore)
	}
	for _, entry := range pass.Errors {
		if strings.Contains(entry.Text, checkEnv["AWS_SECRET_ACCESS_KEY"]) || !strings.Contains(entry.Text, "key=***") || entry.Kind != "pack" {
			t.Errorf("an error: %+v", entry)
		}
	}
	if strings.Contains(log, checkEnv["AWS_SECRET_ACCESS_KEY"]) || !strings.Contains(log, "key=***") {
		t.Errorf("the log:\n%s", log)
	}

	// one long error is cut to 500 characters
	replayRestic(t, resticcapture.Run{Stderr: "error for tree 0011aabb:\n  " + strings.Repeat("é", 600) + "\n  and more\nFatal: repository contains errors\n", Exit: 1})
	pass, _, _, _ = toolCheck(t, checkEnv, "--subset", "", "--pass", "free")
	if len(pass.Errors) != 1 || utf8.RuneCountInString(pass.Errors[0].Text) != backup.MaxCheckErrorText || !strings.HasSuffix(pass.Errors[0].Text, "é…") ||
		!strings.HasPrefix(pass.Errors[0].Text, "error for tree 0011aabb: éé") || pass.Errors[0].ID != "0011aabb" {
		t.Errorf("a long error: %+v", pass.Errors)
	}

	// what an error outcome says, too
	replayRestic(t, resticcapture.Run{Stderr: "Fatal: unable to open repository at s3:http://minio:9000/backups: Access Denied for key " + checkEnv["AWS_SECRET_ACCESS_KEY"] + "\n", Exit: 1})
	pass, _, log, err = toolCheck(t, checkEnv, "--subset", "1/20", "--pass", "free")
	if pass.Outcome != backup.OutcomeError || err == nil || strings.Contains(err.Error(), "secret-check") || strings.Contains(pass.Error, "secret-check") ||
		!strings.Contains(pass.Error, "Access Denied for key ***") || strings.Contains(log, "secret-check") {
		t.Errorf("an error: %+v %v\n%s", pass, err, log)
	}
}

// TestTheCheckOpRefusesWhatItCannotRead (#31): a subset other than a group
// n/t, as restic's percentages and sizes pick packs at random and pass 2
// would read other packs than pass 1, a pass of another name, and a restic
// that is not 0.17, whose check's text the tool cannot read; each is said
// as the pass's error, and no check runs.
func TestTheCheckOpRefusesWhatItCannotRead(t *testing.T) {
	dir := replayRestic(t)
	for _, c := range []struct{ subset, pass, want string }{
		{"5%", "free", "is not group n/t of the data packs"},
		{"500M", "free", "is not group n/t of the data packs"},
		{"0/20", "free", "is not group n/t of the data packs"},
		{"21/20", "free", "1 ≤ n ≤ t ≤ 256"},
		{"1/257", "free", "1 ≤ n ≤ t ≤ 256"},
		{"03/20", "free", "is not group n/t of the data packs"},
		{"1/20", "", "a check's --pass is free or confirm"},
		{"1/20", "both", "a check's --pass is free or confirm"},
	} {
		pass, _, _, err := toolCheck(t, checkEnv, "--subset", c.subset, "--pass", c.pass)
		if err == nil || !strings.Contains(err.Error(), c.want) || pass.Outcome != backup.OutcomeError || pass.Error != err.Error() {
			t.Errorf("%q %q: %+v %v", c.subset, c.pass, pass, err)
		}
	}
	if calls := replayed(t, dir); len(calls) != 0 {
		t.Errorf("restic ran %q", calls)
	}

	for _, version := range []string{"0.18.0", "0.16.4", "1.17.0"} {
		t.Setenv("REPLAY_VERSION", version)
		pass, _, _, err := toolCheck(t, checkEnv, "--subset", "1/20", "--pass", "free")
		want := "PocketCloud reads restic 0.17's check output; this image has restic " + version
		if err == nil || err.Error() != want || pass.Outcome != backup.OutcomeError || pass.Error != want {
			t.Errorf("restic %s: %+v %v", version, pass, err)
		}
	}
	if calls := replayed(t, dir); slices.ContainsFunc(calls, func(call string) bool { return strings.Contains(call, "check") }) {
		t.Errorf("restic ran %q", calls)
	}
}

// TestTheFakeResticChecksAsRestic0173Does (#31): what the fake restic
// prints for a check reads as restic's own does, so that the agent's and the
// panel's tests stand for the real thing: a group read, damaged packs, a
// prune's false errors only without a lock, a damaged index or tree, a lock
// that keeps pass 2 out with the holder a test names, and a read-only bind
// that pass 2 cannot lock.
func TestTheFakeResticChecksAsRestic0173Does(t *testing.T) {
	env, root := backupRepository(t)
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a": "1"})
	for range 3 {
		if _, _, err := runBackup(t, env, nil, "--host", "nas", "--tag", "job:j1", "--path", dir); err != nil {
			t.Fatal(err)
		}
	}
	free := []string{"--subset", "2/4", "--pass", "free"}
	confirm := []string{"--subset", "2/4", "--pass", "confirm"}
	// the tool asks for every counter, which the fake prints then
	pass, marks, log, err := toolCheck(t, env, free...)
	if err != nil || pass.Outcome != backup.OutcomeOK || pass.PacksRead != 2 || pass.PacksTotal != 6 || len(marks) != 8 || marks[6].Percent != 72 ||
		!strings.Contains(log, "no errors were found") {
		t.Errorf("ok: %+v %v %+v\n%s", pass, err, marks, log)
	}

	t.Setenv("FAKE_RESTIC_CHECK", "errors")
	for _, args := range [][]string{free, confirm} {
		pass, _, _, err = toolCheck(t, env, args...)
		if err == nil || pass.Outcome != backup.OutcomeErrors || len(pass.Errors) != 1 || pass.Errors[0].Kind != "pack" || len(pass.Errors[0].ID) != 64 ||
			!slices.Equal(pass.Repair.Packs, []string{pass.Errors[0].ID}) || !pass.Repair.Snapshots || pass.Repair.Other {
			t.Errorf("errors %q: %+v %v", args[3], pass, err)
		}
	}
	t.Setenv("FAKE_RESTIC_CHECK", "errors_free")
	if pass, _, _, _ = toolCheck(t, env, free...); pass.Outcome != backup.OutcomeErrors || len(pass.Errors) != 1 || pass.Errors[0].Kind != "other" ||
		!strings.HasPrefix(pass.Errors[0].Text, "download error: open ") || !pass.Repair.Other {
		t.Errorf("a prune's errors without a lock: %+v", pass)
	}
	if pass, _, _, err = toolCheck(t, env, confirm...); err != nil || pass.Outcome != backup.OutcomeOK {
		t.Errorf("a prune's errors under the lock: %+v %v", pass, err)
	}
	t.Setenv("FAKE_RESTIC_CHECK", "index")
	if pass, _, _, _ = toolCheck(t, env, free...); pass.Outcome != backup.OutcomeErrors || !pass.Repair.Index || len(pass.Errors) != 1 || pass.Errors[0].Kind != "other" {
		t.Errorf("a damaged index: %+v", pass)
	}
	t.Setenv("FAKE_RESTIC_CHECK", "tree")
	if pass, _, _, _ = toolCheck(t, env, "--subset", "", "--pass", "free"); pass.Outcome != backup.OutcomeErrors || !pass.Repair.Other || len(pass.Errors) != 1 ||
		pass.Errors[0].Kind != "tree" || !strings.Contains(pass.Errors[0].Text, ": decrypting blob <tree/") {
		t.Errorf("a damaged tree: %+v", pass)
	}
	t.Setenv("FAKE_RESTIC_CHECK", "")

	t.Setenv("FAKE_RESTIC_LOCKED_CHECK", "2")
	t.Setenv("FAKE_RESTIC_LOCK_HOST", "pc-k3j9x2m4q8w1z7v")
	if pass, _, _, _ = toolCheck(t, env, free...); pass.Outcome != backup.OutcomeOK {
		t.Errorf("pass 1 is never locked out: %+v", pass)
	}
	if pass, _, _, _ = toolCheck(t, env, confirm...); pass.Outcome != backup.OutcomeLocked || pass.Lock == nil || pass.Lock.Host != "pc-k3j9x2m4q8w1z7v" ||
		!pass.Lock.Exclusive || fakerestic.Unlocks(t, root) != 1 {
		t.Errorf("pass 2 locked out: %+v %+v", pass, pass.Lock)
	}
	t.Setenv("FAKE_RESTIC_LOCKED_CHECK", "")
	t.Setenv("FAKE_RESTIC_READONLY", fakerestic.Repository(root, env["RESTIC_REPOSITORY"]))
	if pass, _, _, _ = toolCheck(t, env, confirm...); pass.Outcome != backup.OutcomeError || !strings.HasSuffix(pass.Error, ": read-only file system") {
		t.Errorf("pass 2 on a read-only bind: %+v", pass)
	}
	if pass, _, _, err = toolCheck(t, env, free...); err != nil || pass.Outcome != backup.OutcomeOK {
		t.Errorf("pass 1 on a read-only bind: %+v %v", pass, err)
	}
}
