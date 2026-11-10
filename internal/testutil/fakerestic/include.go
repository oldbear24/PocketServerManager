package fakerestic

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
)

// What a restore's --include patterns pick (#30). A shell matches no glob
// the way restic does, so the fake restic asks the test binary (see init),
// which matches them as restic 0.17.3's filter does: the agent's
// testdata/restic-0.17.3/restore-include.txt holds what the real one picked.

// includePattern is an --include pattern, split at its slashes.
type includePattern struct {
	parts    []string
	absolute bool
}

// parseInclude splits a pattern, cleaned, at its slashes, and refuses a
// part that is no glob filepath.Match takes, as restic refuses it before
// it restores anything.
func parseInclude(raw string) (includePattern, error) {
	clean := path.Clean(raw)
	pattern := includePattern{absolute: strings.HasPrefix(clean, "/")}
	if trimmed := strings.TrimPrefix(clean, "/"); trimmed != "" {
		pattern.parts = strings.Split(trimmed, "/")
	}
	for _, part := range pattern.parts {
		if _, err := filepath.Match(part, ""); err != nil {
			return pattern, err
		}
	}
	return pattern, nil
}

// matches says whether the pattern matches the path of names, or a folder
// that holds it: an absolute pattern from the root of what is restored, one
// that is not at any depth.
func (p includePattern) matches(names []string) bool {
	if len(p.parts) == 0 || len(p.parts) > len(names) {
		return false
	}
	last := len(names) - len(p.parts)
	if p.absolute {
		last = 0
	}
	for offset := 0; offset <= last; offset++ {
		matched := true
		for i, part := range p.parts {
			if ok, _ := filepath.Match(part, names[offset+i]); !ok {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// includePicks are the items of a restore, each a path relative to what it
// restores with a leading slash, such as /sub/b.txt, that patterns pick, in
// their order. Each part of a pattern is a glob of filepath.Match, in which a
// backslash makes the character after it stand for itself; a pattern picks
// what it matches, what that holds, and the folders that hold it, which
// restic makes as it restores into them. ** is not restic's here.
func includePicks(items, patterns []string) ([]string, error) {
	var parsed []includePattern
	for _, raw := range patterns {
		pattern, err := parseInclude(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", raw, err)
		}
		parsed = append(parsed, pattern)
	}
	picked := map[string]bool{}
	for _, item := range items {
		names := strings.Split(strings.TrimPrefix(item, "/"), "/")
		for _, pattern := range parsed {
			if pattern.matches(names) {
				// the folders that hold it come with it
				for at := item; at != "/" && at != "." && !picked[at]; at = path.Dir(at) {
					picked[at] = true
				}
				break
			}
		}
	}
	var kept []string
	for _, item := range items {
		if picked[item] {
			kept = append(kept, item)
		}
	}
	return kept, nil
}

// answerIncludes reads a restore's items, a line each, and writes those that
// includes, patterns a line each, pick; a pattern restic refuses fails as
// restic fails.
func answerIncludes(stdin io.Reader, stdout, stderr io.Writer, includes string) int {
	var patterns, items []string
	for _, line := range strings.Split(includes, "\n") {
		if line != "" {
			patterns = append(patterns, line)
		}
	}
	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			items = append(items, line)
		}
	}
	picked, err := includePicks(items, patterns)
	if err != nil {
		fmt.Fprintf(stderr, "Fatal: --include: invalid pattern(s) provided:\n%v\n", err)
		return 1
	}
	for _, item := range picked {
		fmt.Fprintln(stdout, item)
	}
	return 0
}
