// Package migrations holds the Go migrations of the panel's collections.
// docs/data-model.md describes the fields and is updated together with them.
package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

const (
	maxLongText = 1 << 20   // compose files and the like
	maxSealed   = 1 << 21   // sealed secrets
	maxOutput   = 1 << 26   // command output files
	maxBundle   = 1<<28 + 0 // diagnostic bundles
)

func init() {
	m.Register(up, down)
}

func up(app core.App) error {
	if err := updateUsers(app); err != nil {
		return err
	}

	hosts := newCollection(schema.Hosts, access{list: schema.RuleViewer, view: schema.RuleViewer, update: schema.RuleAdmin, del: schema.RuleAdmin})
	hosts.Fields.Add(
		&core.TextField{Name: "name", Required: true, Max: 64, Pattern: `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`},
		&core.SelectField{Name: "roles", MaxSelect: 3, Values: []string{schema.HostRoleDocker, schema.HostRoleEdge, schema.HostRoleBackup}},
		&core.SelectField{Name: "status", MaxSelect: 1, Values: []string{schema.HostOnline, schema.HostDegraded, schema.HostOffline}},
		&core.TextField{Name: "hostname", Max: 255},
		&core.TextField{Name: "os", Max: 255},
		&core.TextField{Name: "kernel", Max: 255},
		&core.TextField{Name: "arch", Max: 32},
		&core.NumberField{Name: "cpu_cores", OnlyInt: true},
		&core.NumberField{Name: "mem_bytes", OnlyInt: true},
		&core.TextField{Name: "ip4", Max: 64},
		&core.TextField{Name: "ip6", Max: 64},
		&core.JSONField{Name: "labels"},
		&core.TextField{Name: "agent_version", Max: 64},
		&core.TextField{Name: "agent_image_digest", Max: 128},
		&core.TextField{Name: "agent_container_id", Max: 128},
		&core.JSONField{Name: "capabilities"},
		&core.TextField{Name: "docker_version", Max: 64},
		&core.DateField{Name: "last_seen"},
		&core.NumberField{Name: "rtt_ms"},
		&core.NumberField{Name: "clock_skew_ms"},
		&core.DateField{Name: "connected_since"},
		&core.BoolField{Name: "stream_open"},
		&core.NumberField{Name: "uptime_s", OnlyInt: true},
		&core.NumberField{Name: "load1"},
		&core.NumberField{Name: "cpu_percent"},
		&core.NumberField{Name: "mem_used_bytes", OnlyInt: true},
		&core.NumberField{Name: "disk_used_bytes", OnlyInt: true},
		&core.NumberField{Name: "disk_total_bytes", OnlyInt: true},
		&core.JSONField{Name: "runtime"},
		&core.JSONField{Name: "self_checks"},
		&core.DateField{Name: "inventory_at"},
		&core.TextField{Name: "cert_serial", Max: 128},
		&core.TextField{Name: "cert_prev_serial", Max: 128},
		&core.DateField{Name: "cert_expires_at"},
		&core.BoolField{Name: "host_shell_enabled"},
	)
	hosts.AddIndex("idx_hosts_name", true, "name", "")
	if err := app.Save(hosts); err != nil {
		return err
	}

	revoked := newCollection(schema.RevokedCerts, access{list: schema.RuleAdmin, view: schema.RuleAdmin})
	revoked.Fields.Add(
		&core.TextField{Name: "serial", Required: true, Max: 128},
		&core.TextField{Name: "host", Max: 32},
		&core.TextField{Name: "host_name", Max: 64},
		&core.TextField{Name: "reason", Max: 255},
	)
	revoked.AddIndex("idx_revoked_certs_serial", true, "serial", "")
	if err := app.Save(revoked); err != nil {
		return err
	}

	users, err := app.FindCollectionByNameOrId(schema.Users)
	if err != nil {
		return err
	}

	tokens := newCollection(schema.EnrollmentTokens, access{list: schema.RuleAdmin, view: schema.RuleAdmin, del: schema.RuleAdmin})
	tokens.Fields.Add(
		&core.TextField{Name: "token_hash", Required: true, Max: 128, Hidden: true},
		&core.TextField{Name: "hint", Max: 16},
		&core.TextField{Name: "name", Required: true, Max: 64},
		&core.SelectField{Name: "roles", MaxSelect: 3, Values: []string{schema.HostRoleDocker, schema.HostRoleEdge, schema.HostRoleBackup}},
		&core.DateField{Name: "expires_at", Required: true},
		&core.DateField{Name: "used_at"},
		&core.RelationField{Name: "created_by", CollectionId: users.Id, MaxSelect: 1},
		&core.RelationField{Name: "host", CollectionId: hosts.Id, MaxSelect: 1},
	)
	tokens.AddIndex("idx_enrollment_tokens_hash", true, "token_hash", "")
	if err := app.Save(tokens); err != nil {
		return err
	}

	commands := newCollection(schema.Commands, access{list: schema.RuleViewer, view: schema.RuleViewer, create: schema.RuleOperator})
	commands.Fields.Add(
		&core.RelationField{Name: "host", CollectionId: hosts.Id, MaxSelect: 1},
		&core.TextField{Name: "host_name", Max: 64},
		&core.TextField{Name: "type", Required: true, Max: 64},
		&core.TextField{Name: "label", Max: 255},
		&core.JSONField{Name: "payload"},
		&core.SelectField{Name: "status", MaxSelect: 1, Values: []string{
			schema.CommandQueued, schema.CommandDispatched, schema.CommandRunning,
			schema.CommandSucceeded, schema.CommandFailed, schema.CommandTimedOut, schema.CommandCancelled,
		}},
		&core.RelationField{Name: "requested_by", CollectionId: users.Id, MaxSelect: 1},
		&core.TextField{Name: "requested_label", Max: 255},
		&core.TextField{Name: "resource", Max: 255},
		&core.TextField{Name: "correlation_id", Max: 64},
		&core.NumberField{Name: "attempts", OnlyInt: true},
		&core.NumberField{Name: "last_sequence", OnlyInt: true},
		&core.NumberField{Name: "timeout_s", OnlyInt: true},
		&core.DateField{Name: "dispatched_at"},
		&core.DateField{Name: "started_at"},
		&core.DateField{Name: "finished_at"},
		&core.DateField{Name: "expires_at"},
		&core.JSONField{Name: "progress"},
		&core.JSONField{Name: "result"},
		&core.TextField{Name: "error", Max: maxLongText},
		&core.FileField{Name: "output", MaxSelect: 1, MaxSize: maxOutput, Protected: true},
	)
	commands.AddIndex("idx_commands_host_status", false, "host, status", "")
	commands.AddIndex("idx_commands_status", false, "status", "")
	commands.AddIndex("idx_commands_correlation", false, "correlation_id", "")
	commands.AddIndex("idx_commands_created", false, "created", "")
	if err := app.Save(commands); err != nil {
		return err
	}

	// the parent relation needs the collection's own id
	commands.Fields.Add(&core.RelationField{Name: "parent", CollectionId: commands.Id, MaxSelect: 1})
	if err := app.Save(commands); err != nil {
		return err
	}

	events := newCollection(schema.Events, access{list: schema.RuleViewer, view: schema.RuleViewer})
	events.Fields.Add(
		&core.RelationField{Name: "host", CollectionId: hosts.Id, MaxSelect: 1, CascadeDelete: true},
		&core.TextField{Name: "kind", Required: true, Max: 64},
		&core.SelectField{Name: "severity", MaxSelect: 1, Values: []string{schema.SeverityInfo, schema.SeverityWarning, schema.SeverityCritical}},
		&core.TextField{Name: "message", Max: 2048},
		&core.JSONField{Name: "data"},
	)
	events.AddIndex("idx_events_host_created", false, "host, created", "")
	events.AddIndex("idx_events_created", false, "created", "")
	if err := app.Save(events); err != nil {
		return err
	}

	alerts := newCollection(schema.Alerts, access{list: schema.RuleViewer, view: schema.RuleViewer})
	alerts.Fields.Add(
		&core.TextField{Name: "rule", Required: true, Max: 64},
		&core.TextField{Name: "target", Required: true, Max: 255},
		&core.RelationField{Name: "host", CollectionId: hosts.Id, MaxSelect: 1, CascadeDelete: true},
		&core.SelectField{Name: "severity", MaxSelect: 1, Values: []string{schema.SeverityInfo, schema.SeverityWarning, schema.SeverityCritical}},
		&core.SelectField{Name: "status", MaxSelect: 1, Values: []string{schema.AlertOpen, schema.AlertAcknowledged, schema.AlertResolved}},
		&core.DateField{Name: "first_seen"},
		&core.DateField{Name: "last_seen"},
		&core.DateField{Name: "resolved_at"},
		&core.TextField{Name: "message", Max: 2048},
		&core.RelationField{Name: "acknowledged_by", CollectionId: users.Id, MaxSelect: 1},
	)
	alerts.AddIndex("idx_alerts_rule_target_status", false, "rule, target, status", "")
	if err := app.Save(alerts); err != nil {
		return err
	}

	stacks := newCollection(schema.Stacks, access{
		list: schema.RuleViewer, view: schema.RuleViewer,
		create: schema.RuleOperator, update: schema.RuleOperator, del: schema.RuleOperator,
	})
	stacks.Fields.Add(
		&core.RelationField{Name: "host", Required: true, CollectionId: hosts.Id, MaxSelect: 1, CascadeDelete: true},
		&core.TextField{Name: "name", Required: true, Max: 64, Pattern: `^[a-z0-9][a-z0-9_-]*$`},
		&core.TextField{Name: "compose_yaml", Max: maxLongText},
		&core.TextField{Name: "env", Max: maxLongText},
		&core.TextField{Name: "secrets", Max: maxSealed, Hidden: true},
		&core.JSONField{Name: "secret_keys"},
		&core.NumberField{Name: "current_revision", OnlyInt: true},
		&core.TextField{Name: "status", Max: 32},
	)
	stacks.AddIndex("idx_stacks_host_name", true, "host, name", "")
	if err := app.Save(stacks); err != nil {
		return err
	}

	revisions := newCollection(schema.StackRevisions, access{list: schema.RuleViewer, view: schema.RuleViewer})
	revisions.Fields.Add(
		&core.RelationField{Name: "stack", Required: true, CollectionId: stacks.Id, MaxSelect: 1, CascadeDelete: true},
		&core.NumberField{Name: "number", OnlyInt: true},
		&core.TextField{Name: "compose_yaml", Max: maxLongText},
		&core.TextField{Name: "env", Max: maxLongText},
		&core.JSONField{Name: "env_keys"},
		&core.RelationField{Name: "author", CollectionId: users.Id, MaxSelect: 1},
		&core.DateField{Name: "deployed_at"},
		&core.TextField{Name: "note", Max: 1024},
	)
	revisions.AddIndex("idx_stack_revisions_number", true, "stack, number", "")
	if err := app.Save(revisions); err != nil {
		return err
	}

	containers := newCollection(schema.Containers, access{list: schema.RuleViewer, view: schema.RuleViewer})
	containers.Fields.Add(
		&core.RelationField{Name: "host", Required: true, CollectionId: hosts.Id, MaxSelect: 1, CascadeDelete: true},
		&core.RelationField{Name: "stack", CollectionId: stacks.Id, MaxSelect: 1},
		&core.TextField{Name: "project", Max: 128},
		&core.TextField{Name: "service", Max: 128},
		&core.TextField{Name: "docker_id", Required: true, Max: 128},
		&core.TextField{Name: "name", Max: 255},
		&core.TextField{Name: "image", Max: 512},
		&core.TextField{Name: "state", Max: 32},
		&core.TextField{Name: "status_text", Max: 255},
		&core.TextField{Name: "health", Max: 32},
		&core.TextField{Name: "system_role", Max: 32},
		&core.NumberField{Name: "restart_count", OnlyInt: true},
		&core.JSONField{Name: "ports"},
		&core.JSONField{Name: "labels"},
		&core.DateField{Name: "started_at"},
	)
	containers.AddIndex("idx_containers_host_docker", true, "host, docker_id", "")
	containers.AddIndex("idx_containers_stack", false, "stack", "")
	if err := app.Save(containers); err != nil {
		return err
	}

	volumes := newCollection(schema.Volumes, access{list: schema.RuleViewer, view: schema.RuleViewer})
	volumes.Fields.Add(
		&core.RelationField{Name: "host", Required: true, CollectionId: hosts.Id, MaxSelect: 1, CascadeDelete: true},
		&core.TextField{Name: "name", Required: true, Max: 255},
		&core.TextField{Name: "driver", Max: 64},
		&core.NumberField{Name: "size_bytes", OnlyInt: true},
		&core.JSONField{Name: "labels"},
	)
	volumes.AddIndex("idx_volumes_host_name", true, "host, name", "")
	if err := app.Save(volumes); err != nil {
		return err
	}

	networks := newCollection(schema.Networks, access{list: schema.RuleViewer, view: schema.RuleViewer})
	networks.Fields.Add(
		&core.RelationField{Name: "host", Required: true, CollectionId: hosts.Id, MaxSelect: 1, CascadeDelete: true},
		&core.TextField{Name: "name", Required: true, Max: 255},
		&core.TextField{Name: "docker_id", Max: 128},
		&core.TextField{Name: "driver", Max: 64},
		&core.JSONField{Name: "labels"},
	)
	networks.AddIndex("idx_networks_host_name", true, "host, name", "")
	if err := app.Save(networks); err != nil {
		return err
	}

	// the internal CA has no API rules at all: only the panel process touches it
	internalCA := newCollection(schema.InternalCA, access{})
	internalCA.System = true
	internalCA.Fields.Add(
		&core.TextField{Name: "root_cert", Max: maxLongText},
		&core.TextField{Name: "root_key", Max: maxSealed, Hidden: true},
		&core.TextField{Name: "agent_intermediate_cert", Max: maxLongText},
		&core.TextField{Name: "agent_intermediate_key", Max: maxSealed, Hidden: true},
		&core.TextField{Name: "server_cert", Max: maxLongText},
		&core.TextField{Name: "server_key", Max: maxSealed, Hidden: true},
		&core.JSONField{Name: "server_names"},
		&core.DateField{Name: "server_expires_at"},
	)
	if err := app.Save(internalCA); err != nil {
		return err
	}

	settings := newCollection(schema.Settings, access{list: schema.RuleViewer, view: schema.RuleViewer, update: schema.RuleAdmin})
	settings.Fields.Add(
		&core.URLField{Name: "public_url"},
		&core.TextField{Name: "grpc_address", Max: 255},
		&core.NumberField{Name: "audit_retention_days", OnlyInt: true},
		&core.NumberField{Name: "command_queue_ttl_s", OnlyInt: true},
	)
	if err := app.Save(settings); err != nil {
		return err
	}

	defaults := core.NewRecord(settings)
	defaults.Set("audit_retention_days", 365)
	defaults.Set("command_queue_ttl_s", 3600)
	if err := app.Save(defaults); err != nil {
		return err
	}

	bundles := newCollection(schema.DiagnosticBundles, access{list: schema.RuleAdmin, view: schema.RuleAdmin, del: schema.RuleAdmin})
	bundles.Fields.Add(
		&core.RelationField{Name: "host", CollectionId: hosts.Id, MaxSelect: 1, CascadeDelete: true},
		&core.RelationField{Name: "requested_by", CollectionId: users.Id, MaxSelect: 1},
		&core.RelationField{Name: "command", CollectionId: commands.Id, MaxSelect: 1},
		&core.FileField{Name: "file", MaxSelect: 1, MaxSize: maxBundle, Protected: true},
		&core.NumberField{Name: "size_bytes", OnlyInt: true},
		&core.DateField{Name: "expires_at"},
	)
	if err := app.Save(bundles); err != nil {
		return err
	}

	// The audit log goes last so that creating the schema is not itself audited.
	// It has no create, update or delete rules, and hooks reject direct writes,
	// for superusers too. Operators see changes; security events are for admins.
	audit := newCollection(schema.AuditLog, access{
		list: schema.RuleAdmin + ` || (@request.auth.role = "operator" && category = "change")`,
		view: schema.RuleAdmin + ` || (@request.auth.role = "operator" && category = "change")`,
	})
	audit.System = true
	audit.Fields.Add(
		&core.NumberField{Name: "seq", OnlyInt: true},
		&core.DateField{Name: "at"},
		&core.SelectField{Name: "category", MaxSelect: 1, Values: []string{"change", "security", "system"}},
		&core.TextField{Name: "action", Max: 64},
		&core.SelectField{Name: "actor_kind", MaxSelect: 1, Values: []string{"user", "token", "superuser", "system", "anonymous"}},
		&core.TextField{Name: "actor", Max: 64},
		&core.TextField{Name: "actor_label", Max: 255},
		&core.TextField{Name: "source", Max: 32},
		&core.TextField{Name: "ip", Max: 64},
		&core.TextField{Name: "user_agent", Max: 512},
		&core.TextField{Name: "collection", Max: 64},
		&core.TextField{Name: "record_id", Max: 64},
		&core.TextField{Name: "record_label", Max: 255},
		&core.TextField{Name: "host", Max: 64},
		&core.JSONField{Name: "changes", MaxSize: 1 << 22},
		&core.JSONField{Name: "snapshot", MaxSize: 1 << 22},
		&core.TextField{Name: "command", Max: 64},
		&core.TextField{Name: "revision", Max: 64},
		&core.TextField{Name: "note", Max: 1024},
		&core.TextField{Name: "prev_hash", Max: 128},
		&core.TextField{Name: "hash", Max: 128},
	)
	audit.AddIndex("idx_audit_log_seq", true, "seq", "")
	audit.AddIndex("idx_audit_log_category_at", false, "category, at", "")
	audit.AddIndex("idx_audit_log_record", false, "collection, record_id", "")
	audit.AddIndex("idx_audit_log_host", false, "host", "")

	return app.Save(audit)
}

func down(app core.App) error {
	// reverse order, because of the relations between the collections
	for _, name := range []string{
		schema.AuditLog, schema.DiagnosticBundles, schema.Settings, schema.InternalCA,
		schema.Networks, schema.Volumes, schema.Containers, schema.StackRevisions, schema.Stacks,
		schema.Alerts, schema.Events, schema.Commands, schema.EnrollmentTokens, schema.RevokedCerts, schema.Hosts,
	} {
		collection, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			return err
		}

		// system collections cannot be deleted
		if collection.System {
			collection.System = false
			if err := app.Save(collection); err != nil {
				return err
			}
		}

		if err := app.Delete(collection); err != nil {
			return err
		}
	}

	users, err := app.FindCollectionByNameOrId(schema.Users)
	if err != nil {
		return err
	}
	users.Fields.RemoveByName("role")

	return app.Save(users)
}

// updateUsers adds roles to the built-in users collection and closes its public
// sign-up: only admins create accounts, and only admins can change a role.
func updateUsers(app core.App) error {
	users, err := app.FindCollectionByNameOrId(schema.Users)
	if err != nil {
		return err
	}

	users.Fields.Add(&core.SelectField{
		Name:      "role",
		Required:  true,
		MaxSelect: 1,
		Values:    []string{schema.RoleAdmin, schema.RoleOperator, schema.RoleViewer},
	})

	self := `id = @request.auth.id`
	users.ListRule = types.Pointer(self + ` || ` + schema.RuleAdmin)
	users.ViewRule = types.Pointer(self + ` || ` + schema.RuleAdmin)
	users.CreateRule = types.Pointer(schema.RuleAdmin)
	users.UpdateRule = types.Pointer(schema.RuleAdmin + ` || (` + self + ` && @request.body.role:isset = false)`)
	users.DeleteRule = types.Pointer(schema.RuleAdmin)
	users.ManageRule = types.Pointer(schema.RuleAdmin)

	return app.Save(users)
}

// access holds the API rules of a collection; an empty rule means superusers only.
type access struct {
	list, view, create, update, del string
}

func newCollection(name string, rules access) *core.Collection {
	c := core.NewBaseCollection(name)
	c.ListRule = rule(rules.list)
	c.ViewRule = rule(rules.view)
	c.CreateRule = rule(rules.create)
	c.UpdateRule = rule(rules.update)
	c.DeleteRule = rule(rules.del)
	c.Fields.Add(
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	return c
}

func rule(r string) *string {
	if r == "" {
		return nil
	}
	return types.Pointer(r)
}
