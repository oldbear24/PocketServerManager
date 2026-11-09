package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
)

// Backup runs (#29). A run backs up one job's source to one target's
// repository in a job container like a target's check, and then applies the
// job's retention. What it backs up is mounted read-only: each Docker volume
// at /volumes/<name>, and each directory of the server at its own path, so
// that snapshots name the server's paths; restic never runs in the agent's
// own container, and its /host is no part of the job: the agent only follows
// a directory's symbolic links through it first, so that it checks and
// mounts where they lead, which the Engine would follow unchecked. A
// database dump runs in the database's own container with docker exec, and
// the agent writes what it prints to the job's attached standard input, as
// the relay's frames (see internal/backup/relay.go), which the backup tool
// hands to restic.

const (
	// backupVolumes is where a job mounts the volumes it backs up.
	backupVolumes = "/volumes"
	// dumpSocket is where the backup tool listens for restic's relay, in
	// the job's own /tmp.
	dumpSocket = "/tmp/pocketcloud-dump.sock"
	// backupLogTail is how much of the end of a run's output its result
	// keeps.
	backupLogTail = 16 << 10
	// dumpFailed is the exit code of a dump the agent could not run.
	dumpFailed = 125
)

// volumeName is a Docker volume's name, as Docker takes one.
var volumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

// backupJobDirs are the backup job's own directories: its programs, restic's
// temporary files, and its mounts. A directory of the server that is one of
// them, or holds one, would hide it, mounted at its own path.
var backupJobDirs = []string{"/bin", "/sbin", "/lib", "/usr/bin", "/usr/sbin", "/usr/lib", "/usr/local/bin",
	"/tmp", "/run/pocketcloud", "/var/lib/pocketcloud", resticCacheDir, backup.LocalMount, backupVolumes}

// backupJobLinks are symbolic links of the backup job's image: a directory
// mounted at one of them, or in one, would land where it leads in the job,
// such as /var/run on its /run.
var backupJobLinks = []string{"/var/run", "/var/lock", "/var/spool/mail"}

// handleBackupRun backs up a job's volumes, directories or database dump to a
// target's repository, and applies the job's retention.
func (a *Agent) handleBackupRun(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetBackupRun()
	if err := checkRepository(payload.GetRepository()); err != nil {
		return nil, err
	}
	job, err := a.backupRunJob(ctx, payload)
	if err != nil {
		return nil, err
	}
	log := &tailBuffer{limit: backupLogTail}
	output := io.MultiWriter(reporter, log)
	for _, note := range job.notes {
		fmt.Fprintln(output, note)
	}
	out, err := a.backupJob(ctx, payload.GetRepository(), job, output)
	answer := map[string]any{"log": log.String()}
	var result backupRunResult
	saved := len(out) > 0 && json.Unmarshal(out, &result) == nil && result.SnapshotID != ""
	if saved {
		maps.Copy(answer, result.answer())
	}
	var refused *backupRefused
	switch {
	case errors.As(err, &refused):
		// the job ran and restic or the tool said no; a snapshot that lacks
		// files is still said
		answer["refused"] = true
		return answer, err
	case err != nil:
		return answer, missingDirectory(err)
	case !saved:
		return answer, fmt.Errorf("the backup tool said: %s", out)
	}
	return answer, nil
}

// missingDirectory words the Engine's refusal to mount a directory of the
// server that is not there, which the agent cannot see itself.
func missingDirectory(err error) error {
	const missing = "bind source path does not exist: "
	message := err.Error()
	at := strings.Index(message, missing)
	if at < 0 {
		return err
	}
	dir := message[at+len(missing):]
	if end := strings.LastIndex(dir, " (HTTP "); end >= 0 {
		dir = dir[:end]
	}
	return fmt.Errorf("the directory %s does not exist on this server", dir)
}

// answer is a run's result for the panel.
func (r backupRunResult) answer() map[string]any {
	answer := map[string]any{
		"snapshot_id": r.SnapshotID, "bytes_added": r.BytesAdded, "total_bytes": r.TotalBytes,
		"files_new": r.FilesNew, "files_changed": r.FilesChanged, "forgotten": int64(r.Forgotten),
	}
	if r.RetentionError != "" {
		answer["retention_error"] = r.RetentionError
	}
	return answer
}

// backupRunJob checks a run and says the job that does it.
func (a *Agent) backupRunJob(ctx context.Context, payload *agentv1.BackupRun) (backupJobSpec, error) {
	// restic reads files of any owner with DAC_OVERRIDE, which the read-only
	// mounts keep from writing them; never DAC_READ_SEARCH, with which
	// Docker's seccomp profile allows open_by_handle_at, which opens any file
	// of a file system the job mounts a part of
	job := backupJobSpec{capabilities: []string{"DAC_OVERRIDE"}}
	volumes, paths, dump := payload.GetVolumes(), payload.GetPaths(), payload.GetStdinCommand()
	sources := 0
	for _, source := range [][]string{volumes, paths, dump} {
		if len(source) > 0 {
			sources++
		}
	}
	if sources != 1 {
		return job, errors.New("a backup run backs up volumes, directories or a dump: exactly one of them")
	}
	if payload.GetHostName() == "" {
		return job, errors.New("a backup run needs the server's name, for its snapshots")
	}
	if len(payload.GetTags()) == 0 {
		return job, errors.New("a backup run needs its job's tags, which its retention goes by")
	}
	args := []string{"backup", "--host", payload.GetHostName()}
	for _, tag := range payload.GetTags() {
		args = append(args, "--tag", tag)
	}
	retention := payload.GetRetention()
	for i, n := range []int32{retention.GetLast(), retention.GetHourly(), retention.GetDaily(), retention.GetWeekly(), retention.GetMonthly(), retention.GetYearly()} {
		if n < 0 {
			return job, fmt.Errorf("a retention keeps a number of snapshots, not %d", n)
		}
		if n > 0 {
			args = append(args, "--"+keepOptions[i], strconv.Itoa(int(n)))
		}
	}

	switch {
	case len(volumes) > 0:
		for i, name := range volumes {
			if !volumeName.MatchString(name) {
				return job, fmt.Errorf("%q is not a volume's name", name)
			}
			if slices.Contains(volumes[:i], name) {
				return job, fmt.Errorf("the volume %s is there twice", name)
			}
			// never one Docker would make, empty, for the mount
			if _, err := a.Docker.InspectVolume(ctx, name); dockerapi.IsNotFound(err) {
				return job, fmt.Errorf("the volume %s does not exist on this server", name)
			} else if err != nil {
				return job, fmt.Errorf("the volume %s cannot be inspected: %w", name, err)
			}
			target := backupVolumes + "/" + name
			job.mounts = append(job.mounts, map[string]any{"Type": "volume", "Source": name, "Target": target, "ReadOnly": true})
			args = append(args, "--path", target)
		}
	case len(paths) > 0:
		recursive, err := a.recursiveReadOnly(ctx)
		if err != nil {
			return job, err
		}
		var resolved []string
		for _, dir := range paths {
			if err := checkBackupPath(dir); err != nil {
				return job, err
			}
			// the Engine would follow a symbolic link on the way, past
			// what the checks refuse: what is checked and mounted is
			// where it leads
			real, err := resolveServerDir(a.hostRoot(), dir)
			if err != nil {
				return job, err
			}
			if err := checkBackupPath(real); err != nil {
				return job, err
			}
			if slices.Contains(resolved, real) {
				return job, fmt.Errorf("the directory %s is there twice", real)
			}
			if real != dir {
				job.notes = append(job.notes, fmt.Sprintf("The directory %s leads to %s on this server: backing up %s", dir, real, real))
			}
			resolved = append(resolved, real)
			mount := map[string]any{"Type": "bind", "Source": real, "Target": real, "ReadOnly": true}
			if recursive {
				// the start fails rather than leave a mount under it
				// writable
				mount["BindOptions"] = map[string]any{"ReadOnlyForceRecursive": true}
			} else {
				job.notes = append(job.notes, real+" is mounted read-only at its top only: this Docker Engine or kernel cannot make the mounts under it read-only.")
			}
			job.mounts = append(job.mounts, mount)
			args = append(args, "--path", real)
		}
	default:
		name := payload.GetStdinFileName()
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\n\r\x00") {
			return job, fmt.Errorf("a dump's file name cannot be %q", name)
		}
		if payload.GetStdinContainer() == "" {
			return job, errors.New("a dump needs the container it runs in")
		}
		info, err := a.Docker.Inspect(ctx, payload.GetStdinContainer())
		if dockerapi.IsNotFound(err) {
			return job, fmt.Errorf("the dump's container %.12s does not exist on this server", payload.GetStdinContainer())
		} else if err != nil {
			return job, err
		}
		if !info.State.Running {
			return job, fmt.Errorf("the container %s is not running, and the dump runs in it", strings.TrimPrefix(info.Name, "/"))
		}
		args = append(args, "--stdin-name", name, "--relay", dumpSocket)
		var env []string
		for _, key := range slices.Sorted(maps.Keys(payload.GetEnv())) {
			env = append(env, key+"="+payload.GetEnv()[key])
		}
		repo := payload.GetRepository()
		secrets := backup.Secrets(repo.GetPassword(), mergedEnv(repo.GetEnv(), payload.GetEnv()), repo.GetSshKey())
		job.feed = func(ctx context.Context, stdin io.WriteCloser) {
			a.feedDump(ctx, stdin, info.ID, dump, env, secrets)
		}
	}
	job.args = args
	return job, nil
}

// mergedEnv is the values of both environments.
func mergedEnv(a, b map[string]string) map[string]string {
	merged := maps.Clone(a)
	if merged == nil {
		merged = map[string]string{}
	}
	for key, value := range b {
		merged["dump:"+key] = value
	}
	return merged
}

// checkBackupPath checks a directory of the server to back up: absolute,
// clean, neither / nor the kernel's, and none that would hide the job's own
// or land elsewhere in the job. A run checks the directory as the panel
// names it, and again where its symbolic links lead.
func checkBackupPath(dir string) error {
	if !path.IsAbs(dir) || strings.ContainsAny(dir, "\n\r\x00") || path.Clean(dir) != dir {
		return fmt.Errorf("a directory to back up is absolute and clean, such as /srv/app, not %q", dir)
	}
	if dir == "/" {
		return errors.New("the whole of / is not backed up: choose its directories, such as /srv or /home")
	}
	for _, kernel := range []string{"/proc", "/sys", "/dev"} {
		if dir == kernel || strings.HasPrefix(dir, kernel+"/") {
			return fmt.Errorf("%s is the kernel's, with no files to back up", dir)
		}
	}
	for _, own := range backupJobDirs {
		if own == dir || strings.HasPrefix(own, dir+"/") {
			return fmt.Errorf("%s would hide the backup job's own %s, as the job mounts it at its own path: back up the directories in it", dir, own)
		}
	}
	for _, mount := range []string{resticCacheDir, backup.LocalMount} {
		if strings.HasPrefix(dir, mount+"/") {
			return fmt.Errorf("%s is in the backup job's own %s: it cannot be mounted at its own path", dir, mount)
		}
	}
	for _, link := range backupJobLinks {
		if dir == link || strings.HasPrefix(dir, link+"/") {
			return fmt.Errorf("%s is a symbolic link in the backup job, which cannot mount %s at its own path: back up where it leads", link, dir)
		}
	}
	return nil
}

// recursiveReadOnlyAPI is the first Engine API that makes a bind mount
// read-only with the mounts under it, on a kernel from 5.12 on; before, a
// read-only bind is read-only at its top only, and a disk mounted in the
// directory stays writable.
const recursiveReadOnlyAPI = "1.44"

// recursiveReadOnly says whether the Engine and its kernel can make a bind
// mount read-only with every mount under it.
func (a *Agent) recursiveReadOnly(ctx context.Context) (bool, error) {
	version, err := a.Docker.Version(ctx)
	if err != nil {
		return false, err
	}
	if !apiAtLeast(version.APIVersion, recursiveReadOnlyAPI) {
		return false, nil
	}
	info, err := a.Docker.Info(ctx)
	if err != nil {
		return false, err
	}
	return kernelAtLeast(info.KernelVersion, 5, 12), nil
}

// kernelAtLeast says whether a kernel's release, such as 6.1.0-18-amd64, is
// major.minor or later.
func kernelAtLeast(release string, major, minor int) bool {
	number := func(text string) (int, bool) {
		end := strings.IndexFunc(text, func(r rune) bool { return r < '0' || r > '9' })
		if end < 0 {
			end = len(text)
		}
		n, err := strconv.Atoi(text[:end])
		return n, err == nil
	}
	first, rest, _ := strings.Cut(release, ".")
	x, ok := number(first)
	if !ok {
		return false
	}
	y, ok := number(rest)
	if !ok {
		return false
	}
	return x > major || x == major && y >= minor
}

// maxLinks is how many symbolic links a directory of the server may lead
// through, as many as the kernel follows.
const maxLinks = 40

// hostRoot is where the agent sees the server's /.
func (a *Agent) hostRoot() string {
	if a.Config.HostRoot == "" {
		return "/"
	}
	return a.Config.HostRoot
}

// resolveServerDir follows dir, a directory of the server, through root,
// where the agent sees the server's /, as the server would: each symbolic
// link on the way leads where it does on the server, an absolute one from the
// server's /, and none ever out of it. It says the directory with no link
// left in it.
func resolveServerDir(root, dir string) (string, error) {
	if _, err := os.Stat(root); err != nil {
		return "", fmt.Errorf("the agent cannot see the server's directories at %s, where the server's / is mounted: %w", root, err)
	}
	resolved := "/"
	rest := strings.Split(dir, "/")
	links := 0
	for len(rest) > 0 {
		name := rest[0]
		rest = rest[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			// never above the server's /
			resolved = path.Dir(resolved)
			continue
		}
		next := path.Join(resolved, name)
		file := filepath.Join(root, filepath.FromSlash(next))
		info, err := os.Lstat(file)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("the directory %s does not exist on this server", dir)
		case err != nil:
			return "", fmt.Errorf("the directory %s cannot be read on this server: %w", dir, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			if links++; links > maxLinks {
				return "", fmt.Errorf("the directory %s leads through more than %d symbolic links on this server", dir, maxLinks)
			}
			target, err := os.Readlink(file)
			if err != nil {
				return "", fmt.Errorf("the directory %s cannot be read on this server: %w", dir, err)
			}
			if path.IsAbs(target) {
				resolved = "/"
			}
			rest = append(strings.Split(target, "/"), rest...)
			continue
		}
		if !info.IsDir() {
			if strings.Join(rest, "") != "" {
				return "", fmt.Errorf("the directory %s does not exist on this server", dir)
			}
			return "", fmt.Errorf("%s is not a directory on this server: a backup job backs up directories", dir)
		}
		resolved = next
	}
	return resolved, nil
}

// feedDump runs a dump's command in its container with docker exec, and
// writes what it prints to the job's standard input as the relay's frames,
// and last how it ended. A job that stops reading leaves the dump without
// anyone to read it: docker exec cannot stop a process, which runs to its
// end in its container.
func (a *Agent) feedDump(ctx context.Context, stdin io.WriteCloser, container string, cmd, env, secrets []string) {
	defer stdin.Close()
	frames := backup.NewFrameWriter(stdin)
	code, message, ok := a.dump(ctx, container, cmd, env, frames)
	if ok {
		_ = frames.End(code, backup.Redact(message, secrets...))
	}
}

// dump runs a dump and writes its output to frames, and says its exit code
// and its last words. It says nothing when the job stopped reading: restic
// says why.
func (a *Agent) dump(ctx context.Context, container string, cmd, env []string, frames io.Writer) (int, string, bool) {
	id, err := a.Docker.ExecCreate(ctx, container, dockerapi.ExecOptions{Cmd: cmd, Env: env})
	if err != nil {
		return dumpFailed, "the dump could not start in its container: " + err.Error(), true
	}
	conn, err := a.Docker.ExecStart(ctx, id, false)
	if err != nil {
		return dumpFailed, "the dump could not start in its container: " + err.Error(), true
	}
	// reading a hijacked connection does not end with the context by itself
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	stderr := &tailBuffer{limit: 4 << 10}
	var head bytes.Buffer
	var writeErr error
	readErr := dockerapi.ReadMultiplexed(conn, func(stream byte, data []byte) {
		if writeErr != nil {
			return
		}
		if stream == 2 {
			_, _ = stderr.Write(data)
			return
		}
		if room := 512 - head.Len(); room > 0 {
			head.Write(data[:min(len(data), room)])
		}
		if _, writeErr = frames.Write(data); writeErr != nil {
			_ = conn.Close()
		}
	})
	_ = conn.Close()
	switch {
	case writeErr != nil:
		return 0, "", false
	case ctx.Err() != nil:
		return dumpFailed, "the dump was stopped: " + ctx.Err().Error(), true
	case readErr != nil:
		return dumpFailed, "the dump's output broke off: " + readErr.Error(), true
	}
	code, err := a.execExitCode(ctx, id)
	if err != nil {
		return dumpFailed, "how the dump ended is unknown: " + err.Error(), true
	}
	message := lastLines(stderr.String(), 4)
	// the Engine says a command it could not run on its output
	if failed, _, _ := strings.Cut(head.String(), "\n"); message == "" && (code == 126 || code == 127) && strings.HasPrefix(failed, "OCI runtime exec failed") {
		message = strings.TrimSpace(failed)
	}
	return code, message, true
}

// execExitCode is the exit code of a process whose output ended: the Engine
// may say it runs for a moment after.
func (a *Agent) execExitCode(ctx context.Context, id string) (int, error) {
	for attempt := 0; ; attempt++ {
		running, code, err := a.Docker.ExecInspect(ctx, id)
		if err != nil {
			return 0, err
		}
		if !running {
			return code, nil
		}
		if attempt == 50 {
			return 0, errors.New("the process did not end after its output did")
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// lastLines is the last n lines of text that are not empty.
func lastLines(text string, n int) string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines[max(len(lines)-n, 0):], "\n")
}
