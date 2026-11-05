import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ToastProvider } from "../components/toast";
import { AuthProvider } from "../lib/auth";
import { pb, type User } from "../lib/pb";
import { provisionApi } from "../lib/sites";
import { SiteNewPage } from "./SiteNew";

const lists: Record<string, unknown[]> = {};
const records: Record<string, unknown> = {};

vi.mock("../lib/realtime", () => ({
  useLiveList: (collection: string) => {
    const items = lists[collection] ?? [];
    return { items, total: items.length, loading: false, error: null, reload: () => {} };
  },
  useLiveRecord: (collection: string, id?: string) => ({
    record: id ? (records[`${collection}/${id}`] ?? null) : null,
    loading: false,
    error: null,
  }),
  useNow: () => Date.parse("2026-11-05T12:00:00Z"),
}));

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

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider>
        <ToastProvider>
          <Routes>
            <Route path="/sites/new" element={<SiteNewPage />} />
            <Route path="/sites/:id" element={<p>the site page</p>} />
          </Routes>
        </ToastProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

const edge = {
  id: "h1",
  name: "vps-hetzner",
  roles: ["docker", "edge"],
  status: "online",
  public_ip4: "203.0.114.24",
  public_ip6: "",
};

function step(type: string, fields: Record<string, unknown>) {
  return {
    id: `c-${type}`,
    type,
    status: "succeeded",
    host_name: "vps-hetzner",
    started_at: "2026-11-05 12:31:00.000Z",
    finished_at: "2026-11-05 12:31:02.000Z",
    progress: null,
    result: null,
    error: "",
    ...fields,
  };
}

afterEach(() => {
  vi.restoreAllMocks();
  for (const key of Object.keys(lists)) {
    delete lists[key];
  }
  for (const key of Object.keys(records)) {
    delete records[key];
  }
  pb.authStore.clear();
});

describe("the new-site wizard", () => {
  it("shows the zone and its records while the name is typed, and starts the setup", async () => {
    signInAs("operator");
    lists.hosts = [edge];
    lists.acme_accounts = [
      {
        id: "a1",
        role: "primary",
        directory_url: "https://acme-v02.api.letsencrypt.org/directory",
      },
    ];
    const preview = vi.spyOn(provisionApi, "preview").mockResolvedValue({
      hostname: "docs.example.com",
      zone: "example.com",
      provider: "Cloudflare",
      kind: "cloudflare",
      mode: "automatic",
      records: [
        { type: "A", name: "docs.example.com", value: "203.0.114.24", ttl: 300 },
        { type: "CNAME", name: "www.docs.example.com", value: "docs.example.com", ttl: 300 },
      ],
      taken: [],
    });
    const start = vi
      .spyOn(provisionApi, "start")
      .mockResolvedValue({ provision: "p1", site: "s1" });
    renderAt("/sites/new");

    expect(screen.getByRole("heading", { name: "New site" })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /PHP application/ })).toBeDisabled();
    expect(screen.getByLabelText("Certificate authority")).toHaveValue("Let's Encrypt");
    // a step-by-step plan before anything runs
    expect(screen.getByText("DNS propagation checked")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Domain"), { target: { value: "Docs.example.com" } });
    expect(
      await screen.findByText(
        "The zone example.com is at Cloudflare: PocketCloud sets the DNS records.",
      ),
    ).toBeInTheDocument();
    expect(preview).toHaveBeenLastCalledWith("docs.example.com", "h1", true);
    const table = screen.getByRole("region", { name: "The records the domain needs" });
    expect(within(table).getByText("www.docs.example.com")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Repository"), {
      target: { value: "https://git.example.com/me/docs.git" },
    });
    fireEvent.change(screen.getByLabelText("Access token"), { target: { value: "tok" } });
    fireEvent.change(screen.getByLabelText("Build image"), { target: { value: "node:22" } });
    fireEvent.change(screen.getByLabelText("Generator"), { target: { value: "none" } });
    fireEvent.click(screen.getByRole("button", { name: "Create and deploy" }));

    await waitFor(() => expect(start).toHaveBeenCalled());
    expect(start.mock.calls[0][0]).toEqual({
      site: {
        name: "docs",
        host: "h1",
        source: "git",
        repo_url: "https://git.example.com/me/docs.git",
        branch: "main",
        generator: "none",
        // served as it is: nothing to build, whatever the hidden fields held
        build_image: "",
        build_cmd: "",
        output_dir: "",
        https_redirect: true,
        compress: true,
        git_token: "tok",
      },
      hostname: "docs.example.com",
      www: true,
      challenge: "auto",
    });
    expect(start.mock.calls[0][1]).toBeUndefined();
  });

  it("says when the name is taken, and starts nothing", async () => {
    signInAs("operator");
    lists.hosts = [edge];
    vi.spyOn(provisionApi, "preview").mockResolvedValue({
      hostname: "shop.example.com",
      zone: "",
      provider: "",
      kind: "",
      mode: "manual",
      records: [],
      taken: ["shop.example.com"],
    });
    renderAt("/sites/new");
    fireEvent.click(screen.getByRole("radio", { name: /Placeholder/ }));
    fireEvent.change(screen.getByLabelText("Domain"), { target: { value: "shop.example.com" } });
    expect(await screen.findByText("shop.example.com is a domain already.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create and deploy" })).toBeDisabled();
  });

  it("follows a setup step by step, and retries the step that failed", async () => {
    signInAs("operator");
    records["commands/p1"] = {
      id: "p1",
      type: "site.provision",
      status: "running",
      host_name: "vps-hetzner",
      requested_label: "op@example.com",
      payload: {
        site: "s1",
        site_name: "docs",
        hostname: "docs.example.com",
        www: "www.docs.example.com",
      },
      progress: { step: "site.certificate", paused: "c-site.certificate" },
    };
    lists.commands = [
      step("site.create", { result: { name: "docs", server: "vps-hetzner" } }),
      step("site.dns", {
        result: {
          mode: "automatic",
          provider: "Cloudflare",
          records: [
            { type: "A", name: "docs.example.com", value: "203.0.114.24", ttl: 300 },
            { type: "CNAME", name: "www.docs.example.com", value: "docs.example.com", ttl: 300 },
          ],
        },
      }),
      step("site.propagation", { result: { resolvers: ["1.1.1.1:53", "8.8.8.8:53"] } }),
      step("site.deploy", {
        result: { generator: "mkdocs", build_ms: 6000, files: 41, size_bytes: 1_100_000 },
      }),
      step("site.certificate", { status: "failed", error: "acme: too many certificates" }),
    ];
    const retry = vi.spyOn(provisionApi, "retry").mockResolvedValue();
    renderAt("/sites/new?setup=p1");

    expect(screen.getByText("Step 6 of 7")).toBeInTheDocument();
    expect(screen.getByText("A + CNAME (Cloudflare API)")).toBeInTheDocument();
    expect(screen.getByText("1.1.1.1 · 8.8.8.8 · the zone's name servers")).toBeInTheDocument();
    expect(screen.getByText("mkdocs · 6 s")).toBeInTheDocument();
    expect(screen.getByText("acme: too many certificates")).toBeInTheDocument();
    // the summary takes the form's place
    expect(screen.getByText("docs.example.com · www.docs.example.com")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Retry the step" }));
    await waitFor(() => expect(retry).toHaveBeenCalledWith("p1"));
    expect(screen.getByRole("button", { name: "Cancel the setup" })).toBeInTheDocument();
  });

  it("shows the records to make by hand while it waits for them", () => {
    signInAs("operator");
    records["commands/p2"] = {
      id: "p2",
      type: "site.provision",
      status: "running",
      host_name: "vps-hetzner",
      payload: { site: "s2", site_name: "manual", hostname: "manual.example.org" },
      progress: null,
    };
    lists.commands = [
      step("site.create", { result: { name: "manual", server: "vps-hetzner" } }),
      step("site.dns", {
        result: {
          mode: "manual",
          records: [{ type: "A", name: "manual.example.org", value: "203.0.114.24", ttl: 300 }],
        },
      }),
      step("site.propagation", {
        status: "running",
        finished_at: "",
        progress: { message: "Waiting for manual.example.org" },
      }),
    ];
    renderAt("/sites/new?setup=p2");
    expect(screen.getByText("A, to make by hand")).toBeInTheDocument();
    const table = screen.getByRole("region", { name: "Records to make at the registrar" });
    expect(within(table).getByText("203.0.114.24")).toBeInTheDocument();
    expect(screen.getByText("Waiting for manual.example.org")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry the step" })).toBeNull();
  });
});
