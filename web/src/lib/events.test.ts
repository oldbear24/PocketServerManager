import { describe, expect, it } from "vitest";

import i18n from "../i18n";
import { alertLink, alertText, backupAlertRules } from "./events";
import type { Alert } from "./types";

const alert = {
  id: "a1",
  collectionId: "alerts",
  collectionName: "alerts",
  rule: "backup_failed",
  target: "j1:t2",
  host: "h1",
  severity: "warning",
  status: "open",
  first_seen: "",
  last_seen: "",
  message: "The backup of photos to b2 failed.",
} as Alert;

describe("the backup alerts", () => {
  it("link a job's alert to its runs to the target", () => {
    for (const rule of ["backup_failed", "backup_skipped", "backup_retention"]) {
      expect(alertLink({ ...alert, rule })).toBe("/backups?job=j1&target=t2");
    }
    expect(alertLink({ ...alert, target: "j1" })).toBe("/backups?job=j1");
    expect(alertLink({ ...alert, target: "" })).toBe("/backups");
  });

  it("link a target's integrity alert to its checks", () => {
    for (const rule of ["backup_check_failed", "backup_check_unverified"]) {
      expect(alertLink({ ...alert, rule, target: "t2" })).toBe("/backups?check=t2");
    }
  });

  it("are every rule the navigation marks Backups for", () => {
    expect(backupAlertRules).toEqual([
      "backup_failed",
      "backup_skipped",
      "backup_retention",
      "backup_check_failed",
      "backup_check_unverified",
    ]);
  });

  it("are said in the UI's language, by the server's name", () => {
    const en = i18n.getFixedT("en");
    const cs = i18n.getFixedT("cs");
    const failed = { ...alert, rule: "backup_check_failed", target: "t2" };
    expect(alertText(en, failed, "nas")).toBe("nas: a backup repository has errors");
    expect(alertText(cs, failed, "nas")).toBe("nas: repozitář záloh obsahuje chyby");
    expect(alertText(en, alert, "nas")).toBe("nas: a backup failed");
    expect(alertText(cs, { ...alert, rule: "backup_check_unverified" }, "nas")).toBe(
      "nas: repozitář záloh není ověřený",
    );
  });
});
