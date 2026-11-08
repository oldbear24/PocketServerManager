package sites

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestATemplatesFilesGoWhereTheSourceHasNone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wp-config.php"), []byte("<?php // own"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := WriteMissing(dir, map[string]string{"wp-config.php": "<?php // template", "conf/app.php": "<?php // new"})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"wp-config.php": "<?php // own", "conf/app.php": "<?php // new"} {
		if got, _ := os.ReadFile(filepath.Join(dir, name)); string(got) != want {
			t.Errorf("%s: %q", name, got)
		}
	}
	// a link of the source's own is kept, not followed
	outside := filepath.Join(t.TempDir(), "outside.php")
	_ = os.Symlink(outside, filepath.Join(dir, "linked.php"))
	if err := WriteMissing(dir, map[string]string{"linked.php": "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Error("a file was written through a link")
	}
	// nor does a directory that is a link lead out
	_ = os.Symlink(filepath.Dir(outside), filepath.Join(dir, "out"))
	if err := WriteMissing(dir, map[string]string{"out/x.php": "x"}); err == nil {
		t.Error("a file was written through a linked directory")
	}
	if err := WriteMissing(dir, map[string]string{"../x.php": "x"}); err == nil {
		t.Error("a file outside the source was taken")
	}
}

func TestAReleaseGetsItsTemplatesLinksAndEnvironment(t *testing.T) {
	root := t.TempDir()
	release := ReleaseDir(root, "shop", 3)
	for _, dir := range []string{filepath.Join(release, "public", "storage"), filepath.Join(release, "storage", "app", "public")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(release, ".env"), []byte("APP_ENV=local\n"), 0o644)

	links, err := ParseLinks("public/storage=storage/app/public, ")
	if err != nil || len(links) != 1 {
		t.Fatalf("%v %v", links, err)
	}
	// what the build left where the link goes, as artisan's own link, goes
	if err := MakeLinks(root, "shop", 3, links); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(release, "public", "storage")); err != nil || target != "../storage/app/public" {
		t.Errorf("link: %q %v", target, err)
	}
	if err := WriteEnvFile(root, "shop", 3, ".env", "APP_ENV=\"production\"\n"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(release, ".env"))
	if got, _ := os.ReadFile(filepath.Join(release, ".env")); err != nil || string(got) != "APP_ENV=\"production\"\n" || info.Mode().Perm() != 0o600 {
		t.Errorf(".env: %q %v %v", got, info.Mode(), err)
	}

	for _, bad := range []string{"public/storage", "a=../b", "/etc=x", "a=b=c"} {
		if _, err := ParseLinks(bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	// a link cannot be made through a directory that is a link
	_ = os.Symlink(t.TempDir(), filepath.Join(release, "linked"))
	if err := MakeLinks(root, "shop", 3, map[string]string{"linked/storage": "storage"}); err == nil {
		t.Error("a link was made through a link")
	}
	if err := WriteEnvFile(root, "shop", 3, "linked/.env", "x"); err == nil {
		t.Error("an environment file was written through a link")
	}
}

func TestACacheIsEmptiedOnceANewReleaseIsLive(t *testing.T) {
	root := t.TempDir()
	if err := PreparePHP(root, "news"); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(SharedDir(root, "news"), "temp")
	for name, content := range map[string]string{"cache/Nette.Configurator/Container_1.php": "<?php", ".gitignore": "*\n!.gitignore\n", "sessions/a": "x"} {
		path := filepath.Join(temp, name)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = os.WriteFile(path, []byte(content), 0o644)
	}
	// what a link in the cache leads to stays
	elsewhere := t.TempDir()
	_ = os.WriteFile(filepath.Join(elsewhere, "keep"), []byte("x"), 0o644)
	_ = os.Symlink(elsewhere, filepath.Join(temp, "link"))
	logs := filepath.Join(SharedDir(root, "news"), "log")
	_ = os.MkdirAll(logs, 0o755)
	_ = os.WriteFile(filepath.Join(logs, "exception.log"), []byte("x"), 0o644)

	if err := ClearShared(root, "news", []string{"temp", "missing"}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(temp)
	if len(entries) != 1 || entries[0].Name() != ".gitignore" {
		t.Errorf("temp keeps %v", entries)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "keep")); err != nil {
		t.Error("a link was followed")
	}
	if _, err := os.Stat(filepath.Join(logs, "exception.log")); err != nil {
		t.Error("another shared path was emptied")
	}
	if err := ClearShared(root, "news", []string{"../news"}); err == nil {
		t.Error("a path outside shared/ was taken")
	}
}

func TestAnUpdateStartsFromACopyOfTheLiveRelease(t *testing.T) {
	root := t.TempDir()
	release := ReleaseDir(root, "blog", 2)
	for name, content := range map[string]string{"index.php": "<?php // 2", "wp-content/plugins/a/a.php": "<?php"} {
		path := filepath.Join(release, name)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = os.WriteFile(path, []byte(content), 0o644)
	}
	// the shared paths' links are made anew by the release, not copied
	_ = os.Symlink("../../shared/wp-content/uploads", filepath.Join(release, "wp-content", "uploads"))
	if err := Prepare(root, "blog", 3); err != nil {
		t.Fatal(err)
	}
	if err := CopyRelease(root, "blog", 2, 3); err != nil {
		t.Fatal(err)
	}
	src := SourceDir(root, "blog", 3)
	if got, _ := os.ReadFile(filepath.Join(src, "index.php")); string(got) != "<?php // 2" {
		t.Errorf("index.php: %q", got)
	}
	if _, err := os.Stat(filepath.Join(src, "wp-content", "plugins", "a", "a.php")); err != nil {
		t.Error(err)
	}
	if _, err := os.Lstat(filepath.Join(src, "wp-content", "uploads")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the link was copied: %v", err)
	}
	// a change to the copy leaves the release as it was
	_ = os.WriteFile(filepath.Join(src, "index.php"), []byte("<?php // 3"), 0o644)
	if got, _ := os.ReadFile(filepath.Join(release, "index.php")); string(got) != "<?php // 2" {
		t.Errorf("the release changed: %q", got)
	}
	if err := CopyRelease(root, "blog", 7, 3); !errors.Is(err, ErrNoRelease) {
		t.Errorf("a missing release: %v", err)
	}
}
