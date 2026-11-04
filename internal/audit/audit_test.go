package audit_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/panel"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

type env struct {
	app     *tests.TestApp
	panel   *panel.Panel
	handler http.Handler
}

func newEnv(t testing.TB) *env {
	t.Helper()

	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)

	p := panel.New(app, &panel.Config{MasterKey: testutil.MasterKey, GRPCAddr: "127.0.0.1:0"}, nil)
	p.Register()

	return &env{app: app, panel: p, handler: testutil.Handler(t, app)}
}

// entries returns the audit entries of a record, oldest first.
func (e *env) entries(t testing.TB, collection, id string) []*core.Record {
	t.Helper()

	records, err := e.app.FindRecordsByFilter(schema.AuditLog, "collection={:c} && record_id={:id}", "seq", 0, 0,
		map[string]any{"c": collection, "id": id})
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func (e *env) last(t testing.TB, collection, id string) *core.Record {
	t.Helper()

	records := e.entries(t, collection, id)
	if len(records) == 0 {
		t.Fatalf("no audit entries for %s/%s", collection, id)
	}
	return records[len(records)-1]
}

func changesOf(t testing.TB, entry *core.Record) map[string]any {
	t.Helper()

	changes := map[string]any{}
	if err := json.Unmarshal([]byte(entry.GetString("changes")), &changes); err != nil {
		t.Fatal(err)
	}
	return changes
}

func assertActor(t testing.TB, entry *core.Record, kind, actor, source string) {
	t.Helper()

	if got := entry.GetString("actor_kind"); got != kind {
		t.Errorf("actor_kind = %q, want %q", got, kind)
	}
	if got := entry.GetString("actor"); actor != "" && got != actor {
		t.Errorf("actor = %q, want %q", got, actor)
	}
	if got := entry.GetString("source"); got != source {
		t.Errorf("source = %q, want %q", got, source)
	}
}

func recordID(t testing.TB, body string) string {
	t.Helper()

	response := struct {
		ID string `json:"id"`
	}{}
	if err := json.Unmarshal([]byte(body), &response); err != nil || response.ID == "" {
		t.Fatalf("no id in %q (%v)", body, err)
	}
	return response.ID
}

// TestChangesFromEverySource is the first acceptance criterion: a create, an
// update and a delete through the UI, the REST API and the PocketBase dashboard
// each produce one entry with the right actor, source and diff.
func TestChangesFromEverySource(t *testing.T) {
	env := newEnv(t)

	host := testutil.NewHost(t, env.app, "audit-host")
	user, userToken := testutil.NewUser(t, env.app, "ops@example.com", schema.RoleOperator)
	superuser, superuserToken := testutil.NewSuperuser(t, env.app)

	cases := []struct {
		name    string
		token   string
		headers map[string]string
		kind    string
		actor   string
		source  string
	}{
		{"ui", userToken, map[string]string{audit.HeaderClient: audit.ClientWeb, audit.HeaderNote: "from the ui", "User-Agent": "test-browser"}, audit.KindUser, user.Id, audit.SourceUI},
		{"api", userToken, map[string]string{"User-Agent": "curl"}, audit.KindUser, user.Id, audit.SourceAPI},
		{"dashboard", superuserToken, map[string]string{"Referer": "http://panel.test/_/#/collections"}, audit.KindSuperuser, superuser.Id, audit.SourcePBAdmin},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := "/api/collections/stacks/records"

			// create
			status, body := testutil.Do(t, env.handler, testutil.Request{
				Method: "POST", URL: base, Token: tc.token, Headers: tc.headers,
				Body: fmt.Sprintf(`{"host":%q,"name":"stack-%s","compose_yaml":"services: {}"}`, host.Id, tc.name),
			})
			if status != http.StatusOK {
				t.Fatalf("create: %d %s", status, body)
			}
			id := recordID(t, body)

			entries := env.entries(t, schema.Stacks, id)
			if len(entries) != 1 {
				t.Fatalf("create produced %d entries, want 1", len(entries))
			}
			created := entries[0]
			assertActor(t, created, tc.kind, tc.actor, tc.source)
			if created.GetString("action") != audit.ActionCreate {
				t.Errorf("action = %q", created.GetString("action"))
			}
			if created.GetString("record_label") != "stack-"+tc.name {
				t.Errorf("record_label = %q", created.GetString("record_label"))
			}
			if created.GetString("host") != host.Id {
				t.Errorf("host = %q", created.GetString("host"))
			}
			if tc.name == "ui" {
				if created.GetString("note") != "from the ui" {
					t.Errorf("note = %q", created.GetString("note"))
				}
				if created.GetString("user_agent") != "test-browser" {
					t.Errorf("user_agent = %q", created.GetString("user_agent"))
				}
			}
			compose, ok := changesOf(t, created)["compose_yaml"].(map[string]any)
			if !ok || compose["to"] != "services: {}" {
				t.Errorf("create diff: %v", changesOf(t, created))
			}

			// update
			status, body = testutil.Do(t, env.handler, testutil.Request{
				Method: "PATCH", URL: base + "/" + id, Token: tc.token, Headers: tc.headers,
				Body: `{"compose_yaml":"services: {web: {}}"}`,
			})
			if status != http.StatusOK {
				t.Fatalf("update: %d %s", status, body)
			}

			entries = env.entries(t, schema.Stacks, id)
			if len(entries) != 2 {
				t.Fatalf("update produced %d entries in total, want 2", len(entries))
			}
			updated := entries[1]
			assertActor(t, updated, tc.kind, tc.actor, tc.source)
			changes := changesOf(t, updated)
			if len(changes) != 1 {
				t.Errorf("the diff must hold only the changed field: %v", changes)
			}
			diff, ok := changes["compose_yaml"].(map[string]any)
			if !ok || diff["from"] != "services: {}" || diff["to"] != "services: {web: {}}" {
				t.Errorf("update diff: %v", changes)
			}

			// delete
			status, body = testutil.Do(t, env.handler, testutil.Request{Method: "DELETE", URL: base + "/" + id, Token: tc.token, Headers: tc.headers})
			if status != http.StatusNoContent {
				t.Fatalf("delete: %d %s", status, body)
			}

			entries = env.entries(t, schema.Stacks, id)
			if len(entries) != 3 {
				t.Fatalf("delete produced %d entries in total, want 3", len(entries))
			}
			deleted := entries[2]
			assertActor(t, deleted, tc.kind, tc.actor, tc.source)
			snapshot := map[string]any{}
			if err := json.Unmarshal([]byte(deleted.GetString("snapshot")), &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot["compose_yaml"] != "services: {web: {}}" {
				t.Errorf("delete snapshot: %v", snapshot)
			}
		})
	}
}

func TestChangesWithoutRequestAreSystem(t *testing.T) {
	env := newEnv(t)

	host := testutil.NewHost(t, env.app, "system-host")
	assertActor(t, env.last(t, schema.Hosts, host.Id), audit.KindSystem, "", audit.SourcePanel)

	// a component can name itself
	host.Set("labels", map[string]any{"a": "b"})
	ctx := audit.WithActor(t.Context(), audit.System("ACME manager", "acme"))
	if err := env.app.SaveWithContext(ctx, host); err != nil {
		t.Fatal(err)
	}

	entry := env.last(t, schema.Hosts, host.Id)
	assertActor(t, entry, audit.KindSystem, "", "acme")
	if entry.GetString("actor_label") != "ACME manager" {
		t.Errorf("actor_label = %q", entry.GetString("actor_label"))
	}
}

// TestCascadeDeletesKeepTheActor covers saves made inside the same transaction.
func TestCascadeDeletesKeepTheActor(t *testing.T) {
	env := newEnv(t)

	host := testutil.NewHost(t, env.app, "cascade-host")
	admin, adminToken := testutil.NewUser(t, env.app, "admin@example.com", schema.RoleAdmin)

	collection, _ := env.app.FindCollectionByNameOrId(schema.Stacks)
	stack := core.NewRecord(collection)
	stack.Set("host", host.Id)
	stack.Set("name", "cascaded")
	if err := env.app.Save(stack); err != nil {
		t.Fatal(err)
	}

	superuser, superuserToken := testutil.NewSuperuser(t, env.app)
	_ = admin
	_ = adminToken

	status, body := testutil.Do(t, env.handler, testutil.Request{
		Method: "DELETE", URL: "/api/collections/hosts/records/" + host.Id, Token: superuserToken,
	})
	if status != http.StatusNoContent {
		t.Fatalf("%d %s", status, body)
	}

	assertActor(t, env.last(t, schema.Hosts, host.Id), audit.KindSuperuser, superuser.Id, audit.SourceAPI)
	assertActor(t, env.last(t, schema.Stacks, stack.Id), audit.KindSuperuser, superuser.Id, audit.SourceAPI)

	if result, err := audit.Verify(env.app, env.panel.AuditKey()); err != nil || !result.OK {
		t.Fatalf("chain after a cascade: %+v, %v", result, err)
	}
}

func TestLiveStatusIsNotAudited(t *testing.T) {
	env := newEnv(t)

	host := testutil.NewHost(t, env.app, "live-host")
	before := len(env.entries(t, schema.Hosts, host.Id))

	host.Set("status", schema.HostOnline)
	host.Set("rtt_ms", 12)
	host.Set("cpu_percent", 44.5)
	host.Set("last_seen", time.Now())
	if err := env.app.Save(host); err != nil {
		t.Fatal(err)
	}

	if after := len(env.entries(t, schema.Hosts, host.Id)); after != before {
		t.Fatalf("a heartbeat save produced %d entries", after-before)
	}
}

func TestUnauditedCollectionsAreSkipped(t *testing.T) {
	env := newEnv(t)

	host := testutil.NewHost(t, env.app, "skip-host")

	collection, _ := env.app.FindCollectionByNameOrId(schema.Events)
	event := core.NewRecord(collection)
	event.Set("host", host.Id)
	event.Set("kind", "test")
	if err := env.app.Save(event); err != nil {
		t.Fatal(err)
	}

	if entries := env.entries(t, schema.Events, event.Id); len(entries) != 0 {
		t.Fatalf("events must not be audited: %d", len(entries))
	}
}

// TestNoSecretInAuditLog goes over every existing collection with encrypted
// fields: creating, updating and deleting a record must not leak its secrets.
func TestNoSecretInAuditLog(t *testing.T) {
	env := newEnv(t)

	const secret = "SUPER-SECRET-VALUE-do-not-leak"

	host := testutil.NewHost(t, env.app, "secret-host")

	// a database user belongs to a server
	servers, err := env.app.FindCollectionByNameOrId(schema.DBServers)
	if err != nil {
		t.Fatal(err)
	}
	dbServer := core.NewRecord(servers)
	dbServer.Set("host", host.Id)
	dbServer.Set("name", "audit-server")
	dbServer.Set("engine", schema.EngineMariaDB)
	dbServer.Set("kind", schema.DBServerShared)
	if err := env.app.Save(dbServer); err != nil {
		t.Fatal(err)
	}

	// what a collection needs besides a host and a name
	required := map[string]map[string]any{
		schema.DBServers:    {"engine": schema.EngineMariaDB, "kind": schema.DBServerShared},
		schema.DBUsers:      {"server": dbServer.Id, "name": "with_secrets"},
		schema.Certificates: {"names": []string{"secret.example.com"}},
	}

	covered := 0
	for name, fields := range schema.Encrypted {
		collection, err := env.app.FindCollectionByNameOrId(name)
		if err != nil {
			continue // lands with a later milestone
		}
		covered++

		record := core.NewRecord(collection)
		if collection.Fields.GetByName("host") != nil {
			record.Set("host", host.Id)
		}
		if collection.Fields.GetByName("name") != nil {
			record.Set("name", "with-secrets")
		}
		for field, value := range required[name] {
			record.Set(field, value)
		}
		for _, field := range fields {
			record.Set(field, secret+"-create")
		}
		if err := env.app.Save(record); err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		for _, field := range fields {
			record.Set(field, secret+"-update")
		}
		if err := env.app.Save(record); err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		if !collection.System {
			if err := env.app.Delete(record); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
	}
	if covered == 0 {
		t.Fatal("no encrypted collection was tested")
	}

	// user passwords are not encrypted fields but must not leak either
	testutil.NewUser(t, env.app, "secret@example.com", schema.RoleViewer)

	dump := []string{}
	err = env.app.DB().NewQuery("SELECT coalesce(changes,'') || coalesce(snapshot,'') || note || record_label FROM audit_log").Column(&dump)
	if err != nil {
		t.Fatal(err)
	}
	if len(dump) == 0 {
		t.Fatal("the audit log is empty")
	}

	sawChanged := false
	for _, row := range dump {
		if strings.Contains(row, "SUPER-SECRET") || strings.Contains(row, "pcs1:") || strings.Contains(row, "correct horse") {
			t.Fatalf("a secret reached the audit log: %s", row)
		}
		if strings.Contains(row, `"secrets":"changed"`) {
			sawChanged = true
		}
	}
	if !sawChanged {
		t.Error("the entry must record that the secret changed")
	}
}

func TestAuditLogIsAppendOnly(t *testing.T) {
	env := newEnv(t)

	host := testutil.NewHost(t, env.app, "immutable-host")
	entry := env.last(t, schema.Hosts, host.Id)

	entry.Set("note", "tampered")
	if err := env.app.Save(entry); !errors.Is(err, audit.ErrImmutable) {
		t.Errorf("update: %v", err)
	}
	if err := env.app.Delete(entry); !errors.Is(err, audit.ErrImmutable) {
		t.Errorf("delete: %v", err)
	}

	collection, _ := env.app.FindCollectionByNameOrId(schema.AuditLog)
	forged := core.NewRecord(collection)
	forged.Set("seq", 99999)
	if err := env.app.Save(forged); !errors.Is(err, audit.ErrImmutable) {
		t.Errorf("create: %v", err)
	}

	// superusers are refused too
	_, token := testutil.NewSuperuser(t, env.app)
	base := "/api/collections/audit_log/records"

	for _, r := range []testutil.Request{
		{Method: "POST", URL: base, Token: token, Body: `{"seq":99999}`},
		{Method: "PATCH", URL: base + "/" + entry.Id, Token: token, Body: `{"note":"tampered"}`},
		{Method: "DELETE", URL: base + "/" + entry.Id, Token: token},
	} {
		if status, _ := testutil.Do(t, env.handler, r); status < 400 {
			t.Errorf("%s as a superuser: status %d", r.Method, status)
		}
	}

	if result, err := audit.Verify(env.app, env.panel.AuditKey()); err != nil || !result.OK {
		t.Fatalf("%+v, %v", result, err)
	}
}

// TestDirectEditsAreDetected is the third acceptance criterion.
func TestDirectEditsAreDetected(t *testing.T) {
	seed := func(t *testing.T) *env {
		env := newEnv(t)
		for i := range 5 {
			testutil.NewHost(t, env.app, fmt.Sprintf("host-%d", i))
		}
		result, err := audit.Verify(env.app, env.panel.AuditKey())
		if err != nil || !result.OK || result.Checked != 5 {
			t.Fatalf("clean chain: %+v, %v", result, err)
		}
		return env
	}

	exec := func(t *testing.T, env *env, query string) {
		t.Helper()
		if _, err := env.app.DB().NewQuery(query).Execute(); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name   string
		query  string
		badSeq int
	}{
		{"edited value", `UPDATE audit_log SET record_label = 'evil' WHERE seq = 3`, 3},
		{"edited diff", `UPDATE audit_log SET changes = '{"name":{"to":"evil"}}' WHERE seq = 2`, 2},
		{"edited actor", `UPDATE audit_log SET actor_label = 'nobody' WHERE seq = 1`, 1},
		{"edited hash", `UPDATE audit_log SET hash = 'abc' WHERE seq = 4`, 4},
		{"removed entry", `DELETE FROM audit_log WHERE seq = 2`, 2},
		{"removed last-but-one", `DELETE FROM audit_log WHERE seq = 4`, 4},
		{"removed newest entry", `DELETE FROM audit_log WHERE seq = 5`, 5},
		// a checkpoint that points past itself would start the check after every edit
		{"forged checkpoint", `UPDATE audit_log SET record_label = 'evil' WHERE seq = 3;
			UPDATE audit_log SET action = 'checkpoint', snapshot = '{"from_seq":6,"from_prev_hash":"x"}' WHERE seq = 5`, 5},
		// a checkpoint must not leave older entries behind unchecked
		{"checkpoint with older entries left", `UPDATE audit_log SET record_label = 'evil' WHERE seq = 1;
			UPDATE audit_log SET action = 'checkpoint',
				snapshot = json_object('from_seq', 3, 'from_prev_hash', (SELECT hash FROM audit_log WHERE seq = 2)) WHERE seq = 5`, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := seed(t)
			exec(t, env, tc.query)

			result, err := audit.Verify(env.app, env.panel.AuditKey())
			if err != nil {
				t.Fatal(err)
			}
			if result.OK || result.BadSeq != tc.badSeq {
				t.Fatalf("want a break at %d, got %+v", tc.badSeq, result)
			}
		})
	}

	t.Run("reported by the API with an alert", func(t *testing.T) {
		env := seed(t)
		exec(t, env, `UPDATE audit_log SET record_label = 'evil' WHERE seq = 3`)

		_, token := testutil.NewUser(t, env.app, "admin@example.com", schema.RoleAdmin)

		status, body := testutil.Do(t, env.handler, testutil.Request{Method: "POST", URL: "/api/pc/audit/verify", Token: token})
		if status != http.StatusOK || !strings.Contains(body, `"ok":false`) || !strings.Contains(body, `"bad_seq":3`) {
			t.Fatalf("%d %s", status, body)
		}

		alerts, err := env.app.FindRecordsByFilter(schema.Alerts, "rule='audit_integrity' && status='open'", "", 0, 0)
		if err != nil || len(alerts) != 1 {
			t.Fatalf("expected one open alert: %d, %v", len(alerts), err)
		}

		events, err := env.app.FindRecordsByFilter(schema.AuditLog, "action={:a}", "", 0, 0, map[string]any{"a": audit.ActionIntegrityBroken})
		if err != nil || len(events) != 1 {
			t.Fatalf("expected one security event: %d, %v", len(events), err)
		}

		// a second check does not raise a second alert
		testutil.Do(t, env.handler, testutil.Request{Method: "POST", URL: "/api/pc/audit/verify", Token: token})
		alerts, _ = env.app.FindRecordsByFilter(schema.Alerts, "rule='audit_integrity' && status='open'", "", 0, 0)
		if len(alerts) != 1 {
			t.Fatalf("alerts must not repeat: %d", len(alerts))
		}
	})
}

func TestChainStaysOrderedUnderConcurrentSaves(t *testing.T) {
	env := newEnv(t)

	var wg sync.WaitGroup
	errs := make(chan error, 40)

	collection, _ := env.app.FindCollectionByNameOrId(schema.Hosts)
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			host := core.NewRecord(collection)
			host.Set("name", fmt.Sprintf("concurrent-%d", i))
			if err := env.app.Save(host); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	result, err := audit.Verify(env.app, env.panel.AuditKey())
	if err != nil || !result.OK || result.Checked != 40 {
		t.Fatalf("%+v, %v", result, err)
	}
}

func TestFailedSaveLeavesNoEntry(t *testing.T) {
	env := newEnv(t)

	testutil.NewHost(t, env.app, "duplicate")

	before := []int{}
	if err := env.app.DB().NewQuery("SELECT count(*) FROM audit_log").Column(&before); err != nil {
		t.Fatal(err)
	}

	collection, _ := env.app.FindCollectionByNameOrId(schema.Hosts)
	host := core.NewRecord(collection)
	host.Set("name", "duplicate") // violates the unique index in the database
	if err := env.app.SaveNoValidate(host); err == nil {
		t.Fatal("expected the unique index to refuse the save")
	}

	after := []int{}
	if err := env.app.DB().NewQuery("SELECT count(*) FROM audit_log").Column(&after); err != nil {
		t.Fatal(err)
	}
	if before[0] != after[0] {
		t.Fatalf("a failed save left an entry: %d → %d", before[0], after[0])
	}
}

func TestRetentionKeepsTheChainVerifiable(t *testing.T) {
	env := newEnv(t)

	for i := range 5 {
		testutil.NewHost(t, env.app, fmt.Sprintf("old-%d", i))
	}

	// age the first three entries; the raw edit breaks their hash, but they are
	// about to be pruned, and the chain continues from the stored position
	old := time.Now().UTC().Add(-400 * 24 * time.Hour).Format("2006-01-02 15:04:05.000Z")
	if _, err := env.app.DB().NewQuery("UPDATE audit_log SET at = {:at} WHERE seq <= 3").Bind(map[string]any{"at": old}).Execute(); err != nil {
		t.Fatal(err)
	}

	removed, err := env.panel.Audit.Prune(365 * 24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Fatalf("removed %d entries, want 3", removed)
	}

	result, err := audit.Verify(env.app, env.panel.AuditKey())
	if err != nil || !result.OK {
		t.Fatalf("%+v, %v", result, err)
	}
	// seq 4 and 5 and the checkpoint itself
	if result.Checked != 3 {
		t.Fatalf("checked %d entries, want 3", result.Checked)
	}

	checkpoints, err := env.app.FindRecordsByFilter(schema.AuditLog, "action='checkpoint'", "", 0, 0)
	if err != nil || len(checkpoints) != 1 {
		t.Fatalf("expected one checkpoint: %d, %v", len(checkpoints), err)
	}

	// new entries keep chaining, and tampering after a checkpoint is still found
	testutil.NewHost(t, env.app, "after-checkpoint")
	if result, err := audit.Verify(env.app, env.panel.AuditKey()); err != nil || !result.OK {
		t.Fatalf("%+v, %v", result, err)
	}
	if _, err := env.app.DB().NewQuery("UPDATE audit_log SET record_label = 'evil' WHERE seq = 4").Execute(); err != nil {
		t.Fatal(err)
	}
	if result, err := audit.Verify(env.app, env.panel.AuditKey()); err != nil || result.OK || result.BadSeq != 4 {
		t.Fatalf("tampering after a checkpoint: %+v, %v", result, err)
	}
}

func TestSecurityEvents(t *testing.T) {
	env := newEnv(t)

	user, _ := testutil.NewUser(t, env.app, "login@example.com", schema.RoleViewer)
	url := "/api/collections/users/auth-with-password"

	status, body := testutil.Do(t, env.handler, testutil.Request{
		Method: "POST", URL: url, Headers: map[string]string{"User-Agent": "login-test"},
		Body: `{"identity":"login@example.com","password":"correct horse battery staple"}`,
	})
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}

	for _, bad := range []string{
		`{"identity":"login@example.com","password":"wrong-password-value"}`,
		`{"identity":"nobody@example.com","password":"wrong-password-value"}`,
	} {
		if status, _ := testutil.Do(t, env.handler, testutil.Request{Method: "POST", URL: url, Body: bad}); status != http.StatusBadRequest {
			t.Fatalf("a bad sign-in returned %d", status)
		}
	}

	signIns, err := env.app.FindRecordsByFilter(schema.AuditLog, "action={:a}", "", 0, 0, map[string]any{"a": audit.ActionSignIn})
	if err != nil || len(signIns) != 1 {
		t.Fatalf("sign-ins: %d, %v", len(signIns), err)
	}
	if signIns[0].GetString("category") != audit.CategorySecurity || signIns[0].GetString("actor") != user.Id {
		t.Errorf("sign-in entry: %v", signIns[0].FieldsData())
	}

	failures, err := env.app.FindRecordsByFilter(schema.AuditLog, "action={:a}", "seq", 0, 0, map[string]any{"a": audit.ActionSignInFailed})
	if err != nil || len(failures) != 2 {
		t.Fatalf("failures: %d, %v", len(failures), err)
	}
	if failures[1].GetString("record_label") != "nobody@example.com" {
		t.Errorf("failed identity: %q", failures[1].GetString("record_label"))
	}

	// no password anywhere
	dump := []string{}
	if err := env.app.DB().NewQuery("SELECT coalesce(snapshot,'') || coalesce(changes,'') FROM audit_log").Column(&dump); err != nil {
		t.Fatal(err)
	}
	for _, row := range dump {
		if strings.Contains(row, "wrong-password") || strings.Contains(row, "correct horse") {
			t.Fatalf("a password reached the audit log: %s", row)
		}
	}
}

func TestSchemaChangesWithoutARequestAreMigrations(t *testing.T) {
	env := newEnv(t)

	// the very first entry of a new panel is the creation of the log itself
	first, err := env.app.FindFirstRecordByData(schema.AuditLog, "seq", 1)
	if err != nil {
		// this test app applied its migrations before the writer was registered
		collection := core.NewBaseCollection("from_a_migration")
		if err := env.app.Save(collection); err != nil {
			t.Fatal(err)
		}
		first = env.last(t, "_collections", collection.Id)
	}

	assertActor(t, first, audit.KindSystem, "", audit.SourceMigration)
	if first.GetString("actor_label") != "migration" {
		t.Errorf("actor_label = %q", first.GetString("actor_label"))
	}
}

func TestPocketBaseSettingsAndSchemaAreAudited(t *testing.T) {
	env := newEnv(t)

	superuser, token := testutil.NewSuperuser(t, env.app)
	headers := map[string]string{"Referer": "http://panel.test/_/#/settings"}

	status, body := testutil.Do(t, env.handler, testutil.Request{
		Method: "PATCH", URL: "/api/settings", Token: token, Headers: headers,
		Body: `{"meta":{"appName":"Renamed"},"smtp":{"enabled":false,"password":"SMTP-SECRET-VALUE"}}`,
	})
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}

	entry := env.last(t, "_params", "settings")
	assertActor(t, entry, audit.KindSuperuser, superuser.Id, audit.SourcePBAdmin)
	changes := changesOf(t, entry)
	if diff, ok := changes["meta.appName"].(map[string]any); !ok || diff["to"] != "Renamed" {
		t.Errorf("settings diff: %v", changes)
	}
	if strings.Contains(entry.GetString("changes"), "SMTP-SECRET") {
		t.Fatal("the SMTP password reached the audit log")
	}

	// a schema change through the dashboard
	status, body = testutil.Do(t, env.handler, testutil.Request{
		Method: "POST", URL: "/api/collections", Token: token, Headers: headers,
		Body: `{"name":"custom_things","type":"base","fields":[{"name":"title","type":"text"}]}`,
	})
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}
	id := recordID(t, body)

	entry = env.last(t, "_collections", id)
	assertActor(t, entry, audit.KindSuperuser, superuser.Id, audit.SourcePBAdmin)
	if entry.GetString("action") != audit.ActionCreate || entry.GetString("record_label") != "custom_things" {
		t.Errorf("collection create entry: %v", entry.FieldsData())
	}

	status, body = testutil.Do(t, env.handler, testutil.Request{
		Method: "PATCH", URL: "/api/collections/" + id, Token: token, Headers: headers,
		Body: `{"listRule":"@request.auth.id != ''"}`,
	})
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}
	entry = env.last(t, "_collections", id)
	if entry.GetString("action") != audit.ActionUpdate {
		t.Errorf("collection update entry: %v", entry.FieldsData())
	}
	if diff, ok := changesOf(t, entry)["listRule"].(map[string]any); !ok || diff["to"] != "@request.auth.id != ''" {
		t.Errorf("collection update diff: %v", changesOf(t, entry))
	}

	status, _ = testutil.Do(t, env.handler, testutil.Request{Method: "DELETE", URL: "/api/collections/" + id, Token: token, Headers: headers})
	if status != http.StatusNoContent {
		t.Fatalf("delete status %d", status)
	}
	if entry = env.last(t, "_collections", id); entry.GetString("action") != audit.ActionDelete {
		t.Errorf("collection delete entry: %v", entry.FieldsData())
	}

	if result, err := audit.Verify(env.app, env.panel.AuditKey()); err != nil || !result.OK {
		t.Fatalf("%+v, %v", result, err)
	}
}

func TestRevert(t *testing.T) {
	env := newEnv(t)

	_, adminToken := testutil.NewUser(t, env.app, "admin@example.com", schema.RoleAdmin)
	_, operatorToken := testutil.NewUser(t, env.app, "operator@example.com", schema.RoleOperator)

	host := testutil.NewHost(t, env.app, "revert-host")
	host.Set("labels", map[string]any{"env": "prod"})
	if err := env.app.Save(host); err != nil {
		t.Fatal(err)
	}
	host.Set("roles", []string{schema.HostRoleEdge})
	if err := env.app.Save(host); err != nil {
		t.Fatal(err)
	}

	entry := env.last(t, schema.Hosts, host.Id)
	revert := "/api/pc/audit/" + entry.Id + "/revert"

	// only admins can revert
	if status, _ := testutil.Do(t, env.handler, testutil.Request{Method: "POST", URL: revert, Token: operatorToken}); status != http.StatusForbidden {
		t.Fatalf("an operator reverted a change: %d", status)
	}

	if status, body := testutil.Do(t, env.handler, testutil.Request{Method: "POST", URL: revert, Token: adminToken}); status != http.StatusNoContent {
		t.Fatalf("%d %s", status, body)
	}

	fresh, err := env.app.FindRecordById(schema.Hosts, host.Id)
	if err != nil {
		t.Fatal(err)
	}
	if roles := fresh.GetStringSlice("roles"); len(roles) != 1 || roles[0] != schema.HostRoleDocker {
		t.Errorf("roles after revert: %v", roles)
	}

	// the revert is itself a new audited change
	last := env.last(t, schema.Hosts, host.Id)
	if last.Id == entry.Id || last.GetString("actor_kind") != audit.KindUser || !strings.Contains(last.GetString("note"), entry.Id) {
		t.Errorf("revert entry: %v", last.FieldsData())
	}

	// reverting a delete brings the record back with its id
	if err := env.app.Delete(fresh); err != nil {
		t.Fatal(err)
	}
	deleted := env.last(t, schema.Hosts, host.Id)
	if status, body := testutil.Do(t, env.handler, testutil.Request{Method: "POST", URL: "/api/pc/audit/" + deleted.Id + "/revert", Token: adminToken}); status != http.StatusNoContent {
		t.Fatalf("%d %s", status, body)
	}
	if _, err := env.app.FindRecordById(schema.Hosts, host.Id); err != nil {
		t.Fatalf("the deleted record did not come back: %v", err)
	}

	// security events cannot be reverted
	env.panel.Audit.Security(audit.ActionEnrollment, audit.System("gateway", audit.SourcePanel), "x", nil)
	events, _ := env.app.FindRecordsByFilter(schema.AuditLog, "category='security'", "", 1, 0)
	if status, _ := testutil.Do(t, env.handler, testutil.Request{Method: "POST", URL: "/api/pc/audit/" + events[0].Id + "/revert", Token: adminToken}); status != http.StatusBadRequest {
		t.Errorf("reverting a security event: %d", status)
	}
}

func TestExport(t *testing.T) {
	env := newEnv(t)

	_, adminToken := testutil.NewUser(t, env.app, "admin@example.com", schema.RoleAdmin)
	_, operatorToken := testutil.NewUser(t, env.app, "operator@example.com", schema.RoleOperator)
	testutil.NewHost(t, env.app, "export-host")

	if status, _ := testutil.Do(t, env.handler, testutil.Request{Method: "GET", URL: "/api/pc/audit/export", Token: operatorToken}); status != http.StatusForbidden {
		t.Fatalf("an operator exported the log: %d", status)
	}

	status, body := testutil.Do(t, env.handler, testutil.Request{Method: "GET", URL: "/api/pc/audit/export", Token: adminToken})
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}

	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) < 2 {
		t.Fatalf("too few lines: %q", body)
	}

	for _, line := range lines {
		entry := map[string]any{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		if entry["hash"] == "" || entry["hash"] == nil {
			t.Fatalf("no hash in %q", line)
		}
	}

	status, body = testutil.Do(t, env.handler, testutil.Request{Method: "GET", URL: "/api/pc/audit/export?filter=" + "collection%3D%27hosts%27", Token: adminToken})
	if status != http.StatusOK || strings.Count(body, "\n") != 1 {
		t.Fatalf("filtered export: %d %q", status, body)
	}
}

func TestOperatorsSeeChangesButNotSecurityEvents(t *testing.T) {
	env := newEnv(t)

	_, operatorToken := testutil.NewUser(t, env.app, "operator@example.com", schema.RoleOperator)
	_, viewerToken := testutil.NewUser(t, env.app, "viewer@example.com", schema.RoleViewer)
	_, adminToken := testutil.NewUser(t, env.app, "admin@example.com", schema.RoleAdmin)
	testutil.NewHost(t, env.app, "visible-host")
	env.panel.Audit.Security(audit.ActionSignIn, audit.System("t", audit.SourcePanel), "x", nil)

	base := "/api/collections/audit_log/records"

	_, body := testutil.Do(t, env.handler, testutil.Request{Method: "GET", URL: base + "?filter=category%3D%27security%27", Token: operatorToken})
	if !strings.Contains(body, `"totalItems":0`) {
		t.Errorf("an operator saw security events: %.200s", body)
	}
	_, body = testutil.Do(t, env.handler, testutil.Request{Method: "GET", URL: base + "?filter=category%3D%27change%27", Token: operatorToken})
	if strings.Contains(body, `"totalItems":0`) {
		t.Errorf("an operator must see changes: %.200s", body)
	}
	_, body = testutil.Do(t, env.handler, testutil.Request{Method: "GET", URL: base, Token: viewerToken})
	if !strings.Contains(body, `"totalItems":0`) {
		t.Errorf("a viewer has no access: %.200s", body)
	}
	_, body = testutil.Do(t, env.handler, testutil.Request{Method: "GET", URL: base + "?filter=category%3D%27security%27", Token: adminToken})
	if strings.Contains(body, `"totalItems":0`) {
		t.Errorf("an admin must see security events: %.200s", body)
	}
}

// BenchmarkSave measures the overhead of the audit writer per save.
// The numbers are in docs/architecture.md, section "Change audit".
func BenchmarkSave(b *testing.B) {
	run := func(b *testing.B, audited bool) {
		app := testutil.NewApp(b)
		b.Cleanup(app.Cleanup)

		if audited {
			writer := audit.New(app, schema.Audited)
			key := make([]byte, 32)
			writer.SealKey = func() []byte { return key }
			writer.Register()
		}

		collection, err := app.FindCollectionByNameOrId(schema.Hosts)
		if err != nil {
			b.Fatal(err)
		}

		host := core.NewRecord(collection)
		host.Set("name", "bench")
		if err := app.Save(host); err != nil {
			b.Fatal(err)
		}

		b.ResetTimer()
		for i := 0; b.Loop(); i++ {
			host.Set("labels", map[string]any{"i": i})
			if err := app.Save(host); err != nil {
				b.Fatal(err)
			}
		}
	}

	b.Run("without-audit", func(b *testing.B) { run(b, false) })
	b.Run("with-audit", func(b *testing.B) { run(b, true) })
}

// TestTheSealCatchesARewrittenChain: whoever knows the scheme can edit an entry
// and recompute every hash after it, the head's too. Only the seal, made with a
// key derived from the master key, shows it.
func TestTheSealCatchesARewrittenChain(t *testing.T) {
	env := newEnv(t)
	for i := range 4 {
		testutil.NewHost(t, env.app, fmt.Sprintf("host-%d", i))
	}
	key := env.panel.AuditKey()

	result, err := audit.Verify(env.app, key)
	if err != nil || !result.OK || !result.Sealed || result.Unsealed != 0 {
		t.Fatalf("clean chain: %+v, %v", result, err)
	}

	if _, err := env.app.DB().NewQuery(`UPDATE audit_log SET record_label = 'evil' WHERE seq = 2`).Execute(); err != nil {
		t.Fatal(err)
	}
	if err := audit.Rehash(env.app, 2); err != nil {
		t.Fatal(err)
	}

	// the hashes match again, so without the key the edit is invisible
	if result, err := audit.Verify(env.app, nil); err != nil || !result.OK {
		t.Fatalf("rewritten chain without the key: %+v, %v", result, err)
	}
	result, err = audit.Verify(env.app, key)
	if err != nil || result.OK || result.BadSeq != 4 {
		t.Fatalf("rewritten chain with the key: want a break at 4, got %+v, %v", result, err)
	}
}

func TestAMissingHeadIsReported(t *testing.T) {
	env := newEnv(t)
	testutil.NewHost(t, env.app, "host")

	if _, err := env.app.DB().NewQuery(`DELETE FROM ` + audit.HeadTable).Execute(); err != nil {
		t.Fatal(err)
	}
	for _, key := range [][]byte{nil, env.panel.AuditKey()} {
		result, err := audit.Verify(env.app, key)
		if err != nil || result.OK || result.Reason != "the head of the chain is missing" {
			t.Errorf("with key %v: %+v, %v", key != nil, result, err)
		}
	}
}

// TestEntriesWrittenWithoutTheKeyAreSealedLater: a command without the master
// key, such as a migration, still writes entries; the panel seals them when it
// starts with the key, but a change to what was sealed before is still found.
func TestEntriesWrittenWithoutTheKeyAreSealedLater(t *testing.T) {
	env := newEnv(t)
	testutil.NewHost(t, env.app, "sealed")
	key := env.panel.AuditKey()

	seal := env.panel.Audit.SealKey
	env.panel.Audit.SealKey = func() []byte { return nil }
	testutil.NewHost(t, env.app, "unsealed-1")
	testutil.NewHost(t, env.app, "unsealed-2")
	env.panel.Audit.SealKey = seal

	result, err := audit.Verify(env.app, key)
	if err != nil || !result.OK || !result.Sealed || result.Unsealed != 2 {
		t.Fatalf("before sealing: %+v, %v", result, err)
	}

	if err := env.panel.Audit.SealHead(); err != nil {
		t.Fatal(err)
	}
	result, err = audit.Verify(env.app, key)
	if err != nil || !result.OK || !result.Sealed || result.Unsealed != 0 {
		t.Fatalf("after sealing: %+v, %v", result, err)
	}

	t.Run("a change to the sealed part", func(t *testing.T) {
		env := newEnv(t)
		testutil.NewHost(t, env.app, "sealed")
		key := env.panel.AuditKey()

		env.panel.Audit.SealKey = func() []byte { return nil }
		testutil.NewHost(t, env.app, "unsealed")

		// the sealed entry is rewritten, and every hash recomputed
		if _, err := env.app.DB().NewQuery(`UPDATE audit_log SET record_label = 'evil' WHERE seq = 1`).Execute(); err != nil {
			t.Fatal(err)
		}
		if err := audit.Rehash(env.app, 1); err != nil {
			t.Fatal(err)
		}

		result, err := audit.Verify(env.app, key)
		if err != nil || result.OK || result.BadSeq != 1 {
			t.Fatalf("want a break at 1, got %+v, %v", result, err)
		}

		// and sealing does not cover it up
		env.panel.Audit.SealKey = func() []byte { return key }
		if err := env.panel.Audit.SealHead(); err != nil {
			t.Fatal(err)
		}
		testutil.NewHost(t, env.app, "after")
		if result, _ := audit.Verify(env.app, key); result.OK {
			t.Fatalf("a later seal covered up the change: %+v", result)
		}
	})
}

// TestAFreshInstallWritesItsMigrationsToTheLog: the panel binds the audit before
// the migrations run, as `panel serve` does, so the first migrations write
// entries before the table of the head exists.
func TestAFreshInstallWritesItsMigrationsToTheLog(t *testing.T) {
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: t.TempDir(), EncryptionEnv: "pb_test_env"})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.ClearBootstrap() }()

	p := panel.New(app, &panel.Config{MasterKey: testutil.MasterKey, GRPCAddr: "127.0.0.1:0"}, nil)
	p.Register()
	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("the migrations of a fresh install fail: %v", err)
	}
	if err := p.Audit.SealHead(); err != nil {
		t.Fatal(err)
	}

	result, err := audit.Verify(app, p.AuditKey())
	if err != nil || !result.OK || !result.Sealed || result.Checked == 0 || result.Unsealed != 0 {
		t.Fatalf("fresh install: %+v, %v", result, err)
	}
}

// TestARemovedSealIsReported: whoever blanks the seal makes the head look like
// one that was never sealed. Only the process that created a head trusts that.
func TestARemovedSealIsReported(t *testing.T) {
	strip := `UPDATE ` + audit.HeadTable + ` SET mac = '', sealed_seq = 0, sealed_hash = '', sealed_mac = ''`

	cases := map[string]func(t *testing.T, env *env){
		"rewritten entry": func(t *testing.T, env *env) {
			exec(t, env, `UPDATE audit_log SET record_label = 'evil' WHERE seq = 2`)
			if err := audit.Rehash(env.app, 2); err != nil {
				t.Fatal(err)
			}
		},
		"removed newest entries": func(t *testing.T, env *env) {
			exec(t, env, `DELETE FROM audit_log WHERE seq >= 3`)
			if err := audit.Rehash(env.app, 3); err != nil {
				t.Fatal(err)
			}
		},
		"nothing else": func(*testing.T, *env) {},
	}

	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			env := newEnv(t)
			for i := range 4 {
				testutil.NewHost(t, env.app, fmt.Sprintf("host-%d", i))
			}
			key := env.panel.AuditKey()

			change(t, env)
			exec(t, env, strip)

			result, err := audit.Verify(env.app, key)
			if err != nil || result.OK || !strings.Contains(result.Reason, "no seal") {
				t.Fatalf("a stripped seal passes: %+v, %v", result, err)
			}

			// a start of the panel does not seal over it
			if err := env.panel.Audit.SealHead(); err != nil {
				t.Fatal(err)
			}
			testutil.NewHost(t, env.app, "later")
			if result, _ := audit.Verify(env.app, key); result.OK {
				t.Fatalf("a later seal covered up a stripped seal: %+v", result)
			}
		})
	}
}

// TestEntriesBeforeTheStartAreReported: without a checkpoint the chain starts at
// entry 1, and rows with a lower seq would never be checked.
func TestEntriesBeforeTheStartAreReported(t *testing.T) {
	env := newEnv(t)
	testutil.NewHost(t, env.app, "host")

	exec(t, env, `INSERT INTO audit_log (id, seq, at, category, action, actor_label, prev_hash, hash)
		VALUES ('forged0000000a', 0, '2026-01-01 00:00:00.000Z', 'change', 'delete', 'alice', '', 'x')`)

	result, err := audit.Verify(env.app, env.panel.AuditKey())
	if err != nil || result.OK || result.BadSeq != 0 {
		t.Fatalf("an entry before the start passes: %+v, %v", result, err)
	}
}

// TestVerifyIsNotFooledByWritesUnderWay: a save that commits while the check
// runs must not look like a missing entry.
func TestVerifyIsNotFooledByWritesUnderWay(t *testing.T) {
	env := newEnv(t)
	host := testutil.NewHost(t, env.app, "busy")
	key := env.panel.AuditKey()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 300 {
			host.Set("labels", map[string]any{"i": i})
			if err := env.app.Save(host); err != nil {
				t.Error(err)
				return
			}
		}
	}()

	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		result, err := audit.Verify(env.app, key)
		if err != nil || !result.OK {
			t.Fatalf("a check during writes: %+v, %v", result, err)
		}
	}
}

// TestEntriesWithoutTheKeyStayListed: entries passed off as written without the
// key are sealed over later, but the seal lists them, and so does the check.
func TestEntriesWithoutTheKeyStayListed(t *testing.T) {
	env := newEnv(t)
	for i := range 3 {
		testutil.NewHost(t, env.app, fmt.Sprintf("host-%d", i))
	}
	key := env.panel.AuditKey()

	seal := env.panel.Audit.SealKey
	env.panel.Audit.SealKey = func() []byte { return nil }
	testutil.NewHost(t, env.app, "keyless-1")
	testutil.NewHost(t, env.app, "keyless-2")
	env.panel.Audit.SealKey = seal
	testutil.NewHost(t, env.app, "keyed")

	result, err := audit.Verify(env.app, key)
	if err != nil || !result.OK || len(result.Keyless) != 1 || result.Keyless[0] != [2]int{4, 5} {
		t.Fatalf("keyless entries: %+v, %v", result, err)
	}

	if _, err := env.panel.Audit.Prune(time.Hour); err != nil {
		t.Fatal(err)
	}
	env.panel.Audit.SealKey = func() []byte { return nil }
	if _, err := env.panel.Audit.Prune(time.Hour); err == nil {
		t.Error("the log was pruned without the master key")
	}
}

func exec(t *testing.T, env *env, query string) {
	t.Helper()
	if _, err := env.app.DB().NewQuery(query).Execute(); err != nil {
		t.Fatal(err)
	}
}
