import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { ClientResponseError } from "pocketbase";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

import { ToastProvider } from "../components/toast";
import i18n from "../i18n";
import { AuthProvider } from "../lib/auth";
import { jobsApi, type BackupJob, type BackupLock, type BackupRun } from "../lib/backupJobs";
import { restoresApi } from "../lib/backupRestores";
import type { BackupTarget } from "../lib/backups";
import { pb, type User } from "../lib/pb";
import type { Host } from "../lib/types";
import { BackupsPage } from "./Backups";

// Monday 13:00 in Prague, on winter time: an hour ahead of UTC
const now = Date.parse("2026-11-09T12:00:00Z");

const live = vi.hoisted(() => ({
  lists: {} as Record<string, unknown[]>,
  records: {} as Record<string, Record<string, unknown>>,
  /** the records still on their way */
  loading: new Set<string>(),
}));
vi.mock("../lib/realtime", () => {
  const list = (collection: string) => {
    const items = live.lists[collection] ?? [];
    return { items, loading: false, error: null, total: items.length, reload: () => {} };
  };
  return {
    useLiveList: list,
    useLiveRecords: list,
    useLiveRecord: (collection: string, id?: string) => ({
      record: (id && live.records[collection]?.[id]) || null,
      loading: Boolean(id && live.loading.has(id)),
      error: null,
    }),
    useNow: () => now,
  };
});

// the output streams from the panel; here it only says what it is of
vi.mock("../components/CommandOutput", () => ({
  CommandOutput: ({ commandId, title }: { commandId: string; title: ReactNode }) => (
    <section aria-label="Command output" data-command={commandId}>
      <h4>{title}</h4>
    </section>
  ),
}));

const zone = process.env.TZ;

beforeAll(() => {
  // the times are the browser's, here Prague's
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
  // the page lists the picked job's snapshots; their own tests are elsewhere
  vi.spyOn(restoresApi, "snapshots").mockResolvedValue({ pending: true });
});

afterEach(async () => {
  vi.restoreAllMocks();
  pb.authStore.clear();
  live.lists = {};
  live.records = {};
  live.loading = new Set();
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
  { id: "h1", name: "pve-01", status: "online", stream_open: true },
  { id: "h2", name: "nas", status: "online", stream_open: true },
] as Host[];

function target(fields: Partial<BackupTarget>): BackupTarget {
  return {
    id: "t1",
    collectionId: "backup_targets",
    collectionName: "backup_targets",
    name: "Backblaze B2",
    kind: "s3",
    host: "h1",
    location: "s3:homelab-backups",
    repo_id: "5f1c2a",
    status: "ok",
    integrity_status: "ok",
    integrity_error: "",
    ...fields,
  } as BackupTarget;
}

const targets = [
  target({}),
  target({ id: "t2", name: "Panel disk", kind: "local", host: "h2" }),
  target({ id: "t3", name: "backup-node", kind: "sftp" }),
];

function job(fields: Partial<BackupJob>): BackupJob {
  return {
    id: "j2",
    collectionId: "backup_jobs",
    collectionName: "backup_jobs",
    name: "photos",
    host: "h2",
    source_kind: "path",
    source: "/mnt/tank/photos",
    targets: ["t3"],
    schedule: "0 2 * * *",
    retention: { last: 20 },
    enabled: true,
    origin: "manual",
    label_removed: false,
    problem: "",
    next_run_at: "2026-11-10 02:00:00.000Z",
    ...fields,
  } as BackupJob;
}

const photos = job({});
const vaultwarden = job({
  id: "j1",
  name: "vaultwarden",
  source_kind: "volume",
  source: "vw-data",
  targets: ["t1", "t2"],
  schedule: "0 * * * *",
  next_run_at: "2026-11-09 13:00:00.000Z",
});

// the lock restic named: held by an integrity check on pve-01, last
// refreshed at 02:58 in Prague
const checkLock: BackupLock = {
  pid: 4121,
  host: "pc-k3j9x2m4q8w1z7v",
  user: "root",
  exclusive: true,
  created: "2026-11-09 01:58:00.000Z",
  age_s: 120,
  storage_id: "957548b2",
  command: "k3j9x2m4q8w1z7v",
  command_type: "backup.check",
  command_label: "Check the integrity of b2",
  command_host: "h1",
  command_host_name: "pve-01",
};

const lockedError =
  "Fatal: unable to create lock in backend: repository is already locked exclusively by PID 4121 on pc-k3j9x2m4q8w1z7v by root (UID 0, GID 0)\nlock was created at 2026-11-09 02:58:00 (2m0s ago)\nstorage ID 957548b2";

function run(id: string, fields: Partial<BackupRun>): BackupRun {
  return {
    id,
    collectionId: "backup_runs",
    collectionName: "backup_runs",
    created: "2026-11-09 02:00:00.000Z",
    updated: "",
    job: "j2",
    target: "t3",
    host: "h2",
    trigger: "schedule",
    status: "failed",
    scheduled_at: "2026-11-09 02:00:00.000Z",
    started_at: "2026-11-09 02:00:01.000Z",
    finished_at: "2026-11-09 02:00:05.000Z",
    duration_ms: 4000,
    snapshot_id: "",
    bytes_added: 0,
    total_bytes: 0,
    files_new: 0,
    files_changed: 0,
    forgotten: 0,
    retention: "",
    error: lockedError,
    command: "cmd-r1",
    attempt: 1,
    retry_of: "",
    retry_at: "",
    retry_note: "",
    failure: "locked",
    lock: checkLock,
    ...fields,
  } as BackupRun;
}

function show(
  role: User["role"],
  runs: BackupRun[],
  fields: Record<string, unknown[]> = {},
  commands: Record<string, unknown> = {},
) {
  signInAs(role);
  live.lists = {
    backup_targets: targets,
    hosts,
    backup_jobs: [photos, vaultwarden],
    backup_runs: runs,
    ...fields,
  };
  live.records = { commands };
  render(
    <MemoryRouter initialEntries={["/backups"]}>
      <AuthProvider>
        <ToastProvider>
          <BackupsPage />
        </ToastProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

function problems() {
  return within(screen.getByRole("region", { name: "Backup problems" }));
}

function banner(title: string) {
  return within(screen.getByRole("region", { name: title }));
}

const running = { id: "k3j9x2m4q8w1z7v", status: "running", host: "h1" };
const ended = { id: "k3j9x2m4q8w1z7v", status: "succeeded", host: "h1" };

describe("the failed backups", () => {
  it("says a failure in Czech as the design has it, with restic's line and when it locked", async () => {
    await i18n.changeLanguage("cs");
    // no retry left: the red of what fixes it
    show("operator", [run("r1", { attempt: 4 })], {}, { k3j9x2m4q8w1z7v: ended });

    const failed = banner("photos @ nas selhala ve 03:00");
    expect(failed.getByRole("heading", { level: 2 })).toHaveTextContent(
      "photos @ nas selhala ve 03:00",
    );
    const said = failed.getByText(
      "repository is already locked exclusively by PID 4121 on pc-k3j9x2m4q8w1z7v by root (UID 0, GID 0) (02:58)",
    );
    expect(said).toHaveClass("mono");
    expect(failed.getByRole("button", { name: "Odemknout a zopakovat" })).toHaveClass("critical");
    expect(failed.getByRole("button", { name: "Zobrazit log" })).toBeInTheDocument();
    cleanup();

    // "v" before five
    show(
      "operator",
      [run("r1", { finished_at: "2026-11-09 04:00:00.000Z" })],
      {},
      {
        k3j9x2m4q8w1z7v: ended,
      },
    );
    expect(screen.getByRole("region", { name: "photos @ nas selhala v 05:00" })).toBeVisible();
  });

  it("says the day of a failure before today", async () => {
    const yesterday = run("r1", {
      created: "2026-11-08 02:00:00.000Z",
      finished_at: "2026-11-08 02:00:05.000Z",
      failure: "restic",
      lock: null,
      error: "Fatal: unable to save snapshot: connection reset by peer",
    });
    show("operator", [yesterday]);
    const failed = banner("photos @ nas failed Sunday 03:00");
    expect(failed.getByText("unable to save snapshot: connection reset by peer")).toHaveClass(
      "mono",
    );
    cleanup();

    await i18n.changeLanguage("cs");
    show("operator", [yesterday]);
    expect(
      screen.getByRole("region", { name: "photos @ nas selhala (neděle 03:00)" }),
    ).toBeVisible();
  });

  it("is a labelled region, never an alert read out at every update", () => {
    show("viewer", [run("r1", {})], {}, { k3j9x2m4q8w1z7v: ended });
    expect(screen.getByRole("region", { name: "Backup problems" })).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("holds Unlock and retry back while the restic holding the lock still runs", () => {
    show(
      "operator",
      [run("r1", { retry_at: "2026-11-09 02:05:00.000Z" })],
      {},
      { k3j9x2m4q8w1z7v: running },
    );
    const failed = banner("photos @ nas failed at 03:00");
    expect(failed.getByRole("heading", { level: 2 }).closest(".issue-banner")).toHaveClass("warn");
    expect(
      failed.getByText("Held by “Check the integrity of b2” on pve-01, which is still running."),
    ).toBeInTheDocument();
    expect(failed.getByText("attempt 1 of 4 · retries when it ends")).toBeInTheDocument();
    const unlock = failed.getByRole("button", { name: "Unlock and retry" });
    expect(unlock).toBeDisabled();
    expect(unlock).toHaveAttribute(
      "title",
      "A restic that is still running holds the lock: PocketCloud retries once it ends.",
    );
  });

  it("holds Unlock and retry back until it knows whether the holder runs", () => {
    live.loading = new Set(["k3j9x2m4q8w1z7v"]);
    show("operator", [run("r1", { attempt: 4 })]);
    expect(
      banner("photos @ nas failed at 03:00").getByRole("button", { name: "Unlock and retry" }),
    ).toBeDisabled();
  });

  it("counts a command that timed out while its server is away as still running", () => {
    show(
      "operator",
      [run("r1", {})],
      { hosts: [{ ...hosts[0], stream_open: false }, hosts[1]] },
      { k3j9x2m4q8w1z7v: { ...running, status: "timed_out" } },
    );
    expect(
      banner("photos @ nas failed at 03:00").getByRole("button", { name: "Unlock and retry" }),
    ).toBeDisabled();
  });

  it("says a lock was left by a command that ended, and when restic counts it as stale", () => {
    // refreshed at 12:50 UTC: stale from 13:22, in Prague 14:22
    const young = { ...checkLock, created: "2026-11-09 11:50:00.000Z" };
    show(
      "operator",
      [run("r1", { lock: young, attempt: 4, finished_at: "2026-11-09 11:55:00.000Z" })],
      {},
      { k3j9x2m4q8w1z7v: ended },
    );
    const failed = banner("photos @ nas failed at 12:55");
    expect(failed.getByRole("heading", { level: 2 }).closest(".issue-banner")).toHaveClass("err");
    expect(
      failed.getByText("Left by “Check the integrity of b2” on pve-01, which has ended."),
    ).toBeInTheDocument();
    expect(
      failed.getByText("If it is gone, restic counts its lock as stale from 13:22."),
    ).toBeInTheDocument();
    expect(failed.getByText("attempt 4 of 4 · no more automatic retries")).toBeInTheDocument();
    expect(failed.getByRole("button", { name: "Unlock and retry" })).toBeEnabled();
  });

  it("says when a restic PocketCloud does not run holds the lock", () => {
    const foreign: BackupLock = {
      pid: 812,
      host: "laptop",
      user: "anna",
      exclusive: false,
      created: "2026-11-09 01:58:00.000Z",
    };
    show("operator", [run("r1", { lock: foreign, retry_at: "2026-11-09 02:35:00.000Z" })]);
    const failed = banner("photos @ nas failed at 03:00");
    expect(failed.getByRole("heading", { level: 2 }).closest(".issue-banner")).toHaveClass("warn");
    expect(
      failed.getByText(
        "Held by restic on laptop (PID 812), which PocketCloud does not run; last refreshed: 02:58.",
      ),
    ).toBeInTheDocument();
    // stale long ago: nothing to wait for
    expect(failed.queryByText(/counts its lock as stale/)).not.toBeInTheDocument();
    expect(failed.getByText("attempt 1 of 4 · next try: 03:35")).toBeInTheDocument();
    expect(failed.getByRole("button", { name: "Unlock and retry" })).toHaveClass("warning");
  });

  it("unlocks and retries after saying what unlock removes, and the lock", async () => {
    show("operator", [run("r1", {})], {}, { k3j9x2m4q8w1z7v: ended });
    const retried = vi.spyOn(jobsApi, "retry").mockResolvedValue({ run: "r2" });

    fireEvent.click(
      banner("photos @ nas failed at 03:00").getByRole("button", { name: "Unlock and retry" }),
    );
    const dialog = within(
      screen.getByRole("dialog", { name: "Unlock backup-node and retry photos?" }),
    );
    expect(
      dialog.getByText(
        "restic unlock removes only locks no restic holds any more: those nobody refreshed for 30 minutes. A running restic's lock stays, whatever this page shows. Then photos runs again; for a lock younger than 30 minutes, PocketCloud waits until it is stale.",
      ),
    ).toBeInTheDocument();
    const fact = (label: string) => dialog.getByText(label).nextElementSibling;
    expect(fact("Held by")).toHaveTextContent("Check the integrity of b2 (pve-01)");
    expect(fact("Host")).toHaveTextContent("pc-k3j9x2m4q8w1z7v");
    expect(fact("PID")).toHaveTextContent("4121");
    expect(fact("Last refreshed")).toHaveTextContent(/^02:58 \(10 hr\. ago\)$/);
    expect(fact("Lock")).toHaveTextContent("exclusive");
    expect(
      dialog.getByText("The agent of nas runs restic unlock, then the backup."),
    ).toBeInTheDocument();
    expect(retried).not.toHaveBeenCalled();

    // the primary confirmation, not the danger one: unlock removes no live lock
    const confirm = dialog.getByRole("button", { name: "Unlock and retry" });
    expect(confirm).toHaveClass("primary");
    fireEvent.click(confirm);
    expect(retried).toHaveBeenCalledWith("r1");
    expect(await screen.findByText("photos runs again to backup-node.")).toBeInTheDocument();
    expect(
      screen.queryByRole("dialog", { name: "Unlock backup-node and retry photos?" }),
    ).not.toBeInTheDocument();
  });

  it("schedules the retry for when a young lock is stale", async () => {
    const young = { ...checkLock, created: "2026-11-09 11:50:00.000Z" };
    show(
      "operator",
      [run("r1", { lock: young, finished_at: "2026-11-09 11:55:00.000Z" })],
      {},
      { k3j9x2m4q8w1z7v: ended },
    );
    vi.spyOn(jobsApi, "retry").mockResolvedValue({ retry_at: "2026-11-09T12:22:00Z" });

    fireEvent.click(
      banner("photos @ nas failed at 12:55").getByRole("button", { name: "Unlock and retry" }),
    );
    const dialog = within(
      screen.getByRole("dialog", { name: "Unlock backup-node and retry photos?" }),
    );
    fireEvent.click(dialog.getByRole("button", { name: "Schedule for 13:22" }));
    expect(
      await screen.findByText("photos is tried again once restic counts the lock as stale: 13:22."),
    ).toBeInTheDocument();
  });

  it("keeps the dialog open with the panel's refusal", async () => {
    show("operator", [run("r1", {})], {}, { k3j9x2m4q8w1z7v: ended });
    const refusal =
      "The clock of nas is off by 10 minutes: restic would take a live lock for a stale one. Set its clock first.";
    let fail: (error: unknown) => void = () => {};
    vi.spyOn(jobsApi, "retry").mockReturnValue(
      new Promise((_, reject) => {
        fail = reject;
      }),
    );

    fireEvent.click(
      banner("photos @ nas failed at 03:00").getByRole("button", { name: "Unlock and retry" }),
    );
    const dialog = within(
      screen.getByRole("dialog", { name: "Unlock backup-node and retry photos?" }),
    );
    fireEvent.click(dialog.getByRole("button", { name: "Unlock and retry" }));
    expect(dialog.getByRole("button", { name: "Unlocking…" })).toBeDisabled();

    fail(new ClientResponseError({ status: 409, response: { message: refusal } }));
    expect(await dialog.findByText(refusal)).toBeInTheDocument();
    expect(dialog.getByRole("button", { name: "Unlock and retry" })).toBeEnabled();
    expect(
      screen.getByRole("dialog", { name: "Unlock backup-node and retry photos?" }),
    ).toBeInTheDocument();
  });

  it("retries a run that failed for another reason now, and says why it was not", async () => {
    const failed = run("r1", {
      failure: "restic",
      lock: null,
      attempt: 4,
      error: "Fatal: unable to save snapshot: connection reset by peer",
    });
    show("operator", [failed]);
    const retried = vi.spyOn(jobsApi, "retry").mockResolvedValue({ run: "r2" });

    const issue = banner("photos @ nas failed at 03:00");
    expect(issue.getByRole("heading", { level: 2 }).closest(".issue-banner")).toHaveClass("err");
    expect(issue.getByText("attempt 4 of 4 · no more automatic retries")).toBeInTheDocument();
    expect(issue.queryByRole("button", { name: "Unlock and retry" })).not.toBeInTheDocument();
    fireEvent.click(issue.getByRole("button", { name: "Retry now" }));
    expect(retried).toHaveBeenCalledWith("r1");
    expect(await screen.findByText("photos runs again to backup-node.")).toBeInTheDocument();
  });

  it("says why a retry did not start", async () => {
    show("operator", [run("r1", { failure: "password", lock: null })]);
    vi.spyOn(jobsApi, "retry").mockRejectedValue(
      new ClientResponseError({
        status: 409,
        response: { message: "Its previous run is still going." },
      }),
    );
    const issue = banner("photos @ nas failed at 03:00");
    expect(
      issue.getByText("attempt 1 of 4 · not retried automatically: it does not go away by itself"),
    ).toBeInTheDocument();
    fireEvent.click(issue.getByRole("button", { name: "Retry now" }));
    expect(
      await screen.findByText("The retry did not start: Its previous run is still going."),
    ).toBeInTheDocument();
  });

  it("says the next try, or the next scheduled run that takes its place", () => {
    show("operator", [
      run("r1", {
        failure: "restic",
        lock: null,
        retry_at: "2026-11-09 02:35:00.000Z",
        error: "Fatal: x",
      }),
      run("r2", {
        job: "j1",
        target: "t2",
        failure: "interrupted",
        lock: null,
        attempt: 2,
        created: "2026-11-09 01:00:00.000Z",
        finished_at: "2026-11-09 01:00:05.000Z",
        error: "The agent restarted while running the backup.",
        retry_end: "next_run",
        retry_note: "The next scheduled run at 13:00 UTC takes its place.",
      }),
    ]);
    const planned = banner("photos @ nas failed at 03:00");
    expect(planned.getByRole("heading", { level: 2 }).closest(".issue-banner")).toHaveClass("warn");
    expect(planned.getByText("attempt 1 of 4 · next try: 03:35")).toBeInTheDocument();
    expect(planned.getByRole("button", { name: "Retry now" })).toHaveClass("warning");

    // of two targets, it says which; the job's next run tries again
    const dropped = banner("vaultwarden @ nas failed at 02:00");
    expect(
      dropped.getByText(
        "to Panel disk · attempt 2 of 4 · the next scheduled run tries again: 14:00",
      ),
    ).toBeInTheDocument();
    expect(dropped.getByRole("button", { name: "Retry now" })).toBeInTheDocument();
  });

  it("says in red that no more retries come once the panel gave the retry up", () => {
    // the check holding the lock still runs after 12 hours
    show(
      "operator",
      [
        run("r1", {
          retry_end: "given_up",
          retry_note: "The repository stayed locked by Check the integrity of b2 for 12 hours.",
        }),
      ],
      {},
      { k3j9x2m4q8w1z7v: running },
    );
    const failed = banner("photos @ nas failed at 03:00");
    expect(failed.getByRole("heading", { level: 2 }).closest(".issue-banner")).toHaveClass("err");
    expect(
      failed.getByText("Held by “Check the integrity of b2” on pve-01, which is still running."),
    ).toBeInTheDocument();
    expect(failed.getByText("attempt 1 of 4 · no more automatic retries")).toBeInTheDocument();
    const unlock = failed.getByRole("button", { name: "Unlock and retry" });
    expect(unlock).toBeDisabled();
    expect(unlock).toHaveClass("critical");
  });

  it("promises no retry on the held button when none is planned", async () => {
    // the last attempt failed while the check holding the lock still runs
    show("operator", [run("r1", { attempt: 4 })], {}, { k3j9x2m4q8w1z7v: running });
    const held = () =>
      screen
        .getByRole("region", { name: /^photos @ nas / })
        .querySelector("button[disabled]") as HTMLElement;
    expect(held()).toHaveAttribute(
      "title",
      "A restic that is still running holds the lock: try again once it ends.",
    );
    cleanup();

    await i18n.changeLanguage("cs");
    show("operator", [run("r1", { attempt: 4 })], {}, { k3j9x2m4q8w1z7v: running });
    expect(held()).toHaveAccessibleName("Odemknout a zopakovat");
    expect(held()).toHaveAttribute(
      "title",
      "Zámek drží restic, který ještě běží: zkuste to znovu, až skončí.",
    );
  });

  it("offers nothing for an unreadable lock, which a person removes", () => {
    show("operator", [
      run("r1", {
        failure: "lock_invalid",
        lock: null,
        error: "Fatal: unable to create lock in backend: invalid lock file 1a2b",
      }),
    ]);
    const issue = banner("photos @ nas failed at 03:00");
    expect(
      issue.getByText(
        "An unreadable lock file blocks the repository, and restic unlock leaves it: once no restic uses the repository, delete that lock file from the storage, then retry.",
      ),
    ).toBeInTheDocument();
    expect(issue.queryByRole("button", { name: /Retry now|Unlock and retry/ })).toBeNull();
    expect(issue.getByRole("button", { name: "Show log" })).toBeInTheDocument();
  });

  it("speaks of the failed run while its retry is under way", () => {
    show("operator", [
      run("r1", { failure: "restic", lock: null, error: "Fatal: x" }),
      run("r2", {
        status: "running",
        trigger: "retry",
        attempt: 2,
        retry_of: "r1",
        created: "2026-11-09 02:05:00.000Z",
        started_at: "2026-11-09 02:05:01.000Z",
        finished_at: "",
        error: "",
        failure: "",
        lock: null,
        command: "cmd-r2",
      }),
    ]);
    const issue = banner("photos @ nas failed at 03:00");
    expect(issue.getByRole("heading", { level: 2 }).closest(".issue-banner")).toHaveClass("info");
    expect(issue.getByText("x")).toBeInTheDocument();
    expect(issue.getByText("attempt 2 of 4 · Trying again…")).toBeInTheDocument();
    expect(issue.queryByRole("button", { name: /Retry now|Unlock and retry/ })).toBeNull();
    // its log is the retry's, live
    fireEvent.click(issue.getByRole("button", { name: "Show log" }));
    expect(screen.getByRole("region", { name: "Command output" })).toHaveAttribute(
      "data-command",
      "cmd-r2",
    );
  });

  it("keeps a failure and its Unlock and retry when the next slot skipped while it waited", () => {
    // the hourly 01:00 run waited on the lock past 02:00, whose run skipped,
    // and failed at 02:00:30: 03:00 in Prague
    show(
      "operator",
      [
        run("r1", {
          job: "j1",
          target: "t1",
          created: "2026-11-09 01:00:00.000Z",
          scheduled_at: "2026-11-09 01:00:00.000Z",
          started_at: "2026-11-09 01:00:01.000Z",
          finished_at: "2026-11-09 02:00:30.000Z",
          retry_at: "2026-11-09 02:35:00.000Z",
        }),
        run("r2", {
          job: "j1",
          target: "t1",
          status: "skipped",
          created: "2026-11-09 02:00:00.000Z",
          scheduled_at: "2026-11-09 02:00:00.000Z",
          started_at: "",
          finished_at: "",
          error: "The run before it was still going.",
          failure: "",
          lock: null,
          command: "",
        }),
      ],
      {},
      { k3j9x2m4q8w1z7v: ended },
    );
    const failed = banner("vaultwarden @ nas failed at 03:00");
    expect(
      failed.getByText("to Backblaze B2 · attempt 1 of 4 · next try: 03:35"),
    ).toBeInTheDocument();
    expect(failed.getByRole("button", { name: "Unlock and retry" })).toBeEnabled();
  });

  it("leaves once a run succeeds", () => {
    show("operator", [
      run("r1", { failure: "restic", lock: null }),
      run("r2", {
        status: "succeeded",
        created: "2026-11-09 03:00:00.000Z",
        error: "",
        failure: "",
        lock: null,
      }),
    ]);
    expect(screen.queryByRole("region", { name: "Backup problems" })).not.toBeInTheDocument();
  });

  it("shows the log of a run under the problems", () => {
    show("viewer", [run("r1", {})], {}, { k3j9x2m4q8w1z7v: ended });
    fireEvent.click(
      banner("photos @ nas failed at 03:00").getByRole("button", { name: "Show log" }),
    );
    const output = screen.getByRole("region", { name: "Command output" });
    expect(output).toHaveAttribute("data-command", "cmd-r1");
    expect(within(output).getByText("Log of photos → backup-node")).toBeInTheDocument();
  });

  it("shows a viewer the log, but no retry", () => {
    show(
      "viewer",
      [run("r1", {}), run("r2", { job: "j1", target: "t1", failure: "restic", lock: null })],
      {},
      { k3j9x2m4q8w1z7v: ended },
    );
    expect(problems().queryByRole("button", { name: "Unlock and retry" })).not.toBeInTheDocument();
    expect(problems().queryByRole("button", { name: "Retry now" })).not.toBeInTheDocument();
    expect(problems().getAllByRole("button", { name: "Show log" })).toHaveLength(2);
  });

  it("puts a target whose check found errors first, with its details", () => {
    show("viewer", [run("r1", { failure: "restic", lock: null })], {
      backup_targets: [
        target({
          integrity_status: "failed",
          integrity_error: "\npack 1b2c3d: does not exist\npack 3d4e5f: does not exist",
        }),
        ...targets.slice(1),
      ],
    });
    const banners = problems().getAllByRole("heading", { level: 2 });
    expect(banners.map((heading) => heading.textContent)).toEqual([
      "The integrity check of Backblaze B2 found errors",
      "photos @ nas failed at 03:00",
    ]);
    const integrity = banner("The integrity check of Backblaze B2 found errors");
    expect(integrity.getByText("pack 1b2c3d: does not exist")).toHaveClass("mono");
    fireEvent.click(integrity.getByRole("button", { name: "Show details" }));
    expect(screen.getByRole("dialog", { name: "Integrity of Backblaze B2" })).toBeInTheDocument();
  });

  it("shows three problems, then the rest on request", async () => {
    const many = [
      run("r1", { failure: "restic", lock: null }),
      run("r2", { job: "j1", target: "t1", failure: "restic", lock: null }),
      run("r3", { job: "j1", target: "t2", failure: "restic", lock: null }),
    ];
    const failedChecks = [
      target({ integrity_status: "failed", integrity_error: "pack 1: does not exist" }),
      target({
        id: "t2",
        name: "Panel disk",
        integrity_status: "failed",
        integrity_error: "pack 2: does not exist",
      }),
      targets[2],
    ];
    show("operator", many, { backup_targets: failedChecks });

    expect(problems().getAllByRole("heading", { level: 2 })).toHaveLength(3);
    const more = problems().getByRole("button", { name: "Show 2 more problems" });
    expect(more).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(more);
    expect(problems().getAllByRole("heading", { level: 2 })).toHaveLength(5);
    fireEvent.click(problems().getByRole("button", { name: "Show fewer" }));
    expect(problems().getAllByRole("heading", { level: 2 })).toHaveLength(3);
    cleanup();

    await i18n.changeLanguage("cs");
    show("operator", many, { backup_targets: failedChecks });
    expect(
      within(screen.getByRole("region", { name: "Problémy se zálohami" })).getByRole("button", {
        name: "Zobrazit 2 další problémy",
      }),
    ).toBeVisible();
  });
});
