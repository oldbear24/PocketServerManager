package backup

import (
	"fmt"
	"regexp"
	"strconv"
)

// What the agent and the panel say to each other about integrity checks and
// failed runs (#31), as Go types. The agent's answers travel as a
// structpb.Struct: it marshals these to JSON and unmarshals that into a
// map[string]any, as a struct or a typed slice makes structpb.NewStruct fail.
// The panel reads a command's result back into them from JSON. Every field
// of an answer is always there, with its zero value when it does not apply,
// except where omitempty says otherwise.

// MaxGroups is the most groups the packs of a repository are read in: restic
// 0.17.3's --read-data-subset=n/t takes t up to 256.
const MaxGroups = 256

var subsetForm = regexp.MustCompile(`^([1-9][0-9]{0,2})/([1-9][0-9]{0,2})$`)

// ParseSubset reads a check's subset, "n/t": group n of t groups of data
// packs. A pack is in group n when its first byte modulo t is n-1, so the
// same group reads the same packs every time, and t checks read them all.
// Empty is the structure only, with no data read (0, 0). restic's
// percentages and sizes pick packs at random, and are refused.
func ParseSubset(subset string) (n, t int, err error) {
	if subset == "" {
		return 0, 0, nil
	}
	m := subsetForm.FindStringSubmatch(subset)
	if m == nil {
		return 0, 0, fmt.Errorf("the subset %q is not group n/t of the data packs, such as 3/20", subset)
	}
	n, _ = strconv.Atoi(m[1])
	t, _ = strconv.Atoi(m[2])
	if n > t || t > MaxGroups {
		return 0, 0, fmt.Errorf("the subset %q is not a group n/t with 1 ≤ n ≤ t ≤ %d", subset, MaxGroups)
	}
	return n, t, nil
}

// Subset is the subset of group n of t; empty when t is 0, the structure
// only. n outside 1…t is taken as 1, as a new target's next group is 0.
func Subset(n, t int) string {
	if t <= 0 {
		return ""
	}
	if n < 1 || n > t {
		n = 1
	}
	return fmt.Sprintf("%d/%d", n, t)
}

// The passes of a check. The first reads the repository without a lock, from
// a read-only mount; only when it finds errors does the second read the same
// group again under restic's exclusive lock, which a prune that ran meanwhile
// cannot fool.
const (
	PassFree    = "free"
	PassConfirm = "confirm"
)

// The outcomes of one pass of restic check.
const (
	// OutcomeOK is exit code 0.
	OutcomeOK = "ok"
	// OutcomeErrors is exit code 1 with "repository contains errors".
	OutcomeErrors = "errors"
	// OutcomeLocked is exit code 11: another restic holds a lock.
	OutcomeLocked = "locked"
	// OutcomeLockInvalid is exit code 1 with "invalid lock file": a lock
	// file restic cannot read blocks every command that locks.
	OutcomeLockInvalid = "lock_invalid"
	// OutcomeError is any other failure: no repository, a wrong password,
	// the network, or a restic that is not 0.17.
	OutcomeError = "error"
)

// The kinds of error a check found.
const (
	// CheckErrorPack is a pack file: ID is its full id.
	CheckErrorPack = "pack"
	// CheckErrorTree is a tree, its short id in ID, its errors in Text.
	CheckErrorTree = "tree"
	// CheckErrorOther is any other line restic printed among its errors.
	CheckErrorOther = "other"
)

// CheckError is an error a check found, as restic printed it, redacted.
type CheckError struct {
	Kind string `json:"kind"`
	// ID is the pack's full id, or the tree's short one; empty for other.
	ID   string `json:"id,omitempty"`
	Text string `json:"text"`
}

// MaxCheckErrors and MaxCheckErrorText bound the errors a check keeps: the
// rest are counted in ErrorsMore.
const (
	MaxCheckErrors    = 20
	MaxCheckErrorText = 500
)

// CheckRepair is the repair restic advised.
type CheckRepair struct {
	// Index: "restic repair index".
	Index bool `json:"index"`
	// Packs are the ids of "restic repair packs", which restic advises with
	// "restic repair snapshots --forget".
	Packs []string `json:"packs"`
	// Snapshots: "restic repair snapshots --forget".
	Snapshots bool `json:"snapshots"`
	// Other: "The repository is damaged and must be repaired", with no
	// command.
	Other bool `json:"other"`
}

// CheckPass is the last line the backup tool's check op prints, always, for
// one pass, before it exits with an error unless the outcome is ok.
type CheckPass struct {
	Pass       string       `json:"pass"`
	Outcome    string       `json:"outcome"`
	ResticCode int          `json:"restic_code"`
	Subset     string       `json:"subset"`
	PacksRead  int64        `json:"packs_read"`
	PacksTotal int64        `json:"packs_total"`
	Errors     []CheckError `json:"errors"`
	ErrorsMore int          `json:"errors_more"`
	Repair     CheckRepair  `json:"repair"`
	// Notes are what restic says is not an error: additional files,
	// duplicate or mixed packs.
	Notes []string `json:"notes"`
	Lock  *Lock    `json:"lock"`
	// Error says why the pass did not complete, for locked, lock_invalid
	// and error.
	Error string `json:"error"`
}

// The states of a check as the agent answers it.
const (
	// CheckStatusOK: pass 1 found nothing, or pass 2 did not confirm what
	// pass 1 found (Unconfirmed).
	CheckStatusOK = "ok"
	// CheckStatusFailed: pass 2 confirmed errors (Confirmed).
	CheckStatusFailed = "failed"
	// CheckStatusSuspect: pass 1 found errors, and pass 2 could not lock
	// the repository, or failed, to confirm them.
	CheckStatusSuspect = "suspect"
	// CheckStatusError: pass 1 did not complete.
	CheckStatusError = "error"
)

// CheckResult is the agent's answer to backup.check. The command fails
// unless Status is ok, and still carries it.
type CheckResult struct {
	Status string `json:"status"`
	Subset string `json:"subset"`
	// PacksRead and PacksTotal are restic's "read group #n of X data packs
	// (out of total T packs …)".
	PacksRead  int64 `json:"packs_read"`
	PacksTotal int64 `json:"packs_total"`
	// Errors are pass 2's when it confirmed them (failed), else pass 1's:
	// an unconfirmed check says what pass 1 found, as does a suspect one.
	Errors     []CheckError `json:"errors"`
	ErrorsMore int          `json:"errors_more"`
	Repair     CheckRepair  `json:"repair"`
	Notes      []string     `json:"notes"`
	// Unconfirmed: pass 1 found errors that pass 2 did not.
	Unconfirmed bool `json:"unconfirmed"`
	// Confirmed: pass 2 found errors too.
	Confirmed bool `json:"confirmed"`
	// Lock is the lock that kept pass 2 out (suspect).
	Lock       *Lock `json:"lock"`
	ResticCode int   `json:"restic_code"`
	// Error says why the check did not complete (suspect, error).
	Error string `json:"error"`
	// Log is the end of the job's output, redacted.
	Log string `json:"log"`
}

// The classes of a failed backup run (#31). The agent says the first six
// when restic saved nothing; the panel adds the others, and classes older
// agents' failures by their text.
const (
	// FailureLocked: another restic held a lock, after the wait and one
	// unlock of stale locks. Retried.
	FailureLocked = "locked"
	// FailureLockInvalid: a lock file restic cannot read. Never retried:
	// restic unlock leaves it, and someone has to delete it.
	FailureLockInvalid = "lock_invalid"
	// FailureRepository: the repository or its bucket is gone (exit 10).
	FailureRepository = "repository"
	// FailurePassword: the password does not open the repository (exit 12).
	FailurePassword = "password"
	// FailureSource: the dump or the directory to back up failed.
	FailureSource = "source"
	// FailureRestic: any other failure of restic. Retried.
	FailureRestic = "restic"
	// FailurePartial: restic saved a snapshot that lacks files it could not
	// read (exit 3).
	FailurePartial = "partial"
	// FailureInterrupted: the agent restarted while the run went. Retried.
	FailureInterrupted = "interrupted"
	// FailureTimeout: the run did not finish in its time.
	FailureTimeout = "timeout"
)

// Failures lists the classes of a failed run.
var Failures = []string{
	FailureLocked, FailureLockInvalid, FailureRepository, FailurePassword,
	FailurePartial, FailureSource, FailureRestic, FailureInterrupted, FailureTimeout,
}

// RunFailure is what a backup.run's answer carries beside "log" and
// "refused" when restic saved nothing, and what the backup tool's backup op
// prints as its last line then.
type RunFailure struct {
	Failure    string `json:"failure,omitempty"`
	ResticCode int    `json:"restic_code,omitempty"`
	Lock       *Lock  `json:"lock,omitempty"`
}
