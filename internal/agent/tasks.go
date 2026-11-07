package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
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
// in a process group of its own (setsid, where the container has it, or
// timeout's own), and kills it at its timeout where the container has a
// timeout command (coreutils, busybox), with what it started: busybox's kills
// the command alone, so the rest of the group goes after it. Its arguments
// are the timeout in seconds, a file for the process id of the run, which a
// cancel kills and only the user reads, and the command. The file goes when
// the run ends.
const taskScript = `t=$1; f=$2; shift 2
g=; command -v setsid >/dev/null 2>&1 && g=setsid
if command -v timeout >/dev/null 2>&1; then $g timeout -s KILL "$t" sh -c "$1" 2>&1 & else $g sh -c "$1" 2>&1 & fi
p=$!
(umask 077; echo "$p" > "$f") 2>/dev/null
wait "$p"; s=$?
if [ "$s" -eq 124 ] || [ "$s" -eq 137 ]; then kill -KILL "-$p" 2>/dev/null; fi
rm -f "$f"
exit "$s"`

// taskKill kills a run that was cancelled or given up on: its process group,
// as timeout leads one, or else its process. It runs as the task's user, who
// may signal its own processes where the container has no capabilities, and
// no others, and trusts the file only when it is the user's own, as another
// user of the container may have put one there. A run that has just started
// is given a moment to write it. dash's kill takes no "--".
const taskKill = `for _ in 1 2 3 4 5; do [ -s "$1" ] && break; sleep 0.2 2>/dev/null || sleep 1; done
[ -O "$1" ] || exit 0
p=$(cat "$1" 2>/dev/null)
case $p in '' | *[!0-9]* | 0 | 1) exit 0 ;; esac
kill -KILL "-$p" 2>/dev/null || kill -KILL "$p" 2>/dev/null
rm -f "$1"`

// taskID is a scheduled task's record id.
var taskID = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

func (a *Agent) registerTaskHandlers() {
	a.Executor.Handle("task.run", a.handleTaskRun)
}

func (a *Agent) handleTaskRun(ctx context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	payload := command.GetTaskRun()
	container, script := payload.GetContainerId(), payload.GetCommand()
	if container == "" || strings.TrimSpace(script) == "" {
		return nil, errors.New("a task needs a container and a command")
	}
	if !taskID.MatchString(payload.GetTaskId()) {
		return nil, errors.New("the task's id is not one")
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

	output := &tailBuffer{limit: taskOutputTail, mask: maskValues(payload.GetMasked())}
	pidFile := "/tmp/pocketcloud-task-" + payload.GetTaskId() + ".pid"
	runCtx, cancel := context.WithTimeout(ctx, timeout+taskGrace)
	defer cancel()
	started := time.Now()
	ran, err := a.Docker.Run(runCtx, container, dockerapi.ExecOptions{
		Cmd:        []string{"sh", "-c", taskScript, "pocketcloud-task", strconv.Itoa(int(timeout.Seconds())), pidFile, script},
		User:       payload.GetUser(),
		WorkingDir: payload.GetWorkingDir(),
	}, output)
	elapsed := time.Since(started)
	result := map[string]any{"duration_ms": float64(elapsed.Milliseconds()), "output": output.String()}
	if err != nil && runCtx.Err() != nil {
		// docker exec cannot stop what it started: the run is killed here, so
		// that the next one never starts next to it
		a.killTask(container, payload.GetUser(), pidFile)
	}
	switch {
	case err != nil && dockerapi.IsNotFound(err):
		return result, fmt.Errorf("the container %s does not exist", container)
	case err != nil && ctx.Err() != nil:
		return result, ctx.Err()
	case err != nil && runCtx.Err() != nil:
		result["timed_out"] = true
		return result, fmt.Errorf("the task ran past its timeout of %v and was killed", timeout)
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

// killTask kills a run by the process id it left, as its user.
func (a *Agent) killTask(container, user, pidFile string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := a.Docker.Run(ctx, container, dockerapi.ExecOptions{
		Cmd: []string{"sh", "-c", taskKill, "pocketcloud-task-kill", pidFile}, User: user,
	}, io.Discard); err != nil {
		a.Log.Warn("a task that was given up on could not be killed", "container", container, "error", err)
	}
}

// maskValues are the secrets worth hiding: a very short one would hide
// ordinary text.
func maskValues(values []string) [][]byte {
	masked := make([][]byte, 0, len(values))
	for _, value := range values {
		if len(value) >= minMaskedSecret {
			masked = append(masked, []byte(value))
		}
	}
	return masked
}

// taskMaskMargin is what the buffer keeps beyond its limit, so that a secret
// cut where the buffer was trimmed falls in the part that is dropped.
const taskMaskMargin = 1 << 10

// tailBuffer keeps the end of what is written to it, with secrets hidden.
type tailBuffer struct {
	limit     int
	mask      [][]byte
	data      []byte
	truncated bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	if keep := b.limit + taskMaskMargin; len(b.data) > 2*keep {
		b.data = append(b.data[:0], b.data[len(b.data)-keep:]...)
		b.truncated = true
	}
	return len(p), nil
}

// String is the last limit bytes, whole characters only, with secrets
// hidden in the whole of what was kept before it is cut.
func (b *tailBuffer) String() string {
	data := b.data
	if b.truncated {
		// where the buffer was trimmed, the end of a secret may be left
		data = data[secretTail(data, b.mask):]
	}
	for _, secret := range b.mask {
		data = bytes.ReplaceAll(data, secret, []byte("***"))
	}
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

// secretTail is the length of the longest end of a secret that data starts
// with.
func secretTail(data []byte, secrets [][]byte) int {
	longest := 0
	for _, secret := range secrets {
		for n := min(len(secret)-1, len(data)); n > longest; n-- {
			if bytes.HasPrefix(data, secret[len(secret)-n:]) {
				longest = n
				break
			}
		}
	}
	return longest
}
