package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/streams"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

type logMessage struct {
	Type  string            `json:"type"`
	Lines []streams.LogLine `json:"lines"`
	Count int64             `json:"count"`
	Error string            `json:"error"`
}

// logClient is a browser following an event stream.
type logClient struct {
	t        *testing.T
	cancel   context.CancelFunc
	messages chan logMessage
	status   int
}

// serve puts the panel's routes on a real listener: a stream needs a real connection.
func (h *harness) serve() string {
	h.t.Helper()

	server := httptest.NewServer(h.handler)
	h.t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})

	return server.URL
}

func openLogs(t *testing.T, url, token string) *logClient {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", token)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}

	client := &logClient{t: t, cancel: cancel, messages: make(chan logMessage, 1024), status: response.StatusCode}

	go func() {
		defer response.Body.Close()
		defer close(client.messages)

		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 0, 1<<16), 1<<24)
		for scanner.Scan() {
			data, found := strings.CutPrefix(scanner.Text(), "data: ")
			if !found {
				continue
			}
			var message logMessage
			if err := json.Unmarshal([]byte(data), &message); err != nil {
				t.Errorf("%s: %v", data, err)
				return
			}
			client.messages <- message
		}
	}()

	return client
}

// waitForLine reads until a line with the text arrives, and returns how long it took.
func (c *logClient) waitForLine(text string) streams.LogLine {
	c.t.Helper()

	deadline := time.After(waitLimit)
	for {
		select {
		case message, open := <-c.messages:
			if !open {
				c.t.Fatalf("the stream ended while waiting for %q", text)
			}
			for _, line := range message.Lines {
				if line.Text == text {
					return line
				}
			}
		case <-deadline:
			c.t.Fatalf("timed out waiting for the line %q", text)
		}
	}
}

// commandCount counts the commands of a type for a host.
func (h *harness) commandCount(hostID, commandType string) int {
	h.t.Helper()

	records, err := h.app.FindRecordsByFilter(schema.Commands, "host={:host} && type={:type}", "", 0, 0,
		map[string]any{"host": hostID, "type": commandType})
	if err != nil {
		h.t.Fatal(err)
	}

	return len(records)
}

func TestLogsReachEveryViewerAndStopWithTheLast(t *testing.T) {
	h := newHarness(t)
	base := h.serve()

	id := h.docker.AddContainer("web", "nginx:1", nil)
	h.docker.WriteLog("web", "stdout", "an old line")

	_, host := h.enrolled("logger")
	h.waitFor("the inventory", func() bool {
		_, err := h.app.FindFirstRecordByData(schema.Containers, "docker_id", id)
		return err == nil
	})

	_, viewer := testutil.NewUser(t, h.app, "viewer@example.com", schema.RoleViewer)

	url := fmt.Sprintf("%s/api/pc/hosts/%s/logs?containers=%s&tail=10", base, host.Id, id)

	first := openLogs(t, url, viewer)
	if first.status != http.StatusOK {
		t.Fatalf("status: %d", first.status)
	}

	// the tail comes first, and the agent keeps following
	old := first.waitForLine("an old line")
	if old.Container != "web" || old.Stream != "stdout" {
		t.Errorf("line: %+v", old)
	}

	// a new line arrives in under a second; docker writes it at this moment
	before := time.Now()
	h.docker.WriteLog("web", "stderr", "a new line")
	fresh := first.waitForLine("a new line")
	if took := time.Since(before); took > time.Second {
		t.Errorf("the line took %s", took)
	}
	if fresh.Stream != "stderr" {
		t.Errorf("stream: %+v", fresh)
	}

	// someone else joins the same stream, and starts with recent lines
	second := openLogs(t, url, viewer)
	second.waitForLine("a new line")

	h.docker.WriteLog("web", "stdout", "for both")
	first.waitForLine("for both")
	second.waitForLine("for both")

	if count := h.commandCount(host.Id, "logs.follow"); count != 1 {
		t.Errorf("one stream must serve both, got %d follow commands", count)
	}
	if h.docker.LogFollowers("web") != 1 {
		t.Errorf("%d log streams are open on the engine", h.docker.LogFollowers("web"))
	}

	// the first leaves: the second keeps watching, and nothing is stopped
	first.cancel()
	h.waitFor("the first viewer to be gone", func() bool { return h.panel.Streams.Viewers() == 1 })

	h.docker.WriteLog("web", "stdout", "still here")
	second.waitForLine("still here")

	if count := h.commandCount(host.Id, "logs.stop"); count != 0 {
		t.Errorf("%d stop commands while someone is watching", count)
	}

	// closing the last tab stops the stream on the agent
	second.cancel()

	h.waitFor("the stream to stop on the agent", func() bool {
		return h.commandCount(host.Id, "logs.stop") == 1 && h.docker.LogFollowers("web") == 0
	})
	if h.panel.Streams.LogSessions() != 0 {
		t.Errorf("%d sessions are left", h.panel.Streams.LogSessions())
	}

	// a next viewer opens a new stream
	third := openLogs(t, url, viewer)
	third.waitForLine("still here")
	if count := h.commandCount(host.Id, "logs.follow"); count != 2 {
		t.Errorf("follow commands: %d", count)
	}
	third.cancel()
	h.waitFor("the third viewer to be gone", func() bool { return h.panel.Streams.LogSessions() == 0 })
}

// TestNoLogDataIsWrittenToADatabase looks for the text of a line in every database
// file of the panel, including the write-ahead logs.
func TestNoLogDataIsWrittenToADatabase(t *testing.T) {
	h := newHarness(t)
	base := h.serve()

	id := h.docker.AddContainer("web", "nginx:1", nil)

	_, host := h.enrolled("private")
	h.waitFor("the inventory", func() bool {
		_, err := h.app.FindFirstRecordByData(schema.Containers, "docker_id", id)
		return err == nil
	})

	_, viewer := testutil.NewUser(t, h.app, "viewer@example.com", schema.RoleViewer)

	const marker = "secret-log-line-e3b0c44298fc1c149afb"

	client := openLogs(t, fmt.Sprintf("%s/api/pc/hosts/%s/logs?containers=%s", base, host.Id, id), viewer)
	h.docker.WriteLog("web", "stdout", marker)
	client.waitForLine(marker)
	client.cancel()

	h.waitFor("the stream to stop", func() bool { return h.commandCount(host.Id, "logs.stop") == 1 })
	stops, err := h.app.FindRecordsByFilter(schema.Commands, "type='logs.stop'", "", 1, 0)
	if err != nil || len(stops) != 1 {
		t.Fatalf("%v %v", stops, err)
	}
	h.waitForStatus(stops[0].Id, schema.CommandSucceeded)

	files, err := filepath.Glob(filepath.Join(h.app.DataDir(), "*"))
	if err != nil {
		t.Fatal(err)
	}

	seen := 0
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		seen++
		if bytes.Contains(raw, []byte(marker)) {
			t.Errorf("%s holds a log line", filepath.Base(path))
		}
	}
	if seen < 2 {
		t.Errorf("only %d files were checked in %s", seen, h.app.DataDir())
	}
}

func TestStackLogsFollowEveryServiceOfTheStack(t *testing.T) {
	h := newHarness(t)
	base := h.serve()

	labels := func(service string) map[string]string {
		return map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.service": service}
	}
	web := h.docker.AddContainer("shop-web-1", "nginx:1", labels("web"))
	db := h.docker.AddContainer("shop-db-1", "postgres:17", labels("db"))
	h.docker.AddContainer("other", "redis:7", nil)

	h.docker.WriteLog("shop-web-1", "stdout", "from web")
	h.docker.WriteLog("shop-db-1", "stdout", "from db")
	h.docker.WriteLog("other", "stdout", "from another project")

	_, host := h.enrolled("stacked")
	h.waitFor("the inventory", func() bool {
		for _, id := range []string{web, db} {
			if _, err := h.app.FindFirstRecordByData(schema.Containers, "docker_id", id); err != nil {
				return false
			}
		}
		return true
	})

	stack := h.saveStack(host.Id, "shop", "services: {}\n")

	_, viewer := testutil.NewUser(t, h.app, "viewer@example.com", schema.RoleViewer)

	client := openLogs(t, base+"/api/pc/stacks/"+stack+"/logs", viewer)

	if line := client.waitForLine("from web"); line.Service != "web" {
		t.Errorf("line: %+v", line)
	}
	h.docker.WriteLog("shop-db-1", "stdout", "more from db")
	if line := client.waitForLine("more from db"); line.Service != "db" {
		t.Errorf("line: %+v", line)
	}

	h.docker.WriteLog("other", "stdout", "again from another project")
	h.docker.WriteLog("shop-web-1", "stdout", "last")

	deadline := time.After(waitLimit)
	for done := false; !done; {
		select {
		case message, open := <-client.messages:
			if !open {
				t.Fatal("the stream ended")
			}
			for _, line := range message.Lines {
				if strings.Contains(line.Text, "another project") {
					t.Errorf("a line of another project: %+v", line)
				}
				done = done || line.Text == "last"
			}
		case <-deadline:
			t.Fatal("timed out")
		}
	}
}

func TestLogRoutesCheckWhoAndWhat(t *testing.T) {
	h := newHarness(t)
	base := h.serve()

	id := h.docker.AddContainer("web", "nginx:1", nil)

	running, host := h.enrolled("guarded")
	h.waitFor("the inventory", func() bool {
		_, err := h.app.FindFirstRecordByData(schema.Containers, "docker_id", id)
		return err == nil
	})

	_, viewer := testutil.NewUser(t, h.app, "viewer@example.com", schema.RoleViewer)

	status := func(url, token string) int {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, base+url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			request.Header.Set("Authorization", token)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}

	path := fmt.Sprintf("/api/pc/hosts/%s/logs?containers=%s", host.Id, id)

	if got := status(path, ""); got != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", got)
	}
	if got := status(fmt.Sprintf("/api/pc/hosts/%s/logs?containers=unknown", host.Id), viewer); got != http.StatusNotFound {
		t.Errorf("a container of no one: %d", got)
	}
	if got := status(fmt.Sprintf("/api/pc/hosts/%s/logs", host.Id), viewer); got != http.StatusBadRequest {
		t.Errorf("no container: %d", got)
	}
	if got := status(fmt.Sprintf("/api/pc/hosts/nobody/logs?containers=%s", id), viewer); got != http.StatusNotFound {
		t.Errorf("an unknown host: %d", got)
	}

	if err := running.stop(); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the stream to close", func() bool { return !h.panel.Commands.Connected(host.Id) })

	if got := status(path, viewer); got != http.StatusConflict {
		t.Errorf("an offline server: %d", got)
	}
	if count := h.commandCount(host.Id, "logs.follow"); count != 0 {
		t.Errorf("a command was queued for an offline server: %d", count)
	}
}

func TestAStoppingAgentEndsTheLogStream(t *testing.T) {
	h := newHarness(t)
	base := h.serve()

	id := h.docker.AddContainer("web", "nginx:1", nil)

	running, host := h.enrolled("leaving")
	h.waitFor("the inventory", func() bool {
		_, err := h.app.FindFirstRecordByData(schema.Containers, "docker_id", id)
		return err == nil
	})

	_, viewer := testutil.NewUser(t, h.app, "viewer@example.com", schema.RoleViewer)

	client := openLogs(t, fmt.Sprintf("%s/api/pc/hosts/%s/logs?containers=%s", base, host.Id, id), viewer)
	h.docker.WriteLog("web", "stdout", "hello")
	client.waitForLine("hello")

	if err := running.stop(); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(waitLimit)
	for {
		select {
		case message, open := <-client.messages:
			if !open {
				t.Fatal("the stream closed without saying so")
			}
			if message.Type == "end" {
				return
			}
		case <-deadline:
			t.Fatal("the viewer was not told")
		}
	}
}
