import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { DomainDnsPanel } from "../components/DomainDns";
import { ToastProvider } from "../components/toast";
import { AuthProvider } from "../lib/auth";
import { dnsApi, type DnsKind } from "../lib/dns";
import { pb, type User } from "../lib/pb";
import { ConnectDialog } from "./Domains";

beforeAll(() => {
  HTMLDialogElement.prototype.showModal = function showModal(this: HTMLDialogElement) {
    this.open = true;
  };
  HTMLDialogElement.prototype.close = function close(this: HTMLDialogElement) {
    this.open = false;
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

const kinds: DnsKind[] = [
  { kind: "manual", label: "Manual", fields: null, lists_zones: false, manual: true },
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
];

function wrap(children: React.ReactNode) {
  return render(
    <MemoryRouter>
      <AuthProvider>
        <ToastProvider>{children}</ToastProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
  pb.authStore.clear();
});

describe("connecting a DNS provider", () => {
  it("sends the credentials the kind asks for, as a secret field", async () => {
    vi.spyOn(dnsApi, "kinds").mockResolvedValue(kinds);
    const connect = vi.spyOn(dnsApi, "connect").mockResolvedValue({
      provider: { id: "p1", name: "Cloudflare", kind: "cloudflare", status: "ok" } as never,
      zones: [
        { id: "z1", name: "example.com" } as never,
        { id: "z2", name: "example.org" } as never,
      ],
    });
    const onClose = vi.fn();
    wrap(<ConnectDialog hosts={[]} onClose={onClose} />);

    const token = await screen.findByLabelText("API token");
    expect(token).toHaveAttribute("type", "password");
    // Cloudflare lists its zones: nothing to name
    expect(screen.queryByLabelText("Zones, separated by commas")).not.toBeInTheDocument();
    fireEvent.change(token, { target: { value: "cf-secret" } });
    fireEvent.click(screen.getAllByRole("button", { name: "Connect a DNS provider" }).at(-1)!);

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(connect).toHaveBeenCalledWith({
      name: "Cloudflare",
      kind: "cloudflare",
      credentials: { api_token: "cf-secret" },
      zones: [],
    });
    expect(await screen.findByText("Cloudflare is connected, with 2 zones.")).toBeInTheDocument();
  });

  it("asks for the zones of a kind that cannot list them, and shows why it failed", async () => {
    vi.spyOn(dnsApi, "kinds").mockResolvedValue(kinds);
    const connect = vi
      .spyOn(dnsApi, "connect")
      .mockRejectedValue(new Error("WEDOS cannot list its zones: name at least one."));
    wrap(<ConnectDialog hosts={[]} onClose={vi.fn()} />);

    fireEvent.change(await screen.findByLabelText("Provider"), { target: { value: "wedos" } });
    expect(screen.getByText(/allow-list/)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Zones, separated by commas"), {
      target: { value: "example.cz, shop.example.cz" },
    });
    fireEvent.click(screen.getAllByRole("button", { name: "Connect a DNS provider" }).at(-1)!);

    expect(
      await screen.findByText("WEDOS cannot list its zones: name at least one."),
    ).toBeInTheDocument();
    expect(connect.mock.calls[0][0].zones).toEqual(["example.cz", "shop.example.cz"]);
  });
});

describe("connecting a DNS server of the LAN", () => {
  it("reaches AdGuard Home through the agent of the server chosen, with zones of one label", async () => {
    vi.spyOn(dnsApi, "kinds").mockResolvedValue([
      ...kinds,
      {
        kind: "adguard",
        label: "AdGuard Home",
        fields: [
          { name: "url", secret: false },
          { name: "username", secret: false, optional: true },
          { name: "password", secret: true, optional: true },
        ],
        lists_zones: false,
        manual: false,
        agent: true,
      },
    ]);
    const connect = vi.spyOn(dnsApi, "connect").mockResolvedValue({
      provider: { id: "p1", name: "Home", kind: "adguard", status: "ok" } as never,
      zones: [{ id: "z1", name: "lan" } as never],
    });
    const hosts = [
      { id: "h1", name: "pve-01", status: "online" },
      { id: "h2", name: "nas", status: "offline" },
    ] as never;
    wrap(<ConnectDialog hosts={hosts} onClose={vi.fn()} />);

    fireEvent.change(await screen.findByLabelText("Provider"), { target: { value: "adguard" } });
    expect(
      screen.getByText(/through the agent of a server on the same network/),
    ).toBeInTheDocument();
    const confirm = screen.getAllByRole("button", { name: "Connect a DNS provider" }).at(-1)!;
    // no server, no way to reach it
    expect(confirm).toBeDisabled();
    expect(screen.getByRole("option", { name: "nas (offline)" })).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Server whose agent reaches it"), {
      target: { value: "h1" },
    });
    const url = screen.getByLabelText("Address of the web interface");
    expect(url).toHaveAttribute("placeholder", "http://192.168.1.2:3000");
    fireEvent.change(url, { target: { value: "http://192.168.1.2:3000" } });
    fireEvent.change(screen.getByLabelText("Password"), { target: { value: "secret" } });
    const zones = screen.getByLabelText("Zones, separated by commas");
    expect(zones).toHaveAttribute("placeholder", "lan, home.arpa");
    fireEvent.change(zones, { target: { value: "lan" } });
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Home" } });
    fireEvent.click(confirm);

    await waitFor(() => expect(connect).toHaveBeenCalled());
    expect(connect.mock.calls[0][0]).toEqual({
      name: "Home",
      kind: "adguard",
      credentials: { url: "http://192.168.1.2:3000", password: "secret" },
      host: "h1",
      zones: ["lan"],
    });
  });
});

describe("switching kinds", () => {
  it("takes the manual kind, which asks for no credentials", async () => {
    vi.spyOn(dnsApi, "kinds").mockResolvedValue(kinds);
    wrap(<ConnectDialog hosts={[]} onClose={vi.fn()} />);
    fireEvent.change(await screen.findByLabelText("Provider"), { target: { value: "manual" } });
    expect(screen.getByLabelText("Zones, separated by commas")).toBeInTheDocument();
    expect(screen.queryByLabelText("API token")).not.toBeInTheDocument();
  });

  it("sends no zones typed for another kind to one that lists its own", async () => {
    vi.spyOn(dnsApi, "kinds").mockResolvedValue(kinds);
    const connect = vi.spyOn(dnsApi, "connect").mockResolvedValue({
      provider: { id: "p1", name: "Cloudflare", kind: "cloudflare", status: "ok" } as never,
      zones: [],
    });
    wrap(<ConnectDialog hosts={[]} onClose={vi.fn()} />);
    fireEvent.change(await screen.findByLabelText("Provider"), { target: { value: "wedos" } });
    fireEvent.change(screen.getByLabelText("Zones, separated by commas"), {
      target: { value: "example.cz" },
    });
    fireEvent.change(screen.getByLabelText("Provider"), { target: { value: "cloudflare" } });
    fireEvent.change(screen.getByLabelText("API token"), { target: { value: "t" } });
    fireEvent.click(screen.getAllByRole("button", { name: "Connect a DNS provider" }).at(-1)!);
    await waitFor(() => expect(connect).toHaveBeenCalled());
    expect(connect.mock.calls[0][0].zones).toEqual([]);
  });
});

describe("a domain's DNS", () => {
  it("shows the exact records of manual mode and how far they resolve", async () => {
    signInAs("operator");
    const domain = vi.spyOn(dnsApi, "domain").mockResolvedValue({
      status: "manual",
      error: "",
      zone: "",
      manual: true,
      problem: "",
      records: [
        {
          name: "shop.example.cz",
          hostname: "shop.example.cz",
          type: "A",
          value: "203.0.114.7",
          ttl: 300,
        },
        {
          name: "shop.example.cz",
          hostname: "shop.example.cz",
          type: "AAAA",
          value: "2a01:4f8::7",
          ttl: 300,
        },
      ],
      propagation: [
        {
          record: { name: "shop.example.cz", type: "A", value: "203.0.114.7" },
          answers: [
            { server: "1.1.1.1:53", ok: true },
            { server: "8.8.8.8:53", ok: false },
          ],
          resolved: false,
        },
        {
          record: { name: "shop.example.cz", type: "AAAA", value: "2a01:4f8::7" },
          answers: [
            { server: "1.1.1.1:53", ok: true },
            { server: "8.8.8.8:53", ok: true },
          ],
          resolved: true,
        },
      ],
    });
    wrap(<DomainDnsPanel domainId="d1" />);

    expect(await screen.findByText("203.0.114.7")).toBeInTheDocument();
    expect(screen.getByText("2a01:4f8::7")).toBeInTheDocument();
    expect(screen.getByText("1 of 2 servers")).toBeInTheDocument();
    expect(screen.getByText("everywhere")).toBeInTheDocument();
    expect(screen.getByText(/Create these records at the DNS host/)).toBeInTheDocument();
    expect(domain).toHaveBeenCalledWith("d1", true);
    // nothing to set again by hand in manual mode
    expect(screen.queryByRole("button", { name: "Set the records again" })).not.toBeInTheDocument();
  });

  it("says what is missing before records can be made", async () => {
    vi.spyOn(dnsApi, "domain").mockResolvedValue({
      status: "error",
      error: "",
      zone: "example.com",
      manual: false,
      problem:
        "Set the public IP address of vps on its server page, so its domains can point at it.",
      records: [],
      provider: { id: "p1", name: "Cloudflare", kind: "cloudflare" },
    });
    wrap(<DomainDnsPanel domainId="d2" />);
    expect(await screen.findByText(/Set the public IP address of vps/)).toBeInTheDocument();
  });
});
