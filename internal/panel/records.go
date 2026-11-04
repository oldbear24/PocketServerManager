package panel

import (
	"context"
	"errors"
	"reflect"

	"github.com/pocketbase/pocketbase/core"
)

// errUnchanged tells update that a change found nothing to do: nothing is saved,
// and the caller is not told about an error.
var errUnchanged = errors.New("nothing to change")

// update applies change to a record as it is stored now and saves it, in one
// transaction. PocketBase writes through a single connection, so no other save
// lands between this read and this write: a deploy that finishes while someone
// saves a stack's secrets cannot write the old secrets back, and the save cannot
// write back an old status. The caller's record then holds what was saved.
func (p *Panel) update(ctx context.Context, record *core.Record, change func(fresh *core.Record) error) error {
	var saved *core.Record
	err := p.App.RunInTransaction(func(tx core.App) error {
		fresh, err := tx.FindRecordById(record.Collection().Id, record.Id)
		if err != nil {
			return err
		}
		if err := change(fresh); err != nil {
			return err
		}
		saved = fresh
		return tx.SaveWithContext(ctx, fresh)
	})
	if errors.Is(err, errUnchanged) {
		return nil
	}
	if err != nil {
		return err
	}

	record.Load(saved.FieldsData())
	return nil
}

// setFields stores a few fields of a record with update, for the panel's own
// bookkeeping, such as the state of a stack after a command.
func (p *Panel) setFields(record *core.Record, fields map[string]any) {
	err := p.update(context.Background(), record, func(fresh *core.Record) error {
		for name, value := range fields {
			fresh.Set(name, value)
		}
		return nil
	})
	if err != nil {
		p.Log.Error("failed to store fields of a record", "collection", record.Collection().Name, "record", record.Id, "error", err)
	}
}

// saveChanges saves what changed on a record since it was read onto the record
// as it is stored now (see update). Callbacks and routes read a record, change a
// field or two and save it; the rest of what they read may be out of date by
// then, and must not be written back. A new record is simply saved.
func (p *Panel) saveChanges(ctx context.Context, record *core.Record) error {
	if record.IsNew() {
		return p.App.SaveWithContext(ctx, record)
	}

	original := record.Original()
	changed := map[string]any{}
	for _, field := range record.Collection().Fields {
		name := field.GetName()
		if value := record.Get(name); !reflect.DeepEqual(value, original.Get(name)) {
			changed[name] = value
		}
	}
	if len(changed) == 0 {
		return nil
	}

	return p.update(ctx, record, func(fresh *core.Record) error {
		for name, value := range changed {
			fresh.Set(name, value)
		}
		return nil
	})
}
