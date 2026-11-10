package panel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pocketbase/pocketbase/core"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/audit"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// downloadFixture is a job fixture with a served panel and an agent.
type downloadFixture struct {
	*jobFixture
	server *httptest.Server
	stream *commands.Stream
	job    *core.Record
}

func newDownloadFixture(t *testing.T) *downloadFixture {
	t.Helper()
	f := newJobFixture(t)
	server := httptest.NewServer(f.handler)
	t.Cleanup(func() {
		// a download still waiting for its agent ends with its browser
		server.CloseClientConnections()
		server.Close()
	})
	return &downloadFixture{jobFixture: f, server: server, stream: f.agent(f.host),
		job: f.job("data", map[string]any{"source_kind": schema.SourceVolume, "source": "app_data"})}
}

// link asks for a download and returns its record's id and its link.
func (f *downloadFixture) link(body map[string]any) (string, string) {
	f.t.Helper()
	all := map[string]any{"target": f.target.Id, "snapshot": snapshotID, "path": "/volumes/app_data"}
	for key, value := range body {
		all[key] = value
	}
	status, answer := f.send(http.MethodPost, "/api/pc/backup-jobs/"+f.job.Id+"/downloads", f.admin, all)
	if status != http.StatusCreated {
		f.t.Fatalf("asked for a download: %d %v", status, answer)
	}
	restore, _ := answer["restore"].(map[string]any)
	url, _ := answer["url"].(string)
	return restore["id"].(string), url
}

// browsed is what a browser got of a download: the answer's head, and its
// body, which the test closes when it ends.
type browsed struct {
	StatusCode    int
	Header        http.Header
	ContentLength int64
	Body          io.ReadCloser
}

// browse starts a browser's download; its answer comes once the first
// bytes do.
func (f *downloadFixture) browse(ctx context.Context, url string) <-chan *browsed {
	answer := make(chan *browsed, 1)
	go func() {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+url, nil)
		if err != nil {
			f.t.Error(err)
			answer <- nil
			return
		}
		response, err := http.DefaultClient.Do(request) //nolint:bodyclose // the test closes it as it ends
		if err != nil {
			answer <- nil
			return
		}
		answer <- &browsed{StatusCode: response.StatusCode, Header: response.Header, ContentLength: response.ContentLength, Body: response.Body}
	}()
	return answer
}

func (f *downloadFixture) response(answer <-chan *browsed) *browsed {
	f.t.Helper()
	select {
	case response := <-answer:
		if response == nil {
			f.t.Fatal("the download failed to answer")
		}
		f.t.Cleanup(func() { _ = response.Body.Close() })
		return response
	case <-time.After(15 * time.Second):
		f.t.Fatal("the download did not answer")
		return nil
	}
}

// upload is the agent's live upload of a download.
type upload struct {
	writer *io.PipeWriter
	ended  chan error
	sent   chan uint64
}

// started has the agent take the download's command, start it and open its
// upload.
func (f *downloadFixture) started() (*agentv1.Command, *upload) {
	f.t.Helper()
	command := f.received(f.stream)
	f.report(f.host, command.GetId(), 1, agentv1.CommandState_COMMAND_STATE_RUNNING, nil, nil, "")
	reader, writer := io.Pipe()
	up := &upload{writer: writer, ended: make(chan error, 1), sent: make(chan uint64, 1)}
	record := f.stored(schema.Commands, command.GetId())
	go func() {
		n, err := f.p.serveDownload(context.Background(), record, &agentv1.UploadArtifactHeader{CommandId: command.GetId(), Kind: artifactBackupDownload}, reader)
		_ = reader.CloseWithError(errors.New("the upload ended"))
		up.sent <- n
		up.ended <- err
	}()
	return command, up
}

func (u *upload) result(t *testing.T) (uint64, error) {
	t.Helper()
	select {
	case n := <-u.sent:
		return n, <-u.ended
	case <-time.After(15 * time.Second):
		t.Fatal("the upload did not end")
		return 0, nil
	}
}

// TestADownloadStreamsTheTarToTheTicketsHolder (#30, S): an admin's link
// queues restic's dump only once a browser follows it; the agent's upload
// goes on to the browser as it comes, saved under the download's name, with
// no length and nothing kept; the record says what was sent, and the
// download is a security event.
func TestADownloadStreamsTheTarToTheTicketsHolder(t *testing.T) {
	f := newDownloadFixture(t)
	id, url := f.link(nil)
	restore := f.stored(schema.BackupRestores, id)
	if restore.GetString("status") != schema.RestoreQueued || restore.GetString("mode") != schema.RestoreDownload ||
		restore.GetString("download_kind") != schema.DownloadDir || restore.GetString("command") != "" ||
		!strings.HasPrefix(restore.GetString("download_name"), "data-3f9a1c2e-") || !strings.HasSuffix(restore.GetString("download_name"), ".tar") {
		t.Fatalf("the download: %v", restore.FieldsData())
	}
	if !f.quiet(f.stream) {
		t.Fatal("restic ran before a browser followed the link")
	}

	answer := f.browse(context.Background(), url)
	command, up := f.started()
	dump := command.GetBackupDump()
	if dump.GetRestoreId() != id || dump.GetPath() != "/volumes/app_data" || dump.GetJobTag() != "job:"+f.job.Id ||
		dump.GetFileName() != restore.GetString("download_name") || dump.GetRepository().GetPassword() != "repo-password-b2" {
		t.Errorf("the dump: %v", command)
	}
	if running := f.stored(schema.BackupRestores, id); running.GetString("status") != schema.RestoreRunning || running.GetString("command") != command.GetId() {
		t.Errorf("running: %v", running.FieldsData())
	}
	archive := bytes.Repeat([]byte("tar archive block "), 100000)
	if _, err := up.writer.Write(archive[:1000]); err != nil {
		t.Fatal(err)
	}
	response := f.response(answer)
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/x-tar" ||
		response.Header.Get("Content-Disposition") != `attachment; filename="`+restore.GetString("download_name")+`"; filename*=UTF-8''`+restore.GetString("download_name") ||
		response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Content-Type-Options") != "nosniff" ||
		response.Header.Get("X-Accel-Buffering") != "no" || response.ContentLength != -1 {
		t.Errorf("the answer: %d %v", response.StatusCode, response.Header)
	}
	go func() {
		_, _ = up.writer.Write(archive[1000:])
		_ = up.writer.Close()
	}()
	got, err := io.ReadAll(response.Body)
	if err != nil || !bytes.Equal(got, archive) {
		t.Errorf("downloaded %d bytes of %d: %v", len(got), len(archive), err)
	}
	if n, err := up.result(t); err != nil || n != uint64(len(archive)) {
		t.Errorf("the upload: %d %v", n, err)
	}
	f.answer(f.host, command.GetId(), agentv1.CommandState_COMMAND_STATE_SUCCEEDED, map[string]any{"bytes": len(archive), "file_name": dump.GetFileName()}, "")
	done := f.stored(schema.BackupRestores, id)
	if done.GetString("status") != schema.RestoreSucceeded || done.GetInt("bytes") != len(archive) {
		t.Errorf("done: %v", done.FieldsData())
	}
	entries, _ := f.app.FindRecordsByFilter(schema.AuditLog, "action={:action}", "", 0, 0, map[string]any{"action": audit.ActionBackupDownload})
	if len(entries) != 1 || entries[0].GetString("actor_label") != "admin@example.com" {
		t.Errorf("audited: %v", entries)
	}
	if downloaded, _ := f.app.FindRecordsByFilter(schema.Events, "kind='backup.downloaded'", "", 0, 0); len(downloaded) != 1 {
		t.Errorf("%d downloaded events", len(downloaded))
	}
}

// agentTakesName is the agent's rule for the name a browser saves a
// download under (internal/agent's checkDownloadName): valid UTF-8 of at
// most 255 bytes, not empty, "." or "..", with no slash and no control
// character.
func agentTakesName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 255 && utf8.ValidString(name) &&
		!strings.ContainsRune(name, '/') && strings.IndexFunc(name, unicode.IsControl) < 0
}

// TestDownloadNamesAreNamesTheAgentTakes (#30): the panel never stores a
// download's name that the agent refuses, which would fail the download
// at once, every time: control characters, C1 ones (U+0080 to U+009F)
// included, and folders become "-".
func TestDownloadNamesAreNamesTheAgentTakes(t *testing.T) {
	for name, want := range map[string]string{
		"report\u0085.pdf":   "report-.pdf",
		"a\u0080b\u009fc":    "a-b-c",
		"tab\tnew\nline\x7f": "tab-new-line-",
		`back\slash`:         "back-slash",
		"zpráva – 1.pdf":     "zpráva – 1.pdf",
		"":                   "snapshot",
		"..":                 "snapshot",
	} {
		got := fileName(name)
		if got != want || !agentTakesName(got) {
			t.Errorf("%q: %q, which the agent takes: %v", name, got, agentTakesName(got))
		}
	}
	if long := fileName(strings.Repeat("é", 200) + "\u0085"); !agentTakesName(long) {
		t.Errorf("a long name: %q", long)
	}

	// a file of a snapshot whose name holds one, asked for by the route
	f := newDownloadFixture(t)
	id, _ := f.link(map[string]any{"path": "/volumes/app_data/report\u0085.pdf", "kind": "file"})
	if name := f.stored(schema.BackupRestores, id).GetString("download_name"); name != "report-.pdf" {
		t.Errorf("the name: %q", name)
	}
}

// TestDumpJobSnapshotDownloadsAsAFile (#30): one file of a snapshot, as a
// dump job's, downloads as it is, under its own name.
func TestDumpJobSnapshotDownloadsAsAFile(t *testing.T) {
	f := newDownloadFixture(t)
	id, url := f.link(map[string]any{"path": "/shop-db.pgdump", "kind": "file"})
	if name := f.stored(schema.BackupRestores, id).GetString("download_name"); name != "shop-db.pgdump" {
		t.Errorf("the name: %s", name)
	}
	answer := f.browse(context.Background(), url)
	command, up := f.started()
	go func() {
		_, _ = up.writer.Write([]byte("PGDMP"))
		_ = up.writer.Close()
	}()
	response := f.response(answer)
	got, _ := io.ReadAll(response.Body)
	if response.Header.Get("Content-Type") != "application/octet-stream" || string(got) != "PGDMP" ||
		!strings.Contains(response.Header.Get("Content-Disposition"), `filename="shop-db.pgdump"`) || command.GetBackupDump().GetPath() != "/shop-db.pgdump" {
		t.Errorf("a file: %v %q", response.Header, got)
	}
	if _, err := up.result(t); err != nil {
		t.Errorf("the upload of a file: %v", err)
	}
}

// TestADownloadTicketWorksOnce (#30, SAFE): a download's link starts one
// download, for its own record, within its time, while its admin is one.
func TestADownloadTicketWorksOnce(t *testing.T) {
	f := newDownloadFixture(t)
	// a link taken that should not be waits for its agent only briefly
	f.p.DownloadFirstByte = 3 * time.Second
	get := func(url string) (int, string) {
		t.Helper()
		response, err := http.Get(f.server.URL + url)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(body)
	}
	used := `"message":"` + downloadUsed + `"`

	first, firstURL := f.link(nil)
	_, secondURL := f.link(nil)
	// the second's ticket for the first's record
	ticket := secondURL[strings.Index(secondURL, "?"):]
	if status, body := get("/api/pc/backup-downloads/" + first + ticket); status != http.StatusForbidden || !strings.Contains(body, used) {
		t.Errorf("another record's ticket: %d %s", status, body)
	}
	if status, body := get("/api/pc/backup-downloads/" + first + "?ticket=" + strings.Repeat("ab", 32)); status != http.StatusForbidden || !strings.Contains(body, used) {
		t.Errorf("a wrong ticket: %d %s", status, body)
	}
	answer := f.browse(context.Background(), firstURL)
	_, up := f.started()
	if status, body := get(firstURL); status != http.StatusForbidden || !strings.Contains(body, used) {
		t.Errorf("a used ticket: %d %s", status, body)
	}
	go func() {
		_, _ = up.writer.Write([]byte("x"))
		_ = up.writer.Close()
	}()
	f.response(answer)
	if _, err := up.result(t); err != nil {
		t.Errorf("the upload of the first link: %v", err)
	}

	// one that is too old
	_, oldURL := f.link(nil)
	f.p.downloadTickets.mu.Lock()
	for _, ticket := range f.p.downloadTickets.tickets {
		ticket.expires = time.Now().Add(-time.Second)
	}
	f.p.downloadTickets.mu.Unlock()
	if status, body := get(oldURL); status != http.StatusForbidden || !strings.Contains(body, used) {
		t.Errorf("an old ticket: %d %s", status, body)
	}
	// one whose admin is one no more
	_, demotedURL := f.link(nil)
	admin, err := f.app.FindAuthRecordByEmail(schema.Users, "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	admin.Set("role", schema.RoleOperator)
	if err := f.app.Save(admin); err != nil {
		t.Fatal(err)
	}
	if status, body := get(demotedURL); status != http.StatusForbidden || !strings.Contains(body, "Only admins download snapshots.") {
		t.Errorf("a demoted admin's ticket: %d %s", status, body)
	}
	// the refused ticket was used all the same
	admin.Set("role", schema.RoleAdmin)
	if err := f.app.Save(admin); err != nil {
		t.Fatal(err)
	}
	if status, body := get(demotedURL); status != http.StatusForbidden || !strings.Contains(body, used) {
		t.Errorf("a refused ticket again: %d %s", status, body)
	}
	if !f.quiet(f.stream) {
		t.Error("a refused link started a download")
	}
}

// TestAFailedDownloadAbortsTheResponse (#30, SAFE): a download that fails
// before its first byte is answered with why, without the target's
// secrets; one that fails after it is cut off, so that the browser marks
// it failed rather than keep a cut tar.
func TestAFailedDownloadAbortsTheResponse(t *testing.T) {
	f := newDownloadFixture(t)
	_, url := f.link(nil)
	answer := f.browse(context.Background(), url)
	command, up := f.started()
	_ = up.writer.CloseWithError(errors.New("the job failed"))
	f.answer(f.host, command.GetId(), agentv1.CommandState_COMMAND_STATE_FAILED, nil, "restic: wrong password the-secret-key")
	response := f.response(answer)
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadGateway || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/plain") ||
		string(body) != "The download failed: restic: wrong password ***.\n" {
		t.Errorf("before the first byte: %d %v %q", response.StatusCode, response.Header, body)
	}
	if _, err := up.result(t); err == nil {
		t.Error("the upload that failed was taken")
	}

	id, url := f.link(nil)
	answer = f.browse(context.Background(), url)
	command, up = f.started()
	if _, err := up.writer.Write([]byte("the start of a tar")); err != nil {
		t.Fatal(err)
	}
	response = f.response(answer)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the first bytes: %d", response.StatusCode)
	}
	_ = up.writer.CloseWithError(errors.New("restic died"))
	got, err := io.ReadAll(response.Body)
	if err == nil {
		t.Errorf("a cut download read as whole: %q", got)
	}
	if _, err := up.result(t); err == nil {
		t.Error("the upload did not hear of the failure")
	}
	f.answer(f.host, command.GetId(), agentv1.CommandState_COMMAND_STATE_FAILED, nil, "restic died")
	if failed := f.stored(schema.BackupRestores, id); failed.GetString("status") != schema.RestoreFailed ||
		failed.GetString("error") != "The download failed: restic died." {
		t.Errorf("the record: %v", failed.FieldsData())
	}

	// an agent that never sends anything
	f.p.DownloadFirstByte = 300 * time.Millisecond
	_, url = f.link(nil)
	answer = f.browse(context.Background(), url)
	f.received(f.stream)
	response = f.response(answer)
	body, _ = io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadGateway || string(body) != "The agent did not start the download in time: try again.\n" {
		t.Errorf("no first byte: %d %q", response.StatusCode, body)
	}
}

// TestADownloadStopsWhenTheBrowserLeaves (#30): a browser that leaves
// stops the upload and the agent's job, and the record says the browser
// stopped it.
func TestADownloadStopsWhenTheBrowserLeaves(t *testing.T) {
	f := newDownloadFixture(t)
	id, url := f.link(nil)
	ctx, leave := context.WithCancel(context.Background())
	defer leave()
	answer := f.browse(ctx, url)
	command, up := f.started()
	if _, err := up.writer.Write([]byte("the start of a tar")); err != nil {
		t.Fatal(err)
	}
	response := f.response(answer)
	buffer := make([]byte, 4)
	if _, err := io.ReadFull(response.Body, buffer); err != nil {
		t.Fatal(err)
	}
	leave()
	if _, err := up.result(t); err == nil {
		t.Error("the upload went on without its browser")
	}
	select {
	case message := <-f.stream.Out():
		if message.GetCancel().GetCommandId() != command.GetId() {
			t.Errorf("the agent got %v", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the agent was not told to stop")
	}
	f.answer(f.host, command.GetId(), agentv1.CommandState_COMMAND_STATE_CANCELLED, nil, "the upload was aborted")
	if stopped := f.stored(schema.BackupRestores, id); stopped.GetString("status") != schema.RestoreCancelled ||
		stopped.GetString("error") != "The browser stopped the download." {
		t.Errorf("the record: %v", stopped.FieldsData())
	}
}

// TestALiveDownloadNeedsItsOwnRecord (#30, SAFE): an agent's live upload is
// passed on only for a backup.dump under way that a download's record
// links, and only while a browser waits for it.
func TestALiveDownloadNeedsItsOwnRecord(t *testing.T) {
	f := newDownloadFixture(t)
	serve := func(command *core.Record) error {
		t.Helper()
		_, err := f.p.serveDownload(context.Background(), command, &agentv1.UploadArtifactHeader{CommandId: command.Id, Kind: artifactBackupDownload},
			strings.NewReader("data"))
		return err
	}
	restore := f.restore(f.job, map[string]any{"status": schema.RestoreRunning, "overwrite": true, "confirmed_label": "superuser root@example.com"})
	f.p.Commands.Disconnect(f.stream)
	real := f.jobFixture.link(restore, false)
	if err := serve(real); err == nil || err.Error() != "the command sends no download" {
		t.Errorf("a restore's command: %v", err)
	}
	download := f.restore(f.job, map[string]any{"mode": schema.RestoreDownload, "status": schema.RestoreQueued, "roots": nil, "path": "/",
		"download_name": "data.tar"})
	dump := f.jobFixture.link(download, false)
	other := f.restore(f.job, map[string]any{"mode": schema.RestoreDownload, "status": schema.RestoreQueued, "roots": nil, "path": "/"})
	unlinked := f.command(f.host.Id, &agentv1.Command{Payload: &agentv1.Command_BackupDump{BackupDump: &agentv1.BackupDump{
		Repository: &agentv1.BackupRepository{TargetId: f.target.Id}, RestoreId: other.Id, SnapshotId: snapshotID, Path: "/"}}})
	if err := serve(unlinked); err == nil || err.Error() != "the command sends no download" {
		t.Errorf("a dump its record does not link: %v", err)
	}
	if err := serve(dump); err == nil || err.Error() != "nobody downloads this any more" {
		t.Errorf("a dump nobody waits for: %v", err)
	}
	f.ended(dump, schema.CommandCancelled, "", nil)
	if err := serve(f.stored(schema.Commands, dump.Id)); err == nil || err.Error() != "the command sends no download" {
		t.Errorf("a dump that ended: %v", err)
	}
}

// TestADownloadIsAnAdminsAndChecked (#30): only an admin asks for a
// download, of a path of the snapshot, from a server that holds the
// target's secrets; a link nobody used, or a download a panel restart cut,
// fails.
func TestADownloadIsAnAdminsAndChecked(t *testing.T) {
	f := newDownloadFixture(t)
	_, operator := testutil.NewUser(t, f.app, "operator@example.com", schema.RoleOperator)
	post := func(token string, body map[string]any) (int, any) {
		t.Helper()
		all := map[string]any{"target": f.target.Id, "snapshot": snapshotID}
		for key, value := range body {
			all[key] = value
		}
		status, answer := f.send(http.MethodPost, "/api/pc/backup-jobs/"+f.job.Id+"/downloads", token, all)
		return status, answer["message"]
	}
	if status, _ := post(operator, nil); status != http.StatusForbidden {
		t.Errorf("an operator: %d", status)
	}
	for body, want := range map[string]map[string]any{
		"Choose a snapshot.":                     {"snapshot": "3f9a1c2e"},
		"Give an absolute path in the snapshot.": {"path": "volumes"},
		"Download a folder or a file.":           {"kind": "zip"},
	} {
		if status, message := post(f.admin, want); status != http.StatusBadRequest || message != body {
			t.Errorf("%v: %d %v", want, status, message)
		}
	}
	if status, message := post(f.admin, map[string]any{"path": "/", "kind": "file"}); status != http.StatusBadRequest ||
		message != "Give an absolute path in the snapshot." {
		t.Errorf("the root as a file: %d %v", status, message)
	}
	// the whole snapshot by default
	status, answer := f.send(http.MethodPost, "/api/pc/backup-jobs/"+f.job.Id+"/downloads", f.admin,
		map[string]any{"target": f.target.Id, "snapshot": snapshotID})
	if restore, _ := answer["restore"].(map[string]any); status != http.StatusCreated || restore["path"] != "/" {
		t.Errorf("by default: %d %v", status, answer)
	}

	// healed: a link nobody used, a download whose request a restart cut
	unused, _ := f.link(nil)
	if _, err := f.app.DB().NewQuery("UPDATE backup_restores SET created='2020-01-01 00:00:00.000Z' WHERE id={:id}").
		Bind(map[string]any{"id": unused}).Execute(); err != nil {
		t.Fatal(err)
	}
	cut, url := f.link(nil)
	answer2 := f.browse(context.Background(), url)
	command, up := f.started()
	_, _ = up.writer.Write([]byte("x"))
	f.response(answer2)
	// as after a restart: the browser's request is gone
	f.p.downloads.sessions.Delete(command.GetId())
	f.p.settleRestores()
	if stored := f.stored(schema.BackupRestores, unused); stored.GetString("status") != schema.RestoreFailed ||
		stored.GetString("error") != "The download link was not used in time: start the download again." {
		t.Errorf("an unused link: %v", stored.FieldsData())
	}
	if stored := f.stored(schema.BackupRestores, cut); stored.GetString("status") != schema.RestoreFailed ||
		stored.GetString("error") != "The panel restarted during the download." {
		t.Errorf("a cut download: %v", stored.FieldsData())
	}
	_ = up.writer.Close()
}
