package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/backup"
)

// The backup tool's backup op (#29): restic backs up the directories the job
// mounts, or a dump that comes in through the relay, and then thins out the
// job's snapshots to its retention.

// backupRun is what the backup op backs up, and what of it the repository
// keeps.
type backupRun struct {
	host string
	tags []string
	// extraTags are the snapshot's tags besides tags, which may change for
	// a job, such as a database's server (#53): the retention selects by
	// tags alone, and takes every snapshot of them as one group
	extraTags []string
	paths     []string
	// excludes are restic's --exclude patterns, each under a path (#53)
	excludes []string
	// stdinName is the file name of a dump, which the tool serves on the
	// socket relay
	stdinName string
	relay     string
	keep      []keepOption
	// unlock runs restic unlock first: the retry of a run that found the
	// repository locked (#31)
	unlock bool
}

// keepOption is one of restic's --keep-* options.
type keepOption struct {
	name string
	n    int
}

// backupRunResult is what the backup op did.
type backupRunResult struct {
	SnapshotID   string `json:"snapshot_id"`
	BytesAdded   int64  `json:"bytes_added"`
	TotalBytes   int64  `json:"total_bytes"`
	FilesNew     int64  `json:"files_new"`
	FilesChanged int64  `json:"files_changed"`
	Forgotten    int    `json:"forgotten"`
	// RetentionError is what restic said when the snapshot was saved and
	// the retention failed.
	RetentionError string `json:"retention_error,omitempty"`
	// RetentionDeferred is restic's line that says who held the lock the
	// retention needs, when it waits for the next run.
	RetentionDeferred string `json:"retention_deferred,omitempty"`
	// BytesStored is what the snapshot added to the repository, packed and
	// compressed: restic's data_added_packed (#53).
	BytesStored int64 `json:"bytes_stored,omitempty"`
}

// backupRunOf checks the backup op's flags.
func backupRunOf(host string, tags, extraTags, paths, excludes []string, stdinName, relay string, keep []*int) (backupRun, error) {
	run := backupRun{host: host, tags: tags, extraTags: extraTags, paths: paths, excludes: excludes, stdinName: stdinName, relay: relay}
	if host == "" {
		return run, errors.New("a backup needs --host, the server's name, for its snapshot")
	}
	if (len(paths) > 0) == (stdinName != "") {
		return run, errors.New("a backup is of the directories of --path or of a dump, --stdin-name, and not both")
	}
	for _, tag := range append(slices.Clone(tags), extraTags...) {
		// a comma would split it, as restic reads one
		if tag == "" || strings.ContainsAny(tag, ", \n\r\x00") {
			return run, fmt.Errorf("a tag cannot be %q", tag)
		}
	}
	if len(extraTags) > 0 && len(tags) == 0 {
		return run, errors.New("extra tags come with the job's own tags, --tag, which its retention selects by")
	}
	for _, p := range paths {
		if !path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, "\n\r\x00") {
			return run, fmt.Errorf("a directory to back up is absolute and clean, not %q", p)
		}
	}
	if err := checkExcludes(excludes, paths); err != nil {
		return run, err
	}
	if stdinName != "" {
		if strings.ContainsAny(stdinName, "/\n\r\x00") || stdinName == "." || stdinName == ".." {
			return run, fmt.Errorf("the dump's file name cannot be %q", stdinName)
		}
		if relay == "" {
			return run, errors.New("a dump needs the relay's socket, --relay")
		}
	}
	for i, name := range keepOptions {
		switch n := *keep[i]; {
		case n < 0:
			return run, fmt.Errorf("--%s is a number of snapshots, not %d", name, n)
		case n > 0:
			run.keep = append(run.keep, keepOption{name: name, n: n})
		}
	}
	if len(run.keep) > 0 && len(tags) == 0 {
		return run, errors.New("the retention thins out the snapshots with the run's tags: without --tag it would take every other's")
	}
	return run, nil
}

// checkExcludes checks restic's --exclude patterns of a backup (#53): at
// most backup.MaxExcludes, each absolute, clean and under one of the paths
// backed up, as the job's paths name them; a dump has none.
func checkExcludes(excludes, paths []string) error {
	if len(excludes) > backup.MaxExcludes {
		return fmt.Errorf("a backup leaves out at most %d patterns, not %d", backup.MaxExcludes, len(excludes))
	}
	for _, exclude := range excludes {
		if !path.IsAbs(exclude) || path.Clean(exclude) != exclude || strings.ContainsAny(exclude, "\n\r\x00") {
			return fmt.Errorf("a pattern to leave out is absolute and clean, not %q", exclude)
		}
		if !slices.ContainsFunc(paths, func(p string) bool { return exclude != p && backup.Under(exclude, p) }) {
			return fmt.Errorf("%s is under no directory the backup saves", exclude)
		}
	}
	return nil
}

// resticSummary is the summary line of restic backup --json.
type resticSummary struct {
	MessageType         string `json:"message_type"`
	FilesNew            int64  `json:"files_new"`
	FilesChanged        int64  `json:"files_changed"`
	DataAdded           int64  `json:"data_added"`
	DataAddedPacked     int64  `json:"data_added_packed"`
	TotalBytesProcessed int64  `json:"total_bytes_processed"`
	SnapshotID          string `json:"snapshot_id"`
}

// summaryOf finds the summary in what restic backup --json printed.
func summaryOf(out []byte) *resticSummary {
	for _, line := range bytes.Split(out, []byte("\n")) {
		var summary resticSummary
		if json.Unmarshal(bytes.TrimSpace(line), &summary) == nil && summary.MessageType == "summary" && summary.SnapshotID != "" {
			return &summary
		}
	}
	return nil
}

func (s *resticSummary) result() backupRunResult {
	return backupRunResult{SnapshotID: s.SnapshotID, BytesAdded: s.DataAdded, TotalBytes: s.TotalBytesProcessed, FilesNew: s.FilesNew,
		FilesChanged: s.FilesChanged, BytesStored: s.DataAddedPacked}
}

// backup backs up a run's directories or dump, and applies its retention.
// The result of a snapshot that lacks files it could not read comes with
// the error; a backup that saved nothing once restic ran fails with a
// resticError, which says why.
func (r restic) backup(ctx context.Context, run backupRun, stdin io.Reader) (*backupRunResult, error) {
	if run.unlock {
		// plain restic unlock: it removes only the locks no restic holds
		// any more, never a live one, and never one it cannot read
		r.unlock(ctx, "Removing the locks no restic holds any more before the retry")
	}
	// --quiet: no progress, and the summary still; one --tag with commas
	// gives the snapshot every tag
	args := []string{"backup", "--json", "--quiet", "--retry-lock", retryLock, "--host", run.host}
	if tags := append(slices.Clone(run.tags), run.extraTags...); len(tags) > 0 {
		args = append(args, "--tag", strings.Join(tags, ","))
	}
	for _, exclude := range run.excludes {
		args = append(args, "--exclude", exclude)
	}
	var dump *dumpRelay
	if run.stdinName != "" {
		var err error
		if dump, err = startRelay(run.relay, stdin, r.mark); err != nil {
			return nil, err
		}
		defer dump.close()
		dump.panel = run.stdinName == backup.PanelFileName
		// restic runs the relay itself: this binary, with the dump's socket
		self, err := os.Executable()
		if err != nil {
			self = "pocketcloud-agent"
		}
		fmt.Fprintf(r.log, "Backing up %s as %s\n", dump.what(), run.stdinName)
		args = append(args, "--stdin-from-command", "--stdin-filename", run.stdinName, "--", self, "backup-tool", "relay", "--socket", run.relay)
	} else {
		fmt.Fprintf(r.log, "Backing up %s\n", strings.Join(run.paths, ", "))
		args = append(append(args, "--"), run.paths...)
	}
	out, code, said, err := r.run(ctx, args...)
	if err == nil && code == resticLocked && (dump == nil || !dump.connected()) {
		// a lock that no restic refreshes, of a job that was killed, keeps
		// every other out until restic unlock removes it
		r.unlock(ctx, lockedRetry)
		out, code, said, err = r.run(ctx, args...)
	}
	if err != nil {
		return nil, err
	}
	if dump != nil && code != 0 {
		// the dump's own words say why, before restic's that it failed
		if failure, exit := dump.failure(); failure != "" {
			class := backup.FailureSource
			if exit == backup.RelayRetry {
				// EX_TEMPFAIL: the panel's copy broke off or came damaged
				// (#32), or a dump that says another try may pass
				class = backup.FailureInterrupted
			}
			return nil, &resticError{code: code, failure: class, message: backup.Redact(failure, r.secrets...)}
		}
	}
	summary := summaryOf(out)
	switch {
	case code == resticPartial && summary != nil:
		result := summary.result()
		return &result, fmt.Errorf("restic saved the snapshot %.8s, but could not read everything: %s", summary.SnapshotID, said)
	case code != 0:
		return nil, r.failure(code, said)
	case summary == nil:
		return nil, &resticError{failure: backup.FailureRestic,
			message: fmt.Sprintf("restic saved no snapshot and said: %s", strings.TrimSpace(backup.Redact(string(out), r.secrets...)))}
	}
	result := summary.result()
	fmt.Fprintf(r.log, "Saved the snapshot %.8s: %d new and %d changed files, %s added, %s in all\n",
		result.SnapshotID, result.FilesNew, result.FilesChanged, sizeText(result.BytesAdded), sizeText(result.TotalBytes))
	if len(run.keep) > 0 {
		forgotten, err := r.forget(ctx, run.tags, len(run.extraTags) > 0, run.keep)
		result.Forgotten = forgotten
		// the snapshot is saved: the run succeeded, and the panel says the
		// retention did not
		var locked *lockedError
		switch {
		case errors.As(err, &locked):
			fmt.Fprintln(r.log, "The retention waits for the next run, as another restic uses the repository:", locked.said)
			result.RetentionDeferred = locked.said
		case err != nil:
			fmt.Fprintln(r.log, "The retention failed:", err)
			result.RetentionError = err.Error()
		}
	}
	return &result, nil
}

// resticError is a backup that saved nothing once restic ran (#31): its
// error is restic's words, as it was before, and it says the failure's class,
// restic's exit code and the lock that kept it out, for the panel's retries.
type resticError struct {
	code    int
	failure string
	message string
	lock    *backup.Lock
}

func (e *resticError) Error() string { return e.message }

// result is what the backup op says of it, as its last line.
func (e *resticError) result() backup.RunFailure {
	return backup.RunFailure{Failure: e.failure, ResticCode: e.code, Lock: e.lock}
}

// failure is the resticError of restic's exit code and what it said: worded
// as repositoryError words it, classed by the code and the words, with the
// lock of a repository that stayed locked.
func (r restic) failure(code int, said string) *resticError {
	failed := &resticError{code: code, failure: backup.FailureRestic, message: repositoryError(code, said).Error()}
	switch {
	case code == resticLocked:
		failed.failure = backup.FailureLocked
		if r.seen != nil {
			failed.lock = r.seen.lock
		}
	case code == 1 && strings.Contains(said, "invalid lock file"):
		// restic unlock leaves a lock file it cannot read
		failed.failure = backup.FailureLockInvalid
	case code == resticNoRepository, code == 1 && strings.Contains(said, "bucket does not exist"):
		failed.failure = backup.FailureRepository
	case code == resticWrongPassword:
		failed.failure = backup.FailurePassword
	}
	return failed
}

// lockedRetry is what the log says as an op that found the repository
// locked runs restic unlock, before it tries once more.
const lockedRetry = "The repository is locked: removing the locks no restic holds any more, and trying once more"

// lockedError is restic that could not lock the repository, once more after
// restic unlock.
type lockedError struct{ said string }

func (e *lockedError) Error() string { return e.said }

// forget thins out the snapshots that have all of tags to the retention,
// and prunes what they alone held. It counts the snapshots that went, also
// when the prune after them failed. oneGroup takes them all as one group,
// for a job whose snapshots have extra tags that may change (#53).
func (r restic) forget(ctx context.Context, tags []string, oneGroup bool, keep []keepOption) (int, error) {
	// one --tag with commas: snapshots with every tag, where several --tag
	// would take those with any; grouped by tags, as a job's are the same,
	// or not at all when its extra tags may differ, such as a database's
	// server after a move, whose old group would never be thinned out
	group := "tags"
	if oneGroup {
		group = ""
	}
	args := []string{"forget", "--json", "--prune", "--retry-lock", forgetRetryLock, "--group-by", group, "--tag", strings.Join(tags, ",")}
	var policy []string
	for _, option := range keep {
		args = append(args, "--"+option.name, strconv.Itoa(option.n))
		policy = append(policy, strings.TrimPrefix(option.name, "keep-")+" "+strconv.Itoa(option.n))
	}
	fmt.Fprintf(r.log, "Applying the retention: keep %s\n", strings.Join(policy, ", "))
	out, code, said, err := r.run(ctx, args...)
	if err == nil && code == resticLocked {
		r.unlock(ctx, lockedRetry)
		out, code, said, err = r.run(ctx, args...)
	}
	if err != nil {
		return 0, err
	}
	forgotten := 0
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("[")) {
			var groups []struct {
				Remove []json.RawMessage `json:"remove"`
			}
			if json.Unmarshal(line, &groups) == nil {
				for _, group := range groups {
					forgotten += len(group.Remove)
				}
				continue
			}
		}
		// the prune's report
		if len(line) > 0 {
			fmt.Fprintf(r.log, "%s\n", line)
		}
	}
	switch code {
	case 0:
	case resticLocked:
		return forgotten, &lockedError{said: said}
	default:
		return forgotten, errors.New(said)
	}
	fmt.Fprintf(r.log, "Forgot %d snapshots\n", forgotten)
	return forgotten, nil
}

// repositoryError words what restic said of a repository that a command
// expected to be there.
func repositoryError(code int, said string) error {
	switch {
	case code == resticNoRepository, code == 1 && strings.Contains(said, "bucket does not exist"):
		return errors.New("the repository is gone, with its snapshots: " + said)
	case code == resticWrongPassword:
		return errors.New("a repository is there, and the password is not its own: " + said)
	}
	return errors.New(said)
}

// dumpRelay serves restic's relay: it hands the job's standard input, the
// dump's frames, to the relay's connection as it comes, and watches them pass
// for how the dump ended.
type dumpRelay struct {
	listener net.Listener
	// accepted is closed once the relay connected, or never will
	accepted chan struct{}
	conn     net.Conn
	verdict  chan dumpVerdict
	// panel says that the frames are the panel's copy of itself, which
	// the agent fetched from the panel, not a dump (#32)
	panel bool
}

// what names what comes through the relay.
func (d *dumpRelay) what() string {
	if d.panel {
		return "the panel's copy"
	}
	return "the dump"
}

// dumpVerdict is how the frames that passed ended.
type dumpVerdict struct {
	end backup.DumpEnd
	err error
}

// dumpVerdictWait is how long a dump's end may take to pass the watch, once
// restic ended.
const dumpVerdictWait = 5 * time.Second

// startRelay listens for restic's relay, and serves it stdin; mark, when
// set, says relayReady once the relay has connected.
func startRelay(socket string, stdin io.Reader, mark func(string)) (*dumpRelay, error) {
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("the dump's relay cannot listen: %w", err)
	}
	d := &dumpRelay{listener: listener, accepted: make(chan struct{}), verdict: make(chan dumpVerdict, 1)}
	go d.serve(stdin, mark)
	return d, nil
}

func (d *dumpRelay) serve(stdin io.Reader, mark func(string)) {
	conn, err := d.listener.Accept()
	if err == nil {
		d.conn = conn
	}
	close(d.accepted)
	if err != nil {
		// restic ended without running the relay
		return
	}
	// restic has the repository's lock and runs the relay: the agent may
	// start the dump, which nothing read until now
	if mark != nil {
		mark(relayReady)
	}
	defer conn.Close()
	watch, passed := io.Pipe()
	go func() {
		end, err := backup.ReadFrames(watch, io.Discard)
		d.verdict <- dumpVerdict{end: end, err: err}
		_, _ = io.Copy(io.Discard, watch)
	}()
	_, err = io.Copy(conn, io.TeeReader(stdin, passed))
	passed.CloseWithError(err)
}

// failure is what a dump said that restic did not save, once restic ended,
// and the exit code of its last frame: "" when restic never ran the relay or
// the dump ended well, as restic's own error then says why. The code is 0
// without an exit frame; backup.RelayRetry is a failure another try may
// pass (#32).
func (d *dumpRelay) failure() (string, int) {
	d.close()
	<-d.accepted
	if d.conn == nil {
		return "", 0
	}
	select {
	case verdict := <-d.verdict:
		switch {
		case errors.Is(verdict.err, backup.ErrDumpCut):
			return d.what() + " broke off before it said how it went, and restic saved no snapshot", 0
		case verdict.err != nil, verdict.end.Code == 0:
			// restic stopped reading the dump, or failed after it
			return "", 0
		}
		message := strings.Join(strings.Fields(verdict.end.Message), " ")
		if d.panel && message != "" {
			// the agent's own words name the panel's copy and say why
			return "restic saved no snapshot: " + message, verdict.end.Code
		}
		verb := "failed"
		if d.panel {
			verb = "ended"
		}
		failure := fmt.Sprintf("%s %s with exit code %d, and restic saved no snapshot", d.what(), verb, verdict.end.Code)
		if message != "" {
			failure += ": " + message
		}
		return failure, verdict.end.Code
	case <-time.After(dumpVerdictWait):
		return "", 0
	}
}

// connected says whether the relay has connected: what it read of the dump
// is gone, and a dump is not read twice.
func (d *dumpRelay) connected() bool {
	select {
	case <-d.accepted:
		return d.conn != nil
	default:
		return false
	}
}

// close stops listening: no relay connects any more.
func (d *dumpRelay) close() { _ = d.listener.Close() }

// readable turns one of restic's JSON messages into what it says: an
// error's message, such as a file it could not read.
func readable(line string) string {
	if !strings.HasPrefix(strings.TrimSpace(line), "{") {
		return line
	}
	var message struct {
		MessageType string `json:"message_type"`
		Error       struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(line), &message) != nil || message.MessageType != "error" || message.Error.Message == "" {
		return line
	}
	return message.Error.Message
}

// readableWriter passes restic's error output on, line by line, with its
// JSON messages turned readable.
type readableWriter struct {
	w       io.Writer
	partial []byte
}

func (r *readableWriter) Write(data []byte) (int, error) {
	r.partial = append(r.partial, data...)
	for {
		end := bytes.IndexByte(r.partial, '\n')
		if end < 0 {
			return len(data), nil
		}
		line := readable(string(r.partial[:end]))
		r.partial = r.partial[end+1:]
		if _, err := io.WriteString(r.w, line+"\n"); err != nil {
			return len(data), err
		}
	}
}

// Flush passes on an unfinished last line.
func (r *readableWriter) Flush() {
	if len(r.partial) > 0 {
		_, _ = io.WriteString(r.w, readable(string(r.partial)))
		r.partial = nil
	}
}

// sizeText says a number of bytes as people read it.
func sizeText(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	value, prefix := float64(n)/unit, 0
	for value >= unit && prefix < 4 {
		value /= unit
		prefix++
	}
	return strconv.FormatFloat(value, 'f', 1, 64) + " " + string("KMGTP"[prefix]) + "iB"
}
