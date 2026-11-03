import { AlertTriangle, Plus, Server } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { PageHead } from "../components/Layout";
import { CommandStatusPill, HostStatusPill } from "../components/status";
import { Card, EmptyState, Kpi, ProgressBar, Table } from "../components/ui";
import { hasRole, useAuth } from "../lib/auth";
import { alertText } from "../lib/events";
import {
  formatDay,
  formatNumber,
  formatRelative,
  formatTime,
  formatVersion,
  percentOf,
} from "../lib/format";
import { useLiveList, useNow } from "../lib/realtime";
import type { Alert, Command, Container, Host } from "../lib/types";

function Usage({ value, label }: { value: number; label: string }) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 4, minWidth: 80 }}>
      <span className="num">{formatNumber(value)} %</span>
      <ProgressBar value={value} label={label} warnAt={80} errAt={90} />
    </div>
  );
}

function HostRow({ host, containers, now }: { host: Host; containers: number; now: number }) {
  const { t } = useTranslation();
  const offline = host.status === "offline";

  return (
    <tr>
      <td>
        <Link to={`/servers/${host.id}`} className="cell-title" style={{ color: "var(--text)" }}>
          {host.name}
        </Link>
        <div className="cell-sub mono">
          {[host.ip4, host.os].filter(Boolean).join(" · ") || t("common.unknown")}
        </div>
      </td>
      <td>
        <HostStatusPill status={host.status} />
      </td>
      {offline ? (
        <td colSpan={3} className="faint">
          –
        </td>
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
      <td className="num">{containers}</td>
      <td className="num" style={{ textAlign: "right" }}>
        <div>{formatVersion(host.agent_version)}</div>
        <div className="cell-sub">
          {offline
            ? formatRelative(host.last_seen, now)
            : host.rtt_ms
              ? `${formatNumber(host.rtt_ms)} ms`
              : "–"}
        </div>
      </td>
    </tr>
  );
}

export function OverviewPage() {
  const { t } = useTranslation();
  const { user } = useAuth();
  const now = useNow();

  const hosts = useLiveList<Host>("hosts", { sort: "name" });
  const containers = useLiveList<Container>("containers", { perPage: 500 });
  const alerts = useLiveList<Alert>("alerts", {
    filter: "status != 'resolved'",
    sort: "-last_seen",
    perPage: 20,
  });
  // opening a log or a terminal is a command too, but no one is waiting in a queue for it
  const commands = useLiveList<Command>("commands", {
    filter: "type !~ 'logs.%' && type !~ 'exec.%'",
    sort: "-created",
    perPage: 8,
  });

  const online = hosts.items.filter((host) => host.status !== "offline" && host.status !== "");
  const offline = hosts.items.filter((host) => host.status === "offline");
  const running = containers.items.filter((container) => container.state === "running");
  const restarting = containers.items.filter((container) => container.state === "restarting");
  const stopped = containers.items.filter(
    (container) => container.state !== "running" && container.state !== "restarting",
  );

  const hostNames = new Map(hosts.items.map((host) => [host.id, host.name]));

  const perHost = new Map<string, number>();
  for (const container of containers.items) {
    perHost.set(container.host, (perHost.get(container.host) ?? 0) + 1);
  }

  const canConnect = hasRole(user, "admin");

  return (
    <>
      <PageHead
        crumbs={formatDay(new Date(now).toISOString())}
        title={t("overview.title")}
        actions={
          canConnect && (
            <Link to="/agents" className="button primary">
              <Plus size={16} aria-hidden="true" />
              {t("overview.addServer")}
            </Link>
          )
        }
      />

      <div className="kpis">
        <Kpi
          label={t("overview.agents")}
          hint="gRPC"
          value={
            <>
              {online.length} <small>/ {hosts.items.length}</small>
            </>
          }
          note={
            offline.length > 0 ? (
              <span className="err-text">
                {t("overview.agentOffline", {
                  name: offline[0].name,
                  when: formatRelative(offline[0].last_seen, now),
                })}
              </span>
            ) : hosts.items.length === 0 ? (
              t("overview.noServersTitle")
            ) : (
              t("overview.allConnected")
            )
          }
        />
        <Kpi
          label={t("overview.containers")}
          value={
            <>
              {running.length} <small>{t("overview.running")}</small>
            </>
          }
          note={
            <>
              {t("overview.stopped", { count: stopped.length })}
              {restarting.length > 0 && (
                <span className="warn-text">
                  {" · "}
                  {t("overview.restarting", { count: restarting.length })}
                </span>
              )}
            </>
          }
        />
        <Kpi
          label={t("overview.attention")}
          value={alerts.total}
          note={alerts.total ? t("overview.openAlerts") : t("overview.noAlerts")}
        />
      </div>

      <div className="columns">
        <div className="column-wide">
          <Card
            title={t("overview.servers")}
            note={t("overview.serversNote")}
            flush
            side={canConnect && <Link to="/agents">{t("overview.connectMore")}</Link>}
          >
            {hosts.items.length === 0 && !hosts.loading ? (
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
            ) : (
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
                  {hosts.items.map((host) => (
                    <HostRow
                      key={host.id}
                      host={host}
                      containers={perHost.get(host.id) ?? 0}
                      now={now}
                    />
                  ))}
                </tbody>
              </Table>
            )}
          </Card>
        </div>

        <div className="column-side">
          <Card
            title={t("overview.attention")}
            side={t("overview.openCount", { count: alerts.total })}
            flush
          >
            {alerts.items.length === 0 ? (
              <EmptyState title={t("overview.noAlerts")} />
            ) : (
              <ul className="list">
                {alerts.items.map((alert) => (
                  <li key={alert.id}>
                    <div style={{ display: "flex", gap: 12 }}>
                      <span
                        className={`notice-mark ${alert.severity === "critical" ? "err" : "warn"}`}
                        aria-hidden="true"
                      >
                        <AlertTriangle size={14} />
                      </span>
                      <div style={{ minWidth: 0 }}>
                        <div style={{ fontWeight: 500 }}>
                          {alert.host ? (
                            <Link to={`/servers/${alert.host}`} style={{ color: "var(--text)" }}>
                              {alertText(t, alert, hostNames.get(alert.host))}
                            </Link>
                          ) : (
                            alertText(t, alert)
                          )}
                        </div>
                        <div className="cell-sub">
                          {t("overview.since", { when: formatRelative(alert.first_seen, now) })}
                        </div>
                      </div>
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </Card>

          <Card
            title={t("overview.queue")}
            side={<Link to="/audit/commands">{t("overview.history")}</Link>}
            flush
          >
            {commands.items.length === 0 ? (
              <EmptyState title={t("overview.noCommands")} />
            ) : (
              <ul className="list">
                {commands.items.map((command) => (
                  <li key={command.id} className="list-row">
                    <div>
                      <div className="mono">{command.type}</div>
                      <div className="cell-sub">
                        {[command.label, command.host_name, formatTime(command.created)]
                          .filter(Boolean)
                          .join(" · ")}
                      </div>
                    </div>
                    <CommandStatusPill status={command.status} />
                  </li>
                ))}
              </ul>
            )}
          </Card>
        </div>
      </div>
    </>
  );
}
