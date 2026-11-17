package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
)

// Backups (#28). restic runs in a job container of the agent's own image, one
// per command, through the backup tool. The panel adds the repository's
// password and the storage's credentials to the command when it delivers it:
// they go into the job's environment, an SFTP key and the server's pinned
// host key into files copied in before it starts, and nowhere else on the
// server. A local target's directory is the only thing of the server an init
// job mounts; a backup run's job mounts, besides, what it backs up, read-only
// (see backuprun.go), and the first pass of an integrity check mounts it
// read-only (see backupcheck.go). Every job's hostname names its command,
// pc-<command id>, so that a lock restic leaves says which command holds it
// (#31).

const (
	// backupTargetLabel names the target of a backup job container.
	backupTargetLabel = "pocketcloud.backup-target"
	// resticCache is the volume of restic's cache, which every job of the
	// server shares: restic keeps each repository's apart, by its id, and
	// what it holds is as encrypted as the repository. Without it every
	// check would fetch every snapshot's trees again.
	resticCache    = "pocketcloud-restic-cache"
	resticCacheDir = "/cache"
	// backupStopGrace is how long a backup run's job that is cancelled, or
	// a backup job left by the agent, has to stop before it is removed: the
	// backup tool stops restic, which removes its locks; a job removed at
	// once is killed, and its locks keep other backups and retentions
	// waiting. A target's connection check (#28's init) takes no lock, and
	// is removed at once; an integrity check's second pass holds restic's
	// exclusive lock, which a killed one leaves for 30 minutes (#31).
	backupStopGrace = 30 * time.Second
	// maxLockClockSkew is how far the server's clock may be off the panel's
	// before the backup tool removes no lock (#31): restic judges a lock's
	// age by the clock of the server it runs on, and one whose clock is far
	// ahead takes a live lock for a stale one.
	maxLockClockSkew = 5 * time.Minute
	// imageVolume is the VOLUME of the agent's image (images/agent/
	// Dockerfile), the agent's state, which a job covers (#30).
	imageVolume = "/var/lib/pocketcloud"
)

func (a *Agent) registerBackupHandlers() {
	a.Executor.Handle("backup.init", a.handleBackupInit)
	a.Executor.Handle("backup.run", a.handleBackupRun)
	a.Executor.Handle("backup.check", a.handleBackupCheck)
	a.registerRestoreHandlers()
}

// handleBackupInit opens a target's repository, or creates it, and says what
// the panel asked of it.
func (a *Agent) handleBackupInit(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetBackupInit()
	args := []string{"init"}
	if payload.GetRepoId() != "" {
		args = append(args, "--expect", payload.GetRepoId())
	}
	if payload.GetUsage() {
		args = append(args, "--usage")
	}
	out, err := a.backupJob(ctx, payload.GetRepository(), backupJobSpec{args: args, labels: commandLabels(command)}, reporter)
	var refused *backupRefused
	if errors.As(err, &refused) {
		// the job ran and restic, or the tool, said no: the panel may forget
		// a new target, as no repository was made with its password
		return map[string]any{"refused": true}, err
	}
	if err != nil {
		return nil, err
	}
	var result backupInitResult
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("the backup tool said: %s", out)
	}
	answer := map[string]any{"created": result.Created, "id": result.ID}
	if result.Used != nil {
		answer["used_bytes"] = *result.Used
	}
	if result.Free != nil {
		answer["free_bytes"] = *result.Free
	}
	return answer, nil
}

// backupJobSpec is what a backup job does besides opening its repository.
type backupJobSpec struct {
	// args are the backup tool's op and its flags
	args []string
	// mounts are what of the server the job reads
	mounts []map[string]any
	// capabilities are those the job needs besides a local target's
	capabilities []string
	// feed, when set, writes the job's standard input once the backup tool
	// says mark, and closes it; it stops when its context ends, and never
	// starts for a job that ends first
	feed func(ctx context.Context, stdin io.WriteCloser)
	// mark is the tool's line that starts feed: relayReady, once restic
	// reads a dump, when empty; restoreReady for a restore (#30)
	mark string
	// notes are lines for the run's log, said before the job starts
	notes []string
	// stopGrace is how long the job has to stop when its command is
	// cancelled, before it is removed (see backupStopGrace)
	stopGrace time.Duration
	// noCache leaves restic's cache out: the job mounts no cache volume and
	// restic keeps none, as a restore writes nothing of the server but its
	// destination (#30)
	noCache bool
	// progress, when set, gets the JSON of each of the tool's progress
	// lines (see progressLine), which stay out of the log
	progress func(data []byte)
	// attachOutput, when set, gets the job's standard output as the tool
	// writes it, through the job's attached output, and the job keeps no
	// log: a download's bytes never rest in the Engine's log files (#30)
	attachOutput io.Writer
	// logConfig is the job's log driver and its options; json-file when nil
	logConfig map[string]any
	// labels are added to the job's own, such as the command it runs, which
	// names the job's hostname too (see backup.JobHost)
	labels map[string]string
	// repoReadOnly binds a local target's directory read-only, with no
	// capability but DAC_OVERRIDE: the first pass of an integrity check
	// reads the repository without a lock and writes nothing (#31)
	repoReadOnly bool
}

// clockSkewed says how far the server's clock is off the panel's, as the last
// heartbeat measured it, and whether that is more than maxLockClockSkew.
func (a *Agent) clockSkewed() (time.Duration, bool) {
	skew := time.Duration(a.lastSkew.Load()) * time.Microsecond
	if skew < 0 {
		skew = -skew
	}
	return skew, skew > maxLockClockSkew
}

// checkRepository checks that the panel completed a command's repository.
func checkRepository(repo *agentv1.BackupRepository) error {
	if repo.GetUrl() == "" || repo.GetPassword() == "" {
		return errors.New("the command has no repository or no password: the panel adds them when it delivers it")
	}
	return nil
}

// backupJob runs the backup tool for a repository in a job container, and
// returns the JSON it printed last, also when it refused, as a backup that
// saved a snapshot without some files says the snapshot.
func (a *Agent) backupJob(ctx context.Context, repo *agentv1.BackupRepository, job backupJobSpec, log io.Writer) ([]byte, error) {
	if err := checkRepository(repo); err != nil {
		return nil, err
	}
	secrets := backup.Secrets(repo.GetPassword(), repo.GetEnv(), repo.GetSshKey())

	self, err := a.ownContainer(ctx)
	if err != nil {
		return nil, fmt.Errorf("the agent cannot find its own image for the backup job: %w", err)
	}

	env := []string{envResticRepository + "=" + repo.GetUrl(), envResticPassword + "=" + repo.GetPassword()}
	for _, name := range slices.Sorted(maps.Keys(repo.GetEnv())) {
		env = append(env, name+"="+repo.GetEnv()[name])
	}
	cmd := append([]string{"backup-tool"}, job.args...)
	var mounts []map[string]any
	if !job.noCache {
		cmd = append(cmd, "--cache", resticCacheDir)
		mounts = append(mounts, map[string]any{"Type": "volume", "Source": resticCache, "Target": resticCacheDir})
	}
	for _, name := range slices.Sorted(maps.Keys(repo.GetOptions())) {
		cmd = append(cmd, "-o", name+"="+repo.GetOptions()[name])
	}
	if skew, off := a.clockSkewed(); off {
		// the tool's unlocks remove nothing, and say why (#31)
		cmd = append(cmd, "--no-unlock="+strconv.Itoa(int(skew.Round(time.Minute)/time.Minute)))
	}

	// a remote repository is reached over the network; a local one is a
	// directory of the server, which the job alone mounts
	network := "bridge"
	var capabilities []string
	if repo.GetMount() != "" {
		dir, err := backup.LocalPath(repo.GetMount())
		if err != nil {
			return nil, err
		}
		network = "none"
		bind := map[string]any{
			"Type": "bind", "Source": dir, "Target": backup.LocalMount,
			"BindOptions": map[string]any{"CreateMountpoint": true},
		}
		cmd = append(cmd, "--local", backup.LocalMount)
		// root of the job may use the directory whoever owns it, such as a
		// disk mounted for a user: the job reaches nothing but it and has no
		// network
		capabilities = []string{"DAC_OVERRIDE", "FOWNER"}
		if job.repoReadOnly {
			// it reads the directory whoever owns it, and writes nothing,
			// not even the directory where it is gone
			bind = map[string]any{"Type": "bind", "Source": dir, "Target": backup.LocalMount, "ReadOnly": true}
			capabilities = []string{"DAC_OVERRIDE"}
		}
		mounts = append(mounts, bind)
	}
	mounts = append(mounts, job.mounts...)
	// the agent's image declares its state folder a VOLUME, of which the
	// Engine would give every job an anonymous volume, writable and left
	// behind: an empty read-only tmpfs takes its place
	mounts = append(mounts, map[string]any{"Type": "tmpfs", "Target": imageVolume, "ReadOnly": true})
	for _, capability := range job.capabilities {
		if !slices.Contains(capabilities, capability) {
			capabilities = append(capabilities, capability)
		}
	}

	labels := maps.Clone(job.labels)
	if labels == nil {
		labels = map[string]string{}
	}
	// what cleanupBackupJobs goes by
	labels["pocketcloud.role"] = "job"
	labels[backupTargetLabel] = repo.GetTargetId()
	logConfig := job.logConfig
	if logConfig == nil {
		logConfig = map[string]any{"Type": "json-file"}
	}
	if job.attachOutput != nil {
		logConfig = map[string]any{"Type": "none"}
	}
	config := map[string]any{
		"Image":  self.Image,
		"Cmd":    cmd,
		"Env":    env,
		"Labels": labels,
		"HostConfig": map[string]any{
			"Mounts":      mounts,
			"NetworkMode": network,
			"CapDrop":     []string{"ALL"},
			"CapAdd":      capabilities,
			"SecurityOpt": []string{"no-new-privileges"},
			"LogConfig":   logConfig,
		},
	}
	if command := labels["pocketcloud.command"]; command != "" {
		// restic names a lock's holder by its hostname: the job's names
		// its command, and no two jobs that run at once share it, so that
		// restic's rule for a lock of its own host, whose PID is gone, never
		// meets another job's PID (#31)
		config["Hostname"] = backup.JobHost(command)
	}
	if job.feed != nil {
		// its input ends when the agent closes it
		config["OpenStdin"] = true
		config["StdinOnce"] = true
		config["AttachStdin"] = true
	}

	var stdin io.WriteCloser
	feedCtx, stopFeed := context.WithCancel(ctx)
	defer stopFeed()
	fed := make(chan struct{})
	feeding := false
	// the feed starts once the tool says so, as a dump once restic reads it:
	// its marks, and its progress, are read before any secret is taken out
	// of what it says, and kept out of the log
	ready := make(chan struct{})
	marks := &toolMarks{w: &redactingWriter{w: log, secrets: secrets}, progress: job.progress}
	if job.feed != nil {
		mark := job.mark
		if mark == "" {
			mark = relayReady
		}
		marks.marks = map[string]chan struct{}{mark: ready}
	}
	hooks := jobHooks{
		stopGrace: job.stopGrace,
		attach:    job.attachOutput != nil,
		prepare: func(id string) error {
			// ssh reads the key and the pinned host key from files
			for _, file := range []struct {
				name    string
				content string
				mode    int64
			}{
				{backup.SSHKeyFile, repo.GetSshKey(), 0o600},
				{backup.KnownHostsFile, repo.GetKnownHosts(), 0o644},
			} {
				if file.content == "" {
					continue
				}
				dir, name := path.Split(file.name)
				parent, sub := path.Split(path.Clean(dir))
				if err := a.Docker.CopyFile(ctx, id, parent, path.Join(sub, name), []byte(file.content), file.mode); err != nil {
					return fmt.Errorf("the job cannot get the SFTP key: %w", err)
				}
			}
			if job.feed != nil {
				// attached before it starts, so that nothing it reads is lost
				attached, err := a.Docker.Attach(ctx, id)
				if err != nil {
					return fmt.Errorf("the job's input cannot be attached: %w", err)
				}
				stdin = attached
			}
			return nil
		},
		started: func(string) {
			if stdin != nil {
				feeding = true
				go func() {
					defer close(fed)
					select {
					case <-ready:
						if feedCtx.Err() == nil {
							job.feed(feedCtx, stdin)
						}
					case <-feedCtx.Done():
						// the job ended before restic ran the relay, as
						// when it never got the repository's lock: the
						// dump never starts
					}
				}()
			}
		},
	}
	var stdout bytes.Buffer
	var output io.Writer = &stdout
	if job.attachOutput != nil {
		output = job.attachOutput
	}
	err = dockerSites{a: a}.runWith(ctx, config, hooks, output, marks)
	if stdin != nil {
		// what still feeds a job that ended stops
		stopFeed()
		_ = stdin.Close()
		if feeding {
			<-fed
		}
	}
	out := lastLine(stdout.Bytes())
	var exit *exitError
	if errors.As(err, &exit) && toolLine(exit.message) {
		// the tool's own line, last as the job was killed, says nothing of why
		exit.message = ""
	}
	if errors.As(err, &exit) && !exit.oom {
		return out, &backupRefused{backup.Redact(err.Error(), secrets...)}
	}
	if err != nil {
		return nil, errors.New(backup.Redact(err.Error(), secrets...))
	}
	return out, nil
}

// The backup tool's progress lines (#30): `--- pocketcloud: progress {json}
// ---` on its error output, which the agent reads and keeps out of the log.
const (
	progressPrefix = "--- pocketcloud: progress "
	progressSuffix = " ---"
)

// progressLine is the tool's progress line of data, a JSON object.
func progressLine(data []byte) string {
	return progressPrefix + string(data) + progressSuffix
}

// progressOf is the JSON object of a progress line, if line is one.
func progressOf(line string) ([]byte, bool) {
	inner, ok := strings.CutPrefix(line, progressPrefix)
	if !ok {
		return nil, false
	}
	inner, ok = strings.CutSuffix(inner, progressSuffix)
	if !ok || !strings.HasPrefix(inner, "{") || !strings.HasSuffix(inner, "}") {
		return nil, false
	}
	return []byte(inner), true
}

// toolLine says whether a line of a job's output is one of the tool's own:
// a mark or a progress line, which says nothing of why the job failed.
func toolLine(line string) bool {
	_, progress := progressOf(line)
	return line == relayReady || line == restoreReady || progress
}

// toolMarks passes on a job's error output line by line, but for the backup
// tool's own lines: it closes the channel of each of marks it reads, and
// hands each progress line's JSON to progress.
type toolMarks struct {
	w        io.Writer
	marks    map[string]chan struct{}
	progress func(data []byte)
	partial  []byte
}

func (m *toolMarks) Write(data []byte) (int, error) {
	m.partial = append(m.partial, data...)
	for {
		end := bytes.IndexByte(m.partial, '\n')
		if end < 0 {
			return len(data), nil
		}
		line := m.partial[:end+1]
		m.partial = m.partial[end+1:]
		text := string(bytes.TrimRight(line, "\r\n"))
		if seen, ok := m.marks[text]; ok {
			select {
			case <-seen:
			default:
				close(seen)
			}
			continue
		}
		if progress, ok := progressOf(text); ok {
			if m.progress != nil {
				m.progress(progress)
			}
			continue
		}
		if _, err := m.w.Write(line); err != nil {
			return len(data), err
		}
	}
}

// backupRefused is what the backup tool said when it ended with an error: it
// ran, and restic or the tool refused, rather than the job not running.
type backupRefused struct{ message string }

func (b *backupRefused) Error() string { return b.message }

// cleanupBackupJobs removes the job containers of backup commands the agent
// was running when it stopped: the commands ended with it. One still running
// is stopped first, so that restic removes its locks.
func (a *Agent) cleanupBackupJobs(ctx context.Context) {
	containers, err := a.Docker.Containers(ctx)
	if err != nil {
		return
	}
	for _, container := range containers {
		if _, ok := container.Labels[backupTargetLabel]; ok && container.Labels["pocketcloud.role"] == "job" {
			if container.State == "running" {
				stop, cancel := context.WithTimeout(ctx, backupStopGrace+cleanupTimeout)
				if err := a.Docker.Stop(stop, container.ID, int(backupStopGrace/time.Second)); err != nil {
					a.Log.Warn("failed to stop a backup job container", "container", container.ID, "error", err)
				}
				cancel()
			}
			if err := a.Docker.Remove(ctx, container.ID, true, true); err != nil {
				a.Log.Warn("failed to remove a backup job container", "container", container.ID, "error", err)
			}
		}
	}
}
