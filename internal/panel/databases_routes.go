package panel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/stacks"
)

func (p *Panel) bindDatabaseRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	everyone := requireRole(schema.RoleViewer, schema.RoleOperator, schema.RoleAdmin)
	operators := requireRole(schema.RoleOperator, schema.RoleAdmin)
	admins := requireRole(schema.RoleAdmin)

	group.POST("/db-servers", p.handleCreateServer).BindFunc(operators)
	group.GET("/db-servers/candidates", p.handleServerCandidates).BindFunc(everyone)
	group.POST("/db-servers/link", p.handleLinkServer).BindFunc(operators)
	group.POST("/db-servers/{id}/stats", p.handleServerStats).BindFunc(operators)
	group.DELETE("/db-servers/{id}", p.handleRemoveServer).BindFunc(operators)
	group.POST("/db-servers/{id}/move", p.handleMoveServer).BindFunc(operators)
	group.POST("/db-servers/{id}/update", p.handleUpdateServer).BindFunc(operators)

	group.POST("/databases", p.handleCreateDatabase).BindFunc(operators)
	group.POST("/databases/{id}/connect", p.handleConnectDatabase).BindFunc(operators)
	group.POST("/databases/{id}/rotate", p.handleRotateDatabase).BindFunc(operators)
	group.POST("/databases/{id}/clone", p.handleCloneDatabase).BindFunc(operators)
	group.POST("/databases/{id}/export", p.handleExportDatabase).BindFunc(admins)
	group.DELETE("/databases/{id}", p.handleDropDatabase).BindFunc(operators)
	group.POST("/databases/{id}/adminer", p.handleAdminer).BindFunc(admins)
}

// handleCreateServer creates a shared server: a stack the panel generates, with the
// official image, a named volume and no published ports, and deploys it.
func (p *Panel) handleCreateServer(e *core.RequestEvent) error {
	var body struct {
		Host    string `json:"host"`
		Name    string `json:"name"`
		Engine  string `json:"engine"`
		Version string `json:"version"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}

	engine, ok := databases.EngineOf(body.Engine)
	if !ok {
		return e.BadRequestError("Choose MariaDB or PostgreSQL.", nil)
	}
	image, ok := engine.ImageFor(body.Version)
	if !ok {
		return e.BadRequestError(fmt.Sprintf("Choose %s %s.", engine.Label, strings.Join(engine.Versions, ", ")), nil)
	}
	if err := databases.ValidServerName(body.Name); err != nil {
		return e.BadRequestError(err.Error(), nil)
	}

	host, err := p.App.FindRecordById(schema.Hosts, body.Host)
	if err != nil {
		return e.NotFoundError("The server does not exist.", err)
	}

	taken, err := p.App.CountRecords(schema.Stacks, dbx.HashExp{"host": host.Id, "name": body.Name})
	if err != nil {
		return e.InternalServerError("", err)
	}
	if taken > 0 {
		return e.BadRequestError(fmt.Sprintf("The server %s already has a stack named %s.", host.GetString("name"), body.Name), nil)
	}

	compose := databases.ServerCompose(databases.Server{
		Name:           body.Name,
		Engine:         engine,
		Image:          image,
		Volume:         databases.VolumeName(body.Name),
		MaxConnections: engine.MaxConnections,
		Buffer:         databases.BufferSize(uint64(max(host.GetInt("mem_bytes"), 0))), //nolint:gosec // not negative
	})

	ctx := e.Request.Context()
	actor := audit.ActorFromRequest(e)
	actor.Note = "New shared " + engine.Label + " server"

	var stack, server *core.Record
	err = p.App.RunInTransaction(func(tx core.App) error {
		stacksCollection, err := tx.FindCachedCollectionByNameOrId(schema.Stacks)
		if err != nil {
			return err
		}
		stack = core.NewRecord(stacksCollection)
		stack.Set("host", host.Id)
		stack.Set("name", body.Name)
		stack.Set("compose_yaml", compose)
		stack.Set("env", databases.ServerEnv(body.Name))
		stack.Set("source", stackSourceDBServer)
		stack.Set("status", stackDraft)
		stack.Set(recordNote, actor.Note)
		if !e.HasSuperuserAuth() {
			stack.Set(recordAuthor, e.Auth.Id)
		}
		if err := tx.SaveWithContext(audit.WithActor(ctx, actor), stack); err != nil {
			return err
		}

		serversCollection, err := tx.FindCachedCollectionByNameOrId(schema.DBServers)
		if err != nil {
			return err
		}
		server = core.NewRecord(serversCollection)
		server.Set("host", host.Id)
		server.Set("name", body.Name)
		server.Set("engine", engine.Name)
		server.Set("kind", schema.DBServerShared)
		server.Set("image", image)
		server.Set("container_ref", serverContainer(body.Name))
		server.Set("service", databases.ServerService)
		server.Set("stack", stack.Id)
		server.Set("volume", databases.VolumeName(body.Name))
		server.Set("port", engine.Port)
		server.Set("admin_user", engine.AdminUser)
		server.Set("admin_password", databases.NewPassword())
		server.Set("max_connections", engine.MaxConnections)
		server.Set("status", dbCreating)
		return tx.SaveWithContext(audit.WithActor(ctx, actor), server)
	})
	if err != nil {
		return e.BadRequestError("The server could not be created: "+err.Error(), err)
	}

	revision, err := p.findRevision(stack, stack.GetInt("current_revision"))
	if err != nil {
		return e.InternalServerError("", err)
	}

	userID, label := requester(e)
	command, err := p.deployRevision(ctx, stack, revision, true, "", userID, label)
	if err != nil {
		return p.deployAnswer(e, nil, stack, err)
	}

	return e.JSON(http.StatusOK, map[string]any{"server": server, "stack": stack, "command": command})
}

// handleUpdateServer brings a shared server to the newest minor version of its
// series: its image is pulled again and its container recreated. MariaDB upgrades
// its system tables on the way; a new major version of PostgreSQL is a new server
// to move the databases to.
func (p *Panel) handleUpdateServer(e *core.RequestEvent) error {
	server, err := p.App.FindRecordById(schema.DBServers, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The database server does not exist.", err)
	}
	if server.GetString("kind") != schema.DBServerShared {
		return e.BadRequestError("A stack's own database server is updated with its stack.", nil)
	}
	if move := p.moveOfServer(server); move != nil {
		return e.Error(http.StatusConflict, "Databases are moving into or out of this server: update it once that is done.", nil)
	}
	if status := server.GetString("status"); status != dbReady && status != dbFailed {
		return e.BadRequestError("The server is busy.", nil)
	}
	stack, err := p.App.FindRecordById(schema.Stacks, server.GetString("stack"))
	if err != nil {
		return e.NotFoundError("The server's stack does not exist.", err)
	}
	if !p.Commands.Connected(server.GetString("host")) {
		return e.Error(http.StatusConflict, "The server is not connected.", nil)
	}

	revision, err := p.findRevision(stack, stack.GetInt("current_revision"))
	if err != nil {
		return e.InternalServerError("", err)
	}
	userID, label := requester(e)
	command, err := p.deployRevision(e.Request.Context(), stack, revision, true, "", userID, label)
	if err != nil {
		return p.deployAnswer(e, nil, stack, err)
	}
	return e.JSON(http.StatusOK, command)
}

// candidate is a database container inside a stack that no server record covers yet.
type candidate struct {
	Host       string   `json:"host"`
	HostName   string   `json:"host_name"`
	DockerID   string   `json:"docker_id"`
	Name       string   `json:"name"`
	Image      string   `json:"image"`
	Engine     string   `json:"engine"`
	Label      string   `json:"label"`
	Version    string   `json:"version"`
	Extensions []string `json:"extensions"`
	Stack      string   `json:"stack"`
	Project    string   `json:"project"`
	Service    string   `json:"service"`
	State      string   `json:"state"`
	MoveTo     string   `json:"move_to"`

	// what the stack's Compose file says about the administrator
	databases.Credentials
}

// handleServerCandidates lists the database containers found inside stacks: the
// dedicated servers, until someone links their credentials.
func (p *Panel) handleServerCandidates(e *core.RequestEvent) error {
	containers, err := p.App.FindRecordsByFilter(schema.Containers, "project!='' && system_role=''", "name", 0, 0)
	if err != nil {
		return e.InternalServerError("", err)
	}

	servers, err := p.App.FindRecordsByFilter(schema.DBServers, "", "", 0, 0)
	if err != nil {
		return e.InternalServerError("", err)
	}
	known := map[string]bool{}
	for _, server := range servers {
		known[server.GetString("host")+"/"+server.GetString("container_ref")] = true
	}
	// a stack's old database container whose data moved stays stopped until the
	// panel removes it: it is no server to link any more
	moved, err := p.App.FindRecordsByFilter(schema.DBMoves, "status={:done} && source_container!='' && removed_at=''", "", 0, 0,
		dbx.Params{"done": schema.MoveDone})
	if err != nil {
		return e.InternalServerError("", err)
	}
	for _, move := range moved {
		known[move.GetString("host")+"/"+move.GetString("source_container")] = true
	}

	hostNames := map[string]string{}
	list := []candidate{}

	for _, container := range containers {
		found, ok := databases.Recognize(container.GetString("image"))
		if !ok || known[container.GetString("host")+"/"+container.GetString("name")] {
			continue
		}

		hostID := container.GetString("host")
		if _, seen := hostNames[hostID]; !seen {
			if host, err := p.App.FindRecordById(schema.Hosts, hostID); err == nil {
				hostNames[hostID] = host.GetString("name")
			}
		}

		item := candidate{
			Host:       hostID,
			HostName:   hostNames[hostID],
			DockerID:   container.GetString("docker_id"),
			Name:       container.GetString("name"),
			Image:      container.GetString("image"),
			Engine:     found.Engine.Name,
			Label:      found.Label(),
			Version:    found.Version,
			Extensions: found.Extensions,
			Stack:      container.GetString("stack"),
			Project:    container.GetString("project"),
			Service:    container.GetString("service"),
			State:      container.GetString("state"),
		}
		if item.Extensions == nil {
			item.Extensions = []string{}
		}

		item.Credentials = databases.Credentials{User: found.Engine.AdminUser}
		if stack, err := p.App.FindRecordById(schema.Stacks, item.Stack); err == nil {
			item.Credentials = databases.SuggestCredentials(stack.GetString("compose_yaml"), item.Service, found.Engine)
		}

		for _, server := range servers {
			if server.GetString("host") == hostID && server.GetString("kind") == schema.DBServerShared &&
				databases.CanMoveTo(found, server.GetString("engine"), versionOf(server)) {
				item.MoveTo = server.GetString("name")
			}
		}

		list = append(list, item)
	}

	return e.JSON(http.StatusOK, list)
}

// versionOf is a server's version as it said it, or as its image says.
func versionOf(server *core.Record) string {
	if version := server.GetString("version"); version != "" {
		return version
	}
	if found, ok := databases.Recognize(server.GetString("image")); ok {
		return found.Version
	}
	return ""
}

// handleLinkServer makes a dedicated server known with its administrator's
// credentials, which usually come from its stack's own secrets.
func (p *Panel) handleLinkServer(e *core.RequestEvent) error {
	var body struct {
		Host      string `json:"host"`
		DockerID  string `json:"docker_id"`
		AdminUser string `json:"admin_user"`
		SecretKey string `json:"secret_key"`
		Password  string `json:"password"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}

	container, err := p.App.FindFirstRecordByFilter(schema.Containers, "host={:host} && docker_id={:id}",
		dbx.Params{"host": body.Host, "id": body.DockerID})
	if err != nil {
		return e.NotFoundError("This server has no such container.", err)
	}

	found, ok := databases.Recognize(container.GetString("image"))
	if !ok {
		return e.BadRequestError("This container is not a database server PocketCloud knows.", nil)
	}
	if body.AdminUser == "" {
		body.AdminUser = found.Engine.AdminUser
	}

	password := body.Password
	if body.SecretKey != "" {
		stack, err := p.App.FindRecordById(schema.Stacks, container.GetString("stack"))
		if err != nil {
			return e.BadRequestError("The container belongs to no stack of PocketCloud, so its secrets are not known: type the password.", nil)
		}
		secrets, err := p.openStackSecrets(stack)
		if err != nil {
			return e.InternalServerError("", err)
		}
		password = secrets[body.SecretKey]
		if password == "" {
			password = envValue(stack.GetString("env"), body.SecretKey)
		}
		if password == "" {
			return e.BadRequestError(fmt.Sprintf("The stack %s has no secret or variable %s.", stack.GetString("name"), body.SecretKey), nil)
		}
	}
	if password == "" {
		return e.BadRequestError("The administrator's password is needed to read the server's databases.", nil)
	}

	collection, err := p.App.FindCachedCollectionByNameOrId(schema.DBServers)
	if err != nil {
		return e.InternalServerError("", err)
	}

	server := core.NewRecord(collection)
	server.Set("host", container.GetString("host"))
	server.Set("name", container.GetString("name"))
	server.Set("engine", found.Engine.Name)
	server.Set("version", found.Version)
	server.Set("kind", schema.DBServerDedicated)
	server.Set("image", container.GetString("image"))
	server.Set("container_ref", container.GetString("name"))
	server.Set("service", container.GetString("service"))
	server.Set("stack", container.GetString("stack"))
	server.Set("port", found.Engine.Port)
	server.Set("admin_user", body.AdminUser)
	server.Set("admin_password", password)
	server.Set("extensions", found.Extensions)
	server.Set("status", dbReady)

	if err := p.saveChanges(audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e)), server); err != nil {
		return e.BadRequestError("The server could not be linked: "+err.Error(), err)
	}

	userID, _ := requester(e)
	if _, err := p.queueStats(e.Request.Context(), server, userID); err != nil && !errors.Is(err, errStatsRunning) {
		p.Log.Warn("failed to ask a linked server for its numbers", "server", server.Id, "error", err)
	}

	return e.JSON(http.StatusOK, server)
}

// envValue reads one plain variable of an env file. References to panel secrets
// are not values.
func envValue(env, key string) string {
	values, err := dotenv.UnmarshalWithLookup(stacks.SecretReference.ReplaceAllString(env, ""), nil)
	if err != nil {
		return ""
	}
	return values[key]
}

func (p *Panel) handleServerStats(e *core.RequestEvent) error {
	server, err := p.App.FindRecordById(schema.DBServers, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The database server does not exist.", err)
	}
	if server.GetString("admin_password") == "" {
		return e.BadRequestError("The panel does not know this server's credentials.", nil)
	}
	if !p.Commands.Connected(server.GetString("host")) {
		return e.Error(http.StatusConflict, "The server is not connected.", nil)
	}

	userID, _ := requester(e)
	command, err := p.queueStats(e.Request.Context(), server, userID)
	if err != nil && !errors.Is(err, errStatsRunning) {
		return e.InternalServerError("Failed to ask the server.", err)
	}

	return e.JSON(http.StatusOK, command)
}

// handleRemoveServer removes a server that holds no databases any more. A shared
// server's containers go with it; its data volume stays unless asked otherwise.
func (p *Panel) handleRemoveServer(e *core.RequestEvent) error {
	server, err := p.App.FindRecordById(schema.DBServers, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The database server does not exist.", err)
	}
	if move := p.moveOfServer(server); move != nil {
		return e.Error(http.StatusConflict, "Databases are moving into or out of this server.", nil)
	}

	count, err := p.App.CountRecords(schema.Databases, dbx.HashExp{"server": server.Id})
	if err != nil {
		return e.InternalServerError("", err)
	}
	if count > 0 && server.GetString("kind") == schema.DBServerShared {
		// PocketBase capitalises a message, so it does not start with a name
		held, them := fmt.Sprintf("%d databases", count), "them"
		if count == 1 {
			held, them = "a database", "it"
		}
		return e.BadRequestError(fmt.Sprintf("The database server %s still holds %s: drop %s first.", server.GetString("name"), held, them), nil)
	}

	removeVolume := e.Request.URL.Query().Get("remove_volume") == "true"
	ctx := audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e))

	if server.GetString("kind") == schema.DBServerShared {
		stack, err := p.App.FindRecordById(schema.Stacks, server.GetString("stack"))
		if err == nil {
			userID, label := requester(e)
			if _, err := p.Commands.Create(ctx, commands.Options{
				Host:        server.GetString("host"),
				Label:       server.GetString("name"),
				RequestedBy: userID,
				Requester:   label,
				Command: &agentv1.Command{Payload: &agentv1.Command_StackDown{StackDown: &agentv1.StackDown{
					Name:          stack.GetString("name"),
					RemoveVolumes: removeVolume,
				}}},
			}); err != nil {
				return e.InternalServerError("Failed to queue the removal.", err)
			}
			// the stack and everything that belongs to the server go with it
			if err := p.App.DeleteWithContext(ctx, stack); err != nil {
				return e.InternalServerError("", err)
			}
			return e.NoContent(http.StatusNoContent)
		}
	}

	if err := p.App.DeleteWithContext(ctx, server); err != nil {
		return e.InternalServerError("", err)
	}

	return e.NoContent(http.StatusNoContent)
}

// newDatabase creates the records of a database and its user, with a new password,
// and queues the command that creates them on the server.
func (p *Panel) newDatabase(ctx context.Context, e *core.RequestEvent, server *core.Record, name, stackID string, command func(user string) *agentv1.Command) (*core.Record, *core.Record, error) {
	engine, err := engineOfServer(server)
	if err != nil {
		return nil, nil, err
	}
	if err := databases.ValidDatabaseName(engine, name); err != nil {
		return nil, nil, err
	}
	if reason := p.claimedByMove(server, name, stackID); reason != "" {
		return nil, nil, errors.New(reason)
	}
	if server.GetString("admin_password") == "" {
		return nil, nil, errors.New("the panel does not know this server's credentials")
	}

	// a database's secret is found by its name on the host, so names do not repeat there
	secret := databases.SecretName(name)
	used, err := p.App.FindRecordsByFilter(schema.DBUsers, "secret_name={:secret} && server.host={:host}", "", 1, 0,
		dbx.Params{"secret": secret, "host": server.GetString("host")})
	if err != nil {
		return nil, nil, err
	}
	if len(used) > 0 {
		return nil, nil, fmt.Errorf("this server already has a database named %s", name)
	}

	actor := audit.ActorFromRequest(e)
	userID, label := requester(e)

	var database *core.Record
	err = p.App.RunInTransaction(func(tx core.App) error {
		collection, err := tx.FindCachedCollectionByNameOrId(schema.Databases)
		if err != nil {
			return err
		}
		database = core.NewRecord(collection)
		database.Set("server", server.Id)
		database.Set("name", name)
		database.Set("status", dbCreating)
		database.Set("owner_kind", "none")
		if stackID != "" {
			database.Set("owner_kind", "stack")
			database.Set("stack", stackID)
		}
		if err := tx.SaveWithContext(audit.WithActor(ctx, actor), database); err != nil {
			return err
		}

		users, err := tx.FindCachedCollectionByNameOrId(schema.DBUsers)
		if err != nil {
			return err
		}
		account := core.NewRecord(users)
		account.Set("server", server.Id)
		account.Set("name", name)
		account.Set("password", databases.NewPassword())
		account.Set("databases", []string{database.Id})
		account.Set("secret_name", secret)
		return tx.SaveWithContext(audit.WithActor(ctx, actor), account)
	})
	if err != nil {
		return nil, nil, err
	}

	queued, err := p.Commands.Create(ctx, commands.Options{
		Host:        server.GetString("host"),
		Label:       server.GetString("name") + "/" + name,
		RequestedBy: userID,
		Requester:   label,
		Command:     command(name),
	})
	if err != nil {
		return nil, nil, err
	}

	return database, queued, nil
}

func (p *Panel) handleCreateDatabase(e *core.RequestEvent) error {
	var body struct {
		Server string `json:"server"`
		Name   string `json:"name"`
		Stack  string `json:"stack"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}

	server, err := p.App.FindRecordById(schema.DBServers, body.Server)
	if err != nil {
		return e.NotFoundError("The database server does not exist.", err)
	}

	if body.Stack != "" {
		stack, err := p.App.FindRecordById(schema.Stacks, body.Stack)
		if err != nil {
			return e.NotFoundError("The stack does not exist.", err)
		}
		if stack.GetString("host") != server.GetString("host") {
			return e.BadRequestError("A stack can only use the database servers of its own server.", nil)
		}
	}

	database, command, err := p.newDatabase(e.Request.Context(), e, server, body.Name, body.Stack, func(user string) *agentv1.Command {
		return &agentv1.Command{Payload: &agentv1.Command_DbDatabaseCreate{DbDatabaseCreate: &agentv1.DbDatabaseCreate{
			Connection: connectionOf(server),
			Database:   body.Name,
			User:       user,
		}}}
	})
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}

	return e.JSON(http.StatusOK, map[string]any{"database": database, "command": command})
}

// databaseFor loads a database with its server, for the routes of one database.
func (p *Panel) databaseFor(e *core.RequestEvent) (database, server *core.Record, err error) {
	database, err = p.App.FindRecordById(schema.Databases, e.Request.PathValue("id"))
	if err != nil {
		return nil, nil, e.NotFoundError("The database does not exist.", err)
	}
	server, err = p.App.FindRecordById(schema.DBServers, database.GetString("server"))
	if err != nil {
		return nil, nil, e.NotFoundError("The database server does not exist.", err)
	}
	if database.GetString("status") == dbDropping {
		return nil, nil, e.BadRequestError("The database is being dropped.", nil)
	}
	return database, server, nil
}

// ownDatabaseFor is databaseFor for what changes a database or its user: only
// databases on shared servers, which the panel created, have users it knows.
func (p *Panel) ownDatabaseFor(e *core.RequestEvent) (database, server *core.Record, err error) {
	database, server, err = p.databaseFor(e)
	if err == nil && server.GetString("kind") == schema.DBServerDedicated {
		return nil, nil, e.BadRequestError("This database belongs to its stack, which manages its users.", nil)
	}
	// a database that is moving changes in no other way meanwhile, nor does one
	// whose application is stopped for a move, as a change would deploy it
	if err == nil {
		for _, move := range p.movesOfServer(server) {
			if moveNames(move, database.GetString("name")) {
				return nil, nil, e.Error(http.StatusConflict, "This database is moving to "+move.GetString("target_name")+".", nil)
			}
		}
		if stack, found := p.stackOf(database); found {
			if move := p.moveHolding(stack); move != nil {
				return nil, nil, e.Error(http.StatusConflict, "The databases of "+stack.GetString("name")+" are moving: change this one once that is done.", nil)
			}
		}
	}
	return database, server, err
}

func (p *Panel) handleConnectDatabase(e *core.RequestEvent) error {
	database, server, err := p.ownDatabaseFor(e)
	if err != nil {
		return err
	}

	var body struct {
		Stack string `json:"stack"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}

	stack, err := p.App.FindRecordById(schema.Stacks, body.Stack)
	if err != nil {
		return e.NotFoundError("The stack does not exist.", err)
	}
	if stack.GetString("source") == stackSourceDBServer {
		return e.BadRequestError("A database server cannot be an application.", nil)
	}
	if move := p.moveHolding(stack); move != nil {
		return e.Error(http.StatusConflict, "The databases of "+stack.GetString("name")+" are moving: connect this one once that is done.", nil)
	}
	if reason := p.aliasClash(stack.Id, server); reason != "" {
		return e.BadRequestError(strings.ToUpper(reason[:1])+reason[1:]+".", nil)
	}

	ctx := e.Request.Context()
	actor := audit.ActorFromRequest(e)

	database.Set("owner_kind", "stack")
	database.Set("stack", stack.Id)
	if err := p.saveChanges(audit.WithActor(ctx, actor), database); err != nil {
		return e.BadRequestError(err.Error(), nil)
	}

	if database.GetString("status") != dbReady {
		// the creation connects it when it finishes
		return e.JSON(http.StatusOK, database)
	}

	userID, _ := requester(e)
	if err := p.connectStack(ctx, actor, userID, server, database, stack); err != nil {
		return e.BadRequestError("The stack could not be connected: "+err.Error(), nil)
	}

	return e.JSON(http.StatusOK, database)
}

// handleRotateDatabase gives a database's user a new password. It becomes the
// password the panel hands out once the server has taken it; until then it waits.
func (p *Panel) handleRotateDatabase(e *core.RequestEvent) error {
	database, server, err := p.ownDatabaseFor(e)
	if err != nil {
		return err
	}

	account, err := p.userOf(server, database.GetString("name"))
	if err != nil {
		return e.NotFoundError("The database has no user.", err)
	}
	if account.GetBool("app_password") {
		return e.BadRequestError("The application keeps this password in its own settings: change it there and on the server together.", nil)
	}
	if account.GetString("pending_password") != "" {
		return e.BadRequestError("A rotation of this password is already under way.", nil)
	}

	ctx := e.Request.Context()
	account.Set("pending_password", databases.NewPassword())
	if err := p.saveChanges(audit.WithActor(ctx, audit.ActorFromRequest(e)), account); err != nil {
		return e.InternalServerError("", err)
	}

	userID, label := requester(e)
	command, err := p.Commands.Create(ctx, commands.Options{
		Host:        server.GetString("host"),
		Label:       server.GetString("name") + "/" + database.GetString("name"),
		RequestedBy: userID,
		Requester:   label,
		Command: &agentv1.Command{Payload: &agentv1.Command_DbDatabaseRotate{DbDatabaseRotate: &agentv1.DbDatabaseRotate{
			Connection: connectionOf(server),
			User:       account.GetString("name"),
		}}},
	})
	if err != nil {
		return e.InternalServerError("Failed to queue the rotation.", err)
	}

	return e.JSON(http.StatusOK, command)
}

func (p *Panel) handleCloneDatabase(e *core.RequestEvent) error {
	source, server, err := p.ownDatabaseFor(e)
	if err != nil {
		return err
	}

	var body struct {
		Name string `json:"name"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}

	database, command, err := p.newDatabase(e.Request.Context(), e, server, body.Name, "", func(user string) *agentv1.Command {
		return &agentv1.Command{Payload: &agentv1.Command_DbDatabaseClone{DbDatabaseClone: &agentv1.DbDatabaseClone{
			Connection: connectionOf(server),
			Source:     source.GetString("name"),
			Target:     body.Name,
			User:       user,
		}}}
	})
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}

	return e.JSON(http.StatusOK, map[string]any{"database": database, "command": command})
}

func (p *Panel) handleExportDatabase(e *core.RequestEvent) error {
	database, server, err := p.databaseFor(e)
	if err != nil {
		return err
	}

	userID, label := requester(e)
	command, err := p.Commands.Create(e.Request.Context(), commands.Options{
		Host:        server.GetString("host"),
		Label:       server.GetString("name") + "/" + database.GetString("name"),
		RequestedBy: userID,
		Requester:   label,
		Command: &agentv1.Command{Payload: &agentv1.Command_DbDatabaseExport{DbDatabaseExport: &agentv1.DbDatabaseExport{
			Connection: connectionOf(server),
			Database:   database.GetString("name"),
		}}},
	})
	if err != nil {
		return e.InternalServerError("Failed to queue the export.", err)
	}

	return e.JSON(http.StatusOK, command)
}

// handleDropDatabase drops a database and its user. The agent keeps a final dump
// in the panel first, and drops nothing if that fails.
func (p *Panel) handleDropDatabase(e *core.RequestEvent) error {
	database, server, err := p.ownDatabaseFor(e)
	if err != nil {
		return err
	}

	// the user goes with its database, unless it owns others too, as an
	// application's own user does when its databases moved in together
	user, keep := "", false
	if owner, err := p.ownerOf(server, database); err == nil {
		user = owner.GetString("name")
		keep = slices.ContainsFunc(owner.GetStringSlice("databases"), func(id string) bool { return id != database.Id })
	}

	ctx := e.Request.Context()
	database.Set("status", dbDropping)
	if err := p.saveChanges(audit.WithActor(ctx, audit.ActorFromRequest(e)), database); err != nil {
		return e.InternalServerError("", err)
	}

	userID, label := requester(e)
	command, err := p.Commands.Create(ctx, commands.Options{
		Host:        server.GetString("host"),
		Label:       server.GetString("name") + "/" + database.GetString("name"),
		RequestedBy: userID,
		Requester:   label,
		Command: &agentv1.Command{Payload: &agentv1.Command_DbDatabaseDrop{DbDatabaseDrop: &agentv1.DbDatabaseDrop{
			Connection: connectionOf(server),
			Database:   database.GetString("name"),
			User:       user,
			KeepUser:   keep,
		}}},
	})
	if err != nil {
		return e.InternalServerError("Failed to queue the drop.", err)
	}

	return e.JSON(http.StatusOK, command)
}
