import { Download, Play, RefreshCw, RotateCw, Square, Trash2 } from "lucide-react";
import type { TFunction } from "i18next";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useNavigate, useParams } from "react-router-dom";

import { PageHead } from "../components/Layout";
import { useToast } from "../components/toast";
import { ContainerStatePill, HostStatusPill } from "../components/status";
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
import { pb } from "../lib/pb";
import { useLiveList, useLiveRecord, useNow } from "../lib/realtime";
import type { Command, Container, Host, HostEvent, Network, PanelInfo, Volume } from "../lib/types";

const tabs = ["overview", "containers", "volumes", "agent"] as const;
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

  const facts = [
    host.os,
    host.cpu_cores ? t("server.cores", { count: host.cpu_cores }) : "",
    host.mem_bytes ? `${formatBytes(host.mem_bytes)} RAM` : "",
    host.ip4,
    host.ip6,
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
        description={facts.join(" · ")}
        actions={<HostActions host={host} />}
      />

      <Tabs
        label={t("server.tabs")}
        active={active}
        onChange={(next) => navigate(`/servers/${host.id}/${next === "overview" ? "" : next}`)}
        items={[
          { id: "overview", label: t("server.tab.overview") },
          { id: "containers", label: t("server.tab.containers"), count: containers.total },
          { id: "volumes", label: t("server.tab.volumes") },
          { id: "agent", label: t("server.tab.agent") },
        ]}
      />

      {active === "overview" && <OverviewTab host={host} containers={containers.items} />}
      {active === "containers" && <ContainersCard containers={containers.items} host={host} />}
      {active === "volumes" && <VolumesTab host={host} />}
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

  return (
    <>
      <Button disabled={host.status === "offline"} onClick={() => setConfirming(true)}>
        <RotateCw size={16} aria-hidden="true" />
        {t("server.restartAgent")}
      </Button>
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
  return (
    <>
      <div className="columns">
        <div className="column-side" style={{ flexBasis: 380 }}>
          <AgentCard host={host} />
        </div>
        <div className="column-wide">
          <UsageCard host={host} />
        </div>
      </div>
      <ContainersCard containers={containers} host={host} />
      <EventsCard host={host} />
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
    filter: `host = "${host.id}" && (status = "queued" || status = "dispatched" || status = "running")`,
    perPage: 1,
  });
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
          {formatRelative(host.last_seen, now)} · {t("server.interval")}
        </dd>
        <dt>RTT</dt>
        <dd className="num">{host.rtt_ms ? `${formatNumber(host.rtt_ms, 1)} ms` : "–"}</dd>
        <dt>{t("server.connectedSince")}</dt>
        <dd>{formatDateTime(host.connected_since)}</dd>
        <dt>Docker</dt>
        <dd>{host.docker_version ? `Engine ${host.docker_version}` : "–"}</dd>
        <dt>{t("server.commands")}</dt>
        <dd>{t("server.commandCounts", { open: queued.total, done: done.total })}</dd>
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

function UsageCard({ host }: { host: Host }) {
  const { t } = useTranslation();
  const memory = percentOf(host.mem_used_bytes, host.mem_bytes);
  const disk = percentOf(host.disk_used_bytes, host.disk_total_bytes);

  return (
    <Card title={t("server.usage")}>
      <dl className="facts" style={{ gridTemplateColumns: "minmax(70px, 100px) 1fr" }}>
        <dt>CPU</dt>
        <dd>
          <span className="num">{formatNumber(host.cpu_percent)} %</span>
          <ProgressBar value={host.cpu_percent} label="CPU" />
        </dd>
        <dt>RAM</dt>
        <dd>
          <span className="num">
            {formatBytes(host.mem_used_bytes)} / {formatBytes(host.mem_bytes)}
          </span>
          <ProgressBar value={memory} label="RAM" />
        </dd>
        <dt>{t("host.disk")}</dt>
        <dd>
          <span className="num">
            {formatBytes(host.disk_used_bytes)} / {formatBytes(host.disk_total_bytes)}
          </span>
          <ProgressBar value={disk} label={t("host.disk")} warnAt={85} errAt={95} />
        </dd>
        <dt>Load</dt>
        <dd className="num">{formatNumber(host.load1, 2)}</dd>
      </dl>
    </Card>
  );
}

export function ContainersCard({ containers, host }: { containers: Container[]; host: Host }) {
  const { t } = useTranslation();
  const { user } = useAuth();
  const toast = useToast();
  const now = useNow(30000);

  const [removing, setRemoving] = useState<Container | null>(null);
  const [removeVolumes, setRemoveVolumes] = useState(false);
  const [pending, setPending] = useState<Record<string, boolean>>({});

  const canAct = hasRole(user, "operator");

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

  return (
    <Card
      title={t("server.containers")}
      note={t("server.containerCounts", { running, restarting, stopped })}
      flush
    >
      {containers.length === 0 ? (
        <EmptyState title={t("server.noContainers")}>{t("server.noContainersText")}</EmptyState>
      ) : (
        <Table label={t("server.containers")}>
          <thead>
            <tr>
              <th>{t("server.container")}</th>
              <th>Stack</th>
              <th>{t("host.state")}</th>
              <th>{t("server.ports")}</th>
              <th>Uptime</th>
              {canAct && <th style={{ textAlign: "right" }}>{t("common.actions")}</th>}
            </tr>
          </thead>
          <tbody>
            {containers.map((container) => {
              const isSystem = protectedRoles.includes(container.system_role);
              const isRunning = container.state === "running";
              const busy = pending[container.id];

              return (
                <tr key={container.id}>
                  <td>
                    <div className="cell-title">{container.name}</div>
                    <div className="cell-sub">{container.image}</div>
                  </td>
                  <td className="muted">
                    {isSystem ? (
                      <StatusPill tone="off" dot={false}>
                        {t("server.system")}
                      </StatusPill>
                    ) : (
                      container.project || "–"
                    )}
                  </td>
                  <td>
                    <ContainerStatePill state={container.state} health={container.health} />
                  </td>
                  <td className="mono small">
                    {(container.ports ?? [])
                      .filter((port) => port.public)
                      .map((port) => `:${port.public}`)
                      .join(" ") || "–"}
                  </td>
                  <td className="muted">
                    {isRunning && parseDate(container.started_at)
                      ? formatDuration((now - parseDate(container.started_at)!.getTime()) / 1000)
                      : "–"}
                  </td>
                  {canAct && (
                    <td style={{ textAlign: "right", whiteSpace: "nowrap" }}>
                      {!isRunning && (
                        <IconButton
                          bare
                          disabled={busy}
                          aria-label={t("server.startContainer", { name: container.name })}
                          onClick={() => void act(container, "container.start", {})}
                        >
                          <Play size={16} aria-hidden="true" />
                        </IconButton>
                      )}
                      {!isSystem && (
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
                              setRemoving(container);
                            }}
                          >
                            <Trash2 size={16} aria-hidden="true" />
                          </IconButton>
                        </>
                      )}
                    </td>
                  )}
                </tr>
              );
            })}
          </tbody>
        </Table>
      )}

      <ConfirmDialog
        open={removing !== null}
        danger
        title={t("server.removeConfirm", { name: removing?.name })}
        confirmLabel={t("server.removeAction")}
        onCancel={() => setRemoving(null)}
        onConfirm={() => {
          if (removing) {
            void act(removing, "container.remove", {
              remove_volumes: removeVolumes,
              force: true,
            });
          }
          setRemoving(null);
        }}
      >
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
          side={t("server.lastHeartbeat", { when: formatRelative(host.last_seen, now) })}
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
