package agent

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
	"github.com/oldbear24/PocketServerManager/internal/sites"
)

// PHP sites (#50). Each runs its scripts in a PHP-FPM container of its own,
// from PocketCloud's runtime image of its PHP version:
//
//   - it runs as www-data, without capabilities, the Docker socket or
//     published ports, with the site's limits;
//   - it mounts its own site's directory of the sites volume only, read-only,
//     with shared/ writable on top (in classic mode, the whole site is
//     writable): never another site's files;
//   - it is on the site's own network only, where the edge reaches it by its
//     alias, and on the networks of the site's databases. No other site's
//     container reaches its FastCGI port, through which any PHP setting,
//     open_basedir included, can be passed.
//
// The site's pool settings and php.ini lines are files copied into it before
// it starts. A container with other settings or another image is replaced
// with a new one that answers before the old one stops, so that requests
// keep being served; a release switch only reloads PHP-FPM.

const (
	// phpSpecLabel holds a digest of what a PHP-FPM container was made from
	phpSpecLabel = "pocketcloud.php.spec"
	// where the site's own settings go in the runtime image, after its own
	phpPoolDir  = "/usr/local/etc/php-fpm.d"
	phpPoolFile = "zz-pocketcloud-site.conf"
	phpIniDir   = "/usr/local/etc/php/conf.d"
	phpIniFile  = "zzz-pocketcloud-site.ini"
	// phpReadyLimit bounds the wait for a new PHP-FPM to answer
	phpReadyLimit = time.Minute
	// phpStopSeconds lets the requests of an old PHP-FPM finish
	phpStopSeconds = 30
	// phpPids limits the processes of a site's container
	phpPids = 1024
)

func (a *Agent) registerPHPHandlers() {
	a.Executor.Handle("site.php.apply", a.handleSitePHPApply)
	a.Executor.Handle("site.php.status", a.handleSitePHPStatus)
}

// handleSitePHPStatus reads a site's PHP-FPM with the runtime image's status
// tool: its workers, its queue, OPcache and PHP's version.
func (a *Agent) handleSitePHPStatus(ctx context.Context, command *agentv1.Command, _ *Reporter) (map[string]any, error) {
	site := command.GetSitePhpStatus().GetSite()
	if err := sites.ValidName(site); err != nil {
		return nil, err
	}
	var out strings.Builder
	result, err := a.Docker.Run(ctx, sites.PHPContainer(site), dockerapi.ExecOptions{Cmd: []string{"pocketcloud-php-status"}}, &out)
	if err != nil {
		if dockerapi.IsNotFound(err) {
			return nil, fmt.Errorf("%s does not run", sites.PHPContainer(site))
		}
		return nil, err
	}
	// the tool prints what it could read even when a part failed, with the
	// errors that say why
	status := map[string]any{}
	if err := json.Unmarshal([]byte(out.String()), &status); err != nil {
		if result.ExitCode != 0 {
			return nil, fmt.Errorf("the status tool failed: %.300s", strings.TrimSpace(result.Stderr))
		}
		return nil, fmt.Errorf("the status tool said: %.200s", out.String())
	}
	// each worker's last request, with its query string, is the site's
	// visitors' business, and viewers read commands
	if fpm, ok := status["fpm"].(map[string]any); ok {
		delete(fpm, "processes")
	}
	// a result is a protobuf Struct, of JSON's types only
	extensions := []any{}
	for _, name := range a.phpExtensions(ctx, site) {
		extensions = append(extensions, name)
	}
	status["extensions"] = extensions
	if paths := command.GetSitePhpStatus().GetSharedPaths(); len(paths) > 0 {
		shared := map[string]any{}
		for path, size := range a.sharedSizes(ctx, site, paths) {
			shared[path] = size
		}
		status["shared"] = shared
	}
	return status, nil
}

// phpExtensions lists the extensions the runtime image added to PHP's own,
// by their ini files: docker-php-ext-install writes one for each.
func (a *Agent) phpExtensions(ctx context.Context, site string) []string {
	var out strings.Builder
	if _, err := a.Docker.Run(ctx, sites.PHPContainer(site), dockerapi.ExecOptions{Cmd: []string{"ls", "-1", "/usr/local/etc/php/conf.d"}}, &out); err != nil {
		return nil
	}
	extensions := []string{}
	for line := range strings.Lines(out.String()) {
		name, ok := strings.CutPrefix(strings.TrimSpace(line), "docker-php-ext-")
		if name, found := strings.CutSuffix(name, ".ini"); ok && found && name != "" {
			extensions = append(extensions, name)
		}
	}
	slices.Sort(extensions)
	return extensions
}

// sharedSizesWait is how long the sizes of a site's shared paths may take:
// a site with many uploads takes a while.
const sharedSizesWait = 10 * time.Second

// sharedSizes measures a site's shared paths, in bytes, with du in its
// PHP-FPM container, which mounts shared/. A path that is missing, or took
// too long, has no size.
func (a *Agent) sharedSizes(ctx context.Context, site string, paths []string) map[string]int64 {
	// du stops by itself too: the exec's end does not end it in the container
	args := []string{"sh", "-c", `for p; do timeout -s KILL 10 du -sk -- "$p"; done`, "sh"}
	for _, shared := range paths {
		if sites.ValidSharedPath(shared) == nil {
			args = append(args, sites.Root+"/"+site+"/shared/"+path.Clean(shared))
		}
	}
	ctx, cancel := context.WithTimeout(ctx, sharedSizesWait)
	defer cancel()
	var out strings.Builder
	// du fails for a missing path and goes on with the others
	_, _ = a.Docker.Run(ctx, sites.PHPContainer(site), dockerapi.ExecOptions{Cmd: args}, &out)
	prefix := sites.Root + "/" + site + "/shared/"
	sizes := map[string]int64{}
	for line := range strings.Lines(out.String()) {
		kib, path, ok := strings.Cut(strings.TrimSpace(line), "\t")
		size, err := strconv.ParseInt(kib, 10, 64)
		if relative, inside := strings.CutPrefix(path, prefix); ok && err == nil && inside {
			sizes[relative] = size * 1024
		}
	}
	return sizes
}

func (a *Agent) handleSitePHPApply(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	return a.applySitePHP(ctx, command.GetSitePhpApply(), reporter)
}

// applySitePHP makes a site's PHP-FPM container run with its settings, on its
// own or as part of a deployment of a new PHP version.
func (a *Agent) applySitePHP(ctx context.Context, payload *agentv1.SitePhpApply, reporter *Reporter) (map[string]any, error) {
	site := payload.GetSite()
	if err := sites.ValidName(site); err != nil {
		return nil, err
	}
	if payload.GetImage() == "" {
		return nil, errors.New("the PHP site has no runtime image")
	}
	for _, network := range payload.GetNetworks() {
		if !strings.HasPrefix(network, "pcdb-") {
			return nil, fmt.Errorf("%s is not a database network", network)
		}
	}

	reporter.Progress("prepare", "Preparing the site's directories", 10)
	if _, err := a.siteRunner().Tool(ctx, toolRun{Site: site, Args: []string{"php-prepare", "--site", site}}, reporter); err != nil {
		return nil, fmt.Errorf("the site's directories: %w", err)
	}
	network := sites.Network(site)
	if err := a.ensureSiteNetwork(ctx, site); err != nil {
		return nil, fmt.Errorf("the site's network %s: %w", network, err)
	}
	// the edge dials the site's PHP-FPM on its network
	if err := a.attach(ctx, network, edgeName, nil); err != nil && !dockerapi.IsNotFound(err) {
		return nil, fmt.Errorf("the edge could not join the site's network: %w", err)
	}

	reporter.Progress("pull", "Pulling "+payload.GetImage(), 25)
	if err := a.Docker.Pull(ctx, payload.GetImage(), nil, nil); err != nil {
		// the image the server has serves on while its registry cannot be reached
		if _, inspectErr := a.Docker.InspectImage(ctx, payload.GetImage()); inspectErr != nil {
			return nil, fmt.Errorf("the runtime image %s could not be pulled: %w", payload.GetImage(), err)
		}
		fmt.Fprintf(reporter, "The runtime image could not be pulled (%v): the copy this server has is used.\n", err)
	}
	mounts, err := a.phpMounts(ctx, site, payload.GetClassic())
	if err != nil {
		return nil, err
	}
	seccomp, err := phpSeccomp()
	if err != nil {
		return nil, fmt.Errorf("the seccomp profile of PHP: %w", err)
	}
	config := phpContainerConfig(payload, mounts, seccomp)
	name := sites.PHPContainer(site)

	current, err := a.sitePHP(ctx, site, name)
	if err != nil {
		return nil, err
	}
	if current == nil {
		// a swap cut short after the rename: the old container is the one
		// that served, and it serves on until its replacement answers
		if current, err = a.sitePHP(ctx, site, name+"-old"); err != nil {
			return nil, err
		}
		if current != nil {
			if err := a.Docker.Rename(ctx, current.ID, name); err != nil {
				return nil, err
			}
		}
	}
	// a container that was created and never started may lack its settings,
	// which are copied in before the start
	if current != nil && current.State.Status != "created" && checkMounts(current, mounts, "the PHP-FPM container") == nil {
		labels, _ := current.Config["Labels"].(map[string]any)
		if labels[phpSpecLabel] == config["Labels"].(map[string]string)[phpSpecLabel] {
			if !current.State.Running {
				reporter.Progress("start", "Starting "+name, 70)
				if err := a.Docker.Start(ctx, current.ID); err != nil {
					return nil, err
				}
			}
			if err := a.joinDatabaseNetworks(ctx, name, payload.GetNetworks()); err != nil {
				return nil, err
			}
			if err := a.waitPHPReady(ctx, current.ID); err != nil {
				return nil, err
			}
			// one a swap cut short left behind answers to the alias too
			if err := a.removeOldPHP(ctx, site, name+"-old"); err != nil {
				return nil, err
			}
			fmt.Fprintf(reporter, "%s runs as it should: nothing to change.\n", name)
			return a.phpResult(ctx, current.ID, payload.GetImage()), nil
		}
	}

	reporter.Progress("start", "Starting "+name, 60)
	id, err := a.replacePHP(ctx, current, name, config, mounts, payload, reporter)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(reporter, "%s answers, on the network %s.\n", name, network)
	return a.phpResult(ctx, id, payload.GetImage()), nil
}

// replacePHP starts a new PHP-FPM container and, once it answers, stops the
// one it replaces. Both answer to the site's alias meanwhile, and the edge
// tries again on another when one refuses a connection, so no request fails.
// A new container that does not answer is removed, and the old one serves on.
func (a *Agent) replacePHP(ctx context.Context, current *dockerapi.ContainerInspect, name string, config map[string]any,
	mounts []map[string]any, payload *agentv1.SitePhpApply, output io.Writer) (string, error) {
	oldName := name + "-old"
	// a swap that was cut short left this, next to the container that serves
	if err := a.removeOldPHP(ctx, payload.GetSite(), oldName); err != nil {
		return "", err
	}
	// putting the old one back must work after a cancel too
	ctx = context.WithoutCancel(ctx)
	restore := func() {}
	if current != nil {
		if err := a.Docker.Rename(ctx, current.ID, oldName); err != nil {
			return "", err
		}
		restore = func() {
			if err := a.Docker.Rename(ctx, current.ID, name); err != nil {
				a.Log.Error("failed to rename the old PHP-FPM container back", "container", name, "error", err)
			}
			if current.State.Running {
				if err := a.Docker.Start(ctx, current.ID); err != nil {
					a.Log.Error("failed to start the old PHP-FPM container again", "container", name, "error", err)
				}
			}
		}
	}

	id, err := a.Docker.Create(ctx, name, config)
	if err != nil {
		restore()
		return "", fmt.Errorf("the PHP-FPM container could not be created: %w", err)
	}
	fail := func(err error) (string, error) {
		logs := a.tailLogs(ctx, id)
		if removeErr := a.Docker.Remove(ctx, id, false, true); removeErr != nil && !dockerapi.IsNotFound(removeErr) {
			a.Log.Error("failed to remove the new PHP-FPM container", "container", name, "error", removeErr)
		}
		restore()
		if logs != "" {
			return "", fmt.Errorf("%w\n%s", err, logs)
		}
		return "", err
	}
	// an engine that ignored a subpath would give the site every site's files
	info, err := a.Docker.Inspect(ctx, id)
	if err == nil {
		err = checkMounts(info, mounts, "the PHP-FPM container")
	}
	if err != nil {
		return fail(fmt.Errorf("refusing to start PHP-FPM: %w", err))
	}
	if err := a.Docker.CopyFile(ctx, id, phpPoolDir, phpPoolFile, []byte(payload.GetFpmConf()), 0o644); err != nil {
		return fail(fmt.Errorf("the pool settings: %w", err))
	}
	if err := a.Docker.CopyFile(ctx, id, phpIniDir, phpIniFile, []byte(payload.GetPhpIni()), 0o644); err != nil {
		return fail(fmt.Errorf("the php.ini lines: %w", err))
	}
	// the alias the edge dials, which an engine may not have set from the create
	if err := a.attach(ctx, sites.Network(payload.GetSite()), id, []string{name}); err != nil {
		return fail(fmt.Errorf("the site's network: %w", err))
	}
	if err := a.joinDatabaseNetworks(ctx, id, payload.GetNetworks()); err != nil {
		return fail(err)
	}
	if err := a.Docker.Start(ctx, id); err != nil {
		return fail(fmt.Errorf("PHP-FPM did not start: %w", err))
	}
	if err := a.waitPHPReady(ctx, id); err != nil {
		return fail(err)
	}

	if current != nil {
		fmt.Fprintf(output, "Stopping the PHP-FPM it replaces, once its requests are done\n")
		// FPM stops gracefully on its stop signal, SIGQUIT
		if err := a.Docker.Stop(ctx, current.ID, phpStopSeconds); err != nil && !dockerapi.IsNotFound(err) {
			a.Log.Warn("failed to stop the old PHP-FPM container", "container", oldName, "error", err)
		}
		if err := a.Docker.Remove(ctx, current.ID, false, true); err != nil && !dockerapi.IsNotFound(err) {
			a.Log.Warn("failed to remove the old PHP-FPM container", "container", oldName, "error", err)
		}
	}
	return id, nil
}

// phpContainerConfig is a site's PHP-FPM container. Its spec label is a
// digest of all of it, the settings files and secrets included, so that any
// change makes a new container.
func phpContainerConfig(payload *agentv1.SitePhpApply, mounts []map[string]any, seccomp string) map[string]any {
	site := payload.GetSite()
	names := []string{}
	values := map[string]string{}
	for name, value := range payload.GetEnv() {
		names, values[name] = append(names, name), value
	}
	for name, value := range payload.GetSecrets() {
		names, values[name] = append(names, name), value
	}
	slices.Sort(names)
	env := make([]string, 0, len(names))
	for _, name := range slices.Compact(names) {
		env = append(env, name+"="+values[name])
	}

	host := map[string]any{
		"Mounts":        mounts,
		"NetworkMode":   sites.Network(site),
		"RestartPolicy": map[string]any{"Name": "unless-stopped"},
		"PidsLimit":     phpPids,
		"CapDrop":       []string{"ALL"},
		"SecurityOpt":   []string{"no-new-privileges", seccomp},
		"LogConfig":     map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "10m", "max-file": "3"}},
	}
	if cpus := payload.GetCpuLimit(); cpus > 0 {
		host["NanoCpus"] = int64(cpus * 1e9)
	}
	if memory := payload.GetMemLimitBytes(); memory > 0 {
		host["Memory"] = int64(memory)     //nolint:gosec // a limit in bytes, far below 2^63
		host["MemorySwap"] = int64(memory) //nolint:gosec // see above
	}
	config := map[string]any{
		"Image": payload.GetImage(),
		"Env":   env,
		// FPM's graceful stop: running requests finish
		"StopSignal":  "SIGQUIT",
		"StopTimeout": phpStopSeconds,
		"HostConfig":  host,
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{
			sites.Network(site): map[string]any{"Aliases": []string{sites.PHPContainer(site)}},
		}},
	}

	spec, _ := json.Marshal(config)
	// keyed: the spec holds the site's secrets, and viewers see the labels
	sum := hmac.New(sha256.New, specKey)
	sum.Write(spec)
	sum.Write([]byte(payload.GetFpmConf()))
	sum.Write([]byte{0})
	sum.Write([]byte(payload.GetPhpIni()))
	config["Labels"] = map[string]string{
		"pocketcloud.role": "site",
		siteLabel:          site,
		phpSpecLabel:       hex.EncodeToString(sum.Sum(nil))[:16],
	}
	return config
}

// phpMounts mount a site's own directory, read-only unless classic, and its
// shared/ writable on top, where the edge has them: a script's path is the
// same in both, which FastCGI needs.
func (a *Agent) phpMounts(ctx context.Context, site string, classic bool) ([]map[string]any, error) {
	runner := dockerSites{a: a}
	target := path.Join(sites.Root, site)
	own, err := runner.sitesMount(ctx, site, target)
	if err != nil {
		return nil, err
	}
	if classic {
		return []map[string]any{own}, nil
	}
	own["ReadOnly"] = true
	shared, err := runner.sitesMount(ctx, path.Join(site, "shared"), path.Join(target, "shared"))
	if err != nil {
		return nil, err
	}
	return []map[string]any{own, shared}, nil
}

// ensureSiteNetwork creates a PHP site's network. It is not internal: the
// site's code may call out, as to a payment provider's API.
func (a *Agent) ensureSiteNetwork(ctx context.Context, site string) error {
	name := sites.Network(site)
	if _, err := a.Docker.InspectNetwork(ctx, name); err == nil {
		return nil
	} else if !dockerapi.IsNotFound(err) {
		return err
	}
	_, err := a.Docker.CreateNetwork(ctx, name, dockerapi.NetworkOptions{
		Labels: map[string]string{"pocketcloud.role": "site-network", siteLabel: site},
	})
	if err != nil {
		// another command may have created it in the meantime
		if _, inspectErr := a.Docker.InspectNetwork(ctx, name); inspectErr == nil {
			return nil
		}
	}
	return err
}

// joinDatabaseNetworks puts a container on the networks of the site's
// databases, which exist by now.
func (a *Agent) joinDatabaseNetworks(ctx context.Context, container string, networks []string) error {
	for _, network := range networks {
		if err := a.ensureNetwork(ctx, network); err != nil {
			return fmt.Errorf("the database network %s: %w", network, err)
		}
		if err := a.attach(ctx, network, container, nil); err != nil {
			return fmt.Errorf("the database network %s: %w", network, err)
		}
	}
	return nil
}

// joinSiteNetworks puts the edge on the networks of every PHP site, as after
// it was replaced.
func (a *Agent) joinSiteNetworks(ctx context.Context, container string) {
	networks, err := a.Docker.Networks(ctx)
	if err != nil {
		a.Log.Warn("failed to list the networks of the PHP sites", "error", err)
		return
	}
	for _, network := range networks {
		if network.Labels["pocketcloud.role"] != "site-network" {
			continue
		}
		if err := a.attach(ctx, network.Name, container, nil); err != nil {
			a.Log.Warn("the edge could not join a PHP site's network", "network", network.Name, "error", err)
		}
	}
}

// waitPHPReady waits until PHP-FPM answers its ping.
func (a *Agent) waitPHPReady(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, phpReadyLimit)
	defer cancel()
	var last string
	for {
		result, err := a.Docker.Run(ctx, id, dockerapi.ExecOptions{Cmd: []string{"pocketcloud-php-status", "-ping"}}, io.Discard)
		if err == nil && result.ExitCode == 0 {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = strings.TrimSpace(result.Stderr)
		}
		if info, inspectErr := a.Docker.Inspect(ctx, id); inspectErr == nil && !info.State.Running && info.State.Status == "exited" {
			return fmt.Errorf("PHP-FPM stopped with exit code %d", info.State.ExitCode)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("PHP-FPM did not answer within %s: %s", phpReadyLimit, last)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// reloadPHP lets a site's PHP-FPM start its workers anew, as after a release
// switch: the running requests finish first.
func (a *Agent) reloadPHP(ctx context.Context, site string, output io.Writer) {
	name := sites.PHPContainer(site)
	current, err := a.sitePHP(ctx, site, name)
	switch {
	case err != nil:
		fmt.Fprintf(output, "%s could not be reloaded: %v\n", name, err)
		return
	case current == nil:
		fmt.Fprintf(output, "%s does not exist yet: the new release is served once it starts.\n", name)
		return
	}
	if err := a.Docker.Kill(ctx, current.ID, "USR2"); err != nil {
		fmt.Fprintf(output, "%s could not be reloaded: %v\n", name, err)
		return
	}
	fmt.Fprintf(output, "%s reloads its workers.\n", name)
}

// removePHP removes a site's PHP-FPM container and its network.
func (a *Agent) removePHP(ctx context.Context, site string) error {
	name := sites.PHPContainer(site)
	for _, container := range []string{name, name + "-old"} {
		// what is not the site's, such as a container of the same name that a
		// static site's owner runs, stays
		info, err := a.Docker.Inspect(ctx, container)
		if dockerapi.IsNotFound(err) || (err == nil && !ownedPHP(info, site)) {
			continue
		}
		if err != nil {
			return err
		}
		if err := a.Docker.Remove(ctx, info.ID, false, true); err != nil && !dockerapi.IsNotFound(err) {
			return err
		}
	}
	network := sites.Network(site)
	if info, err := a.Docker.InspectNetwork(ctx, network); dockerapi.IsNotFound(err) ||
		(err == nil && (info.Labels["pocketcloud.role"] != "site-network" || info.Labels[siteLabel] != site)) {
		return nil
	}
	if err := a.Docker.DisconnectNetwork(ctx, network, edgeName, true); err != nil && !dockerapi.IsNotFound(err) {
		a.Log.Warn("the edge could not leave a removed site's network", "network", network, "error", err)
	}
	if err := a.Docker.RemoveNetwork(ctx, network); err != nil && !dockerapi.IsNotFound(err) {
		return err
	}
	return nil
}

// specKey keys the digest of a PHP-FPM container's settings in its label: a
// new one at every start of the agent, which replaces a container once more
// at most.
var specKey = func() []byte {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	return key
}()

// ownedPHP says whether a container is the PHP-FPM of a site, as the agent
// labels it: a container of the same name that someone else runs is left as
// it is.
func ownedPHP(info *dockerapi.ContainerInspect, site string) bool {
	labels, _ := info.Config["Labels"].(map[string]any)
	return labels["pocketcloud.role"] == "site" && labels[siteLabel] == site
}

// sitePHP is a site's PHP-FPM container by its name, nil when there is none;
// one of that name that is not the site's is an error.
func (a *Agent) sitePHP(ctx context.Context, site, name string) (*dockerapi.ContainerInspect, error) {
	info, err := a.Docker.Inspect(ctx, name)
	if dockerapi.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !ownedPHP(info, site) {
		return nil, fmt.Errorf("a container named %s is not the site's PHP-FPM: rename or remove it", name)
	}
	return info, nil
}

// removeOldPHP stops and removes the container a replacement left behind.
func (a *Agent) removeOldPHP(ctx context.Context, site, name string) error {
	old, err := a.sitePHP(ctx, site, name)
	if err != nil || old == nil {
		return err
	}
	if err := a.Docker.Stop(ctx, old.ID, phpStopSeconds); err != nil && !dockerapi.IsNotFound(err) {
		a.Log.Warn("failed to stop a leftover PHP-FPM container", "container", name, "error", err)
	}
	if err := a.Docker.Remove(ctx, old.ID, false, true); err != nil && !dockerapi.IsNotFound(err) {
		return err
	}
	return nil
}

func (a *Agent) phpResult(ctx context.Context, id, image string) map[string]any {
	result := map[string]any{"container": id, "image": image, "state": "running"}
	if info, err := a.Docker.InspectImage(ctx, image); err == nil && len(info.RepoDigests) > 0 {
		result["digest"] = info.RepoDigests[0]
	}
	return result
}

// tailLogs is the end of a container's log, for an error that says why it failed.
func (a *Agent) tailLogs(ctx context.Context, id string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	logs, err := a.Docker.Logs(ctx, id, dockerapi.LogOptions{Tail: 20})
	if err != nil {
		return ""
	}
	defer logs.Close()
	var text strings.Builder
	_ = dockerapi.ReadMultiplexed(logs, func(_ byte, data []byte) {
		if text.Len() < 4096 {
			text.Write(data)
		}
	})
	return strings.TrimSpace(text.String())
}
