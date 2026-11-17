package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/agent"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

// Integrity checks, locks and retries (#31) end to end: the panel's scheduler,
// routes and finishers with a real agent, whose engine runs the backup tool
// with the fake restic. The fake's FAKE_RESTIC_CHECK finds errors,
// FAKE_RESTIC_LOCKED_BACKUP and FAKE_RESTIC_LOCKED_CHECK find the repository
// locked by the holder FAKE_RESTIC_LOCK_HOST names, as old as
// FAKE_RESTIC_LOCK_AGE, and FAKE_RESTIC_BACKUP_FAIL fails a backup.

// checksHarness is the jobs harness with users of each role. The test passes
// the scheduler of the checks and of the backups itself, at the moments it
// names: the panel's own passes, at each real minute, would move the targets'
// next checks back to the real next slot meanwhile.
type checksHarness struct {
	*jobsHarness
	operator string
	viewer   string
}

func newChecksHarness(t *testing.T, prepare func(engine *fakedocker.Engine)) *checksHarness {
	t.Helper()
	c := &checksHarness{jobsHarness: newJobsHarness(t, prepare)}
	c.app.Cron().Remove("pocketcloud_backup_checks")
	c.app.Cron().Remove("pocketcloud_backups")
	// after the panel's own finisher, which keeps what the check found
	c.panel.Commands.OnFinish("backup.check", func(command *core.Record) { c.finished.Store(command.Id, true) })
	_, c.operator = testutil.NewUser(t, c.app, "operator@example.com", schema.RoleOperator)
	_, c.viewer = testutil.NewUser(t, c.app, "viewer@example.com", schema.RoleViewer)
	return c
}

// weeklySlot is the next slot of the default schedule of the checks, Sundays
// at 05:00 UTC, after now.
func weeklySlot(now time.Time) time.Time {
	now = now.UTC()
	slot := time.Date(now.Year(), now.Month(), now.Day(), 5, 0, 0, 0, time.UTC)
	for slot.Weekday() != time.Sunday || !slot.After(now) {
		slot = slot.AddDate(0, 0, 1)
	}
	return slot
}

// farSchedule is a schedule of the checks whose next slot is a month or two
// away, and that slot: a check's retry, an hour or more later, never meets
// it.
func farSchedule(now time.Time) (string, time.Time) {
	now = now.UTC()
	month := time.Month((int(now.Month())+1)%12 + 1)
	slot := time.Date(now.Year(), month, 1, 5, 0, 0, 0, time.UTC)
	if !slot.After(now) {
		slot = slot.AddDate(1, 0, 0)
	}
	return fmt.Sprintf("0 5 1 %d *", int(month)), slot
}

// fresh reads a record again.
func (c *checksHarness) fresh(record *core.Record) *core.Record {
	c.t.Helper()
	found, err := c.app.FindRecordById(record.Collection().Name, record.Id)
	if err != nil {
		c.t.Fatal(err)
	}
	return found
}

// checksOf are a target's checks, oldest first.
func (c *checksHarness) checksOf(target *core.Record) []*core.Record {
	c.t.Helper()
	rows, err := c.app.FindRecordsByFilter(schema.BackupChecks, "target={:target}", "created,id", 0, 0, dbx.Params{"target": target.Id})
	if err != nil {
		c.t.Fatal(err)
	}
	return rows
}

// checkEnded waits until a check has ended, and its command's finisher, which
// keeps the target's summary and alerts after it keeps the check, is done.
func (c *checksHarness) checkEnded(id string) *core.Record {
	c.t.Helper()
	var row *core.Record
	c.waitFor("the check "+id+" to end", func() bool {
		found, err := c.app.FindRecordById(schema.BackupChecks, id)
		if err != nil {
			return false
		}
		row = found
		if s := found.GetString("status"); s == schema.CheckQueued || s == schema.CheckRunning {
			return false
		}
		if command := found.GetString("command"); command != "" {
			_, done := c.finished.Load(command)
			return done
		}
		return true
	})
	return row
}

// checkNow asks for a check of a target now, as the token's user, and is the
// answer.
func (c *checksHarness) checkNow(target *core.Record, token string) (int, map[string]any) {
	c.t.Helper()
	return c.send(http.MethodPost, "/api/pc/backup-targets/"+target.Id+"/check", token, nil)
}

// checkedNow checks a target now as the admin, and is the check once it
// ended.
func (c *checksHarness) checkedNow(target *core.Record) *core.Record {
	c.t.Helper()
	status, out := c.checkNow(target, c.admin)
	if status != http.StatusAccepted {
		c.t.Fatalf("check %s now: %d %v", target.GetString("name"), status, out)
	}
	return c.checkEnded(out["check"].(string))
}

// retry tries a run again through the route, as the token's user.
func (c *checksHarness) retry(runID, token string) (int, map[string]any) {
	c.t.Helper()
	return c.send(http.MethodPost, "/api/pc/backup-runs/"+runID+"/retry", token, nil)
}

// isCheck tells a check's job from the others.
func isCheck(job fakedocker.Job) bool {
	return len(job.Cmd) > 1 && job.Cmd[0] == "backup-tool" && job.Cmd[1] == "check"
}

// passOf is the pass a check's job runs: free or confirm.
func passOf(job fakedocker.Job) string {
	if i := slices.Index(job.Cmd, "--pass"); i >= 0 && i+1 < len(job.Cmd) {
		return job.Cmd[i+1]
	}
	return ""
}

// checkJobsOf are the jobs of a check's command, in the order they ran.
func (c *checksHarness) checkJobsOf(commandID string) []fakedocker.Job {
	jobs := []fakedocker.Job{}
	for _, job := range c.ranJobs() {
		if isCheck(job) && job.Labels["pocketcloud.command"] == commandID {
			jobs = append(jobs, job)
		}
	}
	return jobs
}

// holdChecks holds the check jobs until release is called, or until the
// agent stops their container, as restic gets the signal; entered tells
// when one waits.
func (c *checksHarness) holdChecks() (entered <-chan fakedocker.Job, release func()) {
	c.t.Helper()
	in := make(chan fakedocker.Job, 8)
	gate := make(chan struct{})
	var once sync.Once
	run := c.docker.OnJob
	c.docker.OnJob = func(job fakedocker.Job) fakedocker.JobResult {
		if isCheck(job) {
			in <- job
			select {
			case <-gate:
			case <-job.Stop:
			}
		}
		return run(job)
	}
	release = func() { once.Do(func() { close(gate) }) }
	c.t.Cleanup(release)
	return in, release
}

// resticCalls are the fake restic's calls, each its arguments.
func resticCalls(t *testing.T, root string) []string {
	t.Helper()
	calls := []string{}
	for line := range strings.SplitSeq(fakerestic.Calls(t, root), "\n") {
		if args, ok := strings.CutPrefix(line, "restic "); ok {
			calls = append(calls, args)
		}
	}
	return calls
}

// resticCommand is restic's command in a call's arguments, after the global
// options.
func resticCommand(call string) string {
	args := strings.Fields(call)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--no-cache", "--no-lock", "--cleanup-cache":
		case "-o", "--cache-dir", "--retry-lock":
			i++
		default:
			return args[i]
		}
	}
	return ""
}

// noRemoveAll fails the test when restic or a job was ever told to remove
// every lock: a retry runs plain restic unlock, which leaves a live lock.
func (c *checksHarness) noRemoveAll() {
	c.t.Helper()
	if calls := fakerestic.Calls(c.t, c.restic); strings.Contains(calls, "remove-all") {
		c.t.Errorf("restic was told to remove every lock:\n%s", calls)
	}
	for _, job := range c.ranJobs() {
		if strings.Contains(strings.Join(job.Cmd, " "), "remove-all") {
			c.t.Errorf("a job was told to remove every lock: %v", job.Cmd)
		}
	}
}

// openAlert is the open alert of a rule and key, or nil.
func (c *checksHarness) openAlert(rule, key string) *core.Record {
	c.t.Helper()
	return c.openAlerts(rule)[key]
}

// payloadOf is a command's payload, as the panel stored it.
func payloadOf(t *testing.T, command *core.Record) map[string]any {
	t.Helper()
	payload := map[string]any{}
	raw, err := json.Marshal(command.Get("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// lockOf is the lock a run or a check keeps.
func lockOf(t *testing.T, record *core.Record) *backup.Lock {
	t.Helper()
	var lock *backup.Lock
	if err := record.UnmarshalJSONField("lock", &lock); err != nil {
		t.Fatalf("the lock of %s: %v", record.Id, err)
	}
	return lock
}

// near tells whether two moments are less than a second apart: the records
// keep milliseconds, and the routes answer in seconds.
func near(a, b time.Time) bool {
	d := a.Sub(b)
	return d > -time.Second && d < time.Second
}

// TestAWeeklyCheckReadsEachTargetsNextGroup (#31, AC1): at the slot of the
// default schedule, Sundays at 05:00 UTC, each target gets one check on its
// own server, with its own ordering key and the first of 20 groups. Each
// check reads the group without a lock, in a job whose local repository is
// bound read-only and that is named after its command; restic finds nothing,
// so no second pass runs, and the target says it is fine, that its next
// check reads group 2, and when that is. A second pass at the slot starts
// nothing. Check now, for admins only, checks the next group at once.
func TestAWeeklyCheckReadsEachTargetsNextGroup(t *testing.T) {
	c := newChecksHarness(t, nil)
	b2 := c.addS3("b2", "minio-secret-b2")
	slot := weeklySlot(time.Now())

	c.panel.RunDueChecks(slot.Add(-time.Minute))
	for _, target := range []*core.Record{c.disk, b2} {
		if rows := c.checksOf(target); len(rows) != 0 {
			t.Fatalf("%d checks of %s before the slot", len(rows), target.GetString("name"))
		}
		if next := c.fresh(target).GetDateTime("next_check_at").Time(); !next.Equal(slot) {
			t.Fatalf("the next check of %s: %s, not %s", target.GetString("name"), next, slot)
		}
	}

	c.panel.RunDueChecks(slot)
	for _, target := range []*core.Record{c.disk, b2} {
		name := target.GetString("name")
		rows := c.checksOf(target)
		if len(rows) != 1 {
			t.Fatalf("%d checks of %s at the slot", len(rows), name)
		}
		if rows[0].GetString("trigger") != schema.RunSchedule || !rows[0].GetDateTime("scheduled_at").Time().Equal(slot) ||
			rows[0].GetString("subset") != "1/20" || rows[0].GetString("kind") != schema.CheckIntegrity || rows[0].GetString("host") != c.host.Id {
			t.Errorf("the check of %s: %v", name, rows[0].FieldsData())
		}
		command := c.command(rows[0].GetString("command"))
		payload := payloadOf(t, command)
		repository, _ := payload["repository"].(map[string]any)
		if command.GetString("type") != "backup.check" || command.GetString("host") != c.host.Id ||
			command.GetString("resource") != "backup-integrity:"+target.Id || payload["read_data_subset"] != "1/20" ||
			repository["target_id"] != target.Id || repository["password"] != nil || command.GetString("requested_label") != "schedule" {
			t.Errorf("the command of the check of %s: %v %v", name, command.FieldsData(), payload)
		}

		row := c.checkEnded(rows[0].Id)
		if row.GetString("status") != schema.CheckOK || row.GetBool("confirmed") || row.GetBool("unconfirmed") ||
			row.GetDateTime("started_at").IsZero() || row.GetDateTime("finished_at").IsZero() || !strings.Contains(row.GetString("log"), "no errors were found") {
			t.Errorf("the check of %s ended: %v", name, row.FieldsData())
		}
		got := c.fresh(target)
		if got.GetString("integrity_status") != schema.CheckOK || got.GetDateTime("integrity_at").IsZero() || got.GetInt("integrity_group") != 2 ||
			got.GetString("last_check") != row.Id || got.GetString("last_check_status") != schema.CheckOK || got.GetString("integrity_error") != "" ||
			!got.GetDateTime("next_check_at").Time().Equal(slot.AddDate(0, 0, 7)) {
			t.Errorf("the summary of %s: %v", name, got.FieldsData())
		}

		// one pass, without a lock, in a job named after its command
		jobs := c.checkJobsOf(command.Id)
		if len(jobs) != 1 || passOf(jobs[0]) != backup.PassFree || jobs[0].Hostname != "pc-"+command.Id {
			t.Fatalf("the jobs of the check of %s: %v", name, jobs)
		}
		job := jobs[0]
		if _, cache := mounts(job)["/cache"]; cache || job.Host["SecurityOpt"] == nil {
			t.Errorf("the job of the check of %s: %v", name, job.Host)
		}
		if target.Id == c.disk.Id {
			repo := mounts(job)[backup.LocalMount]
			if repo == nil || repo["ReadOnly"] != true || capabilities(job) != "[DAC_OVERRIDE]" || job.Host["NetworkMode"] != "none" {
				t.Errorf("the local repository of the check's job: %v %s", job.Host, capabilities(job))
			}
		} else if capabilities(job) != "[]" || job.Host["NetworkMode"] != "bridge" {
			t.Errorf("the remote check's job: %v %s", job.Host, capabilities(job))
		}
	}
	calls := resticCalls(t, c.restic)
	for _, call := range calls {
		if resticCommand(call) == "check" && !strings.Contains(call, "--no-lock check --read-data-subset=1/20") {
			t.Errorf("a check took a lock: %s", call)
		}
	}
	if alerts := c.openAlerts("backup_check_failed"); len(alerts) != 0 {
		t.Errorf("alerts: %v", alerts)
	}

	// the same slot again starts nothing
	c.panel.RunDueChecks(slot)
	if rows := c.checksOf(c.disk); len(rows) != 1 {
		t.Errorf("%d checks of one slot", len(rows))
	}

	// Check now is the admins'
	for _, token := range []string{c.viewer, c.operator} {
		if status, _ := c.checkNow(c.disk, token); status != http.StatusForbidden {
			t.Errorf("Check now by a non-admin: %d", status)
		}
	}
	now := c.checkedNow(c.disk)
	if now.GetString("trigger") != schema.RunManual || now.GetString("status") != schema.CheckOK || now.GetString("subset") != "2/20" ||
		!now.GetDateTime("scheduled_at").IsZero() {
		t.Errorf("Check now: %v", now.FieldsData())
	}
	if got := c.fresh(c.disk); got.GetInt("integrity_group") != 3 || got.GetString("last_check") != now.Id {
		t.Errorf("the summary after Check now: %v", got.FieldsData())
	}
	started, err := c.app.FindRecordsByFilter(schema.Events, "kind='backup.check_started'", "", 0, 0)
	if err != nil || len(started) != 1 || !strings.Contains(started[0].GetString("message"), "admin@example.com") {
		t.Errorf("the events of Check now: %v %v", started, err)
	}
}

// TestErrorsAreConfirmedWithTheRepositoryLocked (#31, AC1): when the
// lock-free pass finds errors, the check reads the same group again under
// restic's exclusive lock, in a job whose repository is writable. Errors the
// second pass finds too fail the check, with restic's lines and its repair,
// and raise the critical alert of the target on its server; the group stays,
// so that the next check reads it again, and a check that finds nothing
// resolves the alert. Errors only the lock-free pass saw, as a prune running
// meanwhile makes, leave the repository fine: no alert.
func TestErrorsAreConfirmedWithTheRepositoryLocked(t *testing.T) {
	c := newChecksHarness(t, nil)
	b2 := c.addS3("b2", "minio-secret-b2")

	t.Setenv("FAKE_RESTIC_CHECK", "errors")
	failed := c.checkedNow(c.disk)
	found := []backup.CheckError{}
	if err := failed.UnmarshalJSONField("errors", &found); err != nil {
		t.Fatal(err)
	}
	var repair backup.CheckRepair
	if err := failed.UnmarshalJSONField("repair", &repair); err != nil {
		t.Fatal(err)
	}
	if failed.GetString("status") != schema.CheckFailed || !failed.GetBool("confirmed") || failed.GetBool("unconfirmed") ||
		len(found) != 1 || found[0].Kind != backup.CheckErrorPack || len(found[0].ID) != 64 || !strings.Contains(found[0].Text, "contains 1 errors") ||
		len(repair.Packs) != 1 || repair.Packs[0] != found[0].ID || !repair.Snapshots {
		t.Errorf("a check whose errors were confirmed: %v", failed.FieldsData())
	}
	jobs := c.checkJobsOf(failed.GetString("command"))
	if len(jobs) != 2 || passOf(jobs[0]) != backup.PassFree || passOf(jobs[1]) != backup.PassConfirm {
		t.Fatalf("the passes: %v", jobs)
	}
	if repo := mounts(jobs[1])[backup.LocalMount]; repo == nil || repo["ReadOnly"] == true || capabilities(jobs[1]) != "[DAC_OVERRIDE FOWNER]" ||
		jobs[1].Hostname != "pc-"+failed.GetString("command") {
		t.Errorf("the second pass's job: %v %s", jobs[1].Host, capabilities(jobs[1]))
	}
	checks := []string{}
	for _, call := range resticCalls(t, c.restic) {
		if resticCommand(call) == "check" {
			checks = append(checks, call)
		}
	}
	if len(checks) != 2 || !strings.Contains(checks[0], "--no-lock check --read-data-subset=1/20") ||
		!strings.Contains(checks[1], "--retry-lock 30m check --read-data-subset=1/20") || strings.Contains(checks[1], "--no-lock") {
		t.Errorf("restic checked: %v", checks)
	}
	alert := c.openAlert("backup_check_failed", c.disk.Id)
	if alert == nil || alert.GetString("severity") != schema.SeverityCritical || alert.GetString("host") != c.host.Id ||
		!strings.Contains(alert.GetString("message"), "The integrity check of disk found errors in group 1/20: Pack ") {
		t.Errorf("the alert: %v", alert)
	}
	target := c.fresh(c.disk)
	if target.GetString("integrity_status") != schema.CheckFailed || target.GetInt("integrity_group") > 1 ||
		!strings.Contains(target.GetString("integrity_error"), "contains 1 errors") {
		t.Errorf("the target: %v", target.FieldsData())
	}
	if events, err := c.app.FindRecordsByFilter(schema.Events, "kind='backup.check_failed'", "", 0, 0); err != nil || len(events) != 1 ||
		events[0].GetString("severity") != schema.SeverityCritical {
		t.Errorf("events: %v %v", events, err)
	}

	// errors that a prune running meanwhile made, which the locked pass does
	// not see
	t.Setenv("FAKE_RESTIC_CHECK", "errors_free")
	unconfirmed := c.checkedNow(b2)
	if unconfirmed.GetString("status") != schema.CheckOK || !unconfirmed.GetBool("unconfirmed") || unconfirmed.GetBool("confirmed") {
		t.Errorf("a check whose errors were not confirmed: %v", unconfirmed.FieldsData())
	}
	if jobs := c.checkJobsOf(unconfirmed.GetString("command")); len(jobs) != 2 || passOf(jobs[1]) != backup.PassConfirm {
		t.Errorf("the passes of the check not confirmed: %v", jobs)
	}
	if alert := c.openAlert("backup_check_failed", b2.Id); alert != nil {
		t.Errorf("errors not confirmed raised the critical alert: %v", alert.FieldsData())
	}
	if alert := c.openAlert("backup_check_unverified", b2.Id); alert != nil {
		t.Errorf("errors not confirmed raised a warning: %v", alert.FieldsData())
	}
	if got := c.fresh(b2); got.GetString("integrity_status") != schema.CheckOK || got.GetInt("integrity_group") != 2 {
		t.Errorf("the target whose errors were not confirmed: %v", got.FieldsData())
	}

	// the group that failed is read again, and found fine
	t.Setenv("FAKE_RESTIC_CHECK", "")
	fine := c.checkedNow(c.disk)
	if fine.GetString("status") != schema.CheckOK || fine.GetString("subset") != "1/20" {
		t.Errorf("the check after the errors: %v", fine.FieldsData())
	}
	if alert := c.openAlert("backup_check_failed", c.disk.Id); alert != nil {
		t.Errorf("a check that found nothing left the alert: %v", alert.FieldsData())
	}
	if got := c.fresh(c.disk); got.GetString("integrity_status") != schema.CheckOK || got.GetInt("integrity_group") != 2 || got.GetString("integrity_error") != "" {
		t.Errorf("the target after the errors: %v", got.FieldsData())
	}
}

// TestACheckThatCouldNotTellIsUnverifiedAndTriedAgain (#31, AC1): a check
// whose second pass found the repository locked by another restic could not
// confirm what the first found: it is suspect, with the lock, raises the
// warning that the repository is not verified, and is tried again an hour
// later, which finds it fine and resolves the warning. A check that did not
// complete, as with a password that is not the repository's, raises the
// warning too. Admins set the schedule through their route.
func TestACheckThatCouldNotTellIsUnverifiedAndTriedAgain(t *testing.T) {
	c := newChecksHarness(t, nil)
	schedule, slot := farSchedule(time.Now())
	for _, token := range []string{c.viewer, c.operator} {
		if status, _ := c.send(http.MethodPut, "/api/pc/settings/backup-checks", token, map[string]any{"schedule": schedule}); status != http.StatusForbidden {
			t.Errorf("a non-admin sets the checks: %d", status)
		}
	}
	if status, out := c.send(http.MethodPut, "/api/pc/settings/backup-checks", c.admin, map[string]any{"schedule": "*/5 * * * *"}); status != http.StatusBadRequest ||
		out["message"] != "A check runs at most once a day." {
		t.Errorf("checks every 5 minutes: %d %v", status, out)
	}
	status, out := c.send(http.MethodPut, "/api/pc/settings/backup-checks", c.admin, map[string]any{"schedule": schedule, "groups": 10})
	if status != http.StatusOK || out["schedule"] != schedule || out["groups"] != float64(10) || out["next_check_at"] != slot.Format(time.RFC3339) {
		t.Fatalf("set the checks: %d %v", status, out)
	}
	if next := c.fresh(c.disk).GetDateTime("next_check_at").Time(); !next.Equal(slot) {
		t.Errorf("the next check after the schedule changed: %s", next)
	}

	// the first pass finds errors; the second finds the repository locked,
	// before and after its unlock
	t.Setenv("FAKE_RESTIC_CHECK", "errors_free")
	t.Setenv("FAKE_RESTIC_LOCKED_CHECK", "2")
	t.Setenv("FAKE_RESTIC_LOCK_HOST", "nas")
	t.Setenv("FAKE_RESTIC_LOCK_PID", "4121")
	c.panel.RunDueChecks(slot.Add(-time.Minute))
	c.panel.RunDueChecks(slot)
	rows := c.checksOf(c.disk)
	if len(rows) != 1 || rows[0].GetString("subset") != "1/10" {
		t.Fatalf("the checks at the slot: %v", rows)
	}
	suspect := c.checkEnded(rows[0].Id)
	lock := lockOf(t, suspect)
	if suspect.GetString("status") != schema.CheckSuspect || suspect.GetBool("confirmed") || lock == nil || lock.Host != "nas" || lock.PID != 4121 ||
		!lock.Exclusive || lock.Created.IsZero() || !strings.Contains(suspect.GetString("error"), "already locked") {
		t.Fatalf("a check that could not confirm: %v", suspect.FieldsData())
	}
	retryAt := suspect.GetDateTime("retry_at").Time()
	if !near(retryAt, suspect.GetDateTime("finished_at").Time().Add(time.Hour)) {
		t.Errorf("the retry of the suspect check: %s, finished %s", retryAt, suspect.GetDateTime("finished_at"))
	}
	if unlocks := fakerestic.Unlocks(t, c.restic); unlocks != 1 {
		t.Errorf("%d unlocks", unlocks)
	}
	warning := c.openAlert("backup_check_unverified", c.disk.Id)
	if warning == nil || warning.GetString("severity") != schema.SeverityWarning || warning.GetString("host") != c.host.Id {
		t.Errorf("the warning: %v", warning)
	}
	if alert := c.openAlert("backup_check_failed", c.disk.Id); alert != nil {
		t.Errorf("errors not confirmed raised the critical alert: %v", alert.FieldsData())
	}
	if got := c.fresh(c.disk); got.GetString("last_check_status") != schema.CheckSuspect || got.GetString("integrity_status") != "" || got.GetInt("integrity_group") > 1 {
		t.Errorf("the target: %v", got.FieldsData())
	}

	// an hour later it is tried again, for the same group, and found fine
	t.Setenv("FAKE_RESTIC_CHECK", "")
	c.panel.RunDueChecks(retryAt.Add(-time.Minute))
	if rows := c.checksOf(c.disk); len(rows) != 1 {
		t.Fatalf("%d checks before the retry was due", len(rows))
	}
	// the checks' scheduler goes by the minute: the retry's is the next one
	c.panel.RunDueChecks(retryAt.Add(time.Minute))
	rows = c.checksOf(c.disk)
	if len(rows) != 2 || rows[1].GetString("trigger") != schema.RunRetry || rows[1].GetString("retry_of") != suspect.Id || rows[1].GetInt("attempt") != 2 ||
		rows[1].GetString("subset") != "1/10" || !rows[1].GetDateTime("scheduled_at").IsZero() {
		t.Fatalf("the retry: %v", rows)
	}
	if retried := c.fresh(suspect); !retried.GetDateTime("retry_at").IsZero() {
		t.Errorf("the suspect check's retry stays planned: %v", retried.FieldsData())
	}
	if fine := c.checkEnded(rows[1].Id); fine.GetString("status") != schema.CheckOK {
		t.Errorf("the retry ended: %v", fine.FieldsData())
	}
	if warning := c.openAlert("backup_check_unverified", c.disk.Id); warning != nil {
		t.Errorf("a check that found nothing left the warning: %v", warning.FieldsData())
	}
	c.panel.RunDueChecks(retryAt.Add(3 * time.Minute))
	if rows := c.checksOf(c.disk); len(rows) != 2 {
		t.Errorf("%d checks after the retry", len(rows))
	}

	// a password that is not the repository's
	keyFile := filepath.Join(c.diskDir, "keys", "password")
	own, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte("another-password"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := c.checkedNow(c.disk)
	if err := os.WriteFile(keyFile, own, 0o600); err != nil {
		t.Fatal(err)
	}
	if broken.GetString("status") != schema.CheckError || !strings.Contains(broken.GetString("error"), "password") ||
		!broken.GetDateTime("retry_at").IsZero() {
		t.Errorf("a check with another password: %v", broken.FieldsData())
	}
	if warning := c.openAlert("backup_check_unverified", c.disk.Id); warning == nil || !strings.Contains(warning.GetString("message"), "did not complete") {
		t.Errorf("the warning of a check that did not complete: %v", warning)
	}
	if got := c.fresh(c.disk); got.GetString("integrity_status") != schema.CheckOK || got.GetString("last_check_status") != schema.CheckError ||
		got.GetString("last_check_error") == "" {
		t.Errorf("the target after a check that did not complete: %v", got.FieldsData())
	}
	for _, record := range []*core.Record{suspect, broken} {
		if raw, _ := json.Marshal(c.fresh(record).FieldsData()); strings.Contains(string(raw), string(own)) {
			t.Errorf("the check %s holds the repository's password", record.Id)
		}
	}
}

// TestAnAgentThatCannotCheckIsSkippedNotFailed (#31, AC1): a target whose
// server's agent is too old to check repositories gets no check at its slot:
// its check is skipped, saying to update the agent, with the warning that it
// is not verified and never the critical alert, and its integrity stays
// unknown. Check now refuses it. The other server's target is checked.
func TestAnAgentThatCannotCheckIsSkippedNotFailed(t *testing.T) {
	c := newChecksHarness(t, nil)
	old := c.enrolledOn("old-node", nil, func(a *agent.Agent) { a.Executor.Handle("backup.check", nil) })
	body := s3Target("attic", "minio-secret-attic")
	body["host"] = old.host.Id
	if status, out := c.send(http.MethodPost, "/api/pc/backup-targets", c.admin, body); status != http.StatusCreated {
		t.Fatalf("the old server's target: %d %v", status, out)
	}
	c.checksDone()
	attic := c.targetNamed("attic")
	if caps := c.fresh(old.host).GetStringSlice("capabilities"); len(caps) == 0 || slices.Contains(caps, "backup.check") {
		t.Fatalf("the old agent's capabilities: %v", caps)
	}

	slot := weeklySlot(time.Now())
	c.panel.RunDueChecks(slot.Add(-time.Minute))
	c.panel.RunDueChecks(slot)
	rows := c.checksOf(attic)
	if len(rows) != 1 {
		t.Fatalf("%d checks of the old server's target", len(rows))
	}
	skipped := rows[0]
	if skipped.GetString("status") != schema.CheckSkipped || skipped.GetString("command") != "" || skipped.GetString("host") != old.host.Id ||
		skipped.GetString("error") != "The agent of old-node cannot check repositories yet: update it." || !skipped.GetDateTime("retry_at").IsZero() {
		t.Errorf("the check of the old server's target: %v", skipped.FieldsData())
	}
	if commands, err := c.app.FindRecordsByFilter(schema.Commands, "type='backup.check' && host={:host}", "", 0, 0, dbx.Params{"host": old.host.Id}); err != nil || len(commands) != 0 {
		t.Errorf("the old agent was sent a check: %v %v", commands, err)
	}
	warning := c.openAlert("backup_check_unverified", attic.Id)
	if warning == nil || warning.GetString("severity") != schema.SeverityWarning || warning.GetString("host") != old.host.Id ||
		!strings.Contains(warning.GetString("message"), "update it") {
		t.Errorf("the warning: %v", warning)
	}
	if alert := c.openAlert("backup_check_failed", attic.Id); alert != nil {
		t.Errorf("an old agent failed the check: %v", alert.FieldsData())
	}
	if got := c.fresh(attic); got.GetString("integrity_status") != "" || got.GetString("last_check_status") != schema.CheckSkipped {
		t.Errorf("the old server's target: %v", got.FieldsData())
	}
	if status, out := c.checkNow(attic, c.admin); status != http.StatusConflict || out["message"] != "The agent of old-node cannot check repositories yet: update it." {
		t.Errorf("Check now on an old agent: %d %v", status, out)
	}

	disk := c.checksOf(c.disk)
	if len(disk) != 1 {
		t.Fatalf("%d checks of the other server's target", len(disk))
	}
	if row := c.checkEnded(disk[0].Id); row.GetString("status") != schema.CheckOK {
		t.Errorf("the other server's check: %v", row.FieldsData())
	}
}

// TestABackupRunsWhileItsTargetIsChecked (#31, AC1): a check holds its own
// ordering key, so a run of the same target goes while the check reads, and
// a second check of the target is refused while one is under way, as is a
// target being added. Stopping a check gives restic 30 seconds to remove its
// lock; the check is stopped, not failed, and alerts nothing. A server that
// is offline cannot check.
func TestABackupRunsWhileItsTargetIsChecked(t *testing.T) {
	c := newChecksHarness(t, func(engine *fakedocker.Engine) { addVolume(t, engine, "app_data") })
	id := c.addJob(map[string]any{"name": "photos", "source_kind": "volume", "source": "app_data"})["id"].(string)
	entered, release := c.holdChecks()
	status, out := c.checkNow(c.disk, c.admin)
	if status != http.StatusAccepted {
		t.Fatalf("Check now: %d %v", status, out)
	}
	checkID, commandID := out["check"].(string), out["command"].(string)
	select {
	case <-entered:
	case <-time.After(waitLimit):
		t.Fatal("the check's job did not start")
	}
	c.waitForStatus(commandID, schema.CommandRunning)
	if status, out := c.checkNow(c.disk, c.admin); status != http.StatusConflict || out["message"] != "An integrity check of disk is under way." {
		t.Errorf("a second check: %d %v", status, out)
	}

	// the run goes meanwhile
	if run := c.runNow(id)[0]; run.GetString("status") != schema.RunSucceeded {
		t.Errorf("a run while its target is checked: %v", run.FieldsData())
	}
	if command := c.command(commandID); command.GetString("status") != schema.CommandRunning {
		t.Errorf("the check after the run: %s", command.GetString("status"))
	}

	// an operator stops it
	if status, out := c.send(http.MethodPost, "/api/pc/commands/"+commandID+"/cancel", c.viewer, nil); status != http.StatusForbidden {
		t.Errorf("a viewer stops the check: %d %v", status, out)
	}
	if status, out := c.send(http.MethodPost, "/api/pc/commands/"+commandID+"/cancel", c.operator, nil); status >= http.StatusBadRequest {
		t.Fatalf("stop the check: %d %v", status, out)
	}
	stopped := c.checkEnded(checkID)
	if stopped.GetString("status") != schema.CheckCancelled {
		t.Errorf("a stopped check: %v", stopped.FieldsData())
	}
	// the agent stops the job, which the hold lets go once it is stopped
	c.waitFor("the check's job to be stopped with restic's grace", func() bool { return c.docker.Called("/stop?t=30") })
	c.waitFor("the check's command to end on its agent", func() bool {
		return len(c.checkJobsOf(commandID)) == 1
	})
	release()
	if alerts := c.openAlerts("backup_check_unverified"); len(alerts) != 0 {
		t.Errorf("a stopped check alerted: %v", alerts)
	}
	if alerts := c.openAlerts("backup_check_failed"); len(alerts) != 0 {
		t.Errorf("a stopped check alerted: %v", alerts)
	}
	if got := c.fresh(c.disk); got.GetString("last_check_status") != schema.CheckCancelled || got.GetString("integrity_status") != "" {
		t.Errorf("the target of a stopped check: %v", got.FieldsData())
	}

	// a target being added, and a server that is offline
	pending := c.fresh(c.disk)
	pending.Set("status", schema.TargetPending)
	if err := c.app.Save(pending); err != nil {
		t.Fatal(err)
	}
	if status, out := c.checkNow(c.disk, c.admin); status != http.StatusConflict || !strings.Contains(out["message"].(string), "it has no repository") {
		t.Errorf("Check now of a target being added: %d %v", status, out)
	}
	pending.Set("status", schema.TargetOK)
	if err := c.app.Save(pending); err != nil {
		t.Fatal(err)
	}
	if err := c.agent.stop(); err != nil {
		t.Fatal(err)
	}
	c.waitFor("the server to go offline", func() bool { return !c.fresh(c.host).GetBool("stream_open") })
	if status, out := c.checkNow(c.disk, c.admin); status != http.StatusConflict || out["message"] != "The server backup-node is offline: its agent checks the target." {
		t.Errorf("Check now on an offline server: %d %v", status, out)
	}
}

// failLocked makes the next run find the repository locked by a holder, on
// its first try and after its unlock, with a lock as old as age.
func failLocked(t *testing.T, holder, age string) {
	t.Helper()
	t.Setenv("FAKE_RESTIC_LOCKED_BACKUP", "2")
	t.Setenv("FAKE_RESTIC_LOCK_HOST", holder)
	t.Setenv("FAKE_RESTIC_LOCK_AGE", age)
}

// unlockedThenBackedUp tells whether a run's retry ran plain restic unlock
// right before the backup that saved its snapshot: the last two calls.
func unlockedThenBackedUp(t *testing.T, root string) bool {
	t.Helper()
	calls := resticCalls(t, root)
	if len(calls) < 2 {
		return false
	}
	unlock, backed := calls[len(calls)-2], calls[len(calls)-1]
	fields := strings.Fields(unlock)
	return resticCommand(unlock) == "unlock" && fields[len(fields)-1] == "unlock" && resticCommand(backed) == "backup"
}

// TestALockedRunIsUnlockedAndTriedAgainFromItsBanner (#31, AC2): a run that
// found the repository locked, after its wait and its unlock, fails with the
// lock restic named: the holder, a PocketCloud command that ended, by its
// label and server, and when it last refreshed the lock. It is planned again
// in 5 minutes with a warning. Unlock and retry, an operator's, starts a
// retry at once, as the lock is stale: its job runs plain restic unlock,
// never --remove-all, then the backup, which succeeds and resolves the
// alert. The run it tried again cannot be tried again.
func TestALockedRunIsUnlockedAndTriedAgainFromItsBanner(t *testing.T) {
	c := newChecksHarness(t, func(engine *fakedocker.Engine) { addVolume(t, engine, "app_data") })
	id := c.addJob(map[string]any{"name": "photos", "source_kind": "volume", "source": "app_data"})["id"].(string)
	key := id + ":" + c.disk.Id
	holder, err := c.app.FindFirstRecordByFilter(schema.Commands, "type='backup.init'", nil)
	if err != nil {
		t.Fatal(err)
	}
	failLocked(t, backup.JobHost(holder.Id), "40m")

	run := c.runNow(id)[0]
	lock := lockOf(t, run)
	if run.GetString("status") != schema.RunFailed || run.GetString("failure") != backup.FailureLocked || !strings.Contains(run.GetString("error"), "already locked") ||
		max(run.GetInt("attempt"), 1) != 1 {
		t.Fatalf("a locked run: %v", run.FieldsData())
	}
	if lock == nil || lock.Host != "pc-"+holder.Id || lock.PID != 1 || lock.User != "root" || !lock.Exclusive || lock.StorageID != "4f2a9c1e" ||
		lock.Command != holder.Id || lock.CommandType != "backup.init" || lock.CommandLabel != holder.GetString("label") ||
		lock.CommandHost != c.host.Id || lock.CommandHostName != "backup-node" ||
		lock.Created.Before(time.Now().Add(-41*time.Minute)) || lock.Created.After(time.Now().Add(-39*time.Minute)) {
		t.Errorf("the lock: %+v", lock)
	}
	finished := run.GetDateTime("finished_at").Time()
	if retryAt := run.GetDateTime("retry_at").Time(); !near(retryAt, finished.Add(5*time.Minute)) {
		t.Errorf("the retry of the locked run: %s, finished %s", retryAt, finished)
	}
	alert := c.openAlert("backup_failed", key)
	if alert == nil || alert.GetString("severity") != schema.SeverityWarning || !strings.Contains(alert.GetString("message"), "(attempt 2 of 4).") {
		t.Errorf("the alert: %v", alert)
	}

	// Unlock and retry
	if status, _ := c.retry(run.Id, c.viewer); status != http.StatusForbidden {
		t.Errorf("a viewer retries: %d", status)
	}
	status, out := c.retry(run.Id, c.operator)
	if status != http.StatusOK || out["run"] == nil {
		t.Fatalf("Unlock and retry: %d %v", status, out)
	}
	retry := c.runEnded(out["run"].(string))
	if retry.GetString("status") != schema.RunSucceeded || retry.GetString("trigger") != schema.RunRetry || retry.GetString("retry_of") != run.Id ||
		retry.GetInt("attempt") != 2 || retry.GetString("snapshot_id") == "" {
		t.Errorf("the retry: %v", retry.FieldsData())
	}
	command := c.command(retry.GetString("command"))
	if payload := payloadOf(t, command); payload["unlock"] != true || payload["run_id"] != retry.Id || command.GetString("requested_by") == "" {
		t.Errorf("the retry's command: %v %v", command.FieldsData(), payload)
	}
	jobs := c.backupJobs()
	if last := jobs[len(jobs)-1]; !slices.Contains(last.Cmd, "--unlock") || last.Hostname != "pc-"+command.Id {
		t.Errorf("the retry's job: %v %s", last.Cmd, last.Hostname)
	}
	if !unlockedThenBackedUp(t, c.restic) || fakerestic.Unlocks(t, c.restic) != 2 {
		t.Errorf("restic ran: %v", resticCalls(t, c.restic))
	}
	c.noRemoveAll()
	if tried := c.fresh(run); !tried.GetDateTime("retry_at").IsZero() || tried.GetString("status") != schema.RunFailed {
		t.Errorf("the run tried again: %v", tried.FieldsData())
	}
	if alert := c.openAlert("backup_failed", key); alert != nil {
		t.Errorf("the retry left the alert: %v", alert.FieldsData())
	}
	if status, out := c.retry(run.Id, c.operator); status != http.StatusConflict || out["message"] != "A newer run of this job to this target exists." {
		t.Errorf("the run again: %d %v", status, out)
	}
	if status, out := c.retry(retry.Id, c.operator); status != http.StatusConflict || out["message"] != "Only a failed run is tried again." {
		t.Errorf("a run that succeeded: %d %v", status, out)
	}
}

// TestARetryWaitsForAHolderThatStillRuns (#31, AC2): a run that found the
// repository locked by a PocketCloud command that still runs, here a check
// of its target in its second pass, is not unlocked: Unlock and retry is
// refused, saying who holds the lock, and the scheduler's retry waits for
// the holder without counting an attempt. Once the holder ended, Unlock and
// retry starts the retry.
func TestARetryWaitsForAHolderThatStillRuns(t *testing.T) {
	c := newChecksHarness(t, func(engine *fakedocker.Engine) { addVolume(t, engine, "app_data") })
	id := c.addJob(map[string]any{"name": "photos", "source_kind": "volume", "source": "app_data"})["id"].(string)
	entered, release := c.holdChecks()
	status, out := c.checkNow(c.disk, c.admin)
	if status != http.StatusAccepted {
		t.Fatalf("Check now: %d %v", status, out)
	}
	checkID, holder := out["check"].(string), out["command"].(string)
	select {
	case <-entered:
	case <-time.After(waitLimit):
		t.Fatal("the check's job did not start")
	}
	c.waitForStatus(holder, schema.CommandRunning)

	failLocked(t, backup.JobHost(holder), "40m")
	run := c.runNow(id)[0]
	lock := lockOf(t, run)
	if run.GetString("failure") != backup.FailureLocked || lock == nil || lock.Command != holder || lock.CommandType != "backup.check" ||
		lock.CommandLabel != "Check the integrity of disk" {
		t.Fatalf("a run locked out by a check: %v", run.FieldsData())
	}
	retryAt := run.GetDateTime("retry_at").Time()
	if !near(retryAt, run.GetDateTime("finished_at").Time().Add(5*time.Minute)) {
		t.Errorf("the retry follows the holder: %s", retryAt)
	}

	status, out = c.retry(run.Id, c.operator)
	if status != http.StatusConflict ||
		out["message"] != "The repository is locked by Check the integrity of disk on backup-node, which is still running: PocketCloud retries once it ends." {
		t.Errorf("Unlock and retry while the holder runs: %d %v", status, out)
	}
	c.panel.RunDueBackups(retryAt.Add(time.Second))
	waiting := c.fresh(run)
	if runs := c.runs(id); len(runs) != 1 {
		t.Errorf("%d runs while the holder runs", len(runs))
	}
	if !near(waiting.GetDateTime("retry_at").Time(), retryAt.Add(time.Second+30*time.Minute)) ||
		waiting.GetString("retry_note") != "Waits for Check the integrity of disk." {
		t.Errorf("the retry waiting for the holder: %v", waiting.FieldsData())
	}
	if fakerestic.Unlocks(t, c.restic) != 1 {
		t.Errorf("restic ran: %v", resticCalls(t, c.restic))
	}

	// the check ends; the retry goes
	release()
	if row := c.checkEnded(checkID); row.GetString("status") != schema.CheckOK {
		t.Fatalf("the check: %v", row.FieldsData())
	}
	status, out = c.retry(run.Id, c.operator)
	if status != http.StatusOK {
		t.Fatalf("Unlock and retry once the holder ended: %d %v", status, out)
	}
	retry := c.runEnded(out["run"].(string))
	if retry.GetString("status") != schema.RunSucceeded || retry.GetInt("attempt") != 2 || !unlockedThenBackedUp(t, c.restic) {
		t.Errorf("the retry: %v %v", retry.FieldsData(), resticCalls(t, c.restic))
	}
	c.noRemoveAll()
}

// TestARetryOfAYoungLockWaitsUntilResticCountsItStale (#31, AC2): a lock
// another restic refreshed two minutes ago is not stale for 30 minutes, and
// restic unlock would leave it: Unlock and retry answers 202 with the moment
// restic counts it as stale, two minutes more for the servers' clocks, and
// the scheduler starts the retry then, not before.
func TestARetryOfAYoungLockWaitsUntilResticCountsItStale(t *testing.T) {
	c := newChecksHarness(t, func(engine *fakedocker.Engine) { addVolume(t, engine, "app_data") })
	id := c.addJob(map[string]any{"name": "photos", "source_kind": "volume", "source": "app_data"})["id"].(string)
	failLocked(t, "nas", "2m")
	run := c.runNow(id)[0]
	lock := lockOf(t, run)
	if run.GetString("failure") != backup.FailureLocked || lock == nil || lock.Host != "nas" || lock.Command != "" {
		t.Fatalf("a run locked out by another restic: %v", run.FieldsData())
	}
	stale := lock.StaleAt()
	if !near(stale, time.Now().Add(30*time.Minute)) || !near(run.GetDateTime("retry_at").Time(), stale) {
		t.Errorf("the retry of a young lock: %s, stale at %s", run.GetDateTime("retry_at"), stale)
	}

	status, out := c.retry(run.Id, c.operator)
	if status != http.StatusAccepted || out["run"] != nil {
		t.Fatalf("Unlock and retry of a young lock: %d %v", status, out)
	}
	at, err := time.Parse(time.RFC3339, fmt.Sprint(out["retry_at"]))
	if err != nil || !near(at, stale.Truncate(time.Second)) {
		t.Errorf("the moment of the retry: %v %v", out["retry_at"], err)
	}
	planned := c.fresh(run)
	if !near(planned.GetDateTime("retry_at").Time(), stale) || planned.GetString("retry_note") != "Waits until restic counts the lock as stale." {
		t.Errorf("the run planned: %v", planned.FieldsData())
	}
	if runs := c.runs(id); len(runs) != 1 {
		t.Errorf("%d runs before the lock is stale", len(runs))
	}

	c.panel.RunDueBackups(stale.Add(-time.Minute))
	if runs := c.runs(id); len(runs) != 1 {
		t.Fatalf("%d runs a minute before the lock is stale", len(runs))
	}
	c.panel.RunDueBackups(stale.Add(time.Second))
	runs := c.runs(id)
	if len(runs) != 2 || runs[1].GetString("retry_of") != run.Id || runs[1].GetString("trigger") != schema.RunRetry {
		t.Fatalf("the runs once the lock is stale: %v", runs)
	}
	retry := c.runEnded(runs[1].Id)
	if retry.GetString("status") != schema.RunSucceeded || payloadOf(t, c.command(retry.GetString("command")))["unlock"] != true ||
		!unlockedThenBackedUp(t, c.restic) {
		t.Errorf("the retry: %v %v", retry.FieldsData(), resticCalls(t, c.restic))
	}
	if command := c.command(retry.GetString("command")); command.GetString("requested_label") != "schedule" {
		t.Errorf("the scheduler's retry: %v", command.FieldsData())
	}
	c.noRemoveAll()
}

// lockedWhileItsSlotIsSkipped starts a job's run by hand and holds its backup
// while the scheduler's pass of the job's slot skips the slot, as the run
// before it is still going; then lets the backup go, which finds the
// repository locked, and is the run, failed, and the skipped one.
func (c *checksHarness) lockedWhileItsSlotIsSkipped(jobID string, slot time.Time) (failed, skipped *core.Record) {
	c.t.Helper()
	entered, release := c.gateJobs(isBackupRun, nil)
	status, out := c.send(http.MethodPost, "/api/pc/backup-jobs/"+jobID+"/run", c.operator, nil)
	if status != http.StatusOK || len(out["runs"].([]any)) != 1 {
		c.t.Fatalf("run now: %d %v", status, out)
	}
	select {
	case <-entered:
	case <-time.After(waitLimit):
		c.t.Fatal("the backup did not start")
	}
	c.panel.RunDueBackups(slot)
	runs := c.runs(jobID)
	skipped = runs[len(runs)-1]
	if skipped.GetString("status") != schema.RunSkipped || skipped.GetString("error") != "The run before it was still going." ||
		!skipped.GetDateTime("scheduled_at").Time().Equal(slot) {
		c.t.Fatalf("the slot while the run waits on the lock: %v", skipped.FieldsData())
	}
	release()
	failed = c.runEnded(out["runs"].([]any)[0].(string))
	if failed.GetString("status") != schema.RunFailed || failed.GetString("failure") != backup.FailureLocked {
		c.t.Fatalf("the run the lock kept out: %v", failed.FieldsData())
	}
	return failed, skipped
}

// TestALockedRunKeepsItsRetryWhenItsSlotIsSkippedMeanwhile (#31, AC2): an
// hourly job's slot comes while its run waits on a lock, and is skipped, as
// the run before it is still going. The skipped run never ran, so it does
// not take the place of the run the lock then kept out: the scheduler's
// retry still goes, unlocking first, and so does Unlock and retry.
func TestALockedRunKeepsItsRetryWhenItsSlotIsSkippedMeanwhile(t *testing.T) {
	c := newChecksHarness(t, func(engine *fakedocker.Engine) {
		addVolume(t, engine, "app_data")
		addVolume(t, engine, "db_data")
	})
	slot := time.Now().UTC().Truncate(time.Minute)
	earlier := slot.Add(-time.Minute)
	// hourly, at a minute that has passed: the next run is close to an hour
	// away, after the retries
	photos := c.addJob(map[string]any{"name": "photos", "source_kind": "volume", "source": "app_data",
		"schedule": fmt.Sprintf("%d * * * *", slot.Minute())})["id"].(string)
	db := c.addJob(map[string]any{"name": "db", "source_kind": "volume", "source": "db_data",
		"schedule": fmt.Sprintf("%d * * * *", earlier.Minute())})["id"].(string)
	holder, err := c.app.FindFirstRecordByFilter(schema.Commands, "type='backup.init'", nil)
	if err != nil {
		t.Fatal(err)
	}
	failLocked(t, backup.JobHost(holder.Id), "40m")

	// the scheduler's retry
	run, skipped := c.lockedWhileItsSlotIsSkipped(photos, slot)
	retryAt := run.GetDateTime("retry_at").Time()
	if !near(retryAt, run.GetDateTime("finished_at").Time().Add(5*time.Minute)) {
		t.Fatalf("the retry of the locked run: %v", run.FieldsData())
	}
	if alert := c.openAlert("backup_failed", photos+":"+c.disk.Id); alert == nil || alert.GetString("severity") != schema.SeverityWarning ||
		!strings.Contains(alert.GetString("message"), "(attempt 2 of 4).") {
		t.Errorf("the alert: %v", alert)
	}
	c.panel.RunDueBackups(retryAt.Add(time.Second))
	runs := c.runs(photos)
	if len(runs) != 3 || runs[1].Id != skipped.Id || runs[2].GetString("retry_of") != run.Id || runs[2].GetString("trigger") != schema.RunRetry {
		t.Fatalf("the runs once the retry is due: %v %v", runs, c.fresh(run).FieldsData())
	}
	retry := c.runEnded(runs[2].Id)
	if retry.GetString("status") != schema.RunSucceeded || payloadOf(t, c.command(retry.GetString("command")))["unlock"] != true ||
		!unlockedThenBackedUp(t, c.restic) {
		t.Errorf("the scheduler's retry: %v %v", retry.FieldsData(), resticCalls(t, c.restic))
	}
	if alert := c.openAlert("backup_failed", photos+":"+c.disk.Id); alert != nil {
		t.Errorf("the retry left the alert: %v", alert.FieldsData())
	}

	// Unlock and retry
	t.Setenv("FAKE_RESTIC_LOCKED_BACKUP", "4")
	run, _ = c.lockedWhileItsSlotIsSkipped(db, earlier)
	status, out := c.retry(run.Id, c.operator)
	if status != http.StatusOK || out["run"] == nil {
		t.Fatalf("Unlock and retry after the skipped slot: %d %v", status, out)
	}
	retry = c.runEnded(out["run"].(string))
	if retry.GetString("status") != schema.RunSucceeded || retry.GetString("retry_of") != run.Id || retry.GetInt("attempt") != 2 ||
		payloadOf(t, c.command(retry.GetString("command")))["unlock"] != true || !unlockedThenBackedUp(t, c.restic) {
		t.Errorf("Unlock and retry: %v %v", retry.FieldsData(), resticCalls(t, c.restic))
	}
	if alert := c.openAlert("backup_failed", db+":"+c.disk.Id); alert != nil {
		t.Errorf("the retry left the alert: %v", alert.FieldsData())
	}
	c.noRemoveAll()
}

// TestAFailedRunIsTriedAgainWithABackoff (#31): a run restic failed is tried
// again after 5 minutes, 30 minutes and 2 hours, 4 attempts in all, each by
// one run however many passes of the scheduler and retries by hand meet it;
// its alert is a warning that says the next attempt while one is planned,
// and critical once none is left. A wrong password, or files restic could
// not read, are not tried again, and alert critical at once.
func TestAFailedRunIsTriedAgainWithABackoff(t *testing.T) {
	c := newChecksHarness(t, func(engine *fakedocker.Engine) {
		addVolume(t, engine, "app_data")
		addVolume(t, engine, "db_data", "data.txt", "locked.db")
	})
	id := c.addJob(map[string]any{"name": "photos", "source_kind": "volume", "source": "app_data"})["id"].(string)
	key := id + ":" + c.disk.Id
	t.Setenv("FAKE_RESTIC_BACKUP_FAIL", "unable to open repository at s3:minio:9000: connection refused")

	run := c.runNow(id)[0]
	waits := []time.Duration{5 * time.Minute, 30 * time.Minute, 2 * time.Hour}
	for attempt := 1; attempt <= 4; attempt++ {
		if run.GetString("status") != schema.RunFailed || run.GetString("failure") != backup.FailureRestic || max(run.GetInt("attempt"), 1) != attempt {
			t.Fatalf("attempt %d: %v", attempt, run.FieldsData())
		}
		alert := c.openAlert("backup_failed", key)
		if attempt == 4 {
			if !run.GetDateTime("retry_at").IsZero() || alert == nil || alert.GetString("severity") != schema.SeverityCritical ||
				strings.Contains(alert.GetString("message"), "tries again") {
				t.Errorf("the last attempt: %v %v", run.FieldsData(), alert)
			}
			break
		}
		retryAt := run.GetDateTime("retry_at").Time()
		if !near(retryAt, run.GetDateTime("finished_at").Time().Add(waits[attempt-1])) {
			t.Errorf("attempt %d is tried again at %s, finished %s", attempt, retryAt, run.GetDateTime("finished_at"))
		}
		if alert == nil || alert.GetString("severity") != schema.SeverityWarning ||
			!strings.Contains(alert.GetString("message"), fmt.Sprintf("PocketCloud tries again at %s UTC (attempt %d of 4).", retryAt.UTC().Format("15:04"), attempt+1)) {
			t.Errorf("the alert of attempt %d: %v", attempt, alert)
		}

		// not before it is due; then once, whoever meets it
		c.panel.RunDueBackups(retryAt.Add(-time.Minute))
		if runs := c.runs(id); len(runs) != attempt {
			t.Fatalf("%d runs before attempt %d was due", len(runs), attempt+1)
		}
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() { c.panel.RunDueBackups(retryAt.Add(time.Second)) })
		}
		if attempt == 2 {
			// and by hand, at the same time
			wg.Go(func() { c.retry(run.Id, c.operator) })
		}
		wg.Wait()
		runs := c.runs(id)
		if len(runs) != attempt+1 {
			t.Fatalf("%d runs after attempt %d was due", len(runs), attempt+1)
		}
		next := runs[attempt]
		if next.GetString("retry_of") != run.Id || next.GetInt("attempt") != attempt+1 {
			t.Fatalf("attempt %d: %v", attempt+1, next.FieldsData())
		}
		run = c.runEnded(next.Id)
	}
	c.panel.RunDueBackups(time.Now().Add(3 * time.Hour))
	if runs := c.runs(id); len(runs) != 4 {
		t.Errorf("%d runs after the last attempt", len(runs))
	}
	retries, err := c.app.FindRecordsByFilter(schema.BackupRuns, "job={:job} && retry_of!=''", "", 0, 0, dbx.Params{"job": id})
	if err != nil || len(retries) != 3 {
		t.Errorf("retries: %v %v", retries, err)
	}
	t.Setenv("FAKE_RESTIC_BACKUP_FAIL", "")

	// a wrong password, and files restic could not read
	keyFile := filepath.Join(c.diskDir, "keys", "password")
	own, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte("another-password"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := c.addJob(map[string]any{"name": "db", "source_kind": "volume", "source": "db_data"})["id"].(string)
	wrong := c.runNow(db)[0]
	if err := os.WriteFile(keyFile, own, 0o600); err != nil {
		t.Fatal(err)
	}
	if wrong.GetString("failure") != backup.FailurePassword || !wrong.GetDateTime("retry_at").IsZero() {
		t.Errorf("a run with another password: %v", wrong.FieldsData())
	}
	if alert := c.openAlert("backup_failed", db+":"+c.disk.Id); alert == nil || alert.GetString("severity") != schema.SeverityCritical {
		t.Errorf("the alert of a wrong password: %v", alert)
	}
	t.Setenv("FAKE_RESTIC_UNREADABLE", "locked.db")
	partial := c.runNow(db)[0]
	if partial.GetString("status") != schema.RunFailed || partial.GetString("failure") != backup.FailurePartial || partial.GetString("snapshot_id") == "" ||
		!partial.GetDateTime("retry_at").IsZero() {
		t.Errorf("a partial run: %v", partial.FieldsData())
	}
	c.panel.RunDueBackups(time.Now().Add(3 * time.Hour))
	if runs := c.runs(db); len(runs) != 2 {
		t.Errorf("%d runs of the job that is not tried again", len(runs))
	}
	c.noRemoveAll()
}
