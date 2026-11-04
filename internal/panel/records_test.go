package panel

import (
	"context"
	"testing"

	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// TestBookkeepingDoesNotUndoAnEdit: a command's callback holds the stack it read a
// moment ago, and meanwhile someone saved new secrets. Writing the state of the
// command must not write the old secrets back.
func TestBookkeepingDoesNotUndoAnEdit(t *testing.T) {
	p, app, _ := newTestPanel(t)

	host := testutil.NewHost(t, app, "edit-host")
	collection, err := app.FindCollectionByNameOrId(schema.Stacks)
	if err != nil {
		t.Fatal(err)
	}
	stack := core.NewRecord(collection)
	stack.Set("host", host.Id)
	stack.Set("name", "shop")
	stack.Set("env", "A=1\n")
	stack.Set("status", stackDeploying)
	if err := app.Save(stack); err != nil {
		t.Fatal(err)
	}

	// what a callback read before the edit
	stale, err := app.FindRecordById(schema.Stacks, stack.Id)
	if err != nil {
		t.Fatal(err)
	}

	edited, err := app.FindRecordById(schema.Stacks, stack.Id)
	if err != nil {
		t.Fatal(err)
	}
	edited.Set("env", "A=1\nB=2\n")
	if err := p.applySecretChange(edited, &secretChange{Set: map[string]string{"TOKEN": "new-token"}}); err != nil {
		t.Fatal(err)
	}
	if err := app.Save(edited); err != nil {
		t.Fatal(err)
	}

	p.setFields(stale, map[string]any{"status": stackDeployed})

	stored, err := app.FindRecordById(schema.Stacks, stack.Id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.GetString("status") != stackDeployed {
		t.Errorf("status: %s", stored.GetString("status"))
	}
	if stored.GetString("env") != "A=1\nB=2\n" {
		t.Errorf("the edit of the env file was undone: %q", stored.GetString("env"))
	}
	if keys := secretKeysOf(stored); len(keys) != 1 || keys[0] != "TOKEN" {
		t.Errorf("the new secret was undone: %v", keys)
	}
	// and the record the callback holds now shows what was saved
	if stale.GetString("env") != "A=1\nB=2\n" {
		t.Errorf("the caller's record: %q", stale.GetString("env"))
	}

	// a change that finds nothing to do saves nothing
	before := stored.GetString("updated")
	if err := p.update(context.Background(), stored, func(*core.Record) error { return errUnchanged }); err != nil {
		t.Fatal(err)
	}
	if again, _ := app.FindRecordById(schema.Stacks, stack.Id); again.GetString("updated") != before {
		t.Error("an unchanged record was saved")
	}
}

// TestSaveChangesKeepsWhatOthersSaved: a callback that changed one field of what
// it read saves that field only.
func TestSaveChangesKeepsWhatOthersSaved(t *testing.T) {
	p, app, _ := newTestPanel(t)

	host := testutil.NewHost(t, app, "changes-host")
	collection, err := app.FindCollectionByNameOrId(schema.Stacks)
	if err != nil {
		t.Fatal(err)
	}
	stack := core.NewRecord(collection)
	stack.Set("host", host.Id)
	stack.Set("name", "shop")
	stack.Set("env", "A=1\n")
	stack.Set("status", stackDeploying)
	if err := p.saveChanges(context.Background(), stack); err != nil {
		t.Fatal("a new record:", err)
	}

	callback, err := app.FindRecordById(schema.Stacks, stack.Id)
	if err != nil {
		t.Fatal(err)
	}

	editor, err := app.FindRecordById(schema.Stacks, stack.Id)
	if err != nil {
		t.Fatal(err)
	}
	editor.Set("env", "A=2\n")
	if err := app.Save(editor); err != nil {
		t.Fatal(err)
	}

	callback.Set("status", stackDeployed)
	if err := p.saveChanges(context.Background(), callback); err != nil {
		t.Fatal(err)
	}

	stored, err := app.FindRecordById(schema.Stacks, stack.Id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.GetString("env") != "A=2\n" || stored.GetString("status") != stackDeployed {
		t.Errorf("env %q, status %s", stored.GetString("env"), stored.GetString("status"))
	}
}
