package backup

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Lock is a restic lock as restic names it when a command finds the
// repository locked (#31): restic's exit code 11, after its four lines
//
//	unable to create lock in backend: repository is already locked exclusively by PID 1 on pc-k3j9x2m4q8w1z7v by root (UID 0, GID 0)
//	lock was created at 2026-11-10 13:18:55 (12.040098552s ago)
//	storage ID 957548b2
//	the `unlock` command can be used to remove stale locks
//
// A run that failed on a lock, and a check that could not lock the repository,
// carry it. restic writes a new lock file each time its holder refreshes the
// lock, every 5 minutes, so Created is when the holder last refreshed it, and
// StorageID changes with each refresh.
type Lock struct {
	PID       int    `json:"pid"`
	Host      string `json:"host"`
	User      string `json:"user"`
	Exclusive bool   `json:"exclusive"`
	// Created is the reader's now minus restic's "(… ago)", in UTC. The wall
	// time restic prints has no zone, so it is never read. Zero for a lock
	// parsed from the holder line alone, as older agents gave it.
	Created time.Time `json:"created,omitzero"`
	// AgeS is restic's "(… ago)" in whole seconds, as the reader saw it.
	// Negative when the holder's clock is ahead of the reader's.
	AgeS int64 `json:"age_s,omitempty"`
	// StorageID is the first 8 hex digits of the lock file's name.
	StorageID string `json:"storage_id,omitempty"`

	// The panel fills these when Host is a job's, pc-<command id>, and that
	// command exists: the command that holds the lock, and its server.
	Command         string `json:"command,omitempty"`
	CommandType     string `json:"command_type,omitempty"`
	CommandLabel    string `json:"command_label,omitempty"`
	CommandHost     string `json:"command_host,omitempty"`
	CommandHostName string `json:"command_host_name,omitempty"`
}

// StaleAfter is how long after its last refresh restic counts a lock as stale
// on any server: restic unlock removes it then, and no sooner unless its
// holder ran on the same host and is gone (restic 0.17.3's
// StaleLockTimeout).
const StaleAfter = 30 * time.Minute

// StaleMargin is added to StaleAfter before a retry that unlocks, for the
// clocks of two servers that differ a little.
const StaleMargin = 2 * time.Minute

// StaleAt is when a retry's restic unlock removes the lock if its holder is
// gone: StaleAfter and StaleMargin after Created. Zero when Created is not
// known.
func (l *Lock) StaleAt() time.Time {
	if l == nil || l.Created.IsZero() {
		return time.Time{}
	}
	return l.Created.Add(StaleAfter + StaleMargin)
}

// JobHostPrefix starts the hostname of every backup job container: the job of
// command X is "pc-X", so that a lock names the command that holds it.
const JobHostPrefix = "pc-"

// JobHost is the hostname of the job containers of a command.
func JobHost(commandID string) string { return JobHostPrefix + commandID }

var jobHost = regexp.MustCompile(`^pc-([a-z0-9]{15})$`)

// CommandOfHost is the id of the command whose job has the hostname host, or
// "" when host is not a job's.
func CommandOfHost(host string) string {
	if m := jobHost.FindStringSubmatch(host); m != nil {
		return m[1]
	}
	return ""
}

var (
	// the user, and the host, may be empty: restic 0.17.3 leaves the user
	// out when it cannot look it up, as for a uid with no passwd entry and
	// no $USER ("on nas by  (UID 0, GID 0)")
	lockHolder  = regexp.MustCompile(`is already locked (exclusively )?by PID (\d+) on (\S*) by (.*?) \(UID \d+, GID \d+\)`)
	lockCreated = regexp.MustCompile(`^lock was created at \S+ \S+ \((.+) ago\)$`)
	lockStorage = regexp.MustCompile(`^storage ID ([0-9a-f]{8})$`)
)

// ParseLocked reads the lock from restic's error output, and dates it from
// now. It is nil unless a line names the holder; with several, the last one
// counts, with the two lines after it.
func ParseLocked(stderr string, now time.Time) *Lock {
	lines := strings.Split(stderr, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		lock := ParseLockLine(lines[i])
		if lock == nil {
			continue
		}
		for _, line := range lines[i+1 : min(i+3, len(lines))] {
			line = strings.TrimSpace(line)
			if m := lockCreated.FindStringSubmatch(line); m != nil {
				if age, err := time.ParseDuration(m[1]); err == nil {
					lock.Created = now.Add(-age).UTC()
					lock.AgeS = int64(age / time.Second)
				}
			} else if m := lockStorage.FindStringSubmatch(line); m != nil {
				lock.StorageID = m[1]
			}
		}
		return lock
	}
	return nil
}

// ParseLockLine reads the holder from the line that names it alone, as older
// agents' run errors give it: no time and no storage ID. It is nil for any
// other line.
func ParseLockLine(line string) *Lock {
	m := lockHolder.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	pid, err := strconv.Atoi(m[2])
	if err != nil {
		return nil
	}
	return &Lock{PID: pid, Host: m[3], User: m[4], Exclusive: m[1] != ""}
}
