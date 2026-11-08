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
	"regexp"
	"strconv"
	"strings"

	"github.com/oldbear24/PocketServerManager/internal/phpapps"
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
//	             --keep releases and clean up the deployment; --php keeps the
//	             whole source, links --shared paths from shared/ and gives the
//	             release to www-data; --no-switch leaves the switch to activate
//	activate     serve release --n and keep --keep releases, once a PHP
//	             site's release hook ran in it
//	php-prepare  make a PHP site's directories, which its PHP-FPM mounts
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
	// Detected is the PHP application template the source looks like (#52)
	Detected string `json:"detected,omitempty"`
}

// Environment of the site tool's job with what a PHP site's template gives
// (#52): files a source lacks, as a JSON object of path to content, and the
// content of the release's environment file, which holds the site's secrets
// and so goes neither on the command line nor into the log.
const (
	envFiles   = "POCKETCLOUD_FILES"
	envEnvFile = "POCKETCLOUD_ENV_FILE"
	envToken   = "GIT_TOKEN"
)

// SiteToolEnv is what the site tool reads of its job's environment: the
// agent passes these to the job, and the job's binary to the tool.
func SiteToolEnv(getenv func(string) string) map[string]string {
	env := map[string]string{}
	for _, name := range []string{envToken, envFiles, envEnvFile} {
		if value := getenv(name); value != "" {
			env[name] = value
		}
	}
	return env
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
	commit := flags.String("commit", "", "the commit of the branch to build, rather than its newest")
	generator := flags.String("generator", sites.GeneratorAuto, "how to build the source")
	image := flags.String("image", "", "the build image")
	command := flags.String("command", "", "the build command")
	output := flags.String("output", "", "where the build leaves the site")
	keep := flags.Int("keep", 5, "how many releases to keep")
	built := flags.Bool("built", false, "a build ran")
	php := flags.Bool("php", false, "a PHP site (#50)")
	shared := flags.String("shared", "", "a PHP site's shared paths, separated by commas")
	noSwitch := flags.Bool("no-switch", false, "leave the switch to activate")
	from := flags.Int("from", 0, "the release whose copy is the source, for --kind release")
	links := flags.String("links", "", "a PHP release's links, path=target separated by commas")
	envFile := flags.String("env-file", "", "the file of a PHP release that its environment is written into")
	clear := flags.String("clear", "", "shared paths emptied once the release is live, separated by commas")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if op == "sweep" {
		return sweepBuilds(*root, stdout, stderr)
	}
	if op == "detect" {
		found, err := detectRepo(ctx, gitSource{repo: *repo, branch: *branch, token: env[envToken]}, stderr)
		if err != nil {
			return err
		}
		line, err := json.Marshal(found)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, string(line))
		return err
	}
	if err := sites.ValidName(*site); err != nil {
		return err
	}
	needsDeployment := op != "remove" && op != "sweep" && op != "php-prepare"
	if needsDeployment && *n < 1 {
		return errors.New("site-tool: --n names the deployment")
	}

	var result any
	switch op {
	case "source":
		// a PHP site without a source of its own whose template installs it
		// fresh, as WordPress with WP-CLI, starts from nothing (#52)
		fresh := *php && *kind == sites.SourceEmpty && *command != ""
		found, err := siteSource(ctx, *root, *site, *n, *kind, *from, fresh, gitSource{*repo, *branch, *commit, env[envToken]}, stderr)
		if err != nil {
			return err
		}
		switch chosen := *generator; {
		case *php:
			src := sites.SourceDir(*root, *site, *n)
			if !fresh {
				found.Detected = phpapps.DetectDir(src)
				if template, ok := phpapps.Get(found.Detected); ok {
					fmt.Fprintf(stderr, "The source looks like a %s application.\n", template.Title)
				}
			}
			if raw := env[envFiles]; raw != "" {
				var files map[string]string
				if err := json.Unmarshal([]byte(raw), &files); err != nil {
					return fmt.Errorf("site-tool: the files of the template: %w", err)
				}
				if err := sites.WriteMissing(src, files); err != nil {
					return err
				}
			}
			// a PHP site's build runs as www-data, who must write its vendor/
			found.Plan = sites.PHPPlan(*image, *command)
			if err := sites.Chown(src); err != nil {
				return err
			}
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
		var release sites.Release
		var err error
		if *php {
			release, err = phpRelease(*root, *site, *n, *shared, *links, *envFile, env[envEnvFile])
		} else {
			release, err = sites.MakeRelease(*root, *site, *n, *output, *built)
		}
		if err != nil {
			return err
		}
		if *noSwitch {
			if err := sites.Clean(*root, *site, *n); err != nil {
				fmt.Fprintf(stderr, "The build directory could not be removed: %v\n", err)
			}
			fmt.Fprintf(stderr, "Release %d is ready: %d files, %d bytes.\n", *n, release.Files, release.SizeBytes)
			result = releaseResult{Release: release, Releases: sites.Releases(*root, *site)}
			break
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
	case "activate":
		if err := sites.Switch(*root, *site, *n); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "Release %d is live.\n", *n)
		// what the old code cached must not serve the new (#52); the release
		// is live whatever this leaves
		if cleared := splitList(*clear); len(cleared) > 0 {
			if err := sites.ClearShared(*root, *site, cleared); err != nil {
				fmt.Fprintf(stderr, "%s could not all be emptied: %v\n", strings.Join(cleared, ", "), err)
			} else {
				fmt.Fprintf(stderr, "Emptied %s.\n", strings.Join(cleared, ", "))
			}
		}
		if _, err := sites.Prune(*root, *site, max(*keep, 1)); err != nil {
			fmt.Fprintf(stderr, "Old releases could not all be removed: %v\n", err)
		}
		result = releaseResult{Release: sites.Release{Number: *n}, Releases: sites.Releases(*root, *site)}
	case "php-prepare":
		if err := sites.PreparePHP(*root, *site); err != nil {
			return err
		}
		result = map[string]int{}
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

// phpRelease makes a PHP site's release: its whole source, with its shared
// paths linked from shared/, given to www-data, who runs its release hook
// and PHP-FPM.
func phpRelease(root, site string, n int, shared, links, envFile, envContent string) (sites.Release, error) {
	linked, err := sites.ParseLinks(links)
	if err != nil {
		return sites.Release{}, err
	}
	release, err := sites.MakePHPRelease(root, site, n)
	if err != nil {
		return release, err
	}
	if err := sites.PreparePHP(root, site); err != nil {
		return release, err
	}
	if err := sites.LinkShared(root, site, n, splitList(shared)); err != nil {
		return release, err
	}
	// the links a template asks for, which PHP of the site cannot make (#52)
	if err := sites.MakeLinks(root, site, n, linked); err != nil {
		return release, err
	}
	if err := sites.Chown(sites.ReleaseDir(root, site, n)); err != nil {
		return release, err
	}
	// written last, readable by www-data alone
	if envFile != "" {
		if err := sites.WriteEnvFile(root, site, n, envFile, envContent); err != nil {
			return release, err
		}
	}
	return release, nil
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
// gitSource is where a Git source is: a branch, and a commit of it or its newest.
type gitSource struct {
	repo, branch, commit, token string
}

func siteSource(ctx context.Context, root, site string, n int, kind string, from int, fresh bool, git gitSource, log io.Writer) (sourceResult, error) {
	src := sites.SourceDir(root, site, n)
	if err := sites.Prepare(root, site, n); err != nil {
		return sourceResult{}, err
	}
	switch kind {
	case sites.SourceGit:
		return gitClone(ctx, git, src, log)
	case sites.SourceUpload:
		upload := filepath.Join(sites.BuildDir(root, site, n), sites.UploadName)
		fmt.Fprintln(log, "Unpacking the upload.")
		if err := sites.Extract(upload, src); err != nil {
			return sourceResult{}, err
		}
		return sourceResult{}, os.Remove(upload)
	case sites.SourceEmpty:
		if fresh {
			fmt.Fprintln(log, "A fresh install: the build makes the site.")
			return sourceResult{}, nil
		}
		return sourceResult{}, sites.Placeholder(src, site)
	case sites.SourceRelease:
		// an update starts from the live release (#52)
		fmt.Fprintf(log, "Copying release %d.\n", from)
		return sourceResult{}, sites.CopyRelease(root, site, from, n)
	}
	return sourceResult{}, fmt.Errorf("site-tool: unknown source %q", kind)
}

// detected is what a look at a repository found (#52).
type detected struct {
	Template string `json:"template"`
	Commit   string `json:"commit,omitempty"`
}

// detectRepo says which PHP application a repository's branch is, by its
// files: they are listed without being downloaded, from a clone without its
// files' contents, made in a directory of the job's own that goes with it.
func detectRepo(ctx context.Context, git gitSource, log io.Writer) (detected, error) {
	env, err := gitEnv(git.repo, git.token)
	if err != nil {
		return detected{}, err
	}
	dir, err := os.MkdirTemp("", "detect-")
	if err != nil {
		return detected{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	args := []string{"clone", "--depth", "1", "--single-branch", "--no-tags", "--filter=blob:none", "--no-checkout", "--quiet"}
	if git.branch != "" {
		args = append(args, "--branch", git.branch)
	}
	args = append(args, "--", git.repo, dir)
	fmt.Fprintf(log, "$ git clone %s (%s)\n", git.repo, branchOrDefault(git.branch))
	clone := exec.CommandContext(ctx, "git", args...)
	clone.Env = env
	// git's own words say why, as a branch that is not there
	said := &lastLineWriter{}
	// one writer for both, which exec then never writes from two goroutines
	both := io.MultiWriter(log, said)
	clone.Stdout = both
	clone.Stderr = both
	if err := clone.Run(); err != nil {
		if why := strings.TrimPrefix(said.String(), "fatal: "); why != "" {
			return detected{}, fmt.Errorf("git could not clone %s: %s", git.repo, why)
		}
		return detected{}, fmt.Errorf("git could not clone %s: %w", git.repo, err)
	}
	list := exec.CommandContext(ctx, "git", "-C", dir, "ls-tree", "-r", "-z", "--name-only", "HEAD") //nolint:gosec // the look's own clone
	list.Env = env
	list.Stderr = log
	out, err := list.Output()
	if err != nil {
		return detected{}, fmt.Errorf("git could not list the files of %s: %w", git.repo, err)
	}
	head := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD") //nolint:gosec // the look's own clone
	head.Env = env
	commit, _ := head.Output()
	paths := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	return detected{Template: phpapps.Detect(paths), Commit: strings.TrimSpace(string(commit))}, nil
}

// gitClone checks out the newest commit of a branch, with its submodules
// (Hugo themes often are). A token goes to Git in its environment, not on its
// command line, and is never written out.
func gitClone(ctx context.Context, git gitSource, src string, log io.Writer) (sourceResult, error) {
	repo, branch := git.repo, git.branch
	env, err := gitEnv(repo, git.token)
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
	if git.commit != "" {
		if err := gitCheckout(ctx, env, src, git.commit, log); err != nil {
			return sourceResult{}, err
		}
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

// gitCheckout moves a shallow clone to a commit of its branch: fetched by
// itself where the server lets a client ask for one, or with the branch's
// whole history.
func gitCheckout(ctx context.Context, env []string, src, commit string, log io.Writer) error {
	if !commitID.MatchString(commit) {
		return fmt.Errorf("%q is not a commit", commit)
	}
	fmt.Fprintf(log, "$ git checkout %s\n", shortCommit(commit))
	git := func(args ...string) error {
		command := exec.CommandContext(ctx, "git", append([]string{"-C", src}, args...)...) //nolint:gosec // the deployment's own checkout
		command.Env = env
		command.Stdout = log
		command.Stderr = log
		return command.Run()
	}
	if git("fetch", "--depth", "1", "--no-tags", "origin", commit) != nil {
		// the clone fetches its one branch
		if err := git("fetch", "--unshallow", "--no-tags", "origin"); err != nil {
			return fmt.Errorf("git could not fetch the commit %s: %w", shortCommit(commit), err)
		}
	}
	if err := git("checkout", "--quiet", "--detach", commit); err != nil {
		return fmt.Errorf("git could not check out the commit %s: %w", shortCommit(commit), err)
	}
	return git("submodule", "update", "--init", "--recursive", "--depth", "1")
}

// commitID is a commit's full SHA-1 or SHA-256 name.
var commitID = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

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
