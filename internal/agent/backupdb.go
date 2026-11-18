package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
)

// Database restores and restore tests (#53). A database job's snapshot
// holds one file, the database's dump, which a backup-tool job reads out of
// the snapshot as a tar; the agent takes its one file and copies it into
// the shared server's container, where the server's own client restores it
// as the database's own user, over the container's address, whose
// privileges reach that database only: a dump that names another database
// fails on them (AC1). In place, PostgreSQL's restore goes into a scratch
// copy while the application runs, and the copy is put in place of the
// database in one transaction once the application stopped, keeping the
// data before it as pc_old_<restore id>; MariaDB's goes into a scratch copy
// first, a rehearsal, and then into the database, dropped and created
// again, once the application stopped. A restore into a new database makes
// the database and its user first, and drops both when it fails. A restore
// test restores the newest dump into a temporary database pc_rt_<check id>
// and compares its tables with the dump's. See docs/architecture.md, "Per-
// database backups and restore tests (#53)".

const (
	// stageReserve is the room a staged dump needs in the server's /tmp
	// besides its own size, and dataReserve the room a restored copy needs
	// in the server's data directory besides 1.2 times the larger of the
	// database and its dump.
	stageReserve = 256 << 20
	dataReserve  = 512 << 20
	// swapTries is how often the swap of PostgreSQL's copy is tried while
	// a session that came back holds the database.
	swapTries = 5
	// clientLines is how many of the client's last lines a failure says.
	clientLines = 4
	// stagedRemoveTimeout bounds the removal of a staged dump, which runs
	// whatever ended the restore.
	stagedRemoveTimeout = time.Minute
	// swapSettleTime is how long the server's client may still be putting
	// PostgreSQL's copy in place after the agent lost sight of it: each
	// statement of the swap waits 10 s at most for a lock, and a rename 5 s
	// for the sessions that end.
	swapSettleTime = time.Minute
	// rehearsalMargin is the time a MariaDB restore in place needs besides
	// as long as its rehearsal took and a quarter more, before it empties
	// the database: for the sessions, the recreation and the count.
	rehearsalMargin = time.Minute
)

// rehearsalClock is the clock a MariaDB restore in place times its
// rehearsal by, and the time it has left; tests move it.
var rehearsalClock = time.Now

// swapWait is how long the agent waits before it tries the swap again;
// tests shorten it.
var swapWait = 2 * time.Second

// swapSettled waits until a swap begun at started, whose process the agent
// lost sight of, has ended in the server's container, or ctx ends; tests
// replace it.
var swapSettled = func(ctx context.Context, started time.Time) {
	timer := time.NewTimer(time.Until(started.Add(swapSettleTime)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// stopProject is the name of a stack a database restore stops besides its
// own.
var stopProject = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// recordID is a panel record's id.
var recordID = regexp.MustCompile(`^[a-z0-9]{15}$`)

// restoreTargets counts where a restore goes: its original location, a new
// folder, a database.
func restoreTargets(payload *agentv1.BackupRestore) int {
	targets := 0
	for _, set := range []bool{len(payload.GetOriginal()) > 0, payload.GetTargetPath() != "", payload.GetDatabase() != nil} {
		if set {
			targets++
		}
	}
	return targets
}

// checkDatabaseRestore checks what a database restore asks, before the
// server is looked at: its mode, its database, user and file, and its
// copies, which are the restore's own.
func checkDatabaseRestore(command *agentv1.Command) error {
	payload := command.GetBackupRestore()
	db := payload.GetDatabase()
	if len(payload.GetInclude()) > 0 || payload.GetReplace() || payload.GetSource() != "" {
		return errors.New("a database restore restores the database's dump whole: it picks no paths, and has no source or replace")
	}
	engine, err := checkDatabaseNames(db)
	if err != nil {
		return err
	}
	if db.GetConnection().GetAdminPassword() == "" || !payload.GetDryRun() && db.GetPassword() == "" {
		return errors.New("the command has no database password: the panel adds them when it delivers it")
	}
	stops := len(db.GetStopProjects()) > 0 || len(db.GetStopContainers()) > 0
	switch db.GetMode() {
	case backup.DbRestoreReplace:
		if !payload.GetDryRun() && !payload.GetOverwrite() {
			return errors.New("a restore into the database replaces what it holds, and that was not confirmed")
		}
		if want := databases.ScratchName(payload.GetRestoreId()); db.GetScratch() != want || databases.ValidTemporaryName(want) != nil {
			return fmt.Errorf("the scratch copy of a restore is %s and its record's id, not %q", databases.TemporaryScratch, db.GetScratch())
		}
		want := ""
		if engine.Name == databases.Postgres.Name {
			want = databases.PreviousName(payload.GetRestoreId())
		}
		if db.GetPrevious() != want {
			return fmt.Errorf("the copy of the data before a restore is %q, not %q", want, db.GetPrevious())
		}
	case backup.DbRestoreNew:
		if payload.GetOverwrite() || db.GetScratch() != "" || db.GetPrevious() != "" || stops || payload.GetStack() != "" {
			return errors.New("a restore into a new database overwrites nothing, stops nothing and makes no copies")
		}
	default:
		return fmt.Errorf("a database restore puts the dump back into its database or into a new one, not %q", db.GetMode())
	}
	for _, project := range db.GetStopProjects() {
		if !stopProject.MatchString(project) {
			return fmt.Errorf("%q is not a stack's name", project)
		}
	}
	for _, name := range db.GetStopContainers() {
		if !siteContainerName.MatchString(name) {
			return fmt.Errorf("%q is not a site's container: <site>-php or <site>-worker", name)
		}
	}
	return nil
}

// checkDatabaseNames checks the server, the database, its user and the
// file of a database restore or restore test, and says the engine.
func checkDatabaseNames(db *agentv1.BackupDbRestore) (databases.Engine, error) {
	engine, err := connectionOf(db.GetConnection())
	if err != nil {
		return engine, err
	}
	if err := databases.ValidDatabaseName(engine, db.GetConnection().GetAdminUser()); err != nil && db.GetConnection().GetAdminUser() != engine.AdminUser {
		return engine, fmt.Errorf("%q is not the server's administrator", db.GetConnection().GetAdminUser())
	}
	if err := databases.ValidDatabaseName(engine, db.GetDatabase()); err != nil {
		return engine, err
	}
	if err := databases.ValidDatabaseName(engine, db.GetUser()); err != nil {
		return engine, err
	}
	if _, err := databases.FileDatabase(engine, db.GetFile()); err != nil {
		return engine, err
	}
	if db.GetMode() != backup.DbRestoreTest {
		if err := databases.ReservedPrefix(db.GetDatabase()); err != nil {
			return engine, fmt.Errorf("%s is one of PocketCloud's temporary databases, which no restore writes: %w", db.GetDatabase(), err)
		}
	}
	return engine, nil
}

// dbRun is a database restore or restore test under way: the server's
// container, which is a shared server's, and what is staged in it.
type dbRun struct {
	a        *Agent
	command  *agentv1.Command
	repo     *agentv1.BackupRepository
	snapshot string
	jobTag   string
	db       *agentv1.BackupDbRestore
	engine   databases.Engine
	// container is the server's container's id, name its name, image the
	// image it runs, as it names it
	container, name, image string
	// secrets are masked in whatever the run says: the passwords of the
	// administrator, the user and the repository
	secrets  []string
	log      *tailBuffer
	out      io.Writer
	reporter *Reporter
	// staged is the dump in the container, and size its size
	staged string
	size   int64
}

// errNotSharedServer refuses a container that is not a running shared
// server's.
var errNotSharedServer error = sentenceError("Database restores go into a shared server's container only.")

// dbRunOf checks the server's container: it runs, and is a shared server's.
func (a *Agent) dbRunOf(ctx context.Context, command *agentv1.Command, db *agentv1.BackupDbRestore, repo *agentv1.BackupRepository,
	snapshot, jobTag string, reporter *Reporter) (*dbRun, error) {
	engine, err := connectionOf(db.GetConnection())
	if err != nil {
		return nil, err
	}
	info, err := a.Docker.Inspect(ctx, db.GetConnection().GetContainerId())
	if err != nil && !dockerapi.IsNotFound(err) {
		return nil, err
	}
	if err != nil || !info.State.Running || labelOf(info, "pocketcloud.role") != "db" {
		return nil, errNotSharedServer
	}
	image, _ := info.Config["Image"].(string)
	secrets := append(backup.Secrets(repo.GetPassword(), repo.GetEnv(), repo.GetSshKey()), db.GetConnection().GetAdminPassword(), db.GetPassword())
	log := &tailBuffer{limit: backupLogTail, mask: maskValues(secrets)}
	return &dbRun{a: a, command: command, repo: repo, snapshot: snapshot, jobTag: jobTag, db: db, engine: engine,
		container: info.ID, name: strings.TrimPrefix(info.Name, "/"), image: image, secrets: secrets, log: log,
		out: io.MultiWriter(reporter, log), reporter: reporter}, nil
}

// labelOf is a label of an inspected container.
func labelOf(info *dockerapi.ContainerInspect, label string) string {
	labels, _ := info.Config["Labels"].(map[string]any)
	value, _ := labels[label].(string)
	return value
}

// run runs a script in the server's container, and says what it printed;
// a failure says the client's last lines, every password masked.
func (r *dbRun) run(ctx context.Context, script databases.Script) (string, error) {
	var out bytes.Buffer
	result, err := r.a.Docker.Run(ctx, r.container, dockerapi.ExecOptions{Cmd: script.Cmd, Env: script.Env}, &out)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		message := lastLines(maskSecrets(result.Stderr, r.secrets...), clientLines)
		if message == "" {
			message = "no message"
		}
		return out.String(), &clientError{code: result.ExitCode, message: message}
	}
	return out.String(), nil
}

// clientError is the server's client that failed.
type clientError struct {
	code    int
	message string
}

func (e *clientError) Error() string {
	return fmt.Sprintf("the server's client failed with exit code %d: %s", e.code, e.message)
}

// signInFailed says whether the client failed to sign in as the user.
func (e *clientError) signInFailed() bool {
	return strings.Contains(e.message, "password authentication failed") ||
		strings.Contains(e.message, "Access denied for user") && strings.Contains(e.message, "(using password:")
}

// admin is the server's administrator and password.
func (r *dbRun) admin() (string, string) {
	return r.db.GetConnection().GetAdminUser(), r.db.GetConnection().GetAdminPassword()
}

// sql runs statements as the administrator.
func (r *dbRun) sql(ctx context.Context, sql string) (string, error) {
	user, password := r.admin()
	return r.run(ctx, databases.Statements(r.engine, user, password, sql))
}

// tables counts a database's tables, as a dump's count has them.
func (r *dbRun) tables(ctx context.Context, database string) (int64, error) {
	user, password := r.admin()
	out, err := r.run(ctx, databases.CountBaseTables(r.engine, user, password, database))
	if err != nil {
		return 0, err
	}
	return databases.ParseCount(out)
}

// databaseSize is the size of a database, 0 when the server does not say.
func (r *dbRun) databaseSize(ctx context.Context, database string) int64 {
	out, err := r.sql(ctx, databases.StatsSQL(r.engine))
	if err != nil {
		return 0
	}
	stats, err := databases.ParseStats(r.engine, out)
	if err != nil {
		return 0
	}
	return stats.Databases[database].SizeBytes
}

// endSessions ends a database's sessions, and says how many it ended.
func (r *dbRun) endSessions(ctx context.Context, database string) (int64, error) {
	user, password := r.admin()
	out, err := r.run(ctx, databases.EndSessions(r.engine, user, password, database))
	if err != nil {
		return 0, err
	}
	return databases.ParseCount(out)
}

// free is the room in the server's data directory and its /tmp.
func (r *dbRun) free(ctx context.Context) (backup.Space, error) {
	out, err := r.run(ctx, databases.FreeSpace(r.engine.DataDirOf(r.image)))
	if err != nil {
		return backup.Space{}, err
	}
	data, tmp, err := databases.ParseFreeSpace(out)
	return backup.Space{Data: data, Tmp: tmp}, err
}

// neededSpace is the room a restore needs: the dump staged in /tmp, and a
// copy of the larger of the database and its dump in the data directory.
func neededSpace(databaseSize, fileSize int64) backup.Space {
	return backup.Space{Tmp: fileSize + stageReserve, Data: max(databaseSize, fileSize)*6/5 + dataReserve}
}

// discard drops temporary databases of the server: each revoke on its own,
// as MariaDB says "no such grant" of one there is none of.
func (a *Agent) discard(ctx context.Context, container string, engine databases.Engine, connection *agentv1.DbConnection,
	names []string, revokeFrom string, secrets ...string) error {
	revokes, drop, err := databases.DiscardSQL(engine, names, revokeFrom)
	if err != nil {
		return err
	}
	user, password := connection.GetAdminUser(), connection.GetAdminPassword()
	for _, revoke := range revokes {
		err := a.runScript(ctx, container, databases.Statements(engine, user, password, revoke), io.Discard, secrets...)
		if err != nil && !strings.Contains(err.Error(), "no such grant") {
			return err
		}
	}
	return a.runScript(ctx, container, databases.Statements(engine, user, password, drop), io.Discard, secrets...)
}

// discard drops temporary databases of the run's server.
func (r *dbRun) discard(ctx context.Context, names []string, revokeFrom string) error {
	return r.a.discard(ctx, r.container, r.engine, r.db.GetConnection(), names, revokeFrom, r.secrets...)
}

// sweep removes the dumps restores left in the container more than a day
// ago, as when the agent restarted during one.
func (r *dbRun) sweep(ctx context.Context) {
	if _, err := r.run(ctx, databases.SweepStaged()); err != nil {
		fmt.Fprintln(r.out, "Old dumps of restores could not be removed:", err)
	}
}

// removeStaged removes a dump a restore or a restore test staged in a
// server's container: a path RemoveStaged takes only.
func (a *Agent) removeStaged(ctx context.Context, container, file string) error {
	script, err := databases.RemoveStaged(file)
	if err != nil {
		return err
	}
	return a.runScript(ctx, container, script, io.Discard)
}

// unstageInterrupted removes the dump an interrupted database restore
// staged in its server's container, which its deferred removal did not:
// the agent takes no command yet, so no restore of its own needs it. A
// process the agent before started may still read it: removing a file that
// a process has open is safe on Linux, which keeps it for the process
// until it closes it.
func (a *Agent) unstageInterrupted(ctx context.Context, marker restoreMarker) {
	engine, ok := databases.EngineOf(marker.Database.Engine)
	if !ok || marker.Database.Container == "" {
		return
	}
	file := databases.StagedDir + "/" + databases.StagedName(engine, marker.CommandID)
	work, cancel := context.WithTimeout(ctx, stagedRemoveTimeout)
	defer cancel()
	if err := a.removeStaged(work, marker.Database.Container, file); err != nil {
		a.Log.Warn("failed to remove the dump an interrupted restore staged", "container", marker.Database.Container, "file", file, "error", err)
	}
}

// unstage removes the staged dump, whatever ended the restore.
func (r *dbRun) unstage() {
	if r.staged == "" {
		return
	}
	script, err := databases.RemoveStaged(r.staged)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), stagedRemoveTimeout)
	defer cancel()
	if _, err := r.run(ctx, script); err != nil {
		r.a.Log.Warn("failed to remove a staged dump", "container", r.name, "file", r.staged, "error", err)
	}
}

// stageError is a failure of a restore test's class, said as it is.
type stageError struct {
	class   string
	message string
}

func (e *stageError) Error() string { return e.message }

// failureClass is the class of a restore test's failure (backup.Test*).
func failureClass(ctx context.Context, err error) string {
	var staged *stageError
	var client *clientError
	switch {
	case ctx.Err() != nil:
		return backup.TestInterrupted
	case errors.As(err, &staged):
		return staged.class
	case errors.As(err, &client) && client.signInFailed():
		return backup.TestSignIn
	}
	return backup.TestContainer
}

// fetchFailure classes what a job that read the snapshot said when it
// failed: data restic could not read is the backup's damage; a lock, a
// repository that is gone or does not open, or one that could not be
// reached is no word on the backup.
func fetchFailure(err error) string {
	message := err.Error()
	switch {
	case strings.Contains(message, "ciphertext verification failed"), damagedData.MatchString(message):
		return backup.TestDamaged
	case strings.Contains(message, "repository is already locked"), strings.Contains(message, "unable to create lock"):
		return backup.TestLocked
	case strings.Contains(message, "the repository is gone"), strings.Contains(message, "the password is not its own"),
		strings.Contains(message, "no matching ID found"), strings.Contains(message, "is not in the repository"),
		strings.Contains(message, "is not one of this job's"):
		return backup.TestRepository
	}
	return backup.TestUnreachable
}

// damagedData is restic's word of a pack or a blob it could not find or
// read.
var damagedData = regexp.MustCompile(`(?i)(pack|blob) [0-9a-f<>/ ]*not found|not found in index|data/[0-9a-f]+.*no such file`)

// stage reads the dump out of the snapshot and copies it into the server's
// container, as StagedName names it: a backup-tool job writes the snapshot
// as a tar, whose one regular file must be the dump; the tar's header says
// its size, which room checks before a byte is copied.
func (r *dbRun) stage(ctx context.Context, room func(size int64) error) error {
	r.staged = databases.StagedDir + "/" + databases.StagedName(r.engine, r.command.GetId())
	reader, writer := io.Pipe()
	var jobEnded atomic.Bool
	type copied struct {
		err   error
		first bool
	}
	done := make(chan copied, 1)
	go func() {
		err := r.copyDump(ctx, reader, room)
		first := !jobEnded.Load()
		if err != nil {
			// the job's writes fail, and it is stopped
			reader.CloseWithError(err)
		} else {
			_, _ = io.Copy(io.Discard, reader)
		}
		done <- copied{err: err, first: first}
	}()
	job := backupJobSpec{
		args:         []string{"dump", "--snapshot", r.snapshot, "--path", "/", "--expect-tag", r.jobTag},
		attachOutput: writer, stopGrace: backupStopGrace, labels: commandLabels(r.command),
	}
	_, jobErr := r.a.backupJob(ctx, r.repo, job, r.out)
	jobEnded.Store(true)
	writer.CloseWithError(jobErr)
	result := <-done
	var refused *stageError
	switch {
	case errors.As(result.err, &refused):
		return result.err
	case result.err != nil && result.first:
		return fmt.Errorf("the dump could not be copied into %s: %w", r.name, result.err)
	case jobErr != nil:
		return &stageError{class: fetchFailure(jobErr), message: "the dump could not be read from the snapshot: " + jobErr.Error()}
	case result.err != nil:
		return fmt.Errorf("the dump could not be copied into %s: %w", r.name, result.err)
	}
	fmt.Fprintf(r.out, "Copied the dump %s (%s) into %s\n", r.db.GetFile(), sizeText(r.size), r.name)
	return nil
}

// copyDump takes the one file of a snapshot's tar and copies it into the
// server's container.
func (r *dbRun) copyDump(ctx context.Context, archive io.Reader, room func(size int64) error) error {
	file := r.db.GetFile()
	reader := tar.NewReader(archive)
	found := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if !header.FileInfo().Mode().IsRegular() {
			continue
		}
		if found {
			return &stageError{class: backup.TestIncomplete, message: "the snapshot holds more than one file: it is no database's dump"}
		}
		if path.Base(header.Name) != file {
			return &stageError{class: backup.TestIncomplete, message: fmt.Sprintf("the snapshot holds %s, not the dump %s", header.Name, file)}
		}
		found = true
		if room != nil {
			if err := room(header.Size); err != nil {
				return err
			}
		}
		if err := r.a.Docker.CopyFileFrom(ctx, r.container, databases.StagedDir, path.Base(r.staged), reader, header.Size, 0o600); err != nil {
			return err
		}
		r.size = header.Size
	}
	if !found {
		return &stageError{class: backup.TestIncomplete, message: fmt.Sprintf("the dump %s is not in the snapshot", file)}
	}
	return nil
}

// roomFor checks the room on the server once a dump's size is known.
func (r *dbRun) roomFor(ctx context.Context, databaseSize int64) func(int64) error {
	return func(size int64) error {
		free, err := r.free(ctx)
		if err != nil {
			return fmt.Errorf("the room on the server is unknown: %w", err)
		}
		needed := neededSpace(databaseSize, size)
		if free.Data < needed.Data || free.Tmp < needed.Tmp {
			return &stageError{class: backup.TestSpace, message: fmt.Sprintf("%s has %s free for its data and %s in /tmp, and the restore needs about %s and %s",
				r.name, sizeText(free.Data), sizeText(free.Tmp), sizeText(needed.Data), sizeText(needed.Tmp))}
		}
		return nil
	}
}

// countDump counts the staged dump's tables, and refuses a dump that is not
// whole.
func (r *dbRun) countDump(ctx context.Context) (int64, error) {
	out, err := r.run(ctx, databases.CountDump(r.engine, r.staged))
	if err == nil {
		var count int64
		if count, err = databases.ParseCount(out); err == nil {
			fmt.Fprintf(r.out, "The dump holds %d tables\n", count)
			return count, nil
		}
	}
	return 0, &stageError{class: backup.TestIncomplete, message: "the dump is not whole: " + err.Error()}
}

// fetch stages the dump and counts its tables.
func (r *dbRun) fetch(ctx context.Context, databaseSize int64) (int64, error) {
	if err := r.stage(ctx, r.roomFor(ctx, databaseSize)); err != nil {
		return 0, err
	}
	return r.countDump(ctx)
}

// restoreInto restores the staged dump into a database as the database's
// own user; definer, for MariaDB, makes the user the definer of what the
// dump defines.
func (r *dbRun) restoreInto(ctx context.Context, database, definer string) error {
	script, err := databases.RestoreAsUser(r.engine, r.db.GetUser(), r.db.GetPassword(), database, r.staged, definer)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Restoring the dump into %s as %s\n", database, r.db.GetUser())
	if _, err := r.run(ctx, script); err != nil {
		var client *clientError
		if errors.As(err, &client) && client.signInFailed() {
			return &stageError{class: backup.TestSignIn, message: fmt.Sprintf("%s did not sign in with the panel's password: %s", r.db.GetUser(), client.message)}
		}
		return &stageError{class: backup.TestRestore, message: fmt.Sprintf("the restore into %s failed: %s", database, err)}
	}
	return nil
}

// checkTables compares a restored database's tables with the dump's.
func (r *dbRun) checkTables(ctx context.Context, database string, expected int64) (int64, error) {
	tables, err := r.tables(ctx, database)
	if err != nil {
		return 0, err
	}
	if tables != expected {
		return tables, &stageError{class: backup.TestTables, message: fmt.Sprintf("%s has %d tables after the restore, and the dump %d", database, tables, expected)}
	}
	return tables, nil
}

// restoreDatabase previews a database restore, or runs it.
func (a *Agent) restoreDatabase(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetBackupRestore()
	run, err := a.dbRunOf(ctx, command, payload.GetDatabase(), payload.GetRepository(), payload.GetSnapshotId(), payload.GetJobTag(), reporter)
	if err != nil {
		return nil, err
	}
	if payload.GetDryRun() {
		return run.preview(ctx, payload)
	}
	return run.restore(ctx, payload)
}

// stacks are the stacks a restore in place stops: the database's owner's,
// and those whose settings name the database.
func (r *dbRun) stacks(payload *agentv1.BackupRestore) []string {
	return append(stackList(payload.GetStack()), r.db.GetStopProjects()...)
}

// wouldStop is what a restore in place stops; a restore into a new database
// stops nothing.
func (r *dbRun) wouldStop(ctx context.Context, payload *agentv1.BackupRestore, claim bool) ([]restoreUser, error) {
	if r.db.GetMode() != backup.DbRestoreReplace {
		return nil, nil
	}
	return r.a.affected(ctx, r.command.GetId(), r.stacks(payload), nil, r.db.GetStopContainers(), claim)
}

// preview says what a database restore would do: whether the snapshot holds
// the dump, and how large it is; what the database holds now, whether the
// user signs in, its sessions from outside what stops, its settings, the
// room on the server and what the restore needs, and what it stops.
func (r *dbRun) preview(ctx context.Context, payload *agentv1.BackupRestore) (map[string]any, error) {
	database, user, file := r.db.GetDatabase(), r.db.GetUser(), r.db.GetFile()
	users, err := r.wouldStop(ctx, payload, false)
	if err != nil {
		return nil, err
	}
	r.reporter.Progress("check", "Checking the snapshot, the server and the database", -1)
	out, err := r.a.backupJob(ctx, r.repo, backupJobSpec{args: []string{"ls", "--snapshot", r.snapshot, "--dir", "/", "--limit", "20"},
		labels: commandLabels(r.command)}, io.Discard)
	if err != nil {
		return nil, err
	}
	var listed lsResult
	if err := json.Unmarshal(out, &listed); err != nil || listed.Snapshot == nil {
		return nil, fmt.Errorf("the backup tool said: %s", out)
	}
	if !slices.Contains(listed.Snapshot.Tags, r.jobTag) {
		return nil, fmt.Errorf("the snapshot %.8s is not one of this job's", r.snapshot)
	}
	preview := backup.DbRestorePreview{
		DryRun:    true,
		Snapshot:  backup.PreviewSnapshot{ID: listed.Snapshot.ID, Time: listed.Snapshot.Time.UTC().Format(time.RFC3339), Paths: listed.Snapshot.Paths},
		WouldStop: []backup.WouldStop{}, TotalFiles: 1, Notes: []string{},
		Database: backup.DbPreview{Settings: []string{}},
	}
	inSnapshot := false
	for _, entry := range listed.Entries {
		if entry.Name == file && entry.Type == "file" {
			inSnapshot, preview.TotalBytes = true, entry.Size
		}
	}

	exists, err := r.sql(ctx, databases.ExistsSQL(r.engine, database, user))
	if err != nil {
		return nil, err
	}
	preview.Database.Exists, preview.Database.UserExists = databases.ParseExists(exists)
	addresses := map[string]bool{}
	for _, user := range users {
		preview.WouldStop = append(preview.WouldStop, backup.WouldStop{Name: user.name, Project: user.project, Service: user.service})
		if info, err := r.a.Docker.Inspect(ctx, user.id); err == nil {
			for _, network := range info.NetworkSettings.Networks {
				if address, _ := network["IPAddress"].(string); address != "" {
					addresses[address] = true
				}
			}
		}
	}
	if preview.Database.Exists {
		if r.db.GetMode() == backup.DbRestoreReplace {
			preview.Database.SizeBytes = r.databaseSize(ctx, database)
			if preview.Database.Tables, err = r.tables(ctx, database); err != nil {
				return nil, err
			}
		}
		sessions, err := r.sql(ctx, databases.SessionsSQL(r.engine, database))
		if err != nil {
			return nil, err
		}
		for _, address := range databases.ParseNames(sessions) {
			if !addresses[address] {
				preview.Database.ConnectionsOther++
			}
		}
		if r.engine.Name == databases.Postgres.Name {
			settings, err := r.sql(ctx, databases.SettingsSQL(r.engine, database))
			if err != nil {
				return nil, err
			}
			preview.Database.Settings = databases.ParseNames(settings)
		}
	}
	if r.db.GetMode() == backup.DbRestoreReplace && preview.Database.Exists {
		_, err := r.run(ctx, databases.SignIn(r.engine, user, r.db.GetPassword(), database))
		preview.Database.SignIn = err == nil
	}
	if r.engine.Name == databases.MariaDB.Name {
		sandbox, err := r.run(ctx, databases.Sandbox())
		preview.Database.Sandbox = err == nil && strings.TrimSpace(sandbox) == "yes"
	}
	if preview.Free, err = r.free(ctx); err != nil {
		return nil, err
	}
	preview.Needed = neededSpace(preview.Database.SizeBytes, preview.TotalBytes)
	preview.Roots = []backup.PreviewRoot{{Index: 0, Source: "/" + file, Destination: "database " + database, InSnapshot: inSnapshot,
		Exists: preview.Database.Exists, Entries: preview.Database.Tables}}
	if settings := preview.Database.Settings; len(settings) > 0 {
		preview.Notes = append(preview.Notes, fmt.Sprintf("The settings of %s are not in its dump, and the restored database does not have them: %s.", database, strings.Join(settings, ", ")))
	}
	if n := preview.Database.ConnectionsOther; n > 0 {
		preview.Notes = append(preview.Notes, fmt.Sprintf("%d sessions of %s come from outside what the restore stops: they end when it puts the backup in place.", n, database))
	}
	answer, err := backup.ResultMap(preview)
	if err != nil {
		return nil, err
	}
	trimList(answer, "notes", maxPreviewResult)
	return answer, nil
}

// partialError is a MariaDB restore in place that failed after its
// database was emptied.
type partialError struct{ err error }

func (e *partialError) Error() string { return e.err.Error() }

// restore runs a database restore: in place, or into a new database.
func (r *dbRun) restore(ctx context.Context, payload *agentv1.BackupRestore) (map[string]any, error) {
	// the steps before the application starts again end early enough to
	// start it and say so before the panel gives up on the command
	jobCtx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		reserve := min(restoreDeadlineReserve, time.Until(deadline)/2)
		var cancel context.CancelFunc
		jobCtx, cancel = context.WithDeadline(ctx, deadline.Add(-reserve))
		defer cancel()
	}
	database := r.db.GetDatabase()
	keys := []string{databaseClaim(r.container, database)}
	for _, copied := range []string{r.db.GetScratch(), r.db.GetPrevious()} {
		if copied != "" {
			keys = append(keys, databaseClaim(r.container, copied))
		}
	}
	release, err := r.a.claims.claim(true, keys)
	if err != nil {
		return nil, err
	}
	defer release()

	progress := &progressThrottle{reporter: r.reporter, every: restoreProgressForward}
	progress.report("check", "Checking the server, the snapshot and the database", 5, 0, 0, "")
	result := backup.DbRestoreResult{AppStopped: []backup.StoppedApp{}, AppStarted: []string{}, AppNotRunning: []string{},
		StartErrors: []string{}, AppLeftStopped: []string{}}
	session := &restoreSession{a: r.a, marker: restoreMarker{CommandID: r.command.GetId(), RestoreID: payload.GetRestoreId()}}
	r.sweep(ctx)
	defer r.unstage()

	var runErr error
	switch {
	case r.db.GetMode() == backup.DbRestoreNew:
		runErr = r.restoreNew(jobCtx, progress, &result)
	default:
		// what would stop now, against what the preview showed: refused
		// before anything is done, and again, live, before the stop
		users, err := r.wouldStop(ctx, payload, false)
		if err == nil {
			err = checkConfirmedStops(users, payload.GetConfirmedStops())
		}
		if err != nil {
			return nil, err
		}
		session.marker.Database = &databaseMarker{Container: r.container, Engine: r.engine.Name, Name: database,
			Scratch: r.db.GetScratch(), Previous: r.db.GetPrevious(), AdminUser: r.db.GetConnection().GetAdminUser()}
		if r.engine.Name == databases.Postgres.Name {
			runErr = r.replacePostgres(jobCtx, payload, session, progress, &result)
		} else {
			runErr = r.replaceMariaDB(jobCtx, payload, session, progress, &result)
		}
	}

	var partial *partialError
	switch {
	case errors.As(runErr, &partial):
		// the database holds part of the dump: what stopped stays stopped,
		// with its restart policies back
		left, problems := session.putBack()
		result.Partial, result.AppLeftStopped, result.StartErrors = true, left, problems
		runErr = emptiedError(database, left)
	case r.db.GetMode() == backup.DbRestoreReplace:
		if len(session.stopped) > 0 {
			progress.report("start", fmt.Sprintf("Starting %d containers again", len(session.stopped)), 90, 0, 0, "")
		}
		started := session.startAgain(len(session.stopped) > 0)
		if len(session.stopped) > 0 {
			progress.report("verify", "Checking that the containers run", 97, 0, 0, "")
		}
		result.AppStarted, result.AppNotRunning, result.StartErrors = started.started, started.notRunning, started.errors
		fmt.Fprintln(r.out, restoreEnd(started))
	}
	for _, container := range session.stopped {
		result.AppStopped = append(result.AppStopped, backup.StoppedApp{ID: container.ID, Name: container.Name})
	}
	if runErr == nil {
		result.FilesRestored, result.BytesRestored = 1, r.size
	}
	result.Log = r.log.String()
	answer, err := backup.ResultMap(result)
	if err != nil {
		return nil, err
	}
	trimRestoreResult(answer)
	return answer, runErr
}

// emptiedError is the end of a MariaDB restore in place that failed after
// its database was emptied.
func emptiedError(database string, left []string) error {
	if len(left) == 0 {
		return sentenceError(fmt.Sprintf("The restore failed after %s was emptied: it holds part of the backup. Restore again.", database))
	}
	return sentenceError(fmt.Sprintf("The restore failed after %s was emptied: it holds part of the backup, and %s stay stopped. Restore again, then start them on their pages.",
		database, strings.Join(left, ", ")))
}

// nothingChanged is the end of a restore that changed nothing in its
// database: a stop, or the end of its time, says so rather than what it
// cut short.
func nothingChanged(ctx context.Context, database string, err error) error {
	message := strings.TrimSuffix(err.Error(), ".")
	switch {
	case errors.Is(ctx.Err(), context.Canceled), errors.Is(err, context.Canceled):
		message = "the restore was stopped"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		message = "the restore ran out of time"
	}
	return sentenceError(fmt.Sprintf("Nothing changed in %s: %s.", database, message))
}

// writePhase notes the phase of a restore in place in its marker, before
// the step.
func writePhase(session *restoreSession, phase string) error {
	session.marker.Database.Phase = phase
	return session.writeMarker()
}

// stopApplication stops what uses the database, as it is now, which the
// preview must have shown.
func (r *dbRun) stopApplication(ctx context.Context, payload *agentv1.BackupRestore, session *restoreSession, progress *progressThrottle) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	users, err := r.wouldStop(ctx, payload, true)
	session.users = users
	if err == nil {
		err = checkConfirmedStops(users, payload.GetConfirmedStops())
	}
	if err == nil {
		err = writePhase(session, backup.PhaseStopped)
	}
	if err == nil && len(users) > 0 {
		progress.report("stop", fmt.Sprintf("Stopping %d containers", len(users)), 70, 0, 0, "")
		err = session.stop(ctx)
	}
	return err
}

// replacePostgres restores a PostgreSQL database in place: into a scratch
// copy while the application runs, which is put in place of the database in
// one transaction once the application stopped, keeping the data before it.
func (r *dbRun) replacePostgres(ctx context.Context, payload *agentv1.BackupRestore, session *restoreSession, progress *progressThrottle,
	result *backup.DbRestoreResult) error {
	database, scratch, previous := r.db.GetDatabase(), r.db.GetScratch(), r.db.GetPrevious()
	if err := writePhase(session, backup.PhaseScratch); err != nil {
		return err
	}
	// what was made goes again, whatever stopped the restore
	created := false
	undo := func(err error) error {
		if created {
			work, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreStartTimeout)
			defer cancel()
			if _, endErr := r.endSessions(work, scratch); endErr != nil {
				fmt.Fprintln(r.out, "The sessions of the copy did not end:", endErr)
			}
			if dropErr := r.discard(work, []string{scratch}, ""); dropErr != nil {
				fmt.Fprintln(r.out, "The copy could not be dropped, and the panel drops it later:", dropErr)
			}
		}
		return nothingChanged(ctx, database, err)
	}

	progress.report("fetch", "Copying the dump to the database server", 10, 0, 0, "")
	expected, err := r.fetch(ctx, r.databaseSize(ctx, database))
	if err != nil {
		return undo(err)
	}
	result.TablesExpected = expected
	create, err := databases.ScratchSQL(r.engine, scratch, r.db.GetUser(), databases.Charset{})
	if err != nil {
		return undo(err)
	}
	if _, err := r.sql(ctx, create); err != nil {
		return undo(err)
	}
	created = true
	progress.report("dbrestore", "Restoring the database into a copy", 30, 0, 0, "")
	if err := r.restoreInto(ctx, scratch, ""); err != nil {
		return undo(err)
	}
	if result.Tables, err = r.checkTables(ctx, scratch, expected); err != nil {
		return undo(err)
	}
	if err := r.stopApplication(ctx, payload, session, progress); err != nil {
		return undo(err)
	}

	// from here the restore finishes: the swap is one transaction, and a
	// stop now would leave the application stopped for nothing
	if ctx.Err() != nil {
		fmt.Fprintln(r.out, "The restore is putting the copy in place and finishes.")
	}
	work := context.WithoutCancel(ctx)
	// what fails from here says why, and not that the restore was stopped
	ctx = work
	progress.report("swap", "Putting the restored copy in place", 80, 0, 0, "")
	ended, err := r.endSessions(work, database)
	if err != nil {
		return undo(err)
	}
	result.ConnectionsEnded = ended
	swap, err := databases.SwapSQL(database, scratch, previous)
	if err != nil {
		return undo(err)
	}
	var tried time.Time
	for try := 1; ; try++ {
		// the swap's process runs on in the server's container whatever
		// happens to the agent: one that restarts from here asks the server
		// whether it took place
		tried = time.Now().UTC()
		session.marker.Database.SwapStarted = tried
		if err := writePhase(session, backup.PhaseSwapping); err != nil {
			return undo(err)
		}
		_, err = r.sql(work, swap)
		if err == nil || try == swapTries || !strings.Contains(err.Error(), "is being accessed by other users") {
			break
		}
		fmt.Fprintf(r.out, "A session came back to %s: trying again (%d of %d)\n", database, try+1, swapTries)
		time.Sleep(swapWait)
	}
	if err != nil {
		// the swap may have committed before what failed, such as its
		// last statement or the process's stream: the server says
		swapped, checkErr := r.swapTookPlace(work, tried, err)
		switch {
		case checkErr != nil:
			result.Previous = previous
			return swapUnknown(database, previous, fmt.Sprintf("putting the copy in place failed (%s)", strings.TrimSuffix(err.Error(), ".")), checkErr)
		case !swapped:
			return undo(err)
		}
		fmt.Fprintf(r.out, "Putting the copy in place failed after it took place: %v\n", err)
		admin, password := r.admin()
		if closeErr := r.a.closePrevious(work, r.container, admin, password, previous, r.secrets...); closeErr != nil {
			fmt.Fprintf(r.out, "%s could not be closed to connections: %v\n", previous, closeErr)
		}
	}
	result.Previous = previous
	if err := writePhase(session, backup.PhaseSwapped); err != nil {
		fmt.Fprintln(r.out, "The restore's phase could not be noted:", err)
	}
	fmt.Fprintf(r.out, "%s is the backup now; the data before it is %s\n", database, previous)
	return nil
}

// swapTookPlace says whether the swap whose process failed with err took
// place. A process the agent lost sight of, as when its stream broke, may
// still run in the server's container: the agent waits for its end first.
func (r *dbRun) swapTookPlace(ctx context.Context, tried time.Time, err error) (bool, error) {
	var client *clientError
	if !errors.As(err, &client) {
		swapSettled(ctx, tried)
	}
	user, password := r.admin()
	return r.a.swapTookPlace(ctx, r.container, user, password, r.db.GetScratch(), r.db.GetPrevious(), r.secrets...)
}

// swapTookPlace asks a PostgreSQL server whether a restore's swap took
// place. One transaction renames the database to previous and the copy to
// the database, so previous there and the copy's name gone say it did, and
// previous not there that it did not; anything else, or a server that does
// not answer, leaves it open, and the error says why.
func (a *Agent) swapTookPlace(ctx context.Context, container, admin, password, scratch, previous string, secrets ...string) (bool, error) {
	exists := func(name string) (bool, error) {
		var out strings.Builder
		statements := databases.Statements(databases.Postgres, admin, password, databases.ExistsSQL(databases.Postgres, name, admin))
		if err := a.runScript(ctx, container, statements, &out, secrets...); err != nil {
			return false, err
		}
		there, _ := databases.ParseExists(out.String())
		return there, nil
	}
	before, err := exists(previous)
	if err != nil || !before {
		return false, err
	}
	copied, err := exists(scratch)
	switch {
	case err != nil:
		return false, err
	case copied:
		return false, fmt.Errorf("both %s and %s are on the server", scratch, previous)
	}
	return true, nil
}

// closePrevious closes the data before a swap that took place to
// connections, as the swap's last statement would have.
func (a *Agent) closePrevious(ctx context.Context, container, admin, password, previous string, secrets ...string) error {
	sql, err := databases.ClosePreviousSQL(previous)
	if err != nil {
		return err
	}
	return a.runScript(ctx, container, databases.Statements(databases.Postgres, admin, password, sql), io.Discard, secrets...)
}

// swapUnknown is the end of a PostgreSQL restore in place whose swap failed
// and may have taken place all the same, which the server did not say: the
// data before it, if it is there, is kept, so it says neither that the
// restore went through nor that nothing changed.
func swapUnknown(database, previous, what string, err error) error {
	return sentenceError(fmt.Sprintf("The restore of %s may have gone through: %s, and the server did not say whether the copy took its place (%s). If it did, the data before it stays a day as %s.",
		database, what, strings.TrimSuffix(err.Error(), "."), previous))
}

// replaceMariaDB restores a MariaDB database in place: a rehearsal into a
// scratch copy while the application runs, dropped again, and then, once
// the application stopped, into the database, dropped and created again.
func (r *dbRun) replaceMariaDB(ctx context.Context, payload *agentv1.BackupRestore, session *restoreSession, progress *progressThrottle,
	result *backup.DbRestoreResult) error {
	database, scratch, user := r.db.GetDatabase(), r.db.GetScratch(), r.db.GetUser()
	out, err := r.sql(ctx, databases.CharsetSQL(database))
	if err != nil {
		return nothingChanged(ctx, database, err)
	}
	charset, err := databases.ParseCharset(out)
	if err != nil {
		return nothingChanged(ctx, database, err)
	}
	session.marker.Database.Charset, session.marker.Database.Collation = charset.Set, charset.Collation
	if err := writePhase(session, backup.PhaseRehearsal); err != nil {
		return err
	}
	created := false
	dropScratch := func() error {
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreStartTimeout)
		defer cancel()
		if _, err := r.endSessions(work, scratch); err != nil {
			fmt.Fprintln(r.out, "The sessions of the copy did not end:", err)
		}
		return r.discard(work, []string{scratch}, user)
	}
	undo := func(err error) error {
		if created {
			if dropErr := dropScratch(); dropErr != nil {
				fmt.Fprintln(r.out, "The copy could not be dropped, and the panel drops it later:", dropErr)
			}
		}
		return nothingChanged(ctx, database, err)
	}

	progress.report("fetch", "Copying the dump to the database server", 10, 0, 0, "")
	expected, err := r.fetch(ctx, r.databaseSize(ctx, database))
	if err != nil {
		return undo(err)
	}
	result.TablesExpected = expected
	create, err := databases.ScratchSQL(r.engine, scratch, user, charset)
	if err != nil {
		return undo(err)
	}
	rehearsed := rehearsalClock()
	if _, err := r.sql(ctx, create); err != nil {
		return undo(err)
	}
	created = true
	progress.report(backup.StepRehearse, "Rehearsing the restore in a copy", 30, 0, 0, "")
	if err := r.restoreInto(ctx, scratch, ""); err != nil {
		return undo(err)
	}
	if _, err := r.checkTables(ctx, scratch, expected); err != nil {
		return undo(err)
	}
	rehearsal := rehearsalClock().Sub(rehearsed)
	created = false
	if err := dropScratch(); err != nil {
		return nothingChanged(ctx, database, fmt.Errorf("the rehearsal's copy could not be dropped: %w", err))
	}
	// the restore after the drop runs to its end, past the command's
	// deadline if need be, while the panel gives up on the command at it
	// and lets the application start: it begins only when it fits, before
	// the stop and again after it
	if err := restoreAgainFits(ctx, rehearsal); err != nil {
		return nothingChanged(ctx, database, err)
	}
	if err := r.stopApplication(ctx, payload, session, progress); err != nil {
		return nothingChanged(ctx, database, err)
	}
	if err := restoreAgainFits(ctx, rehearsal); err != nil {
		return nothingChanged(ctx, database, err)
	}

	// from here the restore finishes, as the database is about to be
	// emptied
	work := context.WithoutCancel(ctx)
	ctx = work
	if err := writePhase(session, backup.PhaseEmptied); err != nil {
		return nothingChanged(ctx, database, err)
	}
	ended, err := r.endSessions(work, database)
	if err != nil {
		return nothingChanged(ctx, database, err)
	}
	result.ConnectionsEnded = ended
	recreate, err := databases.RecreateSQL(database, charset)
	if err != nil {
		return nothingChanged(ctx, database, err)
	}
	progress.report("dbrestore", "Restoring the database", 75, 0, 0, "")
	if _, err := r.sql(work, recreate); err != nil {
		return &partialError{err: err}
	}
	if err := r.restoreInto(work, database, ""); err != nil {
		return &partialError{err: err}
	}
	if result.Tables, err = r.checkTables(work, database, expected); err != nil {
		return &partialError{err: err}
	}
	if err := writePhase(session, backup.PhaseRestored); err != nil {
		fmt.Fprintln(r.out, "The restore's phase could not be noted:", err)
	}
	return nil
}

// restoreAgainFits says whether a MariaDB restore in place has the time,
// before its deadline, to restore the dump into its database once it is
// dropped: as long as the rehearsal took, a quarter more, and
// rehearsalMargin.
func restoreAgainFits(ctx context.Context, rehearsal time.Duration) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil
	}
	if deadline.Sub(rehearsalClock()) < rehearsal+rehearsal/4+rehearsalMargin {
		return errors.New("too little time is left to restore it again")
	}
	return nil
}

// restoreNew restores the dump into a new database with a new user, which
// it makes first, and drops again when the restore fails.
func (r *dbRun) restoreNew(ctx context.Context, progress *progressThrottle, result *backup.DbRestoreResult) error {
	database, user := r.db.GetDatabase(), r.db.GetUser()
	failed := func(err error) error {
		return sentenceError(fmt.Sprintf("The restore into the new database %s failed: %s.", database, strings.TrimSuffix(err.Error(), ".")))
	}
	out, err := r.sql(ctx, databases.ExistsSQL(r.engine, database, user))
	if err != nil {
		return failed(err)
	}
	if databaseThere, userThere := databases.ParseExists(out); databaseThere || userThere {
		return sentenceError(fmt.Sprintf("A database or user named %s exists on %s already: nothing was changed.", database, r.name))
	}
	progress.report("check", "Creating the database "+database, 5, 0, 0, "")
	if _, err := r.sql(ctx, databases.CreateSQL(r.engine, database, user, r.db.GetPassword())); err != nil {
		return failed(err)
	}
	drop := func(err error) error {
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreStartTimeout)
		defer cancel()
		if _, dropErr := r.sql(work, databases.DropManySQL(r.engine, []string{database}, []string{user})); dropErr != nil {
			fmt.Fprintln(r.out, "The new database could not be dropped again:", dropErr)
			return failed(err)
		}
		result.Dropped = true
		return sentenceError(fmt.Sprintf("The restore into the new database %s failed, and it was dropped again: %s.", database, strings.TrimSuffix(err.Error(), ".")))
	}
	progress.report("fetch", "Copying the dump to the database server", 10, 0, 0, "")
	expected, err := r.fetch(ctx, 0)
	if err != nil {
		return drop(err)
	}
	result.TablesExpected = expected
	definer := ""
	if r.engine.Name == databases.MariaDB.Name {
		// the dump's definers are users the new database does not know
		definer = user
	}
	progress.report("dbrestore", "Restoring the database", 40, 0, 0, "")
	if err := r.restoreInto(ctx, database, definer); err != nil {
		return drop(err)
	}
	if result.Tables, err = r.checkTables(ctx, database, expected); err != nil {
		return drop(err)
	}
	return nil
}

// finishInterruptedDatabaseRestore settles a database restore in place the
// agent restarted during, by its phase: before the swap or the drop
// nothing changed, and what stopped starts again; after the swap, or once
// MariaDB's database holds the whole dump, the restore went through, and
// what stopped starts again; a MariaDB database that was emptied holds part
// of the dump, and what stopped stays stopped with its restart policies
// back. During PostgreSQL's swap, whose process may have run on, the server
// says whether it took place; when it does not say, what stopped starts
// again, and the data before the restore is kept, as it may be there. The
// dump the restore staged goes first, whatever its phase.
func (a *Agent) finishInterruptedDatabaseRestore(ctx context.Context, session *restoreSession) {
	marker := session.marker
	database := marker.Database
	a.unstageInterrupted(ctx, marker)
	result := backup.DbRestoreResult{AppStopped: []backup.StoppedApp{}, AppStarted: []string{}, AppNotRunning: []string{},
		StartErrors: []string{}, AppLeftStopped: []string{}, Interrupted: true}
	for _, container := range marker.Containers {
		result.AppStopped = append(result.AppStopped, backup.StoppedApp{ID: container.ID, Name: container.Name})
	}
	phase, unknown := database.Phase, error(nil)
	if phase == backup.PhaseSwapping {
		phase, unknown = a.swapPhase(ctx, database)
	}
	state, message := stateFailed, ""
	startedAgain := func() string {
		started := session.startAgain(false)
		result.AppStarted, result.StartErrors = started.started, started.errors
		if len(marker.Containers) == 0 {
			return ""
		}
		return fmt.Sprintf(" It started the application again (%d of %d containers).", len(started.started), len(marker.Containers))
	}
	switch {
	case unknown != nil:
		again := startedAgain()
		result.Previous = database.Previous
		message = swapUnknown(database.Name, database.Previous, "the agent restarted while it put the copy in place", unknown).Error() + again
	case phase == backup.PhaseEmptied:
		left, problems := session.putBack()
		result.Partial, result.AppLeftStopped, result.StartErrors = true, left, problems
		message = "The agent restarted during the restore. " + emptiedError(database.Name, left).Error()
	case phase == backup.PhaseSwapped, phase == backup.PhaseRestored:
		startedAgain()
		result.FilesRestored, result.Previous = 1, database.Previous
		state = stateSucceeded
	default:
		message = fmt.Sprintf("Nothing changed in %s: the agent restarted during the restore.", database.Name) + startedAgain()
	}
	a.Log.Warn("a database restore was interrupted", "command", marker.CommandID, "phase", database.Phase, "found", phase, "state", state)
	answer, err := backup.ResultMap(result)
	if err != nil {
		answer = map[string]any{"interrupted": true}
	}
	if err := a.store.setState(marker.CommandID, state, message, answer); err != nil {
		a.Log.Error("failed to store a command state", "command", marker.CommandID, "error", err)
	}
}

// swapPhase is where an interrupted swap of PostgreSQL's copy is, as the
// server says once the swap's process can have ended: swapped, or stopped
// when it did not take place. The marker keeps no password: the
// administrator signs in on the server's local socket, which trusts it, as
// a dump does.
func (a *Agent) swapPhase(ctx context.Context, database *databaseMarker) (string, error) {
	swapSettled(ctx, database.SwapStarted)
	admin := database.AdminUser
	if admin == "" {
		admin = databases.Postgres.AdminUser
	}
	swapped, err := a.swapTookPlace(ctx, database.Container, admin, "", database.Scratch, database.Previous)
	switch {
	case err != nil:
		return backup.PhaseSwapping, err
	case !swapped:
		return backup.PhaseStopped, nil
	}
	if err := a.closePrevious(ctx, database.Container, admin, "", database.Previous); err != nil {
		a.Log.Warn("the data before a restore could not be closed to connections", "database", database.Previous, "error", err)
	}
	return backup.PhaseSwapped, nil
}

// checkRestoreTest checks what a restore test asks, before anything of the
// server is looked at.
func checkRestoreTest(command *agentv1.Command) error {
	payload := command.GetBackupRestoreTest()
	db := payload.GetDatabase()
	switch {
	case !snapshotID.MatchString(payload.GetSnapshotId()):
		return fmt.Errorf("a snapshot is named by its full id, not %q", payload.GetSnapshotId())
	case !jobTag.MatchString(payload.GetJobTag()):
		return fmt.Errorf("a restore test names its job's tag, job: and the job's id, not %q", payload.GetJobTag())
	case !recordID.MatchString(payload.GetCheckId()):
		return fmt.Errorf("a restore test names the panel's record of it, not %q", payload.GetCheckId())
	case !markerName.MatchString(command.GetId()):
		return fmt.Errorf("the command's id %q cannot name the dump it stages", command.GetId())
	case db.GetMode() != backup.DbRestoreTest:
		return fmt.Errorf("a restore test restores into a temporary database, mode %q, not %q", backup.DbRestoreTest, db.GetMode())
	case db.GetDatabase() != databases.TestDatabaseName(payload.GetCheckId()) || databases.ValidTemporaryName(db.GetDatabase()) != nil:
		return fmt.Errorf("a restore test restores into %s and its check's id, not %q", databases.TemporaryTest, db.GetDatabase())
	case db.GetScratch() != "" || db.GetPrevious() != "" || len(db.GetStopProjects()) > 0 || len(db.GetStopContainers()) > 0:
		return errors.New("a restore test makes no copies but its own, and stops nothing")
	case db.GetConnection().GetAdminPassword() == "" || db.GetPassword() == "":
		return errors.New("the command has no database password: the panel adds them when it delivers it")
	}
	if _, err := checkDatabaseNames(db); err != nil {
		return err
	}
	for _, name := range payload.GetDropBefore() {
		if databases.ValidTemporaryName(name) != nil || !strings.HasPrefix(name, databases.TemporaryTest) || name == db.GetDatabase() {
			return fmt.Errorf("a restore test drops earlier tests' databases only, not %q", name)
		}
	}
	return nil
}

// handleBackupRestoreTest restores a database job's dump into a temporary
// database pc_rt_<check id> as the database's own user, and compares its
// tables with the dump's (#53). The copy stays, closed, for the panel to
// drop. The answer (backup.RestoreTestResult) comes with the error of a
// failed test too, whose failure says its class.
func (a *Agent) handleBackupRestoreTest(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetBackupRestoreTest()
	if err := checkRepository(payload.GetRepository()); err != nil {
		return nil, err
	}
	if err := checkRestoreTest(command); err != nil {
		return nil, err
	}
	started := time.Now()
	result := backup.RestoreTestResult{Dropped: []string{}}
	var run *dbRun
	answer := func(err error) (map[string]any, error) {
		result.DurationMs = time.Since(started).Milliseconds()
		if run != nil {
			result.Log = run.log.String()
		}
		if err != nil {
			result.Status, result.Failure = "", failureClass(ctx, err)
		} else {
			result.Status = backup.RestoreTestOK
		}
		out, encodeErr := backup.ResultMap(result)
		if encodeErr != nil {
			return nil, encodeErr
		}
		trimRestoreResult(out)
		return out, err
	}
	db := payload.GetDatabase()
	temporary, user := db.GetDatabase(), db.GetUser()
	run, err := a.dbRunOf(ctx, command, db, payload.GetRepository(), payload.GetSnapshotId(), payload.GetJobTag(), reporter)
	if err != nil {
		return answer(err)
	}
	release, err := a.claims.claim(true, []string{databaseClaim(run.container, temporary)})
	if err != nil {
		return answer(err)
	}
	defer release()
	reporter.Progress("check", "Checking the server", 5)
	run.sweep(ctx)
	defer run.unstage()
	out, err := run.sql(ctx, databases.ExistsSQL(run.engine, temporary, user))
	if err != nil {
		return answer(err)
	}
	if there, _ := databases.ParseExists(out); there {
		return answer(fmt.Errorf("%s is on the server already: a restore test never reuses a database", temporary))
	}
	if earlier := payload.GetDropBefore(); len(earlier) > 0 {
		if err := run.discard(ctx, earlier, user); err != nil {
			fmt.Fprintln(run.out, "The databases of earlier tests could not be dropped, and the panel drops them later:", err)
		} else {
			result.Dropped = slices.Clone(earlier)
		}
	}
	charset := databases.DefaultCharset
	if run.engine.Name == databases.MariaDB.Name {
		// the copy takes the character set of the database the dump is of
		source, _ := databases.FileDatabase(run.engine, db.GetFile())
		if out, err := run.sql(ctx, databases.CharsetSQL(source)); err == nil {
			if read, err := databases.ParseCharset(out); err == nil {
				charset = read
			}
		}
	}
	create, closing, err := databases.TestDatabaseSQL(run.engine, temporary, user, charset)
	if err != nil {
		return answer(err)
	}
	reporter.Progress("create", "Creating the temporary database "+temporary, 10)
	if _, err := run.sql(ctx, create); err != nil {
		return answer(err)
	}
	result.Created = true
	if _, err := run.run(ctx, databases.SignIn(run.engine, user, db.GetPassword(), temporary)); err != nil {
		return answer(&stageError{class: backup.TestSignIn, message: fmt.Sprintf("%s did not sign in with the panel's password: %s", user, err)})
	}
	reporter.Progress("fetch", "Copying the dump to the database server", 20)
	expected, err := run.fetch(ctx, 0)
	result.TablesExpected, result.Bytes = expected, run.size
	if err != nil {
		return answer(err)
	}
	reporter.Progress("dbrestore", "Restoring the dump into "+temporary, 40)
	if err := run.restoreInto(ctx, temporary, ""); err != nil {
		return answer(err)
	}
	tables, err := run.checkTables(ctx, temporary, expected)
	result.Tables = tables
	if err != nil {
		return answer(err)
	}
	if _, err := run.sql(ctx, closing); err != nil {
		fmt.Fprintln(run.out, "The temporary database could not be closed:", err)
	}
	fmt.Fprintf(run.out, "Restored %d tables of %s, as many as the dump holds\n", tables, strconv.Quote(db.GetFile()))
	return answer(nil)
}
