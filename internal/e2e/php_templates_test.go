package e2e

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/sites"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// fakePHPTools puts php and wp first on the PATH of the builds and release
// hooks, which run on the test's machine: each writes what it was asked to a
// log, and wp stands in for WP-CLI's download, install and update. The log is
// returned.
func fakePHPTools(t *testing.T, password string) func() string {
	t.Helper()
	bin, state := t.TempDir(), t.TempDir()
	log := filepath.Join(t.TempDir(), "calls.log")
	scripts := map[string]string{
		"php": "#!/bin/sh\necho \"php $*\" >> \"$PC_TOOLS_LOG\"\n",
		"wp": `#!/bin/sh
echo "wp $*" >> "$PC_TOOLS_LOG"
case "$1 $2" in
"core download")
	printf '<?php // WordPress\n' > index.php
	printf '<?php\n' > wp-load.php
	mkdir -p wp-includes wp-content/themes
	printf '<?php $wp_version = "6.8";\n' > wp-includes/version.php ;;
"core is-installed") [ -f "$PC_TOOLS_STATE/installed" ] ;;
"core install")
	touch "$PC_TOOLS_STATE/installed"
	echo "installed $POCKETCLOUD_INSTALL_TITLE|$POCKETCLOUD_INSTALL_ADMIN_USER|$POCKETCLOUD_INSTALL_ADMIN_EMAIL|$POCKETCLOUD_INSTALL_LOCALE" >> "$PC_TOOLS_LOG"
	[ "$POCKETCLOUD_INSTALL_ADMIN_PASSWORD" = "$PC_TOOLS_PASSWORD" ] && echo "with the password" >> "$PC_TOOLS_LOG" ;;
"core update") printf '<?php // WordPress, updated\n' > index.php ;;
esac
`,
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil { //nolint:gosec // a test's tool
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PC_TOOLS_LOG", log)
	t.Setenv("PC_TOOLS_STATE", state)
	t.Setenv("PC_TOOLS_PASSWORD", password)
	return func() string {
		data, _ := os.ReadFile(log)
		return string(data)
	}
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for name, content := range files {
		file, _ := writer.Create(name)
		_, _ = file.Write([]byte(content))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

// provisionUpload starts a new site from a ZIP, as the wizard does.
func (h *provisionHarness) provisionUpload(path string, data map[string]any, archive []byte) (int, string) {
	h.t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	encoded, _ := json.Marshal(data)
	_ = form.WriteField("data", string(encoded))
	part, _ := form.CreateFormFile("file", "site.zip")
	_, _ = part.Write(archive)
	_ = form.Close()
	return testutil.Do(h.t, h.handler, testutil.Request{Method: http.MethodPost, URL: path, Token: h.operator, Body: body.String(),
		Headers: map[string]string{"Content-Type": form.FormDataContentType()}})
}

func (h *provisionHarness) send(method, url string, body any) (int, map[string]any) {
	h.t.Helper()
	encoded, _ := json.Marshal(body)
	status, raw := testutil.Do(h.t, h.handler, testutil.Request{Method: method, URL: url, Token: h.operator, Body: string(encoded)})
	var out map[string]any
	_ = json.Unmarshal([]byte(raw), &out)
	if out == nil {
		out = map[string]any{"raw": raw}
	}
	return status, out
}

func (h *provisionHarness) deploymentNumbered(siteID string, n int, statuses ...string) *core.Record {
	h.t.Helper()
	var deployment *core.Record
	h.waitFor("the deployment to finish", func() bool {
		found, err := h.app.FindFirstRecordByFilter(schema.Deployments, "site={:s} && number={:n}", dbx.Params{"s": siteID, "n": n})
		deployment = found
		return err == nil && slices.Contains(statuses, found.GetString("status"))
	})
	return deployment
}

func (h *provisionHarness) templateSite(name string) (*core.Record, *core.Record) {
	h.t.Helper()
	site, err := h.app.FindFirstRecordByData(schema.Sites, "name", name)
	if err != nil {
		h.t.Fatal(err)
	}
	settings, err := h.app.FindFirstRecordByData(schema.SitePHP, "site", site.Id)
	if err != nil {
		h.t.Fatal(err)
	}
	return site, settings
}

// TestALaravelSiteIsSetUpWithItsTemplate (#52): a new site made with the
// Laravel template works on its first release: its web root, its storage
// shared and linked into public/, its .env written from the panel's
// environment with a new APP_KEY, migrations and caches in the release hook,
// the scheduler, the queue worker, and its health check at /up. A change of
// its environment deploys the live release again, as Laravel caches it.
func TestALaravelSiteIsSetUpWithItsTemplate(t *testing.T) {
	h := newProvisionHarness(t)
	calls := fakePHPTools(t, "")
	h.resolvers.set("shop.example.org", "A", "203.0.114.7")
	h.resolvers.set("shop.example.org", "AAAA", "2a01:4f8::7")

	archive := zipOf(t, map[string]string{
		"shop/artisan": "#!/usr/bin/env php", "shop/public/index.php": "<?php echo 'shop';",
		"shop/storage/app/public/logo.png": "png", "shop/.env": "APP_ENV=local\nAPP_DEBUG=true",
	})

	// the wizard asks which application it is
	status, raw := h.provisionUpload("/api/pc/sites/detect", map[string]any{"source": "upload"}, archive)
	if status != http.StatusOK || !strings.Contains(raw, `"template":"laravel"`) || !strings.Contains(raw, `"title":"Laravel"`) {
		t.Fatalf("detect: %d %s", status, raw)
	}

	site := map[string]any{"name": "shop", "host": h.host.Id, "type": schema.SitePHPApp, "source": sites.SourceUpload}
	// a Laravel site comes from a source of its own
	if status, out := h.send(http.MethodPost, "/api/pc/sites/provision", map[string]any{
		"site":     map[string]any{"name": "shop", "host": h.host.Id, "type": schema.SitePHPApp, "source": sites.SourceEmpty},
		"hostname": "shop.example.org", "php": map[string]any{"template": "laravel"},
	}); status != http.StatusBadRequest {
		t.Errorf("Laravel without a source: %d %v", status, out)
	}
	status, raw = h.provisionUpload("/api/pc/sites/provision", map[string]any{
		"site": site, "hostname": "shop.example.org",
		"php": map[string]any{"template": "laravel", "queue_worker": true},
	}, archive)
	if status != http.StatusCreated {
		t.Fatalf("the new site: %d %s", status, raw)
	}
	var out map[string]string
	_ = json.Unmarshal([]byte(raw), &out)
	setup := h.waitSetup(out["provision"], schema.CommandSucceeded, schema.CommandFailed)
	if setup.GetString("status") != schema.CommandSucceeded {
		t.Fatalf("the setup: %v; steps %s", setup.PublicExport(), stepTypes(h.steps(out["provision"])))
	}

	record, settings := h.templateSite("shop")
	env := settings.GetString("env")
	for _, want := range []string{"APP_ENV=production", "APP_URL=https://shop.example.org", "APP_KEY=${secret:app-key}", "LOG_CHANNEL=stderr", "SESSION_DRIVER=file", "QUEUE_CONNECTION=sync"} {
		if !strings.Contains(env, want) {
			t.Errorf("the environment lacks %s:\n%s", want, env)
		}
	}
	if settings.GetString("docroot") != "public" || settings.GetString("health_path") != "/up" || !settings.GetBool("queue_worker") ||
		!slices.Equal(settings.GetStringSlice("shared_paths"), []string{"storage"}) || !slices.Equal(settings.GetStringSlice("secret_names"), []string{"app-key"}) ||
		!strings.Contains(settings.GetString("release_hook"), "php artisan optimize") || !strings.Contains(settings.GetString("php_ini"), "memory_limit = 256M") {
		t.Errorf("the settings: %v", settings.PublicExport())
	}
	var excluded []string
	raw2, _ := json.Marshal(settings.Get("backup_exclude"))
	_ = json.Unmarshal(raw2, &excluded)
	if !slices.Contains(excluded, "storage/framework/cache") {
		t.Errorf("backups leave out %v", excluded)
	}

	// the availability check asked Laravel's health route
	h.mu.Lock()
	checks := strings.Join(h.checks, "\n")
	h.mu.Unlock()
	if !strings.Contains(checks, "https://shop.example.org/up @") {
		t.Errorf("checks: %s", checks)
	}

	// the release: storage linked, .env the panel's, the hook run
	current := sites.CurrentLink(h.sites, "shop")
	if logo, err := os.ReadFile(filepath.Join(current, "public", "storage", "logo.png")); err != nil || string(logo) != "png" {
		t.Errorf("public/storage: %q %v", logo, err)
	}
	dotenv, _ := os.ReadFile(filepath.Join(current, ".env"))
	if !strings.Contains(string(dotenv), `APP_KEY="base64:`) || !strings.Contains(string(dotenv), `APP_URL="https://shop.example.org"`) ||
		strings.Contains(string(dotenv), "APP_DEBUG=true") || strings.Contains(string(dotenv), "POCKETCLOUD_") {
		t.Errorf(".env:\n%s", dotenv)
	}
	if log := calls(); !strings.Contains(log, "php artisan optimize") || strings.Contains(log, "migrate") {
		t.Errorf("the release hook ran %q", log)
	}

	// the worker: the same image, in the live release, no alias of PHP-FPM's
	worker := h.docker.Find("shop-worker")
	if worker == nil {
		t.Fatal("no worker")
	}
	if cmd, _ := json.Marshal(worker.Config["Cmd"]); !strings.Contains(string(cmd), "exec php artisan queue:work") ||
		worker.Config["WorkingDir"] != "/srv/sites/shop/current" || worker.Labels["pocketcloud.role"] != "site-worker" ||
		worker.Config["Image"] != h.docker.Find("shop-php").Config["Image"] {
		t.Errorf("the worker: %v %v", worker.Config, worker.Labels)
	}
	if network := h.docker.FindNetwork(sites.Network("shop")); network == nil {
		t.Error("no site network")
	}

	// the scheduler, once the first release was live
	var task *core.Record
	h.waitFor("the scheduler's task", func() bool {
		found, err := h.app.FindFirstRecordByFilter(schema.ScheduledTasks, "site={:s}", dbx.Params{"s": record.Id})
		task = found
		return err == nil
	})
	if task.GetString("schedule") != "* * * * *" || task.GetString("command") != "php artisan schedule:run" || !task.GetBool("enabled") {
		t.Errorf("the task: %v", task.PublicExport())
	}

	// a change of the environment: a copy of the live release, with it
	status, applied := h.send(http.MethodPut, "/api/pc/sites/"+record.Id+"/php", map[string]any{"env": env + "\nMAIL_MAILER=log"})
	if status != http.StatusOK || applied["deployment"] == nil {
		t.Fatalf("the environment: %d %v", status, applied)
	}
	two := h.deploymentNumbered(record.Id, 2, schema.DeploymentSucceeded, schema.DeploymentFailed)
	if two.GetString("status") != schema.DeploymentSucceeded || two.GetInt("base_release") != 1 {
		t.Fatalf("deployment 2: %v", two.PublicExport())
	}
	if sites.Current(h.sites, "shop") != 2 {
		t.Error("release 2 is not live")
	}
	if dotenv, _ := os.ReadFile(filepath.Join(current, ".env")); !strings.Contains(string(dotenv), `MAIL_MAILER="log"`) {
		t.Errorf(".env of release 2:\n%s", dotenv)
	}
	command, _ := h.app.FindRecordById(schema.Commands, two.GetString("command"))
	if payload, _ := json.Marshal(command.Get("payload")); !strings.Contains(string(payload), `"from_release":1`) || !strings.Contains(string(payload), `"apply"`) {
		t.Errorf("the redeploy: %s", payload)
	}
	h.waitFor("the worker to start in release 2", func() bool { return h.docker.Called("restart") || h.docker.Find("shop-worker") != nil })

	// the worker goes when the settings say so
	status, applied = h.send(http.MethodPut, "/api/pc/sites/"+record.Id+"/php", map[string]any{"queue_worker": false})
	if status != http.StatusOK || applied["command"] == nil {
		t.Fatalf("no worker: %d %v", status, applied)
	}
	h.waitFor("the worker to go", func() bool { return h.docker.Find("shop-worker") == nil })
	// and a template without one gets none
	if status, out := h.send(http.MethodPut, "/api/pc/sites/"+record.Id+"/php", map[string]any{"template": "nette", "queue_worker": true}); status != http.StatusBadRequest {
		t.Errorf("a worker for Nette: %d %v", status, out)
	}
}

// TestAFreshWordPressIsInstalledUpdatedAndRolledBack (#52): a WordPress made
// without a source of its own is downloaded by WP-CLI in the language chosen
// and installed by its release hook with the administrator the wizard asked
// for, whose password is a secret of the site until then; WordPress reads
// its settings from the environment through the template's wp-config.php.
// "Update WordPress" builds a copy of the live release with the updates; one
// that breaks the site is seen by the health check and rolled back with one
// click.
func TestAFreshWordPressIsInstalledUpdatedAndRolledBack(t *testing.T) {
	const password = "correct horse battery staple"
	h := newProvisionHarness(t)
	calls := fakePHPTools(t, password)
	h.resolvers.set("blog.example.org", "A", "203.0.114.7")
	h.resolvers.set("blog.example.org", "AAAA", "2a01:4f8::7")
	var broken atomic.Bool
	h.mu.Lock()
	answers := h.answers
	h.answers = func(url string) (int, string, error) {
		if broken.Load() && strings.HasPrefix(url, "https://blog.example.org/") {
			return http.StatusInternalServerError, "", nil
		}
		return answers(url)
	}
	h.mu.Unlock()
	server := h.databaseServer("db-main")

	start := func(php map[string]any) (int, map[string]any) {
		return h.send(http.MethodPost, "/api/pc/sites/provision", map[string]any{
			"site":     map[string]any{"name": "blog", "host": h.host.Id, "type": schema.SitePHPApp, "source": sites.SourceEmpty},
			"hostname": "blog.example.org", "php": php,
		})
	}
	install := map[string]any{"title": "Můj blog", "admin_user": "editor", "admin_email": "editor@example.org", "admin_password": password, "locale": "cs_CZ"}
	for name, php := range map[string]map[string]any{
		"no database":    {"template": "wordpress", "install": install},
		"no install":     {"template": "wordpress", "database_server": server.Id},
		"short password": {"template": "wordpress", "database_server": server.Id, "install": map[string]any{"title": "x", "admin_user": "a", "admin_email": "a@example.org", "admin_password": "short"}},
		"bad locale":     {"template": "wordpress", "database_server": server.Id, "install": map[string]any{"title": "x", "admin_user": "a", "admin_email": "a@example.org", "admin_password": password, "locale": "cs; rm"}},
		"not fresh":      {"template": "nette", "database_server": server.Id, "install": install},
	} {
		if status, out := start(php); status != http.StatusBadRequest {
			t.Errorf("%s: %d %v", name, status, out)
		}
	}
	status, out := start(map[string]any{"template": "wordpress", "database_server": server.Id, "install": install})
	if status != http.StatusCreated {
		t.Fatalf("the new site: %d %v", status, out)
	}
	parent := out["provision"].(string)
	setup := h.waitSetup(parent, schema.CommandSucceeded, schema.CommandFailed)
	if setup.GetString("status") != schema.CommandSucceeded {
		t.Fatalf("the setup: %v; steps %s", setup.PublicExport(), stepTypes(h.steps(parent)))
	}
	if got := stepTypes(h.steps(parent)); !strings.Contains(got, "db.database.create:succeeded site.php.apply:succeeded site.deploy:succeeded") {
		t.Errorf("steps: %s", got)
	}

	site, settings := h.templateSite("blog")
	if site.GetString("build_cmd") != "wp core download --locale=cs_CZ" {
		t.Errorf("build: %q", site.GetString("build_cmd"))
	}
	log := calls()
	for _, want := range []string{"wp core download --locale=cs_CZ", "wp core is-installed", "installed Můj blog|editor|editor@example.org|cs_CZ", "with the password"} {
		if !strings.Contains(log, want) {
			t.Errorf("WP-CLI was not asked %q:\n%s", want, log)
		}
	}
	// the install is done: its password is no secret of the site's any more
	if names := settings.GetStringSlice("secret_names"); !slices.Equal(names, []string{"wp-salt"}) || settings.Get("install") != nil && string(mustJSON(settings.Get("install"))) != "null" {
		t.Errorf("after the install: %v %v", names, settings.Get("install"))
	}
	env := settings.GetString("env")
	for _, want := range []string{"WP_HOME=https://blog.example.org", "WP_SALT=${secret:wp-salt}", "DB_HOST=db-main", "DB_NAME=blog", "DB_PASSWORD=${secret:db/blog}"} {
		if !strings.Contains(env, want) {
			t.Errorf("the environment lacks %s:\n%s", want, env)
		}
	}
	current := sites.CurrentLink(h.sites, "blog")
	if config, _ := os.ReadFile(filepath.Join(current, "wp-config.php")); !strings.Contains(string(config), "DISALLOW_FILE_MODS") {
		t.Errorf("wp-config.php: %s", config)
	}
	if _, err := os.Lstat(filepath.Join(current, "index.html")); err == nil {
		t.Error("a placeholder was left in WordPress")
	}
	if target, err := os.Readlink(filepath.Join(current, "wp-content", "uploads")); err != nil || target != "../../../shared/wp-content/uploads" {
		t.Errorf("uploads: %q %v", target, err)
	}
	// the password is nowhere in what the panel keeps or shows
	commandsKept, _ := h.app.FindAllRecords(schema.Commands)
	for _, command := range commandsKept {
		if strings.Contains(string(mustJSON(command.PublicExport())), password) {
			t.Errorf("the command %s holds the password", command.GetString("type"))
		}
	}
	// WordPress's cron, from the panel
	h.waitFor("WordPress's cron", func() bool {
		found, err := h.app.FindFirstRecordByFilter(schema.ScheduledTasks, "site={:s} && command='wp cron event run --due-now'", dbx.Params{"s": site.Id})
		return err == nil && found.GetString("schedule") == "*/5 * * * *"
	})

	// deploying again builds a copy of the live release: WordPress is not
	// downloaded anew
	if status, out := h.send(http.MethodPost, "/api/pc/sites/"+site.Id+"/deploy", nil); status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("deploy again: %d %v", status, out)
	}
	two := h.deploymentNumbered(site.Id, 2, schema.DeploymentSucceeded, schema.DeploymentFailed)
	if two.GetString("status") != schema.DeploymentSucceeded || two.GetInt("base_release") != 1 || strings.Count(calls(), "core download") != 1 {
		t.Fatalf("deployment 2: %v\n%s", two.PublicExport(), calls())
	}
	if health := mustJSON(two.Get("health")); !strings.Contains(string(health), `"ok":true`) {
		h.waitFor("the health of release 2", func() bool {
			found, _ := h.app.FindRecordById(schema.Deployments, two.Id)
			return strings.Contains(string(mustJSON(found.Get("health"))), `"ok":true`)
		})
	}

	// an update with a plugin to add
	for _, bad := range []map[string]any{{"plugins": []string{"rm -rf /"}}, {"themes": []string{"x'y"}}} {
		if status, out := h.send(http.MethodPost, "/api/pc/sites/"+site.Id+"/php/update", bad); status != http.StatusBadRequest {
			t.Errorf("%v: %d %v", bad, status, out)
		}
	}
	broken.Store(true)
	status, out = h.send(http.MethodPost, "/api/pc/sites/"+site.Id+"/php/update", map[string]any{"plugins": []string{"akismet", "https://example.org/plugin.zip"}})
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, out)
	}
	three := h.deploymentNumbered(site.Id, 3, schema.DeploymentSucceeded, schema.DeploymentFailed)
	if three.GetString("status") != schema.DeploymentSucceeded || three.GetString("trigger") != schema.TriggerUpdate || three.GetInt("base_release") != 2 {
		t.Fatalf("the update: %v", three.PublicExport())
	}
	log = calls()
	for _, want := range []string{"wp core update", "wp plugin update --all", "wp theme update --all", "wp plugin install --activate akismet https://example.org/plugin.zip", "wp core update-db"} {
		if !strings.Contains(log, want) {
			t.Errorf("the update did not run %q:\n%s", want, log)
		}
	}
	if index, _ := os.ReadFile(filepath.Join(sites.ReleaseDir(h.sites, "blog", 3), "index.php")); !strings.Contains(string(index), "updated") {
		t.Errorf("release 3: %q", index)
	}
	if index, _ := os.ReadFile(filepath.Join(sites.ReleaseDir(h.sites, "blog", 2), "index.php")); strings.Contains(string(index), "updated") {
		t.Error("the update changed release 2")
	}
	// the site breaks: the health check says so, once it gave up asking
	h.waitFor("the health check to fail", func() bool {
		found, _ := h.app.FindRecordById(schema.Deployments, three.Id)
		return strings.Contains(string(mustJSON(found.Get("health"))), `"ok":false`)
	})
	if events, _ := h.app.FindRecordsByFilter(schema.Events, "kind='site.unhealthy'", "", 0, 0); len(events) != 1 {
		t.Errorf("unhealthy events: %d", len(events))
	}
	// one click back
	broken.Store(false)
	if status, out := h.send(http.MethodPost, "/api/pc/sites/"+site.Id+"/rollback", map[string]any{"deployment": two.Id}); status != http.StatusOK {
		t.Fatalf("rollback: %d %v", status, out)
	}
	h.waitFor("release 2 to be live and answer", func() bool {
		found, _ := h.app.FindRecordById(schema.Deployments, two.Id)
		live, _ := h.app.FindRecordById(schema.Sites, site.Id)
		return sites.Current(h.sites, "blog") == 2 && live.GetString("active_deployment") == two.Id &&
			strings.Contains(string(mustJSON(found.Get("health"))), `"ok":true`)
	})
}

// TestANetteSiteEmptiesItsCacheAtEachRelease (#52): Nette's web root is www/,
// log/ and temp/ are shared, temp/ is left out of backups and emptied once a
// new release is live; a repository is recognised as Nette by its files.
func TestANetteSiteEmptiesItsCacheAtEachRelease(t *testing.T) {
	h := newProvisionHarness(t)
	fakePHPTools(t, "")
	h.resolvers.set("news.example.org", "A", "203.0.114.7")
	h.resolvers.set("news.example.org", "AAAA", "2a01:4f8::7")
	files := map[string]string{"app/Bootstrap.php": "<?php", "www/index.php": "<?php echo 'news';", "temp/.gitignore": "*\n!.gitignore"}

	// a repository is looked at on the server, without its files
	git := newGitServer(t, files)
	status, out := h.send(http.MethodPost, "/api/pc/sites/detect", map[string]any{"source": "git", "host": h.host.Id, "repo_url": git.url, "branch": "main"})
	if status != http.StatusOK || out["template"] != "nette" {
		t.Fatalf("detect: %d %v", status, out)
	}
	if status, out := h.send(http.MethodPost, "/api/pc/sites/detect", map[string]any{"source": "git", "host": h.host.Id, "repo_url": "ftp://x", "branch": "main"}); status != http.StatusBadRequest {
		t.Errorf("an ftp repository: %d %v", status, out)
	}
	if status, out := h.send(http.MethodPost, "/api/pc/sites/detect", map[string]any{"source": "git", "host": h.host.Id, "repo_url": git.url, "branch": "--upload-pack=x"}); status != http.StatusBadRequest {
		t.Errorf("a branch that is an option: %d %v", status, out)
	}
	looks, _ := h.app.FindRecordsByFilter(schema.Commands, "type='site.detect'", "", 0, 0)
	if len(looks) != 0 {
		t.Errorf("%d looks are kept", len(looks))
	}

	archive := zipOf(t, files)
	status, raw := h.provisionUpload("/api/pc/sites/provision", map[string]any{
		"site":     map[string]any{"name": "news", "host": h.host.Id, "type": schema.SitePHPApp, "source": sites.SourceUpload},
		"hostname": "news.example.org", "php": map[string]any{"template": "nette"},
	}, archive)
	if status != http.StatusCreated {
		t.Fatalf("the new site: %d %s", status, raw)
	}
	var started map[string]string
	_ = json.Unmarshal([]byte(raw), &started)
	if setup := h.waitSetup(started["provision"], schema.CommandSucceeded, schema.CommandFailed); setup.GetString("status") != schema.CommandSucceeded {
		t.Fatalf("the setup: %v", setup.PublicExport())
	}
	site, settings := h.templateSite("news")
	if settings.GetString("docroot") != "www" || !slices.Equal(settings.GetStringSlice("shared_paths"), []string{"log", "temp"}) {
		t.Errorf("settings: %v", settings.PublicExport())
	}
	if excluded := string(mustJSON(settings.Get("backup_exclude"))); excluded != `["temp"]` {
		t.Errorf("backups leave out %s", excluded)
	}
	cache := filepath.Join(sites.SharedDir(h.sites, "news"), "temp", "cache", "latte")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(cache, "page.php"), []byte("<?php"), 0o644)

	status, raw = h.provisionUpload("/api/pc/sites/"+site.Id+"/upload", nil, archive)
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("upload: %d %s", status, raw)
	}
	if two := h.deploymentNumbered(site.Id, 2, schema.DeploymentSucceeded, schema.DeploymentFailed); two.GetString("status") != schema.DeploymentSucceeded {
		t.Fatalf("deployment 2: %v", two.PublicExport())
	}
	entries, _ := os.ReadDir(filepath.Join(sites.SharedDir(h.sites, "news"), "temp"))
	if len(entries) != 1 || entries[0].Name() != ".gitignore" {
		t.Errorf("temp keeps %v", entries)
	}
}

// databaseServer is a shared database server of the harness's host that the
// fake engine answers for.
func (h *provisionHarness) databaseServer(name string) *core.Record {
	h.t.Helper()
	container := h.docker.AddContainer("pocketcloud-"+name, "mariadb:11.8", map[string]string{"pocketcloud.role": "db"})
	database := &fakeDatabase{}
	h.docker.OnExec = func(call *fakedocker.ExecCall, input []byte) fakedocker.ExecResult {
		if call.Container == container || call.Container == "pocketcloud-"+name {
			return database.answer(call, input)
		}
		return fakedocker.ExecResult{}
	}
	collection, err := h.app.FindCollectionByNameOrId(schema.DBServers)
	if err != nil {
		h.t.Fatal(err)
	}
	server := core.NewRecord(collection)
	for field, value := range map[string]any{
		"host": h.host.Id, "name": name, "engine": schema.EngineMariaDB, "kind": schema.DBServerShared, "version": "11.8",
		"container_ref": container, "port": 3306, "admin_user": "root", "admin_password": "rootpw", "status": "ready",
	} {
		server.Set(field, value)
	}
	if err := h.app.Save(server); err != nil {
		h.t.Fatal(err)
	}
	return server
}

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}
