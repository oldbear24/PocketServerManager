package audit

import (
	"encoding/json"
	"regexp"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// Security event actions.
const (
	ActionSignIn             = "sign_in"
	ActionSignInFailed       = "sign_in_failed"
	ActionMFAChanged         = "mfa_changed"
	ActionEnrollmentToken    = "enrollment_token"
	ActionEnrollment         = "enrollment"
	ActionEnrollmentFailed   = "enrollment_failed"
	ActionCertRevoked        = "cert_revoked"
	ActionCertRejected       = "cert_rejected"
	ActionTerminalSession    = "terminal_session"
	ActionTerminalSessionEnd = "terminal_session_end"
	ActionAdminerSession     = "adminer_session"
	ActionPayloadCapture     = "payload_capture"
	ActionIntegrityBroken    = "integrity_broken"
	// ActionChainSealed records `panel audit seal`, which vouches for entries
	// nothing proved.
	ActionChainSealed = "chain_sealed"
)

var sensitiveSetting = regexp.MustCompile(`(?i)password|secret|token|key`)

// registerSecurity records sign-ins and failed sign-ins.
func (w *Writer) registerSecurity() {
	w.app.OnRecordAuthRequest().BindFunc(func(e *core.RecordAuthRequestEvent) error {
		if err := e.Next(); err != nil {
			return err
		}

		// the request has no auth yet: the record is the one signing in
		actor := actorFromHTTP(e.Request, e.Record, e.Collection.Name == schema.Superusers, e.RealIP())
		w.Security(ActionSignIn, actor, e.Record.Email(), map[string]any{"method": e.AuthMethod})

		return nil
	})

	w.app.OnRecordAuthWithPasswordRequest().BindFunc(func(e *core.RecordAuthWithPasswordRequestEvent) error {
		err := e.Next()
		if err != nil {
			actor := ActorFromRequest(e.RequestEvent)
			w.Security(ActionSignInFailed, actor, truncate(e.Identity, 255), map[string]any{"collection": e.Collection.Name})
		}
		return err
	})
}

// registerSettings audits PocketBase's own settings. The values of secret-like
// fields are never written, only the fact that they changed.
func (w *Writer) registerSettings() {
	w.app.OnSettingsUpdateRequest().BindFunc(func(e *core.SettingsUpdateRequestEvent) error {
		actor := ActorFromRequest(e.RequestEvent)

		before := flattenSettings(e.OldSettings)

		original := e.App
		defer func() { e.App = original }()

		return w.inTx(e.App, actor, func(tx core.App) error {
			e.App = tx

			if err := e.Next(); err != nil {
				return err
			}

			after := flattenSettings(e.NewSettings)

			changes := diffFlat(before, after)
			if len(changes) == 0 {
				return nil
			}

			return w.append(tx, entry{
				Category:    CategoryChange,
				Action:      ActionUpdate,
				Actor:       actor,
				Collection:  "_params",
				RecordID:    "settings",
				RecordLabel: "PocketBase settings",
				Changes:     changes,
			})
		})
	})
}

// registerCollections audits changes of the collection schema, including those
// made by migrations once the audit log exists.
func (w *Writer) registerCollections() {
	bindRequest := func(e *core.CollectionRequestEvent) error {
		return w.withRequestActor(e.Collection, e.RequestEvent, e.Next)
	}
	w.app.OnCollectionCreateRequest().BindFunc(bindRequest)
	w.app.OnCollectionUpdateRequest().BindFunc(bindRequest)
	w.app.OnCollectionDeleteRequest().BindFunc(bindRequest)

	const priority = -1000

	w.app.OnCollectionCreateExecute().Bind(&hook.Handler[*core.CollectionEvent]{
		Id: "pocketcloudAuditCollectionCreate", Priority: priority,
		Func: func(e *core.CollectionEvent) error {
			return w.onCollectionChange(e, ActionCreate, nil)
		},
	})
	w.app.OnCollectionUpdateExecute().Bind(&hook.Handler[*core.CollectionEvent]{
		Id: "pocketcloudAuditCollectionUpdate", Priority: priority,
		Func: func(e *core.CollectionEvent) error {
			before, err := e.App.FindCollectionByNameOrId(e.Collection.Id)
			if err != nil {
				return e.Next()
			}
			return w.onCollectionChange(e, ActionUpdate, before)
		},
	})
	w.app.OnCollectionDeleteExecute().Bind(&hook.Handler[*core.CollectionEvent]{
		Id: "pocketcloudAuditCollectionDelete", Priority: priority,
		Func: func(e *core.CollectionEvent) error {
			return w.onCollectionChange(e, ActionDelete, e.Collection)
		},
	})
}

func (w *Writer) onCollectionChange(e *core.CollectionEvent, action string, before *core.Collection) error {
	// nothing but a migration changes the schema without a request
	actor, found := w.findActor(e.Context, e.App, e.Collection)
	if !found {
		actor = System("migration", SourceMigration)
	}

	name := e.Collection.Name

	var beforeFlat map[string]any
	if before != nil {
		beforeFlat = flattenJSON(before)
	}

	original := e.App
	defer func() { e.App = original }()

	return w.inTx(e.App, actor, func(tx core.App) error {
		e.App = tx

		if err := e.Next(); err != nil {
			return err
		}

		var changes map[string]any
		if action != ActionDelete {
			changes = diffFlat(beforeFlat, flattenJSON(e.Collection))
			if action == ActionUpdate && len(changes) == 0 {
				return nil
			}
		}

		return w.append(tx, entry{
			Category:    CategoryChange,
			Action:      action,
			Actor:       actor,
			Collection:  "_collections",
			RecordID:    e.Collection.Id,
			RecordLabel: name,
			Changes:     changes,
		})
	})
}

func flattenSettings(settings *core.Settings) map[string]any {
	if settings == nil {
		return nil
	}
	return flattenJSON(settings)
}

// flattenJSON turns a value into a one-level map of its JSON form. Nested
// objects become dotted keys; arrays are kept whole.
func flattenJSON(value any) map[string]any {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}

	flat := map[string]any{}

	var walk func(prefix string, value map[string]any)
	walk = func(prefix string, value map[string]any) {
		for k, v := range value {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			if nested, ok := v.(map[string]any); ok {
				walk(key, nested)
				continue
			}
			flat[key] = v
		}
	}
	walk("", doc)

	return flat
}

// diffFlat compares two flattened documents. Keys that look secret only say "changed".
func diffFlat(before, after map[string]any) map[string]any {
	changes := map[string]any{}

	record := func(key string, from, to any) {
		if isJSONEqual(from, to) {
			return
		}
		if sensitiveSetting.MatchString(key) {
			changes[key] = secretChanged
			return
		}
		changes[key] = map[string]any{"from": from, "to": to}
	}

	for key, to := range after {
		record(key, before[key], to)
	}
	for key, from := range before {
		if _, ok := after[key]; !ok {
			record(key, from, nil)
		}
	}

	return changes
}

func isJSONEqual(a, b any) bool {
	rawA, errA := json.Marshal(a)
	rawB, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(rawA) == string(rawB)
}
