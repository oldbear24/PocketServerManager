package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
)

const (
	restartDelay       = time.Second
	updaterName        = "pocketcloud-updater"
	updaterHealthLimit = 180
)

func (a *Agent) registerHandlers() {
	x := a.Executor

	x.Handle("agent.restart", a.handleRestart)
	x.Handle("agent.rotate_certificate", a.handleRotateCertificate)
	x.Handle("agent.diagnostics", a.handleDiagnostics)
	x.Handle("agent.set_log_level", a.handleSetLogLevel)
	x.Handle("agent.update", a.handleUpdate)

	x.Handle("container.start", a.handleContainerStart)
	x.Handle("container.stop", a.handleContainerStop)
	x.Handle("container.restart", a.handleContainerRestart)
	x.Handle("container.remove", a.handleContainerRemove)
	x.Handle("image.pull", a.handleImagePull)

	x.Handle("stack.deploy", a.handleStackDeploy)
	x.Handle("stack.stop", a.handleStackStop)
	x.Handle("stack.down", a.handleStackDown)
	x.Handle("stack.pull", a.handleStackPull)
	x.Handle("stack.restart", a.handleStackRestart)
	x.Handle("stack.validate", a.handleStackValidate)
	x.Handle("stack.import", a.handleStackImport)

	x.Handle("logs.follow", a.handleLogsFollow)
	x.Handle("logs.stop", a.handleLogsStop)

	x.Handle("exec.start", a.handleExecStart)
	x.Handle("exec.stop", a.handleExecStop)

	a.registerDatabaseHandlers()
	a.registerEdgeHandlers()
	a.registerSiteHandlers()
	a.registerPHPHandlers()
	a.registerTaskHandlers()
	a.registerRewriteHandlers()
}

// protected refuses work on the containers PocketCloud itself depends on. The UI
// hides these actions too, but the agent has the last word.
func (a *Agent) protected(ctx context.Context, id string) error {
	info, err := a.Docker.Inspect(ctx, id)
	if err != nil {
		return err
	}

	labels, _ := info.Config["Labels"].(map[string]any)
	if role, ok := labels["pocketcloud.role"].(string); ok {
		switch role {
		case "agent", "updater", "edge", "job":
			return fmt.Errorf("%s is a PocketCloud system container (%s) and cannot be stopped or removed from here", strings.TrimPrefix(info.Name, "/"), role)
		}
	}

	return nil
}

func (a *Agent) handleContainerStart(ctx context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	return nil, a.Docker.Start(ctx, command.GetContainerStart().GetDockerId())
}

func (a *Agent) handleContainerStop(ctx context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	payload := command.GetContainerStop()
	if err := a.protected(ctx, payload.GetDockerId()); err != nil {
		return nil, err
	}
	return nil, a.Docker.Stop(ctx, payload.GetDockerId(), int(payload.GetTimeoutS()))
}

func (a *Agent) handleContainerRestart(ctx context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	payload := command.GetContainerRestart()
	if err := a.protected(ctx, payload.GetDockerId()); err != nil {
		return nil, err
	}
	return nil, a.Docker.Restart(ctx, payload.GetDockerId(), int(payload.GetTimeoutS()))
}

func (a *Agent) handleContainerRemove(ctx context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	payload := command.GetContainerRemove()
	if err := a.protected(ctx, payload.GetDockerId()); err != nil {
		return nil, err
	}
	return nil, a.Docker.Remove(ctx, payload.GetDockerId(), payload.GetRemoveVolumes(), payload.GetForce())
}

func (a *Agent) handleImagePull(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetImagePull()

	var auth *dockerapi.PullAuth
	if credentials := payload.GetAuth(); credentials != nil {
		auth = &dockerapi.PullAuth{
			Username:      credentials.GetUsername(),
			Password:      credentials.GetPassword(),
			ServerAddress: credentials.GetServer(),
		}
	}

	reporter.Progress("pull", "Pulling "+payload.GetImage(), -1)

	err := a.Docker.Pull(ctx, payload.GetImage(), auth, func(line string) {
		if _, err := fmt.Fprintln(reporter, line); err != nil {
			a.Log.Debug("failed to report pull progress", "error", err)
		}
	})

	return map[string]any{"image": payload.GetImage()}, err
}

// handleRestart reports success first: the restart ends this process, and it is
// the Docker Engine that performs it, so it completes without us.
func (a *Agent) handleRestart(ctx context.Context, _ *agentv1.Command, _ *Reporter) (map[string]any, error) {
	self, err := a.ownContainer(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot find the agent's own container: %w", err)
	}

	time.AfterFunc(restartDelay, func() {
		if err := a.Docker.Restart(context.Background(), self.ID, 0); err != nil {
			a.Log.Error("failed to restart the agent container", "error", err)
		}
	})

	return map[string]any{"container": self.ID}, nil
}

func (a *Agent) handleRotateCertificate(ctx context.Context, _ *agentv1.Command, _ *Reporter) (map[string]any, error) {
	client := a.client.Load()
	if client == nil {
		return nil, errors.New("not connected")
	}

	if err := a.renewCertificate(ctx, *client); err != nil {
		return nil, err
	}

	// the current connection still uses the old certificate; the reconnect happens
	// after the result was reported
	time.AfterFunc(restartDelay, func() {
		select {
		case a.reconnect <- struct{}{}:
		default:
		}
	})

	return map[string]any{"expires_at": a.Identity.Certificate().NotAfter.Format(time.RFC3339)}, nil
}

// Debug logging always falls back by itself: a command without a limit gets the
// default one, and none lasts longer than a day.
const (
	defaultLogLevelDuration = 15 * time.Minute
	maxLogLevelDuration     = 24 * time.Hour
)

// logLevelDuration is how long a log level set by a command lasts.
func logLevelDuration(seconds int64) time.Duration {
	if seconds <= 0 {
		return defaultLogLevelDuration
	}
	return min(time.Duration(seconds)*time.Second, maxLogLevelDuration)
}

func (a *Agent) handleSetLogLevel(_ context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	payload := command.GetAgentSetLogLevel()
	duration := logLevelDuration(int64(payload.GetDurationS()))

	if err := a.setLogLevel(payload.GetLevel(), duration); err != nil {
		return nil, err
	}

	// info is where every other level falls back to; it has no limit itself
	if strings.EqualFold(payload.GetLevel(), "info") {
		duration = 0
	}

	return map[string]any{"level": payload.GetLevel(), "duration_s": int64(duration.Seconds())}, nil
}

// handleUpdate starts the temporary updater container. From there on the updater
// is in charge, and whichever agent runs afterwards reports the outcome.
func (a *Agent) handleUpdate(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetAgentUpdate()

	for _, image := range []string{payload.GetAgentImage(), payload.GetUpdaterImage()} {
		if !strings.Contains(image, "@sha256:") {
			return nil, fmt.Errorf("refusing to update: %q is not pinned by digest", image)
		}
	}

	reporter.Progress("pull", "Pulling the new agent image", 10)
	if err := a.Docker.Pull(ctx, payload.GetAgentImage(), nil, nil); err != nil {
		return nil, fmt.Errorf("failed to pull the agent image: %w", err)
	}

	reporter.Progress("pull", "Pulling the updater image", 40)
	if err := a.Docker.Pull(ctx, payload.GetUpdaterImage(), nil, nil); err != nil {
		return nil, fmt.Errorf("failed to pull the updater image: %w", err)
	}

	self, err := a.ownContainer(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot find the agent's own container: %w", err)
	}

	timeout := payload.GetHealthTimeoutS()
	if timeout <= 0 {
		timeout = updaterHealthLimit
	}

	stateVolume := volumeAt(self, a.Config.StateDir)
	if stateVolume == "" {
		return nil, fmt.Errorf("the agent's state directory %s is not on a volume", a.Config.StateDir)
	}

	name := strings.TrimPrefix(self.Name, "/")

	config := map[string]any{
		"Image": payload.GetUpdaterImage(),
		"Env": []string{
			"TARGET_CONTAINER=" + name,
			"NEW_IMAGE=" + payload.GetAgentImage(),
			"NEW_VERSION=" + payload.GetVersion(),
			"COMMAND_ID=" + command.GetId(),
			"HEALTH_TIMEOUT_S=" + strconv.Itoa(int(timeout)),
			"STATE_DIR=" + a.Config.StateDir,
		},
		"Labels": map[string]string{
			"pocketcloud.role":    "updater",
			"pocketcloud.command": command.GetId(),
		},
		"HostConfig": map[string]any{
			"AutoRemove": true,
			"Binds": []string{
				"/var/run/docker.sock:/var/run/docker.sock",
				stateVolume + ":" + a.Config.StateDir,
			},
		},
	}

	id, err := a.Docker.Create(ctx, updaterName+"-"+command.GetId(), config)
	if err != nil {
		return nil, fmt.Errorf("failed to create the updater container: %w", err)
	}

	if err := a.Docker.Start(ctx, id); err != nil {
		return nil, fmt.Errorf("failed to start the updater container: %w", err)
	}

	reporter.Progress("updater_started", "The updater is swapping the agent container", 60)

	return nil, errHandedOver
}

// volumeAt returns the volume or bind source mounted at a path of a container.
func volumeAt(info *dockerapi.ContainerInspect, path string) string {
	binds, _ := info.HostConfig["Binds"].([]any)
	for _, bind := range binds {
		text, _ := bind.(string)
		// source:target or source:target:options
		if source, found := strings.CutSuffix(text, ":"+path); found {
			return source
		}
		if source, _, found := strings.Cut(text, ":"+path+":"); found {
			return source
		}
	}

	mounts, _ := info.HostConfig["Mounts"].([]any)
	for _, mount := range mounts {
		fields, _ := mount.(map[string]any)
		if fields["Target"] == path {
			if source, ok := fields["Source"].(string); ok {
				return source
			}
		}
	}

	return ""
}
