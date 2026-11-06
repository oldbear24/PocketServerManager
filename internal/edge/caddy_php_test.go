package edge

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/version"
)

// TestPHPSitesAreServedByCaddyAndPHPFPM serves a PHP site with the pinned edge
// image in front of PocketCloud's PHP runtime image (#50): the front
// controller, scripts, static files, hidden dotfiles, the redirect of a
// directory, and a release switched under both. It needs Docker, and runs when
// POCKETCLOUD_PHP_IMAGE names the runtime image, such as
// ghcr.io/oldbear24/pocketcloud-php:8.4-fpm.
func TestPHPSitesAreServedByCaddyAndPHPFPM(t *testing.T) {
	image := os.Getenv("POCKETCLOUD_PHP_IMAGE")
	if image == "" {
		t.Skip("set POCKETCLOUD_PHP_IMAGE to serve a PHP site with the edge and runtime images in Docker")
	}

	root := t.TempDir()
	// the runtime image runs PHP-FPM as www-data, who must reach the files
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	put := func(name, content string) {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("shop/releases/1/public/index.php", `<?php echo "one ", __FILE__, " ", $_SERVER["REQUEST_URI"];`)
	put("shop/releases/1/public/info.php", `<?php echo "info ", $_SERVER["PATH_INFO"] ?? "";`)
	put("shop/releases/1/public/docs/index.php", `<?php echo "docs";`)
	put("shop/releases/1/public/style.css", "body {}")
	put("shop/releases/1/public/.env", "APP_KEY=secret")
	put("shop/releases/2/public/index.php", `<?php echo "two";`)
	if err := os.Symlink("releases/1", filepath.Join(root, "shop", "current")); err != nil {
		t.Fatal(err)
	}

	config := Config{Routes: []Route{{Hostname: "shop.example.test", Kind: KindSite, Site: &SiteOptions{
		Root:    "/srv/sites/shop/current/public",
		FastCGI: "shop-php:9000",
	}}}}
	built, err := Build(config)
	if err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), built, 0o644); err != nil {
		t.Fatal(err)
	}

	network := "pc-php-test-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	if out, err := exec.Command("docker", "network", "create", network).CombinedOutput(); err != nil {
		t.Fatalf("docker network create: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", append([]string{"run", "-d", "--rm", "--network", network}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker run: %v %s", err, out)
		}
		id := strings.TrimSpace(string(out))
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
		return id
	}
	// the PHP-FPM container sees its own site only, where the edge sees it
	run("--network-alias", "shop-php", "-v", filepath.Join(root, "shop")+":/srv/sites/shop:ro", image)
	edge := run("-p", "127.0.0.1::80", "--tmpfs", "/pocketcloud/run",
		"-v", root+":"+SitesRoot+":ro",
		"-v", configDir+":/c:ro",
		version.EdgeRef(), "caddy", "run", "--config", "/c/config.json")
	out, err := exec.Command("docker", "port", edge, "80/tcp").Output()
	if err != nil {
		t.Fatal(err)
	}
	address := "http://" + strings.TrimSpace(strings.Split(string(out), "\n")[0])

	client := &http.Client{
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	get := func(path string) (int, http.Header, string) {
		t.Helper()
		request, _ := http.NewRequest(http.MethodGet, address+path, nil)
		request.Host = "shop.example.test"
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, response.Header, string(body)
	}
	// both start; PHP-FPM answers once its pool is up
	for start := time.Now(); ; time.Sleep(200 * time.Millisecond) {
		request, _ := http.NewRequest(http.MethodGet, address+"/", nil)
		request.Host = "shop.example.test"
		if response, err := client.Do(request); err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Since(start) > 30*time.Second {
			logs, _ := exec.Command("docker", "logs", edge).CombinedOutput()
			t.Fatalf("the PHP site does not answer: %s", logs)
		}
	}

	// the script runs under the release the symlink points at, not under current
	if status, _, body := get("/"); status != 200 || body != "one /srv/sites/shop/releases/1/public/index.php /" {
		t.Errorf("/: %d %q", status, body)
	}
	// a path that is no file goes to the front controller
	if status, _, body := get("/products/42?page=2"); status != 200 || !strings.HasSuffix(body, " /products/42?page=2") {
		t.Errorf("front controller: %d %q", status, body)
	}
	if status, _, body := get("/info.php/a/b"); status != 200 || body != "info /a/b" {
		t.Errorf("path info: %d %q", status, body)
	}
	if status, header, _ := get("/docs"); status != http.StatusPermanentRedirect || header.Get("Location") != "/docs/" {
		t.Errorf("directory: %d %q", status, header.Get("Location"))
	}
	if status, _, body := get("/docs/"); status != 200 || body != "docs" {
		t.Errorf("directory index: %d %q", status, body)
	}
	if status, header, body := get("/style.css"); status != 200 || body != "body {}" || !strings.HasPrefix(header.Get("Content-Type"), "text/css") {
		t.Errorf("a static file: %d %q %q", status, header.Get("Content-Type"), body)
	}
	// a dotfile is neither served nor run
	if status, _, body := get("/.env"); status != http.StatusNotFound || strings.Contains(body, "APP_KEY") {
		t.Errorf(".env: %d %q", status, body)
	}

	// a release switched under the running edge and PHP-FPM
	next := filepath.Join(root, "shop", ".current-next")
	if err := os.Symlink("releases/2", next); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, filepath.Join(root, "shop", "current")); err != nil {
		t.Fatal(err)
	}
	if status, _, body := get("/"); status != 200 || body != "two" {
		t.Errorf("after the switch: %d %q", status, body)
	}
}
