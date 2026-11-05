// Command pocketcloud-agent runs on every managed server, in a container.
//
// Usage inside the container:
//
//	pocketcloud-agent run           the entrypoint: enroll if needed and stay connected
//	pocketcloud-agent enroll        enroll now and exit
//	pocketcloud-agent healthcheck   exit 0 only while connected to the panel
//	pocketcloud-agent diagnostics   print connectivity, certificate and Docker status
//	pocketcloud-agent version       print the version
//	pocketcloud-agent site-tool     change the sites volume, in a site's job container
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/oldbear24/PocketServerManager/internal/agent"
	"github.com/oldbear24/PocketServerManager/internal/version"
)

func main() {
	command := "run"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	config := agent.ConfigFromEnv()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error

	switch command {
	case "run":
		err = run(ctx, config)
	case "enroll":
		err = enroll(ctx, config)
	case "healthcheck":
		err = agent.Healthy(config)
	case "diagnostics":
		err = diagnostics(ctx, config, len(os.Args) > 2 && os.Args[2] == "--json")
	case "version":
		fmt.Println(version.Version)
	case "site-tool":
		err = agent.SiteTool(ctx, os.Args[2:], map[string]string{"GIT_TOKEN": os.Getenv("GIT_TOKEN")}, os.Stdout, os.Stderr)
	default:
		err = fmt.Errorf("unknown command %q: use run, enroll, healthcheck, diagnostics or version", command)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, config agent.Config) error {
	a, err := agent.New(config, os.Stdout)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}

func enroll(ctx context.Context, config agent.Config) error {
	a, err := agent.New(config, os.Stdout)
	if err != nil {
		return err
	}

	identity, err := agent.Enroll(ctx, config, nil)
	if err != nil {
		return err
	}

	a.Log.Info("the agent enrolled", "host", identity.Name, "id", identity.HostID)

	return nil
}

func diagnostics(ctx context.Context, config agent.Config, asJSON bool) error {
	a, err := agent.New(config, os.Stderr)
	if err != nil {
		return err
	}

	report := a.BuildReport(ctx)

	if asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}

	agent.WriteReportText(os.Stdout, report)

	return nil
}
