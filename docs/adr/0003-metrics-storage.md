# ADR 0003: Metrics in a separate SQLite file, with rollups

**Status:** accepted · **Issue:** #6 · **Affects:** #4, #17

## Context

Agents send host and container samples every 10 seconds. The target is 10 hosts with 50 containers and 2 mounted file systems each: 530 series, 53 samples per second, 4.6 million rows per day at full resolution. PocketBase serialises writes to its database, and that database also holds the commands and the audit log, whose saves must stay fast.

## Measurements

A prototype chose the row layout: with the host and container ids in the primary key a sample took 168 bytes, with a small integer per series and `WITHOUT ROWID` 58.

The store that was built (`internal/metrics`: the real driver `modernc.org/sqlite`, WAL mode, one transaction per batch) was measured with `TestStorageGrowth`, an hour of 10 s samples of the target, and the benchmarks next to it:

| What | Result |
|---|---|
| Bytes per row | **46** |
| A 10 s round: one batch per host, 10 hosts, 530 rows | 11 ms (one batch at most 20 ms) |
| Rolling up that hour, 190,800 rows | **0.85 s** (0.40 s on a second machine); the next run, with nothing to do, 6 ms |
| An hour of one host for a chart, 360 points (`BenchmarkHostRange`) | 0.7 ms |

The rows are smaller than the prototype's because CPU and load are stored in hundredths as integers (a `REAL` always takes 8 bytes), and each kind of series fills only its own columns: load only on hosts, disk usage only on hosts and mounts.

Storage for the target:

| Resolution | Kept for | Rows | Size |
|---|---|---|---|
| 10 s | 24 hours | 4.6 million | 211 MB |
| 1 minute | 7 days | 5.3 million | 246 MB |
| 1 hour | 90 days | 1.1 million | 53 MB |
| | **Total** | **11.1 million** | **about 510 MB** |

The size stays flat once the retention windows are full. A homelab with 3 hosts and 20 containers each needs about 66 MB.

## Decision

Metrics go to their own SQLite file, `pb_data/metrics.db`, written by a small store with a small integer per series and `WITHOUT ROWID`. They are **not a PocketBase collection**, although `docs/data-model.md` first listed them as one.

- A collection would mean one record per sample, with an id, hooks and realtime events for each, on the same write lock as commands and the audit log. Batches of samples would hold that lock often and for no good reason.
- An embedded time-series database is a large dependency for a problem that SQLite handles with one table and three small queries.
- The range API picks the resolution from the requested range: 10 s for an hour, 1 minute for a day, 1 hour for a week.
- Rollups run once a minute, and old rows are removed by the same job. A rolled-up bucket holds the mean of each value and the sum of the traffic.
- A sample lands in the 10 s bucket of its time, and a second one in the same bucket replaces the first. An agent whose clock is more than 5 minutes off gets the panel's time instead.
- Each mounted file system is a series of its own, so a host with several disks shows each of them.

## Consequences

- Live values for the overview stay on the `hosts` record, from the heartbeat, so they work over realtime like everything else. History goes through its own API.
- The panel's own backup (#32) can leave out `metrics.db`: the history is nice to have, and losing it does not affect recovery.
- The UI cannot use PocketBase's realtime or filter syntax on metrics; it does not need to.
