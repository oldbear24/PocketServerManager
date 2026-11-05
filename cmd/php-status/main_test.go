package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/fcgi"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testStatusPath = "/status-0123456789abcdef"
	testPingPath   = "/ping-0123456789abcdef"
	testScript     = "/usr/local/lib/pocketcloud-php/opcache-status.php"
)

// fakeFPM answers like FPM's status listener: the status page, the ping and the
// OPcache script. broken names the parts that fail.
func fakeFPM(t *testing.T, broken ...string) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := fcgi.ProcessEnv(r)
		is := func(part string) bool {
			for _, b := range broken {
				if b == part {
					return true
				}
			}
			return false
		}

		switch {
		case r.URL.Path == testStatusPath && is("status-html"):
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, "<html>pool www</html>")
		case r.URL.Path == testStatusPath && is("full-invalid") && r.URL.RawQuery == "json&full":
			// FPM does not escape a process's user or script
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"pool":"www","processes":[{"pid":7,"user":"a"b","script":"/srv/x.php"}]}`)
		case r.URL.Path == testStatusPath && is("full-invalid") && r.URL.RawQuery == "json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"pool":"www","process manager":"ondemand","accepted conn":12}`)
		case r.URL.Path == testStatusPath:
			if r.URL.RawQuery != "json&full" {
				http.Error(w, "the status must be asked for as json&full, not "+r.URL.RawQuery, http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"pool":"www","process manager":"ondemand","accepted conn":12,"processes":[{"pid":7,"state":"Idle","request uri":"/index.php"}]}`)
		case r.URL.Path == testPingPath:
			w.Header().Set("Content-Type", "text/plain")
			if is("ping") {
				_, _ = fmt.Fprint(w, "pang")
				return
			}
			_, _ = fmt.Fprint(w, "pong")
		case env["SCRIPT_FILENAME"] == testScript:
			if is("opcache") {
				http.Error(w, "", http.StatusInternalServerError)
				return
			}
			// the script answers only who knows the status page's path
			if env["POCKETCLOUD_STATUS_PATH"] != testStatusPath {
				http.Error(w, "", http.StatusNotFound)
				return
			}
			if !strings.Contains(env["PHP_ADMIN_VALUE"], "open_basedir=/usr/local/lib/pocketcloud-php:/run/pocketcloud-php") {
				http.Error(w, "open_basedir restriction in effect", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"php_version":"8.5.12","opcache":{"opcache_enabled":true,"memory_usage":{"used_memory":9000000}}}`)
		default:
			http.Error(w, "File not found.", http.StatusNotFound)
		}
	})
}

// startFakeFPM serves fakeFPM on a Unix socket and writes settings for it as
// the entrypoint would; it returns the settings file.
func startFakeFPM(t *testing.T, broken ...string) string {
	t.Helper()

	socket := serveFCGI(t, "unix", fakeFPM(t, broken...))

	config := filepath.Join(t.TempDir(), "fpm-status.conf")
	content := fmt.Sprintf("; written at start\n[www]\npm.status_listen = %s\npm.status_path = %s\nping.path = \"%s\"\nping.response = pong\n", socket, testStatusPath, testPingPath)
	if err := os.WriteFile(config, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	return config
}

func runCommand(t *testing.T, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestPrintsPoolAndOPcacheAsOneObject(t *testing.T) {
	config := startFakeFPM(t)

	code, stdout, stderr := runCommand(t, "-config", config, "-script", testScript)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if strings.Count(stdout, "\n") != 1 {
		t.Errorf("the output is not one line: %q", stdout)
	}

	var r struct {
		PHPVersion string `json:"php_version"`
		FPM        struct {
			Pool      string           `json:"pool"`
			Accepted  int              `json:"accepted conn"`
			Processes []map[string]any `json:"processes"`
		} `json:"fpm"`
		OPcache struct {
			Enabled bool `json:"opcache_enabled"`
		} `json:"opcache"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(stdout), &r); err != nil {
		t.Fatalf("%v: %s", err, stdout)
	}

	if r.PHPVersion != "8.5.12" || r.FPM.Pool != "www" || r.FPM.Accepted != 12 || len(r.FPM.Processes) != 1 || !r.OPcache.Enabled {
		t.Errorf("printed %s", stdout)
	}
	if r.Errors != nil {
		t.Errorf("errors: %v", r.Errors)
	}
}

func TestKeepsThePartThatWorked(t *testing.T) {
	for _, tc := range []struct {
		broken string
		want   string
		null   string
	}{
		{"opcache", "opcache: " + testScript + " answered with status 500", "opcache"},
		{"status-html", "fpm: the status page did not answer with a JSON object", "fpm"},
	} {
		t.Run(tc.broken, func(t *testing.T) {
			config := startFakeFPM(t, tc.broken)

			code, stdout, stderr := runCommand(t, "-config", config, "-script", testScript)
			if code != 1 {
				t.Fatalf("exit %d", code)
			}

			var r map[string]json.RawMessage
			if err := json.Unmarshal([]byte(stdout), &r); err != nil {
				t.Fatalf("%v: %s", err, stdout)
			}
			if string(r[tc.null]) != "null" {
				t.Errorf("%s is %s, want null", tc.null, r[tc.null])
			}
			for part := range map[string]bool{"fpm": true, "opcache": true} {
				if part != tc.null && !bytes.HasPrefix(r[part], []byte("{")) {
					t.Errorf("%s was lost: %s", part, r[part])
				}
			}
			if !strings.Contains(string(r["errors"]), tc.want) || !strings.Contains(stderr, tc.want) {
				t.Errorf("errors %s, stderr %q; want %q", r["errors"], stderr, tc.want)
			}
		})
	}
}

func TestFallsBackToThePoolWhenItsProcessesAreNotJSON(t *testing.T) {
	config := startFakeFPM(t, "full-invalid")

	code, stdout, stderr := runCommand(t, "-config", config, "-script", testScript)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, stdout, stderr)
	}

	var r report
	if err := json.Unmarshal([]byte(stdout), &r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(r.FPM), `"accepted conn":12`) || len(r.Warnings) != 1 || len(r.Errors) != 0 {
		t.Fatalf("%s", stdout)
	}
}

func TestPing(t *testing.T) {
	config := startFakeFPM(t)
	if code, stdout, stderr := runCommand(t, "-config", config, "-ping"); code != 0 || !strings.Contains(stdout, "answers") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	config = startFakeFPM(t, "ping")
	if code, _, stderr := runCommand(t, "-config", config, "-ping"); code != 1 || !strings.Contains(stderr, `"pang"`) {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestFlagsOverrideTheSettings(t *testing.T) {
	config := startFakeFPM(t)
	settings, err := loadSettings(config, settings{})
	if err != nil {
		t.Fatal(err)
	}

	// what another container would try: the right listener, but guessed paths
	code, stdout, _ := runCommand(t, "-config", filepath.Join(t.TempDir(), "missing.conf"),
		"-listen", settings.Listen, "-status-path", "/status", "-ping-path", "/ping", "-script", testScript)
	if code != 1 || !strings.Contains(stdout, "status page answered with status 404") {
		t.Fatalf("exit %d: %s", code, stdout)
	}

	code, _, _ = runCommand(t, "-config", "/nonexistent", "-listen", settings.Listen, "-ping-path", "/ping", "-ping")
	if code != 1 {
		t.Fatalf("a guessed ping path passed: exit %d", code)
	}
}

func TestMissingSettingsSayWhy(t *testing.T) {
	code, _, stderr := runCommand(t, "-config", filepath.Join(t.TempDir(), "fpm-status.conf"))
	if code != 1 || !strings.Contains(stderr, "entrypoint") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}

	if code, _, _ := runCommand(t, "unexpected"); code != 2 {
		t.Fatalf("an argument gave exit %d", code)
	}
}

func TestParseSettings(t *testing.T) {
	s := parseSettings([]byte(`
; comment
# another
[www]
pm.status_listen = /run/pocketcloud-php/status.sock
pm.status_path='/status-abc'
ping.path = "/ping-abc"
ping.response = pong
listen = 9000
`))
	want := settings{Listen: "/run/pocketcloud-php/status.sock", StatusPath: "/status-abc", PingPath: "/ping-abc", PingResponse: "pong"}
	if s != want {
		t.Fatalf("parsed %+v", s)
	}
}

func TestDialTarget(t *testing.T) {
	for listen, want := range map[string]string{
		"/run/pocketcloud-php/status.sock": "unix /run/pocketcloud-php/status.sock",
		"9001":                             "tcp 127.0.0.1:9001",
		"127.0.0.1:9001":                   "tcp 127.0.0.1:9001",
		"[::1]:9001":                       "tcp [::1]:9001",
		"pc-php-site:9000":                 "tcp pc-php-site:9000",
		// as FPM reads it: anything but a port or host:port is a socket path
		"status.sock":                "unix status.sock",
		"run/pocketcloud-php/x.sock": "unix run/pocketcloud-php/x.sock",
		"9001a":                      "unix 9001a",
	} {
		network, address := dialTarget(listen)
		if got := network + " " + address; got != want {
			t.Errorf("%s: %s, want %s", listen, got, want)
		}
	}
}
