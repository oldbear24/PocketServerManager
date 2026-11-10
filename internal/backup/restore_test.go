package backup

import (
	"encoding/json"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
)

// restorePathCase is one case of testdata/restore_paths.json, which the web's
// mirror of RestorePath reads as well.
type restorePathCase struct {
	Path      string `json:"path"`
	Refused   string `json:"refused"`
	Protected string `json:"protected"`
}

func restorePathCases(t *testing.T) []restorePathCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/restore_paths.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []restorePathCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Cases) == 0 {
		t.Fatal("no cases")
	}
	return file.Cases
}

// checkRestorePaths runs the cases refused for one of reasons, or the taken
// ones when reasons is empty.
func checkRestorePaths(t *testing.T, reasons ...string) {
	t.Helper()
	ran := 0
	for _, tc := range restorePathCases(t) {
		taken := len(reasons) == 0 && tc.Refused == ""
		if !taken && !slices.Contains(reasons, tc.Refused) {
			continue
		}
		ran++
		err := RestorePath(tc.Path)
		switch {
		case tc.Refused == "" && err != nil:
			t.Errorf("%q is refused: %v", tc.Path, err)
		case tc.Refused != "" && err == nil:
			t.Errorf("%q is taken; it is %s", tc.Path, tc.Refused)
		case tc.Refused != "" && reason(tc.Path, err) != tc.Refused:
			t.Errorf("%q: %v; want a refusal for %s", tc.Path, err, tc.Refused)
		case tc.Protected != "" && !strings.Contains(err.Error(), tc.Protected):
			t.Errorf("%q: %v; want it to name %s", tc.Path, err, tc.Protected)
		}
	}
	if ran == 0 {
		t.Fatalf("no case for %v", reasons)
	}
}

// reason tells which rule an error of RestorePath stands for.
func reason(dir string, err error) string {
	switch text := err.Error(); {
	case strings.HasPrefix(text, "give the folder"):
		return "path"
	case strings.Contains(text, "longer than"):
		return "long"
	case strings.Contains(text, "not into /"):
		return "root"
	case strings.HasPrefix(text, dir+" holds "):
		return "holds"
	case strings.Contains(text, "is the system's"):
		return "system"
	case strings.Contains(text, "is PocketCloud's own"):
		return "pocketcloud"
	}
	return "unknown: " + err.Error()
}

// TestRestorePathRefusesSystemAndPocketCloudDirectories: a restore never
// writes the system's directories, those of Docker or of the kernel, nor the
// agent's state, nor a folder that the job container would take for one of
// its own mounts; nor anything under them.
func TestRestorePathRefusesSystemAndPocketCloudDirectories(t *testing.T) {
	checkRestorePaths(t, "system", "pocketcloud")

	// every protected directory, and what lies under it, whatever the table
	for _, dir := range append(append([]string{}, forbidden...), pocketCloudDirs...) {
		for _, p := range []string{dir, dir + "/x", dir + "/x/y"} {
			err := RestorePath(p)
			if err == nil || !strings.Contains(err.Error(), "restore elsewhere") {
				t.Errorf("%s is taken: %v", p, err)
			}
		}
	}
}

// TestRestorePathRefusesParentsOfSystemDirectories: a folder that holds a
// protected directory is refused too. Restored into with replace, which
// deletes what the snapshot lacks, /var would lose /var/lib/docker.
func TestRestorePathRefusesParentsOfSystemDirectories(t *testing.T) {
	checkRestorePaths(t, "holds")

	for _, dir := range append(append([]string{}, forbidden...), pocketCloudDirs...) {
		for parent := path.Dir(dir); parent != "/"; parent = path.Dir(parent) {
			err := RestorePath(parent)
			if err == nil || !strings.Contains(err.Error(), parent+" holds ") && !strings.Contains(err.Error(), "is the system's") {
				t.Errorf("%s holds %s, and is taken: %v", parent, dir, err)
			}
		}
	}
}

// TestRestorePathTakesDataFolders: a folder of one's data is taken, also one
// whose name only starts as a protected directory's, or that holds one of
// the same name deeper down.
func TestRestorePathTakesDataFolders(t *testing.T) {
	checkRestorePaths(t)

	// a path of exactly the most characters, in letters of two bytes each
	if err := RestorePath("/" + strings.Repeat("č", MaxRestorePath-1)); err != nil {
		t.Errorf("a path of %d characters: %v", MaxRestorePath, err)
	}
}

// TestRestorePathRefusesWhatIsNotAnAbsoluteCleanFolder: a relative path, one
// with . or .., a slash too many, a control character or a broken character,
// one too long and / itself are refused before anything else: what the panel
// checks is what the agent resolves.
func TestRestorePathRefusesWhatIsNotAnAbsoluteCleanFolder(t *testing.T) {
	checkRestorePaths(t, "path", "root")

	for name, tc := range map[string]struct {
		dir    string
		reason string
	}{
		"not UTF-8":                        {"/srv/a\xffb", "path"},
		"one character too many":           {"/" + strings.Repeat("a", MaxRestorePath), "long"},
		"too long in letters of two bytes": {"/" + strings.Repeat("č", MaxRestorePath), "long"},
	} {
		t.Run(name, func(t *testing.T) {
			err := RestorePath(tc.dir)
			if err == nil {
				t.Fatal("taken")
			}
			if got := reason(tc.dir, err); got != tc.reason {
				t.Fatalf("%v; want a refusal for %s", err, tc.reason)
			}
		})
	}
}

// TestRestorePathCasesCoverEveryRule keeps the table that the web's mirror
// reads complete: each rule has cases, and each protected directory is named.
func TestRestorePathCasesCoverEveryRule(t *testing.T) {
	seen := map[string]bool{}
	named := map[string]bool{}
	for _, tc := range restorePathCases(t) {
		seen[tc.Refused] = true
		named[tc.Protected] = true
		if (tc.Protected != "") != (tc.Refused == "system" || tc.Refused == "pocketcloud" || tc.Refused == "holds") {
			t.Errorf("%q: protected %q with a refusal for %q", tc.Path, tc.Protected, tc.Refused)
		}
	}
	for _, rule := range []string{"", "path", "root", "system", "pocketcloud", "holds"} {
		if !seen[rule] {
			t.Errorf("no case for %q", rule)
		}
	}
	for _, dir := range append(append([]string{}, forbidden...), pocketCloudDirs...) {
		if !named[dir] && dir != "/run/pocketcloud" {
			t.Errorf("no case names %s", dir)
		}
	}
}

// TestOverlapMatchesParentsAndChildrenButNotPrefixes: two folders overlap
// when one is the other or lies under it, either way round; a folder whose
// name only starts the same does not.
func TestOverlapMatchesParentsAndChildrenButNotPrefixes(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"/srv/app", "/srv/app", true},
		{"/srv/app/data", "/srv/app", true},
		{"/srv/app", "/srv/app/data", true},
		{"/srv", "/srv/app/data/deep", true},
		{"/", "/srv/app", true},
		{"/srv/app", "/", true},
		{"/srv/app2", "/srv/app", false},
		{"/srv/app", "/srv/app2", false},
		{"/srv/app-data", "/srv/app", false},
		{"/srv/a", "/srv/b", false},
		{"/srv/backups/repo", "/srv/backups/repository", false},
	} {
		if got := Overlap(tc.a, tc.b); got != tc.want {
			t.Errorf("Overlap(%s, %s) = %v", tc.a, tc.b, got)
		}
	}
	if !Under("/srv/app/x", "/srv/app") || Under("/srv/app", "/srv/app/x") || !Under("/srv", "/") {
		t.Error("Under is not one way round")
	}
}
