package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
//	release      move the built site into its release (--built: a build ran
//	             and left an output of its own), switch the site to it, keep
//	             --keep releases and clean up the deployment
//	switch       serve release --n
//	clean        remove what deployment --n left, its release too unless the
//	             site serves it
//	remove       remove the site
//	sweep        remove what deployments of every site left, after an agent
//	             restart ended them
//
// Every op takes --root (the volume's mount point), and all but sweep --site.

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
	repo := flags.String("url", "", "the Git repository")
	branch := flags.String("branch", "", "the Git branch")
	generator := flags.String("generator", sites.GeneratorAuto, "how to build the source")
	image := flags.String("image", "", "the build image")
	command := flags.String("command", "", "the build command")
	output := flags.String("output", "", "where the build leaves the site")
	keep := flags.Int("keep", 5, "how many releases to keep")
	built := flags.Bool("built", false, "a build ran")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if op == "sweep" {
		return sweepBuilds(*root, stdout, stderr)
	}
	if err := sites.ValidName(*site); err != nil {
		return err
	}
	needsDeployment := op != "remove" && op != "sweep"
	if needsDeployment && *n < 1 {
		return errors.New("site-tool: --n names the deployment")
	}

	var result any
	switch op {
	case "source":
		found, err := siteSource(ctx, *root, *site, *n, *kind, *repo, *branch, env["GIT_TOKEN"], stderr)
		if err != nil {
			return err
		}
		switch chosen := *generator; {
		case *kind == sites.SourceEmpty:
			// the placeholder is a page of its own, not a source to build
			found.Plan = sites.Plan{Generator: sites.GeneratorNone}
		case chosen == "" || chosen == sites.GeneratorAuto:
			chosen = sites.Detect(sites.SourceDir(*root, *site, *n))
			fmt.Fprintf(stderr, "The source looks like %s.\n", describeGenerator(chosen))
			found.Plan = sites.PlanFor(chosen, *image, *command, *output)
		default:
			found.Plan = sites.PlanFor(chosen, *image, *command, *output)
		}
		result = found
	case "release":
		release, err := sites.MakeRelease(*root, *site, *n, *output, *built)
		if err != nil {
			return err
		}
		if err := sites.Switch(*root, *site, *n); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "Release %d is live: %d files, %d bytes.\n", *n, release.Files, release.SizeBytes)
		// the release is live from here on, whatever tidying up fails
		if _, err := sites.Prune(*root, *site, max(*keep, 1)); err != nil {
			fmt.Fprintf(stderr, "Old releases could not all be removed: %v\n", err)
		}
		if err := sites.Clean(*root, *site, *n); err != nil {
			fmt.Fprintf(stderr, "The build directory could not be removed: %v\n", err)
		}
		result = releaseResult{Release: release, Releases: sites.Releases(*root, *site)}
	case "switch":
		if err := sites.Switch(*root, *site, *n); err != nil {
			return err
		}
		result = map[string]int{"release": *n}
	case "clean":
		if err := sites.Clean(*root, *site, *n); err != nil {
			return err
		}
		// a release that never went live
		if sites.Current(*root, *site) != *n {
			if err := os.RemoveAll(sites.ReleaseDir(*root, *site, *n)); err != nil {
				return err
			}
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
func siteSource(ctx context.Context, root, site string, n int, kind, repo, branch, token string, log io.Writer) (sourceResult, error) {
	src := sites.SourceDir(root, site, n)
	if err := sites.Prepare(root, site, n); err != nil {
		return sourceResult{}, err
	}
	switch kind {
	case sites.SourceGit:
		return gitClone(ctx, repo, branch, token, src, log)
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
func gitClone(ctx context.Context, repo, branch, token, src string, log io.Writer) (sourceResult, error) {
	env, err := gitEnv(repo, token)
	if err != nil {
		return sourceResult{}, err
	}
	args := []string{"clone", "--depth", "1", "--single-branch", "--no-tags", "--recurse-submodules", "--shallow-submodules"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, "--", repo, src)
	fmt.Fprintf(log, "$ git clone %s (%s)\n", repo, branchOrDefault(branch))

	clone := exec.CommandContext(ctx, "git", args...)
	clone.Env = env
	clone.Stdout = log
	clone.Stderr = log
	if err := clone.Run(); err != nil {
		return sourceResult{}, fmt.Errorf("git could not clone %s: %w", repo, err)
	}

	show := exec.CommandContext(ctx, "git", "-C", src, "log", "-1", "--format=%H%n%s") //nolint:gosec // the deployment's own checkout
	show.Env = env
	out, err := show.Output()
	if err != nil {
		return sourceResult{}, fmt.Errorf("git could not read the commit: %w", err)
	}
	commit, message, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	fmt.Fprintf(log, "Commit %s: %s\n", shortCommit(commit), message)
	return sourceResult{Commit: commit, Message: message}, nil
}

// gitEnv is the environment of Git: no prompts, and a token, if any, as a
// header for the repository's own server only, so a submodule elsewhere or one
// over plain HTTP never gets it.
func gitEnv(repo, token string) ([]string, error) {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false")
	if token == "" {
		return env, nil
	}
	origin, err := url.Parse(repo)
	if err != nil || origin.Scheme != "https" || origin.Host == "" {
		return nil, errors.New("a token is only sent to an HTTPS repository")
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return append(env, "GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://"+origin.Host+"/.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic "+basic), nil
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

// sweepBuilds removes the build directories of every site, and the releases
// that never went live: what deployments left that an agent restart ended.
func sweepBuilds(root string, stdout, stderr io.Writer) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	removed := 0
	for _, entry := range entries {
		site := entry.Name()
		if !entry.IsDir() || sites.ValidName(site) != nil {
			continue
		}
		builds, _ := os.ReadDir(filepath.Join(root, site, ".build"))
		for _, build := range builds {
			n, err := strconv.Atoi(build.Name())
			if err != nil {
				continue
			}
			if err := sites.Clean(root, site, n); err != nil {
				return err
			}
			if sites.Current(root, site) != n {
				_ = os.RemoveAll(sites.ReleaseDir(root, site, n))
			}
			removed++
		}
	}
	fmt.Fprintf(stderr, "Removed what %d deployments left.\n", removed)
	_, err = fmt.Fprintf(stdout, "{\"removed\":%d}\n", removed)
	return err
}
