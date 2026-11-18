package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// TestSpecsMatchTheProto keeps the table of command types and the oneof in
// commands.proto in sync: a new payload cannot be forgotten here, or the other way round.
func TestSpecsMatchTheProto(t *testing.T) {
	descriptor := (&agentv1.Command{}).ProtoReflect().Descriptor()
	fields := descriptor.Oneofs().ByName("payload").Fields()

	seen := make(map[string]bool, fields.Len())

	for i := range fields.Len() {
		name := string(fields.Get(i).Name())
		seen[name] = true

		found := false
		for commandType := range Specs {
			if string(fieldName(commandType)) == name {
				found = true
			}
		}
		if !found {
			t.Errorf("the payload %s has no entry in Specs", name)
		}
	}

	for commandType, spec := range Specs {
		if !seen[string(fieldName(commandType))] {
			t.Errorf("%s has no payload in the proto", commandType)
		}
		if spec.Timeout <= 0 {
			t.Errorf("%s has no timeout", commandType)
		}
	}
}

func TestBuild(t *testing.T) {
	command, err := Build("container.stop", []byte(`{"docker_id":"abc","timeout_s":5}`))
	if err != nil {
		t.Fatal(err)
	}

	if command.GetContainerStop().GetDockerId() != "abc" || command.GetContainerStop().GetTimeoutS() != 5 {
		t.Errorf("%v", command)
	}
	if command.GetType() != "container.stop" {
		t.Errorf("type: %s", command.GetType())
	}

	// a payload is optional for types that need none
	if _, err := Build("agent.restart", nil); err != nil {
		t.Errorf("no payload: %v", err)
	}
	if _, err := Build("agent.restart", []byte("null")); err != nil {
		t.Errorf("null payload: %v", err)
	}

	for name, tc := range map[string]struct {
		commandType string
		payload     string
		message     string
	}{
		"unknown type":  {"rm.everything", "{}", "unknown command type"},
		"unknown field": {"container.stop", `{"nope":1}`, "invalid payload"},
		"wrong kind":    {"container.stop", `{"docker_id":5}`, "invalid payload"},
		"not an object": {"container.stop", `"text"`, "invalid payload"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Build(tc.commandType, []byte(tc.payload))
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("%v", err)
			}
		})
	}
}

func TestTypeOfAndPayloadRoundTrip(t *testing.T) {
	for commandType := range Specs {
		command, err := Build(commandType, nil)
		if err != nil {
			t.Fatalf("%s: %v", commandType, err)
		}

		if got := TypeOf(command); got != commandType {
			t.Errorf("TypeOf(%s) = %s", commandType, got)
		}

		raw, err := PayloadJSON(command)
		if err != nil {
			t.Fatalf("%s: %v", commandType, err)
		}
		if _, err := Build(commandType, raw); err != nil {
			t.Errorf("%s does not read back its own payload: %v", commandType, err)
		}
	}
}

func TestResources(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command *agentv1.Command
		want    string
	}{
		{"stack", &agentv1.Command{Payload: &agentv1.Command_StackDeploy{StackDeploy: &agentv1.StackDeploy{Name: "a"}}}, "stack:a"},
		// in order with the deploy that the panel queues right after it
		{"database network", &agentv1.Command{Payload: &agentv1.Command_DbNetworkAttach{DbNetworkAttach: &agentv1.DbNetworkAttach{Network: "pcdb-a"}}}, "stack:a"},
		{"database move", &agentv1.Command{Payload: &agentv1.Command_DbServerMove{DbServerMove: &agentv1.DbServerMove{Stack: "a"}}}, "stack:a"},
		{"container", &agentv1.Command{Payload: &agentv1.Command_ContainerStop{ContainerStop: &agentv1.ContainerStop{DockerId: "x"}}}, "container:x"},
		{"site", &agentv1.Command{Payload: &agentv1.Command_SiteRollback{SiteRollback: &agentv1.SiteRollback{Site: "s"}}}, "site:s"},
		{"backup", &agentv1.Command{Payload: &agentv1.Command_BackupRun{BackupRun: &agentv1.BackupRun{Repository: &agentv1.BackupRepository{Url: "s3:b"}}}}, "backup:s3:b"},
		// local targets share the URL /repo; each is its own resource
		{"backup target", &agentv1.Command{Payload: &agentv1.Command_BackupInit{BackupInit: &agentv1.BackupInit{Repository: &agentv1.BackupRepository{Url: "/repo", TargetId: "t1"}}}}, "backup:t1"},
		{"backup usage", &agentv1.Command{Payload: &agentv1.Command_BackupInit{BackupInit: &agentv1.BackupInit{Usage: true, RepoId: "r1", Repository: &agentv1.BackupRepository{Url: "/repo", TargetId: "t1"}}}}, "backup-usage:t1"},
		{"backup usage of a new target", &agentv1.Command{Payload: &agentv1.Command_BackupInit{BackupInit: &agentv1.BackupInit{Usage: true, Repository: &agentv1.BackupRepository{Url: "/repo", TargetId: "t1"}}}}, "backup:t1"},
		// a check waits for another check of its target, and nothing else
		// waits for it (#31)
		{"backup check", &agentv1.Command{Payload: &agentv1.Command_BackupCheck{BackupCheck: &agentv1.BackupCheck{Repository: &agentv1.BackupRepository{Url: "/repo", TargetId: "t1"}, ReadDataSubset: "3/20"}}}, "backup-integrity:t1"},
		{"backup check of a URL", &agentv1.Command{Payload: &agentv1.Command_BackupCheck{BackupCheck: &agentv1.BackupCheck{Repository: &agentv1.BackupRepository{Url: "s3:b"}}}}, "backup-integrity:s3:b"},
		{"backup check of no repository", &agentv1.Command{Payload: &agentv1.Command_BackupCheck{BackupCheck: &agentv1.BackupCheck{}}}, ""},
		{"backup run of the target", &agentv1.Command{Payload: &agentv1.Command_BackupRun{BackupRun: &agentv1.BackupRun{Repository: &agentv1.BackupRepository{Url: "/repo", TargetId: "t1"}}}}, "backup:t1"},
		{"agent", &agentv1.Command{Payload: &agentv1.Command_AgentRestart{AgentRestart: &agentv1.AgentRestart{}}}, "agent"},
		{"edge", &agentv1.Command{Payload: &agentv1.Command_EdgeApply{EdgeApply: &agentv1.EdgeApply{}}}, "edge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := Specs[TypeOf(tc.command)]
			if spec.Resource == nil {
				t.Fatal("this type has no ordering")
			}
			if got := spec.Resource(tc.command); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestRestoreResourceIsTheStackAndDryRunHasNone: a restore holds what it
// writes, the project of its destination's containers above all, so that a
// deploy cannot start them while their data is half restored; a preview
// writes nothing and holds nothing. No restore holds its target, or it would
// wait behind backups of twelve hours (#30).
func TestRestoreResourceIsTheStackAndDryRunHasNone(t *testing.T) {
	repository := &agentv1.BackupRepository{Url: "/repo", TargetId: "t1"}
	restore := func(r *agentv1.BackupRestore) *agentv1.Command {
		r.Repository = repository
		return &agentv1.Command{Payload: &agentv1.Command_BackupRestore{BackupRestore: r}}
	}
	volume := []*agentv1.BackupRestoreRoot{{Source: "/volumes/app_data", Volume: "app_data"}}
	path := []*agentv1.BackupRestoreRoot{{Source: "/srv/app", Path: "/srv/app"}}

	for _, tc := range []struct {
		name    string
		command *agentv1.Command
		want    string
	}{
		{"a preview", restore(&agentv1.BackupRestore{DryRun: true, Stack: "notes", Original: volume}), ""},
		{"a preview into a folder", restore(&agentv1.BackupRestore{DryRun: true, TargetPath: "/srv/restored"}), ""},
		{"a stack's volumes", restore(&agentv1.BackupRestore{Stack: "notes", Original: []*agentv1.BackupRestoreRoot{
			{Source: "/volumes/notes_pgdata", Volume: "notes_pgdata"}, {Source: "/volumes/notes_appdata", Volume: "notes_appdata"},
		}}), "stack:notes"},
		{"a project's volume", restore(&agentv1.BackupRestore{Stack: "notes", Original: volume}), "stack:notes"},
		{"a volume of no project", restore(&agentv1.BackupRestore{Original: volume}), "volume:app_data"},
		{"a directory", restore(&agentv1.BackupRestore{Original: path}), "path:/srv/app"},
		{"a new folder", restore(&agentv1.BackupRestore{TargetPath: "/srv/restored", Source: "/"}), "path:/srv/restored"},
		{"a new folder used by a project", restore(&agentv1.BackupRestore{TargetPath: "/srv/restored", Stack: "notes"}), "stack:notes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := Specs[TypeOf(tc.command)]
			if spec.Resource == nil {
				t.Fatal("a restore has no ordering")
			}
			if got := spec.Resource(tc.command); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}

	// what the restore's commands do not write, they do not hold
	for _, commandType := range []string{"backup.snapshots", "backup.browse", "backup.dump"} {
		if Specs[commandType].Resource != nil {
			t.Errorf("%s has an ordering key", commandType)
		}
	}
}

// resourceOf is a command's ordering key, or says that its type orders
// nothing.
func resourceOf(command *agentv1.Command) string {
	spec, ok := Specs[TypeOf(command)]
	if !ok || spec.Resource == nil {
		return "<no ordering key>"
	}
	return spec.Resource(command)
}

// TestADatabaseRestoreHoldsItsApplication: a restore of a database (#53)
// holds the owner stack, as a restore to the original location does, or the
// site whose site.* commands would start what it stopped; a restore into a
// new database holds that database on its server. A preview holds nothing.
func TestADatabaseRestoreHoldsItsApplication(t *testing.T) {
	connection := &agentv1.DbConnection{ContainerId: "pcdb-main-db-1", Engine: "postgres", AdminUser: "postgres"}
	restore := func(r *agentv1.BackupRestore) *agentv1.Command {
		r.Repository = &agentv1.BackupRepository{Url: "/repo", TargetId: "t1"}
		r.RestoreId, r.JobTag, r.SnapshotId = "r1", "job:j1", strings.Repeat("3f", 32)
		return &agentv1.Command{Payload: &agentv1.Command_BackupRestore{BackupRestore: r}}
	}
	database := func(mode, name string) *agentv1.BackupDbRestore {
		return &agentv1.BackupDbRestore{Connection: connection, Database: name, File: "shop.pgdump", Mode: mode, User: name,
			Scratch: "pc_rs_aaaaaaaaaaaaaaa", Previous: "pc_old_aaaaaaaaaaaaaaa"}
	}
	site := database("replace", "wp")
	site.Site, site.StopContainers = "wp", []string{"wp-php", "wp-worker"}

	for _, tc := range []struct {
		name    string
		command *agentv1.Command
		want    string
	}{
		{"a stack's database", restore(&agentv1.BackupRestore{Stack: "shop", Overwrite: true, Database: database("replace", "shop")}), "stack:shop"},
		{"a site's database", restore(&agentv1.BackupRestore{Overwrite: true, Database: site}), "site:wp"},
		{"a site's database that a stack owns", restore(&agentv1.BackupRestore{Stack: "wp-stack", Overwrite: true, Database: site}), "stack:wp-stack"},
		{"a database of no application", restore(&agentv1.BackupRestore{Overwrite: true, Database: database("replace", "shop")}), "db:pcdb-main-db-1/shop"},
		{"a new database", restore(&agentv1.BackupRestore{Database: database("new", "shop_restored")}), "db:pcdb-main-db-1/shop_restored"},
		{"a preview", restore(&agentv1.BackupRestore{DryRun: true, Stack: "shop", Database: database("replace", "shop")}), ""},
		{"a preview of a new database", restore(&agentv1.BackupRestore{DryRun: true, Database: database("new", "shop_restored")}), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resourceOf(tc.command); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRestoreTestsAndDiscardsOfAServerRunOneByOne: a server restores one
// test copy at a time, and none of its temporary databases is dropped while
// a test of it runs (#53); another server's tests do not wait.
func TestRestoreTestsAndDiscardsOfAServerRunOneByOne(t *testing.T) {
	test := func(container string) *agentv1.Command {
		return &agentv1.Command{Payload: &agentv1.Command_BackupRestoreTest{BackupRestoreTest: &agentv1.BackupRestoreTest{
			Repository: &agentv1.BackupRepository{Url: "/repo", TargetId: "t1"}, SnapshotId: strings.Repeat("3f", 32), JobTag: "job:j1",
			CheckId: "aaaaaaaaaaaaaaa", Database: &agentv1.BackupDbRestore{
				Connection: &agentv1.DbConnection{ContainerId: container, Engine: "mariadb", AdminUser: "root"},
				Database:   "pc_rt_aaaaaaaaaaaaaaa", File: "shop.sql", Mode: "test", User: "shop",
			},
			DropBefore: []string{"pc_rt_bbbbbbbbbbbbbbb"},
		}}}
	}
	discard := func(container string) *agentv1.Command {
		return &agentv1.Command{Payload: &agentv1.Command_DbDatabaseDiscard{DbDatabaseDiscard: &agentv1.DbDatabaseDiscard{
			Connection: &agentv1.DbConnection{ContainerId: container, Engine: "mariadb", AdminUser: "root"},
			Names:      []string{"pc_rt_bbbbbbbbbbbbbbb"}, RevokeFrom: "shop",
		}}}
	}
	for _, tc := range []struct {
		name    string
		command *agentv1.Command
		want    string
	}{
		{"a test", test("pcdb-maria-db-1"), "db-test:pcdb-maria-db-1"},
		{"a discard", discard("pcdb-maria-db-1"), "db-test:pcdb-maria-db-1"},
		{"a test of another server", test("pcdb-pg-db-1"), "db-test:pcdb-pg-db-1"},
		{"a test of no server", test(""), ""},
		{"a discard of no server", discard(""), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resourceOf(tc.command); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	for commandType, timeout := range map[string]time.Duration{"backup.restore_test": 6 * time.Hour, "db.database.discard": 10 * time.Minute} {
		if spec := Specs[commandType]; spec.Timeout != timeout || spec.SettledByAgent || spec.Uninterruptible {
			t.Errorf("%s: %+v", commandType, spec)
		}
	}
}

// TestTheDatabaseCommandsKeepTheirFieldNumbers pins the wire numbers of #53's
// command types, which agents of either version read.
func TestTheDatabaseCommandsKeepTheirFieldNumbers(t *testing.T) {
	fields := (&agentv1.Command{}).ProtoReflect().Descriptor().Oneofs().ByName("payload").Fields()
	for name, number := range map[protoreflect.Name]protoreflect.FieldNumber{"db_database_discard": 183, "backup_restore_test": 229} {
		field := fields.ByName(name)
		if field == nil || field.Number() != number {
			t.Errorf("%s is %v, not field %d", name, field, number)
		}
	}
}

// TestOnlyRestoresAndDownloadsAreSettledByTheAgent: a cancelled restore waits
// for the agent to say which containers it started again, and a download for
// the agent to stop sending (#30). Every other type is finished by a cancel at
// once, as before.
func TestOnlyRestoresAndDownloadsAreSettledByTheAgent(t *testing.T) {
	for commandType, spec := range Specs {
		want := commandType == "backup.restore" || commandType == "backup.dump"
		if spec.SettledByAgent != want {
			t.Errorf("%s: SettledByAgent is %v", commandType, spec.SettledByAgent)
		}
		if spec.SettledByAgent && spec.Uninterruptible {
			t.Errorf("%s cannot be stopped, and waits for the agent after a stop", commandType)
		}
	}
	for commandType, spec := range PanelSpecs {
		if spec.SettledByAgent {
			t.Errorf("the panel runs %s itself: no agent settles it", commandType)
		}
	}
}

// TestSecretCarryingTypesAreNotOpenToTheAPI: a type whose payload can hold a secret
// may only be created by the panel, which reads the secret right before delivery.
func TestSecretCarryingTypesAreNotOpenToTheAPI(t *testing.T) {
	for _, commandType := range []string{
		"stack.deploy", "cert.install", "db.server.create", "db.database.create", "db.database.rotate",
		"backup.run", "backup.init", "backup.restore", "backup.dump", "agent.update", "exec.start", "edge.apply",
		// the database's passwords and the target's secrets come at delivery (#53)
		"backup.restore_test", "db.database.discard",
	} {
		if role := Specs[commandType].Role; role != "" {
			t.Errorf("%s can be requested through the API by %s", commandType, role)
		}
	}
}

func TestTransitions(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		allowed  bool
	}{
		{schema.CommandQueued, schema.CommandDispatched, true},
		{schema.CommandQueued, schema.CommandCancelled, true},
		{schema.CommandQueued, schema.CommandTimedOut, true},
		{schema.CommandQueued, schema.CommandSucceeded, false},
		{schema.CommandQueued, schema.CommandRunning, false},
		{schema.CommandDispatched, schema.CommandRunning, true},
		{schema.CommandDispatched, schema.CommandSucceeded, true},
		{schema.CommandDispatched, schema.CommandQueued, false},
		{schema.CommandRunning, schema.CommandRunning, true},
		{schema.CommandRunning, schema.CommandFailed, true},
		{schema.CommandRunning, schema.CommandDispatched, false},
		{schema.CommandSucceeded, schema.CommandSucceeded, false},
		{schema.CommandSucceeded, schema.CommandQueued, false},
		{schema.CommandFailed, schema.CommandRunning, false},
		{schema.CommandTimedOut, schema.CommandSucceeded, false},
		{schema.CommandCancelled, schema.CommandQueued, false},
	} {
		if got := transitionAllowed(tc.from, tc.to); got != tc.allowed {
			t.Errorf("%s → %s: got %v", tc.from, tc.to, got)
		}
	}
}

// TestADroppedStreamSendsNothingMore: commands already buffered for a host that
// was removed must not reach its agent; Go's select would otherwise pick the
// buffered message as often as the closed stream.
func TestADroppedStreamSendsNothingMore(t *testing.T) {
	reason := errors.New("the server was removed")

	for range 200 {
		m := &Manager{streams: map[string]*Stream{}}
		stream := &Stream{hostID: "h", out: make(chan *agentv1.SubscribeCommandsResponse, 3), closed: make(chan struct{})}
		m.streams["h"] = stream
		for range 3 {
			stream.out <- &agentv1.SubscribeCommandsResponse{}
		}

		m.Drop("h", reason)

		message, err := stream.Next(context.Background(), nil)
		if message != nil || !errors.Is(err, reason) {
			t.Fatalf("a dropped stream gave %v, %v", message, err)
		}
	}
}
