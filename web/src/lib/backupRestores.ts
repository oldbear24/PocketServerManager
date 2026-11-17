import type { RecordModel } from "pocketbase";
import { useEffect, useRef, useState } from "react";

import i18n from "../i18n";
import { dayKey, lastDays, type BackupJob, type BackupRun } from "./backupJobs";
import type { BackupTarget } from "./backups";
import { formatBytes, formatNumber, parseDate } from "./format";
import { pb, type Role } from "./pb";

/** A snapshot of a job in one target, as the panel lists it (#30). */
export type BackupSnapshot = {
  /** the full id, 64 hex characters */
  id: string;
  short_id: string;
  /** RFC 3339, in UTC */
  time: string;
  hostname: string;
  paths: string[];
  /** what was backed up */
  size_bytes: number;
  /** what the backup added to the repository */
  added_bytes: number;
  files: number;
  /** the run that made it, while the panel keeps the run: of any trigger,
   * a retry and a change run (#32) too */
  run: { id: string; trigger: BackupRun["trigger"] } | null;
};

/** A job's snapshots in one target, newest first. */
export type SnapshotList = {
  target: string;
  /** the server whose agent listed them */
  host: string;
  fetched_at: string;
  /** whether the list came from the panel's cache */
  cached: boolean;
  /** how many there are, of which the newest are listed */
  total: number;
  truncated: boolean;
  snapshots: BackupSnapshot[];
};

/** An entry of a snapshot's folder; `type` is restic's node type. */
export type SnapshotEntry = {
  name: string;
  type: "dir" | "file" | "symlink" | "dev" | "chardev" | "fifo" | "socket";
  size: number;
  mtime: string;
  /** the Unix permission bits */
  mode: number;
  /** the name is not valid UTF-8: it cannot be picked */
  bad_name: boolean;
};

/** A page of a snapshot's folder: directories first, then by name. */
export type SnapshotFolder = {
  path: string;
  total: number;
  dirs: number;
  files: number;
  offset: number;
  entries: SnapshotEntry[];
};

/** The panel asked an agent and is waiting for it: ask again shortly. */
export type Pending = { pending: true };

export type RestoreMode = "original" | "path" | "download";

export const restoreModes: RestoreMode[] = ["original", "path", "download"];

export type RestoreStatus =
  | "previewing"
  | "previewed"
  | "queued"
  | "running"
  | "succeeded"
  | "failed"
  | "cancelled"
  | "expired";

export const restoreStatuses: RestoreStatus[] = [
  "previewing",
  "previewed",
  "queued",
  "running",
  "succeeded",
  "failed",
  "cancelled",
  "expired",
];

/** The steps of a real restore, as its agent reports them. */
export const restoreSteps = ["check", "stop", "restore", "start", "verify"] as const;

export type RestoreStep = (typeof restoreSteps)[number];

/** A root of a restore to the original location: a snapshot path, and the
 * volume or directory of the job's server it goes back into. */
export type RestoreRoot = { source: string; volume?: string; path?: string };

/** What the preview found for a root. */
export type PreviewRoot = {
  index: number;
  source: string;
  destination: string;
  in_snapshot: boolean;
  exists: boolean;
  /** what the destination holds, up to 10,000 */
  entries: number;
  entries_capped: boolean;
  uid: number;
  gid: number;
  /** the free space where a missing folder would be made */
  free_bytes: number;
};

type PerAction = { new: number; updated: number; deleted: number };

/** The agent's dry run of a restore. */
export type RestorePreview = {
  dry_run: true;
  snapshot: { id: string; time: string; paths: string[] };
  roots: PreviewRoot[];
  /** snapshot paths the job no longer backs up */
  not_in_job: string[] | null;
  counts: PerAction & { unchanged: number };
  bytes: PerAction;
  samples: { new: string[] | null; updated: string[] | null; deleted: string[] | null };
  /** the paths of each action left out of the samples */
  truncated: PerAction;
  would_stop: { name: string; project: string }[] | null;
  total_files: number;
  total_bytes: number;
  notes: string[] | null;
};

/** A restore or a download of a job's snapshot: the plan, the preview, and
 * the history. Operators read it; only the panel's routes write it. */
export type BackupRestore = RecordModel & {
  created: string;
  updated: string;
  job: string;
  target: string;
  /** the server it writes on, or whose agent sends the download */
  host: string;
  label: string;
  snapshot_id: string;
  snapshot_time: string;
  mode: RestoreMode;
  roots: RestoreRoot[] | null;
  path: string;
  source: string;
  include: string[] | null;
  replace: boolean;
  overwrite: boolean;
  stack_name: string;
  status: RestoreStatus;
  preview: RestorePreview | null;
  preview_at: string;
  started_at: string;
  finished_at: string;
  expires_at: string;
  preview_command: string;
  command: string;
  requested_by: string;
  requested_label: string;
  confirmed_by: string;
  confirmed_label: string;
  stop_requested_label: string;
  files_restored: number;
  bytes_restored: number;
  files_deleted: number;
  errors: number;
  bytes: number;
  partial: boolean;
  app_stopped: { id: string; name: string }[] | null;
  app_problem: string;
  error: string;
  log: string;
  download_name: string;
  download_kind: "dir" | "file" | "";
};

/** A restore as the panel takes it: a preview of it is queued at once. */
export type NewRestore = {
  target: string;
  /** the full id */
  snapshot: string;
  mode: "original" | "path";
  /** path mode: the server and its folder */
  host?: string;
  path?: string;
  /** path mode: the snapshot's folder restored; "/" is all of it */
  source?: string;
  /** the snapshot paths restored, when not all of it */
  include?: string[];
  replace?: boolean;
};

export type NewDownload = {
  target: string;
  snapshot: string;
  path: string;
  kind: "dir" | "file";
};

/** The record a restore route answers with: the record itself, or, as the
 * contract writes it, `{restore}`. */
function recordOf(answer: BackupRestore | { restore: BackupRestore }): BackupRestore {
  return "restore" in answer && answer.restore && typeof answer.restore === "object"
    ? answer.restore
    : (answer as BackupRestore);
}

function query(values: Record<string, string | number | undefined>): string {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(values)) {
    if (value !== undefined && value !== "") {
      params.set(key, String(value));
    }
  }
  return params.toString();
}

export const restoresApi = {
  /** a job's snapshots in a target, newest first: kept by the panel for 10
   * minutes, or read again with `refresh`; pending while an agent lists them */
  snapshots: (job: string, target: string, refresh = false) =>
    pb.send<SnapshotList | Pending>(
      `/api/pc/backup-jobs/${job}/snapshots?${query({ target, refresh: refresh ? 1 : undefined })}`,
      {},
    ),

  /** a page of a snapshot's folder; pending while an agent lists it */
  files: (
    job: string,
    target: string,
    snapshot: string,
    page: { path: string; offset?: number; limit?: number; match?: string },
  ) =>
    pb.send<SnapshotFolder | Pending>(
      `/api/pc/backup-jobs/${job}/snapshots/${snapshot}/files?${query({
        target,
        path: page.path,
        offset: page.offset || undefined,
        limit: page.limit,
        match: page.match,
      })}`,
      {},
    ),

  /** a restore's draft, whose preview the panel queues at once */
  create: async (job: string, body: NewRestore) =>
    recordOf(
      await pb.send<BackupRestore | { restore: BackupRestore }>(
        `/api/pc/backup-jobs/${job}/restores`,
        { method: "POST", body },
      ),
    ),

  /** starts a previewed restore; one that overwrites needs `confirm` (409
   * without it) */
  start: async (id: string, confirm: boolean) =>
    recordOf(
      await pb.send<BackupRestore | { restore: BackupRestore }>(
        `/api/pc/backup-restores/${id}/start`,
        { method: "POST", body: { confirm_overwrite: confirm } },
      ),
    ),

  /** stops a restore: one queued at once, one running once its agent has
   * started the application again */
  cancel: async (id: string) => {
    const answer = await pb.send<BackupRestore | { restore: BackupRestore } | { stopping: true }>(
      `/api/pc/backup-restores/${id}/cancel`,
      { method: "POST" },
    );
    return "stopping" in answer ? { stopping: true as const } : recordOf(answer);
  },

  /** removes a draft, cancelling its preview */
  discard: (id: string) => pb.send(`/api/pc/backup-restores/${id}`, { method: "DELETE" }),

  /** a download's record, and the link to open once, within a minute */
  download: async (job: string, body: NewDownload) => {
    const answer = await pb.send<{ restore: BackupRestore; url: string }>(
      `/api/pc/backup-jobs/${job}/downloads`,
      { method: "POST", body },
    );
    return { restore: answer.restore, url: answer.url };
  },
};

export function isPending(answer: unknown): answer is Pending {
  return Boolean(answer && typeof answer === "object" && (answer as Pending).pending === true);
}

/** A listing as a component shows it: what it gave, or that it is pending
 * or failed; reload asks again, and with `refresh` past the panel's cache. */
export type Listing<T> = {
  data: T | null;
  pending: boolean;
  loading: boolean;
  error: unknown;
  reload: (refresh?: boolean) => void;
};

/** A listing that the panel may answer as pending: asked again every 2
 * seconds until it is there. A new key starts afresh, and what an older one
 * answers is dropped. */
export function useListing<T>(
  key: string | null,
  load: (refresh: boolean) => Promise<T | Pending>,
): Listing<T> {
  const [state, setState] = useState<{
    key: string | null;
    data: T | null;
    pending: boolean;
    error: unknown;
    loading: boolean;
  }>({ key: null, data: null, pending: false, error: null, loading: false });
  const [version, setVersion] = useState(0);
  const loader = useRef(load);
  const refreshNext = useRef(false);

  useEffect(() => {
    loader.current = load;
  });

  useEffect(() => {
    if (!key) {
      return;
    }
    let live = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const refresh = refreshNext.current;
    refreshNext.current = false;
    setState((current) => ({
      key,
      // a reload keeps what is shown until the new answer is there
      data: current.key === key ? current.data : null,
      pending: false,
      error: null,
      loading: true,
    }));
    const ask = (first: boolean) => {
      loader
        .current(first && refresh)
        .then((answer) => {
          if (!live) {
            return;
          }
          if (isPending(answer)) {
            setState((current) => ({ ...current, pending: true }));
            timer = setTimeout(() => ask(false), 2000);
            return;
          }
          setState({ key, data: answer, pending: false, error: null, loading: false });
        })
        .catch((reason: unknown) => {
          if (live && !(reason as { isAbort?: boolean }).isAbort) {
            setState((current) => ({ ...current, pending: false, error: reason, loading: false }));
          }
        });
    };
    ask(true);
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [key, version]);

  const current = state.key === key && key !== null;
  return {
    data: current ? state.data : null,
    pending: current && state.pending,
    loading: key !== null && (!current || state.loading),
    error: current ? state.error : null,
    reload: (refresh = false) => {
      refreshNext.current = refresh;
      setVersion((value) => value + 1);
    },
  };
}

type Translate = (key: string, options?: Record<string, unknown>) => string;

/** When a snapshot was made, as short as it reads well: "today 12:00",
 * "yesterday 03:00", else its day and month with the time, and its year when
 * it is not this year's. In the browser's time zone unless one is given. */
export function formatSnapshotTime(
  value: string,
  now: number,
  timeZone: string | undefined,
  t: Translate,
  language = i18n.language || "en",
): string {
  const date = parseDate(value);
  if (!date) {
    return "–";
  }
  const time = new Intl.DateTimeFormat(language, {
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
    timeZone,
  }).format(date);
  const day = dayKey(date, timeZone);
  const [yesterday, today] = lastDays(now, 2, timeZone);
  if (day === today) {
    return t("backups.snapshot.today", { time });
  }
  if (day === yesterday) {
    return t("backups.snapshot.yesterday", { time });
  }
  return new Intl.DateTimeFormat(language, {
    day: "numeric",
    month: "short",
    year: day.slice(0, 4) === today.slice(0, 4) ? undefined : "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
    timeZone,
  }).format(date);
}

/** A snapshot's paths, clean, sorted and each once. */
export function snapshotRoots(paths: readonly string[] | null | undefined): string[] {
  const clean = (paths ?? [])
    .map((item) => {
      const parts = item.split("/").filter((part) => part !== "" && part !== ".");
      return `/${parts.join("/")}`;
    })
    .filter((item, index, all) => all.indexOf(item) === index);
  return clean.sort();
}

/** The folder a snapshot is browsed from: its one path, or the deepest
 * folder that holds all of them. */
export function browseStart(paths: readonly string[] | null | undefined): string {
  const roots = snapshotRoots(paths);
  if (roots.length === 0) {
    return "/";
  }
  if (roots.length === 1) {
    return roots[0];
  }
  const split = roots.map((root) => root.split("/").slice(1));
  const common: string[] = [];
  for (let index = 0; index < split[0].length; index++) {
    const part = split[0][index];
    if (split.every((parts) => parts.length > index && parts[index] === part)) {
      common.push(part);
    } else {
      break;
    }
  }
  return `/${common.join("/")}`;
}

/** Whether a snapshot path is a folder or lies under it. */
export function underPath(path: string, folder: string): boolean {
  return folder === "/" || path === folder || path.startsWith(`${folder}/`);
}

/** The parent folder of a snapshot path; / for /. */
export function parentPath(path: string): string {
  const index = path.lastIndexOf("/");
  return index <= 0 ? "/" : path.slice(0, index);
}

/** A path and a name in it. */
export function joinPath(folder: string, name: string): string {
  return folder === "/" ? `/${name}` : `${folder}/${name}`;
}

/** A step of the browser's breadcrumb: the folder it opens, and its name,
 * which is "Volume x" for a volume's folder. */
export type Crumb = { path: string; name: string; volume: boolean };

/** The breadcrumb of a snapshot's folder: the snapshot itself, then each
 * folder; /volumes/<name> is one step, the volume. */
export function pathCrumbs(path: string): Crumb[] {
  const parts = path.split("/").filter(Boolean);
  const crumbs: Crumb[] = [{ path: "/", name: "", volume: false }];
  let index = 0;
  while (index < parts.length) {
    if (index === 0 && parts[0] === "volumes" && parts.length > 1) {
      crumbs.push({ path: `/volumes/${parts[1]}`, name: parts[1], volume: true });
      index = 2;
      continue;
    }
    crumbs.push({
      path: `/${parts.slice(0, index + 1).join("/")}`,
      name: parts[index],
      volume: false,
    });
    index++;
  }
  return crumbs;
}

/** A snapshot path as people read it: /volumes/<name> is "Volume name". */
export function pathLabel(path: string, t: Translate): string {
  const volume = /^\/volumes\/([^/]+)$/.exec(path);
  return volume ? t("backups.files.volume", { name: volume[1] }) : path;
}

function sizeText(bytes: number): string {
  return bytes > 0 ? formatBytes(bytes) : `${formatNumber(0)} B`;
}

/** What a preview says would change, a phrase per action that changes
 * anything: new files, files overwritten, files deleted, files that match. */
export function previewParts(t: Translate, preview: RestorePreview): string[] {
  const counts = preview.counts ?? { new: 0, updated: 0, deleted: 0, unchanged: 0 };
  const bytes = preview.bytes ?? { new: 0, updated: 0, deleted: 0 };
  const parts: string[] = [];
  if (counts.new > 0) {
    parts.push(t("backups.restore.previewNew", { count: counts.new, size: sizeText(bytes.new) }));
  }
  if (counts.updated > 0) {
    parts.push(
      t("backups.restore.previewUpdated", {
        count: counts.updated,
        size: sizeText(bytes.updated),
      }),
    );
  }
  if (counts.deleted > 0) {
    parts.push(t("backups.restore.previewDeleted", { count: counts.deleted }));
  }
  if (counts.unchanged > 0) {
    parts.push(t("backups.restore.previewUnchanged", { count: counts.unchanged }));
  }
  return parts;
}

/** The preview as one sentence: its parts in a list, as the language joins
 * them, with a capital and a full stop. Empty when nothing would change. */
export function previewSentence(
  t: Translate,
  preview: RestorePreview,
  language = i18n.language || "en",
): string {
  const parts = previewParts(t, preview);
  if (parts.length === 0) {
    return "";
  }
  const text = new Intl.ListFormat(language, { type: "conjunction" }).format(parts);
  return `${text.charAt(0).toLocaleUpperCase(language)}${text.slice(1)}.`;
}

/** What a destination already holds: the items of its roots, and whether
 * there are more than were counted. */
export function existingEntries(preview: RestorePreview): { count: number; capped: boolean } {
  let count = 0;
  let capped = false;
  for (const root of preview.roots ?? []) {
    count += Math.max(0, root.entries || 0);
    capped ||= Boolean(root.entries_capped);
  }
  return { count, capped };
}

/** Whether starting a restore needs someone to confirm that it overwrites
 * what the destination holds, as the panel's /start decides it: always at
 * the original location, and elsewhere when the destination holds anything,
 * or the restore would change or delete a file. */
export function needsConfirm(preview: RestorePreview | null, mode: RestoreMode): boolean {
  if (mode === "original") {
    return true;
  }
  if (!preview) {
    return true;
  }
  return (
    (preview.roots ?? []).some((root) => (root.entries || 0) > 0 || root.entries_capped) ||
    (preview.counts?.updated ?? 0) > 0 ||
    (preview.counts?.deleted ?? 0) > 0
  );
}

/** The longest folder a restore writes into, in characters. */
export const maxRestorePath = 1000;

/** The system's directories, which a local target may not be in either. */
const systemDirs = [
  "/proc",
  "/sys",
  "/dev",
  "/run",
  "/var/run",
  "/var/lib/docker",
  "/boot",
  "/etc",
  "/bin",
  "/sbin",
  "/usr",
  "/lib",
  "/lib64",
];

/** PocketCloud's own directories, on the server and in the job container. */
const pocketCloudDirs = [
  "/var/lib/pocketcloud",
  "/run/pocketcloud",
  "/cache",
  "/repo",
  "/volumes",
  "/restore",
  "/host",
];

/** Why a folder is refused, as internal/backup/testdata/restore_paths.json
 * names it. */
export type RestorePathProblem = {
  refused: "path" | "long" | "root" | "system" | "pocketcloud" | "holds";
  /** the directory a system, pocketcloud or holds refusal is about */
  protected?: string;
};

/** Whether text holds a control character (Unicode's Cc: U+0000 to U+001F
 * and U+007F to U+009F), as Go's unicode.IsControl says. */
function hasControl(text: string): boolean {
  for (const char of text) {
    const code = char.codePointAt(0)!;
    if (code <= 0x1f || (code >= 0x7f && code <= 0x9f)) {
      return true;
    }
  }
  return false;
}

const loneSurrogate = /[\ud800-\udbff](?![\udc00-\udfff])|(?<![\ud800-\udbff])[\udc00-\udfff]/;

function under(path: string, dir: string): boolean {
  return path === dir || dir === "/" || path.startsWith(`${dir}/`);
}

function clean(path: string): boolean {
  if (path === "/") {
    return true;
  }
  return (
    path.startsWith("/") &&
    path
      .slice(1)
      .split("/")
      .every((part) => part !== "" && part !== "." && part !== "..")
  );
}

/** Why a server's folder may not take a restore, as the panel's
 * backup.RestorePath decides it; null when it may. */
export function restorePathProblem(path: string): RestorePathProblem | null {
  if (!clean(path) || loneSurrogate.test(path) || hasControl(path)) {
    return { refused: "path" };
  }
  if ([...path].length > maxRestorePath) {
    return { refused: "long" };
  }
  if (path === "/") {
    return { refused: "root" };
  }
  for (const [dirs, kind] of [
    [systemDirs, "system"],
    [pocketCloudDirs, "pocketcloud"],
  ] as const) {
    for (const dir of dirs) {
      if (under(path, dir)) {
        return { refused: kind, protected: dir };
      }
      if (under(dir, path)) {
        return { refused: "holds", protected: dir };
      }
    }
  }
  return null;
}

/** Whether a server's folder may take a restore: absolute and clean, not /,
 * not the system's nor PocketCloud's, nor a folder that holds one of them. */
export function validRestorePath(path: string): boolean {
  return restorePathProblem(path) === null;
}

const rank: Record<Role, number> = { viewer: 1, operator: 2, admin: 3 };

/** Whether a restore mode can be used, and if not, why. */
export type ModeState = {
  disabled: boolean;
  reason?: "dump" | "local" | "role" | "admin" | "panel";
};

/** The modes a person may restore a job's snapshot with from a target: the
 * original location for an operator, but not for a database dump, nor from a
 * local target of another server, nor of the panel itself, which is restored
 * on a machine of its own with its recovery kit (#32); a new folder or a
 * download for an admin only. The panel decides; this only hides what it
 * would refuse. */
export function modesFor(
  target: Pick<BackupTarget, "kind" | "host"> | null | undefined,
  job: Pick<BackupJob, "source_kind" | "host">,
  role: Role | null | undefined,
): Record<RestoreMode, ModeState> {
  const level = role ? rank[role] : -1;
  const admin = level >= rank.admin;
  let original: ModeState = { disabled: false };
  if (job.source_kind === "panel") {
    original = { disabled: true, reason: "panel" };
  } else if (job.source_kind === "db_dump") {
    original = { disabled: true, reason: "dump" };
  } else if (target?.kind === "local" && target.host !== job.host) {
    original = { disabled: true, reason: "local" };
  } else if (level < rank.operator) {
    original = { disabled: true, reason: "role" };
  }
  const elsewhere: ModeState = admin ? { disabled: false } : { disabled: true, reason: "admin" };
  return { original, path: { ...elsewhere }, download: { ...elsewhere } };
}

/** The keys a restore's mode, step or status is shown with, which the code
 * builds from the value. */
export const restoreKeys = [
  ...restoreModes.flatMap((mode) => [
    `backups.restore.mode.${mode}.label`,
    `backups.restore.mode.${mode}.note`,
  ]),
  ...restoreSteps.map((step) => `backups.restore.step.${step}`),
  ...restoreStatuses.map((status) => `backups.restore.status.${status}`),
];

/** The tone of a restore's status pill. */
export function restoreTone(status: RestoreStatus): "ok" | "err" | "warn" | "info" | "off" {
  switch (status) {
    case "succeeded":
      return "ok";
    case "failed":
      return "err";
    case "cancelled":
    case "expired":
      return "warn";
    case "queued":
    case "previewed":
      return "off";
    default:
      return "info";
  }
}

/** Whether a restore has ended. */
export function restoreEnded(status: RestoreStatus): boolean {
  return status === "succeeded" || status === "failed" || status === "cancelled";
}

/** Whether a restore is a draft, which nothing started: its preview is under
 * way, ready or too old. */
export function restoreDraft(status: RestoreStatus): boolean {
  return status === "previewing" || status === "previewed" || status === "expired";
}

/** A preview that failed: a draft too, which nothing started. */
export function failedDraft(restore: Pick<BackupRestore, "status" | "mode" | "command">): boolean {
  return restore.status === "failed" && restore.mode !== "download" && restore.command === "";
}

/** Whether a record is a draft as the panel counts them: a restore nothing
 * started, whose preview is under way, ready, too old or failed, or a
 * download whose link was never used. */
export function draftRecord(restore: Pick<BackupRestore, "status" | "mode" | "command">): boolean {
  return (
    restoreDraft(restore.status) ||
    failedDraft(restore) ||
    (restore.status === "queued" && restore.mode === "download" && restore.command === "")
  );
}

/** The filter of a job's records that are no drafts, as `draftRecord` tells
 * them: the panel leaves drafts out, so that they never take a page's rows. */
export function startedRestoresFilter(job: string): string {
  return pb.filter(
    "job = {:job} && status != 'previewing' && status != 'previewed' && status != 'expired'" +
      " && (status != 'failed' || mode = 'download' || command != '')" +
      " && (status != 'queued' || mode != 'download' || command != '')",
    { job },
  );
}

/** The filter of a job's restores under way, queued or running; a download
 * writes nothing of the job's. */
export function openRestoreFilter(job: string): string {
  return pb.filter(
    "job = {:job} && mode != 'download' && (status = 'queued' || status = 'running')",
    { job },
  );
}

/** A target a job's snapshots are listed from: one it backs up to, or one
 * it backed up to before, which is no longer used. */
export type SnapshotTarget = { target: BackupTarget; old: boolean };

/** The targets a job's snapshots are in: those it backs up to, in its
 * order, then by name those its runs went to that it no longer uses. */
export function snapshotSources<T extends Pick<BackupTarget, "id" | "name">>(
  job: Pick<BackupJob, "id" | "targets">,
  targets: T[],
  runs: { job: string; target: string }[],
): { target: T; old: boolean }[] {
  const current = job.targets ?? [];
  const byId = new Map(targets.map((target) => [target.id, target]));
  const sources = current.flatMap((id) => {
    const target = byId.get(id);
    return target ? [{ target, old: false }] : [];
  });
  const old = new Set<string>();
  for (const run of runs) {
    if (run.job === job.id && !current.includes(run.target) && byId.has(run.target)) {
      old.add(run.target);
    }
  }
  const before = [...old]
    .map((id) => byId.get(id)!)
    .sort((a, b) => a.name.localeCompare(b.name))
    .map((target) => ({ target, old: true }));
  return [...sources, ...before];
}

/** Whether a restore was started and has not ended: queued or running. */
export function restoreOpen(restore: Pick<BackupRestore, "status" | "mode">): boolean {
  return restore.status === "queued" || restore.status === "running";
}

/** What the document in a frame says, when it may be read: the message of
 * the panel's JSON error, which a browser shows in a <pre> beside controls
 * of its own, or else the document's text, as of a text/plain answer. */
function frameText(frame: HTMLIFrameElement): string {
  try {
    const body = frame.contentDocument?.body;
    const text = body?.textContent?.trim() ?? "";
    const shown = body?.querySelector("pre")?.textContent?.trim() ?? "";
    return panelMessage(shown) ?? panelMessage(text) ?? text;
  } catch {
    return "";
  }
}

/** The message of the panel's error, `{"data":{},"message":"…","status":403}`,
 * when a text is one. */
function panelMessage(text: string): string | null {
  if (!text.startsWith("{")) {
    return null;
  }
  try {
    const answer: unknown = JSON.parse(text);
    const message =
      typeof answer === "object" && answer !== null && "message" in answer ? answer.message : null;
    return typeof message === "string" && message.trim() !== "" ? message.trim() : null;
  } catch {
    return null;
  }
}

/** Opens a download's link in a hidden frame, so that an answer that is no
 * file stays out of sight and the app is never navigated away. The frame
 * stays on the page outside the app, so that closing what started it does
 * not cut the download; `onError` gets what an answer that is no file says,
 * such as a link that was used. */
export function openDownloadFrame(
  url: string,
  title: string,
  onError: (message: string) => void,
): HTMLIFrameElement {
  const frame = document.createElement("iframe");
  frame.hidden = true;
  frame.title = title;
  frame.className = "download-frame";
  frame.addEventListener("load", () => {
    // a file never loads a document; an error does, from the panel's origin
    const text = frameText(frame);
    if (text) {
      onError(text);
    }
  });
  frame.src = url;
  document.body.appendChild(frame);
  return frame;
}
