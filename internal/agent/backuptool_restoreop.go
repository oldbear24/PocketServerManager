package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/oldbear24/PocketServerManager/internal/backup"
)

// The backup tool's restore and dump ops (#30).

const (
	// maxRestoreRoots and maxRestoreIncludes bound what one restore names.
	maxRestoreRoots    = 32
	maxRestoreIncludes = 100
	// defaultSample is how many paths of each kind a preview says.
	defaultSample = 50
)

// restoreRoot is one root of a restore: a path of the snapshot, and where
// the job mounts its destination.
type restoreRoot struct {
	// source is the snapshot's path: "" for its only path, "/" for all of it
	source string
	// target is where the job mounts the destination, /restore/<i>
	target string
	// label names the destination in what the tool says
	label string
}

// restoreRequest is what the restore op is asked.
type restoreRequest struct {
	snapshot  string
	roots     []restoreRoot
	includes  []string
	overwrite bool
	replace   bool
	dryRun    bool
	wait      bool
	sample    int
	expectTag string
}

// restoreRequestOf checks the restore op's flags.
func restoreRequestOf(snapshot string, roots, dests, includes []string, overwrite, replace, dryRun, wait bool, sample int, tag string) (restoreRequest, error) {
	request := restoreRequest{snapshot: snapshot, includes: includes, overwrite: overwrite, replace: replace,
		dryRun: dryRun, wait: wait, sample: sample, expectTag: tag}
	if !snapshotID.MatchString(snapshot) {
		return request, fmt.Errorf("a snapshot is named by its full id, not %q", snapshot)
	}
	if len(roots) == 0 || len(roots) > maxRestoreRoots {
		return request, fmt.Errorf("a restore has 1 to %d folders, not %d", maxRestoreRoots, len(roots))
	}
	if len(dests) != 0 && len(dests) != len(roots) {
		return request, errors.New("a restore names each folder's destination, or none")
	}
	for i, root := range roots {
		// where the job mounts each destination: the agent's job mounts
		// root i at /restore/<i>, and nothing else of the server writable
		source, target, ok := strings.Cut(root, "=")
		if !ok || !path.IsAbs(target) || path.Clean(target) != target || target == "/" {
			return request, fmt.Errorf("a folder of a restore is SRC=/restore/<i>, not %q", root)
		}
		for _, other := range request.roots {
			if backup.Overlap(other.target, target) {
				return request, fmt.Errorf("the folders of a restore go to separate places, not %s and %s", other.target, target)
			}
		}
		if source != "" {
			if err := checkSnapshotPath(source); err != nil {
				return request, err
			}
		}
		label := target
		if len(dests) > 0 {
			label = dests[i]
		}
		request.roots = append(request.roots, restoreRoot{source: source, target: target, label: label})
	}
	if len(includes) > maxRestoreIncludes {
		return request, fmt.Errorf("a restore picks at most %d paths, not %d", maxRestoreIncludes, len(includes))
	}
	for _, include := range includes {
		if err := checkSnapshotPath(include); err != nil {
			return request, err
		}
	}
	if replace && len(includes) > 0 {
		return request, errors.New("replace works for the whole snapshot only, without picked paths")
	}
	if replace && !overwrite && !dryRun {
		return request, errors.New("a restore that replaces what the destination holds overwrites it: it needs --overwrite")
	}
	if tag != "" && !jobTag.MatchString(tag) {
		return request, fmt.Errorf("a job's tag is job: and its id, not %q", tag)
	}
	if sample < 0 || sample > 1000 {
		return request, fmt.Errorf("a preview says 0 to 1000 paths of each kind, not %d", sample)
	}
	return request, nil
}

// previewResult is what a restore's dry run found.
type previewResult struct {
	DryRun   bool `json:"dry_run"`
	Snapshot struct {
		ID    string   `json:"id"`
		Time  string   `json:"time"`
		Paths []string `json:"paths"`
	} `json:"snapshot"`
	Roots      []previewRoot  `json:"roots"`
	Counts     actionCounts   `json:"counts"`
	Bytes      actionBytes    `json:"bytes"`
	Samples    actionSamples  `json:"samples"`
	Truncated  actionTruncate `json:"truncated"`
	TotalFiles int64          `json:"total_files"`
	TotalBytes int64          `json:"total_bytes"`
	Notes      []string       `json:"notes"`
}

// previewRoot is one root of a preview.
type previewRoot struct {
	Index         int     `json:"index"`
	Source        string  `json:"source"`
	Destination   string  `json:"destination"`
	InSnapshot    bool    `json:"in_snapshot"`
	Entries       int     `json:"entries"`
	EntriesCapped bool    `json:"entries_capped"`
	UID           *uint32 `json:"uid,omitempty"`
	GID           *uint32 `json:"gid,omitempty"`
}

type actionCounts struct {
	New       int64 `json:"new"`
	Updated   int64 `json:"updated"`
	Unchanged int64 `json:"unchanged"`
	Deleted   int64 `json:"deleted"`
}

type actionBytes struct {
	New     int64 `json:"new"`
	Updated int64 `json:"updated"`
	Deleted int64 `json:"deleted"`
}

type actionSamples struct {
	New     []string `json:"new"`
	Updated []string `json:"updated"`
	Deleted []string `json:"deleted"`
}

type actionTruncate struct {
	New     int64 `json:"new"`
	Updated int64 `json:"updated"`
	Deleted int64 `json:"deleted"`
}

// restoreResult is what a real restore did.
type restoreResult struct {
	DryRun        bool           `json:"dry_run"`
	Roots         []restoredRoot `json:"roots"`
	FilesRestored int64          `json:"files_restored"`
	BytesRestored int64          `json:"bytes_restored"`
	FilesDeleted  int64          `json:"files_deleted"`
	Errors        int64          `json:"errors"`
	ErrorSample   []string       `json:"error_sample"`
	Partial       bool           `json:"partial"`
	Refused       string         `json:"refused"`
}

// restoredRoot is one root of a real restore.
type restoredRoot struct {
	Source        string `json:"source"`
	Destination   string `json:"destination"`
	FilesRestored int64  `json:"files_restored"`
	FilesSkipped  int64  `json:"files_skipped"`
	FilesDeleted  int64  `json:"files_deleted"`
	BytesRestored int64  `json:"bytes_restored"`
	TotalBytes    int64  `json:"total_bytes"`
	Done          bool   `json:"done"`
	Entries       int    `json:"entries,omitempty"`
}

// resticRestoreLine is one line of restic restore --json.
type resticRestoreLine struct {
	MessageType   string  `json:"message_type"`
	PercentDone   float64 `json:"percent_done"`
	Action        string  `json:"action"`
	Item          string  `json:"item"`
	Size          int64   `json:"size"`
	TotalFiles    int64   `json:"total_files"`
	FilesRestored int64   `json:"files_restored"`
	FilesSkipped  int64   `json:"files_skipped"`
	TotalBytes    int64   `json:"total_bytes"`
	BytesRestored int64   `json:"bytes_restored"`
	BytesSkipped  int64   `json:"bytes_skipped"`
}

// restorePlan is a root as the restore found it in the snapshot.
type restorePlan struct {
	restoreRoot
	index int
	// inSnapshot says the source is a folder of the snapshot
	inSnapshot bool
	// node is the source's own, whose owner, mode and time the destination
	// gets; nil for all of the snapshot
	node *resticNode
	// patterns are restic's --include for it; restore is false for a root
	// none of the picked paths is under
	patterns []string
	restore  bool
	entries  int
	capped   bool
}

// includePattern is an absolute path of the snapshot as restic's --include
// takes it in a restore of source, ID:source: relative to source, which
// restic 0.17.3 matches from the root of what it restores; all of a
// snapshot's paths as they are (see testdata/restic-0.17.3). restic takes a
// pattern as globs, so each character a glob gives a meaning stands for
// itself behind a backslash: a picked file[1].txt is that file, not
// file1.txt.
func includePattern(source, include string) string {
	if source != "/" {
		include = "/" + strings.TrimPrefix(include, source+"/")
	}
	return globEscaper.Replace(include)
}

// globEscaper puts a backslash before each character of a path that
// restic's globs, those of filepath.Match, give a meaning; ] means nothing
// without the [ before it.
var globEscaper = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`)

// restore restores the snapshot's roots, or says what it would do. Its
// result, when there is one, is said also with its error: what a restore
// refused, or did before it failed.
func (r restic) restore(ctx context.Context, request restoreRequest, stdin io.Reader) (any, error) {
	if err := r.checkVersion(ctx); err != nil {
		return nil, err
	}
	snapshot, err := r.snapshot(ctx, request.snapshot, request.expectTag)
	if err != nil {
		return nil, err
	}
	plans, notes, err := r.plan(ctx, snapshot, request)
	if err != nil {
		return nil, err
	}
	if request.dryRun {
		// what is checked: the agent says it compares now
		r.mark(progressLine([]byte(`{"step":"preview"}`)))
		return r.preview(ctx, snapshot, request, plans, notes)
	}
	result := &restoreResult{Roots: []restoredRoot{}, ErrorSample: []string{}}
	for _, plan := range plans {
		if plan.restore {
			result.Roots = append(result.Roots, restoredRoot{Source: plan.source, Destination: plan.label})
		}
	}
	for i, plan := range restored(plans) {
		if plan.entries > 0 && !request.overwrite {
			result.Refused = "not_empty"
			result.Roots[i].Entries = plan.entries
			count, them := items(plan.entries), "them"
			switch {
			case plan.capped:
				count = "more than " + count
			case plan.entries == 1:
				them = "it"
			}
			return result, fmt.Errorf("%s holds %s and the restore was not confirmed to overwrite %s: preview again", plan.label, count, them)
		}
	}
	if request.wait {
		// what the job mounts, which the agent holds to the folders it
		// checked before it stops anything, said right before the ready mark
		r.mark(progressLine(mountsReport(request.roots)))
		if err := r.waitForGo(ctx, stdin); err != nil {
			return nil, err
		}
	}
	err = r.restoreRoots(ctx, request, restored(plans), result)
	if err == nil && !request.dryRun && result.Errors > 0 {
		// what restic could not restore leaves the destination short of the
		// snapshot: the restore failed, as a backup that could not read
		// everything does, and says what it did
		err = couldNotRestore(result)
	}
	result.Partial = err != nil && result.FilesRestored+result.FilesDeleted > 0 || result.Errors > 0
	return result, err
}

// items counts items as a sentence says them: "1 item", "3 items".
func items[N int | int64](n N) string {
	if n == 1 {
		return "1 item"
	}
	return strconv.FormatInt(int64(n), 10) + " items"
}

// couldNotRestore says what a restore whose restic restored what it could
// could not restore.
func couldNotRestore(result *restoreResult) error {
	if len(result.ErrorSample) == 0 {
		return fmt.Errorf("restic could not restore %s, so the destination holds part of the snapshot", items(result.Errors))
	}
	return fmt.Errorf("restic could not restore %s, so the destination holds part of the snapshot; the first: %s", items(result.Errors), result.ErrorSample[0])
}

// restored are the plans of the roots that are restored.
func restored(plans []restorePlan) []restorePlan {
	var kept []restorePlan
	for _, plan := range plans {
		if plan.restore {
			kept = append(kept, plan)
		}
	}
	return kept
}

// plan finds each root in the snapshot and counts what its destination
// holds. A real restore refuses a root the snapshot lacks; a preview says
// so.
func (r restic) plan(ctx context.Context, snapshot resticSnapshot, request restoreRequest) ([]restorePlan, []string, error) {
	notes := []string{}
	plans := make([]restorePlan, len(request.roots))
	parents := map[string][]int{}
	for i, root := range request.roots {
		plan := restorePlan{restoreRoot: root, index: i}
		switch {
		case root.source == "" && len(snapshot.Paths) == 1:
			plan.source = snapshot.Paths[0]
		case root.source == "":
			notes = append(notes, fmt.Sprintf("The snapshot holds %d paths: the restore into %s names the one it restores.", len(snapshot.Paths), root.label))
		case root.source == "/":
			plan.inSnapshot = true
		}
		if plan.source != "" && plan.source != "/" {
			for _, saved := range snapshot.Paths {
				if backup.Under(plan.source, saved) {
					parents[path.Dir(plan.source)] = append(parents[path.Dir(plan.source)], i)
					break
				}
			}
		}
		plans[i] = plan
	}
	// each root's own node, with one listing per folder that holds roots
	for _, dir := range sortedKeys(parents) {
		wanted := map[string][]int{}
		for _, i := range parents[dir] {
			wanted[plans[i].source] = append(wanted[plans[i].source], i)
		}
		code, said, err := r.lines(ctx, func(line []byte) {
			var node resticNode
			if json.Unmarshal(line, &node) != nil || !node.node() {
				return
			}
			for _, i := range wanted[node.Path] {
				found := node
				plans[i].node = &found
			}
		}, nil, "--no-lock", "ls", "--json", snapshot.ID, dir)
		if err != nil {
			return nil, nil, err
		}
		if code != 0 {
			return nil, nil, repositoryError(code, said)
		}
	}
	for i := range plans {
		plan := &plans[i]
		if plan.node != nil {
			if plan.node.Type == "dir" {
				plan.inSnapshot = true
			} else {
				notes = append(notes, fmt.Sprintf("%s is a file of the snapshot: restore the folder that holds it, with the file picked.", plan.source))
				plan.node = nil
			}
		}
		if !plan.inSnapshot && plan.source != "" {
			notes = append(notes, fmt.Sprintf("%s is not in the snapshot %.8s: %s stays as it is.", plan.source, snapshot.ID, plan.label))
		}
	}
	// the picked paths go to the roots they are under
	for _, include := range request.includes {
		under := false
		for i := range plans {
			plan := &plans[i]
			if !plan.inSnapshot || !backup.Under(include, plan.source) {
				continue
			}
			under = true
			if include == plan.source {
				plan.patterns = []string{}
			} else if plan.patterns == nil || len(plan.patterns) > 0 {
				plan.patterns = append(plan.patterns, includePattern(plan.source, include))
			}
		}
		if !under {
			if !request.dryRun {
				return nil, nil, fmt.Errorf("%s is in none of the folders the restore restores", include)
			}
			notes = append(notes, fmt.Sprintf("%s is in none of the folders the restore restores.", include))
		}
	}
	for i := range plans {
		plan := &plans[i]
		plan.restore = plan.inSnapshot && (len(request.includes) == 0 || plan.patterns != nil)
		if !plan.inSnapshot && !request.dryRun {
			return nil, nil, fmt.Errorf("%s is not in the snapshot %.8s: preview again", plan.label, snapshot.ID)
		}
		plan.entries, plan.capped = countEntries(plan.target)
	}
	return plans, notes, nil
}

// sortedKeys are a map's keys in order.
func sortedKeys(m map[string][]int) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// countEntries counts what a destination holds, up to maxEntriesCounted,
// and says whether it holds more; a destination the job does not mount
// holds nothing.
func countEntries(dir string) (int, bool) {
	count := 0
	stop := errors.New("enough")
	_ = filepath.WalkDir(dir, func(file string, _ fs.DirEntry, err error) error {
		if err != nil || file == dir {
			return nil //nolint:nilerr // what cannot be read is not counted
		}
		if count++; count > maxEntriesCounted {
			return stop
		}
		return nil
	})
	if count > maxEntriesCounted {
		return maxEntriesCounted, true
	}
	return count, false
}

// mountsReport is the progress line of the step ready: the device and
// inode of what the job mounts at each root's target, in the roots' order,
// zero for one that cannot be read.
func mountsReport(roots []restoreRoot) []byte {
	mounts := make([]fileID, len(roots))
	for i, root := range roots {
		if id, err := dirID(root.target); err == nil {
			mounts[i] = id
		}
	}
	report, _ := json.Marshal(map[string]any{"step": "ready", "mounts": mounts})
	return report
}

// waitForGo says the restore is ready, and waits for the agent's go: it has
// stopped what uses the destination. Anything else calls it off.
func (r restic) waitForGo(ctx context.Context, stdin io.Reader) error {
	if stdin == nil {
		return errors.New("the restore waits for the agent's go, and has no input")
	}
	r.mark(restoreReady)
	answer := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		answer <- strings.TrimSpace(line)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case line := <-answer:
		if line != "go" {
			return errors.New("the restore was called off before anything changed")
		}
		return nil
	}
}

// restoreArgs are restic's arguments for one root.
func restoreArgs(snapshot string, plan restorePlan, request restoreRequest) []string {
	spec := snapshot
	if plan.source != "/" {
		spec += ":" + plan.source
	}
	var args []string
	if request.dryRun {
		// a preview compares as a restore that overwrites would
		args = []string{"--no-lock", "restore", spec, "--target", plan.target, "--dry-run", "-vv", "--json", "--overwrite", "always"}
	} else {
		overwrite := "never"
		if request.overwrite {
			overwrite = "always"
		}
		args = []string{"--retry-lock", restoreRetryLock, "restore", spec, "--target", plan.target, "--json", "-vv", "--overwrite", overwrite}
	}
	if request.replace && (request.overwrite || request.dryRun) && len(request.includes) == 0 {
		args = append(args, "--delete")
	}
	for _, pattern := range plan.patterns {
		args = append(args, "--include", pattern)
	}
	return args
}

// rootRun is how one root's run of restic went.
type rootRun struct {
	summary  *resticRestoreLine
	deleted  int64
	errors   int64
	lines    int64
	code     int
	said     string
	failures []string
}

// runRoot runs restic restore for one root, once more after restic unlock
// when the repository was locked before anything was restored.
func (r restic) runRoot(ctx context.Context, request restoreRequest, plan restorePlan, onLine func(line resticRestoreLine)) (rootRun, error) {
	env := map[string]string{"RESTIC_PROGRESS_FPS": "0.5"}
	if request.dryRun {
		env["RESTIC_PROGRESS_FPS"] = "0.2"
	}
	args := restoreArgs(request.snapshot, plan, request)
	for attempt := 0; ; attempt++ {
		run := rootRun{}
		code, said, err := r.stream(ctx, streamOptions{env: env, line: func(raw []byte) {
			var line resticRestoreLine
			if json.Unmarshal(raw, &line) != nil {
				return
			}
			run.lines++
			switch line.MessageType {
			case "summary":
				summary := line
				run.summary = &summary
			case "verbose_status":
				if line.Action == "deleted" {
					run.deleted++
				}
			}
			onLine(line)
		}, errLine: func(raw string) {
			if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
				return
			}
			if message := readable(raw); message != raw {
				run.errors++
				if len(run.failures) < maxErrorSample {
					run.failures = append(run.failures, backup.Redact(strings.ReplaceAll(message, plan.target, plan.label), r.secrets...))
				}
			}
		}}, args...)
		if err != nil {
			return run, err
		}
		run.code, run.said = code, said
		if code == resticLocked && run.lines == 0 && attempt == 0 {
			// a lock that no restic refreshes, of a job that was killed,
			// keeps the restore out until restic unlock removes it
			r.unlock(ctx, lockedRetry)
			continue
		}
		return run, nil
	}
}

// preview runs each root's dry run and says what the restore would do.
func (r restic) preview(ctx context.Context, snapshot resticSnapshot, request restoreRequest, plans []restorePlan, notes []string) (*previewResult, error) {
	result := &previewResult{DryRun: true, Roots: []previewRoot{}, Notes: notes,
		Samples: actionSamples{New: []string{}, Updated: []string{}, Deleted: []string{}}}
	result.Snapshot.ID = snapshot.ID
	result.Snapshot.Time = snapshot.Time.UTC().Format(time.RFC3339)
	result.Snapshot.Paths = snapshot.Paths
	if result.Snapshot.Paths == nil {
		result.Snapshot.Paths = []string{}
	}
	several := len(restored(plans)) > 1
	for _, plan := range plans {
		root := previewRoot{Index: plan.index, Source: plan.source, Destination: plan.label, InSnapshot: plan.inSnapshot,
			Entries: plan.entries, EntriesCapped: plan.capped}
		if plan.node != nil {
			uid, gid := plan.node.UID, plan.node.GID
			root.UID, root.GID = &uid, &gid
		}
		result.Roots = append(result.Roots, root)
		if !plan.restore {
			continue
		}
		run, err := r.runRoot(ctx, request, plan, func(line resticRestoreLine) {
			if line.MessageType != "verbose_status" {
				return
			}
			sample := samplePath(plan, line.Item, several)
			switch line.Action {
			case "restored":
				result.Counts.New++
				result.Bytes.New += line.Size
				addSample(&result.Samples.New, &result.Truncated.New, sample, request.sample)
			case "updated":
				result.Counts.Updated++
				result.Bytes.Updated += line.Size
				addSample(&result.Samples.Updated, &result.Truncated.Updated, sample, request.sample)
			case "unchanged":
				result.Counts.Unchanged++
			case "deleted":
				result.Counts.Deleted++
				result.Bytes.Deleted += line.Size
				addSample(&result.Samples.Deleted, &result.Truncated.Deleted, sample, request.sample)
			}
		})
		if err != nil {
			return nil, err
		}
		if run.code != 0 && (run.code != 1 || run.errors == 0) {
			return nil, repositoryError(run.code, run.said)
		}
		if run.summary != nil {
			result.TotalFiles += run.summary.TotalFiles + run.summary.FilesSkipped
			result.TotalBytes += run.summary.TotalBytes + run.summary.BytesSkipped
		}
		result.Notes = append(result.Notes, run.failures...)
	}
	return result, nil
}

// samplePath is a path restic restored, relative to its root, with the
// root's name before it when the restore has several, at most
// maxSamplePath characters.
func samplePath(plan restorePlan, item string, several bool) string {
	sample := strings.TrimPrefix(item, "/")
	if several {
		sample = path.Join(path.Base(plan.source), sample)
	}
	if utf8.RuneCountInString(sample) > maxSamplePath {
		runes := []rune(sample)
		sample = string(runes[:maxSamplePath-1]) + "…"
	}
	return sample
}

// addSample keeps a sample while there is room, and counts it left out
// otherwise.
func addSample(samples *[]string, truncated *int64, sample string, room int) {
	if len(*samples) < room {
		*samples = append(*samples, sample)
		return
	}
	*truncated++
}

// restoreRoots runs the real restore of each root, says how far it got, and
// gives each root's destination the snapshot's owner, mode and time of its
// folder, which restic leaves as it was.
func (r restic) restoreRoots(ctx context.Context, request restoreRequest, plans []restorePlan, result *restoreResult) error {
	var doneBefore, totalBefore int64
	last := time.Time{}
	for i, plan := range plans {
		root := &result.Roots[i]
		fmt.Fprintf(r.log, "Restoring %s of the snapshot %.8s into %s\n", plan.source, request.snapshot, plan.label)
		run, err := r.runRoot(ctx, request, plan, func(line resticRestoreLine) {
			if line.MessageType != "status" || time.Since(last) < restoreProgressEvery {
				return
			}
			last = time.Now()
			progress, _ := json.Marshal(map[string]any{
				"percent": int(line.PercentDone * 100), "done": doneBefore + line.BytesRestored, "total": totalBefore + line.TotalBytes,
				"unit": "bytes", "root": i, "roots": len(plans),
			})
			r.mark(progressLine(progress))
		})
		result.Errors += run.errors
		for _, failure := range run.failures {
			if len(result.ErrorSample) < maxErrorSample {
				result.ErrorSample = append(result.ErrorSample, failure)
			}
		}
		if summary := run.summary; summary != nil {
			root.FilesRestored, root.FilesSkipped, root.BytesRestored, root.TotalBytes = summary.FilesRestored, summary.FilesSkipped, summary.BytesRestored, summary.TotalBytes
		}
		root.FilesDeleted = run.deleted
		result.FilesRestored += root.FilesRestored
		result.BytesRestored += root.BytesRestored
		result.FilesDeleted += root.FilesDeleted
		if err != nil {
			return err
		}
		switch {
		case run.code == 0:
		case run.code == 1 && run.errors > 0:
			// restic restored what it could, and said what it could not
			fmt.Fprintf(r.log, "restic could not restore %s into %s\n", items(run.errors), plan.label)
		default:
			return repositoryError(run.code, run.said)
		}
		if plan.node != nil {
			if err := applyNode(plan.target, plan.node); err != nil {
				result.Errors++
				message := strings.ReplaceAll(err.Error(), plan.target, plan.label)
				if len(result.ErrorSample) < maxErrorSample {
					result.ErrorSample = append(result.ErrorSample, message)
				}
				fmt.Fprintln(r.log, message)
			}
		}
		root.Done = true
		doneBefore += root.BytesRestored
		totalBefore += root.TotalBytes
		fmt.Fprintf(r.log, "Restored %d files (%s) into %s, and deleted %d\n", root.FilesRestored, sizeText(root.BytesRestored), plan.label, root.FilesDeleted)
	}
	return nil
}

// applyNode gives a destination the owner, mode and time of the snapshot's
// folder, as a database's data directory needs its owner and 0700 back:
// restic restores what is in a folder, not the folder it restores into.
func applyNode(dir string, node *resticNode) error {
	if err := os.Lchown(dir, int(node.UID), int(node.GID)); err != nil {
		return fmt.Errorf("the owner of %s stays as it was: %w", dir, err)
	}
	// after the owner, which clears setuid and setgid
	if err := os.Chmod(dir, node.Mode&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)); err != nil {
		return fmt.Errorf("the mode of %s stays as it was: %w", dir, err)
	}
	if !node.MTime.IsZero() {
		if err := os.Chtimes(dir, node.MTime, node.MTime); err != nil {
			return fmt.Errorf("the time of %s stays as it was: %w", dir, err)
		}
	}
	return nil
}

// countingWriter counts what passes.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(data []byte) (int, error) {
	n, err := c.w.Write(data)
	c.n += int64(n)
	return n, err
}

// dump writes a path of the snapshot to w: a folder as a tar, a file as it
// is, once the snapshot is known to be the job's. noLock takes no lock, and
// so never unlocks: the panel's recovery reads a repository it writes
// nothing to, a local one bound read-only (#32).
func (r restic) dump(ctx context.Context, id, p, tag string, noLock bool, w io.Writer) error {
	if !snapshotID.MatchString(id) {
		return fmt.Errorf("a snapshot is named by its full id, not %q", id)
	}
	if err := checkSnapshotPath(p); err != nil {
		return err
	}
	if tag != "" && !jobTag.MatchString(tag) {
		return fmt.Errorf("a job's tag is job: and its id, not %q", tag)
	}
	if err := r.checkVersion(ctx); err != nil {
		return err
	}
	if _, err := r.snapshot(ctx, id, tag); err != nil {
		return err
	}
	out := &countingWriter{w: w}
	args := []string{"--retry-lock", restoreRetryLock, "dump", "--archive", "tar", id, p}
	if noLock {
		args = []string{"--no-lock", "dump", "--archive", "tar", id, p}
	}
	code, said, err := r.to(ctx, out, args...)
	if err == nil && code == resticLocked && out.n == 0 && !noLock {
		r.unlock(ctx, lockedRetry)
		code, said, err = r.to(ctx, out, args...)
	}
	if err != nil {
		return err
	}
	if code != 0 {
		return repositoryError(code, said)
	}
	return nil
}
