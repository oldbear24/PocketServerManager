package panel

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/gateway"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// restartedPanel is a panel started again on a copy of a running panel's
// database, as after a restart: nothing the first one held in memory, its
// timers included, is left. prepare changes the new panel before it starts.
func restartedPanel(t *testing.T, app core.App, prepare func(*Panel)) (*Panel, *tests.TestApp) {
	t.Helper()
	dir := t.TempDir()
	if _, err := app.ConcurrentDB().NewQuery("VACUUM INTO {:path}").Bind(dbx.Params{"path": filepath.Join(dir, "data.db")}).Execute(); err != nil {
		t.Fatal(err)
	}
	again := testutil.NewAppFrom(t, dir)
	t.Cleanup(again.Cleanup)
	p := New(again, &Config{MasterKey: testutil.MasterKey, GRPCAddr: "127.0.0.1:0"}, testUI)
	if prepare != nil {
		prepare(p)
	}
	p.Register()
	testutil.Handler(t, again)
	return p, again
}

// changeFixture is a panel whose backup job, to two targets, is carried by
// a connected server, and whose backups after a change come after delay,
// spaced by spacing.
func changeFixture(t *testing.T, delay, spacing time.Duration) (*jobFixture, *core.Record) {
	t.Helper()
	f, _, _ := newRetryFixture(t)
	f.p.SetChangeBackupTiming(delay, spacing)
	// the fixture's own server came before the timing: that backup is let go
	f.p.changes.mu.Lock()
	f.p.changes.pending = false
	f.p.changes.mu.Unlock()
	f.carry(f.host, "backup.run", backup.CapabilityPanel)
	nas := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	return f, f.job("panel (pb_data)", map[string]any{"source_kind": schema.SourcePanel, "targets": []string{f.target.Id, nas.Id}})
}

// changeRuns are a job's runs of a change.
func (f *jobFixture) changeRuns(job *core.Record) []*core.Record {
	f.t.Helper()
	runs := []*core.Record{}
	for _, run := range f.runsOf(job) {
		if run.GetString("trigger") == schema.RunChange {
			runs = append(runs, run)
		}
	}
	return runs
}

// TestAChangeQueuesAPanelBackupOnce (#32): a server enrolled or removed, or
// a certificate revoked, backs the panel up a while later, one change run
// per target of its job, once for the changes that come meanwhile; a change
// while a run of the job is still going waits for it, and a change run
// starts no sooner than the spacing after the one before.
func TestAChangeQueuesAPanelBackupOnce(t *testing.T) {
	delay, spacing := 150*time.Millisecond, 2*time.Second
	f, job := changeFixture(t, delay, spacing)

	enrolled := []*core.Record{}
	for i := range 3 {
		enrolled = append(enrolled, testutil.NewHost(t, f.app, fmt.Sprintf("new-%d", i)))
	}
	if err := gateway.Revoke(f.app, "0a1b2c", enrolled[0].Id, "new-0", "revoked by hand"); err != nil {
		t.Fatal(err)
	}
	f.eventually("the change runs", func() bool { return len(f.changeRuns(job)) == 2 })
	time.Sleep(4 * delay)
	runs := f.changeRuns(job)
	if len(runs) != 2 {
		t.Fatalf("%d change runs", len(runs))
	}
	targets := map[string]bool{}
	for _, run := range runs {
		targets[run.GetString("target")] = true
		if run.GetString("status") != schema.RunQueued || run.GetString("command") == "" || !run.GetDateTime("scheduled_at").IsZero() ||
			run.GetString("host") != f.host.Id {
			t.Errorf("a change run: %v", run.FieldsData())
		}
		if payload := f.payloadOf(run.GetString("command")); payload["panel_export"] != backup.PanelExportID(run.Id) {
			t.Errorf("its command: %v", payload)
		}
		if command := f.fresh(run); command.GetString("command") != "" {
			if stored, err := f.app.FindRecordById(schema.Commands, command.GetString("command")); err != nil || stored.GetString("requested_label") != schema.RunChange {
				t.Errorf("its command's requester: %v %v", stored, err)
			}
		}
	}
	if len(targets) != 2 {
		t.Errorf("the change runs go to %v", targets)
	}

	// a server removed while they are going: it waits for them, past the
	// spacing too, and starts no run that would only be skipped
	if err := f.app.Delete(enrolled[1]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(runs[0].GetDateTime("created").Time().Add(spacing + 4*delay)))
	if got := f.changeRuns(job); len(got) != 2 {
		t.Fatalf("%d change runs while two are going", len(got))
	}
	for _, run := range runs {
		f.endRun(run, map[string]any{"status": schema.CommandSucceeded, "started_at": time.Now().UTC(), "result": map[string]any{"snapshot_id": "4f2a"}})
	}
	f.eventually("the change runs once they ended", func() bool { return len(f.changeRuns(job)) == 4 })
	time.Sleep(4 * delay)
	later := f.changeRuns(job)
	if len(later) != 4 || later[2].GetString("status") != schema.RunQueued || later[3].GetString("status") != schema.RunQueued {
		t.Errorf("%d change runs in the end", len(later))
	}
}

// TestEveryChangeOfTheServersBacksThePanelUp (#32): an enrollment, a
// removal and a revocation each make the panel back itself up a while later,
// and the changes that come meanwhile wait for the same backup; any other
// change of a server does not.
func TestEveryChangeOfTheServersBacksThePanelUp(t *testing.T) {
	f, _ := changeFixture(t, time.Hour, time.Hour)
	pending := func() (bool, int) {
		f.p.changes.mu.Lock()
		defer f.p.changes.mu.Unlock()
		f.p.later.mu.Lock()
		defer f.p.later.mu.Unlock()
		return f.p.changes.pending, len(f.p.later.timers)
	}
	clear := func() {
		f.p.changes.mu.Lock()
		f.p.changes.pending = false
		f.p.changes.mu.Unlock()
	}
	other := testutil.NewHost(t, f.app, "node-2")
	removed := testutil.NewHost(t, f.app, "node-3")
	serial := 0
	for _, change := range []struct {
		name string
		make func()
	}{
		{"an enrollment", func() { testutil.NewHost(t, f.app, fmt.Sprintf("new-%d", serial)) }},
		{"a revocation", func() {
			if err := gateway.Revoke(f.app, fmt.Sprintf("0a1b%d", serial), other.Id, "node-2", "revoked by hand"); err != nil {
				t.Fatal(err)
			}
		}},
		{"a removal", func() {
			if err := f.app.Delete(removed); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		clear()
		_, before := pending()
		serial++
		change.make()
		if on, timers := pending(); !on || timers != before+1 {
			t.Errorf("%s: pending %v, %d timers, not %d", change.name, on, timers, before+1)
		}
		// the next change waits for the same backup
		_, before = pending()
		serial++
		testutil.NewHost(t, f.app, fmt.Sprintf("new-%d", serial))
		if on, timers := pending(); !on || timers != before {
			t.Errorf("%s, then an enrollment: pending %v, %d timers, not %d", change.name, on, timers, before)
		}
	}

	clear()
	_, before := pending()
	other.Set("status", schema.HostOnline)
	other.Set("panel_address", "192.0.2.2:9443")
	if err := f.app.Save(other); err != nil {
		t.Fatal(err)
	}
	if on, timers := pending(); on || timers != before {
		t.Errorf("a server that changed: pending %v, %d timers, not %d", on, timers, before)
	}
}

// TestAChangeRunFollowsTheSchedulersRules (#32): a backup after a change
// needs the panel's job, enabled and with targets; an offline carrier skips
// its runs with an alert, as at a scheduled minute, but a change run is of
// no minute of the schedule.
func TestAChangeRunFollowsTheSchedulersRules(t *testing.T) {
	f, job := changeFixture(t, time.Hour, time.Hour)
	volume := f.job("other", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})

	job.Set("enabled", false)
	if err := f.app.Save(job); err != nil {
		t.Fatal(err)
	}
	f.p.panelBackupAfterChange()
	job.Set("enabled", true)
	job.Set("targets", []string{})
	if err := f.app.Save(job); err != nil {
		t.Fatal(err)
	}
	f.p.panelBackupAfterChange()
	if runs := f.runsOf(job); len(runs) != 0 {
		t.Errorf("%d runs of a disabled job, or one without targets", len(runs))
	}
	if runs := f.runsOf(volume); len(runs) != 0 {
		t.Errorf("%d runs of another job", len(runs))
	}

	job.Set("targets", []string{f.target.Id})
	if err := f.app.Save(job); err != nil {
		t.Fatal(err)
	}
	f.host.Set("status", schema.HostOffline)
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	f.p.SetStarted(time.Now().Add(-time.Hour))
	f.p.panelBackupAfterChange()
	runs := f.runsOf(job)
	if len(runs) != 1 || runs[0].GetString("trigger") != schema.RunChange || runs[0].GetString("status") != schema.RunSkipped ||
		!runs[0].GetDateTime("scheduled_at").IsZero() || !strings.HasPrefix(runs[0].GetString("error"), "The server node-1 was offline at ") {
		t.Fatalf("an offline carrier's change run: %v", runs)
	}
	if a := f.openAlert(alertBackupSkipped, alertKey(job.Id, f.target.Id)); a == nil {
		t.Error("no alert of the skipped run")
	}
	// a skipped run did not start: it does not space the next
	f.host.Set("status", schema.HostOnline)
	if err := f.app.Save(f.host); err != nil {
		t.Fatal(err)
	}
	f.p.panelBackupAfterChange()
	if runs := f.changeRuns(job); len(runs) != 2 || runs[1].GetString("status") != schema.RunQueued {
		t.Errorf("%d change runs", len(runs))
	}
	// a run that did spaces the next, once it ended
	f.endRun(f.changeRuns(job)[1], map[string]any{"status": schema.CommandSucceeded, "started_at": time.Now().UTC(), "result": map[string]any{"snapshot_id": "4f2a"}})
	f.p.panelBackupAfterChange()
	if runs := f.changeRuns(job); len(runs) != 2 {
		t.Errorf("%d change runs within the spacing", len(runs))
	}
}

// TestAChangeBeforeARestartIsStillBackedUp (#32): the backup after a change
// waits in memory, so a start of the panel arms it again when a server was
// enrolled or removed, or a certificate revoked, after the newest run of
// its job that started; a run skipped since does not count, and a panel
// whose servers did not change since that run arms nothing. Once armed, the
// change runs come after the delay.
func TestAChangeBeforeARestartIsStillBackedUp(t *testing.T) {
	pending := func(p *Panel) bool {
		p.changes.mu.Lock()
		defer p.changes.mu.Unlock()
		return p.changes.pending
	}
	// a panel whose job ran once, after its servers were enrolled
	ran := func(t *testing.T) (*jobFixture, *core.Record, *core.Record) {
		t.Helper()
		f, job := changeFixture(t, time.Hour, time.Hour)
		other := testutil.NewHost(t, f.app, "node-2")
		run, err := f.p.startRun(context.Background(), job, f.target, f.host, schema.RunManual, time.Time{}, commands.Options{})
		if err != nil {
			t.Fatal(err)
		}
		f.endRun(run, map[string]any{"status": schema.CommandSucceeded, "started_at": time.Now().UTC(), "result": map[string]any{"snapshot_id": "4f2a"}})
		return f, job, other
	}
	for _, test := range []struct {
		name   string
		change func(t *testing.T, f *jobFixture, job, other *core.Record)
		armed  bool
	}{
		{"nothing since the run", func(*testing.T, *jobFixture, *core.Record, *core.Record) {}, false},
		{"an enrollment", func(t *testing.T, f *jobFixture, _, _ *core.Record) { testutil.NewHost(t, f.app, "new-1") }, true},
		{"a removal", func(t *testing.T, f *jobFixture, _, other *core.Record) {
			if err := f.app.Delete(other); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"a revocation", func(t *testing.T, f *jobFixture, _, other *core.Record) {
			if err := gateway.Revoke(f.app, "0a1b2c", other.Id, "node-2", "revoked by hand"); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"an enrollment, then a skipped run", func(t *testing.T, f *jobFixture, job, _ *core.Record) {
			testutil.NewHost(t, f.app, "new-1")
			f.p.skipRun(job, f.target, f.host, schema.RunChange, time.Time{}, "The server node-1 was offline at 12:00 UTC.", false)
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, job, other := ran(t)
			test.change(t, f, job, other)
			p, _ := restartedPanel(t, f.app, func(p *Panel) { p.SetChangeBackupTiming(time.Hour, time.Hour) })
			if got := pending(p); got != test.armed {
				t.Errorf("a backup after a change pending at the start: %v, not %v", got, test.armed)
			}
		})
	}

	f, job, _ := ran(t)
	testutil.NewHost(t, f.app, "new-1")
	_, app := restartedPanel(t, f.app, func(p *Panel) { p.SetChangeBackupTiming(100*time.Millisecond, time.Hour) })
	waitUntil(t, "the change runs after the restart", func() bool {
		runs, err := app.FindRecordsByFilter(schema.BackupRuns, "job={:job} && trigger={:change} && status={:queued}", "", 0, 0,
			dbx.Params{"job": job.Id, "change": schema.RunChange, "queued": schema.RunQueued})
		return err == nil && len(runs) == 2
	})
}
