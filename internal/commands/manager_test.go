package commands_test

import (
	"context"
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
