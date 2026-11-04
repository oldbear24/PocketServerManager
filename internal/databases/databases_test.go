package databases

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/oldbear24/PocketServerManager/internal/stacks"
)

func TestNames(t *testing.T) {
	for _, name := range []string{"db-main", "pg1", "a"} {
		if err := ValidServerName(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, name := range []string{"", "DB", "db_main", "-db", strings.Repeat("a", 49)} {
		if ValidServerName(name) == nil {
			t.Errorf("%q passed as a server name", name)
		}
	}

	for _, name := range []string{"shop", "shop_staging", "a1"} {
		if err := ValidDatabaseName(MariaDB, name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, name := range []string{"Shop", "1shop", "shop-x", "shop;drop", strings.Repeat("a", 64)} {
		if ValidDatabaseName(MariaDB, name) == nil {
			t.Errorf("%q passed as a database name", name)
		}
	}
	if ValidDatabaseName(MariaDB, "mysql") == nil || ValidDatabaseName(Postgres, "template1") == nil {
		t.Error("an engine's own name passed")
	}
	if ValidDatabaseName(Postgres, "mysql") != nil {
		t.Error("only the engine's own names are reserved")
	}
}

func TestPasswords(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		password := NewPassword()
		if len(password) != 32 || strings.Trim(password, passwordAlphabet) != "" {
			t.Fatalf("password %q", password)
		}
		if seen[password] {
			t.Fatal("a password came twice")
		}
		seen[password] = true
	}
}

func TestBufferSize(t *testing.T) {
	for memory, want := range map[uint64]uint64{
		512 * mib:     128 * mib,  // a small VPS gets the floor
		8 << 30:       1024 * mib, // an eighth
		64 << 30:      2048 * mib, // and no more than 2 GiB
		8<<30 + 12345: 1024 * mib, // in whole MiB
	} {
		if got := BufferSize(memory); got != want {
			t.Errorf("%d: %d, want %d", memory, got, want)
		}
	}
}

func TestEnvOfAConnection(t *testing.T) {
	maria := Connection{Engine: MariaDB, Server: "db-main", Database: "shop", User: "shop"}
	want := []string{"DB_HOST=db-main", "DB_PORT=3306", "DB_NAME=shop", "DB_USER=shop", "DB_PASSWORD=${secret:db/shop}"}
	if got := maria.EnvLines(); !slices.Equal(got, want) {
		t.Errorf("MariaDB: %v", got)
	}

	pg := Connection{Engine: Postgres, Server: "pg-main", Database: "notes", User: "notes"}
	if got := pg.EnvLines(); got[len(got)-1] != "DATABASE_URL=postgres://notes:${secret:db/notes}@pg-main:5432/notes" {
		t.Errorf("PostgreSQL: %v", got)
	}

	env, changed := AddEnv("TZ=Europe/Prague\nDB_HOST=keep-me", maria.EnvLines())
	if !changed || !strings.Contains(env, "DB_HOST=keep-me") || strings.Contains(env, "DB_HOST=db-main") || !strings.HasSuffix(env, "DB_PASSWORD=${secret:db/shop}\n") {
		t.Errorf("env: %q", env)
	}
	if _, changed := AddEnv(env, maria.EnvLines()); changed {
		t.Error("adding the same variables changed the file")
	}

	// the resolver sees the reference like any other
	plain, secrets, err := stacks.ResolveEnv(env, func(name string) (string, error) {
		if name != "db/shop" {
			t.Errorf("resolved %q", name)
		}
		return "s3cret", nil
	})
	if err != nil || secrets["DB_PASSWORD"] != "s3cret" || strings.Contains(plain, "s3cret") {
		t.Errorf("resolved: %q %v %v", plain, secrets, err)
	}
}

// load reads Compose files like the agent's Compose does, with the override last.
func load(t *testing.T, env map[string]string, files ...string) *types.Project {
	t.Helper()

	configs := []types.ConfigFile{}
	for index, content := range files {
		configs = append(configs, types.ConfigFile{Filename: []string{"compose.yaml", "compose.pocketcloud.yaml"}[index], Content: []byte(content)})
	}

	project, err := loader.LoadWithContext(context.Background(), types.ConfigDetails{
		WorkingDir:  "/",
		ConfigFiles: configs,
		Environment: env,
	}, func(options *loader.Options) {
		options.SetProjectName("test", true)
		options.ResolvePaths = false
	})
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func TestServerCompose(t *testing.T) {
	for _, engine := range []Engine{MariaDB, Postgres} {
		t.Run(engine.Name, func(t *testing.T) {
			compose := ServerCompose(Server{
				Name: "db-main", Engine: engine, Image: engine.Image, Volume: VolumeName("db-main"),
				MaxConnections: engine.MaxConnections, Buffer: BufferSize(8 << 30),
			})

			result := stacks.Validate(context.Background(), "db-main", compose, ServerEnv("db-main"), nil)
			if !result.Valid {
				t.Fatalf("%v\n%s", result.Errors, compose)
			}

			project := load(t, map[string]string{ServerEnvKey: "x"}, compose)
			service := project.Services[ServerService]
			if service.Image != engine.Image || len(service.Ports) != 0 || service.Labels["pocketcloud.role"] != "db" || service.HealthCheck == nil {
				t.Errorf("service: %+v", service)
			}
			if project.Volumes["data"].Name != "pcdb-db-main-data" {
				t.Errorf("volumes: %+v", project.Volumes)
			}
			if !strings.Contains(strings.Join(service.Command, " "), "1024M") {
				t.Errorf("no memory preset: %v", service.Command)
			}
			if !strings.Contains(ServerEnv("db-main"), "${secret:db-admin/db-main}") {
				t.Error("the admin password is not a reference")
			}
		})
	}
}

const appCompose = `services:
  web:
    image: nginx
  worker:
    image: busybox
    networks: [backend]
  sidecar:
    image: busybox
    network_mode: "service:web"
networks:
  backend: {}
`

func TestOverrideOfAnApplication(t *testing.T) {
	override, err := Override(appCompose, []Join{{Network: "pcdb-shop"}}, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	project := load(t, nil, appCompose, override)

	networksOf := func(name string) []string {
		names := []string{}
		for network := range project.Services[name].Networks {
			names = append(names, network)
		}
		slices.Sort(names)
		return names
	}

	// a service on the default network stays there, one with networks keeps them
	if got := networksOf("web"); !slices.Equal(got, []string{"default", "pcdb-shop"}) {
		t.Errorf("web: %v", got)
	}
	if got := networksOf("worker"); !slices.Equal(got, []string{"backend", "pcdb-shop"}) {
		t.Errorf("worker: %v", got)
	}
	if got := networksOf("sidecar"); len(got) != 0 {
		t.Errorf("sidecar: %v", got)
	}
	if !project.Networks["pcdb-shop"].External {
		t.Errorf("networks: %+v", project.Networks)
	}

	if none, err := Override(appCompose, nil, "", nil); none != "" || err != nil {
		t.Errorf("without networks: %q %v", none, err)
	}
}

func TestOverrideOfAServer(t *testing.T) {
	compose := ServerCompose(Server{Name: "db-main", Engine: MariaDB, Image: MariaDB.Image, Volume: "v", MaxConnections: 200, Buffer: 128 * mib})

	// on the network of an application whose own database moved here, the server
	// also answers to that database service's names
	override, err := Override(compose, []Join{
		{Network: "pcdb-ghost", Aliases: []string{"db-main", "db", "ghost-db-1"}},
		{Network: "pcdb-shop", Aliases: []string{"db-main"}},
	}, ServerService, nil)
	if err != nil {
		t.Fatal(err)
	}

	project := load(t, map[string]string{ServerEnvKey: "x"}, compose, override)
	networks := project.Services[ServerService].Networks
	if len(networks) != 3 || networks["default"] == nil || !slices.Equal(networks["pcdb-shop"].Aliases, []string{"db-main"}) ||
		!slices.Equal(networks["pcdb-ghost"].Aliases, []string{"db-main", "db", "ghost-db-1"}) {
		t.Errorf("networks: %+v", networks)
	}
}

const movedCompose = `services:
  db:
    image: postgres:16
  cache:
    image: redis:7
  app:
    image: ghost:5
    depends_on:
      db:
        condition: service_healthy
      cache:
        condition: service_started
        restart: true
  worker:
    image: ghost:5
    depends_on: [db]
  plain:
    image: busybox
`

// TestOverrideKeepsAMovedDatabaseStopped: the database service of a stack whose data
// moved to a shared server is in a profile nobody activates, and nothing depends
// on it any more.
func TestOverrideKeepsAMovedDatabaseStopped(t *testing.T) {
	override, err := Override(movedCompose, []Join{{Network: "pcdb-ghost"}}, "", []string{"db"})
	if err != nil {
		t.Fatal(err)
	}

	project := load(t, nil, movedCompose, override)

	if _, active := project.Services["db"]; active {
		t.Fatal("the moved database service is still active")
	}
	if disabled, ok := project.DisabledServices["db"]; !ok || !slices.Equal(disabled.Profiles, []string{RetiredProfile}) {
		t.Errorf("disabled: %+v", project.DisabledServices)
	}

	app := project.Services["app"]
	if len(app.DependsOn) != 1 || app.DependsOn["cache"].Condition != "service_started" || !app.DependsOn["cache"].Restart {
		t.Errorf("app keeps its other dependency as it was: %+v", app.DependsOn)
	}
	if worker := project.Services["worker"]; len(worker.DependsOn) != 0 {
		t.Errorf("worker: %+v", worker.DependsOn)
	}
	for _, name := range []string{"app", "worker", "plain", "cache"} {
		if _, ok := project.Services[name].Networks["pcdb-ghost"]; !ok {
			t.Errorf("%s is not on the database network", name)
		}
	}

	// without networks the retirement alone is an override too
	if alone, err := Override(movedCompose, nil, "", []string{"db"}); err != nil || !strings.Contains(alone, RetiredProfile) {
		t.Errorf("retirement alone: %q %v", alone, err)
	}

	// the agent reads back what the override keeps stopped
	if retired := RetiredServices(override); !slices.Equal(retired, []string{"db"}) {
		t.Errorf("retired services: %v", retired)
	}
	plain, err := Override(movedCompose, []Join{{Network: "pcdb-ghost"}}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if retired := RetiredServices(plain); len(retired) != 0 {
		t.Errorf("retired without a move: %v", retired)
	}
}

func TestRetireProblem(t *testing.T) {
	if err := RetireProblem(movedCompose, "db"); err != nil {
		t.Errorf("depends_on is no problem: %v", err)
	}
	if err := RetireProblem(movedCompose, "nothing"); err == nil {
		t.Error("an unknown service")
	}
	for _, compose := range []string{
		"services:\n  db: {image: postgres}\n  app: {image: x, network_mode: \"service:db\"}\n",
		"services:\n  db: {image: postgres}\n  app: {image: x, links: [\"db:database\"]}\n",
		"services:\n  db: {image: postgres}\n  app: {image: x, volumes_from: [db]}\n",
	} {
		if err := RetireProblem(compose, "db"); err == nil {
			t.Errorf("no problem found in %q", compose)
		}
	}
}

func TestScriptsKeepPasswordsOffTheCommandLine(t *testing.T) {
	for _, engine := range []Engine{MariaDB, Postgres} {
		sql := CreateSQL(engine, "shop", "shop", "new-password")
		scripts := []Script{
			Statements(engine, engine.AdminUser, "admin-password", sql),
			Dump(engine, engine.AdminUser, "admin-password", "shop"),
			Clone(engine, engine.AdminUser, "admin-password", "shop", "shop_copy", "shop_copy"),
			RestoreFile(engine, engine.AdminUser, "admin-password", "shop", "shop", "/tmp/pocketcloud-move.sql"),
			CountTables(engine, engine.AdminUser, "admin-password", "shop"),
		}
		for _, script := range scripts {
			line := strings.Join(script.Cmd, " ")
			if strings.Contains(line, "password") || strings.Contains(line, "shop") {
				t.Errorf("%s: the command line holds what varies: %s", engine.Name, line)
			}
			if !slices.ContainsFunc(script.Env, func(value string) bool { return strings.HasSuffix(value, "=admin-password") }) {
				t.Errorf("%s: no admin password in %v", engine.Name, script.Env)
			}
		}
		if !strings.Contains(scripts[0].Env[len(scripts[0].Env)-1], "new-password") {
			t.Errorf("%s: the statements are not in the environment", engine.Name)
		}
	}
}

func TestSQL(t *testing.T) {
	maria := CreateSQL(MariaDB, "shop", "shop", "pw")
	for _, want := range []string{"CREATE DATABASE IF NOT EXISTS `shop`", "CREATE USER IF NOT EXISTS 'shop'@'%' IDENTIFIED BY 'pw'", "GRANT ALL PRIVILEGES ON `shop`.* TO 'shop'@'%'"} {
		if !strings.Contains(maria, want) {
			t.Errorf("MariaDB create lacks %q:\n%s", want, maria)
		}
	}

	pg := CreateSQL(Postgres, "shop", "shop", "pw")
	for _, want := range []string{`ALTER ROLE "shop" WITH LOGIN PASSWORD 'pw'`, `OWNER "shop"`, `\gexec`, `REVOKE ALL ON DATABASE "shop" FROM PUBLIC`} {
		if !strings.Contains(pg, want) {
			t.Errorf("PostgreSQL create lacks %q:\n%s", want, pg)
		}
	}

	// quotes inside values cannot end the literal
	if got := RotateSQL(MariaDB, "shop", `it's\`); got != `ALTER USER 'shop'@'%' IDENTIFIED BY 'it\'s\\';` {
		t.Errorf("MariaDB rotate: %s", got)
	}
	if got := RotateSQL(Postgres, "shop", "it's"); got != `ALTER ROLE "shop" WITH PASSWORD 'it''s';` {
		t.Errorf("PostgreSQL rotate: %s", got)
	}

	if got := DropSQL(Postgres, "shop", "shop"); !strings.Contains(got, `DROP DATABASE IF EXISTS "shop" WITH (FORCE)`) {
		t.Errorf("drop: %s", got)
	}
}

func TestParseStats(t *testing.T) {
	output := "size\tshop\t1048576\nsize\tmysql\t500\nsize\tghost\t0\nconn\tshop\t3\nconn\t\t2\nmax\t\t200\nversion\t11.8.2-MariaDB\t0\n"

	stats, err := ParseStats(MariaDB, output)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Version != "11.8.2-MariaDB" || stats.MaxConnections != 200 || stats.Connections != 5 || stats.SizeBytes != 1048576+500 {
		t.Errorf("stats: %+v", stats)
	}
	if len(stats.Databases) != 2 || stats.Databases["shop"] != (DatabaseStats{SizeBytes: 1048576, Connections: 3}) {
		t.Errorf("databases: %+v", stats.Databases)
	}

	if _, err := ParseStats(Postgres, "max\t\tlots\n"); err == nil {
		t.Error("a broken answer passed")
	}
}

func TestRecognize(t *testing.T) {
	for image, want := range map[string]string{
		"postgres:16-alpine":                              "PostgreSQL 16",
		"docker.io/library/mariadb:11.4":                  "MariaDB 11.4",
		"mysql:8.0":                                       "MySQL 8.0",
		"bitnami/postgresql:17":                           "PostgreSQL 17",
		"ghcr.io/immich-app/postgres:14-vectorchord0.4.3": "PostgreSQL 14 + VectorChord",
		"tensorchord/pgvecto-rs:pg16-v0.2.0":              "PostgreSQL 16 + pgvecto.rs",
		"postgis/postgis:16-3.4":                          "PostgreSQL 16 + PostGIS",
		"localhost:5000/postgres":                         "",
		"postgres@sha256:" + strings.Repeat("a", 64):      "PostgreSQL",
	} {
		candidate, ok := Recognize(image)
		got := ""
		if ok {
			got = candidate.Label()
		}
		if got != want {
			t.Errorf("%s: %q, want %q", image, got, want)
		}
	}

	for _, image := range []string{"nginx:1", "ghcr.io/umami-software/umami:postgresql-v2", "redis:7"} {
		if _, ok := Recognize(image); ok {
			t.Errorf("%s passed as a database", image)
		}
	}
}

func TestCanMoveTo(t *testing.T) {
	plain, _ := Recognize("postgres:16-alpine")
	if !CanMoveTo(plain, "postgres", "17.2") {
		t.Error("16 does not move to 17")
	}
	if CanMoveTo(plain, "postgres", "15") || CanMoveTo(plain, "mariadb", "17") {
		t.Error("moved to an older server or another engine")
	}
	vector, _ := Recognize("ghcr.io/immich-app/postgres:16-vectorchord0.4.3")
	if CanMoveTo(vector, "postgres", "17") {
		t.Error("a server with extensions moved")
	}
}

func TestSuggestCredentials(t *testing.T) {
	compose := `services:
  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: umami
      POSTGRES_PASSWORD: ${DB_PASS}
  maria:
    image: mariadb:11
    environment:
      - MARIADB_ROOT_PASSWORD=${ROOT_PW:-changeme}
  plain:
    image: mysql:8
    environment:
      MYSQL_USER: app
      MYSQL_PASSWORD: written-in-the-file
  bare:
    image: postgres:17
`
	for service, want := range map[string]Credentials{
		"db":      {User: "umami", PasswordVar: "DB_PASS"},
		"maria":   {User: "root", PasswordVar: "ROOT_PW"},
		"plain":   {User: "app"},
		"bare":    {User: "postgres"},
		"missing": {User: "postgres"},
	} {
		engine := Postgres
		if service == "maria" || service == "plain" {
			engine = MariaDB
		}
		if got := SuggestCredentials(compose, service, engine); got != want {
			t.Errorf("%s: %+v, want %+v", service, got, want)
		}
	}
}

func TestRemoveEnvTakesOutOnlyWhatWasAdded(t *testing.T) {
	maria := Connection{Engine: MariaDB, Server: "db-main", Database: "shop", User: "shop"}

	env, _ := AddEnv("TZ=Europe/Prague\nDB_HOST=keep-me\n", maria.EnvLines())
	// someone changed one of the lines since
	env = strings.Replace(env, "DB_NAME=shop", "DB_NAME=shop_v2", 1)

	cleaned, changed := RemoveEnv(env, maria.EnvLines())
	if !changed {
		t.Fatal("nothing was removed")
	}
	for _, gone := range []string{"DB_PORT=3306", "DB_USER=shop", "${secret:db/shop}"} {
		if strings.Contains(cleaned, gone) {
			t.Errorf("%s stayed: %q", gone, cleaned)
		}
	}
	for _, kept := range []string{"TZ=Europe/Prague", "DB_HOST=keep-me", "DB_NAME=shop_v2"} {
		if !strings.Contains(cleaned, kept) {
			t.Errorf("%s went: %q", kept, cleaned)
		}
	}

	if _, changed := RemoveEnv(cleaned, maria.EnvLines()); changed {
		t.Error("removing again changed the file")
	}
	if again, _ := RemoveEnv("A=1\nexport DB_PORT=3306\n", maria.EnvLines()); again != "A=1\n" {
		t.Errorf("an exported line: %q", again)
	}
}

func TestApplicationCredentials(t *testing.T) {
	const postgres = `services:
  db:
    image: postgres:16
    environment:
      POSTGRES_USER: umami
      POSTGRES_PASSWORD: ${DB_PASS}
`
	if got := ApplicationCredentials(postgres, "db", Postgres); got.User != "umami" || got.PasswordVar != "DB_PASS" {
		t.Errorf("postgres: %+v", got)
	}
	// without POSTGRES_USER the application signs in as the administrator
	if got := ApplicationCredentials("services:\n  db:\n    image: postgres\n    environment: [POSTGRES_PASSWORD=$PW]\n", "db", Postgres); got.User != "postgres" || got.PasswordVar != "PW" {
		t.Errorf("default user: %+v", got)
	}

	// MariaDB: the application's own user, not root
	const mariadb = `services:
  db:
    image: mariadb:11
    environment:
      MARIADB_ROOT_PASSWORD: ${ROOT_PW}
      MARIADB_USER: shop
      MARIADB_PASSWORD: ${SHOP_PW}
`
	if got := ApplicationCredentials(mariadb, "db", MariaDB); got.User != "shop" || got.PasswordVar != "SHOP_PW" {
		t.Errorf("mariadb: %+v", got)
	}
	if got := ApplicationCredentials("services:\n  db:\n    image: mariadb\n    environment: {MARIADB_ROOT_PASSWORD: x}\n", "db", MariaDB); got.User != "" {
		t.Errorf("a root-only server names no application user: %+v", got)
	}
}

func TestParseExists(t *testing.T) {
	if database, user := ParseExists("database\t1\nuser\t0\n"); !database || user {
		t.Errorf("got %v %v", database, user)
	}
	if database, user := ParseExists("database\t0\nuser\t2\n"); database || !user {
		t.Errorf("got %v %v", database, user)
	}
	for _, engine := range []Engine{MariaDB, Postgres} {
		if sql := ExistsSQL(engine, "it's", "o'brien"); strings.Contains(sql, "it's") || strings.Contains(sql, "o'brien") {
			t.Errorf("%s: names are not quoted: %s", engine.Name, sql)
		}
	}
}
