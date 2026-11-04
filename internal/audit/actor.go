package audit

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

// Actor kinds.
const (
	KindUser      = "user"
	KindToken     = "token"
	KindSuperuser = "superuser"
	KindSystem    = "system"
	KindAnonymous = "anonymous"
)

// Sources of a change.
const (
	SourceUI        = "ui"
	SourceAPI       = "api"
	SourcePBAdmin   = "pb_admin"
	SourceMigration = "migration"
	SourceScheduler = "scheduler"
	SourceAgent     = "agent"
	SourcePanel     = "panel"
	// SourceCommand is a panel command run on the command line.
	SourceCommand = "command"
)

const (
	// HeaderNote carries an optional note for the audit entry of a change.
	HeaderNote = "X-PocketCloud-Note"
	// HeaderClient identifies requests sent by PocketCloud's web UI.
	HeaderClient = "X-PocketCloud-Client"
	// ClientWeb is the value of HeaderClient sent by the web UI.
	ClientWeb = "web"
)

// Actor says who made a change and from where.
type Actor struct {
	Kind      string
	ID        string
	Label     string
	Source    string
	IP        string
	UserAgent string
	Note      string
	// Command links the change to the command it caused.
	Command string
	// Revision links the change to a revision, e.g. "stack r7 → r8".
	Revision string
}

// System returns the actor for a change made by a panel component without a
// request, such as the scheduler, the ACME manager or an agent report.
func System(component, source string) Actor {
	return Actor{Kind: KindSystem, Label: component, Source: source}
}

type actorKey struct{}

// WithActor attributes the changes saved with ctx (app.SaveWithContext) to an actor.
func WithActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

func actorFromContext(ctx context.Context) (Actor, bool) {
	if ctx == nil {
		return Actor{}, false
	}
	actor, ok := ctx.Value(actorKey{}).(Actor)
	return actor, ok
}

// ActorFromRequest derives the actor from an API request.
func ActorFromRequest(e *core.RequestEvent) Actor {
	return actorFromHTTP(e.Request, e.Auth, e.HasSuperuserAuth(), e.RealIP())
}

// decodeNote reads a note header: the UI URL-encodes it, because a header cannot
// hold characters outside ASCII.
func decodeNote(value string) string {
	if decoded, err := url.QueryUnescape(value); err == nil {
		return decoded
	}
	return value
}

func actorFromHTTP(r *http.Request, auth *core.Record, superuser bool, ip string) Actor {
	actor := Actor{
		IP:        ip,
		UserAgent: r.UserAgent(),
		Note:      decodeNote(r.Header.Get(HeaderNote)),
	}

	switch {
	case auth == nil:
		actor.Kind = KindAnonymous
		actor.Source = SourceAPI
	case superuser:
		actor.Kind = KindSuperuser
		actor.ID = auth.Id
		actor.Label = auth.Email()
		actor.Source = SourceAPI
		if strings.Contains(r.Referer(), "/_/") {
			actor.Source = SourcePBAdmin
		}
	default:
		actor.Kind = KindUser
		actor.ID = auth.Id
		actor.Label = auth.GetString("name")
		if actor.Label == "" {
			actor.Label = auth.Email()
		}
		actor.Source = SourceAPI
		if r.Header.Get(HeaderClient) == ClientWeb {
			actor.Source = SourceUI
		}
	}

	return actor
}
