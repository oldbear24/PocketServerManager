package backup

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"slices"
)

// Per-database backups, restores and restore tests (#53), as the agent and
// the panel say them to each other. The agent's answers travel as a
// structpb.Struct, as those of checks do (see check.go): ResultMap turns
// these types into the map it takes. Every field of an answer is always
// there, with its zero value when it does not apply. See docs/architecture.md,
// "Per-database backups and restore tests (#53)".

// The modes of a BackupDbRestore.
const (
	// DbRestoreReplace puts the dump back into the database itself.
	DbRestoreReplace = "replace"
	// DbRestoreNew restores the dump into a new database with a new user.
	DbRestoreNew = "new"
	// DbRestoreTest restores the dump into a temporary database pc_rt_<check
	// id>, for a restore test.
	DbRestoreTest = "test"
)

// The prefixes of the extra tags of a database's or a site's snapshot
// (BackupRun.extra_tags): db:<database>, dbserver:<server>, site:<site>.
const (
	TagDatabase = "db:"
	TagServer   = "dbserver:"
	TagSite     = "site:"
)

// DatabaseTags are the extra tags of a snapshot of a database of a server.
func DatabaseTags(database, server string) []string {
	return []string{TagDatabase + database, TagServer + server}
}

// SiteTags are the extra tags of a snapshot of a PHP site's files.
func SiteTags(site string) []string {
	return []string{TagSite + site}
}

// MaxExcludes is how many --exclude patterns a run takes
// (BackupRun.excludes).
const MaxExcludes = 100

// The steps of a database restore, as its agent reports them in its
// progress, beside #30's check, stop, start and verify. A database restore
// reports check, fetch, dbrestore, stop, swap (PostgreSQL in place only),
// start and verify, in that order, leaving out what it does not do.
const (
	// StepFetch copies the dump from the snapshot into the server's
	// container.
	StepFetch = "fetch"
	// StepDbRestore restores the dump with the server's own client.
	StepDbRestore = "dbrestore"
	// StepSwap puts PostgreSQL's restored copy in place of the database.
	StepSwap = "swap"
)

// The phases of a database restore in place, as the agent's restore marker
// keeps them: an agent that restarted reads the phase to know what is left
// to do.
const (
	// PhaseScratch (PostgreSQL): the dump goes into the scratch copy while
	// the application runs. Nothing was stopped or changed.
	PhaseScratch = "scratch"
	// PhaseRehearsal (MariaDB): the dump goes into the scratch copy, which
	// is dropped again. Nothing was stopped or changed.
	PhaseRehearsal = "rehearsal"
	// PhaseStopped: the application is stopped; the database is unchanged.
	PhaseStopped = "stopped"
	// PhaseSwapped (PostgreSQL): the copy is the database, the data before
	// it pc_old_<restore id>.
	PhaseSwapped = "swapped"
	// PhaseEmptied (MariaDB): the database was dropped and is being
	// restored: it holds part of the dump.
	PhaseEmptied = "emptied"
	// PhaseRestored (MariaDB): the database holds the whole dump.
	PhaseRestored = "restored"
)

// RestoreTestOK is a passed restore test's status in its answer.
const RestoreTestOK = "ok"

// The classes of a failed restore test, its answer's failure. The first
// four say the backup could not be restored, and fail the test; the others
// say the test could not tell, and end it in error.
const (
	// TestDamaged: restic could not read the snapshot's data, such as
	// "ciphertext verification failed", or a pack or a blob not found.
	TestDamaged = "damaged"
	// TestIncomplete: the dump is cut short, or pg_restore cannot read its
	// list.
	TestIncomplete = "incomplete"
	// TestRestore: the client failed to restore the dump; the error holds
	// its last lines, with the passwords hidden.
	TestRestore = "restore"
	// TestTables: the tables restored are not as many as the dump holds.
	TestTables = "tables"
	// TestUnreachable: the repository could not be reached.
	TestUnreachable = "unreachable"
	// TestRepository: the repository is gone, or its password does not open
	// it.
	TestRepository = FailureRepository
	// TestLocked: another restic held the repository's exclusive lock.
	TestLocked = FailureLocked
	// TestSpace: the server has too little room for the copy.
	TestSpace = "space"
	// TestContainer: the server's container is not running, or is not a
	// shared server's.
	TestContainer = "container"
	// TestSignIn: the database's user did not sign in with the panel's
	// password.
	TestSignIn = "sign_in"
	// TestInterrupted: the agent restarted while the test went.
	TestInterrupted = FailureInterrupted
)

// TestFailures lists the classes of a failed restore test.
var TestFailures = []string{
	TestDamaged, TestIncomplete, TestRestore, TestTables, TestUnreachable, TestRepository,
	TestLocked, TestSpace, TestContainer, TestSignIn, TestInterrupted,
}

// TestFailed says whether a failure class means that the backup could not
// be restored: the test failed. Any other failure ends it in error, and the
// panel tries it again.
func TestFailed(failure string) bool {
	return slices.Contains([]string{TestDamaged, TestIncomplete, TestRestore, TestTables}, failure)
}

// The alerts of per-database backups. The first two are keyed by the job,
// on the job's server; the third by the temporary database's record.
const (
	// AlertRestoreTestFailed is open, critical, while the newest completed
	// restore test of a job failed: the backup could not be restored.
	AlertRestoreTestFailed = "backup_restore_test_failed"
	// AlertRestoreTestUnverified is open, a warning, while no restore test
	// of a job with snapshots passed lately.
	AlertRestoreTestUnverified = "backup_restore_test_unverified"
	// AlertTemporaryLeft is open, a warning, while the panel failed twice to
	// drop a temporary database.
	AlertTemporaryLeft = "db_temporary_left"
)

// The events of per-database backups.
const (
	EventRestoreTestOK      = "backup.restore_test_ok"
	EventRestoreTestFailed  = "backup.restore_test_failed"
	EventRestoreTestError   = "backup.restore_test_error"
	EventRestoreTestSkipped = "backup.restore_test_skipped"
	// EventJobDisabled is #29's event of a job the panel turned off, which a
	// database's or a site's job gets when its database or site goes.
	EventJobDisabled = "backup.job_disabled"
	// EventTemporaryDropped: the panel dropped a temporary database.
	EventTemporaryDropped = "db.temporary_dropped"
	// EventTemporaryFound: a server reported a temporary database the panel
	// had no record of, which it drops in two hours.
	EventTemporaryFound = "db.temporary_found"
)

// StoppedApp is a container a restore stopped.
type StoppedApp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// DbRestoreResult is what an agent's database restore answers: the result of
// a backup.restore whose database is set and that is no dry run.
type DbRestoreResult struct {
	// FilesRestored is 1 once the dump was restored, as a restore of one
	// file; BytesRestored is the dump's size.
	FilesRestored int64 `json:"files_restored"`
	BytesRestored int64 `json:"bytes_restored"`
	// Tables are the tables restored, TablesExpected those the dump holds.
	Tables         int64 `json:"tables"`
	TablesExpected int64 `json:"tables_expected"`
	// Previous is, after an in-place restore of PostgreSQL, the temporary
	// database that holds the data before it, pc_old_<restore id>.
	Previous string `json:"previous"`
	// ConnectionsEnded are the sessions the restore ended, besides those of
	// the containers it stopped.
	ConnectionsEnded int64 `json:"connections_ended"`
	// AppStopped, AppStarted, AppNotRunning and StartErrors are #30's: what
	// stopped, what started again, what did not stay up, and why.
	AppStopped    []StoppedApp `json:"app_stopped"`
	AppStarted    []string     `json:"app_started"`
	AppNotRunning []string     `json:"app_not_running"`
	StartErrors   []string     `json:"start_errors"`
	// Partial is a MariaDB restore in place that failed after the database
	// was emptied: it holds part of the dump, and AppLeftStopped are the
	// containers that stay stopped, with their restart policies put back.
	Partial        bool     `json:"partial"`
	AppLeftStopped []string `json:"app_left_stopped"`
	// Dropped is a failed restore into a new database whose database and
	// user were dropped again.
	Dropped bool `json:"dropped"`
	// Interrupted is a restore the agent finished after a restart.
	Interrupted bool `json:"interrupted"`
	// Log is the end of the restore's output, with every secret hidden.
	Log string `json:"log"`
}

// RestoreTestResult is what an agent's restore test answers, the result of
// backup.restore_test, also with its error when it failed.
type RestoreTestResult struct {
	// Status is RestoreTestOK when the test passed, empty when it failed.
	Status string `json:"status"`
	// Failure is the class of a failed test, one of TestFailures.
	Failure string `json:"failure"`
	// Tables are the tables restored, TablesExpected those the dump holds.
	Tables         int64 `json:"tables"`
	TablesExpected int64 `json:"tables_expected"`
	// Bytes is the dump's size; DurationMs how long the test took.
	Bytes      int64 `json:"bytes"`
	DurationMs int64 `json:"duration_ms"`
	// Created says the temporary database was created, and is there for the
	// panel to drop.
	Created bool `json:"created"`
	// Dropped are the databases of drop_before that were dropped.
	Dropped []string `json:"dropped"`
	// Log is the end of the test's output, with every secret hidden.
	Log string `json:"log"`
}

// DbRestorePreview is an agent's dry run of a database restore: #30's
// preview with a single root, the dump, and what the database and the
// server say.
type DbRestorePreview struct {
	DryRun     bool            `json:"dry_run"`
	Snapshot   PreviewSnapshot `json:"snapshot"`
	Roots      []PreviewRoot   `json:"roots"`
	WouldStop  []WouldStop     `json:"would_stop"`
	TotalFiles int64           `json:"total_files"`
	TotalBytes int64           `json:"total_bytes"`
	Database   DbPreview       `json:"database"`
	// Free is the room on the server, Needed what the restore needs.
	Free   Space `json:"free"`
	Needed Space `json:"needed"`
	// Notes are sentences for people, as #30's.
	Notes []string `json:"notes"`
}

// PreviewSnapshot is the snapshot a preview read: its full id, its time
// (RFC 3339) and its paths, /<file> for a database's.
type PreviewSnapshot struct {
	ID    string   `json:"id"`
	Time  string   `json:"time"`
	Paths []string `json:"paths"`
}

// PreviewRoot is #30's root of a preview. A database preview has one: index
// 0, source /<file>, destination "database <name>", in_snapshot, exists
// (the database), and entries, the tables it has now.
type PreviewRoot struct {
	Index       int    `json:"index"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	InSnapshot  bool   `json:"in_snapshot"`
	Exists      bool   `json:"exists"`
	Entries     int64  `json:"entries"`
}

// WouldStop is a container a restore would stop.
type WouldStop struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	Service string `json:"service"`
}

// DbPreview is what a database restore's preview found on the server.
type DbPreview struct {
	// Exists and UserExists say the database and the user exist; a restore
	// into a new database needs neither.
	Exists     bool `json:"exists"`
	UserExists bool `json:"user_exists"`
	// SizeBytes and Tables are the database's now (replace).
	SizeBytes int64 `json:"size_bytes"`
	Tables    int64 `json:"tables"`
	// ConnectionsOther are sessions of the database from outside what the
	// restore stops.
	ConnectionsOther int64 `json:"connections_other"`
	// SignIn says the user signed in with the panel's password (replace and
	// test; a new user does not exist yet, and is not tried).
	SignIn bool `json:"sign_in"`
	// Settings are PostgreSQL's settings of the database (name=value), which
	// a restore does not carry.
	Settings []string `json:"settings"`
	// Sandbox says the MariaDB client takes --sandbox.
	Sandbox bool `json:"sandbox"`
}

// Space is bytes on the server: in the database server's data directory,
// and in its container's /tmp, where the dump is staged.
type Space struct {
	Data int64 `json:"data"`
	Tmp  int64 `json:"tmp"`
}

// ResultMap turns an answer into the map[string]any the stream carries.
func ResultMap(answer any) (map[string]any, error) {
	raw, err := json.Marshal(answer)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil || result == nil {
		return nil, fmt.Errorf("an answer is a JSON object, not %s", raw)
	}
	return result, nil
}

// AutoSchedule is the schedule of a server's automatic backups when its
// auto_backup_schedule is empty: daily, between 01:00 and 02:45 UTC, at a
// quarter of an hour that the server's record id picks, so that the servers
// do not all back up at once, and away from the label jobs' 03:00 and the
// integrity checks' Sunday 05:00. With k = FNV-1a 32 of the id modulo 8,
// it is "M H * * *" with H = 1 + k/4 and M = 15 × (k mod 4).
func AutoSchedule(hostID string) string {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(hostID))
	k := hash.Sum32() % 8
	return fmt.Sprintf("%d %d * * *", 15*(k%4), 1+k/4)
}
