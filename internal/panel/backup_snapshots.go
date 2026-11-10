package panel

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/commands"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// Listings (#30) read a job's snapshots in a target, and a folder of a
// snapshot, with restic on the agent of a server that holds the target's
// secrets: the target's own, or for a remote target the job's, when the
// target's is offline. One listing per job and target, or per folder, goes
// to an agent at a time: a request waits up to listingWait for it, and then
// answers that it is pending, as the next request joins the same listing.
// What a listing gave is kept in memory: a job's snapshots for
// snapshotsFresh, unless a backup of it ends, and a folder's page for
// browseFresh, as snapshots never change. Commands are readable by viewers,
// so a listing's command is removed once the panel read it.

const (
	// snapshotsFresh is how long a job's snapshots in a target are kept.
	snapshotsFresh = 10 * time.Minute
	// browseFresh is how long a page of a snapshot's folder is kept.
	browseFresh = 6 * time.Hour
	// listingFailedFor is how long a listing that failed is answered as it
	// failed, so that a request that waited no more still learns of it.
	listingFailedFor = 30 * time.Second
	// listingWait is how long a request waits for a listing.
	listingWait = 25 * time.Second
	// listingQueue is how long a listing waits for its server.
	listingQueue = 2 * time.Minute
	// listingCacheBytes is about how much of the listings is kept, of each
	// kind.
	listingCacheBytes = 32 << 20
	// snapshotLimit is how many of a job's newest snapshots are listed.
	snapshotLimit = 500
	// browseLimit is the most entries of a folder a page lists.
	browseLimit = 1000
	// maxSnapshotPath is the longest path of a snapshot a listing names.
	maxSnapshotPath = 4096
)

// fullSnapshotID is a snapshot's full id.
var fullSnapshotID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// listing is what a listing gave: a value to answer, or a problem.
type listing struct {
	value   map[string]any
	problem *targetProblem
	at      time.Time
	size    int
}

// listingFlight is a listing under way, which requests wait for, and the
// generation of its key it started in.
type listingFlight struct {
	key        string
	generation uint64
	done       chan struct{}
	result     listing
}

// listingCache keeps what listings of one kind gave, the least recently
// used going first past listingCacheBytes, and the listings under way, by
// their key and by their command. A key's generation counts how often what
// is kept for it was forgotten: a listing that started in an older one may
// have read what is no longer so.
type listingCache struct {
	fresh time.Duration

	mu          sync.Mutex
	entries     map[string]*list.Element
	order       *list.List
	bytes       int
	flights     map[string]*listingFlight
	commands    map[string]*listingFlight
	generations map[string]uint64
}

type listingEntry struct {
	key     string
	listing listing
}

func (c *listingCache) init() {
	if c.entries == nil {
		c.entries = map[string]*list.Element{}
		c.order = list.New()
		c.flights = map[string]*listingFlight{}
		c.commands = map[string]*listingFlight{}
		c.generations = map[string]uint64{}
	}
}

// cached is a listing kept for a key that is still good: one that gave
// its value within the cache's time, or failed within listingFailedFor.
// The caller holds the lock.
func (c *listingCache) cached(key string) (listing, bool) {
	c.init()
	element, ok := c.entries[key]
	if !ok {
		return listing{}, false
	}
	entry, _ := element.Value.(*listingEntry)
	good := c.fresh
	if entry.listing.problem != nil {
		good = listingFailedFor
	}
	if time.Since(entry.listing.at) >= good {
		c.drop(element)
		return listing{}, false
	}
	c.order.MoveToFront(element)
	return entry.listing, true
}

// peek is a value kept for a key, if it is still good.
func (c *listingCache) peek(key string) (map[string]any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	found, ok := c.cached(key)
	return found.value, ok && found.problem == nil
}

func (c *listingCache) drop(element *list.Element) {
	entry, _ := element.Value.(*listingEntry)
	c.order.Remove(element)
	delete(c.entries, entry.key)
	c.bytes -= entry.listing.size
}

// forget drops what is kept for a key, and the listing under way, which
// may have read what is no longer so: its requests still get its answer,
// but it is not kept, and the next request starts another listing.
func (c *listingCache) forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	c.generations[key]++
	if element, ok := c.entries[key]; ok {
		c.drop(element)
	}
	delete(c.flights, key)
}

// get answers a key from the cache unless refresh is set, or waits up to
// wait for a listing of it: the one under way, or one start queues with the
// command id it is given. pending says the listing goes on after wait.
func (c *listingCache) get(ctx context.Context, key string, refresh bool, wait time.Duration, start func(commandID string) *targetProblem) (found listing, cached, pending bool) {
	c.mu.Lock()
	if !refresh {
		if kept, ok := c.cached(key); ok {
			c.mu.Unlock()
			return kept, true, false
		}
	}
	c.init()
	flight := c.flights[key]
	if flight == nil {
		id := core.GenerateDefaultRandomId()
		flight = &listingFlight{key: key, generation: c.generations[key], done: make(chan struct{})}
		c.flights[key] = flight
		c.commands[id] = flight
		c.mu.Unlock()
		if problem := start(id); problem != nil {
			c.finish(id, key, listing{problem: problem})
		}
	} else {
		c.mu.Unlock()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-flight.done:
		return flight.result, false, false
	case <-timer.C:
	case <-ctx.Done():
	}
	return listing{}, false, true
}

// finish keeps what the listing of a command gave for key, and tells the
// requests that wait for it. A listing whose request the panel forgot, as
// over a restart, is kept all the same, unless what was kept for its key
// was forgotten since the panel started; one that started before its key
// was forgotten only answers its requests.
func (c *listingCache) finish(commandID, key string, result listing) {
	result.at = time.Now()
	if result.size == 0 {
		raw, _ := json.Marshal(result.value)
		result.size = len(raw) + len(key)
	}
	c.mu.Lock()
	c.init()
	flight := c.commands[commandID]
	delete(c.commands, commandID)
	if flight != nil && c.flights[flight.key] == flight {
		delete(c.flights, flight.key)
	}
	started := uint64(0)
	if flight != nil {
		started = flight.generation
	}
	if started == c.generations[key] && result.size <= listingCacheBytes {
		if element, ok := c.entries[key]; ok {
			c.drop(element)
		}
		c.entries[key] = c.order.PushFront(&listingEntry{key: key, listing: result})
		c.bytes += result.size
		for c.bytes > listingCacheBytes {
			c.drop(c.order.Back())
		}
	}
	c.mu.Unlock()
	if flight != nil {
		flight.result = result
		close(flight.done)
	}
}

// jobAndTarget reads the job of a route and the target its query names,
// which the job backs up to, or once did: a target taken out of a job keeps
// the job's snapshots.
func (p *Panel) jobAndTarget(e *core.RequestEvent, targetID string) (*core.Record, *core.Record, error) {
	job, err := e.App.FindRecordById(schema.BackupJobs, e.Request.PathValue("id"))
	if err != nil {
		return nil, nil, e.NotFoundError("The job does not exist.", err)
	}
	target, err := e.App.FindRecordById(schema.BackupTargets, targetID)
	if err != nil {
		return nil, nil, e.NotFoundError("The target does not exist.", err)
	}
	if !p.jobUsedTarget(job, target) {
		return nil, nil, e.BadRequestError("The job never backed up to that target.", nil)
	}
	return job, target, nil
}

// jobUsedTarget tells whether a job backs up to a target, or once did.
func (p *Panel) jobUsedTarget(job, target *core.Record) bool {
	if slices.Contains(job.GetStringSlice("targets"), target.Id) {
		return true
	}
	runs, err := p.App.FindRecordsByFilter(schema.BackupRuns, "job={:job} && target={:target}", "", 1, 0,
		dbx.Params{"job": job.Id, "target": target.Id})
	return err == nil && len(runs) > 0
}

// listingHost is the server whose agent reads a target for a job: a local
// target's own; for a remote one, the target's server if it is connected,
// or else the job's, if the job backs up to the target and its server is
// connected. does says what the agent would do, for the answer that none
// can.
func (p *Panel) listingHost(job, target *core.Record, does string) (*core.Record, *targetProblem) {
	targetHost, err := p.App.FindRecordById(schema.Hosts, target.GetString("host"))
	if err != nil {
		return nil, &targetProblem{http.StatusConflict, "The server of the target is gone."}
	}
	if p.Commands.Connected(targetHost.Id) {
		return targetHost, nil
	}
	if target.GetString("kind") != backup.KindLocal && backsUpTo(job, target) && job.GetString("host") != targetHost.Id {
		if jobHost, err := p.App.FindRecordById(schema.Hosts, job.GetString("host")); err == nil {
			if p.Commands.Connected(jobHost.Id) {
				return jobHost, nil
			}
			return nil, &targetProblem{http.StatusConflict, fmt.Sprintf("Neither %s nor %s is online: one of their agents %s.",
				targetHost.GetString("name"), jobHost.GetString("name"), does)}
		}
	}
	return nil, &targetProblem{http.StatusConflict, fmt.Sprintf("The server %s is offline: its agent %s.", targetHost.GetString("name"), does)}
}

// repositoryOf is a target's repository as a stored command names it: the
// target and its place, never a secret.
func (p *Panel) repositoryOf(target *core.Record) (*agentv1.BackupRepository, error) {
	secrets, err := p.targetSecretsOf(target)
	if err != nil {
		p.secretsMissing(target, err)
		return nil, err
	}
	located, err := backup.Locate(secrets.kind, secrets.config, secrets.creds)
	if err != nil {
		return nil, err
	}
	return &agentv1.BackupRepository{TargetId: target.Id, Url: located.URL}, nil
}

// listingWait is how long a request waits for a listing.
func (p *Panel) listingWait() time.Duration {
	if p.ListingWait > 0 {
		return p.ListingWait
	}
	return listingWait
}

// snapshotsKey is the key of a job's snapshots in a target.
func snapshotsKey(jobID, targetID string) string { return jobID + "|" + targetID }

// handleSnapshots lists a job's snapshots in a target, newest first.
func (p *Panel) handleSnapshots(e *core.RequestEvent) error {
	query := e.Request.URL.Query()
	job, target, err := p.jobAndTarget(e, query.Get("target"))
	if err != nil {
		return err
	}
	host, problem := p.listingHost(job, target, "lists the snapshots")
	if problem != nil {
		return e.Error(problem.status, problem.message, nil)
	}
	found, cached, pending := p.snapshotCache.get(e.Request.Context(), snapshotsKey(job.Id, target.Id), query.Get("refresh") == "1", p.listingWait(),
		func(id string) *targetProblem {
			return p.queueListing(e, id, host, target, snapshotsCommand(job, target))
		})
	return answerListing(e, found, cached, pending)
}

// answerListing answers what a listing gave.
func answerListing(e *core.RequestEvent, found listing, cached, pending bool) error {
	switch {
	case pending:
		return e.JSON(http.StatusAccepted, map[string]any{"pending": true})
	case found.problem != nil:
		return e.Error(found.problem.status, found.problem.message, nil)
	}
	answer := make(map[string]any, len(found.value)+1)
	for key, value := range found.value {
		answer[key] = value
	}
	if _, ok := answer["snapshots"]; ok {
		answer["cached"] = cached
	}
	return e.JSON(http.StatusOK, answer)
}

// snapshotsCommand lists a job's snapshots in a target.
func snapshotsCommand(job, target *core.Record) *agentv1.Command {
	return &agentv1.Command{Payload: &agentv1.Command_BackupSnapshots{BackupSnapshots: &agentv1.BackupSnapshots{
		Repository: &agentv1.BackupRepository{TargetId: target.Id},
		Tags:       []string{"pocketcloud", "job:" + job.Id},
		Limit:      snapshotLimit,
	}}}
}

// queueListing queues a listing with the id it was given, on the server
// that reads the target, for listingQueue.
func (p *Panel) queueListing(e *core.RequestEvent, id string, host, target *core.Record, command *agentv1.Command) *targetProblem {
	repository, err := p.repositoryOf(target)
	if err != nil {
		return &targetProblem{http.StatusBadRequest, sentence(err)}
	}
	label := "List the snapshots of " + target.GetString("name")
	switch payload := command.GetPayload().(type) {
	case *agentv1.Command_BackupSnapshots:
		payload.BackupSnapshots.Repository = repository
	case *agentv1.Command_BackupBrowse:
		payload.BackupBrowse.Repository = repository
		label = "List " + payload.BackupBrowse.GetPath() + " of a snapshot in " + target.GetString("name")
	}
	requestedBy, requester := requester(e)
	// a client that leaves does not leave the listing without its command
	_, err = p.Commands.Create(context.WithoutCancel(e.Request.Context()), commands.Options{
		ID: id, Host: host.Id, Command: command, Label: cutText(label, 255),
		RequestedBy: requestedBy, Requester: requester, QueueTimeout: listingQueue,
	})
	if err != nil {
		return &targetProblem{http.StatusInternalServerError, "The listing could not be queued: " + sentence(err)}
	}
	return nil
}

// listingProblem is what a listing that did not succeed answers: the
// agent's error, with the target's secrets hidden, or that it did not
// answer in time.
func (p *Panel) listingProblem(command, target *core.Record) *targetProblem {
	if command.GetString("status") == schema.CommandTimedOut {
		return &targetProblem{http.StatusGatewayTimeout,
			fmt.Sprintf("The agent of %s did not answer in time: restic may still be reading the repository. Try again.", command.GetString("host_name"))}
	}
	message := strings.TrimPrefix(command.GetString("error"), "failed to prepare the command: ")
	if message == "" {
		message = "the listing failed"
	}
	if target != nil {
		message = backup.Redact(message, p.targetSecretValues(target)...)
	}
	return &targetProblem{http.StatusBadGateway, sentenceText(message)}
}

// listingPayload is what the finishers read of a listing's payload.
type listingPayload struct {
	Repository struct {
		TargetID string `json:"target_id"`
	} `json:"repository"`
	Tags       []string `json:"tags"`
	SnapshotID string   `json:"snapshot_id"`
	Path       string   `json:"path"`
	Offset     int      `json:"offset"`
	Limit      int      `json:"limit"`
	Match      string   `json:"match"`
	JobTag     string   `json:"job_tag"`
}

// jobID is the job a listing names by its tag.
func (l listingPayload) jobID() string {
	for _, tag := range append([]string{l.JobTag}, l.Tags...) {
		if id, ok := strings.CutPrefix(tag, "job:"); ok {
			return id
		}
	}
	return ""
}

func listingPayloadOf(command *core.Record) listingPayload {
	var payload listingPayload
	raw, _ := json.Marshal(command.Get("payload"))
	_ = json.Unmarshal(raw, &payload)
	return payload
}

// snapshotsListed keeps a job's snapshots as a listing gave them, joined
// with the runs that made them, and removes the listing's command.
func (p *Panel) snapshotsListed(command *core.Record) {
	defer p.removeCommandLater(command.Id)
	payload := listingPayloadOf(command)
	target, _ := p.App.FindRecordById(schema.BackupTargets, payload.Repository.TargetID)
	result := listing{}
	if command.GetString("status") != schema.CommandSucceeded {
		result.problem = p.listingProblem(command, target)
	} else {
		result.value = p.snapshotsAnswer(command, payload.jobID(), payload.Repository.TargetID)
	}
	p.snapshotCache.finish(command.Id, snapshotsKey(payload.jobID(), payload.Repository.TargetID), result)
}

// agentSnapshot is a snapshot as the agent lists it.
type agentSnapshot struct {
	ID         string   `json:"id"`
	ShortID    string   `json:"short_id"`
	Time       string   `json:"time"`
	Hostname   string   `json:"hostname"`
	Paths      []string `json:"paths"`
	SizeBytes  int64    `json:"size_bytes"`
	AddedBytes int64    `json:"added_bytes"`
	Files      int64    `json:"files"`
}

// snapshotsAnswer is the answer of a job's snapshots in a target: each
// with the run that made it, whose sizes stand in for those the agent did
// not give. Runs go after sixty days; snapshots may stay longer.
func (p *Panel) snapshotsAnswer(command *core.Record, jobID, targetID string) map[string]any {
	var result struct {
		Snapshots []agentSnapshot `json:"snapshots"`
		Total     int             `json:"total"`
	}
	raw, _ := json.Marshal(command.Get("result"))
	_ = json.Unmarshal(raw, &result)
	ids := make([]any, 0, 2*len(result.Snapshots))
	for _, snapshot := range result.Snapshots {
		ids = append(ids, snapshot.ID, snapshot.ShortID)
	}
	byID := map[string]*core.Record{}
	if len(ids) > 0 {
		runs := []*core.Record{}
		err := p.App.RecordQuery(schema.BackupRuns).AndWhere(dbx.HashExp{"job": jobID, "target": targetID}).
			AndWhere(dbx.In("snapshot_id", ids...)).All(&runs)
		if err != nil {
			p.Log.Error("failed to read the runs of snapshots", "job", jobID, "error", err)
		}
		for _, run := range runs {
			byID[run.GetString("snapshot_id")] = run
		}
	}
	snapshots := make([]any, 0, len(result.Snapshots))
	for _, snapshot := range result.Snapshots {
		var made any
		run := byID[snapshot.ID]
		if run == nil {
			run = byID[snapshot.ShortID]
		}
		if run != nil {
			made = map[string]any{"id": run.Id, "trigger": run.GetString("trigger")}
			if snapshot.SizeBytes == 0 {
				snapshot.SizeBytes = int64(run.GetInt("total_bytes"))
			}
			if snapshot.AddedBytes == 0 {
				snapshot.AddedBytes = int64(run.GetInt("bytes_added"))
			}
		}
		paths := snapshot.Paths
		if paths == nil {
			paths = []string{}
		}
		snapshots = append(snapshots, map[string]any{
			"id": snapshot.ID, "short_id": snapshot.ShortID, "time": snapshot.Time, "hostname": snapshot.Hostname,
			"paths": paths, "size_bytes": snapshot.SizeBytes, "added_bytes": snapshot.AddedBytes, "files": snapshot.Files, "run": made,
		})
	}
	total := max(result.Total, len(snapshots))
	return map[string]any{
		"target": targetID, "host": command.GetString("host"), "fetched_at": time.Now().UTC().Format(time.RFC3339),
		"total": total, "truncated": total > len(snapshots), "snapshots": snapshots,
	}
}

// cachedSnapshot is a snapshot of a job in a target as its kept listing has
// it, if the panel has one.
func (p *Panel) cachedSnapshot(jobID, targetID, snapshotID string) (agentSnapshot, bool) {
	value, ok := p.snapshotCache.peek(snapshotsKey(jobID, targetID))
	if !ok {
		return agentSnapshot{}, false
	}
	list, _ := value["snapshots"].([]any)
	for _, item := range list {
		snapshot, _ := item.(map[string]any)
		if snapshot["id"] != snapshotID {
			continue
		}
		raw, _ := json.Marshal(snapshot)
		var found agentSnapshot
		return found, json.Unmarshal(raw, &found) == nil
	}
	return agentSnapshot{}, false
}

// backupRunEnded drops a job's snapshots in the target a backup ran to,
// which now has one more.
func (p *Panel) backupRunEnded(command *core.Record) {
	payload := listingPayloadOf(command)
	p.snapshotCache.forget(snapshotsKey(payload.jobID(), payload.Repository.TargetID))
}

// removeCommandLater removes a command once its finishers are done.
func (p *Panel) removeCommandLater(id string) {
	p.later.after(0, func() {
		command, err := p.App.FindRecordById(schema.Commands, id)
		if err != nil {
			return
		}
		if err := p.App.Delete(command); err != nil {
			p.Log.Error("failed to remove a command", "command", id, "error", err)
		}
	})
}

// browseKey is the key of a page of a snapshot's folder.
func browseKey(jobID, targetID, snapshotID, dir string, offset, limit int, match string) string {
	return strings.Join([]string{jobID, targetID, snapshotID, dir, strconv.Itoa(offset), strconv.Itoa(limit), strings.ToLower(match)}, "|")
}

// handleSnapshotFiles lists a page of a snapshot's folder: directories
// first, then by name.
func (p *Panel) handleSnapshotFiles(e *core.RequestEvent) error {
	query := e.Request.URL.Query()
	snapshot := e.Request.PathValue("snapshot")
	if !fullSnapshotID.MatchString(snapshot) {
		return e.BadRequestError("Choose a snapshot.", nil)
	}
	dir := query.Get("path")
	if dir == "" {
		dir = "/"
	}
	if !snapshotPath(dir) {
		return e.BadRequestError("Give an absolute path in the snapshot.", nil)
	}
	offset, limit, match := 0, 0, query.Get("match")
	var err error
	if raw := query.Get("offset"); raw != "" {
		if offset, err = strconv.Atoi(raw); err != nil || offset < 0 {
			return e.BadRequestError("The offset is a count of entries.", nil)
		}
	}
	if raw := query.Get("limit"); raw != "" {
		if limit, err = strconv.Atoi(raw); err != nil || limit < 0 || limit > browseLimit {
			return e.BadRequestError(fmt.Sprintf("A page lists at most %d entries.", browseLimit), nil)
		}
	}
	if utf8.RuneCountInString(match) > 255 || strings.ContainsAny(match, "\x00\n\r") {
		return e.BadRequestError("Filter by a part of a name.", nil)
	}
	job, target, err := p.jobAndTarget(e, query.Get("target"))
	if err != nil {
		return err
	}
	host, problem := p.listingHost(job, target, "lists the snapshots")
	if problem != nil {
		return e.Error(problem.status, problem.message, nil)
	}
	command := &agentv1.Command{Payload: &agentv1.Command_BackupBrowse{BackupBrowse: &agentv1.BackupBrowse{
		Repository: &agentv1.BackupRepository{TargetId: target.Id}, SnapshotId: snapshot, Path: dir,
		Offset: int32(offset), Limit: int32(limit), Match: match, JobTag: "job:" + job.Id, //nolint:gosec // checked above
	}}}
	key := browseKey(job.Id, target.Id, snapshot, dir, offset, limit, match)
	found, cached, pending := p.browseCache.get(e.Request.Context(), key, false, p.listingWait(),
		func(id string) *targetProblem { return p.queueListing(e, id, host, target, command) })
	return answerListing(e, found, cached, pending)
}

// snapshotPath tells whether a path names something in a snapshot: an
// absolute, clean path.
func snapshotPath(dir string) bool {
	return path.IsAbs(dir) && path.Clean(dir) == dir && len(dir) <= maxSnapshotPath && utf8.ValidString(dir) &&
		!strings.ContainsAny(dir, "\x00\n\r")
}

// folderListed keeps a page of a snapshot's folder as a listing gave it,
// and removes the listing's command.
func (p *Panel) folderListed(command *core.Record) {
	defer p.removeCommandLater(command.Id)
	payload := listingPayloadOf(command)
	target, _ := p.App.FindRecordById(schema.BackupTargets, payload.Repository.TargetID)
	result := listing{}
	if command.GetString("status") != schema.CommandSucceeded {
		result.problem = p.listingProblem(command, target)
	} else {
		value := map[string]any{}
		raw, _ := json.Marshal(command.Get("result"))
		_ = json.Unmarshal(raw, &value)
		result.value = value
	}
	key := browseKey(payload.jobID(), payload.Repository.TargetID, payload.SnapshotID, payload.Path, payload.Offset, payload.Limit, payload.Match)
	p.browseCache.finish(command.Id, key, result)
}
