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
)

// registerMoves binds moving databases between servers: the passwords on
// delivery, what follows a move, and the removal of old containers.
func (p *Panel) registerMoves() {
	p.Commands.RegisterEnricher("db.server.move", p.enrichMove)
	p.Commands.OnFinish("db.server.move", p.moveFinished)
	p.App.Cron().MustAdd("pocketcloud_db_moves_cleanup", "50 3 * * *", p.removeOldServers)
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
	stop           []string
	dedicated      bool
	aliases        []string
	// leave is the application's network, which a shared source server leaves
	// when the application uses none of its databases any more
	leave string
}

// handleMoveServer moves the databases of a stack's own database server into a
// shared server, or one database of a shared server into another shared server,
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
			Source:           connectionOf(source),
			Target:           connectionOf(plan.target),
			Databases:        plan.items,
			StopContainerIds: plan.stop,
			Stack:            plan.stack.GetString("name"),
			StopSource:       plan.dedicated,
			DropFromSource:   !plan.dedicated,
			LeaveNetwork:     plan.leave,
		}}},
	})
	if err != nil {
		move.Set("status", schema.MoveFailed)
		move.Set("error", err.Error())
		p.saveAs(move)
		return e.InternalServerError("Failed to queue the move.", err)
	}

	move.Set("command", queued.Id)
	p.saveAs(move)

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

	engine, err := engineOfServer(source)
	if err != nil {
		return nil, err
	}

	// the same engine, nothing the target lacks, and not an older version
	candidate, found := databases.Recognize(source.GetString("image"))
	if !found {
		candidate = databases.Candidate{Engine: engine}
	}
	candidate.Version = versionOf(source)
	if !databases.CanMoveTo(candidate, target.GetString("engine"), versionOf(target)) {
		return nil, fmt.Errorf("%s cannot take these databases: it must run %s of the same or a newer major version, without extensions the source needs", target.GetString("name"), engine.Label)
	}

	if running, _ := p.App.CountRecords(schema.DBMoves, dbx.HashExp{"source_container": source.GetString("container_ref"), "status": schema.MoveRunning}); running > 0 {
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

	if running, _ := p.App.CountRecords(schema.DBMoves, dbx.HashExp{"stack": plan.stack.Id, "status": schema.MoveRunning}); running > 0 {
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

	// the application stops while its data moves
	containers, err := p.App.FindRecordsByFilter(schema.Containers, "host={:host} && project={:project} && state='running'", "", 0, 0,
		dbx.Params{"host": source.GetString("host"), "project": plan.stack.GetString("name")})
	if err != nil {
		return nil, err
	}
	for _, container := range containers {
		if plan.dedicated && container.GetString("name") == source.GetString("container_ref") {
			continue
		}
		plan.stop = append(plan.stop, container.GetString("docker_id"))
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

	for _, alias := range []string{service, source.GetString("container_ref")} {
		if alias != "" && alias != plan.target.GetString("name") && !slices.Contains(plan.aliases, alias) {
			plan.aliases = append(plan.aliases, alias)
		}
	}
	return nil
}

// planSharedMove moves one database of a shared server, with its user and its
// password, as for a new major version. Its stack's variables then name the
// new server.
func (p *Panel) planSharedMove(plan *movePlan, name string) error {
	database, err := p.databaseOf(plan.source, name)
	if err != nil {
		return errors.New("name the database to move")
	}
	if database.GetString("status") != dbReady {
		return fmt.Errorf("%s is not ready", name)
	}
	stack, err := p.App.FindRecordById(schema.Stacks, database.GetString("stack"))
	if err != nil {
		return fmt.Errorf("%s belongs to no stack: only the database of an application moves", name)
	}
	plan.stack = stack

	account, err := p.userOf(plan.source, name)
	if err != nil {
		return fmt.Errorf("%s has no user of PocketCloud", name)
	}
	password, err := p.sealed(account, "password")
	if err != nil || password == "" {
		return fmt.Errorf("the password of %s is not known", account.GetString("name"))
	}
	plan.passwords[account.GetString("name")] = password
	plan.items = []*agentv1.DbMoveItem{{Database: name, User: account.GetString("name")}}

	others, err := p.App.CountRecords(schema.Databases, dbx.And(
		dbx.HashExp{"server": plan.source.Id, "stack": stack.Id},
		dbx.Not(dbx.HashExp{"id": database.Id}),
	))
	if err != nil {
		return err
	}
	if others == 0 {
		plan.leave = databases.NetworkName(stack.GetString("name"))
	}
	return nil
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
		move.Set("status", schema.MoveFailed)
		move.Set("error", record.GetString("error"))
		move.Set("secrets", "")
		p.saveAs(move)
		return
	}

	if err := p.completeMove(move); err != nil {
		p.Log.Error("failed to record a move", "move", move.Id, "error", err)
		move.Set("status", schema.MoveFailed)
		move.Set("error", "The databases moved, but the panel could not switch the application over: "+err.Error())
		p.saveAs(move)
	}
}

// completeMove switches the panel's records and the application to the new server.
func (p *Panel) completeMove(move *core.Record) error {
	ctx := audit.WithActor(context.Background(), dbActor)

	target, err := p.App.FindRecordById(schema.DBServers, move.GetString("target"))
	if err != nil {
		return err
	}
	stack, err := p.App.FindRecordById(schema.Stacks, move.GetString("stack"))
	if err != nil {
		return err
	}
	source, sourceErr := p.serverByContainer(move.GetString("host"), move.GetString("source_container"))

	opened, err := p.sealed(move, "secrets")
	if err != nil {
		return err
	}
	passwords := map[string]string{}
	if err := json.Unmarshal([]byte(opened), &passwords); err != nil {
		return err
	}

	var items []movedItem
	if err := move.UnmarshalJSONField("databases", &items); err != nil {
		return err
	}

	dedicated := move.GetString("source_kind") == schema.DBServerDedicated
	network := databases.NetworkName(stack.GetString("name"))

	err = p.App.RunInTransaction(func(tx core.App) error {
		owned := map[string][]string{}
		for _, item := range items {
			name := item.Name
			var database *core.Record
			if sourceErr == nil {
				database, _ = tx.FindFirstRecordByFilter(schema.Databases, "server={:server} && name={:name}", dbx.Params{"server": source.Id, "name": name})
			}
			if database == nil {
				collection, err := tx.FindCachedCollectionByNameOrId(schema.Databases)
				if err != nil {
					return err
				}
				database = core.NewRecord(collection)
				database.Set("name", name)
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

		// the stack's network to the new server, under the old service's names too
		aliases := []string{}
		if dedicated {
			for _, alias := range []string{move.GetString("source_service"), move.GetString("source_container")} {
				if alias != "" && alias != target.GetString("name") && !slices.Contains(aliases, alias) {
					aliases = append(aliases, alias)
				}
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
		var have []string
		_ = link.UnmarshalJSONField("aliases", &have)
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
			// copies kept of the old server's databases stay, under the new one
			dumps, err := tx.FindRecordsByFilter(schema.DBDumps, "server={:server}", "", 0, 0, dbx.Params{"server": source.Id})
			if err != nil {
				return err
			}
			for _, dump := range dumps {
				dump.Set("server", target.Id)
				if err := tx.Save(dump); err != nil {
					return err
				}
			}

			if dedicated {
				// the stack's own server is gone from the panel; its container
				// stays stopped for a while, and its volume stays
				if err := tx.DeleteWithContext(ctx, source); err != nil {
					return err
				}
			} else if others, _ := tx.CountRecords(schema.Databases, dbx.HashExp{"server": source.Id, "stack": stack.Id}); others == 0 {
				// the application needs the old server no more
				if old, err := tx.FindFirstRecordByFilter(schema.DBNetworks, "server={:server} && stack={:stack}", dbx.Params{"server": source.Id, "stack": stack.Id}); err == nil {
					if err := tx.DeleteWithContext(ctx, old); err != nil {
						return err
					}
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
		return err
	}

	// a shared server's application names its server in variables the panel wrote
	if !dedicated && sourceErr == nil {
		engine, err := engineOfServer(target)
		if err != nil {
			return err
		}
		err = p.update(ctx, stack, func(fresh *core.Record) error {
			env := fresh.GetString("env")
			changed := false
			for _, item := range items {
				name := item.Name
				before := databases.Connection{Engine: engine, Server: source.GetString("name"), Database: name, User: name}
				after := databases.Connection{Engine: engine, Server: target.GetString("name"), Database: name, User: name}
				if cleaned, removed := databases.RemoveEnv(env, before.EnvLines()); removed {
					env, _ = databases.AddEnv(cleaned, after.EnvLines())
					changed = true
				}
			}
			if !changed {
				return errUnchanged
			}
			fresh.Set("env", env)
			fresh.Set(recordNote, "The database moved to "+target.GetString("name"))
			return nil
		})
		if err != nil {
			return err
		}
	}

	return p.redeploy(ctx, stack, move.GetString("requested_by"))
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
func (p *Panel) removeOldServers() {
	now := types.NowDateTime()
	moves, err := p.App.FindRecordsByFilter(schema.DBMoves, "status={:done} && source_kind={:dedicated} && remove_after!='' && remove_after<{:now} && removed_at=''", "", 0, 0,
		dbx.Params{"done": schema.MoveDone, "dedicated": schema.DBServerDedicated, "now": now.String()})
	if err != nil {
		p.Log.Error("failed to list moved database servers", "error", err)
		return
	}

	for _, move := range moves {
		if !p.Commands.Connected(move.GetString("host")) {
			continue
		}
		_, err := p.Commands.Create(context.Background(), commands.Options{
			Host:      move.GetString("host"),
			Label:     move.GetString("source_container"),
			Requester: panelRequester,
			Command: &agentv1.Command{Payload: &agentv1.Command_ContainerRemove{ContainerRemove: &agentv1.ContainerRemove{
				DockerId: move.GetString("source_container"),
			}}},
		})
		if err != nil {
			p.Log.Error("failed to remove an old database container", "move", move.Id, "error", err)
			continue
		}
		move.Set("removed_at", now)
		p.saveAs(move)
	}
}
