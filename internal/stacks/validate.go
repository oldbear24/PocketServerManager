// Package stacks validates Compose files on the panel and works with their
// environment. The agent checks a file again with `docker compose config` before
// it deploys: this one is what lets the editor point at the line right away.
package stacks

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	yaml "go.yaml.in/yaml/v4"
)

// Problem is one finding about a Compose file. Line counts from 1; 0 means that
// the problem has no particular place.
type Problem struct {
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	File    string `json:"file,omitempty"`
	Message string `json:"message"`
}

// Result is the outcome of a validation.
type Result struct {
	Valid    bool      `json:"valid"`
	Errors   []Problem `json:"errors"`
	Warnings []Problem `json:"warnings"`
	Services []string  `json:"services"`
}

// Names of the two files, as the problems refer to them.
const (
	FileCompose = "compose.yaml"
	FileEnv     = ".env"
)

const maxSearchedLines = 5000

var (
	yamlLine        = regexp.MustCompile(`line (\d+)(?:, column (\d+))?`)
	yamlPlace       = regexp.MustCompile(`^(?:while .*? at (?:line \d+(?:, column \d+)?|<unknown position>): )?(?:line \d+(?:, column \d+)?: )?`)
	quotedName      = regexp.MustCompile(`'([^']+)'`)
	serviceName     = regexp.MustCompile(`service "([^"]+)"`)
	validatingFile  = regexp.MustCompile(`^validating [^:]*: `)
	dottedPath      = regexp.MustCompile(`^([A-Za-z0-9_.-]+)\b`)
	invalidProject  = regexp.MustCompile(`: invalid compose project$`)
	envLine         = regexp.MustCompile(`line (\d+)`)
	interpolatePath = regexp.MustCompile(`invalid interpolation format for ([^\s:]+)`)
)

// Validate checks a Compose file and its environment. The keys of secrets count as
// set, with a placeholder: their values are nobody's business here.
// LoadWith loads a stack's Compose file with a second file over it, as a deploy
// does, with the stack's variables and placeholders for its secrets.
func LoadWith(ctx context.Context, name, compose, override, env string, secretKeys []string) (*types.Project, error) {
	environment, problems := parseEnv(env)
	if len(problems) > 0 {
		return nil, errors.New(problems[0].Message)
	}
	for _, key := range secretKeys {
		environment[key] = "secret"
	}
	return LoadResolved(ctx, name, compose, override, environment)
}

// Environment is what Compose interpolates with at a deploy: the stack's env file,
// with its secret references resolved, and its secrets.
func Environment(plainEnv string, secrets map[string]string) (map[string]string, error) {
	environment, problems := parseEnv(plainEnv)
	if len(problems) > 0 {
		return nil, errors.New(problems[0].Message)
	}
	maps.Copy(environment, secrets)
	return environment, nil
}

// LoadResolved loads a stack's Compose file, and a second file over it if there is
// one, with the variables a deploy has, and the profiles its COMPOSE_PROFILES
// variable turns on. extends within the file is followed; what lies outside the
// file is not.
func LoadResolved(ctx context.Context, name, compose, override string, environment map[string]string) (*types.Project, error) {
	files := []types.ConfigFile{{Filename: FileCompose, Content: []byte(compose)}}
	if override != "" {
		files = append(files, types.ConfigFile{Filename: "compose.pocketcloud.yaml", Content: []byte(override)})
	}
	return loader.LoadWithContext(ctx, types.ConfigDetails{
		WorkingDir:  "/",
		ConfigFiles: files,
		Environment: environment,
	}, func(options *loader.Options) {
		options.SetProjectName(name, true)
		options.ResolvePaths = false
		options.SkipInclude = true
		options.Profiles = profilesOf(environment)
	})
}

// profilesOf are the profiles that COMPOSE_PROFILES turns on.
func profilesOf(environment map[string]string) []string {
	profiles := []string{}
	for _, profile := range strings.Split(environment["COMPOSE_PROFILES"], ",") {
		if profile = strings.TrimSpace(profile); profile != "" {
			profiles = append(profiles, profile)
		}
	}
	return profiles
}

// referencingSecrets are the variables of an env file whose values take a
// panel secret.
func referencingSecrets(env string) []string {
	keys := []string{}
	for _, line := range strings.Split(env, "\n") {
		if key, value, found := strings.Cut(line, "="); found && SecretReference.MatchString(value) {
			keys = append(keys, strings.TrimPrefix(strings.TrimSpace(key), "export "))
		}
	}
	return keys
}

func Validate(ctx context.Context, name, compose, env string, secretKeys []string) Result {
	result := Result{Errors: []Problem{}, Warnings: []Problem{}, Services: []string{}}

	var tree yaml.Node
	if err := yaml.Unmarshal([]byte(compose), &tree); err != nil {
		result.Errors = append(result.Errors, yamlProblem(compose, err))
		return result
	}

	environment, problems := parseEnv(env)
	result.Errors = append(result.Errors, problems...)
	if len(problems) > 0 {
		return result
	}

	// the values of secrets are stand-ins here
	unknown := map[string]bool{}
	for _, key := range secretKeys {
		environment[key] = "secret"
		unknown[key] = true
	}
	for _, key := range referencingSecrets(env) {
		unknown[key] = true
	}

	if strings.TrimSpace(compose) == "" {
		result.Errors = append(result.Errors, Problem{File: FileCompose, Line: 1, Message: "The Compose file is empty."})
		return result
	}

	project, err := loader.LoadWithContext(ctx, types.ConfigDetails{
		WorkingDir:  "/",
		ConfigFiles: []types.ConfigFile{{Filename: FileCompose, Content: []byte(compose)}},
		Environment: environment,
	}, func(options *loader.Options) {
		options.SetProjectName(name, true)
		options.ResolvePaths = false
		// what is outside of the file cannot be checked from here; the agent does that
		options.SkipInclude = true
		options.SkipExtends = true
		options.Profiles = profilesOf(environment)
	})
	if err != nil {
		result.Errors = append(result.Errors, loaderProblem(err, &tree))
		return result
	}

	result.Valid = true
	result.Services = slices.Sorted(maps.Keys(project.Services))
	result.Warnings = warningsOf(project, &tree)
	// a label that takes a secret is checked at the deploy
	_, labelProblems := Exposures(project, &tree, placeholdersIn(&tree, unknown))
	result.Warnings = append(result.Warnings, labelProblems...)
	_, backupProblems := BackupLabels(project, &tree, placeholdersIn(&tree, unknown))
	result.Warnings = append(result.Warnings, backupProblems...)

	return result
}

// yamlProblem finds the line of a syntax error. A parser reports where the
// surrounding block began rather than where it went wrong, so the line comes from
// the longest beginning of the file that still parses.
func yamlProblem(compose string, err error) Problem {
	message := strings.TrimPrefix(err.Error(), "yaml: ")

	return Problem{
		File:    FileCompose,
		Line:    firstBrokenLine(compose),
		Message: yamlPlace.ReplaceAllString(message, ""),
	}
}

func firstBrokenLine(compose string) int {
	lines := strings.Split(compose, "\n")

	// a file this long is not written by hand, and each try parses all of it
	if len(lines) > maxSearchedLines {
		return 0
	}

	for count := len(lines) - 1; count > 0; count-- {
		var node yaml.Node
		if yaml.Unmarshal([]byte(strings.Join(lines[:count], "\n")), &node) == nil {
			return count + 1
		}
	}

	return 1
}

// loaderProblem finds the line of a problem compose-go describes by its path, such
// as `services.web.ports must be a list`.
func loaderProblem(err error, tree *yaml.Node) Problem {
	message := strings.TrimSpace(err.Error())
	message = validatingFile.ReplaceAllString(message, "")
	message = invalidProject.ReplaceAllString(message, "")

	problem := Problem{File: FileCompose, Message: message}

	if match := yamlLine.FindStringSubmatch(message); match != nil && strings.Contains(message, "yaml:") {
		problem.Line, _ = strconv.Atoi(match[1])
		problem.Column, _ = strconv.Atoi(match[2])
		return problem
	}

	if match := interpolatePath.FindStringSubmatch(message); match != nil {
		problem.Line, problem.Column = lineOf(tree, strings.Split(match[1], "."))
		return problem
	}

	if match := dottedPath.FindStringSubmatch(message); match != nil && strings.Contains(match[1], ".") || match != nil && match[1] == "services" {
		path := strings.Split(match[1], ".")

		// "services.web additional properties 'foo' not allowed" points at foo
		if extra := quotedName.FindStringSubmatch(message); extra != nil {
			if line, column := exactLineOf(tree, append(slices.Clone(path), extra[1])); line > 0 {
				problem.Line, problem.Column = line, column
				return problem
			}
		}

		problem.Line, problem.Column = lineOf(tree, path)
		return problem
	}

	if match := serviceName.FindStringSubmatch(message); match != nil {
		problem.Line, problem.Column = lineOf(tree, []string{"services", match[1]})
	}

	return problem
}

// lineOf returns the place of a path in the file, or of the deepest part of it
// that exists: a missing key is still best looked for around its parent.
func lineOf(tree *yaml.Node, path []string) (line, column int) {
	node := document(tree)
	if node == nil {
		return 0, 0
	}

	line, column = node.Line, node.Column

	for _, part := range path {
		key, value := child(node, part)
		if value == nil {
			return line, column
		}
		line, column = key.Line, key.Column
		node = value
	}

	return line, column
}

// exactLineOf returns the place of a path only if all of it exists.
func exactLineOf(tree *yaml.Node, path []string) (line, column int) {
	node := document(tree)

	for _, part := range path {
		if node == nil {
			return 0, 0
		}
		key, value := child(node, part)
		if value == nil {
			return 0, 0
		}
		line, column = key.Line, key.Column
		node = value
	}

	return line, column
}

func document(tree *yaml.Node) *yaml.Node {
	if tree.Kind == yaml.DocumentNode && len(tree.Content) > 0 {
		return tree.Content[0]
	}
	return nil
}

// child returns the key node and the value node of a mapping entry or list item.
func child(node *yaml.Node, name string) (key, value *yaml.Node) {
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}

	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == name {
				return node.Content[i], node.Content[i+1]
			}
		}
	case yaml.SequenceNode:
		index, err := strconv.Atoi(name)
		if err == nil && index >= 0 && index < len(node.Content) {
			return node.Content[index], node.Content[index]
		}
	}

	return nil, nil
}

// parseEnv reads an env file. References to panel secrets stand in for values
// that are only known when a command is delivered.
func parseEnv(env string) (map[string]string, []Problem) {
	withoutReferences := SecretReference.ReplaceAllString(env, "secret")

	values, err := dotenv.UnmarshalWithLookup(withoutReferences, nil)
	if err != nil {
		problem := Problem{File: FileEnv, Message: err.Error()}
		if match := envLine.FindStringSubmatch(err.Error()); match != nil {
			problem.Line, _ = strconv.Atoi(match[1])
		}
		return nil, []Problem{problem}
	}

	return values, nil
}

// warningsOf finds what is valid but will not do what it seems to.
func warningsOf(project *types.Project, tree *yaml.Node) []Problem {
	warnings := []Problem{}

	for _, name := range slices.Sorted(maps.Keys(project.Services)) {
		service := project.Services[name]

		for index, volume := range service.Volumes {
			if volume.Type != types.VolumeTypeBind || !isRelative(volume.Source) {
				continue
			}

			line, column := lineOf(tree, []string{"services", name, "volumes", strconv.Itoa(index)})
			warnings = append(warnings, Problem{
				File:   FileCompose,
				Line:   line,
				Column: column,
				Message: fmt.Sprintf("%s: the relative path %s is resolved on the server, under the stack's directory in the agent volume, where it is not reachable as such. "+
					"Use a named volume or an absolute path.", name, volume.Source),
			})
		}
	}

	return warnings
}

func isRelative(path string) bool {
	return strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../") || path == "."
}

// SecretReference finds `${secret:name}` in an env file: a value the panel keeps
// sealed and only puts into the command when it is delivered.
var SecretReference = regexp.MustCompile(`\$\{secret:([A-Za-z0-9_./-]+)\}`)

// ErrUnknownSecret is returned for a reference to a secret that does not exist.
var ErrUnknownSecret = errors.New("unknown secret")

// EnvKeys returns the names an env file sets, sorted.
func EnvKeys(env string) []string {
	values, problems := parseEnv(env)
	if len(problems) > 0 {
		return []string{}
	}
	return slices.Sorted(maps.Keys(values))
}

// ResolveEnv splits an env file into what may be stored on the server and what may
// not: a line that refers to a panel secret leaves the file and becomes a secret of
// the deploy, with the reference replaced by the value.
func ResolveEnv(env string, resolve func(name string) (string, error)) (plain string, secrets map[string]string, err error) {
	secrets = map[string]string{}

	lines := []string{}
	for _, line := range strings.Split(env, "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found || !SecretReference.MatchString(value) {
			lines = append(lines, line)
			continue
		}

		var failure error
		resolved := SecretReference.ReplaceAllStringFunc(value, func(reference string) string {
			secret, err := resolve(SecretReference.FindStringSubmatch(reference)[1])
			if err != nil {
				failure = fmt.Errorf("%s: %w", strings.TrimSpace(key), err)
			}
			return secret
		})
		if failure != nil {
			return "", nil, failure
		}

		secrets[strings.TrimPrefix(strings.TrimSpace(key), "export ")] = strings.Trim(resolved, `"'`)
	}

	return strings.Join(lines, "\n"), secrets, nil
}
