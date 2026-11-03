// Command pocketcloud-updater swaps the agent container for one running a new
// image. The agent starts it in a temporary container with the Docker socket and
// the agent volume mounted; it does one update, leaves the outcome in the volume
// and exits.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
	"github.com/oldbear24/PocketServerManager/internal/updater"
	"github.com/oldbear24/PocketServerManager/internal/version"
)

const (
	defaultTimeout  = 180 * time.Second
	defaultStateDir = "/var/lib/pocketcloud"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version.Version)
		return
	}

	options := updater.Options{
		Target:        os.Getenv("TARGET_CONTAINER"),
		Image:         os.Getenv("NEW_IMAGE"),
		CommandID:     os.Getenv("COMMAND_ID"),
		Version:       os.Getenv("NEW_VERSION"),
		StateDir:      os.Getenv("STATE_DIR"),
		HealthTimeout: defaultTimeout,
	}
	if options.StateDir == "" {
		options.StateDir = defaultStateDir
	}
	if seconds, err := strconv.Atoi(os.Getenv("HEALTH_TIMEOUT_S")); err == nil && seconds > 0 {
		options.HealthTimeout = time.Duration(seconds) * time.Second
	}

	if options.Target == "" || options.Image == "" || options.CommandID == "" {
		slog.Error("TARGET_CONTAINER, NEW_IMAGE and COMMAND_ID are required")
		os.Exit(2)
	}

	docker, err := dockerapi.New(os.Getenv("DOCKER_HOST"))
	if err != nil {
		slog.Error("cannot use Docker", "error", err)
		os.Exit(2)
	}

	outcome := updater.Run(context.Background(), docker, options)

	if err := updater.WriteOutcome(options.StateDir, outcome); err != nil {
		slog.Error("cannot store the outcome", "error", err)
	}

	if !outcome.Succeeded {
		slog.Error("the update failed", "step", outcome.Step, "error", outcome.Error, "rolled_back", outcome.RolledBack)
		os.Exit(1)
	}

	slog.Info("the update succeeded", "image", options.Image)
}
