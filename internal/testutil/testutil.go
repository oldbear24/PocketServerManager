// Package testutil holds helpers for tests that need a real PocketBase app.
package testutil

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	_ "github.com/oldbear24/PocketServerManager/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// MasterKey is the fixed master key of test panels.
var MasterKey = []byte("0123456789abcdef0123456789abcdef")

// NewApp returns a test app with a fresh data dir and every migration applied.
// The data dir is a copy of one migrated once per test binary: PocketBase
// clones the dir it is given and applies only the migrations it lacks, and
// applying all of them for every test is slow under the race detector, whose
// cost grows with each migration (#31's made every e2e test ~6 s slower).
func NewApp(t testing.TB) *tests.TestApp {
	t.Helper()

	dir, err := migratedDataDir()
	if err != nil {
		t.Fatal(err)
	}
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       dir,
		EncryptionEnv: "pb_test_env",
	})
	if err != nil {
		t.Fatal(err)
	}

	return app
}

// NewAppFrom returns a test app whose data dir is a copy of dir, as a panel
// restored into dir starts on it (#32): every file of dir is copied, its
// hidden ones too, and the migrations the copy lacks are applied. dir itself
// is left as it is.
func NewAppFrom(t testing.TB, dir string) *tests.TestApp {
	t.Helper()

	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       dir,
		EncryptionEnv: "pb_test_env",
	})
	if err != nil {
		t.Fatal(err)
	}

	return app
}

var migrated struct {
	once sync.Once
	dir  string
	err  error
}

// migratedPrefix starts the name of a migrated data dir in the system's
// temporary dir.
const migratedPrefix = "pocketcloud-migrated-"

// migratedKept is how long a migrated data dir is left for the test binary
// that made it: nothing ends a test binary that could remove its own, so the
// next one to make one removes those older than this.
const migratedKept = time.Hour

// migratedDataDir is a data dir with every migration applied, made on first
// use, in the system's temporary dir.
func migratedDataDir() (string, error) {
	migrated.once.Do(func() {
		removeOldMigrated(os.TempDir(), time.Now().Add(-migratedKept))
		dir, err := os.MkdirTemp("", migratedPrefix)
		if err != nil {
			migrated.err = err
			return
		}
		app := core.NewBaseApp(core.BaseAppConfig{DataDir: dir, EncryptionEnv: "pb_test_env"})
		if err := app.Bootstrap(); err != nil {
			migrated.err = err
			return
		}
		if err := app.RunAllMigrations(); err != nil {
			migrated.err = err
			return
		}
		// closes the databases, so that what is copied is whole
		if err := app.ClearBootstrap(); err != nil {
			migrated.err = err
			return
		}
		migrated.dir = dir
	})
	return migrated.dir, migrated.err
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

// removeOldMigrated removes the migrated data dirs in tmp last changed before
// before: those of test binaries that ended long ago.
func removeOldMigrated(tmp string, before time.Time) {
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), migratedPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(before) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(tmp, entry.Name()))
	}
}
