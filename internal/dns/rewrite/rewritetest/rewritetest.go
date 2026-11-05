// Package rewritetest has stand-ins for the APIs of AdGuard Home and Pi-hole
// v6, which answer as the real ones do for what PocketCloud asks of them.
package rewritetest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// AdGuard is AdGuard Home's rewrite API.
type AdGuard struct {
	*httptest.Server
	Username, Password string
	// Forbidden answers wrong credentials with 403, as versions before
	// v0.107.56 do; newer ones answer 401
	Forbidden bool

	mu       sync.Mutex
	rewrites []Rewrite
	// Calls counts the requests, by path.
	calls map[string]int
}

// Rewrite is one rewrite of AdGuard Home.
type Rewrite struct {
	Domain  string `json:"domain"`
	Answer  string `json:"answer"`
	Enabled *bool  `json:"enabled,omitempty"`
}

// NewAdGuard starts AdGuard Home with a username and password.
func NewAdGuard(t testing.TB, username, password string, rewrites ...Rewrite) *AdGuard {
	a := &AdGuard{Username: username, Password: password, rewrites: rewrites, calls: map[string]int{}}
	a.Server = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.Close)
	return a
}

// Rewrites are the rewrites now.
func (a *AdGuard) Rewrites() []Rewrite {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.rewrites)
}

// Calls counts the requests of a path.
func (a *AdGuard) Calls(path string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[path]
}

func (a *AdGuard) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls[r.URL.Path]++
	if user, password, ok := r.BasicAuth(); !ok || user != a.Username || password != a.Password {
		if a.Forbidden {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/control/rewrite/list":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(a.rewrites)
	case r.Method == http.MethodPost && (r.URL.Path == "/control/rewrite/add" || r.URL.Path == "/control/rewrite/delete"):
		var rw Rewrite
		if err := json.NewDecoder(r.Body).Decode(&rw); err != nil || rw.Domain == "" || rw.Answer == "" {
			http.Error(w, "decoding request: bad rewrite", http.StatusBadRequest)
			return
		}
		at := slices.IndexFunc(a.rewrites, func(x Rewrite) bool { return x.Domain == rw.Domain && x.Answer == rw.Answer })
		if r.URL.Path == "/control/rewrite/add" {
			if at >= 0 {
				http.Error(w, "rewrite already exists", http.StatusBadRequest)
				return
			}
			a.rewrites = append(a.rewrites, rw)
			return
		}
		if at < 0 {
			http.Error(w, "rewrite not found", http.StatusBadRequest)
			return
		}
		a.rewrites = slices.Delete(a.rewrites, at, at+1)
	default:
		http.NotFound(w, r)
	}
}

// PiHole is Pi-hole v6's API: sessions, dns.hosts and dns.cnameRecords.
type PiHole struct {
	*httptest.Server
	Password string
	// Seats is how many sessions can be open at once.
	Seats int
	// Restart is how long its DNS server is away after a change of
	// dns.cnameRecords, from right after the answer, as Pi-hole restarts it
	// (a real one goes a moment later); zero for none.
	Restart time.Duration

	mu       sync.Mutex
	sessions map[string]bool
	hosts    []string
	cnames   []string
	started  time.Time
	down     [2]time.Time
	refused  []string
}

// Refused are the requests other than its uptime that came while it
// restarted.
func (p *PiHole) Refused() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.refused)
}

// NewPiHole starts Pi-hole with a password, and entries of dns.hosts.
func NewPiHole(t testing.TB, password string, hosts ...string) *PiHole {
	p := &PiHole{Password: password, Seats: 2, Restart: 50 * time.Millisecond, sessions: map[string]bool{}, hosts: hosts, started: time.Now().Add(-time.Hour)}
	p.Server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.Close)
	return p
}

// Hosts are the entries of dns.hosts now.
func (p *PiHole) Hosts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.hosts)
}

// CNAMEs are the entries of dns.cnameRecords now.
func (p *PiHole) CNAMEs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.cnames)
}

// Open counts the sessions still open.
func (p *PiHole) Open() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sessions)
}

func piError(w http.ResponseWriter, status int, key, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"key": key, "message": message, "hint": nil}})
}

func (p *PiHole) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now := time.Now(); !now.Before(p.down[0]) && now.Before(p.down[1]) {
		// restarting: nothing listens
		if r.URL.Path != "/api/info/ftl" {
			p.refused = append(p.refused, r.Method+" "+r.URL.Path)
		}
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/api/info/ftl" {
		if p.Password != "" && !p.sessions[r.Header.Get("X-FTL-SID")] {
			piError(w, http.StatusUnauthorized, "unauthorized", "Unauthorized")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ftl": map[string]any{"uptime": float64(p.uptime().Milliseconds())}})
		return
	}
	if r.URL.Path == "/api/auth" {
		switch r.Method {
		case http.MethodPost:
			var body struct {
				Password string `json:"password"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if p.Password == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"valid": true, "sid": nil, "message": "no password set"}})
				return
			}
			if body.Password != p.Password {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"valid": false, "sid": nil, "message": "password incorrect"}})
				return
			}
			if len(p.sessions) >= p.Seats {
				piError(w, http.StatusTooManyRequests, "api_seats_exceeded", "API seats exceeded")
				return
			}
			raw := make([]byte, 16)
			_, _ = rand.Read(raw)
			sid := hex.EncodeToString(raw)
			p.sessions[sid] = true
			_ = json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"valid": true, "sid": sid, "csrf": "x", "validity": 1800, "message": "password correct"}})
		case http.MethodDelete:
			delete(p.sessions, r.Header.Get("X-FTL-SID"))
			w.WriteHeader(http.StatusGone)
		}
		return
	}
	if p.Password != "" && !p.sessions[r.Header.Get("X-FTL-SID")] {
		piError(w, http.StatusUnauthorized, "unauthorized", "Unauthorized")
		return
	}
	segments := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/api/config/dns/"), "/")
	if !strings.HasPrefix(r.URL.Path, "/api/config/dns/") || len(segments) == 0 {
		piError(w, http.StatusNotFound, "not_found", "Not found")
		return
	}
	var list *[]string
	switch segments[0] {
	case "hosts":
		list = &p.hosts
	case "cnameRecords":
		list = &p.cnames
	default:
		piError(w, http.StatusNotFound, "not_found", "Not found")
		return
	}
	if len(segments) == 1 && r.Method == http.MethodGet {
		config := map[string]any{segments[0]: *list}
		_ = json.NewEncoder(w).Encode(map[string]any{"config": map[string]any{"dns": config}, "took": 0.001})
		return
	}
	if len(segments) != 2 {
		piError(w, http.StatusBadRequest, "bad_request", "Invalid path")
		return
	}
	item, err := url.PathUnescape(segments[1])
	if err != nil {
		piError(w, http.StatusBadRequest, "bad_request", "Invalid path")
		return
	}
	at := slices.Index(*list, item)
	switch r.Method {
	case http.MethodPut:
		if at >= 0 {
			piError(w, http.StatusBadRequest, "bad_request", "Item already present")
			return
		}
		*list = append(*list, item)
		p.restartFor(segments[0])
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"took": 0.001})
	case http.MethodDelete:
		if at < 0 {
			piError(w, http.StatusNotFound, "not_found", "Item not found")
			return
		}
		*list = slices.Delete(*list, at, at+1)
		p.restartFor(segments[0])
		w.WriteHeader(http.StatusNoContent)
	default:
		piError(w, http.StatusMethodNotAllowed, "bad_request", "Method not allowed")
	}
}

// restartFor restarts the DNS server after a change of dns.cnameRecords, as
// Pi-hole does: away a moment after the answer, then started again.
func (p *PiHole) restartFor(list string) {
	if list != "cnameRecords" || p.Restart == 0 {
		return
	}
	from := time.Now()
	p.down = [2]time.Time{from, from.Add(p.Restart)}
}

// uptime is how long its DNS server has run, since the last restart that
// is over.
func (p *PiHole) uptime() time.Duration {
	if !p.down[1].IsZero() && !time.Now().Before(p.down[1]) {
		p.started, p.down = p.down[1], [2]time.Time{}
	}
	return time.Since(p.started)
}
