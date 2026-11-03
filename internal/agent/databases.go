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
)

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
	if err := validNames(engine, payload.GetDatabase(), payload.GetUser()); err != nil {
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
	script := databases.Statements(engine, connection.GetAdminUser(), connection.GetAdminPassword(), databases.DropSQL(engine, payload.GetDatabase(), payload.GetUser()))
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

// attach connects a container to a network, under aliases, unless it is there.
func (a *Agent) attach(ctx context.Context, network, container string, aliases []string) error {
	info, err := a.Docker.Inspect(ctx, container)
	if err != nil {
		return err
	}
	if _, member := info.NetworkSettings.Networks[network]; member {
		return nil
	}

	var endpoint map[string]any
	if len(aliases) > 0 {
		endpoint = map[string]any{"Aliases": aliases}
	}
	return a.Docker.ConnectNetwork(ctx, network, container, endpoint)
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
		err := a.attach(ctx, network.GetNetwork(), server, []string{network.GetServerAlias()})
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
