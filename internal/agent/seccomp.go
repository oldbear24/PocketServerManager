package agent

import (
	_ "embed"
	"encoding/json"
	"slices"
	"sync"
)

//go:embed seccomp/docker-default.json
var dockerSeccomp []byte

// noLinks are the system calls that make symbolic links.
var noLinks = []string{"symlink", "symlinkat"}

// phpSeccomp is the security option of the containers that run a PHP site's
// code, PHP-FPM and the release hook: Docker's default profile, in which
// making a symbolic link fails with EPERM. The edge mounts the whole sites
// volume and follows links, so a link that a site's code made in its shared
// paths, such as uploads/b.txt -> /srv/sites/other/shared/.env, would serve
// another site's files (#50). The links PocketCloud makes itself, current and
// those of the shared paths, are made by the site tool.
var phpSeccomp = sync.OnceValues(func() (string, error) {
	var profile map[string]any
	if err := json.Unmarshal(dockerSeccomp, &profile); err != nil {
		return "", err
	}
	rules, _ := profile["syscalls"].([]any)
	for _, rule := range rules {
		fields, ok := rule.(map[string]any)
		if !ok {
			continue
		}
		names, _ := fields["names"].([]any)
		fields["names"] = slices.DeleteFunc(names, func(name any) bool {
			text, _ := name.(string)
			return slices.Contains(noLinks, text)
		})
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		return "", err
	}
	return "seccomp=" + string(encoded), nil
})
