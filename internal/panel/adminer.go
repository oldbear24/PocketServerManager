package panel

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/pocketbase/core"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/streams"
)

const (
	// adminerImage is Adminer's official image; its sign-in plugin is written for
	// Adminer 4.
	adminerImage = "adminer:4"
	// AdminerLifetime is how long an Adminer session lasts at most.
	AdminerLifetime       = 30 * time.Minute
	adminerTicketLifetime = time.Minute
	adminerCookie         = "pc_adminer"
	adminerPath           = "/api/pc/adminer/"
	adminerAnswerLimit    = time.Minute
)

// adminerAccess binds an Adminer session to the browser that opened it: a ticket in
// the first URL, then a cookie that only that browser has.
type adminerAccess struct {
	relay  *streams.Relay
	cookie string

	ticket         string
	ticketExpires  time.Time
	ticketConsumed bool

	proxy *httputil.ReverseProxy
}

type adminerSessions struct {
	mu       sync.Mutex
	sessions map[string]*adminerAccess
}

func (s *adminerSessions) add(access *adminerAccess) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = make(map[string]*adminerAccess)
	}
	s.sessions[access.relay.ID()] = access
}

func (s *adminerSessions) get(id string) *adminerAccess {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *adminerSessions) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// AdminerURL is the address of an Adminer session, under the panel.
func AdminerURL(session string) string {
	return adminerPath + session + "/"
}

// handleAdminer starts Adminer for a database, for 30 minutes. It signs in as the
// database's own user, so it reaches nothing else; the credentials go only into
// the Adminer container. The session is a security event in the audit.
func (p *Panel) handleAdminer(e *core.RequestEvent) error {
	database, server, err := p.databaseFor(e)
	if err != nil {
		return err
	}
	if server.GetString("admin_password") == "" {
		return e.BadRequestError("The panel does not know this server's credentials.", nil)
	}
	hostID := server.GetString("host")
	if !p.Commands.Connected(hostID) {
		return e.Error(http.StatusConflict, "The server is not connected.", nil)
	}

	ticket, err := randomToken()
	if err != nil {
		return e.InternalServerError("", err)
	}
	cookie, err := randomToken()
	if err != nil {
		return e.InternalServerError("", err)
	}

	lifetime := p.AdminerTTL
	if lifetime <= 0 {
		lifetime = AdminerLifetime
	}

	relay := p.Streams.OpenRelay(hostID, lifetime)
	access := &adminerAccess{relay: relay, cookie: cookie, ticket: ticket, ticketExpires: time.Now().Add(adminerTicketLifetime)}
	access.proxy = p.adminerProxy(relay)
	p.adminers.add(access)

	go func() {
		<-relay.Done()
		p.adminers.remove(relay.ID())
	}()

	label := server.GetString("name") + "/" + database.GetString("name")
	userID, requesterLabel := requester(e)

	// a dedicated server's databases have users the panel does not know: Adminer
	// signs in as the administrator there
	user := database.GetString("name")
	if server.GetString("kind") == schema.DBServerDedicated {
		user = ""
	}

	_, err = p.Commands.Create(e.Request.Context(), commands.Options{
		Host:        hostID,
		Label:       label,
		RequestedBy: userID,
		Requester:   requesterLabel,
		Command: &agentv1.Command{Payload: &agentv1.Command_DbAdminerStart{DbAdminerStart: &agentv1.DbAdminerStart{
			SessionId:   relay.ID(),
			Network:     "pcadm-" + relay.ID()[:12],
			Image:       adminerImage,
			TtlS:        int32(max(lifetime.Seconds(), 1)),
			Connection:  connectionOf(server),
			ServerAlias: server.GetString("name"),
			Database:    database.GetString("name"),
			User:        user,
		}}},
	})
	if err != nil {
		relay.Close("The session could not be requested.")
		return e.InternalServerError("Failed to start Adminer.", err)
	}

	p.Audit.Security(audit.ActionAdminerSession, audit.ActorFromRequest(e), label, map[string]any{
		"host":       hostID,
		"database":   database.Id,
		"expires_at": relay.Expires().UTC().Format(time.RFC3339),
	})

	return e.JSON(http.StatusOK, map[string]any{
		"url":        AdminerURL(relay.ID()) + "?ticket=" + ticket,
		"expires_at": relay.Expires().UTC().Format(time.RFC3339),
	})
}

// adminerProxy passes a browser's requests to Adminer, each over a connection of
// its own through the agent's relay. The path stays as it is, so that Adminer's
// links, redirects and cookies stay under the session's path.
func (p *Panel) adminerProxy(relay *streams.Relay) *httputil.ReverseProxy {
	prefix := AdminerURL(relay.ID())

	return &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.Out.URL.Scheme = "http"
			request.Out.URL.Host = "adminer"
			request.Out.Host = "adminer"
			request.Out.Header.Del("Authorization")

			// the panel's cookie is none of Adminer's business
			cookies := request.In.Cookies()
			request.Out.Header.Del("Cookie")
			for _, cookie := range cookies {
				if cookie.Name != adminerCookie {
					request.Out.AddCookie(cookie)
				}
			}
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return relay.Dial(ctx)
			},
			// Adminer's PHP server closes every connection after one request anyway
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: adminerAnswerLimit,
		},
		ModifyResponse: func(response *http.Response) error {
			response.Header.Set("Referrer-Policy", "no-referrer")
			response.Header.Set("Cache-Control", "no-store")
			if location := response.Header.Get("Location"); strings.HasPrefix(location, "/") && !strings.HasPrefix(location, prefix) {
				response.Header.Set("Location", strings.TrimSuffix(prefix, "/")+location)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.Log.Debug("an Adminer request failed", "session", relay.ID(), "error", err)
			writeAdminerPage(w, r, relay, http.StatusBadGateway)
		},
	}
}

// writeAdminerPage tells the browser what is going on while Adminer cannot answer:
// it is starting, which takes a few seconds, or the session is over.
func writeAdminerPage(w http.ResponseWriter, r *http.Request, relay *streams.Relay, status int) {
	title, text, refresh := "Adminer is starting", "This page reloads by itself in a moment.", true

	if relay == nil {
		title, text, refresh = "This Adminer session has ended", "Start a new one from the Databases page of the panel.", false
	} else {
		select {
		case <-relay.Done():
			reason := relay.Reason()
			if reason == "" {
				reason = "Start a new one from the Databases page of the panel."
			}
			title, text, refresh = "This Adminer session has ended", reason, false
		default:
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	meta := ""
	if refresh && r.Method == http.MethodGet {
		meta = `<meta http-equiv="refresh" content="2">`
	}
	_, _ = fmt.Fprintf(w, `<!doctype html><meta charset="utf-8">%s<title>%s</title>
<body style="font-family:system-ui,sans-serif;background:#0d0f13;color:#e6e8ec;padding:48px">
<h1 style="font-size:20px">%s</h1><p style="color:#a7afbb">%s</p></body>`, meta, html.EscapeString(title), html.EscapeString(title), html.EscapeString(text))
}

// handleAdminerProxy serves an Adminer session to the browser that opened it.
func (p *Panel) handleAdminerProxy(e *core.RequestEvent) error {
	sessionID := e.Request.PathValue("session")
	access := p.adminers.get(sessionID)
	if access == nil {
		writeAdminerPage(e.Response, e.Request, nil, http.StatusNotFound)
		return nil
	}

	prefix := AdminerURL(sessionID)

	// the first request carries the ticket, which becomes a cookie of this browser
	if ticket := e.Request.URL.Query().Get("ticket"); ticket != "" {
		if !p.adminers.consume(access, ticket) {
			return e.ForbiddenError("This Adminer link has been used or has expired. Start a new session.", nil)
		}
		// Secure whenever the panel is served over HTTPS; a panel on plain HTTP in a
		// LAN could not keep the cookie otherwise
		http.SetCookie(e.Response, &http.Cookie{ //nolint:gosec // see above
			Name:     adminerCookie,
			Value:    access.cookie,
			Path:     prefix,
			Expires:  access.relay.Expires(),
			HttpOnly: true,
			Secure:   e.Request.TLS != nil || strings.HasPrefix(p.Config.PublicURL, "https://"),
			SameSite: http.SameSiteStrictMode,
		})
		return e.Redirect(http.StatusSeeOther, prefix)
	}

	cookie, err := e.Request.Cookie(adminerCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(access.cookie)) != 1 {
		return e.ForbiddenError("This Adminer session belongs to another browser.", nil)
	}

	select {
	case <-access.relay.Done():
		writeAdminerPage(e.Response, e.Request, access.relay, http.StatusGone)
		return nil
	default:
	}

	access.proxy.ServeHTTP(e.Response, e.Request)
	return nil
}

// consume accepts a ticket once, while it is fresh.
func (s *adminerSessions) consume(access *adminerAccess, ticket string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if access.ticketConsumed || time.Now().After(access.ticketExpires) {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(ticket), []byte(access.ticket)) != 1 {
		return false
	}
	access.ticketConsumed = true
	return true
}
