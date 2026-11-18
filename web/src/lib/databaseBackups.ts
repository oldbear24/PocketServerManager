// What the Databáze page shows of each database's backups (#53): its job,
// how its last backup went, the restore tests, the server's defaults and the
// temporary databases. Pure: the pages bring the records.
import i18n from "../i18n";
import { checkEnded, type BackupCheck } from "./backupChecks";
import {
  cronPreset,
  dayKey,
  describeCron,
  lastRun,
  runMoment,
  sourceGone,
  timeValue,
  zoneOffset,
  type BackupJob,
  type BackupRun,
  type RunStatus,
} from "./backupJobs";
import type { Database, DbServer } from "./databases";
import { parseDate } from "./format";

type Translate = (key: string, options?: Record<string, unknown>) => string;

/** The temporary namespace of restores and restore tests, as the panel
 * keeps it (schema.TemporaryDatabase). */
export const temporaryName = /^pc_(rt|rs|old)_[a-z0-9]{15}$/;

/** The prefixes of the temporary namespace, which a new database's name
 * may not start with. */
export const reservedPrefixes = ["pc_rt_", "pc_rs_", "pc_old_"];

/** Whether a name starts with a prefix of PocketCloud's temporary databases. */
export function reservedName(name: string): boolean {
  return reservedPrefixes.some((prefix) => name.startsWith(prefix));
}

/** Whether a database is a temporary one of a restore or a restore test. */
export function isTemporary(database: Pick<Database, "temporary_until" | "name">): boolean {
  return Boolean(database.temporary_until) || temporaryName.test(database.name);
}

/** What a temporary database is: a restore test's copy, an in-place
 * restore's scratch copy, the data such a restore replaced, or a name of the
 * namespace that nothing of the panel's made. */
export type TemporaryKind = "test" | "scratch" | "previous" | "orphan";

export function temporaryKind(name: string): TemporaryKind {
  const match = temporaryName.exec(name);
  if (!match) {
    return "orphan";
  }
  return match[1] === "rt" ? "test" : match[1] === "rs" ? "scratch" : "previous";
}

/** The record id a temporary database's name holds: the check of a test, the
 * restore of a scratch or previous copy. */
export function temporaryOwner(name: string): string {
  return temporaryName.test(name) ? name.slice(name.lastIndexOf("_") + 1) : "";
}

/** A database's own job (origin database), if the panel made one. */
export function jobOf<J extends Pick<BackupJob, "origin" | "database">>(
  database: Pick<Database, "id">,
  jobs: J[],
): J | null {
  return jobs.find((job) => job.origin === "database" && job.database === database.id) ?? null;
}

/** The job that backs up a dedicated server: its stack's dump job of the
 * server's service, else of the stack, else null. A dedicated server's
 * databases stay their stack's business (#53 D1). */
export function stackJobOf<J extends Pick<BackupJob, "source_kind" | "stack" | "service">>(
  server: Pick<DbServer, "stack" | "service">,
  jobs: J[],
): J | null {
  if (!server.stack) {
    return null;
  }
  const dumps = jobs.filter((job) => job.source_kind === "db_dump" && job.stack === server.stack);
  return dumps.find((job) => job.service === server.service) ?? dumps[0] ?? null;
}

/** How a database's backups stand, as its cell says it: the newest run's
 * state, or the job's being off, without a target, or never run. */
export type CellState = RunStatus | "ok" | "never" | "noTarget" | "off";

export type BackupCell = {
  state: Exclude<CellState, "succeeded">;
  /** the pill's tone; "muted" is plain text */
  tone: "ok" | "err" | "warn" | "info" | "off" | "muted";
  /** when, for a run that ended */
  at: string;
};

type CellJob = Pick<
  BackupJob,
  "id" | "targets" | "enabled" | "last_status" | "last_run_at" | "label_removed"
> &
  Partial<Pick<BackupJob, "origin" | "database" | "site" | "last_success_at">>;

type CellRun = Pick<
  BackupRun,
  "job" | "target" | "status" | "created" | "finished_at" | "started_at" | "scheduled_at"
> &
  Partial<Pick<BackupRun, "retention">>;

const toneOf: Record<BackupCell["state"], BackupCell["tone"]> = {
  ok: "ok",
  failed: "err",
  skipped: "warn",
  cancelled: "off",
  running: "info",
  queued: "info",
  never: "muted",
  noTarget: "warn",
  off: "off",
};

/** A job's Backup cell: off when it no longer runs (a dropped database's
 * job is off), no target until one is chosen, else how its newest runs went,
 * the worst of its targets' (`lastRun`); older than the runs loaded, what the
 * job says of its last run, or its last success. */
export function backupCell(job: CellJob, runs: CellRun[]): BackupCell {
  const cell = (state: BackupCell["state"], at = ""): BackupCell => ({
    state,
    tone: toneOf[state],
    at,
  });
  if (!job.enabled || sourceGone(job)) {
    return cell("off");
  }
  if ((job.targets ?? []).length === 0) {
    return cell("noTarget");
  }
  const run = lastRun(job, runs);
  if (run) {
    const state = run.status === "succeeded" ? "ok" : run.status;
    return cell(state, run.status === "queued" || run.status === "running" ? "" : runMoment(run));
  }
  if (job.last_status) {
    const state = job.last_status === "succeeded" ? "ok" : job.last_status;
    return cell(state, job.last_run_at);
  }
  if (parseDate(job.last_success_at ?? "")) {
    return cell("ok", job.last_success_at ?? "");
  }
  return cell("never");
}

/** A cell's time: the local time today, with the date when older. */
export function cellTime(value: string, now: number, timeZone?: string): string {
  const date = parseDate(value);
  if (!date) {
    return "–";
  }
  const time: Intl.DateTimeFormatOptions = {
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
    timeZone,
  };
  const language = i18n.language || "en";
  if (dayKey(date, timeZone) === dayKey(new Date(now), timeZone)) {
    return new Intl.DateTimeFormat(language, time).format(date);
  }
  return new Intl.DateTimeFormat(language, { ...time, day: "numeric", month: "numeric" }).format(
    date,
  );
}

const day = 24 * 3600 * 1000;

/** The moment from which the runs of the last `days` days are loaded, at
 * midnight UTC, so that it stays the same all day. */
export function runsSinceDays(now: number, days: number): string {
  return `${new Date(now - days * day).toISOString().slice(0, 10)} 00:00:00.000Z`;
}

/** Whether a job backed up within the last 24 hours: its last success, or a
 * run that succeeded. */
export function backedUpWithin(
  job: Pick<BackupJob, "id"> & Partial<Pick<BackupJob, "last_success_at">>,
  runs: Pick<BackupRun, "job" | "status" | "finished_at" | "created">[],
  now: number,
): boolean {
  const since = now - day;
  const last = parseDate(job.last_success_at ?? "");
  if (last && last.getTime() >= since) {
    return true;
  }
  return runs.some(
    (run) =>
      run.job === job.id &&
      run.status === "succeeded" &&
      (parseDate(run.finished_at || run.created)?.getTime() ?? 0) >= since,
  );
}

/** The KPI "Backups in 24 h": of the ready databases of shared servers that
 * are not temporary, those backed up within a day; how many of them last
 * failed, and how many have no target. Dedicated servers' databases are left
 * out: their stacks back them up (#53 D1). */
export type BackupKpi = {
  done: number;
  total: number;
  failed: number;
  noTarget: number;
  tone: "ok" | "warn" | "off" | "none";
};

export function backupKpi(
  databases: Pick<Database, "id" | "server" | "status" | "temporary_until" | "name">[],
  servers: Pick<DbServer, "id" | "kind">[],
  jobs: (CellJob & Pick<BackupJob, "origin">)[],
  now: number,
  runs: (CellRun & Pick<BackupRun, "finished_at">)[] = [],
): BackupKpi {
  const shared = new Set(servers.filter((item) => item.kind === "shared").map((item) => item.id));
  const counted = databases.filter(
    (item) => item.status === "ready" && !isTemporary(item) && shared.has(item.server),
  );
  let done = 0;
  let failed = 0;
  let noTarget = 0;
  for (const database of counted) {
    const job = jobOf(database, jobs);
    if (!job) {
      continue;
    }
    if (backedUpWithin(job, runs, now)) {
      done++;
    }
    const state = backupCell(job, runs).state;
    if (state === "failed") {
      failed++;
    }
    if (state === "noTarget") {
      noTarget++;
    }
  }
  const total = counted.length;
  let tone: BackupKpi["tone"] = "none";
  if (total === 0) {
    tone = "off";
  } else if (failed > 0 || noTarget > 0) {
    tone = "warn";
  } else if (done === total) {
    tone = "ok";
  }
  return { done, total, failed, noTarget, tone };
}

/** The newest restore test of a job; with `ended`, the newest that ended. */
export function latestTest<
  C extends Pick<BackupCheck, "kind" | "status" | "created"> & { job?: string },
>(checks: C[], jobId: string, ended = false): C | null {
  let newest: C | null = null;
  for (const check of checks) {
    if (check.kind !== "restore_test" || check.job !== jobId || (ended && !checkEnded(check))) {
      continue;
    }
    if (!newest || check.created > newest.created) {
      newest = check;
    }
  }
  return newest;
}

const cronMacros: Record<string, string> = {
  "@yearly": "0 0 1 1 *",
  "@annually": "0 0 1 1 *",
  "@monthly": "0 0 1 * *",
  "@weekly": "0 0 * * 0",
  "@daily": "0 0 * * *",
  "@midnight": "0 0 * * *",
  "@hourly": "0 * * * *",
};

/** The values a cron field allows, from min to max: *, numbers, ranges,
 * steps and lists, as PocketBase reads them; null when it is none. */
function cronValues(field: string, min: number, max: number): Set<number> | null {
  const values = new Set<number>();
  for (const part of field.split(",")) {
    const match = /^(\*|(\d+)(?:-(\d+))?)(?:\/(\d+))?$/.exec(part);
    if (!match) {
      return null;
    }
    const from = match[1] === "*" ? min : Number(match[2]);
    const to = match[1] === "*" ? max : match[3] !== undefined ? Number(match[3]) : from;
    const step = match[4] !== undefined ? Number(match[4]) : 1;
    if (from < min || to > max || from > to || step < 1) {
      return null;
    }
    for (let value = from; value <= to; value += step) {
      values.add(value);
    }
  }
  return values;
}

/** The first minute after a moment that a UTC cron expression is due, as
 * the panel's cronRule.next finds it (both day fields restricted: either
 * day is enough), within a year; null for none, or for no expression. */
export function nextCron(cron: string, after: number): number | null {
  const text = cron.trim();
  const fields = (cronMacros[text] ?? text).split(/\s+/);
  if (fields.length !== 5) {
    return null;
  }
  const minutes = cronValues(fields[0], 0, 59);
  const hours = cronValues(fields[1], 0, 23);
  const days = cronValues(fields[2], 1, 31);
  const months = cronValues(fields[3], 1, 12);
  const weekdays = cronValues(fields[4], 0, 7);
  if (!minutes || !hours || !days || !months || !weekdays) {
    return null;
  }
  if (weekdays.has(7)) {
    weekdays.add(0);
  }
  const either = fields[2] !== "*" && fields[4] !== "*";
  const dayDue = (at: Date) =>
    either
      ? days.has(at.getUTCDate()) || weekdays.has(at.getUTCDay())
      : days.has(at.getUTCDate()) && weekdays.has(at.getUTCDay());
  const minute = 60_000;
  let at = new Date(Math.floor(after / minute) * minute + minute);
  const limit = at.getTime() + 366 * day;
  while (at.getTime() < limit) {
    if (!months.has(at.getUTCMonth() + 1)) {
      at = new Date(Date.UTC(at.getUTCFullYear(), at.getUTCMonth() + 1, 1));
    } else if (!dayDue(at)) {
      at = new Date(Date.UTC(at.getUTCFullYear(), at.getUTCMonth(), at.getUTCDate() + 1));
    } else if (!hours.has(at.getUTCHours())) {
      at = new Date(
        Date.UTC(at.getUTCFullYear(), at.getUTCMonth(), at.getUTCDate(), at.getUTCHours() + 1),
      );
    } else if (minutes.has(at.getUTCMinutes())) {
      return at.getTime();
    } else {
      at = new Date(at.getTime() + minute);
    }
  }
  return null;
}

/** The FNV-1a hash of a text's UTF-8 bytes, 32 bits. */
function fnv32a(text: string): number {
  let hash = 0x811c9dc5;
  for (const byte of new TextEncoder().encode(text)) {
    hash ^= byte;
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return hash >>> 0;
}

/** A server's default schedule when its auto_backup_schedule is empty, as
 * the panel's backup.AutoSchedule makes it: daily at a quarter of an hour
 * from 01:00 to 02:45 UTC that its id picks. */
export function autoSchedule(hostId: string): string {
  const k = fnv32a(hostId) % 8;
  return `${15 * (k % 4)} ${1 + Math.floor(k / 4)} * * *`;
}

/** A server's schedule for its databases' jobs: its own, or the default. */
export function hostSchedule(host: { id: string; auto_backup_schedule?: string }): string {
  return host.auto_backup_schedule?.trim() || autoSchedule(host.id);
}

/** The local time of a daily schedule ("01:30"), or "" when it is not daily. */
export function dailyTime(cron: string, offset = zoneOffset()): string {
  const preset = cronPreset(cron, offset);
  return preset?.kind === "daily" ? timeValue(preset.hour, preset.minute) : "";
}

/** The hint a shared server's card gives of its databases' backups: daily at
 * a time, at a schedule in words, at several times, or that they have no
 * target yet (a warning). Null for a server with no database job. */
export type HostHint = { text: string; warn: boolean };

export function hostHint(
  jobs: Pick<BackupJob, "schedule" | "targets" | "enabled">[],
  t: Translate,
  offset = zoneOffset(),
): HostHint | null {
  const running = jobs.filter((job) => job.enabled);
  if (running.length === 0) {
    return null;
  }
  if (running.some((job) => (job.targets ?? []).length === 0)) {
    return { text: t("db.backupHintNoTarget"), warn: true };
  }
  const schedules = new Set(running.map((job) => job.schedule.trim()));
  if (schedules.size > 1) {
    return { text: t("db.backupHintMixed"), warn: false };
  }
  const [schedule] = [...schedules];
  const time = dailyTime(schedule, offset);
  return time
    ? { text: t("db.backupHint", { time }), warn: false }
    : {
        text: t("db.backupHintCron", { schedule: describeCron(schedule, t, offset) }),
        warn: false,
      };
}
