package panel

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/panelbackup"
	"github.com/oldbear24/PocketServerManager/internal/recoverykit"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

const kitPassphrase = "Příliš žluťoučký kůň úpěl"

// madeAt is when the test backups were made.
var madeAt = time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC)

// recoverFixture is a panel's database, its backup and its recovery kit, as
// a new machine gets them.
type recoverFixture struct {
	app  *core.BaseApp
	pin  string
	work string
	// export and kit are the files `panel recover` reads
	export, kit string
	// dir and keyFile are where it restores
	dir, keyFile string
	manifest     panelbackup.Manifest
}

// panelDatabase is the database of a panel that served: its CA, its key id
// and pin, two servers, an audit log and a stored file.
func panelDatabase(t *testing.T) (*core.BaseApp, string) {
	t.Helper()
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)
	authority := caOfAnEarlierStart(t, app)
	if err := setPanelState(app.DB(), dbx.Params{"key_id": secrets.KeyID(testutil.MasterKey), "ca_pin": authority.Pin()}); err != nil {
		t.Fatal(err)
	}
	testutil.NewHost(t, app, "vm")
	testutil.NewHost(t, app, "node-b")
	return app.BaseApp, authority.Pin()
}

// exportOf writes a PCPB1 export of the app's database, sealed with master,
// as the panel's backup makes it: a copy by VACUUM INTO, changed by change
// if it is given, and one stored file.
func exportOf(t *testing.T, app core.App, master []byte, pin, path string, change func(db *dbx.DB)) panelbackup.Manifest {
	t.Helper()
	dir := t.TempDir()
	copyPath := filepath.Join(dir, "data.db")
	if _, err := app.DB().NewQuery("VACUUM INTO {:path}").Bind(dbx.Params{"path": copyPath}).Execute(); err != nil {
		t.Fatal(err)
	}
	if change != nil {
		db, err := dbx.Open("sqlite", copyPath)
		if err != nil {
			t.Fatal(err)
		}
		change(db)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	stored := []byte("a dump of a removed database")
	hosts, _ := app.CountRecords(schema.Hosts)
	manifest := panelbackup.Manifest{
		PanelVersion: "0.32.0", Migration: "1772000000_panel_recovery.go", CAPin: pin,
		Audit:      panelbackup.AuditHead{Seq: 3812, Hash: strings.Repeat("ab", 32)},
		AgentsDial: []panelbackup.Dial{{Address: "192.0.2.2:9443", Servers: 2}}, Hosts: int(hosts),
		Job: "abc123def456ghi", Run: "run123run456run", Storage: panelbackup.StorageLocal, LeftOut: panelbackup.LeftOut,
		Files: 1, Bytes: int64(len(data) + len(stored)),
	}
	var out bytes.Buffer
	w, err := panelbackup.NewWriter(&out, master, madeAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Manifest(manifest); err != nil {
		t.Fatal(err)
	}
	if err := w.Database(bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if err := w.File("storage/pbc_1234567890/abcdefghijklmno/dump.sql.gz", bytes.NewReader(stored), int64(len(stored))); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return manifest
}

// kitOf seals a recovery kit of master and pin into path.
func kitOf(t *testing.T, master []byte, pin, path string) {
	t.Helper()
	id, err := recoverykit.NewKitID()
	if err != nil {
		t.Fatal(err)
	}
	header := recoverykit.Header{
		Kit: id, Created: time.Date(2026, 11, 17, 10, 2, 0, 0, time.UTC), Panel: "http://192.0.2.2:8090",
		KeyID: secrets.KeyID(master), CAPin: pin, PanelVersion: "0.32.0", AgentImage: "ghcr.io/oldbear24/pocketcloud-agent:0.32.0",
	}
	payload := recoverykit.Payload{
		CreatedBy: "admin@example.com", MasterKey: hex.EncodeToString(master),
		Env:        map[string]string{"POCKETCLOUD_PUBLIC_URL": "http://192.0.2.2:8090", "POCKETCLOUD_GRPC_ADDR": ":9443"},
		AgentsDial: []recoverykit.Dial{{Address: "192.0.2.2:9443", Servers: 2, IP: true}},
	}
	text, err := recoverykit.Seal(header, payload, kitPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newRecoverFixture(t *testing.T) *recoverFixture {
	t.Helper()
	app, pin := panelDatabase(t)
	work := t.TempDir()
	f := &recoverFixture{app: app, pin: pin, work: work,
		export: filepath.Join(work, "sftp.pcpb"), kit: filepath.Join(work, "kit.pckit"),
		dir: filepath.Join(work, "data", "pb_data"), keyFile: filepath.Join(work, "secrets", "pocketcloud-master-key")}
	for _, dir := range []string{filepath.Dir(f.dir), filepath.Dir(f.keyFile)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.manifest = exportOf(t, app, testutil.MasterKey, pin, f.export, nil)
	kitOf(t, testutil.MasterKey, pin, f.kit)
	return f
}

func (f *recoverFixture) options(out *bytes.Buffer) RecoverOptions {
	return RecoverOptions{Kit: f.kit, From: f.export, Dir: f.dir, MasterKeyFile: f.keyFile, Passphrase: kitPassphrase, Out: out}
}

// untouched checks that a refused recovery wrote nothing: no key file, no
// pb_data, no staging directory.
func (f *recoverFixture) untouched(t *testing.T, keyBefore []byte, dirBefore []string) {
	t.Helper()
	key, err := os.ReadFile(f.keyFile)
	if keyBefore == nil && !errors.Is(err, fs.ErrNotExist) || keyBefore != nil && !bytes.Equal(key, keyBefore) {
		t.Errorf("the key file was written: %q, %v", key, err)
	}
	if got := entries(t, f.dir); strings.Join(got, ",") != strings.Join(dirBefore, ",") {
		t.Errorf("pb_data holds %v, before %v", got, dirBefore)
	}
	for _, entry := range entries(t, filepath.Dir(f.dir)) {
		if strings.Contains(entry, ".restore-") || strings.Contains(entry, ".replaced-") {
			t.Errorf("%s is left", entry)
		}
	}
}

// entries lists a directory's entries; nil when it does not exist.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range list {
		names = append(names, entry.Name())
	}
	return names
}

// TestRecoverRestoresIntoAnEmptyDirectory (#32): the backup lands as
// pb_data with its stored files and the restore's marker, the kit's key in
// a file of mode 0600, and the summary says what was restored and what to
// do next. A panel then starts from it, with the backup's CA.
func TestRecoverRestoresIntoAnEmptyDirectory(t *testing.T) {
	f := newRecoverFixture(t)
	var out bytes.Buffer
	options := f.options(&out)
	options.EnvOut = filepath.Join(f.work, "panel.env")
	if err := Recover(options); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(f.keyFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the key file: %v, %v", info, err)
	}
	if key, _ := os.ReadFile(f.keyFile); string(key) != hex.EncodeToString(testutil.MasterKey)+"\n" {
		t.Fatalf("the key file holds %q", key)
	}
	if stored, err := os.ReadFile(filepath.Join(f.dir, "storage/pbc_1234567890/abcdefghijklmno/dump.sql.gz")); err != nil || string(stored) != "a dump of a removed database" {
		t.Fatalf("the stored file: %q, %v", stored, err)
	}
	raw, err := os.ReadFile(filepath.Join(f.dir, restoreMarkerName))
	if err != nil {
		t.Fatal(err)
	}
	var marker restoreMarker
	if err := json.Unmarshal(raw, &marker); err != nil || len(marker.ExportID) != 32 || marker.ExportCreated != "2026-11-17T04:00:00Z" ||
		marker.AuditSeq != 3812 || marker.PanelVersionExport != "0.32.0" || !strings.HasPrefix(marker.KitID, "KIT-") || marker.Migration != f.manifest.Migration {
		t.Fatalf("the marker: %s, %v", raw, err)
	}
	if left := entries(t, filepath.Dir(f.dir)); strings.Join(left, ",") != "pb_data" {
		t.Fatalf("next to pb_data: %v", left)
	}

	want := []string{
		"Opened the recovery kit " + marker.KitID + " of http://192.0.2.2:8090, made 2026-11-17 10:02 UTC by admin@example.com.",
		"The backup was made 2026-11-17 04:00 UTC by panel 0.32.0: 2 servers, audit log up to entry 3812, CA " + f.pin[:11] + "….",
		"Checked it: every part opens with the kit's key, data.db passes SQLite's integrity check, and its CA opens with the kit's key.",
		"Wrote the master key to " + f.keyFile + ".",
		"Restored " + f.dir + ": data.db and 1 stored file, " + size(f.manifest.Bytes) + ".",
		"Servers dial 192.0.2.2:9443 (an IP address: this machine must take it over). Start the panel there with " + options.EnvOut + ".",
		"Before the first start, run: panel audit verify. Nothing done after 2026-11-17 04:00 UTC is in this backup: servers enrolled since must enroll again.",
	}
	if got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the summary:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// a panel starts from it with the key file, and finishes the restore
	restored := core.NewBaseApp(core.BaseAppConfig{DataDir: f.dir, EncryptionEnv: "pb_test_env"})
	if err := restored.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopPanel(t, restored)
		_ = restored.ClearBootstrap()
	})
	if err := restored.RunAllMigrations(); err != nil {
		t.Fatal(err)
	}
	p := New(restored, &Config{MasterKeyFile: f.keyFile}, testUI)
	p.Register()
	if err := serve(t, restored); err != nil {
		t.Fatal(err)
	}
	if p.Authority.Pin() != f.pin {
		t.Fatalf("the restored panel's CA is %s, the backup's %s", p.Authority.Pin(), f.pin)
	}
	if _, err := os.Stat(filepath.Join(f.dir, restoreMarkerName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the first start left the marker")
	}
	if entry := newestSecurityEntry(t, restored); entry.GetString("action") != "panel_restored" {
		t.Fatalf("the newest security entry is %s", entry.GetString("action"))
	}
}

// TestRecoverWritesTheStartEnvironment (#32): --env-out is a docker
// --env-file of the kit's environment and the expected CA pin.
func TestRecoverWritesTheStartEnvironment(t *testing.T) {
	f := newRecoverFixture(t)
	options := f.options(&bytes.Buffer{})
	options.EnvOut = filepath.Join(f.work, "panel.env")
	if err := Recover(options); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(options.EnvOut)
	if err != nil {
		t.Fatal(err)
	}
	want := "POCKETCLOUD_GRPC_ADDR=:9443\nPOCKETCLOUD_PUBLIC_URL=http://192.0.2.2:8090\nPOCKETCLOUD_EXPECT_CA_PIN=" + f.pin + "\n"
	if string(raw) != want {
		t.Fatalf("the environment:\n%s\nwant:\n%s", raw, want)
	}
}

// TestRecoverDryRunWritesNothing (#32).
func TestRecoverDryRunWritesNothing(t *testing.T) {
	f := newRecoverFixture(t)
	var out bytes.Buffer
	options := f.options(&out)
	options.DryRun = true
	options.EnvOut = filepath.Join(f.work, "panel.env")
	if err := Recover(options); err != nil {
		t.Fatal(err)
	}
	f.untouched(t, nil, nil)
	if _, err := os.Stat(options.EnvOut); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a dry run wrote the environment")
	}
	if !strings.Contains(out.String(), "Checked it: every part opens with the kit's key") || !strings.Contains(out.String(), "Dry run: nothing was written") {
		t.Fatalf("the summary: %s", out.String())
	}
}

// TestRecoverReplacesOnlyWhenAsked (#32): a pb_data that holds a panel, such
// as one a start on empty volumes made, is refused without --replace and
// kept as pb_data.replaced-<UTC> with it: nothing is deleted.
func TestRecoverReplacesOnlyWhenAsked(t *testing.T) {
	f := newRecoverFixture(t)
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "data.db"), []byte("a panel of its own"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := Recover(f.options(&bytes.Buffer{}))
	if err == nil || !strings.HasPrefix(err.Error(), f.dir+" already holds a panel (it may have started with a CA of its own): run again with --replace to keep it as "+f.dir+".replaced-") {
		t.Fatalf("a pb_data in use: %v", err)
	}
	f.untouched(t, nil, []string{"data.db"})

	var out bytes.Buffer
	options := f.options(&out)
	options.Replace = true
	if err := Recover(options); err != nil {
		t.Fatal(err)
	}
	var replaced string
	for _, entry := range entries(t, filepath.Dir(f.dir)) {
		if strings.HasPrefix(entry, "pb_data.replaced-") {
			replaced = filepath.Join(filepath.Dir(f.dir), entry)
		}
	}
	if old, err := os.ReadFile(filepath.Join(replaced, "data.db")); err != nil || string(old) != "a panel of its own" {
		t.Fatalf("what was there: %q, %v", old, err)
	}
	if !strings.Contains(out.String(), "; what was there is in "+replaced+".") {
		t.Fatalf("the summary: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(f.dir, restoreMarkerName)); err != nil {
		t.Fatal(err)
	}
}

// TestRecoverRefusals (#32): each check refuses with its sentence, and a
// refused recovery writes nothing.
func TestRecoverRefusals(t *testing.T) {
	for _, test := range []struct {
		name string
		// prepare changes the fixture; it may return the key file's
		// content before and pb_data's entries before
		prepare func(t *testing.T, f *recoverFixture) ([]byte, []string)
		want    string
	}{
		{"the serve lock is held", func(t *testing.T, f *recoverFixture) ([]byte, []string) {
			if err := os.MkdirAll(f.dir, 0o700); err != nil {
				t.Fatal(err)
			}
			lock, err := os.OpenFile(filepath.Join(f.dir, serveLockName), os.O_RDWR|os.O_CREATE, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = lock.Close() })
			if held, err := tryLock(lock); err != nil || !held {
				t.Fatal("the test's lock was not taken")
			}
			return nil, []string{serveLockName}
		}, "A panel is serving {dir}: stop it first."},
		{"a wrong passphrase", func(*testing.T, *recoverFixture) ([]byte, []string) { return nil, nil },
			"The passphrase does not open this kit, or the kit was changed."},
		{"another key in the backup's header", func(t *testing.T, f *recoverFixture) ([]byte, []string) {
			exportOf(t, f.app, otherKey, f.pin, f.export, nil)
			return nil, nil
		}, "This backup was sealed with another master key (id " + secrets.KeyID(otherKey) + ") than the kit's (id " + secrets.KeyID(testutil.MasterKey) +
			"): use the kit of the panel that made it."},
		{"a damaged segment", func(t *testing.T, f *recoverFixture) ([]byte, []string) {
			raw, err := os.ReadFile(f.export)
			if err != nil {
				t.Fatal(err)
			}
			raw[len(raw)/2] ^= 0x40
			if err := os.WriteFile(f.export, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			return nil, nil
		}, "The backup is damaged or incomplete at segment "},
		{"a cut backup", func(t *testing.T, f *recoverFixture) ([]byte, []string) {
			raw, _ := os.ReadFile(f.export)
			if err := os.WriteFile(f.export, raw[:len(raw)-10], 0o600); err != nil {
				t.Fatal(err)
			}
			return nil, nil
		}, "The backup is damaged or incomplete at segment 4."},
		{"no backup at all", func(t *testing.T, f *recoverFixture) ([]byte, []string) {
			if err := os.WriteFile(f.export, []byte("SQLite format 3\x00"), 0o600); err != nil {
				t.Fatal(err)
			}
			return nil, nil
		}, "{export} is no PocketCloud panel backup"},
		{"a migration of a newer panel", func(t *testing.T, f *recoverFixture) ([]byte, []string) {
			exportOf(t, f.app, testutil.MasterKey, f.pin, f.export, func(db *dbx.DB) {
				if _, err := db.Insert(core.DefaultMigrationsTable, dbx.Params{"file": "1799000000_from_the_future.go", "applied": 1}).Execute(); err != nil {
					t.Fatal(err)
				}
			})
			return nil, nil
		}, "The backup comes from a newer panel (migration 1799000000_from_the_future.go): recover with panel 0.32.0 or newer."},
		{"a CA sealed with another key", func(t *testing.T, f *recoverFixture) ([]byte, []string) {
			other, _ := secrets.New(otherKey)
			sealed, _ := other.Seal([]byte("-----BEGIN EC PRIVATE KEY-----"))
			exportOf(t, f.app, testutil.MasterKey, f.pin, f.export, func(db *dbx.DB) {
				if _, err := db.Update(schema.InternalCA, dbx.Params{"root_key": sealed}, nil).Execute(); err != nil {
					t.Fatal(err)
				}
			})
			return nil, nil
		}, "The backup's CA does not open with the kit's key: the kit is not of the panel that made it."},
		{"two CAs", func(t *testing.T, f *recoverFixture) ([]byte, []string) {
			exportOf(t, f.app, testutil.MasterKey, f.pin, f.export, func(db *dbx.DB) {
				if _, err := db.NewQuery("INSERT INTO " + schema.InternalCA + " (id, root_cert, root_key) SELECT 'secondcaxxxxxxx', root_cert, root_key FROM " + schema.InternalCA).Execute(); err != nil {
					t.Fatal(err)
				}
			})
			return nil, nil
		}, "The backup holds 2 internal CAs, not one: it is not a whole panel's database."},
		{"a database sealed with another key", func(t *testing.T, f *recoverFixture) ([]byte, []string) {
			exportOf(t, f.app, testutil.MasterKey, f.pin, f.export, func(db *dbx.DB) {
				if _, err := db.Update(schema.PanelTable, dbx.Params{"key_id": secrets.KeyID(otherKey)}, nil).Execute(); err != nil {
					t.Fatal(err)
				}
			})
			return nil, nil
		}, "The backup's database was sealed with another master key (id " + secrets.KeyID(otherKey) + ") than the kit's (id " + secrets.KeyID(testutil.MasterKey) +
			"): use the kit of the panel that made it."},
		{"another CA than the kit's", func(t *testing.T, f *recoverFixture) ([]byte, []string) {
			kitOf(t, testutil.MasterKey, "sha256:"+strings.Repeat("0f", 32), f.kit)
			return nil, nil
		}, "This backup's CA (pin {pin}) is not the kit's (pin sha256:" + strings.Repeat("0f", 32) + "): use the kit of the panel that made it."},
		{"a CA other than the manifest names", func(t *testing.T, f *recoverFixture) ([]byte, []string) {
			named := "sha256:" + strings.Repeat("0e", 32)
			exportOf(t, f.app, testutil.MasterKey, named, f.export, nil)
			kitOf(t, testutil.MasterKey, named, f.kit)
			return nil, nil
		}, "The backup's CA is not the one its manifest names (pin sha256:" + strings.Repeat("0e", 32) + "): the backup is not whole."},
		{"a damaged database", func(t *testing.T, f *recoverFixture) ([]byte, []string) {
			exportOf(t, f.app, testutil.MasterKey, f.pin, f.export, func(db *dbx.DB) {
				// an index that no longer matches its table
				if _, err := db.NewQuery("PRAGMA writable_schema = ON").Execute(); err != nil {
					t.Fatal(err)
				}
				if _, err := db.NewQuery("UPDATE sqlite_master SET sql = replace(sql, '(`name`)', '(`status`)') WHERE name = 'idx_hosts_name'").Execute(); err != nil {
					t.Fatal(err)
				}
			})
			return nil, nil
		}, "The backup's database fails SQLite's integrity check:"},
		{"a pb_data in use", func(t *testing.T, f *recoverFixture) ([]byte, []string) {
			if err := os.MkdirAll(f.dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.dir, "data.db"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			return nil, []string{"data.db"}
		}, "{dir} already holds a panel (it may have started with a CA of its own): run again with --replace to keep it as {dir}.replaced-"},
		{"another key file", func(t *testing.T, f *recoverFixture) ([]byte, []string) {
			if err := secrets.WriteKey(f.keyFile, otherKey); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(f.keyFile)
			return raw, nil
		}, "{key} holds another master key: it opens nothing of this backup. Move it away and run recover again."},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRecoverFixture(t)
			keyBefore, dirBefore := test.prepare(t, f)
			options := f.options(&bytes.Buffer{})
			want := strings.NewReplacer("{dir}", f.dir, "{key}", f.keyFile, "{pin}", f.pin, "{export}", f.export).Replace(test.want)
			if test.name == "a wrong passphrase" {
				options.Passphrase = "not the passphrase of it"
			}
			err := Recover(options)
			if err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("got %v\nwant %s", err, want)
			}
			f.untouched(t, keyBefore, dirBefore)
		})
	}
}

// TestRecoverKeepsTheKitsKeyFile (#32): a key file that holds the kit's key
// already is kept.
func TestRecoverKeepsTheKitsKeyFile(t *testing.T) {
	f := newRecoverFixture(t)
	if err := secrets.WriteKey(f.keyFile, testutil.MasterKey); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Recover(f.options(&out)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "The master key in "+f.keyFile+" is the kit's: kept it.\n") {
		t.Fatalf("the summary: %s", out.String())
	}
}

// TestAKeyFileOnAReadOnlyMountSaysSo: a key file recover cannot write is
// refused with what to do.
func TestAKeyFileOnAReadOnlyMountSaysSo(t *testing.T) {
	err := keyFileError("/run/secrets/key", &fs.PathError{Op: "open", Path: "/run/secrets/key", Err: syscall.EROFS})
	if err == nil || err.Error() != "The master key cannot be written to /run/secrets/key: mount it writable for recover." {
		t.Fatalf("a read-only key file: %v", err)
	}
}
