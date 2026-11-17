package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/oldbear24/PocketServerManager/internal/backup"
)

// The backup tool's check op (#31): restic check of one group of a
// repository's data packs, or of its structure only, in one of two passes.
//
//	check  --subset n/t, group n of t of the data packs, or "", the
//	       structure only; --pass free: restic --no-lock check, which
//	       takes no lock and writes nothing, from a repository the job may
//	       mount read-only; --pass confirm: under restic's exclusive lock,
//	       which it waits 30 minutes for, and once more after restic unlock.
//	       restic keeps a temporary cache in the job's /tmp, which it
//	       removes. Its last line says the pass (backup.CheckPass), however
//	       it ended, and it fails unless the outcome is ok.
//
// restic 0.17's check says nothing as JSON, so the tool reads its text, and
// refuses any other restic. Its phases and counters on the standard output
// become the tool's progress marks; its error output becomes the pass's
// errors and the repairs restic advises, redacted.

const (
	// checkCacheDir is where restic check keeps its temporary cache: the
	// job's own /tmp, which goes with the job, and never the cache volume
	// the server's jobs share, in which a killed check would leave it.
	checkCacheDir = "/tmp/pocketcloud-check-cache"
	// checkRetryLock is how long pass 2 waits for the exclusive lock.
	checkRetryLock = "30m"
	// checkProgressFPS is how often restic says its counters while it
	// checks: every 5 seconds.
	checkProgressFPS = "0.2"
)

var (
	checkCounter     = regexp.MustCompile(`^\[[0-9:]+\] +([0-9.]+)%  ([0-9]+) / ([0-9]+) (snapshots|packs)$`)
	checkCount       = regexp.MustCompile(`^\[[0-9:]+\] +([0-9]+) (snapshots|packs)$`)
	checkReadGroup   = regexp.MustCompile(`^read group #[0-9]+ of ([0-9]+) data packs \(out of total ([0-9]+) packs in [0-9]+ groups\)$`)
	checkPackError   = regexp.MustCompile(`^pack ([0-9a-f]{64})[: ]`)
	checkTreeError   = regexp.MustCompile(`^error for tree ([0-9a-f]+):`)
	checkRepairPacks = regexp.MustCompile(`^restic repair packs ([0-9a-f]{64}( [0-9a-f]{64})*)$`)
	// checkBackendRetry is a line of the backend's retries, such as
	// "Load(<data/120f5d105f>, 508971, 0) failed: …" or "Save(<lock/…>)
	// returned error, retrying after 1.1s: …": the error that follows, if
	// any, says the same
	checkBackendRetry = regexp.MustCompile(`^[A-Z][A-Za-z]*\(<[a-z]+/[0-9a-f]+>[^)]*\) (failed|returned error, retrying after [^:]*): `)
)

// checkPass checks the check op's flags: a group n/t, or "" for the
// structure only, and the pass.
func checkPass(subset, pass string) error {
	if pass != backup.PassFree && pass != backup.PassConfirm {
		return fmt.Errorf("a check's --pass is free or confirm, not %q", pass)
	}
	_, _, err := backup.ParseSubset(subset)
	return err
}

// check runs one pass of restic check, and says it, whatever happened. The
// error is nil only when the outcome is ok.
func (r restic) check(ctx context.Context, subset, pass string) (backup.CheckPass, error) {
	result := checkResult(pass, subset)
	fail := func(err error) (backup.CheckPass, error) {
		result.Outcome, result.Error = backup.OutcomeError, backup.Redact(err.Error(), r.secrets...)
		return result, errors.New(result.Error)
	}
	if err := checkPass(subset, pass); err != nil {
		return fail(err)
	}
	// restic version and unlock keep no cache; check keeps a temporary one
	// in the job's own /tmp, whatever --cache said
	plain, checking := r, r
	plain.cache, checking.cache = "", checkCacheDir
	if err := plain.checkReadable(ctx); err != nil {
		return fail(err)
	}
	args := []string{"--no-lock", "check"}
	if pass == backup.PassConfirm {
		args = []string{"--retry-lock", checkRetryLock, "check"}
	}
	if subset != "" {
		args = append(args, "--read-data-subset="+subset)
	}
	var reader *checkReader
	run := func() (int, string, error) {
		reader = &checkReader{r: r, result: checkResult(pass, subset), structure: subset == "", tree: -1}
		return checking.stream(ctx, streamOptions{env: map[string]string{"RESTIC_PROGRESS_FPS": checkProgressFPS}, line: reader.line, errLine: reader.errLine}, args...)
	}
	code, said, err := run()
	if err == nil && code == resticLocked && pass == backup.PassConfirm {
		// a lock that no restic refreshes, of a job that was killed, keeps
		// the check out until restic unlock removes it
		plain.unlock(ctx, lockedRetry)
		code, said, err = run()
	}
	if err == nil && code != resticLocked {
		// without the backend's retries, which only say again what follows
		said = r.said([]byte(reader.words()))
	}
	if err != nil {
		if ctx.Err() != nil {
			err = errors.New("the check was stopped before it ended")
		}
		return fail(err)
	}
	result = reader.result
	result.ResticCode = code
	switch {
	case code == 0:
		result.Outcome = backup.OutcomeOK
	case code == 1 && (reader.contains || strings.Contains(said, "repository contains errors")):
		result.Outcome = backup.OutcomeErrors
	case code == resticLocked:
		result.Outcome, result.Error = backup.OutcomeLocked, said
		if r.seen != nil {
			result.Lock = r.seen.lock
		}
	case code == 1 && strings.Contains(said, "invalid lock file"):
		// a lock file restic cannot read: restic unlock leaves it
		result.Outcome, result.Error = backup.OutcomeLockInvalid, said
	default:
		result.Outcome, result.Error = backup.OutcomeError, repositoryError(code, said).Error()
	}
	if result.Outcome != backup.OutcomeErrors {
		// what restic said besides is in the log, and in Error
		result.Errors, result.ErrorsMore = []backup.CheckError{}, 0
		result.Repair = backup.CheckRepair{Packs: []string{}}
	}
	switch result.Outcome {
	case backup.OutcomeOK:
		return result, nil
	case backup.OutcomeErrors:
		return result, fmt.Errorf("restic found %d errors", len(result.Errors)+result.ErrorsMore)
	}
	return result, errors.New(result.Error)
}

// checkResult is a pass that found nothing yet, with every list empty.
func checkResult(pass, subset string) backup.CheckPass {
	return backup.CheckPass{Pass: pass, Subset: subset, Outcome: backup.OutcomeError, Errors: []backup.CheckError{},
		Repair: backup.CheckRepair{Packs: []string{}}, Notes: []string{}}
}

// checkReadable refuses a restic whose check's text the tool cannot read:
// any but 0.17, as check says nothing as JSON in it.
func (r restic) checkReadable(ctx context.Context) error {
	major, minor, version, err := r.version(ctx)
	if err != nil {
		return err
	}
	if major != 0 || minor != 17 {
		return fmt.Errorf("PocketCloud reads restic 0.17's check output; this image has restic %s", version)
	}
	return nil
}

// checkReader reads what one run of restic check prints: its standard output
// line by line into progress marks, notes and the log; its error output into
// errors and repairs. The two come in on goroutines of their own.
type checkReader struct {
	r         restic
	structure bool
	mu        sync.Mutex
	result    backup.CheckPass
	// contains: restic said "repository contains errors"
	contains bool
	// inTree: the lines indented below are a tree's errors; tree is that
	// tree's index in result.Errors, or -1 for one that was only counted
	inTree bool
	tree   int
	// said is what restic said on its error output, but the backend's
	// retries: its last lines
	said []string
	// noteOpen: the last note may go on on the next line
	noteOpen bool
}

// line reads a line of restic's standard output.
func (c *checkReader) line(raw []byte) {
	line := strings.TrimSpace(strings.TrimRight(string(raw), "\r"))
	c.mu.Lock()
	defer c.mu.Unlock()
	noteOpen := c.noteOpen
	c.noteOpen = false
	switch {
	case line == "":
	case line == "create exclusive lock for repository", strings.HasPrefix(line, "repo already locked, waiting up to "):
		c.progress("lock", 0, 0, 0, "")
	case line == "load indexes":
		c.progress("index", 0, 0, 0, "")
	case line == "check all packs":
		c.progress("packs", 0, 0, 0, "")
	case line == "check snapshots, trees and blobs":
		c.progress("snapshots", 0, 0, 0, "")
	case checkReadGroup.MatchString(line):
		m := checkReadGroup.FindStringSubmatch(line)
		c.result.PacksRead, _ = strconv.ParseInt(m[1], 10, 64)
		c.result.PacksTotal, _ = strconv.ParseInt(m[2], 10, 64)
		c.progress("read", 0, 0, c.result.PacksRead, "packs")
	case checkCounter.MatchString(line):
		m := checkCounter.FindStringSubmatch(line)
		percent, _ := strconv.ParseFloat(m[1], 64)
		done, _ := strconv.ParseInt(m[2], 10, 64)
		total, _ := strconv.ParseInt(m[3], 10, 64)
		c.progress(counterStep(m[4]), min(max(percent, 0), 100), done, total, m[4])
	case checkCount.MatchString(line):
		// a counter with nothing to count, as in an empty repository: its
		// step is done
		m := checkCount.FindStringSubmatch(line)
		done, _ := strconv.ParseInt(m[1], 10, 64)
		c.progress(counterStep(m[2]), 100, done, 0, m[2])
	case noteOpen && strings.HasPrefix(line, "This is non-critical"):
		last := len(c.result.Notes) - 1
		c.result.Notes[last] = cutText(c.result.Notes[last] + " " + c.redact(line))
		fmt.Fprintln(c.r.log, line)
	case strings.Contains(line, "additional files were found in the repo"),
		strings.HasPrefix(line, "Duplicate packs are non-critical"), strings.HasPrefix(line, "Mixed packs with tree and data blobs are non-critical"):
		// what restic says is no error
		c.result.Notes = append(c.result.Notes, cutText(c.redact(line)))
		c.noteOpen = true
		fmt.Fprintln(c.r.log, line)
	default:
		fmt.Fprintln(c.r.log, line)
	}
}

// counterStep is the step a counter of unit counts in.
func counterStep(unit string) string {
	if unit == "packs" {
		return "read"
	}
	return "snapshots"
}

// progress marks how far the pass got: percent of step, with what of it is
// done of how many in unit. The steps take the pass's percent from 0 to
// 100: the lock none, the index 0–5, the packs 5–15, the snapshots 15–45 and
// the read 45–100, or the snapshots 15–100 when only the structure is
// checked.
func (c *checkReader) progress(step string, percent float64, done, total int64, unit string) {
	var start, end int64
	switch step {
	case "index":
		start, end = 0, 5
	case "packs":
		start, end = 5, 15
	case "snapshots":
		start, end = 15, 45
		if c.structure {
			end = 100
		}
	case "read":
		start, end = 45, 100
	}
	overall := start + int64(float64(end-start)*percent/100)
	data, _ := json.Marshal(map[string]any{"step": step, "percent": overall, "done": done, "total": total, "unit": unit})
	c.r.mark(progressLine(data))
}

// errLine reads a line of restic's error output.
func (c *checkReader) errLine(raw string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	line := c.redact(strings.TrimRight(raw, "\r"))
	if trimmed := strings.TrimSpace(line); trimmed != "" && !checkBackendRetry.MatchString(trimmed) {
		c.said = append(c.said, trimmed)
		if len(c.said) > 64 {
			c.said = c.said[1:]
		}
	}
	if strings.HasPrefix(line, "  ") && c.inTree {
		// a line of the tree's errors
		if c.tree >= 0 && c.tree < len(c.result.Errors) {
			entry := &c.result.Errors[c.tree]
			entry.Text = cutText(entry.Text + " " + strings.TrimSpace(line))
		}
		return
	}
	c.inTree, c.tree = false, -1
	line = strings.TrimSpace(line)
	switch {
	case line == "", strings.HasPrefix(line, "The repository contains damaged pack files"), strings.HasPrefix(line, "Damaged pack files can be caused"):
	case strings.Contains(line, "repository contains errors"):
		c.contains = true
	case strings.HasPrefix(line, "Fatal:"), checkBackendRetry.MatchString(line):
		// what restic said in the end is the pass's error; the backend's
		// retries say again what the error after them says
	case strings.Contains(line, "restic repair index"):
		c.result.Repair.Index = true
		if strings.HasPrefix(line, "error:") {
			c.add(backup.CheckErrorOther, "", line)
		}
	case checkRepairPacks.MatchString(line):
		for _, id := range strings.Fields(strings.TrimPrefix(line, "restic repair packs ")) {
			if !slices.Contains(c.result.Repair.Packs, id) {
				c.result.Repair.Packs = append(c.result.Repair.Packs, id)
			}
		}
	case line == "restic repair snapshots --forget":
		c.result.Repair.Snapshots = true
	case strings.HasPrefix(line, "The repository is damaged and must be repaired"):
		c.result.Repair.Other = true
	case checkPackError.MatchString(line):
		c.add(backup.CheckErrorPack, checkPackError.FindStringSubmatch(line)[1], line)
	case checkTreeError.MatchString(line):
		c.inTree = true
		if c.add(backup.CheckErrorTree, checkTreeError.FindStringSubmatch(line)[1], line) {
			c.tree = len(c.result.Errors) - 1
		}
	default:
		c.add(backup.CheckErrorOther, "", line)
	}
}

// add keeps an error, unless as many as are kept are already there: it is
// counted then.
func (c *checkReader) add(kind, id, text string) bool {
	if len(c.result.Errors) >= backup.MaxCheckErrors {
		c.result.ErrorsMore++
		return false
	}
	c.result.Errors = append(c.result.Errors, backup.CheckError{Kind: kind, ID: id, Text: cutText(text)})
	return true
}

func (c *checkReader) redact(text string) string { return backup.Redact(text, c.r.secrets...) }

// words are the last lines restic said on its error output, but the
// backend's retries.
func (c *checkReader) words() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.said, "\n")
}

// cutText cuts a text restic said to backup.MaxCheckErrorText characters.
func cutText(text string) string {
	if utf8.RuneCountInString(text) <= backup.MaxCheckErrorText {
		return text
	}
	runes := []rune(text)
	return string(runes[:backup.MaxCheckErrorText-1]) + "…"
}
