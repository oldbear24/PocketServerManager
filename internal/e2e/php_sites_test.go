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
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/php"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/sites"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// phpSettings is a PHP site's settings record.
func (s *sitesHarness) phpSettings(siteID string) *core.Record {
	s.t.Helper()
	record, err := s.app.FindFirstRecordByData(schema.SitePHP, "site", siteID)
	if err != nil {
		s.t.Fatal(err)
	}
	return record
}

// appliedPHP waits for the newest site.php.apply of a site to succeed.
func (s *sitesHarness) appliedPHP(out map[string]any) {
	s.t.Helper()
	id, _ := out["command"].(string)
	if id == "" {
		s.t.Fatalf("no command: %v", out)
	}
	if done := s.waitForStatus(id, schema.CommandSucceeded, schema.CommandFailed); done.GetString("status") != schema.CommandSucceeded {
		s.t.Fatalf("site.php.apply: %s %s", done.GetString("error"), s.commandOutput(done))
	}
}

// TestAPHPSiteRunsInAPHPFPMContainerOfItsOwn: a PHP site (#50) gets a PHP-FPM
// container that sees its own site only, read-only but for shared/, on a
// network of its own with the edge; its releases keep their shared paths and
// run the release hook with the site's secrets before they go live, and
// PHP-FPM reloads after each switch. A new PHP version deploys the release
// again on its image, and a rollback goes back to the old one.
func TestAPHPSiteRunsInAPHPFPMContainerOfItsOwn(t *testing.T) {
	s := newSitesHarness(t)
	siteID := s.newSite(map[string]any{"name": "shop", "type": schema.SitePHPApp, "source": sites.SourceUpload})
	settings := s.phpSettings(siteID)
	first := settings.GetString("php_version")
	if _, found := php.Find(first); !found || settings.GetString("mode") != schema.PHPReleases {
		t.Fatalf("the defaults: %v", settings.PublicExport())
	}

	// an environment that refers to a secret the site does not have is refused
	if status, out := s.request(http.MethodPut, "/api/pc/sites/"+siteID+"/php", s.operator, map[string]any{"env": "APP_KEY=${secret:app-key}"}); status != http.StatusBadRequest {
		t.Errorf("an unknown secret: %d %v", status, out)
	}
	const secret = "base64:sekrit-value-1234"
	status, out := s.request(http.MethodPut, "/api/pc/sites/"+siteID+"/php/secrets", s.operator, map[string]any{"app-key": secret})
	if status != http.StatusOK {
		t.Fatalf("secrets: %d %v", status, out)
	}
	s.appliedPHP(out)
	status, out = s.request(http.MethodPut, "/api/pc/sites/"+siteID+"/php", s.operator, map[string]any{
		"docroot":      "public",
		"env":          "APP_ENV=production\nAPP_KEY=${secret:app-key}",
		"php_ini":      "memory_limit = 256M",
		"shared_paths": []string{"storage"},
		"release_hook": `printf '%s' "$APP_KEY" > hook.txt`,
	})
	if status != http.StatusOK {
		t.Fatalf("settings: %d %v", status, out)
	}
	s.appliedPHP(out)
	if status, _ := s.request(http.MethodPut, "/api/pc/sites/"+siteID+"/php", s.viewer, map[string]any{"php_ini": ""}); status != http.StatusForbidden {
		t.Errorf("a viewer changed the settings: %d", status)
	}
	for _, bad := range []map[string]any{
		{"docroot": "../other"}, {"shared_paths": []string{"../../etc"}}, {"php_ini": "[PHP]"},
		{"php_version": "5.6"}, {"fpm": map[string]any{"pm": "dynamic", "max_children": 0}},
	} {
		if status, out := s.request(http.MethodPut, "/api/pc/sites/"+siteID+"/php", s.operator, bad); status != http.StatusBadRequest {
			t.Errorf("%v was taken: %d %v", bad, status, out)
		}
	}

	container := s.docker.Find("shop-php")
	if container == nil {
		t.Fatal("no PHP-FPM container")
	}
	if container.Labels["pocketcloud.role"] != "site" || container.Labels["pocketcloud.site"] != "shop" {
		t.Errorf("labels: %v", container.Labels)
	}
	if image, _ := container.Config["Image"].(string); image != php.Image+":"+first+"-fpm" {
		t.Errorf("image %q", image)
	}
	env, _ := container.Config["Env"].([]any)
	if !slices.Contains(env, any("APP_KEY="+secret)) || !slices.Contains(env, any("APP_ENV=production")) {
		t.Errorf("env: %v", env)
	}
	if signal, _ := container.Config["StopSignal"].(string); signal != "SIGQUIT" {
		t.Errorf("stop signal %q", signal)
	}
	host := container.Host
	if host["NetworkMode"] != "pcsite-shop" || !slices.Contains(host["CapDrop"].([]any), any("ALL")) {
		t.Errorf("host config: %v", host)
	}
	mounts, _ := host["Mounts"].([]any)
	if len(mounts) != 2 {
		t.Fatalf("mounts: %v", mounts)
	}
	own, shared := mounts[0].(map[string]any), mounts[1].(map[string]any)
	ownOptions, _ := own["VolumeOptions"].(map[string]any)
	sharedOptions, _ := shared["VolumeOptions"].(map[string]any)
	if own["Target"] != "/srv/sites/shop" || own["ReadOnly"] != true || ownOptions["Subpath"] != "shop" {
		t.Errorf("the site's own directory: %v", own)
	}
	if shared["Target"] != "/srv/sites/shop/shared" || shared["ReadOnly"] == true || sharedOptions["Subpath"] != "shop/shared" {
		t.Errorf("shared/: %v", shared)
	}
	pool := string(s.docker.File("shop-php", "/usr/local/etc/php-fpm.d/zz-pocketcloud-site.conf"))
	if !strings.Contains(pool, "php_admin_value[open_basedir] = /srv/sites/shop:/tmp") || !strings.Contains(pool, "pm.max_children = 10") {
		t.Errorf("pool:\n%s", pool)
	}
	if ini := string(s.docker.File("shop-php", "/usr/local/etc/php/conf.d/zzz-pocketcloud-site.ini")); !strings.Contains(ini, "memory_limit = 256M") {
		t.Errorf("php.ini:\n%s", ini)
	}
	network := s.docker.FindNetwork("pcsite-shop")
	if network == nil || network.Internal {
		t.Fatalf("the site's network: %+v", network)
	}
	edge := s.docker.Find("pocketcloud-edge")
	if edge == nil || len(network.Members) != 2 || !slices.Contains(network.Members[container.ID], "shop-php") {
		t.Errorf("on the site's network: %v", network.Members)
	} else if _, found := network.Members[edge.ID]; !found {
		t.Error("the edge is not on the site's network")
	}
	// the secret's value is in no command that was stored
	commands, _ := s.app.FindRecordsByFilter(schema.Commands, "host={:h}", "", 0, 0, dbx.Params{"h": s.host.Id})
	for _, command := range commands {
		if raw, _ := json.Marshal(command.Get("payload")); strings.Contains(string(raw), secret) {
			t.Errorf("%s stored the secret", command.GetString("type"))
		}
	}

	// the first release: its storage moves to shared/, the hook runs in it
	// with the site's secrets, and PHP-FPM reloads after the switch
	s.upload(siteID, map[string]string{
		"public/index.php":        "<?php echo 'shop';",
		"storage/logs/.gitignore": "*",
		"storage/framework/.keep": "",
		"app/Http/Kernel.php":     "<?php",
		".env.example":            "APP_ENV=local",
	})
	one := s.deployment(siteID, 1)
	if one.GetString("status") != schema.DeploymentSucceeded || one.GetString("php_version") != first {
		t.Fatalf("#1: %s %s %s", one.GetString("status"), one.GetString("error"), one.GetString("php_version"))
	}
	release := sites.ReleaseDir(s.sites, "shop", 1)
	if hook, _ := os.ReadFile(filepath.Join(release, "hook.txt")); string(hook) != secret {
		t.Errorf("the hook wrote %q", hook)
	}
	if target, err := os.Readlink(filepath.Join(release, "storage")); err != nil || target != "../../shared/storage" {
		t.Errorf("storage: %q %v", target, err)
	}
	if _, err := os.Stat(filepath.Join(sites.SharedDir(s.sites, "shop"), "storage", "logs", ".gitignore")); err != nil {
		t.Errorf("shared/ was not seeded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(release, "public", "index.php.gz")); err == nil {
		t.Error("a PHP release has gzip copies")
	}
	if signals := s.docker.Find("shop-php").Signals; !slices.Contains(signals, "USR2") {
		t.Errorf("PHP-FPM was not reloaded: %v", signals)
	}

	// the edge hands the site's scripts to its PHP-FPM, from its web root
	if status, out := s.request(http.MethodPost, "/api/collections/domains/records", s.operator, map[string]any{
		"hostname": "shop.example.test", "host": s.host.Id, "target_kind": schema.TargetSite, "site": siteID, "tls": schema.TLSOff,
	}); status != http.StatusOK {
		t.Fatalf("a domain: %d %v", status, out)
	}
	s.waitFor("the edge to serve the PHP site", func() bool {
		server := s.edge.server()
		if server == nil {
			return false
		}
		config := string(server.Config())
		return strings.Contains(config, `"dial":"shop-php:9000"`) && strings.Contains(config, `"root":"/srv/sites/shop/current/public"`)
	})

	// a new PHP version: PHP-FPM on its image, then the release built again on it
	next := ""
	for _, version := range php.Versions() {
		if version.Version != first {
			next = version.Version
			break
		}
	}
	status, out = s.request(http.MethodPut, "/api/pc/sites/"+siteID+"/php", s.operator, map[string]any{"php_version": next})
	if status != http.StatusOK || out["deployment"] == nil {
		t.Fatalf("a new version: %d %v", status, out)
	}
	two := s.deployment(siteID, 2)
	if two.GetString("status") != schema.DeploymentSucceeded || two.GetString("php_version") != next {
		t.Fatalf("#2: %s %s %s", two.GetString("status"), two.GetString("error"), two.GetString("php_version"))
	}
	if image, _ := s.docker.Find("shop-php").Config["Image"].(string); image != php.Image+":"+next+"-fpm" {
		t.Errorf("after the switch: %q", image)
	}
	// PHP-FPM came with the release: a newer one before the switch
	if command, err := s.app.FindRecordById(schema.Commands, two.GetString("command")); err != nil {
		t.Error(err)
	} else {
		var payload struct {
			PHP struct {
				Apply      map[string]any `json:"apply"`
				ApplyFirst bool           `json:"apply_first"`
			} `json:"php"`
		}
		raw, _ := json.Marshal(command.Get("payload"))
		_ = json.Unmarshal(raw, &payload)
		upgrade := php.Compare(next, first) > 0
		if payload.PHP.Apply["image"] != php.Image+":"+next+"-fpm" || payload.PHP.ApplyFirst != upgrade {
			t.Errorf("the deployment's PHP-FPM: %v, first %v", payload.PHP.Apply["image"], payload.PHP.ApplyFirst)
		}
	}
	if s.docker.Find("shop-php-old") != nil {
		t.Error("the replaced PHP-FPM is still there")
	}
	s.waitFor("the settings to say PHP-FPM runs", func() bool {
		return s.phpSettings(siteID).GetString("state") == schema.PHPRunning
	})

	// back to #1, built for the older version
	status, out = s.request(http.MethodPost, "/api/pc/sites/"+siteID+"/rollback", s.operator, map[string]any{"deployment": one.Id})
	if status != http.StatusOK {
		t.Fatalf("rollback: %d %v", status, out)
	}
	if done := s.waitForStatus(out["command"].(string), schema.CommandSucceeded, schema.CommandFailed); done.GetString("status") != schema.CommandSucceeded {
		t.Fatalf("rollback: %s", done.GetString("error"))
	}
	if version := s.phpSettings(siteID).GetString("php_version"); version != first {
		t.Errorf("the version after the rollback: %s", version)
	}
	// an older PHP-FPM follows the switch, as the newer one runs the old release
	s.waitFor("PHP-FPM to go back to "+first, func() bool {
		image, _ := s.docker.Find("shop-php").Config["Image"].(string)
		return image == php.Image+":"+first+"-fpm"
	})
	if current := sites.Current(s.sites, "shop"); current != 1 {
		t.Errorf("serves #%d", current)
	}

	// removing the site removes its container and network
	if status, _ := testutil.Do(t, s.handler, testutil.Request{Method: http.MethodDelete, URL: "/api/collections/sites/records/" + siteID, Token: s.operator}); status != http.StatusNoContent {
		t.Fatalf("remove: %d", status)
	}
	s.waitFor("the site's PHP-FPM and network to go", func() bool {
		return s.docker.Find("shop-php") == nil && s.docker.FindNetwork("pcsite-shop") == nil
	})
}

// TestANewPHPSiteIsSetUpWithItsPHPFPMBeforeItsRelease: the wizard's flow for
// a PHP site (#50): its PHP-FPM, on the version chosen, starts before its
// first release, and the release is served from the web root chosen.
func TestANewPHPSiteIsSetUpWithItsPHPFPMBeforeItsRelease(t *testing.T) {
	h := newProvisionHarness(t)
	h.resolvers.set("php.example.org", "A", "203.0.114.7")
	h.resolvers.set("php.example.org", "AAAA", "2a01:4f8::7")
	version := php.Versions()[len(php.Versions())-1].Version

	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for name, content := range map[string]string{"public/index.php": "<?php echo 'hi';", "src/App.php": "<?php"} {
		file, _ := writer.Create(name)
		_, _ = file.Write([]byte(content))
	}
	_ = writer.Close()
	send := func(phpSettings map[string]any) (int, string) {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		data, _ := json.Marshal(map[string]any{
			"site":     map[string]any{"name": "phpsite", "host": h.host.Id, "type": schema.SitePHPApp, "source": "upload"},
			"hostname": "php.example.org",
			"php":      phpSettings,
		})
		_ = form.WriteField("data", string(data))
		part, _ := form.CreateFormFile("file", "site.zip")
		_, _ = part.Write(archive.Bytes())
		_ = form.Close()
		return testutil.Do(t, h.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/sites/provision", Token: h.operator, Body: body.String(),
			Headers: map[string]string{"Content-Type": form.FormDataContentType()}})
	}

	// what is wrong is said before anything is made
	for _, bad := range []map[string]any{
		{"version": "5.6"}, {"docroot": "../x"}, {"template": "drupal"}, {"database_server": "nosuchserver"},
	} {
		if status, raw := send(bad); status != http.StatusBadRequest {
			t.Errorf("%v: %d %s", bad, status, raw)
		}
	}
	if _, err := h.app.FindFirstRecordByData(schema.Sites, "name", "phpsite"); err == nil {
		t.Fatal("a site was made for a request that was refused")
	}

	status, raw := send(map[string]any{"version": version, "docroot": "public"})
	if status != http.StatusCreated {
		t.Fatalf("the new site: %d %s", status, raw)
	}
	var out map[string]string
	_ = json.Unmarshal([]byte(raw), &out)
	setup := h.waitSetup(out["provision"], schema.CommandSucceeded, schema.CommandFailed)
	steps := h.steps(out["provision"])
	if setup.GetString("status") != schema.CommandSucceeded {
		t.Fatalf("the setup: %v; steps %s", setup.PublicExport(), stepTypes(steps))
	}
	if got := stepTypes(steps); got != "site.create:succeeded site.dns:succeeded site.propagation:succeeded site.php.apply:succeeded site.deploy:succeeded site.certificate:succeeded site.check:succeeded" {
		t.Errorf("steps: %s", got)
	}
	settings, err := h.app.FindFirstRecordByData(schema.SitePHP, "site", out["site"])
	if err != nil || settings.GetString("php_version") != version || settings.GetString("docroot") != "public" || settings.GetString("state") != schema.PHPRunning {
		t.Fatalf("the settings: %v %v", err, settings)
	}
	container := h.docker.Find("phpsite-php")
	if container == nil {
		t.Fatal("no PHP-FPM")
	}
	if image, _ := container.Config["Image"].(string); image != php.Image+":"+version+"-fpm" {
		t.Errorf("image %q", image)
	}
	if served, err := os.ReadFile(filepath.Join(h.sites, "phpsite", "current", "public", "index.php")); err != nil || string(served) != "<?php echo 'hi';" {
		t.Errorf("served: %q %v", served, err)
	}
}

// TestADatabaseOfASiteReachesItsPHPFPM: a database made for a PHP site (#50)
// puts its server on a network of the site's, which its PHP-FPM joins, and
// gives the site's environment what it needs, its password as a secret.
func TestADatabaseOfASiteReachesItsPHPFPM(t *testing.T) {
	d := newDatabaseHarness(t)
	d.host.Set("roles", []string{schema.HostRoleDocker, schema.HostRoleEdge})
	if err := d.app.Save(d.host); err != nil {
		t.Fatal(err)
	}
	server := d.createServer("db-main", schema.EngineMariaDB)
	status, body := d.post("/api/collections/sites/records", d.operator, map[string]any{"name": "shop", "host": d.host.Id, "type": schema.SitePHPApp, "source": "upload"})
	if status != http.StatusOK {
		t.Fatalf("the site: %d %s", status, body)
	}
	var site struct{ ID string }
	answerOf(t, body, &site)

	if status, body := d.post("/api/pc/databases", d.operator, map[string]any{"server": server.Id, "name": "shop", "site": site.ID, "stack": "x"}); status != http.StatusBadRequest {
		t.Errorf("a stack and a site: %d %s", status, body)
	}
	status, body = d.post("/api/pc/databases", d.operator, map[string]any{"server": server.Id, "name": "shop", "site": site.ID})
	if status != http.StatusOK {
		t.Fatalf("the database: %d %s", status, body)
	}
	var created struct {
		Database struct{ ID string } `json:"database"`
	}
	answerOf(t, body, &created)
	d.waitFor("the database to be ready", func() bool {
		database, err := d.app.FindRecordById(schema.Databases, created.Database.ID)
		return err == nil && database.GetString("status") == "ready"
	})
	database, _ := d.app.FindRecordById(schema.Databases, created.Database.ID)
	if database.GetString("owner_kind") != "site" || database.GetString("site") != site.ID || database.GetString("last_error") != "" {
		t.Errorf("the database: %v", database.PublicExport())
	}

	settings, err := d.app.FindFirstRecordByData(schema.SitePHP, "site", site.ID)
	if err != nil {
		t.Fatal(err)
	}
	env := settings.GetString("env")
	for _, line := range []string{"DB_HOST=db-main", "DB_PORT=3306", "DB_NAME=shop", "DB_USER=shop", "DB_PASSWORD=${secret:db/shop}"} {
		if !strings.Contains(env, line) {
			t.Errorf("the environment has no %s:\n%s", line, env)
		}
	}
	if settings.GetString("database") != database.Id {
		t.Errorf("the settings' database: %q", settings.GetString("database"))
	}
	network, err := d.app.FindFirstRecordByFilter(schema.DBNetworks, "site={:s}", dbx.Params{"s": site.ID})
	if err != nil || network.GetString("name") != "pcdb-site.shop" || network.GetString("server") != server.Id {
		t.Fatalf("the network: %v %v", err, network)
	}
	// the server joins it, and the site's PHP-FPM is applied with it
	d.waitFor("the server to join the site's network", func() bool {
		attach, err := d.app.FindFirstRecordByFilter(schema.Commands, "type='db.network.attach' && label={:l}", dbx.Params{"l": "shop → db-main"})
		return err == nil && attach.GetString("status") == schema.CommandSucceeded
	})
	apply, err := d.app.FindFirstRecordByFilter(schema.Commands, "type='site.php.apply'", dbx.Params{})
	if err != nil {
		t.Fatal("PHP-FPM was not applied with the database")
	}
	var payload struct {
		Networks []string `json:"networks"`
	}
	raw, _ := json.Marshal(apply.Get("payload"))
	_ = json.Unmarshal(raw, &payload)
	if !slices.Contains(payload.Networks, "pcdb-site.shop") {
		t.Errorf("PHP-FPM's networks: %v", payload.Networks)
	}
}
