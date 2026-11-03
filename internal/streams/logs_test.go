package streams

import (
	"fmt"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

func newBroker(t *testing.T) (*Broker, string) {
	t.Helper()

	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)

	queue := commands.NewManager(app)
	queue.Register()

	broker := New(queue)
	broker.Register()

	return broker, testutil.NewHost(t, app, "logs-host").Id
}

func batch(texts ...string) []*agentv1.LogLine {
	lines := make([]*agentv1.LogLine, 0, len(texts))
	for _, text := range texts {
		lines = append(lines, &agentv1.LogLine{Ts: timestamppb.Now(), Container: "web", Text: text})
	}
	return lines
}

func sessionOf(t *testing.T, broker *Broker) string {
	t.Helper()

	broker.mu.Lock()
	defer broker.mu.Unlock()

	if len(broker.logSessions) != 1 {
		t.Fatalf("%d sessions", len(broker.logSessions))
	}
	for id := range broker.logSessions {
		return id
	}
	return ""
}

func TestASlowViewerLosesLinesAndHoldsNobodyUp(t *testing.T) {
	broker, host := newBroker(t)

	slow, err := broker.FollowLogs(LogRequest{Host: host, Containers: []string{"c1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Leave()

	fast, err := broker.FollowLogs(LogRequest{Host: host, Containers: []string{"c1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Leave()

	session := sessionOf(t, broker)

	received := 0
	for i := range viewerBacklog * 3 {
		if !broker.Logs(host, session, batch(fmt.Sprintf("line %d", i), "and another")) {
			t.Fatal("the session is gone")
		}
		received += len(<-fast.Lines())
	}

	if want := viewerBacklog * 3 * 2; received != want {
		t.Errorf("the fast viewer got %d lines, want %d", received, want)
	}
	if fast.TakeDropped() != 0 {
		t.Error("the fast viewer lost lines")
	}

	dropped := slow.TakeDropped()
	if want := int64(viewerBacklog * 2 * 2); dropped != want {
		t.Errorf("the slow viewer lost %d lines, want %d", dropped, want)
	}
	if slow.TakeDropped() != 0 {
		t.Error("the count was not reset")
	}

	// what it does read is what it had room for
	first := <-slow.Lines()
	if first[0].Text != "line 0" {
		t.Errorf("first line: %+v", first[0])
	}
}

func TestAnAgentCannotWriteToTheSessionOfAnotherHost(t *testing.T) {
	broker, host := newBroker(t)

	viewer, err := broker.FollowLogs(LogRequest{Host: host, Containers: []string{"c1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Leave()

	session := sessionOf(t, broker)

	if broker.Logs("another-host", session, batch("forged")) {
		t.Error("another host's lines were accepted")
	}

	broker.LogsClosed("another-host", session)
	if broker.LogSessions() != 1 {
		t.Error("another host closed the session")
	}

	if !broker.Logs(host, session, batch("real")) {
		t.Fatal("the host's own lines were refused")
	}
	if line := (<-viewer.Lines())[0]; line.Text != "real" {
		t.Errorf("line: %+v", line)
	}
}

func TestLinesOfAnUnknownSessionTellTheAgentToStop(t *testing.T) {
	broker, host := newBroker(t)

	if broker.Logs(host, "gone", batch("x")) {
		t.Error("a session nobody watches was accepted")
	}
}

func TestTheRingKeepsTheNewestLines(t *testing.T) {
	broker, host := newBroker(t)

	viewer, err := broker.FollowLogs(LogRequest{Host: host, Containers: []string{"c1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Leave()

	session := sessionOf(t, broker)

	for i := range MaxTail + 50 {
		broker.Logs(host, session, batch(fmt.Sprintf("line %d", i)))
		select {
		case <-viewer.Lines():
		default:
		}
	}

	late, err := broker.FollowLogs(LogRequest{Host: host, Containers: []string{"c1"}, Tail: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer late.Leave()

	lines := <-late.Lines()
	if len(lines) != 5 || lines[4].Text != fmt.Sprintf("line %d", MaxTail+49) {
		t.Errorf("tail: %+v", lines)
	}
}

func TestAFailedFollowCommandEndsTheSession(t *testing.T) {
	broker, host := newBroker(t)

	viewer, err := broker.FollowLogs(LogRequest{Host: host, Containers: []string{"c1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Leave()

	records, err := broker.Commands.App().FindRecordsByFilter(schema.Commands, "type='logs.follow'", "", 1, 0)
	if err != nil || len(records) != 1 {
		t.Fatalf("%v %v", records, err)
	}

	if err := broker.Commands.Cancel(records[0].Id, "no way"); err != nil {
		t.Fatal(err)
	}

	if _, open := <-viewer.Lines(); open {
		t.Error("the stream did not end")
	}
	if viewer.Reason() != "no way" {
		t.Errorf("reason: %q", viewer.Reason())
	}
	if broker.LogSessions() != 0 {
		t.Error("the session is left")
	}
}
