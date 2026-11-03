// Package dockerapi is a small client for the Docker Engine API. The official
// client pulls in a large dependency tree, while the updater is meant to stay a
// few megabytes on scratch, and the agent uses only a small part of the API.
package dockerapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultHost is where the Docker socket is mounted in the agent container.
const DefaultHost = "unix:///var/run/docker.sock"

// Client talks to one Docker Engine. Paths carry no API version, so the engine
// uses its own; every field read here is stable across the supported engines.
type Client struct {
	http *http.Client
	base string
}

// New creates a client for a host such as unix:///var/run/docker.sock or tcp://host:2375.
func New(host string) (*Client, error) {
	if host == "" {
		host = DefaultHost
	}

	parsed, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("invalid Docker host %q: %w", host, err)
	}

	transport := &http.Transport{IdleConnTimeout: 90 * time.Second}

	var base string
	switch parsed.Scheme {
	case "unix":
		socket := parsed.Path
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socket)
		}
		base = "http://docker"
	case "tcp", "http":
		base = "http://" + parsed.Host
	default:
		return nil, fmt.Errorf("unsupported Docker host scheme %q", parsed.Scheme)
	}

	return &Client{http: &http.Client{Transport: transport}, base: base}, nil
}

// Error is an error response of the Engine API.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("docker: %s (HTTP %d)", e.Message, e.Status)
}

// IsNotFound reports whether an error means that the object does not exist.
func IsNotFound(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

func (c *Client) request(ctx context.Context, method, path string, query url.Values, body any, headers map[string]string) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = "application/json"
	}

	return c.send(ctx, method, path, query, reader, headers)
}

// requestRaw sends a body that is not JSON, such as an archive.
func (c *Client) requestRaw(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	return c.send(ctx, method, path, query, body, map[string]string{"Content-Type": contentType})
}

func (c *Client) send(ctx context.Context, method, path string, query url.Values, reader io.Reader, headers map[string]string) (*http.Response, error) {
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}

	if res.StatusCode >= 400 {
		defer res.Body.Close()

		raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))

		var message struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &message) != nil || message.Message == "" {
			message.Message = strings.TrimSpace(string(raw))
		}

		return nil, &Error{Status: res.StatusCode, Message: message.Message}
	}

	return res, nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	res, err := c.request(ctx, method, path, query, body, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if out == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}

	return json.NewDecoder(res.Body).Decode(out)
}

// Ping checks that the engine answers.
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/_ping", nil, nil, nil)
}

// VersionInfo is the engine's version.
type VersionInfo struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion"`
	Os         string `json:"Os"`
	Arch       string `json:"Arch"`
}

// Version returns the engine's version.
func (c *Client) Version(ctx context.Context) (VersionInfo, error) {
	var info VersionInfo
	return info, c.do(ctx, http.MethodGet, "/version", nil, nil, &info)
}

// Info is a part of the engine's system information.
type Info struct {
	Name            string `json:"Name"`
	OperatingSystem string `json:"OperatingSystem"`
	KernelVersion   string `json:"KernelVersion"`
	Architecture    string `json:"Architecture"`
	NCPU            int32  `json:"NCPU"`
	MemTotal        uint64 `json:"MemTotal"`
	ServerVersion   string `json:"ServerVersion"`
	Containers      int    `json:"Containers"`
	Images          int    `json:"Images"`
	Driver          string `json:"Driver"`
	DockerRootDir   string `json:"DockerRootDir"`
}

// Info returns the engine's system information.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var info Info
	return info, c.do(ctx, http.MethodGet, "/info", nil, nil, &info)
}

// Port is a port of a listed container.
type Port struct {
	IP          string `json:"IP"`
	PrivatePort uint32 `json:"PrivatePort"`
	PublicPort  uint32 `json:"PublicPort"`
	Type        string `json:"Type"`
}

// Container is one item of the container list.
type Container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Ports  []Port            `json:"Ports"`
	Labels map[string]string `json:"Labels"`
}

// Containers lists every container, running or not.
func (c *Client) Containers(ctx context.Context) ([]Container, error) {
	var list []Container
	return list, c.do(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}}, nil, &list)
}

// ContainerState is the state part of an inspected container.
type ContainerState struct {
	Status     string    `json:"Status"`
	Running    bool      `json:"Running"`
	Restarting bool      `json:"Restarting"`
	Pid        int       `json:"Pid"`
	ExitCode   int       `json:"ExitCode"`
	Error      string    `json:"Error"`
	StartedAt  time.Time `json:"StartedAt"`
	FinishedAt time.Time `json:"FinishedAt"`
	Health     *struct {
		Status string `json:"Status"`
	} `json:"Health"`
}

// ContainerInspect is an inspected container. The config sections stay raw
// documents, so that recreating a container keeps fields this client does not
// know about.
type ContainerInspect struct {
	ID              string         `json:"Id"`
	Name            string         `json:"Name"`
	Image           string         `json:"Image"`
	RestartCount    int32          `json:"RestartCount"`
	State           ContainerState `json:"State"`
	Config          map[string]any `json:"Config"`
	HostConfig      map[string]any `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]map[string]any `json:"Networks"`
	} `json:"NetworkSettings"`
}

// Health returns the health status of the container, or "".
func (i *ContainerInspect) Health() string {
	if i.State.Health == nil {
		return ""
	}
	return i.State.Health.Status
}

// Inspect returns the details of a container.
func (c *Client) Inspect(ctx context.Context, id string) (*ContainerInspect, error) {
	var info ContainerInspect
	if err := c.do(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Start starts a container.
func (c *Client) Start(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
}

// Stop stops a container. A timeout of 0 uses the Docker default.
func (c *Client) Stop(ctx context.Context, id string, timeoutSeconds int) error {
	return c.do(ctx, http.MethodPost, "/containers/"+id+"/stop", timeoutQuery(timeoutSeconds), nil, nil)
}

// Restart restarts a container. A timeout of 0 uses the Docker default.
func (c *Client) Restart(ctx context.Context, id string, timeoutSeconds int) error {
	return c.do(ctx, http.MethodPost, "/containers/"+id+"/restart", timeoutQuery(timeoutSeconds), nil, nil)
}

func timeoutQuery(seconds int) url.Values {
	if seconds <= 0 {
		return nil
	}
	return url.Values{"t": {strconv.Itoa(seconds)}}
}

// Remove removes a container.
func (c *Client) Remove(ctx context.Context, id string, volumes, force bool) error {
	return c.do(ctx, http.MethodDelete, "/containers/"+id, url.Values{
		"v":     {strconv.FormatBool(volumes)},
		"force": {strconv.FormatBool(force)},
	}, nil, nil)
}

// Rename renames a container.
func (c *Client) Rename(ctx context.Context, id, name string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+id+"/rename", url.Values{"name": {name}}, nil, nil)
}

// Create creates a container from a config document and returns its id.
func (c *Client) Create(ctx context.Context, name string, config map[string]any) (string, error) {
	query := url.Values{}
	if name != "" {
		query.Set("name", name)
	}

	var created struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, http.MethodPost, "/containers/create", query, config, &created); err != nil {
		return "", err
	}

	return created.ID, nil
}

// PullAuth holds the credentials of a registry.
type PullAuth struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	ServerAddress string `json:"serveraddress,omitempty"`
}

// Pull pulls an image, calling progress with Docker's status lines.
func (c *Client) Pull(ctx context.Context, image string, auth *PullAuth, progress func(status string)) error {
	repository, tag := splitImage(image)

	query := url.Values{"fromImage": {repository}}
	if tag != "" {
		query.Set("tag", tag)
	}

	headers := map[string]string{}
	if auth != nil {
		raw, err := json.Marshal(auth) //nolint:gosec // this is how the Engine API takes registry credentials
		if err != nil {
			return err
		}
		headers["X-Registry-Auth"] = base64.URLEncoding.EncodeToString(raw)
	}

	res, err := c.request(ctx, http.MethodPost, "/images/create", query, nil, headers)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	// a failure in the middle of a pull only shows up in the stream
	decoder := json.NewDecoder(res.Body)
	for {
		var line struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := decoder.Decode(&line); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if line.Error != "" {
			return errors.New(line.Error)
		}
		if progress != nil && line.Status != "" {
			progress(line.Status)
		}
	}
}

// splitImage separates a reference into what the pull API wants: the digest or
// tag goes into its own parameter.
func splitImage(image string) (repository, tag string) {
	if at := strings.Index(image, "@"); at >= 0 {
		return image[:at], image[at+1:]
	}

	// the last colon only separates a tag if no slash follows it (host:port/name)
	if colon := strings.LastIndex(image, ":"); colon > strings.LastIndex(image, "/") {
		return image[:colon], image[colon+1:]
	}

	return image, ""
}

// ImageInspect is a part of an inspected image.
type ImageInspect struct {
	ID          string   `json:"Id"`
	RepoDigests []string `json:"RepoDigests"`
	Config      struct {
		Env        []string          `json:"Env"`
		Cmd        []string          `json:"Cmd"`
		Entrypoint []string          `json:"Entrypoint"`
		Labels     map[string]string `json:"Labels"`
	} `json:"Config"`
}

// InspectImage returns the details of a local image.
func (c *Client) InspectImage(ctx context.Context, image string) (*ImageInspect, error) {
	var info ImageInspect
	if err := c.do(ctx, http.MethodGet, "/images/"+image+"/json", nil, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// RemoveImage removes a local image.
func (c *Client) RemoveImage(ctx context.Context, image string) error {
	return c.do(ctx, http.MethodDelete, "/images/"+image, nil, nil, nil)
}

// Volume is one volume.
type Volume struct {
	Name   string            `json:"Name"`
	Driver string            `json:"Driver"`
	Labels map[string]string `json:"Labels"`
}

// Volumes lists every volume.
func (c *Client) Volumes(ctx context.Context) ([]Volume, error) {
	var list struct {
		Volumes []Volume `json:"Volumes"`
	}
	return list.Volumes, c.do(ctx, http.MethodGet, "/volumes", nil, nil, &list)
}

// InspectVolume returns one volume.
func (c *Client) InspectVolume(ctx context.Context, name string) (*Volume, error) {
	var volume Volume
	if err := c.do(ctx, http.MethodGet, "/volumes/"+name, nil, nil, &volume); err != nil {
		return nil, err
	}
	return &volume, nil
}

// Network is one network. Containers, keyed by id, is only filled in by
// InspectNetwork.
type Network struct {
	ID         string            `json:"Id"`
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Internal   bool              `json:"Internal"`
	Labels     map[string]string `json:"Labels"`
	Containers map[string]struct {
		Name string `json:"Name"`
	} `json:"Containers"`
}

// Networks lists every network.
func (c *Client) Networks(ctx context.Context) ([]Network, error) {
	var list []Network
	return list, c.do(ctx, http.MethodGet, "/networks", nil, nil, &list)
}

// InspectNetwork returns one network.
func (c *Client) InspectNetwork(ctx context.Context, id string) (*Network, error) {
	var network Network
	if err := c.do(ctx, http.MethodGet, "/networks/"+id, nil, nil, &network); err != nil {
		return nil, err
	}
	return &network, nil
}

// ConnectNetwork attaches a container to a network, with a raw endpoint config.
func (c *Client) ConnectNetwork(ctx context.Context, network, container string, endpoint map[string]any) error {
	return c.do(ctx, http.MethodPost, "/networks/"+network+"/connect", nil, map[string]any{
		"Container":      container,
		"EndpointConfig": endpoint,
	}, nil)
}

// Event is one Docker event.
type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	TimeNano int64 `json:"timeNano"`
}

// Events streams container, volume and network events until ctx ends or the
// connection breaks.
func (c *Client) Events(ctx context.Context, handle func(Event)) error {
	filters, err := json.Marshal(map[string][]string{"type": {"container", "volume", "network"}})
	if err != nil {
		return err
	}

	res, err := c.request(ctx, http.MethodGet, "/events", url.Values{"filters": {string(filters)}}, nil, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	decoder := json.NewDecoder(res.Body)
	for {
		var event Event
		if err := decoder.Decode(&event); err != nil {
			return err
		}
		handle(event)
	}
}

// LogOptions selects what Logs returns.
type LogOptions struct {
	Tail       int
	Follow     bool
	Timestamps bool
}

// Logs opens the log stream of a container. For a container without a TTY the
// stream is multiplexed; see ReadMultiplexed.
func (c *Client) Logs(ctx context.Context, id string, options LogOptions) (io.ReadCloser, error) {
	query := url.Values{
		"stdout":     {"1"},
		"stderr":     {"1"},
		"follow":     {strconv.FormatBool(options.Follow)},
		"timestamps": {strconv.FormatBool(options.Timestamps)},
	}
	if options.Tail >= 0 {
		query.Set("tail", strconv.Itoa(options.Tail))
	}

	res, err := c.request(ctx, http.MethodGet, "/containers/"+id+"/logs", query, nil, nil)
	if err != nil {
		return nil, err
	}

	return res.Body, nil
}

// ReadMultiplexed splits Docker's multiplexed stream into frames.
// stream is 1 for stdout and 2 for stderr.
func ReadMultiplexed(reader io.Reader, frame func(stream byte, data []byte)) error {
	buffered := bufio.NewReader(reader)
	header := make([]byte, 8)

	for {
		if _, err := io.ReadFull(buffered, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}

		size := uint32(header[4])<<24 | uint32(header[5])<<16 | uint32(header[6])<<8 | uint32(header[7])
		data := make([]byte, size)
		if _, err := io.ReadFull(buffered, data); err != nil {
			return err
		}

		frame(header[0], data)
	}
}

// Stats is a part of a container's resource statistics.
type Stats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs     int    `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
}

// ContainerStats returns one immediate sample of a container's statistics. CPU
// usage needs two samples: one-shot avoids the engine's wait for a second one, and
// the caller computes the difference between its own samples.
func (c *Client) ContainerStats(ctx context.Context, id string) (*Stats, error) {
	var stats Stats
	err := c.do(ctx, http.MethodGet, "/containers/"+id+"/stats", url.Values{"stream": {"false"}, "one-shot": {"true"}}, nil, &stats)
	if err != nil {
		return nil, err
	}
	return &stats, nil
}

// ExecOptions describes a process to run inside a container.
type ExecOptions struct {
	Cmd  []string
	User string
	Env  []string
	TTY  bool
	// Stdin attaches the process's standard input.
	Stdin bool
}

// ExecCreate prepares a process inside a container and returns the exec id.
func (c *Client) ExecCreate(ctx context.Context, container string, options ExecOptions) (string, error) {
	var created struct {
		ID string `json:"Id"`
	}
	err := c.do(ctx, http.MethodPost, "/containers/"+container+"/exec", nil, map[string]any{
		"AttachStdin":  options.Stdin,
		"AttachStdout": true,
		"AttachStderr": true,
		"Tty":          options.TTY,
		"Cmd":          options.Cmd,
		"User":         options.User,
		"Env":          options.Env,
	}, &created)
	return created.ID, err
}

// ExecStart starts a prepared process and returns the raw connection to it. A
// process without a TTY produces a multiplexed stream; see ReadMultiplexed.
func (c *Client) ExecStart(ctx context.Context, execID string, tty bool) (io.ReadWriteCloser, error) {
	res, err := c.request(ctx, http.MethodPost, "/exec/"+execID+"/start", nil, map[string]any{"Detach": false, "Tty": tty}, map[string]string{
		"Connection": "Upgrade",
		"Upgrade":    "tcp",
	})
	if err != nil {
		return nil, err
	}

	connection, ok := res.Body.(io.ReadWriteCloser)
	if !ok {
		res.Body.Close()
		return nil, errors.New("the Docker Engine did not upgrade the exec connection")
	}

	return connection, nil
}

// ExecResize resizes the TTY of a running process.
func (c *Client) ExecResize(ctx context.Context, execID string, cols, rows uint32) error {
	return c.do(ctx, http.MethodPost, "/exec/"+execID+"/resize", url.Values{
		"w": {strconv.FormatUint(uint64(cols), 10)},
		"h": {strconv.FormatUint(uint64(rows), 10)},
	}, nil, nil)
}

// ExecInspect returns the exit code of a finished process.
func (c *Client) ExecInspect(ctx context.Context, execID string) (running bool, exitCode int, err error) {
	var info struct {
		Running  bool `json:"Running"`
		ExitCode int  `json:"ExitCode"`
	}
	err = c.do(ctx, http.MethodGet, "/exec/"+execID+"/json", nil, nil, &info)
	return info.Running, info.ExitCode, err
}
