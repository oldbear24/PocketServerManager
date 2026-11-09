package panel

import (
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
