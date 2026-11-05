package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
	"github.com/oldbear24/PocketServerManager/internal/sites"
)

// Static sites (#21). A deployment fetches its source into the sites volume,
// builds it in a job container from the site's build image, and makes the
// result the release the edge serves; one that fails leaves the served
// release as it is. Builds run on the server that serves the site (ADR 0004).

const (
	// siteLabel names the site of a job container.
	siteLabel = "pocketcloud.site"
	// default limits of a build
	defaultBuildCPUs   = 1.0
	defaultBuildMemory = 2 << 30
	// cleanupTimeout bounds removing what a failed deployment left
	cleanupTimeout = time.Minute
)

// toolRun is one run of the site tool.
type toolRun struct {
	Site    string
	Args    []string
	Env     map[string]string
	Network bool
	// Upload is a file to put into the deployment's build directory first.
	Upload     string
	Deployment int
}

// buildRun is one build.
type buildRun struct {
	Site       string
	Deployment int
	Plan       sites.Plan
	CPUs       float64
	Memory     int64
}

// siteRunner runs the steps of a deployment: in job containers, or in a
// directory for tests.
type siteRunner interface {
	// Tool runs the site tool and returns the JSON it printed.
	Tool(ctx context.Context, run toolRun, log io.Writer) ([]byte, error)
	// Build runs a plan's command in its image, in the deployment's source.
	Build(ctx context.Context, run buildRun, log io.Writer) error
}

func (a *Agent) siteRunner() siteRunner {
	if a.Config.LocalSites != "" {
		return localSites{root: a.Config.LocalSites}
	}
	return dockerSites{a: a}
}

func (a *Agent) registerSiteHandlers() {
	x := a.Executor
	x.Handle("site.deploy", a.handleSiteDeploy)
	x.Handle("site.rollback", a.handleSiteRollback)
	x.Handle("site.remove", a.handleSiteRemove)
}

func (a *Agent) handleSiteDeploy(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetSiteDeploy()
	site, n := payload.GetSite(), int(payload.GetDeployment())
	if err := sites.ValidName(site); err != nil {
		return nil, err
	}
	if n < 1 {
		return nil, errors.New("the deployment has no number")
	}
	runner := a.siteRunner()
	deployment := []string{"--site", site, "--n", strconv.Itoa(n)}

	source := toolRun{Site: site, Deployment: n, Args: append([]string{"source",
		"--kind", payload.GetSource(),
		"--generator", payload.GetGenerator(),
		"--image", payload.GetBuildImage(),
		"--command", payload.GetBuildCmd(),
		"--output", payload.GetOutputDir(),
	}, deployment...)}
	switch payload.GetSource() {
	case sites.SourceGit:
		source.Args = append(source.Args, "--url", payload.GetRepoUrl(), "--branch", payload.GetBranch())
		source.Env = map[string]string{"GIT_TOKEN": payload.GetGitToken()}
		source.Network = true
		reporter.Progress("source", "Cloning "+payload.GetRepoUrl(), 10)
	case sites.SourceUpload:
		reporter.Progress("source", "Fetching the upload", 10)
		upload, err := a.fetchUpload(ctx, payload.GetArtifactId())
		if err != nil {
			return nil, fmt.Errorf("the upload could not be fetched: %w", err)
		}
		defer os.Remove(upload)
		source.Upload = upload
	case sites.SourceEmpty:
		reporter.Progress("source", "Writing the placeholder page", 10)
	default:
		return nil, fmt.Errorf("unknown source %q", payload.GetSource())
	}

	failed := func(err error) (map[string]any, error) {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_, _ = runner.Tool(cleanup, toolRun{Site: site, Deployment: n, Args: append([]string{"clean"}, deployment...)}, io.Discard)
		fmt.Fprintf(reporter, "\nThe deployment failed: %v\nThe site still serves the release it served before.\n", err)
		return nil, err
	}

	out, err := runner.Tool(ctx, source, reporter)
	if err != nil {
		return failed(err)
	}
	var found sourceResult
	if err := json.Unmarshal(out, &found); err != nil {
		return failed(fmt.Errorf("the site tool said: %s", out))
	}
	plan := found.Plan

	started := time.Now()
	if plan.Command != "" {
		reporter.Progress("build", "Building in "+plan.Image, 30)
		build := buildRun{Site: site, Deployment: n, Plan: plan, CPUs: payload.GetCpuLimit(), Memory: payload.GetMemLimitBytes()}
		if err := runner.Build(ctx, build, reporter); err != nil {
			return failed(err)
		}
	}
	buildMs := time.Since(started).Milliseconds()

	reporter.Progress("release", "Switching to the new release", 90)
	keep := int(payload.GetKeepReleases())
	if keep < 1 {
		keep = 5
	}
	out, err = runner.Tool(ctx, toolRun{Site: site, Deployment: n, Args: append([]string{"release",
		"--output", plan.Output, "--keep", strconv.Itoa(keep)}, deployment...)}, reporter)
	if err != nil {
		return failed(err)
	}
	var release releaseResult
	if err := json.Unmarshal(out, &release); err != nil {
		return failed(fmt.Errorf("the site tool said: %s", out))
	}

	releases := make([]any, 0, len(release.Releases))
	for _, number := range release.Releases {
		releases = append(releases, float64(number))
	}
	return map[string]any{
		"release":    float64(n),
		"size_bytes": float64(release.SizeBytes),
		"files":      float64(release.Files),
		"releases":   releases,
		"commit":     found.Commit,
		"message":    found.Message,
		"generator":  plan.Generator,
		"image":      plan.Image,
		"command":    plan.Command,
		"build_ms":   float64(buildMs),
	}, nil
}

func (a *Agent) handleSiteRollback(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	payload := command.GetSiteRollback()
	site, n := payload.GetSite(), int(payload.GetDeployment())
	if err := sites.ValidName(site); err != nil {
		return nil, err
	}
	started := time.Now()
	_, err := a.siteRunner().Tool(ctx, toolRun{Site: site, Deployment: n,
		Args: []string{"switch", "--site", site, "--n", strconv.Itoa(n)}}, reporter)
	if err != nil {
		return nil, err
	}
	return map[string]any{"release": float64(n), "switch_ms": float64(time.Since(started).Milliseconds())}, nil
}

func (a *Agent) handleSiteRemove(ctx context.Context, command *agentv1.Command, reporter *Reporter) (map[string]any, error) {
	site := command.GetSiteRemove().GetSite()
	if err := sites.ValidName(site); err != nil {
		return nil, err
	}
	_, err := a.siteRunner().Tool(ctx, toolRun{Site: site, Args: []string{"remove", "--site", site}}, reporter)
	return map[string]any{}, err
}

// fetchUpload downloads an uploaded source from the panel into the agent's
// volume, checking its size and digest.
func (a *Agent) fetchUpload(ctx context.Context, id string) (string, error) {
	client := a.client.Load()
	if client == nil {
		return "", errors.New("not connected")
	}
	stream, err := (*client).FetchArtifact(ctx, &agentv1.FetchArtifactRequest{ArtifactId: id})
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(a.Config.StateDir, "site-upload-*.zip")
	if err != nil {
		return "", err
	}
	path := file.Name()
	fail := func(err error) (string, error) {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}

	hash := sha256.New()
	var total, received uint64
	var digest string
	for first := true; ; first = false {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(err)
		}
		if first {
			total, digest = chunk.GetTotalBytes(), chunk.GetSha256()
		}
		if _, err := file.Write(chunk.GetData()); err != nil {
			return fail(err)
		}
		hash.Write(chunk.GetData())
		received += uint64(len(chunk.GetData()))
	}
	if received != total {
		return fail(fmt.Errorf("got %d of %d bytes", received, total))
	}
	if digest != "" && hex.EncodeToString(hash.Sum(nil)) != digest {
		return fail(errors.New("the upload's digest does not match"))
	}
	return path, file.Close()
}

// dockerSites runs the steps in job containers. The site tool is the agent
// itself, run from the agent's own image with the sites volume mounted; a
// build runs in the site's build image and sees its own source only.
type dockerSites struct{ a *Agent }

func (d dockerSites) Tool(ctx context.Context, run toolRun, log io.Writer) ([]byte, error) {
	self, err := d.a.ownContainer(ctx)
	if err != nil {
		return nil, fmt.Errorf("the agent cannot find its own image for the site tool: %w", err)
	}
	env := []string{}
	for name, value := range run.Env {
		env = append(env, name+"="+value)
	}
	network := "none"
	if run.Network {
		network = "bridge"
	}
	config := map[string]any{
		"Image":  self.Image,
		"Cmd":    append([]string{"site-tool"}, run.Args...),
		"Env":    env,
		"Labels": map[string]string{"pocketcloud.role": "job", siteLabel: run.Site},
		"HostConfig": map[string]any{
			"Mounts":      []map[string]any{{"Type": "volume", "Source": sites.Volume, "Target": sites.Root}},
			"NetworkMode": network,
			"SecurityOpt": []string{"no-new-privileges"},
			"LogConfig":   map[string]any{"Type": "json-file"},
		},
	}
	var stdout bytes.Buffer
	err = d.run(ctx, config, func(id string) error {
		if run.Upload == "" {
			return nil
		}
		file, err := os.Open(run.Upload)
		if err != nil {
			return err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		name := path.Join(run.Site, ".build", strconv.Itoa(run.Deployment), sites.UploadName)
		return d.a.Docker.CopyFileFrom(ctx, id, sites.Root, name, file, info.Size(), 0o644)
	}, &stdout, log)
	if err != nil {
		return nil, err
	}
	return lastLine(stdout.Bytes()), nil
}

func (d dockerSites) Build(ctx context.Context, run buildRun, log io.Writer) error {
	fmt.Fprintf(log, "Pulling %s\n", run.Plan.Image)
	if err := d.a.Docker.Pull(ctx, run.Plan.Image, nil, nil); err != nil {
		return fmt.Errorf("the build image %s could not be pulled: %w", run.Plan.Image, err)
	}
	mount, err := d.sitesMount(ctx, path.Join(run.Site, ".build", strconv.Itoa(run.Deployment), "src"), "/src")
	if err != nil {
		return err
	}
	cpus, memory := run.CPUs, run.Memory
	if cpus <= 0 {
		cpus = defaultBuildCPUs
	}
	if memory <= 0 {
		memory = defaultBuildMemory
	}
	fmt.Fprintf(log, "$ %s\n", run.Plan.Command)
	config := map[string]any{
		"Image":      run.Plan.Image,
		"Entrypoint": []string{"/bin/sh", "-c"},
		"Cmd":        []string{run.Plan.Command},
		"WorkingDir": "/src",
		// the source belongs to root; the build may write anywhere in it
		"User":   "0",
		"Env":    []string{"CI=true", "HOME=/tmp", "HUGO_CACHEDIR=/tmp/hugo_cache", "npm_config_update_notifier=false"},
		"Labels": map[string]string{"pocketcloud.role": "job", siteLabel: run.Site},
		"HostConfig": map[string]any{
			"Mounts":      []map[string]any{mount},
			"NanoCpus":    int64(cpus * 1e9),
			"Memory":      memory,
			"MemorySwap":  memory,
			"PidsLimit":   4096,
			"CapDrop":     []string{"ALL"},
			"CapAdd":      []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "SETGID", "SETUID", "KILL"},
			"SecurityOpt": []string{"no-new-privileges"},
			"LogConfig":   map[string]any{"Type": "json-file"},
		},
	}
	err = d.run(ctx, config, nil, log, log)
	var exit *exitError
	if errors.As(err, &exit) {
		if exit.oom {
			return fmt.Errorf("the build ran out of memory (%d MiB)", memory>>20)
		}
		return fmt.Errorf("the build failed with exit code %d", exit.code)
	}
	return err
}

// exitError is a job container that ended with a code other than 0.
type exitError struct {
	code    int
	oom     bool
	message string
}

func (e *exitError) Error() string {
	if e.message != "" {
		return e.message
	}
	return "exit code " + strconv.Itoa(e.code)
}

// run creates a job container, lets prepare put files into it, runs it to its
// end with its output going to stdout and stderr, and removes it.
func (d dockerSites) run(ctx context.Context, config map[string]any, prepare func(id string) error, stdout, stderr io.Writer) error {
	id, err := d.a.Docker.Create(ctx, "", config)
	if err != nil {
		return err
	}
	defer func() {
		remove, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_ = d.a.Docker.Remove(remove, id, true, true)
	}()
	if prepare != nil {
		if err := prepare(id); err != nil {
			return err
		}
	}
	if err := d.a.Docker.Start(ctx, id); err != nil {
		return err
	}
	logs, err := d.a.Docker.Logs(ctx, id, dockerapi.LogOptions{Tail: -1, Follow: true})
	if err != nil {
		return err
	}
	var lastErr lastLineWriter
	errWriter := io.MultiWriter(stderr, &lastErr)
	_ = dockerapi.ReadMultiplexed(logs, func(stream byte, data []byte) {
		if stream == 2 {
			_, _ = errWriter.Write(data)
			return
		}
		_, _ = stdout.Write(data)
	})
	_ = logs.Close()

	code, err := d.a.Docker.Wait(ctx, id)
	if err != nil {
		return err
	}
	if code != 0 {
		exit := &exitError{code: code, message: strings.TrimPrefix(lastErr.String(), "error: ")}
		if info, err := d.a.Docker.Inspect(ctx, id); err == nil {
			exit.oom = info.State.OOMKilled
		}
		return exit
	}
	return nil
}

// sitesMount mounts a directory of the sites volume, through a volume subpath,
// or through the volume's directory on an Engine without subpaths.
func (d dockerSites) sitesMount(ctx context.Context, subpath, target string) (map[string]any, error) {
	version, err := d.a.Docker.Version(ctx)
	if err != nil {
		return nil, err
	}
	if apiAtLeast(version.APIVersion, minSubpathAPI) {
		return map[string]any{"Type": "volume", "Source": sites.Volume, "Target": target,
			"VolumeOptions": map[string]any{"Subpath": subpath}}, nil
	}
	volume, err := d.a.Docker.InspectVolume(ctx, sites.Volume)
	if err != nil {
		return nil, err
	}
	if volume.Mountpoint == "" {
		return nil, fmt.Errorf("the Docker Engine %s has no volume subpaths, and the volume %s has no directory on the host to mount instead", version.Version, sites.Volume)
	}
	return map[string]any{"Type": "bind", "Source": filepath.Join(volume.Mountpoint, subpath), "Target": target}, nil
}

// localSites runs the steps in a directory that stands in for the volume:
// the tool in-process, and a build as a shell command. Only tests use it.
type localSites struct{ root string }

func (l localSites) Tool(ctx context.Context, run toolRun, log io.Writer) ([]byte, error) {
	if run.Upload != "" {
		dst := filepath.Join(sites.BuildDir(l.root, run.Site, run.Deployment), sites.UploadName)
		if err := copyFile(run.Upload, dst); err != nil {
			return nil, err
		}
	}
	var stdout bytes.Buffer
	if err := SiteTool(ctx, append(run.Args, "--root", l.root), run.Env, &stdout, log); err != nil {
		return nil, err
	}
	return lastLine(stdout.Bytes()), nil
}

func (l localSites) Build(ctx context.Context, run buildRun, log io.Writer) error {
	fmt.Fprintf(log, "$ %s\n", run.Plan.Command)
	build := exec.CommandContext(ctx, "sh", "-c", run.Plan.Command) //nolint:gosec // the site's own build, in tests
	build.Dir = sites.SourceDir(l.root, run.Site, run.Deployment)
	build.Env = append(os.Environ(), "CI=true")
	build.Stdout, build.Stderr = log, log
	if err := build.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("the build failed with exit code %d", exit.ExitCode())
		}
		return err
	}
	return nil
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // the sites volume
		return err
	}
	in, err := os.Open(src) //nolint:gosec // the agent's own download
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst) //nolint:gosec // in the sites directory
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// lastLine is the last line of output that is not empty.
func lastLine(output []byte) []byte {
	lines := bytes.Split(bytes.TrimSpace(output), []byte("\n"))
	return bytes.TrimSpace(lines[len(lines)-1])
}

// lastLineWriter keeps the last line written to it.
type lastLineWriter struct{ last string }

func (w *lastLineWriter) Write(data []byte) (int, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			w.last = line
		}
	}
	return len(data), nil
}

func (w *lastLineWriter) String() string { return w.last }
