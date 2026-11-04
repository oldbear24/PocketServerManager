package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/types"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

const (
	defaultQueueTTL = time.Hour
	reapInterval    = 5 * time.Second
	streamBuffer    = 256
	dispatchBatch   = 100
	maxOutput       = 16 << 20
)

// ErrNotCancellable is returned when a finished command is cancelled.
var ErrNotCancellable = errors.New("the command has already finished")

// ErrUninterruptible is returned when a running command that must finish by itself is cancelled.
var ErrUninterruptible = fmt.Errorf("%w: it is under way and cannot be interrupted", ErrNotCancellable)

// Options describes a new command.
type Options struct {
	// ID is the id of the new record; empty generates one. Knowing it up front lets
	// a change in the same request link to the command it causes.
	ID          string
	Host        string
	Type        string
	Command     *agentv1.Command
	Label       string
	RequestedBy string
	Requester   string
	Parent      string
	Correlation string
	Timeout     time.Duration
}

// Enricher completes a command right before delivery with values that must not
// be stored in the record, such as secrets.
type Enricher func(record *core.Record, command *agentv1.Command) error

// Manager runs the queue: delivery to connected agents, progress and results,
// replay after a reconnect, and timeouts.
type Manager struct {
	app core.App

	// Log receives the queue's errors.
	Log *slog.Logger

	// OnReplay is told about every command that was sent again after a reconnect.
	OnReplay func()

	mu      sync.Mutex
	streams map[string]*Stream

	enrichers map[string]Enricher
	finishers map[string][]func(*core.Record)

	outputMu sync.Mutex
	outputs  map[string]*commandOutput

	// ReapInterval overrides how often timeouts are checked, in tests.
	ReapInterval time.Duration
}

type commandOutput struct {
	buffer    bytes.Buffer
	truncated bool
	watchers  map[chan []byte]struct{}
}

// NewManager creates the queue.
func NewManager(app core.App) *Manager {
	return &Manager{
		app:          app,
		Log:          slog.Default().With("component", "commands"),
		streams:      make(map[string]*Stream),
		enrichers:    make(map[string]Enricher),
		finishers:    make(map[string][]func(*core.Record)),
		outputs:      make(map[string]*commandOutput),
		ReapInterval: reapInterval,
	}
}

// App returns the app of the queue.
func (m *Manager) App() core.App {
	return m.app
}

// RegisterEnricher sets the enricher of a command type.
func (m *Manager) RegisterEnricher(commandType string, enricher Enricher) {
	m.enrichers[commandType] = enricher
}

// OnFinish registers a callback for when a command of a type reaches a final state.
func (m *Manager) OnFinish(commandType string, callback func(*core.Record)) {
	m.finishers[commandType] = append(m.finishers[commandType], callback)
}

// Register binds the queue to the app's hooks.
func (m *Manager) Register() {
	// every new record starts as a clean queued command, however it was created
	m.app.OnRecordCreate(schema.Commands).BindFunc(func(e *core.RecordEvent) error {
		if err := m.prepare(e.App, e.Record); err != nil {
			return err
		}
		return e.Next()
	})

	// requests cannot set what only the queue may set, nor run a type above their role
	m.app.OnRecordCreateRequest(schema.Commands).BindFunc(func(e *core.RecordRequestEvent) error {
		commandType := e.Record.GetString("type")

		spec, ok := Specs[commandType]
		if !ok {
			return e.BadRequestError(fmt.Sprintf("Unknown command type %q.", commandType), nil)
		}

		if !e.HasSuperuserAuth() {
			if spec.Role == "" {
				return e.ForbiddenError(fmt.Sprintf("%s cannot be requested directly.", commandType), nil)
			}
			if e.Auth == nil || !roleAtLeast(e.Auth.GetString("role"), spec.Role) {
				return e.ForbiddenError(fmt.Sprintf("%s needs the %s role.", commandType, spec.Role), nil)
			}
		}

		if e.Record.GetString("host") == "" {
			return e.BadRequestError("The command needs a host.", nil)
		}

		for _, field := range []string{
			"status", "attempts", "last_sequence", "dispatched_at", "started_at", "finished_at",
			"expires_at", "timeout_s", "progress", "result", "error", "requested_label", "host_name",
			"resource", "correlation_id", "parent",
		} {
			e.Record.Set(field, nil)
		}

		if e.Auth != nil {
			if e.HasSuperuserAuth() {
				e.Record.Set("requested_by", "")
				e.Record.Set("requested_label", "superuser "+e.Auth.Email())
			} else {
				e.Record.Set("requested_by", e.Auth.Id)
				e.Record.Set("requested_label", e.Auth.Email())
			}
		}

		return e.Next()
	})

	// the state machine holds for every save, superusers included
	m.app.OnRecordUpdateExecute(schema.Commands).BindFunc(func(e *core.RecordEvent) error {
		stored, err := e.App.FindRecordById(e.Record.Collection(), e.Record.Id)
		if err != nil {
			return e.Next()
		}
		from, to := stored.GetString("status"), e.Record.GetString("status")
		if !transitionAllowed(from, to) {
			return fmt.Errorf("a command cannot go from %s to %s", from, to)
		}
		return e.Next()
	})

	m.app.OnRecordAfterCreateSuccess(schema.Commands).BindFunc(func(e *core.RecordEvent) error {
		m.Kick(e.Record.GetString("host"))
		return e.Next()
	})
}

// transitions lists the allowed state changes of a command.
var transitions = map[string][]string{
	schema.CommandQueued:     {schema.CommandDispatched, schema.CommandCancelled, schema.CommandTimedOut, schema.CommandFailed},
	schema.CommandDispatched: {schema.CommandRunning, schema.CommandSucceeded, schema.CommandFailed, schema.CommandCancelled, schema.CommandTimedOut},
	schema.CommandRunning:    {schema.CommandSucceeded, schema.CommandFailed, schema.CommandCancelled, schema.CommandTimedOut},
}

func transitionAllowed(from, to string) bool {
	return from == to && !isFinal(from) || slices.Contains(transitions[from], to)
}

func isFinal(status string) bool {
	switch status {
	case schema.CommandSucceeded, schema.CommandFailed, schema.CommandTimedOut, schema.CommandCancelled:
		return true
	}
	return false
}

func roleAtLeast(role, minimum string) bool {
	rank := map[string]int{schema.RoleViewer: 1, schema.RoleOperator: 2, schema.RoleAdmin: 3}
	return rank[role] >= rank[minimum]
}

// prepare fills in the fields of a new command.
func (m *Manager) prepare(app core.App, record *core.Record) error {
	commandType := record.GetString("type")

	spec, ok := Specs[commandType]
	if !ok {
		return fmt.Errorf("unknown command type %q", commandType)
	}

	raw, err := json.Marshal(record.Get("payload"))
	if err != nil {
		return err
	}

	command, err := Build(commandType, raw)
	if err != nil {
		return err
	}

	timeout := spec.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	if record.GetInt("timeout_s") == 0 {
		record.Set("timeout_s", int(timeout.Seconds()))
	}

	if spec.Resource != nil {
		record.Set("resource", spec.Resource(command))
	}

	if record.Id == "" {
		record.Id = core.GenerateDefaultRandomId()
	}
	if record.GetString("correlation_id") == "" {
		record.Set("correlation_id", record.Id)
	}

	record.Set("status", schema.CommandQueued)

	if hostID := record.GetString("host"); hostID != "" {
		host, err := app.FindRecordById(schema.Hosts, hostID)
		if err != nil {
			return fmt.Errorf("unknown host: %w", err)
		}
		record.Set("host_name", host.GetString("name"))
	}

	if record.GetDateTime("expires_at").IsZero() {
		expires, err := types.ParseDateTime(time.Now().UTC().Add(queueTTL(app)))
		if err != nil {
			return err
		}
		record.Set("expires_at", expires)
	}

	return nil
}

func queueTTL(app core.App) time.Duration {
	records, err := app.FindRecordsByFilter(schema.Settings, "", "-created", 1, 0)
	if err == nil && len(records) > 0 && records[0].GetInt("command_queue_ttl_s") > 0 {
		return time.Duration(records[0].GetInt("command_queue_ttl_s")) * time.Second
	}
	return defaultQueueTTL
}

// Create adds a command to the queue. It is how the panel's components create commands.
func (m *Manager) Create(ctx context.Context, opts Options) (*core.Record, error) {
	if opts.Command == nil {
		return nil, errors.New("the command has no payload")
	}

	commandType := opts.Type
	if commandType == "" {
		commandType = TypeOf(opts.Command)
	}

	payload, err := PayloadJSON(opts.Command)
	if err != nil {
		return nil, err
	}

	collection, err := m.app.FindCachedCollectionByNameOrId(schema.Commands)
	if err != nil {
		return nil, err
	}

	record := core.NewRecord(collection)
	record.Id = opts.ID
	record.Set("host", opts.Host)
	record.Set("type", commandType)
	record.Set("label", opts.Label)
	record.Set("payload", payload)
	record.Set("requested_by", opts.RequestedBy)
	record.Set("requested_label", opts.Requester)
	record.Set("parent", opts.Parent)
	record.Set("correlation_id", opts.Correlation)
	if opts.Timeout > 0 {
		record.Set("timeout_s", int(opts.Timeout.Seconds()))
	}

	if err := m.app.SaveWithContext(ctx, record); err != nil {
		return nil, err
	}

	return record, nil
}

// Stream is the delivery channel to one connected agent.
type Stream struct {
	hostID       string
	capabilities map[string]bool
	out          chan *agentv1.SubscribeCommandsResponse
	closed       chan struct{}
	closeOnce    sync.Once
	dispatchMu   sync.Mutex
}

// Out returns the messages to send to the agent.
func (s *Stream) Out() <-chan *agentv1.SubscribeCommandsResponse {
	return s.out
}

// Closed is closed when the stream was replaced or overflowed.
func (s *Stream) Closed() <-chan struct{} {
	return s.closed
}

func (s *Stream) close() {
	s.closeOnce.Do(func() { close(s.closed) })
}

func (s *Stream) send(message *agentv1.SubscribeCommandsResponse) bool {
	select {
	case <-s.closed:
		return false
	case s.out <- message:
		return true
	default:
		// the agent is too slow: drop the connection, everything is replayed after a reconnect
		s.close()
		return false
	}
}

// Connect registers the command stream of a host, replacing an older one. It
// settles what the agent says it has finished, and replays what is still open.
func (m *Manager) Connect(hostID string, capabilities []string, finished []*agentv1.FinishedCommand) *Stream {
	stream := &Stream{
		hostID:       hostID,
		capabilities: make(map[string]bool, len(capabilities)),
		out:          make(chan *agentv1.SubscribeCommandsResponse, streamBuffer),
		closed:       make(chan struct{}),
	}
	for _, capability := range capabilities {
		stream.capabilities[capability] = true
	}

	m.mu.Lock()
	if old := m.streams[hostID]; old != nil {
		old.close()
	}
	m.streams[hostID] = stream
	m.mu.Unlock()

	for _, f := range finished {
		m.settle(hostID, f)
	}

	m.replay(stream)
	m.dispatch(stream)

	return stream
}

// Disconnect removes a stream, unless a newer one has already replaced it.
func (m *Manager) Disconnect(stream *Stream) {
	m.mu.Lock()
	if m.streams[stream.hostID] == stream {
		delete(m.streams, stream.hostID)
	}
	m.mu.Unlock()
	stream.close()
}

// Connected tells whether a host has an open command stream.
func (m *Manager) Connected(hostID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streams[hostID] != nil
}

func (m *Manager) stream(hostID string) *Stream {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streams[hostID]
}

// Kick delivers queued commands of a host, if it is connected.
func (m *Manager) Kick(hostID string) {
	if hostID == "" {
		return
	}
	if stream := m.stream(hostID); stream != nil {
		go m.dispatch(stream)
	}
}

// dispatch sends the queued commands of a stream's host that may run now.
// Commands with the same resource run in the order they were created.
func (m *Manager) dispatch(stream *Stream) {
	stream.dispatchMu.Lock()
	defer stream.dispatchMu.Unlock()

	busy, err := m.busyResources(stream.hostID)
	if err != nil {
		m.Log.Error("failed to read the running commands", "host", stream.hostID, "error", err)
		return
	}

	records, err := m.app.FindRecordsByFilter(schema.Commands, "host={:host} && status={:status}", "created,id", dispatchBatch, 0,
		dbx.Params{"host": stream.hostID, "status": schema.CommandQueued})
	if err != nil {
		m.Log.Error("failed to read the queue", "host", stream.hostID, "error", err)
		return
	}

	for _, record := range records {
		resource := record.GetString("resource")
		if resource != "" && busy[resource] {
			continue
		}

		if !stream.capabilities[record.GetString("type")] {
			m.finish(record, schema.CommandFailed, "This agent does not support "+record.GetString("type")+". Update the agent.", nil)
			continue
		}

		command, err := m.build(record)
		if err != nil {
			m.finish(record, schema.CommandFailed, err.Error(), nil)
			continue
		}

		timeout := time.Duration(record.GetInt("timeout_s")) * time.Second
		deadline := time.Now().UTC().Add(timeout)

		expires, err := types.ParseDateTime(deadline)
		if err != nil {
			m.Log.Error("invalid deadline", "error", err)
			continue
		}

		record.Set("status", schema.CommandDispatched)
		record.Set("dispatched_at", types.NowDateTime())
		record.Set("attempts", record.GetInt("attempts")+1)
		record.Set("expires_at", expires)
		if err := m.app.Save(record); err != nil {
			m.Log.Error("failed to mark a command as dispatched", "command", record.Id, "error", err)
			continue
		}

		command.Attempt = int32(record.GetInt("attempts")) //nolint:gosec // a small counter
		command.Deadline = timestamppb.New(deadline)

		if !stream.send(&agentv1.SubscribeCommandsResponse{Event: &agentv1.SubscribeCommandsResponse_Command{Command: command}}) {
			// it stays dispatched and is replayed after the reconnect
			return
		}

		if resource != "" {
			busy[resource] = true
		}
	}
}

func (m *Manager) busyResources(hostID string) (map[string]bool, error) {
	records, err := m.app.FindRecordsByFilter(schema.Commands,
		"host={:host} && resource!='' && (status={:dispatched} || status={:running})", "", 0, 0,
		dbx.Params{"host": hostID, "dispatched": schema.CommandDispatched, "running": schema.CommandRunning})
	if err != nil {
		return nil, err
	}

	busy := make(map[string]bool, len(records))
	for _, record := range records {
		busy[record.GetString("resource")] = true
	}

	return busy, nil
}

// replay re-sends what was dispatched but never finished. The agent answers ids
// it has already seen without running them again.
func (m *Manager) replay(stream *Stream) {
	records, err := m.app.FindRecordsByFilter(schema.Commands,
		"host={:host} && (status={:dispatched} || status={:running})", "created,id", 0, 0,
		dbx.Params{"host": stream.hostID, "dispatched": schema.CommandDispatched, "running": schema.CommandRunning})
	if err != nil {
		m.Log.Error("failed to read the commands to replay", "host", stream.hostID, "error", err)
		return
	}

	for _, record := range records {
		command, err := m.build(record)
		if err != nil {
			m.finish(record, schema.CommandFailed, err.Error(), nil)
			continue
		}

		record.Set("attempts", record.GetInt("attempts")+1)
		if err := m.app.Save(record); err != nil {
			m.Log.Error("failed to count a replay", "command", record.Id, "error", err)
		}

		command.Attempt = int32(record.GetInt("attempts")) //nolint:gosec // a small counter
		command.Deadline = timestamppb.New(record.GetDateTime("expires_at").Time())

		stream.send(&agentv1.SubscribeCommandsResponse{Event: &agentv1.SubscribeCommandsResponse_Command{Command: command}})

		if m.OnReplay != nil {
			m.OnReplay()
		}
	}
}

// build creates the message for a record, including what the enricher adds.
func (m *Manager) build(record *core.Record) (*agentv1.Command, error) {
	raw, err := json.Marshal(record.Get("payload"))
	if err != nil {
		return nil, err
	}

	commandType := record.GetString("type")

	command, err := Build(commandType, raw)
	if err != nil {
		return nil, err
	}

	command.Id = record.Id
	command.ParentId = record.GetString("parent")
	command.CorrelationId = record.GetString("correlation_id")

	if enrich := m.enrichers[commandType]; enrich != nil {
		if err := enrich(record, command); err != nil {
			return nil, fmt.Errorf("failed to prepare the command: %w", err)
		}
	}

	return command, nil
}

// settle applies a final state the agent reported when it reconnected.
func (m *Manager) settle(hostID string, finished *agentv1.FinishedCommand) {
	record, err := m.app.FindRecordById(schema.Commands, finished.GetCommandId())
	if err != nil || record.GetString("host") != hostID || isFinal(record.GetString("status")) {
		return
	}

	status := statusOf(finished.GetState())
	if status == "" || !isFinal(status) {
		return
	}

	m.finish(record, status, finished.GetError(), nil)
}

func statusOf(state agentv1.CommandState) string {
	switch state {
	case agentv1.CommandState_COMMAND_STATE_RUNNING:
		return schema.CommandRunning
	case agentv1.CommandState_COMMAND_STATE_SUCCEEDED:
		return schema.CommandSucceeded
	case agentv1.CommandState_COMMAND_STATE_FAILED:
		return schema.CommandFailed
	case agentv1.CommandState_COMMAND_STATE_CANCELLED:
		return schema.CommandCancelled
	}
	return ""
}

// ErrWrongHost is returned when an agent reports a command of another host.
var ErrWrongHost = errors.New("this command belongs to another host")

// Update applies a progress update or a final result from an agent. It returns
// whether the agent should stop working on the command.
func (m *Manager) Update(hostID string, update *agentv1.ReportCommandUpdateRequest) (cancel bool, err error) {
	record, err := m.app.FindRecordById(schema.Commands, update.GetCommandId())
	if err != nil {
		// an unknown command has been deleted: there is nothing left to work for
		return true, nil //nolint:nilerr // answering "stop" is the handling
	}
	if record.GetString("host") != hostID {
		return false, ErrWrongHost
	}

	status := record.GetString("status")
	if isFinal(status) {
		return status != schema.CommandSucceeded && status != schema.CommandFailed, nil
	}

	// idempotent by id + sequence; a replay's final answer always counts
	if update.GetSequence() <= uint64(record.GetInt("last_sequence")) && !update.GetDuplicate() { //nolint:gosec // never negative
		return false, nil
	}

	if len(update.GetOutput()) > 0 {
		m.appendOutput(record.Id, update.GetOutput())
	}

	newStatus := statusOf(update.GetState())
	if newStatus == "" {
		return false, fmt.Errorf("invalid state %v", update.GetState())
	}

	record.Set("last_sequence", update.GetSequence())

	if isFinal(newStatus) {
		var result map[string]any
		if update.GetResult() != nil {
			result = update.GetResult().AsMap()
		}
		if update.GetProgress() != nil {
			record.Set("progress", progressOf(update.GetProgress()))
		}
		m.finish(record, newStatus, update.GetError(), result)
		return false, nil
	}

	if status == schema.CommandDispatched {
		record.Set("started_at", types.NowDateTime())
	}
	record.Set("status", newStatus)
	if update.GetProgress() != nil {
		record.Set("progress", progressOf(update.GetProgress()))
	}

	return false, m.app.Save(record)
}

func progressOf(progress *agentv1.CommandProgress) map[string]any {
	return map[string]any{
		"step":    progress.GetStep(),
		"message": progress.GetMessage(),
		"percent": progress.GetPercent(),
	}
}

// finish moves a command to a final state and stores its output.
func (m *Manager) finish(record *core.Record, status, message string, result map[string]any) {
	record.Set("status", status)
	record.Set("finished_at", types.NowDateTime())
	if message != "" {
		record.Set("error", message)
	}
	if result != nil {
		record.Set("result", result)
	}

	// the watchers end after the save, so that whoever follows the output finds the
	// final state as soon as it ends
	defer m.dropOutput(record.Id)

	output := m.currentOutput(record.Id)
	if len(output) > 0 {
		file, err := filesystem.NewFileFromBytes(output, record.Id+".log")
		if err == nil {
			record.Set("output", file)
		}
	}

	if err := m.app.Save(record); err != nil {
		m.Log.Error("failed to finish a command", "command", record.Id, "status", status, "error", err)
		return
	}

	for _, callback := range m.finishers[record.GetString("type")] {
		callback(record)
	}

	m.finishParent(record)

	// a finished command may unblock the next one of the same resource
	m.Kick(record.GetString("host"))
}

// finishParent settles a parent command once all of its children have finished.
func (m *Manager) finishParent(record *core.Record) {
	parentID := record.GetString("parent")
	if parentID == "" {
		return
	}

	parent, err := m.app.FindRecordById(schema.Commands, parentID)
	if err != nil || isFinal(parent.GetString("status")) {
		return
	}

	children, err := m.app.FindRecordsByFilter(schema.Commands, "parent={:parent}", "", 0, 0, dbx.Params{"parent": parentID})
	if err != nil {
		m.Log.Error("failed to read child commands", "command", parentID, "error", err)
		return
	}

	status := schema.CommandSucceeded
	message := ""
	for _, child := range children {
		childStatus := child.GetString("status")
		if !isFinal(childStatus) {
			return
		}
		if childStatus != schema.CommandSucceeded && status == schema.CommandSucceeded {
			status = schema.CommandFailed
			message = child.GetString("type") + ": " + child.GetString("error")
		}
	}

	m.finish(parent, status, message, nil)
}

// Cancel stops a command. A queued one never runs; a running one is told to stop.
func (m *Manager) Cancel(id, reason string) error {
	record, err := m.app.FindRecordById(schema.Commands, id)
	if err != nil {
		return err
	}

	status := record.GetString("status")
	if isFinal(status) {
		return ErrNotCancellable
	}
	if spec, ok := Specs[record.GetString("type")]; ok && spec.Uninterruptible && status != schema.CommandQueued {
		return ErrUninterruptible
	}

	m.finish(record, schema.CommandCancelled, reason, nil)

	if status != schema.CommandQueued {
		m.tellAgentToStop(record, reason)
	}

	return nil
}

func (m *Manager) tellAgentToStop(record *core.Record, reason string) {
	if stream := m.stream(record.GetString("host")); stream != nil {
		stream.send(&agentv1.SubscribeCommandsResponse{Event: &agentv1.SubscribeCommandsResponse_Cancel{
			Cancel: &agentv1.CommandCancel{CommandId: record.Id, Reason: reason},
		}})
	}
}

// Run checks for timeouts until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(m.ReapInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Reap()
		}
	}
}

// Reap ends commands that have run out of time, so that none stays open forever:
// queued ones whose host stayed offline and dispatched or running ones whose agent
// went silent.
func (m *Manager) Reap() {
	now, err := types.ParseDateTime(time.Now().UTC())
	if err != nil {
		return
	}

	records, err := m.app.FindRecordsByFilter(schema.Commands,
		"(status={:queued} || status={:dispatched} || status={:running}) && expires_at!='' && expires_at<{:now}", "", 200, 0,
		dbx.Params{"queued": schema.CommandQueued, "dispatched": schema.CommandDispatched, "running": schema.CommandRunning, "now": now.String()})
	if err != nil {
		m.Log.Error("failed to look for expired commands", "error", err)
		return
	}

	for _, record := range records {
		status := record.GetString("status")

		message := "The agent did not finish the command in time."
		if status == schema.CommandQueued {
			message = "The server stayed offline, so the command was never delivered."
		}

		m.finish(record, schema.CommandTimedOut, message, nil)

		if status != schema.CommandQueued {
			m.tellAgentToStop(record, message)
		}

		events.Log(m.app, record.GetString("host"), "command.timed_out", schema.SeverityWarning,
			record.GetString("type")+": "+message, map[string]any{"command": record.Id})
	}
}

func (m *Manager) appendOutput(id string, chunk []byte) {
	m.outputMu.Lock()
	defer m.outputMu.Unlock()

	output := m.outputs[id]
	if output == nil {
		output = &commandOutput{watchers: make(map[chan []byte]struct{})}
		m.outputs[id] = output
	}

	if output.buffer.Len()+len(chunk) <= maxOutput {
		output.buffer.Write(chunk)
	} else if !output.truncated {
		output.truncated = true
		output.buffer.WriteString("\n[the output was too long and has been truncated]\n")
	}

	for watcher := range output.watchers {
		select {
		case watcher <- chunk:
		default: // a slow viewer loses chunks; the file has everything
		}
	}
}

func (m *Manager) currentOutput(id string) []byte {
	m.outputMu.Lock()
	defer m.outputMu.Unlock()

	output := m.outputs[id]
	if output == nil {
		return nil
	}

	return bytes.Clone(output.buffer.Bytes())
}

func (m *Manager) dropOutput(id string) {
	m.outputMu.Lock()
	defer m.outputMu.Unlock()

	output := m.outputs[id]
	if output == nil {
		return
	}

	for watcher := range output.watchers {
		close(watcher)
	}
	delete(m.outputs, id)
}

// WatchOutput returns what a running command has written so far and a channel of
// new chunks, closed when the command finishes. Call stop when done watching. A
// command that has already finished never closes the channel: check its state
// after this call, which leaves no gap.
func (m *Manager) WatchOutput(id string) (past []byte, chunks <-chan []byte, stop func()) {
	m.outputMu.Lock()
	defer m.outputMu.Unlock()

	output := m.outputs[id]
	if output == nil {
		output = &commandOutput{watchers: make(map[chan []byte]struct{})}
		m.outputs[id] = output
	}

	watcher := make(chan []byte, 256)
	output.watchers[watcher] = struct{}{}

	return bytes.Clone(output.buffer.Bytes()), watcher, func() {
		m.outputMu.Lock()
		defer m.outputMu.Unlock()
		if current := m.outputs[id]; current != nil {
			delete(current.watchers, watcher)
			if len(current.watchers) == 0 && current.buffer.Len() == 0 {
				delete(m.outputs, id)
			}
		}
	}
}

// RequireRole is the middleware for routes that create commands.
func RequireRole(minimum string) func(e *core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		if e.HasSuperuserAuth() || (e.Auth != nil && roleAtLeast(e.Auth.GetString("role"), minimum)) {
			return e.Next()
		}
		return e.ForbiddenError("You are not allowed to do this.", nil)
	}
}

// BindRoutes adds the routes of the queue that are not plain record access.
func (m *Manager) BindRoutes(r *router.RouterGroup[*core.RequestEvent]) {
	r.POST("/commands/{id}/cancel", func(e *core.RequestEvent) error {
		reason := "Cancelled by " + e.Auth.Email()

		err := m.Cancel(e.Request.PathValue("id"), reason)
		if errors.Is(err, ErrNotCancellable) {
			return e.BadRequestError(err.Error(), nil)
		}
		if err != nil {
			return e.NotFoundError("The command does not exist.", err)
		}

		return e.NoContent(204)
	}).BindFunc(RequireRole(schema.RoleOperator))
}
