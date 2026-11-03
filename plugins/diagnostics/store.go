package diagnostics

import (
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Disconnect reasons.
const (
	ReasonEOF              = "eof"
	ReasonKeepaliveTimeout = "keepalive_timeout"
	ReasonTLSError         = "tls_error"
	ReasonRevoked          = "revoked_certificate"
	ReasonExpired          = "expired_certificate"
	ReasonShutdown         = "server_shutdown"
	ReasonReplaced         = "replaced"
	ReasonUnknown          = "unknown"
)

// keepaliveSilence is how long a connection must have been silent before its end
// counts as a keepalive timeout rather than a closed connection.
const keepaliveSilence = 5 * time.Second

// ConnectionEvent is a connect or a disconnect of an agent.
type ConnectionEvent struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Reason string    `json:"reason,omitempty"`
	Remote string    `json:"remote,omitempty"`
}

// TraceEntry is the metadata of one message or call, and its masked payload while
// a capture runs.
type TraceEntry struct {
	At        time.Time `json:"at"`
	Agent     string    `json:"agent"`
	Direction string    `json:"direction"`
	Method    string    `json:"method"`
	Kind      string    `json:"kind"`
	Subject   string    `json:"subject,omitempty"`
	Bytes     int       `json:"bytes,omitempty"`
	Code      string    `json:"code,omitempty"`
	Error     string    `json:"error,omitempty"`
	Payload   string    `json:"payload,omitempty"`
}

type agentState struct {
	mu sync.Mutex

	id             string
	connections    int
	connectedSince time.Time
	lastSeen       time.Time
	remote         string
	tlsVersion     string
	certExpiry     time.Time
	messagesIn     uint64
	messagesOut    uint64
	bytesIn        uint64
	bytesOut       uint64
	reconnects     uint64
	everConnected  bool
	lastError      string
	lastCode       string
	lastReason     string
	clockSkewMS    float64

	rtt       []float64
	rttNext   int
	rttFilled bool

	events []ConnectionEvent

	trace     []TraceEntry
	traceNext int
	traceFull bool

	captureUntil time.Time
}

// AgentSummary is the public view of an agent's connection.
type AgentSummary struct {
	ID             string    `json:"id"`
	Connected      bool      `json:"connected"`
	ConnectedSince time.Time `json:"connected_since,omitzero"`
	LastSeen       time.Time `json:"last_seen,omitzero"`
	Remote         string    `json:"remote,omitempty"`
	TLSVersion     string    `json:"tls_version,omitempty"`
	CertExpiry     time.Time `json:"cert_expiry,omitzero"`
	RTTp50MS       float64   `json:"rtt_p50_ms"`
	RTTp95MS       float64   `json:"rtt_p95_ms"`
	ClockSkewMS    float64   `json:"clock_skew_ms"`
	MessagesIn     uint64    `json:"messages_in"`
	MessagesOut    uint64    `json:"messages_out"`
	BytesIn        uint64    `json:"bytes_in"`
	BytesOut       uint64    `json:"bytes_out"`
	Reconnects     uint64    `json:"reconnects"`
	LastError      string    `json:"last_error,omitempty"`
	LastReason     string    `json:"last_reason,omitempty"`
	CaptureUntil   time.Time `json:"capture_until,omitzero"`
}

// AgentDetail adds the history of an agent.
type AgentDetail struct {
	AgentSummary
	Events []ConnectionEvent `json:"events"`
	Trace  []TraceEntry      `json:"trace"`
}

func (p *Plugin) agent(id string) *agentState {
	p.mu.RLock()
	state := p.agents[id]
	p.mu.RUnlock()
	if state != nil {
		return state
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if state = p.agents[id]; state == nil {
		state = &agentState{
			id:    id,
			rtt:   make([]float64, rttSamples),
			trace: make([]TraceEntry, p.config.TraceEntries),
		}
		p.agents[id] = state
	}

	return state
}

func (s *agentState) addEvent(event ConnectionEvent, window time.Duration) {
	cutoff := event.At.Add(-window)

	kept := s.events[:0]
	for _, existing := range s.events {
		if existing.At.After(cutoff) {
			kept = append(kept, existing)
		}
	}
	s.events = append(kept, event)

	if len(s.events) > connectionEvents {
		s.events = s.events[len(s.events)-connectionEvents:]
	}
}

func (s *agentState) addTrace(entry TraceEntry) {
	s.trace[s.traceNext] = entry
	s.traceNext = (s.traceNext + 1) % len(s.trace)
	if s.traceNext == 0 {
		s.traceFull = true
	}
}

func (s *agentState) traceEntries(window time.Duration) []TraceEntry {
	size := s.traceNext
	if s.traceFull {
		size = len(s.trace)
	}

	cutoff := time.Now().Add(-window)

	entries := make([]TraceEntry, 0, size)
	for i := range size {
		index := i
		if s.traceFull {
			index = (s.traceNext + i) % len(s.trace)
		}
		if s.trace[index].At.After(cutoff) {
			entries = append(entries, s.trace[index])
		}
	}

	return entries
}

func (s *agentState) percentile(q float64) float64 {
	size := s.rttNext
	if s.rttFilled {
		size = len(s.rtt)
	}
	if size == 0 {
		return 0
	}

	values := slices.Clone(s.rtt[:size])
	slices.Sort(values)

	return values[int(q*float64(len(values)-1))]
}

func (s *agentState) summary() AgentSummary {
	return AgentSummary{
		ID:             s.id,
		Connected:      s.connections > 0,
		ConnectedSince: s.connectedSince,
		LastSeen:       s.lastSeen,
		Remote:         s.remote,
		TLSVersion:     s.tlsVersion,
		CertExpiry:     s.certExpiry,
		RTTp50MS:       s.percentile(0.5),
		RTTp95MS:       s.percentile(0.95),
		ClockSkewMS:    s.clockSkewMS,
		MessagesIn:     s.messagesIn,
		MessagesOut:    s.messagesOut,
		BytesIn:        s.bytesIn,
		BytesOut:       s.bytesOut,
		Reconnects:     s.reconnects,
		LastError:      s.lastError,
		LastReason:     s.lastReason,
		CaptureUntil:   s.captureUntil,
	}
}

// Agents returns a summary of every agent seen so far.
func (p *Plugin) Agents() []AgentSummary {
	p.mu.RLock()
	states := make([]*agentState, 0, len(p.agents))
	for _, state := range p.agents {
		states = append(states, state)
	}
	p.mu.RUnlock()

	summaries := make([]AgentSummary, 0, len(states))
	for _, state := range states {
		state.mu.Lock()
		summaries = append(summaries, state.summary())
		state.mu.Unlock()
	}

	slices.SortFunc(summaries, func(a, b AgentSummary) int { return strings.Compare(a.ID, b.ID) })

	return summaries
}

// Agent returns the details of an agent.
func (p *Plugin) Agent(id string) (AgentDetail, bool) {
	p.mu.RLock()
	state := p.agents[id]
	p.mu.RUnlock()
	if state == nil {
		return AgentDetail{}, false
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	return AgentDetail{
		AgentSummary: state.summary(),
		Events:       slices.Clone(state.events),
		Trace:        state.traceEntries(p.config.Window),
	}, true
}

// ObserveHeartbeat feeds the round trip and clock skew an agent measured.
func (p *Plugin) ObserveHeartbeat(agentID string, rttMS, skewMS float64) {
	if !p.config.Enabled {
		return
	}

	state := p.agent(agentID)

	state.mu.Lock()
	defer state.mu.Unlock()

	state.clockSkewMS = skewMS
	if rttMS > 0 {
		state.rtt[state.rttNext] = rttMS
		state.rttNext = (state.rttNext + 1) % len(state.rtt)
		if state.rttNext == 0 {
			state.rttFilled = true
		}
	}
}

// StartCapture records masked payloads of one agent for 10 minutes.
func (p *Plugin) StartCapture(agentID string) time.Time {
	state := p.agent(agentID)
	until := time.Now().Add(captureDuration)

	state.mu.Lock()
	defer state.mu.Unlock()

	state.captureUntil = until

	return until
}

// classifyDisconnect turns what is known about a closed connection into a reason.
func classifyDisconnect(lastError string, silence time.Duration, shuttingDown bool) string {
	text := strings.ToLower(lastError)

	switch {
	case strings.Contains(text, "revoked"):
		return ReasonRevoked
	case strings.Contains(text, "expired"):
		return ReasonExpired
	case shuttingDown || strings.Contains(text, "shutting down"):
		return ReasonShutdown
	case strings.Contains(text, "replaced by a newer"):
		return ReasonReplaced
	case strings.Contains(text, "tls") || strings.Contains(text, "handshake") || strings.Contains(text, "certificate"):
		return ReasonTLSError
	case strings.Contains(text, "keepalive"):
		return ReasonKeepaliveTimeout
	case silence >= keepaliveSilence:
		return ReasonKeepaliveTimeout
	default:
		return ReasonEOF
	}
}

// rateCounter counts messages per minute over the last 24 hours.
type rateCounter struct {
	buckets [1440]atomic.Uint64
	stamps  [1440]atomic.Int64
}

func (r *rateCounter) add(now time.Time) {
	minute := now.Unix() / 60
	index := minute % int64(len(r.buckets))

	if r.stamps[index].Swap(minute) != minute {
		r.buckets[index].Store(0)
	}
	r.buckets[index].Add(1)
}

// perMinute returns the average messages per minute over a window.
func (r *rateCounter) perMinute(now time.Time, window time.Duration) float64 {
	minutes := max(int64(window/time.Minute), 1)
	current := now.Unix() / 60

	var total uint64
	for offset := range minutes {
		minute := current - offset
		index := minute % int64(len(r.buckets))
		if r.stamps[index].Load() == minute {
			total += r.buckets[index].Load()
		}
	}

	return float64(total) / float64(minutes)
}

// queueStats counts what happens to the commands of a queue.
type queueStats struct {
	mu       sync.Mutex
	retries  uint64
	replays  uint64
	timeouts uint64
	perType  map[string]*latencyPair
}

type latencyPair struct {
	DispatchToAck latencySummary `json:"dispatch_to_ack"`
	AckToDone     latencySummary `json:"ack_to_done"`
}

type latencySummary struct {
	Count uint64  `json:"count"`
	AvgMS float64 `json:"avg_ms"`
	MaxMS float64 `json:"max_ms"`
}

func (l *latencySummary) add(duration time.Duration) {
	ms := float64(duration.Microseconds()) / 1000
	l.AvgMS = (l.AvgMS*float64(l.Count) + ms) / float64(l.Count+1)
	l.Count++
	l.MaxMS = max(l.MaxMS, ms)
}

// QueueEvent is something that happened to a queued command.
type QueueEvent int

// Queue events.
const (
	QueueRetry QueueEvent = iota
	QueueReplay
	QueueTimeout
)

// ObserveQueueEvent counts a retry, replay or timeout.
func (p *Plugin) ObserveQueueEvent(event QueueEvent) {
	if !p.config.Enabled {
		return
	}

	p.queue.mu.Lock()
	defer p.queue.mu.Unlock()

	switch event {
	case QueueRetry:
		p.queue.retries++
	case QueueReplay:
		p.queue.replays++
	case QueueTimeout:
		p.queue.timeouts++
	}
}

// ObserveCommandLatency records the time from dispatch to the agent's
// acknowledgement and from there to the final result, per command type.
func (p *Plugin) ObserveCommandLatency(commandType string, dispatchToAck, ackToDone time.Duration) {
	if !p.config.Enabled {
		return
	}

	p.queue.mu.Lock()
	defer p.queue.mu.Unlock()

	pair := p.queue.perType[commandType]
	if pair == nil {
		pair = &latencyPair{}
		p.queue.perType[commandType] = pair
	}

	if dispatchToAck > 0 {
		pair.DispatchToAck.add(dispatchToAck)
	}
	if ackToDone > 0 {
		pair.AckToDone.add(ackToDone)
	}
}
