import type { RecordModel } from "pocketbase";

import i18n from "../i18n";
import type { BackupLock } from "./backupJobs";
import type { BackupTarget } from "./backups";
import { parseDate } from "./format";
import { pb } from "./pb";
import type { Alert } from "./types";

/** The states of a check (#31): queued, then running, then how it ended: ok
 * (nothing found, or what the lock-free pass saw was not confirmed), failed
 * (errors confirmed with the repository locked), suspect (errors the second
 * pass could not confirm), error (it did not complete), skipped (it never
 * started) or cancelled (stopped after it started). */
export type CheckStatus =
  "queued" | "running" | "ok" | "failed" | "suspect" | "error" | "skipped" | "cancelled";

/** An error restic printed, redacted: a pack file by its full id, a tree by
 * its short one, or any other line. */
export type CheckError = { kind: "pack" | "tree" | "other"; id?: string; text: string };

/** The repair restic advised. */
export type CheckRepair = {
  index: boolean;
  /** the ids of `restic repair packs`, which goes with `restic repair snapshots --forget` */
  packs: string[] | null;
  snapshots: boolean;
  /** "The repository is damaged and must be repaired", with no command */
  other: boolean;
};

/** One check of a target's repository. #53's restore tests will share the
 * collection, as kind restore_test. */
export type BackupCheck = RecordModel & {
  created: string;
  updated: string;
  target: string;
  /** the target's server, whose agent ran it */
  host: string;
  kind: "integrity" | "restore_test";
  trigger: "schedule" | "manual" | "retry";
  status: CheckStatus;
  /** the slot of a scheduled check's first attempt */
  scheduled_at: string;
  started_at: string;
  finished_at: string;
  duration_ms: number;
  /** "3/20": the group of data packs it read; "" the structure only */
  subset: string;
  packs_read: number;
  packs_total: number;
  /** what restic found was confirmed by the pass with the repository locked */
  confirmed: boolean;
  /** what the lock-free pass found, the locked one did not */
  unconfirmed: boolean;
  /** only when the record is read whole, not in lists */
  errors?: CheckError[] | null;
  errors_more: number;
  repair: CheckRepair | null;
  notes: string[] | null;
  lock: BackupLock | null;
  error: string;
  /** only when the record is read whole */
  log?: string;
  command: string;
  attempt: number;
  retry_of: string;
  retry_at: string;
};

/** What lists read of a check: never its errors or its log, which can be long. */
export const checkFields =
  "id,collectionId,collectionName,created,updated,target,host,kind,trigger,status,scheduled_at,started_at,finished_at,duration_ms,subset,packs_read,packs_total,confirmed,unconfirmed,errors_more,repair,notes,lock,error,command,attempt,retry_of,retry_at";

/** The integrity checks that are queued or running. */
export const openChecksFilter = "kind = 'integrity' && (status = 'queued' || status = 'running')";

/** The open alert that a target's repository is not verified. */
export const unverifiedFilter = "status != 'resolved' && rule = 'backup_check_unverified'";

/** How the integrity checks of all targets stand, as the summary's tile says
 * it: errors found, targets not verified, checks turned off, none run yet, or
 * fine. */
export type IntegrityState = "off" | "never" | "failed" | "unverified" | "ok";

export type IntegritySummary = {
  state: IntegrityState;
  /** the targets with errors, or not verified */
  count: number;
  /** the next check of any target; "" when none is planned */
  next: string;
  /** the target a check is running for */
  running: BackupTarget | null;
};

/** The tile's summary. Pending targets have no repository to check, and are
 * left out. Errors come first, then the targets an open alert says are not
 * verified; the checks are off when no target has a next check. */
export function integritySummary(
  targets: BackupTarget[],
  unverifiedAlerts: Pick<Alert, "rule" | "target" | "status">[],
  openChecks: Pick<BackupCheck, "target" | "status">[],
): IntegritySummary {
  const checked = targets.filter((target) => target.status !== "pending");
  const ids = new Set(checked.map((target) => target.id));
  const failed = checked.filter((target) => target.integrity_status === "failed");
  const unverified = new Set(
    unverifiedAlerts
      .filter(
        (alert) =>
          alert.rule === "backup_check_unverified" &&
          alert.status !== "resolved" &&
          ids.has(alert.target),
      )
      .map((alert) => alert.target),
  );
  const next = checked
    .map((target) => target.next_check_at)
    .filter((value) => parseDate(value) !== null)
    .sort()[0];
  const runningCheck = openChecks.find(
    (check) => check.status === "running" && ids.has(check.target),
  );
  const running = runningCheck
    ? (checked.find((target) => target.id === runningCheck.target) ?? null)
    : null;

  let state: IntegrityState;
  let count = 0;
  if (failed.length > 0) {
    state = "failed";
    count = failed.length;
  } else if (unverified.size > 0) {
    state = "unverified";
    count = unverified.size;
  } else if (checked.length > 0 && next === undefined) {
    state = "off";
  } else if (!checked.some((target) => parseDate(target.integrity_at) !== null)) {
    state = "never";
  } else {
    state = "ok";
  }
  return { state, count, next: next ?? "", running };
}

function locale() {
  return i18n.language || "en";
}

const week = 7 * 24 * 3600 * 1000;

/** A check's moment, as the design writes it: the long weekday and the time
 * within a week of now ("neděle 06:00", "Sunday 06:00"), else the day and
 * month with the time. In the browser's time zone unless one is given. */
export function formatCheckMoment(value: string, now: number, timeZone?: string): string {
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
  if (Math.abs(date.getTime() - now) < week) {
    return new Intl.DateTimeFormat(locale(), { ...time, weekday: "long" }).format(date);
  }
  return new Intl.DateTimeFormat(locale(), { ...time, day: "numeric", month: "numeric" }).format(
    date,
  );
}

/** Which title a time takes in Czech: "ve" before the hours said with two
 * consonants first (dvě, tři, čtyři, dvanáct, třináct, čtrnáct, dvacet…),
 * "v" before the others. English reads both titles the same. */
export function czechAt(time: Date, timeZone?: string): "title" | "titleVe" {
  const hour = Number(
    new Intl.DateTimeFormat("en-US", { hour: "numeric", hourCycle: "h23", timeZone }).format(time),
  );
  return [2, 3, 4, 12, 13, 14, 20, 21, 22, 23].includes(hour) ? "titleVe" : "title";
}

/** "3/20" as group 3 of 20; null for the structure only, or anything else. */
export function parseSubset(subset: string): { n: number; t: number } | null {
  const match = /^([1-9]\d{0,2})\/([1-9]\d{0,2})$/.exec(subset);
  if (!match) {
    return null;
  }
  const n = Number(match[1]);
  const t = Number(match[2]);
  return n <= t ? { n, t } : null;
}

/** The group the next check of a target reads, of `groups`: its cursor, 0 or
 * out of range reading as 1, as the panel takes it. */
export function nextGroup(target: Pick<BackupTarget, "integrity_group">, groups: number): number {
  const group = target.integrity_group || 1;
  return group >= 1 && group <= groups ? group : 1;
}

/** The checks' settings: the schedule, cron in UTC ("" turns them off), and
 * how many groups the data packs are read in (0 the structure only). */
export type CheckSettings = { schedule: string; groups: number };

/** The most groups restic 0.17 reads the packs in. */
export const maxGroups = 256;

/** Groups as typed: a whole number from 0 to 256, else null. */
export function parseGroups(text: string): number | null {
  const value = text.trim();
  return /^\d{1,3}$/.test(value) && Number(value) <= maxGroups ? Number(value) : null;
}

/** Saves the checks' settings; the panel answers with them and the next check. */
export function saveCheckSettings(body: Partial<CheckSettings>) {
  return pb.send<CheckSettings & { next_check_at: string }>("/api/pc/settings/backup-checks", {
    method: "PUT",
    body,
  });
}
