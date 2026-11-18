import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

import i18n from "../i18n";
import type { BackupCheck } from "./backupChecks";
import type { BackupJob, BackupRun } from "./backupJobs";
import {
  autoSchedule,
  backupCell,
  backupKpi,
  cellTime,
  dailyTime,
  hostHint,
  hostSchedule,
  isTemporary,
  jobOf,
  latestTest,
  reservedName,
  stackJobOf,
  temporaryKind,
  temporaryOwner,
} from "./databaseBackups";
import type { Database, DbServer } from "./databases";

// Wednesday 18 November 2026, 13:00 in Prague, on winter time
const now = Date.parse("2026-11-18T12:00:00Z");
const winter = 60;

const zone = process.env.TZ;

beforeAll(() => {
  process.env.TZ = "Europe/Prague";
});

afterAll(() => {
  if (zone === undefined) {
    delete process.env.TZ;
  } else {
    process.env.TZ = zone;
  }
});

afterEach(async () => {
  await i18n.changeLanguage("en");
});

function job(fields: Partial<BackupJob> = {}): BackupJob {
  return {
    id: "j1",
    collectionId: "backup_jobs",
    collectionName: "backup_jobs",
    name: "db-main-shop",
    host: "h1",
    source_kind: "database",
    source: "shop",
    stack: "",
    service: "",
    engine: "postgres",
    targets: ["t1"],
    schedule: "30 0 * * *",
    retention: { daily: 7, weekly: 4, monthly: 6 },
    enabled: true,
    origin: "database",
    database: "d1",
    db_server_name: "db-main",
    own_settings: [],
    last_success_at: "",
    label_removed: false,
    problem: "",
    next_run_at: "",
    last_run_at: "",
    last_status: "",
    ...fields,
  } as BackupJob;
}

function run(id: string, fields: Partial<BackupRun> = {}): BackupRun {
  return {
    id,
    collectionId: "backup_runs",
    collectionName: "backup_runs",
    created: "2026-11-18 00:30:00.000Z",
    updated: "2026-11-18 00:31:00.000Z",
    job: "j1",
    target: "t1",
    host: "h1",
    trigger: "schedule",
    status: "succeeded",
    scheduled_at: "2026-11-18 00:30:00.000Z",
    started_at: "2026-11-18 00:30:01.000Z",
    finished_at: "2026-11-18 00:31:00.000Z",
    duration_ms: 59000,
    snapshot_id: "ab".repeat(32),
    bytes_added: 1024,
    total_bytes: 4096,
    files_new: 0,
    files_changed: 1,
    forgotten: 0,
    retention: "applied",
    error: "",
    ...fields,
  } as BackupRun;
}

function database(id: string, fields: Partial<Database> = {}): Database {
  return {
    id,
    collectionId: "databases",
    collectionName: "databases",
    server: "s1",
    name: id,
    owner_kind: "stack",
    stack: "",
    size_bytes: 0,
    connections: 0,
    temporary_until: "",
    status: "ready",
    last_error: "",
    ...fields,
  } as Database;
}

const servers = [
  { id: "s1", kind: "shared" },
  { id: "s2", kind: "dedicated" },
] as DbServer[];

describe("a database's Backup cell", () => {
  it("says how the newest run went, the worst of its targets'", () => {
    expect(backupCell(job(), [run("r1")])).toEqual({
      state: "ok",
      tone: "ok",
      at: "2026-11-18 00:31:00.000Z",
    });
    const two = job({ targets: ["t1", "t2"] });
    const failed = run("r2", { target: "t2", status: "failed", finished_at: "" });
    expect(backupCell(two, [run("r1"), failed])).toMatchObject({ state: "failed", tone: "err" });
    expect(backupCell(job(), [run("r3", { status: "skipped" })])).toMatchObject({
      state: "skipped",
      tone: "warn",
    });
  });

  it("says a run that is under way, without a time", () => {
    expect(backupCell(job(), [run("r1", { status: "running" })])).toEqual({
      state: "running",
      tone: "info",
      at: "",
    });
    expect(backupCell(job(), [run("r1", { status: "queued" })])).toMatchObject({
      state: "queued",
      tone: "info",
    });
  });

  it("is off for a job that no longer runs, and asks for a target first", () => {
    expect(backupCell(job({ enabled: false }), [run("r1")]).state).toBe("off");
    // a dropped database's job is off, whatever it ran
    expect(backupCell(job({ database: "" }), [run("r1")]).state).toBe("off");
    expect(backupCell(job({ targets: [] }), []).state).toBe("noTarget");
    expect(backupCell(job({ targets: null }), []).tone).toBe("warn");
  });

  it("falls back on the job's own last run and last success, else never", () => {
    expect(
      backupCell(job({ last_status: "failed", last_run_at: "2026-11-10 00:30:00.000Z" }), []),
    ).toEqual({ state: "failed", tone: "err", at: "2026-11-10 00:30:00.000Z" });
    expect(backupCell(job({ last_success_at: "2026-11-12 00:31:00.000Z" }), [])).toEqual({
      state: "ok",
      tone: "ok",
      at: "2026-11-12 00:31:00.000Z",
    });
    expect(backupCell(job(), [])).toEqual({ state: "never", tone: "muted", at: "" });
  });

  it("reads its time locally, with the date when older than today", () => {
    expect(cellTime("2026-11-18 00:30:00.000Z", now)).toBe("01:30");
    expect(cellTime("2026-11-17 00:30:00.000Z", now)).toBe("11/17, 01:30");
    expect(cellTime("", now)).toBe("–");
  });
});

describe("the KPI Backups in 24 h", () => {
  const shop = database("d1");
  const blog = database("d2");
  const jobs = [job(), job({ id: "j2", database: "d2" })];

  it("counts the ready databases of shared servers backed up within a day", () => {
    const recent = run("r1", { finished_at: "2026-11-18 00:31:00.000Z" });
    expect(backupKpi([shop, blog], servers, jobs, now, [recent])).toEqual({
      done: 1,
      total: 2,
      failed: 0,
      noTarget: 0,
      tone: "none",
    });
    // the job's last success counts as well, a day back at most
    const both = [job({ last_success_at: "2026-11-17 12:00:00.000Z" }), jobs[1]];
    expect(backupKpi([shop, blog], servers, both, now, [run("r2", { job: "j2" })])).toMatchObject({
      done: 2,
      total: 2,
      tone: "ok",
    });
    const old = [job({ last_success_at: "2026-11-17 11:59:00.000Z" })];
    expect(backupKpi([shop], servers, old, now).done).toBe(0);
  });

  it("leaves out temporary, dedicated and unready databases", () => {
    const counted = backupKpi(
      [
        shop,
        database("t1", { name: "pc_rt_abcdefghijklmno", temporary_until: "2026-11-18 14:00:00Z" }),
        database("t2", { name: "pc_old_abcdefghijklmno" }),
        database("u1", { server: "s2" }),
        database("c1", { status: "creating" }),
      ],
      servers,
      jobs,
      now,
      [run("r1")],
    );
    expect(counted).toMatchObject({ done: 1, total: 1, tone: "ok" });
  });

  it("warns of failures and of databases without a target, and is off without any", () => {
    const failing = backupKpi([shop], servers, jobs, now, [run("r1", { status: "failed" })]);
    expect(failing).toMatchObject({ done: 0, failed: 1, tone: "warn" });
    const bare = backupKpi([shop, blog], servers, [job({ targets: [] }), jobs[1]], now);
    expect(bare).toMatchObject({ noTarget: 1, tone: "warn" });
    expect(backupKpi([], servers, jobs, now)).toMatchObject({ total: 0, tone: "off" });
  });
});

describe("a database's jobs", () => {
  it("are its own automatic job, or for a dedicated server its stack's dump", () => {
    const own = job();
    const label = job({ id: "j3", origin: "label", source_kind: "db_dump", stack: "k1" });
    expect(jobOf({ id: "d1" }, [label, own])).toBe(own);
    expect(jobOf({ id: "d9" }, [label, own])).toBeNull();
    const web = job({
      id: "j4",
      origin: "label",
      source_kind: "db_dump",
      stack: "k1",
      service: "db",
    });
    expect(stackJobOf({ stack: "k1", service: "db" }, [label, web])).toBe(web);
    expect(stackJobOf({ stack: "k1", service: "other" }, [label, web])).toBe(label);
    expect(stackJobOf({ stack: "", service: "db" }, [label, web])).toBeNull();
  });

  it("have a newest restore test, and a newest that ended", () => {
    const check = (id: string, status: BackupCheck["status"], created: string) =>
      ({ id, kind: "restore_test", job: "j1", status, created }) as BackupCheck;
    const checks = [
      check("c1", "failed", "2026-11-11 05:20:00.000Z"),
      check("c2", "running", "2026-11-18 05:20:00.000Z"),
      { ...check("c3", "ok", "2026-11-19 05:20:00.000Z"), job: "j2" },
      { ...check("c4", "ok", "2026-11-19 05:20:00.000Z"), kind: "integrity" as const },
    ];
    expect(latestTest(checks, "j1")?.id).toBe("c2");
    expect(latestTest(checks, "j1", true)?.id).toBe("c1");
    expect(latestTest(checks, "j9")).toBeNull();
  });
});

describe("a server's defaults", () => {
  it("default to the panel's schedule, as backup.AutoSchedule makes it", () => {
    // the vectors of internal/backup's TestAutomaticBackupsSpreadOverTheNight
    expect(
      Object.fromEntries(
        [
          "host00000000007",
          "host00000000004",
          "host00000000001",
          "host00000000006",
          "host00000000003",
          "host00000000008",
          "host00000000005",
          "host00000000002",
          "vm00000000000ab",
        ].map((id) => [id, autoSchedule(id)]),
      ),
    ).toEqual({
      host00000000007: "0 1 * * *",
      host00000000004: "15 1 * * *",
      host00000000001: "30 1 * * *",
      host00000000006: "45 1 * * *",
      host00000000003: "0 2 * * *",
      host00000000008: "15 2 * * *",
      host00000000005: "30 2 * * *",
      host00000000002: "45 2 * * *",
      vm00000000000ab: "45 2 * * *",
    });
    expect(hostSchedule({ id: "host00000000007", auto_backup_schedule: "" })).toBe("0 1 * * *");
    expect(hostSchedule({ id: "host00000000007", auto_backup_schedule: " 5 4 * * * " })).toBe(
      "5 4 * * *",
    );
  });

  it("say a daily schedule's local time", () => {
    expect(dailyTime("30 0 * * *", winter)).toBe("01:30");
    expect(dailyTime("0 3 * * 1", winter)).toBe("");
  });

  it("give the server card its hint in local time", () => {
    const t = i18n.getFixedT("en");
    expect(hostHint([job(), job({ id: "j2" })], t, winter)).toEqual({
      text: "Backups daily 01:30 · each database on its own",
      warn: false,
    });
    expect(hostHint([job({ schedule: "0 3 * * 1" })], t, winter)?.text).toBe(
      "Backups Mondays 04:00 · each database on its own",
    );
    expect(hostHint([job(), job({ id: "j2", schedule: "0 2 * * *" })], t, winter)?.text).toBe(
      "Backups at several times · each database on its own",
    );
    expect(hostHint([job(), job({ id: "j2", targets: [] })], t, winter)).toEqual({
      text: "Backups have no target yet: choose one",
      warn: true,
    });
    // a job that is off, as a dropped database's, says nothing of the server
    expect(hostHint([job({ enabled: false })], t, winter)).toBeNull();
    expect(hostHint([], t, winter)).toBeNull();
    const cs = i18n.getFixedT("cs");
    expect(hostHint([job()], cs, winter)?.text).toBe("Zálohy denně 01:30 · každá databáze zvlášť");
  });
});

describe("a temporary database", () => {
  it("is a restore test's, a restore's copy, the data before a restore, or left behind", () => {
    expect(temporaryKind("pc_rt_abcdefghijklmn0")).toBe("test");
    expect(temporaryKind("pc_rs_abcdefghijklmn0")).toBe("scratch");
    expect(temporaryKind("pc_old_abcdefghijklmn0")).toBe("previous");
    // the namespace is strict: 15 lowercase letters or digits, from start to end
    for (const name of [
      "pc_rt_short",
      "pc_rt_abcdefghijklmn01",
      "xpc_rt_abcdefghijklmn0",
      "pc_rt_abcdefghijklmN0",
      "pc_tmp_abcdefghijklmn0",
      "shop",
    ]) {
      expect(temporaryKind(name), name).toBe("orphan");
      expect(temporaryOwner(name), name).toBe("");
    }
  });

  it("names its owner, the check or the restore", () => {
    expect(temporaryOwner("pc_rt_abcdefghijklmn0")).toBe("abcdefghijklmn0");
    expect(temporaryOwner("pc_old_abcdefghijklmn0")).toBe("abcdefghijklmn0");
    expect(temporaryOwner("pc_rt_ABCDEFGHIJKLMN0")).toBe("");
  });

  it("is known by its time or its name, and its prefixes are reserved", () => {
    expect(isTemporary({ name: "shop", temporary_until: "2026-11-18 14:00:00.000Z" })).toBe(true);
    expect(isTemporary({ name: "pc_rs_abcdefghijklmn0", temporary_until: "" })).toBe(true);
    expect(isTemporary({ name: "shop", temporary_until: "" })).toBe(false);
    expect(reservedName("pc_rt_x")).toBe(true);
    expect(reservedName("pc_old_")).toBe(true);
    expect(reservedName("pc_r")).toBe(false);
    expect(reservedName("shop_pc_rt_")).toBe(false);
  });
});
