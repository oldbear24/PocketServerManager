// Package sites is what the panel and the agent share about static sites
// (#21): where their releases live on the sites volume, how a source is built,
// the operations on a site's directory, and the push webhooks of Git servers.
//
// A site's directory on the volume:
//
//	<site>/releases/<n>/   one directory per deployment that succeeded
//	<site>/current         a symlink to releases/<n>, what the edge serves
//	<site>/.build/<n>/     a deployment under way: its source and its upload
//
// The edge serves <site>/current. Switching it is a rename, so a release
// becomes live, or an older one again, at once and never half.
package sites

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
)

const (
	// Volume is the Docker volume that holds every site's releases. The edge
	// mounts it read-only; the agent writes it through job containers.
	Volume = "pocketcloud-sites"
	// Root is where the volume is mounted, in the edge and in the jobs.
	Root = "/srv/sites"
)

// Sources of a site.
const (
	SourceGit    = "git"
	SourceUpload = "upload"
	SourceEmpty  = "empty"
)

var siteName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// ValidName checks a site's name, which names its directory and its jobs.
func ValidName(name string) error {
	if !siteName.MatchString(name) {
		return fmt.Errorf("%q is not a site name: lowercase letters, digits and dashes, at most 40, not starting or ending with a dash", name)
	}
	return nil
}

// Dir is a site's directory under root.
func Dir(root, site string) string { return filepath.Join(root, site) }

// ReleaseDir is the directory of release n.
func ReleaseDir(root, site string, n int) string {
	return filepath.Join(root, site, "releases", strconv.Itoa(n))
}

// BuildDir is the directory of a deployment under way.
func BuildDir(root, site string, n int) string {
	return filepath.Join(root, site, ".build", strconv.Itoa(n))
}

// SourceDir is where a deployment's source is checked out or unpacked.
func SourceDir(root, site string, n int) string {
	return filepath.Join(BuildDir(root, site, n), "src")
}

// CurrentLink is the symlink the edge serves.
func CurrentLink(root, site string) string { return filepath.Join(root, site, "current") }

// ServedRoot is the directory the edge serves a site from.
func ServedRoot(site string) string { return CurrentLink(Root, site) }

// UploadName is the name an upload has in its build directory.
const UploadName = "upload.zip"

// ErrNoRelease is returned for a release that is not on the volume.
var ErrNoRelease = errors.New("the release is not on the server")
