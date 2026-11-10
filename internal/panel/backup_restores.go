package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// Restores (#30) put a job's snapshot back: to its original location on the
// job's server, where the containers that use it stop for the restore and
// start again, or into a new folder of any server; or they send a folder of
// it, or one file, to a browser. A backup_restores record is the plan, the
// preview, what lets the panel give the target's secrets to the server that
// restores, and the history. The agent runs restic in a job container and
// stops and starts the containers itself, inside one backup.restore; the
// panel never queues a stack command for a restore.
//
// Each command a record links is checked against the record as it is
// delivered: it is the record's own, it goes where the record says, from the
// record's target, and it is exactly what the record builds, as someone
// confirmed it. A command made by hand, or changed, gets no secret.

const (
	// restorePreviewGood is how long a preview is good to start from.
	restorePreviewGood = 30 * time.Minute
	// restorePreviewQueue is how long a preview waits for its server.
	restorePreviewQueue = 5 * time.Minute
	// maxRestoreIncludes is how many items of a snapshot a restore picks.
	maxRestoreIncludes = 100
)

func (p *Panel) registerBackupRestores() {
	p.snapshotCache.fresh = snapshotsFresh
	p.browseCache.fresh = browseFresh
	p.Commands.OnFinish("backup.snapshots", p.snapshotsListed)
	p.Commands.OnFinish("backup.browse", p.folderListed)
	p.Commands.OnFinish("backup.run", p.backupRunEnded)
	p.Commands.OnFinish("backup.restore", p.restoreFinished)
}

func (p *Panel) bindBackupRestoreRoutes(group *router.RouterGroup[*core.RequestEvent]) {
	everyone := requireRole(schema.RoleViewer, schema.RoleOperator, schema.RoleAdmin)
	operators := requireRole(schema.RoleOperator, schema.RoleAdmin)
	group.GET("/backup-jobs/{id}/snapshots", p.handleSnapshots).BindFunc(everyone)
	group.GET("/backup-jobs/{id}/snapshots/{snapshot}/files", p.handleSnapshotFiles).BindFunc(operators)
	group.POST("/backup-jobs/{id}/restores", p.handleCreateRestore).BindFunc(operators)
	group.DELETE("/backup-restores/{id}", p.handleDiscardRestore).BindFunc(operators)
}

// restoreBody is a restore as the route takes it.
type restoreBody struct {
	Target   string   `json:"target"`
	Snapshot string   `json:"snapshot"`
	Mode     string   `json:"mode"`
	Host     string   `json:"host"`
	Path     string   `json:"path"`
	Source   string   `json:"source"`
	Include  []string `json:"include"`
	Replace  bool     `json:"replace"`
}

// restorePlan is where a restore writes, as the route works it out.
type restorePlan struct {
	host   *core.Record
	roots  []restoreRoot
	stack  string
	path   string
	source string
}

// hasRole tells whether a request's user has at least a role; superusers
// have every role.
func hasRole(e *core.RequestEvent, role string) bool {
	return e.HasSuperuserAuth() || (e.Auth != nil && roleRank[e.Auth.GetString("role")] >= roleRank[role])
}

// handleCreateRestore makes a restore's draft and queues its preview, which
// says what the restore would change and which containers would stop. An
// operator restores to the original location on the job's server; an admin
// restores a folder of the snapshot to a new folder of any server.
func (p *Panel) handleCreateRestore(e *core.RequestEvent) error {
	var body restoreBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Invalid body.", err)
	}
	job, err := e.App.FindRecordById(schema.BackupJobs, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The job does not exist.", err)
	}
	switch body.Mode {
	case schema.RestoreOriginal:
	case schema.RestorePath:
		if !hasRole(e, schema.RoleAdmin) {
			return e.ForbiddenError("Only admins restore to a new folder.", nil)
		}
	default:
		return e.BadRequestError("Choose where the snapshot goes: its original location or a new folder.", nil)
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
		return e.Error(http.StatusConflict, "The target is still being added: restore from it once its check ends.", nil)
	}
	if problem := checkIncludes(body.Include); problem != "" {
		return e.BadRequestError(problem, nil)
	}
	if body.Replace && len(body.Include) > 0 {
		return e.BadRequestError("Replace works for the whole snapshot only.", nil)
	}
	var plan restorePlan
	var problem *targetProblem
	if body.Mode == schema.RestoreOriginal {
		plan, problem = p.originalPlan(job, target, body)
	} else {
		plan, problem = p.pathPlan(job, target, body)
	}
	if problem != nil {
		return e.Error(problem.status, problem.message, nil)
	}
	if problem := p.restoreBlocked(job, target, body.Mode, plan.host, destinationsOf(body.Mode, plan.roots, plan.path), ""); problem != nil {
		return e.Error(problem.status, problem.message, nil)
	}
	repository, err := p.repositoryOf(target)
	if err != nil {
		return e.BadRequestError(sentence(err), nil)
	}

	collection, err := e.App.FindCachedCollectionByNameOrId(schema.BackupRestores)
	if err != nil {
		return e.InternalServerError("", err)
	}
	restore := core.NewRecord(collection)
	restore.Id = core.GenerateDefaultRandomId()
	requestedBy, requester := requester(e)
	preview := core.GenerateDefaultRandomId()
	include := body.Include
	if include == nil {
		include = []string{}
	}
	restore.Load(map[string]any{
		"job": job.Id, "target": target.Id, "host": plan.host.Id, "snapshot_id": body.Snapshot, "mode": body.Mode,
		"roots": plan.roots, "path": plan.path, "source": plan.source, "include": include, "replace": body.Replace,
		"stack_name": plan.stack, "status": schema.RestorePreviewing, "expires_at": time.Now().UTC().Add(restorePreviewGood),
		"requested_by": requestedBy, "requested_label": requester, "preview_command": preview,
	})
	if snapshot, ok := p.cachedSnapshot(job.Id, target.Id, body.Snapshot); ok {
		if at, err := time.Parse(time.RFC3339, snapshot.Time); err == nil {
			restore.Set("snapshot_time", at.UTC())
		}
	}
	restore.Set("label", restoreLabel(job, target, restore, plan.host))
	payload := restorePayload(restore, true)
	payload.Repository = repository
	// a client that leaves does not leave a draft without its preview
	ctx := context.WithoutCancel(e.Request.Context())
	err = e.App.RunInTransaction(func(tx core.App) error {
		// the command is delivered once both are saved, and is the record's
		_, err := p.Commands.CreateIn(ctx, tx, commands.Options{
			ID: preview, Host: plan.host.Id, Label: cutText("Preview "+restore.GetString("label"), 255),
			Command:     &agentv1.Command{Payload: &agentv1.Command_BackupRestore{BackupRestore: payload}},
			RequestedBy: requestedBy, Requester: requester, QueueTimeout: restorePreviewQueue,
		})
		if err != nil {
			return err
		}
		return tx.SaveWithContext(ctx, restore)
	})
	if err != nil {
		return e.BadRequestError("The restore could not be saved: "+saveErrorText(err), nil)
	}
	return e.JSON(http.StatusAccepted, restore)
}

// checkIncludes checks the items of a snapshot a restore picks: at most
// maxRestoreIncludes, each by its absolute path in the snapshot.
func checkIncludes(include []string) string {
	if len(include) > maxRestoreIncludes {
		return fmt.Sprintf("Pick at most %d items, or a whole folder.", maxRestoreIncludes)
	}
	for _, item := range include {
		if !snapshotPath(item) || item == "/" {
			return "Pick each item by its absolute path in the snapshot."
		}
	}
	return ""
}

// originalPlan works out a restore to the original location: the job's
// server, and the job's data as it is now, a volume, a stack's volumes or a
// directory, with the Compose project whose containers use it.
func (p *Panel) originalPlan(job, target *core.Record, body restoreBody) (restorePlan, *targetProblem) {
	if job.GetString("source_kind") == schema.SourceDump {
		return restorePlan{}, &targetProblem{http.StatusBadRequest,
			"A database dump goes back into its database with the database's own restore: restore it to a folder, or download it."}
	}
	if body.Host != "" && body.Host != job.GetString("host") {
		return restorePlan{}, &targetProblem{http.StatusBadRequest, "A restore to the original location goes to the job's server."}
	}
	host, err := p.App.FindRecordById(schema.Hosts, job.GetString("host"))
	if err != nil {
		return restorePlan{}, &targetProblem{http.StatusConflict, "The server of the job is gone."}
	}
	if problem := localTargetElsewhere(p, target, host); problem != nil {
		return restorePlan{}, problem
	}
	plan := restorePlan{host: host}
	switch job.GetString("source_kind") {
	case schema.SourceVolume:
		volume := job.GetString("source")
		plan.roots = []restoreRoot{{Source: "/volumes/" + volume, Volume: volume}}
		plan.stack = p.volumeProject(host.Id, volume)
	case schema.SourceStack:
		volumes, err := p.stackVolumes(job)
		if err != nil {
			return restorePlan{}, &targetProblem{http.StatusBadRequest, sentence(err)}
		}
		for _, volume := range volumes {
			plan.roots = append(plan.roots, restoreRoot{Source: "/volumes/" + volume, Volume: volume})
		}
		if stack, err := p.App.FindRecordById(schema.Stacks, job.GetString("stack")); err == nil {
			plan.stack = stack.GetString("name")
		}
	case schema.SourcePath:
		dir := job.GetString("source")
		if err := backup.RestorePath(dir); err != nil {
			return restorePlan{}, &targetProblem{http.StatusBadRequest, sentence(err)}
		}
		plan.roots = []restoreRoot{{Path: dir}}
	default:
		return restorePlan{}, &targetProblem{http.StatusBadRequest, "The job backs up nothing that goes back to where it was."}
	}
	for _, item := range body.Include {
		if !slices.ContainsFunc(plan.roots, func(root restoreRoot) bool { return backup.Under(item, rootSource(root)) }) {
			return restorePlan{}, &targetProblem{http.StatusBadRequest, "Pick only items of what the job backs up."}
		}
	}
	return plan, nil
}

// rootSource is where a root is in the snapshot: its source, or for a
// directory whose source the preview finds, the directory, which the
// backup saved at its own path.
func rootSource(root restoreRoot) string {
	if root.Source != "" {
		return root.Source
	}
	return root.Path
}

// volumeProject is the Compose project of a server's volume, as the
// server's inventory has it.
func (p *Panel) volumeProject(hostID, name string) string {
	volume, err := p.App.FindFirstRecordByFilter(schema.Volumes, "host={:host} && name={:name}", dbx.Params{"host": hostID, "name": name})
	if err != nil {
		return ""
	}
	labels := map[string]string{}
	_ = volume.UnmarshalJSONField("labels", &labels)
	return labels[labelComposeProject]
}

// localTargetElsewhere refuses a local target's snapshots on another server
// than its own.
func localTargetElsewhere(p *Panel, target, host *core.Record) *targetProblem {
	if target.GetString("kind") != backup.KindLocal || target.GetString("host") == host.Id {
		return nil
	}
	name := target.GetString("host")
	if own, err := p.App.FindRecordById(schema.Hosts, name); err == nil {
		name = own.GetString("name")
	}
	return &targetProblem{http.StatusBadRequest, fmt.Sprintf("The target %s is a folder of %s: only that server restores from it.", target.GetString("name"), name)}
}

// pathPlan works out a restore to a new folder: the server, a folder a
// restore may write that is no local target's repository there, and the
// folder of the snapshot that goes into it.
func (p *Panel) pathPlan(job, target *core.Record, body restoreBody) (restorePlan, *targetProblem) {
	host, err := p.App.FindRecordById(schema.Hosts, body.Host)
	if err != nil {
		return restorePlan{}, &targetProblem{http.StatusBadRequest, "Choose the server the snapshot is restored on."}
	}
	if err := backup.RestorePath(body.Path); err != nil {
		return restorePlan{}, &targetProblem{http.StatusBadRequest, sentence(err)}
	}
	locals, err := p.App.FindRecordsByFilter(schema.BackupTargets, "kind={:kind} && host={:host}", "name", 0, 0,
		dbx.Params{"kind": backup.KindLocal, "host": host.Id})
	if err != nil {
		return restorePlan{}, &targetProblem{http.StatusInternalServerError, "The targets could not be read."}
	}
	for _, local := range locals {
		var config backup.Config
		if err := local.UnmarshalJSONField("config", &config); err == nil && config.Path != "" && backup.Overlap(body.Path, config.Path) {
			return restorePlan{}, &targetProblem{http.StatusBadRequest,
				fmt.Sprintf("%s is the repository of the target %s: restore elsewhere.", config.Path, local.GetString("name"))}
		}
	}
	if problem := localTargetElsewhere(p, target, host); problem != nil {
		return restorePlan{}, problem
	}
	source := body.Source
	if source == "" {
		source = "/"
		if snapshot, ok := p.cachedSnapshot(job.Id, target.Id, body.Snapshot); ok && len(snapshot.Paths) == 1 {
			source = snapshot.Paths[0]
		}
	}
	if !snapshotPath(source) || utf8.RuneCountInString(source) > backup.MaxRestorePath {
		return restorePlan{}, &targetProblem{http.StatusBadRequest, "Give the folder of the snapshot by its absolute path."}
	}
	for _, item := range body.Include {
		if !backup.Under(item, source) {
			return restorePlan{}, &targetProblem{http.StatusBadRequest, "Pick only items in the folder of the snapshot that is restored."}
		}
	}
	return restorePlan{host: host, path: body.Path, source: source}, nil
}

// destination is what a restore writes on its server: a volume or a
// folder.
type destination struct {
	volume string
	path   string
}

func (d destination) String() string {
	if d.volume != "" {
		return d.volume
	}
	return d.path
}

// overlaps tells whether two destinations share data.
func (d destination) overlaps(other destination) bool {
	if d.volume != "" || other.volume != "" {
		return d.volume == other.volume
	}
	return backup.Overlap(d.path, other.path)
}

// destinationsOf is what a restore of a mode writes.
func destinationsOf(mode string, roots []restoreRoot, dir string) []destination {
	switch mode {
	case schema.RestoreOriginal:
		out := make([]destination, 0, len(roots))
		for _, root := range roots {
			out = append(out, destination{volume: root.Volume, path: root.Path})
		}
		return out
	case schema.RestorePath:
		return []destination{{path: dir}}
	}
	return nil
}

// restoreDestinations is what a record's restore writes.
func restoreDestinations(restore *core.Record) []destination {
	return destinationsOf(restore.GetString("mode"), rootsOf(restore), restore.GetString("path"))
}

// restoreBlocked says why a restore cannot be queued now, if it cannot: its
// server is offline, a backup of the job is running, for a restore of the
// job's own data, or another restore into the same data is under way.
func (p *Panel) restoreBlocked(job, target *core.Record, mode string, host *core.Record, dests []destination, self string) *targetProblem {
	if !p.Commands.Connected(host.Id) {
		return &targetProblem{http.StatusConflict, fmt.Sprintf("The server %s is offline: its agent restores the snapshot.", host.GetString("name"))}
	}
	if mode == schema.RestoreOriginal {
		targets := p.jobTargets(job)
		if !slices.ContainsFunc(targets, func(t *core.Record) bool { return t.Id == target.Id }) {
			targets = append(targets, target)
		}
		for _, t := range targets {
			if p.runGoing(job, t) {
				return &targetProblem{http.StatusConflict, fmt.Sprintf("A backup of %s is running: restore when it ends.", job.GetString("name"))}
			}
		}
	}
	if into := p.restoreUnderWay(host.Id, dests, self); into != "" {
		return &targetProblem{http.StatusConflict, fmt.Sprintf("A restore into %s is already under way.", into)}
	}
	return nil
}

// openRestores are a server's restores that were started and have not
// ended: queued or running, downloads aside.
func (p *Panel) openRestores(hostID string) []*core.Record {
	records, err := p.App.FindRecordsByFilter(schema.BackupRestores,
		"host={:host} && mode!={:download} && (status={:queued} || status={:running})", "created", 0, 0,
		dbx.Params{"host": hostID, "download": schema.RestoreDownload, "queued": schema.RestoreQueued, "running": schema.RestoreRunning})
	if err != nil {
		p.Log.Error("failed to read the restores", "host", hostID, "error", err)
		return nil
	}
	return records
}

// restoreUnderWay is what another started restore of a server writes that
// one of dests shares, if any.
func (p *Panel) restoreUnderWay(hostID string, dests []destination, self string) string {
	for _, other := range p.openRestores(hostID) {
		if other.Id == self {
			continue
		}
		for _, theirs := range restoreDestinations(other) {
			for _, ours := range dests {
				if theirs.overlaps(ours) {
					return ours.String()
				}
			}
		}
	}
	return ""
}

// restoreLabel names a restore: the job, the target, the snapshot and where
// it goes, as "vaultwarden ← B2 · 3f9a1c2e → original location".
func restoreLabel(job, target, restore, host *core.Record) string {
	where := "original location"
	switch restore.GetString("mode") {
	case schema.RestorePath:
		where = host.GetString("name") + ":" + restore.GetString("path")
	case schema.RestoreDownload:
		where = "download"
	}
	short := restore.GetString("snapshot_id")
	if len(short) > 8 {
		short = short[:8]
	}
	return cutText(fmt.Sprintf("%s ← %s · %s → %s", job.GetString("name"), target.GetString("name"), short, where), 255)
}

// handleDiscardRestore removes a draft: a restore whose preview is under
// way, ready or too old, or failed, or a download that never started. A
// preview under way is cancelled.
func (p *Panel) handleDiscardRestore(e *core.RequestEvent) error {
	restore, err := e.App.FindRecordById(schema.BackupRestores, e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("The restore does not exist.", err)
	}
	if !hasRole(e, restoreRole(restore.GetString("mode"))) {
		return e.ForbiddenError("You are not allowed to do this.", nil)
	}
	if !restoreDraft(restore) {
		return e.Error(http.StatusConflict, "Only a draft is discarded: this restore was started.", nil)
	}
	if restore.GetString("status") == schema.RestorePreviewing && restore.GetString("preview_command") != "" {
		// its finisher finds no record, and removes the command
		_ = p.Commands.Cancel(restore.GetString("preview_command"), "The draft was discarded.")
	}
	if err := e.App.Delete(restore); err != nil {
		return e.BadRequestError("The draft could not be removed: "+err.Error(), nil)
	}
	return e.NoContent(http.StatusNoContent)
}

// restoreDraft tells whether a record is a draft, which nothing started: a
// preview under way, ready, too old or failed, or a download whose link was
// never used.
func restoreDraft(restore *core.Record) bool {
	switch restore.GetString("status") {
	case schema.RestorePreviewing, schema.RestorePreviewed, schema.RestoreExpired:
		return true
	case schema.RestoreFailed:
		return restore.GetString("mode") != schema.RestoreDownload && restore.GetString("command") == ""
	case schema.RestoreQueued:
		return restore.GetString("mode") == schema.RestoreDownload && restore.GetString("command") == ""
	}
	return false
}

// restoreCommandPayload is what the finishers read of a restore's or a
// download's command.
type restoreCommandPayload struct {
	RestoreID string `json:"restore_id"`
	DryRun    bool   `json:"dry_run"`
}

func restoreCommandPayloadOf(command *core.Record) restoreCommandPayload {
	var payload restoreCommandPayload
	raw, _ := json.Marshal(command.Get("payload"))
	_ = json.Unmarshal(raw, &payload)
	return payload
}

// restoreFinished keeps how a restore's preview, or the restore, ended.
func (p *Panel) restoreFinished(command *core.Record) {
	payload := restoreCommandPayloadOf(command)
	if payload.DryRun {
		// a preview is read once, and its command does not stay
		defer p.removeCommandLater(command.Id)
	}
	restore, err := p.App.FindRecordById(schema.BackupRestores, payload.RestoreID)
	if err != nil {
		return
	}
	if payload.DryRun {
		p.previewFinished(restore, command)
	}
}

// previewFinished keeps a preview on its draft, which is good to start
// from for restorePreviewGood, with the target's secrets hidden; or the
// preview's failure.
func (p *Panel) previewFinished(restore, command *core.Record) {
	hide := []string{}
	if target, err := p.App.FindRecordById(schema.BackupTargets, restore.GetString("target")); err == nil {
		hide = p.targetSecretValues(target)
	}
	fields := map[string]any{}
	switch command.GetString("status") {
	case schema.CommandSucceeded:
		raw, _ := json.Marshal(command.Get("result"))
		redacted := backup.Redact(string(raw), hide...)
		preview := map[string]any{}
		if err := json.Unmarshal([]byte(redacted), &preview); err != nil || len(redacted) > 128<<10 {
			fields["status"] = schema.RestoreFailed
			fields["error"] = "The preview was too large to keep: pick fewer items."
			break
		}
		now := time.Now().UTC()
		fields["status"] = schema.RestorePreviewed
		fields["preview"] = preview
		fields["preview_at"] = now
		fields["expires_at"] = now.Add(restorePreviewGood)
		fields["error"] = ""
		if restore.GetDateTime("snapshot_time").IsZero() {
			if snapshot, ok := preview["snapshot"].(map[string]any); ok {
				if at, err := time.Parse(time.RFC3339, fmt.Sprint(snapshot["time"])); err == nil {
					fields["snapshot_time"] = at.UTC()
				}
			}
		}
	case schema.CommandTimedOut:
		fields["status"] = schema.RestoreFailed
		fields["error"] = "The preview did not finish in time: " + lowerFirst(command.GetString("error"))
	default:
		message := strings.TrimPrefix(command.GetString("error"), "failed to prepare the command: ")
		if message == "" {
			message = "the preview failed"
		}
		fields["status"] = schema.RestoreFailed
		fields["error"] = cutText(sentenceText(backup.Redact(message, hide...)), 4096)
	}
	err := p.update(context.Background(), restore, func(fresh *core.Record) error {
		if fresh.GetString("status") != schema.RestorePreviewing || fresh.GetString("preview_command") != command.Id {
			return errUnchanged
		}
		for name, value := range fields {
			fresh.Set(name, value)
		}
		return nil
	})
	if err != nil {
		p.Log.Error("failed to keep a restore's preview", "restore", restore.Id, "error", err)
	}
}

// lowerFirst makes a sentence the end of another one.
func lowerFirst(text string) string {
	if text == "" {
		return text
	}
	return strings.ToLower(text[:1]) + text[1:]
}

// restoreRoot is a root of a restore to the original location, as a record
// keeps it: a path of the snapshot and the volume or directory of the server
// it goes back into.
type restoreRoot struct {
	Source string `json:"source"`
	Volume string `json:"volume,omitempty"`
	Path   string `json:"path,omitempty"`
}

// restorePreview is what the panel reads of an agent's preview.
type restorePreview struct {
	Snapshot struct {
		ID    string   `json:"id"`
		Time  string   `json:"time"`
		Paths []string `json:"paths"`
	} `json:"snapshot"`
	Roots    []previewRoot `json:"roots"`
	NotInJob []string      `json:"not_in_job"`
	Counts   struct {
		New       int64 `json:"new"`
		Updated   int64 `json:"updated"`
		Unchanged int64 `json:"unchanged"`
		Deleted   int64 `json:"deleted"`
	} `json:"counts"`
	WouldStop  []stoppedContainer `json:"would_stop"`
	TotalFiles int64              `json:"total_files"`
	TotalBytes int64              `json:"total_bytes"`
}

// previewRoot is a root of a preview: what of the snapshot goes where, and
// what the destination holds.
type previewRoot struct {
	Index       int    `json:"index"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	InSnapshot  bool   `json:"in_snapshot"`
	Exists      bool   `json:"exists"`
	Entries     int64  `json:"entries"`
}

// stoppedContainer is a container a restore stops, as a preview names it.
type stoppedContainer struct {
	Name    string `json:"name"`
	Project string `json:"project"`
}

// rootsOf are a record's roots of the original location.
func rootsOf(restore *core.Record) []restoreRoot {
	roots := []restoreRoot{}
	_ = restore.UnmarshalJSONField("roots", &roots)
	return roots
}

// includeOf are the snapshot paths a record restores, if not all of it.
func includeOf(restore *core.Record) []string {
	include := []string{}
	_ = restore.UnmarshalJSONField("include", &include)
	return include
}

// previewOf is a record's preview; ok is false while it has none.
func previewOf(restore *core.Record) (restorePreview, bool) {
	var preview restorePreview
	raw := restore.GetString("preview")
	if raw == "" || raw == "null" {
		return preview, false
	}
	return preview, restore.UnmarshalJSONField("preview", &preview) == nil
}

// confirmedStops are the names of the containers a record's preview said
// would stop: a real restore that would stop another one is refused.
func confirmedStops(restore *core.Record) []string {
	preview, _ := previewOf(restore)
	names := make([]string, 0, len(preview.WouldStop))
	for _, container := range preview.WouldStop {
		names = append(names, container.Name)
	}
	return names
}

// restorePayload is the one builder of a record's backup.restore, the
// preview (dryRun) and the restore, without its repository: the route that
// queues it adds the target and its place, and the enricher compares what it
// delivers with what this builds from the record as it is then.
func restorePayload(restore *core.Record, dryRun bool) *agentv1.BackupRestore {
	payload := &agentv1.BackupRestore{
		SnapshotId: restore.GetString("snapshot_id"),
		RestoreId:  restore.Id,
		JobTag:     "job:" + restore.GetString("job"),
		Include:    includeOf(restore),
		Replace:    restore.GetBool("replace"),
		DryRun:     dryRun,
	}
	switch restore.GetString("mode") {
	case schema.RestoreOriginal:
		for _, root := range rootsOf(restore) {
			payload.Original = append(payload.Original, &agentv1.BackupRestoreRoot{Source: root.Source, Volume: root.Volume, Path: root.Path})
		}
		payload.Stack = restore.GetString("stack_name")
	case schema.RestorePath:
		payload.TargetPath = restore.GetString("path")
		payload.Source = restore.GetString("source")
	}
	if !dryRun {
		payload.Overwrite = restore.GetBool("overwrite")
		payload.ConfirmedStops = confirmedStops(restore)
	}
	return payload
}

// dumpPayload is a download's backup.dump, without its repository.
func dumpPayload(restore *core.Record) *agentv1.BackupDump {
	return &agentv1.BackupDump{
		SnapshotId: restore.GetString("snapshot_id"),
		Path:       restore.GetString("path"),
		RestoreId:  restore.Id,
		FileName:   restore.GetString("download_name"),
		JobTag:     "job:" + restore.GetString("job"),
	}
}

// restorePayloadMatches tells whether a command is what its record builds,
// compared as messages: proto3's JSON leaves out what is false or empty.
func restorePayloadMatches(command *agentv1.Command, restore *core.Record) bool {
	switch payload := command.GetPayload().(type) {
	case *agentv1.Command_BackupRestore:
		got, ok := proto.Clone(payload.BackupRestore).(*agentv1.BackupRestore)
		if !ok {
			return false
		}
		got.Repository = nil
		return proto.Equal(got, restorePayload(restore, got.GetDryRun()))
	case *agentv1.Command_BackupDump:
		got, ok := proto.Clone(payload.BackupDump).(*agentv1.BackupDump)
		if !ok {
			return false
		}
		got.Repository = nil
		return proto.Equal(got, dumpPayload(restore))
	}
	return false
}

// restoreRole is the role a record's mode needs: an operator restores to the
// original location, an admin to a new folder or to a browser.
func restoreRole(mode string) string {
	if mode == schema.RestoreOriginal {
		return schema.RoleOperator
	}
	return schema.RoleAdmin
}

// roleRank orders the roles.
var roleRank = map[string]int{schema.RoleViewer: 1, schema.RoleOperator: 2, schema.RoleAdmin: 3}

// stillHasRole tells whether whoever asked for something still has a role:
// a user as they are now, or a superuser, whose label says so.
func (p *Panel) stillHasRole(userID, label, role string) bool {
	if userID == "" {
		return strings.HasPrefix(label, "superuser ")
	}
	user, err := p.App.FindRecordById(schema.Users, userID)
	return err == nil && roleRank[user.GetString("role")] >= roleRank[role]
}

// checkRestoreDelivery checks a backup.restore or a backup.dump as it is
// delivered, before it gets the target's secrets: its record is still
// there and links this very command, as its preview or as itself; the
// record names the command's target and server; the target is not being
// added; a local target is read on its own server only; the command is
// what the record builds; and an overwrite was confirmed. On its first
// delivery the record waits for it, and whoever asked for it, or started
// it, still has the role its mode needs. A command sent again after its
// agent reconnected reached the agent with the secrets already and may be
// running: it keeps them, as a backup run does (see checkRunDelivery).
func (p *Panel) checkRestoreDelivery(record *core.Record, command *agentv1.Command, target *core.Record) error {
	restoreID, dryRun, download := "", false, false
	switch payload := command.GetPayload().(type) {
	case *agentv1.Command_BackupRestore:
		restoreID, dryRun = payload.BackupRestore.GetRestoreId(), payload.BackupRestore.GetDryRun()
	case *agentv1.Command_BackupDump:
		restoreID, download = payload.BackupDump.GetRestoreId(), true
	}
	restore, err := p.App.FindRecordById(schema.BackupRestores, restoreID)
	if err != nil {
		return errors.New("the restore is gone")
	}
	own := restore.GetString("command")
	if dryRun {
		own = restore.GetString("preview_command")
	}
	mode := restore.GetString("mode")
	if own == "" || own != record.Id || download != (mode == schema.RestoreDownload) {
		// a command someone made by hand, as a superuser may
		return errors.New("the command is not the restore's")
	}
	switch {
	case restore.GetString("target") != target.Id:
		return errors.New("the restore is from another target")
	case restore.GetString("host") != record.GetString("host"):
		return errors.New("the restore goes to another server")
	case target.GetString("status") == schema.TargetPending:
		return errors.New("the target is still being added")
	case target.GetString("kind") == backup.KindLocal && target.GetString("host") != record.GetString("host"):
		return errors.New("a local target is a directory of its own server: only that server restores from it")
	case !restorePayloadMatches(command, restore):
		return errors.New("the command differs from the restore that was confirmed")
	case command.GetBackupRestore().GetOverwrite() && restore.GetString("confirmed_by") == "" && restore.GetString("confirmed_label") == "":
		return errors.New("the overwrite was not confirmed")
	}
	if record.GetString("status") != schema.CommandQueued {
		return nil
	}
	want := schema.RestoreQueued
	if dryRun {
		want = schema.RestorePreviewing
	}
	if restore.GetString("status") != want {
		return errors.New("the restore is not waiting for this command")
	}
	userID, label := restore.GetString("requested_by"), restore.GetString("requested_label")
	if !dryRun && !download {
		userID, label = restore.GetString("confirmed_by"), restore.GetString("confirmed_label")
	}
	if role := restoreRole(mode); !p.stillHasRole(userID, label, role) {
		what := "restore"
		if download {
			what = "download"
		}
		return errors.New("only an " + role + "'s " + what + " gets the target's secrets")
	}
	return nil
}

// checkListDelivery checks a listing of a target's snapshots or of a
// snapshot's folder: it goes to the target's server, or to the server of
// the job whose snapshots it lists, when the job backs up to the target and
// the target is remote. A listing never adds a server that holds the
// target's secrets: only a restore someone authorized does (acceptance
// criterion 2), and a local target is read on its own server only.
func (p *Panel) checkListDelivery(record *core.Record, command *agentv1.Command, target *core.Record) error {
	host := record.GetString("host")
	if target.GetString("host") == host {
		return nil
	}
	refused := errors.New("the backup target is reached from another server")
	tag := command.GetBackupBrowse().GetJobTag()
	if tags := command.GetBackupSnapshots().GetTags(); len(tags) > 0 {
		tag = ""
		if i := slices.IndexFunc(tags, func(tag string) bool { return strings.HasPrefix(tag, "job:") }); i >= 0 {
			tag = tags[i]
		}
	}
	jobID, ok := strings.CutPrefix(tag, "job:")
	if !ok || jobID == "" || target.GetString("kind") == backup.KindLocal {
		return refused
	}
	job, err := p.App.FindRecordById(schema.BackupJobs, jobID)
	if err != nil || job.GetString("host") != host || !backsUpTo(job, target) {
		return refused
	}
	return nil
}
