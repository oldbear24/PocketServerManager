package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/events"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// Downloads (#30) send a folder of a snapshot as a tar, or one file as it
// is, from the repository to a browser, live: the agent streams restic's
// dump through UploadArtifact, and the panel passes each chunk on to the
// browser as it reads it. Nothing is stored on the way, there is no limit
// to the size, and a browser that reads slowly slows restic down. A
// browser's navigation carries no Authorization header: an admin's request
// gives a ticket, used once within downloadTicketLife, and the download's
// GET redeems it. restic runs only once a browser reads.

const (
	// downloadTicketLife is how long a download's link is good.
	downloadTicketLife = 60 * time.Second
	// downloadQueue is how long a download waits for its server.
	downloadQueue = 2 * time.Minute
	// downloadFirstByte is how long the browser waits for the first bytes.
	downloadFirstByte = 120 * time.Second
	// downloadBuffer is what the panel reads at once.
	downloadBuffer = 256 << 10
	// downloadFlushBytes and downloadFlushEvery say how often what was
	// written is flushed to the browser.
	downloadFlushBytes = 1 << 20
	downloadFlushEvery = time.Second
	// artifactBackupDownload is the live upload of a download.
	artifactBackupDownload = "backup_download"
	// downloadUsed answers a link that is wrong, used or too old.
	downloadUsed = "This download link has been used or has expired: start the download again."
)

// downloadTicket lets one browser's navigation start one download.
type downloadTicket struct {
	restore string
	userID  string
	label   string
	actor   audit.Actor
	expires time.Time
}

// downloadTickets are kept in memory: a panel restart ends every download.
type downloadTickets struct {
	mu      sync.Mutex
	tickets map[string]*downloadTicket
}

func (t *downloadTickets) issue(ticket *downloadTicket) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tickets == nil {
		t.tickets = map[string]*downloadTicket{}
	}
	now := time.Now()
	for key, existing := range t.tickets {
		if now.After(existing.expires) {
			delete(t.tickets, key)
		}
	}
	t.tickets[id] = ticket
	return id, nil
}

// take returns a ticket once, if it is still good.
func (t *downloadTickets) take(id string) *downloadTicket {
	t.mu.Lock()
	defer t.mu.Unlock()
	ticket := t.tickets[id]
	if ticket == nil {
		return nil
	}
	delete(t.tickets, id)
	if time.Now().After(ticket.expires) {
		return nil
	}
	return ticket
}

// downloadSession joins a download's GET and its agent's upload, by the
// command's id: the upload hands its reader over, the GET says how the copy
// ended, and the finisher says the command ended. It stays until its
// command's finisher ran, so that a download whose browser is done is not
// taken for one a panel restart cut.
type downloadSession struct {
	restore  string
	body     chan io.Reader
	done     chan error
	served   chan struct{}
	finished chan struct{}

	serveOnce  sync.Once
	finishOnce sync.Once
	// left says the browser went before the end
	left atomic.Bool
}

func newDownloadSession(restore string) *downloadSession {
	return &downloadSession{restore: restore, body: make(chan io.Reader), done: make(chan error, 1),
		served: make(chan struct{}), finished: make(chan struct{})}
}

// end tells the upload how the copy ended, once.
func (s *downloadSession) end(err error) {
	select {
	case s.done <- err:
	default:
	}
}

// downloadSessions are the downloads under way, by their command.
type downloadSessions struct {
	sessions sync.Map
}

func (d *downloadSessions) get(commandID string) *downloadSession {
	session, _ := d.sessions.Load(commandID)
	found, _ := session.(*downloadSession)
	return found
}

// downloadBody is a download as the route takes it.
type downloadBody struct {
	Target   string `json:"target"`
	Snapshot string `json:"snapshot"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
}

// handleCreateDownload saves an admin's download of a folder or a file of a
// snapshot and gives its link, good once for downloadTicketLife.
func (p *Panel) handleCreateDownload(e *core.RequestEvent) error {
	var body downloadBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}
	job, err := e.App.FindRecordById(schema.BackupJobs, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The job does not exist.", err)
	}
	if !fullSnapshotID.MatchString(body.Snapshot) {
		return e.BadRequestError("Choose a snapshot.", nil)
	}
	target, err := e.App.FindRecordById(schema.BackupTargets, body.Target)
	if err != nil {
		return e.NotFoundError("The target does not exist.", err)
	}
	if !p.jobUsedTarget(job, target) {
		return e.BadRequestError("The job never backed up to that target.", nil)
	}
	if target.GetString("status") == schema.TargetPending {
		return e.Error(http.StatusConflict, "The target is still being added: download from it once its check ends.", nil)
	}
	kind := body.Kind
	if kind == "" {
		kind = schema.DownloadDir
	}
	if kind != schema.DownloadDir && kind != schema.DownloadFile && kind != schema.DownloadSQL {
		return e.BadRequestError("Download a folder or a file.", nil)
	}
	snapshot, cached := p.cachedSnapshot(job.Id, target.Id, body.Snapshot)
	dir := body.Path
	if kind == schema.DownloadSQL {
		// a database job's one file, its dump (#53)
		if job.GetString("source_kind") != schema.SourceDatabase {
			return e.BadRequestError("Only a database's job downloads as SQL.", nil)
		}
		dir = "/" + databases.BackupFile(jobEngine(job), job.GetString("source"))
	}
	if dir == "" {
		dir = "/"
		if cached && len(snapshot.Paths) == 1 {
			dir = snapshot.Paths[0]
		}
	}
	if !snapshotPath(dir) || len(dir) > backup.MaxRestorePath || (kind == schema.DownloadFile && dir == "/") {
		return e.BadRequestError("Give an absolute path in the snapshot.", nil)
	}
	var host *core.Record
	var problem *targetProblem
	roots := []restoreRoot{}
	if kind == schema.DownloadSQL && jobEngine(job).Name == databases.Postgres.Name {
		// PostgreSQL's custom dump becomes SQL in a container of the
		// server's own image, on the server's host
		var container string
		host, container, problem = p.sqlConverter(job, target)
		roots = append(roots, restoreRoot{Container: container})
	} else {
		host, problem = p.listingHost(job, target, "sends the download")
	}
	if problem != nil {
		return e.Error(problem.status, problem.message, nil)
	}
	at := time.Now().UTC()
	if parsed, err := time.Parse(time.RFC3339, snapshot.Time); cached && err == nil {
		at = parsed.UTC()
	}
	name := path.Base(dir)
	switch kind {
	case schema.DownloadDir:
		name = fmt.Sprintf("%s-%s-%s.tar", job.GetString("name"), body.Snapshot[:8], at.Format("20060102-1504"))
	case schema.DownloadSQL:
		name = fmt.Sprintf("%s-%s.sql", job.GetString("source"), at.Format("20060102-1504"))
	}
	collection, err := e.App.FindCachedCollectionByNameOrId(schema.BackupRestores)
	if err != nil {
		return e.InternalServerError("", err)
	}
	requestedBy, requester := requester(e)
	restore := core.NewRecord(collection)
	restore.Load(map[string]any{
		"job": job.Id, "target": target.Id, "host": host.Id, "snapshot_id": body.Snapshot, "mode": schema.RestoreDownload,
		"status": schema.RestoreQueued, "path": dir, "download_name": fileName(name), "download_kind": kind,
		"requested_by": requestedBy, "requested_label": requester, "roots": roots,
	})
	if cached {
		restore.Set("snapshot_time", at)
	}
	restore.Set("label", restoreLabel(job, target, restore, host))
	if err := e.App.SaveWithContext(e.Request.Context(), restore); err != nil {
		return e.BadRequestError("The download could not be saved: "+saveErrorText(err), nil)
	}
	ticket, err := p.downloadTickets.issue(&downloadTicket{restore: restore.Id, userID: requestedBy, label: requester,
		actor: audit.ActorFromRequest(e), expires: time.Now().Add(downloadTicketLife)})
	if err != nil {
		return e.InternalServerError("", err)
	}
	return e.JSON(http.StatusCreated, map[string]any{"restore": restore, "url": "/api/pc/backup-downloads/" + restore.Id + "?ticket=" + ticket})
}

// sqlConverter is where a PostgreSQL job's dump becomes SQL (#53): the host
// of the job's database's server, whose agent makes a conversion container
// of the server container's image, the same pg_restore; and that container.
// A dropped database's server is found by its name.
func (p *Panel) sqlConverter(job, target *core.Record) (*core.Record, string, *targetProblem) {
	gone := &targetProblem{http.StatusConflict, "The database's server is gone: download the dump as stored."}
	var server *core.Record
	if database, err := p.App.FindRecordById(schema.Databases, job.GetString("database")); err == nil && job.GetString("database") != "" {
		server, _ = p.App.FindRecordById(schema.DBServers, database.GetString("server"))
	}
	if server == nil {
		found, err := p.App.FindFirstRecordByFilter(schema.DBServers, "host={:host} && name={:name} && kind={:shared}",
			dbx.Params{"host": job.GetString("host"), "name": job.GetString("db_server_name"), "shared": schema.DBServerShared})
		if err != nil {
			return nil, "", gone
		}
		server = found
	}
	if server.GetString("engine") != databases.Postgres.Name || server.GetString("container_ref") == "" {
		return nil, "", gone
	}
	host, err := p.App.FindRecordById(schema.Hosts, server.GetString("host"))
	if err != nil {
		return nil, "", gone
	}
	if !p.Commands.Connected(host.Id) {
		return nil, "", &targetProblem{http.StatusConflict, fmt.Sprintf("The server %s is offline: its agent sends the download.", host.GetString("name"))}
	}
	if !slices.Contains(host.GetStringSlice("capabilities"), backup.CapabilityDatabase) {
		return nil, "", &targetProblem{http.StatusConflict, fmt.Sprintf("The agent of %s cannot convert dumps to SQL yet: update it.", host.GetString("name"))}
	}
	if problem := localTargetElsewhere(p, target, host); problem != nil {
		return nil, "", &targetProblem{http.StatusConflict, problem.message}
	}
	return host, server.GetString("container_ref"), nil
}

// fileName keeps a name a browser saves a download under: no folders, no
// control characters (C0, DEL and C1, which the agent refuses as well), at
// most 255 bytes.
func fileName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return '-'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		name = "snapshot"
	}
	return cutText(name, 255)
}

// handleDownload redeems a download's ticket and streams the download as
// the agent sends it. Before the first byte a failure is answered 502 with
// the agent's words; after it, any failure aborts the response, so that the
// browser marks the download failed rather than keep a cut tar.
func (p *Panel) handleDownload(e *core.RequestEvent) error {
	ticket := p.downloadTickets.take(e.Request.URL.Query().Get("ticket"))
	if ticket == nil || ticket.restore != e.Request.PathValue("id") {
		return e.ForbiddenError(downloadUsed, nil)
	}
	restore, err := e.App.FindRecordById(schema.BackupRestores, ticket.restore)
	if err != nil || restore.GetString("mode") != schema.RestoreDownload || restore.GetString("status") != schema.RestoreQueued || restore.GetString("command") != "" {
		return e.ForbiddenError(downloadUsed, nil)
	}
	if !p.stillHasRole(ticket.userID, ticket.label, schema.RoleAdmin) {
		return e.ForbiddenError("Only admins download snapshots.", nil)
	}
	target, err := e.App.FindRecordById(schema.BackupTargets, restore.GetString("target"))
	if err != nil {
		return e.NotFoundError("The target does not exist.", err)
	}
	repository, err := p.repositoryOf(target)
	if err != nil {
		return downloadFailed(e, sentence(err))
	}

	id := core.GenerateDefaultRandomId()
	session := newDownloadSession(restore.Id)
	p.downloads.sessions.Store(id, session)
	defer session.serveOnce.Do(func() { close(session.served) })
	payload := dumpPayload(restore)
	payload.Repository = repository
	ctx := context.WithoutCancel(e.Request.Context())
	err = e.App.RunInTransaction(func(tx core.App) error {
		fresh, err := tx.FindRecordById(schema.BackupRestores, restore.Id)
		if err != nil {
			return err
		}
		if fresh.GetString("status") != schema.RestoreQueued || fresh.GetString("command") != "" {
			return errRestoreMoved
		}
		_, err = p.Commands.CreateIn(ctx, tx, commands.Options{
			ID: id, Host: restore.GetString("host"), Label: cutText("Download "+restore.GetString("label"), 255),
			Command:     &agentv1.Command{Payload: &agentv1.Command_BackupDump{BackupDump: payload}},
			RequestedBy: ticket.userID, Requester: ticket.label, QueueTimeout: downloadQueue,
		})
		if err != nil {
			return err
		}
		fresh.Set("command", id)
		return tx.SaveWithContext(ctx, fresh)
	})
	if err != nil {
		p.downloads.sessions.Delete(id)
		if errors.Is(err, errRestoreMoved) {
			return e.ForbiddenError(downloadUsed, nil)
		}
		return downloadFailed(e, "The download could not be started: "+saveErrorText(err))
	}
	actor := ticket.actor
	actor.IP = e.RealIP()
	actor.UserAgent = e.Request.UserAgent()
	p.Audit.Security(audit.ActionBackupDownload, actor, restore.GetString("label"), map[string]any{
		"restore": restore.Id, "job": restore.GetString("job"), "target": target.Id, "snapshot": restore.GetString("snapshot_id"),
		"path": restore.GetString("path"), "host": restore.GetString("host"),
	})
	return p.streamDownload(e, id, restore, session)
}

// downloadFailed answers a download that failed before its first byte, in
// words the hidden frame of the UI shows nobody, but a person's browser
// does.
func downloadFailed(e *core.RequestEvent, message string) error {
	e.Response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	e.Response.Header().Set("Cache-Control", "no-store")
	e.Response.WriteHeader(http.StatusBadGateway)
	_, err := io.WriteString(e.Response, message+"\n")
	return err
}

// readResult is a read of a download's body.
type readResult struct {
	n   int
	err error
}

// streamDownload waits for the agent's first bytes, then copies the
// download to the browser as it comes.
func (p *Panel) streamDownload(e *core.RequestEvent, id string, restore *core.Record, session *downloadSession) error {
	ctx := e.Request.Context()
	timer := time.NewTimer(p.downloadFirstByte())
	defer timer.Stop()
	stop := func(reason string) {
		session.left.Store(true)
		_ = p.Commands.Cancel(id, reason)
	}
	var body io.Reader
	select {
	case body = <-session.body:
	case <-session.finished:
		return downloadFailed(e, p.downloadError(id))
	case <-timer.C:
		_ = p.Commands.Cancel(id, "The download did not start in time.")
		return downloadFailed(e, "The agent did not start the download in time: try again.")
	case <-ctx.Done():
		stop("The browser stopped the download.")
		return nil
	}
	// the browser that leaves while the agent is silent ends the upload,
	// whose next chunk the copy waits for
	watch := make(chan struct{})
	defer close(watch)
	go func() {
		select {
		case <-ctx.Done():
			session.end(ctx.Err())
		case <-watch:
		}
	}()

	buffer := make([]byte, downloadBuffer)
	first := make(chan readResult, 1)
	go func() {
		n, err := body.Read(buffer)
		first <- readResult{n, err}
	}()
	var read readResult
	select {
	case read = <-first:
	case <-timer.C:
		session.end(errors.New("the download did not start in time"))
		_ = p.Commands.Cancel(id, "The download did not start in time.")
		return downloadFailed(e, "The agent did not start the download in time: try again.")
	case <-ctx.Done():
		session.end(ctx.Err())
		stop("The browser stopped the download.")
		return nil
	}
	if read.err != nil && !errors.Is(read.err, io.EOF) {
		session.end(read.err)
		// the agent says why, as it ends the command
		select {
		case <-session.finished:
		case <-time.After(10 * time.Second):
		}
		return downloadFailed(e, p.downloadError(id))
	}

	// the response has no deadline: a download may take hours
	_ = http.NewResponseController(e.Response).SetWriteDeadline(time.Time{})
	header := e.Response.Header()
	header.Set("Content-Type", "application/x-tar")
	switch restore.GetString("download_kind") {
	case schema.DownloadFile:
		header.Set("Content-Type", "application/octet-stream")
	case schema.DownloadSQL:
		header.Set("Content-Type", "application/sql; charset=utf-8")
	}
	header.Set("Content-Disposition", contentDisposition(restore.GetString("download_name")))
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Accel-Buffering", "no")
	e.Response.WriteHeader(http.StatusOK)

	// the first bytes go at once: the browser starts saving with them
	unflushed, flushed := 0, time.Time{}
	for {
		if read.n > 0 {
			if _, err := e.Response.Write(buffer[:read.n]); err != nil {
				// the browser left
				session.end(err)
				stop("The browser stopped the download.")
				return nil
			}
			unflushed += read.n
			if unflushed >= downloadFlushBytes || time.Since(flushed) >= downloadFlushEvery {
				if err := e.Flush(); err != nil {
					session.end(err)
					stop("The browser stopped the download.")
					return nil
				}
				unflushed, flushed = 0, time.Now()
			}
		}
		if errors.Is(read.err, io.EOF) {
			// the agent committed: restic ended well
			session.end(nil)
			return e.Flush()
		}
		if read.err != nil {
			session.end(read.err)
			if ctx.Err() != nil {
				stop("The browser stopped the download.")
				return nil
			}
			// after the first byte, a failure must not look like the end
			panic(http.ErrAbortHandler)
		}
		read.n, read.err = body.Read(buffer)
	}
}

// downloadFirstByte is how long a download waits for its first bytes.
func (p *Panel) downloadFirstByte() time.Duration {
	if p.DownloadFirstByte > 0 {
		return p.DownloadFirstByte
	}
	return downloadFirstByte
}

// downloadError is why a download's command failed, without the target's
// secrets.
func (p *Panel) downloadError(id string) string {
	command, err := p.App.FindRecordById(schema.Commands, id)
	if err != nil {
		return "The download failed."
	}
	message := strings.TrimPrefix(command.GetString("error"), "failed to prepare the command: ")
	if message == "" {
		return "The download failed."
	}
	if restore, err := p.App.FindRecordById(schema.BackupRestores, restoreCommandPayloadOf(command).RestoreID); err == nil {
		if target, err := p.App.FindRecordById(schema.BackupTargets, restore.GetString("target")); err == nil {
			message = backup.Redact(message, p.targetSecretValues(target)...)
		}
	}
	return "The download failed: " + lowerFirst(sentenceText(message))
}

// contentDisposition makes a download saved under its name, in ASCII for
// old browsers and in UTF-8 for the others.
func contentDisposition(name string) string {
	plain := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, plain, url.PathEscape(name))
}

// countingReader counts what was read.
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// serveDownload takes the agent's upload of a download and hands it to the
// browser's request that waits for it, then waits for the copy to end. The
// command must be a backup.dump under way whose record is a download.
func (p *Panel) serveDownload(ctx context.Context, command *core.Record, _ *agentv1.UploadArtifactHeader, body io.Reader) (uint64, error) {
	if command.GetString("type") != "backup.dump" || commandFinal(command) {
		return 0, errors.New("the command sends no download")
	}
	restore, err := p.App.FindRecordById(schema.BackupRestores, restoreCommandPayloadOf(command).RestoreID)
	if err != nil || restore.GetString("mode") != schema.RestoreDownload || restore.GetString("command") != command.Id {
		return 0, errors.New("the command sends no download")
	}
	session := p.downloads.get(command.Id)
	if session == nil {
		return 0, errors.New("nobody downloads this any more")
	}
	counted := &countingReader{r: body}
	select {
	case session.body <- counted:
	case <-session.served:
		return 0, errors.New("nobody downloads this any more")
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	select {
	case err := <-session.done:
		return uint64(counted.n.Load()), err //nolint:gosec // a count
	case <-ctx.Done():
		return uint64(counted.n.Load()), ctx.Err() //nolint:gosec // a count
	}
}

// downloadFinished keeps how a download ended, once, and tells the
// browser's request that waits for it.
func (p *Panel) downloadFinished(command *core.Record) {
	session := p.downloads.get(command.Id)
	if session != nil {
		session.finishOnce.Do(func() { close(session.finished) })
		// once its request is done, nothing waits for it any more
		go func() {
			<-session.served
			p.downloads.sessions.Delete(command.Id)
		}()
	}
	restore, err := p.App.FindRecordById(schema.BackupRestores, restoreCommandPayloadOf(command).RestoreID)
	if err != nil || restore.GetString("command") != command.Id {
		return
	}
	var result struct {
		Bytes int64 `json:"bytes"`
	}
	raw, _ := json.Marshal(command.Get("result"))
	_ = json.Unmarshal(raw, &result)
	status, message := schema.RestoreFailed, p.downloadError(command.Id)
	switch {
	case command.GetString("status") == schema.CommandSucceeded:
		status, message = schema.RestoreSucceeded, ""
	case session != nil && session.left.Load():
		status, message = schema.RestoreCancelled, "The browser stopped the download."
	case command.GetString("status") == schema.CommandCancelled:
		status, message = schema.RestoreCancelled, "Stopped: "+lowerFirst(sentenceText(command.GetString("error")))
	case command.GetString("status") == schema.CommandTimedOut && command.GetDateTime("dispatched_at").IsZero():
		why := "was busy"
		if !p.Commands.Connected(command.GetString("host")) || p.markedOffline(command.GetString("host")) {
			why = "stayed offline"
		}
		message = fmt.Sprintf("It waited %d minutes while the server %s %s, and never started.", int(downloadQueue.Minutes()), command.GetString("host_name"), why)
	}
	fields := map[string]any{"status": status, "error": cutText(message, 4096), "bytes": result.Bytes, "finished_at": command.GetDateTime("finished_at")}
	if started := command.GetDateTime("started_at"); !started.IsZero() {
		fields["started_at"] = started
	}
	kept := false
	err = p.update(context.Background(), restore, func(fresh *core.Record) error {
		if s := fresh.GetString("status"); s != schema.RestoreQueued && s != schema.RestoreRunning {
			return errUnchanged
		}
		for name, value := range fields {
			fresh.Set(name, value)
		}
		kept = true
		return nil
	})
	if err != nil {
		p.Log.Error("failed to keep a download", "restore", restore.Id, "error", err)
	}
	if kept && status == schema.RestoreSucceeded {
		events.Log(p.App, restore.GetString("host"), "backup.downloaded", schema.SeverityInfo,
			cutText(fmt.Sprintf("%s: %s downloaded %d bytes.", restore.GetString("label"), restore.GetString("requested_label"), result.Bytes), 2048),
			map[string]any{"restore": restore.Id, "job": restore.GetString("job"), "target": restore.GetString("target"), "command": command.Id})
	}
}
