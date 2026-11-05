package sites

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Generators a site can be built with. GeneratorAuto looks at the source.
const (
	GeneratorAuto   = "auto"
	GeneratorNone   = "none"
	GeneratorHugo   = "hugo"
	GeneratorMkDocs = "mkdocs"
	GeneratorAstro  = "astro"
	GeneratorNPM    = "npm"
	GeneratorCustom = "custom"
)

// Images a generator builds in, pinned so that a build does the same thing
// tomorrow. A site can name its own image and command instead.
const (
	HugoImage   = "ghcr.io/gohugoio/hugo:v0.152.2"
	MkDocsImage = "squidfunk/mkdocs-material:9.6.14"
	NodeImage   = "node:22.20.0-alpine3.22"
)

// Plan is how a source is built: in Image, with Command, leaving the site in
// Output. A plan without a command copies the source as it is.
type Plan struct {
	Generator string `json:"generator"`
	Image     string `json:"image,omitempty"`
	Command   string `json:"command,omitempty"`
	// Output is the directory of the built site, relative to the source; empty
	// looks for the usual ones.
	Output string `json:"output,omitempty"`
}

// npmBuild installs the dependencies as the lock file has them, if there is
// one, and runs the build script.
const npmBuild = "if [ -f package-lock.json ]; then npm ci; else npm install; fi && npm run build"

// PlanFor is the plan of a generator, with the site's own image, command and
// output where it has them.
func PlanFor(generator, image, command, output string) Plan {
	plan := Plan{Generator: generator}
	switch generator {
	case GeneratorHugo:
		plan = Plan{Generator: generator, Image: HugoImage, Command: "hugo --minify", Output: "public"}
	case GeneratorMkDocs:
		plan = Plan{Generator: generator, Image: MkDocsImage, Command: "mkdocs build", Output: "site"}
	case GeneratorAstro:
		plan = Plan{Generator: generator, Image: NodeImage, Command: npmBuild, Output: "dist"}
	case GeneratorNPM:
		plan = Plan{Generator: generator, Image: NodeImage, Command: npmBuild}
	}
	if image != "" {
		plan.Image = image
	}
	if command != "" {
		plan.Command = command
	}
	if output != "" {
		plan.Output = output
	}
	if plan.Command != "" && plan.Image == "" {
		plan.Image = NodeImage
	}
	return plan
}

// Detect says which generator a source is for, by its files.
func Detect(src string) string {
	exists := func(names ...string) bool {
		for _, name := range names {
			if _, err := os.Stat(filepath.Join(src, name)); err == nil {
				return true
			}
		}
		return false
	}
	switch {
	case exists("hugo.toml", "hugo.yaml", "hugo.yml", "hugo.json"),
		exists("config.toml", "config.yaml", "config.yml", "config.json", filepath.Join("config", "_default")) &&
			exists("content", "layouts", "themes", "archetypes"):
		return GeneratorHugo
	case exists("mkdocs.yml", "mkdocs.yaml"):
		return GeneratorMkDocs
	case exists("astro.config.mjs", "astro.config.ts", "astro.config.js", "astro.config.mts"):
		return GeneratorAstro
	case hasBuildScript(filepath.Join(src, "package.json")):
		return GeneratorNPM
	}
	return GeneratorNone
}

func hasBuildScript(path string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // a file of the site's own source
	if err != nil {
		return false
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	return json.Unmarshal(data, &manifest) == nil && manifest.Scripts["build"] != ""
}

// outputCandidates are where builds usually leave a site, in order.
var outputCandidates = []string{"dist", "build", "out", "public", "_site", "site"}

// FindOutput is the directory of the built site: the plan's, or the first of
// the usual ones with an index.html, or, for a site that was not built, the
// source itself. It is a real directory inside the source: a path through a
// symlink, which a repository or a build may have put there, could reach
// another site's files on the volume.
func FindOutput(src, output string, built bool) (string, error) {
	if output != "" {
		dir, err := within(src, output)
		if err != nil {
			return "", err
		}
		return realDirIn(src, dir, output)
	}
	for _, candidate := range outputCandidates {
		if _, err := os.Stat(filepath.Join(src, candidate, "index.html")); err == nil {
			return realDirIn(src, filepath.Join(src, candidate), candidate)
		}
	}
	if built {
		// the whole source would be served: its dependencies, its sources, an .env
		return "", &OutputError{Output: "with an index.html (dist, build, out, public, _site or site)"}
	}
	return realDirIn(src, src, ".")
}

// realDirIn checks that dir is a directory inside src without a symlink on the
// way, and returns it.
func realDirIn(src, dir, name string) (string, error) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return "", &OutputError{Output: name}
	}
	realSrc, err := filepath.EvalSymlinks(src)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", &OutputError{Output: name}
	}
	inside, _ := within(src, strings.TrimPrefix(real, realSrc))
	if real != realSrc && !strings.HasPrefix(real, realSrc+string(filepath.Separator)) || inside != dir {
		return "", fmt.Errorf("the output %s goes through a symlink, which a site's output cannot", name)
	}
	return dir, nil
}

// OutputError says the build left nothing where the site was expected.
type OutputError struct{ Output string }

func (e *OutputError) Error() string {
	return "the build left no directory " + e.Output + ": set the site's output directory"
}
