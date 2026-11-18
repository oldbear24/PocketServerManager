package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestDiagnosticsProbeThePanel (#32): the diagnostics' round trip to the
// panel is a probe. A plain heartbeat on that second connection would count
// as the server's return and wipe what the panel knows of the server's edge.
func TestDiagnosticsProbeThePanel(t *testing.T) {
	ca := newTestCA(t, "PocketCloud panel root")
	panel, address := serveRepoint(t, ca, "h1")
	state := enrolledState(t, ca, address)
	identity, err := LoadIdentity(state)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{Config: Config{StateDir: state, KeepaliveTime: 30 * time.Second}, Identity: identity}

	if got := a.testConnectivity(context.Background()); !strings.HasPrefix(got, "ok, round trip") {
		t.Fatalf("connectivity: %s", got)
	}
	if beats := panel.heartbeats(); len(beats) != 1 || beats[0] != "h1" {
		t.Errorf("heartbeats: %v", beats)
	}
	panel.mu.Lock()
	defer panel.mu.Unlock()
	if panel.kept != 0 {
		t.Errorf("%d heartbeats were no probes", panel.kept)
	}
}
