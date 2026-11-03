package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/streams"
)

const (
	sseKeepalive    = 20 * time.Second
	maxStoredOutput = 16 << 20
)

func (p *Panel) bindStreamRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	everyone := requireRole(schema.RoleViewer, schema.RoleOperator, schema.RoleAdmin)

	group.GET("/hosts/{id}/logs", p.handleHostLogs).BindFunc(everyone)
	group.GET("/stacks/{id}/logs", p.handleStackLogs).BindFunc(everyone)
	group.GET("/commands/{id}/output", p.handleCommandOutput).BindFunc(everyone)
}

// prepareSSE turns the response into an event stream. The server's write timeout
// would end it after a few minutes, so a stream has its own deadline: none.
func prepareSSE(e *core.RequestEvent) error {
	// a test's recorder has no deadline to clear, which is fine
	_ = http.NewResponseController(e.Response).SetWriteDeadline(time.Time{})

	e.Response.Header().Set("Content-Type", "text/event-stream")
	e.Response.Header().Set("Cache-Control", "no-store")
	e.Response.Header().Set("X-Accel-Buffering", "no")
	e.Response.WriteHeader(http.StatusOK)

	return e.Flush()
}

func sendSSE(e *core.RequestEvent, message any) error {
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(e.Response, "data: %s\n\n", raw); err != nil {
		return err
	}
	return e.Flush()
}

type logEvent struct {
	Type  string            `json:"type"`
	Lines []streams.LogLine `json:"lines,omitempty"`
	Count int64             `json:"count,omitempty"`
	Error string            `json:"error,omitempty"`
}

func (p *Panel) handleHostLogs(e *core.RequestEvent) error {
	host, err := p.App.FindRecordById(schema.Hosts, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The server does not exist.", err)
	}

	ids := strings.Split(e.Request.URL.Query().Get("containers"), ",")

	containers := []string{}
	names := []string{}
	for _, id := range ids {
		if id == "" {
			continue
		}
		record, err := p.App.FindFirstRecordByFilter(schema.Containers, "host={:host} && docker_id={:id}", dbx.Params{"host": host.Id, "id": id})
		if err != nil {
			return e.NotFoundError("This server has no such container.", err)
		}
		containers = append(containers, id)
		names = append(names, record.GetString("name"))
	}
	if len(containers) == 0 {
		return e.BadRequestError("Name at least one container.", nil)
	}

	return p.serveLogs(e, host.Id, containers, strings.Join(names, ", "))
}

// handleStackLogs follows every container of a stack.
func (p *Panel) handleStackLogs(e *core.RequestEvent) error {
	stack, err := p.App.FindRecordById(schema.Stacks, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The stack does not exist.", err)
	}

	records, err := p.App.FindRecordsByFilter(schema.Containers, "host={:host} && project={:project}", "name", 0, 0,
		dbx.Params{"host": stack.GetString("host"), "project": stack.GetString("name")})
	if err != nil {
		return e.InternalServerError("Failed to read the containers.", err)
	}
	if len(records) == 0 {
		return e.BadRequestError("This stack has no containers yet.", nil)
	}

	containers := make([]string, 0, len(records))
	for _, record := range records {
		containers = append(containers, record.GetString("docker_id"))
	}

	return p.serveLogs(e, stack.GetString("host"), containers, stack.GetString("name"))
}

// serveLogs relays a log stream as server-sent events until the browser leaves.
// The lines go straight through; the database only sees the commands that open
// and close the stream.
func (p *Panel) serveLogs(e *core.RequestEvent, hostID string, containers []string, label string) error {
	if !p.Commands.Connected(hostID) {
		return e.Error(http.StatusConflict, "The server is not connected.", nil)
	}

	tail, err := strconv.Atoi(e.Request.URL.Query().Get("tail"))
	if err != nil {
		tail = streams.DefaultTail
	}

	userID, requesterLabel := requester(e)

	viewer, err := p.Streams.FollowLogs(streams.LogRequest{
		Host:       hostID,
		Containers: containers,
		Tail:       tail,
		Label:      label,
		UserID:     userID,
		Requester:  requesterLabel,
	})
	if err != nil {
		return e.InternalServerError("Failed to open the log stream.", err)
	}
	defer viewer.Leave()

	if err := prepareSSE(e); err != nil {
		return nil //nolint:nilerr // the browser is gone before it started
	}

	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	for {
		select {
		case <-e.Request.Context().Done():
			return nil
		case <-keepalive.C:
			if _, err := fmt.Fprint(e.Response, ": keepalive\n\n"); err != nil {
				return nil //nolint:nilerr // the browser left
			}
			if err := e.Flush(); err != nil {
				return nil //nolint:nilerr // the browser left
			}
		case lines, open := <-viewer.Lines():
			if !open {
				return sendSSE(e, logEvent{Type: "end", Error: viewer.Reason()})
			}
			if dropped := viewer.TakeDropped(); dropped > 0 {
				if err := sendSSE(e, logEvent{Type: "dropped", Count: dropped}); err != nil {
					return nil //nolint:nilerr // the browser left
				}
			}
			if err := sendSSE(e, logEvent{Type: "lines", Lines: lines}); err != nil {
				return nil //nolint:nilerr // the browser left
			}
		}
	}
}

// handleCommandOutput streams what a running command writes, as server-sent
// events; for a finished command it sends what was stored. The stream ends with
// the command, and carries its final state.
func (p *Panel) handleCommandOutput(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")

	if _, err := p.App.FindRecordById(schema.Commands, id); err != nil {
		return e.NotFoundError("The command does not exist.", err)
	}

	// watching first and reading the state afterwards leaves no gap between them
	past, chunks, stop := p.Commands.WatchOutput(id)
	defer stop()

	record, err := p.App.FindRecordById(schema.Commands, id)
	if err != nil {
		return e.NotFoundError("The command does not exist.", err)
	}

	if err := prepareSSE(e); err != nil {
		return nil //nolint:nilerr // the browser is gone before it started
	}

	type outputEvent struct {
		Type   string `json:"type"`
		Text   string `json:"text,omitempty"`
		Status string `json:"status,omitempty"`
		Error  string `json:"error,omitempty"`
	}

	finished := func() error {
		final, err := p.App.FindRecordById(schema.Commands, id)
		if err != nil {
			return nil //nolint:nilerr // removed in the meantime
		}
		return sendSSE(e, outputEvent{Type: "end", Status: final.GetString("status"), Error: final.GetString("error")})
	}

	if len(past) > 0 {
		if err := sendSSE(e, outputEvent{Type: "output", Text: string(past)}); err != nil {
			return nil //nolint:nilerr // the browser left
		}
	}

	if isFinalCommand(record.GetString("status")) {
		if stored, err := p.storedOutput(record); err == nil && len(stored) > 0 && len(past) == 0 {
			if err := sendSSE(e, outputEvent{Type: "output", Text: string(stored)}); err != nil {
				return nil //nolint:nilerr // the browser left
			}
		}
		return finished()
	}

	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	for {
		select {
		case <-e.Request.Context().Done():
			return nil
		case <-keepalive.C:
			if _, err := fmt.Fprint(e.Response, ": keepalive\n\n"); err != nil {
				return nil //nolint:nilerr // the browser left
			}
			if err := e.Flush(); err != nil {
				return nil //nolint:nilerr // the browser left
			}
		case chunk, open := <-chunks:
			if !open {
				return finished()
			}
			if err := sendSSE(e, outputEvent{Type: "output", Text: string(chunk)}); err != nil {
				return nil //nolint:nilerr // the browser left
			}
		}
	}
}

// storedOutput reads the output file of a finished command.
func (p *Panel) storedOutput(record *core.Record) ([]byte, error) {
	name := record.GetString("output")
	if name == "" {
		return nil, nil
	}

	files, err := p.App.NewFilesystem()
	if err != nil {
		return nil, err
	}
	defer files.Close()

	reader, err := files.GetReader(record.BaseFilesPath() + "/" + name)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	return io.ReadAll(io.LimitReader(reader, maxStoredOutput))
}

func isFinalCommand(status string) bool {
	switch status {
	case schema.CommandSucceeded, schema.CommandFailed, schema.CommandTimedOut, schema.CommandCancelled:
		return true
	}
	return false
}
