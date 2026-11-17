package panel

import (
	"strings"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/ca"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// otherKey is a master key that is not testutil.MasterKey.
var otherKey = []byte("another-key-another-key-another-")

// caOfAnEarlierStart opens the internal CA as an earlier start of the panel
// did, for a test that adds servers before the panel it starts: a database
// with servers always has its CA, and a start does not make one over them.
func caOfAnEarlierStart(t testing.TB, app core.App) *ca.Authority {
	t.Helper()
	box, err := secrets.New(testutil.MasterKey)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := ca.Open(app, box, nil)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

// serve runs the app's OnServe hooks, as `panel serve` does, and returns
// what the start returned.
func serve(t testing.TB, app core.App) error {
	t.Helper()
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	return app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(*core.ServeEvent) error { return nil })
}

// stopPanel stops what a start of the app started, as PocketBase does when
// the panel ends; the app stays open.
func stopPanel(t testing.TB, app core.App) {
	t.Helper()
	if err := app.OnTerminate().Trigger(&core.TerminateEvent{App: app}, func(*core.TerminateEvent) error { return nil }); err != nil {
		t.Error(err)
	}
}

func panelStateOf(t testing.TB, app core.App) panelState {
	t.Helper()
	state, err := readPanelState(app.DB())
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// TestTheKeyAndTheCAAreRecordedAtTheFirstStart (#32): a start records the
// id of the master key and the pin of the CA it opened, which later starts
// and `panel recover` compare; and it gives the agents a grace before any
// is marked offline.
func TestTheKeyAndTheCAAreRecordedAtTheFirstStart(t *testing.T) {
	p, _, _ := newTestPanel(t)

	state := panelStateOf(t, p.App)
	if state.KeyID != secrets.KeyID(testutil.MasterKey) || state.CAPin != p.Authority.Pin() {
		t.Fatalf("recorded %+v, want key %s and pin %s", state, secrets.KeyID(testutil.MasterKey), p.Authority.Pin())
	}
	if p.Gateway.StartGrace != offlineGrace || offlineGrace.Seconds() != 90 {
		t.Fatalf("the gateway's start grace is %s", p.Gateway.StartGrace)
	}
}

// TestAPanelFromBeforeTheGuardsIsUpgraded: a database with servers and a CA
// but nothing recorded yet starts, and gets its key id and pin.
func TestAPanelFromBeforeTheGuardsIsUpgraded(t *testing.T) {
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)
	authority := caOfAnEarlierStart(t, app)
	testutil.NewHost(t, app, "node-1")

	New(app, &Config{MasterKey: testutil.MasterKey}, testUI).Register()
	if err := serve(t, app); err != nil {
		t.Fatal(err)
	}
	if state := panelStateOf(t, app); state.KeyID != secrets.KeyID(testutil.MasterKey) || state.CAPin != authority.Pin() {
		t.Fatalf("recorded %+v", state)
	}
}

// TestAKeyOfAnotherDatabaseIsRefusedBeforeTheCA (#32): a well-formed key
// other than the one that sealed the database stops the start before the
// CA is read and before anything is written, the audit's seal included.
func TestAKeyOfAnotherDatabaseIsRefusedBeforeTheCA(t *testing.T) {
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)
	// the database as its panel left it: its CA, its key's id and its pin
	authority := caOfAnEarlierStart(t, app)
	if err := setPanelState(app.DB(), dbx.Params{"key_id": secrets.KeyID(testutil.MasterKey), "ca_pin": authority.Pin()}); err != nil {
		t.Fatal(err)
	}
	testutil.NewHost(t, app, "node-1")
	before := panelStateOf(t, app)
	cas, err := app.FindAllRecords(schema.InternalCA)
	if err != nil || len(cas) != 1 {
		t.Fatalf("%d CAs, %v", len(cas), err)
	}
	entries, _ := app.CountRecords(schema.AuditLog)

	wrong := New(app, &Config{MasterKey: otherKey}, testUI)
	wrong.Register()
	err = serve(t, app)
	want := "This master key (id " + secrets.KeyID(otherKey) + ") did not seal this database (id " + secrets.KeyID(testutil.MasterKey) +
		"): start the panel with the key file of its recovery kit."
	if err == nil || err.Error() != want {
		t.Fatalf("another key: %v", err)
	}
	if wrong.Authority != nil || wrong.Gateway != nil {
		t.Fatal("the CA was opened with another key")
	}
	if after := panelStateOf(t, app); after != before {
		t.Fatalf("the refused start wrote %+v over %+v", after, before)
	}
	now, _ := app.FindAllRecords(schema.InternalCA)
	if len(now) != 1 || now[0].GetString("updated") != cas[0].GetString("updated") || now[0].GetString("server_cert") != cas[0].GetString("server_cert") {
		t.Fatal("the refused start changed the CA")
	}
	if after, _ := app.CountRecords(schema.AuditLog); after != entries {
		t.Fatalf("the refused start wrote %d audit entries", after-entries)
	}
}

// TestASecondServeIsRefused (#32): two panels on one pb_data would both
// command the servers; the second is refused until the first ends.
func TestASecondServeIsRefused(t *testing.T) {
	first, app, _ := newTestPanel(t)

	second := core.NewBaseApp(core.BaseAppConfig{DataDir: app.DataDir(), EncryptionEnv: "pb_test_env"})
	if err := second.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopPanel(t, second)
		_ = second.ClearBootstrap()
	})
	New(second, &Config{MasterKey: testutil.MasterKey}, testUI).Register()

	err := serve(t, second)
	want := "Another panel serves " + app.DataDir() + ": two panels on one pb_data would both command the servers."
	if err == nil || err.Error() != want {
		t.Fatalf("a second panel on the same pb_data: %v", err)
	}

	// once the first let go, as it does when it ends, the next one serves
	first.releaseServeLock()
	if err := serve(t, second); err != nil {
		t.Fatalf("after the first ended: %v", err)
	}
}

// TestANewCAIsNotMadeOverServers (#32): a database that had servers, revoked
// certificates or a CA but holds no CA now is damaged or another's: the
// start fails instead of making a CA every server would have to enroll with.
func TestANewCAIsNotMadeOverServers(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(t *testing.T, app core.App)
		want    string
	}{
		{"a server", func(t *testing.T, app core.App) { testutil.NewHost(t, app, "node-1") }, "had servers or a CA but"},
		{"a revoked certificate", func(t *testing.T, app core.App) {
			collection, err := app.FindCollectionByNameOrId(schema.RevokedCerts)
			if err != nil {
				t.Fatal(err)
			}
			revoked := core.NewRecord(collection)
			revoked.Set("serial", "0a1b")
			if err := app.Save(revoked); err != nil {
				t.Fatal(err)
			}
		}, "had servers or a CA but"},
		{"a recorded CA", func(t *testing.T, app core.App) {
			if err := setPanelState(app.DB(), dbx.Params{"ca_pin": "sha256:5d1e"}); err != nil {
				t.Fatal(err)
			}
		}, "had servers or a CA (pin sha256:5d1e) but"},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := testutil.NewApp(t)
			t.Cleanup(app.Cleanup)
			test.prepare(t, app)
			New(app, &Config{MasterKey: testutil.MasterKey}, testUI).Register()

			err := serve(t, app)
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.HasPrefix(err.Error(), "This database had servers") {
				t.Fatalf("the start made a CA, or said %v", err)
			}
			if n, _ := app.CountRecords(schema.InternalCA); n != 0 {
				t.Fatal("a CA was made over the servers")
			}
		})
	}
}

// TestTheExpectedPinIsTheStartsCA (#32): with POCKETCLOUD_EXPECT_CA_PIN, an
// empty pb_data does not start with a CA of its own, and another panel's
// database does not start at all.
func TestTheExpectedPinIsTheStartsCA(t *testing.T) {
	pin := "sha256:" + strings.Repeat("5d", 32)
	t.Setenv(EnvExpectCAPin, pin)
	config := ConfigFromEnv()
	if config.ExpectCAPin != pin {
		t.Fatalf("the environment's pin is %q", config.ExpectCAPin)
	}
	config.MasterKey, config.GRPCAddr = testutil.MasterKey, ""

	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)
	New(app, &config, testUI).Register()
	err := serve(t, app)
	if err == nil || err.Error() != "This database has no internal CA, but POCKETCLOUD_EXPECT_CA_PIN expects "+pin+": restore the panel's backup into it first." {
		t.Fatalf("an empty pb_data with an expected pin: %v", err)
	}
	if n, _ := app.CountRecords(schema.InternalCA); n != 0 {
		t.Fatal("a CA was made although another was expected")
	}

	other := testutil.NewApp(t)
	t.Cleanup(other.Cleanup)
	authority := caOfAnEarlierStart(t, other)
	New(other, &config, testUI).Register()
	err = serve(t, other)
	if err == nil || err.Error() != "POCKETCLOUD_EXPECT_CA_PIN is "+pin+", but this database's CA is "+authority.Pin()+": this is not the panel your servers trust." {
		t.Fatalf("another CA: %v", err)
	}
}
