import { describe, expect, it } from "vitest";

import { covers, domainTls } from "./certificates";
import type { Certificate } from "./edge";

const now = Date.parse("2026-10-16T10:24:00Z");
const day = 24 * 3600 * 1000;
const pbDate = (ms: number) => new Date(ms).toISOString().replace("T", " ");

function cert(id: string, fields: Partial<Certificate> = {}): Certificate {
  return {
    id,
    collectionId: "certificates",
    collectionName: "certificates",
    names: ["shop.example.org"],
    issuer: "acme",
    not_before: pbDate(now - 30 * day),
    not_after: pbDate(now + 60 * day),
    status: "active",
    chain_pem: "",
    fingerprint: `fp-${id}`,
    subject: "",
    issuer_name: "CN=R11,O=Let's Encrypt,C=US",
    installed: { h1: `${id}-file` },
    note: "",
    last_error: "",
    challenge: "",
    auto: true,
    renew_after: pbDate(now + 30 * day),
    attempts: 0,
    next_attempt: "",
    attempt_started: "",
    history: null,
    panel: false,
    ...fields,
  } as Certificate;
}

const domain = { hostname: "shop.example.org", tls: "auto" as const, host: "h1" };

describe("covers", () => {
  // the cases of TestCovers in internal/edge/config_test.go, which edge.Covers passes
  it("matches as the edge does", () => {
    const cases: [string[], string, boolean][] = [
      [["app.example.com"], "app.example.com", true],
      [["App.Example.com"], "app.example.com", true],
      [["*.example.com"], "app.example.com", true],
      [["*.example.com"], "example.com", false],
      [["*.example.com"], "a.b.example.com", false],
      [["*.example.com"], "*.example.com", true],
      [["example.com"], "*.example.com", false],
      [["other.example.com"], "app.example.com", false],
    ];
    for (const [names, hostname, want] of cases) {
      expect([names, hostname, covers({ names }, hostname)]).toEqual([names, hostname, want]);
    }
  });

  it("takes no wildcard for a name of one label", () => {
    expect(covers({ names: ["*.nas"] }, "nas")).toBe(false);
    expect(covers({ names: ["nas"] }, "nas")).toBe(true);
  });

  it("reads the domain's name in any case, as the edge lowers both", () => {
    expect(covers({ names: ["app.example.com"] }, "App.Example.com")).toBe(true);
    expect(covers({ names: ["*.example.com"] }, "APP.example.COM")).toBe(true);
  });
});

describe("a domain's TLS", () => {
  it("is off when the domain asks for plain HTTP, whatever covers it", () => {
    expect(domainTls([cert("c1")], { ...domain, tls: "off" }, now)).toEqual({ kind: "off" });
  });

  it("is none without a certificate or an order", () => {
    expect(domainTls([], domain, now)).toEqual({ kind: "none" });
    expect(domainTls([cert("c1", { names: ["blog.example.org"] })], domain, now)).toEqual({
      kind: "none",
    });
  });

  // coveringCertificate in internal/panel/edge.go: the one that lasts
  // longest, an expired one only when there is no other
  it("takes the certificate that lasts longest", () => {
    const short = cert("short", { not_after: pbDate(now + 10 * day) });
    const long = cert("long", { not_after: pbDate(now + 80 * day) });
    const expired = cert("expired", { not_after: pbDate(now - day) });
    expect(domainTls([short, long, expired], domain, now)).toMatchObject({
      kind: "cert",
      cert: { id: "long" },
      state: "valid",
    });
    expect(domainTls([expired, short], domain, now)).toMatchObject({
      cert: { id: "short" },
      state: "expiring",
    });
    expect(domainTls([expired], domain, now)).toMatchObject({
      cert: { id: "expired" },
      state: "expired",
    });
  });

  it("takes a wildcard one level above the name only", () => {
    const wildcard = cert("wild", { names: ["*.example.org"] });
    expect(domainTls([wildcard], domain, now)).toMatchObject({
      kind: "cert",
      cert: { id: "wild" },
    });
    expect(domainTls([wildcard], { ...domain, hostname: "a.shop.example.org" }, now)).toEqual({
      kind: "none",
    });
    expect(domainTls([wildcard], { ...domain, hostname: "example.org" }, now)).toEqual({
      kind: "none",
    });
  });

  // suits in internal/panel/edge.go: the internal CA's for internal TLS,
  // any other for automatic TLS
  it("takes only the internal CA's certificates for internal TLS, and none of them otherwise", () => {
    const internal = cert("int", {
      issuer: "internal",
      issuer_name: "CN=PocketCloud Internal CA",
      not_after: pbDate(now + 300 * day),
    });
    const acme = cert("acme", { not_after: pbDate(now + 80 * day) });
    expect(domainTls([acme, internal], { ...domain, tls: "internal" }, now)).toMatchObject({
      cert: { id: "int" },
    });
    expect(domainTls([acme], { ...domain, tls: "internal" }, now)).toEqual({ kind: "none" });
    expect(domainTls([internal, acme], domain, now)).toMatchObject({ cert: { id: "acme" } });
    expect(domainTls([internal], domain, now)).toEqual({ kind: "none" });
  });

  // TestThePanelsCertificateIsNoEdgesAndFollowsItsName: the panel's own is
  // never an edge's
  it("never takes the panel's own certificate", () => {
    const own = cert("own", { panel: true, not_after: pbDate(now + 89 * day) });
    expect(domainTls([own], domain, now)).toEqual({ kind: "none" });
    expect(domainTls([own, cert("c1")], domain, now)).toMatchObject({ cert: { id: "c1" } });
    // nor its order
    expect(domainTls([{ ...own, fingerprint: "" }], domain, now)).toEqual({ kind: "none" });
  });

  it("takes no certificate without a key, which the edge cannot install", () => {
    const keyless = cert("keyless", { fingerprint: "", issuer: "manual", status: "pending" });
    expect(domainTls([keyless], domain, now)).toEqual({ kind: "none" });
  });

  it("is installing while the certificate is not on the domain's server", () => {
    const c1 = cert("c1", { installed: { h2: "c1-file" } });
    expect(domainTls([c1], domain, now)).toEqual({ kind: "installing", cert: c1 });
    expect(domainTls([cert("c2", { installed: null })], domain, now)).toMatchObject({
      kind: "installing",
    });
  });

  it("falls back to the ACME order the panel is getting for the name", () => {
    const order = cert("order", {
      fingerprint: "",
      status: "pending",
      installed: null,
      challenge: "dns-01",
      attempt_started: pbDate(now - 60_000),
    });
    expect(domainTls([order], domain, now)).toEqual({
      kind: "cert",
      cert: order,
      state: "issuing",
    });
    const failed = { ...order, attempt_started: "", status: "failed" as const };
    expect(domainTls([failed], domain, now)).toMatchObject({ state: "failed" });
    // a certificate that covers the name comes first
    expect(domainTls([order, cert("c1")], domain, now)).toMatchObject({ cert: { id: "c1" } });
    // the panel orders none for internal TLS
    expect(domainTls([order], { ...domain, tls: "internal" }, now)).toEqual({ kind: "none" });
  });

  it("says a certificate's state as the Domains page does", () => {
    const renewing = cert("c1", {
      attempts: 2,
      last_error: "DNS-01 timeout",
      not_after: pbDate(now + 12 * day),
    });
    expect(domainTls([renewing], domain, now)).toMatchObject({ state: "renewal_failed" });
  });
});
