// Package fakerestic puts a fake restic on PATH for tests of backups (#28).
// It answers the commands the backup tool runs, keeps its repositories in a
// directory of the test, and logs every call with restic's variables. Its
// errors repeat the credentials it was given, as a real one's might repeat a
// URL, so that tests see they are taken out before anyone reads them.
//
// A test binary that imports it also runs the dump's relay (#29) when the
// fake restic runs it, as the backup tool, running in the test's process,
// names the test binary for restic's --stdin-from-command.
package fakerestic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
)

// Wrong in a secret access key or a rest-server password makes the storage
// refuse it, as with credentials that are not the bucket's.
const Wrong = "wrong"

// script is restic as far as the backup tool uses it: cat config, init,
// stats, backup, forget and snapshots, behind the global options --no-cache,
// --cache-dir, --no-lock and -o. A repository whose URL is a directory lives
// in it, like restic's local backend; the others live in the test's
// directory.
//
// A snapshot is a directory of the repository's snapshots, numbered in the
// order they were made, with the files it saved and its JSON as restic's
// snapshots --json prints it. backup reads the directories it is given, or
// where FAKE_RESTIC_MOUNTS (lines of job path=directory, which RunJobs sets)
// says they are, and counts as new or changed the files that differ from
// the newest snapshot of the same host and paths. A file whose name is
// FAKE_RESTIC_UNREADABLE cannot be read: the snapshot is saved without it,
// with exit code 3. --stdin-from-command saves what the command printed,
// and nothing when it fails. forget groups by tags only; a snapshot matches
// a --tag a,b that it has every tag of, and any of several --tag. Every
// --keep-X N keeps the newest N snapshots of a group, as --keep-last does:
// together they keep as many as the largest. A forgotten snapshot moves to
// the repository's forgotten directory. FAKE_RESTIC_FORGET_FAIL makes forget
// fail with its text.
const script = `#!/bin/sh
root="${FAKE_RESTIC_ROOT:?}"
{ echo "restic $*"; env | grep -E '^(RESTIC|AWS)_' | sort; echo; } >> "$root/calls"
case "$AWS_SECRET_ACCESS_KEY$RESTIC_REST_PASSWORD" in
*wrong*)
	echo "Fatal: unable to open config file: Stat: Access Denied for key $AWS_SECRET_ACCESS_KEY$RESTIC_REST_PASSWORD" >&2
	echo "Is there a repository at the following location?" >&2
	echo "$RESTIC_REPOSITORY" >&2
	exit 1;;
esac
while [ $# -gt 0 ]; do
	case "$1" in
	--no-cache|--no-lock|--cleanup-cache) shift;;
	-o|--cache-dir) shift 2;;
	*) break;;
	esac
done
case "$RESTIC_REPOSITORY" in
/*) repo="$RESTIC_REPOSITORY";;
*) repo="$root/repos/$(printf %s "$RESTIC_REPOSITORY" | tr -c 'A-Za-z0-9' '_')";;
esac

# open_repo: the repository is there, and the password is its own
open_repo() {
	# a bucket named nobucket is not there until init makes it, as MinIO says
	case "$RESTIC_REPOSITORY" in
	*/nobucket*)
		if [ ! -d "$repo" ]; then
			echo "Fatal: unable to open config file: Stat: The specified bucket does not exist" >&2
			echo "Is there a repository at the following location?" >&2
			exit 1
		fi;;
	esac
	if [ ! -f "$repo/config" ]; then
		echo "Fatal: repository does not exist: unable to open config file: stat $repo/config: no such file or directory" >&2
		exit 10
	fi
	if [ "$(cat "$repo/keys/password")" != "$RESTIC_PASSWORD" ]; then
		echo "Fatal: wrong password or no key found" >&2
		exit 12
	fi
	mkdir -p "$repo/snapshots" "$repo/forgotten" "$repo/aborted"
}

# matches: the snapshot whose tags file is $1 matches the --tag filters that follow
matches() {
	tagfile=$1
	shift
	[ $# -eq 0 ] && return 0
	for filter in "$@"; do
		all=1
		for tag in $(printf %s "$filter" | tr , ' '); do
			grep -qxF -- "$tag" "$tagfile" || all=
		done
		[ -n "$all" ] && return 0
	done
	return 1
}

# snapshots: the repository's finished snapshots, oldest first
snapshots() {
	for name in $(ls "$repo/snapshots"); do
		[ -f "$repo/snapshots/$name/json" ] && echo "$name"
	done
}

# located: where the fake reads a path of the job
located() {
	if [ -n "$FAKE_RESTIC_MOUNTS" ]; then
		while IFS= read -r mount; do
			target=${mount%%=*}
			case "$1" in
			"$target"|"$target"/*) printf '%s%s' "${mount#*=}" "${1#"$target"}"; return;;
			esac
		done <<MOUNTS
$FAKE_RESTIC_MOUNTS
MOUNTS
	fi
	printf %s "$1"
}

# json_list: the lines of a file as a JSON list of strings
json_list() {
	sed 's/.*/"&"/' "$1" | paste -sd, -
}

case "$1" in
cat)
	open_repo
	cat "$repo/config";;
init)
	if [ -f "$repo/config" ]; then
		echo "Fatal: create repository at $RESTIC_REPOSITORY failed: config file already exists" >&2
		exit 1
	fi
	mkdir -p "$repo/keys"
	printf %s "$RESTIC_PASSWORD" > "$repo/keys/password"
	id=$(printf %s "$RESTIC_REPOSITORY" | sha256sum | cut -c1-64)
	printf '{"version":2,"id":"%s","chunker_polynomial":"1"}\n' "$id" > "$repo/config"
	if [ "$2" = --json ]; then
		printf '{"message_type":"initialized","id":"%s","repository":"%s"}\n' "$id" "$RESTIC_REPOSITORY"
	else
		echo "created restic repository at $RESTIC_REPOSITORY"
	fi;;
stats)
	if [ -n "$FAKE_RESTIC_STATS_FAIL" ]; then
		echo "Fatal: $FAKE_RESTIC_STATS_FAIL" >&2
		exit 1
	fi
	printf '{"total_size":%s,"snapshots_count":0}\n' "${FAKE_RESTIC_SIZE:-0}";;
backup)
	shift
	host=fake-job-container
	tags=
	stdin=
	name=stdin
	while [ $# -gt 0 ]; do
		case "$1" in
		--json|--quiet|-q) shift;;
		--retry-lock) shift 2;;
		--host) host=$2; shift 2;;
		--tag) tags="$tags,$2"; shift 2;;
		--stdin-from-command) stdin=1; shift;;
		--stdin-filename) name=$2; shift 2;;
		--) shift; break;;
		*) echo "Fatal: the fake restic knows no backup flag $1" >&2; exit 1;;
		esac
	done
	open_repo
	last=$(snapshots | tail -n 1)
	n=$(expr "${last:-0}" + 1)
	while ! mkdir "$repo/snapshots/$(printf %08d "$n")" 2>/dev/null; do n=$(expr "$n" + 1); done
	snap="$repo/snapshots/$(printf %08d "$n")"
	mkdir "$snap/files"
	: > "$snap/paths"
	partial=
	if [ -n "$stdin" ]; then
		said=$("$@" 2>&1 > "$snap/files/$name")
		code=$?
		[ -n "$said" ] && printf '%s\n' "$said" | sed "s|^|subprocess $1: |" >&2
		if [ "$code" -ne 0 ]; then
			echo "{\"message_type\":\"error\",\"error\":{\"message\":\"failed to save /$name: Fatal: command failed: exit status $code\"},\"during\":\"archival\",\"item\":\"/$name\"}" >&2
			echo "Fatal: unable to save snapshot: failed to save /$name: Fatal: command failed: exit status $code" >&2
			mv "$snap" "$repo/aborted/$n-$$"
			exit 1
		fi
		echo "/$name" > "$snap/paths"
	else
		for p in "$@"; do
			from=$(located "$p")
			if [ ! -e "$from" ]; then
				echo "$p does not exist, skipping" >&2
				continue
			fi
			mkdir -p "$snap/files$(dirname "$p")"
			cp -R "$from" "$snap/files$p"
			echo "$p" >> "$snap/paths"
		done
		if [ ! -s "$snap/paths" ]; then
			echo "Fatal: all source directories/files do not exist" >&2
			mv "$snap" "$repo/aborted/$n-$$"
			exit 1
		fi
		if [ -n "$FAKE_RESTIC_UNREADABLE" ]; then
			unread=$(find "$snap/files" -type f -name "$FAKE_RESTIC_UNREADABLE")
			if [ -n "$unread" ]; then
				partial=1
				printf '%s\n' "$unread" | while IFS= read -r file; do
					item=${file#"$snap/files"}
					echo "{\"message_type\":\"error\",\"error\":{\"message\":\"open $item: permission denied\"},\"during\":\"archival\",\"item\":\"$item\"}" >&2
				done
				find "$snap/files" -type f -name "$FAKE_RESTIC_UNREADABLE" -delete
			fi
		fi
	fi
	printf %s "$tags" | tr , '\n' | sed '/^$/d' > "$snap/tags"
	echo "$host" > "$snap/host"
	# what differs from the newest snapshot of the same host and paths
	parent=
	for other in $(snapshots); do
		o="$repo/snapshots/$other"
		[ "$o" != "$snap" ] && [ "$(cat "$o/host")" = "$host" ] && cmp -s "$o/paths" "$snap/paths" && parent=$o
	done
	new=0; changed=0; unmodified=0; added=0; total=0; count=0
	list=$(cd "$snap/files" && find . -type f)
	while IFS= read -r file; do
		[ -n "$file" ] || continue
		size=$(wc -c < "$snap/files/$file")
		count=$((count + 1)); total=$((total + size))
		if [ -n "$parent" ] && [ -f "$parent/files/$file" ]; then
			if cmp -s "$snap/files/$file" "$parent/files/$file"; then
				unmodified=$((unmodified + 1))
			else
				changed=$((changed + 1)); added=$((added + size))
			fi
		else
			new=$((new + 1)); added=$((added + size))
		fi
	done <<FILES
$list
FILES
	id=$(printf '%s %s %s' "$repo" "$n" "$(date +%s%N)" | sha256sum | cut -c1-64)
	printf '{"time":"%s","paths":[%s],"hostname":"%s","tags":[%s],"id":"%s","short_id":"%.8s"}\n' \
		"$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$(json_list "$snap/paths")" "$host" "$(json_list "$snap/tags")" "$id" "$id" > "$snap/json.new"
	mv "$snap/json.new" "$snap/json"
	printf '{"message_type":"summary","files_new":%d,"files_changed":%d,"files_unmodified":%d,"dirs_new":0,"dirs_changed":0,"dirs_unmodified":0,"data_blobs":%d,"tree_blobs":1,"data_added":%d,"data_added_packed":%d,"total_files_processed":%d,"total_bytes_processed":%d,"total_duration":0.01,"snapshot_id":"%s"}\n' \
		"$new" "$changed" "$unmodified" "$((new + changed))" "$added" "$added" "$count" "$total" "$id"
	if [ -n "$partial" ]; then
		echo "Warning: at least one source file could not be read" >&2
		exit 3
	fi;;
forget)
	shift
	keep=0
	filters=
	while [ $# -gt 0 ]; do
		case "$1" in
		--json|--quiet|-q|--prune) shift;;
		--retry-lock) shift 2;;
		--group-by)
			if [ "$2" != tags ]; then
				echo "Fatal: the fake restic groups by tags only, not $2" >&2
				exit 1
			fi
			shift 2;;
		--tag) filters="$filters $2"; shift 2;;
		--keep-last|--keep-hourly|--keep-daily|--keep-weekly|--keep-monthly|--keep-yearly)
			[ "$2" -gt "$keep" ] && keep=$2
			shift 2;;
		*) echo "Fatal: the fake restic knows no forget flag $1" >&2; exit 1;;
		esac
	done
	open_repo
	if [ -n "$FAKE_RESTIC_FORGET_FAIL" ]; then
		echo "Fatal: $FAKE_RESTIC_FORGET_FAIL" >&2
		exit 1
	fi
	if [ "$keep" -eq 0 ]; then
		echo "no policy was specified, no snapshots will be removed"
		exit 0
	fi
	# each matching snapshot, newest first, after its group: its tags, sorted
	groups=
	for name in $(snapshots | sort -r); do
		matches "$repo/snapshots/$name/tags" $filters || continue
		groups="$groups$(sort "$repo/snapshots/$name/tags" | paste -sd, -) $name
"
	done
	[ -n "$groups" ] || exit 0
	removed=0
	out="["
	for group in $(printf %s "$groups" | cut -d' ' -f1 | sort -u); do
		kept=; gone=; i=0
		for name in $(printf %s "$groups" | grep -F -- "$group " | cut -d' ' -f2); do
			[ "$(sort "$repo/snapshots/$name/tags" | paste -sd, -)" = "$group" ] || continue
			i=$((i + 1))
			if [ "$i" -le "$keep" ]; then
				kept="$kept${kept:+,}$(cat "$repo/snapshots/$name/json")"
			else
				gone="$gone${gone:+,}$(cat "$repo/snapshots/$name/json")"
				mv "$repo/snapshots/$name" "$repo/forgotten/$name"
				removed=$((removed + 1))
			fi
		done
		[ "$out" = "[" ] || out="$out,"
		tagjson=$(printf %s "$group" | tr , '\n' | sed 's/.*/"&"/' | paste -sd, -)
		out="$out{\"tags\":[$tagjson],\"host\":\"\",\"paths\":null,\"keep\":[$kept],\"remove\":${gone:+[}${gone:-null}${gone:+]},\"reasons\":[]}"
	done
	echo "$out]"
	if [ "$removed" -gt 0 ]; then
		echo "loading indexes..."
		echo "to delete: the data of $removed snapshots"
		echo "done"
	fi;;
snapshots)
	shift
	filters=
	while [ $# -gt 0 ]; do
		case "$1" in
		--json|--quiet|-q) shift;;
		--tag) filters="$filters $2"; shift 2;;
		*) echo "Fatal: the fake restic knows no snapshots flag $1" >&2; exit 1;;
		esac
	done
	open_repo
	out=
	for name in $(snapshots); do
		matches "$repo/snapshots/$name/tags" $filters || continue
		out="$out${out:+,}$(cat "$repo/snapshots/$name/json")"
	done
	echo "[$out]";;
*)
	echo "Fatal: the fake restic knows no command $1" >&2
	exit 1;;
esac
`

// init runs the dump's relay when the fake restic runs this test binary for
// restic's --stdin-from-command, as the backup tool asks it to, in place of
// the agent's binary in a job container.
func init() {
	if len(os.Args) < 3 || os.Args[1] != "backup-tool" || os.Args[2] != "relay" {
		return
	}
	socket := ""
	for i, arg := range os.Args {
		if arg == "--socket" && i+1 < len(os.Args) {
			socket = os.Args[i+1]
		}
	}
	os.Exit(backup.RelaySocket(socket, os.Stdout, os.Stderr))
}

// Install puts the fake restic first on PATH for the rest of the test and
// returns the directory that holds its repositories and its log. Tests that
// use it cannot run in parallel.
func Install(t testing.TB) string {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(filepath.Join(root, "repos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "restic"), []byte(script), 0o755); err != nil { //nolint:gosec // a script
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_RESTIC_ROOT", root)
	return root
}

// Calls returns what the fake restic was run with: each call's arguments and
// restic's variables, in order.
func Calls(t testing.TB, root string) string {
	t.Helper()
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(calls)
}

// Repositories counts the repositories the fake restic created in its own
// directory.
func Repositories(t testing.TB, root string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "repos"))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// Snapshot is a snapshot the fake restic keeps.
type Snapshot struct {
	ID       string   `json:"id"`
	Time     string   `json:"time"`
	Paths    []string `json:"paths"`
	Hostname string   `json:"hostname"`
	Tags     []string `json:"tags"`
	// Files holds what it saved: each file's path, as restic names it, and
	// its content.
	Files map[string]string `json:"-"`
}

// Snapshots returns the snapshots a repository of the fake restic holds,
// oldest first, without those forget took. repository is the job's
// RESTIC_REPOSITORY, or a local target's directory.
func Snapshots(t testing.TB, root, repository string) []Snapshot {
	t.Helper()
	dir := filepath.Join(Repository(root, repository), "snapshots")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		t.Fatal(err)
	}
	var snapshots []Snapshot
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name(), "json"))
		if err != nil {
			continue // still being made
		}
		var snapshot Snapshot
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			t.Fatalf("snapshot %s: %v", entry.Name(), err)
		}
		snapshot.Files = map[string]string{}
		files := filepath.Join(dir, entry.Name(), "files")
		err = filepath.WalkDir(files, func(file string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			content, err := os.ReadFile(file) //nolint:gosec // the fake's own snapshot
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(files, file)
			snapshot.Files["/"+filepath.ToSlash(rel)] = string(content)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots
}

// Repository is the directory of a repository of the fake restic: a local
// one's own, or one in its root named after the URL.
func Repository(root, repository string) string {
	if strings.HasPrefix(repository, "/") {
		return repository
	}
	name := []byte(repository)
	for i, c := range name {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			name[i] = '_'
		}
	}
	return filepath.Join(root, "repos", string(name))
}

// Env turns a container's environment into the backup tool's.
func Env(entries []string) map[string]string {
	env := map[string]string{}
	for _, entry := range entries {
		if name, value, ok := strings.Cut(entry, "="); ok {
			env[name] = value
		}
	}
	return env
}

// Tool is the backup tool's entry point, agent.BackupTool.
type Tool func(ctx context.Context, args []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) error

// RunJobs makes a fake engine run backup jobs with the backup tool, in this
// process, as the job container would: a local target's directory takes the
// place of /repo, as the bind mount does; the fake restic reads a volume
// from the engine's VolumeDir and a server's directory from itself; the
// dump's relay listens in a directory of its own, as each job has its own
// /tmp; and the job's attached input is the tool's. The volumes and
// directories a job backs up must be mounted read-only. The tool's error
// ends its output.
func RunJobs(engine *fakedocker.Engine, tool Tool) {
	engine.OnJob = func(job fakedocker.Job) fakedocker.JobResult {
		if len(job.Cmd) == 0 || job.Cmd[0] != "backup-tool" {
			return fakedocker.JobResult{Code: 1, Stderr: "error: not a backup job"}
		}
		env := Env(job.Env)
		args := slices.Clone(job.Cmd[1:])
		var sources []string
		mounts, _ := job.Host["Mounts"].([]any)
		for _, raw := range mounts {
			mount, _ := raw.(map[string]any)
			target, _ := mount["Target"].(string)
			source, _ := mount["Source"].(string)
			switch {
			case target == backup.LocalMount:
				// the Engine makes the directory, as CreateMountpoint asks
				if options, _ := mount["BindOptions"].(map[string]any); options["CreateMountpoint"] == true {
					if err := os.MkdirAll(source, 0o755); err != nil { //nolint:gosec // as the Engine makes it
						return fakedocker.JobResult{Code: 125, Stderr: err.Error()}
					}
				}
				for i, arg := range args {
					if arg == backup.LocalMount {
						args[i] = source
					}
				}
				if env["RESTIC_REPOSITORY"] == backup.LocalMount {
					env["RESTIC_REPOSITORY"] = source
				}
			case mount["Type"] == "volume" && strings.HasPrefix(target, "/volumes/"), mount["Type"] == "bind":
				if mount["ReadOnly"] != true {
					return fakedocker.JobResult{Code: 125, Stderr: "error: a backup job mounts " + target + " writable"}
				}
				if mount["Type"] == "volume" {
					source = engine.VolumeDir(source)
				}
				sources = append(sources, target+"="+source)
			}
		}
		env["FAKE_RESTIC_MOUNTS"] = strings.Join(sources, "\n")
		if i := slices.Index(args, "--relay"); i >= 0 && i+1 < len(args) {
			dir, err := os.MkdirTemp("", "relay")
			if err != nil {
				return fakedocker.JobResult{Code: 125, Stderr: err.Error()}
			}
			defer os.Remove(dir)
			args[i+1] = filepath.Join(dir, "dump.sock")
		}
		var stdin io.Reader = strings.NewReader("")
		if job.Stdin != nil {
			stdin = job.Stdin
		}
		var stdout, stderr bytes.Buffer
		if err := tool(context.Background(), args, env, stdin, &stdout, &stderr); err != nil {
			fmt.Fprintln(&stderr, "error:", err)
			return fakedocker.JobResult{Code: 1, Stdout: stdout.String(), Stderr: stderr.String()}
		}
		return fakedocker.JobResult{Stdout: stdout.String(), Stderr: stderr.String()}
	}
}
