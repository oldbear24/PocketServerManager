package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"

	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/streams"
)

const (
	// terminalTicketLifetime only has to bridge the moment between asking for a
	// terminal and the browser opening its WebSocket.
	terminalTicketLifetime = 30 * time.Second
	terminalInputLimit     = 1 << 16
	terminalWriteTimeout   = 30 * time.Second
)

// terminalTicket lets one browser open one WebSocket, which cannot carry an
// Authorization header. A token in a URL would end up in logs; a ticket that is
// short-lived and works once is worthless by then.
type terminalTicket struct {
	request streams.TerminalRequest
	actor   audit.Actor
	label   string
	expires time.Time
}

// terminalTickets are kept in memory: a panel restart ends every terminal anyway.
type terminalTickets struct {
	mu      sync.Mutex
	tickets map[string]*terminalTicket
}

func (t *terminalTickets) issue(ticket *terminalTicket) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw)

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.tickets == nil {
		t.tickets = make(map[string]*terminalTicket)
	}

	now := time.Now()
	for key, existing := range t.tickets {
		if now.After(existing.expires) {
			delete(t.tickets, key)
		}
	}

	t.tickets[id] = ticket

	return id, nil
}

// take returns a ticket once, if it is still valid.
func (t *terminalTickets) take(id string) *terminalTicket {
	t.mu.Lock()
	defer t.mu.Unlock()

	ticket := t.tickets[id]
	if ticket == nil {
		return nil
	}

	delete(t.tickets, id)

	if time.Now().After(ticket.expires) {
		return nil
	}

	return ticket
}

func (p *Panel) bindTerminalRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	group.POST("/hosts/{id}/terminal", p.handleTerminalTicket).BindFunc(requireRole(schema.RoleAdmin))
}

// handleTerminalTicket checks who may open what, and hands out a ticket for it.
func (p *Panel) handleTerminalTicket(e *core.RequestEvent) error {
	host, err := p.App.FindRecordById(schema.Hosts, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The server does not exist.", err)
	}

	var body struct {
		Container string   `json:"container"`
		HostShell bool     `json:"host_shell"`
		Command   []string `json:"command"`
		User      string   `json:"user"`
		Cols      uint32   `json:"cols"`
		Rows      uint32   `json:"rows"`
	}
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}

	if !p.Commands.Connected(host.Id) {
		return e.Error(http.StatusConflict, "The server is not connected.", nil)
	}

	label := host.GetString("name")
	details := map[string]any{"host": host.Id, "host_name": host.GetString("name")}

	switch {
	case body.HostShell:
		if body.Container != "" {
			return e.BadRequestError("A terminal is for a container or for the host, not for both.", nil)
		}
		if !host.GetBool("host_shell_enabled") {
			return e.BadRequestError("Host shells are disabled on this server. Start its agent with HOST_SHELL_ENABLED=1 to allow them.", nil)
		}
		details["host_shell"] = true

	case body.Container != "":
		container, err := p.App.FindFirstRecordByFilter(schema.Containers, "host={:host} && docker_id={:id}",
			dbx.Params{"host": host.Id, "id": body.Container})
		if err != nil {
			return e.NotFoundError("This server has no such container.", err)
		}
		label = host.GetString("name") + " / " + container.GetString("name")
		details["container"] = container.GetString("name")
		details["container_id"] = body.Container

	default:
		return e.BadRequestError("Name a container.", nil)
	}

	userID, requesterLabel := requester(e)

	ticket, err := p.terminalTickets.issue(&terminalTicket{
		request: streams.TerminalRequest{
			Host:      host.Id,
			Container: body.Container,
			HostShell: body.HostShell,
			Command:   body.Command,
			User:      body.User,
			Cols:      body.Cols,
			Rows:      body.Rows,
			Label:     label,
			UserID:    userID,
			Requester: requesterLabel,
		},
		actor:   audit.ActorFromRequest(e),
		label:   label,
		expires: time.Now().Add(terminalTicketLifetime),
	})
	if err != nil {
		return e.InternalServerError("Failed to create a ticket.", err)
	}

	return e.JSON(http.StatusOK, map[string]any{"ticket": ticket, "expires_in": int(terminalTicketLifetime.Seconds())})
}

// terminalMessage is what travels as text on the WebSocket; bytes of the terminal
// itself are binary messages.
type terminalMessage struct {
	Type  string `json:"type"`
	Cols  uint32 `json:"cols,omitempty"`
	Rows  uint32 `json:"rows,omitempty"`
	Code  int32  `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// handleTerminalSocket joins a browser's WebSocket with an agent's Exec stream. The
// ticket is the authentication: it was issued to an admin moments ago.
func (p *Panel) handleTerminalSocket(e *core.RequestEvent) error {
	ticket := p.terminalTickets.take(e.Request.URL.Query().Get("ticket"))
	if ticket == nil {
		return e.UnauthorizedError("This ticket is not valid, or has been used.", nil)
	}

	conn, err := websocket.Accept(e.Response, e.Request, &websocket.AcceptOptions{OriginPatterns: p.terminalOrigins()})
	if err != nil {
		return nil //nolint:nilerr // Accept has answered the browser
	}
	defer conn.CloseNow() //nolint:errcheck // whatever is left of the connection

	conn.SetReadLimit(terminalInputLimit)

	session, err := p.Streams.OpenTerminal(ticket.request)
	if err != nil {
		p.Log.Error("failed to open a terminal", "host", ticket.request.Host, "error", err)
		return conn.Close(websocket.StatusInternalError, "The terminal could not be opened.")
	}
	defer session.Close()

	details := map[string]any{"session": session.ID(), "host": ticket.request.Host, "host_shell": ticket.request.HostShell}
	if ticket.request.Container != "" {
		details["container"] = ticket.request.Container
	}

	// who, when and where, and nothing of what is typed
	p.Audit.Security(audit.ActionTerminalSession, ticket.actor, ticket.label, details)
	events.Log(p.App, ticket.request.Host, "terminal.opened", schema.SeverityInfo,
		"A terminal was opened by "+ticket.actor.Label, map[string]any{"user": ticket.actor.Label, "target": ticket.label})

	started := time.Now()
	defer func() {
		ended := map[string]any{"session": session.ID(), "host": ticket.request.Host, "duration_s": int(time.Since(started).Seconds())}
		p.Audit.Security(audit.ActionTerminalSessionEnd, ticket.actor, ticket.label, ended)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go p.readTerminalInput(ctx, cancel, conn, session)

	return p.writeTerminalOutput(ctx, conn, session)
}

func (p *Panel) readTerminalInput(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, session *streams.Terminal) {
	defer cancel()

	for {
		kind, data, err := conn.Read(ctx)
		if err != nil {
			return
		}

		if kind == websocket.MessageBinary {
			session.Input(data)
			continue
		}

		var message terminalMessage
		if err := json.Unmarshal(data, &message); err != nil {
			continue
		}
		if message.Type == "resize" && message.Cols > 0 && message.Rows > 0 {
			session.Resize(message.Cols, message.Rows)
		}
	}
}

func (p *Panel) writeTerminalOutput(ctx context.Context, conn *websocket.Conn, session *streams.Terminal) error {
	idle := p.TerminalIdle
	if idle == 0 {
		idle = streams.DefaultTerminalIdle
	}

	idleTimer := time.NewTimer(idle)
	defer idleTimer.Stop()

	attachTimer := time.NewTimer(streams.AttachTimeout())
	defer attachTimer.Stop()

	attached := session.Attached()

	send := func(message terminalMessage) error {
		raw, err := json.Marshal(message)
		if err != nil {
			return err
		}
		writeCtx, cancel := context.WithTimeout(ctx, terminalWriteTimeout)
		defer cancel()
		return conn.Write(writeCtx, websocket.MessageText, raw)
	}

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-attached:
			attached = nil
			attachTimer.Stop()
			if err := send(terminalMessage{Type: "open"}); err != nil {
				return nil //nolint:nilerr // the browser left
			}

		case <-attachTimer.C:
			if attached == nil {
				continue
			}
			if err := send(terminalMessage{Type: "error", Error: "The agent did not open the terminal in time."}); err != nil {
				return nil //nolint:nilerr // the browser left
			}
			return conn.Close(websocket.StatusTryAgainLater, "no answer from the agent")

		case <-session.Activity():
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(idle)

		case <-idleTimer.C:
			if err := send(terminalMessage{Type: "idle"}); err != nil {
				return nil //nolint:nilerr // the browser left
			}
			return conn.Close(websocket.StatusNormalClosure, "closed after a long silence")

		case event := <-session.Events():
			if len(event.Output) > 0 {
				writeCtx, cancel := context.WithTimeout(ctx, terminalWriteTimeout)
				err := conn.Write(writeCtx, websocket.MessageBinary, event.Output)
				cancel()
				if err != nil {
					return nil //nolint:nilerr // the browser left
				}
			}
			if event.Exited {
				if err := send(terminalMessage{Type: "exit", Code: event.Code, Error: event.Error}); err != nil {
					return nil //nolint:nilerr // the browser left
				}
				return conn.Close(websocket.StatusNormalClosure, "the session has ended")
			}

		case <-session.Done():
			// what was written last may still be on its way
			for {
				select {
				case event := <-session.Events():
					if len(event.Output) > 0 {
						writeCtx, cancel := context.WithTimeout(ctx, terminalWriteTimeout)
						err := conn.Write(writeCtx, websocket.MessageBinary, event.Output)
						cancel()
						if err != nil {
							return nil //nolint:nilerr // the browser left
						}
					}
					if event.Exited {
						if err := send(terminalMessage{Type: "exit", Code: event.Code, Error: event.Error}); err != nil {
							return nil //nolint:nilerr // the browser left
						}
						return conn.Close(websocket.StatusNormalClosure, "the session has ended")
					}
				default:
					return closeTerminal(conn, session, send)
				}
			}
		}
	}
}

func closeTerminal(conn *websocket.Conn, session *streams.Terminal, send func(terminalMessage) error) error {
	if reason := session.Reason(); reason != "" {
		if err := send(terminalMessage{Type: "error", Error: reason}); err != nil && !errors.Is(err, context.Canceled) {
			return nil //nolint:nilerr // the browser left
		}
	}
	return conn.Close(websocket.StatusNormalClosure, "the session has ended")
}

// terminalOrigins says from where a browser may open a terminal: the panel's own
// address, and a Vite dev server next to it. Without this list the origin must
// equal the Host header, which a reverse proxy may change.
func (p *Panel) terminalOrigins() []string {
	origins := []string{"localhost:*", "127.0.0.1:*"}
	if parsed, err := url.Parse(p.Config.PublicURL); err == nil && parsed.Host != "" {
		origins = append(origins, parsed.Host)
	}
	return origins
}
