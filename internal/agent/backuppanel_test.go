package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakefetch"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

// The fetch of the panel's copy (#32), against a panel that offers it on a
// connection of its own, which breaks its streams as the tests say.

const testExport = "panel-export:k3j9x2m4q8w1z7v"

// panelCopy is a copy of the panel's of n bytes, as an export starts.
func panelCopy(n int) []byte {
	data := make([]byte, n)
	copy(data, "PCPB\x01")
	for i := 5; i < n; i++ {
		data[i] = byte(i*31 + i>>11)
	}
	return data
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// fedInput is the job's standard input as the feed wrote it.
type fedInput struct {
	bytes.Buffer
	closed bool
	// fail, when set, fails the writes after that many bytes, as a job
	// whose restic stopped reading
	fail int
}

func (f *fedInput) Write(data []byte) (int, error) {
	if f.fail > 0 && f.Len()+len(data) > f.fail {
		return 0, errors.New("restic stopped reading")
	}
	return f.Buffer.Write(data)
}

func (f *fedInput) Close() error {
	f.closed = true
	return nil
}

// fed is what the relay would read of the feed: the copy's bytes, and the
// exit frame, nil when none came.
type fed struct {
	data []byte
	end  *backup.DumpEnd
}

// feed runs the feed of the copy into a job's input, and reads its frames.
func feed(t *testing.T, a *Agent, ctx context.Context, input *fedInput, secrets ...string) fed {
	t.Helper()
	a.feedPanelExport(ctx, input, testExport, secrets)
	if !input.closed {
		t.Error("the job's input was left open")
	}
	var data bytes.Buffer
	end, err := backup.ReadFrames(&input.Buffer, &data)
	if errors.Is(err, backup.ErrDumpCut) {
		return fed{data: data.Bytes()}
	}
	if err != nil {
		t.Fatal(err)
	}
	return fed{data: data.Bytes(), end: &end}
}

// fetchAgent is an agent whose connection of its own reaches a panel that
// offers a copy of n bytes, and whose resumes wait a moment.
func fetchAgent(t *testing.T, n int) (*Agent, *fakefetch.Server, []byte) {
	t.Helper()
	server, dial := fakefetch.NewServer(t)
	data := panelCopy(n)
	server.Offer(testExport, fakefetch.Artifact{Data: data})
	a := &Agent{Log: slog.New(slog.DiscardHandler), LiveUpload: dial, PanelFetchWaits: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}}
	return a, server, data
}

// offsets are the offsets the panel was asked to fetch from.
func offsets(server *fakefetch.Server) []uint64 {
	var list []uint64
	for _, request := range server.Requests() {
		list = append(list, request.GetOffset())
	}
	return list
}

func sameOffsets(got []uint64, want ...uint64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestTheCopyComesWholeAndEndsTheRelayWell (#32): the panel's copy reaches
// the job's input as the relay's frames, as it comes, in one fetch on a
// connection of its own; its size and digest are what the panel said, and
// the relay ends with 0.
func TestTheCopyComesWholeAndEndsTheRelayWell(t *testing.T) {
	a, server, data := fetchAgent(t, 7<<20/2+17)
	got := feed(t, a, context.Background(), &fedInput{})
	if got.end == nil || got.end.Code != 0 || got.end.Message != "" || !bytes.Equal(got.data, data) {
		t.Fatalf("%d bytes, %+v", len(got.data), got.end)
	}
	requests := server.Requests()
	if len(requests) != 1 || requests[0].GetArtifactId() != testExport || requests[0].GetOffset() != 0 || server.Dials() != 1 {
		t.Errorf("%d fetches, %d dials: %v", len(requests), server.Dials(), requests)
	}
}

// TestACopyThatArrivedDamagedIsTriedAgain (#32): a copy whose digest, or
// size, is not what the panel said ends the relay with backup.RelayRetry,
// which the panel tries again, and says what came; so does one the panel
// made anew while it came, as what came before is of the other.
func TestACopyThatArrivedDamagedIsTriedAgain(t *testing.T) {
	a, server, data := fetchAgent(t, 3<<20)
	other := sha256Hex([]byte("another copy"))
	server.Offer(testExport, fakefetch.Artifact{Data: data, SHA256: other})
	got := feed(t, a, context.Background(), &fedInput{})
	want := "the panel's copy arrived damaged: 3145728 bytes, sha256 " + sha256Hex(data) + ", where the panel said 3145728 bytes, sha256 " + other
	if got.end == nil || got.end.Code != backup.RelayRetry || got.end.Message != want {
		t.Errorf("another digest: %+v", got.end)
	}

	// longer than the panel said
	server.Offer(testExport, fakefetch.Artifact{Data: data, Total: 1 << 20})
	got = feed(t, a, context.Background(), &fedInput{})
	if got.end == nil || got.end.Code != backup.RelayRetry || got.end.Message != "the panel's copy arrived damaged: more than the 1048576 bytes, sha256 "+sha256Hex(data)+", the panel said" ||
		len(got.data) != 1<<20 {
		t.Errorf("a longer copy: %d bytes, %+v", len(got.data), got.end)
	}

	// shorter than the panel said: it never ends, and breaks off
	server.Offer(testExport, fakefetch.Artifact{Data: data, Total: 4 << 20})
	got = feed(t, a, context.Background(), &fedInput{})
	if got.end == nil || got.end.Code != backup.RelayRetry || got.end.Message != "the panel's copy broke off after 3.0 MiB of 4.0 MiB: it ended early" {
		t.Errorf("a shorter copy: %+v", got.end)
	}

	// a copy made anew while it came
	server.Offer(testExport, fakefetch.Artifact{Data: data})
	server.Break(fakefetch.Fault{After: 1 << 20, Code: codes.Unavailable, Message: "the panel restarted"})
	renewed := panelCopy(2 << 20)
	a.PanelFetchWaits = []time.Duration{500 * time.Millisecond}
	before := len(server.Requests())
	go func() {
		// once the first fetch took the first copy, before the resume
		for len(server.Requests()) == before {
			time.Sleep(time.Millisecond)
		}
		server.Offer(testExport, fakefetch.Artifact{Data: renewed})
	}()
	got = feed(t, a, context.Background(), &fedInput{})
	want = "the panel's copy arrived damaged: it changed while it came, from 3145728 bytes, sha256 " + sha256Hex(data) + ", to 2097152 bytes, sha256 " + sha256Hex(renewed)
	if got.end == nil || got.end.Code != backup.RelayRetry || got.end.Message != want {
		t.Errorf("a copy made anew: %+v", got.end)
	}
}

// TestACopyThatBrokeOffIsResumedFromWhereItGotTo (#32): a fetch that broke
// off, as the panel went for a moment, its connection was reset or its
// stream ended early, is resumed from the offset it got to, after a wait,
// and the copy comes whole.
func TestACopyThatBrokeOffIsResumedFromWhereItGotTo(t *testing.T) {
	for _, fault := range []fakefetch.Fault{
		{After: 3<<20/2 + 9, Code: codes.Unavailable, Message: "connection reset by peer"},
		{After: 3<<20/2 + 9, Code: codes.Internal, Message: "stream terminated by RST_STREAM"},
		{After: 3<<20/2 + 9, Code: codes.DeadlineExceeded},
		{After: 3<<20/2 + 9, Code: codes.Canceled, Message: "the panel's stream was cancelled"},
		{After: 3<<20/2 + 9, Code: codes.OK},
	} {
		a, server, data := fetchAgent(t, 3<<20)
		a.PanelFetchWaits = []time.Duration{60 * time.Millisecond, time.Hour, time.Hour}
		server.Break(fault)
		started := time.Now()
		got := feed(t, a, context.Background(), &fedInput{})
		if got.end == nil || got.end.Code != 0 || !bytes.Equal(got.data, data) {
			t.Errorf("%v: %d bytes, %+v", fault.Code, len(got.data), got.end)
		}
		if !sameOffsets(offsets(server), 0, 3<<20/2+9) || server.Dials() != 1 || time.Since(started) < 60*time.Millisecond {
			t.Errorf("%v: fetched from %v, %d dials, in %v", fault.Code, offsets(server), server.Dials(), time.Since(started))
		}
	}

	// the panel that is building the copy, then has it
	a, server, data := fetchAgent(t, 1<<20)
	server.Break(fakefetch.Fault{Code: codes.Internal, Message: "the artifact cannot be read: PocketBase is making or restoring a backup: the run is tried again."})
	if got := feed(t, a, context.Background(), &fedInput{}); got.end == nil || got.end.Code != 0 || !bytes.Equal(got.data, data) || !sameOffsets(offsets(server), 0, 0) {
		t.Errorf("a copy that came at the second try: %+v, %v", got.end, offsets(server))
	}
}

// TestACopyThatKeepsBreakingOffIsTriedAgainLater (#32): three resumes at
// most, after 2, 5 and 10 seconds; then the relay ends with
// backup.RelayRetry and says how far the copy got, and the panel tries the
// run again later.
func TestACopyThatKeepsBreakingOffIsTriedAgainLater(t *testing.T) {
	if waits := (&Agent{}).panelFetchWaits(); len(waits) != 3 || waits[0] != 2*time.Second || waits[1] != 5*time.Second || waits[2] != 10*time.Second {
		t.Errorf("the waits: %v", waits)
	}
	a, server, _ := fetchAgent(t, 4<<20)
	a.PanelFetchWaits = []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond}
	for range 4 {
		server.Break(fakefetch.Fault{After: 1 << 19, Code: codes.Unavailable, Message: "connection reset by peer"})
	}
	started := time.Now()
	got := feed(t, a, context.Background(), &fedInput{})
	if got.end == nil || got.end.Code != backup.RelayRetry || got.end.Message != "the panel's copy broke off after 2.0 MiB of 4.0 MiB: connection reset by peer" {
		t.Errorf("%d bytes, %+v", len(got.data), got.end)
	}
	if !sameOffsets(offsets(server), 0, 1<<19, 2<<19, 3<<19) || time.Since(started) < 140*time.Millisecond {
		t.Errorf("fetched from %v in %v", offsets(server), time.Since(started))
	}

	// a panel that never has it
	a, server, _ = fetchAgent(t, 1<<20)
	for range 4 {
		server.Break(fakefetch.Fault{Code: codes.Internal, Message: "the artifact cannot be read: The panel has no room to prepare its backup: 10 MiB free, about 1.2 GiB needed."})
	}
	if got := feed(t, a, context.Background(), &fedInput{}); got.end == nil || got.end.Code != backup.RelayRetry ||
		got.end.Message != "the panel's copy did not come: the artifact cannot be read: The panel has no room to prepare its backup: 10 MiB free, about 1.2 GiB needed." {
		t.Errorf("a copy that never came: %+v", got.end)
	}

	// a refusal that is no break is not resumed, and is tried again later
	a, server, _ = fetchAgent(t, 1<<20)
	server.Break(fakefetch.Fault{After: 10, Code: codes.ResourceExhausted, Message: "too many fetches"})
	if got := feed(t, a, context.Background(), &fedInput{}); got.end == nil || got.end.Code != backup.RelayRetry ||
		got.end.Message != "the panel's copy broke off after 10 B of 1.0 MiB: too many fetches" || len(server.Requests()) != 1 {
		t.Errorf("another refusal: %+v, %d fetches", got.end, len(server.Requests()))
	}
}

// TestACopyThePanelNoLongerOffersEndsTheRun (#32): a copy the panel refuses,
// as the run is over or its server is not the carrier, ends the relay with
// 1, which is not tried again, and is not resumed.
func TestACopyThePanelNoLongerOffersEndsTheRun(t *testing.T) {
	for _, code := range []codes.Code{codes.PermissionDenied, codes.FailedPrecondition, codes.NotFound, codes.Unimplemented} {
		a, server, _ := fetchAgent(t, 3<<20)
		server.Break(fakefetch.Fault{After: 1 << 20, Code: code, Message: "the artifact does not belong to a command of this host"})
		got := feed(t, a, context.Background(), &fedInput{})
		if got.end == nil || got.end.Code != 1 || got.end.Message != "the panel no longer offers this copy: the artifact does not belong to a command of this host" {
			t.Errorf("%v: %+v", code, got.end)
		}
		if len(server.Requests()) != 1 {
			t.Errorf("%v: %d fetches", code, len(server.Requests()))
		}
	}
}

// TestTheFeedStopsWhenResticStopsReading (#32): once restic stopped reading,
// the feed stops fetching and writes no exit frame: restic says why; and a
// feed that is stopped says so.
func TestTheFeedStopsWhenResticStopsReading(t *testing.T) {
	a, server, data := fetchAgent(t, 3<<20)
	got := feed(t, a, context.Background(), &fedInput{fail: 3 << 19})
	if got.end != nil || len(got.data) >= len(data) || len(server.Requests()) != 1 {
		t.Errorf("%d bytes, %+v, %d fetches", len(got.data), got.end, len(server.Requests()))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := feed(t, a, ctx, &fedInput{}); got.end == nil || got.end.Code != 1 || !strings.HasPrefix(got.end.Message, "the fetch of the panel's copy was stopped: context canceled") {
		t.Errorf("a stopped feed: %+v", got.end)
	}

	// an agent that cannot reach the panel tries again later
	a.LiveUpload = func(context.Context) (agentv1.AgentServiceClient, func(), error) {
		return nil, nil, errors.New("the agent has no identity to reach the panel with")
	}
	if got := feed(t, a, context.Background(), &fedInput{}); got.end == nil || got.end.Code != backup.RelayRetry ||
		got.end.Message != "the agent cannot reach the panel for its copy: the agent has no identity to reach the panel with" {
		t.Errorf("no panel: %+v", got.end)
	}
}

// TestWhatTheFeedSaysKeepsNoSecret (#32): every line the feed ends the relay
// with has the repository's secrets taken out.
func TestWhatTheFeedSaysKeepsNoSecret(t *testing.T) {
	a, server, _ := fetchAgent(t, 1<<20)
	server.Break(fakefetch.Fault{Code: codes.PermissionDenied, Message: "denied for repo-password-1 and minio-secret"})
	got := feed(t, a, context.Background(), &fedInput{}, "repo-password-1", "minio-secret")
	if got.end == nil || got.end.Message != "the panel no longer offers this copy: denied for *** and ***" {
		t.Errorf("%+v", got.end)
	}
	for range 4 {
		server.Break(fakefetch.Fault{After: 5, Code: codes.Unavailable, Message: "reset by minio-secret"})
	}
	got = feed(t, a, context.Background(), &fedInput{}, "repo-password-1", "minio-secret")
	if got.end == nil || strings.Contains(got.end.Message, "minio-secret") || !strings.HasSuffix(got.end.Message, "reset by ***") {
		t.Errorf("%+v", got.end)
	}
}

// TestAPanelRunThatBrokeOffIsInterrupted (#32): through the job, a run whose
// copy broke off or came damaged saves no snapshot and is the failure class
// interrupted, which the panel tries again; one the panel no longer offers
// is the source's, which it does not.
func TestAPanelRunThatBrokeOffIsInterrupted(t *testing.T) {
	r := newRunAgent(t)
	repo := r.remote(t)
	server, dial := fakefetch.NewServer(t)
	r.LiveUpload = dial
	r.PanelFetchWaits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	data := panelCopy(2 << 20)
	server.Offer(testExport, fakefetch.Artifact{Data: data, SHA256: sha256Hex(nil)})
	run := panelRun(repo)
	result, err := r.handleBackupRun(context.Background(), commandOf("k3j9x2m4q8w1z7v", run), &Reporter{})
	if err == nil || !strings.Contains(err.Error(), "the panel's copy arrived damaged") || result["failure"] != backup.FailureInterrupted || result["snapshot_id"] != nil {
		t.Errorf("a damaged copy: %v %v", result, err)
	}
	server.Offer(testExport, fakefetch.Artifact{Data: data})
	server.Break(fakefetch.Fault{Code: codes.PermissionDenied, Message: "the artifact does not belong to a command of this host"})
	result, err = r.handleBackupRun(context.Background(), commandOf("k3j9x2m4q8w1z7v", run), &Reporter{})
	if err == nil || !strings.Contains(err.Error(), "the panel no longer offers this copy") || result["failure"] != backup.FailureSource {
		t.Errorf("a copy no longer offered: %v %v", result, err)
	}
	if snapshots := fakerestic.Snapshots(t, r.root, repo.GetUrl()); len(snapshots) != 0 {
		t.Errorf("%d snapshots", len(snapshots))
	}
}

// panelRun is a run of the panel's backup job to repo, as the panel sends
// it before it delivers it.
func panelRun(repo *agentv1.BackupRepository) *agentv1.BackupRun {
	return &agentv1.BackupRun{
		RunId: "k3j9x2m4q8w1z7v", Repository: repo, PanelExport: testExport, StdinFileName: backup.PanelFileName, HostName: backup.PanelHostName,
		Tags: []string{"pocketcloud", "job:p4n3lj0b0000001", backup.PanelTag}, Retention: &agentv1.BackupRetention{Daily: 14, Weekly: 8, Monthly: 12},
	}
}
