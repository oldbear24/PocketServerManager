package commands_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"google.golang.org/protobuf/types/known/structpb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// queue is a manager with a server whose agent is connected, and the
// helpers of the tests of cancels (#30).
type queue struct {
	t       *testing.T
	app     *tests.TestApp
	manager *commands.Manager
	host    *core.Record
	stream  *commands.Stream
	// finished counts the finishers called, by command
	finished map[string]int
}

func newQueue(t *testing.T) *queue {
	t.Helper()
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)
	manager := commands.NewManager(app)
	manager.Register()
	// deliveries under way end before the database closes
	t.Cleanup(manager.Close)
	q := &queue{t: t, app: app, manager: manager, host: testutil.NewHost(t, app, "runner"), finished: map[string]int{}}
	for commandType := range commands.Specs {
		manager.OnFinish(commandType, func(record *core.Record) { q.finished[record.Id]++ })
	}
	capabilities := make([]string, 0, len(commands.Specs))
	for commandType := range commands.Specs {
		capabilities = append(capabilities, commandType)
	}
	q.stream = manager.Connect(q.host.Id, capabilities, nil)
	return q
}

func restoreCommand(stack string) *agentv1.Command {
	return &agentv1.Command{Payload: &agentv1.Command_BackupRestore{BackupRestore: &agentv1.BackupRestore{
		Repository: &agentv1.BackupRepository{TargetId: "target", Url: "s3:https://s3.example.com/b"},
		SnapshotId: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Stack: stack, Overwrite: true,
		Original: []*agentv1.BackupRestoreRoot{{Source: "/volumes/notes_data", Volume: "notes_data"}},
	}}}
}

func pullCommand() *agentv1.Command {
	return &agentv1.Command{Payload: &agentv1.Command_ImagePull{ImagePull: &agentv1.ImagePull{Image: "nginx:1"}}}
}

// create queues a command and returns it as its agent got it.
func (q *queue) create(command *agentv1.Command) *core.Record {
	q.t.Helper()
	record, err := q.manager.Create(context.Background(), commands.Options{Host: q.host.Id, Command: command})
	if err != nil {
		q.t.Fatal(err)
	}
	q.manager.Kick(q.host.Id)
	return record
}

// next is the next message to the agent.
func (q *queue) next() *agentv1.SubscribeCommandsResponse {
	q.t.Helper()
	select {
	case message := <-q.stream.Out():
		return message
	case <-time.After(5 * time.Second):
		q.t.Fatal("nothing was sent to the agent")
		return nil
	}
}

// quiet says whether nothing more is sent to the agent for a moment.
func (q *queue) quiet() bool {
	select {
	case message := <-q.stream.Out():
		q.t.Logf("sent: %v", message)
		return false
	case <-time.After(200 * time.Millisecond):
		return true
	}
}

func (q *queue) update(id string, sequence uint64, state agentv1.CommandState, progress *agentv1.CommandProgress, result map[string]any, message string) bool {
	q.t.Helper()
	update := &agentv1.ReportCommandUpdateRequest{CommandId: id, Sequence: sequence, State: state, Progress: progress, Error: message}
	if result != nil {
		structured, err := structpb.NewStruct(result)
		if err != nil {
			q.t.Fatal(err)
		}
		update.Result = structured
	}
	cancel, err := q.manager.Update(q.host.Id, update)
	if err != nil {
		q.t.Fatal(err)
	}
	return cancel
}

func (q *queue) stored(id string) *core.Record {
	q.t.Helper()
	record, err := q.app.FindRecordById(schema.Commands, id)
	if err != nil {
		q.t.Fatal(err)
	}
	return record
}

// running is a command its agent got and started.
func (q *queue) running(command *agentv1.Command) *core.Record {
	q.t.Helper()
	record := q.create(command)
	if got := q.next(); got.GetCommand().GetId() != record.Id {
		q.t.Fatalf("delivered %v, not %s", got, record.Id)
	}
	if cancel := q.update(record.Id, 1, agentv1.CommandState_COMMAND_STATE_RUNNING, nil, nil, ""); cancel {
		q.t.Fatal("told to stop at its start")
	}
	return record
}

// TestAgentSettledCancelWaitsForTheAgent (#30, SAFE): a restore someone
// stops stays open, holding its stack, until its agent says how it ended:
// the agent starts the application again after the cancel, and its report
// of what it started is kept, not dropped as a cancelled command's would be.
func TestAgentSettledCancelWaitsForTheAgent(t *testing.T) {
	q := newQueue(t)
	restore := q.running(restoreCommand("notes"))
	before := time.Now().UTC()

	err := q.manager.Cancel(restore.Id, "Cancelled by operator@example.com")
	if !errors.Is(err, commands.ErrCancelRequested) {
		t.Fatalf("cancel: %v", err)
	}
	if cancel := q.next().GetCancel(); cancel.GetCommandId() != restore.Id || cancel.GetReason() != "Cancelled by operator@example.com" {
		t.Errorf("the agent was told %v", cancel)
	}
	stored := q.stored(restore.Id)
	progress := map[string]any{}
	_ = stored.UnmarshalJSONField("progress", &progress)
	if stored.GetString("status") != schema.CommandRunning || stored.GetDateTime("cancel_requested_at").Time().Before(before.Add(-time.Second)) ||
		stored.GetString("cancel_reason") != "Cancelled by operator@example.com" || progress["step"] != "cancelling" || progress["percent"] != float64(-1) {
		t.Errorf("after the cancel: %v", stored.FieldsData())
	}
	if expires := stored.GetDateTime("expires_at").Time(); expires.After(time.Now().Add(5*time.Minute)) || expires.Before(before.Add(4*time.Minute)) {
		t.Errorf("the agent has until %s to report", expires)
	}
	if q.finished[restore.Id] != 0 {
		t.Error("finished before its agent reported")
	}

	// the stack stays held: a deploy of it waits
	deploy := q.create(&agentv1.Command{Payload: &agentv1.Command_StackDeploy{StackDeploy: &agentv1.StackDeploy{Name: "notes"}}})
	if !q.quiet() {
		t.Error("a deploy of the stack went out while its restore was stopping")
	}

	result := map[string]any{"app_started": []any{"notes-db-1", "notes-app-1"}, "partial": true}
	q.update(restore.Id, 2, agentv1.CommandState_COMMAND_STATE_CANCELLED, nil, result,
		"The restore was stopped before it was complete: the destination holds part of the snapshot. The application was started again.")
	stored = q.stored(restore.Id)
	kept := map[string]any{}
	_ = stored.UnmarshalJSONField("result", &kept)
	if stored.GetString("status") != schema.CommandCancelled || stored.GetString("error") != "Cancelled by operator@example.com" ||
		len(kept["app_started"].([]any)) != 2 || kept["partial"] != true {
		t.Errorf("settled: %v", stored.FieldsData())
	}
	if q.finished[restore.Id] != 1 {
		t.Errorf("finished %d times", q.finished[restore.Id])
	}
	if got := q.next().GetCommand().GetId(); got != deploy.Id {
		t.Errorf("after the restore, %s went out, not the deploy", got)
	}

	// a restore that had succeeded when it heard of the cancel succeeded
	done := q.running(restoreCommand("other"))
	if err := q.manager.Cancel(done.Id, "Cancelled by operator@example.com"); !errors.Is(err, commands.ErrCancelRequested) {
		t.Fatal(err)
	}
	q.next()
	q.update(done.Id, 2, agentv1.CommandState_COMMAND_STATE_SUCCEEDED, nil, map[string]any{"files_restored": 3}, "")
	if stored := q.stored(done.Id); stored.GetString("status") != schema.CommandSucceeded || stored.GetString("error") != "" {
		t.Errorf("a restore that succeeded first: %v", stored.FieldsData())
	}

	// as when the agent reconnects with it among its finished commands
	failed := q.running(restoreCommand("third"))
	if err := q.manager.Cancel(failed.Id, "Cancelled by admin@example.com"); !errors.Is(err, commands.ErrCancelRequested) {
		t.Fatal(err)
	}
	q.next()
	q.manager.Disconnect(q.stream)
	q.stream = q.manager.Connect(q.host.Id, []string{"backup.restore", "stack.deploy"}, []*agentv1.FinishedCommand{{
		CommandId: failed.Id, State: agentv1.CommandState_COMMAND_STATE_FAILED, Error: "restic was stopped",
	}})
	if stored := q.stored(failed.Id); stored.GetString("status") != schema.CommandCancelled || stored.GetString("error") != "Cancelled by admin@example.com" {
		t.Errorf("settled at the reconnect: %v", stored.FieldsData())
	}
}

// TestCancelRequestedAnswersCancelOnUpdates (#30): while a restore stops,
// every update of its agent is answered with cancel, which sends again a
// cancel the agent missed; its progress says it stops; and asking again
// changes nothing but telling the agent again.
func TestCancelRequestedAnswersCancelOnUpdates(t *testing.T) {
	q := newQueue(t)
	restore := q.running(restoreCommand("notes"))
	if err := q.manager.Cancel(restore.Id, "Cancelled by operator@example.com"); !errors.Is(err, commands.ErrCancelRequested) {
		t.Fatal(err)
	}
	q.next()
	if cancel := q.update(restore.Id, 2, agentv1.CommandState_COMMAND_STATE_RUNNING,
		&agentv1.CommandProgress{Step: "start", Message: "Starting the containers again", Percent: 97}, nil, ""); !cancel {
		t.Error("an update was not told to stop")
	}
	// one that came twice is told too
	if cancel := q.update(restore.Id, 2, agentv1.CommandState_COMMAND_STATE_RUNNING, nil, nil, ""); !cancel {
		t.Error("a repeated update was not told to stop")
	}
	stored := q.stored(restore.Id)
	progress := map[string]any{}
	_ = stored.UnmarshalJSONField("progress", &progress)
	if progress["step"] != "cancelling" || stored.GetString("status") != schema.CommandRunning {
		t.Errorf("while it stops: %v", stored.FieldsData())
	}

	requested := stored.GetDateTime("cancel_requested_at")
	expires := stored.GetDateTime("expires_at")
	if err := q.manager.Cancel(restore.Id, "Cancelled by someone else"); !errors.Is(err, commands.ErrCancelRequested) {
		t.Fatal(err)
	}
	if cancel := q.next().GetCancel(); cancel.GetCommandId() != restore.Id || cancel.GetReason() != "Cancelled by operator@example.com" {
		t.Errorf("told again: %v", cancel)
	}
	again := q.stored(restore.Id)
	if !again.GetDateTime("cancel_requested_at").Equal(requested) || !again.GetDateTime("expires_at").Equal(expires) ||
		again.GetString("cancel_reason") != "Cancelled by operator@example.com" {
		t.Errorf("asked again: %v", again.FieldsData())
	}

	// a dispatched restore that its agent never said it started is held as well
	dispatched := q.create(restoreCommand("other"))
	q.next()
	if err := q.manager.Cancel(dispatched.Id, "Cancelled by operator@example.com"); !errors.Is(err, commands.ErrCancelRequested) {
		t.Fatal(err)
	}
	q.next()
	if cancel := q.update(dispatched.Id, 1, agentv1.CommandState_COMMAND_STATE_RUNNING, nil, nil, ""); !cancel {
		t.Error("its start was not told to stop")
	}
	if stored := q.stored(dispatched.Id); stored.GetString("status") != schema.CommandRunning || stored.GetDateTime("started_at").IsZero() {
		t.Errorf("its start: %v", stored.FieldsData())
	}

	// a download is settled by its agent too
	dump := q.running(&agentv1.Command{Payload: &agentv1.Command_BackupDump{BackupDump: &agentv1.BackupDump{
		Repository: &agentv1.BackupRepository{TargetId: "target"}, SnapshotId: "x", Path: "/volumes/v",
	}}})
	if err := q.manager.Cancel(dump.Id, "The browser stopped the download."); !errors.Is(err, commands.ErrCancelRequested) {
		t.Fatal(err)
	}
}

// TestCancelRequestedTimesOutAfterGrace (#30): an agent that does not report
// a restore it was told to stop within its grace leaves it timed out, saying
// so, and is told to stop once more.
func TestCancelRequestedTimesOutAfterGrace(t *testing.T) {
	q := newQueue(t)
	restore := q.running(restoreCommand("notes"))
	if err := q.manager.Cancel(restore.Id, "Cancelled by operator@example.com"); !errors.Is(err, commands.ErrCancelRequested) {
		t.Fatal(err)
	}
	q.next()
	stored := q.stored(restore.Id)
	stored.Set("expires_at", time.Now().UTC().Add(-time.Second))
	if err := q.app.Save(stored); err != nil {
		t.Fatal(err)
	}
	q.manager.Reap()
	if stored := q.stored(restore.Id); stored.GetString("status") != schema.CommandTimedOut || stored.GetString("error") != "The agent did not stop the command in time." {
		t.Errorf("reaped: %v", stored.FieldsData())
	}
	if cancel := q.next().GetCancel(); cancel.GetCommandId() != restore.Id {
		t.Errorf("told %v", cancel)
	}
}

// TestQueuedAgentSettledCommandCancelsAtOnce (#30): a restore that has not
// left the queue has nothing to settle: it is cancelled at once and never
// sent.
func TestQueuedAgentSettledCommandCancelsAtOnce(t *testing.T) {
	q := newQueue(t)
	q.manager.Disconnect(q.stream)
	restore := q.create(restoreCommand("notes"))
	if err := q.manager.Cancel(restore.Id, "Cancelled by operator@example.com"); err != nil {
		t.Fatal(err)
	}
	stored := q.stored(restore.Id)
	if stored.GetString("status") != schema.CommandCancelled || stored.GetString("error") != "Cancelled by operator@example.com" ||
		!stored.GetDateTime("cancel_requested_at").IsZero() || q.finished[restore.Id] != 1 {
		t.Errorf("cancelled in the queue: %v", stored.FieldsData())
	}
	if err := q.manager.Cancel(restore.Id, "again"); !errors.Is(err, commands.ErrNotCancellable) {
		t.Errorf("cancelled again: %v", err)
	}
}

// TestOtherCommandsKeepTheirCancel (#30): the cancel of every command that
// its agent does not settle is as it was: a running one is cancelled at
// once and its agent told to stop; its agent's later report changes nothing
// and is told to stop; a command that may not be interrupted is not; and one
// whose agent goes silent times out as before.
func TestOtherCommandsKeepTheirCancel(t *testing.T) {
	q := newQueue(t)
	pull := q.running(pullCommand())
	if err := q.manager.Cancel(pull.Id, "Cancelled by operator@example.com"); err != nil {
		t.Fatal(err)
	}
	stored := q.stored(pull.Id)
	if stored.GetString("status") != schema.CommandCancelled || stored.GetString("error") != "Cancelled by operator@example.com" ||
		!stored.GetDateTime("cancel_requested_at").IsZero() || stored.GetString("cancel_reason") != "" || q.finished[pull.Id] != 1 {
		t.Errorf("a cancelled pull: %v", stored.FieldsData())
	}
	if cancel := q.next().GetCancel(); cancel.GetCommandId() != pull.Id {
		t.Errorf("told %v", cancel)
	}
	if cancel := q.update(pull.Id, 2, agentv1.CommandState_COMMAND_STATE_SUCCEEDED, nil, map[string]any{"digest": "sha256:x"}, ""); !cancel {
		t.Error("its agent's late report was not told to stop")
	}
	if stored := q.stored(pull.Id); stored.GetString("status") != schema.CommandCancelled || (stored.GetString("result") != "" && stored.GetString("result") != "null") {
		t.Errorf("after its agent's late report: %v", stored.FieldsData())
	}

	// a backup, which holds its target, is cancelled at once too
	run := q.running(&agentv1.Command{Payload: &agentv1.Command_BackupRun{BackupRun: &agentv1.BackupRun{
		RunId: "run", Repository: &agentv1.BackupRepository{TargetId: "target"},
	}}})
	if err := q.manager.Cancel(run.Id, "Cancelled by operator@example.com"); err != nil {
		t.Fatal(err)
	}
	if stored := q.stored(run.Id); stored.GetString("status") != schema.CommandCancelled {
		t.Errorf("a cancelled backup: %v", stored.FieldsData())
	}
	q.next()

	move := q.running(&agentv1.Command{Payload: &agentv1.Command_DbServerMove{DbServerMove: &agentv1.DbServerMove{Stack: "shop"}}})
	if err := q.manager.Cancel(move.Id, "Cancelled by operator@example.com"); !errors.Is(err, commands.ErrUninterruptible) {
		t.Errorf("a move: %v", err)
	}

	// an update of a running command is not told to stop, and a silent
	// agent's command times out with the message of old
	other := q.running(pullCommand())
	if cancel := q.update(other.Id, 2, agentv1.CommandState_COMMAND_STATE_RUNNING, &agentv1.CommandProgress{Step: "pull", Percent: 40}, nil, ""); cancel {
		t.Error("a running pull was told to stop")
	}
	stored = q.stored(other.Id)
	stored.Set("expires_at", time.Now().UTC().Add(-time.Second))
	if err := q.app.Save(stored); err != nil {
		t.Fatal(err)
	}
	q.manager.Reap()
	if stored := q.stored(other.Id); stored.GetString("status") != schema.CommandTimedOut || stored.GetString("error") != "The agent did not finish the command in time." {
		t.Errorf("a pull that timed out: %v", stored.FieldsData())
	}
}

// TestProgressKeepsDoneAndTotal (#30): a step the agent counts keeps how
// much of it is done, of how much, in what; one it does not count is stored
// as it always was.
func TestProgressKeepsDoneAndTotal(t *testing.T) {
	q := newQueue(t)
	restore := q.running(restoreCommand("notes"))
	q.update(restore.Id, 2, agentv1.CommandState_COMMAND_STATE_RUNNING,
		&agentv1.CommandProgress{Step: "restore", Message: "Restoring", Percent: 40, Done: 3 << 30, Total: 8 << 30, Unit: "bytes"}, nil, "")
	progress := map[string]any{}
	_ = q.stored(restore.Id).UnmarshalJSONField("progress", &progress)
	if progress["step"] != "restore" || progress["percent"] != float64(40) || progress["done"] != float64(3<<30) ||
		progress["total"] != float64(8<<30) || progress["unit"] != "bytes" {
		t.Errorf("counted: %v", progress)
	}

	pull := q.running(pullCommand())
	q.update(pull.Id, 2, agentv1.CommandState_COMMAND_STATE_RUNNING, &agentv1.CommandProgress{Step: "pull", Message: "Pulling", Percent: 10}, nil, "")
	raw, _ := json.Marshal(q.stored(pull.Id).Get("progress"))
	if string(raw) != `{"message":"Pulling","percent":10,"step":"pull"}` {
		t.Errorf("not counted: %s", raw)
	}
}

// TestACommandMadeInATransactionGoesOutWhenItCommits (#30): a command made
// with what it is checked against, such as a restore's record that names it,
// is not delivered before both are saved, and not at all if the transaction
// fails.
func TestACommandMadeInATransactionGoesOutWhenItCommits(t *testing.T) {
	q := newQueue(t)
	var made *core.Record
	err := q.app.RunInTransaction(func(tx core.App) error {
		var err error
		made, err = q.manager.CreateIn(context.Background(), tx, commands.Options{Host: q.host.Id, Command: pullCommand()})
		if err != nil {
			return err
		}
		q.manager.Kick(q.host.Id)
		if !q.quiet() {
			t.Error("delivered before the transaction committed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := q.next().GetCommand().GetId(); got != made.Id {
		t.Errorf("delivered %q, not %s", got, made.Id)
	}

	var rolledBack string
	err = q.app.RunInTransaction(func(tx core.App) error {
		record, err := q.manager.CreateIn(context.Background(), tx, commands.Options{Host: q.host.Id, Command: pullCommand()})
		if err != nil {
			return err
		}
		rolledBack = record.Id
		return errors.New("something else failed")
	})
	if err == nil || !q.quiet() {
		t.Errorf("a transaction that failed: %v", err)
	}
	if _, err := q.app.FindRecordById(schema.Commands, rolledBack); err == nil {
		t.Error("the command of a transaction that failed is in the queue")
	}
}

// TestTheCancelRouteSaysARestoreIsStopping (#30): stopping a restore through
// the cancel route answers 202 with stopping, as its agent still has to say
// how it ended; any other command is cancelled with 204, as before.
func TestTheCancelRouteSaysARestoreIsStopping(t *testing.T) {
	q := newQueue(t)
	q.app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		group := e.Router.Group("/api/pc")
		group.Bind(apis.RequireAuth())
		q.manager.BindRoutes(group)
		return e.Next()
	})
	handler := testutil.Handler(t, q.app)
	_, operator := testutil.NewUser(t, q.app, "operator@example.com", schema.RoleOperator)

	restore := q.running(restoreCommand("notes"))
	status, body := testutil.Do(t, handler, testutil.Request{Method: "POST", URL: "/api/pc/commands/" + restore.Id + "/cancel", Token: operator})
	if status != 202 || body != `{"stopping":true}` {
		t.Errorf("a restore: %d %s", status, body)
	}
	if stored := q.stored(restore.Id); stored.GetString("cancel_reason") != "Cancelled by operator@example.com" {
		t.Errorf("the restore: %v", stored.FieldsData())
	}
	if cancel := q.next().GetCancel(); cancel.GetCommandId() != restore.Id {
		t.Errorf("told %v", cancel)
	}
	pull := q.running(pullCommand())
	status, body = testutil.Do(t, handler, testutil.Request{Method: "POST", URL: "/api/pc/commands/" + pull.Id + "/cancel", Token: operator})
	if status != 204 {
		t.Errorf("a pull: %d %s", status, body)
	}
}
