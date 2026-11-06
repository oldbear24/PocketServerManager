package agent

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestABranchIsBuiltAtTheCommitAsked: a deployment that builds a release
// again builds its commit, not the branch's newest (#50).
func TestABranchIsBuiltAtTheCommitAsked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", repo}, args...)...)
		command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet", "--initial-branch", "main")
	write := func(content string) string {
		if err := os.WriteFile(filepath.Join(repo, "index.php"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "index.php")
		git("commit", "--quiet", "-m", content)
		return git("rev-parse", "HEAD")
	}
	first := write("first")
	write("second")

	src := filepath.Join(t.TempDir(), "src")
	found, err := gitClone(context.Background(), gitSource{repo: "file://" + repo, branch: "main", commit: first}, src, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(filepath.Join(src, "index.php"))
	if found.Commit != first || string(content) != "first" {
		t.Errorf("built %s with %q", found.Commit, content)
	}
	if _, err := gitClone(context.Background(), gitSource{repo: "file://" + repo, branch: "main", commit: "--upload-pack=x"}, filepath.Join(t.TempDir(), "src"), io.Discard); err == nil {
		t.Error("an option was taken for a commit")
	}
}
