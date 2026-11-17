package commands_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// TestAStartIsToldOnce (#29): OnStart hears of a command once, as its agent
// says it started it; later progress, its end, and a command that ends
// before its agent says it started do not count as starts.
func TestAStartIsToldOnce(t *testing.T) {
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)
	manager := commands.NewManager(app)
	manager.Register()
	host := testutil.NewHost(t, app, "runner")

	var started []string
	manager.OnStart("image.pull", func(record *core.Record) {
		if record.GetString("status") != schema.CommandRunning || record.GetDateTime("started_at").IsZero() {
			t.Errorf("told of a start as %s, started at %s", record.GetString("status"), record.GetString("started_at"))
		}
		started = append(started, record.Id)
	})
	finished := 0
	manager.OnFinish("image.pull", func(*core.Record) { finished++ })

	dispatched := func() *core.Record {
		t.Helper()
		record, err := manager.Create(context.Background(), commands.Options{Host: host.Id,
			Command: &agentv1.Command{Payload: &agentv1.Command_ImagePull{ImagePull: &agentv1.ImagePull{Image: "nginx:1"}}}})
		if err != nil {
			t.Fatal(err)
		}
		record.Set("status", schema.CommandDispatched)
		if err := app.Save(record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	update := func(id string, sequence uint64, state agentv1.CommandState) {
		t.Helper()
		if _, err := manager.Update(host.Id, &agentv1.ReportCommandUpdateRequest{CommandId: id, Sequence: sequence, State: state}); err != nil {
			t.Fatal(err)
		}
	}

	first := dispatched()
	update(first.Id, 1, agentv1.CommandState_COMMAND_STATE_RUNNING)
	update(first.Id, 2, agentv1.CommandState_COMMAND_STATE_RUNNING)
	update(first.Id, 3, agentv1.CommandState_COMMAND_STATE_SUCCEEDED)
	// one that ends at once
	second := dispatched()
	update(second.Id, 1, agentv1.CommandState_COMMAND_STATE_FAILED)

	if len(started) != 1 || started[0] != first.Id || finished != 2 {
		t.Errorf("started %v, finished %d", started, finished)
	}
}

// TestACommandMayWaitLongerThanTheQueue (#29): a command that waits for its
// resource, as a backup behind another one, may wait longer than the queue's
// TTL.
func TestACommandMayWaitLongerThanTheQueue(t *testing.T) {
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)
	manager := commands.NewManager(app)
	manager.Register()
	host := testutil.NewHost(t, app, "runner")

	pull := &agentv1.Command{Payload: &agentv1.Command_ImagePull{ImagePull: &agentv1.ImagePull{Image: "nginx:1"}}}
	for _, test := range []struct {
		wait, want time.Duration
	}{{0, time.Hour}, {12 * time.Hour, 12 * time.Hour}} {
		record, err := manager.Create(context.Background(), commands.Options{Host: host.Id, Command: pull, QueueTimeout: test.wait})
		if err != nil {
			t.Fatal(err)
		}
		if left := time.Until(record.GetDateTime("expires_at").Time()); left < test.want-time.Minute || left > test.want {
			t.Errorf("waiting %s: expires in %s", test.wait, left)
		}
	}
}

// TestRESTCannotSetCancelRequested (#30): only the queue marks a command as
// asked to stop. A request that set the mark on a new command would have a
// restore's cancel answered before anyone stopped it, and the reason stand
// for someone who never asked.
func TestRESTCannotSetCancelRequested(t *testing.T) {
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)
	manager := commands.NewManager(app)
	manager.Register()
	host := testutil.NewHost(t, app, "runner")
	_, operator := testutil.NewUser(t, app, "operator@example.com", schema.RoleOperator)
	_, superuser := testutil.NewSuperuser(t, app)
	handler := testutil.Handler(t, app)

	for name, token := range map[string]string{"an operator": operator, "a superuser": superuser} {
		body := `{"type":"container.start","host":"` + host.Id + `","payload":{"docker_id":"x"},` +
			`"cancel_requested_at":"2026-01-01 00:00:00.000Z","cancel_reason":"someone else"}`
		status, response := testutil.Do(t, handler, testutil.Request{Method: "POST", URL: "/api/collections/commands/records", Token: token, Body: body})
		if status != 200 {
			t.Fatalf("%s: %d %s", name, status, response)
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(response), &created); err != nil {
			t.Fatal(err)
		}
		record, err := app.FindRecordById(schema.Commands, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !record.GetDateTime("cancel_requested_at").IsZero() || record.GetString("cancel_reason") != "" {
			t.Errorf("%s set the cancel mark: %v, %q", name, record.GetDateTime("cancel_requested_at"), record.GetString("cancel_reason"))
		}
	}
}

// TestACommandEndsOnce: a final answer that comes twice at once, as over a
// stream that dropped and over the one that replaced it, and in the agent's
// list of finished commands as it reconnects, ends the command once and
// runs its finishers once (a finisher that runs twice may forget what the
// first run knew, as a target's check did with a stale result). The first
// answer's save is held, so that the second reads the command before it ends.
func TestACommandEndsOnce(t *testing.T) {
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)
	manager := commands.NewManager(app)
	manager.Register()
	host := testutil.NewHost(t, app, "runner")
	var finished atomic.Int32
	manager.OnFinish("image.pull", func(*core.Record) { finished.Add(1) })
	var held atomic.Bool
	app.OnRecordUpdateExecute(schema.Commands).BindFunc(func(e *core.RecordEvent) error {
		if e.Record.GetString("status") == schema.CommandFailed && held.CompareAndSwap(false, true) {
			time.Sleep(300 * time.Millisecond)
		}
		return e.Next()
	})

	record, err := manager.Create(context.Background(), commands.Options{Host: host.Id,
		Command: &agentv1.Command{Payload: &agentv1.Command_ImagePull{ImagePull: &agentv1.ImagePull{Image: "nginx:1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	record.Set("status", schema.CommandDispatched)
	if err := app.Save(record); err != nil {
		t.Fatal(err)
	}
	var answers sync.WaitGroup
	answers.Add(2)
	go func() {
		defer answers.Done()
		_, _ = manager.Update(host.Id, &agentv1.ReportCommandUpdateRequest{CommandId: record.Id, Sequence: 1,
			State: agentv1.CommandState_COMMAND_STATE_FAILED, Error: "it failed"})
	}()
	go func() {
		defer answers.Done()
		time.Sleep(100 * time.Millisecond)
		manager.Connect(host.Id, nil, []*agentv1.FinishedCommand{{CommandId: record.Id,
			State: agentv1.CommandState_COMMAND_STATE_FAILED, Error: "it failed"}})
	}()
	answers.Wait()
	if got := finished.Load(); got != 1 {
		t.Errorf("the finishers ran %d times", got)
	}
	if fresh, err := app.FindRecordById(schema.Commands, record.Id); err != nil || fresh.GetString("status") != schema.CommandFailed {
		t.Errorf("the command: %v %v", fresh, err)
	}
}
