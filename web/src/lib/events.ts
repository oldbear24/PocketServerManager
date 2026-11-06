import type { TFunction } from "i18next";

import type { Alert, HostEvent } from "./types";

/**
 * Events are stored with an English message. The UI shows them in its own language
 * when it knows the kind, and falls back to the stored message when it does not.
 */
export function eventText(t: TFunction, event: HostEvent): string {
  const data = event.data ?? {};

  const reason = typeof data.reason === "string" && data.reason ? data.reason : "";
  const remote = typeof data.remote === "string" && data.remote ? data.remote : "";

  if (event.kind === "connection.connected" && remote) {
    return t("event.connection.connectedFrom", { remote });
  }
  if (event.kind === "connection.disconnected" && reason) {
    return t("event.connection.disconnectedBecause", {
      reason: t(`diag.reason.${reason}`, { defaultValue: reason }),
    });
  }

  const check = typeof data.check === "string" ? data.check : "";

  return t(`event.${event.kind}`, {
    ...data,
    check: check && t(`server.check.${check}`, { defaultValue: check }),
    defaultValue: event.message,
  });
}

/** Alerts work the same way, by their rule. */
export function alertText(t: TFunction, alert: Alert, hostName?: string): string {
  return t(`alert.${alert.rule}`, {
    name: hostName ?? alert.target,
    defaultValue: alert.message,
  });
}

/** The rules of the alerts about Domains & TLS, which link there (#27). */
export const domainAlertRules = [
  "certificate_failed",
  "certificate_expiring",
  "dns_provider_failed",
];

/** Where an alert is fixed: the certificate or the DNS provider it is about
 * on Domains & TLS, or its server. */
export function alertLink(alert: Alert): string | null {
  switch (alert.rule) {
    case "certificate_failed":
    case "certificate_expiring":
      return `/domains?certificate=${encodeURIComponent(alert.target)}`;
    case "dns_provider_failed":
      return `/domains?provider=${encodeURIComponent(alert.target)}`;
  }
  return alert.host ? `/servers/${alert.host}` : null;
}
