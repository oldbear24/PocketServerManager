// Package testutil holds helpers for tests that need a real PocketBase app.
package testutil

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	_ "github.com/oldbear24/PocketServerManager/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// MasterKey is the fixed master key of test panels.
var MasterKey = []byte("0123456789abcdef0123456789abcdef")

// NewApp returns a test app with a fresh data dir and every migration applied.
func NewApp(t testing.TB) *tests.TestApp {
	t.Helper()

	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "pb_test_env",
	})
	if err != nil {
		t.Fatal(err)
	}

	return app
}

// NewUser creates a user with a role and returns it with an auth token.
func NewUser(t testing.TB, app core.App, email, role string) (*core.Record, string) {
	t.Helper()

	collection, err := app.FindCollectionByNameOrId(schema.Users)
	if err != nil {
		t.Fatal(err)
	}

	user := core.NewRecord(collection)
	user.SetEmail(email)
	user.SetPassword("correct horse battery staple")
	user.Set("role", role)
	if err := app.Save(user); err != nil {
		t.Fatal(err)
	}

	token, err := user.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}

	return user, token
}

// NewSuperuser creates a superuser and returns it with an auth token.
func NewSuperuser(t testing.TB, app core.App) (*core.Record, string) {
	t.Helper()

	collection, err := app.FindCollectionByNameOrId(schema.Superusers)
	if err != nil {
		t.Fatal(err)
	}

	superuser := core.NewRecord(collection)
	superuser.SetEmail("root@example.com")
	superuser.SetPassword("correct horse battery staple")
	if err := app.Save(superuser); err != nil {
		t.Fatal(err)
	}

	token, err := superuser.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}

	return superuser, token
}

// NewHost creates a minimal host record.
func NewHost(t testing.TB, app core.App, name string) *core.Record {
	t.Helper()

	collection, err := app.FindCollectionByNameOrId(schema.Hosts)
	if err != nil {
		t.Fatal(err)
	}

	host := core.NewRecord(collection)
	host.Set("name", name)
	host.Set("roles", []string{schema.HostRoleDocker})
	host.Set("status", schema.HostOffline)
	if err := app.Save(host); err != nil {
		t.Fatal(err)
	}

	return host
}

// Handler returns the app's HTTP routes, with every OnServe hook applied.
func Handler(t testing.TB, app core.App) http.Handler {
	t.Helper()

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}

	event := new(core.ServeEvent)
	event.App = app
	event.Router = router

	var handler http.Handler
	err = app.OnServe().Trigger(event, func(e *core.ServeEvent) error {
		mux, err := e.Router.BuildMux()
		if err != nil {
			return err
		}
		handler = mux
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return handler
}

// Request describes one call to Handler's routes.
type Request struct {
	Method  string
	URL     string
	Token   string
	Body    string
	Headers map[string]string
}

// Do sends a request to the routes and returns the status and the body.
func Do(t testing.TB, handler http.Handler, r Request) (int, string) {
	t.Helper()

	req := httptest.NewRequest(r.Method, r.URL, strings.NewReader(r.Body))
	if r.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.Token != "" {
		req.Header.Set("Authorization", r.Token)
	}
	for key, value := range r.Headers {
		req.Header.Set(key, value)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	return rec.Code, rec.Body.String()
}
