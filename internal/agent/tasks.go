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

// Scheduled tasks (#51): the panel's scheduler sends task.run when a task is
// due, and the agent runs its command in a running container with docker
// exec: a PHP site's PHP-FPM, as www-data in its live release, or a stack's
// service.

const (
	// taskOutputTail is how much of the end of a run's output is kept.
	taskOutputTail = 4 << 10
	// taskGrace is how long after its timeout a run is given up on, when its
	// container had no timeout command to kill it.
	taskGrace = 15 * time.Second
	// maxTaskTimeout is the longest a task may run.
	maxTaskTimeout = 24 * time.Hour
)

// taskScript runs a task's command with its output and errors as one stream,
// and kills it with its children at its timeout where the container has a
// timeout command (coreutils, busybox), which signals its process group. Its
// arguments are the timeout in seconds and the command.
const taskScript = `t=$1; shift
if command -v timeout >/dev/null 2>&1; then exec timeout -s KILL "$t" sh -c "$1" 2>&1; fi
exec sh -c "$1" 2>&1`

func (a *Agent) registerTaskHandlers() {
	a.Executor.Handle("task.run", a.handleTaskRun)
}

func (a *Agent) handleTaskRun(ctx context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	payload := command.GetTaskRun()
	container, script := payload.GetContainerId(), payload.GetCommand()
	if container == "" || strings.TrimSpace(script) == "" {
		return nil, errors.New("a task needs a container and a command")
	}
	timeout := time.Duration(payload.GetTimeoutS()) * time.Second
	if timeout < time.Second || timeout > maxTaskTimeout {
		return nil, fmt.Errorf("a task's timeout is between a second and %v, not %v", maxTaskTimeout, timeout)
	}
	// a run that is late must not start next to the one still going
	if _, running := a.tasks.LoadOrStore(payload.GetTaskId(), struct{}{}); running {
		return nil, errors.New("the previous run of this task is still going")
	}
	defer a.tasks.Delete(payload.GetTaskId())

	output := &tailBuffer{limit: taskOutputTail}
	runCtx, cancel := context.WithTimeout(ctx, timeout+taskGrace)
	defer cancel()
	started := time.Now()
	ran, err := a.Docker.Run(runCtx, container, dockerapi.ExecOptions{
		Cmd:        []string{"sh", "-c", taskScript, "pocketcloud-task", strconv.Itoa(int(timeout.Seconds())), script},
		User:       payload.GetUser(),
		WorkingDir: payload.GetWorkingDir(),
	}, output)
	elapsed := time.Since(started)
	result := map[string]any{"duration_ms": float64(elapsed.Milliseconds()), "output": output.String()}
	switch {
	case err != nil && dockerapi.IsNotFound(err):
		return result, fmt.Errorf("the container %s does not exist", container)
	case err != nil && runCtx.Err() != nil && ctx.Err() == nil:
		result["timed_out"] = true
		return result, fmt.Errorf("the task ran past its timeout of %v, and its container has no timeout command to stop it", timeout)
	case err != nil:
		return result, err
	}
	result["exit_code"] = float64(ran.ExitCode)
	// timeout's own status for a command it killed: 124, or 128+9 for KILL
	if (ran.ExitCode == 124 || ran.ExitCode == 137) && elapsed >= timeout-time.Second {
		result["timed_out"] = true
		return result, fmt.Errorf("the task ran past its timeout of %v and was killed", timeout)
	}
	if ran.ExitCode != 0 {
		return result, fmt.Errorf("the task exited with %d", ran.ExitCode)
	}
	return result, nil
}

// tailBuffer keeps the end of what is written to it.
type tailBuffer struct {
	limit     int
	data      []byte
	truncated bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	if len(b.data) > 2*b.limit {
		b.data = append(b.data[:0], b.data[len(b.data)-b.limit:]...)
		b.truncated = true
	}
	return len(p), nil
}

// String is the last limit bytes, whole characters only.
func (b *tailBuffer) String() string {
	data := b.data
	cut := b.truncated
	if len(data) > b.limit {
		data, cut = data[len(data)-b.limit:], true
	}
	text := strings.ToValidUTF8(string(data), "")
	if cut {
		return "…" + text
	}
	return text
}
