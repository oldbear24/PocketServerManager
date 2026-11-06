import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { AuthProvider } from "../lib/auth";
import {
  caName,
  certificatesApi,
  certificateState,
  needsAction,
  parseNames,
  providerFor,
  validity,
  type AcmeAccount,
} from "../lib/certificates";
import type { DnsProvider, DnsZone } from "../lib/dns";
import type { Certificate } from "../lib/edge";
import { pb } from "../lib/pb";
import type { Host } from "../lib/types";
import { AcmeAccountsCard } from "./AcmeAccounts";
import { CertificatesCard, RequestCertificateDialog } from "./Certificates";
import { ToastProvider } from "./toast";

const live = vi.hoisted(() => ({ items: [] as unknown[] }));
vi.mock("../lib/realtime", () => ({
  useLiveList: () => ({ items: live.items, loading: false, error: null, total: live.items.length }),
}));

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
  vi.useRealTimers();
  pb.authStore.clear();
  live.items = [];
});

const day = 24 * 60 * 60 * 1000;
const now = Date.parse("2026-11-05T12:00:00Z");

function iso(ms: number) {
  return new Date(ms).toISOString().replace("T", " ");
}

function cert(fields: Partial<Certificate>): Certificate {
  return {
    id: "c1",
    collectionId: "certificates",
    collectionName: "certificates",
    names: ["shop.example.com"],
    issuer: "acme",
    not_before: iso(now - 16 * day),
    not_after: iso(now + 74 * day),
    status: "active",
    chain_pem: "-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----\n",
    fingerprint: "ab12",
    subject: "CN=shop.example.com",
    issuer_name: "CN=R11,O=Let's Encrypt,C=US",
    installed: { h1: "c1-0123456789abcdef" },
    note: "",
    last_error: "",
    challenge: "auto",
    auto: true,
    renew_after: iso(now + 44 * day),
    attempts: 0,
    next_attempt: "",
    attempt_started: "",
    history: [{ at: iso(now - 16 * day), ok: true, challenge: "http-01", account: "primary" }],
    ...fields,
  } as Certificate;
}

describe("the state of a certificate", () => {
  it("follows its validity and its attempts", () => {
    expect(certificateState(cert({}), now)).toBe("valid");
    expect(certificateState(cert({ not_after: iso(now + 10 * day) }), now)).toBe("expiring");
    expect(certificateState(cert({ not_after: iso(now - day) }), now)).toBe("expired");
    expect(certificateState(cert({ attempts: 2 }), now)).toBe("renewal_failed");
    expect(certificateState(cert({ attempt_started: iso(now - 60_000) }), now)).toBe("issuing");
    // an attempt that started an hour ago did not finish
    expect(certificateState(cert({ attempt_started: iso(now - 3_600_000) }), now)).toBe("valid");

    const unissued = { fingerprint: "", chain_pem: "", not_before: "", not_after: "" };
    expect(certificateState(cert({ ...unissued, status: "pending" }), now)).toBe("pending");
    expect(certificateState(cert({ ...unissued, status: "failed", attempts: 1 }), now)).toBe(
      "failed",
    );
    // an uploaded one that is failing to install is still valid
    expect(certificateState(cert({ issuer: "manual", attempts: 0 }), now)).toBe("valid");
  });

  it("needs action when it failed, expires soon or waits for something", () => {
    expect(needsAction(cert({}), now)).toBe(false);
    expect(needsAction(cert({ attempts: 1 }), now)).toBe(true);
    expect(needsAction(cert({ not_after: iso(now + 3 * day) }), now)).toBe(true);
    const waiting = cert({ fingerprint: "", status: "pending", last_error: "Add an ACME account" });
    expect(needsAction(waiting, now)).toBe(true);
    expect(needsAction({ ...waiting, last_error: "" }, now)).toBe(false);
  });

  it("measures the lifetime left", () => {
    expect(validity(cert({}), now)).toEqual({ days: 74, left: 74 / 90 });
    expect(validity(cert({ not_after: iso(now - day) }), now)).toEqual({ days: 0, left: 0 });
    expect(validity(cert({ not_before: "", not_after: "" }), now)).toEqual({ days: 0, left: 0 });
  });

  it("reads the CA's name and the names typed", () => {
    expect(caName("CN=R11,O=Let's Encrypt,C=US")).toBe("Let's Encrypt");
    expect(caName("CN=Pebble Intermediate CA 1234")).toBe("Pebble Intermediate CA 1234");
    expect(parseNames("*.Example.com,\nexample.com. example.com")).toEqual([
      "*.example.com",
      "example.com",
    ]);
  });

  it("finds the provider of a name's zone, the longest one that is managed", () => {
    const zones = [
      { id: "z1", name: "example.com", provider: "p1", managed: true },
      { id: "z2", name: "shop.example.com", provider: "p2", managed: false },
      { id: "z3", name: "eu.example.com", provider: "p3", managed: true },
    ] as DnsZone[];
    const providers = [
      { id: "p1", name: "Cloudflare" },
      { id: "p2", name: "WEDOS" },
      { id: "p3", name: "Hetzner" },
    ] as DnsProvider[];
    expect(providerFor("*.shop.example.com", zones, providers)?.name).toBe("Cloudflare");
    expect(providerFor("a.eu.example.com", zones, providers)?.name).toBe("Hetzner");
    expect(providerFor("example.org", zones, providers)).toBeUndefined();
  });
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

const hosts = [{ id: "h1", name: "vps-1" }] as Host[];

function card(certificates: Certificate[], canRetry: boolean) {
  return wrap(
    <CertificatesCard
      certificates={certificates}
      hosts={hosts}
      zones={[]}
      providers={[]}
      accounts={[]}
      canRetry={canRetry}
      isAdmin={canRetry}
      loading={false}
      onRequest={() => undefined}
    />,
  );
}

describe("the certificates table", () => {
  const failed = cert({
    id: "c2",
    names: ["blog.example.com"],
    fingerprint: "",
    chain_pem: "",
    not_before: "",
    not_after: "",
    status: "failed",
    attempts: 1,
    installed: null,
    last_error: "blog.example.com: Connection refused (connection)",
    history: [
      { at: iso(now - 60_000), ok: false, challenge: "http-01", error: "Connection refused" },
    ],
  });

  it("shows the lifetime left as a meter with its words, and what the CA said", () => {
    vi.useFakeTimers({ now, shouldAdvanceTime: true });
    card([cert({}), failed], false);
    const meter = screen.getByRole("meter", { name: "Validity left" });
    expect(meter.getAttribute("aria-valuenow")).toBe("82");
    expect(meter.getAttribute("aria-valuetext")).toMatch(/^74 days · until /);
    expect(screen.getByText("vps-1")).toBeTruthy();
    expect(screen.getAllByText("Let's Encrypt")).toHaveLength(2);
    expect(screen.getByText("blog.example.com: Connection refused (connection)")).toBeTruthy();
    // without the right to retry, there is no Retry
    expect(screen.queryByRole("button", { name: /Try the certificate/ })).toBeNull();
  });

  it("tries a failed certificate again, and filters what needs action", async () => {
    vi.useFakeTimers({ now, shouldAdvanceTime: true });
    const retry = vi.spyOn(certificatesApi, "retry").mockResolvedValue({ id: "c2" });
    card([cert({}), failed], true);

    // a valid certificate has nothing to try again
    expect(screen.getAllByRole("button", { name: /Try the certificate/ })).toHaveLength(1);
    fireEvent.click(
      screen.getByRole("button", { name: "Try the certificate for blog.example.com again" }),
    );
    await waitFor(() => expect(retry).toHaveBeenCalledWith("c2"));

    fireEvent.click(screen.getByRole("button", { name: /^Needs action/ }));
    expect(screen.queryByText("shop.example.com")).toBeNull();
    expect(screen.getByText("blog.example.com")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Internal" }));
    expect(screen.getByText("Nothing here")).toBeTruthy();
  });

  it("has one action per row: Retry when it fails, Progress while it is issued", () => {
    vi.useFakeTimers({ now, shouldAdvanceTime: true });
    const issuing = cert({
      id: "c3",
      names: ["docs.example.com"],
      attempt_started: iso(now - 5000),
    });
    card([cert({}), failed, issuing], true);

    const retry = screen.getByRole("button", {
      name: "Try the certificate for blog.example.com again",
    });
    expect(retry).toHaveClass("warning");
    const progress = screen.getByRole("button", {
      name: "How far the certificate for docs.example.com got",
    });
    expect(progress).toHaveTextContent("Progress");
    fireEvent.click(progress);
    expect(screen.getByRole("dialog", { name: "docs.example.com" })).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Details of the certificate for shop.example.com" }),
    ).toHaveTextContent("Detail");
  });

  it("shows a renewal that waits as needing action", () => {
    vi.useFakeTimers({ now, shouldAdvanceTime: true });
    const waiting = cert({
      renew_after: iso(now - day),
      last_error: "Waiting for the edge of vps-1 to serve shop.example.com.",
    });
    expect(certificateState(waiting, now)).toBe("renewal_waiting");
    expect(needsAction(waiting, now)).toBe(true);
    card([waiting], true);
    expect(screen.getByText("Renewal waiting")).toBeTruthy();
    expect(screen.getByRole("button", { name: /^Needs action \(1\)/ })).toBeTruthy();
  });

  it("opens the details with the history and the chain, named after the certificate", () => {
    vi.useFakeTimers({ now, shouldAdvanceTime: true });
    card([cert({}), failed], true);
    const opener = screen.getByRole("button", {
      name: "Details of the certificate for shop.example.com",
    });
    opener.focus();
    fireEvent.click(opener);
    const dialog = screen.getByRole("dialog", { name: "shop.example.com" });
    expect(within(dialog).getByText("Issued", { selector: ".timeline-title" })).toBeTruthy();
    expect(within(dialog).getByText("Certificate chain (PEM)")).toBeTruthy();
    expect(
      within(dialog).getByRole("region", { name: "Certificate chain (PEM)" }).textContent,
    ).toContain("BEGIN CERTIFICATE");
    // a valid certificate offers no Retry here either
    expect(within(dialog).queryByRole("button", { name: "Retry" })).toBeNull();
    fireEvent.click(within(dialog).getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(opener);
  });

  it("keeps the details open when Escape closes the removal question", () => {
    card([failed], true);
    fireEvent.click(
      screen.getByRole("button", { name: "Details of the certificate for blog.example.com" }),
    );
    const drawer = screen.getByRole("dialog", { name: "blog.example.com" });
    expect(within(drawer).getByText("Not issued", { selector: ".timeline-title" })).toBeTruthy();
    expect(within(drawer).getByText("Connection refused")).toBeTruthy();
    expect(within(drawer).getByText("by the panel, for a domain")).toBeTruthy();
    fireEvent.click(within(drawer).getByRole("button", { name: "Remove" }));
    const question = screen.getByRole("dialog", { name: /Remove the certificate/ });
    fireEvent(question, new Event("cancel", { cancelable: true }));
    expect(screen.getByRole("dialog", { name: "blog.example.com" })).toBeTruthy();
    expect(screen.queryByRole("dialog", { name: /Remove the certificate/ })).toBeNull();
  });
});

const accounts = [
  { id: "a1", role: "primary", directory_url: "https://acme-v02.api.letsencrypt.org/directory" },
  { id: "a2", role: "fallback", directory_url: "https://acme.zerossl.com/v2/DV90" },
] as AcmeAccount[];

describe("requesting a certificate", () => {
  it("sends the names typed and the challenge", async () => {
    const request = vi.spyOn(certificatesApi, "request").mockResolvedValue({ id: "c9" });
    const onClose = vi.fn();
    wrap(<RequestCertificateDialog accounts={accounts} onClose={onClose} />);

    // both accounts in turn, unless one is chosen
    expect((screen.getByLabelText("Certificate authority") as HTMLSelectElement).value).toBe(
      "acme",
    );
    const confirm = screen.getByRole("button", { name: "Issue certificate" });
    expect((confirm as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Names"), {
      target: { value: "*.example.com\nexample.com" },
    });
    fireEvent.change(screen.getByLabelText("Validation"), { target: { value: "dns-01" } });
    fireEvent.click(confirm);

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(request).toHaveBeenCalledWith(["*.example.com", "example.com"], "dns-01", "acme", "");
  });

  it("asks one ACME CA alone when it is chosen", async () => {
    const request = vi.spyOn(certificatesApi, "request").mockResolvedValue({ id: "c9" });
    const onClose = vi.fn();
    wrap(<RequestCertificateDialog accounts={accounts} onClose={onClose} />);

    const ca = screen.getByLabelText("Certificate authority");
    expect(
      within(ca)
        .getAllByRole("option")
        .map((option) => option.textContent),
    ).toEqual([
      "Let's Encrypt, then ZeroSSL if it fails",
      "Let's Encrypt only (primary)",
      "ZeroSSL only (fallback)",
      "Internal CA",
    ]);
    fireEvent.change(ca, { target: { value: "fallback" } });
    fireEvent.change(screen.getByLabelText("Names"), { target: { value: "shop.example.com" } });
    fireEvent.click(screen.getByRole("button", { name: "Issue certificate" }));

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(request).toHaveBeenCalledWith(["shop.example.com"], "auto", "acme", "fallback");
  });

  it("says when there is no ACME account to ask", () => {
    wrap(<RequestCertificateDialog accounts={[]} onClose={() => undefined} />);
    expect(screen.getByText(/Set an ACME account in Settings/)).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Certificate authority"), {
      target: { value: "internal" },
    });
    expect(screen.queryByText(/Set an ACME account in Settings/)).toBeNull();
  });

  it("asks the internal CA for LAN names, with no validation to choose", async () => {
    const request = vi.spyOn(certificatesApi, "request").mockResolvedValue({ id: "c9" });
    const onClose = vi.fn();
    wrap(<RequestCertificateDialog accounts={accounts} onClose={onClose} />);

    fireEvent.change(screen.getByLabelText("Certificate authority"), {
      target: { value: "internal" },
    });
    expect(screen.queryByLabelText("Validation")).toBeNull();
    expect(screen.getByText(/refuse a wildcard right under a top-level name/)).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Names"), { target: { value: "nas.lan" } });
    fireEvent.click(screen.getByRole("button", { name: "Issue certificate" }));

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(request).toHaveBeenCalledWith(["nas.lan"], "auto", "internal", "");
  });

  it("shows why the panel refused", async () => {
    vi.spyOn(certificatesApi, "request").mockRejectedValue(
      new Error("A wildcard (*.x.com) can only be checked with DNS-01."),
    );
    wrap(<RequestCertificateDialog accounts={accounts} onClose={() => undefined} />);
    fireEvent.change(screen.getByLabelText("Names"), { target: { value: "*.x.com" } });
    fireEvent.click(screen.getByRole("button", { name: "Issue certificate" }));
    expect(
      await screen.findByText("A wildcard (*.x.com) can only be checked with DNS-01."),
    ).toBeTruthy();
  });
});

describe("the ACME accounts in Settings", () => {
  it("asks ZeroSSL for its EAB keys and registers the account", async () => {
    const set = vi.spyOn(certificatesApi, "setAccount").mockResolvedValue({} as AcmeAccount);
    wrap(<AcmeAccountsCard />);

    // the primary account is set up first
    const primary = screen.getByRole("region", { name: "Primary CA" });
    expect(within(primary).queryByLabelText("EAB key ID")).toBeNull();
    fireEvent.change(within(primary).getByLabelText("Certificate authority"), {
      target: { value: "zerossl" },
    });
    fireEvent.change(within(primary).getByLabelText("Contact email"), {
      target: { value: "admin@example.com" },
    });
    fireEvent.change(within(primary).getByLabelText("EAB key ID"), { target: { value: "kid-1" } });
    fireEvent.change(within(primary).getByLabelText("EAB HMAC key"), {
      target: { value: "secret" },
    });
    fireEvent.click(within(primary).getByRole("button", { name: "Save and register" }));

    await waitFor(() =>
      expect(set).toHaveBeenCalledWith("primary", {
        directory_url: "https://acme.zerossl.com/v2/DV90",
        email: "admin@example.com",
        eab_kid: "kid-1",
        eab_hmac: "secret",
      }),
    );
  });

  it("shows what the CA said when it refused, and keeps a stored HMAC key", async () => {
    live.items = [
      {
        id: "a1",
        role: "primary",
        directory_url: "https://acme.example.org/dir",
        email: "admin@example.com",
        eab_kid: "kid-1",
        status: "ok",
        registered_at: iso(Date.now() - day),
      },
    ];
    const set = vi
      .spyOn(certificatesApi, "setAccount")
      .mockRejectedValue(
        new Error(
          "The CA did not take the account: the email domain is not allowed (invalidContact)",
        ),
      );
    wrap(<AcmeAccountsCard />);

    const primary = screen.getByRole("region", { name: "Primary CA" });
    fireEvent.click(within(primary).getByRole("button", { name: "Change" }));
    expect((within(primary).getByLabelText("ACME directory URL") as HTMLInputElement).value).toBe(
      "https://acme.example.org/dir",
    );
    expect(within(primary).getByLabelText("EAB HMAC key").getAttribute("placeholder")).toMatch(
      /stored/,
    );
    fireEvent.click(within(primary).getByRole("button", { name: "Save and register" }));
    expect(await within(primary).findByText(/invalidContact/)).toBeTruthy();
    // no HMAC typed: the same key ID keeps the stored one
    expect(set).toHaveBeenCalledWith("primary", {
      directory_url: "https://acme.example.org/dir",
      email: "admin@example.com",
      eab_kid: "kid-1",
      eab_hmac: "",
    });
  });
});
