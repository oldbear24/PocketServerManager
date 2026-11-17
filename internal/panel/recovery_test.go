package panel

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/recoverykit"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// signInPassword is the password of testutil's users.
const signInPassword = "correct horse battery staple"

// recoveryFixture is a panel with a server, a panel job carried by it to two
// targets, and an admin.
type recoveryFixture struct {
	*jobFixture
	local *core.Record
	panel *core.Record
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	f := &recoveryFixture{jobFixture: newJobFixture(t)}
	f.local = f.addTarget("panel-disk", backup.KindLocal, f.host, backup.Config{Path: "/srv/pc-panel32"}, backup.Credentials{})
	f.panel = f.job("panel (pb_data)", map[string]any{"source_kind": schema.SourcePanel, "targets": []string{f.target.Id, f.local.Id}})
	f.host.Set("panel_address", "192.0.2.2:9443")
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	return f
}

// exportKit downloads a kit as an admin.
func (f *recoveryFixture) exportKit(token, passphrase, password string) (int, map[string]any, http.Header) {
	f.t.Helper()
	raw, _ := json.Marshal(map[string]any{"passphrase": passphrase, "password": password})
	status, headers, out := doWithHeaders(f.t, f.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/recovery/kit", Token: token, Body: string(raw)})
	answer := map[string]any{}
	_ = json.Unmarshal([]byte(out), &answer)
	answer["raw"] = out
	return status, answer, headers
}

func (f *recoveryFixture) checkKit(token, kit, passphrase string) (int, map[string]any) {
	f.t.Helper()
	return f.send(http.MethodPost, "/api/pc/recovery/kit/check", token, map[string]any{"kit": kit, "passphrase": passphrase})
}

func (f *recoveryFixture) recovery() map[string]any {
	f.t.Helper()
	status, out := f.send(http.MethodGet, "/api/pc/recovery", f.admin, nil)
	if status != http.StatusOK {
		f.t.Fatalf("GET /recovery: %d %v", status, out)
	}
	return out
}

func kitOfAnswer(answer map[string]any) map[string]any {
	kit, _ := answer["kit"].(map[string]any)
	return kit
}

// doWithHeaders is testutil.Do with the answer's headers.
func doWithHeaders(t testing.TB, handler http.Handler, r testutil.Request) (int, http.Header, string) {
	t.Helper()
	recorder := &headerRecorder{header: http.Header{}}
	req, err := http.NewRequest(r.Method, r.URL, strings.NewReader(r.Body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", r.Token)
	handler.ServeHTTP(recorder, req)
	return recorder.status, recorder.header, recorder.body.String()
}

type headerRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *headerRecorder) Header() http.Header { return r.header }
func (r *headerRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}
func (r *headerRecorder) WriteHeader(status int) { r.status = status }

// TestTheKitStatusFollowsTheKits (#32): never, then unverified once one
// was downloaded, saved once a check opened it, and stale with the reason
// once a target's credentials changed.
func TestTheKitStatusFollowsTheKits(t *testing.T) {
	f := newRecoveryFixture(t)
	if kit := kitOfAnswer(f.recovery()); kit["status"] != kitNever {
		t.Fatalf("before any kit: %v", kit)
	}

	status, answer, _ := f.exportKit(f.admin, kitPassphrase, signInPassword)
	if status != http.StatusOK {
		t.Fatalf("download: %d %v", status, answer)
	}
	if kit := kitOfAnswer(f.recovery()); kit["status"] != kitUnverified || kit["kit_id"] != answer["kit_id"] || kit["created_by"] != "admin@example.com" {
		t.Fatalf("downloaded: %v", kit)
	}

	status, checked := f.checkKit(f.admin, answer["content"].(string), kitPassphrase)
	if status != http.StatusOK || checked["current"] != true || checked["status"] != kitSaved || checked["kit_id"] != answer["kit_id"] {
		t.Fatalf("check: %d %v", status, checked)
	}
	kit := kitOfAnswer(f.recovery())
	if kit["status"] != kitSaved || kit["verified_by"] != "admin@example.com" || kit["verified_at"] == "" {
		t.Fatalf("checked: %v", kit)
	}

	// the target's credentials change, as a PATCH of them does
	if err := f.p.sealTarget(f.target, backup.Credentials{AccessKeyID: "key-id", SecretAccessKey: "a-new-secret-key"}, "repo-password-b2"); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Save(f.target); err != nil {
		t.Fatal(err)
	}
	kit = kitOfAnswer(f.recovery())
	if kit["status"] != kitStale || !slices.Equal(toStrings(kit["reasons"]), []string{recoverykit.SectionTargets}) {
		t.Fatalf("after a change of credentials: %v", kit)
	}
	// the old kit, checked again, opens but is not current
	status, checked = f.checkKit(f.admin, answer["content"].(string), kitPassphrase)
	if status != http.StatusOK || checked["current"] != false || !slices.Equal(toStrings(checked["reasons"]), []string{"targets"}) || checked["status"] != kitStale {
		t.Fatalf("an old kit: %d %v", status, checked)
	}
	// a change of the panel's addresses names its reason too
	f.host.Set("panel_address", "panel.example.com:9443")
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	if kit := kitOfAnswer(f.recovery()); !slices.Equal(toStrings(kit["reasons"]), []string{recoverykit.SectionAddress, recoverykit.SectionTargets}) {
		t.Fatalf("after a change of address: %v", kit)
	}
}

func toStrings(value any) []string {
	list, _ := value.([]any)
	out := []string{}
	for _, item := range list {
		out = append(out, item.(string))
	}
	return out
}

// TestTheKitRoutesAreForAdmins (#32).
func TestTheKitRoutesAreForAdmins(t *testing.T) {
	f := newRecoveryFixture(t)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	for _, route := range []struct{ method, url string }{
		{http.MethodGet, "/api/pc/recovery"}, {http.MethodPost, "/api/pc/recovery/kit"}, {http.MethodPost, "/api/pc/recovery/kit/check"},
	} {
		if status, _ := f.send(route.method, route.url, operator, map[string]any{"passphrase": kitPassphrase, "password": signInPassword}); status != http.StatusForbidden {
			t.Errorf("%s %s as an operator: %d", route.method, route.url, status)
		}
		if status, _ := f.send(route.method, route.url, "", nil); status != http.StatusUnauthorized {
			t.Errorf("%s %s signed out: %d", route.method, route.url, status)
		}
	}
}

// TestADownloadNeedsThePasswordAndAGoodPassphrase (#32): the sign-in
// password, a passphrase of 12 characters to 1024 bytes that is not the
// password, and 5 attempts per 15 minutes, checks included.
func TestADownloadNeedsThePasswordAndAGoodPassphrase(t *testing.T) {
	f := newRecoveryFixture(t)
	for _, test := range []struct{ passphrase, password, want string }{
		{kitPassphrase, "not my password", "Your password is not right."},
		{"eleven char", signInPassword, "The passphrase needs at least 12 characters."},
		{strings.Repeat("ž", 513), signInPassword, "The passphrase is at most 1024 bytes."},
		{signInPassword, signInPassword, "The passphrase cannot be your sign-in password."},
	} {
		status, answer, _ := f.exportKit(f.admin, test.passphrase, test.password)
		if status != http.StatusBadRequest || answer["message"] != test.want {
			t.Errorf("%q, %q: %d %v", test.passphrase, test.password, status, answer["message"])
		}
	}
	// the fifth attempt, a check, still goes through; the sixth does not
	if status, _ := f.checkKit(f.admin, "no kit", kitPassphrase); status != http.StatusBadRequest {
		t.Fatalf("the fifth attempt: %d", status)
	}
	status, answer, _ := f.exportKit(f.admin, kitPassphrase, signInPassword)
	if status != http.StatusTooManyRequests || answer["message"] != "Too many attempts: try again in 15 minutes." {
		t.Fatalf("the sixth attempt: %d %v", status, answer)
	}
	if status, answer := f.checkKit(f.admin, "no kit", kitPassphrase); status != http.StatusTooManyRequests {
		t.Fatalf("a check after the limit: %d %v", status, answer)
	}
	// another admin has attempts of their own
	_, other := testutil.NewUser(t, f.app, "other@example.com", schema.RoleAdmin)
	if status, answer, _ := f.exportKit(other, kitPassphrase, signInPassword); status != http.StatusOK {
		t.Fatalf("another admin: %d %v", status, answer)
	}
	if n, _ := f.app.CountRecords(schema.RecoveryKits); n != 1 {
		t.Fatalf("%d kits recorded", n)
	}
}

// TestOneKitIsSealedAtATime (#32): argon2 takes 64 MiB, so a second
// download while one is sealed is refused.
func TestOneKitIsSealedAtATime(t *testing.T) {
	f := newRecoveryFixture(t)
	f.p.kitSealing <- struct{}{}
	status, answer, _ := f.exportKit(f.admin, kitPassphrase, signInPassword)
	<-f.p.kitSealing
	if status != http.StatusServiceUnavailable || answer["message"] != "Another recovery kit is being sealed: try again in a moment." {
		t.Fatalf("a second download at once: %d %v", status, answer)
	}
}

// TestTheKitHoldsWhatRecoveryNeeds (#32): the kit opens with its
// passphrase and holds the master key, the panel's CA pin, start
// environment and addresses, its job, and for each target exactly the
// repository the enricher delivers to the job's carrier; the answer is not
// cached.
func TestTheKitHoldsWhatRecoveryNeeds(t *testing.T) {
	f := newRecoveryFixture(t)
	status, answer, headers := f.exportKit(f.admin, kitPassphrase, signInPassword)
	if status != http.StatusOK {
		t.Fatalf("download: %d %v", status, answer)
	}
	if headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control: %q", headers.Get("Cache-Control"))
	}
	if !regexp.MustCompile(`^pocketcloud-recovery-kit-\d{8}-KIT-[0-9A-Z]{4}-[0-9A-Z]{4}\.pckit$`).MatchString(answer["file_name"].(string)) {
		t.Fatalf("file name %v", answer["file_name"])
	}
	header, payload, err := recoverykit.Open(answer["content"].(string), kitPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	pin := f.p.Authority.Pin()
	if header.Kit != answer["kit_id"] || header.KeyID != secrets.KeyID(testutil.MasterKey) || header.CAPin != pin ||
		payload.MasterKey != hex.EncodeToString(testutil.MasterKey) || payload.CreatedBy != "admin@example.com" {
		t.Fatalf("the kit: %+v", header)
	}
	if payload.Env[EnvGRPCAddr] != "127.0.0.1:0" || payload.Env["POCKETCLOUD_METRICS_TOKEN"] != "" {
		t.Fatalf("the environment: %v", payload.Env)
	}
	if len(payload.AgentsDial) != 1 || payload.AgentsDial[0] != (recoverykit.Dial{Address: "192.0.2.2:9443", Servers: 1, IP: true}) {
		t.Fatalf("the addresses: %v", payload.AgentsDial)
	}
	if payload.Job == nil || payload.Job.ID != f.panel.Id || payload.Job.Name != "panel (pb_data)" || len(payload.Targets) != 2 {
		t.Fatalf("the job: %+v %d targets", payload.Job, len(payload.Targets))
	}

	// each target as the carrier gets it with a run of the panel's job
	for _, kitTarget := range payload.Targets {
		target, err := f.app.FindRecordById(schema.BackupTargets, kitTarget.ID)
		if err != nil {
			t.Fatal(err)
		}
		run := f.save(schema.BackupRuns, map[string]any{"job": f.panel.Id, "target": target.Id, "host": f.host.Id, "trigger": schema.RunManual, "status": schema.RunQueued})
		command := f.command(f.host.Id, &agentv1.Command{Payload: &agentv1.Command_BackupRun{BackupRun: &agentv1.BackupRun{
			RunId: run.Id, Repository: &agentv1.BackupRepository{TargetId: target.Id}, PanelExport: backup.PanelExportID(run.Id),
		}}})
		delivered, err := f.deliver(command)
		if err != nil {
			t.Fatal(err)
		}
		repo := delivered.GetBackupRun().GetRepository()
		got := kitTarget.Repository
		if got.URL != repo.GetUrl() || got.Password != repo.GetPassword() || got.SSHKey != repo.GetSshKey() || got.KnownHosts != repo.GetKnownHosts() ||
			got.Mount != repo.GetMount() || !mapsEqual(got.Env, repo.GetEnv()) || !mapsEqual(got.Options, repo.GetOptions()) {
			t.Errorf("%s: the kit holds %+v, the carrier gets %v", kitTarget.Name, got, repo)
		}
		if kitTarget.Name != target.GetString("name") || kitTarget.Kind != target.GetString("kind") {
			t.Errorf("the target %+v", kitTarget)
		}
	}
	if payload.Targets[1].Server != "node-1" || payload.Targets[0].Server != "" {
		t.Errorf("a local target names its server: %+v", payload.Targets)
	}

	// the export is recorded and audited, without a secret
	entry := newestSecurityEntry(t, f.app)
	var details map[string]any
	_ = json.Unmarshal([]byte(entry.GetString("snapshot")), &details)
	if entry.GetString("action") != audit.ActionRecoveryKitExported || entry.GetString("record_label") != header.Kit ||
		details["key_id"] != header.KeyID || details["job"] != f.panel.Id || strings.Join(toStrings(details["targets"]), ",") != "b2,panel-disk" ||
		strings.Join(toStrings(details["addresses"]), ",") != "192.0.2.2:9443" {
		t.Fatalf("the audit entry: %s %v", entry.GetString("action"), details)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

// TestAKitIsCheckedAgainstThisPanel (#32): a kit of another master key is
// refused, as is one that a wrong passphrase does not open and text that is
// no kit; a current kit's check resolves recovery_kit, and a kit the panel
// has no row of, as one made after the backup it was restored from, gets one.
func TestAKitIsCheckedAgainstThisPanel(t *testing.T) {
	f := newRecoveryFixture(t)
	other := sealKit(t, otherKey, f.p.Authority.Pin())
	status, answer := f.checkKit(f.admin, other, kitPassphrase)
	if status != http.StatusConflict || answer["message"] != "This kit holds another master key (id "+secrets.KeyID(otherKey)+"): it is not this panel's." {
		t.Fatalf("another key: %d %v", status, answer)
	}
	if status, answer := f.checkKit(f.admin, other, "not the passphrase"); status != http.StatusBadRequest ||
		answer["message"] != "The passphrase does not open this kit, or the kit was changed." {
		t.Fatalf("a wrong passphrase: %d %v", status, answer)
	}
	if status, answer := f.checkKit(f.admin, "-----BEGIN POCKETCLOUD RECOVERY KIT-----\nKit: x", kitPassphrase); status != http.StatusBadRequest ||
		answer["message"] != "This is no PocketCloud recovery kit." {
		t.Fatalf("no kit: %d %v", status, answer)
	}
	if status, answer := f.send(http.MethodPost, "/api/pc/recovery/kit/check", f.admin, map[string]any{"kit": strings.Repeat("x", 80<<10), "passphrase": kitPassphrase}); status != http.StatusBadRequest {
		t.Fatalf("a body too large: %d %v", status, answer)
	}

	f.p.evaluateRecoveryAlerts()
	if !alertOpen(f.app, backup.AlertRecoveryKit, backup.PanelAlertTarget) {
		t.Fatal("recovery_kit is not open without a kit")
	}
	// a kit of this panel the panel has no row of
	payload, _, err := f.p.currentKit(true)
	if err != nil {
		t.Fatal(err)
	}
	header := recoverykit.Header{Kit: "KIT-0000-0001", Created: time.Now(), Panel: "http://panel.test", KeyID: payload.KeyID, CAPin: payload.CAPin,
		PanelVersion: payload.PanelVersion, AgentImage: payload.AgentImage}
	payload.CreatedBy = "former@example.com"
	text, err := recoverykit.Seal(header, payload, kitPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	status, answer = f.checkKit(f.admin, text, kitPassphrase)
	if status != http.StatusOK || answer["current"] != true || answer["status"] != kitSaved {
		t.Fatalf("a current kit: %d %v", status, answer)
	}
	row, err := f.app.FindFirstRecordByData(schema.RecoveryKits, "kit_id", "KIT-0000-0001")
	if err != nil || row.GetString("created_by") != "former@example.com" || row.GetString("verified_by") != "admin@example.com" {
		t.Fatalf("its row: %v", err)
	}
	if alertOpen(f.app, backup.AlertRecoveryKit, backup.PanelAlertTarget) {
		t.Fatal("a current kit's check left recovery_kit open")
	}
	if entry := newestSecurityEntry(t, f.app); entry.GetString("action") != audit.ActionRecoveryKitChecked || entry.GetString("record_label") != "KIT-0000-0001" {
		t.Fatalf("the audit entry: %s", entry.GetString("action"))
	}
}

// sealKit seals a kit of a master key with kitPassphrase.
func sealKit(t testing.TB, master []byte, pin string) string {
	t.Helper()
	text, err := recoverykit.Seal(recoverykit.Header{Kit: "KIT-0000-0002", Created: time.Now(), Panel: "http://panel.test",
		KeyID: secrets.KeyID(master), CAPin: pin, PanelVersion: "0.32.0", AgentImage: "agent"},
		recoverykit.Payload{MasterKey: hex.EncodeToString(master)}, kitPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// TestTheKitsListKeepsTwenty (#32).
func TestTheKitsListKeepsTwenty(t *testing.T) {
	f := newRecoveryFixture(t)
	for i := range 21 {
		f.save(schema.RecoveryKits, map[string]any{"kit_id": "KIT-0000-" + strings.Repeat("A", 2) + string(rune('A'+i/10)) + string(rune('A'+i%10))})
	}
	f.p.keepNewestKits()
	if n, _ := f.app.CountRecords(schema.RecoveryKits); n != keptKits {
		t.Fatalf("%d kits kept", n)
	}
}

// TestNoSecretReachesLogsEventsOrTheAudit (#32): the passphrase, the
// master key, the sign-in password and the targets' secrets are nowhere the
// panel keeps or logs: the audit log, events, alerts, commands, the kits'
// rows, PocketBase's request log and the panel's own log.
func TestNoSecretReachesLogsEventsOrTheAudit(t *testing.T) {
	f := newRecoveryFixture(t)
	var logged bytes.Buffer
	f.p.Log = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	status, answer, _ := f.exportKit(f.admin, kitPassphrase, signInPassword)
	if status != http.StatusOK {
		t.Fatalf("download: %d %v", status, answer)
	}
	if status, _ := f.checkKit(f.admin, answer["content"].(string), kitPassphrase); status != http.StatusOK {
		t.Fatalf("check: %d", status)
	}
	f.exportKit(f.admin, "short", signInPassword)
	f.exportKit(f.admin, kitPassphrase, "a wrong password")
	f.checkKit(f.admin, answer["content"].(string), "a wrong passphrase of it")
	f.p.evaluateRecoveryAlerts()

	secretsOf := map[string]string{
		"the passphrase": kitPassphrase, "the master key": hex.EncodeToString(testutil.MasterKey), "the raw master key": string(testutil.MasterKey),
		"the sign-in password": signInPassword, "a repository password": "repo-password-b2", "the S3 secret": "the-secret-key",
		"a wrong passphrase": "a wrong passphrase of it",
	}
	var kept strings.Builder
	for _, collection := range []string{schema.AuditLog, schema.Events, schema.Alerts, schema.Commands, schema.RecoveryKits} {
		records, err := f.app.FindAllRecords(collection)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range records {
			raw, _ := json.Marshal(record.FieldsData())
			kept.Write(raw)
		}
	}
	var requests []struct {
		Data    string `db:"data"`
		Message string `db:"message"`
	}
	if err := f.app.AuxDB().NewQuery("SELECT data, message FROM _logs").All(&requests); err != nil {
		t.Fatal(err)
	}
	for _, request := range requests {
		kept.WriteString(request.Data + request.Message)
	}
	kept.WriteString(logged.String())
	for name, secret := range secretsOf {
		if strings.Contains(kept.String(), secret) {
			t.Errorf("%s is kept or logged", name)
		}
	}
	if !strings.Contains(kept.String(), "recovery_kit_exported") {
		t.Fatal("the search found nothing at all")
	}
}

// TestTheRecoveryAlerts (#32): nothing before the first server; then
// recovery_kit and panel_backup open, recovery_kit turns critical after 7
// days, and each resolves once its cause is gone: a checked current kit, a
// job with targets that succeeded within 48 hours.
func TestTheRecoveryAlerts(t *testing.T) {
	p, app, handler := newTestPanel(t)
	p.evaluateRecoveryAlerts()
	if n, _ := app.CountRecords(schema.Alerts); n != 0 {
		t.Fatalf("%d alerts before the first server", n)
	}

	f := &jobFixture{t: t, p: p, app: app, handler: handler, host: testutil.NewHost(t, app, "node-1")}
	f.target = f.addTarget("b2", backup.KindS3, f.host, backup.Config{Endpoint: "https://s3.example.com", Bucket: "backups"},
		backup.Credentials{AccessKeyID: "key-id", SecretAccessKey: "the-secret-key"})
	_, f.admin = testutil.NewUser(t, app, "admin@example.com", schema.RoleAdmin)
	p.evaluateRecoveryAlerts()
	alert := func(rule string) *core.Record {
		t.Helper()
		open, err := app.FindFirstRecordByFilter(schema.Alerts, "rule = {:rule} && target = 'panel' && status != 'resolved'", dbx.Params{"rule": rule})
		if err != nil {
			return nil
		}
		return open
	}
	kit, backupAlert := alert(backup.AlertRecoveryKit), alert(backup.AlertPanelBackup)
	if kit == nil || kit.GetString("severity") != schema.SeverityWarning || kit.GetString("host") != "" ||
		!strings.HasPrefix(kit.GetString("message"), "No recovery kit was downloaded") {
		t.Fatalf("recovery_kit: %v", kit)
	}
	if backupAlert == nil || backupAlert.GetString("severity") != schema.SeverityWarning || !strings.HasPrefix(backupAlert.GetString("message"), "The panel itself is not backed up") {
		t.Fatalf("panel_backup: %v", backupAlert)
	}

	// a week open: critical
	kit.Set("first_seen", types.NowDateTime().Add(-8*24*time.Hour))
	if err := app.Save(kit); err != nil {
		t.Fatal(err)
	}
	p.evaluateRecoveryAlerts()
	if kit := alert(backup.AlertRecoveryKit); kit.GetString("severity") != schema.SeverityCritical {
		t.Fatalf("after a week: %s", kit.GetString("severity"))
	}

	// a job turned off, then one without targets, then one that never
	// succeeded, then one that did
	job := f.job("panel (pb_data)", map[string]any{"source_kind": schema.SourcePanel, "enabled": false})
	p.evaluateRecoveryAlerts()
	if message := alert(backup.AlertPanelBackup).GetString("message"); message != "The panel's backup job panel (pb_data) is turned off." {
		t.Fatalf("a job turned off: %s", message)
	}
	job.Set("enabled", true)
	job.Set("targets", []string{})
	if err := app.Save(job); err != nil {
		t.Fatal(err)
	}
	p.evaluateRecoveryAlerts()
	if message := alert(backup.AlertPanelBackup).GetString("message"); message != "The panel's backup job panel (pb_data) has no target." {
		t.Fatalf("a job without targets: %s", message)
	}
	job.Set("targets", []string{f.target.Id})
	if err := app.Save(job); err != nil {
		t.Fatal(err)
	}
	p.evaluateRecoveryAlerts()
	if open := alert(backup.AlertPanelBackup); open != nil {
		t.Fatalf("a new job is given its first 48 hours: %s", open.GetString("message"))
	}
	if _, err := app.DB().Update(schema.BackupJobs, dbx.Params{"created": types.NowDateTime().Add(-72 * time.Hour).String()}, dbx.HashExp{"id": job.Id}).Execute(); err != nil {
		t.Fatal(err)
	}
	p.evaluateRecoveryAlerts()
	if message := alert(backup.AlertPanelBackup).GetString("message"); message != "The panel's backup has not succeeded once in 48 hours." {
		t.Fatalf("a job that never succeeded: %s", message)
	}
	run := f.run(job)
	run.Set("status", schema.RunSucceeded)
	run.Set("finished_at", types.NowDateTime().Add(-50*time.Hour))
	if err := app.Save(run); err != nil {
		t.Fatal(err)
	}
	p.evaluateRecoveryAlerts()
	if message := alert(backup.AlertPanelBackup).GetString("message"); !strings.HasPrefix(message, "The panel's backup has not succeeded for more than 48 hours: the last success was ") {
		t.Fatalf("an old success: %s", message)
	}
	run.Set("finished_at", types.NowDateTime().Add(-time.Hour))
	if err := app.Save(run); err != nil {
		t.Fatal(err)
	}
	p.evaluateRecoveryAlerts()
	if open := alert(backup.AlertPanelBackup); open != nil {
		t.Fatalf("a recent success: %s", open.GetString("message"))
	}

	// a downloaded kit is not enough; a checked one is
	r := &recoveryFixture{jobFixture: f, panel: job}
	_, answer, _ := r.exportKit(f.admin, kitPassphrase, signInPassword)
	if message := alert(backup.AlertRecoveryKit).GetString("message"); message != "The recovery kit "+answer["kit_id"].(string)+" was downloaded but not checked: check it in Settings › Recovery kit." {
		t.Fatalf("a downloaded kit: %s", message)
	}
	r.checkKit(f.admin, answer["content"].(string), kitPassphrase)
	if alert(backup.AlertRecoveryKit) != nil {
		t.Fatal("a checked kit left the alert open")
	}

	// a change makes the kit stale, and the alert comes back by itself
	recoveryDebounce = 50 * time.Millisecond
	t.Cleanup(func() { recoveryDebounce = 5 * time.Second })
	host, _ := app.FindRecordById(schema.Hosts, f.host.Id)
	host.Set("panel_address", "192.0.2.2:9443")
	if err := app.Save(host); err != nil {
		t.Fatal(err)
	}
	f.eventually("recovery_kit to come back", func() bool {
		open := alert(backup.AlertRecoveryKit)
		return open != nil && open.GetString("message") == "The recovery kit is out of date, as the panel's addresses changed: download a new one and check it."
	})

	// without servers nothing is open
	if err := app.Delete(f.target); err != nil {
		t.Fatal(err)
	}
	if err := app.Delete(host); err != nil {
		t.Fatal(err)
	}
	f.eventually("the alerts to resolve without servers", func() bool {
		return alert(backup.AlertRecoveryKit) == nil && alert(backup.AlertPanelBackup) == nil
	})
}

// TestTheRecoveryShowsWhatARestoreNeeds (#32): GET /api/pc/recovery says
// the key id, the pin, the addresses and the servers that said none yet, the
// start environment with the expected pin, the panel's job, and the restore.
func TestTheRecoveryShowsWhatARestoreNeeds(t *testing.T) {
	f := newRecoveryFixture(t)
	testutil.NewHost(t, f.app, "node-2")
	other := testutil.NewHost(t, f.app, "node-3")
	other.Set("panel_address", "panel.example.com:9443")
	if err := f.app.Save(other); err != nil {
		t.Fatal(err)
	}
	run := f.save(schema.BackupRuns, map[string]any{"job": f.panel.Id, "target": f.target.Id, "host": f.host.Id, "trigger": schema.RunChange,
		"status": schema.RunSucceeded, "finished_at": types.NowDateTime()})
	if err := setPanelState(f.app.DB(), dbx.Params{"restored_at": types.NowDateTime().Add(-time.Minute).String(),
		"restored_from": `{"export_id":"e1","export_created":"2026-11-17T04:00:00Z","snapshot":"4f1c2d3e"}`}); err != nil {
		t.Fatal(err)
	}
	f.host.Set("connected_since", types.NowDateTime())
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}

	answer := f.recovery()
	pin := f.p.Authority.Pin()
	if answer["key_id"] != secrets.KeyID(testutil.MasterKey) || answer["ca_pin"] != pin || answer["unreported"] != float64(1) {
		t.Fatalf("the answer: %v", answer)
	}
	addresses, _ := json.Marshal(answer["addresses"])
	if string(addresses) != `[{"address":"192.0.2.2:9443","ip":true,"servers":1},{"address":"panel.example.com:9443","ip":false,"servers":1}]` {
		t.Fatalf("the addresses: %s", addresses)
	}
	env, _ := answer["start_env"].(map[string]any)
	if env[EnvExpectCAPin] != pin || env[EnvGRPCAddr] != "127.0.0.1:0" {
		t.Fatalf("the start environment: %v", env)
	}
	job, _ := answer["panel_job"].(map[string]any)
	if job["id"] != f.panel.Id || job["carrier"] != f.host.Id || job["carrier_name"] != "node-1" || job["enabled"] != true ||
		strings.Join(toStrings(job["targets"]), ",") != "b2,panel-disk" || job["last_success_at"] != run.GetString("finished_at") {
		t.Fatalf("the job: %v", job)
	}
	restored, _ := answer["restored"].(map[string]any)
	if restored["from"] != "2026-11-17T04:00:00Z" || restored["snapshot"] != "4f1c2d3e" || restored["back"] != float64(1) || restored["total"] != float64(3) {
		t.Fatalf("the restore: %v", restored)
	}

	// without a job or a restore, both are null
	if err := f.app.Delete(f.panel); err != nil {
		t.Fatal(err)
	}
	if err := setPanelState(f.app.DB(), dbx.Params{"restored_at": "", "restored_from": "{}"}); err != nil {
		t.Fatal(err)
	}
	answer = f.recovery()
	if answer["panel_job"] != nil || answer["restored"] != nil {
		t.Fatalf("no job, no restore: %v %v", answer["panel_job"], answer["restored"])
	}
}
