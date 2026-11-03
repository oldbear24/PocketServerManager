// Package version holds the build-time identity of the binaries and of the
// agent and updater images that belong to this release.
package version

// Set with -ldflags at build time. See Taskfile.yml and .goreleaser.yaml.
var (
	// Version is the semantic version of this build.
	Version = "dev"
	// Commit is the Git commit of this build.
	Commit = ""

	// AgentImage is the repository of the agent image of this release.
	AgentImage = "ghcr.io/oldbear24/pocketcloud-agent"
	// AgentDigest is the digest of the agent image of this release (sha256:…).
	AgentDigest = ""
	// UpdaterImage is the repository of the updater image of this release.
	UpdaterImage = "ghcr.io/oldbear24/pocketcloud-updater"
	// UpdaterDigest is the digest of the updater image of this release (sha256:…).
	UpdaterDigest = ""
)

// AgentRef returns the image reference used in install commands. It prefers the
// pinned digest and falls back to the version tag for development builds.
func AgentRef() string {
	if AgentDigest != "" {
		return AgentImage + "@" + AgentDigest
	}
	return AgentImage + ":" + tag()
}

// UpdaterRef returns the updater image reference; see AgentRef.
func UpdaterRef() string {
	if UpdaterDigest != "" {
		return UpdaterImage + "@" + UpdaterDigest
	}
	return UpdaterImage + ":" + tag()
}

// Pinned reports whether this release knows the digests of its images. Agents
// can only be updated by digest, so development builds cannot update them.
func Pinned() bool {
	return AgentDigest != "" && UpdaterDigest != ""
}

func tag() string {
	if Version == "dev" {
		return "latest"
	}
	return Version
}
