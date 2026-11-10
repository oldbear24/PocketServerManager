package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
)

// The containers a restore stops (#30): those that use its destination, and
// its stack's, read live once the restore is ready. Each gets the restart
// policy no while it is stopped, so that neither the Engine nor a reboot
// starts it on half-restored data; a marker in the agent's volume names
// them, and their policies, before the first stops, so that an agent that
// restarts during the restore starts them again (finishInterruptedRestores).

const (
	// restoreMarkers is the folder of the agent's volume the markers are in.
	restoreMarkers = "restores"
	// restoreStopTimeout is how long a container may take to stop, in
	// seconds, as before a move.
	restoreStopTimeout = moveStopTimeout
	// restoreStartTimeout bounds starting the containers again.
	restoreStartTimeout = 3 * time.Minute
	// composeDependsLabel names the services a Compose container depends on:
	// "db:service_started:false,redis:service_healthy:true".
	composeDependsLabel = "com.docker.compose.depends_on"
)

// restoreVerifyWait is how long after starting them the restore looks at
// whether the containers still run; tests shorten it.
var restoreVerifyWait = 10 * time.Second

// markerName is what a command's id may be to name its marker.
var markerName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// restoreMarker is what an interrupted restore left to start again.
type restoreMarker struct {
	CommandID  string            `json:"command_id"`
	RestoreID  string            `json:"restore_id"`
	Containers []markedContainer `json:"containers"`
}

// markedContainer is a container a restore stopped, with the restart policy
// it had.
type markedContainer struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	RestartPolicy restartPolicy `json:"restart_policy"`
}

// restartPolicy is a container's restart policy.
type restartPolicy struct {
	Name     string `json:"name"`
	MaxRetry int    `json:"max_retry"`
}

// policyOf is an inspected container's restart policy.
func policyOf(info *dockerapi.ContainerInspect) restartPolicy {
	raw, _ := info.HostConfig["RestartPolicy"].(map[string]any)
	name, _ := raw["Name"].(string)
	retry, _ := raw["MaximumRetryCount"].(float64)
	return restartPolicy{Name: name, MaxRetry: int(retry)}
}

// restoreUser is a container that uses a restore's destination.
type restoreUser struct {
	id, name, project, service string
	dependsOn                  []string
}

// affected are the running containers a restore stops, dependents first:
// those of stack, and those that mount a destination volume, or a folder of
// the server read-write that holds a destination or is in one. A container
// that reads a folder above a destination, such as a node exporter that
// mounts / read-only, runs on. A PocketCloud container that writes a
// destination refuses the restore.
func (a *Agent) affected(ctx context.Context, commandID, stack string, dests []restoreDest) ([]restoreUser, error) {
	containers, err := a.Docker.Containers(ctx)
	if err != nil {
		return nil, err
	}
	self := a.ownContainerID(ctx)
	var users []restoreUser
	for _, container := range containers {
		// the agent, and the restore.s own job
		if container.ID == self || container.Labels["pocketcloud.command"] == commandID {
			continue
		}
		uses, writes := usesDests(container, dests)
		inStack := stack != "" && container.Labels[composeProjectLabel] == stack
		if !uses && !inStack {
			continue
		}
		name := strings.TrimPrefix(firstName(container), "/")
		if role := container.Labels["pocketcloud.role"]; role != "" {
			if writes {
				return nil, fmt.Errorf("%s is a PocketCloud container (%s) that writes the destination: restore elsewhere", name, role)
			}
			continue
		}
		if container.State != "running" && container.State != "restarting" {
			continue
		}
		user := restoreUser{id: container.ID, name: name, project: container.Labels[composeProjectLabel], service: container.Labels[composeServiceLabel]}
		for depend := range strings.SplitSeq(container.Labels[composeDependsLabel], ",") {
			if service, _, _ := strings.Cut(depend, ":"); service != "" {
				user.dependsOn = append(user.dependsOn, service)
			}
		}
		users = append(users, user)
	}
	return stopOrder(users), nil
}

// firstName is a listed container's name.
func firstName(container dockerapi.Container) string {
	if len(container.Names) == 0 {
		return container.ID
	}
	return container.Names[0]
}

// usesDests says whether a container uses a destination, and writes it.
func usesDests(container dockerapi.Container, dests []restoreDest) (bool, bool) {
	uses, writes := false, false
	for _, mount := range container.Mounts {
		for _, dest := range dests {
			switch {
			case dest.volume != "" && mount.Type == "volume" && mount.Name == dest.volume:
				uses = true
				writes = writes || mount.RW
			case dest.dir != "" && mount.Type == "bind" && mount.RW && backup.Overlap(mount.Source, dest.dir):
				uses, writes = true, true
			}
		}
	}
	return uses, writes
}

// stopOrder orders the containers a restore stops: one that another depends
// on stops after it, and starts before it.
func stopOrder(users []restoreUser) []restoreUser {
	remaining := slices.Clone(users)
	slices.SortFunc(remaining, func(a, b restoreUser) int { return strings.Compare(a.name, b.name) })
	var order []restoreUser
	for len(remaining) > 0 {
		moved := false
		for i := 0; i < len(remaining); i++ {
			user := remaining[i]
			needed := slices.ContainsFunc(remaining, func(other restoreUser) bool {
				return other.id != user.id && user.service != "" && other.project == user.project && slices.Contains(other.dependsOn, user.service)
			})
			if needed {
				continue
			}
			order = append(order, user)
			remaining = slices.Delete(remaining, i, i+1)
			i--
			moved = true
		}
		if !moved {
			// dependencies in a circle: in name order
			order = append(order, remaining...)
			break
		}
	}
	return order
}

// restoringSet are the containers restores hold stopped: none of them is
// started from the panel until its restore started it again.
type restoringSet struct {
	mu    sync.Mutex
	names map[string]string
}

func (s *restoringSet) hold(users []restoreUser) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.names == nil {
		s.names = map[string]string{}
	}
	for _, user := range users {
		s.names[user.id] = user.name
	}
}

func (s *restoringSet) release(users []restoreUser) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, user := range users {
		delete(s.names, user.id)
	}
}

// holds says whether a container, named as a command names it (its id, a
// part of it, or its name), is one a restore holds.
func (s *restoringSet) holds(ref string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref = strings.TrimPrefix(ref, "/")
	for id, name := range s.names {
		if ref == id || ref == name || len(ref) >= 12 && strings.HasPrefix(id, ref) {
			return true
		}
	}
	return false
}

// errRestoring refuses to start a container a restore holds.
var errRestoring error = sentenceError("A restore is writing this container's data: it starts again when the restore ends.")

// restoreSession is a real restore's hold on the containers it stops.
type restoreSession struct {
	a       *Agent
	marker  restoreMarker
	users   []restoreUser
	stopped []markedContainer
}

// markerPath is the file of a command's marker.
func (a *Agent) markerPath(commandID string) string {
	return filepath.Join(a.Config.StateDir, restoreMarkers, commandID+".json")
}

// writeMarker writes what the session stopped, and is about to.
func (s *restoreSession) writeMarker() error {
	dir := filepath.Join(s.a.Config.StateDir, restoreMarkers)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("the agent cannot note what it stops in its volume: %w", err)
	}
	raw, err := json.Marshal(s.marker)
	if err != nil {
		return err
	}
	if err := writeAtomic(s.a.markerPath(s.marker.CommandID), raw, 0o600); err != nil {
		return fmt.Errorf("the agent cannot note what it stops in its volume: %w", err)
	}
	return nil
}

// stop stops the session's containers, each with the restart policy no,
// noting each in the marker before it changes anything of it. It stops at
// the first that fails, or when ctx ends; what it stopped is in s.stopped.
func (s *restoreSession) stop(ctx context.Context) error {
	docker := s.a.Docker
	// a container is never left half handled: what is begun is done
	work := context.WithoutCancel(ctx)
	for _, user := range s.users {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := docker.Inspect(work, user.id)
		if dockerapi.IsNotFound(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("%s cannot be inspected: %w", user.name, err)
		}
		marked := markedContainer{ID: user.id, Name: user.name, RestartPolicy: policyOf(info)}
		s.marker.Containers = append(s.marker.Containers, marked)
		if err := s.writeMarker(); err != nil {
			s.marker.Containers = s.marker.Containers[:len(s.marker.Containers)-1]
			return err
		}
		if err := docker.UpdateRestartPolicy(work, user.id, "no", 0); err != nil && !dockerapi.IsNotFound(err) {
			return fmt.Errorf("the restart policy of %s cannot be changed: %w", user.name, err)
		}
		s.stopped = append(s.stopped, marked)
		if err := docker.Stop(work, user.id, restoreStopTimeout); err != nil && !dockerapi.IsNotFound(err) {
			return fmt.Errorf("%s did not stop: %w", user.name, err)
		}
		if err := s.writeMarker(); err != nil {
			return err
		}
	}
	return nil
}

// restoreStart is how starting the containers again went.
type restoreStart struct {
	started, notRunning, errors []string
}

// startAgain starts what the session stopped, in the reverse order, puts
// their restart policies back, and, with verify, says which of them do not
// run a while later. It runs whatever ended the restore, and removes the
// marker once the Engine answered for every container.
func (s *restoreSession) startAgain(verify bool) restoreStart {
	defer s.a.restoring.release(s.users)
	result := restoreStart{started: []string{}, notRunning: []string{}, errors: []string{}}
	if len(s.stopped) == 0 {
		s.removeMarker()
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), restoreStartTimeout)
	defer cancel()
	docker := s.a.Docker
	answered := true
	var running []markedContainer
	for i := len(s.stopped) - 1; i >= 0; i-- {
		container := s.stopped[i]
		err := docker.Start(ctx, container.ID)
		if err != nil && !dockerapi.IsNotFound(err) {
			// one that runs already, as the Engine says 304
			if info, inspectErr := docker.Inspect(ctx, container.ID); inspectErr == nil && info.State.Running {
				err = nil
			}
		}
		var engine *dockerapi.Error
		switch {
		case err == nil:
			result.started = append(result.started, container.Name)
			running = append(running, container)
		case dockerapi.IsNotFound(err):
			// it went while it was stopped
		default:
			result.errors = append(result.errors, fmt.Sprintf("%s did not start: %v", container.Name, err))
			answered = answered && errors.As(err, &engine)
		}
		policy := container.RestartPolicy
		if err := docker.UpdateRestartPolicy(ctx, container.ID, policy.Name, policy.MaxRetry); err != nil && !dockerapi.IsNotFound(err) {
			result.errors = append(result.errors, fmt.Sprintf("the restart policy of %s stays no: %v", container.Name, err))
			answered = answered && errors.As(err, &engine)
		}
	}
	if verify && len(running) > 0 {
		select {
		case <-time.After(restoreVerifyWait):
		case <-ctx.Done():
		}
		for _, container := range running {
			if info, err := docker.Inspect(ctx, container.ID); err == nil && !info.State.Running {
				result.notRunning = append(result.notRunning, container.Name)
			}
		}
	}
	if answered {
		s.removeMarker()
	}
	return result
}

// removeMarker removes the session's marker.
func (s *restoreSession) removeMarker() {
	if err := os.Remove(s.a.markerPath(s.marker.CommandID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.a.Log.Warn("failed to remove a restore's marker", "command", s.marker.CommandID, "error", err)
	}
}

// interruptedMessage is the end of a restore the agent restarted during.
func interruptedMessage(started, all int) string {
	return fmt.Sprintf("The agent restarted during the restore, so the destination may hold only part of the snapshot. It started the application again (%d of %d containers).", started, all)
}

// finishInterruptedRestores starts again what the restores the agent was
// running when it stopped had stopped, puts their restart policies back,
// and settles each command, which the panel's replay then reads rather than
// running it again. It runs once the jobs of those restores are gone.
func (a *Agent) finishInterruptedRestores(ctx context.Context) {
	dir := filepath.Join(a.Config.StateDir, restoreMarkers)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		file := filepath.Join(dir, entry.Name())
		if strings.HasSuffix(entry.Name(), ".tmp") {
			// a marker that was being written
			_ = os.Remove(file)
			continue
		}
		if ctx.Err() != nil || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(file) //nolint:gosec // the agent's own volume
		var marker restoreMarker
		if err == nil {
			err = json.Unmarshal(raw, &marker)
		}
		if err != nil || !markerName.MatchString(marker.CommandID) || marker.CommandID+".json" != entry.Name() {
			a.Log.Warn("a restore's marker cannot be read: its containers stay as they are", "file", file, "error", err)
			_ = os.Remove(file)
			continue
		}
		session := &restoreSession{a: a, marker: marker, stopped: marker.Containers}
		started := session.startAgain(false)
		a.Log.Warn("a restore was interrupted: its containers were started again", "command", marker.CommandID, "started", started.started, "errors", started.errors)
		result := map[string]any{"interrupted": true, "app_started": anyList(started.started), "start_errors": anyList(started.errors)}
		if err := a.store.setState(marker.CommandID, stateFailed, interruptedMessage(len(started.started), len(marker.Containers)), result); err != nil {
			a.Log.Error("failed to store a command state", "command", marker.CommandID, "error", err)
		}
	}
}

// anyList is a list as a result holds it.
func anyList(values []string) []any {
	list := make([]any, 0, len(values))
	for _, value := range values {
		list = append(list, value)
	}
	return list
}
