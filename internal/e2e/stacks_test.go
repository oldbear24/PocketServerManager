package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

var (
	fakeCLIOnce sync.Once
	fakeCLIPath string
	fakeCLIErr  error
	fakeCLIDir  string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if fakeCLIDir != "" {
		os.RemoveAll(fakeCLIDir)
	}
	os.Exit(code)
}

// fakeCLI builds the stand-in for `docker compose`, once per test run.
func fakeCLI(t *testing.T) string {
	t.Helper()

	fakeCLIOnce.Do(func() {
		fakeCLIDir, fakeCLIErr = os.MkdirTemp("", "pocketcloud-fakecli-*")
		if fakeCLIErr != nil {
			return
		}

		name := "docker"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		fakeCLIPath = filepath.Join(fakeCLIDir, name)

		output, err := exec.Command("go", "build", "-o", fakeCLIPath, "github.com/oldbear24/PocketServerManager/cmd/fakecompose").CombinedOutput()
		if err != nil {
			fakeCLIErr = fmt.Errorf("%w: %s", err, output)
		}
	})

	if fakeCLIErr != nil {
		t.Fatal(fakeCLIErr)
	}

	return fakeCLIPath
}

// composeCall is one call of the stand-in; see cmd/fakecompose.
type composeCall struct {
	Args       []string `json:"args"`
	Verb       string   `json:"verb"`
	Project    string   `json:"project"`
	Directory  string   `json:"directory"`
	Files      []string `json:"files"`
	EnvFile    string   `json:"env_file"`
	EnvContent string   `json:"env_content"`
	VerbArgs   []string `json:"verb_args"`
}

// composeHarness is a panel with an agent that uses the stand-in.
type composeHarness struct {
	*harness
	agent    *runningAgent
	host     *core.Record
	log      string
	operator string
	viewer   string
	hostRoot string
}

func newComposeHarness(t *testing.T, name string) *composeHarness {
	t.Helper()

	h := newHarness(t)

	log := filepath.Join(t.TempDir(), "compose.log")
	t.Setenv("FAKE_COMPOSE_LOG", log)

	hostRoot := fakeHostRoot(t)

	config := h.agentConfig(name, h.token(name, schema.HostRoleDocker))
	config.DockerCLI = fakeCLI(t)
	config.HostRoot = hostRoot

	running := h.start(config)

	var host *core.Record
	h.waitFor("the agent to connect", func() bool {
		found, err := h.app.FindFirstRecordByData(schema.Hosts, "name", name)
		if err != nil || !found.GetBool("stream_open") {
			return false
		}
		host = found
		return true
	})

	_, operator := testutil.NewUser(t, h.app, "operator@example.com", schema.RoleOperator)
	_, viewer := testutil.NewUser(t, h.app, "viewer@example.com", schema.RoleViewer)

	return &composeHarness{harness: h, agent: running, host: host, log: log, operator: operator, viewer: viewer, hostRoot: hostRoot}
}

func (c *composeHarness) calls() []composeCall {
	c.t.Helper()

	file, err := os.Open(c.log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		c.t.Fatal(err)
	}
	defer file.Close()

	calls := []composeCall{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<22)
	for scanner.Scan() {
		var call composeCall
		if err := json.Unmarshal(scanner.Bytes(), &call); err != nil {
			c.t.Fatal(err)
		}
		if call.Verb != "version" {
			calls = append(calls, call)
		}
	}

	return calls
}

func (c *composeHarness) post(url, token string, body any) (int, string) {
	c.t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		c.t.Fatal(err)
	}

	return testutil.Do(c.t, c.handler, testutil.Request{Method: http.MethodPost, URL: url, Token: token, Body: string(raw)})
}

func (c *composeHarness) newStack(name, compose, env string) string {
	c.t.Helper()

	status, body := c.post("/api/collections/stacks/records", c.operator, map[string]any{
		"host":         c.host.Id,
		"name":         name,
		"compose_yaml": compose,
		"env":          env,
	})
	if status != http.StatusOK {
		c.t.Fatalf("creating a stack: %d %s", status, body)
	}

	return recordID(c.t, body)
}

func (c *composeHarness) stack(id string) *core.Record {
	c.t.Helper()

	stack, err := c.app.FindRecordById(schema.Stacks, id)
	if err != nil {
		c.t.Fatal(err)
	}

	return stack
}

func (c *composeHarness) revisions(stackID string) []*core.Record {
	c.t.Helper()

	revisions, err := c.app.FindRecordsByFilter(schema.StackRevisions, "stack={:stack}", "number", 0, 0, dbx.Params{"stack": stackID})
	if err != nil {
		c.t.Fatal(err)
	}

	return revisions
}

// deploy asks for a deploy and returns the command.
func (c *composeHarness) deploy(stackID string, body map[string]any) *core.Record {
	c.t.Helper()

	status, answer := c.post("/api/pc/stacks/"+stackID+"/deploy", c.operator, body)
	if status != http.StatusOK {
		c.t.Fatalf("deploy: %d %s", status, answer)
	}

	var response struct {
		Command struct {
			ID string `json:"id"`
		} `json:"command"`
	}
	if err := json.Unmarshal([]byte(answer), &response); err != nil {
		c.t.Fatal(err)
	}

	return c.command(response.Command.ID)
}

const shopFile = `services:
  web:
    image: nginx:1
    environment:
      TOKEN: ${API_TOKEN:?the token is needed}
      TZ: ${TZ}
  db:
    image: postgres:17
    healthcheck:
      test: ["CMD", "pg_isready"]
`

func TestDeployWritesTheFilesAndStartsTheContainers(t *testing.T) {
	c := newComposeHarness(t, "deployer")

	const secret = `s3cr3t'value$with"everything #\`

	stackID := c.newStack("shop", shopFile, "TZ=Europe/Prague\n")

	status, body := c.post("/api/pc/stacks/"+stackID+"/save", c.operator, map[string]any{
		"secrets": map[string]any{"set": map[string]string{"API_TOKEN": secret}},
	})
	if status != http.StatusOK {
		t.Fatalf("save: %d %s", status, body)
	}
	if strings.Contains(body, "s3cr3t") {
		t.Errorf("a secret came back: %s", body)
	}

	command := c.deploy(stackID, map[string]any{})
	done := c.waitForStatus(command.Id, schema.CommandSucceeded, schema.CommandFailed)
	if done.GetString("status") != schema.CommandSucceeded {
		t.Fatalf("the deploy failed: %s", done.GetString("error"))
	}

	// the files are where the issue says, under the agent's volume
	dir := filepath.Join(c.agent.config.StateDir, "stacks", "shop")

	written, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != shopFile {
		t.Errorf("compose.yaml: %q", written)
	}

	// the order, and the options that matter
	var verbs []string
	var up *composeCall
	calls := c.calls()
	for i := range calls {
		verbs = append(verbs, calls[i].Verb)
		if calls[i].Verb == "up" {
			up = &calls[i]
		}
	}
	if strings.Join(verbs, ",") != "config,up" {
		t.Errorf("calls: %v", verbs)
	}
	if up == nil {
		t.Fatal("no up")
	}
	if up.Project != "shop" || up.Directory != dir || !slices.Contains(up.VerbArgs, "--remove-orphans") || !slices.Contains(up.VerbArgs, "--detach") {
		t.Errorf("up: %+v", up)
	}

	// the secret was in the env file during the deploy, and is gone afterwards
	if !strings.Contains(up.EnvContent, "API_TOKEN=") || !strings.Contains(up.EnvContent, "TZ=Europe/Prague") {
		t.Errorf("the env file during the deploy: %q", up.EnvContent)
	}
	after, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "s3cr3t") || !strings.Contains(string(after), "TZ=Europe/Prague") {
		t.Errorf("the env file after the deploy: %q", after)
	}
	if _, err := os.Stat(filepath.Join(dir, ".env.clean")); !os.IsNotExist(err) {
		t.Errorf("the clean copy is left: %v", err)
	}

	// the value reached the container exactly as it was typed
	web := c.docker.Find("shop-web-1")
	if web == nil {
		t.Fatalf("no container: %v", c.docker.Names())
	}
	if env, _ := web.Config["Env"].([]any); !slices.Contains(env, any("TOKEN="+secret)) {
		t.Errorf("the container's environment: %v", env)
	}

	// no stored command holds the secret, in the payload or its output
	records, err := c.app.FindRecordsByFilter(schema.Commands, "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		raw, err := json.Marshal(record.FieldsData())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "s3cr3t") {
			t.Errorf("command %s holds the secret: %s", record.GetString("type"), raw)
		}
	}

	// the panel knows what happened
	c.waitFor("the state of the stack", func() bool { return c.stack(stackID).GetString("status") == "deployed" })
	stack := c.stack(stackID)
	if stack.GetInt("deployed_revision") != 1 || stack.GetDateTime("deployed_at").IsZero() {
		t.Errorf("stack: %v", stack.FieldsData())
	}
	c.waitFor("the revision to be marked as deployed", func() bool {
		return !c.revisions(stackID)[0].GetDateTime("deployed_at").IsZero()
	})

	// and the inventory links the containers to the stack
	c.waitFor("the inventory", func() bool {
		records, err := c.app.FindRecordsByFilter(schema.Containers, "stack={:stack}", "", 0, 0, dbx.Params{"stack": stackID})
		return err == nil && len(records) == 2
	})
}

func TestInvalidYAMLBlocksTheDeployAndPointsAtTheLine(t *testing.T) {
	c := newComposeHarness(t, "strict")

	stackID := c.newStack("shop", "services:\n  web:\n    image: nginx:1\n", "")

	broken := "services:\n  web:\n    image: nginx:1\n   oops: x\n"

	status, body := c.post("/api/pc/stacks/validate", c.operator, map[string]any{"name": "shop", "compose_yaml": broken})
	if status != http.StatusOK {
		t.Fatalf("validate: %d %s", status, body)
	}

	var result struct {
		Valid  bool `json:"valid"`
		Errors []struct {
			Line    int    `json:"line"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	if result.Valid || len(result.Errors) != 1 || result.Errors[0].Line != 4 {
		t.Errorf("validation: %s", body)
	}

	status, body = c.post("/api/pc/stacks/"+stackID+"/deploy", c.operator, map[string]any{"compose_yaml": broken})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("a broken file: %d %s", status, body)
	}

	var rejection struct {
		Message    string `json:"message"`
		Validation struct {
			Errors []struct {
				Line int `json:"line"`
			} `json:"errors"`
		} `json:"validation"`
	}
	if err := json.Unmarshal([]byte(body), &rejection); err != nil {
		t.Fatal(err)
	}
	if rejection.Message == "" || len(rejection.Validation.Errors) != 1 || rejection.Validation.Errors[0].Line != 4 {
		t.Errorf("rejection: %s", body)
	}

	// nothing reached the server, and no revision was made
	if count := c.commandCount(c.host.Id, "stack.deploy"); count != 0 {
		t.Errorf("%d deploy commands", count)
	}
	if len(c.calls()) != 0 {
		t.Errorf("Compose ran: %+v", c.calls())
	}
	if revisions := c.revisions(stackID); len(revisions) != 1 {
		t.Errorf("%d revisions", len(revisions))
	}

	// a broken draft can be saved, and still not be deployed
	status, body = c.post("/api/pc/stacks/"+stackID+"/save", c.operator, map[string]any{"compose_yaml": broken})
	if status != http.StatusOK {
		t.Fatalf("saving a draft: %d %s", status, body)
	}
	status, body = c.post("/api/pc/stacks/"+stackID+"/deploy", c.operator, map[string]any{})
	if status != http.StatusUnprocessableEntity {
		t.Errorf("deploying the saved draft: %d %s", status, body)
	}
}

func TestEverySaveCreatesARevision(t *testing.T) {
	c := newComposeHarness(t, "reviser")

	stackID := c.newStack("shop", "services: {}\n", "A=1\nB=2\n")

	if revisions := c.revisions(stackID); len(revisions) != 1 || revisions[0].GetInt("number") != 1 {
		t.Fatalf("after creating: %d revisions", len(revisions))
	}

	// through the panel's route, with a note
	status, body := c.post("/api/pc/stacks/"+stackID+"/save", c.operator, map[string]any{
		"compose_yaml": "services:\n  web:\n    image: nginx:1\n",
		"note":         "first service",
	})
	if status != http.StatusOK {
		t.Fatalf("save: %d %s", status, body)
	}

	// and through the REST API
	status, body = testutil.Do(t, c.handler, testutil.Request{
		Method: http.MethodPatch,
		URL:    "/api/collections/stacks/records/" + stackID,
		Token:  c.operator,
		Body:   `{"env":"A=1\nB=3\nC=4\n"}`,
	})
	if status != http.StatusOK {
		t.Fatalf("patch: %d %s", status, body)
	}

	// a save that changes nothing makes no revision
	status, body = c.post("/api/pc/stacks/"+stackID+"/save", c.operator, map[string]any{"env": "A=1\nB=3\nC=4\n"})
	if status != http.StatusOK {
		t.Fatalf("save: %d %s", status, body)
	}

	revisions := c.revisions(stackID)
	if len(revisions) != 3 {
		t.Fatalf("%d revisions", len(revisions))
	}

	if revisions[1].GetString("note") != "first service" || revisions[1].GetString("author") == "" {
		t.Errorf("r2: %v", revisions[1].FieldsData())
	}
	if !strings.Contains(revisions[1].GetString("compose_yaml"), "nginx") {
		t.Errorf("r2: %v", revisions[1].FieldsData())
	}

	var keys []string
	if err := revisions[2].UnmarshalJSONField("env_keys", &keys); err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "A,B,C" {
		t.Errorf("env keys: %v", keys)
	}
	if revisions[2].GetString("author") == "" {
		t.Errorf("r3 has no author")
	}

	if stack := c.stack(stackID); stack.GetInt("current_revision") != 3 {
		t.Errorf("current revision: %d", stack.GetInt("current_revision"))
	}

	// the audit says which revisions a change moved between
	entries, err := c.app.FindRecordsByFilter(schema.AuditLog, "collection='stacks' && record_id={:id} && action='update'", "seq", 0, 0, dbx.Params{"id": stackID})
	if err != nil {
		t.Fatal(err)
	}
	found := []string{}
	for _, entry := range entries {
		found = append(found, entry.GetString("revision"))
	}
	if !slices.Contains(found, "r1 → r2") || !slices.Contains(found, "r2 → r3") {
		t.Errorf("revisions in the audit: %q", found)
	}

	// what only the panel may set cannot be set through the API
	status, body = testutil.Do(t, c.handler, testutil.Request{
		Method: http.MethodPatch,
		URL:    "/api/collections/stacks/records/" + stackID,
		Token:  c.operator,
		Body:   `{"current_revision":99,"deployed_revision":99,"status":"deployed","secret_keys":["X"],"secrets":"plain"}`,
	})
	if status != http.StatusOK {
		t.Fatalf("patch: %d %s", status, body)
	}
	stack := c.stack(stackID)
	if stack.GetInt("current_revision") != 3 || stack.GetInt("deployed_revision") != 0 || stack.GetString("secrets") != "" || stack.GetString("status") == "deployed" {
		t.Errorf("protected fields changed: %v", stack.FieldsData())
	}

	// nor does a stack move
	status, _ = testutil.Do(t, c.handler, testutil.Request{
		Method: http.MethodPatch,
		URL:    "/api/collections/stacks/records/" + stackID,
		Token:  c.operator,
		Body:   `{"name":"renamed"}`,
	})
	if status != http.StatusBadRequest {
		t.Errorf("renaming: %d", status)
	}
}

// watchOutput follows the output of a command, with a stream of events.
func watchOutput(t *testing.T, ctx context.Context, url, token string) <-chan map[string]string {
	t.Helper()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", token)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("output: %d", response.StatusCode)
	}

	messages := make(chan map[string]string, 256)

	go func() {
		defer response.Body.Close()
		defer close(messages)

		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			data, found := strings.CutPrefix(scanner.Text(), "data: ")
			if !found {
				continue
			}
			message := map[string]string{}
			if err := json.Unmarshal([]byte(data), &message); err != nil {
				t.Errorf("%s: %v", data, err)
				return
			}
			messages <- message
		}
	}()

	return messages
}

func TestADeployShowsItsOutputWhileItRuns(t *testing.T) {
	c := newComposeHarness(t, "watched")
	base := c.serve()

	wait := filepath.Join(t.TempDir(), "go-on")
	t.Setenv("FAKE_COMPOSE_WAIT", wait)

	stackID := c.newStack("shop", "services:\n  web:\n    image: nginx:1\n", "")

	command := c.deploy(stackID, map[string]any{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	messages := watchOutput(t, ctx, base+"/api/pc/commands/"+command.Id+"/output", c.viewer)

	// `up` has written its first line and is now waiting: the command is not over
	output := ""
	deadline := time.After(waitLimit)
	for !strings.Contains(output, "Network shop_default Creating") {
		select {
		case message, open := <-messages:
			if !open {
				t.Fatal("the stream ended early")
			}
			if message["type"] == "end" {
				t.Fatalf("the command ended early: %v", message)
			}
			output += message["text"]
		case <-deadline:
			t.Fatalf("no live output: %q", output)
		}
	}

	if status := c.command(command.Id).GetString("status"); status != schema.CommandRunning {
		t.Errorf("status while it runs: %s", status)
	}
	if progress := c.command(command.Id).Get("progress"); progress == nil {
		t.Errorf("no progress")
	}

	if err := os.WriteFile(wait, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var end map[string]string
	deadline = time.After(waitLimit)
	for end == nil {
		select {
		case message, open := <-messages:
			if !open {
				t.Fatal("the stream ended without the final state")
			}
			if message["type"] == "end" {
				end = message
			} else {
				output += message["text"]
			}
		case <-deadline:
			t.Fatal("the command did not end")
		}
	}

	if end["status"] != schema.CommandSucceeded {
		t.Errorf("end: %v", end)
	}
	if !strings.Contains(output, "Container shop-web-1 Started") {
		t.Errorf("output: %q", output)
	}

	// someone who looks later gets what was stored
	later := watchOutput(t, ctx, base+"/api/pc/commands/"+command.Id+"/output", c.viewer)
	stored := ""
	for message := range later {
		stored += message["text"]
	}
	if !strings.Contains(stored, "Container shop-web-1 Started") {
		t.Errorf("stored output: %q", stored)
	}
}

func TestAFailedDeployIsReported(t *testing.T) {
	c := newComposeHarness(t, "failing")

	t.Setenv("FAKE_COMPOSE_FAIL", "up")

	stackID := c.newStack("shop", "services:\n  web:\n    image: nginx:1\n", "")

	if status, body := c.post("/api/pc/stacks/"+stackID+"/save", c.operator, map[string]any{
		"secrets": map[string]any{"set": map[string]string{"X": "still-secret"}},
	}); status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}

	command := c.deploy(stackID, map[string]any{})
	failed := c.waitForStatus(command.Id, schema.CommandFailed)

	if !strings.Contains(failed.GetString("error"), "docker compose up failed") {
		t.Errorf("error: %s", failed.GetString("error"))
	}

	c.waitFor("the state of the stack", func() bool { return c.stack(stackID).GetString("status") == "failed" })
	if stack := c.stack(stackID); stack.GetInt("deployed_revision") != 0 {
		t.Errorf("stack: %v", stack.FieldsData())
	}

	// a failure takes the secrets out of the file as well
	after, err := os.ReadFile(filepath.Join(c.agent.config.StateDir, "stacks", "shop", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "still-secret") {
		t.Errorf("the env file: %q", after)
	}
}

func TestRollbackDeploysAnOldRevisionAsANewOne(t *testing.T) {
	c := newComposeHarness(t, "rollback")

	first := "services:\n  web:\n    image: nginx:1\n"
	second := "services:\n  web:\n    image: nginx:2\n"

	stackID := c.newStack("shop", first, "V=1\n")

	c.waitForStatus(c.deploy(stackID, map[string]any{}).Id, schema.CommandSucceeded)

	c.waitForStatus(c.deploy(stackID, map[string]any{"compose_yaml": second, "env": "V=2\n", "note": "newer"}).Id, schema.CommandSucceeded)

	if image := c.docker.Find("shop-web-1").Image; image != "nginx:2" {
		t.Fatalf("image: %s", image)
	}
	c.waitFor("r2 to be the deployed one", func() bool { return c.stack(stackID).GetInt("deployed_revision") == 2 })

	status, body := c.post("/api/pc/stacks/"+stackID+"/rollback", c.operator, map[string]any{"revision": 1})
	if status != http.StatusOK {
		t.Fatalf("rollback: %d %s", status, body)
	}

	var response struct {
		Command struct {
			ID string `json:"id"`
		} `json:"command"`
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatal(err)
	}
	c.waitForStatus(response.Command.ID, schema.CommandSucceeded)

	revisions := c.revisions(stackID)
	if len(revisions) != 3 {
		t.Fatalf("%d revisions: a rollback adds one", len(revisions))
	}
	if revisions[2].GetString("compose_yaml") != first || revisions[2].GetString("env") != "V=1\n" {
		t.Errorf("r3: %v", revisions[2].FieldsData())
	}
	if !strings.Contains(revisions[2].GetString("note"), "r1") {
		t.Errorf("note: %s", revisions[2].GetString("note"))
	}

	c.waitFor("r3 to be the deployed one", func() bool { return c.stack(stackID).GetInt("deployed_revision") == 3 })
	if stack := c.stack(stackID); stack.GetString("compose_yaml") != first {
		t.Errorf("stack: %v", stack.FieldsData())
	}
	if image := c.docker.Find("shop-web-1").Image; image != "nginx:1" {
		t.Errorf("image after the rollback: %s", image)
	}

	// the audit's revert of a change does the same
	c.waitForStatus(c.deploy(stackID, map[string]any{"compose_yaml": second}).Id, schema.CommandSucceeded)
	c.waitFor("r4 to be the deployed one", func() bool { return c.stack(stackID).GetInt("deployed_revision") == 4 })

	_, admin := testutil.NewUser(t, c.app, "admin@example.com", schema.RoleAdmin)

	entries, err := c.app.FindRecordsByFilter(schema.AuditLog, "collection='stacks' && record_id={:id} && revision='r3 → r4'", "", 1, 0, dbx.Params{"id": stackID})
	if err != nil || len(entries) != 1 {
		t.Fatalf("%v %v", entries, err)
	}

	status, body = testutil.Do(t, c.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/audit/" + entries[0].Id + "/revert", Token: admin})
	if status != http.StatusNoContent {
		t.Fatalf("revert: %d %s", status, body)
	}

	revisions = c.revisions(stackID)
	if len(revisions) != 5 || revisions[4].GetString("compose_yaml") != first {
		t.Errorf("after the revert: %d revisions", len(revisions))
	}
	c.waitFor("the old image to be back", func() bool { return c.docker.Find("shop-web-1").Image == "nginx:1" })

	// a change without a revision has nothing to go back to
	created, err := c.app.FindRecordsByFilter(schema.AuditLog, "collection='stacks' && record_id={:id} && action='create'", "", 1, 0, dbx.Params{"id": stackID})
	if err != nil || len(created) != 1 {
		t.Fatalf("%v %v", created, err)
	}
	status, _ = testutil.Do(t, c.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/audit/" + created[0].Id + "/revert", Token: admin})
	if status != http.StatusBadRequest {
		t.Errorf("reverting a creation: %d", status)
	}
}

func TestSecretsCanBeSetAndNeverReadBack(t *testing.T) {
	c := newComposeHarness(t, "keeper")

	stackID := c.newStack("shop", "services: {}\n", "")

	status, body := c.post("/api/pc/stacks/"+stackID+"/save", c.operator, map[string]any{
		"secrets": map[string]any{"set": map[string]string{"DB_PASS": "hunter2-value", "OTHER": "x"}},
	})
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}

	status, body = testutil.Do(t, c.handler, testutil.Request{Method: http.MethodGet, URL: "/api/collections/stacks/records/" + stackID, Token: c.viewer})
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}
	if strings.Contains(body, "hunter2") || strings.Contains(body, `"secrets"`) {
		t.Errorf("a secret is visible: %s", body)
	}
	if !strings.Contains(body, "DB_PASS") {
		t.Errorf("the names are not visible: %s", body)
	}

	var raw struct {
		Secrets string `db:"secrets"`
	}
	if err := c.app.DB().NewQuery("SELECT secrets FROM stacks WHERE id={:id}").Bind(dbx.Params{"id": stackID}).One(&raw); err != nil {
		t.Fatal(err)
	}
	if raw.Secrets == "" || strings.Contains(raw.Secrets, "hunter2") {
		t.Errorf("not sealed: %q", raw.Secrets)
	}

	// the audit says that they changed, and not to what
	entries, err := c.app.FindRecordsByFilter(schema.AuditLog, "collection='stacks' && record_id={:id}", "", 0, 0, dbx.Params{"id": stackID})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		raw, err := json.Marshal(entry.FieldsData())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "hunter2") {
			t.Errorf("the audit holds a secret: %s", raw)
		}
	}

	// removing one takes it away for good
	status, body = c.post("/api/pc/stacks/"+stackID+"/save", c.operator, map[string]any{
		"secrets": map[string]any{"remove": []string{"OTHER"}},
	})
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}
	var keys []string
	if err := c.stack(stackID).UnmarshalJSONField("secret_keys", &keys); err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "DB_PASS" {
		t.Errorf("keys: %v", keys)
	}

	// a name the env file could not hold is refused
	status, _ = c.post("/api/pc/stacks/"+stackID+"/save", c.operator, map[string]any{
		"secrets": map[string]any{"set": map[string]string{"NOT A NAME": "x"}},
	})
	if status != http.StatusBadRequest {
		t.Errorf("a bad name: %d", status)
	}
}

func TestExistingProjectsCanBeImported(t *testing.T) {
	c := newComposeHarness(t, "importer")

	compose := "services:\n  web:\n    image: nginx:1\n    volumes:\n      - ./html:/usr/share/nginx/html # the content\n      - data:/data\nvolumes:\n  data:\n"

	dir := filepath.Join(c.hostRoot, "opt", "legacy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("TZ=UTC\nDB_PASSWORD='hunter2-imported'\nAPI_KEY=abc123\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	id := c.docker.AddContainer("legacy-web-1", "nginx:1", map[string]string{
		"com.docker.compose.project":              "legacy",
		"com.docker.compose.service":              "web",
		"com.docker.compose.project.config_files": "/opt/legacy/compose.yaml",
		"com.docker.compose.project.working_dir":  "/opt/legacy",
	})
	c.docker.AddContainer("pocketcloud-edge", "caddy:2", map[string]string{
		"com.docker.compose.project": "edge",
		"pocketcloud.role":           "edge",
	})

	// the inventory needs a nudge: the agent connected before these appeared
	c.waitFor("the inventory", func() bool {
		_, err := c.app.FindFirstRecordByData(schema.Containers, "docker_id", id)
		return err == nil
	})

	list := func() string {
		t.Helper()
		status, body := testutil.Do(t, c.handler, testutil.Request{Method: http.MethodGet, URL: "/api/pc/hosts/" + c.host.Id + "/compose-projects", Token: c.viewer})
		if status != http.StatusOK {
			t.Fatalf("%d %s", status, body)
		}
		return body
	}

	if body := list(); !strings.Contains(body, `"project":"legacy"`) || strings.Contains(body, `"project":"edge"`) {
		t.Errorf("projects: %s", body)
	}

	status, body := c.post("/api/pc/hosts/"+c.host.Id+"/compose-projects/legacy/import", c.viewer, nil)
	if status != http.StatusForbidden {
		t.Errorf("a viewer imports: %d %s", status, body)
	}

	status, body = c.post("/api/pc/hosts/"+c.host.Id+"/compose-projects/legacy/import", c.operator, nil)
	if status != http.StatusOK {
		t.Fatalf("import: %d %s", status, body)
	}

	done := c.waitForStatus(recordID(t, body), schema.CommandSucceeded, schema.CommandFailed)
	if done.GetString("status") != schema.CommandSucceeded {
		t.Fatalf("the import failed: %s", done.GetString("error"))
	}

	// nothing of the .env file in the stored command
	raw, err := json.Marshal(done.FieldsData())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2") {
		t.Errorf("the command holds a secret: %s", raw)
	}

	stack, err := c.app.FindFirstRecordByFilter(schema.Stacks, "host={:host} && name='legacy'", dbx.Params{"host": c.host.Id})
	if err != nil {
		t.Fatal(err)
	}

	// the data stays where it is, and the file keeps its comments
	if got := stack.GetString("compose_yaml"); !strings.Contains(got, "- /opt/legacy/html:/usr/share/nginx/html # the content") || !strings.Contains(got, "- data:/data") {
		t.Errorf("compose: %q", got)
	}

	if env := stack.GetString("env"); strings.Contains(env, "hunter2") || strings.Contains(env, "abc123") || !strings.Contains(env, "TZ=UTC") {
		t.Errorf("env: %q", env)
	}
	var keys []string
	if err := stack.UnmarshalJSONField("secret_keys", &keys); err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "API_KEY,DB_PASSWORD" {
		t.Errorf("secret keys: %v", keys)
	}

	opened, err := c.panel.Box.OpenString(stack.GetString("secrets"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(opened, `"hunter2-imported"`) {
		t.Errorf("the quotes of the value did not go away: %s", opened)
	}

	if stack.GetInt("current_revision") != 1 || stack.GetInt("deployed_revision") != 1 || stack.GetString("source") != "import" {
		t.Errorf("stack: %v", stack.FieldsData())
	}

	if body := list(); strings.Contains(body, `"project":"legacy"`) {
		t.Errorf("an imported project is still listed: %s", body)
	}
}

func TestStackActionsWorkFromTheirNames(t *testing.T) {
	c := newComposeHarness(t, "actions")

	stackID := c.newStack("shop", "services:\n  web:\n    image: nginx:1\n    environment:\n      A: ${MUST:?needed}\n", "")

	if status, body := c.post("/api/pc/stacks/"+stackID+"/save", c.operator, map[string]any{
		"secrets": map[string]any{"set": map[string]string{"MUST": "value-of-must"}},
	}); status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}

	c.waitForStatus(c.deploy(stackID, map[string]any{}).Id, schema.CommandSucceeded)

	action := func(commandType string, payload map[string]any) *core.Record {
		t.Helper()
		status, body := c.post("/api/collections/commands/records", c.operator, map[string]any{
			"host": c.host.Id, "type": commandType, "payload": payload,
		})
		if status != http.StatusOK {
			t.Fatalf("%s: %d %s", commandType, status, body)
		}
		return c.waitForStatus(recordID(t, body), schema.CommandSucceeded, schema.CommandFailed)
	}

	// a pull reads the Compose file, so it needs the variable
	if done := action("stack.pull", map[string]any{"name": "shop"}); done.GetString("status") != schema.CommandSucceeded {
		t.Errorf("pull: %s", done.GetString("error"))
	}

	for _, commandType := range []string{"stack.restart", "stack.stop"} {
		if done := action(commandType, map[string]any{"name": "shop"}); done.GetString("status") != schema.CommandSucceeded {
			t.Errorf("%s: %s", commandType, done.GetString("error"))
		}
	}

	if state := c.docker.Find("shop-web-1").Status; state != "exited" {
		t.Errorf("after a stop: %s", state)
	}
	c.waitFor("the stack to be stopped", func() bool { return c.stack(stackID).GetString("status") == "stopped" })

	if done := action("stack.down", map[string]any{"name": "shop"}); done.GetString("status") != schema.CommandSucceeded {
		t.Errorf("down: %s", done.GetString("error"))
	}
	if c.docker.Find("shop-web-1") != nil {
		t.Error("the container is still there after a down")
	}
	c.waitFor("the stack to be down", func() bool { return c.stack(stackID).GetString("status") == "down" })

	// the files do not get in the way of stopping: only the name is used
	for _, call := range c.calls() {
		switch call.Verb {
		case "stop", "restart", "down":
			if len(call.Files) != 0 || call.EnvFile != "" {
				t.Errorf("%s read files: %+v", call.Verb, call)
			}
		case "pull":
			if len(call.Files) != 1 {
				t.Errorf("pull: %+v", call)
			}
			if strings.Contains(call.EnvContent, "value-of-must") {
				t.Errorf("a pull wrote a secret to the env file: %q", call.EnvContent)
			}
		}
	}

	// a deploy only comes from the panel, which has validated it
	status, _ := c.post("/api/collections/commands/records", c.operator, map[string]any{
		"host": c.host.Id, "type": "stack.deploy", "payload": map[string]any{"name": "shop", "compose_yaml": "x"},
	})
	if status != http.StatusForbidden {
		t.Errorf("a direct deploy: %d", status)
	}

	// and viewers look, they do not touch
	if status, _ := c.post("/api/pc/stacks/"+stackID+"/deploy", c.viewer, nil); status != http.StatusForbidden {
		t.Errorf("a viewer deploys: %d", status)
	}
	if status, _ := c.post("/api/pc/stacks/"+stackID+"/save", c.viewer, map[string]any{"env": "A=1"}); status != http.StatusForbidden {
		t.Errorf("a viewer saves: %d", status)
	}
	if status, _ := c.post("/api/pc/stacks/"+stackID+"/rollback", c.viewer, map[string]any{"revision": 1}); status != http.StatusForbidden {
		t.Errorf("a viewer rolls back: %d", status)
	}
}

func TestAnAgentThatDiedInADeployRemovesTheSecretsAtItsNextStart(t *testing.T) {
	c := newComposeHarness(t, "crash")

	if err := c.agent.stop(); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(c.agent.config.StateDir, "stacks", "shop")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("TZ=UTC\nPASS=\"left-behind\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env.clean"), []byte("TZ=UTC\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	second := c.start(c.agent.config)
	defer func() { _ = second.stop() }()

	c.waitFor("the clean file to be put back", func() bool {
		raw, err := os.ReadFile(filepath.Join(dir, ".env"))
		return err == nil && string(raw) == "TZ=UTC\n"
	})
}
