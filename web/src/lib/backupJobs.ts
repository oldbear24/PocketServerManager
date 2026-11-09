import type { RecordModel } from "pocketbase";

import i18n from "../i18n";
import { parseDate } from "./format";
import { pb } from "./pb";

/** What a job backs up (#29). */
export type SourceKind = "volume" | "path" | "stack" | "db_dump";

export const sourceKinds: SourceKind[] = ["volume", "path", "stack", "db_dump"];

/** Whose tools make a database dump, in the service's own container. */
export type DumpEngine = "postgres" | "mysql" | "sqlite";

export const dumpEngines: DumpEngine[] = ["postgres", "mysql", "sqlite"];

export type RunStatus = "queued" | "running" | "succeeded" | "failed" | "skipped" | "cancelled";

/** restic's --keep-* options, in the order the job's dialog asks for them. */
export const retentionKeys = ["last", "hourly", "daily", "weekly", "monthly", "yearly"] as const;

export type RetentionKey = (typeof retentionKeys)[number];

/** How many snapshots restic keeps of each kind; 0 keeps none for it, and all
 * 0 keeps every snapshot. */
export type Retention = Record<RetentionKey, number>;

/** As the panel takes them. */
export const maxRetention = 10000;

/** A backup job: what is backed up, from which server, to which targets, when
 * (a cron expression in UTC) and how long. A label's job keeps what its labels
 * say. */
export type BackupJob = RecordModel & {
  name: string;
  host: string;
  source_kind: SourceKind;
  /** a volume's name, a directory of the server, or a SQLite file in the
   * service's container */
  source: string;
  stack: string;
  service: string;
  engine: DumpEngine | "";
  targets: string[] | null;
  schedule: string;
  retention: Partial<Retention> | null;
  enabled: boolean;
  origin: "manual" | "label";
  /** of a label's job, what its labels set besides its source: any of
   * "schedule", "targets" and "retention". The panel refuses to change them. */
  label_sets?: string[] | null;
  /** a label's job whose label is gone: kept, but it no longer runs */
  label_removed: boolean;
  /** what keeps the job from running, as the panel says it */
  problem: string;
  next_run_at: string;
  last_run_at: string;
  last_status: RunStatus | "";
};

/** One run of a job, to one target. */
export type BackupRun = RecordModel & {
  created: string;
  updated: string;
  job: string;
  target: string;
  host: string;
  trigger: "schedule" | "manual";
  status: RunStatus;
  scheduled_at: string;
  started_at: string;
  finished_at: string;
  duration_ms: number;
  snapshot_id: string;
  bytes_added: number;
  total_bytes: number;
  files_new: number;
  files_changed: number;
  forgotten: number;
  /** restic's error, or why the run did not start */
  error: string;
};

/** What the Jobs card reads of a run: never its log, which can be long. */
export const runFields =
  "id,collectionId,collectionName,created,updated,job,target,status,trigger,scheduled_at,started_at,finished_at,bytes_added,error";

/** A new job, as the panel takes it: only the fields of its source kind. */
export type NewBackupJob = {
  name: string;
  host: string;
  source_kind: SourceKind;
  source?: string;
  stack?: string;
  service?: string;
  /** "" only to clear it, when a job becomes another kind */
  engine?: DumpEngine | "";
  targets: string[];
  schedule: string;
  retention: Retention;
  enabled: boolean;
};

/** What changes on a job: only what was changed. */
export type BackupJobChange = Partial<NewBackupJob>;

export const jobsApi = {
  add: (body: NewBackupJob) => pb.send<BackupJob>("/api/pc/backup-jobs", { method: "POST", body }),

  change: (id: string, body: BackupJobChange) =>
    pb.send<BackupJob>(`/api/pc/backup-jobs/${id}`, { method: "PATCH", body }),

  /** its runs go with it; its snapshots stay in the repositories */
  remove: (id: string) => pb.send(`/api/pc/backup-jobs/${id}`, { method: "DELETE" }),

  /** a run per target, now; a target whose run before is still going is left out */
  run: (id: string) =>
    pb.send<{ runs: string[] }>(`/api/pc/backup-jobs/${id}/run`, { method: "POST" }),
};

type Translate = (key: string, options?: Record<string, unknown>) => string;

const pad = (value: number) => String(value).padStart(2, "0");

const mod = (value: number, base: number) => ((value % base) + base) % base;

/** The minutes local time is ahead of UTC at a moment: 60 in Prague's winter,
 * -300 in New York's. */
export function zoneOffset(at: Date = new Date()): number {
  // 0 - rather than -, so that UTC is 0 and not -0
  return 0 - at.getTimezoneOffset();
}

/** The schedules the job's dialog offers, in local time. */
export type SchedulePreset =
  | { kind: "hourly"; minute: number }
  | { kind: "daily"; hour: number; minute: number }
  | { kind: "weekly"; weekday: number; hour: number; minute: number };

const minutesPerDay = 24 * 60;
const minutesPerWeek = 7 * minutesPerDay;

/** A preset in local time as the cron expression the panel keeps, in UTC,
 * for a zone that is `offset` minutes ahead of UTC. */
export function presetCron(preset: SchedulePreset, offset: number): string {
  switch (preset.kind) {
    case "hourly":
      return `${mod(preset.minute - offset, 60)} * * * *`;
    case "daily": {
      const at = mod(preset.hour * 60 + preset.minute - offset, minutesPerDay);
      return `${at % 60} ${Math.floor(at / 60)} * * *`;
    }
    case "weekly": {
      const at = mod(
        preset.weekday * minutesPerDay + preset.hour * 60 + preset.minute - offset,
        minutesPerWeek,
      );
      const time = at % minutesPerDay;
      return `${time % 60} ${Math.floor(time / 60)} * * ${Math.floor(at / minutesPerDay)}`;
    }
  }
}

const macros: Record<string, string> = {
  "@hourly": "0 * * * *",
  "@daily": "0 0 * * *",
  "@midnight": "0 0 * * *",
  "@weekly": "0 0 * * 0",
  "@monthly": "0 0 1 * *",
  "@yearly": "0 0 1 1 *",
  "@annually": "0 0 1 1 *",
};

function cronFields(cron: string): string[] | null {
  const text = cron.trim();
  const fields = (macros[text] ?? text).split(/\s+/);
  return fields.length === 5 ? fields : null;
}

/** A whole number from 0 to max, as cron writes it; null for anything else. */
function cronNumber(field: string, max: number): number | null {
  return /^\d{1,2}$/.test(field) && Number(field) <= max ? Number(field) : null;
}

/** The preset a UTC cron expression is, in local time; null when it is none,
 * such as several hours or weekdays. */
export function cronPreset(cron: string, offset: number): SchedulePreset | null {
  const fields = cronFields(cron);
  if (!fields) {
    return null;
  }
  const [minuteField, hourField, day, month, weekdayField] = fields;
  const minute = cronNumber(minuteField, 59);
  if (minute === null || day !== "*" || month !== "*") {
    return null;
  }
  if (hourField === "*") {
    return weekdayField === "*" ? { kind: "hourly", minute: mod(minute + offset, 60) } : null;
  }
  const hour = cronNumber(hourField, 23);
  if (hour === null) {
    return null;
  }
  if (weekdayField === "*") {
    const at = mod(hour * 60 + minute + offset, minutesPerDay);
    return { kind: "daily", hour: Math.floor(at / 60), minute: at % 60 };
  }
  // 7 is Sunday too
  const weekday = cronNumber(weekdayField, 7);
  if (weekday === null) {
    return null;
  }
  const at = mod((weekday % 7) * minutesPerDay + hour * 60 + minute + offset, minutesPerWeek);
  const time = at % minutesPerDay;
  return {
    kind: "weekly",
    weekday: Math.floor(at / minutesPerDay),
    hour: Math.floor(time / 60),
    minute: time % 60,
  };
}

/** A preset in words, in local time: hourly, daily 03:00, Mondays 03:00. */
export function describePreset(preset: SchedulePreset, t: Translate): string {
  if (preset.kind === "hourly") {
    return preset.minute === 0
      ? t("backups.cron.hourly")
      : t("backups.cron.hourlyAt", { minute: pad(preset.minute) });
  }
  const time = `${pad(preset.hour)}:${pad(preset.minute)}`;
  return preset.kind === "daily"
    ? t("backups.cron.daily", { time })
    : t(`backups.cron.weekly.${preset.weekday}`, { time });
}

/** A UTC cron expression in words, in local time, where it is a common one;
 * else as it is written, saying it is UTC. */
export function describeCron(cron: string, t: Translate, offset: number): string {
  const preset = cronPreset(cron, offset);
  if (preset) {
    return describePreset(preset, t);
  }
  const fields = cronFields(cron);
  if (fields) {
    const [minuteField, hourField, day, month, weekday] = fields;
    const step = /^\*\/(\d+)$/;
    const minuteStep = step.exec(minuteField);
    // */7 runs at :00, :07 … :56 and again at :00, which is no "every 7 min"
    if (minuteField === "*" || minuteStep) {
      const count = minuteStep ? Number(minuteStep[1]) : 1;
      if (
        hourField === "*" &&
        day === "*" &&
        month === "*" &&
        weekday === "*" &&
        count >= 1 &&
        60 % count === 0
      ) {
        return t("backups.cron.minutes", { count });
      }
    }
    const minute = cronNumber(minuteField, 59);
    const hourStep = step.exec(hourField);
    if (minute !== null && hourStep && day === "*" && month === "*" && weekday === "*") {
      const count = Number(hourStep[1]);
      if (count >= 1 && 24 % count === 0) {
        return t("backups.cron.hours", { count });
      }
    }
    const hour = cronNumber(hourField, 23);
    const date = /^([1-9]|[12]\d|3[01])$/.test(day) ? Number(day) : null;
    if (minute !== null && hour !== null && date !== null && month === "*" && weekday === "*") {
      const at = hour * 60 + minute + offset;
      // in local time on another day of the month, which a day number cannot say
      if (at >= 0 && at < minutesPerDay) {
        return t("backups.cron.monthly", {
          day: date,
          time: `${pad(Math.floor(at / 60))}:${pad(at % 60)}`,
        });
      }
    }
  }
  return t("backups.cron.utc", { cron: cron.trim() });
}

/** "03:00" from an hour and a minute, as a time input takes it. */
export function timeValue(hour: number, minute: number): string {
  return `${pad(hour)}:${pad(minute)}`;
}

/** An hour and a minute from what a time input holds; null when it holds none. */
export function parseTime(value: string): { hour: number; minute: number } | null {
  const match = /^(\d{1,2}):(\d{2})/.exec(value.trim());
  if (!match) {
    return null;
  }
  const hour = Number(match[1]);
  const minute = Number(match[2]);
  return hour <= 23 && minute <= 59 ? { hour, minute } : null;
}

/** A job's retention with every key, 0 where it keeps nothing of that kind. */
export function fullRetention(retention: Partial<Retention> | null | undefined): Retention {
  const full = {} as Retention;
  for (const key of retentionKeys) {
    const value = retention?.[key];
    full[key] = typeof value === "number" && value > 0 ? Math.floor(value) : 0;
  }
  return full;
}

/** Whether two retentions keep the same. */
export function sameRetention(a: Partial<Retention> | null, b: Partial<Retention> | null): boolean {
  const first = fullRetention(a);
  const second = fullRetention(b);
  return retentionKeys.every((key) => first[key] === second[key]);
}

const shorthandUnits: [RetentionKey, string][] = [
  ["hourly", "h"],
  ["daily", "d"],
  ["weekly", "w"],
  ["monthly", "m"],
  ["yearly", "y"],
];

/** A retention as its label writes it: "24h 7d 4w 6m", "last 20", or both. */
export function describeRetention(
  retention: Partial<Retention> | null | undefined,
  t: Translate,
): string {
  const full = fullRetention(retention);
  const parts: string[] = [];
  if (full.last > 0) {
    parts.push(t("backups.retention.last", { count: full.last }));
  }
  const spans = shorthandUnits
    .filter(([key]) => full[key] > 0)
    .map(([key, unit]) => `${full[key]}${unit}`)
    .join(" ");
  if (spans) {
    parts.push(spans);
  }
  return parts.length > 0 ? parts.join(" · ") : t("backups.retention.all");
}

/** A retention's number as typed: empty is 0, anything but a whole number from
 * 0 to 10000 is null. */
export function parseKeep(text: string): number | null {
  const value = text.trim();
  if (value === "") {
    return 0;
  }
  return /^\d{1,5}$/.test(value) && Number(value) <= maxRetention ? Number(value) : null;
}

const dayFormats = new Map<string, Intl.DateTimeFormat>();

/** The calendar day of a moment in a time zone (the browser's by default), as
 * "2026-11-09". */
export function dayKey(at: Date, timeZone?: string): string {
  const zone = timeZone ?? "";
  let format = dayFormats.get(zone);
  if (!format) {
    format = new Intl.DateTimeFormat("en-US", {
      timeZone,
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
    });
    dayFormats.set(zone, format);
  }
  const parts = format.formatToParts(at);
  const part = (type: Intl.DateTimeFormatPartTypes) =>
    parts.find((item) => item.type === type)?.value ?? "";
  return `${part("year")}-${part("month")}-${part("day")}`;
}

/** The last `count` calendar days up to today in a time zone, oldest first. */
export function lastDays(now: number, count = 14, timeZone?: string): string[] {
  const [year, month, day] = dayKey(new Date(now), timeZone).split("-").map(Number);
  return Array.from({ length: count }, (_, index) =>
    new Date(Date.UTC(year, month - 1, day - (count - 1 - index))).toISOString().slice(0, 10),
  );
}

/** The moment from which the runs of the last 14 days are loaded: a day more,
 * at midnight UTC, so that it stays the same all day in any zone. */
export function runsSince(now: number): string {
  const day = new Date(now - 15 * 24 * 3600 * 1000).toISOString().slice(0, 10);
  return `${day} 00:00:00.000Z`;
}

export type StripState = "ok" | "err" | "warn" | "off";

/** One day of the 14-day strip: its runs that ended, and its colour, the
 * worst of them: failed, then skipped, then succeeded. */
export type StripDay = {
  day: string;
  succeeded: number;
  failed: number;
  skipped: number;
  state: StripState;
};

type CountedRun = Pick<BackupRun, "created" | "status">;

/** The runs of the last `count` days, by the local day they were created on,
 * oldest first. */
export function runStrip(
  runs: CountedRun[],
  now: number,
  timeZone?: string,
  count = 14,
): StripDay[] {
  const days = lastDays(now, count, timeZone);
  const index = new Map(days.map((day, position) => [day, position]));
  const cells: StripDay[] = days.map((day) => ({
    day,
    succeeded: 0,
    failed: 0,
    skipped: 0,
    state: "off",
  }));
  for (const run of runs) {
    const at = parseDate(run.created);
    const position = at ? index.get(dayKey(at, timeZone)) : undefined;
    if (position === undefined) {
      continue;
    }
    const cell = cells[position];
    if (run.status === "succeeded" || run.status === "failed" || run.status === "skipped") {
      cell[run.status]++;
    }
  }
  for (const cell of cells) {
    cell.state =
      cell.failed > 0 ? "err" : cell.skipped > 0 ? "warn" : cell.succeeded > 0 ? "ok" : "off";
  }
  return cells;
}

/** How the runs of the last 14 local days went: those that succeeded, of all
 * that succeeded, failed or were skipped. Queued, running and cancelled runs
 * count for neither. */
export function successRate(
  runs: CountedRun[],
  now: number,
  timeZone?: string,
): { succeeded: number; total: number } {
  let succeeded = 0;
  let total = 0;
  for (const day of runStrip(runs, now, timeZone)) {
    succeeded += day.succeeded;
    total += day.succeeded + day.failed + day.skipped;
  }
  return { succeeded, total };
}

/** The share that succeeded, in whole percent, never 100 while one did not:
 * null when there was nothing. */
export function successPercent(succeeded: number, total: number): number | null {
  if (total <= 0) {
    return null;
  }
  return succeeded >= total ? 100 : Math.floor((succeeded / total) * 100);
}

const statusRank: Record<RunStatus, number> = {
  failed: 0,
  skipped: 1,
  cancelled: 2,
  running: 3,
  queued: 4,
  succeeded: 5,
};

/** The run a job's Last run shows: the newest of each of its targets, and of
 * those the worst, so that one target's failure is not hidden behind
 * another's success. null when no run is loaded. */
export function lastRun<R extends Pick<BackupRun, "job" | "target" | "status" | "created">>(
  job: Pick<BackupJob, "id" | "targets">,
  runs: R[],
): R | null {
  const targets = job.targets ?? [];
  const newest = new Map<string, R>();
  for (const run of runs) {
    if (run.job !== job.id || (targets.length > 0 && !targets.includes(run.target))) {
      continue;
    }
    const seen = newest.get(run.target);
    if (!seen || run.created > seen.created) {
      newest.set(run.target, run);
    }
  }
  let shown: R | null = null;
  for (const run of newest.values()) {
    if (
      !shown ||
      statusRank[run.status] < statusRank[shown.status] ||
      (statusRank[run.status] === statusRank[shown.status] && run.created > shown.created)
    ) {
      shown = run;
    }
  }
  return shown;
}

/** When a run happened: when it ended, started, was due or was made. */
export function runMoment(
  run: Pick<BackupRun, "finished_at" | "started_at" | "scheduled_at" | "created">,
): string {
  return run.finished_at || run.started_at || run.scheduled_at || run.created;
}

/** The first line of an error, for a cell; the rest is in its title. */
export function shortError(error: string): string {
  const line = error
    .split(/\r?\n/)
    .map((item) => item.trim())
    .find((item) => item !== "");
  return line ?? "";
}

/** The job whose next run is the earliest, of those that run. */
export function nextJob<J extends Pick<BackupJob, "enabled" | "label_removed" | "next_run_at">>(
  jobs: J[],
): J | null {
  let next: J | null = null;
  for (const job of jobs) {
    if (!job.enabled || job.label_removed || !parseDate(job.next_run_at)) {
      continue;
    }
    if (!next || job.next_run_at < next.next_run_at) {
      next = job;
    }
  }
  return next;
}

function locale() {
  return i18n.language || "en";
}

/** A moment near now, as short as it can be: 13:00 today, Tue 03:00 within a
 * week, else with its date. In the browser's time zone unless one is given. */
export function formatMoment(value: string, now: number, timeZone?: string): string {
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
  if (dayKey(date, timeZone) === dayKey(new Date(now), timeZone)) {
    return new Intl.DateTimeFormat(locale(), time).format(date);
  }
  if (Math.abs(date.getTime() - now) < 6 * 24 * 3600 * 1000) {
    return new Intl.DateTimeFormat(locale(), { ...time, weekday: "short" }).format(date);
  }
  return new Intl.DateTimeFormat(locale(), { ...time, day: "numeric", month: "numeric" }).format(
    date,
  );
}

/** A day of the strip, such as "Sun 8. 11.", for its tooltip. */
export function formatStripDay(day: string): string {
  const [year, month, date] = day.split("-").map(Number);
  return new Intl.DateTimeFormat(locale(), {
    weekday: "short",
    day: "numeric",
    month: "numeric",
    timeZone: "UTC",
  }).format(new Date(Date.UTC(year, month - 1, date)));
}

/** "84%" or "84 %", as the language writes it. */
export function formatPercent(percent: number): string {
  return new Intl.NumberFormat(locale(), { style: "percent", maximumFractionDigits: 0 }).format(
    percent / 100,
  );
}

const volumeName = /^[a-zA-Z0-9][a-zA-Z0-9_.-]+$/;

/** Whether a name is one Docker gives a volume. */
export function validVolume(name: string): boolean {
  return volumeName.test(name.trim());
}

/** Whether a directory is one the panel backs up: absolute and clean, not /,
 * and not in /proc, /sys or /dev. */
export function validPath(path: string): boolean {
  const value = path.trim();
  if (!value.startsWith("/") || value === "/" || value.endsWith("/")) {
    return false;
  }
  const parts = value.slice(1).split("/");
  if (parts.some((part) => part === "" || part === "." || part === "..")) {
    return false;
  }
  return !["proc", "sys", "dev"].includes(parts[0]);
}

/** The engine a service's image most likely runs, to choose first. */
export function guessEngine(image: string): DumpEngine | "" {
  const repository = image
    .split("@")[0]
    .replace(/:[^/]*$/, "")
    .split("/")
    .pop()!;
  if (/postgres|postgis|timescale/.test(repository)) {
    return "postgres";
  }
  if (/mysql|mariadb|percona/.test(repository)) {
    return "mysql";
  }
  return "";
}
