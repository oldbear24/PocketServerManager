package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/cron"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/sites"
	"github.com/oldbear24/PocketServerManager/internal/stacks"
)

// Scheduled tasks (#51): commands that the panel runs on a cron schedule in a
// PHP site's PHP-FPM container, as www-data in its live release, or in a
// service of a stack. A run is a task.run command; a run never starts while
// the one before it is still going, a run that hangs is killed at the task's
// timeout, and a task that keeps failing raises an alert.

const (
	// defaultTaskTimeout is a task's timeout when it does not say.
	defaultTaskTimeout = 5 * 60
	// maxTaskTimeoutS is the longest a task may run, as the agent allows.
	maxTaskTimeoutS = 24 * 60 * 60
	// taskAlertAfter is how many failed runs in a row raise an alert.
	taskAlertAfter = 3
	// keepTaskRuns is how long the commands of runs stay as their history.
	keepTaskRuns = 7 * 24 * time.Hour
	// taskQueueGrace is how long a run may wait for its agent on top of its
	// timeout before its command times out.
	taskQueueGrace = 5 * time.Minute
)

// taskUser is a user a stack's container may run a task as: a name or a
// number, with an optional group.
var taskUser = regexp.MustCompile(`^([a-z_][a-z0-9_.-]{0,31}|[0-9]{1,10})(:([a-z_][a-z0-9_.-]{0,31}|[0-9]{1,10}))?$`)

// errTaskRunning says that a task's previous run is still going.
var errTaskRunning = errors.New("the previous run of this task is still going")

func (p *Panel) registerTasks() {
	p.App.OnRecordValidate(schema.ScheduledTasks).BindFunc(p.validateTask)
	p.Commands.OnFinish("task.run", p.taskFinished)
	p.Commands.RegisterEnricher("task.run", p.enrichTaskRun)
	p.App.Cron().MustAdd("pocketcloud_tasks", "* * * * *", func() { p.RunDueTasks(time.Now()) })
	p.App.Cron().MustAdd("pocketcloud_task_runs_retention", "20 4 * * *", p.pruneTaskRuns)
}

func (p *Panel) bindTaskRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	operators := requireRole(schema.RoleOperator, schema.RoleAdmin)
	group.POST("/tasks/{id}/run", p.handleRunTask).BindFunc(operators)
}

// validateTask checks a task and gives it its server: that of its site or
// stack, which it keeps.
func (p *Panel) validateTask(e *core.RecordEvent) error {
	task := e.Record
	errs := validation.Errors{}
	set := func(field, code, message string) { errs[field] = validation.NewError(code, message) }

	if _, err := cron.NewSchedule(strings.TrimSpace(task.GetString("schedule"))); err != nil {
		set("schedule", "validation_task_schedule", "A schedule has five fields, minute hour day month weekday, such as */5 * * * *, or is one of @hourly, @daily, @weekly, @monthly and @yearly.")
	}
	task.Set("schedule", strings.TrimSpace(task.GetString("schedule")))
	if strings.TrimSpace(task.GetString("command")) == "" {
		set("command", "validation_task_command", "A task needs a command.")
	}
	if task.GetInt("timeout_s") == 0 {
		task.Set("timeout_s", defaultTaskTimeout)
	}
	if timeout := task.GetInt("timeout_s"); timeout < 1 || timeout > maxTaskTimeoutS {
		set("timeout_s", "validation_task_timeout", fmt.Sprintf("A task's timeout is between 1 and %d seconds.", maxTaskTimeoutS))
	}

	var host string
	switch task.GetString("target_kind") {
	case schema.TaskTargetSite:
		site, err := e.App.FindRecordById(schema.Sites, task.GetString("site"))
		switch {
		case err != nil:
			set("site", "validation_task_site", "The site does not exist.")
		case site.GetString("type") != schema.SitePHPApp:
			set("site", "validation_task_site_php", "Only a PHP site has a container to run tasks in.")
		default:
			host = site.GetString("host")
		}
		// its PHP-FPM runs it as www-data
		task.Set("stack", "")
		task.Set("service", "")
		task.Set("user", "")
	case schema.TaskTargetStack:
		stack, err := e.App.FindRecordById(schema.Stacks, task.GetString("stack"))
		if err != nil {
			set("stack", "validation_task_stack", "The stack does not exist.")
		} else {
			host = stack.GetString("host")
		}
		task.Set("service", strings.TrimSpace(task.GetString("service")))
		if task.GetString("service") == "" {
			set("service", "validation_task_service", "Name the service whose container runs the task.")
		}
		if user := task.GetString("user"); user != "" && !taskUser.MatchString(user) {
			set("user", "validation_task_user", "A user is a name or a number, with a group after a colon if any.")
		}
		task.Set("site", "")
	default:
		set("target_kind", "validation_task_target", "A task runs in a PHP site or in a stack's service.")
	}
	if original := task.Original(); !task.IsNew() && original.GetString("host") != "" && host != "" && original.GetString("host") != host {
		set("host", "validation_task_host", "A task stays on its server.")
	}
	if host != "" {
		task.Set("host", host)
	}
	if len(errs) > 0 {
		return errs
	}
	return e.Next()
}

// RunDueTasks starts the enabled tasks whose schedule says now, in UTC: the
// panel calls it every minute.
func (p *Panel) RunDueTasks(now time.Time) {
	tasks, err := p.App.FindRecordsByFilter(schema.ScheduledTasks, "enabled = true", "created", 0, 0)
	if err != nil {
		p.Log.Error("failed to read the scheduled tasks", "error", err)
		return
	}
	moment := cron.NewMoment(now.UTC())
	for _, task := range tasks {
		if !taskDue(task.GetString("schedule"), moment) {
			continue
		}
		if _, err := p.startTask(context.Background(), task, commands.Options{Requester: "schedule"}); err != nil && !errors.Is(err, errTaskRunning) {
			p.Log.Warn("a scheduled task could not start", "task", task.Id, "error", err)
		}
	}
}

// startTask runs a task now: from its schedule or by hand.
func (p *Panel) startTask(ctx context.Context, task *core.Record, options commands.Options) (*core.Record, error) {
	if task.GetString("last_status") == schema.TaskRunning {
		if previous, err := p.App.FindRecordById(schema.Commands, task.GetString("last_command")); err == nil && !commandFinal(previous) {
			p.setFields(task, map[string]any{"last_status": schema.TaskRunning})
			events.Log(p.App, task.GetString("host"), "task.skipped", schema.SeverityWarning,
				fmt.Sprintf("%s: a run was skipped, as the one before it is still going.", taskLabel(task)),
				map[string]any{"task": task.Id, "command": previous.Id})
			return nil, errTaskRunning
		}
	}
	run, err := p.taskRun(task)
	if err != nil {
		failures := task.GetInt("failures") + 1
		p.setFields(task, map[string]any{
			"last_status": schema.TaskFailed, "last_run_at": types.NowDateTime(), "last_output": truncate(err.Error(), 4096),
			"last_exit_code": 0, "last_duration_ms": 0, "failures": failures,
		})
		p.taskFailed(task, failures, err.Error())
		return nil, err
	}
	options.Host = task.GetString("host")
	options.Label = taskLabel(task)
	options.Timeout = time.Duration(task.GetInt("timeout_s"))*time.Second + taskQueueGrace
	options.Command = &agentv1.Command{Payload: &agentv1.Command_TaskRun{TaskRun: run}}
	command, err := p.Commands.Create(ctx, options)
	if err != nil {
		return nil, err
	}
	p.setFields(task, map[string]any{"last_status": schema.TaskRunning, "last_run_at": types.NowDateTime(), "last_command": command.Id})
	return command, nil
}

// taskRun is what the agent runs: in a PHP site's PHP-FPM, as www-data in its
// live release, or in a running container of a stack's service.
func (p *Panel) taskRun(task *core.Record) (*agentv1.TaskRun, error) {
	run := &agentv1.TaskRun{
		TaskId:   task.Id,
		Command:  task.GetString("command"),
		TimeoutS: int32(task.GetInt("timeout_s")), //nolint:gosec // at most a day
	}
	switch task.GetString("target_kind") {
	case schema.TaskTargetSite:
		site, err := p.App.FindRecordById(schema.Sites, task.GetString("site"))
		if err != nil {
			return nil, errors.New("the site is gone")
		}
		name := site.GetString("name")
		run.ContainerId = sites.PHPContainer(name)
		run.User = "www-data"
		run.WorkingDir = sites.Root + "/" + name + "/current"
	case schema.TaskTargetStack:
		stack, err := p.App.FindRecordById(schema.Stacks, task.GetString("stack"))
		if err != nil {
			return nil, errors.New("the stack is gone")
		}
		containers, err := p.App.FindRecordsByFilter(schema.Containers,
			"host={:host} && project={:project} && service={:service} && state='running'", "name", 1, 0,
			dbx.Params{"host": stack.GetString("host"), "project": stack.GetString("name"), "service": task.GetString("service")})
		if err != nil || len(containers) == 0 {
			return nil, fmt.Errorf("the service %s of %s has no running container", task.GetString("service"), stack.GetString("name"))
		}
		run.ContainerId = containers[0].GetString("docker_id")
		run.User = task.GetString("user")
	default:
		return nil, errors.New("the task has no target")
	}
	return run, nil
}

// enrichTaskRun adds the values of the secrets in the container's
// environment when the run is delivered, so that its output, which viewers
// read, hides them: a PHP site's own and its database's, the relay's
// password, or a stack's. They are never stored in the command.
func (p *Panel) enrichTaskRun(_ *core.Record, command *agentv1.Command) error {
	run := command.GetTaskRun()
	task, err := p.App.FindRecordById(schema.ScheduledTasks, run.GetTaskId())
	if err != nil {
		return fmt.Errorf("the task is gone: %w", err)
	}
	values := []string{}
	switch task.GetString("target_kind") {
	case schema.TaskTargetSite:
		site, err := p.App.FindRecordById(schema.Sites, task.GetString("site"))
		if err != nil {
			return fmt.Errorf("the site is gone: %w", err)
		}
		settings, err := p.phpSettings(p.App, site)
		if err != nil {
			return err
		}
		resolved, masked, err := p.phpSecretValues(site, settings)
		if err != nil {
			return err
		}
		values = append(values, masked...)
		smtp, err := p.smtpSecret(maps.Clone(resolved))
		if err != nil {
			return err
		}
		for _, value := range smtp {
			values = append(values, value)
		}
	case schema.TaskTargetStack:
		stack, err := p.App.FindRecordById(schema.Stacks, task.GetString("stack"))
		if err != nil {
			return fmt.Errorf("the stack is gone: %w", err)
		}
		own, err := p.openStackSecrets(stack)
		if err != nil {
			return err
		}
		for _, value := range own {
			values = append(values, value)
		}
		// what its variables take from elsewhere, such as a database's password
		resolve := p.secretResolver(stack)
		_, _, _ = stacks.ResolveEnv(stack.GetString("env"), func(name string) (string, error) {
			value, err := resolve(name)
			if err == nil {
				values = append(values, value)
			}
			return value, err
		})
	}
	run.Masked = values
	return nil
}

// handleRunTask runs a task now, whatever its schedule.
func (p *Panel) handleRunTask(e *core.RequestEvent) error {
	task, err := p.App.FindRecordById(schema.ScheduledTasks, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The task does not exist.", err)
	}
	id, label := requester(e)
	command, err := p.startTask(e.Request.Context(), task, commands.Options{RequestedBy: id, Requester: label})
	if errors.Is(err, errTaskRunning) {
		return e.Error(http.StatusConflict, "Its previous run is still going.", nil)
	}
	if err != nil {
		return e.BadRequestError(capitalize(err.Error())+".", nil)
	}
	return e.JSON(http.StatusOK, map[string]any{"command": command.Id})
}

// taskFinished keeps how a run went on its task.
func (p *Panel) taskFinished(command *core.Record) {
	var payload struct {
		TaskID string `json:"task_id"`
	}
	raw, _ := json.Marshal(command.Get("payload"))
	_ = json.Unmarshal(raw, &payload)
	task, err := p.App.FindRecordById(schema.ScheduledTasks, payload.TaskID)
	if err != nil {
		return
	}
	var result struct {
		ExitCode   *float64 `json:"exit_code"`
		DurationMs float64  `json:"duration_ms"`
		Output     string   `json:"output"`
		TimedOut   bool     `json:"timed_out"`
	}
	raw, _ = json.Marshal(command.Get("result"))
	_ = json.Unmarshal(raw, &result)

	status := schema.TaskFailed
	switch commandStatus := command.GetString("status"); {
	case commandStatus == schema.CommandSucceeded:
		status = schema.TaskSucceeded
	case result.TimedOut:
		status = schema.TaskTimedOut
	case commandStatus == schema.CommandCancelled:
		status = schema.TaskCancelled
	case commandStatus == schema.CommandTimedOut && command.GetString("started_at") == "":
		// never delivered, as to a server that stayed offline: no run of the task
		status = schema.TaskCancelled
	case commandStatus == schema.CommandTimedOut:
		status = schema.TaskTimedOut
	}
	output := result.Output
	if message := command.GetString("error"); status != schema.TaskSucceeded && message != "" {
		output = strings.TrimSpace(output + "\n" + message)
	}
	exitCode := -1
	if result.ExitCode != nil {
		exitCode = int(*result.ExitCode)
	}
	failures := 0
	switch status {
	case schema.TaskCancelled:
		failures = task.GetInt("failures")
	case schema.TaskSucceeded:
	default:
		failures = task.GetInt("failures") + 1
	}
	before := task.GetInt("failures")
	fields := map[string]any{
		"last_status": status, "last_exit_code": exitCode, "last_duration_ms": int(result.DurationMs),
		"last_output": truncate(output, 8000), "failures": failures,
	}
	// a later run of the task, as by hand, keeps its own state
	if task.GetString("last_command") != command.Id {
		delete(fields, "last_status")
	}
	p.setFields(task, fields)

	switch {
	case status == schema.TaskCancelled:
	case status == schema.TaskTimedOut:
		p.taskFailed(task, failures, fmt.Sprintf("it ran past its timeout of %d s and was killed", task.GetInt("timeout_s")))
	case status == schema.TaskFailed:
		reason := command.GetString("error")
		if reason == "" {
			reason = "it failed"
		}
		p.taskFailed(task, failures, reason)
	case before >= taskAlertAfter:
		events.Log(p.App, task.GetString("host"), "task.recovered", schema.SeverityInfo,
			fmt.Sprintf("%s runs again after %d failed runs.", taskLabel(task), before), map[string]any{"task": task.Id})
	}
}

// taskFailed records a failed run in the events, and alerts when the task
// has failed a few times in a row.
func (p *Panel) taskFailed(task *core.Record, failures int, reason string) {
	reason = strings.TrimSuffix(strings.TrimSpace(reason), ".")
	events.Log(p.App, task.GetString("host"), "task.failed", schema.SeverityWarning,
		truncate(fmt.Sprintf("%s failed: %s.", taskLabel(task), reason), 2048), map[string]any{"task": task.Id})
	if failures == taskAlertAfter {
		events.Log(p.App, task.GetString("host"), "task.failing", schema.SeverityCritical,
			fmt.Sprintf("%s failed %d times in a row.", taskLabel(task), failures), map[string]any{"task": task.Id})
	}
}

// pruneTaskRuns removes the commands of runs older than their history keeps.
func (p *Panel) pruneTaskRuns() {
	cutoff := types.NowDateTime().Add(-keepTaskRuns)
	_, err := p.App.DB().NewQuery(
		"DELETE FROM " + schema.Commands + " WHERE type = 'task.run' AND created < {:cutoff} AND status IN ('succeeded', 'failed', 'timed_out', 'cancelled')" +
			// a task's last run stays: the task points at it
			" AND id NOT IN (SELECT last_command FROM " + schema.ScheduledTasks + " WHERE last_command != '')",
	).Bind(dbx.Params{"cutoff": cutoff.String()}).Execute()
	if err != nil {
		p.Log.Error("failed to remove old task runs", "error", err)
	}
}

// taskDue says whether a schedule is due at a moment, as cron has it: when
// both the day of the month and the day of the week are restricted, either
// one matching is enough, where PocketBase's schedule asks for both.
func taskDue(expr string, moment *cron.Moment) bool {
	fields := strings.Fields(expr)
	if len(fields) == 5 && fields[2] != "*" && fields[4] != "*" {
		byDay, err1 := cron.NewSchedule(strings.Join([]string{fields[0], fields[1], fields[2], fields[3], "*"}, " "))
		byWeekday, err2 := cron.NewSchedule(strings.Join([]string{fields[0], fields[1], "*", fields[3], fields[4]}, " "))
		return err1 == nil && err2 == nil && (byDay.IsDue(moment) || byWeekday.IsDue(moment))
	}
	schedule, err := cron.NewSchedule(expr)
	return err == nil && schedule.IsDue(moment)
}

// taskLabel names a task in events and commands: its target and its command.
func taskLabel(task *core.Record) string {
	command := strings.Join(strings.Fields(task.GetString("command")), " ")
	return truncate("Task "+command, 120)
}

func commandFinal(command *core.Record) bool {
	switch command.GetString("status") {
	case schema.CommandSucceeded, schema.CommandFailed, schema.CommandTimedOut, schema.CommandCancelled:
		return true
	}
	return false
}
