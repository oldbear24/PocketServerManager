// Package audit writes every configuration change and security event to an
// append-only, hash-chained log. See "Change audit" in docs/architecture.md.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
	"github.com/pocketbase/pocketbase/tools/types"
	"github.com/spf13/cast"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// Entry actions, besides the ones of security events.
const (
	ActionCreate     = "create"
	ActionUpdate     = "update"
	ActionDelete     = "delete"
	ActionRevert     = "revert"
	ActionCheckpoint = "checkpoint"
)

// Entry categories.
const (
	CategoryChange   = "change"
	CategorySecurity = "security"
	CategorySystem   = "system"
)

const secretChanged = "changed"

// ErrImmutable is returned for any attempt to change or remove an audit entry.
var ErrImmutable = errors.New("audit log entries cannot be changed or deleted")

type writerKey struct{}

// Writer is the only component that writes to the audit log.
type Writer struct {
	app         core.App
	collections map[string]schema.AuditedCollection

	// requestActors holds the actor of an API save by the saved model. PocketBase
	// does not pass the request context to the model hooks of an API save, so the
	// request hooks leave the actor here for the execute hooks to pick up.
	requestActors sync.Map

	// txActors holds the actor of a transaction, so that saves made inside it by
	// hooks (cascade deletes, for example) are attributed to the same actor.
	txActors sync.Map
}

// New creates a writer for the given audited collections.
func New(app core.App, audited []schema.AuditedCollection) *Writer {
	w := &Writer{app: app, collections: make(map[string]schema.AuditedCollection, len(audited))}
	for _, c := range audited {
		w.collections[c.Name] = c
	}
	return w
}

// Register binds the writer to the app's hooks.
func (w *Writer) Register() {
	w.registerGuards()

	// runs before PocketBase's own execute hooks, so that they and the actual
	// write join the transaction started here
	const priority = -1000

	w.app.OnRecordCreateExecute().Bind(&hook.Handler[*core.RecordEvent]{
		Id: "pocketcloudAuditCreate", Priority: priority,
		Func: func(e *core.RecordEvent) error { return w.onRecordSave(e, ActionCreate) },
	})
	w.app.OnRecordUpdateExecute().Bind(&hook.Handler[*core.RecordEvent]{
		Id: "pocketcloudAuditUpdate", Priority: priority,
		Func: func(e *core.RecordEvent) error { return w.onRecordSave(e, ActionUpdate) },
	})
	w.app.OnRecordDeleteExecute().Bind(&hook.Handler[*core.RecordEvent]{
		Id: "pocketcloudAuditDelete", Priority: priority,
		Func: func(e *core.RecordEvent) error { return w.onRecordDelete(e) },
	})

	w.app.OnRecordCreateRequest().BindFunc(func(e *core.RecordRequestEvent) error {
		return w.withRequestActor(e.Record, e.RequestEvent, e.Next)
	})
	w.app.OnRecordUpdateRequest().BindFunc(func(e *core.RecordRequestEvent) error {
		return w.withRequestActor(e.Record, e.RequestEvent, e.Next)
	})
	w.app.OnRecordDeleteRequest().BindFunc(func(e *core.RecordRequestEvent) error {
		return w.withRequestActor(e.Record, e.RequestEvent, e.Next)
	})

	w.registerCollections()
	w.registerSettings()
	w.registerSecurity()
}

func (w *Writer) withRequestActor(model any, e *core.RequestEvent, next func() error) error {
	w.requestActors.Store(model, ActorFromRequest(e))
	defer w.requestActors.Delete(model)
	return next()
}

// resolveActor finds who made a change: an explicit context value first, then
// the request that is saving this model, then the surrounding transaction. A change
// without any of these comes from the panel itself.
func (w *Writer) resolveActor(ctx context.Context, app core.App, model any) Actor {
	if actor, ok := w.findActor(ctx, app, model); ok {
		return actor
	}
	return System("panel", SourcePanel)
}

func (w *Writer) findActor(ctx context.Context, app core.App, model any) (Actor, bool) {
	if actor, ok := actorFromContext(ctx); ok {
		return actor, true
	}
	if actor, ok := w.requestActors.Load(model); ok {
		return actor.(Actor), true
	}
	if info := app.TxInfo(); info != nil {
		if actor, ok := w.txActors.Load(info); ok {
			return actor.(Actor), true
		}
	}
	return Actor{}, false
}

// inTx runs fn in a transaction that remembers the actor for nested saves.
func (w *Writer) inTx(app core.App, actor Actor, fn func(tx core.App) error) error {
	return app.RunInTransaction(func(tx core.App) error {
		info := tx.TxInfo()
		if _, loaded := w.txActors.LoadOrStore(info, actor); !loaded {
			defer w.txActors.Delete(info)
		}
		return fn(tx)
	})
}

func (w *Writer) onRecordSave(e *core.RecordEvent, action string) error {
	audited, ok := w.collections[e.Record.Collection().Name]
	if !ok {
		return e.Next()
	}

	// the fast path for live-status saves: nothing to audit, no transaction needed
	if action == ActionUpdate && len(diffRecord(e.Record, storedRecord(e.App, e.Record), audited)) == 0 {
		return e.Next()
	}

	actor := w.resolveActor(e.Context, e.App, e.Record)

	original := e.App
	defer func() { e.App = original }()

	return w.inTx(e.App, actor, func(tx core.App) error {
		e.App = tx

		var before *core.Record
		if action == ActionUpdate {
			before = storedRecord(tx, e.Record)
		}

		if err := e.Next(); err != nil {
			return err
		}

		// a create only has its final values (and id) after e.Next()
		changes := diffRecord(e.Record, before, audited)
		if action == ActionUpdate && len(changes) == 0 {
			return nil
		}

		if audited.Revision != "" {
			actor.Revision = revisionStep(e.Record, before, audited.Revision)
		}

		return w.append(tx, entry{
			Category:    CategoryChange,
			Action:      action,
			Actor:       actor,
			Collection:  e.Record.Collection().Name,
			RecordID:    e.Record.Id,
			RecordLabel: recordLabel(e.Record, audited),
			Host:        recordHost(e.Record, audited),
			Changes:     changes,
		})
	})
}

// revisionStep describes the move a save made between revisions, such as "r7 → r8".
func revisionStep(record, before *core.Record, field string) string {
	to := record.GetInt(field)
	if before == nil {
		if to == 0 {
			return ""
		}
		return fmt.Sprintf("r%d", to)
	}

	from := before.GetInt(field)
	if from == to {
		return ""
	}

	return fmt.Sprintf("r%d → r%d", from, to)
}

// storedRecord returns the record as it is in the database. Record.Original() is
// not enough: it stays blank for a record created with NewRecord in this process
// and is not refreshed by a save, so a later save of the same instance would be
// compared with stale values.
func storedRecord(app core.App, record *core.Record) *core.Record {
	stored, err := app.FindRecordById(record.Collection(), cast.ToString(record.LastSavedPK()))
	if err != nil {
		return record.Original()
	}
	return stored
}

func (w *Writer) onRecordDelete(e *core.RecordEvent) error {
	name := e.Record.Collection().Name

	if name == schema.AuditLog {
		// guarded separately
		return e.Next()
	}

	audited, ok := w.collections[name]
	if !ok {
		return e.Next()
	}

	actor := w.resolveActor(e.Context, e.App, e.Record)

	original := e.App
	defer func() { e.App = original }()

	return w.inTx(e.App, actor, func(tx core.App) error {
		e.App = tx

		snapshot := snapshotRecord(e.Record)
		label := recordLabel(e.Record, audited)
		host := recordHost(e.Record, audited)

		if err := e.Next(); err != nil {
			return err
		}

		return w.append(tx, entry{
			Category:    CategoryChange,
			Action:      ActionDelete,
			Actor:       actor,
			Collection:  name,
			RecordID:    e.Record.Id,
			RecordLabel: label,
			Host:        host,
			Snapshot:    snapshot,
		})
	})
}

// entry is what the writer needs to know to write one audit entry.
type entry struct {
	Category    string
	Action      string
	Actor       Actor
	Collection  string
	RecordID    string
	RecordLabel string
	Host        string
	Changes     map[string]any
	Snapshot    map[string]any
}

// append chains and stores one entry. It must run inside a transaction: SQLite
// serialises writers, so reading the previous hash here keeps the chain ordered.
func (w *Writer) append(tx core.App, e entry) error {
	collection, err := tx.FindCachedCollectionByNameOrId(schema.AuditLog)
	if err != nil {
		// the audit log does not exist yet: the initial migrations are still running
		return nil //nolint:nilerr
	}

	last := struct {
		Seq  int    `db:"seq"`
		Hash string `db:"hash"`
	}{}
	err = tx.DB().Select("seq", "hash").From(schema.AuditLog).OrderBy("seq DESC").Limit(1).One(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	now, err := types.ParseDateTime(time.Now().UTC())
	if err != nil {
		return err
	}

	record := core.NewRecord(collection)
	record.Set("seq", last.Seq+1)
	record.Set("at", now.String())
	record.Set("category", e.Category)
	record.Set("action", e.Action)
	record.Set("actor_kind", e.Actor.Kind)
	record.Set("actor", e.Actor.ID)
	record.Set("actor_label", truncate(e.Actor.Label, 255))
	record.Set("source", e.Actor.Source)
	record.Set("ip", e.Actor.IP)
	record.Set("user_agent", truncate(e.Actor.UserAgent, 512))
	record.Set("collection", e.Collection)
	record.Set("record_id", e.RecordID)
	record.Set("record_label", truncate(e.RecordLabel, 255))
	record.Set("host", e.Host)
	record.Set("command", e.Actor.Command)
	record.Set("revision", e.Actor.Revision)
	record.Set("note", truncate(e.Actor.Note, 1024))
	record.Set("prev_hash", last.Hash)

	if len(e.Changes) > 0 {
		record.Set("changes", e.Changes)
	}
	if len(e.Snapshot) > 0 {
		record.Set("snapshot", e.Snapshot)
	}

	canonical, err := canonicalEntry(record)
	if err != nil {
		return err
	}
	record.Set("hash", entryHash(last.Hash, canonical))

	return tx.SaveNoValidateWithContext(context.WithValue(context.Background(), writerKey{}, true), record)
}

// Security records a security event outside of any record change.
func (w *Writer) Security(action string, actor Actor, label string, details map[string]any) {
	err := w.app.RunInTransaction(func(tx core.App) error {
		return w.append(tx, entry{
			Category:    CategorySecurity,
			Action:      action,
			Actor:       actor,
			RecordLabel: label,
			Snapshot:    details,
		})
	})
	if err != nil {
		slog.Error("failed to write a security event", "action", action, "error", err)
	}
}

// registerGuards makes the log append-only: direct writes are rejected for
// everyone, superusers included. Only the writer holds the context value that
// lets a new entry through; retention removes old entries with plain SQL.
func (w *Writer) registerGuards() {
	w.app.OnRecordCreateExecute(schema.AuditLog).BindFunc(func(e *core.RecordEvent) error {
		if e.Context == nil || e.Context.Value(writerKey{}) != true {
			return ErrImmutable
		}
		return e.Next()
	})
	w.app.OnRecordUpdateExecute(schema.AuditLog).BindFunc(func(*core.RecordEvent) error {
		return ErrImmutable
	})
	w.app.OnRecordDeleteExecute(schema.AuditLog).BindFunc(func(*core.RecordEvent) error {
		return ErrImmutable
	})
}

// diffRecord returns the field-level diff of a save against the stored record;
// before is nil for a new record. Secret values never get in: encrypted, hidden
// and password fields only say "changed".
func diffRecord(record, before *core.Record, audited schema.AuditedCollection) map[string]any {
	changes := map[string]any{}
	collection := record.Collection()
	isNew := before == nil

	for _, field := range collection.Fields {
		name := field.GetName()

		if isSkippedField(field) || slices.Contains(audited.Ignore, name) {
			continue
		}

		var from any
		if !isNew {
			from = normalize(before.GetRaw(name))
		}
		to := normalize(record.GetRaw(name))

		if isSecretField(collection.Name, field) {
			if isNew && isEmpty(to) {
				continue
			}
			if !isNew && reflect.DeepEqual(from, to) {
				continue
			}
			changes[name] = secretChanged
			continue
		}

		if isNew {
			if !isEmpty(to) {
				changes[name] = map[string]any{"to": to}
			}
			continue
		}

		if !reflect.DeepEqual(from, to) {
			changes[name] = map[string]any{"from": from, "to": to}
		}
	}

	return changes
}

// snapshotRecord returns the last state of a deleted record, without secrets.
func snapshotRecord(record *core.Record) map[string]any {
	snapshot := map[string]any{}
	collection := record.Collection()

	for _, field := range collection.Fields {
		if isSkippedField(field) {
			continue
		}

		name := field.GetName()
		value := normalize(record.GetRaw(name))

		if isSecretField(collection.Name, field) {
			if !isEmpty(value) {
				snapshot[name] = secretChanged
			}
			continue
		}
		if !isEmpty(value) {
			snapshot[name] = value
		}
	}

	return snapshot
}

func isSkippedField(field core.Field) bool {
	if _, ok := field.(*core.AutodateField); ok {
		return true
	}
	return field.GetName() == core.FieldNameId
}

func isSecretField(collection string, field core.Field) bool {
	if _, ok := field.(*core.PasswordField); ok {
		return true
	}
	return field.GetHidden() || schema.IsEncrypted(collection, field.GetName())
}

// normalize turns a raw field value into plain JSON values, so that values can
// be compared and hashed no matter how they were set.
func normalize(value any) any {
	if value == nil {
		return nil
	}

	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}

	var normalized any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return nil
	}

	return normalized
}

func isEmpty(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case float64:
		return v == 0
	case bool:
		return !v
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	}
	return false
}

func recordLabel(record *core.Record, audited schema.AuditedCollection) string {
	if audited.Label == "" {
		return record.Id
	}

	label := strings.Join(record.GetStringSlice(audited.Label), ", ")
	if label == "" {
		label = record.GetString(audited.Label)
	}
	if label == "" {
		return record.Id
	}

	return label
}

func recordHost(record *core.Record, audited schema.AuditedCollection) string {
	if audited.Host != "" {
		return record.GetString(audited.Host)
	}
	if record.Collection().Name == schema.Hosts {
		return record.Id
	}
	return ""
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return strings.ToValidUTF8(value[:limit], "")
}

// Prune removes entries older than the retention period. It first writes a
// checkpoint entry holding the position the chain continues from, so the rest of
// the chain stays verifiable.
func (w *Writer) Prune(retention time.Duration) (int64, error) {
	cutoff, err := types.ParseDateTime(time.Now().UTC().Add(-retention))
	if err != nil {
		return 0, err
	}

	var removed int64

	err = w.app.RunInTransaction(func(tx core.App) error {
		boundary := struct {
			Seq int `db:"seq"`
		}{}
		// the newest entry that is due to be removed
		err := tx.DB().Select("seq").From(schema.AuditLog).
			Where(dbx.NewExp("[[at]] < {:cutoff}", dbx.Params{"cutoff": cutoff.String()})).
			OrderBy("seq DESC").Limit(1).One(&boundary)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}

		last := struct {
			Hash string `db:"hash"`
		}{}
		err = tx.DB().Select("hash").From(schema.AuditLog).
			Where(dbx.HashExp{"seq": boundary.Seq}).One(&last)
		if err != nil {
			return err
		}

		err = w.append(tx, entry{
			Category: CategorySystem,
			Action:   ActionCheckpoint,
			Actor:    System("audit retention", SourcePanel),
			Snapshot: map[string]any{"from_seq": boundary.Seq + 1, "from_prev_hash": last.Hash},
		})
		if err != nil {
			return err
		}

		res, err := tx.DB().NewQuery("DELETE FROM {{" + schema.AuditLog + "}} WHERE [[seq]] <= {:seq}").
			Bind(dbx.Params{"seq": boundary.Seq}).Execute()
		if err != nil {
			return err
		}

		removed, err = res.RowsAffected()

		return err
	})

	return removed, err
}
