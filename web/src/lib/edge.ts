import type { RecordModel } from "pocketbase";

import type { DnsStatus } from "./dns";
import { pb } from "./pb";

/** How a route is served: over HTTPS with a certificate, or plain HTTP. */
export type RouteTLS = "valid" | "expiring" | "expired" | "installing" | "none" | "off";

export type EdgeRoute = {
  id: string;
  hostname: string;
  target_kind: "container" | "redirect";
  target: string;
  tls: RouteTLS;
  certificate?: { id: string; names: string[]; issuer: string; not_after: string };
  https_redirect: boolean;
  hsts: boolean;
  compress: boolean;
  dns_status: DnsStatus;
  dns_error: string;
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
  target_kind: "container" | "redirect";
  container: string;
  port: number;
  redirect_to: string;
  redirect_permanent: boolean;
  tls: "auto" | "off";
  https_redirect: boolean;
  hsts: boolean;
  compress: boolean;
  zone: string;
  dns_status: DnsStatus;
  dns_error: string;
};

export type Certificate = RecordModel & {
  names: string[];
  issuer: "manual" | "acme" | "internal";
  not_before: string;
  not_after: string;
  status: "active" | "expired";
  fingerprint: string;
  subject: string;
  issuer_name: string;
  installed: Record<string, string> | null;
  note: string;
  last_error: string;
};

export const edgeApi = {
  state: (host: string) => pb.send<EdgeState>(`/api/pc/hosts/${host}/edge`, {}),
  apply: (host: string) =>
    pb.send<{ command: string | null }>(`/api/pc/hosts/${host}/edge/apply`, { method: "POST" }),
  upload: (body: { chain_pem: string; key_pem: string; note: string }) =>
    pb.send<{ id: string }>("/api/pc/certificates", { method: "POST", body }),
  removeCertificate: (id: string) => pb.send(`/api/pc/certificates/${id}`, { method: "DELETE" }),
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
