import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { ToastProvider } from "../components/toast";
import { api } from "../lib/api";
import { AuthProvider } from "../lib/auth";
import type { BackupJob, BackupRun } from "../lib/backupJobs";
import type { BackupTarget } from "../lib/backups";
import { pb, type User } from "../lib/pb";
import type { Container, Host } from "../lib/types";
import { ContainersCard, ServerDetailPage } from "./ServerDetail";

// Monday 13:00 in Prague, on winter time
const now = Date.parse("2026-11-09T12:00:00Z");

const live = vi.hoisted(() => ({
  host: null as unknown,
  lists: {} as Record<string, unknown[]>,
  filters: {} as Record<string, string | undefined>,
}));
vi.mock("../lib/realtime", async (importOriginal) => {
  const list = (collection: string, options?: { filter?: string }) => {
    live.filters[collection] = options?.filter;
    const items = live.lists[collection] ?? [];
    return { items, loading: false, error: null, total: items.length, reload: () => {} };
  };
  return {
    ...(await importOriginal<typeof import("../lib/realtime")>()),
    useLiveList: list,
    useLiveRecords: list,
    useLiveRecord: () => ({ record: live.host, loading: false, error: null }),
    useNow: () => now,
  };
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

function container(name: string, state: string, extra: Partial<Container> = {}): Container {
  return {
    id: name,
    collectionId: "containers",
    collectionName: "containers",
    host: "h1",
    stack: "",
    project: "",
    service: "",
    docker_id: `id-${name}`,
    name,
    image: "nginx:1",
    state,
    status_text: "",
    health: "",
    system_role: "",
    restart_count: 0,
    ports: null,
    labels: null,
    started_at: "",
    ...extra,
  } as Container;
}

const containers = [
  container("web", "running"),
  container("worker", "exited"),
  container("pocketcloud-agent", "running", { system_role: "agent", image: "agent:1" }),
];

function renderCard(role: User["role"]) {
  signInAs(role);
  vi.spyOn(pb, "send").mockResolvedValue({});
  const run = vi.spyOn(api, "runCommand").mockResolvedValue({} as never);
  render(
    <MemoryRouter>
      <AuthProvider>
        <ToastProvider>
          <ContainersCard containers={containers} host={{ id: "h1" } as Host} />
        </ToastProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
  return run;
}

afterEach(() => {
  pb.authStore.clear();
  vi.restoreAllMocks();
  live.host = null;
  live.lists = {};
  live.filters = {};
});

describe("the containers table", () => {
  it("lets operators choose containers, but not the system ones", () => {
    renderCard("operator");

    expect(screen.getByRole("checkbox", { name: "Choose web" })).toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: "Choose worker" })).toBeInTheDocument();
    expect(
      screen.queryByRole("checkbox", { name: "Choose pocketcloud-agent" }),
    ).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("checkbox", { name: "Choose all containers" }));
    const bar = screen.getByRole("toolbar", { name: "Actions for the chosen containers" });
    expect(within(bar).getByText("2 chosen")).toBeInTheDocument();
  });

  it("acts on the chosen containers that can take the action", () => {
    const run = renderCard("operator");

    fireEvent.click(screen.getByRole("checkbox", { name: "Choose all containers" }));
    const bar = screen.getByRole("toolbar");

    fireEvent.click(within(bar).getByRole("button", { name: "Stop" }));
    expect(run).toHaveBeenCalledTimes(1);
    expect(run).toHaveBeenCalledWith("h1", "container.stop", { docker_id: "id-web" }, "web");

    // both run nginx:1, which is pulled once
    run.mockClear();
    fireEvent.click(within(bar).getByRole("button", { name: "Pull image" }));
    expect(run).toHaveBeenCalledTimes(1);
    expect(run).toHaveBeenCalledWith("h1", "image.pull", { image: "nginx:1" }, "nginx:1");
  });

  it("asks before removing the chosen containers, without their volumes by default", () => {
    const run = renderCard("operator");

    fireEvent.click(screen.getByRole("checkbox", { name: "Choose all containers" }));
    fireEvent.click(within(screen.getByRole("toolbar")).getByRole("button", { name: "Remove" }));
    expect(run).not.toHaveBeenCalled();

    // jsdom does not open a modal dialog, so its content counts as hidden
    const dialog = screen.getByRole("dialog", { hidden: true });
    expect(within(dialog).getByText("Remove 2 containers?")).toBeInTheDocument();
    expect(within(dialog).getByRole("checkbox", { hidden: true })).not.toBeChecked();
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove", hidden: true }));

    expect(run).toHaveBeenCalledTimes(2);
    expect(run).toHaveBeenCalledWith(
      "h1",
      "container.remove",
      { docker_id: "id-worker", remove_volumes: false, force: true },
      "worker",
    );
  });

  it("shows viewers no choice and no actions", () => {
    renderCard("viewer");

    expect(screen.queryAllByRole("checkbox")).toHaveLength(0);
    expect(screen.queryByRole("button", { name: /Stop|Remove|Restart/ })).not.toBeInTheDocument();
  });
});

describe("the server's backups tab", () => {
  const zone = process.env.TZ;

  beforeAll(() => {
    // the times are the browser's, here Prague's
    process.env.TZ = "Europe/Prague";
  });

  afterAll(() => {
    if (zone === undefined) {
      delete process.env.TZ;
    } else {
      process.env.TZ = zone;
    }
  });

  const nas = { id: "h2", name: "nas", status: "online", roles: [], os: "" } as unknown as Host;

  function job(fields: Partial<BackupJob>): BackupJob {
    return {
      id: "j1",
      collectionId: "backup_jobs",
      collectionName: "backup_jobs",
      name: "vaultwarden",
      host: "h2",
      source_kind: "volume",
      source: "vw-data",
      stack: "",
      service: "",
      engine: "",
      targets: ["t1"],
      schedule: "0 * * * *",
      retention: { daily: 7 },
      enabled: true,
      origin: "manual",
      label_removed: false,
      problem: "",
      next_run_at: "2026-11-09 13:00:00.000Z",
      last_run_at: "",
      last_status: "",
      ...fields,
    } as BackupJob;
  }

  function showTab(lists: Record<string, unknown[]>) {
    signInAs("viewer");
    live.host = nas;
    live.lists = lists;
    render(
      <MemoryRouter initialEntries={["/servers/h2/backups"]}>
        <AuthProvider>
          <ToastProvider>
            <Routes>
              <Route path="/servers/:id/:tab" element={<ServerDetailPage />} />
            </Routes>
          </ToastProvider>
        </AuthProvider>
      </MemoryRouter>,
    );
  }

  it("lists the server's jobs: what each backs up, its last run and its next", () => {
    showTab({
      backup_jobs: [
        job({}),
        job({
          id: "j2",
          name: "nextcloud-db",
          source_kind: "db_dump",
          stack: "s1",
          service: "db",
          engine: "mysql",
          origin: "label",
          targets: [],
          enabled: false,
          next_run_at: "",
          problem: "Choose where it goes: no target.",
        }),
      ],
      backup_runs: [
        {
          id: "r1",
          job: "j1",
          target: "t1",
          host: "h2",
          status: "failed",
          created: "2026-11-09 11:00:00.000Z",
          finished_at: "2026-11-09 11:00:10.000Z",
          error: "Fatal: no space left on device",
        } as BackupRun,
      ],
      backup_targets: [{ id: "t1", name: "Backblaze B2" } as BackupTarget],
      stacks: [{ id: "s1", name: "nextcloud", host: "h2" }],
    });

    expect(screen.queryByText(/milestone/)).not.toBeInTheDocument();
    // only this server's
    expect(live.filters.backup_jobs).toBe('host = "h2"');
    expect(live.filters.backup_runs).toMatch(/^host = "h2" && created >= "/);

    const table = within(screen.getByRole("region", { name: "Backup jobs" }));
    expect(table.getAllByRole("columnheader").map((header) => header.textContent)).toEqual([
      "Job",
      "Last run",
      "Next run",
    ]);
    const rows = table.getAllByRole("row").slice(1);
    expect(rows).toHaveLength(2);

    const vw = within(rows[0]);
    expect(vw.getByText("vaultwarden")).toBeInTheDocument();
    expect(vw.getByText("volume vw-data")).toBeInTheDocument();
    expect(vw.getByText("Error · 12:00")).toHaveClass("run-status", "err");
    expect(vw.getByText("Fatal: no space left on device")).toBeInTheDocument();
    expect(vw.getByText("14:00")).toHaveClass("mono");

    const dump = within(rows[1]);
    expect(
      dump.getByText("mysqldump · nextcloud/db · from label pocketcloud.backup"),
    ).toBeInTheDocument();
    expect(dump.getByText("Choose where it goes: no target.")).toBeInTheDocument();
    expect(dump.getByText("not run yet")).toBeInTheDocument();
    expect(dump.getByText("paused")).toBeInTheDocument();

    expect(screen.getByRole("link", { name: "Manage on the Backups page" })).toHaveAttribute(
      "href",
      "/backups",
    );
  });

  it("says when the server has no jobs, and where to add one", () => {
    showTab({});
    expect(screen.getByText("No backup jobs on this server")).toBeInTheDocument();
    expect(screen.getByText(/Add one on the Backups page\./)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Manage on the Backups page" })).toHaveAttribute(
      "href",
      "/backups",
    );
  });
});
