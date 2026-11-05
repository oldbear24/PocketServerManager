package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/testutil"
)

// fakeHostRoot writes what an agent finds at /host on a real server. The /proc
// files of the host itself differ from the plain ones on purpose: inside a
// container /proc/net and /proc/mounts lead to the container's own view.
func fakeHostRoot(t *testing.T) string {
	t.Helper()

	root := t.TempDir()

	files := map[string]string{
		"proc/stat":    "cpu  1000 0 500 8500 0 0 0 0 0 0\ncpu0 1000 0 500 8500 0 0 0 0 0 0\n",
		"proc/meminfo": "MemTotal:       8000000 kB\nMemFree:         500000 kB\nMemAvailable:   5000000 kB\n",
		"proc/loadavg": "0.62 0.71 0.58 1/234 5678\n",
		"proc/uptime":  "3600.50 7000.00\n",
		"proc/net/dev": "Inter-|   Receive                                                |  Transmit\n" +
			" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
			"  eth0: 11111111    1    0    0    0     0          0         0   22222222    1    0    0    0     0       0          0\n",
		"proc/1/net/dev": "Inter-|   Receive                                                |  Transmit\n" +
			" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
			"  eth0: 1000      1    0    0    0     0          0         0   2000      1    0    0    0     0       0          0\n",
		"proc/mounts":   "overlay / overlay rw 0 0\n/dev/sdz9 /host ext4 ro 0 0\n",
		"proc/1/mounts": "/dev/sda1 / ext4 rw,relatime 0 0\nproc /proc proc rw 0 0\n",
		"etc/hostname":  "real-host\n",
	}

	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

func TestMetricsReachTheRangeAPI(t *testing.T) {
	h := newHarness(t)

	h.docker.AddContainer("web", "nginx:1", nil)
	h.docker.SetLoad("web", 12.5, 300<<20)

	config := h.agentConfig("metered", h.token("metered", schema.HostRoleDocker))
	config.HostRoot = fakeHostRoot(t)
	config.MetricsInterval = 30 * time.Millisecond

	h.start(config)

	var hostID string
	h.waitFor("the agent to connect", func() bool {
		found, err := h.app.FindFirstRecordByData(schema.Hosts, "name", "metered")
		if err != nil || !found.GetBool("stream_open") {
			return false
		}
		hostID = found.Id
		return true
	})

	_, viewer := testutil.NewUser(t, h.app, "viewer@example.com", schema.RoleViewer)

	get := func(url string, into any) int {
		t.Helper()
		status, body := testutil.Do(t, h.handler, testutil.Request{Method: http.MethodGet, URL: url, Token: viewer})
		if status == http.StatusOK && into != nil {
			if err := json.Unmarshal([]byte(body), into); err != nil {
				t.Fatalf("%s: %v", body, err)
			}
		}
		return status
	}

	type chart struct {
		Points struct {
			ResolutionS int       `json:"resolution_s"`
			T           []int64   `json:"t"`
			Mem         []int64   `json:"mem"`
			MemTotal    []int64   `json:"mem_total"`
			Load1       []float64 `json:"load1"`
			NetIn       []float64 `json:"net_in"`
		} `json:"points"`
		Mounts []struct {
			Mount  string `json:"mount"`
			Device string `json:"device"`
			Total  int64  `json:"total"`
		} `json:"mounts"`
	}

	var host chart
	h.waitFor("host samples", func() bool {
		return get("/api/pc/metrics/hosts/"+hostID+"?range=1h", &host) == http.StatusOK && len(host.Points.T) > 0
	})

	if host.Points.ResolutionS != 10 {
		t.Errorf("an hour must use 10 s: %d", host.Points.ResolutionS)
	}
	// total minus MemAvailable, the way free and top count it
	if host.Points.MemTotal[0] != 8000000*1024 || host.Points.Mem[0] != 3000000*1024 {
		t.Errorf("memory: %d of %d", host.Points.Mem[0], host.Points.MemTotal[0])
	}
	if host.Points.Load1[0] != 0.62 {
		t.Errorf("load: %v", host.Points.Load1)
	}

	// the disks are those of process 1, the host's, and not the container's mount table
	if len(host.Mounts) != 1 || host.Mounts[0].Mount != "/" || host.Mounts[0].Device != "/dev/sda1" {
		t.Errorf("mounts: %+v", host.Mounts)
	}

	// the first sample has no base for traffic, and later ones see no change: the
	// counters of the container's own network in proc/net/dev would show up as a jump
	for _, value := range host.Points.NetIn {
		if value != 0 {
			t.Errorf("traffic came from the wrong network namespace: %v", host.Points.NetIn)
			break
		}
	}

	// the host's record has what the heartbeat brings
	record := h.host(hostID)
	h.waitFor("the load average", func() bool {
		record = h.host(hostID)
		return record.GetFloat("load15") == 0.58
	})
	if record.GetFloat("load5") != 0.71 || record.GetString("hostname") != "real-host" {
		t.Errorf("host record: load5 %v, hostname %q", record.GetFloat("load5"), record.GetString("hostname"))
	}

	web := h.docker.Find("web")

	var values map[string]struct {
		CPU      float64 `json:"cpu"`
		Mem      int64   `json:"mem"`
		MemLimit int64   `json:"mem_limit"`
	}
	h.waitFor("container values", func() bool {
		if get("/api/pc/metrics/hosts/"+hostID+"/containers", &values) != http.StatusOK {
			return false
		}
		// the first sample of a container has no base for its CPU usage
		return values[web.ID].CPU > 0
	})

	// the fake engine has four CPUs: an eighth of the host is half a core, on the
	// scale of docker stats
	if value := values[web.ID]; value.CPU != 50 || value.Mem != 300<<20 || value.MemLimit != 1<<30 {
		t.Errorf("container: %+v", value)
	}

	var container struct {
		T   []int64   `json:"t"`
		CPU []float64 `json:"cpu"`
	}
	if status := get(fmt.Sprintf("/api/pc/metrics/hosts/%s/containers/%s?range=1h", hostID, web.ID), &container); status != http.StatusOK || len(container.T) == 0 {
		t.Errorf("container range: %d %+v", status, container)
	}

	// the resolution follows from the range, and nothing else is accepted
	if err := h.panel.Metrics.Rollup(); err != nil {
		t.Fatal(err)
	}
	var day chart
	if get("/api/pc/metrics/hosts/"+hostID+"?range=24h", &day) != http.StatusOK || day.Points.ResolutionS != 60 {
		t.Errorf("a day must use 1 minute: %+v", day.Points)
	}
	var week chart
	if get("/api/pc/metrics/hosts/"+hostID+"?range=7d", &week) != http.StatusOK || week.Points.ResolutionS != 3600 {
		t.Errorf("a week must use 1 hour: %+v", week.Points)
	}
	if status := get("/api/pc/metrics/hosts/"+hostID+"?range=9y", nil); status != http.StatusBadRequest {
		t.Errorf("an unknown range: %d", status)
	}
	if status := get("/api/pc/metrics/hosts/nobody?range=1h", nil); status != http.StatusNotFound {
		t.Errorf("an unknown host: %d", status)
	}

	// signing in is needed
	if status, _ := testutil.Do(t, h.handler, testutil.Request{Method: http.MethodGet, URL: "/api/pc/metrics/hosts/" + hostID}); status != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", status)
	}

	// metrics are not records: the database of the panel holds no samples
	for _, collection := range []string{"metrics", "samples"} {
		if _, err := h.app.FindCollectionByNameOrId(collection); err == nil {
			t.Errorf("a collection %s exists", collection)
		}
	}
}

func TestRemovingAHostRemovesItsHistory(t *testing.T) {
	h := newHarness(t)

	h.docker.AddContainer("web", "nginx:1", nil)

	config := h.agentConfig("short-lived", h.token("short-lived", schema.HostRoleDocker))
	config.HostRoot = fakeHostRoot(t)
	config.MetricsInterval = 30 * time.Millisecond

	running := h.start(config)

	var hostID string
	h.waitFor("samples", func() bool {
		found, err := h.app.FindFirstRecordByData(schema.Hosts, "name", "short-lived")
		if err != nil {
			return false
		}
		hostID = found.Id
		points, err := h.panel.Metrics.HostRange(hostID, metricsHour)
		return err == nil && len(points.T) > 0
	})

	if err := running.stop(); err != nil {
		t.Fatal(err)
	}

	if err := h.app.Delete(h.host(hostID)); err != nil {
		t.Fatal(err)
	}

	points, err := h.panel.Metrics.HostRange(hostID, metricsHour)
	if err != nil {
		t.Fatal(err)
	}
	if len(points.T) != 0 {
		t.Errorf("%d points are left", len(points.T))
	}
}
