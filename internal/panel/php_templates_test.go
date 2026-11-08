package panel

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/phpapps"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func TestAnUpdateAddsPluginsAndThemesQuoted(t *testing.T) {
	wordpress, _ := phpapps.Get("wordpress")
	hook, err := updateHook(wordpress, []string{"akismet", " https://example.org/a-plugin.zip "}, []string{"twentytwentyfive"})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(hook, "\n")
	if lines[0] != "set -e" || !slices.Contains(lines, "wp core update") ||
		lines[len(lines)-2] != "wp plugin install --activate 'akismet' 'https://example.org/a-plugin.zip'" ||
		lines[len(lines)-1] != "wp theme install 'twentytwentyfive'" {
		t.Errorf("hook:\n%s", hook)
	}
	if hook, _ := updateHook(wordpress, nil, nil); hook != strings.TrimSpace(wordpress.Update.Command) {
		t.Errorf("nothing to add:\n%s", hook)
	}
	for _, bad := range [][]string{
		{"Akismet"}, {"-x"}, {"a b"}, {"a;b"}, {"https://example.org/x'.zip"}, {"https://example.org/$(id).zip"[:0] + "ftp://x"},
		{"https://example.org/a b.zip"}, slices.Repeat([]string{"a"}, 21),
	} {
		if _, err := updateHook(wordpress, bad, nil); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	nette, _ := phpapps.Get("nette")
	nette.Update = &phpapps.Update{Command: "true"}
	if _, err := updateHook(nette, []string{"akismet"}, nil); err == nil {
		t.Error("a template without plugins added one")
	}
}

func TestAFreshInstallIsCheckedBeforeAnythingIsMade(t *testing.T) {
	good := freshInstall{Title: " Můj blog ", AdminUser: "editor", AdminEmail: "editor@example.org", AdminPassword: "correct horse battery"}
	install := good
	if err := checkInstall(&install); err != nil || install.Title != "Můj blog" || install.Locale != "en_US" {
		t.Errorf("%+v %v", install, err)
	}
	for name, change := range map[string]func(*freshInstall){
		"no title":        func(i *freshInstall) { i.Title = " " },
		"two lines":       func(i *freshInstall) { i.Title = "a\nb" },
		"long title":      func(i *freshInstall) { i.Title = strings.Repeat("x", 201) },
		"user with space": func(i *freshInstall) { i.AdminUser = "the editor" },
		"no user":         func(i *freshInstall) { i.AdminUser = "" },
		"named address":   func(i *freshInstall) { i.AdminEmail = "Ed <editor@example.org>" },
		"no address":      func(i *freshInstall) { i.AdminEmail = "editor" },
		"short password":  func(i *freshInstall) { i.AdminPassword = "eleven char" },
		"locale":          func(i *freshInstall) { i.Locale = "cs_CZ'" },
	} {
		install := good
		change(&install)
		if err := checkInstall(&install); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
}

func TestAnUploadIsRecognisedByItsFiles(t *testing.T) {
	read := func(files ...string) []string {
		t.Helper()
		var archive bytes.Buffer
		writer := zip.NewWriter(&archive)
		for _, name := range files {
			_, _ = writer.Create(name)
		}
		_ = writer.Close()
		paths, err := zipPaths(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
		if err != nil {
			t.Fatal(err)
		}
		return paths
	}
	// one folder that holds everything is the root, as an upload unpacks
	if got := phpapps.Detect(read("shop/artisan", "shop/public/index.php", "shop/")); got != "laravel" {
		t.Errorf("a Laravel ZIP: %q", got)
	}
	if got := phpapps.Detect(read("./wp-load.php", "wp-includes/version.php")); got != "wordpress" {
		t.Errorf("a WordPress ZIP: %q", got)
	}
	if got := phpapps.Detect(read("docs/artisan", "index.php")); got != "" {
		t.Errorf("artisan in a folder: %q", got)
	}
	if _, err := zipPaths(bytes.NewReader([]byte("not a zip")), 9); err == nil {
		t.Error("a file that is no ZIP was read")
	}
}

func TestANameOfTheLANIsAskedAtTheServersAddressThere(t *testing.T) {
	p, app, _ := newTestPanel(t)
	host := saveRecord(t, app, schema.Hosts, map[string]any{
		"name": "vps", "status": "online", "roles": []string{"docker", "edge"}, "ip4": "192.168.1.20", "public_ip4": "203.0.114.7",
	})
	public := saveRecord(t, app, schema.Domains, map[string]any{
		"hostname": "shop.example.org", "host": host.Id, "target_kind": schema.TargetContainer, "container": "x", "port": 80, "tls": schema.TLSAuto,
	})
	internal := saveRecord(t, app, schema.Domains, map[string]any{
		"hostname": "wiki.lan", "host": host.Id, "target_kind": schema.TargetContainer, "container": "x", "port": 80, "tls": schema.TLSInternal,
	})
	if address, roots := p.siteAddress(host, public); address != "203.0.114.7" || roots != nil {
		t.Errorf("a public name: %s %v", address, roots)
	}
	if address, _ := p.siteAddress(host, internal); address != "192.168.1.20" {
		t.Errorf("a name of the LAN: %s", address)
	}
}

// TestASiteServedWithoutTLSIsAskedOverHTTP: the health check asks a name the
// site serves over HTTPS when it has one, and one served without TLS over
// plain HTTP, rather than calling it broken after every release (#52).
func TestASiteServedWithoutTLSIsAskedOverHTTP(t *testing.T) {
	p, app, _ := newTestPanel(t)
	host := saveRecord(t, app, schema.Hosts, map[string]any{
		"name": "vps", "status": "online", "roles": []string{"docker", "edge"}, "public_ip4": "203.0.114.7",
	})
	site := saveRecord(t, app, schema.Sites, map[string]any{"name": "shop", "host": host.Id, "type": schema.SitePHPApp, "source": "upload"})
	settings, err := p.phpSettings(app, site)
	if err != nil {
		t.Fatal(err)
	}
	settings.Set("health_path", "/up")
	if err := app.Save(settings); err != nil {
		t.Fatal(err)
	}
	deployment := saveRecord(t, app, schema.Deployments, map[string]any{"site": site.Id, "number": 1, "status": schema.DeploymentSucceeded})
	site.Set("active_deployment", deployment.Id)
	if err := app.Save(site); err != nil {
		t.Fatal(err)
	}
	var asked []string
	p.SetSiteCheck(func(_ context.Context, url, _ string) (int, string, error) {
		asked = append(asked, url)
		return 200, "", nil
	})
	domain := func(name, tls string) {
		saveRecord(t, app, schema.Domains, map[string]any{
			"hostname": name, "host": host.Id, "site": site.Id, "target_kind": schema.TargetSite, "tls": tls,
		})
	}
	domain("plain.example.org", schema.TLSOff)
	if again := p.lookAtHealth(site.Id, deployment.Id, time.Now()); again || len(asked) != 1 || asked[0] != "http://plain.example.org/up" {
		t.Errorf("without TLS: %v %v", asked, again)
	}
	domain("shop.example.org", schema.TLSAuto)
	p.lookAtHealth(site.Id, deployment.Id, time.Now())
	if asked[len(asked)-1] != "https://shop.example.org/up" {
		t.Errorf("with a name over HTTPS too: %v", asked)
	}
	if health := deploymentHealth(t, app, deployment.Id); health["ok"] != true || health["url"] != "https://shop.example.org/up" {
		t.Errorf("health: %v", health)
	}
}

func deploymentHealth(t *testing.T, app core.App, id string) map[string]any {
	t.Helper()
	record, err := app.FindRecordById(schema.Deployments, id)
	if err != nil {
		t.Fatal(err)
	}
	health := map[string]any{}
	raw, _ := json.Marshal(record.Get("health"))
	_ = json.Unmarshal(raw, &health)
	return health
}
