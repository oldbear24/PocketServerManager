package panel

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/sites"
	"github.com/oldbear24/PocketServerManager/internal/stacks"
)

// Automatic backup jobs (#53). Every ready database of a shared server gets a
// job of its own (origin and source kind database), and every PHP site, once
// it was deployed, a job <site>-files of its shared/ (origin and source kind
// site). The panel makes them, never the API: what such a job backs up and
// its server follow its database or site, and its schedule, targets and
// retention follow its server's automatic-backup settings (the host's
// auto_backup_* fields), but for those it set for itself (own_settings). The
// job names its database or site by a relation without cascade: when that
// goes, the relation is cleared and the job stays, disabled, with its runs
// and the way to its snapshots. A database of a dedicated server stays its
// stack's business, as its users are. The database and its server are
// resolved at each run, never stored in the job.

// autoJobActor is who makes and changes the automatic jobs.
var autoJobActor = audit.System("backups", "panel")

// What keeps an automatic job whose database or site went from running, and
// what is left to do with it.
const (
	databaseDropped     = "Its database was dropped."
	siteRemoved         = "Its site was removed."
	databaseGoneMessage = "Its database was dropped: restore its snapshots into a new database."
	siteGoneMessage     = "Its site was removed: its snapshots stay, and a restore into a directory brings them back."
)

// siteJobSuffix ends the name of a site's job, as <site>-files.
const siteJobSuffix = "-files"

// defaultAutoRetention is what an automatic job keeps when its server sets
// nothing: 7 daily, 4 weekly and 6 monthly snapshots, as a label's job.
func defaultAutoRetention() stacks.Retention {
	retention, _ := stacks.ParseRetention(stacks.DefaultBackupRetention)
	return retention
}

// registerAutoJobs binds what keeps the automatic jobs: a database that is
// ready, or that moved, a site's first deployment, a database or a site that
// went, and a pass at the panel's start and every hour that makes what a
// hook missed and repairs the rest. Each hook works after the change, as the
// label jobs do: a change of a database never waits for its job.
func (p *Panel) registerAutoJobs() {
	p.App.OnRecordAfterCreateSuccess(schema.Databases).BindFunc(func(e *core.RecordEvent) error {
		id := e.Record.Id
		p.later.after(0, func() { p.logAutoJob("database", id, p.ensureDatabaseJob(id)) })
		return e.Next()
	})
	p.App.OnRecordAfterUpdateSuccess(schema.Databases).BindFunc(func(e *core.RecordEvent) error {
		database, before := e.Record, e.Record.Original()
		// a database whose stats changed changes nothing of its job
		changed := false
		for _, field := range []string{"status", "server", "temporary_until", "name"} {
			if database.GetString(field) != before.GetString(field) {
				changed = true
			}
		}
		if changed {
			id := database.Id
			p.later.after(0, func() { p.logAutoJob("database", id, p.ensureDatabaseJob(id)) })
		}
		return e.Next()
	})
	p.App.OnRecordAfterUpdateSuccess(schema.Sites).BindFunc(func(e *core.RecordEvent) error {
		if e.Record.GetString("active_deployment") != "" && e.Record.Original().GetString("active_deployment") == "" {
			id := e.Record.Id
			p.later.after(0, func() { p.logAutoJob("site", id, p.ensureSiteJob(id)) })
		}
		return e.Next()
	})
	// a site that turns classic, or back, backs up another directory, which
	// its job's source says
	p.App.OnRecordAfterUpdateSuccess(schema.SitePHP).BindFunc(func(e *core.RecordEvent) error {
		if e.Record.GetString("mode") != e.Record.Original().GetString("mode") {
			id := e.Record.GetString("site")
			p.later.after(0, func() { p.logAutoJob("site", id, p.ensureSiteJob(id)) })
		}
		return e.Next()
	})
	// a cascade deletes with app.Delete too, so a server's or a host's
	// databases and sites come here as well
	for _, collection := range []string{schema.Databases, schema.Sites} {
		p.App.OnRecordAfterDeleteSuccess(collection).BindFunc(func(e *core.RecordEvent) error {
			p.later.after(0, p.markGoneAutoJobs)
			return e.Next()
		})
	}
	// once the panel serves, which takes its lock first (#32)
	p.App.OnServe().BindFunc(func(e *core.ServeEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		p.later.after(0, p.reconcileAutoJobs)
		return nil
	})
	p.App.Cron().MustAdd("pocketcloud_auto_backup_jobs", "23 * * * *", p.reconcileAutoJobs)
}

// logAutoJob logs what kept an automatic job from being made or kept.
func (p *Panel) logAutoJob(kind, id string, err error) {
	if err != nil {
		p.Log.Error("failed to keep the backup job of a "+kind, kind, id, "error", err)
	}
}

// autoOrigin tells whether the panel made a job for a database or a site.
func autoOrigin(job *core.Record) bool {
	origin := job.GetString("origin")
	return origin == schema.JobDatabase || origin == schema.JobSite
}

// autoGone tells whether an automatic job lost its database or its site.
func autoGone(job *core.Record) bool {
	switch job.GetString("origin") {
	case schema.JobDatabase:
		return job.GetString("database") == ""
	case schema.JobSite:
		return job.GetString("site") == ""
	}
	return false
}

// sourceGone tells whether a job lost what makes it: a label's job its label
// or its stack, an automatic job its database or its site.
func sourceGone(job *core.Record) bool {
	return labelGone(job) || autoGone(job)
}

// goneMessage says what is left to do with a job whose source is gone.
func goneMessage(job *core.Record) string {
	switch job.GetString("origin") {
	case schema.JobDatabase:
		return databaseGoneMessage
	case schema.JobSite:
		return siteGoneMessage
	}
	return labelGoneMessage
}

// goneProblem is the problem of an automatic job whose source is gone.
func goneProblem(job *core.Record) string {
	if job.GetString("origin") == schema.JobSite {
		return siteRemoved
	}
	return databaseDropped
}

// autoSettings are what a server's automatic jobs follow: its schedule, or
// the staggered daily default; its targets, none until an admin picks them;
// its retention, or 7 daily, 4 weekly and 6 monthly.
type autoSettings struct {
	schedule  string
	targets   []string
	retention stacks.Retention
}

// autoSettingsOf reads a server's automatic-backup settings, as the app (a
// transaction's) sees them. A target that is gone, or a local target of
// another server, is left out.
func autoSettingsOf(app core.App, host *core.Record) autoSettings {
	settings := autoSettings{schedule: stacks.NormalSchedule(host.GetString("auto_backup_schedule")), targets: []string{}}
	if settings.schedule == "" {
		settings.schedule = backup.AutoSchedule(host.Id)
	}
	ids := host.GetStringSlice("auto_backup_targets")
	if found, err := app.FindRecordsByIds(schema.BackupTargets, ids); err == nil {
		slices.SortFunc(found, func(a, b *core.Record) int { return slices.Index(ids, a.Id) - slices.Index(ids, b.Id) })
		for _, target := range found {
			if target.GetString("kind") == backup.KindLocal && target.GetString("host") != host.Id {
				continue
			}
			settings.targets = append(settings.targets, target.Id)
		}
	}
	settings.retention = hostRetention(host)
	return settings
}

// hostRetention is a server's retention of automatic jobs: null keeps the
// default.
func hostRetention(host *core.Record) stacks.Retention {
	var retention *stacks.Retention
	if err := host.UnmarshalJSONField("auto_backup_retention", &retention); err != nil || retention == nil || !retention.Valid() {
		return defaultAutoRetention()
	}
	return *retention
}

// ownSettings are the settings a job set for itself, in a fixed order.
func ownSettings(job *core.Record) []string {
	own := []string{}
	for _, field := range []string{schema.OwnSchedule, schema.OwnTargets, schema.OwnRetention} {
		if slices.Contains(job.GetStringSlice("own_settings"), field) {
			own = append(own, field)
		}
	}
	return own
}

// followSettings gives a job its server's settings, but those it set for
// itself.
func followSettings(job *core.Record, settings autoSettings) {
	own := ownSettings(job)
	if !slices.Contains(own, schema.OwnSchedule) {
		job.Set("schedule", settings.schedule)
	}
	if !slices.Contains(own, schema.OwnTargets) {
		job.Set("targets", slices.Clone(settings.targets))
	}
	if !slices.Contains(own, schema.OwnRetention) {
		job.Set("retention", settings.retention)
	}
}

// databaseJobEngine is a database job's engine, as dumps name them.
func databaseJobEngine(server *core.Record) string {
	if server.GetString("engine") == schema.EngineMariaDB {
		return schema.DumpMySQL
	}
	return schema.DumpPostgres
}

// temporaryName tells whether a database's name is of PocketCloud's own
// temporary databases, or starts as they do.
func temporaryName(name string) bool {
	return schema.TemporaryDatabase.MatchString(name) || databases.ReservedPrefix(name) != nil
}

// wantsDatabaseJob tells whether a database has a job of its own: it is
// ready, no temporary database, and on a shared server.
func wantsDatabaseJob(database, server *core.Record) bool {
	return database.GetString("status") == dbReady && database.GetDateTime("temporary_until").IsZero() &&
		!temporaryName(database.GetString("name")) && server.GetString("kind") == schema.DBServerShared
}

// ensureDatabaseJob makes a database's job, when it has none and wants one;
// a job it has follows its server's name, as after a move.
func (p *Panel) ensureDatabaseJob(id string) error {
	database, err := p.App.FindRecordById(schema.Databases, id)
	if err != nil {
		// dropped meanwhile
		return nil //nolint:nilerr // nothing to make
	}
	server, err := p.App.FindRecordById(schema.DBServers, database.GetString("server"))
	if err != nil {
		return nil //nolint:nilerr // nothing to make
	}
	if job, err := p.App.FindFirstRecordByData(schema.BackupJobs, "database", database.Id); err == nil {
		return p.refreshDatabaseJob(job, server)
	}
	if !wantsDatabaseJob(database, server) {
		return nil
	}
	host, err := p.App.FindRecordById(schema.Hosts, server.GetString("host"))
	if err != nil {
		return err
	}
	fields := map[string]any{
		"host": host.Id, "source_kind": schema.SourceDatabase, "source": database.GetString("name"),
		"engine": databaseJobEngine(server), "database": database.Id, "db_server_name": server.GetString("name"),
		"origin": schema.JobDatabase,
	}
	return p.createAutoJob(server.GetString("name")+"-"+database.GetString("name"), host, fields, "database", database.Id)
}

// refreshDatabaseJob keeps a database's job on its server's name and engine:
// a move keeps the database's record, and its job, on another server.
func (p *Panel) refreshDatabaseJob(job, server *core.Record) error {
	if job.GetString("db_server_name") == server.GetString("name") && job.GetString("engine") == databaseJobEngine(server) {
		return nil
	}
	return p.update(audit.WithActor(context.Background(), autoJobActor), job, func(fresh *core.Record) error {
		if fresh.GetString("db_server_name") == server.GetString("name") && fresh.GetString("engine") == databaseJobEngine(server) {
			return errUnchanged
		}
		fresh.Set("db_server_name", server.GetString("name"))
		fresh.Set("engine", databaseJobEngine(server))
		return nil
	})
}

// siteJobSource is the directory of the sites' volume that a PHP site's
// job backs up, which its source says: the site's shared/, as <site>/shared,
// or a classic site's whole directory, as <site>.
func siteJobSource(site, settings *core.Record) string {
	if settings.GetString("mode") == schema.PHPClassic {
		return site.GetString("name")
	}
	return site.GetString("name") + "/shared"
}

// ensureSiteJob makes a PHP site's job <site>-files once the site was
// deployed: before, it has no shared/ to back up. A job it has follows the
// site's mode in its source.
func (p *Panel) ensureSiteJob(id string) error {
	site, err := p.App.FindRecordById(schema.Sites, id)
	if err != nil || site.GetString("active_deployment") == "" {
		return nil //nolint:nilerr // nothing to make
	}
	settings, err := p.App.FindFirstRecordByData(schema.SitePHP, "site", site.Id)
	if err != nil {
		return nil //nolint:nilerr // a static site keeps nothing of its own
	}
	source := siteJobSource(site, settings)
	if job, err := p.App.FindFirstRecordByData(schema.BackupJobs, "site", site.Id); err == nil {
		if job.GetString("source") == source {
			return nil
		}
		return p.update(audit.WithActor(context.Background(), autoJobActor), job, func(fresh *core.Record) error {
			if fresh.GetString("source") == source {
				return errUnchanged
			}
			fresh.Set("source", source)
			return nil
		})
	}
	host, err := p.App.FindRecordById(schema.Hosts, site.GetString("host"))
	if err != nil {
		return err
	}
	fields := map[string]any{
		"host": host.Id, "source_kind": schema.SourceSite, "source": source, "site": site.Id,
		"origin": schema.JobSite,
	}
	return p.createAutoJob(site.GetString("name")+siteJobSuffix, host, fields, "site", site.Id)
}

// createAutoJob saves a new automatic job under a free name, with its
// server's settings. A unique violation is a hook or a pass that made the job
// meanwhile, or took the name: the job made stays, and another name is tried.
func (p *Panel) createAutoJob(name string, host *core.Record, fields map[string]any, relation, id string) error {
	collection, err := p.App.FindCachedCollectionByNameOrId(schema.BackupJobs)
	if err != nil {
		return err
	}
	for range 3 {
		job := core.NewRecord(collection)
		job.Load(fields)
		job.Set("name", p.freeJobName(name))
		job.Set("enabled", true)
		job.Set("own_settings", []string{})
		followSettings(job, autoSettingsOf(p.App, host))
		job.Set("problem", jobProblem(job))
		job.Set("next_run_at", nextRunOf(job, time.Now()))
		err = p.App.SaveWithContext(audit.WithActor(context.Background(), autoJobActor), job)
		if !uniqueViolation(err) {
			return err
		}
		if _, findErr := p.App.FindFirstRecordByData(schema.BackupJobs, relation, id); findErr == nil {
			return nil
		}
	}
	return err
}

// markGoneAutoJobs disables the automatic jobs whose database or site went,
// saying why, and says so in the events. Their snapshots stay.
func (p *Panel) markGoneAutoJobs() {
	records, err := p.App.FindRecordsByFilter(schema.BackupJobs,
		"((origin={:database} && database='' && (enabled=true || problem!={:dropped})) || (origin={:site} && site='' && (enabled=true || problem!={:removed})))", "", 0, 0,
		dbx.Params{"database": schema.JobDatabase, "site": schema.JobSite, "dropped": databaseDropped, "removed": siteRemoved})
	if err != nil {
		p.Log.Error("failed to read the automatic backup jobs", "error", err)
		return
	}
	for _, record := range records {
		disabled := false
		err := p.update(audit.WithActor(context.Background(), autoJobActor), record, func(fresh *core.Record) error {
			if !autoGone(fresh) || (!fresh.GetBool("enabled") && fresh.GetString("problem") == goneProblem(fresh)) {
				return errUnchanged
			}
			disabled = fresh.GetBool("enabled")
			fresh.Set("enabled", false)
			fresh.Set("next_run_at", "")
			fresh.Set("problem", goneProblem(fresh))
			return nil
		})
		if err != nil {
			p.Log.Error("failed to disable a backup job whose source went", "job", record.Id, "error", err)
			continue
		}
		if disabled {
			what := "its database was dropped"
			if record.GetString("origin") == schema.JobSite {
				what = "its site was removed"
			}
			events.Log(p.App, record.GetString("host"), backup.EventJobDisabled, schema.SeverityWarning,
				fmt.Sprintf("The backup job %s was disabled: %s. Its snapshots stay.", record.GetString("name"), what),
				map[string]any{"job": record.Id})
		}
	}
}

// reconcileAutoJobs makes the automatic jobs that a hook missed, as for
// databases and sites that came before #53 or while the panel was down,
// keeps the database jobs on their servers' names, and disables those whose
// source went.
func (p *Panel) reconcileAutoJobs() {
	if records, err := p.App.FindAllRecords(schema.Databases); err == nil {
		for _, database := range records {
			// one that has its job keeps it on its server's name
			p.logAutoJob("database", database.Id, p.ensureDatabaseJob(database.Id))
		}
	} else {
		p.Log.Error("failed to read the databases", "error", err)
	}
	if sites, err := p.App.FindRecordsByFilter(schema.Sites, "active_deployment!=''", "", 0, 0); err == nil {
		for _, site := range sites {
			p.logAutoJob("site", site.Id, p.ensureSiteJob(site.Id))
		}
	} else {
		p.Log.Error("failed to read the sites", "error", err)
	}
	p.markGoneAutoJobs()
}

// autoChangeProblem is why a change of an automatic job is refused: what it
// backs up and its server are its database's or its site's, and one whose
// source went does not run again.
func autoChangeProblem(job *core.Record, body jobBody) string {
	for field, value := range map[string]*string{
		"source_kind": body.SourceKind, "source": body.Source, "host": body.Host,
		"stack": body.Stack, "service": body.Service, "engine": body.Engine,
	} {
		if value != nil && strings.TrimSpace(*value) != job.GetString(field) {
			if job.GetString("origin") == schema.JobSite {
				return "A site's job always backs up its site: change when, where or how long its backups are kept."
			}
			return "A database's job always backs up its database: change when, where or how long its backups are kept."
		}
	}
	if body.Enabled != nil && *body.Enabled && autoGone(job) {
		if job.GetString("origin") == schema.JobSite {
			return "Its site was removed: the job cannot be turned on again."
		}
		return "Its database was dropped: the job cannot be turned on again."
	}
	if body.Follow != nil {
		for _, field := range *body.Follow {
			if field != schema.OwnSchedule && field != schema.OwnTargets && field != schema.OwnRetention {
				return "A job follows its server's schedule, targets or retention."
			}
		}
	}
	return ""
}

// followOwn sets what a change of an automatic job set for itself, and what
// it follows of its server again: a schedule, targets or retention the body
// sets become the job's own, and those it follows take its server's.
func (p *Panel) followOwn(job *core.Record, body jobBody) error {
	follow := []string{}
	if body.Follow != nil {
		follow = *body.Follow
	}
	own := ownSettings(job)
	for field, set := range map[string]bool{
		schema.OwnSchedule: body.Schedule != nil, schema.OwnTargets: body.Targets != nil, schema.OwnRetention: body.Retention != nil,
	} {
		switch {
		case slices.Contains(follow, field):
			own = slices.DeleteFunc(own, func(f string) bool { return f == field })
		case set && !slices.Contains(own, field):
			own = append(own, field)
		}
	}
	// in the fixed order
	job.Set("own_settings", own)
	job.Set("own_settings", ownSettings(job))
	if len(follow) == 0 {
		return nil
	}
	host, err := p.App.FindRecordById(schema.Hosts, job.GetString("host"))
	if err != nil {
		return errors.New("the server of the job is gone")
	}
	followSettings(job, autoSettingsOf(p.App, host))
	return nil
}

// The runs of automatic jobs (#53). A run resolves the database and its
// server, or the site, as it is made: the job stores neither the container
// nor the files.

// databaseMoved is why a database job's run queued before its database moved
// to another server is not delivered: it names the old server's container.
const databaseMoved = "the database moved to another server since the run was queued"

// sourceRefusal is a run refused for what it backs up, as a database that was
// dropped: the run fails with the class source, which is never tried again.
type sourceRefusal struct{ text string }

func (r sourceRefusal) Error() string { return r.text }

// refusalClass is the failure class of a run that could not be made: source
// for one refused for what it backs up, none otherwise.
func refusalClass(err error) string {
	var refusal sourceRefusal
	if errors.As(err, &refusal) {
		return backup.FailureSource
	}
	return ""
}

// backsUpDatabases tells whether a server's agent backs up single databases
// and PHP sites' files: it says so with the pseudo-capability
// backup.database. An older agent would refuse or leave out what these runs
// need.
func backsUpDatabases(host *core.Record) bool {
	return slices.Contains(host.GetStringSlice("capabilities"), backup.CapabilityDatabase)
}

// agentTooOld says that a server's agent cannot run an automatic job yet.
func agentTooOld(job, host *core.Record) string {
	if job.GetString("source_kind") == schema.SourceSite {
		return fmt.Sprintf("the agent of %s cannot back up PHP sites' files yet: update it", host.GetString("name"))
	}
	return fmt.Sprintf("the agent of %s cannot back up databases one by one yet: update it", host.GetString("name"))
}

// autoRunProblem is why a database's or a site's job cannot run by hand on
// its server now: its agent is too old.
func autoRunProblem(job, host *core.Record) string {
	kind := job.GetString("source_kind")
	if (kind == schema.SourceDatabase || kind == schema.SourceSite) && !backsUpDatabases(host) {
		return sentenceText(agentTooOld(job, host))
	}
	return ""
}

// databaseBusy is why a database job does not run now: its database is
// being made or dropped. Its scheduled runs are skipped without an alert.
func (p *Panel) databaseBusy(job *core.Record) string {
	if job.GetString("source_kind") != schema.SourceDatabase || job.GetString("database") == "" {
		return ""
	}
	database, err := p.App.FindRecordById(schema.Databases, job.GetString("database"))
	if err != nil {
		return ""
	}
	switch database.GetString("status") {
	case dbCreating:
		return fmt.Sprintf("The database %s is being created.", database.GetString("name"))
	case dbDropping:
		return fmt.Sprintf("The database %s is being dropped.", database.GetString("name"))
	}
	return ""
}

// jobDatabase is the database of a database job and its server, as they are
// now; the refusal of a run says why there are none.
func (p *Panel) jobDatabase(job *core.Record) (database, server *core.Record, err error) {
	if job.GetString("database") == "" {
		return nil, nil, sourceRefusal{"the database was dropped"}
	}
	database, err = p.App.FindRecordById(schema.Databases, job.GetString("database"))
	if err != nil {
		return nil, nil, sourceRefusal{"the database was dropped"}
	}
	server, err = p.App.FindRecordById(schema.DBServers, database.GetString("server"))
	if err != nil {
		return nil, nil, sourceRefusal{"the database's server is gone"}
	}
	if server.GetString("kind") != schema.DBServerShared {
		return nil, nil, sourceRefusal{fmt.Sprintf("the database %s is no longer on a shared server", database.GetString("name"))}
	}
	return database, server, nil
}

// databaseDumpOf completes the command of a database job's run: the shared
// server's container, by name, which dumps the one database with the
// server's own client and the password of its own environment, so that the
// run carries none; the dump's file; and the tags of the database and its
// server. The administrator's name and the database's are the dump's
// arguments, never its environment, which stays empty.
func (p *Panel) databaseDumpOf(job, host *core.Record, payload *agentv1.BackupRun) error {
	database, server, err := p.jobDatabase(job)
	if err != nil {
		return err
	}
	if !backsUpDatabases(host) {
		return errors.New(agentTooOld(job, host))
	}
	engine, err := engineOfServer(server)
	if err != nil {
		return err
	}
	admin := server.GetString("admin_user")
	if admin == "" {
		admin = engine.AdminUser
	}
	name := database.GetString("name")
	command, err := databases.DumpCommand(engine, admin, name)
	if err != nil {
		return err
	}
	payload.StdinContainer = server.GetString("container_ref")
	payload.StdinFileName = databases.BackupFile(engine, name)
	payload.StdinCommand = command
	payload.ExtraTags = backup.DatabaseTags(name, server.GetString("name"))
	return nil
}

// siteFilesOf completes the command of a PHP site's job's run: what the site
// keeps outside its releases, its shared/, or for a classic site its whole
// directory but its builds, read-only, without what the site's
// backup_exclude leaves out, and the site's tag.
func (p *Panel) siteFilesOf(job, host *core.Record, payload *agentv1.BackupRun) error {
	if job.GetString("site") == "" {
		return sourceRefusal{"the site was removed"}
	}
	site, err := p.App.FindRecordById(schema.Sites, job.GetString("site"))
	if err != nil {
		return sourceRefusal{"the site was removed"}
	}
	settings, err := p.App.FindFirstRecordByData(schema.SitePHP, "site", site.Id)
	if err != nil {
		return sourceRefusal{fmt.Sprintf("the site %s is no PHP site", site.GetString("name"))}
	}
	if !backsUpDatabases(host) {
		return errors.New(agentTooOld(job, host))
	}
	name := site.GetString("name")
	dir := path.Join(sites.Root, name)
	var excludes []string
	add := func(pattern string) {
		if len(excludes) < backup.MaxExcludes && !slices.Contains(excludes, pattern) {
			excludes = append(excludes, pattern)
		}
	}
	classic := settings.GetString("mode") == schema.PHPClassic
	for _, exclude := range settings.GetStringSlice("backup_exclude") {
		exclude = strings.TrimSuffix(strings.TrimSpace(exclude), "/")
		if sites.ValidSharedPath(exclude) != nil {
			// a path out of the site, or none, is no exclude
			continue
		}
		exclude = path.Clean(exclude)
		add(path.Join(dir, "shared", exclude))
		if classic {
			// a classic site's code is backed up too, in its one release
			add(path.Join(dir, "releases", "*", exclude))
		}
	}
	payload.SiteDir = siteJobSource(site, settings)
	if classic {
		add(path.Join(dir, ".build"))
	}
	payload.Excludes = excludes
	payload.ExtraTags = backup.SiteTags(name)
	return nil
}

// databaseMoved tells whether a database job's run names another container
// than its database's server's now: the database moved since it was queued.
func (p *Panel) databaseMoved(job *core.Record, payload *agentv1.BackupRun) bool {
	_, server, err := p.jobDatabase(job)
	return err == nil && payload.GetStdinContainer() != server.GetString("container_ref")
}

// jobDatabaseSecrets are the passwords of a database job's database, its
// server's administrator's and its user's, to hide from what a run keeps: a
// run never carries them, but a dump's error might say what its server's
// environment holds.
func (p *Panel) jobDatabaseSecrets(job *core.Record) []string {
	if job.GetString("source_kind") != schema.SourceDatabase || job.GetString("database") == "" {
		return nil
	}
	database, err := p.App.FindRecordById(schema.Databases, job.GetString("database"))
	if err != nil {
		return nil
	}
	server, err := p.App.FindRecordById(schema.DBServers, database.GetString("server"))
	if err != nil {
		return nil
	}
	secrets := []string{}
	if password, err := p.sealed(server, "admin_password"); err == nil && password != "" {
		secrets = append(secrets, password)
	}
	if account, err := p.ownerOf(server, database); err == nil {
		for _, field := range []string{"password", "pending_password"} {
			if password, err := p.sealed(account, field); err == nil && password != "" {
				secrets = append(secrets, password)
			}
		}
	}
	// the longest first, so that one inside another goes whole
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	return secrets
}
