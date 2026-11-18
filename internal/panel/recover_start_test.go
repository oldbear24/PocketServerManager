package panel

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// restoredMarker is the marker `panel recover` leaves, of a backup made at
// 04:00 on 17 November 2026.
func restoredMarker(exportID string) restoreMarker {
	return restoreMarker{
		ExportID: exportID, ExportCreated: "2026-11-17T04:00:00Z", PanelVersionExport: "0.32.0",
		Migration: "1772000000_panel_recovery.go", AuditSeq: 3812, AuditHash: strings.Repeat("ab", 32),
		KitID: "KIT-7Q2M-9XHC", RecoveredAt: "2026-11-17T10:15:00Z", ToolVersion: "0.32.0",
	}
}

func writeMarker(t testing.TB, dir string, marker restoreMarker) string {
	t.Helper()
	raw, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, restoreMarkerName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// newCommand saves a command of a host in a status.
func newCommand(t testing.TB, app core.App, host *core.Record, kind, status string) *core.Record {
	t.Helper()
	collection, err := app.FindCollectionByNameOrId(schema.Commands)
	if err != nil {
		t.Fatal(err)
	}
	command := core.NewRecord(collection)
	command.Load(map[string]any{"host": host.Id, "type": kind, "status": status, "resource": "container:web"})
	if err := app.Save(command); err != nil {
		t.Fatal(err)
	}
	return command
}

// onlineHost is a server as the backup knew it: online, its stream open,
// connected long before the disaster.
func onlineHost(t testing.TB, app core.App, name string) *core.Record {
	t.Helper()
	host := testutil.NewHost(t, app, name)
	host.Set("status", schema.HostOnline)
	host.Set("stream_open", true)
	host.Set("connected_since", types.NowDateTime().Add(-48*time.Hour))
	host.Set("last_seen", types.NowDateTime().Add(-24*time.Hour))
	if err := app.Save(host); err != nil {
		t.Fatal(err)
	}
	return host
}

func newestSecurityEntry(t testing.TB, app core.App) *core.Record {
	t.Helper()
	entries, err := app.FindRecordsByFilter(schema.AuditLog, "category = 'security'", "-seq", 1, 0)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no security entry: %v", err)
	}
	return entries[0]
}

// TestFinishRestore (#32): the first start after `panel recover` writes the
// sealed panel_restored entry, cancels what was queued and leaves what was
// dispatched to the agents, marks every server offline without an alert,
// ends every session, records the restore and raises panel_restored, which
// resolves once every server is back. A second start from the same export
// only removes the marker; data of a newer panel keeps it.
func TestFinishRestore(t *testing.T) {
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)
	// the database as the backup held it
	authority := caOfAnEarlierStart(t, app)
	if err := setPanelState(app.DB(), dbx.Params{"key_id": secrets.KeyID(testutil.MasterKey), "ca_pin": authority.Pin()}); err != nil {
		t.Fatal(err)
	}
	vm, node := onlineHost(t, app, "vm"), onlineHost(t, app, "node-b")
	queued := newCommand(t, app, node, "container.restart", schema.CommandQueued)
	dispatched := newCommand(t, app, vm, "container.restart", schema.CommandDispatched)
	user, oldToken := testutil.NewUser(t, app, "admin@example.com", schema.RoleAdmin)
	marker := writeMarker(t, app.DataDir(), restoredMarker(strings.Repeat("e1", 16)))

	p := New(app, &Config{MasterKey: testutil.MasterKey, GRPCAddr: "127.0.0.1:0"}, testUI)
	p.Register()
	handler := testutil.Handler(t, app)

	// the sealed entry, where the restored log resumes
	result, err := audit.Verify(app, p.AuditKey())
	if err != nil || !result.OK || result.Unsealed != 0 {
		t.Fatalf("the audit log after the restore: %+v, %v", result, err)
	}
	entry := newestSecurityEntry(t, app)
	var details map[string]any
	if err := json.Unmarshal([]byte(entry.GetString("snapshot")), &details); err != nil {
		t.Fatal(err)
	}
	if entry.GetString("action") != audit.ActionPanelRestored || entry.GetString("actor_label") != "panel recover" ||
		entry.GetString("source") != audit.SourceCommand || entry.GetString("record_label") != "2026-11-17 04:00 UTC" ||
		details["export_id"] != strings.Repeat("e1", 16) || details["audit_seq"] != float64(3812) || details["kit_id"] != "KIT-7Q2M-9XHC" ||
		details["entries_after_lost"] != true || details["sessions_ended"] != true || details["queued_cancelled"] != float64(1) {
		t.Fatalf("the entry: %s %s %s %v", entry.GetString("action"), entry.GetString("actor_label"), entry.GetString("record_label"), details)
	}

	// what was queued then is not sent; what was dispatched is the agent's
	if stored, _ := app.FindRecordById(schema.Commands, queued.Id); stored.GetString("status") != schema.CommandCancelled ||
		stored.GetString("error") != "The panel was restored from a backup made 2026-11-17 04:00 UTC: what was queued then is not sent. Queue it again if it is still wanted." {
		t.Fatalf("the queued command: %s %q", stored.GetString("status"), stored.GetString("error"))
	}
	if stored, _ := app.FindRecordById(schema.Commands, dispatched.Id); stored.GetString("status") != schema.CommandDispatched {
		t.Fatalf("the dispatched command was touched: %s", stored.GetString("status"))
	}

	// offline, so that each reconnect is a return, without an alert
	for _, host := range []*core.Record{vm, node} {
		stored, _ := app.FindRecordById(schema.Hosts, host.Id)
		if stored.GetString("status") != schema.HostOffline || stored.GetBool("stream_open") {
			t.Fatalf("%s is %s, stream open %v", host.GetString("name"), stored.GetString("status"), stored.GetBool("stream_open"))
		}
	}
	if n, _ := app.CountRecords(schema.Alerts, dbx.HashExp{"rule": "agent_offline"}); n != 0 {
		t.Fatalf("%d offline alerts", n)
	}
	if n, _ := app.CountRecords(schema.Events, dbx.HashExp{"kind": "agent.offline"}); n != 0 {
		t.Fatalf("%d offline events", n)
	}

	// every session from before ends
	if status, _ := testutil.Do(t, handler, testutil.Request{Method: http.MethodGet, URL: "/api/pc/info", Token: oldToken}); status != http.StatusUnauthorized {
		t.Fatalf("a token from before the restore: %d", status)
	}
	fresh, _ := app.FindRecordById(schema.Users, user.Id)
	newToken, err := fresh.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := testutil.Do(t, handler, testutil.Request{Method: http.MethodGet, URL: "/api/pc/info", Token: newToken}); status != http.StatusOK {
		t.Fatalf("a new sign-in: %d", status)
	}

	// the restore is recorded, the marker gone, the alert open
	state := panelStateOf(t, app)
	var from restoreMarker
	if err := json.Unmarshal([]byte(state.RestoredFrom), &from); err != nil || from.ExportID != strings.Repeat("e1", 16) || state.RestoredAt == "" {
		t.Fatalf("recorded %+v", state)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the marker is left: %v", err)
	}
	alerts, err := app.FindRecordsByFilter(schema.Alerts, "rule = 'panel_restored' && target = 'panel' && status = 'open'", "", 0, 0)
	if err != nil || len(alerts) != 1 || alerts[0].GetString("message") != "The panel was restored from a backup made 2026-11-17 04:00 UTC: 0 of 2 servers are back." ||
		alerts[0].GetString("severity") != schema.SeverityWarning || alerts[0].GetString("host") != "" {
		t.Fatalf("the panel_restored alert: %d, %v", len(alerts), err)
	}

	// the servers come back one by one; with both back it resolves
	reconnect := func(host *core.Record) {
		stored, _ := app.FindRecordById(schema.Hosts, host.Id)
		stored.Set("status", schema.HostOnline)
		stored.Set("stream_open", true)
		stored.Set("connected_since", types.NowDateTime())
		if err := app.Save(stored); err != nil {
			t.Fatal(err)
		}
	}
	reconnect(vm)
	waitUntil(t, "one server back", func() bool {
		alert, err := app.FindFirstRecordByFilter(schema.Alerts, "rule = 'panel_restored' && status = 'open'")
		return err == nil && strings.HasSuffix(alert.GetString("message"), ": 1 of 2 servers are back.")
	})
	reconnect(node)
	waitUntil(t, "every server back", func() bool {
		return !alertOpen(app, backup.AlertPanelRestored, backup.PanelAlertTarget) && !p.restoreWatch.Load()
	})

	// a second start from the same export only removes the marker
	entries, _ := app.CountRecords(schema.AuditLog)
	later := newCommand(t, app, node, "container.restart", schema.CommandQueued)
	again := writeMarker(t, app.DataDir(), restoredMarker(strings.Repeat("e1", 16)))
	if err := p.finishRestore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(again); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the marker of a finished restore is left")
	}
	if after, _ := app.CountRecords(schema.AuditLog); after != entries {
		t.Fatalf("a second start wrote %d entries", after-entries)
	}
	if stored, _ := app.FindRecordById(schema.Commands, later.Id); stored.GetString("status") != schema.CommandQueued {
		t.Fatal("a second start cancelled what was queued since")
	}
	if status, _ := testutil.Do(t, handler, testutil.Request{Method: http.MethodGet, URL: "/api/pc/info", Token: newToken}); status != http.StatusOK {
		t.Fatalf("a second start ended the sessions again: %d", status)
	}

	// data of a newer panel: the start stops, the marker stays
	if _, err := app.DB().Insert(core.DefaultMigrationsTable, dbx.Params{"file": "1799000000_from_the_future.go", "applied": time.Now().UnixMicro()}).Execute(); err != nil {
		t.Fatal(err)
	}
	newer := restoredMarker(strings.Repeat("f2", 16))
	newer.PanelVersionExport = "0.40.0"
	kept := writeMarker(t, app.DataDir(), newer)
	err = p.finishRestore()
	if err == nil || err.Error() != "The data comes from a newer panel (0.40.0): start that version or a newer one." {
		t.Fatalf("a newer panel's data: %v", err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("the marker of a refused restore is gone: %v", err)
	}
	if stored, _ := app.FindRecordById(schema.Commands, later.Id); stored.GetString("status") != schema.CommandQueued {
		t.Fatal("a refused restore cancelled commands")
	}
	if after := panelStateOf(t, app); after.RestoredFrom != state.RestoredFrom {
		t.Fatal("a refused restore was recorded")
	}
}

// waitUntil waits up to 10 seconds for a condition.
func waitUntil(t testing.TB, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTheRestoreIsWatchedAfterARestart (#32): a restored panel started
// again while servers are still coming back, as with a docker restart in
// its first minute, follows them on: the panel_restored alert counts each
// server that comes back and resolves once every one did.
func TestTheRestoreIsWatchedAfterARestart(t *testing.T) {
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)
	authority := caOfAnEarlierStart(t, app)
	if err := setPanelState(app.DB(), dbx.Params{"key_id": secrets.KeyID(testutil.MasterKey), "ca_pin": authority.Pin()}); err != nil {
		t.Fatal(err)
	}
	vm, node := onlineHost(t, app, "vm"), onlineHost(t, app, "node-b")
	writeMarker(t, app.DataDir(), restoredMarker(strings.Repeat("e1", 16)))
	first := New(app, &Config{MasterKey: testutil.MasterKey, GRPCAddr: "127.0.0.1:0"}, testUI)
	first.Register()
	testutil.Handler(t, app)
	if !alertOpen(app, backup.AlertPanelRestored, backup.PanelAlertTarget) {
		t.Fatal("no panel_restored alert after the restore")
	}

	// the panel starts again, its memory gone
	_, again := restartedPanel(t, app, nil)
	message := func() string {
		alert, err := again.FindFirstRecordByFilter(schema.Alerts, "rule = 'panel_restored' && status = 'open'")
		if err != nil {
			return ""
		}
		return alert.GetString("message")
	}
	if got := message(); got != "The panel was restored from a backup made 2026-11-17 04:00 UTC: 0 of 2 servers are back." {
		t.Fatalf("after the restart: %q", got)
	}
	reconnect := func(host *core.Record) {
		stored, err := again.FindRecordById(schema.Hosts, host.Id)
		if err != nil {
			t.Fatal(err)
		}
		stored.Set("status", schema.HostOnline)
		stored.Set("stream_open", true)
		stored.Set("connected_since", types.NowDateTime())
		if err := again.Save(stored); err != nil {
			t.Fatal(err)
		}
	}
	reconnect(vm)
	waitUntil(t, "one server back", func() bool { return strings.HasSuffix(message(), ": 1 of 2 servers are back.") })
	reconnect(node)
	waitUntil(t, "every server back", func() bool { return !alertOpen(again, backup.AlertPanelRestored, backup.PanelAlertTarget) })
}
