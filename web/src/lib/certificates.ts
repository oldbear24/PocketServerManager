import type { RecordModel } from "pocketbase";

import type { Tone } from "../components/ui";
import type { DnsProvider, DnsZone } from "./dns";
import type { Certificate } from "./edge";
import { parseDate } from "./format";
import { pb } from "./pb";

/** An account at an ACME certificate authority (#23). */
export type AcmeAccount = RecordModel & {
  role: "primary" | "fallback";
  directory_url: string;
  email: string;
  eab_kid: string;
  uri: string;
  status: "ok" | "error" | "";
  last_error: string;
  registered_at: string;
};

export type AcmeAccountBody = {
  directory_url: string;
  email: string;
  eab_kid: string;
  eab_hmac: string;
};

/** The CAs PocketCloud knows by name; any other directory is "custom". */
export const directories = [
  { id: "letsencrypt", url: "https://acme-v02.api.letsencrypt.org/directory", eab: false },
  {
    id: "letsencrypt-staging",
    url: "https://acme-staging-v02.api.letsencrypt.org/directory",
    eab: false,
  },
  { id: "zerossl", url: "https://acme.zerossl.com/v2/DV90", eab: true },
] as const;

export type DirectoryId = (typeof directories)[number]["id"] | "custom";

export function directoryId(url: string): DirectoryId {
  return directories.find((directory) => directory.url === url)?.id ?? "custom";
}

export type Challenge = "auto" | "http-01" | "dns-01";

export const certificatesApi = {
  /** issuer internal asks the internal CA, for LAN names (#26) */
  request: (names: string[], challenge: Challenge, issuer: "acme" | "internal" = "acme") =>
    pb.send<{ id: string }>("/api/pc/certificates/acme", {
      method: "POST",
      body: { names, challenge, issuer },
    }),
  retry: (id: string) =>
    pb.send<{ id: string }>(`/api/pc/certificates/${id}/retry`, { method: "POST" }),
  setAccount: (role: AcmeAccount["role"], body: AcmeAccountBody) =>
    pb.send<AcmeAccount>(`/api/pc/acme-accounts/${role}`, { method: "PUT", body }),
  removeAccount: (role: AcmeAccount["role"]) =>
    pb.send(`/api/pc/acme-accounts/${role}`, { method: "DELETE" }),
};

/** How a certificate stands, as the table shows it. */
export type CertState =
  | "valid"
  | "expiring"
  | "expired"
  | "issuing"
  | "pending"
  | "failed"
  | "renewal_failed"
  | "renewal_waiting";

const day = 24 * 60 * 60 * 1000;
/** As the panel's alert: a certificate that expires within 14 days needs a look. */
export const expiringWithin = 14 * day;
/** An attempt that started longer ago than this did not finish. */
const staleAttempt = 30 * 60 * 1000;

export function certificateState(cert: Certificate, now = Date.now()): CertState {
  const started = parseDate(cert.attempt_started)?.getTime();
  const issuing = started !== undefined && now - started < staleAttempt;
  if (!cert.fingerprint) {
    if (issuing) {
      return "issuing";
    }
    return cert.status === "failed" ? "failed" : "pending";
  }
  if (issuing) {
    return "issuing";
  }
  const end = parseDate(cert.not_after)?.getTime() ?? 0;
  if (end <= now) {
    return "expired";
  }
  // the internal CA renews its own as ACME does
  if (cert.issuer !== "manual" && cert.attempts > 0) {
    return "renewal_failed";
  }
  // due, but it waits: for an account, or for the edge
  const renew = parseDate(cert.renew_after)?.getTime();
  if (cert.issuer !== "manual" && cert.last_error && renew !== undefined && renew <= now) {
    return "renewal_waiting";
  }
  return end - now < expiringWithin ? "expiring" : "valid";
}

/** Whether someone should look at a certificate. */
export function needsAction(cert: Certificate, now = Date.now()): boolean {
  const state = certificateState(cert, now);
  return (
    state === "expired" ||
    state === "expiring" ||
    state === "failed" ||
    state === "renewal_failed" ||
    state === "renewal_waiting" ||
    // an order that waits, or a certificate that failed to install
    (state !== "issuing" && cert.last_error !== "")
  );
}

export function stateTone(state: CertState): Tone {
  switch (state) {
    case "valid":
      return "ok";
    case "expiring":
    case "renewal_failed":
    case "renewal_waiting":
      return "warn";
    case "expired":
    case "failed":
      return "err";
    case "issuing":
      return "info";
    default:
      return "off";
  }
}

/** Whole days left, and the share of its lifetime a certificate has left. */
export function validity(cert: Certificate, now = Date.now()): { days: number; left: number } {
  const start = parseDate(cert.not_before)?.getTime();
  const end = parseDate(cert.not_after)?.getTime();
  if (start === undefined || end === undefined || end <= start) {
    return { days: 0, left: 0 };
  }
  const left = Math.min(1, Math.max(0, (end - now) / (end - start)));
  return { days: Math.max(0, Math.floor((end - now) / day)), left };
}

/** The name of the CA in a certificate's issuer, such as "Let's Encrypt". */
export function caName(issuerName: string): string {
  const organization = /(?:^|,)O=([^,]+)/.exec(issuerName)?.[1];
  const common = /(?:^|,)CN=([^,]+)/.exec(issuerName)?.[1];
  return (organization ?? common ?? "").trim();
}

/** The challenge a certificate was last issued with, or will be. */
export function lastChallenge(cert: Certificate): string {
  const history = cert.history ?? [];
  for (let i = history.length - 1; i >= 0; i--) {
    if (history[i].challenge) {
      return history[i].challenge ?? "";
    }
  }
  return cert.challenge && cert.challenge !== "auto" ? cert.challenge : "";
}

/** The DNS provider of the zone that holds a name, for DNS-01. */
export function providerFor(
  name: string,
  zones: DnsZone[],
  providers: DnsProvider[],
): DnsProvider | undefined {
  const base = name.replace(/^\*\./, "").toLowerCase();
  let best: DnsZone | undefined;
  for (const zone of zones) {
    const zoneName = zone.name.toLowerCase();
    if (
      zone.managed &&
      (base === zoneName || base.endsWith(`.${zoneName}`)) &&
      zoneName.length > (best?.name.length ?? 0)
    ) {
      best = zone;
    }
  }
  return best && providers.find((provider) => provider.id === best.provider);
}

/** Names typed one per line or separated by commas or spaces. */
export function parseNames(text: string): string[] {
  const names = text
    .split(/[\s,]+/)
    .map((name) => name.trim().toLowerCase().replace(/\.$/, ""))
    .filter(Boolean);
  return [...new Set(names)];
}

/** The internal CA as its card shows it (#26). */
export type InternalCA = {
  root: { subject: string; not_after: string; sha256: string };
  agents: number;
  lan_certificates: number;
};

export const caApi = {
  info: () => pb.send<InternalCA>("/api/pc/ca", {}),
  /** where the root certificate is downloaded, without an account */
  rootURL: (format: "crt" | "pem") => pb.buildURL(`/api/pc/ca.${format}`),
};
