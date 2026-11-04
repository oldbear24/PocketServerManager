package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
	"github.com/oldbear24/PocketServerManager/internal/edge"
)

// The edge proxy is Caddy in a container of its own, so that updating the agent
// never touches traffic (ADR 0002). The agent reaches it through directories of
// its own volume that the edge mounts: Caddy's admin socket, the certificates,
// the ACME challenge answers and Caddy's autosaved config, which lets a new edge
// container resume the last config by itself. The rest of the volume, with the
// agent's identity, is never mounted into the edge.
const (
	edgeName    = "pocketcloud-edge"
	edgeOldName = edgeName + "-old"
	// edgeNetwork joins the edge and the containers its routes forward to.
	edgeNetwork = "pocketcloud-edge"
	// edgeSpecLabel holds the digest of the container's settings, so that an
	// agent that changes them replaces the container.
	edgeSpecLabel = "pocketcloud.edge.spec"

	edgeAdminWait  = 30 * time.Second
	edgeLoadLimit  = 2 * time.Minute
	edgeProbeLimit = 3 * time.Second
	edgeStopLimit  = 10

	// minSubpathAPI is the first Engine API with volume subpaths (Engine 26).
	minSubpathAPI = "1.45"
)

// edgeDirs are the directories of the agent's volume the edge mounts, where.
var edgeDirs = []struct {
	dir      string
	target   string
	readOnly bool
}{
	{"run", path.Dir(edge.AdminSocket), false},
	{"certs", edge.CertDir, true},
	{"acme", edge.ChallengeRoot, true},
	// XDG_CONFIG_HOME of the official image, where `caddy run --resume` keeps
	// the last loaded config
	{"config", "/config", false},
}

// edgeState is what the agent keeps about its edge, in edge/state.json: enough
// to bring the edge back on its own when the panel is away.
type edgeState struct {
	Image string `json:"image"`
	// Config is the config as loaded, with the agent's admin block.
	Config   json.RawMessage `json:"config,omitempty"`
	Digest   string          `json:"digest,omitempty"`
	Revision int64           `json:"revision,omitempty"`
	Attach   []string        `json:"attach,omitempty"`
	// Error is the last failure, until an apply succeeds.
	Error        string `json:"error,omitempty"`
	CaddyVersion string `json:"caddy_version,omitempty"`
}

func (a *Agent) edgeDir(parts ...string) string {
	return filepath.Join(append([]string{a.Config.StateDir, "edge"}, parts...)...)
}

func (a *Agent) edgeSocket() string {
	return a.edgeDir("run", path.Base(edge.AdminSocket))
}

func (a *Agent) challengeDir() string {
	return a.edgeDir("acme", ".well-known", "acme-challenge")
}

func (a *Agent) loadEdgeState() (*edgeState, error) {
	raw, err := os.ReadFile(a.edgeDir("state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state edgeState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("the edge's state is damaged: %w", err)
	}
	return &state, nil
}

func (a *Agent) saveEdgeState(state *edgeState) error {
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(a.edgeDir("state.json"), raw, 0o600)
}

// recordEdgeError keeps a failure for the self-check and the heartbeat. A state
// without a config means the edge never served anything.
func (a *Agent) recordEdgeError(err error) {
	state, loadErr := a.loadEdgeState()
	if loadErr != nil || state == nil {
		state = &edgeState{}
	}
	state.Error = err.Error()
	if saveErr := a.saveEdgeState(state); saveErr != nil {
		a.Log.Warn("failed to record the edge's error", "error", saveErr)
	}
}

// edgeHTTP talks to Caddy's admin API on its Unix socket. Caddy closes the
// connection after a /load, so every request gets a new one.
func (a *Agent) edgeHTTP(timeout time.Duration) *http.Client {
	socket := a.edgeSocket()
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			Proxy:             nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socket)
			},
		},
	}
}

func (a *Agent) edgeRequest(ctx context.Context, method, route string, body []byte, timeout time.Duration) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://edge"+route, reader)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := a.edgeHTTP(timeout).Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	return data, response.StatusCode, err
}

// loadEdgeConfig posts a complete config. Caddy checks it before it replaces the
// running one, so a refused config leaves the old one serving.
func (a *Agent) loadEdgeConfig(ctx context.Context, config []byte) error {
	body, status, err := a.edgeRequest(ctx, http.MethodPost, "/load", config, edgeLoadLimit)
	if err != nil {
		return fmt.Errorf("the edge's admin API does not answer: %w", err)
	}
	if status >= 300 {
		var refusal struct {
			Error string `json:"error"`
		}
		message := strings.TrimSpace(string(body))
		if json.Unmarshal(body, &refusal) == nil && refusal.Error != "" {
			message = refusal.Error
		}
		return fmt.Errorf("caddy refused the config, the previous one stays active: %s", message)
	}
	return nil
}

// waitForEdgeAdmin waits until a new edge's admin API answers.
func (a *Agent) waitForEdgeAdmin(ctx context.Context) error {
	deadline := time.Now().Add(edgeAdminWait)
	for {
		_, status, err := a.edgeRequest(ctx, http.MethodGet, "/config/", nil, edgeProbeLimit)
		if err == nil && status == http.StatusOK {
			return nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("status %d", status)
			}
			return fmt.Errorf("the edge's admin API did not answer within %s: %w", edgeAdminWait, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// withAdmin sets the agent's admin block on a config, so that no config can move
// the admin API somewhere the agent cannot reach.
func withAdmin(caddyJSON string) ([]byte, error) {
	var config map[string]json.RawMessage
	if err := json.Unmarshal([]byte(caddyJSON), &config); err != nil || config == nil {
		return nil, errors.New("the edge config is not a JSON object")
	}
	admin, err := json.Marshal(edge.Admin())
	if err != nil {
		return nil, err
	}
	config["admin"] = admin
	return json.Marshal(config)
}

// sameJSON compares two configs as values, not as text.
func sameJSON(a, b []byte) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	normalizedLeft, _ := json.Marshal(left)
	normalizedRight, _ := json.Marshal(right)
	return bytes.Equal(normalizedLeft, normalizedRight)
}

func (a *Agent) prepareEdgeDirs() error {
	for _, dir := range edgeDirs {
		if err := os.MkdirAll(a.edgeDir(dir.dir), 0o755); err != nil {
			return err
		}
	}
	// keys stay readable by root only; Caddy runs as root in its container
	if err := os.Chmod(a.edgeDir("certs"), 0o700); err != nil { //nolint:gosec // a directory needs x to be entered
		return err
	}
	return os.MkdirAll(a.challengeDir(), 0o755)
}

// edgeMounts mounts the edge's directories of the agent's volume, and nothing
// else of it: as volume subpaths on Engine 26 and newer, otherwise as binds of
// the volume's directory on the host. Outside a container, as in development,
// the state directory is a path on the host.
func (a *Agent) edgeMounts(ctx context.Context) ([]map[string]any, error) {
	source := a.Config.StateDir
	kind := "bind"

	if self, err := a.ownContainer(ctx); err == nil {
		source = volumeAt(self, a.Config.StateDir)
		if source == "" {
			return nil, fmt.Errorf("the agent's state directory %s is not on a volume", a.Config.StateDir)
		}
		if !strings.HasPrefix(source, "/") {
			kind = "volume"
		}
	}

	if kind == "volume" {
		version, err := a.Docker.Version(ctx)
		if err != nil {
			return nil, err
		}
		if !apiAtLeast(version.APIVersion, minSubpathAPI) {
			volume, err := a.Docker.InspectVolume(ctx, source)
			if err != nil {
				return nil, err
			}
			if volume.Mountpoint == "" {
				return nil, fmt.Errorf("the Docker Engine %s has no volume subpaths, and the volume %s has no directory on the host to mount instead", version.Version, source)
			}
			source, kind = volume.Mountpoint, "bind"
		}
	}

	mounts := make([]map[string]any, 0, len(edgeDirs))
	for _, dir := range edgeDirs {
		mount := map[string]any{"Target": dir.target, "ReadOnly": dir.readOnly}
		if kind == "volume" {
			mount["Type"] = "volume"
			mount["Source"] = source
			mount["VolumeOptions"] = map[string]any{"Subpath": "edge/" + dir.dir}
		} else {
			mount["Type"] = "bind"
			mount["Source"] = filepath.Join(source, "edge", dir.dir)
		}
		mounts = append(mounts, mount)
	}
	return mounts, nil
}

// apiAtLeast compares Engine API versions such as 1.45.
func apiAtLeast(version, minimum string) bool {
	parse := func(text string) (int, int) {
		major, minor, _ := strings.Cut(text, ".")
		x, _ := strconv.Atoi(major)
		y, _ := strconv.Atoi(minor)
		return x, y
	}
	x, y := parse(version)
	mx, my := parse(minimum)
	return x > mx || x == mx && y >= my
}

// checkEdgeMounts makes sure the engine mounted exactly what was asked. An engine
// that ignored a subpath would mount the whole volume, with the agent's identity,
// into the one container that faces the internet.
func checkEdgeMounts(info *dockerapi.ContainerInspect, wanted []map[string]any) error {
	mounts, _ := info.HostConfig["Mounts"].([]any)
	binds, _ := info.HostConfig["Binds"].([]any)
	if len(binds) > 0 || len(mounts) != len(wanted) {
		return errors.New("the edge container has other mounts than its own directories")
	}
	for _, want := range wanted {
		found := false
		for _, item := range mounts {
			got, _ := item.(map[string]any)
			if got["Target"] != want["Target"] {
				continue
			}
			found = got["Type"] == want["Type"] && got["Source"] == want["Source"] && got["ReadOnly"] == want["ReadOnly"]
			if options, ok := want["VolumeOptions"].(map[string]any); ok {
				gotOptions, _ := got["VolumeOptions"].(map[string]any)
				found = found && gotOptions != nil && gotOptions["Subpath"] == options["Subpath"]
			}
		}
		if !found {
			return fmt.Errorf("the Docker Engine did not mount %s as asked", want["Target"])
		}
	}
	return nil
}

func edgeContainerConfig(image string, mounts []map[string]any) (map[string]any, error) {
	config := map[string]any{
		"Image": image,
		"Cmd":   []string{"caddy", "run", "--resume"},
		// the admin API of a first start, before any config was loaded
		"Env": []string{"CADDY_ADMIN=unix/" + edge.AdminSocket},
		"ExposedPorts": map[string]any{
			"80/tcp": map[string]any{}, "443/tcp": map[string]any{}, "443/udp": map[string]any{},
		},
		"HostConfig": map[string]any{
			"NetworkMode":   edgeNetwork,
			"RestartPolicy": map[string]any{"Name": "unless-stopped"},
			// 443/udp is HTTP/3
			"PortBindings": map[string]any{
				"80/tcp":  []any{map[string]any{"HostPort": "80"}},
				"443/tcp": []any{map[string]any{"HostPort": "443"}},
				"443/udp": []any{map[string]any{"HostPort": "443"}},
			},
			"CapDrop":     []string{"ALL"},
			"CapAdd":      []string{"NET_BIND_SERVICE"},
			"SecurityOpt": []string{"no-new-privileges"},
			"Mounts":      mounts,
		},
	}

	spec, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(spec)
	config["Labels"] = map[string]string{
		"pocketcloud.role": "edge",
		edgeSpecLabel:      hex.EncodeToString(sum[:])[:16],
	}
	return config, nil
}

// edgeSwap is the result of ensuring the edge container: commit keeps it, undo
// goes back to what ran before, if anything was replaced.
type edgeSwap struct {
	commit func()
	undo   func()
}

func noSwap() edgeSwap { return edgeSwap{commit: func() {}, undo: func() {}} }

// ensureEdge makes the edge container run the given image with the current
// settings. A container with another image or other settings is replaced: the
// old one stops, the new one starts and resumes the last config from the shared
// directory, and the old one is removed only once the caller committed.
func (a *Agent) ensureEdge(ctx context.Context, image string, output io.Writer) (edgeSwap, error) {
	mounts, err := a.edgeMounts(ctx)
	if err != nil {
		return noSwap(), err
	}
	config, err := edgeContainerConfig(image, mounts)
	if err != nil {
		return noSwap(), err
	}
	spec := config["Labels"].(map[string]string)[edgeSpecLabel]

	if err := a.ensureEdgeNetwork(ctx); err != nil {
		return noSwap(), err
	}

	current, err := a.Docker.Inspect(ctx, edgeName)
	if err != nil && !dockerapi.IsNotFound(err) {
		return noSwap(), err
	}

	if current != nil {
		labels, _ := current.Config["Labels"].(map[string]any)
		currentImage, _ := current.Config["Image"].(string)
		if currentImage == image && labels[edgeSpecLabel] == spec {
			if !current.State.Running {
				_, _ = fmt.Fprintln(output, "Starting the edge container")
				if err := a.Docker.Start(ctx, current.ID); err != nil {
					return noSwap(), a.explainEdgeStart(ctx, err)
				}
			}
			return noSwap(), nil
		}
	}

	_, _ = fmt.Fprintln(output, "Pulling "+image)
	if err := a.Docker.Pull(ctx, image, nil, nil); err != nil {
		return noSwap(), fmt.Errorf("failed to pull the edge image: %w", err)
	}

	if current == nil {
		_, _ = fmt.Fprintln(output, "Creating the edge container")
		if _, err := a.startEdge(ctx, config, mounts); err != nil {
			return noSwap(), err
		}
		return noSwap(), nil
	}

	// a replacement: both cannot publish 80 and 443, so the edge is down for a
	// moment, and the old container stays until the new one has loaded the config
	_, _ = fmt.Fprintln(output, "Replacing the edge container")
	if leftover, err := a.Docker.Inspect(ctx, edgeOldName); err == nil {
		if err := a.Docker.Remove(ctx, leftover.ID, false, true); err != nil {
			return noSwap(), err
		}
	}
	if err := a.Docker.Stop(ctx, current.ID, edgeStopLimit); err != nil {
		return noSwap(), err
	}
	if err := a.Docker.Rename(ctx, current.ID, edgeOldName); err != nil {
		_ = a.Docker.Start(ctx, current.ID)
		return noSwap(), err
	}

	restore := func(newID string) {
		if newID != "" {
			if err := a.Docker.Remove(ctx, newID, false, true); err != nil && !dockerapi.IsNotFound(err) {
				a.Log.Error("failed to remove the new edge container", "error", err)
			}
		}
		if err := a.Docker.Rename(ctx, current.ID, edgeName); err != nil {
			a.Log.Error("failed to rename the old edge container back", "error", err)
		}
		if err := a.Docker.Start(ctx, current.ID); err != nil {
			a.Log.Error("failed to start the old edge container again", "error", err)
		}
	}

	newID, err := a.startEdge(ctx, config, mounts)
	if err != nil {
		restore("")
		return noSwap(), err
	}

	oldImage, _ := current.Config["Image"].(string)
	return edgeSwap{
		commit: func() {
			if err := a.Docker.Remove(ctx, current.ID, false, true); err != nil && !dockerapi.IsNotFound(err) {
				a.Log.Warn("failed to remove the old edge container", "error", err)
			}
			if oldImage != "" && oldImage != image {
				if err := a.Docker.RemoveImage(ctx, oldImage); err != nil {
					a.Log.Info("the old edge image was kept", "image", oldImage, "reason", err)
				}
			}
		},
		undo: func() { restore(newID) },
	}, nil
}

// startEdge creates and starts an edge container and checks its mounts. A
// container that cannot start is removed again.
func (a *Agent) startEdge(ctx context.Context, config map[string]any, mounts []map[string]any) (string, error) {
	if err := a.prepareEdgeDirs(); err != nil {
		return "", err
	}

	id, err := a.Docker.Create(ctx, edgeName, config)
	if err != nil {
		return "", fmt.Errorf("failed to create the edge container: %w", err)
	}

	remove := func() {
		if err := a.Docker.Remove(ctx, id, false, true); err != nil && !dockerapi.IsNotFound(err) {
			a.Log.Error("failed to remove the edge container that did not start", "error", err)
		}
	}

	info, err := a.Docker.Inspect(ctx, id)
	if err == nil {
		err = checkEdgeMounts(info, mounts)
	}
	if err != nil {
		remove()
		return "", fmt.Errorf("refusing to start the edge: %w", err)
	}

	if err := a.Docker.Start(ctx, id); err != nil {
		remove()
		return "", a.explainEdgeStart(ctx, err)
	}

	return id, nil
}

// explainEdgeStart turns Docker's port errors into a message that names who
// holds the port. PocketCloud never stops anyone's container for its edge.
func (a *Agent) explainEdgeStart(ctx context.Context, err error) error {
	text := err.Error()
	if !strings.Contains(text, "port is already allocated") && !strings.Contains(text, "address already in use") {
		return fmt.Errorf("failed to start the edge container: %w", err)
	}

	if containers, listErr := a.Docker.Containers(ctx); listErr == nil {
		for _, container := range containers {
			name := ""
			if len(container.Names) > 0 {
				name = strings.TrimPrefix(container.Names[0], "/")
			}
			if name == edgeName || name == edgeOldName || container.State != "running" {
				continue
			}
			for _, port := range container.Ports {
				if port.PublicPort == 80 || port.PublicPort == 443 {
					return fmt.Errorf("port %d is already used by the container %s: free it, then apply the edge again", port.PublicPort, name)
				}
			}
		}
	}

	return fmt.Errorf("a program on the host already listens on port 80 or 443: free the ports, then apply the edge again (%s)", text)
}

// ensureEdgeNetwork creates the network of the edge and its upstreams. It cannot
// be internal: the edge publishes its ports through it.
func (a *Agent) ensureEdgeNetwork(ctx context.Context) error {
	if _, err := a.Docker.InspectNetwork(ctx, edgeNetwork); err == nil {
		return nil
	} else if !dockerapi.IsNotFound(err) {
		return err
	}
	_, err := a.Docker.CreateNetwork(ctx, edgeNetwork, dockerapi.NetworkOptions{
		Labels: map[string]string{"pocketcloud.role": "edge-network"},
	})
	if err != nil {
		// another command may have created it in the meantime
		if _, inspectErr := a.Docker.InspectNetwork(ctx, edgeNetwork); inspectErr == nil {
			return nil
		}
	}
	return err
}

// attachUpstreams connects the containers the routes forward to. One that does
// not exist yet is only noted: it joins when it starts.
func (a *Agent) attachUpstreams(ctx context.Context, names []string, output io.Writer) {
	for _, name := range names {
		err := a.attach(ctx, edgeNetwork, name, nil)
		switch {
		case err == nil:
		case dockerapi.IsNotFound(err):
			_, _ = fmt.Fprintf(output, "The container %s does not exist yet: it joins the edge's network when it starts.\n", name)
		default:
			_, _ = fmt.Fprintf(output, "The container %s could not join the edge's network: %v\n", name, err)
		}
	}
}

func (a *Agent) registerEdgeHandlers() {
	x := a.Executor
	x.Handle("edge.apply", a.handleEdgeApply)
	x.Handle("cert.install", a.handleCertInstall)
	x.Handle("cert.remove", a.handleCertRemove)
	x.Handle("acme.challenge.present", a.handleChallengePresent)
	x.Handle("acme.challenge.cleanup", a.handleChallengeCleanup)
}

func (a *Agent) handleEdgeApply(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetEdgeApply()

	a.edgeMu.Lock()
	defer a.edgeMu.Unlock()

	if payload.GetCaddyJson() == "" {
		return a.removeEdge(ctx, reporter)
	}

	result, err := a.applyEdge(ctx, payload, reporter)
	if err != nil {
		a.recordEdgeError(err)
	}
	a.checks.runEdge(ctx)
	return result, err
}

func (a *Agent) applyEdge(ctx context.Context, payload *agentv1.EdgeApply, reporter *Reporter) (map[string]any, error) {
	image := payload.GetCaddyImage()
	if !strings.Contains(image, "@sha256:") {
		return nil, fmt.Errorf("refusing the edge image %q: it is not pinned by digest", image)
	}

	config, err := withAdmin(payload.GetCaddyJson())
	if err != nil {
		return nil, err
	}

	if err := a.prepareEdgeDirs(); err != nil {
		return nil, err
	}

	reporter.Progress("container", "Preparing the edge container", 20)
	swap, err := a.ensureEdge(ctx, image, reporter)
	if err != nil {
		return nil, err
	}

	reporter.Progress("attach", "Connecting the upstream containers", 50)
	a.attachUpstreams(ctx, payload.GetAttachContainers(), reporter)

	reporter.Progress("load", "Loading the config", 70)
	err = a.waitForEdgeAdmin(ctx)
	if err == nil {
		err = a.loadEdgeConfig(ctx, config)
	}
	if err != nil {
		swap.undo()
		return nil, err
	}
	swap.commit()

	state := &edgeState{
		Image:        image,
		Config:       config,
		Digest:       edge.Digest(image, payload.GetCaddyJson()),
		Revision:     payload.GetRevision(),
		Attach:       payload.GetAttachContainers(),
		CaddyVersion: a.edgeCaddyVersion(ctx),
	}
	if err := a.saveEdgeState(state); err != nil {
		return nil, fmt.Errorf("the config is active, but the agent could not store it: %w", err)
	}

	return map[string]any{"revision": state.Revision, "digest": state.Digest, "caddy_version": state.CaddyVersion}, nil
}

// edgeCaddyVersion reads the Caddy version from the edge container's environment,
// where the official image keeps it.
func (a *Agent) edgeCaddyVersion(ctx context.Context) string {
	info, err := a.Docker.Inspect(ctx, edgeName)
	if err != nil {
		return ""
	}
	env, _ := info.Config["Env"].([]any)
	for _, item := range env {
		if text, ok := item.(string); ok {
			if version, found := strings.CutPrefix(text, "CADDY_VERSION="); found {
				return strings.TrimPrefix(version, "v")
			}
		}
	}
	return ""
}

// removeEdge takes the edge away, with its certificates and keys, when the
// server loses the edge role.
func (a *Agent) removeEdge(ctx context.Context, reporter *Reporter) (map[string]any, error) {
	reporter.Progress("remove", "Removing the edge container", 30)
	for _, name := range []string{edgeName, edgeOldName} {
		if err := a.Docker.Remove(ctx, name, false, true); err != nil && !dockerapi.IsNotFound(err) {
			return nil, fmt.Errorf("failed to remove %s: %w", name, err)
		}
	}

	if err := os.RemoveAll(a.edgeDir()); err != nil {
		return nil, fmt.Errorf("the edge is gone, but its files could not be removed: %w", err)
	}

	// the network goes too once nothing else uses it
	if err := a.Docker.RemoveNetwork(ctx, edgeNetwork); err != nil && !dockerapi.IsNotFound(err) {
		_, _ = fmt.Fprintln(reporter, "The edge's network stays, other containers still use it.")
	}

	a.checks.runEdge(ctx)
	return map[string]any{"removed": true}, nil
}

// restoreEdge brings the edge back as it was, when the agent starts: a container
// that is gone is created again, a stopped one is started, and a config that
// differs from the last one loaded is loaded again. An interrupted replacement is
// undone first.
func (a *Agent) restoreEdge(ctx context.Context) {
	a.edgeMu.Lock()
	defer a.edgeMu.Unlock()

	state, err := a.loadEdgeState()
	if err != nil {
		a.Log.Error("cannot read the edge's state", "error", err)
		return
	}
	if state == nil || len(state.Config) == 0 {
		return
	}

	if _, err := a.Docker.Inspect(ctx, edgeName); dockerapi.IsNotFound(err) {
		if old, err := a.Docker.Inspect(ctx, edgeOldName); err == nil {
			a.Log.Warn("putting back the edge container of an interrupted replacement")
			if err := a.Docker.Rename(ctx, old.ID, edgeName); err != nil {
				a.Log.Error("failed to rename the old edge container back", "error", err)
			}
		}
	}

	swap, err := a.ensureEdge(ctx, state.Image, io.Discard)
	if err == nil {
		err = a.waitForEdgeAdmin(ctx)
	}
	if err == nil {
		var live []byte
		live, _, err = a.edgeRequest(ctx, http.MethodGet, "/config/", nil, edgeProbeLimit)
		if err == nil && !sameJSON(live, state.Config) {
			a.Log.Info("the edge lost its config, loading the last one again")
			err = a.loadEdgeConfig(ctx, state.Config)
		}
	}
	if err != nil {
		swap.undo()
		a.Log.Error("failed to bring back the edge", "error", err)
		a.recordEdgeError(fmt.Errorf("the edge could not be brought back: %w", err))
		return
	}
	swap.commit()

	if state.Error != "" {
		state.Error = ""
		if err := a.saveEdgeState(state); err != nil {
			a.Log.Warn("failed to clear the edge's error", "error", err)
		}
	}
	a.attachUpstreams(ctx, state.Attach, io.Discard)
}

// reattachUpstream puts a container the routes forward to back on the edge's
// network when it starts: Compose recreates containers, and a new one is not on
// the networks its predecessor was connected to by hand.
func (a *Agent) reattachUpstream(ctx context.Context, name string) {
	state, err := a.loadEdgeState()
	if err != nil || state == nil || !slices.Contains(state.Attach, name) {
		return
	}
	if err := a.attach(ctx, edgeNetwork, name, nil); err != nil && !dockerapi.IsNotFound(err) {
		a.Log.Warn("failed to put a container back on the edge's network", "container", name, "error", err)
	}
}

// edgeStatus is the edge's part of the heartbeat; nil on a server without one.
func (a *Agent) edgeStatus(ctx context.Context) *agentv1.EdgeStatus {
	state, err := a.loadEdgeState()
	if err != nil {
		return &agentv1.EdgeStatus{Error: err.Error()}
	}
	if state == nil {
		return nil
	}

	status := &agentv1.EdgeStatus{
		Digest:       state.Digest,
		Revision:     state.Revision,
		Error:        state.Error,
		CaddyVersion: state.CaddyVersion,
	}
	if len(state.Config) == 0 {
		return status
	}

	total, err := a.edgeRequestCount(ctx)
	if err != nil {
		return status
	}
	status.Running = true

	a.edgeMetricsMu.Lock()
	now := time.Now()
	if !a.edgeCountAt.IsZero() && total >= a.edgeCount {
		if elapsed := now.Sub(a.edgeCountAt).Seconds(); elapsed > 0 {
			status.RequestsPerS = (total - a.edgeCount) / elapsed
		}
	}
	a.edgeCount, a.edgeCountAt = total, now
	a.edgeMetricsMu.Unlock()

	return status
}

// edgeRequestCount sums Caddy's request counters of the handlers that answer
// requests, so that a request passing several handlers counts once.
func (a *Agent) edgeRequestCount(ctx context.Context) (float64, error) {
	body, status, err := a.edgeRequest(ctx, http.MethodGet, "/metrics", nil, edgeProbeLimit)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("the edge's metrics answered %d", status)
	}
	return countRequests(body), nil
}

func countRequests(metrics []byte) float64 {
	var total float64
	scanner := bufio.NewScanner(bytes.NewReader(metrics))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		rest, ok := strings.CutPrefix(line, "caddy_http_requests_total{")
		if !ok {
			continue
		}
		labels, value, ok := strings.Cut(rest, "} ")
		if !ok {
			continue
		}
		answering := false
		for _, handler := range edge.AnsweringHandlers {
			if strings.Contains(labels, `handler="`+handler+`"`) {
				answering = true
			}
		}
		if !answering {
			continue
		}
		if number, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
			total += number
		}
	}
	return total
}

// usedCertFiles lists the certificate files the loaded config refers to.
func (a *Agent) usedCertFiles() (map[string]bool, error) {
	used := map[string]bool{}
	state, err := a.loadEdgeState()
	if err != nil || state == nil {
		return used, err
	}
	prefix := edge.CertDir + "/"
	text := string(state.Config)
	for {
		index := strings.Index(text, prefix)
		if index < 0 {
			return used, nil
		}
		text = text[index+len(prefix):]
		end := strings.IndexAny(text, `"\`)
		if end < 0 {
			return used, nil
		}
		used[text[:end]] = true
	}
}

// removeCertFiles removes the files of a certificate that the loaded config does
// not use, except keep; Caddy would not start again without files it refers to.
func (a *Agent) removeCertFiles(id, keep string) (removed []string, inUse []string, err error) {
	used, err := a.usedCertFiles()
	if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(a.edgeDir("certs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".crt"), ".key")
		if !strings.HasPrefix(base, id+"-") || base == keep {
			continue
		}
		if used[name] {
			inUse = append(inUse, base)
			continue
		}
		if err := os.Remove(a.edgeDir("certs", name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, inUse, err
		}
		removed = append(removed, name)
	}
	return removed, inUse, nil
}

func (a *Agent) handleCertInstall(_ context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	payload := command.GetCertInstall()

	file, err := edge.CertFileID(payload.GetCertificateId(), payload.GetChainPem())
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(payload.GetChainPem(), payload.GetKeyPem())
	if err != nil {
		return nil, fmt.Errorf("the certificate and its key do not belong together: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}

	a.edgeMu.Lock()
	defer a.edgeMu.Unlock()

	if err := a.prepareEdgeDirs(); err != nil {
		return nil, err
	}
	// the key first: a config can only refer to a certificate whose files are complete
	if err := writeAtomic(a.edgeDir("certs", file+".key"), payload.GetKeyPem(), 0o600); err != nil {
		return nil, err
	}
	if err := writeAtomic(a.edgeDir("certs", file+".crt"), normalizedChain(payload.GetChainPem()), 0o644); err != nil {
		return nil, err
	}

	// older files of the same certificate, from before a renewal
	if _, _, err := a.removeCertFiles(payload.GetCertificateId(), file); err != nil {
		a.Log.Warn("failed to remove older certificate files", "certificate", payload.GetCertificateId(), "error", err)
	}

	return map[string]any{"file": file, "not_after": leaf.NotAfter.UTC().Format(time.RFC3339)}, nil
}

// normalizedChain keeps only the certificates of a chain, so nothing else from a
// pasted file ends up on the edge.
func normalizedChain(chain []byte) []byte {
	var out bytes.Buffer
	rest := chain
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return out.Bytes()
		}
		if block.Type == "CERTIFICATE" {
			_ = pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})
		}
	}
}

func (a *Agent) handleCertRemove(_ context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	id := command.GetCertRemove().GetCertificateId()
	if _, err := edge.CertFileID(id, []byte("-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----\n")); err != nil {
		return nil, err
	}

	a.edgeMu.Lock()
	defer a.edgeMu.Unlock()

	removed, inUse, err := a.removeCertFiles(id, "")
	if err != nil {
		return nil, err
	}
	if len(inUse) > 0 {
		return nil, fmt.Errorf("the edge's config still uses this certificate (%s): apply a config without it first", strings.Join(inUse, ", "))
	}
	return map[string]any{"removed": removed}, nil
}

// validToken accepts the base64url tokens of ACME (RFC 8555), which become file names.
func validToken(token string) bool {
	if token == "" || len(token) > 256 {
		return false
	}
	for _, r := range token {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		digit := r >= '0' && r <= '9'
		if !letter && !digit && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func (a *Agent) handleChallengePresent(_ context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	payload := command.GetAcmeChallengePresent()
	if !validToken(payload.GetToken()) {
		return nil, errors.New("the challenge token is not a valid ACME token")
	}
	if err := os.MkdirAll(a.challengeDir(), 0o755); err != nil {
		return nil, err
	}
	err := writeAtomic(filepath.Join(a.challengeDir(), payload.GetToken()), []byte(payload.GetKeyAuthorization()), 0o644)
	return nil, err
}

func (a *Agent) handleChallengeCleanup(_ context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	token := command.GetAcmeChallengeCleanup().GetToken()
	if !validToken(token) {
		return nil, errors.New("the challenge token is not a valid ACME token")
	}
	err := os.Remove(filepath.Join(a.challengeDir(), token))
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return nil, err
}
