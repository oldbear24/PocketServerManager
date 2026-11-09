package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/cron"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/stacks"
)

// Backup jobs (#29) say what is backed up, from which server, to which
// targets, when, and how many snapshots restic keeps. A run backs a job up
// to one target: a backup.run command for the agent of the job's server,
// which runs restic in a job container that mounts only what it backs up,
// read-only. The stored command names the target, the sources and the
// retention, never a secret: the enricher adds the target's as the command
// is delivered, as for every backup command (see backup_targets.go).
//
// The scheduler starts the due jobs every minute, in UTC, with a run per
// target. A server that is offline at that minute gets skipped runs and an
// alert; a run whose previous one of the same job and target is still going
// is skipped. A failed run raises an alert with restic's error, which the
// next run that succeeds resolves. Jobs made by labels are kept by the deploys
// of their stacks (see syncBackupLabels).

const (
	// keepBackupRuns is how long runs stay as a job's history; a job's
	// newest run stays whatever its age.
	keepBackupRuns = 60 * 24 * time.Hour
	// backupRunQueue is how long a run may wait for its server and for the
	// runs before it of the same target, as a long backup.
	backupRunQueue = 12 * time.Hour
	// maxJobTargets is how many targets a job backs up to.
	maxJobTargets = 10
	// maxRunLog is how much of the end of a run's output its record keeps.
	maxRunLog = 32000
)

// Rules of the alerts of backups, each keyed by job and target.
const (
	alertBackupFailed    = "backup_failed"
	alertBackupSkipped   = "backup_skipped"
	alertBackupRetention = "backup_retention"
)

// noTarget is the problem of a job that backs up to no target: it does not
// run.
const noTarget = "Choose where it goes: no target."

// staleTarget is why a run queued to a target that its job no longer backs
// up to is not delivered.
const staleTarget = "the target is no longer one of the job's targets"

// What a label's job may take from its labels besides what it backs up, as
// its label_sets says: an admin changes the rest.
const (
	labelSetsSchedule  = "schedule"
	labelSetsTargets   = "targets"
	labelSetsRetention = "retention"
)

var (
	// dockerVolume is a volume's name as Docker allows it.
	dockerVolume = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)
	// composeService is a service's name as Compose allows it.
	composeService = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
)

func (p *Panel) registerBackupJobs() {
	p.Commands.OnStart("backup.run", p.backupRunStarted)
	p.Commands.OnFinish("backup.run", p.backupRunFinished)
	p.App.Cron().MustAdd("pocketcloud_backups", "* * * * *", func() { p.RunDueBackups(time.Now()) })
	p.App.Cron().MustAdd("pocketcloud_backup_runs_retention", "35 4 * * *", p.pruneBackupRuns)

	// the alerts of a job to a target go with the job, the target, the
	// job's label, or the target's place among the job's: nothing would
	// resolve them any more
	p.App.OnRecordAfterDeleteSuccess(schema.BackupJobs).BindFunc(func(e *core.RecordEvent) error {
		p.resolveBackupAlerts(e.App, dbx.Like("target", e.Record.Id+":").Match(false, true))
		return e.Next()
	})
	p.App.OnRecordAfterDeleteSuccess(schema.BackupTargets).BindFunc(func(e *core.RecordEvent) error {
		p.resolveBackupAlerts(e.App, dbx.Like("target", ":"+e.Record.Id).Match(true, false))
		return e.Next()
	})
	p.App.OnRecordAfterUpdateSuccess(schema.BackupJobs).BindFunc(func(e *core.RecordEvent) error {
		job, before := e.Record, e.Record.Original()
		if labelGone(job) && !labelGone(before) {
			p.resolveBackupAlerts(e.App, dbx.Like("target", job.Id+":").Match(false, true))
			return e.Next()
		}
		for _, target := range before.GetStringSlice("targets") {
			if !slices.Contains(job.GetStringSlice("targets"), target) {
				p.resolveBackupAlerts(e.App, dbx.HashExp{"target": alertKey(job.Id, target)})
			}
		}
		return e.Next()
	})
}

// resolveBackupAlerts resolves the open backup alerts of the jobs and targets
// that match.
func (p *Panel) resolveBackupAlerts(app core.App, target dbx.Expression) {
	alerts := []*core.Record{}
	err := app.RecordQuery(schema.Alerts).
		AndWhere(dbx.In("rule", alertBackupFailed, alertBackupSkipped, alertBackupRetention)).
		AndWhere(target).
		AndWhere(dbx.Not(dbx.HashExp{"status": schema.AlertResolved})).
		All(&alerts)
	if err != nil {
		p.Log.Error("failed to read the backup alerts", "error", err)
		return
	}
	for _, alert := range alerts {
		if err := events.ResolveAlert(app, alert.GetString("rule"), alert.GetString("target")); err != nil {
			p.Log.Error("failed to resolve an alert", "rule", alert.GetString("rule"), "error", err)
		}
	}
}

func (p *Panel) bindBackupJobRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	admins := requireRole(schema.RoleAdmin)
	operators := requireRole(schema.RoleOperator, schema.RoleAdmin)
	group.POST("/backup-jobs", p.handleAddJob).BindFunc(admins)
	group.PATCH("/backup-jobs/{id}", p.handleChangeJob).BindFunc(admins)
	group.DELETE("/backup-jobs/{id}", p.handleRemoveJob).BindFunc(admins)
	group.POST("/backup-jobs/{id}/run", p.handleRunJob).BindFunc(operators)
}

// jobBody is a job as the routes take it. Pointers are what a change may
// leave out.
type jobBody struct {
	Name       *string           `json:"name"`
	Host       *string           `json:"host"`
	SourceKind *string           `json:"source_kind"`
	Source     *string           `json:"source"`
	Stack      *string           `json:"stack"`
	Service    *string           `json:"service"`
	Engine     *string           `json:"engine"`
	Targets    *[]string         `json:"targets"`
	Schedule   *string           `json:"schedule"`
	Retention  *stacks.Retention `json:"retention"`
	Enabled    *bool             `json:"enabled"`
}

// jobFields are what the routes set on a job.
var jobFields = []string{"name", "host", "source_kind", "source", "stack", "service", "engine", "targets", "schedule", "retention", "enabled", "problem", "next_run_at"}

// apply sets what the body says on a job.
func (b jobBody) apply(job *core.Record) {
	for field, value := range map[string]*string{
		"name": b.Name, "host": b.Host, "source_kind": b.SourceKind, "source": b.Source,
		"stack": b.Stack, "service": b.Service, "engine": b.Engine, "schedule": b.Schedule,
	} {
		if value != nil {
			job.Set(field, strings.TrimSpace(*value))
		}
	}
	if b.Targets != nil {
		targets := []string{}
		for _, id := range *b.Targets {
			if id = strings.TrimSpace(id); id != "" && !slices.Contains(targets, id) {
				targets = append(targets, id)
			}
		}
		job.Set("targets", targets)
	}
	if b.Retention != nil {
		job.Set("retention", *b.Retention)
	}
	if b.Enabled != nil {
		job.Set("enabled", *b.Enabled)
	}
}

// handleAddJob adds a job, which runs from its next due minute.
func (p *Panel) handleAddJob(e *core.RequestEvent) error {
	var body jobBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}
	collection, err := e.App.FindCachedCollectionByNameOrId(schema.BackupJobs)
	if err != nil {
		return e.InternalServerError("", err)
	}
	job := core.NewRecord(collection)
	job.Set("origin", schema.JobManual)
	job.Set("enabled", true)
	job.Set("retention", stacks.Retention{})
	body.apply(job)
	if problem := p.checkJob(job, body); problem != "" {
		return e.BadRequestError(problem, nil)
	}
	job.Set("problem", jobProblem(job))
	job.Set("next_run_at", nextRunOf(job, time.Now()))
	if err := e.App.SaveWithContext(audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e)), job); err != nil {
		return e.BadRequestError("The job could not be saved: "+saveErrorText(err), nil)
	}
	return e.JSON(http.StatusCreated, job)
}

// handleChangeJob changes a job. A label's job keeps what its labels say:
// what it backs up and its server never change, its schedule, targets and
// retention only where its labels do not set them.
func (p *Panel) handleChangeJob(e *core.RequestEvent) error {
	job, err := e.App.FindRecordById(schema.BackupJobs, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The job does not exist.", err)
	}
	var body jobBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}
	if job.GetString("origin") == schema.JobLabel {
		if problem := p.checkLabelJobChange(job, body); problem != "" {
			return e.BadRequestError(problem, nil)
		}
	}
	draft := job.Fresh()
	body.apply(draft)
	if problem := p.checkJob(draft, body); problem != "" {
		return e.BadRequestError(problem, nil)
	}
	draft.Set("problem", jobProblem(draft))
	draft.Set("next_run_at", nextRunOf(draft, time.Now()))
	err = p.update(audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e)), job, func(fresh *core.Record) error {
		for _, field := range jobFields {
			fresh.Set(field, draft.Get(field))
		}
		return nil
	})
	if err != nil {
		return e.BadRequestError("The job could not be saved: "+saveErrorText(err), nil)
	}
	return e.JSON(http.StatusOK, job)
}

// checkLabelJobChange refuses a change of what a label's job takes from its
// labels: the labels win at the next deploy.
func (p *Panel) checkLabelJobChange(job *core.Record, body jobBody) string {
	stack, service := p.labelStackName(job), job.GetString("service")
	if labelGone(job) {
		return labelGoneMessage
	}
	for field, value := range map[string]*string{
		"source_kind": body.SourceKind, "source": body.Source, "stack": body.Stack,
		"service": body.Service, "engine": body.Engine, "host": body.Host,
	} {
		if value != nil && strings.TrimSpace(*value) != job.GetString(field) {
			return fmt.Sprintf("This job comes from the %s label of %s (%s): change what it backs up there, and deploy.", stacks.LabelBackup, stack, service)
		}
	}
	sets := job.GetStringSlice("label_sets")
	from := func(what, name string) string {
		return fmt.Sprintf("%s from the %s label of %s (%s): change it there, and deploy.", what, name, stack, service)
	}
	switch {
	case body.Schedule != nil && stacks.NormalSchedule(*body.Schedule) != job.GetString("schedule") && slices.Contains(sets, labelSetsSchedule):
		return from("The schedule of this job comes", stacks.LabelBackupSchedule)
	case body.Targets != nil && !sameSet(*body.Targets, job.GetStringSlice("targets")) && slices.Contains(sets, labelSetsTargets):
		return from("The targets of this job come", stacks.LabelBackupTarget)
	case body.Retention != nil && *body.Retention != retentionOf(job) && slices.Contains(sets, labelSetsRetention):
		return from("The retention of this job comes", stacks.LabelBackupRetention)
	}
	return ""
}

// labelGone tells whether a label's job lost its label, or its stack.
func labelGone(job *core.Record) bool {
	return job.GetString("origin") == schema.JobLabel && (job.GetBool("label_removed") || job.GetString("stack") == "")
}

// labelGoneMessage says what is left to do with a label's job whose label is
// gone.
var labelGoneMessage = fmt.Sprintf("The %s label of this job is gone: remove the job.", stacks.LabelBackup)

// handleRemoveJob removes a job with its runs; its snapshots stay in the
// repositories. A label's job whose label is still there would come back.
func (p *Panel) handleRemoveJob(e *core.RequestEvent) error {
	job, err := e.App.FindRecordById(schema.BackupJobs, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The job does not exist.", err)
	}
	if job.GetString("origin") == schema.JobLabel && !labelGone(job) {
		return e.Error(http.StatusConflict, fmt.Sprintf("It comes back at the stack's next deploy: remove its %s label instead.", stacks.LabelBackup), nil)
	}
	// runs that have not left the queue do not start for a job that is gone
	runs, err := e.App.FindRecordsByFilter(schema.BackupRuns, "job={:job} && status={:queued} && command!=''", "", 0, 0,
		dbx.Params{"job": job.Id, "queued": schema.RunQueued})
	if err == nil {
		for _, run := range runs {
			if command, err := e.App.FindRecordById(schema.Commands, run.GetString("command")); err == nil && command.GetString("status") == schema.CommandQueued {
				_ = p.Commands.Cancel(command.Id, "The backup job was removed.")
			}
		}
	}
	if err := e.App.DeleteWithContext(audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e)), job); err != nil {
		return e.BadRequestError("The job could not be removed: "+err.Error(), nil)
	}
	return e.NoContent(http.StatusNoContent)
}

// handleRunJob backs a job up now, to each of its targets whose run before is
// not still going.
func (p *Panel) handleRunJob(e *core.RequestEvent) error {
	job, err := e.App.FindRecordById(schema.BackupJobs, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The job does not exist.", err)
	}
	if labelGone(job) {
		return e.Error(http.StatusConflict, labelGoneMessage, nil)
	}
	targets := p.jobTargets(job)
	if len(targets) == 0 {
		return e.BadRequestError("Choose where it goes: the job has no target.", nil)
	}
	host, err := e.App.FindRecordById(schema.Hosts, job.GetString("host"))
	if err != nil {
		return e.Error(http.StatusConflict, "The server of the job is gone.", nil)
	}
	if !p.Commands.Connected(host.Id) {
		return e.Error(http.StatusConflict, fmt.Sprintf("The server %s is offline: its agent runs the backup.", host.GetString("name")), nil)
	}
	requestedBy, requester := requester(e)
	runs := []string{}
	for _, target := range targets {
		if p.runGoing(job, target) {
			continue
		}
		// a client that leaves does not leave a run without its command
		run, err := p.startRun(context.WithoutCancel(e.Request.Context()), job, target, host, schema.RunManual, time.Time{},
			commands.Options{RequestedBy: requestedBy, Requester: requester})
		if err != nil {
			p.Log.Error("failed to start a backup run", "job", job.Id, "target", target.Id, "error", err)
			continue
		}
		runs = append(runs, run.Id)
	}
	if len(runs) == 0 {
		return e.Error(http.StatusConflict, "Its previous runs are still going.", nil)
	}
	return e.JSON(http.StatusOK, map[string]any{"runs": runs})
}

// checkJob checks a job as it would be saved and tidies it: what it backs
// up, from which server, to which targets, when, and how much is kept. It
// returns what is wrong, as a sentence.
func (p *Panel) checkJob(job *core.Record, body jobBody) string {
	name := strings.TrimSpace(job.GetString("name"))
	switch {
	case name == "":
		return "Give the job a name."
	case utf8.RuneCountInString(name) > 100:
		return "The name is at most 100 characters."
	}
	others, err := p.App.FindRecordsByFilter(schema.BackupJobs, "name={:name} && id!={:self}", "", 1, 0, dbx.Params{"name": name, "self": job.Id})
	if err == nil && len(others) > 0 {
		return fmt.Sprintf("A job named %q exists already.", name)
	}
	job.Set("name", name)

	// a label's source never changes: it is its labels'
	if job.GetString("origin") != schema.JobLabel {
		if problem := p.checkSource(job, body); problem != "" {
			return problem
		}
	}
	if _, err := p.App.FindRecordById(schema.Hosts, job.GetString("host")); err != nil {
		return "Choose the server whose data the job backs up."
	}

	targets := job.GetStringSlice("targets")
	if len(targets) > maxJobTargets {
		return fmt.Sprintf("A job backs up to at most %d targets.", maxJobTargets)
	}
	found, err := p.App.FindRecordsByIds(schema.BackupTargets, targets)
	if err != nil || len(found) != len(targets) {
		return "One of the targets does not exist."
	}
	for _, target := range found {
		if target.GetString("kind") == backup.KindLocal && target.GetString("host") != job.GetString("host") {
			return localElsewhere(target)
		}
	}

	schedule := stacks.NormalSchedule(job.GetString("schedule"))
	if !stacks.ValidSchedule(schedule) {
		return stacks.ScheduleProblem
	}
	job.Set("schedule", schedule)

	var retention stacks.Retention
	if err := job.UnmarshalJSONField("retention", &retention); err != nil || !retention.Valid() {
		return "Each count of the retention is between 0 and 10000."
	}
	job.Set("retention", retention)
	return ""
}

// checkSource checks what a job backs up, by its kind: a volume or a
// directory of its server, a stack's volumes or a dump of a stack's
// database, whose server is the stack's.
func (p *Panel) checkSource(job *core.Record, body jobBody) string {
	source := job.GetString("source")
	switch kind := job.GetString("source_kind"); kind {
	case schema.SourceVolume:
		if !dockerVolume.MatchString(source) || len(source) > 255 {
			return "Name the volume as Docker does: letters, digits, and . _ - after the first."
		}
		job.Set("stack", "")
		job.Set("service", "")
		job.Set("engine", "")
	case schema.SourcePath:
		if problem := checkBackupPath(source); problem != "" {
			return problem
		}
		job.Set("stack", "")
		job.Set("service", "")
		job.Set("engine", "")
	case schema.SourceStack, schema.SourceDump:
		stack, err := p.App.FindRecordById(schema.Stacks, job.GetString("stack"))
		if err != nil {
			return "Choose the stack."
		}
		if body.Host != nil && strings.TrimSpace(*body.Host) != "" && strings.TrimSpace(*body.Host) != stack.GetString("host") {
			return fmt.Sprintf("The stack %s is on another server: a job backs up the server it runs on.", stack.GetString("name"))
		}
		job.Set("host", stack.GetString("host"))
		if kind == schema.SourceStack {
			job.Set("source", "")
			job.Set("service", "")
			job.Set("engine", "")
			return ""
		}
		if service := job.GetString("service"); service == "" || len(service) > 255 || !composeService.MatchString(service) {
			return "Name the service whose container dumps the database."
		}
		switch job.GetString("engine") {
		case schema.DumpPostgres, schema.DumpMySQL:
			job.Set("source", "")
		case schema.DumpSQLite:
			if !path.IsAbs(source) || path.Clean(source) != source || source == "/" || len(source) > 1000 {
				return "Name the SQLite file by its absolute path in the service's container, such as /data/app.db."
			}
		default:
			return "Choose the database: PostgreSQL, MySQL or MariaDB, or SQLite."
		}
	default:
		return "Choose what the job backs up: a volume, a directory, a stack or a database dump."
	}
	return ""
}

// jobDirectories are the directories of a backup's job container, from its
// image or its mounts: a directory of the server that is one of them, or
// holds one, would hide it, and the agent refuses it.
var jobDirectories = []string{
	"/bin", "/sbin", "/lib", "/usr/bin", "/usr/sbin", "/usr/lib", "/usr/local/bin", "/tmp",
	"/run/pocketcloud", "/var/lib/pocketcloud", "/cache", "/repo", "/volumes",
}

// notBackedUp are directories nothing under which is backed up: the
// kernel's, and those of the job container and of PocketCloud.
var notBackedUp = []string{"/proc", "/sys", "/dev", "/cache", "/repo", "/volumes", "/run/pocketcloud", "/var/lib/pocketcloud"}

// checkBackupPath checks a directory of a server to back up, as the agent
// does: an absolute, clean path that is not the whole server, not a directory
// the job container needs nor one that holds one, and not under the kernel's
// or PocketCloud's own.
func checkBackupPath(dir string) string {
	if !path.IsAbs(dir) || path.Clean(dir) != dir || strings.ContainsAny(dir, "\x00\n\r") || len(dir) > 1000 {
		return "Name the directory by its absolute path, such as /srv/data."
	}
	if dir == "/" {
		return "Name a directory, not the whole server."
	}
	for _, root := range notBackedUp {
		if dir == root || strings.HasPrefix(dir, root+"/") {
			return fmt.Sprintf("Nothing under %s is backed up: it is the kernel's, or PocketCloud's own.", root)
		}
	}
	for _, own := range jobDirectories {
		switch {
		case dir == own:
			return fmt.Sprintf("The backup's job needs %s for itself: name a directory of your data, such as /srv/data.", dir)
		case strings.HasPrefix(own, dir+"/"):
			return fmt.Sprintf("%s holds %s, which the backup's job needs for itself: name a directory of your data, such as /srv/data.", dir, own)
		}
	}
	return ""
}

// localElsewhere says that a local target is not a job's.
func localElsewhere(target *core.Record) string {
	return fmt.Sprintf("The target %s is a directory of its own server: only that server's jobs back up to it.", target.GetString("name"))
}

// jobProblem is what keeps a job from running, as it is saved: no target, or
// what its labels said at the last deploy.
func jobProblem(job *core.Record) string {
	switch problem := job.GetString("problem"); {
	case len(job.GetStringSlice("targets")) == 0:
		return noTarget
	case problem == noTarget:
		return ""
	default:
		return problem
	}
}

// sameSet says whether two lists hold the same ids.
func sameSet(a, b []string) bool {
	set := func(list []string) []string {
		out := []string{}
		for _, item := range list {
			if item = strings.TrimSpace(item); item != "" && !slices.Contains(out, item) {
				out = append(out, item)
			}
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(set(a), set(b))
}

// retentionOf reads a job's retention.
func retentionOf(job *core.Record) stacks.Retention {
	var retention stacks.Retention
	_ = job.UnmarshalJSONField("retention", &retention)
	return retention
}

// nextRunOf is the minute after now that a job runs next; empty for one that
// does not run.
func nextRunOf(job *core.Record, now time.Time) any {
	if !job.GetBool("enabled") || job.GetBool("label_removed") || len(job.GetStringSlice("targets")) == 0 {
		return ""
	}
	next := parseCronRule(job.GetString("schedule")).next(now)
	if next.IsZero() {
		return ""
	}
	return next
}

// RunDueBackups starts the enabled jobs whose schedule says now, in UTC,
// with a run per target: the panel calls it every minute. Then each job's
// next run is set.
func (p *Panel) RunDueBackups(now time.Time) {
	minute := now.UTC().Truncate(time.Minute)
	moment := cron.NewMoment(minute)
	jobs, err := p.App.FindRecordsByFilter(schema.BackupJobs, "enabled = true && label_removed = false", "created", 0, 0)
	if err != nil {
		p.Log.Error("failed to read the backup jobs", "error", err)
		return
	}
	for _, job := range jobs {
		if len(job.GetStringSlice("targets")) == 0 || !parseCronRule(job.GetString("schedule")).due(moment) {
			continue
		}
		p.runOnSchedule(job, minute)
	}
	p.refreshNextRuns(now)
}

// runOnSchedule starts a job's runs of a minute: skipped when its server is
// offline or the run before is still going.
func (p *Panel) runOnSchedule(job *core.Record, minute time.Time) {
	host, err := p.App.FindRecordById(schema.Hosts, job.GetString("host"))
	if err != nil {
		return
	}
	online := p.Commands.Connected(host.Id)
	for _, target := range p.jobTargets(job) {
		switch {
		case !online:
			p.skipRun(job, target, host, minute, fmt.Sprintf("The server %s was offline at %s UTC.", host.GetString("name"), minute.Format("15:04")), true)
		case p.runGoing(job, target):
			p.skipRun(job, target, host, minute, "The run before it was still going.", false)
		default:
			if _, err := p.startRun(context.Background(), job, target, host, schema.RunSchedule, minute, commands.Options{Requester: "schedule"}); err != nil {
				p.Log.Error("failed to start a backup run", "job", job.Id, "target", target.Id, "error", err)
			}
		}
	}
}

// refreshNextRuns sets each job's next run, as it is after now.
func (p *Panel) refreshNextRuns(now time.Time) {
	jobs, err := p.App.FindAllRecords(schema.BackupJobs)
	if err != nil {
		return
	}
	for _, job := range jobs {
		want := nextRunOf(job, now)
		if sameDate(job.GetDateTime("next_run_at"), want) {
			continue
		}
		err := p.update(context.Background(), job, func(fresh *core.Record) error {
			want := nextRunOf(fresh, now)
			if sameDate(fresh.GetDateTime("next_run_at"), want) {
				return errUnchanged
			}
			fresh.Set("next_run_at", want)
			return nil
		})
		if err != nil {
			p.Log.Error("failed to store the next run of a backup job", "job", job.Id, "error", err)
		}
	}
}

// sameDate says whether a stored date is a next run, or both are none.
func sameDate(stored types.DateTime, next any) bool {
	at, ok := next.(time.Time)
	if !ok {
		return stored.IsZero()
	}
	return stored.Time().Equal(at)
}

// jobTargets are a job's targets, in its order.
func (p *Panel) jobTargets(job *core.Record) []*core.Record {
	ids := job.GetStringSlice("targets")
	found, err := p.App.FindRecordsByIds(schema.BackupTargets, ids)
	if err != nil {
		return nil
	}
	slices.SortFunc(found, func(a, b *core.Record) int { return slices.Index(ids, a.Id) - slices.Index(ids, b.Id) })
	return found
}

// runGoing tells whether a run of a job to a target is still queued or
// running. A run whose command ended without its finisher, as when the
// panel stopped in between, is settled from the command first.
func (p *Panel) runGoing(job, target *core.Record) bool {
	runs, err := p.App.FindRecordsByFilter(schema.BackupRuns, "job={:job} && target={:target} && (status={:queued} || status={:running})", "-created", 0, 0,
		dbx.Params{"job": job.Id, "target": target.Id, "queued": schema.RunQueued, "running": schema.RunRunning})
	if err != nil {
		return true
	}
	going := false
	for _, run := range runs {
		command, err := p.App.FindRecordById(schema.Commands, run.GetString("command"))
		switch {
		case err == nil && !commandFinal(command):
			going = true
		case err == nil:
			p.backupRunFinished(command)
		case run.GetString("command") == "" && time.Since(run.GetDateTime("created").Time()) < time.Minute:
			// its command is being made
			going = true
		default:
			p.settleRun(run, schema.RunCancelled, "Its command is gone.")
		}
	}
	return going
}

// startRun makes a run of a job to a target and queues its command. A run
// whose command cannot be made fails at once.
func (p *Panel) startRun(ctx context.Context, job, target, host *core.Record, trigger string, scheduledAt time.Time, options commands.Options) (*core.Record, error) {
	collection, err := p.App.FindCachedCollectionByNameOrId(schema.BackupRuns)
	if err != nil {
		return nil, err
	}
	run := core.NewRecord(collection)
	run.Set("job", job.Id)
	run.Set("target", target.Id)
	run.Set("host", host.Id)
	run.Set("trigger", trigger)
	run.Set("status", schema.RunQueued)
	if !scheduledAt.IsZero() {
		run.Set("scheduled_at", scheduledAt)
	}
	if err := p.App.Save(run); err != nil {
		return nil, err
	}
	payload, err := p.backupRun(job, target, host, run)
	if err != nil {
		p.runFailed(run, job, target, sentence(err))
		return run, nil
	}
	options.Host = host.Id
	options.Label = backupLabel(job, target)
	options.Command = &agentv1.Command{Payload: &agentv1.Command_BackupRun{BackupRun: payload}}
	options.QueueTimeout = backupRunQueue
	command, err := p.Commands.Create(ctx, options)
	if err != nil {
		p.runFailed(run, job, target, sentence(err))
		return run, nil
	}
	p.setFields(run, map[string]any{"command": command.Id})
	p.noteJobRun(job.Id)
	return run, nil
}

// skipRun keeps a run that did not start, and says so in the events; an
// offline server raises an alert too.
func (p *Panel) skipRun(job, target, host *core.Record, minute time.Time, reason string, alert bool) {
	collection, err := p.App.FindCachedCollectionByNameOrId(schema.BackupRuns)
	if err != nil {
		return
	}
	run := core.NewRecord(collection)
	run.Load(map[string]any{
		"job": job.Id, "target": target.Id, "host": host.Id, "trigger": schema.RunSchedule, "status": schema.RunSkipped,
		"scheduled_at": minute, "finished_at": types.NowDateTime(), "error": reason,
	})
	if err := p.App.Save(run); err != nil {
		p.Log.Error("failed to keep a skipped backup run", "job", job.Id, "error", err)
		return
	}
	message := cutText(fmt.Sprintf("%s was skipped: %s", backupLabel(job, target), strings.ToLower(reason[:1])+reason[1:]), 2048)
	events.Log(p.App, host.Id, "backup.skipped", schema.SeverityWarning, message, map[string]any{"job": job.Id, "target": target.Id, "run": run.Id})
	if alert {
		p.raiseJobAlert(alertBackupSkipped, job, target, schema.SeverityWarning, message)
	}
	p.noteJobRun(job.Id)
}

// runFailed fails a run that could not start, and alerts.
func (p *Panel) runFailed(run, job, target *core.Record, reason string) {
	p.setFields(run, map[string]any{"status": schema.RunFailed, "error": cutText(reason, 4096), "finished_at": types.NowDateTime()})
	p.alertFailure(job, target, run, reason)
	p.noteJobRun(job.Id)
}

// alertFailure raises the alert of a failed run, and logs it.
func (p *Panel) alertFailure(job, target, run *core.Record, reason string) {
	message := cutText(fmt.Sprintf("%s failed: %s", backupLabel(job, target), sentenceText(reason)), 2048)
	events.Log(p.App, job.GetString("host"), "backup.failed", schema.SeverityCritical, message, map[string]any{"job": job.Id, "target": target.Id, "run": run.Id})
	p.raiseJobAlert(alertBackupFailed, job, target, schema.SeverityCritical, message)
}

// raiseJobAlert raises an alert of a job to a target. A job no longer alerts
// about a target it does not back up to, nor once its label is gone: their
// alerts were resolved as the job changed (see resolveBackupAlerts).
func (p *Panel) raiseJobAlert(rule string, job, target *core.Record, severity, message string) {
	if !backsUpTo(job, target) {
		return
	}
	if err := events.RaiseAlert(p.App, rule, alertKey(job.Id, target.Id), job.GetString("host"), severity, message); err != nil {
		p.Log.Error("failed to raise an alert", "rule", rule, "error", err)
	}
}

// backsUpTo tells whether a job still backs up to a target: the target is one
// of its own, and a label's job still has its label.
func backsUpTo(job, target *core.Record) bool {
	return slices.Contains(job.GetStringSlice("targets"), target.Id) && !labelGone(job)
}

// settleRun ends a run that its command can no longer end.
func (p *Panel) settleRun(run *core.Record, status, reason string) {
	err := p.update(context.Background(), run, func(fresh *core.Record) error {
		if s := fresh.GetString("status"); s != schema.RunQueued && s != schema.RunRunning {
			return errUnchanged
		}
		fresh.Set("status", status)
		fresh.Set("error", reason)
		fresh.Set("finished_at", types.NowDateTime())
		return nil
	})
	if err != nil {
		p.Log.Error("failed to settle a backup run", "run", run.Id, "error", err)
	}
	p.noteJobRun(run.GetString("job"))
}

// alertKey is the target of a job's alerts to one target.
func alertKey(jobID, targetID string) string { return jobID + ":" + targetID }

// backupLabel names a run in its command and the events.
func backupLabel(job, target *core.Record) string {
	return cutText("Backup "+job.GetString("name")+" → "+target.GetString("name"), 255)
}

// backupRun is the command of a run: the target's repository, without its
// secrets, what the job backs up, its tags, the server's name and the
// retention.
func (p *Panel) backupRun(job, target, host *core.Record, run *core.Record) (*agentv1.BackupRun, error) {
	secrets, err := p.targetSecretsOf(target)
	if err != nil {
		p.secretsMissing(target, err)
		return nil, errors.New(strings.ToLower(secretsMissingMessage[:1]) + secretsMissingMessage[1:])
	}
	repo, err := backup.Locate(secrets.kind, secrets.config, secrets.creds)
	if err != nil {
		return nil, err
	}
	retention := retentionOf(job)
	payload := &agentv1.BackupRun{
		RunId:      run.Id,
		Repository: &agentv1.BackupRepository{TargetId: target.Id, Url: repo.URL},
		Tags:       []string{"pocketcloud", "job:" + job.Id},
		HostName:   host.GetString("name"),
		Retention: &agentv1.BackupRetention{
			Last: int32(retention.Last), Hourly: int32(retention.Hourly), Daily: int32(retention.Daily), //nolint:gosec // at most 10000
			Weekly: int32(retention.Weekly), Monthly: int32(retention.Monthly), Yearly: int32(retention.Yearly), //nolint:gosec // at most 10000
		},
	}
	if retention == (stacks.Retention{}) {
		payload.Retention = nil
	}
	switch job.GetString("source_kind") {
	case schema.SourceVolume:
		payload.Volumes = []string{job.GetString("source")}
	case schema.SourcePath:
		payload.Paths = []string{job.GetString("source")}
	case schema.SourceStack:
		volumes, err := p.stackVolumes(job)
		if err != nil {
			return nil, err
		}
		payload.Volumes = volumes
	case schema.SourceDump:
		if err := p.dumpOf(job, payload); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("the job backs up nothing")
	}
	return payload, nil
}

// stackVolumes are the volumes a stack's job backs up: those its label named,
// or every named volume of the stack's Compose project, as the inventory of
// its server has them.
func (p *Panel) stackVolumes(job *core.Record) ([]string, error) {
	stack, err := p.App.FindRecordById(schema.Stacks, job.GetString("stack"))
	if err != nil {
		return nil, errors.New("the stack is gone")
	}
	if source := job.GetString("source"); source != "" {
		return strings.Split(source, ","), nil
	}
	records, err := p.App.FindRecordsByFilter(schema.Volumes, "host={:host}", "name", 0, 0, dbx.Params{"host": stack.GetString("host")})
	if err != nil {
		return nil, err
	}
	volumes := []string{}
	for _, record := range records {
		labels := map[string]string{}
		_ = record.UnmarshalJSONField("labels", &labels)
		if labels[labelComposeProject] == stack.GetString("name") {
			volumes = append(volumes, record.GetString("name"))
		}
	}
	if len(volumes) == 0 {
		return nil, fmt.Errorf("the stack %s has no volumes to back up", stack.GetString("name"))
	}
	return volumes, nil
}

// dumpCommand is what dumps a job's database to its standard output in its
// service's container, with what the container's own environment says, so
// that no password is in the command; and the extension of the dump's file.
func dumpCommand(engine, file string) ([]string, string, bool) {
	switch engine {
	case schema.DumpPostgres:
		return []string{"sh", "-c", `exec pg_dump --format=custom --compress=0 --username="${POSTGRES_USER:-postgres}" "${POSTGRES_DB:-${POSTGRES_USER:-postgres}}"`}, "pgdump", true
	case schema.DumpMySQL:
		return []string{"sh", "-c", `if command -v mariadb-dump >/dev/null 2>&1; then d=mariadb-dump; else d=mysqldump; fi; ` +
			`MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-$MYSQL_ROOT_PASSWORD}" exec "$d" --user=root --single-transaction --routines --triggers --events --hex-blob --all-databases`}, "sql", true
	case schema.DumpSQLite:
		// the image needs sqlite3: the shell's error says so when it has none
		return []string{"sh", "-c", sqliteDump, "sh", file}, "sql", true
	}
	return nil, "", false
}

// sqliteDump dumps the SQLite file it is given ($1). sqlite3 exits 0 when it
// cannot read the database, with only an error in the dump, and makes an
// empty file where there was none: so it opens the file read-only, waits up
// to a minute for a writer's lock, and any error it says fails the dump, with
// its words.
const sqliteDump = `exec 3>&1; e=$(sqlite3 -readonly -cmd '.timeout 60000' "$1" .dump 2>&1 >&3); s=$?; ` +
	`if [ -n "$e" ]; then printf '%s\n' "$e" >&2; exit 1; fi; exit $s`

// dumpOf completes the command of a dump: the running container of the
// job's service, which runs the engine's dump, and the dump's file name.
func (p *Panel) dumpOf(job *core.Record, payload *agentv1.BackupRun) error {
	stack, err := p.App.FindRecordById(schema.Stacks, job.GetString("stack"))
	if err != nil {
		return errors.New("the stack is gone")
	}
	command, extension, ok := dumpCommand(job.GetString("engine"), job.GetString("source"))
	if !ok {
		return errors.New("the job names no database")
	}
	service := job.GetString("service")
	containers, err := p.App.FindRecordsByFilter(schema.Containers,
		"host={:host} && project={:project} && service={:service} && state='running'", "name", 1, 0,
		dbx.Params{"host": stack.GetString("host"), "project": stack.GetString("name"), "service": service})
	if err != nil || len(containers) == 0 {
		return fmt.Errorf("the service %s of %s has no running container to dump its database in", service, stack.GetString("name"))
	}
	payload.StdinContainer = containers[0].GetString("docker_id")
	payload.StdinFileName = stack.GetString("name") + "-" + service + "." + extension
	payload.StdinCommand = command
	return nil
}

// checkRunDelivery checks a backup.run as it is delivered: its run and its
// job are still there, the command goes to the job's server, and the target
// takes the job's backups. A run queued before its target was taken out of
// the job, or before its label went, gets no secrets of the target: the
// server need no longer have them.
func (p *Panel) checkRunDelivery(record *core.Record, payload *agentv1.BackupRun, target *core.Record) error {
	run, err := p.App.FindRecordById(schema.BackupRuns, payload.GetRunId())
	if err != nil {
		return errors.New("the backup run is gone")
	}
	job, err := p.App.FindRecordById(schema.BackupJobs, run.GetString("job"))
	if err != nil {
		return errors.New("the backup job is gone")
	}
	switch {
	case job.GetString("host") != record.GetString("host"):
		return errors.New("the backup job runs on another server")
	case run.GetString("target") != target.Id:
		return errors.New("the backup run is for another target")
	case !slices.Contains(job.GetStringSlice("targets"), target.Id):
		return errors.New(staleTarget)
	case labelGone(job):
		return fmt.Errorf("the %s label of the job is gone", stacks.LabelBackup)
	case target.GetString("status") == schema.TargetPending:
		return errors.New("the target is still being added")
	case target.GetString("kind") == backup.KindLocal && target.GetString("host") != job.GetString("host"):
		return errors.New("a local target is a directory of its own server: only that server's jobs back up to it")
	}
	return nil
}

// backupRunPayload is what the finisher reads of a backup.run.
type backupRunPayload struct {
	RunID string `json:"run_id"`
}

// runOfCommand is the run a backup.run backs up.
func (p *Panel) runOfCommand(command *core.Record) (*core.Record, error) {
	var payload backupRunPayload
	raw, _ := json.Marshal(command.Get("payload"))
	_ = json.Unmarshal(raw, &payload)
	return p.App.FindRecordById(schema.BackupRuns, payload.RunID)
}

// backupRunStarted marks a run as running once its agent started it.
func (p *Panel) backupRunStarted(command *core.Record) {
	run, err := p.runOfCommand(command)
	if err != nil {
		return
	}
	err = p.update(context.Background(), run, func(fresh *core.Record) error {
		if fresh.GetString("status") != schema.RunQueued {
			return errUnchanged
		}
		fresh.Set("status", schema.RunRunning)
		fresh.Set("started_at", command.GetDateTime("started_at"))
		return nil
	})
	if err != nil {
		p.Log.Error("failed to mark a backup run as running", "run", run.Id, "error", err)
	}
	p.noteJobRun(run.GetString("job"))
}

// backupRunResult is what the agent's backup.run answers.
type backupRunResult struct {
	SnapshotID     string  `json:"snapshot_id"`
	BytesAdded     float64 `json:"bytes_added"`
	TotalBytes     float64 `json:"total_bytes"`
	FilesNew       float64 `json:"files_new"`
	FilesChanged   float64 `json:"files_changed"`
	Forgotten      float64 `json:"forgotten"`
	Log            string  `json:"log"`
	RetentionError string  `json:"retention_error"`
}

// backupRunFinished keeps how a run went, and alerts: a failure with
// restic's error, a retention that failed after a backup that did not; a run
// that succeeds resolves its job's alerts to its target.
func (p *Panel) backupRunFinished(command *core.Record) {
	run, err := p.runOfCommand(command)
	if err != nil {
		return
	}
	if s := run.GetString("status"); s != schema.RunQueued && s != schema.RunRunning {
		return
	}
	job, err := p.App.FindRecordById(schema.BackupJobs, run.GetString("job"))
	if err != nil {
		return
	}
	target, err := p.App.FindRecordById(schema.BackupTargets, run.GetString("target"))
	if err != nil {
		return
	}
	var result backupRunResult
	raw, _ := json.Marshal(command.Get("result"))
	_ = json.Unmarshal(raw, &result)

	started := command.GetDateTime("started_at")
	status := schema.RunFailed
	switch commandStatus := command.GetString("status"); {
	case commandStatus == schema.CommandSucceeded:
		status = schema.RunSucceeded
	case commandStatus == schema.CommandCancelled:
		status = schema.RunCancelled
	case commandStatus == schema.CommandTimedOut && started.IsZero():
		// never delivered, as to a server that stayed offline: no run
		status = schema.RunCancelled
	}
	if status == schema.RunFailed && started.IsZero() && !backsUpTo(job, target) {
		// not delivered: the job no longer backs up to the target
		status = schema.RunCancelled
	}
	message := command.GetString("error")
	if status == schema.RunFailed && message == "" {
		message = "The backup failed."
		if command.GetString("status") == schema.CommandTimedOut {
			message = "The backup did not finish in time."
		}
	}
	// the agent took the secrets out already; whatever it missed
	hide := p.targetSecretValues(target)
	message = capitalize(backup.Redact(message, hide...))
	retentionError := backup.Redact(result.RetentionError, hide...)

	finished := command.GetDateTime("finished_at")
	if finished.IsZero() {
		finished = types.NowDateTime()
	}
	fields := map[string]any{
		"status": status, "finished_at": finished, "error": cutText(message, 4096),
		"snapshot_id": cutText(result.SnapshotID, 100), "bytes_added": int64(result.BytesAdded), "total_bytes": int64(result.TotalBytes),
		"files_new": int64(result.FilesNew), "files_changed": int64(result.FilesChanged), "forgotten": int64(result.Forgotten),
		"log": cutTail(backup.Redact(result.Log, hide...), maxRunLog),
	}
	if !started.IsZero() {
		fields["started_at"] = started
		fields["duration_ms"] = finished.Time().Sub(started.Time()).Milliseconds()
	}
	if status == schema.RunSucceeded {
		fields["error"] = ""
	}
	// once: the run's command may be settled from two places
	kept := false
	err = p.update(context.Background(), run, func(fresh *core.Record) error {
		if s := fresh.GetString("status"); s != schema.RunQueued && s != schema.RunRunning {
			return errUnchanged
		}
		for name, value := range fields {
			fresh.Set(name, value)
		}
		kept = true
		return nil
	})
	if err != nil {
		p.Log.Error("failed to keep a backup run", "run", run.Id, "error", err)
	}
	if !kept {
		return
	}

	key := alertKey(job.Id, target.Id)
	host := job.GetString("host")
	switch status {
	case schema.RunFailed:
		p.alertFailure(job, target, run, message)
	case schema.RunSucceeded:
		for _, rule := range []string{alertBackupFailed, alertBackupSkipped} {
			if err := events.ResolveAlert(p.App, rule, key); err != nil {
				p.Log.Error("failed to resolve an alert", "rule", rule, "error", err)
			}
		}
		if retentionError != "" {
			text := cutText(fmt.Sprintf("%s kept every snapshot: restic could not forget the old ones: %s", backupLabel(job, target), sentenceText(retentionError)), 2048)
			events.Log(p.App, host, "backup.retention_failed", schema.SeverityWarning, text, map[string]any{"job": job.Id, "target": target.Id, "run": run.Id})
			p.raiseJobAlert(alertBackupRetention, job, target, schema.SeverityWarning, text)
		} else if err := events.ResolveAlert(p.App, alertBackupRetention, key); err != nil {
			p.Log.Error("failed to resolve an alert", "rule", alertBackupRetention, "error", err)
		}
	}
	p.noteJobRun(job.Id)
}

// targetSecretValues are a target's secrets, to hide from what is kept.
func (p *Panel) targetSecretValues(target *core.Record) []string {
	secrets, err := p.targetSecretsOf(target)
	if err != nil {
		return nil
	}
	repo, _ := backup.Locate(secrets.kind, secrets.config, secrets.creds)
	return backup.Secrets(secrets.password, repo.Env, repo.SSHKey)
}

// noteJobRun keeps the state of a job's newest run on the job. The run is
// read in the transaction that writes the job: a run's start and its end
// noted at once leave the job with the later state, whichever is noted last.
func (p *Panel) noteJobRun(jobID string) {
	err := p.App.RunInTransaction(func(tx core.App) error {
		runs, err := tx.FindRecordsByFilter(schema.BackupRuns, "job={:job}", "-created,-id", 1, 0, dbx.Params{"job": jobID})
		if err != nil || len(runs) == 0 {
			return err
		}
		newest := runs[0]
		job, err := tx.FindRecordById(schema.BackupJobs, jobID)
		if err != nil {
			// a job removed meanwhile has nothing to keep
			return nil //nolint:nilerr // nothing to do
		}
		if job.GetString("last_status") == newest.GetString("status") && job.GetDateTime("last_run_at").Equal(newest.GetDateTime("created")) {
			return nil
		}
		job.Set("last_status", newest.GetString("status"))
		job.Set("last_run_at", newest.GetDateTime("created"))
		return tx.Save(job)
	})
	if err != nil {
		p.Log.Error("failed to keep the last run of a backup job", "job", jobID, "error", err)
	}
}

// pruneBackupRuns removes the runs older than their history keeps, but each
// job's newest one: a run goes only when its job has a newer one.
func (p *Panel) pruneBackupRuns() {
	cutoff := types.NowDateTime().Add(-keepBackupRuns)
	_, err := p.App.DB().NewQuery(
		"DELETE FROM " + schema.BackupRuns + " WHERE created < {:cutoff} AND status NOT IN ('queued', 'running')" +
			" AND EXISTS (SELECT 1 FROM " + schema.BackupRuns + " n WHERE n.job = " + schema.BackupRuns + ".job" +
			" AND (n.created > " + schema.BackupRuns + ".created OR (n.created = " + schema.BackupRuns + ".created AND n.id > " + schema.BackupRuns + ".id)))",
	).Bind(dbx.Params{"cutoff": cutoff.String()}).Execute()
	if err != nil {
		p.Log.Error("failed to remove old backup runs", "error", err)
	}
}

// sentence makes an error a sentence, as runs and alerts keep them.
func sentence(err error) string { return sentenceText(err.Error()) }

func sentenceText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return text
	}
	return capitalize(strings.TrimSuffix(text, ".")) + "."
}

// cutText cuts a text to at most limit bytes, on a character's boundary.
func cutText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// cutTail keeps the end of a text, at most limit bytes, from a character's
// boundary.
func cutTail(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	start := len(text) - limit
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:]
}
