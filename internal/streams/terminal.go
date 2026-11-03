package streams

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// DefaultTerminalIdle is when a session without any input or output is closed.
const DefaultTerminalIdle = 30 * time.Minute

const (
	terminalBuffer = 256
	attachTimeout  = 30 * time.Second
)

// TerminalRequest describes a terminal session to open.
type TerminalRequest struct {
	Host      string
	Container string
	HostShell bool
	Command   []string
	User      string
	Cols      uint32
	Rows      uint32
	Label     string
	UserID    string
	Requester string
}

// TerminalEvent is what a browser receives from a session: output, or the end.
type TerminalEvent struct {
	Output []byte
	Exited bool
	Code   int32
	Error  string
}

// terminal is one session: one browser and one Exec stream of an agent.
type terminal struct {
	id     string
	host   string
	broker *Broker

	events  chan TerminalEvent
	control chan *agentv1.ExecResponse

	attached chan struct{}

	doneOnce sync.Once
	done     chan struct{}
	reason   string

	activity chan struct{}
}

// Terminal is the browser's side of a session.
type Terminal struct {
	session *terminal
}

// OpenTerminal registers a session and asks the agent to open its Exec stream.
func (b *Broker) OpenTerminal(request TerminalRequest) (*Terminal, error) {
	session := &terminal{
		id:       newSessionID(),
		host:     request.Host,
		broker:   b,
		events:   make(chan TerminalEvent, terminalBuffer),
		control:  make(chan *agentv1.ExecResponse, terminalBuffer),
		attached: make(chan struct{}),
		done:     make(chan struct{}),
		activity: make(chan struct{}, 1),
	}

	b.mu.Lock()
	b.terminals[session.id] = session
	b.mu.Unlock()

	var size *agentv1.TerminalSizeHint
	if request.Cols > 0 && request.Rows > 0 {
		size = &agentv1.TerminalSizeHint{Cols: request.Cols, Rows: request.Rows}
	}

	_, err := b.Commands.Create(context.Background(), commands.Options{
		Host:        request.Host,
		Label:       request.Label,
		RequestedBy: request.UserID,
		Requester:   request.Requester,
		Command: &agentv1.Command{Payload: &agentv1.Command_ExecStart{ExecStart: &agentv1.ExecStart{
			SessionId:   session.id,
			ContainerId: request.Container,
			Command:     request.Command,
			User:        request.User,
			Size:        size,
			HostShell:   request.HostShell,
		}}},
	})
	if err != nil {
		session.finish("The terminal could not be requested.")
		return nil, err
	}

	return &Terminal{session: session}, nil
}

// ID returns the session id.
func (t *Terminal) ID() string {
	return t.session.id
}

// Events delivers output and the end of the session. Done tells when to stop reading.
func (t *Terminal) Events() <-chan TerminalEvent {
	return t.session.events
}

// Done closes when the session is over.
func (t *Terminal) Done() <-chan struct{} {
	return t.session.done
}

// Activity signals input or output, for the idle timeout.
func (t *Terminal) Activity() <-chan struct{} {
	return t.session.activity
}

// Reason says why a session ended, if it did not simply exit.
func (t *Terminal) Reason() string {
	return t.session.reason
}

// Attached closes when the agent's stream has joined.
func (t *Terminal) Attached() <-chan struct{} {
	return t.session.attached
}

// Input sends what the user typed.
func (t *Terminal) Input(data []byte) {
	t.session.send(&agentv1.ExecResponse{Input: data})
	t.session.touch()
}

// Resize changes the size of the terminal.
func (t *Terminal) Resize(cols, rows uint32) {
	t.session.send(&agentv1.ExecResponse{Resize: &agentv1.TerminalSize{Cols: cols, Rows: rows}})
}

// Close ends the session from the browser's side. If the agent's stream has not
// joined yet, the agent is told not to bother.
func (t *Terminal) Close() {
	session := t.session

	select {
	case <-session.attached:
	default:
		_, err := session.broker.Commands.Create(context.Background(), commands.Options{
			Host: session.host,
			Command: &agentv1.Command{Payload: &agentv1.Command_ExecStop{ExecStop: &agentv1.ExecStop{
				SessionId: session.id,
			}}},
		})
		if err != nil {
			session.broker.Log.Warn("failed to stop a terminal session", "session", session.id, "error", err)
		}
	}

	session.finish("")
}

func (s *terminal) touch() {
	select {
	case s.activity <- struct{}{}:
	default:
	}
}

// send queues a message for the agent. A viewer who floods a stuck session loses input.
func (s *terminal) send(message *agentv1.ExecResponse) {
	select {
	case s.control <- message:
	case <-s.done:
	default:
	}
}

func (s *terminal) finish(reason string) {
	s.doneOnce.Do(func() {
		s.reason = reason
		close(s.done)

		s.broker.mu.Lock()
		delete(s.broker.terminals, s.id)
		s.broker.mu.Unlock()
	})
}

func (s *terminal) deliver(event TerminalEvent) {
	select {
	case s.events <- event:
	case <-s.done:
	}
	s.touch()
}

// Terminal implements gateway.StreamBroker: it joins an agent's Exec stream with
// the browser waiting for it, and blocks until the session is over.
func (b *Broker) Terminal(ctx context.Context, hostID, sessionID string, stream agentv1.AgentService_ExecServer) error {
	b.mu.Lock()
	session := b.terminals[sessionID]
	b.mu.Unlock()

	if session == nil || session.host != hostID {
		return status.Error(codes.NotFound, "nobody is waiting for this terminal session")
	}

	select {
	case <-session.attached:
		return status.Error(codes.AlreadyExists, "this terminal session already has a stream")
	default:
		close(session.attached)
	}

	// what the user types goes to the agent until the session ends
	sendDone := make(chan error, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				sendDone <- ctx.Err()
				return
			case <-session.done:
				sendDone <- stream.Send(&agentv1.ExecResponse{Close: true})
				return
			case message := <-session.control:
				if err := stream.Send(message); err != nil {
					sendDone <- err
					return
				}
			}
		}
	}()

	defer func() {
		session.finish("")
		// The sender must be done with the stream before the handler returns. What
		// it failed to send no longer matters: the session is over.
		if err := <-sendDone; err != nil && !errors.Is(err, context.Canceled) {
			b.Log.Debug("terminal input stopped", "session", sessionID, "error", err)
		}
	}()

	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			session.deliver(TerminalEvent{Exited: true, Code: -1, Error: "The connection to the agent was lost."})
			return err
		}

		if output := message.GetOutput(); len(output) > 0 {
			session.deliver(TerminalEvent{Output: output})
		}

		if message.GetExited() || message.GetError() != "" {
			session.deliver(TerminalEvent{Exited: true, Code: message.GetExitCode(), Error: message.GetError()})
			return nil
		}
	}
}

// execStartFinished ends the session of a command that could not open its stream,
// such as a refused host shell.
func (b *Broker) execStartFinished(record *core.Record) {
	if record.GetString("status") == schema.CommandSucceeded {
		return
	}

	var payload struct {
		SessionID string `json:"session_id"`
	}
	raw, err := json.Marshal(record.Get("payload"))
	if err == nil {
		err = json.Unmarshal(raw, &payload)
	}
	if err != nil {
		return
	}

	b.mu.Lock()
	session := b.terminals[payload.SessionID]
	b.mu.Unlock()

	if session != nil {
		session.deliver(TerminalEvent{Exited: true, Code: -1, Error: record.GetString("error")})
		session.finish(record.GetString("error"))
	}
}

// Terminals returns how many terminal sessions are open.
func (b *Broker) Terminals() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.terminals)
}

// AttachTimeout is how long a browser waits for the agent's stream to join.
func AttachTimeout() time.Duration {
	return attachTimeout
}
