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

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// Backup targets (#28) are where backups go: an S3-compatible bucket, a
// directory on an SFTP server, a restic rest-server, or a directory of a
// server. The panel keeps their settings, their credentials and each
// repository's password, sealed; the agent of the target's server runs
// restic. A new target is saved only once that agent opened its repository,
// or created it: one that cannot be reached is refused with restic's words.
//
// The secrets leave the panel in one place only: the backup command for the
// agent that runs it, completed as it is delivered. The stored command names
// the target, which viewers may read; a target's secrets never reach the API.

const (
	// backupInitWait is how long adding or checking a target waits for the
	// agent: restic may retry a storage that does not answer for a while.
	backupInitWait   = 2 * time.Minute
	maxImmutableDays = 36500
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

// pendingTargets holds the secrets of a target that are not stored yet, by
// the id of the backup.init that checks them: those of a new target, or new
// credentials, until restic took them.
var pendingTargets sync.Map

// targetProblem is an answer of the target routes other than success.
type targetProblem struct {
	status  int
	message string
}

func (t *targetProblem) Error() string { return t.message }

// targetCheck is what backup.init found.
type targetCheck struct {
	Created  bool   `json:"created"`
	ID       string `json:"id"`
	Used     int64  `json:"used_bytes"`
	Capacity int64  `json:"capacity_bytes"`
}

func (p *Panel) registerBackups() {
	for _, commandType := range backupCommandTypes {
		p.Commands.RegisterEnricher(commandType, p.enrichBackup)
	}
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
	// Password is that of an existing repository, to add it; a new one gets
	// a password of its own.
	Password string `json:"password"`
}

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
	if problem := p.targetPlace(body.Kind, location, host.Id, ""); problem != "" {
		return e.BadRequestError(problem, nil)
	}
	password := body.Password
	if password == "" {
		if password, err = backup.NewPassword(); err != nil {
			return e.InternalServerError("", err)
		}
	} else if len(password) > 1000 || strings.ContainsAny(password, "\n\r\x00") {
		return e.BadRequestError("The repository's password is one line of at most 1000 characters.", nil)
	}

	id := core.GenerateDefaultRandomId()
	secrets := targetSecrets{kind: body.Kind, config: config, creds: creds, password: password}
	requestedBy, requester := requester(e)
	check, err := p.checkTarget(e.Request.Context(), id, name, host, secrets, true, "", requestedBy, requester)
	if err != nil {
		return targetError(e, err)
	}

	collection, err := e.App.FindCachedCollectionByNameOrId(schema.BackupTargets)
	if err != nil {
		return e.InternalServerError("", err)
	}
	target := core.NewRecord(collection)
	target.Id = id
	target.Set("name", name)
	target.Set("kind", body.Kind)
	target.Set("host", host.Id)
	target.Set("config", config)
	target.Set("location", location)
	target.Set("immutable_days", days)
	target.Set("note", note)
	if err := p.sealTarget(target, creds, password); err != nil {
		return e.InternalServerError("", err)
	}
	checked(target, check)
	if err := e.App.Save(target); err != nil {
		return e.BadRequestError("The target could not be saved: "+err.Error(), nil)
	}
	return e.JSON(http.StatusCreated, target)
}

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
	if body.Name != nil {
		name, problem := p.targetName(body.Name, target.Id)
		if problem != "" {
			return e.BadRequestError(problem, nil)
		}
		target.Set("name", name)
	}
	if body.ImmutableDays != nil || body.Note != nil {
		days, note, problem := targetNotes(body.ImmutableDays, body.Note)
		if problem != "" {
			return e.BadRequestError(problem, nil)
		}
		if body.ImmutableDays != nil {
			target.Set("immutable_days", days)
		}
		if body.Note != nil {
			target.Set("note", note)
		}
	}

	if body.Config != nil || body.Credentials != nil || (body.Host != nil && *body.Host != target.GetString("host")) {
		stored, err := p.targetSecretsOf(target)
		if err != nil {
			return e.InternalServerError("", err)
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
		secrets := targetSecrets{kind: kind, config: config, creds: creds, password: stored.password}
		check, err := p.checkTarget(e.Request.Context(), target.Id, target.GetString("name"), host, secrets, true,
			target.GetString("repo_id"), requestedBy, requester)
		if err != nil {
			return targetError(e, err)
		}
		target.Set("host", host.Id)
		target.Set("config", config)
		if err := p.sealTarget(target, creds, stored.password); err != nil {
			return e.InternalServerError("", err)
		}
		checked(target, check)
	}
	if err := e.App.Save(target); err != nil {
		return e.BadRequestError("The target could not be saved: "+err.Error(), nil)
	}
	return e.JSON(http.StatusOK, target)
}

// handleRefreshTarget checks a target again: whether its repository still
// opens, and what it holds. A check that fails is the target's state.
func (p *Panel) handleRefreshTarget(e *core.RequestEvent) error {
	target, err := e.App.FindRecordById(schema.BackupTargets, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The target does not exist.", err)
	}
	requestedBy, requester := requester(e)
	if err := p.refreshTarget(e.Request.Context(), target, requestedBy, requester); err != nil {
		return targetError(e, err)
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
	if err := e.App.Delete(target); err != nil {
		return e.BadRequestError("The target could not be removed: "+err.Error(), nil)
	}
	return e.NoContent(http.StatusNoContent)
}

// refreshTarget checks a stored target with its stored secrets, and keeps
// what the check found. Only an agent that could not be asked is an error.
func (p *Panel) refreshTarget(ctx context.Context, target *core.Record, requestedBy, requester string) error {
	host, err := p.App.FindRecordById(schema.Hosts, target.GetString("host"))
	if err != nil {
		return &targetProblem{http.StatusConflict, "The server of the target is gone."}
	}
	secrets, err := p.targetSecretsOf(target)
	if err != nil {
		return err
	}
	check, err := p.checkTarget(ctx, target.Id, target.GetString("name"), host, secrets, false,
		target.GetString("repo_id"), requestedBy, requester)
	var problem *targetProblem
	switch {
	case errors.As(err, &problem) && problem.status == http.StatusBadRequest:
		// restic's answer: the target's state now
		p.setFields(target, map[string]any{"status": schema.TargetFailed, "last_error": truncateText(problem.message, 4000), "checked_at": types.NowDateTime()})
		return nil
	case err != nil:
		return err
	}
	fields := map[string]any{"status": schema.TargetOK, "last_error": "", "checked_at": types.NowDateTime(),
		"used_bytes": check.Used, "capacity_bytes": check.Capacity}
	if target.GetString("repo_id") == "" {
		fields["repo_id"] = check.ID
	}
	p.setFields(target, fields)
	return nil
}

// CheckBackupTargets checks every target whose server is connected, each on its
// own: hourly, for what they hold and whether they still answer.
func (p *Panel) CheckBackupTargets() {
	targets, err := p.App.FindAllRecords(schema.BackupTargets)
	if err != nil {
		p.Log.Error("failed to read the backup targets", "error", err)
		return
	}
	for _, target := range targets {
		if !p.Commands.Connected(target.GetString("host")) {
			continue
		}
		p.later.after(0, func() {
			ctx, cancel := context.WithTimeout(context.Background(), backupInitWait+time.Minute)
			defer cancel()
			if err := p.refreshTarget(ctx, target, "", "PocketCloud"); err != nil {
				p.Log.Warn("failed to check a backup target", "target", target.GetString("name"), "error", err)
			}
		})
	}
}

// checkTarget has the agent of host open the target's repository with the
// secrets given, which are pending when they are not stored yet. expect is
// the id of the repository the target has had, which is only opened; without
// it, a missing repository is created. A problem says what to answer: 400
// with restic's words, 409 offline, 504 no answer in time.
func (p *Panel) checkTarget(ctx context.Context, targetID, name string, host *core.Record, secrets targetSecrets, pending bool,
	expect, requestedBy, requester string,
) (targetCheck, error) {
	var check targetCheck
	repo, err := backup.Locate(secrets.kind, secrets.config, secrets.creds)
	if err != nil {
		return check, &targetProblem{http.StatusBadRequest, capitalize(err.Error()) + "."}
	}
	if !p.Commands.Connected(host.Id) {
		return check, &targetProblem{http.StatusConflict, fmt.Sprintf("%s is offline: its agent checks the target with restic.", host.GetString("name"))}
	}
	commandID := core.GenerateDefaultRandomId()
	if pending {
		// there before the command can be delivered
		pendingTargets.Store(commandID, secrets)
		defer pendingTargets.Delete(commandID)
	}
	command, err := p.Commands.Create(ctx, commands.Options{
		ID:   commandID,
		Host: host.Id,
		// the place, never a secret: the enricher adds those
		Command: &agentv1.Command{Payload: &agentv1.Command_BackupInit{BackupInit: &agentv1.BackupInit{
			Repository: &agentv1.BackupRepository{TargetId: targetID, Url: repo.URL},
			RepoId:     expect,
		}}},
		Label:       truncateText("Check the backup target "+name, 255),
		RequestedBy: requestedBy,
		Requester:   requester,
	})
	if err != nil {
		return check, &targetProblem{http.StatusBadRequest, capitalize(err.Error()) + "."}
	}
	done := p.waitForCommand(ctx, command.Id, backupInitWait)
	if done == nil {
		// a check that ends later would surprise whoever waits no more
		_ = p.Commands.Cancel(command.Id, "The panel stopped waiting for it.")
		return check, &targetProblem{http.StatusGatewayTimeout, fmt.Sprintf("The agent of %s did not answer in time.", host.GetString("name"))}
	}
	if done.GetString("status") != schema.CommandSucceeded {
		message := done.GetString("error")
		if message == "" {
			message = "the check " + done.GetString("status")
		}
		// the agent took the secrets out already; whatever it missed
		message = backup.Redact(message, backup.Secrets(secrets.password, repo.Env, repo.SSHKey)...)
		return check, &targetProblem{http.StatusBadRequest, capitalize(message)}
	}
	raw, _ := json.Marshal(done.Get("result"))
	if err := json.Unmarshal(raw, &check); err != nil || check.ID == "" {
		// a command settled from the agent's list of finished ones has no result
		return check, &targetProblem{http.StatusBadGateway, "The agent's answer has no repository: try again."}
	}
	return check, nil
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
	if pending, ok := pendingTargets.Load(record.Id); ok {
		secrets, _ = pending.(targetSecrets)
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

// checked keeps what a check found in a target's record.
func checked(target *core.Record, check targetCheck) {
	target.Set("status", schema.TargetOK)
	target.Set("last_error", "")
	target.Set("checked_at", types.NowDateTime())
	target.Set("repo_id", check.ID)
	target.Set("used_bytes", check.Used)
	target.Set("capacity_bytes", check.Capacity)
}

// targetName checks a target's name, which no other target has.
func (p *Panel) targetName(name *string, self string) (string, string) {
	if name == nil || strings.TrimSpace(*name) == "" {
		return "", "Give the target a name."
	}
	trimmed := strings.TrimSpace(*name)
	if len(trimmed) > 100 {
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
	if len(n) > 500 {
		return 0, "", "The note is at most 500 characters."
	}
	return d, n, ""
}

// targetError answers a problem of a target route.
func targetError(e *core.RequestEvent, err error) error {
	var problem *targetProblem
	if errors.As(err, &problem) {
		return e.Error(problem.status, problem.message, nil)
	}
	return e.InternalServerError("", err)
}
