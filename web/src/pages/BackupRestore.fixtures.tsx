// What the tests of the snapshots and restores (#30) share: servers,
// targets, jobs, snapshots and restore records, and the providers a page
// needs. Only tests import it.
import { render } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { ToastProvider } from "../components/toast";
import { AuthProvider } from "../lib/auth";
import type { BackupJob, BackupRun } from "../lib/backupJobs";
import type {
  BackupRestore,
  BackupSnapshot,
  RestorePreview,
  SnapshotFolder,
  SnapshotList,
} from "../lib/backupRestores";
import type { BackupTarget } from "../lib/backups";
import { pb, type User } from "../lib/pb";
import type { Command, Host } from "../lib/types";

// Monday 13:00 in Prague, on winter time: an hour ahead of UTC
export const now = Date.parse("2026-11-09T12:00:00Z");

export function signInAs(role: User["role"]) {
  const payload = btoa(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600, type: "auth" }));
  pb.authStore.save(`header.${payload}.signature`, {
    id: "u1",
    collectionId: "users",
    collectionName: "users",
    email: `${role}@example.com`,
    name: "",
    role,
  } as unknown as User);
}

/** Where the app is: its path and query, to see what a click put in it. */
function Location() {
  const location = useLocation();
  return <output data-testid="location">{location.pathname + location.search}</output>;
}

export function wrap(children: ReactNode, path = "/backups") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider>
        <ToastProvider>
          {children}
          <Location />
        </ToastProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

export const hosts = [
  { id: "h1", name: "pve-01", status: "online" },
  { id: "h2", name: "nas", status: "online" },
  { id: "h3", name: "edge-01", status: "offline" },
] as Host[];

function target(fields: Partial<BackupTarget>): BackupTarget {
  return {
    id: "t1",
    collectionId: "backup_targets",
    collectionName: "backup_targets",
    name: "Backblaze B2",
    kind: "s3",
    host: "h1",
    config: null,
    location: "s3:homelab-backups",
    repo_id: "",
    status: "ok",
    last_error: "",
    checked_at: "",
    usage_at: "",
    used_bytes: 0,
    capacity_bytes: 0,
    immutable_days: 0,
    note: "",
    ...fields,
  } as BackupTarget;
}

export const bucket = target({});
// a directory of nas: only nas restores from it
export const disk = target({
  id: "t2",
  name: "Panel disk",
  kind: "local",
  host: "h2",
  location: "/srv/backups/restic",
});
// the job backed up to it before
export const node = target({ id: "t3", name: "backup-node", kind: "sftp" });
export const targets = [bucket, disk, node];

export function job(fields: Partial<BackupJob>): BackupJob {
  return {
    id: "j1",
    collectionId: "backup_jobs",
    collectionName: "backup_jobs",
    name: "vaultwarden",
    host: "h2",
    source_kind: "volume",
    source: "vw_data",
    stack: "",
    service: "",
    engine: "",
    targets: ["t1", "t2"],
    schedule: "0 * * * *",
    retention: { daily: 7 },
    enabled: true,
    origin: "manual",
    label_removed: false,
    problem: "",
    next_run_at: "2026-11-09 13:00:00.000Z",
    last_run_at: "",
    last_status: "",
    ...fields,
  } as BackupJob;
}

export const vaultwarden = job({});
export const photos = job({
  id: "j2",
  name: "photos",
  host: "h1",
  source_kind: "path",
  source: "/mnt/tank/photos",
  targets: ["t1"],
});
export const umami = job({
  id: "j3",
  name: "umami-db",
  host: "h1",
  source_kind: "db_dump",
  source: "",
  stack: "s1",
  service: "db",
  engine: "postgres",
  targets: ["t1"],
});
export const jobs = [vaultwarden, photos, umami];

export function run(id: string, fields: Partial<BackupRun>): BackupRun {
  return {
    id,
    collectionId: "backup_runs",
    collectionName: "backup_runs",
    created: "2026-11-09 11:00:00.000Z",
    updated: "",
    job: "j1",
    target: "t1",
    host: "h2",
    trigger: "schedule",
    status: "succeeded",
    scheduled_at: "",
    started_at: "",
    finished_at: "2026-11-09 11:00:40.000Z",
    duration_ms: 0,
    snapshot_id: "",
    bytes_added: 0,
    total_bytes: 0,
    files_new: 0,
    files_changed: 0,
    forgotten: 0,
    retention: "",
    error: "",
    ...fields,
  } as BackupRun;
}

/** A full snapshot id that starts with `short`. */
export function snapshotId(short: string): string {
  return short.padEnd(64, "0");
}

export function snapshot(short: string, fields: Partial<BackupSnapshot> = {}): BackupSnapshot {
  return {
    id: snapshotId(short),
    short_id: short,
    time: "2026-11-09T11:00:00Z",
    hostname: "nas",
    paths: ["/volumes/vw_data"],
    size_bytes: 1.2e9,
    added_bytes: 84e6,
    files: 1200,
    run: null,
    ...fields,
  };
}

/** A job's snapshots, newest first: one an hour back from 12:00 UTC. */
export function snapshots(count: number, fields: Partial<SnapshotList> = {}): SnapshotList {
  return {
    target: "t1",
    host: "h1",
    fetched_at: "2026-11-09T11:58:00Z",
    cached: false,
    total: count,
    truncated: false,
    snapshots: Array.from({ length: count }, (_, index) =>
      snapshot((0x3f9a1c2e - index).toString(16), {
        time: new Date(Date.parse("2026-11-09T11:00:00Z") - index * 3600_000).toISOString(),
        added_bytes: (index + 1) * 1e6,
      }),
    ),
    ...fields,
  };
}

export function folder(path: string, fields: Partial<SnapshotFolder> = {}): SnapshotFolder {
  return {
    path,
    total: 3,
    dirs: 1,
    files: 2,
    offset: 0,
    entries: [
      {
        name: "attachments",
        type: "dir",
        size: 0,
        mtime: "2026-11-09T10:00:00Z",
        mode: 493,
        bad_name: false,
      },
      {
        name: "db.sqlite3",
        type: "file",
        size: 4.2e6,
        mtime: "2026-11-09T10:59:00Z",
        mode: 420,
        bad_name: false,
      },
      {
        name: "config.json",
        type: "file",
        size: 812,
        mtime: "2026-11-01T08:00:00Z",
        mode: 420,
        bad_name: false,
      },
    ],
    ...fields,
  };
}

export function preview(fields: Partial<RestorePreview> = {}): RestorePreview {
  return {
    dry_run: true,
    snapshot: {
      id: snapshotId("3f9a1c2e"),
      time: "2026-11-09T11:00:00Z",
      paths: ["/volumes/vw_data"],
    },
    roots: [
      {
        index: 0,
        source: "/volumes/vw_data",
        destination: "vw_data",
        in_snapshot: true,
        exists: true,
        entries: 0,
        entries_capped: false,
        uid: 0,
        gid: 0,
        free_bytes: 0,
      },
    ],
    not_in_job: [],
    counts: { new: 0, updated: 0, unchanged: 0, deleted: 0 },
    bytes: { new: 0, updated: 0, deleted: 0 },
    samples: { new: [], updated: [], deleted: [] },
    truncated: { new: 0, updated: 0, deleted: 0 },
    would_stop: [],
    total_files: 1200,
    total_bytes: 1.2e9,
    notes: [],
    ...fields,
  };
}

export function restore(fields: Partial<BackupRestore> = {}): BackupRestore {
  return {
    id: "r1",
    collectionId: "backup_restores",
    collectionName: "backup_restores",
    created: "2026-11-09 11:50:00.000Z",
    updated: "2026-11-09 11:50:00.000Z",
    job: "j1",
    target: "t1",
    host: "h2",
    label: "vaultwarden ← Backblaze B2 · 3f9a1c2e → original location",
    snapshot_id: snapshotId("3f9a1c2e"),
    snapshot_time: "2026-11-09 11:00:00.000Z",
    mode: "original",
    roots: [{ source: "/volumes/vw_data", volume: "vw_data" }],
    path: "",
    source: "",
    include: null,
    replace: true,
    overwrite: false,
    stack_name: "vaultwarden",
    status: "previewing",
    preview: null,
    preview_at: "",
    started_at: "",
    finished_at: "",
    expires_at: "2026-11-09 12:20:00.000Z",
    preview_command: "c0",
    command: "",
    requested_by: "u1",
    requested_label: "operator@example.com",
    confirmed_by: "",
    confirmed_label: "",
    stop_requested_label: "",
    files_restored: 0,
    bytes_restored: 0,
    files_deleted: 0,
    errors: 0,
    bytes: 0,
    partial: false,
    app_stopped: null,
    app_problem: "",
    error: "",
    log: "",
    download_name: "",
    download_kind: "",
    ...fields,
  };
}

export function command(fields: Partial<Command> & Record<string, unknown> = {}): Command {
  return {
    id: "c1",
    collectionId: "commands",
    collectionName: "commands",
    host: "h2",
    host_name: "nas",
    type: "backup.restore",
    label: "",
    payload: null,
    status: "running",
    requested_by: "u1",
    requested_label: "",
    correlation_id: "",
    attempts: 1,
    dispatched_at: "",
    started_at: "",
    finished_at: "",
    progress: null,
    result: null,
    error: "",
    output: "",
    ...fields,
  } as Command;
}
