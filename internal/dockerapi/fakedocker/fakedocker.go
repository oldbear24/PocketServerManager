// Package fakedocker is an in-memory Docker Engine, for tests and for working
// without Docker. It speaks the small part of the Engine API that dockerapi uses,
// over a real HTTP connection.
package fakedocker

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Container is a fake container.
type Container struct {
	ID      string
	Name    string
	Image   string
	Status  string
	Health  string
	Labels  map[string]string
	Config  map[string]any
	Host    map[string]any
	Started time.Time
	Restart int32

	// CPUPercent and MemBytes are what the statistics report.
	CPUPercent float64
	MemBytes   uint64
	cpuTotal   uint64
	logs       *logBuffer
}

// Engine is the fake. All fields are guarded by mu; use the methods from tests.
type Engine struct {
	Server *httptest.Server

	mu         sync.Mutex
	containers map[string]*Container
	images     map[string]map[string]any
	volumes    map[string]map[string]string
	networks   map[string]*Network
	calls      []string
	events     map[chan map[string]any]struct{}
	systemCPU  uint64
	execs      map[string]*ExecCall
	execList   []*ExecCall

	// OnExec answers a process started without a terminal.
	OnExec func(call *ExecCall, stdin []byte) ExecResult
	// FailOn makes requests fail whose "METHOD path" contains the key.
	FailOn map[string]string
	// BlockOn makes requests whose "METHOD path" contains the key wait on the channel.
	BlockOn map[string]chan struct{}
	// OnStart decides what a started container turns into: status and health.
	OnStart func(c *Container) (status, health string)
	// OnPull is called for each pulled image.
	OnPull func(image string)
}

func newEngine() *Engine {
	return &Engine{
		containers: make(map[string]*Container),
		images:     make(map[string]map[string]any),
		volumes:    make(map[string]map[string]string),
		networks:   map[string]*Network{"bridge": {Name: "bridge", Driver: "bridge", Members: map[string][]string{}}},
		events:     make(map[chan map[string]any]struct{}),
		execs:      make(map[string]*ExecCall),
		FailOn:     make(map[string]string),
		BlockOn:    make(map[string]chan struct{}),
	}
}

// New starts a fake engine for a test, on a free port.
func New(t testing.TB) *Engine {
	t.Helper()

	engine := newEngine()
	engine.Server = httptest.NewServer(http.HandlerFunc(engine.serve))
	t.Cleanup(engine.Close)

	return engine
}

// Listen starts a fake engine on a fixed address.
func Listen(addr string) (*Engine, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	engine := newEngine()
	engine.Server = &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: http.HandlerFunc(engine.serve), ReadHeaderTimeout: 5 * time.Second},
	}
	engine.Server.Start()

	return engine, nil
}

// Close stops the engine.
func (e *Engine) Close() {
	e.closeEventStreams()
	e.Server.CloseClientConnections()
	e.Server.Close()
}

// Host returns the value for a DOCKER_HOST setting.
func (e *Engine) Host() string {
	return "tcp://" + strings.TrimPrefix(e.Server.URL, "http://")
}

func newID() string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw)
}

// AddContainer creates a running container and returns its id.
func (e *Engine) AddContainer(name, image string, labels map[string]string) string {
	e.mu.Lock()
	defer e.mu.Unlock()

	id := newID()
	e.containers[id] = &Container{
		ID:      id,
		Name:    name,
		Image:   image,
		Status:  "running",
		Labels:  labels,
		Started: time.Now().UTC(),
		Config: map[string]any{
			"Image":    image,
			"Hostname": id[:12],
			"Labels":   toAny(labels),
			"Env":      []string{"PATH=/usr/bin"},
		},
		Host: map[string]any{"NetworkMode": "bridge", "RestartPolicy": map[string]any{"Name": "unless-stopped"}},
	}

	e.broadcast("container", "start", id, nil)

	return id
}

// SetHostConfig replaces the host config of a container with a name.
func (e *Engine) SetHostConfig(name string, config map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, container := range e.containers {
		if container.Name == name {
			container.Host = config
		}
	}
}

// SetStatus changes a container's status and sends the event.
func (e *Engine) SetStatus(id, status string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if container := e.containers[id]; container != nil {
		container.Status = status
		e.broadcast("container", "die", id, nil)
	}
}

// RemoveContainer removes a container and sends the event.
func (e *Engine) RemoveContainer(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	delete(e.containers, id)
	e.removeFromNetworks(id)
	e.broadcast("container", "destroy", id, nil)
}

// Find returns a copy of the container with a name, or nil.
func (e *Engine) Find(name string) *Container {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, container := range e.containers {
		if container.Name == name {
			copied := *container
			return &copied
		}
	}
	return nil
}

// Names lists the names of all containers.
func (e *Engine) Names() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	names := make([]string, 0, len(e.containers))
	for _, container := range e.containers {
		names = append(names, container.Name)
	}
	return names
}

// SetLoad sets what the statistics of a container report.
func (e *Engine) SetLoad(name string, cpuPercent float64, memBytes uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, container := range e.containers {
		if container.Name == name {
			container.CPUPercent = cpuPercent
			container.MemBytes = memBytes
		}
	}
}

// AddImage registers a local image with its defaults.
func (e *Engine) AddImage(reference string, config map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.images[reference] = config
}

// AddVolume registers a volume.
func (e *Engine) AddVolume(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.volumes[name] = map[string]string{}
}

// Called reports whether a request containing the text was made.
func (e *Engine) Called(text string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, call := range e.calls {
		if strings.Contains(call, text) {
			return true
		}
	}
	return false
}

// Calls returns every request made so far.
func (e *Engine) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string{}, e.calls...)
}

func toAny(labels map[string]string) map[string]any {
	result := make(map[string]any, len(labels))
	for key, value := range labels {
		result[key] = value
	}
	return result
}

func (e *Engine) broadcast(kind, action, id string, attributes map[string]string) {
	event := map[string]any{
		"Type":     kind,
		"Action":   action,
		"Actor":    map[string]any{"ID": id, "Attributes": attributes},
		"timeNano": time.Now().UnixNano(),
	}
	for stream := range e.events {
		select {
		case stream <- event:
		default:
		}
	}
}

func (e *Engine) closeEventStreams() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for stream := range e.events {
		close(stream)
	}
	e.events = make(map[chan map[string]any]struct{})
}

func (e *Engine) find(reference string) *Container {
	if container := e.containers[reference]; container != nil {
		return container
	}
	for _, container := range e.containers {
		if container.Name == reference || strings.HasPrefix(container.ID, reference) && len(reference) >= 12 {
			return container
		}
	}
	return nil
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func (e *Engine) serve(w http.ResponseWriter, r *http.Request) {
	call := r.Method + " " + r.URL.Path

	e.mu.Lock()
	e.calls = append(e.calls, call+"?"+r.URL.RawQuery)
	var failure string
	for key, message := range e.FailOn {
		if strings.Contains(call, key) {
			failure = message
		}
	}
	var block chan struct{}
	for key, channel := range e.BlockOn {
		if strings.Contains(call, key) {
			block = channel
		}
	}
	e.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-r.Context().Done():
			return
		}
	}

	if failure != "" {
		writeError(w, http.StatusInternalServerError, failure)
		return
	}

	path := r.URL.Path

	switch {
	case path == "/_ping":
		_, _ = w.Write([]byte("OK"))
	case path == "/version":
		writeJSON(w, map[string]string{"Version": "27.3.1", "ApiVersion": "1.47", "Os": "linux", "Arch": "amd64"})
	case path == "/info":
		writeJSON(w, map[string]any{
			"Name": "fake-host", "OperatingSystem": "Fake Linux 1.0", "KernelVersion": "6.1.0-fake",
			"Architecture": "x86_64", "NCPU": 4, "MemTotal": 8 << 30, "ServerVersion": "27.3.1",
		})
	case path == "/containers/json":
		e.listContainers(w)
	case path == "/containers/create":
		e.createContainer(w, r)
	case strings.HasPrefix(path, "/containers/"):
		e.containerRoute(w, r, strings.TrimPrefix(path, "/containers/"))
	case strings.HasPrefix(path, "/exec/"):
		e.execRoute(w, r, strings.TrimPrefix(path, "/exec/"))
	case path == "/images/create":
		e.pullImage(w, r)
	case strings.HasPrefix(path, "/images/"):
		e.imageRoute(w, r, strings.TrimPrefix(path, "/images/"))
	case path == "/volumes":
		e.listVolumes(w)
	case path == "/networks":
		e.listNetworks(w)
	case path == "/networks/create":
		e.createNetwork(w, r)
	case strings.HasPrefix(path, "/networks/"):
		e.networkRoute(w, r, strings.TrimPrefix(path, "/networks/"))
	case path == "/events":
		e.streamEvents(w, r)
	default:
		writeError(w, http.StatusNotFound, "unknown route "+path)
	}
}

func (e *Engine) listContainers(w http.ResponseWriter) {
	e.mu.Lock()
	defer e.mu.Unlock()

	list := make([]map[string]any, 0, len(e.containers))
	for _, container := range e.containers {
		list = append(list, map[string]any{
			"Id":     container.ID,
			"Names":  []string{"/" + container.Name},
			"Image":  container.Image,
			"State":  container.Status,
			"Status": "Up 1 minute",
			"Labels": container.Labels,
			"Ports":  []map[string]any{{"PrivatePort": 80, "PublicPort": 8080, "Type": "tcp", "IP": "0.0.0.0"}},
		})
	}
	writeJSON(w, list)
}

func (e *Engine) createContainer(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	name := r.URL.Query().Get("name")

	e.mu.Lock()
	defer e.mu.Unlock()

	for _, existing := range e.containers {
		if existing.Name == name && name != "" {
			writeError(w, http.StatusConflict, fmt.Sprintf("the name %q is already in use", name))
			return
		}
	}

	image, _ := body["Image"].(string)
	if _, known := e.images[image]; !known {
		writeError(w, http.StatusNotFound, "no such image: "+image)
		return
	}

	id := newID()

	host, _ := body["HostConfig"].(map[string]any)
	if name == "" {
		name = "generated-" + id[:6]
	}

	labels := map[string]string{}
	if raw, ok := body["Labels"].(map[string]any); ok {
		for key, value := range raw {
			labels[key], _ = value.(string)
		}
	}

	config := map[string]any{}
	for key, value := range body {
		if key != "HostConfig" && key != "NetworkingConfig" {
			config[key] = value
		}
	}

	e.containers[id] = &Container{ID: id, Name: name, Image: image, Status: "created", Labels: labels, Config: config, Host: host}
	e.broadcast("container", "create", id, nil)

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]string{"Id": id})
}

func (e *Engine) containerRoute(w http.ResponseWriter, r *http.Request, rest string) {
	reference, action, _ := strings.Cut(rest, "/")

	e.mu.Lock()
	defer e.mu.Unlock()

	container := e.find(reference)
	if container == nil {
		writeError(w, http.StatusNotFound, "no such container: "+reference)
		return
	}

	switch {
	case r.Method == http.MethodDelete:
		if container.Status == "running" && r.URL.Query().Get("force") != "true" {
			writeError(w, http.StatusConflict, "the container is running")
			return
		}
		delete(e.containers, container.ID)
		e.removeFromNetworks(container.ID)
		e.broadcast("container", "destroy", container.ID, nil)
		w.WriteHeader(http.StatusNoContent)

	case action == "json":
		writeJSON(w, inspectDocument(container))

	case action == "start":
		status, health := "running", container.Health
		if _, checked := container.Config["Healthcheck"]; checked {
			health = "healthy"
		}
		if e.OnStart != nil {
			status, health = e.OnStart(container)
		}
		container.Status = status
		container.Health = health
		container.Started = time.Now().UTC()
		e.broadcast("container", "start", container.ID, nil)
		w.WriteHeader(http.StatusNoContent)

	case action == "stop":
		container.Status = "exited"
		e.broadcast("container", "die", container.ID, nil)
		w.WriteHeader(http.StatusNoContent)

	case action == "restart":
		container.Status = "running"
		container.Restart++
		e.broadcast("container", "restart", container.ID, nil)
		w.WriteHeader(http.StatusNoContent)

	case action == "rename":
		container.Name = r.URL.Query().Get("name")
		e.broadcast("container", "rename", container.ID, nil)
		w.WriteHeader(http.StatusNoContent)

	case action == "stats":
		// the host's counter moves by 1000 per call, and the container's by its share of that
		e.systemCPU += 1000
		container.cpuTotal += uint64(container.CPUPercent * 10)

		memory := container.MemBytes
		if memory == 0 {
			memory = 150 << 20
		}

		writeJSON(w, map[string]any{
			"cpu_stats":    map[string]any{"cpu_usage": map[string]any{"total_usage": container.cpuTotal}, "system_cpu_usage": e.systemCPU, "online_cpus": 4},
			"memory_stats": map[string]any{"usage": memory + 50<<20, "limit": 1 << 30, "stats": map[string]uint64{"inactive_file": 50 << 20}},
			"networks":     map[string]any{"eth0": map[string]any{"rx_bytes": e.systemCPU, "tx_bytes": 2 * e.systemCPU}},
		})

	case action == "exec":
		e.createExec(w, r, container)

	case action == "logs":
		e.mu.Unlock()
		e.streamLogs(w, r, container)
		e.mu.Lock()

	default:
		writeError(w, http.StatusNotFound, "unknown action "+action)
	}
}

func inspectDocument(c *Container) map[string]any {
	state := map[string]any{
		"Status":  c.Status,
		"Running": c.Status == "running",
		"ExitCode": func() int {
			if c.Status == "exited" {
				return 1
			}
			return 0
		}(),
		"StartedAt": c.Started.Format(time.RFC3339Nano),
	}
	if c.Health != "" {
		state["Health"] = map[string]any{"Status": c.Health}
	}

	return map[string]any{
		"Id":              c.ID,
		"Name":            "/" + c.Name,
		"Image":           "sha256:" + strings.Repeat("a", 64),
		"RestartCount":    c.Restart,
		"State":           state,
		"Config":          c.Config,
		"HostConfig":      c.Host,
		"NetworkSettings": map[string]any{"Networks": map[string]any{"bridge": map[string]any{"Aliases": []string{c.ID[:12], "keep-this-alias"}, "IPAddress": "172.17.0.2"}}},
	}
}

func (e *Engine) pullImage(w http.ResponseWriter, r *http.Request) {
	reference := r.URL.Query().Get("fromImage")
	tag := r.URL.Query().Get("tag")
	if strings.HasPrefix(tag, "sha256:") {
		reference += "@" + tag
	} else if tag != "" {
		reference += ":" + tag
	}

	e.mu.Lock()
	if _, known := e.images[reference]; !known {
		e.images[reference] = map[string]any{"Env": []string{"PATH=/usr/bin"}}
	}
	onPull := e.OnPull
	e.mu.Unlock()

	if onPull != nil {
		onPull(reference)
	}

	writeJSON(w, map[string]string{"status": "Pulled " + reference})
}

func (e *Engine) imageRoute(w http.ResponseWriter, r *http.Request, rest string) {
	reference := strings.TrimSuffix(rest, "/json")

	e.mu.Lock()
	defer e.mu.Unlock()

	config, known := e.images[reference]

	if r.Method == http.MethodDelete {
		delete(e.images, reference)
		w.WriteHeader(http.StatusOK)
		writeJSON(w, []string{})
		return
	}

	if !known {
		writeError(w, http.StatusNotFound, "no such image: "+reference)
		return
	}

	writeJSON(w, map[string]any{"Id": "sha256:" + strings.Repeat("b", 64), "Config": config})
}

func (e *Engine) listVolumes(w http.ResponseWriter) {
	e.mu.Lock()
	defer e.mu.Unlock()

	list := make([]map[string]any, 0, len(e.volumes))
	for name, labels := range e.volumes {
		list = append(list, map[string]any{"Name": name, "Driver": "local", "Labels": labels})
	}
	writeJSON(w, map[string]any{"Volumes": list})
}

func (e *Engine) listNetworks(w http.ResponseWriter) {
	e.mu.Lock()
	defer e.mu.Unlock()

	list := make([]map[string]any, 0, len(e.networks))
	for name, network := range e.networks {
		list = append(list, map[string]any{"Id": "net-" + name, "Name": name, "Driver": network.Driver, "Internal": network.Internal, "Labels": network.Labels})
	}
	writeJSON(w, list)
}

func (e *Engine) streamEvents(w http.ResponseWriter, r *http.Request) {
	stream := make(chan map[string]any, 64)

	e.mu.Lock()
	e.events[stream] = struct{}{}
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		delete(e.events, stream)
		e.mu.Unlock()
	}()

	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	encoder := json.NewEncoder(w)

	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-stream:
			if !ok {
				return
			}
			if err := encoder.Encode(event); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}
}
