// Package fakedocker is an in-memory Docker Engine, for tests and for working
// without Docker. It speaks the small part of the Engine API that dockerapi uses,
// over a real HTTP connection.
package fakedocker

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	// Signals are those sent with kill, in order.
	Signals []string

	// CPUPercent and MemBytes are what the statistics report.
	CPUPercent float64
	MemBytes   uint64
	cpuTotal   uint64
	logs       *logBuffer
	files      map[string][]byte
	// a job's end, and its exit code (see OnJob)
	exited   chan struct{}
	exitCode int
	// stop is closed once a job is stopped or removed (see Job.Stop)
	stop chan struct{}
	// the standard input of a container created with OpenStdin, which an
	// attached client writes to
	stdinR    *io.PipeReader
	stdinW    *io.PipeWriter
	stdinOnce bool
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

	networkCount int

	// OnExec answers a process started without a terminal.
	OnExec func(call *ExecCall, stdin []byte) ExecResult
	// FailOn makes requests fail whose "METHOD path" contains the key.
	FailOn map[string]string
	// BlockOn makes requests whose "METHOD path" contains the key wait on the channel.
	BlockOn map[string]chan struct{}
	// OnStart decides what a started container turns into: status and health.
	// It runs with the engine locked, so it must not call the engine.
	OnStart func(c *Container) (status, health string)
	// OnStop is told about a running container that stops or is removed. It
	// runs with the engine locked too.
	OnStop func(c *Container)
	// OnPull is called for each pulled image.
	OnPull func(image string)
	// OnJob, when set, runs what a started job container (one labelled
	// pocketcloud.role=job) does, as the Engine would run its command: the
	// container runs until OnJob returns, then exits with its code, and its
	// output is the container's log. It runs without the engine locked.
	OnJob func(job Job) JobResult
	// APIVersion is the Engine API version the fake reports; empty is 1.47.
	APIVersion string
	// KernelVersion is the kernel release the fake's info reports; empty is
	// 6.1.0-fake.
	KernelVersion string
	// VolumeRoot is where volumes say their directories are on the host.
	VolumeRoot string
}

// Job is a started job container, as OnJob sees it.
type Job struct {
	Name   string
	Labels map[string]string
	// Cmd and Env are the container's command and environment.
	Cmd []string
	Env []string
	// Host is the container's HostConfig.
	Host map[string]any
	// Files are those copied into the container before it started.
	Files map[string][]byte
	// Stdin is what a client attached to a job created with OpenStdin
	// writes, which ends when it leaves one created with StdinOnce; nil for
	// a job without OpenStdin.
	Stdin io.Reader
	// Stop is closed when the job's container is stopped or removed, as
	// the Engine signals its process: a stop waits for the job to end, up
	// to its timeout, as the Engine's does.
	Stop <-chan struct{}
	// Stdout and Stderr reach the container's log line by line as OnJob
	// writes them, as a running process's output does; JobResult's output
	// follows what they took.
	Stdout, Stderr io.Writer
}

// JobResult is how a job ended.
type JobResult struct {
	Code           int
	Stdout, Stderr string
}

func newEngine() *Engine {
	return &Engine{
		containers: make(map[string]*Container),
		images:     make(map[string]map[string]any),
		volumes:    make(map[string]map[string]string),
		networks:   map[string]*Network{"bridge": {Name: "bridge", Driver: "bridge", Members: map[string][]string{}, Addresses: map[string]string{}}},
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

	e.broadcast("container", "start", id, map[string]string{"name": name})

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
		e.stopping(container)
		container.Status = status
		e.broadcast("container", "die", id, nil)
	}
}

// stopping tells OnStop about a running container that stops.
func (e *Engine) stopping(container *Container) {
	if e.OnStop != nil && container.Status == "running" {
		e.OnStop(container)
	}
}

// RemoveContainer removes a container and sends the event.
func (e *Engine) RemoveContainer(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if container := e.containers[id]; container != nil {
		e.stopping(container)
		closeStdin(container)
		stopJob(container)
	}
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

// SetLoad sets what the statistics of a container report: cpuPercent is its
// share of the whole host, which has four CPUs.
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
		e.mu.Lock()
		api := e.APIVersion
		e.mu.Unlock()
		if api == "" {
			api = "1.47"
		}
		writeJSON(w, map[string]string{"Version": "27.3.1", "ApiVersion": api, "Os": "linux", "Arch": "amd64"})
	case path == "/info":
		e.mu.Lock()
		kernel := e.KernelVersion
		e.mu.Unlock()
		if kernel == "" {
			kernel = "6.1.0-fake"
		}
		writeJSON(w, map[string]any{
			"Name": "fake-host", "OperatingSystem": "Fake Linux 1.0", "KernelVersion": kernel,
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
	case strings.HasPrefix(path, "/volumes/") && r.Method == http.MethodGet:
		e.inspectVolume(w, strings.TrimPrefix(path, "/volumes/"))
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
	omitEmptyMountFields(host)
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

	container := &Container{ID: id, Name: name, Image: image, Status: "created", Labels: labels, Config: config, Host: host}
	if open, _ := body["OpenStdin"].(bool); open {
		container.stdinR, container.stdinW = io.Pipe()
		container.stdinOnce, _ = body["StdinOnce"].(bool)
	}
	e.containers[id] = container

	// a container created on a network of its own starts out on it
	if mode, _ := host["NetworkMode"].(string); mode != "" {
		if network := e.networks[mode]; network != nil {
			network.join(id, nil)
		}
	}

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
		e.stopping(container)
		closeStdin(container)
		stopJob(container)
		delete(e.containers, container.ID)
		e.removeFromNetworks(container.ID)
		e.broadcast("container", "destroy", container.ID, nil)
		w.WriteHeader(http.StatusNoContent)

	case action == "json":
		writeJSON(w, e.inspectDocument(container))

	case action == "archive" && r.Method == http.MethodPut:
		e.receiveArchive(w, r, container)

	case action == "attach" && r.Method == http.MethodPost:
		stdin, once := container.stdinW, container.stdinOnce
		e.mu.Unlock()
		attach(w, r, stdin, once)
		e.mu.Lock()

	case action == "start" && e.OnJob != nil && container.Labels["pocketcloud.role"] == "job":
		container.Status = "running"
		container.Started = time.Now().UTC()
		container.exited = make(chan struct{})
		container.stop = make(chan struct{})
		if container.logs == nil {
			container.logs = &logBuffer{}
		}
		e.broadcast("container", "start", container.ID, map[string]string{"name": container.Name})
		go e.runJob(container, jobOf(container))
		w.WriteHeader(http.StatusNoContent)

	case action == "wait" && r.Method == http.MethodPost:
		e.waitFor(w, r, container)

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
		e.broadcast("container", "start", container.ID, map[string]string{"name": container.Name})
		w.WriteHeader(http.StatusNoContent)

	case action == "stop":
		if exited := container.exited; exited != nil && container.Status == "running" {
			// a job's process gets the signal, and the time to end
			stopJob(container)
			grace := 10 * time.Second
			if seconds, err := strconv.Atoi(r.URL.Query().Get("t")); err == nil && seconds > 0 {
				grace = time.Duration(seconds) * time.Second
			}
			e.mu.Unlock()
			select {
			case <-exited:
			case <-time.After(grace):
			case <-r.Context().Done():
			}
			e.mu.Lock()
		}
		e.stopping(container)
		container.Status = "exited"
		e.broadcast("container", "die", container.ID, nil)
		w.WriteHeader(http.StatusNoContent)

	case action == "kill" && r.Method == http.MethodPost:
		if container.Status != "running" {
			writeError(w, http.StatusConflict, "container "+container.ID+" is not running")
			return
		}
		container.Signals = append(container.Signals, r.URL.Query().Get("signal"))
		w.WriteHeader(http.StatusNoContent)

	case action == "restart":
		container.Status = "running"
		container.Restart++
		e.broadcast("container", "restart", container.ID, nil)
		w.WriteHeader(http.StatusNoContent)

	case action == "update" && r.Method == http.MethodPost:
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if container.Host == nil {
			container.Host = map[string]any{}
		}
		if policy, ok := body["RestartPolicy"]; ok {
			container.Host["RestartPolicy"] = policy
		}
		writeJSON(w, map[string]any{"Warnings": []string{}})

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

func (e *Engine) inspectDocument(c *Container) map[string]any {
	state := map[string]any{
		"Status":     c.Status,
		"Running":    c.Status == "running",
		"Restarting": c.Status == "restarting",
		"ExitCode": func() int {
			if c.exited != nil {
				return c.exitCode
			}
			if c.Status == "exited" || c.Status == "restarting" {
				return 1
			}
			return 0
		}(),
		"StartedAt": c.Started.Format(time.RFC3339Nano),
	}
	if c.Health != "" {
		state["Health"] = map[string]any{"Status": c.Health}
	}

	networks := map[string]any{"bridge": map[string]any{"Aliases": []string{c.ID[:12], "keep-this-alias"}, "IPAddress": "172.17.0.2"}}
	for name, network := range e.networks {
		if aliases, member := network.Members[c.ID]; member {
			networks[name] = map[string]any{"Aliases": aliases, "IPAddress": network.Addresses[c.ID], "NetworkID": "net-" + name}
		}
	}

	return map[string]any{
		"Id":              c.ID,
		"Name":            "/" + c.Name,
		"Image":           "sha256:" + strings.Repeat("a", 64),
		"RestartCount":    c.Restart,
		"State":           state,
		"Config":          c.Config,
		"HostConfig":      c.Host,
		"NetworkSettings": map[string]any{"Networks": networks},
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

func (e *Engine) inspectVolume(w http.ResponseWriter, name string) {
	e.mu.Lock()
	labels, ok := e.volumes[name]
	root := e.VolumeRoot
	e.mu.Unlock()

	if !ok {
		writeError(w, http.StatusNotFound, "get "+name+": no such volume")
		return
	}
	writeJSON(w, map[string]any{"Name": name, "Driver": "local", "Labels": labels, "Mountpoint": volumeDir(root, name)})
}

// VolumeDir is the directory a volume says it has on the host, under
// VolumeRoot, as its inspect reports it.
func (e *Engine) VolumeDir(name string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return volumeDir(e.VolumeRoot, name)
}

func volumeDir(root, name string) string {
	if root == "" {
		root = "/var/lib/docker/volumes"
	}
	return root + "/" + name + "/_data"
}

// Fail makes requests whose "METHOD path" contains key fail with message, from
// now on; an empty message ends it. Unlike FailOn, it is safe while the engine
// serves requests.
func (e *Engine) Fail(key, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if message == "" {
		delete(e.FailOn, key)
		return
	}
	e.FailOn[key] = message
}

// omitEmptyMountFields drops what the Engine leaves out of an inspect when it
// is empty, as ReadOnly: false, so tests see mounts the way a real engine
// reports them.
func omitEmptyMountFields(host map[string]any) {
	mounts, _ := host["Mounts"].([]any)
	for _, item := range mounts {
		mount, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for key, value := range mount {
			if value == false || value == "" || value == nil {
				delete(mount, key)
			}
		}
	}
}

// stopJob tells a job's process to stop, once. It is called with the engine
// locked.
func stopJob(c *Container) {
	if c.stop == nil {
		return
	}
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
}

// jobOf copies what OnJob may see of a container.
func jobOf(c *Container) Job {
	job := Job{Name: c.Name, Labels: maps.Clone(c.Labels), Host: c.Host, Files: maps.Clone(c.files), Stop: c.stop}
	if c.stdinR != nil {
		job.Stdin = c.stdinR
	}
	job.Stdout, job.Stderr = &liveLog{logs: c.logs, stream: 1}, &liveLog{logs: c.logs, stream: 2}
	if cmd, ok := c.Config["Cmd"].([]any); ok {
		for _, arg := range cmd {
			text, _ := arg.(string)
			job.Cmd = append(job.Cmd, text)
		}
	}
	if env, ok := c.Config["Env"].([]any); ok {
		for _, entry := range env {
			text, _ := entry.(string)
			job.Env = append(job.Env, text)
		}
	}
	return job
}

// liveLog writes a running job's output to its log, a line at a time.
type liveLog struct {
	logs    *logBuffer
	stream  byte
	mu      sync.Mutex
	partial []byte
}

func (l *liveLog) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.partial = append(l.partial, data...)
	for {
		end := strings.IndexByte(string(l.partial), '\n')
		if end < 0 {
			return len(data), nil
		}
		l.logs.add(l.stream, string(l.partial[:end]))
		l.partial = l.partial[end+1:]
	}
}

// flush writes an unfinished last line.
func (l *liveLog) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.partial) > 0 {
		l.logs.add(l.stream, string(l.partial))
		l.partial = nil
	}
}

// runJob runs a job container's work and ends it.
func (e *Engine) runJob(c *Container, job Job) {
	result := e.OnJob(job)
	for _, out := range []io.Writer{job.Stdout, job.Stderr} {
		if live, ok := out.(*liveLog); ok {
			live.flush()
		}
	}
	if c.stdinR != nil {
		// what is still written to it has nowhere to go
		c.stdinR.CloseWithError(errors.New("the container has exited"))
	}
	for _, out := range []struct {
		stream byte
		text   string
	}{{1, result.Stdout}, {2, result.Stderr}} {
		for _, line := range strings.Split(strings.TrimSuffix(out.text, "\n"), "\n") {
			if line != "" {
				c.logs.add(out.stream, line)
			}
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c.exitCode = result.Code
	if c.Status == "running" {
		c.Status = "exited"
	}
	close(c.exited)
	c.logs.add(0, "") // wakes the followers, who see the end
	e.broadcast("container", "die", c.ID, nil)
}

// waitFor answers when a container is no longer running, with its exit code.
// It is called with the engine locked.
func (e *Engine) waitFor(w http.ResponseWriter, r *http.Request, c *Container) {
	for c.Status == "running" {
		exited := c.exited
		e.mu.Unlock()
		if exited != nil {
			select {
			case <-exited:
			case <-r.Context().Done():
			}
		} else {
			select {
			case <-time.After(20 * time.Millisecond):
			case <-r.Context().Done():
			}
		}
		e.mu.Lock()
		if r.Context().Err() != nil {
			return
		}
	}
	code := 0
	if c.exited != nil {
		code = c.exitCode
	} else if c.Status == "exited" {
		code = 1
	}
	writeJSON(w, map[string]any{"StatusCode": code})
}
