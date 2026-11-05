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

// The edge proxy is the official Caddy image. The panel generates configs for
// this Caddy version, so the image is pinned here, by the digest of its
// multi-platform index, and changes only together with the generator in
// internal/edge.
const (
	// EdgeImage is the repository of the edge proxy image.
	EdgeImage = "docker.io/library/caddy"
	// EdgeVersion is the Caddy version of the edge proxy.
	EdgeVersion = "2.11.7"
	// EdgeDigest is the digest of the image index of EdgeVersion.
	EdgeDigest = "sha256:36c24ad91a7ff7bc4c87fe22bfa8df3c4b18da0773d73fc12ee2c52b98c0bb7e"
)

// EdgeRef returns the edge proxy image, with its tag for people and its digest
// for Docker, which pulls by the digest.
func EdgeRef() string {
	return EdgeImage + ":" + EdgeVersion + "@" + EdgeDigest
}

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
