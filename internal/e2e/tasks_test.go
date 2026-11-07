package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/sites"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// taskRuns answers the runs of scheduled tasks in the fake engine by their
// command, and keeps what was run.
type taskRuns struct {
	mu   sync.Mutex
	runs []*fakedocker.ExecCall
	// env is what env prints, as the container's variables
	env string
}

func (r *taskRuns) answer(call *fakedocker.ExecCall, _ []byte) fakedocker.ExecResult {
	if len(call.Cmd) < 7 || call.Cmd[3] != "pocketcloud-task" {
		return fakedocker.ExecResult{}
	}
	r.mu.Lock()
	r.runs = append(r.runs, call)
	r.mu.Unlock()
	switch command := call.Cmd[6]; {
	case strings.HasPrefix(command, "wp cron"):
		return fakedocker.ExecResult{Stdout: "Executed the cron event 'wp_version_check'.\nSuccess: Executed a total of 2 cron events.\n"}
	case command == "env":
		return fakedocker.ExecResult{Stdout: r.env}
	case strings.HasPrefix(command, "sleep"):
		// as timeout reports a command it killed, at the timeout
		time.Sleep(1100 * time.Millisecond)
		return fakedocker.ExecResult{Code: 137}
	default:
		return fakedocker.ExecResult{Code: 1, Stdout: "boom\n"}
	}
}

func (r *taskRuns) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}

func (s *sitesHarness) task(id string) *core.Record {
	s.t.Helper()
	record, err := s.app.FindRecordById(schema.ScheduledTasks, id)
	if err != nil {
		s.t.Fatal(err)
	}
	return record
}

// TestAPHPSitesTaskRunsOnItsSchedule: a scheduled task of a PHP site (#51)
// runs in its PHP-FPM as www-data in its live release when its schedule says
// so, and keeps how its last run went; Run now runs it at once; a run that
// fails is in the events, and a few in a row raise an alert; a run that hangs
// is stopped at its timeout and reported.
func TestAPHPSitesTaskRunsOnItsSchedule(t *testing.T) {
	s := newSitesHarness(t)
	siteID := s.newSite(map[string]any{"name": "blog", "type": schema.SitePHPApp, "source": sites.SourceUpload})
	status, out := s.request(http.MethodPost, "/api/pc/sites/"+siteID+"/php/apply", s.operator, nil)
	if status != http.StatusOK {
		t.Fatalf("apply: %d %v", status, out)
	}
	s.waitForStatus(out["command"].(string), schema.CommandSucceeded, schema.CommandFailed)
	runs := &taskRuns{}
	s.docker.OnExec = runs.answer

	// a viewer cannot make one, nor can a schedule that is no schedule
	task := map[string]any{"target_kind": schema.TaskTargetSite, "site": siteID, "schedule": "*/5 * * * *",
		"command": "wp cron event run --due-now", "enabled": true}
	if status, _ := s.request(http.MethodPost, "/api/collections/scheduled_tasks/records", s.viewer, task); status != http.StatusForbidden && status != http.StatusBadRequest {
		t.Errorf("a viewer made a task: %d", status)
	}
	bad := map[string]any{"target_kind": schema.TaskTargetSite, "site": siteID, "schedule": "every five minutes", "command": "true"}
	if status, out := s.request(http.MethodPost, "/api/collections/scheduled_tasks/records", s.operator, bad); status != http.StatusBadRequest || out["data"].(map[string]any)["schedule"] == nil {
		t.Errorf("a schedule that is none: %d %v", status, out)
	}
	status, out = s.request(http.MethodPost, "/api/collections/scheduled_tasks/records", s.operator, task)
	if status != http.StatusOK {
		t.Fatalf("a task: %d %v", status, out)
	}
	taskID := out["id"].(string)
	if record := s.task(taskID); record.GetString("host") != s.host.Id || record.GetInt("timeout_s") != 300 {
		t.Errorf("the task: %v", record.PublicExport())
	}

	// not at 12:03, at 12:05
	s.panel.RunDueTasks(time.Date(2026, 11, 7, 12, 3, 0, 0, time.UTC))
	if runs.count() != 0 {
		t.Fatalf("ran off its schedule")
	}
	s.panel.RunDueTasks(time.Date(2026, 11, 7, 12, 5, 0, 0, time.UTC))
	s.waitFor("the run to be kept", func() bool {
		return s.task(taskID).GetString("last_status") == schema.TaskSucceeded
	})
	record := s.task(taskID)
	if !strings.Contains(record.GetString("last_output"), "Executed a total of 2 cron events") || record.GetInt("last_exit_code") != 0 {
		t.Errorf("the last run: %v", record.PublicExport())
	}
	call := runs.runs[0]
	if call.User != "www-data" || call.WorkingDir != "/srv/sites/blog/current" || call.Cmd[4] != "300" || call.Cmd[6] != "wp cron event run --due-now" {
		t.Errorf("the run: user %q, dir %q, %v", call.User, call.WorkingDir, call.Cmd)
	}
	if found := s.docker.Find("blog-php"); found == nil || call.Container != found.ID {
		t.Errorf("ran in %s", call.Container)
	}

	// the output, which viewers read, hides the site's secrets, and no
	// command keeps them
	const secret = "base64:task-sekrit-5150"
	status, out = s.request(http.MethodPut, "/api/pc/sites/"+siteID+"/php/secrets", s.operator, map[string]any{"app-key": secret})
	if status != http.StatusOK {
		t.Fatalf("secrets: %d %v", status, out)
	}
	s.appliedPHP(out)
	status, out = s.request(http.MethodPut, "/api/pc/sites/"+siteID+"/php", s.operator, map[string]any{"env": "APP_KEY=${secret:app-key}"})
	if status != http.StatusOK {
		t.Fatalf("settings: %d %v", status, out)
	}
	s.appliedPHP(out)
	runs.env = "HOME=/var/www\nAPP_KEY=" + secret + "\n"
	status, out = s.request(http.MethodPatch, "/api/collections/scheduled_tasks/records/"+taskID, s.operator, map[string]any{"command": "env"})
	if status != http.StatusOK {
		t.Fatalf("edit: %d %v", status, out)
	}
	status, out = s.request(http.MethodPost, "/api/pc/tasks/"+taskID+"/run", s.operator, nil)
	if status != http.StatusOK {
		t.Fatalf("run: %d %v", status, out)
	}
	s.waitForStatus(out["command"].(string), schema.CommandSucceeded, schema.CommandFailed)
	s.waitFor("the run to be kept", func() bool {
		task := s.task(taskID)
		return task.GetString("last_command") == out["command"] && task.GetString("last_status") != schema.TaskRunning
	})
	if output := s.task(taskID).GetString("last_output"); strings.Contains(output, secret) || !strings.Contains(output, "APP_KEY=***") {
		t.Errorf("the output shows the secret: %q", output)
	}
	if _, raw := testutil.Do(t, s.handler, testutil.Request{URL: "/api/collections/commands/records?perPage=200", Token: s.operator}); strings.Contains(raw, secret) {
		t.Error("a command shows the secret")
	}

	// a failing command, three times in a row: each is an event, the third an alert
	status, out = s.request(http.MethodPatch, "/api/collections/scheduled_tasks/records/"+taskID, s.operator, map[string]any{"command": "false"})
	if status != http.StatusOK {
		t.Fatalf("edit: %d %v", status, out)
	}
	for i := range 3 {
		status, out := s.request(http.MethodPost, "/api/pc/tasks/"+taskID+"/run", s.operator, nil)
		if status != http.StatusOK {
			t.Fatalf("run %d: %d %v", i, status, out)
		}
		s.waitForStatus(out["command"].(string), schema.CommandSucceeded, schema.CommandFailed)
		s.waitFor("the failure to be kept", func() bool { return s.task(taskID).GetInt("failures") == i+1 })
	}
	if record := s.task(taskID); record.GetString("last_status") != schema.TaskFailed || !strings.Contains(record.GetString("last_output"), "boom") ||
		record.GetInt("last_exit_code") != 1 {
		t.Errorf("a failed run: %v", record.PublicExport())
	}
	// the events follow the task's own record
	var failing, failed []*core.Record
	s.waitFor("the events of the failures", func() bool {
		failing, _ = s.app.FindRecordsByFilter(schema.Events, "kind = 'task.failing'", "", 0, 0)
		failed, _ = s.app.FindRecordsByFilter(schema.Events, "kind = 'task.failed'", "", 0, 0)
		return len(failing) >= 1 && len(failed) >= 3
	})
	if len(failing) != 1 || len(failed) != 3 || failing[0].GetString("severity") != schema.SeverityCritical {
		t.Errorf("events: %d failing, %d failed", len(failing), len(failed))
	}
	if status, _ := s.request(http.MethodPost, "/api/pc/tasks/"+taskID+"/run", s.viewer, nil); status != http.StatusForbidden {
		t.Errorf("a viewer ran a task: %d", status)
	}

	// a run that hangs is killed at its timeout and reported
	status, out = s.request(http.MethodPatch, "/api/collections/scheduled_tasks/records/"+taskID, s.operator,
		map[string]any{"command": "sleep 600", "timeout_s": 1})
	if status != http.StatusOK {
		t.Fatalf("edit: %d %v", status, out)
	}
	status, out = s.request(http.MethodPost, "/api/pc/tasks/"+taskID+"/run", s.operator, nil)
	if status != http.StatusOK {
		t.Fatalf("run: %d %v", status, out)
	}
	s.waitForStatus(out["command"].(string), schema.CommandSucceeded, schema.CommandFailed)
	s.waitFor("the timeout to be kept", func() bool { return s.task(taskID).GetString("last_status") == schema.TaskTimedOut })
	if output := s.task(taskID).GetString("last_output"); !strings.Contains(output, "past its timeout of 1s and was killed") {
		t.Errorf("the hung run: %q", output)
	}
	var timedOut []*core.Record
	s.waitFor("the timeout in the events", func() bool {
		timedOut, _ = s.app.FindRecordsByFilter(schema.Events, "kind = 'task.failed' && message ~ 'timeout'", "", 0, 0)
		return len(timedOut) >= 1
	})
	if len(timedOut) != 1 {
		t.Errorf("the timeout is in the events %d times", len(timedOut))
	}

	// a run never starts next to one still going
	status, out = s.request(http.MethodPatch, "/api/collections/scheduled_tasks/records/"+taskID, s.operator,
		map[string]any{"timeout_s": 5})
	if status != http.StatusOK {
		t.Fatalf("edit: %d %v", status, out)
	}
	before := runs.count()
	status, first := s.request(http.MethodPost, "/api/pc/tasks/"+taskID+"/run", s.operator, nil)
	if status != http.StatusOK {
		t.Fatalf("run: %d %v", status, first)
	}
	if status, _ := s.request(http.MethodPost, "/api/pc/tasks/"+taskID+"/run", s.operator, nil); status != http.StatusConflict {
		t.Errorf("a second run started: %d", status)
	}
	s.waitForStatus(first["command"].(string), schema.CommandSucceeded, schema.CommandFailed)
	if runs.count() != before+1 {
		t.Errorf("%d runs", runs.count()-before)
	}

	// a static site has no container to run tasks in
	static := s.newSite(map[string]any{"name": "docs", "source": sites.SourceUpload})
	if status, _ := s.request(http.MethodPost, "/api/collections/scheduled_tasks/records", s.operator,
		map[string]any{"target_kind": schema.TaskTargetSite, "site": static, "schedule": "@daily", "command": "true"}); status != http.StatusBadRequest {
		t.Errorf("a task of a static site: %d", status)
	}
	// removing the site removes its tasks
	if status, _ := s.request(http.MethodDelete, "/api/collections/sites/records/"+siteID, s.operator, nil); status != http.StatusNoContent {
		t.Fatalf("remove: %d", status)
	}
	if left, _ := s.app.FindRecordsByFilter(schema.ScheduledTasks, "site = {:site}", "", 0, 0, dbx.Params{"site": siteID}); len(left) != 0 {
		t.Errorf("%d tasks are left", len(left))
	}
}

// TestAStacksTaskRunsInItsServicesContainer: a task of a stack (#51) runs in
// a running container of its service, as the user it names.
func TestAStacksTaskRunsInItsServicesContainer(t *testing.T) {
	h := newHarness(t)
	labels := map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.service": "app"}
	app := h.docker.AddContainer("shop-app-1", "nextcloud:31", labels)
	h.docker.AddContainer("shop-db-1", "mariadb:11", map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.service": "db"})
	runs := &taskRuns{}
	h.docker.OnExec = runs.answer
	_, host := h.enrolled("stacked")
	h.waitFor("the inventory", func() bool {
		_, err := h.app.FindFirstRecordByData(schema.Containers, "docker_id", app)
		return err == nil
	})
	stack := h.saveStack(host.Id, "shop", "services: {}\n")
	_, operator := testutil.NewUser(t, h.app, "op@example.com", schema.RoleOperator)

	request := func(method, url string, body string) (int, map[string]any) {
		status, raw := testutil.Do(t, h.handler, testutil.Request{Method: method, URL: url, Token: operator, Body: body})
		var out map[string]any
		_ = json.Unmarshal([]byte(raw), &out)
		return status, out
	}
	status, out := request(http.MethodPost, "/api/collections/scheduled_tasks/records",
		`{"target_kind":"stack","stack":"`+stack+`","service":"app","user":"www-data","schedule":"*/5 * * * *","command":"php occ background:cron","enabled":true}`)
	if status != http.StatusOK {
		t.Fatalf("a task: %d %v", status, out)
	}
	taskID := out["id"].(string)
	if status, out := request(http.MethodPost, "/api/collections/scheduled_tasks/records",
		`{"target_kind":"stack","stack":"`+stack+`","service":"app","user":"root; rm -rf /","schedule":"@daily","command":"true"}`); status != http.StatusBadRequest {
		t.Errorf("a user that is none: %d %v", status, out)
	}
	status, out = request(http.MethodPost, "/api/pc/tasks/"+taskID+"/run", "")
	if status != http.StatusOK {
		t.Fatalf("run: %d %v", status, out)
	}
	h.waitForStatus(out["command"].(string), schema.CommandSucceeded, schema.CommandFailed)
	if runs.count() != 1 || runs.runs[0].Container != app || runs.runs[0].User != "www-data" || runs.runs[0].WorkingDir != "" {
		t.Fatalf("the runs: %d", runs.count())
	}

	// the output hides the stack's secrets
	const secret = "stack-sekrit-7788"
	if status, out := request(http.MethodPost, "/api/pc/stacks/"+stack+"/save", `{"secrets":{"set":{"DB_PASS":"`+secret+`"}}}`); status != http.StatusOK {
		t.Fatalf("secrets: %d %v", status, out)
	}
	runs.env = "DB_PASS=" + secret + "\n"
	if status, out := request(http.MethodPatch, "/api/collections/scheduled_tasks/records/"+taskID, `{"command":"env"}`); status != http.StatusOK {
		t.Fatalf("edit: %d %v", status, out)
	}
	status, out = request(http.MethodPost, "/api/pc/tasks/"+taskID+"/run", "")
	if status != http.StatusOK {
		t.Fatalf("run: %d %v", status, out)
	}
	h.waitForStatus(out["command"].(string), schema.CommandSucceeded, schema.CommandFailed)
	h.waitFor("the run to be kept", func() bool {
		task, err := h.app.FindRecordById(schema.ScheduledTasks, taskID)
		return err == nil && task.GetString("last_command") == out["command"] && task.GetString("last_status") != schema.TaskRunning
	})
	if task, _ := h.app.FindRecordById(schema.ScheduledTasks, taskID); strings.Contains(task.GetString("last_output"), secret) ||
		!strings.Contains(task.GetString("last_output"), "DB_PASS=***") {
		t.Errorf("the output shows the secret: %q", task.GetString("last_output"))
	}

	// a service without a running container says so
	status, out = request(http.MethodPatch, "/api/collections/scheduled_tasks/records/"+taskID, `{"service":"worker"}`)
	if status != http.StatusOK {
		t.Fatalf("edit: %d %v", status, out)
	}
	if status, out := request(http.MethodPost, "/api/pc/tasks/"+taskID+"/run", ""); status != http.StatusBadRequest ||
		!strings.Contains(out["message"].(string), "has no running container") {
		t.Errorf("a service without a container: %d %v", status, out)
	}
}
