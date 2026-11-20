import { screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { pb } from "../lib/pb";
import type { Host } from "../lib/types";
import { now, signInAs, wrap } from "./BackupRestore.fixtures";
import { ServersPage } from "./Servers";

const live = vi.hoisted(() => ({ hosts: [] as unknown[] }));
vi.mock("../lib/realtime", () => ({
  useLiveList: () => ({
    items: live.hosts,
    total: live.hosts.length,
    loading: false,
    error: null,
    reload: () => {},
  }),
  useNow: () => now,
}));

beforeEach(() => signInAs("viewer"));

afterEach(() => {
  pb.authStore.clear();
  live.hosts = [];
});

function host(name: string, cpu: number, mem: number, disk: number): Host {
  return {
    id: name,
    collectionId: "hosts",
    collectionName: "hosts",
    name,
    roles: ["docker"],
    status: "online",
    ip4: "10.0.0.20",
    os: "Debian 12",
    cpu_percent: cpu,
    mem_used_bytes: mem,
    mem_bytes: 100,
    disk_used_bytes: disk,
    disk_total_bytes: 100,
    load1: 0.5,
    agent_version: "0.4.2",
    last_seen: "2026-11-09 11:59:50.000Z",
  } as Host;
}

describe("the servers' usage", () => {
  it("turns amber from 75 % and red from 90 %, as on the Overview (#33)", () => {
    live.hosts = [host("nas", 74, 75, 89), host("vps-wedos", 90, 12, 100)];
    wrap(<ServersPage />, "/servers");
    const level = (name: string) =>
      screen.getByRole("progressbar", { name }).className.replace("progress", "").trim();

    expect(level("CPU of nas")).toBe("");
    expect(level("Memory of nas")).toBe("warn");
    expect(level("Disk of nas")).toBe("warn");
    expect(level("CPU of vps-wedos")).toBe("err");
    expect(level("Memory of vps-wedos")).toBe("");
    expect(level("Disk of vps-wedos")).toBe("err");
  });
});
