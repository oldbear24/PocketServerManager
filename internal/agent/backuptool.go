package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"

	"github.com/oldbear24/PocketServerManager/internal/backup"
)

// The backup tool runs restic for a backup command (#28): it runs as
// `pocketcloud-agent backup-tool <op>` in a job container of the agent's own
// image, which holds restic, and finds the repository in its environment:
// RESTIC_REPOSITORY, RESTIC_PASSWORD and the storage's credentials, which the
// panel added to the command when it delivered it. It writes what restic says
// to stderr, with those secrets taken out, and last its result as one line of
// JSON to stdout.
//
//	init   open the repository, or create it where there is none, and say
//	       its id. --usage: say how much it holds too, which reads every
//	       snapshot and takes a while on a large repository. --local DIR:
//	       the repository is the directory DIR, whose room left it says; a
//	       directory that holds other files and no repository is refused.
//	       --expect ID: the repository is the one with this id, which is only
//	       opened; a missing or another one is an error.
//
// Every op takes restic's options as -o key=value, and --cache DIR for
// restic's cache; without it restic keeps none.

// The environment of the backup tool's job: restic's own variables.
const (
	envResticRepository = "RESTIC_REPOSITORY"
	envResticPassword   = "RESTIC_PASSWORD"
)

// restic's exit codes, since 0.17
const (
	resticNoRepository  = 10
	resticWrongPassword = 12
)

// backupInitResult is what the init op found.
type backupInitResult struct {
	// Created says the repository is new.
	Created bool   `json:"created"`
	ID      string `json:"id"`
	// Used is what the repository's data takes, compressed: measured when
	// asked, and nothing in a new one.
	Used *int64 `json:"used_bytes,omitempty"`
	// Free is the room left on a local target's file system.
	Free *int64 `json:"free_bytes,omitempty"`
}

// BackupToolEnv is what the backup tool takes of its job's environment: restic's
// variables and the storage's credentials.
func BackupToolEnv(environ []string) map[string]string {
	env := map[string]string{}
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if ok && (strings.HasPrefix(name, "RESTIC_") || strings.HasPrefix(name, "AWS_")) {
			env[name] = value
		}
	}
	return env
}

// options is a repeated -o key=value flag.
type options []string

func (o *options) String() string { return strings.Join(*o, ",") }

func (o *options) Set(value string) error {
	if key, _, ok := strings.Cut(value, "="); !ok || key == "" {
		return fmt.Errorf("an option is key=value, not %q", value)
	}
	*o = append(*o, value)
	return nil
}

// BackupTool runs one op of the backup tool. env holds restic's variables, on
// top of the process's own environment.
func BackupTool(ctx context.Context, args []string, env map[string]string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("name an op: init")
	}
	op := args[0]
	flags := flag.NewFlagSet("backup-tool "+op, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var opts options
	flags.Var(&opts, "o", "a restic option, key=value")
	local := flags.String("local", "", "the directory of a local repository")
	expect := flags.String("expect", "", "the id of the repository, which must exist")
	usage := flags.Bool("usage", false, "say how much the repository holds")
	cache := flags.String("cache", "", "restic's cache directory")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if env[envResticRepository] == "" || env[envResticPassword] == "" {
		return errors.New("the job has no repository or no password: the panel adds them when it delivers the command")
	}
	secrets := backup.Secrets(env[envResticPassword], withoutRestic(env), "")
	log := &redactingWriter{w: stderr, secrets: secrets}
	defer log.Flush()
	r := restic{env: env, options: opts, log: log, secrets: secrets, cache: *cache}

	switch op {
	case "init":
		result, err := r.init(ctx, *local, *expect, *usage)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(result)
	}
	return fmt.Errorf("unknown op %q: use init", op)
}

// withoutRestic is the storage's credentials: env without restic's own
// repository and password.
func withoutRestic(env map[string]string) map[string]string {
	rest := map[string]string{}
	for name, value := range env {
		if name != envResticRepository && name != envResticPassword {
			rest[name] = value
		}
	}
	return rest
}

// restic runs restic for one repository.
type restic struct {
	env     map[string]string
	options []string
	log     io.Writer
	secrets []string
	// cache is restic's cache directory, if it keeps one
	cache string
}

// run runs restic and returns what it printed to stdout and its exit code. An
// error is one restic did not get to: a code of its own is not one.
func (r restic) run(ctx context.Context, args ...string) ([]byte, int, string, error) {
	full := []string{"--no-cache"}
	if r.cache != "" {
		full = []string{"--cache-dir", r.cache}
	}
	for _, option := range r.options {
		full = append(full, "-o", option)
	}
	full = append(full, args...)
	command := exec.CommandContext(ctx, "restic", full...) //nolint:gosec // restic with the job's repository
	command.Env = os.Environ()
	names := make([]string, 0, len(r.env))
	for name := range r.env {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		command.Env = append(command.Env, name+"="+r.env[name])
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = io.MultiWriter(&stderr, r.log)
	err := command.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && ctx.Err() == nil {
		return stdout.Bytes(), exit.ExitCode(), r.said(stderr.Bytes()), nil
	}
	if err != nil {
		return nil, -1, "", err
	}
	return stdout.Bytes(), 0, "", nil
}

// sshSaid prefixes what restic passes on of the ssh it runs for SFTP.
const sshSaid = "subprocess ssh: "

// said is what restic's error output comes down to, on one line: from its
// first "Fatal:" on, or its last lines, without the secrets. ssh's last words
// come first, as restic's own error after them only says the connection
// closed: they say why, such as a host key that changed or a key refused.
func (r restic) said(stderr []byte) string {
	var lines []string
	scanner := bufio.NewScanner(bytes.NewReader(stderr))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	var ssh []string
	for i, line := range lines {
		if strings.HasPrefix(line, "Fatal:") {
			for _, before := range lines[:i] {
				if text, ok := strings.CutPrefix(before, sshSaid); ok && !strings.Contains(text, "@@@") {
					ssh = append(ssh, strings.TrimSpace(text))
				}
			}
			lines = lines[i:]
			break
		}
	}
	if len(lines) > 4 {
		lines = lines[len(lines)-4:]
	}
	if len(ssh) > 2 {
		ssh = ssh[len(ssh)-2:]
	}
	if len(ssh) > 0 {
		lines = append([]string{"ssh: " + strings.Join(ssh, " ")}, lines...)
	}
	text := backup.Redact(strings.Join(lines, " "), r.secrets...)
	if len(text) > 1000 {
		text = text[:1000] + "…"
	}
	if text == "" {
		text = "restic failed without saying why"
	}
	return text
}

func (r restic) init(ctx context.Context, local, expect string, usage bool) (backupInitResult, error) {
	var result backupInitResult
	out, code, said, err := r.run(ctx, "--no-lock", "cat", "config")
	if err != nil {
		return result, err
	}
	// a bucket that is not there yet holds no repository either: init
	// creates it, or says the key may not
	if code == 1 && strings.Contains(said, "bucket does not exist") {
		code = resticNoRepository
	}
	switch code {
	case 0:
	case resticNoRepository:
		if expect != "" {
			return result, errors.New("the repository is gone, with its snapshots: " + said)
		}
		if local != "" {
			entries, err := os.ReadDir(local)
			if err != nil {
				return result, fmt.Errorf("the directory cannot be read: %w", err)
			}
			if len(entries) > 0 {
				return result, errors.New("the directory holds other files and no restic repository: choose an empty directory, or one with a repository")
			}
		}
		fmt.Fprintln(r.log, "There is no repository yet: creating it")
		created, code, said, err := r.run(ctx, "init", "--json")
		if err != nil {
			return result, err
		} else if code != 0 {
			return result, errors.New(said)
		}
		// its id comes with it: nothing that could fail is asked once the
		// repository exists, as the panel would forget a password that one
		// had already taken
		var initialized struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(lastLine(created), &initialized); err != nil || initialized.ID == "" {
			return result, fmt.Errorf("restic created the repository and said: %s", strings.TrimSpace(string(created)))
		}
		out = []byte(`{"id":"` + initialized.ID + `"}`)
		result.Created = true
		nothing := int64(0)
		result.Used = &nothing
		if local != "" {
			// only for the owner, as restic keeps its files
			if err := os.Chmod(local, 0o700); err != nil { //nolint:gosec // a directory
				fmt.Fprintln(r.log, "The directory's permissions stay as they are:", err)
			}
		}
	case resticWrongPassword:
		return result, errors.New("a repository is there, and the password is not its own: " + said)
	default:
		return result, errors.New(said)
	}

	var config struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &config); err != nil || config.ID == "" {
		return result, fmt.Errorf("restic's config cannot be read: %s", strings.TrimSpace(backup.Redact(string(out), r.secrets...)))
	}
	result.ID = config.ID
	if expect != "" && config.ID != expect {
		return result, fmt.Errorf("this is another repository (%.8s), not the target's (%.8s)", config.ID, expect)
	}

	if usage && !result.Created {
		// the repository opened: statistics that fail, as while a prune
		// runs, leave what it holds unsaid, not the check failed
		out, code, said, err = r.run(ctx, "--no-lock", "stats", "--mode", "raw-data", "--json")
		var stats struct {
			TotalSize *int64 `json:"total_size"`
		}
		switch {
		case err != nil:
			return result, err
		case code != 0:
			fmt.Fprintln(r.log, "How much the repository holds stays unsaid:", said)
		case json.Unmarshal(lastLine(out), &stats) != nil || stats.TotalSize == nil:
			fmt.Fprintln(r.log, "restic's statistics cannot be read:", strings.TrimSpace(string(out)))
		default:
			result.Used = stats.TotalSize
		}
	}

	if local != "" {
		if _, _, available, err := diskUsage(local); err == nil {
			free := int64(min(available, uint64(1)<<62)) //nolint:gosec // capped
			result.Free = &free
		}
	}
	return result, nil
}

// redactingWriter takes secrets out of what it passes on, line by line, so
// that none is split between two writes.
type redactingWriter struct {
	w       io.Writer
	secrets []string
	mu      sync.Mutex
	partial []byte
}

func (r *redactingWriter) Write(data []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.partial = append(r.partial, data...)
	for {
		end := bytes.IndexByte(r.partial, '\n')
		if end < 0 {
			break
		}
		line := backup.Redact(string(r.partial[:end+1]), r.secrets...)
		r.partial = r.partial[end+1:]
		if _, err := io.WriteString(r.w, line); err != nil {
			return len(data), err
		}
	}
	return len(data), nil
}

// Flush passes on what is left of an unfinished line.
func (r *redactingWriter) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.partial) > 0 {
		_, _ = io.WriteString(r.w, backup.Redact(string(r.partial), r.secrets...))
		r.partial = nil
	}
}
