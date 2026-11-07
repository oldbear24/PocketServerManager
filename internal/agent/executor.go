package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
)

const (
	maxWorkers    = 8
	reportTimeout = 15 * time.Second
	outputChunk   = 64 << 10
	// outputFlush is how long output may wait before it goes to the panel: someone
	// may be watching a deploy.
	outputFlush = 300 * time.Millisecond
)

// perTypeLimits caps commands of a kind on top of the global worker limit: pulls
// and backups are heavy, and the panel already orders commands per resource.
var perTypeLimits = map[string]int{
	"image.pull":  2,
	"stack.":      3,
	"site.deploy": 2,
	"backup.":     2,
	"db.":         3,
	// scheduled tasks may run for hours: they never take every worker
	"task.": 4,
}

// errHandedOver is returned by a handler that started work which outlives this
// process. Whoever runs next reports the final state.
var errHandedOver = errors.New("the command continues outside of this process")

// Handler executes one command type.
type Handler func(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error)

// Executor runs commands in a worker pool and makes sure each of them runs at
// most once, also across restarts of the agent.
type Executor struct {
	store    *commandStore
	handlers map[string]Handler
	client   atomic.Pointer[agentv1.AgentServiceClient]
	log      *slog.Logger

	workers chan struct{}
	limits  map[string]chan struct{}

	mu      sync.Mutex
	running map[string]context.CancelFunc
	wg      sync.WaitGroup

	busy atomic.Int32
	base context.Context
	stop context.CancelFunc
}

func newExecutor(store *commandStore, log *slog.Logger) *Executor {
	base, stop := context.WithCancel(context.Background())

	limits := make(map[string]chan struct{}, len(perTypeLimits))
	for prefix, limit := range perTypeLimits {
		limits[prefix] = make(chan struct{}, limit)
	}

	return &Executor{
		store:    store,
		handlers: make(map[string]Handler),
		log:      log,
		workers:  make(chan struct{}, maxWorkers),
		limits:   limits,
		running:  make(map[string]context.CancelFunc),
		base:     base,
		stop:     stop,
	}
}

// Handle registers the handler of a command type.
func (x *Executor) Handle(commandType string, handler Handler) {
	x.handlers[commandType] = handler
}

// Capabilities lists the command types this agent can execute.
func (x *Executor) Capabilities() []string {
	capabilities := make([]string, 0, len(x.handlers))
	for commandType := range x.handlers {
		capabilities = append(capabilities, commandType)
	}
	slices.Sort(capabilities)
	return capabilities
}

// SetClient sets the connection used for reports, or nil while disconnected.
func (x *Executor) SetClient(client agentv1.AgentServiceClient) {
	if client == nil {
		x.client.Store(nil)
		return
	}
	x.client.Store(&client)
}

// Submit accepts a command from the panel. A replay of a command that has been
// seen before is answered from the store and never executed again.
func (x *Executor) Submit(command *agentv1.Command) {
	id := command.GetId()

	if entry, seen := x.store.get(id); seen {
		x.mu.Lock()
		_, alive := x.running[id]
		x.mu.Unlock()

		switch {
		case entry.finished():
			x.reportFinal(id, entry.protoState(), entry.Error, entry.Result, true)
		case alive:
			// still running: its updates reach the panel by themselves
		case entry.State == stateHandedOver:
			// the outcome is reported as soon as the updater has written it
		default:
			// it started, and the process that ran it is gone; running it again
			// could do the work twice
			message := "The agent restarted while running this command, so it may be incomplete."
			if err := x.store.setState(id, stateFailed, message, nil); err != nil {
				x.log.Error("failed to store a command state", "command", id, "error", err)
			}
			x.reportFinal(id, agentv1.CommandState_COMMAND_STATE_FAILED, message, nil, true)
		}
		return
	}

	handler, ok := x.handlers[command.GetType()]
	if !ok {
		x.reportFinal(id, agentv1.CommandState_COMMAND_STATE_FAILED, "This agent does not support "+command.GetType()+".", nil, false)
		return
	}

	if err := x.store.start(id, command.GetType()); err != nil {
		x.log.Error("failed to store a command", "command", id, "error", err)
		x.reportFinal(id, agentv1.CommandState_COMMAND_STATE_FAILED, "The agent could not write to its volume: "+err.Error(), nil, false)
		return
	}

	ctx, cancel := context.WithCancel(x.base)
	if deadline := command.GetDeadline(); deadline != nil && !deadline.AsTime().IsZero() {
		ctx, cancel = context.WithDeadline(x.base, deadline.AsTime())
	}

	x.mu.Lock()
	x.running[id] = cancel
	x.mu.Unlock()

	x.wg.Add(1)
	go x.run(ctx, cancel, command, handler)
}

func (x *Executor) run(ctx context.Context, cancel context.CancelFunc, command *agentv1.Command, handler Handler) {
	defer x.wg.Done()
	defer cancel()

	id := command.GetId()

	defer func() {
		x.mu.Lock()
		delete(x.running, id)
		x.mu.Unlock()
	}()

	release, err := x.acquire(ctx, command.GetType())
	if err != nil {
		x.finish(id, stateCancelled, "The command was cancelled while waiting for a free worker.", nil)
		return
	}
	defer release()

	x.busy.Add(1)
	defer x.busy.Add(-1)

	reporter := &Reporter{executor: x, commandID: id}
	reporter.state(agentv1.CommandState_COMMAND_STATE_RUNNING, nil)

	x.log.Debug("running a command", "command", id, "type", command.GetType())

	stopFlushing := reporter.flushRegularly()
	result, err := x.safely(ctx, handler, command, reporter)
	stopFlushing()
	reporter.flush()

	switch {
	case errors.Is(err, errHandedOver):
		if storeErr := x.store.setState(id, stateHandedOver, "", nil); storeErr != nil {
			x.log.Error("failed to store a command state", "command", id, "error", storeErr)
		}
	case err == nil:
		x.finish(id, stateSucceeded, "", result)
	case errors.Is(ctx.Err(), context.Canceled):
		x.finish(id, stateCancelled, err.Error(), result)
	default:
		x.finish(id, stateFailed, err.Error(), result)
	}
}

// safely keeps a panicking handler from taking the whole agent down.
func (x *Executor) safely(ctx context.Context, handler Handler, command *agentv1.Command, reporter *Reporter) (result map[string]any, err error) {
	defer func() {
		if r := recover(); r != nil {
			x.log.Error("a command handler panicked", "type", command.GetType(), "panic", r)
			err = fmt.Errorf("the agent failed unexpectedly: %v", r)
		}
	}()
	return handler(ctx, command, reporter)
}

func (x *Executor) acquire(ctx context.Context, commandType string) (func(), error) {
	var limit chan struct{}
	for prefix, candidate := range x.limits {
		if strings.HasPrefix(commandType, prefix) {
			limit = candidate
			break
		}
	}

	if limit != nil {
		select {
		case limit <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	select {
	case x.workers <- struct{}{}:
	case <-ctx.Done():
		if limit != nil {
			<-limit
		}
		return nil, ctx.Err()
	}

	return func() {
		<-x.workers
		if limit != nil {
			<-limit
		}
	}, nil
}

func (x *Executor) finish(id, state, message string, result map[string]any) {
	if err := x.store.setState(id, state, message, result); err != nil {
		x.log.Error("failed to store a command state", "command", id, "error", err)
	}

	entry := commandEntry{State: state}
	x.reportFinal(id, entry.protoState(), message, result, false)
}

// reportFinal sends a final state. If that fails it stays in the store, and the
// panel gets it with the next SubscribeCommands.
func (x *Executor) reportFinal(id string, state agentv1.CommandState, message string, result map[string]any, duplicate bool) {
	update := &agentv1.ReportCommandUpdateRequest{
		CommandId: id,
		// the final report always goes through, whatever came before it
		Sequence:  uint64(time.Now().UnixNano()), //nolint:gosec // a positive clock value
		State:     state,
		Error:     message,
		Duplicate: duplicate,
	}

	if result != nil {
		encoded, err := structpb.NewStruct(result)
		if err != nil {
			x.log.Error("a command result cannot be encoded", "command", id, "error", err)
		} else {
			update.Result = encoded
		}
	}

	if err := x.send(update); err != nil {
		x.log.Debug("a final report waits for the next connection", "command", id, "error", err)
	}
}

func (x *Executor) send(update *agentv1.ReportCommandUpdateRequest) error {
	client := x.client.Load()
	if client == nil {
		return errors.New("not connected")
	}

	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()

	response, err := (*client).ReportCommandUpdate(ctx, update)
	if err != nil {
		return err
	}

	if response.GetCancel() {
		x.Cancel(update.GetCommandId())
	}

	return nil
}

// Cancel stops a running command.
func (x *Executor) Cancel(id string) {
	x.mu.Lock()
	cancel := x.running[id]
	x.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// Shutdown cancels what is running and waits for the final reports.
func (x *Executor) Shutdown(timeout time.Duration) {
	x.stop()

	done := make(chan struct{})
	go func() {
		x.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		x.log.Warn("some commands did not stop in time")
	}
}

// Reporter sends a command's progress and output to the panel.
type Reporter struct {
	executor  *Executor
	commandID string

	// sending keeps updates in the order of their sequence numbers
	sending sync.Mutex

	mu       sync.Mutex
	sequence uint64
	pending  []byte
}

// flushRegularly sends pending output until the returned function is called.
func (r *Reporter) flushRegularly() (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		defer close(finished)

		ticker := time.NewTicker(outputFlush)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				r.flush()
			}
		}
	}()

	return func() {
		close(done)
		<-finished
	}
}

// Progress reports the current step. A percent of -1 means unknown.
func (r *Reporter) Progress(step, message string, percent int32) {
	r.state(agentv1.CommandState_COMMAND_STATE_RUNNING, &agentv1.CommandProgress{Step: step, Message: message, Percent: percent})
}

// Write adds to the command's output; it makes a Reporter an io.Writer.
func (r *Reporter) Write(data []byte) (int, error) {
	r.mu.Lock()
	r.pending = append(r.pending, data...)
	full := len(r.pending) >= outputChunk
	r.mu.Unlock()

	if full {
		r.flush()
	}

	return len(data), nil
}

func (r *Reporter) flush() {
	r.mu.Lock()
	empty := len(r.pending) == 0
	r.mu.Unlock()

	if !empty {
		r.state(agentv1.CommandState_COMMAND_STATE_RUNNING, nil)
	}
}

func (r *Reporter) state(state agentv1.CommandState, progress *agentv1.CommandProgress) {
	r.sending.Lock()
	defer r.sending.Unlock()

	r.mu.Lock()
	r.sequence++
	update := &agentv1.ReportCommandUpdateRequest{
		CommandId: r.commandID,
		Sequence:  r.sequence,
		State:     state,
		Progress:  progress,
		Output:    r.pending,
	}
	r.pending = nil
	r.mu.Unlock()

	if err := r.executor.send(update); err != nil {
		r.executor.log.Debug("a progress update was lost", "command", r.commandID, "error", err)
	}
}
