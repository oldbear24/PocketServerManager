import {
  Activity,
  Archive,
  Boxes,
  Database,
  Gauge,
  Globe,
  History,
  Layers,
  LayoutDashboard,
  Lock,
  LogOut,
  Server,
  Settings,
  SquarePlus,
  type LucideIcon,
} from "lucide-react";
import { useEffect, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Link, NavLink, Outlet } from "react-router-dom";

import { AlertsProvider, openAlertsQuery, sectionAlertsQuery } from "../lib/alerts";
import { api } from "../lib/api";
import { backupAlertRules, domainAlertRules } from "../lib/events";
import { hasRole, useAuth } from "../lib/auth";
import type { Database as DatabaseRecord } from "../lib/databases";
import { HostsProvider, hostSummaryFields, type HostSummary } from "../lib/hosts";
import type { Role } from "../lib/pb";
import { useLiveList } from "../lib/realtime";
import type { Site } from "../lib/sites";
import type { Alert, Container, PanelInfo } from "../lib/types";
import { IconButton } from "./ui";

type NavItem = {
  to: string;
  label: string;
  icon: LucideIcon;
  end?: boolean;
  role?: Role;
  count?: number;
  dot?: "err" | "warn";
  /** what the dot means, for a screen reader */
  dotLabel?: string;
};

function Item({ item }: { item: NavItem }) {
  const Icon = item.icon;
  return (
    <NavLink
      to={item.to}
      end={item.end}
      className={({ isActive }) => `nav-link${isActive ? " active" : ""}`}
    >
      <Icon size={18} className="icon" aria-hidden="true" />
      <span>{item.label}</span>
      {item.count !== undefined && <span className="nav-count">{item.count}</span>}
      {item.dot && (
        <>
          <span className={`nav-dot status-dot ${item.dot}`} aria-hidden="true" />
          <span className="visually-hidden">{item.dotLabel}</span>
        </>
      )}
    </NavLink>
  );
}

/** A section's dot from its open alerts, as sectionAlertsQuery reads them
 * (the most severe first) with the panel's count of all: err when one is
 * critical, else warn, and none without an alert. */
export function alertDot(
  alerts: Pick<Alert, "severity">[],
  total: number,
): "err" | "warn" | undefined {
  if (alerts.some((alert) => alert.severity === "critical")) {
    return "err";
  }
  return total > 0 ? "warn" : undefined;
}

const domainAlertsQuery = sectionAlertsQuery(domainAlertRules);
const backupAlertsQuery = sectionAlertsQuery(backupAlertRules);

function Section({ heading, items }: { heading?: string; items: NavItem[] }) {
  return (
    <div className="nav-section">
      {heading && <div className="nav-heading">{heading}</div>}
      <div className="nav-items">
        {items.map((item) => (
          <Item key={item.to} item={item} />
        ))}
      </div>
    </div>
  );
}

function GatewayCard({ hosts }: { hosts: Pick<HostSummary, "stream_open">[] }) {
  const { t } = useTranslation();
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
        // the card only stays empty; the page itself reports what is wrong
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (!info) {
    return null;
  }

  const connected = hosts.filter((host) => host.stream_open).length;

  return (
    <div className="gateway-card">
      <strong>
        <span className={`status-dot ${info.gateway_running ? "ok" : "err"}`} aria-hidden="true" />
        {info.gateway_running ? t("nav.gatewayRunning") : t("nav.gatewayStopped")}
      </strong>
      <div className="mono">{info.grpc_address} · mTLS</div>
      <div>{t("nav.agentsConnected", { connected, total: hosts.length })}</div>
    </div>
  );
}

export function Layout() {
  const { t } = useTranslation();
  const { user, signOut } = useAuth();

  // the one list of the servers, which the pages share (#33): a heartbeat
  // brings only what the sidebar and the Overview show
  const hosts = useLiveList<HostSummary>("hosts", { sort: "name", fields: hostSummaryFields });
  // the counts: no filter and no sort, so an update cannot change them, and
  // only a create or a delete reads the count again
  const containers = useLiveList<Container>("containers", { perPage: 1, inPlace: true });
  // a restore's or a restore test's temporary copy is no database of its own (#53)
  const databases = useLiveList<DatabaseRecord>("databases", {
    filter: "temporary_until = ''",
    perPage: 1,
  });
  const sites = useLiveList<Site>("sites", { perPage: 1, inPlace: true });
  // the one list of the open alerts, which the pages share (#33), so that a
  // page reads none of its own: with the two below, three reads with a
  // filter, where a fourth that finds nothing PocketBase holds up
  const alerts = useLiveList<Alert>("alerts", openAlertsQuery);
  // a failing certificate or DNS provider marks Domains & TLS (#27), and a
  // failed backup or a repository with errors marks Backups (#31): each from
  // its own read, as the newest 200 of all may not hold their alerts
  const domainAlerts = useLiveList<Alert>("alerts", domainAlertsQuery);
  const domainsDot = alertDot(domainAlerts.items, domainAlerts.total);
  const backupAlerts = useLiveList<Alert>("alerts", backupAlertsQuery);
  const backupsDot = alertDot(backupAlerts.items, backupAlerts.total);

  const overview: NavItem[] = [
    { to: "/", label: t("nav.overview"), icon: LayoutDashboard, end: true },
  ];

  const infrastructure: NavItem[] = [
    { to: "/servers", label: t("nav.servers"), icon: Server, count: hosts.total },
    { to: "/stacks", label: t("nav.stacks"), icon: Layers, count: containers.total },
    { to: "/databases", label: t("nav.databases"), icon: Database, count: databases.total },
    {
      to: "/backups",
      label: t("nav.backups"),
      icon: Archive,
      dot: backupsDot,
      dotLabel: t("nav.backupsAlert", { count: backupAlerts.total }),
    },
  ];

  const web: NavItem[] = [
    { to: "/sites", label: t("nav.sites"), icon: Globe, count: sites.total },
    {
      to: "/domains",
      label: t("nav.domains"),
      icon: Lock,
      dot: domainsDot,
      dotLabel: t("nav.domainsAlert", { count: domainAlerts.total }),
    },
  ];

  const system: NavItem[] = (
    [
      { to: "/agents", label: t("nav.agents"), icon: SquarePlus, role: "admin" },
      { to: "/audit", label: t("nav.audit"), icon: History, role: "operator" },
      { to: "/diagnostics", label: t("nav.diagnostics"), icon: Gauge, role: "admin" },
      { to: "/settings", label: t("nav.settings"), icon: Settings },
    ] as NavItem[]
  ).filter((item) => !item.role || hasRole(user, item.role));

  return (
    <div className="app">
      <nav className="sidebar" aria-label={t("nav.label")}>
        <Link to="/" className="brand">
          <span className="brand-mark" aria-hidden="true">
            <Boxes size={18} />
          </span>
          PocketCloud
        </Link>

        <Section items={overview} />
        <Section heading={t("nav.infrastructure")} items={infrastructure} />
        <Section heading={t("nav.web")} items={web} />
        <Section heading={t("nav.system")} items={system} />

        <GatewayCard hosts={hosts.items} />

        {alerts.total > 0 && (
          <Link to="/" className="account">
            <Activity size={14} aria-hidden="true" />
            <span>{t("nav.openAlerts", { count: alerts.total })}</span>
          </Link>
        )}

        <div className="account">
          <span title={user?.email}>{user?.name || user?.email}</span>
          <IconButton bare aria-label={t("auth.signOut")} onClick={signOut}>
            <LogOut size={16} aria-hidden="true" />
          </IconButton>
        </div>
      </nav>

      <main className="main">
        <HostsProvider value={hosts}>
          <AlertsProvider value={alerts}>
            <Outlet />
          </AlertsProvider>
        </HostsProvider>
      </main>
    </div>
  );
}

export function PageHead({
  crumbs,
  title,
  titleExtra,
  description,
  actions,
}: {
  crumbs?: ReactNode;
  title: ReactNode;
  titleExtra?: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <header className="page-head">
      <div className="page-head-text">
        {crumbs && <div className="breadcrumbs">{crumbs}</div>}
        <div className="page-title-row">
          <h1 className="page-title">{title}</h1>
          {titleExtra}
        </div>
        {description && <p className="muted">{description}</p>}
      </div>
      {actions && <div className="page-actions">{actions}</div>}
    </header>
  );
}
