package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// RevertError says why a change cannot be reverted. Its message is safe to show.
type RevertError struct {
	Reason string
}

func (e *RevertError) Error() string {
	return e.Reason
}

// revertible lists collections restored by this generic revert. Stacks are not
// among them: they revert by deploying an older revision as a new one.
func revertible(collection string) bool {
	for _, c := range schema.Audited {
		if c.Name == collection {
			return collection != schema.Stacks
		}
	}
	return false
}

// Revert restores the previous non-secret values of a change, as a new change
// attributed to actor: an update gets its old values back, a create is removed
// and a delete is recreated (secrets excluded, so they must be set again).
func Revert(ctx context.Context, app core.App, entry *core.Record, actor Actor) error {
	if entry.GetString("category") != CategoryChange {
		return &RevertError{"Only configuration changes can be reverted."}
	}

	name := entry.GetString("collection")
	if !revertible(name) {
		return &RevertError{fmt.Sprintf("Changes of %q cannot be reverted automatically.", name)}
	}

	collection, err := app.FindCachedCollectionByNameOrId(name)
	if err != nil {
		return &RevertError{"The collection no longer exists."}
	}

	ctx = WithActor(ctx, actor)
	id := entry.GetString("record_id")

	switch entry.GetString("action") {
	case ActionCreate:
		record, err := app.FindRecordById(collection, id)
		if err != nil {
			return &RevertError{"The record no longer exists."}
		}
		return app.DeleteWithContext(ctx, record)

	case ActionUpdate:
		record, err := app.FindRecordById(collection, id)
		if err != nil {
			return &RevertError{"The record no longer exists."}
		}

		changes := map[string]any{}
		if err := json.Unmarshal([]byte(entry.GetString("changes")), &changes); err != nil {
			return err
		}

		restored := 0
		for field, change := range changes {
			diff, ok := change.(map[string]any)
			if !ok {
				// a secret: only the fact that it changed was recorded
				continue
			}
			record.Set(field, diff["from"])
			restored++
		}
		if restored == 0 {
			return &RevertError{"This change only touched secrets, which are not recorded."}
		}

		return app.SaveWithContext(ctx, record)

	case ActionDelete:
		snapshot := map[string]any{}
		if err := json.Unmarshal([]byte(entry.GetString("snapshot")), &snapshot); err != nil {
			return err
		}

		record := core.NewRecord(collection)
		record.Id = id
		for field, value := range snapshot {
			if definition := collection.Fields.GetByName(field); definition != nil && isSecretField(name, definition) {
				continue
			}
			record.Set(field, value)
		}

		return app.SaveWithContext(ctx, record)
	}

	return &RevertError{"This entry cannot be reverted."}
}
