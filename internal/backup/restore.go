package backup

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxRestorePath is the longest folder a restore writes into, in characters.
const MaxRestorePath = 1000

// pocketCloudDirs are directories a restore never writes: the agent's state
// and runtime directories on the server, and the directories of the job
// container that runs restic. A restore names a server's folder; one of
// these on the server is PocketCloud's, or would be taken for a mount of
// the job.
var pocketCloudDirs = []string{
	"/var/lib/pocketcloud", "/run/pocketcloud",
	"/cache", LocalMount, "/volumes", "/restore", "/host",
}

// RestorePath checks a server's folder that a restore writes into (#30),
// whether a new folder or the original location of a directory. It must be
// absolute and clean, at most MaxRestorePath characters of valid UTF-8 with
// no control character, and not / itself; not the system's, nor under one
// of its directories (those a local target may not be in either), nor
// PocketCloud's own; and not a folder that holds one of them, such as /var,
// which a restore that deletes what the snapshot lacks would empty. The
// panel checks the folder as it is given; the agent checks it again, and
// again once it has followed its links.
func RestorePath(dir string) error {
	if !path.IsAbs(dir) || path.Clean(dir) != dir || !utf8.ValidString(dir) || strings.IndexFunc(dir, unicode.IsControl) >= 0 {
		return errors.New("give the folder by its absolute path, such as /srv/restored")
	}
	if utf8.RuneCountInString(dir) > MaxRestorePath {
		return fmt.Errorf("the folder's path is longer than %d characters", MaxRestorePath)
	}
	if dir == "/" {
		return errors.New("restore into a folder of its own, not into /")
	}
	for _, system := range forbidden {
		if Overlap(dir, system) {
			return refused(dir, system, "the system's")
		}
	}
	for _, own := range pocketCloudDirs {
		if Overlap(dir, own) {
			return refused(dir, own, "PocketCloud's own")
		}
	}
	return nil
}

// refused says why a restore may not write dir, which is protected or lies
// under protected, or holds it.
func refused(dir, protected, whose string) error {
	if Under(protected, dir) && protected != dir {
		return fmt.Errorf("%s holds %s, which is %s: restore elsewhere, such as /srv/restored", dir, protected, whose)
	}
	return fmt.Errorf("%s is %s: restore elsewhere, such as /srv/restored", protected, whose)
}

// Under says whether p is dir or lies under it. Both are clean absolute
// paths; a name that only starts the same, such as /srv/app2 for /srv/app,
// is not under it.
func Under(p, dir string) bool {
	return p == dir || dir == "/" || strings.HasPrefix(p, dir+"/")
}

// Overlap says whether two folders share files: one is the other, or lies
// under it. A restore into one and a backup or another restore of the other
// would write or read the same files.
func Overlap(a, b string) bool {
	return Under(a, b) || Under(b, a)
}
