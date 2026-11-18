import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import i18n from "../i18n";
import type { BackupCheck } from "../lib/backupChecks";
import { jobsApi, type BackupJob } from "../lib/backupJobs";
import { restoresApi } from "../lib/backupRestores";
import { pb, type User } from "../lib/pb";
import type { Alert } from "../lib/types";
import { BackupsPage } from "./Backups";
import { hosts, job, run, signInAs, targets, wrap } from "./BackupRestore.fixtures";

// Wednesday 18 November 2026, 13:00 in Prague: the tests' default slot,
// Wednesdays 05:20 UTC, was this morning
const now = Date.parse("2026-11-18T12:00:00Z");

const live = vi.hoisted(() => ({
  lists: {} as Record<string, unknown[]>,
  records: {} as Record<string, unknown>,
}));
vi.mock("../lib/realtime", () => {
  const list = (collection: string, options: { enabled?: boolean } = {}) => {
    const items = options.enabled === false ? [] : (live.lists[collection] ?? []);
    return { items, loading: false, error: null, total: items.length, reload: () => {} };
  };
  const useLiveRecord = (collection: string, id: string | undefined) => ({
    record: id ? (live.records[`${collection}/${id}`] ?? null) : null,
    loading: false,
    error: null,
  });
  return { useLiveList: list, useLiveRecords: list, useLiveRecord, useNow: () => now };
});

vi.mock("../components/CommandOutput", () => ({
  CommandOutput: ({ title }: { title: ReactNode }) => (
    <section aria-label="Command output">
      <h4>{title}</h4>
    </section>
  ),
}));

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

afterEach(async () => {
  vi.restoreAllMocks();
  pb.authStore.clear();
  live.lists = {};
  live.records = {};
  await i18n.changeLanguage("en");
});

function database(id: string, name: string, fields: Partial<BackupJob> = {}): BackupJob {
  return job({
    id,
    name: `db-main-${name}`,
    host: "h1",
    source_kind: "database",
    source: name,
    engine: "postgres",
    targets: ["t1"],
    schedule: "30 0 * * *",
    origin: "database",
    database: `db-${name}`,
    db_server_name: "db-main",
    own_settings: [],
    ...fields,
  });
}

const blog = database("d2", "blog");
const shop = database("d1", "shop");
const notes = database("d3", "notes");
// dropped, and never tested: not listed
const gone = database("d4", "old", { database: "", enabled: false });

function check(id: string, fields: Partial<BackupCheck>): BackupCheck {
  return {
    id,
    collectionId: "backup_checks",
    collectionName: "backup_checks",
    created: "2026-11-18 04:20:00.000Z",
    updated: "2026-11-18 04:22:00.000Z",
    target: "t1",
    host: "h1",
    kind: "restore_test",
    trigger: "schedule",
    status: "ok",
    scheduled_at: "2026-11-18 04:20:00.000Z",
    started_at: "2026-11-18 04:20:05.000Z",
    finished_at: "2026-11-18 04:21:00.000Z",
    duration_ms: 4200,
    subset: "",
    packs_read: 0,
    packs_total: 0,
    confirmed: false,
    unconfirmed: false,
    errors_more: 0,
    repair: null,
    notes: null,
    lock: null,
    error: "",
    command: "cmd1",
    attempt: 1,
    retry_of: "",
    retry_at: "",
    job: "d1",
    database_name: "shop",
    snapshot_id: `3f2a9c1b${"0".repeat(56)}`,
    temporary: `pc_rt_${id.padEnd(15, "0")}`,
    tables_expected: 12,
    tables_restored: 12,
    dump_bytes: 1048576,
    failure: "",
    ...fields,
  } as BackupCheck;
}

const passed = check("c2", { finished_at: "2026-11-18 04:22:00.000Z" });
const failed = check("c1", {
  job: "d2",
  database_name: "blog",
  status: "failed",
  failure: "damaged",
  tables_restored: 0,
  error: "restic could not read the snapshot: ciphertext verification failed",
  command: "cmd2",
});

const failedAlert = {
  id: "a1",
  collectionId: "alerts",
  collectionName: "alerts",
  rule: "backup_restore_test_failed",
  target: "d2",
  host: "h1",
  severity: "critical",
  status: "open",
  first_seen: "",
  last_seen: "",
  message: "The restore test of blog on db-main failed.",
} as Alert;

function showPage(
  role: User["role"],
  fields: Record<string, unknown[]> = {},
  path = "/backups?job=j1",
) {
  signInAs(role);
  vi.spyOn(restoresApi, "snapshots").mockResolvedValue({ pending: true });
  live.lists = {
    backup_targets: targets,
    hosts,
    backup_jobs: [job({}), blog, shop, notes, gone],
    backup_runs: [run("r1", {})],
    backup_checks: [passed, failed],
    settings: [{ id: "s1", backup_check_groups: 20, restore_test_schedule: "20 5 * * 3" }],
    alerts: [],
    ...fields,
  };
  return wrap(<BackupsPage />, path);
}

function card() {
  return within(screen.getByRole("list", { name: "Restore tests" }));
}

describe("the restore tests card", () => {
  it("lists each database job's newest test, failed first, with when, what and how long", () => {
    showPage("viewer");
    const rows = card().getAllByRole("listitem");
    expect(rows.map((row) => row.querySelector(".mono")?.textContent)).toEqual([
      "blog",
      "notes",
      "shop",
    ]);
    const first = within(rows[0]);
    expect(first.getByText("Failed")).toBeInTheDocument();
    expect(first.getByText("db-main")).toBeInTheDocument();
    expect(rows[0].querySelector(".test-facts")).toHaveTextContent(
      "WhenWednesday 05:21Snapshot3f2a9c1bTables0 / 12Took4 s",
    );
    expect(
      first.getByText("restic could not read the snapshot: ciphertext verification failed"),
    ).toHaveClass("failed");
    expect(within(rows[1]).getByText("No restore test yet")).toBeInTheDocument();
    expect(within(rows[2]).getByText("Passed")).toBeInTheDocument();
    expect(rows[2].querySelector(".test-facts")).toHaveTextContent("Tables12 / 12");
    // the next slot, a week on
    expect(screen.getByText("next Wednesday 06:20")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Weekly, the newest backup of each database goes into a temporary database on its server, which is dropped two hours later.",
      ),
    ).toBeInTheDocument();
  });

  it("puts a failed test first, then one that did not complete or was skipped, then by name", () => {
    showPage("viewer", {
      backup_checks: [
        check("c5", { job: "d1", database_name: "shop", status: "failed" }),
        check("c6", { job: "d3", database_name: "notes", status: "skipped" }),
        check("c7", { job: "d2", database_name: "blog", status: "ok" }),
      ],
    });
    expect(
      card()
        .getAllByRole("listitem")
        .map((row) => row.querySelector(".mono")?.textContent),
    ).toEqual(["shop", "notes", "blog"]);
  });

  it("says when the next try of one that did not complete is", () => {
    showPage("viewer", {
      backup_checks: [
        check("c3", {
          status: "error",
          error: "b2 cannot be reached",
          retry_at: "2026-11-18 13:00:00.000Z",
        }),
      ],
    });
    const row = within(card().getAllByRole("listitem")[0]);
    expect(row.getByText("Did not complete")).toBeInTheDocument();
    expect(row.getByText("b2 cannot be reached")).not.toHaveClass("failed");
    expect(row.getByText("next try Wednesday 14:00")).toBeInTheDocument();
  });

  it("says the tests are off, and is not there without database jobs", () => {
    showPage("viewer", {
      settings: [{ id: "s1", backup_check_groups: 20, restore_test_schedule: "" }],
    });
    // the card's head, and the integrity tile
    expect(screen.getAllByText("restore tests off")).toHaveLength(2);
    expect(screen.queryByText(/^next /)).not.toBeInTheDocument();
    cleanup();
    showPage("viewer", { backup_jobs: [job({})] });
    expect(screen.queryByRole("list", { name: "Restore tests" })).not.toBeInTheDocument();
  });
});

describe("the integrity tile's restore tests", () => {
  function tile() {
    return screen
      .getByText(/^(Integrity check|Kontrola integrity)$/)
      .closest(".kpi") as HTMLElement;
  }

  it("says how many database jobs' newest test passed, and when the next comes", () => {
    showPage("viewer");
    expect(
      within(tile()).getByText("restore tests 1 / 3 OK · Wednesday 06:20"),
    ).toBeInTheDocument();
  });

  it("says the tests are off, and nothing without database jobs or the setting", () => {
    showPage("viewer", {
      settings: [{ id: "s1", backup_check_groups: 20, restore_test_schedule: "" }],
    });
    expect(within(tile()).getByText("restore tests off")).toBeInTheDocument();
    cleanup();
    showPage("viewer", { backup_jobs: [job({})] });
    expect(within(tile()).queryByText(/restore tests/)).not.toBeInTheDocument();
    cleanup();
    // an older panel keeps no schedule
    showPage("viewer", { settings: [{ id: "s1", backup_check_groups: 20 }] });
    expect(within(tile()).queryByText(/restore tests/)).not.toBeInTheDocument();
  });

  it("is said in Czech", async () => {
    await i18n.changeLanguage("cs");
    showPage("viewer");
    expect(within(tile()).getByText(/^testy obnovy 1 \/ 3 v pořádku · /)).toBeInTheDocument();
  });
});

describe("the banner of failed restore tests", () => {
  it("counts the open alerts, and shows the tests", () => {
    const scroll = vi.fn();
    Element.prototype.scrollIntoView = scroll;
    showPage("viewer", {
      alerts: [failedAlert, { ...failedAlert, id: "a2", status: "resolved" }],
    });
    const banner = screen.getByRole("region", {
      name: "1 restore test failed: that backup could not be restored",
    });
    fireEvent.click(within(banner).getByRole("button", { name: "Show the restore tests" }));
    expect(scroll).toHaveBeenCalled();
    expect(scroll.mock.contexts[0]).toBe(document.getElementById("restore-tests"));
  });

  it("is said with the Czech plural, and not at all without an open alert", async () => {
    await i18n.changeLanguage("cs");
    showPage("viewer", { alerts: [failedAlert, { ...failedAlert, id: "a2", target: "d1" }] });
    expect(
      screen.getByRole("region", { name: "2 testy obnovy selhaly: ty zálohy nešlo obnovit" }),
    ).toBeInTheDocument();
    cleanup();
    showPage("viewer", {
      alerts: [
        { ...failedAlert, status: "resolved" },
        { ...failedAlert, rule: "backup_failed" },
      ],
    });
    expect(screen.queryByRole("region", { name: /testy? obnovy/ })).not.toBeInTheDocument();
  });
});

describe("a restore test's details", () => {
  it("open from its row, with what it restored and its error, and test again for an operator", async () => {
    live.records["backup_checks/c1"] = failed;
    const test = vi.spyOn(jobsApi, "restoreTest").mockResolvedValue({ check: "c9" });
    const view = showPage("operator");
    fireEvent.click(card().getByRole("button", { name: "blog" }));
    expect(screen.getByTestId("location")).toHaveTextContent("test=c1");

    const drawer = within(view.container.ownerDocument.querySelector("dialog.drawer")!);
    expect(drawer.getByRole("heading", { name: "Restore tests · blog" })).toBeInTheDocument();
    expect(drawer.getByText("db-main/blog")).toBeInTheDocument();
    expect(drawer.getByText("db-main-blog")).toBeInTheDocument();
    expect(drawer.getByText("Backblaze B2")).toBeInTheDocument();
    expect(drawer.getByText("3f2a9c1b · 1 MB")).toBeInTheDocument();
    expect(drawer.getByText("0 / 12")).toBeInTheDocument();
    expect(
      drawer.getByText("restic could not read the snapshot: ciphertext verification failed"),
    ).toBeInTheDocument();
    fireEvent.click(drawer.getByRole("button", { name: "Show output" }));
    expect(screen.getByRole("region", { name: "Command output" })).toHaveTextContent(
      "Restore tests · blog",
    );

    fireEvent.click(drawer.getByRole("button", { name: "Test the restore now" }));
    await waitFor(() => expect(test).toHaveBeenCalledWith("d2"));
    expect(
      await screen.findByText(
        "The restore test waits for its server: the newest backup goes into a temporary database, dropped two hours after the test.",
      ),
    ).toBeInTheDocument();

    fireEvent.click(drawer.getByRole("button", { name: "Close" }));
    expect(screen.getByTestId("location")).not.toHaveTextContent("test=");
  });

  it("let a viewer read them only", () => {
    live.records["backup_checks/c2"] = passed;
    showPage("viewer", {}, "/backups?test=c2");
    const drawer = within(document.querySelector("dialog.drawer") as HTMLElement);
    expect(drawer.getByText("Passed")).toBeInTheDocument();
    expect(drawer.queryByRole("button", { name: "Test the restore now" })).not.toBeInTheDocument();
    act(() => {
      fireEvent.click(drawer.getByRole("button", { name: "Close" }));
    });
  });
});
