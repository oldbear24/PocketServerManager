package migrations

import (
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/oldbear24/PocketServerManager/internal/audit"
)

// The head of the audit chain starts from the newest entry, unsealed; the first
// write with the master key seals it (see audit.HeadTable).
func init() {
	m.Register(audit.CreateHeadTable, audit.DropHeadTable)
}
