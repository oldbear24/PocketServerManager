// Package phpapps is PocketCloud's PHP application templates (#52):
// WordPress, Laravel and Nette, as YAML in templates/. A template says how a
// framework's site is laid out and run: the directory the edge serves, what
// survives its releases and what a backup leaves out, its release hook, the
// links PocketCloud makes in each release, its cron tasks, php.ini lines,
// environment and secrets, its health check, how a source is recognised as
// one of its sites, and, for some, how a site is made without a source of its
// own, how it is updated, and a worker container.
//
// The panel applies a template's defaults when a site is made with it, into
// the site's PHP settings, which can be changed after; the rest of it (the
// files, links, caches emptied at a release, the environment file, the
// worker and the update) is read from the template whenever a site of it is
// deployed or applied.
package phpapps

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"

	yaml "go.yaml.in/yaml/v4"
)

//go:embed templates
var templateFS embed.FS

// Database needs of a template.
const (
	// DatabaseRequired templates do not run without a database.
	DatabaseRequired = "required"
)

// Kinds of a secret a template makes for a new site.
const (
	// SecretRandom is 48 random bytes, hex encoded.
	SecretRandom = "random"
	// SecretLaravelKey is Laravel's APP_KEY: base64: and 32 random bytes.
	SecretLaravelKey = "laravel-key" //nolint:gosec // the name of a kind, not a secret
)

// Template is a PHP application's template.
type Template struct {
	// Name is how sites name it: wordpress, laravel, nette.
	Name  string `yaml:"name"`
	Title string `yaml:"title"`
	// Detect are paths of a source, any of which makes it one of the
	// template's sites.
	Detect []string `yaml:"detect"`
	// Docroot is the directory the edge serves, in the release; empty for the
	// release itself.
	Docroot string `yaml:"docroot"`
	// Database is required when the application does not run without one.
	Database string `yaml:"database"`
	// Health is the path the panel asks after each release, which answers
	// below 400 while the site works.
	Health string `yaml:"health"`
	// Fresh is how a site without a source of its own is made.
	Fresh *Fresh `yaml:"fresh"`
	// Files are put into a source that does not have them, before its build.
	Files []File `yaml:"files"`
	// ReleaseHook runs in each new release before it goes live.
	ReleaseHook string `yaml:"release_hook"`
	// Links are made in each release, relative, as PHP of a site cannot.
	Links []Link `yaml:"links"`
	// Shared are the paths kept in shared/ across releases.
	Shared []Shared `yaml:"shared"`
	// BackupExclude are paths of the site, in its release or its shared
	// paths, that a backup leaves out besides the shared paths that say so.
	BackupExclude []string `yaml:"backup_exclude"`
	// EnvFile is a file of the release that the site's environment is
	// written into, as KEY="value" lines, at each release.
	EnvFile string `yaml:"env_file"`
	// RedeployOnEnv: the application caches its environment at a release, so
	// a change of it deploys the live release again.
	RedeployOnEnv bool     `yaml:"redeploy_on_env"`
	PHPIni        []string `yaml:"php_ini"`
	Env           []Env    `yaml:"env"`
	// DatabaseEnv is added to the environment when a database is connected
	// to the site, besides DB_HOST, DB_PORT, DB_NAME, DB_USER and DB_PASSWORD.
	DatabaseEnv []Env    `yaml:"database_env"`
	Secrets     []Secret `yaml:"secrets"`
	Tasks       []Task   `yaml:"tasks"`
	Worker      *Worker  `yaml:"worker"`
	Update      *Update  `yaml:"update"`
}

// Fresh is how a site without a source of its own is made: Build runs in its
// runtime image as the first release's build.
type Fresh struct {
	Build string `yaml:"build"`
}

// File is a file the template gives a source that lacks it.
type File struct {
	Path string `yaml:"path"`
	// From is the file in templates/.
	From    string `yaml:"from"`
	Content string `yaml:"-"`
}

// Link is a symlink at Path in the release that leads to Target, both
// relative to the release.
type Link struct {
	Path   string `yaml:"path"`
	Target string `yaml:"target"`
}

// Shared is a path kept in shared/ across releases.
type Shared struct {
	Path string `yaml:"path"`
	// Backup false leaves the path out of the site's backups.
	Backup *bool `yaml:"backup"`
	// Clear empties it once a new release is live.
	Clear bool `yaml:"clear"`
}

// Env is a variable of the site's environment. Value is a text/template with
// the fields of Vars, or of DatabaseVars for DatabaseEnv.
type Env struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
	// WithoutDatabase: only for a site made without a database.
	WithoutDatabase bool `yaml:"without_database"`
}

// Secret is a secret of the site that the template makes for a new site.
type Secret struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind"`
}

// Task is a scheduled task of a new site, in UTC.
type Task struct {
	Schedule string `yaml:"schedule"`
	Command  string `yaml:"command"`
	TimeoutS int    `yaml:"timeout_s"`
}

// Worker is a container of the site that runs Command in the live release,
// from the site's runtime image, restarted at each release.
type Worker struct {
	Command string `yaml:"command"`
}

// Update is what "Update" runs in a copy of the live release before its
// release hook. AddPlugin and AddTheme add plugins or themes with it, each
// named after the command, quoted.
type Update struct {
	Command   string `yaml:"command"`
	AddPlugin string `yaml:"add_plugin"`
	AddTheme  string `yaml:"add_theme"`
}

// Vars are what a template's environment and fresh build refer to.
type Vars struct {
	// URL is the site's address, such as https://blog.example.com.
	URL      string
	Hostname string
	// Locale is the language of a fresh install, such as en_US.
	Locale string
}

// DatabaseVars are what a template's database environment refers to.
type DatabaseVars struct {
	// Engine is mariadb, mysql or postgres.
	Engine   string
	Host     string
	Port     int
	Database string
	User     string
}

var (
	envName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	locale     = regexp.MustCompile(`^[a-z]{2,3}(_[A-Z]{2})?(_[a-z]+)?$`)
	all        = mustLoad()
)

// All are the templates, by name.
func All() []Template {
	return slices.Clone(all)
}

// Get is the template of a name, if there is one; custom has none.
func Get(name string) (Template, bool) {
	for _, template := range all {
		if template.Name == name {
			return template, true
		}
	}
	return Template{}, false
}

// Names are the templates' names.
func Names() []string {
	names := make([]string, 0, len(all))
	for _, template := range all {
		names = append(names, template.Name)
	}
	return names
}

// ValidLocale checks the language of a fresh install, such as en_US or cs_CZ.
func ValidLocale(value string) error {
	if !locale.MatchString(value) {
		return fmt.Errorf("%q is not a language such as en_US or cs_CZ", value)
	}
	return nil
}

// Detect is the template whose site a source is, by the paths it has
// (slash-separated, relative to its root), or "" for none. Laravel comes
// before WordPress and Nette, as a Laravel application may have a
// wp-config.php of a site it imports, and none of theirs has artisan.
func Detect(paths []string) string {
	have := make(map[string]bool, len(paths))
	for _, p := range paths {
		have[strings.TrimSuffix(strings.TrimPrefix(path.Clean("/"+p), "/"), "/")] = true
	}
	for _, template := range all {
		for _, p := range template.Detect {
			if have[p] {
				return template.Name
			}
		}
	}
	return ""
}

// DetectDir is Detect for a source on disk.
func DetectDir(dir string) string {
	var paths []string
	for _, template := range all {
		for _, p := range template.Detect {
			if info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(p))); err == nil && info.Mode().IsRegular() {
				paths = append(paths, p)
			}
		}
	}
	return Detect(paths)
}

// FreshBuild is the build of a fresh install in a language.
func (t Template) FreshBuild(vars Vars) (string, error) {
	if t.Fresh == nil {
		return "", fmt.Errorf("%s has no fresh install", t.Title)
	}
	if err := ValidLocale(vars.Locale); err != nil {
		return "", err
	}
	return render(t.Fresh.Build, vars)
}

// SharedPaths are the paths kept in shared/.
func (t Template) SharedPaths() []string {
	paths := make([]string, 0, len(t.Shared))
	for _, shared := range t.Shared {
		paths = append(paths, shared.Path)
	}
	return paths
}

// Cleared are the shared paths emptied once a new release is live.
func (t Template) Cleared() []string {
	var paths []string
	for _, shared := range t.Shared {
		if shared.Clear {
			paths = append(paths, shared.Path)
		}
	}
	return paths
}

// NoBackup are the paths a backup of the site leaves out.
func (t Template) NoBackup() []string {
	paths := slices.Clone(t.BackupExclude)
	for _, shared := range t.Shared {
		if shared.Backup != nil && !*shared.Backup {
			paths = append(paths, shared.Path)
		}
	}
	return paths
}

// LinkSpecs are the links as path=target.
func (t Template) LinkSpecs() []string {
	specs := make([]string, 0, len(t.Links))
	for _, link := range t.Links {
		specs = append(specs, link.Path+"="+link.Target)
	}
	return specs
}

// FileContents are the template's files by their path in the source.
func (t Template) FileContents() map[string]string {
	files := make(map[string]string, len(t.Files))
	for _, file := range t.Files {
		files[file.Path] = file.Content
	}
	return files
}

// IniLines are the template's php.ini lines, as the site's settings keep them.
func (t Template) IniLines() string {
	if len(t.PHPIni) == 0 {
		return ""
	}
	return strings.Join(t.PHPIni, "\n") + "\n"
}

// EnvLines are the template's environment of a new site, as NAME=value lines:
// with database says whether the site is made with one.
func (t Template) EnvLines(vars Vars, withDatabase bool) ([]string, error) {
	lines := make([]string, 0, len(t.Env))
	for _, env := range t.Env {
		if env.WithoutDatabase && withDatabase {
			continue
		}
		value, err := render(env.Value, vars)
		if err != nil {
			return nil, err
		}
		lines = append(lines, env.Name+"="+value)
	}
	return lines, nil
}

// DatabaseEnvLines are the template's lines for a database of the site.
func (t Template) DatabaseEnvLines(vars DatabaseVars) ([]string, error) {
	lines := make([]string, 0, len(t.DatabaseEnv))
	for _, env := range t.DatabaseEnv {
		value, err := render(env.Value, vars)
		if err != nil {
			return nil, err
		}
		lines = append(lines, env.Name+"="+value)
	}
	return lines, nil
}

// NewSecrets makes the template's secrets for a new site.
func (t Template) NewSecrets() (map[string]string, error) {
	secrets := make(map[string]string, len(t.Secrets))
	for _, secret := range t.Secrets {
		value, err := newSecret(secret.Kind)
		if err != nil {
			return nil, err
		}
		secrets[secret.Name] = value
	}
	return secrets, nil
}

func newSecret(kind string) (string, error) {
	switch kind {
	case SecretRandom:
		key := make([]byte, 48)
		if _, err := rand.Read(key); err != nil {
			return "", err
		}
		return hex.EncodeToString(key), nil
	case SecretLaravelKey:
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return "", err
		}
		return "base64:" + base64.StdEncoding.EncodeToString(key), nil
	}
	return "", fmt.Errorf("unknown kind of secret %q", kind)
}

func render(text string, data any) (string, error) {
	parsed, err := template.New("").Option("missingkey=error").Parse(text)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := parsed.Execute(&out, data); err != nil {
		return "", err
	}
	return out.String(), nil
}

// EnvFile is the environment as a file of KEY="value" lines that Laravel's
// phpdotenv reads as the values are, sorted by name. A value is in double
// quotes, with \, " and $ escaped, and a line break as \n: phpdotenv would
// otherwise expand ${...} in it.
func EnvFile(env map[string]string) string {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	slices.Sort(names)
	var out strings.Builder
	out.WriteString("# Written by PocketCloud at the release, from the site's environment.\n")
	out.WriteString("# Change it on the site's page in PocketCloud: the next release writes it anew.\n")
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "\n", `\n`, "\r", `\r`)
	for _, name := range names {
		fmt.Fprintf(&out, "%s=\"%s\"\n", name, replacer.Replace(env[name]))
	}
	return out.String()
}

func mustLoad() []Template {
	templates, err := load(templateFS)
	if err != nil {
		panic(err)
	}
	return templates
}

// load reads and checks the templates, in the order Detect tries them.
func load(fsys fs.FS) ([]Template, error) {
	entries, err := fs.Glob(fsys, "templates/*.yaml")
	if err != nil {
		return nil, err
	}
	var templates []Template
	for _, entry := range entries {
		data, err := fs.ReadFile(fsys, entry)
		if err != nil {
			return nil, err
		}
		var t Template
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&t); err != nil {
			return nil, fmt.Errorf("%s: %w", entry, err)
		}
		for i, file := range t.Files {
			content, err := fs.ReadFile(fsys, path.Join("templates", file.From))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", entry, err)
			}
			t.Files[i].Content = string(content)
		}
		if err := t.check(); err != nil {
			return nil, fmt.Errorf("%s: %w", entry, err)
		}
		if path.Base(entry) != t.Name+".yaml" {
			return nil, fmt.Errorf("%s: names %s", entry, t.Name)
		}
		templates = append(templates, t)
	}
	order := map[string]int{"laravel": 0, "wordpress": 1, "nette": 2}
	slices.SortStableFunc(templates, func(a, b Template) int { return order[a.Name] - order[b.Name] })
	return templates, nil
}

// releasePath checks a path inside a release: relative, without .. or a
// leading /, as sites.ValidSharedPath has them.
func releasePath(p string) error {
	clean := path.Clean(p)
	if p == "" || path.IsAbs(p) || clean != p || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") ||
		strings.ContainsAny(p, ",=\\\n\r\x00") {
		return fmt.Errorf("%q is not a path inside the release", p)
	}
	return nil
}

func (t Template) check() error {
	var problems []error
	if t.Name == "" || t.Title == "" {
		problems = append(problems, errors.New("a template has a name and a title"))
	}
	if t.Docroot != "" {
		problems = append(problems, releasePath(t.Docroot))
	}
	if t.Database != "" && t.Database != DatabaseRequired {
		problems = append(problems, fmt.Errorf("database is %s or left out", DatabaseRequired))
	}
	if t.Health == "" || !strings.HasPrefix(t.Health, "/") || strings.ContainsAny(t.Health, " \n\r#") {
		problems = append(problems, fmt.Errorf("health %q is a path such as /up", t.Health))
	}
	for _, p := range t.Detect {
		problems = append(problems, releasePath(p))
	}
	for _, file := range t.Files {
		problems = append(problems, releasePath(file.Path))
	}
	for _, link := range t.Links {
		problems = append(problems, releasePath(link.Path), releasePath(link.Target))
	}
	for _, shared := range t.Shared {
		problems = append(problems, releasePath(shared.Path))
	}
	for _, p := range t.BackupExclude {
		problems = append(problems, releasePath(p))
	}
	if t.EnvFile != "" {
		problems = append(problems, releasePath(t.EnvFile))
	}
	for _, env := range append(slices.Clone(t.Env), t.DatabaseEnv...) {
		if !envName.MatchString(env.Name) || strings.HasPrefix(env.Name, "POCKETCLOUD_") {
			problems = append(problems, fmt.Errorf("%q is not a variable name of a site", env.Name))
		}
		if _, err := template.New("").Parse(env.Value); err != nil {
			problems = append(problems, err)
		}
	}
	for _, secret := range t.Secrets {
		if !secretName.MatchString(secret.Name) {
			problems = append(problems, fmt.Errorf("%q is not a secret name", secret.Name))
		}
		if _, err := newSecret(secret.Kind); err != nil {
			problems = append(problems, err)
		}
	}
	for _, task := range t.Tasks {
		if task.Schedule == "" || task.Command == "" || task.TimeoutS < 1 || task.TimeoutS > 86400 {
			problems = append(problems, fmt.Errorf("the task %q needs a schedule, a command and a timeout of 1 to 86400 s", task.Command))
		}
	}
	if t.Fresh != nil && t.Fresh.Build == "" {
		problems = append(problems, errors.New("a fresh install has a build"))
	}
	if t.Worker != nil && t.Worker.Command == "" {
		problems = append(problems, errors.New("a worker has a command"))
	}
	if t.Update != nil && t.Update.Command == "" {
		problems = append(problems, errors.New("an update has a command"))
	}
	return errors.Join(problems...)
}
