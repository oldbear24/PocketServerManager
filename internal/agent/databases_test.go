package agent

import (
	"context"
	"net"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/databases"
)

// The drops of temporary databases (#53), on the fake engine and the fake
// database servers.

func discardCommand(engine databases.Engine, revokeFrom string, names ...string) *agentv1.Command {
	return &agentv1.Command{Id: "x1", Type: "db.database.discard", Payload: &agentv1.Command_DbDatabaseDiscard{DbDatabaseDiscard: &agentv1.DbDatabaseDiscard{
		Connection: connection(engine), Names: names, RevokeFrom: revokeFrom}}}
}

// TestADiscardDropsTemporaryDatabasesOnly (#53): db.database.discard drops
// databases of the temporary namespace, with the user's privileges on them
// on MariaDB, which outlive a drop; any other name is refused before the
// server is asked, and so is a database a restore or a restore test holds.
func TestADiscardDropsTemporaryDatabasesOnly(t *testing.T) {
	d := newDBHarness(t)
	maria := databases.MariaDB
	server := d.servers[maria.Name]
	d.shop(maria)
	test, scratch := databases.TestDatabaseName(testCheckID), databases.ScratchName(testRestoreID)
	d.db.AddDatabase(server, test, "shop", shopPassword, "items")
	d.db.AddDatabase(server, scratch, "shop", shopPassword)
	result, err := d.handleDatabaseDiscard(context.Background(), discardCommand(maria, "shop", test, scratch), &Reporter{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(anyStrings(result["dropped"]), []string{test, scratch}) || !slices.Equal(d.db.DatabaseNames(server), []string{"blog", "shop"}) {
		t.Errorf("after: %v, %q", result, d.db.DatabaseNames(server))
	}
	// MariaDB kept no privilege of shop on them, and shop keeps its own
	d.db.AddDatabase(server, test, "", "")
	if copied, _ := d.db.Database(server, test); len(copied.Granted) != 0 {
		t.Errorf("the grants on %s outlived it: %v", test, copied.Granted)
	}
	if shop, _ := d.db.Database(server, "shop"); !slices.Equal(shop.Granted, []string{"shop"}) || len(shop.Tables) != 2 {
		t.Errorf("shop: %+v", shop)
	}

	asked := len(d.db.Execs())
	for _, tc := range []struct {
		names      []string
		revokeFrom string
		want       string
	}{
		{[]string{"shop"}, "", "is not one of PocketCloud's temporary databases"},
		{[]string{test, "blog"}, "", "is not one of PocketCloud's temporary databases"},
		{[]string{"pc_rt_short"}, "", "is not one of PocketCloud's temporary databases"},
		{[]string{"PC_RT_CCCCCCCCCCCCCCC"}, "", "is not one of PocketCloud's temporary databases"},
		{[]string{"pc_xx_ccccccccccccccc"}, "", "is not one of PocketCloud's temporary databases"},
		{[]string{test + "`; DROP DATABASE shop; --"}, "", "is not one of PocketCloud's temporary databases"},
		{nil, "", "names no temporary database"},
		{[]string{test}, "Shop;", "not a valid database name"},
	} {
		if _, err := d.handleDatabaseDiscard(context.Background(), discardCommand(maria, tc.revokeFrom, tc.names...), &Reporter{}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v", tc.names, err)
		}
	}
	if len(d.db.Execs()) != asked {
		t.Errorf("the server was asked: %+v", d.db.Execs()[asked:])
	}

	// a test that holds its copy
	release, err := d.claims.claim(true, []string{databaseClaim(server, test)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.handleDatabaseDiscard(context.Background(), discardCommand(maria, "", test), &Reporter{}); err == nil ||
		err.Error() != "A restore or a restore test still uses "+test+": drop it once that ends." {
		t.Errorf("a database a test holds: %v", err)
	}
	release()
	if _, err := d.handleDatabaseDiscard(context.Background(), discardCommand(maria, "", test), &Reporter{}); err != nil {
		t.Errorf("once the test ended: %v", err)
	}
}

// uploadsTo makes the agent's own connection upload artifacts to a panel.
func uploadsTo(t *testing.T, a *Agent) *uploadPanel {
	t.Helper()
	panel := &uploadPanel{ended: make(chan error, 4)}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(server, panel)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///panel", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := agentv1.NewAgentServiceClient(conn)
	a.client.Store(&client)
	return panel
}

func dropCommand(engine databases.Engine, database, user string, temporaries ...string) *agentv1.Command {
	return &agentv1.Command{Id: "x2", Type: "db.database.drop", Payload: &agentv1.Command_DbDatabaseDrop{DbDatabaseDrop: &agentv1.DbDatabaseDrop{
		Connection: connection(engine), Database: database, User: user, Temporaries: temporaries}}}
}

// TestADropDropsTheDatabasesTemporaryCopiesBeforeItsUser (#53): the
// temporary copies of a dropped database, which its user owns, go after its
// final dump and before the user, which PostgreSQL would keep while it owns
// them; a name outside the namespace refuses the drop before the dump.
func TestADropDropsTheDatabasesTemporaryCopiesBeforeItsUser(t *testing.T) {
	d := newDBHarness(t)
	pg := databases.Postgres
	server := d.servers[pg.Name]
	uploadsTo(t, d.Agent)
	d.shop(pg)
	previous := databases.PreviousName(testRestoreID)
	d.db.AddDatabase(server, previous, "shop", shopPassword, "items")

	// without them, the user cannot go
	if _, err := d.handleDatabaseDrop(context.Background(), dropCommand(pg, "shop", "shop"), &Reporter{}); err == nil ||
		!strings.Contains(err.Error(), `role "shop" cannot be dropped because some objects depend on it`) {
		t.Errorf("a drop that leaves the copy: %v", err)
	}
	d.db.AddDatabase(server, "shop", "shop", shopPassword, "items", "orders")

	asked := len(d.db.Execs())
	if _, err := d.handleDatabaseDrop(context.Background(), dropCommand(pg, "shop", "shop", previous, "blog"), &Reporter{}); err == nil ||
		!strings.Contains(err.Error(), "is not one of PocketCloud's temporary databases") || len(d.db.Execs()) != asked {
		t.Errorf("a drop of another database as a copy: %v", err)
	}

	result, err := d.handleDatabaseDrop(context.Background(), dropCommand(pg, "shop", "shop", previous), &Reporter{})
	if err != nil {
		t.Fatalf("%v %v", err, result)
	}
	if names := d.db.DatabaseNames(server); !slices.Equal(names, []string{"blog"}) {
		t.Errorf("the server has %q", names)
	}
	if _, there := d.db.Password(server, "shop"); there {
		t.Error("the user is left")
	}
	var order []string
	for _, exec := range d.db.Execs()[asked:] {
		switch label := execLabel(exec); {
		case exec.Kind == "export-dump", strings.HasPrefix(label, "drop:"):
			order = append(order, exec.Kind+" "+label)
		}
	}
	if want := []string{"export-dump export-dump:shop", "statements drop:" + previous, "statements drop:shop"}; !slices.Equal(order, want) {
		t.Errorf("the order: %q", order)
	}
}
