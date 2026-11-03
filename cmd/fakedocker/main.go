// Command fakedocker runs an in-memory Docker Engine, for working on the panel and
// the agent on a machine without Docker. It starts with a few example containers;
// point an agent at it with DOCKER_HOST=tcp://127.0.0.1:2399.
//
// It only speaks what the agent's Docker client uses: `docker compose` needs a
// real engine.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:2399", "address to listen on")
	flag.Parse()

	engine, err := fakedocker.Listen(*addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer engine.Close()

	engine.AddContainer("pocketcloud-agent", "ghcr.io/oldbear24/pocketcloud-agent:dev", map[string]string{"pocketcloud.role": "agent"})
	engine.AddContainer("caddy-edge", "caddy:2.8-alpine", map[string]string{"pocketcloud.role": "edge"})
	engine.AddContainer("umami", "ghcr.io/umami-software/umami:postgresql-v2", map[string]string{
		"com.docker.compose.project": "analytics",
		"com.docker.compose.service": "umami",
	})
	engine.AddContainer("umami-db", "postgres:16-alpine", map[string]string{
		"com.docker.compose.project": "analytics",
		"com.docker.compose.service": "db",
	})
	engine.AddContainer("vaultwarden", "vaultwarden/server:1.32.0", map[string]string{
		"com.docker.compose.project": "vault",
		"com.docker.compose.service": "vaultwarden",
	})
	stopped := engine.AddContainer("old-wiki", "requarks/wiki:2", map[string]string{
		"com.docker.compose.project": "wiki",
		"com.docker.compose.service": "wiki",
	})
	engine.SetStatus(stopped, "exited")

	engine.AddVolume("pocketcloud-agent")
	engine.AddVolume("umami-db-data")
	engine.AddVolume("vaultwarden-data")

	fmt.Printf("fake Docker Engine on tcp://%s; press Ctrl+C to stop\n", *addr)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
}
