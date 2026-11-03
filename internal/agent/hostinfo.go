package agent

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
)

// hostInfo reads the host's state from the read-only host mount: the container's
// own /proc shows the container's view of a few of these values, not the host's.
type hostInfo struct {
	root string

	mu      sync.Mutex
	prevCPU cpuTimes
	prevNet netCounters
}

type cpuTimes struct {
	total uint64
	idle  uint64
}

type netCounters struct {
	rx, tx uint64
	seen   bool
}

func newHostInfo(root string) *hostInfo {
	// outside of a container (development) there is no host mount
	if _, err := os.Stat(filepath.Join(root, "proc", "stat")); err != nil {
		root = "/"
	}
	return &hostInfo{root: root}
}

func (h *hostInfo) proc(name string) string {
	return filepath.Join(h.root, "proc", name)
}

// vitals returns the compact summary for a heartbeat.
func (h *hostInfo) vitals() *agentv1.HostVitals {
	vitals := &agentv1.HostVitals{}

	if raw, err := os.ReadFile(h.proc("uptime")); err == nil {
		vitals.Uptime = durationpb.New(parseUptime(string(raw)))
	}

	if raw, err := os.ReadFile(h.proc("loadavg")); err == nil {
		vitals.Load1, vitals.Load5, vitals.Load15 = parseLoadavg(string(raw))
	}

	vitals.CpuPercent = h.cpuPercent()

	if raw, err := os.ReadFile(h.proc("meminfo")); err == nil {
		vitals.MemTotalBytes, vitals.MemUsedBytes = parseMeminfo(string(raw))
	}

	if total, free, err := diskUsage(h.root); err == nil {
		vitals.DiskTotalBytes = total
		vitals.DiskUsedBytes = total - free
	}

	return vitals
}

// cpuPercent is the usage since the previous call; the first call has no base.
func (h *hostInfo) cpuPercent() float64 {
	raw, err := os.ReadFile(h.proc("stat"))
	if err != nil {
		return 0
	}

	current := parseCPUTimes(string(raw))

	h.mu.Lock()
	defer h.mu.Unlock()

	previous := h.prevCPU
	h.prevCPU = current

	return cpuPercentBetween(previous, current)
}

func cpuPercentBetween(previous, current cpuTimes) float64 {
	if previous.total == 0 || current.total <= previous.total {
		return 0
	}

	total := float64(current.total - previous.total)
	idle := float64(current.idle - previous.idle)

	return (total - idle) / total * 100
}

func parseCPUTimes(stat string) cpuTimes {
	line, _, _ := strings.Cut(stat, "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuTimes{}
	}

	var times cpuTimes
	for i, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			continue
		}
		// guest time is already included in user and nice
		if i < 8 {
			times.total += value
		}
		// idle + iowait
		if i == 3 || i == 4 {
			times.idle += value
		}
	}

	return times
}

func parseUptime(content string) time.Duration {
	fields := strings.Fields(content)
	if len(fields) == 0 {
		return 0
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

func parseLoadavg(content string) (load1, load5, load15 float64) {
	fields := strings.Fields(content)
	if len(fields) < 3 {
		return 0, 0, 0
	}
	load1, _ = strconv.ParseFloat(fields[0], 64)
	load5, _ = strconv.ParseFloat(fields[1], 64)
	load15, _ = strconv.ParseFloat(fields[2], 64)
	return load1, load5, load15
}

// parseMeminfo returns the total and the used memory; used means total minus
// MemAvailable, like `free` and `top` do.
func parseMeminfo(content string) (total, used uint64) {
	var available uint64
	values := map[string]*uint64{"MemTotal:": &total, "MemAvailable:": &available}

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		if target, ok := values[fields[0]]; ok {
			if value, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				*target = value * 1024
			}
		}
	}

	if total < available {
		return total, 0
	}

	return total, total - available
}

// netDelta returns the bytes received and sent since the previous call, for all
// interfaces except loopback and the virtual ones Docker creates.
func (h *hostInfo) netDelta() (rx, tx uint64) {
	raw, err := os.ReadFile(h.proc("net/dev"))
	if err != nil {
		return 0, 0
	}

	current := parseNetDev(string(raw))

	h.mu.Lock()
	defer h.mu.Unlock()

	previous := h.prevNet
	h.prevNet = current

	if !previous.seen || current.rx < previous.rx || current.tx < previous.tx {
		return 0, 0
	}

	return current.rx - previous.rx, current.tx - previous.tx
}

func parseNetDev(content string) netCounters {
	counters := netCounters{seen: true}

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		name, rest, found := strings.Cut(scanner.Text(), ":")
		if !found {
			continue
		}

		name = strings.TrimSpace(name)
		if name == "lo" || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "docker") {
			continue
		}

		fields := strings.Fields(rest)
		if len(fields) < 16 {
			continue
		}

		rx, errRx := strconv.ParseUint(fields[0], 10, 64)
		tx, errTx := strconv.ParseUint(fields[8], 10, 64)
		if errRx == nil && errTx == nil {
			counters.rx += rx
			counters.tx += tx
		}
	}

	return counters
}

// mounts returns the usage of the host's real file systems.
func (h *hostInfo) mounts() []*agentv1.DiskUsage {
	raw, err := os.ReadFile(h.proc("mounts"))
	if err != nil {
		return nil
	}

	usage := []*agentv1.DiskUsage{}
	seen := map[string]bool{}

	for _, mount := range parseMounts(string(raw)) {
		if seen[mount.device] {
			continue
		}
		seen[mount.device] = true

		total, free, err := diskUsage(filepath.Join(h.root, mount.path))
		if err != nil || total == 0 {
			continue
		}

		usage = append(usage, &agentv1.DiskUsage{
			Mount:      mount.path,
			Device:     mount.device,
			FsType:     mount.fsType,
			UsedBytes:  total - free,
			TotalBytes: total,
		})
	}

	return usage
}

type mountEntry struct {
	device, path, fsType string
}

var realFilesystems = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true,
	"f2fs": true, "vfat": true, "ntfs": true, "ntfs3": true, "exfat": true, "nfs": true, "nfs4": true, "cifs": true,
}

func parseMounts(content string) []mountEntry {
	entries := []mountEntry{}

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || !realFilesystems[fields[2]] {
			continue
		}
		entries = append(entries, mountEntry{device: fields[0], path: fields[1], fsType: fields[2]})
	}

	return entries
}

// facts returns the rarely changing properties of the host.
func (h *hostInfo) facts() *agentv1.HostFacts {
	facts := &agentv1.HostFacts{
		Arch:     runtime.GOARCH,
		CpuCores: int32(runtime.NumCPU()), //nolint:gosec // a small number
		Os:       h.osRelease(),
	}

	if raw, err := os.ReadFile(h.proc("sys/kernel/osrelease")); err == nil {
		facts.Kernel = strings.TrimSpace(string(raw))
	}
	if raw, err := os.ReadFile(h.proc("sys/kernel/hostname")); err == nil {
		facts.Hostname = strings.TrimSpace(string(raw))
	}
	if raw, err := os.ReadFile(h.proc("meminfo")); err == nil {
		facts.MemBytes, _ = parseMeminfo(string(raw))
	}

	facts.Ip4, facts.Ip6 = primaryAddresses()

	return facts
}

func (h *hostInfo) osRelease() string {
	raw, err := os.ReadFile(filepath.Join(h.root, "etc", "os-release"))
	if err != nil {
		return runtime.GOOS
	}

	return parseOSRelease(string(raw))
}

func parseOSRelease(content string) string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		if value, ok := strings.CutPrefix(scanner.Text(), "PRETTY_NAME="); ok {
			return strings.Trim(value, `"'`)
		}
	}
	return "Linux"
}

// primaryAddresses returns the addresses used for outgoing traffic. No packet is
// sent: connecting a UDP socket only makes the kernel pick a source address.
func primaryAddresses() (ip4, ip6 string) {
	if conn, err := net.Dial("udp4", "192.0.2.1:9"); err == nil {
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			ip4 = addr.IP.String()
		}
		conn.Close()
	}
	if conn, err := net.Dial("udp6", "[2001:db8::1]:9"); err == nil {
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			ip6 = addr.IP.String()
		}
		conn.Close()
	}
	return ip4, ip6
}
