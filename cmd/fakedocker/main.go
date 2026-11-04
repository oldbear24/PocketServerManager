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
	"maps"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"sync"
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

	engine.OnExec = (&databaseServers{engine: engine, databases: map[string]map[string]bool{}}).answer

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

// databaseServers answers the scripts an agent runs in database containers, as
// far as the panel's pages need it: the databases that statements created and
// dropped, with made-up sizes. Nothing is stored for real.
type databaseServers struct {
	engine    *fakedocker.Engine
	mu        sync.Mutex
	databases map[string]map[string]bool
}

// initialDatabases are what an official image creates on its first start, from
// the container's environment.
func (s *databaseServers) initialDatabases(id string) map[string]bool {
	known := map[string]bool{}
	for _, name := range s.engine.Names() {
		container := s.engine.Find(name)
		if container == nil || container.ID != id {
			continue
		}
		env, _ := container.Config["Env"].([]any)
		values := map[string]string{}
		for _, item := range env {
			key, value, _ := strings.Cut(fmt.Sprint(item), "=")
			values[key] = value
		}
		for _, key := range []string{"POSTGRES_DB", "MARIADB_DATABASE", "MYSQL_DATABASE"} {
			if values[key] != "" {
				known[values[key]] = true
			}
		}
		if values["POSTGRES_DB"] == "" && values["POSTGRES_USER"] != "" {
			known[values["POSTGRES_USER"]] = true
		}
	}
	return known
}

var (
	createDatabase = regexp.MustCompile("CREATE DATABASE (?:IF NOT EXISTS )?[`\"]?([a-z0-9_]+)")
	dropDatabase   = regexp.MustCompile("DROP DATABASE IF EXISTS [`\"]?([a-z0-9_]+)")
)

func (s *databaseServers) answer(call *fakedocker.ExecCall, _ []byte) fakedocker.ExecResult {
	env := map[string]string{}
	for _, item := range call.Env {
		key, value, _ := strings.Cut(item, "=")
		env[key] = value
	}

	// the engine's lock is not held here, but finding the container takes it
	seeded := s.initialDatabases(call.Container)

	s.mu.Lock()
	defer s.mu.Unlock()

	known := s.databases[call.Container]
	if known == nil {
		known = seeded
		s.databases[call.Container] = known
	}

	sql := env["PC_SQL"]
	switch {
	case strings.Contains(sql, "'version'"):
		var out strings.Builder
		for index, name := range slices.Sorted(maps.Keys(known)) {
			fmt.Fprintf(&out, "size\t%s\t%d\nconn\t%s\t%d\n", name, (index+3)*37<<20, name, index+2)
		}
		version := "11.8.2-MariaDB"
		if _, postgres := env["PGPASSWORD"]; postgres {
			version = "17.2"
		}
		fmt.Fprintf(&out, "max\t\t%d\nversion\t%s\t0\n", map[bool]int{true: 100, false: 200}[version == "17.2"], version)
		return fakedocker.ExecResult{Stdout: out.String()}
	case sql != "":
		if match := createDatabase.FindStringSubmatch(sql); match != nil {
			known[match[1]] = true
		}
		if match := dropDatabase.FindStringSubmatch(sql); match != nil {
			delete(known, match[1])
		}
		return fakedocker.ExecResult{}
	case env["PC_SOURCE"] != "":
		known[env["PC_TARGET"]] = true
		return fakedocker.ExecResult{}
	case env["PC_DATABASE"] != "":
		return fakedocker.ExecResult{Stdout: "-- a made-up dump of " + env["PC_DATABASE"] + "\nCREATE TABLE orders (id int);\n"}
	}

	return fakedocker.ExecResult{Code: 127, Stderr: "fakedocker: no such program"}
}
