import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

import { ToastProvider } from "../components/toast";
import { AuthProvider } from "../lib/auth";
import { caApi } from "../lib/certificates";
import { dnsApi, type DnsKind, type DnsProvider, type DnsZone } from "../lib/dns";
import type { Certificate, Domain } from "../lib/edge";
import { alertLink } from "../lib/events";
import { pb, type User } from "../lib/pb";
import type { Alert, Host } from "../lib/types";
import { DomainsPage } from "./Domains";

const live = vi.hoisted(() => ({ lists: {} as Record<string, unknown[]> }));
vi.mock("../lib/realtime", () => ({
  useLiveList: (collection: string) => {
    const items = live.lists[collection] ?? [];
    return { items, loading: false, error: null, total: items.length };
  },
  useNow: () => Date.parse("2026-11-05T12:00:00Z"),
}));

beforeAll(() => {
  HTMLDialogElement.prototype.showModal = function showModal(this: HTMLDialogElement) {
    this.open = true;
  };
  HTMLDialogElement.prototype.close = function close(this: HTMLDialogElement) {
    this.open = false;
  };
  Element.prototype.scrollIntoView = function scrollIntoView() {};
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

const providers = [
  {
    id: "p1",
    name: "Cloudflare",
    kind: "cloudflare",
    host: "",
    status: "ok",
    last_check: "",
    last_error: "",
  },
  {
    id: "p2",
    name: "WEDOS (WAPI)",
    kind: "wedos",
    host: "",
    status: "error",
    last_check: "",
    last_error: "WAPI refuses requests from 203.0.113.24: add the address to its allow-list",
  },
  {
    id: "p3",
    name: "AdGuard Home",
    kind: "adguard",
    host: "h1",
    status: "ok",
    last_check: "",
    last_error: "",
  },
] as DnsProvider[];

const zones = [
  { id: "z1", provider: "p1", name: "example.cz", managed: true, status: "ok" },
  { id: "z2", provider: "p2", name: "example.org", managed: true, status: "error" },
  { id: "z3", provider: "p3", name: "lan", managed: true, status: "ok" },
  { id: "z4", provider: "p1", name: "example.net", managed: false, status: "ok" },
] as DnsZone[];

const domains = [
  { id: "d1", hostname: "shop.example.org", zone: "z2" },
  { id: "d2", hostname: "status.example.org", zone: "z2" },
  { id: "d3", hostname: "wiki.lan", zone: "z3" },
] as Domain[];

const hosts = [
  { id: "h1", name: "pve-01", status: "online" },
  { id: "h2", name: "nas", status: "offline" },
] as Host[];

const kinds: DnsKind[] = [
  {
    kind: "cloudflare",
    label: "Cloudflare",
    fields: [{ name: "api_token", secret: true }],
    lists_zones: true,
    manual: false,
  },
  {
    kind: "wedos",
    label: "WEDOS",
    fields: [
      { name: "username", secret: false },
      { name: "password", secret: true },
    ],
    lists_zones: false,
    manual: false,
  },
  {
    kind: "adguard",
    label: "AdGuard Home",
    fields: [
      { name: "url", secret: false },
      { name: "password", secret: true, optional: true },
    ],
    lists_zones: false,
    manual: false,
    agent: true,
  },
];

const certificate = {
  id: "c1",
  names: ["shop.example.org"],
  issuer: "acme",
  status: "failed",
  not_before: "",
  not_after: "",
  chain_pem: "",
  fingerprint: "",
  installed: {},
  last_error: "The CA refused the order",
  challenge: "auto",
  auto: true,
  attempts: 2,
  history: [],
} as unknown as Certificate;

function Location() {
  const location = useLocation();
  return <output data-testid="location">{location.pathname + location.search}</output>;
}

function page(path = "/domains") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider>
        <ToastProvider>
          <Routes>
            <Route path="/domains" element={<DomainsPage />} />
          </Routes>
          <Location />
        </ToastProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  live.lists = {
    dns_providers: providers,
    dns_zones: zones,
    domains,
    hosts,
    certificates: [certificate],
    acme_accounts: [],
  };
  vi.spyOn(caApi, "info").mockReturnValue(new Promise(() => {}));
  vi.spyOn(dnsApi, "kinds").mockResolvedValue(kinds);
});

afterEach(() => {
  vi.restoreAllMocks();
  pb.authStore.clear();
});

describe("the DNS providers", () => {
  it("shows each as a card, with the fix for one that fails", () => {
    signInAs("admin");
    page();

    const wedos = screen.getByRole("article", { name: "WEDOS (WAPI)" });
    expect(wedos).toHaveClass("warn");
    expect(within(wedos).getByText("API error")).toBeInTheDocument();
    expect(within(wedos).getByText(/WAPI refuses requests from 203.0.113.24/)).toHaveClass(
      "warn-text",
    );
    expect(within(wedos).getByRole("button", { name: "Fix WEDOS (WAPI)" })).toHaveTextContent(
      "Fix",
    );

    const cloudflare = screen.getByRole("article", { name: "Cloudflare" });
    expect(cloudflare).not.toHaveClass("warn");
    expect(within(cloudflare).getByText("CF")).toBeInTheDocument();
    expect(within(cloudflare).getByText("example.cz, example.net")).toBeInTheDocument();
    expect(within(cloudflare).getByText("Token with Zone.DNS:Edit")).toBeInTheDocument();
    expect(within(cloudflare).getByRole("button", { name: "Manage Cloudflare" })).toBeTruthy();

    const adguard = screen.getByRole("article", { name: "AdGuard Home" });
    expect(within(adguard).getByText("lan (internal rewrites)")).toBeInTheDocument();
    expect(within(adguard).getByText("through the agent of pve-01")).toBeInTheDocument();

    // the kinds not connected yet, and the way to connect one
    const more = screen.getByRole("article", { name: "Hetzner DNS and others" });
    expect(within(more).getByText("Not connected")).toBeInTheDocument();
    expect(within(more).getByText("Also supported: Hetzner DNS, Pi-hole.")).toBeInTheDocument();
    fireEvent.click(within(more).getByRole("button", { name: "Connect" }));
    expect(screen.getByRole("dialog", { name: "Connect a DNS provider" })).toBeInTheDocument();
  });

  it("marks a provider whose managed zone fails", () => {
    signInAs("admin");
    live.lists.dns_providers = [providers[0]];
    live.lists.dns_zones = [
      { ...zones[0], status: "error", last_error: "the token cannot edit this zone" },
    ];
    page();
    const card = screen.getByRole("article", { name: "Cloudflare" });
    expect(card).toHaveClass("warn");
    expect(
      within(card).getByText("example.cz: the token cannot edit this zone"),
    ).toBeInTheDocument();
  });

  it("is a card to connect the first one when there is none", () => {
    signInAs("admin");
    live.lists.dns_providers = [];
    live.lists.dns_zones = [];
    page();
    expect(screen.getByRole("article", { name: "No DNS provider yet" })).toBeInTheDocument();
    expect(screen.getByText("No zones yet")).toBeInTheDocument();
  });

  it("lets a viewer look, not change", () => {
    signInAs("viewer");
    page();
    expect(screen.queryByRole("button", { name: "Fix WEDOS (WAPI)" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Connect" })).toBeNull();
    expect(screen.queryByRole("button", { name: /Issue certificate/ })).toBeNull();
  });
});

describe("a warning's link", () => {
  it("brings the failing provider into view", async () => {
    signInAs("admin");
    page("/domains?provider=p2");
    await waitFor(() =>
      expect(document.activeElement).toBe(screen.getByRole("article", { name: "WEDOS (WAPI)" })),
    );
    // the link is used once: it is not kept in the address
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/domains$/);
  });

  it("opens the certificate it is about", async () => {
    signInAs("admin");
    page("/domains?certificate=c1");
    expect(await screen.findByRole("dialog", { name: "shop.example.org" })).toBeInTheDocument();
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/domains$/);
  });

  it("leads from an alert to what it is about", () => {
    const alert = { rule: "", target: "", host: "" } as Alert;
    expect(alertLink({ ...alert, rule: "certificate_failed", target: "c1" })).toBe(
      "/domains?certificate=c1",
    );
    expect(alertLink({ ...alert, rule: "certificate_expiring", target: "c1" })).toBe(
      "/domains?certificate=c1",
    );
    expect(alertLink({ ...alert, rule: "dns_provider_failed", target: "p2" })).toBe(
      "/domains?provider=p2",
    );
    expect(alertLink({ ...alert, rule: "agent_offline", target: "h1", host: "h1" })).toBe(
      "/servers/h1",
    );
    expect(alertLink({ ...alert, rule: "audit_integrity" })).toBeNull();
  });
});

describe("managing a provider", () => {
  it("shows why it fails and replaces its credentials", async () => {
    signInAs("admin");
    const change = vi.spyOn(dnsApi, "change").mockResolvedValue({
      provider: { ...providers[1], status: "ok", last_error: "" },
      zones: [],
    });
    page();
    fireEvent.click(screen.getByRole("button", { name: "Fix WEDOS (WAPI)" }));
    const drawer = screen.getByRole("dialog", { name: "WEDOS (WAPI)" });
    expect(within(drawer).getByText(/WAPI refuses requests/)).toBeInTheDocument();

    fireEvent.change(await within(drawer).findByLabelText("User name"), {
      target: { value: "admin@example.cz" },
    });
    expect(within(drawer).getByLabelText("WAPI password")).toHaveAttribute("type", "password");
    fireEvent.change(within(drawer).getByLabelText("WAPI password"), {
      target: { value: "new-secret" },
    });
    fireEvent.click(within(drawer).getByRole("button", { name: "Replace the credentials" }));

    await waitFor(() =>
      expect(change).toHaveBeenCalledWith("p2", {
        credentials: { username: "admin@example.cz", password: "new-secret" },
      }),
    );
    expect(
      await screen.findByText("The credentials of WEDOS (WAPI) were replaced."),
    ).toBeInTheDocument();
  });

  it("adds and removes the zones of a kind that cannot list them", async () => {
    signInAs("admin");
    const add = vi.spyOn(dnsApi, "addZone").mockResolvedValue({ provider: providers[1], zones });
    const removeZone = vi.spyOn(dnsApi, "removeZone").mockResolvedValue({});
    const setManaged = vi.spyOn(dnsApi, "setManaged").mockResolvedValue(zones[1]);
    page();
    fireEvent.click(screen.getByRole("button", { name: "Fix WEDOS (WAPI)" }));
    const drawer = screen.getByRole("dialog", { name: "WEDOS (WAPI)" });

    fireEvent.change(await within(drawer).findByLabelText("Add a zone"), {
      target: { value: "shop.example.cz" },
    });
    fireEvent.click(within(drawer).getByRole("button", { name: "Add" }));
    await waitFor(() => expect(add).toHaveBeenCalledWith("p2", "shop.example.cz"));

    fireEvent.click(within(drawer).getByLabelText("PocketCloud manages example.org"));
    await waitFor(() => expect(setManaged).toHaveBeenCalledWith("z2", false));

    fireEvent.click(within(drawer).getByRole("button", { name: "Remove the zone example.org" }));
    await waitFor(() => expect(removeZone).toHaveBeenCalledWith("z2"));
  });

  it("lists the zones of a kind that lists its own, with nothing to add", async () => {
    signInAs("admin");
    page();
    fireEvent.click(screen.getByRole("button", { name: "Manage Cloudflare" }));
    const drawer = screen.getByRole("dialog", { name: "Cloudflare" });
    expect(await within(drawer).findByLabelText("API token")).toBeInTheDocument();
    expect(within(drawer).getByText("example.net")).toBeInTheDocument();
    expect(within(drawer).queryByLabelText("Add a zone")).toBeNull();
    expect(within(drawer).queryByRole("button", { name: /Remove the zone/ })).toBeNull();
  });

  it("moves a DNS server of the LAN to another server's agent", async () => {
    signInAs("admin");
    const change = vi.spyOn(dnsApi, "change").mockResolvedValue({
      provider: { ...providers[2], host: "h2" },
      zones: [],
    });
    page();
    fireEvent.click(screen.getByRole("button", { name: "Manage AdGuard Home" }));
    const drawer = screen.getByRole("dialog", { name: "AdGuard Home" });
    const server = within(drawer).getByLabelText("Server whose agent reaches it");
    expect(within(server).getByRole("option", { name: "nas (offline)" })).toBeInTheDocument();
    fireEvent.change(server, { target: { value: "h2" } });
    await waitFor(() => expect(change).toHaveBeenCalledWith("p3", { host: "h2" }));
  });

  it("tests a provider and says when it still fails", async () => {
    signInAs("admin");
    vi.spyOn(dnsApi, "test").mockResolvedValue({
      provider: { ...providers[1], last_error: "WAPI still refuses 203.0.113.24" },
      zones: [],
    });
    page();
    fireEvent.click(screen.getByRole("button", { name: "Fix WEDOS (WAPI)" }));
    const drawer = screen.getByRole("dialog", { name: "WEDOS (WAPI)" });
    fireEvent.click(within(drawer).getByRole("button", { name: "Test" }));
    expect(await within(drawer).findByText("WAPI still refuses 203.0.113.24")).toBeInTheDocument();
  });

  it("removes a provider once asked", async () => {
    signInAs("admin");
    const remove = vi.spyOn(dnsApi, "remove").mockResolvedValue({});
    page();
    fireEvent.click(screen.getByRole("button", { name: "Manage Cloudflare" }));
    const drawer = screen.getByRole("dialog", { name: "Cloudflare" });
    fireEvent.click(within(drawer).getByRole("button", { name: "Remove Cloudflare" }));
    const question = screen.getByRole("dialog", { name: "Remove Cloudflare?" });
    fireEvent.click(within(question).getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(remove).toHaveBeenCalledWith("p1"));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Cloudflare" })).toBeNull());
  });
});

describe("the zones", () => {
  it("lists each with its provider, the names in it and its state", () => {
    signInAs("viewer");
    page();
    const table = screen.getByRole("region", { name: "Zones" });
    const rows = within(table).getAllByRole("row");
    const org = rows.find((row) => within(row).queryByText("example.org"))!;
    expect(within(org).getByText("WEDOS (WAPI)")).toBeInTheDocument();
    expect(within(org).getByText("2 hosts · shop, status")).toBeInTheDocument();
    expect(within(org).getByText("API error")).toBeInTheDocument();
    const lan = rows.find((row) => within(row).queryByText("lan"))!;
    expect(within(lan).getByText("1 host · wiki")).toBeInTheDocument();
    const net = rows.find((row) => within(row).queryByText("example.net"))!;
    expect(within(net).getByText("not managed by PocketCloud")).toBeInTheDocument();
  });
});
