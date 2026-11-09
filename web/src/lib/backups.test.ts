import { afterEach, describe, expect, it, vi } from "vitest";

import i18n from "../i18n";
import {
  anyCredential,
  backupsApi,
  emptyTargetForm,
  formatUsedOf,
  halfCredentials,
  hostKeys,
  keyscanCommand,
  parseDays,
  sameHostKeys,
  targetConfig,
  targetCredentials,
  targetTone,
  targetUsage,
  type TargetForm,
} from "./backups";
import { pb } from "./pb";

const TB = 1e12;
const GB = 1e9;

afterEach(() => {
  vi.restoreAllMocks();
});

const measured = "2026-11-09 09:00:00.000Z";

describe("the usage of a target", () => {
  it("reads used / capacity in the capacity's unit, with a share for the bar", () => {
    expect(
      targetUsage({
        kind: "local",
        used_bytes: 0.6 * TB,
        capacity_bytes: 4 * TB,
        usage_at: measured,
      }),
    ).toEqual({
      kind: "capacity",
      text: "0.6 / 4 TB",
      percent: 15,
    });
    expect(
      targetUsage({
        kind: "local",
        used_bytes: 0.3 * GB,
        capacity_bytes: 20 * GB,
        usage_at: measured,
      }),
    ).toEqual({
      kind: "capacity",
      text: "0.3 / 20 GB",
      percent: 1.5,
    });
  });

  it("says a bucket without a capacity has no limit", () => {
    expect(
      targetUsage({ kind: "s3", used_bytes: 1.2 * TB, capacity_bytes: 0, usage_at: measured }),
    ).toEqual({
      kind: "unlimited",
      used: "1.2 TB",
    });
  });

  it("gives just the used size of another target that reports no capacity", () => {
    expect(
      targetUsage({ kind: "sftp", used_bytes: 600 * GB, capacity_bytes: 0, usage_at: measured }),
    ).toEqual({
      kind: "used",
      used: "600 GB",
    });
    expect(
      targetUsage({ kind: "rest", used_bytes: 84e6, capacity_bytes: 0, usage_at: measured }),
    ).toEqual({
      kind: "used",
      used: "84 MB",
    });
  });

  it("says nothing until the usage is measured, whatever the bytes say", () => {
    expect(targetUsage({ kind: "s3", used_bytes: 0, capacity_bytes: 0, usage_at: "" })).toEqual({
      kind: "unknown",
    });
    // the repository opened, and its size has not arrived yet
    expect(
      targetUsage({ kind: "local", used_bytes: 5 * GB, capacity_bytes: 20 * GB, usage_at: "" }),
    ).toEqual({ kind: "unknown" });
  });

  it("says a new repository, measured, holds nothing yet", () => {
    expect(
      targetUsage({ kind: "s3", used_bytes: 0, capacity_bytes: 0, usage_at: measured }),
    ).toEqual({
      kind: "unlimited",
      used: "0 B",
    });
    expect(
      targetUsage({ kind: "sftp", used_bytes: 0, capacity_bytes: 0, usage_at: measured }),
    ).toEqual({
      kind: "used",
      used: "0 B",
    });
  });

  it("keeps the bar within its track", () => {
    const usage = targetUsage({
      kind: "local",
      used_bytes: 5 * TB,
      capacity_bytes: 4 * TB,
      usage_at: measured,
    });
    expect(usage.kind === "capacity" && usage.percent).toBe(100);
  });
});

describe("used of capacity", () => {
  it("gives the used size its own unit when it would round to nothing", () => {
    expect(formatUsedOf(300e3, 4 * TB)).toBe("300 kB / 4 TB");
    expect(formatUsedOf(0.05 * TB, 4 * TB)).toBe("0.1 / 4 TB");
  });

  it("drops the decimals of a large share", () => {
    expect(formatUsedOf(250 * GB, 500 * GB)).toBe("250 / 500 GB");
  });

  it("shows an empty repository as 0", () => {
    expect(formatUsedOf(0, 20 * GB)).toBe("0 / 20 GB");
  });

  it("reads the Czech way in Czech", async () => {
    await i18n.changeLanguage("cs");
    try {
      expect(formatUsedOf(0.6 * TB, 4 * TB)).toBe("0,6 / 4 TB");
    } finally {
      await i18n.changeLanguage("en");
    }
  });
});

describe("the Object Lock note", () => {
  it("has every Czech plural", () => {
    const cs = i18n.getFixedT("cs");
    expect(cs("backups.objectLock", { count: 1 })).toBe(
      "Object Lock 1 den – ochrana proti smazání",
    );
    expect(cs("backups.objectLock", { count: 3 })).toBe(
      "Object Lock 3 dny – ochrana proti smazání",
    );
    expect(cs("backups.objectLock", { count: 30 })).toBe(
      "Object Lock 30 dní – ochrana proti smazání",
    );
    expect(cs("backups.objectLock", { count: 1.5 })).toBe(
      "Object Lock 1.5 dne – ochrana proti smazání",
    );
  });
});

describe("the tone of a target's state", () => {
  it("follows its last check", () => {
    expect(targetTone("ok")).toBe("ok");
    expect(targetTone("failed")).toBe("err");
    expect(targetTone("pending")).toBe("info");
    expect(targetTone("")).toBe("off");
  });
});

function form(fields: Partial<TargetForm>): TargetForm {
  return { ...emptyTargetForm, ...fields };
}

describe("the settings of a kind", () => {
  const typed = form({
    endpoint: " https://s3.eu-central-003.backblazeb2.com ",
    bucket: "homelab-backups",
    prefix: "/pc/",
    region: "",
    access_key_id: "003abc ",
    secret_access_key: "K003secret",
    address: "backup-node",
    user: "backup",
    path: "/srv/restic",
    host_key: "# backup-node:22 SSH-2.0-OpenSSH_9.6\nbackup-node ssh-ed25519 AAAAC3Nza",
    private_key: "-----BEGIN OPENSSH PRIVATE KEY-----\nb3Bl\n-----END OPENSSH PRIVATE KEY-----",
    url: "https://backup-node:8000/homelab/",
  });

  it("takes only the kind's own config keys, and optional ones when set", () => {
    expect(targetConfig("s3", typed)).toEqual({
      endpoint: "https://s3.eu-central-003.backblazeb2.com",
      bucket: "homelab-backups",
      prefix: "pc",
    });
    expect(targetConfig("s3", { ...typed, region: "eu-central-003", path_style: true })).toEqual({
      endpoint: "https://s3.eu-central-003.backblazeb2.com",
      bucket: "homelab-backups",
      prefix: "pc",
      region: "eu-central-003",
      path_style: true,
    });
    expect(targetConfig("sftp", typed)).toEqual({
      address: "backup-node",
      user: "backup",
      path: "/srv/restic",
      host_key: "ssh-ed25519 AAAAC3Nza",
    });
    expect(targetConfig("rest", typed)).toEqual({ url: "https://backup-node:8000/homelab/" });
    expect(targetConfig("local", typed)).toEqual({ path: "/srv/restic" });
  });

  it("takes only the kind's own credentials", () => {
    expect(targetCredentials("s3", typed)).toEqual({
      access_key_id: "003abc",
      secret_access_key: "K003secret",
    });
    expect(targetCredentials("sftp", typed)).toEqual({
      private_key: "-----BEGIN OPENSSH PRIVATE KEY-----\nb3Bl\n-----END OPENSSH PRIVATE KEY-----\n",
    });
    expect(targetCredentials("local", typed)).toEqual({});
  });

  it("gives a rest-server both its user and password, or nothing", () => {
    expect(targetCredentials("rest", typed)).toEqual({});
    expect(targetCredentials("rest", form({ username: "pc", password: "secret" }))).toEqual({
      username: "pc",
      password: "secret",
    });
    expect(halfCredentials("rest", form({ username: "pc" }))).toBe(true);
    expect(halfCredentials("rest", form({ username: "pc", password: "secret" }))).toBe(false);
    expect(halfCredentials("rest", form({}))).toBe(false);
  });

  it("knows a half typed S3 key", () => {
    expect(halfCredentials("s3", form({ secret_access_key: "K003" }))).toBe(true);
    expect(halfCredentials("s3", form({}))).toBe(false);
  });

  it("knows whether any credential was typed", () => {
    expect(anyCredential("s3", form({}))).toBe(false);
    expect(anyCredential("sftp", form({ private_key: "  " }))).toBe(false);
    expect(anyCredential("sftp", form({ private_key: "key" }))).toBe(true);
    expect(anyCredential("rest", form({ password: "secret" }))).toBe(true);
    expect(anyCredential("local", form({ path: "/srv" }))).toBe(false);
  });
});

describe("SFTP host keys", () => {
  const keyscan = [
    "# backup-node:22 SSH-2.0-OpenSSH_9.6",
    "backup-node ssh-rsa AAAAB3NzaC1yc2E=",
    "# backup-node:22 SSH-2.0-OpenSSH_9.6",
    "backup-node ecdsa-sha2-nistp256 AAAAE2VjZHNh",
    "",
    "backup-node ssh-ed25519 AAAAC3NzaC1lZDI1NTE5",
  ].join("\n");

  it("takes every key ssh-keyscan prints, without its comments and the host name", () => {
    expect(hostKeys(keyscan)).toBe(
      [
        "ssh-rsa AAAAB3NzaC1yc2E=",
        "ecdsa-sha2-nistp256 AAAAE2VjZHNh",
        "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5",
      ].join("\n"),
    );
    expect(hostKeys("[backup-node]:2222 ssh-ed25519 AAAAC3Nza\r\n")).toBe("ssh-ed25519 AAAAC3Nza");
  });

  it("takes a key line as it is, without a comment after it, and each key once", () => {
    expect(hostKeys("  ssh-ed25519 AAAAC3Nza  ")).toBe("ssh-ed25519 AAAAC3Nza");
    expect(hostKeys("ssh-ed25519 AAAAC3Nza root@backup-node")).toBe("ssh-ed25519 AAAAC3Nza");
    expect(hostKeys("a ssh-ed25519 AAAAC3Nza\nb ssh-ed25519 AAAAC3Nza")).toBe(
      "ssh-ed25519 AAAAC3Nza",
    );
  });

  it("finds no key in what holds none", () => {
    expect(hostKeys("")).toBe("");
    // a single line input joins ssh-keyscan's two into one comment
    expect(hostKeys("# backup-node:22 SSH-2.0-OpenSSH_9.6 backup-node ssh-ed25519 AAAA")).toBe("");
    expect(hostKeys("backup-node")).toBe("");
    expect(hostKeys("ssh-ed25519 AAAA…")).toBe("");
  });

  it("knows the same keys in another order, or as ssh-keyscan prints them", () => {
    const stored = "ecdsa-sha2-nistp256 AAAAE2VjZHNh\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5";
    expect(
      sameHostKeys("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5\necdsa-sha2-nistp256 AAAAE2VjZHNh", stored),
    ).toBe(true);
    expect(sameHostKeys(stored, stored)).toBe(true);
    expect(sameHostKeys("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5", stored)).toBe(false);
    expect(sameHostKeys(keyscan, stored)).toBe(false);
  });

  it("is printed by a command for the server's address", () => {
    expect(keyscanCommand("backup-node")).toBe("ssh-keyscan -t ed25519 backup-node");
    expect(keyscanCommand("backup-node:2222")).toBe("ssh-keyscan -p 2222 -t ed25519 backup-node");
    expect(keyscanCommand("backup-node:22")).toBe("ssh-keyscan -t ed25519 backup-node");
    expect(keyscanCommand("[fd00::5]:2222")).toBe("ssh-keyscan -p 2222 -t ed25519 fd00::5");
    expect(keyscanCommand("")).toBe("ssh-keyscan -t ed25519 backup-node");
  });
});

describe("Object Lock days", () => {
  it("are a whole number, none when empty", () => {
    expect(parseDays("")).toBe(0);
    expect(parseDays(" 30 ")).toBe(30);
    expect(parseDays("-1")).toBeNull();
    expect(parseDays("1.5")).toBeNull();
    expect(parseDays("abc")).toBeNull();
    expect(parseDays("36500")).toBe(36500);
    expect(parseDays("36501")).toBeNull();
  });
});

describe("the backup targets API", () => {
  it("calls the panel's routes", async () => {
    const send = vi.spyOn(pb, "send").mockResolvedValue({});
    const body = {
      name: "NAS",
      kind: "local" as const,
      host: "h1",
      config: { path: "/srv/backups/restic" },
      credentials: {},
      immutable_days: 0,
      note: "",
    };
    await backupsApi.add(body);
    await backupsApi.change("t1", { name: "NAS 2" });
    await backupsApi.refresh("t1");
    await backupsApi.remove("t1");
    expect(send.mock.calls).toEqual([
      ["/api/pc/backup-targets", { method: "POST", body }],
      ["/api/pc/backup-targets/t1", { method: "PATCH", body: { name: "NAS 2" } }],
      ["/api/pc/backup-targets/t1/refresh", { method: "POST" }],
      ["/api/pc/backup-targets/t1", { method: "DELETE" }],
    ]);
  });
});
