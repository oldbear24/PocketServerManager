package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
)

// Restores (#30). A job's snapshots are listed and browsed, a restore is
// previewed and run, each in a job container of the agent's own image, like
// a backup, with the backup tool. A real restore checks everything in its
// job first; once the tool says it is ready, the agent stops the containers
// that use the destination, with the restart policy no and a marker in its
// volume, and says go; it starts them again, whatever ended the restore.
// Only the destination is mounted writable, and restic keeps no cache.

const (
	// restoreJobMount is where a restore's job mounts root i's destination:
	// /restore/<i>.
	restoreJobMount = "/restore"
	// What the results may take: the panel keeps a command's result in a
	// field of 1 MiB, and a page reads them.
	maxSnapshotsResult = 160_000
	maxBrowseResult    = 200_000
	maxPreviewResult   = 64 << 10
	maxRestoreResult   = 32 << 10
	// restoreDeadlineReserve is the part of a restore's time left for
	// starting the application again and saying how it went, after the job
	// was stopped: the panel gives up on a command at its deadline.
	restoreDeadlineReserve = 5 * time.Minute
	// maxBrowseMatch is how long a filter of a folder may be.
	maxBrowseMatch = 255
)

// restoreProgressForward is how often a restore's progress goes to the panel,
// which saves each: a new step goes at once. Tests shorten it.
var restoreProgressForward = 5 * time.Second

func (a *Agent) registerRestoreHandlers() {
	a.Executor.Handle("backup.snapshots", a.handleBackupSnapshots)
	a.Executor.Handle("backup.browse", a.handleBackupBrowse)
	a.Executor.Handle("backup.restore", a.handleBackupRestore)
	a.Executor.Handle("backup.dump", a.handleBackupDump)
}

// commandLabels are a job's labels that name its command.
func commandLabels(command *agentv1.Command) map[string]string {
	return map[string]string{"pocketcloud.command": command.GetId()}
}

// toolResult is the JSON object the backup tool printed.
func toolResult(out []byte) (map[string]any, error) {
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil || result == nil {
		return nil, fmt.Errorf("the backup tool said: %s", out)
	}
	return result, nil
}

// resultSize is how large a result is as JSON.
func resultSize(result map[string]any) int {
	raw, _ := json.Marshal(result)
	return len(raw)
}

// trimList drops the last items of a list of a result until the result fits
// in limit bytes.
func trimList(result map[string]any, key string, limit int) {
	for resultSize(result) > limit {
		list, _ := result[key].([]any)
		if len(list) == 0 {
			return
		}
		result[key] = list[:len(list)-max(1, len(list)/10)]
	}
}

// handleBackupSnapshots lists a job's snapshots in a target, newest first.
func (a *Agent) handleBackupSnapshots(ctx context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	payload := command.GetBackupSnapshots()
	if err := checkRepository(payload.GetRepository()); err != nil {
		return nil, err
	}
	if err := checkTags(payload.GetTags()); err != nil {
		return nil, err
	}
	limit := int(payload.GetLimit())
	if limit < 0 || limit > maxSnapshotsListed {
		return nil, fmt.Errorf("a list says 1 to %d snapshots, not %d", maxSnapshotsListed, limit)
	}
	if limit == 0 {
		limit = defaultSnapshotsListed
	}
	args := []string{"snapshots"}
	for _, tag := range payload.GetTags() {
		args = append(args, "--tag", tag)
	}
	args = append(args, "--limit", strconv.Itoa(limit))
	out, err := a.backupJob(ctx, payload.GetRepository(), backupJobSpec{args: args, labels: commandLabels(command)}, io.Discard)
	if err != nil {
		return nil, err
	}
	result, err := toolResult(out)
	if err != nil {
		return nil, err
	}
	trimList(result, "snapshots", maxSnapshotsResult)
	return result, nil
}

// handleBackupBrowse lists one page of a folder of a snapshot.
func (a *Agent) handleBackupBrowse(ctx context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	payload := command.GetBackupBrowse()
	if err := checkRepository(payload.GetRepository()); err != nil {
		return nil, err
	}
	if !snapshotID.MatchString(payload.GetSnapshotId()) {
		return nil, fmt.Errorf("a snapshot is named by its full id, not %q", payload.GetSnapshotId())
	}
	if err := checkSnapshotPath(payload.GetPath()); err != nil {
		return nil, err
	}
	offset, limit := int(payload.GetOffset()), int(payload.GetLimit())
	if offset < 0 || limit < 0 || limit > maxEntriesListed {
		return nil, fmt.Errorf("a page of a folder starts at 0 or later and says 1 to %d entries", maxEntriesListed)
	}
	if limit == 0 {
		limit = defaultEntriesListed
	}
	match := payload.GetMatch()
	if len(match) > maxBrowseMatch || !validText(match) {
		return nil, errors.New("a filter of a folder is a part of a name, of at most 255 bytes")
	}
	args := []string{"ls", "--snapshot", payload.GetSnapshotId(), "--dir", payload.GetPath(),
		"--offset", strconv.Itoa(offset), "--limit", strconv.Itoa(limit)}
	if match != "" {
		args = append(args, "--match", match)
	}
	out, err := a.backupJob(ctx, payload.GetRepository(), backupJobSpec{args: args, labels: commandLabels(command)}, io.Discard)
	if err != nil {
		return nil, err
	}
	result, err := toolResult(out)
	if err != nil {
		return nil, err
	}
	trimList(result, "entries", maxBrowseResult)
	return result, nil
}

// validText says whether text has no line break and no NUL.
func validText(text string) bool {
	for _, r := range text {
		if r == '\n' || r == '\r' || r == 0 {
			return false
		}
	}
	return true
}

// checkRestore checks what a restore asks, before anything of the server is
// looked at.
func checkRestore(command *agentv1.Command) error {
	payload := command.GetBackupRestore()
	switch {
	case payload.GetRestoreId() == "":
		return errors.New("a restore names the panel's record of it")
	case !snapshotID.MatchString(payload.GetSnapshotId()):
		return fmt.Errorf("a snapshot is named by its full id, not %q", payload.GetSnapshotId())
	case !jobTag.MatchString(payload.GetJobTag()):
		return fmt.Errorf("a restore names its job's tag, job: and the job's id, not %q", payload.GetJobTag())
	case payload.GetTargetVolume() != "":
		return errors.New("a restore goes back into a volume through its original location, not target_volume")
	case (len(payload.GetOriginal()) > 0) == (payload.GetTargetPath() != ""):
		return errors.New("a restore goes to its original location or to a new folder: exactly one of them")
	case len(payload.GetOriginal()) > maxRestoreRoots:
		return fmt.Errorf("a restore has at most %d folders, not %d", maxRestoreRoots, len(payload.GetOriginal()))
	case len(payload.GetInclude()) > maxRestoreIncludes:
		return fmt.Errorf("a restore picks at most %d paths, not %d", maxRestoreIncludes, len(payload.GetInclude()))
	case payload.GetReplace() && len(payload.GetInclude()) > 0:
		return errors.New("replace works for the whole snapshot only, without picked paths")
	case !payload.GetDryRun() && payload.GetReplace() && !payload.GetOverwrite():
		return errors.New("a restore that replaces what the destination holds overwrites it, and that was not confirmed")
	case !payload.GetDryRun() && len(payload.GetOriginal()) > 0 && !payload.GetOverwrite():
		return errors.New("a restore to the original location overwrites it, and that was not confirmed")
	case !payload.GetDryRun() && !markerName.MatchString(command.GetId()):
		return fmt.Errorf("the command's id %q cannot name the agent's note of what it stops", command.GetId())
	}
	if source := payload.GetSource(); source != "" {
		if err := checkSnapshotPath(source); err != nil {
			return err
		}
	}
	for _, include := range payload.GetInclude() {
		if err := checkSnapshotPath(include); err != nil {
			return err
		}
		if !includeUnderARoot(payload, include) {
			return fmt.Errorf("%s is in none of the folders the restore restores", include)
		}
	}
	return nil
}

// includeUnderARoot says whether a picked path is in a root of the restore;
// a root whose path the preview finds takes any.
func includeUnderARoot(payload *agentv1.BackupRestore, include string) bool {
	if payload.GetTargetPath() != "" {
		source := payload.GetSource()
		return source == "" || backup.Under(include, source)
	}
	for _, root := range payload.GetOriginal() {
		if root.GetSource() == "" || backup.Under(include, root.GetSource()) {
			return true
		}
	}
	return false
}

// handleBackupRestore previews a restore, or runs it.
func (a *Agent) handleBackupRestore(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetBackupRestore()
	if err := checkRepository(payload.GetRepository()); err != nil {
		return nil, err
	}
	if err := checkRestore(command); err != nil {
		return nil, err
	}
	dests, notes, err := a.restoreDests(ctx, payload)
	if err != nil {
		return nil, err
	}
	if payload.GetDryRun() {
		return a.previewRestore(ctx, command, dests, notes, reporter)
	}
	return a.runRestore(ctx, command, dests, notes, reporter)
}

// restoreToolArgs are the backup tool's flags of a restore.
func restoreToolArgs(payload *agentv1.BackupRestore, dests []restoreDest) []string {
	args := []string{"restore", "--snapshot", payload.GetSnapshotId(), "--expect-tag", payload.GetJobTag()}
	for i, dest := range dests {
		args = append(args, "--root", dest.source+"="+restoreJobMount+"/"+strconv.Itoa(i), "--dest", dest.label())
	}
	for _, include := range payload.GetInclude() {
		args = append(args, "--include", include)
	}
	if payload.GetReplace() {
		args = append(args, "--replace")
	}
	if payload.GetDryRun() {
		return append(args, "--dry-run", "--sample", strconv.Itoa(defaultSample))
	}
	if payload.GetOverwrite() {
		args = append(args, "--overwrite")
	}
	return append(args, "--wait")
}

// restoreMounts are how a restore's job mounts its destinations.
func restoreMounts(dests []restoreDest, writes bool) []map[string]any {
	var mounts []map[string]any
	for i, dest := range dests {
		if mount := dest.mount(restoreJobMount+"/"+strconv.Itoa(i), writes); mount != nil {
			mounts = append(mounts, mount)
		}
	}
	return mounts
}

// toolProgress is a progress line of the backup tool.
type toolProgress struct {
	Step    string `json:"step"`
	Percent int64  `json:"percent"`
	Done    int64  `json:"done"`
	Total   int64  `json:"total"`
	Unit    string `json:"unit"`
	Root    int64  `json:"root"`
	Roots   int64  `json:"roots"`
}

// previewRestore runs a restore's dry run, and adds what the agent knows:
// the containers that would stop, whether each folder is there, and the
// room it has.
func (a *Agent) previewRestore(ctx context.Context, command *agentv1.Command, dests []restoreDest, notes []string, reporter *Reporter) (map[string]any, error) {
	payload := command.GetBackupRestore()
	// a PocketCloud container that writes the destination refuses it now
	users, err := a.affected(ctx, command.GetId(), payload.GetStack(), dests)
	if err != nil {
		return nil, err
	}
	reporter.Progress("check", "Checking the repository, the snapshot and the destination", -1)
	// the destinations read-only: the preview changes nothing, a folder that
	// is not there is not made, and restic may read what it compares with
	job := backupJobSpec{
		args: restoreToolArgs(payload, dests), mounts: restoreMounts(dests, false),
		capabilities: []string{"DAC_OVERRIDE"}, labels: commandLabels(command),
		progress: func(data []byte) {
			var progress toolProgress
			if json.Unmarshal(data, &progress) == nil && progress.Step == "preview" {
				reporter.Progress("preview", "Comparing the snapshot with the destination", -1)
			}
		},
	}
	out, err := a.backupJob(ctx, payload.GetRepository(), job, io.Discard)
	if err != nil {
		return nil, err
	}
	result, err := toolResult(out)
	if err != nil {
		return nil, err
	}
	wouldStop := []any{}
	for _, user := range users {
		wouldStop = append(wouldStop, map[string]any{"name": user.name, "project": user.project})
	}
	result["would_stop"] = wouldStop
	roots, _ := result["roots"].([]any)
	var sources []string
	for i, raw := range roots {
		root, _ := raw.(map[string]any)
		if root == nil || i >= len(dests) {
			continue
		}
		root["exists"] = dests[i].exists
		if free, ok := a.freeBytes(ctx, dests[i]); ok {
			root["free_bytes"] = free
		}
		if source, _ := root["source"].(string); source != "" {
			sources = append(sources, source)
		}
	}
	// what the snapshot holds that the job no longer backs up
	notInJob := []any{}
	if len(payload.GetOriginal()) > 0 {
		snapshot, _ := result["snapshot"].(map[string]any)
		paths, _ := snapshot["paths"].([]any)
		for _, raw := range paths {
			saved, _ := raw.(string)
			covered := slices.ContainsFunc(sources, func(source string) bool { return backup.Overlap(saved, source) })
			if saved != "" && !covered {
				notInJob = append(notInJob, saved)
			}
		}
	}
	result["not_in_job"] = notInJob
	toolNotes, _ := result["notes"].([]any)
	result["notes"] = append(anyList(notes), toolNotes...)
	trimPreview(result)
	return result, nil
}

// trimPreview cuts a preview to maxPreviewResult: its samples first, then
// its notes.
func trimPreview(result map[string]any) {
	samples, _ := result["samples"].(map[string]any)
	truncated, _ := result["truncated"].(map[string]any)
	for resultSize(result) > maxPreviewResult && samples != nil {
		longest := ""
		for kind, raw := range samples {
			if list, _ := raw.([]any); len(list) > 0 && (longest == "" || len(list) > len(samples[longest].([]any))) {
				longest = kind
			}
		}
		if longest == "" {
			break
		}
		list := samples[longest].([]any)
		cut := max(1, len(list)/4)
		samples[longest] = list[:len(list)-cut]
		if truncated != nil {
			count, _ := truncated[longest].(float64)
			truncated[longest] = count + float64(cut)
		}
	}
	trimList(result, "notes", maxPreviewResult)
}

// freeBytes is the room left where a destination is, or the nearest folder
// above it that is there.
func (a *Agent) freeBytes(ctx context.Context, dest restoreDest) (int64, bool) {
	dir := dest.dir
	if dest.volume != "" {
		volume, err := a.Docker.InspectVolume(ctx, dest.volume)
		if err != nil || volume.Mountpoint == "" {
			return 0, false
		}
		dir = volume.Mountpoint
	}
	for {
		if _, _, available, err := diskUsage(filepath.Join(a.hostRoot(), filepath.FromSlash(dir))); err == nil {
			return int64(min(available, uint64(1)<<62)), true //nolint:gosec // capped
		}
		if dir == "/" {
			return 0, false
		}
		dir = path.Dir(dir)
	}
}

// progressThrottle forwards a command's progress at most every so often, and
// a new step at once.
type progressThrottle struct {
	reporter *Reporter
	every    time.Duration
	mu       sync.Mutex
	step     string
	last     time.Time
}

func (p *progressThrottle) report(step, message string, percent int32, done, total int64, unit string) {
	p.mu.Lock()
	if step == p.step && time.Since(p.last) < p.every {
		p.mu.Unlock()
		return
	}
	p.step, p.last = step, time.Now()
	p.mu.Unlock()
	p.reporter.Report(&agentv1.CommandProgress{Step: step, Message: message, Percent: percent, Done: done, Total: total, Unit: unit})
}

// checkConfirmedStops refuses a restore that would stop a container its
// preview did not show, which a person did not see when they confirmed it.
func checkConfirmedStops(users []restoreUser, confirmed []string) error {
	for _, user := range users {
		if !slices.Contains(confirmed, user.name) {
			return sentenceError(user.name + " started using the destination after the preview: preview again.")
		}
	}
	return nil
}

// restoreCapabilities are what a restore's job may do: write files of any
// owner, and give them their owner, mode and setgid bit back. Never
// DAC_READ_SEARCH (see backupRunJob), SYS_ADMIN, SETFCAP or MKNOD.
var restoreCapabilities = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID"}

// runRestore runs a real restore: the job checks everything, the agent
// stops what uses the destination once the job is ready, the job restores,
// and the agent starts what it stopped again, whatever happened.
func (a *Agent) runRestore(ctx context.Context, command *agentv1.Command, dests []restoreDest, notes []string, reporter *Reporter) (map[string]any, error) {
	payload := command.GetBackupRestore()
	// the job ends early enough to start the application again and say so
	// before the panel gives up on the command
	jobCtx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		reserve := min(restoreDeadlineReserve, time.Until(deadline)/2)
		var cancel context.CancelFunc
		jobCtx, cancel = context.WithDeadline(ctx, deadline.Add(-reserve))
		defer cancel()
	}
	var keys []string
	for _, dest := range dests {
		keys = append(keys, dest.claimKey())
	}
	release, err := a.claims.claim(true, keys)
	if err != nil {
		return nil, err
	}
	defer release()

	progress := &progressThrottle{reporter: reporter, every: restoreProgressForward}
	progress.report("check", "Checking the repository, the snapshot and the destination", 5, 0, 0, "")
	// what would stop now, against what the preview showed: refused before
	// the job starts, and again, live, once it is ready
	users, err := a.affected(ctx, command.GetId(), payload.GetStack(), dests)
	if err != nil {
		return nil, err
	}
	if err := checkConfirmedStops(users, payload.GetConfirmedStops()); err != nil {
		return nil, err
	}

	session := &restoreSession{a: a, marker: restoreMarker{CommandID: command.GetId(), RestoreID: payload.GetRestoreId()}}
	var stopErr error
	wentAhead := false
	labels := make([]string, len(dests))
	for i, dest := range dests {
		labels[i] = dest.label()
	}
	job := backupJobSpec{
		args: restoreToolArgs(payload, dests), mounts: restoreMounts(dests, true), capabilities: restoreCapabilities,
		noCache: true, stopGrace: backupStopGrace, mark: restoreReady, labels: commandLabels(command),
		logConfig: map[string]any{"Type": "json-file", "Config": map[string]any{"max-size": "10m", "max-file": "1"}},
		feed: func(feedCtx context.Context, stdin io.WriteCloser) {
			defer stdin.Close()
			// the users of the destination as they are now
			users, err := a.affected(feedCtx, command.GetId(), payload.GetStack(), dests)
			if err == nil {
				err = checkConfirmedStops(users, payload.GetConfirmedStops())
			}
			if err == nil && len(users) > 0 {
				progress.report("stop", fmt.Sprintf("Stopping %d containers", len(users)), 10, 0, 0, "")
				session.users = users
				a.restoring.hold(users)
				err = session.stop(feedCtx)
			}
			if err != nil {
				stopErr = err
				_, _ = io.WriteString(stdin, "abort\n")
				return
			}
			wentAhead = true
			progress.report("restore", "Restoring the snapshot", 10, 0, 0, "")
			_, _ = io.WriteString(stdin, "go\n")
		},
		progress: func(data []byte) {
			var line toolProgress
			if json.Unmarshal(data, &line) != nil || line.Roots <= 0 {
				return
			}
			overall := 10 + 85*(line.Root*100+min(line.Percent, 100))/(line.Roots*100)
			message := "Restoring the snapshot"
			if line.Root < int64(len(labels)) {
				message = "Restoring into " + labels[line.Root]
			}
			progress.report("restore", message, int32(overall), line.Done, line.Total, line.Unit) //nolint:gosec // a percent
		},
	}
	repo := payload.GetRepository()
	log := &tailBuffer{limit: backupLogTail, mask: maskValues(backup.Secrets(repo.GetPassword(), repo.GetEnv(), repo.GetSshKey()))}
	output := io.MultiWriter(reporter, log)
	for _, note := range notes {
		fmt.Fprintln(output, note)
	}
	out, jobErr := a.backupJob(jobCtx, repo, job, output)

	// whatever ended the restore, what it stopped starts again
	if len(session.stopped) > 0 {
		progress.report("start", fmt.Sprintf("Starting %d containers again", len(session.stopped)), 97, 0, 0, "")
	}
	started := session.startAgain(len(session.stopped) > 0)
	if len(session.stopped) > 0 {
		progress.report("verify", "Checking that the containers run", 99, 0, 0, "")
	}

	result := map[string]any{}
	if len(out) > 0 {
		if decoded, err := toolResult(out); err == nil {
			result = decoded
		}
	}
	stopped := []any{}
	for _, container := range session.stopped {
		stopped = append(stopped, map[string]any{"id": container.ID, "name": container.Name})
	}
	result["dry_run"] = false
	result["app_stopped"] = stopped
	result["app_started"] = anyList(started.started)
	result["app_not_running"] = anyList(started.notRunning)
	result["start_errors"] = anyList(started.errors)
	if _, ok := result["refused"]; !ok {
		result["refused"] = ""
	}
	partial, _ := result["partial"].(bool)
	result["partial"] = partial || wentAhead && jobErr != nil
	fmt.Fprintln(log, restoreEnd(started))
	result["log"] = log.String()
	trimRestoreResult(result)

	startedAgain := ""
	if len(session.stopped) > 0 {
		startedAgain = " The application was started again."
	}
	// what happened decides, not what was asked last: a stop that came once
	// the job ended well, as the application starts again, changes nothing
	switch {
	case stopErr != nil && errors.Is(ctx.Err(), context.Canceled):
		// a stop while the containers stopped: the job never went ahead
		return result, sentenceError("The restore was stopped before anything changed.")
	case stopErr != nil:
		return result, stopErr
	case jobErr == nil:
		return result, nil
	case errors.Is(ctx.Err(), context.Canceled) && wentAhead:
		return result, sentenceError("The restore was stopped before it was complete: the destination holds part of the snapshot." + startedAgain)
	case errors.Is(ctx.Err(), context.Canceled):
		return result, sentenceError("The restore was stopped before anything changed.")
	case errors.Is(jobCtx.Err(), context.DeadlineExceeded) && wentAhead:
		return result, sentenceError("The restore ran out of time: the destination holds part of the snapshot." + startedAgain)
	default:
		return result, missingDirectory(jobErr)
	}
}

// sentenceError is an error said as a whole sentence, as the panel shows it.
type sentenceError string

func (e sentenceError) Error() string { return string(e) }

// restoreEnd is the log's last line of how the containers started again.
func restoreEnd(started restoreStart) string {
	switch {
	case len(started.started) == 0 && len(started.errors) == 0:
		return "No container used the destination."
	case len(started.errors) > 0:
		return fmt.Sprintf("Started %d containers again; %d problems: %v", len(started.started), len(started.errors), started.errors)
	case len(started.notRunning) > 0:
		return fmt.Sprintf("Started %d containers again; these did not stay up: %v", len(started.started), started.notRunning)
	}
	return fmt.Sprintf("Started %d containers again.", len(started.started))
}

// trimRestoreResult cuts a restore's result to maxRestoreResult: its log
// first, then its errors.
func trimRestoreResult(result map[string]any) {
	for resultSize(result) > maxRestoreResult {
		log, _ := result["log"].(string)
		if len(log) < 1024 {
			break
		}
		cut := len(log) / 2
		for cut < len(log) && !utf8.RuneStart(log[cut]) {
			cut++
		}
		result["log"] = "…" + log[cut:]
	}
	trimList(result, "error_sample", maxRestoreResult)
}
