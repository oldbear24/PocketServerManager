package panel

import (
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// Database restores (#53) put a database job's dump back with the shared
// server's own client, as the database's own user, so that no restore can
// write another database of the server: back into the database, with what
// uses it stopped and its name typed to confirm, or into a new database with
// a user of its own. They are #30's restores, a draft, its preview, a start
// and the agent's report; what is new is where they write, the plan in their
// one root, and the temporary databases a restore in place makes.

// databaseMode tells whether a restore's mode restores a database.
func databaseMode(mode string) bool {
	return mode == schema.RestoreDatabase || mode == schema.RestoreNewDatabase
}

// databaseRoot is a database restore's one root, its plan.
func databaseRoot(restore *core.Record) (restoreRoot, bool) {
	if !databaseMode(restore.GetString("mode")) {
		return restoreRoot{}, false
	}
	roots := rootsOf(restore)
	if len(roots) != 1 {
		return restoreRoot{}, false
	}
	return roots[0], true
}

// databaseRestoreOpen is the started restore that writes a database of a
// server, if any, queued or running: back into it, or into a new database of
// that name.
func (p *Panel) databaseRestoreOpen(server *core.Record, name string) *core.Record {
	for _, restore := range p.openRestores(server.GetString("host")) {
		if root, ok := databaseRoot(restore); ok && root.Container == server.GetString("container_ref") && root.Database == name {
			return restore
		}
	}
	return nil
}
