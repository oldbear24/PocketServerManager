// Package fakefetch serves FetchArtifact as the panel's gateway does, from
// the offset an interrupted fetch got to, and breaks fetches on purpose, for
// tests of the agent's fetch of the panel's copy of itself (#32): Server is
// a panel of its own on an in-memory connection, which an agent reaches
// through its LiveUpload; Breaker breaks the streams of a real gateway, as
// one of its StreamInterceptors.
package fakefetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
)

// FetchMethod is the full name of the gRPC method a Breaker breaks.
const FetchMethod = "/pocketcloud.agent.v1.AgentService/FetchArtifact"

// chunkSize is the size of a chunk Server sends, the gateway's.
const chunkSize = 1 << 20

// Fault breaks one fetch: once After bytes of it passed, it ends with Code
// and Message, or, with codes.OK, as a stream that ends early. With After 0
// nothing passes, not even the size and digest of the first chunk; a
// fetch whose bytes end first is not broken.
type Fault struct {
	After   int
	Code    codes.Code
	Message string
}

// faults hands out faults, one per fetch, in order; a fetch past the last
// passes unharmed.
type faults struct {
	mu     sync.Mutex
	list   []Fault
	broken int
}

func (f *faults) next() *Fault {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.list) == 0 {
		return nil
	}
	fault := f.list[0]
	f.list = f.list[1:]
	f.broken++
	return &fault
}

func (f *faults) add(list []Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list = append(f.list, list...)
}

// count is how many fetches were broken.
func (f *faults) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.broken
}

// breaking is a fetch's stream that a fault breaks: what it sends passes
// until the fault's bytes did, and then it fails.
type breaking struct {
	grpc.ServerStream
	fault *Fault
	sent  int
}

// errEarly stops a fetch whose fault ends it early, as a stream that ends
// with no error.
var errEarly = errors.New("the fetch ends early")

func (b *breaking) SendMsg(message any) error {
	chunk, ok := message.(*agentv1.FetchArtifactResponse)
	if !ok || b.fault == nil {
		return b.ServerStream.SendMsg(message)
	}
	room := b.fault.After - b.sent
	if room > len(chunk.GetData()) || room == len(chunk.GetData()) && room > 0 {
		b.sent += len(chunk.GetData())
		return b.ServerStream.SendMsg(message)
	}
	if room > 0 {
		// what passes of it, with its size and digest on the first
		part := &agentv1.FetchArtifactResponse{Data: chunk.GetData()[:room], TotalBytes: chunk.GetTotalBytes(), Sha256: chunk.GetSha256()}
		if err := b.ServerStream.SendMsg(part); err != nil {
			return err
		}
		b.sent += room
	}
	if b.fault.Code == codes.OK {
		return errEarly
	}
	return status.Error(b.fault.Code, b.fault.Message)
}

// result is what a broken fetch's handler returns: an early end is none.
func result(err error) error {
	if err == errEarly { //nolint:errorlint // the sentinel itself, never wrapped
		return nil
	}
	return err
}

// Breaker breaks the fetches of a real gateway, one fault per fetch in the
// order they come, as a stream interceptor: append Interceptor to the
// gateway's StreamInterceptors before it starts.
type Breaker struct{ faults faults }

// NewBreaker breaks the next fetches with faults, one each.
func NewBreaker(list ...Fault) *Breaker {
	b := &Breaker{}
	b.faults.add(list)
	return b
}

// Add breaks the fetches after those already planned with more faults.
func (b *Breaker) Add(list ...Fault) { b.faults.add(list) }

// Broken counts the fetches it broke.
func (b *Breaker) Broken() int { return b.faults.count() }

// Interceptor breaks FetchArtifact streams, and passes the others on.
func (b *Breaker) Interceptor(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if info.FullMethod != FetchMethod {
		return handler(srv, stream)
	}
	fault := b.faults.next()
	if fault == nil {
		return handler(srv, stream)
	}
	return result(handler(srv, &breaking{ServerStream: stream, fault: fault}))
}

// Artifact is a file Server offers.
type Artifact struct {
	Data []byte
	// SHA256 is the digest Server says; the data's when empty.
	SHA256 string
	// Total is the size Server says; the data's when 0.
	Total uint64
}

// Server is a panel that offers artifacts to fetch, and breaks the fetches
// it was told to: the gateway's FetchArtifact, from memory.
type Server struct {
	agentv1.UnimplementedAgentServiceServer

	mu        sync.Mutex
	artifacts map[string]Artifact
	requests  []*agentv1.FetchArtifactRequest
	faults    faults
	dials     int
}

// NewServer serves artifacts on a connection in memory until the test ends.
// Its Dial is an agent's LiveUpload.
func NewServer(t testing.TB) (*Server, func(ctx context.Context) (agentv1.AgentServiceClient, func(), error)) {
	t.Helper()
	s := &Server{artifacts: map[string]Artifact{}}
	listener := bufconn.Listen(4 << 20)
	server := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(server, s)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	dial := func(context.Context) (agentv1.AgentServiceClient, func(), error) {
		s.mu.Lock()
		s.dials++
		s.mu.Unlock()
		conn, err := grpc.NewClient("passthrough:///panel", grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
		if err != nil {
			return nil, nil, err
		}
		return agentv1.NewAgentServiceClient(conn), func() { _ = conn.Close() }, nil
	}
	return s, dial
}

// Offer offers an artifact under id.
func (s *Server) Offer(id string, artifact Artifact) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.artifacts[id] = artifact
}

// Break breaks the next fetches with faults, one each.
func (s *Server) Break(list ...Fault) { s.faults.add(list) }

// Requests are the fetches the server got, in order.
func (s *Server) Requests() []*agentv1.FetchArtifactRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*agentv1.FetchArtifactRequest(nil), s.requests...)
}

// Dials counts the connections of its own an agent opened.
func (s *Server) Dials() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dials
}

// FetchArtifact sends an artifact from the request's offset in chunks of 1
// MiB, the first with the artifact's size and digest, as the gateway does;
// an artifact it does not offer is PermissionDenied, as there.
func (s *Server) FetchArtifact(request *agentv1.FetchArtifactRequest, stream grpc.ServerStreamingServer[agentv1.FetchArtifactResponse]) error {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	artifact, ok := s.artifacts[request.GetArtifactId()]
	s.mu.Unlock()
	fault := s.faults.next()
	if !ok {
		return status.Error(codes.PermissionDenied, "the artifact does not belong to a command of this host")
	}
	total, digest := artifact.Total, artifact.SHA256
	if total == 0 {
		total = uint64(len(artifact.Data))
	}
	if digest == "" {
		sum := sha256.Sum256(artifact.Data)
		digest = hex.EncodeToString(sum[:])
	}
	offset := request.GetOffset()
	if offset > uint64(len(artifact.Data)) {
		return status.Error(codes.OutOfRange, "the offset is past the end of the artifact")
	}
	var out grpc.ServerStream = stream
	if fault != nil {
		out = &breaking{ServerStream: stream, fault: fault}
	}
	data := artifact.Data[offset:]
	for first := true; first || len(data) > 0; first = false {
		n := min(len(data), chunkSize)
		chunk := &agentv1.FetchArtifactResponse{Data: data[:n]}
		if first {
			chunk.TotalBytes, chunk.Sha256 = total, digest
		}
		if err := out.SendMsg(chunk); err != nil {
			return result(err)
		}
		data = data[n:]
	}
	return nil
}
