package panel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"

	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/gateway"
	"github.com/oldbear24/PocketServerManager/internal/panelbackup"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// exportFixture is a panel whose backup job is carried by a connected
// server.
type exportFixture struct {
	*jobFixture
	job    *core.Record
	stream *commands.Stream
	// quiet is a server whose agent is not connected
	quiet *core.Record
}

// idle is a server whose agent is not connected.
func (f *exportFixture) idle() *core.Record {
	f.t.Helper()
	if f.quiet == nil {
		f.quiet = testutil.NewHost(f.t, f.app, "node-idle")
	}
	return f.quiet
}

func newExportFixture(t *testing.T) *exportFixture {
	t.Helper()
	f := newJobFixture(t)
	f.carry(f.host, "backup.run", backup.CapabilityPanel)
	return &exportFixture{jobFixture: f, job: f.job("panel (pb_data)", map[string]any{"source_kind": schema.SourcePanel}), stream: f.agent(f.host)}
}

// deliveredRun starts a run of the panel's job to a target and delivers its
// command to the carrier, which may then fetch the run's copy.
func (f *exportFixture) deliveredRun(target *core.Record) *core.Record {
	f.t.Helper()
	run, err := f.p.startRun(context.Background(), f.job, target, f.host, schema.RunManual, time.Time{}, commands.Options{})
	if err != nil {
		f.t.Fatal(err)
	}
	run = f.fresh(run)
	if run.GetString("command") == "" {
		f.t.Fatalf("a run without its command: %v", run.FieldsData())
	}
	if got := f.received(f.stream); got.GetId() != run.GetString("command") {
		f.t.Fatalf("the carrier got %s, not %s", got.GetId(), run.GetString("command"))
	}
	f.eventually("the run's command delivered", func() bool {
		command, err := f.app.FindRecordById(schema.Commands, run.GetString("command"))
		return err == nil && command.GetString("status") == schema.CommandDispatched
	})
	return run
}

// fetch fetches a run's copy as its carrier would, whole, and checks it
// against its size and digest.
func (f *exportFixture) fetch(run *core.Record) []byte {
	f.t.Helper()
	artifact, err := f.p.fetchArtifact(f.host.Id, backup.PanelExportID(run.Id))
	if err != nil {
		f.t.Fatalf("fetch: %v", err)
	}
	defer artifact.Reader.Close()
	data, err := io.ReadAll(artifact.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if uint64(len(data)) != artifact.Size || hex.EncodeToString(sum[:]) != artifact.SHA256 {
		f.t.Fatalf("%d bytes, sha256 %x: the artifact said %d, %s", len(data), sum, artifact.Size, artifact.SHA256)
	}
	return data
}

// spool lists the files of the spool.
func (f *exportFixture) spool() []string {
	f.t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.app.DataDir(), exportSpool))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// storedFile saves a file in a record's file field, as PocketBase does with
// an upload, and is its name in a copy and its path.
func (f *exportFixture) storedFile(record *core.Record, field string, content []byte) (string, string) {
	f.t.Helper()
	file, err := filesystem.NewFileFromBytes(content, "file.txt")
	if err != nil {
		f.t.Fatal(err)
	}
	record.Set(field, file)
	if err := f.app.Save(record); err != nil {
		f.t.Fatal(err)
	}
	name := record.GetString(field)
	return "storage/" + record.Collection().Id + "/" + record.Id + "/" + name,
		filepath.Join(f.app.DataDir(), "storage", record.Collection().Id, record.Id, name)
}

// commandWithOutput is a command, with an output file, of a server that is
// not connected: it stays queued.
func (f *exportFixture) commandWithOutput(host *core.Record, content []byte) (*core.Record, string, string) {
	f.t.Helper()
	if f.p.Commands.Connected(host.Id) {
		f.t.Fatalf("%s is connected", host.GetString("name"))
	}
	collection, err := f.app.FindCollectionByNameOrId(schema.Commands)
	if err != nil {
		f.t.Fatal(err)
	}
	command := core.NewRecord(collection)
	command.Load(map[string]any{"host": host.Id, "type": "image.pull", "payload": map[string]any{"image": "busybox"}})
	name, path := f.storedFile(command, "output", content)
	return command, name, path
}

// extractExport opens a copy with the master key into a new data dir.
func extractExport(t *testing.T, data []byte) (string, panelbackup.Summary) {
	t.Helper()
	dir := t.TempDir()
	summary, err := panelbackup.Extract(bytes.NewReader(data), testutil.MasterKey, dir)
	if err != nil {
		t.Fatalf("the copy does not open: %v", err)
	}
	return dir, summary
}

// extractedApp is a PocketBase app on an extracted copy.
func extractedApp(t *testing.T, dir string) core.App {
	t.Helper()
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: dir, EncryptionEnv: "pb_test_env"})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.ClearBootstrap() })
	return app
}

// TestTheExportIsConsistentWhileThePanelWrites (#32): the copy is one read of
// the database while the panel writes: its audit log verifies with its
// sealed head, which the manifest names, and the manifest says what the
// panel was: its key, its CA, its servers and the addresses they dial, the
// newest migration, the job and the run.
func TestTheExportIsConsistentWhileThePanelWrites(t *testing.T) {
	f := newExportFixture(t)
	for i, address := range []string{"192.0.2.2:9443", "192.0.2.2:9443", "panel.example.com:9443", ""} {
		host := testutil.NewHost(t, f.app, fmt.Sprintf("dial-%d", i))
		host.Set("panel_address", address)
		if err := f.app.Save(host); err != nil {
			t.Fatal(err)
		}
	}
	run := f.deliveredRun(f.target)

	stop, written := make(chan struct{}), atomic.Int64{}
	var writers sync.WaitGroup
	var failed atomic.Value
	for w := range 3 {
		writers.Go(func() {
			collection, err := f.app.FindCollectionByNameOrId(schema.Stacks)
			if err != nil {
				failed.Store(err)
				return
			}
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				stack := core.NewRecord(collection)
				stack.Load(map[string]any{"host": f.host.Id, "name": fmt.Sprintf("w%d-%d", w, i)})
				if err := f.app.Save(stack); err != nil {
					failed.Store(err)
					return
				}
				written.Add(1)
			}
		})
	}
	// the copy is made while they write
	for written.Load() < 20 {
		time.Sleep(5 * time.Millisecond)
	}
	data := f.fetch(run)
	close(stop)
	writers.Wait()
	if err, _ := failed.Load().(error); err != nil {
		t.Fatal(err)
	}

	if !bytes.HasPrefix(data, []byte("PCPB")) || bytes.Contains(data, []byte("SQLite format 3")) {
		t.Error("the copy is not encrypted")
	}
	dir, summary := extractExport(t, data)
	manifest := summary.Manifest
	if summary.Header.KeyID != secrets.KeyID(testutil.MasterKey) || manifest.KeyID != summary.Header.KeyID ||
		manifest.CAPin != f.p.Authority.Pin() || manifest.Job != f.job.Id || manifest.Run != run.Id || manifest.Hosts != 5 ||
		manifest.Migration != "1772000000_panel_recovery.go" || manifest.Storage != panelbackup.StorageLocal ||
		fmt.Sprint(manifest.AgentsDial) != "[{192.0.2.2:9443 2} {panel.example.com:9443 1}]" || manifest.Audit.Seq == 0 {
		t.Errorf("the manifest: %+v", manifest)
	}

	restored := extractedApp(t, dir)
	sealKey, err := secrets.DeriveKey(testutil.MasterKey, audit.SealPurpose)
	if err != nil {
		t.Fatal(err)
	}
	result, err := audit.Verify(restored, sealKey)
	if err != nil || !result.OK || !result.Sealed || result.LastSeq != int(manifest.Audit.Seq) {
		t.Errorf("the copy's audit log: %+v %v, the manifest's head %+v", result, err, manifest.Audit)
	}
	var head struct {
		Seq  int64  `db:"seq"`
		Hash string `db:"hash"`
	}
	if err := restored.DB().NewQuery("SELECT seq, hash FROM _pc_audit_head WHERE id = 1").One(&head); err != nil ||
		head.Seq != manifest.Audit.Seq || head.Hash != manifest.Audit.Hash {
		t.Errorf("the copy's head %+v %v, the manifest's %+v", head, err, manifest.Audit)
	}
	// the panel wrote on: the copy is of one moment, not of the end
	live, err := audit.Verify(f.app, sealKey)
	if err != nil || !live.OK || live.LastSeq <= result.LastSeq {
		t.Errorf("the panel's own log %+v %v, the copy's %d", live, err, result.LastSeq)
	}
}

// TestTheExportLeavesOutLogsMetricsAndBundles (#32): the copy holds the
// database and the stored files it names, in path order, and leaves out the
// request logs, the metrics, the diagnostic bundles and whatever else is in
// the data dir.
func TestTheExportLeavesOutLogsMetricsAndBundles(t *testing.T) {
	f := newExportFixture(t)
	other := testutil.NewHost(t, f.app, "node-2")
	_, first, firstPath := f.commandWithOutput(other, []byte("the first output"))
	_, second, _ := f.commandWithOutput(other, []byte("the second output"))
	bundles, err := f.app.FindCollectionByNameOrId(schema.DiagnosticBundles)
	if err != nil {
		t.Fatal(err)
	}
	bundle := core.NewRecord(bundles)
	bundle.Set("host", other.Id)
	bundleName, bundlePath := f.storedFile(bundle, "file", []byte("what a server said"))
	if _, err := os.Stat(bundlePath); err != nil {
		t.Fatal(err)
	}
	// a thumbnail and a file no record names
	if err := os.WriteFile(filepath.Join(filepath.Dir(firstPath), "orphan.txt"), []byte("nobody's"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"metrics.db", "auxiliary.db"} {
		if _, err := os.Stat(filepath.Join(f.app.DataDir(), name)); err != nil {
			t.Fatalf("the panel has no %s: %v", name, err)
		}
	}

	dir, summary := extractExport(t, f.fetch(f.deliveredRun(f.target)))
	got := []string{}
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			got = append(got, filepath.ToSlash(rel))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"data.db", first, second}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the copy holds %v, not %v", got, want)
	}
	if content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(first))); err != nil || string(content) != "the first output" {
		t.Errorf("a stored file: %q %v", content, err)
	}
	manifest := summary.Manifest
	if manifest.Files != 2 || summary.Files != 2 || len(manifest.Missing) != 0 || strings.Join(manifest.LeftOut, ",") != "auxiliary.db,metrics.db,diagnostic_bundles" ||
		slices.Contains(got, bundleName) {
		t.Errorf("the manifest: %+v", manifest)
	}
	// the bundle's record is in the copy, its file is not
	restored := extractedApp(t, dir)
	if record, err := restored.FindRecordById(schema.DiagnosticBundles, bundle.Id); err != nil || record.GetString("file") == "" {
		t.Errorf("the bundle's record: %v", err)
	}
}

// TestADeletedFileIsListedMissing (#32): a file the copy names that is gone
// from the data dir is listed missing, and the copy is made without it.
func TestADeletedFileIsListedMissing(t *testing.T) {
	f := newExportFixture(t)
	_, kept, _ := f.commandWithOutput(f.idle(), []byte("kept"))
	_, gone, gonePath := f.commandWithOutput(f.idle(), []byte("gone"))
	if err := os.Remove(gonePath); err != nil {
		t.Fatal(err)
	}
	dir, summary := extractExport(t, f.fetch(f.deliveredRun(f.target)))
	if summary.Manifest.Files != 1 || strings.Join(summary.Manifest.Missing, ",") != gone {
		t.Errorf("the manifest: %+v", summary.Manifest)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(kept))); err != nil {
		t.Errorf("the file that is there: %v", err)
	}
}

// TestAnExportOfS3StorageHoldsNoFiles (#32): while PocketBase keeps the
// stored files in S3, the copy holds the database alone and says so.
func TestAnExportOfS3StorageHoldsNoFiles(t *testing.T) {
	f := newExportFixture(t)
	f.commandWithOutput(f.idle(), []byte("an output"))
	f.app.Settings().S3.Enabled = true
	t.Cleanup(func() { f.app.Settings().S3.Enabled = false })
	_, summary := extractExport(t, f.fetch(f.deliveredRun(f.target)))
	if summary.Manifest.Storage != panelbackup.StorageS3 || summary.Manifest.Files != 0 || len(summary.Manifest.Missing) != 0 {
		t.Errorf("the manifest: %+v", summary.Manifest)
	}
}

// withSpace makes the spool report the room a function says, for one test.
func withSpace(t *testing.T, space func(dir string) (uint64, error)) {
	t.Helper()
	before := availableSpace
	availableSpace = space
	t.Cleanup(func() { availableSpace = before })
}

// TestTheExportHoldsPocketBasesBackupKey (#32): while the panel copies
// itself, PocketBase's own backups and restores do not start, and after it
// they do.
func TestTheExportHoldsPocketBasesBackupKey(t *testing.T) {
	f := newExportFixture(t)
	run := f.deliveredRun(f.target)
	var held any
	var during error
	withSpace(t, func(dir string) (uint64, error) {
		held = f.app.Store().Get(core.StoreKeyActiveBackup)
		during = f.app.CreateBackup(context.Background(), "during.zip")
		return freeSpace(dir)
	})
	f.fetch(run)
	if held != "pocketcloud-panel-export-"+run.Id || during == nil {
		t.Errorf("during the copy: %v held, PocketBase's backup %v", held, during)
	}
	if f.app.Store().Has(core.StoreKeyActiveBackup) {
		t.Error("the key stayed")
	}
}

// TestItWaitsForPocketBasesOwnBackup (#32): while PocketBase makes or
// restores a backup, a run's copy is not made, with a sentence the run
// keeps, and nothing is left in the spool.
func TestItWaitsForPocketBasesOwnBackup(t *testing.T) {
	f := newExportFixture(t)
	run := f.deliveredRun(f.target)
	f.app.Store().Set(core.StoreKeyActiveBackup, "pb_backup_x.zip")
	_, err := f.p.fetchArtifact(f.host.Id, backup.PanelExportID(run.Id))
	if err == nil || errors.Is(err, gateway.ErrNoArtifact) || err.Error() != "PocketBase is making or restoring a backup: the run is tried again." {
		t.Errorf("during PocketBase's backup: %v", err)
	}
	if f.app.Store().Get(core.StoreKeyActiveBackup) != "pb_backup_x.zip" || len(f.spool()) != 0 {
		t.Errorf("PocketBase's key %v, the spool %v", f.app.Store().Get(core.StoreKeyActiveBackup), f.spool())
	}
	f.app.Store().Remove(core.StoreKeyActiveBackup)
	f.fetch(run)
}

// TestAFullDiskFailsWithItsSentence (#32): a copy the spool has no room for
// is not started: the run says how much is free and how much it needs.
func TestAFullDiskFailsWithItsSentence(t *testing.T) {
	f := newExportFixture(t)
	f.commandWithOutput(f.idle(), bytes.Repeat([]byte("x"), 300<<10))
	run := f.deliveredRun(f.target)
	asked, want := "", int64(0)
	withSpace(t, func(dir string) (uint64, error) {
		asked = dir
		// twice the database and its log, the stored file, a tenth more
		for _, name := range []string{"data.db", "data.db-wal"} {
			if info, err := os.Stat(filepath.Join(f.app.DataDir(), name)); err == nil {
				want += 2 * info.Size()
			}
		}
		want += 300 << 10
		want += want / 10
		return 1536, nil
	})
	_, err := f.p.fetchArtifact(f.host.Id, backup.PanelExportID(run.Id))
	if err == nil || err.Error() != "The panel has no room to prepare its backup: 1.5 KiB free, about "+bytesText(uint64(want))+" needed." {
		t.Fatalf("on a full disk: %v, about %s needed", err, bytesText(uint64(want)))
	}
	if asked != filepath.Join(f.app.DataDir(), exportSpool) || len(f.spool()) != 0 {
		t.Errorf("asked about %s; the spool %v", asked, f.spool())
	}
}

// TestTheSpoolIsPrivateAndThePlainCopyGone (#32): the spool and its copies
// are the panel's alone, and the plain copy of the database never stays:
// not after a copy is made, nor after one failed once the database was
// copied.
func TestTheSpoolIsPrivateAndThePlainCopyGone(t *testing.T) {
	f := newExportFixture(t)
	spool := filepath.Join(f.app.DataDir(), exportSpool)
	if err := os.MkdirAll(spool, 0o755); err != nil {
		t.Fatal(err)
	}
	run := f.deliveredRun(f.target)
	f.fetch(run)
	if info, err := os.Stat(spool); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the spool: %v %v", info.Mode(), err)
	}
	names := f.spool()
	if len(names) != 1 || !strings.HasSuffix(names[0], ".pcpb") || !strings.HasPrefix(names[0], f.job.Id+"-") {
		t.Fatalf("the spool holds %v", names)
	}
	if info, err := os.Stat(filepath.Join(spool, names[0])); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the copy: %v %v", info.Mode(), err)
	}

	// a database that names another master key fails after its copy
	if _, err := f.app.DB().NewQuery("UPDATE _pc_panel SET key_id = 'ffffffffffffffffffffffffffffffff' WHERE id = 1").Execute(); err != nil {
		t.Fatal(err)
	}
	other := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	f.job.Set("targets", []string{f.target.Id, other.Id})
	if err := f.app.Save(f.job); err != nil {
		t.Fatal(err)
	}
	// the first copy is too old to serve
	f.p.exports.jobs[f.job.Id][0].built = time.Now().Add(-exportReuse)
	late := f.deliveredRun(other)
	_, err := f.p.fetchArtifact(f.host.Id, backup.PanelExportID(late.Id))
	if err == nil || err.Error() != "the panel's database names the master key ffffffffffffffffffffffffffffffff, not the one the panel runs with ("+secrets.KeyID(testutil.MasterKey)+")" {
		t.Errorf("another key's database: %v", err)
	}
	if after := f.spool(); !slices.Equal(after, names) {
		t.Errorf("the spool holds %v after a copy failed", after)
	}

	// a stored file that cannot be read fails the copy as it is written
	if _, err := f.app.DB().NewQuery("UPDATE _pc_panel SET key_id = '' WHERE id = 1").Execute(); err != nil {
		t.Fatal(err)
	}
	f.commandWithOutput(f.idle(), []byte("an output"))
	before := openStored
	openStored = func(path string) (storedSource, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		// the part of the copy is there by now
		if parts, _ := filepath.Glob(filepath.Join(spool, "*.pcpb.part")); len(parts) != 1 {
			t.Errorf("the copy is written to %v", parts)
		}
		return brokenFile{file}, nil
	}
	t.Cleanup(func() { openStored = before })
	_, err = f.p.fetchArtifact(f.host.Id, backup.PanelExportID(late.Id))
	if err == nil || !strings.Contains(err.Error(), "the disk failed") {
		t.Errorf("a file that cannot be read: %v", err)
	}
	if after := f.spool(); !slices.Equal(after, names) {
		t.Errorf("the spool holds %v after a copy failed as it was written", after)
	}
}

// brokenFile is a file whose reads fail.
type brokenFile struct{ *os.File }

func (brokenFile) ReadAt([]byte, int64) (int, error) { return 0, errors.New("the disk failed") }

// TestADamagedCopyFailsWithItsSentence (#32): a copy of the database that
// SQLite finds damaged is no backup.
func TestADamagedCopyFailsWithItsSentence(t *testing.T) {
	f := newExportFixture(t)
	path := filepath.Join(t.TempDir(), "copy.db")
	if _, err := f.app.ConcurrentDB().NewQuery("VACUUM INTO {:path}").Bind(dbx.Params{"path": path}).Execute(); err != nil {
		t.Fatal(err)
	}
	if facts, err := readExportCopy(path); err != nil || facts.migration == "" {
		t.Fatalf("a good copy: %v", err)
	} else {
		facts.close()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// the b-tree pages after the first, overwritten
	for i := 4096; i < len(raw) && i < 64<<10; i++ {
		raw[i] = 0x5a
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readExportCopy(path); err == nil || !strings.HasPrefix(err.Error(), "the copy of the panel's database is damaged: *** in database main ***") {
		t.Errorf("a damaged copy: %v", err)
	}
	// SQLite does not even read it as a database
	if err := os.WriteFile(path, bytes.Repeat([]byte("not a database "), 512), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readExportCopy(path); err == nil || !strings.HasPrefix(err.Error(), "the copy of the panel's database is damaged: file is not a database") {
		t.Errorf("no database: %v", err)
	}
}

// TestTheRunsOfAMinuteShareOneExport (#32): the runs of a minute, one per
// target, get one copy, made once; a copy serves for 15 minutes, after which
// a run gets a new one, while a run that fetched the old one gets it again,
// as when its download resumes. A new copy takes the place of the old ones
// that no run still going fetched.
func TestTheRunsOfAMinuteShareOneExport(t *testing.T) {
	f := newExportFixture(t)
	nas := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	sftp := f.addTarget("sftp", backup.KindS3, f.host, backup.Config{Endpoint: "https://sftp.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	late := f.addTarget("late", backup.KindS3, f.host, backup.Config{Endpoint: "https://late.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	f.job.Set("targets", []string{f.target.Id, nas.Id, sftp.Id, late.Id})
	if err := f.app.Save(f.job); err != nil {
		t.Fatal(err)
	}
	first, second := f.deliveredRun(f.target), f.deliveredRun(nas)

	// both ask at once: one copy is made
	copies := make([][]byte, 2)
	var both sync.WaitGroup
	for i, run := range []*core.Record{first, second} {
		both.Go(func() {
			artifact, err := f.p.fetchArtifact(f.host.Id, backup.PanelExportID(run.Id))
			if err != nil {
				t.Error(err)
				return
			}
			defer artifact.Reader.Close()
			copies[i], _ = io.ReadAll(artifact.Reader)
		})
	}
	both.Wait()
	if len(copies[0]) == 0 || !bytes.Equal(copies[0], copies[1]) || len(f.spool()) != 1 {
		t.Fatalf("two runs of a minute: %d and %d bytes, the spool %v", len(copies[0]), len(copies[1]), f.spool())
	}

	// 15 minutes later: a new run gets a new copy, the first its own again
	f.p.exports.jobs[f.job.Id][0].built = time.Now().Add(-exportReuse)
	third := f.deliveredRun(sftp)
	newer := f.fetch(third)
	if bytes.Equal(newer, copies[0]) || len(f.spool()) != 2 {
		t.Errorf("a run 15 minutes later: the same copy %v, the spool %v", bytes.Equal(newer, copies[0]), f.spool())
	}
	if again := f.fetch(first); !bytes.Equal(again, copies[0]) {
		t.Error("a run that fetched the old copy got another")
	}
	if again := f.fetch(third); !bytes.Equal(again, newer) {
		t.Error("the new run got another copy")
	}

	// once the runs that fetched the old copy ended, a new copy takes its place
	for _, run := range []*core.Record{first, second} {
		f.endRun(run, map[string]any{"status": schema.CommandSucceeded, "result": map[string]any{"snapshot_id": "4f1c2d3e"}})
	}
	f.p.exports.jobs[f.job.Id][0].built = time.Now().Add(-exportReuse)
	f.fetch(f.deliveredRun(late))
	if spool := f.spool(); len(spool) != 2 || len(f.p.exports.jobs[f.job.Id]) != 2 {
		t.Errorf("the spool %v, %d copies kept", spool, len(f.p.exports.jobs[f.job.Id]))
	}
	if _, err := os.Stat(f.p.exports.jobs[f.job.Id][1].path); err != nil {
		t.Errorf("the copy a running run fetched: %v", err)
	}

	// a copy whose file is gone from the spool: its run gets a new one
	if err := os.Remove(f.p.exports.jobs[f.job.Id][1].path); err != nil {
		t.Fatal(err)
	}
	if again := f.fetch(third); bytes.Equal(again, newer) || len(f.p.exports.jobs[f.job.Id]) != 2 {
		t.Errorf("a run whose copy is gone: the same %v, %d copies kept", bytes.Equal(again, newer), len(f.p.exports.jobs[f.job.Id]))
	}
}

// TestAChangeRunGetsACopyMadeAfterIt (#32): a copy serves no run created
// after it read the database, however young it is: a change run soon after
// a run of the job backs up the server enrolled in between, and names
// itself; the change runs to the job's targets, created together, still
// share one copy.
func TestAChangeRunGetsACopyMadeAfterIt(t *testing.T) {
	f := newExportFixture(t)
	nas := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	f.job.Set("targets", []string{f.target.Id, nas.Id})
	if err := f.app.Save(f.job); err != nil {
		t.Fatal(err)
	}
	manifestOf := func(data []byte) panelbackup.Manifest {
		t.Helper()
		reader, err := panelbackup.NewReader(bytes.NewReader(data), testutil.MasterKey)
		if err != nil {
			t.Fatal(err)
		}
		return reader.Manifest()
	}

	first := f.deliveredRun(f.target)
	before := f.fetch(first)
	if manifest := manifestOf(before); manifest.Hosts != 1 || manifest.Run != first.Id {
		t.Fatalf("the first copy: %d servers, run %s", manifest.Hosts, manifest.Run)
	}
	f.endRun(first, map[string]any{"status": schema.CommandSucceeded, "result": map[string]any{"snapshot_id": "4f1c2d3e"}})

	// then a server enrolls, and its change run comes while the first
	// copy is still young
	testutil.NewHost(t, f.app, "node-2")
	f.p.panelBackupAfterChange()
	for range 2 {
		f.received(f.stream)
	}
	changes := f.changeRuns(f.job)
	if len(changes) != 2 {
		t.Fatalf("%d change runs", len(changes))
	}
	copies := [][]byte{}
	for _, change := range changes {
		f.eventually("the change run's command delivered", func() bool {
			command, err := f.app.FindRecordById(schema.Commands, f.fresh(change).GetString("command"))
			return err == nil && command.GetString("status") == schema.CommandDispatched
		})
		copies = append(copies, f.fetch(change))
	}
	if bytes.Equal(copies[0], before) {
		t.Fatal("the change run got the copy made before the change")
	}
	if manifest := manifestOf(copies[0]); manifest.Hosts != 2 || manifest.Run != changes[0].Id {
		t.Errorf("the change run's copy: %d servers, run %s, not 2 and %s", manifest.Hosts, manifest.Run, changes[0].Id)
	}
	if !bytes.Equal(copies[1], copies[0]) {
		t.Error("the change runs to the two targets got two copies")
	}
}

// TestARunCreatedWhileACopyIsMadeGetsANewOne (#32): a copy holds what the
// database held when it was read, so a run created while the copy is
// written, after that read, gets a copy of its own.
func TestARunCreatedWhileACopyIsMadeGetsANewOne(t *testing.T) {
	f := newExportFixture(t)
	nas := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	f.job.Set("targets", []string{f.target.Id, nas.Id})
	if err := f.app.Save(f.job); err != nil {
		t.Fatal(err)
	}
	// a stored file, which the copy reads after the database
	f.commandWithOutput(f.idle(), []byte("an output"))
	first := f.deliveredRun(f.target)

	var late *core.Record
	before := openStored
	openStored = func(path string) (storedSource, error) {
		if late == nil {
			late = f.deliveredRun(nas)
		}
		return os.Open(path)
	}
	t.Cleanup(func() { openStored = before })
	copied := f.fetch(first)
	openStored = before
	if late == nil {
		t.Fatal("no run was created while the copy was written")
	}
	if again := f.fetch(late); bytes.Equal(again, copied) {
		t.Error("a run created while the copy was written got it")
	}
}

// TestACopyNeedsTheMasterKey (#32): a panel that did not load its master key
// makes no copy.
func TestACopyNeedsTheMasterKey(t *testing.T) {
	f := newExportFixture(t)
	run := f.deliveredRun(f.target)
	key := f.p.masterKey
	f.p.masterKey = nil
	if _, err := f.p.fetchArtifact(f.host.Id, backup.PanelExportID(run.Id)); err == nil || err.Error() != "the panel has not loaded its master key" {
		t.Errorf("without the key: %v", err)
	}
	f.p.masterKey = key
	if len(f.spool()) != 0 {
		t.Errorf("the spool %v", f.spool())
	}
	f.fetch(run)
}

// TestTheSpoolIsSwept (#32): the sweep removes the copies older than 15
// minutes that no run still going fetched, the copies of a panel that
// stopped once they are as old, and at once what a build left that is not
// under way, as a plain copy; it keeps the rest.
func TestTheSpoolIsSwept(t *testing.T) {
	f := newExportFixture(t)
	nas := f.addTarget("nas", backup.KindS3, f.host, backup.Config{Endpoint: "https://nas.example.com", Bucket: "b"}, backup.Credentials{AccessKeyID: "k", SecretAccessKey: "s"})
	f.job.Set("targets", []string{f.target.Id, nas.Id})
	if err := f.app.Save(f.job); err != nil {
		t.Fatal(err)
	}
	going := f.deliveredRun(f.target)
	f.fetch(going)
	f.p.exports.jobs[f.job.Id][0].built = time.Now().Add(-exportReuse)
	ended := f.deliveredRun(nas)
	f.fetch(ended)
	f.endRun(ended, map[string]any{"status": schema.CommandSucceeded, "result": map[string]any{"snapshot_id": "4f1c2d3e"}})
	copies := f.p.exports.jobs[f.job.Id]
	if len(copies) != 2 {
		t.Fatalf("%d copies", len(copies))
	}
	young, old := copies[0], copies[1]

	spool := filepath.Join(f.app.DataDir(), exportSpool)
	now := time.Now()
	leftover := map[string]time.Time{
		"aaaaaaaaaaaaaaa-old.pcpb":          now.Add(-time.Hour),
		"aaaaaaaaaaaaaaa-young.pcpb":        now.Add(-time.Minute),
		"aaaaaaaaaaaaaaa-plain.db":          now.Add(-time.Minute),
		"aaaaaaaaaaaaaaa-plain.db-wal":      now.Add(-time.Minute),
		"aaaaaaaaaaaaaaa-part.pcpb.part":    now.Add(-time.Minute),
		f.job.Id + "-building.db":           now.Add(-time.Hour),
		f.job.Id + "-building.pcpb.part":    now.Add(-time.Hour),
		"aaaaaaaaaaaaaaa-unfinished.pcpb.x": now.Add(-time.Minute),
	}
	for name, at := range leftover {
		path := filepath.Join(spool, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	f.p.exports.mark(f.job.Id+"-building", true)

	// within 15 minutes of the new copy, and the old one fetched by a run
	// still going: only the leftovers go
	f.p.sweepPanelExports(now)
	want := []string{filepath.Base(old.path), filepath.Base(young.path), "aaaaaaaaaaaaaaa-young.pcpb", f.job.Id + "-building.db", f.job.Id + "-building.pcpb.part"}
	slices.Sort(want)
	if got := f.spool(); !slices.Equal(got, want) {
		t.Errorf("after the first sweep %v, not %v", got, want)
	}
	// and both still serve: the new one the runs of its minute
	if copies := f.p.exports.jobs[f.job.Id]; len(copies) != 2 || copies[0] != young || copies[1] != old {
		t.Errorf("%d copies kept", len(copies))
	}

	// later, the run ended: both copies and the young leftover go, the
	// build under way stays
	f.endRun(going, map[string]any{"status": schema.CommandSucceeded, "result": map[string]any{"snapshot_id": "4f1c2d3e"}})
	f.p.sweepPanelExports(now.Add(exportReuse))
	want = []string{f.job.Id + "-building.db", f.job.Id + "-building.pcpb.part"}
	if got := f.spool(); !slices.Equal(got, want) || len(f.p.exports.jobs[f.job.Id]) != 0 {
		t.Errorf("after the second sweep %v, not %v; %d copies kept", got, want, len(f.p.exports.jobs[f.job.Id]))
	}
	f.p.exports.mark(f.job.Id+"-building", false)
	f.p.sweepPanelExports(now.Add(exportReuse))
	if got := f.spool(); len(got) != 0 {
		t.Errorf("after the build ended %v", got)
	}

	// as the panel starts again: its serve hooks run once more, and the
	// gateway they start replaces the first
	if err := os.WriteFile(filepath.Join(spool, "bbbbbbbbbbbbbbb-crashed.db"), []byte("plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := f.p.Gateway
	testutil.Handler(t, f.app)
	first.Stop()
	if got := f.spool(); len(got) != 0 {
		t.Errorf("at start %v", got)
	}
}
