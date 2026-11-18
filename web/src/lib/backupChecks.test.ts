import { afterEach, describe, expect, it, vi } from "vitest";

import i18n from "../i18n";
import {
  checkEnded,
  checkFields,
  checkRetryAt,
  czechAt,
  formatCheckMoment,
  integritySummary,
  nextGroup,
  parseGroups,
  parseSubset,
  restoreTestFields,
  restoreTestKeys,
  restoreTestStatuses,
  restoreTestTone,
  saveCheckSettings,
} from "./backupChecks";
import type { BackupTarget } from "./backups";
import { pb } from "./pb";

afterEach(async () => {
  vi.restoreAllMocks();
  await i18n.changeLanguage("en");
});

// Monday 9 November 2026, 13:00 in Prague
const now = Date.parse("2026-11-09T12:00:00Z");

function target(fields: Partial<BackupTarget>): BackupTarget {
  return {
    id: "t1",
    name: "b2",
    status: "ok",
    repo_id: "5f1c2a",
    integrity_status: "ok",
    integrity_at: "2026-11-08 05:20:00.000Z",
    integrity_error: "",
    last_check: "c1",
    last_check_status: "ok",
    last_check_error: "",
    next_check_at: "2026-11-15 05:00:00.000Z",
    integrity_group: 2,
    ...fields,
  } as BackupTarget;
}

const unverified = (id: string) => ({
  rule: "backup_check_unverified",
  target: id,
  status: "open" as const,
});

describe("the integrity tile's summary", () => {
  it("is fine when every target's newest check was, and says the next check", () => {
    const summary = integritySummary(
      [
        target({ id: "t1", next_check_at: "2026-11-15 05:00:00.000Z" }),
        target({ id: "t2", next_check_at: "2026-11-15 04:59:00.000Z" }),
      ],
      [],
      [],
    );
    expect(summary).toEqual({
      state: "ok",
      count: 0,
      next: "2026-11-15 04:59:00.000Z",
      running: null,
    });
  });

  it("counts the targets with errors first, then those not verified", () => {
    const targets = [
      target({ id: "t1", integrity_status: "failed" }),
      target({ id: "t2", integrity_status: "failed" }),
      target({ id: "t3" }),
    ];
    expect(integritySummary(targets, [unverified("t3")], [])).toMatchObject({
      state: "failed",
      count: 2,
    });
    const fine = [target({ id: "t1" }), target({ id: "t2" }), target({ id: "t3" })];
    // one alert per target, though it came twice; a removed target's is left out
    expect(
      integritySummary(
        fine,
        [unverified("t1"), unverified("t1"), unverified("t3"), unverified("gone")],
        [],
      ),
    ).toMatchObject({ state: "unverified", count: 2 });
    // a resolved one or another rule says nothing
    expect(
      integritySummary(
        fine,
        [
          { ...unverified("t1"), status: "resolved" },
          { rule: "backup_check_failed", target: "t2", status: "open" },
        ],
        [],
      ).state,
    ).toBe("ok");
  });

  it("says the checks are off when no target has a next check", () => {
    const off = [target({ id: "t1", next_check_at: "" }), target({ id: "t2", next_check_at: "" })];
    expect(integritySummary(off, [], [])).toEqual({
      state: "off",
      count: 0,
      next: "",
      running: null,
    });
    // a pending target is never checked, so it does not keep them on
    expect(
      integritySummary(
        [
          ...off,
          target({ id: "t3", status: "pending", next_check_at: "2026-11-15 05:00:00.000Z" }),
        ],
        [],
        [],
      ).state,
    ).toBe("off");
    // errors found before they were turned off still stand
    expect(
      integritySummary([target({ next_check_at: "", integrity_status: "failed" })], [], []).state,
    ).toBe("failed");
  });

  it("says no check has run before one completed", () => {
    const fresh = [
      target({ id: "t1", integrity_status: "", integrity_at: "", last_check_status: "" }),
      target({ id: "t2", integrity_status: "", integrity_at: "", last_check_status: "skipped" }),
    ];
    expect(integritySummary(fresh, [], []).state).toBe("never");
    // with no target to check, there is nothing to turn off either
    expect(integritySummary([], [], []).state).toBe("never");
    expect(integritySummary([target({ status: "pending" })], [], []).state).toBe("never");
  });

  it("names the target a check runs for, but not one only queued", () => {
    const targets = [target({ id: "t1", name: "b2" }), target({ id: "t2", name: "nas" })];
    expect(
      integritySummary(
        targets,
        [],
        [
          { target: "t1", status: "queued" },
          { target: "t2", status: "running" },
        ],
      ).running?.name,
    ).toBe("nas");
    expect(integritySummary(targets, [], [{ target: "t1", status: "queued" }]).running).toBeNull();
  });
});

describe("the moment of a check", () => {
  it("is the long weekday and the time within a week, in local time", async () => {
    // Sunday 05:00 UTC is 06:00 in Prague
    expect(formatCheckMoment("2026-11-15 05:00:00.000Z", now, "Europe/Prague")).toBe(
      "Sunday 06:00",
    );
    await i18n.changeLanguage("cs");
    expect(formatCheckMoment("2026-11-15 05:00:00.000Z", now, "Europe/Prague")).toBe(
      "neděle 06:00",
    );
    // a week back is still within it
    expect(formatCheckMoment("2026-11-02 12:30:00.000Z", now, "Europe/Prague")).toBe(
      "pondělí 13:30",
    );
  });

  it("has its date further off, and a dash when there is none", async () => {
    await i18n.changeLanguage("cs");
    expect(formatCheckMoment("2026-11-22 05:00:00.000Z", now, "Europe/Prague")).toBe(
      "22. 11. 06:00",
    );
    expect(formatCheckMoment("2026-10-25 05:00:00.000Z", now, "Europe/Prague")).toBe(
      "25. 10. 06:00",
    );
    expect(formatCheckMoment("", now)).toBe("–");
  });
});

describe("the Czech preposition of a time", () => {
  it("is ve before two consonants: 2, 3, 4, 12–14 and 20–23 o'clock", () => {
    const at = (hour: number) => new Date(Date.UTC(2026, 10, 9, hour, 30));
    const ve = [2, 3, 4, 12, 13, 14, 20, 21, 22, 23];
    for (let hour = 0; hour < 24; hour++) {
      expect(czechAt(at(hour), "UTC"), `${hour}:30`).toBe(ve.includes(hour) ? "titleVe" : "title");
    }
  });

  it("reads the hour in local time", () => {
    // 02:00 UTC is 03:00 in Prague: ve třech
    expect(czechAt(new Date("2026-11-09T02:00:00Z"), "Europe/Prague")).toBe("titleVe");
    // 04:00 UTC is 05:00 in Prague: v pět
    expect(czechAt(new Date("2026-11-09T04:00:00Z"), "Europe/Prague")).toBe("title");
  });
});

describe("the groups of a check", () => {
  it("reads a subset, and nothing for the structure only", () => {
    expect(parseSubset("3/20")).toEqual({ n: 3, t: 20 });
    expect(parseSubset("256/256")).toEqual({ n: 256, t: 256 });
    expect(parseSubset("")).toBeNull();
    expect(parseSubset("21/20")).toBeNull();
    expect(parseSubset("0/20")).toBeNull();
    expect(parseSubset("5%")).toBeNull();
  });

  it("takes the next group as the panel does: 0 or out of range is 1", () => {
    expect(nextGroup({ integrity_group: 3 }, 20)).toBe(3);
    expect(nextGroup({ integrity_group: 0 }, 20)).toBe(1);
    expect(nextGroup({ integrity_group: 21 }, 20)).toBe(1);
  });

  it("takes groups as a whole number from 0 to 256", () => {
    expect(parseGroups("20")).toBe(20);
    expect(parseGroups(" 0 ")).toBe(0);
    expect(parseGroups("256")).toBe(256);
    expect(parseGroups("257")).toBeNull();
    expect(parseGroups("2.5")).toBeNull();
    expect(parseGroups("-1")).toBeNull();
    expect(parseGroups("")).toBeNull();
  });
});

describe("the checks' settings", () => {
  it("are saved through the panel's route", async () => {
    const send = vi
      .spyOn(pb, "send")
      .mockResolvedValue({ schedule: "", groups: 0, next_check_at: "" });
    await saveCheckSettings({ schedule: "", groups: 0 });
    expect(send).toHaveBeenCalledWith("/api/pc/settings/backup-checks", {
      method: "PUT",
      body: { schedule: "", groups: 0 },
    });
  });
});

describe("a check in a list", () => {
  it("is read without its errors and its log, which can be long", () => {
    const fields = checkFields.split(",");
    expect(fields).not.toContain("errors");
    expect(fields).not.toContain("log");
    // what a line, the tile and the drawer's timeline show
    for (const field of [
      "target",
      "status",
      "subset",
      "finished_at",
      "command",
      "errors_more",
      "retry_at",
    ]) {
      expect(fields).toContain(field);
    }
  });
});

describe("a check's retry", () => {
  const at = "2026-11-09 13:00:00.000Z";

  it("is planned for an error, a suspect or a skipped check that has retry_at", () => {
    for (const status of ["error", "suspect", "skipped"] as const) {
      expect(checkRetryAt({ status, retry_at: at })).toBe(at);
    }
  });

  it("is none without retry_at, as for a check by hand, nor for any other end", () => {
    expect(checkRetryAt({ status: "suspect", retry_at: "" })).toBe("");
    for (const status of ["ok", "failed", "cancelled", "queued", "running"] as const) {
      expect(checkRetryAt({ status, retry_at: at })).toBe("");
    }
    expect(checkRetryAt(null)).toBe("");
  });
});

describe("a restore test (#53)", () => {
  it("has a key for every status, in both languages", () => {
    expect(restoreTestKeys).toEqual([
      "backups.restoreTests.status.queued",
      "backups.restoreTests.status.running",
      "backups.restoreTests.status.ok",
      "backups.restoreTests.status.failed",
      "backups.restoreTests.status.error",
      "backups.restoreTests.status.skipped",
      "backups.restoreTests.status.cancelled",
    ]);
    for (const language of ["en", "cs"]) {
      for (const key of restoreTestKeys) {
        expect(i18n.exists(key, { lng: language }), `${language} ${key}`).toBe(true);
      }
    }
  });

  it("is red only when the backup could not be restored", () => {
    expect(restoreTestStatuses.map((status) => restoreTestTone(status))).toEqual([
      "info",
      "info",
      "ok",
      "err",
      "warn",
      "warn",
      "off",
    ]);
  });

  it("is read in lists without its log, with what the cards show", () => {
    const fields = restoreTestFields.split(",");
    expect(fields).not.toContain("log");
    for (const field of [
      "job",
      "database_name",
      "snapshot_id",
      "temporary",
      "tables_expected",
      "tables_restored",
      "dump_bytes",
      "failure",
      "retry_at",
      "duration_ms",
    ]) {
      expect(fields).toContain(field);
    }
  });

  it("ends when it is neither queued nor running", () => {
    expect(checkEnded({ status: "queued" })).toBe(false);
    expect(checkEnded({ status: "running" })).toBe(false);
    expect(checkEnded({ status: "error" })).toBe(true);
  });
});
