package panel

import (
	"testing"

	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// TestADeploymentSettledWithoutItsResultKeepsWhatIsKnown: a command that
// finished while its agent was away is settled without its result. Its
// deployment goes live, and nothing is guessed about the other releases.
func TestADeploymentSettledWithoutItsResultKeepsWhatIsKnown(t *testing.T) {
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)
	p := New(app, &Config{MasterKey: testutil.MasterKey}, testUI)
	p.Register()
	testutil.Handler(t, app)

	host := testutil.NewHost(t, app, "edge-1")
	host.Set("roles", []string{schema.HostRoleDocker, schema.HostRoleEdge})
	if err := app.Save(host); err != nil {
		t.Fatal(err)
	}
	sitesCollection, _ := app.FindCollectionByNameOrId(schema.Sites)
	site := core.NewRecord(sitesCollection)
	site.Set("name", "blog")
	site.Set("host", host.Id)
	site.Set("source", "upload")
	if err := app.Save(site); err != nil {
		t.Fatal(err)
	}

	deploymentsCollection, _ := app.FindCollectionByNameOrId(schema.Deployments)
	older := core.NewRecord(deploymentsCollection)
	older.Load(map[string]any{"site": site.Id, "number": 1, "status": schema.DeploymentSucceeded, "on_server": true, "size_bytes": 100})
	if err := app.Save(older); err != nil {
		t.Fatal(err)
	}
	commandsCollection, _ := app.FindCollectionByNameOrId(schema.Commands)
	command := core.NewRecord(commandsCollection)
	command.Load(map[string]any{"host": host.Id, "type": "site.deploy", "status": schema.CommandSucceeded,
		"payload": map[string]any{"site": "blog", "deployment": 2}})
	if err := app.Save(command); err != nil {
		t.Fatal(err)
	}
	// the command finished before its deployment recorded it
	latest := core.NewRecord(deploymentsCollection)
	latest.Load(map[string]any{"site": site.Id, "number": 2, "status": schema.DeploymentBuilding})
	if err := app.Save(latest); err != nil {
		t.Fatal(err)
	}

	// as the manager hands a finished command to its finishers
	command.Set("status", schema.CommandSucceeded)
	command.Set("result", nil)
	p.siteDeployFinished(command)

	latest, _ = app.FindRecordById(schema.Deployments, latest.Id)
	older, _ = app.FindRecordById(schema.Deployments, older.Id)
	site, _ = app.FindRecordById(schema.Sites, site.Id)
	if latest.GetString("status") != schema.DeploymentSucceeded || !latest.GetBool("on_server") || site.GetString("active_deployment") != latest.Id {
		t.Errorf("the settled deployment: %v, active %s", latest.PublicExport(), site.GetString("active_deployment"))
	}
	if !older.GetBool("on_server") || older.GetInt("size_bytes") != 100 {
		t.Errorf("the older release was marked without knowing: %v", older.PublicExport())
	}
}
