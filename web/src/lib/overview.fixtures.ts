// The Overview's design as records (#33): seven servers, rpi-garaz offline
// for 14 minutes, 42 containers, 11 stacks, four open alerts, the command
// queue, five sites with their domains, certificates and deployments, and
// five backup jobs whose runs give 11 of 12 in the last 24 hours, nas/photos
// failed at 03:00. Only tests import it.
import type { BackupJob, BackupRun } from "./backupJobs";
import type { BackupTarget } from "./backups";
import type { DnsProvider } from "./dns";
import type { Certificate, Domain } from "./edge";
import type { HostSummary } from "./hosts";
import type { SitePhp } from "./php";
import type { Deployment, Site } from "./sites";
import type { Alert, Command, Container, Stack } from "./types";

// Friday 16 October 2026, 12:24 in Prague (summer time, two hours ahead of UTC)
export const now = Date.parse("2026-10-16T10:24:00Z");
const minute = 60_000;
const hour = 60 * minute;
const day = 24 * hour;

/** A PocketBase date. */
export const pbDate = (ms: number) => new Date(ms).toISOString().replace("T", " ");

function rec<T>(collection: string, id: string, fields: Record<string, unknown>): T {
  return {
    id,
    collectionId: collection,
    collectionName: collection,
    created: pbDate(now - 30 * day),
    updated: pbDate(now - day),
    ...fields,
  } as T;
}

const gb = 1e9;

export function host(id: string, fields: Partial<HostSummary>): HostSummary {
  return rec<HostSummary>("hosts", id, {
    name: id,
    roles: ["docker"],
    status: "online",
    stream_open: true,
    ip4: "",
    public_ip4: "",
    os: "",
    cpu_percent: 10,
    mem_used_bytes: 1 * gb,
    mem_bytes: 10 * gb,
    disk_used_bytes: 10 * gb,
    disk_total_bytes: 100 * gb,
    rtt_ms: 2,
    last_seen: pbDate(now - 4000),
    agent_version: "0.4.2",
    ...fields,
  });
}

/** A server's usage in per cent, as mem and disk bytes of 100 GB. */
const usage = (cpu: number, mem: number, disk: number) => ({
  cpu_percent: cpu,
  mem_used_bytes: mem * gb,
  mem_bytes: 100 * gb,
  disk_used_bytes: disk * gb,
  disk_total_bytes: 100 * gb,
});

export const hosts: HostSummary[] = [
  host("h1", {
    name: "vps-hetzner",
    roles: ["docker", "edge"],
    public_ip4: "203.0.113.24",
    os: "Debian GNU/Linux 12",
    rtt_ms: 28,
    ...usage(22, 44, 37),
  }),
  host("h2", {
    name: "pve-01",
    ip4: "10.0.0.11",
    os: "Proxmox VE 8.2",
    rtt_ms: 3,
    ...usage(34, 61, 48),
  }),
  host("h3", {
    name: "nas",
    ip4: "10.0.0.20",
    os: "TrueNAS SCALE",
    rtt_ms: 2,
    ...usage(12, 72, 81),
  }),
  host("h4", {
    name: "vps-wedos",
    public_ip4: "198.51.100.7",
    os: "Debian GNU/Linux 12",
    rtt_ms: 19,
    agent_version: "0.4.1",
    ...usage(93, 68, 55),
  }),
  host("h5", {
    name: "backup-node",
    roles: ["docker", "backup"],
    ip4: "10.0.0.30",
    os: "Debian GNU/Linux 12",
    ...usage(6, 23, 64),
  }),
  host("h6", {
    name: "dev-box",
    ip4: "10.0.0.15",
    os: "Ubuntu 24.04",
    rtt_ms: 4,
    ...usage(18, 39, 29),
  }),
  host("h7", {
    name: "rpi-garaz",
    status: "offline",
    stream_open: false,
    ip4: "10.0.0.42",
    os: "Raspberry Pi OS",
    agent_version: "0.3.9",
    rtt_ms: 0,
    last_seen: pbDate(now - 14 * minute),
    ...usage(0, 0, 0),
  }),
];

/** 42 containers: 38 running, 3 stopped and 1 restarting. */
export const containers: Container[] = (
  [
    ["h1", 8],
    ["h2", 14],
    ["h3", 9],
    ["h4", 6],
    ["h5", 2],
    ["h6", 3],
  ] as const
).flatMap(([hostId, count]) =>
  Array.from({ length: count }, (_, index) => {
    let state = "running";
    if (hostId === "h2" && index < 3) {
      state = "exited";
    } else if (hostId === "h3" && index === 0) {
      state = "restarting";
    }
    return rec<Container>("containers", `${hostId}-c${index}`, { host: hostId, state });
  }),
);

export const stacks: Stack[] = [
  ["k1", "immich", "h3"],
  ["k2", "gitea", "h2"],
  ["k3", "vaultwarden", "h1"],
  ["k4", "umami", "h1"],
  ["k5", "n8n", "h4"],
  ["k6", "grafana", "h2"],
  ["k7", "jellyfin", "h3"],
  ["k8", "paperless", "h2"],
  ["k9", "uptime", "h6"],
  ["k10", "minio", "h5"],
  ["k11", "immich", "h2"],
].map(([id, name, hostId]) => rec<Stack>("stacks", id, { name, host: hostId }));

export const alerts: Alert[] = [
  rec<Alert>("alerts", "a1", {
    rule: "agent_offline",
    target: "h7",
    host: "h7",
    severity: "critical",
    status: "open",
    first_seen: pbDate(now - 13 * minute),
    last_seen: pbDate(now - minute),
    message: "rpi-garaz stopped responding",
  }),
  rec<Alert>("alerts", "a2", {
    rule: "backup_failed",
    target: "j3:t3",
    host: "h3",
    severity: "critical",
    status: "open",
    first_seen: pbDate(Date.parse("2026-10-16T01:00:50Z")),
    last_seen: pbDate(Date.parse("2026-10-16T01:00:50Z")),
    message: "The backup of photos to backup-node failed.",
  }),
  rec<Alert>("alerts", "a3", {
    rule: "certificate_expiring",
    target: "c3",
    host: "",
    severity: "warning",
    status: "open",
    first_seen: pbDate(now - 2 * day),
    last_seen: pbDate(now - 10 * minute),
    message: "The certificate for shop.example.org expires in 12 days.",
  }),
  rec<Alert>("alerts", "a4", {
    rule: "dns_provider_failed",
    target: "p1",
    host: "",
    severity: "warning",
    status: "open",
    first_seen: pbDate(now - 3 * hour),
    last_seen: pbDate(now - 20 * minute),
    message: "The DNS provider WEDOS failed.",
  }),
];

export const providers: DnsProvider[] = [
  rec<DnsProvider>("dns_providers", "p1", {
    name: "WEDOS",
    last_error: "WAPI answered 2051: access not allowed from this IP\nretry later",
  }),
];

export function command(id: string, fields: Partial<Command>): Command {
  return rec<Command>("commands", id, {
    host: "",
    host_name: "",
    type: "",
    label: "",
    status: "succeeded",
    resource: "",
    parent: "",
    ...fields,
  });
}

/** The queue: newest first, as the panel sends it, with what it leaves out. */
export const commands: Command[] = [
  // reads, measures and steps, newer than the queue's five
  command("x1", {
    type: "db.stats",
    host: "h2",
    host_name: "pve-01",
    created: pbDate(now - 10_000),
  }),
  command("x2", {
    type: "backup.init",
    resource: "backup-usage:t1",
    host: "h5",
    host_name: "backup-node",
    created: pbDate(now - 20_000),
  }),
  command("x3", { type: "site.dns", parent: "x9", created: pbDate(now - 30_000) }),
  command("x4", {
    type: "logs.follow",
    host: "h1",
    host_name: "vps-hetzner",
    created: pbDate(now - 40_000),
  }),
  command("cmd1", {
    type: "site.deploy",
    label: "docs.example.cz",
    host: "h1",
    host_name: "vps-hetzner",
    status: "dispatched",
    resource: "site:s2",
    created: pbDate(now - minute),
  }),
  command("cmd2", {
    type: "edge.apply",
    host: "h1",
    host_name: "vps-hetzner",
    status: "queued",
    resource: "edge",
    created: pbDate(now - 2 * minute),
  }),
  command("cmd3", {
    type: "stack.restart",
    label: "immich",
    host: "h3",
    host_name: "nas",
    resource: "stack:immich",
    created: pbDate(now - 6 * minute),
  }),
  command("cmd4", {
    type: "image.pull",
    label: "gitea",
    host: "h2",
    host_name: "pve-01",
    created: pbDate(now - 19 * minute),
  }),
  command("cmd5", {
    type: "backup.run",
    label: "photos",
    host: "h3",
    host_name: "nas",
    status: "failed",
    resource: "backup:t3",
    created: pbDate(Date.parse("2026-10-16T01:00:00Z")),
  }),
];

export function site(id: string, fields: Partial<Site>): Site {
  return rec<Site>("sites", id, {
    name: id,
    host: "h1",
    type: "static",
    source: "git",
    generator: "auto",
    active_deployment: "",
    ...fields,
  });
}

const active = (id: string, generator: string, finished: number) => ({
  active_deployment: id,
  expand: {
    active_deployment: { id, status: "succeeded", generator, finished_at: pbDate(finished) },
  },
});

export const sites: Site[] = [
  site("s1", { name: "blog", generator: "hugo", ...active("d1", "hugo", now - 2 * minute) }),
  site("s2", { name: "docs", generator: "mkdocs", ...active("d2", "mkdocs", now - 3 * day) }),
  site("s3", { name: "shop", type: "php", host: "h4", ...active("d3", "", now - 6 * day) }),
  site("s4", { name: "status", generator: "astro", ...active("d4", "astro", now - 26 * hour) }),
  site("s5", {
    name: "wiki",
    host: "h2",
    source: "upload",
    generator: "none",
    ...active("d5", "", now - 21 * day),
  }),
];

export const sitePhp: SitePhp[] = [
  rec<SitePhp>("site_php", "p3", {
    site: "s3",
    php_version: "8.3",
    template: "wordpress",
    state: "running",
  }),
];

export function domain(id: string, fields: Partial<Domain>): Domain {
  return rec<Domain>("domains", id, { hostname: id, host: "h1", site: "", tls: "auto", ...fields });
}

/** By hostname, as the page reads them. */
export const domains: Domain[] = [
  domain("o1", { hostname: "blog.example.cz", site: "s1" }),
  domain("o2", { hostname: "docs.example.cz", site: "s2" }),
  domain("o3", { hostname: "shop.example.org", site: "s3", host: "h4" }),
  domain("o4", { hostname: "status.example.org", site: "s4" }),
  domain("o5", { hostname: "wiki.lan", site: "s5", host: "h2", tls: "internal" }),
  domain("o6", { hostname: "www.shop.example.org", site: "s3", host: "h4" }),
];

export function certificate(id: string, fields: Partial<Certificate>): Certificate {
  return rec<Certificate>("certificates", id, {
    names: [],
    issuer: "acme",
    issuer_name: "CN=R11,O=Let's Encrypt,C=US",
    not_before: pbDate(now - 30 * day),
    not_after: pbDate(now + 60 * day),
    status: "active",
    fingerprint: `fp-${id}`,
    installed: { h1: `${id}-file` },
    attempts: 0,
    attempt_started: "",
    renew_after: pbDate(now + 30 * day),
    last_error: "",
    challenge: "",
    panel: false,
    ...fields,
  });
}

export const certificates: Certificate[] = [
  certificate("c1", { names: ["blog.example.cz"], not_after: pbDate(now + 89 * day + 12 * hour) }),
  // the order for docs, being issued over DNS-01
  certificate("c2", {
    names: ["docs.example.cz"],
    fingerprint: "",
    status: "pending",
    installed: null,
    not_before: "",
    not_after: "",
    challenge: "dns-01",
    attempt_started: pbDate(now - minute),
  }),
  // 12 days left, and its renewal failed twice
  certificate("c3", {
    names: ["shop.example.org", "www.shop.example.org"],
    not_before: pbDate(now - 77 * day),
    not_after: pbDate(now + 12 * day + 12 * hour),
    renew_after: pbDate(now - 18 * day),
    installed: { h4: "c3-file" },
    attempts: 2,
    last_error: "DNS-01 timeout (WEDOS API)\npresenting the record took too long",
  }),
  certificate("c4", {
    names: ["status.example.org"],
    issuer_name: "CN=ZeroSSL ECC Domain Secure Site CA,O=ZeroSSL,C=AT",
    not_after: pbDate(now + 41 * day + 12 * hour),
  }),
  certificate("c5", {
    names: ["wiki.lan"],
    issuer: "internal",
    issuer_name: "CN=PocketCloud internal CA",
    not_after: pbDate(now + 301 * day + 12 * hour),
    installed: { h2: "c5-file" },
  }),
];

/** Under way or failed, newest first: docs is being deployed again. */
export const deployments: Deployment[] = [
  rec<Deployment>("deployments", "d2b", {
    site: "s2",
    status: "building",
    created: pbDate(now - minute),
    finished_at: "",
  }),
];

export const targets: BackupTarget[] = [
  rec<BackupTarget>("backup_targets", "t1", { name: "Backblaze B2" }),
  rec<BackupTarget>("backup_targets", "t3", { name: "backup-node" }),
];

export function job(id: string, fields: Partial<BackupJob>): BackupJob {
  return rec<BackupJob>("backup_jobs", id, {
    name: id,
    host: "h1",
    source_kind: "volume",
    targets: ["t1"],
    origin: "manual",
    database: "",
    site: "",
    label_removed: false,
    enabled: true,
    ...fields,
  });
}

export const jobs: BackupJob[] = [
  job("j1", { name: "vaultwarden", targets: ["t1", "t3"] }),
  job("j2", { name: "immich-library", host: "h3", source_kind: "path" }),
  job("j3", { name: "photos", host: "h3", source_kind: "path", targets: ["t3"] }),
  job("j4", { name: "umami-db", source_kind: "db_dump", targets: ["t3"] }),
  job("j5", { name: "panel (pb_data)", host: "h5", source_kind: "panel" }),
];

let runs = 0;
export function run(
  jobId: string,
  target: string,
  at: string,
  status: BackupRun["status"],
  fields: Partial<BackupRun> = {},
): BackupRun {
  const start = Date.parse(at);
  return rec<BackupRun>("backup_runs", `r${++runs}`, {
    created: pbDate(start),
    updated: pbDate(start + minute),
    job: jobId,
    target,
    host: "h1",
    status,
    trigger: "schedule",
    scheduled_at: pbDate(start),
    started_at: pbDate(start + 1000),
    finished_at: status === "running" || status === "queued" ? "" : pbDate(start + 50_000),
    total_bytes: 0,
    error: "",
    command: "",
    retry_of: "",
    ...fields,
  });
}

/** 12 runs in the last 24 hours, 11 of them OK; one before. */
export const backupRuns: BackupRun[] = [
  run("j1", "t1", "2026-10-16T10:00:00Z", "succeeded", { total_bytes: 84e6 }),
  run("j1", "t3", "2026-10-16T10:00:00Z", "succeeded", { total_bytes: 80e6 }),
  run("j1", "t1", "2026-10-15T22:00:00Z", "succeeded", { total_bytes: 83e6 }),
  run("j1", "t3", "2026-10-15T22:00:00Z", "succeeded", { total_bytes: 79e6 }),
  run("j2", "t1", "2026-10-16T00:14:00Z", "succeeded", { host: "h3", total_bytes: 2.1e9 }),
  run("j2", "t1", "2026-10-15T12:14:00Z", "succeeded", { host: "h3", total_bytes: 2.08e9 }),
  run("j3", "t3", "2026-10-16T01:00:00Z", "failed", {
    host: "h3",
    command: "cmd5",
    error: "Fatal: unable to create lock in backend: repository is already locked\nby PID 4242",
  }),
  run("j3", "t3", "2026-10-15T13:00:00Z", "succeeded", { host: "h3", total_bytes: 41e9 }),
  run("j4", "t3", "2026-10-15T23:30:00Z", "succeeded", { total_bytes: 46e6 }),
  run("j4", "t3", "2026-10-15T11:30:00Z", "succeeded", { total_bytes: 45e6 }),
  run("j5", "t1", "2026-10-16T02:00:00Z", "succeeded", { host: "h5", total_bytes: 312e6 }),
  run("j5", "t1", "2026-10-15T14:00:00Z", "succeeded", { host: "h5", total_bytes: 311e6 }),
  // before the 24 hours
  run("j1", "t1", "2026-10-15T09:00:00Z", "failed", { error: "an older failure" }),
];

/** Each list the page reads, by collection. */
export const lists: Record<string, unknown[]> = {
  containers,
  stacks,
  alerts,
  commands,
  sites,
  site_php: sitePhp,
  domains,
  certificates,
  deployments,
  backup_jobs: jobs,
  backup_targets: targets,
  backup_runs: backupRuns,
  dns_providers: providers,
};
