package panel

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// The agent stops the containers that use a restore's destination and
// starts them again (#30); while a restore to the original location of a
// stack is queued or running, the panel holds the stack, as it holds one
// whose databases move: what would deploy it, change it or start its
// containers waits. A scheduled backup of data being restored is skipped,
// as it would feed half a restore to the retention, and one by hand is
// refused; a job, a target or a server is not removed while a restore of
// theirs is open.

// restoreHolding is the restore that holds a stack, if any: one to the
// original location of the stack's Compose project on its server, queued or
// running.
func (p *Panel) restoreHolding(stack *core.Record) *core.Record {
	return p.restoreHoldingProject(stack.GetString("host"), stack.GetString("name"))
}

func (p *Panel) restoreHoldingProject(hostID, project string) *core.Record {
	if project == "" {
		return nil
	}
	records, err := p.App.FindRecordsByFilter(schema.BackupRestores,
		"host={:host} && stack_name={:name} && mode={:original} && (status={:queued} || status={:running})", "created", 1, 0,
		dbx.Params{"host": hostID, "name": project, "original": schema.RestoreOriginal, "queued": schema.RestoreQueued, "running": schema.RestoreRunning})
	if err != nil || len(records) == 0 {
		return nil
	}
	return records[0]
}

// restoreHoldMessage says that a stack waits for its restore.
func restoreHoldMessage(name string) string {
	return fmt.Sprintf("The stack %s is being restored from a backup: this waits until that is done.", name)
}

// guardRestoringStack refuses, while a restore holds a stack, the API's
// commands of the stack and the starts, restarts and removals of its
// containers; and of any container a started restore stops, whichever
// stack it is in.
func (p *Panel) guardRestoringStack(e *core.RecordRequestEvent) error {
	commandType := e.Record.GetString("type")
	host := e.Record.GetString("host")
	var payload struct {
		DockerID string `json:"docker_id"`
		Name     string `json:"name"`
		Project  string `json:"project"`
	}
	if err := e.Record.UnmarshalJSONField("payload", &payload); err != nil {
		return e.Next()
	}
	switch commandType {
	case "container.start", "container.restart", "container.remove":
		container, err := p.App.FindFirstRecordByFilter(schema.Containers, "host={:host} && docker_id={:id}", dbx.Params{"host": host, "id": payload.DockerID})
		if err != nil {
			return e.Next()
		}
		if project := container.GetString("project"); p.restoreHoldingProject(host, project) != nil {
			return e.Error(http.StatusConflict, restoreHoldMessage(project), nil)
		}
		if p.restoreStops(host, container.GetString("name")) {
			return e.Error(http.StatusConflict, "A restore is writing this container's data: it starts again when the restore ends.", nil)
		}
	default:
		if !strings.HasPrefix(commandType, "stack.") {
			return e.Next()
		}
		project := payload.Name
		if project == "" {
			project = payload.Project
		}
		if p.restoreHoldingProject(host, project) != nil {
			return e.Error(http.StatusConflict, restoreHoldMessage(project), nil)
		}
	}
	return e.Next()
}

// restoreStops tells whether a started restore of a server stops a
// container, as its preview said.
func (p *Panel) restoreStops(hostID, name string) bool {
	for _, restore := range p.openRestores(hostID) {
		if slices.Contains(confirmedStops(restore), name) {
			return true
		}
	}
	return false
}

// restoreWriting is the started restore of a job's server that writes what
// the job backs up, if any: the job's own to its original location, or
// another into the same volume, the same stack's, or a folder that is, holds
// or lies in the job's directory.
func (p *Panel) restoreWriting(job *core.Record) *core.Record {
	restores := p.openRestores(job.GetString("host"))
	if len(restores) == 0 {
		return nil
	}
	backedUp := []destination{}
	project := ""
	switch job.GetString("source_kind") {
	case schema.SourceVolume:
		backedUp = append(backedUp, destination{volume: job.GetString("source")})
	case schema.SourcePath:
		backedUp = append(backedUp, destination{path: job.GetString("source")})
	case schema.SourceStack, schema.SourceDump:
		if stack, err := p.App.FindRecordById(schema.Stacks, job.GetString("stack")); err == nil {
			project = stack.GetString("name")
		}
		if job.GetString("source_kind") == schema.SourceStack {
			if volumes, err := p.stackVolumes(job); err == nil {
				for _, volume := range volumes {
					backedUp = append(backedUp, destination{volume: volume})
				}
			}
		}
	}
	for _, restore := range restores {
		if restore.GetString("mode") == schema.RestoreOriginal &&
			(restore.GetString("job") == job.Id || (project != "" && restore.GetString("stack_name") == project)) {
			return restore
		}
		for _, theirs := range restoreDestinations(restore) {
			for _, ours := range backedUp {
				if theirs.overlaps(ours) {
					return restore
				}
			}
		}
	}
	return nil
}

// restoreOpenFor is a restore that is queued or running and that a filter
// names, such as a job's or a target's, downloads included.
func (p *Panel) restoreOpenFor(filter string, params dbx.Params) bool {
	params["queued"] = schema.RestoreQueued
	params["running"] = schema.RestoreRunning
	records, err := p.App.FindRecordsByFilter(schema.BackupRestores, "("+filter+") && (status={:queued} || status={:running})", "", 1, 0, params)
	return err != nil || len(records) > 0
}

// restoreUnderWayMessage refuses to remove what a restore needs.
const restoreUnderWayMessage = "A restore from it is under way."
