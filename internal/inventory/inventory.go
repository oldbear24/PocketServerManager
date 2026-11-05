// Package inventory keeps the panel's picture of the Docker objects on each host:
// a complete snapshot after every connect, then incremental updates from events.
package inventory

import (
	"encoding/json"
	"log/slog"
	"maps"
	"reflect"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

const (
	labelProject = "com.docker.compose.project"
	labelService = "com.docker.compose.service"
	labelRole    = "pocketcloud.role"
	// labelAuth holds the password hashes of a service's domains (see
	// stacks.LabelAuth), which are not for everyone who may see containers
	labelAuth = "pocketcloud.auth"
)

// Store writes inventory reports to the collections.
type Store struct {
	App core.App

	// locks keeps the snapshot and the events of one host apart: right after a
	// connect both arrive at once, and two writers that each find no record for a
	// new container would both insert it.
	locks sync.Map
}

func (s *Store) lock(hostID string) func() {
	value, _ := s.locks.LoadOrStore(hostID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

// Sync makes the stored inventory of a host match a snapshot: it updates what
// changed, adds what is new and removes what is gone. Records are matched by
// their Docker identity, so an agent restart produces no duplicates.
func (s *Store) Sync(hostID string, report *agentv1.ReportInventoryRequest) error {
	defer s.lock(hostID)()

	// the snapshot and the events come on separate streams, so an event
	// about a container that appeared after the snapshot was collected can
	// arrive first; what it stored is newer than the snapshot, and stays
	var collected time.Time
	if at := report.GetCollectedAt(); at.IsValid() {
		collected = at.AsTime()
	}

	return s.App.RunInTransaction(func(tx core.App) error {
		stacks, err := stacksByName(tx, hostID)
		if err != nil {
			return err
		}

		seen := make(map[string]bool, len(report.GetContainers()))
		for _, container := range report.GetContainers() {
			seen[container.GetDockerId()] = true
			if err := upsertContainer(tx, hostID, container, stacks); err != nil {
				return err
			}
		}
		if err := removeMissing(tx, schema.Containers, hostID, "docker_id", seen, collected); err != nil {
			return err
		}

		seen = make(map[string]bool, len(report.GetVolumes()))
		for _, volume := range report.GetVolumes() {
			seen[volume.GetName()] = true
			if err := upsertVolume(tx, hostID, volume); err != nil {
				return err
			}
		}
		if err := removeMissing(tx, schema.Volumes, hostID, "name", seen, collected); err != nil {
			return err
		}

		seen = make(map[string]bool, len(report.GetNetworks()))
		for _, network := range report.GetNetworks() {
			seen[network.GetName()] = true
			if err := upsertNetwork(tx, hostID, network); err != nil {
				return err
			}
		}
		return removeMissing(tx, schema.Networks, hostID, "name", seen, collected)
	})
}

// Apply stores one incremental change.
func (s *Store) Apply(hostID string, event *agentv1.ReportEventsRequest) error {
	defer s.lock(hostID)()

	switch change := event.GetChange().(type) {
	case *agentv1.ReportEventsRequest_ContainerUpdated:
		stacks, err := stacksByName(s.App, hostID)
		if err != nil {
			return err
		}
		return upsertContainer(s.App, hostID, change.ContainerUpdated, stacks)
	case *agentv1.ReportEventsRequest_ContainerRemoved:
		return removeOne(s.App, schema.Containers, hostID, "docker_id", change.ContainerRemoved)
	case *agentv1.ReportEventsRequest_VolumeUpdated:
		return upsertVolume(s.App, hostID, change.VolumeUpdated)
	case *agentv1.ReportEventsRequest_VolumeRemoved:
		return removeOne(s.App, schema.Volumes, hostID, "name", change.VolumeRemoved)
	case *agentv1.ReportEventsRequest_NetworkUpdated:
		return upsertNetwork(s.App, hostID, change.NetworkUpdated)
	case *agentv1.ReportEventsRequest_NetworkRemoved:
		return removeOne(s.App, schema.Networks, hostID, "name", change.NetworkRemoved)
	}
	return nil
}

func stacksByName(app core.App, hostID string) (map[string]string, error) {
	records, err := app.FindRecordsByFilter(schema.Stacks, "host={:host}", "", 0, 0, dbx.Params{"host": hostID})
	if err != nil {
		return nil, err
	}

	byName := make(map[string]string, len(records))
	for _, record := range records {
		byName[record.GetString("name")] = record.Id
	}

	return byName, nil
}

func upsertContainer(app core.App, hostID string, info *agentv1.ContainerInfo, stacks map[string]string) error {
	labels := redactLabels(info.GetLabels())
	project := labels[labelProject]

	ports := make([]map[string]any, 0, len(info.GetPorts()))
	for _, port := range info.GetPorts() {
		ports = append(ports, map[string]any{
			"private":  port.GetPrivatePort(),
			"public":   port.GetPublicPort(),
			"protocol": port.GetProtocol(),
			"ip":       port.GetIp(),
		})
	}

	fields := map[string]any{
		"host":          hostID,
		"stack":         stacks[project],
		"project":       project,
		"service":       labels[labelService],
		"docker_id":     info.GetDockerId(),
		"name":          info.GetName(),
		"image":         info.GetImage(),
		"state":         info.GetState(),
		"status_text":   info.GetStatus(),
		"health":        info.GetHealth(),
		"system_role":   labels[labelRole],
		"restart_count": info.GetRestartCount(),
		"ports":         ports,
		"labels":        labels,
	}

	if started := info.GetStartedAt(); started != nil && !started.AsTime().IsZero() && started.AsTime().Year() > 1 {
		if at, err := types.ParseDateTime(started.AsTime().UTC().Truncate(time.Millisecond)); err == nil {
			fields["started_at"] = at
		}
	}

	return upsert(app, schema.Containers, hostID, "docker_id", info.GetDockerId(), fields)
}

// redactLabels hides the values of labels that hold secrets, keeping that they
// are there.
func redactLabels(labels map[string]string) map[string]string {
	if _, ok := labels[labelAuth]; !ok {
		return labels
	}
	redacted := maps.Clone(labels)
	redacted[labelAuth] = Redacted
	return redacted
}

// Redacted stands in for the value of a label that holds secrets.
const Redacted = "(hidden)"

func upsertVolume(app core.App, hostID string, info *agentv1.VolumeInfo) error {
	return upsert(app, schema.Volumes, hostID, "name", info.GetName(), map[string]any{
		"host":       hostID,
		"name":       info.GetName(),
		"driver":     info.GetDriver(),
		"size_bytes": info.GetSizeBytes(),
		"labels":     info.GetLabels(),
	})
}

func upsertNetwork(app core.App, hostID string, info *agentv1.NetworkInfo) error {
	return upsert(app, schema.Networks, hostID, "name", info.GetName(), map[string]any{
		"host":      hostID,
		"name":      info.GetName(),
		"docker_id": info.GetDockerId(),
		"driver":    info.GetDriver(),
		"labels":    info.GetLabels(),
	})
}

// upsert saves a record only if something changed: every save goes out to the
// browsers over realtime, and most inventory reports change nothing.
func upsert(app core.App, collectionName, hostID, keyField, key string, fields map[string]any) error {
	records, err := app.FindRecordsByFilter(collectionName, "host={:host} && "+keyField+"={:key}", "", 1, 0,
		dbx.Params{"host": hostID, "key": key})
	if err != nil {
		return err
	}

	var record *core.Record
	if len(records) > 0 {
		record = records[0]
		if !changed(record, fields) {
			return nil
		}
	} else {
		collection, err := app.FindCachedCollectionByNameOrId(collectionName)
		if err != nil {
			return err
		}
		record = core.NewRecord(collection)
	}

	for name, value := range fields {
		record.Set(name, value)
	}

	return app.Save(record)
}

func changed(record *core.Record, fields map[string]any) bool {
	for name, value := range fields {
		if !sameValue(record.Get(name), value) {
			return true
		}
	}
	return false
}

func sameValue(a, b any) bool {
	rawA, errA := json.Marshal(a)
	rawB, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}

	var valueA, valueB any
	if json.Unmarshal(rawA, &valueA) != nil || json.Unmarshal(rawB, &valueB) != nil {
		return false
	}

	// an empty map or list equals a missing value
	if isBlank(valueA) && isBlank(valueB) {
		return true
	}

	return reflect.DeepEqual(valueA, valueB)
}

func isBlank(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case float64:
		return v == 0
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	}
	return false
}

// removeMissing removes what a snapshot collected at collected no longer has,
// but not a record stored since then: an event saw it after the snapshot was
// taken. A zero time removes everything that is missing.
func removeMissing(app core.App, collectionName, hostID, keyField string, seen map[string]bool, collected time.Time) error {
	records, err := app.FindRecordsByFilter(collectionName, "host={:host}", "", 0, 0, dbx.Params{"host": hostID})
	if err != nil {
		return err
	}

	for _, record := range records {
		if seen[record.GetString(keyField)] {
			continue
		}
		if !collected.IsZero() && !record.GetDateTime("updated").Time().Before(collected) {
			continue
		}
		if err := app.Delete(record); err != nil {
			return err
		}
	}

	return nil
}

func removeOne(app core.App, collectionName, hostID, keyField, key string) error {
	records, err := app.FindRecordsByFilter(collectionName, "host={:host} && "+keyField+"={:key}", "", 1, 0,
		dbx.Params{"host": hostID, "key": key})
	if err != nil {
		return err
	}

	for _, record := range records {
		if err := app.Delete(record); err != nil {
			slog.Error("failed to remove an inventory record", "collection", collectionName, "error", err)
			return err
		}
	}

	return nil
}
