package agent

import (
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

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
)

const (
	stacksDir   = "stacks"
	composeFile = "compose.yaml"
	envFile     = ".env"
)

var stackNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func (a *Agent) stackDir(name string) (string, error) {
	if !stackNamePattern.MatchString(name) {
		return "", fmt.Errorf("invalid stack name %q", name)
	}
	return filepath.Join(a.Config.StateDir, stacksDir, name), nil
}

// compose runs `docker compose` for a stack, streaming its output.
func (a *Agent) compose(ctx context.Context, name string, output io.Writer, extraEnv []string, args ...string) error {
	dir, err := a.stackDir(name)
	if err != nil {
		return err
	}

	arguments := append([]string{
		"compose",
		"--project-name", name,
		"--project-directory", dir,
		"--file", filepath.Join(dir, composeFile),
		"--env-file", filepath.Join(dir, envFile),
	}, args...)

	command := exec.CommandContext(ctx, "docker", arguments...) //nolint:gosec // no shell, and the stack name is validated above
	command.Dir = dir
	command.Stdout = output
	command.Stderr = output
	command.Env = append(os.Environ(), extraEnv...)

	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("docker compose %s failed with exit code %d", args[0], exit.ExitCode())
		}
		return fmt.Errorf("docker compose %s: %w", args[0], err)
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

	if err := writeEnvFile(filepath.Join(dir, envFile), payload.GetEnv(), payload.GetSecrets()); err != nil {
		return nil, err
	}
	defer func() {
		// the file stays usable for later commands, only without the secrets
		if err := writeEnvFile(filepath.Join(dir, envFile), payload.GetEnv(), nil); err != nil {
			a.Log.Error("failed to remove secrets from an env file", "stack", payload.GetName(), "error", err)
		}
	}()

	env, cleanup, err := registryEnv(payload.GetRegistries())
	if err != nil {
		return nil, err
	}
	defer cleanup()

	reporter.Progress("validate", "Validating the Compose file", 10)
	if err := a.compose(ctx, payload.GetName(), reporter, env, "config", "--quiet"); err != nil {
		return nil, err
	}

	if payload.GetPull() {
		reporter.Progress("pull", "Pulling images", 30)
		if err := a.compose(ctx, payload.GetName(), reporter, env, "pull"); err != nil {
			return nil, err
		}
	}

	reporter.Progress("up", "Starting containers", 60)
	if err := a.compose(ctx, payload.GetName(), reporter, env, "up", "--detach", "--remove-orphans"); err != nil {
		return nil, err
	}

	return map[string]any{"revision": payload.GetRevision()}, nil
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

// quoteEnvValue single-quotes a value for Compose's env file syntax, so that a
// password may contain `$`, `#` or spaces without being interpreted.
func quoteEnvValue(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
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
	return nil, a.compose(ctx, command.GetStackStop().GetName(), reporter, nil, "stop")
}

func (a *Agent) handleStackDown(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetStackDown()

	args := []string{"down", "--remove-orphans"}
	if payload.GetRemoveVolumes() {
		args = append(args, "--volumes")
	}

	return nil, a.compose(ctx, payload.GetName(), reporter, nil, args...)
}

func (a *Agent) handleStackPull(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	return nil, a.compose(ctx, command.GetStackPull().GetName(), reporter, nil, "pull")
}

func (a *Agent) handleStackRestart(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	return nil, a.compose(ctx, command.GetStackRestart().GetName(), reporter, nil, "restart")
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

	validation := exec.CommandContext(ctx, "docker", "compose", //nolint:gosec // no shell, and the stack name is validated above
		"--project-name", payload.GetName(), "--project-directory", dir,
		"--file", compose, "--env-file", env, "config", "--quiet")
	validation.Stdout = reporter
	validation.Stderr = reporter

	if err := validation.Run(); err != nil {
		return nil, fmt.Errorf("the Compose file is not valid: %w", err)
	}

	return map[string]any{"valid": true}, nil
}
