package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
)

const (
	logBatchInterval = 200 * time.Millisecond
	logBatchLines    = 200
	// logRateLimit is the most lines per second sent for one session. A container
	// that writes more loses lines, with a notice, rather than swamp the panel.
	logRateLimit = 2000

	defaultLogTail = 200

	hostShellEnv   = "HOST_SHELL_ENABLED"
	hostShellImage = "alpine:3.22"
	execChunkSize  = 32 << 10
)

// handleLogsFollow opens the log stream of a session. It finishes as soon as the
// stream is open; the lines keep flowing until logs.stop or a lost connection.
func (a *Agent) handleLogsFollow(_ context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	payload := command.GetLogsFollow()

	client := a.client.Load()
	if client == nil {
		return nil, errors.New("not connected")
	}

	ctx, cancel := context.WithCancel(context.Background())

	stream, err := (*client).StreamLogs(ctx)
	if err != nil {
		cancel()
		return nil, err
	}

	a.logSessions.add(payload.GetSessionId(), cancel)

	go a.followLogs(ctx, cancel, payload, stream)

	return map[string]any{"session": payload.GetSessionId()}, nil
}

func (a *Agent) handleLogsStop(_ context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	a.logSessions.stop(command.GetLogsStop().GetSessionId())
	return nil, nil
}

func (a *Agent) followLogs(ctx context.Context, cancel context.CancelFunc, payload *agentv1.LogsFollow, stream agentv1.AgentService_StreamLogsClient) {
	defer cancel()
	defer a.logSessions.remove(payload.GetSessionId())

	sessionID := payload.GetSessionId()

	tail := int(payload.GetTail())
	if tail <= 0 {
		tail = defaultLogTail
	}

	lines := make(chan *agentv1.LogLine, logBatchLines*4)

	var readers sync.WaitGroup
	for _, id := range payload.GetContainerIds() {
		readers.Add(1)
		go func() {
			defer readers.Done()
			if err := a.readContainerLogs(ctx, id, tail, lines); err != nil && ctx.Err() == nil {
				a.Log.Debug("a log stream ended", "container", id, "error", err)
			}
		}()
	}

	go func() {
		readers.Wait()
		close(lines)
	}()

	a.forwardLogs(ctx, sessionID, lines, stream)

	if _, err := stream.CloseAndRecv(); err != nil && ctx.Err() == nil {
		a.Log.Debug("closing a log stream failed", "session", sessionID, "error", err)
	}
}

// forwardLogs batches lines and applies the rate limit of a session.
func (a *Agent) forwardLogs(ctx context.Context, sessionID string, lines <-chan *agentv1.LogLine, stream agentv1.AgentService_StreamLogsClient) {
	ticker := time.NewTicker(logBatchInterval)
	defer ticker.Stop()

	batch := make([]*agentv1.LogLine, 0, logBatchLines)
	windowStart := time.Now()
	sent := 0
	dropped := 0

	flush := func() bool {
		if dropped > 0 && time.Since(windowStart) >= time.Second {
			batch = append(batch, &agentv1.LogLine{
				Ts:     timestamppb.Now(),
				Stream: "stderr",
				Text:   fmt.Sprintf("[PocketCloud: %d lines were dropped, the rate limit is %d lines per second]", dropped, logRateLimit),
			})
			dropped = 0
		}
		if len(batch) == 0 {
			return true
		}
		err := stream.Send(&agentv1.StreamLogsRequest{SessionId: sessionID, Lines: batch})
		batch = batch[:0]
		return err == nil
	}

	// the first message identifies the session even if nothing has been logged yet
	if err := stream.Send(&agentv1.StreamLogsRequest{SessionId: sessionID}); err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-lines:
			if !ok {
				flush()
				return
			}

			if time.Since(windowStart) >= time.Second {
				windowStart = time.Now()
				sent = 0
			}
			if sent >= logRateLimit {
				dropped++
				continue
			}
			sent++

			batch = append(batch, line)
			if len(batch) >= logBatchLines && !flush() {
				return
			}
		case <-ticker.C:
			if !flush() {
				return
			}
		}
	}
}

func (a *Agent) readContainerLogs(ctx context.Context, id string, tail int, lines chan<- *agentv1.LogLine) error {
	info, err := a.Docker.Inspect(ctx, id)
	if err != nil {
		return err
	}

	name := strings.TrimPrefix(info.Name, "/")
	service := ""
	if labels, ok := info.Config["Labels"].(map[string]any); ok {
		service, _ = labels["com.docker.compose.service"].(string)
	}
	tty, _ := info.Config["Tty"].(bool)

	reader, err := a.Docker.Logs(ctx, id, dockerapi.LogOptions{Tail: tail, Follow: true, Timestamps: true})
	if err != nil {
		return err
	}
	defer reader.Close()

	emit := func(stream string, text string) {
		line := parseLogLine(text)
		line.Container = name
		line.Service = service
		line.Stream = stream

		select {
		case lines <- line:
		case <-ctx.Done():
		}
	}

	if tty {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for scanner.Scan() {
			emit("stdout", scanner.Text())
		}
		return scanner.Err()
	}

	// a frame may end in the middle of a line, and may hold several lines
	var stdoutTail, stderrTail bytes.Buffer

	return dockerapi.ReadMultiplexed(reader, func(stream byte, data []byte) {
		name, pending := "stdout", &stdoutTail
		if stream == 2 {
			name, pending = "stderr", &stderrTail
		}

		pending.Write(data)
		for {
			index := bytes.IndexByte(pending.Bytes(), '\n')
			if index < 0 {
				return
			}
			emit(name, string(pending.Next(index)))
			pending.Next(1)
		}
	})
}

// parseLogLine splits the timestamp Docker puts in front of a line.
func parseLogLine(text string) *agentv1.LogLine {
	text = strings.TrimRight(text, "\r")

	if stamp, rest, ok := strings.Cut(text, " "); ok {
		if at, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
			return &agentv1.LogLine{Ts: timestamppb.New(at), Text: rest}
		}
	}

	return &agentv1.LogLine{Ts: timestamppb.Now(), Text: text}
}

// handleExecStart opens a terminal session. Host shells are refused unless they
// were enabled on this agent: from a container they need a privileged helper.
func (a *Agent) handleExecStart(_ context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	payload := command.GetExecStart()

	client := a.client.Load()
	if client == nil {
		return nil, errors.New("not connected")
	}

	if payload.GetHostShell() && os.Getenv(hostShellEnv) != "1" {
		return nil, fmt.Errorf("host shells are disabled on this server: start the agent with %s=1 to allow them", hostShellEnv)
	}

	ctx, cancel := context.WithCancel(context.Background())

	stream, err := (*client).Exec(ctx)
	if err != nil {
		cancel()
		return nil, err
	}

	a.execSessions.add(payload.GetSessionId(), cancel)

	go func() {
		defer cancel()
		defer a.execSessions.remove(payload.GetSessionId())
		a.runTerminal(ctx, payload, stream)
	}()

	return map[string]any{"session": payload.GetSessionId()}, nil
}

func (a *Agent) handleExecStop(_ context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	a.execSessions.stop(command.GetExecStop().GetSessionId())
	return nil, nil
}

func (a *Agent) runTerminal(ctx context.Context, payload *agentv1.ExecStart, stream agentv1.AgentService_ExecClient) {
	sessionID := payload.GetSessionId()

	fail := func(err error) {
		if sendErr := stream.Send(&agentv1.ExecRequest{SessionId: sessionID, Error: err.Error(), Exited: true, ExitCode: -1}); sendErr != nil {
			a.Log.Debug("failed to report a terminal error", "error", sendErr)
		}
	}

	if err := stream.Send(&agentv1.ExecRequest{SessionId: sessionID}); err != nil {
		return
	}

	target := payload.GetContainerId()
	cmd := payload.GetCommand()

	if payload.GetHostShell() {
		helper, err := a.startHostShellHelper(ctx)
		if err != nil {
			fail(err)
			return
		}
		defer func() {
			if err := a.Docker.Remove(context.Background(), helper, false, true); err != nil {
				a.Log.Warn("failed to remove the host shell helper", "error", err)
			}
		}()
		target = helper
		cmd = []string{"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "/bin/sh", "-l"}
	}

	if len(cmd) == 0 {
		cmd = []string{"/bin/sh"}
	}

	execID, err := a.Docker.ExecCreate(ctx, target, dockerapi.ExecOptions{
		Cmd:   cmd,
		User:  payload.GetUser(),
		TTY:   true,
		Stdin: true,
		Env:   []string{"TERM=xterm-256color"},
	})
	if err != nil {
		fail(err)
		return
	}

	connection, err := a.Docker.ExecStart(ctx, execID, true)
	if err != nil {
		fail(err)
		return
	}
	defer connection.Close()

	if size := payload.GetSize(); size != nil && size.GetCols() > 0 {
		if err := a.Docker.ExecResize(ctx, execID, size.GetCols(), size.GetRows()); err != nil {
			a.Log.Debug("failed to set the initial terminal size", "error", err)
		}
	}

	go a.terminalInput(ctx, execID, connection, stream)

	buffer := make([]byte, execChunkSize)
	for {
		count, err := connection.Read(buffer)
		if count > 0 {
			if sendErr := stream.Send(&agentv1.ExecRequest{Output: bytes.Clone(buffer[:count])}); sendErr != nil {
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				a.Log.Debug("a terminal session broke", "error", err)
			}
			break
		}
	}

	exitCode := 0
	if _, code, err := a.Docker.ExecInspect(context.Background(), execID); err == nil {
		exitCode = code
	}

	if err := stream.Send(&agentv1.ExecRequest{Exited: true, ExitCode: int32(exitCode)}); err != nil { //nolint:gosec // an exit code
		a.Log.Debug("failed to report the end of a terminal session", "error", err)
	}
	if err := stream.CloseSend(); err != nil {
		a.Log.Debug("failed to close a terminal stream", "error", err)
	}
}

func (a *Agent) terminalInput(ctx context.Context, execID string, connection io.WriteCloser, stream agentv1.AgentService_ExecClient) {
	for {
		message, err := stream.Recv()
		if err != nil {
			connection.Close()
			return
		}

		if input := message.GetInput(); len(input) > 0 {
			if _, err := connection.Write(input); err != nil {
				return
			}
		}
		if size := message.GetResize(); size != nil && size.GetCols() > 0 {
			if err := a.Docker.ExecResize(ctx, execID, size.GetCols(), size.GetRows()); err != nil {
				a.Log.Debug("failed to resize a terminal", "error", err)
			}
		}
		if message.GetClose() {
			connection.Close()
			return
		}
	}
}

// startHostShellHelper starts the privileged container that makes a host shell
// possible from a containerised agent. It is removed when the session ends.
func (a *Agent) startHostShellHelper(ctx context.Context) (string, error) {
	if err := a.Docker.Pull(ctx, hostShellImage, nil, nil); err != nil {
		return "", fmt.Errorf("failed to pull the host shell helper: %w", err)
	}

	id, err := a.Docker.Create(ctx, "", map[string]any{
		"Image":  hostShellImage,
		"Cmd":    []string{"sleep", "86400"},
		"Labels": map[string]string{"pocketcloud.role": "job"},
		"HostConfig": map[string]any{
			"Privileged": true,
			"PidMode":    "host",
			"AutoRemove": true,
		},
	})
	if err != nil {
		return "", err
	}

	if err := a.Docker.Start(ctx, id); err != nil {
		return "", err
	}

	return id, nil
}
