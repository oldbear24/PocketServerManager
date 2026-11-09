import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { ToastProvider } from "../components/toast";
import { AuthProvider } from "../lib/auth";
import { backupsApi, type BackupTarget } from "../lib/backups";
import { pb, type User } from "../lib/pb";
import type { Host } from "../lib/types";
import { AddTargetDialog, BackupsPage, EditTargetDialog } from "./Backups";

const now = Date.parse("2026-11-09T12:00:00Z");

const live = vi.hoisted(() => ({ lists: {} as Record<string, unknown[]> }));
vi.mock("../lib/realtime", () => ({
  useLiveList: (collection: string) => {
    const items = live.lists[collection] ?? [];
    return { items, loading: false, error: null, total: items.length, reload: () => {} };
  },
  useNow: () => now,
}));

beforeAll(() => {
  HTMLDialogElement.prototype.showModal = function showModal(this: HTMLDialogElement) {
    this.open = true;
  };
  HTMLDialogElement.prototype.close = function close(this: HTMLDialogElement) {
    this.open = false;
  };
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
      screen.getByText("restic on the agents · encrypted with a key kept in the panel"),
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
    expect(screen.getAllByRole("listitem")).toHaveLength(3);
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
