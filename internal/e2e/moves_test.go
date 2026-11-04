package e2e

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

const analyticsFile = `services:
  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: shop
      POSTGRES_PASSWORD: ${DB_PASS}
  web:
    image: umami:2
    depends_on:
      - db
`

// linkedAnalytics deploys a stack with its own PostgreSQL, links the server and
// waits until its database is listed.
func (d *databaseHarness) linkedAnalytics() (stackID string, server *core.Record) {
	d.t.Helper()

	stackID = d.newStack("analytics", analyticsFile, "")
	status, body := d.post("/api/pc/stacks/"+stackID+"/save", d.operator, map[string]any{"secrets": map[string]any{"set": map[string]string{"DB_PASS": "app-secret-7"}}})
	if status != http.StatusOK {
		d.t.Fatalf("secrets: %d %s", status, body)
	}
	d.waitForStatus(d.deploy(stackID, map[string]any{}).Id, schema.CommandSucceeded)

	old := d.docker.Find("analytics-db-1")
	if old == nil {
		d.t.Fatal("the stack's database container is missing")
	}
	status, body = d.post("/api/pc/db-servers/link", d.operator, map[string]any{
		"host": d.host.Id, "docker_id": old.ID, "admin_user": "shop", "secret_key": "DB_PASS",
	})
	if status != http.StatusOK {
		d.t.Fatalf("link: %d %s", status, body)
	}
	server, err := d.app.FindRecordById(schema.DBServers, recordID(d.t, body))
	if err != nil {
		d.t.Fatal(err)
	}

	d.waitFor("the dedicated server's database to be listed", func() bool {
		_, err := d.app.FindFirstRecordByFilter(schema.Databases, "server={:server} && name='shop'", dbx.Params{"server": server.Id})
		return err == nil
	})
	return stackID, server
}

func TestADedicatedServerMovesIntoASharedOne(t *testing.T) {
	d := newDatabaseHarness(t)

	shared := d.createServer("pg-main", "postgres")
	stackID, dedicated := d.linkedAnalytics()

	// a viewer cannot, and a server does not move into itself
	status, body := d.post("/api/pc/db-servers/"+dedicated.Id+"/move", d.viewer, map[string]any{"target": shared.Id})
	if status != http.StatusForbidden {
		t.Errorf("a viewer moved a server: %d %s", status, body)
	}
	status, body = d.post("/api/pc/db-servers/"+dedicated.Id+"/move", d.operator, map[string]any{"target": dedicated.Id})
	if status != http.StatusBadRequest {
		t.Errorf("a move into itself: %d %s", status, body)
	}

	status, body = d.post("/api/pc/db-servers/"+dedicated.Id+"/move", d.operator, map[string]any{"target": shared.Id})
	if status != http.StatusOK {
		t.Fatalf("move: %d %s", status, body)
	}
	if strings.Contains(body, "app-secret-7") {
		t.Errorf("the answer holds the application's password: %s", body)
	}
	var answer struct {
		Move    struct{ ID string } `json:"move"`
		Command struct{ ID string } `json:"command"`
	}
	answerOf(t, body, &answer)

	// a second move of the same server waits for the first
	status, body = d.post("/api/pc/db-servers/"+dedicated.Id+"/move", d.operator, map[string]any{"target": shared.Id})
	if status != http.StatusBadRequest || !strings.Contains(body, "under way") {
		t.Errorf("a second move: %d %s", status, body)
	}

	d.waitForStatus(answer.Command.ID, schema.CommandSucceeded)

	var move *core.Record
	d.waitFor("the move to be recorded", func() bool {
		found, err := d.app.FindRecordById(schema.DBMoves, answer.Move.ID)
		move = found
		return err == nil && found.GetString("status") == schema.MoveDone
	})
	if move.GetString("secrets") != "" || move.GetString("source_service") != "db" || move.GetDateTime("remove_after").IsZero() {
		t.Errorf("move: %v", move)
	}

	// the data went from the old container to the shared server, with the
	// application's own user and password
	dumped, created, restored := false, false, false
	for _, exec := range d.server.execs {
		switch {
		case exec.container == d.docker.Find("analytics-db-1").ID && exec.env["PC_DATABASE"] == "shop" && strings.Contains(strings.Join(exec.cmd, " "), "pg_dump"):
			dumped = true
		case exec.container == d.docker.Find("pg-main-db-1").ID && strings.Contains(exec.env["PC_SQL"], "CREATE DATABASE"):
			created = strings.Contains(exec.env["PC_SQL"], `"shop"`) && strings.Contains(exec.env["PC_SQL"], "'app-secret-7'")
		case exec.container == d.docker.Find("pg-main-db-1").ID && exec.env["PC_FILE"] != "":
			restored = exec.env["PC_OWNER"] == "shop" && len(d.docker.File("pg-main-db-1", exec.env["PC_FILE"])) > 0
		}
	}
	if !dumped || !created || !restored {
		t.Errorf("dumped %v, created %v, restored %v", dumped, created, restored)
	}

	// the panel knows the database on its new server, and the old server no more
	database, err := d.app.FindFirstRecordByFilter(schema.Databases, "name='shop'", nil)
	if err != nil || database.GetString("server") != shared.Id || database.GetString("stack") != stackID {
		t.Errorf("database: %v %v", database, err)
	}
	if _, err := d.app.FindRecordById(schema.DBServers, dedicated.Id); err == nil {
		t.Error("the old server is still listed")
	}
	account := d.user(shared.Id, "shop")
	if !account.GetBool("app_password") || d.password(account, "password") != "app-secret-7" {
		t.Errorf("user: %v", account)
	}
	dump, err := d.app.FindFirstRecordByFilter(schema.DBDumps, "kind='move'", nil)
	if err != nil || dump.GetString("server") != shared.Id || dump.GetString("database_name") != "shop" {
		t.Errorf("the copy kept of the moved database: %v %v", dump, err)
	}

	// the application is deployed again: its old database stays stopped, and the
	// shared server answers to the old service's names on the stack's network
	d.waitFor("the application to be deployed again", func() bool {
		deploys, err := d.app.FindRecordsByFilter(schema.Commands, "type='stack.deploy' && label~'analytics'", "-created", 0, 0, nil)
		return err == nil && len(deploys) >= 2 && deploys[0].GetString("status") == schema.CommandSucceeded
	})
	if old := d.docker.Find("analytics-db-1"); old == nil || old.Status != "exited" {
		t.Errorf("the old database container: %+v", old)
	}
	if web := d.docker.Find("analytics-web-1"); web == nil || web.Status != "running" {
		t.Errorf("the application: %+v", web)
	}
	network := d.docker.FindNetwork("pcdb-analytics")
	if network == nil {
		t.Fatal("the stack has no database network")
	}
	aliases := network.Members[d.docker.Find("pg-main-db-1").ID]
	if !slices.Contains(aliases, "db") || !slices.Contains(aliases, "analytics-db-1") || !slices.Contains(aliases, "pg-main") {
		t.Errorf("the shared server's names on the stack's network: %v", aliases)
	}
	if _, member := network.Members[d.docker.Find("analytics-web-1").ID]; !member {
		t.Error("the application is not on the database network")
	}

	// the stack's own restart leaves the old database stopped
	restart := d.create(d.host.Id, &agentv1.Command{Payload: &agentv1.Command_StackRestart{StackRestart: &agentv1.StackRestart{Name: "analytics"}}})
	d.waitForStatus(restart.Id, schema.CommandSucceeded)
	if old := d.docker.Find("analytics-db-1"); old == nil || old.Status != "exited" {
		t.Errorf("the old database container after a restart: %+v", old)
	}
	if web := d.docker.Find("analytics-web-1"); web == nil || web.Restart == 0 {
		t.Errorf("the application was not restarted: %+v", web)
	}

	// the application keeps its password in its own settings: the panel does not change it
	status, body = d.post("/api/pc/databases/"+database.Id+"/rotate", d.operator, map[string]any{})
	if status != http.StatusBadRequest || !strings.Contains(body, "own settings") {
		t.Errorf("rotating the application's password: %d %s", status, body)
	}

	// a viewer sees the move, without its secrets
	status, body = testutil.Do(t, d.handler, testutil.Request{Method: http.MethodGet, URL: "/api/collections/db_moves/records", Token: d.viewer})
	if status != http.StatusOK || !strings.Contains(body, answer.Move.ID) || strings.Contains(body, `"secrets"`) {
		t.Errorf("moves for a viewer: %d %s", status, body)
	}
	d.noFileHolds("app-secret-7", "the application's password")

	// a week later the old container goes; its volume stays
	move.Set("remove_after", types.NowDateTime().Add(-time.Minute))
	if err := d.app.Save(move); err != nil {
		t.Fatal(err)
	}
	for _, job := range d.app.Cron().Jobs() {
		if job.Id() == "pocketcloud_db_moves_cleanup" {
			job.Run()
		}
	}
	d.waitFor("the old container to be removed", func() bool {
		return d.docker.Find("analytics-db-1") == nil
	})
	removal, err := d.app.FindFirstRecordByFilter(schema.Commands, "type='container.remove'", nil)
	if err != nil || strings.Contains(removal.GetString("payload"), "remove_volumes") {
		t.Errorf("removal: %v %v", removal, err)
	}
}

func TestAMoveThatFailsLeavesEverythingAsItWas(t *testing.T) {
	d := newDatabaseHarness(t)

	shared := d.createServer("pg-main", "postgres")
	stackID, dedicated := d.linkedAnalytics()

	// the restore into the shared server fails
	answer := d.server.answer
	d.docker.OnExec = func(call *fakedocker.ExecCall, stdin []byte) fakedocker.ExecResult {
		for _, item := range call.Env {
			if strings.HasPrefix(item, "PC_FILE=") {
				return fakedocker.ExecResult{Code: 3, Stderr: "ERROR: relation already exists"}
			}
		}
		return answer(call, stdin)
	}

	status, body := d.post("/api/pc/db-servers/"+dedicated.Id+"/move", d.operator, map[string]any{"target": shared.Id})
	if status != http.StatusOK {
		t.Fatalf("move: %d %s", status, body)
	}
	var started struct {
		Move    struct{ ID string } `json:"move"`
		Command struct{ ID string } `json:"command"`
	}
	answerOf(t, body, &started)
	d.waitForStatus(started.Command.ID, schema.CommandFailed)

	d.waitFor("the move to be marked failed", func() bool {
		found, err := d.app.FindRecordById(schema.DBMoves, started.Move.ID)
		return err == nil && found.GetString("status") == schema.MoveFailed && found.GetString("secrets") == ""
	})

	// the half-made database is dropped, the application runs on its own server
	dropped := false
	for _, exec := range d.server.statements() {
		if exec.container == d.docker.Find("pg-main-db-1").ID && strings.Contains(exec.env["PC_SQL"], "DROP DATABASE") {
			dropped = true
		}
	}
	if !dropped {
		t.Error("the half-made database stayed on the shared server")
	}
	if _, err := d.app.FindRecordById(schema.DBServers, dedicated.Id); err != nil {
		t.Errorf("the old server is gone: %v", err)
	}
	database, err := d.app.FindFirstRecordByFilter(schema.Databases, "name='shop'", nil)
	if err != nil || database.GetString("server") != dedicated.Id {
		t.Errorf("database: %v %v", database, err)
	}
	d.waitFor("the application to run again", func() bool {
		db, web := d.docker.Find("analytics-db-1"), d.docker.Find("analytics-web-1")
		return db != nil && db.Status == "running" && web != nil && web.Status == "running"
	})
	_ = stackID
}

func TestAMoveNeedsTheApplicationsOwnUser(t *testing.T) {
	d := newDatabaseHarness(t)

	shared := d.createServer("pg-main", "postgres")
	stackID := d.newStack("analytics", strings.Replace(analyticsFile, "      POSTGRES_USER: shop\n", "", 1), "")
	status, body := d.post("/api/pc/stacks/"+stackID+"/save", d.operator, map[string]any{"secrets": map[string]any{"set": map[string]string{"DB_PASS": "app-secret-7"}}})
	if status != http.StatusOK {
		t.Fatalf("secrets: %d %s", status, body)
	}
	d.waitForStatus(d.deploy(stackID, map[string]any{}).Id, schema.CommandSucceeded)

	status, body = d.post("/api/pc/db-servers/link", d.operator, map[string]any{
		"host": d.host.Id, "docker_id": d.docker.Find("analytics-db-1").ID, "admin_user": "postgres", "secret_key": "DB_PASS",
	})
	if status != http.StatusOK {
		t.Fatalf("link: %d %s", status, body)
	}
	dedicated := recordID(t, body)
	d.waitFor("the dedicated server's database to be listed", func() bool {
		_, err := d.app.FindFirstRecordByFilter(schema.Databases, "server={:server}", dbx.Params{"server": dedicated})
		return err == nil
	})

	// the application signs in as the administrator, which the shared server keeps
	status, body = d.post("/api/pc/db-servers/"+dedicated+"/move", d.operator, map[string]any{"target": shared.Id})
	if status != http.StatusBadRequest || !strings.Contains(body, "administrator") {
		t.Errorf("a move as the administrator: %d %s", status, body)
	}
}
