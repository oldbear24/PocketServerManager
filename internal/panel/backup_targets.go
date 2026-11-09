package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// Backup targets (#28) are where backups go: an S3-compatible bucket, a
// directory on an SFTP server, a restic rest-server, or a directory of a
// server. The panel keeps their settings, their credentials and each
// repository's password, sealed; the agent of the target's server runs
// restic. A new target is kept only once that agent opened its repository,
// or created it: one that cannot be reached is refused with restic's words.
//
// The secrets leave the panel in one place only: the backup command for the
// agent that runs it, completed as it is delivered. The stored command names
// the target, which viewers may read; a target's secrets never reach the API.
//
// A check only opens a repository, which is quick. What a repository holds
// is measured afterwards, as restic reads every snapshot for it: after a
// target is added or checked again, and hourly, without anyone waiting.
// Every check's result is kept by the check's finisher, which runs before the
// target's next command is delivered: a change of credentials that a check
// took is stored by then, so no command goes out with the old ones.

const (
	// backupInitWait is how long adding or checking a target waits for the
	// agent: restic may retry a storage that does not answer for a while.
	backupInitWait   = 2 * time.Minute
	maxImmutableDays = 36500
	// maxPlace is the longest place a target's record holds.
	maxPlace = 1000
)

// backupCommandTypes are the commands that carry a target's repository.
var backupCommandTypes = []string{
	"backup.init", "backup.run", "backup.restore", "backup.snapshots",
	"backup.browse", "backup.check", "backup.unlock", "backup.forget",
}

// targetSecrets is what restic needs of a target.
type targetSecrets struct {
	kind     string
	config   backup.Config
	creds    backup.Credentials
	password string
}

// targetChange is a change of a stored target that waits for its check: new
// credentials or settings, or another server. The check's finisher keeps it,
// with the check's result.
type targetChange struct {
	host    string
	secrets targetSecrets
	actor   audit.Actor
}

// checkOutcome is how a check went, as the finisher tells the route waiting
// for it: no problem, or what to answer.
type checkOutcome struct {
	problem *targetProblem
	created bool
}

var (
	// targetChanges holds the changes that wait for their checks, by the id
	// of the backup.init.
	targetChanges sync.Map
	// targetWaiters holds the routes waiting for a check, by its id.
	targetWaiters sync.Map
	// targetEnriched holds what a backup.init was completed with from a
	// stored target, by its id: a result found with credentials or a server
	// that have changed since is no news of the target as it is.
	targetEnriched sync.Map
)

// targetProblem is an answer of the target routes other than success.
type targetProblem struct {
	status  int
	message string
}

func (t *targetProblem) Error() string { return t.message }

// targetCheck is what backup.init found.
type targetCheck struct {
	Created bool   `json:"created"`
	ID      string `json:"id"`
	// Used is what the repository holds, when it was measured.
	Used *int64 `json:"used_bytes"`
	// Free is the room a local target's file system has left.
	Free *int64 `json:"free_bytes"`
}

func (p *Panel) registerBackups() {
	for _, commandType := range backupCommandTypes {
		p.Commands.RegisterEnricher(commandType, p.enrichBackup)
	}
	p.Commands.OnFinish("backup.init", p.targetChecked)
	p.App.OnRecordDeleteRequest(schema.Hosts).BindFunc(p.keepTargetsServer)
	// what each target holds, and whether it still answers
	p.App.Cron().MustAdd("pocketcloud_backup_targets", "23 * * * *", p.CheckBackupTargets)
}

func (p *Panel) bindBackupRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	admins := requireRole(schema.RoleAdmin)
	group.POST("/backup-targets", p.handleAddTarget).BindFunc(admins)
	group.PATCH("/backup-targets/{id}", p.handleChangeTarget).BindFunc(admins)
	group.POST("/backup-targets/{id}/refresh", p.handleRefreshTarget).BindFunc(admins)
	group.DELETE("/backup-targets/{id}", p.handleRemoveTarget).BindFunc(admins)
}

// targetBody is a target as the routes take it. Pointers are what a change
// may leave out.
type targetBody struct {
	Name          *string             `json:"name"`
	Kind          string              `json:"kind"`
	Host          *string             `json:"host"`
	Config        *backup.Config      `json:"config"`
	Credentials   *backup.Credentials `json:"credentials"`
	ImmutableDays *int                `json:"immutable_days"`
	Note          *string             `json:"note"`
	// Password is that of an existing repository, to add it, or one the admin
	// chose to keep a copy of; without it, the panel makes one.
	Password string `json:"password"`
}

// handleAddTarget adds a target. It is kept, waiting, with its password
// before restic runs: a check that does not end in time leaves a target to
// check again, never a repository whose password is lost.
func (p *Panel) handleAddTarget(e *core.RequestEvent) error {
	var body targetBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}
	name, problem := p.targetName(body.Name, "")
	if problem != "" {
		return e.BadRequestError(problem, nil)
	}
	config, creds := backup.Config{}, backup.Credentials{}
	if body.Config != nil {
		config = *body.Config
	}
	if body.Credentials != nil {
		creds = *body.Credentials
	}
	config, creds, err := backup.Check(body.Kind, config, creds)
	if err != nil {
		return e.BadRequestError(capitalize(err.Error())+".", nil)
	}
	days, note, problem := targetNotes(body.ImmutableDays, body.Note)
	if problem != "" {
		return e.BadRequestError(problem, nil)
	}
	hostID := ""
	if body.Host != nil {
		hostID = *body.Host
	}
	host, err := e.App.FindRecordById(schema.Hosts, hostID)
	if err != nil {
		return e.BadRequestError("Choose the server whose agent reaches the target.", nil)
	}
	location := backup.Where(body.Kind, config)
	if utf8.RuneCountInString(location) > maxPlace {
		return e.BadRequestError(fmt.Sprintf("The target's place is at most %d characters.", maxPlace), nil)
	}
	if problem := p.targetPlace(body.Kind, location, host.Id, ""); problem != "" {
		return e.BadRequestError(problem, nil)
	}
	password := body.Password
	if password == "" {
		if password, err = backup.NewPassword(); err != nil {
			return e.InternalServerError("", err)
		}
	} else if utf8.RuneCountInString(password) > 1000 || strings.ContainsAny(password, "\n\r\x00") {
		return e.BadRequestError("The repository's password is one line of at most 1000 characters.", nil)
	}
	if !p.Commands.Connected(host.Id) {
		return e.Error(http.StatusConflict, offlineTarget(host), nil)
	}

	ctx := audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e))
	collection, err := e.App.FindCachedCollectionByNameOrId(schema.BackupTargets)
	if err != nil {
		return e.InternalServerError("", err)
	}
	target := core.NewRecord(collection)
	target.Set("name", name)
	target.Set("kind", body.Kind)
	target.Set("host", host.Id)
	target.Set("config", config)
	target.Set("location", location)
	target.Set("immutable_days", days)
	target.Set("note", note)
	target.Set("status", schema.TargetPending)
	if err := p.sealTarget(target, creds, password); err != nil {
		return e.InternalServerError("", err)
	}
	if err := e.App.SaveWithContext(ctx, target); err != nil {
		return e.BadRequestError("The target could not be saved: "+err.Error(), nil)
	}

	requestedBy, requester := requester(e)
	// a client that leaves does not end the check: its target stays as it went
	outcome := p.checkTarget(context.WithoutCancel(e.Request.Context()), target, host, nil, "", requestedBy, requester)
	switch {
	case outcome.problem == nil:
	case outcome.problem.status == http.StatusBadRequest || outcome.problem.status == http.StatusConflict:
		// restic refused it, or nothing ran: the target goes again
		if err := e.App.DeleteWithContext(ctx, target); err != nil {
			p.Log.Error("failed to remove a refused backup target", "target", name, "error", err)
		}
		return e.Error(outcome.problem.status, outcome.problem.message, nil)
	default:
		message := outcome.problem.message + " The target is kept, waiting: check it again to finish adding it, or remove it."
		p.setFields(target, map[string]any{"last_error": message})
		return e.Error(outcome.problem.status, message, nil)
	}
	if !outcome.created {
		p.measureTarget(target, requestedBy, requester)
	}
	fresh, err := e.App.FindRecordById(schema.BackupTargets, target.Id)
	if err != nil {
		return e.InternalServerError("", err)
	}
	return e.JSON(http.StatusCreated, fresh)
}

// handleChangeTarget changes a target. New credentials, settings or another
// server are checked with restic first and kept only if the repository opens
// with them; the kind and the place never change.
func (p *Panel) handleChangeTarget(e *core.RequestEvent) error {
	target, err := e.App.FindRecordById(schema.BackupTargets, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The target does not exist.", err)
	}
	var body targetBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}
	kind := target.GetString("kind")
	if body.Kind != "" && body.Kind != kind {
		return e.BadRequestError("A target keeps its kind: add a new target for another kind of storage.", nil)
	}
	fields := map[string]any{}
	if body.Name != nil {
		name, problem := p.targetName(body.Name, target.Id)
		if problem != "" {
			return e.BadRequestError(problem, nil)
		}
		fields["name"] = name
	}
	if body.ImmutableDays != nil || body.Note != nil {
		days, note, problem := targetNotes(body.ImmutableDays, body.Note)
		if problem != "" {
			return e.BadRequestError(problem, nil)
		}
		if body.ImmutableDays != nil {
			fields["immutable_days"] = days
		}
		if body.Note != nil {
			fields["note"] = note
		}
	}

	actor := audit.ActorFromRequest(e)
	if body.Config != nil || body.Credentials != nil || (body.Host != nil && *body.Host != target.GetString("host")) {
		stored, err := p.targetSecretsOf(target)
		if err != nil {
			p.secretsMissing(target, err)
			return e.BadRequestError(secretsMissingMessage, nil)
		}
		config, creds := stored.config, stored.creds
		if body.Config != nil {
			config = *body.Config
		}
		if body.Credentials != nil {
			creds = *body.Credentials
		}
		config, creds, err = backup.Check(kind, config, creds)
		if err != nil {
			return e.BadRequestError(capitalize(err.Error())+".", nil)
		}
		if backup.Where(kind, config) != target.GetString("location") {
			return e.BadRequestError("A target stays where it is: add a new target for another place.", nil)
		}
		hostID := target.GetString("host")
		if body.Host != nil && *body.Host != hostID {
			if kind == backup.KindLocal {
				return e.BadRequestError("A local target is a directory of its server: add a new target on another server.", nil)
			}
			hostID = *body.Host
		}
		host, err := e.App.FindRecordById(schema.Hosts, hostID)
		if err != nil {
			return e.BadRequestError("Choose the server whose agent reaches the target.", nil)
		}
		requestedBy, requester := requester(e)
		change := &targetChange{host: host.Id, secrets: targetSecrets{kind: kind, config: config, creds: creds, password: stored.password}, actor: actor}
		// the finisher keeps the change once the repository opened with it
		outcome := p.checkTarget(context.WithoutCancel(e.Request.Context()), target, host, change, target.GetString("repo_id"), requestedBy, requester)
		if outcome.problem != nil {
			return e.Error(outcome.problem.status, outcome.problem.message, nil)
		}
	}

	if len(fields) > 0 {
		// onto the target as it is now, which the check changed meanwhile
		err := p.update(audit.WithActor(e.Request.Context(), actor), target, func(fresh *core.Record) error {
			for name, value := range fields {
				fresh.Set(name, value)
			}
			return nil
		})
		if err != nil {
			return e.BadRequestError("The target could not be saved: "+err.Error(), nil)
		}
	}
	fresh, err := e.App.FindRecordById(schema.BackupTargets, target.Id)
	if err != nil {
		return e.NotFoundError("The target does not exist.", err)
	}
	return e.JSON(http.StatusOK, fresh)
}

// handleRefreshTarget checks a target again: whether its repository still
// opens, and then, without waiting, what it holds. A check that fails is the
// target's state; one of a target still waiting to be added finishes it.
func (p *Panel) handleRefreshTarget(e *core.RequestEvent) error {
	target, err := e.App.FindRecordById(schema.BackupTargets, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The target does not exist.", err)
	}
	host, err := e.App.FindRecordById(schema.Hosts, target.GetString("host"))
	if err != nil {
		return e.Error(http.StatusConflict, "The server of the target is gone.", nil)
	}
	requestedBy, requester := requester(e)
	outcome := p.checkTarget(e.Request.Context(), target, host, nil, target.GetString("repo_id"), requestedBy, requester)
	switch {
	case outcome.problem == nil:
		if !outcome.created {
			p.measureTarget(target, requestedBy, requester)
		}
	case outcome.problem.status == http.StatusBadRequest:
		// the target's state now, which the answer shows
	default:
		return e.Error(outcome.problem.status, outcome.problem.message, nil)
	}
	fresh, err := e.App.FindRecordById(schema.BackupTargets, target.Id)
	if err != nil {
		return e.NotFoundError("The target does not exist.", err)
	}
	return e.JSON(http.StatusOK, fresh)
}

// handleRemoveTarget forgets a target. Its repository stays where it is, but
// PocketCloud forgets its password with it.
func (p *Panel) handleRemoveTarget(e *core.RequestEvent) error {
	target, err := e.App.FindRecordById(schema.BackupTargets, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The target does not exist.", err)
	}
	// backup jobs (#29) will be counted here: a target they use stays
	if err := e.App.DeleteWithContext(audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e)), target); err != nil {
		return e.BadRequestError("The target could not be removed: "+err.Error(), nil)
	}
	return e.NoContent(http.StatusNoContent)
}

// keepTargetsServer refuses to remove a server whose agent reaches backup
// targets: they would lose the only way to their repositories. They move to
// another server first, or are removed.
func (p *Panel) keepTargetsServer(e *core.RecordRequestEvent) error {
	targets, err := e.App.FindRecordsByFilter(schema.BackupTargets, "host={:host}", "name", 0, 0, dbx.Params{"host": e.Record.Id})
	if err != nil || len(targets) == 0 {
		return e.Next()
	}
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.GetString("name"))
	}
	// the API begins every answer with a capital: a server's name does not
	return e.BadRequestError(fmt.Sprintf("The server %s reaches the backup targets %s: move them to another server first, or remove them, which forgets their repositories' passwords.",
		e.Record.GetString("name"), strings.Join(names, ", ")), nil)
}

// CheckBackupTargets measures every target whose server is connected, each on
// its own, hourly: whether it still answers, and what it holds. Nothing waits
// for the checks; their finisher keeps what they found.
func (p *Panel) CheckBackupTargets() {
	targets, err := p.App.FindAllRecords(schema.BackupTargets)
	if err != nil {
		p.Log.Error("failed to read the backup targets", "error", err)
		return
	}
	for _, target := range targets {
		p.measureTarget(target, "", "PocketCloud")
	}
}

// measureTarget asks the agent of a target's server what the target holds,
// without waiting: a check of its own, which the finisher keeps. A target
// whose server is offline, or that is measured already, is passed by.
func (p *Panel) measureTarget(target *core.Record, requestedBy, requester string) {
	hostID := target.GetString("host")
	if !p.Commands.Connected(hostID) {
		return
	}
	expect := target.GetString("repo_id")
	measure := &agentv1.BackupInit{RepoId: expect, Usage: true, Repository: &agentv1.BackupRepository{TargetId: target.Id}}
	under, err := p.App.FindRecordsByFilter(schema.Commands,
		"type='backup.init' && resource={:resource} && (status={:queued} || status={:dispatched} || status={:running})", "", 1, 0,
		dbx.Params{"resource": commands.Specs["backup.init"].Resource(&agentv1.Command{Payload: &agentv1.Command_BackupInit{BackupInit: measure}}),
			"queued": schema.CommandQueued, "dispatched": schema.CommandDispatched, "running": schema.CommandRunning})
	if err != nil || len(under) > 0 {
		return
	}
	secrets, err := p.targetSecretsOf(target)
	if err != nil {
		p.secretsMissing(target, err)
		return
	}
	repo, err := backup.Locate(secrets.kind, secrets.config, secrets.creds)
	if err != nil {
		p.Log.Warn("a backup target cannot be located", "target", target.GetString("name"), "error", err)
		return
	}
	measure.Repository.Url = repo.URL
	if _, err := p.Commands.Create(context.Background(), commands.Options{
		Host:        hostID,
		Command:     &agentv1.Command{Payload: &agentv1.Command_BackupInit{BackupInit: measure}},
		Label:       truncateText("Measure the backup target "+target.GetString("name"), 255),
		RequestedBy: requestedBy,
		Requester:   requester,
	}); err != nil {
		p.Log.Warn("failed to queue the measure of a backup target", "target", target.GetString("name"), "error", err)
	}
}

// checkTarget has the agent of host open the target's repository and waits
// for the finisher to say how it went: with the stored secrets, or with
// those of a change, which the finisher keeps if the repository opened.
// expect is the id of the repository the target has had, which is only
// opened; without it, a missing repository is created. A problem says what
// to answer: 400 with restic's words, 409 offline, 504 no answer in time.
func (p *Panel) checkTarget(ctx context.Context, target, host *core.Record, change *targetChange, expect, requestedBy, requester string) checkOutcome {
	if !p.Commands.Connected(host.Id) {
		return checkOutcome{problem: &targetProblem{http.StatusConflict, offlineTarget(host)}}
	}
	secrets := targetSecrets{}
	if change != nil {
		secrets = change.secrets
	} else {
		var err error
		if secrets, err = p.targetSecretsOf(target); err != nil {
			p.secretsMissing(target, err)
			return checkOutcome{problem: &targetProblem{http.StatusBadRequest, secretsMissingMessage}}
		}
	}
	repo, err := backup.Locate(secrets.kind, secrets.config, secrets.creds)
	if err != nil {
		return checkOutcome{problem: &targetProblem{http.StatusBadRequest, capitalize(err.Error()) + "."}}
	}

	id := core.GenerateDefaultRandomId()
	waiter := make(chan checkOutcome, 1)
	targetWaiters.Store(id, waiter)
	defer targetWaiters.Delete(id)
	// there before the command can be delivered; the finisher takes it
	if change != nil {
		targetChanges.Store(id, *change)
	}
	_, err = p.Commands.Create(ctx, commands.Options{
		ID:   id,
		Host: host.Id,
		// the place, never a secret: the enricher adds those
		Command: &agentv1.Command{Payload: &agentv1.Command_BackupInit{BackupInit: &agentv1.BackupInit{
			Repository: &agentv1.BackupRepository{TargetId: target.Id, Url: repo.URL},
			RepoId:     expect,
		}}},
		Label:       truncateText("Check the backup target "+target.GetString("name"), 255),
		RequestedBy: requestedBy,
		Requester:   requester,
	})
	if err != nil {
		targetChanges.Delete(id)
		return checkOutcome{problem: &targetProblem{http.StatusBadRequest, capitalize(err.Error()) + "."}}
	}
	wait := p.BackupCheckWait
	if wait <= 0 {
		wait = backupInitWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case outcome := <-waiter:
		return outcome
	case <-timer.C:
	case <-ctx.Done():
	}
	// a check that ends later would surprise whoever waits no more
	targetWaiters.Delete(id)
	_ = p.Commands.Cancel(id, "The panel stopped waiting for it.")
	select {
	case outcome := <-waiter:
		// it ended in between
		return outcome
	default:
	}
	return checkOutcome{problem: &targetProblem{http.StatusGatewayTimeout, fmt.Sprintf("The agent of %s did not answer in time.", host.GetString("name"))}}
}

// targetChecked keeps what a backup.init found, and tells the route that
// waits for it, if one does. It runs as the check ends, before the target's
// next command is delivered.
func (p *Panel) targetChecked(command *core.Record) {
	var payload struct {
		Repository struct {
			TargetID string `json:"target_id"`
		} `json:"repository"`
		Usage bool `json:"usage"`
	}
	raw, _ := json.Marshal(command.Get("payload"))
	_ = json.Unmarshal(raw, &payload)
	var change *targetChange
	if pending, ok := targetChanges.LoadAndDelete(command.Id); ok {
		c, _ := pending.(targetChange)
		change = &c
	}
	enriched, _ := targetEnriched.LoadAndDelete(command.Id)
	was, _ := enriched.(string)
	outcome := p.keepCheck(command, payload.Repository.TargetID, payload.Usage, change, was)
	if waiter, ok := targetWaiters.LoadAndDelete(command.Id); ok {
		waiter.(chan checkOutcome) <- outcome
	}
}

// keepCheck stores a check's result in its target. was is what a check of
// the stored target was completed with: if the target's credentials or
// server changed since, the result says nothing of it as it is now.
func (p *Panel) keepCheck(command *core.Record, targetID string, usage bool, change *targetChange, was string) checkOutcome {
	target, err := p.App.FindRecordById(schema.BackupTargets, targetID)
	if err != nil {
		return checkOutcome{problem: &targetProblem{http.StatusConflict, "The target is gone."}}
	}
	ctx := context.Background()
	if change != nil {
		ctx = audit.WithActor(ctx, change.actor)
	}
	stale := func(fresh *core.Record) bool {
		return change == nil && was != "" && was != targetFingerprint(fresh)
	}

	switch command.GetString("status") {
	case schema.CommandSucceeded:
		var check targetCheck
		raw, _ := json.Marshal(command.Get("result"))
		if err := json.Unmarshal(raw, &check); err != nil || check.ID == "" {
			// a command settled from the agent's list of finished ones has no result
			return checkOutcome{problem: &targetProblem{http.StatusBadGateway, "The agent's answer has no repository: check it again."}}
		}
		err := p.update(ctx, target, func(fresh *core.Record) error {
			if stale(fresh) {
				return errUnchanged
			}
			if change != nil {
				fresh.Set("host", change.host)
				fresh.Set("config", change.secrets.config)
				if err := p.sealTarget(fresh, change.secrets.creds, change.secrets.password); err != nil {
					return err
				}
			}
			if fresh.GetString("repo_id") == "" {
				fresh.Set("repo_id", check.ID)
			}
			fresh.Set("status", schema.TargetOK)
			fresh.Set("last_error", "")
			fresh.Set("checked_at", types.NowDateTime())
			used := int64(fresh.GetInt("used_bytes"))
			if check.Used != nil {
				used = *check.Used
				fresh.Set("used_bytes", used)
				fresh.Set("usage_at", types.NowDateTime())
			}
			if check.Free != nil {
				fresh.Set("capacity_bytes", used+*check.Free)
			}
			return nil
		})
		if err != nil {
			p.Log.Error("failed to keep the check of a backup target", "target", targetID, "error", err)
			return checkOutcome{problem: &targetProblem{http.StatusInternalServerError, "The check's result could not be kept."}}
		}
		return checkOutcome{created: check.Created}

	case schema.CommandFailed, schema.CommandTimedOut:
		message := command.GetString("error")
		if command.GetString("status") == schema.CommandTimedOut {
			message = "the check did not finish in time"
		}
		if message == "" {
			message = "the check failed"
		}
		secrets := targetSecrets{}
		if change != nil {
			secrets = change.secrets
		} else {
			secrets, _ = p.targetSecretsOf(target)
		}
		repo, _ := backup.Locate(secrets.kind, secrets.config, secrets.creds)
		// the agent took the secrets out already; whatever it missed
		message = capitalize(backup.Redact(message, backup.Secrets(secrets.password, repo.Env, repo.SSHKey)...))
		problem := &targetProblem{http.StatusBadRequest, message}
		// a change that failed leaves the target as it was; a measure that
		// takes longer than its time says nothing of whether it answers
		if change != nil || (usage && command.GetString("status") == schema.CommandTimedOut) {
			return checkOutcome{problem: problem}
		}
		err := p.update(ctx, target, func(fresh *core.Record) error {
			if stale(fresh) {
				return errUnchanged
			}
			fresh.Set("status", schema.TargetFailed)
			fresh.Set("last_error", truncateText(message, 4000))
			fresh.Set("checked_at", types.NowDateTime())
			return nil
		})
		if err != nil {
			p.Log.Error("failed to keep the check of a backup target", "target", targetID, "error", err)
		}
		return checkOutcome{problem: problem}
	}
	return checkOutcome{problem: &targetProblem{http.StatusGatewayTimeout, "The check was cancelled."}}
}

// targetFingerprint is what a target's commands are completed with, besides
// what never changes: its sealed credentials, its settings and its server.
func targetFingerprint(target *core.Record) string {
	return target.GetString("credentials") + "\n" + target.GetString("config") + "\n" + target.GetString("host")
}

// enrichBackup completes a backup command with its target's repository as it
// is delivered: its URL, the password, the storage's credentials and options,
// an SFTP key and host key, a local target's directory.
func (p *Panel) enrichBackup(record *core.Record, command *agentv1.Command) error {
	repo := backupRepository(command)
	if repo == nil || repo.GetTargetId() == "" {
		return errors.New("the command names no backup target")
	}
	var secrets targetSecrets
	if pending, ok := targetChanges.Load(record.Id); ok {
		change, _ := pending.(targetChange)
		if change.host != record.GetString("host") {
			return errors.New("the change of the backup target is for another server")
		}
		secrets = change.secrets
	} else {
		target, err := p.App.FindRecordById(schema.BackupTargets, repo.GetTargetId())
		if err != nil {
			return errors.New("the backup target is gone")
		}
		if target.GetString("host") != record.GetString("host") {
			return errors.New("the backup target is reached from another server")
		}
		if secrets, err = p.targetSecretsOf(target); err != nil {
			return err
		}
		if record.GetString("type") == "backup.init" {
			targetEnriched.Store(record.Id, targetFingerprint(target))
		}
	}
	located, err := backup.Locate(secrets.kind, secrets.config, secrets.creds)
	if err != nil {
		return err
	}
	repo.Url = located.URL
	repo.Password = secrets.password
	repo.Env = located.Env
	repo.Options = located.Options
	repo.SshKey = located.SSHKey
	repo.KnownHosts = located.KnownHosts
	repo.Mount = located.Mount
	return nil
}

// backupRepository is the repository a backup command carries.
func backupRepository(command *agentv1.Command) *agentv1.BackupRepository {
	switch payload := command.GetPayload().(type) {
	case *agentv1.Command_BackupInit:
		return payload.BackupInit.GetRepository()
	case *agentv1.Command_BackupRun:
		return payload.BackupRun.GetRepository()
	case *agentv1.Command_BackupRestore:
		return payload.BackupRestore.GetRepository()
	case *agentv1.Command_BackupSnapshots:
		return payload.BackupSnapshots.GetRepository()
	case *agentv1.Command_BackupBrowse:
		return payload.BackupBrowse.GetRepository()
	case *agentv1.Command_BackupCheck:
		return payload.BackupCheck.GetRepository()
	case *agentv1.Command_BackupUnlock:
		return payload.BackupUnlock.GetRepository()
	case *agentv1.Command_BackupForget:
		return payload.BackupForget.GetRepository()
	}
	return nil
}

// secretsMissingMessage says what is left of a target whose secrets cannot be
// opened, as one brought back without them.
const secretsMissingMessage = "The target's credentials or its repository's password cannot be opened: remove it and add it again." //nolint:gosec // a message, no credential

// secretsMissing marks a target whose secrets cannot be opened as failed: it
// never reads as one that answers.
func (p *Panel) secretsMissing(target *core.Record, err error) {
	p.Log.Warn("the secrets of a backup target cannot be opened", "target", target.GetString("name"), "error", err)
	p.setFields(target, map[string]any{"status": schema.TargetFailed, "last_error": secretsMissingMessage, "checked_at": types.NowDateTime()})
}

// targetSecretsOf opens a stored target's secrets.
func (p *Panel) targetSecretsOf(target *core.Record) (targetSecrets, error) {
	secrets := targetSecrets{kind: target.GetString("kind")}
	if err := target.UnmarshalJSONField("config", &secrets.config); err != nil {
		return secrets, fmt.Errorf("the target's settings cannot be read: %w", err)
	}
	if sealed := target.GetString("credentials"); sealed != "" {
		plain, err := p.Box.OpenString(sealed)
		if err != nil {
			return secrets, fmt.Errorf("the target's credentials cannot be opened: %w", err)
		}
		if err := json.Unmarshal([]byte(plain), &secrets.creds); err != nil {
			return secrets, fmt.Errorf("the target's credentials cannot be read: %w", err)
		}
	}
	password, err := p.Box.OpenString(target.GetString("repo_password"))
	if err != nil {
		return secrets, fmt.Errorf("the repository's password cannot be opened: %w", err)
	}
	secrets.password = password
	return secrets, nil
}

// sealTarget seals a target's credentials and its repository's password into
// its record.
func (p *Panel) sealTarget(target *core.Record, creds backup.Credentials, password string) error {
	raw, err := json.Marshal(creds) //nolint:gosec // to be sealed right below
	if err != nil {
		return err
	}
	sealedCreds, err := p.Box.SealString(string(raw))
	if err != nil {
		return err
	}
	sealedPassword, err := p.Box.SealString(password)
	if err != nil {
		return err
	}
	target.Set("credentials", sealedCreds)
	target.Set("repo_password", sealedPassword)
	return nil
}

// offlineTarget says that a target's server cannot check it now.
func offlineTarget(host *core.Record) string {
	return fmt.Sprintf("The server %s is offline: its agent checks the target with restic.", host.GetString("name"))
}

// targetName checks a target's name, which no other target has.
func (p *Panel) targetName(name *string, self string) (string, string) {
	if name == nil || strings.TrimSpace(*name) == "" {
		return "", "Give the target a name."
	}
	trimmed := strings.TrimSpace(*name)
	if utf8.RuneCountInString(trimmed) > 100 {
		return "", "The name is at most 100 characters."
	}
	others, err := p.App.FindRecordsByFilter(schema.BackupTargets, "name={:name} && id!={:self}", "", 1, 0,
		dbx.Params{"name": trimmed, "self": self})
	if err == nil && len(others) > 0 {
		return "", fmt.Sprintf("A target named %q exists already.", trimmed)
	}
	return trimmed, ""
}

// targetPlace refuses a second target of the same repository: their jobs
// would run side by side, and a change to one would not reach the other. A
// local target's place is a directory of its own server.
func (p *Panel) targetPlace(kind, location, hostID, self string) string {
	filter := "kind={:kind} && location={:location} && id!={:self}"
	if kind == backup.KindLocal {
		filter += " && host={:host}"
	}
	others, err := p.App.FindRecordsByFilter(schema.BackupTargets, filter, "", 1, 0,
		dbx.Params{"kind": kind, "location": location, "self": self, "host": hostID})
	if err == nil && len(others) > 0 {
		return fmt.Sprintf("The target %q is there already.", others[0].GetString("name"))
	}
	return ""
}

// targetNotes checks what an admin notes of a target: the days a bucket's
// Object Lock keeps objects, and a note.
func targetNotes(days *int, note *string) (int, string, string) {
	d, n := 0, ""
	if days != nil {
		d = *days
	}
	if note != nil {
		n = strings.TrimSpace(*note)
	}
	if d < 0 || d > maxImmutableDays {
		return 0, "", "Object Lock keeps objects for 0 to 36500 days."
	}
	if utf8.RuneCountInString(n) > 500 {
		return 0, "", "The note is at most 500 characters."
	}
	return d, n, ""
}
