package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/oldbear24/PocketServerManager/internal/sites"
)

// The site tool changes the sites volume for a deployment: it runs as
// `pocketcloud-agent site-tool <op>` in a job container that mounts the volume
// (the agent's own container does not), and in-process in tests. It writes
// what it does to stderr and, last, its result as one line of JSON to stdout.
//
//	source       make the deployment's directories and fetch its source:
//	             --kind git (--url, --branch, GIT_TOKEN), upload or empty;
//	             then say how to build it (--generator, --image, --command,
//	             --output)
//	release      move the built site into its release, switch the site to it,
//	             keep --keep releases and clean up the deployment
//	switch       serve release --n
//	clean        remove what deployment --n left
//	remove       remove the site
//
// Every op takes --root (the volume's mount point) and --site.

// sourceResult is what the source op found.
type sourceResult struct {
	Commit  string     `json:"commit,omitempty"`
	Message string     `json:"message,omitempty"`
	Plan    sites.Plan `json:"plan"`
}

// releaseResult is what the release op made.
type releaseResult struct {
	sites.Release
	Releases []int `json:"releases"`
}

// SiteTool runs one op of the site tool. env holds the variables it reads
// (GIT_TOKEN).
func SiteTool(ctx context.Context, args []string, env map[string]string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("site-tool: name an op: source, release, switch, clean or remove")
	}
	op := args[0]
	flags := flag.NewFlagSet("site-tool "+op, flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", sites.Root, "where the sites volume is mounted")
	site := flags.String("site", "", "the site")
	n := flags.Int("n", 0, "the deployment")
	kind := flags.String("kind", "", "the source: git, upload or empty")
	url := flags.String("url", "", "the Git repository")
	branch := flags.String("branch", "", "the Git branch")
	generator := flags.String("generator", sites.GeneratorAuto, "how to build the source")
	image := flags.String("image", "", "the build image")
	command := flags.String("command", "", "the build command")
	output := flags.String("output", "", "where the build leaves the site")
	keep := flags.Int("keep", 5, "how many releases to keep")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if err := sites.ValidName(*site); err != nil {
		return err
	}
	needsDeployment := op != "remove"
	if needsDeployment && *n < 1 {
		return errors.New("site-tool: --n names the deployment")
	}

	var result any
	switch op {
	case "source":
		found, err := siteSource(ctx, *root, *site, *n, *kind, *url, *branch, env["GIT_TOKEN"], stderr)
		if err != nil {
			return err
		}
		chosen := *generator
		if chosen == "" || chosen == sites.GeneratorAuto {
			chosen = sites.Detect(sites.SourceDir(*root, *site, *n))
			fmt.Fprintf(stderr, "The source looks like %s.\n", describeGenerator(chosen))
		}
		found.Plan = sites.PlanFor(chosen, *image, *command, *output)
		result = found
	case "release":
		release, err := sites.MakeRelease(*root, *site, *n, *output)
		if err != nil {
			return err
		}
		if err := sites.Switch(*root, *site, *n); err != nil {
			return err
		}
		left, err := sites.Prune(*root, *site, max(*keep, 1))
		if err != nil {
			return err
		}
		if err := sites.Clean(*root, *site, *n); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "Release %d is live: %d files, %d bytes.\n", *n, release.Files, release.SizeBytes)
		result = releaseResult{Release: release, Releases: left}
	case "switch":
		if err := sites.Switch(*root, *site, *n); err != nil {
			return err
		}
		result = map[string]int{"release": *n}
	case "clean":
		if err := sites.Clean(*root, *site, *n); err != nil {
			return err
		}
		result = map[string]int{}
	case "remove":
		if err := sites.Remove(*root, *site); err != nil {
			return err
		}
		result = map[string]int{}
	default:
		return fmt.Errorf("site-tool: unknown op %q", op)
	}
	line, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(line))
	return err
}

func describeGenerator(generator string) string {
	switch generator {
	case sites.GeneratorHugo:
		return "a Hugo site"
	case sites.GeneratorMkDocs:
		return "an MkDocs site"
	case sites.GeneratorAstro:
		return "an Astro site"
	case sites.GeneratorNPM:
		return "an npm project with a build script"
	}
	return "a site to serve as it is"
}

// siteSource makes the deployment's directories and puts its source there.
// An upload is already in the build directory, put there before the tool
// started.
func siteSource(ctx context.Context, root, site string, n int, kind, url, branch, token string, log io.Writer) (sourceResult, error) {
	src := sites.SourceDir(root, site, n)
	if err := sites.Prepare(root, site, n); err != nil {
		return sourceResult{}, err
	}
	switch kind {
	case sites.SourceGit:
		return gitClone(ctx, url, branch, token, src, log)
	case sites.SourceUpload:
		upload := filepath.Join(sites.BuildDir(root, site, n), sites.UploadName)
		fmt.Fprintln(log, "Unpacking the upload.")
		if err := sites.Extract(upload, src); err != nil {
			return sourceResult{}, err
		}
		return sourceResult{}, os.Remove(upload)
	case sites.SourceEmpty:
		return sourceResult{}, sites.Placeholder(src, site)
	}
	return sourceResult{}, fmt.Errorf("site-tool: unknown source %q", kind)
}

// gitClone checks out the newest commit of a branch, with its submodules
// (Hugo themes often are). A token goes to Git in its environment, not on its
// command line, and is never written out.
func gitClone(ctx context.Context, url, branch, token, src string, log io.Writer) (sourceResult, error) {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false")
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+basic)
	}
	args := []string{"clone", "--depth", "1", "--single-branch", "--no-tags", "--recurse-submodules", "--shallow-submodules"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, "--", url, src)
	fmt.Fprintf(log, "$ git clone %s (%s)\n", url, branchOrDefault(branch))

	clone := exec.CommandContext(ctx, "git", args...)
	clone.Env = env
	clone.Stdout = log
	clone.Stderr = log
	if err := clone.Run(); err != nil {
		return sourceResult{}, fmt.Errorf("git could not clone %s: %w", url, err)
	}

	show := exec.CommandContext(ctx, "git", "-C", src, "log", "-1", "--format=%H%n%s")
	show.Env = env
	out, err := show.Output()
	if err != nil {
		return sourceResult{}, fmt.Errorf("git could not read the commit: %w", err)
	}
	commit, message, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	fmt.Fprintf(log, "Commit %s: %s\n", shortCommit(commit), message)
	return sourceResult{Commit: commit, Message: message}, nil
}

func branchOrDefault(branch string) string {
	if branch == "" {
		return "the default branch"
	}
	return "branch " + branch
}

func shortCommit(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}
