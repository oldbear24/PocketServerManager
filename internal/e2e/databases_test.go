package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/agent"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi/fakedocker"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// fakeDatabase answers the scripts the agent runs inside a server's container, the
// way the engine's own client would.
type fakeDatabase struct {
	mu    sync.Mutex
	execs []fakeExec
}

type fakeExec struct {
	container string
	cmd       []string
	env       map[string]string
}

func (f *fakeDatabase) answer(call *fakedocker.ExecCall, _ []byte) fakedocker.ExecResult {
	env := map[string]string{}
	for _, item := range call.Env {
		key, value, _ := strings.Cut(item, "=")
		env[key] = value
	}

	f.mu.Lock()
	f.execs = append(f.execs, fakeExec{container: call.Container, cmd: call.Cmd, env: env})
	f.mu.Unlock()

	switch {
	case strings.Contains(env["PC_SQL"], "'version'"):
		return fakedocker.ExecResult{Stdout: "size\tshop\t2048\nconn\tshop\t3\nconn\t\t1\nmax\t\t200\nversion\t11.8.2-MariaDB\t0\n"}
	case env["PC_SQL"] != "", env["PC_SOURCE"] != "", env["PC_FILE"] != "":
		return fakedocker.ExecResult{}
	case strings.Contains(strings.Join(call.Cmd, " "), "information_schema.tables"):
		return fakedocker.ExecResult{Stdout: "1\n"}
	case env["PC_DATABASE"] != "":
		return fakedocker.ExecResult{Stdout: "-- the dump of " + env["PC_DATABASE"] + "\nCREATE TABLE orders (id int);\n"}
	}
	return fakedocker.ExecResult{Code: 127, Stderr: "unknown script"}
}

// statements returns the SQL the server got, oldest first.
func (f *fakeDatabase) statements() []fakeExec {
	f.mu.Lock()
	defer f.mu.Unlock()

	found := []fakeExec{}
	for _, exec := range f.execs {
		if exec.env["PC_SQL"] != "" {
			found = append(found, exec)
		}
	}
	return found
}

type databaseHarness struct {
	*composeHarness
	server *fakeDatabase
	admin  string
}

func newDatabaseHarness(t *testing.T, prepare ...func(*agent.Agent)) *databaseHarness {
	t.Helper()

	c := newComposeHarness(t, "dbhost", prepare...)
	server := &fakeDatabase{}
	c.docker.OnExec = server.answer

	_, admin := testutil.NewUser(t, c.app, "admin@example.com", schema.RoleAdmin)

	return &databaseHarness{composeHarness: c, server: server, admin: admin}
}

func answerOf(t *testing.T, body string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
}

// createServer creates a shared server and waits until its stack runs.
func (d *databaseHarness) createServer(name, engine string) *core.Record {
	d.t.Helper()

	status, body := d.post("/api/pc/db-servers", d.operator, map[string]any{"host": d.host.Id, "name": name, "engine": engine})
	if status != http.StatusOK {
		d.t.Fatalf("creating a server: %d %s", status, body)
	}

	var answer struct {
		Server  struct{ ID string } `json:"server"`
		Command struct{ ID string } `json:"command"`
	}
	answerOf(d.t, body, &answer)

	d.waitForStatus(answer.Command.ID, schema.CommandSucceeded)

	var server *core.Record
	d.waitFor("the server to be ready", func() bool {
		found, err := d.app.FindRecordById(schema.DBServers, answer.Server.ID)
		server = found
		return err == nil && found.GetString("status") == "ready"
	})

	return server
}

// createDatabase creates a database, for a stack if one is given, and waits for it.
func (d *databaseHarness) createDatabase(serverID, name, stackID string) *core.Record {
	d.t.Helper()

	status, body := d.post("/api/pc/databases", d.operator, map[string]any{"server": serverID, "name": name, "stack": stackID})
	if status != http.StatusOK {
		d.t.Fatalf("creating a database: %d %s", status, body)
	}

	var answer struct {
		Database struct{ ID string } `json:"database"`
		Command  struct{ ID string } `json:"command"`
	}
	answerOf(d.t, body, &answer)

	d.waitForStatus(answer.Command.ID, schema.CommandSucceeded)

	var database *core.Record
	d.waitFor("the database to be ready", func() bool {
		found, err := d.app.FindRecordById(schema.Databases, answer.Database.ID)
		database = found
		return err == nil && found.GetString("status") == "ready"
	})

	return database
}

// password opens a sealed field.
func (d *databaseHarness) password(record *core.Record, field string) string {
	d.t.Helper()

	fresh, err := d.app.FindRecordById(record.Collection().Name, record.Id)
	if err != nil {
		d.t.Fatal(err)
	}
	if fresh.GetString(field) == "" {
		return ""
	}
	plain, err := d.panel.Box.OpenString(fresh.GetString(field))
	if err != nil {
		d.t.Fatal(err)
	}
	return plain
}

func (d *databaseHarness) user(serverID, name string) *core.Record {
	d.t.Helper()

	account, err := d.app.FindFirstRecordByFilter(schema.DBUsers, "server={:server} && name={:name}", dbx.Params{"server": serverID, "name": name})
	if err != nil {
		d.t.Fatal(err)
	}
	return account
}

// deployedApp creates a stack with one service and deploys it.
func (d *databaseHarness) deployedApp(name string) string {
	d.t.Helper()

	stackID := d.newStack(name, "services:\n  web:\n    image: nginx:1\n", "TZ=Europe/Prague\n")
	d.waitForStatus(d.deploy(stackID, map[string]any{}).Id, schema.CommandSucceeded)
	return stackID
}

// noFileHolds checks that a text appears in none of the panel's database files.
func (d *databaseHarness) noFileHolds(text, what string) {
	d.t.Helper()

	files, err := filepath.Glob(filepath.Join(d.app.DataDir(), "*"))
	if err != nil {
		d.t.Fatal(err)
	}

	seen := 0
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			d.t.Fatal(err)
		}
		seen++
		if bytes.Contains(raw, []byte(text)) {
			d.t.Errorf("%s holds %s", filepath.Base(path), what)
		}
	}
	if seen < 2 {
		d.t.Errorf("only %d files were checked", seen)
	}
}

func TestASharedServerRunsWithoutPublishedPorts(t *testing.T) {
	d := newDatabaseHarness(t)

	server := d.createServer("db-main", "mariadb")

	container := d.docker.Find("db-main-db-1")
	if container == nil {
		t.Fatalf("no server container: %v", d.docker.Names())
	}
	if container.Status != "running" || container.Health != "healthy" || container.Labels["pocketcloud.role"] != "db" {
		t.Errorf("container: %s %s %v", container.Status, container.Health, container.Labels)
	}
	if bindings := container.Host["PortBindings"]; bindings != nil {
		t.Errorf("published ports: %v", bindings)
	}

	stack := d.stack(server.GetString("stack"))
	if stack.GetString("source") != "db_server" || !strings.Contains(stack.GetString("env"), "${secret:db-admin/db-main}") {
		t.Errorf("stack: %s %q", stack.GetString("source"), stack.GetString("env"))
	}

	// the admin password reached the container's environment, through the env file only
	admin := d.password(server, "admin_password")
	if len(admin) != 32 {
		t.Fatalf("admin password %q", admin)
	}
	env, _ := container.Config["Env"].([]any)
	found := false
	for _, item := range env {
		if item == "MARIADB_ROOT_PASSWORD="+admin {
			found = true
		}
	}
	if !found {
		t.Errorf("the server did not get its password: %v", env)
	}
	d.noFileHolds(admin, "the administrator's password")

	// nobody else's stack can use the server's password
	other := d.newStack("thief", "services:\n  web:\n    image: nginx:1\n    environment:\n      X: ${X}\n", "X=${secret:db-admin/db-main}\n")
	status, body := d.post("/api/pc/stacks/"+other+"/deploy", d.operator, map[string]any{})
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, "only the server's own stack") {
		t.Errorf("another stack used the admin password: %d %s", status, body)
	}

	// and the server's stack cannot be deleted from under it
	status, body = testutil.Do(t, d.handler, testutil.Request{Method: http.MethodDelete, URL: "/api/collections/stacks/records/" + stack.Id, Token: d.operator})
	if status != http.StatusBadRequest {
		t.Errorf("deleting the server's stack: %d %s", status, body)
	}
}

func TestADatabaseReachesItsStackWithoutItsPasswordBeingStored(t *testing.T) {
	d := newDatabaseHarness(t)

	server := d.createServer("db-main", "mariadb")
	stackID := d.deployedApp("shop")
	before := len(d.calls())

	database := d.createDatabase(server.Id, "shop", stackID)

	// the stack is connected as soon as the database is there
	d.waitFor("the stack to be connected", func() bool { return d.stack(stackID).GetInt("current_revision") == 2 })

	account := d.user(server.Id, "shop")
	password := d.password(account, "password")
	admin := d.password(server, "admin_password")
	if len(password) != 32 || account.GetString("secret_name") != "db/shop" {
		t.Fatalf("user: %q %s", password, account.GetString("secret_name"))
	}

	// the server's own client created it, with every password in the environment only
	statements := d.server.statements()
	if len(statements) != 1 {
		t.Fatalf("statements: %+v", statements)
	}
	create := statements[0]
	if create.container != d.docker.Find("db-main-db-1").ID || create.env["MYSQL_PWD"] != admin ||
		!strings.Contains(create.env["PC_SQL"], "IDENTIFIED BY '"+password+"'") || !strings.Contains(create.env["PC_SQL"], "GRANT ALL PRIVILEGES ON `shop`.*") {
		t.Errorf("create: %+v", create)
	}
	if line := strings.Join(create.cmd, " "); strings.Contains(line, password) || strings.Contains(line, admin) {
		t.Errorf("a password on the command line: %s", line)
	}

	// the stack got its variables in a new revision, with the password as a reference
	stack := d.stack(stackID)
	for _, want := range []string{"DB_HOST=db-main", "DB_PORT=3306", "DB_NAME=shop", "DB_USER=shop", "DB_PASSWORD=${secret:db/shop}"} {
		if !strings.Contains(stack.GetString("env"), want) {
			t.Errorf("env lacks %s: %q", want, stack.GetString("env"))
		}
	}
	if stack.GetInt("current_revision") != 2 {
		t.Errorf("revision %d", stack.GetInt("current_revision"))
	}

	// the network joins the application and the server, which is known by its name
	var network *fakedocker.Network
	d.waitFor("the application to join its network", func() bool {
		network = d.docker.FindNetwork("pcdb-shop")
		if network == nil {
			return false
		}
		_, app := network.Members[d.docker.Find("shop-web-1").ID]
		return app
	})
	if !network.Internal {
		t.Errorf("the network is not internal: %+v", network)
	}
	aliases := network.Members[d.docker.Find("db-main-db-1").ID]
	if len(aliases) != 1 || aliases[0] != "db-main" {
		t.Errorf("server aliases: %v", aliases)
	}

	// the stack was deployed again, with the network in the override and the
	// password in the env file for the duration of the deploy
	var up *composeCall
	d.waitFor("the stack to be deployed again", func() bool {
		for _, call := range d.calls()[before:] {
			if call.Project == "shop" && call.Verb == "up" {
				up = &call
			}
		}
		return up != nil
	})
	if len(up.Files) != 2 || !strings.HasSuffix(up.Files[1], "compose.pocketcloud.yaml") {
		t.Errorf("files: %v", up.Files)
	}
	if !strings.Contains(up.EnvContent, `DB_PASSWORD="`+password+`"`) {
		t.Errorf("the deploy did not get the password: %q", up.EnvContent)
	}
	if database.GetString("owner_kind") != "stack" || database.GetString("stack") != stackID {
		t.Errorf("owner: %s %s", database.GetString("owner_kind"), database.GetString("stack"))
	}

	// and the password is nowhere in the panel's data: not in commands, revisions or the audit
	commandsWithIt, err := d.app.FindRecordsByFilter(schema.Commands, "payload~{:password} || result~{:password} || error~{:password}", "", 0, 0, dbx.Params{"password": password})
	if err != nil || len(commandsWithIt) != 0 {
		t.Errorf("commands hold the password: %d %v", len(commandsWithIt), err)
	}
	d.noFileHolds(password, "a database password")

	// the numbers of the server reach the records
	status, body := d.post("/api/pc/db-servers/"+server.Id+"/stats", d.operator, map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("stats: %d %s", status, body)
	}
	d.waitFor("the numbers of the server", func() bool {
		fresh, err := d.app.FindRecordById(schema.Databases, database.Id)
		return err == nil && fresh.GetInt("size_bytes") == 2048 && fresh.GetInt("connections") == 3
	})
	server, _ = d.app.FindRecordById(schema.DBServers, server.Id)
	if server.GetString("version") != "11.8.2-MariaDB" || server.GetInt("max_connections") != 200 || server.GetInt("connections") != 4 {
		t.Errorf("server numbers: %s %d %d", server.GetString("version"), server.GetInt("max_connections"), server.GetInt("connections"))
	}
}

func TestApplicationsOnlyShareTheirOwnNetwork(t *testing.T) {
	d := newDatabaseHarness(t)

	server := d.createServer("pg-main", "postgres")
	first := d.deployedApp("alpha")
	second := d.deployedApp("beta")

	d.createDatabase(server.Id, "alpha", first)
	d.createDatabase(server.Id, "beta", second)

	// the deploys that connect them recreate the containers: their ids change
	var alpha, beta, db *fakedocker.Container
	d.waitFor("both applications on their networks", func() bool {
		alpha, beta, db = d.docker.Find("alpha-web-1"), d.docker.Find("beta-web-1"), d.docker.Find("pg-main-db-1")
		a, b := d.docker.FindNetwork("pcdb-alpha"), d.docker.FindNetwork("pcdb-beta")
		if a == nil || b == nil || alpha == nil || beta == nil {
			return false
		}
		_, alphaOnA := a.Members[alpha.ID]
		_, betaOnB := b.Members[beta.ID]
		return alphaOnA && betaOnB
	})

	a, b := d.docker.FindNetwork("pcdb-alpha"), d.docker.FindNetwork("pcdb-beta")
	if _, crossed := a.Members[beta.ID]; crossed {
		t.Error("beta is on alpha's network")
	}
	if _, crossed := b.Members[alpha.ID]; crossed {
		t.Error("alpha is on beta's network")
	}
	if _, onA := a.Members[db.ID]; !onA {
		t.Error("the server is not on alpha's network")
	}
	if _, onB := b.Members[db.ID]; !onB {
		t.Error("the server is not on beta's network")
	}

	// an application deploys while its server is not there: the server joins later
	d.docker.RemoveContainer(d.docker.Find("pg-main-db-1").ID)
	d.waitForStatus(d.deploy(first, map[string]any{}).Id, schema.CommandSucceeded)
	d.waitForStatus(d.deploy(server.GetString("stack"), map[string]any{}).Id, schema.CommandSucceeded)
	d.waitFor("the new server container on alpha's network", func() bool {
		server, network := d.docker.Find("pg-main-db-1"), d.docker.FindNetwork("pcdb-alpha")
		if server == nil || network == nil {
			return false
		}
		aliases, member := network.Members[server.ID]
		return member && len(aliases) == 1 && aliases[0] == "pg-main"
	})

	// a PostgreSQL application also gets a URL
	if env := d.stack(first).GetString("env"); !strings.Contains(env, "DATABASE_URL=postgres://alpha:${secret:db/alpha}@pg-main:5432/alpha") {
		t.Errorf("env: %q", env)
	}

	// deploying the server again keeps it on both networks through its override
	before := len(d.calls())
	serverStack := server.GetString("stack")
	d.waitForStatus(d.deploy(serverStack, map[string]any{}).Id, schema.CommandSucceeded)

	var up *composeCall
	for _, call := range d.calls()[before:] {
		if call.Project == "pg-main" && call.Verb == "up" {
			up = &call
		}
	}
	if up == nil || len(up.Files) != 2 {
		t.Fatalf("the server's deploy: %+v", up)
	}
	override, err := os.ReadFile(up.Files[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pcdb-alpha", "pcdb-beta", "pg-main", "external: true"} {
		if !strings.Contains(string(override), want) {
			t.Errorf("the server's override lacks %s:\n%s", want, override)
		}
	}
}

func TestARotationSwitchesThePasswordOnceTheServerHasIt(t *testing.T) {
	d := newDatabaseHarness(t)

	server := d.createServer("db-main", "mariadb")
	stackID := d.deployedApp("shop")
	database := d.createDatabase(server.Id, "shop", stackID)

	account := d.user(server.Id, "shop")
	old := d.password(account, "password")

	// the deploy that connects the stack comes first
	d.waitFor("the stack to be connected", func() bool { return d.stack(stackID).GetInt("deployed_revision") == 2 })
	before := len(d.calls())

	status, body := d.post("/api/pc/databases/"+database.Id+"/rotate", d.operator, map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("rotate: %d %s", status, body)
	}
	var command struct{ ID string }
	answerOf(t, body, &command)
	d.waitForStatus(command.ID, schema.CommandSucceeded)

	d.waitFor("the new password", func() bool { return d.password(account, "password") != old })
	fresh := d.password(account, "password")
	if d.password(account, "pending_password") != "" || d.user(server.Id, "shop").GetDateTime("rotated_at").IsZero() {
		t.Error("the rotation did not settle")
	}

	statements := d.server.statements()
	last := statements[len(statements)-1]
	if last.env["PC_SQL"] != "ALTER USER 'shop'@'%' IDENTIFIED BY '"+fresh+"';" {
		t.Errorf("rotate SQL: %q", last.env["PC_SQL"])
	}

	// the application was restarted with the new password
	d.waitFor("the restart", func() bool {
		for _, call := range d.calls()[before:] {
			if call.Project == "shop" && call.Verb == "up" && strings.Contains(call.EnvContent, fresh) && !strings.Contains(call.EnvContent, old) {
				return true
			}
		}
		return false
	})

	d.noFileHolds(fresh, "the new password")
}

func TestARotationThatFailsKeepsTheOldPassword(t *testing.T) {
	d := newDatabaseHarness(t)

	server := d.createServer("db-main", "mariadb")
	database := d.createDatabase(server.Id, "shop", "")
	account := d.user(server.Id, "shop")
	old := d.password(account, "password")

	var refused string
	d.docker.OnExec = func(call *fakedocker.ExecCall, _ []byte) fakedocker.ExecResult {
		for _, item := range call.Env {
			if sql, ok := strings.CutPrefix(item, "PC_SQL=ALTER USER"); ok {
				refused = sql
				// a client may well repeat the statement, password and all
				return fakedocker.ExecResult{Code: 1, Stderr: "ERROR 1396 (HY000) at line 1: Operation ALTER USER failed for" + sql}
			}
		}
		return fakedocker.ExecResult{}
	}

	status, body := d.post("/api/pc/databases/"+database.Id+"/rotate", d.operator, map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("rotate: %d %s", status, body)
	}
	var command struct{ ID string }
	answerOf(t, body, &command)
	failed := d.waitForStatus(command.ID, schema.CommandFailed)

	_, newPassword, _ := strings.Cut(refused, "IDENTIFIED BY '")
	newPassword = strings.TrimSuffix(newPassword, "';")
	if len(newPassword) != 32 || strings.Contains(failed.GetString("error"), newPassword) || !strings.Contains(failed.GetString("error"), "IDENTIFIED BY '***'") {
		t.Errorf("the error is not masked: %q (password %q)", failed.GetString("error"), newPassword)
	}

	d.waitFor("the pending password to go", func() bool { return d.password(account, "pending_password") == "" })
	if d.password(account, "password") != old {
		t.Error("the password changed although the server refused it")
	}
}

func TestDropKeepsAFinalDumpAndExportsCanBeDownloaded(t *testing.T) {
	d := newDatabaseHarness(t)

	server := d.createServer("db-main", "mariadb")
	stackID := d.deployedApp("shop")
	database := d.createDatabase(server.Id, "shop", stackID)

	// only admins export: a dump holds every row
	status, body := d.post("/api/pc/databases/"+database.Id+"/export", d.operator, map[string]any{})
	if status != http.StatusForbidden {
		t.Errorf("an operator exported: %d %s", status, body)
	}
	status, body = d.post("/api/pc/databases/"+database.Id+"/export", d.admin, map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("export: %d %s", status, body)
	}
	var command struct{ ID string }
	answerOf(t, body, &command)
	d.waitForStatus(command.ID, schema.CommandSucceeded)

	dumps, err := d.app.FindRecordsByFilter(schema.DBDumps, "kind='export'", "", 0, 0)
	if err != nil || len(dumps) != 1 {
		t.Fatalf("dumps: %v %v", dumps, err)
	}
	export := dumps[0]
	if export.GetString("database_name") != "shop" || export.GetDateTime("expires_at").Time().Before(time.Now().Add(6*24*time.Hour)) {
		t.Errorf("export: %v", export)
	}

	file, err := d.app.NewFilesystem()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := file.GetReader(export.BaseFilesPath() + "/" + export.GetString("file"))
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || len(content) == 0 || !bytes.HasPrefix(content, []byte{0x1f, 0x8b}) {
		t.Errorf("the dump is not gzipped: %d bytes, %v", len(content), err)
	}

	// dropping keeps a final dump first, then removes the records and the network
	status, body = testutil.Do(t, d.handler, testutil.Request{Method: http.MethodDelete, URL: "/api/pc/databases/" + database.Id, Token: d.operator})
	if status != http.StatusOK {
		t.Fatalf("drop: %d %s", status, body)
	}
	answerOf(t, body, &command)
	d.waitForStatus(command.ID, schema.CommandSucceeded)

	// the database, its user and the stack's network to the server go
	d.waitFor("the records to go", func() bool {
		_, err := d.app.FindRecordById(schema.Databases, database.Id)
		users, _ := d.app.FindRecordsByFilter(schema.DBUsers, "server={:server}", "", 0, 0, dbx.Params{"server": server.Id})
		networks, _ := d.app.FindRecordsByFilter(schema.DBNetworks, "server={:server}", "", 0, 0, dbx.Params{"server": server.Id})
		return err != nil && len(users) == 0 && len(networks) == 0
	})
	finals, err := d.app.FindRecordsByFilter(schema.DBDumps, "kind='final'", "", 0, 0)
	if err != nil || len(finals) != 1 || finals[0].GetInt("size_bytes") == 0 {
		t.Errorf("final dumps: %v %v", finals, err)
	}

	statements := d.server.statements()
	if drop := statements[len(statements)-1].env["PC_SQL"]; drop != "DROP DATABASE IF EXISTS `shop`;\nDROP USER IF EXISTS 'shop'@'%';" {
		t.Errorf("drop SQL: %q", drop)
	}

	// the stack loses the variables the panel added: a reference to the secret
	// that is gone would make its next deploy fail
	d.waitFor("the variables to go", func() bool {
		return !strings.Contains(d.stack(stackID).GetString("env"), "${secret:db/shop}")
	})
	d.waitForStatus(d.deploy(stackID, map[string]any{}).Id, schema.CommandSucceeded)
}

func TestDedicatedServersAreFoundAndLinked(t *testing.T) {
	d := newDatabaseHarness(t)

	d.createServer("pg-main", "postgres")

	stackID := d.newStack("analytics", "services:\n  db:\n    image: postgres:16-alpine\n  web:\n    image: umami:2\n", "")
	d.waitForStatus(d.deploy(stackID, map[string]any{}).Id, schema.CommandSucceeded)

	status, body := d.post("/api/pc/stacks/"+stackID+"/save", d.operator, map[string]any{"secrets": map[string]any{"set": map[string]string{"DB_PASS": "dedicated-secret"}}})
	if status != http.StatusOK {
		t.Fatalf("secrets: %d %s", status, body)
	}

	var candidates []struct {
		DockerID string   `json:"docker_id"`
		Name     string   `json:"name"`
		Label    string   `json:"label"`
		MoveTo   string   `json:"move_to"`
		Ext      []string `json:"extensions"`
	}
	d.waitFor("the dedicated server to be found", func() bool {
		status, body := testutil.Do(t, d.handler, testutil.Request{Method: http.MethodGet, URL: "/api/pc/db-servers/candidates", Token: d.viewer})
		if status != http.StatusOK {
			t.Fatalf("candidates: %d %s", status, body)
		}
		answerOf(t, body, &candidates)
		return len(candidates) == 1
	})
	found := candidates[0]
	if found.Name != "analytics-db-1" || found.Label != "PostgreSQL 16" || found.MoveTo != "pg-main" {
		t.Errorf("candidate: %+v", found)
	}

	status, body = d.post("/api/pc/db-servers/link", d.operator, map[string]any{
		"host": d.host.Id, "docker_id": found.DockerID, "admin_user": "umami", "secret_key": "DB_PASS",
	})
	if status != http.StatusOK {
		t.Fatalf("link: %d %s", status, body)
	}
	var linked struct{ ID string }
	answerOf(t, body, &linked)

	server, err := d.app.FindRecordById(schema.DBServers, linked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if server.GetString("kind") != "dedicated" || d.password(server, "admin_password") != "dedicated-secret" || server.GetString("stack") != stackID {
		t.Errorf("linked: %v", server)
	}

	// once linked it is no longer a candidate, and its numbers are read
	status, body = testutil.Do(t, d.handler, testutil.Request{Method: http.MethodGet, URL: "/api/pc/db-servers/candidates", Token: d.viewer})
	if status != http.StatusOK || strings.Contains(body, "analytics-db-1") {
		t.Errorf("still a candidate: %d %s", status, body)
	}
	d.waitFor("the numbers of the dedicated server", func() bool {
		fresh, err := d.app.FindRecordById(schema.DBServers, server.Id)
		return err == nil && fresh.GetString("version") == "11.8.2-MariaDB"
	})

	// its databases are listed as the stack's, with users the panel does not know
	var listed *core.Record
	d.waitFor("the dedicated server's database to be listed", func() bool {
		found, err := d.app.FindFirstRecordByFilter(schema.Databases, "server={:server} && name='shop'", dbx.Params{"server": server.Id})
		listed = found
		return err == nil
	})
	if listed.GetString("owner_kind") != "stack" || listed.GetString("stack") != stackID || listed.GetInt("size_bytes") != 2048 {
		t.Errorf("listed: %v", listed)
	}
	status, body = d.post("/api/pc/databases/"+listed.Id+"/rotate", d.operator, map[string]any{})
	if status != http.StatusBadRequest || !strings.Contains(body, "its stack") {
		t.Errorf("rotating a dedicated database: %d %s", status, body)
	}
}

func TestAdminerIsOnlyReachableThroughTheRelay(t *testing.T) {
	// Adminer is a web server in a container nobody can reach but the agent; here it
	// is a test server the agent dials instead
	var mu sync.Mutex
	seen := []*http.Request{}
	adminer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Clone(context.Background()))
		mu.Unlock()

		w.Header().Set("Connection", "close")
		switch {
		case r.Method == http.MethodPost:
			http.Redirect(w, r, r.URL.Path+"?server=db-main&username=shop&db=shop", http.StatusFound)
		case r.URL.Query().Get("username") != "":
			_, _ = fmt.Fprint(w, "<h1>Adminer: tables of shop</h1>")
		default:
			http.SetCookie(w, &http.Cookie{Name: "adminer_sid", Value: "abc", Path: r.URL.Path})
			_, _ = fmt.Fprint(w, "<form method=post><button>Login</button></form>")
		}
	}))
	defer adminer.Close()

	dialed := make(chan string, 16)
	d := newDatabaseHarness(t, func(a *agent.Agent) {
		a.DialTool = func(ctx context.Context, address string) (net.Conn, error) {
			dialed <- address
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", adminer.Listener.Addr().String())
		}
	})
	d.panel.AdminerTTL = 5 * time.Second
	base := d.serve()

	server := d.createServer("db-main", "mariadb")
	database := d.createDatabase(server.Id, "shop", "")
	password := d.password(d.user(server.Id, "shop"), "password")

	// only admins start Adminer
	status, body := d.post("/api/pc/databases/"+database.Id+"/adminer", d.operator, map[string]any{})
	if status != http.StatusForbidden {
		t.Errorf("an operator started Adminer: %d %s", status, body)
	}
	status, body = d.post("/api/pc/databases/"+database.Id+"/adminer", d.admin, map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("adminer: %d %s", status, body)
	}
	var session struct {
		URL string `json:"url"`
	}
	answerOf(t, body, &session)
	prefix, _, _ := strings.Cut(session.URL, "?")

	type page struct {
		status  int
		header  http.Header
		cookies []*http.Cookie
		body    string
	}
	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(url string, cookies ...*http.Cookie) page {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, base+url, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, cookie := range cookies {
			request.AddCookie(cookie)
		}
		response, err := browser.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return page{response.StatusCode, response.Header, response.Cookies(), string(raw)}
	}

	// the ticket becomes a cookie of this browser, once
	first := get(session.URL)
	if first.status != http.StatusSeeOther || first.header.Get("Location") != prefix {
		t.Fatalf("ticket: %d %s", first.status, first.header.Get("Location"))
	}
	var cookie *http.Cookie
	for _, candidate := range first.cookies {
		if candidate.Name == "pc_adminer" {
			cookie = candidate
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != prefix {
		t.Fatalf("cookie: %+v", cookie)
	}
	if again := get(session.URL); again.status != http.StatusForbidden {
		t.Errorf("a ticket worked twice: %d", again.status)
	}
	if stranger := get(prefix); stranger.status != http.StatusForbidden {
		t.Errorf("another browser got in: %d", stranger.status)
	}

	// the page comes from Adminer, through the relay, once Adminer runs
	var login page
	d.waitFor("Adminer to answer", func() bool {
		login = get(prefix, cookie)
		return login.status == http.StatusOK
	})
	if !strings.Contains(login.body, "Login") {
		t.Errorf("page: %s", login.body)
	}

	// a form posted to it is answered with a redirect that stays under the session
	request, err := http.NewRequest(http.MethodPost, base+prefix, strings.NewReader("auth[driver]=server"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	posted, err := browser.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	posted.Body.Close()
	if posted.StatusCode != http.StatusFound || !strings.HasPrefix(posted.Header.Get("Location"), prefix+"?server=db-main") {
		t.Errorf("post: %d %s", posted.StatusCode, posted.Header.Get("Location"))
	}
	if next := get(posted.Header.Get("Location"), cookie); !strings.Contains(next.body, "tables of shop") {
		t.Errorf("after signing in: %s", next.body)
	}

	// Adminer saw the session's path, but neither the panel's cookie nor any token
	mu.Lock()
	for _, request := range seen {
		if !strings.HasPrefix(request.URL.Path, prefix) {
			t.Errorf("Adminer saw %s", request.URL.Path)
		}
		if _, err := request.Cookie("pc_adminer"); err == nil || request.Header.Get("Authorization") != "" {
			t.Error("Adminer saw the panel's credentials")
		}
	}
	requests := len(seen)
	mu.Unlock()
	if requests < 3 {
		t.Errorf("Adminer saw %d requests", requests)
	}

	// the container was on a network of its own, with the user's credentials only in
	// its environment, and the sign-in plugin installed
	var container *fakedocker.Container
	for _, name := range d.docker.Names() {
		if strings.HasPrefix(name, "pcadm-") {
			container = d.docker.Find(name)
		}
	}
	if container == nil {
		t.Fatalf("no Adminer container: %v", d.docker.Names())
	}
	network, _ := container.Host["NetworkMode"].(string)
	if !strings.HasPrefix(network, "pcadm-") || container.Host["PortBindings"] != nil {
		t.Errorf("Adminer's network: %v", container.Host)
	}
	env, _ := container.Config["Env"].([]any)
	if !containsAny(env, "PC_PASSWORD="+password) || !containsAny(env, "PC_USER=shop") || !containsAny(env, "PC_SERVER=db-main") {
		t.Errorf("Adminer's environment: %v", env)
	}
	if plugin := d.docker.File(container.Name, "/var/www/html/plugins-enabled/pocketcloud.php"); !bytes.Contains(plugin, []byte("function credentials()")) {
		t.Errorf("plugin: %s", plugin)
	}
	address := <-dialed
	if !strings.HasSuffix(address, ":8080") || strings.HasPrefix(address, "127.") {
		t.Errorf("the agent dialled %s", address)
	}

	// the session is a security event
	events, err := d.app.FindRecordsByFilter(schema.AuditLog, "action='adminer_session'", "", 0, 0)
	if err != nil || len(events) != 1 || events[0].GetString("record_label") != "db-main/shop" {
		t.Errorf("audit: %v %v", events, err)
	}

	// and after its time, everything is gone
	d.waitFor("Adminer to be removed", func() bool {
		for _, name := range d.docker.Names() {
			if strings.HasPrefix(name, "pcadm-") {
				return false
			}
		}
		return d.docker.FindNetwork(network) == nil
	})
	if ended := get(prefix, cookie); ended.status != http.StatusNotFound {
		t.Errorf("after the session: %d", ended.status)
	}
	d.noFileHolds(password, "a database password")
}

func containsAny(list []any, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
