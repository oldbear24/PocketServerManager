// Package fakecaddy is a stand-in for the admin API of the Caddy edge proxy, for
// tests. It listens on a Unix socket like the real edge, keeps the loaded config,
// and refuses configs that Caddy would refuse in the ways the tests need: invalid
// JSON, unknown apps and unknown handler modules. It does not serve traffic; the
// real Caddy is checked on a real Docker host.
package fakecaddy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"
)

// Server is a fake Caddy admin endpoint.
type Server struct {
	socket   string
	autosave string

	listener net.Listener
	server   *http.Server

	mu       sync.Mutex
	config   json.RawMessage
	loads    []json.RawMessage
	rejected []string

	// Reject refuses a config for a reason of the test's own, after the checks
	// the fake makes itself.
	Reject func(config map[string]any) error
	// Delay holds every /load for a while, like a reload of a large config.
	Delay time.Duration

	requests float64
	byHost   map[string]float64
}

// Start listens on a Unix socket. With an autosave path the fake resumes from it
// and saves every loaded config there, like `caddy run --resume`.
func Start(socket, autosave string) (*Server, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		return nil, err
	}
	_ = os.Remove(socket)

	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}

	s := &Server{socket: socket, autosave: autosave, listener: listener}

	if autosave != "" {
		if saved, err := os.ReadFile(autosave); err == nil && json.Valid(saved) {
			s.config = saved
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /load", s.load)
	mux.HandleFunc("GET /config/", s.getConfig)
	mux.HandleFunc("GET /metrics", s.metrics)

	s.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.server.Serve(listener) }()

	return s, nil
}

// Close stops listening and removes the socket, as a stopped edge does.
func (s *Server) Close() error {
	err := s.server.Close()
	_ = os.Remove(s.socket)
	return err
}

// Config returns the active config, or nil before the first load.
func (s *Server) Config() json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.config)
}

// Loads returns every accepted config, oldest first.
func (s *Server) Loads() []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.loads)
}

// Rejected returns the errors of the refused configs, oldest first.
func (s *Server) Rejected() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.rejected)
}

func (s *Server) load(w http.ResponseWriter, r *http.Request) {
	if s.Delay > 0 {
		select {
		case <-time.After(s.Delay):
		case <-r.Context().Done():
			return
		}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		s.refuse(w, err)
		return
	}

	if err := s.check(body); err != nil {
		s.refuse(w, err)
		return
	}

	s.mu.Lock()
	s.config = body
	s.loads = append(s.loads, body)
	s.mu.Unlock()

	if s.autosave != "" {
		if err := os.MkdirAll(filepath.Dir(s.autosave), 0o755); err == nil {
			_ = os.WriteFile(s.autosave, body, 0o600)
		}
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) refuse(w http.ResponseWriter, err error) {
	s.mu.Lock()
	s.rejected = append(s.rejected, err.Error())
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func (s *Server) getConfig(w http.ResponseWriter, _ *http.Request) {
	config := s.Config()
	if config == nil {
		config = json.RawMessage("null")
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(config)
	_, _ = w.Write([]byte("\n"))
}

// apps are the Caddy apps the fake knows.
var apps = []string{"http", "tls"}

// handlers are the HTTP handler modules of a standard Caddy build that the
// panel may use.
var handlers = []string{
	"encode", "error", "file_server", "headers", "map", "reverse_proxy", "rewrite",
	"static_response", "subroute", "vars", "authentication", "request_body",
}

func (s *Server) check(body []byte) error {
	var config map[string]any
	if err := json.Unmarshal(body, &config); err != nil {
		return fmt.Errorf("decoding request body: %w", err)
	}

	if appsField, ok := config["apps"].(map[string]any); ok {
		names := make([]string, 0, len(appsField))
		for name := range appsField {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if !slices.Contains(apps, name) {
				return fmt.Errorf("loading config: loading new config: loading app module: module not registered: %s", name)
			}
		}
	}

	if err := checkHandlers(config); err != nil {
		return fmt.Errorf("loading config: loading new config: loading http app module: %w", err)
	}

	if s.Reject != nil {
		if err := s.Reject(config); err != nil {
			return fmt.Errorf("loading config: loading new config: %w", err)
		}
	}

	return nil
}

// checkHandlers finds every "handler" key, wherever it is nested.
func checkHandlers(value any) error {
	switch v := value.(type) {
	case map[string]any:
		if name, ok := v["handler"].(string); ok && !slices.Contains(handlers, name) {
			return errors.New("loading module '" + name + "': unknown module: http.handlers." + name)
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if err := checkHandlers(v[key]); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range v {
			if err := checkHandlers(item); err != nil {
				return err
			}
		}
	}
	return nil
}

// Serve counts requests the edge served, as Caddy's metrics would show them.
func (s *Server) Serve(count float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests += count
}

// ServeHost counts requests a site's file server answered for a hostname, as
// Caddy does with per_host metrics.
func (s *Server) ServeHost(host string, count float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byHost == nil {
		s.byHost = map[string]float64{}
	}
	s.byHost[host] += count
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	requests := s.requests
	byHost := maps.Clone(s.byHost)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	// as Caddy 2.11 counts: once per request, under the first handler of its
	// route, with the hostname of a route or _other
	_, _ = fmt.Fprintf(w, "# TYPE caddy_http_requests_total counter\n")
	_, _ = fmt.Fprintf(w, "caddy_http_requests_total{handler=\"headers\",host=\"_other\",server=\"http\"} %g\n", requests)
	for _, host := range slices.Sorted(maps.Keys(byHost)) {
		_, _ = fmt.Fprintf(w, "caddy_http_requests_total{handler=\"encode\",host=%q,server=\"https\"} %g\n", host, byHost[host])
	}
}
