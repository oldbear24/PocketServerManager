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
      "panel_backup",
      "backup_restore_test_failed",
      "backup_restore_test_unverified",
      "db_temporary_left",
    ]);
  });

  it("link a job's restore test alerts to the job, and a temporary database to Databáze (#53)", () => {
    for (const rule of ["backup_restore_test_failed", "backup_restore_test_unverified"]) {
      expect(alertLink({ ...alert, rule, target: "j7" })).toBe("/backups?job=j7");
      expect(alertLink({ ...alert, rule, target: "" })).toBe("/backups");
    }
    expect(alertLink({ ...alert, rule: "db_temporary_left", target: "d9" })).toBe("/databases");
  });

  it("say the restore tests' alerts in the UI's language (#53)", () => {
    const en = i18n.getFixedT("en");
    const cs = i18n.getFixedT("cs");
    const failed = { ...alert, rule: "backup_restore_test_failed", target: "j7" };
    expect(alertText(en, failed, { name: "db-main" })).toBe("db-main: a restore test failed");
    expect(alertText(cs, failed, { name: "db-main" })).toBe("db-main: test obnovy selhal");
    expect(alertText(cs, { ...failed, rule: "db_temporary_left" }, { name: "nas" })).toBe(
      "nas: dočasná databáze se nesmazala",
    );
  });

  it("link the panel's own backup to its setup, and its kit and restore to Settings (#32)", () => {
    const panel = { ...alert, target: "panel", host: "" };
    expect(alertLink({ ...panel, rule: "panel_backup" })).toBe("/backups?setup=panel");
    expect(alertLink({ ...panel, rule: "recovery_kit" })).toBe("/settings#recovery-kit");
    expect(alertLink({ ...panel, rule: "panel_restored" })).toBe("/settings#recovery-kit");
  });

  it("say the panel's own alerts in the UI's language", () => {
    const en = i18n.getFixedT("en");
    const cs = i18n.getFixedT("cs");
    const panel = { ...alert, target: "panel", host: "", message: "stored" };
    expect(alertText(en, { ...panel, rule: "recovery_kit" })).toBe(
      "The panel's recovery kit is not saved",
    );
    expect(alertText(cs, { ...panel, rule: "panel_backup" })).toBe("Samotný panel se nezálohuje");
    expect(alertText(cs, { ...panel, rule: "panel_restored" })).toBe(
      "Panel byl obnoven ze zálohy: servery se znovu připojují",
    );
  });

  it("are said in the UI's language, by their subject's name (#33)", () => {
    const en = i18n.getFixedT("en");
    const cs = i18n.getFixedT("cs");
    const failed = { ...alert, rule: "backup_check_failed", target: "t2" };
    expect(alertText(en, failed, { name: "Backblaze B2" })).toBe(
      "The repository Backblaze B2 has errors",
    );
    expect(alertText(cs, failed, { name: "Backblaze B2" })).toBe(
      "Repozitář Backblaze B2 obsahuje chyby",
    );
    expect(alertText(en, alert, { name: "nas/photos" })).toBe("Backup nas/photos failed");
    expect(alertText(cs, alert, { name: "nas/photos" })).toBe("Záloha nas/photos selhala");
    expect(alertText(cs, { ...alert, rule: "backup_check_unverified" }, { name: "B2" })).toBe(
      "Repozitář B2 není ověřený",
    );
    expect(alertText(cs, { ...alert, rule: "agent_offline" }, { name: "rpi-garaz" })).toBe(
      "rpi-garaz: agent bez spojení",
    );
  });

  it("name their stored target without a subject, and say the stored message for a rule the UI does not know", () => {
    const cs = i18n.getFixedT("cs");
    expect(alertText(cs, alert)).toBe("Záloha j1:t2 selhala");
    expect(alertText(cs, alert, {})).toBe("Záloha j1:t2 selhala");
    expect(alertText(cs, { ...alert, rule: "something_new" })).toBe(
      "The backup of photos to b2 failed.",
    );
  });
});

describe("the certificates' alerts (#33)", () => {
  const expiring = {
    ...alert,
    rule: "certificate_expiring",
    target: "c1",
    host: "",
    message: "The certificate for shop.example.org expires in 12 days.",
  };

  it("say how many days a certificate has left, in each plural form", () => {
    const en = i18n.getFixedT("en");
    const cs = i18n.getFixedT("cs");
    const name = "shop.example.org";
    expect(alertText(en, expiring, { name, count: 1 })).toBe(
      "The certificate shop.example.org expires in 1 day",
    );
    expect(alertText(en, expiring, { name, count: 12 })).toBe(
      "The certificate shop.example.org expires in 12 days",
    );
    expect(alertText(cs, expiring, { name, count: 1 })).toBe(
      "Certifikát shop.example.org vyprší za 1 den",
    );
    expect(alertText(cs, expiring, { name, count: 3 })).toBe(
      "Certifikát shop.example.org vyprší za 3 dny",
    );
    expect(alertText(cs, expiring, { name, count: 12 })).toBe(
      "Certifikát shop.example.org vyprší za 12 dní",
    );
    expect(alertText(cs, expiring, { name, count: 0 })).toBe(
      "Certifikát shop.example.org vyprší za 0 dní",
    );
  });

  it("say it without a number when the count is not known", () => {
    const en = i18n.getFixedT("en");
    const cs = i18n.getFixedT("cs");
    expect(alertText(en, expiring, { name: "shop.example.org" })).toBe(
      "The certificate shop.example.org expires soon",
    );
    expect(alertText(cs, expiring, { name: "shop.example.org" })).toBe(
      "Certifikát shop.example.org brzy vyprší",
    );
  });

  it("take the subject's rule, as an expiring certificate's once it expired", () => {
    const cs = i18n.getFixedT("cs");
    expect(alertText(cs, expiring, { name: "shop.example.org", rule: "certificate_expired" })).toBe(
      "Certifikát shop.example.org vypršel",
    );
    expect(alertText(cs, { ...expiring, rule: "certificate_failed" }, { name: "a.lan" })).toBe(
      "Certifikát a.lan se nepodařilo získat",
    );
    expect(alertText(cs, { ...expiring, rule: "dns_provider_failed" }, { name: "WEDOS" })).toBe(
      "DNS poskytovatel WEDOS hlásí chybu",
    );
    expect(alertText(cs, { ...expiring, rule: "backup_restore" }, { name: "nas" })).toBe(
      "nas: obnova ze zálohy potřebuje pozornost",
    );
  });
});
