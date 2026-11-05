package edge

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
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

// TestSitesAreServedByCaddy serves two sites from a directory laid out like
// the sites volume, with the pinned edge image: the 404 page, headers, cache
// rules, gzip copies, a single-page application behind a password, and a
// release switched under a running Caddy. It needs Docker and runs when
// POCKETCLOUD_CADDY_TEST is set.
func TestSitesAreServedByCaddy(t *testing.T) {
	if os.Getenv("POCKETCLOUD_CADDY_TEST") == "" {
		t.Skip("set POCKETCLOUD_CADDY_TEST=1 to serve sites with the edge image in Docker")
	}

	root := t.TempDir()
	put := func(name, content string) {
		path := filepath.Join(root, name)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	css := strings.Repeat("body { color: red; }\n", 100)
	put("blog/releases/1/index.html", "one")
	put("blog/releases/1/404.html", "not here")
	put("blog/releases/1/about/index.html", "about")
	put("blog/releases/1/assets/app.css", css)
	var gz bytes.Buffer
	writer := gzip.NewWriter(&gz)
	_, _ = writer.Write([]byte(css))
	_ = writer.Close()
	put("blog/releases/1/assets/app.css.gz", gz.String())
	put("blog/releases/2/index.html", "two")
	put("app/releases/1/index.html", "spa")
	put("wild/releases/1/index.html", "wild")
	put("wild/releases/1/404.html", "the wildcard's 404")
	put("tame/releases/1/index.html", "tame")
	for _, site := range []string{"blog", "app", "wild", "tame"} {
		if err := os.Symlink("releases/1", filepath.Join(root, site, "current")); err != nil {
			t.Fatal(err)
		}
	}

	certs := t.TempDir()
	writeSelfSigned(t, certs, "certapp", "app.example.test")
	config := Config{Certificates: []Certificate{{ID: "certapp"}}, Routes: []Route{
		{Hostname: "blog.example.test", Kind: KindSite, Compress: true, Site: &SiteOptions{
			Root:     "/srv/sites/blog/current",
			NotFound: "/404.html",
			Headers:  []SiteHeader{{Name: "X-Frame-Options", Value: "DENY"}},
			Cache:    []CacheRule{{Path: "/assets/*", MaxAge: 3600}},
		}},
		// a site under another's wildcard gets none of the wildcard's 404 page
		{Hostname: "*.wild.test", Kind: KindSite, Site: &SiteOptions{Root: "/srv/sites/wild/current", NotFound: "/404.html"}},
		{Hostname: "tame.wild.test", Kind: KindSite, Site: &SiteOptions{Root: "/srv/sites/tame/current"}},
		{Hostname: "app.example.test", Kind: KindSite, Certificate: "certapp", HTTPSRedirect: true, Site: &SiteOptions{Root: "/srv/sites/app/current", SPA: true}, BasicAuth: []BasicAuthUser{
			{Username: "carol", Hash: "$2a$05$cdSx43KWUy4Imu5KZU9RrOmFF/YsG/lIIWlXA8QAYE1NcKuKSOyte"},
		}},
	}}
	built, err := Build(config)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveSecrets(string(built), Secrets(config))
	if err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(resolved), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command("docker", "run", "-d", "--rm", "-p", "127.0.0.1::80", "-p", "127.0.0.1::443",
		"--tmpfs", "/pocketcloud/run",
		"-v", root+":"+SitesRoot+":ro",
		"-v", certs+":"+CertDir+":ro",
		"-v", configDir+":/c:ro",
		version.EdgeRef(), "caddy", "run", "--config", "/c/config.json").CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v %s", err, out)
	}
	container := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", container).Run() })
	published := func(port string) string {
		out, err := exec.Command("docker", "port", container, port).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(strings.Split(string(out), "\n")[0])
	}
	address := "http://" + published("80/tcp")
	secureAddress := "https://" + published("443/tcp")

	client := &http.Client{
		Transport:     &http.Transport{DisableCompression: true, Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	get := func(host, path string, headers ...string) (int, http.Header, string) {
		t.Helper()
		request, _ := http.NewRequest(http.MethodGet, address+path, nil)
		request.Host = host
		for i := 0; i+1 < len(headers); i += 2 {
			request.Header.Set(headers[i], headers[i+1])
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, response.Header, string(body)
	}
	for start := time.Now(); ; time.Sleep(100 * time.Millisecond) {
		request, _ := http.NewRequest(http.MethodGet, address+"/", nil)
		request.Host = "blog.example.test"
		if response, err := client.Do(request); err == nil {
			response.Body.Close()
			break
		}
		if time.Since(start) > 15*time.Second {
			logs, _ := exec.Command("docker", "logs", container).CombinedOutput()
			t.Fatalf("Caddy does not answer: %s", logs)
		}
	}

	if status, header, body := get("blog.example.test", "/"); status != 200 || body != "one" || header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("the home page: %d %q %v", status, body, header)
	}
	if status, _, body := get("blog.example.test", "/about/"); status != 200 || body != "about" {
		t.Errorf("a directory: %d %q", status, body)
	}
	status, header, body := get("blog.example.test", "/assets/app.css", "Accept-Encoding", "gzip")
	if status != 200 || header.Get("Content-Encoding") != "gzip" || body != gz.String() || header.Get("Cache-Control") != "public, max-age=3600" {
		t.Errorf("the stylesheet: %d %v (%d bytes)", status, header, len(body))
	}
	if _, header, body := get("blog.example.test", "/assets/app.css"); header.Get("Content-Encoding") != "" || body != css {
		t.Errorf("the stylesheet without gzip: %v (%d bytes)", header, len(body))
	}
	if status, _, body := get("blog.example.test", "/missing"); status != 404 || body != "not here" {
		t.Errorf("a missing page: %d %q", status, body)
	}
	if status, _, body := get("any.wild.test", "/missing"); status != 404 || body != "the wildcard's 404" {
		t.Errorf("the wildcard's missing page: %d %q", status, body)
	}
	if status, _, body := get("tame.wild.test", "/missing"); status != 404 || strings.Contains(body, "wildcard") {
		t.Errorf("a site under the wildcard got its 404 page: %d %q", status, body)
	}

	// a password, over HTTPS only, and a path that is no file answered by the
	// application
	if status, _, _ := get("app.example.test", "/deep/link"); status != 308 {
		t.Errorf("a password over plain HTTP: %d", status)
	}
	secure := &http.Client{Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{ServerName: "app.example.test", InsecureSkipVerify: true}}} //nolint:gosec // a certificate of the test's own
	asApp := func(user, password string) (int, string) {
		request, _ := http.NewRequest(http.MethodGet, secureAddress+"/deep/link", nil)
		request.Host = "app.example.test"
		if user != "" {
			request.SetBasicAuth(user, password)
		}
		response, err := secure.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(body)
	}
	if status, _ := asApp("", ""); status != 401 {
		t.Errorf("without the password: %d", status)
	}
	if status, body := asApp("carol", "carol-password"); status != 200 || body != "spa" {
		t.Errorf("the application's route: %d %q", status, body)
	}

	// a release switched while Caddy runs is served from the next request on
	next := filepath.Join(root, "blog", ".current-next")
	if err := os.Symlink("releases/2", next); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, filepath.Join(root, "blog", "current")); err != nil {
		t.Fatal(err)
	}
	if _, _, body := get("blog.example.test", "/"); body != "two" {
		t.Errorf("after the switch: %q", body)
	}
}
