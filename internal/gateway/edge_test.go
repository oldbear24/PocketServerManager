package gateway

import (
	"testing"

	"github.com/pocketbase/pocketbase/core"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
)

// TestAStaleHeartbeatDoesNotUndoAnApply: a heartbeat built before an apply
// finished carries the older revision; the newer one the apply recorded stays.
func TestAStaleHeartbeatDoesNotUndoAnApply(t *testing.T) {
	collection := core.NewBaseCollection("hosts")
	collection.Fields.Add(
		&core.TextField{Name: "edge_applied_digest"},
		&core.NumberField{Name: "edge_applied_revision"},
		&core.BoolField{Name: "edge_running"},
		&core.TextField{Name: "edge_error"},
		&core.NumberField{Name: "edge_requests_per_s"},
		&core.TextField{Name: "edge_caddy_version"},
	)
	host := core.NewRecord(collection)
	host.Set("edge_applied_digest", "new")
	host.Set("edge_applied_revision", 5)

	applyEdgeStatus(host, &agentv1.EdgeStatus{Digest: "old", Revision: 4, Running: true})
	if host.GetString("edge_applied_digest") != "new" || host.GetInt("edge_applied_revision") != 5 {
		t.Errorf("a stale heartbeat undid the apply: %s %d", host.GetString("edge_applied_digest"), host.GetInt("edge_applied_revision"))
	}
	if !host.GetBool("edge_running") {
		t.Error("the rest of a stale heartbeat was dropped")
	}

	applyEdgeStatus(host, &agentv1.EdgeStatus{Digest: "newer", Revision: 6})
	if host.GetString("edge_applied_digest") != "newer" {
		t.Error("a newer heartbeat was ignored")
	}

	// a server whose edge was removed reports none
	applyEdgeStatus(host, nil)
	if host.GetString("edge_applied_digest") != "" || host.GetInt("edge_applied_revision") != 0 {
		t.Error("a removed edge still counts as applied")
	}
}
