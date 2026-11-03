package streams

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

const relayConnBacklog = 256

// ErrRelayClosed is returned by Dial when the session is over.
var ErrRelayClosed = errors.New("the tool's session has ended")

// Relay is a session with a temporary tool container, such as Adminer, behind an
// agent. One agent stream carries every connection of the session; connections
// are numbered here, and the agent opens one to the tool for each.
type Relay struct {
	id      string
	host    string
	broker  *Broker
	expires time.Time

	attached chan struct{}
	done     chan struct{}
	doneOnce sync.Once
	reason   string

	mu      sync.Mutex
	stream  agentv1.AgentService_RelayServer
	nextID  uint32
	conns   map[uint32]*relayConn
	timer   *time.Timer
	sendMu  sync.Mutex
	started bool
}

// relayConn is the panel's end of one connection: the other end of the pipe is
// what Dial returned.
type relayConn struct {
	id       uint32
	pipe     net.Conn
	incoming chan []byte

	// eof says that the tool closed the connection: what it sent before still
	// has to reach the reader, so the pipe stays open until then
	eofOnce sync.Once
	eof     chan struct{}

	once   sync.Once
	closed chan struct{}
}

func (c *relayConn) close() {
	c.once.Do(func() {
		close(c.closed)
		c.pipe.Close()
	})
}

func (c *relayConn) endOfData() {
	c.eofOnce.Do(func() { close(c.eof) })
}

// OpenRelay registers a relay session of a host that ends after ttl. Queue the
// command that makes the agent open the stream with its ID.
func (b *Broker) OpenRelay(host string, ttl time.Duration) *Relay {
	relay := &Relay{
		id:       newSessionID(),
		host:     host,
		broker:   b,
		expires:  time.Now().Add(ttl),
		attached: make(chan struct{}),
		done:     make(chan struct{}),
		conns:    make(map[uint32]*relayConn),
	}
	relay.timer = time.AfterFunc(ttl, func() { relay.Close("The session reached its time limit.") })

	b.mu.Lock()
	if b.relays == nil {
		b.relays = make(map[string]*Relay)
	}
	b.relays[relay.id] = relay
	b.mu.Unlock()

	return relay
}

// FindRelay returns an open relay session, or nil.
func (b *Broker) FindRelay(id string) *Relay {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.relays[id]
}

// ID returns the session id.
func (r *Relay) ID() string {
	return r.id
}

// Host returns the host of the session.
func (r *Relay) Host() string {
	return r.host
}

// Expires is when the session ends at the latest.
func (r *Relay) Expires() time.Time {
	return r.expires
}

// Done closes when the session is over.
func (r *Relay) Done() <-chan struct{} {
	return r.done
}

// Reason says why a session ended, if it did not simply time out or close.
func (r *Relay) Reason() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reason
}

// Close ends the session: its connections close, and the agent removes the tool.
func (r *Relay) Close(reason string) {
	r.doneOnce.Do(func() {
		r.mu.Lock()
		r.reason = reason
		conns := r.conns
		r.conns = map[uint32]*relayConn{}
		stream := r.stream
		r.mu.Unlock()

		r.timer.Stop()
		close(r.done)

		for _, conn := range conns {
			conn.close()
		}
		if stream != nil {
			r.send(&agentv1.RelayResponse{Close: true})
		}

		r.broker.mu.Lock()
		delete(r.broker.relays, r.id)
		r.broker.mu.Unlock()
	})
}

func (r *Relay) send(message *agentv1.RelayResponse) bool {
	r.mu.Lock()
	stream := r.stream
	r.mu.Unlock()
	if stream == nil {
		return false
	}

	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	return stream.Send(message) == nil
}

// Dial opens a connection to the tool. It waits for the agent's stream to join.
func (r *Relay) Dial(ctx context.Context) (net.Conn, error) {
	select {
	case <-r.attached:
	case <-r.done:
		return nil, ErrRelayClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	client, server := net.Pipe()

	r.mu.Lock()
	if r.stream == nil {
		r.mu.Unlock()
		return nil, ErrRelayClosed
	}
	r.nextID++
	conn := &relayConn{
		id:       r.nextID,
		pipe:     server,
		incoming: make(chan []byte, relayConnBacklog),
		eof:      make(chan struct{}),
		closed:   make(chan struct{}),
	}
	r.conns[conn.id] = conn
	r.mu.Unlock()

	if !r.send(&agentv1.RelayResponse{ConnId: conn.id, Open: true}) {
		r.forget(conn)
		return nil, ErrRelayClosed
	}

	// what the browser's request writes goes to the agent
	go func() {
		buffer := make([]byte, 32<<10)
		for {
			count, err := server.Read(buffer)
			if count > 0 && !r.send(&agentv1.RelayResponse{ConnId: conn.id, Data: append([]byte(nil), buffer[:count]...)}) {
				r.forget(conn)
				return
			}
			if err != nil {
				select {
				case <-conn.closed:
				default:
					r.send(&agentv1.RelayResponse{ConnId: conn.id, Close: true})
				}
				r.forget(conn)
				return
			}
		}
	}()

	// and what the tool answers goes back, in order, without holding up the stream
	go func() {
		for {
			select {
			case data := <-conn.incoming:
				if _, err := server.Write(data); err != nil {
					r.forget(conn)
					return
				}
			case <-conn.eof:
				// the tool sent everything before it closed: deliver it, then end
				for {
					select {
					case data := <-conn.incoming:
						if _, err := server.Write(data); err != nil {
							r.forget(conn)
							return
						}
					default:
						r.forget(conn)
						return
					}
				}
			case <-conn.closed:
				return
			}
		}
	}()

	return client, nil
}

func (r *Relay) forget(conn *relayConn) {
	r.mu.Lock()
	if r.conns[conn.id] == conn {
		delete(r.conns, conn.id)
	}
	r.mu.Unlock()
	conn.close()
}

func (r *Relay) conn(id uint32) *relayConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conns[id]
}

// Relay implements gateway.StreamBroker: it joins an agent's relay stream with its
// session and carries the connections until the session ends.
func (b *Broker) Relay(ctx context.Context, hostID, sessionID string, stream agentv1.AgentService_RelayServer) error {
	relay := b.FindRelay(sessionID)
	if relay == nil || relay.host != hostID {
		return status.Error(codes.NotFound, "nobody is waiting for this relay")
	}

	relay.mu.Lock()
	if relay.started {
		relay.mu.Unlock()
		return status.Error(codes.AlreadyExists, "this relay already has a stream")
	}
	relay.started = true
	relay.stream = stream
	relay.mu.Unlock()
	close(relay.attached)

	received := make(chan error, 1)
	go func() {
		for {
			message, err := stream.Recv()
			if err != nil {
				received <- err
				return
			}

			conn := relay.conn(message.GetConnId())
			if conn == nil {
				continue
			}
			if data := message.GetData(); len(data) > 0 {
				select {
				case conn.incoming <- data:
				case <-conn.closed:
				default:
					// a reader this slow is not reading: the connection ends rather than the stream
					relay.forget(conn)
					relay.send(&agentv1.RelayResponse{ConnId: conn.id, Close: true})
				}
			}
			if message.GetClosed() {
				if message.GetError() != "" {
					b.Log.Debug("a relayed connection was refused", "session", relay.id, "error", message.GetError())
				}
				conn.endOfData()
			}
		}
	}()

	select {
	case err := <-received:
		if errors.Is(err, io.EOF) {
			relay.Close("")
			return nil
		}
		relay.Close("The connection to the agent was lost.")
		return err
	case <-relay.done:
		return nil
	case <-ctx.Done():
		relay.Close("The connection to the agent was lost.")
		return ctx.Err()
	}
}

// adminerStartFinished ends the session of an Adminer that could not start.
func (b *Broker) adminerStartFinished(record *core.Record) {
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

	if relay := b.FindRelay(payload.SessionID); relay != nil {
		relay.Close(record.GetString("error"))
	}
}

// Relays returns how many relay sessions are open.
func (b *Broker) Relays() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.relays)
}
