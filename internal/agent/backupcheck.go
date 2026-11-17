package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
)

// Integrity checks (#31). A check reads one group of a target's data packs,
// or its structure only, with restic check, in a job like the target's
// other jobs, on the target's own server. The first pass runs restic without
// a lock, from a local target's directory bound read-only: it writes
// nothing, and keeps no backup of any server waiting. Only when it finds
// errors does a second pass read the same group again under restic's
// exclusive lock, from the directory bound writable, as restic writes its
// lock there: a prune that ran meanwhile, from another server, makes the
// first pass see packs go that the second does not miss. restic keeps a
// temporary cache in the job's own /tmp, and no cache volume is mounted.

// checkProgressForward is how often a check's progress goes to the panel,
// which saves each: a new step goes at once. Tests shorten it.
var checkProgressForward = 5 * time.Second

// checkSteps word the steps of a pass, as the tool says them.
var checkSteps = map[string]string{
	"lock":      "Waiting for the repository's lock",
	"index":     "Loading the index",
	"packs":     "Checking the packs",
	"snapshots": "Checking the snapshots, trees and blobs",
	"read":      "Reading the data packs",
}

// handleBackupCheck checks a target's repository, one group of its data
// packs or its structure, in one or two passes, and says how it went
// (backup.CheckResult) whatever happened: the command fails unless the
// status is ok, and keeps the answer.
func (a *Agent) handleBackupCheck(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetBackupCheck()
	repo := payload.GetRepository()
	if err := checkRepository(repo); err != nil {
		return nil, err
	}
	subset := payload.GetReadDataSubset()
	if _, _, err := backup.ParseSubset(subset); err != nil {
		return nil, err
	}
	log := &tailBuffer{limit: backupLogTail, mask: maskValues(backup.Secrets(repo.GetPassword(), repo.GetEnv(), repo.GetSshKey()))}
	output := io.MultiWriter(reporter, log)
	progress := &progressThrottle{reporter: reporter, every: checkProgressForward}

	first := a.checkPass(ctx, command, repo, subset, backup.PassFree, output, progress)
	result := checkAnswer(first, subset)
	var err error
	switch first.Outcome {
	case backup.OutcomeOK:
		result.Status = backup.CheckStatusOK
	case backup.OutcomeErrors:
		if ctx.Err() != nil {
			// stopped as pass 1 ended: nothing confirms what it found
			result.Status, result.Error = backup.CheckStatusSuspect, checkStopped
			err = errors.New(checkStopped)
			break
		}
		fmt.Fprintf(output, "restic found errors without a lock, as a prune running meanwhile can cause: checking %s again with the repository locked\n", checkWhat(subset))
		second := a.checkPass(ctx, command, repo, subset, backup.PassConfirm, output, progress)
		result, err = confirmed(first, second, subset)
	default:
		result.Status = backup.CheckStatusError
		err = errors.New(first.Error)
	}
	result.Log = log.String()
	answer, encodeErr := checkResultMap(result)
	if encodeErr != nil {
		return nil, encodeErr
	}
	return answer, err
}

// checkStopped is the error of a check that was stopped.
const checkStopped = "the check was stopped before it ended"

// confirmed is the answer of a check whose pass 1 found errors, after pass 2
// read the same group again under the exclusive lock. Its restic_code is pass
// 2's, the last restic that ran.
func confirmed(first, second backup.CheckPass, subset string) (backup.CheckResult, error) {
	switch second.Outcome {
	case backup.OutcomeOK:
		// a prune that ran meanwhile, or a pack that could not be read
		// once: what pass 1 found is said, as it found it, with the packs
		// and notes of pass 2, which read the group to its end
		result := checkAnswer(first, subset)
		result.Status, result.Unconfirmed = backup.CheckStatusOK, true
		result.PacksRead, result.PacksTotal, result.Notes = second.PacksRead, second.PacksTotal, second.Notes
		result.ResticCode = second.ResticCode
		return result, nil
	case backup.OutcomeErrors:
		result := checkAnswer(second, subset)
		result.Status, result.Confirmed = backup.CheckStatusFailed, true
		return result, fmt.Errorf("restic found %d errors in %s", len(second.Errors)+second.ErrorsMore, checkWhat(subset))
	}
	// pass 2 did not get to its end: what pass 1 found stays unconfirmed
	result := checkAnswer(first, subset)
	result.Status = backup.CheckStatusSuspect
	result.Lock, result.Error, result.ResticCode = second.Lock, second.Error, second.ResticCode
	return result, errors.New(second.Error)
}

// checkWhat names what a check reads: the group n/t, or the structure.
func checkWhat(subset string) string {
	if subset == "" {
		return "the repository's structure"
	}
	return "group " + subset
}

// checkAnswer is a check's answer with what one pass found.
func checkAnswer(pass backup.CheckPass, subset string) backup.CheckResult {
	return backup.CheckResult{
		Subset: subset, PacksRead: pass.PacksRead, PacksTotal: pass.PacksTotal,
		Errors: pass.Errors, ErrorsMore: pass.ErrorsMore, Repair: pass.Repair, Notes: pass.Notes,
		Lock: pass.Lock, ResticCode: pass.ResticCode, Error: pass.Error,
	}
}

// checkResultMap is a check's answer as the stream carries it: structpb
// takes no struct or typed slice, so it goes through JSON.
func checkResultMap(result backup.CheckResult) (map[string]any, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	var answer map[string]any
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, err
	}
	return answer, nil
}

// checkPass runs one pass of a check in a job of its own, and says what it
// found, also when the job did not run.
func (a *Agent) checkPass(ctx context.Context, command *agentv1.Command, repo *agentv1.BackupRepository, subset, pass string,
	output io.Writer, progress *progressThrottle) backup.CheckPass {
	job := backupJobSpec{
		args: []string{"check", "--subset", subset, "--pass", pass},
		// restic keeps a temporary cache in the job's /tmp, which goes with
		// the job: a killed check leaves nothing in the cache volume
		noCache: true,
		// pass 1 writes nothing; pass 2 writes, refreshes and removes its
		// exclusive lock, and needs the directory writable
		repoReadOnly: pass == backup.PassFree,
		// a pass 2 that is killed leaves its exclusive lock for 30 minutes
		stopGrace: backupStopGrace,
		labels:    commandLabels(command),
		logConfig: map[string]any{"Type": "json-file", "Config": map[string]any{"max-size": "10m", "max-file": "1"}},
		progress: func(data []byte) {
			var line toolProgress
			if json.Unmarshal(data, &line) != nil {
				return
			}
			step, message := line.Step, checkSteps[line.Step]
			if message == "" {
				message = "Checking the repository"
			}
			if line.Step == "read" && subset != "" {
				message = "Reading group " + subset + " of the data packs"
			}
			if pass == backup.PassConfirm {
				// pass 2 is one step, whatever restic does
				step, message = "confirm", "Checking "+checkWhat(subset)+" again with the repository locked: "+message
			}
			progress.report(step, message, int32(min(max(line.Percent, 0), 100)), line.Done, line.Total, line.Unit) //nolint:gosec // a percent
		},
	}
	out, err := a.backupJob(ctx, repo, job, output)
	var said backup.CheckPass
	if len(out) > 0 && json.Unmarshal(out, &said) == nil && said.Outcome != "" {
		return said
	}
	// the job did not run, or ended before the tool said the pass
	said = backup.CheckPass{Pass: pass, Subset: subset, Outcome: backup.OutcomeError,
		Errors: []backup.CheckError{}, Repair: backup.CheckRepair{Packs: []string{}}, Notes: []string{}}
	switch {
	case ctx.Err() != nil:
		said.Error = checkStopped
	case err != nil:
		said.Error = missingDirectory(err).Error()
	default:
		said.Error = fmt.Sprintf("the backup tool said: %s", out)
	}
	return said
}
