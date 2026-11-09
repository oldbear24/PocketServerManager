import type { RecordModel } from "pocketbase";

import type { Tone } from "../components/ui";
import { formatBytes, formatNumber, percentOf } from "./format";
import { pb } from "./pb";

/** Where restic keeps a repository (#28). */
export type BackupTargetKind = "s3" | "sftp" | "rest" | "local";

export const targetKinds: BackupTargetKind[] = ["s3", "sftp", "rest", "local"];

/** A target's settings that are not secret, per kind. */
export type BackupTargetConfig = {
  // s3
  endpoint?: string;
  bucket?: string;
  prefix?: string;
  region?: string;
  path_style?: boolean;
  // sftp
  address?: string;
  user?: string;
  host_key?: string;
  // sftp, local
  path?: string;
  // rest
  url?: string;
};

export type BackupTargetStatus = "pending" | "ok" | "failed";

/** Where backups go. Its credentials and the repository's password never
 * reach the UI. */
export type BackupTarget = RecordModel & {
  name: string;
  kind: BackupTargetKind;
  /** the server whose agent reaches the target, and holds a local one */
  host: string;
  config: BackupTargetConfig | null;
  /** where it is, as people read it */
  location: string;
  repo_id: string;
  status: BackupTargetStatus;
  /** restic's last lines when the last check failed */
  last_error: string;
  checked_at: string;
  used_bytes: number;
  /** 0: unknown, or no limit */
  capacity_bytes: number;
  /** the days a bucket's Object Lock keeps objects, as the admin states it */
  immutable_days: number;
  note: string;
};

export type NewBackupTarget = {
  name: string;
  kind: BackupTargetKind;
  host: string;
  config: BackupTargetConfig;
  credentials: Record<string, string>;
  immutable_days: number;
  note: string;
  /** the password of an existing repository, to add it; without one, the
   * panel makes a new password and creates the repository if there is none */
  password?: string;
};

/** What may change on a target. Credentials replace the stored ones only when
 * given; the config only takes what does not move the repository. Another
 * server may reach any target but a local one. */
export type BackupTargetChange = {
  name?: string;
  host?: string;
  config?: BackupTargetConfig;
  credentials?: Record<string, string>;
  immutable_days?: number;
  note?: string;
};

export const backupsApi = {
  /** restic opens the repository, or creates it: up to about two minutes */
  add: (body: NewBackupTarget) =>
    pb.send<BackupTarget>("/api/pc/backup-targets", { method: "POST", body }),

  change: (id: string, body: BackupTargetChange) =>
    pb.send<BackupTarget>(`/api/pc/backup-targets/${id}`, { method: "PATCH", body }),

  /** checks the target again; a failed check comes back as its status */
  refresh: (id: string) =>
    pb.send<BackupTarget>(`/api/pc/backup-targets/${id}/refresh`, { method: "POST" }),

  /** the repository's data stays where it is */
  remove: (id: string) => pb.send(`/api/pc/backup-targets/${id}`, { method: "DELETE" }),
};

export function targetTone(status: BackupTargetStatus | ""): Tone {
  switch (status) {
    case "ok":
      return "ok";
    case "failed":
      return "err";
    case "pending":
      return "info";
    default:
      return "off";
  }
}

/** How much a target holds, as its line reads. */
export type Usage =
  /** "0.6 / 4 TB", with a bar */
  | { kind: "capacity"; text: string; percent: number }
  /** "1.2 TB · no limit": a bucket has none */
  | { kind: "unlimited"; used: string }
  /** "0.6 TB": a server that reports no capacity */
  | { kind: "used"; used: string }
  /** not checked yet */
  | { kind: "unknown" };

export function targetUsage(
  target: Pick<BackupTarget, "kind" | "used_bytes" | "capacity_bytes" | "checked_at">,
): Usage {
  if (!target.checked_at) {
    return { kind: "unknown" };
  }
  const used = Math.max(0, target.used_bytes || 0);
  const capacity = target.capacity_bytes || 0;
  if (capacity > 0) {
    return {
      kind: "capacity",
      text: formatUsedOf(used, capacity),
      percent: percentOf(used, capacity),
    };
  }
  // a new repository holds nothing yet, which is no dash
  const size = used > 0 ? formatBytes(used) : `${formatNumber(0)} B`;
  return target.kind === "s3" ? { kind: "unlimited", used: size } : { kind: "used", used: size };
}

const byteUnits = ["B", "kB", "MB", "GB", "TB", "PB"];

/** "0.6 / 4 TB": the used size in the unit of the capacity, as the design
 * reads it, unless it would round to nothing there: "300 kB / 4 TB". */
export function formatUsedOf(used: number, capacity: number): string {
  const exponent = Math.min(
    Math.max(0, Math.floor(Math.log10(capacity) / 3)),
    byteUnits.length - 1,
  );
  const scaled = used / 1000 ** exponent;
  if (used > 0 && scaled < 0.05) {
    return `${formatBytes(used)} / ${formatBytes(capacity)}`;
  }
  return `${formatNumber(scaled, scaled >= 100 || exponent === 0 ? 0 : 1)} / ${formatBytes(capacity)}`;
}

/** Everything the dialogs ask for, as typed. */
export type TargetForm = {
  endpoint: string;
  bucket: string;
  prefix: string;
  region: string;
  path_style: boolean;
  access_key_id: string;
  secret_access_key: string;
  address: string;
  user: string;
  path: string;
  host_key: string;
  private_key: string;
  url: string;
  username: string;
  password: string;
};

export const emptyTargetForm: TargetForm = {
  endpoint: "",
  bucket: "",
  prefix: "",
  region: "",
  path_style: false,
  access_key_id: "",
  secret_access_key: "",
  address: "",
  user: "",
  path: "",
  host_key: "",
  private_key: "",
  url: "",
  username: "",
  password: "",
};

/** The fields a new target of a kind cannot do without. */
export const requiredFields: Record<BackupTargetKind, (keyof TargetForm)[]> = {
  s3: ["endpoint", "bucket", "access_key_id", "secret_access_key"],
  sftp: ["address", "user", "path", "host_key", "private_key"],
  rest: ["url"],
  local: ["path"],
};

/** The config of a kind: its own keys only, the optional ones when set. */
export function targetConfig(kind: BackupTargetKind, form: TargetForm): BackupTargetConfig {
  switch (kind) {
    case "s3": {
      const config: BackupTargetConfig = {
        endpoint: form.endpoint.trim(),
        bucket: form.bucket.trim(),
      };
      const prefix = form.prefix.trim().replace(/^\/+|\/+$/g, "");
      if (prefix) {
        config.prefix = prefix;
      }
      if (form.region.trim()) {
        config.region = form.region.trim();
      }
      if (form.path_style) {
        config.path_style = true;
      }
      return config;
    }
    case "sftp":
      return {
        address: form.address.trim(),
        user: form.user.trim(),
        path: form.path.trim(),
        host_key: hostKeyLine(form.host_key),
      };
    case "rest":
      return { url: form.url.trim() };
    case "local":
      return { path: form.path.trim() };
  }
}

/** The credentials of a kind, as typed: empty values where nothing was. A
 * rest-server without a user takes none at all. */
export function targetCredentials(
  kind: BackupTargetKind,
  form: TargetForm,
): Record<string, string> {
  switch (kind) {
    case "s3":
      return {
        access_key_id: form.access_key_id.trim(),
        secret_access_key: form.secret_access_key.trim(),
      };
    case "sftp":
      return { private_key: privateKey(form.private_key) };
    case "rest":
      return form.username.trim() || form.password
        ? { username: form.username.trim(), password: form.password }
        : {};
    case "local":
      return {};
  }
}

/** Whether any credential was typed: when editing, none keeps the stored ones. */
export function anyCredential(kind: BackupTargetKind, form: TargetForm): boolean {
  return Object.values(targetCredentials(kind, form)).some((value) => value !== "");
}

/** Credentials that come in pairs and were half typed: an S3 key without
 * its secret, a rest-server user without a password. */
export function halfCredentials(kind: BackupTargetKind, form: TargetForm): boolean {
  if (kind === "s3") {
    return !form.access_key_id.trim() !== !form.secret_access_key.trim();
  }
  if (kind === "rest") {
    return !form.username.trim() !== !form.password;
  }
  return false;
}

/** A private key as its file has it, ending with a newline. */
function privateKey(text: string): string {
  const key = text.trim();
  return key ? `${key}\n` : "";
}

/** The host key as one line, "ssh-ed25519 AAAA…": the first key line of what
 * ssh-keyscan prints, without the host name it starts with. */
export function hostKeyLine(text: string): string {
  const line =
    text
      .split(/\r?\n/)
      .map((item) => item.trim())
      .find((item) => item !== "" && !item.startsWith("#")) ?? "";
  const parts = line.split(/\s+/);
  if (
    parts.length >= 3 &&
    /^(ssh-|ecdsa-|sk-)/.test(parts[1]) &&
    !/^(ssh-|ecdsa-|sk-)/.test(parts[0])
  ) {
    return parts.slice(1).join(" ");
  }
  return line;
}

/** The command that prints an SFTP server's host key, for its address. */
export function keyscanCommand(address: string): string {
  const value = address.trim();
  const match = /^(?:\[([^\]]+)\]|([^:\s]+))(?::(\d+))?$/.exec(value);
  const host = match ? (match[1] ?? match[2]) : value;
  const port = match?.[3] && match[3] !== "22" ? `-p ${match[3]} ` : "";
  return `ssh-keyscan ${port}-t ed25519 ${host || "backup-node"}`;
}

/** As the panel takes them: a hundred years. */
export const maxImmutableDays = 36500;

/** Object Lock days as typed: empty is none, anything but a whole number of
 * days is null. */
export function parseDays(text: string): number | null {
  const value = text.trim();
  if (value === "") {
    return 0;
  }
  return /^\d{1,5}$/.test(value) && Number(value) <= maxImmutableDays ? Number(value) : null;
}
