package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
)

// Artifact kinds of database dumps.
const (
	ArtifactKindSQLExport = "sql_export"
	ArtifactKindSQLFinal  = "sql_final"
	ArtifactKindSQLMove   = "sql_move"
)

// moveStopTimeout is how long an application's container may take to stop before
// a move; database containers get Docker's default.
const moveStopTimeout = 30

// moveCommitTimeout bounds what a move does once its databases are copied: the
// old server stops or drops them, and nothing may cut that short.
const moveCommitTimeout = 5 * time.Minute

const dumpDir = "tmp"

func (a *Agent) registerDatabaseHandlers() {
	x := a.Executor

	x.Handle("db.database.create", a.handleDatabaseCreate)
	x.Handle("db.database.rotate", a.handleDatabaseRotate)
	x.Handle("db.database.drop", a.handleDatabaseDrop)
	x.Handle("db.database.clone", a.handleDatabaseClone)
	x.Handle("db.database.export", a.handleDatabaseExport)
	x.Handle("db.network.attach", a.handleNetworkAttach)
	x.Handle("db.stats", a.handleDatabaseStats)
	x.Handle("db.adminer.start", a.handleAdminerStart)
	x.Handle("db.adminer.stop", a.handleAdminerStop)
	x.Handle("db.server.move", a.handleServerMove)
}

// connectionOf checks what a command says about its server.
func connectionOf(connection *agentv1.DbConnection) (databases.Engine, error) {
	engine, ok := databases.EngineOf(connection.GetEngine())
	if !ok {
		return databases.Engine{}, fmt.Errorf("unknown database engine %q", connection.GetEngine())
	}
	if connection.GetContainerId() == "" {
		return databases.Engine{}, errors.New("the command does not say which container the database server is")
	}
	return engine, nil
}

// runScript runs a script in a server's container. What the script writes to its
// error output goes into the error, with every secret of the command masked:
// PostgreSQL quotes the statement that failed, password and all.
func (a *Agent) runScript(ctx context.Context, container string, script databases.Script, stdout io.Writer, secrets ...string) error {
	result, err := a.Docker.Run(ctx, container, dockerapi.ExecOptions{Cmd: script.Cmd, Env: script.Env}, stdout)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		message := strings.TrimSpace(maskSecrets(result.Stderr, secrets...))
		if message == "" {
			message = "no message"
		}
		return fmt.Errorf("the database server's client failed with exit code %d: %s", result.ExitCode, message)
	}
	return nil
}

func maskSecrets(text string, secrets ...string) string {
	for _, secret := range secrets {
		if len(secret) >= minMaskedSecret {
			text = strings.ReplaceAll(text, secret, "***")
		}
	}
	return text
}

func (a *Agent) handleDatabaseCreate(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetDbDatabaseCreate()
	connection := payload.GetConnection()

	engine, err := connectionOf(connection)
	if err != nil {
		return nil, err
	}
	if err := validNames(engine, payload.GetDatabase(), payload.GetUser()); err != nil {
		return nil, err
	}

	reporter.Progress("create", "Creating the database "+payload.GetDatabase(), 30)

	sql := databases.CreateSQL(engine, payload.GetDatabase(), payload.GetUser(), payload.GetPassword())
	script := databases.Statements(engine, connection.GetAdminUser(), connection.GetAdminPassword(), sql)
	if err := a.runScript(ctx, connection.GetContainerId(), script, io.Discard, payload.GetPassword(), connection.GetAdminPassword()); err != nil {
		return nil, err
	}

	return map[string]any{"database": payload.GetDatabase(), "user": payload.GetUser()}, nil
}

func (a *Agent) handleDatabaseRotate(ctx context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	payload := command.GetDbDatabaseRotate()
	connection := payload.GetConnection()

	engine, err := connectionOf(connection)
	if err != nil {
		return nil, err
	}
	if err := validNames(engine, payload.GetUser()); err != nil {
		return nil, err
	}
	if payload.GetPassword() == "" {
		return nil, errors.New("the command has no new password")
	}

	script := databases.Statements(engine, connection.GetAdminUser(), connection.GetAdminPassword(), databases.RotateSQL(engine, payload.GetUser(), payload.GetPassword()))
	if err := a.runScript(ctx, connection.GetContainerId(), script, io.Discard, payload.GetPassword(), connection.GetAdminPassword()); err != nil {
		return nil, err
	}

	return map[string]any{"user": payload.GetUser()}, nil
}

func (a *Agent) handleDatabaseClone(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetDbDatabaseClone()
	connection := payload.GetConnection()

	engine, err := connectionOf(connection)
	if err != nil {
		return nil, err
	}
	if err := validNames(engine, payload.GetSource(), payload.GetTarget(), payload.GetUser()); err != nil {
		return nil, err
	}

	secrets := []string{payload.GetPassword(), connection.GetAdminPassword()}

	reporter.Progress("create", "Creating the database "+payload.GetTarget(), 10)
	create := databases.Statements(engine, connection.GetAdminUser(), connection.GetAdminPassword(),
		databases.CreateSQL(engine, payload.GetTarget(), payload.GetUser(), payload.GetPassword()))
	if err := a.runScript(ctx, connection.GetContainerId(), create, io.Discard, secrets...); err != nil {
		return nil, err
	}

	reporter.Progress("copy", fmt.Sprintf("Copying %s into %s", payload.GetSource(), payload.GetTarget()), 30)
	clone := databases.Clone(engine, connection.GetAdminUser(), connection.GetAdminPassword(), payload.GetSource(), payload.GetTarget(), payload.GetUser())
	if err := a.runScript(ctx, connection.GetContainerId(), clone, reporter, secrets...); err != nil {
		return nil, err
	}

	return map[string]any{"database": payload.GetTarget()}, nil
}

// dumpToFile writes a gzipped dump to the agent volume. It is only uploaded once
// the dump is complete, so that a broken dump never passes for a good one.
func (a *Agent) dumpToFile(ctx context.Context, connection *agentv1.DbConnection, engine databases.Engine, database string) (string, error) {
	dir := filepath.Join(a.Config.StateDir, dumpDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	file, err := os.CreateTemp(dir, "dump-*.sql.gz")
	if err != nil {
		return "", err
	}

	compressed := gzip.NewWriter(file)
	script := databases.Dump(engine, connection.GetAdminUser(), connection.GetAdminPassword(), database)
	err = a.runScript(ctx, connection.GetContainerId(), script, compressed, connection.GetAdminPassword())
	if closeErr := compressed.Close(); err == nil {
		err = closeErr
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(file.Name())
		return "", fmt.Errorf("the dump of %s failed: %w", database, err)
	}

	return file.Name(), nil
}

func dumpName(database, kind string) string {
	return fmt.Sprintf("%s-%s-%s.sql.gz", database, kind, time.Now().UTC().Format("20060102-150405"))
}

func (a *Agent) handleDatabaseExport(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetDbDatabaseExport()
	connection := payload.GetConnection()

	engine, err := connectionOf(connection)
	if err != nil {
		return nil, err
	}
	if err := validNames(engine, payload.GetDatabase()); err != nil {
		return nil, err
	}

	reporter.Progress("dump", "Dumping "+payload.GetDatabase(), 20)
	path, err := a.dumpToFile(ctx, connection, engine, payload.GetDatabase())
	if err != nil {
		return nil, err
	}
	defer os.Remove(path)

	reporter.Progress("upload", "Uploading the dump to the panel", 70)
	id, size, err := a.uploadArtifactFile(ctx, command.GetId(), ArtifactKindSQLExport, dumpName(payload.GetDatabase(), "export"), path)
	if err != nil {
		return nil, fmt.Errorf("failed to upload the dump: %w", err)
	}

	return map[string]any{"dump": id, "size_bytes": float64(size)}, nil
}

// handleDatabaseDrop keeps a final dump in the panel before anything is dropped.
func (a *Agent) handleDatabaseDrop(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetDbDatabaseDrop()
	connection := payload.GetConnection()

	engine, err := connectionOf(connection)
	if err != nil {
		return nil, err
	}
	// without a user the database goes alone: its owner owns others too
	users := []string{}
	if payload.GetUser() != "" {
		users = append(users, payload.GetUser())
	}
	if err := validNames(engine, append([]string{payload.GetDatabase()}, users...)...); err != nil {
		return nil, err
	}

	reporter.Progress("dump", "Keeping a final dump of "+payload.GetDatabase(), 20)
	path, err := a.dumpToFile(ctx, connection, engine, payload.GetDatabase())
	if err != nil {
		return nil, err
	}
	defer os.Remove(path)

	id, size, err := a.uploadArtifactFile(ctx, command.GetId(), ArtifactKindSQLFinal, dumpName(payload.GetDatabase(), "final"), path)
	if err != nil {
		return nil, fmt.Errorf("the final dump could not be stored, so nothing was dropped: %w", err)
	}

	reporter.Progress("drop", "Dropping "+payload.GetDatabase(), 80)
	script := databases.Statements(engine, connection.GetAdminUser(), connection.GetAdminPassword(), databases.DropManySQL(engine, []string{payload.GetDatabase()}, users))
	if err := a.runScript(ctx, connection.GetContainerId(), script, io.Discard, connection.GetAdminPassword()); err != nil {
		return nil, err
	}

	return map[string]any{"dump": id, "size_bytes": float64(size)}, nil
}

func (a *Agent) handleDatabaseStats(ctx context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	connection := command.GetDbStats().GetConnection()

	engine, err := connectionOf(connection)
	if err != nil {
		return nil, err
	}

	var output bytes.Buffer
	script := databases.Statements(engine, connection.GetAdminUser(), connection.GetAdminPassword(), databases.StatsSQL(engine))
	if err := a.runScript(ctx, connection.GetContainerId(), script, &output, connection.GetAdminPassword()); err != nil {
		return nil, err
	}

	stats, err := databases.ParseStats(engine, output.String())
	if err != nil {
		return nil, err
	}

	result := map[string]any{
		"version":         stats.Version,
		"max_connections": float64(stats.MaxConnections),
		"connections":     float64(stats.Connections),
		"size_bytes":      float64(stats.SizeBytes),
	}
	list := map[string]any{}
	for name, database := range stats.Databases {
		list[name] = map[string]any{"size_bytes": float64(database.SizeBytes), "connections": float64(database.Connections)}
	}
	result["databases"] = list

	return result, nil
}

// validNames refuses names the panel would never send: they go into SQL.
func validNames(engine databases.Engine, names ...string) error {
	for _, name := range names {
		if err := databases.ValidDatabaseName(engine, name); err != nil {
			return err
		}
	}
	return nil
}

// ensureNetwork creates a database network if it does not exist yet. It is
// internal: whatever is on it reaches the outside through its other networks only.
func (a *Agent) ensureNetwork(ctx context.Context, name string) error {
	if _, err := a.Docker.InspectNetwork(ctx, name); err == nil {
		return nil
	} else if !dockerapi.IsNotFound(err) {
		return err
	}

	_, err := a.Docker.CreateNetwork(ctx, name, dockerapi.NetworkOptions{
		Internal: true,
		Labels:   map[string]string{"pocketcloud.role": "db-network"},
	})
	// another command may have created it in the meantime
	if err != nil {
		if _, inspectErr := a.Docker.InspectNetwork(ctx, name); inspectErr == nil {
			return nil
		}
	}
	return err
}

// attach connects a container to a network, under aliases, unless it is there
// with all of them. Another command may attach it at the same moment: the engine
// then refuses one of the two ("endpoint ... already exists"), and that one is
// done all the same. Docker cannot add aliases to an endpoint, so a container on
// the network without some of them is connected anew.
func (a *Agent) attach(ctx context.Context, network, container string, aliases []string) error {
	// state reports whether the container is on the network, and with every alias
	state := func() (member, complete bool, err error) {
		info, err := a.Docker.Inspect(ctx, container)
		if err != nil {
			return false, false, err
		}
		endpoint, found := info.NetworkSettings.Networks[network]
		if !found {
			return false, false, nil
		}
		have := []string{}
		if list, ok := endpoint["Aliases"].([]any); ok {
			for _, alias := range list {
				if name, ok := alias.(string); ok {
					have = append(have, name)
				}
			}
		}
		for _, alias := range aliases {
			if !slices.Contains(have, alias) {
				return true, false, nil
			}
		}
		return true, true, nil
	}

	member, complete, err := state()
	if err != nil || complete {
		return err
	}
	if member {
		if err := a.Docker.DisconnectNetwork(ctx, network, container, false); err != nil && !dockerapi.IsNotFound(err) {
			return err
		}
	}

	var endpoint map[string]any
	if len(aliases) > 0 {
		endpoint = map[string]any{"Aliases": aliases}
	}
	err = a.Docker.ConnectNetwork(ctx, network, container, endpoint)
	if err != nil {
		if _, complete, stateErr := state(); stateErr == nil && complete {
			return nil
		}
	}
	return err
}

// prepareNetworks makes a deploy's database networks exist, with their servers on
// them. With an output, as for a deploy, a server whose container does not exist
// yet is only noted: its own deploy puts it on the network through its override.
func (a *Agent) prepareNetworks(ctx context.Context, networks []*agentv1.DbNetworkAttach, output io.Writer) error {
	for _, network := range networks {
		if err := a.ensureNetwork(ctx, network.GetNetwork()); err != nil {
			return fmt.Errorf("the database network %s: %w", network.GetNetwork(), err)
		}
		server := network.GetServerContainerId()
		if server == "" {
			continue
		}
		aliases := append([]string{network.GetServerAlias()}, network.GetExtraAliases()...)
		err := a.attach(ctx, network.GetNetwork(), server, aliases)
		if err != nil && output != nil && dockerapi.IsNotFound(err) {
			_, _ = fmt.Fprintf(output, "The database server %s does not run yet: it joins %s when it starts.\n", network.GetServerAlias(), network.GetNetwork())
			continue
		}
		if err != nil {
			return fmt.Errorf("attaching the database server to %s: %w", network.GetNetwork(), err)
		}
	}
	return nil
}

// handleNetworkAttach connects an application to a database server right away.
// The application's next deploy keeps it there through its override.
func (a *Agent) handleNetworkAttach(ctx context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	payload := command.GetDbNetworkAttach()
	if !strings.HasPrefix(payload.GetNetwork(), "pcdb-") {
		return nil, fmt.Errorf("%q is not a database network", payload.GetNetwork())
	}

	if err := a.prepareNetworks(ctx, []*agentv1.DbNetworkAttach{payload}, nil); err != nil {
		return nil, err
	}

	for _, container := range payload.GetAppContainerIds() {
		if err := a.attach(ctx, payload.GetNetwork(), container, nil); err != nil {
			return nil, fmt.Errorf("attaching %s: %w", container, err)
		}
	}

	return map[string]any{"network": payload.GetNetwork(), "containers": float64(len(payload.GetAppContainerIds()))}, nil
}

// movedDatabase is what the target got of one database.
type movedDatabase struct {
	name string
	user string
}

// handleServerMove moves databases into another server of the same engine: the
// application stops, each database is dumped, created on the target with the
// user the application already signs in as, restored and compared table by table.
// Nothing on the source changes, so a failure removes what the target got and
// starts the application again on its old server.
func (a *Agent) handleServerMove(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetDbServerMove()
	source, target := payload.GetSource(), payload.GetTarget()

	engine, err := connectionOf(source)
	if err != nil {
		return nil, fmt.Errorf("the source: %w", err)
	}
	targetEngine, err := connectionOf(target)
	if err != nil {
		return nil, fmt.Errorf("the target: %w", err)
	}
	if engine.Name != targetEngine.Name {
		return nil, errors.New("the two servers run different engines")
	}
	if len(payload.GetDatabases()) == 0 {
		return nil, errors.New("the command names no database to move")
	}

	secrets := []string{source.GetAdminPassword(), target.GetAdminPassword()}
	for _, item := range payload.GetDatabases() {
		if err := validNames(engine, item.GetDatabase(), item.GetUser()); err != nil {
			return nil, err
		}
		if item.GetPassword() == "" {
			return nil, fmt.Errorf("the command has no password for %s", item.GetUser())
		}
		secrets = append(secrets, item.GetPassword())
	}

	// nothing on the target may be in the way: a create would quietly reuse it
	reporter.Progress("check", "Checking the target server", 5)
	for _, item := range payload.GetDatabases() {
		var output bytes.Buffer
		check := databases.Statements(engine, target.GetAdminUser(), target.GetAdminPassword(), databases.ExistsSQL(engine, item.GetDatabase(), item.GetUser()))
		if err := a.runScript(ctx, target.GetContainerId(), check, &output, secrets...); err != nil {
			return nil, err
		}
		database, user := databases.ParseExists(output.String())
		if database {
			return nil, fmt.Errorf("the target server already has a database named %s", item.GetDatabase())
		}
		if user {
			return nil, fmt.Errorf("the target server already has a user named %s", item.GetUser())
		}
	}

	// a stack's own server moves whole: a database the move does not name would
	// stay behind in a container that is about to stop for good
	if payload.GetStopSource() {
		if err := a.checkWholeServer(ctx, source, engine, payload.GetDatabases(), secrets); err != nil {
			return nil, err
		}
	}

	// the passwords the databases keep are the ones the old server knows: the
	// application signs in with them today, and will on the new server
	if err := a.checkSignIns(ctx, source, engine, payload.GetDatabases(), secrets); err != nil {
		return nil, err
	}

	// the application stops: its containers as they are now, not as the panel
	// saw them, since a deploy may have recreated them since
	reporter.Progress("stop", "Stopping the application", 10)
	containers, err := a.Docker.Containers(ctx)
	if err != nil {
		return nil, err
	}
	stopped := []string{}
	startAgain := func() {
		for _, id := range stopped {
			if err := a.Docker.Start(context.Background(), id); err != nil {
				a.Log.Warn("failed to start an application container again", "container", id, "error", err)
			}
		}
	}
	for _, container := range containers {
		if container.Labels[composeProjectLabel] != payload.GetStack() || isContainer(container, source.GetContainerId()) {
			continue
		}
		if container.State != "running" && container.State != "restarting" {
			continue
		}
		if err := a.Docker.Stop(ctx, container.ID, moveStopTimeout); err != nil && !dockerapi.IsNotFound(err) {
			startAgain()
			return nil, fmt.Errorf("failed to stop the application: %w", err)
		}
		stopped = append(stopped, container.ID)
	}

	moved := []movedDatabase{}
	undo := func() {
		if len(moved) > 0 {
			names, users := []string{}, []string{}
			for _, done := range moved {
				names = append(names, done.name)
				users = append(users, done.user)
			}
			drop := databases.Statements(engine, target.GetAdminUser(), target.GetAdminPassword(), databases.DropManySQL(engine, names, users))
			if err := a.runScript(context.Background(), target.GetContainerId(), drop, io.Discard, secrets...); err != nil {
				a.Log.Warn("failed to remove the databases the move had created", "databases", names, "error", err)
			}
		}
		startAgain()
	}

	results := []any{}
	count := len(payload.GetDatabases())
	for index, item := range payload.GetDatabases() {
		share := int32(10 + 80*index/count) //nolint:gosec // a percentage
		reporter.Progress("move", fmt.Sprintf("Moving %s (%d of %d)", item.GetDatabase(), index+1, count), share)

		result, err := a.moveDatabase(ctx, command.GetId(), index, payload, item, engine, secrets, &moved)
		if err != nil {
			undo()
			return nil, err
		}
		results = append(results, result)
	}

	// until here a cancel, the timeout or a shutdown undoes the move; from here on
	// it finishes, as the old server is about to stop or lose what moved
	if err := ctx.Err(); err != nil {
		undo()
		return nil, fmt.Errorf("the move was interrupted before it was complete, and is undone: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), moveCommitTimeout)
	defer cancel()

	stoppedSource := false
	if payload.GetStopSource() {
		reporter.Progress("source", "Stopping the old database server", 95)
		stoppedSource = a.retire(ctx, source.GetContainerId())
	}

	// a shared server keeps no stale copy that a new database of the same name
	// would quietly reuse
	dropped := false
	if payload.GetDropFromSource() {
		reporter.Progress("source", "Dropping the databases from the old server", 95)
		names, users := []string{}, []string{}
		for _, item := range payload.GetDatabases() {
			names = append(names, item.GetDatabase())
			users = append(users, item.GetUser())
		}
		drop := databases.Statements(engine, source.GetAdminUser(), source.GetAdminPassword(), databases.DropManySQL(engine, names, users))
		if err := a.runScript(ctx, source.GetContainerId(), drop, io.Discard, secrets...); err != nil {
			a.Log.Warn("failed to drop the moved databases from the old server", "error", err)
		} else {
			dropped = true
		}
	}
	if network := payload.GetLeaveNetwork(); network != "" {
		if err := a.Docker.DisconnectNetwork(ctx, network, source.GetContainerId(), false); err != nil && !dockerapi.IsNotFound(err) {
			a.Log.Warn("the old server failed to leave the application's network", "network", network, "error", err)
		}
	}

	return map[string]any{
		"databases":      results,
		"source_stopped": stoppedSource,
		"source_dropped": dropped,
		// the panel deploys the application again if it was running
		"app_stopped": len(stopped),
	}, nil
}

// checkWholeServer refuses to move a server whose databases the move does not
// all name, such as one created since the panel last read the server or one
// with a name the panel cannot take, and a PostgreSQL server whose application
// keeps tables in the database postgres, which a shared server keeps for itself.
func (a *Agent) checkWholeServer(ctx context.Context, source *agentv1.DbConnection, engine databases.Engine, items []*agentv1.DbMoveItem, secrets []string) error {
	var output bytes.Buffer
	list := databases.Statements(engine, source.GetAdminUser(), source.GetAdminPassword(), databases.DatabasesSQL(engine))
	if err := a.runScript(ctx, source.GetContainerId(), list, &output, secrets...); err != nil {
		return err
	}
	named := map[string]bool{}
	for _, item := range items {
		named[item.GetDatabase()] = true
	}
	left := []string{}
	for _, name := range databases.ParseNames(output.String()) {
		if !named[name] {
			left = append(left, name)
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("the server also has %s, which the move does not name: read the server's numbers again, or move by hand what the panel cannot", strings.Join(left, ", "))
	}

	if engine.Name == databases.Postgres.Name {
		tables, err := a.countTables(ctx, source, engine, engine.AdminUser, secrets)
		if err != nil {
			return err
		}
		if tables > 0 {
			return fmt.Errorf("the application keeps %d tables in the database %s, which a shared server keeps for itself", tables, engine.AdminUser)
		}
	}
	return nil
}

// checkSignIns signs in as each user of the move with the password it keeps.
func (a *Agent) checkSignIns(ctx context.Context, source *agentv1.DbConnection, engine databases.Engine, items []*agentv1.DbMoveItem, secrets []string) error {
	checked := map[string]bool{}
	for _, item := range items {
		if checked[item.GetUser()] {
			continue
		}
		checked[item.GetUser()] = true
		check := databases.SignIn(engine, item.GetUser(), item.GetPassword(), item.GetDatabase())
		if err := a.runScript(ctx, source.GetContainerId(), check, io.Discard, secrets...); err != nil {
			return fmt.Errorf("%s cannot sign in to %s with the password the move would give it, so nothing was changed: %w", item.GetUser(), item.GetDatabase(), err)
		}
	}
	return nil
}

// retire keeps a stack's old database container out of the way for good: it is
// stopped, Docker no longer starts it by itself, and it leaves its networks, so
// that nothing reaches the old data if someone starts it anyway.
func (a *Agent) retire(ctx context.Context, container string) bool {
	if err := a.Docker.Stop(ctx, container, 0); err != nil && !dockerapi.IsNotFound(err) {
		a.Log.Warn("failed to stop the old database server", "container", container, "error", err)
		return false
	}
	if err := a.Docker.SetRestartPolicy(ctx, container, "no"); err != nil {
		a.Log.Warn("failed to keep the old database server from starting again", "container", container, "error", err)
	}
	inspect, err := a.Docker.Inspect(ctx, container)
	if err != nil {
		a.Log.Warn("failed to read the old database server's networks", "container", container, "error", err)
		return true
	}
	for network := range inspect.NetworkSettings.Networks {
		if err := a.Docker.DisconnectNetwork(ctx, network, container, true); err != nil && !dockerapi.IsNotFound(err) {
			a.Log.Warn("the old database server failed to leave a network", "network", network, "error", err)
		}
	}
	return true
}

// isContainer tells whether a listed container is the one a reference names, by
// its id, the start of its id, or its name.
func isContainer(container dockerapi.Container, reference string) bool {
	if reference == "" {
		return false
	}
	return container.ID == reference || strings.HasPrefix(container.ID, reference) || slices.Contains(container.Names, "/"+reference)
}

// moveDatabase moves one database. Once it exists on the target it is in moved,
// so that a later failure removes it again.
func (a *Agent) moveDatabase(ctx context.Context, commandID string, index int, payload *agentv1.DbServerMove, item *agentv1.DbMoveItem, engine databases.Engine, secrets []string, moved *[]movedDatabase) (map[string]any, error) {
	source, target := payload.GetSource(), payload.GetTarget()
	name := item.GetDatabase()

	dir := filepath.Join(a.Config.StateDir, dumpDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(dir, "move-*.sql")
	if err != nil {
		return nil, err
	}
	defer os.Remove(file.Name())

	dump := databases.Dump(engine, source.GetAdminUser(), source.GetAdminPassword(), name)
	err = a.runScript(ctx, source.GetContainerId(), dump, file, secrets...)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, fmt.Errorf("the dump of %s failed: %w", name, err)
	}

	info, err := os.Stat(file.Name())
	if err != nil {
		return nil, err
	}

	// the target was checked at the start, but something else may have made
	// the name since, and a create would quietly reuse it
	var exists bytes.Buffer
	check := databases.Statements(engine, target.GetAdminUser(), target.GetAdminPassword(), databases.ExistsSQL(engine, name, item.GetUser()))
	if err := a.runScript(ctx, target.GetContainerId(), check, &exists, secrets...); err != nil {
		return nil, err
	}
	databaseThere, userThere := databases.ParseExists(exists.String())
	ownUser := slices.ContainsFunc(*moved, func(done movedDatabase) bool { return done.user == item.GetUser() })
	if databaseThere || userThere && !ownUser {
		return nil, fmt.Errorf("the target server has a database or user named like %s since the move began", name)
	}

	create := databases.Statements(engine, target.GetAdminUser(), target.GetAdminPassword(), databases.CreateSQL(engine, name, item.GetUser(), item.GetPassword()))
	if err := a.runScript(ctx, target.GetContainerId(), create, io.Discard, secrets...); err != nil {
		return nil, fmt.Errorf("creating %s on the target failed: %w", name, err)
	}
	*moved = append(*moved, movedDatabase{name: name, user: item.GetUser()})

	// the dump goes into the target's container and is restored from there
	inside := fmt.Sprintf("pocketcloud-move-%s-%d.sql", commandID, index)
	content, err := os.Open(file.Name())
	if err != nil {
		return nil, err
	}
	err = a.Docker.CopyFileFrom(ctx, target.GetContainerId(), "/tmp", inside, content, info.Size(), 0o600)
	content.Close()
	if err != nil {
		return nil, fmt.Errorf("copying the dump of %s to the target failed: %w", name, err)
	}

	restore := databases.RestoreFile(engine, target.GetAdminUser(), target.GetAdminPassword(), name, item.GetUser(), "/tmp/"+inside)
	if err := a.runScript(ctx, target.GetContainerId(), restore, io.Discard, secrets...); err != nil {
		return nil, fmt.Errorf("restoring %s on the target failed: %w", name, err)
	}

	// the same tables on both sides, or the move does not count
	before, err := a.countTables(ctx, source, engine, name, secrets)
	if err != nil {
		return nil, err
	}
	after, err := a.countTables(ctx, target, engine, name, secrets)
	if err != nil {
		return nil, err
	}
	if before != after {
		return nil, fmt.Errorf("%s has %d tables on the old server but %d on the new one", name, before, after)
	}

	result := map[string]any{"database": name, "tables": float64(after), "size_bytes": float64(info.Size())}

	// a copy of what moved stays in the panel for a while; the old server's data
	// stays too, so a failed upload does not undo the move
	if id, size, err := a.uploadMoveDump(ctx, commandID, name, file.Name()); err != nil {
		result["dump_error"] = err.Error()
	} else {
		result["dump"], result["dump_bytes"] = id, float64(size)
	}

	return result, nil
}

func (a *Agent) countTables(ctx context.Context, connection *agentv1.DbConnection, engine databases.Engine, database string, secrets []string) (int, error) {
	var output bytes.Buffer
	script := databases.CountTables(engine, connection.GetAdminUser(), connection.GetAdminPassword(), database)
	if err := a.runScript(ctx, connection.GetContainerId(), script, &output, secrets...); err != nil {
		return 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(output.String()))
	if err != nil {
		return 0, fmt.Errorf("the table count of %s cannot be read: %q", database, strings.TrimSpace(output.String()))
	}
	return count, nil
}

// uploadMoveDump compresses a plain dump and uploads it.
func (a *Agent) uploadMoveDump(ctx context.Context, commandID, database, plain string) (string, int64, error) {
	compressed, err := os.CreateTemp(filepath.Dir(plain), "move-*.sql.gz")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(compressed.Name())

	in, err := os.Open(plain)
	if err != nil {
		compressed.Close()
		return "", 0, err
	}
	writer := gzip.NewWriter(compressed)
	_, err = io.Copy(writer, in)
	in.Close()
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	if closeErr := compressed.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", 0, err
	}

	id, size, err := a.uploadArtifactFile(ctx, commandID, ArtifactKindSQLMove, dumpName(database, "move"), compressed.Name())
	return id, int64(size), err //nolint:gosec // the size of a file on disk
}
