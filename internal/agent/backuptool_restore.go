package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/oldbear24/PocketServerManager/internal/backup"
)

// The backup tool's ops that read a repository's snapshots and restore them
// (#30). They run in a job container like a backup, and print one line of
// JSON, but dump, which prints the snapshot's files themselves.
//
//	snapshots  list the snapshots that have every --tag, newest first, at
//	           most --limit of them, with what each saved.
//	ls         list one folder of a snapshot, --dir of --snapshot: its
//	           folders first, then by name, from --offset, at most --limit,
//	           only names that hold --match in any case when it is given.
//	restore    restore --snapshot: each --root SRC=/restore/<i>, a path of
//	           the snapshot ("" for its only path, "/" for all of it) into
//	           what the job mounts there, with --dest naming it in what the
//	           tool says. It checks the repository, that the snapshot has
//	           --expect-tag, and that each destination is empty unless
//	           --overwrite; with --wait it then says restoreReady and waits
//	           for a line "go" on its standard input before it writes
//	           anything. --include limits each root to the paths under it;
//	           --replace deletes what the snapshot does not hold, as restic's
//	           --delete. --dry-run says what the restore would do, with up
//	           to --sample paths of each kind, and writes nothing.
//	dump       write --path of --snapshot to the standard output, a folder as
//	           a tar and a file as it is, once the snapshot is known to have
//	           --expect-tag. --no-lock takes no lock, and so needs no unlock:
//	           the panel's recovery (#32) writes nothing to the repository,
//	           and binds a local one read-only.

const (
	// minResticMajor and minResticMinor are the first restic whose restore,
	// ls and dump the tool reads: 0.17, with restore's --dry-run, --delete
	// and --overwrite, and the JSON it prints.
	minResticMajor, minResticMinor = 0, 17
	// restoreRetryLock is how long a restore or a download waits for a lock
	// another restic holds, such as a prune's.
	restoreRetryLock = "5m"
	// maxNameBytes is how long a name ls says may be.
	maxNameBytes = 1024
	// maxEntriesCounted is how many entries of a destination a restore
	// counts before it says it holds more.
	maxEntriesCounted = 10_000
	// maxSamplePath is how long a sample path of a preview may be, in
	// characters.
	maxSamplePath = 300
	// maxErrorSample is how many of restic's errors a restore keeps.
	maxErrorSample = 20
	// streamTail is how much of restic's error output a streamed run keeps
	// for what it said.
	streamTail = 256 << 10
)

var (
	// maxListed is how many entries of one folder ls reads; tests lower it.
	maxListed = 1_000_000
	// restoreProgressEvery is how often a restore says how far it got;
	// tests shorten it.
	restoreProgressEvery = 2 * time.Second
)

// snapshotID is a snapshot's full id, as the panel names it.
var snapshotID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// jobTag is a job's restic tag.
var jobTag = regexp.MustCompile(`^job:[a-z0-9]{15}$`)

// resticVersion finds restic's version in what `restic version` says.
var resticVersion = regexp.MustCompile(`restic (\d+)\.(\d+)\.(\d+)`)

// streamOptions say where a streamed run of restic sends what it prints.
type streamOptions struct {
	// env are more of restic's variables
	env map[string]string
	// line gets each line of its standard output
	line func(line []byte)
	// to gets its standard output as it is, instead of line
	to io.Writer
	// errLine gets each line of its error output, before the log does
	errLine func(line string)
}

// stream runs restic like run, with what it prints handed on as it comes
// rather than kept: a restore's lines, or a dump's bytes. It keeps run's
// global options, its stop on cancel and what restic said.
func (r restic) stream(ctx context.Context, options streamOptions, args ...string) (int, string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	full := r.globals()
	full = append(full, args...)
	command := exec.CommandContext(ctx, "restic", full...) //nolint:gosec // restic with the job's repository
	command.Cancel = func() error { return command.Process.Signal(gracefulStop) }
	command.WaitDelay = resticStopWait
	command.Env = r.environ(options.env)
	tail := &tailBuffer{limit: streamTail}
	log := &readableWriter{w: r.log}
	errWriters := []io.Writer{tail}
	if options.errLine != nil {
		errWriters = append(errWriters, &lineWriter{line: options.errLine})
	}
	command.Stderr = io.MultiWriter(append(errWriters, log)...)
	var scanErr error
	switch {
	case options.to != nil:
		command.Stdout = options.to
		scanErr = command.Start()
	default:
		stdout, err := command.StdoutPipe()
		if err != nil {
			return -1, "", err
		}
		if err := command.Start(); err != nil {
			return -1, "", err
		}
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			if options.line != nil {
				options.line(scanner.Bytes())
			}
		}
		if scanErr = scanner.Err(); scanErr != nil {
			// restic waits on a pipe nobody reads any more
			cancel()
		}
	}
	if scanErr != nil && command.Process == nil {
		return -1, "", scanErr
	}
	err := command.Wait()
	log.Flush()
	if scanErr != nil {
		return -1, "", fmt.Errorf("restic's output cannot be read: %w", scanErr)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && ctx.Err() == nil {
		said := r.said([]byte(tail.String()))
		if exit.ExitCode() == resticLocked {
			r.sawLocked(tail.String())
		}
		if line := lockLine([]byte(tail.String())); exit.ExitCode() == resticLocked && line != "" {
			said = backup.Redact(line, r.secrets...)
		}
		return exit.ExitCode(), said, nil
	}
	if err != nil {
		return -1, "", err
	}
	return 0, "", nil
}

// globals are restic's global options for every run: its cache, or none,
// and the repository's options.
func (r restic) globals() []string {
	full := []string{"--no-cache"}
	if r.cache != "" {
		// what restic has not used for 30 days goes, as a removed target's
		full = []string{"--cache-dir", r.cache, "--cleanup-cache"}
	}
	for _, option := range r.options {
		full = append(full, "-o", option)
	}
	return full
}

// environ is the process's environment with the repository's variables and
// more.
func (r restic) environ(more map[string]string) []string {
	env := os.Environ()
	for _, name := range slices.Sorted(maps.Keys(r.env)) {
		env = append(env, name+"="+r.env[name])
	}
	for _, name := range slices.Sorted(maps.Keys(more)) {
		env = append(env, name+"="+more[name])
	}
	return env
}

// lines runs restic and hands each line of its standard output to onLine.
func (r restic) lines(ctx context.Context, onLine func(line []byte), env map[string]string, args ...string) (int, string, error) {
	return r.stream(ctx, streamOptions{env: env, line: onLine}, args...)
}

// to runs restic with its standard output going to w as it is.
func (r restic) to(ctx context.Context, w io.Writer, args ...string) (int, string, error) {
	return r.stream(ctx, streamOptions{to: w}, args...)
}

// lineWriter hands each line written to it on to line.
type lineWriter struct {
	line    func(line string)
	partial []byte
}

func (l *lineWriter) Write(data []byte) (int, error) {
	l.partial = append(l.partial, data...)
	for {
		end := bytes.IndexByte(l.partial, '\n')
		if end < 0 {
			return len(data), nil
		}
		l.line(string(bytes.TrimRight(l.partial[:end], "\r")))
		l.partial = l.partial[end+1:]
	}
}

// checkVersion refuses a restic older than the one whose restore, ls and
// dump the tool reads.
func (r restic) checkVersion(ctx context.Context) error {
	major, minor, version, err := r.version(ctx)
	if err != nil {
		return err
	}
	if major < minResticMajor || major == minResticMajor && minor < minResticMinor {
		return fmt.Errorf("restic %s is too old to restore: the agent's image needs restic %d.%d or later", version, minResticMajor, minResticMinor)
	}
	return nil
}

// version is restic's version, such as 0.17.3, and its major and minor.
func (r restic) version(ctx context.Context) (int, int, string, error) {
	out, code, said, err := r.run(ctx, "version")
	if err != nil {
		return 0, 0, "", err
	}
	if code != 0 {
		return 0, 0, "", fmt.Errorf("restic's version cannot be read: %s", said)
	}
	found := resticVersion.FindStringSubmatch(string(out))
	if found == nil {
		return 0, 0, "", fmt.Errorf("restic's version cannot be read: %s", strings.TrimSpace(string(out)))
	}
	major, _ := strconv.Atoi(found[1])
	minor, _ := strconv.Atoi(found[2])
	return major, minor, found[1] + "." + found[2] + "." + found[3], nil
}

// resticSnapshot is a snapshot as restic snapshots --json says it.
type resticSnapshot struct {
	ID             string    `json:"id"`
	ShortID        string    `json:"short_id"`
	Time           time.Time `json:"time"`
	Hostname       string    `json:"hostname"`
	Paths          []string  `json:"paths"`
	Tags           []string  `json:"tags"`
	ProgramVersion string    `json:"program_version"`
	Summary        *struct {
		DataAdded           int64 `json:"data_added"`
		TotalFilesProcessed int64 `json:"total_files_processed"`
		TotalBytesProcessed int64 `json:"total_bytes_processed"`
	} `json:"summary"`
}

// snapshotsResult is what the snapshots op found.
type snapshotsResult struct {
	Snapshots []snapshotItem `json:"snapshots"`
	Total     int            `json:"total"`
}

// snapshotItem is one snapshot the snapshots op lists.
type snapshotItem struct {
	ID             string   `json:"id"`
	ShortID        string   `json:"short_id"`
	Time           string   `json:"time"`
	Hostname       string   `json:"hostname"`
	Paths          []string `json:"paths"`
	Tags           []string `json:"tags"`
	SizeBytes      *int64   `json:"size_bytes,omitempty"`
	AddedBytes     *int64   `json:"added_bytes,omitempty"`
	Files          *int64   `json:"files,omitempty"`
	ProgramVersion string   `json:"program_version,omitempty"`
}

// maxSnapshotPaths is how many of a snapshot's paths the list says.
const maxSnapshotPaths = 20

// checkTags checks the tags a list or a restore goes by.
func checkTags(tags []string) error {
	for _, tag := range tags {
		// a comma would split it, as restic reads one
		if tag == "" || strings.ContainsAny(tag, ", \n\r\x00") {
			return fmt.Errorf("a tag cannot be %q", tag)
		}
	}
	return nil
}

// snapshots lists the snapshots that have every tag, newest first.
func (r restic) snapshots(ctx context.Context, tags []string, limit int) (snapshotsResult, error) {
	result := snapshotsResult{Snapshots: []snapshotItem{}}
	if err := checkTags(tags); err != nil {
		return result, err
	}
	args := []string{"--no-lock", "snapshots", "--json"}
	if len(tags) > 0 {
		// one --tag with commas: snapshots with every tag
		args = append(args, "--tag", strings.Join(tags, ","))
	}
	out, code, said, err := r.run(ctx, args...)
	if err != nil {
		return result, err
	}
	if code != 0 {
		return result, repositoryError(code, said)
	}
	var found []resticSnapshot
	if err := json.Unmarshal(lastLine(out), &found); err != nil {
		return result, fmt.Errorf("restic's snapshots cannot be read: %s", strings.TrimSpace(backup.Redact(string(out), r.secrets...)))
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].Time.After(found[j].Time) })
	result.Total = len(found)
	for _, snapshot := range found[:min(len(found), limit)] {
		item := snapshotItem{
			ID: snapshot.ID, ShortID: snapshot.ShortID, Time: snapshot.Time.UTC().Format(time.RFC3339),
			Hostname: snapshot.Hostname, Paths: snapshot.Paths[:min(len(snapshot.Paths), maxSnapshotPaths)],
			Tags: snapshot.Tags, ProgramVersion: snapshot.ProgramVersion,
		}
		if item.Paths == nil {
			item.Paths = []string{}
		}
		if item.Tags == nil {
			item.Tags = []string{}
		}
		if summary := snapshot.Summary; summary != nil {
			// only a snapshot of restic 0.17 or later says what it saved
			item.SizeBytes, item.AddedBytes, item.Files = &summary.TotalBytesProcessed, &summary.DataAdded, &summary.TotalFilesProcessed
		}
		result.Snapshots = append(result.Snapshots, item)
	}
	return result, nil
}

// snapshot finds one snapshot by its full id, and checks that it has tag.
func (r restic) snapshot(ctx context.Context, id, tag string) (resticSnapshot, error) {
	var snapshot resticSnapshot
	out, code, said, err := r.run(ctx, "--no-lock", "snapshots", "--json", id)
	if err != nil {
		return snapshot, err
	}
	if code != 0 {
		return snapshot, repositoryError(code, said)
	}
	var found []resticSnapshot
	if err := json.Unmarshal(lastLine(out), &found); err != nil {
		return snapshot, fmt.Errorf("restic's snapshots cannot be read: %s", strings.TrimSpace(backup.Redact(string(out), r.secrets...)))
	}
	for _, candidate := range found {
		if candidate.ID == id {
			snapshot = candidate
		}
	}
	if snapshot.ID == "" {
		return snapshot, fmt.Errorf("the snapshot %.8s is not in the repository: it may have been forgotten by the job's retention", id)
	}
	if tag != "" && !slices.Contains(snapshot.Tags, tag) {
		return snapshot, fmt.Errorf("the snapshot %.8s is not one of this job's: it has no tag %s", id, tag)
	}
	return snapshot, nil
}

// resticNode is one line of restic ls --json: the snapshot, or a node.
type resticNode struct {
	MessageType string      `json:"message_type"`
	StructType  string      `json:"struct_type"`
	Name        string      `json:"name"`
	Type        string      `json:"type"`
	Path        string      `json:"path"`
	UID         uint32      `json:"uid"`
	GID         uint32      `json:"gid"`
	Size        int64       `json:"size"`
	Mode        fs.FileMode `json:"mode"`
	MTime       time.Time   `json:"mtime"`
}

// node says whether a line is a node rather than the snapshot.
func (n *resticNode) node() bool {
	return n.MessageType == "node" || n.StructType == "node"
}

// unixMode is a node's permission bits as the server's chmod takes them:
// its rwx bits, setuid, setgid and the sticky bit.
func unixMode(mode fs.FileMode) uint32 {
	bits := uint32(mode.Perm())
	if mode&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if mode&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if mode&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

// lsResult is one page of a snapshot's folder.
type lsResult struct {
	Path    string    `json:"path"`
	Total   int       `json:"total"`
	Dirs    int       `json:"dirs"`
	Files   int       `json:"files"`
	Offset  int       `json:"offset"`
	Entries []lsEntry `json:"entries"`
}

// lsEntry is one entry of a folder.
type lsEntry struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Size    int64  `json:"size"`
	MTime   string `json:"mtime"`
	Mode    uint32 `json:"mode"`
	BadName bool   `json:"bad_name,omitempty"`
}

// errTooManyEntries is a folder too large to list whole.
var errTooManyEntries = errors.New("the folder holds more than a million items: filter it")

// checkSnapshotPath checks an absolute path of a snapshot.
func checkSnapshotPath(p string) error {
	if !path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, "\n\r\x00") || len(p) > 4096 {
		return fmt.Errorf("a path of the snapshot is absolute and clean, not %q", p)
	}
	return nil
}

// ls lists one page of a folder of a snapshot.
func (r restic) ls(ctx context.Context, id, dir string, offset, limit int, match string) (lsResult, error) {
	result := lsResult{Path: dir, Offset: offset, Entries: []lsEntry{}}
	if !snapshotID.MatchString(id) {
		return result, fmt.Errorf("a snapshot is named by its full id, not %q", id)
	}
	if err := checkSnapshotPath(dir); err != nil {
		return result, err
	}
	if err := r.checkVersion(ctx); err != nil {
		return result, err
	}
	match = strings.ToLower(match)
	var entries []lsEntry
	found := dir == "/"
	var kind string
	var tooMany bool
	code, said, err := r.lines(ctx, func(line []byte) {
		var node resticNode
		if tooMany || json.Unmarshal(line, &node) != nil || !node.node() {
			return
		}
		if node.Path == dir {
			found, kind = true, node.Type
			return
		}
		if path.Dir(node.Path) != dir || match != "" && !strings.Contains(strings.ToLower(node.Name), match) {
			return
		}
		if len(entries) >= maxListed {
			tooMany = true
			return
		}
		entries = append(entries, lsEntryOf(node))
	}, nil, "--no-lock", "ls", "--json", id, dir)
	if err != nil {
		return result, err
	}
	if code != 0 {
		return result, repositoryError(code, said)
	}
	switch {
	case tooMany:
		return result, errTooManyEntries
	case !found:
		return result, fmt.Errorf("%s is not in the snapshot", dir)
	case kind != "" && kind != "dir":
		return result, fmt.Errorf("%s is a file of the snapshot, not a folder", dir)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if (entries[i].Type == "dir") != (entries[j].Type == "dir") {
			return entries[i].Type == "dir"
		}
		return entries[i].Name < entries[j].Name
	})
	for _, entry := range entries {
		if entry.Type == "dir" {
			result.Dirs++
		} else {
			result.Files++
		}
	}
	result.Total = len(entries)
	if offset < len(entries) {
		result.Entries = entries[offset:min(offset+limit, len(entries))]
	}
	return result, nil
}

// lsEntryOf is a node as ls says it: a name of at most maxNameBytes, which a
// name that was not valid UTF-8 is marked bad in, as restic printed U+FFFD
// for it.
func lsEntryOf(node resticNode) lsEntry {
	name := node.Name
	if len(name) > maxNameBytes {
		cut := maxNameBytes
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = name[:cut]
	}
	return lsEntry{
		Name: name, Type: node.Type, Size: node.Size, Mode: unixMode(node.Mode),
		MTime: node.MTime.UTC().Format(time.RFC3339), BadName: strings.ContainsRune(node.Name, utf8.RuneError),
	}
}
