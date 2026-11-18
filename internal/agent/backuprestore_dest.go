package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
)

// Where a restore writes (#30): each root's destination, a volume or a
// folder of the server, checked before the job mounts it.

// restoreDest is one root of a restore and where it goes on the server.
type restoreDest struct {
	// source is the snapshot's path of the root; "" finds the snapshot's
	// only path, "/" is all of it
	source string
	// volume is a volume of the server the root goes back into
	volume string
	// dir is a folder of the server, with no symbolic link left in it
	dir string
	// asked is the folder as the command named it
	asked string
	// exists says the folder is there; a volume always is
	exists bool
	// create asks the Engine to make a missing folder, as for a new one
	create bool
}

// label is how the restore names the destination.
func (d restoreDest) label() string {
	if d.volume != "" {
		return d.volume
	}
	return d.dir
}

// claimKey is the destination's data claim.
func (d restoreDest) claimKey() string {
	if d.volume != "" {
		return "volume:" + d.volume
	}
	return "path:" + d.dir
}

// mount is how the restore's job mounts the destination at target: writable
// for a restore that writes, read-only for a preview, which does not mount
// a folder that is not there, and so creates nothing.
func (d restoreDest) mount(target string, writes bool) map[string]any {
	switch {
	case d.volume != "":
		mount := map[string]any{"Type": "volume", "Source": d.volume, "Target": target}
		if !writes {
			mount["ReadOnly"] = true
		}
		return mount
	case !d.exists && !writes:
		return nil
	}
	mount := map[string]any{"Type": "bind", "Source": d.dir, "Target": target}
	switch {
	case !writes:
		mount["ReadOnly"] = true
	case d.create:
		mount["BindOptions"] = map[string]any{"CreateMountpoint": true}
	}
	return mount
}

// restoreDests checks where a restore writes, and says what it found of
// folders that lead elsewhere. A restore to the original location names
// volumes that must exist and folders that must exist; a restore to a new
// folder names one folder, which may not be there yet.
func (a *Agent) restoreDests(ctx context.Context, payload *agentv1.BackupRestore) ([]restoreDest, []string, error) {
	var dests []restoreDest
	var notes []string
	if payload.GetTargetPath() != "" {
		dest, note, err := a.restoreFolder(payload.GetTargetPath(), false)
		if err != nil {
			return nil, nil, err
		}
		dest.source = payload.GetSource()
		if dest.source == "" {
			dest.source = "/"
		}
		dest.create = true
		dests = append(dests, dest)
		notes = append(notes, note...)
	}
	for _, root := range payload.GetOriginal() {
		var dest restoreDest
		switch {
		case root.GetVolume() != "" && root.GetPath() != "":
			return nil, nil, errors.New("a root of a restore goes back into a volume or a folder, not both")
		case root.GetVolume() != "":
			if err := a.checkRestoreVolume(ctx, root.GetVolume()); err != nil {
				return nil, nil, err
			}
			dest = restoreDest{volume: root.GetVolume(), exists: true}
		case root.GetPath() != "":
			var note []string
			var err error
			if dest, note, err = a.restoreFolder(root.GetPath(), true); err != nil {
				return nil, nil, err
			}
			notes = append(notes, note...)
		default:
			return nil, nil, errors.New("a root of a restore names the volume or the folder it goes back into")
		}
		dest.source = root.GetSource()
		if dest.source != "" {
			if err := checkSnapshotPath(dest.source); err != nil {
				return nil, nil, err
			}
		}
		dests = append(dests, dest)
	}
	// each destination once, and none in another
	for i, dest := range dests {
		for _, other := range dests[:i] {
			if dest.volume != "" && dest.volume == other.volume || dest.dir != "" && other.dir != "" && backup.Overlap(dest.dir, other.dir) {
				return nil, nil, fmt.Errorf("%s and %s are the same data: a restore writes each once", other.label(), dest.label())
			}
		}
	}
	// never a local target's own repository, nor what holds it
	if mount := payload.GetRepository().GetMount(); mount != "" {
		local, err := backup.LocalPath(mount)
		if err != nil {
			return nil, nil, err
		}
		if real, _, err := resolveServerTarget(a.hostRoot(), local); err == nil {
			local = real
		}
		for _, dest := range dests {
			if dest.dir != "" && backup.Overlap(dest.dir, local) {
				return nil, nil, fmt.Errorf("%s is the repository of the target this restore reads: restore elsewhere", local)
			}
		}
	}
	return dests, notes, nil
}

// restoreFolder checks a folder of the server a restore writes into, as the
// command names it and again where its symbolic links lead.
func (a *Agent) restoreFolder(dir string, mustExist bool) (restoreDest, []string, error) {
	if err := backup.RestorePath(dir); err != nil {
		return restoreDest{}, nil, err
	}
	real, exists, err := resolveServerTarget(a.hostRoot(), dir)
	if err != nil {
		return restoreDest{}, nil, err
	}
	if err := backup.RestorePath(real); err != nil {
		return restoreDest{}, nil, fmt.Errorf("%s leads to %s on this server: %w", dir, real, err)
	}
	if mustExist && !exists {
		return restoreDest{}, nil, fmt.Errorf("the folder %s does not exist on this server", dir)
	}
	var notes []string
	if real != dir {
		notes = append(notes, fmt.Sprintf("The folder %s leads to %s on this server: restoring into %s", dir, real, real))
	}
	return restoreDest{dir: real, asked: dir, exists: exists}, notes, nil
}

// fileID is a folder's device and inode, which no link and no name can
// stand in for.
type fileID struct {
	Dev uint64 `json:"dev"`
	Ino uint64 `json:"ino"`
}

// checkMounted refuses a restore whose job mounts another folder than the
// one checked (#30): the Engine follows a destination's links as the job
// starts, well after restoreFolder followed them, so a link put in the
// place of the folder, or of one above it, in between would send the
// writable mount elsewhere on the server. mounted is what the job says it
// mounts at /restore/<i>; each destination folder is opened again on the
// server, name by name without following a link, and must be that very
// folder. Volumes go by their names.
func (a *Agent) checkMounted(dests []restoreDest, mounted []fileID) error {
	for i, dest := range dests {
		if dest.dir == "" {
			continue
		}
		id, err := dirIDNoLinks(a.hostRoot(), dest.dir)
		if err != nil || i >= len(mounted) || id != mounted[i] || id == (fileID{}) {
			a.Log.Warn("a restore's destination is not the folder that was checked", "destination", dest.dir, "error", err)
			return sentenceError(dest.dir + " changed after it was checked: preview again.")
		}
	}
	return nil
}

// checkRestoreVolume checks a volume a restore writes back into: it exists,
// and it is no part of PocketCloud.
func (a *Agent) checkRestoreVolume(ctx context.Context, name string) error {
	if !volumeName.MatchString(name) {
		return fmt.Errorf("%q is not a volume's name", name)
	}
	if name == resticCache {
		return fmt.Errorf("%s is restic's cache, which no restore writes", name)
	}
	volume, err := a.Docker.InspectVolume(ctx, name)
	if dockerapi.IsNotFound(err) {
		return fmt.Errorf("the volume %s does not exist on this server", name)
	} else if err != nil {
		return fmt.Errorf("the volume %s cannot be inspected: %w", name, err)
	}
	if role := volume.Labels["pocketcloud.role"]; role != "" {
		return fmt.Errorf("%s is PocketCloud's own volume (%s), which no restore writes", name, role)
	}
	own, err := a.ownVolumes(ctx)
	if err != nil {
		return err
	}
	if slices.Contains(own, name) {
		return fmt.Errorf("%s is the agent's own volume, which no restore writes", name)
	}
	return nil
}

// ownVolumes are the volumes the agent's own container mounts.
func (a *Agent) ownVolumes(ctx context.Context) ([]string, error) {
	self := a.ownContainerID(ctx)
	if self == "" {
		return nil, nil
	}
	containers, err := a.Docker.Containers(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, container := range containers {
		if container.ID != self {
			continue
		}
		for _, mount := range container.Mounts {
			if mount.Type == "volume" && mount.Name != "" {
				names = append(names, mount.Name)
			}
		}
	}
	return names, nil
}

// resolveServerTarget follows dir, a folder of the server a restore writes
// into, through root, where the agent sees the server's /, as
// resolveServerDir does. A folder that is not there yet is no error: at the
// first missing part, it says the folder with no link left in what is
// there, the missing names after it, and that it does not exist. A file of
// the server in the way is an error.
func resolveServerTarget(root, dir string) (string, bool, error) {
	return followServerPath(root, dir, false)
}

// resolveServerSource follows a bind mount's source, a folder or a file of
// the server, through root as the Engine follows it, and says where it
// leads; the source as it is when it cannot be followed.
func resolveServerSource(root, source string) string {
	real, _, err := followServerPath(root, source, true)
	if err != nil {
		return source
	}
	return real
}

// followServerPath is resolveServerTarget, which, with endsInFile, takes a
// file of the server at the end of dir too.
func followServerPath(root, dir string, endsInFile bool) (string, bool, error) {
	if _, err := os.Stat(root); err != nil {
		return "", false, fmt.Errorf("the agent cannot see the server's directories at %s, where the server's / is mounted: %w", root, err)
	}
	resolved := "/"
	rest := strings.Split(dir, "/")
	links := 0
	for len(rest) > 0 {
		name := rest[0]
		rest = rest[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			// never above the server's /
			resolved = path.Dir(resolved)
			continue
		}
		next := path.Join(resolved, name)
		file := filepath.Join(root, filepath.FromSlash(next))
		info, err := os.Lstat(file)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// what is missing is made as it is named
			return path.Join(append([]string{next}, rest...)...), false, nil
		case err != nil:
			return "", false, fmt.Errorf("the folder %s cannot be read on this server: %w", dir, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			if links++; links > maxLinks {
				return "", false, fmt.Errorf("the folder %s leads through more than %d symbolic links on this server", dir, maxLinks)
			}
			target, err := os.Readlink(file)
			if err != nil {
				return "", false, fmt.Errorf("the folder %s cannot be read on this server: %w", dir, err)
			}
			if path.IsAbs(target) {
				resolved = "/"
			}
			rest = append(strings.Split(target, "/"), rest...)
			continue
		}
		if !info.IsDir() {
			if endsInFile && !slices.ContainsFunc(rest, func(name string) bool { return name != "" && name != "." }) {
				return next, true, nil
			}
			return "", false, fmt.Errorf("%s is a file on this server, not a folder: a restore writes into a folder", next)
		}
		resolved = next
	}
	return resolved, true, nil
}

// Data claims (#30): a restore writes its destinations, which a backup of
// them reads; neither runs while the other does, on this server.

// dataClaims are the volumes and folders backups read and restores write
// now.
type dataClaims struct {
	mu   sync.Mutex
	held []*dataClaim
}

// dataClaim is one command's hold on data.
type dataClaim struct {
	keys  []string
	write bool
}

// claimsOverlap says whether two claim keys name the same data: the same
// volume, or folders one of which holds the other.
func claimsOverlap(a, b string) bool {
	kindA, nameA, _ := strings.Cut(a, ":")
	kindB, nameB, _ := strings.Cut(b, ":")
	if kindA != kindB {
		return false
	}
	if kindA == "path" {
		return backup.Overlap(nameA, nameB)
	}
	return nameA == nameB
}

// claim holds keys, for writing or reading: a write keeps out every other
// claim of the same data, and a read keeps out writes. A claim that cannot
// be had fails at once, with the data that is held.
func (c *dataClaims) claim(write bool, keys []string) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, held := range c.held {
		if !write && !held.write {
			continue
		}
		for _, key := range keys {
			for _, other := range held.keys {
				if claimsOverlap(key, other) {
					return nil, &claimConflict{key: other, write: held.write}
				}
			}
		}
	}
	claim := &dataClaim{keys: keys, write: write}
	c.held = append(c.held, claim)
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.held = slices.DeleteFunc(c.held, func(other *dataClaim) bool { return other == claim })
	}, nil
}

// claimConflict is data that another command holds.
type claimConflict struct {
	key   string
	write bool
}

func (e *claimConflict) Error() string {
	kind, name, _ := strings.Cut(e.key, ":")
	if kind == "db" {
		// a database of a server's container is said by its name (#53)
		name = path.Base(name)
	}
	if e.write {
		return fmt.Sprintf("A restore into %s is under way: back it up once it ends.", name)
	}
	return fmt.Sprintf("A backup of %s is running: restore when it ends.", name)
}

// backupClaimKeys are the data claims of a backup job: what it mounts, and
// the database a dump reads (#53).
func backupClaimKeys(job backupJobSpec) []string {
	keys := slices.Clone(job.claims)
	for _, mount := range job.mounts {
		source, _ := mount["Source"].(string)
		switch mount["Type"] {
		case "volume":
			keys = append(keys, "volume:"+source)
		case "bind":
			keys = append(keys, "path:"+source)
		}
	}
	return keys
}
