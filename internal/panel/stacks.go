package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/gateway"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/stacks"
)

// States of a stack, in stacks.status.
const (
	stackDraft     = "draft"
	stackDeploying = "deploying"
	stackDeployed  = "deployed"
	stackFailed    = "failed"
	stackStopped   = "stopped"
	stackDown      = "down"
	stackImported  = "imported"
)

const (
	labelComposeProject = "com.docker.compose.project"
	labelConfigFiles    = "com.docker.compose.project.config_files"
	labelWorkingDir     = "com.docker.compose.project.working_dir"

	artifactStackImport = "stack_import"

	// a save passes these on to the revision hook; they are not fields of a stack
	recordAuthor = "@revision_author"
	recordNote   = "@revision_note"
)

var (
	secretKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretLikeKey    = regexp.MustCompile(`(?i)pass|secret|token|credential|private|key`)
	revisionStepFrom = regexp.MustCompile(`^r(\d+) → r\d+$`)
)

// registerStacks binds what every save of a stack goes through, however it arrives.
func (p *Panel) registerStacks() {
	p.App.OnRecordCreateRequest(schema.Stacks).BindFunc(p.guardNewStack)
	p.App.OnRecordUpdateRequest(schema.Stacks).BindFunc(p.guardStackUpdate)

	p.App.OnRecordCreateExecute(schema.Stacks).BindFunc(p.reviseStack)
	p.App.OnRecordUpdateExecute(schema.Stacks).BindFunc(p.reviseStack)

	p.Commands.RegisterEnricher("stack.deploy", p.enrichStackDeploy)
	p.Commands.RegisterEnricher("stack.pull", p.enrichStackPull)

	p.Commands.OnFinish("stack.deploy", p.stackDeployFinished)
	p.Commands.OnFinish("stack.stop", p.stackStateAfter(stackStopped))
	p.Commands.OnFinish("stack.down", p.stackStateAfter(stackDown))
	p.Commands.OnFinish("stack.restart", p.stackRestartFinished)
}

// guardNewStack keeps what only the panel may set out of a new stack made through
// the REST API. Secrets have their own way in.
func (p *Panel) guardNewStack(e *core.RecordRequestEvent) error {
	for _, field := range []string{"secrets", "secret_keys", "current_revision", "deployed_revision", "deployed_at", "source"} {
		e.Record.Set(field, nil)
	}
	e.Record.Set("status", stackDraft)

	if e.Auth != nil && !e.HasSuperuserAuth() {
		e.Record.Set(recordAuthor, e.Auth.Id)
	}

	return e.Next()
}

// guardStackUpdate does the same for a change. A stack keeps its name and its host:
// they say which Compose project on which server it is.
func (p *Panel) guardStackUpdate(e *core.RecordRequestEvent) error {
	stored, err := e.App.FindRecordById(schema.Stacks, e.Record.Id)
	if err != nil {
		return e.NotFoundError("The stack does not exist.", err)
	}

	if e.Record.GetString("name") != stored.GetString("name") || e.Record.GetString("host") != stored.GetString("host") {
		return e.BadRequestError("A stack keeps its name and its server. Create a new stack instead.", nil)
	}

	for _, field := range []string{"secrets", "secret_keys", "current_revision", "deployed_revision", "deployed_at", "source", "status"} {
		e.Record.Set(field, stored.Get(field))
	}

	if e.Auth != nil && !e.HasSuperuserAuth() {
		e.Record.Set(recordAuthor, e.Auth.Id)
	}

	return e.Next()
}

// reviseStack adds a revision for every save that changes the files, in the same
// transaction. It runs inside the audit's transaction, and so does the audit's
// entry that says which revision this was.
func (p *Panel) reviseStack(e *core.RecordEvent) error {
	compose, env := e.Record.GetString("compose_yaml"), e.Record.GetString("env")

	stored, err := e.App.FindRecordById(schema.Stacks, e.Record.Id)
	if err != nil {
		stored = nil
	}

	var changed bool
	if stored == nil {
		changed = compose != "" || env != ""
	} else {
		changed = compose != stored.GetString("compose_yaml") || env != stored.GetString("env")
	}
	if !changed {
		return e.Next()
	}

	number := 1
	if stored != nil {
		latest, err := e.App.FindRecordsByFilter(schema.StackRevisions, "stack={:stack}", "-number", 1, 0, dbx.Params{"stack": e.Record.Id})
		if err != nil {
			return err
		}
		if len(latest) > 0 {
			number = latest[0].GetInt("number") + 1
		}
	}

	e.Record.Set("current_revision", number)

	if err := e.Next(); err != nil {
		return err
	}

	collection, err := e.App.FindCachedCollectionByNameOrId(schema.StackRevisions)
	if err != nil {
		return err
	}

	revision := core.NewRecord(collection)
	revision.Set("stack", e.Record.Id)
	revision.Set("number", number)
	revision.Set("compose_yaml", compose)
	revision.Set("env", env)
	revision.Set("env_keys", stacks.EnvKeys(env))
	revision.Set("author", e.Record.GetString(recordAuthor))
	revision.Set("note", e.Record.GetString(recordNote))

	return e.App.Save(revision)
}

// openStackSecrets returns the secrets of a stack.
func (p *Panel) openStackSecrets(stack *core.Record) (map[string]string, error) {
	sealed := stack.GetString("secrets")
	if sealed == "" {
		return map[string]string{}, nil
	}

	plain, err := p.Box.OpenString(sealed)
	if err != nil {
		return nil, fmt.Errorf("the secrets of %s cannot be opened: %w", stack.GetString("name"), err)
	}

	secrets := map[string]string{}
	if err := json.Unmarshal([]byte(plain), &secrets); err != nil {
		return nil, fmt.Errorf("the secrets of %s are damaged: %w", stack.GetString("name"), err)
	}

	return secrets, nil
}

// secretChange is how secrets change: values go in, and are never read back.
type secretChange struct {
	Set    map[string]string `json:"set"`
	Remove []string          `json:"remove"`
}

func (p *Panel) applySecretChange(stack *core.Record, change *secretChange) error {
	if change == nil || len(change.Set)+len(change.Remove) == 0 {
		return nil
	}

	secrets, err := p.openStackSecrets(stack)
	if err != nil {
		return err
	}

	for key, value := range change.Set {
		if !secretKeyPattern.MatchString(key) {
			return fmt.Errorf("%q is not a valid variable name", key)
		}
		secrets[key] = value
	}
	for _, key := range change.Remove {
		delete(secrets, key)
	}

	raw, err := json.Marshal(secrets)
	if err != nil {
		return err
	}

	stack.Set("secrets", string(raw))
	stack.Set("secret_keys", slices.Sorted(maps.Keys(secrets)))

	return nil
}

func secretKeysOf(stack *core.Record) []string {
	keys := []string{}
	if err := stack.UnmarshalJSONField("secret_keys", &keys); err != nil {
		return []string{}
	}
	return keys
}

// enrichStackDeploy puts the secrets into the command right before delivery. The
// stored payload only has the env file with its references.
func (p *Panel) enrichStackDeploy(record *core.Record, command *agentv1.Command) error {
	payload := command.GetStackDeploy()

	stack, err := p.App.FindFirstRecordByFilter(schema.Stacks, "host={:host} && name={:name}",
		dbx.Params{"host": record.GetString("host"), "name": payload.GetName()})
	if err != nil {
		return fmt.Errorf("the stack %s no longer exists: %w", payload.GetName(), err)
	}

	secrets, err := p.openStackSecrets(stack)
	if err != nil {
		return err
	}

	plain, referenced, err := stacks.ResolveEnv(payload.GetEnv(), p.secretResolver(stack))
	if err != nil {
		return err
	}
	maps.Copy(secrets, referenced)

	payload.Env = plain
	payload.Secrets = secrets
	payload.OverrideYaml, payload.DbNetworks, err = p.stackOverride(stack, payload.GetComposeYaml())

	return err
}

// enrichStackPull gives a pull the same variables as a deploy: the Compose file may
// need them just to be read.
func (p *Panel) enrichStackPull(record *core.Record, command *agentv1.Command) error {
	payload := command.GetStackPull()

	stack, err := p.App.FindFirstRecordByFilter(schema.Stacks, "host={:host} && name={:name}",
		dbx.Params{"host": record.GetString("host"), "name": payload.GetName()})
	if err != nil {
		// a project that is only known from the inventory has nothing to add
		return nil //nolint:nilerr
	}

	secrets, err := p.openStackSecrets(stack)
	if err != nil {
		return err
	}

	_, referenced, err := stacks.ResolveEnv(stack.GetString("env"), p.secretResolver(stack))
	if err != nil {
		return err
	}
	maps.Copy(secrets, referenced)

	payload.Secrets = secrets

	return nil
}

// stackDeployFinished records the outcome of a deploy.
func (p *Panel) stackDeployFinished(record *core.Record) {
	raw, err := json.Marshal(record.Get("payload"))
	if err != nil {
		return
	}

	var payload struct {
		Name     string `json:"name"`
		Revision int    `json:"revision"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return
	}

	stack, err := p.App.FindFirstRecordByFilter(schema.Stacks, "host={:host} && name={:name}",
		dbx.Params{"host": record.GetString("host"), "name": payload.Name})
	if err != nil {
		return
	}

	if record.GetString("status") != schema.CommandSucceeded {
		p.setFields(stack, map[string]any{"status": stackFailed})
		return
	}

	now := types.NowDateTime()

	p.setFields(stack, map[string]any{"status": stackDeployed, "deployed_revision": payload.Revision, "deployed_at": now})

	revision, err := p.App.FindFirstRecordByFilter(schema.StackRevisions, "stack={:stack} && number={:number}",
		dbx.Params{"stack": stack.Id, "number": payload.Revision})
	if err != nil {
		return
	}
	revision.Set("deployed_at", now)
	if err := p.App.Save(revision); err != nil {
		p.Log.Error("failed to mark a revision as deployed", "revision", revision.Id, "error", err)
	}
}

// stackStateAfter stores what a successful stop or down leaves behind.
func (p *Panel) stackStateAfter(state string) func(*core.Record) {
	return func(record *core.Record) {
		if stack := p.succeededStack(record); stack != nil {
			p.setFields(stack, map[string]any{"status": state})
		}
	}
}

// stackRestartFinished: a restart starts stopped containers again, so a stopped
// stack is back to what it was before the stop. A down left no containers to
// start, and other states do not change.
func (p *Panel) stackRestartFinished(record *core.Record) {
	stack := p.succeededStack(record)
	if stack == nil {
		return
	}

	err := p.update(context.Background(), stack, func(fresh *core.Record) error {
		if fresh.GetString("status") != stackStopped {
			return errUnchanged
		}
		state := stackImported
		if fresh.GetInt("deployed_revision") > 0 {
			state = stackDeployed
		}
		fresh.Set("status", state)
		return nil
	})
	if err != nil {
		p.Log.Error("failed to store the state of a stack", "stack", stack.Id, "error", err)
	}
}

// succeededStack returns the stack a succeeded stack command was about, if it
// still exists.
func (p *Panel) succeededStack(record *core.Record) *core.Record {
	if record.GetString("status") != schema.CommandSucceeded {
		return nil
	}

	raw, err := json.Marshal(record.Get("payload"))
	if err != nil {
		return nil
	}

	var payload struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}

	stack, err := p.App.FindFirstRecordByFilter(schema.Stacks, "host={:host} && name={:name}",
		dbx.Params{"host": record.GetString("host"), "name": payload.Name})
	if err != nil {
		return nil
	}

	return stack
}

func (p *Panel) bindStackRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	everyone := requireRole(schema.RoleViewer, schema.RoleOperator, schema.RoleAdmin)
	operators := requireRole(schema.RoleOperator, schema.RoleAdmin)

	group.POST("/stacks/validate", p.handleStackValidate).BindFunc(operators)
	group.POST("/stacks/{id}/save", p.handleStackSave).BindFunc(operators)
	group.POST("/stacks/{id}/deploy", p.handleStackDeploy).BindFunc(operators)
	group.POST("/stacks/{id}/rollback", p.handleStackRollback).BindFunc(operators)
	group.GET("/hosts/{id}/compose-projects", p.handleComposeProjects).BindFunc(everyone)
	group.POST("/hosts/{id}/compose-projects/{project}/import", p.handleComposeImport).BindFunc(operators)
}

// handleStackValidate is what the editor asks while someone types.
func (p *Panel) handleStackValidate(e *core.RequestEvent) error {
	var body struct {
		Name       string   `json:"name"`
		Compose    string   `json:"compose_yaml"`
		Env        string   `json:"env"`
		SecretKeys []string `json:"secret_keys"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}

	name := body.Name
	if name == "" {
		name = "stack"
	}

	return e.JSON(http.StatusOK, stacks.Validate(e.Request.Context(), name, body.Compose, body.Env, body.SecretKeys))
}

// stackEdit is a change of a stack's files and secrets.
type stackEdit struct {
	Compose *string       `json:"compose_yaml"`
	Env     *string       `json:"env"`
	Note    string        `json:"note"`
	Secrets *secretChange `json:"secrets"`
}

// saveStackEdit applies an edit. If the files change, the hook adds a revision.
func (p *Panel) saveStackEdit(ctx context.Context, e *core.RequestEvent, stack *core.Record, edit stackEdit, actor audit.Actor) error {
	if edit.Note != "" && actor.Note == "" {
		actor.Note = edit.Note
	}

	// the edit applies to the stack as it is stored at this moment, not as it was
	// read when the request came in
	return p.update(audit.WithActor(ctx, actor), stack, func(fresh *core.Record) error {
		if edit.Compose != nil {
			fresh.Set("compose_yaml", *edit.Compose)
		}
		if edit.Env != nil {
			fresh.Set("env", *edit.Env)
		}
		if err := p.applySecretChange(fresh, edit.Secrets); err != nil {
			return err
		}

		if edit.Note != "" {
			fresh.Set(recordNote, edit.Note)
		}
		if !e.HasSuperuserAuth() {
			fresh.Set(recordAuthor, e.Auth.Id)
		}
		return nil
	})
}

func (p *Panel) handleStackSave(e *core.RequestEvent) error {
	stack, err := p.App.FindRecordById(schema.Stacks, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The stack does not exist.", err)
	}

	var edit stackEdit
	if err := e.BindBody(&edit); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}

	if err := p.saveStackEdit(e.Request.Context(), e, stack, edit, audit.ActorFromRequest(e)); err != nil {
		return e.BadRequestError(err.Error(), nil)
	}

	return e.JSON(http.StatusOK, stack)
}

// deployRejection is the answer to a deploy of a file with problems: what the
// editor needs to point at the lines.
type deployRejection struct {
	Message string         `json:"message"`
	Result  *stacks.Result `json:"validation,omitempty"`
}

func (r *deployRejection) Error() string {
	return r.Message
}

// deployRevision validates a revision and queues its deploy. Nothing goes to the
// server if the file has a problem.
func (p *Panel) deployRevision(ctx context.Context, stack, revision *core.Record, pull bool, commandID, userID, label string) (*core.Record, error) {
	compose, env := revision.GetString("compose_yaml"), revision.GetString("env")

	result := stacks.Validate(ctx, stack.GetString("name"), compose, env, secretKeysOf(stack))
	if !result.Valid {
		return nil, &deployRejection{Message: "The Compose file has problems, so it was not deployed.", Result: &result}
	}

	if _, _, err := stacks.ResolveEnv(env, p.secretResolver(stack)); err != nil {
		return nil, &deployRejection{Message: "The env file refers to a secret that does not exist: " + err.Error()}
	}

	command, err := p.Commands.Create(ctx, commands.Options{
		ID:          commandID,
		Host:        stack.GetString("host"),
		Label:       fmt.Sprintf("%s r%d", stack.GetString("name"), revision.GetInt("number")),
		RequestedBy: userID,
		Requester:   label,
		Command: &agentv1.Command{Payload: &agentv1.Command_StackDeploy{StackDeploy: &agentv1.StackDeploy{
			Name:        stack.GetString("name"),
			ComposeYaml: compose,
			Env:         env,
			Revision:    int32(revision.GetInt("number")), //nolint:gosec // a small counter
			Pull:        pull,
		}}},
	})
	if err != nil {
		return nil, err
	}

	err = p.update(ctx, stack, func(fresh *core.Record) error {
		fresh.Set("status", stackDeploying)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return command, nil
}

func (p *Panel) findRevision(stack *core.Record, number int) (*core.Record, error) {
	return p.App.FindFirstRecordByFilter(schema.StackRevisions, "stack={:stack} && number={:number}",
		dbx.Params{"stack": stack.Id, "number": number})
}

func (p *Panel) deployAnswer(e *core.RequestEvent, command *core.Record, stack *core.Record, err error) error {
	if err != nil {
		var rejected *deployRejection
		if errors.As(err, &rejected) {
			return e.JSON(http.StatusUnprocessableEntity, rejected)
		}
		return e.InternalServerError("Failed to queue the deploy.", err)
	}

	return e.JSON(http.StatusOK, map[string]any{"command": command, "stack": stack})
}

// handleStackDeploy saves what the editor holds, if anything changed, and deploys
// the current revision or the one that was asked for.
func (p *Panel) handleStackDeploy(e *core.RequestEvent) error {
	stack, err := p.App.FindRecordById(schema.Stacks, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The stack does not exist.", err)
	}

	var body struct {
		stackEdit
		Revision int  `json:"revision"`
		Pull     bool `json:"pull"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}

	ctx := e.Request.Context()

	// a file with problems is not worth a revision either
	if body.Compose != nil {
		env := stack.GetString("env")
		if body.Env != nil {
			env = *body.Env
		}
		result := stacks.Validate(ctx, stack.GetString("name"), *body.Compose, env, secretKeysOf(stack))
		if !result.Valid {
			return p.deployAnswer(e, nil, stack, &deployRejection{Message: "The Compose file has problems, so it was not deployed.", Result: &result})
		}
	}

	commandID := core.GenerateDefaultRandomId()

	actor := audit.ActorFromRequest(e)
	actor.Command = commandID

	if err := p.saveStackEdit(ctx, e, stack, body.stackEdit, actor); err != nil {
		return e.BadRequestError(err.Error(), nil)
	}

	number := stack.GetInt("current_revision")
	if body.Revision > 0 {
		number = body.Revision
	}

	revision, err := p.findRevision(stack, number)
	if err != nil {
		return e.BadRequestError(fmt.Sprintf("The stack has no revision %d.", number), err)
	}

	userID, label := requester(e)

	command, err := p.deployRevision(ctx, stack, revision, body.Pull, commandID, userID, label)

	return p.deployAnswer(e, command, stack, err)
}

// rollbackStack deploys an old revision as a new one: the history only grows, and
// the old files reach the server the same way as any others.
func (p *Panel) rollbackStack(ctx context.Context, e *core.RequestEvent, stack *core.Record, number int, actor audit.Actor) (*core.Record, error) {
	old, err := p.findRevision(stack, number)
	if err != nil {
		return nil, &deployRejection{Message: fmt.Sprintf("The stack has no revision %d.", number)}
	}

	compose, env := old.GetString("compose_yaml"), old.GetString("env")

	commandID := core.GenerateDefaultRandomId()
	actor.Command = commandID
	if actor.Note == "" {
		actor.Note = fmt.Sprintf("Rollback to r%d", number)
	}

	edit := stackEdit{Compose: &compose, Env: &env, Note: fmt.Sprintf("Rollback to r%d", number)}
	if err := p.saveStackEdit(ctx, e, stack, edit, actor); err != nil {
		return nil, err
	}

	revision, err := p.findRevision(stack, stack.GetInt("current_revision"))
	if err != nil {
		return nil, err
	}

	userID, label := requester(e)

	return p.deployRevision(ctx, stack, revision, false, commandID, userID, label)
}

func (p *Panel) handleStackRollback(e *core.RequestEvent) error {
	stack, err := p.App.FindRecordById(schema.Stacks, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The stack does not exist.", err)
	}

	var body struct {
		Revision int `json:"revision"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}

	command, err := p.rollbackStack(e.Request.Context(), e, stack, body.Revision, audit.ActorFromRequest(e))

	return p.deployAnswer(e, command, stack, err)
}

// revertStack is the audit's revert for a stack: it deploys the revision the entry
// moved away from, as a new one.
func (p *Panel) revertStack(e *core.RequestEvent, entry *core.Record, actor audit.Actor) error {
	match := revisionStepFrom.FindStringSubmatch(entry.GetString("revision"))
	if match == nil {
		return e.BadRequestError("This change did not touch the stack's files, so there is no older revision to go back to.", nil)
	}

	number, err := strconv.Atoi(match[1])
	if err != nil || number == 0 {
		return e.BadRequestError("There is no older revision to go back to.", err)
	}

	stack, err := p.App.FindRecordById(schema.Stacks, entry.GetString("record_id"))
	if err != nil {
		return e.BadRequestError("The stack no longer exists.", err)
	}

	if _, err := p.rollbackStack(e.Request.Context(), e, stack, number, actor); err != nil {
		var rejected *deployRejection
		if errors.As(err, &rejected) {
			return e.BadRequestError(rejected.Message, nil)
		}
		return e.InternalServerError("Failed to revert the change.", err)
	}

	return e.NoContent(http.StatusNoContent)
}

// composeProject is a Compose project found on a host that no stack manages.
type composeProject struct {
	Project     string   `json:"project"`
	Services    []string `json:"services"`
	Running     int      `json:"running"`
	Total       int      `json:"total"`
	ConfigFiles []string `json:"config_files"`
	WorkingDir  string   `json:"working_dir"`
}

// unmanagedProjects reads them from the inventory. PocketCloud's own containers
// have a role and are not there to be imported.
func (p *Panel) unmanagedProjects(hostID string) ([]*composeProject, error) {
	records, err := p.App.FindRecordsByFilter(schema.Containers, "host={:host} && project!='' && system_role=''", "project,service", 0, 0, dbx.Params{"host": hostID})
	if err != nil {
		return nil, err
	}

	managed, err := p.App.FindRecordsByFilter(schema.Stacks, "host={:host}", "", 0, 0, dbx.Params{"host": hostID})
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, stack := range managed {
		known[stack.GetString("name")] = true
	}

	projects := []*composeProject{}
	byName := map[string]*composeProject{}

	for _, record := range records {
		name := record.GetString("project")
		if known[name] {
			continue
		}

		project := byName[name]
		if project == nil {
			project = &composeProject{Project: name, Services: []string{}, ConfigFiles: []string{}}
			byName[name] = project
			projects = append(projects, project)
		}

		project.Total++
		if record.GetString("state") == "running" {
			project.Running++
		}
		if service := record.GetString("service"); service != "" && !slices.Contains(project.Services, service) {
			project.Services = append(project.Services, service)
		}

		labels := map[string]string{}
		if err := record.UnmarshalJSONField("labels", &labels); err == nil {
			if files := labels[labelConfigFiles]; files != "" && len(project.ConfigFiles) == 0 {
				project.ConfigFiles = strings.Split(files, ",")
			}
			if dir := labels[labelWorkingDir]; dir != "" {
				project.WorkingDir = dir
			}
		}
	}

	return projects, nil
}

func (p *Panel) handleComposeProjects(e *core.RequestEvent) error {
	host, err := p.App.FindRecordById(schema.Hosts, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The server does not exist.", err)
	}

	projects, err := p.unmanagedProjects(host.Id)
	if err != nil {
		return e.InternalServerError("Failed to read the inventory.", err)
	}

	return e.JSON(http.StatusOK, projects)
}

func (p *Panel) handleComposeImport(e *core.RequestEvent) error {
	host, err := p.App.FindRecordById(schema.Hosts, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The server does not exist.", err)
	}

	projects, err := p.unmanagedProjects(host.Id)
	if err != nil {
		return e.InternalServerError("Failed to read the inventory.", err)
	}

	name := e.Request.PathValue("project")

	var project *composeProject
	for _, candidate := range projects {
		if candidate.Project == name {
			project = candidate
		}
	}
	if project == nil {
		return e.NotFoundError("This server has no such project to import.", nil)
	}

	userID, label := requester(e)

	command, err := p.Commands.Create(e.Request.Context(), commands.Options{
		Host:        host.Id,
		Label:       name,
		RequestedBy: userID,
		Requester:   label,
		Command: &agentv1.Command{Payload: &agentv1.Command_StackImport{StackImport: &agentv1.StackImport{
			Project:     name,
			ConfigFiles: project.ConfigFiles,
			WorkingDir:  project.WorkingDir,
		}}},
	})
	if err != nil {
		return e.InternalServerError("Failed to queue the import.", err)
	}

	return e.JSON(http.StatusOK, command)
}

// artifactHandlers lists what the gateway does with uploads besides diagnostics.
func (p *Panel) artifactHandlers() map[string]gateway.ArtifactHandler {
	return map[string]gateway.ArtifactHandler{
		artifactStackImport: p.importStack,
		artifactSQLExport:   p.storeDump(artifactSQLExport),
		artifactSQLFinal:    p.storeDump(artifactSQLFinal),
	}
}

// importStack makes a stack of the files of a Compose project. What looks like a
// secret in its .env file moves into the sealed secrets, so that it is not shown.
// The stack counts as deployed: its containers are running from these files.
func (p *Panel) importStack(command *core.Record, _, path string, _ uint64) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	var imported struct {
		Compose    string `json:"compose_yaml"`
		Env        string `json:"env"`
		WorkingDir string `json:"working_dir"`
	}
	if err := json.Unmarshal(raw, &imported); err != nil {
		return "", err
	}

	rawPayload, err := json.Marshal(command.Get("payload"))
	if err != nil {
		return "", err
	}

	var payload struct {
		Project string `json:"project"`
	}
	if err := json.Unmarshal(rawPayload, &payload); err != nil {
		return "", err
	}

	collection, err := p.App.FindCachedCollectionByNameOrId(schema.Stacks)
	if err != nil {
		return "", err
	}

	plain, secrets := stacks.SplitSecrets(imported.Env, secretLikeKey)

	stack := core.NewRecord(collection)
	stack.Set("host", command.GetString("host"))
	stack.Set("name", payload.Project)
	stack.Set("compose_yaml", stacks.AbsoluteBinds(imported.Compose, imported.WorkingDir))
	stack.Set("env", plain)
	stack.Set("source", "import")
	stack.Set("status", stackImported)
	stack.Set(recordAuthor, command.GetString("requested_by"))
	stack.Set(recordNote, "Imported from the server")

	if len(secrets) > 0 {
		if err := p.applySecretChange(stack, &secretChange{Set: secrets}); err != nil {
			return "", err
		}
	}

	if err := p.App.Save(stack); err != nil {
		return "", err
	}

	stack.Set("deployed_revision", stack.GetInt("current_revision"))
	if err := p.App.Save(stack); err != nil {
		return "", err
	}

	return stack.Id, nil
}
