package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/oldbear24/PocketServerManager/internal/stacks"
)

// States of a database server and of a database.
const (
	dbCreating = "creating"
	dbReady    = "ready"
	dbFailed   = "failed"
	dbDropping = "dropping"

	// stackSourceDBServer marks the stack of a shared server, which the panel generated.
	stackSourceDBServer = "db_server"

	// artifact kinds of the dumps agents upload
	artifactSQLExport = "sql_export"
	artifactSQLFinal  = "sql_final"

	// panelRequester is who asked for what the panel does by itself
	panelRequester = "PocketCloud"

	exportLifetime    = 7 * 24 * time.Hour
	finalDumpLifetime = 30 * 24 * time.Hour
	statsInterval     = time.Minute
)

var dbActor = audit.System("databases", "panel")

// registerDatabases binds what database commands need: their passwords on
// delivery, and what follows when they finish.
func (p *Panel) registerDatabases() {
	for _, commandType := range []string{
		"db.database.create", "db.database.rotate", "db.database.drop", "db.database.clone",
		"db.database.export", "db.stats", "db.adminer.start",
	} {
		p.Commands.RegisterEnricher(commandType, p.enrichDatabaseCommand)
	}

	p.Commands.OnFinish("db.database.create", p.databaseCreateFinished)
	p.Commands.OnFinish("db.database.clone", p.databaseCloneFinished)
	p.Commands.OnFinish("db.database.rotate", p.databaseRotateFinished)
	p.Commands.OnFinish("db.database.drop", p.databaseDropFinished)
	p.Commands.OnFinish("db.stats", p.databaseStatsFinished)
	p.Commands.OnFinish("stack.deploy", p.serverDeployFinished)

	// a shared server's stack goes with its server, not while databases live on it
	p.App.OnRecordDeleteRequest(schema.Stacks).BindFunc(func(e *core.RecordRequestEvent) error {
		if e.Record.GetString("source") == stackSourceDBServer {
			return e.BadRequestError("This stack is a database server: remove it on the Databases page.", nil)
		}
		return e.Next()
	})

	p.App.Cron().MustAdd("pocketcloud_db_stats", "17 * * * *", p.refreshAllStats)
	p.App.Cron().MustAdd("pocketcloud_db_dumps_retention", "40 3 * * *", p.pruneDumps)
}

// sealed opens a secret field of a record. An empty field is an empty secret.
func (p *Panel) sealed(record *core.Record, field string) (string, error) {
	value := record.GetString(field)
	if value == "" {
		return "", nil
	}
	if p.Box == nil {
		return "", errors.New("the master key is not loaded")
	}
	return p.Box.OpenString(value)
}

// serverContainer is the container of a shared server's database: Compose names it
// after the project and the service.
func serverContainer(server string) string {
	return server + "-" + databases.ServerService + "-1"
}

func (p *Panel) serverByContainer(hostID, container string) (*core.Record, error) {
	return p.App.FindFirstRecordByFilter(schema.DBServers, "host={:host} && container_ref={:container}",
		dbx.Params{"host": hostID, "container": container})
}

func engineOfServer(server *core.Record) (databases.Engine, error) {
	engine, ok := databases.EngineOf(server.GetString("engine"))
	if !ok {
		return databases.Engine{}, fmt.Errorf("%s has an unknown engine %q", server.GetString("name"), server.GetString("engine"))
	}
	return engine, nil
}

// connectionOf describes a server for a command, without its password: an enricher
// adds that on delivery.
func connectionOf(server *core.Record) *agentv1.DbConnection {
	return &agentv1.DbConnection{
		ContainerId: server.GetString("container_ref"),
		Engine:      server.GetString("engine"),
		AdminUser:   server.GetString("admin_user"),
	}
}

func (p *Panel) userOf(server *core.Record, name string) (*core.Record, error) {
	return p.App.FindFirstRecordByFilter(schema.DBUsers, "server={:server} && name={:name}",
		dbx.Params{"server": server.Id, "name": name})
}

func (p *Panel) databaseOf(server *core.Record, name string) (*core.Record, error) {
	return p.App.FindFirstRecordByFilter(schema.Databases, "server={:server} && name={:name}",
		dbx.Params{"server": server.Id, "name": name})
}

// secretResolver finds the secrets an env file refers to as `${secret:name}`. They
// belong to the host of the stack: `db/<database>` is the password of a database
// on one of its servers, and `db-admin/<server>` the administrator's password of
// a shared server, for that server's own stack only.
func (p *Panel) secretResolver(stack *core.Record) func(string) (string, error) {
	return func(name string) (string, error) {
		hostID := stack.GetString("host")

		if serverName, ok := strings.CutPrefix(name, "db-admin/"); ok {
			if stack.GetString("source") != stackSourceDBServer || stack.GetString("name") != serverName {
				return "", fmt.Errorf("%s: only the server's own stack can use it: %w", name, stacks.ErrUnknownSecret)
			}
			server, err := p.App.FindFirstRecordByFilter(schema.DBServers, "host={:host} && name={:name}",
				dbx.Params{"host": hostID, "name": serverName})
			if err != nil {
				return "", fmt.Errorf("%s: %w", name, stacks.ErrUnknownSecret)
			}
			return p.sealed(server, "admin_password")
		}

		if _, ok := strings.CutPrefix(name, "db/"); ok {
			users, err := p.App.FindRecordsByFilter(schema.DBUsers, "secret_name={:secret} && server.host={:host}", "", 1, 0,
				dbx.Params{"secret": name, "host": hostID})
			if err != nil || len(users) == 0 {
				return "", fmt.Errorf("%s: no database of this server has it: %w", name, stacks.ErrUnknownSecret)
			}
			password, err := p.sealed(users[0], "password")
			if err == nil && password == "" {
				err = fmt.Errorf("%s has no password yet: %w", name, stacks.ErrUnknownSecret)
			}
			return password, err
		}

		return "", fmt.Errorf("%s: %w", name, stacks.ErrUnknownSecret)
	}
}

// stackOverride generates the second Compose file of a deploy, which puts a stack on
// its database networks and keeps a database service stopped whose data moved to
// a shared server, and the networks the agent prepares for it. Most stacks have
// neither.
func (p *Panel) stackOverride(stack *core.Record, compose string) (string, []*agentv1.DbNetworkAttach, error) {
	// a shared server joins the networks of every application that uses it, under
	// its own name and those of the database services that moved to it
	if stack.GetString("source") == stackSourceDBServer {
		server, err := p.App.FindFirstRecordByFilter(schema.DBServers, "stack={:stack}", dbx.Params{"stack": stack.Id})
		if err != nil {
			return "", nil, nil //nolint:nilerr // a stack that is no longer a server
		}

		networks, err := p.App.FindRecordsByFilter(schema.DBNetworks, "server={:server}", "name", 0, 0, dbx.Params{"server": server.Id})
		if err != nil {
			return "", nil, err
		}

		joins := []databases.Join{}
		attach := []*agentv1.DbNetworkAttach{}
		for _, network := range networks {
			joins = append(joins, databases.Join{
				Network: network.GetString("name"),
				Aliases: append([]string{server.GetString("name")}, aliasesOf(network)...),
			})
			attach = append(attach, &agentv1.DbNetworkAttach{Network: network.GetString("name")})
		}

		override, err := databases.Override(compose, joins, databases.ServerService, nil)
		return override, attach, err
	}

	networks, err := p.App.FindRecordsByFilter(schema.DBNetworks, "stack={:stack}", "name", 0, 0, dbx.Params{"stack": stack.Id})
	if err != nil {
		return "", nil, err
	}
	retired, err := p.retiredServices(stack)
	if err != nil {
		return "", nil, err
	}
	if len(networks) == 0 && len(retired) == 0 {
		return "", nil, nil
	}

	joins := []databases.Join{}
	attach := []*agentv1.DbNetworkAttach{}
	for _, network := range networks {
		server, err := p.App.FindRecordById(schema.DBServers, network.GetString("server"))
		if err != nil {
			continue
		}
		attach = append(attach, &agentv1.DbNetworkAttach{
			Network:           network.GetString("name"),
			ServerContainerId: server.GetString("container_ref"),
			ServerAlias:       server.GetString("name"),
			ExtraAliases:      aliasesOf(network),
		})
		if !slices.ContainsFunc(joins, func(join databases.Join) bool { return join.Network == network.GetString("name") }) {
			joins = append(joins, databases.Join{Network: network.GetString("name")})
		}
	}

	override, err := databases.Override(compose, joins, "", retired)
	return override, attach, err
}

// aliasesOf reads the extra names a server answers to on a database network.
func aliasesOf(network *core.Record) []string {
	var aliases []string
	if err := network.UnmarshalJSONField("aliases", &aliases); err != nil {
		return nil
	}
	return aliases
}

// enrichDatabaseCommand adds the passwords to a database command right before it
// is delivered. The stored payload names the server's container and the users;
// the passwords stay sealed in the panel until then.
func (p *Panel) enrichDatabaseCommand(record *core.Record, command *agentv1.Command) error {
	var connection *agentv1.DbConnection
	var user string
	var setPassword func(string)
	pending := false

	switch payload := command.GetPayload().(type) {
	case *agentv1.Command_DbDatabaseCreate:
		connection, user = payload.DbDatabaseCreate.GetConnection(), payload.DbDatabaseCreate.GetUser()
		setPassword = func(password string) { payload.DbDatabaseCreate.Password = password }
	case *agentv1.Command_DbDatabaseRotate:
		connection, user, pending = payload.DbDatabaseRotate.GetConnection(), payload.DbDatabaseRotate.GetUser(), true
		setPassword = func(password string) { payload.DbDatabaseRotate.Password = password }
	case *agentv1.Command_DbDatabaseClone:
		connection, user = payload.DbDatabaseClone.GetConnection(), payload.DbDatabaseClone.GetUser()
		setPassword = func(password string) { payload.DbDatabaseClone.Password = password }
	case *agentv1.Command_DbAdminerStart:
		connection, user = payload.DbAdminerStart.GetConnection(), payload.DbAdminerStart.GetUser()
		setPassword = func(password string) { payload.DbAdminerStart.Password = password }
	case *agentv1.Command_DbDatabaseDrop:
		connection = payload.DbDatabaseDrop.GetConnection()
	case *agentv1.Command_DbDatabaseExport:
		connection = payload.DbDatabaseExport.GetConnection()
	case *agentv1.Command_DbStats:
		connection = payload.DbStats.GetConnection()
	}
	if connection == nil {
		return errors.New("the command names no database server")
	}

	server, err := p.serverByContainer(record.GetString("host"), connection.GetContainerId())
	if err != nil {
		return fmt.Errorf("the database server %s is no longer known: %w", connection.GetContainerId(), err)
	}

	admin, err := p.sealed(server, "admin_password")
	if err != nil {
		return err
	}
	if admin == "" {
		return fmt.Errorf("the panel does not know the administrator's password of %s", server.GetString("name"))
	}
	connection.AdminPassword = admin

	if setPassword == nil || user == "" {
		return nil
	}

	account, err := p.userOf(server, user)
	if err != nil {
		return fmt.Errorf("the database user %s is no longer known: %w", user, err)
	}

	field := "password"
	if pending {
		field = "pending_password"
	}
	password, err := p.sealed(account, field)
	if err != nil {
		return err
	}
	if password == "" {
		return fmt.Errorf("the database user %s has no %s", user, strings.ReplaceAll(field, "_", " "))
	}
	setPassword(password)

	return nil
}

// payloadInto reads a command's stored payload.
func payloadInto(record *core.Record, into any) error {
	raw, err := json.Marshal(record.Get("payload"))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

type commandConnection struct {
	ContainerID string `json:"container_id"`
}

// targetOf finds the server and the database a finished command was about.
func (p *Panel) targetOf(record *core.Record, container, name string) (server, database *core.Record, err error) {
	server, err = p.serverByContainer(record.GetString("host"), container)
	if err != nil {
		return nil, nil, err
	}
	database, err = p.databaseOf(server, name)
	return server, database, err
}

func (p *Panel) saveAs(record *core.Record) {
	if err := p.saveChanges(audit.WithActor(context.Background(), dbActor), record); err != nil {
		p.Log.Error("failed to save a database record", "collection", record.Collection().Name, "id", record.Id, "error", err)
	}
}

func (p *Panel) databaseCreateFinished(record *core.Record) {
	var payload struct {
		Connection commandConnection `json:"connection"`
		Database   string            `json:"database"`
	}
	if payloadInto(record, &payload) != nil {
		return
	}

	server, database, err := p.targetOf(record, payload.Connection.ContainerID, payload.Database)
	if err != nil {
		return
	}

	if record.GetString("status") != schema.CommandSucceeded {
		database.Set("status", dbFailed)
		database.Set("last_error", record.GetString("error"))
		p.saveAs(database)
		return
	}

	// a database for a stack is ready once the stack is connected to it
	database.Set("last_error", "")
	if stackID := database.GetString("stack"); stackID != "" {
		if stack, err := p.App.FindRecordById(schema.Stacks, stackID); err == nil {
			if err := p.connectStack(context.Background(), dbActor, record.GetString("requested_by"), server, database, stack); err != nil {
				p.Log.Error("failed to connect a stack to its database", "database", database.Id, "error", err)
				database.Set("last_error", "The database is ready, but connecting "+stack.GetString("name")+" failed: "+err.Error())
			}
		}
	}

	database.Set("status", dbReady)
	p.saveAs(database)
}

func (p *Panel) databaseCloneFinished(record *core.Record) {
	var payload struct {
		Connection commandConnection `json:"connection"`
		Target     string            `json:"target"`
	}
	if payloadInto(record, &payload) != nil {
		return
	}

	_, database, err := p.targetOf(record, payload.Connection.ContainerID, payload.Target)
	if err != nil {
		return
	}

	if record.GetString("status") == schema.CommandSucceeded {
		database.Set("status", dbReady)
		database.Set("last_error", "")
	} else {
		database.Set("status", dbFailed)
		database.Set("last_error", record.GetString("error"))
	}
	p.saveAs(database)
}

// databaseRotateFinished makes the new password the one the panel hands out, once
// the server has taken it, and restarts the application with it.
func (p *Panel) databaseRotateFinished(record *core.Record) {
	var payload struct {
		Connection commandConnection `json:"connection"`
		User       string            `json:"user"`
	}
	if payloadInto(record, &payload) != nil {
		return
	}

	server, err := p.serverByContainer(record.GetString("host"), payload.Connection.ContainerID)
	if err != nil {
		return
	}
	account, err := p.userOf(server, payload.User)
	if err != nil {
		return
	}

	succeeded := record.GetString("status") == schema.CommandSucceeded
	if succeeded {
		// both are sealed with the same key: the sealed value moves as it is
		account.Set("password", account.GetString("pending_password"))
		account.Set("rotated_at", types.NowDateTime())
	}
	account.Set("pending_password", "")
	p.saveAs(account)

	database, err := p.databaseOf(server, payload.User)
	if err != nil {
		return
	}
	if !succeeded {
		database.Set("last_error", "The password was not changed: "+record.GetString("error"))
		p.saveAs(database)
		return
	}

	if stackID := database.GetString("stack"); stackID != "" {
		stack, err := p.App.FindRecordById(schema.Stacks, stackID)
		if err == nil && isLive(stack) {
			if err := p.redeploy(context.Background(), stack, record.GetString("requested_by")); err != nil {
				p.Log.Error("failed to restart a stack after a password rotation", "stack", stack.Id, "error", err)
			}
		}
	}
}

func (p *Panel) databaseDropFinished(record *core.Record) {
	var payload struct {
		Connection commandConnection `json:"connection"`
		Database   string            `json:"database"`
		User       string            `json:"user"`
	}
	if payloadInto(record, &payload) != nil {
		return
	}

	server, database, err := p.targetOf(record, payload.Connection.ContainerID, payload.Database)
	if err != nil {
		return
	}

	if record.GetString("status") != schema.CommandSucceeded {
		database.Set("status", dbReady)
		database.Set("last_error", "The database was not dropped: "+record.GetString("error"))
		p.saveAs(database)
		return
	}

	ctx := audit.WithActor(context.Background(), dbActor)
	stackID := database.GetString("stack")

	if account, err := p.userOf(server, payload.User); err == nil {
		if err := p.App.DeleteWithContext(ctx, account); err != nil {
			p.Log.Error("failed to remove a database user", "user", account.Id, "error", err)
		}
	}
	if err := p.App.DeleteWithContext(ctx, database); err != nil {
		p.Log.Error("failed to remove a database", "database", database.Id, "error", err)
	}

	// the variables the panel added for this database go too: their secret is gone
	if stackID != "" {
		p.removeConnection(ctx, server, payload.Database, payload.User, stackID)
	}

	// the application keeps its network while it has another database on the server
	if stackID != "" {
		others, err := p.App.CountRecords(schema.Databases, dbx.HashExp{"server": server.Id, "stack": stackID})
		if err == nil && others == 0 {
			if network, err := p.App.FindFirstRecordByFilter(schema.DBNetworks, "server={:server} && stack={:stack}",
				dbx.Params{"server": server.Id, "stack": stackID}); err == nil {
				if err := p.App.DeleteWithContext(ctx, network); err != nil {
					p.Log.Error("failed to remove a database network", "network", network.Id, "error", err)
				}
			}
		}
	}
}

// removeConnection takes a dropped database's variables out of its stack's env
// file, as a new revision. The stack is not deployed for that: its containers
// keep running as they are until its next deploy.
func (p *Panel) removeConnection(ctx context.Context, server *core.Record, database, user, stackID string) {
	engine, err := engineOfServer(server)
	if err != nil {
		return
	}
	stack, err := p.App.FindRecordById(schema.Stacks, stackID)
	if err != nil {
		return
	}

	connection := databases.Connection{Engine: engine, Server: server.GetString("name"), Database: database, User: user}
	err = p.update(ctx, stack, func(fresh *core.Record) error {
		env, changed := databases.RemoveEnv(fresh.GetString("env"), connection.EnvLines())
		if !changed {
			return errUnchanged
		}
		fresh.Set("env", env)
		fresh.Set(recordNote, "The database "+database+" was dropped")
		return nil
	})
	if err != nil {
		p.Log.Error("failed to remove the variables of a dropped database", "stack", stackID, "error", err)
	}
}

// databaseStatsFinished stores what a server said about itself and its databases.
func (p *Panel) databaseStatsFinished(record *core.Record) {
	var payload struct {
		Connection commandConnection `json:"connection"`
	}
	if payloadInto(record, &payload) != nil {
		return
	}

	server, err := p.serverByContainer(record.GetString("host"), payload.Connection.ContainerID)
	if err != nil {
		return
	}

	if record.GetString("status") != schema.CommandSucceeded {
		server.Set("last_error", record.GetString("error"))
		p.saveAs(server)
		return
	}

	var result databases.Stats
	raw, err := json.Marshal(record.Get("result"))
	if err != nil || json.Unmarshal(raw, &result) != nil {
		return
	}

	server.Set("version", result.Version)
	server.Set("max_connections", result.MaxConnections)
	server.Set("connections", result.Connections)
	server.Set("size_bytes", result.SizeBytes)
	server.Set("stats_at", types.NowDateTime())
	server.Set("last_error", "")
	p.saveAs(server)

	records, err := p.App.FindRecordsByFilter(schema.Databases, "server={:server}", "", 0, 0, dbx.Params{"server": server.Id})
	if err != nil {
		return
	}

	// a dedicated server's databases belong to its stack: the panel lists what it
	// reports, and forgets what it no longer does
	if server.GetString("kind") == schema.DBServerDedicated {
		records = p.syncDedicatedDatabases(server, records, result)
	}

	for _, database := range records {
		stats := result.Databases[database.GetString("name")]
		if database.GetInt("size_bytes") == int(stats.SizeBytes) && database.GetInt("connections") == stats.Connections {
			continue
		}
		database.Set("size_bytes", stats.SizeBytes)
		database.Set("connections", stats.Connections)
		p.saveAs(database)
	}
}

// syncDedicatedDatabases makes the records of a dedicated server's databases match
// what it reported, and returns the records that remain.
func (p *Panel) syncDedicatedDatabases(server *core.Record, records []*core.Record, result databases.Stats) []*core.Record {
	ctx := audit.WithActor(context.Background(), dbActor)

	kept := []*core.Record{}
	known := map[string]bool{}
	for _, database := range records {
		if _, reported := result.Databases[database.GetString("name")]; !reported {
			if err := p.App.DeleteWithContext(ctx, database); err != nil {
				p.Log.Error("failed to forget a database", "database", database.Id, "error", err)
			}
			continue
		}
		known[database.GetString("name")] = true
		kept = append(kept, database)
	}

	collection, err := p.App.FindCachedCollectionByNameOrId(schema.Databases)
	if err != nil {
		return kept
	}

	engine, err := engineOfServer(server)
	if err != nil {
		return kept
	}

	for name := range result.Databases {
		// only names PocketCloud could also create are taken over
		if known[name] || databases.ValidDatabaseName(engine, name) != nil {
			continue
		}
		stats := result.Databases[name]
		database := core.NewRecord(collection)
		database.Set("server", server.Id)
		database.Set("name", name)
		database.Set("status", dbReady)
		database.Set("owner_kind", "stack")
		database.Set("stack", server.GetString("stack"))
		database.Set("size_bytes", stats.SizeBytes)
		database.Set("connections", stats.Connections)
		if err := p.App.SaveWithContext(ctx, database); err != nil {
			p.Log.Error("failed to list a database", "server", server.Id, "name", name, "error", err)
			continue
		}
		kept = append(kept, database)
	}

	return kept
}

// serverDeployFinished keeps the state of a shared server in step with its stack.
func (p *Panel) serverDeployFinished(record *core.Record) {
	var payload struct {
		Name string `json:"name"`
	}
	if payloadInto(record, &payload) != nil {
		return
	}

	server, err := p.App.FindFirstRecordByFilter(schema.DBServers, "host={:host} && name={:name} && kind={:kind}",
		dbx.Params{"host": record.GetString("host"), "name": payload.Name, "kind": schema.DBServerShared})
	if err != nil {
		return
	}

	if record.GetString("status") == schema.CommandSucceeded {
		server.Set("status", dbReady)
		server.Set("last_error", "")
	} else {
		server.Set("status", dbFailed)
		server.Set("last_error", record.GetString("error"))
	}
	p.saveAs(server)
}

// isLive tells whether a stack runs or is about to: a deploy that is under way
// may carry what has just changed, such as an old password, so it needs another.
func isLive(stack *core.Record) bool {
	status := stack.GetString("status")
	return status == stackDeployed || status == stackDeploying
}

// redeploy deploys a stack's current revision again, as the panel's own doing on
// behalf of whoever asked for what made it necessary.
func (p *Panel) redeploy(ctx context.Context, stack *core.Record, userID string) error {
	revision, err := p.findRevision(stack, stack.GetInt("current_revision"))
	if err != nil {
		return err
	}
	_, err = p.deployRevision(ctx, stack, revision, false, "", userID, panelRequester)
	return err
}

// connectStack gives an application its database: the variables in its env file,
// a new revision, the network to the server right away, and a deploy that keeps it
// there, if the stack is deployed.
func (p *Panel) connectStack(ctx context.Context, actor audit.Actor, userID string, server, database, stack *core.Record) error {
	if stack.GetString("host") != server.GetString("host") {
		return errors.New("a stack can only use the database servers of its own server")
	}

	engine, err := engineOfServer(server)
	if err != nil {
		return err
	}

	network := databases.NetworkName(stack.GetString("name"))

	if _, err := p.App.FindFirstRecordByFilter(schema.DBNetworks, "server={:server} && stack={:stack}",
		dbx.Params{"server": server.Id, "stack": stack.Id}); err != nil {
		collection, err := p.App.FindCachedCollectionByNameOrId(schema.DBNetworks)
		if err != nil {
			return err
		}
		record := core.NewRecord(collection)
		record.Set("server", server.Id)
		record.Set("stack", stack.Id)
		record.Set("name", network)
		if err := p.App.SaveWithContext(audit.WithActor(ctx, actor), record); err != nil {
			return err
		}
	}

	account, err := p.userOf(server, database.GetString("name"))
	if err != nil {
		return err
	}

	connection := databases.Connection{Engine: engine, Server: server.GetString("name"), Database: database.GetString("name"), User: account.GetString("name")}
	err = p.update(audit.WithActor(ctx, actor), stack, func(fresh *core.Record) error {
		env, changed := databases.AddEnv(fresh.GetString("env"), connection.EnvLines())
		if !changed {
			return errUnchanged
		}
		fresh.Set("env", env)
		fresh.Set(recordNote, "Connected to the database "+database.GetString("name"))
		if userID != "" {
			fresh.Set(recordAuthor, userID)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// running containers join at once; the next deploy keeps them there
	containers, err := p.App.FindRecordsByFilter(schema.Containers, "host={:host} && project={:project} && state='running'", "", 0, 0,
		dbx.Params{"host": stack.GetString("host"), "project": stack.GetString("name")})
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(containers))
	for _, container := range containers {
		ids = append(ids, container.GetString("docker_id"))
	}

	_, err = p.Commands.Create(ctx, commands.Options{
		Host:        stack.GetString("host"),
		Label:       stack.GetString("name") + " → " + server.GetString("name"),
		RequestedBy: userID,
		Command: &agentv1.Command{Payload: &agentv1.Command_DbNetworkAttach{DbNetworkAttach: &agentv1.DbNetworkAttach{
			Network:           network,
			ServerContainerId: server.GetString("container_ref"),
			ServerAlias:       server.GetString("name"),
			AppContainerIds:   ids,
		}}},
	})
	if err != nil {
		return err
	}

	if isLive(stack) {
		return p.redeploy(ctx, stack, userID)
	}

	return nil
}

// storeDump keeps a dump an agent uploaded, for a while.
func (p *Panel) storeDump(kind string) func(command *core.Record, fileName, path string, size uint64) (string, error) {
	return func(command *core.Record, fileName, path string, size uint64) (string, error) {
		var payload struct {
			Connection commandConnection `json:"connection"`
			Database   string            `json:"database"`
		}
		if err := payloadInto(command, &payload); err != nil {
			return "", err
		}

		server, err := p.serverByContainer(command.GetString("host"), payload.Connection.ContainerID)
		if err != nil {
			return "", fmt.Errorf("the dump belongs to no known server: %w", err)
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

		lifetime := exportLifetime
		dumpKind := schema.DumpExport
		if kind == artifactSQLFinal {
			lifetime, dumpKind = finalDumpLifetime, schema.DumpFinal
		}
		expires, err := types.ParseDateTime(time.Now().UTC().Add(lifetime))
		if err != nil {
			return "", err
		}

		dump := core.NewRecord(collection)
		dump.Set("server", server.Id)
		dump.Set("database_name", payload.Database)
		dump.Set("kind", dumpKind)
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
}

// pruneDumps removes dumps that have expired.
func (p *Panel) pruneDumps() {
	now, err := types.ParseDateTime(time.Now().UTC())
	if err != nil {
		return
	}
	records, err := p.App.FindRecordsByFilter(schema.DBDumps, "expires_at<{:now}", "", 0, 0, dbx.Params{"now": now.String()})
	if err != nil {
		p.Log.Error("failed to look for expired dumps", "error", err)
		return
	}
	for _, record := range records {
		if err := p.App.Delete(record); err != nil {
			p.Log.Error("failed to remove an expired dump", "dump", record.Id, "error", err)
		}
	}
}

// refreshAllStats asks every server with known credentials for its numbers, once
// an hour. The page asks again when someone looks.
func (p *Panel) refreshAllStats() {
	servers, err := p.App.FindRecordsByFilter(schema.DBServers, "admin_password!=''", "", 0, 0)
	if err != nil {
		p.Log.Error("failed to read the database servers", "error", err)
		return
	}
	for _, server := range servers {
		if !p.Commands.Connected(server.GetString("host")) {
			continue
		}
		if _, err := p.queueStats(context.Background(), server, ""); err != nil {
			p.Log.Debug("failed to ask a database server for its numbers", "server", server.Id, "error", err)
		}
	}
}

// errStatsRunning is returned when a server's numbers are already being read.
var errStatsRunning = errors.New("the numbers of this server were asked for a moment ago")

// queueStats asks a server for its numbers, unless that was done in the last minute.
func (p *Panel) queueStats(ctx context.Context, server *core.Record, userID string) (*core.Record, error) {
	since, err := types.ParseDateTime(time.Now().UTC().Add(-statsInterval))
	if err != nil {
		return nil, err
	}
	recent, err := p.App.FindRecordsByFilter(schema.Commands,
		"host={:host} && type='db.stats' && label={:label} && created>{:since}", "", 1, 0,
		dbx.Params{"host": server.GetString("host"), "label": server.GetString("name"), "since": since.String()})
	if err == nil && len(recent) > 0 {
		return recent[0], errStatsRunning
	}

	return p.Commands.Create(ctx, commands.Options{
		Host:        server.GetString("host"),
		Label:       server.GetString("name"),
		RequestedBy: userID,
		Command:     &agentv1.Command{Payload: &agentv1.Command_DbStats{DbStats: &agentv1.DbStats{Connection: connectionOf(server)}}},
	})
}
