import { AlertTriangle, Bell, Info, Lock, LockOpen, Plus, Server } from "lucide-react";
import {
  memo,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type MouseEvent,
  type ReactNode,
} from "react";
import { useTranslation } from "react-i18next";
import { Link, useNavigate } from "react-router-dom";

import { PageHead } from "../components/Layout";
import { CommandStatusPill, HostStatusPill } from "../components/status";
import {
  Banner,
  Button,
  Card,
  EmptyState,
  IconButton,
  Kpi,
  Table,
  type Tone,
} from "../components/ui";
import { errorMessage } from "../lib/api";
import { hasRole, useAuth } from "../lib/auth";
import { useOpenAlerts } from "../lib/alerts";
import { formatMoment, type BackupJob, type BackupRun } from "../lib/backupJobs";
import type { BackupTarget } from "../lib/backups";
import type { DnsProvider } from "../lib/dns";
import type { Certificate, Domain } from "../lib/edge";
import {
  formatHour,
  formatNumber,
  formatRelative,
  formatToday,
  formatVersion,
  percentOf,
} from "../lib/format";
import { HostUsage, useHosts, type HostSummary } from "../lib/hosts";
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
  overviewQueries,
  siteRows,
  sitesSummary,
  sortHosts,
  type AttentionItem,
  type Note,
  type OverviewSite,
  type SiteRow,
} from "../lib/overview";
import { pb } from "../lib/pb";
import type { SitePhp } from "../lib/php";
import { useLiveList, useLiveRecords, useNow, type ListState } from "../lib/realtime";
import type { Deployment, Site } from "../lib/sites";
import type { Alert, Command, Container, HostEvent, Stack } from "../lib/types";

/** Needs attention shows this many, then all on request. */
const attentionShown = 6;
const commandsShown = 5;
const sitesShown = 8;
const backupsShown = 5;

type AnyList = Pick<ListState<never>, "loading" | "error" | "reload">;

/** A section of lists: loading until each has loaded once, failed when any
 * did, and its retry reads again those that failed. */
function sectionOf(...lists: AnyList[]) {
  const failed = lists.filter((list) => list.error);
  return {
    loading: lists.some((list) => list.loading),
    error: failed[0]?.error ?? null,
    retry: () => {
      for (const list of failed) {
        list.reload();
      }
    },
  };
}

type Section = ReturnType<typeof sectionOf>;

function LoadError({ section }: { section: Section }) {
  const { t } = useTranslation();
  return (
    <div className="card-pad">
      <Banner tone="err">
        <span>{t("overview.loadFailed", { error: errorMessage(section.error) })}</span>
        <Button size="small" className="banner-action" onClick={section.retry}>
          {t("overview.retry")}
        </Button>
      </Banner>
    </div>
  );
}

function Loading() {
  const { t } = useTranslation();
  return <p className="card-pad muted">{t("common.loading")}</p>;
}

function NoteText({ note }: { note: Note }) {
  return note.tone ? <span className={`${note.tone}-text`}>{note.text}</span> : note.text;
}

/** A tile's figures while its lists load, or when they failed. */
function tileState(section: Section, t: ReturnType<typeof useTranslation>["t"]) {
  if (section.error) {
    return {
      value: "–",
      note: (
        <span className="err-text">
          {t("overview.loadFailed", { error: errorMessage(section.error) })}
        </span>
      ),
    };
  }
  if (section.loading) {
    return { value: "–", note: t("common.loading") };
  }
  return null;
}

/** A row goes to its page on a click anywhere but on a link or a button,
 * which do their own. */
function rowClick(go: () => void) {
  return (event: MouseEvent<HTMLTableRowElement>) => {
    if ((event.target as HTMLElement).closest("a, button")) {
      return;
    }
    go();
  };
}

function Usage({ value, label }: { value: number; label: string }) {
  return (
    <div className="usage">
      <HostUsage value={value} label={label} track="ramp" />
    </div>
  );
}

/** A server that is offline has no figures: a dash on an empty track. */
function NoUsage() {
  return (
    <div className="usage">
      <span className="num faint">—</span>
      <div className="progress empty" aria-hidden="true" />
    </div>
  );
}

/** A server's row, rendered again only when its server, its count or the
 * minute changes: a heartbeat of one server renders one row. The count is
 * null while the containers load, or when they could not be read. */
const HostRow = memo(function HostRow({
  host,
  containers,
  now,
}: {
  host: HostSummary;
  containers: number | null;
  now: number;
}) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const offline = host.status === "offline";
  const address = host.ip4 || host.public_ip4;
  const kind = hostKind(host, t);

  return (
    <tr className="clickable" onClick={rowClick(() => navigate(`/servers/${host.id}`))}>
      <td className="cell-main">
        <Link to={`/servers/${host.id}`} className="cell-name">
          {host.name}
        </Link>
        <div className="cell-sub mono ellipsis">{address ? `${address} · ${kind}` : kind}</div>
      </td>
      <td>
        <HostStatusPill status={host.status} />
      </td>
      {offline ? (
        <>
          <td>
            <NoUsage />
          </td>
          <td>
            <NoUsage />
          </td>
          <td>
            <NoUsage />
          </td>
        </>
      ) : (
        <>
          <td>
            <Usage value={host.cpu_percent} label={t("host.cpuOf", { name: host.name })} />
          </td>
          <td>
            <Usage
              value={percentOf(host.mem_used_bytes, host.mem_bytes)}
              label={t("host.memOf", { name: host.name })}
            />
          </td>
          <td>
            <Usage
              value={percentOf(host.disk_used_bytes, host.disk_total_bytes)}
              label={t("host.diskOf", { name: host.name })}
            />
          </td>
        </>
      )}
      {/* an offline server's count is the last it reported */}
      <td className={`num${offline || containers === null ? " faint" : ""}`}>
        {containers ?? "–"}
      </td>
      <td className="num" style={{ textAlign: "right" }}>
        <div>{formatVersion(host.agent_version)}</div>
        <div className="cell-sub">
          {offline
            ? formatRelative(host.last_seen, now, { past: true })
            : host.rtt_ms
              ? `${formatNumber(host.rtt_ms)} ms`
              : "–"}
        </div>
      </td>
    </tr>
  );
});

function ServersCard({
  hosts,
  perHost,
  now,
  canConnect,
}: {
  hosts: ListState<HostSummary>;
  /** each server's containers; null while they load, or when they could
   * not be read, as the containers' tile then says */
  perHost: Map<string, number> | null;
  now: number;
  canConnect: boolean;
}) {
  const { t } = useTranslation();
  const sorted = useMemo(() => sortHosts(hosts.items), [hosts.items]);
  const section = sectionOf(hosts);
  const loading = hosts.loading && hosts.items.length === 0;

  let body: ReactNode;
  if (hosts.error && hosts.items.length === 0) {
    body = <LoadError section={section} />;
  } else if (!loading && hosts.items.length === 0) {
    body = (
      <EmptyState
        icon={Server}
        title={t("overview.noServersTitle")}
        action={
          canConnect && (
            <Link to="/agents" className="button primary">
              {t("overview.addServer")}
            </Link>
          )
        }
      >
        {t("overview.noServersText")}
      </EmptyState>
    );
  } else {
    body = (
      <>
        {hosts.error && <LoadError section={section} />}
        <Table label={t("overview.servers")}>
          <thead>
            <tr>
              <th>{t("host.server")}</th>
              <th>{t("host.state")}</th>
              <th>CPU</th>
              <th>RAM</th>
              <th>{t("host.disk")}</th>
              <th>{t("host.containersShort")}</th>
              <th style={{ textAlign: "right" }}>{t("host.agentRtt")}</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr>
                <td colSpan={7} className="muted">
                  {t("common.loading")}
                </td>
              </tr>
            ) : (
              sorted.map((host) => (
                <HostRow
                  key={host.id}
                  host={host}
                  containers={perHost ? (perHost.get(host.id) ?? 0) : null}
                  now={now}
                />
              ))
            )}
          </tbody>
        </Table>
      </>
    );
  }

  return (
    <Card
      title={t("overview.servers")}
      note={t("overview.serversNote")}
      flush
      side={canConnect && <Link to="/agents">{t("overview.connectMore")}</Link>}
    >
      {body}
    </Card>
  );
}

function AttentionCard({ alerts, items }: { alerts: ListState<Alert>; items: AttentionItem[] }) {
  const { t } = useTranslation();
  const [all, setAll] = useState(false);
  const section = sectionOf(alerts);
  const shown = all ? items : items.slice(0, attentionShown);

  let body: ReactNode;
  if (alerts.error) {
    body = <LoadError section={section} />;
  } else if (alerts.loading) {
    body = <Loading />;
  } else if (items.length === 0) {
    body = <EmptyState title={t("overview.noAlerts")} />;
  } else {
    body = (
      <>
        <ul className="list">
          {shown.map((item) => {
            const content = (
              <>
                <span className={`notice-mark ${item.tone}`} aria-hidden="true">
                  {item.tone === "info" ? <Info size={14} /> : <AlertTriangle size={14} />}
                </span>
                <span className="list-text">
                  <span className="list-title">{item.title}</span>
                  <span className="cell-sub">{item.detail}</span>
                </span>
              </>
            );
            return (
              <li key={item.id} className="link-item">
                {item.link ? (
                  <Link className="list-link" to={item.link}>
                    {content}
                  </Link>
                ) : (
                  <div className="list-link">{content}</div>
                )}
              </li>
            );
          })}
        </ul>
        {items.length > attentionShown && (
          <div className="card-foot">
            <Button size="small" aria-expanded={all} onClick={() => setAll(!all)}>
              {all ? t("overview.showLess") : t("overview.showAll", { total: items.length })}
            </Button>
          </div>
        )}
      </>
    );
  }

  return (
    <Card
      id="attention"
      // the bell's focus when the first alert has no link
      title={
        <span id="attention-heading" tabIndex={-1}>
          {t("overview.attention")}
        </span>
      }
      side={!alerts.loading && !alerts.error && t("overview.openCount", { count: alerts.total })}
      flush
    >
      {body}
    </Card>
  );
}

function QueueCard({
  commands,
  links,
  now,
  canAudit,
}: {
  commands: ListState<Command>;
  links: Map<string, string | null>;
  now: number;
  canAudit: boolean;
}) {
  const { t } = useTranslation();
  const section = sectionOf(commands);

  let body: ReactNode;
  if (commands.error) {
    body = <LoadError section={section} />;
  } else if (commands.loading) {
    body = <Loading />;
  } else if (commands.items.length === 0) {
    body = <EmptyState title={t("overview.noCommands")} />;
  } else {
    body = (
      <ul className="list">
        {commands.items.slice(0, commandsShown).map((command) => {
          const link = links.get(command.id) ?? null;
          const target = commandTarget(command);
          const content = (
            <>
              <span className="list-text">
                <span className="mono list-title">{command.type}</span>
                <span className="cell-sub ellipsis">
                  {[target, formatMoment(command.created as string, now)]
                    .filter(Boolean)
                    .join(" · ")}
                </span>
              </span>
              <CommandStatusPill status={command.status} />
            </>
          );
          return (
            <li key={command.id} className="link-item">
              {link ? (
                <Link className="list-link list-row" to={link}>
                  {content}
                </Link>
              ) : (
                <div className="list-link list-row">{content}</div>
              )}
            </li>
          );
        })}
      </ul>
    );
  }

  return (
    <Card
      id="activity"
      title={t("overview.queue")}
      side={canAudit && <Link to="/audit/commands">{t("overview.history")}</Link>}
      flush
    >
      {body}
    </Card>
  );
}

function toneText(tone: Tone | undefined): string {
  return tone ? `${tone}-text` : "";
}

function SiteRowView({ row }: { row: SiteRow }) {
  const navigate = useNavigate();
  const Icon =
    row.tls.kind === "off" || row.tls.kind === "none" || row.tls.kind === "noDomain"
      ? LockOpen
      : Lock;
  return (
    <tr className="clickable" onClick={rowClick(() => navigate(`/sites/${row.id}`))}>
      <td className="cell-main">
        <Link to={`/sites/${row.id}`} className="cell-name">
          {row.domain}
        </Link>
        {row.more > 0 && <span className="faint">{` +${row.more}`}</span>}
        <div className="cell-sub ellipsis">{row.source}</div>
      </td>
      <td className="mono muted">{row.hostName}</td>
      <td>
        <span className="site-tls">
          <Icon size={14} className={`route-lock ${row.tls.tone}`} aria-hidden="true" />
          <span className={toneText(row.tls.tone)}>{row.tls.text}</span>
        </span>
      </td>
      <td className="cell-sub" style={{ textAlign: "right" }}>
        <span className={toneText(row.deploy.tone)}>{row.deploy.text}</span>
      </td>
    </tr>
  );
}

function SitesCard({
  rows,
  section,
  canOperate,
}: {
  rows: SiteRow[];
  section: Section;
  canOperate: boolean;
}) {
  const { t } = useTranslation();

  let body: ReactNode;
  if (section.error) {
    body = <LoadError section={section} />;
  } else if (section.loading) {
    body = <Loading />;
  } else if (rows.length === 0) {
    body = (
      <EmptyState
        title={t("site.none")}
        action={
          canOperate && (
            <Link to="/sites/new" className="button">
              {t("site.new")}
            </Link>
          )
        }
      />
    );
  } else {
    body = (
      <>
        <Table label={t("overview.sitesCard")}>
          <thead className="visually-hidden">
            <tr>
              <th>{t("site.list")}</th>
              <th>{t("host.server")}</th>
              <th>{t("site.certificate")}</th>
              <th>{t("overview.deployCol")}</th>
            </tr>
          </thead>
          <tbody>
            {rows.slice(0, sitesShown).map((row) => (
              <SiteRowView key={row.id} row={row} />
            ))}
          </tbody>
        </Table>
        {rows.length > sitesShown && (
          <div className="card-foot">
            <Link to="/sites">{t("overview.moreSites", { count: rows.length - sitesShown })}</Link>
          </div>
        )}
      </>
    );
  }

  return (
    <Card
      title={t("overview.sitesCard")}
      note={t("overview.sitesNote")}
      side={<Link to="/domains">{t("overview.allCertificates")}</Link>}
      flush
    >
      {body}
    </Card>
  );
}

function BackupsCard({
  rows,
  jobs,
  section,
}: {
  rows: ReturnType<typeof backupRows>;
  jobs: number;
  section: Section;
}) {
  const { t } = useTranslation();

  let body: ReactNode;
  if (section.error) {
    body = <LoadError section={section} />;
  } else if (section.loading) {
    body = <Loading />;
  } else if (jobs === 0) {
    body = <EmptyState title={t("overview.noJobs")} />;
  } else if (rows.length === 0) {
    body = <p className="card-pad muted">{t("overview.backupsNone")}</p>;
  } else {
    body = (
      <ul className="list">
        {rows.slice(0, backupsShown).map((row) => (
          <li key={row.job.id} className="link-item">
            <Link className="list-link list-row" to={row.link}>
              <span className={`status-dot ${row.tone}`} aria-hidden="true" />
              <span className="list-text">
                <span className="list-title">{row.job.name}</span>
                <span className="cell-sub ellipsis">
                  {`${row.server} → `}
                  {row.targets.length > 0 ? (
                    row.targets.join(" + ")
                  ) : (
                    <span className="warn-text">{t("backups.job.noTarget")}</span>
                  )}
                </span>
              </span>
              <span className="run-result">
                <span className={toneText(row.tone)} title={row.title}>
                  {row.result}
                </span>
                <span className="faint">{row.when}</span>
              </span>
            </Link>
          </li>
        ))}
      </ul>
    );
  }

  return (
    <Card
      title={t("overview.backupRuns")}
      side={<Link to="/backups">{t("overview.detail")}</Link>}
      flush
    >
      {body}
    </Card>
  );
}

/** Why each server that is offline last lost its stream, when the
 * diagnostics plugin wrote it: read in one list for all of them, once per
 * set of servers offline and the times they were last seen, not live. */
function useDisconnectReasons(hosts: HostSummary[]): Map<string, string> {
  const key = hosts
    .filter((host) => host.status === "offline")
    .map((host) => `${host.id}@${host.last_seen}`)
    .join(",");
  const [reasons, setReasons] = useState<Map<string, string>>(() => new Map());

  useEffect(() => {
    if (!key) {
      return;
    }
    let cancelled = false;
    const ids = key.split(",").map((item) => item.split("@")[0]);
    void pb
      .collection("events")
      .getList<HostEvent>(1, disconnectsRead, disconnectQuery(ids))
      .then((list) => disconnectReasons(list.items))
      .catch(() => new Map<string, string>())
      .then((found) => {
        if (!cancelled) {
          setReasons(found);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [key]);

  return reasons;
}

export function OverviewPage() {
  const { t } = useTranslation();
  const { user } = useAuth();
  const now = useNow(30_000);
  const canOperate = hasRole(user, "operator");
  const canAdmin = hasRole(user, "admin");

  // the Layout's servers and open alerts, which the sidebar shows too
  const hosts = useHosts();
  const alerts = useOpenAlerts();
  const queries = overviewQueries(now);
  const containers = useLiveRecords<Container>("containers", queries.containers);
  const stacks = useLiveList<Stack>("stacks", queries.stacks);
  const commands = useLiveList<Command>("commands", queries.commands);
  const sites = useLiveList<Site>("sites", queries.sites);
  const sitePhp = useLiveList<SitePhp>("site_php", queries.sitePhp);
  const domains = useLiveList<Domain>("domains", queries.domains);
  const certificates = useLiveList<Certificate>("certificates", queries.certificates);
  const deployments = useLiveList<Deployment>("deployments", queries.deployments);
  const jobs = useLiveList<BackupJob>("backup_jobs", queries.jobs);
  const targets = useLiveList<BackupTarget>("backup_targets", queries.targets);
  const runs = useLiveRecords<BackupRun>("backup_runs", queries.runs);
  const providers = useLiveList<DnsProvider>("dns_providers", {
    ...queries.dnsProviders,
    enabled: alerts.items.some((alert) => alert.rule === "dns_provider_failed"),
  });
  const reasons = useDisconnectReasons(hosts.items);

  const hostMap = useMemo(() => new Map(hosts.items.map((host) => [host.id, host])), [hosts.items]);
  const hostNames = useMemo(
    () => new Map(hosts.items.map((host) => [host.id, host.name])),
    [hosts.items],
  );
  const agents = useMemo(() => agentSummary(hosts.items, now, t), [hosts.items, now, t]);
  const counts = useMemo(() => containerSummary(containers.items), [containers.items]);
  const rows = useMemo(
    () =>
      siteRows(
        {
          sites: sites.items as OverviewSite[],
          sitePhp: sitePhp.items,
          domains: domains.items,
          certificates: certificates.items,
          deployments: deployments.items,
          hosts: hostMap,
        },
        now,
        t,
      ),
    [
      sites.items,
      sitePhp.items,
      domains.items,
      certificates.items,
      deployments.items,
      hostMap,
      now,
      t,
    ],
  );
  const sitesTile = useMemo(() => sitesSummary(rows, now, t), [rows, now, t]);
  const backupsTile = useMemo(
    () => backupSummary(jobs.items, runs.items, hostNames, now, t),
    [jobs.items, runs.items, hostNames, now, t],
  );
  const targetNames = useMemo(
    () => new Map(targets.items.map((target) => [target.id, target.name])),
    [targets.items],
  );
  const latestRuns = useMemo(
    () => backupRows(jobs.items, targetNames, runs.items, hostNames, now, t),
    [jobs.items, targetNames, runs.items, hostNames, now, t],
  );
  const items = useMemo(
    () =>
      attentionItems(
        alerts.items,
        {
          hosts: hostMap,
          jobs: new Map(jobs.items.map((job) => [job.id, job])),
          targets: targetNames,
          certificates: new Map(certificates.items.map((cert) => [cert.id, cert])),
          providers: new Map(providers.items.map((provider) => [provider.id, provider])),
          runs: runs.items,
          reasons,
          canAudit: canOperate,
        },
        now,
        t,
      ),
    [
      alerts.items,
      hostMap,
      jobs.items,
      targetNames,
      certificates.items,
      providers.items,
      runs.items,
      reasons,
      canOperate,
      now,
      t,
    ],
  );
  const commandLinks = useMemo(
    () =>
      new Map(
        commands.items.map((command) => [
          command.id,
          commandLink(command, { stacks: stacks.items, runs: runs.items }),
        ]),
      ),
    [commands.items, stacks.items, runs.items],
  );

  // a site is online only on a server that is not offline, and a job and
  // its runs are named by their server: both wait for the servers too
  const sitesSection = sectionOf(hosts, sites, sitePhp, domains, certificates, deployments);
  const backupsSection = sectionOf(hosts, jobs, targets, runs);
  const lists: AnyList[] = [
    hosts,
    containers,
    stacks,
    alerts,
    commands,
    sites,
    sitePhp,
    domains,
    certificates,
    deployments,
    jobs,
    targets,
    runs,
  ];
  const loaded = lists.every((list) => !list.loading);
  const failed = lists.some((list) => list.error);

  // the moments a probe of the page measures: its first render, and the
  // frame after every list has loaded once
  const marked = useRef({ shell: false, filled: false });
  const [filled, setFilled] = useState(false);
  useLayoutEffect(() => {
    if (!marked.current.shell) {
      marked.current.shell = true;
      performance.mark("pc:overview:shell");
    }
  }, []);
  useEffect(() => {
    if (!loaded || marked.current.filled) {
      return;
    }
    const frame = requestAnimationFrame(() => {
      marked.current.filled = true;
      performance.mark("pc:overview:filled");
      setFilled(true);
    });
    return () => cancelAnimationFrame(frame);
  }, [loaded]);
  const state = !filled ? "loading" : failed ? "error" : "filled";

  // the bell counts only what has loaded: never "none new" while the alerts
  // load, or once their read failed
  const openAlerts =
    alerts.loading || alerts.error
      ? 0
      : alerts.items.filter((alert) => alert.status === "open").length;
  const bellLabel = alerts.error
    ? t("overview.bellFailed")
    : alerts.loading
      ? t("overview.bellLoading")
      : openAlerts > 0
        ? t("overview.bell", { count: openAlerts })
        : t("overview.bellNone");
  // the bell brings the focus to the first alert, by its link; to the card's
  // heading when that alert has no link (audit_integrity for a viewer, which
  // as critical comes first), or there is none, so that nothing is skipped
  const focusAttention = () => {
    const card = document.getElementById("attention");
    card?.scrollIntoView?.({ block: "start" });
    const first = card?.querySelector(".list > li")?.querySelector<HTMLElement>("a[href]");
    (first ?? document.getElementById("attention-heading"))?.focus();
  };

  const agentsTile = tileState(sectionOf(hosts), t);
  const containersTile = tileState(sectionOf(containers), t);
  const sitesTileState = tileState(sitesSection, t);
  const backupsTileState = tileState(sectionOf(hosts, jobs, runs), t);

  return (
    <div className="overview" data-overview-state={state}>
      <PageHead
        crumbs={`${formatToday(now)} · ${formatHour(new Date(now))}`}
        title={t("overview.title")}
        actions={
          <>
            <IconButton className="bell" aria-label={bellLabel} onClick={focusAttention}>
              <Bell size={18} aria-hidden="true" />
              {openAlerts > 0 && (
                <span className="bell-badge" aria-hidden="true">
                  {openAlerts > 99 ? "99+" : openAlerts}
                </span>
              )}
            </IconButton>
            {canOperate && (
              <Link to="/sites/new" className="button">
                {t("site.new")}
              </Link>
            )}
            {canAdmin && (
              <Link to="/agents" className="button primary">
                <Plus size={16} aria-hidden="true" />
                {t("overview.addServer")}
              </Link>
            )}
          </>
        }
      />

      <section className="kpis" aria-label={t("overview.summary")}>
        <Kpi
          to="/servers"
          label={t("overview.agents")}
          hint="gRPC"
          value={
            agentsTile?.value ?? (
              <>
                {formatNumber(agents.connected)} <small>/ {formatNumber(agents.total)}</small>
              </>
            )
          }
          note={agentsTile?.note ?? <NoteText note={agents.note} />}
        />
        <Kpi
          to="/stacks"
          label={t("overview.containers")}
          hint={!stacks.loading && !stacks.error && t("overview.stacks", { count: stacks.total })}
          value={
            containersTile?.value ?? (
              <>
                {formatNumber(counts.running)} <small>{t("overview.running")}</small>
              </>
            )
          }
          note={
            containersTile?.note ?? (
              <>
                {t("overview.stopped", { count: counts.stopped })}
                {counts.restarting > 0 && (
                  <>
                    {" · "}
                    <span className="warn-text">
                      {t("overview.restarting", { count: counts.restarting })}
                    </span>
                  </>
                )}
              </>
            )
          }
        />
        <Kpi
          to="/sites"
          label={t("overview.sites")}
          hint="ACME"
          value={
            sitesTileState?.value ?? (
              <>
                {formatNumber(sitesTile.online)} <small>{sitesTile.unit}</small>
              </>
            )
          }
          note={sitesTileState?.note ?? <NoteText note={sitesTile.note} />}
        />
        <Kpi
          to="/backups"
          label={t("overview.backups")}
          hint="restic"
          value={
            backupsTileState?.value ??
            (backupsTile.total === 0 ? (
              "–"
            ) : (
              <>
                {formatNumber(backupsTile.succeeded)}{" "}
                <small>{t("overview.backupsOk", { total: formatNumber(backupsTile.total) })}</small>
              </>
            ))
          }
          note={backupsTileState?.note ?? <NoteText note={backupsTile.note} />}
        />
      </section>

      <div className="columns">
        <div className="column-wide">
          <ServersCard
            hosts={hosts}
            perHost={
              containers.loading || (containers.error && containers.items.length === 0)
                ? null
                : counts.perHost
            }
            now={now}
            canConnect={canAdmin}
          />
        </div>
        <div className="column-side">
          <AttentionCard alerts={alerts} items={items} />
          <QueueCard commands={commands} links={commandLinks} now={now} canAudit={canOperate} />
        </div>
      </div>

      <div className="columns">
        <div className="column-wide">
          <SitesCard rows={rows} section={sitesSection} canOperate={canOperate} />
        </div>
        <div className="column-side">
          <BackupsCard rows={latestRuns} jobs={jobs.items.length} section={backupsSection} />
        </div>
      </div>
    </div>
  );
}
