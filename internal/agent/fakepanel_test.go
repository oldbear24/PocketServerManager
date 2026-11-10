package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
)

// fakePanel is the panel's side of the command reports, as the executor sees
// it: it keeps every update, and answers cancel once told to.
type fakePanel struct {
	agentv1.AgentServiceClient
	mu      sync.Mutex
	updates []*agentv1.ReportCommandUpdateRequest
	cancel  atomic.Bool
}

func (f *fakePanel) ReportCommandUpdate(_ context.Context, in *agentv1.ReportCommandUpdateRequest, _ ...grpc.CallOption) (*agentv1.ReportCommandUpdateResponse, error) {
	f.mu.Lock()
	f.updates = append(f.updates, proto.CloneOf(in))
	f.mu.Unlock()
	return &agentv1.ReportCommandUpdateResponse{Cancel: f.cancel.Load()}, nil
}

// reports are the updates of a command so far.
func (f *fakePanel) reports(id string) []*agentv1.ReportCommandUpdateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var reports []*agentv1.ReportCommandUpdateRequest
	for _, update := range f.updates {
		if update.GetCommandId() == id {
			reports = append(reports, update)
		}
	}
	return reports
}

// final waits for a command's final report.
func (f *fakePanel) final(t *testing.T, id string) *agentv1.ReportCommandUpdateRequest {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, update := range f.reports(id) {
			switch update.GetState() {
			case agentv1.CommandState_COMMAND_STATE_SUCCEEDED, agentv1.CommandState_COMMAND_STATE_FAILED, agentv1.CommandState_COMMAND_STATE_CANCELLED:
				return update
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the command %s never ended", id)
	return nil
}
