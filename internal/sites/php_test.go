package sites

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSharedPathsLiveOutsideTheReleases(t *testing.T) {
	root := t.TempDir()
	put := func(name, content string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			return "missing: " + err.Error()
		}
		return string(data)
	}
	if err := PreparePHP(root, "shop"); err != nil {
		t.Fatal(err)
	}
	paths := []string{"storage", "wp-content/uploads", "cache"}

	// the first release seeds shared/ with its own copy
	put("shop/releases/1/storage/logs/.gitignore", "*")
	put("shop/releases/1/wp-content/uploads/a.jpg", "first")
	put("shop/releases/1/index.php", "<?php")
	if err := LinkShared(root, "shop", 1, paths); err != nil {
		t.Fatal(err)
	}
	if got := read("shop/shared/wp-content/uploads/a.jpg"); got != "first" {
		t.Errorf("seeded: %q", got)
	}
	if got := read("shop/releases/1/wp-content/uploads/a.jpg"); got != "first" {
		t.Errorf("through the link: %q", got)
	}
	if target, err := os.Readlink(filepath.Join(root, "shop/releases/1/wp-content/uploads")); err != nil || target != "../../../shared/wp-content/uploads" {
		t.Errorf("link: %q %v", target, err)
	}
	if info, err := os.Stat(filepath.Join(root, "shop/shared/cache")); err != nil || !info.IsDir() {
		t.Errorf("a path neither has: %v", err)
	}

	// what the site wrote stays; the next release's own copy does not replace it
	put("shop/shared/wp-content/uploads/b.jpg", "uploaded")
	put("shop/releases/2/wp-content/uploads/a.jpg", "second")
	if err := LinkShared(root, "shop", 2, paths); err != nil {
		t.Fatal(err)
	}
	if got := read("shop/releases/2/wp-content/uploads/a.jpg"); got != "first" {
		t.Errorf("the release's copy replaced shared/: %q", got)
	}
	if got := read("shop/releases/2/wp-content/uploads/b.jpg"); got != "uploaded" {
		t.Errorf("what the site wrote: %q", got)
	}
}

func TestSharedPathsStayInsideTheRelease(t *testing.T) {
	for _, path := range []string{"", ".", "..", "../other", "/etc", "a/../../b", "a,b", "storage/", "a\\b"} {
		if path == "storage/" {
			if err := ValidSharedPath(path); err != nil {
				t.Errorf("%q: %v", path, err)
			}
			continue
		}
		if err := ValidSharedPath(path); err == nil {
			t.Errorf("%q was taken", path)
		}
	}

	// a release whose directory on the way is a symlink cannot place a link
	// through it, wherever it leads
	root := t.TempDir()
	if err := PreparePHP(root, "shop"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "shop/releases/1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/tmp", filepath.Join(root, "shop/releases/1/wp-content")); err != nil {
		t.Fatal(err)
	}
	if err := LinkShared(root, "shop", 1, []string{"wp-content/uploads"}); err == nil {
		t.Error("linked through a symlink")
	}
}

func TestAPHPReleaseIsTheWholeSourceWithoutGzipCopies(t *testing.T) {
	root := t.TempDir()
	if err := PreparePHP(root, "shop"); err != nil {
		t.Fatal(err)
	}
	src := SourceDir(root, "shop", 1)
	if err := os.MkdirAll(filepath.Join(src, "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 4096)
	for i := range big {
		big[i] = 'a'
	}
	for name, data := range map[string][]byte{"public/index.php": []byte("<?php"), "vendor/autoload.php": []byte("<?php"), "public/app.js": big, ".env": []byte("X=1")} {
		path := filepath.Join(src, name)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	release, err := MakePHPRelease(root, "shop", 1)
	if err != nil {
		t.Fatal(err)
	}
	if release.Files != 4 {
		t.Errorf("files: %d", release.Files)
	}
	dir := ReleaseDir(root, "shop", 1)
	if _, err := os.Stat(filepath.Join(dir, "vendor/autoload.php")); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "public/app.js.gz")); err == nil {
		t.Error("a gzip copy in a PHP release")
	}
}

func TestAPHPSiteIsBuiltWithComposerOrItsOwnCommand(t *testing.T) {
	if plan := PHPPlan("php:8.4", ""); plan.Command != ComposerBuild || plan.Image != "php:8.4" || plan.Output != "." {
		t.Errorf("%+v", plan)
	}
	if plan := PHPPlan("php:8.4", "composer install && npm run build"); plan.Command != "composer install && npm run build" {
		t.Errorf("%+v", plan)
	}
}
