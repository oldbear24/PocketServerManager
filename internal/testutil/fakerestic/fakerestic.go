// Package fakerestic puts a fake restic on PATH for tests of backups (#28).
// It answers the commands the backup tool runs, keeps its repositories in a
// directory of the test, and logs every call with restic's variables. Its
// errors repeat the credentials it was given, as a real one's might repeat a
// URL, so that tests see they are taken out before anyone reads them.
package fakerestic

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
)

// Wrong in a secret access key or a rest-server password makes the storage
// refuse it, as with credentials that are not the bucket's.
const Wrong = "wrong"

// script is restic as far as the backup tool uses it: cat config, init and
// stats, behind the global options --no-cache, --cache-dir, --no-lock and -o. A
// repository whose URL is a directory lives in it, like restic's local
// backend; the others live in the test's directory.
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
case "$1" in
cat)
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
*)
	echo "Fatal: the fake restic knows no command $1" >&2
	exit 1;;
esac
`

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
type Tool func(ctx context.Context, args []string, env map[string]string, stdout, stderr io.Writer) error

// RunJobs makes a fake engine run backup jobs with the backup tool, in this
// process, as the job container would: a local target's directory takes the
// place of /repo, as the bind mount does, and the tool's error ends its
// output.
func RunJobs(engine *fakedocker.Engine, tool Tool) {
	engine.OnJob = func(job fakedocker.Job) fakedocker.JobResult {
		if len(job.Cmd) == 0 || job.Cmd[0] != "backup-tool" {
			return fakedocker.JobResult{Code: 1, Stderr: "error: not a backup job"}
		}
		env := Env(job.Env)
		args := slices.Clone(job.Cmd[1:])
		mounts, _ := job.Host["Mounts"].([]any)
		for _, raw := range mounts {
			mount, _ := raw.(map[string]any)
			if mount["Target"] == "/repo" {
				source, _ := mount["Source"].(string)
				// the Engine makes the directory, as CreateMountpoint asks
				if options, _ := mount["BindOptions"].(map[string]any); options["CreateMountpoint"] == true {
					if err := os.MkdirAll(source, 0o755); err != nil { //nolint:gosec // as the Engine makes it
						return fakedocker.JobResult{Code: 125, Stderr: err.Error()}
					}
				}
				for i, arg := range args {
					if arg == "/repo" {
						args[i] = source
					}
				}
				if env["RESTIC_REPOSITORY"] == "/repo" {
					env["RESTIC_REPOSITORY"] = source
				}
			}
		}
		var stdout, stderr bytes.Buffer
		if err := tool(context.Background(), args, env, &stdout, &stderr); err != nil {
			fmt.Fprintln(&stderr, "error:", err)
			return fakedocker.JobResult{Code: 1, Stdout: stdout.String(), Stderr: stderr.String()}
		}
		return fakedocker.JobResult{Stdout: stdout.String(), Stderr: stderr.String()}
	}
}
