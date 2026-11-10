package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
)

// TestReadsUseTheirOwnSlots (#30): a snapshot list, a folder of a snapshot
// and a restore's preview never wait behind backups and restores, which hold
// their slots for hours; restores and downloads have two each.
func TestReadsUseTheirOwnSlots(t *testing.T) {
	command := func(payload any) *agentv1.Command {
		c := &agentv1.Command{}
		switch p := payload.(type) {
		case *agentv1.BackupSnapshots:
			c.Type, c.Payload = "backup.snapshots", &agentv1.Command_BackupSnapshots{BackupSnapshots: p}
		case *agentv1.BackupBrowse:
			c.Type, c.Payload = "backup.browse", &agentv1.Command_BackupBrowse{BackupBrowse: p}
		case *agentv1.BackupRestore:
			c.Type, c.Payload = "backup.restore", &agentv1.Command_BackupRestore{BackupRestore: p}
		case *agentv1.BackupDump:
			c.Type, c.Payload = "backup.dump", &agentv1.Command_BackupDump{BackupDump: p}
		}
		return c
	}
	for want, c := range map[string]*agentv1.Command{
		"backup-read#1":  command(&agentv1.BackupSnapshots{}),
		"backup-read#2":  command(&agentv1.BackupBrowse{}),
		"backup-read#3":  command(&agentv1.BackupRestore{DryRun: true}),
		"backup.restore": command(&agentv1.BackupRestore{}),
		"backup.dump":    command(&agentv1.BackupDump{}),
	} {
		if got := limitKey(c); got != strings.Split(want, "#")[0] {
			t.Errorf("%s: %s", want, got)
		}
	}

	x := newExecutor(nil, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// two backups and two other backup commands hold their slots for hours
	for _, key := range []string{"backup.run", "backup.run", "backup.init", "backup.init"} {
		release, err := x.acquire(ctx, key)
		if err != nil {
			t.Fatalf("%s waited: %v", key, err)
		}
		defer release()
	}
	for range 4 {
		release, err := x.acquire(ctx, "backup-read")
		if err != nil {
			t.Fatalf("a read waited behind the backups: %v", err)
		}
		defer release()
	}
	if _, err := x.acquire(ctx, "backup-read"); err == nil {
		t.Error("a fifth read ran beside four")
	}
	// on an executor of their own, as eight workers are busy
	x = newExecutor(nil, slog.New(slog.DiscardHandler))
	for _, key := range []string{"backup.restore", "backup.dump"} {
		for range 2 {
			release, err := x.acquire(context.Background(), key)
			if err != nil {
				t.Fatalf("%s waited: %v", key, err)
			}
			defer release()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		if _, err := x.acquire(ctx, key); err == nil {
			t.Errorf("a third %s ran beside two", key)
		}
		cancel()
	}
}

// TestEphemeralResultsAreNotStored (#30): a snapshot list, a folder and a
// preview are reported with their results, which the agent keeps in memory
// for a report sent again, but never in commands.json, which it rewrites
// whole at every start and end; a restore's result is stored as before.
func TestEphemeralResultsAreNotStored(t *testing.T) {
	dir := t.TempDir()
	store, err := openCommandStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	x := newExecutor(store, slog.New(slog.DiscardHandler))
	panel := &fakePanel{}
	x.SetClient(panel)
	big := strings.Repeat("x", 4096)
	answer := func(context.Context, *agentv1.Command, *Reporter) (map[string]any, error) {
		return map[string]any{"listing": big}, nil
	}
	x.Handle("backup.snapshots", answer)
	x.Handle("backup.browse", answer)
	x.Handle("backup.restore", answer)
	commands := map[string]*agentv1.Command{
		"list":    {Id: "list", Type: "backup.snapshots", Payload: &agentv1.Command_BackupSnapshots{BackupSnapshots: &agentv1.BackupSnapshots{}}},
		"browse":  {Id: "browse", Type: "backup.browse", Payload: &agentv1.Command_BackupBrowse{BackupBrowse: &agentv1.BackupBrowse{}}},
		"preview": {Id: "preview", Type: "backup.restore", Payload: &agentv1.Command_BackupRestore{BackupRestore: &agentv1.BackupRestore{DryRun: true}}},
		"restore": {Id: "restore", Type: "backup.restore", Payload: &agentv1.Command_BackupRestore{BackupRestore: &agentv1.BackupRestore{}}},
	}
	for _, id := range []string{"list", "browse", "preview", "restore"} {
		x.Submit(commands[id])
		if final := panel.final(t, id); final.GetResult().GetFields()["listing"].GetStringValue() != big {
			t.Errorf("%s was reported without its result: %v", id, final)
		}
	}
	x.Shutdown(time.Second)

	raw, err := os.ReadFile(filepath.Join(dir, storeFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), big) != 1 {
		t.Errorf("commands.json holds %d results", strings.Count(string(raw), big))
	}
	// a report sent again while the agent runs keeps the result
	x.Submit(commands["list"])
	replays := panel.reports("list")
	if last := replays[len(replays)-1]; !last.GetDuplicate() || last.GetResult().GetFields()["listing"].GetStringValue() != big {
		t.Errorf("the replay: %v", last)
	}
	for _, finished := range store.recentlyFinished() {
		if finished.GetResult() == nil {
			t.Errorf("%s is reported back without its result", finished.GetCommandId())
		}
	}

	// after a restart, the listing is known, and nothing runs it again
	reopened, err := openCommandStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"list", "browse", "preview"} {
		if entry, ok := reopened.get(id); !ok || !entry.finished() || entry.result() != nil {
			t.Errorf("%s after a restart: %+v", id, entry)
		}
	}
	if entry, _ := reopened.get("restore"); entry.result()["listing"] != big {
		t.Errorf("the restore's result: %+v", entry)
	}
}

// TestRecentlyFinishedStaysUnderFourMiB (#30): the commands an agent reports
// back when it connects carry their results, newest first, only while those
// stay under 4 MiB, so that the request stays under the gateway's 8 MiB
// limit; the older ones are listed without.
func TestRecentlyFinishedStaysUnderFourMiB(t *testing.T) {
	store, err := openCommandStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]any{"log": strings.Repeat("y", 200<<10)}
	// as many finished commands as are reported back, each with a large
	// result, put straight into the store, which would write them 200 times
	for i := range reportedBack {
		id := "c" + strings.Repeat("0", 3-len(itoa(i))) + itoa(i)
		store.entries[id] = &commandEntry{ID: id, Type: "backup.restore", State: stateSucceeded, Result: result, FinishedAt: time.Now()}
		store.order = append(store.order, id)
	}
	finished := store.recentlyFinished()
	if len(finished) != reportedBack {
		t.Fatalf("%d reported back", len(finished))
	}
	total, with := 0, 0
	for i, command := range finished {
		if command.GetResult() != nil {
			with++
			if i >= with {
				t.Errorf("%s has a result after one without", command.GetCommandId())
			}
		}
		total += proto.Size(command)
	}
	if finished[0].GetCommandId() != "c099" || finished[0].GetResult() == nil {
		t.Errorf("the newest: %v", finished[0].GetCommandId())
	}
	if with < 15 || with > 20 || total > reportedBackBytes+64<<10 {
		t.Errorf("%d results, %d bytes", with, total)
	}
	request := &agentv1.SubscribeCommandsRequest{RecentlyFinished: finished}
	if size := proto.Size(request); size > 8<<20 {
		t.Errorf("the request is %d bytes", size)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for ; i > 0; i /= 10 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
	}
	return string(digits)
}
