import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { ToastProvider } from "../components/toast";
import { AuthProvider } from "../lib/auth";
import { edgeApi, type EdgeState } from "../lib/edge";
import { pb, type User } from "../lib/pb";
import type { Container, Host } from "../lib/types";
import { ProxyTab } from "./EdgeProxy";

vi.mock("../lib/realtime", () => ({
  useLiveList: () => ({ items: [], total: 0, loading: false, error: null }),
}));

beforeAll(() => {
  // jsdom has no modal dialogs
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

const state: EdgeState = {
  routes: [
    {
      id: "d1",
      hostname: "app.example.com",
      target_kind: "container",
      target: "web:8080",
      tls: "valid",
      certificate: {
        id: "c1",
        names: ["app.example.com"],
        issuer: "manual",
        not_after: "2027-03-01T00:00:00Z",
      },
      https_redirect: true,
      hsts: false,
      compress: false,
      dns_status: "ok",
      dns_error: "",
      origin: "",
      stack: "",
      service: "",
      label_removed: false,
      password: false,
    },
    {
      id: "d2",
      hostname: "old.example.com",
      target_kind: "redirect",
      target: "https://new.example.com",
      tls: "none",
      https_redirect: false,
      hsts: false,
      compress: false,
      dns_status: "manual",
      dns_error: "",
      origin: "",
      stack: "",
      service: "",
      label_removed: false,
      password: false,
    },
  ],
  edge: {
    role: true,
    in_sync: true,
    applying: false,
    running: true,
    error: "",
    revision: 3,
    caddy_version: "2.11.7",
    requests_per_s: 4.2,
    image_version: "2.11.7",
    agent_has_edge: true,
    last_apply: { id: "x", status: "succeeded", error: "" },
  },
};

const host = { id: "h1", name: "edge-1", roles: ["docker", "edge"] } as unknown as Host;
const containers = [
  { id: "c-web", name: "web", system_role: "" },
  { id: "c-agent", name: "pocketcloud-agent", system_role: "agent" },
] as unknown as Container[];

function renderTab(role: User["role"], edge: EdgeState = state) {
  signInAs(role);
  vi.spyOn(edgeApi, "state").mockResolvedValue(edge);
  render(
    <MemoryRouter>
      <AuthProvider>
        <ToastProvider>
          <ProxyTab host={host} containers={containers} />
        </ToastProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

afterEach(() => {
  pb.authStore.clear();
  vi.restoreAllMocks();
});

describe("the edge proxy tab", () => {
  it("lists the routes with their TLS state", async () => {
    renderTab("viewer");

    expect(await screen.findByText("app.example.com")).toBeInTheDocument();
    expect(screen.getByText("web:8080")).toBeInTheDocument();
    expect(screen.getByRole("img", { name: "HTTPS" })).toBeInTheDocument();
    expect(screen.getByText(/^HTTPS until/)).toBeInTheDocument();
    expect(screen.getByText("HTTP only: no certificate covers this name")).toBeInTheDocument();
    expect(screen.getByText("Serving revision 3")).toBeInTheDocument();
    expect(screen.getByText("4.2 / s")).toBeInTheDocument();
  });

  it("shows viewers no actions, operators the domains, admins the certificates too", async () => {
    renderTab("viewer");
    await screen.findByText("app.example.com");
    expect(screen.queryByRole("button", { name: /Add domain/ })).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /Remove app.example.com/ }),
    ).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Upload certificate/ })).not.toBeInTheDocument();
  });

  it("lets operators add a domain to a container, but not to PocketCloud's own", async () => {
    renderTab("operator");
    await screen.findByText("app.example.com");
    expect(screen.queryByRole("button", { name: /Upload certificate/ })).not.toBeInTheDocument();

    const create = vi.fn().mockResolvedValue({});
    vi.spyOn(pb, "collection").mockReturnValue({ create } as never);

    fireEvent.click(screen.getByRole("button", { name: /Add domain/ }));
    const select = screen.getByLabelText("Container");
    expect(Array.from((select as HTMLSelectElement).options).map((option) => option.value)).toEqual(
      ["web"],
    );

    fireEvent.change(screen.getByLabelText("Domain"), { target: { value: "Shop.Example.com " } });
    fireEvent.change(screen.getByLabelText("Port"), { target: { value: "3000" } });
    fireEvent.click(screen.getByRole("button", { name: "Add" }));

    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(create.mock.calls[0][0]).toMatchObject({
      hostname: "Shop.Example.com",
      host: "h1",
      target_kind: "container",
      container: "web",
      port: 3000,
      tls: "auto",
      https_redirect: true,
    });
  });

  it("shows why the last config failed", async () => {
    renderTab("operator", {
      ...state,
      edge: {
        ...state.edge,
        in_sync: false,
        last_apply: { id: "y", status: "failed", error: "caddy refused the config" },
      },
    });
    expect(await screen.findByText("caddy refused the config")).toBeInTheDocument();
    expect(screen.getByText("The last config failed")).toBeInTheDocument();
  });
});

describe("domains from labels", () => {
  const labelled = (removed: boolean): EdgeState => ({
    ...state,
    routes: [
      {
        ...state.routes[0],
        id: "d3",
        hostname: "shop.example.com",
        origin: "label",
        stack: "s1",
        service: "web",
        label_removed: removed,
        password: true,
      },
    ],
  });

  it("links a label's domain to its stack, and offers no removal while the label is there", async () => {
    renderTab("operator", labelled(false));
    const link = await screen.findByRole("link", { name: "label of web" });
    expect(link.getAttribute("href")).toBe("/stacks/s1");
    expect(screen.getByText("password")).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /Remove shop.example.com/ }),
    ).not.toBeInTheDocument();
  });

  it("offers the removal or keeping it once the label is gone", async () => {
    const keep = vi.spyOn(edgeApi, "keepDomain").mockResolvedValue({ id: "d3" });
    renderTab("operator", labelled(true));
    const link = await screen.findByRole("link", { name: "label of web removed" });
    expect(link.getAttribute("href")).toBe("/stacks/s1");
    expect(screen.getByRole("button", { name: /Remove shop.example.com/ })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Keep as a domain: shop.example.com" }));
    await waitFor(() => expect(keep).toHaveBeenCalledWith("d3"));
  });

  it("offers viewers neither", async () => {
    renderTab("viewer", labelled(true));
    expect(await screen.findByText("label of web removed")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Keep as a domain/ })).not.toBeInTheDocument();
  });
});

describe("the edge's state", () => {
  it("shows an edge that stopped answering as stopped, not serving", async () => {
    renderTab("viewer", { ...state, edge: { ...state.edge, running: false } });
    expect(await screen.findByText("The edge does not answer")).toBeInTheDocument();
    expect(screen.queryByText("Serving revision 3")).not.toBeInTheDocument();
  });
});
