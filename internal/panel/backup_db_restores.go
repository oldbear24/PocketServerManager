package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// Database restores (#53) put a database job's dump back with the shared
// server's own client, as the database's own user, so that no restore can
// write another database of the server: back into the database, with what
// uses it stopped and its name typed to confirm, or into a new database with
// a user of its own. They are #30's restores, a draft, its preview, a start
// and the agent's report; what is new is where they write, the plan in their
// one root, and the temporary databases a restore in place makes, whose
// records are made in the transaction that queues it.

const (
	// restoreCopyLife is how long a restore's temporary copies are kept at
	// most: its time and an hour.
	restoreCopyLife = 13 * time.Hour
	// previousLife is how long the data a PostgreSQL restore in place
	// replaced stays.
	previousLife = 24 * time.Hour
)

// confirmNameMessage asks for the typed confirmation of a restore in place.
const confirmNameMessage = "Type the database's name to confirm that its data is replaced."

// databaseMode tells whether a restore's mode restores a database.
func databaseMode(mode string) bool {
	return mode == schema.RestoreDatabase || mode == schema.RestoreNewDatabase
}

// databaseRoot is a database restore's one root, its plan.
func databaseRoot(restore *core.Record) (restoreRoot, bool) {
	if !databaseMode(restore.GetString("mode")) {
		return restoreRoot{}, false
	}
	roots := rootsOf(restore)
	if len(roots) != 1 {
		return restoreRoot{}, false
	}
	return roots[0], true
}

// databaseRestoreOpen is the started restore that writes a database of a
// server, if any, queued or running: back into it, or into a new database of
// that name.
func (p *Panel) databaseRestoreOpen(server *core.Record, name string) *core.Record {
	for _, restore := range p.openRestores(server.GetString("host")) {
		if root, ok := databaseRoot(restore); ok && root.Container == server.GetString("container_ref") && root.Database == name {
			return restore
		}
	}
	return nil
}

// jobEngine is the engine of a database job's dumps.
func jobEngine(job *core.Record) databases.Engine {
	if job.GetString("engine") == schema.DumpMySQL {
		return databases.MariaDB
	}
	return databases.Postgres
}

// databasePlan works out a database restore: back into the job's database,
// on its server, as its user, stopping its owner stack (the record's
// stack_name), a site's PHP containers and the other stacks of the server
// whose env names the database's secret, with a scratch copy and, on
// PostgreSQL, the copy of the data before it; or into a new database of a
// name on the database's server, or on a shared server of the same engine
// for a dropped database. restoreID names the copies.
func (p *Panel) databasePlan(job, target *core.Record, body restoreBody, restoreID string) (restorePlan, *targetProblem) {
	bad := func(message string) (restorePlan, *targetProblem) {
		return restorePlan{}, &targetProblem{http.StatusBadRequest, message}
	}
	conflict := func(format string, args ...any) (restorePlan, *targetProblem) {
		return restorePlan{}, &targetProblem{http.StatusConflict, fmt.Sprintf(format, args...)}
	}
	if job.GetString("source_kind") != schema.SourceDatabase {
		return bad("Only a database's job restores into a database.")
	}
	if len(body.Include) > 0 || body.Replace {
		return bad("A database is restored whole: pick no items.")
	}
	host, err := p.App.FindRecordById(schema.Hosts, job.GetString("host"))
	if err != nil {
		return conflict("The server of the job is gone.")
	}
	if problem := localTargetElsewhere(p, target, host); problem != nil {
		return restorePlan{}, problem
	}
	var database, server *core.Record
	if id := job.GetString("database"); id != "" {
		if found, err := p.App.FindRecordById(schema.Databases, id); err == nil {
			if own, err := p.App.FindRecordById(schema.DBServers, found.GetString("server")); err == nil {
				database, server = found, own
			}
		}
	}
	name := ""
	switch body.Mode {
	case schema.RestoreDatabase:
		if database == nil {
			return conflict("The database was dropped: restore it into a new database.")
		}
		name = database.GetString("name")
		switch database.GetString("status") {
		case dbReady:
		case dbDropping:
			return conflict("The database %s is being dropped.", name)
		default:
			return conflict("The database %s is not ready.", name)
		}
	default:
		name = strings.TrimSpace(body.Name)
		if database != nil {
			if body.Server != "" && body.Server != server.Id {
				return bad("A new database of this job goes on the server of its database.")
			}
		} else {
			found, err := p.App.FindRecordById(schema.DBServers, body.Server)
			if body.Server == "" || err != nil {
				return bad("Choose the database server the new database goes on.")
			}
			server = found
		}
	}
	if server.GetString("kind") != schema.DBServerShared || server.GetString("host") != host.Id {
		return bad("A database is restored onto a shared server of the job's server.")
	}
	engine, ok := databases.EngineOf(server.GetString("engine"))
	if !ok || engine.Name != jobEngine(job).Name {
		return bad(fmt.Sprintf("The server %s runs another engine than the job's dumps are of.", server.GetString("name")))
	}
	if server.GetString("status") != dbReady {
		return conflict("The database server %s is not ready.", server.GetString("name"))
	}
	if server.GetString("admin_password") == "" {
		return conflict("The panel does not know the credentials of %s.", server.GetString("name"))
	}
	if !slices.Contains(host.GetStringSlice("capabilities"), backup.CapabilityDatabase) {
		return conflict("The agent of %s cannot restore databases yet: update it.", host.GetString("name"))
	}

	root := restoreRoot{
		Source: "/" + databases.BackupFile(engine, job.GetString("source")), Container: server.GetString("container_ref"),
		Engine: engine.Name, AdminUser: server.GetString("admin_user"), Database: name,
	}
	plan := restorePlan{host: host, databaseName: name}
	if body.Mode == schema.RestoreNewDatabase {
		if err := databases.ValidDatabaseName(engine, name); err != nil {
			return bad(sentence(err))
		}
		if databases.ReservedPrefix(name) != nil {
			return bad(databases.ReservedPrefixProblem)
		}
		if holder := p.databaseNameTaken(server, name); holder != "" {
			return conflict("A database named %s exists on %s already.", name, holder)
		}
		if reason := p.claimedByMove(server, name, ""); reason != "" {
			return conflict("%s", sentenceText(reason))
		}
		if problem := p.olderServer(job, server, engine); problem != "" {
			return bad(problem)
		}
		root.User = name
	} else {
		for _, move := range p.movesOfServer(server) {
			if moveNames(move, name) {
				return conflict("This database is moving to %s.", move.GetString("target_name"))
			}
		}
		owner, err := p.ownerOf(server, database)
		if err != nil {
			return conflict("The database %s has no user the panel knows.", name)
		}
		if owner.GetString("pending_password") != "" {
			return conflict("A rotation of %s's password is under way.", owner.GetString("name"))
		}
		root.User = owner.GetString("name")
		plan.database = database.Id
		if stack, ok := p.stackOf(database); ok {
			plan.stack = stack.GetString("name")
			if p.restoreHolding(stack) != nil {
				return conflict("%s", restoreHoldMessage(plan.stack))
			}
		}
		if site, err := p.App.FindRecordById(schema.Sites, database.GetString("site")); err == nil && database.GetString("site") != "" {
			root.Site = site.GetString("name")
			root.StopContainers = []string{root.Site + "-php", root.Site + "-worker"}
		}
		root.StopProjects = p.projectsUsing(host.Id, owner, name, plan.stack)
		root.Scratch = databases.ScratchName(restoreID)
		if engine.Name == databases.Postgres.Name {
			root.Previous = databases.PreviousName(restoreID)
		}
	}
	if p.databaseRestoreOpen(server, name) != nil {
		return conflict("A restore of %s is under way.", name)
	}
	if p.databaseTestOpen(server, name) != nil {
		return conflict("A restore test of %s is under way.", name)
	}
	plan.roots = []restoreRoot{root}
	return plan, nil
}

// databaseNameTaken says which server has a name a new database may not take
// on a server, if one has: a database or a user of the server has it, or its
// secret is another database's on the host, which a database's name is found
// by.
func (p *Panel) databaseNameTaken(server *core.Record, name string) string {
	if _, err := p.databaseOf(server, name); err == nil {
		return server.GetString("name")
	}
	if _, err := p.userOf(server, name); err == nil {
		return server.GetString("name")
	}
	used, err := p.App.FindRecordsByFilter(schema.DBUsers, "secret_name={:secret} && server.host={:host}", "", 1, 0,
		dbx.Params{"secret": databases.SecretName(name), "host": server.GetString("host")})
	if err != nil {
		return server.GetString("name")
	}
	if len(used) > 0 {
		if other, err := p.App.FindRecordById(schema.DBServers, used[0].GetString("server")); err == nil {
			return other.GetString("name")
		}
		return server.GetString("name")
	}
	return ""
}

// olderServer says why a dropped database's PostgreSQL dump does not go into
// a server, if it does not: the server it was made on is still there and
// runs a newer major version, whose dumps an older pg_restore does not read.
func (p *Panel) olderServer(job, server *core.Record, engine databases.Engine) string {
	if engine.Name != databases.Postgres.Name {
		return ""
	}
	source, err := p.App.FindFirstRecordByFilter(schema.DBServers, "host={:host} && name={:name}",
		dbx.Params{"host": job.GetString("host"), "name": job.GetString("db_server_name")})
	if err != nil || source.Id == server.Id {
		return ""
	}
	made, into := databases.MajorVersion(versionOf(source)), databases.MajorVersion(versionOf(server))
	if made == 0 || into == 0 || into >= made {
		return ""
	}
	return fmt.Sprintf("The server %s runs PostgreSQL %d, older than the %d the backup was made with: restore into a server of the same or a newer version.",
		server.GetString("name"), into, made)
}

// projectsUsing are the stacks of a server, but its own, whose env names a
// database's secret, which its user carries: they stop while the database is
// restored in place, as they would write it meanwhile.
func (p *Panel) projectsUsing(hostID string, owner *core.Record, name, own string) []string {
	if owner.GetString("secret_name") != databases.SecretName(name) {
		return nil
	}
	stacks, err := p.App.FindRecordsByFilter(schema.Stacks, "host={:host} && source!={:server}", "name", 0, 0,
		dbx.Params{"host": hostID, "server": stackSourceDBServer})
	if err != nil {
		return nil
	}
	reference := "${secret:" + databases.SecretName(name) + "}"
	projects := []string{}
	for _, stack := range stacks {
		if stack.GetString("name") != own && strings.Contains(stack.GetString("env"), reference) {
			projects = append(projects, stack.GetString("name"))
		}
	}
	if len(projects) == 0 {
		return nil
	}
	return projects
}

// databaseRestorePayload is a database restore's BackupDbRestore, from its
// plan.
func databaseRestorePayload(restore *core.Record, root restoreRoot) *agentv1.BackupDbRestore {
	mode := backup.DbRestoreReplace
	if restore.GetString("mode") == schema.RestoreNewDatabase {
		mode = backup.DbRestoreNew
	}
	return &agentv1.BackupDbRestore{
		Connection:     &agentv1.DbConnection{ContainerId: root.Container, Engine: root.Engine, AdminUser: root.AdminUser},
		Database:       root.Database,
		File:           strings.TrimPrefix(root.Source, "/"),
		Mode:           mode,
		User:           root.User,
		Scratch:        root.Scratch,
		Previous:       root.Previous,
		StopProjects:   root.StopProjects,
		StopContainers: root.StopContainers,
		Site:           root.Site,
	}
}

// databaseDestination is what a database restore writes, as its ordering
// key names it: a database of a server's container.
func databaseDestination(root restoreRoot) string {
	return root.Container + "/" + root.Database
}

// dbPreviewOf is a database restore's preview, as the agent answered it.
func dbPreviewOf(restore *core.Record) (backup.DbRestorePreview, bool) {
	var preview backup.DbRestorePreview
	raw := restore.GetString("preview")
	if raw == "" || raw == "null" {
		return preview, false
	}
	return preview, restore.UnmarshalJSONField("preview", &preview) == nil
}

// sizeText says a size in bytes as people read it; nothing below zero.
func sizeText(n int64) string {
	return bytesText(uint64(max(n, 0))) //nolint:gosec // not negative
}

// databaseStart checks the start of a database restore: a restore in place
// needs its database's name typed; the preview found the dump, the user
// signed in, and the server has room; and the plan, worked out again, is
// the one that was previewed: what it writes, as whom, and what it stops.
func (p *Panel) databaseStart(restore, job, target *core.Record, confirmOverwrite bool, confirmName string) *targetProblem {
	conflict := func(format string, args ...any) *targetProblem {
		return &targetProblem{http.StatusConflict, fmt.Sprintf(format, args...)}
	}
	mode := restore.GetString("mode")
	name := restore.GetString("database_name")
	if mode == schema.RestoreDatabase && (!confirmOverwrite || confirmName != name) {
		return conflict(confirmNameMessage)
	}
	root, ok := databaseRoot(restore)
	if !ok {
		return conflict("The restore has no plan: preview it again.")
	}
	server, err := p.serverByContainer(restore.GetString("host"), root.Container)
	if err != nil {
		return conflict("The database's server is gone.")
	}
	preview, ok := dbPreviewOf(restore)
	if !ok {
		return conflict("The preview is not ready yet.")
	}
	switch {
	case len(preview.Roots) == 0 || !preview.Roots[0].InSnapshot:
		return conflict("The dump is not in this snapshot.")
	case mode == schema.RestoreDatabase && !preview.Database.SignIn:
		return conflict("The panel's password of %s no longer signs in on %s: restore into a new database.", root.User, server.GetString("name"))
	case mode == schema.RestoreNewDatabase && (preview.Database.Exists || preview.Database.UserExists):
		return conflict("A database named %s exists on %s already.", name, server.GetString("name"))
	case preview.Needed.Data > preview.Free.Data:
		return conflict("The server %s has %s free, and the restore needs about %s.", server.GetString("name"), sizeText(preview.Free.Data), sizeText(preview.Needed.Data))
	case preview.Needed.Tmp > preview.Free.Tmp:
		return conflict("The server %s has %s free, and the restore needs about %s.", server.GetString("name"), sizeText(preview.Free.Tmp), sizeText(preview.Needed.Tmp))
	}
	body := restoreBody{Target: target.Id, Snapshot: restore.GetString("snapshot_id"), Mode: mode, Server: server.Id}
	if mode == schema.RestoreNewDatabase {
		body.Name = name
	}
	plan, problem := p.databasePlan(job, target, body, restore.Id)
	if problem != nil {
		return problem
	}
	if !reflect.DeepEqual(plan.roots, rootsOf(restore)) || plan.database != restore.GetString("database") || plan.stack != restore.GetString("stack_name") {
		return conflict("What the restore writes or stops changed since its preview: preview it again.")
	}
	return nil
}

// restoreRecordsIn saves, in the transaction that queues a database
// restore, the records of the databases it makes: a restore in place's
// temporary copies, which the panel drops once their time comes, or a new
// database's record and its user's, with a new password. ctx carries who
// asked for the new database; the copies are the panel's own.
func (p *Panel) restoreRecordsIn(ctx context.Context, tx core.App, restore *core.Record, root restoreRoot) error {
	server, err := tx.FindFirstRecordByFilter(schema.DBServers, "host={:host} && container_ref={:container}",
		dbx.Params{"host": restore.GetString("host"), "container": root.Container})
	if err != nil {
		return err
	}
	if restore.GetString("mode") == schema.RestoreNewDatabase {
		database, err := databaseRecordsIn(ctx, tx, server, root.Database, "", "")
		if err != nil {
			return err
		}
		restore.Set("database", database.Id)
		return nil
	}
	collection, err := tx.FindCachedCollectionByNameOrId(schema.Databases)
	if err != nil {
		return err
	}
	until := time.Now().UTC().Add(restoreCopyLife)
	for _, name := range []string{root.Scratch, root.Previous} {
		if name == "" {
			continue
		}
		if err := databases.ValidTemporaryName(name); err != nil {
			return err
		}
		copied := core.NewRecord(collection)
		copied.Load(map[string]any{"server": server.Id, "name": name, "status": dbCreating, "owner_kind": "none", "temporary_until": until})
		if err := tx.SaveWithContext(audit.WithActor(ctx, dbActor), copied); err != nil {
			return err
		}
	}
	return nil
}

// databaseRestored follows a database restore that ended (command, or nil
// when its command is gone): its temporary copies, the database it made, and
// the database it left part restored.
func (p *Panel) databaseRestored(restore, command *core.Record) {
	root, ok := databaseRoot(restore)
	if !ok {
		return
	}
	restore, err := p.App.FindRecordById(schema.BackupRestores, restore.Id)
	if err != nil {
		return
	}
	server, err := p.serverByContainer(restore.GetString("host"), root.Container)
	if err != nil {
		return
	}
	var result backup.DbRestoreResult
	neverStarted := false
	if command != nil {
		raw, _ := json.Marshal(command.Get("result"))
		_ = json.Unmarshal(raw, &result)
		stored := strings.TrimSpace(command.GetString("result"))
		hasResult := stored != "" && stored != "null" && stored != "{}"
		neverStarted = !hasResult && command.GetDateTime("started_at").IsZero()
	}
	finished := types.NowDateTime()
	if command != nil && !command.GetDateTime("finished_at").IsZero() {
		finished = command.GetDateTime("finished_at")
	}
	succeeded := restore.GetString("status") == schema.RestoreSucceeded
	ctx := audit.WithActor(context.Background(), dbActor)
	remove := func(record *core.Record) {
		if err := p.App.DeleteWithContext(ctx, record); err != nil {
			p.Log.Error("failed to remove a database record", "database", record.Id, "error", err)
		}
	}

	if restore.GetString("mode") == schema.RestoreNewDatabase {
		database, err := p.App.FindRecordById(schema.Databases, restore.GetString("database"))
		if err != nil {
			return
		}
		switch {
		case succeeded:
			p.setFields(database, map[string]any{"status": dbReady, "last_error": ""})
			p.statsLater(server)
		case result.Dropped || neverStarted:
			// nothing of it is left on the server
			if account, err := p.userOf(server, root.User); err == nil {
				remove(account)
			}
			remove(database)
		default:
			p.setFields(database, map[string]any{"status": dbFailed, "last_error": cutText(restore.GetString("error"), 2048)})
		}
		return
	}

	scratch, _ := p.databaseOf(server, root.Scratch)
	var previous *core.Record
	if root.Previous != "" {
		previous, _ = p.databaseOf(server, root.Previous)
	}
	if scratch != nil && !isTemporary(scratch) {
		scratch = nil
	}
	if previous != nil && !isTemporary(previous) {
		previous = nil
	}
	switch {
	case succeeded:
		// the scratch copy is the database now, or was dropped after the
		// rehearsal; the data before it stays a day, closed
		if scratch != nil {
			remove(scratch)
		}
		if previous != nil {
			p.setFields(previous, map[string]any{"status": dbReady, "temporary_until": finished.Time().Add(previousLife)})
		}
		p.statsLater(server)
	case neverStarted:
		for _, record := range []*core.Record{scratch, previous} {
			if record != nil {
				remove(record)
			}
		}
	default:
		// the sweep drops the scratch copy; the data before the restore
		// stays a day when the swap happened, and was never there before it
		if scratch != nil {
			p.setFields(scratch, map[string]any{"status": dbReady, "temporary_until": time.Now().UTC()})
		}
		if previous != nil {
			if result.Previous != "" || command == nil {
				p.setFields(previous, map[string]any{"status": dbReady, "temporary_until": finished.Time().Add(previousLife)})
			} else {
				remove(previous)
			}
		}
		if result.Partial {
			if database, err := p.App.FindRecordById(schema.Databases, restore.GetString("database")); err == nil {
				p.setFields(database, map[string]any{"last_error": cutText(restore.GetString("error"), 2048)})
			}
		}
	}
}

// statsLater asks a server for its numbers once a restore changed a
// database's size.
func (p *Panel) statsLater(server *core.Record) {
	if _, err := p.queueStats(context.Background(), server, ""); err != nil && !errors.Is(err, errStatsRunning) {
		p.Log.Debug("failed to ask a database server for its numbers", "server", server.Id, "error", err)
	}
}

// databaseSecretValues are the passwords of a database restore, its server's
// administrator's and its user's, to hide from what is kept.
func (p *Panel) databaseSecretValues(restore *core.Record) []string {
	root, ok := databaseRoot(restore)
	if !ok {
		return nil
	}
	server, err := p.serverByContainer(restore.GetString("host"), root.Container)
	if err != nil {
		return nil
	}
	return p.serverSecrets(server, root.User)
}

// serverSecrets are a server's administrator's password and a user's.
func (p *Panel) serverSecrets(server *core.Record, user string) []string {
	secrets := []string{}
	if admin, err := p.sealed(server, "admin_password"); err == nil && admin != "" {
		secrets = append(secrets, admin)
	}
	if account, err := p.userOf(server, user); err == nil {
		for _, field := range []string{"password", "pending_password"} {
			if password, err := p.sealed(account, field); err == nil && password != "" {
				secrets = append(secrets, password)
			}
		}
	}
	return secrets
}

// checkDatabaseDelivery checks a database restore's plan as it is
// delivered: its server is still the shared server of its container; back
// into a database, that database is the restore's, on that server, and its
// user still the one the plan restores as; into a new database, the records
// the start made are there.
func (p *Panel) checkDatabaseDelivery(record, restore *core.Record, root restoreRoot, dryRun bool) error {
	server, err := p.serverByContainer(record.GetString("host"), root.Container)
	if err != nil || server.GetString("kind") != schema.DBServerShared {
		return errors.New("the database's server is gone")
	}
	if restore.GetString("mode") == schema.RestoreNewDatabase && dryRun {
		return nil
	}
	database, err := p.App.FindRecordById(schema.Databases, restore.GetString("database"))
	if err != nil || database.GetString("server") != server.Id || database.GetString("name") != root.Database {
		return errors.New("the database is not where the restore was planned")
	}
	owner, err := p.ownerOf(server, database)
	if err != nil || owner.GetString("name") != root.User {
		return errors.New("the database's user is not the one the restore was planned with")
	}
	return nil
}

// databaseRestoreWhere says in words where a database restore writes.
func (p *Panel) databaseRestoreWhere(restore *core.Record, root restoreRoot) string {
	if server, err := p.serverByContainer(restore.GetString("host"), root.Container); err == nil {
		return server.GetString("name") + "/" + root.Database
	}
	return root.Database
}

// siteRestoreMessage says that a site waits for a restore of its database.
func siteRestoreMessage(name string) string {
	return fmt.Sprintf("The site %s is being restored from a backup: this waits until that is done.", name)
}

// restoreHoldingSite is the started restore of a server that stops a site's
// containers, if any: of the site's database, or one whose preview stops its
// PHP container.
func (p *Panel) restoreHoldingSite(hostID, site string) *core.Record {
	if site == "" {
		return nil
	}
	for _, restore := range p.openRestores(hostID) {
		if root, ok := databaseRoot(restore); ok && root.Site == site {
			return restore
		}
		if slices.Contains(confirmedStops(restore), site+"-php") {
			return restore
		}
	}
	return nil
}

// siteRestoring refuses what would deploy, change or remove a site while a
// restore stops it.
func (p *Panel) siteRestoring(e *core.RequestEvent, site *core.Record) error {
	if p.restoreHoldingSite(site.GetString("host"), site.GetString("name")) != nil {
		return e.Error(http.StatusConflict, siteRestoreMessage(site.GetString("name")), nil)
	}
	return nil
}
