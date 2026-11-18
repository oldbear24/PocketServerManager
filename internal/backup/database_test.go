package backup

import (
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// fieldKeys are the JSON names of a struct's fields, in order.
func fieldKeys(t *testing.T, v any) []string {
	t.Helper()
	typ := reflect.TypeOf(v)
	keys := []string{}
	for i := range typ.NumField() {
		name, options, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if options != "" {
			t.Errorf("%s.%s is %s: every field of an answer is always there", typ.Name(), typ.Field(i).Name, options)
		}
		keys = append(keys, name)
	}
	return keys
}

// TestTheAnswersKeepTheirJSONNames pins the JSON of the database restore's,
// the restore test's and the preview's answers, which the agent writes, the
// panel reads and the web shows (#53).
func TestTheAnswersKeepTheirJSONNames(t *testing.T) {
	for name, tc := range map[string]struct {
		value any
		keys  []string
	}{
		"restore": {DbRestoreResult{}, []string{
			"files_restored", "bytes_restored", "tables", "tables_expected", "previous", "connections_ended",
			"app_stopped", "app_started", "app_not_running", "start_errors", "partial", "app_left_stopped", "dropped",
			"interrupted", "log",
		}},
		"test": {RestoreTestResult{}, []string{
			"status", "failure", "tables", "tables_expected", "bytes", "duration_ms", "created", "dropped", "log",
		}},
		"preview": {DbRestorePreview{}, []string{
			"dry_run", "snapshot", "roots", "would_stop", "total_files", "total_bytes", "database", "free", "needed", "notes",
		}},
		"snapshot":    {PreviewSnapshot{}, []string{"id", "time", "paths"}},
		"root":        {PreviewRoot{}, []string{"index", "source", "destination", "in_snapshot", "exists", "entries"}},
		"would stop":  {WouldStop{}, []string{"name", "project", "service"}},
		"database":    {DbPreview{}, []string{"exists", "user_exists", "size_bytes", "tables", "connections_other", "sign_in", "settings", "sandbox"}},
		"space":       {Space{}, []string{"data", "tmp"}},
		"stopped app": {StoppedApp{}, []string{"id", "name"}},
	} {
		if got := fieldKeys(t, tc.value); !slices.Equal(got, tc.keys) {
			t.Errorf("%s: %v, want %v", name, got, tc.keys)
		}
	}
}

// TestAnAnswerTravelsAsAMap: structpb takes no struct or typed slice, so an
// answer goes through JSON, and comes back from the panel's JSON the same.
func TestAnAnswerTravelsAsAMap(t *testing.T) {
	answer := DbRestoreResult{
		FilesRestored: 1, BytesRestored: 1 << 33, Tables: 12, TablesExpected: 12, Previous: "pc_old_abcdefghijklmno",
		AppStopped: []StoppedApp{{ID: "c1", Name: "shop-web-1"}}, AppStarted: []string{"shop-web-1"}, Log: "restored",
	}
	result, err := ResultMap(answer)
	if err != nil {
		t.Fatal(err)
	}
	if stopped, ok := result["app_stopped"].([]any); !ok || len(stopped) != 1 || stopped[0].(map[string]any)["name"] != "shop-web-1" {
		t.Errorf("app_stopped is %#v", result["app_stopped"])
	}
	if started, ok := result["app_started"].([]any); !ok || started[0] != "shop-web-1" {
		t.Errorf("app_started is %#v", result["app_started"])
	}
	// nil lists are there, as null, as every other field
	if v, ok := result["app_left_stopped"]; !ok || v != nil {
		t.Errorf("app_left_stopped is %#v, %v", v, ok)
	}
	if result["bytes_restored"] != float64(1<<33) || result["partial"] != false {
		t.Errorf("the numbers and flags: %v", result)
	}
	raw, _ := json.Marshal(result)
	var back DbRestoreResult
	if err := json.Unmarshal(raw, &back); err != nil || !reflect.DeepEqual(back, answer) {
		t.Errorf("back: %+v, %v", back, err)
	}

	test, err := ResultMap(RestoreTestResult{Status: RestoreTestOK, Tables: 3, TablesExpected: 3, Created: true, Dropped: []string{"pc_rt_abcdefghijklmno"}})
	if err != nil || test["status"] != "ok" || test["failure"] != "" || len(test["dropped"].([]any)) != 1 {
		t.Errorf("a test's answer: %v, %v", test, err)
	}
	if _, err := ResultMap([]string{"not", "an", "object"}); err == nil {
		t.Error("a list passed as an answer")
	}
}

// TestRestoreTestFailuresSayWhetherTheBackupFailed: damaged, incomplete,
// restore and tables mean the backup could not be restored, a failed test;
// the others that the test could not tell, an error the panel retries.
func TestRestoreTestFailuresSayWhetherTheBackupFailed(t *testing.T) {
	failed := []string{}
	for _, failure := range TestFailures {
		if TestFailed(failure) {
			failed = append(failed, failure)
		}
		// backup_checks.failure holds 32 characters
		if failure == "" || len(failure) > 32 || strings.ToLower(failure) != failure {
			t.Errorf("the class %q", failure)
		}
	}
	sort.Strings(failed)
	if !slices.Equal(failed, []string{"damaged", "incomplete", "restore", "tables"}) {
		t.Errorf("failed tests: %v", failed)
	}
	want := []string{"damaged", "incomplete", "restore", "tables", "unreachable", "repository", "locked", "space", "container", "sign_in", "interrupted"}
	if !slices.Equal(TestFailures, want) {
		t.Errorf("the classes are %v, not %v", TestFailures, want)
	}
	if TestFailed("") || TestFailed("timeout") || TestFailed("Damaged") {
		t.Error("an unknown class failed the test")
	}
}

// TestTheDatabaseNamesOfTheContract: the agent reports, and the web, the
// panel and the docs read, these names.
func TestTheDatabaseNamesOfTheContract(t *testing.T) {
	for got, want := range map[string]string{
		CapabilityDatabase: "backup.database",
		DbRestoreReplace:   "replace", DbRestoreNew: "new", DbRestoreTest: "test",
		StepFetch: "fetch", StepRehearse: "rehearse", StepDbRestore: "dbrestore", StepSwap: "swap",
		PhaseScratch: "scratch", PhaseRehearsal: "rehearsal", PhaseStopped: "stopped",
		PhaseSwapped: "swapped", PhaseEmptied: "emptied", PhaseRestored: "restored",
		AlertRestoreTestFailed:     "backup_restore_test_failed",
		AlertRestoreTestUnverified: "backup_restore_test_unverified",
		AlertTemporaryLeft:         "db_temporary_left",
		EventRestoreTestOK:         "backup.restore_test_ok",
		EventRestoreTestFailed:     "backup.restore_test_failed",
		EventRestoreTestError:      "backup.restore_test_error",
		EventRestoreTestSkipped:    "backup.restore_test_skipped",
		EventJobDisabled:           "backup.job_disabled",
		EventTemporaryDropped:      "db.temporary_dropped",
		EventTemporaryFound:        "db.temporary_found",
		RestoreTestOK:              "ok",
	} {
		if got != want {
			t.Errorf("%q is now %q", want, got)
		}
	}
	if MaxExcludes != 100 {
		t.Errorf("a run takes %d excludes", MaxExcludes)
	}
}

// TestTheExtraTagsNameTheDatabaseAndTheSite: they pass the backup tool's
// tag checks, and a move only changes the server's.
func TestTheExtraTagsNameTheDatabaseAndTheSite(t *testing.T) {
	if got := DatabaseTags("shop", "db-main"); !slices.Equal(got, []string{"db:shop", "dbserver:db-main"}) {
		t.Errorf("a database's tags: %v", got)
	}
	if got := SiteTags("wp"); !slices.Equal(got, []string{"site:wp"}) {
		t.Errorf("a site's tags: %v", got)
	}
	for _, tag := range append(DatabaseTags("shop_staging", "pg18"), SiteTags("my-site")...) {
		if strings.ContainsAny(tag, ", \n") {
			t.Errorf("%q would be refused", tag)
		}
	}
}

// TestAutomaticBackupsSpreadOverTheNight pins the default schedule of a
// server's automatic backups: a quarter of an hour from 01:00 to 02:45 that
// its id picks. The values come from an independent FNV-1a.
func TestAutomaticBackupsSpreadOverTheNight(t *testing.T) {
	for id, want := range map[string]string{
		"host00000000007": "0 1 * * *",
		"host00000000004": "15 1 * * *",
		"host00000000001": "30 1 * * *",
		"host00000000006": "45 1 * * *",
		"host00000000003": "0 2 * * *",
		"host00000000008": "15 2 * * *",
		"host00000000005": "30 2 * * *",
		"host00000000002": "45 2 * * *",
		"vm00000000000ab": "45 2 * * *",
	} {
		if got := AutoSchedule(id); got != want {
			t.Errorf("AutoSchedule(%q) = %q, want %q", id, got, want)
		}
	}
	seen := map[string]bool{}
	for i := range 2000 {
		seen[AutoSchedule(strings.Repeat("x", i%7)+string(rune('a'+i%26))+strings.Repeat("0", i%13))] = true
	}
	if len(seen) != 8 {
		t.Errorf("the schedules are %v", seen)
	}
}
