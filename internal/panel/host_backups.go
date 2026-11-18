package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"

	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/stacks"
)

// A server's automatic-backup settings (#53): the schedule, targets and
// retention that the automatic jobs of its databases and PHP sites follow,
// but for what a job set for itself. Admins set them with PUT
// /hosts/{id}/auto-backups, which saves the server and its jobs in one
// transaction; the records API keeps them as they are, so that no job is
// left behind its server.

// hostAutoFields are a server's automatic-backup settings.
var hostAutoFields = []string{"auto_backup_schedule", "auto_backup_targets", "auto_backup_retention"}

func (p *Panel) registerHostBackups() {
	p.App.OnRecordUpdateRequest(schema.Hosts).BindFunc(p.keepHostAutoBackups)
}

func (p *Panel) bindHostBackupRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	group.PUT("/hosts/{id}/auto-backups", p.handleHostAutoBackups).BindFunc(requireRole(schema.RoleAdmin))
}

// keepHostAutoBackups keeps a server's automatic-backup settings out of the
// records API: they change through their route, with the jobs that follow
// them.
func (p *Panel) keepHostAutoBackups(e *core.RecordRequestEvent) error {
	stored, err := e.App.FindRecordById(schema.Hosts, e.Record.Id)
	if err != nil {
		return e.NotFoundError("The server does not exist.", err)
	}
	for _, field := range hostAutoFields {
		e.Record.Set(field, stored.Get(field))
	}
	return e.Next()
}

// hostAutoBody is a server's automatic-backup settings as the route takes
// them, whole: an empty schedule is the staggered daily default, no targets
// none, and a null retention 7 daily, 4 weekly and 6 monthly.
type hostAutoBody struct {
	Schedule  string            `json:"schedule"`
	Targets   []string          `json:"targets"`
	Retention *stacks.Retention `json:"retention"`
}

// handleHostAutoBackups sets a server's automatic-backup settings, and gives
// every automatic job of the server what it does not set for itself, with its
// problem and next run again, in one transaction.
func (p *Panel) handleHostAutoBackups(e *core.RequestEvent) error {
	host, err := e.App.FindRecordById(schema.Hosts, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The server does not exist.", err)
	}
	var body hostAutoBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}
	schedule := stacks.NormalSchedule(body.Schedule)
	if schedule != "" && !stacks.ValidSchedule(schedule) {
		return e.BadRequestError(stacks.ScheduleProblem, nil)
	}
	targets := []string{}
	for _, id := range body.Targets {
		if id = strings.TrimSpace(id); id != "" && !slices.Contains(targets, id) {
			targets = append(targets, id)
		}
	}
	if len(targets) > maxJobTargets {
		return e.BadRequestError(fmt.Sprintf("A job backs up to at most %d targets.", maxJobTargets), nil)
	}
	found, err := e.App.FindRecordsByIds(schema.BackupTargets, targets)
	if err != nil || len(found) != len(targets) {
		return e.BadRequestError("One of the targets does not exist.", nil)
	}
	for _, target := range found {
		if target.GetString("kind") == backup.KindLocal && target.GetString("host") != host.Id {
			return e.BadRequestError(localElsewhere(target), nil)
		}
	}
	var retention any
	if body.Retention != nil {
		if !body.Retention.Valid() {
			return e.BadRequestError("Each count of the retention is between 0 and 10000.", nil)
		}
		retention = *body.Retention
	}

	ctx := audit.WithActor(e.Request.Context(), audit.ActorFromRequest(e))
	changed := 0
	err = e.App.RunInTransaction(func(tx core.App) error {
		changed = 0
		fresh, err := tx.FindRecordById(schema.Hosts, host.Id)
		if err != nil {
			return err
		}
		fresh.Set("auto_backup_schedule", schedule)
		fresh.Set("auto_backup_targets", targets)
		fresh.Set("auto_backup_retention", retention)
		if err := tx.SaveWithContext(ctx, fresh); err != nil {
			return err
		}
		host = fresh
		settings := autoSettingsOf(tx, fresh)
		jobs, err := tx.FindRecordsByFilter(schema.BackupJobs, "host={:host} && (origin={:database} || origin={:site})", "created", 0, 0,
			dbx.Params{"host": fresh.Id, "database": schema.JobDatabase, "site": schema.JobSite})
		if err != nil {
			return err
		}
		now := time.Now()
		for _, job := range jobs {
			before, kept := followed(job), derived(job)
			followSettings(job, settings)
			job.Set("problem", jobProblem(job))
			job.Set("next_run_at", nextRunOf(job, now))
			if followed(job) == before && derived(job) == kept {
				continue
			}
			if err := tx.SaveWithContext(ctx, job); err != nil {
				return err
			}
			if followed(job) != before {
				changed++
			}
		}
		return nil
	})
	if err != nil {
		return e.BadRequestError("The settings could not be saved: "+saveErrorText(err), nil)
	}
	return e.JSON(http.StatusOK, map[string]any{"host": host, "jobs_changed": changed})
}

// followed is what a job follows of its server's settings, to compare.
func followed(job *core.Record) string {
	raw, _ := json.Marshal([]any{job.GetString("schedule"), job.GetStringSlice("targets"), retentionOf(job)})
	return string(raw)
}

// derived is what follows from a job's settings, to compare.
func derived(job *core.Record) string {
	return job.GetString("problem") + "\x00" + job.GetDateTime("next_run_at").String()
}
