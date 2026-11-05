package sites

import (
	"archive/zip"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Limits of an upload, unpacked.
const (
	MaxUploadFiles = 100_000
	MaxUploadBytes = 2 << 30
)

// within joins a relative path to dir and refuses one that leaves it.
func within(dir, name string) (string, error) {
	clean := filepath.Clean("/" + filepath.ToSlash(name))
	path := filepath.Join(dir, clean)
	if path != dir && !strings.HasPrefix(path, dir+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside of the site", name)
	}
	return path, nil
}

// Prepare makes the directories of a deployment, with an empty source
// directory. An upload already in the build directory stays.
func Prepare(root, site string, n int) error {
	if err := os.RemoveAll(SourceDir(root, site, n)); err != nil {
		return err
	}
	for _, dir := range []string{filepath.Join(Dir(root, site), "releases"), SourceDir(root, site, n)} {
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // served by the edge
			return err
		}
	}
	return nil
}

// Clean removes what a deployment left in its build directory.
func Clean(root, site string, n int) error {
	return os.RemoveAll(BuildDir(root, site, n))
}

// Extract unpacks an upload into dst. A ZIP of a single folder is unpacked
// from inside that folder, as one of a site's files would be. Symlinks and
// paths that leave dst are refused.
func Extract(zipPath, dst string) error {
	archive, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("the upload is not a ZIP file: %w", err)
	}
	defer func() { _ = archive.Close() }()

	files := []*zip.File{}
	for _, file := range archive.File {
		name := strings.TrimPrefix(filepath.ToSlash(file.Name), "./")
		if name == "" || strings.HasPrefix(name, "__MACOSX/") || strings.HasSuffix(name, "/.DS_Store") || name == ".DS_Store" {
			continue
		}
		files = append(files, file)
	}
	if len(files) > MaxUploadFiles {
		return fmt.Errorf("the upload has %d files, more than %d", len(files), MaxUploadFiles)
	}
	prefix := commonFolder(files)

	var total uint64
	for _, file := range files {
		name := strings.TrimPrefix(strings.TrimPrefix(filepath.ToSlash(file.Name), "./"), prefix)
		if name == "" {
			continue
		}
		mode := file.Mode()
		if mode&fs.ModeSymlink != 0 {
			return fmt.Errorf("the upload has a symlink, %s, which a site cannot have", file.Name)
		}
		path, err := within(dst, name)
		if err != nil {
			return err
		}
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0o755); err != nil { //nolint:gosec // served by the edge
				return err
			}
			continue
		}
		total += file.UncompressedSize64
		if total > MaxUploadBytes {
			return fmt.Errorf("the upload unpacks to more than %d GiB", MaxUploadBytes>>30)
		}
		if err := extractFile(file, path); err != nil {
			return fmt.Errorf("%s: %w", file.Name, err)
		}
	}
	return nil
}

// commonFolder is the folder every file of a ZIP is in, with its slash, or "".
func commonFolder(files []*zip.File) string {
	prefix := ""
	for _, file := range files {
		name := strings.TrimPrefix(filepath.ToSlash(file.Name), "./")
		first, _, nested := strings.Cut(name, "/")
		if !nested {
			// a file at the top
			return ""
		}
		if prefix == "" {
			prefix = first
		} else if first != prefix {
			return ""
		}
	}
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}

func extractFile(file *zip.File, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // served by the edge
		return err
	}
	reader, err := file.Open()
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644) //nolint:gosec // inside dst, checked
	if err != nil {
		return err
	}
	// the sizes the ZIP states are added up above, and archive/zip fails a
	// file that unpacks to more than its own
	if _, err := io.Copy(out, io.LimitReader(reader, MaxUploadBytes)); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// Placeholder is the page of a site that has no release of its own yet.
func Placeholder(dst, site string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil { //nolint:gosec // served by the edge
		return err
	}
	name := html.EscapeString(site)
	page := `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + name + `</title>
<style>
body { margin: 0; min-height: 100vh; display: grid; place-items: center; font: 16px/1.5 system-ui, sans-serif; background: #0f1115; color: #e6e8eb; }
main { text-align: center; padding: 24px; }
h1 { font-size: 24px; margin: 0 0 8px; }
p { margin: 0; color: #9aa3ad; }
</style>
</head>
<body>
<main>
<h1>` + name + `</h1>
<p>This site has no content yet.</p>
</main>
</body>
</html>
`
	return os.WriteFile(filepath.Join(dst, "index.html"), []byte(page), 0o644) //nolint:gosec // served by the edge
}

// Release is what a deployment put on the server.
type Release struct {
	Number    int   `json:"number"`
	SizeBytes int64 `json:"size_bytes"`
	Files     int   `json:"files"`
}

// MakeRelease moves the built site from a deployment's source into release n,
// without what a site never serves (.git), and adds a gzip copy of the files
// that compress, which the edge sends to browsers that take it.
func MakeRelease(root, site string, n int, output string) (Release, error) {
	src := SourceDir(root, site, n)
	dir, err := FindOutput(src, output)
	if err != nil {
		return Release{}, err
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		if entries, _ := os.ReadDir(dir); len(entries) == 0 {
			return Release{}, errors.New("the build left an empty site")
		}
	}
	release := ReleaseDir(root, site, n)
	if err := os.RemoveAll(release); err != nil {
		return Release{}, err
	}
	// the same volume, so this is a rename and takes no time
	if err := os.Rename(dir, release); err != nil {
		return Release{}, err
	}
	for _, name := range []string{".git", ".pocketcloud"} {
		if err := os.RemoveAll(filepath.Join(release, name)); err != nil {
			return Release{}, err
		}
	}
	if err := os.Chmod(release, 0o755); err != nil { //nolint:gosec // served by the edge
		return Release{}, err
	}

	result := Release{Number: n}
	err = filepath.WalkDir(release, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if !entry.Type().IsRegular() {
			// a symlink from the build could point anywhere on the volume
			return os.Remove(path)
		}
		if isPrecompressed(path) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		result.Files++
		result.SizeBytes += info.Size()
		return precompress(path, info.Size())
	})
	return result, err
}

func isPrecompressed(path string) bool {
	for _, extension := range []string{".gz", ".br", ".zst"} {
		if strings.HasSuffix(path, extension) {
			if _, err := os.Stat(strings.TrimSuffix(path, extension)); err == nil {
				return true
			}
		}
	}
	return false
}

// compressible are the types worth a gzip copy.
var compressible = []string{".html", ".htm", ".css", ".js", ".mjs", ".json", ".svg", ".xml", ".txt", ".map", ".wasm", ".ico", ".md", ".csv", ".webmanifest"}

func precompress(path string, size int64) error {
	if size < 1024 || !slices.Contains(compressible, strings.ToLower(filepath.Ext(path))) {
		return nil
	}
	if _, err := os.Stat(path + ".gz"); err == nil {
		return nil
	}
	in, err := os.Open(path) //nolint:gosec // a file of the release
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(path+".gz", os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644) //nolint:gosec // served by the edge
	if err != nil {
		return err
	}
	writer, _ := gzip.NewWriterLevel(out, gzip.BestCompression)
	if _, err := io.Copy(writer, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		_ = out.Close()
		return err
	}
	info, err := out.Stat()
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// not worth sending compressed
	if info.Size() >= size*9/10 {
		return os.Remove(path + ".gz")
	}
	return nil
}

// Switch makes release n the one the edge serves, at once: a new symlink
// renamed over the old one.
func Switch(root, site string, n int) error {
	release := ReleaseDir(root, site, n)
	if info, err := os.Stat(release); err != nil || !info.IsDir() {
		return fmt.Errorf("release %d: %w", n, ErrNoRelease)
	}
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	link := filepath.Join(Dir(root, site), ".current-"+hex.EncodeToString(suffix))
	if err := os.Symlink(filepath.Join("releases", strconv.Itoa(n)), link); err != nil {
		return err
	}
	if err := os.Rename(link, CurrentLink(root, site)); err != nil {
		_ = os.Remove(link)
		return err
	}
	return nil
}

// Current is the release the edge serves, or 0.
func Current(root, site string) int {
	target, err := os.Readlink(CurrentLink(root, site))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(filepath.Base(target))
	if err != nil {
		return 0
	}
	return n
}

// Releases are the releases on the server, newest first.
func Releases(root, site string) []int {
	entries, _ := os.ReadDir(filepath.Join(Dir(root, site), "releases"))
	numbers := []int{}
	for _, entry := range entries {
		if n, err := strconv.Atoi(entry.Name()); err == nil && entry.IsDir() {
			numbers = append(numbers, n)
		}
	}
	slices.SortFunc(numbers, func(a, b int) int { return b - a })
	return numbers
}

// Prune keeps the newest keep releases and the one the edge serves, and
// removes the rest. It returns the releases that are left.
func Prune(root, site string, keep int) ([]int, error) {
	current := Current(root, site)
	left := []int{}
	for i, n := range Releases(root, site) {
		if i < keep || n == current {
			left = append(left, n)
			continue
		}
		if err := os.RemoveAll(ReleaseDir(root, site, n)); err != nil {
			return nil, err
		}
	}
	return left, nil
}

// Remove removes a site's directory and everything in it.
func Remove(root, site string) error {
	if err := ValidName(site); err != nil {
		return err
	}
	return os.RemoveAll(Dir(root, site))
}
