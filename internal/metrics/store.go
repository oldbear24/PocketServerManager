// Package metrics keeps the usage history of hosts and containers in its own
// SQLite file, with rollups. It is not a PocketBase collection: see
// docs/adr/0003-metrics-storage.md.
package metrics

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // the driver PocketBase uses as well

	agentv1 "github.com/oldbear24/PocketServerManager/gen/go/pocketcloud/agent/v1"
)

// Resolutions in seconds, and how long each of them is kept.
const (
	Res10s = 10
	Res1m  = 60
	Res1h  = 3600

	Keep10s = 24 * time.Hour
	Keep1m  = 7 * 24 * time.Hour
	Keep1h  = 90 * 24 * time.Hour
)

const (
	kindHost      = 0
	kindContainer = 1
	kindMount     = 2

	// maxClockSkew is how far an agent's clock may be off before its time is not trusted.
	maxClockSkew = 5 * time.Minute

	freshContainer = 2 * time.Minute
	freshMount     = 5 * time.Minute
)

const schema = `
CREATE TABLE IF NOT EXISTS series (
	id      INTEGER PRIMARY KEY,
	host    TEXT NOT NULL,
	kind    INTEGER NOT NULL,
	name    TEXT NOT NULL,
	device  TEXT NOT NULL DEFAULT '',
	fs_type TEXT NOT NULL DEFAULT '',
	UNIQUE (host, kind, name)
);
CREATE TABLE IF NOT EXISTS samples (
	series     INTEGER NOT NULL,
	res        INTEGER NOT NULL,
	ts         INTEGER NOT NULL,
	cpu        INTEGER,
	mem        INTEGER,
	mem_total  INTEGER,
	net_in     INTEGER,
	net_out    INTEGER,
	disk       INTEGER,
	disk_total INTEGER,
	load1      INTEGER,
	PRIMARY KEY (series, res, ts)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value INTEGER NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS requests (
	host  TEXT NOT NULL,
	name  TEXT NOT NULL,
	hour  INTEGER NOT NULL,
	count INTEGER NOT NULL,
	PRIMARY KEY (host, name, hour)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS latency (
	host   TEXT NOT NULL,
	name   TEXT NOT NULL,
	hour   INTEGER NOT NULL,
	bucket INTEGER NOT NULL,
	count  INTEGER NOT NULL,
	PRIMARY KEY (host, name, hour, bucket)
) WITHOUT ROWID;
`

// KeepRequests is how long the hourly request counts of hostnames are kept.
const KeepRequests = 48 * time.Hour

// CPU and load are stored in hundredths, as integers: a REAL always takes 8 bytes.
const hundredths = 100

// Store is one metrics database. Writes go one at a time, reads run next to them.
type Store struct {
	db *sql.DB

	// Now is the clock; tests move it.
	Now func() time.Time

	mu     sync.Mutex
	series map[seriesKey]seriesEntry
}

type seriesKey struct {
	host string
	kind int
	name string
}

type seriesEntry struct {
	id     int64
	device string
	fsType string
}

// Open creates or opens the database at path.
func Open(path string) (*Store, error) {
	// busy_timeout comes first: setting WAL may have to wait for another connection
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=journal_size_limit(67108864)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create the metrics tables: %w", err)
	}

	return &Store{db: db, Now: time.Now, series: make(map[seriesKey]seriesEntry)}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

func clampInt(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}

// sampleTime is the agent's clock, unless it is clearly wrong.
func sampleTime(sample *agentv1.MetricSample, now time.Time) time.Time {
	if sample.GetTs() == nil {
		return now
	}

	at := sample.GetTs().AsTime()
	if at.After(now.Add(maxClockSkew)) || at.Before(now.Add(-maxClockSkew)) {
		return now
	}

	return at
}

// Write stores one batch of an agent. A sample lands in the 10 s bucket of its
// time; a second sample in the same bucket replaces the first.
func (s *Store) Write(hostID string, samples []*agentv1.MetricSample) error {
	if len(samples) == 0 {
		return nil
	}

	now := s.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after the commit

	insert, err := tx.Prepare(`INSERT OR REPLACE INTO samples
		(series, res, ts, cpu, mem, mem_total, net_in, net_out, disk, disk_total, load1)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insert.Close()

	failed := func(err error) error {
		// new series were rolled back with the transaction
		s.series = make(map[seriesKey]seriesEntry)
		return err
	}

	for _, sample := range samples {
		ts := sampleTime(sample, now).Unix() / Res10s * Res10s

		if container := sample.GetContainerId(); container != "" {
			id, err := s.seriesID(tx, seriesKey{hostID, kindContainer, container}, "", "")
			if err != nil {
				return failed(err)
			}
			_, err = insert.Exec(id, Res10s, ts,
				round(sample.GetCpuPercent()), clampInt(sample.GetMemBytes()), clampInt(sample.GetMemTotalBytes()),
				clampInt(sample.GetNetInBytes()), clampInt(sample.GetNetOutBytes()), nil, nil, nil)
			if err != nil {
				return failed(err)
			}
			continue
		}

		id, err := s.seriesID(tx, seriesKey{hostID, kindHost, ""}, "", "")
		if err != nil {
			return failed(err)
		}
		_, err = insert.Exec(id, Res10s, ts,
			round(sample.GetCpuPercent()), clampInt(sample.GetMemBytes()), clampInt(sample.GetMemTotalBytes()),
			clampInt(sample.GetNetInBytes()), clampInt(sample.GetNetOutBytes()),
			clampInt(sample.GetDiskUsedBytes()), clampInt(sample.GetDiskTotalBytes()), round(sample.GetLoad1()))
		if err != nil {
			return failed(err)
		}

		for _, mount := range sample.GetMounts() {
			id, err := s.seriesID(tx, seriesKey{hostID, kindMount, mount.GetMount()}, mount.GetDevice(), mount.GetFsType())
			if err != nil {
				return failed(err)
			}
			_, err = insert.Exec(id, Res10s, ts, nil, nil, nil, nil, nil,
				clampInt(mount.GetUsedBytes()), clampInt(mount.GetTotalBytes()), nil)
			if err != nil {
				return failed(err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return failed(err)
	}

	return nil
}

func round(value float64) int64 {
	return int64(math.Round(value * hundredths))
}

// seriesID finds or creates a series. The ids of known series come from memory:
// nearly every sample of a batch belongs to one.
func (s *Store) seriesID(tx *sql.Tx, key seriesKey, device, fsType string) (int64, error) {
	entry, known := s.series[key]
	if known && entry.device == device && entry.fsType == fsType {
		return entry.id, nil
	}

	err := tx.QueryRow(`INSERT INTO series (host, kind, name, device, fs_type) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (host, kind, name) DO UPDATE SET device = excluded.device, fs_type = excluded.fs_type
		RETURNING id`, key.host, key.kind, key.name, device, fsType).Scan(&entry.id)
	if err != nil {
		return 0, err
	}

	entry.device, entry.fsType = device, fsType
	s.series[key] = entry

	return entry.id, nil
}

// Rollup aggregates the buckets that have ended into the next resolution and
// removes what is past its retention. It runs once a minute.
func (s *Store) Rollup() error {
	now := s.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after the commit

	if err := rollupLevel(tx, Res10s, Res1m, now); err != nil {
		return err
	}
	if err := rollupLevel(tx, Res1m, Res1h, now); err != nil {
		return err
	}

	for _, level := range []struct {
		res  int
		keep time.Duration
	}{{Res10s, Keep10s}, {Res1m, Keep1m}, {Res1h, Keep1h}} {
		// IN keeps this on the primary key: the columns start with the series
		_, err := tx.Exec(`DELETE FROM samples WHERE series IN (SELECT id FROM series) AND res = ? AND ts < ?`,
			level.res, now.Add(-level.keep).Unix())
		if err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`DELETE FROM requests WHERE hour < ?`, now.Add(-KeepRequests).Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM latency WHERE hour < ?`, now.Add(-KeepRequests).Unix()); err != nil {
		return err
	}

	// containers come and go; their series end with their last sample
	result, err := tx.Exec(`DELETE FROM series WHERE NOT EXISTS (SELECT 1 FROM samples WHERE samples.series = series.id)`)
	if err != nil {
		return err
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	if removed > 0 {
		s.series = make(map[seriesKey]seriesEntry)
	}

	return nil
}

// rollupLevel aggregates the buckets that ended since the previous run. A bucket
// holds the mean of each value and the sum of the traffic.
func rollupLevel(tx *sql.Tx, from, to int, now time.Time) error {
	key := fmt.Sprintf("rollup_%d", to)

	var since int64
	err := tx.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&since)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// the current bucket is still filling
	until := now.Unix() / int64(to) * int64(to)
	if since >= until {
		return nil
	}

	_, err = tx.Exec(`INSERT OR REPLACE INTO samples
		(series, res, ts, cpu, mem, mem_total, net_in, net_out, disk, disk_total, load1)
		SELECT series, ?1, ts / ?1 * ?1,
			CAST(ROUND(AVG(cpu)) AS INTEGER), CAST(ROUND(AVG(mem)) AS INTEGER), MAX(mem_total),
			SUM(net_in), SUM(net_out),
			CAST(ROUND(AVG(disk)) AS INTEGER), MAX(disk_total), CAST(ROUND(AVG(load1)) AS INTEGER)
		FROM samples
		WHERE series IN (SELECT id FROM series) AND res = ?2 AND ts >= ?3 AND ts < ?4
		GROUP BY series, ts / ?1`, to, from, since, until)
	if err != nil {
		return err
	}

	_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, key, until)
	return err
}

// DeleteHost removes the history of a host.
func (s *Store) DeleteHost(hostID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after the commit

	if _, err := tx.Exec(`DELETE FROM samples WHERE series IN (SELECT id FROM series WHERE host = ?)`, hostID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM series WHERE host = ?`, hostID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM requests WHERE host = ?`, hostID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM latency WHERE host = ?`, hostID); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	s.series = make(map[seriesKey]seriesEntry)

	return nil
}

// Window is what the range API offers: the length of a chart and its resolution.
type Window struct {
	Name       string
	Span       time.Duration
	Resolution int
}

// Windows lists the ranges of the API. The resolution follows from the range: 10 s
// for an hour, 1 minute for a day, 1 hour for a week.
var Windows = map[string]Window{
	"1h":  {"1h", time.Hour, Res10s},
	"24h": {"24h", 24 * time.Hour, Res1m},
	"7d":  {"7d", 7 * 24 * time.Hour, Res1h},
}

// Points is a chart's data, one list per value. Traffic is in bytes per second.
type Points struct {
	Range       string    `json:"range"`
	ResolutionS int       `json:"resolution_s"`
	From        int64     `json:"from"`
	To          int64     `json:"to"`
	T           []int64   `json:"t"`
	CPU         []float64 `json:"cpu"`
	Mem         []int64   `json:"mem"`
	MemTotal    []int64   `json:"mem_total"`
	NetIn       []float64 `json:"net_in"`
	NetOut      []float64 `json:"net_out"`
	Disk        []int64   `json:"disk,omitempty"`
	DiskTotal   []int64   `json:"disk_total,omitempty"`
	Load1       []float64 `json:"load1,omitempty"`
}

// HostRange returns a host's history for a window.
func (s *Store) HostRange(hostID string, window Window) (*Points, error) {
	return s.rangeOf(seriesKey{hostID, kindHost, ""}, window)
}

// ContainerRange returns a container's history for a window.
func (s *Store) ContainerRange(hostID, containerID string, window Window) (*Points, error) {
	return s.rangeOf(seriesKey{hostID, kindContainer, containerID}, window)
}

func (s *Store) rangeOf(key seriesKey, window Window) (*Points, error) {
	now := s.Now()
	from := now.Add(-window.Span).Unix()

	points := &Points{
		Range:       window.Name,
		ResolutionS: window.Resolution,
		From:        from,
		To:          now.Unix(),
		T:           []int64{},
		CPU:         []float64{},
		Mem:         []int64{},
		MemTotal:    []int64{},
		NetIn:       []float64{},
		NetOut:      []float64{},
	}

	rows, err := s.db.Query(`SELECT ts, COALESCE(cpu, 0), COALESCE(mem, 0), COALESCE(mem_total, 0),
			COALESCE(net_in, 0), COALESCE(net_out, 0), COALESCE(disk, 0), COALESCE(disk_total, 0), COALESCE(load1, 0)
		FROM samples
		WHERE series = (SELECT id FROM series WHERE host = ? AND kind = ? AND name = ?) AND res = ? AND ts >= ?
		ORDER BY ts`, key.host, key.kind, key.name, window.Resolution, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	isHost := key.kind == kindHost
	seconds := float64(window.Resolution)

	for rows.Next() {
		var ts, cpu, mem, memTotal, netIn, netOut, disk, diskTotal, load int64
		if err := rows.Scan(&ts, &cpu, &mem, &memTotal, &netIn, &netOut, &disk, &diskTotal, &load); err != nil {
			return nil, err
		}

		points.T = append(points.T, ts)
		points.CPU = append(points.CPU, float64(cpu)/hundredths)
		points.Mem = append(points.Mem, mem)
		points.MemTotal = append(points.MemTotal, memTotal)
		points.NetIn = append(points.NetIn, float64(netIn)/seconds)
		points.NetOut = append(points.NetOut, float64(netOut)/seconds)

		if isHost {
			points.Disk = append(points.Disk, disk)
			points.DiskTotal = append(points.DiskTotal, diskTotal)
			points.Load1 = append(points.Load1, float64(load)/hundredths)
		}
	}

	return points, rows.Err()
}

// ContainerValue is the latest sample of a container.
type ContainerValue struct {
	CPU      float64 `json:"cpu"`
	Mem      int64   `json:"mem"`
	MemLimit int64   `json:"mem_limit"`
	NetIn    float64 `json:"net_in"`
	NetOut   float64 `json:"net_out"`
	At       int64   `json:"at"`
}

// LatestContainers returns the current values of a host's containers, by docker
// id. A container that has stopped reporting is not included.
func (s *Store) LatestContainers(hostID string) (map[string]ContainerValue, error) {
	rows, err := s.db.Query(`SELECT series.name, samples.ts, COALESCE(samples.cpu, 0), COALESCE(samples.mem, 0),
			COALESCE(samples.mem_total, 0), COALESCE(samples.net_in, 0), COALESCE(samples.net_out, 0)
		FROM series
		JOIN samples ON samples.series = series.id AND samples.res = ?1
			AND samples.ts = (SELECT MAX(ts) FROM samples AS latest WHERE latest.series = series.id AND latest.res = ?1)
		WHERE series.host = ?2 AND series.kind = ?3 AND samples.ts >= ?4`,
		Res10s, hostID, kindContainer, s.Now().Add(-freshContainer).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	values := map[string]ContainerValue{}
	for rows.Next() {
		var (
			name          string
			cpu           int64
			netIn, netOut int64
			value         ContainerValue
		)
		if err := rows.Scan(&name, &value.At, &cpu, &value.Mem, &value.MemLimit, &netIn, &netOut); err != nil {
			return nil, err
		}
		value.CPU = float64(cpu) / hundredths
		value.NetIn = float64(netIn) / Res10s
		value.NetOut = float64(netOut) / Res10s
		values[name] = value
	}

	return values, rows.Err()
}

// Mount is the latest usage of one file system of a host.
type Mount struct {
	Mount  string `json:"mount"`
	Device string `json:"device"`
	FSType string `json:"fs_type"`
	Used   int64  `json:"used"`
	Total  int64  `json:"total"`
	At     int64  `json:"at"`
}

// Mounts returns the file systems a host currently reports, by mount point.
func (s *Store) Mounts(hostID string) ([]Mount, error) {
	rows, err := s.db.Query(`SELECT series.name, series.device, series.fs_type, samples.ts,
			COALESCE(samples.disk, 0), COALESCE(samples.disk_total, 0)
		FROM series
		JOIN samples ON samples.series = series.id AND samples.res = ?1
			AND samples.ts = (SELECT MAX(ts) FROM samples AS latest WHERE latest.series = series.id AND latest.res = ?1)
		WHERE series.host = ?2 AND series.kind = ?3 AND samples.ts >= ?4
		ORDER BY series.name`, Res10s, hostID, kindMount, s.Now().Add(-freshMount).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	mounts := []Mount{}
	for rows.Next() {
		var mount Mount
		if err := rows.Scan(&mount.Mount, &mount.Device, &mount.FSType, &mount.At, &mount.Used, &mount.Total); err != nil {
			return nil, err
		}
		mounts = append(mounts, mount)
	}

	return mounts, rows.Err()
}

// AddRequests adds the requests an edge served for each hostname to the
// counts of the current hour.
func (s *Store) AddRequests(hostID string, counts map[string]uint64) error {
	if len(counts) == 0 {
		return nil
	}
	hour := s.Now().Truncate(time.Hour).Unix()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after the commit

	for name, count := range counts {
		if count > math.MaxInt64 {
			count = math.MaxInt64
		}
		_, err := tx.Exec(`INSERT INTO requests (host, name, hour, count) VALUES (?, ?, ?, ?)
			ON CONFLICT (host, name, hour) DO UPDATE SET count = count + excluded.count`,
			hostID, strings.ToLower(name), hour, int64(count))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LatencyBounds are the upper bounds of the latency buckets, in milliseconds,
// as Caddy's histogram has them; the last bucket has none.
var LatencyBounds = []float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

// errorsBucket holds a hostname's requests answered with a 5xx.
const errorsBucket = -1

// HostLatency is a hostname's requests by how long they took, one bucket each,
// and those answered with a 5xx.
type HostLatency struct {
	Buckets []uint64
	Errors  uint64
}

// AddLatency adds the durations of the requests an edge served for each
// hostname to those of the current hour (#50).
func (s *Store) AddLatency(hostID string, byName map[string]HostLatency) error {
	if len(byName) == 0 {
		return nil
	}
	hour := s.Now().Truncate(time.Hour).Unix()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after the commit

	add := func(name string, bucket int, count uint64) error {
		if count == 0 {
			return nil
		}
		_, err := tx.Exec(`INSERT INTO latency (host, name, hour, bucket, count) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (host, name, hour, bucket) DO UPDATE SET count = count + excluded.count`,
			hostID, strings.ToLower(name), hour, bucket, int64(min(count, math.MaxInt64)))
		return err
	}
	for name, latency := range byName {
		for bucket, count := range latency.Buckets {
			if bucket > len(LatencyBounds) {
				break
			}
			if err := add(name, bucket, count); err != nil {
				return err
			}
		}
		if err := add(name, errorsBucket, latency.Errors); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Latency is what the requests of hostnames took in the last hours, with the
// same names as Requests: how many there were, their 50th and 95th percentile
// in milliseconds, estimated within their buckets, and how many failed.
type Latency struct {
	Requests int64   `json:"requests"`
	Errors   int64   `json:"errors"`
	P50Ms    float64 `json:"p50_ms"`
	P95Ms    float64 `json:"p95_ms"`
}

// Latency adds up the durations of the requests of hostnames served by a
// host's edge in the last hours.
func (s *Store) Latency(hostID string, names, exact []string, hours int) (Latency, error) {
	var latency Latency
	if len(names) == 0 || hours <= 0 {
		return latency, nil
	}
	first := s.Now().Truncate(time.Hour).Add(-time.Duration(hours-1) * time.Hour).Unix()
	rows, err := s.db.Query(`SELECT name, bucket, count FROM latency WHERE host = ? AND hour >= ?`, hostID, first)
	if err != nil {
		return latency, err
	}
	defer rows.Close()
	buckets := make([]int64, len(LatencyBounds)+1)
	for rows.Next() {
		var name string
		var bucket, count int64
		if err := rows.Scan(&name, &bucket, &count); err != nil {
			return latency, err
		}
		if !matchesAny(names, exact, name) {
			continue
		}
		switch {
		case bucket == errorsBucket:
			latency.Errors += count
		case bucket >= 0 && bucket < int64(len(buckets)):
			buckets[bucket] += count
			latency.Requests += count
		}
	}
	if err := rows.Err(); err != nil {
		return latency, err
	}
	latency.P50Ms = percentile(buckets, latency.Requests, 0.5)
	latency.P95Ms = percentile(buckets, latency.Requests, 0.95)
	return latency, nil
}

// percentile estimates a percentile from bucket counts, linearly within its
// bucket; one in the last bucket, which has no bound, is its lower bound.
func percentile(buckets []int64, total int64, p float64) float64 {
	if total == 0 {
		return 0
	}
	rank := p * float64(total)
	var seen float64
	for i, count := range buckets {
		if count == 0 {
			continue
		}
		if seen+float64(count) >= rank {
			lower := 0.0
			if i > 0 {
				lower = LatencyBounds[i-1]
			}
			if i >= len(LatencyBounds) {
				return lower
			}
			return lower + (LatencyBounds[i]-lower)*(rank-seen)/float64(count)
		}
		seen += float64(count)
	}
	return LatencyBounds[len(LatencyBounds)-1]
}

// Requests are the requests of hostnames served by a host's edge in each of the
// last hours, oldest first; the last is the hour under way. A wildcard such as
// *.example.com counts the names one label below it, but not those in exact,
// which the edge sends to routes of their own.
func (s *Store) Requests(hostID string, names, exact []string, hours int) ([]int64, error) {
	counts := make([]int64, hours)
	if len(names) == 0 || hours <= 0 {
		return counts, nil
	}
	current := s.Now().Truncate(time.Hour)
	first := current.Add(-time.Duration(hours-1) * time.Hour).Unix()

	rows, err := s.db.Query(`SELECT name, hour, count FROM requests WHERE host = ? AND hour >= ?`, hostID, first)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var hour, count int64
		if err := rows.Scan(&name, &hour, &count); err != nil {
			return nil, err
		}
		if !matchesAny(names, exact, name) {
			continue
		}
		if index := (hour - first) / 3600; index >= 0 && index < int64(hours) {
			counts[index] += count
		}
	}
	return counts, rows.Err()
}

// matchesAny says whether a hostname is one of names, or one label below a
// wildcard of them and not one of exact.
func matchesAny(names, exact []string, hostname string) bool {
	for _, name := range names {
		name = strings.ToLower(name)
		if name == hostname {
			return true
		}
		if suffix, ok := strings.CutPrefix(name, "*"); ok && !slices.ContainsFunc(exact, func(other string) bool { return strings.EqualFold(other, hostname) }) {
			if label, found := strings.CutSuffix(hostname, suffix); found && label != "" && !strings.Contains(label, ".") {
				return true
			}
		}
	}
	return false
}
