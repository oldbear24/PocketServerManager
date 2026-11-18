package panel

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/pocketbase/dbx"
	"golang.org/x/term"

	"github.com/oldbear24/PocketServerManager/internal/panelbackup"
	"github.com/oldbear24/PocketServerManager/internal/pki"
	"github.com/oldbear24/PocketServerManager/internal/recoverykit"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/version"
)

// `panel recover` (#32) puts a panel's backup in place on a new machine:
// it opens the recovery kit, checks the backup with the kit's key as it
// decrypts it into a staging directory next to pb_data, checks the
// database, and only then writes the key file and swaps the staging
// directory in. It runs before PocketBase opens anything: every PocketBase
// command bootstraps pb_data, which would make a database where the backup
// goes. See docs/disaster-recovery.md.

// EnvKitPassphrase names the environment variable a kit's passphrase may
// come from, as `docker exec -e` passes it without it showing in the
// container's configuration.
const EnvKitPassphrase = "POCKETCLOUD_KIT_PASSPHRASE"

// RecoverOptions are the flags of `panel recover`.
type RecoverOptions struct {
	// Kit is the recovery kit's file.
	Kit string
	// From is the backup's file, the .pcpb that `pocketcloud-agent
	// panel-recovery fetch` or a download wrote.
	From string
	// Dir is the pb_data to restore.
	Dir string
	// MasterKeyFile is where the kit's master key goes.
	MasterKeyFile string
	// Passphrase opens the kit.
	Passphrase string
	// Replace keeps a pb_data that holds a panel as <dir>.replaced-<UTC>.
	Replace bool
	// EnvOut, when set, is where the kit's environment and the expected
	// CA pin go, as a docker --env-file.
	EnvOut string
	// DryRun checks everything and writes nothing.
	DryRun bool
	// Out takes the summary.
	Out io.Writer
}

// Recover runs `panel recover`. Nothing is written before every check
// passed; a refusal is a sentence.
func Recover(o RecoverOptions) error {
	if o.Kit == "" || o.From == "" {
		return refusal("panel recover needs --kit FILE and --from FILE.pcpb.")
	}
	if o.Dir == "" || o.MasterKeyFile == "" {
		return refusal("panel recover needs --dir and --master-key-file.")
	}
	out := o.Out
	if out == nil {
		out = io.Discard
	}
	dir := filepath.Clean(o.Dir)

	// a panel that serves the directory keeps it
	lock, err := recoverLock(dir)
	if err != nil {
		return err
	}
	if lock != nil {
		defer lock.Close()
	}

	header, kit, key, err := openKit(o.Kit, o.Passphrase)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Opened the recovery kit %s of %s, made %s by %s.\n", header.Kit, header.Panel, utcMinute(header.Created), kit.CreatedBy)

	from, err := os.Open(o.From)
	if err != nil {
		return fmt.Errorf("the backup: %w", err)
	}
	defer from.Close()
	exported, err := panelbackup.ReadHeader(from)
	if err != nil {
		return refusalf("%s is no PocketCloud panel backup: %v.", o.From, err)
	}
	if exported.KeyID != header.KeyID {
		return refusalf("This backup was sealed with another master key (id %s) than the kit's (id %s): use the kit of the panel that made it.", exported.KeyID, header.KeyID)
	}
	if _, err := from.Seek(0, io.SeekStart); err != nil {
		return err
	}

	staging, err := newStaging(dir)
	if err != nil {
		return err
	}
	staged := false
	defer func() {
		if !staged {
			_ = os.RemoveAll(staging)
		}
	}()
	summary, err := panelbackup.Extract(from, key, staging)
	if err != nil {
		var damaged *panelbackup.DamagedError
		if errors.As(err, &damaged) {
			return refusalf("The backup is damaged or incomplete at segment %d.", damaged.Segment)
		}
		return fmt.Errorf("the backup cannot be read: %w", err)
	}
	manifest := summary.Manifest
	if manifest.KeyID != header.KeyID || manifest.CAPin != header.CAPin {
		return refusalf("This backup's CA (pin %s) is not the kit's (pin %s): use the kit of the panel that made it.", manifest.CAPin, header.CAPin)
	}
	if err := checkStagedDatabase(filepath.Join(staging, "data.db"), key, header, manifest); err != nil {
		return err
	}
	fmt.Fprintf(out, "The backup was made %s by panel %s: %s, audit log up to entry %d, CA %s.\n",
		utcMinute(summary.Header.Created), manifest.PanelVersion, servers(manifest.Hosts), manifest.Audit.Seq, shortPin(manifest.CAPin))
	fmt.Fprintln(out, "Checked it: every part opens with the kit's key, data.db passes SQLite's integrity check, and its CA opens with the kit's key.")

	// every refusal before the first write, and before a dry run ends: a
	// dry run that passes is a run that does not refuse for them
	keyThere, err := keyFileHolds(o.MasterKeyFile, key)
	if err != nil {
		return err
	}
	occupied, err := holdsAPanel(dir)
	if err != nil {
		return err
	}
	replaced := dir + ".replaced-" + time.Now().UTC().Format("20060102T150405Z")
	if occupied && !o.Replace {
		return refusalf("%s already holds a panel (it may have started with a CA of its own): run again with --replace to keep it as %s.", dir, replaced)
	}
	if o.EnvOut != "" {
		if _, err := os.Stat(filepath.Dir(o.EnvOut)); err != nil {
			return fmt.Errorf("--env-out: %w", err)
		}
	}

	if o.DryRun {
		fmt.Fprintf(out, "Dry run: nothing was written; %s and %s stay as they are.\n", dir, o.MasterKeyFile)
		return nil
	}

	if keyThere {
		fmt.Fprintf(out, "The master key in %s is the kit's: kept it.\n", o.MasterKeyFile)
	} else {
		if err := secrets.WriteKey(o.MasterKeyFile, key); err != nil {
			return keyFileError(o.MasterKeyFile, err)
		}
		fmt.Fprintf(out, "Wrote the master key to %s.\n", o.MasterKeyFile)
	}

	marker := restoreMarker{
		ExportID: summary.Header.ExportID, ExportCreated: summary.Header.Created.UTC().Format(time.RFC3339),
		PanelVersionExport: manifest.PanelVersion, Migration: manifest.Migration,
		AuditSeq: manifest.Audit.Seq, AuditHash: manifest.Audit.Hash, KitID: header.Kit,
		RecoveredAt: time.Now().UTC().Format(time.RFC3339), ToolVersion: version.Version,
	}
	if err := writeNew(filepath.Join(staging, restoreMarkerName), marker); err != nil {
		return err
	}
	if err := swapIn(staging, dir, occupied, replaced); err != nil {
		return err
	}
	staged = true
	what := ""
	if occupied {
		what = "; what was there is in " + replaced
	}
	fmt.Fprintf(out, "Restored %s: data.db and %s, %s%s.\n", dir, storedFiles(summary.Files), size(summary.Bytes), what)

	if o.EnvOut != "" {
		if err := writeEnv(o.EnvOut, kit.Env, header.CAPin); err != nil {
			return fmt.Errorf("--env-out: %w", err)
		}
	}
	fmt.Fprintln(out, dialSentence(manifest.AgentsDial, o.EnvOut, header.CAPin))
	fmt.Fprintf(out, "Before the first start, run: panel audit verify. Nothing done after %s is in this backup: servers enrolled since must enroll again.\n",
		utcMinute(summary.Header.Created))
	return nil
}

// recoverLock takes the serve lock of a pb_data that exists, so that no
// panel serves it during the recovery; nil when there is nothing to lock.
func recoverLock(dir string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(dir, serveLockName), os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the serve lock of %s: %w", dir, err)
	}
	held, err := tryLock(file)
	if err != nil || !held {
		_ = file.Close()
		if err != nil {
			return nil, fmt.Errorf("the serve lock of %s: %w", dir, err)
		}
		return nil, refusalf("A panel is serving %s: stop it first.", dir)
	}
	return file, nil
}

// openKit reads and opens a recovery kit.
func openKit(path, passphrase string) (recoverykit.Header, recoverykit.Payload, []byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return recoverykit.Header{}, recoverykit.Payload{}, nil, fmt.Errorf("the recovery kit: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, recoverykit.MaxSize+1))
	if err != nil {
		return recoverykit.Header{}, recoverykit.Payload{}, nil, fmt.Errorf("the recovery kit: %w", err)
	}
	header, kit, err := recoverykit.Open(string(raw), passphrase)
	switch {
	case errors.Is(err, recoverykit.ErrPassphrase):
		return header, kit, nil, refusal("The passphrase does not open this kit, or the kit was changed.")
	case errors.Is(err, recoverykit.ErrNotAKit):
		return header, kit, nil, refusal(sentenceText(err.Error()))
	case err != nil:
		return header, kit, nil, err
	}
	key, err := kit.Key()
	return header, kit, key, err
}

// newStaging makes the directory a backup is decrypted into: next to dir,
// on the same filesystem, so that the swap is a rename.
func newStaging(dir string) (string, error) {
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	staging := dir + ".restore-" + hex.EncodeToString(suffix)
	if err := os.Mkdir(staging, 0o700); err != nil {
		return "", fmt.Errorf("the staging directory: %w", err)
	}
	return staging, nil
}

// checkStagedDatabase checks the decrypted data.db, read only: SQLite's
// integrity check, migrations this panel knows, one CA whose key opens with
// the kit's key and whose pin is the backup's, and the kit's key id.
func checkStagedDatabase(path string, key []byte, header recoverykit.Header, manifest panelbackup.Manifest) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	db, err := dbx.Open("sqlite", (&url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro"}).String())
	if err != nil {
		return fmt.Errorf("the backup's database: %w", err)
	}
	defer db.Close()

	var integrity []struct {
		Line string `db:"integrity_check"`
	}
	if err := db.NewQuery("PRAGMA integrity_check").All(&integrity); err != nil {
		return refusalf("The backup's database does not open: %v.", err)
	}
	if len(integrity) == 0 || integrity[0].Line != "ok" {
		first := "no answer"
		if len(integrity) > 0 {
			first = integrity[0].Line
		}
		return refusalf("The backup's database fails SQLite's integrity check: %s.", first)
	}

	unknown, err := unknownMigrations(db)
	if err != nil {
		return fmt.Errorf("the backup's migrations: %w", err)
	}
	if len(unknown) > 0 {
		return refusalf("The backup comes from a newer panel (migration %s): recover with panel %s or newer.", unknown[len(unknown)-1], manifest.PanelVersion)
	}

	var cas []struct {
		RootCert string `db:"root_cert"`
		RootKey  string `db:"root_key"`
	}
	if err := db.Select("root_cert", "root_key").From(schema.InternalCA).All(&cas); err != nil {
		return fmt.Errorf("the backup's CA: %w", err)
	}
	if len(cas) != 1 {
		return refusalf("The backup holds %d internal CAs, not one: it is not a whole panel's database.", len(cas))
	}
	box, err := secrets.New(key)
	if err != nil {
		return err
	}
	if _, err := box.Open(cas[0].RootKey); err != nil {
		return refusal("The backup's CA does not open with the kit's key: the kit is not of the panel that made it.")
	}
	block, _ := pem.Decode([]byte(cas[0].RootCert))
	if block == nil || pki.PinOf(block.Bytes) != manifest.CAPin {
		return refusalf("The backup's CA is not the one its manifest names (pin %s): the backup is not whole.", manifest.CAPin)
	}

	var state panelState
	err = db.Select("key_id", "ca_pin", "restored_at", "restored_from").From(schema.PanelTable).Where(dbx.HashExp{"id": 1}).One(&state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("the backup's %s: %w", schema.PanelTable, err)
	}
	if state.KeyID != header.KeyID {
		return refusalf("The backup's database was sealed with another master key (id %s) than the kit's (id %s): use the kit of the panel that made it.", state.KeyID, header.KeyID)
	}
	return nil
}

// keyFileHolds says whether the key file holds the key already, and
// refuses one that holds another key.
func keyFileHolds(path string, key []byte) (bool, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("the master key file: %w", err)
	}
	existing, err := secrets.LoadKeyFile(path)
	if err != nil || subtle.ConstantTimeCompare(existing, key) != 1 {
		return false, refusalf("%s holds another master key: it opens nothing of this backup. Move it away and run recover again.", path)
	}
	return true, nil
}

// keyFileError says why the key could not be written.
func keyFileError(path string, err error) error {
	switch {
	case errors.Is(err, secrets.ErrKeyExists):
		return refusalf("%s holds another master key: it opens nothing of this backup. Move it away and run recover again.", path)
	case errors.Is(err, syscall.EROFS), errors.Is(err, fs.ErrPermission):
		return refusalf("The master key cannot be written to %s: mount it writable for recover.", path)
	}
	return fmt.Errorf("the master key file: %w", err)
}

// holdsAPanel says whether a pb_data exists with anything in it but its
// serve lock.
func holdsAPanel(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(entries, func(entry os.DirEntry) bool { return entry.Name() != serveLockName }), nil
}

// swapIn puts the staging directory in dir's place, keeping what was there
// as replaced; nothing is deleted but an empty pb_data.
func swapIn(staging, dir string, occupied bool, replaced string) error {
	if occupied {
		if err := os.Rename(dir, replaced); err != nil {
			return fmt.Errorf("keeping %s as %s: %w", dir, replaced, err)
		}
	} else if _, err := os.Stat(dir); err == nil {
		// empty but its serve lock
		_ = os.Remove(filepath.Join(dir, serveLockName))
		if err := os.Remove(dir); err != nil {
			return fmt.Errorf("replacing the empty %s: %w", dir, err)
		}
	}
	if err := os.Rename(staging, dir); err != nil {
		return fmt.Errorf("putting the backup in place as %s: %w", dir, err)
	}
	return nil
}

// writeNew writes a value as JSON to a new file of mode 0600.
func writeNew(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// writeEnv writes the kit's environment and the expected CA pin as a docker
// --env-file: NAME=value lines, in order.
func writeEnv(path string, env map[string]string, pin string) error {
	var out strings.Builder
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		value := env[name]
		if name == EnvExpectCAPin || strings.ContainsAny(name+value, "\n\r\x00") {
			continue
		}
		out.WriteString(name + "=" + value + "\n")
	}
	out.WriteString(EnvExpectCAPin + "=" + pin + "\n")
	// no secret: readable by whoever runs docker --env-file with it, as root
	// wrote it in recover's container
	return os.WriteFile(path, []byte(out.String()), 0o644) //nolint:gosec // it holds no secret
}

// dialSentence says where the servers dial, and what that asks of this
// machine.
func dialSentence(dials []panelbackup.Dial, envOut, pin string) string {
	start := "Start the panel there with " + envOut + "."
	if envOut == "" {
		start = "Start the panel there with the kit's environment and " + EnvExpectCAPin + "=" + pin + " (--env-out writes them)."
	}
	if len(dials) == 0 {
		return "No server had said which address it dials: give this machine the panel's old address. " + start
	}
	addresses := make([]string, 0, len(dials))
	ip := false
	for _, dial := range dials {
		addresses = append(addresses, dial.Address)
		if host, _, err := net.SplitHostPort(dial.Address); err == nil && net.ParseIP(host) != nil {
			ip = true
		}
	}
	what := "a DNS name: point it at this machine"
	if ip {
		what = "an IP address: this machine must take it over"
	}
	if len(addresses) > 1 {
		what = "addresses this machine must answer at; an IP address it must take over"
	}
	return fmt.Sprintf("Servers dial %s (%s). %s", strings.Join(addresses, ", "), what, start)
}

// servers counts servers in words.
func servers(n int) string {
	if n == 1 {
		return "1 server"
	}
	return fmt.Sprintf("%d servers", n)
}

// storedFiles counts stored files in words.
func storedFiles(n int) string {
	if n == 1 {
		return "1 stored file"
	}
	return fmt.Sprintf("%d stored files", n)
}

// shortPin is a CA pin as people compare it at a glance.
func shortPin(pin string) string {
	if len(pin) > len("sha256:")+4 {
		return pin[:len("sha256:")+4] + "…"
	}
	return pin
}

// size is a size in bytes as people read it.
func size(n int64) string {
	if n < 1_000_000 {
		return fmt.Sprintf("%d kB", (n+999)/1000)
	}
	return fmt.Sprintf("%d MB", (n+500_000)/1_000_000)
}

// ReadPassphrase gets a kit's passphrase: from a file, from
// POCKETCLOUD_KIT_PASSPHRASE, or typed at a terminal without echo.
func ReadPassphrase(file string, stdin *os.File, prompt io.Writer) (string, error) {
	if file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("--passphrase-file: %w", err)
		}
		return strings.TrimRight(string(raw), "\r\n"), nil
	}
	if passphrase := os.Getenv(EnvKitPassphrase); passphrase != "" {
		return passphrase, nil
	}
	fd := int(stdin.Fd()) //nolint:gosec // a descriptor fits an int
	if !term.IsTerminal(fd) {
		return "", refusalf("No passphrase: give --passphrase-file, set %s, or run it at a terminal.", EnvKitPassphrase)
	}
	fmt.Fprint(prompt, "Passphrase of the recovery kit: ")
	raw, err := term.ReadPassword(fd)
	fmt.Fprintln(prompt)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
