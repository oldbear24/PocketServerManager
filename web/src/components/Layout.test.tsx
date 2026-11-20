import { render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { openAlertsQuery, useOpenAlerts } from "../lib/alerts";
import { api } from "../lib/api";
import { AuthProvider } from "../lib/auth";
import { hostSummaryFields, useHosts } from "../lib/hosts";
import { pb, type User } from "../lib/pb";
import { matchesFilter } from "../test-filter";
import type { Alert, PanelInfo } from "../lib/types";
import { alertDot, Layout } from "./Layout";

const live = vi.hoisted(() => ({
  lists: {} as Record<string, unknown[]>,
  /** a list's count, where the panel counts more than it sends */
  totals: {} as Record<string, number>,
  /** each list the Layout asked for, with its options */
  asked: [] as { collection: string; options: Record<string, unknown> }[],
}));
vi.mock("../lib/realtime", () => ({
  useLiveList: (collection: string, options: { filter?: string } = {}) => {
    live.asked.push({ collection, options });
    const items = (live.lists[collection] ?? []).filter(
      (item) => !options.filter || matchesFilter(item as Record<string, unknown>, options.filter),
    );
    const total = live.totals[collection] ?? items.length;
    return { items, loading: false, error: null, total, reload: () => {} };
  },
}));

beforeEach(() => {
  // the gateway card waits; it is not what these tests are about
  vi.spyOn(api, "info").mockReturnValue(new Promise(() => {}));
  const payload = btoa(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600, type: "auth" }));
  pb.authStore.save(`header.${payload}.signature`, {
    id: "u1",
    collectionId: "users",
    collectionName: "users",
    email: "viewer@example.com",
    name: "",
    role: "viewer",
  } as unknown as User);
});

afterEach(() => {
  vi.restoreAllMocks();
  pb.authStore.clear();
  live.lists = {};
  live.totals = {};
  live.asked = [];
});

function alert(fields: Partial<Alert>): Alert {
  return {
    id: fields.rule ?? "a1",
    collectionId: "alerts",
    collectionName: "alerts",
    rule: "backup_failed",
    target: "j1:t1",
    host: "h1",
    severity: "warning",
    status: "open",
    first_seen: "",
    last_seen: "",
    message: "",
    ...fields,
  } as Alert;
}

function backups() {
  render(
    <MemoryRouter initialEntries={["/"]}>
      <AuthProvider>
        <Routes>
          <Route path="*" element={<Layout />} />
        </Routes>
      </AuthProvider>
    </MemoryRouter>,
  );
  return screen.getByRole("link", { name: /^Backups/ });
}

describe("the Backups item of the navigation", () => {
  it("has no dot without an open backup alert", () => {
    live.lists = {
      alerts: [
        alert({ rule: "agent_offline", target: "h1", severity: "critical" }),
        alert({ status: "resolved", severity: "critical" }),
      ],
    };
    const link = backups();
    expect(link.querySelector(".nav-dot")).toBeNull();
    expect(link).toHaveAccessibleName("Backups");
  });

  it("is marked warn by a warning, and says how many alerts are open", () => {
    live.lists = {
      alerts: [
        alert({ rule: "backup_failed", severity: "warning" }),
        alert({ rule: "backup_check_unverified", target: "t1", severity: "warning" }),
      ],
    };
    const link = backups();
    expect(link.querySelector(".nav-dot")).toHaveClass("warn");
    expect(within(link).getByText("2 open alerts")).toHaveClass("visually-hidden");
  });

  it("is marked err when any backup alert is critical", () => {
    live.lists = {
      alerts: [
        alert({ rule: "backup_retention", severity: "warning" }),
        alert({ rule: "backup_check_failed", target: "t1", severity: "critical" }),
      ],
    };
    expect(backups().querySelector(".nav-dot")).toHaveClass("err");
  });

  it("is marked by a restore test that failed or went unverified, and a temporary database left (#53)", () => {
    live.lists = {
      alerts: [alert({ rule: "backup_restore_test_failed", target: "j1", severity: "critical" })],
    };
    expect(backups().querySelector(".nav-dot")).toHaveClass("err");
    document.body.innerHTML = "";
    live.lists = {
      alerts: [
        alert({ rule: "backup_restore_test_unverified", target: "j1", severity: "warning" }),
        alert({ id: "a2", rule: "db_temporary_left", target: "d9", severity: "warning" }),
      ],
    };
    const link = backups();
    expect(link.querySelector(".nav-dot")).toHaveClass("warn");
    expect(within(link).getByText("2 open alerts")).toBeInTheDocument();
  });
});

describe("the Databases item of the navigation (#53)", () => {
  it("counts no temporary database of a restore or a restore test", () => {
    const database = (id: string, temporary_until = "") => ({ id, name: id, temporary_until });
    live.lists = {
      databases: [
        database("shop"),
        database("blog"),
        database("pc_rt_abcdefghijklmno", "2026-11-18 14:00:00.000Z"),
      ],
    };
    backups();
    expect(screen.getByRole("link", { name: /^Databases/ })).toHaveTextContent("2");
  });
});

describe("the servers the Layout shares with its pages (#33)", () => {
  function host(id: string, name: string, stream_open: boolean) {
    return { id, collectionId: "hosts", collectionName: "hosts", name, stream_open };
  }

  /** a page that shows the servers it is given */
  function SharedHosts() {
    const hosts = useHosts();
    return (
      <ul aria-label="shared servers" data-loading={hosts.loading}>
        {hosts.items.map((item) => (
          <li key={item.id}>{item.name}</li>
        ))}
      </ul>
    );
  }

  function renderPage() {
    render(
      <MemoryRouter initialEntries={["/"]}>
        <AuthProvider>
          <Routes>
            <Route element={<Layout />}>
              <Route path="/" element={<SharedHosts />} />
            </Route>
          </Routes>
        </AuthProvider>
      </MemoryRouter>,
    );
  }

  const optionsOf = (collection: string) =>
    live.asked.filter((list) => list.collection === collection).map((list) => list.options);

  it("reads the servers once, with the fields of a summary, and gives its pages that list", () => {
    live.lists = { hosts: [host("h1", "nas", true), host("h2", "pve-01", false)] };
    renderPage();
    const shared = screen.getByRole("list", { name: "shared servers" });
    expect(
      within(shared)
        .getAllByRole("listitem")
        .map((item) => item.textContent),
    ).toEqual(["nas", "pve-01"]);
    expect(shared).toHaveAttribute("data-loading", "false");
    expect(screen.getByRole("link", { name: /^Servers/ })).toHaveTextContent("2");

    const hosts = optionsOf("hosts");
    expect(hosts.length).toBeGreaterThan(0);
    for (const options of hosts) {
      expect(options).toEqual({ sort: "name", fields: hostSummaryFields });
    }
  });

  it("counts the connected agents of the shared list in the gateway card", async () => {
    vi.spyOn(api, "info").mockResolvedValue({
      gateway_running: true,
      grpc_address: "panel.example.cz:9443",
    } as PanelInfo);
    live.lists = {
      hosts: [host("h1", "nas", true), host("h2", "pve-01", false), host("h3", "rpi", true)],
    };
    renderPage();
    expect(await screen.findByText("2 of 3 agents connected")).toBeInTheDocument();
  });

  it("keeps the counts of containers and sites in place, and no list whose records move", () => {
    renderPage();
    for (const collection of ["containers", "sites"]) {
      const lists = optionsOf(collection);
      expect(lists.length).toBeGreaterThan(0);
      for (const options of lists) {
        expect(options).toEqual({ perPage: 1, inPlace: true });
      }
    }
    // an alert resolved or a database's temporary end leaves their filter
    for (const collection of ["hosts", "alerts", "databases"]) {
      for (const options of optionsOf(collection)) {
        expect(options).not.toHaveProperty("inPlace");
      }
    }
  });

  it("is what useHosts needs: without it a page fails at once", () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    expect(() => render(<SharedHosts />)).toThrow(/HostsProvider/);
  });
});

describe("a section's dot", () => {
  it("is err for a critical alert, warn for any other, none without", () => {
    expect(alertDot([{ severity: "warning" }, { severity: "critical" }])).toBe("err");
    expect(alertDot([{ severity: "warning" }])).toBe("warn");
    expect(alertDot([{ severity: "info" }])).toBe("warn");
    expect(alertDot([])).toBeUndefined();
  });
});

describe("the open alerts the Layout shares with its pages (#33)", () => {
  /** a page that shows the open alerts it is given */
  function SharedAlerts() {
    const alerts = useOpenAlerts();
    return (
      <ul aria-label="shared alerts" data-total={alerts.total}>
        {alerts.items.map((item) => (
          <li key={item.id}>{item.rule}</li>
        ))}
      </ul>
    );
  }

  function renderPage() {
    render(
      <MemoryRouter initialEntries={["/"]}>
        <AuthProvider>
          <Routes>
            <Route element={<Layout />}>
              <Route path="/" element={<SharedAlerts />} />
            </Route>
          </Routes>
        </AuthProvider>
      </MemoryRouter>,
    );
  }

  const open = () => [
    alert({ id: "a1", rule: "agent_offline", target: "h1", severity: "critical" }),
    alert({ id: "a2", rule: "certificate_expiring", target: "c1", severity: "warning" }),
    alert({ id: "a3", rule: "backup_failed", severity: "warning" }),
    alert({ id: "a4", rule: "backup_failed", severity: "critical", status: "resolved" }),
  ];

  it("reads them once, as the Overview needs them, and gives its pages that list", () => {
    live.lists = { alerts: open() };
    renderPage();
    const shared = screen.getByRole("list", { name: "shared alerts" });
    expect(
      within(shared)
        .getAllByRole("listitem")
        .map((item) => item.textContent),
    ).toEqual(["agent_offline", "certificate_expiring", "backup_failed"]);

    // one list of the alerts, every render the same: no section reads its own
    const asked = live.asked.filter((list) => list.collection === "alerts");
    expect(asked.length).toBeGreaterThan(0);
    for (const { options } of asked) {
      expect(options).toEqual(openAlertsQuery);
    }
    expect(openAlertsQuery).toEqual({
      filter: "status != 'resolved'",
      sort: "-last_seen",
      perPage: 200,
      fields:
        "id,collectionId,collectionName,rule,target,host,severity,status,first_seen,last_seen,message,updated",
    });
  });

  it("counts every open alert the panel counts, more than it sends", () => {
    live.lists = { alerts: open() };
    live.totals = { alerts: 250 };
    renderPage();
    expect(screen.getByRole("link", { name: "250 open alerts" })).toHaveAttribute("href", "/");
    expect(screen.getByRole("list", { name: "shared alerts" })).toHaveAttribute(
      "data-total",
      "250",
    );
  });

  it("says nothing of open alerts when there are none", () => {
    live.lists = { alerts: [alert({ status: "resolved" })] };
    renderPage();
    expect(screen.queryByRole("link", { name: /open alert/ })).toBeNull();
    expect(
      screen.getByRole("link", { name: /^Domains & TLS/ }).querySelector(".nav-dot"),
    ).toBeNull();
    expect(screen.getByRole("link", { name: /^Backups/ }).querySelector(".nav-dot")).toBeNull();
  });

  it("marks Domains & TLS and Backups each by the alerts of its rules only", () => {
    live.lists = { alerts: open() };
    renderPage();
    // the critical alert of a server and the resolved one of a backup mark neither
    const domains = screen.getByRole("link", { name: /^Domains & TLS/ });
    expect(domains.querySelector(".nav-dot")).toHaveClass("warn");
    expect(within(domains).getByText("1 open alert")).toHaveClass("visually-hidden");
    const backups = screen.getByRole("link", { name: /^Backups/ });
    expect(backups.querySelector(".nav-dot")).toHaveClass("warn");
    expect(within(backups).getByText("1 open alert")).toHaveClass("visually-hidden");
    expect(screen.getByRole("link", { name: "3 open alerts" })).toBeInTheDocument();
  });

  it("marks Domains & TLS err for a critical alert of a certificate or a DNS provider", () => {
    live.lists = {
      alerts: [
        alert({ id: "a1", rule: "certificate_failed", target: "c1", severity: "critical" }),
        alert({ id: "a2", rule: "dns_provider_failed", target: "p1", severity: "warning" }),
      ],
    };
    renderPage();
    const domains = screen.getByRole("link", { name: /^Domains & TLS/ });
    expect(domains.querySelector(".nav-dot")).toHaveClass("err");
    expect(within(domains).getByText("2 open alerts")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /^Backups/ }).querySelector(".nav-dot")).toBeNull();
  });

  it("is what useOpenAlerts needs: without it a page fails at once", () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    expect(() => render(<SharedAlerts />)).toThrow(/AlertsProvider/);
  });
});
