package agent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
	"github.com/oldbear24/PocketServerManager/internal/sites"
)

// A PHP site's worker (#52), such as Laravel's queue worker: a container of
// its own from the site's runtime image, with its PHP-FPM's environment,
// mounts, limits and networks, that runs a command in the live release. Its
// code is the release it started in, so it starts anew after every switch.
// It has no alias on the site's network: the edge dials PHP-FPM alone.

// workerRole labels a site's worker container.
const workerRole = "site-worker"

// workerStopSeconds lets a worker finish the job it is on: Laravel's stops
// after it on SIGTERM.
const workerStopSeconds = 60

// applyWorker makes the site's worker match its settings: none without a
// command, or one running it. It runs after PHP-FPM was applied, with the
// same mounts.
func (a *Agent) applyWorker(ctx context.Context, payload *agentv1.SitePhpApply, mounts []map[string]any, seccomp string, output io.Writer) error {
	site := payload.GetSite()
	name := sites.PHPWorker(site)
	if payload.GetWorkerCommand() == "" {
		// a container of that name that is not the site's is none of its business
		return a.removeSiteWorker(ctx, site)
	}
	current, err := a.siteWorker(ctx, site, name)
	if err != nil {
		return err
	}
	config := workerConfig(payload, mounts, seccomp)
	if current != nil && current.State.Running && checkMounts(current, mounts, "the worker") == nil {
		labels, _ := current.Config["Labels"].(map[string]any)
		if labels[phpSpecLabel] == config["Labels"].(map[string]string)[phpSpecLabel] {
			return a.joinDatabaseNetworks(ctx, name, payload.GetNetworks())
		}
	}
	if current != nil {
		if err := a.removeWorker(ctx, current); err != nil {
			return err
		}
	}
	id, err := a.Docker.Create(ctx, name, config)
	if err != nil {
		return fmt.Errorf("the worker %s: %w", name, err)
	}
	fail := func(err error) error {
		if removeErr := a.Docker.Remove(context.WithoutCancel(ctx), id, false, true); removeErr != nil && !dockerapi.IsNotFound(removeErr) {
			a.Log.Error("failed to remove a worker that did not start", "container", name, "error", removeErr)
		}
		return fmt.Errorf("the worker %s: %w", name, err)
	}
	info, err := a.Docker.Inspect(ctx, id)
	if err == nil {
		err = checkMounts(info, mounts, "the worker")
	}
	if err != nil {
		return fail(err)
	}
	if err := a.Docker.CopyFile(ctx, id, phpIniDir, phpIniFile, []byte(payload.GetPhpIni()), 0o644); err != nil {
		return fail(fmt.Errorf("the php.ini lines: %w", err))
	}
	if err := a.joinDatabaseNetworks(ctx, id, payload.GetNetworks()); err != nil {
		return fail(err)
	}
	if err := a.Docker.Start(ctx, id); err != nil {
		return fail(err)
	}
	fmt.Fprintf(output, "The worker %s runs %s.\n", name, payload.GetWorkerCommand())
	return nil
}

// workerConfig is the worker's container: PHP-FPM's, with the command and
// working directory of the worker, no health check of FPM and no alias.
func workerConfig(payload *agentv1.SitePhpApply, mounts []map[string]any, seccomp string) map[string]any {
	site := payload.GetSite()
	config := phpContainerConfig(payload, mounts, seccomp)
	// it runs in the release live when it starts, which a new site has once
	// its first deployment went live: the read-only mount has no current
	// before, which the engine cannot make as a working directory
	current := path.Join(sites.Root, site, "current")
	// the shell is PID 1 while it waits, which ignores SIGTERM unless trapped:
	// a stop would wait out its timeout
	config["Cmd"] = []string{"sh", "-c", fmt.Sprintf(
		`trap 'exit 0' TERM; [ -d %[1]s ] || echo "Waiting for the site's first release."; until cd %[1]s 2>/dev/null; do sleep 5 & wait $!; done; exec %[2]s`,
		current, payload.GetWorkerCommand())}
	config["Healthcheck"] = map[string]any{"Test": []string{"NONE"}}
	config["StopSignal"] = "SIGTERM"
	config["StopTimeout"] = workerStopSeconds
	delete(config, "NetworkingConfig")
	delete(config, "Labels")

	spec, _ := json.Marshal(config)
	sum := hmac.New(sha256.New, specKey)
	sum.Write(spec)
	sum.Write([]byte{0})
	sum.Write([]byte(payload.GetPhpIni()))
	config["Labels"] = map[string]string{
		"pocketcloud.role": workerRole,
		siteLabel:          site,
		phpSpecLabel:       hex.EncodeToString(sum.Sum(nil))[:16],
	}
	return config
}

// siteWorker is a site's worker container by its name, nil when there is
// none; one of that name that is not the site's is an error.
func (a *Agent) siteWorker(ctx context.Context, site, name string) (*dockerapi.ContainerInspect, error) {
	info, err := a.Docker.Inspect(ctx, name)
	if dockerapi.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	labels, _ := info.Config["Labels"].(map[string]any)
	if labels["pocketcloud.role"] != workerRole || labels[siteLabel] != site {
		return nil, fmt.Errorf("a container named %s is not the site's worker: rename or remove it", name)
	}
	return info, nil
}

func (a *Agent) removeWorker(ctx context.Context, info *dockerapi.ContainerInspect) error {
	if info.State.Running {
		if err := a.Docker.Stop(ctx, info.ID, workerStopSeconds); err != nil && !dockerapi.IsNotFound(err) {
			a.Log.Warn("failed to stop a worker", "container", info.Name, "error", err)
		}
	}
	if err := a.Docker.Remove(ctx, info.ID, false, true); err != nil && !dockerapi.IsNotFound(err) {
		return err
	}
	return nil
}

// restartWorker starts a site's worker anew in the release live now, as after
// a switch. It finishes the job it is on first, which may take a while: the
// switch or rollback that asked does not wait for it.
func (a *Agent) restartWorker(ctx context.Context, site string, output io.Writer) {
	name := sites.PHPWorker(site)
	current, err := a.siteWorker(ctx, site, name)
	if err != nil {
		fmt.Fprintf(output, "%s could not be restarted: %v\n", name, err)
		return
	}
	if current == nil {
		return
	}
	fmt.Fprintf(output, "%s starts anew in the new release once its job is done.\n", name)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*workerStopSeconds*time.Second)
		defer cancel()
		if err := a.Docker.Restart(ctx, current.ID, workerStopSeconds); err != nil {
			a.Log.Warn("failed to restart a site's worker", "container", name, "error", err)
		}
	}()
}

// removeSiteWorker removes a site's worker, if it has one, as the site goes
// or no longer wants one; a container of the name that is not its stays.
func (a *Agent) removeSiteWorker(ctx context.Context, site string) error {
	info, err := a.Docker.Inspect(ctx, sites.PHPWorker(site))
	if dockerapi.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	labels, _ := info.Config["Labels"].(map[string]any)
	if labels["pocketcloud.role"] != workerRole || labels[siteLabel] != site {
		return nil
	}
	// it finishes the job it is on
	return a.removeWorker(ctx, info)
}
