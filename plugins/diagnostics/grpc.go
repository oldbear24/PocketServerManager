package diagnostics

import (
	"context"
	"crypto/tls"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	directionIn  = "in"
	directionOut = "out"

	kindOther = "other"
	kindError = "error"
)

func remoteAddressOf(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}

type connKey struct{}
type rpcKey struct{}

// connState follows one connection, which belongs to one agent once it made a call.
type connState struct {
	agent        atomic.Pointer[agentState]
	remote       string
	lastActivity atomic.Int64
	lastError    atomic.Pointer[string]
}

// StatsHandler returns the gRPC stats handler: connection begin and end, payload
// sizes and RPC timing.
func (p *Plugin) StatsHandler() stats.Handler {
	return &statsHandler{plugin: p}
}

type statsHandler struct {
	plugin *Plugin
}

// MarkShutdown makes connections that close from now on count as a server shutdown.
func (p *Plugin) MarkShutdown() {
	p.shuttingDown.Store(true)
}

func (h *statsHandler) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	state := &connState{}
	if info.RemoteAddr != nil {
		state.remote = info.RemoteAddr.String()
	}
	state.lastActivity.Store(time.Now().UnixNano())
	return context.WithValue(ctx, connKey{}, state)
}

func (h *statsHandler) HandleConn(ctx context.Context, event stats.ConnStats) {
	state, _ := ctx.Value(connKey{}).(*connState)
	if state == nil {
		return
	}

	if _, ended := event.(*stats.ConnEnd); ended {
		h.plugin.connectionEnded(state)
	}
}

func (h *statsHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	return context.WithValue(ctx, rpcKey{}, info.FullMethodName)
}

func (h *statsHandler) HandleRPC(ctx context.Context, event stats.RPCStats) {
	state, _ := ctx.Value(connKey{}).(*connState)
	if state == nil {
		return
	}

	now := time.Now()
	state.lastActivity.Store(now.UnixNano())

	agent := state.agent.Load()
	if agent == nil {
		agent = h.plugin.bindConnection(ctx, state)
		if agent == nil {
			return
		}
	}

	switch e := event.(type) {
	case *stats.InPayload:
		h.plugin.rate.add(now)
		agent.mu.Lock()
		agent.messagesIn++
		agent.bytesIn += uint64(e.WireLength) //nolint:gosec // a length
		agent.lastSeen = now
		agent.mu.Unlock()

	case *stats.OutPayload:
		h.plugin.rate.add(now)
		agent.mu.Lock()
		agent.messagesOut++
		agent.bytesOut += uint64(e.WireLength) //nolint:gosec // a length
		agent.mu.Unlock()

	case *stats.End:
		if e.Error != nil {
			message := e.Error.Error()
			state.lastError.Store(&message)
		}
	}
}

// bindConnection ties a connection to its agent, on the first call it makes:
// before that, nothing says who is on the other end.
func (p *Plugin) bindConnection(ctx context.Context, state *connState) *agentState {
	id := p.config.AgentOf(ctx)
	if id == "" {
		return nil
	}

	agent := p.agent(id)
	if !state.agent.CompareAndSwap(nil, agent) {
		return state.agent.Load()
	}

	now := time.Now().UTC()

	agent.mu.Lock()
	first := agent.connections == 0
	agent.connections++
	if first {
		agent.connectedSince = now
		agent.remote = state.remote
		if agent.everConnected {
			agent.reconnects++
		}
		agent.everConnected = true
	}
	if pr, ok := peer.FromContext(ctx); ok {
		if info, ok := pr.AuthInfo.(credentials.TLSInfo); ok {
			agent.tlsVersion = tls.VersionName(info.State.Version)
		}
	}
	if p.config.CertExpiry != nil {
		agent.certExpiry = p.config.CertExpiry(ctx)
	}
	event := ConnectionEvent{At: now, Kind: "connected", Remote: state.remote}
	if first {
		agent.addEvent(event, p.config.Window)
	}
	agent.mu.Unlock()

	if first && p.config.OnConnection != nil {
		p.config.OnConnection(id, event)
	}

	return agent
}

func (p *Plugin) connectionEnded(state *connState) {
	agent := state.agent.Load()
	if agent == nil {
		return
	}

	lastError := ""
	if message := state.lastError.Load(); message != nil {
		lastError = *message
	}

	silence := time.Since(time.Unix(0, state.lastActivity.Load()))
	reason := classifyDisconnect(lastError, silence, p.shuttingDown.Load())

	now := time.Now().UTC()
	event := ConnectionEvent{At: now, Kind: "disconnected", Reason: reason, Remote: state.remote}

	agent.mu.Lock()
	agent.connections--
	last := agent.connections <= 0
	if last {
		agent.connections = 0
		agent.lastReason = reason
		agent.addEvent(event, p.config.Window)
	}
	id := agent.id
	agent.mu.Unlock()

	if last && p.config.OnConnection != nil {
		p.config.OnConnection(id, event)
	}
}

// UnaryInterceptor records status codes and message metadata of unary calls.
func (p *Plugin) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		p.traceMessage(ctx, info.FullMethod, directionIn, req)

		response, err := handler(ctx, req)

		if err != nil {
			p.traceError(ctx, info.FullMethod, err)
		} else {
			p.traceMessage(ctx, info.FullMethod, directionOut, response)
		}

		return response, err
	}
}

// StreamInterceptor records the messages of streaming calls.
func (p *Plugin) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		err := handler(srv, &observedStream{ServerStream: stream, plugin: p, method: info.FullMethod})
		if err != nil {
			p.traceError(stream.Context(), info.FullMethod, err)
		}
		return err
	}
}

type observedStream struct {
	grpc.ServerStream
	plugin *Plugin
	method string
}

func (s *observedStream) SendMsg(message any) error {
	s.plugin.traceMessage(s.Context(), s.method, directionOut, message)
	return s.ServerStream.SendMsg(message)
}

func (s *observedStream) RecvMsg(message any) error {
	err := s.ServerStream.RecvMsg(message)
	if err == nil {
		s.plugin.traceMessage(s.Context(), s.method, directionIn, message)
	}
	return err
}

// agentIDOf prefers what the connection already knows: finding out who is calling
// may involve verifying a certificate, which is too slow to do for every message.
func (p *Plugin) agentIDOf(ctx context.Context) string {
	if state, ok := ctx.Value(connKey{}).(*connState); ok {
		if agent := state.agent.Load(); agent != nil {
			return agent.id
		}
	}
	return p.config.AgentOf(ctx)
}

func (p *Plugin) traceMessage(ctx context.Context, method, direction string, message any) {
	agentID := p.agentIDOf(ctx)
	if agentID == "" {
		return
	}

	entry := TraceEntry{
		At:        time.Now().UTC(),
		Agent:     agentID,
		Direction: direction,
		Method:    shortMethod(method),
		Kind:      kindOther,
	}

	protoMessage, isProto := message.(proto.Message)
	if isProto {
		entry.Bytes = proto.Size(protoMessage)
		if p.config.Describe != nil {
			if kind, subject := p.config.Describe(protoMessage); kind != "" {
				entry.Kind = kind
				entry.Subject = subject
			}
		}
	}

	agent := p.agent(agentID)

	agent.mu.Lock()
	capturing := time.Now().Before(agent.captureUntil)
	agent.mu.Unlock()

	// the expensive part only happens while someone asked for it
	if capturing && isProto {
		entry.Payload = MaskedJSON(protoMessage)
	}

	agent.mu.Lock()
	agent.addTrace(entry)
	agent.mu.Unlock()

	p.publish(entry)
}

func (p *Plugin) traceError(ctx context.Context, method string, err error) {
	agentID := p.agentIDOf(ctx)
	if agentID == "" {
		return
	}

	converted := status.Convert(err)

	entry := TraceEntry{
		At:        time.Now().UTC(),
		Agent:     agentID,
		Direction: directionOut,
		Method:    shortMethod(method),
		Kind:      kindError,
		Code:      converted.Code().String(),
		Error:     converted.Message(),
	}

	agent := p.agent(agentID)

	agent.mu.Lock()
	agent.addTrace(entry)
	agent.lastError = converted.Message()
	agent.lastCode = converted.Code().String()
	agent.mu.Unlock()

	if state, ok := ctx.Value(connKey{}).(*connState); ok {
		message := converted.Message()
		state.lastError.Store(&message)
	}

	p.publish(entry)
}

func shortMethod(full string) string {
	for i := len(full) - 1; i >= 0; i-- {
		if full[i] == '/' {
			return full[i+1:]
		}
	}
	return full
}

// subscribe adds a viewer of the live trace; filter is an agent id, or empty for all.
func (p *Plugin) subscribe(filter string) (chan TraceEntry, func()) {
	channel := make(chan TraceEntry, 256)

	p.subscribersMu.Lock()
	p.subscribers[channel] = filter
	p.subscribersMu.Unlock()

	return channel, func() {
		p.subscribersMu.Lock()
		delete(p.subscribers, channel)
		p.subscribersMu.Unlock()
	}
}

func (p *Plugin) publish(entry TraceEntry) {
	p.subscribersMu.Lock()
	defer p.subscribersMu.Unlock()

	for channel, filter := range p.subscribers {
		if filter != "" && filter != entry.Agent {
			continue
		}
		select {
		case channel <- entry:
		default: // a slow viewer loses entries; the ring buffer still has them
		}
	}
}
