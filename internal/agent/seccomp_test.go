package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestASitesCodeCannotMakeSymbolicLinks: the profile of PHP's containers is
// Docker's default one without the calls that make symbolic links.
func TestASitesCodeCannotMakeSymbolicLinks(t *testing.T) {
	option, err := phpSeccomp()
	if err != nil {
		t.Fatal(err)
	}
	raw, found := strings.CutPrefix(option, "seccomp=")
	if !found {
		t.Fatalf("not a seccomp option: %.40s", option)
	}
	var profile struct {
		DefaultAction string `json:"defaultAction"`
		Syscalls      []struct {
			Names  []string `json:"names"`
			Action string   `json:"action"`
		} `json:"syscalls"`
	}
	if err := json.Unmarshal([]byte(raw), &profile); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, rule := range profile.Syscalls {
		for _, name := range rule.Names {
			allowed[name] = allowed[name] || rule.Action == "SCMP_ACT_ALLOW"
		}
	}
	if profile.DefaultAction != "SCMP_ACT_ERRNO" || allowed["symlink"] || allowed["symlinkat"] {
		t.Errorf("default %s, symlink %v, symlinkat %v", profile.DefaultAction, allowed["symlink"], allowed["symlinkat"])
	}
	// what PHP needs stays
	for _, name := range []string{"openat", "rename", "renameat2", "link", "unlink", "mkdir", "execve", "socket"} {
		if !allowed[name] {
			t.Errorf("%s is not allowed", name)
		}
	}
}
