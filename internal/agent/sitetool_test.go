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
	second := filepath.Join(sites.BuildDir(root, "docs", 2), sites.UploadName)
	_ = os.MkdirAll(filepath.Dir(second), 0o755)
	if err := os.WriteFile(second, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	found, _ = tool(t, root, nil, "source", "--site", "docs", "--n", "2", "--kind", "upload", "--generator", "custom", "--image", "alpine:3.22", "--command", "make", "--output", "out")
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

func TestATokenGoesToTheRepositorysServerOnly(t *testing.T) {
	env, err := gitEnv("https://git.example.com:8443/me/blog.git", "tok")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GIT_CONFIG_KEY_0=http.https://git.example.com:8443/.extraHeader") || strings.Contains(joined, "GIT_CONFIG_KEY_0=http.extraHeader") {
		t.Errorf("the header is not scoped: %s", joined)
	}
	if _, err := gitEnv("http://git.example.com/me/blog.git", "tok"); err == nil {
		t.Error("a token over plain HTTP")
	}
	if env, _ := gitEnv("http://git.example.com/me/blog.git", ""); strings.Contains(strings.Join(env, "\n"), "extraHeader") {
		t.Error("a header without a token")
	}
}

func TestImagesWithoutATagGetLatest(t *testing.T) {
	for image, want := range map[string]string{
		"node":                         "node:latest",
		"node:22-alpine":               "node:22-alpine",
		"localhost:5000/hugo":          "localhost:5000/hugo:latest",
		"localhost:5000/hugo:0.152":    "localhost:5000/hugo:0.152",
		"ghcr.io/a/b@sha256:" + "0123": "ghcr.io/a/b@sha256:0123",
	} {
		if got := withTag(image); got != want {
			t.Errorf("%s: %s, not %s", image, got, want)
		}
	}
}

// TestWhatADeploymentLeftIsRemoved: a failed deployment's release, and what
// deployments an agent restart ended left, go; the served release stays.
func TestWhatADeploymentLeftIsRemoved(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"1", "2"} {
		if _, err := tool(t, root, nil, "source", "--site", "blog", "--n", n, "--kind", "empty"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tool(t, root, nil, "release", "--site", "blog", "--n", "1"); err != nil {
		t.Fatal(err)
	}
	// a release that was made but never switched to, as a cancel may leave it
	if _, err := sites.MakeRelease(root, "blog", 2, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := tool(t, root, nil, "clean", "--site", "blog", "--n", "2"); err != nil {
		t.Fatal(err)
	}
	if releases := sites.Releases(root, "blog"); len(releases) != 1 || releases[0] != 1 {
		t.Errorf("releases after clean: %v", releases)
	}

	// deployments an agent restart ended
	for _, n := range []string{"3", "4"} {
		if _, err := tool(t, root, nil, "source", "--site", "blog", "--n", n, "--kind", "empty"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tool(t, root, nil, "sweep"); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "blog", ".build")); len(entries) != 0 {
		t.Errorf("left after the sweep: %v", entries)
	}
	if sites.Current(root, "blog") != 1 {
		t.Error("the sweep changed the served release")
	}
}

func TestAPlaceholderIsNotBuilt(t *testing.T) {
	root := t.TempDir()
	found, err := tool(t, root, nil, "source", "--site", "blog", "--n", "1", "--kind", "empty",
		"--generator", "hugo", "--command", "hugo", "--output", "public")
	if err != nil {
		t.Fatal(err)
	}
	if plan := found["plan"].(map[string]any); plan["generator"] != sites.GeneratorNone || plan["command"] != nil || plan["output"] != nil {
		t.Errorf("the placeholder's plan: %v", plan)
	}
}

// TestATemplatesSiteIsReleasedWithWhatItAsks: the site tool's part of a PHP
// application template (#52): a fresh install starts from nothing, the
// template's files go where the source has none, the links and the
// environment file are made in the release, a cache is emptied once it is
// live, and an update starts from a copy of the live release.
func TestATemplatesSiteIsReleasedWithWhatItAsks(t *testing.T) {
	root := t.TempDir()
	files := `{"wp-config.php":"<?php // PocketCloud"}`

	// a fresh install: no placeholder in the way of WP-CLI
	found, err := tool(t, root, map[string]string{envFiles: files}, "source", "--site", "blog", "--n", "1", "--kind", "empty", "--php",
		"--image", "pocketcloud-php:8.4-fpm", "--command", "wp core download --locale=cs_CZ")
	if err != nil {
		t.Fatal(err)
	}
	if plan := found["plan"].(map[string]any); plan["command"] != "wp core download --locale=cs_CZ" || plan["image"] != "pocketcloud-php:8.4-fpm" {
		t.Errorf("plan: %v", plan)
	}
	entries, _ := os.ReadDir(sites.SourceDir(root, "blog", 1))
	if len(entries) != 1 || entries[0].Name() != "wp-config.php" {
		t.Errorf("a fresh source holds %v", entries)
	}

	// an upload of Laravel: recognised, its .env replaced, storage linked
	upload := filepath.Join(t.TempDir(), "site.zip")
	writeZip(t, upload, map[string]string{"artisan": "#!/usr/bin/env php", "public/index.php": "<?php", ".env": "APP_ENV=local",
		"storage/app/public/logo.png": "png"})
	if err := os.MkdirAll(sites.BuildDir(root, "shop", 1), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(upload, filepath.Join(sites.BuildDir(root, "shop", 1), sites.UploadName)); err != nil {
		t.Fatal(err)
	}
	found, err = tool(t, root, nil, "source", "--site", "shop", "--n", "1", "--kind", "upload", "--php", "--image", "php")
	if err != nil || found["detected"] != "laravel" {
		t.Fatalf("source: %v %v", found, err)
	}
	if _, err := tool(t, root, map[string]string{envEnvFile: "APP_ENV=\"production\"\n"}, "release", "--site", "shop", "--n", "1", "--php",
		"--no-switch", "--shared", "storage", "--links", "public/storage=storage/app/public", "--env-file", ".env"); err != nil {
		t.Fatal(err)
	}
	release := sites.ReleaseDir(root, "shop", 1)
	if env, _ := os.ReadFile(filepath.Join(release, ".env")); string(env) != "APP_ENV=\"production\"\n" {
		t.Errorf(".env: %q", env)
	}
	if logo, err := os.ReadFile(filepath.Join(release, "public", "storage", "logo.png")); err != nil || string(logo) != "png" {
		t.Errorf("public/storage leads nowhere: %q %v", logo, err)
	}
	if _, err := os.Stat(filepath.Join(sites.SharedDir(root, "shop"), "storage", "app", "public", "logo.png")); err != nil {
		t.Errorf("storage is not shared: %v", err)
	}

	// once live, the cache goes
	cache := filepath.Join(sites.SharedDir(root, "shop"), "storage", "framework", "views")
	_ = os.MkdirAll(cache, 0o755)
	_ = os.WriteFile(filepath.Join(cache, "old.php"), []byte("<?php"), 0o644)
	if _, err := tool(t, root, nil, "activate", "--site", "shop", "--n", "1", "--clear", "storage/framework/views"); err != nil {
		t.Fatal(err)
	}
	if sites.Current(root, "shop") != 1 {
		t.Error("release 1 is not live")
	}
	if entries, _ := os.ReadDir(cache); len(entries) != 0 {
		t.Errorf("the cache keeps %v", entries)
	}

	// an update: a copy of release 1
	found, err = tool(t, root, nil, "source", "--site", "shop", "--n", "2", "--kind", "release", "--from", "1", "--php", "--image", "php")
	if err != nil || found["detected"] != "laravel" {
		t.Fatalf("update source: %v %v", found, err)
	}
	if _, err := os.Lstat(filepath.Join(sites.SourceDir(root, "shop", 2), "public", "storage")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the copy has the release's link: %v", err)
	}
	if _, err := tool(t, root, nil, "source", "--site", "shop", "--n", "3", "--kind", "release", "--from", "9", "--php"); err == nil {
		t.Error("a release that is not there was copied")
	}
}

func TestTheSiteToolRecognisesARepositoryWithoutItsFiles(t *testing.T) {
	repo, commit := gitRepo(t, map[string]string{"composer.json": "{}", "index.php": "<?php"})
	found, err := tool(t, t.TempDir(), nil, "detect", "--url", repo, "--branch", "main")
	if err != nil || found["template"] != "" || len(found["commit"].(string)) != 40 {
		t.Fatalf("plain PHP: %v %v", found, err)
	}
	commit("Nette", map[string]string{"app/Bootstrap.php": "<?php", "www/index.php": "<?php"})
	if found, err = tool(t, t.TempDir(), nil, "detect", "--url", repo); err != nil || found["template"] != "nette" {
		t.Errorf("nette: %v %v", found, err)
	}
	// git says why
	if _, err := tool(t, t.TempDir(), nil, "detect", "--url", repo, "--branch", "nosuchbranch"); err == nil || !strings.Contains(err.Error(), "nosuchbranch not found") {
		t.Errorf("a branch that is not there: %v", err)
	}
}

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for name, content := range files {
		writer, _ := archive.Create(name)
		_, _ = writer.Write([]byte(content))
	}
	_ = archive.Close()
	if err := os.WriteFile(path, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
