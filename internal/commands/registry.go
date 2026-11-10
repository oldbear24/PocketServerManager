// Package commands implements the command queue. Every action becomes a record in
// the commands collection, which is the command history and the delivery queue at
// once. See "Command lifecycle" in docs/architecture.md.
package commands

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/schema"
)

const defaultTimeout = 5 * time.Minute

// Spec describes one command type.
type Spec struct {
	// Timeout is how long the panel waits for the agent to finish.
	Timeout time.Duration
	// Role is the minimum role needed to request this type through the API.
	// Empty means the panel's own components create it, never the REST API.
	Role string
	// Resource returns the ordering key: commands with the same key run one after
	// another, the others in parallel.
	Resource func(*agentv1.Command) string
	// Uninterruptible commands cannot be cancelled once they left the queue: only
	// the agent knows whether a cancel would land where it can still undo its
	// work, so it decides by itself, within the command's timeout.
	Uninterruptible bool
	// SettledByAgent commands stay open when they are cancelled after they
	// left the queue, until the agent reports how they ended: the agent still
	// has work to do after a cancel, such as starting again the containers a
	// restore stopped, and its report says what it did (#30). Its ordering
	// key is held until then. A queued one is cancelled at once, as any other.
	SettledByAgent bool
}

func stack(c *agentv1.Command) string {
	switch p := c.GetPayload().(type) {
	case *agentv1.Command_StackDeploy:
		return "stack:" + p.StackDeploy.GetName()
	case *agentv1.Command_StackStop:
		return "stack:" + p.StackStop.GetName()
	case *agentv1.Command_StackDown:
		return "stack:" + p.StackDown.GetName()
	case *agentv1.Command_StackPull:
		return "stack:" + p.StackPull.GetName()
	case *agentv1.Command_StackRestart:
		return "stack:" + p.StackRestart.GetName()
	case *agentv1.Command_StackImport:
		return "stack:" + p.StackImport.GetProject()
	}
	return ""
}

// databaseNetwork orders an application's attach to its database network with
// its stack's own commands. The network is named after the stack
// (databases.NetworkName), and the panel queues a deploy right after the attach:
// run side by side, both would put the server on the network at the same moment,
// and the engine refuses the second.
func databaseNetwork(c *agentv1.Command) string {
	if name, ok := strings.CutPrefix(c.GetDbNetworkAttach().GetNetwork(), "pcdb-"); ok && name != "" {
		return "stack:" + name
	}
	return ""
}

// databaseMove orders a move with the deploys of the stack whose databases move:
// the panel deploys the stack again right after, and none may run in between.
func databaseMove(c *agentv1.Command) string {
	if name := c.GetDbServerMove().GetStack(); name != "" {
		return "stack:" + name
	}
	return ""
}

func container(c *agentv1.Command) string {
	switch p := c.GetPayload().(type) {
	case *agentv1.Command_ContainerStart:
		return "container:" + p.ContainerStart.GetDockerId()
	case *agentv1.Command_ContainerStop:
		return "container:" + p.ContainerStop.GetDockerId()
	case *agentv1.Command_ContainerRestart:
		return "container:" + p.ContainerRestart.GetDockerId()
	case *agentv1.Command_ContainerRemove:
		return "container:" + p.ContainerRemove.GetDockerId()
	}
	return ""
}

func site(c *agentv1.Command) string {
	switch p := c.GetPayload().(type) {
	case *agentv1.Command_SiteDeploy:
		return "site:" + p.SiteDeploy.GetSite()
	case *agentv1.Command_SiteRollback:
		return "site:" + p.SiteRollback.GetSite()
	case *agentv1.Command_SiteRemove:
		return "site:" + p.SiteRemove.GetSite()
	case *agentv1.Command_SitePhpApply:
		return "site:" + p.SitePhpApply.GetSite()
	}
	return ""
}

func backup(c *agentv1.Command) string {
	var repository *agentv1.BackupRepository
	switch p := c.GetPayload().(type) {
	case *agentv1.Command_BackupInit:
		repository = p.BackupInit.GetRepository()
	case *agentv1.Command_BackupRun:
		repository = p.BackupRun.GetRepository()
	case *agentv1.Command_BackupCheck:
		repository = p.BackupCheck.GetRepository()
	case *agentv1.Command_BackupUnlock:
		repository = p.BackupUnlock.GetRepository()
	case *agentv1.Command_BackupForget:
		repository = p.BackupForget.GetRepository()
	}
	if repository == nil {
		return ""
	}
	// a target's commands run one after another; the URL of a local one is
	// the same /repo in each job, whatever the directory. Measuring what a
	// target's repository reads without a lock and may take long: it waits
	// only for another measure, never holds up a check someone waits for. One
	// that may create the repository still runs in turn with the others.
	if id := repository.GetTargetId(); id != "" {
		if init := c.GetBackupInit(); init.GetUsage() && init.GetRepoId() != "" {
			return "backup-usage:" + id
		}
		return "backup:" + id
	}
	return "backup:" + repository.GetUrl()
}

// restoreResource orders a restore with what else writes its destination
// (#30). A restore to the original location of a Compose project holds the
// project, as a deploy does: deploys, stack commands and moves of its
// databases wait for it. Any other restore holds the volume or the folder it
// writes. A preview writes nothing, and waits for nothing. It is never the
// target's key: a restore would wait behind backups of twelve hours, and a
// backup would wait behind it.
func restoreResource(c *agentv1.Command) string {
	restore := c.GetBackupRestore()
	switch {
	case restore == nil || restore.GetDryRun():
		return ""
	case restore.GetStack() != "":
		return "stack:" + restore.GetStack()
	case len(restore.GetOriginal()) > 0:
		root := restore.GetOriginal()[0]
		if root.GetVolume() != "" {
			return "volume:" + root.GetVolume()
		}
		if root.GetPath() != "" {
			return "path:" + root.GetPath()
		}
	case restore.GetTargetPath() != "":
		return "path:" + restore.GetTargetPath()
	}
	return ""
}

func self(*agentv1.Command) string {
	return "agent"
}

func edge(*agentv1.Command) string {
	return "edge"
}

// Specs lists every command type. A test keeps it in sync with the oneof in
// proto/pocketcloud/agent/v1/commands.proto.
var Specs = map[string]Spec{
	"container.start":   {Timeout: time.Minute, Role: schema.RoleOperator, Resource: container},
	"container.stop":    {Timeout: 2 * time.Minute, Role: schema.RoleOperator, Resource: container},
	"container.restart": {Timeout: 2 * time.Minute, Role: schema.RoleOperator, Resource: container},
	"container.remove":  {Timeout: 2 * time.Minute, Role: schema.RoleOperator, Resource: container},

	"image.pull": {Timeout: 15 * time.Minute, Role: schema.RoleOperator},

	"stack.deploy":   {Timeout: 30 * time.Minute, Resource: stack},
	"stack.stop":     {Timeout: 5 * time.Minute, Role: schema.RoleOperator, Resource: stack},
	"stack.down":     {Timeout: 5 * time.Minute, Role: schema.RoleOperator, Resource: stack},
	"stack.pull":     {Timeout: 15 * time.Minute, Role: schema.RoleOperator, Resource: stack},
	"stack.restart":  {Timeout: 5 * time.Minute, Role: schema.RoleOperator, Resource: stack},
	"stack.validate": {Timeout: time.Minute},
	"stack.import":   {Timeout: 2 * time.Minute, Resource: stack},

	"logs.follow": {Timeout: time.Minute},
	"logs.stop":   {Timeout: time.Minute},

	"exec.start": {Timeout: time.Minute},
	"exec.stop":  {Timeout: time.Minute},

	"site.deploy":    {Timeout: 30 * time.Minute, Resource: site},
	"site.rollback":  {Timeout: time.Minute, Resource: site},
	"site.remove":    {Timeout: 5 * time.Minute, Resource: site},
	"site.php.apply": {Timeout: 5 * time.Minute, Resource: site},
	// a read, which waits for no deployment
	"site.php.status": {Timeout: 30 * time.Second},
	"site.php.mail":   {Timeout: 30 * time.Second},
	// a look at a repository for the new-site wizard (#52)
	"site.detect": {Timeout: 2 * time.Minute},

	"task.run": {Timeout: time.Hour},

	"db.server.create":   {Timeout: 15 * time.Minute},
	"db.server.upgrade":  {Timeout: 30 * time.Minute},
	"db.server.remove":   {Timeout: 5 * time.Minute},
	"db.database.create": {Timeout: 2 * time.Minute},
	"db.database.drop":   {Timeout: time.Hour},
	"db.database.rotate": {Timeout: 2 * time.Minute},
	"db.database.clone":  {Timeout: 6 * time.Hour},
	"db.database.export": {Timeout: 6 * time.Hour},
	"db.network.attach":  {Timeout: 2 * time.Minute, Resource: databaseNetwork},
	"db.server.move":     {Timeout: 6 * time.Hour, Resource: databaseMove, Uninterruptible: true},
	"db.stats":           {Timeout: 2 * time.Minute},
	"db.adminer.start":   {Timeout: 5 * time.Minute},
	"db.adminer.stop":    {Timeout: 2 * time.Minute},

	"edge.apply": {Timeout: 5 * time.Minute, Resource: edge},

	"cert.install": {Timeout: 2 * time.Minute, Resource: edge},
	"cert.remove":  {Timeout: 2 * time.Minute, Resource: edge},

	// changes of one DNS server of a LAN, one after another
	"dns.rewrite": {Timeout: time.Minute, Resource: func(c *agentv1.Command) string { return "dns-rewrite:" + c.GetDnsRewrite().GetProviderId() }},

	"acme.challenge.present": {Timeout: time.Minute},
	"acme.challenge.cleanup": {Timeout: time.Minute},

	"backup.init": {Timeout: time.Hour, Resource: backup},
	"backup.run":  {Timeout: 12 * time.Hour, Resource: backup},
	// a preview, or a restore that stops and starts the destination's users
	// itself (#30)
	"backup.restore":   {Timeout: 12 * time.Hour, Resource: restoreResource, SettledByAgent: true},
	"backup.snapshots": {Timeout: 5 * time.Minute},
	"backup.browse":    {Timeout: 5 * time.Minute},
	"backup.check":     {Timeout: 12 * time.Hour, Resource: backup},
	"backup.unlock":    {Timeout: 5 * time.Minute, Resource: backup},
	"backup.forget":    {Timeout: 2 * time.Hour, Resource: backup},
	// a download streamed from the repository to a browser (#30)
	"backup.dump": {Timeout: 12 * time.Hour, SettledByAgent: true},

	"agent.update":             {Timeout: 15 * time.Minute, Resource: self},
	"agent.restart":            {Timeout: 2 * time.Minute, Role: schema.RoleAdmin, Resource: self},
	"agent.rotate_certificate": {Timeout: 2 * time.Minute, Role: schema.RoleAdmin, Resource: self},
	"agent.diagnostics":        {Timeout: 10 * time.Minute, Role: schema.RoleAdmin},
	"agent.set_log_level":      {Timeout: time.Minute, Role: schema.RoleAdmin},
}

// PanelSpecs lists the command types the panel runs itself, such as the steps
// of a new site's provisioning (#24): each step is a command, so the command
// history shows it, but none has an agent payload. They start as running, are
// never sent to an agent, and the panel settles them with Manager.Settle. A
// parent of this kind is settled by the flow that runs it, not by its
// children. None can be requested through the API.
var PanelSpecs = map[string]Spec{
	"site.provision":   {Timeout: 7 * 24 * time.Hour},
	"site.create":      {Timeout: time.Minute},
	"site.dns":         {Timeout: 10 * time.Minute},
	"site.propagation": {Timeout: 48 * time.Hour},
	"site.certificate": {Timeout: 2 * time.Hour},
	"site.check":       {Timeout: 10 * time.Minute},
}

// PanelRun tells whether the panel runs a command type itself.
func PanelRun(commandType string) bool {
	_, ok := PanelSpecs[commandType]
	return ok
}

// fieldName returns the oneof field of a command type: dots become underscores.
func fieldName(commandType string) protoreflect.Name {
	return protoreflect.Name(strings.ReplaceAll(commandType, ".", "_"))
}

// Build creates the protobuf command for a type and its JSON payload. An unknown
// type, or a payload that does not fit the type, is an error.
func Build(commandType string, payload []byte) (*agentv1.Command, error) {
	if _, ok := Specs[commandType]; !ok {
		return nil, fmt.Errorf("unknown command type %q", commandType)
	}

	command := &agentv1.Command{Type: commandType}

	reflected := command.ProtoReflect()
	field := reflected.Descriptor().Fields().ByName(fieldName(commandType))
	if field == nil || field.ContainingOneof() == nil {
		return nil, fmt.Errorf("command type %q has no payload message", commandType)
	}

	message := reflected.Mutable(field).Message().Interface()

	if len(payload) > 0 && string(payload) != "null" {
		if err := protojson.Unmarshal(payload, message); err != nil {
			return nil, fmt.Errorf("invalid payload of %s: %w", commandType, err)
		}
	}

	return command, nil
}

// TypeOf returns the command type of a payload message; see Specs.
func TypeOf(command *agentv1.Command) string {
	reflected := command.ProtoReflect()
	oneof := reflected.Descriptor().Oneofs().ByName("payload")
	field := reflected.WhichOneof(oneof)
	if field == nil {
		return ""
	}

	name := string(field.Name())
	for commandType := range Specs {
		if string(fieldName(commandType)) == name {
			return commandType
		}
	}

	return ""
}

// PayloadJSON returns the JSON form of a command's payload, for the record.
func PayloadJSON(command *agentv1.Command) (json.RawMessage, error) {
	reflected := command.ProtoReflect()
	oneof := reflected.Descriptor().Oneofs().ByName("payload")
	field := reflected.WhichOneof(oneof)
	if field == nil {
		return nil, nil
	}

	return protojson.MarshalOptions{UseProtoNames: true}.Marshal(reflected.Get(field).Message().Interface())
}
