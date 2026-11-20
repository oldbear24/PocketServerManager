import { describe, expect, it } from "vitest";

import { listPage, matchesFilter } from "./test-filter";

describe("a filter of a test's list", () => {
  it("compares fields with strings and other fields", () => {
    const record = { status: "open", rule: "agent_offline", created: "2026-10-16 10:00:00.000Z" };
    expect(matchesFilter(record, "status != 'resolved' && rule = 'agent_offline'")).toBe(true);
    expect(matchesFilter(record, "created >= '2026-10-17' || (status = 'open')")).toBe(true);
    expect(matchesFilter(record, "status = rule")).toBe(false);
    expect(matchesFilter({ parent: null }, "parent = ''")).toBe(true);
  });

  it("takes the escapes of quotes and backslashes in a string", () => {
    expect(matchesFilter({ name: "it's" }, "name = 'it\\'s'")).toBe(true);
    expect(matchesFilter({ name: 'say "hi"' }, 'name = "say \\"hi\\""')).toBe(true);
    expect(matchesFilter({ path: "a\\b" }, "path = 'a\\\\b'")).toBe(true);
  });

  describe("with ~, as PocketBase's LIKE", () => {
    it("finds a pattern without % anywhere in the value", () => {
      expect(matchesFilter({ type: "logs.follow" }, "type ~ 'follow'")).toBe(true);
      expect(matchesFilter({ type: "logs.follow" }, "type ~ 'logs.'")).toBe(true);
      expect(matchesFilter({ type: "logs.follow" }, "type ~ 'tail'")).toBe(false);
    });

    it("takes _ and \\ of a pattern without % as themselves", () => {
      expect(matchesFilter({ type: "db_stats" }, "type ~ 'db_stats'")).toBe(true);
      // _ is no wildcard here: it is escaped
      expect(matchesFilter({ type: "dbxstats" }, "type ~ 'b_s'")).toBe(false);
    });

    it("anchors a pattern with %, where % is any text and _ one character", () => {
      expect(matchesFilter({ type: "logs.follow" }, "type ~ 'logs.%'")).toBe(true);
      // anchored at the start: logs. must begin the value
      expect(matchesFilter({ type: "site.logs.follow" }, "type ~ 'logs.%'")).toBe(false);
      expect(matchesFilter({ type: "logs." }, "type ~ 'logs.%'")).toBe(true);
      expect(matchesFilter({ resource: "backup-usage:t1" }, "resource ~ 'backup-usage:%'")).toBe(
        true,
      );
      expect(matchesFilter({ resource: "backup:t1" }, "resource ~ 'backup-usage:%'")).toBe(false);
      expect(matchesFilter({ type: "exec.run" }, "type ~ 'exec._un%'")).toBe(true);
      expect(matchesFilter({ type: "exec.rerun" }, "type ~ 'exec._un%'")).toBe(false);
      expect(matchesFilter({ type: "acme.challenge.set" }, "type ~ '%.challenge.%'")).toBe(true);
    });

    it("takes an escaped % or _ in a pattern as itself", () => {
      expect(matchesFilter({ name: "50%" }, "name ~ '50\\%'")).toBe(true);
      expect(matchesFilter({ name: "50x" }, "name ~ '%0\\_'")).toBe(false);
      expect(matchesFilter({ name: "50_" }, "name ~ '%0\\_'")).toBe(true);
    });

    it("does not count the case of A to Z, as SQLite", () => {
      expect(matchesFilter({ name: "Logs.Follow" }, "name ~ 'logs.%'")).toBe(true);
      expect(matchesFilter({ name: "NAS" }, "name ~ 'a'")).toBe(true);
      expect(matchesFilter({ name: "Č" }, "name ~ 'č'")).toBe(false);
    });

    it("reads an empty field as empty text", () => {
      expect(matchesFilter({}, "resource ~ 'backup-usage:%'")).toBe(false);
      expect(matchesFilter({ resource: null }, "resource !~ 'backup-usage:%'")).toBe(true);
    });
  });

  describe("with !~", () => {
    it("is true where ~ is false", () => {
      expect(matchesFilter({ type: "logs.follow" }, "type !~ 'logs.%'")).toBe(false);
      expect(matchesFilter({ type: "stack.restart" }, "type !~ 'logs.%'")).toBe(true);
      expect(matchesFilter({ type: "logs.follow" }, "type !~ 'follow'")).toBe(false);
      expect(matchesFilter({ type: "stack.restart" }, "type !~ 'follow'")).toBe(true);
    });

    it("hides commands in a queue's filter", () => {
      const queue =
        "parent = '' && resource !~ 'backup-usage:%' && type !~ 'logs.%' && type != 'db.stats'";
      const command = (fields: Record<string, string>) => ({
        parent: "",
        resource: "",
        type: "stack.restart",
        ...fields,
      });
      expect(matchesFilter(command({}), queue)).toBe(true);
      expect(matchesFilter(command({ type: "logs.follow" }), queue)).toBe(false);
      expect(matchesFilter(command({ type: "db.stats" }), queue)).toBe(false);
      expect(matchesFilter(command({ resource: "backup-usage:t1" }), queue)).toBe(false);
      expect(matchesFilter(command({ resource: "backup:t1", type: "backup.init" }), queue)).toBe(
        true,
      );
      expect(matchesFilter(command({ parent: "c1" }), queue)).toBe(false);
    });
  });

  it("refuses what it cannot read", () => {
    expect(() => matchesFilter({}, "type ~")).toThrow(/unexpected end/);
    expect(() => matchesFilter({}, "type ? 'x'")).toThrow();
  });
});

describe("a page of a test's list", () => {
  it("filters, sorts by one field and cuts at perPage", () => {
    const records = [
      { id: "a", type: "logs.follow", created: "1" },
      { id: "b", type: "stack.restart", created: "3" },
      { id: "c", type: "image.pull", created: "2" },
    ];
    expect(
      listPage(records, { filter: "type !~ 'logs.%'", sort: "-created", perPage: 1 }).map(
        (r) => r.id,
      ),
    ).toEqual(["b"]);
  });
});
