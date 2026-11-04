package panel

import (
	"testing"
	"time"

	"github.com/pocketbase/dbx"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// TestAStartingPanelRefreshesItsEdges: the digest stored with a host names the
// config of the release that sent it. A panel that starts sends a host whose
// config it would generate differently a new one, without anyone asking.
func TestAStartingPanelRefreshesItsEdges(t *testing.T) {
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)

	// as an older release left it
	host := testutil.NewHost(t, app, "edge-1")
	host.Set("roles", []string{schema.HostRoleDocker, schema.HostRoleEdge})
	host.Set("capabilities", []string{"edge.apply"})
	host.Set("edge_digest", "from-the-old-release")
	host.Set("edge_applied_digest", "from-the-old-release")
	if err := app.Save(host); err != nil {
		t.Fatal(err)
	}

	p := New(app, &Config{MasterKey: testutil.MasterKey, GRPCAddr: "127.0.0.1:0"}, testUI)
	p.Register()
	testutil.Handler(t, app)

	deadline := time.Now().Add(5 * time.Second)
	for {
		count, err := app.CountRecords(schema.Commands, dbx.HashExp{"host": host.Id, "type": "edge.apply"})
		if err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d edge.apply commands after the start, want 1", count)
		}
		time.Sleep(50 * time.Millisecond)
	}

	refreshed, err := app.FindRecordById(schema.Hosts, host.Id)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.GetString("edge_digest") == "from-the-old-release" {
		t.Error("the wanted digest was not recomputed")
	}
}
