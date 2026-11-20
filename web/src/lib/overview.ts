// What the Overview shows (#33), worked out from the records it reads: each
// card's figures and lines, the rows of its tables and lists, and where each
// links. Pure, so that every rule is tested without the page.
import type { TFunction } from "i18next";

import type { Tone } from "../components/ui";
import {
  failedRuns,
  formatMoment,
  lastRun,
  overviewRunFields,
  resticLine,
  runMoment,
  shortError,
  successSince,
  type BackupJob,
  type BackupRun,
  type RunStatus,
} from "./backupJobs";
import { czechAt } from "./backupChecks";
import {
  caName,
  certificateState,
  domainTls,
  expiringWithin,
  stateTone,
  validity,
  type CertState,
  type DomainTls,
} from "./certificates";
import type { DnsProvider } from "./dns";
import type { Certificate, Domain } from "./edge";
import { alertLink, alertText, type AlertSubject } from "./events";
import { formatBytes, formatDuration, formatHour, formatRelative, parseDate } from "./format";
import type { HostSummary } from "./hosts";
import { pb } from "./pb";
import type { SitePhp } from "./php";
import type { ListOptions } from "./realtime";
import type { Deployment, Site } from "./sites";
import type { Alert, Command, Container, HostEvent, Stack } from "./types";

const minute = 60 * 1000;
const hour = 60 * minute;
const day = 24 * hour;

/** The commands the queue leaves out: what reads or measures, as a log, a
 * terminal, a PHP site's status, a database's sizes each hour or a target's
 * usage each hour (resource backup-usage:<target>), and the steps of a
 * command, which their parent stands for. What is left is work someone
 * waits for, as the types in internal/commands/registry.go have it. The
 * filter is on fields a command never changes, so the list stays in place. */
export const queueFilter = [
  "parent = ''",
  "resource !~ 'backup-usage:%'",
  "type !~ 'logs.%'",
  "type !~ 'exec.%'",
  "type !~ 'acme.challenge.%'",
  "type !~ 'db.adminer.%'",
  "type != 'site.php.status'",
  "type != 'site.php.mail'",
  "type != 'site.detect'",
  "type != 'stack.validate'",
  "type != 'db.stats'",
  "type != 'backup.snapshots'",
  "type != 'backup.browse'",
].join(" && ");

/** The lists the Overview reads besides the servers and the open alerts,
 * which the Layout shares (lib/hosts.tsx, lib/alerts.tsx). */
export type QueryName =
  | "containers"
  | "stacks"
  | "commands"
  | "sites"
  | "sitePhp"
  | "domains"
  | "certificates"
  | "deployments"
  | "jobs"
  | "targets"
  | "runs"
  | "dnsProviders";

/** The collection each list reads. */
export const overviewCollections: Record<QueryName, string> = {
  containers: "containers",
  stacks: "stacks",
  commands: "commands",
  sites: "sites",
  sitePhp: "site_php",
  domains: "domains",
  certificates: "certificates",
  deployments: "deployments",
  jobs: "backup_jobs",
  targets: "backup_targets",
  runs: "backup_runs",
  dnsProviders: "dns_providers",
};

/** A PocketBase date: "2026-10-16 10:00:00.000Z". */
function pbDate(ms: number): string {
  return new Date(ms).toISOString().replace("T", " ");
}

/** The deployments since midnight UTC 7 days ago: a site's newest one that
 * is under way or failed. The filter changes once a day. */
export function deploymentsSince(now: number): string {
  return `${new Date(now - 7 * day).toISOString().slice(0, 10)} 00:00:00.000Z`;
}

/** The runs made since the start of the hour UTC 37 hours ago: the 24 hours
 * the Overview counts, up to 12 more a run may have waited in the queue
 * (internal/panel/backup_jobs.go), and the hour it is rounded to. The
 * filter changes once an hour, which reads the runs again. */
export function runsMadeSince(now: number): string {
  return pbDate(Math.floor(now / hour) * hour - 37 * hour);
}

/**
 * What the Overview reads, each list with the fields it shows only, as the
 * panel's timing test of the page reads it (internal/panel/overview_load_test.go):
 * containers and runs as whole live sets (useLiveRecords, filter and fields
 * only), the rest with useLiveList. The servers and the open alerts are the
 * Layout's (useHosts, useOpenAlerts). The DNS providers are read only while
 * an alert of one is open; the page adds `enabled`.
 */
export function overviewQueries(now: number): Record<QueryName, ListOptions> {
  return {
    containers: { fields: "id,host,state,updated" },
    stacks: {
      sort: "name",
      perPage: 500,
      // never a stack's compose file or env, each up to 1 MB
      fields: "id,collectionId,collectionName,name,host,updated",
    },
    commands: {
      filter: queueFilter,
      sort: "-created",
      perPage: 5,
      // the commands are never pruned: their count would be read with each page
      skipTotal: true,
      // a command's progress replaces it, only a new one reads the five again
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
      // never a chain or the history of attempts
      fields:
        "id,collectionId,collectionName,updated,names,issuer,issuer_name,not_before,not_after," +
        "status,fingerprint,installed,attempts,attempt_started,renew_after,last_error,challenge,panel",
    },
    deployments: {
      filter: pb.filter(
        "(status = 'queued' || status = 'building' || status = 'failed') && created >= {:since}",
        { since: deploymentsSince(now) },
      ),
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
      filter: pb.filter("created >= {:since}", { since: runsMadeSince(now) }),
      fields: overviewRunFields,
    },
    dnsProviders: {
      perPage: 100,
      fields: "id,collectionId,collectionName,updated,name,last_error",
    },
  };
}

/** How many disconnects the Overview reads, newest first, for the servers
 * that are offline. */
export const disconnectsRead = 200;

/** How the servers that went away went: the newest disconnects of their
 * streams, whose data says why when the diagnostics plugin wrote it. Read
 * with getList(1, disconnectsRead) once for all the servers that are
 * offline, not live: a read for each would make the fourth list of events
 * with a filter once four are offline, which PocketBase holds up when it
 * finds nothing (lib/alerts.tsx). A server's newest disconnect is the first
 * of its own; it falls past the page only after another offline server
 * disconnected 200 times since, and its line then names no reason. */
export function disconnectQuery(hosts: string[]) {
  const params = Object.fromEntries(hosts.map((host, index) => [`h${index}`, host]));
  const each = hosts.map((_, index) => `host = {:h${index}}`).join(" || ");
  return {
    filter: pb.filter(`(${each}) && kind = 'connection.disconnected'`, params),
    sort: "-created",
    fields: "id,host,data,created",
    skipTotal: true,
  };
}

/** Each server's reason, from its newest disconnect of those read newest
 * first; none for a server whose newest names none. */
export function disconnectReasons(events: Pick<HostEvent, "host" | "data">[]): Map<string, string> {
  const reasons = new Map<string, string>();
  const newest = new Set<string>();
  for (const event of events) {
    if (newest.has(event.host)) {
      continue;
    }
    newest.add(event.host);
    const reason = event.data?.reason;
    if (typeof reason === "string" && reason) {
      reasons.set(event.host, reason);
    }
  }
  return reasons;
}

/** A line of a tile, in its tone; no tone is the plain note colour. */
export type Note = { text: string; tone?: "err" | "warn" | "info" | "ok" };

type HostLike = Pick<HostSummary, "id" | "name" | "status" | "stream_open" | "last_seen">;

/** The agents' tile: connected as the sidebar counts them, by their stream,
 * and the line: the server offline most recently, those whose agent takes
 * no commands, or that all are connected. */
export function agentSummary(
  hosts: HostLike[],
  now: number,
  t: TFunction,
): { connected: number; total: number; note: Note } {
  const connected = hosts.filter((host) => host.stream_open).length;
  const total = hosts.length;
  if (total === 0) {
    return { connected, total, note: { text: t("overview.noServersTitle") } };
  }
  const offline = hosts
    .filter((host) => host.status === "offline")
    .sort((a, b) => (a.last_seen < b.last_seen ? 1 : a.last_seen > b.last_seen ? -1 : 0));
  if (offline.length > 0) {
    const [latest] = offline;
    const seen = parseDate(latest.last_seen)?.getTime();
    const text =
      offline.length === 1
        ? t("overview.agentsOfflineFor", {
            name: latest.name,
            duration: formatDuration(seen === undefined ? 0 : (now - seen) / 1000),
          })
        : t("overview.agentsOfflineMore", { name: latest.name, count: offline.length - 1 });
    return { connected, total, note: { text, tone: "err" } };
  }
  const unreachable = hosts.filter((host) => !host.stream_open).length;
  if (unreachable > 0) {
    return {
      connected,
      total,
      note: { text: t("overview.agentsUnreachable", { count: unreachable }), tone: "warn" },
    };
  }
  return { connected, total, note: { text: t("overview.allConnected") } };
}

/** The containers' tile, over every container, and each server's count.
 * Stopped is any state but running and restarting. */
export function containerSummary(containers: Pick<Container, "host" | "state">[]): {
  running: number;
  restarting: number;
  stopped: number;
  perHost: Map<string, number>;
} {
  let running = 0;
  let restarting = 0;
  const perHost = new Map<string, number>();
  for (const container of containers) {
    if (container.state === "running") {
      running++;
    } else if (container.state === "restarting") {
      restarting++;
    }
    perHost.set(container.host, (perHost.get(container.host) ?? 0) + 1);
  }
  return { running, restarting, stopped: containers.length - running - restarting, perHost };
}

/** The servers as the design lists them: those that are not offline by
 * name, then those offline by name. */
export function sortHosts<H extends Pick<HostSummary, "name" | "status">>(hosts: H[]): H[] {
  const byName = (a: H, b: H) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0);
  return [
    ...hosts.filter((host) => host.status !== "offline").sort(byName),
    ...hosts.filter((host) => host.status === "offline").sort(byName),
  ];
}

/** What a server is, under its name: an edge proxy or a backup target by
 * its role, else its system. */
export function hostKind(host: Pick<HostSummary, "roles" | "os">, t: TFunction): string {
  const roles = host.roles ?? [];
  if (roles.includes("edge")) {
    return t("host.role.edge");
  }
  if (roles.includes("backup")) {
    return t("host.role.backup");
  }
  return host.os || t("common.unknown");
}

/** A site as the Overview reads it, with its live deployment. */
export type OverviewSite = Pick<
  Site,
  "id" | "name" | "host" | "type" | "source" | "generator" | "active_deployment"
> & {
  expand?: {
    active_deployment?: Pick<Deployment, "id" | "status" | "generator" | "finished_at">;
  };
};

export type SiteInput = {
  sites: OverviewSite[];
  sitePhp: Pick<SitePhp, "site" | "php_version" | "template" | "state">[];
  /** by hostname */
  domains: Pick<Domain, "hostname" | "host" | "site" | "tls">[];
  certificates: Certificate[];
  /** newest first */
  deployments: (Pick<Deployment, "site" | "status" | "finished_at"> & { created?: string })[];
  hosts: Map<string, Pick<HostSummary, "name" | "status">>;
};

/** A site's TLS as its row says it. */
export type SiteTls = {
  kind: DomainTls["kind"] | "noDomain";
  state?: CertState;
  cert?: Certificate;
  text: string;
  tone: Tone;
};

export type SiteRow = {
  id: string;
  /** its first domain by name, or its own name without one */
  domain: string;
  /** its other domains */
  more: number;
  source: string;
  hostName: string;
  online: boolean;
  tls: SiteTls;
  deploy: { text: string; tone?: Tone };
};

const toneRank: Partial<Record<Tone, number>> = { err: 0, warn: 1, info: 2 };

function rank(tone: Tone | undefined): number {
  return (tone && toneRank[tone]) ?? 3;
}

const generatorsShown = ["hugo", "mkdocs", "astro", "npm"];

/** How many whole days a certificate has left. */
function daysLeft(cert: Certificate, now: number): number {
  return validity(cert, now).days;
}

/** The CA of a certificate, as a row names it. */
function caOf(cert: Certificate, t: TFunction): string {
  if (cert.issuer === "internal") {
    return t("cert.issuer.internal");
  }
  return caName(cert.issuer_name) || t(`cert.issuer.${cert.issuer}`);
}

/** Whether a certificate that is due, or whose renewal failed, expires
 * within 14 days: its days left say more then, as the design's
 * "Expires in 12 days" of a certificate whose renewal failed twice. */
function expiresSoon(cert: Certificate, state: CertState, now: number): boolean {
  if (state === "expiring") {
    return true;
  }
  const end = parseDate(cert.not_after)?.getTime();
  return (
    (state === "renewal_failed" || state === "renewal_waiting") &&
    end !== undefined &&
    end - now < expiringWithin
  );
}

function siteTls(
  domain: SiteInput["domains"][number] | undefined,
  certificates: Certificate[],
  now: number,
  t: TFunction,
): SiteTls {
  if (!domain) {
    return { kind: "noDomain", text: t("overview.tlsNoDomain"), tone: "off" };
  }
  const tls = domainTls(certificates, domain, now);
  switch (tls.kind) {
    case "off":
    case "none":
      return { kind: tls.kind, text: t(`edge.tlsShort.${tls.kind}`), tone: "off" };
    case "installing":
      return {
        kind: "installing",
        cert: tls.cert,
        text: t("edge.tlsShort.installing"),
        tone: "info",
      };
  }
  const { cert, state } = tls;
  const shown = { kind: "cert" as const, cert, state, tone: stateTone(state) };
  if (expiresSoon(cert, state, now)) {
    return { ...shown, text: t("overview.tlsExpiring", { count: daysLeft(cert, now) }) };
  }
  switch (state) {
    case "valid":
      return {
        ...shown,
        text: t("overview.tlsValid", { ca: caOf(cert, t), count: daysLeft(cert, now) }),
      };
    case "issuing": {
      const challenge =
        cert.challenge === "dns-01" || cert.challenge === "http-01"
          ? cert.challenge.toUpperCase()
          : "";
      return {
        ...shown,
        text: challenge ? t("overview.tlsIssuing", { challenge }) : t("cert.state.issuing"),
      };
    }
    default:
      return { ...shown, text: t(`cert.state.${state}`) };
  }
}

function phpTone(state: string | undefined): Tone | undefined {
  return state === "failed" ? "err" : state === "pending" ? "info" : undefined;
}

/** The rows of the sites' card: each site by its first domain, what serves
 * it, its TLS as its edge has it, and its deployment; those that need a look
 * first, err, then warn, then info, each by domain. */
export function siteRows(input: SiteInput, now: number, t: TFunction): SiteRow[] {
  const php = new Map(input.sitePhp.map((item) => [item.site, item]));
  const domains = new Map<string, SiteInput["domains"]>();
  for (const domain of input.domains) {
    domains.set(domain.site, [...(domains.get(domain.site) ?? []), domain]);
  }
  const rows = input.sites.map((site): SiteRow => {
    const own = domains.get(site.id) ?? [];
    const settings = php.get(site.id);
    const active = site.expand?.active_deployment;
    const host = input.hosts.get(site.host);

    let source: string;
    if (site.type === "php" && settings) {
      source = t("overview.sitePhp", {
        version: settings.php_version,
        template: t(`phpSite.template.${settings.template}`),
      });
    } else {
      const generator = active?.generator || site.generator;
      source = t("overview.siteStatic", {
        how: generatorsShown.includes(generator)
          ? t(`site.generator.${generator}`)
          : t(`overview.siteSource.${site.source}`),
      });
    }

    const online =
      host?.status !== "offline" &&
      (site.type === "php" ? settings?.state === "running" : Boolean(site.active_deployment));

    return {
      id: site.id,
      domain: own[0]?.hostname ?? site.name,
      more: Math.max(0, own.length - 1),
      source,
      hostName: host?.name ?? "",
      online,
      tls: siteTls(own[0], input.certificates, now, t),
      deploy: deployCell(site, settings, input.deployments, now, t),
    };
  });
  const order = (row: SiteRow) => Math.min(rank(row.tls.tone), rank(row.deploy.tone));
  return rows.sort(
    (a, b) => order(a) - order(b) || (a.domain < b.domain ? -1 : a.domain > b.domain ? 1 : 0),
  );
}

function deployCell(
  site: OverviewSite,
  settings: SiteInput["sitePhp"][number] | undefined,
  deployments: SiteInput["deployments"],
  now: number,
  t: TFunction,
): SiteRow["deploy"] {
  const active = site.expand?.active_deployment;
  const newest = deployments.find((deployment) => deployment.site === site.id);
  const activeEnd = parseDate(active?.finished_at)?.getTime();
  const newestAt = parseDate(newest?.created)?.getTime() ?? 0;
  // one that came after the live one: under way, or failed
  if (newest && (!site.active_deployment || activeEnd === undefined || newestAt > activeEnd)) {
    if (newest.status === "queued" || newest.status === "building") {
      return { text: t("overview.deploying"), tone: "info" };
    }
    if (newest.status === "failed") {
      return {
        text: t("overview.deployFailed", {
          when: formatRelative(newest.finished_at || newest.created, now, { past: true }),
        }),
        tone: "err",
      };
    }
  }
  if (site.active_deployment) {
    return {
      text: t("overview.deployed", {
        when: formatRelative(active?.finished_at, now, { past: true }),
      }),
    };
  }
  if (site.type === "php" && settings?.state) {
    return { text: t(`phpSite.state.${settings.state}`), tone: phpTone(settings.state) };
  }
  return { text: t("overview.notDeployed") };
}

/** The sites' tile: those online, and its line: a certificate that expired
 * or was not issued, one that expires soon or whose renewal failed, each the
 * soonest to end first, then the sites not online, or that all is valid. */
export function sitesSummary(
  rows: SiteRow[],
  now: number,
  t: TFunction,
): { online: number; total: number; unit: string; note: Note } {
  const total = rows.length;
  const online = rows.filter((row) => row.online).length;
  if (total === 0) {
    return {
      online,
      total,
      unit: t("overview.sitesOnline", { count: 0 }),
      note: { text: t("site.none") },
    };
  }
  const unit =
    online === total
      ? t("overview.sitesOnline", { count: online })
      : t("overview.sitesOnlineOf", { count: total });
  const soonest = (a: SiteRow, b: SiteRow) => {
    const left = a.tls.cert?.not_after ?? "";
    const right = b.tls.cert?.not_after ?? "";
    return left < right ? -1 : left > right ? 1 : 0;
  };
  const failing = rows
    .filter((row) => row.tls.state === "expired" || row.tls.state === "failed")
    .sort(soonest);
  if (failing.length > 0) {
    const [row] = failing;
    const key = row.tls.state === "expired" ? "overview.certExpired" : "overview.certFailed";
    return { online, total, unit, note: { text: t(key, { name: row.domain }), tone: "err" } };
  }
  const due = rows
    .filter(
      (row) =>
        row.tls.state === "expiring" ||
        row.tls.state === "renewal_failed" ||
        row.tls.state === "renewal_waiting",
    )
    .sort(soonest);
  if (due.length > 0) {
    const [row] = due;
    const cert = row.tls.cert!;
    const text = expiresSoon(cert, row.tls.state!, now)
      ? t("overview.certExpiresIn", { name: row.domain, count: daysLeft(cert, now) })
      : t("overview.certRenewalFailed", { name: row.domain });
    return { online, total, unit, note: { text, tone: "warn" } };
  }
  if (online < total) {
    return {
      online,
      total,
      unit,
      note: { text: t("overview.sitesNotOnline", { count: total - online }), tone: "warn" },
    };
  }
  return { online, total, unit, note: { text: t("overview.certsAllValid") } };
}

export type OverviewJob = Pick<
  BackupJob,
  | "id"
  | "name"
  | "host"
  | "source_kind"
  | "targets"
  | "origin"
  | "database"
  | "site"
  | "label_removed"
  | "enabled"
>;

export type OverviewRun = Pick<
  BackupRun,
  | "id"
  | "created"
  | "job"
  | "target"
  | "host"
  | "status"
  | "trigger"
  | "scheduled_at"
  | "started_at"
  | "finished_at"
  | "total_bytes"
  | "error"
  | "command"
  | "retry_of"
>;

/** A job's server as its route names it: the panel for the panel's own,
 * whose job's server is only the carrier. */
function jobServer(
  job: Pick<OverviewJob, "host" | "source_kind">,
  hostNames: Map<string, string>,
  t: TFunction,
): string {
  return job.source_kind === "panel" ? t("backups.job.panelHost") : (hostNames.get(job.host) ?? "");
}

/** A job as its alerts and the tile name it: server/job. */
export function jobName(
  job: Pick<OverviewJob, "host" | "source_kind" | "name">,
  hostNames: Map<string, string>,
  t: TFunction,
): string {
  return `${jobServer(job, hostNames, t)}/${job.name}`;
}

/** The runs of the last 24 hours, by when they happened. */
export function runsIn24h<R extends OverviewRun>(runs: R[], now: number): R[] {
  const since = now - day;
  return runs.filter((run) => (parseDate(runMoment(run))?.getTime() ?? 0) >= since);
}

/** The backups' tile: of the runs of the last 24 hours, those that
 * succeeded of all that ended or were skipped, and its line: the newest
 * failure by its job and time, the runs skipped, or that all went fine. */
export function backupSummary(
  jobs: OverviewJob[],
  runs: OverviewRun[],
  hostNames: Map<string, string>,
  now: number,
  t: TFunction,
): { succeeded: number; total: number; note: Note } {
  const counts = successSince(runs, now - day);
  const { succeeded, total } = counts;
  if (total === 0) {
    return { succeeded, total, note: { text: t("overview.backupsNone") } };
  }
  if (counts.failed > 0) {
    const recent = runsIn24h(runs, now);
    // the newest failure that still stands, as the Backups page's banners
    // have it; else the newest the count has, as one a later run followed
    const superseded = new Set(recent.map((run) => run.retry_of ?? "").filter(Boolean));
    const failed =
      failedRuns(jobs, recent).find((run) => run.status === "failed") ??
      recent
        .filter((run) => run.status === "failed" && !superseded.has(run.id))
        .sort((a, b) => (runMoment(a) < runMoment(b) ? 1 : -1))[0];
    if (failed) {
      const job = jobs.find((item) => item.id === failed.job);
      const name = job ? jobName(job, hostNames, t) : (hostNames.get(failed.host) ?? failed.job);
      const moment = parseDate(runMoment(failed))!;
      const today = new Date(now).toDateString() === moment.toDateString();
      const text = today
        ? t(
            czechAt(moment) === "titleVe" ? "overview.backupFailedAtVe" : "overview.backupFailedAt",
            {
              name,
              time: formatHour(moment),
            },
          )
        : t("overview.backupFailedDay", { name, when: formatMoment(runMoment(failed), now) });
      return { succeeded, total, note: { text, tone: "err" } };
    }
  }
  if (counts.skipped > 0) {
    return {
      succeeded,
      total,
      note: { text: t("overview.backupsSkipped", { count: counts.skipped }), tone: "warn" },
    };
  }
  return { succeeded, total, note: { text: t("overview.backupsAllOk") } };
}

/** The tone of a run's dot and result, as the Backups page's. */
export function runTone(status: RunStatus): Tone {
  switch (status) {
    case "queued":
    case "running":
      return "info";
    case "succeeded":
      return "ok";
    case "failed":
      return "err";
    case "skipped":
      return "warn";
    default:
      return "off";
  }
}

export type BackupRow = {
  job: OverviewJob;
  /** the run the row shows: the worst of each target's newest */
  shown: OverviewRun;
  tone: Tone;
  server: string;
  /** the job's targets by name; none is a job that backs up nowhere */
  targets: string[];
  result: string;
  /** restic's line of a failure */
  title?: string;
  when: string;
  link: string;
};

/** The rows of the latest runs: each job that ran in the last 24 hours, by
 * the worst of each target's newest run, failures first, then the newest. */
export function backupRows(
  jobs: OverviewJob[],
  targets: Map<string, string>,
  runs: OverviewRun[],
  hostNames: Map<string, string>,
  now: number,
  t: TFunction,
): BackupRow[] {
  const recent = runsIn24h(runs, now);
  const rows: BackupRow[] = [];
  for (const job of jobs) {
    const shown = lastRun(job, recent);
    if (!shown) {
      continue;
    }
    let result: string;
    if (shown.status === "succeeded") {
      // the size of the newest succeeded run to each target, the largest
      const newest = new Map<string, OverviewRun>();
      for (const run of recent) {
        if (run.job === job.id && run.status === "succeeded") {
          const seen = newest.get(run.target);
          if (!seen || run.created > seen.created) {
            newest.set(run.target, run);
          }
        }
      }
      const size = Math.max(0, ...[...newest.values()].map((run) => run.total_bytes || 0));
      result = t("overview.runOk", { size: formatBytes(size) });
    } else {
      result = t(`backups.runStatus.${shown.status}`);
    }
    rows.push({
      job,
      shown,
      tone: runTone(shown.status),
      server: jobServer(job, hostNames, t),
      targets: (job.targets ?? []).map((id) => targets.get(id) ?? id),
      result,
      title: shown.status === "failed" && shown.error ? resticLine(shown.error) : undefined,
      when: formatMoment(runMoment(shown), now),
      link: `/backups?${new URLSearchParams({ job: job.id, target: shown.target }).toString()}`,
    });
  }
  return rows.sort((a, b) => {
    const failed = Number(b.shown.status === "failed") - Number(a.shown.status === "failed");
    if (failed !== 0) {
      return failed;
    }
    const left = runMoment(a.shown);
    const right = runMoment(b.shown);
    return left < right ? 1 : left > right ? -1 : 0;
  });
}

export type AttentionContext = {
  hosts: Map<string, Pick<HostSummary, "name" | "last_seen">>;
  jobs: Map<string, OverviewJob>;
  targets: Map<string, string>;
  certificates: Map<string, Certificate>;
  providers: Map<string, Pick<DnsProvider, "name" | "last_error">>;
  runs: OverviewRun[];
  /** why a server's stream was last closed, by server */
  reasons: Map<string, string>;
  /** an operator may open the audit log */
  canAudit: boolean;
};

export type AttentionItem = {
  id: string;
  tone: "err" | "warn" | "info";
  title: string;
  detail: string;
  link: string | null;
};

const severityRank: Record<Alert["severity"], number> = { critical: 0, warning: 1, info: 2 };

/** What an alert's title names: its server, job, repository, certificate or
 * DNS provider, when the page holds that record; its server or stored target
 * otherwise. */
function subjectOf(
  alert: Alert,
  ctx: AttentionContext,
  now: number,
  t: TFunction,
): AlertSubject | undefined {
  const hostName = ctx.hosts.get(alert.host)?.name;
  const fallback = hostName ? { name: hostName } : undefined;
  switch (alert.rule) {
    case "backup_failed":
    case "backup_skipped":
    case "backup_retention": {
      const job = ctx.jobs.get(alert.target.split(":")[0]);
      const hostNames = new Map([...ctx.hosts].map(([id, host]) => [id, host.name]));
      return job ? { name: jobName(job, hostNames, t) } : fallback;
    }
    case "backup_check_failed":
    case "backup_check_unverified": {
      const target = ctx.targets.get(alert.target);
      return target ? { name: target } : fallback;
    }
    case "backup_restore_test_failed":
    case "backup_restore_test_unverified": {
      const job = ctx.jobs.get(alert.target);
      return job ? { name: job.name } : fallback;
    }
    case "certificate_expiring":
    case "certificate_failed": {
      const cert = ctx.certificates.get(alert.target);
      if (!cert) {
        return fallback;
      }
      const name = cert.names?.[0] ?? alert.target;
      if (alert.rule === "certificate_failed") {
        return { name };
      }
      const end = parseDate(cert.not_after)?.getTime();
      if (end !== undefined && end <= now) {
        return { name, rule: "certificate_expired" };
      }
      return { name, count: end === undefined ? undefined : daysLeft(cert, now) };
    }
    case "dns_provider_failed": {
      const provider = ctx.providers.get(alert.target);
      return provider ? { name: provider.name } : fallback;
    }
  }
  return fallback;
}

/** The line under an alert's title: what the page knows of it, else since
 * when it is open. */
function detailOf(alert: Alert, ctx: AttentionContext, now: number, t: TFunction): string {
  const since = t("overview.since", {
    when: formatRelative(alert.first_seen, now, { past: true }),
  });
  switch (alert.rule) {
    case "agent_offline": {
      const host = ctx.hosts.get(alert.host);
      if (!host?.last_seen) {
        return since;
      }
      const line = t("overview.lastHeartbeat", { time: formatMoment(host.last_seen, now) });
      const reason = ctx.reasons.get(alert.host);
      return reason ? `${line} · ${t(`diag.reason.${reason}`, { defaultValue: reason })}` : line;
    }
    case "backup_failed":
    case "backup_skipped": {
      const [job, target = ""] = alert.target.split(":");
      const status = alert.rule === "backup_failed" ? "failed" : "skipped";
      const newest = ctx.runs
        .filter((run) => run.job === job && run.target === target && run.status === status)
        .sort((a, b) => (a.created < b.created ? 1 : a.created > b.created ? -1 : 0))[0];
      const line = newest?.error ? resticLine(newest.error) : "";
      return line || since;
    }
    case "certificate_expiring":
    case "certificate_failed": {
      const cert = ctx.certificates.get(alert.target);
      if (!cert) {
        return since;
      }
      if (cert.attempts > 0 && cert.last_error) {
        return t("overview.autoRenewFailed", {
          times: cert.attempts,
          error: shortError(cert.last_error),
        });
      }
      if (certificateState(cert, now) === "issuing") {
        const challenge =
          cert.challenge === "dns-01" || cert.challenge === "http-01"
            ? cert.challenge.toUpperCase()
            : "";
        return challenge ? t("overview.tlsIssuing", { challenge }) : t("cert.state.issuing");
      }
      return since;
    }
    case "dns_provider_failed": {
      const error = ctx.providers.get(alert.target)?.last_error;
      return error ? shortError(error) : since;
    }
  }
  return since;
}

/** Needs attention: every open alert, critical first, then warnings, then
 * notes, each the newest first; each named by its subject, with what the
 * page knows of it, and a link to where it is fixed. The audit log's
 * integrity links to the log for those who may read it. */
export function attentionItems(
  alerts: Alert[],
  ctx: AttentionContext,
  now: number,
  t: TFunction,
): AttentionItem[] {
  return [...alerts]
    .sort(
      (a, b) =>
        (severityRank[a.severity] ?? 3) - (severityRank[b.severity] ?? 3) ||
        (a.last_seen < b.last_seen ? 1 : a.last_seen > b.last_seen ? -1 : 0),
    )
    .map((alert) => ({
      id: alert.id,
      tone: alert.severity === "critical" ? "err" : alert.severity === "warning" ? "warn" : "info",
      title: alertText(t, alert, subjectOf(alert, ctx, now, t)),
      detail: detailOf(alert, ctx, now, t),
      link: alert.rule === "audit_integrity" ? (ctx.canAudit ? "/audit" : null) : alertLink(alert),
    }));
}

export type OverviewCommand = Pick<
  Command,
  "id" | "host" | "host_name" | "type" | "label" | "status" | "resource"
>;

/** What a command acts on, under its type: a deploy as label → server,
 * anything else as label @ server, or whichever of the two it has. */
export function commandTarget(
  command: Pick<OverviewCommand, "type" | "label" | "host_name">,
): string {
  const { label, host_name: host } = command;
  if (label && host) {
    return command.type.endsWith(".deploy") ? `${label} → ${host}` : `${label} @ ${host}`;
  }
  return label || host || "";
}

export type CommandContext = {
  stacks: Pick<Stack, "id" | "name" | "host">[];
  runs: Pick<OverviewRun, "command" | "job" | "target">[];
};

/** Where a command's object is, by the resource it holds: its site, its
 * container, its stack (a name is a stack's only on its server), its job's
 * runs to a target or a target's checks, the databases, the server's edge
 * or agent, or its server. Each is a page a viewer may open. */
export function commandLink(command: OverviewCommand, ctx: CommandContext): string | null {
  const resource = command.resource ?? "";
  const host = command.host;
  const [kind, ...rest] = resource.split(":");
  const key = rest.join(":");
  switch (kind) {
    case "site":
      if (key) {
        return `/sites/${key}`;
      }
      break;
    case "container":
      if (key && host) {
        return `/servers/${host}/containers/${key}`;
      }
      break;
    case "stack": {
      const stack = ctx.stacks.find((item) => item.host === host && item.name === key);
      if (stack) {
        return `/stacks/${stack.id}`;
      }
      break;
    }
    case "backup": {
      const run = ctx.runs.find((item) => item.command === command.id);
      if (run) {
        return `/backups?${new URLSearchParams({ job: run.job, target: run.target }).toString()}`;
      }
      return "/backups";
    }
    case "backup-integrity":
      return key ? `/backups?check=${encodeURIComponent(key)}` : "/backups";
    case "db":
    case "db-test":
      return "/databases";
    case "edge":
      if (host) {
        return `/servers/${host}/proxy`;
      }
      break;
    case "agent":
      if (host) {
        return `/servers/${host}/agent`;
      }
      break;
  }
  if (command.type.startsWith("db.")) {
    return "/databases";
  }
  return host ? `/servers/${host}` : null;
}
