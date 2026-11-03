package stacks

import (
	"path"
	"regexp"
	"strings"

	yaml "go.yaml.in/yaml/v4"
)

var envAssignment = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$`)

// SplitSecrets moves the lines whose names match out of an env file, for a file that
// was not written with sealed secrets in mind. Comments and everything else stay.
func SplitSecrets(env string, secretLike *regexp.Regexp) (plain string, secrets map[string]string) {
	secrets = map[string]string{}
	lines := []string{}

	for _, line := range strings.Split(env, "\n") {
		match := envAssignment.FindStringSubmatch(line)
		if match == nil || !secretLike.MatchString(match[1]) {
			lines = append(lines, line)
			continue
		}

		values, problems := parseEnv(match[1] + "=" + match[2])
		if len(problems) > 0 {
			// what cannot be read is left where it is, to be seen in the editor
			lines = append(lines, line)
			continue
		}
		secrets[match[1]] = values[match[1]]
	}

	return strings.Join(lines, "\n"), secrets
}

// AbsoluteBinds rewrites relative bind mounts to absolute paths under the directory
// a project was started in. An imported stack is deployed from another directory,
// and its data must stay where it is. Only those values change, in place, so that
// the rest of the file keeps its comments and its layout.
func AbsoluteBinds(compose, workingDir string) string {
	if workingDir == "" {
		return compose
	}

	var tree yaml.Node
	if err := yaml.Unmarshal([]byte(compose), &tree); err != nil {
		return compose
	}

	root := document(&tree)
	if root == nil {
		return compose
	}

	_, services := child(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return compose
	}

	lines := strings.Split(compose, "\n")

	replace := func(node *yaml.Node, from, to string) {
		index := node.Line - 1
		if index >= 0 && index < len(lines) {
			lines[index] = strings.Replace(lines[index], from, to, 1)
		}
	}

	for i := 1; i < len(services.Content); i += 2 {
		_, volumes := child(services.Content[i], "volumes")
		if volumes == nil || volumes.Kind != yaml.SequenceNode {
			continue
		}

		for _, volume := range volumes.Content {
			switch volume.Kind {
			case yaml.ScalarNode:
				source, _, found := strings.Cut(volume.Value, ":")
				if found && isRelative(source) {
					replace(volume, source, path.Join(workingDir, source))
				}
			case yaml.MappingNode:
				_, source := child(volume, "source")
				if source != nil && isRelative(source.Value) {
					replace(source, source.Value, path.Join(workingDir, source.Value))
				}
			}
		}
	}

	return strings.Join(lines, "\n")
}
