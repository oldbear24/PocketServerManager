package phpapps

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestTheTemplatesAreWordPressLaravelAndNette(t *testing.T) {
	if got := strings.Join(Names(), " "); got != "laravel wordpress nette" {
		t.Fatalf("templates: %s", got)
	}
	wordpress, _ := Get("wordpress")
	laravel, _ := Get("laravel")
	nette, _ := Get("nette")
	if _, ok := Get("custom"); ok {
		t.Error("custom is a template")
	}

	for _, c := range []struct {
		t                Template
		docroot, health  string
		shared, noBackup []string
	}{
		{wordpress, "", "/", []string{"wp-content/uploads"}, []string{"wp-content/cache", "wp-content/upgrade"}},
		{laravel, "public", "/up", []string{"storage"}, []string{"storage/framework/cache", "storage/framework/views", "storage/logs"}},
		{nette, "www", "/", []string{"log", "temp"}, []string{"temp"}},
	} {
		if c.t.Docroot != c.docroot || c.t.Health != c.health {
			t.Errorf("%s: docroot %q, health %q", c.t.Name, c.t.Docroot, c.t.Health)
		}
		if !slices.Equal(c.t.SharedPaths(), c.shared) || !slices.Equal(c.t.NoBackup(), c.noBackup) {
			t.Errorf("%s: shared %v, no backup %v", c.t.Name, c.t.SharedPaths(), c.t.NoBackup())
		}
	}

	// WordPress: a fresh install, its wp-config.php, its cron and its update
	if wordpress.Database != DatabaseRequired || wordpress.Fresh == nil || wordpress.Update == nil {
		t.Errorf("wordpress: %+v", wordpress)
	}
	if !wordpress.TakesDatabase("mariadb") || wordpress.TakesDatabase("postgres") || !laravel.TakesDatabase("postgres") {
		t.Error("the engines of the databases")
	}
	config := wordpress.FileContents()["wp-config.php"]
	// written anew at every release while it is the template's
	if !strings.Contains(config[:1024], "@pocketcloud-template") {
		t.Error("wp-config.php lacks its marker in its first KiB")
	}
	// file mods only while a release is built, and no background updates
	for _, want := range []string{"getenv($name)", "'DISALLOW_FILE_MODS', $pocketcloud_releases && $pocketcloud_env('POCKETCLOUD_RELEASE') !== 'building'",
		"'AUTOMATIC_UPDATER_DISABLED', $pocketcloud_releases", "'DISABLE_WP_CRON'", "WP_SALT", "wp-settings.php"} {
		if !strings.Contains(config, want) {
			t.Errorf("wp-config.php lacks %s", want)
		}
	}
	if len(wordpress.Tasks) != 1 || wordpress.Tasks[0].Command != "wp cron event run --due-now" {
		t.Errorf("wordpress tasks: %+v", wordpress.Tasks)
	}
	for _, want := range []string{"wp core update", "wp plugin update --all", "wp theme update --all"} {
		if !strings.Contains(wordpress.Update.Command, want) {
			t.Errorf("the update lacks %s", want)
		}
	}
	// what WordPress cached of updates for 12 hours is asked anew
	for _, cache := range []string{"update_core", "update_plugins", "update_themes"} {
		if !strings.Contains(wordpress.Update.Command, "wp transient delete "+cache+" --network") ||
			strings.Index(wordpress.Update.Command, cache) > strings.Index(wordpress.Update.Command, "wp core update") {
			t.Errorf("the update does not forget %s first", cache)
		}
	}

	// Laravel: storage:link, its .env, the scheduler, a worker
	if !slices.Equal(laravel.LinkSpecs(), []string{"public/storage=storage/app/public"}) || laravel.EnvFile != ".env" ||
		!laravel.RedeployOnEnv || laravel.Worker == nil || !strings.Contains(laravel.ReleaseHook, "php artisan migrate --force") {
		t.Errorf("laravel: %+v", laravel)
	}
	if len(laravel.Tasks) != 1 || laravel.Tasks[0].Schedule != "* * * * *" || laravel.Tasks[0].Command != "php artisan schedule:run" {
		t.Errorf("laravel tasks: %+v", laravel.Tasks)
	}

	// Nette: temp/ is emptied at each release
	if !slices.Equal(nette.Cleared(), []string{"temp"}) {
		t.Errorf("nette clears %v", nette.Cleared())
	}
}

func TestASourceIsRecognisedByItsFiles(t *testing.T) {
	for _, c := range []struct {
		paths []string
		want  string
	}{
		{[]string{"artisan", "composer.json", "public/index.php"}, "laravel"},
		{[]string{"wp-load.php", "wp-includes/version.php", "index.php"}, "wordpress"},
		{[]string{"wp-config.php"}, "wordpress"},
		{[]string{"app/Bootstrap.php", "www/index.php"}, "nette"},
		{[]string{"./app/bootstrap.php"}, "nette"},
		// Laravel first: an application may carry a site it imports
		{[]string{"artisan", "legacy/wp-config.php", "wp-config.php"}, "laravel"},
		{[]string{"index.php", "composer.json"}, ""},
		{[]string{"docs/artisan.md", "vendor/laravel/framework/artisan/"}, ""},
		{nil, ""},
	} {
		if got := Detect(c.paths); got != c.want {
			t.Errorf("%v: %q, want %q", c.paths, got, c.want)
		}
	}

	dir := t.TempDir()
	if got := DetectDir(dir); got != "" {
		t.Errorf("an empty directory is %q", got)
	}
	_ = os.MkdirAll(filepath.Join(dir, "app"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "app", "Bootstrap.php"), []byte("<?php"), 0o644)
	if got := DetectDir(dir); got != "nette" {
		t.Errorf("a Nette source is %q", got)
	}
	// a directory or a link of the name is no file of the framework
	_ = os.Mkdir(filepath.Join(dir, "artisan"), 0o755)
	_ = os.Symlink("/etc/passwd", filepath.Join(dir, "wp-load.php"))
	if got := DetectDir(dir); got != "nette" {
		t.Errorf("with a directory artisan and a link wp-load.php: %q", got)
	}
}

func TestATemplatesValuesComeFromTheSite(t *testing.T) {
	laravel, _ := Get("laravel")
	vars := Vars{URL: "https://shop.example.com", Hostname: "shop.example.com"}
	lines, err := laravel.EnvLines(vars, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"APP_URL=https://shop.example.com", "APP_KEY=${secret:app-key}", "SESSION_DRIVER=file", "QUEUE_CONNECTION=sync"} {
		if !slices.Contains(lines, want) {
			t.Errorf("without a database, the environment lacks %s: %v", want, lines)
		}
	}
	lines, _ = laravel.EnvLines(vars, true)
	if slices.Contains(lines, "SESSION_DRIVER=file") || slices.Contains(lines, "QUEUE_CONNECTION=sync") {
		t.Errorf("with a database, the environment keeps Laravel's defaults: %v", lines)
	}

	for engine, driver := range map[string]string{"mariadb": "mysql", "postgres": "pgsql"} {
		lines, err := laravel.DatabaseEnvLines(DatabaseVars{Engine: engine, Host: "db-main", Port: 3306, Database: "shop", User: "shop_u"})
		if err != nil || !slices.Equal(lines, []string{"DB_CONNECTION=" + driver, "DB_DATABASE=shop", "DB_USERNAME=shop_u"}) {
			t.Errorf("%s: %v %v", engine, lines, err)
		}
	}

	secrets, err := laravel.NewSecrets()
	if err != nil || !strings.HasPrefix(secrets["app-key"], "base64:") || len(secrets["app-key"]) != len("base64:")+44 {
		t.Errorf("app-key %q %v", secrets["app-key"], err)
	}
	again, _ := laravel.NewSecrets()
	if again["app-key"] == secrets["app-key"] {
		t.Error("two sites got the same key")
	}
	wordpress, _ := Get("wordpress")
	if salt, _ := wordpress.NewSecrets(); len(salt["wp-salt"]) != 96 {
		t.Errorf("wp-salt %q", salt["wp-salt"])
	}

	build, err := wordpress.FreshBuild(Vars{Locale: "cs_CZ"})
	if err != nil || build != "wp core download --locale=cs_CZ" {
		t.Errorf("fresh build %q %v", build, err)
	}
	for _, bad := range []string{"", "cs_CZ; rm -rf /", "CS", "en-US"} {
		if _, err := wordpress.FreshBuild(Vars{Locale: bad}); err == nil {
			t.Errorf("the locale %q was taken", bad)
		}
	}
	if _, err := laravel.FreshBuild(Vars{Locale: "en_US"}); err == nil {
		t.Error("Laravel has a fresh install")
	}
}

// TestTheEnvironmentFileKeepsItsValues: each value in double quotes, with
// what phpdotenv would read otherwise escaped; that Laravel reads it as the
// values are is checked on Docker.
func TestTheEnvironmentFileKeepsItsValues(t *testing.T) {
	text := EnvFile(map[string]string{
		"APP_KEY":     "base64:abc+/=",
		"DB_PASSWORD": `p"a\s$s${HOME}#x`,
		"APP_NAME":    "My shop",
		"MULTI":       "one\ntwo",
		"EMPTY":       "",
	})
	want := `# Written by PocketCloud at the release, from the site's environment.
# Change it on the site's page in PocketCloud: the next release writes it anew.
APP_KEY="base64:abc+/="
APP_NAME="My shop"
DB_PASSWORD="p\"a\\s\$s\${HOME}#x"
EMPTY=""
MULTI="one\ntwo"
`
	if text != want {
		t.Errorf("got\n%s\nwant\n%s", text, want)
	}
}

func TestABrokenTemplateIsRefused(t *testing.T) {
	good := "name: x\ntitle: X\nhealth: /\n"
	for name, text := range map[string]string{
		"no title":      "name: x\nhealth: /\n",
		"health":        "name: x\ntitle: X\nhealth: up\n",
		"unknown field": good + "colour: red\n",
		"docroot":       good + "docroot: ../www\n",
		"shared":        good + "shared:\n  - path: /etc\n",
		"link":          good + "links:\n  - path: public/storage\n    target: ../../other\n",
		"env":           good + "env:\n  - name: POCKETCLOUD_MODE\n    value: x\n",
		"env template":  good + "env:\n  - name: A\n    value: '{{.URL'\n",
		"secret":        good + "secrets:\n  - name: k\n    kind: weak\n",
		"task":          good + "tasks:\n  - schedule: '* * * * *'\n    command: x\n",
		"database":      good + "database: maybe\n",
		"file":          good + "files:\n  - path: a.php\n    from: missing.php\n",
		"named":         "name: y\ntitle: Y\nhealth: /\n",
	} {
		_, err := load(fstest.MapFS{"templates/x.yaml": {Data: []byte(text)}})
		if err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	if templates, err := load(fstest.MapFS{"templates/x.yaml": {Data: []byte(good)}}); err != nil || len(templates) != 1 {
		t.Errorf("a good template: %v %v", templates, err)
	}
}
