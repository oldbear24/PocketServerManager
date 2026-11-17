package panel

import (
	"context"
	"testing"

	"github.com/pocketbase/dbx"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/sites"
)

// TestASiteIsConnectedOnceThoughConnectedTwiceAtOnce (#50): a new PHP site's
// database is connected to it by the database's own end, and the setup's
// next step connects it as well when it finds it not connected yet; a look of
// the setup's tick may start that step while the database's end still
// connects it. One connects the site, the other finds it connected: one
// network, one settings record, no error.
func TestASiteIsConnectedOnceThoughConnectedTwiceAtOnce(t *testing.T) {
	f := newJobFixture(t)
	// a site lives on a server with the edge role
	f.host.Set("roles", append(f.host.GetStringSlice("roles"), schema.HostRoleEdge))
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	server := f.save(schema.DBServers, map[string]any{"host": f.host.Id, "name": "pool", "engine": schema.EnginePostgres, "kind": schema.DBServerShared})
	database := f.save(schema.Databases, map[string]any{"server": server.Id, "name": "blog"})
	f.save(schema.DBUsers, map[string]any{"server": server.Id, "name": "blog"})
	site := f.save(schema.Sites, map[string]any{"host": f.host.Id, "name": "blog", "source": sites.SourceEmpty})

	const callers = 8
	start := make(chan struct{})
	errs := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			errs <- f.p.connectSite(context.Background(), databaseRequest{Actor: dbActor}, server, database, site, false)
		}()
	}
	close(start)
	for range callers {
		if err := <-errs; err != nil {
			t.Errorf("a connect: %v", err)
		}
	}

	networks, err := f.app.FindRecordsByFilter(schema.DBNetworks, "site={:site}", "", 0, 0, dbx.Params{"site": site.Id})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := f.app.FindRecordsByFilter(schema.SitePHP, "site={:site}", "", 0, 0, dbx.Params{"site": site.Id})
	if err != nil {
		t.Fatal(err)
	}
	if len(networks) != 1 || len(settings) != 1 || settings[0].GetString("database") != database.Id {
		t.Errorf("%d networks, %d settings records", len(networks), len(settings))
	}
}
