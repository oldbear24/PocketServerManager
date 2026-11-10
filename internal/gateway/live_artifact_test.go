package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// fakeUpload is an agent's UploadArtifact stream: a header, chunks, and then
// the agent's end, or an error.
type fakeUpload struct {
	ctx      context.Context
	parts    []*agentv1.UploadArtifactRequest
	end      error
	received int
	response *agentv1.UploadArtifactResponse
}

func (f *fakeUpload) Recv() (*agentv1.UploadArtifactRequest, error) {
	if f.received < len(f.parts) {
		f.received++
		return f.parts[f.received-1], nil
	}
	return nil, f.end
}

func (f *fakeUpload) SendAndClose(response *agentv1.UploadArtifactResponse) error {
	f.response = response
	return nil
}

func (f *fakeUpload) Context() context.Context     { return f.ctx }
func (f *fakeUpload) SetHeader(metadata.MD) error  { return nil }
func (f *fakeUpload) SendHeader(metadata.MD) error { return nil }
func (f *fakeUpload) SetTrailer(metadata.MD)       {}
func (f *fakeUpload) SendMsg(any) error            { return nil }
func (f *fakeUpload) RecvMsg(any) error            { return nil }

// liveFixture is a gateway with a server, its command and an upload of it.
type liveFixture struct {
	app     *tests.TestApp
	server  *Server
	host    *core.Record
	command *core.Record
}

func newLiveFixture(t *testing.T, handler LiveArtifactHandler) *liveFixture {
	t.Helper()
	app := testutil.NewApp(t)
	t.Cleanup(app.Cleanup)
	host := testutil.NewHost(t, app, "node-1")
	collection, err := app.FindCollectionByNameOrId(schema.Commands)
	if err != nil {
		t.Fatal(err)
	}
	command := core.NewRecord(collection)
	command.Load(map[string]any{"host": host.Id, "type": "backup.dump", "status": schema.CommandRunning})
	if err := app.Save(command); err != nil {
		t.Fatal(err)
	}
	return &liveFixture{app: app, host: host, command: command,
		server: &Server{App: app, LiveArtifacts: map[string]LiveArtifactHandler{"backup_download": handler}}}
}

// upload is an upload of the fixture's command by the agent of host.
func (f *liveFixture) upload(host string, chunks ...[]byte) *fakeUpload {
	parts := []*agentv1.UploadArtifactRequest{{Part: &agentv1.UploadArtifactRequest_Header{Header: &agentv1.UploadArtifactHeader{
		CommandId: f.command.Id, Kind: "backup_download", FileName: "data.tar",
	}}}}
	for _, chunk := range chunks {
		parts = append(parts, &agentv1.UploadArtifactRequest{Part: &agentv1.UploadArtifactRequest_Chunk{Chunk: chunk}})
	}
	return &fakeUpload{ctx: context.WithValue(context.Background(), identityKey{}, identity{HostID: host}), parts: parts, end: io.EOF}
}

// TestLiveArtifactStreamsWithoutSpooling (#30): a live upload is passed to
// its handler as it arrives, a chunk received only once the one before was
// read, and nothing of it is stored on the panel.
func TestLiveArtifactStreamsWithoutSpooling(t *testing.T) {
	var received []int
	var stream *fakeUpload
	var got bytes.Buffer
	f := newLiveFixture(t, func(_ context.Context, command *core.Record, header *agentv1.UploadArtifactHeader, body io.Reader) (uint64, error) {
		if header.GetFileName() != "data.tar" || command.GetString("type") != "backup.dump" {
			t.Errorf("handed %s of %s", header.GetFileName(), command.GetString("type"))
		}
		buffer := make([]byte, 3)
		for {
			n, err := body.Read(buffer)
			got.Write(buffer[:n])
			// the header, and the chunk the read is in
			received = append(received, stream.received)
			if errors.Is(err, io.EOF) {
				return uint64(got.Len()), nil //nolint:gosec // a length
			}
			if err != nil {
				return 0, err
			}
		}
	})
	stream = f.upload(f.host.Id, []byte("tar-"), []byte("archive"), []byte{}, []byte("-end"))
	if err := f.server.UploadArtifact(stream); err != nil {
		t.Fatal(err)
	}
	if got.String() != "tar-archive-end" || f.response(stream) != uint64(len("tar-archive-end")) {
		t.Errorf("passed on %q, answered %d", got.String(), f.response(stream))
	}
	// reads of 3 bytes: tar | - | arc | hiv | e | -en (past the empty
	// chunk) | d | the end
	if want := []int{2, 2, 3, 3, 3, 5, 5, 5}; !equalInts(received, want) {
		t.Errorf("received the chunks at the reads %v, not %v", received, want)
	}
	temp := filepath.Join(f.app.DataDir(), core.LocalTempDirName)
	if entries, err := os.ReadDir(temp); err == nil && len(entries) > 0 {
		t.Errorf("stored on the way: %v", entries)
	}
}

func (f *liveFixture) response(stream *fakeUpload) uint64 {
	if stream.response == nil {
		return 0
	}
	return stream.response.GetSizeBytes()
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestLiveArtifactErrorAbortsTheUpload (#30): a handler that fails, as when
// the browser left, aborts the upload, so that the agent stops sending and
// never takes it as delivered; an agent that aborts reaches the handler as
// an error, not as the end.
func TestLiveArtifactErrorAbortsTheUpload(t *testing.T) {
	f := newLiveFixture(t, func(_ context.Context, _ *core.Record, _ *agentv1.UploadArtifactHeader, body io.Reader) (uint64, error) {
		buffer := make([]byte, 4)
		if _, err := body.Read(buffer); err != nil {
			return 0, err
		}
		return 0, errors.New("nobody downloads this any more")
	})
	stream := f.upload(f.host.Id, []byte("tar-archive"))
	err := f.server.UploadArtifact(stream)
	if status.Code(err) != codes.Aborted || stream.response != nil {
		t.Errorf("a handler that failed: %v, answered %v", err, stream.response)
	}

	var seen error
	f.server.LiveArtifacts["backup_download"] = func(_ context.Context, _ *core.Record, _ *agentv1.UploadArtifactHeader, body io.Reader) (uint64, error) {
		_, seen = io.Copy(io.Discard, body)
		return 0, seen
	}
	stream = f.upload(f.host.Id, []byte("tar-"))
	stream.end = status.Error(codes.Canceled, "the job failed")
	if err := f.server.UploadArtifact(stream); status.Code(err) != codes.Aborted || status.Code(seen) != codes.Canceled {
		t.Errorf("an agent that aborted: %v, the handler saw %v", err, seen)
	}

	// a second header is no chunk
	stream = f.upload(f.host.Id, []byte("tar-"))
	stream.parts = append(stream.parts, stream.parts[0])
	if err := f.server.UploadArtifact(stream); status.Code(err) != codes.Aborted || status.Code(seen) != codes.InvalidArgument {
		t.Errorf("a second header: %v, the handler saw %v", err, seen)
	}
}

// TestLiveArtifactNeedsACommandOfThisHost (#30): an agent passes on only
// what a command of its own host asks for: one of another host, or none,
// never reaches the handler.
func TestLiveArtifactNeedsACommandOfThisHost(t *testing.T) {
	called := false
	f := newLiveFixture(t, func(context.Context, *core.Record, *agentv1.UploadArtifactHeader, io.Reader) (uint64, error) {
		called = true
		return 0, nil
	})
	other := testutil.NewHost(t, f.app, "node-2")
	if err := f.server.UploadArtifact(f.upload(other.Id, []byte("x"))); status.Code(err) != codes.PermissionDenied {
		t.Errorf("another host's command: %v", err)
	}
	stream := f.upload(f.host.Id, []byte("x"))
	stream.parts[0].GetHeader().CommandId = "nosuchcommand00"
	if err := f.server.UploadArtifact(stream); status.Code(err) != codes.PermissionDenied {
		t.Errorf("no command: %v", err)
	}
	if called {
		t.Error("the handler was called")
	}
}
