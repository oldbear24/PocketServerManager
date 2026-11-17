package panel

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/list"

	"github.com/oldbear24/PocketServerManager/internal/gateway"
	"github.com/oldbear24/PocketServerManager/internal/panelbackup"
	"github.com/oldbear24/PocketServerManager/internal/pki"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/version"
)

// The panel's copy of itself (#32). A run of the panel's backup job asks for
// it as the artifact panel-export:<run> (see fetchArtifact): the panel
// copies its database with VACUUM INTO, one read transaction, so that what
// it holds agrees with itself, the audit log with its sealed head included;
// lists the stored files the copy names; and writes them encrypted
// (internal/panelbackup, PCPB1) into its spool, with keys derived from the
// master key. The plain copy of the database is removed at once. Only the
// run's carrier gets the copy, and the runs of one minute, one per target,
// share it: a copy serves for 15 minutes, and a run that fetched one gets
// the same again, as when its download resumes. The spool is swept at start
// and hourly.

const (
	// exportSpool is the spool of the copies, in the data dir: not
	// PocketBase's temporary dir, which every command line of the panel
	// empties, and left out of PocketBase's own backups.
	exportSpool = ".pc_panel_exports"
	// exportReuse is how long a copy serves the runs that ask for it.
	exportReuse = 15 * time.Minute
)

// availableSpace says how much room is left in a directory; tests replace
// it.
var availableSpace = freeSpace

// panelExport is an encrypted copy of the panel in the spool.
type panelExport struct {
	path   string
	size   int64
	sha256 string
	built  time.Time
	// runs are the runs that fetched it
	runs map[string]bool
}

// panelExports are the copies of the panel's job, newest first, and the
// copies being written, by the start of their files' names.
type panelExports struct {
	mu       sync.Mutex
	jobs     map[string][]*panelExport
	building map[string]bool
	// builds holds a build per job: the runs that ask meanwhile wait for it
	builds keyedLocks
}

// fetchPanelExport opens the copy of the panel that a run of its backup job
// fetches. Only the run's carrier gets it, while the run waits for it: the
// run is queued or running and is the caller's, its job backs up the panel
// and is carried by the caller, and its command is the caller's and was
// delivered. Anything else is gateway.ErrNoArtifact. An error of the build
// is the gateway's Internal, with its sentence, and the agent tries again.
func (p *Panel) fetchPanelExport(hostID, runID string) (*gateway.Artifact, error) {
	run, err := p.App.FindRecordById(schema.BackupRuns, runID)
	if err != nil || run.GetString("host") != hostID ||
		(run.GetString("status") != schema.RunQueued && run.GetString("status") != schema.RunRunning) {
		return nil, gateway.ErrNoArtifact
	}
	job, err := p.App.FindRecordById(schema.BackupJobs, run.GetString("job"))
	if err != nil || job.GetString("source_kind") != schema.SourcePanel || job.GetString("host") != hostID {
		return nil, gateway.ErrNoArtifact
	}
	command, err := p.App.FindRecordById(schema.Commands, run.GetString("command"))
	if err != nil || command.GetString("host") != hostID ||
		(command.GetString("status") != schema.CommandDispatched && command.GetString("status") != schema.CommandRunning) {
		return nil, gateway.ErrNoArtifact
	}
	file, export, err := p.panelExportOf(job, run)
	if err != nil {
		return nil, err
	}
	return &gateway.Artifact{Reader: file, Size: uint64(export.size), SHA256: export.sha256}, nil //nolint:gosec // a size
}

// panelExportOf opens the copy a run of a job gets: one it fetched before,
// the newest while it is young, or a new one.
func (p *Panel) panelExportOf(job, run *core.Record) (*os.File, *panelExport, error) {
	build := p.exports.builds.of(job.Id)
	build.Lock()
	defer build.Unlock()
	if file, export := p.exports.reuse(job.Id, run.Id, time.Now()); file != nil {
		return file, export, nil
	}
	base := job.Id + "-" + core.GenerateDefaultRandomId()
	p.exports.mark(base, true)
	defer p.exports.mark(base, false)
	export, err := p.buildPanelExport(job, run, base)
	if err != nil {
		p.Log.Warn("the panel could not copy itself for its backup", "job", job.Id, "run", run.Id, "error", err)
		return nil, nil, err
	}
	export.runs = map[string]bool{run.Id: true}
	going, err := p.goingPanelRuns()
	if err != nil {
		// nothing older is removed while it is not known who fetches it
		going = nil
	}
	file, err := p.exports.keep(job.Id, export, going, err == nil)
	if err != nil {
		return nil, nil, err
	}
	return file, export, nil
}

// mark says that a copy whose files' names start with base is being
// written, or no longer is.
func (x *panelExports) mark(base string, building bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.building == nil {
		x.building = map[string]bool{}
	}
	if building {
		x.building[base] = true
	} else {
		delete(x.building, base)
	}
}

// reuse opens a copy of a job's that a run may have: one it fetched before,
// or the newest while it is young.
func (x *panelExports) reuse(jobID, runID string, now time.Time) (*os.File, *panelExport) {
	x.mu.Lock()
	defer x.mu.Unlock()
	copies := x.jobs[jobID]
	var pick *panelExport
	for _, export := range copies {
		if export.runs[runID] {
			pick = export
			break
		}
	}
	if pick == nil && len(copies) > 0 && now.Sub(copies[0].built) < exportReuse {
		pick = copies[0]
	}
	if pick == nil {
		return nil, nil
	}
	file, err := os.Open(pick.path)
	if err != nil {
		// gone from the spool: a new one is made
		x.jobs[jobID] = slices.DeleteFunc(copies, func(export *panelExport) bool { return export == pick })
		return nil, nil
	}
	pick.runs[runID] = true
	return file, pick
}

// keep opens a job's new copy and keeps it as its newest. Its older copies
// go, but those a run that is still going fetched, while that is known.
func (x *panelExports) keep(jobID string, export *panelExport, going map[string]bool, known bool) (*os.File, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	file, err := os.Open(export.path)
	if err != nil {
		_ = os.Remove(export.path)
		return nil, err
	}
	kept := []*panelExport{export}
	for _, old := range x.jobs[jobID] {
		if !known || fetchedByAny(old, going) {
			kept = append(kept, old)
			continue
		}
		_ = os.Remove(old.path)
	}
	if x.jobs == nil {
		x.jobs = map[string][]*panelExport{}
	}
	x.jobs[jobID] = kept
	return file, nil
}

// fetchedByAny tells whether one of the runs fetched a copy.
func fetchedByAny(export *panelExport, runs map[string]bool) bool {
	for run := range export.runs {
		if runs[run] {
			return true
		}
	}
	return false
}

// goingPanelRuns are the runs of the panel's job that are queued or
// running: they may still fetch the copies they fetched.
func (p *Panel) goingPanelRuns() (map[string]bool, error) {
	runs, err := p.App.FindRecordsByFilter(schema.BackupRuns, "job.source_kind={:panel} && (status={:queued} || status={:running})", "", 0, 0,
		dbx.Params{"panel": schema.SourcePanel, "queued": schema.RunQueued, "running": schema.RunRunning})
	if err != nil {
		return nil, err
	}
	going := make(map[string]bool, len(runs))
	for _, run := range runs {
		going[run.Id] = true
	}
	return going, nil
}

// buildPanelExport writes a new copy of the panel for a run of its job into
// the spool, as base.pcpb. Its plain copy of the database, base.db, is gone
// when it returns, as is all of it on an error.
func (p *Panel) buildPanelExport(job, run *core.Record, base string) (_ *panelExport, err error) {
	if len(p.masterKey) == 0 {
		return nil, errors.New("the panel has not loaded its master key")
	}
	// PocketBase's own backups and restores copy the same database: one at
	// a time, and neither starts while this one runs
	mine, claimed := "pocketcloud-panel-export-"+run.Id, false
	p.App.Store().GetOrSet(core.StoreKeyActiveBackup, func() any {
		claimed = true
		return mine
	})
	if !claimed {
		return nil, errors.New("PocketBase is making or restoring a backup: the run is tried again.") //nolint:staticcheck // the run's sentence
	}
	defer p.App.Store().Remove(core.StoreKeyActiveBackup)

	spool := filepath.Join(p.App.DataDir(), exportSpool)
	if err := os.MkdirAll(spool, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(spool, 0o700); err != nil { //nolint:gosec // a directory: the panel alone enters it
		return nil, err
	}
	plain := filepath.Join(spool, base+".db")
	part := filepath.Join(spool, base+".pcpb.part")
	final := filepath.Join(spool, base+".pcpb")
	defer func() {
		// the plain copy never stays, nor a copy that failed
		for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
			_ = os.Remove(plain + suffix)
		}
		if err != nil {
			_ = os.Remove(part)
		}
	}()

	if err := p.roomForExport(spool); err != nil {
		return nil, err
	}

	// one read transaction: the copy agrees with itself
	if _, err := p.App.ConcurrentDB().NewQuery("VACUUM INTO {:path}").Bind(dbx.Params{"path": plain}).Execute(); err != nil {
		return nil, fmt.Errorf("the panel's database could not be copied: %w", err)
	}
	// as PocketBase's own backup does: what the copy read does not stay in
	// the write-ahead log
	_, _ = p.App.NonconcurrentDB().NewQuery("PRAGMA wal_checkpoint(TRUNCATE)").Execute()
	if err := os.Chmod(plain, 0o600); err != nil {
		return nil, err
	}

	facts, err := readExportCopy(plain)
	if err != nil {
		return nil, err
	}
	defer facts.close()
	if keyID := secrets.KeyID(p.masterKey); facts.keyID != "" && facts.keyID != keyID {
		return nil, fmt.Errorf("the panel's database names the master key %s, not the one the panel runs with (%s)", facts.keyID, keyID)
	}
	storage := panelbackup.StorageLocal
	var files []storedFile
	var missing []string
	if p.App.Settings().S3.Enabled {
		storage = panelbackup.StorageS3
	} else {
		files, missing, err = storedFilesOf(facts.db, p.App.DataDir())
		if err != nil {
			return nil, err
		}
	}

	size, written, err := writeExport(final, part, plain, p.masterKey, panelbackup.Manifest{
		PanelVersion: version.Version,
		Migration:    facts.migration,
		CAPin:        facts.caPin,
		Audit:        facts.audit,
		AgentsDial:   facts.dials,
		Hosts:        facts.hosts,
		Job:          job.Id,
		Run:          run.Id,
		Storage:      storage,
		Missing:      missing,
		LeftOut:      panelbackup.LeftOut,
	}, files)
	if err != nil {
		return nil, err
	}
	p.Log.Info("the panel copied itself for its backup", "job", job.Id, "run", run.Id, "files", len(files), "missing", len(missing), "bytes", size)
	return &panelExport{path: final, size: size, sha256: written, built: time.Now()}, nil
}

// roomForExport refuses a copy the spool has no room for: the plain copy of
// the database and its write-ahead log, the encrypted one, and the stored
// files, and a tenth more.
func (p *Panel) roomForExport(spool string) error {
	need := int64(0)
	for _, name := range []string{"data.db", "data.db-wal"} {
		if info, err := os.Stat(filepath.Join(p.App.DataDir(), name)); err == nil {
			need += 2 * info.Size()
		}
	}
	if !p.App.Settings().S3.Enabled {
		files, _, err := storedFilesOf(p.App.ConcurrentDB(), p.App.DataDir())
		if err != nil {
			return err
		}
		for _, file := range files {
			need += file.size
		}
	}
	need += need / 10
	free, err := availableSpace(spool)
	if err != nil {
		return err
	}
	if free < uint64(need) { //nolint:gosec // a size
		return fmt.Errorf("The panel has no room to prepare its backup: %s free, about %s needed.", bytesText(free), bytesText(uint64(need))) //nolint:staticcheck,gosec // the run's sentence; a size
	}
	return nil
}

// exportFacts is what the copy of the database says of the panel.
type exportFacts struct {
	db        *dbx.DB
	migration string
	audit     panelbackup.AuditHead
	keyID     string
	caPin     string
	hosts     int
	dials     []panelbackup.Dial
}

func (f exportFacts) close() {
	if f.db != nil {
		_ = f.db.Close()
	}
}

// readExportCopy opens the plain copy of the database, read-only, checks it,
// and reads what the manifest says of the panel. It is open until close,
// for the stored files it names.
func readExportCopy(path string) (facts exportFacts, err error) {
	db, err := dbx.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&immutable=1")
	if err != nil {
		return exportFacts{}, err
	}
	defer func() {
		if err != nil {
			_ = db.Close()
		}
	}()
	facts.db = db
	var check []string
	if err := db.NewQuery("PRAGMA quick_check").Column(&check); err != nil {
		return exportFacts{}, fmt.Errorf("the copy of the panel's database is damaged: %s", firstLine(err.Error()))
	}
	if len(check) == 0 || check[0] != "ok" {
		first := ""
		if len(check) > 0 {
			first = check[0]
		}
		return exportFacts{}, fmt.Errorf("the copy of the panel's database is damaged: %s", firstLine(first))
	}
	if err := db.NewQuery("SELECT [[file]] FROM {{_migrations}} ORDER BY [[applied]] DESC, [[file]] DESC LIMIT 1").Row(&facts.migration); err != nil {
		return exportFacts{}, fmt.Errorf("the copy's migrations: %w", err)
	}
	var heads int
	if err := db.NewQuery("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = '_pc_audit_head'").Row(&heads); err != nil {
		return exportFacts{}, err
	}
	if heads > 0 {
		err := db.NewQuery("SELECT [[seq]], [[hash]] FROM {{_pc_audit_head}} WHERE [[id]] = 1").Row(&facts.audit.Seq, &facts.audit.Hash)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return exportFacts{}, fmt.Errorf("the copy's audit head: %w", err)
		}
	}
	if err := db.NewQuery("SELECT [[key_id]] FROM {{" + schema.PanelTable + "}} WHERE [[id]] = 1").Row(&facts.keyID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return exportFacts{}, fmt.Errorf("the copy's key id: %w", err)
	}
	var rootCert string
	err = db.NewQuery("SELECT [[root_cert]] FROM {{" + schema.InternalCA + "}} ORDER BY [[created]] DESC LIMIT 1").Row(&rootCert)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return exportFacts{}, fmt.Errorf("the copy's CA: %w", err)
	default:
		if block, _ := pem.Decode([]byte(rootCert)); block != nil {
			facts.caPin = pki.PinOf(block.Bytes)
		}
	}
	if err := db.NewQuery("SELECT COUNT(*) FROM {{" + schema.Hosts + "}}").Row(&facts.hosts); err != nil {
		return exportFacts{}, err
	}
	facts.dials = []panelbackup.Dial{}
	err = db.NewQuery("SELECT [[panel_address]] AS [[address]], COUNT(*) AS [[servers]] FROM {{" + schema.Hosts + "}} WHERE [[panel_address]] != '' GROUP BY [[panel_address]] ORDER BY [[panel_address]]").
		All(&facts.dials)
	if err != nil {
		return exportFacts{}, err
	}
	return facts, nil
}

// firstLine is the first line of a text.
func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return line
}

// storedFile is a stored file a copy holds: its name in the copy,
// storage/<collection id>/<record id>/<file>, where it is, and its size.
type storedFile struct {
	name string
	path string
	size int64
}

// storedFilesOf lists the stored files a database names, in every file field
// of every record, in byte order of their names, and those that are gone
// from the data dir. Diagnostic bundles are left out: they are made again,
// hold what a server said, and go after a week. PocketBase's thumbnails are
// no field's.
func storedFilesOf(db dbx.Builder, dataDir string) ([]storedFile, []string, error) {
	collections := []struct {
		ID     string `db:"id"`
		Name   string `db:"name"`
		Type   string `db:"type"`
		Fields string `db:"fields"`
	}{}
	if err := db.NewQuery("SELECT [[id]], [[name]], [[type]], [[fields]] FROM {{_collections}} ORDER BY [[id]]").All(&collections); err != nil {
		return nil, nil, err
	}
	files, missing := []storedFile{}, []string{}
	seen := map[string]bool{}
	for _, collection := range collections {
		if collection.Type == core.CollectionTypeView || collection.Name == schema.DiagnosticBundles {
			continue
		}
		var fields core.FieldsList
		if err := json.Unmarshal([]byte(collection.Fields), &fields); err != nil {
			return nil, nil, fmt.Errorf("the fields of %s: %w", collection.Name, err)
		}
		for _, field := range fields {
			if _, ok := field.(*core.FileField); !ok {
				continue
			}
			rows := []struct {
				ID    string `db:"id"`
				Value string `db:"value"`
			}{}
			err := db.NewQuery("SELECT [[id]], [[" + field.GetName() + "]] AS [[value]] FROM {{" + collection.Name + "}} WHERE [[" + field.GetName() + "]] != ''").All(&rows)
			if err != nil {
				return nil, nil, fmt.Errorf("the files of %s: %w", collection.Name, err)
			}
			for _, row := range rows {
				for _, file := range list.ToUniqueStringSlice(row.Value) {
					name := "storage/" + collection.ID + "/" + row.ID + "/" + file
					if !panelbackup.ValidFileName(name) {
						return nil, nil, fmt.Errorf("the stored file %q has a name a panel backup cannot hold", name)
					}
					if seen[name] {
						continue
					}
					seen[name] = true
					path := filepath.Join(dataDir, core.LocalStorageDirName, collection.ID, row.ID, file)
					info, err := os.Stat(path)
					switch {
					case errors.Is(err, os.ErrNotExist):
						missing = append(missing, name)
					case err != nil:
						return nil, nil, err
					case !info.Mode().IsRegular():
						return nil, nil, fmt.Errorf("the stored file %s is no file", name)
					default:
						files = append(files, storedFile{name: name, path: path, size: info.Size()})
					}
				}
			}
		}
	}
	slices.SortFunc(files, func(a, b storedFile) int { return strings.Compare(a.name, b.name) })
	slices.Sort(missing)
	return files, missing, nil
}

// writeExport writes the encrypted copy of the plain database and the
// stored files into part, syncs it and renames it final. It returns its
// size and its SHA-256.
func writeExport(final, part, plain string, master []byte, manifest panelbackup.Manifest, files []storedFile) (int64, string, error) {
	database, err := os.Open(plain)
	if err != nil {
		return 0, "", err
	}
	defer database.Close()
	info, err := database.Stat()
	if err != nil {
		return 0, "", err
	}
	manifest.Files, manifest.Bytes = len(files), info.Size()
	for _, file := range files {
		manifest.Bytes += file.size
	}

	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, "", err
	}
	defer out.Close()
	digest, counted := sha256.New(), &byteCounter{}
	buffered := bufio.NewWriterSize(io.MultiWriter(out, digest, counted), 1<<20)
	w, err := panelbackup.NewWriter(buffered, master, time.Now())
	if err != nil {
		return 0, "", err
	}
	if err := w.Manifest(manifest); err != nil {
		return 0, "", err
	}
	if err := w.Database(database, info.Size()); err != nil {
		return 0, "", err
	}
	for _, file := range files {
		if err := writeStoredFile(w, file); err != nil {
			return 0, "", err
		}
	}
	if err := w.Close(); err != nil {
		return 0, "", err
	}
	if err := buffered.Flush(); err != nil {
		return 0, "", err
	}
	if err := out.Sync(); err != nil {
		return 0, "", err
	}
	if err := out.Close(); err != nil {
		return 0, "", err
	}
	if err := os.Rename(part, final); err != nil {
		return 0, "", err
	}
	return counted.n, hex.EncodeToString(digest.Sum(nil)), nil
}

// storedSource is a stored file as a copy reads it.
type storedSource interface {
	io.ReaderAt
	io.Closer
}

// openStored opens a stored file for a copy; tests replace it.
var openStored = func(path string) (storedSource, error) { return os.Open(path) }

// writeStoredFile writes one stored file into a copy.
func writeStoredFile(w *panelbackup.Writer, file storedFile) error {
	src, err := openStored(file.path)
	if err != nil {
		return fmt.Errorf("the stored file %s: %w", file.name, err)
	}
	defer src.Close()
	return w.File(file.name, src, file.size)
}

// byteCounter counts what is written to it.
type byteCounter struct{ n int64 }

func (c *byteCounter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// sweepPanelExports removes from the spool what no run fetches any more: a
// copy older than 15 minutes that no run that is still going fetched, and
// what a build left that is not one under way, as the plain copy of a build
// that a stopped panel did not finish.
func (p *Panel) sweepPanelExports(now time.Time) {
	spool := filepath.Join(p.App.DataDir(), exportSpool)
	entries, err := os.ReadDir(spool)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			p.Log.Error("failed to read the spool of the panel's copies", "error", err)
		}
		return
	}
	going, err := p.goingPanelRuns()
	if err != nil {
		p.Log.Error("failed to read the runs of the panel's backup", "error", err)
		return
	}
	x := &p.exports
	x.mu.Lock()
	defer x.mu.Unlock()
	kept := map[string]bool{}
	for job, copies := range x.jobs {
		x.jobs[job] = slices.DeleteFunc(copies, func(export *panelExport) bool {
			if now.Sub(export.built) < exportReuse || fetchedByAny(export, going) {
				kept[export.path] = true
				return false
			}
			return true
		})
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(spool, name)
		base, _, _ := strings.Cut(name, ".")
		if !entry.Type().IsRegular() || kept[path] || x.building[base] {
			continue
		}
		if strings.HasSuffix(name, ".pcpb") {
			// a copy of a panel before this one started
			info, err := entry.Info()
			if err != nil || now.Sub(info.ModTime()) < exportReuse {
				continue
			}
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			p.Log.Error("failed to remove a copy of the panel", "file", name, "error", err)
		}
	}
}

// registerPanelExports sweeps the spool as the panel starts, before its
// gateway serves a copy, and hourly.
func (p *Panel) registerPanelExports() {
	p.App.OnServe().BindFunc(func(e *core.ServeEvent) error {
		p.sweepPanelExports(time.Now())
		return e.Next()
	})
	p.App.Cron().MustAdd("pocketcloud_panel_exports", "13 * * * *", func() { p.sweepPanelExports(time.Now()) })
}

// bytesText says a number of bytes as people read it.
func bytesText(n uint64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatUint(n, 10) + " B"
	}
	value, prefix := float64(n)/unit, 0
	for value >= unit && prefix < 4 {
		value /= unit
		prefix++
	}
	return strconv.FormatFloat(value, 'f', 1, 64) + " " + string("KMGTP"[prefix]) + "iB"
}
