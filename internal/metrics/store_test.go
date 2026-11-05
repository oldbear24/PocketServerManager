package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
)

// start is a moment on a whole hour, which keeps the buckets of a test obvious.
var start = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func open(t testing.TB) (*Store, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "metrics.db")

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	return store, path
}

func hostSample(at time.Time, cpu float64, mem uint64) *agentv1.MetricSample {
	return &agentv1.MetricSample{
		Ts:             timestamppb.New(at),
		CpuPercent:     cpu,
		MemBytes:       mem,
		MemTotalBytes:  8 << 30,
		DiskUsedBytes:  50 << 30,
		DiskTotalBytes: 160 << 30,
		NetInBytes:     1000,
		NetOutBytes:    5000,
		Load1:          0.62,
		Mounts: []*agentv1.DiskUsage{
			{Mount: "/", Device: "/dev/sda1", FsType: "ext4", UsedBytes: 50 << 30, TotalBytes: 160 << 30},
		},
	}
}

func containerSample(at time.Time, id string, cpu float64, mem uint64) *agentv1.MetricSample {
	return &agentv1.MetricSample{
		Ts:            timestamppb.New(at),
		ContainerId:   id,
		CpuPercent:    cpu,
		MemBytes:      mem,
		MemTotalBytes: 1 << 30,
		NetInBytes:    100,
		NetOutBytes:   200,
	}
}

func TestWriteAndReadAnHour(t *testing.T) {
	store, _ := open(t)

	store.Now = func() time.Time { return start.Add(time.Minute) }

	for i := range 6 {
		at := start.Add(time.Duration(i) * 10 * time.Second)
		err := store.Write("h1", []*agentv1.MetricSample{hostSample(at, float64(10+i), uint64(i+1)<<30), containerSample(at, "c1", 1.5, 200<<20)})
		if err != nil {
			t.Fatal(err)
		}
	}

	points, err := store.HostRange("h1", Windows["1h"])
	if err != nil {
		t.Fatal(err)
	}

	if points.ResolutionS != Res10s || len(points.T) != 6 {
		t.Fatalf("resolution %d, %d points", points.ResolutionS, len(points.T))
	}
	if points.T[0] != start.Unix() || points.T[5] != start.Add(50*time.Second).Unix() {
		t.Errorf("times: %v", points.T)
	}
	if points.CPU[3] != 13 || points.Mem[1] != 2<<30 || points.Load1[0] != 0.62 {
		t.Errorf("values: %+v", points)
	}
	// a 10 s sample holds 10 seconds of traffic
	if points.NetIn[0] != 100 || points.NetOut[0] != 500 {
		t.Errorf("traffic: %v %v", points.NetIn[0], points.NetOut[0])
	}
	if points.DiskTotal[0] != 160<<30 {
		t.Errorf("disk: %v", points.DiskTotal[0])
	}

	container, err := store.ContainerRange("h1", "c1", Windows["1h"])
	if err != nil {
		t.Fatal(err)
	}
	if len(container.T) != 6 || container.CPU[0] != 1.5 || container.Mem[0] != 200<<20 || len(container.Disk) != 0 {
		t.Errorf("container: %+v", container)
	}

	other, err := store.HostRange("nobody", Windows["1h"])
	if err != nil {
		t.Fatal(err)
	}
	if len(other.T) != 0 {
		t.Errorf("an unknown host has points: %+v", other)
	}
}

func TestRollupMeansValuesAndSumsTraffic(t *testing.T) {
	store, _ := open(t)

	clock := start
	store.Now = func() time.Time { return clock }

	// two whole minutes of samples, and one more in the minute that is not over yet
	for i := range 13 {
		at := start.Add(time.Duration(i) * 10 * time.Second)
		clock = at
		cpu := 10.0
		if i >= 6 {
			cpu = 30
		}
		if err := store.Write("h1", []*agentv1.MetricSample{hostSample(at, cpu, 1<<30), containerSample(at, "c1", cpu/10, 100<<20)}); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.Rollup(); err != nil {
		t.Fatal(err)
	}

	points, err := store.HostRange("h1", Windows["24h"])
	if err != nil {
		t.Fatal(err)
	}

	if points.ResolutionS != Res1m || len(points.T) != 2 {
		t.Fatalf("resolution %d, %d points: %v", points.ResolutionS, len(points.T), points.T)
	}
	if points.CPU[0] != 10 || points.CPU[1] != 30 {
		t.Errorf("means: %v", points.CPU)
	}
	if points.Mem[0] != 1<<30 || points.MemTotal[0] != 8<<30 {
		t.Errorf("memory: %v %v", points.Mem, points.MemTotal)
	}
	// six samples of 1000 bytes in 60 seconds
	if points.NetIn[0] != 100 {
		t.Errorf("traffic: %v", points.NetIn)
	}

	// an hour later the finished hour is rolled up too, and the run is repeatable
	clock = start.Add(time.Hour + time.Second)
	for range 2 {
		if err := store.Rollup(); err != nil {
			t.Fatal(err)
		}
	}

	week, err := store.HostRange("h1", Windows["7d"])
	if err != nil {
		t.Fatal(err)
	}
	if week.ResolutionS != Res1h || len(week.T) != 1 || week.T[0] != start.Unix() {
		t.Fatalf("resolution %d, points %v", week.ResolutionS, week.T)
	}
	// the means of three minutes: 10, 30 and the single sample of the third
	if week.CPU[0] != 23.33 {
		t.Errorf("the hour's mean: %v", week.CPU)
	}
	// the sum of all 13 samples, spread over the hour
	if want := 13.0 * 1000 / Res1h; week.NetIn[0] != want {
		t.Errorf("the hour's traffic: %v, want %v", week.NetIn, want)
	}
}

func TestRetentionRemovesWhatIsTooOld(t *testing.T) {
	store, _ := open(t)

	clock := start
	store.Now = func() time.Time { return clock }

	if err := store.Write("h1", []*agentv1.MetricSample{hostSample(start, 10, 1<<30), containerSample(start, "gone", 1, 1)}); err != nil {
		t.Fatal(err)
	}

	count := func(res int) int {
		t.Helper()
		var n int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM samples WHERE res = ?`, res).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	clock = start.Add(2 * time.Hour)
	if err := store.Rollup(); err != nil {
		t.Fatal(err)
	}
	if count(Res10s) == 0 || count(Res1m) == 0 || count(Res1h) == 0 {
		t.Fatalf("rows: %d %d %d", count(Res10s), count(Res1m), count(Res1h))
	}

	clock = start.Add(Keep10s + time.Hour)
	if err := store.Rollup(); err != nil {
		t.Fatal(err)
	}
	if count(Res10s) != 0 || count(Res1m) == 0 {
		t.Fatalf("after a day: %d %d", count(Res10s), count(Res1m))
	}

	clock = start.Add(Keep1m + time.Hour)
	if err := store.Rollup(); err != nil {
		t.Fatal(err)
	}
	if count(Res1m) != 0 || count(Res1h) == 0 {
		t.Fatalf("after a week: %d %d", count(Res1m), count(Res1h))
	}

	clock = start.Add(Keep1h + time.Hour)
	if err := store.Rollup(); err != nil {
		t.Fatal(err)
	}
	if count(Res1h) != 0 {
		t.Fatalf("after 90 days: %d", count(Res1h))
	}

	var series int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM series`).Scan(&series); err != nil {
		t.Fatal(err)
	}
	if series != 0 {
		t.Errorf("%d series without samples are left", series)
	}

	// and a series that comes back is usable again
	if err := store.Write("h1", []*agentv1.MetricSample{hostSample(clock, 10, 1<<30)}); err != nil {
		t.Fatal(err)
	}
	points, err := store.HostRange("h1", Windows["1h"])
	if err != nil {
		t.Fatal(err)
	}
	if len(points.T) != 1 {
		t.Errorf("points after coming back: %v", points.T)
	}
}

func TestLatestValues(t *testing.T) {
	store, _ := open(t)

	clock := start
	store.Now = func() time.Time { return clock }

	if err := store.Write("h1", []*agentv1.MetricSample{hostSample(start, 10, 1<<30), containerSample(start, "stopped", 3, 1<<20), containerSample(start, "c1", 1, 1<<20)}); err != nil {
		t.Fatal(err)
	}

	clock = start.Add(5 * time.Minute)
	if err := store.Write("h1", []*agentv1.MetricSample{hostSample(clock, 10, 1<<30), containerSample(clock, "c1", 2.25, 2<<20)}); err != nil {
		t.Fatal(err)
	}

	values, err := store.LatestContainers("h1")
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values["c1"].CPU != 2.25 || values["c1"].Mem != 2<<20 || values["c1"].MemLimit != 1<<30 {
		t.Errorf("containers: %+v", values)
	}

	mounts, err := store.Mounts("h1")
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 || mounts[0].Mount != "/" || mounts[0].Device != "/dev/sda1" || mounts[0].FSType != "ext4" || mounts[0].Total != 160<<30 {
		t.Errorf("mounts: %+v", mounts)
	}

	// a mount that has disappeared drops out after a few minutes
	clock = clock.Add(time.Hour)
	mounts, err = store.Mounts("h1")
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 0 {
		t.Errorf("stale mounts: %+v", mounts)
	}
}

func TestAWrongAgentClockIsNotTrusted(t *testing.T) {
	store, _ := open(t)
	store.Now = func() time.Time { return start }

	future := hostSample(start.Add(48*time.Hour), 10, 1<<30)
	if err := store.Write("h1", []*agentv1.MetricSample{future}); err != nil {
		t.Fatal(err)
	}

	points, err := store.HostRange("h1", Windows["1h"])
	if err != nil {
		t.Fatal(err)
	}
	if len(points.T) != 1 || points.T[0] != start.Unix() {
		t.Errorf("times: %v", points.T)
	}
}

func TestDeleteHostRemovesItsHistory(t *testing.T) {
	store, _ := open(t)
	store.Now = func() time.Time { return start }

	if err := store.Write("h1", []*agentv1.MetricSample{hostSample(start, 10, 1<<30), containerSample(start, "c1", 1, 1)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Write("h2", []*agentv1.MetricSample{hostSample(start, 20, 1<<30)}); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteHost("h1"); err != nil {
		t.Fatal(err)
	}

	gone, err := store.HostRange("h1", Windows["1h"])
	if err != nil {
		t.Fatal(err)
	}
	kept, err := store.HostRange("h2", Windows["1h"])
	if err != nil {
		t.Fatal(err)
	}
	if len(gone.T) != 0 || len(kept.T) != 1 {
		t.Errorf("after deleting: %d and %d points", len(gone.T), len(kept.T))
	}

	// the host may come back, for example after enrolling again
	if err := store.Write("h1", []*agentv1.MetricSample{hostSample(start, 10, 1<<30)}); err != nil {
		t.Fatal(err)
	}
	back, err := store.HostRange("h1", Windows["1h"])
	if err != nil {
		t.Fatal(err)
	}
	if len(back.T) != 1 {
		t.Errorf("after coming back: %v", back.T)
	}
}

func TestHistorySurvivesARestart(t *testing.T) {
	store, path := open(t)
	store.Now = func() time.Time { return start }

	if err := store.Write("h1", []*agentv1.MetricSample{hostSample(start, 10, 1<<30)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.Now = func() time.Time { return start }

	points, err := reopened.HostRange("h1", Windows["1h"])
	if err != nil {
		t.Fatal(err)
	}
	if len(points.T) != 1 {
		t.Errorf("points: %v", points.T)
	}
}

// batchFor is one report of a host with its containers, with values that change
// the way real ones do.
func batchFor(host, step, containers int, at time.Time) []*agentv1.MetricSample {
	sample := hostSample(at, 10+float64(step%700)/10, uint64(3<<30)+uint64(step*4096))
	sample.Mounts = []*agentv1.DiskUsage{
		{Mount: "/", Device: "/dev/sda1", FsType: "ext4", UsedBytes: 50<<30 + uint64(step)*4096, TotalBytes: 160 << 30},
		{Mount: "/srv", Device: "/dev/sdb1", FsType: "xfs", UsedBytes: 20<<30 + uint64(step)*4096, TotalBytes: 80 << 30},
	}
	sample.NetInBytes = uint64(10000 + step%9000)
	sample.NetOutBytes = uint64(20000 + step%7000)

	samples := []*agentv1.MetricSample{sample}
	for c := range containers {
		samples = append(samples, &agentv1.MetricSample{
			Ts:            timestamppb.New(at),
			ContainerId:   fmt.Sprintf("%064d", host*1000+c),
			CpuPercent:    float64((step+c)%500) / 100,
			MemBytes:      uint64(100<<20 + (step+c)%(1<<20)),
			MemTotalBytes: 2 << 30,
			NetInBytes:    uint64(1000 + (step+c)%5000),
			NetOutBytes:   uint64(1000 + (step+c)%3000),
		})
	}
	return samples
}

func dbSize(t testing.TB, store *Store, path string) int64 {
	t.Helper()

	if _, err := store.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	return info.Size()
}

// TestStorageGrowth measures the bytes per row with the real tables, for the
// numbers in docs/adr/0003-metrics-storage.md: 10 hosts with 50 containers each,
// and two file systems per host, for an hour of samples and a day of rollups.
func TestStorageGrowth(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a large database")
	}

	const (
		hosts      = 10
		containers = 50
		steps      = 360 // an hour of 10 s samples
	)

	store, path := open(t)

	clock := start
	store.Now = func() time.Time { return clock }

	began := time.Now()
	var longest time.Duration
	for step := range steps {
		at := start.Add(time.Duration(step) * 10 * time.Second)
		clock = at
		for host := range hosts {
			before := time.Now()
			if err := store.Write(fmt.Sprintf("host%02d", host), batchFor(host, step, containers, at)); err != nil {
				t.Fatal(err)
			}
			longest = max(longest, time.Since(before))
		}
	}
	elapsed := time.Since(began)

	var rows int64
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM samples`).Scan(&rows); err != nil {
		t.Fatal(err)
	}

	size := dbSize(t, store, path)
	perRow := float64(size) / float64(rows)

	// every series of the target, for each window of the retention
	series := int64(hosts * (1 + containers + 2))
	full := func(keep time.Duration, res int) float64 {
		return float64(series) * keep.Seconds() / float64(res) * perRow
	}
	total := full(Keep10s, Res10s) + full(Keep1m, Res1m) + full(Keep1h, Res1h)

	t.Logf("%d rows, %d bytes, %.1f bytes per row", rows, size, perRow)
	t.Logf("a batch of %d samples for 10 hosts took %s on average, the longest single host %s",
		hosts*(1+containers), elapsed/steps, longest)
	t.Logf("at retention: 10 s %.0f MB, 1 min %.0f MB, 1 h %.0f MB, total %.0f MB",
		full(Keep10s, Res10s)/1e6, full(Keep1m, Res1m)/1e6, full(Keep1h, Res1h)/1e6, total/1e6)

	if perRow > 90 {
		t.Errorf("%.1f bytes per row is well above the ADR's 58", perRow)
	}

	// a rollup over a full hour stays well below the one minute between runs
	clock = start.Add(time.Hour + time.Second)
	before := time.Now()
	if err := store.Rollup(); err != nil {
		t.Fatal(err)
	}
	took := time.Since(before)
	t.Logf("rolling up an hour of %d rows took %s", rows, took)

	if took > 30*time.Second && !raceEnabled {
		t.Errorf("a rollup took %s", took)
	}

	// and so does the next run, which finds nothing to do
	clock = clock.Add(time.Minute)
	before = time.Now()
	if err := store.Rollup(); err != nil {
		t.Fatal(err)
	}
	t.Logf("the next rollup took %s", time.Since(before))
}

func BenchmarkWriteBatch(b *testing.B) {
	store, _ := open(b)

	clock := start
	store.Now = func() time.Time { return clock }

	for i := 0; b.Loop(); i++ {
		clock = start.Add(time.Duration(i) * 10 * time.Second)
		if err := store.Write("host", batchFor(0, i, 50, clock)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHostRange(b *testing.B) {
	store, _ := open(b)

	clock := start
	store.Now = func() time.Time { return clock }

	for i := range 360 {
		clock = start.Add(time.Duration(i) * 10 * time.Second)
		if err := store.Write("host", batchFor(0, i, 5, clock)); err != nil {
			b.Fatal(err)
		}
	}

	for b.Loop() {
		if _, err := store.HostRange("host", Windows["1h"]); err != nil {
			b.Fatal(err)
		}
	}
}

// TestRequestsAreCountedPerHourAndHostname: the requests of a site's
// hostnames add up per hour, for its chart of the last day.
func TestRequestsAreCountedPerHourAndHostname(t *testing.T) {
	store, _ := open(t)
	clock := start.Truncate(time.Hour).Add(10 * time.Minute)
	store.Now = func() time.Time { return clock }

	for _, counts := range []map[string]uint64{{"blog.example.com": 3, "www.blog.example.com": 1}, {"Blog.example.com": 2, "other.example.com": 50}} {
		if err := store.AddRequests("h1", counts); err != nil {
			t.Fatal(err)
		}
	}
	clock = clock.Add(2 * time.Hour)
	if err := store.AddRequests("h1", map[string]uint64{"blog.example.com": 7}); err != nil {
		t.Fatal(err)
	}
	// another server's edge serves a name of the same spelling
	if err := store.AddRequests("h2", map[string]uint64{"blog.example.com": 100}); err != nil {
		t.Fatal(err)
	}

	got, err := store.Requests("h1", []string{"blog.example.com", "www.blog.example.com"}, nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{0, 6, 0, 7}; !slices.Equal(got, want) {
		t.Errorf("requests %v, want %v", got, want)
	}
	// a wildcard domain counts the names one label below it, but not a name
	// that a domain of its own serves
	if err := store.AddRequests("h1", map[string]uint64{"a.wild.example.com": 4, "b.a.wild.example.com": 9, "tame.wild.example.com": 20}); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Requests("h1", []string{"*.wild.example.com"}, []string{"Tame.wild.example.com"}, 4); !slices.Equal(got, []int64{0, 0, 0, 4}) {
		t.Errorf("the wildcard's requests: %v", got)
	}

	// kept for two days
	clock = clock.Add(KeepRequests + time.Hour)
	if err := store.Rollup(); err != nil {
		t.Fatal(err)
	}
	var left int
	_ = store.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&left)
	if left != 0 {
		t.Errorf("%d counts are past their retention", left)
	}
	if err := store.DeleteHost("h2"); err != nil {
		t.Fatal(err)
	}
}
