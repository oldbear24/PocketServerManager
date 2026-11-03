package panel

import (
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/stacks"
)

// resolveDatabaseSecret finds a database password by its secret name, `db/<name>`.
func (p *Panel) resolveDatabaseSecret(string) (string, error) {
	return "", stacks.ErrUnknownSecret
}

// stackOverride generates the second Compose file of a stack, which attaches it to
// its database networks. Most stacks have none.
func (p *Panel) stackOverride(*core.Record, string) (string, error) {
	return "", nil
}
