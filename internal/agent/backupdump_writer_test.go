package agent

import (
	"errors"
	"testing"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
)

// chunkStream keeps the chunks sent to it, and fails once after some, as a
// stream that would take the next if it were sent.
type chunkStream struct {
	sizes  []int
	fail   int
	failed bool
}

func (c *chunkStream) Send(request *agentv1.UploadArtifactRequest) error {
	if c.fail > 0 && len(c.sizes) == c.fail && !c.failed {
		c.failed = true
		return errors.New("the stream broke")
	}
	c.sizes = append(c.sizes, len(request.GetChunk()))
	return nil
}

// TestArtifactWriterSendsChunksOfAtMostOneMiB (#30): what the job prints is
// sent in chunks of at most 1 MiB, as the gateway reads them; a send that
// fails fails that write and every one after it.
func TestArtifactWriterSendsChunksOfAtMostOneMiB(t *testing.T) {
	stream := &chunkStream{}
	writer := &artifactWriter{stream: stream}
	if n, err := writer.Write(make([]byte, 2*uploadChunkSize+10)); err != nil || n != 2*uploadChunkSize+10 {
		t.Fatalf("%d %v", n, err)
	}
	if len(stream.sizes) != 3 || stream.sizes[0] != uploadChunkSize || stream.sizes[1] != uploadChunkSize || stream.sizes[2] != 10 || writer.sent.Load() != 2*uploadChunkSize+10 {
		t.Errorf("chunks %v, %d sent", stream.sizes, writer.sent.Load())
	}
	broken := &artifactWriter{stream: &chunkStream{fail: 1}}
	if n, err := broken.Write(make([]byte, uploadChunkSize+1)); err == nil || n != uploadChunkSize {
		t.Errorf("a broken stream: %d %v", n, err)
	}
	if _, err := broken.Write([]byte("more")); err == nil || broken.err() == nil {
		t.Error("a write after a failed one went on")
	}
}
