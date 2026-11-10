import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { ClientResponseError } from "pocketbase";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { restoresApi } from "../lib/backupRestores";
import { pb, type User } from "../lib/pb";
import { BackupsPage } from "./Backups";
import {
  bucket,
  hosts,
  jobs,
  now,
  photos,
  restore,
  run,
  signInAs,
  snapshot,
  snapshots,
  targets,
  vaultwarden,
  wrap,
} from "./BackupRestore.fixtures";
import { RecentRestores, SnapshotsCard } from "./BackupSnapshots";

type ListOptions = { enabled?: boolean; filter?: string; sort?: string; perPage?: number };

const live = vi.hoisted(() => ({
  lists: {} as Record<string, unknown[]>,
  // each list a collection was asked for, with its options
  options: {} as Record<string, ListOptions[]>,
}));
vi.mock("../lib/realtime", async () => {
  const { listPage } = await import("../test-filter");
  const list = (collection: string, options: ListOptions = {}) => {
    (live.options[collection] ??= []).push(options);
    let items = options.enabled === false ? [] : (live.lists[collection] ?? []);
    if (collection === "backup_restores") {
      // the restores are listed as the panel lists them: a page of what the filter matches
      items = listPage(items as Record<string, unknown>[], options);
    }
    return { items, loading: false, error: null, total: items.length, reload: () => {} };
  };
  return {
    useLiveList: list,
    useLiveRecords: list,
    // the drawer a restore's Show opens
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

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  pb.authStore.clear();
  live.lists = {};
  live.options = {};
});

function showPage(role: User["role"], path = "/backups", lists: Record<string, unknown[]> = {}) {
  signInAs(role);
  live.lists = {
    backup_targets: targets,
    hosts,
    backup_jobs: jobs,
    backup_runs: [run("r1", {}), run("r2", { target: "t3", created: "2026-11-01 02:00:00.000Z" })],
    stacks: [],
    ...lists,
  };
  return wrap(<BackupsPage />, path);
}

function card() {
  return within(screen.getByRole("heading", { name: /^Snapshots · / }).closest("section")!);
}

function location() {
  return screen.getByTestId("location").textContent;
}

function failure(status: number, message: string) {
  return new ClientResponseError({ status, response: { message }, url: "", isAbort: false });
}

describe("the snapshots card", () => {
  it("lists the snapshots of the selected job and target", async () => {
    const list = vi.spyOn(restoresApi, "snapshots").mockResolvedValue(snapshots(3));
    showPage("viewer");

    // the first job is picked until one is
    const pressed = screen.getByRole("button", { name: "Show the snapshots of vaultwarden" });
    expect(pressed).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: "Show the snapshots of photos" })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
    expect(screen.getByText("click a name for its snapshots")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Snapshots · vaultwarden" })).toBeInTheDocument();
    expect(card().getByText("nas → Backblaze B2")).toBeInTheDocument();
    expect(list).toHaveBeenCalledWith("j1", "t1", false);

    const rows = await card().findAllByRole("listitem");
    expect(rows).toHaveLength(3);
    const first = within(rows[0]);
    expect(first.getByText("3f9a1c2e")).toHaveAttribute("title", "3f9a1c2e".padEnd(64, "0"));
    expect(first.getByText("today 12:00")).toBeInTheDocument();
    expect(first.getByText("1.2 GB")).toHaveAttribute("title", "Size of what was backed up");
    expect(within(rows[1]).getByText("today 11:00")).toBeInTheDocument();
    expect(card().getByText("As of 12:58")).toBeInTheDocument();
    expect(
      card().getByText(
        "Restore: to the original location (the containers stop for a while), to a new folder on any server, or download as a tar.",
      ),
    ).toBeInTheDocument();
  });

  it("shows newest instead of a delta on the newest row", async () => {
    vi.spyOn(restoresApi, "snapshots").mockResolvedValue(snapshots(3));
    showPage("viewer", "/backups?job=j1");
    const rows = await card().findAllByRole("listitem");
    expect(within(rows[0]).getByText("newest")).toBeInTheDocument();
    expect(within(rows[0]).queryByText(/^\+/)).not.toBeInTheDocument();
    expect(within(rows[1]).getByText("+2 MB")).toHaveAttribute("title", "Added to the repository");
    expect(within(rows[2]).getByText("+3 MB")).toBeInTheDocument();
  });

  it("measures a snapshot without a summary by its run", async () => {
    vi.spyOn(restoresApi, "snapshots").mockResolvedValue(
      snapshots(0, {
        snapshots: [snapshot("aaaa1111"), snapshot("bbbb2222", { size_bytes: 0, added_bytes: 0 })],
      }),
    );
    showPage("viewer", "/backups?job=j1", {
      backup_runs: [
        run("r9", { snapshot_id: "bbbb2222".padEnd(64, "0"), total_bytes: 3e9, bytes_added: 5e6 }),
      ],
    });
    const rows = await card().findAllByRole("listitem");
    expect(within(rows[1]).getByText("3 GB")).toBeInTheDocument();
    expect(within(rows[1]).getByText("+5 MB")).toBeInTheDocument();
  });

  it("switches targets, and names one the job no longer uses", async () => {
    const list = vi.spyOn(restoresApi, "snapshots").mockResolvedValue(snapshots(2));
    showPage("viewer", "/backups?job=j1");
    await card().findAllByRole("listitem");

    const group = card().getByRole("group", { name: "Target" });
    const buttons = within(group).getAllByRole("button");
    expect(buttons.map((button) => button.textContent)).toEqual([
      "Backblaze B2",
      "Panel disk",
      "backup-node (no longer used)",
    ]);
    expect(within(group).getByRole("button", { name: "Backblaze B2" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );

    fireEvent.click(within(group).getByRole("button", { name: "Panel disk" }));
    expect(location()).toBe("/backups?job=j1&target=t2");
    await waitFor(() => expect(list).toHaveBeenLastCalledWith("j1", "t2", false));
    expect(card().getByText("nas → Panel disk")).toBeInTheDocument();
    expect(within(group).getByRole("button", { name: "Panel disk" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
  });

  it("asks again while the panel says pending", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const list = vi
      .spyOn(restoresApi, "snapshots")
      .mockResolvedValueOnce({ pending: true })
      .mockResolvedValueOnce({ pending: true })
      .mockResolvedValue(snapshots(1));
    showPage("viewer", "/backups?job=j1");

    expect(await card().findByText("restic lists the snapshots on pve-01…")).toBeInTheDocument();
    await act(() => vi.advanceTimersByTimeAsync(2000));
    expect(list).toHaveBeenCalledTimes(2);
    await act(() => vi.advanceTimersByTimeAsync(2000));
    expect(await card().findByText("newest")).toBeInTheDocument();
    expect(list).toHaveBeenCalledTimes(3);
    expect(card().queryByText(/restic lists/)).not.toBeInTheDocument();
  });

  it("stops asking for a target it no longer shows", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const list = vi.spyOn(restoresApi, "snapshots").mockResolvedValue({ pending: true });
    const view = showPage("viewer", "/backups?job=j1");
    await card().findByText("restic lists the snapshots on pve-01…");
    view.unmount();
    await act(() => vi.advanceTimersByTimeAsync(6000));
    expect(list).toHaveBeenCalledTimes(1);
  });

  it("refreshes the list from the repository", async () => {
    const list = vi.spyOn(restoresApi, "snapshots").mockResolvedValue(snapshots(1));
    showPage("viewer", "/backups?job=j1");
    await card().findByText("newest");
    fireEvent.click(card().getByRole("button", { name: "Refresh the snapshots" }));
    await waitFor(() => expect(list).toHaveBeenLastCalledWith("j1", "t1", true));
    expect(list).toHaveBeenCalledTimes(2);
  });

  it("shows a banner when the server is offline, and one with retry when restic failed", async () => {
    const list = vi
      .spyOn(restoresApi, "snapshots")
      .mockRejectedValueOnce(
        failure(409, "The server pve-01 is offline: its agent lists the snapshots."),
      );
    showPage("viewer", "/backups?job=j1");
    const offline = await card().findByText(
      "The server pve-01 is offline: its agent lists the snapshots.",
    );
    expect(offline.closest(".banner")).toHaveClass("warn");
    expect(card().queryByRole("button", { name: "Try again" })).not.toBeInTheDocument();

    list.mockRejectedValueOnce(
      failure(
        504,
        "The agent of pve-01 did not answer in time: restic may still be reading the repository. Try again.",
      ),
    );
    fireEvent.click(card().getByRole("button", { name: "Panel disk" }));
    const timeout = await card().findByText(/did not answer in time/);
    expect(timeout.closest(".banner")).toHaveClass("err");
    expect(timeout).toHaveTextContent(/^The snapshots could not be listed: The agent of pve-01/);

    list.mockResolvedValueOnce(snapshots(2));
    fireEvent.click(card().getByRole("button", { name: "Try again" }));
    expect(await card().findByText("newest")).toBeInTheDocument();
    expect(list).toHaveBeenLastCalledWith("j1", "t2", false);
  });

  it("says when a target holds no snapshot of the job, or the job has no target", async () => {
    vi.spyOn(restoresApi, "snapshots").mockResolvedValue(snapshots(0));
    showPage("viewer", "/backups?job=j1");
    expect(
      await card().findByText("No snapshots of this job in Backblaze B2 yet."),
    ).toBeInTheDocument();
  });

  it("shows 20 rows, then the older ones, and says when the list is cut short", async () => {
    vi.spyOn(restoresApi, "snapshots").mockResolvedValue(
      snapshots(23, { total: 600, truncated: true }),
    );
    showPage("viewer", "/backups?job=j1");
    await card().findByText("newest");
    expect(card().getAllByRole("listitem")).toHaveLength(20);
    expect(card().getByText("Only the newest 23 snapshots are listed.")).toBeInTheDocument();
    fireEvent.click(card().getByRole("button", { name: "Show 3 older" }));
    expect(card().getAllByRole("listitem")).toHaveLength(23);
    expect(card().queryByRole("button", { name: /older/ })).not.toBeInTheDocument();
  });

  it("selects a job by its name", async () => {
    const list = vi.spyOn(restoresApi, "snapshots").mockResolvedValue(snapshots(1));
    showPage("viewer", "/backups?job=j2&target=t1");
    await card().findByText("newest");

    const button = screen.getByRole("button", { name: "Show the snapshots of vaultwarden" });
    expect(button).toHaveAttribute("aria-pressed", "false");
    expect(button).toHaveTextContent("vaultwarden");
    fireEvent.click(button);

    expect(location()).toBe("/backups?job=j1");
    expect(button).toHaveAttribute("aria-pressed", "true");
    expect(button.closest("tr")).toHaveClass("job-selected");
    expect(screen.getByRole("heading", { name: "Snapshots · vaultwarden" })).toBeInTheDocument();
    await waitFor(() => expect(list).toHaveBeenLastCalledWith("j1", "t1", false));
    // the actions of the row keep their names
    expect(screen.queryByRole("button", { name: "Run vaultwarden now" })).not.toBeInTheDocument();
  });

  it("keeps the restores from viewers, and shows an operator an open one", async () => {
    vi.spyOn(restoresApi, "snapshots").mockResolvedValue(snapshots(1));
    const lists = {
      backup_restores: [
        restore({ id: "r2", status: "running", command: "c1" }),
        restore({ id: "r3", status: "succeeded", finished_at: "2026-11-08 02:00:00.000Z" }),
        // a draft is someone's own, in their drawer
        restore({ id: "r4", status: "previewed" }),
      ],
    };
    const view = showPage("viewer", "/backups?job=j1", lists);
    await card().findByText("newest");
    expect(live.options.backup_restores.every((options) => options.enabled === false)).toBe(true);
    expect(screen.queryByText("Recent restores")).not.toBeInTheDocument();
    expect(screen.queryByText(/is under way/)).not.toBeInTheDocument();
    view.unmount();

    showPage("operator", "/backups?job=j1", lists);
    await card().findByText("newest");
    expect(live.options.backup_restores).toContainEqual(
      expect.objectContaining({ enabled: true, sort: "-created", perPage: 5 }),
    );
    expect(card().getByText("A restore of vaultwarden is under way.")).toBeInTheDocument();
    fireEvent.click(card().getByRole("button", { name: "Show" }));
    expect(location()).toBe("/backups?job=j1&restore=r2");

    const recent = within(screen.getByRole("list", { name: "Recent restores" }));
    const items = recent.getAllByRole("listitem");
    expect(items).toHaveLength(2);
    expect(within(items[0]).getByText("Running")).toBeInTheDocument();
    expect(within(items[1]).getByText("Done")).toBeInTheDocument();
    expect(
      within(items[1]).getByText(/^Sun 03:00 · nas · operator@example.com$/),
    ).toBeInTheDocument();
  });

  it("leaves the drafts out of the recent restores, and never loses the restore under way", async () => {
    vi.spyOn(restoresApi, "snapshots").mockResolvedValue(snapshots(1));
    const at = (minute: number) => `2026-11-09 11:${String(minute).padStart(2, "0")}:00.000Z`;
    const running = restore({
      id: "r2",
      status: "running",
      command: "c1",
      created: at(10),
      label: "the restore under way",
    });
    const recentLabels = () =>
      within(screen.getByRole("list", { name: "Recent restores" }))
        .getAllByRole("listitem")
        .map((item) => item.querySelector(".restore-label")?.textContent);

    // drafts, newer than the restore: five previews, one that failed, and a
    // download whose link was never used
    const view = showPage("operator", "/backups?job=j1", {
      backup_restores: [
        running,
        ...[1, 2, 3, 4, 5].map((n) =>
          restore({ id: `d${n}`, status: "previewed", created: at(20 + n), label: `draft ${n}` }),
        ),
        restore({
          id: "d6",
          status: "failed",
          error: "vaultwarden-app-1 of PocketCloud writes vw_data.",
          created: at(30),
          label: "a preview that failed",
        }),
        restore({
          id: "d7",
          mode: "download",
          status: "queued",
          created: at(31),
          label: "a download nobody started",
        }),
      ],
    });
    await card().findByText("newest");
    expect(card().getByText("A restore of vaultwarden is under way.")).toBeInTheDocument();
    fireEvent.click(card().getByRole("button", { name: "Show" }));
    expect(location()).toBe("/backups?job=j1&restore=r2");
    expect(recentLabels()).toEqual(["the restore under way"]);
    expect(screen.queryByText("Failed")).not.toBeInTheDocument();
    view.unmount();

    // five downloads since: the restore under way still shows, and a
    // download is no restore of the job's data
    showPage("operator", "/backups?job=j1", {
      backup_restores: [
        running,
        ...[1, 2, 3, 4].map((n) =>
          restore({
            id: `g${n}`,
            mode: "download",
            status: "succeeded",
            command: `c${n + 1}`,
            created: at(20 + n),
            label: `download ${n}`,
          }),
        ),
        restore({
          id: "g5",
          mode: "download",
          status: "running",
          command: "c6",
          created: at(25),
          label: "download 5",
        }),
      ],
    });
    await card().findByText("newest");
    fireEvent.click(card().getByRole("button", { name: "Show" }));
    expect(location()).toBe("/backups?job=j1&restore=r2");
    expect(recentLabels()).toEqual([
      "download 5",
      "download 4",
      "download 3",
      "download 2",
      "download 1",
      "the restore under way",
    ]);
  });
});

describe("the snapshots' row actions", () => {
  function showCard(canRestore: boolean) {
    const onRestore = vi.fn();
    const onFiles = vi.fn();
    vi.spyOn(restoresApi, "snapshots").mockResolvedValue(snapshots(2));
    wrap(
      <SnapshotsCard
        job={vaultwarden}
        sources={[{ target: bucket, old: false }]}
        target={bucket}
        hosts={hosts}
        runs={[]}
        now={now}
        canRestore={canRestore}
        open={null}
        onTarget={vi.fn()}
        onRestore={onRestore}
        onFiles={onFiles}
        onShowRestore={vi.fn()}
      />,
    );
    return { onRestore, onFiles };
  }

  it("keeps Restore and Files hidden from viewers", async () => {
    showCard(false);
    await screen.findByText("newest");
    expect(screen.queryByRole("button", { name: /^Restore snapshot/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Browse the files/ })).not.toBeInTheDocument();
  });

  it("gives an operator Files and Restore on each snapshot", async () => {
    const { onRestore, onFiles } = showCard(true);
    await screen.findByText("newest");
    // a single target needs no switch
    expect(screen.queryByRole("group", { name: "Target" })).not.toBeInTheDocument();
    const restoreButton = screen.getByRole("button", { name: "Restore snapshot 3f9a1c2e" });
    expect(restoreButton).toHaveTextContent("Restore…");
    fireEvent.click(restoreButton);
    expect(onRestore).toHaveBeenCalledWith(expect.objectContaining({ short_id: "3f9a1c2e" }));
    fireEvent.click(screen.getByRole("button", { name: "Browse the files of snapshot 3f9a1c2d" }));
    expect(onFiles).toHaveBeenCalledWith(expect.objectContaining({ short_id: "3f9a1c2d" }));
  });

  it("lists nothing for a job without a target", () => {
    const list = vi.spyOn(restoresApi, "snapshots");
    wrap(
      <SnapshotsCard
        job={{ ...photos, targets: [] }}
        sources={[]}
        target={null}
        hosts={hosts}
        runs={[]}
        now={now}
        canRestore
        open={null}
        onTarget={vi.fn()}
        onShowRestore={vi.fn()}
      />,
    );
    expect(screen.getByText("This job backs up to no target yet.")).toBeInTheDocument();
    expect(list).not.toHaveBeenCalled();
  });
});

describe("the recent restores", () => {
  it("say nothing when there are none", () => {
    const view = wrap(<RecentRestores restores={[]} hosts={hosts} now={now} onShow={vi.fn()} />);
    expect(view.container.querySelector(".card")).toBeNull();
  });
});
