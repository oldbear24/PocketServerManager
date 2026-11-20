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

/** What an alert is about, as its title names it (#33): a server, a job as
 * server/job, a repository, a certificate or a DNS provider by its name; how
 * many days a certificate has left; and the rule whose title fits it better
 * now, as an expiring certificate's once it expired. */
export type AlertSubject = { name?: string; count?: number; rule?: string };

/** Alerts work the same way, by their rule. Without a subject the title
 * names the alert's target as it is stored. */
export function alertText(t: TFunction, alert: Alert, subject?: AlertSubject): string {
  return t(`alert.${subject?.rule ?? alert.rule}`, {
    name: subject?.name ?? alert.target,
    // without a count the title is the one without a number
    count: subject?.count,
    defaultValue: alert.message,
  });
}

/** The rules of the alerts about Domains & TLS, which link there (#27). */
export const domainAlertRules = [
  "certificate_failed",
  "certificate_expiring",
  "dns_provider_failed",
];

/** The rules of the alerts about backups, which link there and mark it in
 * the navigation (#29, #31, #32): a job's to a target, keyed "<job>:<target>",
 * a target's repository, keyed by the target, and the panel's own backup;
 * a database job's restore tests, keyed by the job, and a temporary database
 * that was not dropped, keyed by its record (#53). */
export const backupAlertRules = [
  "backup_failed",
  "backup_skipped",
  "backup_retention",
  "backup_check_failed",
  "backup_check_unverified",
  "panel_backup",
  "backup_restore_test_failed",
  "backup_restore_test_unverified",
  "db_temporary_left",
];

/** Where an alert is fixed: the certificate or the DNS provider it is about
 * on Domains & TLS, a job's runs to a target or a target's integrity checks
 * on Backups, the panel's own backup there and its recovery kit in Settings
 * (#32), a database job's restore tests on Backups and a temporary database
 * on Databáze (#53), or its server. */
export function alertLink(alert: Alert): string | null {
  switch (alert.rule) {
    case "certificate_failed":
    case "certificate_expiring":
      return `/domains?certificate=${encodeURIComponent(alert.target)}`;
    case "dns_provider_failed":
      return `/domains?provider=${encodeURIComponent(alert.target)}`;
    case "backup_failed":
    case "backup_skipped":
    case "backup_retention": {
      const [job, target = ""] = alert.target.split(":");
      if (job) {
        const query = new URLSearchParams({ job });
        if (target) {
          query.set("target", target);
        }
        return `/backups?${query.toString()}`;
      }
      return "/backups";
    }
    case "backup_check_failed":
    case "backup_check_unverified":
      return alert.target ? `/backups?check=${encodeURIComponent(alert.target)}` : "/backups";
    case "backup_restore_test_failed":
    case "backup_restore_test_unverified":
      return alert.target ? `/backups?job=${encodeURIComponent(alert.target)}` : "/backups";
    case "db_temporary_left":
      return "/databases";
    case "recovery_kit":
    case "panel_restored":
      return "/settings#recovery-kit";
    case "panel_backup":
      return "/backups?setup=panel";
  }
  return alert.host ? `/servers/${alert.host}` : null;
}
