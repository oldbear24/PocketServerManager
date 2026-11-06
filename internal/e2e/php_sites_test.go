package e2e

import (
	"encoding/json"
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
