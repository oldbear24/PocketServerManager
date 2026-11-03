package panel

import (
	"errors"
	"fmt"
	"slices"

	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
)

// bindEncryption seals every field listed in schema.Encrypted before it is
// stored. Reading a value back needs an explicit Open by the code that needs it.
func (p *Panel) bindEncryption() {
	tags := make([]string, 0, len(schema.Encrypted))
	for name := range schema.Encrypted {
		tags = append(tags, name)
	}
	slices.Sort(tags)

	seal := func(e *core.RecordEvent) error {
		for _, field := range schema.Encrypted[e.Record.Collection().Name] {
			value := e.Record.GetString(field)
			if value == "" || secrets.IsSealed(value) {
				continue
			}

			if p.Box == nil {
				return errors.New("the master key is not loaded")
			}

			sealed, err := p.Box.SealString(value)
			if err != nil {
				return fmt.Errorf("failed to encrypt %s: %w", field, err)
			}
			e.Record.Set(field, sealed)
		}

		return e.Next()
	}

	p.App.OnRecordCreateExecute(tags...).BindFunc(seal)
	p.App.OnRecordUpdateExecute(tags...).BindFunc(seal)
}
