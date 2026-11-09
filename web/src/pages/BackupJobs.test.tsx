import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ClientResponseError } from "pocketbase";
import { MemoryRouter } from "react-router-dom";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { ToastProvider } from "../components/toast";
import i18n from "../i18n";
import { AuthProvider } from "../lib/auth";
import { jobsApi, type BackupJob, type BackupRun } from "../lib/backupJobs";
import type { BackupTarget } from "../lib/backups";
import { pb, type User } from "../lib/pb";
import type { Container, Host, Volume } from "../lib/types";
import { BackupsPage } from "./Backups";
import { describeSource, JobDialog, type StackName } from "./BackupJobs";

// Monday 13:00 in Prague, on winter time: an hour ahead of UTC
const now = Date.parse("2026-11-09T12:00:00Z");

const live = vi.hoisted(() => ({ lists: {} as Record<string, unknown[]> }));
vi.mock("../lib/realtime", () => {
  const list = (collection: string) => {
    const items = live.lists[collection] ?? [];
    return { items, loading: false, error: null, total: items.length, reload: () => {} };
  };
  return { useLiveList: list, useLiveRecords: list, useNow: () => now };
});

const zone = process.env.TZ;

beforeAll(() => {
  // the schedules, times and days are the browser's, here Prague's
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

afterEach(() => {
  vi.restoreAllMocks();
  pb.authStore.clear();
  live.lists = {};
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

function wrap(children: React.ReactNode) {
  return render(
    <MemoryRouter>
      <AuthProvider>
        <ToastProvider>{children}</ToastProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

const hosts = [
  { id: "h1", name: "pve-01", status: "online" },
  { id: "h2", name: "nas", status: "offline" },
] as Host[];

function target(fields: Partial<BackupTarget>): BackupTarget {
  return {
    id: "t1",
    collectionId: "backup_targets",
    collectionName: "backup_targets",
    name: "Backblaze B2",
    kind: "s3",
    host: "h1",
    config: null,
    location: "s3:homelab-backups",
    repo_id: "",
    status: "ok",
    last_error: "",
    checked_at: "2026-11-09 11:55:00.000Z",
    usage_at: "2026-11-09 11:56:00.000Z",
    used_bytes: 1.2e12,
    capacity_bytes: 0,
    immutable_days: 0,
    note: "",
    ...fields,
  } as BackupTarget;
}

const bucket = target({});
// a directory of nas: only nas's jobs back up to it
const disk = target({
  id: "t2",
  name: "Panel disk",
  kind: "local",
  host: "h2",
  location: "/srv/backups/restic",
  used_bytes: 0.3e9,
  capacity_bytes: 20e9,
});
// not measured yet: the Stored tile leaves it out
const node = target({ id: "t3", name: "backup-node", kind: "sftp", usage_at: "", used_bytes: 0 });
const targets = [bucket, disk, node];

const stacks: StackName[] = [
  { id: "s1", name: "umami", host: "h1" },
  { id: "s2", name: "vaultwarden", host: "h2" },
];

function job(fields: Partial<BackupJob>): BackupJob {
  return {
    id: "j1",
    collectionId: "backup_jobs",
    collectionName: "backup_jobs",
    name: "vaultwarden",
    host: "h2",
    source_kind: "volume",
    source: "vw-data",
    stack: "",
    service: "",
    engine: "",
    targets: ["t1", "t2"],
    schedule: "0 * * * *",
    retention: { hourly: 24, daily: 7, weekly: 4, monthly: 6 },
    enabled: true,
    origin: "manual",
    label_removed: false,
    problem: "",
    next_run_at: "2026-11-09 13:00:00.000Z",
    last_run_at: "",
    last_status: "",
    ...fields,
  } as BackupJob;
}

const vaultwarden = job({});
const photos = job({
  id: "j2",
  name: "photos",
  source_kind: "path",
  source: "/mnt/tank/photos",
  targets: ["t3"],
  schedule: "0 2 * * *",
  retention: { last: 20 },
  next_run_at: "2026-11-10 02:00:00.000Z",
});
const umami = job({
  id: "j3",
  name: "umami-db",
  host: "h1",
  source_kind: "db_dump",
  source: "",
  stack: "s1",
  service: "db",
  engine: "postgres",
  targets: ["t3"],
  schedule: "30 0 * * *",
  retention: { daily: 14, weekly: 8 },
  origin: "label",
  next_run_at: "2026-11-10 00:30:00.000Z",
});
// a service's label names its own volumes, as the panel saves them
const gone = job({
  id: "j4",
  name: "old-db",
  host: "h1",
  source_kind: "stack",
  source: "umami_db-data,umami_db-backups",
  stack: "s1",
  service: "db",
  targets: ["t1"],
  origin: "label",
  label_removed: true,
  enabled: false,
  next_run_at: "",
  last_status: "succeeded",
  last_run_at: "2026-10-01 02:00:00.000Z",
});
const unplaced = job({
  id: "j5",
  name: "umami-cache",
  host: "h1",
  source_kind: "stack",
  source: "umami_cache-data",
  stack: "s1",
  service: "cache",
  targets: [],
  origin: "label",
  problem: "Choose where it goes: no target.",
  enabled: false,
  next_run_at: "",
});
const jobs = [vaultwarden, photos, umami, gone, unplaced];

function run(id: string, fields: Partial<BackupRun>): BackupRun {
  return {
    id,
    collectionId: "backup_runs",
    collectionName: "backup_runs",
    created: "",
    updated: "",
    job: "j1",
    target: "t1",
    host: "h2",
    trigger: "schedule",
    status: "succeeded",
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
    error: "",
    ...fields,
  } as BackupRun;
}

const locked =
  "Fatal: unable to create lock in backend: repository is already locked by PID 4121 on nas\nthe lock was made at 02:58";

const runs = [
  run("r1", {
    created: "2026-11-09 11:00:00.000Z",
    finished_at: "2026-11-09 11:00:40.000Z",
    bytes_added: 84e6,
  }),
  run("r2", {
    target: "t2",
    created: "2026-11-09 11:00:00.000Z",
    finished_at: "2026-11-09 11:00:30.000Z",
    bytes_added: 80e6,
  }),
  run("r3", {
    target: "t2",
    status: "skipped",
    created: "2026-11-08 02:00:00.000Z",
    error: "The server nas was offline at 02:00 UTC.",
  }),
  run("r4", { created: "2026-11-08 02:00:00.000Z" }),
  run("r5", {
    job: "j2",
    target: "t3",
    status: "failed",
    created: "2026-11-09 02:00:00.000Z",
    finished_at: "2026-11-09 02:00:05.000Z",
    error: locked,
  }),
  run("r6", { job: "j2", target: "t3", created: "2026-11-08 02:00:00.000Z" }),
  // before the 14 days
  run("r7", { status: "failed", created: "2026-10-20 02:00:00.000Z" }),
];

function showPage(role: User["role"], fields: Record<string, unknown[]> = {}) {
  signInAs(role);
  live.lists = {
    backup_targets: targets,
    hosts,
    backup_jobs: jobs,
    backup_runs: runs,
    stacks,
    ...fields,
  };
  wrap(<BackupsPage />);
}

function row(name: string) {
  const cell = screen.getAllByText(name).find((element) => element.className === "job-name");
  return within(cell!.closest("tr")!);
}

function tile(label: string) {
  return within(screen.getByText(label).closest(".kpi") as HTMLElement);
}

describe("the backups summary", () => {
  it("says how the last 14 days went, what the targets hold and what runs next", () => {
    // the zone took: noon UTC is 13:00 in Prague
    expect(new Date(now).getHours()).toBe(13);
    showPage("viewer");

    expect(screen.getByRole("region", { name: "Backup summary" })).toBeInTheDocument();
    // 4 succeeded, 1 failed and 1 skipped since 27 October
    const success = tile("Success over 14 days");
    expect(success.getByText("66%")).toBeInTheDocument();
    expect(success.getByText("4 of 6 runs")).toBeInTheDocument();

    const stored = tile("Stored");
    expect(stored.getByText("1.2 TB")).toBeInTheDocument();
    expect(stored.getByText("in 2 targets")).toBeInTheDocument();

    const next = tile("Next run");
    expect(next.getByText("14:00")).toBeInTheDocument();
    expect(next.getByText("vaultwarden · nas")).toBeInTheDocument();
  });

  it("says there was nothing yet", () => {
    showPage("viewer", { backup_jobs: [], backup_runs: [], backup_targets: [node] });
    expect(tile("Success over 14 days").getByText("no runs yet")).toBeInTheDocument();
    expect(tile("Stored").getByText("the targets are not measured yet")).toBeInTheDocument();
    expect(tile("Next run").getByText("no job is scheduled")).toBeInTheDocument();
    expect(screen.getByText("No backup jobs yet")).toBeInTheDocument();
  });
});

describe("a job's source", () => {
  const t = i18n.getFixedT("en");
  const cs = i18n.getFixedT("cs");

  it("is every volume of a stack, when no service names its own", () => {
    const whole = job({ source_kind: "stack", source: "", stack: "s1", service: "" });
    expect(describeSource(whole, stacks, t)).toBe("volumes of stack umami");
    expect(describeSource(whole, stacks, cs)).toBe("svazky stacku umami");
  });

  it("is the volumes a service's label names, of that service only", () => {
    const label = job({
      source_kind: "stack",
      source: "shop_app-data,shop_uploads",
      stack: "s1",
      service: "app",
      origin: "label",
    });
    expect(describeSource(label, stacks, t)).toBe(
      "volumes of service app: shop_app-data, shop_uploads · from label pocketcloud.backup",
    );
    expect(describeSource(label, stacks, cs)).toBe(
      "svazky služby app: shop_app-data, shop_uploads · z labelu pocketcloud.backup",
    );
  });
});

describe("the jobs", () => {
  it("lists what each backs up, where to, when, how long and how it went", () => {
    showPage("viewer");

    const vw = row("vaultwarden");
    expect(vw.getByText("volume vw-data")).toBeInTheDocument();
    expect(vw.getByText("nas")).toHaveClass("mono");
    expect(vw.getByText(/Backblaze B2 \+ Panel disk/)).toBeInTheDocument();
    expect(vw.getByText("hourly")).toBeInTheDocument();
    expect(vw.getByText("24h 7d 4w 6m")).toBeInTheDocument();
    // the newest run of each target: both succeeded, at 12:00 in Prague
    expect(vw.getByText("OK · 12:00")).toHaveClass("run-status", "ok");
    expect(vw.getByText("+84 MB")).toBeInTheDocument();

    const photosRow = row("photos");
    expect(photosRow.getByText("/mnt/tank/photos")).toBeInTheDocument();
    expect(photosRow.getByText("daily 03:00")).toBeInTheDocument();
    expect(photosRow.getByText("last 20")).toBeInTheDocument();
    expect(photosRow.getByText("Error · 03:00")).toHaveClass("err");
    const error = photosRow.getByText(
      "Fatal: unable to create lock in backend: repository is already locked by PID 4121 on nas",
    );
    expect(error).toHaveAttribute("title", locked);

    const label = row("umami-db");
    expect(
      label.getByText("pg_dump · umami/db · from label pocketcloud.backup"),
    ).toBeInTheDocument();
    expect(label.getByText("daily 01:30")).toBeInTheDocument();
    expect(label.getByText("14d 8w")).toBeInTheDocument();
    expect(label.getByText("not run yet")).toBeInTheDocument();

    // older than the runs loaded: what the job says
    expect(row("old-db").getByText(/^OK · /)).toBeInTheDocument();
    // its note says why it does not run
    expect(row("old-db").queryByText(/paused/)).not.toBeInTheDocument();

    // a label's volumes are its service's, not every volume of the stack
    expect(
      row("old-db").getByText(
        "volumes of service db: umami_db-data, umami_db-backups · from label pocketcloud.backup",
      ),
    ).toBeInTheDocument();

    const unplacedRow = row("umami-cache");
    expect(
      unplacedRow.getByText(
        "volumes of service cache: umami_cache-data · from label pocketcloud.backup",
      ),
    ).toBeInTheDocument();
    expect(unplacedRow.getByText("· paused")).toBeInTheDocument();
    expect(unplacedRow.getByText("no target")).toBeInTheDocument();
    expect(unplacedRow.getByText("Choose where it goes: no target.")).toBeInTheDocument();
  });

  it("names the target a run that failed was about, of several", () => {
    showPage("viewer", {
      backup_runs: [
        run("r1", { created: "2026-11-09 11:00:00.000Z", bytes_added: 84e6 }),
        run("r2", {
          target: "t2",
          status: "failed",
          created: "2026-11-09 11:00:00.000Z",
          finished_at: "2026-11-09 11:00:10.000Z",
          error: "Fatal: no space left on device",
        }),
      ],
    });
    const vw = row("vaultwarden");
    expect(vw.getByText("Error · 12:00")).toBeInTheDocument();
    expect(vw.getByText("Panel disk: Fatal: no space left on device")).toBeInTheDocument();
    expect(vw.queryByText("+84 MB")).not.toBeInTheDocument();
  });

  it("draws 14 local days, each the colour of its worst run", () => {
    showPage("viewer");

    const strip = row("vaultwarden").getByRole("img");
    expect(strip).toHaveAccessibleName("Last 14 days: 3 succeeded, 1 skipped");
    const days = strip.querySelectorAll("span");
    expect(days).toHaveLength(14);
    expect([...days].map((day) => day.className).slice(-3)).toEqual(["off", "warn", "ok"]);
    expect(days[12].getAttribute("title")).toMatch(/: 1 succeeded, 1 skipped$/);
    expect(days[0].getAttribute("title")).toMatch(/: no run$/);

    const failed = row("photos").getByRole("img");
    expect(failed).toHaveAccessibleName("Last 14 days: 1 succeeded, 1 failed");
    expect([...failed.querySelectorAll("span")].map((day) => day.className).slice(-2)).toEqual([
      "ok",
      "err",
    ]);
    expect(row("umami-db").getByRole("img")).toHaveAccessibleName("Last 14 days: no run");

    const legend = screen.getByRole("list", { name: "What the colours of the 14 days mean" });
    expect(
      within(legend)
        .getAllByRole("listitem")
        .map((item) => item.textContent),
    ).toEqual(["success", "error", "skipped", "no run"]);
  });

  it("shows a viewer no actions", () => {
    showPage("viewer");
    expect(screen.queryByRole("button", { name: "New job" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Run .* now$/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Edit job / })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Remove job / })).not.toBeInTheDocument();
  });

  it("lets an operator run a job now, and nothing else", () => {
    showPage("operator");
    expect(screen.getByRole("button", { name: "Run vaultwarden now" })).toBeEnabled();
    expect(screen.queryByRole("button", { name: "New job" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Edit job / })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Remove job / })).not.toBeInTheDocument();
  });

  it("gives an admin every action a job allows", () => {
    showPage("admin");
    expect(screen.getByRole("button", { name: "New job" })).toBeInTheDocument();
    for (const name of ["vaultwarden", "photos"]) {
      expect(screen.getByRole("button", { name: `Edit job ${name}` })).toBeInTheDocument();
      expect(screen.getByRole("button", { name: `Remove job ${name}` })).toBeInTheDocument();
    }
    // a label's job comes back at the next deploy: its label removes it
    expect(screen.getByRole("button", { name: "Edit job umami-db" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Remove job umami-db" })).not.toBeInTheDocument();
    // one whose label is gone says so, with Remove
    const old = row("old-db");
    expect(
      old.getByText("Its pocketcloud.backup label is gone: the job no longer runs."),
    ).toBeInTheDocument();
    expect(old.getByRole("button", { name: "Remove job old-db" })).toHaveTextContent("Remove");
    expect(old.queryByRole("button", { name: "Edit job old-db" })).not.toBeInTheDocument();
    expect(old.getByRole("button", { name: "Run old-db now" })).toBeDisabled();
    // nowhere to go, nothing to run
    expect(screen.getByRole("button", { name: "Run umami-cache now" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Run umami-cache now" })).toHaveAttribute(
      "title",
      "It has no target to back up to.",
    );
  });

  it("runs a job now, to each target", async () => {
    showPage("operator");
    const send = vi.spyOn(jobsApi, "run").mockResolvedValue({ runs: ["r8", "r9"] });
    fireEvent.click(screen.getByRole("button", { name: "Run vaultwarden now" }));
    expect(send).toHaveBeenCalledWith("j1");
    expect(await screen.findByText("vaultwarden runs now, to 2 targets.")).toBeInTheDocument();
  });

  it("says why the panel did not run it", async () => {
    showPage("operator");
    const send = vi.spyOn(pb, "send").mockRejectedValue(
      new ClientResponseError({
        status: 409,
        response: { message: "Its previous runs are still going." },
      }),
    );
    fireEvent.click(screen.getByRole("button", { name: "Run photos now" }));
    expect(send).toHaveBeenCalledWith("/api/pc/backup-jobs/j2/run", { method: "POST" });
    expect(await screen.findByText("Its previous runs are still going.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Run photos now" })).toBeEnabled();
  });

  it("removes a job after saying its snapshots stay", async () => {
    showPage("admin");
    const send = vi.spyOn(pb, "send").mockResolvedValue(undefined);
    fireEvent.click(screen.getByRole("button", { name: "Remove job old-db" }));
    const dialog = screen.getByRole("dialog", { name: "Remove the job old-db?" });
    expect(
      within(dialog).getByText(/Its snapshots stay in the targets' repositories/),
    ).toBeInTheDocument();
    expect(send).not.toHaveBeenCalled();

    fireEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    await waitFor(() =>
      expect(send).toHaveBeenCalledWith("/api/pc/backup-jobs/j4", { method: "DELETE" }),
    );
    expect(await screen.findByText("The job old-db was removed.")).toBeInTheDocument();
  });

  it("opens the dialog for a new job, and to edit one", () => {
    showPage("admin");
    fireEvent.click(screen.getByRole("button", { name: "New job" }));
    expect(screen.getByRole("dialog", { name: "New backup job" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(screen.getByRole("button", { name: "Edit job photos" }));
    expect(screen.getByRole("dialog", { name: "Edit photos" })).toBeInTheDocument();
    expect(screen.getByLabelText("Directory")).toHaveValue("/mnt/tank/photos");
  });
});

function type(label: string | RegExp, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

function confirm(name: string) {
  return screen.getAllByRole("button", { name }).at(-1)!;
}

function openDialog(edited: BackupJob | null, lists: Record<string, unknown[]> = {}) {
  live.lists = lists;
  const onClose = vi.fn();
  wrap(
    <JobDialog job={edited} targets={targets} hosts={hosts} stacks={stacks} onClose={onClose} />,
  );
  return onClose;
}

const keepDefault = { last: 0, hourly: 0, daily: 7, weekly: 4, monthly: 6, yearly: 0 };

describe("a new job", () => {
  it("backs up a volume, suggested from its server, daily at 03:00 local time", async () => {
    const add = vi.spyOn(jobsApi, "add").mockResolvedValue(vaultwarden);
    const onClose = openDialog(null, {
      volumes: [{ id: "v1", host: "h1", name: "vw-data" }] as Volume[],
    });

    expect(screen.getByLabelText("What to back up")).toHaveValue("volume");
    expect(confirm("Add job")).toBeDisabled();
    type("Name", "vaultwarden");
    fireEvent.change(screen.getByLabelText("Server"), { target: { value: "h1" } });
    expect(document.querySelector("#job-volumes option")).toHaveAttribute("value", "vw-data");
    type("Volume", "vw data");
    expect(screen.getByText(/A Docker volume's name/)).toBeInTheDocument();
    expect(confirm("Add job")).toBeDisabled();
    type("Volume", "vw-data");

    // a local target of another server is no choice
    expect(screen.getByLabelText(/^Panel disk/)).toBeDisabled();
    expect(screen.getByText(/a directory of nas: only its jobs back up to it/)).toBeInTheDocument();
    fireEvent.click(screen.getByLabelText("Backblaze B2"));

    // the preset: every day at 03:00 in Prague, 02:00 in UTC
    expect(screen.getByLabelText("Schedule")).toHaveValue("daily");
    expect(screen.getByLabelText("Time")).toHaveValue("03:00");
    expect(screen.getByText(/The panel keeps it in UTC, as 0 2 \* \* \*/)).toBeInTheDocument();
    expect(screen.getByText(/Europe\/Prague/)).toBeInTheDocument();
    expect(screen.getByText(/Now: 7d 4w 6m\./)).toBeInTheDocument();
    fireEvent.click(confirm("Add job"));

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(add).toHaveBeenCalledWith({
      name: "vaultwarden",
      host: "h1",
      source_kind: "volume",
      source: "vw-data",
      targets: ["t1"],
      schedule: "0 2 * * *",
      retention: keepDefault,
      enabled: true,
    });
    expect(await screen.findByText("The job vaultwarden was added.")).toBeInTheDocument();
  });

  it("backs up a directory weekly, Monday 00:30 local being Sunday in UTC", async () => {
    const add = vi.spyOn(jobsApi, "add").mockResolvedValue(photos);
    openDialog(null);

    fireEvent.change(screen.getByLabelText("What to back up"), { target: { value: "path" } });
    type("Name", "photos");
    fireEvent.change(screen.getByLabelText("Server"), { target: { value: "h2" } });
    type("Directory", "/proc/1");
    expect(screen.getByText(/not in \/proc, \/sys or \/dev/)).toBeInTheDocument();
    type("Directory", "/mnt/tank/photos");
    // now nas's own directory may be chosen
    fireEvent.click(screen.getByLabelText("Panel disk"));
    fireEvent.click(screen.getByLabelText("backup-node"));
    fireEvent.change(screen.getByLabelText("Schedule"), { target: { value: "weekly" } });
    expect(screen.getByLabelText("Day")).toHaveValue("1");
    type("Time", "00:30");
    expect(screen.getByText(/as 30 23 \* \* 0/)).toBeInTheDocument();
    for (const keep of ["Daily", "Weekly", "Monthly"]) {
      type(keep, "");
    }
    type("Last", "20");
    expect(screen.getByText(/Now: last 20\./)).toBeInTheDocument();
    fireEvent.click(screen.getByLabelText("Enabled: it runs on its schedule"));
    fireEvent.click(confirm("Add job"));

    await waitFor(() => expect(add).toHaveBeenCalled());
    expect(add.mock.calls[0][0]).toEqual({
      name: "photos",
      host: "h2",
      source_kind: "path",
      source: "/mnt/tank/photos",
      targets: ["t2", "t3"],
      schedule: "30 23 * * 0",
      retention: { last: 20, hourly: 0, daily: 0, weekly: 0, monthly: 0, yearly: 0 },
      enabled: false,
    });
  });

  it("backs up a stack's volumes from the stack's server, hourly", async () => {
    const add = vi.spyOn(jobsApi, "add").mockResolvedValue(vaultwarden);
    openDialog(null);

    fireEvent.change(screen.getByLabelText("What to back up"), { target: { value: "stack" } });
    // the stack says the server
    expect(screen.queryByLabelText("Server")).not.toBeInTheDocument();
    expect(screen.getByLabelText(/^Panel disk/)).toBeDisabled();
    type("Name", "vaultwarden");
    fireEvent.change(screen.getByLabelText("Stack"), { target: { value: "s2" } });
    expect(screen.getByRole("option", { name: "vaultwarden · nas" })).toBeInTheDocument();
    fireEvent.click(screen.getByLabelText("Panel disk"));
    fireEvent.change(screen.getByLabelText("Schedule"), { target: { value: "hourly" } });
    type("Minute", "15");
    fireEvent.click(confirm("Add job"));

    await waitFor(() => expect(add).toHaveBeenCalled());
    expect(add.mock.calls[0][0]).toEqual({
      name: "vaultwarden",
      host: "h2",
      source_kind: "stack",
      stack: "s2",
      targets: ["t2"],
      schedule: "15 * * * *",
      retention: keepDefault,
      enabled: true,
    });
  });

  it("dumps a service's database, its engine guessed from its image", async () => {
    const add = vi.spyOn(jobsApi, "add").mockResolvedValue(umami);
    openDialog(null, {
      containers: [
        { id: "c1", stack: "s1", service: "umami", image: "ghcr.io/umami-software/umami:latest" },
        { id: "c2", stack: "s1", service: "db", image: "postgres:16-alpine" },
      ] as Container[],
    });

    fireEvent.change(screen.getByLabelText("What to back up"), { target: { value: "db_dump" } });
    type("Name", "umami-db");
    fireEvent.change(screen.getByLabelText("Stack"), { target: { value: "s1" } });
    fireEvent.change(screen.getByLabelText("Service"), { target: { value: "db" } });
    expect(screen.getByLabelText("Database")).toHaveValue("postgres");
    expect(screen.getByText(/pg_dump as POSTGRES_USER/)).toBeInTheDocument();
    fireEvent.click(screen.getByLabelText("backup-node"));
    // a cron expression of its own, in UTC
    fireEvent.change(screen.getByLabelText("Schedule"), { target: { value: "custom" } });
    expect(screen.getByLabelText("Cron expression (UTC)")).toHaveValue("0 2 * * *");
    type("Cron expression (UTC)", "0 4 * * 1-5");
    expect(screen.getByText(/In your time: 0 4 \* \* 1-5 UTC\./)).toBeInTheDocument();
    fireEvent.click(confirm("Add job"));

    await waitFor(() => expect(add).toHaveBeenCalled());
    expect(add.mock.calls[0][0]).toEqual({
      name: "umami-db",
      host: "h1",
      source_kind: "db_dump",
      stack: "s1",
      service: "db",
      engine: "postgres",
      targets: ["t3"],
      schedule: "0 4 * * 1-5",
      retention: keepDefault,
      enabled: true,
    });
  });

  it("dumps a SQLite file by its path in the container", async () => {
    const add = vi.spyOn(jobsApi, "add").mockResolvedValue(umami);
    openDialog(null);

    fireEvent.change(screen.getByLabelText("What to back up"), { target: { value: "db_dump" } });
    type("Name", "vaultwarden-db");
    fireEvent.change(screen.getByLabelText("Stack"), { target: { value: "s2" } });
    // no containers yet: the service by name
    type("Service", "vaultwarden");
    fireEvent.change(screen.getByLabelText("Database"), { target: { value: "sqlite" } });
    expect(screen.getByText(/the image needs sqlite3/)).toBeInTheDocument();
    expect(confirm("Add job")).toBeDisabled();
    type("Database file", "data/db.sqlite3");
    expect(screen.getByText(/The file's absolute path in the container/)).toBeInTheDocument();
    type("Database file", "/data/db.sqlite3");
    fireEvent.click(confirm("Add job"));

    await waitFor(() => expect(add).toHaveBeenCalled());
    expect(add.mock.calls[0][0]).toMatchObject({
      host: "h2",
      source_kind: "db_dump",
      source: "/data/db.sqlite3",
      stack: "s2",
      service: "vaultwarden",
      engine: "sqlite",
      targets: [],
    });
  });

  it("holds back numbers restic does not keep", () => {
    openDialog(null);
    type("Name", "photos");
    fireEvent.change(screen.getByLabelText("Server"), { target: { value: "h1" } });
    type("Volume", "photos");
    expect(confirm("Add job")).toBeEnabled();
    type("Daily", "1.5");
    expect(screen.getByText(/Whole numbers from 0 to 10000/)).toBeInTheDocument();
    expect(confirm("Add job")).toBeDisabled();
  });

  it("shows why the panel refused, and stays open", async () => {
    vi.spyOn(jobsApi, "add").mockRejectedValue(
      new ClientResponseError({ status: 400, response: { message: "The name is taken." } }),
    );
    const onClose = openDialog(null);
    type("Name", "photos");
    fireEvent.change(screen.getByLabelText("Server"), { target: { value: "h1" } });
    type("Volume", "photos");
    fireEvent.click(confirm("Add job"));

    expect(await screen.findByText("The name is taken.")).toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
    expect(confirm("Add job")).toBeEnabled();
    expect(screen.getByLabelText("Volume")).toHaveValue("photos");
  });
});

describe("changing a job", () => {
  it("shows the job's schedule in local time, and sends only what changed", async () => {
    const change = vi.spyOn(jobsApi, "change").mockResolvedValue(photos);
    const onClose = openDialog(photos);

    expect(screen.getByLabelText("What to back up")).toHaveValue("path");
    expect(screen.getByLabelText("Server")).toHaveValue("h2");
    expect(screen.getByLabelText("Schedule")).toHaveValue("daily");
    expect(screen.getByLabelText("Time")).toHaveValue("03:00");
    expect(screen.getByLabelText("Last")).toHaveValue(20);
    // nothing changed yet
    expect(confirm("Save")).toBeDisabled();

    fireEvent.click(screen.getByLabelText("Backblaze B2"));
    fireEvent.click(confirm("Save"));

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(change).toHaveBeenCalledWith("j2", { targets: ["t1", "t3"] });
    expect(await screen.findByText("The job photos was saved.")).toBeInTheDocument();
  });

  it("keeps a schedule it does not write as it is, until it is changed", async () => {
    const change = vi.spyOn(jobsApi, "change").mockResolvedValue(photos);
    openDialog(job({ ...photos, schedule: "@daily" }));

    // @daily is 01:00 in Prague: the same, written otherwise, is no change
    expect(screen.getByLabelText("Time")).toHaveValue("01:00");
    expect(confirm("Save")).toBeDisabled();
    type("Time", "04:00");
    fireEvent.click(confirm("Save"));
    await waitFor(() => expect(change).toHaveBeenCalledWith("j2", { schedule: "0 3 * * *" }));
  });

  it("sends a new kind with every field of its source", async () => {
    const change = vi.spyOn(jobsApi, "change").mockResolvedValue(photos);
    openDialog(photos);
    fireEvent.change(screen.getByLabelText("What to back up"), { target: { value: "stack" } });
    fireEvent.change(screen.getByLabelText("Stack"), { target: { value: "s2" } });
    fireEvent.click(confirm("Save"));
    await waitFor(() => expect(change).toHaveBeenCalled());
    expect(change.mock.calls[0]).toEqual([
      "j2",
      {
        host: "h2",
        source_kind: "stack",
        source: "",
        stack: "s2",
        service: "",
        engine: "",
      },
    ]);
  });

  it("changes only what a label's job lets change, and never its source", async () => {
    const change = vi.spyOn(jobsApi, "change").mockResolvedValue(umami);
    openDialog(umami);

    expect(screen.queryByLabelText("What to back up")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Stack")).not.toBeInTheDocument();
    expect(
      screen.getByText("pg_dump · umami/db · from label pocketcloud.backup"),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/pocketcloud.backup.schedule, .target and .retention/),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Time")).toHaveValue("01:30");

    fireEvent.click(screen.getByLabelText("Backblaze B2"));
    type("Weekly", "4");
    fireEvent.click(confirm("Save"));
    await waitFor(() => expect(change).toHaveBeenCalled());
    expect(change.mock.calls[0]).toEqual([
      "j3",
      {
        targets: ["t1", "t3"],
        retention: { last: 0, hourly: 0, daily: 14, weekly: 4, monthly: 0, yearly: 0 },
      },
    ]);
  });

  it("names the volumes a label's job backs up", () => {
    openDialog(unplaced);
    expect(screen.queryByLabelText("What to back up")).not.toBeInTheDocument();
    expect(
      screen.getByText(
        "volumes of service cache: umami_cache-data · from label pocketcloud.backup",
      ),
    ).toBeInTheDocument();
  });

  it("shows what a label's labels set read-only, and never sends it", async () => {
    const change = vi.spyOn(jobsApi, "change").mockResolvedValue(umami);
    openDialog(job({ ...umami, label_sets: ["schedule", "retention"] }));

    // as the labels say, in local time
    expect(screen.getByLabelText("Schedule")).toBeDisabled();
    expect(screen.getByLabelText("Time")).toBeDisabled();
    expect(screen.getByLabelText("Time")).toHaveValue("01:30");
    expect(
      screen.getByText(
        "Set by the label pocketcloud.backup.schedule; it changes in the Compose file.",
      ),
    ).toBeInTheDocument();
    for (const keep of ["Last", "Hourly", "Daily", "Weekly", "Monthly", "Yearly"]) {
      expect(screen.getByLabelText(keep)).toBeDisabled();
    }
    expect(screen.getByLabelText("Daily")).toHaveValue(14);
    expect(
      screen.getByText(
        "Set by the label pocketcloud.backup.retention; it changes in the Compose file.",
      ),
    ).toBeInTheDocument();

    // the targets are the admin's
    expect(screen.getByLabelText("Backblaze B2")).toBeEnabled();
    expect(screen.queryByText(/pocketcloud\.backup\.target;/)).not.toBeInTheDocument();
    expect(confirm("Save")).toBeDisabled();
    fireEvent.click(screen.getByLabelText("Backblaze B2"));
    fireEvent.click(confirm("Save"));
    await waitFor(() => expect(change).toHaveBeenCalled());
    expect(change.mock.calls[0]).toEqual(["j3", { targets: ["t1", "t3"] }]);
  });

  it("never sends the targets a label sets, though one no longer fits", async () => {
    const change = vi.spyOn(jobsApi, "change").mockResolvedValue(umami);
    // Panel disk is a directory of nas; umami-db backs up pve-01
    openDialog(job({ ...umami, targets: ["t2", "t3"], label_sets: ["targets"] }));

    expect(screen.getByLabelText("Backblaze B2")).toBeDisabled();
    expect(screen.getByLabelText("backup-node")).toBeDisabled();
    expect(screen.getByLabelText("backup-node")).toBeChecked();
    expect(
      screen.getByText(
        "Set by the label pocketcloud.backup.target; they change in the Compose file.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Time")).toBeEnabled();
    expect(screen.getByLabelText("Daily")).toBeEnabled();
    // what the dialog cannot show of them is no change
    expect(confirm("Save")).toBeDisabled();

    type("Name", "umami-postgres");
    fireEvent.click(confirm("Save"));
    await waitFor(() => expect(change).toHaveBeenCalled());
    expect(change.mock.calls[0]).toEqual(["j3", { name: "umami-postgres" }]);
  });

  it.each([{ sets: [] }, { sets: null }])(
    "lets a label's job whose labels set $sets change all but its source",
    async ({ sets }) => {
      const change = vi.spyOn(jobsApi, "change").mockResolvedValue(umami);
      openDialog(job({ ...umami, label_sets: sets }));

      for (const label of ["Schedule", "Time", "Backblaze B2", "Daily", "Weekly"]) {
        expect(screen.getByLabelText(label)).toBeEnabled();
      }
      expect(screen.queryByText(/change in the Compose file\.$/)).not.toBeInTheDocument();

      fireEvent.click(screen.getByLabelText("Backblaze B2"));
      type("Time", "02:00");
      type("Weekly", "4");
      fireEvent.click(confirm("Save"));
      await waitFor(() => expect(change).toHaveBeenCalled());
      expect(change.mock.calls[0]).toEqual([
        "j3",
        {
          targets: ["t1", "t3"],
          schedule: "0 1 * * *",
          retention: { last: 0, hourly: 0, daily: 14, weekly: 4, monthly: 0, yearly: 0 },
        },
      ]);
    },
  );

  it("closes nothing when its dialog was closed before the save ended", async () => {
    let done: (job: BackupJob) => void = () => {};
    vi.spyOn(jobsApi, "change").mockReturnValue(
      new Promise((resolve) => {
        done = resolve;
      }),
    );
    const onClose = vi.fn();
    const dialog = (
      <JobDialog job={photos} targets={targets} hosts={hosts} stacks={stacks} onClose={onClose} />
    );
    const view = wrap(dialog);
    type("Name", "pictures");
    fireEvent.click(confirm("Save"));
    expect(await screen.findByRole("button", { name: "Saving…" })).toBeDisabled();
    view.rerender(
      <MemoryRouter>
        <AuthProvider>
          <ToastProvider>{null}</ToastProvider>
        </AuthProvider>
      </MemoryRouter>,
    );

    done({ ...photos, name: "pictures" });
    expect(await screen.findByText("The job pictures was saved.")).toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
  });

  it("shows the panel's refusal of what a label sets", async () => {
    vi.spyOn(jobsApi, "change").mockRejectedValue(
      new ClientResponseError({
        status: 400,
        response: { message: "The label pocketcloud.backup.schedule sets the schedule." },
      }),
    );
    const onClose = openDialog(umami);
    type("Time", "02:00");
    fireEvent.click(confirm("Save"));
    expect(
      await screen.findByText("The label pocketcloud.backup.schedule sets the schedule."),
    ).toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
  });
});
