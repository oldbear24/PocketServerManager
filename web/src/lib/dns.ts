import type { RecordModel } from "pocketbase";

import { pb } from "./pb";

/** The state of a provider, a zone or a domain's records (internal/schema). */
export type DnsStatus = "unknown" | "ok" | "error" | "pending" | "drift" | "manual" | "none" | "";

/** A kind of DNS provider and the credentials it asks for. */
export type DnsKind = {
  kind: string;
  label: string;
  /** null for a kind without credentials, as the manual one */
  fields: { name: string; secret: boolean; optional?: boolean }[] | null;
  lists_zones: boolean;
  manual: boolean;
  note?: string;
};

export type DnsProvider = RecordModel & {
  name: string;
  kind: string;
  status: DnsStatus;
  last_check: string;
  last_error: string;
};

export type DnsZone = RecordModel & {
  provider: string;
  name: string;
  managed: boolean;
  status: DnsStatus;
  last_check: string;
  last_error: string;
};

export type DnsRecord = {
  name: string;
  hostname: string;
  type: string;
  value: string;
  ttl: number;
};

export type DnsAnswer = { server: string; values?: string[]; error?: string; ok: boolean };

/** What a domain needs in DNS, and what the servers answered. */
export type DomainDns = {
  status: DnsStatus;
  error: string;
  zone: string;
  manual: boolean;
  problem: string;
  records: DnsRecord[];
  provider?: { id: string; name: string; kind: string };
  propagation?: {
    record: { name: string; type: string; value: string };
    answers: DnsAnswer[];
    resolved: boolean;
  }[];
};

export type ProviderWithZones = { provider: DnsProvider; zones: DnsZone[] };

export const dnsApi = {
  kinds: () => pb.send<DnsKind[]>("/api/pc/dns/kinds", {}),

  connect: (body: {
    name: string;
    kind: string;
    credentials: Record<string, string>;
    zones: string[];
  }) => pb.send<ProviderWithZones>("/api/pc/dns-providers", { method: "POST", body }),

  test: (id: string) =>
    pb.send<ProviderWithZones>(`/api/pc/dns-providers/${id}/test`, { method: "POST" }),

  remove: (id: string) => pb.send(`/api/pc/dns-providers/${id}`, { method: "DELETE" }),

  addZone: (provider: string, name: string) =>
    pb.send<ProviderWithZones>("/api/pc/dns-zones", { method: "POST", body: { provider, name } }),

  setManaged: (id: string, managed: boolean) =>
    pb.send<DnsZone>(`/api/pc/dns-zones/${id}`, { method: "PATCH", body: { managed } }),

  domain: (id: string, check = false) =>
    pb.send<DomainDns>(`/api/pc/domains/${id}/dns${check ? "?check=1" : ""}`, {}),

  sync: (id: string) => pb.send<DomainDns>(`/api/pc/domains/${id}/dns/sync`, { method: "POST" }),
};

/** The tone of a DNS state's pill. */
export function dnsTone(status: DnsStatus): "ok" | "warn" | "err" | "off" | "info" {
  switch (status) {
    case "ok":
      return "ok";
    case "error":
      return "err";
    case "drift":
      return "warn";
    case "pending":
    case "manual":
      return "info";
    default:
      return "off";
  }
}
