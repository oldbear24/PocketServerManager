package panel

import (
	"errors"
	"slices"
	"strings"

	"github.com/pocketbase/pocketbase/core"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
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
