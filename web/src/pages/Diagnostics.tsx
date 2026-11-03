import { Check, Download, Minus, X } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import { PageHead } from "../components/Layout";
import { useToast } from "../components/toast";
import {
  Button,
  Card,
  EmptyState,
  Kpi,
  SegmentedControl,
  StatusPill,
  Table,
  type Tone,
} from "../components/ui";
import { api, download, followEvents } from "../lib/api";
import {
  formatBytes,
  formatCompact,
  formatDuration,
  formatNumber,
  formatRelative,
  formatSkew,
  formatTime,
  formatVersion,
} from "../lib/format";
import { useLiveList, useNow } from "../lib/realtime";
import { connectionStrip } from "../lib/strip";
import type {
  DiagAgent,
  DiagAgentDetail,
  DiagOverview,
  DiagTraceEntry,
  Host,
  SelfCheck,
} from "../lib/types";

const refreshInterval = 5000;
const traceRows = 200;
const traceFlushInterval = 500;

type TraceFilter = "all" | "command" | "heartbeat" | "error";

export function DiagnosticsPage() {
  const { t } = useTranslation();
  const toast = useToast();
  const now = useNow();

  const [window, setWindow] = useState("15m");
  const [overview, setOverview] = useState<DiagOverview | null>(null);
  const [agents, setAgents] = useState<DiagAgent[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [unavailable, setUnavailable] = useState(false);

  const hosts = useLiveList<Host>("hosts", { sort: "name" });

  useEffect(() => {
    let cancelled = false;

    const load = async () => {
      try {
        const [nextOverview, nextAgents] = await Promise.all([
          api.diagOverview(window),
          api.diagAgents(),
        ]);
        if (!cancelled) {
          setOverview(nextOverview);
          setAgents(nextAgents);
          setUnavailable(false);
        }
      } catch {
        if (!cancelled) {
          setUnavailable(true);
        }
      }
    };

    void load();
    const timer = setInterval(() => void load(), refreshInterval);

    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [window]);

  const names = useMemo(
    () => new Map(hosts.items.map((host) => [host.id, host.name])),
    [hosts.items],
  );

  const current = selected ?? agents[0]?.id ?? null;
  const selectedHost = hosts.items.find((host) => host.id === current) ?? null;

  const bundle = async () => {
    try {
      await download("/api/diag/bundle", "panel-diagnostics.zip");
    } catch (err) {
      toast.error(err);
    }
  };

  if (unavailable) {
    return (
      <>
        <PageHead
          crumbs={`${t("nav.system")} / ${t("nav.diagnostics")}`}
          title={t("nav.diagnostics")}
        />
        <Card>
          <EmptyState title={t("diag.unavailableTitle")}>{t("diag.unavailableText")}</EmptyState>
        </Card>
      </>
    );
  }

  const depth = overview?.queue.depth ?? {};
  const open = (depth.dispatched ?? 0) + (depth.running ?? 0);

  return (
    <>
      <PageHead
        crumbs={`${t("nav.system")} / ${t("nav.diagnostics")}`}
        title={t("nav.diagnostics")}
        description={t("diag.description")}
        actions={
          <>
            <SegmentedControl
              label={t("diag.window")}
              value={window}
              onChange={setWindow}
              options={[
                { value: "15m", label: "15 min" },
                { value: "1h", label: "1 h" },
                { value: "24h", label: "24 h" },
              ]}
            />
            <Button variant="primary" onClick={() => void bundle()}>
              <Download size={16} aria-hidden="true" />
              {t("diag.panelBundle")}
            </Button>
          </>
        }
      />

      {overview && (
        <div className="kpis">
          <Kpi
            label={t("diag.gateway")}
            value={
              <>
                {overview.gateway.agents_connected} <small>/ {hosts.items.length}</small>
              </>
            }
            note={t("diag.agentsConnectedNote")}
          />
          <Kpi
            label={t("diag.messages")}
            value={
              <>
                {formatNumber(overview.messages.per_minute)} <small>/ min</small>
              </>
            }
            note={t("diag.messagesNote")}
          />
          <Kpi
            label={t("diag.queue")}
            value={
              <>
                {depth.queued ?? 0} <small>{t("diag.waiting")}</small> · {open}{" "}
                <small>{t("diag.running")}</small>
              </>
            }
            note={
              <span className={overview.queue.timeouts ? "warn-text" : undefined}>
                {t("diag.queueNote", {
                  timeouts: overview.queue.timeouts,
                  replays: overview.queue.replays,
                })}
              </span>
            }
          />
          <Kpi
            label={t("diag.process")}
            value={
              <>
                <small>uptime</small> {formatDuration(overview.process.uptime_s)}
              </>
            }
            note={t("diag.processNote", {
              goroutines: overview.process.goroutines,
              heap: formatBytes(overview.process.heap_bytes),
              db: formatBytes(overview.process.sqlite_bytes + overview.process.wal_bytes),
            })}
          />
        </div>
      )}

      <Card title={t("diag.connections")} note={t("diag.connectionsNote")} flush>
        {agents.length === 0 ? (
          <EmptyState title={t("diag.noAgents")}>{t("diag.noAgentsText")}</EmptyState>
        ) : (
          <Table label={t("diag.connections")}>
            <thead>
              <tr>
                <th>{t("host.agent")}</th>
                <th>Stream</th>
                <th>{t("diag.connected")}</th>
                <th>RTT p50/p95</th>
                <th>{t("diag.skew")}</th>
                <th>{t("diag.messagesInOut")}</th>
                <th>{t("diag.reconnects")}</th>
                <th>{t("diag.lastError")}</th>
                <th>{t("diag.cert")}</th>
              </tr>
            </thead>
            <tbody>
              {agents.map((agent) => (
                <AgentRow
                  key={agent.id}
                  agent={agent}
                  name={names.get(agent.id) ?? agent.id}
                  selected={agent.id === current}
                  now={now}
                  onSelect={() => setSelected(agent.id)}
                />
              ))}
            </tbody>
          </Table>
        )}
      </Card>

      {current && (
        <AgentDetailView
          agentId={current}
          name={names.get(current) ?? current}
          host={selectedHost}
          now={now}
        />
      )}

      {current && <Trace agentId={current} name={names.get(current) ?? current} />}

      {overview && <Internals overview={overview} />}
    </>
  );
}

function streamTone(agent: DiagAgent): Tone {
  if (!agent.connected) {
    return "err";
  }
  return agent.reconnects >= 5 ? "warn" : "ok";
}

function AgentRow({
  agent,
  name,
  selected,
  now,
  onSelect,
}: {
  agent: DiagAgent;
  name: string;
  selected: boolean;
  now: number;
  onSelect: () => void;
}) {
  const { t } = useTranslation();
  const tone = streamTone(agent);

  const certDays = agent.cert_expiry
    ? Math.round((new Date(agent.cert_expiry).getTime() - now) / 86400000)
    : null;

  return (
    <tr
      className={`clickable${selected ? " selected" : ""}`}
      onClick={onSelect}
      aria-selected={selected}
    >
      <td>
        <button
          type="button"
          className="mono"
          style={{
            background: "none",
            border: 0,
            color: selected ? "var(--accent)" : "var(--text)",
            cursor: "pointer",
            padding: 0,
            font: "inherit",
          }}
          onClick={onSelect}
        >
          {name}
        </button>
      </td>
      <td>
        <StatusPill tone={tone}>
          {tone === "ok"
            ? t("diag.streamOpen")
            : tone === "warn"
              ? t("diag.streamUnstable")
              : t("host.status.offline")}
        </StatusPill>
      </td>
      <td className="muted">
        {agent.connected ? formatRelative(agent.connected_since, now) : "–"}
      </td>
      <td className="num">
        {agent.connected
          ? `${formatNumber(agent.rtt_p50_ms, 1)} / ${formatNumber(agent.rtt_p95_ms, 1)} ms`
          : "–"}
      </td>
      <td className="num">{agent.connected ? formatSkew(agent.clock_skew_ms) : "–"}</td>
      <td className="num">
        {formatCompact(agent.messages_in)} / {formatCompact(agent.messages_out)}
      </td>
      <td className="num">{agent.reconnects}</td>
      <td className={agent.connected ? "muted" : "err-text"} style={{ maxWidth: 280 }}>
        {agent.last_error || agent.last_reason || "–"}
      </td>
      <td className="muted">{certDays === null ? "–" : t("diag.days", { count: certDays })}</td>
    </tr>
  );
}

function checkIcon(check: SelfCheck) {
  if (check.skipped) {
    return <Minus size={14} aria-hidden="true" />;
  }
  return check.ok ? <Check size={14} aria-hidden="true" /> : <X size={14} aria-hidden="true" />;
}

function AgentDetailView({
  agentId,
  name,
  host,
  now,
}: {
  agentId: string;
  name: string;
  host: Host | null;
  now: number;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const [detail, setDetail] = useState<DiagAgentDetail | null>(null);

  useEffect(() => {
    let cancelled = false;

    const load = () => {
      api
        .diagAgent(agentId)
        .then((loaded) => {
          if (!cancelled) {
            setDetail(loaded);
          }
        })
        .catch(() => {
          // the table above already shows what is known
        });
    };

    load();
    const timer = setInterval(load, refreshInterval);

    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [agentId]);

  const run = async (type: string, payload: object, message: string) => {
    try {
      await api.runCommand(agentId, type, payload, name);
      toast.success(message);
    } catch (err) {
      toast.error(err);
    }
  };

  const events = detail?.events ?? [];
  const cells = connectionStrip(events, now);

  return (
    <div className="columns">
      <div className="column-wide">
        <Card
          title={t("diag.connectionOf", { name })}
          side={
            <>
              <span>
                <i className="status-dot ok" aria-hidden="true" /> {t("diag.legendConnected")}
              </span>
              <span>
                <i className="status-dot warn" aria-hidden="true" /> {t("diag.legendDrops")}
              </span>
              <span>
                <i className="status-dot err" aria-hidden="true" /> {t("diag.legendDown")}
              </span>
            </>
          }
        >
          <div className="strip" role="img" aria-label={t("diag.stripLabel", { name })}>
            {cells.map((cell, index) => (
              <span key={index} className={cell} />
            ))}
          </div>
          <div className="strip-axis" aria-hidden="true">
            <span>{t("diag.ago24h")}</span>
            <span>{t("diag.ago12h")}</span>
            <span>{t("diag.nowLabel")}</span>
          </div>
          {events.length === 0 ? (
            <p className="muted" style={{ marginTop: 14 }}>
              {t("diag.noConnectionEvents")}
            </p>
          ) : (
            <ul className="list" style={{ marginTop: 10 }}>
              {[...events]
                .reverse()
                .slice(0, 8)
                .map((event, index) => (
                  <li
                    key={index}
                    style={{ display: "flex", gap: 12, padding: "8px 0", alignItems: "center" }}
                  >
                    <span
                      className={`status-dot ${event.kind === "connected" ? "ok" : event.reason === "eof" ? "warn" : "err"}`}
                      aria-hidden="true"
                    />
                    <span className="mono muted small">{formatTime(event.at)}</span>
                    <span>
                      {event.kind === "connected"
                        ? t("diag.eventConnected", { remote: event.remote })
                        : t("diag.eventDisconnected", {
                            reason: t(`diag.reason.${event.reason}`, event.reason ?? ""),
                          })}
                    </span>
                  </li>
                ))}
            </ul>
          )}
        </Card>
      </div>

      <div className="column-side">
        <Card
          title={t("diag.agentState")}
          side={
            host
              ? t("server.lastHeartbeat", { when: formatRelative(host.last_seen, now) })
              : undefined
          }
        >
          {host?.runtime ? (
            <dl className="facts">
              <dt>{t("server.agentVersion")}</dt>
              <dd className="mono">{formatVersion(host.agent_version)}</dd>
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
          ) : (
            <p className="muted">{t("diag.noRuntime")}</p>
          )}

          {(host?.self_checks ?? []).length > 0 && (
            <>
              <div className="nav-heading" style={{ padding: "14px 0 6px" }}>
                {t("diag.checks")}
              </div>
              <ul className="list">
                {(host?.self_checks ?? []).map((check) => (
                  <li
                    key={check.name}
                    style={{
                      padding: "6px 0",
                      border: 0,
                      display: "flex",
                      gap: 10,
                      alignItems: "center",
                    }}
                  >
                    <span
                      className={`notice-mark ${check.skipped ? "" : check.ok ? "info" : "err"}`}
                      style={{ color: check.ok && !check.skipped ? "var(--ok)" : undefined }}
                    >
                      {checkIcon(check)}
                    </span>
                    <span style={{ flex: 1, minWidth: 0 }}>
                      {t(`server.check.${check.name}`, check.name)}
                      <span className="visually-hidden">
                        {": "}
                        {check.skipped
                          ? t("server.checkSkipped")
                          : check.ok
                            ? "OK"
                            : t("server.checkFailed")}
                      </span>
                      <div className={`small ${check.ok ? "faint" : "err-text"}`}>
                        {check.detail}
                      </div>
                    </span>
                  </li>
                ))}
              </ul>
            </>
          )}

          <div style={{ display: "flex", flexWrap: "wrap", gap: 8, marginTop: 14 }}>
            <Button
              onClick={() =>
                void run(
                  "agent.set_log_level",
                  { level: "debug", duration_s: 900 },
                  t("diag.debugQueued"),
                )
              }
            >
              {t("diag.debugLogging")}
            </Button>
            <Button
              onClick={() =>
                void run("agent.diagnostics", { log_lines: 1000 }, t("server.diagnosticsQueued"))
              }
            >
              {t("diag.agentBundle")}
            </Button>
          </div>
        </Card>
      </div>
    </div>
  );
}

function Trace({ agentId, name }: { agentId: string; name: string }) {
  const { t } = useTranslation();
  const toast = useToast();

  const [entries, setEntries] = useState<DiagTraceEntry[]>([]);
  const [filter, setFilter] = useState<TraceFilter>("all");
  const [live, setLive] = useState(false);

  const buffer = useRef<DiagTraceEntry[]>([]);

  // the rate of screen updates is capped, and the list holds a fixed number of
  // rows, so a chatty agent cannot freeze the page
  useEffect(() => {
    setEntries([]);
    buffer.current = [];

    const controller = new AbortController();

    // start with what the panel still remembers, then go on with the live messages
    api
      .diagAgent(agentId)
      .then((detail) => {
        if (!controller.signal.aborted) {
          setEntries((current) =>
            current.length > 0 ? current : [...(detail.trace ?? [])].reverse().slice(0, traceRows),
          );
        }
      })
      .catch(() => undefined);

    setLive(true);
    followEvents<DiagTraceEntry>(
      `/api/diag/trace?agent=${encodeURIComponent(agentId)}`,
      (entry) => buffer.current.push(entry),
      controller.signal,
    )
      .catch(() => undefined)
      .finally(() => setLive(false));

    const flush = setInterval(() => {
      if (buffer.current.length === 0) {
        return;
      }
      const incoming = buffer.current;
      buffer.current = [];
      setEntries((current) => [...incoming.reverse(), ...current].slice(0, traceRows));
    }, traceFlushInterval);

    return () => {
      controller.abort();
      clearInterval(flush);
    };
  }, [agentId]);

  const visible = entries.filter((entry) => filter === "all" || entry.kind === filter);

  const capture = async () => {
    try {
      await api.diagCapture(agentId);
      toast.success(t("diag.captureStarted"));
    } catch (err) {
      toast.error(err);
    }
  };

  return (
    <Card
      title={t("diag.traceOf", { name })}
      note={
        <StatusPill tone={live ? "ok" : "off"}>
          {live ? t("diag.live") : t("diag.notLive")}
        </StatusPill>
      }
      side={
        <>
          <SegmentedControl
            label={t("diag.traceFilter")}
            value={filter}
            onChange={setFilter}
            options={[
              { value: "all", label: t("diag.traceAll") },
              { value: "command", label: t("diag.traceCommands") },
              { value: "heartbeat", label: "Heartbeat" },
              { value: "error", label: t("diag.traceErrors") },
            ]}
          />
          <Button onClick={() => void capture()}>{t("diag.capture")}</Button>
        </>
      }
      flush
    >
      <div className="trace" tabIndex={0} role="region" aria-label={t("diag.traceOf", { name })}>
        <table className="table">
          <thead>
            <tr>
              <th>{t("diag.time")}</th>
              <th>{t("diag.direction")}</th>
              <th>RPC</th>
              <th>{t("diag.detail")}</th>
              <th>{t("diag.size")}</th>
              <th>{t("host.state")}</th>
            </tr>
          </thead>
          <tbody>
            {visible.map((entry, index) => (
              <tr key={`${entry.at}-${index}`}>
                <td className="mono muted small">{formatTime(entry.at)}</td>
                <td className="mono">{entry.direction === "in" ? "←" : "→"}</td>
                <td className="mono">{entry.method}</td>
                <td className="small" style={{ overflowWrap: "anywhere", maxWidth: 420 }}>
                  {entry.error || entry.subject || "–"}
                  {entry.payload && <div className="mono faint">{entry.payload}</div>}
                </td>
                <td className="num small">{entry.bytes ? formatBytes(entry.bytes) : "–"}</td>
                <td>
                  <StatusPill tone={entry.code ? "err" : "ok"} dot={false}>
                    {entry.code || "OK"}
                  </StatusPill>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {visible.length === 0 && <EmptyState title={t("diag.traceEmpty")} />}
      </div>
      <div className="card-body muted small" style={{ borderTop: "1px solid var(--border)" }}>
        {t("diag.traceNote")}
      </div>
    </Card>
  );
}

function Internals({ overview }: { overview: DiagOverview }) {
  const { t } = useTranslation();

  return (
    <div className="columns">
      <div className="column-wide">
        <Card title={t("diag.internals")} note={t("diag.internalsNote")} flush>
          <ul className="list">
            {(overview.cron ?? []).map((job) => (
              <li key={job.id} className="list-row">
                <span className="mono">{job.id}</span>
                <span className="mono muted small">{job.expression}</span>
              </li>
            ))}
          </ul>
          <div className="card-body">
            <dl className="stat-row" style={{ borderTop: 0, margin: 0, padding: 0 }}>
              <div>
                <dt>{t("diag.relays")}</dt>
                <dd>{overview.process.streams}</dd>
              </div>
              <div>
                <dt>{t("diag.realtimeClients")}</dt>
                <dd>{overview.process.realtime_clients}</dd>
              </div>
              <div>
                <dt>SQLite WAL</dt>
                <dd>{formatBytes(overview.process.wal_bytes)}</dd>
              </div>
              <div>
                <dt>{t("diag.oldestQueued")}</dt>
                <dd>{formatDuration(overview.queue.oldest_queued_ms / 1000)}</dd>
              </div>
            </dl>
          </div>
        </Card>
      </div>
      <div className="column-side">
        <Card title={t("diag.recentErrors")} flush>
          {(overview.errors ?? []).length === 0 ? (
            <EmptyState title={t("diag.noErrors")} />
          ) : (
            <ul className="list">
              {[...(overview.errors ?? [])]
                .reverse()
                .slice(0, 8)
                .map((error, index) => (
                  <li key={index}>
                    <div className="mono muted small">{formatTime(error.at)}</div>
                    <div className="mono err-text small" style={{ overflowWrap: "anywhere" }}>
                      {error.message}
                      {error.detail && `: ${error.detail}`}
                    </div>
                  </li>
                ))}
            </ul>
          )}
        </Card>
      </div>
    </div>
  );
}
