package panel

import (
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// TestAChangeTheRestartForgotIsNotMadeFromTheStoredTarget (second review of
// #28): a change's new settings live in the panel's memory until its check
// ends. After a restart, its check is neither completed with the stored
// settings, which would say nothing of the change and fail a moved target
// as reached from another server, nor kept as news of the target.
func TestAChangeTheRestartForgotIsNotMadeFromTheStoredTarget(t *testing.T) {
	p, app, _ := newTestPanel(t)
	host := testutil.NewHost(t, app, "backup-node")
	collection, err := app.FindCollectionByNameOrId(schema.BackupTargets)
	if err != nil {
		t.Fatal(err)
	}
	target := core.NewRecord(collection)
	target.Load(map[string]any{"name": "disk", "kind": backup.KindLocal, "host": host.Id, "config": map[string]any{"path": "/srv/disk"},
		"location": "/srv/disk", "status": schema.TargetOK, "repo_id": strings.Repeat("a", 64)})
	if err := p.sealTarget(target, backup.Credentials{}, "repo-password"); err != nil {
		t.Fatal(err)
	}
	if err := app.Save(target); err != nil {
		t.Fatal(err)
	}

	commands, err := app.FindCollectionByNameOrId(schema.Commands)
	if err != nil {
		t.Fatal(err)
	}
	check := core.NewRecord(commands)
	check.Id = "restartcheck001"
	check.Load(map[string]any{"host": host.Id, "type": "backup.init", "status": schema.CommandFailed,
		"payload": map[string]any{"repository": map[string]any{"target_id": target.Id, "url": "/repo"}, "repo_id": target.GetString("repo_id"), "change": true},
		"error":   "the agent restarted while running this command", "dispatched_at": types.NowDateTime()})
	command := &agentv1.Command{Payload: &agentv1.Command_BackupInit{BackupInit: &agentv1.BackupInit{
		Change: true, Repository: &agentv1.BackupRepository{TargetId: target.Id, Url: "/repo"},
	}}}
	if err := p.enrichBackup(check, command); err == nil || !strings.Contains(err.Error(), "restarted during the change") ||
		command.GetBackupInit().GetRepository().GetPassword() != "" {
		t.Errorf("enriched: %v", err)
	}

	p.targetChecked(check)
	stored, err := app.FindRecordById(schema.BackupTargets, target.Id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.GetString("status") != schema.TargetOK || stored.GetString("last_error") != "" {
		t.Errorf("the forgotten change's check was kept: %s %s", stored.GetString("status"), stored.GetString("last_error"))
	}
}
