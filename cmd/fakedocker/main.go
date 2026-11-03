// Command fakedocker runs an in-memory Docker Engine, for working on the panel and
// the agent on a machine without Docker. It starts with a few example containers;
// point an agent at it with DOCKER_HOST=tcp://127.0.0.1:2399.
//
// It only speaks what the agent's Docker client uses. For `docker compose`, point
// the agent at cmd/fakecompose with DOCKER_CLI.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
)

// sampleLines is what the running containers write, so that log views have something to show.
var sampleLines = []struct{ stream, text string }{
	{"stdout", "INFO  POST /api/send 200 3ms"},
	{"stdout", "INFO  GET /api/websites/3f1c/stats 200 41ms"},
	{"stdout", "INFO  POST /api/send 200 4ms"},
	{"stderr", "WARN  slow query getSessionStats 812ms"},
	{"stdout", "INFO  GET /script.js 304 1ms"},
	{"stdout", "LOG   checkpoint starting: time"},
	{"stdout", "LOG   checkpoint complete: wrote 42 buffers (0.3%); write=4.1 s"},
	{"stderr", "ERROR client disconnected before response (ECONNRESET)"},
}

func main() {
	addr := flag.String("addr", "127.0.0.1:2399", "address to listen on")
	chatter := flag.Duration("chatter", 2*time.Second, "how often running containers write a sample log line; 0 turns it off")
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

	if *chatter > 0 {
		go writeSampleLogs(engine, *chatter)
	}

	fmt.Printf("fake Docker Engine on tcp://%s; press Ctrl+C to stop\n", *addr)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
}

// writeSampleLogs makes every running container write a line now and then.
func writeSampleLogs(engine *fakedocker.Engine, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for turn := 0; ; turn++ {
		<-ticker.C
		for i, name := range engine.Names() {
			if container := engine.Find(name); container == nil || container.Status != "running" {
				continue
			}
			line := sampleLines[(turn+i)%len(sampleLines)]
			engine.WriteLog(name, line.stream, line.text)
		}
	}
}
