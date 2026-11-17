package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
	"github.com/oldbear24/PocketServerManager/internal/panelbackup"
	"github.com/oldbear24/PocketServerManager/internal/recoverykit"
)

// The panel's recovery (#32): `pocketcloud-agent panel-recovery
// targets|snapshots|fetch` takes the panel's copy of itself out of a target
// of the panel's backup job, with what the recovery kit holds, onto a new
// machine, where `panel recover` puts it in place. restic runs only in the
// agent's own hardened job container, as for any backup command: on a
// server that survived, through docker exec beside its agent, or from a
// launcher with the Docker socket on the new machine. The kit names each
// target's repository exactly as the panel's enricher delivers it; the job
// dumps the snapshot's one file with no lock, a local target's directory
// bound read-only with no network, and the agent checks the copy with the
// kit's key as it writes it. Nothing of the agent's state is touched: no
// state directory is made and no command store opened.

const (
	// envKitPassphrase is the kit's passphrase, for docker exec -e, which
	// keeps it out of what docker inspect shows of the container.
	envKitPassphrase = "POCKETCLOUD_KIT_PASSPHRASE"
	// recoverySnapshots is how many snapshots `snapshots` lists.
	recoverySnapshots = 20
	// maxPassphraseFile is the most a passphrase file may hold.
	maxPassphraseFile = 4 << 10
)

// ErrNoTerminal is a passphrase prompt with no terminal to ask on.
var ErrNoTerminal = errors.New("no terminal")

// RecoveryIO is what panel-recovery reads and writes.
type RecoveryIO struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Getenv reads the environment, POCKETCLOUD_KIT_PASSPHRASE.
	Getenv func(string) string
	// Prompt asks for the kit's passphrase without echo, on the terminal
	// of the standard input; ErrNoTerminal when there is none. Nil asks
	// nobody.
	Prompt func() (string, error)
}

// NewRecovery prepares an agent for panel-recovery: its configuration, the
// Docker Engine and a log on stderr, nothing else. Unlike New it neither
// makes the state directory nor opens the command store, so that it runs
// beside an agent that serves.
func NewRecovery(config Config, stderr io.Writer) (*Agent, error) {
	docker, err := dockerapi.New(config.DockerHost)
	if err != nil {
		return nil, err
	}
	return &Agent{Config: config, Docker: docker, Log: slog.New(slog.NewTextHandler(stderr, nil))}, nil
}

// recoveryOptions are panel-recovery's flags.
type recoveryOptions struct {
	op, kit, passphraseFile, target, snapshot, out string
	noVerify                                       bool
}

func recoveryOptionsOf(args []string) (recoveryOptions, error) {
	var o recoveryOptions
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return o, errors.New("name an op: targets, snapshots or fetch")
	}
	o.op = args[0]
	switch o.op {
	case "targets", "snapshots", "fetch":
	default:
		return o, fmt.Errorf("unknown op %q: use targets, snapshots or fetch", o.op)
	}
	flags := flag.NewFlagSet("panel-recovery "+o.op, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&o.kit, "kit", "", "the recovery kit's file, or - for the standard input")
	flags.StringVar(&o.passphraseFile, "passphrase-file", "", "a file that holds the kit's passphrase")
	flags.StringVar(&o.target, "target", "", "the name of the target to take the copy from")
	snapshot := flags.String("snapshot", "", "fetch: the snapshot, by its id, its first 8 characters, or latest")
	out := flags.String("out", "", "fetch: where the copy goes, a new file, or - for the standard output")
	noVerify := flags.Bool("no-verify", false, "fetch: write the copy without checking it with the kit's key")
	if err := flags.Parse(args[1:]); err != nil {
		return o, err
	}
	if flags.NArg() > 0 {
		return o, fmt.Errorf("panel-recovery %s takes flags only, not %q", o.op, flags.Arg(0))
	}
	if o.kit == "" {
		return o, errors.New("name the recovery kit: --kit FILE, or --kit - for the standard input")
	}
	if o.op != "fetch" && (*snapshot != "" || *out != "" || *noVerify) {
		return o, fmt.Errorf("--snapshot, --out and --no-verify are fetch's, not %s's", o.op)
	}
	o.snapshot, o.out, o.noVerify = *snapshot, *out, *noVerify
	if o.snapshot == "" {
		o.snapshot = "latest"
	}
	if o.out == "" {
		o.out = "-"
	}
	return o, nil
}

// PanelRecovery runs `pocketcloud-agent panel-recovery` with args: targets
// lists the kit's targets, snapshots a target's panel snapshots, and fetch
// writes one's copy, checked with the kit's key.
func (a *Agent) PanelRecovery(ctx context.Context, args []string, rio RecoveryIO) error {
	o, err := recoveryOptionsOf(args)
	if err != nil {
		return err
	}
	header, kit, err := openKit(o, rio)
	if err != nil {
		return err
	}
	key, err := kit.Key()
	if err != nil {
		return err
	}
	// the first line says the kit, then the target and the snapshot, once
	// known; a warning comes before it
	line := fmt.Sprintf("Kit %s of %s (key %.8s…).", header.Kit, header.Panel, header.KeyID)
	say := func(more string) { fmt.Fprintln(rio.Stderr, line+more) }
	if o.op == "targets" {
		say("")
		return listTargets(kit, rio.Stdout)
	}
	if kit.Job == nil || !jobTag.MatchString("job:"+kit.Job.ID) {
		say("")
		return errNoPanelJob
	}
	target, err := chooseTarget(kit, o.target)
	if err != nil {
		say("")
		return err
	}
	line += fmt.Sprintf(" Target %s (%s)", target.Name, target.Kind)
	if target.Repository.Mount != "" && target.Server != a.serverName() {
		fmt.Fprintf(rio.Stderr, "Warning: the target %s is a directory of %s: run this there.\n", target.Name, target.Server)
	}
	repo := &agentv1.BackupRepository{
		TargetId: target.ID, Url: target.Repository.URL, Password: target.Repository.Password, Env: target.Repository.Env,
		Options: target.Repository.Options, SshKey: target.Repository.SSHKey, KnownHosts: target.Repository.KnownHosts, Mount: target.Repository.Mount,
	}
	tag := "job:" + kit.Job.ID
	limit := recoverySnapshots
	if o.op == "fetch" {
		limit = maxSnapshotsListed
	}
	listed, err := a.recoverySnapshots(ctx, repo, tag, limit, rio.Stderr)
	if err != nil {
		say(".")
		return err
	}
	if len(listed.Snapshots) == 0 {
		say(".")
		return fmt.Errorf("no panel snapshot of %s on %s", tag, target.Name)
	}
	if o.op == "snapshots" {
		say(fmt.Sprintf(": %d panel snapshots of %s, newest first.", listed.Total, tag))
		for _, snapshot := range listed.Snapshots {
			size := ""
			if snapshot.SizeBytes != nil {
				size = "  " + sizeText(*snapshot.SizeBytes)
			}
			fmt.Fprintf(rio.Stdout, "%.8s  %s  %s%s\n", snapshot.ID, utcMinute(snapshot.Time), snapshot.Hostname, size)
		}
		return nil
	}
	snapshot, err := pickSnapshot(listed.Snapshots, o.snapshot)
	if err != nil {
		say(".")
		return err
	}
	which := "panel snapshot"
	if snapshot.ID == listed.Snapshots[0].ID {
		which = "newest panel snapshot"
	}
	say(fmt.Sprintf(": %s %.8s from %s.", which, snapshot.ID, utcMinute(snapshot.Time)))
	return a.fetchCopy(ctx, repo, tag, snapshot.ID, key, header, o, rio)
}

// errNoPanelJob is a kit made while the panel had no backup job.
var errNoPanelJob = errors.New("the kit holds no backup job of the panel: it holds the master key alone, and no target to fetch the panel's copy from")

// openKit reads the kit, and opens it with its passphrase.
func openKit(o recoveryOptions, rio RecoveryIO) (recoverykit.Header, recoverykit.Payload, error) {
	var source io.Reader
	if o.kit == "-" {
		source = rio.Stdin
	} else {
		file, err := os.Open(o.kit)
		if err != nil {
			return recoverykit.Header{}, recoverykit.Payload{}, fmt.Errorf("the kit cannot be read: %w", err)
		}
		defer file.Close()
		source = file
	}
	if source == nil {
		source = strings.NewReader("")
	}
	text, err := io.ReadAll(io.LimitReader(source, recoverykit.MaxSize+1))
	if err != nil {
		return recoverykit.Header{}, recoverykit.Payload{}, fmt.Errorf("the kit cannot be read: %w", err)
	}
	if len(text) > recoverykit.MaxSize {
		return recoverykit.Header{}, recoverykit.Payload{}, fmt.Errorf("%w: it is larger than %d KiB", recoverykit.ErrNotAKit, recoverykit.MaxSize>>10)
	}
	// the header first, which needs no passphrase: a file that is no kit
	// asks for none
	if _, err := recoverykit.ParseHeader(string(text)); err != nil {
		return recoverykit.Header{}, recoverykit.Payload{}, err
	}
	passphrase, err := kitPassphrase(o, rio)
	if err != nil {
		return recoverykit.Header{}, recoverykit.Payload{}, err
	}
	header, kit, err := recoverykit.Open(string(text), passphrase)
	if errors.Is(err, recoverykit.ErrPassphrase) {
		return header, kit, recoverykit.ErrPassphrase
	}
	return header, kit, err
}

// kitPassphrase is the kit's passphrase: from --passphrase-file, from
// POCKETCLOUD_KIT_PASSPHRASE, or asked on the terminal.
func kitPassphrase(o recoveryOptions, rio RecoveryIO) (string, error) {
	if o.passphraseFile != "" {
		file, err := os.Open(o.passphraseFile)
		if err != nil {
			return "", fmt.Errorf("the passphrase file cannot be read: %w", err)
		}
		defer file.Close()
		raw, err := io.ReadAll(io.LimitReader(file, maxPassphraseFile+1))
		if err != nil {
			return "", fmt.Errorf("the passphrase file cannot be read: %w", err)
		}
		if len(raw) > maxPassphraseFile {
			return "", errors.New("the passphrase file holds more than a passphrase")
		}
		// one line, as an editor or echo ends it
		text := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
		return text, nil
	}
	if rio.Getenv != nil {
		if passphrase := rio.Getenv(envKitPassphrase); passphrase != "" {
			return passphrase, nil
		}
	}
	if rio.Prompt != nil && o.kit != "-" {
		passphrase, err := rio.Prompt()
		if err == nil {
			return passphrase, nil
		}
		if !errors.Is(err, ErrNoTerminal) {
			return "", err
		}
	}
	return "", fmt.Errorf("the kit's passphrase comes from --passphrase-file, %s or a terminal, and none is there", envKitPassphrase)
}

// listTargets lists the kit's targets, the one fetch takes first.
func listTargets(kit recoverykit.Payload, w io.Writer) error {
	if kit.Job == nil || len(kit.Targets) == 0 {
		return errNoPanelJob
	}
	chosen, _ := chooseTarget(kit, "")
	fmt.Fprintf(w, "The panel's backup job %s (%s) backs up to:\n", kit.Job.Name, kit.Job.ID)
	for _, target := range kit.Targets {
		mark := " "
		if target.ID == chosen.ID {
			mark = "*"
		}
		secrets := backup.Secrets(target.Repository.Password, target.Repository.Env, target.Repository.SSHKey)
		where := backup.Redact(target.Repository.URL, secrets...)
		if target.Repository.Mount != "" {
			where = target.Repository.Mount + " on " + target.Server
		}
		fmt.Fprintf(w, "%s %s (%s): %s\n", mark, target.Name, target.Kind, where)
	}
	return nil
}

// chooseTarget is the target named, or the kit's first remote one, or its
// only one when it is a server's directory.
func chooseTarget(kit recoverykit.Payload, name string) (recoverykit.Target, error) {
	if len(kit.Targets) == 0 {
		return recoverykit.Target{}, errors.New("the kit holds no target of the panel's backup job")
	}
	names := make([]string, 0, len(kit.Targets))
	for _, target := range kit.Targets {
		if name != "" && target.Name == name {
			return target, nil
		}
		names = append(names, target.Name)
	}
	if name != "" {
		return recoverykit.Target{}, fmt.Errorf("the kit has no target %s: it has %s", name, strings.Join(names, ", "))
	}
	for _, target := range kit.Targets {
		if target.Repository.Mount == "" {
			return target, nil
		}
	}
	if len(kit.Targets) == 1 {
		return kit.Targets[0], nil
	}
	return recoverykit.Target{}, fmt.Errorf("the kit's targets are directories of servers: name the one to take with --target: %s", strings.Join(names, ", "))
}

// serverName is the name of this agent's server, as its identity says, or
// "" without one, as on a new machine.
func (a *Agent) serverName() string {
	identity, err := LoadIdentity(a.Config.StateDir)
	if err != nil {
		return ""
	}
	return identity.Name
}

// recoveryLabels are the labels of a recovery's job: a command id of its
// own, which names its hostname pc-<id>, as every backup job's.
func recoveryLabels() (map[string]string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	raw := make([]byte, 15)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	for i, b := range raw {
		raw[i] = alphabet[int(b)%len(alphabet)]
	}
	return map[string]string{"pocketcloud.command": string(raw)}, nil
}

// recoverySnapshots lists the snapshots of the job's tag in the repository,
// newest first, at most limit.
func (a *Agent) recoverySnapshots(ctx context.Context, repo *agentv1.BackupRepository, tag string, limit int, log io.Writer) (snapshotsResult, error) {
	labels, err := recoveryLabels()
	if err != nil {
		return snapshotsResult{}, err
	}
	out, err := a.backupJob(ctx, repo, backupJobSpec{
		args:   []string{"snapshots", "--tag", "pocketcloud", "--tag", tag, "--limit", fmt.Sprint(limit)},
		labels: labels, noCache: true, repoReadOnly: true,
	}, log)
	if err != nil {
		return snapshotsResult{}, err
	}
	var listed snapshotsResult
	if err := json.Unmarshal(out, &listed); err != nil {
		return snapshotsResult{}, fmt.Errorf("the backup tool said: %s", out)
	}
	return listed, nil
}

// pickSnapshot is the snapshot named: latest, the newest; a full id; or its
// first 8 characters, or more.
func pickSnapshot(snapshots []snapshotItem, name string) (snapshotItem, error) {
	if name == "latest" {
		return snapshots[0], nil
	}
	if len(name) < 8 || len(name) > 64 || strings.Trim(name, "0123456789abcdef") != "" {
		return snapshotItem{}, fmt.Errorf("a snapshot is latest, or its id or its first 8 characters, not %q", name)
	}
	var found []snapshotItem
	for _, snapshot := range snapshots {
		if strings.HasPrefix(snapshot.ID, name) {
			found = append(found, snapshot)
		}
	}
	switch len(found) {
	case 0:
		return snapshotItem{}, fmt.Errorf("no panel snapshot %s of this job on the target", name)
	case 1:
		return found[0], nil
	}
	return snapshotItem{}, fmt.Errorf("%s starts %d snapshots: give more of the id", name, len(found))
}

// fetchCopy dumps the snapshot's copy into o.out, checking it with the
// kit's key as it comes; a copy that fails leaves no file.
func (a *Agent) fetchCopy(ctx context.Context, repo *agentv1.BackupRepository, tag, snapshot string, key []byte, header recoverykit.Header, o recoveryOptions, rio RecoveryIO) error {
	out := rio.Stdout
	var file *os.File
	if o.out != "-" {
		var err error
		// never over a file that is there, such as another copy
		file, err = os.OpenFile(o.out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("the copy cannot be written: %w", err)
		}
		out = file
	}
	fail := func(err error) error {
		if file != nil {
			_ = file.Close()
			_ = os.Remove(o.out)
		}
		return err
	}
	labels, err := recoveryLabels()
	if err != nil {
		return fail(err)
	}
	verifier := newCopyVerifier(out, key, !o.noVerify)
	_, jobErr := a.backupJob(ctx, repo, backupJobSpec{
		args:         []string{"dump", "--no-lock", "--snapshot", snapshot, "--path", "/" + backup.PanelFileName, "--expect-tag", tag},
		attachOutput: verifier, noCache: true, repoReadOnly: true, stopGrace: backupStopGrace, labels: labels,
	}, rio.Stderr)
	summary, verifyErr := verifier.finish()
	switch {
	case errors.Is(verifyErr, panelbackup.ErrOtherKey):
		return fail(errors.New("the copy does not open with the kit's key: it belongs to another panel"))
	case verifyErr != nil:
		return fail(fmt.Errorf("the copy does not open with the kit's key: %w", verifyErr))
	case jobErr != nil:
		return fail(jobErr)
	}
	if !o.noVerify && summary.Manifest.CAPin != header.CAPin {
		return fail(fmt.Errorf("the copy is of a panel whose CA is %s, not the kit's %s: it belongs to another panel", summary.Manifest.CAPin, header.CAPin))
	}
	if file != nil {
		if err := file.Sync(); err != nil {
			return fail(fmt.Errorf("the copy cannot be written: %w", err))
		}
		if err := file.Close(); err != nil {
			file = nil
			_ = os.Remove(o.out)
			return fmt.Errorf("the copy cannot be written: %w", err)
		}
	}
	digest := hex.EncodeToString(verifier.hash.Sum(nil))
	if o.noVerify {
		fmt.Fprintf(rio.Stderr, "Fetched it: %s, sha256 %s; not checked with the kit's key (--no-verify).\n", sizeText(verifier.n), digest)
		return nil
	}
	m := summary.Manifest
	servers := fmt.Sprintf("%d servers", m.Hosts)
	if m.Hosts == 1 {
		servers = "1 server"
	}
	fmt.Fprintf(rio.Stderr, "Fetched it: %s, sha256 %s; every part opens with the kit's key (export %s, panel %s, %s).\n",
		sizeText(verifier.n), digest, utcMinute(m.CreatedAt), m.PanelVersion, servers)
	return nil
}

// utcMinute says an RFC 3339 time to the minute, in UTC, as people read it.
func utcMinute(text string) string {
	at, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return text
	}
	return at.UTC().Format("2006-01-02 15:04 UTC")
}

// errVerifyStopped fails the writes after the check of a copy failed.
var errVerifyStopped = errors.New("the copy stopped opening with the kit's key")

// copyVerifier writes a copy on as it comes, counts it and takes its
// digest, and checks it with the kit's key on the way: panelbackup.Verify
// reads what passes, and a copy it refuses fails the writes after.
type copyVerifier struct {
	out     io.Writer
	hash    hash.Hash
	n       int64
	pipe    *io.PipeWriter
	done    chan struct{}
	summary panelbackup.Summary
	err     error
}

func newCopyVerifier(out io.Writer, key []byte, verify bool) *copyVerifier {
	v := &copyVerifier{out: out, hash: sha256.New(), done: make(chan struct{})}
	if !verify {
		close(v.done)
		return v
	}
	r, w := io.Pipe()
	v.pipe = w
	go func() {
		defer close(v.done)
		v.summary, v.err = panelbackup.Verify(r, key)
		r.CloseWithError(errVerifyStopped)
	}()
	return v
}

// Write checks data before it writes it on.
func (v *copyVerifier) Write(data []byte) (int, error) {
	if v.pipe != nil {
		if _, err := v.pipe.Write(data); err != nil {
			return 0, err
		}
	}
	n, err := v.out.Write(data)
	v.hash.Write(data[:n])
	v.n += int64(n)
	return n, err
}

// finish ends the copy, and says what the check found.
func (v *copyVerifier) finish() (panelbackup.Summary, error) {
	if v.pipe != nil {
		_ = v.pipe.Close()
	}
	<-v.done
	return v.summary, v.err
}
