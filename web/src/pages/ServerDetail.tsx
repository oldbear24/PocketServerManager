import {
  AlignLeft,
  Download,
  MoreHorizontal,
  Play,
  RefreshCw,
  RotateCw,
  Square,
  TerminalSquare,
  Trash2,
} from "lucide-react";
import type { TFunction } from "i18next";
import { Fragment, useEffect, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Link, useNavigate, useParams } from "react-router-dom";

import { PageHead } from "../components/Layout";
import { useToast } from "../components/toast";
import { ContainerStatePill, HostStatusPill } from "../components/status";
import { UsageChart } from "../components/UsageChart";
import {
  Banner,
  Button,
  Card,
  ConfirmDialog,
  EmptyState,
  IconButton,
  ProgressBar,
  StatusPill,
  Table,
  Tabs,
  Timeline,
  type TimelineItem,
  type TimelineState,
} from "../components/ui";
import { api, errorMessage } from "../lib/api";
import { hasRole, useAuth } from "../lib/auth";
import {
  formatBitrate,
  formatBytes,
  formatDate,
  formatDateTime,
  formatDuration,
  formatNumber,
  formatRelative,
  formatTime,
  formatVersion,
  parseDate,
  percentOf,
} from "../lib/format";
import { eventText } from "../lib/events";
import {
  useContainerValues,
  useHostMetrics,
  type MetricRange,
  type MountUsage,
} from "../lib/metrics";
import { pb } from "../lib/pb";
import { useLiveList, useLiveRecord, useNow } from "../lib/realtime";
import type { Command, Container, Host, HostEvent, Network, PanelInfo, Volume } from "../lib/types";

const tabs = ["overview", "containers", "proxy", "volumes", "backups", "agent"] as const;
type Tab = (typeof tabs)[number];

const protectedRoles = ["agent", "updater", "edge", "job"];

export function ServerDetailPage() {
  const { t } = useTranslation();
  const { id, tab } = useParams();
  const navigate = useNavigate();
  const { record: host, loading, error } = useLiveRecord<Host>("hosts", id);

  const containers = useLiveList<Container>("containers", {
    filter: id ? `host = "${id}"` : undefined,
    sort: "name",
    enabled: Boolean(id),
  });

  const active: Tab = (tabs as readonly string[]).includes(tab ?? "") ? (tab as Tab) : "overview";

  if (loading) {
    return null;
  }

  if (error || !host) {
    return <EmptyState title={t("server.notFound")} />;
  }

  // addresses are set in the mono face, like every other technical value
  const facts: ReactNode[] = [
    host.os,
    host.cpu_cores ? t("server.cores", { count: host.cpu_cores }) : "",
    host.mem_bytes ? `${formatBytes(host.mem_bytes)} RAM` : "",
    host.disk_total_bytes ? formatBytes(host.disk_total_bytes) : "",
    host.ip4 ? <span className="mono">{host.ip4}</span> : "",
    host.ip6 ? <span className="mono">{host.ip6}</span> : "",
    host.uptime_s ? t("server.uptime", { value: formatDuration(host.uptime_s) }) : "",
  ].filter(Boolean);

  return (
    <>
      <PageHead
        crumbs={
          <>
            <Link to="/servers">{t("nav.servers")}</Link> / {host.name}
          </>
        }
        title={<span className="mono">{host.name}</span>}
        titleExtra={
          <>
            <HostStatusPill status={host.status} />
            {host.roles.map((role) => (
              <StatusPill key={role} tone="info" dot={false}>
                {t(`host.role.${role}`)}
              </StatusPill>
            ))}
          </>
        }
        description={facts.map((fact, index) => (
          <Fragment key={index}>
            {index > 0 && " · "}
            {fact}
          </Fragment>
        ))}
        actions={<HostActions host={host} />}
      />

      <Tabs
        label={t("server.tabs")}
        active={active}
        onChange={(next) => navigate(`/servers/${host.id}/${next === "overview" ? "" : next}`)}
        items={[
          { id: "overview", label: t("server.tab.overview") },
          { id: "containers", label: t("server.tab.containers"), count: containers.total },
          { id: "proxy", label: t("server.tab.proxy") },
          { id: "volumes", label: t("server.tab.volumes") },
          { id: "backups", label: t("server.tab.backups") },
          { id: "agent", label: t("server.tab.agent") },
        ]}
      />

      {active === "overview" && <OverviewTab host={host} containers={containers.items} />}
      {active === "containers" && <ContainersCard containers={containers.items} host={host} />}
      {active === "proxy" && (
        <Card>
          <EmptyState title={t("server.tab.proxy")}>{t("server.proxyLater")}</EmptyState>
        </Card>
      )}
      {active === "volumes" && <VolumesTab host={host} />}
      {active === "backups" && (
        <Card>
          <EmptyState title={t("server.tab.backups")}>{t("server.backupsLater")}</EmptyState>
        </Card>
      )}
      {active === "agent" && <AgentTab host={host} />}
    </>
  );
}

function HostActions({ host }: { host: Host }) {
  const { t } = useTranslation();
  const { user } = useAuth();
  const toast = useToast();
  const [confirming, setConfirming] = useState(false);

  if (!hasRole(user, "admin")) {
    return null;
  }

  const restart = async () => {
    setConfirming(false);
    try {
      await api.runCommand(host.id, "agent.restart", {}, host.name);
      toast.success(t("server.restartQueued"));
    } catch (err) {
      toast.error(err);
    }
  };

  const offline = host.status === "offline";

  const run = async (type: string, payload: object, message: string) => {
    try {
      await api.runCommand(host.id, type, payload, host.name);
      toast.success(message);
    } catch (err) {
      toast.error(err);
    }
  };

  return (
    <>
      <Link className="button" to={`/servers/${host.id}/terminal`} aria-disabled={offline}>
        <TerminalSquare size={16} aria-hidden="true" />
        {t("terminal.open")}
      </Link>
      <Button disabled={offline} onClick={() => setConfirming(true)}>
        <RotateCw size={16} aria-hidden="true" />
        {t("server.restartAgent")}
      </Button>
      <details className="menu">
        <summary
          className="button icon-button"
          aria-label={t("server.more")}
          title={t("server.more")}
        >
          <MoreHorizontal size={16} aria-hidden="true" />
        </summary>
        <div className="menu-items">
          <button
            type="button"
            disabled={offline}
            onClick={() => void run("agent.rotate_certificate", {}, t("server.rotateQueued"))}
          >
            {t("server.rotateCert")}
          </button>
          <button
            type="button"
            disabled={offline}
            onClick={() =>
              void run("agent.diagnostics", { log_lines: 1000 }, t("server.diagnosticsQueued"))
            }
          >
            {t("server.downloadDiagnostics")}
          </button>
          <Link to={`/servers/${host.id}/agent`}>{t("server.tab.agent")}</Link>
        </div>
      </details>
      <ConfirmDialog
        open={confirming}
        title={t("server.restartConfirm", { name: host.name })}
        confirmLabel={t("server.restartAgent")}
        onConfirm={() => void restart()}
        onCancel={() => setConfirming(false)}
      >
        <p className="muted">{t("server.restartNote")}</p>
      </ConfirmDialog>
    </>
  );
}

function OverviewTab({ host, containers }: { host: Host; containers: Container[] }) {
  const [range, setRange] = useState<MetricRange>("24h");
  const metrics = useHostMetrics(host.id, range);
  const isEdge = host.roles.includes("edge");

  return (
    <>
      <div className="columns">
        <div className="column-side" style={{ flexBasis: 380 }}>
          <AgentCard host={host} />
        </div>
        <div className="column-wide">
          <UsageCard host={host} metrics={metrics} range={range} onRange={setRange} />
        </div>
      </div>
      <ContainersCard containers={containers} host={host} />
      <div className="columns">
        <div className="column-wide">
          {isEdge ? <EdgeRoutesCard /> : <EventsCard host={host} />}
        </div>
        <div className="column-side">
          <DisksCard mounts={metrics.data?.mounts ?? []} />
          {isEdge && <EventsCard host={host} />}
        </div>
      </div>
    </>
  );
}

function AgentCard({ host }: { host: Host }) {
  const { t } = useTranslation();
  const { user } = useAuth();
  const toast = useToast();
  const now = useNow();
  const [info, setInfo] = useState<PanelInfo | null>(null);

  useEffect(() => {
    let cancelled = false;
    api
      .info()
      .then((loaded) => {
        if (!cancelled) {
          setInfo(loaded);
        }
      })
      .catch(() => {
        // only the "latest" badge and the endpoint are missing then
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const queued = useLiveList<Command>("commands", {
    filter: `host = "${host.id}" && status = "queued"`,
    perPage: 1,
  });
  const sent = useLiveList<Command>("commands", {
    filter: `host = "${host.id}" && (status = "dispatched" || status = "running")`,
    perPage: 1,
  });
  const agentContainer = useLiveList<Container>("containers", {
    filter: `host = "${host.id}" && system_role = "agent"`,
    perPage: 1,
  }).items[0];
  const done = useLiveList<Command>("commands", {
    filter: `host = "${host.id}" && status = "succeeded"`,
    perPage: 1,
  });

  const isAdmin = hasRole(user, "admin");
  const offline = host.status === "offline";
  const latest = info && info.version.replace(/^v/, "") === host.agent_version;

  const run = async (type: string, message: string) => {
    try {
      await api.runCommand(
        host.id,
        type,
        type === "agent.diagnostics" ? { log_lines: 1000 } : {},
        host.name,
      );
      toast.success(message);
    } catch (err) {
      toast.error(err);
    }
  };

  return (
    <Card
      title={t("server.agentCard")}
      side={
        <StatusPill tone={host.stream_open ? "ok" : "off"}>
          {host.stream_open ? t("server.streamOpen") : t("server.streamClosed")}
        </StatusPill>
      }
    >
      <dl className="facts">
        <dt>{t("server.connection")}</dt>
        <dd className="mono">dial-out → {info?.grpc_address ?? "–"}</dd>
        <dt>{t("server.encryption")}</dt>
        <dd>{t("server.mtlsUntil", { date: formatDate(host.cert_expires_at) })}</dd>
        <dt>{t("server.agentVersion")}</dt>
        <dd>
          <span className="mono">{formatVersion(host.agent_version)}</span>
          {latest && <span className="ok-text"> · {t("server.latest")}</span>}
        </dd>
        <dt>Heartbeat</dt>
        <dd>
          {formatRelative(host.last_seen, now, { past: true })} · {t("server.interval")}
        </dd>
        <dt>RTT</dt>
        <dd className="num">{host.rtt_ms ? `${formatNumber(host.rtt_ms, 1)} ms` : "–"}</dd>
        <dt>{t("server.connectedSince")}</dt>
        <dd>{formatDateTime(host.connected_since)}</dd>
        {agentContainer && (
          <>
            <dt>{t("server.containerRow")}</dt>
            <dd>
              {t("server.agentContainerLine", {
                name: agentContainer.name,
                version: formatVersion(host.agent_version),
              })}
              {agentContainer.health && ` · ${agentContainer.health}`}
            </dd>
          </>
        )}
        <dt>Docker</dt>
        <dd>
          {[
            host.docker_version ? `Engine ${host.docker_version}` : "",
            host.compose_version ? t("server.composeShort", { version: host.compose_version }) : "",
          ]
            .filter(Boolean)
            .join(" · ") || "–"}
        </dd>
        <dt>{t("server.commands")}</dt>
        <dd>
          {t("server.commandCounts3", {
            queued: queued.total,
            sent: sent.total,
            done: done.total,
          })}
        </dd>
      </dl>
      {isAdmin && (
        <div style={{ display: "flex", flexWrap: "wrap", gap: 8, marginTop: 16 }}>
          <Button
            disabled={offline}
            onClick={() => void run("agent.rotate_certificate", t("server.rotateQueued"))}
          >
            {t("server.rotateCert")}
          </Button>
          <Button
            disabled={offline}
            onClick={() => void run("agent.diagnostics", t("server.diagnosticsQueued"))}
          >
            {t("server.downloadDiagnostics")}
          </Button>
        </div>
      )}
    </Card>
  );
}

function UsageCard({
  host,
  metrics,
  range,
  onRange,
}: {
  host: Host;
  metrics: ReturnType<typeof useHostMetrics>;
  range: MetricRange;
  onRange: (range: MetricRange) => void;
}) {
  const { t } = useTranslation();
  const points = metrics.data?.points ?? null;
  const last = points ? points.t.length - 1 : -1;

  return (
    <UsageChart
      title={t("server.usage")}
      points={points}
      stale={metrics.stale}
      range={range}
      onRange={onRange}
      currentCpu={host.cpu_percent}
      currentMem={`${formatBytes(host.mem_used_bytes)} / ${formatBytes(host.mem_bytes)}`}
      footer={
        <dl className="stat-row">
          <div>
            <dt>{t("server.netIn")}</dt>
            <dd>{last >= 0 ? formatBitrate(points?.net_in[last]) : "–"}</dd>
          </div>
          <div>
            <dt>{t("server.netOut")}</dt>
            <dd>{last >= 0 ? formatBitrate(points?.net_out[last]) : "–"}</dd>
          </div>
          <div>
            <dt>{t("server.load")}</dt>
            <dd>
              {[host.load1, host.load5, host.load15]
                .map((value) => formatNumber(value, 2))
                .join(" ")}
            </dd>
          </div>
        </dl>
      }
    />
  );
}

/** The routes of an edge proxy arrive with sites and domains. */
function EdgeRoutesCard() {
  const { t } = useTranslation();

  return (
    <Card title={t("server.edgeRoutes")} note={t("server.edgeRoutesNote")}>
      <p className="muted">{t("server.edgeRoutesLater")}</p>
    </Card>
  );
}

function DisksCard({ mounts }: { mounts: MountUsage[] }) {
  const { t } = useTranslation();

  return (
    <Card title={t("server.disks")}>
      {mounts.length === 0 ? (
        <p className="muted">{t("server.noDisks")}</p>
      ) : (
        <div className="disk-list">
          {mounts.map((mount) => (
            <div key={mount.mount}>
              <div className="disk-line">
                <span className="mono">{mount.mount}</span>
                <span className="num muted">
                  {formatBytes(mount.used)} / {formatBytes(mount.total)}
                </span>
              </div>
              <ProgressBar
                value={percentOf(mount.used, mount.total)}
                label={t("server.diskOf", { mount: mount.mount })}
                warnAt={85}
                errAt={95}
              />
            </div>
          ))}
        </div>
      )}
    </Card>
  );
}

/**
 * How a container can be reached: its published ports, each once (Docker lists a
 * port for IPv4 and again for IPv6), or only from its Docker networks.
 */
function exposure(container: Container, t: TFunction): string {
  const ports = container.ports ?? [];
  const published = [...new Set(ports.filter((port) => port.public).map((port) => port.public))];
  if (published.length > 0) {
    return published.map((port) => `:${port}`).join(" ");
  }
  if (ports.length > 0 && container.state === "running") {
    return t("server.internalOnly");
  }
  return "–";
}

/** The stack of a container: a link if PocketCloud manages it, else the project's name. */
function StackLink({ container }: { container: Container }) {
  if (container.stack) {
    return <Link to={`/stacks/${container.stack}`}>{container.project}</Link>;
  }
  return <>{container.project || "–"}</>;
}

export function ContainersCard({ containers, host }: { containers: Container[]; host: Host }) {
  const { t } = useTranslation();
  const { user } = useAuth();
  const toast = useToast();
  const now = useNow(30000);
  const values = useContainerValues(host.id).data ?? {};

  const [removing, setRemoving] = useState<Container[] | null>(null);
  const [removeVolumes, setRemoveVolumes] = useState(false);
  const [pending, setPending] = useState<Record<string, boolean>>({});
  const [selected, setSelected] = useState<Set<string>>(new Set());

  const canAct = hasRole(user, "operator");
  const isAdmin = hasRole(user, "admin");

  // system containers are not chosen with others: they cannot be stopped or removed here
  const selectable = canAct
    ? containers.filter((container) => !protectedRoles.includes(container.system_role))
    : [];
  const chosen = selectable.filter((container) => selected.has(container.id));
  const allChosen = selectable.length > 0 && chosen.length === selectable.length;

  const toggle = (id: string) =>
    setSelected((current) => {
      const next = new Set(current);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });

  const running = containers.filter((container) => container.state === "running").length;
  const restarting = containers.filter((container) => container.state === "restarting").length;
  const stopped = containers.length - running - restarting;

  const act = async (container: Container, type: string, payload: object) => {
    setPending((current) => ({ ...current, [container.id]: true }));
    try {
      await api.runCommand(
        host.id,
        type,
        { docker_id: container.docker_id, ...payload },
        container.name,
      );
    } catch (err) {
      toast.error(err);
    } finally {
      // the next inventory event shows the outcome; the pending state only bridges the wait
      setTimeout(
        () =>
          setPending((current) => {
            const next = { ...current };
            delete next[container.id];
            return next;
          }),
        1500,
      );
    }
  };

  const runAll = (list: Container[], type: string) => {
    for (const container of list) {
      void act(container, type, {});
    }
  };

  // an image shared by several containers is pulled once
  const pullImages = async (list: Container[]) => {
    const images = [...new Set(list.map((container) => container.image))];
    try {
      await Promise.all(
        images.map((image) => api.runCommand(host.id, "image.pull", { image }, image)),
      );
      toast.success(t("server.pullQueued", { count: images.length }));
    } catch (err) {
      toast.error(err);
    }
  };

  return (
    <Card
      title={t("server.containers")}
      note={t("server.containerCounts", { running, restarting, stopped })}
      side={
        canAct && (
          <Link className="button small" to="/stacks">
            {t("server.newStack")}
          </Link>
        )
      }
      flush
    >
      {chosen.length > 0 && (
        <div className="bulk-bar" role="toolbar" aria-label={t("server.bulkActions")}>
          <span className="bulk-count">{t("server.selectedCount", { count: chosen.length })}</span>
          <Button
            size="small"
            onClick={() =>
              runAll(
                chosen.filter((container) => container.state !== "running"),
                "container.start",
              )
            }
          >
            <Play size={14} aria-hidden="true" />
            {t("server.bulkStart")}
          </Button>
          <Button
            size="small"
            onClick={() =>
              runAll(
                chosen.filter((container) => container.state === "running"),
                "container.stop",
              )
            }
          >
            <Square size={14} aria-hidden="true" />
            {t("server.bulkStop")}
          </Button>
          <Button size="small" onClick={() => runAll(chosen, "container.restart")}>
            <RefreshCw size={14} aria-hidden="true" />
            {t("server.bulkRestart")}
          </Button>
          <Button size="small" onClick={() => void pullImages(chosen)}>
            <Download size={14} aria-hidden="true" />
            {t("server.bulkPull")}
          </Button>
          <Button
            size="small"
            variant="danger"
            onClick={() => {
              setRemoveVolumes(false);
              setRemoving(chosen);
            }}
          >
            <Trash2 size={14} aria-hidden="true" />
            {t("server.bulkRemove")}
          </Button>
          <button
            type="button"
            className="link-button small"
            onClick={() => setSelected(new Set())}
          >
            {t("server.clearSelection")}
          </button>
        </div>
      )}
      {containers.length === 0 ? (
        <EmptyState title={t("server.noContainers")}>{t("server.noContainersText")}</EmptyState>
      ) : (
        <Table label={t("server.containers")}>
          <thead>
            <tr>
              {canAct && (
                <th className="select-cell">
                  <input
                    type="checkbox"
                    aria-label={t("server.selectAll")}
                    checked={allChosen}
                    disabled={selectable.length === 0}
                    ref={(input) => {
                      if (input) {
                        input.indeterminate = chosen.length > 0 && !allChosen;
                      }
                    }}
                    onChange={() =>
                      setSelected(
                        allChosen ? new Set() : new Set(selectable.map((item) => item.id)),
                      )
                    }
                  />
                </th>
              )}
              <th>{t("server.container")}</th>
              <th>Stack</th>
              <th>{t("host.state")}</th>
              <th>{t("server.cpuShort")}</th>
              <th>{t("server.ramShort")}</th>
              <th>{t("server.ports")}</th>
              <th>Uptime</th>
              <th style={{ textAlign: "right" }}>{t("common.actions")}</th>
            </tr>
          </thead>
          <tbody>
            {containers.map((container) => {
              const isSystem = protectedRoles.includes(container.system_role);
              const isRunning = container.state === "running";
              const busy = pending[container.id];
              const usage = isRunning ? values[container.docker_id] : undefined;

              return (
                <tr
                  key={container.id}
                  aria-selected={canAct ? selected.has(container.id) : undefined}
                >
                  {canAct && (
                    <td className="select-cell">
                      {!isSystem && (
                        <input
                          type="checkbox"
                          aria-label={t("server.selectContainer", { name: container.name })}
                          checked={selected.has(container.id)}
                          onChange={() => toggle(container.id)}
                        />
                      )}
                    </td>
                  )}
                  <td>
                    <Link
                      className="cell-title"
                      to={`/servers/${host.id}/containers/${container.docker_id}`}
                    >
                      {container.name}
                    </Link>
                    <div className="cell-sub">{container.image}</div>
                  </td>
                  <td className="muted">
                    {isSystem ? (
                      <StatusPill tone="off" dot={false}>
                        {t("server.system")}
                      </StatusPill>
                    ) : (
                      <StackLink container={container} />
                    )}
                  </td>
                  <td>
                    <ContainerStatePill state={container.state} health={container.health} />
                  </td>
                  <td className="num">{usage ? `${formatNumber(usage.cpu, 1)} %` : "–"}</td>
                  <td className="num">{usage ? formatBytes(usage.mem) : "–"}</td>
                  <td className="mono small">{exposure(container, t)}</td>
                  <td className="muted">
                    {isRunning && parseDate(container.started_at)
                      ? formatDuration((now - parseDate(container.started_at)!.getTime()) / 1000)
                      : container.state === "restarting"
                        ? t("server.restarts", { count: container.restart_count })
                        : "–"}
                  </td>
                  <td style={{ textAlign: "right", whiteSpace: "nowrap" }}>
                    <Link
                      className="button icon-button bare"
                      to={`/servers/${host.id}/containers/${container.docker_id}`}
                      aria-label={t("server.showLogs", { name: container.name })}
                      title={t("server.showLogs", { name: container.name })}
                    >
                      <AlignLeft size={16} aria-hidden="true" />
                    </Link>
                    {isAdmin && isRunning && (
                      <Link
                        className="button icon-button bare"
                        to={`/servers/${host.id}/terminal?container=${container.docker_id}`}
                        aria-label={t("server.openTerminal", { name: container.name })}
                        title={t("server.openTerminal", { name: container.name })}
                      >
                        <TerminalSquare size={16} aria-hidden="true" />
                      </Link>
                    )}
                    {canAct && !isRunning && (
                      <IconButton
                        bare
                        disabled={busy}
                        aria-label={t("server.startContainer", { name: container.name })}
                        onClick={() => void act(container, "container.start", {})}
                      >
                        <Play size={16} aria-hidden="true" />
                      </IconButton>
                    )}
                    {canAct && !isSystem && (
                      <>
                        {isRunning && (
                          <IconButton
                            bare
                            disabled={busy}
                            aria-label={t("server.stopContainer", { name: container.name })}
                            onClick={() => void act(container, "container.stop", {})}
                          >
                            <Square size={16} aria-hidden="true" />
                          </IconButton>
                        )}
                        <IconButton
                          bare
                          disabled={busy}
                          aria-label={t("server.restartContainer", { name: container.name })}
                          onClick={() => void act(container, "container.restart", {})}
                        >
                          <RefreshCw size={16} aria-hidden="true" />
                        </IconButton>
                        <IconButton
                          bare
                          disabled={busy}
                          aria-label={t("server.removeContainer", { name: container.name })}
                          onClick={() => {
                            setRemoveVolumes(false);
                            setRemoving([container]);
                          }}
                        >
                          <Trash2 size={16} aria-hidden="true" />
                        </IconButton>
                      </>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </Table>
      )}

      <ConfirmDialog
        open={removing !== null}
        danger
        title={
          removing && removing.length > 1
            ? t("server.removeManyConfirm", { count: removing.length })
            : t("server.removeConfirm", { name: removing?.[0]?.name })
        }
        confirmLabel={t("server.removeAction")}
        onCancel={() => setRemoving(null)}
        onConfirm={() => {
          for (const container of removing ?? []) {
            void act(container, "container.remove", {
              remove_volumes: removeVolumes,
              force: true,
            });
          }
          if (removing && removing.length > 1) {
            setSelected(new Set());
          }
          setRemoving(null);
        }}
      >
        {removing && removing.length > 1 && (
          <p className="muted" style={{ marginBottom: 10 }}>
            {removing.map((container) => container.name).join(", ")}
          </p>
        )}
        <label style={{ display: "flex", gap: 8, alignItems: "center" }}>
          <input
            type="checkbox"
            checked={removeVolumes}
            onChange={(event) => setRemoveVolumes(event.target.checked)}
          />
          {t("server.removeVolumes")}
        </label>
      </ConfirmDialog>
    </Card>
  );
}

function EventsCard({ host }: { host: Host }) {
  const { t } = useTranslation();
  const events = useLiveList<HostEvent>("events", {
    filter: `host = "${host.id}"`,
    sort: "-created",
    perPage: 12,
  });

  return (
    <Card title={t("server.events")} flush>
      {events.items.length === 0 ? (
        <EmptyState title={t("server.noEvents")} />
      ) : (
        <ul className="list">
          {events.items.map((event) => (
            <li key={event.id} style={{ display: "flex", gap: 14 }}>
              <span className="mono muted small" style={{ flex: "none", paddingTop: 2 }}>
                {formatTime(event.created)}
              </span>
              <span
                className={
                  event.severity === "critical"
                    ? "err-text"
                    : event.severity === "warning"
                      ? "warn-text"
                      : ""
                }
              >
                {eventText(t, event)}
              </span>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

function VolumesTab({ host }: { host: Host }) {
  const { t } = useTranslation();
  const filter = `host = "${host.id}"`;
  const volumes = useLiveList<Volume>("volumes", { filter, sort: "name" });
  const networks = useLiveList<Network>("networks", { filter, sort: "name" });

  return (
    <div className="columns">
      <div className="column-wide">
        <Card title={t("server.volumes")} flush>
          {volumes.items.length === 0 ? (
            <EmptyState title={t("server.noVolumes")} />
          ) : (
            <Table label={t("server.volumes")} compact>
              <thead>
                <tr>
                  <th>{t("common.name")}</th>
                  <th>Driver</th>
                </tr>
              </thead>
              <tbody>
                {volumes.items.map((volume) => (
                  <tr key={volume.id}>
                    <td className="cell-title">{volume.name}</td>
                    <td className="muted">{volume.driver}</td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
        </Card>
      </div>
      <div className="column-side">
        <Card title={t("server.networks")} flush>
          {networks.items.length === 0 ? (
            <EmptyState title={t("server.noNetworks")} />
          ) : (
            <Table label={t("server.networks")} compact>
              <thead>
                <tr>
                  <th>{t("common.name")}</th>
                  <th>Driver</th>
                </tr>
              </thead>
              <tbody>
                {networks.items.map((network) => (
                  <tr key={network.id}>
                    <td className="cell-title">{network.name}</td>
                    <td className="muted">{network.driver}</td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
        </Card>
      </div>
    </div>
  );
}

type Bundle = {
  id: string;
  collectionId: string;
  collectionName: string;
  file: string;
  size_bytes: number;
  created: string;
  expires_at: string;
};

/**
 * The steps of the last update of this agent: pull, updater started, new agent
 * connected, old container removed or rolled back.
 */
function updateTimeline(command: Command, t: TFunction): TimelineItem[] {
  const finished = !["queued", "dispatched", "running"].includes(command.status);
  const succeeded = command.status === "succeeded";
  const step = command.progress?.step ?? "";
  const started = step === "updater_started" || finished;
  const rolledBack = command.result?.rolled_back === true;

  const state = (done: boolean, active: boolean): TimelineState =>
    done ? "done" : active ? "active" : "pending";

  return [
    {
      id: "pull",
      state: state(started || succeeded, step === "pull" || command.status === "dispatched"),
      title: t("server.updateStepPull"),
    },
    {
      id: "updater",
      state: state(started, false),
      title: t("server.updateStepUpdater"),
    },
    {
      id: "connected",
      state: succeeded ? "done" : finished ? "failed" : state(false, started),
      title: t("server.updateStepConnected"),
      note: finished && !succeeded ? command.error : undefined,
    },
    {
      id: "swap",
      state: succeeded ? "done" : rolledBack ? "failed" : "pending",
      title: rolledBack ? t("server.updateStepRolledBack") : t("server.updateStepRemoved"),
    },
  ];
}

function UpdateCard({ host }: { host: Host }) {
  const { t } = useTranslation();
  const toast = useToast();
  const [info, setInfo] = useState<PanelInfo | null>(null);

  useEffect(() => {
    let cancelled = false;
    api
      .info()
      .then((loaded) => {
        if (!cancelled) {
          setInfo(loaded);
        }
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, []);

  const updates = useLiveList<Command>("commands", {
    filter: pb.filter("host = {:host} && type = 'agent.update'", { host: host.id }),
    sort: "-created",
    perPage: 1,
  });

  const last = updates.items[0];
  const running = last && ["queued", "dispatched", "running"].includes(last.status);
  const latest = info?.version.replace(/^v/, "") === host.agent_version;

  const update = async () => {
    try {
      await api.updateHost(host.id);
    } catch (err) {
      toast.error(err);
    }
  };

  return (
    <Card
      title={t("server.update")}
      side={info ? t("connect.latestVersion", { version: formatVersion(info.version) }) : undefined}
    >
      {last ? (
        <Timeline label={t("server.update")} items={updateTimeline(last, t)} />
      ) : (
        <p className="muted">{latest ? t("server.upToDate") : t("server.updateAvailable")}</p>
      )}
      {info && !info.images_pinned && (
        <p className="warn-text small" style={{ marginTop: 10 }}>
          {t("connect.notPinned")}
        </p>
      )}
      <div style={{ marginTop: 14 }}>
        <Button
          disabled={running || latest || !info?.images_pinned || host.status === "offline"}
          onClick={() => void update()}
        >
          {t("server.updateAgent")}
        </Button>
      </div>
    </Card>
  );
}

function AgentTab({ host }: { host: Host }) {
  const { t } = useTranslation();
  const { user } = useAuth();
  const toast = useToast();
  const now = useNow();
  const navigate = useNavigate();
  const [removing, setRemoving] = useState(false);

  const isAdmin = hasRole(user, "admin");

  const bundles = useLiveList<Bundle & { collectionId: string; collectionName: string }>(
    "diagnostic_bundles",
    {
      filter: `host = "${host.id}"`,
      sort: "-created",
      perPage: 5,
      enabled: isAdmin,
    },
  );

  const downloadBundle = async (bundle: Bundle) => {
    try {
      const token = await pb.files.getToken();
      window.location.href = pb.files.getURL(bundle, bundle.file, { token });
    } catch (err) {
      toast.error(err);
    }
  };

  const remove = async () => {
    try {
      await pb.collection("hosts").delete(host.id);
      navigate("/servers");
    } catch (err) {
      setRemoving(false);
      toast.error(errorMessage(err));
    }
  };

  return (
    <div className="columns">
      <div className="column-wide">
        <AgentCard host={host} />
        <Card
          title={t("server.selfChecks")}
          side={t("server.lastHeartbeat", {
            when: formatRelative(host.last_seen, now, { past: true }),
          })}
          flush
        >
          {(host.self_checks ?? []).length === 0 ? (
            <EmptyState title={t("server.noChecks")} />
          ) : (
            <ul className="list">
              {(host.self_checks ?? []).map((check) => (
                <li key={check.name} className="list-row">
                  <div>
                    <div style={{ fontWeight: 500 }}>
                      {t(`server.check.${check.name}`, check.name)}
                    </div>
                    <div className="cell-sub">{check.detail}</div>
                  </div>
                  <StatusPill tone={check.skipped ? "off" : check.ok ? "ok" : "err"} dot={false}>
                    {check.skipped
                      ? t("server.checkSkipped")
                      : check.ok
                        ? "OK"
                        : t("server.checkFailed")}
                  </StatusPill>
                </li>
              ))}
            </ul>
          )}
        </Card>
      </div>

      <div className="column-side">
        {isAdmin && <UpdateCard host={host} />}

        {host.runtime && (
          <Card title={t("server.runtime")}>
            <dl className="facts">
              <dt>{t("server.goroutinesHeap")}</dt>
              <dd className="mono">
                {host.runtime.goroutines} · {formatBytes(host.runtime.heap_bytes)}
              </dd>
              <dt>{t("server.workers")}</dt>
              <dd className="mono">
                {host.runtime.workers_busy} / {host.runtime.workers_max}
              </dd>
              <dt>Docker API</dt>
              <dd className="mono">{formatNumber(host.runtime.docker_api_latency_ms, 1)} ms</dd>
              <dt>{t("server.volumeFree")}</dt>
              <dd className="mono">{formatBytes(host.runtime.volume_free_bytes)}</dd>
              <dt>{t("server.logLevel")}</dt>
              <dd className="mono">{host.runtime.log_level}</dd>
            </dl>
          </Card>
        )}

        {isAdmin && (
          <Card title={t("server.bundles")} flush>
            {bundles.items.length === 0 ? (
              <EmptyState title={t("server.noBundles")} />
            ) : (
              <ul className="list">
                {bundles.items.map((bundle) => (
                  <li key={bundle.id} className="list-row">
                    <div>
                      <div>{formatDateTime(bundle.created)}</div>
                      <div className="cell-sub">
                        {formatBytes(bundle.size_bytes)} ·{" "}
                        {t("server.expires", { when: formatRelative(bundle.expires_at, now) })}
                      </div>
                    </div>
                    <IconButton
                      aria-label={t("server.downloadBundle")}
                      onClick={() => void downloadBundle(bundle)}
                    >
                      <Download size={16} aria-hidden="true" />
                    </IconButton>
                  </li>
                ))}
              </ul>
            )}
          </Card>
        )}

        {isAdmin && (
          <Card title={t("server.removeTitle")}>
            <Banner tone="warn">{t("server.removeHostNote")}</Banner>
            <div style={{ marginTop: 12 }}>
              <Button variant="danger" onClick={() => setRemoving(true)}>
                {t("server.removeHost")}
              </Button>
            </div>
            <ConfirmDialog
              open={removing}
              danger
              title={t("server.removeHostConfirm", { name: host.name })}
              confirmLabel={t("server.removeHost")}
              onConfirm={() => void remove()}
              onCancel={() => setRemoving(false)}
            />
          </Card>
        )}
      </div>
    </div>
  );
}
