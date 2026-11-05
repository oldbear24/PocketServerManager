package edge

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden configs in testdata")

// golden are the configs checked byte for byte, and loaded into a real Caddy by
// TestGoldenConfigsLoadInCaddy.
var golden = map[string]Config{
	"empty": {},
	"full": {
		Certificates: []Certificate{{ID: "certwild"}, {ID: "certapp"}},
		Routes: []Route{
			{Hostname: "app.example.com", Kind: KindProxy, Upstream: "web-app-1:8080", Certificate: "certapp", HTTPSRedirect: true, HSTS: true, Compress: true},
			{Hostname: "*.example.com", Kind: KindProxy, Upstream: "catchall:80", Certificate: "certwild", HTTPSRedirect: true},
			{Hostname: "plain.example.org", Kind: KindProxy, Upstream: "plain:3000", Compress: true},
			{Hostname: "admin.example.com", Kind: KindProxy, Upstream: "admin:9000", Certificate: "certapp", BasicAuth: []BasicAuthUser{
				{Username: "bob", Hash: "$2a$05$6PVNJize1kOg8S8pGtSUw.DfmvfGyAZxXEGMlQNylBUi5z9iaiBRW"},
				{Username: "alice", Hash: "$2a$05$NnlHVH0N.LRsqgwlooqk/uPIOW3vVv7Z2s9b5lkSo9KUdbOFJB.TS"},
			}},
			{Hostname: "private.example.org", Kind: KindProxy, Upstream: "private:80", BasicAuth: []BasicAuthUser{
				{Username: "carol", Hash: "$2a$05$cdSx43KWUy4Imu5KZU9RrOmFF/YsG/lIIWlXA8QAYE1NcKuKSOyte"},
			}},
			{Hostname: "example.com", Kind: KindRedirect, RedirectTo: "https://www.example.com", RedirectPermanent: true, Certificate: "certapp"},
			{Hostname: "old.example.org", Kind: KindRedirect, RedirectTo: "https://new.example.org/landing"},
		},
	},
}

func TestGoldenConfigs(t *testing.T) {
	for name, config := range golden {
		t.Run(name, func(t *testing.T) {
			got, err := Build(config)
			if err != nil {
				t.Fatal(err)
			}

			path := filepath.Join("testdata", name+".json")
			if *update {
				if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run go test ./internal/edge -update to create it)", err)
			}
			if !bytes.Equal(append(got, '\n'), want) {
				t.Errorf("%s differs from %s; run go test ./internal/edge -update and review the diff", name, path)
			}
		})
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	config := golden["full"]
	first, err := Build(config)
	if err != nil {
		t.Fatal(err)
	}

	reversed := Config{}
	for i := len(config.Routes) - 1; i >= 0; i-- {
		reversed.Routes = append(reversed.Routes, config.Routes[i])
	}
	for i := len(config.Certificates) - 1; i >= 0; i-- {
		reversed.Certificates = append(reversed.Certificates, config.Certificates[i])
	}

	second, err := Build(reversed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("the order of routes and certificates changes the generated config")
	}
}

// decoded is the part of a generated config the tests look at.
type decoded struct {
	Admin struct {
		Listen string `json:"listen"`
	} `json:"admin"`
	Apps struct {
		HTTP struct {
			Servers map[string]struct {
				Listen []string `json:"listen"`
				Routes []struct {
					Match []struct {
						Host []string `json:"host"`
						Path []string `json:"path"`
					} `json:"match"`
					Handle []map[string]any `json:"handle"`
				} `json:"routes"`
			} `json:"servers"`
		} `json:"http"`
		TLS *struct{} `json:"tls"`
	} `json:"apps"`
}

func decode(t *testing.T, config Config) decoded {
	t.Helper()
	raw, err := Build(config)
	if err != nil {
		t.Fatal(err)
	}
	var out decoded
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRoutesWithoutCertificatesServePlainHTTPOnly(t *testing.T) {
	out := decode(t, Config{Routes: []Route{{Hostname: "a.example.com", Kind: KindProxy, Upstream: "a:80"}}})

	if _, ok := out.Apps.HTTP.Servers["https"]; ok {
		t.Error("an edge without certificates listens on 443")
	}
	if out.Apps.TLS != nil {
		t.Error("an edge without certificates has a tls app")
	}

	routes := out.Apps.HTTP.Servers["http"].Routes
	if len(routes) != 3 {
		t.Fatalf("got %d routes on port 80, want the challenge, the host and the fallback", len(routes))
	}
	if routes[0].Match[0].Path[0] != "/.well-known/acme-challenge/*" {
		t.Error("the ACME challenge route is not first")
	}
	if routes[1].Handle[0]["handler"] != "reverse_proxy" {
		t.Errorf("the host is not proxied on port 80: %v", routes[1].Handle)
	}
	if out.Admin.Listen != "unix/"+AdminSocket {
		t.Errorf("the admin API listens on %q", out.Admin.Listen)
	}
}

func TestHTTPSRedirectComesAfterTheChallenge(t *testing.T) {
	out := decode(t, Config{
		Certificates: []Certificate{{ID: "c1"}},
		Routes:       []Route{{Hostname: "a.example.com", Kind: KindProxy, Upstream: "a:80", Certificate: "c1", HTTPSRedirect: true}},
	})

	plain := out.Apps.HTTP.Servers["http"].Routes
	if len(plain[0].Match[0].Path) == 0 {
		t.Fatal("the first route on port 80 is not the ACME challenge")
	}
	if plain[1].Handle[0]["status_code"] != float64(308) {
		t.Errorf("port 80 does not redirect to HTTPS: %v", plain[1].Handle)
	}

	secure := out.Apps.HTTP.Servers["https"]
	if secure.Listen[0] != ":443" || secure.Routes[0].Handle[0]["handler"] != "reverse_proxy" {
		t.Errorf("port 443 does not serve the route: %+v", secure)
	}
}

func TestExactNamesComeBeforeWildcards(t *testing.T) {
	out := decode(t, Config{Routes: []Route{
		{Hostname: "*.example.com", Kind: KindProxy, Upstream: "wild:80"},
		{Hostname: "zzz.example.com", Kind: KindProxy, Upstream: "exact:80"},
	}})

	routes := out.Apps.HTTP.Servers["http"].Routes
	if routes[1].Match[0].Host[0] != "zzz.example.com" || routes[2].Match[0].Host[0] != "*.example.com" {
		t.Errorf("a wildcard comes before a name it covers: %v, %v", routes[1].Match, routes[2].Match)
	}
}

func TestRedirectKeepsThePath(t *testing.T) {
	cases := map[string]string{
		"https://www.example.com":       "https://www.example.com{http.request.uri}",
		"https://www.example.com/":      "https://www.example.com{http.request.uri}",
		"https://www.example.com/start": "https://www.example.com/start",
		"https://www.example.com/?a=b":  "https://www.example.com/?a=b",
	}
	for target, want := range cases {
		if got := redirectLocation(target); got != want {
			t.Errorf("redirectLocation(%q) = %q, want %q", target, got, want)
		}
	}
}

func TestValidateRefusesMistakes(t *testing.T) {
	cases := map[string]Config{
		"a duplicate name":         {Routes: []Route{{Hostname: "a.example.com", Kind: KindProxy, Upstream: "a:80"}, {Hostname: "A.example.com", Kind: KindProxy, Upstream: "b:80"}}},
		"an IP address":            {Routes: []Route{{Hostname: "192.0.2.1", Kind: KindProxy, Upstream: "a:80"}}},
		"a single label":           {Routes: []Route{{Hostname: "localhost", Kind: KindProxy, Upstream: "a:80"}}},
		"a bad character":          {Routes: []Route{{Hostname: "a_b.example.com", Kind: KindProxy, Upstream: "a:80"}}},
		"a deep wildcard":          {Routes: []Route{{Hostname: "*.*.example.com", Kind: KindProxy, Upstream: "a:80"}}},
		"an upstream without port": {Routes: []Route{{Hostname: "a.example.com", Kind: KindProxy, Upstream: "a"}}},
		"an upstream port of 0":    {Routes: []Route{{Hostname: "a.example.com", Kind: KindProxy, Upstream: "a:0"}}},
		"an upstream with a URL":   {Routes: []Route{{Hostname: "a.example.com", Kind: KindProxy, Upstream: "http://a:80"}}},
		"a redirect to a path":     {Routes: []Route{{Hostname: "a.example.com", Kind: KindRedirect, RedirectTo: "/elsewhere"}}},
		"a redirect to ftp":        {Routes: []Route{{Hostname: "a.example.com", Kind: KindRedirect, RedirectTo: "ftp://example.com"}}},
		// Caddy would send the file's content in the Location header
		"a redirect with a placeholder":      {Routes: []Route{{Hostname: "a.example.com", Kind: KindRedirect, RedirectTo: "https://x.example.com/{file./pocketcloud/certs/c.key}"}}},
		"a redirect with an env placeholder": {Routes: []Route{{Hostname: "a.example.com", Kind: KindRedirect, RedirectTo: "https://x.example.com/?{env.HOME}"}}},
		"an unknown kind":                    {Routes: []Route{{Hostname: "a.example.com", Kind: "static"}}},
		"a missing certificate":              {Routes: []Route{{Hostname: "a.example.com", Kind: KindProxy, Upstream: "a:80", Certificate: "nope"}}},
		"HTTPS without a certificate":        {Routes: []Route{{Hostname: "a.example.com", Kind: KindProxy, Upstream: "a:80", HTTPSRedirect: true}}},
		"a certificate id with a slash":      {Certificates: []Certificate{{ID: "../x"}}},
	}
	for name, config := range cases {
		if _, err := Build(config); err == nil {
			t.Errorf("%s is accepted", name)
		}
	}
}

func TestValidHostname(t *testing.T) {
	for _, good := range []string{"example.com", "a-b.example.com", "*.example.com", "xn--bcher-kva.example"} {
		if err := ValidHostname(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
	for _, bad := range []string{"", "Example.com", "-a.example.com", "a..example.com", "example.com.", strings.Repeat("a", 64) + ".com", "*", "*.com.*"} {
		if err := ValidHostname(bad); err == nil {
			t.Errorf("%q is accepted", bad)
		}
	}
}

func TestCovers(t *testing.T) {
	cases := []struct {
		names    []string
		hostname string
		want     bool
	}{
		{[]string{"app.example.com"}, "app.example.com", true},
		{[]string{"App.Example.com"}, "app.example.com", true},
		{[]string{"*.example.com"}, "app.example.com", true},
		{[]string{"*.example.com"}, "example.com", false},
		{[]string{"*.example.com"}, "a.b.example.com", false},
		{[]string{"*.example.com"}, "*.example.com", true},
		{[]string{"example.com"}, "*.example.com", false},
		{[]string{"other.example.com"}, "app.example.com", false},
	}
	for _, c := range cases {
		if got := Covers(c.names, c.hostname); got != c.want {
			t.Errorf("Covers(%v, %q) = %v", c.names, c.hostname, got)
		}
	}
}

// TestHTTPOnlyNamesAreNotServedByAWildcardOnHTTPS: a wildcard certificate
// completes the handshake for every name under it; a name served over HTTP
// only must not get the wildcard route's site there.
func TestHTTPOnlyNamesAreNotServedByAWildcardOnHTTPS(t *testing.T) {
	out := decode(t, Config{
		Certificates: []Certificate{{ID: "wild"}},
		Routes: []Route{
			{Hostname: "*.example.com", Kind: KindProxy, Upstream: "a:80", Certificate: "wild"},
			{Hostname: "legacy.example.com", Kind: KindProxy, Upstream: "b:80"},
		},
	})
	secure := out.Apps.HTTP.Servers["https"].Routes
	if len(secure) < 2 || secure[0].Match[0].Host[0] != "legacy.example.com" || secure[0].Handle[0]["status_code"] != float64(308) {
		t.Fatalf("the HTTP-only name on 443: %+v", secure)
	}
}

func TestUpstreamsAreContainers(t *testing.T) {
	for _, upstream := range []string{"localhost:80", "LOCALHOST:443", "127.0.0.1:80", "169.254.169.254:80", "10.0.0.1:80", "pocketcloud-edge:443", "-a:80", "..:80", "1.2:80"} {
		if err := validUpstream(upstream); err == nil {
			t.Errorf("%s is accepted", upstream)
		}
	}
	for _, upstream := range []string{"web:80", "stack-web-1:8080", "app.internal:3000", "a_b:1"} {
		if err := validUpstream(upstream); err != nil {
			t.Errorf("%s: %v", upstream, err)
		}
	}
}

func TestRedirectTargets(t *testing.T) {
	for _, bad := range []string{"https://:80", "https://x.example.com/a b", "https://x.example.com/\"", `https://x.example.com/a\`, "https://x.example.com/'"} {
		if err := ValidRedirect(bad); err == nil {
			t.Errorf("%q is accepted", bad)
		}
	}
	for target, want := range map[string]string{
		"https://x.example.com#frag": "https://x.example.com#frag",
		"https://x.example.com?":     "https://x.example.com?",
	} {
		if got := redirectLocation(target); got != want {
			t.Errorf("redirectLocation(%q) = %q, want %q", target, got, want)
		}
	}
	for target, loops := range map[string]bool{
		"https://loop.example.com/":      true,
		"https://LOOP.example.com/start": true,
		"http://loop.example.com":        true,
		"https://loop.example.com:8443/": false,
		"https://www.loop.example.com/":  false,
	} {
		if got := RedirectsToItself("loop.example.com", target); got != loops {
			t.Errorf("RedirectsToItself(%q) = %v", target, got)
		}
	}
}

// TestAPasswordIsNeverAskedForOverPlainHTTP: a route with basic
// authentication answers plain HTTP with a redirect once it has a
// certificate, and refuses before; over HTTPS the password is checked before
// the upstream sees the request.
func TestAPasswordIsNeverAskedForOverPlainHTTP(t *testing.T) {
	users := []BasicAuthUser{{Username: "bob", Hash: "$2a$05$6PVNJize1kOg8S8pGtSUw.DfmvfGyAZxXEGMlQNylBUi5z9iaiBRW"}}
	raw, err := Build(Config{
		Certificates: []Certificate{{ID: "c1"}},
		Routes: []Route{
			{Hostname: "with.example.com", Kind: KindProxy, Upstream: "a:80", Certificate: "c1", BasicAuth: users},
			{Hostname: "without.example.com", Kind: KindProxy, Upstream: "b:80", BasicAuth: users},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	raw = compact.Bytes()
	text := string(raw)
	var config struct {
		Apps struct {
			HTTP struct {
				Servers map[string]json.RawMessage `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	var plainCompact bytes.Buffer
	_ = json.Compact(&plainCompact, config.Apps.HTTP.Servers["http"])
	plain := plainCompact.String()
	if !strings.Contains(text, `"authentication"`) || !strings.Contains(text, `"realm":"PocketCloud"`) {
		t.Errorf("no authentication handler: %s", text)
	}
	if !strings.Contains(plain, "This site asks for a password, so it is served over HTTPS only") || !strings.Contains(plain, "https://{http.request.host}") {
		t.Error("a route without a certificate is not refused over plain HTTP")
	}
	if strings.Contains(plain, `"dial":"b:80"`) || strings.Contains(plain, `"dial":"a:80"`) {
		t.Errorf("a route with a password is proxied over plain HTTP: %s", plain)
	}
}

func TestBasicAuthIsChecked(t *testing.T) {
	good := "$2a$05$6PVNJize1kOg8S8pGtSUw.DfmvfGyAZxXEGMlQNylBUi5z9iaiBRW"
	for name, users := range map[string][]BasicAuthUser{
		"not bcrypt":   {{Username: "bob", Hash: "{SHA}W6ph5Mm5Pz8GgiULbPgzG37mj9g="}},
		"a colon":      {{Username: "bo:b", Hash: good}},
		"no name":      {{Username: "", Hash: good}},
		"twice":        {{Username: "bob", Hash: good}, {Username: "bob", Hash: good}},
		"a short hash": {{Username: "bob", Hash: good[:59]}},
		"a control":    {{Username: "bo\x01b", Hash: good}},
	} {
		if err := ValidBasicAuth(users); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	redirect := Config{Routes: []Route{{Hostname: "r.example.com", Kind: KindRedirect, RedirectTo: "https://x.example.com", BasicAuth: []BasicAuthUser{{Username: "bob", Hash: good}}}}}
	if err := redirect.Validate(); err == nil {
		t.Error("a redirect with a password was accepted")
	}
}
