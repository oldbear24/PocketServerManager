package commands

import (
	"strings"
	"testing"

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
		{"container", &agentv1.Command{Payload: &agentv1.Command_ContainerStop{ContainerStop: &agentv1.ContainerStop{DockerId: "x"}}}, "container:x"},
		{"site", &agentv1.Command{Payload: &agentv1.Command_SiteRollback{SiteRollback: &agentv1.SiteRollback{Site: "s"}}}, "site:s"},
		{"backup", &agentv1.Command{Payload: &agentv1.Command_BackupRun{BackupRun: &agentv1.BackupRun{Repository: &agentv1.BackupRepository{Url: "s3:b"}}}}, "backup:s3:b"},
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

// TestSecretCarryingTypesAreNotOpenToTheAPI: a type whose payload can hold a secret
// may only be created by the panel, which reads the secret right before delivery.
func TestSecretCarryingTypesAreNotOpenToTheAPI(t *testing.T) {
	for _, commandType := range []string{
		"stack.deploy", "cert.install", "db.server.create", "db.database.create", "db.database.rotate",
		"backup.run", "backup.init", "backup.restore", "agent.update", "exec.start", "edge.apply",
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
