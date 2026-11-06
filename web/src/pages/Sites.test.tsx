import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

import { RequestBars } from "../components/RequestBars";
import { ToastProvider } from "../components/toast";
import { AuthProvider } from "../lib/auth";
import { edgeApi } from "../lib/edge";
import { pb, type User } from "../lib/pb";
import { sitesApi, type Deployment, type Site } from "../lib/sites";
import { SitesPage } from "./Sites";

const lists: Record<string, unknown[]> = {};

vi.mock("../lib/realtime", () => ({
  useLiveList: (collection: string) => {
    const items = lists[collection] ?? [];
    return { items, total: items.length, loading: false, error: null, reload: () => {} };
  },
  useLiveRecord: () => ({ record: null, loading: false, error: null }),
  useNow: () => Date.parse("2026-11-05T12:00:00Z"),
}));

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

const site = {
  id: "s1",
  name: "blog",
  host: "h1",
  source: "git",
  repo_url: "https://git.example.com/me/blog.git",
  branch: "main",
  generator: "auto",
  build_image: "",
  build_cmd: "",
  output_dir: "",
  keep_releases: 5,
  https_redirect: true,
  hsts: false,
  compress: true,
  spa: false,
  not_found: "",
  basic_auth_users: [],
  git_token_set: false,
  active_deployment: "d3",
} as unknown as Site;

function deployment(fields: Partial<Deployment>): Deployment {
  return {
    site: "s1",
    status: "succeeded",
    trigger: "push",
    source: "git",
    branch: "main",
    commit_sha: "a3f9c21e00000000000000000000000000000000",
    message: "",
    requested_by: "webhook",
    command: "c-" + fields.id,
    generator: "hugo",
    image: "ghcr.io/gohugoio/hugo:v0.152.2",
    build_cmd: "hugo --minify",
    build_ms: 18000,
    size_bytes: 2_400_000,
    files: 312,
    error: "",
    on_server: true,
    upload: "",
    created: "2026-11-05 11:00:00.000Z",
    finished_at: "2026-11-05 11:58:00.000Z",
    ...fields,
  } as Deployment;
}

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider>
        <ToastProvider>
          <Routes>
            <Route path="/sites" element={<SitesPage />} />
            <Route path="/sites/:id" element={<SitesPage />} />
            <Route path="/sites/new" element={<p>the wizard</p>} />
          </Routes>
        </ToastProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.spyOn(sitesApi, "requests").mockResolvedValue({
    from: "2026-11-04T13:00:00Z",
    hours: Array(24).fill(0),
    total: 0,
  });
  vi.spyOn(edgeApi, "state").mockRejectedValue(new Error("no edge in this test"));
});

afterEach(() => {
  vi.restoreAllMocks();
  for (const key of Object.keys(lists)) {
    delete lists[key];
  }
  pb.authStore.clear();
});

describe("the sites page", () => {
  it("offers a new site when there is none, in the wizard", () => {
    signInAs("operator");
    renderAt("/sites");
    expect(screen.getByText("No sites yet")).toBeInTheDocument();
    fireEvent.click(screen.getAllByRole("button", { name: "New site" })[0]);
    expect(screen.getByText("the wizard")).toBeInTheDocument();
  });

  it("shows a site's deployments, and rolls back to one on the server", async () => {
    signInAs("operator");
    lists.sites = [site];
    lists.hosts = [{ id: "h1", name: "vps-hetzner", roles: ["docker", "edge"], status: "online" }];
    lists.deployments = [
      deployment({ id: "d3", number: 48, message: "Article: backups with restic" }),
      deployment({ id: "d2", number: 47, message: "Fix the RSS feed" }),
      deployment({
        id: "d1",
        number: 46,
        status: "failed",
        error: "hugo: template error",
        message: "New footer",
      }),
      deployment({ id: "d0", number: 45, on_server: false, message: "Old" }),
    ];
    const rollback = vi.spyOn(sitesApi, "rollback").mockResolvedValue({ command: "c9" });
    renderAt("/sites/s1");

    expect(screen.getByRole("heading", { name: "blog" })).toBeInTheDocument();
    expect(
      screen.getByText("Static site · Hugo · on vps-hetzner · /srv/sites/blog"),
    ).toBeInTheDocument();
    expect(screen.getByText("#48 · a3f9c21")).toBeInTheDocument();
    expect(screen.getByText("312 files · gzip")).toBeInTheDocument();

    const table = screen.getByRole("region", { name: "Deployments" });
    const rows = within(table).getAllByRole("row").slice(1);
    expect(within(rows[0]).getByText("active")).toBeInTheDocument();
    expect(within(rows[0]).queryByRole("button", { name: /Roll back/ })).toBeNull();
    expect(within(rows[2]).getByText("hugo: template error")).toBeInTheDocument();
    // a release that was pruned cannot come back
    expect(within(rows[3]).queryByRole("button", { name: /Roll back/ })).toBeNull();

    fireEvent.click(within(rows[1]).getByRole("button", { name: "Roll back to #47" }));
    await waitFor(() => expect(rollback).toHaveBeenCalledWith("s1", "d2"));
  });

  it("keeps a site of the LAN on the internal CA", async () => {
    signInAs("operator");
    lists.sites = [site];
    lists.domains = [
      {
        id: "dm1",
        hostname: "wiki.lan",
        host: "h1",
        target_kind: "site",
        site: "s1",
        tls: "internal",
      },
    ];
    vi.spyOn(edgeApi, "state").mockResolvedValue({
      routes: [
        {
          id: "dm1",
          hostname: "wiki.lan",
          target_kind: "site",
          target: "blog",
          tls: "valid",
          certificate: {
            id: "c1",
            names: ["wiki.lan"],
            issuer: "internal",
            not_after: "2027-12-07T00:00:00Z",
          },
        },
      ],
      edge: { role: true, in_sync: true },
    } as never);
    const create = vi.fn().mockResolvedValue({});
    const collection = pb.collection.bind(pb);
    vi.spyOn(pb, "collection").mockImplementation((name: string) =>
      name === "domains" ? ({ create, delete: vi.fn() } as never) : collection(name),
    );
    renderAt("/sites/s1");

    // it renews like one from ACME, not by hand
    expect(await screen.findByText("Internal CA · renews by itself")).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Add a domain"), { target: { value: "docs.lan" } });
    fireEvent.click(screen.getByRole("button", { name: "Connect" }));
    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(create.mock.calls[0][0]).toMatchObject({ hostname: "docs.lan", tls: "internal" });
  });

  it("gives viewers nothing to change", () => {
    signInAs("viewer");
    lists.sites = [site];
    lists.deployments = [
      deployment({ id: "d3", number: 48 }),
      deployment({ id: "d2", number: 47 }),
    ];
    renderAt("/sites/s1");
    expect(screen.queryByRole("button", { name: /Roll back/ })).toBeNull();
    expect(screen.queryByRole("button", { name: "Deploy again" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Upload ZIP" })).toBeNull();
    expect(screen.queryByLabelText("Add a domain")).toBeNull();
    for (const box of screen.getAllByRole("checkbox")) {
      expect((box as HTMLInputElement).disabled).toBe(true);
    }
  });
});

describe("the requests chart", () => {
  it("can be read with the keyboard", () => {
    const hours = Array.from({ length: 24 }, (_, index) => index);
    render(<RequestBars hours={hours} from="2026-11-04T13:00:00Z" />);
    const plot = screen.getByLabelText(/arrow keys/);
    fireEvent.focus(plot);
    expect(screen.getByText(/23 requests/)).toBeInTheDocument();
    fireEvent.keyDown(plot, { key: "ArrowLeft" });
    expect(screen.getByText(/22 requests/)).toBeInTheDocument();
    fireEvent.keyDown(plot, { key: "Home" });
    expect(screen.getByText(/0 requests/)).toBeInTheDocument();
  });

  it("has a bar and a table row for each hour, and says what a bar holds", () => {
    const hours = Array.from({ length: 24 }, (_, index) => index * 10);
    const { container } = render(<RequestBars hours={hours} from="2026-11-04T13:00:00Z" />);
    expect(container.querySelectorAll("path")).toHaveLength(23);
    expect(screen.getAllByRole("row")).toHaveLength(25);
    fireEvent.mouseEnter(container.querySelectorAll("g")[23]);
    expect(screen.getByText(/230 requests/)).toBeInTheDocument();
  });
});
