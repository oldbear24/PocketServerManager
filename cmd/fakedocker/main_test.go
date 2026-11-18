package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/oldbear24/PocketServerManager/internal/databases"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
)

// TestTheEngineAnswersTheDatabaseServersScripts (#53): the development
// Engine answers what the agent runs in a database server's container with
// the fake servers: the databases a statement creates are in the server's
// numbers, and a database's dump names it.
func TestTheEngineAnswersTheDatabaseServersScripts(t *testing.T) {
	engine := fakedocker.New(t)
	servers := seed(engine)
	db := servers.AddServer("pcdb-main-db-1", "postgres")
	client, err := dockerapi.New(engine.Host())
	if err != nil {
		t.Fatal(err)
	}
	run := func(script databases.Script) string {
		t.Helper()
		var out bytes.Buffer
		result, err := client.Run(context.Background(), db, dockerapi.ExecOptions{Cmd: script.Cmd, Env: script.Env}, &out)
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("%q: %v %+v", script.Cmd, err, result)
		}
		return out.String()
	}
	run(databases.Statements(databases.Postgres, "postgres", "pw", databases.CreateSQL(databases.Postgres, "shop", "shop", "shop-pw")))
	stats, err := databases.ParseStats(databases.Postgres, run(databases.Statements(databases.Postgres, "postgres", "pw", databases.StatsSQL(databases.Postgres))))
	if err != nil || len(stats.Databases) != 1 || stats.Databases["shop"].SizeBytes == 0 {
		t.Errorf("the server's numbers: %+v %v", stats, err)
	}
	command, err := databases.DumpCommand(databases.Postgres, "postgres", "shop")
	if err != nil {
		t.Fatal(err)
	}
	if dump := run(databases.Script{Cmd: command}); !strings.HasPrefix(dump, "PGDMP") || !strings.Contains(dump, "dbname: shop") {
		t.Errorf("the dump: %q", dump)
	}
	if engine.Find("umami-db") == nil || engine.Find("pocketcloud-agent") == nil {
		t.Errorf("the example containers: %q", engine.Names())
	}
}
