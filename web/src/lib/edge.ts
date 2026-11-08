import type { RecordModel } from "pocketbase";

import type { DnsStatus } from "./dns";
import { pb } from "./pb";

/** How a route is served: over HTTPS with a certificate, or plain HTTP. */
export type RouteTLS = "valid" | "expiring" | "expired" | "installing" | "none" | "off";

export type EdgeRoute = {
  id: string;
  hostname: string;
  target_kind: "container" | "redirect" | "site";
  target: string;
  tls: RouteTLS;
  certificate?: { id: string; names: string[]; issuer: string; not_after: string };
  https_redirect: boolean;
  hsts: boolean;
  compress: boolean;
  dns_status: DnsStatus;
  dns_error: string;
  /** label: the pocketcloud.domain label of a stack's service asks for it (#25) */
  origin: "" | "label";
  stack: string;
  service: string;
  /** the label is gone: the domain waits for someone to remove or keep it */
  label_removed: boolean;
  /** the route asks for a password */
  password: boolean;
  /** the site a domain serves (#21) */
  site?: string;
};

export type EdgeState = {
  routes: EdgeRoute[];
  edge: {
    role: boolean;
    in_sync: boolean;
    applying: boolean;
    running: boolean;
    error: string;
    revision: number;
    caddy_version: string;
    requests_per_s: number;
    image_version: string;
    agent_has_edge: boolean;
    last_apply: { id: string; status: string; error: string } | null;
  };
};

export type Domain = RecordModel & {
  hostname: string;
  host: string;
  target_kind: "container" | "redirect" | "site";
  container: string;
  port: number;
  redirect_to: string;
  redirect_permanent: boolean;
  /** internal: a certificate of the internal CA, for a LAN name (#26) */
  tls: "auto" | "off" | "internal";
  https_redirect: boolean;
  hsts: boolean;
  compress: boolean;
  zone: string;
  dns_status: DnsStatus;
  dns_error: string;
  origin: "" | "label";
  stack: string;
  service: string;
  label_removed: boolean;
  /** the site it serves, for target_kind site */
  site: string;
};

/** One attempt to get a certificate from ACME. */
export type CertificateAttempt = {
  at: string;
  ok: boolean;
  renewal?: boolean;
  challenge?: string;
  account?: string;
  error?: string;
  not_after?: string;
};

export type Certificate = RecordModel & {
  names: string[];
  issuer: "manual" | "acme" | "internal";
  not_before: string;
  not_after: string;
  /** pending and failed: from ACME, before it was first issued */
  status: "active" | "expired" | "pending" | "failed";
  chain_pem: string;
  fingerprint: string;
  subject: string;
  issuer_name: string;
  installed: Record<string, string> | null;
  note: string;
  last_error: string;
  challenge: "" | "auto" | "http-01" | "dns-01";
  /** ordered by the panel for a domain no certificate covered */
  auto: boolean;
  renew_after: string;
  /** failed attempts in a row */
  attempts: number;
  next_attempt: string;
  attempt_started: string;
  history: CertificateAttempt[] | null;
  /** asked of this ACME account alone, without the other (#27) */
  account?: "" | "primary" | "fallback";
  /** the panel's own, for its UI and API; never an edge's (#63) */
  panel?: boolean;
};

export const edgeApi = {
  state: (host: string) => pb.send<EdgeState>(`/api/pc/hosts/${host}/edge`, {}),
  apply: (host: string) =>
    pb.send<{ command: string | null }>(`/api/pc/hosts/${host}/edge/apply`, { method: "POST" }),
  upload: (body: { chain_pem: string; key_pem: string; note: string }) =>
    pb.send<{ id: string }>("/api/pc/certificates", { method: "POST", body }),
  removeCertificate: (id: string) => pb.send(`/api/pc/certificates/${id}`, { method: "DELETE" }),
  /** keeps the domain of a label that is gone as a domain of its own */
  keepDomain: (id: string) =>
    pb.send<{ id: string }>(`/api/pc/domains/${id}/keep`, { method: "POST" }),
  removeDomain: (id: string) => pb.collection("domains").delete(id),
};

/** The tone of a route's lock in the design: green when valid, blue while it is put in place. */
export function tlsTone(tls: RouteTLS): "ok" | "warn" | "err" | "info" | "off" {
  switch (tls) {
    case "valid":
      return "ok";
    case "expiring":
      return "warn";
    case "expired":
      return "err";
    case "installing":
      return "info";
    default:
      return "off";
  }
}

/** Whether the edge is serving what the panel wants, is on its way there, or failed. */
export function edgeCondition(
  edge: EdgeState["edge"],
): "synced" | "applying" | "failed" | "stopped" | "waiting" {
  if (edge.applying) {
    return "applying";
  }
  // an edge that served a config but whose admin API does not answer
  if (edge.revision > 0 && !edge.running) {
    return "stopped";
  }
  if (edge.in_sync && !edge.error) {
    return "synced";
  }
  if (
    edge.error ||
    edge.last_apply?.status === "failed" ||
    edge.last_apply?.status === "timed_out"
  ) {
    return "failed";
  }
  return "waiting";
}
