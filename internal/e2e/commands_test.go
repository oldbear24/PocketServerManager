package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/agent"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

func pull(image string) *agentv1.Command {
	return &agentv1.Command{Payload: &agentv1.Command_ImagePull{ImagePull: &agentv1.ImagePull{Image: image}}}
}

func (h *harness) create(hostID string, command *agentv1.Command) *core.Record {
	h.t.Helper()

	record, err := h.panel.Commands.Create(context.Background(), commands.Options{Host: hostID, Command: command, Requester: "test"})
	if err != nil {
		h.t.Fatal(err)
	}

	return record
}

func TestCommandRunsAndReportsItsResult(t *testing.T) {
	h := newHarness(t)

	_, host := h.enrolled("runner")

	id := h.docker.AddContainer("web", "nginx:1", nil)

	record, err := h.panel.Commands.Create(context.Background(), commands.Options{
		Host:    host.Id,
		Label:   "web",
		Command: &agentv1.Command{Payload: &agentv1.Command_ContainerStop{ContainerStop: &agentv1.ContainerStop{DockerId: id}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	done := h.waitForStatus(record.Id, schema.CommandSucceeded)

	if done.GetInt("attempts") != 1 {
		t.Errorf("attempts: %d", done.GetInt("attempts"))
	}
	if done.GetDateTime("dispatched_at").IsZero() || done.GetDateTime("started_at").IsZero() || done.GetDateTime("finished_at").IsZero() {
		t.Errorf("the timeline is incomplete: %v", done.FieldsData())
	}
	if done.GetString("host_name") != "runner" || done.GetString("correlation_id") != done.Id {
		t.Errorf("defaults: %v", done.FieldsData())
	}

	if h.docker.Find("web").Status != "exited" {
		t.Error("the container was not stopped")
	}
}

func TestSystemContainersAreProtected(t *testing.T) {
	h := newHarness(t)

	_, host := h.enrolled("protected")

	for _, role := range []string{"agent", "updater", "edge", "job"} {
		id := h.docker.AddContainer("system-"+role, "x", map[string]string{"pocketcloud.role": role})

		record := h.create(host.Id, &agentv1.Command{Payload: &agentv1.Command_ContainerRemove{ContainerRemove: &agentv1.ContainerRemove{DockerId: id, Force: true}}})

		failed := h.waitForStatus(record.Id, schema.CommandFailed)
		if !strings.Contains(failed.GetString("error"), "system container") {
			t.Errorf("error: %s", failed.GetString("error"))
		}
		if h.docker.Find("system-"+role) == nil {
			t.Errorf("the %s container was removed", role)
		}
	}
}

// TestReplayedCommandIsNotExecutedTwice is the acknowledged-replay path: the agent
// finished the command, but the panel never heard about it.
func TestReplayedCommandIsNotExecutedTwice(t *testing.T) {
	h := newHarness(t)

	var executed atomic.Int32
	handler := func(context.Context, *agentv1.Command, *agent.Reporter) (map[string]any, error) {
		executed.Add(1)
		return map[string]any{"done": true}, nil
	}

	running, host := h.enrolled("replay", func(a *agent.Agent) { a.Executor.Handle("image.pull", handler) })

	record := h.create(host.Id, pull("nginx:1"))
	h.waitForStatus(record.Id, schema.CommandSucceeded)

	if err := running.stop(); err != nil {
		t.Fatal(err)
	}

	// make the panel believe that the command never finished, by writing directly
	if _, err := h.app.DB().NewQuery("UPDATE commands SET status='dispatched', finished_at='' WHERE id={:id}").Bind(dbx.Params{"id": record.Id}).Execute(); err != nil {
		t.Fatal(err)
	}

	second := h.start(running.config, func(a *agent.Agent) { a.Executor.Handle("image.pull", handler) })
	defer func() { _ = second.stop() }()

	done := h.waitForStatus(record.Id, schema.CommandSucceeded)
	if done.GetInt("attempts") > 2 {
		t.Errorf("attempts: %d", done.GetInt("attempts"))
	}

	// the agent's own view stays the same as well
	time.Sleep(200 * time.Millisecond)
	if executed.Load() != 1 {
		t.Fatalf("the command ran %d times", executed.Load())
	}
}

// TestKillingTheAgentMidCommand: the command never stays open, and never runs twice.
func TestKillingTheAgentMidCommand(t *testing.T) {
	h := newHarness(t)

	var executed atomic.Int32
	started := make(chan struct{}, 4)
	block := func(ctx context.Context, _ *agentv1.Command, _ *agent.Reporter) (map[string]any, error) {
		executed.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	// a crash cannot be simulated by a graceful stop, which reports the cancel; the
	// final report is what is lost in a crash, so it is blocked here
	running, host := h.enrolled("killed", func(a *agent.Agent) { a.Executor.Handle("image.pull", block) })

	record := h.create(host.Id, pull("nginx:1"))
	<-started

	// the process dies: what is on disk stays, and the report never goes out
	running.Executor.SetClient(nil)
	if err := running.stop(); err != nil {
		t.Fatal(err)
	}

	// the store now says "cancelled"; put back what a kill -9 leaves behind
	restoreStarted(t, running.config.StateDir, record.Id)

	if status := h.command(record.Id).GetString("status"); status == schema.CommandSucceeded {
		t.Fatalf("status right after the kill: %s", status)
	}

	second := h.start(running.config, func(a *agent.Agent) { a.Executor.Handle("image.pull", block) })
	defer func() { _ = second.stop() }()

	failed := h.waitForStatus(record.Id, schema.CommandFailed)
	if !strings.Contains(failed.GetString("error"), "restarted while running") {
		t.Errorf("error: %s", failed.GetString("error"))
	}
	if executed.Load() != 1 {
		t.Fatalf("the command ran %d times", executed.Load())
	}
}

// TestGracefulStopReportsRunningCommands is what SIGTERM does.
func TestGracefulStopReportsRunningCommands(t *testing.T) {
	h := newHarness(t)

	started := make(chan struct{})
	block := func(ctx context.Context, _ *agentv1.Command, _ *agent.Reporter) (map[string]any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	running, host := h.enrolled("graceful", func(a *agent.Agent) { a.Executor.Handle("image.pull", block) })

	record := h.create(host.Id, pull("nginx:1"))
	<-started

	if err := running.stop(); err != nil {
		t.Fatal(err)
	}

	if status := h.command(record.Id).GetString("status"); status != schema.CommandCancelled {
		t.Fatalf("status after a graceful stop: %s", status)
	}
}

func TestLongOutputIsStoredAsAFile(t *testing.T) {
	h := newHarness(t)

	const line = "a line of output that the command wrote\n"

	handler := func(_ context.Context, _ *agentv1.Command, reporter *agent.Reporter) (map[string]any, error) {
		for range 5000 {
			if _, err := reporter.Write([]byte(line)); err != nil {
				return nil, err
			}
		}
		return map[string]any{"ok": true}, nil
	}

	_, host := h.enrolled("output", func(a *agent.Agent) { a.Executor.Handle("image.pull", handler) })

	record := h.create(host.Id, pull("nginx:1"))
	done := h.waitForStatus(record.Id, schema.CommandSucceeded)

	if done.GetString("output") == "" {
		t.Fatal("the output was not stored")
	}

	fsys, err := h.app.NewFilesystem()
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()

	reader, err := fsys.GetReader(done.BaseFilesPath() + "/" + done.GetString("output"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 5000*len(line) {
		t.Fatalf("stored %d bytes, want %d", len(raw), 5000*len(line))
	}
}

func TestAgentThatStaysAwayEndsInTimedOut(t *testing.T) {
	h := newHarness(t)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	block := func(context.Context, *agentv1.Command, *agent.Reporter) (map[string]any, error) {
		<-release
		return nil, nil
	}

	running, host := h.enrolled("silent", func(a *agent.Agent) { a.Executor.Handle("image.pull", block) })

	record, err := h.panel.Commands.Create(context.Background(), commands.Options{
		Host: host.Id, Command: pull("nginx:1"), Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	h.waitForStatus(record.Id, schema.CommandDispatched, schema.CommandRunning)

	running.Executor.SetClient(nil)
	running.cancel()

	h.waitFor("the command to time out", func() bool {
		h.panel.Commands.Reap()
		return h.command(record.Id).GetString("status") == schema.CommandTimedOut
	})
}

func TestQueuedCommandForAnOfflineHostExpires(t *testing.T) {
	h := newHarness(t)

	host := testutil.NewHost(t, h.app, "never-there")

	record := h.create(host.Id, pull("nginx:1"))
	if status := record.GetString("status"); status != schema.CommandQueued {
		t.Fatalf("status: %s", status)
	}

	h.panel.Commands.Reap()
	if status := h.command(record.Id).GetString("status"); status != schema.CommandQueued {
		t.Fatalf("expired too early: %s", status)
	}

	fresh := h.command(record.Id)
	fresh.Set("expires_at", time.Now().Add(-time.Second).UTC().Format("2006-01-02 15:04:05.000Z"))
	if err := h.app.Save(fresh); err != nil {
		t.Fatal(err)
	}

	h.panel.Commands.Reap()

	timedOut := h.command(record.Id)
	if timedOut.GetString("status") != schema.CommandTimedOut || !strings.Contains(timedOut.GetString("error"), "offline") {
		t.Fatalf("%v", timedOut.FieldsData())
	}
}

func TestQueuedCommandIsDeliveredWhenTheAgentConnects(t *testing.T) {
	h := newHarness(t)

	running, host := h.enrolled("late")
	if err := running.stop(); err != nil {
		t.Fatal(err)
	}

	record := h.create(host.Id, pull("nginx:1"))
	time.Sleep(100 * time.Millisecond)
	if status := h.command(record.Id).GetString("status"); status != schema.CommandQueued {
		t.Fatalf("status while offline: %s", status)
	}

	second := h.start(running.config)
	defer func() { _ = second.stop() }()

	h.waitForStatus(record.Id, schema.CommandSucceeded)
}

func TestSameResourceRunsInOrderOthersInParallel(t *testing.T) {
	h := newHarness(t)

	var (
		mu      sync.Mutex
		order   []string
		running = map[string]int{}
		overlap bool
	)

	release := make(chan struct{})

	handler := func(_ context.Context, command *agentv1.Command, _ *agent.Reporter) (map[string]any, error) {
		name := command.GetStackRestart().GetName()

		mu.Lock()
		order = append(order, name)
		running[name]++
		if running[name] > 1 {
			overlap = true
		}
		mu.Unlock()

		<-release

		mu.Lock()
		running[name]--
		mu.Unlock()

		return nil, nil
	}

	_, host := h.enrolled("ordered", func(a *agent.Agent) { a.Executor.Handle("stack.restart", handler) })

	restart := func(name string) *agentv1.Command {
		return &agentv1.Command{Payload: &agentv1.Command_StackRestart{StackRestart: &agentv1.StackRestart{Name: name}}}
	}

	first := h.create(host.Id, restart("alpha"))
	second := h.create(host.Id, restart("alpha"))
	other := h.create(host.Id, restart("beta"))

	if first.GetString("resource") != "stack:alpha" {
		t.Errorf("resource: %q", first.GetString("resource"))
	}

	// the other stack starts while the first one is still running
	h.waitFor("both stacks to run", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return running["alpha"] == 1 && running["beta"] == 1
	})

	if status := h.command(second.Id).GetString("status"); status != schema.CommandQueued {
		t.Errorf("the second command of a stack must wait: %s", status)
	}

	close(release)

	for _, id := range []string{first.Id, second.Id, other.Id} {
		h.waitForStatus(id, schema.CommandSucceeded)
	}

	mu.Lock()
	defer mu.Unlock()
	if overlap {
		t.Error("two commands of the same stack ran at once")
	}
	if len(order) != 3 {
		t.Errorf("order: %v", order)
	}
}

func TestUnsupportedCommandFailsAtOnce(t *testing.T) {
	h := newHarness(t)

	_, host := h.enrolled("old-agent")

	// no agent of this version can run these yet
	record := h.create(host.Id, &agentv1.Command{Payload: &agentv1.Command_SiteRemove{SiteRemove: &agentv1.SiteRemove{Site: "x"}}})

	failed := h.waitForStatus(record.Id, schema.CommandFailed)
	if !strings.Contains(failed.GetString("error"), "does not support") {
		t.Errorf("error: %s", failed.GetString("error"))
	}
}

func TestCancellingACommand(t *testing.T) {
	h := newHarness(t)

	cancelled := make(chan struct{})
	started := make(chan struct{})

	handler := func(ctx context.Context, _ *agentv1.Command, _ *agent.Reporter) (map[string]any, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}

	_, host := h.enrolled("cancel", func(a *agent.Agent) { a.Executor.Handle("image.pull", handler) })

	_, operator := testutil.NewUser(t, h.app, "operator@example.com", schema.RoleOperator)
	_, viewer := testutil.NewUser(t, h.app, "viewer@example.com", schema.RoleViewer)

	record := h.create(host.Id, pull("nginx:1"))
	<-started

	url := "/api/pc/commands/" + record.Id + "/cancel"

	if status, _ := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: url, Token: viewer}); status != http.StatusForbidden {
		t.Fatalf("a viewer cancelled a command: %d", status)
	}

	if status, body := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: url, Token: operator}); status != http.StatusNoContent {
		t.Fatalf("%d %s", status, body)
	}

	select {
	case <-cancelled:
	case <-time.After(waitLimit):
		t.Fatal("the agent did not stop the command")
	}

	if status := h.command(record.Id).GetString("status"); status != schema.CommandCancelled {
		t.Fatalf("status: %s", status)
	}

	if status, _ := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: url, Token: operator}); status != http.StatusBadRequest {
		t.Errorf("cancelling a finished command: %d", status)
	}
}

func TestStateMachineIsEnforced(t *testing.T) {
	h := newHarness(t)

	host := testutil.NewHost(t, h.app, "transitions")
	record := h.create(host.Id, pull("nginx:1"))

	record.Set("status", schema.CommandSucceeded)
	if err := h.app.Save(record); err == nil {
		t.Error("queued → succeeded must be refused")
	}

	fresh := h.command(record.Id)
	fresh.Set("status", schema.CommandCancelled)
	if err := h.app.Save(fresh); err != nil {
		t.Fatal(err)
	}

	fresh = h.command(record.Id)
	fresh.Set("status", schema.CommandQueued)
	if err := h.app.Save(fresh); err == nil {
		t.Error("a finished command must not start over")
	}
}

func TestAPICreatesCleanCommands(t *testing.T) {
	h := newHarness(t)

	host := testutil.NewHost(t, h.app, "api-commands")

	operator, operatorToken := testutil.NewUser(t, h.app, "operator@example.com", schema.RoleOperator)
	_, viewerToken := testutil.NewUser(t, h.app, "viewer@example.com", schema.RoleViewer)
	_, adminToken := testutil.NewUser(t, h.app, "admin@example.com", schema.RoleAdmin)

	base := "/api/collections/commands/records"
	body := func(commandType, payload string) string {
		return fmt.Sprintf(`{"host":%q,"type":%q,"payload":%s,"status":"succeeded","attempts":99,"requested_by":"someone"}`, host.Id, commandType, payload)
	}

	if status, _ := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: base, Token: viewerToken, Body: body("container.start", `{"docker_id":"x"}`)}); status == http.StatusOK {
		t.Error("a viewer created a command")
	}

	status, response := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: base, Token: operatorToken, Body: body("container.start", `{"docker_id":"x"}`)})
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, response)
	}
	if !strings.Contains(response, `"status":"queued"`) || !strings.Contains(response, `"attempts":0`) || !strings.Contains(response, operator.Id) {
		t.Errorf("the fields of the queue must not come from the request: %s", response)
	}

	for name, tc := range map[string]struct {
		token  string
		body   string
		status int
	}{
		"admin-only type":     {operatorToken, body("agent.restart", `{}`), http.StatusForbidden},
		"admin may":           {adminToken, body("agent.restart", `{}`), http.StatusOK},
		"internal type":       {adminToken, body("stack.deploy", `{"name":"x"}`), http.StatusForbidden},
		"unknown type":        {adminToken, body("rm.everything", `{}`), http.StatusBadRequest},
		"payload of nonsense": {adminToken, body("container.start", `{"nope":1}`), http.StatusBadRequest},
		"no host":             {adminToken, `{"type":"container.start","payload":{"docker_id":"x"}}`, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			status, response := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: base, Token: tc.token, Body: tc.body})
			if status != tc.status {
				t.Fatalf("%d, want %d: %s", status, tc.status, response)
			}
		})
	}

	// no one edits a command through the API
	record := h.create(host.Id, pull("x"))
	if status, _ := testutil.Do(t, h.handler, testutil.Request{Method: "PATCH", URL: base + "/" + record.Id, Token: adminToken, Body: `{"status":"succeeded"}`}); status == http.StatusOK {
		t.Error("an admin edited a command")
	}
}

func TestDiagnosticsBundle(t *testing.T) {
	h := newHarness(t)

	running, host := h.enrolled("bundle")

	record := h.create(host.Id, &agentv1.Command{Payload: &agentv1.Command_AgentDiagnostics{AgentDiagnostics: &agentv1.AgentDiagnostics{LogLines: 100}}})

	done := h.waitForStatus(record.Id, schema.CommandSucceeded, schema.CommandFailed)
	if done.GetString("status") != schema.CommandSucceeded {
		t.Fatalf("%v", done.FieldsData())
	}

	bundles, err := h.app.FindRecordsByFilter(schema.DiagnosticBundles, "host={:h}", "", 0, 0, dbx.Params{"h": host.Id})
	if err != nil || len(bundles) != 1 {
		t.Fatalf("%d bundles, %v", len(bundles), err)
	}

	bundle := bundles[0]
	if bundle.GetInt("size_bytes") == 0 || bundle.GetDateTime("expires_at").IsZero() || bundle.GetString("command") != record.Id {
		t.Errorf("%v", bundle.FieldsData())
	}

	fsys, err := h.app.NewFilesystem()
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()

	reader, err := fsys.GetReader(bundle.BaseFilesPath() + "/" + bundle.GetString("file"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}

	archive, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}

	names := map[string]bool{}
	all := strings.Builder{}
	for _, file := range archive.File {
		names[file.Name] = true
		content, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(content)
		content.Close()
		all.Write(data)
	}

	for _, name := range []string{"report.txt", "report.json", "agent.log", "environment.txt"} {
		if !names[name] {
			t.Errorf("%s is missing from the bundle", name)
		}
	}

	text := all.String()
	if !strings.Contains(text, "Self-checks") || !strings.Contains(text, "bundle") {
		t.Errorf("the report is incomplete: %.400s", text)
	}

	// no secrets: neither the token nor the private key
	if strings.Contains(text, running.config.Token) {
		t.Error("the enrollment token is in the bundle")
	}
	if strings.Contains(text, "PRIVATE KEY") {
		t.Error("the private key is in the bundle")
	}

	// the Agent tab downloads the bundle as an admin, with a file token
	_, adminToken := testutil.NewUser(t, h.app, "admin@example.com", schema.RoleAdmin)
	_, operatorToken := testutil.NewUser(t, h.app, "operator@example.com", schema.RoleOperator)

	fileURL := "/api/files/" + schema.DiagnosticBundles + "/" + bundle.Id + "/" + bundle.GetString("file")
	fileToken := func(token string) string {
		status, body := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: "/api/files/token", Token: token})
		var answer struct {
			Token string `json:"token"`
		}
		if status != http.StatusOK || json.Unmarshal([]byte(body), &answer) != nil || answer.Token == "" {
			t.Fatalf("file token: %d %s", status, body)
		}
		return answer.Token
	}

	status, downloaded := testutil.Do(t, h.handler, testutil.Request{Method: "GET", URL: fileURL + "?token=" + fileToken(adminToken)})
	if status != http.StatusOK || downloaded != string(raw) {
		t.Errorf("an admin's download: %d, %d bytes of %d", status, len(downloaded), len(raw))
	}
	for name, url := range map[string]string{
		"without a token":      fileURL,
		"with an operator's":   fileURL + "?token=" + fileToken(operatorToken),
		"with a made-up token": fileURL + "?token=nope",
	} {
		if status, _ := testutil.Do(t, h.handler, testutil.Request{Method: "GET", URL: url}); status == http.StatusOK {
			t.Errorf("the bundle downloads %s", name)
		}
	}
}

func TestExpiredBundlesAreRemoved(t *testing.T) {
	h := newHarness(t)

	_, host := h.enrolled("expiring")

	record := h.create(host.Id, &agentv1.Command{Payload: &agentv1.Command_AgentDiagnostics{AgentDiagnostics: &agentv1.AgentDiagnostics{LogLines: 10}}})
	h.waitForStatus(record.Id, schema.CommandSucceeded)
	record = h.create(host.Id, &agentv1.Command{Payload: &agentv1.Command_AgentDiagnostics{AgentDiagnostics: &agentv1.AgentDiagnostics{LogLines: 10}}})
	h.waitForStatus(record.Id, schema.CommandSucceeded)

	bundles, err := h.app.FindRecordsByFilter(schema.DiagnosticBundles, "host={:h}", "created", 0, 0, dbx.Params{"h": host.Id})
	if err != nil || len(bundles) != 2 {
		t.Fatalf("%d bundles, %v", len(bundles), err)
	}

	expired := bundles[0]
	expired.Set("expires_at", time.Now().UTC().Add(-time.Minute))
	if err := h.app.Save(expired); err != nil {
		t.Fatal(err)
	}

	h.panel.PruneBundles()

	if _, err := h.app.FindRecordById(schema.DiagnosticBundles, expired.Id); err == nil {
		t.Error("the expired bundle is still there")
	}
	if _, err := h.app.FindRecordById(schema.DiagnosticBundles, bundles[1].Id); err != nil {
		t.Errorf("a bundle that has not expired was removed: %v", err)
	}
}

func TestDebugLoggingFallsBackByItself(t *testing.T) {
	h := newHarness(t)

	_, host := h.enrolled("loglevel")

	record := h.create(host.Id, &agentv1.Command{Payload: &agentv1.Command_AgentSetLogLevel{AgentSetLogLevel: &agentv1.AgentSetLogLevel{Level: "debug", DurationS: 1}}})
	h.waitForStatus(record.Id, schema.CommandSucceeded)

	h.waitFor("the debug level to show up", func() bool {
		return strings.Contains(h.host(host.Id).GetString("runtime"), `"debug"`)
	})

	h.waitFor("the level to fall back", func() bool {
		return strings.Contains(h.host(host.Id).GetString("runtime"), `"info"`)
	})

	// without a limit, debug logging gets the default one instead of staying on
	record = h.create(host.Id, &agentv1.Command{Payload: &agentv1.Command_AgentSetLogLevel{AgentSetLogLevel: &agentv1.AgentSetLogLevel{Level: "debug"}}})
	done := h.waitForStatus(record.Id, schema.CommandSucceeded)
	if !strings.Contains(done.GetString("result"), `"duration_s":900`) {
		t.Errorf("debug logging without a limit: %s", done.GetString("result"))
	}

	// info has no limit of its own
	record = h.create(host.Id, &agentv1.Command{Payload: &agentv1.Command_AgentSetLogLevel{AgentSetLogLevel: &agentv1.AgentSetLogLevel{Level: "info"}}})
	done = h.waitForStatus(record.Id, schema.CommandSucceeded)
	if !strings.Contains(done.GetString("result"), `"duration_s":0`) {
		t.Errorf("back to info: %s", done.GetString("result"))
	}
}

func TestUpdatedRoutesNeedAdmin(t *testing.T) {
	h := newHarness(t)

	host := testutil.NewHost(t, h.app, "update-routes")
	_, operatorToken := testutil.NewUser(t, h.app, "operator@example.com", schema.RoleOperator)
	_, adminToken := testutil.NewUser(t, h.app, "admin@example.com", schema.RoleAdmin)

	url := "/api/pc/hosts/" + host.Id + "/update"

	if status, _ := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: url, Token: operatorToken}); status != http.StatusForbidden {
		t.Errorf("an operator updated an agent: %d", status)
	}

	// a development build does not know its digests
	status, body := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: url, Token: adminToken})
	if status != http.StatusBadRequest || !strings.Contains(body, "digests") {
		t.Errorf("%d %s", status, body)
	}
}

func TestEnrollmentTokenRoute(t *testing.T) {
	h := newHarness(t)

	_, adminToken := testutil.NewUser(t, h.app, "admin@example.com", schema.RoleAdmin)
	_, operatorToken := testutil.NewUser(t, h.app, "operator@example.com", schema.RoleOperator)

	url := "/api/pc/enrollment-tokens"
	body := `{"name":"nas-02","roles":["docker","edge"]}`

	if status, _ := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: url, Token: operatorToken, Body: body}); status != http.StatusForbidden {
		t.Errorf("an operator created a token: %d", status)
	}

	status, response := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: url, Token: adminToken, Body: body})
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, response)
	}
	for _, want := range []string{`"token":"ENR-`, `"ca_pin":"sha256:`, `"panel":"panel.test:`, "pocketcloud-agent"} {
		if !strings.Contains(response, want) {
			t.Errorf("%q is missing from %s", want, response)
		}
	}

	// the token is only ever in that response
	records, err := h.app.FindRecordsByFilter(schema.EnrollmentTokens, "name='nas-02'", "", 0, 0)
	if err != nil || len(records) != 1 {
		t.Fatalf("%d, %v", len(records), err)
	}
	if strings.Contains(records[0].GetString("token_hash"), "ENR-") {
		t.Error("the token is stored in plain text")
	}
	status, listed := testutil.Do(t, h.handler, testutil.Request{Method: "GET", URL: "/api/collections/enrollment_tokens/records", Token: adminToken})
	if status != http.StatusOK || strings.Contains(listed, "token_hash") {
		t.Errorf("the hash must stay hidden: %s", listed)
	}

	for name, bad := range map[string]string{
		"bad name":     `{"name":"Not Valid!","roles":["docker"]}`,
		"unknown role": `{"name":"ok","roles":["root"]}`,
	} {
		if status, _ := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: url, Token: adminToken, Body: bad}); status != http.StatusBadRequest {
			t.Errorf("%s: %d", name, status)
		}
	}

	testutil.NewHost(t, h.app, "existing")
	if status, _ := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: url, Token: adminToken, Body: `{"name":"existing","roles":["docker"]}`}); status != http.StatusBadRequest {
		t.Errorf("a taken name must be refused: %d", status)
	}

	// what an unauthenticated server needs
	status, pem := testutil.Do(t, h.handler, testutil.Request{Method: "GET", URL: "/api/pc/ca.pem"})
	if status != http.StatusOK || !strings.Contains(pem, "BEGIN CERTIFICATE") {
		t.Errorf("%d %s", status, pem)
	}
	status, script := testutil.Do(t, h.handler, testutil.Request{Method: "GET", URL: "/api/pc/install.sh"})
	if status != http.StatusOK || !strings.Contains(script, "docker run") || !strings.Contains(script, h.panel.Authority.Pin()) {
		t.Errorf("%d %s", status, script)
	}
	// docker run refuses the rslave mount of a root that is not shared, with an
	// error that does not say what to do
	if !strings.Contains(script, "mount --make-rshared /") {
		t.Errorf("the script does not check the root mount: %s", script)
	}
}

func TestInfoRoute(t *testing.T) {
	h := newHarness(t)

	_, viewerToken := testutil.NewUser(t, h.app, "viewer@example.com", schema.RoleViewer)

	if status, _ := testutil.Do(t, h.handler, testutil.Request{Method: "GET", URL: "/api/pc/info"}); status != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", status)
	}

	h.enrolled("info-host")

	status, body := testutil.Do(t, h.handler, testutil.Request{Method: "GET", URL: "/api/pc/info", Token: viewerToken})
	if status != http.StatusOK || !strings.Contains(body, `"connected":1`) || !strings.Contains(body, `"total":1`) {
		t.Errorf("%d %s", status, body)
	}
}

func TestInventorySync(t *testing.T) {
	h := newHarness(t)

	h.docker.AddVolume("data")

	keep := h.docker.AddContainer("keep", "nginx:1", map[string]string{"com.docker.compose.project": "demo", "com.docker.compose.service": "web"})
	gone := h.docker.AddContainer("gone", "redis:7", map[string]string{"pocketcloud.role": "agent"})

	running, host := h.enrolled("inventory")

	containers := func() []*core.Record {
		records, err := h.app.FindRecordsByFilter(schema.Containers, "host={:h}", "", 0, 0, dbx.Params{"h": host.Id})
		if err != nil {
			t.Fatal(err)
		}
		return records
	}

	h.waitFor("the first snapshot", func() bool { return len(containers()) == 2 })

	record, err := h.app.FindFirstRecordByData(schema.Containers, "docker_id", keep)
	if err != nil {
		t.Fatal(err)
	}
	if record.GetString("project") != "demo" || record.GetString("service") != "web" || record.GetString("state") != "running" {
		t.Errorf("%v", record.FieldsData())
	}

	system, err := h.app.FindFirstRecordByData(schema.Containers, "docker_id", gone)
	if err != nil {
		t.Fatal(err)
	}
	if system.GetString("system_role") != "agent" {
		t.Errorf("system role: %v", system.FieldsData())
	}

	volumes, _ := h.app.FindRecordsByFilter(schema.Volumes, "host={:h}", "", 0, 0, dbx.Params{"h": host.Id})
	if len(volumes) != 1 || volumes[0].GetString("name") != "data" {
		t.Errorf("volumes: %d", len(volumes))
	}
	networks, _ := h.app.FindRecordsByFilter(schema.Networks, "host={:h}", "", 0, 0, dbx.Params{"h": host.Id})
	if len(networks) != 1 {
		t.Errorf("networks: %d", len(networks))
	}

	// a change on the host shows up by itself
	started := time.Now()
	h.docker.SetStatus(keep, "exited")
	h.waitFor("the state change", func() bool {
		fresh, err := h.app.FindFirstRecordByData(schema.Containers, "docker_id", keep)
		return err == nil && fresh.GetString("state") == "exited"
	})
	if took := time.Since(started); took > 2*time.Second {
		t.Errorf("the change took %s", took)
	}

	h.docker.RemoveContainer(gone)
	h.waitFor("the removal", func() bool { return len(containers()) == 1 })

	// a new container, then a restart of the agent: no duplicates
	h.docker.AddContainer("new", "alpine", nil)
	h.waitFor("the new container", func() bool { return len(containers()) == 2 })

	if err := running.stop(); err != nil {
		t.Fatal(err)
	}

	h.docker.RemoveContainer(keep)

	second := h.start(running.config)
	defer func() { _ = second.stop() }()

	h.waitFor("the snapshot to remove what vanished while disconnected", func() bool { return len(containers()) == 1 })
	time.Sleep(200 * time.Millisecond)
	if count := len(containers()); count != 1 {
		t.Errorf("duplicates after a restart: %d", count)
	}
}

func TestComposeContainersLinkToTheirStack(t *testing.T) {
	h := newHarness(t)

	running, host := h.enrolled("link")

	collection, _ := h.app.FindCollectionByNameOrId(schema.Stacks)
	stack := core.NewRecord(collection)
	stack.Set("host", host.Id)
	stack.Set("name", "demo")
	if err := h.app.Save(stack); err != nil {
		t.Fatal(err)
	}

	id := h.docker.AddContainer("demo-web-1", "nginx:1", map[string]string{"com.docker.compose.project": "demo"})

	h.waitFor("the link", func() bool {
		record, err := h.app.FindFirstRecordByData(schema.Containers, "docker_id", id)
		return err == nil && record.GetString("stack") == stack.Id
	})

	_ = running
}
