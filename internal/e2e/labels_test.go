package e2e

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// labelsHarness is the ACME harness whose edge agent also runs stacks, with
// the stand-in for docker compose.
type labelsHarness struct {
	*acmeHarness
}

func newLabelsHarness(t *testing.T) *labelsHarness {
	a := newACMEHarness(t, acmeOptions{compose: true})
	a.mustSetAccount()
	return &labelsHarness{acmeHarness: a}
}

func (l *labelsHarness) post(url string, body any) (int, string) {
	l.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		l.t.Fatal(err)
	}
	return testutil.Do(l.t, l.handler, testutil.Request{Method: http.MethodPost, URL: url, Token: l.admin, Body: string(raw)})
}

func (l *labelsHarness) newStack(name, compose string) string {
	l.t.Helper()
	status, body := l.post("/api/collections/stacks/records", map[string]any{"host": l.host.Id, "name": name, "compose_yaml": compose})
	if status != http.StatusOK {
		l.t.Fatalf("creating a stack: %d %s", status, body)
	}
	return recordID(l.t, body)
}

// deploy deploys a stack with a Compose file and waits until the deploy and
// the label sync after it are done.
func (l *labelsHarness) deploy(stackID, compose string) *core.Record {
	l.t.Helper()
	before := l.stackRecord(stackID).GetDateTime("labels_at")
	status, answer := l.post("/api/pc/stacks/"+stackID+"/deploy", map[string]any{"compose_yaml": compose})
	if status != http.StatusOK {
		l.t.Fatalf("deploy: %d %s", status, answer)
	}
	var response struct {
		Command struct {
			ID string `json:"id"`
		} `json:"command"`
	}
	_ = json.Unmarshal([]byte(answer), &response)
	done := l.waitForStatus(response.Command.ID, schema.CommandSucceeded, schema.CommandFailed)
	if done.GetString("status") != schema.CommandSucceeded {
		l.t.Fatalf("the deploy failed: %s", done.GetString("error"))
	}
	// the labels are read once the deploy is recorded
	var stack *core.Record
	l.waitFor("the labels to be read", func() bool {
		stack = l.stackRecord(stackID)
		return stack.GetString("status") == "deployed" && stack.GetDateTime("labels_at").After(before)
	})
	return stack
}

func (l *labelsHarness) stackRecord(id string) *core.Record {
	l.t.Helper()
	record, err := l.app.FindRecordById(schema.Stacks, id)
	if err != nil {
		l.t.Fatal(err)
	}
	return record
}

func (l *labelsHarness) labelDomain(hostname string) *core.Record {
	l.t.Helper()
	var found *core.Record
	l.waitFor("the domain "+hostname, func() bool {
		record, err := l.app.FindFirstRecordByData(schema.Domains, "hostname", hostname)
		if err != nil {
			return false
		}
		found = record
		return true
	})
	return found
}

func problemsOf(stack *core.Record) []string {
	var problems []struct {
		Message string `json:"message"`
	}
	raw, _ := json.Marshal(stack.Get("label_problems"))
	_ = json.Unmarshal(raw, &problems)
	out := []string{}
	for _, problem := range problems {
		out = append(out, problem.Message)
	}
	return out
}

const labelledShop = `services:
  web:
    image: nginx:1
    expose: ["80"]
    labels:
      pocketcloud.domain: shop.example.test
  db:
    image: postgres:17
`

// TestALabelPutsAServiceOnHTTPS: adding the label and deploying exposes the
// service over HTTPS: a domain of the stack's server for the service's
// container, a certificate from the CA, and the edge serving both.
func TestALabelPutsAServiceOnHTTPS(t *testing.T) {
	l := newLabelsHarness(t)
	stackID := l.newStack("shop", labelledShop)
	stack := l.deploy(stackID, labelledShop)
	if problems := problemsOf(stack); len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}

	domain := l.labelDomain("shop.example.test")
	if domain.GetString("origin") != schema.OriginLabel || domain.GetString("stack") != stackID || domain.GetString("service") != "web" ||
		domain.GetString("host") != l.host.Id || domain.GetString("container") != "shop-web-1" || domain.GetInt("port") != 80 ||
		domain.GetString("tls") != schema.TLSAuto || !domain.GetBool("https_redirect") {
		t.Errorf("the domain: %v", domain.PublicExport())
	}

	cert := l.waitCertificate(l.certificateFor("shop.example.test").Id, "to be issued", issued)
	if err := l.ca.Verify([]byte(cert.GetString("chain_pem")), "shop.example.test"); err != nil {
		t.Errorf("the chain: %v", err)
	}
	l.served("shop.example.test", cert)
	route := l.edgeRoute("shop.example.test")
	if route["tls"] != "valid" || route["target"] != "shop-web-1:80" || route["origin"] != schema.OriginLabel || route["service"] != "web" {
		t.Errorf("the route: %v", route)
	}

	// the edge serves it over HTTPS, and sends plain HTTP there
	config := l.edgeConfig(t)
	if !strings.Contains(config, `"dial":"shop-web-1:80"`) || !strings.Contains(config, `"shop.example.test"`) {
		t.Errorf("the edge's config: %s", config)
	}

	// the same labels again change nothing
	changes := func() int64 {
		count, _ := l.app.CountRecords(schema.AuditLog, dbx.HashExp{"record_id": domain.Id})
		return count
	}
	before := changes()
	l.deploy(stackID, labelledShop)
	if changes() != before {
		t.Error("an unchanged label changed its domain")
	}
}

// edgeConfig is the config the fake Caddy of the harness's edge runs.
func (l *labelsHarness) edgeConfig(t *testing.T) string {
	t.Helper()
	var config string
	l.waitFor("the edge to serve its config", func() bool {
		host := l.harness.host(l.host.Id)
		return host.GetString("edge_digest") != "" && host.GetString("edge_digest") == host.GetString("edge_applied_digest")
	})
	commands, err := l.app.FindRecordsByFilter(schema.Commands, "host={:h} && type='edge.apply' && status='succeeded'", "-created", 1, 0, dbx.Params{"h": l.host.Id})
	if err != nil || len(commands) == 0 {
		t.Fatalf("no edge.apply: %v", err)
	}
	var payload struct {
		CaddyJSON string `json:"caddy_json"`
	}
	raw, _ := json.Marshal(commands[0].Get("payload"))
	_ = json.Unmarshal(raw, &payload)
	config = strings.Join(strings.Fields(payload.CaddyJSON), "")
	return config
}

// TestARemovedLabelWaitsForConfirmation: a label that is gone leaves its
// domain in place, marked, until someone removes it or keeps it; the panel's
// routes refuse to change a domain a label owns.
func TestARemovedLabelWaitsForConfirmation(t *testing.T) {
	l := newLabelsHarness(t)
	twoNames := strings.Replace(labelledShop, "shop.example.test", "shop.example.test, www.shop.example.test", 1)
	stackID := l.newStack("shop", twoNames)
	l.deploy(stackID, twoNames)
	shop, www := l.labelDomain("shop.example.test"), l.labelDomain("www.shop.example.test")

	// a domain of a label is changed in the label only
	_, operator := testutil.NewUser(t, l.app, "op@example.com", schema.RoleOperator)
	if status, body := testutil.Do(t, l.handler, testutil.Request{Method: "PATCH", URL: "/api/collections/domains/records/" + shop.Id, Token: operator, Body: `{"port":8080}`}); status != http.StatusBadRequest || !strings.Contains(body, "label") {
		t.Errorf("changing a label's domain: %d %s", status, body)
	}
	if status, body := testutil.Do(t, l.handler, testutil.Request{Method: "DELETE", URL: "/api/collections/domains/records/" + shop.Id, Token: operator}); status != http.StatusBadRequest {
		t.Errorf("removing a label's domain: %d %s", status, body)
	}
	// nor can one be made by hand
	status, body := addDomain(t, l.harness, operator, fmt.Sprintf(`{"hostname":"hand.example.test","host":%q,"target_kind":"redirect","redirect_to":"https://example.org","origin":"label","stack":%q}`, l.host.Id, stackID))
	if status != http.StatusOK {
		t.Fatalf("a domain by hand: %d %s", status, body)
	}
	if hand := l.labelDomain("hand.example.test"); hand.GetString("origin") != "" || hand.GetString("stack") != "" {
		t.Errorf("a domain by hand claims a label: %v", hand.PublicExport())
	}

	// a label with a mistake in it holds its domains as they are: it is not gone
	broken := strings.Replace(twoNames, `expose: ["80"]`, `expose: ["80", "443"]`, 1)
	stack := l.deploy(stackID, broken)
	if problems := strings.Join(problemsOf(stack), "\n"); !strings.Contains(problems, "the service listens on 80 and 443") {
		t.Errorf("problems: %s", problems)
	}
	for _, domain := range []*core.Record{shop, www} {
		held, _ := l.app.FindRecordById(schema.Domains, domain.Id)
		if held.GetBool("label_removed") || held.GetInt("port") != 80 || held.GetString("container") != "shop-web-1" {
			t.Errorf("a broken label changed its domain: %v", held.PublicExport())
		}
	}
	l.deploy(stackID, twoNames)

	// the label loses a name: its domain stays, marked
	l.deploy(stackID, labelledShop)
	l.waitFor("www to be marked", func() bool {
		record, err := l.app.FindRecordById(schema.Domains, www.Id)
		return err == nil && record.GetBool("label_removed")
	})
	if record, _ := l.app.FindRecordById(schema.Domains, shop.Id); record.GetBool("label_removed") {
		t.Error("the name that stays is marked too")
	}
	if route := l.edgeRoute("www.shop.example.test"); route["label_removed"] != true {
		t.Errorf("the route of a removed label: %v", route)
	}
	// changing it is still not the way; removing it is
	if status, _ := testutil.Do(t, l.handler, testutil.Request{Method: "PATCH", URL: "/api/collections/domains/records/" + www.Id, Token: operator, Body: `{"port":8080}`}); status != http.StatusBadRequest {
		t.Errorf("changing a removed label's domain: %d", status)
	}
	if status, body := testutil.Do(t, l.handler, testutil.Request{Method: "DELETE", URL: "/api/collections/domains/records/" + www.Id, Token: operator}); status != http.StatusNoContent {
		t.Errorf("removing it: %d %s", status, body)
	}

	// a stack that is gone leaves the other to be kept as a domain of its own
	if status, body := testutil.Do(t, l.handler, testutil.Request{Method: "POST", URL: "/api/pc/domains/" + shop.Id + "/keep", Token: operator}); status != http.StatusBadRequest {
		t.Errorf("keeping a domain whose label is there: %d %s", status, body)
	}
	if err := l.app.Delete(l.stackRecord(stackID)); err != nil {
		t.Fatal(err)
	}
	l.waitFor("shop to be marked", func() bool {
		record, err := l.app.FindRecordById(schema.Domains, shop.Id)
		return err == nil && record.GetBool("label_removed") && record.GetString("stack") == ""
	})
	if status, body := testutil.Do(t, l.handler, testutil.Request{Method: "POST", URL: "/api/pc/domains/" + shop.Id + "/keep", Token: operator}); status != http.StatusOK {
		t.Fatalf("keep: %d %s", status, body)
	}
	kept, _ := l.app.FindRecordById(schema.Domains, shop.Id)
	if kept.GetString("origin") != "" || kept.GetBool("label_removed") || kept.GetString("container") != "shop-web-1" {
		t.Errorf("kept: %v", kept.PublicExport())
	}
	if status, _ := testutil.Do(t, l.handler, testutil.Request{Method: "PATCH", URL: "/api/collections/domains/records/" + shop.Id, Token: operator, Body: `{"port":8080}`}); status != http.StatusOK {
		t.Errorf("a kept domain cannot be changed: %d", status)
	}
}

// TestWrongLabelsShowOnTheStack: what is wrong with the labels at a deploy
// is on the stack, and a name another domain has is not taken from it.
func TestWrongLabelsShowOnTheStack(t *testing.T) {
	l := newLabelsHarness(t)
	l.domain("taken.example.test")

	compose := `services:
  web:
    image: nginx:1
    labels:
      pocketcloud.domain: web.example.test
  api:
    image: api:1
    expose: ["8080"]
    labels:
      pocketcloud.domain: taken.example.test
`
	stackID := l.newStack("shop", compose)
	stack := l.deploy(stackID, compose)
	problems := strings.Join(problemsOf(stack), "\n")
	if !strings.Contains(problems, "web: set pocketcloud.port") || !strings.Contains(problems, "api: taken.example.test is a domain added by hand already") {
		t.Errorf("problems: %s", problems)
	}
	if taken, _ := l.app.FindFirstRecordByData(schema.Domains, "hostname", "taken.example.test"); taken.GetString("origin") != "" {
		t.Error("a label took a domain added by hand")
	}

	// the problems go once the labels are right
	fixed := strings.Replace(compose, "taken.example.test", "api.example.test", 1)
	fixed = strings.Replace(fixed, "pocketcloud.domain: web.example.test", "pocketcloud.domain: web.example.test\n      pocketcloud.port: \"80\"", 1)
	stack = l.deploy(stackID, fixed)
	if problems := problemsOf(stack); len(problems) != 0 {
		t.Errorf("problems left: %v", problems)
	}
	l.labelDomain("web.example.test")
	l.labelDomain("api.example.test")
}

// TestALabelCanAskForAPassword: pocketcloud.auth puts basic authentication
// in front of the service; the hashes are sealed, and the edge asks for them.
func TestALabelCanAskForAPassword(t *testing.T) {
	l := newLabelsHarness(t)
	compose := `services:
  admin:
    image: admin:1
    expose: ["3000"]
    labels:
      pocketcloud.domain: admin.example.test
      pocketcloud.auth: "bob:$$2a$$05$$6PVNJize1kOg8S8pGtSUw.DfmvfGyAZxXEGMlQNylBUi5z9iaiBRW"
`
	stackID := l.newStack("tools", compose)
	stack := l.deploy(stackID, compose)
	if problems := problemsOf(stack); len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	domain := l.labelDomain("admin.example.test")
	if !secrets.IsSealed(domain.GetString("basic_auth")) {
		t.Error("the passwords are not sealed")
	}
	status, raw := testutil.Do(t, l.handler, testutil.Request{URL: "/api/collections/domains/records/" + domain.Id, Token: l.admin})
	if status != http.StatusOK || strings.Contains(raw, "basic_auth") || strings.Contains(raw, "$2a$") {
		t.Errorf("the API shows the passwords: %s", raw)
	}

	cert := l.waitCertificate(l.certificateFor("admin.example.test").Id, "to be issued", issued)
	l.served("admin.example.test", cert)
	if route := l.edgeRoute("admin.example.test"); route["password"] != true {
		t.Errorf("the route: %v", route)
	}
	config := l.edgeConfig(t)
	if !strings.Contains(config, `"handler":"authentication"`) || !strings.Contains(config, `"username":"bob"`) {
		t.Errorf("no password on the edge: %s", config)
	}
	// the command that carried it holds a reference, not the hash: anyone who
	// may read commands could read it there
	hash := "$2a$05$6PVNJize1kOg8S8pGtSUw.DfmvfGyAZxXEGMlQNylBUi5z9iaiBRW"
	encoded := base64.StdEncoding.EncodeToString([]byte(hash))
	_, viewer := testutil.NewUser(t, l.app, "viewer@example.com", schema.RoleViewer)
	status, raw = testutil.Do(t, l.handler, testutil.Request{URL: "/api/collections/commands/records?filter=type%3D%27edge.apply%27&perPage=200", Token: viewer})
	if status != http.StatusOK || !strings.Contains(raw, "edge.apply") {
		t.Fatalf("commands as a viewer: %d %s", status, raw)
	}
	if strings.Contains(raw, "$2a$") || strings.Contains(raw, encoded) || !strings.Contains(raw, "pocketcloud-secret:") {
		t.Errorf("the stored command shows the hash: %s", raw)
	}
	if strings.Contains(config, "$2a$") || strings.Contains(config, encoded) {
		t.Errorf("the stored config holds the hash: %s", config)
	}
	// while the edge's Caddy was given the hash itself
	l.waitFor("the edge's Caddy to have the hash", func() bool {
		server := l.caddy.server()
		return server != nil && strings.Contains(string(server.Config()), encoded)
	})

	// a hash that is not one is refused at the deploy, with the reason
	broken := strings.Replace(compose, "$$2a$$05$$", "plain", 1)
	stack = l.deploy(stackID, broken)
	if problems := strings.Join(problemsOf(stack), "\n"); !strings.Contains(problems, "not a bcrypt hash") {
		t.Errorf("problems: %s", problems)
	}
	// and the domain keeps the password it had
	held, _ := l.app.FindRecordById(schema.Domains, domain.Id)
	if held.GetString("basic_auth") != domain.GetString("basic_auth") || held.GetBool("label_removed") {
		t.Errorf("a broken label changed its domain: %v", held.PublicExport())
	}

	// kept as a domain of its own, it keeps its password, and with it stays
	// a container behind HTTPS
	withoutLabels := "services:\n  admin:\n    image: admin:1\n"
	l.deploy(stackID, withoutLabels)
	l.waitFor("the domain to be marked", func() bool {
		record, err := l.app.FindRecordById(schema.Domains, domain.Id)
		return err == nil && record.GetBool("label_removed")
	})
	_, operator := testutil.NewUser(t, l.app, "op@example.com", schema.RoleOperator)
	if status, body := testutil.Do(t, l.handler, testutil.Request{Method: "POST", URL: "/api/pc/domains/" + domain.Id + "/keep", Token: operator}); status != http.StatusOK {
		t.Fatalf("keep: %d %s", status, body)
	}
	for body, field := range map[string]string{
		`{"target_kind":"redirect","redirect_to":"https://example.org"}`: "target_kind",
		`{"tls":"off"}`: "tls",
	} {
		status, raw := testutil.Do(t, l.handler, testutil.Request{Method: "PATCH", URL: "/api/collections/domains/records/" + domain.Id, Token: operator, Body: body})
		if status != http.StatusBadRequest || !strings.Contains(raw, `"`+field+`"`) || !strings.Contains(raw, "asks for a password") {
			t.Errorf("%s on a domain with a password: %d %s", body, status, raw)
		}
	}
	if status, body := testutil.Do(t, l.handler, testutil.Request{Method: "PATCH", URL: "/api/collections/domains/records/" + domain.Id, Token: operator, Body: `{"port":3001}`}); status != http.StatusOK {
		t.Errorf("a kept domain with a password cannot be changed: %d %s", status, body)
	}
	if route := l.edgeRoute("admin.example.test"); route["password"] != true {
		t.Errorf("the kept route: %v", route)
	}
}
