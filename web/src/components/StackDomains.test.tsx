import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { AuthProvider } from "../lib/auth";
import { edgeApi, type Certificate, type Domain, type EdgeRoute } from "../lib/edge";
import type { Stack } from "../lib/types";
import { LabelDomainsCard, ServiceDomains } from "./StackDomains";
import { ToastProvider } from "./toast";

beforeAll(() => {
  HTMLDialogElement.prototype.showModal = function showModal(this: HTMLDialogElement) {
    this.open = true;
  };
  HTMLDialogElement.prototype.close = function close(this: HTMLDialogElement) {
    this.open = false;
  };
});

afterEach(() => {
  vi.restoreAllMocks();
});

function wrap(children: React.ReactNode) {
  return render(
    <MemoryRouter>
      <AuthProvider>
        <ToastProvider>{children}</ToastProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

function domain(fields: Partial<Domain>): Domain {
  return {
    id: "d1",
    collectionId: "domains",
    collectionName: "domains",
    hostname: "shop.example.com",
    host: "h1",
    target_kind: "container",
    container: "shop-web-1",
    port: 80,
    redirect_to: "",
    redirect_permanent: false,
    tls: "auto",
    https_redirect: true,
    hsts: false,
    compress: false,
    zone: "",
    dns_status: "ok",
    dns_error: "",
    origin: "label",
    stack: "s1",
    service: "web",
    label_removed: false,
    ...fields,
  } as Domain;
}

function route(fields: Partial<EdgeRoute>): EdgeRoute {
  return {
    id: "d1",
    hostname: "shop.example.com",
    target_kind: "container",
    target: "shop-web-1:80",
    tls: "valid",
    certificate: {
      id: "c1",
      names: ["shop.example.com"],
      issuer: "acme",
      not_after: "2027-01-18T00:00:00Z",
    },
    https_redirect: true,
    hsts: false,
    compress: false,
    dns_status: "ok",
    dns_error: "",
    origin: "label",
    stack: "s1",
    service: "web",
    label_removed: false,
    password: false,
    ...fields,
  };
}

function order(fields: Partial<Certificate>): Certificate {
  return {
    id: "c2",
    names: ["admin.example.com"],
    issuer: "acme",
    fingerprint: "",
    status: "pending",
    last_error: "",
    ...fields,
  } as Certificate;
}

/** The routes of two domains: shop has its certificate, admin none yet. */
function twoRoutes(certificates: Certificate[]) {
  return {
    routes: new Map([
      ["d1", route({})],
      [
        "d2",
        route({
          id: "d2",
          hostname: "admin.example.com",
          tls: "none",
          certificate: undefined,
          password: true,
        }),
      ],
    ]),
    certificates,
  };
}

const twoDomains = [domain({}), domain({ id: "d2", hostname: "admin.example.com" })];

describe("a service's domains", () => {
  it("shows each domain with its TLS state, from the label", () => {
    wrap(<ServiceDomains domains={twoDomains} routes={twoRoutes([order({})])} />);
    const list = screen.getByRole("list", { name: "Domains" });
    const shop = within(list).getByRole("link", { name: "shop.example.com" });
    expect(shop.getAttribute("href")).toBe("https://shop.example.com");
    expect(within(list).getByText(/^HTTPS until/)).toBeTruthy();
    expect(within(list).getByRole("img", { name: "HTTPS" })).toBeTruthy();
    // no certificate yet: the panel is getting one, and it is plain HTTP until then
    expect(within(list).getByRole("link", { name: "admin.example.com" }).getAttribute("href")).toBe(
      "http://admin.example.com",
    );
    expect(within(list).getByText("Getting a certificate")).toBeTruthy();
    expect(within(list).getByRole("img", { name: "password" })).toBeTruthy();
    expect(within(list).getAllByText("from the pocketcloud.domain label")).toHaveLength(2);
  });

  it("says when the certificate is held up, and when none is on its way", () => {
    const { container } = wrap(
      <ServiceDomains
        domains={twoDomains}
        routes={twoRoutes([order({ last_error: "No ACME account: add one under Domains." })])}
      />,
    );
    const failed = screen.getByRole("link", { name: "No certificate yet: see Domains" });
    expect(failed.getAttribute("href")).toBe("/domains");
    expect(failed.getAttribute("title")).toBe("No ACME account: add one under Domains.");
    expect(screen.queryByText("Getting a certificate")).toBeNull();
    container.remove();

    // a certificate of another name is not this one's
    wrap(
      <ServiceDomains
        domains={twoDomains}
        routes={twoRoutes([order({ names: ["other.example.com"] })])}
      />,
    );
    expect(screen.getByText("HTTP only: no certificate covers this name")).toBeTruthy();
    expect(screen.queryByText("Getting a certificate")).toBeNull();
  });

  it("does not wait for the panel's own certificate of the same name (#63)", () => {
    wrap(
      <ServiceDomains
        domains={twoDomains}
        routes={twoRoutes([order({ panel: true, last_error: "Timeout during connect" })])}
      />,
    );
    expect(screen.getByText("HTTP only: no certificate covers this name")).toBeTruthy();
    expect(screen.queryByText("Getting a certificate")).toBeNull();
    expect(screen.queryByRole("link", { name: "No certificate yet: see Domains" })).toBeNull();
  });
});

const stack = {
  id: "s1",
  name: "shop",
  host: "h1",
  label_problems: [
    {
      line: 5,
      message: "web: set pocketcloud.port: the service names no port in expose or ports.",
    },
  ],
} as unknown as Stack;

describe("the labels of a stack", () => {
  it("shows what was wrong with them, with the line", () => {
    wrap(<LabelDomainsCard stack={stack} domains={[]} canAct />);
    expect(screen.getByText("1 problem with the labels:")).toBeTruthy();
    expect(screen.getByText("line 5:")).toBeTruthy();
    expect(screen.getByText(/web: set pocketcloud.port/)).toBeTruthy();
  });

  it("lets operators remove or keep the domains whose label is gone", async () => {
    const keep = vi.spyOn(edgeApi, "keepDomain").mockResolvedValue({ id: "d2" });
    const remove = vi.spyOn(edgeApi, "removeDomain").mockResolvedValue(true);
    wrap(
      <LabelDomainsCard
        stack={{ ...stack, label_problems: [] }}
        domains={[
          domain({}),
          domain({ id: "d2", hostname: "www.shop.example.com", label_removed: true }),
        ]}
        canAct
      />,
    );
    // the domain whose label is there is not offered
    expect(screen.queryByText("shop.example.com")).toBeNull();
    expect(screen.getByText("the label of web is gone")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Keep as a domain: www.shop.example.com" }));
    await waitFor(() => expect(keep).toHaveBeenCalledWith("d2"));

    fireEvent.click(screen.getByRole("button", { name: "Remove www.shop.example.com" }));
    const dialog = screen.getByRole("dialog", { name: "Remove www.shop.example.com?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(remove).toHaveBeenCalledWith("d2"));
  });

  it("does not remove a domain twice while the first removal runs", async () => {
    let finish = () => {};
    const remove = vi.spyOn(edgeApi, "removeDomain").mockImplementation(
      () =>
        new Promise<boolean>((resolve) => {
          finish = () => resolve(true);
        }),
    );
    wrap(
      <LabelDomainsCard
        stack={{ ...stack, label_problems: [] }}
        domains={[domain({ id: "d2", hostname: "www.shop.example.com", label_removed: true })]}
        canAct
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Remove www.shop.example.com" }));
    const dialog = screen.getByRole("dialog", { name: "Remove www.shop.example.com?" });
    const confirm = within(dialog).getByRole("button", { name: "Remove" });
    fireEvent.click(confirm);
    await waitFor(() => expect((confirm as HTMLButtonElement).disabled).toBe(true));
    fireEvent.click(confirm);
    expect(remove).toHaveBeenCalledTimes(1);
    finish();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("shows viewers no actions, and nothing when all is well", () => {
    const { container } = wrap(
      <LabelDomainsCard
        stack={{ ...stack, label_problems: [] }}
        domains={[domain({ label_removed: true })]}
        canAct={false}
      />,
    );
    expect(screen.queryByRole("button")).toBeNull();
    container.remove();
    wrap(
      <LabelDomainsCard stack={{ ...stack, label_problems: null }} domains={[domain({})]} canAct />,
    );
    expect(screen.queryByText("Domains from labels")).toBeNull();
  });
});
