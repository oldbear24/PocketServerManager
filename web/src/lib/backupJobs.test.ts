import { afterEach, describe, expect, it, vi } from "vitest";

import i18n from "../i18n";
import {
  cronPreset,
  dayKey,
  describeCron,
  describeRetention,
  formatMoment,
  guessEngine,
  jobsApi,
  lastDays,
  lastRun,
  nextJob,
  parseKeep,
  parseTime,
  presetCron,
  runFields,
  runStrip,
  runsSince,
  shortError,
  successPercent,
  successRate,
  validPath,
  validVolume,
  type BackupRun,
  type RunStatus,
  type SchedulePreset,
} from "./backupJobs";
import { pb } from "./pb";

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
  });
});
