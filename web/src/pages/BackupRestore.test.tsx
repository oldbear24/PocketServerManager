import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { ClientResponseError } from "pocketbase";
import { useState, type ReactNode } from "react";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import i18n from "../i18n";
import { api } from "../lib/api";
import { restoresApi, type BackupRestore, type SnapshotFolder } from "../lib/backupRestores";
import { pb, type User } from "../lib/pb";
import type { PanelInfo } from "../lib/types";
import { BackupsPage } from "./Backups";
import {
  command,
  folder,
  hosts,
  job,
  jobs,
  now,
  preview,
  restore,
  run,
  signInAs,
  snapshotId,
  snapshots,
  targets,
  umami,
  wrap,
} from "./BackupRestore.fixtures";
import { RestoreDrawer, type RestoreStart } from "./BackupRestore";

const live = vi.hoisted(() => ({
  lists: {} as Record<string, unknown[]>,
  records: {} as Record<string, unknown>,
  listeners: new Set<() => void>(),
}));
vi.mock("../lib/realtime", async () => {
  const { useEffect, useState } = await import("react");
  const list = (collection: string, options: { enabled?: boolean } = {}) => {
    const items = options.enabled === false ? [] : (live.lists[collection] ?? []);
    return { items, loading: false, error: null, total: items.length, reload: () => {} };
  };
  function useLiveRecord(collection: string, id: string | undefined) {
    const [, setTick] = useState(0);
    useEffect(() => {
      const listener = () => setTick((tick) => tick + 1);
      live.listeners.add(listener);
      return () => {
        live.listeners.delete(listener);
      };
    }, []);
    const record = id ? (live.records[`${collection}/${id}`] ?? null) : null;
    return { record, loading: false, error: null };
  }
  return { useLiveList: list, useLiveRecords: list, useLiveRecord, useNow: () => now };
});

// the output streams from the panel; here it only says what it is of
vi.mock("../components/CommandOutput", () => ({
  CommandOutput: ({ title, status }: { title: ReactNode; status?: ReactNode }) => (
    <section aria-label="Command output">
      <h4>{title}</h4>
      {status}
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

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  pb.authStore.clear();
  live.lists = {};
  live.records = {};
  live.listeners.clear();
  document.querySelectorAll("iframe").forEach((frame) => frame.remove());
});

/** A record as realtime sends it, saved a moment after the one before. */
let saved = 0;
function push(collection: string, record: { id: string } & Record<string, unknown>) {
  saved++;
  act(() => {
    live.records[`${collection}/${record.id}`] = {
      ...record,
      updated: `2026-11-09 12:00:${String(saved % 60).padStart(2, "0")}.${String(saved).padStart(3, "0")}Z`,
    };
    live.listeners.forEach((listener) => listener());
  });
}

const first = snapshotId("3f9a1c2e");

function showDrawer(role: User["role"], start: RestoreStart = {}, onRecord = vi.fn()) {
  signInAs(role);
  vi.spyOn(restoresApi, "snapshots").mockResolvedValue(snapshots(3));
  const onClose = vi.fn();
  const view = wrap(
    <RestoreDrawer
      jobs={jobs}
      targets={targets}
      hosts={hosts}
      runs={[run("r1", {})]}
      stacks={[]}
      start={{ job: "j1", target: "t1", snapshot: first, ...start }}
      onRecord={onRecord}
      onClose={onClose}
    />,
  );
  return { view, onClose, onRecord };
}

function drawer() {
  const element = document.querySelector<HTMLElement>("dialog.drawer");
  if (!element) {
    throw new Error("the drawer is not open");
  }
  return within(element);
}

function failure(status: number, message: string) {
  return new ClientResponseError({ status, response: { message }, url: "", isAbort: false });
}

/** A draft as the panel makes it, and as it reads once previewed. */
function previewed(fields: Partial<BackupRestore> = {}): BackupRestore {
  return restore({
    status: "previewed",
    preview_at: "2026-11-09 11:51:00.000Z",
    preview: preview(),
    ...fields,
  });
}

describe("the restore drawer", () => {
  it("previews before restoring", async () => {
    const create = vi.spyOn(restoresApi, "create").mockResolvedValue(restore());
    const { onRecord } = showDrawer("operator");

    expect(await drawer().findByText("Time")).toBeInTheDocument();
    expect(drawer().getByRole("heading", { name: "Restore vaultwarden" })).toBeInTheDocument();
    expect(drawer().getByText("today 12:00")).toBeInTheDocument();
    expect(drawer().getByText("The whole snapshot")).toBeInTheDocument();
    const original = drawer().getByRole("radio", { name: /To the original location/ });
    expect(original).toHaveAttribute("aria-checked", "true");
    expect(original).toHaveTextContent(
      "On nas: volume vw_data. The containers that use this data stop for a while and start again.",
    );
    // an operator restores to the original location only
    expect(drawer().getByRole("radio", { name: /To a new folder/ })).toBeDisabled();
    expect(drawer().getByRole("radio", { name: /Download as a tar/ })).toHaveTextContent(
      "Only admins restore to another folder or download.",
    );
    // back to the snapshot's state, at the original location
    expect(
      drawer().getByRole("checkbox", { name: /Also delete what the destination/ }),
    ).toBeChecked();

    // nothing restores before a preview
    expect(drawer().queryByRole("button", { name: /^Restore/ })).not.toBeInTheDocument();
    fireEvent.click(drawer().getByRole("button", { name: "Preview" }));
    await waitFor(() =>
      expect(create).toHaveBeenCalledWith("j1", {
        target: "t1",
        snapshot: first,
        mode: "original",
        replace: true,
      }),
    );
    expect(onRecord).toHaveBeenCalledWith("r1");
    expect(
      await drawer().findByText(
        "restic compares the snapshot with vw_data on nas; nothing changes yet.",
      ),
    ).toBeInTheDocument();
    expect(drawer().getByRole("button", { name: "Preview" })).toBeDisabled();

    push("backup_restores", previewed());
    expect(await drawer().findByRole("button", { name: "Restore…" })).toBeEnabled();
    expect(
      drawer().getByText("The destination is empty: nothing is overwritten."),
    ).toBeInTheDocument();
  });

  it("reads the preview as a sentence", async () => {
    live.records["backup_restores/r1"] = previewed({
      preview: preview({
        roots: [
          { ...preview().roots[0], entries: 12 },
          {
            ...preview().roots[0],
            index: 1,
            source: "/volumes/vw_icons",
            destination: "vw_icons",
            in_snapshot: false,
            exists: true,
          },
        ],
        not_in_job: ["/volumes/vw_old"],
        counts: { new: 3, updated: 2, unchanged: 40, deleted: 1 },
        bytes: { new: 4.2e6, updated: 1.5e6, deleted: 2e3 },
        samples: {
          new: ["db.sqlite3", "attachments/a.bin"],
          updated: ["config.json"],
          deleted: ["tmp/x"],
        },
        truncated: { new: 1, updated: 1, deleted: 0 },
        would_stop: [
          { name: "vaultwarden-app-1", project: "vaultwarden" },
          { name: "vaultwarden-db-1", project: "vaultwarden" },
        ],
        notes: ["The folder /srv/app leads to /data/app."],
      }),
    });
    showDrawer("operator", { restore: "r1" });

    expect(
      await drawer().findByText(
        "3 new files (4.2 MB), 2 changed files overwritten (1.5 MB), 1 extra file deleted, and 40 files already match.",
      ),
    ).toBeInTheDocument();
    expect(drawer().getByText("The destination already holds 12 items.")).toBeInTheDocument();
    expect(
      drawer().getByText("Volume vw_icons is not in this snapshot: it stays as it is."),
    ).toBeInTheDocument();
    expect(
      drawer().getByText(
        "The snapshot also holds /volumes/vw_old, which the job no longer backs up: restore it to a folder.",
      ),
    ).toBeInTheDocument();
    expect(
      drawer().getByText("These containers stop for a while and start again:"),
    ).toBeInTheDocument();
    expect(drawer().getByText("vaultwarden-app-1")).toBeInTheDocument();
    expect(drawer().getByText("vaultwarden-db-1")).toBeInTheDocument();

    const changes = within(drawer().getByRole("region", { name: "What changes" }));
    expect(changes.getAllByText(/./).map((line) => line.textContent)).toEqual(
      expect.arrayContaining(["+", "db.sqlite3", "~", "config.json", "−", "tmp/x", "and 2 more"]),
    );
    expect(changes.getByText("db.sqlite3").closest(".diff-line")).toHaveClass("add");
    expect(changes.getByText("tmp/x").closest(".diff-line")).toHaveClass("del");
    expect(drawer().getByRole("region", { name: "What changes" })).toHaveAttribute("tabindex", "0");
    expect(drawer().getByText("The folder /srv/app leads to /data/app.")).toBeInTheDocument();
  });

  it("says when the preview is too old, and previews again", async () => {
    live.records["backup_restores/r1"] = previewed({ expires_at: "2026-11-09 11:59:00.000Z" });
    const discard = vi.spyOn(restoresApi, "discard").mockResolvedValue(undefined);
    const create = vi.spyOn(restoresApi, "create").mockResolvedValue(restore({ id: "r2" }));
    showDrawer("operator", { restore: "r1" });
    expect(
      await drawer().findByText("The preview is older than 30 minutes: preview again."),
    ).toBeInTheDocument();
    expect(drawer().queryByRole("button", { name: "Restore…" })).not.toBeInTheDocument();
    fireEvent.click(drawer().getByRole("button", { name: "Preview" }));
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(discard).toHaveBeenCalledWith("r1");
  });

  it("keeps Restore disabled until the overwrite is confirmed", async () => {
    live.records["backup_restores/r1"] = previewed();
    const start = vi
      .spyOn(restoresApi, "start")
      .mockRejectedValueOnce(
        failure(
          409,
          "The destination already holds files: confirm that the restore overwrites them.",
        ),
      )
      .mockResolvedValueOnce(restore({ status: "queued", command: "c1", overwrite: true }));
    showDrawer("operator", { restore: "r1" });

    fireEvent.click(await drawer().findByRole("button", { name: "Restore…" }));
    const dialog = within(screen.getByRole("dialog", { name: "Overwrite vw_data on nas?" }));
    expect(
      dialog.getByText(
        "The restore replaces files with those of the snapshot from Nov 9, 2026, 12:00:00 PM. This cannot be undone.",
      ),
    ).toBeInTheDocument();
    const confirm = dialog.getByRole("button", { name: "Restore and overwrite" });
    expect(confirm).toBeDisabled();
    fireEvent.click(confirm);
    expect(start).not.toHaveBeenCalled();

    const box = dialog.getByRole("checkbox", {
      name: "I understand that files in vw_data will be overwritten",
    });
    fireEvent.click(box);
    expect(confirm).toBeEnabled();
    // the route's 409 is shown in the dialog, which stays open
    fireEvent.click(confirm);
    expect(
      await dialog.findByText(
        "The destination already holds files: confirm that the restore overwrites them.",
      ),
    ).toBeInTheDocument();
    expect(start).toHaveBeenLastCalledWith("r1", true);

    fireEvent.click(confirm);
    await waitFor(() => expect(start).toHaveBeenCalledTimes(2));
    expect(await screen.findByText("The restore of vaultwarden started.")).toBeInTheDocument();
    expect(drawer().getByText("Waits for nas.")).toBeInTheDocument();
  });

  it("asks nothing more when a new folder is empty and nothing would change", async () => {
    live.records["backup_restores/r1"] = previewed({
      mode: "path",
      host: "h1",
      path: "/srv/restored",
      source: "/volumes/vw_data",
      replace: false,
      roots: null,
      preview: preview({
        roots: [
          { ...preview().roots[0], destination: "/srv/restored", exists: false, free_bytes: 40e9 },
        ],
        counts: { new: 1200, updated: 0, unchanged: 0, deleted: 0 },
        bytes: { new: 1.2e9, updated: 0, deleted: 0 },
      }),
    });
    const start = vi
      .spyOn(restoresApi, "start")
      .mockResolvedValue(restore({ status: "queued", command: "c1" }));
    showDrawer("admin", { restore: "r1" });
    expect(await drawer().findByText("40 GB free; the snapshot holds 1.2 GB.")).toBeInTheDocument();
    fireEvent.click(drawer().getByRole("button", { name: "Restore" }));
    await waitFor(() => expect(start).toHaveBeenCalledWith("r1", false));
    expect(screen.queryByRole("dialog", { name: /^Overwrite/ })).not.toBeInTheDocument();
  });

  it("offers only its own server for a local target", async () => {
    showDrawer("admin", { target: "t2" });
    fireEvent.click(await drawer().findByRole("radio", { name: /To a new folder/ }));
    const select = drawer().getByLabelText("Server");
    // the snapshots of a folder of nas restore on nas
    expect(select).toHaveValue("h2");
    expect(within(select).getByRole("option", { name: "nas" })).toBeEnabled();
    expect(within(select).getByRole("option", { name: "pve-01" })).toBeDisabled();
    expect(within(select).getByRole("option", { name: "edge-01 (offline)" })).toBeDisabled();
    expect(
      drawer().getByText("Panel disk is a folder of nas: only that server restores from it."),
    ).toBeInTheDocument();
  });

  it("restores into a new folder of any server for a remote target", async () => {
    const create = vi.spyOn(restoresApi, "create").mockResolvedValue(restore({ mode: "path" }));
    showDrawer("admin");
    fireEvent.click(await drawer().findByRole("radio", { name: /To a new folder/ }));
    const select = drawer().getByLabelText("Server");
    expect(within(select).getByRole("option", { name: "pve-01" })).toBeEnabled();
    fireEvent.change(select, { target: { value: "h1" } });
    // a new folder keeps what it holds unless asked
    expect(drawer().getByRole("checkbox", { name: /Also delete/ })).not.toBeChecked();

    const path = drawer().getByLabelText("Folder");
    fireEvent.change(path, { target: { value: "/etc/restored" } });
    expect(
      drawer().getByText(
        "Give an absolute folder outside the system's folders, such as /srv/restored.",
      ),
    ).toBeInTheDocument();
    expect(drawer().getByRole("button", { name: "Preview" })).toBeDisabled();
    fireEvent.change(path, { target: { value: "/srv/restored" } });
    expect(drawer().getByLabelText("Folder of the snapshot")).toHaveValue("/volumes/vw_data");

    fireEvent.click(drawer().getByRole("button", { name: "Preview" }));
    await waitFor(() =>
      expect(create).toHaveBeenCalledWith("j1", {
        target: "t1",
        snapshot: first,
        mode: "path",
        host: "h1",
        path: "/srv/restored",
        source: "/volumes/vw_data",
        replace: false,
      }),
    );
  });

  it("offers no original location from another server's local target", async () => {
    signInAs("admin");
    vi.spyOn(restoresApi, "snapshots").mockResolvedValue(snapshots(1));
    const elsewhere = job({ id: "j9", name: "notes", host: "h1", targets: ["t2"] });
    wrap(
      <RestoreDrawer
        jobs={[elsewhere]}
        targets={targets}
        hosts={hosts}
        runs={[]}
        stacks={[]}
        start={{ job: "j9", target: "t2", snapshot: first }}
        onRecord={vi.fn()}
        onClose={vi.fn()}
      />,
    );
    const original = await drawer().findByRole("radio", { name: /To the original location/ });
    expect(original).toBeDisabled();
    expect(original).toHaveTextContent(
      "Panel disk is a folder of nas: only that server restores from it.",
    );
    expect(drawer().getByRole("radio", { name: /To a new folder/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
  });

  it("tells where a stack's volumes go back, in both languages", async () => {
    signInAs("operator");
    vi.spyOn(restoresApi, "snapshots").mockResolvedValue(snapshots(1));
    const shop = job({ id: "j9", name: "shop", source_kind: "stack", source: "", stack: "s2" });
    const view = () =>
      wrap(
        <RestoreDrawer
          jobs={[shop]}
          targets={targets}
          hosts={hosts}
          runs={[]}
          stacks={[{ id: "s2", name: "shop", host: "h2" }]}
          start={{ job: "j9", target: "t1", snapshot: first }}
          onRecord={vi.fn()}
          onClose={vi.fn()}
        />,
      );
    const shown = view();
    // a stack's volumes are many: the note does not hang on how many
    expect(
      await drawer().findByRole("radio", { name: /To the original location/ }),
    ).toHaveTextContent(
      "On nas: volumes of stack shop. The containers that use this data stop for a while and start again.",
    );
    shown.unmount();

    await act(() => i18n.changeLanguage("cs"));
    try {
      view();
      expect(await drawer().findByRole("radio", { name: /Na původní místo/ })).toHaveTextContent(
        "Na serveru nas: svazky stacku shop. Kontejnery, které tato data používají, se na chvíli zastaví a pak znovu spustí.",
      );
    } finally {
      await act(() => i18n.changeLanguage("en"));
    }
  });

  it("offers no original location for a dump", async () => {
    signInAs("admin");
    vi.spyOn(restoresApi, "snapshots").mockResolvedValue(snapshots(1));
    wrap(
      <RestoreDrawer
        jobs={jobs}
        targets={targets}
        hosts={hosts}
        runs={[]}
        stacks={[]}
        start={{ job: umami.id, target: "t1", snapshot: first }}
        onRecord={vi.fn()}
        onClose={vi.fn()}
      />,
    );
    const original = await drawer().findByRole("radio", { name: /To the original location/ });
    expect(original).toBeDisabled();
    expect(original).toHaveTextContent(
      "A database dump goes back into its database with the database's own restore: restore it to a folder, or download it.",
    );
    expect(drawer().getByRole("radio", { name: /To a new folder/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
  });

  it("changing an option asks for a new preview", async () => {
    live.records["backup_restores/r1"] = previewed();
    const discard = vi.spyOn(restoresApi, "discard").mockResolvedValue(undefined);
    const { onRecord } = showDrawer("operator", { restore: "r1" });
    await drawer().findByRole("button", { name: "Restore…" });

    fireEvent.click(drawer().getByRole("checkbox", { name: /Also delete/ }));
    expect(discard).toHaveBeenCalledWith("r1");
    expect(onRecord).toHaveBeenLastCalledWith(null);
    expect(drawer().getByText("The options changed: preview again.")).toBeInTheDocument();
    expect(drawer().queryByRole("button", { name: "Restore…" })).not.toBeInTheDocument();
    expect(drawer().getByRole("button", { name: "Preview" })).toBeEnabled();
    expect(drawer().getByRole("checkbox", { name: /Also delete/ })).not.toBeChecked();
  });

  it("discards its draft when it closes", async () => {
    live.records["backup_restores/r1"] = previewed();
    const discard = vi.spyOn(restoresApi, "discard").mockResolvedValue(undefined);
    const { onClose } = showDrawer("operator", { restore: "r1" });
    await drawer().findByRole("button", { name: "Restore…" });
    fireEvent.click(drawer().getByRole("button", { name: "Cancel" }));
    expect(discard).toHaveBeenCalledWith("r1");
    expect(onClose).toHaveBeenCalled();
  });

  it("browses a folder and pages through it", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const page = (path: string, offset: number, fields: Partial<SnapshotFolder> = {}) =>
      folder(path, {
        total: 450,
        offset,
        entries: Array.from({ length: offset < 400 ? 200 : 50 }, (_, index) => ({
          name: `${offset === 0 && index === 0 ? "attachments" : `file-${offset + index}`}`,
          type: offset === 0 && index === 0 ? ("dir" as const) : ("file" as const),
          size: 1000,
          mtime: "2026-11-09T10:00:00Z",
          mode: 420,
          bad_name: false,
        })),
        ...fields,
      });
    const files = vi
      .spyOn(restoresApi, "files")
      .mockImplementation(async (_job, _target, _snapshot, request) => {
        if (request.match) {
          return folder(request.path, {
            total: 1,
            files: 1,
            dirs: 0,
            entries: [folder("/").entries[1]],
          });
        }
        if (request.path === "/volumes/vw_data/attachments") {
          return folder(request.path, {
            entries: [
              { ...folder("/").entries[2], name: "bad�name", bad_name: true },
              // restic shows what is not UTF-8 as U+FFFD, though the agent did not say so
              { ...folder("/").entries[2], name: "odd�.txt", bad_name: false },
              folder("/").entries[2],
            ],
          });
        }
        return page(request.path, request.offset ?? 0);
      });
    showDrawer("operator", { browse: true });

    const nav = await screen.findByRole("navigation", { name: "Path in the snapshot" });
    expect(within(nav).getByText("Volume vw_data")).toHaveAttribute("aria-current", "location");
    expect(await drawer().findByText("1–200 of 450")).toBeInTheDocument();
    expect(files).toHaveBeenCalledWith("j1", "t1", first, {
      path: "/volumes/vw_data",
      offset: 0,
      limit: 200,
      match: "",
    });
    fireEvent.click(drawer().getByRole("button", { name: "Next 200" }));
    expect(await drawer().findByText("201–400 of 450")).toBeInTheDocument();
    fireEvent.click(drawer().getByRole("button", { name: "Next 50" }));
    expect(await drawer().findByText("401–450 of 450")).toBeInTheDocument();
    expect(drawer().queryByRole("button", { name: /^Next/ })).not.toBeInTheDocument();
    fireEvent.click(drawer().getByRole("button", { name: "Previous" }));
    expect(await drawer().findByText("201–400 of 450")).toBeInTheDocument();

    // a filter asks once the typing stops
    fireEvent.change(drawer().getByRole("searchbox", { name: "Filter by name" }), {
      target: { value: "db" },
    });
    await act(() => vi.advanceTimersByTimeAsync(300));
    await waitFor(() =>
      expect(files).toHaveBeenLastCalledWith("j1", "t1", first, {
        path: "/volumes/vw_data",
        offset: 0,
        limit: 200,
        match: "db",
      }),
    );
    fireEvent.change(drawer().getByRole("searchbox", { name: "Filter by name" }), {
      target: { value: "" },
    });
    await act(() => vi.advanceTimersByTimeAsync(300));

    // a folder opens; a name that is not valid UTF-8 cannot be picked
    fireEvent.click(await drawer().findByRole("button", { name: "attachments" }));
    expect(await within(nav).findByText("attachments")).toHaveAttribute("aria-current", "location");
    expect(await drawer().findByRole("checkbox", { name: "Pick bad�name" })).toBeDisabled();
    expect(drawer().getByRole("checkbox", { name: "Pick odd�.txt" })).toBeDisabled();
    fireEvent.click(drawer().getByRole("checkbox", { name: "Pick config.json" }));
    expect(drawer().getByText("1 item picked")).toBeInTheDocument();
    // an operator downloads nothing
    expect(drawer().queryByRole("button", { name: /^Download/ })).not.toBeInTheDocument();

    fireEvent.click(within(nav).getByRole("button", { name: "Up one level" }));
    expect(await within(nav).findByText("Volume vw_data")).toHaveAttribute(
      "aria-current",
      "location",
    );

    fireEvent.click(drawer().getByRole("button", { name: "Done" }));
    expect(drawer().getByText("/volumes/vw_data/attachments/config.json")).toBeInTheDocument();
    // only the whole snapshot is put back as it was
    expect(drawer().getByRole("checkbox", { name: /Also delete/ })).toBeDisabled();
    expect(drawer().getByText("Only when the whole snapshot is restored.")).toBeInTheDocument();
    fireEvent.click(
      drawer().getByRole("button", { name: "Remove /volumes/vw_data/attachments/config.json" }),
    );
    expect(drawer().getByText("The whole snapshot")).toBeInTheDocument();
  });

  it("picks at most 100 items", async () => {
    vi.spyOn(restoresApi, "files").mockResolvedValue(
      folder("/volumes/vw_data", {
        total: 101,
        entries: Array.from({ length: 101 }, (_, index) => ({
          name: `f${index}`,
          type: "file" as const,
          size: 1,
          mtime: "",
          mode: 420,
          bad_name: false,
        })),
      }),
    );
    showDrawer("operator", { browse: true });
    await drawer().findByText("1–101 of 101");
    // the rows stay the same elements as they are picked
    const boxes = [
      ...document.querySelectorAll<HTMLInputElement>(
        'input[type="checkbox"][aria-label^="Pick f"]',
      ),
    ];
    expect(boxes).toHaveLength(101);
    for (const box of boxes.slice(0, 100)) {
      fireEvent.click(box);
    }
    expect(boxes[100]).toBeDisabled();
    expect(boxes[100]).toHaveAccessibleName("Pick f100");
    expect(boxes[99]).toBeEnabled();
    expect(boxes[99]).toBeChecked();
    expect(
      drawer().getByText("100 items picked Pick at most 100 items, or a whole folder."),
    ).toBeInTheDocument();
  });

  it("starts the download in a hidden frame", async () => {
    const download = vi.spyOn(restoresApi, "download").mockResolvedValue({
      restore: restore({
        id: "d1",
        mode: "download",
        status: "queued",
        path: "/volumes/vw_data",
        preview_command: "",
        download_name: "vaultwarden-3f9a1c2e-20261109-1200.tar",
        download_kind: "dir",
      }),
      url: "/api/pc/backup-downloads/d1?ticket=ab12",
    });
    const { onRecord } = showDrawer("admin");
    fireEvent.click(await drawer().findByRole("radio", { name: /Download as a tar/ }));
    expect(drawer().queryByRole("button", { name: "Preview" })).not.toBeInTheDocument();
    fireEvent.click(drawer().getByRole("button", { name: "Download" }));

    await waitFor(() =>
      expect(download).toHaveBeenCalledWith("j1", {
        target: "t1",
        snapshot: first,
        path: "/volumes/vw_data",
        kind: "dir",
      }),
    );
    const frame = document.querySelector("iframe")!;
    expect(frame.hidden).toBe(true);
    expect(frame.getAttribute("src")).toBe("/api/pc/backup-downloads/d1?ticket=ab12");
    expect(frame.title).toBe("Download of vaultwarden-3f9a1c2e-20261109-1200.tar");
    // outside the app, so that the drawer closing does not cut it
    expect(frame.parentElement).toBe(document.body);
    expect(onRecord).toHaveBeenCalledWith("d1");
    expect(
      await drawer().findByText("If the download does not start, try again."),
    ).toBeInTheDocument();
    expect(
      drawer().getByText("The download cannot be resumed: if it breaks, start it again."),
    ).toBeInTheDocument();

    push("backup_restores", {
      ...restore({
        id: "d1",
        mode: "download",
        status: "running",
        command: "c2",
        download_name: "vaultwarden-3f9a1c2e-20261109-1200.tar",
      }),
    });
    push(
      "commands",
      command({
        id: "c2",
        type: "backup.dump",
        progress: { step: "download", percent: -1, done: 5e6, total: 1.2e9, unit: "bytes" },
      }),
    );
    expect(await drawer().findByText("Downloading: 5 MB of about 1.2 GB")).toBeInTheDocument();
    push(
      "commands",
      command({
        id: "c2",
        type: "backup.dump",
        progress: { step: "download", percent: -1, done: 7e6, total: 0, unit: "bytes" },
      }),
    );
    expect(await drawer().findByText("Downloading: 7 MB so far")).toBeInTheDocument();
    push("backup_restores", {
      ...restore({ id: "d1", mode: "download", status: "succeeded", command: "c2", bytes: 1.19e9 }),
    });
    expect(await drawer().findByText("Downloaded 1.2 GB.")).toBeInTheDocument();
  });

  it("says why a download's link did not give a file", async () => {
    vi.spyOn(restoresApi, "download").mockResolvedValue({
      restore: restore({ id: "d1", mode: "download", status: "queued", path: "/volumes/vw_data" }),
      url: "/api/pc/backup-downloads/d1?ticket=ab12",
    });
    showDrawer("admin");
    fireEvent.click(await drawer().findByRole("radio", { name: /Download as a tar/ }));
    fireEvent.click(drawer().getByRole("button", { name: "Download" }));
    await waitFor(() => expect(document.querySelector("iframe")).not.toBeNull());
    const frame = document.querySelector("iframe")!;
    // the panel answers its refusals as JSON, which a browser shows in a <pre>
    const page = document.implementation.createHTMLDocument("");
    const pre = page.createElement("pre");
    pre.textContent =
      '{"data":{},"message":"This download link has been used or has expired: start the download again.","status":403}';
    page.body.append(pre);
    Object.defineProperty(frame, "contentDocument", { value: page, configurable: true });
    act(() => {
      frame.dispatchEvent(new Event("load"));
    });
    expect(
      await drawer().findByText(
        "The download failed: This download link has been used or has expired: start the download again.",
      ),
    ).toBeInTheDocument();
    expect(drawer().getByRole("button", { name: "Download" })).toBeEnabled();
  });

  it("downloads one file of a snapshot from the browser", async () => {
    vi.spyOn(restoresApi, "files").mockResolvedValue(folder("/volumes/vw_data"));
    const download = vi.spyOn(restoresApi, "download").mockResolvedValue({
      restore: restore({
        id: "d1",
        mode: "download",
        status: "queued",
        path: "/volumes/vw_data/db.sqlite3",
      }),
      url: "/api/pc/backup-downloads/d1?ticket=cd34",
    });
    showDrawer("admin", { browse: true });
    fireEvent.click(await drawer().findByRole("button", { name: "Download db.sqlite3" }));
    await waitFor(() =>
      expect(download).toHaveBeenCalledWith("j1", {
        target: "t1",
        snapshot: first,
        path: "/volumes/vw_data/db.sqlite3",
        kind: "file",
      }),
    );
    fireEvent.click(drawer().getByRole("button", { name: "Download" }));
  });

  it("shows progress with bytes and stops the restore", async () => {
    live.records["backup_restores/r1"] = restore({
      status: "running",
      command: "c1",
      preview: preview({ would_stop: [{ name: "vaultwarden-app-1", project: "vaultwarden" }] }),
    });
    live.records["commands/c1"] = command({
      progress: { step: "restore", percent: 40, done: 4.8e8, total: 1.2e9, unit: "bytes" },
    });
    const cancel = vi.spyOn(restoresApi, "cancel").mockResolvedValue({ stopping: true });
    showDrawer("operator", { restore: "r1" });

    expect(await drawer().findByText("480 MB of 1.2 GB")).toBeInTheDocument();
    const bar = drawer().getByRole("progressbar", { name: "Progress of the restore" });
    expect(bar).toHaveAttribute("aria-valuenow", "40");
    const steps = within(drawer().getByRole("list", { name: "Steps of the restore" }));
    const items = steps.getAllByRole("listitem");
    expect(items.map((item) => item.textContent)).toEqual([
      "Check the snapshot",
      "Stop the containers",
      "Restore the files",
      "Start the containers",
      "Check the containers",
    ]);
    expect(items.map((item) => item.className)).toEqual([
      "done",
      "done",
      "active",
      "pending",
      "pending",
    ]);
    expect(drawer().getByRole("region", { name: "Command output" })).toHaveTextContent(
      "Restore outputrunning",
    );

    fireEvent.click(drawer().getByRole("button", { name: "Stop the restore" }));
    const ask = within(screen.getByRole("dialog", { name: "Stop the restore?" }));
    expect(
      ask.getByText("What is restored so far stays, and the containers start again."),
    ).toBeInTheDocument();
    fireEvent.click(ask.getByRole("button", { name: "Stop the restore" }));
    await waitFor(() => expect(cancel).toHaveBeenCalledWith("r1"));
    expect(
      await drawer().findByText("Stopping: the agent starts the containers again first."),
    ).toBeInTheDocument();
    expect(drawer().queryByRole("button", { name: "Stop the restore" })).not.toBeInTheDocument();

    push("backup_restores", {
      ...restore({
        status: "cancelled",
        command: "c1",
        partial: true,
        app_stopped: [{ id: "a1", name: "vaultwarden-app-1" }],
        error:
          "Stopped by operator@example.com before it was complete: the destination holds part of the snapshot. The application was started again.",
        preview: preview({ would_stop: [{ name: "vaultwarden-app-1", project: "vaultwarden" }] }),
      }),
    });
    expect(
      await drawer().findByText(
        "The restore was stopped. The destination keeps what was restored so far.",
      ),
    ).toBeInTheDocument();
    expect(
      drawer().getByText("Some files were already restored: restore again before using the data."),
    ).toBeInTheDocument();
    expect(drawer().getByRole("button", { name: "Restore again" })).toBeInTheDocument();
    // the containers were started again
    expect(steps.getAllByRole("listitem")[3]).toHaveClass("done");
  });

  it("shows the stop the panel was asked for, after a reload", async () => {
    live.records["backup_restores/r1"] = restore({
      status: "running",
      command: "c1",
      stop_requested_label: "admin@example.com",
    });
    live.records["commands/c1"] = command({
      cancel_requested_at: "2026-11-09 11:59:00.000Z",
      progress: { step: "cancelling", percent: -1 },
    });
    showDrawer("operator", { restore: "r1" });
    expect(
      await drawer().findByText("Stopping: the agent starts the containers again first."),
    ).toBeInTheDocument();
  });

  it("the progress bar never turns red", async () => {
    live.records["backup_restores/r1"] = restore({ status: "running", command: "c1" });
    live.records["commands/c1"] = command({
      progress: { step: "restore", percent: 95, done: 1.14e9, total: 1.2e9, unit: "bytes" },
    });
    showDrawer("operator", { restore: "r1" });
    const bar = await drawer().findByRole("progressbar", { name: "Progress of the restore" });
    expect(bar).toHaveAttribute("aria-valuenow", "95");
    expect(bar).not.toHaveClass("warn");
    expect(bar).not.toHaveClass("err");
    // nothing stops: no step stops or starts containers
    const steps = within(drawer().getByRole("list", { name: "Steps of the restore" }));
    expect(steps.getAllByRole("listitem").map((item) => item.textContent)).toEqual([
      "Check the snapshot",
      "Restore the files",
      "Check the containers",
    ]);
  });

  it("says how a restore ended, and who did not stay up", async () => {
    live.records["backup_restores/r1"] = restore({
      status: "succeeded",
      command: "c1",
      files_restored: 1200,
      bytes_restored: 1.2e9,
      app_stopped: [{ id: "a1", name: "vaultwarden-app-1" }],
      app_problem: "vaultwarden-app-1 did not stay up after the restore.",
    });
    live.records["commands/c1"] = command({
      status: "succeeded",
      result: { app_not_running: ["vaultwarden-app-1"] },
    });
    showDrawer("operator", { restore: "r1" });
    expect(await drawer().findByText(/^Restored 1200 files \(1\.2 GB\)\./)).toBeInTheDocument();
    expect(drawer().queryByText(/The containers run again/)).not.toBeInTheDocument();
    expect(
      drawer().getByText("vaultwarden-app-1 did not stay up: look at its logs."),
    ).toBeInTheDocument();
    expect(drawer().queryByRole("button", { name: "Stop the restore" })).not.toBeInTheDocument();
    expect(drawer().queryByRole("button", { name: "Restore again" })).not.toBeInTheDocument();
    expect(drawer().getByRole("button", { name: "Close" })).toBeInTheDocument();
  });

  it("says that the containers run again after a restore that went well", async () => {
    live.records["backup_restores/r1"] = restore({
      status: "succeeded",
      command: "c1",
      files_restored: 1,
      bytes_restored: 812,
      app_stopped: [{ id: "a1", name: "vaultwarden-app-1" }],
    });
    showDrawer("operator", { restore: "r1" });
    expect(
      await drawer().findByText("Restored 1 file (812 B). The containers run again."),
    ).toBeInTheDocument();
  });

  it("offers to restore again after a failure", async () => {
    live.records["backup_restores/r1"] = restore({
      status: "failed",
      command: "c1",
      error: "Fatal: unable to open repository.",
    });
    const { onRecord } = showDrawer("operator", { restore: "r1" });
    expect(
      await drawer().findByText("The restore failed: Fatal: unable to open repository."),
    ).toBeInTheDocument();
    fireEvent.click(drawer().getByRole("button", { name: "Restore again" }));
    expect(onRecord).toHaveBeenLastCalledWith(null);
    expect(await drawer().findByRole("button", { name: "Preview" })).toBeInTheDocument();
  });

  it("says that a restore stopped before it wrote changed nothing", async () => {
    // cancelled in the queue, in the panel's words
    live.records["backup_restores/r1"] = restore({
      status: "cancelled",
      command: "c1",
      error: "Cancelled before it started: nothing was stopped or changed.",
    });
    const { view } = showDrawer("operator", { restore: "r1" });
    expect(
      await drawer().findByText("Cancelled before it started: nothing was stopped or changed."),
    ).toBeInTheDocument();
    expect(drawer().queryByText(/keeps what was restored so far/)).not.toBeInTheDocument();
    expect(drawer().queryByText(/Some files were already restored/)).not.toBeInTheDocument();
    view.unmount();

    // stopped under way before it wrote
    live.records["backup_restores/r1"] = restore({
      status: "cancelled",
      command: "c1",
      error: "Stopped by admin@example.com before it changed anything.",
    });
    const { view: again } = showDrawer("operator", { restore: "r1" });
    expect(
      await drawer().findByText("Stopped by admin@example.com before it changed anything."),
    ).toBeInTheDocument();
    expect(drawer().queryByText(/keeps what was restored so far/)).not.toBeInTheDocument();
    again.unmount();

    // with no words of the panel's
    live.records["backup_restores/r1"] = restore({ status: "cancelled", command: "c1" });
    showDrawer("operator", { restore: "r1" });
    expect(
      await drawer().findByText("The restore was stopped before it changed anything."),
    ).toBeInTheDocument();
    expect(drawer().queryByText(/keeps what was restored so far/)).not.toBeInTheDocument();
  });

  it("says when a preview failed", async () => {
    live.records["backup_restores/r1"] = restore({
      status: "failed",
      error: "Fatal: wrong password or no key found.",
    });
    showDrawer("operator", { restore: "r1" });
    expect(
      await drawer().findByText("The preview failed: Fatal: wrong password or no key found."),
    ).toBeInTheDocument();
    expect(drawer().getByRole("button", { name: "Preview" })).toBeEnabled();
  });

  it("takes the page's job and snapshot once the jobs have loaded", async () => {
    signInAs("operator");
    vi.spyOn(restoresApi, "snapshots").mockResolvedValue(snapshots(2));
    // the page opens the drawer from the address before its lists are there
    function Page() {
      const [loaded, setLoaded] = useState<typeof jobs>([]);
      return (
        <>
          <button type="button" onClick={() => setLoaded(jobs)}>
            load
          </button>
          <RestoreDrawer
            jobs={loaded}
            targets={targets}
            hosts={hosts}
            runs={[]}
            stacks={[]}
            start={{ job: loaded.length > 0 ? "j1" : undefined, snapshot: first, restore: "new" }}
            onRecord={vi.fn()}
            onClose={vi.fn()}
          />
        </>
      );
    }
    wrap(<Page />);
    expect(drawer().getByRole("heading", { name: "Restore from backup" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "load", hidden: true }));
    expect(await screen.findByRole("heading", { name: "Restore vaultwarden" })).toBeInTheDocument();
    expect(await drawer().findByText("Time")).toBeInTheDocument();
  });

  it("picks the job, the target and the snapshot when none is given", async () => {
    signInAs("operator");
    const list = vi.spyOn(restoresApi, "snapshots").mockResolvedValue(snapshots(2));
    wrap(
      <RestoreDrawer
        jobs={jobs}
        targets={targets}
        hosts={hosts}
        runs={[]}
        stacks={[]}
        start={{ restore: "new" }}
        onRecord={vi.fn()}
        onClose={vi.fn()}
      />,
    );
    expect(drawer().getByRole("heading", { name: "Restore from backup" })).toBeInTheDocument();
    fireEvent.change(drawer().getByLabelText("Job"), { target: { value: "j1" } });
    expect(drawer().getByLabelText("Target")).toHaveValue("t1");
    await waitFor(() => expect(list).toHaveBeenCalledWith("j1", "t1", false));
    const snapshot = drawer().getByLabelText("Snapshot");
    await waitFor(() => expect(within(snapshot).getAllByRole("option")).toHaveLength(3));
    expect(drawer().getByRole("button", { name: "Preview" })).toBeDisabled();
    fireEvent.change(snapshot, { target: { value: first } });
    expect(await drawer().findByText("Time")).toBeInTheDocument();
    expect(drawer().getByRole("button", { name: "Preview" })).toBeEnabled();
  });
});

describe("the restore on the Backups page", () => {
  function showPage(role: User["role"], path: string) {
    signInAs(role);
    vi.spyOn(restoresApi, "snapshots").mockResolvedValue(snapshots(2));
    live.lists = {
      backup_targets: targets,
      hosts,
      backup_jobs: jobs,
      backup_runs: [run("r1", {})],
      stacks: [],
    };
    wrap(<BackupsPage />, path);
  }

  it("opens a snapshot's restore from its row, and closes it", async () => {
    showPage("operator", "/backups?job=j1");
    const card = within(screen.getByRole("heading", { name: /^Snapshots · / }).closest("section")!);
    fireEvent.click(await card.findByRole("button", { name: "Restore snapshot 3f9a1c2e" }));
    expect(screen.getByTestId("location")).toHaveTextContent(
      `/backups?job=j1&restore=new&snapshot=${first}`,
    );
    expect(await screen.findByRole("heading", { name: "Restore vaultwarden" })).toBeInTheDocument();
    expect(await drawer().findByText("Time")).toBeInTheDocument();
    fireEvent.click(drawer().getByRole("button", { name: "Cancel" }));
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/backups\?job=j1$/);
  });

  it("opens a snapshot's files from its row", async () => {
    vi.spyOn(restoresApi, "files").mockResolvedValue(folder("/volumes/vw_data"));
    showPage("operator", "/backups?job=j1");
    const card = within(screen.getByRole("heading", { name: /^Snapshots · / }).closest("section")!);
    fireEvent.click(
      await card.findByRole("button", { name: "Browse the files of snapshot 3f9a1c2e" }),
    );
    expect(
      await screen.findByRole("navigation", { name: "Path in the snapshot" }),
    ).toBeInTheDocument();
  });

  it("shows a restore again from the address", async () => {
    live.records["backup_restores/r7"] = restore({
      id: "r7",
      status: "running",
      command: "c1",
    });
    showPage("operator", "/backups?restore=r7");
    expect(await screen.findByRole("heading", { name: "Restore vaultwarden" })).toBeInTheDocument();
    expect(await drawer().findByText("Progress of the restore")).toBeInTheDocument();
  });

  it("opens nothing for a viewer", async () => {
    // the drawer would have loaded by the time the snapshots are listed
    await import("./BackupRestore");
    showPage("viewer", "/backups?restore=new");
    expect(await screen.findByText("newest")).toBeInTheDocument();
    await act(() => new Promise((resolve) => setTimeout(resolve, 50)));
    expect(screen.queryByRole("button", { name: "Restore from backup" })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /^Restore / })).not.toBeInTheDocument();
  });
});

describe("the panel's own backup in the restore drawer (#32)", () => {
  afterEach(async () => {
    cleanup();
    await i18n.changeLanguage("en");
  });

  const panel = job({
    id: "jp",
    name: "panel (pb_data)",
    host: "h1",
    source_kind: "panel",
    source: "",
    targets: ["t3"],
  });
  // two of the servers dial an IP address, one a name
  const panelHosts = hosts.map((host, index) => ({
    ...host,
    panel_address: index < 2 ? "192.0.2.2:9443" : "panel.example.com:9443",
  }));
  const info = {
    version: "0.32.0",
    public_url: "http://192.0.2.2:8090",
    grpc_address: "panel.example.com:9443",
    gateway_running: true,
    ca_pin: "sha256:ab",
    agent_image: "ghcr.io/oldbear24/pocketcloud-agent@sha256:5f1c",
    updater_image: "",
    images_pinned: true,
    agents: { connected: 3, total: 3 },
  } as PanelInfo;

  function showPanel(role: User["role"], start: RestoreStart = {}) {
    signInAs(role);
    vi.spyOn(api, "info").mockResolvedValue(info);
    vi.spyOn(restoresApi, "snapshots").mockResolvedValue(snapshots(2, { target: "t3" }));
    wrap(
      <RestoreDrawer
        jobs={[...jobs, panel]}
        targets={targets}
        hosts={panelHosts}
        runs={[]}
        stacks={[]}
        start={{ job: "jp", target: "t3", snapshot: first, ...start }}
        onRecord={vi.fn()}
        onClose={vi.fn()}
      />,
    );
  }

  const step1 = "1 · Fetch the snapshot, on a server with an agent";

  it("gives the steps, filled from the snapshot, the target, the images and the address, and no modes", async () => {
    showPanel("admin");
    expect(await drawer().findByText("Time")).toBeInTheDocument();
    expect(drawer().getByRole("heading", { name: "Restore the panel" })).toBeInTheDocument();
    // no restore here: neither its modes nor a preview
    expect(drawer().queryByRole("radiogroup")).not.toBeInTheDocument();
    expect(drawer().queryByRole("button", { name: "Preview" })).not.toBeInTheDocument();
    expect(
      drawer().getByText(
        "The panel is restored on a machine of its own, with its recovery kit: see the steps.",
      ),
    ).toBeInTheDocument();
    expect(
      await drawer().findByText(/where the servers dial it \(192\.0\.2\.2:9443\): they reconnect/),
    ).toBeInTheDocument();

    await waitFor(() =>
      expect(drawer().getByRole("region", { name: step1 }).textContent).toBe(
        [
          "read -rs POCKETCLOUD_KIT_PASSPHRASE && export POCKETCLOUD_KIT_PASSPHRASE",
          "docker exec -i -e POCKETCLOUD_KIT_PASSPHRASE pocketcloud-agent \\",
          "  pocketcloud-agent panel-recovery fetch --kit - \\",
          "  --target backup-node --snapshot 3f9a1c2e --out - \\",
          "  < kit.pckit > pocketcloud-panel.pcpb.part \\",
          "  && mv pocketcloud-panel.pcpb.part pocketcloud-panel.pcpb",
        ].join("\n"),
      ),
    );
    expect(
      drawer().getByRole("region", { name: "Or on the new machine, through the agent's image:" })
        .textContent,
    ).toBe(
      [
        "docker run --rm -it \\",
        "  -v /var/run/docker.sock:/var/run/docker.sock \\",
        '  -v "$PWD":/work ghcr.io/oldbear24/pocketcloud-agent@sha256:5f1c \\',
        "  panel-recovery fetch --kit /work/kit.pckit \\",
        "  --target backup-node --snapshot 3f9a1c2e \\",
        "  --out /work/pocketcloud-panel.pcpb",
      ].join("\n"),
    );
    expect(
      drawer().getByRole("region", { name: "2 · Restore it into the new panel's volumes" })
        .textContent,
    ).toBe(
      [
        "docker run --rm -it -v pocketcloud:/data \\",
        '  -v pocketcloud-secrets:/run/secrets -v "$PWD":/work \\',
        "  ghcr.io/oldbear24/pocketcloud-panel:0.32.0 recover \\",
        "  --kit /work/kit.pckit --from /work/pocketcloud-panel.pcpb \\",
        "  --env-out /work/panel.env",
      ].join("\n"),
    );
    expect(
      drawer().getByRole("region", {
        name: "3 · Check it and start the panel at the same address",
      }).textContent,
    ).toBe(
      [
        "docker run --rm -v pocketcloud:/data \\",
        "  -v pocketcloud-secrets:/run/secrets:ro \\",
        "  ghcr.io/oldbear24/pocketcloud-panel:0.32.0 audit verify",
        "docker run -d --name pocketcloud-panel --restart unless-stopped \\",
        "  --env-file panel.env -p 8090:8090 -p 9443:9443 \\",
        "  -v pocketcloud:/data -v pocketcloud-secrets:/run/secrets:ro \\",
        "  ghcr.io/oldbear24/pocketcloud-panel:0.32.0",
      ].join("\n"),
    );
    expect(
      drawer().getByText("4 · In Settings › Recovery kit, watch the servers come back"),
    ).toBeInTheDocument();
    expect(drawer().getByRole("link", { name: "Recovery kit" })).toHaveAttribute(
      "href",
      "/settings#recovery-kit",
    );
    expect(drawer().getByText("Recovery guide: docs/disaster-recovery.md")).toBeInTheDocument();
  });

  it("fetches the newest snapshot before one is picked", async () => {
    showPanel("operator", { snapshot: "" });
    expect(await drawer().findByRole("heading", { name: "Restore the panel" })).toBeInTheDocument();
    await waitFor(() =>
      expect(drawer().getByRole("region", { name: step1 })).toHaveTextContent("--snapshot latest"),
    );
    expect(drawer().queryByText("Time")).not.toBeInTheDocument();
  });

  it("downloads the encrypted copy for an admin, as panel recover takes it", async () => {
    const download = vi.spyOn(restoresApi, "download").mockResolvedValue({
      restore: restore({
        id: "d1",
        job: "jp",
        target: "t3",
        mode: "download",
        status: "queued",
        path: "/pocketcloud-panel.pcpb",
        download_name: "pocketcloud-panel.pcpb",
        download_kind: "file",
      }),
      url: "/api/pc/backup-downloads/d1?ticket=ab12",
    });
    showPanel("admin");
    const button = await drawer().findByRole("button", { name: "Download the encrypted copy" });
    await waitFor(() => expect(button).toBeEnabled());
    fireEvent.click(button);
    await waitFor(() =>
      expect(download).toHaveBeenCalledWith("jp", {
        target: "t3",
        snapshot: first,
        path: "/pocketcloud-panel.pcpb",
        kind: "file",
      }),
    );
    expect(document.querySelector("iframe")?.getAttribute("src")).toBe(
      "/api/pc/backup-downloads/d1?ticket=ab12",
    );
  });

  it("gives an operator the steps, but not the copy", async () => {
    showPanel("operator");
    expect(await drawer().findByRole("heading", { name: "Restore the panel" })).toBeInTheDocument();
    expect(
      drawer().queryByRole("button", { name: "Download the encrypted copy" }),
    ).not.toBeInTheDocument();
    expect(drawer().getByRole("button", { name: "Cancel" })).toBeInTheDocument();
  });

  it("says the steps in Czech", async () => {
    await act(() => i18n.changeLanguage("cs"));
    showPanel("admin");
    expect(await drawer().findByRole("heading", { name: "Obnova panelu" })).toBeInTheDocument();
    expect(
      drawer().getByText("Panel se obnovuje na vlastním stroji se záchrannou sadou: viz postup."),
    ).toBeInTheDocument();
    expect(drawer().getByText("1 · Stáhni snapshot na serveru s agentem")).toBeInTheDocument();
    expect(
      await drawer().findByRole("button", { name: "Stáhnout šifrovanou kopii" }),
    ).toBeInTheDocument();
  });
});

describe("a database's restore in the drawer (#53)", () => {
  const shopJob = job({
    id: "j9",
    name: "db-main-shop",
    host: "h1",
    source_kind: "database",
    source: "shop",
    engine: "postgres",
    targets: ["t1"],
    origin: "database",
    database: "db1",
    db_server_name: "db-main",
  });
  const able = hosts.map((host) =>
    host.id === "h1" ? { ...host, capabilities: ["backup.restore", "backup.database"] } : host,
  );
  const stacks = [{ id: "s1", name: "shop", host: "h1" }];

  function showDatabase(
    role: User["role"],
    fields: Partial<typeof shopJob> = {},
    start: RestoreStart = {},
    serverHosts = able,
  ) {
    signInAs(role);
    vi.spyOn(restoresApi, "snapshots").mockResolvedValue(snapshots(3));
    live.records["databases/db1"] ??= { id: "db1", stack: "s1", site: "" };
    wrap(
      <RestoreDrawer
        jobs={[{ ...shopJob, ...fields }]}
        targets={targets}
        hosts={serverHosts}
        runs={[run("r1", { job: "j9" })]}
        stacks={stacks}
        start={{ job: "j9", target: "t1", snapshot: first, ...start }}
        onRecord={vi.fn()}
        onClose={vi.fn()}
      />,
    );
  }

  const dbRoots = [
    {
      source: "/shop.pgdump",
      container: "pcdb-main-db-1",
      engine: "postgres",
      admin_user: "postgres",
      database: "shop",
      user: "shop",
      scratch: "pc_rs_r1aaaaaaaaaaaaa",
      previous: "pc_old_r1aaaaaaaaaaaaa",
    },
  ];

  function dbPreview(fields: Partial<ReturnType<typeof preview>> = {}) {
    return preview({
      snapshot: { id: first, time: "2026-11-09T11:00:00Z", paths: ["/shop.pgdump"] },
      roots: [
        {
          ...preview().roots[0],
          source: "/shop.pgdump",
          destination: "database shop",
          entries: 12,
        },
      ],
      would_stop: [{ name: "shop-web-1", project: "shop", service: "web" }],
      total_files: 1,
      total_bytes: 1048576,
      database: {
        exists: true,
        user_exists: true,
        size_bytes: 8388608,
        tables: 12,
        connections_other: 0,
        sign_in: true,
        settings: [],
        sandbox: false,
      },
      free: { data: 53687091200, tmp: 53687091200 },
      needed: { data: 10603200, tmp: 269484032 },
      ...fields,
    });
  }

  function dbRecord(fields: Partial<BackupRestore> = {}): BackupRestore {
    return previewed({
      job: "j9",
      host: "h1",
      mode: "database",
      database: "db1",
      database_name: "shop",
      previous: "pc_old_r1aaaaaaaaaaaaa",
      roots: dbRoots,
      preview: dbPreview(),
      ...fields,
    });
  }

  it("goes back into the database or into a new one for an operator, nothing to pick", async () => {
    const create = vi.spyOn(restoresApi, "create").mockResolvedValue(restore({ job: "j9" }));
    showDatabase("operator");

    const back = await drawer().findByRole("radio", { name: /Back into the database/ });
    expect(back).toHaveAttribute("aria-checked", "true");
    expect(back).toHaveTextContent(
      "shop on db-main is replaced by the backup. shop stops for a while; the other databases on db-main are not touched.",
    );
    expect(drawer().getByRole("radio", { name: /Into a new database/ })).toHaveTextContent(
      "On db-main, with a user and password of its own: connect it to a stack once it is ready. Nothing else changes.",
    );
    expect(drawer().getByRole("radio", { name: /To a new folder/ })).toBeDisabled();
    expect(drawer().getByRole("radio", { name: /Download/ })).toBeDisabled();
    expect(drawer().queryByRole("radio", { name: /original location/ })).not.toBeInTheDocument();
    // the snapshot holds the one dump
    expect(drawer().queryByText("The whole snapshot")).not.toBeInTheDocument();
    expect(drawer().queryByRole("button", { name: /Choose/ })).not.toBeInTheDocument();
    expect(drawer().queryByRole("checkbox")).not.toBeInTheDocument();

    fireEvent.click(drawer().getByRole("button", { name: "Preview" }));
    await waitFor(() =>
      expect(create).toHaveBeenCalledWith("j9", {
        target: "t1",
        snapshot: first,
        mode: "database",
      }),
    );
  });

  it("says that no application stops when the database has none", async () => {
    live.records["databases/db1"] = { id: "db1", stack: "", site: "" };
    showDatabase("operator");
    expect(
      await drawer().findByRole("radio", { name: /Back into the database/ }),
    ).toHaveTextContent(
      "shop on db-main is replaced by the backup. No application stops for a while;",
    );
  });

  it("restores a dropped database into a new one, on a shared server of its engine", async () => {
    live.lists.db_servers = [
      { id: "s1", name: "db-main", engine: "postgres", kind: "shared", host: "h1" },
      { id: "s2", name: "maria", engine: "mariadb", kind: "shared", host: "h1" },
      { id: "s3", name: "pg-nas", engine: "postgres", kind: "shared", host: "h2" },
    ];
    const create = vi.spyOn(restoresApi, "create").mockResolvedValue(restore({ job: "j9" }));
    showDatabase("operator", { database: "" });

    const back = await drawer().findByRole("radio", { name: /Back into the database/ });
    expect(back).toBeDisabled();
    expect(back).toHaveTextContent("The database was dropped: restore it into a new one.");
    expect(drawer().getByRole("radio", { name: /Into a new database/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    const server = drawer().getByLabelText("Database server");
    expect(
      within(server)
        .getAllByRole("option")
        .map((option) => option.textContent),
    ).toEqual(["db-main"]);
    fireEvent.change(drawer().getByLabelText("Name of the new database"), {
      target: { value: "shop_restored" },
    });
    fireEvent.click(drawer().getByRole("button", { name: "Preview" }));
    await waitFor(() =>
      expect(create).toHaveBeenCalledWith("j9", {
        target: "t1",
        snapshot: first,
        mode: "new_database",
        name: "shop_restored",
        server: "s1",
      }),
    );
  });

  it("waits for an agent that restores databases", async () => {
    const old = able.map((host) =>
      host.id === "h1" ? { ...host, capabilities: ["backup.restore"] } : host,
    );
    showDatabase("admin", {}, {}, old);
    for (const name of [/Back into the database/, /Into a new database/]) {
      const radio = await drawer().findByRole("radio", { name });
      expect(radio).toBeDisabled();
      expect(radio).toHaveTextContent(
        "The agent of pve-01 cannot restore databases yet: update it.",
      );
    }
    expect(drawer().getByRole("radio", { name: /To a new folder/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
  });

  it("takes a new name only as the panel does, never one of the temporary namespace", async () => {
    const create = vi.spyOn(restoresApi, "create").mockResolvedValue(restore({ job: "j9" }));
    showDatabase("operator");
    fireEvent.click(await drawer().findByRole("radio", { name: /Into a new database/ }));
    const name = drawer().getByLabelText("Name of the new database");
    expect(
      drawer().getByText(
        "Lowercase letters, digits and _, starting with a letter; at most 63. Its user gets the same name.",
      ),
    ).toBeInTheDocument();
    const previewButton = drawer().getByRole("button", { name: "Preview" });
    expect(previewButton).toBeDisabled();
    for (const value of ["Shop", "1shop", "shop-copy", "a".repeat(64)]) {
      fireEvent.change(name, { target: { value } });
      expect(
        drawer().getByText("Start with a letter, then lower case letters, digits and underscores."),
      ).toBeInTheDocument();
      expect(previewButton).toBeDisabled();
    }
    for (const value of ["pc_rt_abcdefghijklmno", "pc_rs_x", "pc_old_shop"]) {
      fireEvent.change(name, { target: { value } });
      expect(
        drawer().getByText(
          "Names that start with pc_rt_, pc_rs_ or pc_old_ are PocketCloud's own temporary databases.",
        ),
      ).toBeInTheDocument();
      expect(previewButton).toBeDisabled();
    }
    fireEvent.change(name, { target: { value: "shop_copy" } });
    expect(name).toHaveAttribute("aria-invalid", "false");
    expect(previewButton).toBeEnabled();
    fireEvent.click(previewButton);
    await waitFor(() =>
      expect(create).toHaveBeenCalledWith("j9", {
        target: "t1",
        snapshot: first,
        mode: "new_database",
        name: "shop_copy",
      }),
    );
  });

  it("shows what the preview found: the dump, the tables, the room, what stops and the copy kept", async () => {
    live.records["backup_restores/r1"] = dbRecord();
    showDatabase("operator", {}, { restore: "r1" });
    expect(await drawer().findByText("The backup holds a dump of 1 MB")).toBeInTheDocument();
    expect(drawer().getByText("shop has 12 tables now")).toBeInTheDocument();
    expect(
      drawer().getByText("53.7 GB free on db-main; the restore needs about 10.6 MB"),
    ).toBeInTheDocument();
    expect(
      drawer().getByText("Also stops shop-web-1, whose settings use this database."),
    ).toBeInTheDocument();
    expect(
      drawer().getByText(
        "The data as it is now stays on db-main as pc_old_r1aaaaaaaaaaaaa for 24 hours.",
      ),
    ).toBeInTheDocument();
    expect(drawer().getByRole("button", { name: "Restore…" })).toBeEnabled();
  });

  it("warns that MariaDB keeps no copy of the data as it is now", async () => {
    live.records["backup_restores/r1"] = dbRecord({
      previous: "",
      roots: [{ ...dbRoots[0], engine: "mariadb", previous: "", source: "/shop.sql" }],
    });
    showDatabase("operator", { engine: "mysql" }, { restore: "r1" });
    expect(
      await drawer().findByText(
        "No copy of the data as it is now stays: back up first if you may need it.",
      ),
    ).toBeInTheDocument();
  });

  it.each([
    {
      what: "the dump is missing",
      fields: { roots: [{ ...dbPreview().roots[0], in_snapshot: false }] },
      says: "The dump is not in this snapshot.",
    },
    {
      what: "the password no longer signs in",
      fields: { database: { ...dbPreview().database!, sign_in: false } },
      says: "The panel's password of shop no longer signs in on db-main, and the restore runs as that user.",
    },
    {
      what: "the room is short",
      fields: { free: { data: 1e6, tmp: 1e9 } },
      says: "db-main has too little room: restore into a new database elsewhere, or make room first.",
    },
  ])("starts nothing when $what", async ({ fields, says }) => {
    live.records["backup_restores/r1"] = dbRecord({ preview: dbPreview(fields) });
    showDatabase("operator", {}, { restore: "r1" });
    expect(await drawer().findByText(says)).toBeInTheDocument();
    expect(drawer().getByRole("button", { name: "Restore…" })).toBeDisabled();
  });

  it("keeps Replace disabled until the database's name is typed, and sends it", async () => {
    live.records["backup_restores/r1"] = dbRecord();
    const start = vi
      .spyOn(restoresApi, "start")
      .mockRejectedValueOnce(
        failure(409, "Type the database's name to confirm that its data is replaced."),
      )
      .mockResolvedValueOnce(dbRecord({ status: "queued", command: "c1" }));
    showDatabase("operator", {}, { restore: "r1" });

    fireEvent.click(await drawer().findByRole("button", { name: "Restore…" }));
    const dialog = within(screen.getByRole("dialog", { name: "Replace the database shop?" }));
    expect(
      dialog.getByText(
        "Everything shop holds now is replaced by the backup of Nov 9, 2026, 12:00:00 PM. 1 container stops meanwhile and starts again.",
      ),
    ).toBeInTheDocument();
    const confirm = dialog.getByRole("button", { name: "Replace shop" });
    const typed = dialog.getByLabelText("Type shop to confirm");
    expect(confirm).toBeDisabled();
    for (const value of ["", "Shop", "shop ", "sho", "blog"]) {
      fireEvent.change(typed, { target: { value } });
      expect(confirm).toBeDisabled();
      fireEvent.click(confirm);
    }
    expect(start).not.toHaveBeenCalled();

    fireEvent.change(typed, { target: { value: "shop" } });
    expect(confirm).toBeEnabled();
    fireEvent.click(confirm);
    // the panel checks the name too; its 409 stays in the dialog
    expect(
      await dialog.findByText("Type the database's name to confirm that its data is replaced."),
    ).toBeInTheDocument();
    expect(start).toHaveBeenLastCalledWith("r1", true, "shop");
    fireEvent.click(confirm);
    await waitFor(() => expect(start).toHaveBeenCalledTimes(2));
    expect(await screen.findByText("The restore of db-main-shop started.")).toBeInTheDocument();
  });

  it("starts a restore into a new database without asking, unless the name is taken", async () => {
    live.records["backup_restores/r1"] = dbRecord({
      mode: "new_database",
      database_name: "shop_copy",
      previous: "",
      roots: [{ ...dbRoots[0], database: "shop_copy", user: "shop_copy", previous: "" }],
      preview: dbPreview({
        would_stop: [],
        database: { ...dbPreview().database!, exists: false, user_exists: false, tables: 0 },
      }),
    });
    const start = vi
      .spyOn(restoresApi, "start")
      .mockResolvedValue(dbRecord({ mode: "new_database", status: "queued", command: "c1" }));
    showDatabase("operator", {}, { restore: "r1" });
    fireEvent.click(await drawer().findByRole("button", { name: "Restore" }));
    await waitFor(() => expect(start).toHaveBeenCalledWith("r1", false));
    expect(screen.queryByRole("dialog", { name: /Replace the database/ })).not.toBeInTheDocument();
    cleanup();

    live.records["backup_restores/r1"] = dbRecord({
      mode: "new_database",
      database_name: "shop_copy",
      preview: dbPreview({
        would_stop: [],
        database: { ...dbPreview().database!, exists: false, user_exists: true },
      }),
    });
    showDatabase("operator", {}, { restore: "r1" });
    expect(
      await drawer().findByText("A database named shop_copy exists on db-main already."),
    ).toBeInTheDocument();
    expect(drawer().getByRole("button", { name: "Restore" })).toBeDisabled();
  });

  it("downloads the dump as SQL, and PostgreSQL's as stored too, for an admin", async () => {
    const download = vi.spyOn(restoresApi, "download").mockResolvedValue({
      restore: restore({ id: "r5", job: "j9", mode: "download", status: "queued" }),
      url: "/api/pc/backup-downloads/r5?token=x",
    });
    showDatabase("admin");
    fireEvent.click(await drawer().findByRole("radio", { name: /Download/ }));
    expect(
      drawer().getByText(
        "Plain SQL straight to your browser, made from the backup as it streams: keep this page open until it finishes.",
      ),
    ).toBeInTheDocument();
    fireEvent.click(drawer().getByRole("button", { name: "Download as SQL" }));
    await waitFor(() =>
      expect(download).toHaveBeenCalledWith("j9", { target: "t1", snapshot: first, kind: "sql" }),
    );
    cleanup();

    showDatabase("admin");
    fireEvent.click(await drawer().findByRole("radio", { name: /Download/ }));
    fireEvent.click(drawer().getByRole("button", { name: "Download as stored, for pg_restore" }));
    await waitFor(() =>
      expect(download).toHaveBeenLastCalledWith("j9", {
        target: "t1",
        snapshot: first,
        path: "/shop.pgdump",
        kind: "file",
      }),
    );
    cleanup();

    // MariaDB's dump is SQL already
    showDatabase("admin", { engine: "mysql" });
    fireEvent.click(await drawer().findByRole("radio", { name: /Download/ }));
    expect(drawer().getByRole("button", { name: "Download as SQL" })).toBeInTheDocument();
    expect(
      drawer().queryByRole("button", { name: "Download as stored, for pg_restore" }),
    ).not.toBeInTheDocument();
  });

  it("takes its steps, and says the tables it restored", async () => {
    live.records["backup_restores/r1"] = dbRecord({
      status: "running",
      command: "c1",
    });
    live.records["commands/c1"] = command({
      host: "h1",
      progress: { step: "dbrestore", percent: 40, done: 0, total: 0, unit: "bytes" },
    });
    showDatabase("operator", {}, { restore: "r1" });
    const steps = within(await drawer().findByRole("list", { name: "Steps of the restore" }));
    expect(steps.getAllByRole("listitem").map((item) => item.textContent)).toEqual([
      "Check the snapshot",
      "Copy the dump to the database server",
      "Restore the database",
      "Stop the containers",
      "Put the restored copy in place",
      "Start the containers",
      "Check the containers",
    ]);
    cleanup();

    live.records["backup_restores/r1"] = dbRecord({
      status: "succeeded",
      command: "c1",
      tables: 12,
      app_stopped: [{ id: "a1", name: "shop-web-1" }],
    });
    live.records["commands/c1"] = command({ host: "h1", status: "succeeded" });
    showDatabase("operator", {}, { restore: "r1" });
    expect(
      await drawer().findByText(
        "12 tables restored, as many as the dump has. The containers run again.",
      ),
    ).toBeInTheDocument();
  });

  it("says what stays stopped when MariaDB's restore failed after emptying the database", async () => {
    live.records["backup_restores/r1"] = dbRecord({
      status: "failed",
      command: "c1",
      partial: true,
      previous: "",
      roots: [{ ...dbRoots[0], engine: "mariadb", previous: "" }],
      error: "The restore failed after shop was emptied.",
    });
    live.records["commands/c1"] = command({
      host: "h1",
      status: "failed",
      result: { partial: true, app_left_stopped: ["shop-web-1", "shop-worker-1"] },
    });
    showDatabase("operator", { engine: "mysql" }, { restore: "r1" });
    expect(
      await drawer().findByText(
        "shop-web-1, shop-worker-1 stay stopped: shop holds only part of the backup. Restore again, then start them on their pages.",
      ),
    ).toBeInTheDocument();
    const steps = within(drawer().getByRole("list", { name: "Steps of the restore" }));
    expect(steps.queryByText("Put the restored copy in place")).not.toBeInTheDocument();
  });
});
