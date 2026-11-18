package panel

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
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// newCheckFixture is a fixture of jobs whose server is online and whose agent
// checks repositories, with the checks on a schedule whose slots the panel's
// own scheduler, which runs in the tests too, never meets: once a year, two
// months from now.
func newCheckFixture(t *testing.T) *jobFixture {
	t.Helper()
	// the panel's own scheduler passes at the start of a minute: the tests'
	// passes come first
	if now := time.Now(); now.Second() > 50 {
		time.Sleep(now.Truncate(time.Minute).Add(time.Minute + time.Second).Sub(now))
	}
	f := newJobFixture(t)
	f.host.Set("status", schema.HostOnline)
	f.host.Set("capabilities", []string{"backup.init", "backup.run", "backup.check"})
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	f.checkSettings(farSchedule(), 20)
	return f
}

// farSchedule is a schedule whose next slot is a month or two away.
func farSchedule() string {
	return fmt.Sprintf("0 5 1 %d *", (int(time.Now().UTC().Month())+1)%12+1)
}

// checkSettings stores the schedule and the groups of the checks.
func (f *jobFixture) checkSettings(schedule string, groups int) {
	f.t.Helper()
	settings, err := f.p.settingsRecord()
	if err != nil {
		f.t.Fatal(err)
	}
	settings.Set("backup_check_schedule", schedule)
	settings.Set("backup_check_groups", groups)
	if err := f.app.Save(settings); err != nil {
		f.t.Fatal(err)
	}
}

// checks are a target's checks, oldest first.
func (f *jobFixture) checks(target *core.Record) []*core.Record {
	f.t.Helper()
	rows, err := f.app.FindRecordsByFilter(schema.BackupChecks, "target={:target}", "created,id", 0, 0, map[string]any{"target": target.Id})
	if err != nil {
		f.t.Fatal(err)
	}
	return rows
}

// startedCheck starts a check of a target as the scheduler or the route does,
// and is its record and its command.
func (f *jobFixture) startedCheck(target *core.Record, trigger string) (*core.Record, *core.Record) {
	f.t.Helper()
	row, err := f.p.startCheck(target, checkStart{trigger: trigger, requester: "test"})
	if err != nil {
		f.t.Fatal(err)
	}
	command, err := f.app.FindRecordById(schema.Commands, row.GetString("command"))
	if err != nil {
		f.t.Fatalf("the check %v has no command: %v", row.FieldsData(), err)
	}
	return row, command
}

// endCheck ends a check's command as the fields say, as the agent and the
// queue would, and is the check as kept.
func (f *jobFixture) endCheck(row, command *core.Record, fields map[string]any) *core.Record {
	f.t.Helper()
	status, _ := fields["status"].(string)
	f.closeCommand(command, status)
	command.Load(map[string]any{"finished_at": types.NowDateTime()})
	command.Load(fields)
	f.p.backupCheckFinished(command)
	return f.fresh(row)
}

// answer is a check's answer as the command's result holds it.
func answer(t *testing.T, result backup.CheckResult) map[string]any {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestChecksStartInTheirSlot (#31, AC1): at the slot of the schedule, each
// target with a repository gets one check on its own server: a queued record
// and a backup.check with the target's ordering key, the next group, and no
// secret; a slot the panel missed while it was down, less than a day ago, is
// caught up once; a target being added is not checked; and each target's next
// check is the next slot.
func TestChecksStartInTheirSlot(t *testing.T) {
	f := newCheckFixture(t)
	nas := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	pending := f.addTarget("new", backup.KindS3, f.host, backup.Config{Endpoint: "https://new.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	pending.Set("status", schema.TargetPending)
	pending.Set("repo_id", "")
	if err := f.app.Save(pending); err != nil {
		t.Fatal(err)
	}
	rule := parseCronRule(farSchedule())
	slot := rule.next(time.Now())

	f.p.RunDueChecks(slot.Add(-time.Minute))
	for _, target := range []*core.Record{f.target, nas, pending} {
		if rows := f.checks(target); len(rows) != 0 {
			t.Fatalf("%d checks of %s before the slot", len(rows), target.GetString("name"))
		}
		if next := f.fresh(target).GetDateTime("next_check_at").Time(); !next.Equal(slot) {
			t.Errorf("the next check of %s: %s, not %s", target.GetString("name"), next, slot)
		}
	}
	// the panel was down at the slot of nas, which a day later is too late
	nas.Set("next_check_at", slot.Add(-25*time.Hour))
	if err := f.app.Save(nas); err != nil {
		t.Fatal(err)
	}

	f.p.RunDueChecks(slot.Add(20 * time.Second))
	rows := f.checks(f.target)
	if len(rows) != 1 {
		t.Fatalf("%d checks at the slot", len(rows))
	}
	row := rows[0]
	if row.GetString("status") != schema.CheckQueued || row.GetString("trigger") != schema.RunSchedule || row.GetString("kind") != schema.CheckIntegrity ||
		!row.GetDateTime("scheduled_at").Time().Equal(slot) || row.GetString("subset") != "1/20" || row.GetInt("attempt") != 1 ||
		row.GetString("host") != f.host.Id {
		t.Errorf("the check: %v", row.FieldsData())
	}
	command, err := f.app.FindRecordById(schema.Commands, row.GetString("command"))
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{}
	raw, _ := json.Marshal(command.Get("payload"))
	_ = json.Unmarshal(raw, &payload)
	repository, _ := payload["repository"].(map[string]any)
	if command.GetString("type") != "backup.check" || command.GetString("host") != f.host.Id || command.GetString("resource") != "backup-integrity:"+f.target.Id ||
		payload["read_data_subset"] != "1/20" || repository["target_id"] != f.target.Id || repository["url"] != "s3:https://s3.example.com/backups/node" ||
		strings.Contains(string(raw), "the-secret-key") || strings.Contains(string(raw), "repo-password") ||
		command.GetString("label") != "Check the integrity of b2" || command.GetString("requested_label") != "schedule" {
		t.Errorf("the command: %v %s", command.FieldsData(), raw)
	}
	if expires := command.GetDateTime("expires_at").Time(); expires.Before(time.Now().Add(checkQueue-time.Minute)) || expires.After(time.Now().Add(checkQueue+time.Minute)) {
		t.Errorf("the check waits until %s", expires)
	}
	target := f.fresh(f.target)
	if target.GetString("last_check") != row.Id || target.GetString("last_check_status") != schema.CheckQueued ||
		!target.GetDateTime("next_check_at").Time().Equal(rule.next(slot)) {
		t.Errorf("the target: %v", target.FieldsData())
	}
	if rows := f.checks(nas); len(rows) != 0 {
		t.Errorf("a slot more than a day old was caught up: %v", rows[0].FieldsData())
	}
	if rows := f.checks(pending); len(rows) != 0 {
		t.Error("a target being added was checked")
	}

	// once: the same slot again, as by a pass that read the target before
	f.target.Set("next_check_at", slot)
	if err := f.app.Save(f.target); err != nil {
		t.Fatal(err)
	}
	f.p.RunDueChecks(slot.Add(40 * time.Second))
	if rows := f.checks(f.target); len(rows) != 1 {
		t.Errorf("%d checks of one slot", len(rows))
	}

	// a slot the panel missed less than a day ago is caught up, for that slot
	nas.Set("next_check_at", slot)
	if err := f.app.Save(nas); err != nil {
		t.Fatal(err)
	}
	f.p.RunDueChecks(slot.Add(3 * time.Hour))
	if rows := f.checks(nas); len(rows) != 1 || !rows[0].GetDateTime("scheduled_at").Time().Equal(slot) {
		t.Errorf("the missed slot: %v", rows)
	}
}

// TestOnePassOfTheChecksRunsAtATime (#31): PocketBase's cron starts a pass
// every minute whether the one before has ended or not; a pass waits for the
// one under way, and two at once start one check per slot.
func TestOnePassOfTheChecksRunsAtATime(t *testing.T) {
	f := newCheckFixture(t)
	slot := parseCronRule(farSchedule()).next(time.Now())
	f.target.Set("next_check_at", slot)
	if err := f.app.Save(f.target); err != nil {
		t.Fatal(err)
	}
	f.p.checkPass.Lock()
	done := make(chan struct{})
	go func() {
		f.p.RunDueChecks(slot.Add(20 * time.Second))
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("a pass ran while another held the scheduler")
	case <-time.After(200 * time.Millisecond):
	}
	f.p.checkPass.Unlock()
	<-done

	f.target.Set("next_check_at", slot)
	if err := f.app.Save(f.target); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			f.p.RunDueChecks(slot.Add(30 * time.Second))
		}()
	}
	group.Wait()
	if rows := f.checks(f.target); len(rows) != 1 {
		t.Errorf("%d checks", len(rows))
	}
}

// TestAServerOfflineAtItsSlotSkipsTheCheck (#31): a server marked offline at
// the slot gets a skipped check, an event and no alert, and the check is tried
// again an hour later, as a retry of its own; a retry whose server is still
// offline waits another hour.
func TestAServerOfflineAtItsSlotSkipsTheCheck(t *testing.T) {
	f := newCheckFixture(t)
	f.host.Set("status", schema.HostOffline)
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	f.p.SetStarted(time.Now().Add(-3 * time.Minute))
	slot := parseCronRule(farSchedule()).next(time.Now())
	f.target.Set("next_check_at", slot)
	if err := f.app.Save(f.target); err != nil {
		t.Fatal(err)
	}
	f.p.RunDueChecks(slot.Add(20 * time.Second))
	rows := f.checks(f.target)
	if len(rows) != 1 {
		t.Fatalf("%d checks", len(rows))
	}
	skipped := rows[0]
	retry := skipped.GetDateTime("retry_at").Time()
	if skipped.GetString("status") != schema.CheckSkipped || skipped.GetString("error") != "The server node-1 was offline at 05:00 UTC." ||
		skipped.GetString("command") != "" || !retry.Equal(slot.Add(time.Hour+20*time.Second)) {
		t.Errorf("the skipped check: %v", skipped.FieldsData())
	}
	if f.openAlert(alertCheckUnverified, f.target.Id) != nil {
		t.Error("a server offline at the slot raised an alert")
	}
	if found := f.eventsOf("backup.check_skipped"); len(found) != 1 || found[0].GetString("message") != "The integrity check of b2 was skipped: the server node-1 was offline at 05:00 UTC. PocketCloud checks again at "+retry.UTC().Format("15:04")+" UTC." {
		t.Errorf("events: %v", found)
	}
	if target := f.fresh(f.target); target.GetString("last_check_status") != schema.CheckSkipped || target.GetString("last_check_error") != "The server node-1 was offline at 05:00 UTC." {
		t.Errorf("the target: %v", target.FieldsData())
	}

	// still offline: an hour later
	f.p.RunDueChecks(retry.Add(time.Minute))
	if rows := f.checks(f.target); len(rows) != 1 {
		t.Fatalf("%d checks while the server stays offline", len(rows))
	}
	if moved := f.fresh(skipped).GetDateTime("retry_at").Time(); !moved.After(retry.Add(59 * time.Minute)) {
		t.Errorf("the retry moved to %s", moved)
	}

	// back
	f.host.Set("status", schema.HostOnline)
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	f.p.RunDueChecks(retry.Add(2 * time.Hour))
	rows = f.checks(f.target)
	if len(rows) != 2 {
		t.Fatalf("%d checks once the server is back", len(rows))
	}
	again := rows[1]
	if again.GetString("trigger") != schema.RunRetry || again.GetInt("attempt") != 2 || again.GetString("retry_of") != skipped.Id ||
		again.GetString("status") != schema.CheckQueued || again.GetString("command") == "" || !again.GetDateTime("scheduled_at").IsZero() {
		t.Errorf("the retry: %v", again.FieldsData())
	}
	if !f.fresh(skipped).GetDateTime("retry_at").IsZero() {
		t.Error("the skipped check is still to be tried again")
	}
	// once
	f.p.RunDueChecks(retry.Add(2*time.Hour + time.Minute))
	if rows := f.checks(f.target); len(rows) != 2 {
		t.Errorf("%d checks", len(rows))
	}
}

// TestAnAgentThatCannotCheckSkipsTheCheck (#31): an agent that says it has no
// backup.check gets no command: its check is skipped, saying to update it,
// with the warning that the target is not verified, and is not tried again.
// A check before it that is still going skips the next without an alert.
func TestAnAgentThatCannotCheckSkipsTheCheck(t *testing.T) {
	f := newCheckFixture(t)
	f.host.Set("capabilities", []string{"backup.init", "backup.run"})
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	slot := parseCronRule(farSchedule()).next(time.Now())
	f.target.Set("next_check_at", slot)
	if err := f.app.Save(f.target); err != nil {
		t.Fatal(err)
	}
	f.p.RunDueChecks(slot.Add(20 * time.Second))
	rows := f.checks(f.target)
	if len(rows) != 1 || rows[0].GetString("status") != schema.CheckSkipped || rows[0].GetString("error") != "The agent of node-1 cannot check repositories yet: update it." ||
		!rows[0].GetDateTime("retry_at").IsZero() || rows[0].GetString("command") != "" {
		t.Fatalf("checks: %v", rows)
	}
	if a := f.openAlert(alertCheckUnverified, f.target.Id); a == nil || a.GetString("severity") != schema.SeverityWarning || a.GetString("host") != f.host.Id ||
		a.GetString("message") != "The integrity check of b2 was skipped: the agent of node-1 cannot check repositories yet: update it." {
		t.Errorf("the alert: %v", a)
	}

	// the agent can now, but a check of the target is still going
	f.host.Set("capabilities", []string{"backup.check"})
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	if err := events.ResolveAlert(f.app, alertCheckUnverified, f.target.Id); err != nil {
		t.Fatal(err)
	}
	f.startedCheck(f.target, schema.RunManual)
	next := parseCronRule(farSchedule()).next(slot)
	f.target = f.fresh(f.target)
	f.target.Set("next_check_at", next)
	if err := f.app.Save(f.target); err != nil {
		t.Fatal(err)
	}
	f.p.RunDueChecks(next.Add(20 * time.Second))
	rows = f.checks(f.target)
	if len(rows) != 3 || rows[2].GetString("status") != schema.CheckSkipped || rows[2].GetString("error") != "The check before it was still going." ||
		!rows[2].GetDateTime("retry_at").IsZero() {
		t.Fatalf("checks: %d", len(rows))
	}
	if f.openAlert(alertCheckUnverified, f.target.Id) != nil {
		t.Error("a check still going raised an alert")
	}
}

// TestEveryEndOfACheckIsKept (#31, AC1): how a check ended, from its command
// and the agent's answer: what restic found, redacted, the target's summary,
// the group its next check reads, the retries of checks that did not
// complete, and the alerts.
func TestEveryEndOfACheckIsKept(t *testing.T) {
	f := newCheckFixture(t)
	started := types.NowDateTime().Add(-90 * time.Second)

	// ok: the group moves on, and nothing is alerted
	row, command := f.startedCheck(f.target, schema.RunSchedule)
	command.Set("started_at", started)
	f.p.backupCheckStarted(command)
	if running := f.fresh(row); running.GetString("status") != schema.CheckRunning || running.GetDateTime("started_at").String() != started.String() ||
		f.fresh(f.target).GetString("last_check_status") != schema.CheckRunning {
		t.Errorf("a check its agent started: %v", running.FieldsData())
	}
	ok := f.endCheck(row, command, map[string]any{"status": schema.CommandSucceeded, "started_at": started, "result": answer(t, backup.CheckResult{
		Status: backup.CheckStatusOK, Subset: "1/20", PacksRead: 118, PacksTotal: 2361, Notes: []string{"3 additional files were found in the repo"},
		Log: "no errors were found, repo-password-b2"})})
	if ok.GetString("status") != schema.CheckOK || ok.GetInt("packs_read") != 118 || ok.GetInt("packs_total") != 2361 || ok.GetInt("duration_ms") < 89000 ||
		strings.Contains(ok.GetString("log"), "repo-password") || ok.GetString("error") != "" || !ok.GetDateTime("retry_at").IsZero() ||
		!strings.Contains(ok.GetString("notes"), "additional files") {
		t.Errorf("ok: %v", ok.FieldsData())
	}
	target := f.fresh(f.target)
	if target.GetString("integrity_status") != schema.CheckOK || target.GetInt("integrity_group") != 2 || target.GetDateTime("integrity_at").IsZero() ||
		target.GetString("last_check") != row.Id || target.GetString("last_check_status") != schema.CheckOK {
		t.Errorf("the target after ok: %v", target.FieldsData())
	}
	if found := f.eventsOf("backup.check_ok"); len(found) != 1 || found[0].GetString("message") != "The integrity check of b2 found no errors in group 1/20." {
		t.Errorf("events: %v", found)
	}

	// failed: confirmed errors, with the target's secret hidden; the group
	// stays, to be read again
	row, command = f.startedCheck(f.target, schema.RunSchedule)
	if row.GetString("subset") != "2/20" {
		t.Errorf("the next check reads %s", row.GetString("subset"))
	}
	pack := strings.Repeat("b6", 32)
	failed := f.endCheck(row, command, map[string]any{"status": schema.CommandFailed, "started_at": started, "error": "restic found 2 errors in group 2/20",
		"result": answer(t, backup.CheckResult{Status: backup.CheckStatusFailed, Subset: "2/20", Confirmed: true, ResticCode: 1,
			Errors: []backup.CheckError{{Kind: backup.CheckErrorPack, ID: pack, Text: "pack " + pack + ": does not exist"},
				{Kind: backup.CheckErrorOther, Text: "error: load <data/" + pack[:8] + ">: the-secret-key"}},
			Repair: backup.CheckRepair{Packs: []string{pack}, Snapshots: true}})})
	if failed.GetString("status") != schema.CheckFailed || !failed.GetBool("confirmed") || failed.GetString("error") != "" ||
		strings.Contains(failed.GetString("errors"), "the-secret-key") || !strings.Contains(failed.GetString("repair"), pack) ||
		!failed.GetDateTime("retry_at").IsZero() {
		t.Errorf("failed: %v", failed.FieldsData())
	}
	target = f.fresh(f.target)
	if target.GetString("integrity_status") != schema.CheckFailed || target.GetInt("integrity_group") != 2 ||
		target.GetString("integrity_error") != "pack "+pack+": does not exist\nerror: load <data/"+pack[:8]+">: ***" {
		t.Errorf("the target after failed: %v", target.FieldsData())
	}
	if a := f.openAlert(alertCheckFailed, f.target.Id); a == nil || a.GetString("severity") != schema.SeverityCritical || a.GetString("host") != f.host.Id ||
		a.GetString("message") != "The integrity check of b2 found errors in group 2/20: Pack "+pack+": does not exist." {
		t.Errorf("the alert: %v", a)
	}
	if found := f.eventsOf("backup.check_failed"); len(found) != 1 || found[0].GetString("severity") != schema.SeverityCritical {
		t.Errorf("events: %v", found)
	}

	// suspect: pass 2 was locked out; tried again in an hour, and the
	// target is not verified
	row, command = f.startedCheck(f.target, schema.RunSchedule)
	lock := &backup.Lock{PID: 1, Host: "nas", User: "root", Exclusive: true, Created: time.Now().Add(-5 * time.Minute).UTC(), AgeS: 300, StorageID: "957548b2"}
	suspect := f.endCheck(row, command, map[string]any{"status": schema.CommandFailed, "started_at": started, "error": "unable to create lock in backend",
		"result": answer(t, backup.CheckResult{Status: backup.CheckStatusSuspect, Subset: "2/20", ResticCode: 11, Lock: lock,
			Error:  "repository is already locked exclusively by PID 1 on nas by root (UID 0, GID 0)",
			Errors: []backup.CheckError{{Kind: backup.CheckErrorPack, ID: pack, Text: "pack " + pack + ": does not exist"}}})})
	retry := suspect.GetDateTime("retry_at").Time()
	if suspect.GetString("status") != schema.CheckSuspect || suspect.GetString("error") != "Repository is already locked exclusively by PID 1 on nas by root (UID 0, GID 0)" ||
		!strings.Contains(suspect.GetString("lock"), `"host":"nas"`) || retry.Before(time.Now().Add(59*time.Minute)) || retry.After(time.Now().Add(time.Hour)) {
		t.Errorf("suspect: %v", suspect.FieldsData())
	}
	if a := f.openAlert(alertCheckUnverified, f.target.Id); a == nil || a.GetString("severity") != schema.SeverityWarning ||
		!strings.HasPrefix(a.GetString("message"), "The integrity check of b2 found errors in group 2/20 that it could not confirm with the repository locked: Repository is already locked") ||
		!strings.HasSuffix(a.GetString("message"), " PocketCloud checks again at "+retry.UTC().Format("15:04")+" UTC.") {
		t.Errorf("the alert: %v", a)
	}
	if target := f.fresh(f.target); target.GetString("integrity_status") != schema.CheckFailed || target.GetString("last_check_status") != schema.CheckSuspect ||
		!strings.HasPrefix(target.GetString("last_check_error"), "Repository is already locked") {
		t.Errorf("the target after suspect: %v", target.FieldsData())
	}

	// an ok check resolves both alerts
	row, command = f.startedCheck(f.target, schema.RunSchedule)
	f.endCheck(row, command, map[string]any{"status": schema.CommandSucceeded, "started_at": started,
		"result": answer(t, backup.CheckResult{Status: backup.CheckStatusOK, Subset: "2/20", Unconfirmed: true})})
	if f.openAlert(alertCheckFailed, f.target.Id) != nil || f.openAlert(alertCheckUnverified, f.target.Id) != nil {
		t.Error("an ok check did not resolve the alerts")
	}
	if target := f.fresh(f.target); target.GetString("integrity_status") != schema.CheckOK || target.GetInt("integrity_group") != 3 {
		t.Errorf("the target after ok: %v", target.FieldsData())
	}

	for _, test := range []struct {
		name, trigger string
		attempt       int
		fields        map[string]any
		connected     bool
		status, error string
		retried       bool
	}{
		{name: "stopped", trigger: schema.RunSchedule, fields: map[string]any{"status": schema.CommandCancelled, "started_at": started, "error": "Cancelled by admin@example.com"},
			status: schema.CheckCancelled, error: "Cancelled by admin@example.com"},
		{name: "cancelled before it started", trigger: schema.RunSchedule, fields: map[string]any{"status": schema.CommandCancelled, "error": "Cancelled by admin@example.com"},
			status: schema.CheckCancelled, error: "Cancelled by admin@example.com"},
		{name: "never delivered", trigger: schema.RunSchedule, fields: map[string]any{"status": schema.CommandTimedOut, "error": "The server stayed offline, so the command was never delivered."},
			status: schema.CheckSkipped, error: "It waited 6 hours while the server node-1 stayed offline, and never started.", retried: true},
		{name: "waited for a free worker", trigger: schema.RunSchedule, connected: true, fields: map[string]any{"status": schema.CommandCancelled,
			"error": "The command was cancelled while waiting for a free worker.", "expires_at": types.NowDateTime().Add(-time.Second)},
			status: schema.CheckSkipped, error: "It waited 6 hours while the server node-1 was busy with other checks, and never started."},
		{name: "ran out of time", trigger: schema.RunSchedule, fields: map[string]any{"status": schema.CommandTimedOut, "started_at": started, "error": "The agent did not finish the command in time."},
			status: schema.CheckError, error: "The check did not finish in 12 hours.", retried: true},
		{name: "an old agent", trigger: schema.RunSchedule, fields: map[string]any{"status": schema.CommandFailed, "error": "This agent does not support backup.check. Update the agent."},
			status: schema.CheckError, error: "This agent does not support backup.check. Update the agent.", retried: true},
		{name: "the agent restarted", trigger: schema.RunRetry, attempt: 5, fields: map[string]any{"status": schema.CommandFailed, "started_at": started,
			"error": "The agent restarted while running this command, so it may be incomplete."},
			status: schema.CheckError, error: "The agent restarted while running this command, so it may be incomplete.", retried: true},
		{name: "the last attempt", trigger: schema.RunRetry, attempt: 6, fields: map[string]any{"status": schema.CommandFailed, "started_at": started,
			"error": "a repository is there, and the password is not its own", "result": answer(t, backup.CheckResult{Status: backup.CheckStatusError, ResticCode: 12})},
			status: schema.CheckError, error: "A repository is there, and the password is not its own"},
		{name: "by hand", trigger: schema.RunManual, fields: map[string]any{"status": schema.CommandFailed, "started_at": started, "error": "Fatal: the-secret-key denied"},
			status: schema.CheckError, error: "Fatal: *** denied"},
	} {
		t.Run(test.name, func(t *testing.T) {
			stream := f.p.Commands.Connect(f.host.Id, []string{"backup.check"}, nil)
			if !test.connected {
				f.p.Commands.Disconnect(stream)
			} else {
				defer f.p.Commands.Disconnect(stream)
			}
			row, command := f.startedCheck(f.target, test.trigger)
			if test.attempt > 0 {
				row.Set("attempt", test.attempt)
				if err := f.app.Save(row); err != nil {
					t.Fatal(err)
				}
			}
			// the agent cancelled it once it had waited its time
			if _, ok := test.fields["expires_at"]; ok {
				command.Set("expires_at", test.fields["expires_at"])
			}
			ended := f.endCheck(row, command, test.fields)
			if ended.GetString("status") != test.status || ended.GetString("error") != test.error || ended.GetDateTime("retry_at").IsZero() == test.retried {
				t.Errorf("%v", ended.FieldsData())
			}
			if target := f.fresh(f.target); target.GetString("last_check_status") != test.status || target.GetString("integrity_status") != schema.CheckOK {
				t.Errorf("the target: %v", target.FieldsData())
			}
		})
	}
	// kept once: a second finish of the same command changes nothing
	row, command = f.startedCheck(f.target, schema.RunSchedule)
	f.endCheck(row, command, map[string]any{"status": schema.CommandSucceeded, "started_at": started, "result": answer(t, backup.CheckResult{Status: backup.CheckStatusOK, Subset: "3/20"})})
	command.Load(map[string]any{"status": schema.CommandFailed, "error": "late"})
	f.p.backupCheckFinished(command)
	if again := f.fresh(row); again.GetString("status") != schema.CheckOK || f.fresh(f.target).GetInt("integrity_group") != 4 {
		t.Errorf("finished twice: %v", again.FieldsData())
	}
}

// TestWhatACheckFoundFitsItsRecord (#31): JSON writes < and > in six bytes
// each, so 20 errors of 500 characters of restic's <data/…> may not fit the
// 16 KiB of the errors: the trailing ones are counted in errors_more instead,
// and the record is kept.
func TestWhatACheckFoundFitsItsRecord(t *testing.T) {
	f := newCheckFixture(t)
	found := []backup.CheckError{}
	for i := range backup.MaxCheckErrors {
		found = append(found, backup.CheckError{Kind: backup.CheckErrorOther, Text: fmt.Sprintf("%02d %s", i, strings.Repeat("<>", 248))})
	}
	packs := []string{}
	for i := range 200 {
		packs = append(packs, fmt.Sprintf("%064x", i))
	}
	row, command := f.startedCheck(f.target, schema.RunSchedule)
	failed := f.endCheck(row, command, map[string]any{"status": schema.CommandFailed, "started_at": types.NowDateTime().Add(-time.Minute),
		"result": answer(t, backup.CheckResult{Status: backup.CheckStatusFailed, Subset: "1/20", Confirmed: true, Errors: found, ErrorsMore: 7,
			Repair: backup.CheckRepair{Packs: packs}})})
	kept := checkErrors(failed)
	if failed.GetString("status") != schema.CheckFailed || len(kept) == 0 || len(kept) == len(found) ||
		failed.GetInt("errors_more") != 7+len(found)-len(kept) || len(failed.GetString("errors")) > maxCheckErrorsJSON ||
		len(failed.GetString("repair")) > maxCheckSmallJSON || kept[0].Text != found[0].Text {
		t.Errorf("%d errors kept, %d more, %d bytes, repair %d bytes", len(kept), failed.GetInt("errors_more"), len(failed.GetString("errors")), len(failed.GetString("repair")))
	}
}

// TestAStructureOnlyCheckKeepsItsGroup (#31): with 0 groups a check reads no
// data packs and leaves the target's group as it is; a group past the groups,
// as after an admin made them fewer, reads as the first.
func TestAStructureOnlyCheckKeepsItsGroup(t *testing.T) {
	f := newCheckFixture(t)
	f.checkSettings(farSchedule(), 0)
	f.target.Set("integrity_group", 7)
	if err := f.app.Save(f.target); err != nil {
		t.Fatal(err)
	}
	row, command := f.startedCheck(f.target, schema.RunManual)
	if row.GetString("subset") != "" || strings.Contains(fmt.Sprint(command.Get("payload")), "read_data_subset") && strings.Contains(fmt.Sprint(command.Get("payload")), "/") {
		t.Errorf("a structure-only check: %v %v", row.FieldsData(), command.Get("payload"))
	}
	f.endCheck(row, command, map[string]any{"status": schema.CommandSucceeded, "started_at": types.NowDateTime(), "result": answer(t, backup.CheckResult{Status: backup.CheckStatusOK})})
	if target := f.fresh(f.target); target.GetInt("integrity_group") != 7 || target.GetString("integrity_status") != schema.CheckOK {
		t.Errorf("the target: %v", target.FieldsData())
	}
	if found := f.eventsOf("backup.check_ok"); len(found) != 1 || found[0].GetString("message") != "The integrity check of b2 found no errors in its structure." {
		t.Errorf("events: %v", found)
	}
	f.checkSettings(farSchedule(), 5)
	row, command = f.startedCheck(f.target, schema.RunManual)
	if row.GetString("subset") != "1/5" {
		t.Errorf("group 7 of 5 reads %s", row.GetString("subset"))
	}
	f.endCheck(row, command, map[string]any{"status": schema.CommandSucceeded, "started_at": types.NowDateTime(), "result": answer(t, backup.CheckResult{Status: backup.CheckStatusOK, Subset: "5/5"})})
	if target := f.fresh(f.target); target.GetInt("integrity_group") != 1 {
		t.Errorf("after 5/5 the group is %d", target.GetInt("integrity_group"))
	}
}

// TestCheckAlertsGoWithWhatTheyAreAbout (#31): removing a target resolves its
// check alerts, and nobody else's; turning the checks off resolves every
// warning that a target is not verified, as nothing would resolve them, and
// drops the retries of checks; confirmed errors stay.
func TestCheckAlertsGoWithWhatTheyAreAbout(t *testing.T) {
	f := newCheckFixture(t)
	nas := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	for _, target := range []*core.Record{f.target, nas} {
		for _, rule := range []string{alertCheckFailed, alertCheckUnverified} {
			if err := events.RaiseAlert(f.app, rule, target.Id, f.host.Id, schema.SeverityWarning, "it went wrong"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if status, out := f.send(http.MethodDelete, "/api/pc/backup-targets/"+nas.Id, f.admin, nil); status != http.StatusNoContent {
		t.Fatalf("remove nas: %d %v", status, out)
	}
	if f.openAlert(alertCheckFailed, nas.Id) != nil || f.openAlert(alertCheckUnverified, nas.Id) != nil {
		t.Error("the alerts of a removed target are open")
	}
	if f.openAlert(alertCheckFailed, f.target.Id) == nil || f.openAlert(alertCheckUnverified, f.target.Id) == nil {
		t.Error("removing a target resolved another's alerts")
	}

	row, command := f.startedCheck(f.target, schema.RunSchedule)
	f.endCheck(row, command, map[string]any{"status": schema.CommandFailed, "started_at": types.NowDateTime(), "error": "boom"})
	if f.fresh(row).GetDateTime("retry_at").IsZero() {
		t.Fatal("no retry planned")
	}
	if status, out := f.send(http.MethodPut, "/api/pc/settings/backup-checks", f.admin, map[string]any{"schedule": ""}); status != http.StatusOK {
		t.Fatalf("turn the checks off: %d %v", status, out)
	}
	if f.openAlert(alertCheckUnverified, f.target.Id) != nil {
		t.Error("turning the checks off left a target unverified")
	}
	if f.openAlert(alertCheckFailed, f.target.Id) == nil {
		t.Error("turning the checks off resolved confirmed errors")
	}
	if !f.fresh(row).GetDateTime("retry_at").IsZero() {
		t.Error("a retry of a check stays while the checks are off")
	}
	if target := f.fresh(f.target); !target.GetDateTime("next_check_at").IsZero() {
		t.Errorf("a next check while the checks are off: %s", target.GetDateTime("next_check_at"))
	}
}

// TestTheWatchdogSaysWhatNoCheckVerified (#31, AC1): hourly, a target whose
// newest check that completed, or whose adding when none did, is older than
// twice the time between two slots and a day (15 days for a weekly schedule)
// raises the warning that it is not verified; a target added before the panel
// began to check counts from then; one with confirmed errors, one being
// added, and all while the checks are off, do not.
func TestTheWatchdogSaysWhatNoCheckVerified(t *testing.T) {
	f := newCheckFixture(t)
	f.checkSettings(schema.DefaultCheckSchedule, 20)
	now := time.Now().UTC()
	nas := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	never := f.addTarget("never", backup.KindS3, f.host, backup.Config{Endpoint: "https://never.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	broken := f.addTarget("broken", backup.KindS3, f.host, backup.Config{Endpoint: "https://broken.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	set := func(target *core.Record, fields map[string]any) {
		t.Helper()
		target.Load(fields)
		if err := f.app.Save(target); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-15*24*time.Hour - time.Hour)
	set(f.target, map[string]any{"integrity_status": schema.CheckOK, "integrity_at": old})
	set(nas, map[string]any{"integrity_status": schema.CheckOK, "integrity_at": now.Add(-14 * 24 * time.Hour)})
	set(broken, map[string]any{"integrity_status": schema.CheckFailed, "integrity_at": old})
	if _, err := f.app.DB().NewQuery("UPDATE backup_targets SET created={:at} WHERE id={:id}").
		Bind(map[string]any{"at": types.NowDateTime().Add(-16 * 24 * time.Hour).String(), "id": never.Id}).Execute(); err != nil {
		t.Fatal(err)
	}

	// a target the panel had before it could check, as at an upgrade, was
	// not unverified for all that time
	f.p.WatchChecks(now)
	if a := f.openAlert(alertCheckUnverified, never.Id); a != nil {
		t.Errorf("a target added before the checks began is not verified: %v", a)
	}
	began := now.Add(-15*24*time.Hour - 2*time.Hour)
	if _, err := f.app.DB().NewQuery("UPDATE _migrations SET applied={:at} WHERE file={:file}").
		Bind(map[string]any{"at": began.UnixMicro(), "file": checksMigration}).Execute(); err != nil {
		t.Fatal(err)
	}

	f.p.WatchChecks(now)
	if a := f.openAlert(alertCheckUnverified, f.target.Id); a == nil || a.GetString("severity") != schema.SeverityWarning ||
		a.GetString("message") != "No integrity check of b2 has completed since "+old.Format("2006-01-02 15:04")+" UTC." {
		t.Errorf("the alert of a target checked 15 days ago: %v", a)
	}
	if a := f.openAlert(alertCheckUnverified, never.Id); a == nil || a.GetString("message") != "No integrity check of never has completed since "+began.Format("2006-01-02 15:04")+" UTC." {
		t.Errorf("the alert of a target never checked: %v", a)
	}
	for _, target := range []*core.Record{nas, broken} {
		if f.openAlert(alertCheckUnverified, target.Id) != nil {
			t.Errorf("%s is not verified", target.GetString("name"))
		}
	}

	// off: nothing
	if err := events.ResolveAlert(f.app, alertCheckUnverified, f.target.Id); err != nil {
		t.Fatal(err)
	}
	f.checkSettings("", 20)
	f.p.WatchChecks(now)
	if f.openAlert(alertCheckUnverified, f.target.Id) != nil {
		t.Error("the watchdog alerted while the checks are off")
	}
}

// TestTheWatchdogGoesByTheLongestGap (#31): the watchdog's limit is twice the
// longest gap of the schedule and a day, whatever the weekday it runs on: a
// schedule of Saturdays and Sundays, checked on Sunday, is not unverified on
// Wednesday, nor a schedule of weekdays, checked on Friday, on Monday before
// its check; each is once two of its longest gaps and a day have passed.
func TestTheWatchdogGoesByTheLongestGap(t *testing.T) {
	f := newCheckFixture(t)
	at := func(day int, hour, minute int) time.Time {
		return time.Date(2026, time.November, day, hour, minute, 0, 0, time.UTC)
	}
	for _, test := range []struct {
		schedule       string
		checked, quiet time.Time
		unverified     time.Time
	}{
		// Sunday the 15th; Wednesday the 18th; 6 days twice and one
		{"0 5 * * 0,6", at(15, 5, 30), at(18, 5, 41), at(28, 5, 41)},
		// Friday the 20th; Monday the 23rd at 05:41; 3 days twice and one
		{"0 5 * * 1-5", at(20, 5, 30), at(23, 5, 41), at(27, 5, 41)},
		// the weekly default: 15 days
		{schema.DefaultCheckSchedule, at(1, 5, 30), at(16, 5, 29), at(16, 5, 41)},
	} {
		f.checkSettings(test.schedule, 20)
		f.target.Load(map[string]any{"integrity_status": schema.CheckOK, "integrity_at": test.checked})
		if err := f.app.Save(f.target); err != nil {
			t.Fatal(err)
		}
		f.p.WatchChecks(test.quiet)
		if a := f.openAlert(alertCheckUnverified, f.target.Id); a != nil {
			t.Errorf("%s, checked %s, watched %s: %v", test.schedule, test.checked, test.quiet, a.GetString("message"))
		}
		f.p.WatchChecks(test.unverified)
		if a := f.openAlert(alertCheckUnverified, f.target.Id); a == nil ||
			a.GetString("message") != "No integrity check of b2 has completed since "+test.checked.Format("2006-01-02 15:04")+" UTC." {
			t.Errorf("%s, checked %s, watched %s: %v", test.schedule, test.checked, test.unverified, a)
		}
		if err := events.ResolveAlert(f.app, alertCheckUnverified, f.target.Id); err != nil {
			t.Fatal(err)
		}
	}
}

// TestANewTargetKnowsItsNextCheck (#31): a target has its next check as it
// is added, before the scheduler's next pass; none while the checks are off.
func TestANewTargetKnowsItsNextCheck(t *testing.T) {
	f := newCheckFixture(t)
	slot := parseCronRule(farSchedule()).next(time.Now())
	added := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	if next := f.fresh(added).GetDateTime("next_check_at").Time(); !next.Equal(slot) {
		t.Errorf("the next check of a new target: %s, not %s", next, slot)
	}
	f.checkSettings("", 20)
	off := f.addTarget("off", backup.KindS3, f.host, backup.Config{Endpoint: "https://off.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	if next := f.fresh(off).GetDateTime("next_check_at"); !next.IsZero() {
		t.Errorf("the next check of a target added while the checks are off: %s", next)
	}
}

// TestOldChecksGo (#31): checks older than 180 days go, but each target's
// newest, and its newest that completed.
func TestOldChecksGo(t *testing.T) {
	f := newCheckFixture(t)
	nas := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	check := func(target *core.Record, status string, age time.Duration) *core.Record {
		t.Helper()
		row := f.save(schema.BackupChecks, map[string]any{"target": target.Id, "host": f.host.Id, "kind": schema.CheckIntegrity, "trigger": schema.RunSchedule, "status": status})
		if _, err := f.app.DB().NewQuery("UPDATE backup_checks SET created={:at} WHERE id={:id}").
			Bind(map[string]any{"at": types.NowDateTime().Add(-age).String(), "id": row.Id}).Execute(); err != nil {
			t.Fatal(err)
		}
		return row
	}
	day := 24 * time.Hour
	oldOK := check(f.target, schema.CheckOK, 200*day)
	newerOK := check(f.target, schema.CheckOK, 190*day)
	oldError := check(f.target, schema.CheckError, 185*day)
	newest := check(f.target, schema.CheckError, 181*day)
	recent := check(nas, schema.CheckOK, 10*day)
	nasOld := check(nas, schema.CheckFailed, 300*day)
	f.p.pruneBackupChecks()
	for row, kept := range map[*core.Record]bool{oldOK: false, newerOK: true, oldError: false, newest: true, recent: true, nasOld: false} {
		if _, err := f.app.FindRecordById(schema.BackupChecks, row.Id); (err == nil) != kept {
			t.Errorf("check %s (%s) kept: %v", row.Id, row.GetString("status"), err == nil)
		}
	}
}

// TestTheChecksSettingsAreHeldToTheirRules (#31): admins set the schedule and
// the groups, through the route or the record API, which hold both to the
// same rules; the change is audited as the admin, and each target's next
// check follows it. Operators and viewers cannot.
func TestTheChecksSettingsAreHeldToTheirRules(t *testing.T) {
	f := newCheckFixture(t)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	_, viewer := testutil.NewUser(t, f.app, "viewer@example.com", schema.RoleViewer)
	for _, token := range []string{operator, viewer} {
		if status, _ := f.send(http.MethodPut, "/api/pc/settings/backup-checks", token, map[string]any{"schedule": "0 3 * * *"}); status != http.StatusForbidden {
			t.Errorf("a non-admin set the checks: %d", status)
		}
	}
	for _, test := range []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"schedule": "every sunday"}, checkScheduleProblem},
		{map[string]any{"schedule": "0 5 * *"}, checkScheduleProblem},
		{map[string]any{"schedule": "0 5,6 * * 0"}, checkOftenProblem},
		{map[string]any{"schedule": "*/30 * * * *"}, checkOftenProblem},
		// 23 hours, then one
		{map[string]any{"schedule": "0 0,23 * * *"}, checkOftenProblem},
		{map[string]any{"schedule": "0 0 30 2 *"}, checkNeverProblem},
		{map[string]any{"groups": 2.5}, checkGroupsProblem},
		{map[string]any{"groups": -1}, checkGroupsProblem},
		{map[string]any{"groups": 257}, checkGroupsProblem},
	} {
		status, out := f.send(http.MethodPut, "/api/pc/settings/backup-checks", f.admin, test.body)
		if status != http.StatusBadRequest || out["message"] != test.want {
			t.Errorf("%v: %d %v", test.body, status, out["message"])
		}
	}
	countAudit := func() int {
		t.Helper()
		entries, err := f.app.FindRecordsByFilter(schema.AuditLog, "collection={:c}", "", 0, 0, map[string]any{"c": schema.Settings})
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	audited := countAudit()
	status, out := f.send(http.MethodPut, "/api/pc/settings/backup-checks", f.admin, map[string]any{"schedule": "  15  4 * * 1 ", "groups": 0})
	next := parseCronRule("15 4 * * 1").next(time.Now())
	if status != http.StatusOK || out["schedule"] != "15 4 * * 1" || out["groups"] != float64(0) || out["next_check_at"] != next.Format(time.RFC3339) {
		t.Fatalf("set: %d %v", status, out)
	}
	if settings, _ := f.p.settingsRecord(); settings.GetString("backup_check_schedule") != "15 4 * * 1" || settings.GetInt("backup_check_groups") != 0 {
		t.Errorf("the settings: %v", settings.FieldsData())
	}
	if countAudit() != audited+1 {
		t.Errorf("%d audit entries of the settings, not %d", countAudit(), audited+1)
	}
	if entries, err := f.app.FindRecordsByFilter(schema.AuditLog, "collection={:c}", "-created,-id", 1, 0, map[string]any{"c": schema.Settings}); err != nil || len(entries) != 1 ||
		entries[0].GetString("actor_kind") != audit.KindUser || entries[0].GetString("actor_label") != "admin@example.com" {
		t.Errorf("the audit entry of the settings: %v %v", entries, err)
	}
	if target := f.fresh(f.target); !target.GetDateTime("next_check_at").Time().Equal(next) {
		t.Errorf("the next check: %s, not %s", target.GetDateTime("next_check_at"), next)
	}
	// a field left out stays
	if status, out := f.send(http.MethodPut, "/api/pc/settings/backup-checks", f.admin, map[string]any{"groups": 20}); status != http.StatusOK || out["schedule"] != "15 4 * * 1" {
		t.Errorf("groups only: %d %v", status, out)
	}

	// the record API, which admins may use
	settings, err := f.p.settingsRecord()
	if err != nil {
		t.Fatal(err)
	}
	url := "/api/collections/settings/records/" + settings.Id
	for body, want := range map[string]string{
		`{"backup_check_schedule":"* * * * *"}`: checkOftenProblem,
		`{"backup_check_schedule":"nope"}`:      checkScheduleProblem,
		`{"backup_check_groups":300}`:           checkGroupsProblem,
	} {
		status, out := testutil.Do(t, f.handler, testutil.Request{Method: http.MethodPatch, URL: url, Token: f.admin, Body: body})
		if status != http.StatusBadRequest || !strings.Contains(out, want) {
			t.Errorf("%s through the record API: %d %s", body, status, out)
		}
	}
	if status, out := testutil.Do(t, f.handler, testutil.Request{Method: http.MethodPatch, URL: url, Token: f.admin, Body: `{"backup_check_schedule":" 0 2  * * 3"}`}); status != http.StatusOK {
		t.Fatalf("a schedule through the record API: %d %s", status, out)
	}
	if settings, _ := f.p.settingsRecord(); settings.GetString("backup_check_schedule") != "0 2 * * 3" {
		t.Errorf("the schedule: %q", settings.GetString("backup_check_schedule"))
	}
	if target := f.fresh(f.target); !target.GetDateTime("next_check_at").Time().Equal(parseCronRule("0 2 * * 3").next(time.Now())) {
		t.Errorf("the next check after the record API: %s", target.GetDateTime("next_check_at"))
	}
	// what else the settings hold is changed as before
	if status, out := testutil.Do(t, f.handler, testutil.Request{Method: http.MethodPatch, URL: url, Token: f.admin, Body: `{"audit_retention_days":90}`}); status != http.StatusOK {
		t.Errorf("another setting: %d %s", status, out)
	}
}

// TestAChecksRecordsAreNotAudited (#31): a check, and the summary it keeps on
// its target, write nothing to the audit log; a target with a check under
// way is not removed.
func TestAChecksRecordsAreNotAudited(t *testing.T) {
	f := newCheckFixture(t)
	count := func() int {
		t.Helper()
		entries, err := f.app.FindAllRecords(schema.AuditLog)
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	before := count()
	row, command := f.startedCheck(f.target, schema.RunSchedule)
	if status, out := f.send(http.MethodDelete, "/api/pc/backup-targets/"+f.target.Id, f.admin, nil); status != http.StatusConflict ||
		out["message"] != "An integrity check of it is under way: stop it first, or wait." {
		t.Errorf("remove a target with a check under way: %d %v", status, out)
	}
	command.Set("started_at", types.NowDateTime())
	f.p.backupCheckStarted(command)
	f.endCheck(row, command, map[string]any{"status": schema.CommandSucceeded, "started_at": types.NowDateTime(), "result": answer(t, backup.CheckResult{Status: backup.CheckStatusOK, Subset: "1/20"})})
	f.p.refreshNextChecks(time.Now())
	if after := count(); after != before {
		t.Errorf("%d audit entries for a check", after-before)
	}
	if status, _ := f.send(http.MethodDelete, "/api/pc/backup-targets/"+f.target.Id, f.admin, nil); status != http.StatusNoContent {
		t.Errorf("remove a target once its check ended: %d", status)
	}
	if rows := f.checks(f.target); len(rows) != 0 {
		t.Errorf("%d checks of a removed target", len(rows))
	}
}

// TestCheckingATargetNow (#31, AC1): admins check a target now; the route
// refuses a target being added, an offline server, an agent that cannot
// check, and a check under way; it answers the check and its command at
// once, and logs that it started. Operators and viewers cannot.
func TestCheckingATargetNow(t *testing.T) {
	f := newCheckFixture(t)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	_, viewer := testutil.NewUser(t, f.app, "viewer@example.com", schema.RoleViewer)
	url := "/api/pc/backup-targets/" + f.target.Id + "/check"
	for _, token := range []string{operator, viewer} {
		if status, _ := f.send(http.MethodPost, url, token, nil); status != http.StatusForbidden {
			t.Errorf("a non-admin checked a target: %d", status)
		}
	}
	if status, out := f.send(http.MethodPost, "/api/pc/backup-targets/nope/check", f.admin, nil); status != http.StatusNotFound || out["message"] != "The target does not exist." {
		t.Errorf("a target that does not exist: %d %v", status, out)
	}
	pending := f.addTarget("new", backup.KindS3, f.host, backup.Config{Endpoint: "https://new.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	pending.Set("status", schema.TargetPending)
	pending.Set("repo_id", "")
	if err := f.app.Save(pending); err != nil {
		t.Fatal(err)
	}
	if status, out := f.send(http.MethodPost, "/api/pc/backup-targets/"+pending.Id+"/check", f.admin, nil); status != http.StatusConflict ||
		out["message"] != "The target new is not checked yet: it has no repository." {
		t.Errorf("a target being added: %d %v", status, out)
	}
	if status, out := f.send(http.MethodPost, url, f.admin, nil); status != http.StatusConflict || out["message"] != "The server node-1 is offline: its agent checks the target." {
		t.Errorf("an offline server: %d %v", status, out)
	}
	stream := f.p.Commands.Connect(f.host.Id, []string{"backup.init", "backup.run", "backup.check"}, nil)
	defer f.p.Commands.Disconnect(stream)
	f.host.Set("capabilities", []string{"backup.init", "backup.run"})
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	if status, out := f.send(http.MethodPost, url, f.admin, nil); status != http.StatusConflict || out["message"] != "The agent of node-1 cannot check repositories yet: update it." {
		t.Errorf("an agent that cannot check: %d %v", status, out)
	}
	f.host.Set("capabilities", []string{"backup.init", "backup.run", "backup.check"})
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	status, out := f.send(http.MethodPost, url, f.admin, nil)
	if status != http.StatusAccepted || out["check"] == "" || out["command"] == "" {
		t.Fatalf("check now: %d %v", status, out)
	}
	rows := f.checks(f.target)
	if len(rows) != 1 || rows[0].Id != out["check"] || rows[0].GetString("command") != out["command"] || rows[0].GetString("trigger") != schema.RunManual ||
		!rows[0].GetDateTime("scheduled_at").IsZero() {
		t.Fatalf("checks: %v", rows)
	}
	if command, err := f.app.FindRecordById(schema.Commands, rows[0].GetString("command")); err != nil || command.GetString("requested_label") != "admin@example.com" {
		t.Errorf("the command: %v %v", command, err)
	}
	if found := f.eventsOf("backup.check_started"); len(found) != 1 || found[0].GetString("severity") != schema.SeverityInfo ||
		found[0].GetString("message") != "admin@example.com started the integrity check of b2." {
		t.Errorf("events: %v", found)
	}
	if status, out := f.send(http.MethodPost, url, f.admin, nil); status != http.StatusConflict || out["message"] != "An integrity check of b2 is under way." {
		t.Errorf("a second check: %d %v", status, out)
	}
	if rows := f.checks(f.target); len(rows) != 1 {
		t.Errorf("%d checks after a refused one", len(rows))
	}
}

// TestACheckGetsItsTargetsSecretsOnItsServerOnly (#31): the enricher gives a
// check its target's secrets on the target's own server, and on no other.
func TestACheckGetsItsTargetsSecretsOnItsServerOnly(t *testing.T) {
	f := newCheckFixture(t)
	_, command := f.startedCheck(f.target, schema.RunManual)
	delivered, err := f.deliver(command)
	if err != nil || delivered.GetBackupCheck().GetRepository().GetPassword() != "repo-password-b2" || delivered.GetBackupCheck().GetReadDataSubset() != "1/20" {
		t.Errorf("on its server: %v %v", delivered, err)
	}
	other := testutil.NewHost(t, f.app, "node-2")
	moved := f.fresh(command)
	moved.Set("host", other.Id)
	if err := f.app.Save(moved); err != nil {
		t.Fatal(err)
	}
	delivered, err = f.deliver(moved)
	if err == nil || err.Error() != "the backup target is reached from another server" || delivered.GetBackupCheck().GetRepository().GetPassword() != "" {
		t.Errorf("on another server: %v %v", delivered, err)
	}
}

// TestAChecksEndThePanelMissedIsKept (#31): a check whose command ended
// without its finisher, as when the panel stopped in between, is kept from its
// command at the scheduler's next pass; one whose command is gone did not
// complete, and is tried again as such a check is.
func TestAChecksEndThePanelMissedIsKept(t *testing.T) {
	f := newCheckFixture(t)
	row, command := f.startedCheck(f.target, schema.RunSchedule)
	f.closeCommand(command, schema.CommandDispatched)
	f.closeCommand(command, schema.CommandRunning)
	ended := f.fresh(command)
	ended.Load(map[string]any{"status": schema.CommandSucceeded, "started_at": types.NowDateTime().Add(-time.Minute), "finished_at": types.NowDateTime(),
		"result": answer(t, backup.CheckResult{Status: backup.CheckStatusOK, Subset: "1/20"})})
	if err := f.app.Save(ended); err != nil {
		t.Fatal(err)
	}
	if f.fresh(row).GetString("status") != schema.CheckQueued {
		t.Fatal("the check was kept without the panel")
	}
	f.p.RunDueChecks(time.Now())
	if kept := f.fresh(row); kept.GetString("status") != schema.CheckOK {
		t.Errorf("a check whose command ended: %v", kept.FieldsData())
	}
	if target := f.fresh(f.target); target.GetString("integrity_status") != schema.CheckOK || target.GetInt("integrity_group") != 2 {
		t.Errorf("the target: %v", target.FieldsData())
	}

	row, command = f.startedCheck(f.target, schema.RunSchedule)
	if err := f.app.Delete(f.fresh(command)); err != nil {
		t.Fatal(err)
	}
	f.p.RunDueChecks(time.Now())
	lost := f.fresh(row)
	if lost.GetString("status") != schema.CheckError || lost.GetString("error") != "The check ended without the panel recording how." ||
		lost.GetDateTime("retry_at").IsZero() || lost.GetDateTime("finished_at").IsZero() {
		t.Errorf("a check whose command is gone: %v", lost.FieldsData())
	}
	if a := f.openAlert(alertCheckUnverified, f.target.Id); a == nil ||
		!strings.HasPrefix(a.GetString("message"), "The integrity check of b2 did not complete: The check ended without the panel recording how.") {
		t.Errorf("its alert: %v", a)
	}
	// once
	f.p.RunDueChecks(time.Now())
	if found := f.eventsOf("backup.check_unverified"); len(found) != 1 {
		t.Errorf("%d events", len(found))
	}
}

// testRow saves a restore test (#53) of a job, from a target, as the panel
// keeps one, with the fields given.
func (f *jobFixture) testRow(job, target *core.Record, fields map[string]any) *core.Record {
	f.t.Helper()
	all := map[string]any{"target": target.Id, "host": job.GetString("host"), "job": job.Id, "kind": schema.CheckRestoreTest,
		"trigger": schema.RunSchedule, "status": schema.CheckQueued, "database_name": "shop", "snapshot_id": snapshotID}
	for key, value := range fields {
		all[key] = value
	}
	return f.save(schema.BackupChecks, all)
}

// setTest stores fields of a check, as the panel would.
func (f *jobFixture) setTest(row *core.Record, fields map[string]any) {
	f.t.Helper()
	row.Load(fields)
	if err := f.app.Save(row); err != nil {
		f.t.Fatal(err)
	}
}

// TestARestoreTestIsNoIntegrityCheck (#53, D21): the five places of #31 that
// read every check as an integrity check read integrity checks only. A
// restore test never changes its target's integrity or its newest check; the
// checks' scheduler never settles it, never tries it again as an integrity
// check, and turning the checks off leaves its retry; and it shares a target
// and a slot with an integrity check.
func TestARestoreTestIsNoIntegrityCheck(t *testing.T) {
	f := newCheckFixture(t)
	job := f.job("db-main-shop", map[string]any{"origin": schema.JobDatabase, "source_kind": schema.SourceDatabase, "source": "shop", "engine": schema.DumpPostgres})
	slot := parseCronRule(farSchedule()).next(time.Now())

	// an integrity check and a restore test share the target and the slot
	check, err := f.p.startCheck(f.target, checkStart{trigger: schema.RunSchedule, scheduledAt: slot, requester: "schedule", now: slot})
	if err != nil {
		t.Fatal(err)
	}
	test := f.testRow(job, f.target, map[string]any{"scheduled_at": slot})
	if _, err := f.p.startCheck(f.target, checkStart{trigger: schema.RunSchedule, scheduledAt: slot, requester: "schedule", now: slot}); !errors.Is(err, errCheckTaken) {
		t.Errorf("a second integrity check of the slot: %v", err)
	}

	// a newer restore test that passed is no check of the target's integrity
	finished := time.Now().UTC()
	f.setTest(test, map[string]any{"status": schema.CheckOK, "finished_at": finished})
	f.p.noteTargetCheck(f.target.Id, f.fresh(test))
	target := f.fresh(f.target)
	if target.GetString("last_check") != check.Id || target.GetString("last_check_status") != schema.CheckQueued ||
		!target.GetDateTime("integrity_at").IsZero() || target.GetString("integrity_status") != "" {
		t.Errorf("a restore test changed the target's summary: %v", target.FieldsData())
	}

	// the checks' scheduler neither settles a test whose command is gone, nor
	// tries one again, and turning the checks off leaves its retry
	open := f.testRow(job, f.target, map[string]any{"status": schema.CheckRunning})
	retry := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	due := f.testRow(job, f.target, map[string]any{"status": schema.CheckError, "attempt": 1, "retry_at": retry, "finished_at": finished})
	f.p.RunDueChecks(time.Now())
	if kept := f.fresh(open); kept.GetString("status") != schema.CheckRunning || kept.GetString("error") != "" {
		t.Errorf("the checks' scheduler settled a restore test: %v", kept.FieldsData())
	}
	if kept := f.fresh(due); !kept.GetDateTime("retry_at").Time().Equal(retry) {
		t.Errorf("the checks' scheduler took a restore test's retry: %v", kept.FieldsData())
	}
	integrity, err := f.app.FindRecordsByFilter(schema.BackupChecks, "kind={:kind}", "", 0, 0, map[string]any{"kind": schema.CheckIntegrity})
	if err != nil || len(integrity) != 1 {
		t.Errorf("%d integrity checks after the pass (%v)", len(integrity), err)
	}
	f.p.checksTurnedOff()
	if kept := f.fresh(due); !kept.GetDateTime("retry_at").Time().Equal(retry) {
		t.Errorf("turning the checks off took a restore test's retry: %v", kept.FieldsData())
	}

	// an integrity check's finisher leaves a restore test alone, whatever
	// command it names
	command := f.save(schema.Commands, map[string]any{"host": f.host.Id, "type": "backup.check",
		"payload": map[string]any{"repository": map[string]any{"target_id": f.target.Id, "url": "s3:x"}}})
	linked := f.testRow(job, f.target, map[string]any{"status": schema.CheckRunning, "command": command.Id})
	f.closeCommand(command, schema.CommandSucceeded)
	f.p.backupCheckFinished(f.fresh(command))
	if kept := f.fresh(linked); kept.GetString("status") != schema.CheckRunning {
		t.Errorf("an integrity check's finisher kept a restore test: %v", kept.FieldsData())
	}
}

// TestOldRestoreTestsGoPerKindAndJob (#53, D21): old checks go per kind and,
// for restore tests, per job: a target keeps its newest integrity check
// whatever tests came after it, and each job its newest test.
func TestOldRestoreTestsGoPerKindAndJob(t *testing.T) {
	f := newCheckFixture(t)
	shop := f.job("db-main-shop", map[string]any{"origin": schema.JobDatabase, "source_kind": schema.SourceDatabase, "source": "shop", "engine": schema.DumpPostgres})
	blog := f.job("db-main-blog", map[string]any{"origin": schema.JobDatabase, "source_kind": schema.SourceDatabase, "source": "blog", "engine": schema.DumpPostgres})
	aged := func(row *core.Record, age time.Duration) *core.Record {
		t.Helper()
		if _, err := f.app.DB().NewQuery("UPDATE backup_checks SET created={:at} WHERE id={:id}").
			Bind(map[string]any{"at": types.NowDateTime().Add(-age).String(), "id": row.Id}).Execute(); err != nil {
			t.Fatal(err)
		}
		return row
	}
	day := 24 * time.Hour
	integrity := aged(f.save(schema.BackupChecks, map[string]any{"target": f.target.Id, "host": f.host.Id, "kind": schema.CheckIntegrity,
		"trigger": schema.RunSchedule, "status": schema.CheckOK}), 300*day)
	shopOld := aged(f.testRow(shop, f.target, map[string]any{"status": schema.CheckOK}), 250*day)
	shopNewest := aged(f.testRow(shop, f.target, map[string]any{"status": schema.CheckOK}), 200*day)
	blogNewest := aged(f.testRow(blog, f.target, map[string]any{"status": schema.CheckFailed}), 260*day)
	f.p.pruneBackupChecks()
	for row, kept := range map[*core.Record]bool{integrity: true, shopOld: false, shopNewest: true, blogNewest: true} {
		if _, err := f.app.FindRecordById(schema.BackupChecks, row.Id); (err == nil) != kept {
			t.Errorf("check %s (%s %s) kept: %v", row.Id, row.GetString("kind"), row.GetString("job"), err == nil)
		}
	}
}
