package agent

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
)

// TestATasksOutputHidesItsSecrets: the output of a run, which viewers read,
// never shows a secret of its container, whole or in part where the output
// was cut (#51).
func TestATasksOutputHidesItsSecrets(t *testing.T) {
	const secret = "s3cr3t-database-password"
	b := &tailBuffer{limit: 64, mask: maskValues([]string{secret, "abc"})}
	_, _ = b.Write([]byte("connecting with " + secret + "\nabc stays, being too short to hide\n"))
	if text := b.String(); strings.Contains(text, secret) || !strings.Contains(text, "***") || !strings.Contains(text, "abc stays") {
		t.Errorf("short output: %q", text)
	}

	// a long output, trimmed in the middle of a secret, and then shortened by
	// the secrets it hides, so that what was trimmed would show
	for cut := 1; cut < len(secret); cut++ {
		b := &tailBuffer{limit: 200, mask: maskValues([]string{secret})}
		keep := b.limit + taskMaskMargin
		rest := keep + cut - len(secret)
		output := strings.Repeat("x", keep+1-cut) + secret +
			strings.Repeat(secret, rest/len(secret)) + strings.Repeat("y", rest%len(secret))
		_, _ = b.Write([]byte(output))
		if !b.truncated || len(b.data) != keep {
			t.Fatalf("cut %d: the buffer was not trimmed: %d", cut, len(b.data))
		}
		text := b.String()
		for n := minMaskedSecret; n < len(secret); n++ {
			if strings.Contains(text, secret[len(secret)-n:]) || strings.Contains(text, secret[:n]) {
				t.Fatalf("cut %d: a part of the secret shows: %q", cut, text)
			}
		}
		if !strings.HasPrefix(text, "…") {
			t.Errorf("cut %d: a cut output does not say so: %q", cut, text)
		}
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestATaskThatIsCancelledIsKilled: docker exec cannot stop what it started,
// so a run that is cancelled is killed by the process id it left, as its user,
// and a second run of a task never starts next to the first (#51).
func TestATaskThatIsCancelledIsKilled(t *testing.T) {
	engine := fakedocker.New(t)
	engine.AddContainer("blog-php", "pocketcloud/php:8.4", nil)
	release := make(chan struct{})
	engine.OnExec = func(call *fakedocker.ExecCall, _ []byte) fakedocker.ExecResult {
		if len(call.Cmd) > 3 && call.Cmd[3] == "pocketcloud-task" {
			<-release
		}
		return fakedocker.ExecResult{}
	}
	t.Cleanup(func() { close(release) })
	client, err := dockerapi.New(engine.Host())
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{Docker: client, Log: slog.New(slog.DiscardHandler)}
	command := &agentv1.Command{Payload: &agentv1.Command_TaskRun{TaskRun: &agentv1.TaskRun{
		TaskId: "task1", ContainerId: "blog-php", User: "www-data", Command: "sleep 600",
		TimeoutS: 600, WorkingDir: "/srv/sites/blog/current",
	}}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.handleTaskRun(ctx, command, nil)
		done <- err
	}()
	waitFor(t, func() bool { return engine.LastExec("blog-php") != nil })
	run := engine.LastExec("blog-php")
	if run.User != "www-data" || run.WorkingDir != "/srv/sites/blog/current" || len(run.Cmd) != 7 ||
		run.Cmd[4] != "600" || run.Cmd[5] != "/tmp/pocketcloud-task-task1.pid" || run.Cmd[6] != "sleep 600" {
		t.Fatalf("run: %+v", run)
	}
	if _, err := a.handleTaskRun(context.Background(), command, nil); err == nil || !strings.Contains(err.Error(), "still going") {
		t.Errorf("a second run: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "canceled") {
			t.Errorf("a cancelled run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled run did not end")
	}
	kill := engine.LastExec("blog-php")
	if kill == run || kill.User != "www-data" || len(kill.Cmd) != 5 || kill.Cmd[3] != "pocketcloud-task-kill" || kill.Cmd[4] != "/tmp/pocketcloud-task-task1.pid" {
		t.Errorf("kill: %+v", kill)
	}
	if _, running := a.tasks.Load("task1"); running {
		t.Error("the task is still held after its run ended")
	}
}

// TestATasksRunIsJudgedByItsExit: timeout's own status at the timeout is a
// run that was killed there; another status is a failure (#51).
func TestATasksRunIsJudgedByItsExit(t *testing.T) {
	engine := fakedocker.New(t)
	engine.AddContainer("blog-php", "pocketcloud/php:8.4", nil)
	engine.OnExec = func(call *fakedocker.ExecCall, _ []byte) fakedocker.ExecResult {
		switch call.Cmd[6] {
		case "sleep 5":
			time.Sleep(time.Second)
			return fakedocker.ExecResult{Code: 137, Stdout: "started\n"}
		case "false":
			return fakedocker.ExecResult{Code: 1, Stdout: "boom\n"}
		}
		return fakedocker.ExecResult{Stdout: "Success: Executed a total of 2 cron events.\n"}
	}
	client, err := dockerapi.New(engine.Host())
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{Docker: client, Log: slog.New(slog.DiscardHandler)}
	run := func(script string, timeout int32) (map[string]any, error) {
		return a.handleTaskRun(context.Background(), &agentv1.Command{Payload: &agentv1.Command_TaskRun{TaskRun: &agentv1.TaskRun{
			TaskId: "task1", ContainerId: "blog-php", Command: script, TimeoutS: timeout,
		}}}, nil)
	}
	if result, err := run("wp cron event run --due-now", 300); err != nil || result["exit_code"] != float64(0) || !strings.Contains(result["output"].(string), "2 cron events") {
		t.Errorf("a run that succeeds: %v %v", result, err)
	}
	if result, err := run("false", 300); err == nil || result["exit_code"] != float64(1) || result["timed_out"] != nil {
		t.Errorf("a run that fails: %v %v", result, err)
	}
	if result, err := run("sleep 5", 1); err == nil || result["timed_out"] != true || result["output"] != "started\n" {
		t.Errorf("a run killed at its timeout: %v %v", result, err)
	}
	for _, bad := range []*agentv1.TaskRun{
		{TaskId: "task1", ContainerId: "blog-php", TimeoutS: 300},
		{TaskId: "../x", ContainerId: "blog-php", Command: "true", TimeoutS: 300},
		{TaskId: "task1", ContainerId: "blog-php", Command: "true", TimeoutS: 0},
	} {
		if _, err := a.handleTaskRun(context.Background(), &agentv1.Command{Payload: &agentv1.Command_TaskRun{TaskRun: bad}}, nil); err == nil {
			t.Errorf("%+v ran", bad)
		}
	}
}
