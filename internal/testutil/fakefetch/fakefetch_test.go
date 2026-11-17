package fakefetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
)

// fetched is what one fetch gave: its bytes, the size and digest of its first
// chunk, how many chunks came, and how it ended.
type fetched struct {
	data   []byte
	total  uint64
	digest string
	chunks int
	err    error
}

func fetch(t *testing.T, client agentv1.AgentServiceClient, id string, offset uint64) fetched {
	t.Helper()
	stream, err := client.FetchArtifact(context.Background(), &agentv1.FetchArtifactRequest{ArtifactId: id, Offset: offset})
	if err != nil {
		t.Fatal(err)
	}
	var f fetched
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return f
		}
		if err != nil {
			f.err = err
			return f
		}
		if f.chunks == 0 {
			f.total, f.digest = chunk.GetTotalBytes(), chunk.GetSha256()
		}
		f.chunks++
		f.data = append(f.data, chunk.GetData()...)
	}
}

func payload(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i * 7)
	}
	return data
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestTheServerFetchesFromTheOffset: the server sends an artifact as the
// gateway does, in chunks of 1 MiB from the offset asked, the first with
// the whole artifact's size and digest; one it does not offer is
// PermissionDenied, and an offset past the end OutOfRange.
func TestTheServerFetchesFromTheOffset(t *testing.T) {
	server, dial := NewServer(t)
	data := payload(5<<20/2 + 3)
	server.Offer("panel-export:aaaaaaaaaaaaaaa", Artifact{Data: data})
	client, closeClient, err := dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()

	whole := fetch(t, client, "panel-export:aaaaaaaaaaaaaaa", 0)
	if whole.err != nil || !bytes.Equal(whole.data, data) || whole.chunks != 3 || whole.total != uint64(len(data)) || whole.digest != digestOf(data) {
		t.Errorf("the whole: %d bytes in %d chunks, %d %s %v", len(whole.data), whole.chunks, whole.total, whole.digest, whole.err)
	}
	rest := fetch(t, client, "panel-export:aaaaaaaaaaaaaaa", 3<<20/2)
	if rest.err != nil || !bytes.Equal(rest.data, data[3<<20/2:]) || rest.total != uint64(len(data)) || rest.digest != digestOf(data) {
		t.Errorf("the rest: %d bytes, %d %s %v", len(rest.data), rest.total, rest.digest, rest.err)
	}
	end := fetch(t, client, "panel-export:aaaaaaaaaaaaaaa", uint64(len(data)))
	if end.err != nil || len(end.data) != 0 || end.chunks != 1 || end.total != uint64(len(data)) {
		t.Errorf("at the end: %+v", end)
	}
	if past := fetch(t, client, "panel-export:aaaaaaaaaaaaaaa", uint64(len(data))+1); status.Code(past.err) != codes.OutOfRange {
		t.Errorf("past the end: %v", past.err)
	}
	if other := fetch(t, client, "panel-export:bbbbbbbbbbbbbbb", 0); status.Code(other.err) != codes.PermissionDenied || len(other.data) != 0 {
		t.Errorf("another artifact: %v", other.err)
	}
	server.Offer("damaged", Artifact{Data: data[:10], SHA256: digestOf(nil), Total: 12})
	if damaged := fetch(t, client, "damaged", 0); damaged.total != 12 || damaged.digest != digestOf(nil) || len(damaged.data) != 10 {
		t.Errorf("a size and digest of its own: %+v", damaged)
	}
	requests := server.Requests()
	if len(requests) != 6 || requests[1].GetOffset() != 3<<20/2 || server.Dials() != 1 {
		t.Errorf("%d requests, %d dials", len(requests), server.Dials())
	}
}

// TestAFaultBreaksOneFetch: each fault breaks the next fetch once its bytes
// passed, with its status or as a stream that ends early; with no bytes
// nothing of it comes, and the fetches after the faults pass unharmed.
func TestAFaultBreaksOneFetch(t *testing.T) {
	server, dial := NewServer(t)
	data := payload(3 << 20)
	server.Offer("a", Artifact{Data: data})
	server.Break(Fault{After: 1<<20 + 5, Code: codes.Unavailable, Message: "connection reset"}, Fault{After: 100}, Fault{Code: codes.Internal, Message: "the copy cannot be built"})
	client, closeClient, err := dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()

	broken := fetch(t, client, "a", 0)
	if status.Code(broken.err) != codes.Unavailable || status.Convert(broken.err).Message() != "connection reset" || !bytes.Equal(broken.data, data[:1<<20+5]) ||
		broken.total != uint64(len(data)) || broken.digest != digestOf(data) {
		t.Errorf("the first fetch: %d bytes, %v", len(broken.data), broken.err)
	}
	early := fetch(t, client, "a", 1<<20+5)
	if early.err != nil || !bytes.Equal(early.data, data[1<<20+5:1<<20+105]) || early.total != uint64(len(data)) {
		t.Errorf("the second: %d bytes, %v", len(early.data), early.err)
	}
	none := fetch(t, client, "a", 0)
	if status.Code(none.err) != codes.Internal || none.chunks != 0 {
		t.Errorf("the third: %+v", none)
	}
	if whole := fetch(t, client, "a", 0); whole.err != nil || !bytes.Equal(whole.data, data) {
		t.Errorf("the fourth: %d bytes, %v", len(whole.data), whole.err)
	}
	// a fault past the end breaks nothing
	server.Break(Fault{After: len(data), Code: codes.Unavailable})
	if whole := fetch(t, client, "a", 0); whole.err != nil || !bytes.Equal(whole.data, data) {
		t.Errorf("a fault past the end: %d bytes, %v", len(whole.data), whole.err)
	}
}

// TestTheBreakerBreaksAGatewaysFetches: a Breaker, as a server's stream
// interceptor, breaks its FetchArtifact streams one fault each, counts them,
// and leaves every other stream alone.
func TestTheBreakerBreaksAGatewaysFetches(t *testing.T) {
	breaker := NewBreaker(Fault{After: 10, Code: codes.Unavailable, Message: "gone"})
	panel := &Server{artifacts: map[string]Artifact{}}
	data := payload(2 << 20)
	panel.Offer("a", Artifact{Data: data})
	listener := bufconn.Listen(4 << 20)
	server := grpc.NewServer(grpc.ChainStreamInterceptor(breaker.Interceptor))
	agentv1.RegisterAgentServiceServer(server, panel)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///panel", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := agentv1.NewAgentServiceClient(conn)

	// another stream passes
	logs, err := client.StreamLogs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logs.CloseAndRecv(); status.Code(err) != codes.Unimplemented {
		t.Errorf("another stream: %v", err)
	}
	if broken := fetch(t, client, "a", 0); status.Code(broken.err) != codes.Unavailable || !bytes.Equal(broken.data, data[:10]) {
		t.Errorf("the first fetch: %d bytes, %v", len(broken.data), broken.err)
	}
	breaker.Add(Fault{After: 1 << 20, Code: codes.OK})
	if early := fetch(t, client, "a", 10); early.err != nil || !bytes.Equal(early.data, data[10:10+1<<20]) {
		t.Errorf("the second fetch: %d bytes, %v", len(early.data), early.err)
	}
	if whole := fetch(t, client, "a", 0); whole.err != nil || !bytes.Equal(whole.data, data) {
		t.Errorf("the third fetch: %d bytes, %v", len(whole.data), whole.err)
	}
	if breaker.Broken() != 2 {
		t.Errorf("%d broken", breaker.Broken())
	}
}
