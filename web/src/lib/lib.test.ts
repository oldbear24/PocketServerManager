import { describe, expect, it } from "vitest";

import { auditFilter, emptyFilters, initials, lineDiff, windowStart, withContext } from "./audit";
import {
  formatBitrate,
  formatBytes,
  formatChartTime,
  formatDuration,
  formatRelative,
  formatSkew,
  formatVersion,
  parseDate,
  percentOf,
} from "./format";
import { connectionShare, envSnippet, serverLabel, type DbServer } from "./databases";
import { composeSnippet, dockerRunCommand, scriptCommand, type InstallParams } from "./install";
import {
  compileFilter,
  insertLines,
  levelTone,
  logFileText,
  splitLevel,
  visibleLines,
  type LogEntry,
} from "./logs";
import { addedLines, diffStats, envEntries, rejectionOf } from "./stacks";
import { connectionStrip } from "./strip";

describe("install commands", () => {
  const params: InstallParams = {
    panel: "panel.example.cz:9443",
    caPin: "sha256:abc",
    token: "ENR-7Q2K",
    name: "nas-02",
    roles: ["docker", "edge"],
    image: "ghcr.io/oldbear24/pocketcloud-agent:0.4.2",
    origin: "https://panel.example.cz",
  };

  it("docker run has every mount and value", () => {
    const command = dockerRunCommand(params);

    for (const part of [
      "--name pocketcloud-agent",
      "--restart unless-stopped",
      "-v /var/run/docker.sock:/var/run/docker.sock",
      "-v pocketcloud-agent:/var/lib/pocketcloud",
      "-v /:/host:ro,rslave",
      "-e PANEL=panel.example.cz:9443",
      "-e CA_PIN=sha256:abc",
      "-e TOKEN=ENR-7Q2K",
      "-e NAME=nas-02",
      "-e ROLES=docker,edge",
      "ghcr.io/oldbear24/pocketcloud-agent:0.4.2",
    ]) {
      expect(command).toContain(part);
    }
  });

  it("compose snippet says the same", () => {
    const snippet = composeSnippet(params);

    for (const part of [
      "image: ghcr.io/oldbear24/pocketcloud-agent:0.4.2",
      "- /:/host:ro,rslave",
      "CA_PIN: sha256:abc",
      "TOKEN: ENR-7Q2K",
      "ROLES: docker,edge",
    ]) {
      expect(snippet).toContain(part);
    }
  });

  it("the script needs only what the panel cannot know", () => {
    const script = scriptCommand(params);

    expect(script).toContain("https://panel.example.cz/api/pc/install.sh");
    expect(script).toContain("TOKEN=ENR-7Q2K");
    expect(script).toContain("NAME=nas-02");
    expect(script).toContain("ROLES=docker,edge");
  });
});

describe("audit filter", () => {
  const now = Date.UTC(2026, 9, 16, 12, 0, 0);

  it("selects the category and the time window", () => {
    const filter = auditFilter("change", emptyFilters, now);

    expect(filter).toContain('category = "change"');
    expect(filter).toContain('at >= "2026-10-15 12:00:00.000Z"');
  });

  it("has no time limit for all", () => {
    expect(windowStart("all", now)).toBeNull();
    expect(auditFilter("security", { ...emptyFilters, window: "all" }, now)).toBe(
      'category = "security"',
    );
  });

  it("escapes what the user typed", () => {
    const filter = auditFilter("change", { ...emptyFilters, q: 'a" || 1=1 || "' }, now);

    expect(filter).toContain('record_label ~ "a\\" || 1=1 || \\""');
  });

  it("adds every set filter", () => {
    const filter = auditFilter(
      "change",
      {
        ...emptyFilters,
        host: "h1",
        action: "update",
        collection: "stacks",
        actor: "jana",
        record: "r1",
      },
      now,
    );

    for (const part of [
      'host = "h1"',
      'action = "update"',
      'collection = "stacks"',
      'actor_label = "jana"',
      'record_id = "r1"',
    ]) {
      expect(filter).toContain(part);
    }
  });
});

describe("line diff", () => {
  it("finds added and removed lines", () => {
    const lines = lineDiff("a\nb\nc", "a\nx\nc\nd");

    expect(lines).toEqual([
      { kind: "same", text: "a" },
      { kind: "del", text: "b" },
      { kind: "add", text: "x" },
      { kind: "same", text: "c" },
      { kind: "add", text: "d" },
    ]);
  });

  it("is empty of changes for equal text", () => {
    expect(lineDiff("a\nb", "a\nb").every((line) => line.kind === "same")).toBe(true);
  });

  it("keeps only the context around changes", () => {
    const before = Array.from({ length: 20 }, (_, i) => `line ${i}`).join("\n");
    const after = before.replace("line 10", "changed");

    const shown = withContext(lineDiff(before, after), 1);

    expect(shown.map((line) => line.text)).toEqual(["line 9", "line 10", "changed", "line 11"]);
  });

  it("stays usable for huge files", () => {
    const huge = Array.from({ length: 3000 }, (_, i) => `line ${i}`).join("\n");
    expect(lineDiff(huge, "other").length).toBe(3001);
  });
});

describe("initials", () => {
  it.each([
    ["jana", "JA"],
    ["admin@example.com", "AE"],
    ["ci-deploy", "CD"],
    ["PocketCloud system", "PS"],
    ["", "?"],
  ])("of %s", (label, expected) => {
    expect(initials(label)).toBe(expected);
  });
});

describe("formatting", () => {
  it("parses the dates PocketBase stores", () => {
    expect(parseDate("2026-10-16 13:42:10.000Z")?.toISOString()).toBe("2026-10-16T13:42:10.000Z");
    expect(parseDate("")).toBeNull();
    expect(parseDate("nonsense")).toBeNull();
  });

  it("formats sizes", () => {
    expect(formatBytes(0)).toBe("–");
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(1500)).toBe("1.5 kB");
    expect(formatBytes(8_000_000_000)).toBe("8 GB");
  });

  it("formats durations", () => {
    expect(formatDuration(0)).toBe("–");
    expect(formatDuration(30)).toBe("30 s");
    expect(formatDuration(600)).toBe("10 min");
    expect(formatDuration(7200)).toBe("2 h");
    expect(formatDuration(86400 * 41)).toBe("41 d");
  });

  it("formats relative times with the largest unit that fits", () => {
    const now = Date.UTC(2026, 0, 1);
    const ago = (seconds: number) => new Date(now - seconds * 1000);

    expect(formatRelative(ago(4), now)).toContain("4 sec");
    expect(formatRelative(ago(120), now)).toContain("2 min");
    expect(formatRelative(ago(7200), now)).toContain("2 hr");
    expect(formatRelative(new Date(now + 86400000 * 3), now)).toContain("3 day");
    expect(formatRelative(null, now)).toBe("–");
    // a heartbeat a moment ahead of the browser's clock has just happened
    expect(formatRelative(new Date(now + 2000), now)).toContain("in 2 sec");
    expect(formatRelative(new Date(now + 2000), now, { past: true })).toBe("now");
  });

  it("formats versions", () => {
    expect(formatVersion("0.4.2")).toBe("v0.4.2");
    expect(formatVersion("dev")).toBe("dev");
    expect(formatVersion("5cadcc0-dirty")).toBe("5cadcc0-dirty");
    expect(formatVersion("")).toBe("–");
  });

  it("formats clock skew without a negative zero", () => {
    expect(formatSkew(200)).toBe("+0.2 s");
    expect(formatSkew(-1800)).toBe("-1.8 s");
    expect(formatSkew(-4)).toBe("0.0 s");
    expect(formatSkew(0)).toBe("0.0 s");
  });

  it("keeps percentages between 0 and 100", () => {
    expect(percentOf(1, 4)).toBe(25);
    expect(percentOf(5, 0)).toBe(0);
    expect(percentOf(9, 3)).toBe(100);
  });
});

describe("connection strip", () => {
  const now = Date.UTC(2026, 0, 2);
  const hour = 3600 * 1000;
  const at = (hoursAgo: number) => new Date(now - hoursAgo * hour).toISOString();

  it("is unknown before the first event", () => {
    expect(connectionStrip([], now, 4)).toEqual(["off", "off", "off", "off"]);
  });

  it("shows a stable connection", () => {
    const cells = connectionStrip([{ at: at(20), kind: "connected" }], now, 4);
    // six-hour cells; the connection started in the first one
    expect(cells).toEqual(["ok", "ok", "ok", "ok"]);
  });

  it("marks a period with a drop", () => {
    const cells = connectionStrip(
      [
        { at: at(20), kind: "connected" },
        { at: at(10), kind: "disconnected", reason: "eof" },
        { at: at(9), kind: "connected" },
      ],
      now,
      4,
    );
    expect(cells).toEqual(["ok", "ok", "warn", "ok"]);
  });

  it("stays down until the agent comes back", () => {
    const cells = connectionStrip(
      [
        { at: at(23), kind: "connected" },
        { at: at(15), kind: "disconnected", reason: "keepalive_timeout" },
      ],
      now,
      4,
    );
    expect(cells).toEqual(["ok", "err", "err", "err"]);
  });

  it("counts events from before the window", () => {
    const cells = connectionStrip([{ at: at(40), kind: "connected" }], now, 2);
    expect(cells).toEqual(["ok", "ok"]);
  });
});

describe("traffic", () => {
  it("is shown in bits per second", () => {
    expect(formatBitrate(0)).toBe("0 b/s");
    expect(formatBitrate(125)).toBe("1 kb/s");
    expect(formatBitrate(300000)).toBe("2.4 Mb/s");
    expect(formatBitrate(null)).toBe("–");
  });

  it("charts show the time, and the date for a week", () => {
    const noon = Date.UTC(2026, 9, 16, 12, 0) / 1000;
    expect(formatChartTime(noon, "1h")).toMatch(/^\d\d:\d\d$/);
    expect(formatChartTime(noon, "7d")).toContain("10");
  });
});

describe("logs", () => {
  const line = (seq: number, text: string, service = "web"): LogEntry => ({
    seq,
    at: Date.parse("2026-10-16T12:00:00Z"),
    ts: "2026-10-16T12:00:00Z",
    service,
    text,
  });
  const lines = [line(1, "GET / 200"), line(2, "ERROR boom", "db"), line(3, "GET /x 500")];

  it("a filter that cannot be compiled yet filters nothing, and says so", () => {
    expect(compileFilter("").regex).toBeNull();
    expect(compileFilter("[").invalid).toBe(true);
    expect(compileFilter("[").regex).toBeNull();
    expect(compileFilter("get").regex?.test("GET /")).toBe(true);
  });

  it("the view filters by service, by text, and stops at a pause", () => {
    expect(visibleLines(lines, { regex: null })).toHaveLength(3);
    expect(visibleLines(lines, { service: "db", regex: null })).toEqual([lines[1]]);
    expect(visibleLines(lines, { regex: /GET/ })).toHaveLength(2);
    expect(visibleLines(lines, { regex: null, upTo: 2 })).toHaveLength(2);
  });

  it("finds the level of a line", () => {
    expect(splitLevel("12:00 WARN slow query")).toEqual({
      before: "12:00 ",
      level: "WARN",
      after: " slow query",
    });
    expect(splitLevel("nothing special")).toBeNull();
    expect(levelTone("ERROR")).toBe("err");
    expect(levelTone("WARN")).toBe("warn");
    expect(levelTone("INFO")).toBe("info");
  });

  it("keeps the lines of several containers in time order", () => {
    const at = (seq: number, ts: string, service: string): LogEntry => ({
      seq,
      at: Date.parse(ts),
      ts,
      service,
      text: `${service} ${seq}`,
    });
    const view: LogEntry[] = [];

    // the first lines come per container: web's, then db's, which are partly older
    insertLines(view, [at(1, "2026-10-16T12:00:01Z", "web"), at(2, "2026-10-16T12:00:05Z", "web")]);
    insertLines(view, [at(3, "2026-10-16T12:00:02Z", "db"), at(4, "2026-10-16T12:00:05Z", "db")]);
    // then new lines, and one without a readable time
    insertLines(view, [at(5, "2026-10-16T12:00:06Z", "web"), at(6, "garbage", "db")]);

    expect(view.map((entry) => entry.seq)).toEqual([1, 3, 2, 4, 5, 6]);
  });

  it("a download holds the lines oldest first", () => {
    expect(logFileText(lines.slice(0, 2))).toBe(
      "2026-10-16T12:00:00Z web GET / 200\n2026-10-16T12:00:00Z db ERROR boom",
    );
  });
});

describe("stacks", () => {
  it("counts the changes of a revision", () => {
    expect(diffStats("a\nb\n", "a\nc\nd\n")).toEqual({ added: 2, removed: 1 });
    expect(diffStats("same", "same")).toEqual({ added: 0, removed: 0 });
  });

  it("finds the new lines for the editor", () => {
    expect(addedLines("a\nb", "a\nnew\nb\nend")).toEqual([2, 4]);
    expect(addedLines("a", "a")).toEqual([]);
  });

  it("lists the plain variables of an env file", () => {
    expect(
      envEntries("# comment\nTZ=Europe/Prague\n\nexport A='1'\nB=\"two\"\nnot a variable\n"),
    ).toEqual([
      { key: "TZ", value: "Europe/Prague" },
      { key: "A", value: "1" },
      { key: "B", value: "two" },
    ]);
  });

  it("reads the validation out of a rejected deploy", () => {
    const validation = {
      valid: false,
      errors: [{ line: 4, message: "x" }],
      warnings: [],
      services: [],
    };
    expect(rejectionOf({ response: { validation } })).toEqual(validation);
    expect(rejectionOf(new Error("x"))).toBeNull();
    expect(rejectionOf(null)).toBeNull();
  });
});

describe("databases", () => {
  const server = (fields: Partial<DbServer>): DbServer =>
    ({
      name: "db-main",
      engine: "mariadb",
      port: 3306,
      version: "",
      image: "mariadb:11.8",
      extensions: null,
      connections: 0,
      max_connections: 0,
      ...fields,
    }) as DbServer;

  it("gives an application the variables the panel adds, with the password as a reference", () => {
    expect(envSnippet(server({}), "shop", "shop")).toBe(
      "DB_HOST=db-main\nDB_PORT=3306\nDB_NAME=shop\nDB_USER=shop\nDB_PASSWORD=${secret:db/shop}",
    );
    const postgres = envSnippet(
      server({ name: "pg-main", engine: "postgres", port: 5432 }),
      "notes",
      "notes",
    );
    expect(postgres).toContain(
      "DATABASE_URL=postgres://notes:${secret:db/notes}@pg-main:5432/notes",
    );
  });

  it("names a server by what it said, or by its image", () => {
    expect(serverLabel(server({}))).toBe("MariaDB 11.8");
    expect(serverLabel(server({ version: "11.8.2-MariaDB-ubu2404" }))).toBe("MariaDB 11.8.2");
    expect(
      serverLabel(server({ engine: "postgres", version: "17.11 (Debian 17.11-1.pgdg13+2)" })),
    ).toBe("PostgreSQL 17.11");
    expect(
      serverLabel(
        server({
          engine: "postgres",
          image: "ghcr.io/immich-app/postgres:16-vectorchord",
          extensions: ["VectorChord"],
        }),
      ),
    ).toBe("PostgreSQL 16 + VectorChord");
    expect(serverLabel(server({ image: "registry.lan:5000/mariadb:10.11" }))).toBe("MariaDB 10.11");
  });

  it("measures the connections in use", () => {
    expect(connectionShare(server({ connections: 23, max_connections: 200 }))).toBe(12);
    expect(connectionShare(server({ connections: 5 }))).toBe(0);
    expect(connectionShare(server({ connections: 500, max_connections: 100 }))).toBe(100);
  });
});
