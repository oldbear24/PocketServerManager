package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
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
	// a PostgreSQL dump downloaded as SQL (#53): converted by the server's
	// own pg_restore, in a container of its own
	var converter *dockerapi.ContainerInspect
	if from := payload.GetSqlFrom(); from != "" {
		var err error
		if converter, err = a.sqlSource(ctx, from, payload.GetPath()); err != nil {
			return nil, err
		}
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
	if converter != nil {
		err = a.convertToSQL(ctx, command, converter, repo, job, writer, io.MultiWriter(reporter, log))
	} else {
		_, err = a.backupJob(ctx, repo, job, io.MultiWriter(reporter, log))
	}
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

// errNotPostgresServer refuses a conversion to SQL in a container that is
// not a running shared PostgreSQL server's.
var errNotPostgresServer error = sentenceError("The SQL is made by a shared PostgreSQL server's own pg_restore only.")

// sqlSource checks the container whose image converts a PostgreSQL dump to
// SQL (#53): a running shared PostgreSQL server's; and the dump, which is
// a database job's one file.
func (a *Agent) sqlSource(ctx context.Context, container, file string) (*dockerapi.ContainerInspect, error) {
	if path.Dir(file) != "/" {
		return nil, fmt.Errorf("a dump converted to SQL is the one file of a database's snapshot, not %s", file)
	}
	if _, err := databases.FileDatabase(databases.Postgres, path.Base(file)); err != nil {
		return nil, err
	}
	info, err := a.Docker.Inspect(ctx, container)
	if dockerapi.IsNotFound(err) {
		return nil, errNotPostgresServer
	} else if err != nil {
		return nil, err
	}
	image, _ := info.Config["Image"].(string)
	if !info.State.Running || labelOf(info, "pocketcloud.role") != "db" || !strings.Contains(image, "postgres") || info.Image == "" {
		return nil, errNotPostgresServer
	}
	return info, nil
}

// convertToSQL runs a dump job whose output goes to the standard input of a
// conversion container made from the server's own image, so that its
// pg_restore is the server's, which writes plain SQL to out (#53). The
// container has no network, no capability, a read-only root and its own
// /tmp, and keeps no log; a cancelled download stops it, and the agent
// removes one it left when it starts again. A failure of either aborts the
// download.
func (a *Agent) convertToSQL(ctx context.Context, command *agentv1.Command, server *dockerapi.ContainerInspect, repo *agentv1.BackupRepository,
	dump backupJobSpec, out, log io.Writer) error {
	config := map[string]any{
		"Image":       server.Image,
		"Entrypoint":  []string{"pg_restore"},
		"Cmd":         []string{"--file=-", "--no-owner", "--no-privileges"},
		"User":        "postgres",
		"OpenStdin":   true,
		"StdinOnce":   true,
		"AttachStdin": true,
		// what cleanupBackupJobs goes by
		"Labels": map[string]string{"pocketcloud.role": "job", "pocketcloud.command": command.GetId(), backupTargetLabel: repo.GetTargetId()},
		"HostConfig": map[string]any{
			"NetworkMode":    "none",
			"CapDrop":        []string{"ALL"},
			"SecurityOpt":    []string{"no-new-privileges"},
			"ReadonlyRootfs": true,
			"Tmpfs":          map[string]string{"/tmp": "rw,noexec,nosuid,size=64m"},
			"PidsLimit":      64,
			"LogConfig":      map[string]any{"Type": "none"},
		},
	}
	var stdin io.WriteCloser
	fed := make(chan error, 1)
	started := false
	hooks := jobHooks{
		stopGrace: backupStopGrace,
		attach:    true,
		prepare: func(id string) error {
			attached, err := a.Docker.Attach(ctx, id)
			if err != nil {
				return fmt.Errorf("the conversion's input cannot be attached: %w", err)
			}
			stdin = attached
			return nil
		},
		started: func(string) {
			started = true
			go func() {
				// the dump is the conversion's input, which ends with it
				dump.attachOutput = stdin
				_, err := a.backupJob(ctx, repo, dump, log)
				_ = stdin.Close()
				fed <- err
			}()
		},
	}
	stderr := &tailBuffer{limit: 4 << 10, mask: maskValues(backup.Secrets(repo.GetPassword(), repo.GetEnv(), repo.GetSshKey()))}
	err := dockerSites{a: a}.runWith(ctx, config, hooks, out, stderr)
	if stdin != nil {
		_ = stdin.Close()
	}
	var dumpErr error
	if started {
		dumpErr = <-fed
	}
	var refused *backupRefused
	var exit *exitError
	switch {
	case errors.As(dumpErr, &refused):
		// restic's or the tool's own words: the conversion only ran out of
		// input
		return dumpErr
	case errors.As(err, &exit):
		message := lastLines(stderr.String(), 4)
		if message == "" {
			message = "no message"
		}
		return fmt.Errorf("the conversion to SQL failed with exit code %d: %s", exit.code, message)
	case err != nil:
		return err
	}
	return dumpErr
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
