package agent

import (
	"context"
	"log/slog"
	"testing"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
)

// TestAnIntegrityCheckHasOneSlotOfItsOwn (#31): a check reads for hours; the
// agent runs one at a time, beside the backups, which hold the slots of
// "backup." and "backup.run", and never in theirs; #28's check of a target's
// connection keeps its own slots.
func TestAnIntegrityCheckHasOneSlotOfItsOwn(t *testing.T) {
	check := &agentv1.Command{Type: "backup.check", Payload: &agentv1.Command_BackupCheck{BackupCheck: &agentv1.BackupCheck{ReadDataSubset: "3/20"}}}
	if key := limitKey(check); key != "backup.check" {
		t.Fatalf("a check counts against %s", key)
	}
	if perTypeLimits["backup.check"] != 1 {
		t.Errorf("limits: %v", perTypeLimits)
	}

	x := newExecutor(nil, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	for _, key := range []string{"backup.run", "backup.run", "backup.init", "backup.init", "backup-check", "backup-check"} {
		release, err := x.acquire(ctx, key)
		if err != nil {
			t.Fatalf("%s waited: %v", key, err)
		}
		defer release()
	}
	release, err := x.acquire(ctx, limitKey(check))
	if err != nil {
		t.Fatalf("a check waited behind the backups: %v", err)
	}
	if _, err := x.acquire(ctx, limitKey(check)); err == nil {
		t.Error("a second check ran beside the first")
	}
	release()
	ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if release, err := x.acquire(ctx, limitKey(check)); err != nil {
		t.Errorf("the next check waited after the first ended: %v", err)
	} else {
		release()
	}
}
