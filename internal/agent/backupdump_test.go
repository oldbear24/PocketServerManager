package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/testutil/fakerestic"
)

// uploadPanel is the panel's side of a live upload: it keeps what came, and
// says how the upload ended.
type uploadPanel struct {
	agentv1.UnimplementedAgentServiceServer
	mu      sync.Mutex
	header  *agentv1.UploadArtifactHeader
	data    bytes.Buffer
	chunks  int
	largest int
	// leave, when set, ends the upload once this much came, as a browser
	// that went away
	leave int
	ended chan error
}

func (p *uploadPanel) UploadArtifact(stream grpc.ClientStreamingServer[agentv1.UploadArtifactRequest, agentv1.UploadArtifactResponse]) error {
	for {
		request, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			p.ended <- nil
			return stream.SendAndClose(&agentv1.UploadArtifactResponse{SizeBytes: uint64(p.size())}) //nolint:gosec // a test's size
		}
		if err != nil {
			p.ended <- err
			return err
		}
		p.mu.Lock()
		switch part := request.GetPart().(type) {
		case *agentv1.UploadArtifactRequest_Header:
			p.header = part.Header
		case *agentv1.UploadArtifactRequest_Chunk:
			p.data.Write(part.Chunk)
			p.chunks++
			p.largest = max(p.largest, len(part.Chunk))
		}
		left := p.leave > 0 && p.data.Len() >= p.leave
		p.mu.Unlock()
		if left {
			p.ended <- errors.New("the browser left")
			return status.Error(codes.Aborted, "the browser left")
		}
	}
}

func (p *uploadPanel) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.data.Len()
}

// livePanel serves the agent's live uploads on a connection of its own.
func livePanel(t *testing.T, a *Agent) *uploadPanel {
	t.Helper()
	panel := &uploadPanel{ended: make(chan error, 4)}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(server, panel)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	dials := 0
	a.LiveUpload = func(context.Context) (agentv1.AgentServiceClient, func(), error) {
		dials++
		conn, err := grpc.NewClient("passthrough:///panel", grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
		if err != nil {
			return nil, nil, err
		}
		return agentv1.NewAgentServiceClient(conn), func() { _ = conn.Close() }, nil
	}
	return panel
}

func dumpCommand(id string, payload *agentv1.BackupDump) *agentv1.Command {
	return &agentv1.Command{Id: id, Type: "backup.dump", Payload: &agentv1.Command_BackupDump{BackupDump: payload}}
}

// TestADumpStreamsTheSnapshotOnAConnectionOfItsOwn (#30, S): a download goes
// from restic to the panel on a connection of its own, a folder as a tar of
// what it holds and a file as it is, in chunks of at most 1 MiB, and is
// committed once restic ended well.
func TestADumpStreamsTheSnapshotOnAConnectionOfItsOwn(t *testing.T) {
	h := newRestoreHarness(t)
	big := strings.Repeat("0123456789abcdef", 200<<10)
	h.volume(t, "app_data", map[string]string{"big.bin": big, "sub/small.txt": "small"})
	snapshot := h.backup(t, "app_data")
	panel := livePanel(t, h.Agent)
	payload := &agentv1.BackupDump{Repository: h.repo, SnapshotId: snapshot, Path: "/volumes/app_data/big.bin", RestoreId: "r1",
		FileName: "big.bin", JobTag: testJobTag}
	result, err := h.handleBackupDump(context.Background(), dumpCommand("d1", payload), &Reporter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-panel.ended; err != nil {
		t.Fatalf("the upload: %v", err)
	}
	if panel.data.String() != big || panel.header.GetKind() != "backup_download" || panel.header.GetCommandId() != "d1" ||
		panel.header.GetFileName() != "big.bin" || panel.largest > uploadChunkSize || panel.chunks < 2 {
		t.Errorf("the upload: %d bytes, %v, %d chunks of at most %d", panel.data.Len(), panel.header, panel.chunks, panel.largest)
	}
	if result["bytes"] != int64(len(big)) || result["file_name"] != "big.bin" {
		t.Errorf("the result: %v", result)
	}

	// a folder, as a tar
	panel = livePanel(t, h.Agent)
	payload.Path, payload.FileName = "/volumes/app_data", "app.tar"
	if _, err := h.handleBackupDump(context.Background(), dumpCommand("d2", payload), &Reporter{}); err != nil {
		t.Fatal(err)
	}
	if err := <-panel.ended; err != nil {
		t.Fatalf("the upload: %v", err)
	}
	archive := tar.NewReader(&panel.data)
	names := map[string]bool{}
	for {
		header, err := archive.Next()
		if err != nil {
			break
		}
		names[header.Name] = true
	}
	if !names["volumes/app_data/big.bin"] || !names["volumes/app_data/sub/small.txt"] {
		t.Errorf("the tar: %v", names)
	}
}

// TestDumpJobLogsNothing (#30, S/SAFE): a download's job keeps no log, so
// that no copy of the snapshot's files rests in the Engine's log files, and
// mounts nothing of the server but restic's cache.
func TestDumpJobLogsNothing(t *testing.T) {
	h := newRestoreHarness(t)
	h.volume(t, "app_data", map[string]string{"secret.txt": "the data"})
	snapshot := h.backup(t, "app_data")
	livePanel(t, h.Agent)
	payload := &agentv1.BackupDump{Repository: h.repo, SnapshotId: snapshot, Path: "/volumes/app_data/secret.txt", RestoreId: "r1",
		FileName: "secret.txt", JobTag: testJobTag}
	if _, err := h.handleBackupDump(context.Background(), dumpCommand("d1", payload), &Reporter{}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	job := h.jobs[len(h.jobs)-1]
	h.mu.Unlock()
	logConfig, _ := job.Host["LogConfig"].(map[string]any)
	mounts := mountsOf(job)
	if logConfig["Type"] != "none" || len(mounts) != 1 || mounts["/cache"] == nil || job.Labels["pocketcloud.command"] != "d1" ||
		!strings.Contains(strings.Join(job.Cmd, " "), "dump --snapshot "+snapshot+" --path /volumes/app_data/secret.txt --expect-tag "+testJobTag) {
		t.Errorf("the job: %v %v %q", job.Host["LogConfig"], mounts, job.Cmd)
	}
	// the fake names a job after its id
	prefix := "/containers/" + strings.TrimPrefix(job.Name, "generated-")
	attached := false
	for _, call := range h.engine.Calls() {
		if strings.Contains(call, prefix) && strings.Contains(call, "/logs") {
			t.Errorf("the job's log was read: %s", call)
		}
		attached = attached || strings.Contains(call, prefix) && strings.Contains(call, "/attach?stderr=1&stdout=1&stream=1")
	}
	if !attached {
		t.Error("the job's output was not attached")
	}
}

// TestDumpAbortsTheUploadWhenTheJobFails (#30, SAFE): a download whose
// restic fails part way aborts the upload rather than commit it, so that
// the browser marks the download failed and keeps no cut file; one whose
// browser leaves stops its job with its grace.
func TestDumpAbortsTheUploadWhenTheJobFails(t *testing.T) {
	h := newRestoreHarness(t)
	big := strings.Repeat("x", 3<<20)
	h.volume(t, "app_data", map[string]string{"big.bin": big})
	snapshot := h.backup(t, "app_data")
	panel := livePanel(t, h.Agent)
	t.Setenv("FAKE_RESTIC_DUMP_FAIL", "100000")
	payload := &agentv1.BackupDump{Repository: h.repo, SnapshotId: snapshot, Path: "/volumes/app_data/big.bin", RestoreId: "r1",
		FileName: "big.bin", JobTag: testJobTag}
	if _, err := h.handleBackupDump(context.Background(), dumpCommand("d1", payload), &Reporter{}); err == nil || !strings.Contains(err.Error(), "ciphertext verification failed") {
		t.Errorf("the dump: %v", err)
	}
	select {
	case err := <-panel.ended:
		if status.Code(err) != codes.Canceled {
			t.Errorf("the upload ended with %v, not cancelled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the upload never ended")
	}

	// the browser leaves part way
	t.Setenv("FAKE_RESTIC_DUMP_FAIL", "")
	t.Setenv("FAKE_RESTIC_SLOW_MS", "20")
	panel = livePanel(t, h.Agent)
	panel.leave = 256 << 10
	_, err := h.handleBackupDump(context.Background(), dumpCommand("d2", payload), &Reporter{})
	if !errors.Is(err, errDownloadGone) {
		t.Errorf("a download whose browser left: %v", err)
	}
	stopped := false
	for _, call := range h.engine.Calls() {
		stopped = stopped || strings.HasSuffix(call, "/stop?t=30")
	}
	if !stopped {
		t.Error("the job was not stopped with its grace")
	}
	if strings.Count(fakerestic.Calls(t, h.root), " dump ") != 2 {
		t.Errorf("restic ran:\n%s", fakerestic.Calls(t, h.root))
	}
}

// TestADumpChecksWhatItIsAsked (#30): a download names its record, its
// snapshot in full, its job's tag, an absolute path and a plain file name.
func TestADumpChecksWhatItIsAsked(t *testing.T) {
	h := newRestoreHarness(t)
	good := func() *agentv1.BackupDump {
		return &agentv1.BackupDump{Repository: h.repo, SnapshotId: strings.Repeat("a", 64), Path: "/volumes/x", RestoreId: "r1", FileName: "x.tar", JobTag: testJobTag}
	}
	for name, change := range map[string]func(*agentv1.BackupDump){
		"no record":          func(p *agentv1.BackupDump) { p.RestoreId = "" },
		"a short id":         func(p *agentv1.BackupDump) { p.SnapshotId = "3f9a1c2e" },
		"no tag":             func(p *agentv1.BackupDump) { p.JobTag = "" },
		"a relative path":    func(p *agentv1.BackupDump) { p.Path = "volumes/x" },
		"a name with a /":    func(p *agentv1.BackupDump) { p.FileName = "../x.tar" },
		"a name with a line": func(p *agentv1.BackupDump) { p.FileName = "x\n.tar" },
		"no password":        func(p *agentv1.BackupDump) { p.Repository = &agentv1.BackupRepository{Url: "s3:x"} },
	} {
		payload := good()
		change(payload)
		if _, err := h.handleBackupDump(context.Background(), dumpCommand("d1", payload), &Reporter{}); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
}

// TestTheSnapshotListAndAFolderComeFromAJob (#30, S): the list and a folder
// are read by the backup tool in a job, which keeps nothing of the server
// but restic's cache, and their results fit the panel's field.
func TestTheSnapshotListAndAFolderComeFromAJob(t *testing.T) {
	h := newRestoreHarness(t)
	h.volume(t, "app_data", map[string]string{"a.txt": "a", "sub/b": "b"})
	first := h.backup(t, "app_data")
	second := h.backup(t, "app_data")
	list, err := h.handleBackupSnapshots(context.Background(), &agentv1.Command{Id: "s1", Type: "backup.snapshots", Payload: &agentv1.Command_BackupSnapshots{
		BackupSnapshots: &agentv1.BackupSnapshots{Repository: h.repo, Tags: []string{"pocketcloud", testJobTag}}}}, &Reporter{})
	if err != nil {
		t.Fatal(err)
	}
	snapshots, _ := list["snapshots"].([]any)
	if len(snapshots) != 2 || snapshots[0].(map[string]any)["id"] != second || snapshots[1].(map[string]any)["id"] != first || list["total"] != float64(2) {
		t.Errorf("the list: %v", list)
	}
	h.mu.Lock()
	job := h.jobs[len(h.jobs)-1]
	h.mu.Unlock()
	if !strings.Contains(strings.Join(job.Cmd, " "), "snapshots --tag pocketcloud --tag "+testJobTag+" --limit 500") || len(mountsOf(job)) != 1 ||
		job.Labels["pocketcloud.command"] != "s1" {
		t.Errorf("the list's job: %q %v", job.Cmd, mountsOf(job))
	}
	folder, err := h.handleBackupBrowse(context.Background(), &agentv1.Command{Id: "b1", Type: "backup.browse", Payload: &agentv1.Command_BackupBrowse{
		BackupBrowse: &agentv1.BackupBrowse{Repository: h.repo, SnapshotId: second, Path: "/volumes/app_data", Limit: 1, JobTag: testJobTag}}}, &Reporter{})
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := folder["entries"].([]any)
	if folder["total"] != float64(2) || len(entries) != 1 || entries[0].(map[string]any)["name"] != "sub" {
		t.Errorf("the folder: %v", folder)
	}
	for name, payload := range map[string]*agentv1.BackupBrowse{
		"a short id":      {Repository: h.repo, SnapshotId: second[:8], Path: "/"},
		"a relative path": {Repository: h.repo, SnapshotId: second, Path: "volumes"},
		"a big page":      {Repository: h.repo, SnapshotId: second, Path: "/", Limit: 1001},
		"a filter line":   {Repository: h.repo, SnapshotId: second, Path: "/", Match: "a\nb"},
	} {
		if _, err := h.handleBackupBrowse(context.Background(), &agentv1.Command{Payload: &agentv1.Command_BackupBrowse{BackupBrowse: payload}}, &Reporter{}); err == nil {
			t.Errorf("%s was taken", name)
		}
	}

	// a list too large for the panel loses its oldest
	result := map[string]any{"snapshots": []any{}, "total": float64(3000)}
	for range 3000 {
		result["snapshots"] = append(result["snapshots"].([]any), map[string]any{"id": strings.Repeat("a", 64), "paths": []any{strings.Repeat("/p", 50)}})
	}
	trimList(result, "snapshots", maxSnapshotsResult)
	if size := resultSize(result); size > maxSnapshotsResult || len(result["snapshots"].([]any)) < 500 {
		t.Errorf("trimmed to %d bytes, %d snapshots", size, len(result["snapshots"].([]any)))
	}
}
