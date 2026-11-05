package inventory

import (
	"strings"
	"testing"

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
