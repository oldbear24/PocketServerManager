package panel

import (
	"strings"
	"testing"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// TestNoDatabasePasswordIsKept (#53, P10, AC1): a database's passwords, its
// server administrator's and its user's, reach an agent only in the commands
// it is delivered: after a restore back into a database, one into a new
// database and a restore test, whose agents echo every password in their
// previews, errors and logs, no stored payload, no restore or test row, no
// event, alert or audit entry holds one.
func TestNoDatabasePasswordIsKept(t *testing.T) {
	f := newDBFixture(t, databases.Postgres.Name)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	stream := f.dbAgent()
	secrets := []string{adminSecretPG, userSecretShop}
	// an agent can only echo what it was delivered: the administrator's
	// password and the restore's user's
	echo := func(user string) string { return " (" + adminSecretPG + ", " + user + ")" }

	// back into the database
	status, body := f.send("POST", "/api/pc/backup-jobs/"+f.job.Id+"/restores", operator,
		map[string]any{"target": f.target.Id, "snapshot": snapshotID, "mode": schema.RestoreDatabase})
	if status != 202 {
		t.Fatalf("the draft: %d %v", status, body)
	}
	preview := f.receivedOf(stream, "backup.restore")
	if preview.GetBackupRestore().GetDatabase().GetPassword() != userSecretShop {
		t.Fatalf("the preview went without the user's password: %v", preview)
	}
	answer := dbPreview(t, "shop.pgdump", true, 1<<30, 1<<20)
	answer["notes"] = []any{"signed in as shop" + echo(userSecretShop)}
	f.answer(f.host, preview.GetId(), agentv1.CommandState_COMMAND_STATE_SUCCEEDED, answer, "")
	restore := f.stored(schema.BackupRestores, body["id"].(string))
	if status, body := f.startDatabase(restore, operator, true, "shop"); status != 200 {
		t.Fatalf("started: %d %v", status, body)
	}
	delivered := f.receivedOf(stream, "backup.restore")
	f.answer(f.host, delivered.GetId(), agentv1.CommandState_COMMAND_STATE_FAILED,
		map[string]any{"files_restored": 0, "log": "pg_restore as shop" + echo(userSecretShop)}, "Nothing changed in shop: password authentication failed"+echo(userSecretShop))

	// into a new database
	fresh := dbPreview(t, "shop.pgdump", false, 1<<30, 1<<20)
	fresh["database"].(map[string]any)["exists"] = false
	fresh["database"].(map[string]any)["user_exists"] = false
	copied := f.draft(stream, operator, map[string]any{"mode": schema.RestoreNewDatabase, "name": "shop_copy"}, fresh)
	if status, body := f.startDatabase(copied, operator, false, ""); status != 200 {
		t.Fatalf("started a new database: %d %v", status, body)
	}
	delivered = f.receivedOf(stream, "backup.restore")
	created := delivered.GetBackupRestore().GetDatabase().GetPassword()
	secrets = append(secrets, created)
	f.answer(f.host, delivered.GetId(), agentv1.CommandState_COMMAND_STATE_FAILED,
		map[string]any{"dropped": false, "log": "created" + echo(created)}, "pg_restore failed"+echo(created))

	// a restore test
	_, command := f.startedTest(f.job, schema.RunSchedule)
	test := f.receivedOf(stream, "backup.restore_test")
	if test.GetId() != command.Id || test.GetBackupRestoreTest().GetDatabase().GetPassword() != userSecretShop {
		t.Fatalf("the test went without the user's password: %v", test)
	}
	f.answer(f.host, test.GetId(), agentv1.CommandState_COMMAND_STATE_FAILED,
		testAnswer(t, backup.RestoreTestResult{Failure: backup.TestRestore, Created: true, Log: "restoring" + echo(userSecretShop)}), "the client failed"+echo(userSecretShop))

	for _, secret := range secrets {
		if len(secret) < 8 {
			t.Fatalf("a password too short to look for: %q", secret)
		}
	}
	scan := func(collection string, fields ...string) {
		t.Helper()
		records, err := f.app.FindAllRecords(collection)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range records {
			texts := []string{}
			if len(fields) == 0 {
				texts = append(texts, mustString(t, record.FieldsData()))
			}
			for _, field := range fields {
				texts = append(texts, mustString(t, record.Get(field)))
			}
			for _, text := range texts {
				for _, secret := range secrets {
					if strings.Contains(text, secret) {
						t.Errorf("%s %s holds a password: %s", collection, record.Id, text)
					}
				}
			}
		}
	}
	scan(schema.Commands, "payload", "label")
	scan(schema.BackupRestores)
	scan(schema.BackupChecks)
	scan(schema.Events)
	scan(schema.Alerts)
	scan(schema.AuditLog)
	if rows := f.testsOf(f.job); len(rows) == 0 || rows[len(rows)-1].GetString("status") != schema.CheckFailed {
		t.Errorf("the test did not end: %v", rows)
	}
	if kept := f.fresh(restore); kept.GetString("status") != schema.RestoreFailed || !strings.Contains(kept.GetString("error"), "***") {
		t.Errorf("the restore did not end as expected: %v", kept.FieldsData())
	}

	// the temporary databases' records are the panel's own, in the audit;
	// the new database is whoever asked for it
	entries, err := f.app.FindRecordsByFilter(schema.AuditLog, "collection={:collection}", "", 0, 0, map[string]any{"collection": schema.Databases})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, entry := range entries {
		seen[entry.GetString("record_label")] = entry.GetString("actor_kind") + " " + entry.GetString("actor_label")
	}
	if seen["pc_rs_"+restore.Id] != audit.KindSystem+" databases" || seen["pc_old_"+restore.Id] != audit.KindSystem+" databases" ||
		seen["shop_copy"] != audit.KindUser+" operator@example.com" {
		t.Errorf("the audit of the databases: %v", seen)
	}
}
