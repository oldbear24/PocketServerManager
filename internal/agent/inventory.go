package agent

import (
	"context"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
)

const dockerRetryDelay = 5 * time.Second

// ignoredContainerActions are events that change nothing in the inventory.
var ignoredContainerActions = []string{"exec_", "attach", "detach", "resize", "top", "archive-path", "extract-to-dir", "copy", "commit", "export"}

func containerInfo(listed dockerapi.Container, inspected *dockerapi.ContainerInspect) *agentv1.ContainerInfo {
	info := &agentv1.ContainerInfo{
		DockerId: listed.ID,
		Image:    listed.Image,
		State:    listed.State,
		Status:   listed.Status,
		Labels:   listed.Labels,
	}

	if len(listed.Names) > 0 {
		info.Name = strings.TrimPrefix(listed.Names[0], "/")
	}

	for _, port := range listed.Ports {
		info.Ports = append(info.Ports, &agentv1.PortMapping{
			PrivatePort: port.PrivatePort,
			PublicPort:  port.PublicPort,
			Protocol:    port.Type,
			Ip:          port.IP,
		})
	}

	if inspected != nil {
		info.Health = inspected.Health()
		info.RestartCount = inspected.RestartCount
		if !inspected.State.StartedAt.IsZero() {
			info.StartedAt = timestamppb.New(inspected.State.StartedAt)
		}
	}

	return info
}

// collectInventory takes a complete snapshot of the Docker objects.
func (a *Agent) collectInventory(ctx context.Context) (*agentv1.ReportInventoryRequest, error) {
	containers, err := a.Docker.Containers(ctx)
	if err != nil {
		return nil, err
	}

	report := &agentv1.ReportInventoryRequest{CollectedAt: timestamppb.Now()}

	for _, listed := range containers {
		inspected, err := a.Docker.Inspect(ctx, listed.ID)
		if err != nil && !dockerapi.IsNotFound(err) {
			return nil, err
		}
		report.Containers = append(report.Containers, containerInfo(listed, inspected))
	}

	volumes, err := a.Docker.Volumes(ctx)
	if err != nil {
		return nil, err
	}
	for _, volume := range volumes {
		report.Volumes = append(report.Volumes, volumeInfo(volume))
	}

	networks, err := a.Docker.Networks(ctx)
	if err != nil {
		return nil, err
	}
	for _, network := range networks {
		report.Networks = append(report.Networks, networkInfo(network))
	}

	return report, nil
}

func volumeInfo(volume dockerapi.Volume) *agentv1.VolumeInfo {
	return &agentv1.VolumeInfo{Name: volume.Name, Driver: volume.Driver, SizeBytes: -1, Labels: volume.Labels}
}

func networkInfo(network dockerapi.Network) *agentv1.NetworkInfo {
	return &agentv1.NetworkInfo{Name: network.Name, Driver: network.Driver, DockerId: network.ID, Labels: network.Labels}
}

// inventoryLoop sends a snapshot after connecting and periodically afterwards,
// which also repairs anything the event stream may have missed.
func (a *Agent) inventoryLoop(ctx context.Context, client agentv1.AgentServiceClient) error {
	ticker := time.NewTicker(a.Config.InventoryInterval)
	defer ticker.Stop()

	for {
		if err := a.sendInventory(ctx, client); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// sendInventory only fails for errors of the panel connection. If Docker does not
// answer the stored inventory stays as it is, and the self-check says what is wrong.
func (a *Agent) sendInventory(parent context.Context, client agentv1.AgentServiceClient) error {
	ctx, cancel := context.WithTimeout(parent, inventoryTimeout)
	defer cancel()

	report, err := a.collectInventory(ctx)
	if err != nil {
		a.Log.Warn("failed to collect the inventory", "error", err)
		return nil
	}

	_, err = client.ReportInventory(ctx, report)

	return err
}

// eventsLoop turns Docker events into incremental inventory updates.
func (a *Agent) eventsLoop(ctx context.Context, client agentv1.AgentServiceClient) error {
	stream, err := client.ReportEvents(ctx)
	if err != nil {
		return err
	}

	for {
		var sendErr error

		err := a.Docker.Events(ctx, func(event dockerapi.Event) {
			if sendErr != nil {
				return
			}
			update := a.eventToUpdate(ctx, event)
			if update != nil {
				sendErr = stream.Send(update)
			}
		})
		if sendErr != nil {
			return sendErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		a.Log.Debug("the Docker event stream ended", "error", err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dockerRetryDelay):
		}

		// events may have been missed in the meantime
		if err := a.sendInventory(ctx, client); err != nil {
			return err
		}
	}
}

func (a *Agent) eventToUpdate(ctx context.Context, event dockerapi.Event) *agentv1.ReportEventsRequest {
	update := &agentv1.ReportEventsRequest{OccurredAt: timestamppb.Now()}

	switch event.Type {
	case "container":
		for _, ignored := range ignoredContainerActions {
			if strings.HasPrefix(event.Action, ignored) {
				return nil
			}
		}

		if event.Action == "destroy" {
			update.Change = &agentv1.ReportEventsRequest_ContainerRemoved{ContainerRemoved: event.Actor.ID}
			return update
		}

		inspected, err := a.Docker.Inspect(ctx, event.Actor.ID)
		if err != nil {
			// removed in the meantime: the destroy event takes care of it
			return nil
		}

		update.Change = &agentv1.ReportEventsRequest_ContainerUpdated{ContainerUpdated: a.containerFromInspect(ctx, inspected)}

		return update

	case "volume":
		switch event.Action {
		case "destroy":
			update.Change = &agentv1.ReportEventsRequest_VolumeRemoved{VolumeRemoved: event.Actor.ID}
			return update
		case "create":
			volume, err := a.Docker.InspectVolume(ctx, event.Actor.ID)
			if err != nil {
				return nil
			}
			update.Change = &agentv1.ReportEventsRequest_VolumeUpdated{VolumeUpdated: volumeInfo(*volume)}
			return update
		}

	case "network":
		switch event.Action {
		case "destroy":
			update.Change = &agentv1.ReportEventsRequest_NetworkRemoved{NetworkRemoved: event.Actor.Attributes["name"]}
			return update
		case "create":
			network, err := a.Docker.InspectNetwork(ctx, event.Actor.ID)
			if err != nil {
				return nil
			}
			update.Change = &agentv1.ReportEventsRequest_NetworkUpdated{NetworkUpdated: networkInfo(*network)}
			return update
		}
	}

	return nil
}

// containerFromInspect builds the inventory entry of one container. Inspect has
// no ports or status text in list form, so the list is asked for the rest.
func (a *Agent) containerFromInspect(ctx context.Context, inspected *dockerapi.ContainerInspect) *agentv1.ContainerInfo {
	listed := dockerapi.Container{ID: inspected.ID, Names: []string{inspected.Name}, State: inspected.State.Status}
	if image, ok := inspected.Config["Image"].(string); ok {
		listed.Image = image
	}

	if labels, ok := inspected.Config["Labels"].(map[string]any); ok {
		listed.Labels = make(map[string]string, len(labels))
		for key, value := range labels {
			if text, ok := value.(string); ok {
				listed.Labels[key] = text
			}
		}
	}

	if containers, err := a.Docker.Containers(ctx); err == nil {
		for _, candidate := range containers {
			if candidate.ID == inspected.ID {
				listed = candidate
				break
			}
		}
	}

	return containerInfo(listed, inspected)
}
