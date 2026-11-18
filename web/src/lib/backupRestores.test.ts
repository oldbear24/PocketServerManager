import { readFileSync } from "node:fs";
import { afterEach, describe, expect, it, vi } from "vitest";

import cs from "../i18n/cs.json";
import en from "../i18n/en.json";
import i18n from "../i18n";
import { matchesFilter } from "../test-filter";
import {
  browseStart,
  downloadAsked,
  draftRecord,
  existingEntries,
  formatSnapshotTime,
  modesFor,
  needsConfirm,
  offeredModes,
  openDownloadFrame,
  openRestoreFilter,
  pathCrumbs,
  pathLabel,
  previewParts,
  previewSentence,
  restoreDraft,
  restoreKeys,
  restoreModes,
  restorePathProblem,
  roomEnough,
  restoresApi,
  restoreStatuses,
  snapshotRoots,
  snapshotSources,
  startedRestoresFilter,
  stepsOf,
  validRestorePath,
  type RestorePreview,
} from "./backupRestores";
import { pb } from "./pb";

// Monday 13:00 in Prague, on winter time
const now = Date.parse("2026-11-09T12:00:00Z");
const prague = "Europe/Prague";

afterEach(() => {
  vi.restoreAllMocks();
  document.querySelectorAll("iframe").forEach((frame) => frame.remove());
});

function preview(fields: Partial<RestorePreview> = {}): RestorePreview {
  return {
    dry_run: true,
    snapshot: {
      id: "3f9a1c2e".padEnd(64, "0"),
      time: "2026-11-09T11:00:00Z",
      paths: ["/volumes/vw_data"],
    },
    roots: [
      {
        index: 0,
        source: "/volumes/vw_data",
        destination: "vw_data",
        in_snapshot: true,
        exists: true,
        entries: 0,
        entries_capped: false,
        uid: 0,
        gid: 0,
        free_bytes: 0,
      },
    ],
    not_in_job: [],
    counts: { new: 0, updated: 0, unchanged: 0, deleted: 0 },
    bytes: { new: 0, updated: 0, deleted: 0 },
    samples: { new: [], updated: [], deleted: [] },
    truncated: { new: 0, updated: 0, deleted: 0 },
    would_stop: [],
    total_files: 0,
    total_bytes: 0,
    notes: [],
    ...fields,
  };
}

describe("a snapshot's time", () => {
  it("formats today and yesterday in Czech", () => {
    const t = i18n.getFixedT("cs");
    expect(formatSnapshotTime("2026-11-09T11:00:00Z", now, prague, t, "cs")).toBe("dnes 12:00");
    // 01:30 UTC on the 9th is already the 9th in Prague
    expect(formatSnapshotTime("2026-11-08T23:30:00Z", now, prague, t, "cs")).toBe("dnes 00:30");
    expect(formatSnapshotTime("2026-11-08T02:00:00Z", now, prague, t, "cs")).toBe("včera 03:00");
    expect(formatSnapshotTime("2026-11-01T02:00:00Z", now, prague, t, "cs")).toBe("1. 11. 03:00");
    // another year says which
    expect(formatSnapshotTime("2025-12-24T18:30:00Z", now, prague, t, "cs")).toBe(
      "24. 12. 2025 19:30",
    );
  });

  it("formats today and yesterday in English", () => {
    const t = i18n.getFixedT("en");
    expect(formatSnapshotTime("2026-11-09T11:00:00Z", now, prague, t, "en")).toBe("today 12:00");
    expect(formatSnapshotTime("2026-11-08T02:00:00Z", now, prague, t, "en")).toBe(
      "yesterday 03:00",
    );
    expect(formatSnapshotTime("2026-11-01T02:00:00Z", now, prague, t, "en")).toBe("Nov 1, 03:00");
    expect(formatSnapshotTime("", now, prague, t, "en")).toBe("–");
  });
});

describe("a preview", () => {
  const changed = preview({
    counts: { new: 3, updated: 2, unchanged: 5, deleted: 1 },
    bytes: { new: 4.2e6, updated: 1.5e6, deleted: 2e3 },
  });

  it("builds the preview with Czech plurals", async () => {
    await i18n.changeLanguage("cs");
    try {
      const t = i18n.getFixedT("cs");
      expect(previewParts(t, changed)).toEqual([
        "3 nové soubory (4,2 MB)",
        "přepíšou se 2 změněné soubory (1,5 MB)",
        "smaže se 1 soubor navíc",
        "5 souborů už odpovídá",
      ]);
      expect(previewSentence(t, changed, "cs")).toBe(
        // Czech keeps "a" with the word after it
        "3 nové soubory (4,2 MB), přepíšou se 2 změněné soubory (1,5 MB), smaže se 1 soubor navíc a\u00a05 souborů už odpovídá.",
      );
      // a sentence that starts with a verb gets its capital
      const overwrite = preview({
        counts: { new: 0, updated: 1, unchanged: 0, deleted: 0 },
        bytes: { new: 0, updated: 1e3, deleted: 0 },
      });
      expect(previewSentence(t, overwrite, "cs")).toBe("Přepíše se 1 změněný soubor (1 kB).");
    } finally {
      await i18n.changeLanguage("en");
    }
  });

  it("reads as an English sentence, and says nothing when nothing would change", () => {
    const t = i18n.getFixedT("en");
    expect(previewSentence(t, changed, "en")).toBe(
      "3 new files (4.2 MB), 2 changed files overwritten (1.5 MB), 1 extra file deleted, and 5 files already match.",
    );
    // empty files still say their size
    const empty = preview({ counts: { new: 1, updated: 0, unchanged: 0, deleted: 0 } });
    expect(previewParts(t, empty)).toEqual(["1 new file (0 B)"]);
    expect(previewSentence(t, preview(), "en")).toBe("");
  });

  it("counts what the destination holds over its roots", () => {
    const two = preview({
      roots: [
        { ...preview().roots[0], entries: 4 },
        { ...preview().roots[0], index: 1, entries: 10000, entries_capped: true },
      ],
    });
    expect(existingEntries(two)).toEqual({ count: 10004, capped: true });
    expect(existingEntries(preview())).toEqual({ count: 0, capped: false });
  });
});

describe("the overwrite confirmation", () => {
  it("is always needed at the original location", () => {
    expect(needsConfirm(preview(), "original")).toBe(true);
  });

  it("is needed in a new folder only when it holds something or files would change", () => {
    expect(needsConfirm(preview(), "path")).toBe(false);
    expect(
      needsConfirm(preview({ counts: { new: 9, updated: 0, unchanged: 0, deleted: 0 } }), "path"),
    ).toBe(false);
    expect(needsConfirm(preview({ roots: [{ ...preview().roots[0], entries: 1 }] }), "path")).toBe(
      true,
    );
    expect(
      needsConfirm(preview({ counts: { new: 0, updated: 1, unchanged: 0, deleted: 0 } }), "path"),
    ).toBe(true);
    expect(
      needsConfirm(preview({ counts: { new: 0, updated: 0, unchanged: 0, deleted: 1 } }), "path"),
    ).toBe(true);
    // no preview: nothing says it is safe
    expect(needsConfirm(null, "path")).toBe(true);
  });
});

type PathCase = { path: string; refused?: string; protected?: string };

describe("the folder a restore writes into", () => {
  const table = JSON.parse(
    readFileSync("../internal/backup/testdata/restore_paths.json", "utf8"),
  ) as { cases: PathCase[] };

  it("validRestorePath matches the Go cases", () => {
    expect(table.cases.length).toBeGreaterThan(50);
    for (const item of table.cases) {
      const problem = restorePathProblem(item.path);
      expect({ path: item.path, problem }).toEqual({
        path: item.path,
        problem: item.refused
          ? {
              refused: item.refused,
              ...(item.protected ? { protected: item.protected } : {}),
            }
          : null,
      });
      expect(validRestorePath(item.path)).toBe(!item.refused);
    }
  });

  it("counts its length in characters, as the panel does", () => {
    // each takes two UTF-16 units, and is one character
    const long = `/srv/${"😀".repeat(995)}`;
    expect([...long].length).toBe(1000);
    expect(validRestorePath(long)).toBe(true);
    expect(restorePathProblem(`${long}x`)).toEqual({ refused: "long" });
  });

  it("refuses text that is not valid UTF-8", () => {
    expect(restorePathProblem("/srv/\ud800x")).toEqual({ refused: "path" });
    expect(validRestorePath("/srv/😀")).toBe(true);
  });
});

describe("the modes of a restore", () => {
  const remote = { kind: "s3" as const, host: "h1" };
  const local = { kind: "local" as const, host: "h1" };
  const volume = { source_kind: "volume" as const, host: "h2" };

  const notDatabase = { disabled: true, reason: "notDatabase" };

  it("gives an operator the original location only", () => {
    expect(modesFor(remote, volume, "operator")).toEqual({
      original: { disabled: false },
      path: { disabled: true, reason: "admin" },
      download: { disabled: true, reason: "admin" },
      database: notDatabase,
      new_database: notDatabase,
    });
  });

  it("gives an admin every mode of files", () => {
    expect(modesFor(remote, volume, "admin")).toEqual({
      original: { disabled: false },
      path: { disabled: false },
      download: { disabled: false },
      database: notDatabase,
      new_database: notDatabase,
    });
  });

  it("offers no original location for a dump, nor from another server's local target", () => {
    expect(modesFor(remote, { ...volume, source_kind: "db_dump" }, "admin").original).toEqual({
      disabled: true,
      reason: "dump",
    });
    expect(modesFor(local, volume, "admin").original).toEqual({ disabled: true, reason: "local" });
    expect(modesFor(local, { ...volume, host: "h1" }, "admin").original).toEqual({
      disabled: false,
    });
  });

  it("gives a viewer nothing", () => {
    const modes = modesFor(remote, volume, "viewer");
    expect(Object.values(modes).every((mode) => mode.disabled)).toBe(true);
    expect(modes.original.reason).toBe("role");
  });

  it("never restores the panel into itself, from any target, for anyone (#32)", () => {
    const panel = { source_kind: "panel" as const, host: "h1" };
    for (const role of ["viewer", "operator", "admin"] as const) {
      for (const target of [remote, local]) {
        expect(modesFor(target, panel, role).original).toEqual({ disabled: true, reason: "panel" });
      }
    }
    // an admin still downloads the encrypted copy
    expect(modesFor(remote, panel, "admin").download).toEqual({ disabled: false });
    expect(modesFor(remote, panel, "operator").download).toEqual({
      disabled: true,
      reason: "admin",
    });
  });

  describe("of a database's job (#53)", () => {
    const shop = { source_kind: "database" as const, host: "h1", database: "d1" };
    const able = { capabilities: ["backup.restore", "backup.database"] };

    it("gives an operator the database and a new one, an admin a folder and a download too", () => {
      expect(modesFor(remote, shop, "operator", able)).toEqual({
        original: { disabled: true, reason: "database" },
        path: { disabled: true, reason: "admin" },
        download: { disabled: true, reason: "admin" },
        database: { disabled: false },
        new_database: { disabled: false },
      });
      expect(modesFor(remote, shop, "admin", able)).toEqual({
        original: { disabled: true, reason: "database" },
        path: { disabled: false },
        download: { disabled: false },
        database: { disabled: false },
        new_database: { disabled: false },
      });
      const viewer = modesFor(remote, shop, "viewer", able);
      expect(viewer.database).toEqual({ disabled: true, reason: "role" });
      expect(viewer.new_database).toEqual({ disabled: true, reason: "role" });
    });

    it("restores a dropped database into a new one only", () => {
      const gone = modesFor(remote, { ...shop, database: "" }, "admin", able);
      expect(gone.database).toEqual({ disabled: true, reason: "dbGone" });
      expect(gone.new_database).toEqual({ disabled: false });
    });

    it("waits for an agent that restores databases", () => {
      for (const host of [{ capabilities: ["backup.restore"] }, { capabilities: null }]) {
        const modes = modesFor(remote, shop, "admin", host);
        expect(modes.database).toEqual({ disabled: true, reason: "agent" });
        expect(modes.new_database).toEqual({ disabled: true, reason: "agent" });
        // a folder and a download need no new agent
        expect(modes.path).toEqual({ disabled: false });
      }
      // a dropped database says so first
      expect(modesFor(remote, { ...shop, database: "" }, "admin", {}).database.reason).toBe(
        "dbGone",
      );
    });

    it("restores from a local target of the database's own server only", () => {
      const other = modesFor({ kind: "local", host: "h2" }, shop, "admin", able);
      expect(other.database).toEqual({ disabled: true, reason: "local" });
      expect(other.new_database).toEqual({ disabled: true, reason: "local" });
      expect(modesFor(local, shop, "admin", able).database).toEqual({ disabled: false });
    });

    it("offers the database first, then a new one, a folder and a download", () => {
      expect(offeredModes(shop)).toEqual(["database", "new_database", "path", "download"]);
      expect(offeredModes(volume)).toEqual(["original", "path", "download"]);
    });
  });
});

describe("a database's restore (#53)", () => {
  it("is confirmed when it replaces the database, not into a new one", () => {
    expect(needsConfirm(null, "database")).toBe(true);
    expect(needsConfirm(preview(), "database")).toBe(true);
    expect(needsConfirm(null, "new_database")).toBe(false);
  });

  it("takes its steps in its own order", () => {
    expect(stepsOf("database")).toEqual([
      "check",
      "fetch",
      "dbrestore",
      "stop",
      "swap",
      "start",
      "verify",
    ]);
    expect(stepsOf("database", "mariadb")).toEqual([
      "check",
      "fetch",
      "rehearse",
      "stop",
      "dbrestore",
      "start",
      "verify",
    ]);
    expect(stepsOf("database", "postgres")).toEqual(stepsOf("database"));
    expect(stepsOf("original")).toEqual(["check", "stop", "restore", "start", "verify"]);
  });

  it("needs the room of the data and of /tmp", () => {
    expect(roomEnough({})).toBe(true);
    expect(roomEnough({ free: { data: 10, tmp: 10 }, needed: { data: 10, tmp: 10 } })).toBe(true);
    expect(roomEnough({ free: { data: 9, tmp: 10 }, needed: { data: 10, tmp: 1 } })).toBe(false);
    expect(roomEnough({ free: { data: 10, tmp: 0 }, needed: { data: 1, tmp: 1 } })).toBe(false);
  });

  it("downloads as SQL again by the kind alone, a file or a folder by its path", () => {
    expect(downloadAsked({ path: "/shop.pgdump", download_kind: "sql" })).toEqual({ kind: "sql" });
    expect(downloadAsked({ path: "/shop.pgdump", download_kind: "file" })).toEqual({
      path: "/shop.pgdump",
      kind: "file",
    });
    expect(downloadAsked({ path: "/volumes/vw_data", download_kind: "dir" })).toEqual({
      path: "/volumes/vw_data",
      kind: "dir",
    });
    // a record of an older panel says no kind: a folder, as before
    expect(downloadAsked({ path: "/volumes/vw_data", download_kind: "" })).toEqual({
      path: "/volumes/vw_data",
      kind: "dir",
    });
  });

  it("starts with the typed name, and without one as before", async () => {
    const send = vi.spyOn(pb, "send").mockResolvedValue({ id: "r1" });
    await restoresApi.start("r1", true, "shop");
    await restoresApi.start("r2", true);
    expect(send.mock.calls.map((call) => call[1]?.body)).toEqual([
      { confirm_overwrite: true, confirm_name: "shop" },
      { confirm_overwrite: true },
    ]);
  });
});

describe("a snapshot's paths", () => {
  it("are browsed from the folder that holds them all", () => {
    expect(snapshotRoots(["/volumes/b", "/volumes/a/", "/volumes/b"])).toEqual([
      "/volumes/a",
      "/volumes/b",
    ]);
    expect(browseStart(["/volumes/vw_data"])).toBe("/volumes/vw_data");
    expect(browseStart(["/volumes/notes_pgdata", "/volumes/notes_appdata"])).toBe("/volumes");
    expect(browseStart(["/srv/app", "/volumes/app"])).toBe("/");
    expect(browseStart([])).toBe("/");
  });

  it("show a volume's folder as the volume", () => {
    const t = i18n.getFixedT("en");
    expect(pathCrumbs("/volumes/vw_data/attachments/a")).toEqual([
      { path: "/", name: "", volume: false },
      { path: "/volumes/vw_data", name: "vw_data", volume: true },
      { path: "/volumes/vw_data/attachments", name: "attachments", volume: false },
      { path: "/volumes/vw_data/attachments/a", name: "a", volume: false },
    ]);
    expect(pathCrumbs("/volumes")).toEqual([
      { path: "/", name: "", volume: false },
      { path: "/volumes", name: "volumes", volume: false },
    ]);
    expect(pathCrumbs("/")).toEqual([{ path: "/", name: "", volume: false }]);
    expect(pathLabel("/volumes/vw_data", t)).toBe("Volume vw_data");
    expect(pathLabel("/srv/app", t)).toBe("/srv/app");
  });
});

describe("the targets of a job's snapshots", () => {
  const targets = [
    { id: "t1", name: "B2" },
    { id: "t2", name: "Disk" },
    { id: "t3", name: "backup-node" },
    { id: "t4", name: "Archive" },
  ];

  it("are the job's in its order, then by name those its runs went to before", () => {
    const runs = [
      { job: "j1", target: "t3" },
      { job: "j1", target: "t4" },
      { job: "j1", target: "t2" },
      { job: "j2", target: "t1" },
      // a target that was removed has no snapshots to list
      { job: "j1", target: "gone" },
    ];
    expect(snapshotSources({ id: "j1", targets: ["t2", "t1"] }, targets, runs)).toEqual([
      { target: targets[1], old: false },
      { target: targets[0], old: false },
      { target: targets[3], old: true },
      { target: targets[2], old: true },
    ]);
    expect(snapshotSources({ id: "j1", targets: null }, targets, [])).toEqual([]);
  });

  it("are no drafts once started", () => {
    expect(["previewing", "previewed", "expired"].every((s) => restoreDraft(s as never))).toBe(
      true,
    );
    expect(
      ["queued", "running", "succeeded", "failed", "cancelled"].some((s) =>
        restoreDraft(s as never),
      ),
    ).toBe(false);
  });

  it("are drafts as the panel counts them, and the list's filter leaves out the same", () => {
    const records = restoreStatuses.flatMap((status) =>
      restoreModes.flatMap((mode) =>
        ["", "c1"].map((command) => ({ job: "j1", status, mode, command })),
      ),
    );
    const drafts = records.filter((record) => draftRecord(record));
    // a preview under way, ready, too old or failed, or a download whose link was never used
    expect(drafts.filter((record) => !restoreDraft(record.status))).toEqual([
      { job: "j1", status: "queued", mode: "download", command: "" },
      { job: "j1", status: "failed", mode: "original", command: "" },
      { job: "j1", status: "failed", mode: "path", command: "" },
      // a database's preview that failed (#53)
      { job: "j1", status: "failed", mode: "database", command: "" },
      { job: "j1", status: "failed", mode: "new_database", command: "" },
    ]);
    const listed = records.filter((record) => matchesFilter(record, startedRestoresFilter("j1")));
    expect(listed).toEqual(records.filter((record) => !draftRecord(record)));
    expect(records.some((record) => matchesFilter(record, startedRestoresFilter("j2")))).toBe(
      false,
    );
    // the restore under way is one that writes
    const writes = ["original", "path", "database", "new_database"].flatMap((mode) => [mode, mode]);
    expect(
      records.filter((record) => matchesFilter(record, openRestoreFilter("j1"))).map((r) => r.mode),
    ).toEqual([...writes, ...writes]);
  });
});

type Tree = { [key: string]: string | Tree };

function has(tree: Tree, key: string): boolean {
  let node: string | Tree | undefined = tree;
  for (const part of key.split(".")) {
    node = typeof node === "object" ? node[part] : undefined;
  }
  return typeof node === "string" && node.trim() !== "";
}

describe("the keys a restore builds", () => {
  it("every mode, step and status key exists in both languages", () => {
    expect(restoreKeys).toHaveLength(5 * 2 + 9 + 8);
    expect(restoreKeys.filter((key) => !has(en, key))).toEqual([]);
    expect(restoreKeys.filter((key) => !has(cs, key))).toEqual([]);
  });
});

describe("the restores API", () => {
  it("asks for the snapshots and a folder's page with only what is set", async () => {
    const send = vi.spyOn(pb, "send").mockResolvedValue({ pending: true });
    await restoresApi.snapshots("j1", "t1");
    await restoresApi.snapshots("j1", "t1", true);
    await restoresApi.files("j1", "t1", "ab".repeat(32), { path: "/volumes/a b", match: "" });
    await restoresApi.files("j1", "t1", "ab".repeat(32), {
      path: "/",
      offset: 200,
      limit: 200,
      match: "log",
    });
    expect(send.mock.calls.map((call) => call[0])).toEqual([
      "/api/pc/backup-jobs/j1/snapshots?target=t1",
      "/api/pc/backup-jobs/j1/snapshots?target=t1&refresh=1",
      `/api/pc/backup-jobs/j1/snapshots/${"ab".repeat(32)}/files?target=t1&path=%2Fvolumes%2Fa+b`,
      `/api/pc/backup-jobs/j1/snapshots/${"ab".repeat(32)}/files?target=t1&path=%2F&offset=200&limit=200&match=log`,
    ]);
  });

  it("takes a restore as the record or as {restore}", async () => {
    const record = { id: "r1", status: "previewing" };
    const send = vi.spyOn(pb, "send").mockResolvedValueOnce(record);
    send.mockResolvedValueOnce({ restore: { id: "r1", status: "queued" } });
    const body = { target: "t1", snapshot: "ab".repeat(32), mode: "original" as const };
    expect(await restoresApi.create("j1", body)).toEqual(record);
    expect(await restoresApi.start("r1", true)).toEqual({ id: "r1", status: "queued" });
    expect(send.mock.calls).toEqual([
      ["/api/pc/backup-jobs/j1/restores", { method: "POST", body }],
      ["/api/pc/backup-restores/r1/start", { method: "POST", body: { confirm_overwrite: true } }],
    ]);
  });

  it("says when a stop waits for the agent", async () => {
    const send = vi.spyOn(pb, "send").mockResolvedValueOnce({ stopping: true });
    send.mockResolvedValueOnce({ id: "r1", status: "cancelled" });
    expect(await restoresApi.cancel("r1")).toEqual({ stopping: true });
    expect(await restoresApi.cancel("r1")).toEqual({ id: "r1", status: "cancelled" });
    expect(send).toHaveBeenCalledWith("/api/pc/backup-restores/r1/cancel", { method: "POST" });
  });

  it("discards a draft and asks for a download's link", async () => {
    const send = vi.spyOn(pb, "send").mockResolvedValueOnce(undefined);
    send.mockResolvedValueOnce({
      restore: { id: "d1" },
      url: "/api/pc/backup-downloads/d1?ticket=ab",
    });
    await restoresApi.discard("r1");
    const body = { target: "t1", snapshot: "ab".repeat(32), path: "/", kind: "dir" as const };
    expect(await restoresApi.download("j1", body)).toEqual({
      restore: { id: "d1" },
      url: "/api/pc/backup-downloads/d1?ticket=ab",
    });
    expect(send.mock.calls).toEqual([
      ["/api/pc/backup-restores/r1", { method: "DELETE" }],
      ["/api/pc/backup-jobs/j1/downloads", { method: "POST", body }],
    ]);
  });
});

describe("a download's frame", () => {
  it("is hidden, outside the app, and passes on what an answer that is no file says", () => {
    const onError = vi.fn();
    const frame = openDownloadFrame(
      "/api/pc/backup-downloads/d1?ticket=ab",
      "Download of x",
      onError,
    );
    expect(frame.hidden).toBe(true);
    expect(frame.title).toBe("Download of x");
    expect(frame.parentElement).toBe(document.body);
    expect(frame.getAttribute("src")).toBe("/api/pc/backup-downloads/d1?ticket=ab");

    // a load with nothing to read is no error
    frame.dispatchEvent(new Event("load"));
    expect(onError).not.toHaveBeenCalled();

    // the panel's 403 loads as a document in the frame: its JSON, which a
    // browser shows in a <pre>, beside controls of its own
    const used = "This download link has been used or has expired: start the download again.";
    const page = document.implementation.createHTMLDocument("");
    const pre = page.createElement("pre");
    pre.textContent = JSON.stringify({ data: {}, message: used, status: 403 });
    const controls = page.createElement("div");
    controls.innerHTML = '<label><input type="checkbox">Pretty-print</label>';
    page.body.append(pre, controls);
    Object.defineProperty(frame, "contentDocument", { value: page, configurable: true });
    frame.dispatchEvent(new Event("load"));
    expect(onError).toHaveBeenLastCalledWith(used);

    // the JSON as the document's text
    page.body.textContent = JSON.stringify({
      data: {},
      message: "Only admins download snapshots.",
      status: 403,
    });
    frame.dispatchEvent(new Event("load"));
    expect(onError).toHaveBeenLastCalledWith("Only admins download snapshots.");

    // a text/plain answer, as the panel's 502 with the agent's words
    page.body.innerHTML = "<pre>restic: snapshot 3f9a1c2e not found</pre>";
    frame.dispatchEvent(new Event("load"));
    expect(onError).toHaveBeenLastCalledWith("restic: snapshot 3f9a1c2e not found");

    // JSON that is no error of the panel's is passed on as it is
    page.body.textContent = '{"status":502}';
    frame.dispatchEvent(new Event("load"));
    expect(onError).toHaveBeenLastCalledWith('{"status":502}');
  });
});
