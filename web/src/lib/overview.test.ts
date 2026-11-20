import type { TFunction } from "i18next";
import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

import i18n from "../i18n";
import { matchesFilter } from "../test-filter";
import type { Certificate } from "./edge";
import type { HostSummary } from "./hosts";
import {
  agentSummary,
  attentionItems,
  backupRows,
  backupSummary,
  commandLink,
  commandTarget,
  containerSummary,
  disconnectQuery,
  disconnectReasons,
  disconnectsRead,
  hostKind,
  overviewCollections,
  overviewQueries,
  queueFilter,
  runTone,
  siteRows,
  sitesSummary,
  sortHosts,
  type AttentionContext,
  type SiteInput,
} from "./overview";
import * as f from "./overview.fixtures";
import type { Alert } from "./types";

const { now, pbDate } = f;
const minute = 60_000;
const hour = 60 * minute;
const day = 24 * hour;

const zone = process.env.TZ;

beforeAll(() => {
  // the times are the browser's, here Prague's
  process.env.TZ = "Europe/Prague";
});

afterAll(() => {
  if (zone === undefined) {
    delete process.env.TZ;
  } else {
    process.env.TZ = zone;
  }
});

afterEach(async () => {
  await i18n.changeLanguage("en");
});

/** The UI in a language: its t, and the formats of its locale. */
async function lang(code: "en" | "cs"): Promise<TFunction> {
  await i18n.changeLanguage(code);
  return i18n.getFixedT(code);
}

const hostNames = new Map(f.hosts.map((host) => [host.id, host.name]));

describe("the agents' tile", () => {
  it("counts the agents connected by their stream, and names the server offline most recently", async () => {
    const cs = await lang("cs");
    expect(agentSummary(f.hosts, now, cs)).toEqual({
      connected: 6,
      total: 7,
      note: { text: "rpi-garaz bez spojení 14 min", tone: "err" },
    });
    const en = await lang("en");
    expect(agentSummary(f.hosts, now, en).note.text).toBe("rpi-garaz offline for 14 min");
  });

  it("names the newest of several offline and counts the others", async () => {
    // listed first, but offline for longer
    const two = [
      f.host("h8", {
        name: "old-box",
        status: "offline",
        stream_open: false,
        last_seen: pbDate(now - 3 * day),
      }),
      ...f.hosts,
    ];
    const cs = await lang("cs");
    expect(agentSummary(two, now, cs)).toEqual({
      connected: 6,
      total: 8,
      note: { text: "rpi-garaz a 1 další bez spojení", tone: "err" },
    });
    const five = [
      ...two,
      ...[9, 10, 11, 12].map((n) =>
        f.host(`h${n}`, { status: "offline", stream_open: false, last_seen: pbDate(now - day) }),
      ),
    ];
    expect(agentSummary(five, now, cs).note.text).toBe("rpi-garaz a 5 dalších bez spojení");
    const en = await lang("en");
    expect(agentSummary(two, now, en).note.text).toBe("rpi-garaz and 1 more offline");
  });

  it("warns of a server that is not offline but whose agent takes no commands", async () => {
    const closed = f.hosts.map((host) =>
      host.id === "h7"
        ? { ...host, status: "online" as const, stream_open: true }
        : host.id === "h4"
          ? { ...host, stream_open: false }
          : host,
    );
    const cs = await lang("cs");
    expect(agentSummary(closed, now, cs)).toEqual({
      connected: 6,
      total: 7,
      note: { text: "1 agent nepřijímá příkazy", tone: "warn" },
    });
    const three = closed.map((host) =>
      ["h1", "h2"].includes(host.id)
        ? { ...host, status: "degraded" as const, stream_open: false }
        : host,
    );
    expect(agentSummary(three, now, cs).note.text).toBe("3 agenti nepřijímají příkazy");
    const en = await lang("en");
    expect(agentSummary(three, now, en).note.text).toBe("3 agents take no commands");
  });

  it("says all are connected, and that there are none", async () => {
    const all = f.hosts.filter((host) => host.id !== "h7");
    const cs = await lang("cs");
    expect(agentSummary(all, now, cs)).toEqual({
      connected: 6,
      total: 6,
      note: { text: "Všichni agenti jsou připojeni" },
    });
    expect(agentSummary([], now, cs)).toEqual({
      connected: 0,
      total: 0,
      note: { text: "Zatím žádné servery" },
    });
    const en = await lang("en");
    expect(agentSummary(all, now, en).note.text).toBe("Every agent is connected");
  });
});

describe("the containers' tile", () => {
  it("counts every container, past the 500 a page once held", () => {
    const many = Array.from({ length: 1200 }, (_, index) => ({
      host: `h${index % 20}`,
      state: index % 100 === 0 ? "restarting" : index % 10 === 0 ? "exited" : "running",
    }));
    const summary = containerSummary(many);
    expect(summary).toMatchObject({ running: 1080, restarting: 12, stopped: 108 });
    expect(summary.perHost.get("h0")).toBe(60);
    expect([...summary.perHost.values()].reduce((a, b) => a + b, 0)).toBe(1200);
  });

  it("counts any state but running and restarting as stopped, as the design's 38, 3 and 1", () => {
    expect(containerSummary(f.containers)).toMatchObject({
      running: 38,
      restarting: 1,
      stopped: 3,
    });
    const odd = ["paused", "dead", "created", "exited", "removing", ""].map((state) => ({
      host: "h1",
      state,
    }));
    expect(containerSummary(odd)).toMatchObject({ running: 0, restarting: 0, stopped: 6 });
  });
});

describe("the servers' table", () => {
  it("lists the servers by name, those offline last", () => {
    const shuffled = [...f.hosts].reverse();
    expect(sortHosts(shuffled).map((host) => host.name)).toEqual([
      "backup-node",
      "dev-box",
      "nas",
      "pve-01",
      "vps-hetzner",
      "vps-wedos",
      "rpi-garaz",
    ]);
    const two = [...f.hosts, f.host("h0", { name: "a-old", status: "offline" })];
    expect(
      sortHosts(two)
        .slice(-2)
        .map((host) => host.name),
    ).toEqual(["a-old", "rpi-garaz"]);
  });

  it("says what a server is: an edge or a backup target by its role, else its system", async () => {
    const cs = await lang("cs");
    const en = await lang("en");
    const kind = (host: Partial<HostSummary>) => [
      hostKind({ roles: [], os: "", ...host }, en),
      hostKind({ roles: [], os: "", ...host }, cs),
    ];
    expect(kind({ roles: ["docker", "edge"], os: "Debian" })).toEqual(["Edge proxy", "Edge proxy"]);
    expect(kind({ roles: ["docker", "backup"], os: "Debian" })).toEqual([
      "Backup target",
      "Cíl záloh",
    ]);
    expect(kind({ roles: ["edge", "backup"] })).toEqual(["Edge proxy", "Edge proxy"]);
    expect(kind({ roles: ["docker"], os: "TrueNAS SCALE" })).toEqual([
      "TrueNAS SCALE",
      "TrueNAS SCALE",
    ]);
    expect(kind({ roles: null as unknown as string[] })).toEqual(["unknown", "neznámé"]);
  });
});

function siteInput(fields: Partial<SiteInput> = {}): SiteInput {
  return {
    sites: f.sites,
    sitePhp: f.sitePhp,
    domains: f.domains,
    certificates: f.certificates,
    deployments: f.deployments,
    hosts: new Map(f.hosts.map((host) => [host.id, host])),
    ...fields,
  };
}

describe("the sites' rows", () => {
  it("show the design's sites in Czech, those that need a look first", async () => {
    const cs = await lang("cs");
    const rows = siteRows(siteInput(), now, cs);
    expect(
      rows.map((row) => [
        row.domain,
        row.more,
        row.source,
        row.hostName,
        row.tls.text,
        row.tls.tone,
        row.deploy.text,
      ]),
    ).toEqual([
      [
        "shop.example.org",
        1,
        "PHP 8.3 · WordPress",
        "vps-wedos",
        "Vyprší za 12 dní",
        "warn",
        "nasazeno před 6 dny",
      ],
      [
        "docs.example.cz",
        0,
        "statický · MkDocs",
        "vps-hetzner",
        "Vydává se (DNS-01)",
        "info",
        "probíhá",
      ],
      [
        "blog.example.cz",
        0,
        "statický · Hugo",
        "vps-hetzner",
        "Let's Encrypt · 89 dní",
        "ok",
        "nasazeno před 2 min",
      ],
      [
        "status.example.org",
        0,
        "statický · Astro",
        "vps-hetzner",
        "ZeroSSL · 41 dní",
        "ok",
        "nasazeno včera",
      ],
      [
        "wiki.lan",
        0,
        "statický · nahraný",
        "pve-01",
        "Interní CA · 301 dní",
        "ok",
        "nasazeno před 21 dny",
      ],
    ]);
    expect(rows.every((row) => row.online)).toBe(true);
    const en = await lang("en");
    expect(
      siteRows(siteInput(), now, en).map((row) => [row.source, row.tls.text, row.deploy.text]),
    ).toEqual([
      ["PHP 8.3 · WordPress", "Expires in 12 days", "deployed 6 days ago"],
      ["static · MkDocs", "Being issued (DNS-01)", "in progress"],
      ["static · Hugo", "Let's Encrypt · 89 days", "deployed 2 min. ago"],
      ["static · Astro", "ZeroSSL · 41 days", "deployed yesterday"],
      ["static · uploaded", "Internal CA · 301 days", "deployed 21 days ago"],
    ]);
  });

  it("are online when deployed, or a PHP site whose runtime runs, on a server that is not offline", async () => {
    const cs = await lang("cs");
    const online = (input: Partial<SiteInput>) =>
      Object.fromEntries(siteRows(siteInput(input), now, cs).map((row) => [row.id, row.online]));
    const sites = [
      f.site("static", { active_deployment: "d1" }),
      f.site("bare", {}),
      f.site("php", { type: "php", active_deployment: "" }),
      f.site("phpdown", { type: "php", active_deployment: "d9" }),
      f.site("away", { host: "h7", active_deployment: "d1" }),
    ];
    const sitePhp = [
      { site: "php", php_version: "8.4", template: "laravel" as const, state: "running" as const },
      {
        site: "phpdown",
        php_version: "8.4",
        template: "custom" as const,
        state: "failed" as const,
      },
    ];
    expect(online({ sites, sitePhp, domains: [] })).toEqual({
      static: true,
      bare: false,
      php: true,
      phpdown: false,
      away: false,
    });
  });

  it("say each TLS state of a site's domains, with the rest counted", async () => {
    const cs = await lang("cs");
    const tlsOf = (
      certificates: Certificate[],
      tls: "auto" | "off" | "internal" = "auto",
      installedOn = "h1",
    ) =>
      siteRows(
        siteInput({
          sites: [f.site("x", { active_deployment: "d1" })],
          domains: [
            f.domain("a", { hostname: "a.example.com", site: "x", tls, host: installedOn }),
            f.domain("b", { hostname: "b.example.com", site: "x", tls }),
            f.domain("c", { hostname: "c.example.com", site: "x", tls }),
          ],
          certificates,
        }),
        now,
        cs,
      )[0];
    const cert = (fields: Partial<Certificate>) =>
      f.certificate("k", { names: ["*.example.com"], ...fields });
    const cases: [Certificate[], string, string][] = [
      [[cert({})], "Let's Encrypt · 60 dní", "ok"],
      [[cert({ issuer: "manual", issuer_name: "" })], "Nahraný · 60 dní", "ok"],
      [[cert({ not_after: pbDate(now + 3 * day + hour) })], "Vyprší za 3 dny", "warn"],
      [[cert({ not_after: pbDate(now + day + hour) })], "Vyprší za 1 den", "warn"],
      [[cert({ not_after: pbDate(now - hour) })], "Vypršel", "err"],
      [[cert({ attempts: 1, last_error: "timeout" })], "Obnova selhala", "warn"],
      [
        [cert({ last_error: "no account", renew_after: pbDate(now - hour) })],
        "Obnova čeká",
        "warn",
      ],
      [
        [cert({ fingerprint: "", attempt_started: pbDate(now - minute), challenge: "http-01" })],
        "Vydává se (HTTP-01)",
        "info",
      ],
      [
        [cert({ fingerprint: "", attempt_started: pbDate(now - minute), challenge: "auto" })],
        "Vydává se",
        "info",
      ],
      [[cert({ fingerprint: "", status: "failed" })], "Nevydán", "err"],
      [[cert({ fingerprint: "", status: "pending" })], "Čeká", "off"],
      [[cert({ installed: {} })], "Instaluje se certifikát", "info"],
      [[], "Jen HTTP", "off"],
    ];
    for (const [certificates, text, tone] of cases) {
      const row = tlsOf(certificates);
      expect([text, row.tls.text, row.tls.tone]).toEqual([text, text, tone]);
      expect(row.domain).toBe("a.example.com");
      expect(row.more).toBe(2);
    }
    expect(tlsOf([cert({})], "off").tls).toMatchObject({
      text: "Jen HTTP",
      tone: "off",
      kind: "off",
    });
    // a site without a domain is named by itself
    const bare = siteRows(
      siteInput({ sites: [f.site("x", { name: "intranet" })], domains: [] }),
      now,
      cs,
    )[0];
    expect(bare).toMatchObject({
      domain: "intranet",
      more: 0,
      tls: { text: "Bez domény", tone: "off" },
    });
  });

  it("say the worst TLS of all of a site's domains, by its domain when it is not the first", async () => {
    const cs = await lang("cs");
    // a certificate for each hostname, as the panel orders them
    const blog = (b: Partial<Certificate>) =>
      siteRows(
        siteInput({
          sites: [f.site("s1", { name: "blog", active_deployment: "d1" }), f.site("s2", {})],
          domains: [
            f.domain("a", { hostname: "a.example.cz", site: "s1" }),
            f.domain("b", { hostname: "b.example.cz", site: "s1" }),
            f.domain("z", { hostname: "z.example.cz", site: "s2" }),
          ],
          certificates: [
            f.certificate("ka", { names: ["a.example.cz"] }),
            f.certificate("kb", { names: ["b.example.cz"], ...b }),
            f.certificate("kz", { names: ["z.example.cz"], not_after: pbDate(now + 5 * day) }),
          ],
        }),
        now,
        cs,
      );
    const expired = blog({ not_after: pbDate(now - day) });
    // the expired second domain comes before the other site's that expires soon
    expect(expired.map((row) => [row.domain, row.more, row.tls.text, row.tls.tone])).toEqual([
      ["a.example.cz", 1, "b.example.cz: Vypršel", "err"],
      ["z.example.cz", 0, "Vyprší za 5 dní", "warn"],
    ]);
    expect(expired[0].domainsTls.map((item) => [item.hostname, item.tls.text])).toEqual([
      ["a.example.cz", "Let's Encrypt · 60 dní"],
      ["b.example.cz", "Vypršel"],
    ]);
    const failed = blog({ fingerprint: "", status: "failed", attempts: 1, installed: null });
    expect([failed[0].tls.text, failed[0].tls.tone]).toEqual(["b.example.cz: Nevydán", "err"]);
    // the first domain's own TLS needs no name, when it is the worst or all are alike
    const fine = blog({});
    expect([fine[1].domain, fine[1].tls.text, fine[1].tls.tone]).toEqual([
      "a.example.cz",
      "Let's Encrypt · 60 dní",
      "ok",
    ]);
  });

  it("say each state of the deployment", async () => {
    const cs = await lang("cs");
    const deployOf = (
      sites: SiteInput["sites"],
      deployments: SiteInput["deployments"],
      sitePhp: SiteInput["sitePhp"] = [],
    ) =>
      siteRows(siteInput({ sites, deployments, sitePhp, domains: [] }), now, cs).map(
        (row) => row.deploy,
      );
    const live = f.site("x", {
      active_deployment: "d1",
      expand: {
        active_deployment: {
          id: "d1",
          status: "succeeded",
          generator: "",
          finished_at: pbDate(now - 3 * hour),
        },
      },
    } as never);
    expect(deployOf([live], [])).toEqual([{ text: "nasazeno před 3 h" }]);
    // a newer one under way, or failed
    const building = {
      site: "x",
      status: "building" as const,
      created: pbDate(now - minute),
      finished_at: "",
    };
    expect(deployOf([live], [building])).toEqual([{ text: "probíhá", tone: "info" }]);
    expect(deployOf([live], [{ ...building, status: "queued" }])).toEqual([
      { text: "probíhá", tone: "info" },
    ]);
    const failed = {
      ...building,
      status: "failed" as const,
      finished_at: pbDate(now - 5 * minute),
    };
    expect(deployOf([live], [failed])).toEqual([
      { text: "nasazení selhalo před 5 min", tone: "err" },
    ]);
    // one older than the live one is history
    const older = { ...failed, created: pbDate(now - day), finished_at: pbDate(now - day) };
    expect(deployOf([live], [older])).toEqual([{ text: "nasazeno před 3 h" }]);
    // a site that was never deployed
    expect(deployOf([f.site("x", {})], [])).toEqual([{ text: "zatím nenasazeno" }]);
    expect(deployOf([f.site("x", {})], [failed])).toEqual([
      { text: "nasazení selhalo před 5 min", tone: "err" },
    ]);
    // a PHP site without a deployment says how its runtime is
    const php = f.site("x", { type: "php" });
    const settings = (state: "pending" | "running" | "failed") => [
      { site: "x", php_version: "8.4", template: "nette" as const, state },
    ];
    expect(deployOf([php], [], settings("running"))).toEqual([{ text: "běží", tone: undefined }]);
    expect(deployOf([php], [], settings("pending"))).toEqual([
      { text: "spouští se", tone: "info" },
    ]);
    expect(deployOf([php], [], settings("failed"))).toEqual([{ text: "selhalo", tone: "err" }]);
  });

  it("put those that need a look first: err, warn, info, then the rest, each by domain", async () => {
    const cs = await lang("cs");
    const sites = ["d", "c", "b", "a", "e"].map((id) => f.site(id, { active_deployment: "d1" }));
    const domains = ["a", "b", "c", "d", "e"].map((id) =>
      f.domain(id, { hostname: `${id}.example.com`, site: id }),
    );
    const certificates = [
      f.certificate("ka", { names: ["a.example.com"] }),
      f.certificate("kb", { names: ["b.example.com"], not_after: pbDate(now + 5 * day) }),
      f.certificate("kc", { names: ["c.example.com"], not_after: pbDate(now - day) }),
      f.certificate("ke", { names: ["e.example.com"] }),
    ];
    const deployments = [
      { site: "e", status: "building" as const, created: pbDate(now), finished_at: "" },
    ];
    const rows = siteRows(siteInput({ sites, domains, certificates, deployments }), now, cs);
    // c expired, b expiring, e deploying, then a and d (HTTP only) by name
    expect(rows.map((row) => row.domain)).toEqual([
      "c.example.com",
      "b.example.com",
      "e.example.com",
      "a.example.com",
      "d.example.com",
    ]);
  });
});

describe("the sites' tile", () => {
  async function summary(input: Partial<SiteInput>, code: "en" | "cs" = "cs") {
    const t = await lang(code);
    return sitesSummary(siteRows(siteInput(input), now, t), now, t);
  }

  it("counts the sites online, and names the certificate that ends soonest", async () => {
    expect(await summary({})).toEqual({
      online: 5,
      total: 5,
      unit: "webů online",
      note: { text: "shop.example.org – certifikát vyprší za 12 dní", tone: "warn" },
    });
    expect(await summary({}, "en")).toMatchObject({
      unit: "sites online",
      note: { text: "shop.example.org – the certificate expires in 12 days" },
    });
  });

  it("puts a certificate that expired or was not issued before one that expires soon", async () => {
    const certificates = [
      ...f.certificates.filter((cert) => cert.id !== "c4"),
      f.certificate("c4", { names: ["status.example.org"], not_after: pbDate(now - hour) }),
    ];
    expect((await summary({ certificates })).note).toEqual({
      text: "status.example.org – certifikát vypršel",
      tone: "err",
    });
    const failed = f.certificates.map((cert) =>
      cert.id === "c2" ? { ...cert, attempt_started: "", status: "failed" as const } : cert,
    );
    expect((await summary({ certificates: failed })).note).toEqual({
      text: "docs.example.cz – certifikát se nepodařilo vydat",
      tone: "err",
    });
    expect((await summary({ certificates: failed }, "en")).note.text).toBe(
      "docs.example.cz – the certificate could not be issued",
    );
  });

  it("names the certificate of any domain of a site, not of its first only", async () => {
    // blog.example.cz is fine; its second domain has a certificate of its own
    const domains = [...f.domains, f.domain("o7", { hostname: "www.blog.example.cz", site: "s1" })];
    const valid = f.certificates.filter((cert) => cert.id !== "c3");
    const second = (fields: Partial<Certificate>) => [
      ...valid,
      f.certificate("c7", { names: ["www.blog.example.cz"], ...fields }),
    ];
    expect((await summary({ domains, certificates: second({}) })).note).toEqual({
      text: "Všechny certifikáty jsou platné",
    });
    expect(
      (await summary({ domains, certificates: second({ not_after: pbDate(now - day) }) })).note,
    ).toEqual({ text: "www.blog.example.cz – certifikát vypršel", tone: "err" });
    expect(
      (
        await summary({
          domains,
          certificates: second({ fingerprint: "", status: "failed", attempts: 1, installed: null }),
        })
      ).note,
    ).toEqual({ text: "www.blog.example.cz – certifikát se nepodařilo vydat", tone: "err" });
    expect(
      (await summary({ domains, certificates: second({ not_after: pbDate(now + 3 * day) }) })).note,
    ).toEqual({ text: "www.blog.example.cz – certifikát vyprší za 3 dny", tone: "warn" });
  });

  it("names a failed renewal with time left as such, the soonest first", async () => {
    const certificates = f.certificates.map((cert) =>
      cert.id === "c3" ? { ...cert, not_after: pbDate(now + 40 * day) } : cert,
    );
    expect((await summary({ certificates })).note).toEqual({
      text: "shop.example.org – obnova certifikátu selhala",
      tone: "warn",
    });
    const sooner = [
      ...certificates.filter((cert) => cert.id !== "c1"),
      f.certificate("c1", { names: ["blog.example.cz"], not_after: pbDate(now + 2 * day + hour) }),
    ];
    expect((await summary({ certificates: sooner })).note.text).toBe(
      "blog.example.cz – certifikát vyprší za 2 dny",
    );
  });

  it("counts the sites not online, when every certificate is fine", async () => {
    const certificates = f.certificates.filter((cert) => cert.id !== "c3");
    const sitePhp = [{ ...f.sitePhp[0], state: "failed" as const }];
    expect(await summary({ certificates, sitePhp })).toEqual({
      online: 4,
      total: 5,
      unit: "/ 5 webů online",
      note: { text: "1 web není online", tone: "warn" },
    });
    expect(await summary({ certificates, sitePhp }, "en")).toMatchObject({
      unit: "/ 5 sites online",
      note: { text: "1 site is not online" },
    });
    expect((await summary({ certificates })).note).toEqual({
      text: "Všechny certifikáty jsou platné",
    });
  });

  it("says there are no sites", async () => {
    expect(await summary({ sites: [] })).toEqual({
      online: 0,
      total: 0,
      unit: "webů online",
      note: { text: "Zatím žádné weby" },
    });
  });
});

describe("the backups' tile", () => {
  it("counts the runs of 24 hours, and names the newest failure by server/job at its time", async () => {
    const cs = await lang("cs");
    expect(backupSummary(f.jobs, f.backupRuns, hostNames, now, cs)).toEqual({
      succeeded: 11,
      total: 12,
      note: { text: "nas/photos selhala ve 03:00", tone: "err" },
    });
    const en = await lang("en");
    expect(backupSummary(f.jobs, f.backupRuns, hostNames, now, en).note.text).toBe(
      "nas/photos failed at 03:00",
    );
  });

  it("takes v or ve as the hour asks, and the day for a failure before today", async () => {
    const cs = await lang("cs");
    const failedAt = (at: string) =>
      backupSummary(f.jobs, [f.run("j3", "t3", at, "failed", { host: "h3" })], hostNames, now, cs)
        .note.text;
    // 05:00 and 03:00 in Prague
    expect(failedAt("2026-10-16T03:00:00Z")).toBe("nas/photos selhala v 05:00");
    expect(failedAt("2026-10-16T01:00:00Z")).toBe("nas/photos selhala ve 03:00");
    expect(failedAt("2026-10-16T10:00:00Z")).toBe("nas/photos selhala ve 12:00");
    // 23:00 yesterday in Prague
    expect(failedAt("2026-10-15T21:00:00Z")).toBe("nas/photos selhala (čt 23:00)");
    const en = await lang("en");
    expect(
      backupSummary(
        f.jobs,
        [f.run("j3", "t3", "2026-10-15T21:00:00Z", "failed")],
        hostNames,
        now,
        en,
      ).note.text,
    ).toBe("nas/photos failed (Thu 23:00)");
  });

  it("names the panel's own job by the panel, not its carrier", async () => {
    const cs = await lang("cs");
    const failed = f.run("j5", "t1", "2026-10-16T02:00:00Z", "failed", { host: "h5" });
    expect(backupSummary(f.jobs, [failed], hostNames, now, cs).note.text).toBe(
      "panel/panel (pb_data) selhala ve 04:00",
    );
  });

  it("names a failure that still stands before a newer one of a job that no longer runs", async () => {
    const cs = await lang("cs");
    const jobs = [...f.jobs, f.job("j6", { name: "old-label", host: "h2", label_removed: true })];
    const runs = [
      f.run("j3", "t3", "2026-10-16T01:00:00Z", "failed", { host: "h3" }),
      f.run("j6", "t1", "2026-10-16T03:00:00Z", "failed", { host: "h2" }),
    ];
    expect(backupSummary(jobs, runs, hostNames, now, cs)).toEqual({
      succeeded: 0,
      total: 2,
      note: { text: "nas/photos selhala ve 03:00", tone: "err" },
    });
  });

  it("names a failure that a later run followed, which the count still has", async () => {
    const cs = await lang("cs");
    const runs = [
      f.run("j3", "t3", "2026-10-16T01:00:00Z", "failed", { host: "h3" }),
      f.run("j3", "t3", "2026-10-16T02:00:00Z", "succeeded", { host: "h3" }),
    ];
    expect(backupSummary(f.jobs, runs, hostNames, now, cs)).toEqual({
      succeeded: 1,
      total: 2,
      note: { text: "nas/photos selhala ve 03:00", tone: "err" },
    });
  });

  it("names a failure while its retry is queued or running, as its alert stays open", async () => {
    const cs = await lang("cs");
    const ok = f.run("j1", "t1", "2026-10-16T04:00:00Z", "succeeded");
    const failed = f.run("j3", "t3", "2026-10-16T06:00:00Z", "failed", { host: "h3" });
    for (const status of ["running", "queued"] as const) {
      const retry = f.run("j3", "t3", "2026-10-16T06:30:00Z", status, {
        host: "h3",
        trigger: "retry",
        retry_of: failed.id,
        started_at: status === "running" ? "2026-10-16 06:30:01.000Z" : "",
      });
      // the retry stands for the failure in the count, and is no result yet
      expect(backupSummary(f.jobs, [ok, failed, retry], hostNames, now, cs)).toEqual({
        succeeded: 1,
        total: 1,
        note: { text: "nas/photos selhala v 08:00", tone: "err" },
      });
      // nothing else ran: still not that none ran
      expect(backupSummary(f.jobs, [failed, retry], hostNames, now, cs)).toEqual({
        succeeded: 0,
        total: 0,
        note: { text: "nas/photos selhala v 08:00", tone: "err" },
      });
    }
  });

  it("names the failure the count has when a retry never ran", async () => {
    const cs = await lang("cs");
    const failed = f.run("j3", "t3", "2026-10-16T01:00:00Z", "failed", { host: "h3" });
    const runs = [
      failed,
      // a retry that never ran stands for no failure
      f.run("j3", "t3", "2026-10-16T01:30:00Z", "skipped", {
        host: "h3",
        trigger: "retry",
        retry_of: failed.id,
        started_at: "",
      }),
      f.run("j3", "t3", "2026-10-16T02:00:00Z", "succeeded", { host: "h3" }),
    ];
    expect(backupSummary(f.jobs, runs, hostNames, now, cs)).toEqual({
      succeeded: 1,
      total: 3,
      note: { text: "nas/photos selhala ve 03:00", tone: "err" },
    });
  });

  it("counts the runs skipped, says all went fine, and that none ran", async () => {
    const cs = await lang("cs");
    const ok = f.backupRuns.filter((run) => run.status === "succeeded");
    const skipped = [
      ...ok,
      f.run("j4", "t3", "2026-10-16T05:00:00Z", "skipped"),
      f.run("j1", "t3", "2026-10-16T05:00:00Z", "skipped"),
    ];
    expect(backupSummary(f.jobs, skipped, hostNames, now, cs)).toEqual({
      succeeded: 11,
      total: 13,
      note: { text: "2 přeskočené", tone: "warn" },
    });
    expect(backupSummary(f.jobs, ok, hostNames, now, cs)).toEqual({
      succeeded: 11,
      total: 11,
      note: { text: "Všechny proběhly v pořádku" },
    });
    // running and queued are no result yet
    const pending = [f.run("j1", "t1", "2026-10-16T10:20:00Z", "running")];
    expect(backupSummary(f.jobs, pending, hostNames, now, cs)).toEqual({
      succeeded: 0,
      total: 0,
      note: { text: "Za posledních 24 h neproběhla žádná záloha" },
    });
    const en = await lang("en");
    expect(backupSummary(f.jobs, skipped, hostNames, now, en).note.text).toBe("2 skipped");
    expect(backupSummary(f.jobs, [], hostNames, now, en).note.text).toBe(
      "No backup ran in the last 24 h",
    );
  });
});

describe("the latest runs", () => {
  const targetNames = new Map(f.targets.map((target) => [target.id, target.name]));

  it("show each job by its worst latest run, failures first, then the newest", async () => {
    const cs = await lang("cs");
    const rows = backupRows(f.jobs, targetNames, f.backupRuns, hostNames, now, cs);
    expect(
      rows.map((row) => [
        row.job.name,
        row.server,
        row.targets.join(" + "),
        row.result,
        row.when,
        row.tone,
      ]),
    ).toEqual([
      ["photos", "nas", "backup-node", "Chyba", "03:00", "err"],
      ["vaultwarden", "vps-hetzner", "Backblaze B2 + backup-node", "OK · 84 MB", "12:00", "ok"],
      ["panel (pb_data)", "panel", "Backblaze B2", "OK · 312 MB", "04:00", "ok"],
      ["immich-library", "nas", "Backblaze B2", "OK · 2,1 GB", "02:14", "ok"],
      ["umami-db", "vps-hetzner", "backup-node", "OK · 46 MB", "01:30", "ok"],
    ]);
    expect(rows[0]).toMatchObject({
      title: "repository is already locked",
      link: "/backups?job=j3&target=t3",
    });
    expect(rows[1].link).toBe("/backups?job=j1&target=t1");
    const en = await lang("en");
    expect(
      backupRows(f.jobs, targetNames, f.backupRuns, hostNames, now, en).map((row) => row.result),
    ).toEqual(["Error", "OK · 84 MB", "OK · 312 MB", "OK · 2.1 GB", "OK · 46 MB"]);
  });

  it("show a target's failure behind another's newer success, and link to that target", async () => {
    const cs = await lang("cs");
    const runs = [
      f.run("j1", "t3", "2026-10-16T09:00:00Z", "failed", { error: "Fatal: wrong password" }),
      f.run("j1", "t1", "2026-10-16T10:00:00Z", "succeeded", { total_bytes: 84e6 }),
    ];
    const [row] = backupRows(f.jobs, targetNames, runs, hostNames, now, cs);
    expect(row).toMatchObject({
      result: "Chyba",
      tone: "err",
      title: "wrong password",
      when: "11:00",
      link: "/backups?job=j1&target=t3",
    });
  });

  it("say the size of the largest of the job's newest backups to each target", async () => {
    const cs = await lang("cs");
    const runs = [
      f.run("j1", "t1", "2026-10-16T09:00:00Z", "succeeded", { total_bytes: 84e6 }),
      f.run("j1", "t3", "2026-10-16T10:00:00Z", "succeeded", { total_bytes: 80e6 }),
      f.run("j1", "t3", "2026-10-16T05:00:00Z", "succeeded", { total_bytes: 99e6 }),
    ];
    const [row] = backupRows(f.jobs, targetNames, runs, hostNames, now, cs);
    expect(row).toMatchObject({
      result: "OK · 84 MB",
      when: "12:00",
      link: "/backups?job=j1&target=t3",
    });
  });

  it("say each status, a job without a target, and leave out jobs that did not run", async () => {
    const cs = await lang("cs");
    const jobs = [
      ...f.jobs,
      f.job("j9", { name: "nowhere", targets: [] }),
      f.job("j8", { name: "idle", targets: ["t1"] }),
    ];
    const statuses = ["queued", "running", "skipped", "cancelled"] as const;
    const runs = [
      ...statuses.map((status, index) =>
        f.run(
          ["j1", "j2", "j4", "j5"][index],
          index === 0 ? "t3" : index === 2 ? "t3" : "t1",
          "2026-10-16T09:00:00Z",
          status,
        ),
      ),
      f.run("j9", "t1", "2026-10-16T08:00:00Z", "succeeded", { total_bytes: 1e6 }),
    ];
    const rows = backupRows(jobs, targetNames, runs, hostNames, now, cs);
    expect(
      Object.fromEntries(rows.map((row) => [row.job.name, [row.result, row.tone, row.targets]])),
    ).toEqual({
      vaultwarden: ["Ve frontě", "info", ["Backblaze B2", "backup-node"]],
      "immich-library": ["Běží", "info", ["Backblaze B2"]],
      "umami-db": ["Přeskočeno", "warn", ["backup-node"]],
      "panel (pb_data)": ["Zrušeno", "off", ["Backblaze B2"]],
      nowhere: ["OK · 1 MB", "ok", []],
    });
  });

  it("give each status its tone, as the Backups page", () => {
    expect(
      (["queued", "running", "succeeded", "failed", "skipped", "cancelled"] as const).map(runTone),
    ).toEqual(["info", "info", "ok", "err", "warn", "off"]);
  });
});

function context(fields: Partial<AttentionContext> = {}): AttentionContext {
  return {
    hosts: new Map(f.hosts.map((host) => [host.id, host])),
    jobs: new Map(f.jobs.map((job) => [job.id, job])),
    targets: new Map(f.targets.map((target) => [target.id, target.name])),
    certificates: new Map(f.certificates.map((cert) => [cert.id, cert])),
    providers: new Map(f.providers.map((provider) => [provider.id, provider])),
    runs: f.backupRuns,
    reasons: new Map([["h7", "eof"]]),
    canAudit: false,
    ...fields,
  };
}

function alert(rule: string, fields: Partial<Alert> = {}): Alert {
  return {
    id: rule,
    collectionId: "alerts",
    collectionName: "alerts",
    rule,
    target: "",
    host: "",
    severity: "warning",
    status: "open",
    first_seen: pbDate(now - 2 * hour),
    last_seen: pbDate(now - hour),
    message: `stored ${rule}`,
    ...fields,
  } as Alert;
}

describe("needs attention", () => {
  it("names each alert of the design by its subject, with what the page knows of it", async () => {
    const cs = await lang("cs");
    expect(attentionItems(f.alerts, context(), now, cs)).toEqual([
      {
        id: "a1",
        tone: "err",
        title: "rpi-garaz: agent bez spojení",
        detail: "Poslední heartbeat 12:10 · spojení bylo uzavřeno",
        link: "/servers/h7",
      },
      {
        id: "a2",
        tone: "err",
        title: "Záloha nas/photos selhala",
        detail: "repository is already locked",
        link: "/backups?job=j3&target=t3",
      },
      {
        id: "a3",
        tone: "warn",
        title: "Certifikát shop.example.org vyprší za 12 dní",
        detail: "Auto-obnova selhala 2× – DNS-01 timeout (WEDOS API)",
        link: "/domains?certificate=c3",
      },
      {
        id: "a4",
        tone: "warn",
        title: "DNS poskytovatel WEDOS hlásí chybu",
        detail: "WAPI answered 2051: access not allowed from this IP",
        link: "/domains?provider=p1",
      },
    ]);
    const en = await lang("en");
    expect(
      attentionItems(f.alerts, context(), now, en).map((item) => [item.title, item.detail]),
    ).toEqual([
      ["rpi-garaz: the agent is not connected", "Last heartbeat 12:10 · the connection was closed"],
      ["Backup nas/photos failed", "repository is already locked"],
      [
        "The certificate shop.example.org expires in 12 days",
        "Automatic renewal failed 2× – DNS-01 timeout (WEDOS API)",
      ],
      [
        "The DNS provider WEDOS reports an error",
        "WAPI answered 2051: access not allowed from this IP",
      ],
    ]);
  });

  it("puts critical first, then warnings, then notes, each the newest first", async () => {
    const cs = await lang("cs");
    const items = attentionItems(
      [
        alert("i-old", { severity: "info", last_seen: pbDate(now - 3 * hour) }),
        alert("w-old", { severity: "warning", last_seen: pbDate(now - 5 * hour) }),
        alert("c-old", { severity: "critical", last_seen: pbDate(now - 9 * hour) }),
        alert("i-new", { severity: "info", last_seen: pbDate(now - minute) }),
        alert("w-new", { severity: "warning", last_seen: pbDate(now - minute) }),
        alert("c-new", { severity: "critical", last_seen: pbDate(now - 2 * hour) }),
      ],
      context(),
      now,
      cs,
    );
    expect(items.map((item) => [item.id, item.tone])).toEqual([
      ["c-new", "err"],
      ["c-old", "err"],
      ["w-new", "warn"],
      ["w-old", "warn"],
      ["i-new", "info"],
      ["i-old", "info"],
    ]);
  });

  it("names each rule's subject, its server when a job is not loaded, and no id or server for another", async () => {
    const cs = await lang("cs");
    const title = (a: Alert, ctx = context()) => attentionItems([a], ctx, now, cs)[0].title;
    expect(title(alert("backup_check_failed", { target: "t1", host: "h5" }))).toBe(
      "Repozitář Backblaze B2 obsahuje chyby",
    );
    // a target not loaded: its server is no repository
    expect(title(alert("backup_check_unverified", { target: "t9", host: "h3" }))).toBe(
      "Repozitář záloh není ověřený",
    );
    expect(title(alert("backup_check_failed", { target: "t9", host: "h3" }))).toBe(
      "Repozitář záloh obsahuje chyby",
    );
    expect(title(alert("backup_restore_test_failed", { target: "j4", host: "h1" }))).toBe(
      "umami-db: test obnovy selhal",
    );
    expect(title(alert("backup_skipped", { target: "j5:t1", host: "h5" }))).toBe(
      "Záloha panel/panel (pb_data) byla přeskočena",
    );
    expect(title(alert("backup_retention", { target: "j1:t3", host: "h1" }))).toBe(
      "Záloha vps-hetzner/vaultwarden: staré snapshoty se neodstraňují",
    );
    expect(title(alert("backup_failed", { target: "gone:t1", host: "h3" }))).toBe(
      "Záloha nas selhala",
    );
    expect(title(alert("db_temporary_left", { target: "d9", host: "h2" }))).toBe(
      "pve-01: dočasná databáze se nesmazala",
    );
    expect(title(alert("backup_restore", { target: "h3", host: "h3" }))).toBe(
      "nas: obnova ze zálohy potřebuje pozornost",
    );
    expect(title(alert("certificate_failed", { target: "c2" }))).toBe(
      "Certifikát docs.example.cz se nepodařilo získat",
    );
    // a certificate or a provider not loaded is named by no id
    expect(title(alert("certificate_expiring", { target: "gone" }))).toBe("Certifikát brzy vyprší");
    expect(title(alert("certificate_failed", { target: "gone" }))).toBe(
      "Certifikát se nepodařilo získat",
    );
    expect(title(alert("dns_provider_failed", { target: "k3j2h1g0f9e8d7c" }))).toBe(
      "DNS poskytovatel hlásí chybu",
    );
    const en = await lang("en");
    expect(
      [
        "backup_check_failed",
        "backup_check_unverified",
        "certificate_expiring",
        "dns_provider_failed",
      ].map(
        (rule) =>
          attentionItems([alert(rule, { target: "x9", host: "h3" })], context(), now, en)[0].title,
      ),
    ).toEqual([
      "A backup repository has errors",
      "A backup repository is not verified",
      "A certificate expires soon",
      "A DNS provider reports an error",
    ]);
    expect(title(alert("agent_offline", { target: "h9", host: "h9" }))).toBe(
      "h9: agent bez spojení",
    );
    // a certificate that expired since its alert was raised
    const expired = new Map([["c3", { ...f.certificates[2], not_after: pbDate(now - hour) }]]);
    expect(
      title(alert("certificate_expiring", { target: "c3" }), context({ certificates: expired })),
    ).toBe("Certifikát shop.example.org vypršel");
  });

  it("says a certificate that was never issued failed to be issued, not to be renewed", async () => {
    const cs = await lang("cs");
    // a new domain whose DNS does not point at its edge yet: its first order failed
    const order = f.certificate("cx", {
      names: ["shop.example.org"],
      fingerprint: "",
      status: "failed",
      installed: null,
      not_before: "",
      not_after: "",
      attempts: 1,
      last_error: "acme: error: 400 :: urn:ietf:params:acme:error:dns :: NXDOMAIN\nmore",
    });
    const item = (t: TFunction) =>
      attentionItems(
        [alert("certificate_failed", { target: "cx", severity: "critical" })],
        context({ certificates: new Map([["cx", order]]) }),
        now,
        t,
      )[0];
    expect(item(cs)).toMatchObject({
      title: "Certifikát shop.example.org se nepodařilo získat",
      detail: "Vydání selhalo 1× – acme: error: 400 :: urn:ietf:params:acme:error:dns :: NXDOMAIN",
    });
    expect(item(await lang("en")).detail).toBe(
      "Issuing failed 1× – acme: error: 400 :: urn:ietf:params:acme:error:dns :: NXDOMAIN",
    );
  });

  it("falls back to when it was opened for what the page knows nothing of", async () => {
    const cs = await lang("cs");
    const detail = (a: Alert, ctx = context()) => attentionItems([a], ctx, now, cs)[0].detail;
    // no reason without the diagnostics plugin
    expect(
      detail(alert("agent_offline", { target: "h7", host: "h7" }), context({ reasons: new Map() })),
    ).toBe("Poslední heartbeat 12:10");
    expect(detail(alert("agent_offline", { target: "h9", host: "h9" }))).toBe("otevřeno před 2 h");
    // a run without an error, or none loaded
    expect(detail(alert("backup_failed", { target: "j4:t1" }))).toBe("otevřeno před 2 h");
    expect(detail(alert("backup_skipped", { target: "j1:t3" }))).toBe("otevřeno před 2 h");
    const skipped = [
      f.run("j1", "t3", "2026-10-16T09:00:00Z", "skipped", {
        error: "The run before still held the repository.",
      }),
    ];
    expect(detail(alert("backup_skipped", { target: "j1:t3" }), context({ runs: skipped }))).toBe(
      "The run before still held the repository.",
    );
    // a certificate being issued, one that failed without an attempt, or none loaded
    expect(detail(alert("certificate_failed", { target: "c2" }))).toBe("Vydává se (DNS-01)");
    expect(detail(alert("certificate_expiring", { target: "c1" }))).toBe("otevřeno před 2 h");
    expect(detail(alert("certificate_expiring", { target: "gone" }))).toBe("otevřeno před 2 h");
    expect(detail(alert("dns_provider_failed", { target: "p9" }))).toBe("otevřeno před 2 h");
    expect(detail(alert("backup_check_failed", { target: "t1" }))).toBe("otevřeno před 2 h");
    // in words that take a relative time, in both languages
    const fresh = alert("panel_backup", { first_seen: pbDate(now - 20_000) });
    expect(detail(fresh)).toBe("otevřeno před 20 s");
    const en = await lang("en");
    expect(attentionItems([alert("backup_retention")], context(), now, en)[0].detail).toBe(
      "opened 2 hr. ago",
    );
  });

  it("links each alert where it is fixed, and the audit log's to the log for operators only", async () => {
    const cs = await lang("cs");
    const link = (a: Alert, canAudit = false) =>
      attentionItems([a], context({ canAudit }), now, cs)[0].link;
    const audit = alert("audit_integrity", { target: "audit_log", severity: "critical" });
    expect(link(audit)).toBeNull();
    expect(link(audit, true)).toBe("/audit");
    expect(link(alert("backup_check_failed", { target: "t1", host: "h5" }))).toBe(
      "/backups?check=t1",
    );
    expect(link(alert("recovery_kit", { target: "panel" }))).toBe("/settings#recovery-kit");
    expect(link(alert("backup_restore", { target: "h3", host: "h3" }))).toBe("/servers/h3");
    expect(link(alert("db_temporary_left", { target: "d1", host: "h2" }))).toBe("/databases");
  });

  it("says every rule the panel raises in Czech, with no stored English", async () => {
    const cs = await lang("cs");
    const rules = [
      "agent_offline",
      "audit_integrity",
      "backup_failed",
      "backup_skipped",
      "backup_retention",
      "backup_check_failed",
      "backup_check_unverified",
      "recovery_kit",
      "panel_backup",
      "panel_restored",
      "backup_restore_test_failed",
      "backup_restore_test_unverified",
      "db_temporary_left",
      "certificate_failed",
      "certificate_expiring",
      "dns_provider_failed",
      "backup_restore",
    ];
    const items = attentionItems(
      rules.map((rule) => alert(rule, { target: "x", host: "h1" })),
      context(),
      now,
      cs,
    );
    for (const item of items) {
      expect(item.title).not.toMatch(/stored|the |is |was /);
    }
    expect(items).toHaveLength(rules.length);
  });
});

describe("the command queue", () => {
  it("hides reads, measures and steps, as PocketBase reads its filter", () => {
    const shown = f.commands.filter((command) =>
      matchesFilter(command as unknown as Record<string, unknown>, queueFilter),
    );
    expect(shown.map((command) => command.id)).toEqual(["cmd1", "cmd2", "cmd3", "cmd4", "cmd5"]);
    const hidden = [
      "logs.follow",
      "logs.stop",
      "exec.start",
      "exec.stop",
      "acme.challenge.present",
      "acme.challenge.cleanup",
      "db.adminer.start",
      "db.adminer.stop",
      "site.php.status",
      "site.php.mail",
      "site.detect",
      "stack.validate",
      "db.stats",
      "backup.snapshots",
      "backup.browse",
    ];
    for (const type of hidden) {
      expect([type, matchesFilter({ type, parent: "", resource: "" }, queueFilter)]).toEqual([
        type,
        false,
      ]);
    }
    for (const type of [
      "backup.run",
      "backup.init",
      "site.php.apply",
      "db.database.create",
      "stack.deploy",
    ]) {
      expect([type, matchesFilter({ type, parent: "", resource: "" }, queueFilter)]).toEqual([
        type,
        true,
      ]);
    }
    // a target's hourly measure, but not its first init
    expect(
      matchesFilter({ type: "backup.init", parent: "", resource: "backup-usage:t1" }, queueFilter),
    ).toBe(false);
    expect(
      matchesFilter({ type: "backup.init", parent: "", resource: "backup:t1" }, queueFilter),
    ).toBe(true);
    // a step of a provisioning
    expect(matchesFilter({ type: "site.dns", parent: "x9", resource: "" }, queueFilter)).toBe(
      false,
    );
  });

  it("says what a command acts on", () => {
    expect(
      commandTarget({ type: "site.deploy", label: "docs.example.cz", host_name: "vps-hetzner" }),
    ).toBe("docs.example.cz → vps-hetzner");
    expect(commandTarget({ type: "stack.deploy", label: "immich", host_name: "nas" })).toBe(
      "immich → nas",
    );
    expect(commandTarget({ type: "stack.restart", label: "immich", host_name: "nas" })).toBe(
      "immich @ nas",
    );
    expect(commandTarget({ type: "edge.apply", label: "", host_name: "vps-hetzner" })).toBe(
      "vps-hetzner",
    );
    expect(commandTarget({ type: "site.provision", label: "blog", host_name: "" })).toBe("blog");
    expect(commandTarget({ type: "x", label: "", host_name: "" })).toBe("");
  });

  it("links each command to its object, by the resource it holds", () => {
    const ctx = {
      stacks: f.stacks,
      runs: [{ command: "cmd5", job: "j3", target: "t3" }],
    };
    const link = (fields: Parameters<typeof f.command>[1]) =>
      commandLink(f.command("c", { host: "h3", ...fields }), ctx);
    expect(link({ resource: "site:s2", type: "site.deploy" })).toBe("/sites/s2");
    expect(link({ resource: "container:abc123", type: "container.stop" })).toBe(
      "/servers/h3/containers/abc123",
    );
    // a stack's name is its own on its server only
    expect(link({ resource: "stack:immich", type: "stack.restart" })).toBe("/stacks/k1");
    expect(link({ resource: "stack:immich", type: "stack.restart", host: "h2" })).toBe(
      "/stacks/k11",
    );
    expect(link({ resource: "stack:gone", type: "stack.deploy" })).toBe("/servers/h3");
    expect(
      commandLink(
        f.command("cmd5", { host: "h3", resource: "backup:t3", type: "backup.run" }),
        ctx,
      ),
    ).toBe("/backups?job=j3&target=t3");
    expect(link({ resource: "backup:t3", type: "backup.unlock" })).toBe("/backups");
    expect(link({ resource: "backup-integrity:t1", type: "backup.check" })).toBe(
      "/backups?check=t1",
    );
    expect(link({ resource: "db:abc/shop", type: "backup.restore" })).toBe("/databases");
    expect(link({ resource: "db-test:abc", type: "backup.restore_test" })).toBe("/databases");
    expect(link({ resource: "", type: "db.database.create" })).toBe("/databases");
    expect(link({ resource: "stack:shop", type: "db.network.attach" })).toBe("/databases");
    expect(link({ resource: "edge", type: "edge.apply" })).toBe("/servers/h3/proxy");
    expect(link({ resource: "agent", type: "agent.update" })).toBe("/servers/h3/agent");
    expect(link({ resource: "", type: "image.pull" })).toBe("/servers/h3");
    expect(link({ resource: "volume:data", type: "backup.restore" })).toBe("/servers/h3");
    expect(link({ resource: "", type: "site.provision", host: "" })).toBeNull();
    expect(link({ resource: "edge", type: "edge.apply", host: "" })).toBeNull();
  });
});

describe("what the Overview reads", () => {
  it("is these lists, each with its fields only", () => {
    expect(overviewQueries(now)).toEqual({
      containers: { fields: "id,host,state,updated" },
      stacks: {
        sort: "name",
        perPage: 500,
        fields: "id,collectionId,collectionName,name,host,updated",
      },
      commands: {
        filter:
          "parent = '' && resource !~ 'backup-usage:%' && type !~ 'logs.%' && type !~ 'exec.%' && " +
          "type !~ 'acme.challenge.%' && type !~ 'db.adminer.%' && type != 'site.php.status' && " +
          "type != 'site.php.mail' && type != 'site.detect' && type != 'stack.validate' && " +
          "type != 'db.stats' && type != 'backup.snapshots' && type != 'backup.browse'",
        sort: "-created",
        perPage: 5,
        skipTotal: true,
        inPlace: true,
        fields:
          "id,collectionId,collectionName,created,updated,host,host_name,type,label,status,resource",
      },
      sites: {
        sort: "name",
        perPage: 500,
        expand: "active_deployment",
        fields:
          "id,collectionId,collectionName,updated,name,host,type,source,generator,active_deployment," +
          "expand.active_deployment.id,expand.active_deployment.status," +
          "expand.active_deployment.generator,expand.active_deployment.finished_at",
      },
      sitePhp: {
        perPage: 500,
        fields: "id,collectionId,collectionName,updated,site,php_version,template,state",
      },
      domains: {
        filter: "site != ''",
        sort: "hostname",
        perPage: 1000,
        fields: "id,collectionId,collectionName,updated,hostname,host,site,tls",
      },
      certificates: {
        sort: "-not_after",
        perPage: 500,
        fields:
          "id,collectionId,collectionName,updated,names,issuer,issuer_name,not_before,not_after," +
          "status,fingerprint,installed,attempts,attempt_started,renew_after,last_error,challenge,panel",
      },
      deployments: {
        filter:
          "(status = 'queued' || status = 'building' || status = 'failed') && created >= \"2026-10-09 00:00:00.000Z\"",
        sort: "-created",
        perPage: 100,
        skipTotal: true,
        fields: "id,collectionId,collectionName,created,updated,site,status,finished_at",
      },
      jobs: {
        sort: "name",
        perPage: 1000,
        fields:
          "id,collectionId,collectionName,updated,name,host,source_kind,targets,origin,database,site,label_removed,enabled",
      },
      targets: { perPage: 500, fields: "id,collectionId,collectionName,updated,name" },
      runs: {
        filter: 'created >= "2026-10-14 09:00:00.000Z"',
        sort: "created",
        fields:
          "id,created,updated,job,target,host,status,trigger,scheduled_at,started_at,finished_at,total_bytes,error,command,retry_of",
      },
      dnsProviders: {
        perPage: 100,
        fields: "id,collectionId,collectionName,updated,name,last_error",
      },
    });
    expect(Object.keys(overviewCollections).sort()).toEqual(
      Object.keys(overviewQueries(now)).sort(),
    );
  });

  it("reads the runs from the hour 49 hours back, which changes once an hour", () => {
    const at = (iso: string) => overviewQueries(Date.parse(iso)).runs.filter;
    expect(at("2026-10-16T10:59:59Z")).toBe(at("2026-10-16T10:00:00Z"));
    expect(at("2026-10-16T11:00:00Z")).toBe('created >= "2026-10-14 10:00:00.000Z"');
  });

  // A run waits up to 12 hours in the queue and, once it starts, has 12
  // more before its deadline: it may end 24 hours after it was made.
  it("reads every run that may have ended in the last 24 hours, so the tile counts it", async () => {
    const filter = overviewQueries(now).runs.filter!;
    const ran = (made: number, waited: number, took: number) =>
      f.run("j3", "t3", new Date(now - made * hour).toISOString(), "failed", {
        host: "h3",
        started_at: pbDate(now - (made - waited) * hour),
        finished_at: pbDate(now - (made - waited - took) * hour),
        error: "Fatal: unable to open repository",
      });
    const runs = [
      // made 37.5 hours ago, waited 12, failed 23.5 hours ago
      ran(37.5, 12, 2),
      // made 38 hours ago, waited 4, stopped at its deadline 22 hours ago
      ran(38, 4, 12),
      // made 47 hours ago, waited its 12 hours, ran its 12: 23 hours ago
      ran(47, 12, 12),
    ];
    expect(runs.filter((run) => matchesFilter(run, filter))).toEqual(runs);
    const cs = await lang("cs");
    expect(backupSummary(f.jobs, runs, hostNames, now, cs)).toMatchObject({
      succeeded: 0,
      total: 3,
    });
    // made before, it ended before the 24 hours
    expect(matchesFilter({ created: pbDate(now - 50 * hour) }, filter)).toBe(false);
  });

  it("reads the last disconnects of every server offline in one list", () => {
    expect(disconnectQuery(["h7"])).toEqual({
      filter: "(host = \"h7\") && kind = 'connection.disconnected'",
      sort: "-created",
      fields: "id,host,data,created",
      skipTotal: true,
    });
    const filter = disconnectQuery(["h7", "h8", "h9", "h10"]).filter;
    expect(filter).toBe(
      '(host = "h7" || host = "h8" || host = "h9" || host = "h10") && ' +
        "kind = 'connection.disconnected'",
    );
    const event = (host: string, kind = "connection.disconnected") => ({ host, kind });
    expect(matchesFilter(event("h9"), filter)).toBe(true);
    expect(matchesFilter(event("h1"), filter)).toBe(false);
    expect(matchesFilter(event("h9", "connection.connected"), filter)).toBe(false);
    expect(disconnectsRead).toBe(200);
  });

  it("takes each server's reason from its newest disconnect only", () => {
    const event = (host: string, reason?: unknown) => ({
      host,
      data: reason === undefined ? null : { reason },
    });
    expect(
      disconnectReasons([
        event("h7", "eof"),
        event("h8"),
        event("h7", "keepalive"),
        event("h8", "reset"),
        event("h9", 42),
        event("h10", "canceled"),
      ]),
    ).toEqual(
      new Map([
        ["h7", "eof"],
        ["h10", "canceled"],
      ]),
    );
    expect(disconnectReasons([])).toEqual(new Map());
  });
});
