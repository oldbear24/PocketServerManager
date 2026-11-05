import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { AuthProvider } from "../lib/auth";
import { edgeApi, type Domain, type EdgeRoute } from "../lib/edge";
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

describe("a service's domains", () => {
  it("shows each domain with its TLS state, from the label", () => {
    wrap(
      <ServiceDomains
        domains={[domain({}), domain({ id: "d2", hostname: "admin.example.com" })]}
        routes={
          new Map([
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
          ])
        }
      />,
    );
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
    expect(screen.getByText("1 label could not be used:")).toBeTruthy();
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

    fireEvent.click(screen.getByRole("button", { name: "Keep www.shop.example.com as a domain" }));
    await waitFor(() => expect(keep).toHaveBeenCalledWith("d2"));

    fireEvent.click(screen.getByRole("button", { name: "Remove www.shop.example.com" }));
    const dialog = screen.getByRole("dialog", { name: "Remove www.shop.example.com?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(remove).toHaveBeenCalledWith("d2"));
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
