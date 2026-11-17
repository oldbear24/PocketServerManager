package agent

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/panelbackup"
	"github.com/oldbear24/PocketServerManager/internal/recoverykit"
	"github.com/oldbear24/PocketServerManager/internal/secrets"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakefetch"
)

// The panel's recovery (#32): panel-recovery against a fake engine whose
// jobs run the backup tool and the fake restic, from repositories the
// panel's own backup runs filled.

const (
	recoveryJob        = "p4n3lj0b0000001"
	recoveryPassphrase = "correct horse battery staple"
	recoveryPin        = "sha256:5d1e5d1e5d1e5d1e5d1e5d1e5d1e5d1e5d1e5d1e5d1e5d1e5d1e5d1e5d1e5d1e"
)

// masterOf is a master key whose bytes start at b and count up.
func masterOf(b byte) []byte {
	key := make([]byte, secrets.KeySize)
	for i := range key {
		key[i] = b + byte(i)
	}
	return key
}

// exportOf is a copy of the panel's sealed with master: a database and one
// stored file, made at made, of a panel whose CA is pin.
func exportOf(t *testing.T, master []byte, made time.Time, pin, db string) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := panelbackup.NewWriter(&out, master, made)
	if err != nil {
		t.Fatal(err)
	}
	file := []byte("a stored file")
	m := panelbackup.Manifest{PanelVersion: "0.32.0", Migration: "1772000000_panel_recovery.go", CAPin: pin,
		Audit: panelbackup.AuditHead{Seq: 3812, Hash: strings.Repeat("ab", 32)}, Hosts: 3, Job: recoveryJob, Run: "k3j9x2m4q8w1z7v",
		Storage: panelbackup.StorageLocal, Files: 1, Bytes: int64(len(db) + len(file)), LeftOut: panelbackup.LeftOut}
	if err := w.Manifest(m); err != nil {
		t.Fatal(err)
	}
	if err := w.Database(strings.NewReader(db), int64(len(db))); err != nil {
		t.Fatal(err)
	}
	if err := w.File("storage/pbc_2084539284/abcdefghijklmno/report.pdf", bytes.NewReader(file), int64(len(file))); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// recoveryHarness is a server whose agent carried the panel's backup to a
// remote target and to a directory of its own, and a kit of that panel.
type recoveryHarness struct {
	*runAgent
	remote, local *agentv1.BackupRepository
	fetch         *fakefetch.Server
	master        []byte
	kit           string
	dir           string
}

func newRecoveryHarness(t *testing.T) *recoveryHarness {
	t.Helper()
	r := newRunAgent(t)
	h := &recoveryHarness{runAgent: r, master: masterOf(7), dir: t.TempDir()}
	h.remote = r.remote(t)
	h.local = &agentv1.BackupRepository{TargetId: "tgtdisk00000002", Url: backup.LocalMount, Password: "repo-password-disk", Mount: filepath.Join(t.TempDir(), "pc-panel32")}
	if _, err := r.handleBackupInit(context.Background(), initCommand(h.local), &Reporter{}); err != nil {
		t.Fatal(err)
	}
	var dial func(context.Context) (agentv1.AgentServiceClient, func(), error)
	h.fetch, dial = fakefetch.NewServer(t)
	r.LiveUpload = dial
	h.kit = h.sealKit(t, h.master, recoveryPin)
	return h
}

// sealKit seals a kit of master whose panel's CA is pin, with the
// harness's two targets.
func (h *recoveryHarness) sealKit(t *testing.T, master []byte, pin string) string {
	t.Helper()
	header := recoverykit.Header{Kit: "KIT-7Q2M-9XHC", Created: time.Date(2026, 11, 17, 10, 2, 0, 0, time.UTC), Panel: "http://192.0.2.2:8090",
		KeyID: secrets.KeyID(master), CAPin: pin, PanelVersion: "0.32.0", AgentImage: "ghcr.io/oldbear24/pocketcloud-agent:0.32.0"}
	repository := func(repo *agentv1.BackupRepository) recoverykit.Repository {
		return recoverykit.Repository{URL: repo.GetUrl(), Password: repo.GetPassword(), Env: repo.GetEnv(), Options: repo.GetOptions(),
			SSHKey: repo.GetSshKey(), KnownHosts: repo.GetKnownHosts(), Mount: repo.GetMount()}
	}
	payload := recoverykit.Payload{
		CreatedBy: "admin@example.com", MasterKey: hex.EncodeToString(master), KeyID: secrets.KeyID(master),
		Env:        map[string]string{"POCKETCLOUD_PUBLIC_URL": "http://192.0.2.2:8090"},
		AgentsDial: []recoverykit.Dial{{Address: "192.0.2.2:9443", Servers: 2, IP: true}},
		Job:        &recoverykit.Job{ID: recoveryJob, Name: "panel (pb_data)"},
		Targets: []recoverykit.Target{
			{ID: "tgtdisk00000002", Name: "panel-disk", Kind: "local", Server: "vm", Repository: repository(h.local)},
			{ID: h.remote.GetTargetId(), Name: "panel-minio", Kind: "s3", Repository: repository(h.remote)},
		},
	}
	text, err := recoverykit.Seal(header, payload, recoveryPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// store backs a copy up to both targets, as the panel's job runs do, and
// returns the copy.
func (h *recoveryHarness) store(t *testing.T, export []byte) []byte {
	t.Helper()
	h.fetch.Offer(testExport, fakefetch.Artifact{Data: export})
	for _, repo := range []*agentv1.BackupRepository{h.remote, h.local} {
		run := panelRun(repo)
		run.Tags = []string{"pocketcloud", "job:" + recoveryJob, backup.PanelTag}
		if result, err := h.handleBackupRun(context.Background(), commandOf("k3j9x2m4q8w1z7v", run), &Reporter{}); err != nil {
			t.Fatalf("%v: %v", err, result)
		}
	}
	return export
}

// recovered is what a panel-recovery printed.
type recovered struct {
	stdout, stderr string
	err            error
}

// recover runs panel-recovery with args, the kit's file and its
// passphrase from the environment.
func (h *recoveryHarness) recover(t *testing.T, args ...string) recovered {
	t.Helper()
	kit := filepath.Join(h.dir, "kit.pckit")
	if err := os.WriteFile(kit, []byte(h.kit), 0o600); err != nil {
		t.Fatal(err)
	}
	args = append(slices.Clone(args), "--kit", kit)
	return h.run(t, args, nil, map[string]string{envKitPassphrase: recoveryPassphrase})
}

func (h *recoveryHarness) run(t *testing.T, args []string, stdin io.Reader, env map[string]string) recovered {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := h.PanelRecovery(context.Background(), args, RecoveryIO{Stdin: stdin, Stdout: &stdout, Stderr: &stderr, Getenv: func(name string) string { return env[name] }})
	return recovered{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

// recoveryJobs are the jobs panel-recovery ran, after the first n jobs.
func (h *recoveryHarness) jobsSince(n int) []fakedocker.Job {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.jobs[n:])
}

// TestRecoveryFetchesTheNewestCopyCheckedWithTheKitsKey (#32): fetch takes
// the target's newest panel snapshot, or the one named, writes its copy
// whole to a new file of mode 0600 and checks it with the kit's key as it
// comes; what it says names the kit, the target, the snapshot, the copy's
// size, digest and panel.
func TestRecoveryFetchesTheNewestCopyCheckedWithTheKitsKey(t *testing.T) {
	h := newRecoveryHarness(t)
	first := h.store(t, exportOf(t, h.master, time.Date(2026, 11, 16, 4, 0, 0, 0, time.UTC), recoveryPin, "the database of Monday"))
	newest := h.store(t, exportOf(t, h.master, time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC), recoveryPin, "the database of Tuesday"))
	out := filepath.Join(h.dir, "panel.pcpb")
	got := h.recover(t, "fetch", "--out", out)
	if got.err != nil {
		t.Fatalf("%v\n%s", got.err, got.stderr)
	}
	written, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(written, newest) {
		t.Fatalf("the copy: %d bytes, %v", len(written), err)
	}
	if info, _ := os.Stat(out); info.Mode().Perm() != 0o600 {
		t.Errorf("the copy's mode: %v", info.Mode())
	}
	lines := strings.Split(strings.TrimSpace(got.stderr), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "Kit KIT-7Q2M-9XHC of http://192.0.2.2:8090 (key "+secrets.KeyID(h.master)[:8]+"…). Target panel-minio (s3): newest panel snapshot ") ||
		!strings.HasSuffix(lines[0], " UTC.") {
		t.Errorf("the first line:\n%s", got.stderr)
	}
	if last := lines[len(lines)-1]; last != "Fetched it: "+sizeText(int64(len(newest)))+", sha256 "+sha256Hex(newest)+
		"; every part opens with the kit's key (export 2026-11-17 04:00 UTC, panel 0.32.0, 3 servers)." {
		t.Errorf("the last line: %s", last)
	}
	if got.stdout != "" {
		t.Errorf("stdout: %q", got.stdout)
	}

	// the older one, by the first 8 characters of its id, to the standard
	// output
	listed := h.recover(t, "snapshots")
	ids := strings.Fields(listed.stdout)
	if listed.err != nil || len(ids) < 8 {
		t.Fatalf("%v: %q\n%s", listed.err, listed.stdout, listed.stderr)
	}
	older := strings.Split(strings.TrimSpace(listed.stdout), "\n")[1][:8]
	got = h.recover(t, "fetch", "--snapshot", older, "--out", "-")
	if got.err != nil || got.stdout != string(first) || !strings.Contains(got.stderr, "Target panel-minio (s3): panel snapshot "+older+" from ") ||
		!strings.Contains(got.stderr, "(export 2026-11-16 04:00 UTC, panel 0.32.0, 3 servers)") {
		t.Errorf("the older copy: %d bytes, %v\n%s", len(got.stdout), got.err, got.stderr)
	}
	for _, bad := range []string{"0123", "zzzzzzzz", "00000000"} {
		if got := h.recover(t, "fetch", "--snapshot", bad, "--out", "-"); got.err == nil || got.stdout != "" {
			t.Errorf("the snapshot %s: %v", bad, got.err)
		}
	}

	// a file that is there is never written over
	got = h.recover(t, "fetch", "--out", out)
	if got.err == nil || !strings.Contains(got.err.Error(), "file exists") {
		t.Errorf("over a file: %v", got.err)
	}
	if written, _ := os.ReadFile(out); !bytes.Equal(written, newest) {
		t.Error("the file was changed")
	}
}

// TestRecoveryRunsResticInTheHardenedJob (#32): restic runs only in the
// agent's own job container: no capability, no new privileges, no cache,
// the hostname of a command of its own, no lock; a local target's directory
// bound read-only, with DAC_OVERRIDE alone and no network. A local target
// of another server is fetched with a warning.
func TestRecoveryRunsResticInTheHardenedJob(t *testing.T) {
	h := newRecoveryHarness(t)
	export := h.store(t, exportOf(t, h.master, time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC), recoveryPin, "data"))
	for _, target := range []string{"panel-minio", "panel-disk"} {
		before := h.jobCount()
		got := h.recover(t, "fetch", "--target", target, "--out", "-")
		if got.err != nil || got.stdout != string(export) {
			t.Fatalf("%s: %v\n%s", target, got.err, got.stderr)
		}
		jobs := h.jobsSince(before)
		if len(jobs) != 2 {
			t.Fatalf("%s: %d jobs", target, len(jobs))
		}
		if !slices.Equal(jobs[0].Cmd[:7], []string{"backup-tool", "snapshots", "--tag", "pocketcloud", "--tag", "job:" + recoveryJob, "--limit"}) ||
			!slices.Equal(jobs[1].Cmd[:9], []string{"backup-tool", "dump", "--no-lock", "--snapshot", jobs[1].Cmd[4], "--path", "/pocketcloud-panel.pcpb", "--expect-tag", "job:" + recoveryJob}) ||
			!snapshotID.MatchString(jobs[1].Cmd[4]) {
			t.Errorf("%s: the jobs ran %q and %q", target, jobs[0].Cmd, jobs[1].Cmd)
		}
		hosts := map[string]bool{}
		for _, job := range jobs {
			if slices.Contains(job.Cmd, "--cache") || !slices.Equal(anyStrings(job.Host["CapDrop"]), []string{"ALL"}) ||
				!slices.Equal(anyStrings(job.Host["SecurityOpt"]), []string{"no-new-privileges"}) || backup.CommandOfHost(job.Hostname) == "" ||
				job.Labels["pocketcloud.role"] != "job" || job.Labels["pocketcloud.command"] != strings.TrimPrefix(job.Hostname, "pc-") {
				t.Errorf("%s: the job %q: %v, %v, hostname %q", target, job.Cmd[1], job.Host, job.Labels, job.Hostname)
			}
			hosts[job.Hostname] = true
			mounts := mountsOf(job)
			if mounts[resticCacheDir] != nil {
				t.Errorf("%s: the job mounts the cache", target)
			}
			if target == "panel-minio" {
				if len(mounts) != 1 || job.Host["NetworkMode"] != "bridge" || len(anyStrings(job.Host["CapAdd"])) != 0 {
					t.Errorf("%s: %v", target, job.Host)
				}
				continue
			}
			repo := mounts[backup.LocalMount]
			if len(mounts) != 2 || repo["ReadOnly"] != true || repo["Source"] != h.local.GetMount() || job.Host["NetworkMode"] != "none" ||
				!slices.Equal(anyStrings(job.Host["CapAdd"]), []string{"DAC_OVERRIDE"}) {
				t.Errorf("%s: %v", target, job.Host)
			}
		}
		if len(hosts) != 2 {
			t.Errorf("%s: the jobs share a hostname: %v", target, hosts)
		}
		warned := strings.Contains(got.stderr, "Warning: the target panel-disk is a directory of vm: run this there.\n")
		if warned != (target == "panel-disk") {
			t.Errorf("%s: the warning:\n%s", target, got.stderr)
		}
	}
	// on vm itself, no warning
	state := t.TempDir()
	writeIdentityName(t, state, "vm")
	h.Config.StateDir = state
	if got := h.recover(t, "fetch", "--target", "panel-disk", "--out", "-"); got.err != nil || strings.Contains(got.stderr, "Warning") {
		t.Errorf("on vm: %v\n%s", got.err, got.stderr)
	}
}

// writeIdentityName writes an identity of the server name, as the agent's
// enrollment leaves it.
func writeIdentityName(t *testing.T, state, name string) {
	t.Helper()
	dir := filepath.Join(state, identityDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	identity := &Identity{Panel: "192.0.2.2:9443", HostID: "h1", Name: name, dir: dir}
	key, cert, ca := testIdentityPEM(t)
	if err := identity.save(key, cert, ca); err != nil {
		t.Fatal(err)
	}
}

// TestRecoveryRefusesACopyOfAnotherPanel (#32): a copy sealed with another
// master key, or of a panel with another CA, does not open with the kit,
// and leaves no file; --no-verify writes it unchecked, for a look at a
// damaged copy.
func TestRecoveryRefusesACopyOfAnotherPanel(t *testing.T) {
	h := newRecoveryHarness(t)
	// a large one, which the check stops at its header: the job is stopped
	// as a download whose browser left
	noise := make([]byte, 3<<20)
	_, _ = io.ReadFull(mrand.NewChaCha8([32]byte{1}), noise)
	other := h.store(t, exportOf(t, masterOf(100), time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC), recoveryPin, string(noise)))
	out := filepath.Join(h.dir, "panel.pcpb")
	got := h.recover(t, "fetch", "--out", out)
	if got.err == nil || got.err.Error() != "the copy does not open with the kit's key: it belongs to another panel" {
		t.Errorf("another key: %v\n%s", got.err, got.stderr)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the copy is left: %v", err)
	}
	stopped := false
	for _, call := range h.engine.Calls() {
		stopped = stopped || strings.HasSuffix(call, "/stop?t=30")
	}
	if !stopped {
		t.Error("the dump went on after the copy did not open")
	}
	got = h.recover(t, "fetch", "--no-verify", "--out", out)
	if written, _ := os.ReadFile(out); got.err != nil || !bytes.Equal(written, other) ||
		!strings.HasSuffix(got.stderr, "Fetched it: "+sizeText(int64(len(other)))+", sha256 "+sha256Hex(other)+"; not checked with the kit's key (--no-verify).\n") {
		t.Errorf("--no-verify: %v\n%s", got.err, got.stderr)
	}
	_ = os.Remove(out)

	// another CA, the same key
	h.store(t, exportOf(t, h.master, time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC), "sha256:"+strings.Repeat("9a", 32), "a panel that made a CA anew"))
	if got := h.recover(t, "fetch", "--out", out); got.err == nil || !strings.Contains(got.err.Error(), "not the kit's "+recoveryPin+": it belongs to another panel") {
		t.Errorf("another CA: %v", got.err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the copy is left: %v", err)
	}

	// a damaged copy, which stops as it comes
	damaged := exportOf(t, h.master, time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC), recoveryPin, strings.Repeat("rows ", 50000))
	damaged[len(damaged)/2] ^= 1
	h.store(t, damaged)
	if got := h.recover(t, "fetch", "--out", out); got.err == nil || !strings.HasPrefix(got.err.Error(), "the copy does not open with the kit's key: the panel backup is damaged") {
		t.Errorf("a damaged copy: %v", got.err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the copy is left: %v", err)
	}
}

// TestRecoveryOpensTheKitWithItsPassphrase (#32): the passphrase comes from
// --passphrase-file, the environment or the terminal; a wrong one opens
// nothing and runs no job, as does a file that is no kit, and a kit read
// from the standard input asks no terminal.
func TestRecoveryOpensTheKitWithItsPassphrase(t *testing.T) {
	h := newRecoveryHarness(t)
	h.store(t, exportOf(t, h.master, time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC), recoveryPin, "data"))
	kit := filepath.Join(h.dir, "kit.pckit")
	if err := os.WriteFile(kit, []byte(h.kit), 0o600); err != nil {
		t.Fatal(err)
	}
	pass := filepath.Join(h.dir, "pass")
	if err := os.WriteFile(pass, []byte(recoveryPassphrase+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs := h.jobCount()
	for _, c := range []struct {
		name  string
		args  []string
		stdin string
		env   map[string]string
		want  string
	}{
		{name: "a wrong passphrase", args: []string{"targets", "--kit", kit}, env: map[string]string{envKitPassphrase: "a wrong passphrase"},
			want: "the passphrase does not open this kit, or the kit was changed"},
		{name: "no passphrase", args: []string{"targets", "--kit", kit},
			want: "the kit's passphrase comes from --passphrase-file, POCKETCLOUD_KIT_PASSPHRASE or a terminal, and none is there"},
		{name: "a file that is no kit", args: []string{"targets", "--kit", pass}, env: map[string]string{envKitPassphrase: recoveryPassphrase},
			want: "this is no PocketCloud recovery kit"},
		{name: "a kit too large", args: []string{"targets", "--kit", "-"}, stdin: strings.Repeat("x", recoverykit.MaxSize+1), env: map[string]string{envKitPassphrase: recoveryPassphrase},
			want: "this is no PocketCloud recovery kit: it is larger than 64 KiB"},
		{name: "a kit that is not there", args: []string{"targets", "--kit", filepath.Join(h.dir, "gone")}, want: "the kit cannot be read"},
		{name: "no kit", args: []string{"targets"}, want: "name the recovery kit"},
		{name: "no op", args: []string{"--kit", kit}, want: "name an op"},
		{name: "another op", args: []string{"restore", "--kit", kit}, want: `unknown op "restore"`},
		{name: "fetch's flags", args: []string{"snapshots", "--kit", kit, "--out", "x"}, want: "--snapshot, --out and --no-verify are fetch's, not snapshots's"},
		{name: "a target that is not there", args: []string{"fetch", "--kit", kit, "--target", "nas"}, env: map[string]string{envKitPassphrase: recoveryPassphrase},
			want: "the kit has no target nas: it has panel-disk, panel-minio"},
	} {
		got := h.run(t, c.args, strings.NewReader(c.stdin), c.env)
		if got.err == nil || !strings.HasPrefix(got.err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, got.err)
		}
	}
	if h.jobCount() != jobs {
		t.Errorf("%d jobs ran", h.jobCount()-jobs)
	}

	// each source of the passphrase
	asked := 0
	prompt := func() (string, error) {
		asked++
		return recoveryPassphrase, nil
	}
	var stdout, stderr bytes.Buffer
	for _, c := range []struct {
		name  string
		args  []string
		stdin io.Reader
		env   map[string]string
	}{
		{name: "a file", args: []string{"targets", "--kit", kit, "--passphrase-file", pass}},
		{name: "the environment", args: []string{"targets", "--kit", kit}, env: map[string]string{envKitPassphrase: recoveryPassphrase}},
		{name: "the standard input", args: []string{"targets", "--kit", "-"}, stdin: strings.NewReader(h.kit), env: map[string]string{envKitPassphrase: recoveryPassphrase}},
		{name: "the terminal", args: []string{"targets", "--kit", kit}},
	} {
		stdout.Reset()
		err := h.PanelRecovery(context.Background(), c.args, RecoveryIO{Stdin: c.stdin, Stdout: &stdout, Stderr: &stderr, Prompt: prompt,
			Getenv: func(name string) string { return c.env[name] }})
		if err != nil || !strings.Contains(stdout.String(), "* panel-minio (s3): s3:http://minio:9000/backups\n") ||
			!strings.Contains(stdout.String(), "  panel-disk (local): "+h.local.GetMount()+" on vm\n") {
			t.Errorf("%s: %v\n%s", c.name, err, stdout.String())
		}
	}
	if asked != 1 {
		t.Errorf("the terminal was asked %d times", asked)
	}
	// a kit on the standard input asks no terminal
	if err := h.PanelRecovery(context.Background(), []string{"targets", "--kit", "-"}, RecoveryIO{Stdin: strings.NewReader(h.kit), Stdout: io.Discard, Stderr: io.Discard,
		Prompt: prompt, Getenv: func(string) string { return "" }}); err == nil || asked != 1 {
		t.Errorf("a kit on the standard input: %v, asked %d times", err, asked)
	}
	// no terminal
	noTerminal := func() (string, error) { return "", ErrNoTerminal }
	if err := h.PanelRecovery(context.Background(), []string{"targets", "--kit", kit}, RecoveryIO{Stdout: io.Discard, Stderr: io.Discard, Prompt: noTerminal,
		Getenv: func(string) string { return "" }}); err == nil || !strings.Contains(err.Error(), "and none is there") {
		t.Errorf("no terminal: %v", err)
	}
}

// TestRecoverySaysWhenThereIsNothingToFetch (#32): a target without a panel
// snapshot of the kit's job says so, and a kit made while the panel had no
// backup job holds no target.
func TestRecoverySaysWhenThereIsNothingToFetch(t *testing.T) {
	h := newRecoveryHarness(t)
	if got := h.recover(t, "fetch", "--out", "-"); got.err == nil || got.err.Error() != "no panel snapshot of job:"+recoveryJob+" on panel-minio" {
		t.Errorf("no snapshot: %v\n%s", got.err, got.stderr)
	}
	header := recoverykit.Header{Kit: "KIT-7Q2M-9XHD", Created: time.Date(2026, 11, 17, 10, 2, 0, 0, time.UTC), Panel: "http://192.0.2.2:8090",
		KeyID: secrets.KeyID(h.master), CAPin: recoveryPin, PanelVersion: "0.32.0", AgentImage: "agent"}
	text, err := recoverykit.Seal(header, recoverykit.Payload{CreatedBy: "admin@example.com", MasterKey: hex.EncodeToString(h.master), KeyID: secrets.KeyID(h.master),
		Env: map[string]string{}, AgentsDial: []recoverykit.Dial{}, Targets: []recoverykit.Target{}}, recoveryPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	h.kit = text
	for _, op := range []string{"targets", "snapshots", "fetch"} {
		if got := h.recover(t, op); got.err == nil || !strings.HasPrefix(got.err.Error(), "the kit holds no backup job of the panel: it holds the master key alone") {
			t.Errorf("%s: %v", op, got.err)
		}
	}
}

// TestRecoveryKeepsItsSecrets (#32): neither the kit's passphrase, nor the
// master key, nor a repository's password or credentials reach what
// panel-recovery says, restic's own lines included.
func TestRecoveryKeepsItsSecrets(t *testing.T) {
	h := newRecoveryHarness(t)
	h.store(t, exportOf(t, h.master, time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC), recoveryPin, "data"))
	said := h.recover(t, "fetch", "--out", filepath.Join(h.dir, "a.pcpb")).stderr
	said += h.recover(t, "snapshots").stderr + h.recover(t, "targets").stdout
	// storage that refuses the credentials, which the fake restic repeats
	h.remote.Env = map[string]string{"AWS_ACCESS_KEY_ID": "minio-key", "AWS_SECRET_ACCESS_KEY": "wrong-minio-secret"}
	h.kit = h.sealKit(t, h.master, recoveryPin)
	refused := h.recover(t, "fetch", "--out", filepath.Join(h.dir, "b.pcpb"))
	if refused.err == nil || !strings.Contains(refused.stderr+refused.err.Error(), "Access Denied for key ***") {
		t.Errorf("refused: %v\n%s", refused.err, refused.stderr)
	}
	said += refused.stderr + refused.err.Error()
	for _, secret := range []string{recoveryPassphrase, hex.EncodeToString(h.master), "repo-password-1", "repo-password-disk", "minio-secret", "minio-key"} {
		if strings.Contains(said, secret) {
			t.Errorf("%q was said:\n%s", secret, said)
		}
	}
}

// TestRecoveryOpensNoCommandStore (#32): NewRecovery makes no state
// directory and opens no command store, so that it runs beside an agent
// that serves; it runs its jobs on the Engine all the same.
func TestRecoveryOpensNoCommandStore(t *testing.T) {
	h := newRecoveryHarness(t)
	export := h.store(t, exportOf(t, h.master, time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC), recoveryPin, "data"))
	state := filepath.Join(t.TempDir(), "state")
	var log bytes.Buffer
	a, err := NewRecovery(Config{StateDir: state, DockerHost: h.engine.Host(), ContainerName: "pocketcloud-agent"}, &log)
	if err != nil {
		t.Fatal(err)
	}
	if a.store != nil || a.Executor != nil || a.Identity != nil {
		t.Errorf("the agent: %+v", a)
	}
	kit := filepath.Join(h.dir, "kit.pckit")
	if err := os.WriteFile(kit, []byte(h.kit), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = a.PanelRecovery(context.Background(), []string{"fetch", "--kit", kit, "--out", "-"}, RecoveryIO{Stdout: &stdout, Stderr: &stderr,
		Getenv: func(name string) string { return map[string]string{envKitPassphrase: recoveryPassphrase}[name] }})
	if err != nil || stdout.String() != string(export) {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the state directory: %v", err)
	}
	// no job is left behind
	containers, err := h.Docker.Containers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, container := range containers {
		if container.Labels["pocketcloud.role"] == "job" {
			t.Errorf("a job is left: %v", container.Labels)
		}
	}
}
