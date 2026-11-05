package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
	"github.com/oldbear24/PocketServerManager/internal/dockerapi"
)

func TestParseMeminfo(t *testing.T) {
	// `free` calls used what is not available, and so does this
	total, used := parseMeminfo(`MemTotal:       16384000 kB
MemFree:         1000000 kB
MemAvailable:    8192000 kB
Buffers:          200000 kB
`)
	if total != 16384000*1024 {
		t.Errorf("total: %d", total)
	}
	if used != 8192000*1024 {
		t.Errorf("used: %d", used)
	}
}

func TestParseCPUTimes(t *testing.T) {
	times := parseCPUTimes("cpu  100 0 50 800 50 0 0 0 7 7\ncpu0 1 2 3 4\nintr 1\n")

	if times.total != 1000 {
		t.Errorf("total: %d (guest time counts only once)", times.total)
	}
	if times.idle != 850 {
		t.Errorf("idle plus iowait: %d", times.idle)
	}
	if got := parseCPUTimes("nonsense"); got != (cpuTimes{}) {
		t.Errorf("%v", got)
	}
}

func TestCPUPercent(t *testing.T) {
	previous := cpuTimes{total: 1000, idle: 800}

	if got := cpuPercentBetween(previous, cpuTimes{total: 1100, idle: 875}); got != 25 {
		t.Errorf("got %f", got)
	}
	if got := cpuPercentBetween(cpuTimes{}, cpuTimes{total: 100}); got != 0 {
		t.Errorf("the first reading has no base: %f", got)
	}
	if got := cpuPercentBetween(previous, previous); got != 0 {
		t.Errorf("no time passed: %f", got)
	}
}

func TestParseLoadavgAndUptime(t *testing.T) {
	load1, load5, load15 := parseLoadavg("0.62 0.71 0.58 2/345 1234\n")
	if load1 != 0.62 || load5 != 0.71 || load15 != 0.58 {
		t.Errorf("%f %f %f", load1, load5, load15)
	}
	if load1, _, _ := parseLoadavg(""); load1 != 0 {
		t.Errorf("empty: %f", load1)
	}

	if got := parseUptime("3600.50 7000.00\n"); got != 3600*time.Second+500*time.Millisecond {
		t.Errorf("uptime: %s", got)
	}
}

func TestParseNetDev(t *testing.T) {
	counters := parseNetDev(`Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 999999    1    0    0    0     0          0         0   999999    1    0    0    0     0       0          0
  eth0: 1000      1    0    0    0     0          0         0   2000      1    0    0    0     0       0          0
  eth1: 500       1    0    0    0     0          0         0   600       1    0    0    0     0       0          0
docker0: 77777    1    0    0    0     0          0         0   77777     1    0    0    0     0       0          0
vethabc: 88888    1    0    0    0     0          0         0   88888     1    0    0    0     0       0          0
br-abcd: 99999    1    0    0    0     0          0         0   99999     1    0    0    0     0       0          0
`)

	if counters.rx != 1500 || counters.tx != 2600 {
		t.Errorf("rx %d, tx %d: only real interfaces count", counters.rx, counters.tx)
	}
}

func TestParseMounts(t *testing.T) {
	mounts := parseMounts(`/dev/sda1 / ext4 rw,relatime 0 0
proc /proc proc rw 0 0
tmpfs /run tmpfs rw 0 0
/dev/sdb1 /srv xfs rw 0 0
overlay /var/lib/docker/overlay2/x/merged overlay rw 0 0
`)

	if len(mounts) != 2 || mounts[0].path != "/" || mounts[1].fsType != "xfs" {
		t.Errorf("%+v", mounts)
	}
}

func TestParseOSRelease(t *testing.T) {
	got := parseOSRelease(`NAME="Ubuntu"
PRETTY_NAME="Ubuntu 24.04.1 LTS"
ID=ubuntu
`)
	if got != "Ubuntu 24.04.1 LTS" {
		t.Errorf("%q", got)
	}
}

func TestParseLogLine(t *testing.T) {
	line := parseLogLine("2026-10-16T13:42:10.123456789Z hello world\r")
	if line.GetText() != "hello world" {
		t.Errorf("text: %q", line.GetText())
	}
	if line.GetTs().AsTime().Year() != 2026 {
		t.Errorf("time: %v", line.GetTs())
	}

	// a line without Docker's timestamp stays as it is
	if got := parseLogLine("no timestamp here").GetText(); got != "no timestamp here" {
		t.Errorf("%q", got)
	}
}

func TestEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")

	err := writeEnvFile(path, "PLAIN=1", map[string]string{
		"Z_LAST":  "simple",
		"A_FIRST": `pa$$ w'rd #1`,
	})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	want := "PLAIN=1\nA_FIRST=\"pa\\$\\$ w'rd #1\"\nZ_LAST=\"simple\"\n"
	if string(raw) != want {
		t.Errorf("got %q, want %q", raw, want)
	}

	// after a deploy only the plain environment stays behind
	if err := writeEnvFile(path, "PLAIN=1\n", nil); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	if string(raw) != "PLAIN=1\n" {
		t.Errorf("%q", raw)
	}
}

func TestStackNames(t *testing.T) {
	a := &Agent{Config: Config{StateDir: "/state"}}

	for _, good := range []string{"demo", "my-stack", "a1_b"} {
		if _, err := a.stackDir(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}

	// a name ends up in a path and on a command line
	for _, bad := range []string{"", "..", "../../etc", "a/b", "A", "-rf", "a b", "a;b"} {
		if _, err := a.stackDir(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestVolumeAt(t *testing.T) {
	info := &dockerapi.ContainerInspect{HostConfig: map[string]any{
		"Binds": []any{
			"/var/run/docker.sock:/var/run/docker.sock",
			"pocketcloud-agent:/var/lib/pocketcloud",
			"/:/host:ro,rslave",
		},
	}}

	if got := volumeAt(info, "/var/lib/pocketcloud"); got != "pocketcloud-agent" {
		t.Errorf("got %q", got)
	}
	if got := volumeAt(info, "/host"); got != "/" {
		t.Errorf("a bind with options: %q", got)
	}
	if got := volumeAt(info, "/missing"); got != "" {
		t.Errorf("got %q", got)
	}

	mounted := &dockerapi.ContainerInspect{HostConfig: map[string]any{
		"Mounts": []any{map[string]any{"Type": "volume", "Source": "from-compose", "Target": "/var/lib/pocketcloud"}},
	}}
	if got := volumeAt(mounted, "/var/lib/pocketcloud"); got != "from-compose" {
		t.Errorf("got %q", got)
	}
}

func TestLogRing(t *testing.T) {
	ring := newLogRing(3)

	if got := ring.last(5); len(got) != 0 {
		t.Errorf("%v", got)
	}

	for _, line := range []string{"a", "b"} {
		if _, err := ring.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if got := ring.last(5); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("%v", got)
	}

	for _, line := range []string{"c", "d", "e"} {
		if _, err := ring.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if got := ring.last(5); !slices.Equal(got, []string{"c", "d", "e"}) {
		t.Errorf("the oldest lines must go first: %v", got)
	}
	if got := ring.last(2); !slices.Equal(got, []string{"d", "e"}) {
		t.Errorf("%v", got)
	}
}

func TestCommandStorePersists(t *testing.T) {
	dir := t.TempDir()

	store, err := openCommandStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.start("running", "image.pull"); err != nil {
		t.Fatal(err)
	}
	if err := store.start("done", "image.pull"); err != nil {
		t.Fatal(err)
	}
	if err := store.setState("done", stateSucceeded, "", map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}

	// what a restart sees
	reopened, err := openCommandStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	entry, ok := reopened.get("running")
	if !ok || entry.State != stateStarted || entry.finished() {
		t.Errorf("%+v", entry)
	}

	entry, ok = reopened.get("done")
	if !ok || !entry.finished() || entry.Result["ok"] != true {
		t.Errorf("%+v", entry)
	}

	finished := reopened.recentlyFinished()
	if len(finished) != 1 || finished[0].GetCommandId() != "done" || finished[0].GetState() != agentv1.CommandState_COMMAND_STATE_SUCCEEDED {
		t.Errorf("only finished commands go to the panel: %v", finished)
	}

	if _, ok := reopened.get("unknown"); ok {
		t.Error("an unknown id must not be found")
	}
}

func TestCommandStoreIsBounded(t *testing.T) {
	store, err := openCommandStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	store.order = make([]string, 0, storeCapacity+1)
	for i := range storeCapacity {
		id := "old" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + time.Duration(i).String()
		store.entries[id] = &commandEntry{ID: id}
		store.order = append(store.order, id)
	}
	first := store.order[0]

	if err := store.start("new", "x"); err != nil {
		t.Fatal(err)
	}

	if len(store.order) != storeCapacity {
		t.Errorf("%d entries", len(store.order))
	}
	if _, ok := store.get(first); ok {
		t.Error("the oldest entry must go")
	}
	if _, ok := store.get("new"); !ok {
		t.Error("the new entry is missing")
	}
}

func TestCorruptedStoreDoesNotStopTheAgent(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, storeFile), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := openCommandStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.entries) != 0 {
		t.Errorf("%v", store.entries)
	}
}

func TestSplitList(t *testing.T) {
	if got := splitList(" docker, edge ,,backup "); !slices.Equal(got, []string{"docker", "edge", "backup"}) {
		t.Errorf("%v", got)
	}
	if got := splitList(""); len(got) != 0 {
		t.Errorf("%v", got)
	}
}

func TestALogLevelAlwaysFallsBack(t *testing.T) {
	cases := map[int64]time.Duration{
		0:         15 * time.Minute,
		-5:        15 * time.Minute,
		30:        30 * time.Second,
		900:       15 * time.Minute,
		7 * 86400: 24 * time.Hour,
	}
	for seconds, want := range cases {
		if got := logLevelDuration(seconds); got != want {
			t.Errorf("logLevelDuration(%d) = %s, want %s", seconds, got, want)
		}
	}
}
