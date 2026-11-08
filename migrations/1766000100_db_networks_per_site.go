package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

func init() {
	m.Register(upDBNetworksPerSite, downDBNetworksPerSite)
}

// upDBNetworksPerSite makes a database server's networks unique per stack and
// per site: the index was on the server and its stack alone, so the second PHP
// site of a server, whose stack is empty like the first's, could not be
// connected to it (#50, found with #52).
func upDBNetworksPerSite(app core.App) error {
	networks, err := app.FindCollectionByNameOrId(schema.DBNetworks)
	if err != nil {
		return err
	}
	networks.RemoveIndex("idx_db_networks_server_stack")
	networks.AddIndex("idx_db_networks_server_owner", true, "server, stack, site", "")
	return app.Save(networks)
}

func downDBNetworksPerSite(app core.App) error {
	networks, err := app.FindCollectionByNameOrId(schema.DBNetworks)
	if err != nil {
		return err
	}
	networks.RemoveIndex("idx_db_networks_server_owner")
	networks.AddIndex("idx_db_networks_server_stack", true, "server, stack", "")
	return app.Save(networks)
}
