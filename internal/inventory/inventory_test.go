package inventory

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// TestPasswordHashesAreNotStoredWithContainers: everyone who may see a
// container sees its labels, so the hashes of pocketcloud.auth stay out.
func TestPasswordHashesAreNotStoredWithContainers(t *testing.T) {
	app := testutil.NewApp(t)
	host := testutil.NewHost(t, app, "web-1")
	store := &Store{App: app}

	hash := "bob:$2a$05$6PVNJize1kOg8S8pGtSUw.DfmvfGyAZxXEGMlQNylBUi5z9iaiBRW"
	labels := map[string]string{
		"com.docker.compose.project": "tools",
		"com.docker.compose.service": "admin",
		"pocketcloud.domain":         "admin.example.com",
		"pocketcloud.auth":           hash,
	}
	report := &agentv1.ReportInventoryRequest{Containers: []*agentv1.ContainerInfo{{
		DockerId: "c1", Name: "tools-admin-1", Image: "admin:1", State: "running", Labels: labels,
	}}}
	if err := store.Sync(host.Id, report); err != nil {
		t.Fatal(err)
	}

	record, err := app.FindFirstRecordByData(schema.Containers, "docker_id", "c1")
	if err != nil {
		t.Fatal(err)
	}
	stored := record.GetString("labels")
	if strings.Contains(stored, "$2a$") || !strings.Contains(stored, `"pocketcloud.auth":"(hidden)"`) || !strings.Contains(stored, "admin.example.com") {
		t.Errorf("labels: %s", stored)
	}
	// the report the agent sent is left as it was
	if labels["pocketcloud.auth"] != hash {
		t.Error("the report was changed")
	}
}

// TestASnapshotOlderThanAnEventKeepsWhatTheEventStored: the snapshot and the
// events come on separate streams. A container that appears just after a
// snapshot was collected may reach the panel by its event first; the
// snapshot that follows must not remove it, while one collected later does.
func TestASnapshotOlderThanAnEventKeepsWhatTheEventStored(t *testing.T) {
	app := testutil.NewApp(t)
	host := testutil.NewHost(t, app, "web-1")
	store := &Store{App: app}

	collected := timestamppb.New(time.Now().Add(-time.Second))
	event := &agentv1.ReportEventsRequest{Change: &agentv1.ReportEventsRequest_ContainerUpdated{
		ContainerUpdated: &agentv1.ContainerInfo{DockerId: "new", Name: "demo-web-1", Image: "nginx:1", State: "running"},
	}}
	if err := store.Apply(host.Id, event); err != nil {
		t.Fatal(err)
	}
	if err := store.Sync(host.Id, &agentv1.ReportInventoryRequest{CollectedAt: collected}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.FindFirstRecordByData(schema.Containers, "docker_id", "new"); err != nil {
		t.Fatalf("an older snapshot removed what an event stored: %v", err)
	}

	// a snapshot collected after the event says what is gone
	if err := store.Sync(host.Id, &agentv1.ReportInventoryRequest{CollectedAt: timestamppb.New(time.Now().Add(time.Second))}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.FindFirstRecordByData(schema.Containers, "docker_id", "new"); err == nil {
		t.Error("a newer snapshot left a container that is gone")
	}
}
