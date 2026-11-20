package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/dbx"

	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/gateway"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// The Overview (#33) stays live over PocketBase's realtime connection. These
// are the page's subscriptions, with the options it gives them, which must
// stay equal to web/src/lib/overview.ts's, web/src/lib/hosts.tsx's and
// web/src/lib/alerts.tsx's (taken from the spec of #33, §4.2 and §4.3): the
// servers and the open alerts of Layout's one list of each, every container
// of useLiveRecords and the command queue, which alone sends its filter as
// it is kept in place.
const (
	overviewHostFields = "id,collectionId,collectionName,updated,name,roles,status,stream_open,ip4,public_ip4,os,cpu_percent," +
		"mem_used_bytes,mem_bytes,disk_used_bytes,disk_total_bytes,rtt_ms,last_seen,agent_version"
	overviewContainerFields = "id,host,state,updated"
	overviewAlertFields     = "id,collectionId,collectionName,rule,target,host,severity,status,first_seen,last_seen,message,updated"
	overviewCommandFields   = "id,collectionId,collectionName,created,updated,host,host_name,type,label,status,resource"
	overviewQueueFilter     = "parent = '' && resource !~ 'backup-usage:%' && type !~ 'logs.%' && type !~ 'exec.%'" +
		" && type !~ 'acme.challenge.%' && type !~ 'db.adminer.%' && type != 'site.php.status'" +
		" && type != 'site.php.mail' && type != 'site.detect' && type != 'stack.validate'" +
		" && type != 'db.stats' && type != 'backup.snapshots' && type != 'backup.browse'"
)

// queueHides tells the commands the queue leaves out, as its filter does.
func queueHides(command map[string]any) bool {
	kind, _ := command["type"].(string)
	resource, _ := command["resource"].(string)
	for _, prefix := range []string{"logs.", "exec.", "acme.challenge.", "db.adminer."} {
		if strings.HasPrefix(kind, prefix) {
			return true
		}
	}
	return strings.HasPrefix(resource, "backup-usage:") || slices.Contains([]string{"site.php.status", "site.php.mail",
		"site.detect", "stack.validate", "db.stats", "backup.snapshots", "backup.browse"}, kind)
}

// encodeURIComponent escapes as JavaScript's function of the name does,
// which the SDK escapes a subscription's options with.
func encodeURIComponent(value string) string {
	var out strings.Builder
	for _, c := range []byte(value) {
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			out.WriteByte(c)
		} else {
			fmt.Fprintf(&out, "%%%02X", c)
		}
	}
	return out.String()
}

// subscription is a collection's topic with options, as the SDK's
// collection(name).subscribe("*", callback, options) sends it.
func subscription(collection string, query map[string]string) string {
	options, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		panic(err)
	}
	return collection + "/*?options=" + encodeURIComponent(string(options))
}

// The Overview's subscriptions.
var (
	hostsTopic      = subscription(schema.Hosts, map[string]string{"fields": overviewHostFields})
	containersTopic = subscription(schema.Containers, map[string]string{"fields": overviewContainerFields})
	alertsTopic     = subscription(schema.Alerts, map[string]string{"fields": overviewAlertFields})
	commandsTopic   = subscription(schema.Commands, map[string]string{"filter": overviewQueueFilter, "fields": overviewCommandFields})
)

// realtimeEvent is a message of a subscription.
type realtimeEvent struct {
	topic  string
	at     time.Time
	Action string         `json:"action"`
	Record map[string]any `json:"record"`
}

// realtimeClient is a browser's realtime connection to the panel.
type realtimeClient struct {
	t      *testing.T
	base   string
	id     string
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	events []realtimeEvent
}

// openRealtime connects as the SDK does, and waits for PB_CONNECT.
func openRealtime(t *testing.T, base string) *realtimeClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/realtime", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	c := &realtimeClient{t: t, base: base, cancel: cancel, done: make(chan struct{})}
	t.Cleanup(c.close)

	connected := make(chan string, 1)
	go func() {
		defer close(c.done)
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 0, 1<<16), 1<<24)
		name, data := "", ""
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				name = strings.TrimPrefix(line, "event:")
			case strings.HasPrefix(line, "data:"):
				data = strings.TrimPrefix(line, "data:")
			case line == "" && name != "":
				if name == "PB_CONNECT" {
					var hello struct {
						ClientID string `json:"clientId"`
					}
					if err := json.Unmarshal([]byte(data), &hello); err != nil {
						t.Errorf("PB_CONNECT: %v", err)
					}
					connected <- hello.ClientID
				} else {
					event := realtimeEvent{topic: name, at: time.Now()}
					if err := json.Unmarshal([]byte(data), &event); err != nil {
						t.Errorf("%s: %v", data, err)
					}
					c.mu.Lock()
					c.events = append(c.events, event)
					c.mu.Unlock()
				}
				name, data = "", ""
			}
		}
	}()

	select {
	case c.id = <-connected:
	case <-time.After(waitLimit):
		t.Fatal("no PB_CONNECT")
	}
	return c
}

// close drops the connection, as a network that goes away does.
func (c *realtimeClient) close() {
	c.cancel()
	<-c.done
}

// subscribe sends the client's subscriptions as a user, as the SDK does
// for the subscriptions made in one tick.
func (c *realtimeClient) subscribe(token string, topics ...string) {
	c.t.Helper()
	body, err := json.Marshal(map[string]any{"clientId": c.id, "subscriptions": topics})
	if err != nil {
		c.t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, c.base+"/api/realtime", strings.NewReader(string(body)))
	if err != nil {
		c.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		c.t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		c.t.Fatalf("subscribing: %d", response.StatusCode)
	}
}

// received are the events of a topic so far.
func (c *realtimeClient) received(topic string) []realtimeEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	events := []realtimeEvent{}
	for _, event := range c.events {
		if event.topic == topic {
			events = append(events, event)
		}
	}
	return events
}

// waitFor waits for an event of a topic that matches, and returns it with
// its place among every event received.
func (c *realtimeClient) waitFor(what, topic string, match func(realtimeEvent) bool) (realtimeEvent, int) {
	c.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		c.mu.Lock()
		for i, event := range c.events {
			if event.topic == topic && match(event) {
				c.mu.Unlock()
				return event, i
			}
		}
		c.mu.Unlock()
		if time.Now().After(deadline) {
			c.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// keysOf is a record's fields, sorted and joined, as fields lists them.
func keysOf(record map[string]any) string {
	names := []string{}
	for name := range record {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func sortedFields(fields string) string {
	names := strings.Split(fields, ",")
	sort.Strings(names)
	return strings.Join(names, ",")
}

// overviewList reads a list as the page does, as a viewer: a page of
// useLiveList, or with all every page, as useLiveRecords's getFullList.
func overviewList(t *testing.T, h *harness, token, collection string, perPage int, all bool, query map[string]string) []map[string]any {
	t.Helper()
	items := []map[string]any{}
	for page := 1; ; page++ {
		values := url.Values{"page": {strconv.Itoa(page)}, "perPage": {strconv.Itoa(perPage)}}
		for key, value := range query {
			values.Set(key, value)
		}
		if all {
			values.Set("skipTotal", "1")
		}
		status, body := testutil.Do(t, h.handler, testutil.Request{Method: http.MethodGet,
			URL: "/api/collections/" + collection + "/records?" + values.Encode(), Token: token})
		if status != http.StatusOK {
			t.Fatalf("%s: %d %s", collection, status, body)
		}
		var list struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal([]byte(body), &list); err != nil {
			t.Fatal(err)
		}
		items = append(items, list.Items...)
		if !all || len(list.Items) < perPage {
			return items
		}
	}
}

// overviewRead is the page's read of the lists it subscribes to, as it
// reads them first and again after each reconnect.
type overviewRead struct {
	hosts, containers, alerts, commands []map[string]any
}

func readOverview(t *testing.T, h *harness, token string) overviewRead {
	t.Helper()
	return overviewRead{
		hosts:      overviewList(t, h, token, schema.Hosts, 200, false, map[string]string{"sort": "name", "fields": overviewHostFields}),
		containers: overviewList(t, h, token, schema.Containers, 1000, true, map[string]string{"fields": overviewContainerFields}),
		alerts: overviewList(t, h, token, schema.Alerts, 200, false, map[string]string{"filter": "status != 'resolved'", "sort": "-last_seen",
			"fields": overviewAlertFields}),
		commands: overviewList(t, h, token, schema.Commands, 5, false, map[string]string{"filter": overviewQueueFilter, "sort": "-created",
			"skipTotal": "true", "fields": overviewCommandFields}),
	}
}

func findByID(records []map[string]any, id string) map[string]any {
	for _, record := range records {
		if record["id"] == id {
			return record
		}
	}
	return nil
}

// TestTheOverviewStaysLive (#33, AC1): a viewer subscribed as the Overview
// is gets a server that goes offline, with only the fields the page asks
// for, and then its alert; a container that stops; the commands of the
// queue as they move, and never a log stream's; after a reconnect the
// page's read finds what changed meanwhile.
func TestTheOverviewStaysLive(t *testing.T) {
	// the panel looks for silent servers when the test says so: one second
	// of silence is enough, and nothing marks a server offline by itself
	j := newJobsHarness(t, func(engine *fakedocker.Engine) {
		addVolume(t, engine, "app_data")
		engine.AddContainer("web", "nginx:1.27", nil)
		engine.AddContainer("db", "postgres:17", nil)
	}, func(server *gateway.Server) {
		server.OfflineAfter, server.MonitorInterval, server.StartGrace = time.Second, time.Hour, 0
	})
	second, node := j.enrolled("node-2")
	base := j.serve()
	_, viewer := testutil.NewUser(t, j.app, "viewer@example.com", schema.RoleViewer)
	_, operator := testutil.NewUser(t, j.app, "operator@example.com", schema.RoleOperator)
	web, db := j.docker.Find("web").ID, j.docker.Find("db").ID
	j.waitForInventory(j.host.Id, web)
	j.waitForInventory(j.host.Id, db)
	webRecord, err := j.app.FindFirstRecordByFilter(schema.Containers, "host={:host} && docker_id={:id}", dbx.Params{"host": j.host.Id, "id": web})
	if err != nil {
		t.Fatal(err)
	}

	page := openRealtime(t, base)
	page.subscribe(viewer, hostsTopic, containersTopic, alertsTopic, commandsTopic)
	// events flow: the agents' heartbeats update their servers ten times a
	// second
	page.waitFor("a heartbeat", hostsTopic, func(realtimeEvent) bool { return true })

	// an operator stops a container: the queue sees its command move, in
	// place, and the container's state follows
	before := time.Now()
	status, out := j.send(http.MethodPost, "/api/collections/commands/records", operator,
		map[string]any{"host": j.host.Id, "type": "container.stop", "payload": map[string]any{"docker_id": web}, "label": "web"})
	if status != http.StatusOK {
		t.Fatalf("the stop: %d %v", status, out)
	}
	stop := out["id"].(string)
	page.waitFor("the stop to succeed", commandsTopic, func(event realtimeEvent) bool {
		return event.Record["id"] == stop && event.Record["status"] == schema.CommandSucceeded
	})
	stopped, _ := page.waitFor("the container to stop", containersTopic, func(event realtimeEvent) bool {
		return event.Record["id"] == webRecord.Id && event.Record["state"] == "exited"
	})
	t.Logf("the container's stop reached the page %s after the request", stopped.at.Sub(before).Round(time.Millisecond))
	moves := []string{}
	for _, event := range page.received(commandsTopic) {
		if event.Record["id"] == stop {
			moves = append(moves, event.Action+":"+event.Record["status"].(string))
		}
	}
	if len(moves) < 2 || moves[0] != "create:queued" || moves[len(moves)-1] != "update:succeeded" {
		t.Errorf("the stop's events: %v", moves)
	}

	// a viewer follows a container's log: its commands are not the queue's
	logs := openLogs(t, fmt.Sprintf("%s/api/pc/hosts/%s/logs?containers=%s&tail=10", base, j.host.Id, db), viewer)
	j.docker.WriteLog("db", "stdout", "database system is ready to accept connections")
	logs.waitForLine("database system is ready to accept connections")
	logs.cancel()
	j.waitFor("the log stream to stop", func() bool {
		ended, err := j.app.FindRecordsByFilter(schema.Commands, "type ~ 'logs.%' && (status = 'queued' || status = 'dispatched' || status = 'running')", "", 0, 0)
		return err == nil && len(ended) == 0 && j.commandCount(j.host.Id, "logs.stop") == 1
	})
	if j.commandCount(j.host.Id, "logs.follow") != 1 {
		t.Fatalf("%d log streams were followed", j.commandCount(j.host.Id, "logs.follow"))
	}

	// a backup runs: the queue sees its command move; it comes after the
	// log stream's, which therefore never came
	job := j.addJob(map[string]any{"name": "app", "source_kind": "volume", "source": "app_data"})
	runs := j.runNow(job["id"].(string))
	if len(runs) != 1 || runs[0].GetString("status") != schema.RunSucceeded {
		t.Fatalf("the run: %v", runs)
	}
	run := runs[0].GetString("command")
	page.waitFor("the backup to succeed", commandsTopic, func(event realtimeEvent) bool {
		return event.Record["id"] == run && event.Record["type"] == "backup.run" && event.Record["status"] == schema.CommandSucceeded
	})
	commandEvents := page.received(commandsTopic)
	for _, event := range commandEvents {
		if queueHides(event.Record) {
			t.Errorf("the queue got %s %v", event.Action, event.Record)
		}
		if keys := keysOf(event.Record); keys != sortedFields(overviewCommandFields) {
			t.Errorf("a command came with %s", keys)
		}
	}
	// the commands it left out were made: the log stream's follow and stop
	hidden, err := j.app.FindRecordsByFilter(schema.Commands, "type ~ 'logs.%'", "", 0, 0)
	if err != nil || len(hidden) != 2 {
		t.Fatalf("the log stream's commands: %v, %v", hidden, err)
	}

	// the second agent stops: its server goes offline, with the page's
	// fields only, then its alert opens
	if err := second.stop(); err != nil {
		t.Fatal(err)
	}
	j.waitFor("the server to be offline", func() bool {
		j.panel.Gateway.CheckOffline()
		return j.harness.host(node.Id).GetString("status") == schema.HostOffline
	})
	offline, hostAt := page.waitFor("the server offline", hostsTopic, func(event realtimeEvent) bool {
		return event.Record["id"] == node.Id && event.Record["status"] == schema.HostOffline
	})
	if offline.Action != "update" || offline.Record["stream_open"] != false || keysOf(offline.Record) != sortedFields(overviewHostFields) {
		t.Errorf("the offline server: %s %v", offline.Action, offline.Record)
	}
	alert, alertAt := page.waitFor("the server's alert", alertsTopic, func(event realtimeEvent) bool {
		return event.Record["rule"] == "agent_offline" && event.Record["target"] == node.Id
	})
	if alert.Action != "create" || alert.Record["status"] != schema.AlertOpen || keysOf(alert.Record) != sortedFields(overviewAlertFields) {
		t.Errorf("the alert: %s %v", alert.Action, alert.Record)
	}
	if alertAt < hostAt {
		t.Error("the alert came before its server went offline")
	}
	for _, event := range page.received(hostsTopic) {
		if keys := keysOf(event.Record); keys != sortedFields(overviewHostFields) {
			t.Fatalf("a server came with %s", keys)
		}
	}
	for _, event := range page.received(containersTopic) {
		if keys := keysOf(event.Record); keys != sortedFields(overviewContainerFields) {
			t.Fatalf("a container came with %s", keys)
		}
	}

	// the connection drops; meanwhile the agent comes back, which resolves
	// its alert, and the container starts again
	page.close()
	third := j.start(second.config)
	t.Cleanup(func() { _ = third.stop() })
	j.waitFor("the server to be back", func() bool {
		host := j.harness.host(node.Id)
		return host.GetBool("stream_open") && host.GetString("status") != schema.HostOffline
	})
	j.waitFor("the alert to resolve", func() bool {
		open, err := j.app.FindRecordsByFilter(schema.Alerts, "rule = 'agent_offline' && target = {:target} && status != 'resolved'", "", 0, 0,
			dbx.Params{"target": node.Id})
		return err == nil && len(open) == 0
	})
	status, out = j.send(http.MethodPost, "/api/collections/commands/records", operator,
		map[string]any{"host": j.host.Id, "type": "container.start", "payload": map[string]any{"docker_id": web}, "label": "web"})
	if status != http.StatusOK {
		t.Fatalf("the start: %d %v", status, out)
	}
	started := out["id"].(string)
	j.waitForStatus(started, schema.CommandSucceeded)
	j.waitFor("the container to run", func() bool {
		record, err := j.app.FindRecordById(schema.Containers, webRecord.Id)
		return err == nil && record.GetString("state") == "running"
	})

	// the page connects again, with a new client, subscribes again and
	// reads again: what it reads is what changed while it was away
	again := openRealtime(t, base)
	if again.id == page.id {
		t.Fatal("the new connection has the old client's id")
	}
	again.subscribe(viewer, hostsTopic, containersTopic, alertsTopic, commandsTopic)
	read := readOverview(t, j.harness, viewer)
	if host := findByID(read.hosts, node.Id); host == nil || host["stream_open"] != true || host["status"] == schema.HostOffline {
		t.Errorf("the server after the reconnect: %v", host)
	}
	if container := findByID(read.containers, webRecord.Id); container == nil || container["state"] != "running" {
		t.Errorf("the container after the reconnect: %v", container)
	}
	for _, alert := range read.alerts {
		if alert["rule"] == "agent_offline" && alert["target"] == node.Id {
			t.Errorf("the resolved alert is still read: %v", alert)
		}
	}
	if len(read.commands) == 0 || read.commands[0]["id"] != started || read.commands[0]["status"] != schema.CommandSucceeded {
		t.Errorf("the queue after the reconnect: %v", read.commands)
	}
	for _, command := range read.commands {
		if queueHides(command) {
			t.Errorf("the queue reads %v", command)
		}
	}
	// and the new subscriptions are live, but what changed while the page
	// was away never arrives: only the read finds it
	again.waitFor("a heartbeat of the server that came back", hostsTopic, func(event realtimeEvent) bool {
		return event.Record["id"] == node.Id && event.Record["stream_open"] == true
	})
	for _, event := range again.received(containersTopic) {
		if event.Record["id"] == webRecord.Id {
			t.Errorf("the container's start of the outage arrived: %v", event.Record)
		}
	}
	for _, event := range again.received(commandsTopic) {
		if event.Record["id"] == started {
			t.Errorf("the start of the outage arrived: %v", event.Record)
		}
	}
}
