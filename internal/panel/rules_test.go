package panel

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// roleSet says which roles may do something: anonymous, viewer, operator, admin.
type roleSet struct{ anon, viewer, operator, admin bool }

var (
	everyone   = roleSet{false, true, true, true} // every signed-in role
	operators  = roleSet{false, false, true, true}
	adminsOnly = roleSet{false, false, false, true}
	nobody     = roleSet{}
)

func (r roleSet) allows(role string) bool {
	switch role {
	case "":
		return r.anon
	case schema.RoleViewer:
		return r.viewer
	case schema.RoleOperator:
		return r.operator
	case schema.RoleAdmin:
		return r.admin
	}
	return false
}

// TestAPIRules covers the permission matrix of the collections' API rules.
func TestAPIRules(t *testing.T) {
	_, app, handler := newTestPanel(t)

	tokens := map[string]string{"": ""}
	for _, role := range []string{schema.RoleViewer, schema.RoleOperator, schema.RoleAdmin} {
		_, token := testutil.NewUser(t, app, role+"@example.com", role)
		tokens[role] = token
	}

	host := testutil.NewHost(t, app, "rules-host")
	stack := saveRecord(t, app, schema.Stacks, map[string]any{"host": host.Id, "name": "rules-stack"})
	settings, err := app.FindRecordsByFilter(schema.Settings, "", "", 1, 0)
	if err != nil {
		t.Fatal(err)
	}

	records := map[string]*core.Record{
		schema.Hosts:             host,
		schema.RevokedCerts:      saveRecord(t, app, schema.RevokedCerts, map[string]any{"serial": "abc"}),
		schema.EnrollmentTokens:  saveRecord(t, app, schema.EnrollmentTokens, map[string]any{"token_hash": "x", "name": "n", "expires_at": "2030-01-01 00:00:00.000Z"}),
		schema.Commands:          saveRecord(t, app, schema.Commands, map[string]any{"type": "agent.restart", "host": host.Id, "status": schema.CommandSucceeded}),
		schema.Events:            saveRecord(t, app, schema.Events, map[string]any{"kind": "test", "host": host.Id}),
		schema.Alerts:            saveRecord(t, app, schema.Alerts, map[string]any{"rule": "r", "target": "t", "host": host.Id}),
		schema.Stacks:            stack,
		schema.StackRevisions:    saveRecord(t, app, schema.StackRevisions, map[string]any{"stack": stack.Id, "number": 1}),
		schema.Containers:        saveRecord(t, app, schema.Containers, map[string]any{"host": host.Id, "docker_id": "c1"}),
		schema.Volumes:           saveRecord(t, app, schema.Volumes, map[string]any{"host": host.Id, "name": "v1"}),
		schema.Networks:          saveRecord(t, app, schema.Networks, map[string]any{"host": host.Id, "name": "n1"}),
		schema.Settings:          settings[0],
		schema.DiagnosticBundles: saveRecord(t, app, schema.DiagnosticBundles, map[string]any{"host": host.Id}),
		schema.InternalCA:        saveRecord(t, app, schema.InternalCA, map[string]any{"root_cert": "x"}),
	}

	// bodies that would be valid, so a create can only fail because of the rule
	bodies := map[string]string{
		schema.Commands: fmt.Sprintf(`{"type":"container.start","host":%q,"payload":{"docker_id":"x"}}`, host.Id),
		schema.Stacks:   fmt.Sprintf(`{"host":%q,"name":"created-%%s"}`, host.Id),
	}

	matrix := []struct {
		collection                string
		read, create, update, del roleSet
	}{
		{schema.Hosts, everyone, nobody, adminsOnly, adminsOnly},
		{schema.RevokedCerts, adminsOnly, nobody, nobody, nobody},
		{schema.EnrollmentTokens, adminsOnly, nobody, nobody, adminsOnly},
		{schema.Commands, everyone, operators, nobody, nobody},
		{schema.Events, everyone, nobody, nobody, nobody},
		{schema.Alerts, everyone, nobody, nobody, nobody},
		{schema.Stacks, everyone, operators, operators, operators},
		{schema.StackRevisions, everyone, nobody, nobody, nobody},
		{schema.Containers, everyone, nobody, nobody, nobody},
		{schema.Volumes, everyone, nobody, nobody, nobody},
		{schema.Networks, everyone, nobody, nobody, nobody},
		{schema.Settings, everyone, nobody, adminsOnly, nobody},
		{schema.DiagnosticBundles, adminsOnly, nobody, nobody, adminsOnly},
		{schema.InternalCA, nobody, nobody, nobody, nobody},
	}

	for _, row := range matrix {
		record := records[row.collection]
		base := "/api/collections/" + row.collection + "/records"

		for role, token := range tokens {
			t.Run(row.collection+"/"+roleName(role), func(t *testing.T) {
				// a list with a failing rule answers 200 with no items, a view 404
				status, body := testutil.Do(t, handler, testutil.Request{Method: "GET", URL: base, Token: token})
				got := status == http.StatusOK && !strings.Contains(body, `"totalItems":0`)
				if got != row.read.allows(role) {
					t.Errorf("list: status %d, body %.120s", status, body)
				}

				status, _ = testutil.Do(t, handler, testutil.Request{Method: "GET", URL: base + "/" + record.Id, Token: token})
				if (status == http.StatusOK) != row.read.allows(role) {
					t.Errorf("view: status %d", status)
				}

				body = bodies[row.collection]
				if strings.Contains(body, "%s") {
					body = fmt.Sprintf(body, roleName(role))
				}
				if body == "" {
					body = "{}"
				}
				status, response := testutil.Do(t, handler, testutil.Request{Method: "POST", URL: base, Token: token, Body: body})
				if allowed := row.create.allows(role); allowed != (status == http.StatusOK) {
					t.Errorf("create: status %d, body %.200s", status, response)
				}

				// an empty update changes nothing; only the rule can refuse it
				status, _ = testutil.Do(t, handler, testutil.Request{Method: "PATCH", URL: base + "/" + record.Id, Token: token, Body: "{}"})
				if (status == http.StatusOK) != row.update.allows(role) {
					t.Errorf("update: status %d", status)
				}
			})
		}

		// deleting last, on a throwaway record of the collections that allow it
		if row.del != nobody {
			for role, token := range tokens {
				target := saveRecord(t, app, row.collection, map[string]any{
					"host": host.Id, "name": "del-" + roleName(role),
					"token_hash": "hash-" + roleName(role), "expires_at": "2030-01-01 00:00:00.000Z",
				})
				status, _ := testutil.Do(t, handler, testutil.Request{Method: "DELETE", URL: base + "/" + target.Id, Token: token})
				if (status == http.StatusNoContent) != row.del.allows(role) {
					t.Errorf("%s delete as %s: status %d", row.collection, roleName(role), status)
				}
			}
		} else {
			for role, token := range tokens {
				status, _ := testutil.Do(t, handler, testutil.Request{Method: "DELETE", URL: base + "/" + record.Id, Token: token})
				if status == http.StatusNoContent {
					t.Errorf("%s delete as %s must be refused", row.collection, roleName(role))
				}
			}
		}
	}
}

func TestUsersRules(t *testing.T) {
	_, app, handler := newTestPanel(t)

	viewer, viewerToken := testutil.NewUser(t, app, "viewer@example.com", schema.RoleViewer)
	operator, _ := testutil.NewUser(t, app, "operator@example.com", schema.RoleOperator)
	_, adminToken := testutil.NewUser(t, app, "admin@example.com", schema.RoleAdmin)

	base := "/api/collections/users/records"
	signUp := `{"email":"new@example.com","password":"correct horse battery","passwordConfirm":"correct horse battery","role":"viewer"}`

	// there is no public sign-up
	if status, _ := testutil.Do(t, handler, testutil.Request{Method: "POST", URL: base, Body: signUp}); status == http.StatusOK {
		t.Error("anonymous sign-up must be refused")
	}
	if status, _ := testutil.Do(t, handler, testutil.Request{Method: "POST", URL: base, Token: viewerToken, Body: signUp}); status == http.StatusOK {
		t.Error("a viewer must not create users")
	}
	if status, body := testutil.Do(t, handler, testutil.Request{Method: "POST", URL: base, Token: adminToken, Body: signUp}); status != http.StatusOK {
		t.Errorf("an admin must create users: %d %s", status, body)
	}

	// nobody can raise their own role
	if status, _ := testutil.Do(t, handler, testutil.Request{Method: "PATCH", URL: base + "/" + viewer.Id, Token: viewerToken, Body: `{"role":"admin"}`}); status == http.StatusOK {
		t.Error("a viewer must not change their own role")
	}
	if status, body := testutil.Do(t, handler, testutil.Request{Method: "PATCH", URL: base + "/" + viewer.Id, Token: viewerToken, Body: `{"name":"V"}`}); status != http.StatusOK {
		t.Errorf("a viewer may change their own name: %d %s", status, body)
	}
	if status, _ := testutil.Do(t, handler, testutil.Request{Method: "PATCH", URL: base + "/" + operator.Id, Token: viewerToken, Body: `{"name":"X"}`}); status == http.StatusOK {
		t.Error("a viewer must not change someone else")
	}
	if status, body := testutil.Do(t, handler, testutil.Request{Method: "PATCH", URL: base + "/" + viewer.Id, Token: adminToken, Body: `{"role":"operator"}`}); status != http.StatusOK {
		t.Errorf("an admin may change a role: %d %s", status, body)
	}

	// users only see themselves, admins see everyone
	_, body := testutil.Do(t, handler, testutil.Request{Method: "GET", URL: base, Token: viewerToken})
	if !strings.Contains(body, `"totalItems":1`) {
		t.Errorf("a viewer must see only themselves: %.200s", body)
	}
	_, body = testutil.Do(t, handler, testutil.Request{Method: "GET", URL: base, Token: adminToken})
	if !strings.Contains(body, `"totalItems":4`) {
		t.Errorf("an admin must see everyone: %.200s", body)
	}
}

func roleName(role string) string {
	if role == "" {
		return "anonymous"
	}
	return role
}

func saveRecord(t *testing.T, app core.App, name string, fields map[string]any) *core.Record {
	t.Helper()

	collection, err := app.FindCollectionByNameOrId(name)
	if err != nil {
		t.Fatal(err)
	}

	record := core.NewRecord(collection)
	for key, value := range fields {
		if collection.Fields.GetByName(key) != nil {
			record.Set(key, value)
		}
	}
	if err := app.Save(record); err != nil {
		t.Fatalf("%s: %v", name, err)
	}

	return record
}
