package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
)

// The panel's backup of itself (#32). A server whose agent carries the
// panel's backup job runs it as a backup run of a fourth source: the
// panel's encrypted copy of itself (internal/panelbackup's PCPB1), which the
// agent fetches with FetchArtifact on a connection of its own and writes to
// the job's standard input as the relay's frames, as it writes a database
// dump's. Nothing of the server is mounted, no command runs in a container,
// and nothing is spooled on the server: the copy goes from the panel to
// restic as it comes, and the agent only checks its size and its digest. A
// fetch that breaks off is resumed from where it got to; one that does not
// end well ends the relay with backup.RelayRetry, which the panel tries
// again, or 1 when the panel no longer offers the copy.

// defaultPanelFetchWaits are the waits before each resume of the panel's
// copy: three resumes at most.
var defaultPanelFetchWaits = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}

// panelFetchWaits are the waits before each resume of the panel's copy.
func (a *Agent) panelFetchWaits() []time.Duration {
	if a.PanelFetchWaits != nil {
		return a.PanelFetchWaits
	}
	return defaultPanelFetchWaits
}

// feedPanelExport fetches the panel's copy id and writes it to the job's
// standard input as the relay's frames, and last how it ended, with the
// secrets taken out of what it says. It writes no exit frame when writing
// failed: restic, which stopped reading, says why.
func (a *Agent) feedPanelExport(ctx context.Context, stdin io.WriteCloser, id string, secrets []string) {
	defer stdin.Close()
	frames := backup.NewFrameWriter(stdin)
	code, message, ok := a.fetchPanelExport(ctx, id, frames)
	if ok {
		_ = frames.End(code, backup.Redact(message, secrets...))
	}
}

// errFetchWrite is a fetch whose bytes could not be written on.
type errFetchWrite struct{ err error }

func (e *errFetchWrite) Error() string { return e.err.Error() }

// errFetchDamaged is a copy that is not what the panel said it is: its
// size or its digest differ.
type errFetchDamaged struct{ message string }

func (e *errFetchDamaged) Error() string { return e.message }

// exportFetch is the fetch of the panel's copy, over its resumes: what the
// panel said of the copy, and what of it came.
type exportFetch struct {
	id       string
	known    bool
	total    uint64
	digest   string
	received uint64
	hash     hash.Hash
}

// fetchPanelExport writes the panel's copy id to w, and says the relay's
// exit code and its last words; false when writing to w failed.
func (a *Agent) fetchPanelExport(ctx context.Context, id string, w io.Writer) (int, string, bool) {
	client, closeClient, err := a.liveClient(ctx)
	if err != nil {
		return backup.RelayRetry, "the agent cannot reach the panel for its copy: " + err.Error(), true
	}
	defer closeClient()
	f := &exportFetch{id: id, hash: sha256.New()}
	waits := a.panelFetchWaits()
	for resumes := 0; ; resumes++ {
		err := f.stream(ctx, client, w)
		var write *errFetchWrite
		var damaged *errFetchDamaged
		switch {
		case errors.As(err, &write):
			return 0, "", false
		case errors.As(err, &damaged):
			return backup.RelayRetry, damaged.message, true
		case ctx.Err() != nil:
			return 1, "the fetch of the panel's copy was stopped: " + ctx.Err().Error(), true
		case err == nil && f.known && f.received == f.total:
			if got := hex.EncodeToString(f.hash.Sum(nil)); got != f.digest {
				return backup.RelayRetry, f.damaged(got), true
			}
			return 0, "", true
		}
		said := "it ended early"
		if err != nil {
			said = status.Convert(err).Message()
		}
		switch status.Code(err) {
		case codes.PermissionDenied, codes.FailedPrecondition, codes.NotFound, codes.Unimplemented:
			return 1, "the panel no longer offers this copy: " + said, true
		case codes.OK, codes.Unavailable, codes.DeadlineExceeded, codes.Internal, codes.Canceled:
			// an early end, a reset or a panel that went for a moment:
			// resumed from where it got to
		default:
			return backup.RelayRetry, f.brokeOff(said), true
		}
		if resumes == len(waits) {
			return backup.RelayRetry, f.brokeOff(said), true
		}
		a.Log.Warn("the panel's copy broke off: resuming", "copy", id, "after", f.received, "of", f.total, "error", said)
		select {
		case <-ctx.Done():
			return 1, "the fetch of the panel's copy was stopped: " + ctx.Err().Error(), true
		case <-time.After(waits[resumes]):
		}
	}
}

// stream fetches the copy from where the fetch got to, and writes what
// comes to w; nil when the stream ended, early or not.
func (f *exportFetch) stream(parent context.Context, client agentv1.AgentServiceClient, w io.Writer) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stream, err := client.FetchArtifact(ctx, &agentv1.FetchArtifactRequest{ArtifactId: f.id, Offset: f.received})
	if err != nil {
		return err
	}
	for first := true; ; first = false {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if first {
			if !f.known {
				f.known, f.total, f.digest = true, chunk.GetTotalBytes(), chunk.GetSha256()
			} else if chunk.GetTotalBytes() != f.total || chunk.GetSha256() != f.digest {
				// the panel made another copy meanwhile: what came of the
				// first is of no use with it
				return &errFetchDamaged{fmt.Sprintf("the panel's copy arrived damaged: it changed while it came, from %d bytes, sha256 %s, to %d bytes, sha256 %s",
					f.total, f.digest, chunk.GetTotalBytes(), chunk.GetSha256())}
			}
		}
		data := chunk.GetData()
		if uint64(len(data)) > f.total-f.received {
			return &errFetchDamaged{fmt.Sprintf("the panel's copy arrived damaged: more than the %d bytes, sha256 %s, the panel said", f.total, f.digest)}
		}
		if _, err := w.Write(data); err != nil {
			return &errFetchWrite{err}
		}
		f.hash.Write(data)
		f.received += uint64(len(data))
	}
}

// brokeOff says that the fetch broke off, and how far it got.
func (f *exportFetch) brokeOff(said string) string {
	if !f.known {
		return "the panel's copy did not come: " + said
	}
	return fmt.Sprintf("the panel's copy broke off after %s of %s: %s", sizeText(int64(f.received)), sizeText(int64(f.total)), said) //nolint:gosec // sizes of a file
}

// damaged says that the copy that came, whose digest is got, is not the
// one the panel said.
func (f *exportFetch) damaged(got string) string {
	return fmt.Sprintf("the panel's copy arrived damaged: %d bytes, sha256 %s, where the panel said %d bytes, sha256 %s", f.received, got, f.total, f.digest)
}
