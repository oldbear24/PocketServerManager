import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { ClientResponseError } from "pocketbase";
import { useState, type ReactNode } from "react";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import i18n from "../i18n";
import { restoresApi, type BackupRestore, type SnapshotFolder } from "../lib/backupRestores";
import { pb, type User } from "../lib/pb";
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
