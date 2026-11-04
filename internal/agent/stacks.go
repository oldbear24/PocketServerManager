package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/databases"
)

const (
	stacksDir    = "stacks"
	composeFile  = "compose.yaml"
	overrideFile = "compose.pocketcloud.yaml"
	envFile      = ".env"
	// cleanEnvFile holds the env file without secrets while a deploy runs. If the
	// agent dies in the middle, the next start puts it back in place.
	cleanEnvFile = ".env.clean"

	composeProjectLabel = "com.docker.compose.project"
	composeServiceLabel = "com.docker.compose.service"

	composeVersionTimeout = 5 * time.Second
	composeVersionMaxAge  = 10 * time.Minute

	replaceAttempts   = 5
	replaceRetryDelay = 200 * time.Millisecond

	maxImportedFile = 1 << 20
	// minMaskedSecret is the shortest value hidden in output: masking every "1"
	// would only make the output unreadable.
	minMaskedSecret = 4
)

var stackNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// composeVersion asks the Compose plugin for its version, which takes a process:
// heartbeats use the remembered answer.
func (a *Agent) composeVersion(parent context.Context) string {
	a.composeMu.Lock()
	defer a.composeMu.Unlock()

	if !a.composeAt.IsZero() && time.Since(a.composeAt) < composeVersionMaxAge {
		return a.composeRelease
	}

	ctx, cancel := context.WithTimeout(parent, composeVersionTimeout)
	defer cancel()

	a.composeAt = time.Now()
	a.composeRelease = ""

	output, err := a.dockerCommand(ctx, "compose", "version", "--short").Output()
	if err != nil {
		a.Log.Debug("the Compose plugin did not answer", "error", err)
		return ""
	}

	a.composeRelease = strings.TrimSpace(string(output))

	return a.composeRelease
}

// dockerCommand prepares a run of the Docker command line tool for this agent's engine.
func (a *Agent) dockerCommand(ctx context.Context, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, a.Config.dockerCLI(), args...) //nolint:gosec // no shell, and the arguments come from this package
	command.Env = os.Environ()
	if a.Config.DockerHost != "" {
		command.Env = append(command.Env, "DOCKER_HOST="+a.Config.DockerHost)
	}
	return command
}

func (a *Agent) stackDir(name string) (string, error) {
	if !stackNamePattern.MatchString(name) {
		return "", fmt.Errorf("invalid stack name %q", name)
	}
	return filepath.Join(a.Config.StateDir, stacksDir, name), nil
}

// compose runs `docker compose` with a stack's files, streaming its output.
func (a *Agent) compose(ctx context.Context, name string, output io.Writer, extraEnv []string, args ...string) error {
	dir, err := a.stackDir(name)
	if err != nil {
		return err
	}

	arguments := []string{
		"compose",
		"--project-name", name,
		"--project-directory", dir,
		"--file", filepath.Join(dir, composeFile),
	}
	if _, err := os.Stat(filepath.Join(dir, overrideFile)); err == nil {
		arguments = append(arguments, "--file", filepath.Join(dir, overrideFile))
	}
	arguments = append(arguments, "--env-file", filepath.Join(dir, envFile))

	return a.runCompose(ctx, dir, output, extraEnv, args[0], append(arguments, args...))
}

// composeByName runs `docker compose` with only the project's name: the files are
// not read, so a variable that needs a secret does not get in the way of stopping
// a stack, and a project that was started elsewhere can be handled too.
func (a *Agent) composeByName(ctx context.Context, name string, output io.Writer, args ...string) error {
	if _, err := a.stackDir(name); err != nil {
		return err
	}

	return a.runCompose(ctx, "", output, nil, args[0], append([]string{"compose", "--project-name", name}, args...))
}

func (a *Agent) runCompose(ctx context.Context, dir string, output io.Writer, extraEnv []string, verb string, arguments []string) error {
	command := a.dockerCommand(ctx, arguments...)
	command.Dir = dir
	command.Stdout = output
	command.Stderr = output
	command.Env = append(command.Env, extraEnv...)

	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("docker compose %s failed with exit code %d", verb, exit.ExitCode())
		}
		return fmt.Errorf("docker compose %s: %w", verb, err)
	}

	return nil
}

// handleStackDeploy writes the stack's files and brings it up. Secrets go into
// the env file only for the duration of the deploy; the containers keep them.
func (a *Agent) handleStackDeploy(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetStackDeploy()

	dir, err := a.stackDir(payload.GetName())
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	if err := writeAtomic(filepath.Join(dir, composeFile), []byte(payload.GetComposeYaml()), 0o644); err != nil {
		return nil, err
	}

	override := filepath.Join(dir, overrideFile)
	if payload.GetOverrideYaml() != "" {
		if err := writeAtomic(override, []byte(payload.GetOverrideYaml()), 0o644); err != nil {
			return nil, err
		}
	} else if err := os.Remove(override); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	// the clean copy goes first: from here on a crash can be repaired
	clean := filepath.Join(dir, cleanEnvFile)
	if err := writeEnvFile(clean, payload.GetEnv(), nil); err != nil {
		return nil, err
	}
	if err := writeEnvFile(filepath.Join(dir, envFile), payload.GetEnv(), payload.GetSecrets()); err != nil {
		return nil, err
	}
	defer func() {
		// the file stays usable for later commands, only without the secrets
		if err := replaceFile(clean, filepath.Join(dir, envFile)); err != nil {
			a.Log.Error("failed to remove secrets from an env file", "stack", payload.GetName(), "error", err)
		}
	}()

	env, cleanup, err := registryEnv(payload.GetRegistries())
	if err != nil {
		return nil, err
	}
	defer cleanup()

	output := newMaskingWriter(reporter, payload.GetSecrets())

	// the override refers to the database networks as external: they must exist
	if len(payload.GetDbNetworks()) > 0 {
		reporter.Progress("networks", "Preparing the database networks", 5)
		if err := a.prepareNetworks(ctx, payload.GetDbNetworks(), output); err != nil {
			return nil, err
		}
	}

	reporter.Progress("validate", "Validating the Compose file", 10)
	if err := a.compose(ctx, payload.GetName(), output, env, "config", "--quiet"); err != nil {
		return nil, err
	}

	if payload.GetPull() {
		reporter.Progress("pull", "Pulling images", 30)
		if err := a.compose(ctx, payload.GetName(), output, env, "pull"); err != nil {
			return nil, err
		}
	}

	reporter.Progress("up", "Starting containers", 60)
	if err := a.compose(ctx, payload.GetName(), output, env, "up", "--detach", "--remove-orphans"); err != nil {
		return nil, err
	}

	return map[string]any{"revision": payload.GetRevision()}, nil
}

// restoreCleanEnvFiles repairs what a deploy left behind when the agent died in the
// middle of it: an env file with secrets, on the agent's volume.
func (a *Agent) restoreCleanEnvFiles() {
	clean, err := filepath.Glob(filepath.Join(a.Config.StateDir, stacksDir, "*", cleanEnvFile))
	if err != nil {
		return
	}

	for _, path := range clean {
		if err := replaceFile(path, filepath.Join(filepath.Dir(path), envFile)); err != nil {
			a.Log.Error("failed to remove secrets from an env file", "path", path, "error", err)
		}
	}
}

// replaceFile moves a file over another. These files must not stay behind, and a
// virus scanner on a development machine can hold one for a moment.
func replaceFile(from, to string) error {
	var err error
	for range replaceAttempts {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(replaceRetryDelay)
	}
	return err
}

// writeEnvFile writes the plain environment followed by the secrets.
func writeEnvFile(path, env string, secrets map[string]string) error {
	var content strings.Builder
	content.WriteString(env)
	if env != "" && !strings.HasSuffix(env, "\n") {
		content.WriteByte('\n')
	}

	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	slices.Sort(names)

	for _, name := range names {
		content.WriteString(name)
		content.WriteByte('=')
		content.WriteString(quoteEnvValue(secrets[name]))
		content.WriteByte('\n')
	}

	return writeAtomic(path, []byte(content.String()), 0o600)
}

var envEscapes = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "\n", `\n`, "\r", `\r`)

// quoteEnvValue double-quotes a value for Compose's env file syntax, so that a
// password may contain `$`, `#`, quotes or spaces without being interpreted. A
// single-quoted value cannot end with a backslash in that syntax.
func quoteEnvValue(value string) string {
	return `"` + envEscapes.Replace(value) + `"`
}

// maskingWriter hides the values of secrets in what a command writes. It works per
// write, and Compose writes whole lines.
type maskingWriter struct {
	next    io.Writer
	secrets [][]byte
}

func newMaskingWriter(next io.Writer, secrets map[string]string) io.Writer {
	values := make([][]byte, 0, len(secrets))
	for _, value := range secrets {
		if len(value) >= minMaskedSecret {
			values = append(values, []byte(value))
		}
	}
	if len(values) == 0 {
		return next
	}
	return &maskingWriter{next: next, secrets: values}
}

func (m *maskingWriter) Write(data []byte) (int, error) {
	masked := data
	for _, secret := range m.secrets {
		masked = bytes.ReplaceAll(masked, secret, []byte("***"))
	}
	if _, err := m.next.Write(masked); err != nil {
		return 0, err
	}
	return len(data), nil
}

// registryEnv creates a temporary Docker config with registry credentials.
func registryEnv(registries []*agentv1.RegistryAuth) ([]string, func(), error) {
	if len(registries) == 0 {
		return nil, func() {}, nil
	}

	dir, err := os.MkdirTemp("", "pocketcloud-docker-config-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	auths := make(map[string]map[string]string, len(registries))
	for _, registry := range registries {
		auths[registry.GetServer()] = map[string]string{
			"auth": base64.StdEncoding.EncodeToString([]byte(registry.GetUsername() + ":" + registry.GetPassword())),
		}
	}

	raw, err := json.Marshal(map[string]any{"auths": auths})
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0o600); err != nil {
		cleanup()
		return nil, nil, err
	}

	return []string{"DOCKER_CONFIG=" + dir}, cleanup, nil
}

func (a *Agent) handleStackStop(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	return nil, a.composeByName(ctx, command.GetStackStop().GetName(), reporter, "stop")
}

func (a *Agent) handleStackDown(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetStackDown()

	args := []string{"down", "--remove-orphans"}
	if payload.GetRemoveVolumes() {
		args = append(args, "--volumes")
	}

	return nil, a.composeByName(ctx, payload.GetName(), reporter, args...)
}

// handleStackPull reads the Compose file, so its variables need values: the secrets
// come with the command and only live in the environment of this one process.
func (a *Agent) handleStackPull(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetStackPull()

	env, cleanup, err := registryEnv(payload.GetRegistries())
	if err != nil {
		return nil, err
	}
	defer cleanup()

	for name, value := range payload.GetSecrets() {
		env = append(env, name+"="+value)
	}

	return nil, a.compose(ctx, payload.GetName(), newMaskingWriter(reporter, payload.GetSecrets()), env, "pull")
}

func (a *Agent) handleStackRestart(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	name := command.GetStackRestart().GetName()
	services, err := a.liveServices(ctx, name)
	if err != nil {
		return nil, err
	}
	return nil, a.composeByName(ctx, name, reporter, append([]string{"restart"}, services...)...)
}

// liveServices are the services of a stack to restart by name: none named, which
// means all of them, unless the stack's override keeps some stopped because their
// data moved to a shared server. Without its files, `restart` would start those too.
func (a *Agent) liveServices(ctx context.Context, name string) ([]string, error) {
	dir, err := a.stackDir(name)
	if err != nil {
		return nil, err
	}
	override, err := os.ReadFile(filepath.Join(dir, overrideFile))
	if err != nil {
		return nil, nil //nolint:nilerr // no override, nothing kept stopped
	}
	retired := databases.RetiredServices(string(override))
	if len(retired) == 0 {
		return nil, nil
	}

	containers, err := a.Docker.Containers(ctx)
	if err != nil {
		return nil, err
	}
	services := []string{}
	for _, container := range containers {
		if container.Labels[composeProjectLabel] != name {
			continue
		}
		service := container.Labels[composeServiceLabel]
		if service != "" && !slices.Contains(retired, service) && !slices.Contains(services, service) {
			services = append(services, service)
		}
	}
	if len(services) == 0 {
		return nil, fmt.Errorf("%s has nothing to restart but services kept stopped", name)
	}
	slices.Sort(services)
	return services, nil
}

// handleStackValidate checks files with the same Compose version that will run
// them, in a scratch directory, so the deployed files stay untouched.
func (a *Agent) handleStackValidate(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetStackValidate()

	if !stackNamePattern.MatchString(payload.GetName()) {
		return nil, fmt.Errorf("invalid stack name %q", payload.GetName())
	}

	dir, err := os.MkdirTemp("", "pocketcloud-validate-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	compose := filepath.Join(dir, composeFile)
	env := filepath.Join(dir, envFile)

	if err := os.WriteFile(compose, []byte(payload.GetComposeYaml()), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(env, []byte(payload.GetEnv()), 0o600); err != nil {
		return nil, err
	}

	validation := a.dockerCommand(ctx, "compose",
		"--project-name", payload.GetName(), "--project-directory", dir,
		"--file", compose, "--env-file", env, "config", "--quiet")
	validation.Stdout = reporter
	validation.Stderr = reporter

	if err := validation.Run(); err != nil {
		return nil, fmt.Errorf("the Compose file is not valid: %w", err)
	}

	return map[string]any{"valid": true}, nil
}

// ImportedStack is what stack.import uploads.
type ImportedStack struct {
	ComposeYAML string `json:"compose_yaml"`
	Env         string `json:"env"`
	WorkingDir  string `json:"working_dir"`
}

// ArtifactKindStackImport is the kind of the upload of stack.import.
const ArtifactKindStackImport = "stack_import"

// handleStackImport reads a project's files through the host mount. They go up as
// an artifact, because a command's result is visible to everyone who can read
// commands and these files may hold passwords.
func (a *Agent) handleStackImport(ctx context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	payload := command.GetStackImport()

	files := payload.GetConfigFiles()
	switch {
	case len(files) == 0:
		return nil, errors.New("the Docker labels of this project do not say where its Compose file is")
	case len(files) > 1:
		return nil, fmt.Errorf("this project was started from %d Compose files; merge them into one first", len(files))
	}

	compose, err := a.readHostFile(files[0])
	if err != nil {
		return nil, fmt.Errorf("cannot read the Compose file: %w", err)
	}

	imported := ImportedStack{ComposeYAML: string(compose), WorkingDir: payload.GetWorkingDir()}

	if payload.GetWorkingDir() != "" {
		env, err := a.readHostFile(filepath.Join(payload.GetWorkingDir(), envFile))
		switch {
		case err == nil:
			imported.Env = string(env)
		case !errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("cannot read the .env file: %w", err)
		}
	}

	raw, err := json.Marshal(imported)
	if err != nil {
		return nil, err
	}

	id, err := a.uploadArtifact(ctx, command.GetId(), ArtifactKindStackImport, payload.GetProject()+".json", raw)
	if err != nil {
		return nil, err
	}

	return map[string]any{"stack": id}, nil
}

// readHostFile reads a file of the host through the read-only mount.
func (a *Agent) readHostFile(path string) ([]byte, error) {
	file, err := os.Open(filepath.Join(a.host.root, filepath.Clean("/"+filepath.ToSlash(path))))
	if err != nil {
		return nil, err
	}
	defer file.Close()

	content, err := io.ReadAll(io.LimitReader(file, maxImportedFile+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxImportedFile {
		return nil, fmt.Errorf("%s is too large", path)
	}

	return content, nil
}
