package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oldbear24/PocketServerManager/internal/sites"
)

func gitRepo(t *testing.T, files map[string]string) (string, func(message string, files map[string]string) string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = dir
		command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	commit := func(message string, files map[string]string) string {
		for name, content := range files {
			path := filepath.Join(dir, name)
			_ = os.MkdirAll(filepath.Dir(path), 0o755)
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		git("add", "-A")
		git("commit", "-q", "-m", message)
		return git("rev-parse", "HEAD")
	}
	commit("First", files)
	return dir, commit
}

func tool(t *testing.T, root string, env map[string]string, args ...string) (map[string]any, error) {
	t.Helper()
	var stdout, log bytes.Buffer
	err := SiteTool(context.Background(), append(args, "--root", root), env, &stdout, &log)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(lastLine(stdout.Bytes()), &result); err != nil {
		t.Fatalf("not JSON: %q (log %s)", stdout.String(), log.String())
	}
	return result, nil
}

func TestTheSiteToolDeploysAGitBranch(t *testing.T) {
	repo, commit := gitRepo(t, map[string]string{"index.html": "<h1>one</h1>"})
	root := t.TempDir()

	found, err := tool(t, root, nil, "source", "--site", "blog", "--n", "1", "--kind", "git", "--url", repo, "--branch", "main")
	if err != nil {
		t.Fatal(err)
	}
	if found["message"] != "First" || len(found["commit"].(string)) != 40 {
		t.Errorf("source: %v", found)
	}
	if plan := found["plan"].(map[string]any); plan["generator"] != sites.GeneratorNone || plan["command"] != nil {
		t.Errorf("plan: %v", plan)
	}
	released, err := tool(t, root, nil, "release", "--site", "blog", "--n", "1", "--keep", "1")
	if err != nil {
		t.Fatal(err)
	}
	if released["number"] != float64(1) || released["files"] != float64(1) {
		t.Errorf("release: %v", released)
	}
	served, _ := os.ReadFile(filepath.Join(sites.CurrentLink(root, "blog"), "index.html"))
	if string(served) != "<h1>one</h1>" {
		t.Errorf("served: %s", served)
	}
	if _, err := os.Stat(filepath.Join(sites.CurrentLink(root, "blog"), ".git")); err == nil {
		t.Error(".git is served")
	}
	if _, err := os.Stat(sites.BuildDir(root, "blog", 1)); !os.IsNotExist(err) {
		t.Errorf("the build directory is left: %v", err)
	}

	second := commit("Second", map[string]string{"index.html": "<h1>two</h1>"})
	found, _ = tool(t, root, nil, "source", "--site", "blog", "--n", "2", "--kind", "git", "--url", repo, "--branch", "main")
	if found["commit"] != second {
		t.Errorf("the second commit: %v, not %s", found["commit"], second)
	}
	released, _ = tool(t, root, nil, "release", "--site", "blog", "--n", "2", "--keep", "1")
	if releases := released["releases"].([]any); len(releases) != 1 || releases[0] != float64(2) {
		t.Errorf("kept: %v", releases)
	}
	// the first release was pruned, so it cannot come back
	if _, err := tool(t, root, nil, "switch", "--site", "blog", "--n", "1"); !errors.Is(err, sites.ErrNoRelease) {
		t.Errorf("switching to a pruned release: %v", err)
	}

	// a branch that is not there
	if _, err := tool(t, root, nil, "source", "--site", "blog", "--n", "3", "--kind", "git", "--url", repo, "--branch", "nope"); err == nil || !strings.Contains(err.Error(), "could not clone") {
		t.Errorf("a missing branch: %v", err)
	}
}

func TestTheSiteToolUnpacksAnUploadAndPlansItsBuild(t *testing.T) {
	root := t.TempDir()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for name, content := range map[string]string{"docs/mkdocs.yml": "site_name: x", "docs/docs/index.md": "# hi"} {
		writer, _ := archive.Create(name)
		_, _ = writer.Write([]byte(content))
	}
	_ = archive.Close()
	upload := filepath.Join(sites.BuildDir(root, "docs", 1), sites.UploadName)
	_ = os.MkdirAll(filepath.Dir(upload), 0o755)
	if err := os.WriteFile(upload, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	found, err := tool(t, root, nil, "source", "--site", "docs", "--n", "1", "--kind", "upload")
	if err != nil {
		t.Fatal(err)
	}
	plan := found["plan"].(map[string]any)
	if plan["generator"] != sites.GeneratorMkDocs || plan["image"] != sites.MkDocsImage || plan["output"] != "site" {
		t.Errorf("plan: %v", plan)
	}
	if _, err := os.Stat(filepath.Join(sites.SourceDir(root, "docs", 1), "mkdocs.yml")); err != nil {
		t.Errorf("unpacked: %v", err)
	}
	if _, err := os.Stat(upload); !os.IsNotExist(err) {
		t.Errorf("the upload is left: %v", err)
	}

	// the site's own choice wins over what the files say
	found, _ = tool(t, root, nil, "source", "--site", "docs", "--n", "2", "--kind", "empty", "--generator", "custom", "--image", "alpine:3.22", "--command", "make", "--output", "out")
	if plan := found["plan"].(map[string]any); plan["image"] != "alpine:3.22" || plan["command"] != "make" || plan["output"] != "out" {
		t.Errorf("custom: %v", plan)
	}
}

func TestABuildRunsInItsSourceAndAFailureSaysWhy(t *testing.T) {
	root := t.TempDir()
	runner := localSites{root: root}
	if _, err := tool(t, root, nil, "source", "--site", "app", "--n", "1", "--kind", "empty"); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	build := buildRun{Site: "app", Deployment: 1, Plan: sites.Plan{Command: "mkdir -p dist && echo built > dist/index.html"}}
	if err := runner.Build(context.Background(), build, &log); err != nil {
		t.Fatalf("%v: %s", err, log.String())
	}
	if !strings.Contains(log.String(), "$ mkdir -p dist") {
		t.Errorf("the log: %s", log.String())
	}
	released, err := tool(t, root, nil, "release", "--site", "app", "--n", "1", "--output", "dist")
	if err != nil || released["files"] != float64(1) {
		t.Fatalf("%v %v", released, err)
	}

	if _, err := tool(t, root, nil, "source", "--site", "app", "--n", "2", "--kind", "empty"); err != nil {
		t.Fatal(err)
	}
	build = buildRun{Site: "app", Deployment: 2, Plan: sites.Plan{Command: "echo failing >&2; exit 3"}}
	if err := runner.Build(context.Background(), build, &log); err == nil || !strings.Contains(err.Error(), "exit code 3") {
		t.Errorf("a failing build: %v", err)
	}
}

func TestTheSiteToolRefusesWhatIsNotASite(t *testing.T) {
	root := t.TempDir()
	cases := [][]string{
		{},
		{"source", "--site", "../etc", "--n", "1", "--kind", "empty"},
		{"source", "--site", "blog", "--kind", "empty"},
		{"source", "--site", "blog", "--n", "1", "--kind", "ftp"},
		{"explode", "--site", "blog", "--n", "1"},
	}
	for _, args := range cases {
		if _, err := tool(t, root, nil, args...); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}
