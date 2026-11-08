package sites

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// What a PHP application's template (#52) asks of a site's releases: files a
// source lacks, links PHP of the site cannot make, caches emptied once a new
// release is live, an environment file, and a copy of the live release for an
// update. The site tool does each as root with the whole sites volume, so each
// works through an os.Root of the site's directory: a link the site's code
// left in its shared paths, or in a release it copies, leads nowhere else.

// TemplateMarker marks a template's file in a source: one that still carries
// it in its first KiB is the template's, written anew, as a copy of a release
// for an update carries the old one forward.
const TemplateMarker = "@pocketcloud-template"

// WriteMissing puts files into a source that does not have them, such as a
// wp-config.php that reads the site's environment: a source with its own keeps
// it, and one with an earlier one of the template's gets the template's own.
// They belong to whoever runs this; the site tool gives a PHP site's source to
// www-data after.
func WriteMissing(dir string, files map[string]string) error {
	if len(files) == 0 {
		return nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for name, content := range files {
		if err := ValidSharedPath(name); err != nil {
			return err
		}
		name = filepath.Clean(name)
		if info, err := root.Lstat(name); err == nil {
			if !info.Mode().IsRegular() || !templates(root, name) {
				continue
			}
			if err := root.Remove(name); err != nil {
				return err
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil { //nolint:gosec // served by the edge
			return err
		}
		if err := root.WriteFile(name, []byte(content), 0o644); err != nil { //nolint:gosec // served by the edge
			return err
		}
	}
	return nil
}

// templates says whether a file of a source is a template's, by its marker.
func templates(root *os.Root, name string) bool {
	file, err := root.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	head := make([]byte, 1024)
	n, _ := io.ReadFull(file, head)
	return strings.Contains(string(head[:n]), TemplateMarker)
}

// ParseLinks reads links as the site tool gets them: path=target, separated by
// commas, both inside the release.
func ParseLinks(specs string) (map[string]string, error) {
	links := map[string]string{}
	for _, spec := range strings.Split(specs, ",") {
		if spec = strings.TrimSpace(spec); spec == "" {
			continue
		}
		path, target, ok := strings.Cut(spec, "=")
		if !ok || strings.Contains(target, "=") {
			return nil, fmt.Errorf("%q is not a link: path=target", spec)
		}
		for _, p := range []string{path, target} {
			if err := ValidSharedPath(p); err != nil {
				return nil, err
			}
		}
		links[filepath.Clean(path)] = filepath.Clean(target)
	}
	return links, nil
}

// MakeLinks makes a release's links: at path, a relative link to target, both
// relative to the release, as `php artisan storage:link` makes public/storage.
// What the build left at path goes. Target need not be there yet, as one in a
// shared path appears when the site writes it.
func MakeLinks(root, site string, n int, links map[string]string) error {
	if len(links) == 0 {
		return nil
	}
	dir, err := os.OpenRoot(ReleaseDir(root, site, n))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	for path, target := range links {
		if err := insideReal(ReleaseDir(root, site, n), filepath.Dir(filepath.Join(ReleaseDir(root, site, n), path))); err != nil {
			return err
		}
		if err := dir.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // served by the edge
			return err
		}
		if err := dir.RemoveAll(path); err != nil {
			return err
		}
		relative, err := filepath.Rel(filepath.Dir(path), target)
		if err != nil {
			return err
		}
		if err := dir.Symlink(relative, path); err != nil {
			return err
		}
		if os.Geteuid() == 0 {
			if err := dir.Lchown(path, PHPUser, PHPGroup); err != nil {
				return err
			}
		}
	}
	return nil
}

// ClearShared empties shared paths, such as Nette's temp/, keeping the
// directories: once a new release is live, what the old code cached there
// must not be used by the new. A path that is not a directory is left alone.
func ClearShared(root, site string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	shared, err := os.OpenRoot(SharedDir(root, site))
	if err != nil {
		return err
	}
	defer func() { _ = shared.Close() }()
	var problems []error
	for _, path := range paths {
		if err := ValidSharedPath(path); err != nil {
			return err
		}
		path = filepath.Clean(path)
		info, err := shared.Lstat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		entries, err := fs.ReadDir(shared.FS(), filepath.ToSlash(path))
		if err != nil {
			problems = append(problems, err)
			continue
		}
		for _, entry := range entries {
			// a .gitignore keeps the directory in a repository
			if entry.Name() == ".gitignore" || entry.Name() == ".htaccess" {
				continue
			}
			if err := shared.RemoveAll(filepath.Join(path, entry.Name())); err != nil {
				problems = append(problems, err)
			}
		}
	}
	return errors.Join(problems...)
}

// WriteEnvFile writes a release's environment file, such as Laravel's .env,
// replacing the source's: readable by www-data alone, as it holds the site's
// secrets.
func WriteEnvFile(root, site string, n int, name, content string) error {
	if err := ValidSharedPath(name); err != nil {
		return err
	}
	dir, err := os.OpenRoot(ReleaseDir(root, site, n))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	name = filepath.Clean(name)
	if err := insideReal(ReleaseDir(root, site, n), filepath.Dir(filepath.Join(ReleaseDir(root, site, n), name))); err != nil {
		return err
	}
	if err := dir.RemoveAll(name); err != nil {
		return err
	}
	file, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(file, content); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		return dir.Lchown(name, PHPUser, PHPGroup)
	}
	return nil
}

// CopyRelease copies release from into deployment n's source, as the source
// of an update: its files and directories, not its links, which the release
// makes anew, nor what it keeps in shared/.
func CopyRelease(root, site string, from, n int) error {
	if from < 1 {
		return errors.New("no release to copy")
	}
	release, err := os.OpenRoot(ReleaseDir(root, site, from))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNoRelease
		}
		return err
	}
	defer func() { _ = release.Close() }()
	src := SourceDir(root, site, n)
	if err := os.MkdirAll(src, 0o755); err != nil { //nolint:gosec // served by the edge
		return err
	}
	dst, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer func() { _ = dst.Close() }()
	return fs.WalkDir(release.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == "." {
			return err
		}
		switch {
		case entry.IsDir():
			return dst.Mkdir(path, 0o755) //nolint:gosec // served by the edge
		case entry.Type().IsRegular():
			return copyIn(release, dst, path)
		}
		// links, the shared paths' among them
		return nil
	})
}

func copyIn(from, to *os.Root, path string) error {
	in, err := from.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := to.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
