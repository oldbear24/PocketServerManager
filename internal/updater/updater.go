// Package updater swaps the agent container for one running a new image. A
// container cannot safely replace itself, so this runs in a short-lived updater
// container started by the agent. See "Agent updates" in docs/architecture.md.
package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
)

const (
	// OutcomeDir is where outcomes are stored inside the agent volume.
	OutcomeDir = "updates"

	oldSuffix = "-old"

	stopTimeoutSeconds = 30
	pollInterval       = 2 * time.Second
)

// Outcome is what the updater leaves in the agent volume. Whichever agent runs
// afterwards reports it for the command id.
type Outcome struct {
	CommandID  string `json:"command_id"`
	Succeeded  bool   `json:"succeeded"`
	Error      string `json:"error,omitempty"`
	Step       string `json:"step,omitempty"`
	OldImage   string `json:"old_image,omitempty"`
	NewImage   string `json:"new_image,omitempty"`
	RolledBack bool   `json:"rolled_back,omitempty"`
	Version    string `json:"version,omitempty"`
}

// Options configures one update.
type Options struct {
	// Target is the name of the agent container.
	Target string
	// Image is the new image, pinned by digest.
	Image string
	// CommandID is the agent.update command this update belongs to.
	CommandID string
	// Version is the agent version that belongs to the image.
	Version string
	// HealthTimeout is how long the new container may take to turn healthy.
	HealthTimeout time.Duration
	// StateDir is where the agent volume is mounted.
	StateDir string
	// PollInterval overrides how often the new container is checked, in tests.
	PollInterval time.Duration
}

// Run performs the update: it stops and renames the old container, creates the
// new one from the same config and waits for it to turn healthy, which means it
// connected to the panel. On failure it puts the old container back. The old
// container is removed only after the new one has proven itself, so it can never
// be lost, even if this process is killed at any point.
func Run(ctx context.Context, docker *dockerapi.Client, options Options) Outcome {
	outcome := Outcome{CommandID: options.CommandID, NewImage: options.Image, Version: options.Version}

	if options.PollInterval == 0 {
		options.PollInterval = pollInterval
	}

	if !strings.Contains(options.Image, "@sha256:") {
		outcome.Error = "the new image is not pinned by digest"
		return outcome
	}

	old, err := docker.Inspect(ctx, options.Target)
	if err != nil {
		outcome.Step = "inspect"
		outcome.Error = err.Error()
		return outcome
	}

	if image, ok := old.Config["Image"].(string); ok {
		outcome.OldImage = image
	}

	config, err := newContainerConfig(ctx, docker, old, options.Image)
	if err != nil {
		outcome.Step = "prepare"
		outcome.Error = err.Error()
		return outcome
	}

	oldName := options.Target + oldSuffix

	// a leftover of an earlier failed update would block the rename; the target is
	// what runs now, so the leftover is not needed
	if leftover, err := docker.Inspect(ctx, oldName); err == nil && leftover.ID != old.ID {
		if err := docker.Remove(ctx, leftover.ID, false, true); err != nil {
			outcome.Step = "cleanup"
			outcome.Error = err.Error()
			return outcome
		}
	}

	slog.Info("stopping the old agent", "container", old.ID)
	if err := docker.Stop(ctx, old.ID, stopTimeoutSeconds); err != nil {
		outcome.Step = "stop"
		outcome.Error = err.Error()
		return recoverOld(ctx, docker, old.ID, options.Target, outcome)
	}

	if err := docker.Rename(ctx, old.ID, oldName); err != nil {
		outcome.Step = "rename"
		outcome.Error = err.Error()
		return recoverOld(ctx, docker, old.ID, options.Target, outcome)
	}

	newID, err := docker.Create(ctx, options.Target, config)
	if err != nil {
		outcome.Step = "create"
		outcome.Error = err.Error()
		return recoverOld(ctx, docker, old.ID, options.Target, outcome)
	}

	if err := connectExtraNetworks(ctx, docker, old, newID); err != nil {
		outcome.Step = "network"
		outcome.Error = err.Error()
		return rollBack(ctx, docker, newID, old.ID, options.Target, outcome)
	}

	slog.Info("starting the new agent", "container", newID, "image", options.Image)
	if err := docker.Start(ctx, newID); err != nil {
		outcome.Step = "start"
		outcome.Error = err.Error()
		return rollBack(ctx, docker, newID, old.ID, options.Target, outcome)
	}

	if err := waitHealthy(ctx, docker, newID, options.HealthTimeout, options.PollInterval); err != nil {
		outcome.Step = "health"
		outcome.Error = err.Error()
		return rollBack(ctx, docker, newID, old.ID, options.Target, outcome)
	}

	slog.Info("the new agent is healthy, removing the old one")
	if err := docker.Remove(ctx, old.ID, false, true); err != nil {
		// the update itself worked; the leftover is only reported
		slog.Warn("failed to remove the old agent container", "error", err)
		outcome.Error = "the old container could not be removed: " + err.Error()
	}

	if outcome.OldImage != "" && outcome.OldImage != options.Image {
		if err := docker.RemoveImage(ctx, outcome.OldImage); err != nil {
			slog.Info("the old agent image was kept", "image", outcome.OldImage, "reason", err)
		}
	}

	outcome.Succeeded = true

	return outcome
}

// recoverOld puts the old container back after a failure before the new one exists.
func recoverOld(ctx context.Context, docker *dockerapi.Client, oldID, target string, outcome Outcome) Outcome {
	if err := restoreOld(ctx, docker, oldID, target); err != nil {
		outcome.Error += "; restoring the old container failed: " + err.Error()
	}
	outcome.RolledBack = true
	return outcome
}

// rollBack removes the new container and restores the old one.
func rollBack(ctx context.Context, docker *dockerapi.Client, newID, oldID, target string, outcome Outcome) Outcome {
	slog.Warn("rolling back", "step", outcome.Step, "error", outcome.Error)

	if err := docker.Remove(ctx, newID, false, true); err != nil && !dockerapi.IsNotFound(err) {
		outcome.Error += "; removing the new container failed: " + err.Error()
	}

	return recoverOld(ctx, docker, oldID, target, outcome)
}

func restoreOld(ctx context.Context, docker *dockerapi.Client, oldID, target string) error {
	info, err := docker.Inspect(ctx, oldID)
	if err != nil {
		return err
	}

	if strings.TrimPrefix(info.Name, "/") != target {
		if err := docker.Rename(ctx, oldID, target); err != nil {
			return err
		}
	}

	if !info.State.Running {
		return docker.Start(ctx, oldID)
	}

	return nil
}

// newContainerConfig builds the new container from the old one's config. Three
// values are not carried over as they are, because they come from the old
// container or image and would otherwise pin them for good:
//   - an auto-generated hostname (the old container's short id);
//   - the default environment, command and entrypoint of the old image;
//   - network aliases that are the old container's short id.
func newContainerConfig(ctx context.Context, docker *dockerapi.Client, old *dockerapi.ContainerInspect, image string) (map[string]any, error) {
	config := make(map[string]any, len(old.Config)+2)
	for key, value := range old.Config {
		config[key] = value
	}
	config["Image"] = image

	if hostname, _ := config["Hostname"].(string); hostname != "" && strings.HasPrefix(old.ID, hostname) {
		delete(config, "Hostname")
	}

	if imageName, _ := old.Config["Image"].(string); imageName != "" {
		if oldImage, err := docker.InspectImage(ctx, imageName); err == nil {
			config["Env"] = withoutImageEnv(toStrings(config["Env"]), oldImage.Config.Env)
			if slices.Equal(toStrings(config["Cmd"]), oldImage.Config.Cmd) {
				delete(config, "Cmd")
			}
			if slices.Equal(toStrings(config["Entrypoint"]), oldImage.Config.Entrypoint) {
				delete(config, "Entrypoint")
			}
			config["Labels"] = withoutImageLabels(old.Config["Labels"], oldImage.Config.Labels)
		} else {
			slog.Warn("the old image could not be inspected, keeping its defaults", "error", err)
		}
	}

	config["HostConfig"] = old.HostConfig

	// a create takes one network; the others are connected before the start
	if name, endpoint := primaryNetwork(old); name != "" {
		config["NetworkingConfig"] = map[string]any{
			"EndpointsConfig": map[string]any{name: cleanEndpoint(endpoint, old.ID)},
		}
	}

	return config, nil
}

func primaryNetwork(old *dockerapi.ContainerInspect) (string, map[string]any) {
	mode, _ := old.HostConfig["NetworkMode"].(string)
	if endpoint, ok := old.NetworkSettings.Networks[mode]; ok {
		return mode, endpoint
	}

	names := make([]string, 0, len(old.NetworkSettings.Networks))
	for name := range old.NetworkSettings.Networks {
		names = append(names, name)
	}
	slices.Sort(names)

	if len(names) == 0 {
		return "", nil
	}

	return names[0], old.NetworkSettings.Networks[names[0]]
}

func connectExtraNetworks(ctx context.Context, docker *dockerapi.Client, old *dockerapi.ContainerInspect, newID string) error {
	primary, _ := primaryNetwork(old)

	for name, endpoint := range old.NetworkSettings.Networks {
		if name == primary {
			continue
		}
		if err := docker.ConnectNetwork(ctx, name, newID, cleanEndpoint(endpoint, old.ID)); err != nil {
			return fmt.Errorf("network %s: %w", name, err)
		}
	}

	return nil
}

// cleanEndpoint keeps what was configured and drops what Docker assigned to the
// old container: addresses, ids and the alias made from its id.
func cleanEndpoint(endpoint map[string]any, oldID string) map[string]any {
	clean := map[string]any{}

	for _, key := range []string{"IPAMConfig", "Links", "DriverOpts"} {
		if value, ok := endpoint[key]; ok && value != nil {
			clean[key] = value
		}
	}

	aliases := []string{}
	for _, alias := range toStrings(endpoint["Aliases"]) {
		if !strings.HasPrefix(oldID, alias) {
			aliases = append(aliases, alias)
		}
	}
	if len(aliases) > 0 {
		clean["Aliases"] = aliases
	}

	return clean
}

func waitHealthy(ctx context.Context, docker *dockerapi.Client, id string, timeout, interval time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string

	for time.Now().Before(deadline) {
		info, err := docker.Inspect(ctx, id)
		if err != nil {
			return err
		}

		if info.State.Status == "exited" || info.State.Status == "dead" {
			return fmt.Errorf("the new agent exited with code %d", info.State.ExitCode)
		}
		// the restart policy it took over keeps a crashing agent from staying
		// exited: without this, a release that cannot start would only be rolled
		// back at the end of the timeout, minutes without an agent
		if info.State.Restarting || info.RestartCount > 0 {
			return fmt.Errorf("the new agent keeps exiting, last with code %d", info.State.ExitCode)
		}

		last = info.Health()
		switch last {
		case "healthy":
			return nil
		case "":
			return errors.New("the new image has no health check")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}

	return fmt.Errorf("the new agent did not connect to the panel within %s (health: %s)", timeout, last)
}

func toStrings(value any) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []any:
		result := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	}
	return nil
}

func withoutImageEnv(env, imageEnv []string) []string {
	result := make([]string, 0, len(env))
	for _, entry := range env {
		if !slices.Contains(imageEnv, entry) {
			result = append(result, entry)
		}
	}
	return result
}

func withoutImageLabels(labels any, imageLabels map[string]string) map[string]any {
	source, _ := labels.(map[string]any)

	result := make(map[string]any, len(source))
	for key, value := range source {
		if text, ok := value.(string); ok && imageLabels[key] == text {
			continue
		}
		result[key] = value
	}

	return result
}

// WriteOutcome stores an outcome in the agent volume.
func WriteOutcome(stateDir string, outcome Outcome) error {
	dir := filepath.Join(stateDir, OutcomeDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	raw, err := json.MarshalIndent(outcome, "", "  ")
	if err != nil {
		return err
	}

	path := filepath.Join(dir, outcome.CommandID+".json")
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, raw, 0o600); err != nil {
		return err
	}

	return os.Rename(temporary, path)
}

// ReadOutcomes returns the outcomes waiting to be reported.
func ReadOutcomes(stateDir string) ([]Outcome, error) {
	entries, err := os.ReadDir(filepath.Join(stateDir, OutcomeDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	outcomes := []Outcome{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		raw, err := os.ReadFile(filepath.Join(stateDir, OutcomeDir, entry.Name()))
		if err != nil {
			return nil, err
		}

		var outcome Outcome
		if err := json.Unmarshal(raw, &outcome); err != nil {
			return nil, err
		}
		outcomes = append(outcomes, outcome)
	}

	return outcomes, nil
}

// RemoveOutcome deletes an outcome after it has been reported.
func RemoveOutcome(stateDir, commandID string) error {
	return os.Remove(filepath.Join(stateDir, OutcomeDir, commandID+".json"))
}
