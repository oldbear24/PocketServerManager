// Command fakecompose stands in for the Docker command line tool with its Compose
// plugin, for tests and for working on a machine without Docker. It reads Compose
// files the way the real one does and creates the containers on the Engine API at
// DOCKER_HOST, which is usually the in-memory engine of cmd/fakedocker.
//
// Point an agent at it with DOCKER_CLI=/path/to/fakecompose.
//
// It proves that the agent calls Compose as intended and that the panel, the agent
// and the UI work together. It says nothing about the real Compose.
//
// FAKE_COMPOSE_LOG names a file that gets one JSON line per call, FAKE_COMPOSE_FAIL
// names a verb that fails, and FAKE_COMPOSE_WAIT names a file that `up` waits for
// after its first line of output.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
)

const (
	labelProject = "com.docker.compose.project"
	labelService = "com.docker.compose.service"
	labelFiles   = "com.docker.compose.project.config_files"
	labelDir     = "com.docker.compose.project.working_dir"
	labelHash    = "com.docker.compose.config-hash"

	waitLimit = 30 * time.Second
)

// call is what a run was asked to do.
type call struct {
	Args       []string `json:"args"`
	Verb       string   `json:"verb"`
	Project    string   `json:"project"`
	Directory  string   `json:"directory,omitempty"`
	Files      []string `json:"files,omitempty"`
	EnvFile    string   `json:"env_file,omitempty"`
	EnvContent string   `json:"env_content,omitempty"`
	VerbArgs   []string `json:"verb_args,omitempty"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fakecompose:", err)
		os.Exit(1)
	}
}

func parse(args []string) (*call, error) {
	if len(args) == 0 || args[0] != "compose" {
		return nil, errors.New("only `docker compose` is known here")
	}

	parsed := &call{Args: args}

	rest := args[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "--") {
		if len(rest) < 2 {
			return nil, fmt.Errorf("%s needs a value", rest[0])
		}
		switch rest[0] {
		case "--project-name":
			parsed.Project = rest[1]
		case "--project-directory":
			parsed.Directory = rest[1]
		case "--file":
			parsed.Files = append(parsed.Files, rest[1])
		case "--env-file":
			parsed.EnvFile = rest[1]
		default:
			return nil, fmt.Errorf("unknown option %s", rest[0])
		}
		rest = rest[2:]
	}

	if len(rest) == 0 {
		return nil, errors.New("no command")
	}

	parsed.Verb = rest[0]
	parsed.VerbArgs = rest[1:]

	return parsed, nil
}

func run(args []string) error {
	parsed, err := parse(args)
	if err != nil {
		return err
	}

	if parsed.EnvFile != "" {
		raw, err := os.ReadFile(parsed.EnvFile)
		if err != nil {
			return fmt.Errorf("couldn't find env file: %w", err)
		}
		parsed.EnvContent = string(raw)
	}

	if err := writeLog(parsed); err != nil {
		return err
	}

	if os.Getenv("FAKE_COMPOSE_FAIL") == parsed.Verb {
		fmt.Fprintln(os.Stderr, "Error response from daemon: the fake was told to fail")
		return fmt.Errorf("%s failed", parsed.Verb)
	}

	if parsed.Verb == "version" {
		fmt.Println("2.29.7-fake")
		return nil
	}

	ctx := context.Background()

	client, err := dockerapi.New(os.Getenv("DOCKER_HOST"))
	if err != nil {
		return err
	}

	switch parsed.Verb {
	case "config":
		_, err := load(ctx, parsed)
		return err
	case "pull":
		return pull(ctx, client, parsed)
	case "up":
		return up(ctx, client, parsed)
	case "stop":
		return forEachContainer(ctx, client, parsed, "Stopped", func(id string) error { return client.Stop(ctx, id, 0) })
	case "restart":
		return forEachContainer(ctx, client, parsed, "Restarted", func(id string) error { return client.Restart(ctx, id, 0) })
	case "down":
		return forEachContainer(ctx, client, parsed, "Removed", func(id string) error { return client.Remove(ctx, id, false, true) })
	}

	return fmt.Errorf("unknown command %s", parsed.Verb)
}

func writeLog(parsed *call) error {
	path := os.Getenv("FAKE_COMPOSE_LOG")
	if path == "" {
		return nil
	}

	raw, err := json.Marshal(parsed)
	if err != nil {
		return err
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // the test that runs this tool names the file
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = file.Write(append(raw, '\n'))

	return err
}

// load reads the project like Compose does: the environment of the process wins
// over the env file.
func load(ctx context.Context, parsed *call) (*types.Project, error) {
	if len(parsed.Files) == 0 {
		return nil, errors.New("no configuration file provided")
	}

	environment, err := dotenv.UnmarshalWithLookup(parsed.EnvContent, nil)
	if err != nil {
		return nil, fmt.Errorf("the env file: %w", err)
	}
	for _, variable := range os.Environ() {
		if name, value, found := strings.Cut(variable, "="); found {
			environment[name] = value
		}
	}

	files := make([]types.ConfigFile, 0, len(parsed.Files))
	for _, path := range parsed.Files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		files = append(files, types.ConfigFile{Filename: path, Content: raw})
	}

	return loader.LoadWithContext(ctx, types.ConfigDetails{
		WorkingDir:  parsed.Directory,
		ConfigFiles: files,
		Environment: environment,
	}, func(options *loader.Options) {
		options.SetProjectName(parsed.Project, true)
		options.ResolvePaths = false
		options.SkipExtends = true
		options.SkipInclude = true
	})
}

func pull(ctx context.Context, client *dockerapi.Client, parsed *call) error {
	project, err := load(ctx, parsed)
	if err != nil {
		return err
	}

	for _, name := range slices.Sorted(maps.Keys(project.Services)) {
		image := project.Services[name].Image
		fmt.Printf(" Image %s Pulling\n", image)
		if err := client.Pull(ctx, image, nil, nil); err != nil {
			return err
		}
		fmt.Printf(" Image %s Pulled\n", image)
	}

	return nil
}

func up(ctx context.Context, client *dockerapi.Client, parsed *call) error {
	project, err := load(ctx, parsed)
	if err != nil {
		return err
	}

	existing, err := containersOf(ctx, client, parsed.Project)
	if err != nil {
		return err
	}

	fmt.Printf(" Network %s_default Creating\n", parsed.Project)
	if err := waitIfAsked(); err != nil {
		return err
	}

	for _, name := range slices.Sorted(maps.Keys(project.Services)) {
		service := project.Services[name]

		hash := configHash(service)

		var current *dockerapi.Container
		for i := range existing {
			if existing[i].Labels[labelService] == name {
				current = &existing[i]
			}
		}

		containerName := service.ContainerName
		if containerName == "" {
			containerName = parsed.Project + "-" + name + "-1"
		}

		if current != nil && current.Labels[labelHash] == hash {
			if current.State != "running" {
				if err := client.Start(ctx, current.ID); err != nil {
					return err
				}
			}
			fmt.Printf(" Container %s Running\n", containerName)
			continue
		}

		if current != nil {
			fmt.Printf(" Container %s Recreate\n", containerName)
			if err := client.Remove(ctx, current.ID, false, true); err != nil {
				return err
			}
		}

		if err := client.Pull(ctx, service.Image, nil, nil); err != nil {
			return err
		}

		labels := map[string]string{
			labelProject: parsed.Project,
			labelService: name,
			labelHash:    hash,
			labelFiles:   strings.Join(parsed.Files, ","),
			labelDir:     parsed.Directory,
		}
		maps.Copy(labels, service.Labels)

		config := map[string]any{
			"Image":  service.Image,
			"Labels": labels,
			"Env":    environmentOf(service),
		}
		if service.HealthCheck != nil && !service.HealthCheck.Disable {
			config["Healthcheck"] = map[string]any{"Test": service.HealthCheck.Test}
		}
		if len(service.Command) > 0 {
			config["Cmd"] = []string(service.Command)
		}

		id, err := client.Create(ctx, containerName, config)
		if err != nil {
			return err
		}

		for _, network := range slices.Sorted(maps.Keys(service.Networks)) {
			if project.Networks[network].External {
				var endpoint map[string]any
				if config := service.Networks[network]; config != nil && len(config.Aliases) > 0 {
					endpoint = map[string]any{"Aliases": config.Aliases}
				}
				err := client.ConnectNetwork(ctx, project.Networks[network].Name, id, endpoint)
				if err != nil {
					return fmt.Errorf("network %s declared as external, but could not be found: %w", network, err)
				}
			}
		}

		if err := client.Start(ctx, id); err != nil {
			return err
		}

		fmt.Printf(" Container %s Started\n", containerName)
	}

	if slices.Contains(parsed.VerbArgs, "--remove-orphans") {
		for _, container := range existing {
			if _, known := project.Services[container.Labels[labelService]]; known {
				continue
			}
			// like the real one, a service of a profile nobody activated is no orphan
			if _, disabled := project.DisabledServices[container.Labels[labelService]]; disabled {
				continue
			}
			fmt.Printf(" Container %s Removed\n", strings.TrimPrefix(container.Names[0], "/"))
			if err := client.Remove(ctx, container.ID, false, true); err != nil {
				return err
			}
		}
	}

	return nil
}

// waitIfAsked lets a test watch a deploy that is still running.
func waitIfAsked() error {
	path := os.Getenv("FAKE_COMPOSE_WAIT")
	if path == "" {
		return nil
	}

	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil { //nolint:gosec // the test that runs this tool names the file
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}

	return errors.New("waited too long")
}

func environmentOf(service types.ServiceConfig) []string {
	values := []string{}
	for _, name := range slices.Sorted(maps.Keys(service.Environment)) {
		value := ""
		if pointer := service.Environment[name]; pointer != nil {
			value = *pointer
		}
		values = append(values, name+"="+value)
	}
	return values
}

func configHash(service types.ServiceConfig) string {
	raw, err := json.Marshal(map[string]any{
		"image":    service.Image,
		"env":      environmentOf(service),
		"labels":   service.Labels,
		"networks": slices.Sorted(maps.Keys(service.Networks)),
	})
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func containersOf(ctx context.Context, client *dockerapi.Client, project string) ([]dockerapi.Container, error) {
	all, err := client.Containers(ctx)
	if err != nil {
		return nil, err
	}

	result := []dockerapi.Container{}
	for _, container := range all {
		if container.Labels[labelProject] == project {
			result = append(result, container)
		}
	}

	return result, nil
}

// forEachContainer acts on the containers of a project. Like the real one, it
// leaves alone those of services in a profile nobody activated.
func forEachContainer(ctx context.Context, client *dockerapi.Client, parsed *call, done string, action func(id string) error) error {
	containers, err := containersOf(ctx, client, parsed.Project)
	if err != nil {
		return err
	}

	disabled := types.Services{}
	if len(parsed.Files) > 0 {
		project, err := load(ctx, parsed)
		if err != nil {
			return err
		}
		disabled = project.DisabledServices
	}

	// the services named after the verb, or all of them
	named := []string{}
	for _, arg := range parsed.VerbArgs {
		if !strings.HasPrefix(arg, "-") {
			named = append(named, arg)
		}
	}

	for _, container := range containers {
		service := container.Labels[labelService]
		if _, skip := disabled[service]; skip {
			continue
		}
		if len(named) > 0 && !slices.Contains(named, service) {
			continue
		}
		if err := action(container.ID); err != nil {
			return err
		}
		fmt.Printf(" Container %s %s\n", strings.TrimPrefix(container.Names[0], "/"), done)
	}

	return nil
}
