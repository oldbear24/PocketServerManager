// Package streams relays logs and terminal sessions between agents and browsers.
// What passes through here is never stored: see "Logs and terminal" in
// docs/architecture.md.
package streams

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"

	"github.com/oldbear24/PocketServerManager/internal/commands"
)

// Broker connects the streams of agents with the browsers watching them.
type Broker struct {
	Commands *commands.Manager
	Log      *slog.Logger

	mu sync.Mutex

	logSessions map[string]*logSession
	logSources  map[string]*logSession

	terminals map[string]*terminal

	relays map[string]*Relay
}

// New creates a broker for a command queue. Call Register before the queue starts.
func New(queue *commands.Manager) *Broker {
	return &Broker{
		Commands:    queue,
		Log:         slog.Default().With("component", "streams"),
		logSessions: make(map[string]*logSession),
		logSources:  make(map[string]*logSession),
		terminals:   make(map[string]*terminal),
		relays:      make(map[string]*Relay),
	}
}

// Register tells the broker about commands that failed to open a stream.
func (b *Broker) Register() {
	b.Commands.OnFinish("logs.follow", b.logFollowFinished)
	b.Commands.OnFinish("exec.start", b.execStartFinished)
	b.Commands.OnFinish("db.adminer.start", b.adminerStartFinished)
}

// newSessionID returns a random id. It is what binds an agent's stream to a
// browser session, so it cannot be guessable.
func newSessionID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw)
}
