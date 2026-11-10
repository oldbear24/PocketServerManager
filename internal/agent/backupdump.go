package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
)

// Downloads (#30). A snapshot's folder, as a tar, or one file, goes from
// restic to the browser as restic prints it: through the job's attached
// output, which keeps nothing in the Engine's logs, and an upload to the
// panel of a kind the panel passes on as it reads it. Nothing is stored on
// the way, and a slow browser slows restic down. The upload has a
// connection of its own, so that a transfer that stalls never holds up the
// command stream's keepalive.

// artifactKindBackupDownload is the upload of a download, which the panel
// passes on as it arrives.
const artifactKindBackupDownload = "backup_download"

// downloadProgressEvery is how often a download says how much it sent;
// tests shorten it.
var downloadProgressEvery = 2 * time.Second

// errDownloadGone is a download whose browser, or panel, went away.
var errDownloadGone = errors.New("nobody downloads this any more: the browser or the panel went away")

// liveClient opens a client of a connection of its own to the panel.
func (a *Agent) liveClient(ctx context.Context) (agentv1.AgentServiceClient, func(), error) {
	if a.LiveUpload != nil {
		return a.LiveUpload(ctx)
	}
	if a.Identity == nil {
		return nil, nil, errors.New("the agent has no identity to reach the panel with")
	}
	conn, err := a.dial()
	if err != nil {
		return nil, nil, err
	}
	return agentv1.NewAgentServiceClient(conn), func() { _ = conn.Close() }, nil
}

// checkDownloadName checks the name a browser saves a download under.
func checkDownloadName(name string) error {
	if name == "" || name == "." || name == ".." || len(name) > 255 || !utf8.ValidString(name) ||
		strings.ContainsRune(name, '/') || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("a download's file name cannot be %q", name)
	}
	return nil
}

// handleBackupDump streams a snapshot's folder, or file, to the panel while
// a browser downloads it.
func (a *Agent) handleBackupDump(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetBackupDump()
	if err := checkRepository(payload.GetRepository()); err != nil {
		return nil, err
	}
	switch {
	case payload.GetRestoreId() == "":
		return nil, errors.New("a download names the panel's record of it")
	case !snapshotID.MatchString(payload.GetSnapshotId()):
		return nil, fmt.Errorf("a snapshot is named by its full id, not %q", payload.GetSnapshotId())
	case !jobTag.MatchString(payload.GetJobTag()):
		return nil, fmt.Errorf("a download names its job's tag, job: and the job's id, not %q", payload.GetJobTag())
	}
	if err := checkSnapshotPath(payload.GetPath()); err != nil {
		return nil, err
	}
	if err := checkDownloadName(payload.GetFileName()); err != nil {
		return nil, err
	}

	client, closeClient, err := a.liveClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("the agent cannot reach the panel for the download: %w", err)
	}
	defer closeClient()
	// an upload that is cancelled tells the panel to abort the browser's
	streamCtx, abort := context.WithCancel(ctx)
	defer abort()
	stream, err := client.UploadArtifact(streamCtx)
	if err != nil {
		return nil, fmt.Errorf("the agent cannot reach the panel for the download: %w", err)
	}
	header := &agentv1.UploadArtifactHeader{CommandId: command.GetId(), Kind: artifactKindBackupDownload, FileName: payload.GetFileName()}
	if err := stream.Send(&agentv1.UploadArtifactRequest{Part: &agentv1.UploadArtifactRequest_Header{Header: header}}); err != nil {
		return nil, errDownloadGone
	}

	// a browser that goes fails a write, which stops the job with its grace
	// (see runWith): restic removes its lock
	writer := &artifactWriter{stream: stream}
	ticking := make(chan struct{})
	var ticker sync.WaitGroup
	ticker.Go(func() {
		tick := time.NewTicker(downloadProgressEvery)
		defer tick.Stop()
		for {
			select {
			case <-ticking:
				return
			case <-tick.C:
				sent := writer.sent.Load()
				reporter.Report(&agentv1.CommandProgress{Step: "download", Message: "Sent " + sizeText(sent), Percent: -1, Done: sent, Unit: "bytes"})
			}
		}
	})
	repo := payload.GetRepository()
	log := &tailBuffer{limit: backupLogTail, mask: maskValues(backup.Secrets(repo.GetPassword(), repo.GetEnv(), repo.GetSshKey()))}
	job := backupJobSpec{
		args:         []string{"dump", "--snapshot", payload.GetSnapshotId(), "--path", payload.GetPath(), "--expect-tag", payload.GetJobTag()},
		attachOutput: writer, stopGrace: backupStopGrace, labels: commandLabels(command),
	}
	_, err = a.backupJob(ctx, repo, job, io.MultiWriter(reporter, log))
	close(ticking)
	ticker.Wait()
	result := map[string]any{"bytes": writer.sent.Load(), "file_name": payload.GetFileName()}
	switch {
	case writer.err() != nil:
		abort()
		return result, errDownloadGone
	case err != nil:
		// the browser's download is aborted, not kept cut short
		abort()
		return result, err
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		return result, fmt.Errorf("the panel did not take the whole download: %w", err)
	}
	return result, nil
}

// artifactWriter sends what it is given to an upload, in chunks of at most
// uploadChunkSize, each once the upload took it; the first that fails
// fails every write after it.
type artifactWriter struct {
	stream interface {
		Send(*agentv1.UploadArtifactRequest) error
	}
	sent    atomic.Int64
	mu      sync.Mutex
	sendErr error
}

func (w *artifactWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sendErr != nil {
		return 0, w.sendErr
	}
	written := 0
	for written < len(data) {
		chunk := data[written:min(len(data), written+uploadChunkSize)]
		if err := w.stream.Send(&agentv1.UploadArtifactRequest{Part: &agentv1.UploadArtifactRequest_Chunk{Chunk: chunk}}); err != nil {
			w.sendErr = err
			return written, err
		}
		written += len(chunk)
		w.sent.Add(int64(len(chunk)))
	}
	return written, nil
}

// err is the error of the send that failed, if one did.
func (w *artifactWriter) err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sendErr
}
