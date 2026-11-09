package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
)

// Backups (#28). restic runs in a job container of the agent's own image, one
// per command, through the backup tool. The panel adds the repository's
// password and the storage's credentials to the command when it delivers it:
// they go into the job's environment, an SFTP key and the server's pinned
// host key into files copied in before it starts, and nowhere else on the
// server. A local target's directory is the only thing the job mounts.

const (
	// backupTargetLabel names the target of a backup job container.
	backupTargetLabel = "pocketcloud.backup-target"
	// resticCache is the volume of restic's cache, which every job of the
	// server shares: restic keeps each repository's apart, by its id, and
	// what it holds is as encrypted as the repository. Without it every
	// check would fetch every snapshot's trees again.
	resticCache    = "pocketcloud-restic-cache"
	resticCacheDir = "/cache"
)

func (a *Agent) registerBackupHandlers() {
	a.Executor.Handle("backup.init", a.handleBackupInit)
}

// handleBackupInit opens a target's repository, or creates it, and says what
// the panel asked of it.
func (a *Agent) handleBackupInit(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetBackupInit()
	args := []string{"init"}
	if payload.GetRepoId() != "" {
		args = append(args, "--expect", payload.GetRepoId())
	}
	if payload.GetUsage() {
		args = append(args, "--usage")
	}
	out, err := a.backupJob(ctx, payload.GetRepository(), args, reporter)
	if err != nil {
		return nil, err
	}
	var result backupInitResult
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("the backup tool said: %s", out)
	}
	answer := map[string]any{"created": result.Created, "id": result.ID}
	if result.Used != nil {
		answer["used_bytes"] = *result.Used
	}
	if result.Free != nil {
		answer["free_bytes"] = *result.Free
	}
	return answer, nil
}

// backupJob runs the backup tool for a repository in a job container, and
// returns the JSON it printed last.
func (a *Agent) backupJob(ctx context.Context, repo *agentv1.BackupRepository, args []string, log io.Writer) ([]byte, error) {
	if repo.GetUrl() == "" || repo.GetPassword() == "" {
		return nil, errors.New("the command has no repository or no password: the panel adds them when it delivers it")
	}
	secrets := backup.Secrets(repo.GetPassword(), repo.GetEnv(), repo.GetSshKey())

	self, err := a.ownContainer(ctx)
	if err != nil {
		return nil, fmt.Errorf("the agent cannot find its own image for the backup job: %w", err)
	}

	env := []string{envResticRepository + "=" + repo.GetUrl(), envResticPassword + "=" + repo.GetPassword()}
	for _, name := range slices.Sorted(maps.Keys(repo.GetEnv())) {
		env = append(env, name+"="+repo.GetEnv()[name])
	}
	cmd := append([]string{"backup-tool"}, args...)
	cmd = append(cmd, "--cache", resticCacheDir)
	for _, name := range slices.Sorted(maps.Keys(repo.GetOptions())) {
		cmd = append(cmd, "-o", name+"="+repo.GetOptions()[name])
	}

	// a remote repository is reached over the network; a local one is a
	// directory of the server, which the job alone mounts
	network := "bridge"
	mounts := []map[string]any{{"Type": "volume", "Source": resticCache, "Target": resticCacheDir}}
	var capabilities []string
	if repo.GetMount() != "" {
		dir, err := backup.LocalPath(repo.GetMount())
		if err != nil {
			return nil, err
		}
		network = "none"
		mounts = append(mounts, map[string]any{
			"Type": "bind", "Source": dir, "Target": backup.LocalMount,
			"BindOptions": map[string]any{"CreateMountpoint": true},
		})
		cmd = append(cmd, "--local", backup.LocalMount)
		// root of the job may use the directory whoever owns it, such as a
		// disk mounted for a user: the job reaches nothing but it and has no
		// network
		capabilities = []string{"DAC_OVERRIDE", "FOWNER"}
	}

	config := map[string]any{
		"Image":  self.Image,
		"Cmd":    cmd,
		"Env":    env,
		"Labels": map[string]string{"pocketcloud.role": "job", backupTargetLabel: repo.GetTargetId()},
		"HostConfig": map[string]any{
			"Mounts":      mounts,
			"NetworkMode": network,
			"CapDrop":     []string{"ALL"},
			"CapAdd":      capabilities,
			"SecurityOpt": []string{"no-new-privileges"},
			"LogConfig":   map[string]any{"Type": "json-file"},
		},
	}
	var stdout bytes.Buffer
	err = dockerSites{a: a}.run(ctx, config, func(id string) error {
		// ssh reads the key and the pinned host key from files
		for _, file := range []struct {
			name    string
			content string
			mode    int64
		}{
			{backup.SSHKeyFile, repo.GetSshKey(), 0o600},
			{backup.KnownHostsFile, repo.GetKnownHosts(), 0o644},
		} {
			if file.content == "" {
				continue
			}
			dir, name := path.Split(file.name)
			parent, sub := path.Split(path.Clean(dir))
			if err := a.Docker.CopyFile(ctx, id, parent, path.Join(sub, name), []byte(file.content), file.mode); err != nil {
				return fmt.Errorf("the job cannot get the SFTP key: %w", err)
			}
		}
		return nil
	}, &stdout, &redactingWriter{w: log, secrets: secrets})
	if err != nil {
		return nil, errors.New(backup.Redact(err.Error(), secrets...))
	}
	return lastLine(stdout.Bytes()), nil
}

// cleanupBackupJobs removes the job containers of backup commands the agent
// was running when it stopped: the commands ended with it.
func (a *Agent) cleanupBackupJobs(ctx context.Context) {
	containers, err := a.Docker.Containers(ctx)
	if err != nil {
		return
	}
	for _, container := range containers {
		if _, ok := container.Labels[backupTargetLabel]; ok && container.Labels["pocketcloud.role"] == "job" {
			if err := a.Docker.Remove(ctx, container.ID, true, true); err != nil {
				a.Log.Warn("failed to remove a backup job container", "container", container.ID, "error", err)
			}
		}
	}
}
