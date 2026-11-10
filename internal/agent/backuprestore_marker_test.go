package agent

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestTheMarkerNamesAContainerBeforeItStops (#30, SAFE): the marker names a
// container, with the restart policy it had, before the agent changes its
// policy or stops it, so that an agent that goes during the stop still
// starts it again at its next start.
func TestTheMarkerNamesAContainerBeforeItStops(t *testing.T) {
	h := newRestoreHarness(t)
	h.volume(t, "app_data", map[string]string{"a": "1"})
	id := h.container("app-1", nil, "on-failure", 4, volumeMount("app_data", "/data", false))
	block := make(chan struct{})
	h.engine.BlockOn["POST /containers/"+id+"/stop"] = block
	session := &restoreSession{a: h.Agent, marker: restoreMarker{CommandID: "c1", RestoreID: "r1"}}
	users, err := h.affected(context.Background(), "c1", "", []restoreDest{{volume: "app_data"}}, false)
	if err != nil || len(users) != 1 {
		t.Fatalf("%v %v", users, err)
	}
	session.users = users
	stopped := make(chan error, 1)
	go func() { stopped <- session.stop(context.Background()) }()
	for deadline := time.Now().Add(10 * time.Second); !h.engine.Called("POST /containers/" + id + "/stop"); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the stop never came")
		}
	}
	raw, err := os.ReadFile(h.markerPath("c1"))
	var marker restoreMarker
	if err == nil {
		err = json.Unmarshal(raw, &marker)
	}
	if err != nil || len(marker.Containers) != 1 || marker.Containers[0].ID != id || marker.Containers[0].RestartPolicy != (restartPolicy{Name: "on-failure", MaxRetry: 4}) {
		t.Errorf("the marker while it stops: %+v %v", marker, err)
	}
	close(block)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	session.startAgain(false)
	if h.policy(t, "app-1") != (restartPolicy{Name: "on-failure", MaxRetry: 4}) || h.status("app-1") != "running" {
		t.Errorf("after: %v %s", h.policy(t, "app-1"), h.status("app-1"))
	}
}

// TestAPocketCloudContainerThatWritesTheDestinationRefusesTheRestore (#30,
// SAFE): the agent never stops PocketCloud's own containers for a restore;
// one that writes the destination refuses it, a preview included, and one
// that only reads it is left running.
func TestAPocketCloudContainerThatWritesTheDestinationRefusesTheRestore(t *testing.T) {
	h := newRestoreHarness(t)
	h.volume(t, "app_data", map[string]string{"a": "1"})
	snapshot := h.backup(t, "app_data")
	h.container("pocketcloud-edge", map[string]string{"pocketcloud.role": "edge"}, "always", 0, volumeMount("app_data", "/srv", true))
	payload := h.payload(snapshot, "app_data")
	payload.DryRun, payload.Overwrite = true, false
	result, err := h.restore(context.Background(), "p1", payload)
	if err != nil {
		t.Fatal(err)
	}
	if stops, _ := result["would_stop"].([]any); len(stops) != 0 {
		t.Errorf("would stop %v", stops)
	}
	h.container("pocketcloud-sites", map[string]string{"pocketcloud.role": "sites"}, "always", 0, volumeMount("app_data", "/data", false))
	for _, dryRun := range []bool{true, false} {
		payload := h.payload(snapshot, "app_data")
		payload.DryRun = dryRun
		_, err := h.restore(context.Background(), "c1", payload)
		if err == nil || err.Error() != "pocketcloud-sites is a PocketCloud container (sites) that writes the destination: restore elsewhere" {
			t.Errorf("a restore (dry run %v): %v", dryRun, err)
		}
	}
	if h.status("pocketcloud-sites") != "running" || h.status("pocketcloud-edge") != "running" || len(h.restoreJobs()) != 1 {
		t.Errorf("after: %s %s, %d jobs", h.status("pocketcloud-sites"), h.status("pocketcloud-edge"), len(h.restoreJobs()))
	}
}
