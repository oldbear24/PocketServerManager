package sites

import (
	"archive/zip"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSiteNamesAreSafeForPathsAndContainers(t *testing.T) {
	for _, name := range []string{"blog", "docs-2", "a"} {
		if err := ValidName(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, name := range []string{"", "Blog", "-blog", "blog-", "../x", "a/b", "a.b", strings.Repeat("a", 41)} {
		if ValidName(name) == nil {
			t.Errorf("%q is accepted", name)
		}
	}
}

func TestTheGeneratorIsFoundByItsFiles(t *testing.T) {
	cases := map[string]map[string]string{
		GeneratorHugo:   {"hugo.toml": "title = 'x'"},
		GeneratorMkDocs: {"mkdocs.yml": "site_name: x"},
		GeneratorAstro:  {"astro.config.mjs": "export default {}", "package.json": `{"scripts":{"build":"astro build"}}`},
		GeneratorNPM:    {"package.json": `{"scripts":{"build":"vite build"}}`},
		GeneratorNone:   {"index.html": "<h1>hi</h1>", "package.json": `{"scripts":{"test":"x"}}`},
	}
	for want, files := range cases {
		dir := t.TempDir()
		for name, content := range files {
			write(t, filepath.Join(dir, name), content)
		}
		if got := Detect(dir); got != want {
			t.Errorf("%v: %s, not %s", files, got, want)
		}
	}
	// an older Hugo site has config.toml and content/
	dir := t.TempDir()
	write(t, filepath.Join(dir, "config.toml"), "")
	write(t, filepath.Join(dir, "content", "_index.md"), "")
	if got := Detect(dir); got != GeneratorHugo {
		t.Errorf("config.toml with content: %s", got)
	}
}

func TestAPlanHasTheSitesOwnChoices(t *testing.T) {
	if plan := PlanFor(GeneratorHugo, "", "", ""); plan.Image != HugoImage || plan.Command != "hugo --minify" || plan.Output != "public" {
		t.Errorf("hugo: %+v", plan)
	}
	if plan := PlanFor(GeneratorHugo, "hugomods/hugo:exts-0.147.0", "hugo", "dist"); plan.Image != "hugomods/hugo:exts-0.147.0" || plan.Command != "hugo" || plan.Output != "dist" {
		t.Errorf("hugo with its own: %+v", plan)
	}
	if plan := PlanFor(GeneratorNone, "", "", ""); plan.Command != "" || plan.Image != "" {
		t.Errorf("none: %+v", plan)
	}
	// a command needs an image to run in
	if plan := PlanFor(GeneratorCustom, "", "make site", "out"); plan.Image != NodeImage {
		t.Errorf("custom: %+v", plan)
	}
}

func zipOf(t *testing.T, files map[string]string, links ...string) string {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for name := range files {
			if !yield(name) {
				return
			}
		}
	}) {
		writer, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = writer.Write([]byte(files[name]))
	}
	for _, link := range links {
		header := &zip.FileHeader{Name: link}
		header.SetMode(os.ModeSymlink | 0o777)
		writer, _ := archive.CreateHeader(header)
		_, _ = writer.Write([]byte("/etc/passwd"))
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "upload.zip")
	if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAnUploadIsUnpackedSafely(t *testing.T) {
	// a ZIP of a folder is unpacked from inside it
	dst := t.TempDir()
	path := zipOf(t, map[string]string{"site/index.html": "<h1>hi</h1>", "site/css/a.css": "a{}", "__MACOSX/site/._index.html": "x"})
	if err := Extract(path, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "index.html")); err != nil {
		t.Errorf("index.html: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "css", "a.css")); err != nil {
		t.Errorf("css/a.css: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "__MACOSX")); err == nil {
		t.Error("macOS metadata was unpacked")
	}

	// files at the top stay where they are
	dst = t.TempDir()
	if err := Extract(zipOf(t, map[string]string{"index.html": "x", "docs/a.html": "y"}), dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "docs", "a.html")); err != nil {
		t.Errorf("docs/a.html: %v", err)
	}

	// nothing leaves the directory, and no symlink comes in
	outside := t.TempDir()
	if err := Extract(zipOf(t, map[string]string{"index.html": "x", "../../" + filepath.Base(outside) + "/evil": "x"}), filepath.Join(t.TempDir(), "dst")); err != nil {
		// a path that leaves is cleaned into the directory, or refused
		t.Logf("refused: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("a file was written outside: %v", entries)
	}
	if err := Extract(zipOf(t, map[string]string{"index.html": "x"}, "link"), t.TempDir()); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("a symlink: %v", err)
	}
	if err := Extract(filepath.Join(t.TempDir(), "missing.zip"), t.TempDir()); err == nil {
		t.Error("a missing upload")
	}
}

func TestAReleaseIsTheBuiltSiteWithoutWhatIsNotServed(t *testing.T) {
	root := t.TempDir()
	if err := Prepare(root, "blog", 3); err != nil {
		t.Fatal(err)
	}
	src := SourceDir(root, "blog", 3)
	big := strings.Repeat("<p>hello world</p>\n", 200)
	write(t, filepath.Join(src, "dist", "index.html"), big)
	write(t, filepath.Join(src, "dist", "logo.png"), strings.Repeat("x", 5000))
	write(t, filepath.Join(src, "dist", ".git", "HEAD"), "ref: x")
	write(t, filepath.Join(src, "README.md"), "not served")
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "dist", "passwd")); err != nil {
		t.Fatal(err)
	}

	release, err := MakeRelease(root, "blog", 3, "")
	if err != nil {
		t.Fatal(err)
	}
	dir := ReleaseDir(root, "blog", 3)
	if release.Number != 3 || release.Files != 2 || release.SizeBytes != int64(len(big)+5000) {
		t.Errorf("release: %+v", release)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html.gz")); err != nil {
		t.Errorf("no gzip copy of index.html: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "logo.png.gz")); err == nil {
		t.Error("an image got a gzip copy")
	}
	for _, name := range []string{".git", "passwd", "README.md"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s is in the release", name)
		}
	}

	// an output directory the build did not make
	if err := Prepare(root, "blog", 4); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(SourceDir(root, "blog", 4), "index.html"), "x")
	var missing *OutputError
	if _, err := MakeRelease(root, "blog", 4, "public"); !errors.As(err, &missing) {
		t.Errorf("a missing output: %v", err)
	}
	if _, err := MakeRelease(root, "blog", 4, "../../etc"); err == nil {
		t.Error("an output outside of the source")
	}
	// without an output, the source itself
	if release, err := MakeRelease(root, "blog", 4, ""); err != nil || release.Files != 1 {
		t.Errorf("the source as it is: %+v %v", release, err)
	}
}

func TestSwitchingAReleaseIsAtOnceAndPruningKeepsTheServedOne(t *testing.T) {
	root := t.TempDir()
	for n := 1; n <= 4; n++ {
		write(t, filepath.Join(ReleaseDir(root, "blog", n), "index.html"), "release "+string(rune('0'+n)))
	}
	if Current(root, "blog") != 0 {
		t.Error("a release is served before any switch")
	}
	if err := Switch(root, "blog", 4); err != nil {
		t.Fatal(err)
	}
	served := func() string {
		data, _ := os.ReadFile(filepath.Join(CurrentLink(root, "blog"), "index.html"))
		return string(data)
	}
	if Current(root, "blog") != 4 || served() != "release 4" {
		t.Errorf("current %d: %s", Current(root, "blog"), served())
	}
	// a rollback
	if err := Switch(root, "blog", 2); err != nil || served() != "release 2" {
		t.Errorf("rollback: %v %s", err, served())
	}
	// a release that is gone leaves the served one
	if err := Switch(root, "blog", 9); !errors.Is(err, ErrNoRelease) || Current(root, "blog") != 2 {
		t.Errorf("a missing release: %v, current %d", err, Current(root, "blog"))
	}
	if entries, _ := filepath.Glob(filepath.Join(Dir(root, "blog"), ".current-*")); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}

	left, err := Prune(root, "blog", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(left, []int{4, 2}) || !slices.Equal(Releases(root, "blog"), []int{4, 2}) {
		t.Errorf("left %v, on disk %v", left, Releases(root, "blog"))
	}

	if err := Remove(root, "blog"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Dir(root, "blog")); !os.IsNotExist(err) {
		t.Errorf("the site is still there: %v", err)
	}
	if Remove(root, "..") == nil {
		t.Error("removing outside of the sites")
	}
}

func TestAnEmptySiteHasAPage(t *testing.T) {
	dir := t.TempDir()
	if err := Placeholder(dir, "blog<script>"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "index.html"))
	if !strings.Contains(string(data), "blog&lt;script&gt;") || strings.Contains(string(data), "<script>") {
		t.Errorf("the page: %s", data)
	}
}

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestAPushIsReadFromAnySignedWebhook(t *testing.T) {
	const secret = "s3cret"
	github := `{"ref":"refs/heads/main","after":"a3f9c21e","head_commit":{"id":"a3f9c21e","message":"Fix the RSS feed\n\nLonger text"}}`
	header := http.Header{}
	header.Set("X-GitHub-Event", "push")
	header.Set("X-Hub-Signature-256", "sha256="+sign(secret, github))
	push, err := ParsePush(header, []byte(github), secret)
	if err != nil || push != (Push{Branch: "main", Commit: "a3f9c21e", Message: "Fix the RSS feed"}) {
		t.Errorf("GitHub: %+v %v", push, err)
	}
	if _, err := ParsePush(header, []byte(github), "other"); !errors.Is(err, ErrSignature) {
		t.Errorf("a wrong secret: %v", err)
	}
	if _, err := ParsePush(http.Header{}, []byte(github), secret); !errors.Is(err, ErrSignature) {
		t.Errorf("no signature: %v", err)
	}
	ping := http.Header{}
	ping.Set("X-GitHub-Event", "ping")
	ping.Set("X-Hub-Signature-256", "sha256="+sign(secret, "{}"))
	if _, err := ParsePush(ping, []byte("{}"), secret); !errors.Is(err, ErrNotAPush) {
		t.Errorf("a ping: %v", err)
	}

	// Gitea signs without a prefix
	gitea := http.Header{}
	gitea.Set("X-Gitea-Event", "push")
	gitea.Set("X-Gitea-Signature", sign(secret, github))
	if push, err := ParsePush(gitea, []byte(github), secret); err != nil || push.Commit != "a3f9c21e" {
		t.Errorf("Gitea: %+v %v", push, err)
	}

	// GitLab sends the secret, and its commits
	gitlab := `{"ref":"refs/heads/docs","checkout_sha":"b2","commits":[{"id":"b1","message":"one"},{"id":"b2","message":"two"}]}`
	header = http.Header{}
	header.Set("X-Gitlab-Event", "Push Hook")
	header.Set("X-Gitlab-Token", secret)
	if push, err := ParsePush(header, []byte(gitlab), secret); err != nil || push != (Push{Branch: "docs", Commit: "b2", Message: "two"}) {
		t.Errorf("GitLab: %+v %v", push, err)
	}
	header.Set("X-Gitlab-Token", "wrong")
	if _, err := ParsePush(header, []byte(gitlab), secret); !errors.Is(err, ErrSignature) {
		t.Errorf("GitLab with a wrong token: %v", err)
	}

	// a tag, and a deleted branch, ask for nothing
	for _, body := range []string{`{"ref":"refs/tags/v1","after":"abc"}`, `{"ref":"refs/heads/main","after":"0000000000000000000000000000000000000000"}`} {
		header := http.Header{}
		header.Set("X-Hub-Signature-256", "sha256="+sign(secret, body))
		if _, err := ParsePush(header, []byte(body), secret); !errors.Is(err, ErrNotAPush) {
			t.Errorf("%s: %v", body, err)
		}
	}
	// no secret, no webhook
	if _, err := ParsePush(http.Header{}, []byte(github), ""); !errors.Is(err, ErrSignature) {
		t.Errorf("no secret: %v", err)
	}
}
