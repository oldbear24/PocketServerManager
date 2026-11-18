package panel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// Temporary databases (#53) are PocketCloud's own copies on a shared server:
// a restore test's pc_rt_<check id>, an in-place restore's scratch copy
// pc_rs_<restore id>, and the data an in-place restore of PostgreSQL
// replaced, pc_old_<restore id>. Each is a databases record with
// temporary_until, made in the transaction of the command that makes the
// database, so the record is the schedule: once its time has come and its
// restore or test has ended, the sweep drops it with db.database.discard,
// across restarts of the panel and of the agent. A name of the namespace that
// a shared server reports without a record, as after a panel restored from
// an older backup (#32), becomes one, and is dropped too. Nothing outside the
// namespace is ever dropped as temporary.

const (
	// orphanLife is how long a temporary database found without a record
	// stays.
	orphanLife = 2 * time.Hour
	// discardAgain is when a discard that failed is tried again.
	discardAgain = 10 * time.Minute
	// sweepBatch is how many temporary databases a pass of the sweep drops.
	sweepBatch = 100
)

// temporaryMessage refuses what is not done with a temporary database.
const temporaryMessage = "This is one of PocketCloud's temporary databases of a restore or a restore test: it is dropped by itself, and nothing else is done with it."

func (p *Panel) registerTemporaries() {
	p.Commands.OnFinish("db.database.discard", p.discardFinished)
	p.App.Cron().MustAdd("pocketcloud_db_temporaries", "* * * * *", func() { p.SweepTemporaries(time.Now()) })
	// the alert of a temporary database that was not dropped goes with its
	// record, however it goes
	p.App.OnRecordAfterDeleteSuccess(schema.Databases).BindFunc(func(e *core.RecordEvent) error {
		if isTemporary(e.Record) {
			if err := events.ResolveAlert(e.App, backup.AlertTemporaryLeft, e.Record.Id); err != nil {
				p.Log.Error("failed to resolve an alert", "rule", backup.AlertTemporaryLeft, "error", err)
			}
		}
		return e.Next()
	})
}

// isTemporary tells whether a database is one of PocketCloud's temporary
// databases: it has a time to go, or a name of the namespace.
func isTemporary(database *core.Record) bool {
	return !database.GetDateTime("temporary_until").IsZero() || schema.TemporaryDatabase.MatchString(database.GetString("name"))
}

// temporaryHeld says why a temporary database is not dropped now, as the
// route answers it: its restore test or its restore is queued or running;
// "" once it ended, or when nothing it names is left.
func (p *Panel) temporaryHeld(database *core.Record) string {
	prefix, id, ok := databases.TemporaryOwner(database.GetString("name"))
	if !ok {
		return ""
	}
	if prefix == databases.TemporaryTest {
		row, err := p.App.FindRecordById(schema.BackupChecks, id)
		if err == nil && row.GetString("kind") == schema.CheckRestoreTest &&
			(row.GetString("status") == schema.CheckQueued || row.GetString("status") == schema.CheckRunning) {
			return "Its restore test is still running."
		}
		return ""
	}
	restore, err := p.App.FindRecordById(schema.BackupRestores, id)
	if err == nil && (restore.GetString("status") == schema.RestoreQueued || restore.GetString("status") == schema.RestoreRunning) {
		return "Its restore is still running."
	}
	return ""
}

// SweepTemporaries drops the temporary databases whose time has come: of a
// restore or a restore test that ended, or of none, on a server whose agent
// is connected. The panel calls it every minute.
func (p *Panel) SweepTemporaries(now time.Time) {
	at, err := types.ParseDateTime(now.UTC())
	if err != nil {
		return
	}
	due, err := p.App.FindRecordsByFilter(schema.Databases, "temporary_until!='' && temporary_until<={:now} && status!={:dropping}",
		"temporary_until,id", sweepBatch, 0, dbx.Params{"now": at.String(), "dropping": dbDropping})
	if err != nil {
		p.Log.Error("failed to read the temporary databases", "error", err)
		return
	}
	for _, database := range due {
		// nothing outside the namespace is dropped as temporary
		if databases.ValidTemporaryName(database.GetString("name")) != nil || p.temporaryHeld(database) != "" {
			continue
		}
		server, err := p.App.FindRecordById(schema.DBServers, database.GetString("server"))
		if err != nil || server.GetString("admin_password") == "" || !p.Commands.Connected(server.GetString("host")) {
			continue
		}
		if _, err := p.discardTemporary(database, server, "", panelRequester); err != nil && !errors.Is(err, errUnchanged) {
			p.Log.Error("failed to drop a temporary database", "database", database.Id, "error", err)
		}
	}
}

// discardTemporary marks a temporary database as dropping and queues its
// db.database.discard, in one transaction, and is the command: errUnchanged
// when it is being dropped already.
func (p *Panel) discardTemporary(database, server *core.Record, userID, label string) (*core.Record, error) {
	name := database.GetString("name")
	// checked again, as the record may come from anywhere: nothing outside
	// the namespace is dropped as temporary
	if err := databases.ValidTemporaryName(name); err != nil {
		return nil, err
	}
	revoke := p.revokeFrom(database, server)
	var command *core.Record
	err := p.App.RunInTransaction(func(tx core.App) error {
		fresh, err := tx.FindRecordById(schema.Databases, database.Id)
		if err != nil {
			return err
		}
		if fresh.GetString("status") == dbDropping || !isTemporary(fresh) {
			return errUnchanged
		}
		fresh.Set("status", dbDropping)
		if err := tx.SaveWithContext(audit.WithActor(context.Background(), dbActor), fresh); err != nil {
			return err
		}
		command, err = p.Commands.CreateIn(context.Background(), tx, commands.Options{
			Host:        server.GetString("host"),
			Label:       server.GetString("name") + "/" + name,
			RequestedBy: userID,
			Requester:   label,
			Command: &agentv1.Command{Payload: &agentv1.Command_DbDatabaseDiscard{DbDatabaseDiscard: &agentv1.DbDatabaseDiscard{
				Connection: connectionOf(server),
				Names:      []string{name},
				RevokeFrom: revoke,
			}}},
		})
		return err
	})
	return command, err
}

// revokeFrom is the user granted on a temporary database: the user of the
// database its restore or restore test restored, as the restore restores as
// it; "" when that is not known, as for one found without a record.
func (p *Panel) revokeFrom(database, server *core.Record) string {
	prefix, id, ok := databases.TemporaryOwner(database.GetString("name"))
	if !ok {
		return ""
	}
	if prefix == databases.TemporaryTest {
		row, err := p.App.FindRecordById(schema.BackupChecks, id)
		if err != nil {
			return ""
		}
		source, err := p.databaseOf(server, row.GetString("database_name"))
		if err != nil {
			return ""
		}
		owner, err := p.ownerOf(server, source)
		if err != nil {
			return ""
		}
		return owner.GetString("name")
	}
	restore, err := p.App.FindRecordById(schema.BackupRestores, id)
	if err != nil {
		return ""
	}
	if root, ok := databaseRoot(restore); ok {
		return root.User
	}
	return ""
}

// discardFinished keeps how a discard ended: a temporary database dropped
// goes, with an event; one that was not is tried again in ten minutes, and
// alerts once that failed twice.
func (p *Panel) discardFinished(command *core.Record) {
	var payload struct {
		Connection commandConnection `json:"connection"`
		Names      []string          `json:"names"`
	}
	if payloadInto(command, &payload) != nil {
		return
	}
	server, err := p.serverByContainer(command.GetString("host"), payload.Connection.ContainerID)
	if err != nil {
		return
	}
	succeeded := command.GetString("status") == schema.CommandSucceeded
	reason := sentenceText(command.GetString("error"))
	if reason == "" {
		reason = "The drop did not finish."
	}
	ctx := audit.WithActor(context.Background(), dbActor)
	for _, name := range payload.Names {
		database, err := p.databaseOf(server, name)
		if err != nil || !isTemporary(database) {
			continue
		}
		data := map[string]any{"database": database.Id, "server": server.Id, "command": command.Id}
		if succeeded {
			if err := p.App.DeleteWithContext(ctx, database); err != nil {
				p.Log.Error("failed to remove a temporary database", "database", database.Id, "error", err)
				continue
			}
			events.Log(p.App, server.GetString("host"), backup.EventTemporaryDropped, schema.SeverityInfo,
				fmt.Sprintf("The temporary database %s on %s was dropped.", name, server.GetString("name")), data)
			continue
		}
		again := database.GetString("last_error") != ""
		err = p.update(ctx, database, func(fresh *core.Record) error {
			fresh.Set("status", dbReady)
			fresh.Set("last_error", cutText("The temporary database was not dropped: "+reason, 2048))
			fresh.Set("temporary_until", time.Now().UTC().Add(discardAgain))
			return nil
		})
		if err != nil {
			p.Log.Error("failed to keep a temporary database's drop", "database", database.Id, "error", err)
		}
		if again {
			message := cutText(fmt.Sprintf("The temporary database %s on %s could not be dropped: %s", name, server.GetString("name"), reason), 2048)
			if err := events.RaiseAlert(p.App, backup.AlertTemporaryLeft, database.Id, server.GetString("host"), schema.SeverityWarning, message); err != nil {
				p.Log.Error("failed to raise an alert", "rule", backup.AlertTemporaryLeft, "error", err)
			}
		}
	}
}

// adoptTemporaries makes a record of each database of the namespace that a
// shared server reports and the panel has none of, as after a panel restored
// from an older backup, to be dropped in two hours.
func (p *Panel) adoptTemporaries(server *core.Record, records []*core.Record, result databases.Stats) {
	known := map[string]bool{}
	for _, database := range records {
		known[database.GetString("name")] = true
	}
	collection, err := p.App.FindCachedCollectionByNameOrId(schema.Databases)
	if err != nil {
		return
	}
	names := make([]string, 0, len(result.Databases))
	for name := range result.Databases {
		names = append(names, name)
	}
	slices.Sort(names)
	ctx := audit.WithActor(context.Background(), dbActor)
	for _, name := range names {
		if known[name] || databases.ValidTemporaryName(name) != nil {
			continue
		}
		until := time.Now().UTC().Add(orphanLife)
		database := core.NewRecord(collection)
		database.Load(map[string]any{
			"server": server.Id, "name": name, "status": dbReady, "owner_kind": "none", "temporary_until": until,
			"size_bytes": result.Databases[name].SizeBytes, "connections": result.Databases[name].Connections,
		})
		if err := p.App.SaveWithContext(ctx, database); err != nil {
			p.Log.Error("failed to keep a temporary database", "server", server.Id, "name", name, "error", err)
			continue
		}
		events.Log(p.App, server.GetString("host"), backup.EventTemporaryFound, schema.SeverityInfo,
			fmt.Sprintf("The server %s holds the temporary database %s, which the panel had no record of: it is dropped at %s UTC.",
				server.GetString("name"), name, until.Format("2006-01-02 15:04")),
			map[string]any{"database": database.Id, "server": server.Id})
	}
}

// temporaryCopies are the temporary databases of a server that hold a copy
// of one of its databases, by the restore or the restore test their names
// name: what the database's user owns there, which goes before the user.
func (p *Panel) temporaryCopies(server, database *core.Record) []*core.Record {
	records, err := p.App.FindRecordsByFilter(schema.Databases, "server={:server} && temporary_until!=''", "name", 0, 0,
		dbx.Params{"server": server.Id})
	if err != nil {
		return nil
	}
	copies := []*core.Record{}
	for _, record := range records {
		prefix, id, ok := databases.TemporaryOwner(record.GetString("name"))
		if !ok {
			continue
		}
		if prefix == databases.TemporaryTest {
			row, err := p.App.FindRecordById(schema.BackupChecks, id)
			if err == nil && row.GetString("database_name") == database.GetString("name") && row.GetString("host") == server.GetString("host") {
				copies = append(copies, record)
			}
			continue
		}
		restore, err := p.App.FindRecordById(schema.BackupRestores, id)
		if err != nil {
			continue
		}
		if root, ok := databaseRoot(restore); ok && root.Container == server.GetString("container_ref") && root.Database == database.GetString("name") {
			copies = append(copies, record)
		}
	}
	return copies
}

// latestUntil is the latest time a temporary database of a list goes.
func latestUntil(records []*core.Record) time.Time {
	latest := time.Time{}
	for _, record := range records {
		latest = maxTime(latest, record.GetDateTime("temporary_until").Time())
	}
	return latest.UTC()
}

// dropTemporary drops a temporary database now, for an operator: 202 and its
// discard, unless its restore or restore test is still running.
func (p *Panel) dropTemporary(e *core.RequestEvent, database *core.Record) error {
	if databases.ValidTemporaryName(database.GetString("name")) != nil {
		return e.BadRequestError(temporaryMessage, nil)
	}
	if database.GetString("status") == dbDropping {
		return e.BadRequestError("The database is being dropped.", nil)
	}
	if held := p.temporaryHeld(database); held != "" {
		return e.Error(http.StatusConflict, held, nil)
	}
	server, err := p.App.FindRecordById(schema.DBServers, database.GetString("server"))
	if err != nil {
		return e.NotFoundError("The database server does not exist.", err)
	}
	userID, label := requester(e)
	command, err := p.discardTemporary(database, server, userID, label)
	if errors.Is(err, errUnchanged) {
		return e.BadRequestError("The database is being dropped.", nil)
	}
	if err != nil {
		return e.InternalServerError("Failed to queue the drop.", err)
	}
	return e.JSON(http.StatusAccepted, map[string]any{"database": database.Id, "command": command})
}

// openOnServer says whether a restore of a database, or a restore test, of a
// server is queued or running.
func (p *Panel) openOnServer(server *core.Record) bool {
	for _, restore := range p.openRestores(server.GetString("host")) {
		if root, ok := databaseRoot(restore); ok && root.Container == server.GetString("container_ref") {
			return true
		}
	}
	tests, err := p.App.FindRecordsByFilter(schema.BackupChecks, "kind={:kind} && host={:host} && (status={:queued} || status={:running})", "", 0, 0,
		dbx.Params{"kind": schema.CheckRestoreTest, "host": server.GetString("host"), "queued": schema.CheckQueued, "running": schema.CheckRunning})
	if err != nil {
		return true
	}
	for _, row := range tests {
		if _, err := p.databaseOf(server, row.GetString("temporary")); err == nil {
			return true
		}
	}
	return false
}
