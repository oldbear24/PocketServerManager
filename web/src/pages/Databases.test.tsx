import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

import { ToastProvider } from "../components/toast";
import i18n from "../i18n";
import { AuthProvider } from "../lib/auth";
import type { BackupCheck } from "../lib/backupChecks";
import { jobsApi, type BackupJob, type BackupRun } from "../lib/backupJobs";
import { restoresApi } from "../lib/backupRestores";
import type { BackupTarget } from "../lib/backups";
import { autoSchedule, dailyTime } from "../lib/databaseBackups";
import { dbApi, type Database, type DbServer } from "../lib/databases";
import { pb, type User } from "../lib/pb";
import type { Container, Host } from "../lib/types";
import { matchesFilter } from "../test-filter";
import { DatabasesPage } from "./Databases";

// Wednesday 18 November 2026, 13:00 in Prague, on winter time
const now = Date.parse("2026-11-18T12:00:00Z");

const live = vi.hoisted(() => ({
  lists: {} as Record<string, unknown[]>,
  /** what each list of a collection asked for */
  asked: {} as Record<string, Record<string, unknown>[]>,
}));
vi.mock("../lib/realtime", () => {
  const list = (
    collection: string,
    options: { filter?: string; enabled?: boolean } & Record<string, unknown> = {},
  ) => {
    (live.asked[collection] ??= []).push(options);
    const items =
      options.enabled === false
        ? []
        : (live.lists[collection] ?? []).filter(
            (item) =>
              !options.filter || matchesFilter(item as Record<string, unknown>, options.filter),
          );
    return { items, loading: false, error: null, total: items.length, reload: () => {} };
  };
  return {
    useLiveList: list,
    useLiveRecords: list,
    useLiveRecord: () => ({ record: null, loading: false, error: null }),
    useNow: () => now,
  };
});

const zone = process.env.TZ;

beforeAll(() => {
  process.env.TZ = "Europe/Prague";
  HTMLDialogElement.prototype.showModal = function showModal(this: HTMLDialogElement) {
    this.open = true;
  };
  HTMLDialogElement.prototype.close = function close(this: HTMLDialogElement) {
    this.open = false;
  };
});

afterAll(() => {
  if (zone === undefined) {
    delete process.env.TZ;
  } else {
    process.env.TZ = zone;
  }
});

beforeEach(() => {
  vi.spyOn(dbApi, "candidates").mockResolvedValue([]);
  vi.spyOn(dbApi, "stats").mockResolvedValue({} as never);
});

afterEach(async () => {
  vi.restoreAllMocks();
  pb.authStore.clear();
  live.lists = {};
  live.asked = {};
  await i18n.changeLanguage("en");
});

function signInAs(role: User["role"]) {
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

const hosts = [
  { id: "h1", name: "pve-01", status: "online", capabilities: ["backup.database"] },
  { id: "h2", name: "nas", status: "online" },
] as Host[];

function server(fields: Partial<DbServer>): DbServer {
  return {
    id: "s1",
    collectionId: "db_servers",
    collectionName: "db_servers",
    host: "h1",
    name: "db-main",
    engine: "postgres",
    version: "17.6",
    kind: "shared",
    image: "postgres:17",
    container_ref: "pcdb-main-db-1",
    service: "",
    stack: "",
    volume: "pcdb-main",
    port: 5432,
    admin_user: "postgres",
    max_connections: 100,
    connections: 10,
    extensions: [],
    status: "ready",
    size_bytes: 1e9,
    stats_at: "",
    last_error: "",
    ...fields,
  } as DbServer;
}

const main = server({});
const umamiServer = server({
  id: "s2",
  name: "umami-db",
  kind: "dedicated",
  stack: "k1",
  service: "db",
  container_ref: "umami-db-1",
});

function database(id: string, fields: Partial<Database> = {}): Database {
  return {
    id,
    collectionId: "databases",
    collectionName: "databases",
    server: "s1",
    name: id,
    owner_kind: "stack",
    stack: "",
    site: "",
    size_bytes: 1e6,
    connections: 1,
    temporary_until: "",
    status: "ready",
    last_error: "",
    ...fields,
  } as Database;
}

const shop = database("shop", { stack: "k2" });
const blog = database("blog", { owner_kind: "site", site: "site1" });
const fresh = database("fresh", { status: "creating" });
// a restore test's copy, the data before a restore, and one nothing made
const test = database("pc_rt_abcdefghijklmno", { temporary_until: "2026-11-18 14:00:00.000Z" });
const before = database("pc_old_restore00000001", {
  name: "pc_old_restore00000001",
  temporary_until: "2026-11-19 10:00:00.000Z",
});
const orphan = database("pc_rt_zzzzzzzzzzzzzzz", {
  temporary_until: "2026-11-18 13:30:00.000Z",
  status: "dropping",
});
const umami = database("umami", { server: "s2", stack: "k1" });

function job(fields: Partial<BackupJob>): BackupJob {
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
    retention: { daily: 14, weekly: 4, monthly: 6 },
    enabled: true,
    origin: "database",
    database: "shop",
    db_server_name: "db-main",
    own_settings: ["retention"],
    last_success_at: "2026-11-18 00:31:00.000Z",
    label_removed: false,
    problem: "",
    next_run_at: "2026-11-19 00:30:00.000Z",
    last_run_at: "",
    last_status: "",
    ...fields,
  } as BackupJob;
}

const shopJob = job({});
const blogJob = job({
  id: "j2",
  name: "db-main-blog",
  source: "blog",
  database: "blog",
  targets: [],
  own_settings: [],
  last_success_at: "",
});
const umamiJob = job({
  id: "j3",
  name: "umami-db",
  source_kind: "db_dump",
  source: "",
  stack: "k1",
  service: "db",
  origin: "label",
  database: "",
  db_server_name: "",
  last_success_at: "",
  last_status: "succeeded",
  last_run_at: "2026-11-18 02:00:00.000Z",
});
// a manual job of a volume is no database's
const volumeJob = job({ id: "j4", name: "photos", source_kind: "volume", origin: "manual" });

const runs = [
  {
    id: "r1",
    collectionId: "backup_runs",
    collectionName: "backup_runs",
    created: "2026-11-18 00:30:00.000Z",
    job: "j1",
    target: "t1",
    status: "succeeded",
    scheduled_at: "2026-11-18 00:30:00.000Z",
    started_at: "2026-11-18 00:30:01.000Z",
    finished_at: "2026-11-18 00:31:00.000Z",
    total_bytes: 4100,
    bytes_added: 1000,
  },
] as BackupRun[];

function check(id: string, fields: Partial<BackupCheck>): BackupCheck {
  return {
    id,
    collectionId: "backup_checks",
    collectionName: "backup_checks",
    created: "2026-11-18 04:20:00.000Z",
    kind: "restore_test",
    status: "ok",
    job: "j1",
    database_name: "shop",
    started_at: "2026-11-18 04:20:05.000Z",
    finished_at: "2026-11-18 04:21:00.000Z",
    tables_expected: 12,
    tables_restored: 12,
    error: "",
    ...fields,
  } as BackupCheck;
}

const failedTest = check("abcdefghijklmno", {
  status: "failed",
  tables_restored: 0,
  error: "ciphertext verification failed",
});

const targets = [
  { id: "t1", name: "Backblaze B2", kind: "s3", host: "h1" },
  { id: "t2", name: "nas disk", kind: "local", host: "h2" },
] as BackupTarget[];

const containers = [
  {
    id: "c1",
    host: "h1",
    name: "pcdb-main-db-1",
    project: "pcdb-main",
    state: "running",
    health: "",
  },
  { id: "c2", host: "h1", name: "umami-db-1", project: "umami", state: "running", health: "" },
] as Container[];

function showPage(role: User["role"], fields: Record<string, unknown[]> = {}) {
  signInAs(role);
  live.lists = {
    db_servers: [main, umamiServer],
    databases: [shop, blog, fresh, test, before, orphan, umami],
    db_users: [],
    db_moves: [],
    db_networks: [],
    hosts,
    stacks: [
      { id: "k1", name: "umami", host: "h1" },
      { id: "k2", name: "shop", host: "h1" },
    ],
    containers,
    backup_jobs: [shopJob, blogJob, umamiJob, volumeJob],
    backup_runs: runs,
    backup_checks: [failedTest],
    sites: [{ id: "site1", name: "wp" }],
    backup_targets: targets,
    settings: [{ id: "s1", restore_test_schedule: "20 5 * * 3" }],
    backup_restores: [{ id: "restore00000001", mode: "database", database_name: "shop" }],
    db_dumps: [],
    ...fields,
  };
  return render(
    <MemoryRouter initialEntries={["/databases"]}>
      <AuthProvider>
        <ToastProvider>
          <DatabasesPage />
        </ToastProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

function tile(label: string) {
  const labels = [...document.querySelectorAll(".kpi-label > span")];
  return labels.find((item) => item.textContent === label)!.closest(".kpi") as HTMLElement;
}

function row(name: string) {
  const button = screen.getByRole("button", { name });
  return within(button.closest("tr")!);
}

function card(title: string) {
  return within(screen.getByRole("heading", { name: title }).closest("section") as HTMLElement);
}

describe("the KPI of the databases' backups", () => {
  it("counts the ready databases of shared servers backed up within a day, and warns of no target", () => {
    showPage("viewer");
    const backups = tile("Backups in 24 h");
    // shop and blog: neither the one being created, the copies nor umami's dedicated server
    expect(within(backups).getByText("1 / 2")).toHaveClass("kpi-value", "warn");
    expect(backups.querySelector("svg")).not.toBeNull();
    expect(within(backups).getByText("1 database has no target")).toBeInTheDocument();
    // the copies are in no figure
    expect(within(tile("Databases")).getByText("4")).toBeInTheDocument();
  });

  it("is OK when every one was backed up, and a dash without any", async () => {
    showPage("viewer", {
      backup_jobs: [
        shopJob,
        { ...blogJob, targets: ["t1"], last_success_at: shopJob.last_success_at },
      ],
    });
    const backups = tile("Backups in 24 h");
    expect(within(backups).getByText("2 / 2")).toHaveClass("ok");
    expect(
      within(backups).getByText("a dump of every database, each on its own"),
    ).toBeInTheDocument();
    cleanup();

    await i18n.changeLanguage("cs");
    showPage("viewer", { databases: [umami] });
    const none = tile("Zálohy za 24 h");
    expect(within(none).getByText("–")).toBeInTheDocument();
    expect(within(none).getByText("zatím žádná databáze na sdíleném serveru")).toBeInTheDocument();
  });

  it("says the databases without a target with the Czech plural", async () => {
    await i18n.changeLanguage("cs");
    showPage("viewer", { backup_jobs: [{ ...shopJob, targets: [] }, blogJob] });
    expect(within(tile("Zálohy za 24 h")).getByText("2 databáze nemají cíl")).toBeInTheDocument();
  });

  it("loads the jobs of databases, sites and dumps, two days of runs, and the restore tests", () => {
    showPage("viewer");
    expect(live.asked.backup_jobs?.[0]).toMatchObject({
      filter: "origin = 'database' || origin = 'site' || source_kind = 'db_dump'",
      perPage: 500,
    });
    expect(live.asked.backup_runs?.[0]?.filter).toBe('created >= "2026-11-16 00:00:00.000Z"');
    expect(live.asked.backup_checks?.[0]).toMatchObject({
      filter: "kind = 'restore_test'",
      sort: "-created",
      perPage: 500,
    });
  });
});

describe("the databases' table", () => {
  it("shows each database's Backup cell, linking to its job", () => {
    showPage("viewer");
    const shopRow = row("shop");
    const ok = shopRow.getByText("OK · 01:31");
    expect(ok.closest("a")).toHaveAttribute("href", "/backups?job=j1");
    // its newest restore test that ended failed
    expect(shopRow.getByText("restore test failed")).toHaveClass("db-backup-test");
    expect(row("blog").getByText("No target")).toBeInTheDocument();
    // its job comes once it is ready
    expect(row("fresh").getByText("—")).toBeInTheDocument();
  });

  it("links a site's database to its site", () => {
    showPage("viewer");
    expect(row("blog").getByRole("link", { name: "web wp" })).toHaveAttribute(
      "href",
      "/sites/site1",
    );
  });

  it("says whose a temporary database is and when it is dropped, without a Backup cell", () => {
    showPage("operator");
    const copy = row("pc_rt_abcdefghijklmno");
    expect(copy.getByText("restore test of shop · dropped in 2 hr.")).toBeInTheDocument();
    expect(copy.getByText("—")).toBeInTheDocument();
    expect(
      row("pc_old_restore00000001").getByText("shop before the restore · dropped in 22 hr."),
    ).toBeInTheDocument();
    expect(row("pc_rt_zzzzzzzzzzzzzzz").getByText("being dropped")).toBeInTheDocument();
  });

  it("says a copy nothing of the panel's owns was left behind", () => {
    showPage("operator", { backup_checks: [], backup_restores: [] });
    expect(
      row("pc_rt_abcdefghijklmno").getByText("left behind · dropped in 2 hr."),
    ).toBeInTheDocument();
    expect(
      row("pc_old_restore00000001").getByText("left behind · dropped in 22 hr."),
    ).toBeInTheDocument();
  });

  it("shows a dedicated server's database the stack's job", () => {
    showPage("viewer");
    fireEvent.click(screen.getByRole("button", { name: /^umami-db/ }));
    const cell = row("umami");
    expect(cell.getByText("OK · 03:00").closest("a")).toHaveAttribute("href", "/backups?job=j3");
    expect(cell.getByText("by the job umami-db")).toHaveAttribute(
      "title",
      "A dedicated server is backed up by its stack's job; a PostgreSQL dump of it holds its POSTGRES_DB database only.",
    );
    expect(
      card("Backups").getByText("A dedicated server is backed up by its stack's jobs."),
    ).toBeInTheDocument();
  });
});

describe("a shared server's card", () => {
  it("says when its databases are backed up, or that they have no target", () => {
    showPage("viewer", {
      backup_jobs: [shopJob, { ...blogJob, targets: ["t1"] }],
    });
    const button = screen.getByRole("button", { name: /^db-main/ });
    expect(within(button).getByText("Backups daily 01:30 · each database on its own")).toHaveClass(
      "muted",
    );
    cleanup();
    showPage("viewer");
    expect(
      within(screen.getByRole("button", { name: /^db-main/ })).getByText(
        "Backups have no target yet: choose one",
      ),
    ).toHaveClass("warn-text");
  });
});

describe("a database's backups card", () => {
  it("says its job, when, where, how long, its last backup and its restore test", () => {
    showPage("viewer");
    const backups = card("Backups");
    expect(backups.getByRole("link", { name: "db-main-shop" })).toHaveAttribute(
      "href",
      "/backups?job=j1",
    );
    expect(backups.getByText("daily 01:30")).toBeInTheDocument();
    expect(backups.getAllByText("· as set for pve-01")).toHaveLength(2);
    expect(backups.getByText("· its own")).toBeInTheDocument();
    expect(backups.getByText("14d 4w 6m")).toBeInTheDocument();
    expect(backups.getByText("01:31 to Backblaze B2: dump 4.1 kB, 1 kB new")).toBeInTheDocument();
    expect(backups.getByText("Failed Wednesday 05:21: ciphertext verification failed")).toHaveClass(
      "err-text",
    );
    // a viewer reads it
    expect(backups.queryByRole("button")).not.toBeInTheDocument();
  });

  it("says a passed test, none yet with the next slot, or that tests are off", () => {
    showPage("viewer", { backup_checks: [check("c2", {})] });
    expect(card("Backups").getByText("Passed Wednesday 05:21 · 12 tables")).toBeInTheDocument();
    cleanup();
    showPage("viewer", { backup_checks: [] });
    expect(card("Backups").getByText("None yet · next Wednesday 06:20")).toBeInTheDocument();
    cleanup();
    showPage("viewer", { backup_checks: [], settings: [{ id: "s1", restore_test_schedule: "" }] });
    expect(card("Backups").getByText("Restore tests are off")).toBeInTheDocument();
  });

  it("lets an operator back up, test and restore now, and an admin change the job", async () => {
    const run = vi.spyOn(jobsApi, "run").mockResolvedValue({ runs: ["r9"] });
    const restoreTest = vi.spyOn(jobsApi, "restoreTest").mockResolvedValue({ check: "c9" });
    vi.spyOn(restoresApi, "snapshots").mockResolvedValue({ pending: true });
    showPage("operator");
    const backups = card("Backups");
    expect(backups.queryByRole("button", { name: "Backup settings…" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Backup settings" })).not.toBeInTheDocument();

    fireEvent.click(backups.getByRole("button", { name: "Back up now" }));
    await waitFor(() => expect(run).toHaveBeenCalledWith("j1"));
    expect(await screen.findByText("db-main-shop runs now, to 1 target.")).toBeInTheDocument();

    fireEvent.click(backups.getByRole("button", { name: "Test the restore now" }));
    await waitFor(() => expect(restoreTest).toHaveBeenCalledWith("j1"));
    expect(
      await screen.findByText(
        "The restore test waits for its server: the newest backup goes into a temporary database, dropped two hours after the test.",
      ),
    ).toBeInTheDocument();

    fireEvent.click(backups.getByRole("button", { name: "Restore…" }));
    expect(
      await screen.findByRole("heading", { name: "Restore db-main-shop" }, { timeout: 5000 }),
    ).toBeInTheDocument();
    cleanup();

    showPage("admin");
    fireEvent.click(card("Backups").getByRole("button", { name: "Backup settings…" }));
    expect(screen.getByRole("dialog", { name: "Edit db-main-shop" })).toBeInTheDocument();
  });

  it("waits for a database that is not ready", () => {
    showPage("operator");
    fireEvent.click(screen.getByRole("button", { name: "fresh" }));
    expect(
      card("Backups").getByText("Its backup job appears once the database is ready."),
    ).toBeInTheDocument();
    expect(card("Backups").queryByRole("button")).not.toBeInTheDocument();
  });
});

describe("a temporary database's card", () => {
  it("says the panel drops it, and lets an operator drop it now", async () => {
    const drop = vi.spyOn(dbApi, "drop").mockResolvedValue({} as never);
    showPage("operator");
    fireEvent.click(screen.getByRole("button", { name: "pc_rt_abcdefghijklmno" }));
    const temporary = card("Temporary database pc_rt_abcdefghijklmno");
    expect(
      temporary.getByText(
        /^PocketCloud made this database for a restore or a restore test and drops it by itself at /,
      ),
    ).toBeInTheDocument();
    // its connection offers nothing
    const connection = card("Connection · pc_rt_abcdefghijklmno");
    expect(connection.queryByRole("button")).not.toBeInTheDocument();
    expect(connection.queryByText(/secret:db/)).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Backups" })).not.toBeInTheDocument();

    fireEvent.click(temporary.getByRole("button", { name: "Drop now" }));
    const dialog = within(
      screen.getByRole("dialog", { name: "Drop pc_rt_abcdefghijklmno now? It cannot be undone." }),
    );
    fireEvent.click(dialog.getByRole("button", { name: "Drop now" }));
    await waitFor(() => expect(drop).toHaveBeenCalledWith("pc_rt_abcdefghijklmno"));
    expect(await screen.findByText("pc_rt_abcdefghijklmno is being dropped.")).toBeInTheDocument();
  });

  it("is a viewer's to read only, and offers nothing while it is dropped", () => {
    showPage("viewer");
    fireEvent.click(screen.getByRole("button", { name: "pc_rt_abcdefghijklmno" }));
    expect(
      card("Temporary database pc_rt_abcdefghijklmno").queryByRole("button"),
    ).not.toBeInTheDocument();
    cleanup();
    showPage("operator");
    fireEvent.click(screen.getByRole("button", { name: "pc_rt_zzzzzzzzzzzzzzz" }));
    expect(
      card("Temporary database pc_rt_zzzzzzzzzzzzzzz").queryByRole("button"),
    ).not.toBeInTheDocument();
  });
});

describe("a server's defaults for its databases' backups", () => {
  it("are an admin's: when, where and how long, saved for the jobs that follow them", async () => {
    const save = vi.spyOn(jobsApi, "hostDefaults").mockResolvedValue({ host: {}, jobs_changed: 2 });
    showPage("admin");
    fireEvent.click(screen.getByRole("button", { name: "Backup settings" }));
    const dialog = within(screen.getByRole("dialog", { name: "Automatic backups on pve-01" }));
    expect(
      dialog.getByText(
        "Every database of a shared server on pve-01, and every PHP site there, has a backup job of its own. Each job follows these settings unless it has its own.",
      ),
    ).toBeInTheDocument();
    // empty is the panel's own schedule, which the server's id picks
    expect(
      dialog.getByText(`Empty: daily at ${dailyTime(autoSchedule("h1"))}, this server's own time`),
    ).toBeInTheDocument();
    expect(dialog.getByLabelText("Backblaze B2")).toBeEnabled();
    // a directory of nas backs up nas's jobs only
    expect(dialog.getByLabelText(/^nas disk/)).toBeDisabled();
    expect(dialog.getByText("No target: the jobs wait until one is chosen.")).toBeInTheDocument();
    // shop's job keeps its own retention, but follows the rest
    expect(dialog.getByText("2 jobs follow these settings")).toBeInTheDocument();
    expect(dialog.getByLabelText("Daily")).toHaveValue(7);

    fireEvent.change(dialog.getByLabelText("When (cron, UTC)"), {
      target: { value: "15 1 * * *" },
    });
    expect(dialog.getByText("In your time: daily 02:15.")).toBeInTheDocument();
    fireEvent.click(dialog.getByLabelText("Backblaze B2"));
    fireEvent.click(dialog.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(save).toHaveBeenCalledWith("h1", {
        schedule: "15 1 * * *",
        targets: ["t1"],
        retention: { last: 0, hourly: 0, daily: 7, weekly: 4, monthly: 6, yearly: 0 },
      }),
    );
    expect(await screen.findByText("Saved: 2 jobs changed.")).toBeInTheDocument();
  });

  it("start from what the server has", () => {
    showPage("admin", {
      hosts: [
        {
          ...hosts[0],
          auto_backup_schedule: "0 2 * * *",
          auto_backup_targets: ["t1"],
          auto_backup_retention: { daily: 3 },
        },
        hosts[1],
      ],
    });
    fireEvent.click(screen.getByRole("button", { name: "Backup settings" }));
    const dialog = within(screen.getByRole("dialog", { name: "Automatic backups on pve-01" }));
    expect(dialog.getByLabelText("When (cron, UTC)")).toHaveValue("0 2 * * *");
    expect(dialog.getByLabelText("Backblaze B2")).toBeChecked();
    expect(dialog.getByLabelText("Daily")).toHaveValue(3);
    expect(dialog.getByLabelText("Weekly")).toHaveValue(null);
  });
});
