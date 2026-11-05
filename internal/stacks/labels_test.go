package stacks

import (
	"context"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v4"
)

func exposures(t *testing.T, compose string, env map[string]string, checkHashes bool) ([]Exposure, []Problem) {
	t.Helper()
	project, err := LoadResolved(context.Background(), "shop", compose, "", env)
	if err != nil {
		t.Fatal(err)
	}
	var tree yaml.Node
	if err := yaml.Unmarshal([]byte(compose), &tree); err != nil {
		t.Fatal(err)
	}
	return Exposures(project, &tree, checkHashes)
}

const bob = "bob:$$2a$$05$$6PVNJize1kOg8S8pGtSUw.DfmvfGyAZxXEGMlQNylBUi5z9iaiBRW"

func TestLabelsPutServicesOnDomains(t *testing.T) {
	compose := `services:
  web:
    image: nginx:1
    expose: ["80"]
    labels:
      pocketcloud.domain: Shop.example.com, www.shop.example.com
  api:
    image: api:1
    container_name: shop-api
    ports: ["9000:8080", "9443:8443"]
    labels:
      - pocketcloud.domain=api.example.com
      - pocketcloud.port=8080
      - pocketcloud.tls=off
  admin:
    image: admin:1
    expose: ["3000"]
    labels:
      pocketcloud.domain: admin.example.com
      pocketcloud.auth: "` + bob + `"
  db:
    image: postgres:17
`
	got, problems := exposures(t, compose, nil, true)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if len(got) != 3 {
		t.Fatalf("%d exposures: %+v", len(got), got)
	}
	admin, api, web := got[0], got[1], got[2]
	if web.Container != "shop-web-1" || web.Port != 80 || web.TLS != TLSACME || strings.Join(web.Hostnames, ",") != "shop.example.com,www.shop.example.com" {
		t.Errorf("web: %+v", web)
	}
	if api.Container != "shop-api" || api.Port != 8080 || api.TLS != TLSOff {
		t.Errorf("api: %+v", api)
	}
	if len(admin.BasicAuth) != 1 || admin.BasicAuth[0].Username != "bob" || !strings.HasPrefix(admin.BasicAuth[0].Hash, "$2a$05$") {
		t.Errorf("admin: %+v", admin)
	}
}

func TestWrongLabelsSayWhatIsWrongAndWhere(t *testing.T) {
	compose := `services:
  noport:
    image: a:1
    labels:
      pocketcloud.domain: a.example.com
  twoports:
    image: b:1
    expose: ["80", "443"]
    labels:
      pocketcloud.domain: b.example.com
  badport:
    image: c:1
    labels:
      pocketcloud.domain: c.example.com
      pocketcloud.port: http
  badname:
    image: d:1
    expose: ["80"]
    labels:
      pocketcloud.domain: not_a_name
  internal:
    image: e:1
    expose: ["80"]
    labels:
      pocketcloud.domain: e.example.com
      pocketcloud.tls: internal
  badauth:
    image: f:1
    expose: ["80"]
    labels:
      pocketcloud.domain: f.example.com
      pocketcloud.auth: "bob:plaintext"
  twice:
    image: g:1
    expose: ["80"]
    labels:
      pocketcloud.domain: a.example.com
  lonely:
    image: h:1
    labels:
      pocketcloud.port: "80"
  insecure:
    image: i:1
    expose: ["80"]
    labels:
      pocketcloud.domain: i.example.com
      pocketcloud.tls: "off"
      pocketcloud.auth: "` + bob + `"
`
	got, problems := exposures(t, compose, nil, true)
	if len(got) != 0 {
		t.Errorf("exposed although wrong: %+v", got)
	}
	want := map[string]string{
		"noport:":   "set pocketcloud.port: the service names no port",
		"twoports:": "the service listens on 80 and 443",
		"badport:":  `not "http"`,
		"badname:":  "not_a_name is not a fully qualified hostname",
		"internal:": "internal CA are not available yet",
		"badauth:":  "is not a bcrypt hash",
		"twice:":    "a.example.com is the domain of noport already",
		"lonely:":   "does nothing without pocketcloud.domain",
		"insecure:": "a password needs HTTPS",
	}
	for prefix, text := range want {
		found := false
		for _, problem := range problems {
			if strings.HasPrefix(problem.Message, prefix) && strings.Contains(problem.Message, text) {
				found = true
				if problem.Line == 0 {
					t.Errorf("%s has no line", problem.Message)
				}
			}
		}
		if !found {
			t.Errorf("no problem %q %q in %v", prefix, text, problems)
		}
	}
	// the line is the label's own
	for _, problem := range problems {
		if strings.HasPrefix(problem.Message, "badport:") && problem.Line != 15 {
			t.Errorf("badport at line %d", problem.Line)
		}
	}
}

// TestTheEditorCannotCheckHashesFromSecrets: a hash in a secret is a
// placeholder for the editor, so only the deploy checks it.
func TestTheEditorCannotCheckHashesFromSecrets(t *testing.T) {
	compose := `services:
  admin:
    image: admin:1
    expose: ["3000"]
    labels:
      pocketcloud.domain: admin.example.com
      pocketcloud.auth: "bob:${ADMIN_HASH}"
`
	result := validate(compose, "", "ADMIN_HASH")
	for _, warning := range result.Warnings {
		t.Errorf("a warning about a secret: %v", warning)
	}
	if _, problems := exposures(t, compose, map[string]string{"ADMIN_HASH": "secret"}, true); len(problems) != 1 {
		t.Errorf("the deploy check: %v", problems)
	}
	// the editor shows the label problems it can see
	result = validate(strings.ReplaceAll(compose, `expose: ["3000"]`, ""), "", "ADMIN_HASH")
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0].Message, "pocketcloud.port") {
		t.Errorf("warnings: %v", result.Warnings)
	}
}

func TestScaledServicesAreWarnedAbout(t *testing.T) {
	compose := `services:
  web:
    image: nginx:1
    expose: ["80"]
    deploy:
      replicas: 3
    labels:
      pocketcloud.domain: shop.example.com
`
	got, problems := exposures(t, compose, nil, true)
	if len(got) != 1 || len(problems) != 1 || !strings.Contains(problems[0].Message, "only the first of its 3 containers") {
		t.Errorf("%+v %v", got, problems)
	}
}
