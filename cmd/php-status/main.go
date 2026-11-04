// Command pocketcloud-php-status prints the state of the PHP-FPM in its
// container as one JSON object: the status of the pool and the statistics of
// OPcache. It is part of the PHP runtime images (images/php), where the agent
// runs it with `docker exec`.
//
// It talks FastCGI to the status listener that FPM opens next to the pool
// (pm.status_listen), a Unix socket inside the container. The pool's status
// comes from FPM's status page (?json&full); OPcache's statistics come from a
// small PHP script run by FPM, as only FPM's own processes share its OPcache:
// the command-line PHP has an OPcache of its own.
//
// With -ping it only checks that FPM answers, for the image's health check.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

const (
	// defaultConfig is where the image's entrypoint writes the status settings
	// at every start, with paths that change each time.
	defaultConfig = "/run/pocketcloud-php/fpm-status.conf"
	// defaultScript answers with OPcache's statistics.
	defaultScript = "/usr/local/lib/pocketcloud-php/opcache-status.php"
)

// settings say where FPM's status listener is and at which paths it answers.
type settings struct {
	Listen       string
	StatusPath   string
	PingPath     string
	PingResponse string
}

// report is what the command prints. A part that could not be read is null,
// and the errors say why.
type report struct {
	PHPVersion string          `json:"php_version,omitempty"`
	FPM        json.RawMessage `json:"fpm"`
	OPcache    json.RawMessage `json:"opcache"`
	Errors     []string        `json:"errors,omitempty"`
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// run is the command; it returns the exit status: 0 when every part was read,
// 1 when one was not (the JSON says which) and 2 for a wrong invocation.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("pocketcloud-php-status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfig, "the status settings that the image's entrypoint writes at start")
	listen := flags.String("listen", "", "the status listener, a socket path or host:port, instead of the one in -config")
	statusPath := flags.String("status-path", "", "the path of FPM's status page, instead of the one in -config")
	pingPath := flags.String("ping-path", "", "the path of FPM's ping, instead of the one in -config")
	script := flags.String("script", defaultScript, "the PHP script that reports OPcache's statistics")
	ping := flags.Bool("ping", false, "only check that FPM answers its ping, as a health check")
	timeout := flags.Duration("timeout", 5*time.Second, "how long to wait for FPM")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: pocketcloud-php-status [flags]")
		fmt.Fprintln(stderr, "Prints FPM's pool status and OPcache's statistics as one JSON object.")
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "pocketcloud-php-status takes no arguments, only flags: %q\n", flags.Arg(0))
		return 2
	}

	s, err := loadSettings(*configPath, settings{Listen: *listen, StatusPath: *statusPath, PingPath: *pingPath})
	if err != nil {
		fmt.Fprintln(stderr, "pocketcloud-php-status:", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if *ping {
		if err := checkPing(ctx, s); err != nil {
			fmt.Fprintln(stderr, "pocketcloud-php-status:", err)
			return 1
		}
		fmt.Fprintln(stdout, "FPM answers its ping")
		return 0
	}

	r := collect(ctx, s, *script)

	encoded, err := json.Marshal(r)
	if err != nil {
		fmt.Fprintln(stderr, "pocketcloud-php-status:", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\n", encoded)

	for _, message := range r.Errors {
		fmt.Fprintln(stderr, "pocketcloud-php-status:", message)
	}
	if len(r.Errors) > 0 {
		return 1
	}
	return 0
}

// loadSettings reads the settings the entrypoint wrote; what is set in
// overrides wins. The file may be missing when overrides say everything needed.
func loadSettings(file string, overrides settings) (settings, error) {
	var s settings

	content, err := os.ReadFile(file)
	switch {
	case err == nil:
		s = parseSettings(content)
	case errors.Is(err, fs.ErrNotExist):
		if overrides.Listen == "" {
			return s, fmt.Errorf("FPM's status is off: %s is missing, so the container did not start with the image's entrypoint, or could not write it", file)
		}
	default:
		return s, err
	}

	if overrides.Listen != "" {
		s.Listen = overrides.Listen
	}
	if overrides.StatusPath != "" {
		s.StatusPath = overrides.StatusPath
	}
	if overrides.PingPath != "" {
		s.PingPath = overrides.PingPath
	}
	if s.PingResponse == "" {
		s.PingResponse = "pong"
	}

	if s.Listen == "" {
		return s, fmt.Errorf("%s does not say where FPM's status listener is (pm.status_listen)", file)
	}

	return s, nil
}

// parseSettings reads the entries of a pool configuration that matter here.
// Values may be quoted; comments and section names are skipped.
func parseSettings(content []byte) settings {
	var s settings

	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}

		switch key {
		case "pm.status_listen":
			s.Listen = value
		case "pm.status_path":
			s.StatusPath = value
		case "ping.path":
			s.PingPath = value
		case "ping.response":
			s.PingResponse = value
		}
	}

	return s
}

// dialTarget turns an FPM listen value into a network and an address: a path
// is a Unix socket, a bare port is on the loopback address.
func dialTarget(listen string) (network, address string) {
	switch {
	case strings.HasPrefix(listen, "/"):
		return "unix", listen
	case !strings.Contains(listen, ":"):
		return "tcp", "127.0.0.1:" + listen
	default:
		return "tcp", listen
	}
}

// collect reads both parts. One that fails leaves an error and null, so the
// other is still reported.
func collect(ctx context.Context, s settings, script string) report {
	r := report{FPM: json.RawMessage("null"), OPcache: json.RawMessage("null")}

	if pool, err := fpmStatus(ctx, s); err != nil {
		r.Errors = append(r.Errors, "fpm: "+err.Error())
	} else {
		r.FPM = pool
	}

	if version, opcache, err := opcacheStatus(ctx, s, script); err != nil {
		r.Errors = append(r.Errors, "opcache: "+err.Error())
	} else {
		r.PHPVersion = version
		r.OPcache = opcache
	}

	return r
}

// fpmStatus fetches the full status of the pool as JSON.
func fpmStatus(ctx context.Context, s settings) (json.RawMessage, error) {
	if s.StatusPath == "" {
		return nil, errors.New("the settings do not say the path of the status page (pm.status_path)")
	}

	response, err := request(ctx, s, requestParams(s.StatusPath, s.StatusPath, "json&full"))
	if err != nil {
		return nil, err
	}
	if response.Status != 200 {
		return nil, fmt.Errorf("the status page answered with status %d%s", response.Status, stderrNote(response))
	}

	body := bytes.TrimSpace(response.Body)
	if !json.Valid(body) || !bytes.HasPrefix(body, []byte("{")) {
		return nil, errors.New("the status page did not answer with a JSON object")
	}

	return body, nil
}

// opcacheStatus runs the script that reports OPcache's statistics. The status
// pool takes the site pool's php_admin_value settings, so the request lifts an
// open_basedir that would keep the script out, the files a site may have FPM
// run before and after every script, and a restriction of OPcache's functions
// to the site's own files.
func opcacheStatus(ctx context.Context, s settings, script string) (version string, opcache json.RawMessage, err error) {
	params := requestParams("/"+path.Base(script), script, "")
	params["DOCUMENT_ROOT"] = path.Dir(script)
	params["PHP_ADMIN_VALUE"] = strings.Join([]string{
		"open_basedir=" + path.Dir(script),
		"auto_prepend_file=",
		"auto_append_file=",
		"opcache.restrict_api=",
	}, "\n")

	response, err := request(ctx, s, params)
	if err != nil {
		return "", nil, err
	}
	if response.Status != 200 {
		return "", nil, fmt.Errorf("%s answered with status %d%s", script, response.Status, stderrNote(response))
	}

	var answer struct {
		PHPVersion string          `json:"php_version"`
		OPcache    json.RawMessage `json:"opcache"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(response.Body), &answer); err != nil || !bytes.HasPrefix(answer.OPcache, []byte("{")) {
		return "", nil, fmt.Errorf("%s did not answer with OPcache's statistics", script)
	}

	return answer.PHPVersion, answer.OPcache, nil
}

// checkPing asks FPM's ping and expects its configured answer.
func checkPing(ctx context.Context, s settings) error {
	if s.PingPath == "" {
		return errors.New("the settings do not say the path of the ping (ping.path)")
	}

	response, err := request(ctx, s, requestParams(s.PingPath, s.PingPath, ""))
	if err != nil {
		return err
	}
	if response.Status != 200 || strings.TrimSpace(string(response.Body)) != s.PingResponse {
		return fmt.Errorf("the ping answered with status %d and %q", response.Status, truncate(string(response.Body), 80))
	}

	return nil
}

func request(ctx context.Context, s settings, params map[string]string) (*fcgiResponse, error) {
	network, address := dialTarget(s.Listen)

	response, err := fcgiGet(ctx, network, address, params)
	if err != nil {
		return nil, fmt.Errorf("asking FPM at %s: %w", s.Listen, err)
	}

	return response, nil
}

// requestParams are the CGI variables of a GET request, as a web server sends them.
func requestParams(scriptName, scriptFilename, query string) map[string]string {
	uri := scriptName
	if query != "" {
		uri += "?" + query
	}

	return map[string]string{
		"GATEWAY_INTERFACE": "CGI/1.1",
		"REQUEST_METHOD":    "GET",
		"REQUEST_URI":       uri,
		"SCRIPT_NAME":       scriptName,
		"SCRIPT_FILENAME":   scriptFilename,
		"QUERY_STRING":      query,
		"SERVER_PROTOCOL":   "HTTP/1.1",
		"SERVER_SOFTWARE":   "pocketcloud-php-status",
		"SERVER_NAME":       "localhost",
		"REMOTE_ADDR":       "127.0.0.1",
	}
}

// stderrNote quotes what FPM wrote to the error stream, which says why a script
// failed.
func stderrNote(response *fcgiResponse) string {
	message := strings.TrimSpace(string(response.Stderr))
	if message == "" {
		return ""
	}
	return ": " + truncate(message, 300)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
