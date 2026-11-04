package panel

import (
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

var testUI = fstest.MapFS{
	"index.html":    {Data: []byte("<title>PocketCloud UI</title>")},
	"assets/app.js": {Data: []byte("console.log('app')")},
}

func newTestPanel(t testing.TB) (*Panel, *tests.TestApp, http.Handler) {
	t.Helper()

	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)

	p := New(app, &Config{MasterKey: testutil.MasterKey, GRPCAddr: "127.0.0.1:0"}, testUI)
	p.Register()

	return p, app, testutil.Handler(t, app)
}

func TestFreshDataDirHasAllCollections(t *testing.T) {
	_, app, _ := newTestPanel(t)

	for _, name := range []string{
		schema.Users, schema.Hosts, schema.RevokedCerts, schema.EnrollmentTokens, schema.Commands,
		schema.Events, schema.Alerts, schema.Stacks, schema.StackRevisions, schema.Containers,
		schema.Volumes, schema.Networks, schema.InternalCA, schema.Settings, schema.AuditLog,
		schema.DiagnosticBundles, schema.DBServers, schema.Databases, schema.DBUsers, schema.DBNetworks,
		schema.DBDumps, schema.DBMoves,
	} {
		if _, err := app.FindCollectionByNameOrId(name); err != nil {
			t.Errorf("collection %s is missing: %v", name, err)
		}
	}

	settings, err := app.FindRecordsByFilter(schema.Settings, "", "", 0, 0)
	if err != nil || len(settings) != 1 {
		t.Fatalf("expected one settings record, got %d (%v)", len(settings), err)
	}
	if got := settings[0].GetInt("audit_retention_days"); got != 365 {
		t.Errorf("default audit retention is %d", got)
	}
}

func TestEncryptedFieldsAreHiddenText(t *testing.T) {
	_, app, _ := newTestPanel(t)

	for name, fields := range schema.Encrypted {
		collection, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			continue // lands with a later milestone
		}

		for _, field := range fields {
			definition := collection.Fields.GetByName(field)
			if definition == nil {
				t.Errorf("%s.%s does not exist", name, field)
				continue
			}
			if _, ok := definition.(*core.TextField); !ok {
				t.Errorf("%s.%s must be a text field to hold a sealed value", name, field)
			}
			if !definition.GetHidden() {
				t.Errorf("%s.%s must be hidden from the API", name, field)
			}
		}
	}
}

func TestUIHosting(t *testing.T) {
	_, _, handler := newTestPanel(t)

	for _, tc := range []struct {
		url      string
		status   int
		contains string
	}{
		{"/", 200, "PocketCloud UI"},
		{"/servers/abc", 200, "PocketCloud UI"},
		{"/assets/app.js", 200, "console.log"},
		{"/api/health", 200, "API is healthy"},
		{"/api/unknown", 404, "wasn't found"},
	} {
		status, body := testutil.Do(t, handler, testutil.Request{Method: "GET", URL: tc.url})
		if status != tc.status || !strings.Contains(body, tc.contains) {
			t.Errorf("GET %s: status %d, body %.80q; want %d with %q", tc.url, status, body, tc.status, tc.contains)
		}
	}
}

func TestUINotBuilt(t *testing.T) {
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)

	New(app, &Config{MasterKey: testutil.MasterKey}, fstest.MapFS{}).Register()

	status, body := testutil.Do(t, testutil.Handler(t, app), testutil.Request{Method: "GET", URL: "/"})
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "task web") {
		t.Fatalf("status %d, body %q", status, body)
	}
}

// TestStartingOpensNoBrowser guards against PocketBase's default installer, which
// launches the default browser when no superuser exists yet.
func TestStartingOpensNoBrowser(t *testing.T) {
	// a plain app: the test app of PocketBase turns the installer off by itself
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: t.TempDir()})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.OnTerminate().Trigger(&core.TerminateEvent{App: app}); err != nil {
			t.Error(err)
		}
		if err := app.ClearBootstrap(); err != nil {
			t.Error(err)
		}
	})
	if err := app.RunAllMigrations(); err != nil {
		t.Fatal(err)
	}

	New(app, &Config{MasterKey: testutil.MasterKey}, testUI).Register()

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}

	event := &core.ServeEvent{
		App:    app,
		Router: router,
		InstallerFunc: func(core.App, *core.Record, string) error {
			t.Error("the installer must not run")
			return nil
		},
	}

	err = app.OnServe().Trigger(event, func(e *core.ServeEvent) error {
		if e.InstallerFunc != nil {
			t.Error("the panel must turn off PocketBase's installer, which opens a browser")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPanelRefusesToStartWithoutMasterKey(t *testing.T) {
	p := &Panel{Config: &Config{MasterKeyFile: t.TempDir() + "/missing.key"}}

	err := p.loadMasterKey()
	if err == nil || !strings.Contains(err.Error(), "panel keygen") {
		t.Fatalf("expected a helpful error, got %v", err)
	}
}

func TestPanelRefusesWrongSizedMasterKey(t *testing.T) {
	p := &Panel{Config: &Config{MasterKey: []byte("short")}}

	if err := p.loadMasterKey(); err == nil {
		t.Fatal("expected an error")
	}
}

func TestSecretsAreEncryptedAtRest(t *testing.T) {
	p, app, _ := newTestPanel(t)

	host := testutil.NewHost(t, app, "enc-host")

	collection, err := app.FindCollectionByNameOrId(schema.Stacks)
	if err != nil {
		t.Fatal(err)
	}

	stack := core.NewRecord(collection)
	stack.Set("host", host.Id)
	stack.Set("name", "demo")
	stack.Set("secrets", `{"DB_PASSWORD":"hunter2-super-secret"}`)
	if err := app.Save(stack); err != nil {
		t.Fatal(err)
	}

	raw := struct {
		Secrets string `db:"secrets"`
	}{}
	if err := app.DB().NewQuery("SELECT secrets FROM stacks WHERE id={:id}").Bind(map[string]any{"id": stack.Id}).One(&raw); err != nil {
		t.Fatal(err)
	}

	if !secrets.IsSealed(raw.Secrets) {
		t.Fatalf("the stored value is not sealed: %q", raw.Secrets)
	}
	if strings.Contains(raw.Secrets, "hunter2") {
		t.Fatal("the plaintext is in the database")
	}

	opened, err := p.Box.OpenString(raw.Secrets)
	if err != nil || !strings.Contains(opened, "hunter2-super-secret") {
		t.Fatalf("cannot open the value: %v", err)
	}

	// saving again must not seal it twice
	stack.Set("status", "running")
	if err := app.Save(stack); err != nil {
		t.Fatal(err)
	}
	fresh, _ := app.FindRecordById(schema.Stacks, stack.Id)
	if opened, err := p.Box.OpenString(fresh.GetString("secrets")); err != nil || !strings.Contains(opened, "hunter2") {
		t.Fatalf("value after a second save: %q, %v", opened, err)
	}
}

func TestAgentEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config Config
		host   string
		want   string
	}{
		{"configured", Config{GRPCPublicAddr: "agents.example.com:443", PublicURL: "https://panel.example.com", GRPCAddr: ":9443"}, "x", "agents.example.com:443"},
		{"from the public URL", Config{PublicURL: "https://panel.example.com", GRPCAddr: ":9443"}, "other:8090", "panel.example.com:9443"},
		{"from the request", Config{GRPCAddr: ":9443"}, "192.168.1.10:8090", "192.168.1.10:9443"},
		{"request without a port", Config{GRPCAddr: ":7000"}, "panel.lan", "panel.lan:7000"},
		{"a random port is never advertised", Config{GRPCAddr: "127.0.0.1:0"}, "panel.lan", "panel.lan:9443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.config.AgentEndpointFor(tc.host); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}
