package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/edge"
	"github.com/oldbear24/PocketServerManager/internal/edge/fakecaddy"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/version"
)

// fakeEdge is the Caddy of an edge agent under test: the fake admin API, started
// on the socket in the agent's state directory when the edge container starts,
// and stopped with it. Like `caddy run --resume`, it resumes the last config
// from the shared config directory.
type fakeEdge struct {
	t        *testing.T
	socket   string
	autosave string

	mu     sync.Mutex
	caddy  *fakecaddy.Server
	reject func(map[string]any) error
	starts int
}

func (f *fakeEdge) start() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.caddy != nil {
		return
	}
	server, err := fakecaddy.Start(f.socket, f.autosave)
	if err != nil {
		f.t.Errorf("the fake edge did not start: %v", err)
		return
	}
	server.Reject = f.reject
	f.caddy = server
	f.starts++
}

func (f *fakeEdge) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.caddy != nil {
		_ = f.caddy.Close()
		f.caddy = nil
	}
}

func (f *fakeEdge) server() *fakecaddy.Server {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.caddy
}

// rejectAll makes the edge refuse every config from now on, as a Caddy that
// does not know the config's fields would.
func (f *fakeEdge) rejectAll(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reject = func(map[string]any) error { return errors.New(reason) }
	if f.caddy != nil {
		f.caddy.Reject = f.reject
	}
}

func (f *fakeEdge) config(t *testing.T) map[string]any {
	t.Helper()
	server := f.server()
	if server == nil {
		t.Fatal("the edge does not run")
	}
	var config map[string]any
	if err := json.Unmarshal(server.Config(), &config); err != nil {
		t.Fatalf("the edge has no config: %v", err)
	}
	return config
}

// shortStateDir is a state directory short enough for the admin socket in it: a
// Unix socket path has room for about 100 bytes.
func shortStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pce")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// edgeAgent starts an agent with the edge role whose edge is a fake Caddy.
func (h *harness) edgeAgent(name string) (*runningAgent, *core.Record, *fakeEdge) {
	h.t.Helper()

	config := h.agentConfig(name, h.token(name, schema.HostRoleDocker, schema.HostRoleEdge))
	config.StateDir = shortStateDir(h.t)

	fake := h.fakeEdge(config.StateDir)

	running := h.start(config)

	var host *core.Record
	h.waitFor("the edge agent to connect", func() bool {
		found, err := h.app.FindFirstRecordByData(schema.Hosts, "name", name)
		if err != nil || !found.GetBool("stream_open") {
			return false
		}
		host = found
		return true
	})

	return running, host, fake
}

func (h *harness) fakeEdge(stateDir string) *fakeEdge {
	fake := &fakeEdge{
		t:        h.t,
		socket:   filepath.Join(stateDir, "edge", "run", "admin.sock"),
		autosave: filepath.Join(stateDir, "edge", "config", "caddy", "autosave.json"),
	}
	h.t.Cleanup(fake.stop)

	h.docker.OnStart = func(c *fakedocker.Container) (string, string) {
		if c.Labels["pocketcloud.role"] == "edge" {
			fake.start()
		}
		return "running", c.Health
	}
	h.docker.OnStop = func(c *fakedocker.Container) {
		if c.Labels["pocketcloud.role"] == "edge" {
			fake.stop()
		}
	}
	return fake
}

func edgeApply(t *testing.T, image string, revision int64, config edge.Config, attach ...string) *agentv1.Command {
	t.Helper()
	raw, err := edge.Build(config)
	if err != nil {
		t.Fatal(err)
	}
	return &agentv1.Command{Payload: &agentv1.Command_EdgeApply{EdgeApply: &agentv1.EdgeApply{
		CaddyJson: string(raw), CaddyImage: image, Revision: revision, AttachContainers: attach,
	}}}
}

var oneRoute = edge.Config{Routes: []edge.Route{{Hostname: "app.example.com", Kind: edge.KindProxy, Upstream: "web:8080"}}}

func adminListen(config map[string]any) string {
	admin, _ := config["admin"].(map[string]any)
	listen, _ := admin["listen"].(string)
	return listen
}

func hostRoutes(config map[string]any) []string {
	apps, _ := config["apps"].(map[string]any)
	httpApp, _ := apps["http"].(map[string]any)
	servers, _ := httpApp["servers"].(map[string]any)
	server, _ := servers["http"].(map[string]any)
	routes, _ := server["routes"].([]any)
	var hosts []string
	for _, route := range routes {
		matchers, _ := route.(map[string]any)["match"].([]any)
		for _, matcher := range matchers {
			hosts = append(hosts, asStrings(matcher.(map[string]any)["host"])...)
		}
	}
	return hosts
}

func asStrings(value any) []string {
	list, _ := value.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func selfCheck(t *testing.T, host *core.Record, name string) (ok, skipped bool, detail string) {
	t.Helper()
	var checks []struct {
		Name    string `json:"name"`
		OK      bool   `json:"ok"`
		Skipped bool   `json:"skipped"`
		Detail  string `json:"detail"`
	}
	_ = json.Unmarshal([]byte(host.GetString("self_checks")), &checks)
	for _, check := range checks {
		if check.Name == name {
			return check.OK, check.Skipped, check.Detail
		}
	}
	return false, false, "missing"
}

// TestEdgeApplyStartsTheEdgeAndLoadsItsConfig covers the first edge.apply: the
// container with its ports, mounts and limits, the network for the upstreams,
// the config with the agent's admin block, and a host that is no longer
// degraded for want of an edge.
func TestEdgeApplyStartsTheEdgeAndLoadsItsConfig(t *testing.T) {
	h := newHarness(t)
	h.docker.AddContainer("web", "nginx:1", nil)

	running, host, fake := h.edgeAgent("edge-1")

	// an edge-role host without an edge yet is not degraded for it
	h.waitFor("the first self-checks", func() bool {
		_, _, detail := selfCheck(t, h.host(host.Id), "edge")
		return detail != "missing"
	})
	ok, skipped, detail := selfCheck(t, h.host(host.Id), "edge")
	if !ok || !skipped {
		t.Errorf("the edge check before any edge: ok %v, skipped %v, %s", ok, skipped, detail)
	}

	record := h.create(host.Id, edgeApply(t, version.EdgeRef(), 1, oneRoute, "web"))
	done := h.waitForStatus(record.Id, schema.CommandSucceeded, schema.CommandFailed)
	if done.GetString("status") != schema.CommandSucceeded {
		t.Fatalf("edge.apply: %s", done.GetString("error"))
	}

	container := h.docker.Find("pocketcloud-edge")
	if container == nil {
		t.Fatal("no edge container")
	}
	if container.Labels["pocketcloud.role"] != "edge" || container.Image != version.EdgeRef() || container.Status != "running" {
		t.Errorf("edge container: %+v", container)
	}
	hostConfig, _ := json.Marshal(container.Host)
	for _, want := range []string{`"80/tcp"`, `"443/tcp"`, `"443/udp"`, `"CapDrop":["ALL"]`, `"no-new-privileges"`, `"unless-stopped"`} {
		if !strings.Contains(string(hostConfig), want) {
			t.Errorf("the edge's host config lacks %s: %s", want, hostConfig)
		}
	}
	mounts, _ := container.Host["Mounts"].([]any)
	if len(mounts) != 4 {
		t.Errorf("the edge has %d mounts: %v", len(mounts), mounts)
	}
	for _, mount := range mounts {
		source, _ := mount.(map[string]any)["Source"].(string)
		if !strings.HasPrefix(source, filepath.Join(running.config.StateDir, "edge")+"/") {
			t.Errorf("the edge mounts %s, outside the agent's edge directory", source)
		}
	}

	if networks := h.docker.NetworksOf(h.docker.Find("web").ID); !slices.Contains(networks, "pocketcloud-edge") {
		t.Errorf("the upstream is not on the edge's network: %v", networks)
	}

	config := fake.config(t)
	if adminListen(config) != "unix/"+edge.AdminSocket {
		t.Errorf("the admin API listens on %q", adminListen(config))
	}
	if routes := hostRoutes(config); !slices.Equal(routes, []string{"app.example.com"}) {
		t.Errorf("routes: %v", routes)
	}

	h.waitFor("the edge check to pass", func() bool {
		ok, skipped, _ := selfCheck(t, h.host(host.Id), "edge")
		return ok && !skipped
	})
	// the other checks of a test agent fail (no restic, no host mount), so the
	// host's status says nothing here; the edge's own check is what counts
	t.Logf("self checks: %s", h.host(host.Id).GetString("self_checks"))
}

// TestARefusedConfigKeepsTheOldOne is acceptance criterion 2 of #20.
func TestARefusedConfigKeepsTheOldOne(t *testing.T) {
	h := newHarness(t)
	_, host, fake := h.edgeAgent("edge-2")

	first := h.create(host.Id, edgeApply(t, version.EdgeRef(), 1, oneRoute))
	h.waitForStatus(first.Id, schema.CommandSucceeded)
	good := fake.server().Config()

	fake.rejectAll("loading http app module: unknown module: http.handlers.nope")
	second := h.create(host.Id, edgeApply(t, version.EdgeRef(), 2, edge.Config{Routes: []edge.Route{
		{Hostname: "other.example.com", Kind: edge.KindProxy, Upstream: "other:80"},
	}}))
	done := h.waitForStatus(second.Id, schema.CommandSucceeded, schema.CommandFailed)

	if done.GetString("status") != schema.CommandFailed || !strings.Contains(done.GetString("error"), "unknown module") ||
		!strings.Contains(done.GetString("error"), "previous one stays active") {
		t.Errorf("a refused config: %s %s", done.GetString("status"), done.GetString("error"))
	}
	if string(fake.server().Config()) != string(good) {
		t.Error("the refused config replaced the good one")
	}
	if h.docker.Find("pocketcloud-edge") == nil {
		t.Error("the edge container is gone after a refused config")
	}
}

// TestTheAgentOwnsTheAdminBlock: a config cannot move the admin API somewhere the
// agent cannot reach it, which would lock the agent out until the edge is rebuilt.
func TestTheAgentOwnsTheAdminBlock(t *testing.T) {
	h := newHarness(t)
	_, host, fake := h.edgeAgent("edge-3")

	command := edgeApply(t, version.EdgeRef(), 1, oneRoute)
	payload := command.GetEdgeApply()
	payload.CaddyJson = strings.Replace(payload.CaddyJson, `"unix//pocketcloud/run/admin.sock"`, `"localhost:2019"`, 1)

	record := h.create(host.Id, command)
	h.waitForStatus(record.Id, schema.CommandSucceeded)

	if listen := adminListen(fake.config(t)); listen != "unix/"+edge.AdminSocket {
		t.Errorf("the admin API moved to %q", listen)
	}
}

func TestAnEdgeImageMustBePinned(t *testing.T) {
	h := newHarness(t)
	_, host, _ := h.edgeAgent("edge-4")

	record := h.create(host.Id, edgeApply(t, "caddy:2", 1, oneRoute))
	done := h.waitForStatus(record.Id, schema.CommandSucceeded, schema.CommandFailed)
	if done.GetString("status") != schema.CommandFailed || !strings.Contains(done.GetString("error"), "not pinned by digest") {
		t.Errorf("%s %s", done.GetString("status"), done.GetString("error"))
	}
	if h.docker.Find("pocketcloud-edge") != nil {
		t.Error("an edge was created from an unpinned image")
	}
}

// TestAnEdgeThatIsGoneComesBack: the agent keeps the last config, so it brings
// back an edge container that was removed while the agent was down.
func TestAnEdgeThatIsGoneComesBack(t *testing.T) {
	h := newHarness(t)
	running, host, fake := h.edgeAgent("edge-5")

	record := h.create(host.Id, edgeApply(t, version.EdgeRef(), 1, oneRoute))
	h.waitForStatus(record.Id, schema.CommandSucceeded)

	if err := running.stop(); err != nil {
		t.Fatal(err)
	}
	h.docker.RemoveContainer(h.docker.Find("pocketcloud-edge").ID)
	// the autosave was in the removed container's directory as far as Caddy goes;
	// remove it too, so only the agent can bring the config back
	_ = os.Remove(fake.autosave)

	again := h.start(running.config)
	h.waitFor("the edge to come back", func() bool {
		server := fake.server()
		if server == nil || h.docker.Find("pocketcloud-edge") == nil {
			return false
		}
		var config map[string]any
		return json.Unmarshal(server.Config(), &config) == nil && slices.Equal(hostRoutes(config), []string{"app.example.com"})
	})
	_ = again
}

// TestPortsTakenAreExplained: an edge that cannot publish 80 and 443 says so, the
// container that could not start is removed, and the host shows degraded.
func TestPortsTakenAreExplained(t *testing.T) {
	h := newHarness(t)
	_, host, _ := h.edgeAgent("edge-6")

	h.docker.Fail("/start", "driver failed programming external connectivity on endpoint pocketcloud-edge: failed to bind host port 0.0.0.0:80/tcp: address already in use")

	record := h.create(host.Id, edgeApply(t, version.EdgeRef(), 1, oneRoute))
	done := h.waitForStatus(record.Id, schema.CommandSucceeded, schema.CommandFailed)
	if done.GetString("status") != schema.CommandFailed || !strings.Contains(done.GetString("error"), "already listens on port 80 or 443") {
		t.Errorf("ports taken: %s %s", done.GetString("status"), done.GetString("error"))
	}
	if h.docker.Find("pocketcloud-edge") != nil {
		t.Error("the edge container that could not start was left behind")
	}

	h.waitFor("the host to show degraded", func() bool {
		ok, _, detail := selfCheck(t, h.host(host.Id), "edge")
		return !ok && strings.Contains(detail, "port 80") && h.host(host.Id).GetString("status") == schema.HostDegraded
	})
}

// TestANewEdgeImageReplacesTheContainer: a release with a newer Caddy swaps the
// edge container, and a config the new one refuses puts the old one back.
func TestANewEdgeImageReplacesTheContainer(t *testing.T) {
	h := newHarness(t)
	_, host, fake := h.edgeAgent("edge-7")

	first := "docker.io/library/caddy:2.11.6@sha256:" + strings.Repeat("a", 64)
	second := "docker.io/library/caddy:2.11.7@sha256:" + strings.Repeat("b", 64)
	third := "docker.io/library/caddy:2.12.0@sha256:" + strings.Repeat("c", 64)

	record := h.create(host.Id, edgeApply(t, first, 1, oneRoute))
	h.waitForStatus(record.Id, schema.CommandSucceeded)
	firstID := h.docker.Find("pocketcloud-edge").ID

	record = h.create(host.Id, edgeApply(t, second, 2, oneRoute))
	done := h.waitForStatus(record.Id, schema.CommandSucceeded, schema.CommandFailed)
	if done.GetString("status") != schema.CommandSucceeded {
		t.Fatalf("the replacement failed: %s", done.GetString("error"))
	}
	replaced := h.docker.Find("pocketcloud-edge")
	if replaced == nil || replaced.ID == firstID || replaced.Image != second {
		t.Fatalf("the edge was not replaced: %+v", replaced)
	}
	if h.docker.Find("pocketcloud-edge-old") != nil {
		t.Error("the old edge container was left behind")
	}

	// the newest Caddy refuses the config: the one before comes back
	fake.rejectAll("json: unknown field \"stream_close_delay\"")
	record = h.create(host.Id, edgeApply(t, third, 3, oneRoute))
	done = h.waitForStatus(record.Id, schema.CommandSucceeded, schema.CommandFailed)
	if done.GetString("status") != schema.CommandFailed {
		t.Fatal("a config the new image refuses was accepted")
	}
	back := h.docker.Find("pocketcloud-edge")
	if back == nil || back.ID != replaced.ID || back.Image != second || back.Status != "running" {
		t.Errorf("the previous edge did not come back: %+v", back)
	}
}

// TestTheEdgeMountsOnlyItsOwnDirectories: an agent in a container mounts parts
// of its volume into the edge, as subpaths, or as binds on engines without them.
func TestTheEdgeMountsOnlyItsOwnDirectories(t *testing.T) {
	for _, api := range []string{"1.47", "1.44"} {
		t.Run(api, func(t *testing.T) {
			h := newHarness(t)
			h.docker.APIVersion = api
			h.docker.VolumeRoot = "/var/lib/docker/volumes"
			h.docker.AddVolume("pocketcloud-agent")

			config := h.agentConfig("edge-8", h.token("edge-8", schema.HostRoleEdge))
			config.StateDir = shortStateDir(t)
			h.docker.AddContainer("pocketcloud-agent", "ghcr.io/oldbear24/pocketcloud-agent:dev", map[string]string{"pocketcloud.role": "agent"})
			h.docker.SetHostConfig("pocketcloud-agent", map[string]any{"Binds": []any{"pocketcloud-agent:" + config.StateDir}})
			h.fakeEdge(config.StateDir)

			h.start(config)
			var host *core.Record
			h.waitFor("the agent to connect", func() bool {
				found, err := h.app.FindFirstRecordByData(schema.Hosts, "name", "edge-8")
				host = found
				return err == nil && found.GetBool("stream_open")
			})

			record := h.create(host.Id, edgeApply(t, version.EdgeRef(), 1, oneRoute))
			done := h.waitForStatus(record.Id, schema.CommandSucceeded, schema.CommandFailed)
			if done.GetString("status") != schema.CommandSucceeded {
				t.Fatalf("edge.apply: %s", done.GetString("error"))
			}

			mounts, _ := h.docker.Find("pocketcloud-edge").Host["Mounts"].([]any)
			if len(mounts) != 4 {
				t.Fatalf("mounts: %v", mounts)
			}
			for _, item := range mounts {
				mount := item.(map[string]any)
				switch api {
				case "1.47":
					options, _ := mount["VolumeOptions"].(map[string]any)
					if mount["Type"] != "volume" || mount["Source"] != "pocketcloud-agent" || !strings.HasPrefix(options["Subpath"].(string), "edge/") {
						t.Errorf("a subpath mount: %v", mount)
					}
				case "1.44":
					if mount["Type"] != "bind" || !strings.HasPrefix(mount["Source"].(string), "/var/lib/docker/volumes/pocketcloud-agent/_data/edge/") {
						t.Errorf("a bind of the volume's directory: %v", mount)
					}
				}
			}
		})
	}
}

// TestCertificatesAndChallengesAreFiles covers cert.install, cert.remove and the
// HTTP-01 answers, as the edge serves them from the agent's directories.
func TestCertificatesAndChallengesAreFiles(t *testing.T) {
	h := newHarness(t)
	running, host, _ := h.edgeAgent("edge-9")
	dir := filepath.Join(running.config.StateDir, "edge")

	chain, key := selfSigned(t, "app.example.com")
	record := h.create(host.Id, &agentv1.Command{Payload: &agentv1.Command_CertInstall{CertInstall: &agentv1.CertInstall{
		CertificateId: "cert1", Names: []string{"app.example.com"}, ChainPem: chain, KeyPem: key,
	}}})
	done := h.waitForStatus(record.Id, schema.CommandSucceeded, schema.CommandFailed)
	if done.GetString("status") != schema.CommandSucceeded {
		t.Fatalf("cert.install: %s", done.GetString("error"))
	}
	file, err := edge.CertFileID("cert1", chain)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "certs", file+".key"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the key file: %v %v", info, err)
	}
	if strings.Contains(done.GetString("result"), "PRIVATE KEY") {
		t.Error("the key is in the command's result")
	}

	// the config refers to it, so it cannot be removed
	config := edge.Config{
		Certificates: []edge.Certificate{{ID: file}},
		Routes:       []edge.Route{{Hostname: "app.example.com", Kind: edge.KindProxy, Upstream: "web:80", Certificate: file, HTTPSRedirect: true}},
	}
	record = h.create(host.Id, edgeApply(t, version.EdgeRef(), 1, config))
	h.waitForStatus(record.Id, schema.CommandSucceeded)

	record = h.create(host.Id, &agentv1.Command{Payload: &agentv1.Command_CertRemove{CertRemove: &agentv1.CertRemove{CertificateId: "cert1"}}})
	done = h.waitForStatus(record.Id, schema.CommandSucceeded, schema.CommandFailed)
	if done.GetString("status") != schema.CommandFailed || !strings.Contains(done.GetString("error"), "still uses") {
		t.Errorf("removing a certificate in use: %s %s", done.GetString("status"), done.GetString("error"))
	}

	// a renewal brings new files and removes the old ones once nothing uses them
	record = h.create(host.Id, edgeApply(t, version.EdgeRef(), 2, oneRoute))
	h.waitForStatus(record.Id, schema.CommandSucceeded)
	renewed, renewedKey := selfSigned(t, "app.example.com")
	record = h.create(host.Id, &agentv1.Command{Payload: &agentv1.Command_CertInstall{CertInstall: &agentv1.CertInstall{
		CertificateId: "cert1", ChainPem: renewed, KeyPem: renewedKey,
	}}})
	h.waitForStatus(record.Id, schema.CommandSucceeded)
	if _, err := os.Stat(filepath.Join(dir, "certs", file+".crt")); !os.IsNotExist(err) {
		t.Error("the old files of a renewed certificate are left behind")
	}

	record = h.create(host.Id, &agentv1.Command{Payload: &agentv1.Command_CertRemove{CertRemove: &agentv1.CertRemove{CertificateId: "cert1"}}})
	h.waitForStatus(record.Id, schema.CommandSucceeded)
	if entries, _ := os.ReadDir(filepath.Join(dir, "certs")); len(entries) != 0 {
		t.Errorf("files left after cert.remove: %v", entries)
	}

	// a key that does not belong to the certificate is refused
	_, otherKey := selfSigned(t, "other.example.com")
	record = h.create(host.Id, &agentv1.Command{Payload: &agentv1.Command_CertInstall{CertInstall: &agentv1.CertInstall{
		CertificateId: "cert2", ChainPem: chain, KeyPem: otherKey,
	}}})
	if done := h.waitForStatus(record.Id, schema.CommandSucceeded, schema.CommandFailed); done.GetString("status") != schema.CommandFailed {
		t.Error("a key of another certificate was accepted")
	}

	// HTTP-01: the answer is a file the edge serves, and goes again
	record = h.create(host.Id, &agentv1.Command{Payload: &agentv1.Command_AcmeChallengePresent{AcmeChallengePresent: &agentv1.AcmeChallengePresent{
		Token: "tok-EN_1", KeyAuthorization: "tok-EN_1.thumbprint",
	}}})
	h.waitForStatus(record.Id, schema.CommandSucceeded)
	answer := filepath.Join(dir, "acme", ".well-known", "acme-challenge", "tok-EN_1")
	if content, err := os.ReadFile(answer); err != nil || string(content) != "tok-EN_1.thumbprint" {
		t.Errorf("the challenge answer: %q %v", content, err)
	}
	record = h.create(host.Id, &agentv1.Command{Payload: &agentv1.Command_AcmeChallengeCleanup{AcmeChallengeCleanup: &agentv1.AcmeChallengeCleanup{Token: "tok-EN_1"}}})
	h.waitForStatus(record.Id, schema.CommandSucceeded)
	if _, err := os.Stat(answer); !os.IsNotExist(err) {
		t.Error("the challenge answer is left behind")
	}

	for _, token := range []string{"../../certs/x", "a/b", ""} {
		record = h.create(host.Id, &agentv1.Command{Payload: &agentv1.Command_AcmeChallengePresent{AcmeChallengePresent: &agentv1.AcmeChallengePresent{Token: token, KeyAuthorization: "x"}}})
		if done := h.waitForStatus(record.Id, schema.CommandSucceeded, schema.CommandFailed); done.GetString("status") != schema.CommandFailed {
			t.Errorf("the token %q was accepted", token)
		}
	}
}

// TestRemovingTheEdge: an empty config takes the edge away with its keys.
func TestRemovingTheEdge(t *testing.T) {
	h := newHarness(t)
	running, host, fake := h.edgeAgent("edge-10")

	record := h.create(host.Id, edgeApply(t, version.EdgeRef(), 1, oneRoute))
	h.waitForStatus(record.Id, schema.CommandSucceeded)

	record = h.create(host.Id, &agentv1.Command{Payload: &agentv1.Command_EdgeApply{EdgeApply: &agentv1.EdgeApply{}}})
	done := h.waitForStatus(record.Id, schema.CommandSucceeded, schema.CommandFailed)
	if done.GetString("status") != schema.CommandSucceeded {
		t.Fatalf("removing the edge: %s", done.GetString("error"))
	}
	if h.docker.Find("pocketcloud-edge") != nil || fake.server() != nil {
		t.Error("the edge still runs")
	}
	if _, err := os.Stat(filepath.Join(running.config.StateDir, "edge")); !os.IsNotExist(err) {
		t.Error("the edge's directory is left behind")
	}
	h.waitFor("the edge check to be skipped again", func() bool {
		ok, skipped, _ := selfCheck(t, h.host(host.Id), "edge")
		return ok && skipped
	})
}

// selfSigned returns a certificate and its key for a name, valid for a day.
func selfSigned(t *testing.T, name string) (chain, key []byte) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}
