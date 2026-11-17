import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ClientResponseError } from "pocketbase";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { ToastProvider } from "../components/toast";
import i18n from "../i18n";
import { api } from "../lib/api";
import { AuthProvider } from "../lib/auth";
import type { BackupCheck } from "../lib/backupChecks";
import { backupsApi, type BackupTarget } from "../lib/backups";
import { pb, type User } from "../lib/pb";
import type { Host } from "../lib/types";
import { AddTargetDialog, BackupsPage, EditTargetDialog, ServerField } from "./Backups";

const now = Date.parse("2026-11-09T12:00:00Z");

const live = vi.hoisted(() => ({
  lists: {} as Record<string, unknown[]>,
  records: {} as Record<string, Record<string, unknown>>,
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
      loading: false,
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
  // the times of the checks are the browser's, here Prague's
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

function wrap(children: React.ReactNode, path = "/backups") {
  return render(
    <MemoryRouter initialEntries={[path]}>
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
    config: {
      endpoint: "https://s3.eu-central-003.backblazeb2.com",
      bucket: "homelab-backups",
      prefix: "pc",
    },
    location: "s3:homelab-backups/pc @ s3.eu-central-003.backblazeb2.com",
    repo_id: "5f1c2a",
    status: "ok",
    last_error: "",
    checked_at: "2026-11-09 11:55:00.000Z",
    usage_at: "2026-11-09 11:56:00.000Z",
    used_bytes: 1.2e12,
    capacity_bytes: 0,
    immutable_days: 30,
    note: "",
    ...fields,
  } as BackupTarget;
}

const bucket = target({});
const disk = target({
  id: "t2",
  name: "Panel disk",
  kind: "local",
  host: "h2",
  config: { path: "/srv/backups/restic" },
  location: "/srv/backups/restic",
  used_bytes: 0.3e9,
  capacity_bytes: 20e9,
  immutable_days: 0,
  note: "Only for small configurations and exports",
});
const node = target({
  id: "t3",
  name: "backup-node",
  kind: "sftp",
  host: "h1",
  config: {
    address: "backup-node",
    user: "backup",
    path: "/srv/restic",
    host_key: "ssh-ed25519 AAAAold",
  },
  location: "sftp:backup@backup-node:/srv/restic",
  status: "failed",
  last_error: "Fatal: unable to open repository: ssh: handshake failed: host key mismatch",
  usage_at: "",
  used_bytes: 0,
  immutable_days: 0,
});

function showPage(role: User["role"], targets: BackupTarget[] = [bucket, disk, node]) {
  signInAs(role);
  live.lists = { backup_targets: targets, hosts };
  wrap(<BackupsPage />);
}

function item(name: string) {
  return within(screen.getByRole("listitem", { name }));
}

describe("the backup targets", () => {
  it("lists each with its usage, where it is and how its last check went", () => {
    showPage("viewer");

    expect(screen.getByRole("heading", { name: "Backups" })).toBeInTheDocument();
    expect(
      screen.getByText(
        "restic on the agents · encrypted with a key kept in the panel · the panel schedules, the agent runs",
      ),
    ).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Targets" })).toBeInTheDocument();

    // a bucket has no capacity: no limit, and no bar
    const b2 = item("Backblaze B2");
    expect(b2.getByText("1.2 TB · no limit")).toBeInTheDocument();
    expect(
      b2.getByText("s3:homelab-backups/pc @ s3.eu-central-003.backblazeb2.com"),
    ).toBeInTheDocument();
    expect(b2.queryByRole("progressbar")).not.toBeInTheDocument();
    expect(b2.getByText("Object Lock 30 days – protected against deletion")).toBeInTheDocument();
    expect(b2.getByText("OK")).toBeInTheDocument();
    expect(b2.getByText(/^through the agent of pve-01 · checked /)).toBeInTheDocument();

    // a directory on a server knows its capacity
    const local = item("Panel disk");
    expect(local.getByText("0.3 / 20 GB")).toBeInTheDocument();
    expect(local.getByRole("progressbar", { name: "Space used at Panel disk" })).toHaveAttribute(
      "aria-valuenow",
      "2",
    );
    expect(local.getByText("Only for small configurations and exports")).toBeInTheDocument();
    expect(local.getByText(/^on nas · checked /)).toBeInTheDocument();
    expect(local.queryByText(/Object Lock/)).not.toBeInTheDocument();

    // a failed check says why
    const sftp = item("backup-node");
    expect(sftp.getByText("Check failed")).toBeInTheDocument();
    expect(
      sftp.getByText("Fatal: unable to open repository: ssh: handshake failed: host key mismatch"),
    ).toBeInTheDocument();
    // never opened, never measured: nothing on the right
    expect(sftp.queryByText(/ B$|no limit/)).not.toBeInTheDocument();
  });

  it("shows a target the agent did not answer for as pending, with why", () => {
    const waiting = target({
      id: "t4",
      name: "Wasabi",
      status: "pending",
      last_error: "The agent of pve-01 did not answer in time.",
      checked_at: "",
      usage_at: "",
      used_bytes: 0,
      immutable_days: 0,
    });
    showPage("admin", [waiting]);

    const row = item("Wasabi");
    expect(row.getByText("Not checked yet").closest(".pill")).toHaveClass("info");
    expect(row.getByText("The agent of pve-01 did not answer in time.")).toBeInTheDocument();
    expect(row.getByText("through the agent of pve-01")).toBeInTheDocument();
    expect(row.queryByText(/ B$|no limit/)).not.toBeInTheDocument();
    expect(row.getByRole("button", { name: "Refresh Wasabi" })).toBeEnabled();
  });

  it("measures the usage after the repository opened", () => {
    showPage("viewer", [target({ used_bytes: 0, usage_at: "" })]);
    expect(item("Backblaze B2").queryByText(/no limit/)).not.toBeInTheDocument();
    cleanup();
    showPage("viewer", [target({ used_bytes: 0 })]);
    expect(item("Backblaze B2").getByText("0 B · no limit")).toBeInTheDocument();
  });

  it.each(["viewer", "operator"] as const)("shows no actions to a %s", (role) => {
    showPage(role);
    expect(
      within(screen.getByRole("list", { name: "Targets" })).getAllByRole("listitem"),
    ).toHaveLength(3);
    expect(screen.queryByRole("button", { name: "Add target" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Refresh / })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Edit / })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Remove / })).not.toBeInTheDocument();
  });

  it("gives an admin the actions of each target", () => {
    showPage("admin");
    for (const name of ["Backblaze B2", "Panel disk", "backup-node"]) {
      for (const action of ["Refresh", "Edit", "Remove"]) {
        expect(screen.getByRole("button", { name: `${action} ${name}` })).toBeInTheDocument();
      }
    }
  });

  it("says what a target is when there is none", () => {
    showPage("admin", []);
    expect(screen.getByText("No targets yet")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add target" })).toBeInTheDocument();
  });

  it("lets an admin open the dialog to add one", () => {
    showPage("admin");
    fireEvent.click(screen.getByRole("button", { name: "Add target" }));
    expect(screen.getByRole("dialog", { name: "Add a backup target" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "nas (offline)" })).toBeInTheDocument();
  });

  it("removes a target after a confirmation that its data stays, but cannot be restored", async () => {
    showPage("admin");
    const send = vi.spyOn(pb, "send").mockResolvedValue(undefined);

    fireEvent.click(screen.getByRole("button", { name: "Remove Backblaze B2" }));
    const dialog = screen.getByRole("dialog", { name: "Remove Backblaze B2?" });
    expect(
      within(dialog).getByText(
        /snapshots stay where they are, but PocketCloud can no longer restore from them/,
      ),
    ).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Remove" })).toHaveClass("danger");
    expect(send).not.toHaveBeenCalled();

    fireEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    await waitFor(() =>
      expect(send).toHaveBeenCalledWith("/api/pc/backup-targets/t1", { method: "DELETE" }),
    );
    expect(await screen.findByText("The target Backblaze B2 was removed.")).toBeInTheDocument();
  });

  it("leaves another removal's confirmation open when an earlier one ends", async () => {
    showPage("admin");
    let done: () => void = () => {};
    vi.spyOn(pb, "send").mockReturnValue(
      new Promise((resolve) => {
        done = () => resolve(undefined);
      }),
    );

    fireEvent.click(screen.getByRole("button", { name: "Remove Backblaze B2" }));
    fireEvent.click(
      within(screen.getByRole("dialog", { name: "Remove Backblaze B2?" })).getByRole("button", {
        name: "Remove",
      }),
    );
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove Panel disk" }));
    expect(screen.getByRole("dialog", { name: "Remove Panel disk?" })).toBeInTheDocument();

    done();
    expect(await screen.findByText("The target Backblaze B2 was removed.")).toBeInTheDocument();
    expect(screen.getByRole("dialog", { name: "Remove Panel disk?" })).toBeInTheDocument();
  });

  it("leaves the dialog opened since when an add closed earlier ends", async () => {
    showPage("admin");
    let done: (target: BackupTarget) => void = () => {};
    vi.spyOn(backupsApi, "add").mockReturnValue(
      new Promise((resolve) => {
        done = resolve;
      }),
    );

    fireEvent.click(screen.getByRole("button", { name: "Add target" }));
    fireEvent.change(screen.getByLabelText("Kind"), { target: { value: "local" } });
    type("Name", "Panel disk");
    fireEvent.change(screen.getByLabelText("Server"), { target: { value: "h2" } });
    type("Directory", "/srv/backups/restic");
    fireEvent.click(screen.getAllByRole("button", { name: "Add target" }).at(-1)!);
    await screen.findByRole("button", { name: "Checking with restic…" });
    // closed while restic runs, and opened again for another
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog", { name: "Add a backup target" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Add target" }));
    type("Name", "Second copy");

    done(disk);
    expect(await screen.findByText("The target Panel disk was added.")).toBeInTheDocument();
    expect(screen.getByRole("dialog", { name: "Add a backup target" })).toBeInTheDocument();
    expect(screen.getByLabelText("Name")).toHaveValue("Second copy");
  });

  it("closes the add dialog when the agent did not answer, and the panel kept the target", async () => {
    const kept =
      "The agent of pve-01 did not answer in time. The target is kept, waiting: check it again to finish adding it, or remove it.";
    const list = [bucket];
    signInAs("admin");
    live.lists = { backup_targets: list, hosts };
    wrap(<BackupsPage />);
    let fail: (error: unknown) => void = () => {};
    vi.spyOn(backupsApi, "add").mockReturnValue(
      new Promise((_, reject) => {
        fail = reject;
      }),
    );

    fireEvent.click(screen.getByRole("button", { name: "Add target" }));
    fireEvent.change(screen.getByLabelText("Kind"), { target: { value: "local" } });
    type("Name", "Panel disk");
    fireEvent.change(screen.getByLabelText("Server"), { target: { value: "h1" } });
    type("Directory", "/srv/backups/restic");
    fireEvent.click(screen.getAllByRole("button", { name: "Add target" }).at(-1)!);
    await screen.findByRole("button", { name: "Checking with restic…" });

    // realtime brings the pending target the panel kept
    list.push(
      target({
        ...disk,
        host: "h1",
        status: "pending",
        last_error: "The agent of pve-01 did not answer in time.",
        checked_at: "",
        usage_at: "",
      }),
    );
    fail(new ClientResponseError({ status: 504, response: { message: kept } }));

    expect(await screen.findByText(kept)).toBeInTheDocument();
    expect(screen.queryByRole("dialog", { name: "Add a backup target" })).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    const row = item("Panel disk");
    expect(row.getByText("Not checked yet")).toBeInTheDocument();
    expect(row.getByText("The agent of pve-01 did not answer in time.")).toBeInTheDocument();
    expect(row.getByRole("button", { name: "Refresh Panel disk" })).toBeEnabled();
  });

  it("tells how a refresh went, with restic's error when it failed", async () => {
    showPage("admin");
    const refresh = vi
      .spyOn(backupsApi, "refresh")
      .mockResolvedValueOnce({ ...node, last_error: "Fatal: wrong password or no key found" })
      .mockResolvedValueOnce({ ...bucket, used_bytes: 1.3e12 });

    fireEvent.click(screen.getByRole("button", { name: "Refresh backup-node" }));
    expect(refresh).toHaveBeenCalledWith("t3");
    expect(
      await screen.findByText(
        "backup-node failed its check: Fatal: wrong password or no key found",
      ),
    ).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Refresh Backblaze B2" }));
    expect(
      await screen.findByText("Backblaze B2 answers; what it holds follows shortly."),
    ).toBeInTheDocument();
  });
});

function confirmButton(name: string) {
  return screen.getAllByRole("button", { name }).at(-1)!;
}

function type(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

describe("adding a target", () => {
  it("sends an S3 bucket's config and keys, and only those", async () => {
    const add = vi.spyOn(backupsApi, "add").mockResolvedValue(bucket);
    const onClose = vi.fn();
    wrap(<AddTargetDialog hosts={hosts} onClose={onClose} />);

    expect(screen.getByLabelText("Kind")).toHaveValue("s3");
    expect(screen.getByLabelText("Secret access key")).toHaveAttribute("type", "password");
    expect(screen.getByText(/MinIO needs them/)).toBeInTheDocument();
    const confirm = confirmButton("Add target");
    expect(confirm).toBeDisabled();

    type("Name", "Backblaze B2");
    fireEvent.change(screen.getByLabelText("Server"), { target: { value: "h1" } });
    type("Endpoint", "https://s3.eu-central-003.backblazeb2.com");
    type("Bucket", "homelab-backups");
    type("Prefix (optional)", "pc");
    type("Access key ID", "003abc");
    type("Secret access key", "K003secret");
    type("Object Lock in days (optional)", "30");
    expect(confirm).toBeEnabled();
    fireEvent.click(confirm);

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(add).toHaveBeenCalledWith({
      name: "Backblaze B2",
      kind: "s3",
      host: "h1",
      config: {
        endpoint: "https://s3.eu-central-003.backblazeb2.com",
        bucket: "homelab-backups",
        prefix: "pc",
      },
      credentials: { access_key_id: "003abc", secret_access_key: "K003secret" },
      immutable_days: 30,
      note: "",
    });
    // a new repository: the panel makes its password
    expect(add.mock.calls[0][0]).not.toHaveProperty("password");
    expect(await screen.findByText("The target Backblaze B2 was added.")).toBeInTheDocument();
  });

  it("sends the host keys ssh-keyscan printed, and the private key", async () => {
    const add = vi.spyOn(backupsApi, "add").mockResolvedValue(node);
    wrap(<AddTargetDialog hosts={hosts} onClose={vi.fn()} />);

    fireEvent.change(screen.getByLabelText("Kind"), { target: { value: "sftp" } });
    expect(screen.queryByLabelText("Object Lock in days (optional)")).not.toBeInTheDocument();
    type("Name", "backup-node");
    fireEvent.change(screen.getByLabelText("Server"), { target: { value: "h1" } });
    type("Address", "backup-node:2222");
    type("User", "backup");
    type("Path", "/srv/restic");
    expect(screen.getByText("ssh-keyscan -p 2222 -t ed25519 backup-node")).toBeInTheDocument();
    const hostKey = screen.getByLabelText("Host key");
    expect(hostKey.tagName).toBe("TEXTAREA");
    // what an input of one line makes of it: no key, and no way on
    type("Host key", "# backup-node:22 SSH-2.0-OpenSSH_9.6 [backup-node]:2222 ssh-ed25519 AAAA");
    expect(screen.getByText(/There is no host key in this/)).toBeInTheDocument();
    type(
      "Private key",
      "-----BEGIN OPENSSH PRIVATE KEY-----\nb3Bl\n-----END OPENSSH PRIVATE KEY-----",
    );
    expect(confirmButton("Add target")).toBeDisabled();
    type(
      "Host key",
      "# backup-node:22 SSH-2.0-OpenSSH_9.6\n[backup-node]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5\n",
    );
    expect(screen.queryByText(/There is no host key in this/)).not.toBeInTheDocument();
    type(
      "Private key",
      "-----BEGIN OPENSSH PRIVATE KEY-----\nb3Bl\n-----END OPENSSH PRIVATE KEY-----",
    );
    type("Note (optional)", "  in the rack  ");
    fireEvent.click(confirmButton("Add target"));

    await waitFor(() => expect(add).toHaveBeenCalled());
    expect(add.mock.calls[0][0]).toEqual({
      name: "backup-node",
      kind: "sftp",
      host: "h1",
      config: {
        address: "backup-node:2222",
        user: "backup",
        path: "/srv/restic",
        host_key: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5",
      },
      credentials: {
        private_key:
          "-----BEGIN OPENSSH PRIVATE KEY-----\nb3Bl\n-----END OPENSSH PRIVATE KEY-----\n",
      },
      immutable_days: 0,
      note: "in the rack",
    });
  });

  it("sends a local directory with no credentials", async () => {
    const add = vi.spyOn(backupsApi, "add").mockResolvedValue(disk);
    wrap(<AddTargetDialog hosts={hosts} onClose={vi.fn()} />);

    fireEvent.change(screen.getByLabelText("Kind"), { target: { value: "local" } });
    expect(screen.getByText("A directory on that server; created if missing.")).toBeInTheDocument();
    type("Name", "Panel disk");
    fireEvent.change(screen.getByLabelText("Server"), { target: { value: "h2" } });
    type("Directory", "/srv/backups/restic");
    fireEvent.click(confirmButton("Add target"));

    await waitFor(() => expect(add).toHaveBeenCalled());
    expect(add.mock.calls[0][0]).toEqual({
      name: "Panel disk",
      kind: "local",
      host: "h2",
      config: { path: "/srv/backups/restic" },
      credentials: {},
      immutable_days: 0,
      note: "",
    });
  });

  it("adds an existing repository with its password, and sends none when it is empty", async () => {
    const add = vi.spyOn(backupsApi, "add").mockResolvedValue(disk);
    wrap(<AddTargetDialog hosts={hosts} onClose={vi.fn()} />);

    const password = screen.getByLabelText("Repository password (optional)");
    expect(password).toHaveAttribute("type", "password");
    expect(
      screen.getByText(/opens only with this panel's database and master key/),
    ).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Kind"), { target: { value: "rest" } });
    type("Name", "NAS");
    fireEvent.change(screen.getByLabelText("Server"), { target: { value: "h1" } });
    type("URL", "https://backup-node:8000/homelab/");
    type("Repository password (optional)", "the old one");
    fireEvent.click(confirmButton("Add target"));

    await waitFor(() => expect(add).toHaveBeenCalled());
    expect(add.mock.calls[0][0]).toEqual({
      name: "NAS",
      kind: "rest",
      host: "h1",
      config: { url: "https://backup-node:8000/homelab/" },
      credentials: {},
      immutable_days: 0,
      note: "",
      password: "the old one",
    });
  });

  it("asks a rest-server for both its user and password, or neither", () => {
    wrap(<AddTargetDialog hosts={hosts} onClose={vi.fn()} />);
    fireEvent.change(screen.getByLabelText("Kind"), { target: { value: "rest" } });
    type("Name", "NAS");
    fireEvent.change(screen.getByLabelText("Server"), { target: { value: "h1" } });
    type("URL", "https://backup-node:8000/homelab/");
    expect(confirmButton("Add target")).toBeEnabled();

    type("User (optional)", "pc");
    expect(screen.getByText("Fill in both, or neither.")).toBeInTheDocument();
    expect(confirmButton("Add target")).toBeDisabled();
  });

  it("waits for restic, then shows the panel's error and stays open", async () => {
    let reject: (error: unknown) => void = () => {};
    vi.spyOn(backupsApi, "add").mockReturnValue(
      new Promise((_, fail) => {
        reject = fail;
      }),
    );
    const onClose = vi.fn();
    wrap(<AddTargetDialog hosts={hosts} onClose={onClose} />);

    fireEvent.change(screen.getByLabelText("Kind"), { target: { value: "local" } });
    type("Name", "Panel disk");
    fireEvent.change(screen.getByLabelText("Server"), { target: { value: "h1" } });
    type("Directory", "/etc");
    fireEvent.click(confirmButton("Add target"));

    const busy = await screen.findByRole("button", { name: "Checking with restic…" });
    expect(busy).toBeDisabled();
    expect(screen.getByText(/The agent of pve-01 opens the repository/)).toBeInTheDocument();

    reject(new Error("Fatal: create repository at /etc failed: a system directory"));
    expect(
      await screen.findByText("Fatal: create repository at /etc failed: a system directory"),
    ).toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
    expect(confirmButton("Add target")).toBeEnabled();
    expect(screen.getByLabelText("Directory")).toHaveValue("/etc");
  });
});

describe("editing a target", () => {
  it("keeps the stored credentials when none are typed", async () => {
    const change = vi.spyOn(backupsApi, "change").mockResolvedValue(bucket);
    const onClose = vi.fn();
    wrap(<EditTargetDialog target={bucket} hosts={hosts} onClose={onClose} />);

    // what moves the repository is not to be changed here
    expect(screen.getByText("S3-compatible")).toBeInTheDocument();
    expect(screen.getByText(bucket.location)).toBeInTheDocument();
    expect(screen.getByLabelText("Server")).toHaveValue("h1");
    expect(screen.queryByLabelText("Endpoint")).not.toBeInTheDocument();
    expect(screen.getByLabelText("Object Lock in days (optional)")).toHaveValue(30);

    type("Name", "B2");
    type("Note (optional)", "EU region");
    fireEvent.click(confirmButton("Save"));

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(change).toHaveBeenCalledWith("t1", {
      name: "B2",
      note: "EU region",
      immutable_days: 30,
    });
    expect(await screen.findByText("The target Backblaze B2 was saved.")).toBeInTheDocument();
  });

  it("sends new credentials when they are typed, and restic checks them", async () => {
    let resolve: (target: BackupTarget) => void = () => {};
    const change = vi.spyOn(backupsApi, "change").mockReturnValue(
      new Promise((done) => {
        resolve = done;
      }),
    );
    wrap(<EditTargetDialog target={bucket} hosts={hosts} onClose={vi.fn()} />);

    type("Access key ID", "004new");
    expect(screen.getByText("Fill in both, or neither.")).toBeInTheDocument();
    expect(confirmButton("Save")).toBeDisabled();
    type("Secret access key", "K004secret");
    fireEvent.click(confirmButton("Save"));

    expect(await screen.findByRole("button", { name: "Checking with restic…" })).toBeDisabled();
    expect(change).toHaveBeenCalledWith("t1", {
      name: "Backblaze B2",
      note: "",
      immutable_days: 30,
      credentials: { access_key_id: "004new", secret_access_key: "K004secret" },
    });
    resolve(bucket);
    expect(await screen.findByText("The target Backblaze B2 was saved.")).toBeInTheDocument();
  });

  it("sends a new SFTP host key with the address, user and path it belongs to", async () => {
    const change = vi.spyOn(backupsApi, "change").mockResolvedValue(node);
    wrap(<EditTargetDialog target={node} hosts={hosts} onClose={vi.fn()} />);

    expect(screen.getByLabelText("Host key")).toHaveValue("ssh-ed25519 AAAAold");
    expect(screen.queryByLabelText("Object Lock in days (optional)")).not.toBeInTheDocument();
    expect(screen.getByText("ssh-keyscan -t ed25519 backup-node")).toBeInTheDocument();
    type("Host key", "backup-node ssh-ed25519 AAAAnew");
    fireEvent.click(confirmButton("Save"));

    await waitFor(() => expect(change).toHaveBeenCalled());
    expect(change.mock.calls[0]).toEqual([
      "t3",
      {
        name: "backup-node",
        note: "",
        config: {
          address: "backup-node",
          user: "backup",
          path: "/srv/restic",
          host_key: "ssh-ed25519 AAAAnew",
        },
      },
    ]);
  });

  it("sends no host keys when the same are there, in any order", async () => {
    const stored = target({
      ...node,
      config: {
        address: "backup-node",
        user: "backup",
        path: "/srv/restic",
        host_key: "ecdsa-sha2-nistp256 AAAAE2VjZHNh\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5",
      },
    });
    const change = vi.spyOn(backupsApi, "change").mockResolvedValue(stored);
    wrap(<EditTargetDialog target={stored} hosts={hosts} onClose={vi.fn()} />);

    expect(screen.getByLabelText("Host key")).toHaveValue(
      "ecdsa-sha2-nistp256 AAAAE2VjZHNh\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5",
    );
    type(
      "Host key",
      "# backup-node:22 SSH-2.0-OpenSSH_9.6\nbackup-node ssh-ed25519 AAAAC3NzaC1lZDI1NTE5\nbackup-node ecdsa-sha2-nistp256 AAAAE2VjZHNh",
    );
    fireEvent.click(confirmButton("Save"));

    await waitFor(() => expect(change).toHaveBeenCalled());
    expect(change.mock.calls[0]).toEqual(["t3", { name: "backup-node", note: "" }]);
  });

  it("holds back host keys that hold no key", () => {
    wrap(<EditTargetDialog target={node} hosts={hosts} onClose={vi.fn()} />);
    type("Host key", "backup-node");
    expect(screen.getByText(/There is no host key in this/)).toBeInTheDocument();
    expect(confirmButton("Save")).toBeDisabled();
  });

  it("moves a target to another server, which restic checks first", async () => {
    const change = vi.spyOn(backupsApi, "change").mockReturnValue(new Promise(() => {}));
    wrap(<EditTargetDialog target={node} hosts={hosts} onClose={vi.fn()} />);

    fireEvent.change(screen.getByLabelText("Server"), { target: { value: "h2" } });
    fireEvent.click(confirmButton("Save"));

    expect(await screen.findByRole("button", { name: "Checking with restic…" })).toBeDisabled();
    expect(
      screen.getByText(
        "The agent of nas opens the repository with restic with the new settings. This can take up to two minutes.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText(/creates it/)).not.toBeInTheDocument();
    expect(change).toHaveBeenCalledWith("t3", { name: "backup-node", note: "", host: "h2" });
  });

  it("keeps a local directory on its server", () => {
    wrap(<EditTargetDialog target={disk} hosts={hosts} onClose={vi.fn()} />);
    expect(screen.queryByLabelText("Server")).not.toBeInTheDocument();
    expect(screen.getByText("nas")).toBeInTheDocument();
    expect(screen.getByText(/The kind, the server and the location stay/)).toBeInTheDocument();
  });

  it("closes nothing when its dialog was closed before the save ended", async () => {
    let done: (target: BackupTarget) => void = () => {};
    vi.spyOn(backupsApi, "change").mockReturnValue(
      new Promise((resolve) => {
        done = resolve;
      }),
    );
    const onClose = vi.fn();
    const view = wrap(<EditTargetDialog target={bucket} hosts={hosts} onClose={onClose} />);
    type("Name", "B2");
    fireEvent.click(confirmButton("Save"));
    await screen.findByRole("button", { name: "Saving…" });
    view.rerender(
      <MemoryRouter>
        <AuthProvider>
          <ToastProvider>{null}</ToastProvider>
        </AuthProvider>
      </MemoryRouter>,
    );

    done({ ...bucket, name: "B2" });
    expect(await screen.findByText("The target B2 was saved.")).toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
  });

  it("shows why the panel refused, and stays open", async () => {
    vi.spyOn(backupsApi, "change").mockRejectedValue(new Error("The name is taken."));
    const onClose = vi.fn();
    wrap(<EditTargetDialog target={disk} hosts={hosts} onClose={onClose} />);

    // a local directory has no credentials to replace
    expect(screen.queryByText("New credentials")).not.toBeInTheDocument();
    type("Name", "Backblaze B2");
    fireEvent.click(confirmButton("Save"));

    expect(await screen.findByText("The name is taken.")).toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
  });
});

describe("the page's restore", () => {
  it("the header offers Restore from backup to operators", async () => {
    showPage("viewer");
    expect(screen.queryByRole("button", { name: "Restore from backup" })).not.toBeInTheDocument();
    cleanup();

    showPage("operator");
    const button = screen.getByRole("button", { name: "Restore from backup" });
    // only an admin adds a job
    expect(screen.queryByRole("button", { name: "New job" })).not.toBeInTheDocument();
    fireEvent.click(button);
    expect(await screen.findByRole("heading", { name: "Restore from backup" })).toBeInTheDocument();
    expect(screen.getByLabelText("Job")).toBeInTheDocument();
    cleanup();

    // left of New job, as the design has it
    showPage("admin");
    // the first New job is the header's; the jobs card offers one too
    const actions = screen.getAllByRole("button", { name: "New job" })[0].parentElement!;
    expect(
      within(actions)
        .getAllByRole("button")
        .map((item) => item.textContent),
    ).toEqual(["Restore from backup", " New job"]);
  });
});

describe("the server field", () => {
  it("takes its own id and label, and keeps out the servers a choice may not use", () => {
    const onChange = vi.fn();
    wrap(
      <ServerField
        id="restore-host"
        label="Restore on"
        hosts={hosts}
        value="h1"
        onChange={onChange}
        hint="Where the files go."
        unavailable={(host) => host.id === "h2"}
      />,
    );
    const select = screen.getByLabelText("Restore on");
    expect(select).toHaveAttribute("id", "restore-host");
    expect(within(select).getByRole("option", { name: "pve-01" })).toBeEnabled();
    expect(within(select).getByRole("option", { name: "nas (offline)" })).toBeDisabled();
    expect(screen.getByText("Where the files go.")).toBeInTheDocument();
  });
});

describe("a target's integrity", () => {
  // checked last Sunday at 05:21 UTC, next on Sunday at 05:00 UTC: 06:21 and
  // 06:00 in Prague
  const checked = (fields: Partial<BackupTarget> = {}) =>
    target({
      integrity_status: "ok",
      integrity_at: "2026-11-08 05:21:00.000Z",
      integrity_error: "",
      last_check: "c1",
      last_check_status: "ok",
      last_check_error: "",
      next_check_at: "2026-11-15 05:00:00.000Z",
      integrity_group: 4,
      ...fields,
    });

  function check(fields: Partial<BackupCheck>): BackupCheck {
    return {
      id: "c1",
      collectionId: "backup_checks",
      collectionName: "backup_checks",
      created: "2026-11-08 05:00:00.000Z",
      updated: "2026-11-08 05:21:00.000Z",
      target: "t1",
      host: "h1",
      kind: "integrity",
      trigger: "schedule",
      status: "ok",
      scheduled_at: "2026-11-08 05:00:00.000Z",
      started_at: "2026-11-08 05:00:02.000Z",
      finished_at: "2026-11-08 05:21:00.000Z",
      duration_ms: 1258000,
      subset: "3/20",
      packs_read: 118,
      packs_total: 2361,
      confirmed: false,
      unconfirmed: false,
      errors_more: 0,
      repair: null,
      notes: null,
      lock: null,
      error: "",
      command: "k3j9x2m4q8w1z7v",
      attempt: 1,
      retry_of: "",
      retry_at: "",
      ...fields,
    } as BackupCheck;
  }

  function show(
    role: User["role"],
    targets: BackupTarget[],
    lists: Record<string, unknown[]> = {},
    path = "/backups",
  ) {
    signInAs(role);
    live.lists = { backup_targets: targets, hosts, ...lists };
    wrap(<BackupsPage />, path);
  }

  function integrity(name: string) {
    const line = item(name).getByText("Integrity").closest(".backup-target-integrity");
    return within(line as HTMLElement);
  }

  function drawer(name: string) {
    return within(screen.getByRole("dialog", { name: `Integrity of ${name}` }));
  }

  it("says how its checks stand, the group the last read and when the next is", () => {
    show("viewer", [checked()], { backup_checks: [check({})] });

    const line = integrity("Backblaze B2");
    expect(line.getByText("OK").closest(".pill")).toHaveClass("ok");
    expect(
      line.getByText("restic check Sunday 06:21 · group 3 of 20 · next Sunday 06:00"),
    ).toBeInTheDocument();
    // everyone reads the details; only an admin checks
    expect(
      line.getByRole("button", { name: "Details of the integrity checks of Backblaze B2" }),
    ).toBeInTheDocument();
    expect(line.queryByRole("button", { name: /^Check now: / })).not.toBeInTheDocument();
  });

  it("is said in Czech, in local time", async () => {
    await i18n.changeLanguage("cs");
    show("viewer", [checked()], { backup_checks: [check({})] });

    const line = within(
      item("Backblaze B2")
        .getByText("Integrita")
        .closest(".backup-target-integrity") as HTMLElement,
    );
    expect(line.getByText("V pořádku").closest(".pill")).toHaveClass("ok");
    expect(
      line.getByText("restic check neděle 06:21 · skupina 3 z 20 · další neděle 06:00"),
    ).toBeInTheDocument();
  });

  it("says a target was never checked, apart from the reachability check", () => {
    show("viewer", [bucket]);
    const line = integrity("Backblaze B2");
    expect(line.getByText("No check yet").closest(".pill")).toHaveClass("off");
    // #28's check of the target stays its own
    expect(item("Backblaze B2").getByText("OK")).toBeInTheDocument();
  });

  it("shows the step and how far a running check is", () => {
    live.records = { commands: { cmd2: { id: "cmd2", progress: { step: "read", percent: 62 } } } };
    show("viewer", [checked({ last_check: "c2", last_check_status: "running" })], {
      backup_checks: [check({ id: "c2", status: "running", command: "cmd2", finished_at: "" })],
    });

    const line = integrity("Backblaze B2");
    expect(line.getByText("Checking").closest(".pill")).toHaveClass("info");
    expect(line.getByText(/^reading data · 62% · restic check Sunday 06:21/)).toBeInTheDocument();
  });

  it("shows the first three lines of the errors a check confirmed", () => {
    show(
      "viewer",
      [
        checked({
          integrity_status: "failed",
          last_check_status: "failed",
          integrity_error:
            "pack 1b2c: does not exist\npack 3d4e: does not exist\n\npack 5f60: does not exist\npack 7a8b: does not exist",
        }),
      ],
      { backup_checks: [check({ status: "failed", confirmed: true })] },
    );

    const line = integrity("Backblaze B2");
    expect(line.getByText("Errors found").closest(".pill")).toHaveClass("err");
    const errors = item("Backblaze B2").getByText(/^pack 1b2c/);
    expect(errors.tagName).toBe("PRE");
    expect(errors.textContent).toBe(
      "pack 1b2c: does not exist\npack 3d4e: does not exist\npack 5f60: does not exist",
    );
  });

  it("says why the newest attempt did not complete", () => {
    show(
      "viewer",
      [
        checked({
          last_check: "c2",
          last_check_status: "error",
          last_check_error: "The check did not finish in 12 hours.",
        }),
      ],
      {
        backup_checks: [
          check({ id: "c2", status: "error", finished_at: "2026-11-09 08:00:00.000Z" }),
        ],
      },
    );

    const line = integrity("Backblaze B2");
    expect(line.getByText("Could not check").closest(".pill")).toHaveClass("warn");
    expect(
      line.getByText(
        "restic check Sunday 06:21 · next Sunday 06:00 · last attempt Monday 09:00: The check did not finish in 12 hours.",
      ),
    ).toBeInTheDocument();
  });

  it("says when the panel tries a check that did not complete again", () => {
    show(
      "viewer",
      [
        checked({
          last_check: "c2",
          last_check_status: "error",
          last_check_error: "The check did not finish in 12 hours.",
        }),
      ],
      {
        backup_checks: [
          check({
            id: "c2",
            status: "error",
            finished_at: "2026-11-09 08:00:00.000Z",
            retry_at: "2026-11-09 13:00:00.000Z",
          }),
        ],
      },
    );

    expect(
      integrity("Backblaze B2").getByText(
        "restic check Sunday 06:21 · next Sunday 06:00 · last attempt Monday 09:00: The check did not finish in 12 hours. · next try: Monday 14:00",
      ),
    ).toBeInTheDocument();
  });

  it("lets an admin check now, after saying what is read and that only errors lock", async () => {
    show("admin", [checked()]);
    const started = vi.spyOn(backupsApi, "check").mockResolvedValue({ check: "c9", command: "x" });

    fireEvent.click(
      screen.getByRole("button", { name: "Check now: the integrity of Backblaze B2" }),
    );
    const dialog = within(screen.getByRole("dialog", { name: "Check Backblaze B2 now?" }));
    expect(
      dialog.getByText(
        "restic reads group 4 of 20 of the data packs, about 5% of the repository, through the agent of pve-01; storage that bills downloads bills it. The check locks nothing; only if it finds errors does it check that group again with the repository locked, and backups to it wait meanwhile.",
      ),
    ).toBeInTheDocument();
    expect(started).not.toHaveBeenCalled();

    fireEvent.click(dialog.getByRole("button", { name: "Check now" }));
    expect(started).toHaveBeenCalledWith("t1");
    expect(
      await screen.findByText("The integrity check of Backblaze B2 is queued."),
    ).toBeInTheDocument();
  });

  it("says a check of the structure only reads no data", () => {
    show("admin", [checked()], {
      settings: [{ id: "s1", backup_check_schedule: "0 5 * * 0", backup_check_groups: 0 }],
    });
    fireEvent.click(
      screen.getByRole("button", { name: "Check now: the integrity of Backblaze B2" }),
    );
    expect(
      within(screen.getByRole("dialog", { name: "Check Backblaze B2 now?" })).getByText(
        "restic checks the repository's structure through the agent of pve-01 and reads no data packs. The check locks nothing; only if it finds errors does it check again with the repository locked.",
      ),
    ).toBeInTheDocument();
  });

  it("shows why the panel refused a check", async () => {
    show("admin", [checked()]);
    vi.spyOn(backupsApi, "check").mockRejectedValue(
      new ClientResponseError({
        status: 409,
        response: { message: "An integrity check of Backblaze B2 is under way." },
      }),
    );
    fireEvent.click(
      screen.getByRole("button", { name: "Check now: the integrity of Backblaze B2" }),
    );
    fireEvent.click(
      within(screen.getByRole("dialog", { name: "Check Backblaze B2 now?" })).getByRole("button", {
        name: "Check now",
      }),
    );
    expect(
      await screen.findByText(
        "The integrity check of Backblaze B2 did not start: An integrity check of Backblaze B2 is under way.",
      ),
    ).toBeInTheDocument();
  });

  it("holds Check now back, saying why, when the panel would refuse it", () => {
    const old = { id: "h3", name: "edge", status: "online", capabilities: ["backup.run"] };
    show(
      "admin",
      [
        checked({ id: "p", name: "Pending", status: "pending", repo_id: "" }),
        checked({ id: "o", name: "Offline", host: "h2" }),
        checked({ id: "b", name: "Busy" }),
        checked({ id: "a", name: "Old agent", host: "h3" }),
        checked({ id: "r", name: "Ready" }),
      ],
      {
        hosts: [...hosts, old],
        backup_checks: [check({ id: "q", target: "b", status: "queued", finished_at: "" })],
      },
    );

    const button = (name: string) =>
      screen.getByRole("button", { name: `Check now: the integrity of ${name}` });
    expect(button("Pending")).toBeDisabled();
    expect(button("Pending")).toHaveAttribute("title", "Finish adding the target first.");
    expect(button("Offline")).toBeDisabled();
    expect(button("Offline")).toHaveAttribute("title", "Its server nas is offline.");
    expect(button("Busy")).toBeDisabled();
    expect(button("Busy")).toHaveAttribute("title", "A check of it is under way.");
    expect(button("Old agent")).toBeDisabled();
    expect(button("Old agent")).toHaveAttribute(
      "title",
      "The agent of edge cannot check repositories yet: update it.",
    );
    expect(button("Ready")).toBeEnabled();
    expect(button("Ready")).not.toHaveAttribute("title");
  });

  it("opens the details, with the checks, what one found, how to repair and its output", () => {
    const failed = check({
      id: "c2",
      created: "2026-11-09 05:00:00.000Z",
      finished_at: "2026-11-09 05:40:00.000Z",
      status: "failed",
      trigger: "manual",
      subset: "4/20",
      confirmed: true,
      errors_more: 1,
      command: "cmd2",
      repair: { index: true, packs: ["1b2c3d", "3d4e5f"], snapshots: true, other: false },
      notes: ["2 additional files were found in the repo, which likely contain duplicate data."],
    });
    show(
      "viewer",
      [checked({ integrity_status: "failed", last_check: "c2", last_check_status: "failed" })],
      { backup_checks: [failed, check({})] },
    );
    live.records = {
      backup_checks: {
        c2: {
          ...failed,
          errors: [
            { kind: "pack", id: "1b2c3d", text: "pack 1b2c3d: does not exist" },
            { kind: "pack", id: "3d4e5f", text: "pack 3d4e5f contains 2 errors: [blob x]" },
          ],
        },
      },
    };

    fireEvent.click(
      screen.getByRole("button", { name: "Details of the integrity checks of Backblaze B2" }),
    );
    const details = drawer("Backblaze B2");
    // what comes next
    expect(details.getByText("Next check").nextElementSibling).toHaveTextContent("Sunday 06:00");
    expect(details.getByText("Next group").nextElementSibling).toHaveTextContent("group 4 of 20");
    expect(details.getByText("Last OK").nextElementSibling).toHaveTextContent("Sunday 06:21");
    // the checks, newest first
    const history = within(details.getByRole("list", { name: "Recent checks" }));
    expect(
      history
        .getAllByRole("listitem")
        .map((row) => row.querySelector(".timeline-title")?.textContent),
    ).toEqual(["Errors found · by hand", "OK · scheduled"]);
    expect(history.getByText("Monday 06:40 · group 4 of 20 · took 21 min")).toBeInTheDocument();
    // what the newest found
    expect(details.getByText("3 errors")).toBeInTheDocument();
    expect(details.getByText("pack 1b2c3d: does not exist")).toBeInTheDocument();
    expect(details.getByText("and 1 more line")).toBeInTheDocument();
    expect(
      details.getByText("Confirmed by a second check with the repository locked."),
    ).toBeInTheDocument();
    expect(details.getByText("The index is damaged: run restic repair index.")).toBeInTheDocument();
    expect(
      details.getByText(
        "Pack files are damaged: read restic's troubleshooting guide, then run restic repair packs 1b2c3d 3d4e5f and restic repair snapshots --forget.",
      ),
    ).toBeInTheDocument();
    expect(
      details.getByText(
        "PocketCloud never repairs a repository itself: restic repair writes to it. Check again afterwards.",
      ),
    ).toBeInTheDocument();
    expect(
      details.getByText(
        "2 additional files were found in the repo, which likely contain duplicate data.",
      ),
    ).toBeInTheDocument();

    fireEvent.click(details.getByRole("button", { name: "Show output" }));
    const output = details.getByRole("region", { name: "Command output" });
    expect(output).toHaveAttribute("data-command", "cmd2");
    expect(within(output).getByText("Integrity check of Backblaze B2")).toBeInTheDocument();
    // a viewer neither checks nor stops
    expect(details.queryByRole("button", { name: /^Check now: / })).not.toBeInTheDocument();
  });

  it("says a check could not confirm its errors, and that it is tried again only when it is", () => {
    const suspectText =
      "The first check found errors; the second, with the repository locked, could not be completed, so they are neither confirmed nor ruled out.";
    // by hand, which the panel does not try again
    const manual = check({
      id: "c2",
      created: "2026-11-09 11:00:00.000Z",
      finished_at: "2026-11-09 11:40:00.000Z",
      status: "suspect",
      trigger: "manual",
      subset: "4/20",
      error: "the second pass did not start: the Docker Engine of pve-01 did not answer",
    });
    const errors = [{ kind: "pack", id: "1b2c3d", text: "pack 1b2c3d: does not exist" }];
    const shown = (row: BackupCheck) => {
      live.records = { backup_checks: { c2: { ...row, errors } } };
      show(
        "viewer",
        [checked({ last_check: "c2", last_check_status: "suspect", last_check_error: row.error })],
        { backup_checks: [row, check({})] },
        "/backups?check=t1",
      );
      return drawer("Backblaze B2");
    };

    let details = shown(manual);
    expect(details.getByText("pack 1b2c3d: does not exist")).toBeInTheDocument();
    expect(details.getByText(suspectText)).toBeInTheDocument();
    // why the second pass did not complete
    expect(details.getByText(manual.error)).toHaveClass("backup-target-error");
    expect(details.queryByText(/PocketCloud tries again/)).not.toBeInTheDocument();
    expect(details.queryByText(/^next try:/)).not.toBeInTheDocument();
    cleanup();

    // scheduled, and tried again an hour later
    details = shown({ ...manual, trigger: "schedule", retry_at: "2026-11-09 12:40:00.000Z" });
    expect(details.getByText(`${suspectText} PocketCloud tries again.`)).toBeInTheDocument();
    expect(details.getByText("next try: Monday 13:40")).toBeInTheDocument();
  });

  it("says when a check that did not complete or was skipped is tried again", () => {
    const skipped = check({
      id: "c2",
      created: "2026-11-09 05:00:00.000Z",
      started_at: "",
      finished_at: "2026-11-09 05:00:00.000Z",
      duration_ms: 0,
      status: "skipped",
      subset: "",
      packs_read: 0,
      packs_total: 0,
      error: "The server pve-01 was offline at 05:00 UTC.",
      // still offline at each try since: an hour more each time
      retry_at: "2026-11-09 13:00:00.000Z",
    });
    live.records = { backup_checks: { c2: skipped } };
    show(
      "viewer",
      [
        checked({
          last_check: "c2",
          last_check_status: "skipped",
          last_check_error: skipped.error,
        }),
      ],
      { backup_checks: [skipped, check({})] },
      "/backups?check=t1",
    );

    const details = drawer("Backblaze B2");
    expect(details.getByText("The server pve-01 was offline at 05:00 UTC.")).toBeInTheDocument();
    expect(details.getByText("next try: Monday 14:00")).toBeInTheDocument();
    expect(
      integrity("Backblaze B2").getByText(
        "restic check Sunday 06:21 · next Sunday 06:00 · last attempt Monday 06:00: The server pve-01 was offline at 05:00 UTC. · next try: Monday 14:00",
      ),
    ).toBeInTheDocument();
  });

  it("shows another check when it is picked", () => {
    const unconfirmed = check({ id: "c0", unconfirmed: true, created: "2026-11-01 05:00:00.000Z" });
    live.records = { backup_checks: { c1: check({}), c0: unconfirmed } };
    show("viewer", [checked()], { backup_checks: [check({}), unconfirmed] }, "/backups?check=t1");

    const details = drawer("Backblaze B2");
    expect(details.getByText("restic found no errors.")).toBeInTheDocument();
    const rows = details.getAllByRole("button", { name: /^OK · scheduled: Show the check of / });
    expect(rows[0]).toHaveAttribute("aria-pressed", "true");
    fireEvent.click(rows[1]);
    expect(rows[1]).toHaveAttribute("aria-pressed", "true");
    expect(
      details.getByText(/^The first check, without a lock, saw errors that the second/),
    ).toBeInTheDocument();
  });

  it("says no check has run yet", () => {
    show("viewer", [bucket], {}, "/backups?check=t1");
    expect(drawer("Backblaze B2").getByText("No check has run yet.")).toBeInTheDocument();
  });

  it("lets an operator stop a running check, after a confirmation", async () => {
    const running = check({ id: "c2", status: "running", command: "cmd2", finished_at: "" });
    live.records = {
      commands: { cmd2: { id: "cmd2", progress: { step: "confirm", percent: 80 } } },
    };
    show(
      "operator",
      [checked({ last_check: "c2", last_check_status: "running" })],
      {
        backup_checks: [running, check({})],
      },
      "/backups?check=t1",
    );
    const stopped = vi.spyOn(api, "cancelCommand").mockResolvedValue({});

    const details = drawer("Backblaze B2");
    expect(
      details.getByRole("progressbar", { name: "How far the check of Backblaze B2 is" }),
    ).toHaveAttribute("aria-valuenow", "80");
    expect(
      details.getByText("checking again with the repository locked · 80%"),
    ).toBeInTheDocument();
    // an operator may stop it, but only an admin checks
    expect(details.queryByRole("button", { name: /^Check now: / })).not.toBeInTheDocument();
    fireEvent.click(details.getByRole("button", { name: "Stop" }));
    const confirm = within(
      screen.getByRole("dialog", { name: "Stop the integrity check of Backblaze B2?" }),
    );
    expect(
      confirm.getByText(
        "restic stops and removes its lock; nothing in the repository changes. The next check reads the same group.",
      ),
    ).toBeInTheDocument();
    expect(stopped).not.toHaveBeenCalled();
    fireEvent.click(confirm.getByRole("button", { name: "Stop" }));
    expect(stopped).toHaveBeenCalledWith("cmd2");
    expect(await screen.findByText("The check of Backblaze B2 is stopping.")).toBeInTheDocument();
  });

  it("shows a viewer no Stop, and an admin Check now in the details", () => {
    const running = check({ id: "c2", status: "running", command: "cmd2", finished_at: "" });
    show(
      "viewer",
      [checked({ last_check: "c2", last_check_status: "running" })],
      {
        backup_checks: [running],
      },
      "/backups?check=t1",
    );
    expect(drawer("Backblaze B2").queryByRole("button", { name: "Stop" })).not.toBeInTheDocument();
    cleanup();

    show("admin", [checked()], {}, "/backups?check=t1");
    expect(
      drawer("Backblaze B2").getByRole("button", {
        name: "Check now: the integrity of Backblaze B2",
      }),
    ).toBeEnabled();
  });
});
