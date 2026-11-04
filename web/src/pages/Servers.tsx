import { Plus, Server } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { PageHead } from "../components/Layout";
import { HostStatusPill } from "../components/status";
import { Card, EmptyState, ProgressBar, Table } from "../components/ui";
import { hasRole, useAuth } from "../lib/auth";
import { formatNumber, formatRelative, formatVersion, percentOf } from "../lib/format";
import { useLiveList, useNow } from "../lib/realtime";
import type { Host } from "../lib/types";

export function ServersPage() {
  const { t } = useTranslation();
  const { user } = useAuth();
  const now = useNow();
  const hosts = useLiveList<Host>("hosts", { sort: "name" });

  const canConnect = hasRole(user, "admin");

  return (
    <>
      <PageHead
        title={t("nav.servers")}
        actions={
          canConnect && (
            <Link to="/agents" className="button primary">
              <Plus size={16} aria-hidden="true" />
              {t("overview.addServer")}
            </Link>
          )
        }
      />

      <Card flush>
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
          <Table label={t("nav.servers")}>
            <thead>
              <tr>
                <th>{t("host.server")}</th>
                <th>{t("host.state")}</th>
                <th>{t("host.roles")}</th>
                <th>CPU</th>
                <th>RAM</th>
                <th>{t("host.disk")}</th>
                <th>{t("server.load1")}</th>
                <th>{t("host.agent")}</th>
                <th>{t("host.lastSeen")}</th>
              </tr>
            </thead>
            <tbody>
              {hosts.items.map((host) => (
                <tr key={host.id}>
                  <td>
                    <Link to={`/servers/${host.id}`} className="cell-title">
                      {host.name}
                    </Link>
                    <div className="cell-sub mono">
                      {[host.ip4, host.os].filter(Boolean).join(" · ")}
                    </div>
                  </td>
                  <td>
                    <HostStatusPill status={host.status} />
                  </td>
                  <td className="muted">
                    {host.roles.map((role) => t(`host.role.${role}`)).join(", ")}
                  </td>
                  <td style={{ minWidth: 90 }}>
                    <span className="num">{formatNumber(host.cpu_percent)} %</span>
                    <ProgressBar
                      value={host.cpu_percent}
                      label={t("host.cpuOf", { name: host.name })}
                    />
                  </td>
                  <td style={{ minWidth: 90 }}>
                    <span className="num">
                      {formatNumber(percentOf(host.mem_used_bytes, host.mem_bytes))} %
                    </span>
                    <ProgressBar
                      value={percentOf(host.mem_used_bytes, host.mem_bytes)}
                      label={t("host.memOf", { name: host.name })}
                    />
                  </td>
                  <td style={{ minWidth: 90 }}>
                    <span className="num">
                      {formatNumber(percentOf(host.disk_used_bytes, host.disk_total_bytes))} %
                    </span>
                    <ProgressBar
                      value={percentOf(host.disk_used_bytes, host.disk_total_bytes)}
                      label={t("host.diskOf", { name: host.name })}
                    />
                  </td>
                  <td className="num">
                    {host.status === "offline" ? "–" : formatNumber(host.load1, 2)}
                  </td>
                  <td className="num">{formatVersion(host.agent_version)}</td>
                  <td className="muted">{formatRelative(host.last_seen, now, { past: true })}</td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Card>
    </>
  );
}
