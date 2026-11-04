package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// startMove asks for a move and returns the ids of the move and its command.
func (d *databaseHarness) startMove(server string, body map[string]any) (move, command string) {
	d.t.Helper()
	status, answer := d.post("/api/pc/db-servers/"+server+"/move", d.operator, body)
	if status != http.StatusOK {
		d.t.Fatalf("move: %d %s", status, answer)
	}
	var started struct {
		Move    struct{ ID string } `json:"move"`
		Command struct{ ID string } `json:"command"`
	}
	answerOf(d.t, answer, &started)
	return started.Move.ID, started.Command.ID
}

// finishMove waits until a move is recorded as done.
func (d *databaseHarness) finishMove(move, command string) {
	d.t.Helper()
	d.waitForStatus(command, schema.CommandSucceeded)
	d.waitFor("the move to be recorded", func() bool {
		found, err := d.app.FindRecordById(schema.DBMoves, move)
		return err == nil && found.GetString("status") == schema.MoveDone
	})
}

// analyticsWith deploys and links an analytics stack with its own PostgreSQL,
// from a Compose file and env of the test's choosing.
func (d *databaseHarness) analyticsWith(compose, env string, secrets map[string]string) (stackID string, server *core.Record) {
	d.t.Helper()
	stackID = d.newStack("analytics", compose, env)
	if status, body := d.post("/api/pc/stacks/"+stackID+"/save", d.operator, map[string]any{"secrets": map[string]any{"set": secrets}}); status != http.StatusOK {
		d.t.Fatalf("secrets: %d %s", status, body)
	}
	d.waitForStatus(d.deploy(stackID, map[string]any{}).Id, schema.CommandSucceeded)
	status, body := d.post("/api/pc/db-servers/link", d.operator, map[string]any{
		"host": d.host.Id, "docker_id": d.docker.Find("analytics-db-1").ID, "admin_user": "postgres", "password": "admin-pass-1",
	})
	if status != http.StatusOK {
		d.t.Fatalf("link: %d %s", status, body)
	}
	server, err := d.app.FindRecordById(schema.DBServers, recordID(d.t, body))
	if err != nil {
		d.t.Fatal(err)
	}
	d.waitFor("the dedicated server's database to be listed", func() bool {
		_, err := d.app.FindFirstRecordByFilter(schema.Databases, "server={:server}", dbx.Params{"server": server.Id})
		return err == nil
	})
	return stackID, server
}

func TestNothingElseChangesWhatAMoveWorksOn(t *testing.T) {
	d := newDatabaseHarness(t)

	shared := d.createServer("pg-main", "postgres")
	stackID, dedicated := d.linkedAnalytics()
	release := d.holdRestores()
	defer release()

	move, command := d.startMove(dedicated.Id, map[string]any{"target": shared.Id})
	d.waitFor("the application to stop", func() bool { return d.docker.Find("analytics-web-1").Status == "exited" })

	// a running move is not interrupted from the panel
	status, body := d.post("/api/pc/commands/"+command+"/cancel", d.operator, map[string]any{})
	if status != http.StatusBadRequest || !strings.Contains(body, "cannot be interrupted") {
		t.Errorf("cancelling the move: %d %s", status, body)
	}

	// the stack is neither deployed nor restarted, and its databases stay as they are
	if status, body := d.post("/api/pc/stacks/"+stackID+"/deploy", d.operator, map[string]any{}); status == http.StatusOK || !strings.Contains(body, "moving") {
		t.Errorf("deploying the stack during the move: %d %s", status, body)
	}
	status, body = d.post("/api/collections/commands/records", d.operator, map[string]any{
		"host": d.host.Id, "type": "stack.restart", "label": "analytics", "payload": map[string]any{"name": "analytics"},
	})
	if status != http.StatusConflict {
		t.Errorf("restarting the stack during the move: %d %s", status, body)
	}
	status, body = d.post("/api/pc/databases", d.operator, map[string]any{"server": shared.Id, "name": "reports", "stack": stackID})
	if status != http.StatusBadRequest || !strings.Contains(body, "moving") {
		t.Errorf("a new database for the stack during the move: %d %s", status, body)
	}
	// nor is a database of the moving name made on the target
	status, body = d.post("/api/pc/databases", d.operator, map[string]any{"server": shared.Id, "name": "shop"})
	if status != http.StatusBadRequest || !strings.Contains(body, "moving into") {
		t.Errorf("a database of the moving name on the target: %d %s", status, body)
	}
	// and the target's own stack is not deployed or stopped meanwhile
	if status, body := d.post("/api/pc/stacks/"+shared.GetString("stack")+"/deploy", d.operator, map[string]any{}); status == http.StatusOK {
		t.Errorf("deploying the target's stack during the move: %d %s", status, body)
	}
	status, body = d.post("/api/collections/commands/records", d.operator, map[string]any{
		"host": d.host.Id, "type": "stack.stop", "label": "pg-main", "payload": map[string]any{"name": "pg-main"},
	})
	if status != http.StatusConflict {
		t.Errorf("stopping the target's stack during the move: %d %s", status, body)
	}

	release()
	d.finishMove(move, command)
	if status, body := d.post("/api/pc/stacks/"+stackID+"/deploy", d.operator, map[string]any{}); status != http.StatusOK {
		t.Errorf("deploying the stack after the move: %d %s", status, body)
	}
}

func TestTwoMovesDoNotClaimOneName(t *testing.T) {
	d := newDatabaseHarness(t)

	shared := d.createServer("pg-main", "postgres")
	_, first := d.linkedAnalytics()

	// a second application with its own server and the same database and user
	second := d.newStack("shop", analyticsFile, "")
	if status, body := d.post("/api/pc/stacks/"+second+"/save", d.operator, map[string]any{"secrets": map[string]any{"set": map[string]string{"DB_PASS": "other-secret-1"}}}); status != http.StatusOK {
		t.Fatalf("secrets: %d %s", status, body)
	}
	d.waitForStatus(d.deploy(second, map[string]any{}).Id, schema.CommandSucceeded)
	status, body := d.post("/api/pc/db-servers/link", d.operator, map[string]any{
		"host": d.host.Id, "docker_id": d.docker.Find("shop-db-1").ID, "admin_user": "shop", "secret_key": "DB_PASS",
	})
	if status != http.StatusOK {
		t.Fatalf("link: %d %s", status, body)
	}
	other := recordID(t, body)
	d.waitFor("the second server's database to be listed", func() bool {
		_, err := d.app.FindFirstRecordByFilter(schema.Databases, "server={:server}", dbx.Params{"server": other})
		return err == nil
	})

	release := d.holdRestores()
	defer release()
	move, command := d.startMove(first.Id, map[string]any{"target": shared.Id})

	status, body = d.post("/api/pc/db-servers/"+other+"/move", d.operator, map[string]any{"target": shared.Id})
	if status != http.StatusBadRequest || !strings.Contains(body, "already moving into") {
		t.Errorf("a second move of the same names: %d %s", status, body)
	}
	release()
	d.finishMove(move, command)
}

func TestAMovedDatabaseIsGuardedByEveryMoveOfItsServer(t *testing.T) {
	d := newDatabaseHarness(t)

	main := d.createServer("pg-main", "postgres")
	next := d.createServer("pg-next", "postgres")
	store := d.deployedApp("store")
	storeDB := d.createDatabase(main.Id, "store", store)
	d.waitFor("store to be connected", func() bool { return d.stack(store).GetInt("current_revision") == 2 })

	// a move into pg-main is under way, then store moves out of it
	_, dedicated := d.linkedAnalytics()
	release := d.holdRestores()
	defer release()
	d.startMove(dedicated.Id, map[string]any{"target": main.Id})
	d.startMove(main.Id, map[string]any{"target": next.Id, "database": "store"})

	for _, action := range []string{"rotate", "clone"} {
		status, body := d.post("/api/pc/databases/"+storeDB.Id+"/"+action, d.operator, map[string]any{"name": "store_copy"})
		if status != http.StatusConflict {
			t.Errorf("%s of a moving database: %d %s", action, status, body)
		}
	}
	status, body := testutil.Do(t, d.handler, testutil.Request{Method: http.MethodDelete, URL: "/api/pc/databases/" + storeDB.Id, Token: d.operator})
	if status != http.StatusConflict {
		t.Errorf("dropping a moving database: %d %s", status, body)
	}
}

func TestADatabaseMovesOnlyWhenItsApplicationCanFollow(t *testing.T) {
	d := newDatabaseHarness(t)

	main := d.createServer("pg-main", "postgres")
	next := d.createServer("pg-next", "postgres")
	shop := d.deployedApp("shop")
	d.createDatabase(main.Id, "shop", shop)
	d.waitFor("shop to be connected", func() bool { return d.stack(shop).GetInt("current_revision") == 2 })

	// another stack uses shop too
	worker := d.newStack("worker", "services:\n  run:\n    image: alpine:3.22\n", "SHOP_PASSWORD=${secret:db/shop}\n")
	status, body := d.post("/api/pc/db-servers/"+main.Id+"/move", d.operator, map[string]any{"target": next.Id, "database": "shop"})
	if status != http.StatusBadRequest || !strings.Contains(body, "stack worker also uses shop") {
		t.Errorf("a database two stacks use: %d %s", status, body)
	}
	if status, body := testutil.Do(t, d.handler, testutil.Request{Method: http.MethodDelete, URL: "/api/collections/stacks/records/" + worker, Token: d.operator}); status != http.StatusNoContent {
		t.Fatalf("removing worker: %d %s", status, body)
	}

	// a variable of its own names the old server
	env := d.stack(shop).GetString("env") + "REPORTS_HOST=pg-main\n"
	if status, body := d.post("/api/pc/stacks/"+shop+"/save", d.operator, map[string]any{"env": env}); status != http.StatusOK {
		t.Fatalf("save: %d %s", status, body)
	}
	status, body = d.post("/api/pc/db-servers/"+main.Id+"/move", d.operator, map[string]any{"target": next.Id, "database": "shop"})
	if status != http.StatusBadRequest || !strings.Contains(body, "REPORTS_HOST") {
		t.Errorf("a variable naming the old server: %d %s", status, body)
	}

	// a rotation under way
	env = strings.Replace(d.stack(shop).GetString("env"), "REPORTS_HOST=pg-main\n", "", 1)
	if status, body := d.post("/api/pc/stacks/"+shop+"/save", d.operator, map[string]any{"env": env}); status != http.StatusOK {
		t.Fatalf("save: %d %s", status, body)
	}
	account := d.user(main.Id, "shop")
	account.Set("pending_password", "sealed-anything")
	if err := d.app.Save(account); err != nil {
		t.Fatal(err)
	}
	status, body = d.post("/api/pc/db-servers/"+main.Id+"/move", d.operator, map[string]any{"target": next.Id, "database": "shop"})
	if status != http.StatusBadRequest || !strings.Contains(body, "being rotated") {
		t.Errorf("a move during a rotation: %d %s", status, body)
	}
}

func TestADedicatedMoveReadsTheStackAsComposeDoes(t *testing.T) {
	// the password without a value comes from the stack's variables, the user
	// from a variable with a default, and a dollar sign is escaped
	compose := `services:
  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: ${PG_USER:-shop}
      POSTGRES_PASSWORD:
  web:
    image: umami:2
    environment:
      NOTE: "pa$$word"
`
	d := newDatabaseHarness(t)
	shared := d.createServer("pg-main", "postgres")
	_, dedicated := d.analyticsWith(compose, "", map[string]string{"POSTGRES_PASSWORD": "from-the-env-9"})

	move, command := d.startMove(dedicated.Id, map[string]any{"target": shared.Id})
	d.finishMove(move, command)
	created := false
	for _, exec := range d.server.statements() {
		if strings.Contains(exec.env["PC_SQL"], "CREATE DATABASE") && strings.Contains(exec.env["PC_SQL"], `rolname = 'shop'`) {
			created = strings.Contains(exec.env["PC_SQL"], "'from-the-env-9'")
		}
	}
	if !created {
		t.Error("the user was not created with the password Compose gives the database")
	}
}

func TestADedicatedMoveRefusesWhatComposeWouldUndo(t *testing.T) {
	d := newDatabaseHarness(t)
	shared := d.createServer("pg-main", "postgres")

	// every profile on: no override keeps the old database off
	stackID, dedicated := d.analyticsWith(analyticsFile, "COMPOSE_PROFILES=*\n", map[string]string{"DB_PASS": "app-secret-7"})
	status, body := d.post("/api/pc/db-servers/"+dedicated.Id+"/move", d.operator, map[string]any{"target": shared.Id})
	if status != http.StatusBadRequest || !strings.Contains(body, "COMPOSE_PROFILES") {
		t.Errorf("COMPOSE_PROFILES=*: %d %s", status, body)
	}

	// the service is db, and the stack also uses another shared server, which
	// answers to db on its network too
	if status, body := d.post("/api/pc/stacks/"+stackID+"/save", d.operator, map[string]any{"env": ""}); status != http.StatusOK {
		t.Fatalf("save: %d %s", status, body)
	}
	other := d.createServer("maria-main", "mariadb")
	d.createDatabase(other.Id, "stats", stackID)
	status, body = d.post("/api/pc/db-servers/"+dedicated.Id+"/move", d.operator, map[string]any{"target": shared.Id})
	if status != http.StatusBadRequest || !strings.Contains(body, "rename the service") {
		t.Errorf("a name other shared servers answer to: %d %s", status, body)
	}
}

func TestAMoveChecksTheApplicationCanSignIn(t *testing.T) {
	d := newDatabaseHarness(t)
	shared := d.createServer("pg-main", "postgres")
	_, dedicated := d.linkedAnalytics()

	answer := d.server.answer
	d.docker.OnExec = func(call *fakedocker.ExecCall, stdin []byte) fakedocker.ExecResult {
		if strings.Contains(strings.Join(call.Cmd, " "), "hostname -i") {
			return fakedocker.ExecResult{Code: 2, Stderr: "FATAL: password authentication failed for user \"shop\""}
		}
		return answer(call, stdin)
	}
	_, command := d.startMove(dedicated.Id, map[string]any{"target": shared.Id})
	failed := d.waitForStatus(command, schema.CommandFailed)
	if !strings.Contains(failed.GetString("error"), "cannot sign in") {
		t.Errorf("error: %s", failed.GetString("error"))
	}
	if web := d.docker.Find("analytics-web-1"); web.Status != "running" || web.Restart != 0 {
		t.Errorf("the application was touched: %+v", web)
	}
}

func TestDroppingAMovedDatabaseKeepsAUserThatOwnsMore(t *testing.T) {
	d := newDatabaseHarness(t)
	shared := d.createServer("pg-main", "postgres")
	_, dedicated := d.linkedAnalytics()

	// the application's server has a second database of the application's user
	collection, err := d.app.FindCollectionByNameOrId(schema.Databases)
	if err != nil {
		t.Fatal(err)
	}
	archive := core.NewRecord(collection)
	archive.Set("server", dedicated.Id)
	archive.Set("name", "shop_archive")
	archive.Set("owner_kind", "stack")
	archive.Set("stack", dedicated.GetString("stack"))
	archive.Set("status", "ready")
	if err := d.app.Save(archive); err != nil {
		t.Fatal(err)
	}

	move, command := d.startMove(dedicated.Id, map[string]any{"target": shared.Id})
	d.finishMove(move, command)

	database, err := d.app.FindFirstRecordByFilter(schema.Databases, "name='shop'", nil)
	if err != nil {
		t.Fatal(err)
	}
	status, body := testutil.Do(t, d.handler, testutil.Request{Method: http.MethodDelete, URL: "/api/pc/databases/" + database.Id, Token: d.operator})
	if status != http.StatusOK {
		t.Fatalf("drop: %d %s", status, body)
	}
	if payload := d.command(recordID(t, body)).GetString("payload"); strings.Contains(payload, `"user":"shop"`) {
		t.Errorf("the drop takes the user that owns shop_archive too: %s", payload)
	}
}
