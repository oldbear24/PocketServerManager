package e2e

import (
	"net/http"
	"slices"
	"strings"
	"sync"
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

	// the old container is no candidate to link any more
	status, body = testutil.Do(t, d.handler, testutil.Request{Method: http.MethodGet, URL: "/api/pc/db-servers/candidates", Token: d.viewer})
	if status != http.StatusOK || strings.Contains(body, "analytics-db-1") {
		t.Errorf("candidates after the move: %d %s", status, body)
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
	// the move counts as cleaned up once the container is gone from the inventory
	if fresh, _ := d.app.FindRecordById(schema.DBMoves, move.Id); !fresh.GetDateTime("removed_at").IsZero() {
		t.Error("the removal was recorded before the container was gone")
	}
	d.waitFor("the inventory to lose the old container", func() bool {
		_, err := d.app.FindFirstRecordByFilter(schema.Containers, "name='analytics-db-1'", nil)
		return err != nil
	})
	for _, job := range d.app.Cron().Jobs() {
		if job.Id() == "pocketcloud_db_moves_cleanup" {
			job.Run()
		}
	}
	if fresh, _ := d.app.FindRecordById(schema.DBMoves, move.Id); fresh.GetDateTime("removed_at").IsZero() {
		t.Error("the removal is not recorded")
	}
}

func TestAMoveThatFailsLeavesEverythingAsItWas(t *testing.T) {
	d := newDatabaseHarness(t)

	shared := d.createServer("pg-main", "postgres")
	stackID, dedicated := d.linkedAnalytics()

	// the restore into the shared server fails, after many notices
	answer := d.server.answer
	d.docker.OnExec = func(call *fakedocker.ExecCall, stdin []byte) fakedocker.ExecResult {
		for _, item := range call.Env {
			if strings.HasPrefix(item, "PC_FILE=") {
				return fakedocker.ExecResult{Code: 3, Stderr: strings.Repeat("NOTICE: a table is skipped\n", 300) + "ERROR: relation already exists"}
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
	failed, err := d.app.FindRecordById(schema.DBMoves, started.Move.ID)
	if err != nil || len(failed.GetString("error")) > 2000 || failed.GetString("error") == "" {
		t.Errorf("the error of the move: %d characters", len(failed.GetString("error")))
	}

	// the failed move holds nothing up
	status, body = d.post("/api/pc/db-servers/"+dedicated.Id+"/move", d.operator, map[string]any{"target": shared.Id})
	if status != http.StatusOK {
		t.Errorf("a second move: %d %s", status, body)
	}
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

func TestSharedServersChooseTheirVersionAndAreUpdated(t *testing.T) {
	d := newDatabaseHarness(t)

	// a new major version is a new server, of a series the panel offers
	status, body := d.post("/api/pc/db-servers", d.operator, map[string]any{"host": d.host.Id, "name": "pg-old", "engine": "postgres", "version": "9.6"})
	if status != http.StatusBadRequest || !strings.Contains(body, "17, 18, 16") {
		t.Errorf("an unknown version: %d %s", status, body)
	}
	status, body = d.post("/api/pc/db-servers", d.operator, map[string]any{"host": d.host.Id, "name": "pg-next", "engine": "postgres", "version": "18"})
	if status != http.StatusOK {
		t.Fatalf("a server of PostgreSQL 18: %d %s", status, body)
	}
	var created struct {
		Server  struct{ ID string } `json:"server"`
		Command struct{ ID string } `json:"command"`
	}
	answerOf(t, body, &created)
	d.waitForStatus(created.Command.ID, schema.CommandSucceeded)
	var server *core.Record
	d.waitFor("the server to be ready", func() bool {
		found, err := d.app.FindRecordById(schema.DBServers, created.Server.ID)
		server = found
		return err == nil && found.GetString("status") == "ready"
	})
	if server.GetString("image") != "postgres:18" {
		t.Fatalf("server: %v", server)
	}
	if container := d.docker.Find("pg-next-db-1"); container == nil || container.Image != "postgres:18" {
		t.Errorf("container: %+v", container)
	}
	stack := d.stack(server.GetString("stack"))
	if !strings.Contains(stack.GetString("compose_yaml"), "data:/var/lib/postgresql\n") {
		t.Errorf("the data of PostgreSQL 18:\n%s", stack.GetString("compose_yaml"))
	}

	// an update pulls the image of its series again and recreates the container
	pulls := func() int {
		count := 0
		for _, call := range d.calls() {
			if call.Project == "pg-next" && call.Verb == "pull" {
				count++
			}
		}
		return count
	}
	before := pulls()
	status, body = d.post("/api/pc/db-servers/"+server.Id+"/update", d.viewer, map[string]any{})
	if status != http.StatusForbidden {
		t.Errorf("a viewer updated a server: %d %s", status, body)
	}
	status, body = d.post("/api/pc/db-servers/"+server.Id+"/update", d.operator, map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("update: %d %s", status, body)
	}
	d.waitForStatus(recordID(t, body), schema.CommandSucceeded)
	if pulls() != before+1 {
		t.Errorf("the update pulled %d times", pulls()-before)
	}

	// a stack's own server is updated with its stack
	_, dedicated := d.linkedAnalytics()
	status, body = d.post("/api/pc/db-servers/"+dedicated.Id+"/update", d.operator, map[string]any{})
	if status != http.StatusBadRequest {
		t.Errorf("updating a dedicated server: %d %s", status, body)
	}
}

func TestADatabaseMovesToANewerSharedServer(t *testing.T) {
	d := newDatabaseHarness(t)

	old := d.createServer("pg-main", "postgres")
	next := d.createServer("pg-next", "postgres")
	stackID := d.deployedApp("shop")
	database := d.createDatabase(old.Id, "shop", stackID)
	d.waitFor("the stack to be connected", func() bool { return d.stack(stackID).GetInt("current_revision") == 2 })
	password := d.password(d.user(old.Id, "shop"), "password")

	status, body := d.post("/api/pc/db-servers/"+old.Id+"/move", d.operator, map[string]any{"target": next.Id, "database": "nothing"})
	if status != http.StatusBadRequest {
		t.Errorf("a database that does not exist: %d %s", status, body)
	}
	status, body = d.post("/api/pc/db-servers/"+old.Id+"/move", d.operator, map[string]any{"target": next.Id, "database": "shop"})
	if status != http.StatusOK {
		t.Fatalf("move: %d %s", status, body)
	}
	var answer struct {
		Move    struct{ ID string } `json:"move"`
		Command struct{ ID string } `json:"command"`
	}
	answerOf(t, body, &answer)
	d.waitForStatus(answer.Command.ID, schema.CommandSucceeded)
	d.waitFor("the move to be recorded", func() bool {
		found, err := d.app.FindRecordById(schema.DBMoves, answer.Move.ID)
		return err == nil && found.GetString("status") == schema.MoveDone
	})

	// created on the new server with the same password, dropped from the old one
	created, dropped := false, false
	for _, exec := range d.server.statements() {
		switch {
		case exec.container == d.docker.Find("pg-next-db-1").ID && strings.Contains(exec.env["PC_SQL"], "CREATE DATABASE"):
			created = strings.Contains(exec.env["PC_SQL"], "'"+password+"'")
		case exec.container == d.docker.Find("pg-main-db-1").ID && strings.Contains(exec.env["PC_SQL"], "DROP DATABASE"):
			dropped = strings.Contains(exec.env["PC_SQL"], `"shop"`)
		}
	}
	if !created || !dropped {
		t.Errorf("created %v, dropped %v", created, dropped)
	}

	// the database, its user and secret, and the stack's variables follow it
	fresh, err := d.app.FindRecordById(schema.Databases, database.Id)
	if err != nil || fresh.GetString("server") != next.Id {
		t.Errorf("database: %v %v", fresh, err)
	}
	account := d.user(next.Id, "shop")
	if account.GetString("secret_name") != "db/shop" || account.GetBool("app_password") || d.password(account, "password") != password {
		t.Errorf("user: %v", account)
	}
	env := d.stack(stackID).GetString("env")
	if !strings.Contains(env, "DB_HOST=pg-next") || strings.Contains(env, "pg-main") {
		t.Errorf("env: %q", env)
	}
	if links, _ := d.app.FindRecordsByFilter(schema.DBNetworks, "stack={:stack}", "", 0, 0, dbx.Params{"stack": stackID}); len(links) != 1 || links[0].GetString("server") != next.Id {
		t.Errorf("networks: %v", links)
	}

	// the application reaches the new server on its network, and the old one no more
	d.waitFor("the application to reach the new server", func() bool {
		network := d.docker.FindNetwork("pcdb-shop")
		if network == nil {
			return false
		}
		next, old := d.docker.Find("pg-next-db-1"), d.docker.Find("pg-main-db-1")
		if next == nil || old == nil {
			return false
		}
		_, newServer := network.Members[next.ID]
		_, oldServer := network.Members[old.ID]
		return newServer && !oldServer
	})
	d.waitFor("the application to be deployed again", func() bool {
		return d.stack(stackID).GetInt("current_revision") == 3 && d.stack(stackID).GetString("status") == "deployed"
	})
}

// holdRestores keeps every restore waiting until the returned function is called.
func (d *databaseHarness) holdRestores() (release func()) {
	gate := make(chan struct{})
	answer := d.server.answer
	d.docker.OnExec = func(call *fakedocker.ExecCall, stdin []byte) fakedocker.ExecResult {
		for _, item := range call.Env {
			if strings.HasPrefix(item, "PC_FILE=") {
				<-gate
			}
		}
		return answer(call, stdin)
	}
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

func TestAMoveHoldsTheContainersAndServersItWorksOn(t *testing.T) {
	d := newDatabaseHarness(t)

	shared := d.createServer("pg-main", "postgres")
	_, dedicated := d.linkedAnalytics()
	release := d.holdRestores()
	defer release()

	status, body := d.post("/api/pc/db-servers/"+dedicated.Id+"/move", d.operator, map[string]any{"target": shared.Id})
	if status != http.StatusOK {
		t.Fatalf("move: %d %s", status, body)
	}
	var answer struct {
		Move    struct{ ID string } `json:"move"`
		Command struct{ ID string } `json:"command"`
	}
	answerOf(t, body, &answer)
	d.waitFor("the application to stop", func() bool {
		web := d.docker.Find("analytics-web-1")
		return web != nil && web.Status == "exited"
	})

	start := func(name string) (int, string) {
		return d.post("/api/collections/commands/records", d.operator, map[string]any{
			"host": d.host.Id, "type": "container.start", "label": name,
			"payload": map[string]any{"docker_id": d.docker.Find(name).ID},
		})
	}

	// the application's containers wait for the move
	if status, body := start("analytics-web-1"); status != http.StatusConflict || !strings.Contains(body, "moving") {
		t.Errorf("starting the application during the move: %d %s", status, body)
	}
	// the target is neither updated nor removed meanwhile
	if status, body := d.post("/api/pc/db-servers/"+shared.Id+"/update", d.operator, map[string]any{}); status != http.StatusConflict {
		t.Errorf("updating the target during the move: %d %s", status, body)
	}
	status, body = testutil.Do(t, d.handler, testutil.Request{Method: http.MethodDelete, URL: "/api/pc/db-servers/" + shared.Id, Token: d.operator})
	if status != http.StatusConflict {
		t.Errorf("removing the target during the move: %d %s", status, body)
	}

	release()
	d.waitForStatus(answer.Command.ID, schema.CommandSucceeded)
	d.waitFor("the move to be recorded", func() bool {
		found, err := d.app.FindRecordById(schema.DBMoves, answer.Move.ID)
		return err == nil && found.GetString("status") == schema.MoveDone
	})

	// the old database stays stopped: Docker does not start it, and neither does the table
	old := d.docker.Find("analytics-db-1")
	if policy, _ := old.Host["RestartPolicy"].(map[string]any); policy["Name"] != "no" {
		t.Errorf("the restart policy of the old database: %v", old.Host["RestartPolicy"])
	}
	if networks := d.docker.NetworksOf(old.ID); len(networks) != 0 {
		t.Errorf("the old database is still on %v", networks)
	}
	if status, body := start("analytics-db-1"); status != http.StatusBadRequest || !strings.Contains(body, "stays stopped") {
		t.Errorf("starting the old database: %d %s", status, body)
	}
	d.waitFor("the application to run again", func() bool {
		web := d.docker.Find("analytics-web-1")
		return web != nil && web.Status == "running"
	})
	if status, body := start("analytics-web-1"); status != http.StatusOK {
		t.Errorf("starting the application after the move: %d %s", status, body)
	}
}

func TestAStacksOwnServerMovesWhole(t *testing.T) {
	d := newDatabaseHarness(t)

	shared := d.createServer("pg-main", "postgres")
	_, dedicated := d.linkedAnalytics()

	// the server has a database the panel does not know
	answer := d.server.answer
	d.docker.OnExec = func(call *fakedocker.ExecCall, stdin []byte) fakedocker.ExecResult {
		for _, item := range call.Env {
			if strings.HasPrefix(item, "PC_SQL=") && strings.Contains(item, "NOT datistemplate AND datname <>") {
				return fakedocker.ExecResult{Stdout: "shop\nshop-archive\n"}
			}
		}
		return answer(call, stdin)
	}

	status, body := d.post("/api/pc/db-servers/"+dedicated.Id+"/move", d.operator, map[string]any{"target": shared.Id})
	if status != http.StatusOK {
		t.Fatalf("move: %d %s", status, body)
	}
	var started struct {
		Command struct{ ID string } `json:"command"`
	}
	answerOf(t, body, &started)
	failed := d.waitForStatus(started.Command.ID, schema.CommandFailed)
	if !strings.Contains(failed.GetString("error"), "shop-archive") {
		t.Errorf("error: %s", failed.GetString("error"))
	}
	// nothing stopped and nothing created
	if web := d.docker.Find("analytics-web-1"); web.Status != "running" || web.Restart != 0 {
		t.Errorf("the application: %+v", web)
	}
	for _, exec := range d.server.statements() {
		if exec.container == d.docker.Find("pg-main-db-1").ID && strings.Contains(exec.env["PC_SQL"], "CREATE DATABASE") {
			t.Error("a database was created on the shared server")
		}
	}
}

func TestOnlyTheVariablesThePanelWroteAreSwitched(t *testing.T) {
	d := newDatabaseHarness(t)

	old := d.createServer("pg-main", "postgres")
	next := d.createServer("pg-next", "postgres")
	stackID := d.deployedApp("shop")
	d.createDatabase(old.Id, "shop", stackID)
	d.waitFor("the stack to be connected", func() bool { return d.stack(stackID).GetInt("current_revision") == 2 })
	d.createDatabase(old.Id, "shop_logs", stackID)

	// an export of another application's database on the old server
	blog := d.deployedApp("blog")
	d.createDatabase(old.Id, "blog", blog)
	collection, err := d.app.FindCollectionByNameOrId(schema.DBDumps)
	if err != nil {
		t.Fatal(err)
	}
	dump := core.NewRecord(collection)
	dump.Set("server", old.Id)
	dump.Set("database_name", "blog")
	dump.Set("kind", schema.DumpExport)
	if err := d.app.Save(dump); err != nil {
		t.Fatal(err)
	}

	// shop_logs got no variables of its own, since shop's took the names first
	status, body := d.post("/api/pc/db-servers/"+old.Id+"/move", d.operator, map[string]any{"target": next.Id, "database": "shop_logs"})
	if status != http.StatusBadRequest || !strings.Contains(body, "not all as the panel wrote them") {
		t.Errorf("moving shop_logs: %d %s", status, body)
	}

	status, body = d.post("/api/pc/db-servers/"+old.Id+"/move", d.operator, map[string]any{"target": next.Id, "database": "shop"})
	if status != http.StatusOK {
		t.Fatalf("moving shop: %d %s", status, body)
	}
	var answer struct {
		Move    struct{ ID string } `json:"move"`
		Command struct{ ID string } `json:"command"`
	}
	answerOf(t, body, &answer)
	d.waitForStatus(answer.Command.ID, schema.CommandSucceeded)
	d.waitFor("the move to be recorded", func() bool {
		found, err := d.app.FindRecordById(schema.DBMoves, answer.Move.ID)
		return err == nil && found.GetString("status") == schema.MoveDone
	})

	env := d.stack(stackID).GetString("env")
	if !strings.Contains(env, "DB_HOST=pg-next") || !strings.Contains(env, "DB_NAME=shop\n") || strings.Contains(env, "@pg-main") {
		t.Errorf("env: %q", env)
	}
	// shop_logs stays, and so do the network to the old server and its copy
	if links, _ := d.app.FindRecordsByFilter(schema.DBNetworks, "stack={:stack}", "", 0, 0, dbx.Params{"stack": stackID}); len(links) != 2 {
		t.Errorf("networks: %v", links)
	}
	if fresh, err := d.app.FindRecordById(schema.DBDumps, dump.Id); err != nil || fresh.GetString("server") != old.Id {
		t.Errorf("another application's dump moved: %v %v", fresh, err)
	}
	dropped := false
	for _, exec := range d.server.statements() {
		if exec.container == d.docker.Find("pg-main-db-1").ID && strings.Contains(exec.env["PC_SQL"], "DROP") {
			dropped = strings.Contains(exec.env["PC_SQL"], `"shop"`) && !strings.Contains(exec.env["PC_SQL"], "shop_logs")
		}
	}
	if !dropped {
		t.Error("the old copy of shop was not dropped, or more was")
	}
}

func TestADatabaseKeepsTheNamesItCameWithWhenItMovesAgain(t *testing.T) {
	d := newDatabaseHarness(t)

	shared := d.createServer("pg-main", "postgres")
	next := d.createServer("pg-next", "postgres")
	stackID, dedicated := d.linkedAnalytics()

	move := func(server string, body map[string]any) {
		t.Helper()
		status, answer := d.post("/api/pc/db-servers/"+server+"/move", d.operator, body)
		if status != http.StatusOK {
			t.Fatalf("move: %d %s", status, answer)
		}
		var started struct {
			Move    struct{ ID string } `json:"move"`
			Command struct{ ID string } `json:"command"`
		}
		answerOf(t, answer, &started)
		d.waitForStatus(started.Command.ID, schema.CommandSucceeded)
		d.waitFor("the move to be recorded", func() bool {
			found, err := d.app.FindRecordById(schema.DBMoves, started.Move.ID)
			return err == nil && found.GetString("status") == schema.MoveDone
		})
	}

	move(dedicated.Id, map[string]any{"target": shared.Id})
	envBefore := d.stack(stackID).GetString("env")

	// as for PostgreSQL 18: the application still knows its database as db
	move(shared.Id, map[string]any{"target": next.Id, "database": "shop"})

	links, _ := d.app.FindRecordsByFilter(schema.DBNetworks, "stack={:stack}", "", 0, 0, dbx.Params{"stack": stackID})
	if len(links) != 1 || links[0].GetString("server") != next.Id {
		t.Fatalf("networks: %v", links)
	}
	var aliases []string
	_ = links[0].UnmarshalJSONField("aliases", &aliases)
	if !slices.Contains(aliases, "db") || !slices.Contains(aliases, "analytics-db-1") {
		t.Errorf("aliases: %v", aliases)
	}
	account := d.user(next.Id, "shop")
	if !account.GetBool("app_password") || d.password(account, "password") != "app-secret-7" {
		t.Errorf("user: %v", account)
	}
	if env := d.stack(stackID).GetString("env"); env != envBefore {
		t.Errorf("the variables changed: %q", env)
	}
	d.waitFor("the new server to answer as db", func() bool {
		network := d.docker.FindNetwork("pcdb-analytics")
		server := d.docker.Find("pg-next-db-1")
		if network == nil || server == nil {
			return false
		}
		return slices.Contains(network.Members[server.ID], "db")
	})
}

func TestAMoveLeavesAStoppedStackStopped(t *testing.T) {
	d := newDatabaseHarness(t)

	old := d.createServer("pg-main", "postgres")
	next := d.createServer("pg-next", "postgres")
	stackID := d.newStack("quiet", "services:\n  web:\n    image: nginx:1\n", "")
	d.createDatabase(old.Id, "quiet", stackID)

	status, body := d.post("/api/pc/db-servers/"+old.Id+"/move", d.operator, map[string]any{"target": next.Id, "database": "quiet"})
	if status != http.StatusOK {
		t.Fatalf("move: %d %s", status, body)
	}
	var answer struct {
		Move    struct{ ID string } `json:"move"`
		Command struct{ ID string } `json:"command"`
	}
	answerOf(t, body, &answer)
	d.waitForStatus(answer.Command.ID, schema.CommandSucceeded)
	d.waitFor("the move to be recorded", func() bool {
		found, err := d.app.FindRecordById(schema.DBMoves, answer.Move.ID)
		return err == nil && found.GetString("status") == schema.MoveDone
	})
	if deploys, _ := d.app.CountRecords(schema.Commands, dbx.HashExp{"type": "stack.deploy", "resource": "stack:quiet"}); deploys != 0 {
		t.Errorf("a stack that never ran was deployed %d times", deploys)
	}
}
