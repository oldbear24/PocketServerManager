package e2e

import (
	"archive/zip"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/agent"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/sites"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// sitesHarness is a panel with an edge agent whose sites volume is a
// directory, so deployments run without Docker.
type sitesHarness struct {
	*harness
	host     *core.Record
	edge     *fakeEdge
	sites    string
	operator string
	viewer   string
}

func newSitesHarness(t *testing.T) *sitesHarness {
	h := newHarness(t)
	dir := t.TempDir()
	_, host, fake := h.managedEdge("sites-edge", func(config *agent.Config) { config.LocalSites = dir })
	_, operator := testutil.NewUser(t, h.app, "op@example.com", schema.RoleOperator)
	_, viewer := testutil.NewUser(t, h.app, "viewer@example.com", schema.RoleViewer)
	return &sitesHarness{harness: h, host: host, edge: fake, sites: dir, operator: operator, viewer: viewer}
}

func (s *sitesHarness) request(method, url, token string, body any) (int, map[string]any) {
	s.t.Helper()
	raw := ""
	if body != nil {
		encoded, _ := json.Marshal(body)
		raw = string(encoded)
	}
	status, out := testutil.Do(s.t, s.handler, testutil.Request{Method: method, URL: url, Token: token, Body: raw})
	var decoded map[string]any
	_ = json.Unmarshal([]byte(out), &decoded)
	return status, decoded
}

func (s *sitesHarness) newSite(fields map[string]any) string {
	s.t.Helper()
	fields["host"] = s.host.Id
	status, out := s.request(http.MethodPost, "/api/collections/sites/records", s.operator, fields)
	if status != http.StatusOK {
		s.t.Fatalf("a new site: %d %v", status, out)
	}
	return out["id"].(string)
}

func (s *sitesHarness) site(id string) *core.Record {
	s.t.Helper()
	record, err := s.app.FindRecordById(schema.Sites, id)
	if err != nil {
		s.t.Fatal(err)
	}
	return record
}

// deployment waits until deployment n of a site has finished.
func (s *sitesHarness) deployment(siteID string, n int) *core.Record {
	s.t.Helper()
	var found *core.Record
	s.waitFor(fmt.Sprintf("deployment #%d to finish", n), func() bool {
		record, err := s.app.FindFirstRecordByFilter(schema.Deployments, "site={:s} && number={:n}", dbx.Params{"s": siteID, "n": n})
		if err != nil {
			return false
		}
		found = record
		return record.GetString("status") == schema.DeploymentSucceeded || record.GetString("status") == schema.DeploymentFailed
	})
	return found
}

// served is the index page the edge serves for a site.
func (s *sitesHarness) served(site string) string {
	data, _ := os.ReadFile(filepath.Join(sites.CurrentLink(s.sites, site), "index.html"))
	return strings.TrimSpace(string(data))
}

// gitServer is a Git repository served over HTTPS by git http-backend, as a
// Git hosting service would serve it.
type gitServer struct {
	url    string
	commit func(message string, files map[string]string) string
	mu     sync.Mutex
	auth   []string
}

func newGitServer(t *testing.T, files map[string]string) *gitServer {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	git := func(dir string, args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = dir
		command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(root, "init", "-q", "--bare", "-b", "main", "blog.git")
	git(root, "init", "-q", "-b", "main", "work")
	server := &gitServer{}
	server.commit = func(message string, files map[string]string) string {
		for name, content := range files {
			path := filepath.Join(work, name)
			_ = os.MkdirAll(filepath.Dir(path), 0o755)
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		git(work, "add", "-A")
		git(work, "commit", "-q", "-m", message)
		git(work, "push", "-q", filepath.Join(root, "blog.git"), "main")
		return git(work, "rev-parse", "HEAD")
	}
	server.commit("First", files)

	backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	https := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.mu.Lock()
		server.auth = append(server.auth, r.Header.Get("Authorization"))
		server.mu.Unlock()
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(https.Close)
	// git trusts the test server's certificate, and nothing else of the test's
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: https.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", ca)
	server.url = https.URL + "/blog.git"
	return server
}

func (g *gitServer) sawAuthorization(value string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, seen := range g.auth {
		if seen == value {
			return true
		}
	}
	return false
}

func signPush(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestAPushToTheBranchDeploysTheSite: a site from Git is deployed when it is
// made, and again on every push to its branch that its webhook reports; the
// deployments show in its list.
func TestAPushToTheBranchDeploysTheSite(t *testing.T) {
	s := newSitesHarness(t)
	repo := newGitServer(t, map[string]string{"index.html": "one\n"})

	// a private repository: the token comes with the new site, and its first
	// deployment already has it
	siteID := s.newSite(map[string]any{
		"name": "blog", "source": sites.SourceGit, "repo_url": repo.url, "branch": "main", "git_token": "tok-123",
		"generator": sites.GeneratorCustom, "build_image": "alpine:3.22",
		"build_cmd": "mkdir -p public && cp index.html public/ && echo built >> public/index.html", "output_dir": "public",
	})
	first := s.deployment(siteID, 1)
	if !repo.sawAuthorization("Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:tok-123"))) {
		t.Error("the first clone went without the token")
	}
	if first.GetString("status") != schema.DeploymentSucceeded || first.GetString("trigger") != schema.TriggerCreate || first.GetString("message") != "First" || len(first.GetString("commit_sha")) != 40 {
		t.Fatalf("the first deployment: %v", first.PublicExport())
	}
	if got := s.served("blog"); got != "one\nbuilt" {
		t.Errorf("served %q", got)
	}
	s.waitFor("the first deployment to be the active one", func() bool { return s.site(siteID).GetString("active_deployment") == first.Id })

	if s.site(siteID).GetBool("git_token_set") != true {
		t.Error("the site does not say it has a token")
	}

	status, webhook := s.request(http.MethodGet, "/api/pc/sites/"+siteID+"/webhook", s.operator, nil)
	if status != http.StatusOK || !strings.HasSuffix(webhook["url"].(string), "/api/pc/hooks/sites/"+siteID) {
		t.Fatalf("the webhook: %d %v", status, webhook)
	}
	secret := webhook["secret"].(string)
	if status, _ := s.request(http.MethodGet, "/api/pc/sites/"+siteID+"/webhook", s.viewer, nil); status != http.StatusForbidden {
		t.Errorf("a viewer reads the webhook secret: %d", status)
	}

	second := repo.commit("Fix the RSS feed", map[string]string{"index.html": "two\n"})
	push := fmt.Sprintf(`{"ref":"refs/heads/main","after":%q,"head_commit":{"id":%q,"message":"Fix the RSS feed"}}`, second, second)
	hook := func(body, signature string) (int, string) {
		return testutil.Do(t, s.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/hooks/sites/" + siteID, Body: body,
			Headers: map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": signature}})
	}
	if status, out := hook(push, signPush("wrong", push)); status != http.StatusUnauthorized {
		t.Errorf("a wrong signature: %d %s", status, out)
	}
	other := `{"ref":"refs/heads/drafts","after":"abc"}`
	if status, out := hook(other, signPush(secret, other)); status != http.StatusAccepted || !strings.Contains(out, "drafts") {
		t.Errorf("a push to another branch: %d %s", status, out)
	}
	if status, out := hook(push, signPush(secret, push)); status != http.StatusCreated {
		t.Fatalf("the push: %d %s", status, out)
	}

	next := s.deployment(siteID, 2)
	if next.GetString("status") != schema.DeploymentSucceeded || next.GetString("trigger") != schema.TriggerPush || next.GetString("commit_sha") != second || next.GetString("message") != "Fix the RSS feed" {
		t.Fatalf("the pushed deployment: %v", next.PublicExport())
	}
	if got := s.served("blog"); got != "two\nbuilt" {
		t.Errorf("served %q after the push", got)
	}
	if !repo.sawAuthorization("Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:tok-123"))) {
		t.Error("git did not send the token")
	}

	// the list of deployments, for everyone who may see the site
	status, list := s.request(http.MethodGet, "/api/collections/deployments/records?filter="+urlQuery("site='"+siteID+"'")+"&sort=-number", s.viewer, nil)
	if items, _ := list["items"].([]any); status != http.StatusOK || len(items) != 2 || items[0].(map[string]any)["trigger"] != schema.TriggerPush {
		t.Errorf("the deployments: %d %v", status, list)
	}
	// the token is neither in the commands nor in the site
	for _, url := range []string{"/api/collections/commands/records?perPage=200", "/api/collections/sites/records/" + siteID} {
		raw := ""
		_, raw = testutil.Do(t, s.handler, testutil.Request{URL: url, Token: s.viewer})
		if strings.Contains(raw, "tok-123") || strings.Contains(raw, secret) {
			t.Errorf("%s shows a secret: %s", url, raw)
		}
	}

	// a domain of the site: the edge serves its current release
	status, out := s.request(http.MethodPost, "/api/collections/domains/records", s.operator, map[string]any{
		"hostname": "blog.example.test", "host": s.host.Id, "target_kind": schema.TargetSite, "site": siteID, "tls": schema.TLSAuto,
	})
	if status != http.StatusOK {
		t.Fatalf("a domain of the site: %d %v", status, out)
	}
	s.waitFor("the edge to serve the site", func() bool {
		server := s.edge.server()
		return server != nil && strings.Contains(string(server.Config()), `"root":"/srv/sites/blog/current"`)
	})

	// its requests, counted by the edge per hostname
	s.edge.server().ServeHost("blog.example.test", 12)
	s.waitFor("the requests to be counted", func() bool {
		_, requests := s.request(http.MethodGet, "/api/pc/sites/"+siteID+"/requests", s.viewer, nil)
		hours, _ := requests["hours"].([]any)
		return requests["total"] == float64(12) && len(hours) == 24 && hours[23] == float64(12)
	})
}

func urlQuery(filter string) string {
	return strings.NewReplacer(" ", "%20", "'", "%27", "=", "%3D", "&", "%26").Replace(filter)
}

// TestAFailedBuildKeepsTheCurrentReleaseOnline: a deployment whose build
// fails says why, and the site goes on serving the release it served.
func TestAFailedBuildKeepsTheCurrentReleaseOnline(t *testing.T) {
	s := newSitesHarness(t)
	siteID := s.newSite(map[string]any{"name": "shop", "source": sites.SourceUpload})
	s.upload(siteID, map[string]string{"index.html": "the shop"})
	first := s.deployment(siteID, 1)
	if first.GetString("status") != schema.DeploymentSucceeded || s.served("shop") != "the shop" {
		t.Fatalf("the first release: %v %q", first.PublicExport(), s.served("shop"))
	}
	placeholder := s.served("shop")

	// right after the deployment: the edit keeps the live deployment as it
	// is stored, and cannot change it itself
	if status, out := s.request(http.MethodPatch, "/api/collections/sites/records/"+siteID, s.operator, map[string]any{
		"generator": sites.GeneratorCustom, "build_image": "alpine:3.22", "build_cmd": "echo the template is broken >&2; exit 7",
		"active_deployment": "",
	}); status != http.StatusOK {
		t.Fatalf("changing the build: %d %v", status, out)
	}
	if status, out := s.request(http.MethodPost, "/api/pc/sites/"+siteID+"/deploy", s.operator, nil); status != http.StatusOK {
		t.Fatalf("deploy: %d %v", status, out)
	}
	failed := s.deployment(siteID, 2)
	if failed.GetString("status") != schema.DeploymentFailed || !strings.Contains(failed.GetString("error"), "exit code 7") {
		t.Errorf("the failed deployment: %v", failed.PublicExport())
	}
	if served, active := s.served("shop"), s.site(siteID).GetString("active_deployment"); served != placeholder || active != first.Id {
		t.Errorf("a failed build changed what the site serves: %q, active deployment %q (the first is %s)", served, active, first.Id)
	}
	if _, err := os.Stat(sites.BuildDir(s.sites, "shop", 2)); !os.IsNotExist(err) {
		t.Errorf("the failed build left its directory: %v", err)
	}
	// its log has what the build said
	command, _ := s.app.FindRecordById(schema.Commands, failed.GetString("command"))
	if output := s.commandOutput(command); !strings.Contains(output, "the template is broken") || !strings.Contains(output, "still serves the release") {
		t.Errorf("the build log: %s", output)
	}
	// the event is logged once the deployment is marked failed
	s.waitFor("the failure's event", func() bool {
		events, _ := s.app.FindRecordsByFilter(schema.Events, "kind='site.deploy_failed'", "", 0, 0)
		return len(events) == 1
	})
}

// commandOutput is the log a command stored.
func (s *sitesHarness) commandOutput(command *core.Record) string {
	s.t.Helper()
	status, out := testutil.Do(s.t, s.handler, testutil.Request{URL: "/api/pc/commands/" + command.Id + "/output", Token: s.operator})
	if status != http.StatusOK {
		s.t.Fatalf("the output: %d %s", status, out)
	}
	return out
}

// TestARollbackSwitchesAtOnce: going back to a release on the server is a
// switch, without a build; a release that was pruned cannot come back.
func TestARollbackSwitchesAtOnce(t *testing.T) {
	s := newSitesHarness(t)
	siteID := s.newSite(map[string]any{
		"name": "docs", "source": sites.SourceUpload, "keep_releases": 2,
		"generator": sites.GeneratorCustom, "build_image": "alpine:3.22",
		// each release says which deployment built it
		"build_cmd": `mkdir -p out && basename "$(dirname "$PWD")" > out/index.html`, "output_dir": "out",
	})
	s.upload(siteID, map[string]string{"README.md": "the source"})
	s.deployment(siteID, 1)
	for n := 2; n <= 3; n++ {
		if status, out := s.request(http.MethodPost, "/api/pc/sites/"+siteID+"/deploy", s.operator, nil); status != http.StatusOK {
			t.Fatalf("deploy: %d %v", status, out)
		}
		s.deployment(siteID, n)
	}
	if s.served("docs") != "3" {
		t.Fatalf("served %q", s.served("docs"))
	}
	// the older releases are marked once the newest one is live
	s.waitFor("#1 to be pruned", func() bool { return !s.deployment(siteID, 1).GetBool("on_server") })
	one, two := s.deployment(siteID, 1), s.deployment(siteID, 2)
	if !two.GetBool("on_server") {
		t.Error("#2 is not on the server")
	}

	status, out := s.request(http.MethodPost, "/api/pc/sites/"+siteID+"/rollback", s.operator, map[string]any{"deployment": two.Id})
	if status != http.StatusOK {
		t.Fatalf("rollback: %d %v", status, out)
	}
	done := s.waitForStatus(out["command"].(string), schema.CommandSucceeded, schema.CommandFailed)
	var result struct {
		SwitchMs float64 `json:"switch_ms"`
	}
	raw, _ := json.Marshal(done.Get("result"))
	_ = json.Unmarshal(raw, &result)
	if done.GetString("status") != schema.CommandSucceeded || result.SwitchMs >= 1000 {
		t.Errorf("the rollback: %s, %v ms", done.GetString("status"), result.SwitchMs)
	}
	s.waitFor("#2 to be the active deployment", func() bool { return s.site(siteID).GetString("active_deployment") == two.Id })
	if s.served("docs") != "2" {
		t.Errorf("served %q after the rollback", s.served("docs"))
	}

	if status, _ := s.request(http.MethodPost, "/api/pc/sites/"+siteID+"/rollback", s.operator, map[string]any{"deployment": one.Id}); status != http.StatusBadRequest {
		t.Errorf("a pruned release came back: %d", status)
	}
	if status, _ := s.request(http.MethodPost, "/api/pc/sites/"+siteID+"/rollback", s.operator, map[string]any{"deployment": two.Id}); status != http.StatusBadRequest {
		t.Errorf("going back to the served release: %d", status)
	}
	if status, _ := s.request(http.MethodPost, "/api/pc/sites/"+siteID+"/rollback", s.viewer, map[string]any{"deployment": two.Id}); status != http.StatusForbidden {
		t.Errorf("a viewer rolls back: %d", status)
	}
}

// upload deploys a site from a ZIP of files.
func (s *sitesHarness) upload(siteID string, files map[string]string) {
	s.t.Helper()
	body, contentType := zipBody(s.t, files)
	status, out := testutil.Do(s.t, s.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/sites/" + siteID + "/upload", Token: s.operator, Body: body,
		Headers: map[string]string{"Content-Type": contentType}})
	if status != http.StatusOK {
		s.t.Fatalf("upload: %d %s", status, out)
	}
}

func zipBody(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for name, content := range files {
		file, _ := writer.Create(name)
		_, _ = file.Write([]byte(content))
	}
	_ = writer.Close()

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, _ := form.CreateFormFile("file", "site.zip")
	_, _ = part.Write(archive.Bytes())
	_ = form.Close()
	return body.String(), form.FormDataContentType()
}

// TestAnUploadIsDeployed: a ZIP goes to the panel, the agent fetches it and
// deploys it; deploying again uses it again.
func TestAnUploadIsDeployed(t *testing.T) {
	s := newSitesHarness(t)
	siteID := s.newSite(map[string]any{"name": "photos", "source": sites.SourceUpload})
	if status, _ := s.request(http.MethodPost, "/api/pc/sites/"+siteID+"/deploy", s.operator, nil); status != http.StatusBadRequest {
		t.Errorf("deploying before an upload: %d", status)
	}

	upload := func(token, body, contentType string) (int, string) {
		return testutil.Do(t, s.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/sites/" + siteID + "/upload", Token: token, Body: body,
			Headers: map[string]string{"Content-Type": contentType}})
	}
	body, contentType := zipBody(t, map[string]string{"photos/index.html": "from the zip", "photos/img/a.txt": "a"})
	if status, _ := upload(s.viewer, body, contentType); status != http.StatusForbidden {
		t.Errorf("a viewer uploads: %d", status)
	}
	if status, out := upload(s.operator, body, contentType); status != http.StatusOK {
		t.Fatalf("upload: %d %s", status, out)
	}
	first := s.deployment(siteID, 1)
	if first.GetString("status") != schema.DeploymentSucceeded || first.GetString("trigger") != schema.TriggerUpload || first.GetInt("files") != 2 {
		t.Fatalf("the upload's deployment: %v", first.PublicExport())
	}
	if s.served("photos") != "from the zip" {
		t.Errorf("served %q", s.served("photos"))
	}

	if status, out := s.request(http.MethodPost, "/api/pc/sites/"+siteID+"/deploy", s.operator, nil); status != http.StatusOK {
		t.Fatalf("deploying again: %d %v", status, out)
	}
	if again := s.deployment(siteID, 2); again.GetString("status") != schema.DeploymentSucceeded || again.GetString("source") != sites.SourceUpload {
		t.Errorf("deploying the upload again: %v", again.PublicExport())
	}

	// an upload goes with its release, unless it is the newest one
	if status, out := s.request(http.MethodPatch, "/api/collections/sites/records/"+siteID, s.operator, map[string]any{"keep_releases": 1}); status != http.StatusOK {
		t.Fatalf("keep one release: %d %v", status, out)
	}
	body, contentType = zipBody(t, map[string]string{"index.html": "the second zip"})
	if status, out := upload(s.operator, body, contentType); status != http.StatusOK {
		t.Fatalf("a second upload: %d %s", status, out)
	}
	if third := s.deployment(siteID, 3); third.GetString("status") != schema.DeploymentSucceeded || s.served("photos") != "the second zip" {
		t.Fatalf("the second upload: %v", third.PublicExport())
	}
	s.waitFor("the first upload to go with its release", func() bool {
		return s.deployment(siteID, 1).GetString("upload") == ""
	})
	if s.deployment(siteID, 3).GetString("upload") == "" {
		t.Error("the newest upload went too")
	}

	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	part, _ := writer.CreateFormFile("file", "notes.txt")
	_, _ = part.Write([]byte("not a zip"))
	_ = writer.Close()
	if status, _ := upload(s.operator, form.String(), writer.FormDataContentType()); status != http.StatusBadRequest {
		t.Errorf("an upload that is not a ZIP: %d", status)
	}
}

// TestASiteIsCheckedBeforeItIsSaved: what the agent could not use, or what
// would leak a secret, is refused with the reason.
func TestASiteIsCheckedBeforeItIsSaved(t *testing.T) {
	s := newSitesHarness(t)
	plain := testutil.NewHost(t, s.app, "no-edge")
	cases := map[string]map[string]any{
		"an address over plain HTTP": {"name": "a", "source": sites.SourceGit, "repo_url": "http://git.example.com/a.git"},
		"a token in the address":     {"name": "b", "source": sites.SourceGit, "repo_url": "https://user:secret@git.example.com/a.git"},
		"a custom build without one": {"name": "c", "source": sites.SourceEmpty, "generator": sites.GeneratorCustom},
		"an output outside":          {"name": "d", "source": sites.SourceEmpty, "output_dir": "../../etc"},
		"a bad name":                 {"name": "Bad_Name", "source": sites.SourceEmpty},
		"a header with a line break": {"name": "e", "source": sites.SourceEmpty, "headers": []map[string]string{{"name": "X-A", "value": "a\r\nSet-Cookie: x"}}},
	}
	for name, fields := range cases {
		fields["host"] = s.host.Id
		if status, out := s.request(http.MethodPost, "/api/collections/sites/records", s.operator, fields); status != http.StatusBadRequest {
			t.Errorf("%s: %d %v", name, status, out)
		}
	}
	if status, _ := s.request(http.MethodPost, "/api/collections/sites/records", s.operator, map[string]any{"name": "f", "source": sites.SourceEmpty, "host": plain.Id}); status != http.StatusBadRequest {
		t.Errorf("a server without the edge role: %d", status)
	}

	siteID := s.newSite(map[string]any{"name": "kept", "source": sites.SourceEmpty})
	if status, _ := s.request(http.MethodPatch, "/api/collections/sites/records/"+siteID, s.operator, map[string]any{"name": "renamed"}); status != http.StatusBadRequest {
		t.Errorf("a rename: %d", status)
	}
	if status, _ := s.request(http.MethodPut, "/api/pc/sites/"+siteID+"/credentials", s.operator, map[string]any{"basic_auth": "bob:plaintext"}); status != http.StatusBadRequest {
		t.Errorf("a password that is not a hash: %d", status)
	}
	// caddy hash-password prints the hash alone: without a user it would be no password at all
	if status, _ := s.request(http.MethodPut, "/api/pc/sites/"+siteID+"/credentials", s.operator, map[string]any{"basic_auth": bobHash}); status != http.StatusBadRequest {
		t.Errorf("a hash without a user: %d", status)
	}

	// a password needs HTTPS on every domain of the site
	if status, out := s.request(http.MethodPost, "/api/collections/domains/records", s.operator, map[string]any{
		"hostname": "kept.example.test", "host": s.host.Id, "target_kind": schema.TargetSite, "site": siteID, "tls": schema.TLSOff,
	}); status != http.StatusOK {
		t.Fatalf("a plain domain: %d %v", status, out)
	}
	if status, _ := s.request(http.MethodPut, "/api/pc/sites/"+siteID+"/credentials", s.operator, map[string]any{"basic_auth": "bob:" + bobHash}); status != http.StatusBadRequest {
		t.Errorf("a password with a plain HTTP domain: %d", status)
	}
	other := s.newSite(map[string]any{"name": "private", "source": sites.SourceEmpty, "basic_auth": "bob:" + bobHash})
	if s.site(other).GetString("basic_auth") == "" {
		t.Fatal("the password of a new site was not kept")
	}
	if status, _ := s.request(http.MethodPost, "/api/collections/domains/records", s.operator, map[string]any{
		"hostname": "private.example.test", "host": s.host.Id, "target_kind": schema.TargetSite, "site": other, "tls": schema.TLSOff,
	}); status != http.StatusBadRequest {
		t.Errorf("a plain domain for a site with a password: %d", status)
	}

	// the server keeps its edge role while sites are on it
	_, admin := testutil.NewUser(t, s.app, "admin@example.com", schema.RoleAdmin)
	if status, out := s.request(http.MethodPatch, "/api/collections/hosts/records/"+s.host.Id, admin, map[string]any{"roles": []string{schema.HostRoleDocker}}); status != http.StatusBadRequest {
		t.Errorf("removing the edge role from a server with sites: %d %v", status, out)
	}
}

const bobHash = "$2a$05$6PVNJize1kOg8S8pGtSUw.DfmvfGyAZxXEGMlQNylBUi5z9iaiBRW"

// TestRemovingASiteRemovesItsFilesAndDomains: the server loses the site's
// releases, and its domains go.
func TestRemovingASiteRemovesItsFilesAndDomains(t *testing.T) {
	s := newSitesHarness(t)
	siteID := s.newSite(map[string]any{"name": "old", "source": sites.SourceEmpty})
	s.deployment(siteID, 1)
	if status, out := s.request(http.MethodPost, "/api/collections/domains/records", s.operator, map[string]any{
		"hostname": "old.example.test", "host": s.host.Id, "target_kind": schema.TargetSite, "site": siteID, "tls": schema.TLSOff,
	}); status != http.StatusOK {
		t.Fatalf("a domain: %d %v", status, out)
	}

	if status, _ := testutil.Do(t, s.handler, testutil.Request{Method: http.MethodDelete, URL: "/api/collections/sites/records/" + siteID, Token: s.operator}); status != http.StatusNoContent {
		t.Fatalf("remove: %d", status)
	}
	s.waitFor("the site's files to go", func() bool {
		_, err := os.Stat(sites.Dir(s.sites, "old"))
		return os.IsNotExist(err)
	})
	if domains, _ := s.app.FindRecordsByFilter(schema.Domains, "hostname='old.example.test'", "", 0, 0); len(domains) != 0 {
		t.Error("the site's domain stayed")
	}
}
