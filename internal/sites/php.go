package sites

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PHP sites (#50). Their code lives on the sites volume like a static site's,
// where the edge serves the static files itself, and a PHP-FPM container of
// their own runs the scripts. The layout adds one directory:
//
//	<site>/shared/   what survives every release, such as uploads or storage,
//	                 linked into each release; the only one PHP-FPM may write
//	                 in releases mode
//
// A PHP site's files belong to www-data of the runtime images (Debian, uid
// 33), which runs the builds, the release hook and PHP-FPM.

// GeneratorPHP builds a PHP site in its runtime image: composer install, when
// it has a composer.json, or the site's own command.
const GeneratorPHP = "php"

// ComposerBuild installs a PHP site's dependencies for production.
const ComposerBuild = "if [ -f composer.json ]; then composer install --no-dev --optimize-autoloader --no-interaction --no-progress --prefer-dist; else echo 'No composer.json: nothing to install.'; fi"

// PHPUser and PHPGroup are www-data of the runtime images.
const (
	PHPUser  = 33
	PHPGroup = 33
)

// FastCGIPort is where a site's PHP-FPM container listens.
const FastCGIPort = 9000

// PHPContainer names a site's PHP-FPM container, and its alias on the site's
// network, which the edge dials.
func PHPContainer(site string) string { return site + "-php" }

// Network names a PHP site's own network: its PHP-FPM container and the edge
// are on it, and nothing else, so that no other container reaches the
// FastCGI port, through which any PHP setting can be passed.
func Network(site string) string { return "pcsite-" + site }

// FastCGIUpstream is what the edge dials for a PHP site's scripts.
func FastCGIUpstream(site string) string {
	return PHPContainer(site) + ":" + strconv.Itoa(FastCGIPort)
}

// SharedDir is where a site keeps what survives its releases.
func SharedDir(root, site string) string {
	return filepath.Join(Dir(root, site), "shared")
}

// PHPPlan is how a PHP site is built: in its runtime image, with composer or
// its own command, and released as a whole.
func PHPPlan(image, command string) Plan {
	if command == "" {
		command = ComposerBuild
	}
	return Plan{Generator: GeneratorPHP, Image: image, Command: command, Output: "."}
}

// PreparePHP makes a PHP site's directories, which PHP-FPM mounts: the site's
// own and shared/, which www-data owns.
func PreparePHP(root, site string) error {
	for _, dir := range []string{filepath.Join(Dir(root, site), "releases"), SharedDir(root, site)} {
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // served by the edge
			return err
		}
	}
	return Chown(SharedDir(root, site))
}

// ValidSharedPath checks a shared path: a path inside the release, such as
// storage or wp-content/uploads, without .. or a leading /.
func ValidSharedPath(path string) error {
	clean := filepath.Clean(path)
	if path == "" || filepath.IsAbs(path) || clean != strings.TrimSuffix(path, "/") || clean == "." ||
		clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsAny(path, ",\\\n\r\x00") {
		return fmt.Errorf("%q is not a path inside the release, such as storage or wp-content/uploads", path)
	}
	return nil
}

// LinkShared puts a release's shared paths into shared/ and links them into
// the release: a path the release has and shared/ has not yet moves there, as
// on a site's first release, and the release's own copy of one shared/ has
// already is dropped. A path neither has becomes an empty directory. The
// links are relative, so that the edge and PHP-FPM, which mount the site in
// different places, both follow them.
func LinkShared(root, site string, n int, paths []string) error {
	release, shared := ReleaseDir(root, site, n), SharedDir(root, site)
	for _, path := range paths {
		if err := ValidSharedPath(path); err != nil {
			return err
		}
		path = filepath.Clean(path)
		inRelease, inShared := filepath.Join(release, path), filepath.Join(shared, path)
		if err := insideReal(release, filepath.Dir(inRelease)); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(inShared), 0o755); err != nil { //nolint:gosec // served by the edge
			return err
		}
		_, releaseErr := os.Lstat(inRelease)
		_, sharedErr := os.Lstat(inShared)
		switch {
		case releaseErr == nil && errors.Is(sharedErr, fs.ErrNotExist):
			if err := os.Rename(inRelease, inShared); err != nil {
				return err
			}
		case releaseErr == nil:
			if err := os.RemoveAll(inRelease); err != nil {
				return err
			}
		case errors.Is(sharedErr, fs.ErrNotExist):
			if err := os.MkdirAll(inShared, 0o755); err != nil { //nolint:gosec // served by the edge
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(inRelease), 0o755); err != nil { //nolint:gosec // served by the edge
			return err
		}
		target, err := filepath.Rel(filepath.Dir(inRelease), inShared)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, inRelease); err != nil {
			return err
		}
	}
	return Chown(shared)
}

// insideReal checks that dir, once its symlinks are followed, is still inside
// base: a release has none, but a path through one could lead out of it.
func insideReal(base, dir string) error {
	for dir != base && strings.HasPrefix(dir, base+string(filepath.Separator)) {
		if info, err := os.Lstat(dir); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", dir)
		}
		dir = filepath.Dir(dir)
	}
	return nil
}

// Chown gives a tree to www-data of the runtime images, without following
// symlinks, and without leaving the tree: shared/ is written by the site's
// PHP while this runs, and a directory it swaps for a symlink must not lead
// the change elsewhere. The site tool runs as root in its job container; run
// by anyone else, as in tests, it leaves the owners as they are.
func Chown(dir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return fs.WalkDir(root.FS(), ".", func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return root.Lchown(path, PHPUser, PHPGroup)
	})
}
