package streams

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pocketbase/pocketbase/core"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

const (
	// DefaultTail is how many past lines a viewer starts with.
	DefaultTail = 200
	// MaxTail is the most past lines a viewer can ask for, and what the ring holds.
	MaxTail = 1000

	// viewerBacklog is how many batches may wait for a slow viewer. An agent sends
	// up to 2000 lines per second, in batches of at most 200 lines.
	viewerBacklog = 64
)

// LogLine is one line on its way to the browsers.
type LogLine struct {
	TS        string `json:"ts"`
	Container string `json:"container,omitempty"`
	Service   string `json:"service,omitempty"`
	Stream    string `json:"stream,omitempty"`
	Text      string `json:"text"`
}

// LogRequest asks for the logs of containers of one host.
type LogRequest struct {
	Host       string
	Containers []string
	Tail       int
	Label      string
	UserID     string
	Requester  string
}

// logSession is one agent stream. Everyone watching the same containers shares it.
type logSession struct {
	id     string
	host   string
	source string

	viewers map[*LogViewer]struct{}

	// ring holds recent lines for those who join later. It lives only in memory.
	ring []LogLine
}

// LogViewer is one browser following a session.
type LogViewer struct {
	broker  *Broker
	session *logSession

	lines   chan []LogLine
	dropped atomic.Int64

	endOnce sync.Once
	reason  atomic.Pointer[string]
	left    atomic.Bool
}

// Lines delivers batches of lines. It closes when the stream has ended.
func (v *LogViewer) Lines() <-chan []LogLine {
	return v.lines
}

// TakeDropped returns how many lines this viewer lost since the last call: a viewer
// that reads too slowly loses lines instead of holding up the others.
func (v *LogViewer) TakeDropped() int64 {
	return v.dropped.Swap(0)
}

// Reason says why the stream ended, if it was not simply over.
func (v *LogViewer) Reason() string {
	if reason := v.reason.Load(); reason != nil {
		return *reason
	}
	return ""
}

func (v *LogViewer) end(reason string) {
	v.endOnce.Do(func() {
		if reason != "" {
			v.reason.Store(&reason)
		}
		close(v.lines)
	})
}

// Leave stops following. The last viewer to leave stops the agent's stream.
func (v *LogViewer) Leave() {
	if v.left.Swap(true) {
		return
	}
	v.broker.leave(v)
}

func logSource(host string, containers []string) string {
	sorted := slices.Clone(containers)
	slices.Sort(sorted)
	return host + "|" + strings.Join(sorted, ",")
}

// FollowLogs joins the stream of these containers, opening it if nobody is
// watching them yet. A new viewer starts with the most recent lines.
func (b *Broker) FollowLogs(request LogRequest) (*LogViewer, error) {
	tail := request.Tail
	if tail <= 0 {
		tail = DefaultTail
	}
	tail = min(tail, MaxTail)

	source := logSource(request.Host, request.Containers)

	b.mu.Lock()

	session, joined := b.logSources[source]
	if !joined {
		session = &logSession{
			id:      newSessionID(),
			host:    request.Host,
			source:  source,
			viewers: make(map[*LogViewer]struct{}),
		}
		b.logSources[source] = session
		b.logSessions[session.id] = session
	}

	viewer := &LogViewer{broker: b, session: session, lines: make(chan []LogLine, viewerBacklog)}
	session.viewers[viewer] = struct{}{}

	if count := min(tail, len(session.ring)); count > 0 {
		viewer.lines <- slices.Clone(session.ring[len(session.ring)-count:])
	}

	b.mu.Unlock()

	if joined {
		return viewer, nil
	}

	label := request.Label
	if label == "" {
		label = strings.Join(request.Containers, ", ")
	}

	_, err := b.Commands.Create(context.Background(), commands.Options{
		Host:        request.Host,
		Label:       label,
		RequestedBy: request.UserID,
		Requester:   request.Requester,
		Command: &agentv1.Command{Payload: &agentv1.Command_LogsFollow{LogsFollow: &agentv1.LogsFollow{
			SessionId:    session.id,
			ContainerIds: request.Containers,
			Tail:         int32(tail), //nolint:gosec // limited by MaxTail
		}}},
	})
	if err != nil {
		b.endLogSession(session, "The logs could not be requested.")
		return nil, err
	}

	return viewer, nil
}

// leave removes a viewer, and the session with its last viewer.
func (b *Broker) leave(viewer *LogViewer) {
	session := viewer.session

	b.mu.Lock()
	delete(session.viewers, viewer)
	last := len(session.viewers) == 0 && b.logSessions[session.id] == session
	if last {
		b.removeLogSession(session)
	}
	b.mu.Unlock()

	viewer.end("")

	if !last {
		return
	}

	_, err := b.Commands.Create(context.Background(), commands.Options{
		Host:  session.host,
		Label: session.source[len(session.host)+1:],
		Command: &agentv1.Command{Payload: &agentv1.Command_LogsStop{LogsStop: &agentv1.LogsStop{
			SessionId: session.id,
		}}},
	})
	if err != nil {
		// the agent's next send finds the session gone, which ends its stream as well
		b.Log.Warn("failed to stop a log stream", "session", session.id, "error", err)
	}
}

func (b *Broker) removeLogSession(session *logSession) {
	delete(b.logSessions, session.id)
	if b.logSources[session.source] == session {
		delete(b.logSources, session.source)
	}
}

// endLogSession closes a session for everyone watching it.
func (b *Broker) endLogSession(session *logSession, reason string) {
	b.mu.Lock()
	b.removeLogSession(session)
	viewers := make([]*LogViewer, 0, len(session.viewers))
	for viewer := range session.viewers {
		viewers = append(viewers, viewer)
	}
	session.viewers = make(map[*LogViewer]struct{})
	b.mu.Unlock()

	for _, viewer := range viewers {
		viewer.end(reason)
	}
}

// Logs implements gateway.StreamBroker: it fans a batch out to the viewers.
func (b *Broker) Logs(hostID, sessionID string, lines []*agentv1.LogLine) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	session := b.logSessions[sessionID]
	if session == nil || session.host != hostID {
		return false
	}

	if len(lines) == 0 {
		return true
	}

	batch := make([]LogLine, 0, len(lines))
	for _, line := range lines {
		batch = append(batch, LogLine{
			TS:        line.GetTs().AsTime().UTC().Format(time.RFC3339Nano),
			Container: line.GetContainer(),
			Service:   line.GetService(),
			Stream:    line.GetStream(),
			Text:      line.GetText(),
		})
	}

	session.ring = append(session.ring, batch...)
	if extra := len(session.ring) - MaxTail; extra > 0 {
		session.ring = slices.Delete(session.ring, 0, extra)
	}

	for viewer := range session.viewers {
		select {
		case viewer.lines <- batch:
		default:
			viewer.dropped.Add(int64(len(batch)))
		}
	}

	return true
}

// LogsClosed implements gateway.StreamBroker: the agent's stream has ended.
func (b *Broker) LogsClosed(hostID, sessionID string) {
	b.mu.Lock()
	session := b.logSessions[sessionID]
	b.mu.Unlock()

	if session == nil || session.host != hostID {
		return
	}

	b.endLogSession(session, "")
}

// logFollowFinished ends the session of a command that could not open its stream.
func (b *Broker) logFollowFinished(record *core.Record) {
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
	session := b.logSessions[payload.SessionID]
	b.mu.Unlock()

	if session != nil {
		b.endLogSession(session, record.GetString("error"))
	}
}

// Viewers returns how many browsers are watching logs, for diagnostics.
func (b *Broker) Viewers() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	count := 0
	for _, session := range b.logSessions {
		count += len(session.viewers)
	}
	return count
}

// LogSessions returns how many agent log streams are open.
func (b *Broker) LogSessions() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.logSessions)
}

// Close ends every session, when the panel shuts down.
func (b *Broker) Close() {
	b.mu.Lock()
	sessions := make([]*logSession, 0, len(b.logSessions))
	for _, session := range b.logSessions {
		sessions = append(sessions, session)
	}
	terminals := make([]*terminal, 0, len(b.terminals))
	for _, session := range b.terminals {
		terminals = append(terminals, session)
	}
	b.mu.Unlock()

	for _, session := range sessions {
		b.endLogSession(session, "The panel is shutting down.")
	}
	for _, session := range terminals {
		session.finish("The panel is shutting down.")
	}
}
