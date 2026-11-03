# ADR 0003: Metrics in a separate SQLite file, with rollups

**Status:** accepted · **Issue:** #6 · **Affects:** #4, #17

## Context

Agents send host and container samples every 10 seconds. The target is 10 hosts with 50 containers each: 510 series, 51 samples per second, 4.4 million samples per day at full resolution. PocketBase serialises writes to its database, and that database also holds the commands and the audit log, whose saves must stay fast.

## Measurements

With the real driver (`modernc.org/sqlite`), WAL mode, one transaction per batch, on the development machine:

| Row layout | Bytes per sample | 510 samples in one transaction | 360 points of one series |
|---|---|---|---|
| Host and container ids in the primary key | 168 | 46 ms | 0.5 ms |
| A small integer per series, `WITHOUT ROWID`, disk and load only on host samples | **58** | **23 ms** | **0.5 ms** |

Storage for the target, with the second layout:

| Resolution | Kept for | Rows | Size |
|---|---|---|---|
| 10 s | 24 hours | 4.4 million | 257 MB |
| 1 minute | 7 days | 5.1 million | 300 MB |
| 1 hour | 90 days | 1.1 million | 64 MB |
| | **Total** | **10.6 million** | **about 620 MB** |

The size stays flat once the retention windows are full. A homelab with 3 hosts and 20 containers each needs about 80 MB.

## Decision

Metrics go to their own SQLite file, `pb_data/metrics.db`, written by a small store with the second layout. They are **not a PocketBase collection**, although `docs/data-model.md` first listed them as one.

- A collection would mean one record per sample, with an id, hooks and realtime events for each, on the same write lock as commands and the audit log. Batches of samples would hold that lock often and for no good reason.
- An embedded time-series database is a large dependency for a problem that SQLite handles with one table and three small queries.
- The range API picks the resolution from the requested range: 10 s for an hour, 1 minute for a day, 1 hour for a week.
- Rollups run once a minute, and old rows are removed by the same job.

## Consequences

- Live values for the overview stay on the `hosts` record, from the heartbeat, so they work over realtime like everything else. History goes through its own API.
- The panel's own backup (#32) can leave out `metrics.db`: the history is nice to have, and losing it does not affect recovery.
- The UI cannot use PocketBase's realtime or filter syntax on metrics; it does not need to.
