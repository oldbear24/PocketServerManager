import { afterEach, describe, expect, it, vi } from "vitest";

import i18n from "../i18n";
import {
  cronPreset,
  dayKey,
  describeCron,
  describeRetention,
  failedRuns,
  formatMoment,
  guessEngine,
  holderState,
  jobsApi,
  lastDays,
  lastRun,
  lockStaleAt,
  nextJob,
  parseKeep,
  parseTime,
  presetCron,
  resticLine,
  runFields,
  runIssue,
  runStrip,
  runsSince,
  shortError,
  successPercent,
  successRate,
  supersededRuns,
  validPath,
  validVolume,
  type BackupRun,
  type RunStatus,
  type SchedulePreset,
} from "./backupJobs";
import { pb } from "./pb";
import type { Command, Host } from "./types";

afterEach(() => {
  vi.restoreAllMocks();
});

const en = i18n.getFixedT("en");
const cs = i18n.getFixedT("cs");

// minutes ahead of UTC
const prague = 60;
const pragueSummer = 120;
const newYork = -300;
const india = 330;

describe("a schedule in local time, kept in UTC", () => {
  it("turns a daily time into the UTC hour, across midnight too", () => {
    expect(presetCron({ kind: "daily", hour: 3, minute: 0 }, prague)).toBe("0 2 * * *");
    expect(presetCron({ kind: "daily", hour: 3, minute: 0 }, pragueSummer)).toBe("0 1 * * *");
    expect(presetCron({ kind: "daily", hour: 0, minute: 30 }, prague)).toBe("30 23 * * *");
    expect(presetCron({ kind: "daily", hour: 22, minute: 0 }, newYork)).toBe("0 3 * * *");
    expect(presetCron({ kind: "daily", hour: 3, minute: 0 }, 0)).toBe("0 3 * * *");
  });

  it("moves a weekly run to the UTC weekday when midnight is between", () => {
    // Monday 00:30 in Prague's summer is Sunday 22:30 UTC
    expect(presetCron({ kind: "weekly", weekday: 1, hour: 0, minute: 30 }, pragueSummer)).toBe(
      "30 22 * * 0",
    );
    // Saturday 22:00 in New York is Sunday 03:00 UTC
    expect(presetCron({ kind: "weekly", weekday: 6, hour: 22, minute: 0 }, newYork)).toBe(
      "0 3 * * 0",
    );
    // Sunday 00:15 in Prague is Saturday 23:15 UTC: the week wraps
    expect(presetCron({ kind: "weekly", weekday: 0, hour: 0, minute: 15 }, prague)).toBe(
      "15 23 * * 6",
    );
  });

  it("moves an hourly minute where the zone is off by half an hour", () => {
    expect(presetCron({ kind: "hourly", minute: 0 }, india)).toBe("30 * * * *");
    expect(presetCron({ kind: "hourly", minute: 15 }, prague)).toBe("15 * * * *");
  });

  it("reads back each preset it writes, in any zone", () => {
    const presets: SchedulePreset[] = [
      { kind: "hourly", minute: 0 },
      { kind: "hourly", minute: 45 },
      { kind: "daily", hour: 0, minute: 0 },
      { kind: "daily", hour: 23, minute: 59 },
      { kind: "weekly", weekday: 0, hour: 0, minute: 0 },
      { kind: "weekly", weekday: 6, hour: 23, minute: 30 },
      { kind: "weekly", weekday: 3, hour: 3, minute: 0 },
    ];
    for (const offset of [0, prague, pragueSummer, newYork, india, -570, 840]) {
      for (const preset of presets) {
        expect(cronPreset(presetCron(preset, offset), offset)).toEqual(preset);
      }
    }
  });

  it("reads a UTC expression as a preset in local time, or as none", () => {
    expect(cronPreset("0 2 * * *", prague)).toEqual({ kind: "daily", hour: 3, minute: 0 });
    expect(cronPreset("@daily", prague)).toEqual({ kind: "daily", hour: 1, minute: 0 });
    expect(cronPreset("@hourly", india)).toEqual({ kind: "hourly", minute: 30 });
    // 7 is Sunday as well
    expect(cronPreset("0 4 * * 7", prague)).toEqual({
      kind: "weekly",
      weekday: 0,
      hour: 5,
      minute: 0,
    });
    for (const cron of ["0 3 * * 1-5", "0 3 1 * *", "*/15 * * * *", "0 */6 * * *", "0 3", ""]) {
      expect(cronPreset(cron, prague)).toBeNull();
    }
  });
});

describe("a schedule in words", () => {
  it("says the common ones in local time", () => {
    expect(describeCron("0 2 * * *", en, prague)).toBe("daily 03:00");
    expect(describeCron("0 2 * * *", en, newYork)).toBe("daily 21:00");
    expect(describeCron("0 * * * *", en, prague)).toBe("hourly");
    expect(describeCron("0 * * * *", en, india)).toBe("hourly at :30");
    expect(describeCron("30 22 * * 0", en, pragueSummer)).toBe("Mondays 00:30");
    expect(describeCron("@weekly", en, 0)).toBe("Sundays 00:00");
    expect(describeCron("*/15 * * * *", en, prague)).toBe("every 15 min");
    expect(describeCron("* * * * *", en, prague)).toBe("every minute");
    expect(describeCron("0 */6 * * *", en, prague)).toBe("every 6 h");
    expect(describeCron("0 2 1 * *", en, prague)).toBe("monthly, day 1, 03:00");
  });

  it("falls back to the expression, in UTC, for the rest", () => {
    // */7 runs at :56 and again at :00
    expect(describeCron("*/7 * * * *", en, prague)).toBe("*/7 * * * * UTC");
    expect(describeCron("0 3 * * 1-5", en, prague)).toBe("0 3 * * 1-5 UTC");
    // the 1st at 23:00 UTC is the 2nd in Prague, which a day number cannot say
    expect(describeCron(" 0 23 1 * * ", en, prague)).toBe("0 23 1 * * UTC");
  });

  it("speaks Czech", () => {
    expect(describeCron("0 2 * * *", cs, prague)).toBe("denně 03:00");
    expect(describeCron("0 2 * * 3", cs, prague)).toBe("ve středu 03:00");
    expect(describeCron("0 * * * *", cs, prague)).toBe("každou hodinu");
    expect(describeCron("*/5 * * * *", cs, prague)).toBe("každých 5 min");
  });

  it("reads a time input's value", () => {
    expect(parseTime("03:00")).toEqual({ hour: 3, minute: 0 });
    expect(parseTime("23:59:00")).toEqual({ hour: 23, minute: 59 });
    expect(parseTime("24:00")).toBeNull();
    expect(parseTime("")).toBeNull();
  });
});

describe("a retention in short", () => {
  it("writes it as its label does", () => {
    expect(describeRetention({ hourly: 24, daily: 7, weekly: 4, monthly: 6 }, en)).toBe(
      "24h 7d 4w 6m",
    );
    expect(describeRetention({ last: 20 }, en)).toBe("last 20");
    expect(describeRetention({ last: 5, daily: 7, yearly: 2 }, en)).toBe("last 5 · 7d 2y");
    expect(describeRetention({ last: 0, hourly: 0 }, en)).toBe("keeps every snapshot");
    expect(describeRetention(null, en)).toBe("keeps every snapshot");
  });

  it("counts in Czech", () => {
    expect(describeRetention({ last: 20 }, cs)).toBe("posledních 20");
    expect(describeRetention({ last: 3 }, cs)).toBe("poslední 3");
    expect(describeRetention({}, cs)).toBe("ponechává všechny snapshoty");
  });

  it("takes whole numbers from 0 to 10000, and empty as 0", () => {
    expect(parseKeep("")).toBe(0);
    expect(parseKeep(" 7 ")).toBe(7);
    expect(parseKeep("10000")).toBe(10000);
    for (const text of ["10001", "-1", "1.5", "seven"]) {
      expect(parseKeep(text)).toBeNull();
    }
  });
});

function run(created: string, status: RunStatus, fields: Partial<BackupRun> = {}): BackupRun {
  return {
    id: `${created}-${status}`,
    collectionId: "backup_runs",
    collectionName: "backup_runs",
    created,
    updated: created,
    job: "j1",
    target: "t1",
    host: "h1",
    trigger: "schedule",
    status,
    scheduled_at: "",
    started_at: "",
    finished_at: "",
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

// Monday noon UTC; Prague is on winter time, an hour ahead
const now = Date.parse("2026-11-09T12:00:00Z");

describe("the 14-day strip", () => {
  it("has the last 14 local days, oldest first", () => {
    const days = lastDays(now, 14, "Europe/Prague");
    expect(days).toHaveLength(14);
    expect(days[0]).toBe("2026-10-27");
    expect(days[13]).toBe("2026-11-09");
    // late on Sunday in New York, it is Monday in UTC
    expect(lastDays(Date.parse("2026-11-09T03:00:00Z"), 2, "America/New_York")).toEqual([
      "2026-11-07",
      "2026-11-08",
    ]);
    expect(dayKey(new Date("2026-11-08T23:30:00Z"), "Europe/Prague")).toBe("2026-11-09");
    expect(dayKey(new Date("2026-11-08T23:30:00Z"), "UTC")).toBe("2026-11-08");
  });

  it("counts a run on the local day it was made", () => {
    // 00:30 on Monday in Prague, 23:30 on Sunday in UTC
    const runs = [run("2026-11-08 23:30:00.000Z", "succeeded")];
    expect(runStrip(runs, now, "Europe/Prague")[13].succeeded).toBe(1);
    expect(runStrip(runs, now, "UTC")[12].succeeded).toBe(1);
    expect(runStrip(runs, now, "UTC")[13].succeeded).toBe(0);
    // 22:00 on Sunday in New York
    const evening = [run("2026-11-09 03:00:00.000Z", "failed")];
    expect(runStrip(evening, now, "America/New_York")[12].state).toBe("err");
  });

  it("leaves out runs before its first local day", () => {
    const runs = [
      // 00:30 on 27 October in Prague: the first day
      run("2026-10-26 23:30:00.000Z", "succeeded"),
      // 23:30 on 26 October in Prague: a day too early
      run("2026-10-26 22:30:00.000Z", "failed"),
    ];
    const strip = runStrip(runs, now, "Europe/Prague");
    expect(strip[0]).toEqual({
      day: "2026-10-27",
      succeeded: 1,
      failed: 0,
      skipped: 0,
      retried: 0,
      state: "ok",
    });
    expect(strip.every((day) => day.failed === 0)).toBe(true);
  });

  it("colours a day by its worst run: failed, then skipped, then succeeded", () => {
    const runs = [
      run("2026-11-09 01:00:00.000Z", "succeeded"),
      run("2026-11-09 02:00:00.000Z", "failed"),
      run("2026-11-09 03:00:00.000Z", "skipped"),
      run("2026-11-08 01:00:00.000Z", "succeeded"),
      run("2026-11-08 02:00:00.000Z", "skipped"),
      run("2026-11-07 01:00:00.000Z", "succeeded"),
      run("2026-11-06 01:00:00.000Z", "running"),
      run("2026-11-05 01:00:00.000Z", "cancelled"),
      run("2026-11-04 01:00:00.000Z", "queued"),
    ];
    const strip = runStrip(runs, now, "UTC");
    expect(strip.slice(-6).map((day) => day.state)).toEqual([
      "off",
      "off",
      "off",
      "ok",
      "warn",
      "err",
    ]);
    expect(strip[13]).toMatchObject({ succeeded: 1, failed: 1, skipped: 1 });
    expect(strip[0].state).toBe("off");
  });
});

describe("the success over 14 days", () => {
  it("counts the runs that succeeded of those that ended or were skipped", () => {
    const runs = [
      run("2026-11-09 01:00:00.000Z", "succeeded"),
      run("2026-11-08 01:00:00.000Z", "succeeded"),
      run("2026-11-07 01:00:00.000Z", "failed"),
      run("2026-11-06 01:00:00.000Z", "skipped"),
      // neither: not ended, or stopped by hand
      run("2026-11-09 11:00:00.000Z", "running"),
      run("2026-11-09 10:00:00.000Z", "queued"),
      run("2026-11-05 01:00:00.000Z", "cancelled"),
      // before the 14 days
      run("2026-10-20 01:00:00.000Z", "failed"),
    ];
    expect(successRate(runs, now, "UTC")).toEqual({ succeeded: 2, total: 4 });
  });

  it("never says 100 % while a run did not succeed", () => {
    expect(successPercent(131, 136)).toBe(96);
    expect(successPercent(999, 1000)).toBe(99);
    expect(successPercent(5, 5)).toBe(100);
    expect(successPercent(0, 3)).toBe(0);
    expect(successPercent(0, 0)).toBeNull();
  });
});

describe("a run tried again", () => {
  // 03:00 locked, retried at 03:35 and at 04:05, which succeeded
  const first = run("2026-11-09 03:00:00.000Z", "failed", { id: "r1", failure: "locked" });
  const second = run("2026-11-09 03:35:00.000Z", "failed", {
    id: "r2",
    trigger: "retry",
    attempt: 2,
    retry_of: "r1",
    failure: "restic",
  });
  const third = run("2026-11-09 04:05:00.000Z", "succeeded", {
    id: "r3",
    trigger: "retry",
    attempt: 3,
    retry_of: "r2",
  });

  it("counts only the last attempt of its chain in the strip and the success", () => {
    expect([...supersededRuns([first, second, third])]).toEqual(["r1", "r2"]);
    const day = runStrip([first, second, third], now, "UTC")[13];
    expect(day).toEqual({
      day: "2026-11-09",
      succeeded: 1,
      failed: 0,
      skipped: 0,
      retried: 1,
      state: "ok",
    });
    expect(successRate([first, second, third], now, "UTC")).toEqual({ succeeded: 1, total: 1 });
  });

  it("leaves a failure out while its retry runs, and counts it once that is gone", () => {
    const going = { ...third, status: "running" as const };
    expect(runStrip([first, second, going], now, "UTC")[13].state).toBe("off");
    expect(successRate([first, second, going], now, "UTC")).toEqual({ succeeded: 0, total: 0 });
    // the retry's record is older than the runs loaded: the failure stands
    expect(runStrip([second], now, "UTC")[13]).toMatchObject({ failed: 1, state: "err" });
    // a retry that failed in the end is the day's failure
    const last = { ...third, status: "failed" as const };
    expect(runStrip([first, second, last], now, "UTC")[13]).toMatchObject({
      failed: 1,
      retried: 0,
      state: "err",
    });
  });

  it("counts a failure whose retry never ran: skipped, or cancelled before it started", () => {
    // the retry waited its whole time in the queue, as its server stayed away
    const skipped = run("2026-11-09 03:35:00.000Z", "skipped", {
      id: "r2",
      trigger: "retry",
      attempt: 2,
      retry_of: "r1",
    });
    expect([...supersededRuns([first, skipped])]).toEqual([]);
    expect(runStrip([first, skipped], now, "UTC")[13]).toMatchObject({
      failed: 1,
      skipped: 1,
      state: "err",
    });
    expect(successRate([first, skipped], now, "UTC")).toEqual({ succeeded: 0, total: 2 });
    // its agent stopped while it waited for a worker
    const cancelled = { ...skipped, status: "cancelled" as const };
    expect(runStrip([first, cancelled], now, "UTC")[13]).toMatchObject({
      failed: 1,
      skipped: 0,
      state: "err",
    });
    expect(successRate([first, cancelled], now, "UTC")).toEqual({ succeeded: 0, total: 1 });
    // a retry that started, then was stopped, stands for it and counts for neither
    const stopped = { ...cancelled, started_at: "2026-11-09 03:35:01.000Z" };
    expect(runStrip([first, stopped], now, "UTC")[13].state).toBe("off");
    expect(successRate([first, stopped], now, "UTC")).toEqual({ succeeded: 0, total: 0 });
  });

  it("is read with its retry fields, still without its log", () => {
    const fields = runFields.split(",");
    for (const field of [
      "host",
      "command",
      "attempt",
      "retry_of",
      "retry_at",
      "retry_note",
      "failure",
      "lock",
    ]) {
      expect(fields).toContain(field);
    }
    expect(fields).not.toContain("log");
  });
});

describe("the runs a banner is about", () => {
  const jobs = [
    { id: "j1", targets: ["t1", "t2"], label_removed: false },
    { id: "j2", targets: ["t1"], label_removed: false },
    { id: "j3", targets: ["t1"], label_removed: true },
  ];

  it("are the newest of each job and target that failed, or a retry under way, newest first", () => {
    const runs = [
      // j1 → t1: failed, then succeeded: nothing to say
      run("2026-11-09 01:00:00.000Z", "failed", { id: "a1" }),
      run("2026-11-09 02:00:00.000Z", "succeeded", { id: "a2" }),
      // j1 → t2: succeeded, then failed
      run("2026-11-09 01:00:00.000Z", "succeeded", { id: "b1", target: "t2" }),
      run("2026-11-09 03:00:00.000Z", "failed", { id: "b2", target: "t2" }),
      // j2 → t1: failed, and its retry is queued
      run("2026-11-09 04:00:00.000Z", "failed", { id: "c1", job: "j2" }),
      run("2026-11-09 04:35:00.000Z", "queued", {
        id: "c2",
        job: "j2",
        trigger: "retry",
        retry_of: "c1",
      }),
    ];
    expect(failedRuns(jobs, runs).map((item) => item.id)).toEqual(["c2", "b2"]);
  });

  it("leave out runs a retry could not start: a label gone, a target taken out, a job gone", () => {
    const runs = [
      run("2026-11-09 01:00:00.000Z", "failed", { id: "x1", job: "j3" }),
      run("2026-11-09 01:00:00.000Z", "failed", { id: "x2", job: "j2", target: "t2" }),
      run("2026-11-09 01:00:00.000Z", "failed", { id: "x3", job: "gone" }),
    ];
    expect(failedRuns(jobs, runs)).toEqual([]);
  });

  it("are not a run queued by hand, which takes the failure's place", () => {
    const runs = [
      run("2026-11-09 01:00:00.000Z", "failed", { id: "y1" }),
      run("2026-11-09 02:00:00.000Z", "queued", { id: "y2", trigger: "manual" }),
    ];
    expect(failedRuns(jobs, runs)).toEqual([]);
  });

  it("keep a failure that a run which never ran came after: skipped, or cancelled before it started", () => {
    const runs = [
      // j1 → t1: the hourly 01:00 run waited on a lock past 02:00, whose
      // run skipped, then failed at 02:00:30
      run("2026-11-09 01:00:00.000Z", "failed", {
        id: "z1",
        started_at: "2026-11-09 01:00:01.000Z",
        finished_at: "2026-11-09 02:00:30.000Z",
        failure: "locked",
      }),
      run("2026-11-09 02:00:00.000Z", "skipped", { id: "z2" }),
      // j1 → t2: its retry was cancelled while it waited for a worker
      run("2026-11-09 01:00:00.000Z", "failed", { id: "w1", target: "t2" }),
      run("2026-11-09 01:05:00.000Z", "cancelled", {
        id: "w2",
        target: "t2",
        trigger: "retry",
        retry_of: "w1",
      }),
      // j2 → t1: its retry started, and was stopped: it took the place
      run("2026-11-09 01:00:00.000Z", "failed", { id: "v1", job: "j2" }),
      run("2026-11-09 01:05:00.000Z", "cancelled", {
        id: "v2",
        job: "j2",
        trigger: "retry",
        retry_of: "v1",
        started_at: "2026-11-09 01:05:01.000Z",
      }),
    ];
    expect(failedRuns(jobs, runs).map((item) => item.id)).toEqual(["z1", "w1"]);
    // a run that skipped or never started is no banner of its own
    expect(failedRuns(jobs, [runs[1], runs[3]])).toEqual([]);
  });
});

describe("the holder of a lock", () => {
  const hosts = [
    { id: "h1", stream_open: true },
    { id: "h2", stream_open: false },
  ] as Host[];
  const command = (status: Command["status"], host = "h1") => ({ status, host });
  const lock = { command: "k3j9x2m4q8w1z7v" };

  it("is alive while its command is open, or timed out with its server not connected", () => {
    for (const status of ["queued", "dispatched", "running"] as const) {
      expect(holderState(lock, command(status), hosts)).toBe("alive");
    }
    expect(holderState(lock, command("timed_out", "h2"), hosts)).toBe("alive");
    expect(holderState(lock, command("timed_out", "gone"), hosts)).toBe("alive");
  });

  it("is gone once its command ended, or is no more", () => {
    for (const status of ["succeeded", "failed", "cancelled"] as const) {
      expect(holderState(lock, command(status), hosts)).toBe("gone");
    }
    expect(holderState(lock, command("timed_out", "h1"), hosts)).toBe("gone");
    expect(holderState(lock, undefined, hosts)).toBe("gone");
  });

  it("is unknown when PocketCloud does not run it", () => {
    expect(holderState({ command: "" }, undefined, hosts)).toBe("unknown");
    expect(holderState(null, undefined, hosts)).toBe("unknown");
  });

  it("leaves a lock that restic counts as stale 32 minutes after its last refresh", () => {
    expect(lockStaleAt({ created: "2026-11-09T02:58:00Z" })).toBe(
      Date.parse("2026-11-09T03:30:00Z"),
    );
    // an older agent's lock line has no time
    expect(lockStaleAt({})).toBeNull();
    expect(lockStaleAt(null)).toBeNull();
  });
});

describe("what a failed run's banner says", () => {
  const job = { enabled: true, label_removed: false, next_run_at: "2026-11-10 02:00:00.000Z" };
  const planned = "2026-11-09 03:35:00.000Z";

  it("waits for a holder that still runs, with the button held", () => {
    expect(
      runIssue({ status: "failed", failure: "locked", retry_at: planned }, "alive", job),
    ).toEqual({ tone: "warn", state: "waitsHolder", action: "unlock", held: true });
  });

  it("offers unlock and retry once the holder is gone or unknown", () => {
    for (const holder of ["gone", "unknown"] as const) {
      expect(
        runIssue({ status: "failed", failure: "locked", retry_at: planned }, holder, job),
      ).toEqual({ tone: "warn", state: "retryAt", action: "unlock", held: false });
      expect(
        runIssue({ status: "failed", failure: "locked", retry_at: "", attempt: 4 }, holder, job),
      ).toEqual({ tone: "err", state: "noMore", action: "unlock", held: false });
    }
  });

  it("says when a retryable failure is tried again", () => {
    for (const failure of ["restic", "interrupted"] as const) {
      expect(runIssue({ status: "failed", failure, retry_at: planned }, "unknown", job)).toEqual({
        tone: "warn",
        state: "retryAt",
        action: "retry",
        held: false,
      });
    }
  });

  it("says the next scheduled run takes a dropped retry's place, else no more retries", () => {
    const dropped = { status: "failed" as const, failure: "restic" as const, retry_at: "" };
    expect(runIssue(dropped, "unknown", job)).toEqual({
      tone: "warn",
      state: "nextRun",
      action: "retry",
      held: false,
    });
    expect(runIssue({ ...dropped, attempt: 4 }, "unknown", job).state).toBe("noMore");
    expect(runIssue(dropped, "unknown", { ...job, enabled: false }).state).toBe("noMore");
    expect(runIssue(dropped, "unknown", { ...job, next_run_at: "" }).state).toBe("noMore");
  });

  it("says a failure that does not pass by itself is not retried", () => {
    for (const failure of ["repository", "password", "partial", "source", "timeout", ""] as const) {
      expect(runIssue({ status: "failed", failure }, "unknown", job)).toEqual({
        tone: "err",
        state: "notRetried",
        action: "retry",
        held: false,
      });
    }
  });

  it("offers nothing for an unreadable lock, nor while a retry runs", () => {
    expect(runIssue({ status: "failed", failure: "lock_invalid" }, "unknown", job)).toEqual({
      tone: "err",
      state: "invalidLock",
      action: null,
      held: false,
    });
    for (const status of ["queued", "running"] as const) {
      expect(runIssue({ status }, "unknown", job)).toEqual({
        tone: "info",
        state: "retrying",
        action: null,
        held: false,
      });
    }
  });

  it("shows restic's own line without what goes before it", () => {
    expect(
      resticLine(
        "unable to create lock in backend: repository is already locked by PID 1 on nas by root (UID 0, GID 0)\nmore",
      ),
    ).toBe("repository is already locked by PID 1 on nas by root (UID 0, GID 0)");
    expect(
      resticLine("Fatal: Unable to create lock in backend: repository is already locked"),
    ).toBe("repository is already locked");
    expect(resticLine("Fatal: wrong password or no key found")).toBe(
      "wrong password or no key found",
    );
  });
});

describe("a job's last run", () => {
  const job = { id: "j1", targets: ["t1", "t2"] };

  it("shows the worst of each target's newest run", () => {
    const runs = [
      run("2026-11-09 03:00:00.000Z", "succeeded", { target: "t1" }),
      run("2026-11-08 03:00:00.000Z", "failed", { target: "t1" }),
      run("2026-11-09 03:00:01.000Z", "failed", { target: "t2", error: "Fatal: locked" }),
      // another job's, and a target the job no longer has
      run("2026-11-09 04:00:00.000Z", "skipped", { job: "j2", target: "t1" }),
      run("2026-11-09 05:00:00.000Z", "skipped", { target: "t3" }),
    ];
    expect(lastRun(job, runs)).toMatchObject({ target: "t2", status: "failed" });
    // the failure of the day before is over
    expect(lastRun({ id: "j1", targets: ["t1"] }, runs)).toMatchObject({ status: "succeeded" });
  });

  it("shows the newest when all went the same way", () => {
    const runs = [
      run("2026-11-09 03:00:00.000Z", "succeeded", { target: "t1" }),
      run("2026-11-09 03:00:05.000Z", "succeeded", { target: "t2" }),
    ];
    expect(lastRun(job, runs)?.target).toBe("t2");
    expect(lastRun(job, [])).toBeNull();
  });

  it("shows a retention that failed or was put off, before one that was applied", () => {
    const deferred = run("2026-11-09 03:00:00.000Z", "succeeded", {
      target: "t1",
      retention: "deferred",
    });
    const applied = run("2026-11-09 03:00:05.000Z", "succeeded", {
      target: "t2",
      retention: "applied",
    });
    expect(lastRun(job, [deferred, applied])).toBe(deferred);
    const failed = { ...applied, retention: "failed" as const };
    expect(lastRun(job, [deferred, failed])).toBe(failed);
    // a run that failed is worse still
    const broken = { ...applied, status: "failed" as const, retention: "" as const };
    expect(lastRun(job, [deferred, broken])).toBe(broken);
  });

  it("reads each run's retention for the Jobs card", () => {
    expect(runFields.split(",")).toContain("retention");
  });

  it("keeps the first line of an error for its cell", () => {
    expect(shortError("\nFatal: repository is already locked\nby PID 4121\n")).toBe(
      "Fatal: repository is already locked",
    );
    expect(shortError("")).toBe("");
  });
});

describe("the next run", () => {
  const job = (name: string, next_run_at: string, fields = {}) => ({
    name,
    next_run_at,
    enabled: true,
    label_removed: false,
    ...fields,
  });

  it("is the earliest of the jobs that run", () => {
    const jobs = [
      job("photos", "2026-11-10 02:00:00.000Z"),
      job("paused", "2026-11-09 12:30:00.000Z", { enabled: false }),
      job("gone", "2026-11-09 12:15:00.000Z", { label_removed: true }),
      job("no target", ""),
      job("vaultwarden", "2026-11-09 13:00:00.000Z"),
    ];
    expect(nextJob(jobs)?.name).toBe("vaultwarden");
    expect(nextJob([job("no target", "")])).toBeNull();
  });

  it("says it as short as it can", () => {
    expect(formatMoment("2026-11-09 12:00:00.000Z", now, "Europe/Prague")).toBe("13:00");
    expect(formatMoment("2026-11-10 02:00:00.000Z", now, "Europe/Prague")).toBe("Tue 03:00");
    expect(formatMoment("2026-11-20 02:00:00.000Z", now, "Europe/Prague")).toMatch(
      /^11\/20,? 03:00$/,
    );
    expect(formatMoment("", now)).toBe("–");
  });

  it("loads the runs of a day more than 14, from midnight UTC", () => {
    expect(runsSince(now)).toBe("2026-10-25 00:00:00.000Z");
    expect(runsSince(Date.parse("2026-11-09T23:59:00Z"))).toBe("2026-10-25 00:00:00.000Z");
  });
});

describe("a job's source", () => {
  it("takes Docker's volume names", () => {
    expect(validVolume("vw-data")).toBe(true);
    expect(validVolume("immich_upload.1")).toBe(true);
    for (const name of ["", "a", "-data", "data/x", "my data"]) {
      expect(validVolume(name)).toBe(false);
    }
  });

  it("takes absolute, clean directories outside the kernel's", () => {
    expect(validPath("/srv/sites")).toBe(true);
    expect(validPath("/mnt/tank/photos")).toBe(true);
    expect(validPath("/proc-data")).toBe(true);
    for (const path of [
      "",
      "/",
      "srv",
      "/srv/",
      "/srv//x",
      "/srv/../etc",
      "/proc/1",
      "/sys",
      "/dev/sda",
    ]) {
      expect(validPath(path)).toBe(false);
    }
  });

  it("guesses a service's database from its image", () => {
    expect(guessEngine("postgres:16-alpine")).toBe("postgres");
    expect(guessEngine("docker.io/library/mariadb:11")).toBe("mysql");
    expect(guessEngine("mysql@sha256:abc")).toBe("mysql");
    expect(guessEngine("ghcr.io/umami-software/umami:postgresql-latest")).toBe("");
    expect(guessEngine("")).toBe("");
  });
});

describe("the jobs API", () => {
  it("calls the panel's routes", async () => {
    const send = vi.spyOn(pb, "send").mockResolvedValue({ runs: ["r1"] });
    await jobsApi.run("j1");
    expect(send).toHaveBeenCalledWith("/api/pc/backup-jobs/j1/run", { method: "POST" });
    await jobsApi.change("j1", { enabled: false });
    expect(send).toHaveBeenCalledWith("/api/pc/backup-jobs/j1", {
      method: "PATCH",
      body: { enabled: false },
    });
    await jobsApi.remove("j1");
    expect(send).toHaveBeenCalledWith("/api/pc/backup-jobs/j1", { method: "DELETE" });
    await jobsApi.retry("r1");
    expect(send).toHaveBeenCalledWith("/api/pc/backup-runs/r1/retry", { method: "POST" });
  });
});
