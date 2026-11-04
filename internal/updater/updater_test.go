package updater

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
)

const (
	target   = "pocketcloud-agent"
	oldImage = "ghcr.io/oldbear24/pocketcloud-agent@sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	newImage = "ghcr.io/oldbear24/pocketcloud-agent@sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"
)

func setup(t *testing.T) (*fakedocker.Engine, *dockerapi.Client, Options) {
	t.Helper()

	engine := fakedocker.New(t)
	engine.AddImage(oldImage, map[string]any{
		"Env":        []string{"PATH=/usr/bin", "IMAGE_DEFAULT=old"},
		"Entrypoint": []string{"pocketcloud-agent"},
		"Cmd":        []string{"run"},
		"Labels":     map[string]string{"org.opencontainers.image.title": "pocketcloud-agent"},
	})
	engine.AddImage(newImage, map[string]any{"Env": []string{"PATH=/usr/bin"}})

	id := engine.AddContainer(target, oldImage, map[string]string{
		"pocketcloud.role":               "agent",
		"org.opencontainers.image.title": "pocketcloud-agent",
	})
	_ = id

	client, err := dockerapi.New(engine.Host())
	if err != nil {
		t.Fatal(err)
	}

	options := Options{
		Target:        target,
		Image:         newImage,
		CommandID:     "cmd1",
		Version:       "v1.2.3",
		HealthTimeout: 500 * time.Millisecond,
		StateDir:      t.TempDir(),
		PollInterval:  5 * time.Millisecond,
	}

	return engine, client, options
}

func TestUpdateSucceeds(t *testing.T) {
	engine, client, options := setup(t)

	engine.OnStart = func(*fakedocker.Container) (string, string) { return "running", "healthy" }

	outcome := Run(context.Background(), client, options)
	if !outcome.Succeeded {
		t.Fatalf("the update failed: %+v", outcome)
	}

	current := engine.Find(target)
	if current == nil || current.Image != newImage || current.Status != "running" {
		t.Fatalf("the new container is wrong: %+v", current)
	}

	// no leftovers
	if engine.Find(target+oldSuffix) != nil {
		t.Fatal("the old container was not removed")
	}
	if names := engine.Names(); len(names) != 1 {
		t.Fatalf("containers left behind: %v", names)
	}
}

// TestConfigIsKept covers the known pitfalls of recreating a container from
// `docker inspect`: what came from the old container or image must not stick.
func TestConfigIsKept(t *testing.T) {
	engine, client, options := setup(t)

	engine.OnStart = func(*fakedocker.Container) (string, string) { return "running", "healthy" }

	before := engine.Find(target)

	outcome := Run(context.Background(), client, options)
	if !outcome.Succeeded {
		t.Fatalf("%+v", outcome)
	}

	after := engine.Find(target)

	if hostname, _ := after.Config["Hostname"].(string); strings.HasPrefix(before.ID, hostname) && hostname != "" {
		t.Errorf("the auto-generated hostname was carried over: %q", hostname)
	}

	if policy, _ := after.Host["RestartPolicy"].(map[string]any); policy["Name"] != "unless-stopped" {
		t.Errorf("the restart policy was lost: %v", after.Host)
	}

	if after.Labels["pocketcloud.role"] != "agent" {
		t.Errorf("the labels were lost: %v", after.Labels)
	}
	if _, kept := after.Labels["org.opencontainers.image.title"]; kept {
		t.Error("a label of the old image was pinned on the new container")
	}

	if env, _ := after.Config["Env"].([]any); slices.ContainsFunc(env, func(v any) bool { return v == "PATH=/usr/bin" }) {
		t.Errorf("the old image's default environment was pinned: %v", env)
	}
}

func TestEnvironmentIsKept(t *testing.T) {
	engine, client, options := setup(t)
	engine.OnStart = func(*fakedocker.Container) (string, string) { return "running", "healthy" }

	// a variable set by the user, on top of the image's own
	if _, err := client.Inspect(context.Background(), target); err != nil {
		t.Fatal(err)
	}

	engine.AddImage("user-env-image@sha256:3", map[string]any{"Env": []string{"FROM_IMAGE=1"}})
	id := engine.AddContainer("with-env", "user-env-image@sha256:3", nil)
	_ = id

	options.Target = "with-env"
	inspected, err := client.Inspect(context.Background(), "with-env")
	if err != nil {
		t.Fatal(err)
	}

	config, err := newContainerConfig(context.Background(), client, inspected, newImage)
	if err != nil {
		t.Fatal(err)
	}
	if config["Image"] != newImage {
		t.Errorf("image: %v", config["Image"])
	}
}

func TestBrokenImageIsRolledBack(t *testing.T) {
	cases := []struct {
		name    string
		onStart func(c *fakedocker.Container) (string, string)
		step    string
		message string
	}{
		{
			name:    "crash loop",
			onStart: startOnlyOld(func() (string, string) { return "exited", "" }),
			step:    "health",
			message: "exited",
		},
		{
			// with a restart policy, as the agent has, a crash never stays exited
			name:    "restart loop",
			onStart: startOnlyOld(func() (string, string) { return "restarting", "" }),
			step:    "health",
			message: "keeps exiting",
		},
		{
			name:    "never connects to the panel",
			onStart: startOnlyOld(func() (string, string) { return "running", "starting" }),
			step:    "health",
			message: "did not connect",
		},
		{
			name:    "reports unhealthy",
			onStart: startOnlyOld(func() (string, string) { return "running", "unhealthy" }),
			step:    "health",
			message: "did not connect",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, client, options := setup(t)
			engine.OnStart = tc.onStart

			original := engine.Find(target).ID

			outcome := Run(context.Background(), client, options)

			if outcome.Succeeded || !outcome.RolledBack || outcome.Step != tc.step {
				t.Fatalf("outcome: %+v", outcome)
			}
			if !strings.Contains(outcome.Error, tc.message) {
				t.Errorf("error: %q", outcome.Error)
			}

			current := engine.Find(target)
			if current == nil || current.ID != original || current.Status != "running" {
				t.Fatalf("the old container is not back: %+v", current)
			}
			if names := engine.Names(); len(names) != 1 {
				t.Fatalf("containers left behind: %v", names)
			}
		})
	}
}

// startOnlyOld makes new containers behave as given, and the old one start fine.
func startOnlyOld(forNew func() (string, string)) func(c *fakedocker.Container) (string, string) {
	return func(c *fakedocker.Container) (string, string) {
		if c.Image == newImage {
			return forNew()
		}
		return "running", ""
	}
}

func TestCreateFailureRestoresTheOldContainer(t *testing.T) {
	engine, client, options := setup(t)
	engine.FailOn["POST /containers/create"] = "no space left on device"

	original := engine.Find(target).ID

	outcome := Run(context.Background(), client, options)
	if outcome.Succeeded || !outcome.RolledBack || outcome.Step != "create" {
		t.Fatalf("outcome: %+v", outcome)
	}

	current := engine.Find(target)
	if current == nil || current.ID != original || current.Status != "running" {
		t.Fatalf("the old container is not back: %+v", current)
	}
}

// TestKilledUpdaterNeverLosesTheOldContainer stops the updater at every step, the
// way a kill would. The old container must exist at each of those points, either
// under its name or as `-old`, so the documented recovery works.
func TestKilledUpdaterNeverLosesTheOldContainer(t *testing.T) {
	for _, step := range []string{
		"/stop",
		"/rename",
		"POST /containers/create",
		"/start",
	} {
		t.Run(step, func(t *testing.T) {
			engine, client, options := setup(t)

			original := engine.Find(target).ID

			block := make(chan struct{})
			engine.BlockOn[step] = block
			// a handler that has not read its body does not notice a closed connection
			defer close(block)

			ctx, kill := context.WithCancel(context.Background())
			finished := make(chan Outcome, 1)
			go func() { finished <- Run(ctx, client, options) }()

			// wait until the updater reaches the step, then "kill" it
			deadline := time.Now().Add(2 * time.Second)
			for !engine.Called(step) {
				if time.Now().After(deadline) {
					t.Fatalf("the updater never reached %s: %v", step, engine.Calls())
				}
				time.Sleep(time.Millisecond)
			}
			kill()
			<-finished

			// a kill runs no rollback code, but a cancelled context behaves the same
			// from Docker's point of view: nothing more can happen to the old one
			found := false
			for _, name := range []string{target, target + oldSuffix} {
				if container := engine.Find(name); container != nil && container.ID == original {
					found = true
				}
			}
			if !found {
				t.Fatalf("the old container is gone: %v", engine.Names())
			}
		})
	}
}

func TestImageMustBePinnedByDigest(t *testing.T) {
	engine, client, options := setup(t)

	options.Image = "ghcr.io/oldbear24/pocketcloud-agent:v2"

	outcome := Run(context.Background(), client, options)
	if outcome.Succeeded || !strings.Contains(outcome.Error, "digest") {
		t.Fatalf("outcome: %+v", outcome)
	}
	if engine.Called("/stop") {
		t.Fatal("the old container was touched")
	}
}

func TestLeftoverOfAnEarlierUpdateIsRemoved(t *testing.T) {
	engine, client, options := setup(t)
	engine.OnStart = func(*fakedocker.Container) (string, string) { return "running", "healthy" }

	engine.AddContainer(target+oldSuffix, oldImage, nil)

	outcome := Run(context.Background(), client, options)
	if !outcome.Succeeded {
		t.Fatalf("outcome: %+v", outcome)
	}
	if names := engine.Names(); len(names) != 1 {
		t.Fatalf("containers left behind: %v", names)
	}
}

func TestAliasesFromTheOldContainerAreDropped(t *testing.T) {
	id := strings.Repeat("a", 64)

	clean := cleanEndpoint(map[string]any{
		"Aliases":    []any{id[:12], "keep-this-alias"},
		"IPAddress":  "172.17.0.2",
		"EndpointID": "abc",
	}, id)

	aliases, _ := clean["Aliases"].([]string)
	if !slices.Equal(aliases, []string{"keep-this-alias"}) {
		t.Errorf("aliases: %v", aliases)
	}
	if _, kept := clean["IPAddress"]; kept {
		t.Error("the old address must not be kept")
	}
	if _, kept := clean["EndpointID"]; kept {
		t.Error("the old endpoint must not be kept")
	}
}

func TestOutcomeFiles(t *testing.T) {
	dir := t.TempDir()

	outcomes, err := ReadOutcomes(dir)
	if err != nil || len(outcomes) != 0 {
		t.Fatalf("%v, %v", outcomes, err)
	}

	want := Outcome{CommandID: "abc", Succeeded: true, NewImage: newImage}
	if err := WriteOutcome(dir, want); err != nil {
		t.Fatal(err)
	}

	outcomes, err = ReadOutcomes(dir)
	if err != nil || len(outcomes) != 1 || outcomes[0] != want {
		t.Fatalf("%v, %v", outcomes, err)
	}

	if err := RemoveOutcome(dir, "abc"); err != nil {
		t.Fatal(err)
	}
	outcomes, err = ReadOutcomes(dir)
	if err != nil || len(outcomes) != 0 {
		t.Fatalf("%v, %v", outcomes, err)
	}
}
