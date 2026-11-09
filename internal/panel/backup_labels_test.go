package panel

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/stacks"
)

// TestLabelJobsFollowTheirLabels (#29): a deploy makes a job of each
// labelled service, named after the stack and the service, with the label's
// schedule, targets and retention where it gives them; a label that is gone
// leaves its job disabled and marked, one that is back runs again.
func TestLabelJobsFollowTheirLabels(t *testing.T) {
	f := newJobFixture(t)
	stack := f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "shop"})
	f.job("shop-db", map[string]any{"source_kind": schema.SourceVolume, "source": "taken"})

	problems := f.p.syncBackupLabels(stack, []stacks.BackupLabel{
		{Service: "db", Kind: stacks.BackupPostgres},
		{Service: "app", Kind: stacks.BackupVolumes, Volumes: []string{"shop_uploads"}, Schedule: "0 */6 * * *", TargetsSet: true, Targets: []string{"b2", "nowhere"}},
	})
	if len(problems) != 1 || problems[0].Message != "app: pocketcloud.backup.target names nowhere, which is no backup target." {
		t.Errorf("problems: %v", problems)
	}
	jobs := func() map[string]*core.Record {
		t.Helper()
		records, err := f.app.FindRecordsByFilter(schema.BackupJobs, "origin='label'", "", 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		byService := map[string]*core.Record{}
		for _, record := range records {
			byService[record.GetString("service")] = record
		}
		return byService
	}
	got := jobs()
	db, app := got["db"], got["app"]
	if db == nil || db.GetString("name") != "shop-db-2" || db.GetString("source_kind") != schema.SourceDump || db.GetString("engine") != schema.DumpPostgres ||
		db.GetString("schedule") != "0 3 * * *" || retentionOf(db) != (stacks.Retention{Daily: 7, Weekly: 4, Monthly: 6}) || db.GetString("problem") != noTarget ||
		!db.GetBool("enabled") || !db.GetDateTime("next_run_at").IsZero() || db.GetString("host") != f.host.Id || len(db.GetStringSlice("label_sets")) != 0 {
		t.Errorf("db: %v", db)
	}
	if app == nil || app.GetString("name") != "shop-app" || app.GetString("source_kind") != schema.SourceStack || app.GetString("source") != "shop_uploads" ||
		app.GetString("schedule") != "0 */6 * * *" || strings.Join(app.GetStringSlice("targets"), ",") != f.target.Id ||
		!strings.Contains(app.GetString("problem"), "nowhere") || app.GetDateTime("next_run_at").IsZero() ||
		strings.Join(app.GetStringSlice("label_sets"), ",") != "schedule,targets" {
		t.Errorf("app: %v", app)
	}

	// what an admin chose where the labels say nothing stays
	db.Set("schedule", "30 1 * * *")
	db.Set("targets", []string{f.target.Id})
	if err := f.app.Save(db); err != nil {
		t.Fatal(err)
	}
	f.p.syncBackupLabels(stack, []stacks.BackupLabel{{Service: "db", Kind: stacks.BackupPostgres}})
	got = jobs()
	if db := got["db"]; db.GetString("schedule") != "30 1 * * *" || len(db.GetStringSlice("targets")) != 1 || db.GetString("problem") != "" {
		t.Errorf("db after the admin's change: %v", db)
	}
	if app := got["app"]; !app.GetBool("label_removed") || app.GetBool("enabled") || !app.GetDateTime("next_run_at").IsZero() {
		t.Errorf("app without its label: %v", app)
	}
	// the label is back
	f.p.syncBackupLabels(stack, []stacks.BackupLabel{{Service: "db", Kind: stacks.BackupPostgres}, {Service: "app", Kind: stacks.BackupVolumes, Volumes: []string{"shop_uploads"}}})
	if app := jobs()["app"]; app.GetBool("label_removed") || !app.GetBool("enabled") {
		t.Errorf("app with its label back: %v", app)
	}

	// a removed stack leaves its label jobs marked
	if err := f.app.Delete(stack); err != nil {
		t.Fatal(err)
	}
	f.p.markOrphanedLabelJobs()
	for service, job := range jobs() {
		if !job.GetBool("label_removed") || job.GetBool("enabled") {
			t.Errorf("%s of a removed stack: %v", service, job)
		}
	}
}

// TestAJobOfARemovedStackStopsUntilItIsChanged (#29): a job an admin made of
// a stack's volumes or of its database stops when the stack is removed, as it
// would fail at each run, and says why; once it backs up something else its
// problem is gone, as it was just checked.
func TestAJobOfARemovedStackStopsUntilItIsChanged(t *testing.T) {
	f := newJobFixture(t)
	shop := f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "shop"})
	other := f.save(schema.Stacks, map[string]any{"host": f.host.Id, "name": "other"})
	status, out := f.send(http.MethodPost, "/api/pc/backup-jobs", f.admin, map[string]any{"name": "shop", "source_kind": "stack", "stack": shop.Id,
		"targets": []string{f.target.Id}, "schedule": "@daily"})
	if status != http.StatusCreated {
		t.Fatalf("add: %d %v", status, out)
	}
	id := out["id"].(string)
	dump := f.job("shop-db", map[string]any{"source_kind": schema.SourceDump, "stack": shop.Id, "service": "db", "engine": schema.DumpPostgres, "targets": []string{}, "problem": noTarget})
	volume := f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})

	if err := f.app.Delete(shop); err != nil {
		t.Fatal(err)
	}
	f.p.markOrphanedLabelJobs()
	for _, jobID := range []string{id, dump.Id} {
		job, err := f.app.FindRecordById(schema.BackupJobs, jobID)
		if err != nil || job.GetString("problem") != "Its stack was removed." || job.GetBool("enabled") || !job.GetDateTime("next_run_at").IsZero() {
			t.Errorf("a job of the removed stack: %v %v", job, err)
		}
	}
	if job, _ := f.app.FindRecordById(schema.BackupJobs, volume.Id); !job.GetBool("enabled") || job.GetString("problem") != "" {
		t.Errorf("a job of a volume: %v", job)
	}
	events, err := f.app.FindRecordsByFilter(schema.Events, "kind='backup.job_disabled'", "", 0, 0)
	if err != nil || len(events) != 2 || !strings.Contains(events[0].GetString("message"), "its stack was removed") {
		t.Errorf("events: %v %v", events, err)
	}
	// once is enough
	f.p.markOrphanedLabelJobs()
	if events, _ := f.app.FindRecordsByFilter(schema.Events, "kind='backup.job_disabled'", "", 0, 0); len(events) != 2 {
		t.Errorf("%d events", len(events))
	}

	// it runs again once it backs up another stack
	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+id, f.admin, map[string]any{"stack": other.Id, "enabled": true}); status != http.StatusOK ||
		out["problem"] != "" || out["enabled"] != true || out["next_run_at"] == "" {
		t.Errorf("point it at another stack: %d %v", status, out)
	}
	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+dump.Id, f.admin, map[string]any{"source_kind": "volume", "source": "shop_data",
		"targets": []string{f.target.Id}}); status != http.StatusOK || out["problem"] != "" {
		t.Errorf("make it a volume's: %d %v", status, out)
	}
	if status, out := f.send(http.MethodPatch, "/api/pc/backup-jobs/"+dump.Id, f.admin, map[string]any{"targets": []string{}}); status != http.StatusOK || out["problem"] != noTarget {
		t.Errorf("take its target out: %d %v", status, out)
	}
}
