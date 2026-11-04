import type { RecordModel } from "pocketbase";

import { pb } from "./pb";
import type { Command } from "./types";

export type DbEngine = "mariadb" | "postgres";

export type DbServer = RecordModel & {
  host: string;
  name: string;
  engine: DbEngine;
  version: string;
  kind: "shared" | "dedicated";
  image: string;
  container_ref: string;
  service: string;
  stack: string;
  volume: string;
  port: number;
  admin_user: string;
  max_connections: number;
  connections: number;
  extensions: string[] | null;
  status: string;
  size_bytes: number;
  stats_at: string;
  last_error: string;
};

export type Database = RecordModel & {
  server: string;
  name: string;
  owner_kind: "none" | "stack" | "site" | "";
  stack: string;
  size_bytes: number;
  connections: number;
  temporary_until: string;
  status: string;
  last_error: string;
};

export type DbUser = RecordModel & {
  server: string;
  name: string;
  databases: string[];
  secret_name: string;
  rotated_at: string;
};

export type DbDump = RecordModel & {
  server: string;
  database_name: string;
  kind: "export" | "final";
  command: string;
  file: string;
  size_bytes: number;
  expires_at: string;
};

/** A database container found inside a stack, not linked yet. */
export type DbCandidate = {
  host: string;
  host_name: string;
  docker_id: string;
  name: string;
  image: string;
  engine: DbEngine;
  label: string;
  version: string;
  extensions: string[];
  stack: string;
  project: string;
  service: string;
  state: string;
  move_to: string;
  /** What the stack's Compose file says about the administrator. */
  admin_user: string;
  password_key: string;
};

export const engines: { value: DbEngine; label: string; port: number }[] = [
  { value: "mariadb", label: "MariaDB", port: 3306 },
  { value: "postgres", label: "PostgreSQL", port: 5432 },
];

export function engineLabel(engine: string): string {
  return engines.find((item) => item.value === engine)?.label ?? engine;
}

/** "MariaDB 11.8" from what the server said, or from its image tag. */
export function serverLabel(server: DbServer): string {
  // the tag follows the last colon after the last slash; one before it is a registry's port
  const tag = server.image.split("@")[0].split("/").pop()?.split(":")[1] ?? "";
  const version = server.version || tag.match(/^(?:pg)?(\d+(\.\d+)?)/)?.[1] || "";
  const label = `${engineLabel(server.engine)}${version ? ` ${version.replace(/-.*$/, "")}` : ""}`;
  const extensions = server.extensions ?? [];
  return extensions.length ? `${label} + ${extensions.join(", ")}` : label;
}

export const databaseNamePattern = /^[a-z][a-z0-9_]{0,62}$/;
export const serverNamePattern = /^[a-z0-9][a-z0-9-]{0,47}$/;

/**
 * What an application gets in its env file, as the panel adds it: the password is a
 * reference that is resolved when a deploy is delivered, never a value.
 */
export function envSnippet(server: DbServer, database: string, user: string): string {
  const password = `\${secret:db/${database}}`;
  const lines = [
    `DB_HOST=${server.name}`,
    `DB_PORT=${server.port || engines.find((item) => item.value === server.engine)?.port}`,
    `DB_NAME=${database}`,
    `DB_USER=${user}`,
    `DB_PASSWORD=${password}`,
  ];
  if (server.engine === "postgres") {
    lines.push(
      `DATABASE_URL=postgres://${user}:${password}@${server.name}:${server.port}/${database}`,
    );
  }
  return lines.join("\n");
}

/** The share of a server's connections in use, from 0 to 100. */
export function connectionShare(server: DbServer): number {
  if (!server.max_connections) {
    return 0;
  }
  return Math.min(100, Math.round((server.connections / server.max_connections) * 100));
}

export const dbApi = {
  createServer: (body: { host: string; name: string; engine: DbEngine }) =>
    pb.send<{ server: DbServer; command: Command }>("/api/pc/db-servers", {
      method: "POST",
      body,
    }),

  candidates: () => pb.send<DbCandidate[]>("/api/pc/db-servers/candidates", {}),

  linkServer: (body: {
    host: string;
    docker_id: string;
    admin_user: string;
    secret_key?: string;
    password?: string;
  }) => pb.send<DbServer>("/api/pc/db-servers/link", { method: "POST", body }),

  stats: (id: string) => pb.send<Command>(`/api/pc/db-servers/${id}/stats`, { method: "POST" }),

  removeServer: (id: string, removeVolume: boolean) =>
    pb.send(`/api/pc/db-servers/${id}?remove_volume=${removeVolume}`, { method: "DELETE" }),

  createDatabase: (body: { server: string; name: string; stack: string }) =>
    pb.send<{ database: Database; command: Command }>("/api/pc/databases", {
      method: "POST",
      body,
    }),

  connect: (id: string, stack: string) =>
    pb.send<Database>(`/api/pc/databases/${id}/connect`, { method: "POST", body: { stack } }),

  rotate: (id: string) => pb.send<Command>(`/api/pc/databases/${id}/rotate`, { method: "POST" }),

  clone: (id: string, name: string) =>
    pb.send<{ database: Database; command: Command }>(`/api/pc/databases/${id}/clone`, {
      method: "POST",
      body: { name },
    }),

  export: (id: string) => pb.send<Command>(`/api/pc/databases/${id}/export`, { method: "POST" }),

  drop: (id: string) => pb.send<Command>(`/api/pc/databases/${id}`, { method: "DELETE" }),

  adminer: (id: string) =>
    pb.send<{ url: string; expires_at: string }>(`/api/pc/databases/${id}/adminer`, {
      method: "POST",
    }),
};
