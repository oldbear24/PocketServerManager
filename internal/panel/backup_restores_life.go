package panel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// A restore someone started (#30) is queued for restoreQueue, so that an
// application never stops long after someone asked, and goes from queued
// to running as its agent starts it, then to succeeded, failed or
// cancelled. The agent stops the containers that use the destination and
// starts them again, also after a failure or a stop; its report says which
// it started and which did not stay up. Someone who stops a restore waits
// for that report: the command stays open until the agent sends it (see
// commands.Spec.SettledByAgent).

// alertBackupRestore is the rule of the alerts of restores, each keyed by
// the server and the data a restore writes.
const alertBackupRestore = "backup_restore"

// previewExpired says a preview is too old to start from.
const previewExpired = "The preview is older than 30 minutes: preview again."

// errRestoreMoved says a restore changed while someone started it.
var errRestoreMoved = errors.New("the restore changed meanwhile")

// startedRoots are the roots a restore to the original location starts
// with: those of its preview that are in the snapshot, each with the
// snapshot path the preview found.
func startedRoots(restore *core.Record, preview restorePreview) []restoreRoot {
	planned := rootsOf(restore)
	started := []restoreRoot{}
	for _, root := range preview.Roots {
		if !root.InSnapshot || root.Index < 0 || root.Index >= len(planned) {
			continue
		}
		next := planned[root.Index]
		if root.Source != "" {
			next.Source = root.Source
		}
		started = append(started, next)
	}
	return started
}

// needsConfirm tells whether a restore overwrites what is there, which
// someone must confirm: it goes back to the original location, or its
// destination holds anything, or the preview would change or delete files.
func needsConfirm(restore *core.Record, preview restorePreview) bool {
	// a database is replaced whole; a new one replaces nothing (#53)
	switch restore.GetString("mode") {
	case schema.RestoreDatabase:
		return true
	case schema.RestoreNewDatabase:
		return false
	}
	if restore.GetString("mode") == schema.RestoreOriginal || preview.Counts.Updated > 0 || preview.Counts.Deleted > 0 {
		return true
	}
	for _, root := range preview.Roots {
		if root.Entries > 0 {
			return true
		}
	}
	return false
}

// handleStartRestore starts a restore from its preview, which is at most 30
// minutes old. A restore that overwrites what its destination holds needs
// confirm_overwrite (acceptance criterion 3); the agent refuses a
// destination that holds files unless the command says overwrite, which is
// set only then. The containers that stop are those the preview showed.
func (p *Panel) handleStartRestore(e *core.RequestEvent) error {
	var body struct {
		ConfirmOverwrite bool `json:"confirm_overwrite"`
		// ConfirmName is the database's name, typed to confirm a restore
		// back into it (#53)
		ConfirmName string `json:"confirm_name"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}
	restore, err := e.App.FindRecordById(schema.BackupRestores, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The restore does not exist.", err)
	}
	mode := restore.GetString("mode")
	if mode == schema.RestoreDownload {
		return e.BadRequestError("A download starts from its link.", nil)
	}
	if !hasRole(e, restoreRole(mode)) {
		return e.ForbiddenError("You are not allowed to do this.", nil)
	}
	switch status := restore.GetString("status"); {
	case status == schema.RestorePreviewing:
		return e.Error(http.StatusConflict, "The preview is not ready yet.", nil)
	case status == schema.RestoreExpired:
		return e.Error(http.StatusConflict, previewExpired, nil)
	case status == schema.RestoreFailed && restoreDraft(restore):
		return e.Error(http.StatusConflict, sentenceText(restore.GetString("error")), nil)
	case status == schema.RestoreQueued, status == schema.RestoreRunning:
		return e.Error(http.StatusConflict, "The restore was started already.", nil)
	case status != schema.RestorePreviewed:
		return e.Error(http.StatusConflict, "The restore has ended: preview it again.", nil)
	}
	if expires := restore.GetDateTime("expires_at"); !expires.IsZero() && time.Now().After(expires.Time()) {
		p.expireDraft(restore)
		return e.Error(http.StatusConflict, previewExpired, nil)
	}
	preview, ok := previewOf(restore)
	if !ok {
		return e.Error(http.StatusConflict, "The preview is not ready yet.", nil)
	}
	roots := startedRoots(restore, preview)
	switch {
	case mode == schema.RestoreOriginal && len(roots) == 0:
		return e.Error(http.StatusConflict, "None of what the job backs up is in this snapshot.", nil)
	case mode == schema.RestorePath && (len(preview.Roots) == 0 || !preview.Roots[0].InSnapshot):
		return e.Error(http.StatusConflict, fmt.Sprintf("%s is not in this snapshot.", restore.GetString("source")), nil)
	}
	job, err := e.App.FindRecordById(schema.BackupJobs, restore.GetString("job"))
	if err != nil {
		return e.NotFoundError("The job does not exist.", err)
	}
	target, err := e.App.FindRecordById(schema.BackupTargets, restore.GetString("target"))
	if err != nil {
		return e.NotFoundError("The target does not exist.", err)
	}
	// a database restore: the typed name, what the preview found, and the
	// plan as it was previewed (#53)
	if databaseMode(mode) {
		if problem := p.databaseStart(restore, job, target, body.ConfirmOverwrite, body.ConfirmName); problem != nil {
			return e.Error(problem.status, problem.message, nil)
		}
	}
	needs := needsConfirm(restore, preview)
	if needs && !body.ConfirmOverwrite {
		return e.Error(http.StatusConflict, "The destination already holds files: confirm that the restore overwrites them.", nil)
	}
	host, err := e.App.FindRecordById(schema.Hosts, restore.GetString("host"))
	if err != nil {
		return e.Error(http.StatusConflict, "The server of the restore is gone.", nil)
	}
	draft := restore.Fresh()
	if mode == schema.RestoreOriginal {
		draft.Set("roots", roots)
	}
	stacks := stoppedProjects(restore)
	if mode == schema.RestoreOriginal || mode == schema.RestoreDatabase {
		stacks = append(stacks, restore.GetString("stack_name"))
	}
	if root, ok := databaseRoot(restore); ok {
		stacks = append(stacks, root.StopProjects...)
	}
	if problem := p.restoreBlocked(job, target, mode, host, restoreDestinations(draft), stacks, restore.Id); problem != nil {
		return e.Error(problem.status, problem.message, nil)
	}
	repository, err := p.repositoryOf(target)
	if err != nil {
		return e.BadRequestError(sentence(err), nil)
	}

	id := core.GenerateDefaultRandomId()
	requestedBy, requester := requester(e)
	// a client that leaves does not leave a restore without its command
	ctx := context.WithoutCancel(e.Request.Context())
	var started *core.Record
	err = e.App.RunInTransaction(func(tx core.App) error {
		fresh, err := tx.FindRecordById(schema.BackupRestores, restore.Id)
		if err != nil {
			return err
		}
		if fresh.GetString("status") != schema.RestorePreviewed || fresh.GetString("preview_at") != restore.GetString("preview_at") {
			return errRestoreMoved
		}
		fresh.Load(map[string]any{"status": schema.RestoreQueued, "overwrite": needs, "confirmed_by": requestedBy,
			"confirmed_label": requester, "command": id, "error": ""})
		if !needs {
			// an empty destination holds nothing to delete, and the agent
			// replaces only what someone confirmed it overwrites: it
			// refuses a destination filled since the preview
			fresh.Set("replace", false)
		}
		if mode == schema.RestoreOriginal {
			fresh.Set("roots", roots)
		}
		// the databases it makes are recorded with its command (#53)
		if root, ok := databaseRoot(fresh); ok {
			if err := p.restoreRecordsIn(audit.WithActor(ctx, audit.ActorFromRequest(e)), tx, fresh, root); err != nil {
				return err
			}
		}
		payload := restorePayload(fresh, false)
		payload.Repository = repository
		_, err = p.Commands.CreateIn(ctx, tx, commands.Options{
			ID: id, Host: host.Id, Label: cutText("Restore "+fresh.GetString("label"), 255),
			Command:     &agentv1.Command{Payload: &agentv1.Command_BackupRestore{BackupRestore: payload}},
			RequestedBy: requestedBy, Requester: requester, QueueTimeout: restoreQueue,
		})
		if err != nil {
			return err
		}
		started = fresh
		return tx.SaveWithContext(ctx, fresh)
	})
	if errors.Is(err, errRestoreMoved) {
		return e.Error(http.StatusConflict, "The restore changed meanwhile: look at it again.", nil)
	}
	if databaseMode(mode) && uniqueViolation(err) {
		return e.Error(http.StatusConflict, fmt.Sprintf("A database named %s exists on its server already.", restore.GetString("database_name")), nil)
	}
	if err != nil {
		return e.BadRequestError("The restore could not be started: "+saveErrorText(err), nil)
	}
	dests := []string{}
	for _, dest := range restoreDestinations(started) {
		dests = append(dests, dest.String())
	}
	data := map[string]any{
		"restore": started.Id, "job": job.Id, "target": target.Id, "snapshot": started.GetString("snapshot_id"), "mode": mode,
		"host": host.Id, "destination": dests, "replace": started.GetBool("replace"), "overwrite": needs, "includes": includeOf(started),
	}
	// a database restore names the database and its server (#53)
	if root, ok := databaseRoot(started); ok {
		data["database"] = started.GetString("database_name")
		if server, err := p.serverByContainer(host.Id, root.Container); err == nil {
			data["server"] = server.GetString("name")
		}
	}
	p.Audit.Security(audit.ActionBackupRestore, audit.ActorFromRequest(e), started.GetString("label"), data)
	return e.JSON(http.StatusOK, started)
}

// expireDraft marks a draft whose preview is too old as expired.
func (p *Panel) expireDraft(restore *core.Record) {
	err := p.update(context.Background(), restore, func(fresh *core.Record) error {
		if fresh.GetString("status") != schema.RestorePreviewed {
			return errUnchanged
		}
		fresh.Set("status", schema.RestoreExpired)
		return nil
	})
	if err != nil {
		p.Log.Error("failed to expire a restore's draft", "restore", restore.Id, "error", err)
	}
}

// handleCancelRestore stops a restore or a download. One that waits in the
// queue is cancelled at once, having stopped or changed nothing; one under
// way stops once its agent says so, as the agent starts the application
// again first (202 {stopping}).
func (p *Panel) handleCancelRestore(e *core.RequestEvent) error {
	restore, err := e.App.FindRecordById(schema.BackupRestores, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The restore does not exist.", err)
	}
	switch restore.GetString("status") {
	case schema.RestorePreviewing, schema.RestorePreviewed, schema.RestoreExpired:
		return e.BadRequestError("This is a draft: discard it instead.", nil)
	case schema.RestoreQueued, schema.RestoreRunning:
	default:
		return e.BadRequestError("The restore has ended.", nil)
	}
	_, who := requester(e)
	command := restore.GetString("command")
	if command == "" {
		// a download whose link nobody used
		p.settleRestore(restore, schema.RestoreCancelled, "Cancelled before it started: nothing was stopped or changed.")
		return p.answerRestore(e, restore.Id, http.StatusOK)
	}
	if restore.GetString("stop_requested_label") == "" {
		p.setFields(restore, map[string]any{"stop_requested_label": who})
	}
	err = p.Commands.Cancel(command, "Cancelled by "+who)
	switch {
	case errors.Is(err, commands.ErrCancelRequested):
		return e.JSON(http.StatusAccepted, map[string]any{"stopping": true})
	case err == nil, errors.Is(err, commands.ErrNotCancellable):
		// it was settled at once, or had ended
		return p.answerRestore(e, restore.Id, http.StatusOK)
	default:
		return e.InternalServerError("The restore could not be stopped.", err)
	}
}

// answerRestore answers with a restore as it is stored now.
func (p *Panel) answerRestore(e *core.RequestEvent, id string, status int) error {
	restore, err := e.App.FindRecordById(schema.BackupRestores, id)
	if err != nil {
		return e.NotFoundError("The restore does not exist.", err)
	}
	return e.JSON(status, restore)
}

// settleRestore ends a restore that its command can no longer end.
func (p *Panel) settleRestore(restore *core.Record, status, reason string) {
	err := p.update(context.Background(), restore, func(fresh *core.Record) error {
		if s := fresh.GetString("status"); s != schema.RestoreQueued && s != schema.RestoreRunning && s != schema.RestorePreviewing {
			return errUnchanged
		}
		fresh.Set("status", status)
		fresh.Set("error", cutText(reason, 4096))
		fresh.Set("finished_at", types.NowDateTime())
		return nil
	})
	if err != nil {
		p.Log.Error("failed to settle a restore", "restore", restore.Id, "error", err)
	}
}

// restoreStarted marks a restore or a download as running once its agent
// started it.
func (p *Panel) restoreStarted(command *core.Record) {
	payload := restoreCommandPayloadOf(command)
	if payload.DryRun {
		return
	}
	restore, err := p.App.FindRecordById(schema.BackupRestores, payload.RestoreID)
	if err != nil || restore.GetString("command") != command.Id {
		return
	}
	err = p.update(context.Background(), restore, func(fresh *core.Record) error {
		if fresh.GetString("status") != schema.RestoreQueued {
			return errUnchanged
		}
		fresh.Set("status", schema.RestoreRunning)
		fresh.Set("started_at", command.GetDateTime("started_at"))
		return nil
	})
	if err != nil {
		p.Log.Error("failed to mark a restore as running", "restore", restore.Id, "error", err)
	}
}

// restoreResult is what an agent's restore answers.
type restoreResult struct {
	FilesRestored int64            `json:"files_restored"`
	BytesRestored int64            `json:"bytes_restored"`
	FilesDeleted  int64            `json:"files_deleted"`
	Errors        int64            `json:"errors"`
	AppStopped    []map[string]any `json:"app_stopped"`
	AppStarted    []string         `json:"app_started"`
	AppNotRunning []string         `json:"app_not_running"`
	StartErrors   []string         `json:"start_errors"`
	Partial       bool             `json:"partial"`
	Log           string           `json:"log"`
}

// realRestoreFinished keeps how a restore ended, once, with the target's
// secrets hidden, and says so in the events and alerts.
func (p *Panel) realRestoreFinished(restore, command *core.Record) {
	if restore.GetString("command") != command.Id {
		return
	}
	if s := restore.GetString("status"); s != schema.RestoreQueued && s != schema.RestoreRunning {
		return
	}
	var result restoreResult
	hasResult := command.GetString("result") != "" && command.GetString("result") != "null"
	raw, _ := json.Marshal(command.Get("result"))
	_ = json.Unmarshal(raw, &result)
	hide := []string{}
	if target, err := p.App.FindRecordById(schema.BackupTargets, restore.GetString("target")); err == nil {
		hide = p.targetSecretValues(target)
	}
	// and a database restore's passwords (#53)
	hide = append(hide, p.databaseSecretValues(restore)...)
	status, message := p.restoreOutcome(restore, command, result, hasResult)
	message = backup.Redact(message, hide...)
	problem := ""
	switch {
	case len(result.AppNotRunning) > 0:
		problem = strings.Join(result.AppNotRunning, ", ") + " did not stay up after the restore."
	case len(result.StartErrors) > 0:
		problem = "The application could not be started again: " + strings.Join(result.StartErrors, "; ")
	}
	problem = cutText(backup.Redact(problem, hide...), 1000)
	stopped := make([]any, 0, len(result.AppStopped))
	for _, container := range result.AppStopped {
		stopped = append(stopped, container)
	}
	finished := command.GetDateTime("finished_at")
	if finished.IsZero() {
		finished = types.NowDateTime()
	}
	fields := map[string]any{
		"status": status, "error": cutText(message, 4096), "finished_at": finished,
		"files_restored": result.FilesRestored, "bytes_restored": result.BytesRestored, "files_deleted": result.FilesDeleted,
		"errors": result.Errors, "partial": result.Partial, "app_stopped": stopped, "app_problem": problem,
		"log": cutTail(backup.Redact(result.Log, hide...), maxRunLog),
	}
	if started := command.GetDateTime("started_at"); !started.IsZero() {
		fields["started_at"] = started
	}
	if databaseMode(restore.GetString("mode")) {
		var restored backup.DbRestoreResult
		_ = json.Unmarshal(raw, &restored)
		fields["tables"] = restored.Tables
		fields["previous"] = restored.Previous
	}
	// once: the restore's command may be settled from two places
	kept := false
	err := p.update(context.Background(), restore, func(fresh *core.Record) error {
		if s := fresh.GetString("status"); s != schema.RestoreQueued && s != schema.RestoreRunning {
			return errUnchanged
		}
		for name, value := range fields {
			fresh.Set(name, value)
		}
		kept = true
		return nil
	})
	if err != nil {
		p.Log.Error("failed to keep a restore", "restore", restore.Id, "error", err)
	}
	if kept {
		// what a database restore made or left (#53)
		p.databaseRestored(restore, command)
		p.noteRestore(restore, result)
	}
}

// restoreOutcome is how a restore ended, and what it says: as its command
// ended, but for one that waited its whole time and never started, which
// failed having changed nothing.
func (p *Panel) restoreOutcome(restore, command *core.Record, result restoreResult, hasResult bool) (string, string) {
	started := command.GetDateTime("started_at")
	finished := command.GetDateTime("finished_at")
	if finished.IsZero() {
		finished = types.NowDateTime()
	}
	reason := strings.TrimPrefix(command.GetString("error"), "failed to prepare the command: ")
	switch command.GetString("status") {
	case schema.CommandSucceeded:
		return schema.RestoreSucceeded, ""
	case schema.CommandTimedOut:
		if started.IsZero() && command.GetDateTime("dispatched_at").IsZero() {
			return schema.RestoreFailed, p.restoreNeverStarted(command)
		}
		if reason == "" {
			reason = "The restore did not finish in time."
		}
		return schema.RestoreFailed, sentenceText(reason)
	case schema.CommandCancelled:
		if started.IsZero() && command.GetDateTime("cancel_requested_at").IsZero() && cancelledByItsTime(command, finished) {
			return schema.RestoreFailed, p.restoreNeverStarted(command)
		}
		if !hasResult && started.IsZero() {
			return schema.RestoreCancelled, "Cancelled before it started: nothing was stopped or changed."
		}
		who := restore.GetString("stop_requested_label")
		if who == "" {
			who = strings.TrimPrefix(command.GetString("cancel_reason"), "Cancelled by ")
		}
		if who == "" {
			who = strings.TrimPrefix(command.GetString("error"), "Cancelled by ")
		}
		if !result.Partial && result.FilesRestored == 0 && result.FilesDeleted == 0 && len(result.AppStopped) == 0 {
			return schema.RestoreCancelled, fmt.Sprintf("Stopped by %s before it changed anything.", strings.TrimSuffix(who, "."))
		}
		message := fmt.Sprintf("Stopped by %s before it was complete: the destination holds part of the snapshot.", strings.TrimSuffix(who, "."))
		if len(result.AppStarted) > 0 {
			message += " The application was started again."
		}
		return schema.RestoreCancelled, message
	}
	if reason == "" {
		reason = "The restore failed."
	}
	return schema.RestoreFailed, sentenceText(reason)
}

// restoreNeverStarted says why a restore whose command waited its whole
// time never started.
func (p *Panel) restoreNeverStarted(command *core.Record) string {
	why := "was busy"
	if !p.Commands.Connected(command.GetString("host")) || p.markedOffline(command.GetString("host")) {
		why = "stayed offline"
	}
	return fmt.Sprintf("It waited %d minutes while the server %s %s, and never started: nothing was stopped or changed.",
		int(restoreQueue.Minutes()), command.GetString("host_name"), why)
}

// restoreDestinationKey names the data a restore writes, as its ordering
// key does: a database by its record (#53), the stack of the original
// location, or the volume or folder.
func restoreDestinationKey(restore *core.Record) string {
	if root, ok := databaseRoot(restore); ok {
		if id := restore.GetString("database"); id != "" {
			return "database:" + id
		}
		return "database:" + databaseDestination(root)
	}
	if restore.GetString("mode") == schema.RestoreOriginal && restore.GetString("stack_name") != "" {
		return "stack:" + restore.GetString("stack_name")
	}
	if dests := restoreDestinations(restore); len(dests) > 0 {
		if dests[0].volume != "" {
			return "volume:" + dests[0].volume
		}
		return "path:" + dests[0].path
	}
	return ""
}

// restoreAlertKey is the target of a restore's alerts: its server and the
// data it writes. The data's name is hashed when the key would not fit.
func restoreAlertKey(restore *core.Record) string {
	key := restore.GetString("host") + ":" + restoreDestinationKey(restore)
	if len(key) <= 255 {
		return key
	}
	sum := sha256.Sum256([]byte(restoreDestinationKey(restore)))
	return restore.GetString("host") + ":sha256:" + hex.EncodeToString(sum[:])
}

// restoreWhere says in words what a restore writes.
func (p *Panel) restoreWhere(restore *core.Record) string {
	if root, ok := databaseRoot(restore); ok {
		return p.databaseRestoreWhere(restore, root)
	}
	dests := []string{}
	for _, dest := range restoreDestinations(restore) {
		dests = append(dests, dest.String())
	}
	where := strings.Join(dests, ", ")
	if restore.GetString("mode") == schema.RestorePath {
		if host, err := p.App.FindRecordById(schema.Hosts, restore.GetString("host")); err == nil {
			where = host.GetString("name") + ":" + where
		}
	}
	return where
}

// noteRestore says how a restore ended in the events, and raises its alert:
// a failure, or an application that did not stay up after it, which the
// next restore of the same data that goes well resolves.
func (p *Panel) noteRestore(before *core.Record, result restoreResult) {
	restore, err := p.App.FindRecordById(schema.BackupRestores, before.Id)
	if err != nil {
		return
	}
	jobName := ""
	if job, err := p.App.FindRecordById(schema.BackupJobs, restore.GetString("job")); err == nil {
		jobName = job.GetString("name")
	}
	host := restore.GetString("host")
	label := restore.GetString("label")
	data := map[string]any{"restore": restore.Id, "job": restore.GetString("job"), "target": restore.GetString("target"), "command": restore.GetString("command")}
	key := restoreAlertKey(restore)
	message := restore.GetString("error")
	status := restore.GetString("status")
	problem := restore.GetString("app_problem")
	var failed, down string
	if status == schema.RestoreFailed {
		failed = fmt.Sprintf("The restore of %s into %s failed: %s", jobName, p.restoreWhere(restore), sentenceText(message))
		if len(result.AppStarted) > 0 {
			failed += " The application was started again."
		}
		failed = cutText(failed, 2048)
	}
	if problem != "" {
		down = cutText(fmt.Sprintf("After the restore of %s into %s: %s", jobName, p.restoreWhere(restore), problem), 2048)
	}

	// the alert first, then the events: whoever sees a restore's event sees
	// its alert as it is now
	switch {
	case status == schema.RestoreFailed:
		p.raiseRestoreAlert(host, key, schema.SeverityCritical, failed)
	case problem != "":
		p.raiseRestoreAlert(host, key, schema.SeverityWarning, down)
	case status == schema.RestoreSucceeded:
		if err := events.ResolveAlert(p.App, alertBackupRestore, key); err != nil {
			p.Log.Error("failed to resolve an alert", "rule", alertBackupRestore, "error", err)
		}
	}

	switch status {
	case schema.RestoreSucceeded:
		text := cutText(fmt.Sprintf("%s restored %d files, %d bytes.", label, result.FilesRestored, result.BytesRestored), 2048)
		if databaseMode(restore.GetString("mode")) {
			text = cutText(fmt.Sprintf("%s restored %d tables, %d bytes.", label, restore.GetInt("tables"), result.BytesRestored), 2048)
		}
		events.Log(p.App, host, "backup.restored", schema.SeverityInfo, text, data)
	case schema.RestoreFailed:
		events.Log(p.App, host, "backup.restore_failed", schema.SeverityCritical, failed, data)
	case schema.RestoreCancelled:
		events.Log(p.App, host, "backup.restore_stopped", schema.SeverityWarning, cutText(label+": "+message, 2048), data)
	}
	if problem != "" {
		events.Log(p.App, host, "backup.restore_app_down", schema.SeverityWarning, down, data)
	}
}

// raiseRestoreAlert raises the alert of a restore's data on a server that
// is still there.
func (p *Panel) raiseRestoreAlert(host, key, severity, message string) {
	if _, err := p.App.FindRecordById(schema.Hosts, host); err != nil {
		return
	}
	if err := events.RaiseAlert(p.App, alertBackupRestore, key, host, severity, message); err != nil {
		p.Log.Error("failed to raise an alert", "rule", alertBackupRestore, "error", err)
	}
}

// restoreRemoved resolves the alert of a restore's data once no restore of
// that data is left that failed or left an application down: as a job or a
// target goes, with their restores, nothing would resolve it any more.
func (p *Panel) restoreRemoved(app core.App, removed *core.Record) {
	key := restoreAlertKey(removed)
	others, err := app.FindRecordsByFilter(schema.BackupRestores, "host={:host} && id!={:id} && (status={:failed} || app_problem!='')", "", 0, 0,
		dbx.Params{"host": removed.GetString("host"), "id": removed.Id, "failed": schema.RestoreFailed})
	if err != nil {
		p.Log.Error("failed to read the restores", "error", err)
		return
	}
	for _, other := range others {
		if restoreAlertKey(other) == key {
			return
		}
	}
	if err := events.ResolveAlert(app, alertBackupRestore, key); err != nil {
		p.Log.Error("failed to resolve an alert", "rule", alertBackupRestore, "error", err)
	}
}
