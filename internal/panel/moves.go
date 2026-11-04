package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

const (
	// artifactSQLMove is the copy of a database as it was when it moved
	artifactSQLMove = "sql_move"
	// moveKeepOld is how long a stack's old database container stays stopped
	// after its data moved, before the panel removes it; its volume stays.
	moveKeepOld = 7 * 24 * time.Hour
	// moveErrorMax is as much of an error as a move keeps: a failed restore can
	// say a lot more
	moveErrorMax = 2000
	// moveSettle is how long a finished move command may take to be recorded
	// before the panel counts its move as one that ended unrecorded
	moveSettle = time.Minute
)

// registerMoves binds moving databases between servers: the passwords on
// delivery, what follows a move, the removal of old containers, and the
// containers a move keeps stopped.
func (p *Panel) registerMoves() {
	p.Commands.RegisterEnricher("db.server.move", p.enrichMove)
	p.Commands.OnFinish("db.server.move", p.moveFinished)
	p.App.Cron().MustAdd("pocketcloud_db_moves_cleanup", "50 3 * * *", p.removeOldServers)
	p.App.OnRecordCreateRequest(schema.Commands).BindFunc(p.guardMovedContainers)
}

// retiredServices are the services of a stack whose database data moved to a
// shared server: their containers stay stopped for good.
func (p *Panel) retiredServices(stack *core.Record) ([]string, error) {
	moves, err := p.App.FindRecordsByFilter(schema.DBMoves, "stack={:stack} && status={:done} && source_service!=''", "", 0, 0,
		dbx.Params{"stack": stack.Id, "done": schema.MoveDone})
	if err != nil {
		return nil, err
	}
	retired := []string{}
	for _, move := range moves {
		if service := move.GetString("source_service"); !slices.Contains(retired, service) {
			retired = append(retired, service)
		}
	}
	return retired, nil
}

// clip keeps as much of an error as a move can hold.
func clip(text string) string {
	if len(text) <= moveErrorMax {
		return text
	}
	cut := moveErrorMax - len("…")
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

// openMoves are the moves under way that match a filter. A move whose command
// ended long enough ago without the panel recording it, as when the panel
// stopped in between, is marked failed instead, so that it holds nothing up.
func (p *Panel) openMoves(filter string, params dbx.Params) ([]*core.Record, error) {
	params["moving"] = schema.MoveRunning
	moves, err := p.App.FindRecordsByFilter(schema.DBMoves, "status={:moving} && ("+filter+")", "", 0, 0, params)
	if err != nil {
		return nil, err
	}
	open := []*core.Record{}
	settled := time.Now().Add(-moveSettle)
	for _, move := range moves {
		stale := false
		if id := move.GetString("command"); id == "" {
			stale = move.GetDateTime("created").Time().Before(settled)
		} else if command, err := p.App.FindRecordById(schema.Commands, id); err != nil {
			stale = true
		} else if finished := command.GetDateTime("finished_at"); !finished.IsZero() && finished.Time().Before(settled) {
			stale = true
		}
		if !stale {
			open = append(open, move)
			continue
		}
		p.setFields(move, map[string]any{
			"status":  schema.MoveFailed,
			"error":   "The move ended without the panel recording how: check both servers before moving again.",
			"secrets": "",
		})
	}
	return open, nil
}

// moveOfServer is a move under way into or out of a server, if any.
func (p *Panel) moveOfServer(server *core.Record) *core.Record {
	moves, err := p.openMoves("target={:server} || (host={:host} && source_container={:container})",
		dbx.Params{"server": server.Id, "host": server.GetString("host"), "container": server.GetString("container_ref")})
	if err != nil || len(moves) == 0 {
		return nil
	}
	return moves[0]
}

// moveOfStack is a move under way of a stack's databases, if any.
func (p *Panel) moveOfStack(hostID, stackName string) *core.Record {
	stack, err := p.App.FindFirstRecordByFilter(schema.Stacks, "host={:host} && name={:name}", dbx.Params{"host": hostID, "name": stackName})
	if err != nil {
		return nil
	}
	moves, err := p.openMoves("stack={:stack}", dbx.Params{"stack": stack.Id})
	if err != nil || len(moves) == 0 {
		return nil
	}
	return moves[0]
}

// serverDeploying tells whether a shared server's own stack is being deployed,
// as for an update, which recreates its container.
func (p *Panel) serverDeploying(server *core.Record) bool {
	if server.GetString("kind") != schema.DBServerShared {
		return false
	}
	count, err := p.App.CountRecords(schema.Commands, dbx.And(
		dbx.HashExp{"host": server.GetString("host"), "resource": "stack:" + server.GetString("name")},
		dbx.In("status", schema.CommandQueued, schema.CommandDispatched, schema.CommandRunning),
	))
	return err == nil && count > 0
}

// guardMovedContainers keeps the containers a move holds from being started: those
// of a stack whose databases are moving, and a stack's old database container,
// which stays stopped until the panel removes it.
func (p *Panel) guardMovedContainers(e *core.RecordRequestEvent) error {
	switch e.Record.GetString("type") {
	case "container.start", "container.restart":
	default:
		return e.Next()
	}
	var payload struct {
		DockerID string `json:"docker_id"`
	}
	if err := e.Record.UnmarshalJSONField("payload", &payload); err != nil || payload.DockerID == "" {
		return e.Next()
	}
	host := e.Record.GetString("host")
	container, err := p.App.FindFirstRecordByFilter(schema.Containers, "host={:host} && docker_id={:id}", dbx.Params{"host": host, "id": payload.DockerID})
	if err != nil {
		return e.Next()
	}

	name := container.GetString("name")
	if old, err := p.App.FindFirstRecordByFilter(schema.DBMoves, "host={:host} && source_container={:name} && status={:done} && source_kind={:dedicated} && removed_at=''",
		dbx.Params{"host": host, "name": name, "done": schema.MoveDone, "dedicated": schema.DBServerDedicated}); err == nil {
		return e.BadRequestError(fmt.Sprintf("The container %s held the data that moved to %s: it stays stopped until the panel removes it.", name, old.GetString("target_name")), nil)
	}
	if project := container.GetString("project"); project != "" {
		if move := p.moveOfStack(host, project); move != nil {
			return e.Error(http.StatusConflict, fmt.Sprintf("The databases of %s are moving to %s: its containers start again when that is done.", project, move.GetString("target_name")), nil)
		}
	}
	return e.Next()
}

// movedItem is a database of a move and the user that owns it.
type movedItem struct {
	Name string `json:"name"`
	User string `json:"user"`
}

// movePlan is what a move does, worked out before anything happens.
type movePlan struct {
	source, target *core.Record
	stack          *core.Record
	engine         databases.Engine
	items          []*agentv1.DbMoveItem
	passwords      map[string]string
	dedicated      bool
	// leave is the application's network, which a shared source server leaves
	// when the application uses none of its databases any more
	leave string
}

// handleMoveServer moves the databases of a stack's own database server into a
// shared server, or a database of a shared server into another shared server,
// as for a new major version. The application stops for the move and keeps its
// user and password; the panel switches it over and deploys it again.
func (p *Panel) handleMoveServer(e *core.RequestEvent) error {
	source, err := p.App.FindRecordById(schema.DBServers, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The database server does not exist.", err)
	}

	var body struct {
		Target   string `json:"target"`
		Database string `json:"database"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}

	plan, err := p.planMove(source, body.Target, body.Database)
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	if !p.Commands.Connected(source.GetString("host")) {
		return e.Error(http.StatusConflict, "The server is not connected.", nil)
	}

	ctx := e.Request.Context()
	actor := audit.ActorFromRequest(e)
	userID, label := requester(e)

	items := []movedItem{}
	for _, item := range plan.items {
		items = append(items, movedItem{Name: item.GetDatabase(), User: item.GetUser()})
	}
	secrets, err := json.Marshal(plan.passwords)
	if err != nil {
		return e.InternalServerError("", err)
	}

	collection, err := p.App.FindCachedCollectionByNameOrId(schema.DBMoves)
	if err != nil {
		return e.InternalServerError("", err)
	}
	move := core.NewRecord(collection)
	move.Set("host", source.GetString("host"))
	move.Set("stack", plan.stack.Id)
	move.Set("target", plan.target.Id)
	move.Set("target_name", plan.target.GetString("name"))
	move.Set("source_name", source.GetString("name"))
	move.Set("source_kind", source.GetString("kind"))
	move.Set("source_container", source.GetString("container_ref"))
	move.Set("source_version", source.GetString("version"))
	if plan.dedicated {
		move.Set("source_service", source.GetString("service"))
		move.Set("source_volume", source.GetString("volume"))
	}
	move.Set("databases", items)
	move.Set("secrets", string(secrets))
	move.Set("status", schema.MoveRunning)
	if userID != "" {
		move.Set("requested_by", userID)
	}
	if err := p.App.SaveWithContext(audit.WithActor(ctx, actor), move); err != nil {
		return e.InternalServerError("Failed to record the move.", err)
	}

	queued, err := p.Commands.Create(ctx, commands.Options{
		Host:        source.GetString("host"),
		Label:       fmt.Sprintf("%s → %s", source.GetString("name"), plan.target.GetString("name")),
		RequestedBy: userID,
		Requester:   label,
		Command: &agentv1.Command{Payload: &agentv1.Command_DbServerMove{DbServerMove: &agentv1.DbServerMove{
			Source:         connectionOf(source),
			Target:         connectionOf(plan.target),
			Databases:      plan.items,
			Stack:          plan.stack.GetString("name"),
			StopSource:     plan.dedicated,
			DropFromSource: !plan.dedicated,
			LeaveNetwork:   plan.leave,
		}}},
	})
	if err != nil {
		p.setFields(move, map[string]any{"status": schema.MoveFailed, "error": clip(err.Error()), "secrets": ""})
		return e.InternalServerError("Failed to queue the move.", err)
	}

	// only the command: the move may have failed already, and that stays
	p.setFields(move, map[string]any{"command": queued.Id})

	return e.JSON(http.StatusOK, map[string]any{"move": move, "command": queued})
}

// planMove checks a move and works out what it does.
func (p *Panel) planMove(source *core.Record, targetID, database string) (*movePlan, error) {
	target, err := p.App.FindRecordById(schema.DBServers, targetID)
	if err != nil {
		return nil, errors.New("choose a shared server to move to")
	}
	if target.Id == source.Id {
		return nil, errors.New("a server cannot move into itself")
	}
	if target.GetString("kind") != schema.DBServerShared || target.GetString("host") != source.GetString("host") {
		return nil, errors.New("databases move only into a shared server on the same server")
	}
	if target.GetString("status") != dbReady || source.GetString("status") != dbReady {
		return nil, errors.New("both database servers must be ready")
	}
	if source.GetString("admin_password") == "" || target.GetString("admin_password") == "" {
		return nil, errors.New("the panel does not know the credentials of both servers")
	}
	for _, server := range []*core.Record{source, target} {
		if p.serverDeploying(server) {
			return nil, fmt.Errorf("%s is being deployed or updated: move once that is done", server.GetString("name"))
		}
	}

	engine, err := engineOfServer(source)
	if err != nil {
		return nil, err
	}

	// the same engine, nothing the target lacks, and not an older series
	candidate, found := databases.Recognize(source.GetString("image"))
	if !found {
		candidate = databases.Candidate{Engine: engine}
	}
	candidate.Version = versionOf(source)
	if !databases.CanMoveTo(candidate, target.GetString("engine"), versionOf(target)) {
		return nil, fmt.Errorf("%s cannot take these databases: it must run %s of the same or a newer version, without extensions the source needs", target.GetString("name"), engine.Label)
	}

	if moves, _ := p.openMoves("host={:host} && source_container={:container}", dbx.Params{"host": source.GetString("host"), "container": source.GetString("container_ref")}); len(moves) > 0 {
		return nil, errors.New("a move of this server is already under way")
	}

	plan := &movePlan{source: source, target: target, engine: engine, passwords: map[string]string{}}
	if source.GetString("kind") == schema.DBServerDedicated {
		err = p.planDedicatedMove(plan)
	} else {
		err = p.planSharedMove(plan, database)
	}
	if err != nil {
		return nil, err
	}

	if moves, _ := p.openMoves("stack={:stack}", dbx.Params{"stack": plan.stack.Id}); len(moves) > 0 {
		return nil, fmt.Errorf("a move of the databases of %s is already under way", plan.stack.GetString("name"))
	}

	// nothing of the same name may wait on the target
	for _, item := range plan.items {
		if _, err := p.databaseOf(target, item.GetDatabase()); err == nil {
			return nil, fmt.Errorf("%s already has a database named %s", target.GetString("name"), item.GetDatabase())
		}
		if _, err := p.userOf(target, item.GetUser()); err == nil {
			return nil, fmt.Errorf("%s already has a user named %s", target.GetString("name"), item.GetUser())
		}
	}

	return plan, nil
}

// planDedicatedMove moves every database of a stack's own server. The shared
// server answers to the old service's names on the stack's network, so the
// application keeps its whole configuration, user and password included.
func (p *Panel) planDedicatedMove(plan *movePlan) error {
	source := plan.source
	plan.dedicated = true

	stack, err := p.App.FindRecordById(schema.Stacks, source.GetString("stack"))
	if err != nil {
		return errors.New("the server belongs to no stack of PocketCloud")
	}
	plan.stack = stack

	service := source.GetString("service")
	compose := stack.GetString("compose_yaml")
	if service == "" {
		return errors.New("the panel does not know which service of the stack the server is")
	}
	if err := databases.RetireProblem(compose, service); err != nil {
		return fmt.Errorf("%s cannot stay stopped while the rest of the stack runs: %w", service, err)
	}

	// other applications reach it on networks the move would take away
	if others, _ := p.App.CountRecords(schema.DBNetworks, dbx.And(dbx.HashExp{"server": source.Id}, dbx.Not(dbx.HashExp{"stack": stack.Id}))); others > 0 {
		return errors.New("other applications use databases of this server: move or disconnect them first")
	}

	login := databases.ApplicationCredentials(compose, service, plan.engine)
	switch login.User {
	case "":
		return fmt.Errorf("the stack does not say which user its application signs in with on %s", service)
	case plan.engine.AdminUser:
		return fmt.Errorf("the application signs in as %s, the administrator, which a shared server keeps for itself: give the application a user of its own first", login.User)
	}

	password := login.Password
	if login.PasswordVar != "" {
		secrets, err := p.openStackSecrets(stack)
		if err != nil {
			return err
		}
		password = secrets[login.PasswordVar]
		if password == "" {
			password = envValue(stack.GetString("env"), login.PasswordVar)
		}
	}
	if password == "" {
		return fmt.Errorf("the password of %s cannot be found in the stack", login.User)
	}

	// the agent checks that these are all the server has
	list, err := p.App.FindRecordsByFilter(schema.Databases, "server={:server}", "name", 0, 0, dbx.Params{"server": source.Id})
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return errors.New("the panel knows no database of this server yet: refresh its numbers first")
	}
	for _, database := range list {
		// a database the panel created here keeps its own user; the rest belong
		// to the application's
		user, secret := login.User, password
		if account, err := p.userOf(source, database.GetString("name")); err == nil {
			if own, err := p.sealed(account, "password"); err == nil && own != "" {
				user, secret = account.GetString("name"), own
			}
		}
		plan.passwords[user] = secret
		plan.items = append(plan.items, &agentv1.DbMoveItem{Database: database.GetString("name"), User: user})
	}
	return nil
}

// planSharedMove moves a database of a shared server, with its user and its
// password, as for a new major version. An application that names the server in
// the variables the panel wrote gets them switched. One that reaches it under the
// name of its old database service, as after a stack's own server moved in,
// keeps doing so: then all its databases there move together, as that name can
// lead to one server only.
func (p *Panel) planSharedMove(plan *movePlan, name string) error {
	database, err := p.databaseOf(plan.source, name)
	if err != nil {
		return errors.New("name the database to move")
	}
	stack, err := p.App.FindRecordById(schema.Stacks, database.GetString("stack"))
	if err != nil {
		return fmt.Errorf("%s belongs to no stack: only the database of an application moves", name)
	}
	plan.stack = stack

	var aliases []string
	if link, err := p.App.FindFirstRecordByFilter(schema.DBNetworks, "server={:server} && stack={:stack}",
		dbx.Params{"server": plan.source.Id, "stack": stack.Id}); err == nil {
		aliases = aliasesOf(link)
	}

	moving := []*core.Record{database}
	if len(aliases) > 0 {
		moving, err = p.App.FindRecordsByFilter(schema.Databases, "server={:server} && stack={:stack}", "name", 0, 0,
			dbx.Params{"server": plan.source.Id, "stack": stack.Id})
		if err != nil {
			return err
		}
	}

	for _, item := range moving {
		if item.GetString("status") != dbReady {
			return fmt.Errorf("%s is not ready", item.GetString("name"))
		}
		account, err := p.ownerOf(plan.source, item)
		if err != nil {
			return fmt.Errorf("%s has no user the panel knows", item.GetString("name"))
		}
		password, err := p.sealed(account, "password")
		if err != nil || password == "" {
			return fmt.Errorf("the password of %s is not known", account.GetString("name"))
		}
		user := account.GetString("name")
		plan.passwords[user] = password
		plan.items = append(plan.items, &agentv1.DbMoveItem{Database: item.GetString("name"), User: user})

		// without the old names the variables the panel wrote must all be there,
		// or the application would go on looking for the old server
		if len(aliases) == 0 {
			before := databases.Connection{Engine: plan.engine, Server: plan.source.GetString("name"), Database: item.GetString("name"), User: user}
			if !databases.HasEnv(stack.GetString("env"), before.EnvLines()) {
				return fmt.Errorf("the variables of %s for %s are not all as the panel wrote them, so the panel cannot switch them to %s: put them back, or move the database by hand", stack.GetString("name"), item.GetString("name"), plan.target.GetString("name"))
			}
		}
	}

	others, err := p.App.CountRecords(schema.Databases, dbx.And(
		dbx.HashExp{"server": plan.source.Id, "stack": stack.Id},
		dbx.Not(dbx.In("id", recordIDs(moving)...)),
	))
	if err != nil {
		return err
	}
	if others == 0 {
		plan.leave = databases.NetworkName(stack.GetString("name"))
	}
	return nil
}

// ownerOf is the user of a database on a server: the one named after it, as the
// panel creates them, or the one that owns it, as after a stack's own server
// moved in with the application's user.
func (p *Panel) ownerOf(server, database *core.Record) (*core.Record, error) {
	if account, err := p.userOf(server, database.GetString("name")); err == nil {
		return account, nil
	}
	return p.App.FindFirstRecordByFilter(schema.DBUsers, "server={:server} && databases~{:database}",
		dbx.Params{"server": server.Id, "database": database.Id})
}

// moveNames tells whether a move takes a database of a name.
func moveNames(move *core.Record, name string) bool {
	var items []movedItem
	if err := move.UnmarshalJSONField("databases", &items); err != nil {
		return false
	}
	return slices.ContainsFunc(items, func(item movedItem) bool { return item.Name == name })
}

func recordIDs(records []*core.Record) []any {
	ids := make([]any, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.Id)
	}
	return ids
}

// enrichMove adds the passwords right before the move is delivered: both
// administrators', and those the moved databases keep.
func (p *Panel) enrichMove(record *core.Record, command *agentv1.Command) error {
	payload := command.GetDbServerMove()
	host := record.GetString("host")

	for _, connection := range []*agentv1.DbConnection{payload.GetSource(), payload.GetTarget()} {
		server, err := p.serverByContainer(host, connection.GetContainerId())
		if err != nil {
			return fmt.Errorf("the database server %s is no longer known: %w", connection.GetContainerId(), err)
		}
		admin, err := p.sealed(server, "admin_password")
		if err != nil {
			return err
		}
		connection.AdminPassword = admin
	}

	move, err := p.runningMove(host, payload.GetStack())
	if err != nil {
		return err
	}
	opened, err := p.sealed(move, "secrets")
	if err != nil {
		return err
	}
	passwords := map[string]string{}
	if err := json.Unmarshal([]byte(opened), &passwords); err != nil {
		return err
	}
	for _, item := range payload.GetDatabases() {
		item.Password = passwords[item.GetUser()]
		if item.Password == "" {
			return fmt.Errorf("the password of %s is not known", item.GetUser())
		}
	}
	return nil
}

func (p *Panel) runningMove(hostID, stackName string) (*core.Record, error) {
	stack, err := p.App.FindFirstRecordByFilter(schema.Stacks, "host={:host} && name={:name}", dbx.Params{"host": hostID, "name": stackName})
	if err != nil {
		return nil, fmt.Errorf("the stack %s is no longer known: %w", stackName, err)
	}
	return p.App.FindFirstRecordByFilter(schema.DBMoves, "stack={:stack} && status={:running}", dbx.Params{"stack": stack.Id, "running": schema.MoveRunning})
}

// moveFinished records where the databases live now, puts the application on the
// new server and deploys it.
func (p *Panel) moveFinished(record *core.Record) {
	var payload struct {
		Stack  string            `json:"stack"`
		Source commandConnection `json:"source"`
		Target commandConnection `json:"target"`
	}
	if payloadInto(record, &payload) != nil {
		return
	}

	move, err := p.App.FindFirstRecordByFilter(schema.DBMoves, "command={:command}", dbx.Params{"command": record.Id})
	if err != nil {
		if move, err = p.runningMove(record.GetString("host"), payload.Stack); err != nil {
			p.Log.Error("a move finished that the panel has no record of", "command", record.Id)
			return
		}
	}

	if record.GetString("status") != schema.CommandSucceeded {
		p.setFields(move, map[string]any{"status": schema.MoveFailed, "error": clip(record.GetString("error")), "secrets": "", "command": record.Id})
		return
	}

	var result struct {
		AppStopped int `json:"app_stopped"`
	}
	_ = record.UnmarshalJSONField("result", &result)

	switched, err := p.completeMove(move, result.AppStopped > 0)
	switch {
	case err == nil:
	case switched:
		// the databases and the records moved; only the deploy did not happen
		p.Log.Error("failed to deploy a stack after its databases moved", "move", move.Id, "error", err)
		p.setFields(move, map[string]any{"error": clip("The databases moved, but the stack could not be deployed again: " + err.Error())})
	default:
		p.Log.Error("failed to record a move", "move", move.Id, "error", err)
		p.setFields(move, map[string]any{"status": schema.MoveFailed, "error": clip("The databases moved, but the panel could not switch the application over: " + err.Error())})
	}
}

// completeMove switches the panel's records and the application to the new
// server, and tells whether it got that far, as the deploy that follows can fail
// on its own. A stack that ran before the move is deployed again.
func (p *Panel) completeMove(move *core.Record, wasRunning bool) (bool, error) {
	ctx := audit.WithActor(context.Background(), dbActor)

	target, err := p.App.FindRecordById(schema.DBServers, move.GetString("target"))
	if err != nil {
		return false, err
	}
	stack, err := p.App.FindRecordById(schema.Stacks, move.GetString("stack"))
	if err != nil {
		return false, err
	}
	source, sourceErr := p.serverByContainer(move.GetString("host"), move.GetString("source_container"))

	opened, err := p.sealed(move, "secrets")
	if err != nil {
		return false, err
	}
	passwords := map[string]string{}
	if err := json.Unmarshal([]byte(opened), &passwords); err != nil {
		return false, err
	}

	var items []movedItem
	if err := move.UnmarshalJSONField("databases", &items); err != nil {
		return false, err
	}

	dedicated := move.GetString("source_kind") == schema.DBServerDedicated
	network := databases.NetworkName(stack.GetString("name"))
	engine, err := engineOfServer(target)
	if err != nil {
		return false, err
	}

	err = p.App.RunInTransaction(func(tx core.App) error {
		owned := map[string][]string{}
		for _, item := range items {
			var database *core.Record
			if sourceErr == nil {
				database, _ = tx.FindFirstRecordByFilter(schema.Databases, "server={:server} && name={:name}", dbx.Params{"server": source.Id, "name": item.Name})
			}
			if database == nil {
				collection, err := tx.FindCachedCollectionByNameOrId(schema.Databases)
				if err != nil {
					return err
				}
				database = core.NewRecord(collection)
				database.Set("name", item.Name)
			}
			database.Set("server", target.Id)
			database.Set("owner_kind", "stack")
			database.Set("stack", stack.Id)
			database.Set("status", dbReady)
			database.Set("last_error", "")
			if err := tx.SaveWithContext(ctx, database); err != nil {
				return err
			}
			owned[item.User] = append(owned[item.User], database.Id)
		}

		// the users: one the panel created moves with its password and secret;
		// the application's own keeps the password it has in its own settings,
		// which the panel therefore does not change
		for user, ids := range owned {
			var account *core.Record
			if sourceErr == nil {
				account, _ = tx.FindFirstRecordByFilter(schema.DBUsers, "server={:server} && name={:name}", dbx.Params{"server": source.Id, "name": user})
			}
			if account == nil {
				collection, err := tx.FindCachedCollectionByNameOrId(schema.DBUsers)
				if err != nil {
					return err
				}
				account = core.NewRecord(collection)
				account.Set("name", user)
				account.Set("password", passwords[user])
				account.Set("app_password", true)
			}
			account.Set("server", target.Id)
			account.Set("databases", ids)
			if err := tx.SaveWithContext(ctx, account); err != nil {
				return err
			}
		}

		// the names the application reaches its databases under come along: those
		// of its own old database service, or those it had on the old server
		aliases := []string{}
		var oldLink *core.Record
		if sourceErr == nil {
			oldLink, _ = tx.FindFirstRecordByFilter(schema.DBNetworks, "server={:server} && stack={:stack}", dbx.Params{"server": source.Id, "stack": stack.Id})
		}
		candidates := []string{move.GetString("source_service"), move.GetString("source_container")}
		if !dedicated && oldLink != nil {
			candidates = aliasesOf(oldLink)
		}
		for _, alias := range candidates {
			if alias != "" && alias != target.GetString("name") && !slices.Contains(aliases, alias) {
				aliases = append(aliases, alias)
			}
		}
		link, err := tx.FindFirstRecordByFilter(schema.DBNetworks, "server={:server} && stack={:stack}", dbx.Params{"server": target.Id, "stack": stack.Id})
		if err != nil {
			collection, err := tx.FindCachedCollectionByNameOrId(schema.DBNetworks)
			if err != nil {
				return err
			}
			link = core.NewRecord(collection)
			link.Set("server", target.Id)
			link.Set("stack", stack.Id)
			link.Set("name", network)
		}
		have := aliasesOf(link)
		for _, alias := range aliases {
			if !slices.Contains(have, alias) {
				have = append(have, alias)
			}
		}
		link.Set("aliases", have)
		if err := tx.SaveWithContext(ctx, link); err != nil {
			return err
		}

		if sourceErr == nil {
			// copies kept of the moved databases stay, under their new server;
			// those of other applications stay where they are
			for _, item := range items {
				dumps, err := tx.FindRecordsByFilter(schema.DBDumps, "server={:server} && database_name={:name}", "", 0, 0,
					dbx.Params{"server": source.Id, "name": item.Name})
				if err != nil {
					return err
				}
				for _, dump := range dumps {
					dump.Set("server", target.Id)
					if err := tx.Save(dump); err != nil {
						return err
					}
				}
			}

			if dedicated {
				// the stack's own server is gone from the panel; its container
				// stays stopped for a while, and its volume stays
				if err := tx.DeleteWithContext(ctx, source); err != nil {
					return err
				}
			} else if others, _ := tx.CountRecords(schema.Databases, dbx.HashExp{"server": source.Id, "stack": stack.Id}); others == 0 && oldLink != nil {
				// the application needs the old server no more
				if err := tx.DeleteWithContext(ctx, oldLink); err != nil {
					return err
				}
			}
		}

		// an application that names its server in the variables the panel wrote
		// gets them switched with the records, so that a move that says it is done
		// has switched the application over
		if !dedicated && sourceErr == nil {
			fresh, err := tx.FindRecordById(schema.Stacks, stack.Id)
			if err != nil {
				return err
			}
			env := fresh.GetString("env")
			changed := false
			for _, item := range items {
				before := databases.Connection{Engine: engine, Server: source.GetString("name"), Database: item.Name, User: item.User}
				after := databases.Connection{Engine: engine, Server: target.GetString("name"), Database: item.Name, User: item.User}
				if replaced, ok := databases.ReplaceEnv(env, before.EnvLines(), after.EnvLines()); ok {
					env, changed = replaced, true
				}
			}
			if changed {
				fresh.Set("env", env)
				fresh.Set(recordNote, "The database moved to "+target.GetString("name"))
				if err := tx.SaveWithContext(ctx, fresh); err != nil {
					return err
				}
			}
		}

		now := types.NowDateTime()
		move.Set("status", schema.MoveDone)
		move.Set("moved_at", now)
		move.Set("secrets", "")
		move.Set("error", "")
		if dedicated {
			move.Set("remove_after", now.Add(moveKeepOld))
		}
		return tx.SaveWithContext(ctx, move)
	})
	if err != nil {
		return false, err
	}

	// the deploy takes the stack as it is now, with a new revision if its
	// variables changed; a stack that was not running stays as it was, and its
	// next deploy keeps the old database stopped
	stack, err = p.App.FindRecordById(schema.Stacks, stack.Id)
	if err != nil {
		return true, err
	}
	if !wasRunning && !isLive(stack) {
		return true, nil
	}
	return true, p.redeploy(ctx, stack, move.GetString("requested_by"))
}

// storeMoveDump keeps the copy of a moved database, under its new server.
func (p *Panel) storeMoveDump(command *core.Record, fileName, path string, size uint64) (string, error) {
	var payload struct {
		Target commandConnection `json:"target"`
	}
	if err := payloadInto(command, &payload); err != nil {
		return "", err
	}
	server, err := p.serverByContainer(command.GetString("host"), payload.Target.ContainerID)
	if err != nil {
		return "", fmt.Errorf("the dump belongs to no known server: %w", err)
	}

	// the agent names the file <database>-move-<time>.sql.gz
	database, _, found := strings.Cut(fileName, "-move-")
	if !found {
		return "", fmt.Errorf("the dump %s names no database", fileName)
	}

	collection, err := p.App.FindCachedCollectionByNameOrId(schema.DBDumps)
	if err != nil {
		return "", err
	}
	file, err := filesystem.NewFileFromPath(path)
	if err != nil {
		return "", err
	}
	file.OriginalName = fileName
	expires, err := types.ParseDateTime(time.Now().UTC().Add(finalDumpLifetime))
	if err != nil {
		return "", err
	}

	dump := core.NewRecord(collection)
	dump.Set("server", server.Id)
	dump.Set("database_name", database)
	dump.Set("kind", schema.DumpMove)
	dump.Set("command", command.Id)
	dump.Set("requested_by", command.GetString("requested_by"))
	dump.Set("file", file)
	dump.Set("size_bytes", size)
	dump.Set("expires_at", expires)
	if err := p.App.Save(dump); err != nil {
		return "", err
	}
	return dump.Id, nil
}

// removeOldServers removes the stopped containers of stacks' own database servers
// once their week is over. Their volumes stay: removing data is a person's call.
// A move counts as cleaned up once its container is gone from the inventory, so
// a removal that fails is asked for again the next day.
func (p *Panel) removeOldServers() {
	now := types.NowDateTime()
	moves, err := p.App.FindRecordsByFilter(schema.DBMoves, "status={:done} && source_kind={:dedicated} && remove_after!='' && remove_after<{:now} && removed_at=''", "", 0, 0,
		dbx.Params{"done": schema.MoveDone, "dedicated": schema.DBServerDedicated, "now": now.String()})
	if err != nil {
		p.Log.Error("failed to list moved database servers", "error", err)
		return
	}

	for _, move := range moves {
		host, name := move.GetString("host"), move.GetString("source_container")
		if _, err := p.App.FindFirstRecordByFilter(schema.Containers, "host={:host} && name={:name}", dbx.Params{"host": host, "name": name}); err != nil {
			p.setFields(move, map[string]any{"removed_at": now})
			continue
		}
		if !p.Commands.Connected(host) {
			continue
		}
		_, err := p.Commands.Create(context.Background(), commands.Options{
			Host:      host,
			Label:     name,
			Requester: panelRequester,
			Command: &agentv1.Command{Payload: &agentv1.Command_ContainerRemove{ContainerRemove: &agentv1.ContainerRemove{
				DockerId: name,
			}}},
		})
		if err != nil {
			p.Log.Error("failed to remove an old database container", "move", move.Id, "error", err)
		}
	}
}
