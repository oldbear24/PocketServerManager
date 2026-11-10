package backup

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/testutil/resticcapture"
)

var lockNow = time.Date(2026, 11, 10, 14, 0, 0, 0, time.FixedZone("CET", 3600))

// The fake restic's lines (internal/testutil/fakerestic), and restic's own.
func TestParseLockedReadsResticsFourLines(t *testing.T) {
	stderr := "unable to create lock in backend: repository is already locked exclusively by PID 1 on other-server by root (UID 0, GID 0)\n" +
		"lock was created at 2026-11-10 12:54:59 (5m0.12s ago)\n" +
		"storage ID 0a1b2c3d\n" +
		"the `unlock` command can be used to remove stale locks\n"
	lock := ParseLocked(stderr, lockNow)
	want := Lock{PID: 1, Host: "other-server", User: "root", Exclusive: true,
		Created: lockNow.Add(-(5*time.Minute + 120*time.Millisecond)).UTC(), AgeS: 300, StorageID: "0a1b2c3d"}
	if lock == nil || *lock != want {
		t.Fatalf("lock = %+v, want %+v", lock, want)
	}
	if lock.Created.Location() != time.UTC {
		t.Errorf("created is in %v, want UTC", lock.Created.Location())
	}
	if got := lock.StaleAt(); !got.Equal(want.Created.Add(32 * time.Minute)) {
		t.Errorf("stale at %v", got)
	}
}

func TestParseLockedReadsWhatResticPrinted(t *testing.T) {
	for _, c := range []struct {
		file      string
		run       int
		exclusive bool
		host      string
		pid       int
		age       time.Duration
		storage   string
	}{
		{"backup-locked.txt", 0, true, "pc-k3j9x2m4q8w1z7v", 27095, 15348873386, "784b7d26"},
		{"check-locked.txt", 0, true, "pc-k3j9x2m4q8w1z7v", 22562, 12570619994, "957548b2"},
		{"check-locked.txt", 1, false, "nas", 23062, 21566720445, "4e44bf94"},
	} {
		runs, err := resticcapture.Read(c.file)
		if err != nil {
			t.Fatal(err)
		}
		run := runs[c.run]
		if run.Exit != 11 {
			t.Fatalf("%s: exit %d, want 11", c.file, run.Exit)
		}
		lock := ParseLocked(run.Stderr, lockNow)
		want := Lock{PID: c.pid, Host: c.host, User: "root", Exclusive: c.exclusive,
			Created: lockNow.Add(-c.age).UTC(), AgeS: int64(c.age / time.Second), StorageID: c.storage}
		if lock == nil || *lock != want {
			t.Errorf("%s: lock = %+v, want %+v", c.file, lock, want)
		}
	}
}

func TestParseLockedTakesTheLastLockSaid(t *testing.T) {
	stderr := "unable to create lock in backend: repository is already locked by PID 4 on nas by root (UID 0, GID 0)\n" +
		"lock was created at 2026-11-10 12:00:00 (31m0s ago)\n" +
		"storage ID 11111111\n" +
		"the `unlock` command can be used to remove stale locks\n" +
		"unable to create lock in backend: repository is already locked exclusively by PID 9 on pc-aaaaaaaaaaaaaaa by backup user (UID 1000, GID 1000)\n" +
		"lock was created at 2026-11-10 12:30:00 (1m0s ago)\n" +
		"storage ID 22222222\n"
	lock := ParseLocked(stderr, lockNow)
	if lock == nil || lock.PID != 9 || lock.User != "backup user" || lock.AgeS != 60 || lock.StorageID != "22222222" || !lock.Exclusive {
		t.Fatalf("lock = %+v", lock)
	}
}

func TestParseLockedWithoutTheTimeHasNone(t *testing.T) {
	// the fake restic's single line, and an older agent's run error
	lock := ParseLocked("unable to create lock in backend: repository is already locked by PID 1 on other-server by root (UID 0, GID 0)\n", lockNow)
	if lock == nil || lock.Host != "other-server" || lock.Exclusive || !lock.Created.IsZero() || lock.AgeS != 0 || lock.StorageID != "" {
		t.Fatalf("lock = %+v", lock)
	}
	if !lock.StaleAt().IsZero() {
		t.Errorf("a lock of unknown age is stale at %v", lock.StaleAt())
	}
	raw, err := json.Marshal(lock)
	if err != nil || strings.Contains(string(raw), "created") || strings.Contains(string(raw), "command") {
		t.Errorf("json = %s", raw)
	}
	line := ParseLockLine("restic: Fatal: unable to create lock in backend: repository is already locked exclusively by PID 77 on nas by root (UID 0, GID 0)")
	if line == nil || *line != (Lock{PID: 77, Host: "nas", User: "root", Exclusive: true}) {
		t.Errorf("line = %+v", line)
	}
}

func TestParseLockedReadsAHolderAheadOfTheReader(t *testing.T) {
	// a holder whose clock is ahead: restic's "(… ago)" is negative
	lock := ParseLocked("repository is already locked by PID 3 on nas by root (UID 0, GID 0)\nlock was created at 2026-11-10 13:05:00 (-4m30s ago)\n", lockNow)
	if lock == nil || !lock.Created.Equal(lockNow.Add(4*time.Minute+30*time.Second)) || lock.AgeS != -270 {
		t.Fatalf("lock = %+v", lock)
	}
}

func TestParseLockedIgnoresWhatIsNotALock(t *testing.T) {
	for _, stderr := range []string{
		"",
		"Fatal: wrong password or no key found\n",
		"Fatal: invalid lock file: LoadRaw(<lock/5a09675764>): invalid data returned\n",
		"repository is already locked by PID x on nas by root (UID 0, GID 0)\n",
		"repository is already locked by PID 1 on nas by root\n",
		"lock was created at 2026-11-10 12:00:00 (1m0s ago)\nstorage ID 0a1b2c3d\n",
	} {
		if lock := ParseLocked(stderr, lockNow); lock != nil {
			t.Errorf("%q gave %+v", stderr, lock)
		}
	}
	// lines that are not the next two, or garbled, leave their fields out
	lock := ParseLocked("repository is already locked by PID 1 on nas by root (UID 0, GID 0)\nsomething else\nnot yet\nlock was created at 2026-11-10 12:00:00 (1m0s ago)\n", lockNow)
	if lock == nil || !lock.Created.IsZero() {
		t.Errorf("lock = %+v", lock)
	}
	lock = ParseLocked("repository is already locked by PID 1 on nas by root (UID 0, GID 0)\nlock was created at 2026-11-10 12:00:00 (soon ago)\nstorage ID 0A1B2C3D\n", lockNow)
	if lock == nil || !lock.Created.IsZero() || lock.StorageID != "" {
		t.Errorf("lock = %+v", lock)
	}
}

func TestAJobsHostNamesItsCommand(t *testing.T) {
	if host := JobHost("k3j9x2m4q8w1z7v"); host != "pc-k3j9x2m4q8w1z7v" {
		t.Fatalf("host = %q", host)
	}
	for host, want := range map[string]string{
		"pc-k3j9x2m4q8w1z7v":  "k3j9x2m4q8w1z7v",
		"pc-k3j9x2m4q8w1z7":   "",
		"pc-k3j9x2m4q8w1z7vv": "",
		"pc-K3J9X2M4Q8W1Z7V":  "",
		"nas":                 "",
		"xpc-k3j9x2m4q8w1z7v": "",
		"4f1c2a3b4c5d":        "",
	} {
		if got := CommandOfHost(host); got != want {
			t.Errorf("CommandOfHost(%q) = %q, want %q", host, got, want)
		}
	}
}
