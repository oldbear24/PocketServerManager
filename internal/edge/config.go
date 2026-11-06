// Package edge generates the complete configuration of an edge proxy: Caddy's
// JSON config for one host, built from its routes and certificates. The panel
// sends it with edge.apply and the agent loads it through Caddy's admin API
// (ADR 0002). Caddy never manages certificates itself: automatic HTTPS is off,
// and certificates are files the agent installs with cert.install.
package edge

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
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
	// KindSite serves the files of a static site's current release (#21).
	KindSite = "site"
)

// SitesRoot is where the edge mounts the sites volume, read-only.
const SitesRoot = "/srv/sites"

// StreamCloseDelay is how long a WebSocket through a route survives a reload.
const StreamCloseDelay = "24h"

// HSTSValue is the Strict-Transport-Security header of routes that ask for it:
// a year, without includeSubDomains or preload, which are hard to take back.
const HSTSValue = "max-age=31536000"

// Route is one hostname served by the edge.
type Route struct {
	// Hostname is the name the route answers to; "*.example.com" is a wildcard.
	Hostname string `json:"hostname"`
	// Kind is KindProxy, KindRedirect or KindSite.
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
	// BasicAuth asks for a user name and password before a request reaches
	// the upstream or the files, for KindProxy and KindSite. Such a route is
	// never served over plain HTTP: it redirects to HTTPS, or refuses until it
	// has a certificate.
	BasicAuth []BasicAuthUser `json:"basic_auth,omitempty"`

	// Site is how a KindSite route serves its files.
	Site *SiteOptions `json:"site,omitempty"`
}

// SiteOptions are how the edge serves a static site.
type SiteOptions struct {
	// Root is the directory served, under SitesRoot: the site's current
	// release, a symlink switched when a release goes live.
	Root string `json:"root"`
	// SPA answers a path that is no file with /index.html, for single-page
	// applications that route in the browser.
	SPA bool `json:"spa,omitempty"`
	// NotFound is the page sent with a 404, such as /404.html.
	NotFound string `json:"not_found,omitempty"`
	// Headers are added to the responses of the paths they match.
	Headers []SiteHeader `json:"headers,omitempty"`
	// Cache sets how long browsers may keep the files of a path.
	Cache []CacheRule `json:"cache,omitempty"`
}

// SiteHeader is a response header for the paths that match Path: "/" plus a
// path that may end in *, or *.ext; empty for every path.
type SiteHeader struct {
	Path  string `json:"path,omitempty"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

// CacheRule lets browsers keep the files of Path for MaxAge seconds.
type CacheRule struct {
	Path   string `json:"path"`
	MaxAge int    `json:"max_age"`
}

// BasicAuthUser may sign in to a route.
type BasicAuthUser struct {
	Username string `json:"username"`
	// Hash is the password's bcrypt hash, as htpasswd -B or caddy
	// hash-password make it: $2a$, $2b$ or $2y$.
	Hash string `json:"hash"`
}

// BasicAuthRealm is what browsers show when they ask for the password.
const BasicAuthRealm = "PocketCloud"

// secretPrefix starts a JSON string that stands for a secret of the config.
const secretPrefix = "pocketcloud-secret:"

var secretRefs = regexp.MustCompile(`"` + secretPrefix + `([0-9a-f]{32})"`)

// secretName names a password hash by its own digest, so that a new hash is
// a new name and a new config.
func secretName(hash string) string {
	sum := sha256.Sum256([]byte(hash))
	return hex.EncodeToString(sum[:16])
}

// SecretRef is what a config holds in place of a password hash.
func SecretRef(hash string) string { return secretPrefix + secretName(hash) }

// Secrets are the values the references of a config stand for: each user's
// hash, base64-encoded as Caddy takes it in every Caddy 2 release.
func Secrets(c Config) map[string]string {
	secrets := map[string]string{}
	for _, route := range c.Routes {
		for _, user := range route.BasicAuth {
			secrets[secretName(user.Hash)] = base64.StdEncoding.EncodeToString([]byte(user.Hash))
		}
	}
	return secrets
}

// SecretNames are the names of the secrets a config refers to.
func SecretNames(caddyJSON string) []string {
	names := []string{}
	for _, match := range secretRefs.FindAllStringSubmatch(caddyJSON, -1) {
		if !slices.Contains(names, match[1]) {
			names = append(names, match[1])
		}
	}
	return names
}

// ResolveSecrets puts the secrets into a config where it refers to them. A
// reference without its secret is an error: Caddy would take the reference
// for the hash.
func ResolveSecrets(caddyJSON string, secrets map[string]string) (string, error) {
	var missing error
	resolved := secretRefs.ReplaceAllStringFunc(caddyJSON, func(ref string) string {
		name := secretRefs.FindStringSubmatch(ref)[1]
		value, ok := secrets[name]
		if !ok {
			missing = fmt.Errorf("the config refers to a secret it was not given (%s)", name)
			return ref
		}
		quoted, _ := json.Marshal(value)
		return string(quoted)
	})
	return resolved, missing
}

var userName = regexp.MustCompile(`^[A-Za-z0-9._@+-]+$`)

var bcryptHash = regexp.MustCompile(`^\$2[aby]\$(0[4-9]|[12][0-9]|3[01])\$[./A-Za-z0-9]{53}$`)

// ValidBasicAuth checks the users of a route: names without colons or
// control characters, each once, and a bcrypt hash for each.
func ValidBasicAuth(users []BasicAuthUser) error {
	seen := map[string]bool{}
	for _, user := range users {
		// letters, digits and . _ @ + -: Caddy fills in placeholders in user
		// names, such as {env.*} and {file.*}
		if user.Username == "" || len(user.Username) > 64 || !userName.MatchString(user.Username) {
			return fmt.Errorf("%q is not a user name for basic authentication: use letters, digits and . _ @ + -", user.Username)
		}
		if seen[user.Username] {
			return fmt.Errorf("the user %s is there twice", user.Username)
		}
		seen[user.Username] = true
		if !bcryptHash.MatchString(user.Hash) {
			return fmt.Errorf("the password of %s is not a bcrypt hash: make one with htpasswd -nB %s, or caddy hash-password", user.Username, user.Username)
		}
	}
	return nil
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
			if err := ValidRedirect(route.RedirectTo); err != nil {
				return fmt.Errorf("%s: %w", host, err)
			}
		case KindSite:
			if err := ValidSiteOptions(route.Site); err != nil {
				return fmt.Errorf("%s: %w", host, err)
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
		if len(route.BasicAuth) > 0 {
			if route.Kind != KindProxy && route.Kind != KindSite {
				return fmt.Errorf("%s: only a route to a container or a site can ask for a password", host)
			}
			if err := ValidBasicAuth(route.BasicAuth); err != nil {
				return fmt.Errorf("%s: %w", host, err)
			}
		}
	}

	return nil
}

var (
	headerName = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]+$`)
	sitePath   = regexp.MustCompile(`^(/[^\s{}]*|\*\.[A-Za-z0-9]+)$`)
)

// ValidSiteOptions checks how a site is served.
func ValidSiteOptions(options *SiteOptions) error {
	if options == nil {
		return errors.New("a site route needs its options")
	}
	if !strings.HasPrefix(options.Root, SitesRoot+"/") || strings.Contains(options.Root, "..") || strings.ContainsAny(options.Root, "{} ") {
		return fmt.Errorf("the site's root %q is not in %s", options.Root, SitesRoot)
	}
	if options.NotFound != "" && (!strings.HasPrefix(options.NotFound, "/") || strings.Contains(options.NotFound, "..") || strings.ContainsAny(options.NotFound, "{} ")) {
		return fmt.Errorf("the 404 page %q is not a path such as /404.html", options.NotFound)
	}
	for _, header := range options.Headers {
		if !headerName.MatchString(header.Name) {
			return fmt.Errorf("%q is not a header name", header.Name)
		}
		if strings.ContainsAny(header.Value, "\r\n") || strings.Contains(header.Value, "{") {
			return fmt.Errorf("the value of %s has a line break or a placeholder", header.Name)
		}
		if header.Path != "" && !sitePath.MatchString(header.Path) {
			return fmt.Errorf("%q is not a path such as /assets/* or *.css", header.Path)
		}
	}
	for _, rule := range options.Cache {
		if !sitePath.MatchString(rule.Path) {
			return fmt.Errorf("%q is not a path such as /assets/* or *.css", rule.Path)
		}
		if rule.MaxAge < 0 || rule.MaxAge > 31536000 {
			return fmt.Errorf("%s: a cache time from 0 seconds to a year", rule.Path)
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

// ValidRedirect checks a redirect target. Caddy fills in placeholders such as
// {env.*} and {file.*} in the headers it sends, so a target with braces could
// make the edge answer with the content of any file it can read, its keys among
// them; braces are refused, and %7B and %7D work instead.
func ValidRedirect(target string) error {
	if strings.ContainsAny(target, "{}") {
		return errors.New("a redirect target cannot contain { or }: write them as %7B and %7D")
	}
	// a backslash would escape what follows for Caddy, and spaces and quotes
	// have no place in a URL
	if strings.ContainsAny(target, "\\\"' \t\r\n") {
		return errors.New("a redirect target cannot contain backslashes, quotes or spaces")
	}
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return errors.New("the redirect target must be an http or https URL")
	}
	return nil
}

// RedirectsToItself tells whether a redirect of hostname to target would loop:
// a redirect domain answers every request with the redirect, so any target on
// the same name, whatever its path, sends the browser round in circles.
func RedirectsToItself(hostname, target string) bool {
	parsed, err := url.Parse(target)
	return err == nil && parsed.Port() == "" && strings.EqualFold(parsed.Hostname(), hostname)
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
	return ValidContainerName(host)
}

// ValidContainerName checks an upstream: a container name as Docker allows it.
// Addresses are refused, and so is the edge itself: localhost or the edge's
// own name would make it proxy to itself, every request multiplying, and an IP
// would publish whatever answers there.
func ValidContainerName(name string) error {
	if name == "" || !letterOrDigit(rune(name[0])) {
		return fmt.Errorf("%q is not a valid container name", name)
	}
	for _, r := range name {
		if !letterOrDigit(r) && !strings.ContainsRune("-_.", r) {
			return fmt.Errorf("%q is not a valid container name", name)
		}
	}
	lower := strings.ToLower(name)
	if lower == "localhost" || strings.HasPrefix(lower, "localhost.") || lower == "pocketcloud-edge" || net.ParseIP(name) != nil || allDigitsAndDots(name) {
		return fmt.Errorf("%s is not a container of this server", name)
	}
	return nil
}

func allDigitsAndDots(name string) bool {
	for _, r := range name {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
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
	// the handshake of each name serves the certificate its route has, not
	// any loaded one that covers the name: a certificate of the internal CA
	// must not answer for a name that has a public one (#26)
	policies := []any{}

	for _, route := range routes {
		if route.Certificate != "" {
			secure = append(secure, hostRoute(route, true))
			policies = append(policies, object{
				"match":                 object{"sni": []string{route.Hostname}},
				"certificate_selection": object{"any_tag": []string{route.Certificate}},
			})
		} else {
			// a wildcard certificate may still complete the handshake for this
			// name; without this the wildcard's route would answer it on 443
			secure = append(secure, httpOnly(route.Hostname))
		}
		switch {
		// a password never crosses the network in the clear
		case route.HTTPSRedirect || len(route.BasicAuth) > 0 && route.Certificate != "":
			plain = append(plain, httpsRedirect(route.Hostname))
		case len(route.BasicAuth) > 0:
			plain = append(plain, needsHTTPS(route.Hostname))
		default:
			plain = append(plain, hostRoute(route, false))
		}
	}

	plain = append(plain, notFound())
	secure = append(secure, notFound())

	// a site's own 404 page, for the errors its files give
	errorRoutes := []any{}
	for _, route := range routes {
		if route.Kind == KindSite && route.Site.NotFound != "" {
			errorRoutes = append(errorRoutes, siteNotFound(route))
		}
	}
	withErrors := func(server object) object {
		if len(errorRoutes) > 0 {
			server["errors"] = object{"routes": errorRoutes}
		}
		return server
	}

	servers := object{
		"http": withErrors(object{
			"listen":          []string{":80"},
			"automatic_https": object{"disable": true},
			"routes":          plain,
		}),
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
		servers["https"] = withErrors(object{
			"listen":                  []string{":443"},
			"automatic_https":         object{"disable": true},
			"protocols":               []string{"h1", "h2", "h3"},
			"tls_connection_policies": append(policies, object{}),
			"routes":                  secure,
		})
	}

	apps["http"] = object{
		// request counts per handler and hostname, read by the agent from the
		// admin API; no grace period, so a reload lets every request under way
		// finish
		"metrics": object{"per_host": true},
		"servers": servers,
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

// httpOnly sends HTTPS requests for a name served over HTTP only back to HTTP.
func httpOnly(hostname string) object {
	return object{
		"match": hostMatch(hostname),
		"handle": []any{object{
			"handler":     "static_response",
			"status_code": 308,
			"headers":     object{"Location": []string{"http://{http.request.host}{http.request.uri}"}},
		}},
		"terminal": true,
	}
}

// needsHTTPS answers plain HTTP requests for a route with a password that has
// no certificate yet.
func needsHTTPS(hostname string) object {
	return object{
		"match": hostMatch(hostname),
		"handle": []any{object{
			"handler":     "static_response",
			"status_code": 503,
			"headers":     object{"Content-Type": []string{"text/plain; charset=utf-8"}, "Retry-After": []string{"300"}},
			"body":        "This site asks for a password, so it is served over HTTPS only, once it has its certificate.\n",
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

	if len(route.BasicAuth) > 0 && (route.Kind == KindProxy || route.Kind == KindSite) {
		users := slices.Clone(route.BasicAuth)
		slices.SortFunc(users, func(a, b BasicAuthUser) int { return strings.Compare(a.Username, b.Username) })
		accounts := make([]any, 0, len(users))
		for _, user := range users {
			// a reference: the hash reaches the edge with the command, and is
			// never stored with it
			accounts = append(accounts, object{"username": user.Username, "password": SecretRef(user.Hash)})
		}
		handlers = append(handlers, object{
			"handler": "authentication",
			"providers": object{"http_basic": object{
				"accounts": accounts,
				"hash":     object{"algorithm": "bcrypt"},
				"realm":    BasicAuthRealm,
			}},
		})
	}

	if route.Compress && (route.Kind == KindProxy || route.Kind == KindSite) {
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
			// a reload would otherwise cut WebSockets and other upgraded
			// connections at once; the old config is kept until they end
			"stream_close_delay": StreamCloseDelay,
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
	case KindSite:
		handlers = append(handlers, object{"handler": "subroute", "routes": siteRoutes(route)})
	}

	return object{
		"match":    hostMatch(route.Hostname),
		"handle":   handlers,
		"terminal": true,
	}
}

// siteRoutes serve a site's files: its headers and cache rules first, then a
// path that is no file goes to /index.html for a single-page application, and
// the file server answers. A file's precompressed copy is sent to a browser
// that takes it.
func siteRoutes(route Route) []any {
	options := route.Site
	// the site's own 404 page answers its own errors only, not those of a
	// site whose name its wildcard covers
	routes := []any{object{"handle": []any{object{"handler": "vars", siteVar: options.Root}}}}
	pathMatch := func(path string) []any {
		if path == "" {
			return nil
		}
		return []any{object{"path": []string{path}}}
	}
	for _, header := range options.Headers {
		routes = append(routes, withMatch(pathMatch(header.Path), object{
			"handle": []any{object{"handler": "headers", "response": object{"set": object{header.Name: []string{header.Value}}}}},
		}))
	}
	for _, rule := range options.Cache {
		value := "no-cache"
		if rule.MaxAge > 0 {
			value = "public, max-age=" + strconv.Itoa(rule.MaxAge)
		}
		routes = append(routes, withMatch(pathMatch(rule.Path), object{
			"handle": []any{object{"handler": "headers", "response": object{"set": object{"Cache-Control": []string{value}}}}},
		}))
	}
	if options.SPA {
		routes = append(routes, object{
			"match": []any{object{"file": object{
				"root":      options.Root,
				"try_files": []string{"{http.request.uri.path}", "/index.html"},
			}}},
			"handle": []any{object{"handler": "rewrite", "uri": "{http.matchers.file.relative}"}},
		})
	}
	routes = append(routes, object{"handle": []any{siteFiles(options.Root, "")}})
	return routes
}

func withMatch(match []any, route object) object {
	if match != nil {
		route["match"] = match
	}
	return route
}

// siteFiles is the file server of a site. Its root is the site's current
// release, a symlink: switching it serves the new release from the next
// request on.
func siteFiles(root, status string) object {
	server := object{
		"handler":             "file_server",
		"root":                root,
		"precompressed":       object{"br": object{}, "zstd": object{}, "gzip": object{}},
		"precompressed_order": []string{"br", "zstd", "gzip"},
	}
	if status != "" {
		server["status_code"] = status
	}
	return server
}

// siteVar names the site whose route a request went through.
const siteVar = "pocketcloud_site"

// siteNotFound sends a site's own 404 page with the 404 of a file that is not
// there.
func siteNotFound(route Route) object {
	return object{
		"match": []any{object{"host": []string{route.Hostname}, "vars": object{siteVar: []string{route.Site.Root}}}},
		"handle": []any{object{"handler": "subroute", "routes": []any{object{
			"match": []any{object{"expression": "{http.error.status_code} == 404"}},
			"handle": []any{
				object{"handler": "rewrite", "uri": route.Site.NotFound},
				siteFiles(route.Site.Root, "{http.error.status_code}"),
			},
		}}}},
		"terminal": true,
	}
}

// redirectLocation keeps the request's path and query when the target has no
// path of its own: example.com → www.example.com sends /a?b to /a?b.
func redirectLocation(target string) string {
	parsed, err := url.Parse(target)
	if err != nil {
		return target
	}
	canonical := parsed.String()
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return canonical
	}
	return strings.TrimSuffix(canonical, "/") + "{http.request.uri}"
}
