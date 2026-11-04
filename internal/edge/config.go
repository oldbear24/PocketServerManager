// Package edge generates the complete configuration of an edge proxy: Caddy's
// JSON config for one host, built from its routes and certificates. The panel
// sends it with edge.apply and the agent loads it through Caddy's admin API
// (ADR 0002). Caddy never manages certificates itself: automatic HTTPS is off,
// and certificates are files the agent installs with cert.install.
package edge

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Paths inside the edge container. The agent mounts its own directories there.
const (
	// AdminSocket is where Caddy's admin API listens. It is a Unix socket, so
	// nothing on a network can reach it.
	AdminSocket = "/pocketcloud/run/admin.sock"
	// CertDir holds the installed certificates, <id>.crt and <id>.key.
	CertDir = "/pocketcloud/certs"
	// ChallengeRoot is served for /.well-known/acme-challenge/ on port 80; the
	// answers are files in its .well-known/acme-challenge directory.
	ChallengeRoot = "/pocketcloud/acme"
)

// Kinds of routes.
const (
	// KindProxy forwards requests to a container on the edge network.
	KindProxy = "proxy"
	// KindRedirect answers every request with a redirect.
	KindRedirect = "redirect"
)

// HSTSValue is the Strict-Transport-Security header of routes that ask for it:
// a year, without includeSubDomains or preload, which are hard to take back.
const HSTSValue = "max-age=31536000"

// Route is one hostname served by the edge.
type Route struct {
	// Hostname is the name the route answers to; "*.example.com" is a wildcard.
	Hostname string `json:"hostname"`
	// Kind is KindProxy or KindRedirect.
	Kind string `json:"kind"`
	// Upstream is "<name>:<port>" of the container, for KindProxy.
	Upstream string `json:"upstream,omitempty"`
	// RedirectTo is the target URL, for KindRedirect. The path and query of the
	// request are kept when it has no path of its own.
	RedirectTo string `json:"redirect_to,omitempty"`
	// RedirectPermanent answers with 308 instead of 307.
	RedirectPermanent bool `json:"redirect_permanent,omitempty"`
	// Certificate is the id of the installed certificate that covers the
	// hostname. Without one, the route is served over plain HTTP only.
	Certificate string `json:"certificate,omitempty"`
	// HTTPSRedirect sends plain HTTP requests to HTTPS. It needs a certificate.
	HTTPSRedirect bool `json:"https_redirect,omitempty"`
	// HSTS adds the Strict-Transport-Security header to HTTPS responses.
	HSTS bool `json:"hsts,omitempty"`
	// Compress encodes responses with zstd or gzip when the client accepts it.
	Compress bool `json:"compress,omitempty"`
}

// Certificate is one certificate installed on the edge.
type Certificate struct {
	// ID names the files: <CertDir>/<ID>.crt and <ID>.key.
	ID string `json:"id"`
}

// Config is everything an edge serves.
type Config struct {
	Routes       []Route       `json:"routes"`
	Certificates []Certificate `json:"certificates"`
}

// Validate checks a config before it is built, so a mistake is reported by the
// panel with the route it belongs to instead of as an error of Caddy's.
func (c Config) Validate() error {
	installed := map[string]bool{}
	for _, cert := range c.Certificates {
		if !validID(cert.ID) {
			return fmt.Errorf("the certificate id %q is not a valid file name", cert.ID)
		}
		installed[cert.ID] = true
	}

	seen := map[string]bool{}
	for _, route := range c.Routes {
		host := strings.ToLower(route.Hostname)
		if err := ValidHostname(host); err != nil {
			return err
		}
		if seen[host] {
			return fmt.Errorf("%s has more than one route", host)
		}
		seen[host] = true

		switch route.Kind {
		case KindProxy:
			if err := validUpstream(route.Upstream); err != nil {
				return fmt.Errorf("%s: %w", host, err)
			}
		case KindRedirect:
			target, err := url.Parse(route.RedirectTo)
			if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
				return fmt.Errorf("%s: the redirect target must be an http or https URL", host)
			}
		default:
			return fmt.Errorf("%s: unknown kind of route %q", host, route.Kind)
		}

		if route.Certificate != "" && !installed[route.Certificate] {
			return fmt.Errorf("%s: the certificate %s is not installed on this edge", host, route.Certificate)
		}
		if route.HTTPSRedirect && route.Certificate == "" {
			return fmt.Errorf("%s: redirecting to HTTPS needs a certificate", host)
		}
	}

	return nil
}

// ValidHostname checks a hostname, or a wildcard of one level, as routes use it.
func ValidHostname(host string) error {
	name := strings.TrimPrefix(host, "*.")
	if host == "" || len(host) > 253 || name != strings.ToLower(name) {
		return fmt.Errorf("%q is not a valid hostname", host)
	}
	if net.ParseIP(name) != nil {
		return fmt.Errorf("%s is an IP address: a route needs a hostname", host)
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return fmt.Errorf("%s is not a fully qualified hostname", host)
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("%q is not a valid hostname", host)
		}
		for _, r := range label {
			if !lowerOrDigit(r) && r != '-' {
				return fmt.Errorf("%q is not a valid hostname", host)
			}
		}
	}
	return nil
}

func validUpstream(upstream string) error {
	host, port, err := net.SplitHostPort(upstream)
	if err != nil || host == "" {
		return errors.New("the target must be <container>:<port>")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("%s is not a valid port", port)
	}
	for _, r := range host {
		if !letterOrDigit(r) && !strings.ContainsRune("-_.", r) {
			return fmt.Errorf("%q is not a valid container name", host)
		}
	}
	return nil
}

// validID allows the record ids of PocketBase and similar plain names, which
// end up in file names.
func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !letterOrDigit(r) && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func lowerOrDigit(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
}

func letterOrDigit(r rune) bool {
	return lowerOrDigit(r) || r >= 'A' && r <= 'Z'
}

// object is a JSON object of the config; a map keeps the generated JSON sorted
// by key, so the same input always gives the same bytes.
type object = map[string]any

// Admin is the admin block of every config. The agent sets it on whatever it
// loads, so no config can move the admin API somewhere the agent cannot reach.
func Admin() object {
	return object{
		"listen": "unix/" + AdminSocket,
		"config": object{"persist": true},
	}
}

// Build returns the complete Caddy config. Routes are ordered by hostname, with
// exact names before wildcards, so the output only changes when the input does.
func Build(c Config) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}

	routes := slices.Clone(c.Routes)
	for i := range routes {
		routes[i].Hostname = strings.ToLower(routes[i].Hostname)
	}
	slices.SortFunc(routes, func(a, b Route) int {
		// Caddy tries routes in order: a name must win over a wildcard covering it
		aw, bw := strings.HasPrefix(a.Hostname, "*."), strings.HasPrefix(b.Hostname, "*.")
		if aw != bw {
			if aw {
				return 1
			}
			return -1
		}
		return strings.Compare(a.Hostname, b.Hostname)
	})

	plain := []any{challengeRoute()}
	secure := []any{}

	for _, route := range routes {
		if route.Certificate != "" {
			secure = append(secure, hostRoute(route, true))
		}
		if route.HTTPSRedirect {
			plain = append(plain, httpsRedirect(route.Hostname))
		} else {
			plain = append(plain, hostRoute(route, false))
		}
	}

	plain = append(plain, notFound())
	secure = append(secure, notFound())

	servers := object{
		"http": object{
			"listen":          []string{":80"},
			"automatic_https": object{"disable": true},
			"routes":          plain,
		},
	}

	apps := object{}

	if len(c.Certificates) > 0 {
		files := make([]any, 0, len(c.Certificates))
		certs := slices.Clone(c.Certificates)
		slices.SortFunc(certs, func(a, b Certificate) int { return strings.Compare(a.ID, b.ID) })
		for _, cert := range certs {
			files = append(files, object{
				"certificate": CertDir + "/" + cert.ID + ".crt",
				"key":         CertDir + "/" + cert.ID + ".key",
				"tags":        []string{cert.ID},
			})
		}
		// loaded files are never managed: with automatic HTTPS off, Caddy obtains
		// and renews nothing by itself
		apps["tls"] = object{
			"certificates": object{"load_files": files},
		}

		// with automatic HTTPS off, a server only speaks TLS when it has a
		// connection policy; HTTP/3 needs 443/udp published as well
		servers["https"] = object{
			"listen":                  []string{":443"},
			"automatic_https":         object{"disable": true},
			"protocols":               []string{"h1", "h2", "h3"},
			"tls_connection_policies": []any{object{}},
			"routes":                  secure,
		}
	}

	apps["http"] = object{
		// a reload waits this long for requests under way before it closes them
		"grace_period": "30s",
		"servers":      servers,
	}

	config := object{
		"admin": Admin(),
		"apps":  apps,
	}

	return json.MarshalIndent(config, "", "  ")
}

func hostMatch(hostname string) []any {
	return []any{object{"host": []string{hostname}}}
}

// challengeRoute answers HTTP-01 challenges for every hostname, before any
// redirect, from the files the agent writes.
func challengeRoute() object {
	return object{
		"match": []any{object{"path": []string{"/.well-known/acme-challenge/*"}}},
		"handle": []any{
			object{"handler": "file_server", "root": ChallengeRoot},
		},
		"terminal": true,
	}
}

func httpsRedirect(hostname string) object {
	return object{
		"match": hostMatch(hostname),
		"handle": []any{object{
			"handler":     "static_response",
			"status_code": 308,
			"headers":     object{"Location": []string{"https://{http.request.host}{http.request.uri}"}},
		}},
		"terminal": true,
	}
}

func notFound() object {
	return object{
		"handle": []any{object{
			"handler":     "static_response",
			"status_code": 404,
			"headers":     object{"Content-Type": []string{"text/plain; charset=utf-8"}},
			"body":        "No site is configured for this address.\n",
		}},
	}
}

func hostRoute(route Route, secure bool) object {
	handlers := []any{}

	if secure && route.HSTS {
		handlers = append(handlers, object{
			"handler":  "headers",
			"response": object{"set": object{"Strict-Transport-Security": []string{HSTSValue}}},
		})
	}

	if route.Compress && route.Kind == KindProxy {
		handlers = append(handlers, object{
			"handler":   "encode",
			"encodings": object{"zstd": object{}, "gzip": object{}},
			"prefer":    []string{"zstd", "gzip"},
		})
	}

	switch route.Kind {
	case KindProxy:
		handlers = append(handlers, object{
			"handler":   "reverse_proxy",
			"upstreams": []any{object{"dial": route.Upstream}},
		})
	case KindRedirect:
		status := 307
		if route.RedirectPermanent {
			status = 308
		}
		handlers = append(handlers, object{
			"handler":     "static_response",
			"status_code": status,
			"headers":     object{"Location": []string{redirectLocation(route.RedirectTo)}},
		})
	}

	return object{
		"match":    hostMatch(route.Hostname),
		"handle":   handlers,
		"terminal": true,
	}
}

// redirectLocation keeps the request's path and query when the target has no
// path of its own: example.com → www.example.com sends /a?b to /a?b.
func redirectLocation(target string) string {
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" {
		return target
	}
	return strings.TrimSuffix(target, "/") + "{http.request.uri}"
}
