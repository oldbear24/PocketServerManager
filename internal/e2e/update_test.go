package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/agent"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
	"github.com/oldbear24/PocketServerManager/internal/updater"
	"github.com/oldbear24/PocketServerManager/internal/version"
)

const (
	testAgentImage   = "ghcr.io/oldbear24/pocketcloud-agent"
	testUpdaterImage = "ghcr.io/oldbear24/pocketcloud-updater"
	agentDigest      = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	updaterDigest    = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	oldAgentRef      = testAgentImage + "@sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

// pinRelease makes this test build a release that knows its image digests.
func pinRelease(t *testing.T) {
	t.Helper()

	previousVersion, previousAgent, previousUpdater := version.Version, version.AgentDigest, version.UpdaterDigest
	version.Version = "v1.2.3"
	version.AgentDigest = agentDigest
	version.UpdaterDigest = updaterDigest

	t.Cleanup(func() {
		version.Version, version.AgentDigest, version.UpdaterDigest = previousVersion, previousAgent, previousUpdater
	})
}

// updateSetup is a server whose agent runs in a container of the fake engine.
func (h *harness) updateSetup(name string) (*runningAgent, string) {
	h.t.Helper()

	h.docker.AddImage(oldAgentRef, map[string]any{"Env": []string{"PATH=/usr/bin"}})

	config := h.agentConfig(name, h.token(name, schema.HostRoleDocker))
	config.ContainerName = name + "-agent"

	h.docker.AddContainer(config.ContainerName, oldAgentRef, map[string]string{"pocketcloud.role": "agent"})
	h.docker.SetHostConfig(config.ContainerName, map[string]any{
		"NetworkMode": "bridge",
		"Binds":       []any{"pocketcloud-agent:" + config.StateDir},
	})

	running := h.start(config)

	h.waitFor("the agent to connect", func() bool { return agent.Healthy(config) == nil })

	host, err := h.app.FindFirstRecordByData(schema.Hosts, "name", name)
	if err != nil {
		h.t.Fatal(err)
	}

	return running, host.Id
}

// runUpdater does what the updater container would do, with the settings the
// agent gave that container.
func (h *harness) runUpdater(updaterName, stateDir string) updater.Outcome {
	h.t.Helper()

	container := h.docker.Find(updaterName)
	if container == nil {
		h.t.Fatalf("the agent did not start the updater; containers: %v", h.docker.Names())
	}

	env := map[string]string{}
	values, _ := container.Config["Env"].([]any)
	for _, value := range values {
		key, val, _ := strings.Cut(value.(string), "=")
		env[key] = val
	}

	client, err := dockerapi.New(h.docker.Host())
	if err != nil {
		h.t.Fatal(err)
	}

	outcome := updater.Run(context.Background(), client, updater.Options{
		Target:        env["TARGET_CONTAINER"],
		Image:         env["NEW_IMAGE"],
		CommandID:     env["COMMAND_ID"],
		Version:       env["NEW_VERSION"],
		StateDir:      stateDir,
		HealthTimeout: 300 * time.Millisecond,
		PollInterval:  5 * time.Millisecond,
	})

	if err := updater.WriteOutcome(stateDir, outcome); err != nil {
		h.t.Fatal(err)
	}

	h.docker.RemoveContainer(container.ID) // AutoRemove

	return outcome
}

func TestAgentUpdateEndToEnd(t *testing.T) {
	pinRelease(t)

	h := newHarness(t)
	_, adminToken := testutil.NewUser(t, h.app, "admin@example.com", schema.RoleAdmin)

	running, hostID := h.updateSetup("updated")

	status, body := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: "/api/pc/hosts/" + hostID + "/update", Token: adminToken})
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}
	id := recordID(t, body)

	// the agent pulls both images by digest and starts the updater
	updaterName := "pocketcloud-updater-" + id
	h.waitFor("the updater container", func() bool { return h.docker.Find(updaterName) != nil })

	for _, ref := range []string{"fromImage=ghcr.io%2Foldbear24%2Fpocketcloud-agent&tag=sha256", "fromImage=ghcr.io%2Foldbear24%2Fpocketcloud-updater&tag=sha256"} {
		if !h.docker.Called(ref) {
			t.Errorf("%s was not pulled by digest: %v", ref, h.docker.Calls())
		}
	}

	container := h.docker.Find(updaterName)
	if container.Labels["pocketcloud.role"] != "updater" {
		t.Errorf("labels: %v", container.Labels)
	}
	if container.Host["AutoRemove"] != true {
		t.Errorf("the updater must remove itself: %v", container.Host)
	}

	h.waitFor("the progress to show the updater started", func() bool {
		return strings.Contains(h.command(id).GetString("progress"), "updater_started")
	})

	// the old agent stays running until the updater stops it: the command stays open
	if status := h.command(id).GetString("status"); status != schema.CommandRunning {
		t.Errorf("status while the updater works: %s", status)
	}

	// the updater's new container turns healthy as soon as it starts
	h.docker.OnStart = func(*fakedocker.Container) (string, string) { return "running", "healthy" }

	outcome := h.runUpdater(updaterName, running.config.StateDir)
	if !outcome.Succeeded {
		t.Fatalf("%+v", outcome)
	}

	// the new container runs the new image, and no helper is left behind
	current := h.docker.Find(running.config.ContainerName)
	if current == nil || !strings.HasSuffix(current.Image, agentDigest) {
		t.Fatalf("the new agent container: %+v", current)
	}
	if h.docker.Find(updaterName) != nil || h.docker.Find(running.config.ContainerName+"-old") != nil {
		t.Fatalf("containers left behind: %v", h.docker.Names())
	}

	// in reality the old process is gone by now, and the new one reports
	if err := running.stop(); err != nil {
		t.Fatal(err)
	}
	second := h.start(running.config)
	defer func() { _ = second.stop() }()

	done := h.waitForStatus(id, schema.CommandSucceeded, schema.CommandFailed)
	if done.GetString("status") != schema.CommandSucceeded {
		t.Fatalf("%v", done.FieldsData())
	}
	if !strings.Contains(done.GetString("result"), agentDigest) {
		t.Errorf("result: %s", done.GetString("result"))
	}

	// the outcome is reported once and then cleaned up
	h.waitFor("the outcome file to go", func() bool {
		outcomes, err := updater.ReadOutcomes(running.config.StateDir)
		return err == nil && len(outcomes) == 0
	})
}

func TestBrokenAgentUpdateIsRolledBackAndReported(t *testing.T) {
	pinRelease(t)

	h := newHarness(t)
	_, adminToken := testutil.NewUser(t, h.app, "admin@example.com", schema.RoleAdmin)

	running, hostID := h.updateSetup("rollback")

	originalID := h.docker.Find(running.config.ContainerName).ID

	status, body := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: "/api/pc/hosts/" + hostID + "/update", Token: adminToken})
	if status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}
	id := recordID(t, body)

	updaterName := "pocketcloud-updater-" + id
	h.waitFor("the updater container", func() bool { return h.docker.Find(updaterName) != nil })

	// the new image never connects to the panel
	h.docker.OnStart = func(c *fakedocker.Container) (string, string) {
		if strings.HasSuffix(c.Image, agentDigest) {
			return "running", "starting"
		}
		return "running", ""
	}

	outcome := h.runUpdater(updaterName, running.config.StateDir)
	if outcome.Succeeded || !outcome.RolledBack {
		t.Fatalf("%+v", outcome)
	}

	restored := h.docker.Find(running.config.ContainerName)
	if restored == nil || restored.ID != originalID || restored.Status != "running" {
		t.Fatalf("the old container is not back: %+v", restored)
	}

	// the old agent comes back and reports what happened
	if err := running.stop(); err != nil {
		t.Fatal(err)
	}
	second := h.start(running.config)
	defer func() { _ = second.stop() }()

	failed := h.waitForStatus(id, schema.CommandFailed)
	if !strings.Contains(failed.GetString("error"), "rolled back") {
		t.Errorf("error: %s", failed.GetString("error"))
	}
	if !strings.Contains(failed.GetString("result"), `"rolled_back":true`) {
		t.Errorf("result: %s", failed.GetString("result"))
	}
}

func TestUpdateRefusesImagesWithoutADigest(t *testing.T) {
	h := newHarness(t)

	running, hostID := h.updateSetup("no-digest")
	_ = running

	// a development build has no digests, so this can only come from a direct record
	record := h.create(hostID, &agentv1.Command{Payload: &agentv1.Command_AgentUpdate{AgentUpdate: &agentv1.AgentUpdate{
		AgentImage:   testAgentImage + ":latest",
		UpdaterImage: testUpdaterImage + ":latest",
	}}})

	failed := h.waitForStatus(record.Id, schema.CommandFailed)
	if !strings.Contains(failed.GetString("error"), "not pinned by digest") {
		t.Errorf("error: %s", failed.GetString("error"))
	}
	if h.docker.Called("/images/create") {
		t.Error("nothing should have been pulled")
	}
}

func TestBatchUpdatesGoOneHostAtATime(t *testing.T) {
	pinRelease(t)

	h := newHarness(t)
	_, adminToken := testutil.NewUser(t, h.app, "admin@example.com", schema.RoleAdmin)

	first, firstID := h.updateSetup("batch-a")
	second, secondID := h.updateSetup("batch-b")
	third, thirdID := h.updateSetup("batch-c")

	// agents in this process report the same version as the panel: stop them, and
	// make the panel believe that they run an older one
	for _, running := range []*runningAgent{first, second, third} {
		if err := running.stop(); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{firstID, secondID, thirdID} {
		host := h.host(id)
		host.Set("agent_version", "1.0.0")
		if err := h.app.Save(host); err != nil {
			t.Fatal(err)
		}
	}

	status, body := testutil.Do(t, h.handler, testutil.Request{Method: "POST", URL: "/api/pc/hosts/update-all", Token: adminToken})
	if status != http.StatusOK || !strings.Contains(body, `"hosts":3`) {
		t.Fatalf("%d %s", status, body)
	}

	updates := func(hostID string) int {
		records, err := h.app.FindRecordsByFilter(schema.Commands, "host={:h} && type='agent.update'", "", 0, 0, dbx.Params{"h": hostID})
		if err != nil {
			t.Fatal(err)
		}
		return len(records)
	}

	if updates(firstID) != 1 || updates(secondID) != 0 || updates(thirdID) != 0 {
		t.Fatalf("only the first host may start: %d %d %d", updates(firstID), updates(secondID), updates(thirdID))
	}

	records, err := h.app.FindRecordsByFilter(schema.Commands, "host={:h} && type='agent.update'", "", 1, 0, dbx.Params{"h": firstID})
	if err != nil || len(records) != 1 {
		t.Fatal(err)
	}

	// the first one fails: the batch stops there
	if err := h.panel.Commands.Cancel(records[0].Id, "test"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	if updates(secondID) != 0 {
		t.Fatal("a failed update must stop the batch")
	}
}
