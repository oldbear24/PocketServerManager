package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
)

const (
	storeFile     = "commands.json"
	storeCapacity = 1000
	reportedBack  = 100
	// reportedBackBytes caps the results the agent reports back when it
	// connects, newest first, so that the request stays well under the
	// gateway's 8 MiB message limit; the commands after it are reported
	// without their results (#30).
	reportedBackBytes = 4 << 20
)

// Local states of a command in the store.
const (
	stateStarted    = "started"
	stateHandedOver = "handed_over"
	stateSucceeded  = "succeeded"
	stateFailed     = "failed"
	stateCancelled  = "cancelled"
)

// commandEntry is what the agent remembers about a command it has seen.
type commandEntry struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	State      string         `json:"state"`
	Error      string         `json:"error,omitempty"`
	Result     map[string]any `json:"result,omitempty"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at,omitzero"`
	// kept is the result of an ephemeral command, which only this process
	// remembers (see setEphemeral)
	kept map[string]any
}

// result is what the command answered: what the store keeps on disk, or in
// memory for an ephemeral one.
func (e *commandEntry) result() map[string]any {
	if e.Result != nil {
		return e.Result
	}
	return e.kept
}

func (e *commandEntry) finished() bool {
	return e.State == stateSucceeded || e.State == stateFailed || e.State == stateCancelled
}

func (e *commandEntry) protoState() agentv1.CommandState {
	switch e.State {
	case stateSucceeded:
		return agentv1.CommandState_COMMAND_STATE_SUCCEEDED
	case stateFailed:
		return agentv1.CommandState_COMMAND_STATE_FAILED
	case stateCancelled:
		return agentv1.CommandState_COMMAND_STATE_CANCELLED
	}
	return agentv1.CommandState_COMMAND_STATE_RUNNING
}

// commandStore persists the recent commands in the agent volume, so that a replay
// after a restart is acknowledged instead of being executed a second time.
type commandStore struct {
	path string

	mu      sync.Mutex
	entries map[string]*commandEntry
	order   []string
}

func openCommandStore(stateDir string) (*commandStore, error) {
	store := &commandStore{
		path:    filepath.Join(stateDir, storeFile),
		entries: make(map[string]*commandEntry),
	}

	raw, err := os.ReadFile(store.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, nil
		}
		return nil, err
	}

	var list []*commandEntry
	if err := json.Unmarshal(raw, &list); err != nil {
		// a corrupted store must not keep the agent from running
		return store, nil //nolint:nilerr
	}

	for _, entry := range list {
		store.entries[entry.ID] = entry
		store.order = append(store.order, entry.ID)
	}

	return store, nil
}

// get returns a copy of an entry.
func (s *commandStore) get(id string) (commandEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[id]
	if !ok {
		return commandEntry{}, false
	}
	return *entry, true
}

// start records a new command. The state is on disk before the command runs, so
// a crash during the command cannot lead to it being executed again.
func (s *commandStore) start(id, commandType string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries[id] = &commandEntry{ID: id, Type: commandType, State: stateStarted, StartedAt: time.Now().UTC()}
	s.order = append(s.order, id)

	for len(s.order) > storeCapacity {
		delete(s.entries, s.order[0])
		s.order = s.order[1:]
	}

	return s.saveLocked()
}

func (s *commandStore) setState(id, state, message string, result map[string]any) error {
	return s.set(id, state, message, result, nil)
}

// setEphemeral is setState for a command whose result is read once (#30):
// the result stays in memory, out of the file the store rewrites whole at
// every start and end, and is gone after a restart, when the panel has long
// read it or given up on it.
func (s *commandStore) setEphemeral(id, state, message string, result map[string]any) error {
	return s.set(id, state, message, nil, result)
}

func (s *commandStore) set(id, state, message string, result, kept map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.entries[id]
	if !ok {
		// an entry the store does not have, such as one a start-up repair
		// settles: added, and the oldest go as when a command starts
		entry = &commandEntry{ID: id, StartedAt: time.Now().UTC()}
		s.entries[id] = entry
		s.order = append(s.order, id)
		for len(s.order) > storeCapacity {
			delete(s.entries, s.order[0])
			s.order = s.order[1:]
		}
	}

	entry.State = state
	entry.Error = message
	entry.Result = result
	entry.kept = kept
	if entry.finished() {
		entry.FinishedAt = time.Now().UTC()
	}

	return s.saveLocked()
}

func (s *commandStore) saveLocked() error {
	list := make([]*commandEntry, 0, len(s.order))
	for _, id := range s.order {
		list = append(list, s.entries[id])
	}

	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}

	return writeAtomic(s.path, raw, 0o600)
}

// recentlyFinished lists the newest finished commands for the panel, with
// their results while those stay under reportedBackBytes, newest first.
func (s *commandStore) recentlyFinished() []*agentv1.FinishedCommand {
	s.mu.Lock()
	defer s.mu.Unlock()

	finished := []*agentv1.FinishedCommand{}
	budget := reportedBackBytes
	for i := len(s.order) - 1; i >= 0 && len(finished) < reportedBack; i-- {
		entry := s.entries[s.order[i]]
		if !entry.finished() {
			continue
		}
		command := &agentv1.FinishedCommand{
			CommandId:  entry.ID,
			State:      entry.protoState(),
			Error:      entry.Error,
			FinishedAt: timestamppb.New(entry.FinishedAt),
		}
		// a final report lost on the way keeps its result, such as a task's,
		// as long as the results fit in the request
		if result := entry.result(); result != nil && budget > 0 {
			if encoded, err := structpb.NewStruct(result); err == nil {
				if size := proto.Size(encoded); size <= budget {
					command.Result = encoded
					budget -= size
				} else {
					budget = 0
				}
			}
		}
		finished = append(finished, command)
	}

	return finished
}

// recent returns the newest entries for diagnostics, without their results.
func (s *commandStore) recent(limit int) []commandEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := []commandEntry{}
	for i := len(s.order) - 1; i >= 0 && len(result) < limit; i-- {
		entry := *s.entries[s.order[i]]
		entry.Result = nil
		result = append(result, entry)
	}

	return result
}
