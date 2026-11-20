import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import axe from "axe-core";
import { useState } from "react";
import { MemoryRouter, Outlet, Route, Routes, useLocation } from "react-router-dom";
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

import { ToastProvider } from "../components/toast";
import i18n from "../i18n";
import { AlertsProvider, openAlertsQuery } from "../lib/alerts";
import { AuthProvider } from "../lib/auth";
import { alertLink } from "../lib/events";
import { HostsProvider, type HostSummary } from "../lib/hosts";
import { hostKind } from "../lib/overview";
import * as f from "../lib/overview.fixtures";
import { pb } from "../lib/pb";
import type { ListState } from "../lib/realtime";
import type { Alert } from "../lib/types";
import { listPage } from "../test-filter";
import { signInAs } from "./BackupRestore.fixtures";
import { OverviewPage } from "./Overview";

const live = vi.hoisted(() => ({
  lists: {} as Record<string, unknown[]>,
  /** a list that is still loading, or failed */
  state: {} as Record<string, { loading?: boolean; error?: unknown }>,
  reloads: {} as Record<string, number>,
  /** a list's count, where the panel counts more than it sends */
  totals: {} as Record<string, number>,
  asked: [] as { collection: string; options: Record<string, unknown> }[],
  now: 0,
}));
vi.mock("../lib/realtime", () => {
  const list = (
    collection: string,
    options: { filter?: string; sort?: string; perPage?: number; enabled?: boolean } = {},
  ) => {
    live.asked.push({ collection, options });
    const enabled = options.enabled ?? true;
    const state = live.state[collection] ?? {};
    const all = enabled && !state.loading ? (live.lists[collection] ?? []) : [];
    const items = listPage(all as Record<string, unknown>[], {
      ...options,
      perPage: options.perPage ?? 100_000,
    });
    return {
      items,
      total: items.length,
      loading: enabled && Boolean(state.loading),
      error: state.error ?? null,
      reload: () => {
        live.reloads[collection] = (live.reloads[collection] ?? 0) + 1;
      },
    };
  };
  return { useLiveList: list, useLiveRecords: list, useNow: () => live.now };
});

// a heartbeat of one server renders its row again, and no other
vi.mock("../lib/overview", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/overview")>();
  return { ...actual, hostKind: vi.fn(actual.hostKind) };
});

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

beforeEach(async () => {
  live.lists = { ...f.lists };
  live.now = f.now;
  await i18n.changeLanguage("cs");
  // why rpi-garaz went away, as the diagnostics plugin writes it
  vi.spyOn(pb, "collection").mockImplementation(((name: string) => ({
    getList: async () => ({
      page: 1,
      perPage: 1,
      totalItems: -1,
      totalPages: -1,
      items:
        name === "events" ? [{ id: "e1", host: "h7", data: { reason: "eof" }, created: "" }] : [],
    }),
  })) as unknown as typeof pb.collection);
});

afterEach(async () => {
  vi.restoreAllMocks();
  pb.authStore.clear();
  live.lists = {};
  live.state = {};
  live.reloads = {};
  live.totals = {};
  live.asked = [];
  await i18n.changeLanguage("en");
});

function hostsState(items: HostSummary[], fields: Partial<ListState<HostSummary>> = {}) {
  return {
    items,
    total: items.length,
    loading: false,
    error: null,
    reload: () => {
      live.reloads.hosts = (live.reloads.hosts ?? 0) + 1;
    },
    ...fields,
  };
}

function Location() {
  const location = useLocation();
  return <output data-testid="location">{location.pathname + location.search}</output>;
}

const shared = { set: null as ((hosts: ListState<HostSummary>) => void) | null };

/** The Layout's open alerts, as live.lists and live.state hold them, read
 * with the Layout's options; not asked for, as the page asks for its own
 * lists. */
function alertsState(): ListState<Alert> {
  const state = live.state.alerts ?? {};
  const all = state.loading ? [] : ((live.lists.alerts ?? []) as Alert[]);
  const items = listPage(all, openAlertsQuery);
  return {
    items,
    total: live.totals.alerts ?? items.length,
    loading: Boolean(state.loading),
    error: state.error ?? null,
    reload: () => {
      live.reloads.alerts = (live.reloads.alerts ?? 0) + 1;
    },
  };
}

/** The Layout's part: it holds the servers and the open alerts and gives
 * them to the page, so that a heartbeat renders the page again through the
 * context only. */
function HostsLayout({ initial }: { initial: ListState<HostSummary> }) {
  const [hosts, setHosts] = useState(initial);
  shared.set = setHosts;
  return (
    <HostsProvider value={hosts}>
      <AlertsProvider value={alertsState()}>
        <Outlet />
      </AlertsProvider>
    </HostsProvider>
  );
}

function page(hosts: ListState<HostSummary> = hostsState(f.hosts)) {
  const view = render(
    <MemoryRouter initialEntries={["/"]}>
      <AuthProvider>
        <ToastProvider>
          <main>
            <Routes>
              <Route element={<HostsLayout initial={hosts} />}>
                <Route index element={<OverviewPage />} />
              </Route>
              <Route path="*" element={<p>elsewhere</p>} />
            </Routes>
          </main>
          <Location />
        </ToastProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
  return { ...view, update: (next: ListState<HostSummary>) => act(() => shared.set!(next)) };
}

const location = () => screen.getByTestId("location").textContent;

function card(name: string) {
  return screen.getByRole("heading", { name, level: 2 }).closest("section")!;
}

function tile(label: RegExp) {
  return screen.getByRole("link", { name: label });
}

describe("the Overview of the design", () => {
  it("says the date and each tile's figures and lines in Czech", async () => {
    signInAs("viewer");
    page();
    expect(screen.getByText("pátek 16. 10. · 12:24")).toBeInTheDocument();

    expect(tile(/^Agenti připojení/)).toHaveTextContent("6 / 7");
    expect(within(tile(/^Agenti připojení/)).getByText("rpi-garaz bez spojení 14 min")).toHaveClass(
      "err-text",
    );
    const containers = tile(/^Kontejnery/);
    expect(containers).toHaveTextContent("38 běží");
    expect(containers).toHaveTextContent("3 zastavené · 1 se restartuje");
    expect(within(containers).getByText("1 se restartuje")).toHaveClass("warn-text");
    expect(containers).toHaveTextContent("11 stacků");
    const sites = tile(/^Weby a certifikáty/);
    expect(sites).toHaveTextContent("5 webů online");
    expect(within(sites).getByText("shop.example.org – certifikát vyprší za 12 dní")).toHaveClass(
      "warn-text",
    );
    const backups = tile(/^Zálohy za 24 h/);
    expect(backups).toHaveTextContent("11 / 12 v pořádku");
    expect(within(backups).getByText("nas/photos selhala ve 03:00")).toHaveClass("err-text");

    expect(screen.getByRole("button", { name: "Upozornění, 4 nová" })).toBeInTheDocument();
    await waitFor(() =>
      expect(
        screen.getByText("Poslední heartbeat 12:10 · spojení bylo uzavřeno"),
      ).toBeInTheDocument(),
    );
    // why it went away is read for the server that is offline only
    const events = vi.mocked(pb.collection).mock.calls.filter(([name]) => name === "events");
    expect(events).toHaveLength(1);
  });

  it("shows the sites, their certificates and deployments, and the latest runs", () => {
    signInAs("viewer");
    page();
    const sites = card("Weby a domény");
    for (const text of [
      "Let's Encrypt · 89 dní",
      "Vydává se (DNS-01)",
      "Vyprší za 12 dní",
      "ZeroSSL · 41 dní",
      "Interní CA · 301 dní",
      "nasazeno před 2 min",
      "probíhá",
      "PHP 8.3 · WordPress",
      "statický · Hugo",
    ]) {
      expect(within(sites).getByText(text)).toBeInTheDocument();
    }
    expect(within(sites).getByText("Vyprší za 12 dní")).toHaveClass("warn-text");
    expect(within(sites).getByText("+1")).toHaveClass("faint");
    expect(within(sites).getByText("obsluhují je edge proxy agentů")).toBeInTheDocument();

    const runs = card("Zálohy – poslední běhy");
    const rows = within(runs).getAllByRole("listitem");
    expect(rows).toHaveLength(5);
    expect(rows[0]).toHaveTextContent("photos");
    expect(rows[0]).toHaveTextContent("nas → backup-node");
    expect(within(rows[0]).getByText("Chyba")).toHaveAttribute(
      "title",
      "repository is already locked",
    );
    expect(rows[0].querySelector(".status-dot.err")).not.toBeNull();
    expect(rows[1]).toHaveTextContent("vaultwarden");
    expect(rows[1]).toHaveTextContent("vps-hetzner → Backblaze B2 + backup-node");
    expect(within(rows[1]).getByText("OK · 84 MB")).toHaveClass("ok-text");
    expect(rows[2]).toHaveTextContent("panel → Backblaze B2");
    expect(rows[3]).toHaveTextContent("OK · 2,1 GB");
  });

  it("reads why every offline server went away in one list, each from its newest disconnect", async () => {
    const getList = vi.fn(async () => ({
      page: 1,
      perPage: 200,
      totalItems: -1,
      totalPages: -1,
      items: [
        { id: "e3", host: "h1", data: { reason: "keepalive_timeout" }, created: "" },
        { id: "e2", host: "h7", data: { reason: "eof" }, created: "" },
        { id: "e1", host: "h1", data: { reason: "tls_error" }, created: "" },
      ],
    }));
    vi.mocked(pb.collection).mockImplementation(((name: string) =>
      name === "events" ? { getList } : {}) as unknown as typeof pb.collection);
    const hetzner = f.hosts.find((host) => host.id === "h1")!;
    live.lists.alerts = [
      ...f.alerts,
      {
        ...f.alerts[0],
        id: "a9",
        target: "h1",
        host: "h1",
        message: "vps-hetzner stopped responding",
      },
    ];
    signInAs("viewer");
    page(
      hostsState(
        f.hosts.map((host) =>
          host === hetzner
            ? {
                ...host,
                status: "offline",
                stream_open: false,
                last_seen: f.pbDate(f.now - 3_600_000),
              }
            : host,
        ),
      ),
    );
    const attention = card("Vyžaduje pozornost");
    await waitFor(() =>
      expect(
        within(attention).getByText(/keepalive bez odpovědi, možná výpadek sítě/),
      ).toBeInTheDocument(),
    );
    expect(within(attention).getByText(/spojení bylo uzavřeno/)).toBeInTheDocument();
    expect(within(attention).queryByText(/chyba TLS/)).toBeNull();
    expect(getList).toHaveBeenCalledTimes(1);
    expect(getList).toHaveBeenCalledWith(1, 200, {
      filter: '(host = "h1" || host = "h7") && kind = \'connection.disconnected\'',
      sort: "-created",
      fields: "id,host,data,created",
      skipTotal: true,
    });
  });

  it("names each alert by its subject, with its detail, critical first", async () => {
    signInAs("viewer");
    page();
    const attention = card("Vyžaduje pozornost");
    expect(within(attention).getByText("4 otevřené")).toBeInTheDocument();
    const items = within(attention).getAllByRole("listitem");
    expect(items.map((item) => item.querySelector(".list-title")?.textContent)).toEqual([
      "rpi-garaz: agent bez spojení",
      "Záloha nas/photos selhala",
      "Certifikát shop.example.org vyprší za 12 dní",
      "DNS poskytovatel WEDOS hlásí chybu",
    ]);
    expect(items[0].querySelector(".notice-mark.err")).not.toBeNull();
    expect(items[2].querySelector(".notice-mark.warn")).not.toBeNull();
    expect(within(items[1]).getByText("repository is already locked")).toBeInTheDocument();
    expect(
      within(items[2]).getByText("Auto-obnova selhala 2× – DNS-01 timeout (WEDOS API)"),
    ).toBeInTheDocument();
    expect(
      within(items[3]).getByText("WAPI answered 2051: access not allowed from this IP"),
    ).toBeInTheDocument();
    await waitFor(() =>
      expect(within(items[0]).getByText(/spojení bylo uzavřeno/)).toBeInTheDocument(),
    );
    // each links where the alert is fixed
    for (const [index, alert] of f.alerts.entries()) {
      expect(within(items[index]).getByRole("link")).toHaveAttribute("href", alertLink(alert)!);
    }
  });

  it("lists the queue without reads, measures and steps, each command linked to its object", () => {
    signInAs("viewer");
    page();
    const queue = card("Fronta příkazů");
    const items = within(queue).getAllByRole("listitem");
    expect(items.map((item) => item.querySelector(".mono")?.textContent)).toEqual([
      "site.deploy",
      "edge.apply",
      "stack.restart",
      "image.pull",
      "backup.run",
    ]);
    for (const hidden of ["db.stats", "backup.init", "site.dns", "logs.follow"]) {
      expect(within(queue).queryByText(hidden)).toBeNull();
    }
    expect(items[0]).toHaveTextContent("docs.example.cz → vps-hetzner · 12:23");
    expect(within(items[0]).getByText("odesláno")).toBeInTheDocument();
    expect(items[2]).toHaveTextContent("immich @ nas · 12:18");
    expect(items[4]).toHaveTextContent("photos @ nas · 03:00");
    expect(within(items[4]).getByText("chyba")).toBeInTheDocument();
    expect(items.map((item) => within(item).getByRole("link").getAttribute("href"))).toEqual([
      "/sites/s2",
      "/servers/h1/proxy",
      "/stacks/k1",
      "/servers/h2",
      "/backups?job=j3&target=t3",
    ]);
    // the queue the panel is asked for
    const asked = live.asked.find((item) => item.collection === "commands")!.options;
    expect(asked).toMatchObject({ perPage: 5, skipTotal: true, inPlace: true, sort: "-created" });
  });
});

describe("the Overview's links", () => {
  it("lead each tile to its page", () => {
    signInAs("viewer");
    page();
    expect(tile(/^Agenti připojení/)).toHaveAttribute("href", "/servers");
    expect(tile(/^Kontejnery/)).toHaveAttribute("href", "/stacks");
    expect(tile(/^Weby a certifikáty/)).toHaveAttribute("href", "/sites");
    expect(tile(/^Zálohy za 24 h/)).toHaveAttribute("href", "/backups");
    expect(screen.getByRole("link", { name: "Všechny certifikáty" })).toHaveAttribute(
      "href",
      "/domains",
    );
    expect(screen.getByRole("link", { name: "Detail" })).toHaveAttribute("href", "/backups");
  });

  it("go to a server from anywhere on its row", () => {
    signInAs("viewer");
    page();
    const servers = card("Servery");
    const nas = within(servers).getByRole("link", { name: "nas" }).closest("tr")!;
    expect(within(servers).getByRole("link", { name: "nas" })).toHaveAttribute(
      "href",
      "/servers/h3",
    );
    fireEvent.click(within(nas).getByText("TrueNAS SCALE", { exact: false }));
    expect(location()).toBe("/servers/h3");
  });

  it("leave a click on a row's link to the link, as one that opens a new tab", () => {
    // the browser would open the tab; jsdom only says it cannot navigate
    const stay = (event: Event) => event.preventDefault();
    document.addEventListener("click", stay);
    try {
      signInAs("viewer");
      page();
      fireEvent.click(screen.getByRole("link", { name: "nas" }), { ctrlKey: true });
      expect(location()).toBe("/");
      fireEvent.click(screen.getByRole("link", { name: "wiki.lan" }), { ctrlKey: true });
      expect(location()).toBe("/");
    } finally {
      document.removeEventListener("click", stay);
    }
  });

  it("go to a site from anywhere on its row, and each run to its job and target", () => {
    signInAs("viewer");
    page();
    const sites = card("Weby a domény");
    expect(within(sites).getByRole("link", { name: "blog.example.cz" })).toHaveAttribute(
      "href",
      "/sites/s1",
    );
    const wiki = within(sites).getByRole("link", { name: "wiki.lan" }).closest("tr")!;
    fireEvent.click(within(wiki).getByText("Interní CA · 301 dní"));
    expect(location()).toBe("/sites/s5");
  });

  it("lead each run to its job and target", () => {
    signInAs("viewer");
    page();
    const runs = card("Zálohy – poslední běhy");
    expect(
      within(runs)
        .getAllByRole("link")
        .map((link) => link.getAttribute("href")),
    ).toEqual([
      "/backups",
      "/backups?job=j3&target=t3",
      "/backups?job=j1&target=t1",
      "/backups?job=j5&target=t1",
      "/backups?job=j2&target=t1",
      "/backups?job=j4&target=t3",
    ]);
  });

  it("lead the audit log's alert to the log for operators only", () => {
    const audit = {
      ...f.alerts[0],
      id: "a9",
      rule: "audit_integrity",
      target: "audit_log",
      host: "",
      message: "Audit log integrity check failed",
    } as Alert;
    live.lists.alerts = [audit];
    signInAs("viewer");
    const viewer = page();
    const title = screen.getByText("Kontrola integrity auditu selhala");
    expect(title.closest("a")).toBeNull();
    viewer.unmount();

    signInAs("operator");
    page();
    expect(screen.getByText("Kontrola integrity auditu selhala").closest("a")).toHaveAttribute(
      "href",
      "/audit",
    );
  });

  it("bring the bell to what needs attention", () => {
    signInAs("viewer");
    page();
    const bell = screen.getByRole("button", { name: "Upozornění, 4 nová" });
    expect(bell.querySelector(".bell-badge")).toHaveTextContent("4");
    expect(bell.querySelector(".bell-badge")).toHaveAttribute("aria-hidden", "true");
    fireEvent.click(bell);
    expect(document.activeElement).toBe(within(card("Vyžaduje pozornost")).getAllByRole("link")[0]);
  });

  it("bring the bell to the card's heading when the first alert has no link, or there is none", () => {
    // a viewer's audit_integrity has no link, and as critical comes first
    const audit = {
      ...f.alerts[0],
      id: "a9",
      rule: "audit_integrity",
      target: "audit_log",
      host: "",
      message: "Audit log integrity check failed",
    } as Alert;
    const heading = () => screen.getByRole("heading", { name: "Vyžaduje pozornost", level: 2 });
    const cases: [Alert[], string][] = [
      [[audit, f.alerts[1]], "Upozornění, 2 nová"],
      [[audit], "Upozornění, 1 nové"],
      [[], "Upozornění, žádná nová"],
    ];
    signInAs("viewer");
    for (const [alerts, name] of cases) {
      live.lists.alerts = alerts;
      const view = page();
      const bell = screen.getByRole("button", { name });
      bell.focus();
      fireEvent.click(bell);
      expect(document.activeElement).not.toBe(bell);
      // not the second alert's link: nothing comes before where the focus is
      expect(heading()).toContainElement(document.activeElement as HTMLElement);
      view.unmount();
    }
  });

  it("count only the open alerts on the bell, and say when there are none", () => {
    live.lists.alerts = [
      { ...f.alerts[0], status: "acknowledged" },
      ...Array.from({ length: 120 }, (_, index) => ({ ...f.alerts[1], id: `n${index}` })),
    ];
    signInAs("viewer");
    const view = page();
    const bell = screen.getByRole("button", { name: "Upozornění, 120 nových" });
    expect(bell.querySelector(".bell-badge")).toHaveTextContent("99+");
    view.unmount();

    live.lists.alerts = [];
    page();
    const none = screen.getByRole("button", { name: "Upozornění, žádná nová" });
    expect(none.querySelector(".bell-badge")).toBeNull();
  });
});

describe("the Overview's meters", () => {
  it("turn amber from 75 % and red from 90 %, on their own track, and say so", () => {
    const host = f.host("h1", {
      name: "edge",
      cpu_percent: 74,
      mem_used_bytes: 75,
      mem_bytes: 100,
      disk_used_bytes: 90,
      disk_total_bytes: 100,
    });
    signInAs("viewer");
    page(hostsState([host, f.hosts[6]]));
    const cpu = screen.getByRole("progressbar", { name: "CPU serveru edge" });
    const mem = screen.getByRole("progressbar", { name: "Paměť serveru edge" });
    const disk = screen.getByRole("progressbar", { name: "Disk serveru edge" });
    expect(cpu).toHaveClass("progress", "ramp");
    expect(cpu).not.toHaveClass("warn");
    expect(cpu).not.toHaveClass("err");
    expect(cpu).not.toHaveAttribute("aria-valuetext");
    expect(mem).toHaveClass("ramp", "warn");
    expect(mem).toHaveAttribute("aria-valuetext", "75 %, vysoké vytížení");
    expect(disk).toHaveClass("ramp", "err");
    expect(disk).toHaveAttribute("aria-valuetext", "90 %, vysoké vytížení");
    expect(within(cpu.closest("td")!).getByText("74 %")).toBeInTheDocument();
  });

  it("show an offline server's figures as dashes on empty tracks, its count faint, and when it was seen", () => {
    signInAs("viewer");
    page();
    const row = screen.getByRole("link", { name: "rpi-garaz" }).closest("tr")!;
    expect(within(row).getAllByText("—")).toHaveLength(3);
    expect(within(row).queryAllByRole("progressbar")).toHaveLength(0);
    expect(row.querySelectorAll(".progress.empty[aria-hidden='true']")).toHaveLength(3);
    expect(within(row).getByText("před 14 min")).toBeInTheDocument();
    expect(within(row).getByText("v0.3.9")).toBeInTheDocument();
    expect(within(row).getByText("0")).toHaveClass("faint");
    // the offline server comes last
    const names = within(card("Servery"))
      .getAllByRole("row")
      .slice(1)
      .map((tr) => tr.querySelector(".cell-name")?.textContent);
    expect(names).toEqual([
      "backup-node",
      "dev-box",
      "nas",
      "pve-01",
      "vps-hetzner",
      "vps-wedos",
      "rpi-garaz",
    ]);
  });

  it("say what each server is under its name", () => {
    signInAs("viewer");
    page();
    const servers = card("Servery");
    expect(within(servers).getByText("203.0.113.24 · Edge proxy")).toHaveClass("cell-sub", "mono");
    expect(within(servers).getByText("10.0.0.30 · Cíl záloh")).toBeInTheDocument();
    expect(within(servers).getByText("10.0.0.20 · TrueNAS SCALE")).toBeInTheDocument();
  });
});

describe("the Overview's open alerts, which are the Layout's (#33)", () => {
  it("shows the alerts the Layout gives it, and reads none of its own", () => {
    signInAs("viewer");
    page();
    expect(live.asked.filter((item) => item.collection === "alerts")).toEqual([]);
    const attention = card("Vyžaduje pozornost");
    expect(within(attention).getAllByRole("listitem")).toHaveLength(f.alerts.length);
    expect(screen.getByRole("button", { name: "Upozornění, 4 nová" })).toBeInTheDocument();
  });

  it("counts every open alert the panel counts, more than the Layout holds, and says so", () => {
    live.totals.alerts = 250;
    signInAs("viewer");
    const view = page();
    const attention = card("Vyžaduje pozornost");
    expect(within(attention).getByText("250 otevřených")).toBeInTheDocument();
    expect(within(attention).getAllByRole("listitem")).toHaveLength(f.alerts.length);
    expect(
      within(attention).getByText("Vypsána jsou jen nejnovější 4 z 250 otevřených upozornění"),
    ).toBeInTheDocument();
    view.unmount();

    // the newest 200, as the Layout reads them: none is shown as all
    live.lists.alerts = Array.from({ length: 200 }, (_, index) => ({
      ...f.alerts[1],
      id: `n${index}`,
    }));
    live.totals.alerts = 221;
    page();
    const all = card("Vyžaduje pozornost");
    expect(within(all).getByText("221 otevřených")).toBeInTheDocument();
    expect(
      within(all).getByText("Vypsáno je jen nejnovějších 200 z 221 otevřených upozornění"),
    ).toBeInTheDocument();
    expect(within(all).queryByRole("button", { name: /Zobrazit všechna/ })).toBeNull();
    fireEvent.click(within(all).getByRole("button", { name: "Zobrazit nejnovějších 200" }));
    expect(within(all).getAllByRole("listitem")).toHaveLength(200);
  });

  it("follows the Layout's list as an alert opens and another resolves, without a remount", () => {
    signInAs("viewer");
    const view = page();
    const attention = card("Vyžaduje pozornost");
    expect(within(attention).getByText("rpi-garaz: agent bez spojení")).toBeInTheDocument();
    live.lists.alerts = [
      ...f.alerts.filter((alert) => alert.rule !== "agent_offline"),
      { ...f.alerts[0], id: "a9", status: "resolved" },
      { ...f.alerts[1], id: "a10", last_seen: f.pbDate(f.now - 60_000) },
    ];
    view.update(hostsState(f.hosts));
    expect(card("Vyžaduje pozornost")).toBe(attention);
    expect(within(attention).queryByText("rpi-garaz: agent bez spojení")).toBeNull();
    expect(within(attention).getAllByRole("listitem")).toHaveLength(f.alerts.length);
    expect(screen.getByRole("button", { name: "Upozornění, 4 nová" })).toBeInTheDocument();
  });
});

describe("the Overview's lists of alerts", () => {
  it("show six, then all on request, a note in its own tone", () => {
    live.lists.alerts = Array.from({ length: 9 }, (_, index) => ({
      ...f.alerts[1],
      id: `x${index}`,
      severity: index === 0 ? "info" : "warning",
      last_seen: f.pbDate(f.now - (index + 1) * 60_000),
    }));
    signInAs("viewer");
    page();
    const attention = card("Vyžaduje pozornost");
    expect(within(attention).getAllByRole("listitem")).toHaveLength(6);
    const more = within(attention).getByRole("button", { name: "Zobrazit všechna (9)" });
    expect(more).toHaveAttribute("aria-expanded", "false");
    // all of them are held: nothing to say of more
    expect(within(attention).queryByText(/Vypsán/)).toBeNull();
    fireEvent.click(more);
    const items = within(attention).getAllByRole("listitem");
    expect(items).toHaveLength(9);
    // the note comes last, with its own mark
    expect(items[8].querySelector(".notice-mark.info")).not.toBeNull();
    fireEvent.click(within(attention).getByRole("button", { name: "Zobrazit méně" }));
    expect(within(attention).getAllByRole("listitem")).toHaveLength(6);
  });

  it("say every rule the panel raises in Czech, never in the stored English", () => {
    const rules = [
      "agent_offline",
      "audit_integrity",
      "backup_failed",
      "backup_skipped",
      "backup_retention",
      "backup_check_failed",
      "backup_check_unverified",
      "recovery_kit",
      "panel_backup",
      "panel_restored",
      "backup_restore_test_failed",
      "backup_restore_test_unverified",
      "db_temporary_left",
      "certificate_failed",
      "certificate_expiring",
      "dns_provider_failed",
      "backup_restore",
    ];
    live.lists.alerts = rules.map((rule, index) => ({
      ...f.alerts[0],
      id: `r${index}`,
      rule,
      target: "x",
      host: "h3",
      message: `stored English of ${rule}`,
    }));
    signInAs("operator");
    page();
    const attention = card("Vyžaduje pozornost");
    fireEvent.click(
      within(attention).getByRole("button", { name: `Zobrazit všechna (${rules.length})` }),
    );
    expect(within(attention).getAllByRole("listitem")).toHaveLength(rules.length);
    expect(within(attention).queryByText(/stored English/)).toBeNull();
  });
});

describe("the Overview's states", () => {
  const collections = [
    "containers",
    "stacks",
    "alerts",
    "commands",
    "sites",
    "site_php",
    "domains",
    "certificates",
    "deployments",
    "backup_jobs",
    "backup_targets",
    "backup_runs",
  ];

  it("says it is loading everywhere, and claims nothing while it is", () => {
    live.state = Object.fromEntries(collections.map((name) => [name, { loading: true }]));
    signInAs("viewer");
    const { container } = page(hostsState([], { loading: true }));
    for (const name of [
      /^Agenti připojení/,
      /^Kontejnery/,
      /^Weby a certifikáty/,
      /^Zálohy za 24 h/,
    ]) {
      expect(within(tile(name)).getByText("–")).toBeInTheDocument();
      expect(within(tile(name)).getByText("Načítám…")).toBeInTheDocument();
    }
    for (const name of [
      "Servery",
      "Vyžaduje pozornost",
      "Fronta příkazů",
      "Weby a domény",
      "Zálohy – poslední běhy",
    ]) {
      expect(within(card(name)).getByText("Načítám…")).toBeInTheDocument();
    }
    expect(screen.queryByText("Nic nevyžaduje pozornost")).toBeNull();
    // the bell says nothing is new only once the alerts have loaded
    const bell = screen.getByRole("button", { name: "Upozornění, načítám…" });
    expect(bell).toHaveAttribute("title", "Upozornění, načítám…");
    expect(bell.querySelector(".bell-badge")).toBeNull();
    expect(screen.queryByText("Zatím žádné příkazy")).toBeNull();
    expect(screen.queryByText("Zatím žádné servery")).toBeNull();
    expect(screen.queryByText(/0 \/ 0/)).toBeNull();
    expect(screen.queryByText("11 stacků")).toBeNull();
    expect(container.querySelector("[data-overview-state]")).toHaveAttribute(
      "data-overview-state",
      "loading",
    );
  });

  it("says a list failed, offers to read it again, and keeps the rest of the page", () => {
    live.state = { alerts: { error: new Error("the panel is away") } };
    signInAs("viewer");
    page();
    const attention = card("Vyžaduje pozornost");
    const banner = within(attention).getByRole("alert");
    expect(banner).toHaveTextContent("Nepodařilo se načíst: the panel is away");
    fireEvent.click(within(banner).getByRole("button", { name: "Zkusit znovu" }));
    expect(live.reloads.alerts).toBe(1);
    expect(within(attention).queryByText("Nic nevyžaduje pozornost")).toBeNull();
    // nor does the bell count what it could not read
    const bell = screen.getByRole("button", { name: "Upozornění se nepodařilo načíst" });
    expect(bell.querySelector(".bell-badge")).toBeNull();
    // the rest stays
    expect(tile(/^Agenti připojení/)).toHaveTextContent("6 / 7");
    expect(within(card("Fronta příkazů")).getAllByRole("listitem")).toHaveLength(5);
  });

  it("says a tile failed, and reads its lists again", () => {
    live.state = { backup_runs: { error: new Error("timeout") } };
    signInAs("viewer");
    page();
    const backups = tile(/^Zálohy za 24 h/);
    expect(within(backups).getByText("–")).toBeInTheDocument();
    expect(within(backups).getByText("Nepodařilo se načíst: timeout")).toHaveClass("err-text");
    const runs = card("Zálohy – poslední běhy");
    fireEvent.click(within(runs).getByRole("button", { name: "Zkusit znovu" }));
    expect(live.reloads.backup_runs).toBe(1);
    expect(live.reloads.backup_jobs).toBeUndefined();
  });

  it("says what is empty once it has loaded", () => {
    live.lists = {};
    signInAs("viewer");
    page(hostsState([]));
    expect(tile(/^Agenti připojení/)).toHaveTextContent("0 / 0");
    expect(within(tile(/^Agenti připojení/)).getByText("Zatím žádné servery")).toBeInTheDocument();
    expect(tile(/^Kontejnery/)).toHaveTextContent("0 běží");
    expect(tile(/^Weby a certifikáty/)).toHaveTextContent("0 webů online");
    expect(within(tile(/^Weby a certifikáty/)).getByText("Zatím žádné weby")).toBeInTheDocument();
    expect(tile(/^Zálohy za 24 h/)).toHaveTextContent("–");
    expect(tile(/^Zálohy za 24 h/)).toHaveTextContent("Za posledních 24 h neproběhla žádná záloha");
    expect(within(card("Servery")).getByText("Zatím žádné servery")).toBeInTheDocument();
    expect(
      within(card("Vyžaduje pozornost")).getByText("Nic nevyžaduje pozornost"),
    ).toBeInTheDocument();
    expect(within(card("Fronta příkazů")).getByText("Zatím žádné příkazy")).toBeInTheDocument();
    expect(within(card("Weby a domény")).getByText("Zatím žádné weby")).toBeInTheDocument();
    expect(
      within(card("Zálohy – poslední běhy")).getByText("Zatím nic nezálohuješ"),
    ).toBeInTheDocument();
  });

  it("says no backup ran when jobs have no runs in the 24 hours", () => {
    live.lists.backup_runs = [];
    signInAs("viewer");
    page();
    expect(
      within(card("Zálohy – poslední běhy")).getByText(
        "Za posledních 24 h neproběhla žádná záloha",
      ),
    ).toBeInTheDocument();
  });

  it("counts no containers on a server while they load, or when they could not be read", () => {
    const count = () =>
      within(screen.getByRole("link", { name: "nas" }).closest("tr")!).getAllByRole("cell")[5];
    live.state = { containers: { loading: true } };
    signInAs("viewer");
    const loading = page();
    expect(count()).toHaveTextContent(/^–$/);
    expect(count()).toHaveClass("num", "faint");
    loading.unmount();

    // a first read that failed leaves no containers
    live.state = { containers: { error: new Error("timeout") } };
    live.lists.containers = [];
    const failed = page();
    expect(count()).toHaveTextContent(/^–$/);
    expect(within(tile(/^Kontejnery/)).getByText("Nepodařilo se načíst: timeout")).toBeVisible();
    failed.unmount();

    live.state = {};
    live.lists.containers = f.containers;
    page();
    expect(count()).toHaveTextContent(/^9$/);
    expect(count()).not.toHaveClass("faint");
  });

  it("claims no site online and names no server while the servers load or could not be read", () => {
    const sitesAndBackups = () => {
      for (const name of [/^Weby a certifikáty/, /^Zálohy za 24 h/]) {
        expect(within(tile(name)).getByText("–")).toBeInTheDocument();
      }
      // no site counted online on a server not known to be offline, no job without its server
      expect(screen.queryByText(/webů online/)).toBeNull();
      expect(within(tile(/^Zálohy za 24 h/)).queryByText(/selhala/)).toBeNull();
      expect(screen.queryByText(/→ backup-node/)).toBeNull();
    };
    signInAs("viewer");
    const loading = page(hostsState([], { loading: true }));
    sitesAndBackups();
    for (const name of [/^Weby a certifikáty/, /^Zálohy za 24 h/]) {
      expect(within(tile(name)).getByText("Načítám…")).toBeInTheDocument();
    }
    for (const name of ["Weby a domény", "Zálohy – poslední běhy"]) {
      expect(within(card(name)).getByText("Načítám…")).toBeInTheDocument();
    }
    loading.unmount();

    page(hostsState([], { error: new Error("forbidden") }));
    sitesAndBackups();
    for (const name of [/^Weby a certifikáty/, /^Zálohy za 24 h/]) {
      expect(within(tile(name)).getByText("Nepodařilo se načíst: forbidden")).toHaveClass(
        "err-text",
      );
    }
    for (const name of ["Weby a domény", "Zálohy – poslední běhy"]) {
      const banner = within(card(name)).getByRole("alert");
      expect(banner).toHaveTextContent("Nepodařilo se načíst: forbidden");
      fireEvent.click(within(banner).getByRole("button", { name: "Zkusit znovu" }));
    }
    expect(live.reloads.hosts).toBe(2);
    expect(live.reloads.sites).toBeUndefined();
  });

  it("says the servers failed to load, and reads them again", () => {
    signInAs("viewer");
    page(hostsState([], { error: new Error("forbidden") }));
    const servers = card("Servery");
    const banner = within(servers).getByRole("alert");
    expect(banner).toHaveTextContent("Nepodařilo se načíst: forbidden");
    expect(within(servers).queryByText("Zatím žádné servery")).toBeNull();
    fireEvent.click(within(banner).getByRole("button", { name: "Zkusit znovu" }));
    expect(live.reloads.hosts).toBe(1);
  });

  it("marks its first render and the frame after every list loaded, once each", async () => {
    const mark = vi.spyOn(performance, "mark");
    live.state = { backup_runs: { loading: true } };
    signInAs("viewer");
    const view = page();
    const root = view.container.querySelector("[data-overview-state]")!;
    expect(mark.mock.calls.filter(([name]) => name === "pc:overview:shell")).toHaveLength(1);
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(root).toHaveAttribute("data-overview-state", "loading");
    expect(mark.mock.calls.filter(([name]) => name === "pc:overview:filled")).toHaveLength(0);

    live.state = {};
    view.update(hostsState(f.hosts));
    await waitFor(() => expect(root).toHaveAttribute("data-overview-state", "filled"));
    // later renders mark nothing more, nor a list that loads again
    live.state = { backup_runs: { loading: true } };
    view.update(hostsState([...f.hosts]));
    live.state = {};
    view.update(hostsState([...f.hosts]));
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(mark.mock.calls.filter(([name]) => name === "pc:overview:shell")).toHaveLength(1);
    expect(mark.mock.calls.filter(([name]) => name === "pc:overview:filled")).toHaveLength(1);
  });

  it("is in error once loaded when a list failed", async () => {
    live.state = { stacks: { error: new Error("no") } };
    signInAs("viewer");
    const view = page();
    await waitFor(() =>
      expect(view.container.querySelector("[data-overview-state]")).toHaveAttribute(
        "data-overview-state",
        "error",
      ),
    );
  });

  it("waits for the servers too before it is filled", async () => {
    signInAs("viewer");
    const view = page(hostsState([], { loading: true }));
    await new Promise((resolve) => setTimeout(resolve, 50));
    const root = view.container.querySelector("[data-overview-state]")!;
    expect(root).toHaveAttribute("data-overview-state", "loading");
    view.update(hostsState(f.hosts));
    await waitFor(() => expect(root).toHaveAttribute("data-overview-state", "filled"));
  });
});

describe("the Overview's roles", () => {
  it("gives a viewer every card and link, and nothing to act with", () => {
    signInAs("viewer");
    page();
    expect(screen.queryByRole("link", { name: /Přidat server/ })).toBeNull();
    expect(screen.queryByRole("link", { name: "Nový web" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Historie" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Připojit další" })).toBeNull();
    for (const link of screen.getAllByRole("link")) {
      expect(link.getAttribute("href")).not.toMatch(/^\/(audit|agents|diagnostics)/);
    }
  });

  it("gives an operator a new site and the history", () => {
    signInAs("operator");
    page();
    expect(screen.getByRole("link", { name: "Nový web" })).toHaveAttribute("href", "/sites/new");
    expect(screen.getByRole("link", { name: "Historie" })).toHaveAttribute(
      "href",
      "/audit/commands",
    );
    expect(screen.queryByRole("link", { name: /Přidat server/ })).toBeNull();
    expect(screen.queryByRole("link", { name: "Připojit další" })).toBeNull();
  });

  it("gives an admin a new server too", () => {
    signInAs("admin");
    page();
    expect(screen.getByRole("link", { name: "Nový web" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Historie" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Přidat server/ })).toHaveAttribute("href", "/agents");
    expect(screen.getByRole("link", { name: "Připojit další" })).toHaveAttribute("href", "/agents");
  });

  it("offers an operator a new site when there is none", () => {
    live.lists.sites = [];
    signInAs("operator");
    page();
    const sites = card("Weby a domény");
    expect(within(sites).getByRole("link", { name: "Nový web" })).toHaveAttribute(
      "href",
      "/sites/new",
    );
  });
});

describe("the Overview, live", () => {
  it("updates its texts as a server goes offline and a container stops, without a remount", () => {
    signInAs("viewer");
    const view = page();
    const root = view.container.querySelector(".overview");
    const offline = f.hosts.map((host) =>
      host.id === "h6"
        ? {
            ...host,
            status: "offline" as const,
            stream_open: false,
            last_seen: f.pbDate(f.now - 60_000),
          }
        : host,
    );
    live.lists.containers = f.containers.map((container, index) =>
      index === 0 ? { ...container, state: "exited" } : container,
    );
    view.update(hostsState(offline));
    expect(view.container.querySelector(".overview")).toBe(root);
    expect(tile(/^Agenti připojení/)).toHaveTextContent("5 / 7");
    expect(
      within(tile(/^Agenti připojení/)).getByText("dev-box a 1 další bez spojení"),
    ).toBeInTheDocument();
    expect(tile(/^Kontejnery/)).toHaveTextContent("37 běží");
    expect(tile(/^Kontejnery/)).toHaveTextContent("4 zastavené");
    const row = screen.getByRole("link", { name: "dev-box" }).closest("tr")!;
    expect(within(row).getByText("Offline")).toBeInTheDocument();
  });

  it("renders again only the row of the server that changed", () => {
    signInAs("viewer");
    const view = page();
    const kind = vi.mocked(hostKind);
    kind.mockClear();
    const changed = f.hosts.map((host) => (host.id === "h3" ? { ...host, cpu_percent: 55 } : host));
    view.update(hostsState(changed));
    expect(kind.mock.calls.map(([host]) => (host as HostSummary).name)).toEqual(["nas"]);
    expect(screen.getByRole("progressbar", { name: "CPU serveru nas" })).toHaveAttribute(
      "aria-valuenow",
      "55",
    );
  });
});

describe("the Overview's accessibility", () => {
  it("has no violations", async () => {
    // axe measures some texts on a canvas, which jsdom has not
    vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockReturnValue(null);
    signInAs("admin");
    const { container } = page();
    await waitFor(() => expect(screen.getByText(/spojení bylo uzavřeno/)).toBeInTheDocument());
    const results = await axe.run(container, {
      // jsdom has no layout, so colours cannot be measured here
      rules: { "color-contrast": { enabled: false } },
    });
    expect(results.violations.map((violation) => `${violation.id}: ${violation.help}`)).toEqual([]);
  });
});

describe("the Overview in English", () => {
  it("says the date and the tiles in English", async () => {
    await i18n.changeLanguage("en");
    signInAs("viewer");
    page();
    expect(screen.getByText("Friday, 10/16 · 12:24")).toBeInTheDocument();
    expect(tile(/^Agents connected/)).toHaveTextContent("rpi-garaz offline for 14 min");
    expect(tile(/^Containers/)).toHaveTextContent("11 stacks");
    expect(tile(/^Sites and certificates/)).toHaveTextContent("5 sites online");
    expect(tile(/^Backups in 24 h/)).toHaveTextContent("11 / 12 OK");
    expect(screen.getByRole("button", { name: "Alerts, 4 new" })).toBeInTheDocument();
  });
});

describe("the Overview's sites and providers", () => {
  it("shows eight sites, then a link to the rest", () => {
    live.lists.sites = Array.from({ length: 11 }, (_, index) =>
      f.site(`z${index}`, {
        name: `site-${String(index).padStart(2, "0")}`,
        active_deployment: "d1",
      }),
    );
    live.lists.domains = [];
    signInAs("viewer");
    page();
    const sites = card("Weby a domény");
    // the hidden head and eight rows
    expect(within(sites).getAllByRole("row")).toHaveLength(9);
    expect(within(sites).getByRole("link", { name: "3 další weby" })).toHaveAttribute(
      "href",
      "/sites",
    );
  });

  it("reads the DNS providers only while one of their alerts is open", () => {
    const providers = () =>
      live.asked.filter((item) => item.collection === "dns_providers").at(-1)!.options;
    signInAs("viewer");
    const view = page();
    expect(providers()).toMatchObject({ enabled: true, perPage: 100 });
    view.unmount();

    live.lists.alerts = f.alerts.filter((alert) => alert.rule !== "dns_provider_failed");
    page();
    expect(providers()).toMatchObject({ enabled: false });
  });
});
