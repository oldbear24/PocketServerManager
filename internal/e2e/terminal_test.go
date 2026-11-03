package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/dbx"

	"github.com/oldbear24/PocketServerManager/internal/agent"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// terminalClient is a browser with an open terminal.
type terminalClient struct {
	t      *testing.T
	conn   *websocket.Conn
	output strings.Builder
	last   map[string]any
}

func (h *harness) ticket(hostID, token string, body map[string]any) (int, string) {
	h.t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		h.t.Fatal(err)
	}

	return testutil.Do(h.t, h.handler, testutil.Request{Method: http.MethodPost, URL: "/api/pc/hosts/" + hostID + "/terminal", Token: token, Body: string(raw)})
}

func ticketFrom(t *testing.T, body string) string {
	t.Helper()

	var answer struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil || answer.Ticket == "" {
		t.Fatalf("no ticket in %q (%v)", body, err)
	}

	return answer.Ticket
}

func dialTerminal(t *testing.T, base, ticket string) *terminalClient {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()

	conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/api/pc/terminal/socket?ticket="+ticket, nil)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	return &terminalClient{t: t, conn: conn}
}

// readUntil reads until the output holds the text. It returns false if the
// connection ended first.
func (c *terminalClient) readUntil(check func(*terminalClient) bool) bool {
	c.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()

	for !check(c) {
		kind, data, err := c.conn.Read(ctx)
		if err != nil {
			return false
		}
		if kind == websocket.MessageBinary {
			c.output.Write(data)
			continue
		}
		c.last = map[string]any{}
		if err := json.Unmarshal(data, &c.last); err != nil {
			c.t.Fatal(err)
		}
	}

	return true
}

func (c *terminalClient) waitForOutput(text string) {
	c.t.Helper()

	if !c.readUntil(func(client *terminalClient) bool { return strings.Contains(client.output.String(), text) }) {
		c.t.Fatalf("the connection ended before %q; output: %q", text, c.output.String())
	}
}

func (c *terminalClient) send(text string) {
	c.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()

	if err := c.conn.Write(ctx, websocket.MessageBinary, []byte(text)); err != nil {
		c.t.Fatal(err)
	}
}

func (c *terminalClient) resize(cols, rows int) {
	c.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()

	raw := fmt.Appendf(nil, `{"type":"resize","cols":%d,"rows":%d}`, cols, rows)
	if err := c.conn.Write(ctx, websocket.MessageText, raw); err != nil {
		c.t.Fatal(err)
	}
}

func TestAnInteractiveTerminal(t *testing.T) {
	h := newHarness(t)
	base := h.serve()

	id := h.docker.AddContainer("web", "nginx:1", nil)

	_, host := h.enrolled("shell")
	h.waitFor("the inventory", func() bool {
		_, err := h.app.FindFirstRecordByData(schema.Containers, "docker_id", id)
		return err == nil
	})

	_, admin := testutil.NewUser(t, h.app, "admin@example.com", schema.RoleAdmin)

	status, body := h.ticket(host.Id, admin, map[string]any{"container": id, "cols": 100, "rows": 30})
	if status != http.StatusOK {
		t.Fatalf("ticket: %d %s", status, body)
	}

	terminal := dialTerminal(t, base, ticketFrom(t, body))

	// the prompt comes from the program in the container
	terminal.waitForOutput("$ ")

	exec := h.docker.LastExec("web")
	if exec == nil || !exec.TTY || !exec.Stdin {
		t.Fatalf("exec: %+v", exec)
	}
	if strings.Join(exec.Cmd, " ") != "/bin/sh" {
		t.Errorf("command: %v", exec.Cmd)
	}
	if cols, rows := exec.Size(); cols != 100 || rows != 30 {
		t.Errorf("the initial size: %dx%d", cols, rows)
	}

	// typing goes in, and the echo comes back
	terminal.send("hello")
	terminal.waitForOutput("hello")

	// Ctrl+C interrupts what is being typed
	terminal.send("\x03")
	terminal.waitForOutput("^C")

	// a resize reaches the terminal
	terminal.resize(132, 43)
	h.waitFor("the new size", func() bool {
		cols, rows := exec.Size()
		return cols == 132 && rows == 43
	})
	terminal.send("size\n")
	terminal.waitForOutput("132x43")

	// the session's end comes with its exit code
	terminal.send("exit 3\n")
	if !terminal.readUntil(func(client *terminalClient) bool { return client.last["type"] == "exit" }) {
		t.Fatalf("no exit message; output: %q", terminal.output.String())
	}
	if code, _ := terminal.last["code"].(float64); code != 3 {
		t.Errorf("exit code: %v", terminal.last)
	}

	h.waitFor("the session to be gone", func() bool { return h.panel.Streams.Terminals() == 0 })

	// it appears in the audit, with who and where and nothing of what was typed
	var entry struct {
		Actor    string `db:"actor_label"`
		Label    string `db:"record_label"`
		Category string `db:"category"`
		Snapshot string `db:"snapshot"`
	}
	err := h.app.DB().NewQuery("SELECT actor_label, record_label, category, snapshot FROM audit_log WHERE action={:action}").
		Bind(dbx.Params{"action": audit.ActionTerminalSession}).One(&entry)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Category != "security" || entry.Actor != "admin@example.com" || !strings.Contains(entry.Label, "web") {
		t.Errorf("entry: %+v", entry)
	}

	entries, err := h.app.FindRecordsByFilter(schema.AuditLog, "action={:start} || action={:end}", "seq", 0, 0,
		dbx.Params{"start": audit.ActionTerminalSession, "end": audit.ActionTerminalSessionEnd})
	if err != nil {
		t.Fatal(err)
	}
	h.waitFor("the end of the session in the audit", func() bool {
		entries, err = h.app.FindRecordsByFilter(schema.AuditLog, "action={:start} || action={:end}", "seq", 0, 0,
			dbx.Params{"start": audit.ActionTerminalSession, "end": audit.ActionTerminalSessionEnd})
		return err == nil && len(entries) == 2
	})
	for _, record := range entries {
		raw, err := json.Marshal(record.FieldsData())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "hello") {
			t.Errorf("keystrokes in the audit: %s", raw)
		}
	}

	// and on the server's timeline
	timeline, err := h.app.FindRecordsByFilter(schema.Events, "host={:host} && kind='terminal.opened'", "", 1, 0, dbx.Params{"host": host.Id})
	if err != nil || len(timeline) != 1 {
		t.Errorf("event: %v %v", timeline, err)
	}

	// the command history shows that it happened, not what was typed
	commands, err := h.app.FindRecordsByFilter(schema.Commands, "type='exec.start'", "", 0, 0)
	if err != nil || len(commands) != 1 {
		t.Fatalf("%v %v", commands, err)
	}
	if raw, err := json.Marshal(commands[0].FieldsData()); err != nil || strings.Contains(string(raw), "hello") {
		t.Errorf("the command: %s %v", raw, err)
	}
}

func TestClosingTheTabEndsTheSessionOnTheAgent(t *testing.T) {
	h := newHarness(t)
	base := h.serve()

	id := h.docker.AddContainer("web", "nginx:1", nil)

	_, host := h.enrolled("leaving-shell")
	h.waitFor("the inventory", func() bool {
		_, err := h.app.FindFirstRecordByData(schema.Containers, "docker_id", id)
		return err == nil
	})

	_, admin := testutil.NewUser(t, h.app, "admin@example.com", schema.RoleAdmin)

	status, body := h.ticket(host.Id, admin, map[string]any{"container": id})
	if status != http.StatusOK {
		t.Fatalf("ticket: %d %s", status, body)
	}

	terminal := dialTerminal(t, base, ticketFrom(t, body))
	terminal.waitForOutput("$ ")

	exec := h.docker.LastExec("web")

	if err := terminal.conn.CloseNow(); err != nil {
		t.Fatal(err)
	}

	h.waitFor("the process to end", func() bool { return !exec.Running() })
	h.waitFor("the session to be gone", func() bool { return h.panel.Streams.Terminals() == 0 })
}

func TestTerminalTicketsAreForAdminsAndWorkOnce(t *testing.T) {
	h := newHarness(t)
	base := h.serve()

	id := h.docker.AddContainer("web", "nginx:1", nil)

	running, host := h.enrolled("tickets")
	h.waitFor("the inventory", func() bool {
		_, err := h.app.FindFirstRecordByData(schema.Containers, "docker_id", id)
		return err == nil
	})

	_, admin := testutil.NewUser(t, h.app, "admin@example.com", schema.RoleAdmin)
	_, operator := testutil.NewUser(t, h.app, "operator@example.com", schema.RoleOperator)
	_, viewer := testutil.NewUser(t, h.app, "viewer@example.com", schema.RoleViewer)

	for name, token := range map[string]string{"anonymous": "", "viewer": viewer, "operator": operator} {
		status, _ := h.ticket(host.Id, token, map[string]any{"container": id})
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			t.Errorf("%s got a ticket: %d", name, status)
		}
	}

	if status, _ := h.ticket(host.Id, admin, map[string]any{"container": "unknown"}); status != http.StatusNotFound {
		t.Errorf("a container of no one: %d", status)
	}
	if status, _ := h.ticket(host.Id, admin, map[string]any{}); status != http.StatusBadRequest {
		t.Errorf("no target: %d", status)
	}

	status, body := h.ticket(host.Id, admin, map[string]any{"container": id})
	if status != http.StatusOK {
		t.Fatalf("ticket: %d %s", status, body)
	}
	ticket := ticketFrom(t, body)

	// nothing but a valid ticket opens the socket
	dialStatus := func(ticket string) int {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
		defer cancel()
		conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/api/pc/terminal/socket?ticket="+ticket, nil)
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if err == nil {
			_ = conn.CloseNow()
			return http.StatusSwitchingProtocols
		}
		if response == nil {
			t.Fatal(err)
		}
		return response.StatusCode
	}

	if got := dialStatus("not-a-ticket"); got != http.StatusUnauthorized {
		t.Errorf("a made-up ticket: %d", got)
	}

	terminal := dialTerminal(t, base, ticket)
	terminal.waitForOutput("$ ")

	if got := dialStatus(ticket); got != http.StatusUnauthorized {
		t.Errorf("a ticket used twice: %d", got)
	}

	// an auth token is no ticket: tokens do not belong in URLs
	if got := dialStatus(admin); got != http.StatusUnauthorized {
		t.Errorf("an auth token as a ticket: %d", got)
	}

	if err := terminal.conn.CloseNow(); err != nil {
		t.Fatal(err)
	}

	// a server that is not connected has no terminals
	if err := running.stop(); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the stream to close", func() bool { return !h.panel.Commands.Connected(host.Id) })

	if status, _ := h.ticket(host.Id, admin, map[string]any{"container": id}); status != http.StatusConflict {
		t.Errorf("an offline server: %d", status)
	}
}

func TestATerminalClosesAfterALongSilence(t *testing.T) {
	h := newHarness(t)
	h.panel.TerminalIdle = 300 * time.Millisecond
	base := h.serve()

	id := h.docker.AddContainer("web", "nginx:1", nil)

	_, host := h.enrolled("idle")
	h.waitFor("the inventory", func() bool {
		_, err := h.app.FindFirstRecordByData(schema.Containers, "docker_id", id)
		return err == nil
	})

	_, admin := testutil.NewUser(t, h.app, "admin@example.com", schema.RoleAdmin)

	status, body := h.ticket(host.Id, admin, map[string]any{"container": id})
	if status != http.StatusOK {
		t.Fatalf("ticket: %d %s", status, body)
	}

	terminal := dialTerminal(t, base, ticketFrom(t, body))
	terminal.waitForOutput("$ ")

	if !terminal.readUntil(func(client *terminalClient) bool { return client.last["type"] == "idle" }) {
		t.Fatal("the connection ended without saying why")
	}

	exec := h.docker.LastExec("web")
	h.waitFor("the process to end", func() bool { return !exec.Running() })
}

func TestHostShellsNeedTheAgentsOptIn(t *testing.T) {
	h := newHarness(t)
	base := h.serve()

	h.docker.AddImage("alpine:3.22", map[string]any{})

	// without the opt-in the panel knows, and refuses
	_, closedHost := h.enrolled("no-shell")
	if closedHost.GetBool("host_shell_enabled") {
		t.Error("a host shell without the opt-in")
	}

	_, admin := testutil.NewUser(t, h.app, "admin@example.com", schema.RoleAdmin)

	status, body := h.ticket(closedHost.Id, admin, map[string]any{"host_shell": true})
	if status != http.StatusBadRequest || !strings.Contains(body, "HOST_SHELL_ENABLED") {
		t.Errorf("a ticket without the opt-in: %d %s", status, body)
	}

	// and the agent has the last word, whatever the panel thinks
	closedHost.Set("host_shell_enabled", true)
	if err := h.app.Save(closedHost); err != nil {
		t.Fatal(err)
	}

	status, body = h.ticket(closedHost.Id, admin, map[string]any{"host_shell": true})
	if status != http.StatusOK {
		t.Fatalf("ticket: %d %s", status, body)
	}

	refused := dialTerminal(t, base, ticketFrom(t, body))
	if !refused.readUntil(func(client *terminalClient) bool {
		message, _ := client.last["error"].(string)
		return strings.Contains(message, "disabled")
	}) {
		t.Fatalf("the agent did not refuse: %v, %q", refused.last, refused.output.String())
	}
	if h.docker.Called("/containers/create") {
		t.Error("a helper was created without the opt-in")
	}

	// with the opt-in, the agent says so, and a privileged helper does the work
	opened, openHost := h.enrolled("yes-shell", func(a *agent.Agent) { a.Config.HostShell = true })
	defer func() { _ = opened.stop() }()

	h.waitFor("the opt-in to reach the panel", func() bool { return h.host(openHost.Id).GetBool("host_shell_enabled") })

	status, body = h.ticket(openHost.Id, admin, map[string]any{"host_shell": true})
	if status != http.StatusOK {
		t.Fatalf("ticket: %d %s", status, body)
	}

	shell := dialTerminal(t, base, ticketFrom(t, body))
	shell.waitForOutput("$ ")

	execs := h.docker.Execs()
	last := execs[len(execs)-1]
	if strings.Join(last.Cmd, " ") != "nsenter -t 1 -m -u -i -n -p -- /bin/sh -l" {
		t.Errorf("command: %v", last.Cmd)
	}

	var helper string
	for _, name := range h.docker.Names() {
		if strings.HasPrefix(name, "generated-") {
			helper = name
		}
	}
	if helper == "" {
		t.Fatalf("no helper container: %v", h.docker.Names())
	}

	found := h.docker.Find(helper)
	if privileged, _ := found.Host["Privileged"].(bool); !privileged || found.Host["PidMode"] != "host" || found.Labels["pocketcloud.role"] != "job" {
		t.Errorf("helper: %+v", found)
	}

	shell.send("exit\n")
	if !shell.readUntil(func(client *terminalClient) bool { return client.last["type"] == "exit" }) {
		t.Fatalf("no exit: last %v, output %q", shell.last, shell.output.String())
	}

	// the helper does not stay behind
	h.waitFor("the helper to be gone", func() bool { return h.docker.Find(helper) == nil })

	// a change of the opt-in is on the record
	entries, err := h.app.FindRecordsByFilter(schema.AuditLog, "collection='hosts' && record_id={:id}", "", 0, 0, dbx.Params{"id": openHost.Id})
	if err != nil {
		t.Fatal(err)
	}
	foundEntry := false
	for _, entry := range entries {
		foundEntry = foundEntry || strings.Contains(entry.GetString("changes"), "host_shell_enabled")
	}
	if !foundEntry {
		t.Error("the opt-in is not in the audit")
	}
}
