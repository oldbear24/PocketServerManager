package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/agent"
	pcdns "github.com/oldbear24/PocketServerManager/internal/dns"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// provisionHarness is the ACME harness with an edge that deploys sites into a
// directory, resolvers that answer what the fake Cloudflare holds, and an
// availability check that answers as a working edge would.
type provisionHarness struct {
	*acmeHarness
	operator string
	viewer   string
	// sites is the edge's sites volume
	sites string

	mu      sync.Mutex
	checks  []string
	answers func(url string) (int, string, error)
}

func newProvisionHarness(t *testing.T, zones ...string) *provisionHarness {
	dir := t.TempDir()
	a := newACMEHarness(t, acmeOptions{configure: func(config *agent.Config) { config.LocalSites = dir }}, zones...)
	a.mustSetAccount()
	a.panel.SetProvisionTick(100 * time.Millisecond)
	_, operator := testutil.NewUser(t, a.app, "op@example.com", schema.RoleOperator)
	_, viewer := testutil.NewUser(t, a.app, "viewer@example.com", schema.RoleViewer)
	h := &provisionHarness{acmeHarness: a, operator: operator, viewer: viewer, sites: dir}
	h.answers = func(url string) (int, string, error) {
		if strings.HasPrefix(url, "https://www.") {
			return http.StatusMovedPermanently, "https://" + strings.TrimPrefix(strings.TrimPrefix(url, "https://www."), "/"), nil
		}
		return http.StatusOK, "", nil
	}
	a.panel.SetSiteCheck(func(_ context.Context, url, address string) (int, string, error) {
		h.mu.Lock()
		h.checks = append(h.checks, url+" @ "+address)
		answer := h.answers
		h.mu.Unlock()
		return answer(url)
	})
	return h
}

// propagateFromCloudflare makes the resolvers answer what the fake
// Cloudflare holds, as they would a moment after the records were set; TXT
// records come from the CA's DNS server, as in the ACME harness.
func (h *provisionHarness) propagateFromCloudflare(zone string) {
	checker := h.resolvers.checker()
	real := pcdns.NewChecker().Lookup
	checker.NameServers = func(context.Context, string) ([]string, error) { return []string{h.ca.DNS.Addr}, nil }
	checker.Lookup = func(ctx context.Context, server, name, kind string) ([]string, error) {
		if kind == "TXT" {
			return real(ctx, h.ca.DNS.Addr, name, kind)
		}
		values := []string{}
		for _, record := range h.api.Records(zone) {
			if record.Type == kind && strings.EqualFold(record.Name, strings.TrimSuffix(name, ".")) {
				values = append(values, record.Content)
			}
		}
		return values, nil
	}
	h.panel.SetDNSChecker(checker)
}

// provision asks for a new site, as the wizard does.
func (h *provisionHarness) provision(site map[string]any, hostname string, www bool) (int, map[string]any) {
	h.t.Helper()
	site["host"] = h.host.Id
	body, _ := json.Marshal(map[string]any{"site": site, "hostname": hostname, "www": www, "challenge": "auto"})
	status, raw := testutil.Do(h.t, h.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/sites/provision", Token: h.operator, Body: string(body)})
	var out map[string]any
	_ = json.Unmarshal([]byte(raw), &out)
	return status, out
}

// steps are a setup's steps as the command history lists them, to a viewer.
func (h *provisionHarness) steps(parent string) []map[string]any {
	h.t.Helper()
	url := "/api/collections/commands/records?sort=created,id&perPage=50&filter=" + urlQuery("parent='"+parent+"'")
	status, raw := testutil.Do(h.t, h.handler, testutil.Request{Method: http.MethodGet, URL: url, Token: h.viewer})
	if status != http.StatusOK {
		h.t.Fatalf("the history: %d %s", status, raw)
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal([]byte(raw), &page)
	return page.Items
}

func stepTypes(steps []map[string]any) string {
	types := make([]string, 0, len(steps))
	for _, step := range steps {
		types = append(types, fmt.Sprintf("%s:%s", step["type"], step["status"]))
	}
	return strings.Join(types, " ")
}

// waitLong waits for a setup's step, which can take a while under the race
// detector: a certificate, twenty checks.
func (h *provisionHarness) waitLong(what string, condition func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (h *provisionHarness) waitSetup(parent string, statuses ...string) *core.Record {
	h.t.Helper()
	var record *core.Record
	ok := false
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		record = h.command(parent)
		for _, status := range statuses {
			if record.GetString("status") == status {
				ok = true
			}
		}
		if ok {
			return record
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("the setup did not reach %v: %v; steps %s", statuses, record.PublicExport(), stepTypes(h.steps(parent)))
	return nil
}

// TestANewSiteIsSetUpThroughCloudflareInUnderTwoMinutes: the wizard's happy
// path. The zone is at Cloudflare: the site, its records and a www alias,
// their propagation, the first deployment, one certificate for both names
// and the availability check run one after the other, each a command in the
// history, in well under two minutes.
func TestANewSiteIsSetUpThroughCloudflareInUnderTwoMinutes(t *testing.T) {
	h := newProvisionHarness(t, "example.com")
	if status, out := h.connect("cf-token"); status != http.StatusOK {
		t.Fatalf("%d %v", status, out)
	}
	h.propagateFromCloudflare("example.com")

	// what the wizard shows while the name is typed
	status, raw := testutil.Do(t, h.handler, testutil.Request{Method: http.MethodGet, Token: h.operator,
		URL: "/api/pc/dns/preview?hostname=Docs.example.com&www=true&host=" + h.host.Id})
	var preview struct {
		Mode     string              `json:"mode"`
		Provider string              `json:"provider"`
		Records  []map[string]string `json:"records"`
	}
	_ = json.Unmarshal([]byte(raw), &preview)
	shown := []string{}
	for _, r := range preview.Records {
		shown = append(shown, r["type"]+" "+r["name"]+" "+r["value"])
	}
	if status != http.StatusOK || preview.Mode != "automatic" || preview.Provider != "Cloudflare" ||
		strings.Join(shown, ", ") != "A docs.example.com 203.0.114.7, AAAA docs.example.com 2a01:4f8::7, CNAME www.docs.example.com docs.example.com" {
		t.Fatalf("the preview: %d %s", status, raw)
	}

	started := time.Now()
	status, out := h.provision(map[string]any{"name": "docs", "source": "empty", "https_redirect": true}, "docs.example.com", true)
	if status != http.StatusCreated {
		t.Fatalf("the new site: %d %v", status, out)
	}
	parent := out["provision"].(string)
	setup := h.waitSetup(parent, schema.CommandSucceeded, schema.CommandFailed, schema.CommandTimedOut)
	took := time.Since(started)
	if setup.GetString("status") != schema.CommandSucceeded {
		t.Fatalf("the setup: %v; steps %s", setup.PublicExport(), stepTypes(h.steps(parent)))
	}
	if took > 2*time.Minute {
		t.Errorf("the setup took %v", took)
	}
	t.Logf("the setup took %v", took.Round(time.Millisecond))

	// every step, in the history, one after the other
	steps := h.steps(parent)
	if got := stepTypes(steps); got != "site.create:succeeded site.dns:succeeded site.propagation:succeeded site.deploy:succeeded site.certificate:succeeded site.check:succeeded" {
		t.Errorf("the steps: %s", got)
	}
	for _, step := range steps {
		if step["correlation_id"] != parent || step["requested_label"] != "op@example.com" {
			t.Errorf("a step: %v", step)
		}
	}
	if dnsStep := steps[1]["result"].(map[string]any); dnsStep["mode"] != "automatic" || dnsStep["provider"] != "Cloudflare" {
		t.Errorf("the DNS step: %v", dnsStep)
	}

	records := cloudflareRecords(h.api, "example.com")
	if records["docs.example.com A"] != "203.0.114.7" || records["docs.example.com AAAA"] != "2a01:4f8::7" ||
		strings.TrimSuffix(records["www.docs.example.com CNAME"], ".") != "docs.example.com" {
		t.Errorf("the zone: %v", records)
	}
	site, err := h.app.FindRecordById(schema.Sites, out["site"].(string))
	if err != nil || site.GetString("active_deployment") == "" {
		t.Fatalf("the site: %v %v", err, site)
	}
	deployments, _ := h.app.FindRecordsByFilter(schema.Deployments, "site={:s}", "", 0, 0, dbx.Params{"s": site.Id})
	if len(deployments) != 1 {
		t.Errorf("%d deployments: the site deployed by itself as well", len(deployments))
	}
	cert := h.certificateFor("docs.example.com", "www.docs.example.com")
	if !issued(cert) || installedOf(cert)[h.host.Id] == "" {
		t.Errorf("the certificate: %v", cert.PublicExport())
	}
	h.mu.Lock()
	checks := strings.Join(h.checks, ", ")
	h.mu.Unlock()
	if !strings.Contains(checks, "https://docs.example.com/ @ 203.0.114.7") || !strings.Contains(checks, "https://www.docs.example.com/ @ 203.0.114.7") {
		t.Errorf("the availability checks: %s", checks)
	}
	if jsonMap(setup.Get("result"))["url"] != "https://docs.example.com" {
		t.Errorf("the setup's result: %v", setup.Get("result"))
	}
	if events, _ := h.app.FindRecordsByFilter(schema.Events, "kind='site.provisioned'", "", 0, 0); len(events) != 1 {
		t.Errorf("%d events", len(events))
	}

	// the steps cannot be asked for through the API, not even by an admin
	if status, raw := testutil.Do(t, h.handler, testutil.Request{Method: http.MethodPost, URL: "/api/collections/commands/records", Token: h.admin,
		Body: fmt.Sprintf(`{"host":%q,"type":"site.check","payload":{}}`, h.host.Id)}); status != http.StatusBadRequest {
		t.Errorf("a step through the API: %d %s", status, raw)
	}
}

// TestTheManualPathWaitsAndContinuesByItself: a zone at no connected provider
// is set by hand. The setup shows the records to make, waits while they do
// not resolve, and goes on by itself once they do.
func TestTheManualPathWaitsAndContinuesByItself(t *testing.T) {
	h := newProvisionHarness(t)
	status, out := h.provision(map[string]any{"name": "manual", "source": "empty"}, "manual.example.org", false)
	if status != http.StatusCreated {
		t.Fatalf("the new site: %d %v", status, out)
	}
	parent := out["provision"].(string)

	var propagation map[string]any
	h.waitLong("the setup to wait for the records", func() bool {
		steps := h.steps(parent)
		if len(steps) < 3 || steps[2]["type"] != "site.propagation" || steps[2]["status"] != schema.CommandRunning {
			return false
		}
		propagation = steps[2]
		progress, _ := propagation["progress"].(map[string]any)
		return progress != nil && strings.Contains(fmt.Sprint(progress["message"]), "manual.example.org")
	})
	steps := h.steps(parent)
	result := steps[1]["result"].(map[string]any)
	if result["mode"] != "manual" || !strings.Contains(fmt.Sprint(result["records"]), "name:manual.example.org") || !strings.Contains(fmt.Sprint(result["records"]), "value:203.0.114.7") {
		t.Fatalf("the records to make: %v", result)
	}
	time.Sleep(time.Second)
	if h.command(parent).GetString("status") != schema.CommandRunning || h.command(propagation["id"].(string)).GetString("status") != schema.CommandRunning {
		t.Fatal("the setup did not wait")
	}

	// someone makes the records at the registrar
	h.resolvers.set("manual.example.org", "A", "203.0.114.7")
	h.resolvers.set("manual.example.org", "AAAA", "2a01:4f8::7")
	setup := h.waitSetup(parent, schema.CommandSucceeded, schema.CommandFailed)
	if setup.GetString("status") != schema.CommandSucceeded {
		t.Fatalf("the setup: %v; steps %s", setup.PublicExport(), stepTypes(h.steps(parent)))
	}
	if got := stepTypes(h.steps(parent)); !strings.HasSuffix(got, "site.certificate:succeeded site.check:succeeded") {
		t.Errorf("the steps: %s", got)
	}
}

// TestAFailedStepIsRetriedAndACancelledSetupRemovesItsRecords: a step that
// fails pauses the setup; Retry runs it again as a new command, and the
// history keeps both. A setup that is cancelled removes the domains it
// created, with their records at the provider, and the site stays.
func TestAFailedStepIsRetriedAndACancelledSetupRemovesItsRecords(t *testing.T) {
	h := newProvisionHarness(t, "example.com")
	if status, out := h.connect("cf-token"); status != http.StatusOK {
		t.Fatalf("%d %v", status, out)
	}
	h.propagateFromCloudflare("example.com")
	h.mu.Lock()
	h.answers = func(string) (int, string, error) { return http.StatusBadGateway, "", nil }
	h.mu.Unlock()

	status, out := h.provision(map[string]any{"name": "flaky", "source": "empty"}, "flaky.example.com", true)
	if status != http.StatusCreated {
		t.Fatalf("the new site: %d %v", status, out)
	}
	parent := out["provision"].(string)
	h.waitLong("the check to fail and pause the setup", func() bool {
		steps := h.steps(parent)
		last := steps[len(steps)-1]
		progress := jsonMap(h.command(parent).Get("progress"))
		return last["type"] == "site.check" && last["status"] == schema.CommandFailed && progress["paused"] == last["id"]
	})
	if setup := h.command(parent); setup.GetString("status") != schema.CommandRunning ||
		time.Until(setup.GetDateTime("expires_at").Time()) < 23*time.Hour {
		t.Fatalf("the paused setup: %v", setup.PublicExport())
	}
	if steps := h.steps(parent); !strings.Contains(fmt.Sprint(steps[len(steps)-1]["error"]), "answered 502") {
		t.Errorf("the failed check: %v", steps[len(steps)-1])
	}

	// the site works now: Retry
	h.mu.Lock()
	h.answers = func(url string) (int, string, error) {
		if strings.HasPrefix(url, "https://www.") {
			return http.StatusMovedPermanently, "https://flaky.example.com/", nil
		}
		return http.StatusOK, "", nil
	}
	h.mu.Unlock()
	if status, raw := testutil.Do(t, h.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/provisions/" + parent + "/retry", Token: h.viewer}); status != http.StatusForbidden {
		t.Errorf("a viewer's retry: %d %s", status, raw)
	}
	if status, raw := testutil.Do(t, h.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/provisions/" + parent + "/retry", Token: h.operator}); status != http.StatusNoContent {
		t.Fatalf("retry: %d %s", status, raw)
	}
	h.waitSetup(parent, schema.CommandSucceeded)
	if got := stepTypes(h.steps(parent)); !strings.HasSuffix(got, "site.check:failed site.check:succeeded") {
		t.Errorf("the history: %s", got)
	}

	// a second site whose setup is cancelled while it waits
	h.mu.Lock()
	h.answers = func(string) (int, string, error) { return 0, "", fmt.Errorf("connection refused") }
	h.mu.Unlock()
	status, out = h.provision(map[string]any{"name": "gone", "source": "empty"}, "gone.example.com", true)
	if status != http.StatusCreated {
		t.Fatalf("the second site: %d %v", status, out)
	}
	second := out["provision"].(string)
	h.waitLong("its records at Cloudflare", func() bool {
		records := cloudflareRecords(h.api, "example.com")
		_, cname := records["www.gone.example.com CNAME"]
		return records["gone.example.com A"] == "203.0.114.7" && cname
	})
	h.waitLong("its check to be under way", func() bool {
		steps := h.steps(second)
		return len(steps) == 6 && steps[5]["status"] == schema.CommandRunning
	})
	if status, raw := testutil.Do(t, h.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/commands/" + second + "/cancel", Token: h.operator}); status != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", status, raw)
	}
	h.waitFor("its domains and records to go", func() bool {
		records := cloudflareRecords(h.api, "example.com")
		_, a := records["gone.example.com A"]
		_, cname := records["www.gone.example.com CNAME"]
		left, _ := h.app.FindRecordsByFilter(schema.Domains, "hostname~'gone.example.com'", "", 0, 0)
		return !a && !cname && len(left) == 0
	})
	if steps := h.steps(second); steps[5]["status"] != schema.CommandCancelled {
		t.Errorf("the step under way: %v", steps[5])
	}
	if _, err := h.app.FindRecordById(schema.Sites, out["site"].(string)); err != nil {
		t.Errorf("the site went too: %v", err)
	}
	// the first site keeps what it has
	if records := cloudflareRecords(h.api, "example.com"); records["flaky.example.com A"] != "203.0.114.7" {
		t.Errorf("the other site's records: %v", records)
	}
	h.waitFor("the event of the ended setup", func() bool {
		events, _ := h.app.FindRecordsByFilter(schema.Events, "kind='site.provision_ended'", "", 0, 0)
		return len(events) == 1 && strings.Contains(events[0].GetString("message"), "gone.example.com")
	})
}

// TestANewSiteFromAZipIsDeployedByItsSetup: a ZIP sent with the new site
// waits, recorded, until the setup's deploy step sends it; the site is not
// deployed before, nor twice.
func TestANewSiteFromAZipIsDeployedByItsSetup(t *testing.T) {
	h := newProvisionHarness(t)
	h.resolvers.set("zip.example.org", "A", "203.0.114.7")
	h.resolvers.set("zip.example.org", "AAAA", "2a01:4f8::7")

	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	file, _ := writer.Create("index.html")
	_, _ = file.Write([]byte("from the zip"))
	_ = writer.Close()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	data, _ := json.Marshal(map[string]any{"site": map[string]any{"name": "zipsite", "host": h.host.Id, "source": "upload"}, "hostname": "zip.example.org"})
	_ = form.WriteField("data", string(data))
	part, _ := form.CreateFormFile("file", "site.zip")
	_, _ = part.Write(archive.Bytes())
	_ = form.Close()

	status, raw := testutil.Do(t, h.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/sites/provision", Token: h.operator, Body: body.String(),
		Headers: map[string]string{"Content-Type": form.FormDataContentType()}})
	if status != http.StatusCreated {
		t.Fatalf("the new site: %d %s", status, raw)
	}
	var out map[string]string
	_ = json.Unmarshal([]byte(raw), &out)
	setup := h.waitSetup(out["provision"], schema.CommandSucceeded, schema.CommandFailed)
	if setup.GetString("status") != schema.CommandSucceeded {
		t.Fatalf("the setup: %v; steps %s", setup.PublicExport(), stepTypes(h.steps(out["provision"])))
	}
	deployments, _ := h.app.FindRecordsByFilter(schema.Deployments, "site={:s}", "", 0, 0, dbx.Params{"s": out["site"]})
	if len(deployments) != 1 || deployments[0].GetString("upload") == "" || deployments[0].GetString("status") != schema.DeploymentSucceeded {
		t.Fatalf("the deployments: %d %v", len(deployments), deployments)
	}
	if served, err := os.ReadFile(filepath.Join(h.sites, "zipsite", "current", "index.html")); err != nil || string(served) != "from the zip" {
		t.Errorf("served: %q %v", served, err)
	}
	steps := h.steps(out["provision"])
	if steps[3]["id"] != deployments[0].GetString("command") {
		t.Errorf("the deploy step is not the deployment's command: %v", steps[3])
	}

	// a ZIP is needed for a site from an upload
	status, raw = testutil.Do(t, h.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/sites/provision", Token: h.operator,
		Body: fmt.Sprintf(`{"site":{"name":"nozip","host":%q,"source":"upload"},"hostname":"nozip.example.org"}`, h.host.Id)})
	if status != http.StatusBadRequest || !strings.Contains(raw, "ZIP") {
		t.Errorf("without its ZIP: %d %s", status, raw)
	}
	if _, err := h.app.FindFirstRecordByData(schema.Sites, "name", "nozip"); err == nil {
		t.Error("the site was created without its ZIP")
	}
}

// jsonMap reads a JSON field of a record.
func jsonMap(value any) map[string]any {
	out := map[string]any{}
	raw, _ := json.Marshal(value)
	_ = json.Unmarshal(raw, &out)
	return out
}

// installedOf reads where a certificate is installed.
func installedOf(cert *core.Record) map[string]string {
	installed := map[string]string{}
	_ = json.Unmarshal([]byte(cert.GetString("installed")), &installed)
	return installed
}
