package panel

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// Retries of failed runs (#31). A run that started and failed in a way that
// may pass by itself (another restic held a lock, restic failed otherwise,
// or the agent restarted) is tried again by a run of its own: after 5
// minutes, 30 minutes and 2 hours, 4 attempts in all, unless the job's next
// scheduled run comes first. A run a lock kept out is tried again no sooner
// than restic counts the lock as stale, and its retry's job runs restic
// unlock first, which removes only stale locks; while the lock's holder is a
// PocketCloud command that still runs, the retry waits for it. The retry is
// planned on the run (retry_at) and claimed in one transaction with its
// command, so that a retry is neither lost across a restart nor made twice.
// The run's alert is a warning while a retry is planned, critical once none
// is left.

const (
	// maxRunAttempts is how many times the panel runs a backup that fails,
	// the first run included.
	maxRunAttempts = 4
	// holderPatience is how long after a run failed the panel waits for a
	// lock's holder that still runs before it gives up the retry.
	holderPatience = 12 * time.Hour
	// retryMissedAfter is how late a retry may start, as after the panel
	// was down, before it is given up.
	retryMissedAfter = 24 * time.Hour
	// retryRearm is how long a retry waits again for a lock's holder that
	// still runs.
	retryRearm = 30 * time.Minute
	// retryNudge is how long a retry waits for its server, a restore, or the
	// run before it; and how close the job's next run may come before it
	// takes the retry's place.
	retryNudge = 10 * time.Minute
	// retryBatch is how many due retries a pass of the scheduler starts.
	retryBatch = 50
	// maxLockSkew is how far a server's clock may be off the panel's for an
	// unlock on it: restic judges a lock's age by it.
	maxLockSkew = 5 * time.Minute
)

// runBackoff is how long after a run failed it is tried again, by attempt.
var runBackoff = []time.Duration{5 * time.Minute, 30 * time.Minute, 2 * time.Hour}

// agentRestarted starts the error of a command whose agent restarted while it
// ran.
const agentRestarted = "The agent restarted while running"

// jobDisabledNote says that a disabled job's run is not tried again.
const jobDisabledNote = "The job is disabled, so it is not tried again."

var (
	// errRetryTaken says the run is tried again already.
	errRetryTaken = errors.New("it is being tried again already")
	// errRunNewer says the job has a newer run to the target.
	errRunNewer = errors.New("a newer run of this job to this target exists")
	// errNotFailed says the run did not fail.
	errNotFailed = errors.New("only a failed run is tried again")
)

// runFailure classes a run that failed, and reads the lock that kept it out:
// from the agent's answer, or for an older agent from its error.
func runFailure(command *core.Record, result backupRunResult, started bool) (string, *backup.Lock) {
	message := command.GetString("error")
	class, lock := "", (*backup.Lock)(nil)
	switch {
	case started && result.SnapshotID != "":
		class = backup.FailurePartial
	case started && command.GetString("status") == schema.CommandTimedOut:
		class = backup.FailureTimeout
	case strings.HasPrefix(message, agentRestarted):
		class = backup.FailureInterrupted
	case slices.Contains(backup.Failures, result.Failure):
		class, lock = result.Failure, result.Lock
	case !started:
		// it never ran: delivery refused it, or its agent cannot run it
	case strings.Contains(message, "is already locked"):
		class = backup.FailureLocked
	case strings.Contains(message, "invalid lock file"):
		class = backup.FailureLockInvalid
	case strings.Contains(message, "the repository is gone"):
		class = backup.FailureRepository
	case strings.Contains(message, "the password is not its own"):
		class = backup.FailurePassword
	case strings.Contains(message, "does not exist on this server"), strings.Contains(message, "the dump failed"):
		class = backup.FailureSource
	default:
		class = backup.FailureRestic
	}
	if class == backup.FailureLocked && lock == nil {
		// an older agent's error names the holder alone
		lock = backup.ParseLockLine(message)
	}
	return class, lock
}

// describeLock adds to a lock the command that holds it and its server, when
// the lock's host is a job's, pc-<command id>, and that command exists.
func (p *Panel) describeLock(lock *backup.Lock) *backup.Lock {
	if lock == nil {
		return nil
	}
	described := *lock
	id := backup.CommandOfHost(lock.Host)
	if id == "" {
		return &described
	}
	command, err := p.App.FindRecordById(schema.Commands, id)
	if err != nil {
		return &described
	}
	described.Command = command.Id
	described.CommandType = command.GetString("type")
	described.CommandLabel = command.GetString("label")
	described.CommandHost = command.GetString("host")
	described.CommandHostName = command.GetString("host_name")
	return &described
}

// runLock reads the lock a run keeps; nil when it has none.
func runLock(run *core.Record) *backup.Lock {
	var lock *backup.Lock
	if err := run.UnmarshalJSONField("lock", &lock); err != nil || lock == nil || lock.Host == "" {
		return nil
	}
	return lock
}

// holderState is whether a lock's holder still runs, as far as the panel
// knows.
type holderState int

const (
	// holderUnknown is a holder PocketCloud does not run: another hostname.
	holderUnknown holderState = iota
	// holderAlive is a PocketCloud command that is queued, delivered or
	// running, or that timed out while its server was not connected, which
	// may still run it.
	holderAlive
	// holderGone is a PocketCloud command that ended, or is gone.
	holderGone
)

// lockHolder says whether the holder of a lock still runs, and is its command
// when the panel has it. The panel decides only whether to wait: restic's
// unlock never removes a lock that its holder still refreshes.
func (p *Panel) lockHolder(lock *backup.Lock) (holderState, *core.Record) {
	if lock == nil {
		return holderUnknown, nil
	}
	id := lock.Command
	if id == "" {
		id = backup.CommandOfHost(lock.Host)
	}
	if id == "" {
		return holderUnknown, nil
	}
	command, err := p.App.FindRecordById(schema.Commands, id)
	if err != nil {
		return holderGone, nil
	}
	switch command.GetString("status") {
	case schema.CommandQueued, schema.CommandDispatched, schema.CommandRunning:
		return holderAlive, command
	case schema.CommandTimedOut:
		// the panel could not tell its agent to stop it
		if !p.Commands.Connected(command.GetString("host")) {
			return holderAlive, command
		}
	}
	return holderGone, command
}

// holderNames names a lock's holder and its server in words.
func holderNames(lock *backup.Lock, holder *core.Record) (label, server string) {
	switch {
	case holder != nil:
		return holder.GetString("label"), holder.GetString("host_name")
	case lock != nil && lock.CommandLabel != "":
		return lock.CommandLabel, lock.CommandHostName
	case lock != nil:
		return "restic", lock.Host
	}
	return "restic", ""
}

// retryPlan is the retry a failed run gets: when, and which attempt it is;
// or why it gets none, and how its retry ended (schema.Retry*), when the run
// would have been tried again.
type retryPlan struct {
	at      time.Time
	attempt int
	note    string
	end     string
}

// planRetry plans the retry of a run that failed with a class: one that
// started and may pass by itself, before its last attempt, unless the job's
// next scheduled run comes first; never one of a job that is disabled, or
// whose label is gone, which the scheduler would drop. A run its agent
// stored but had not started yet, as it waited for a free worker, when the
// agent restarted is interrupted too, and tried again: nothing of it ran. A
// job whose database or site went (#53) gets none either, as one whose label
// went. A
// lock's retry waits until restic counts the lock as stale, or follows its
// holder while it still runs.
func (p *Panel) planRetry(job *core.Record, attempt int, failure string, lock *backup.Lock, started bool, finished time.Time) retryPlan {
	attempt = max(attempt, 1)
	switch failure {
	case backup.FailureLocked, backup.FailureRestic, backup.FailureInterrupted:
	default:
		return retryPlan{}
	}
	if !started && failure != backup.FailureInterrupted {
		return retryPlan{}
	}
	switch {
	case attempt >= maxRunAttempts:
		return retryPlan{end: schema.RetryGivenUp}
	case !job.GetBool("enabled"):
		// as a disabled job's run by hand
		return retryPlan{note: jobDisabledNote, end: schema.RetryGivenUp}
	case sourceGone(job):
		return retryPlan{note: goneMessage(job), end: schema.RetryGivenUp}
	}
	at := finished.UTC().Add(runBackoff[attempt-1])
	if failure == backup.FailureLocked {
		if stale := lock.StaleAt(); stale.After(at) {
			at = stale
		}
		if state, _ := p.lockHolder(lock); state == holderAlive {
			at = finished.UTC().Add(runBackoff[0])
		}
	}
	if next, ok := nextRunOf(job, finished).(time.Time); ok && !next.After(at.Add(retryNudge)) {
		return retryPlan{note: nextRunNote(next), end: schema.RetryNextRun}
	}
	return retryPlan{at: at, attempt: attempt + 1}
}

// nextRunNote says that a job's next run takes a retry's place.
func nextRunNote(next time.Time) string {
	return fmt.Sprintf("The next scheduled run at %s UTC takes its place.", next.UTC().Format("15:04"))
}

// retryDueRuns tries again the failed runs whose retry is due, oldest first.
func (p *Panel) retryDueRuns(now time.Time) {
	at, err := types.ParseDateTime(now.UTC())
	if err != nil {
		return
	}
	due, err := p.App.FindRecordsByFilter(schema.BackupRuns, "status={:failed} && retry_at!='' && retry_at<={:now}", "retry_at,id", retryBatch, 0,
		dbx.Params{"failed": schema.RunFailed, "now": at.String()})
	if err != nil {
		p.Log.Error("failed to read the retries of backup runs", "error", err)
		return
	}
	for _, run := range due {
		p.retryDueRun(run, now.UTC())
	}
}

// retryDueRun tries a failed run again, or gives its retry up, or moves it.
func (p *Panel) retryDueRun(run *core.Record, now time.Time) {
	job, err := p.App.FindRecordById(schema.BackupJobs, run.GetString("job"))
	if err != nil {
		return
	}
	target, err := p.App.FindRecordById(schema.BackupTargets, run.GetString("target"))
	if err != nil {
		return
	}
	// a retry given up raises the run's alert again, critical, when nothing
	// else settles it: the alerts of a job whose label went, or of a target
	// taken out of it, were resolved, and a newer run's outcome is the
	// alert's now
	giveUp := func(end, note string, alert bool) {
		if p.endRetry(run, end, note) && alert {
			p.alertFailure(job, target, run, retryGivenUp(run, note), retryPlan{})
		}
	}
	host, err := p.App.FindRecordById(schema.Hosts, job.GetString("host"))
	if err != nil {
		giveUp(schema.RetryGivenUp, "The server of the job is gone.", true)
		return
	}
	switch {
	case !job.GetBool("enabled"):
		giveUp(schema.RetryGivenUp, jobDisabledNote, true)
		return
	case sourceGone(job):
		giveUp(schema.RetryGivenUp, goneMessage(job), false)
		return
	case !backsUpTo(job, target):
		giveUp(schema.RetryGivenUp, "The target is no longer one of the job's targets.", false)
		return
	case newerRunIn(p.App, run):
		giveUp(schema.RetryGivenUp, "A newer run of the job to the target took its place.", false)
		return
	}
	if next, ok := nextRunOf(job, now).(time.Time); ok && !next.After(now.Add(retryNudge)) {
		giveUp(schema.RetryNextRun, nextRunNote(next), true)
		return
	}

	lock := runLock(run)
	state, holder := holderUnknown, (*core.Record)(nil)
	if run.GetString("failure") == backup.FailureLocked {
		state, holder = p.lockHolder(lock)
	}
	label, _ := holderNames(lock, holder)
	retryAt := run.GetDateTime("retry_at").Time()
	waited := now.Sub(run.GetDateTime("finished_at").Time())
	switch {
	case state == holderAlive && waited < holderPatience:
		// not an attempt: the holder ends first
		p.moveRetry(run, now.Add(retryRearm), fmt.Sprintf("Waits for %s.", label))
	case state == holderAlive:
		giveUp(schema.RetryGivenUp, fmt.Sprintf("The repository stayed locked by %s for %d hours.", label, int(holderPatience.Hours())), true)
	case now.Sub(retryAt) > retryMissedAfter:
		giveUp(schema.RetryGivenUp, "Its retry was missed while the panel was down.", true)
	case p.runGoing(job, target) || p.offline(host) || p.restoreWriting(job) != nil:
		p.moveRetry(run, maxTime(retryAt, now).Add(retryNudge), run.GetString("retry_note"))
	default:
		_, err := p.claimRetry(context.Background(), run, job, target, host, run.GetString("failure") == backup.FailureLocked, commands.Options{Requester: "schedule"})
		if err != nil && !errors.Is(err, errRetryTaken) && !errors.Is(err, errRunNewer) && !errors.Is(err, errNotFailed) {
			p.Log.Error("failed to try a backup run again", "run", run.Id, "error", err)
		}
	}
}

// retryGivenUp is the reason of a failed run's alert once its retry was
// given up: its error, and why.
func retryGivenUp(run *core.Record, note string) string {
	note = strings.TrimSuffix(note, ".")
	if note != "" {
		note = strings.ToLower(note[:1]) + note[1:]
	}
	return strings.TrimSuffix(run.GetString("error"), ".") + "; " + note
}

// maxTime is the later of two moments.
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// moveRetry moves a run's planned retry to another moment, saying why. It
// tells whether it did: the retry may have been claimed or given up
// meanwhile.
func (p *Panel) moveRetry(run *core.Record, at time.Time, note string) bool {
	return p.setRetry(run, at, "", note)
}

// endRetry gives up a run's planned retry without making it, saying why and
// how it ended: schema.RetryNextRun when the job's next scheduled run takes
// its place, schema.RetryGivenUp otherwise. It tells whether it did.
func (p *Panel) endRetry(run *core.Record, end, note string) bool {
	return p.setRetry(run, "", end, note)
}

// setRetry moves a run's planned retry, or with an empty at ends it.
func (p *Panel) setRetry(run *core.Record, at any, end, note string) bool {
	moved := false
	err := p.update(context.Background(), run, func(fresh *core.Record) error {
		if fresh.GetDateTime("retry_at").IsZero() || fresh.GetString("status") != schema.RunFailed {
			return errUnchanged
		}
		fresh.Set("retry_at", at)
		fresh.Set("retry_end", end)
		fresh.Set("retry_note", cutText(note, 500))
		moved = true
		return nil
	})
	if err != nil {
		p.Log.Error("failed to move the retry of a backup run", "run", run.Id, "error", err)
	}
	return moved
}

// newerRunIn tells whether a job has a run to a target newer than a run, as
// the app (a transaction's) sees it. A run that never ran does not take a
// run's place: one skipped, as at the job's next slot while the run before
// still waited on a lock, nor one cancelled before it started.
func newerRunIn(app core.App, run *core.Record) bool {
	newer, err := app.FindRecordsByFilter(schema.BackupRuns,
		"job={:job} && target={:target} && (created>{:created} || (created={:created} && id>{:id}))"+
			" && status!={:skipped} && (status!={:cancelled} || started_at!='')", "", 1, 0,
		dbx.Params{"job": run.GetString("job"), "target": run.GetString("target"), "created": run.GetDateTime("created").String(), "id": run.Id,
			"skipped": schema.RunSkipped, "cancelled": schema.RunCancelled})
	return err != nil || len(newer) > 0
}

// retryNeverRanNote says on a retry that ended without having run that the
// run it was to try again may be tried again.
const retryNeverRanNote = "It never ran, so the run it was to try again may be tried again."

// freeRetryOf frees the run that a retry ending without having run, skipped
// or cancelled before it started, was to try again: it clears the retry's
// retry_of, saying why, so that the route and the scheduler may try that
// run again, which the unique retry_of would refuse for good. It returns the
// run it freed, "" for none.
func freeRetryOf(retry *core.Record) string {
	prev := retry.GetString("retry_of")
	status := retry.GetString("status")
	neverRan := status == schema.RunSkipped || (status == schema.RunCancelled && retry.GetDateTime("started_at").IsZero())
	if prev == "" || !neverRan {
		return ""
	}
	retry.Set("retry_of", "")
	retry.Set("retry_note", retryNeverRanNote)
	return prev
}

// retryNeverRan raises again, critical, the alert of a run whose retry never
// ran: the alert promised that retry, and no other is planned. A run that is
// tried again meanwhile, or whose place a newer run took, is left alone.
func (p *Panel) retryNeverRan(runID string) {
	if runID == "" {
		return
	}
	run, err := p.App.FindRecordById(schema.BackupRuns, runID)
	if err != nil || run.GetString("status") != schema.RunFailed || !run.GetDateTime("retry_at").IsZero() || retriedIn(p.App, run.Id) || newerRunIn(p.App, run) {
		return
	}
	job, err := p.App.FindRecordById(schema.BackupJobs, run.GetString("job"))
	if err != nil {
		return
	}
	target, err := p.App.FindRecordById(schema.BackupTargets, run.GetString("target"))
	if err != nil {
		return
	}
	p.alertFailure(job, target, run, retryGivenUp(run, "Its retry never ran."), retryPlan{})
}

// retriedIn tells whether a run is tried again by another, as the app (a
// transaction's) sees it.
func retriedIn(app core.App, runID string) bool {
	children, err := app.FindRecordsByFilter(schema.BackupRuns, "retry_of={:run}", "", 1, 0, dbx.Params{"run": runID})
	return err != nil || len(children) > 0
}

// claimRetry tries a failed run again, in one transaction: it reads the run
// again, which must still be failed, the newest of its job to its target, and
// not tried again yet; saves the retry, its next attempt, and its command;
// and gives up the run's planned retry. unlock has the retry's job run
// restic unlock first. A retry whose command cannot be made fails at once,
// with a critical alert.
func (p *Panel) claimRetry(ctx context.Context, prev, job, target, host *core.Record, unlock bool, options commands.Options) (*core.Record, error) {
	run, err := p.newRunRecord(job, target, host, schema.RunRetry, time.Time{})
	if err != nil {
		return nil, err
	}
	run.Set("attempt", max(prev.GetInt("attempt"), 1)+1)
	run.Set("retry_of", prev.Id)
	payload, payloadErr := p.backupRun(job, target, host, run)
	if payloadErr == nil {
		payload.Unlock = unlock
	}
	err = p.App.RunInTransaction(func(tx core.App) error {
		fresh, err := tx.FindRecordById(schema.BackupRuns, prev.Id)
		if err != nil {
			return err
		}
		switch {
		case fresh.GetString("status") != schema.RunFailed:
			return errNotFailed
		case retriedIn(tx, prev.Id):
			return errRetryTaken
		case newerRunIn(tx, fresh):
			return errRunNewer
		}
		fresh.Set("retry_at", "")
		if err := tx.Save(fresh); err != nil {
			return err
		}
		if payloadErr != nil {
			run.Set("status", schema.RunFailed)
			run.Set("error", cutText(sentence(payloadErr), 4096))
			run.Set("failure", refusalClass(payloadErr))
			run.Set("finished_at", types.NowDateTime())
			return tx.Save(run)
		}
		return p.queueRunIn(ctx, tx, run, payload, job, target, host, options)
	})
	if uniqueViolation(err) {
		return nil, errRetryTaken
	}
	if err != nil {
		return nil, err
	}
	if payloadErr != nil {
		p.alertFailure(job, target, run, sentence(payloadErr), retryPlan{})
	}
	p.noteJobRun(job.Id)
	return run, nil
}

// uniqueViolation tells whether a save failed on a unique index.
func uniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var errs validation.Errors
	if errors.As(err, &errs) {
		for _, fieldErr := range errs {
			var code validation.ErrorObject
			if errors.As(fieldErr, &code) && code.Code() == "validation_not_unique" {
				return true
			}
		}
	}
	return strings.Contains(strings.ToLower(err.Error()), "unique constraint failed")
}

// dropRetries gives up the planned retries of a job's runs to a target, as a
// run started by hand takes their place.
func (p *Panel) dropRetries(job, target *core.Record, note string) {
	runs, err := p.App.FindRecordsByFilter(schema.BackupRuns, "job={:job} && target={:target} && retry_at!=''", "", 0, 0,
		dbx.Params{"job": job.Id, "target": target.Id})
	if err != nil {
		p.Log.Error("failed to read the retries of backup runs", "job", job.Id, "error", err)
		return
	}
	for _, run := range runs {
		p.endRetry(run, schema.RetryGivenUp, note)
	}
}

// handleRetryRun tries a failed run again now, after restic unlock when a
// lock kept it out. While the lock's holder still runs, the retry is refused,
// and for a lock restic does not count as stale yet, it is planned for when
// restic does.
func (p *Panel) handleRetryRun(e *core.RequestEvent) error {
	run, err := e.App.FindRecordById(schema.BackupRuns, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The run does not exist.", err)
	}
	conflict := func(message string) error { return e.Error(http.StatusConflict, message, nil) }
	switch {
	case run.GetString("status") != schema.RunFailed:
		return conflict("Only a failed run is tried again.")
	case newerRunIn(e.App, run):
		return conflict("A newer run of this job to this target exists.")
	case retriedIn(e.App, run.Id):
		return conflict("It is being tried again already.")
	}
	job, err := e.App.FindRecordById(schema.BackupJobs, run.GetString("job"))
	if err != nil {
		return e.NotFoundError("The run does not exist.", err)
	}
	if sourceGone(job) {
		return conflict(goneMessage(job))
	}
	target, err := e.App.FindRecordById(schema.BackupTargets, run.GetString("target"))
	if err != nil || !slices.Contains(job.GetStringSlice("targets"), target.Id) {
		return conflict("The target is no longer one of the job's targets.")
	}
	host, err := e.App.FindRecordById(schema.Hosts, job.GetString("host"))
	if err != nil {
		return conflict("The server of the job is gone.")
	}
	switch {
	case !p.Commands.Connected(host.Id):
		return conflict(fmt.Sprintf("The server %s is offline: its agent runs the backup.", host.GetString("name")))
	case p.restoreWriting(job) != nil:
		return conflict("A restore into its data is under way: back it up once it ends.")
	case p.runGoing(job, target):
		return conflict("Its previous run is still going.")
	}
	locked := run.GetString("failure") == backup.FailureLocked
	if locked {
		skew := time.Duration(host.GetFloat("clock_skew_ms")) * time.Millisecond
		if skew > maxLockSkew || skew < -maxLockSkew {
			return conflict(fmt.Sprintf("The clock of %s is off by %d minutes: restic would take a live lock for a stale one. Set its clock first.",
				host.GetString("name"), int(math.Round(math.Abs(skew.Minutes())))))
		}
		lock := runLock(run)
		if state, holder := p.lockHolder(lock); state == holderAlive {
			label, server := holderNames(lock, holder)
			// only a planned retry follows the holder
			then := "try again once it ends"
			if !run.GetDateTime("retry_at").IsZero() {
				then = "PocketCloud retries once it ends"
			}
			return conflict(fmt.Sprintf("The repository is locked by %s on %s, which is still running: %s.", label, server, then))
		}
		if stale := lock.StaleAt(); time.Now().Before(stale) {
			planned, err := p.planRetryAt(run, stale)
			if err != nil {
				return conflict(sentence(err))
			}
			if !planned {
				return conflict("It is being tried again already.")
			}
			return e.JSON(http.StatusAccepted, map[string]any{"retry_at": stale.UTC().Format(time.RFC3339)})
		}
	}
	requestedBy, requester := requester(e)
	// a client that leaves does not leave a retry without its command
	retry, err := p.claimRetry(context.WithoutCancel(e.Request.Context()), run, job, target, host, locked,
		commands.Options{RequestedBy: requestedBy, Requester: requester})
	switch {
	case errors.Is(err, errRetryTaken):
		return conflict("It is being tried again already.")
	case errors.Is(err, errRunNewer):
		return conflict("A newer run of this job to this target exists.")
	case errors.Is(err, errNotFailed):
		return conflict("Only a failed run is tried again.")
	case err != nil:
		return e.BadRequestError("The run could not be tried again: "+saveErrorText(err), nil)
	}
	return e.JSON(http.StatusOK, map[string]any{"run": retry.Id})
}

// planRetryAt plans a failed run's retry for a moment, as for when restic
// counts its lock as stale; false when the run is tried again already, or no
// longer the newest. A job that is disabled, or whose label is gone, gets no
// retry, which the scheduler would drop: the error says so.
func (p *Panel) planRetryAt(run *core.Record, at time.Time) (bool, error) {
	planned := false
	err := p.App.RunInTransaction(func(tx core.App) error {
		fresh, err := tx.FindRecordById(schema.BackupRuns, run.Id)
		if err != nil {
			return err
		}
		if fresh.GetString("status") != schema.RunFailed || retriedIn(tx, run.Id) || newerRunIn(tx, fresh) {
			return nil
		}
		job, err := tx.FindRecordById(schema.BackupJobs, fresh.GetString("job"))
		if err != nil {
			return err
		}
		switch {
		case sourceGone(job):
			return errors.New(goneMessage(job))
		case !job.GetBool("enabled"):
			return fmt.Errorf("the job is disabled, so PocketCloud plans no retry of it: try again once restic counts the lock as stale, at %s UTC", at.UTC().Format("15:04"))
		}
		fresh.Set("retry_at", at.UTC())
		fresh.Set("retry_end", "")
		fresh.Set("retry_note", "Waits until restic counts the lock as stale.")
		planned = true
		return tx.Save(fresh)
	})
	return planned, err
}

// queueRunIn queues a run's command in a transaction, and links the run to
// it: the command is delivered once the transaction commits, with the run
// its enricher checks it against.
func (p *Panel) queueRunIn(ctx context.Context, tx core.App, run *core.Record, payload *agentv1.BackupRun, job, target, host *core.Record, options commands.Options) error {
	options.Host = host.Id
	options.Label = backupLabel(job, target)
	options.Command = &agentv1.Command{Payload: &agentv1.Command_BackupRun{BackupRun: payload}}
	options.QueueTimeout = backupRunQueue
	command, err := p.Commands.CreateIn(ctx, tx, options)
	if err != nil {
		return err
	}
	run.Set("command", command.Id)
	return tx.Save(run)
}
