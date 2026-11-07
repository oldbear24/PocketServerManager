import { ExternalLink, Globe, Plus, RotateCcw, Trash2, Upload } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, Navigate, useNavigate, useParams } from "react-router-dom";

import { CommandOutput } from "../components/CommandOutput";
import { DnsPill } from "../components/DomainDns";
import { PageHead } from "../components/Layout";
import { RequestBars } from "../components/RequestBars";
import { useDomainRoutes, type DomainRoutes } from "../components/StackDomains";
import { useToast } from "../components/toast";
import {
  Banner,
  Button,
  Card,
  ConfirmDialog,
  EmptyState,
  Field,
  IconButton,
  SegmentedControl,
  StatusPill,
  Table,
  type Tone,
} from "../components/ui";
import { errorMessage } from "../lib/api";
import { hasRole, useAuth } from "../lib/auth";
import type { Domain } from "../lib/edge";
import {
  formatBytes,
  formatDuration,
  formatNumber,
  formatRelative,
  parseDate,
} from "../lib/format";
import { pb } from "../lib/pb";
import { useLiveList, useLiveRecord, useNow } from "../lib/realtime";
import {
  generators,
  shortCommit,
  sitesApi,
  type Deployment,
  type Generator,
  type Site,
  type SiteRequests,
  type SiteSource,
} from "../lib/sites";
import type { Host } from "../lib/types";
import { tlsText } from "./EdgeProxy";
import {
  PhpDatabaseCard,
  PhpEnvCard,
  PhpKpis,
  PhpLogCard,
  PhpRuntimeCard,
  PhpSharedCard,
  usePhpSite,
  type OnApplied,
} from "./SitePhp";

/** The errors of a record the API refused, by field. */
export function fieldErrors(reason: unknown): Record<string, string> {
  const data = (reason as { response?: { data?: Record<string, { message?: string }> } }).response
    ?.data;
  const fields: Record<string, string> = {};
  for (const [key, value] of Object.entries(data ?? {})) {
    if (value?.message) {
      fields[key] = value.message;
    }
  }
  return Object.keys(fields).length > 0 ? fields : { form: errorMessage(reason) };
}

/** Static sites (#21): every site has its chip; the page of one shows its
 * deployments, its domains and how it is built and served. */
export function SitesPage() {
  const { t } = useTranslation();
  const { id } = useParams();
  const { user } = useAuth();
  const canAct = hasRole(user, "operator");
  const sites = useLiveList<Site>("sites", { sort: "name" });
  const hosts = useLiveList<Host>("hosts", { sort: "name" });
  const navigate = useNavigate();
  const create = () => navigate("/sites/new");

  if (!id && sites.items.length > 0) {
    return <Navigate to={`/sites/${sites.items[0].id}`} replace />;
  }
  const site = sites.items.find((item) => item.id === id);
  const host = hosts.items.find((item) => item.id === site?.host);

  return (
    <>
      <nav className="site-chips" aria-label={t("site.list")}>
        {sites.items.map((item) => (
          <Link
            key={item.id}
            to={`/sites/${item.id}`}
            className={`site-chip${item.id === id ? " active" : ""}`}
            aria-current={item.id === id ? "page" : undefined}
          >
            <span className={`dot ${item.active_deployment ? "ok" : "off"}`} aria-hidden="true" />
            <span className="mono">{item.name}</span>
          </Link>
        ))}
        {canAct && (
          <button type="button" className="site-chip" onClick={create}>
            <Plus size={13} aria-hidden="true" />
            {t("site.new")}
          </button>
        )}
      </nav>

      {sites.loading && sites.items.length === 0 ? (
        <p className="muted">{t("common.loading")}</p>
      ) : sites.items.length === 0 ? (
        <Card>
          <EmptyState
            icon={Globe}
            title={t("site.none")}
            action={
              canAct && (
                <Button variant="primary" onClick={create}>
                  {t("site.new")}
                </Button>
              )
            }
          >
            {t("site.noneText")}
          </EmptyState>
        </Card>
      ) : !site ? (
        <Banner tone="warn">{t("site.notFound")}</Banner>
      ) : (
        <SiteDetail key={site.id} site={site} host={host} canAct={canAct} />
      )}
    </>
  );
}

function SiteDetail({ site, host, canAct }: { site: Site; host?: Host; canAct: boolean }) {
  const { t } = useTranslation();
  const toast = useToast();
  const recent = useLiveList<Deployment>("deployments", {
    filter: pb.filter("site = {:site}", { site: site.id }),
    sort: "-number",
    perPage: 30,
  });
  // releases to go back to, however many deployments came after them
  const onServer = useLiveList<Deployment>("deployments", {
    filter: pb.filter("site = {:site} && on_server = true", { site: site.id }),
    sort: "-number",
    perPage: 60,
  });
  const uploads = useLiveList<Deployment>("deployments", {
    filter: pb.filter("site = {:site} && upload != ''", { site: site.id }),
    perPage: 1,
  });
  const { record: liveActive } = useLiveRecord<Deployment>(
    "deployments",
    site.active_deployment || undefined,
  );
  const deployments = {
    items: [
      ...recent.items,
      ...onServer.items.filter((item) => !recent.items.some((r) => r.id === item.id)),
    ].sort((a, b) => b.number - a.number),
  };
  const domains = useLiveList<Domain>("domains", {
    filter: pb.filter("site = {:site}", { site: site.id }),
    sort: "hostname",
  });
  const { record: liveHost } = useLiveRecord<Host>("hosts", site.host);
  const server = liveHost ?? host;
  const routes = useDomainRoutes(
    site.host,
    [server?.edge_digest, server?.edge_applied_digest].join("|"),
    domains.items,
  );
  const [requests, setRequests] = useState<SiteRequests | null>(null);
  const [watching, setWatching] = useState<{ command: string; title: string } | null>(null);
  // a PHP site's runtime, database and status (#50)
  const php = site.type === "php";
  const phpData = usePhpSite(site, php);
  const onApplied: OnApplied = (applied, title) => {
    if (applied.command) {
      setWatching({ command: applied.command, title });
    }
  };
  const [busy, setBusy] = useState(false);
  const upload = useRef<HTMLInputElement>(null);

  useEffect(() => {
    let cancelled = false;
    const load = () =>
      sitesApi
        .requests(site.id)
        .then((answer) => !cancelled && setRequests(answer))
        .catch(() => {
          // the chart stays empty
        });
    void load();
    const timer = window.setInterval(() => void load(), 5 * 60_000);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [site.id]);

  const active =
    deployments.items.find((item) => item.id === site.active_deployment) ??
    (liveActive?.id === site.active_deployment ? liveActive : undefined);
  const first = domains.items[0];
  const firstRoute = first ? routes.routes.get(first.id) : undefined;
  const secure = firstRoute && ["valid", "expiring", "installing"].includes(firstRoute.tls);
  const generator = active?.generator || site.generator;

  const run = async (action: () => Promise<{ command: string; title: string } | null>) => {
    setBusy(true);
    try {
      const started = await action();
      if (started?.command) {
        setWatching(started);
      }
    } catch (reason) {
      toast.error(reason);
    } finally {
      setBusy(false);
    }
  };
  const deployAgain = () =>
    run(async () => {
      const deployment = await sitesApi.deploy(site.id);
      toast.success(t("site.deployStarted", { number: deployment.number }));
      return {
        command: deployment.command,
        title: t("site.deploymentTitle", { name: site.name, number: deployment.number }),
      };
    });
  const uploadZip = (file: File) =>
    run(async () => {
      const deployment = await sitesApi.upload(site.id, file);
      toast.success(t("site.deployStarted", { number: deployment.number }));
      return {
        command: deployment.command,
        title: t("site.deploymentTitle", { name: site.name, number: deployment.number }),
      };
    });
  const rollback = (deployment: Deployment) =>
    run(async () => {
      const { command } = await sitesApi.rollback(site.id, deployment.id);
      toast.success(t("site.rollbackStarted", { number: deployment.number }));
      return {
        command,
        title: t("site.rollbackTitle", { name: site.name, number: deployment.number }),
      };
    });

  return (
    <>
      <PageHead
        crumbs={
          <>
            <Link to="/sites">{t("nav.sites")}</Link>
            {server && (
              <>
                {" / "}
                <Link to={`/servers/${server.id}/proxy`}>{server.name}</Link>
              </>
            )}
          </>
        }
        title={site.name}
        titleExtra={
          <>
            {server && !site.active_deployment ? (
              <StatusPill tone="off">{t("site.notLive")}</StatusPill>
            ) : server?.status === "online" ? (
              <StatusPill tone="ok">{t("site.online")}</StatusPill>
            ) : server?.status === "degraded" ? (
              <StatusPill tone="warn">{t("site.serverDegraded")}</StatusPill>
            ) : server ? (
              <StatusPill tone="err">{t("site.serverOffline")}</StatusPill>
            ) : null}
            {secure && (
              <StatusPill tone="ok" dot={false}>
                HTTPS
              </StatusPill>
            )}
            {php && phpData.settings && (
              <StatusPill tone="info" dot={false}>
                {/* the live release's, while a change of version deploys */}
                PHP {active?.php_version || phpData.settings.php_version}
              </StatusPill>
            )}
            {php && phpData.settings && phpData.settings.template !== "custom" && (
              <StatusPill tone="off" dot={false}>
                {t(`phpSite.template.${phpData.settings.template}`)}
              </StatusPill>
            )}
          </>
        }
        description={
          php
            ? t(phpData.dbServer ? "phpSite.descriptionDb" : "phpSite.description", {
                server: server?.name ?? "–",
                root: `/srv/sites/${site.name}/current`,
                database: phpData.dbServer?.name ?? "",
              })
            : t("site.description", {
                generator: t(`site.generator.${generator}`),
                server: server?.name ?? "–",
                root: `/srv/sites/${site.name}`,
              })
        }
        actions={
          <>
            {first && (
              <a
                className="button"
                href={`${secure ? "https" : "http"}://${first.hostname}`}
                target="_blank"
                rel="noreferrer"
              >
                <ExternalLink size={14} aria-hidden="true" />
                {t("site.open")}
              </a>
            )}
            {canAct && (
              <>
                <input
                  ref={upload}
                  type="file"
                  accept=".zip,application/zip"
                  className="visually-hidden"
                  aria-label={t("site.uploadZip")}
                  tabIndex={-1}
                  onChange={(event) => {
                    const file = event.target.files?.[0];
                    event.target.value = "";
                    if (file) {
                      void uploadZip(file);
                    }
                  }}
                />
                <Button disabled={busy} onClick={() => upload.current?.click()}>
                  <Upload size={14} aria-hidden="true" />
                  {t("site.uploadZip")}
                </Button>
                {site.source !== "upload" || uploads.items.length > 0 ? (
                  <Button variant="primary" disabled={busy} onClick={() => void deployAgain()}>
                    {t("site.deployAgain")}
                  </Button>
                ) : null}
              </>
            )}
          </>
        }
      />

      {watching && (
        <CommandOutput
          commandId={watching.command}
          title={watching.title}
          onClose={() => setWatching(null)}
        />
      )}

      {php ? (
        <PhpKpis data={phpData} requests={requests} />
      ) : (
        <SiteKpis
          site={site}
          active={active}
          requests={requests}
          routes={routes}
          domains={domains.items}
        />
      )}

      <div className="columns">
        <div className="column-wide">
          {php && (
            <PhpRuntimeCard site={site} data={phpData} canAct={canAct} onApplied={onApplied} />
          )}
          {!php && (
            <SiteDomainsCard site={site} domains={domains.items} routes={routes} canAct={canAct} />
          )}
          <DeploymentsCard
            site={site}
            deployments={deployments.items}
            canAct={canAct}
            busy={busy}
            onRollback={(deployment) => void rollback(deployment)}
            onLog={(deployment) =>
              setWatching({
                command: deployment.command,
                title: t("site.deploymentTitle", { name: site.name, number: deployment.number }),
              })
            }
          />
          {php && (
            <SiteDomainsCard site={site} domains={domains.items} routes={routes} canAct={canAct} />
          )}
        </div>
        <div className="column-side">
          {php && (
            <>
              <PhpDatabaseCard data={phpData} />
              <PhpSharedCard data={phpData} />
              <PhpEnvCard site={site} data={phpData} canAct={canAct} onApplied={onApplied} />
            </>
          )}
          <SourceCard site={site} active={active} canAct={canAct} />
        </div>
      </div>
      {php && phpData.settings && <PhpLogCard site={site} />}
    </>
  );
}

function SiteKpis({
  site,
  active,
  requests,
  routes,
  domains,
}: {
  site: Site;
  active?: Deployment;
  requests: SiteRequests | null;
  routes: DomainRoutes;
  domains: Domain[];
}) {
  const { t } = useTranslation();
  const now = useNow(30_000);
  const route = domains[0] ? routes.routes.get(domains[0].id) : undefined;
  const cert = route?.certificate;
  const days = cert
    ? Math.floor(((parseDate(cert.not_after)?.getTime() ?? now) - now) / 86_400_000)
    : null;
  const expired = days !== null && (days < 0 || route?.tls === "expired");

  return (
    <div className="kpis site-kpis">
      <div className="card kpi">
        <div className="kpi-label">
          <span>{t("site.activeDeployment")}</span>
        </div>
        <div className="kpi-value mono">
          {active
            ? `#${active.number}${active.commit_sha ? ` · ${shortCommit(active.commit_sha)}` : ""}`
            : "–"}
        </div>
        <div className="kpi-note">
          {active
            ? t("site.activeNote", {
                when: formatRelative(active.finished_at || active.updated, now, { past: true }),
                build: formatDuration(active.build_ms / 1000),
              })
            : site.active_deployment
              ? ""
              : t("site.noRelease")}
        </div>
      </div>
      <div className="card kpi">
        <div className="kpi-label">
          <span>{t("site.requests24h")}</span>
          <span className="num">{requests ? formatNumber(requests.total) : "–"}</span>
        </div>
        {requests && requests.total > 0 ? (
          <RequestBars hours={requests.hours} from={requests.from} />
        ) : (
          <div className="kpi-note">{t("site.noRequests")}</div>
        )}
      </div>
      <div className="card kpi">
        <div className="kpi-label">
          <span>{t("site.size")}</span>
        </div>
        <div className="kpi-value">{active ? formatBytes(active.size_bytes) : "–"}</div>
        <div className="kpi-note">
          {active
            ? t("site.files", { count: active.files, formatted: formatNumber(active.files) })
            : ""}
        </div>
      </div>
      <div className="card kpi">
        <div className="kpi-label">
          <span>{t("site.certificate")}</span>
        </div>
        <div
          className={`kpi-value${expired ? " err" : days !== null && days >= 14 ? " ok" : days !== null ? " warn" : ""}`}
        >
          {expired ? t("site.expired") : days !== null ? t("site.daysLeft", { count: days }) : "–"}
        </div>
        <div className="kpi-note">
          {cert
            ? t(
                cert.issuer === "manual"
                  ? "site.uploadedCertificate"
                  : cert.issuer === "internal"
                    ? "site.internalRenews"
                    : "site.renewsByItself",
              )
            : domains.length === 0
              ? t("site.noDomain")
              : t("site.noCertificate")}
        </div>
      </div>
    </div>
  );
}

function SiteDomainsCard({
  site,
  domains,
  routes,
  canAct,
}: {
  site: Site;
  domains: Domain[];
  routes: DomainRoutes;
  canAct: boolean;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const [hostname, setHostname] = useState("");
  const [error, setError] = useState("");
  const [removing, setRemoving] = useState<Domain | null>(null);
  const [removeBusy, setRemoveBusy] = useState(false);

  const connect = async () => {
    setError("");
    try {
      await pb.collection("domains").create({
        hostname: hostname.trim(),
        host: site.host,
        target_kind: "site",
        site: site.id,
        // a site of the LAN keeps its certificates from the internal CA (#26)
        tls: domains.some((domain) => domain.tls === "internal") ? "internal" : "auto",
      });
      toast.success(t("site.domainConnected", { hostname: hostname.trim().toLowerCase() }));
      setHostname("");
    } catch (reason) {
      const fields = fieldErrors(reason);
      setError(fields.hostname ?? fields.site ?? fields.host ?? Object.values(fields)[0]);
    }
  };

  const remove = async () => {
    if (!removing) {
      return;
    }
    setRemoveBusy(true);
    try {
      await pb.collection("domains").delete(removing.id);
      toast.success(t("edge.domainRemoved", { hostname: removing.hostname }));
    } catch (reason) {
      toast.error(reason);
    } finally {
      setRemoveBusy(false);
    }
    setRemoving(null);
  };

  return (
    <Card title={t("site.domains")} note={t("site.domainsNote")} flush>
      {domains.length > 0 && (
        <ul className="list site-domains">
          {domains.map((domain) => {
            const route = routes.routes.get(domain.id);
            return (
              <li key={domain.id} className="list-row">
                <div>
                  <div className="mono">{domain.hostname}</div>
                  <div className="muted small">{route ? tlsText(t, route) : "–"}</div>
                </div>
                <div className="row-actions">
                  {domain.dns_status && <DnsPill status={domain.dns_status} />}
                  {canAct && (
                    <IconButton
                      bare
                      aria-label={t("edge.removeDomain", { hostname: domain.hostname })}
                      title={t("edge.removeDomain", { hostname: domain.hostname })}
                      onClick={() => setRemoving(domain)}
                    >
                      <Trash2 size={15} aria-hidden="true" />
                    </IconButton>
                  )}
                </div>
              </li>
            );
          })}
        </ul>
      )}
      {canAct && (
        <form
          className="site-connect"
          onSubmit={(event) => {
            event.preventDefault();
            void connect();
          }}
        >
          <Field label={t("site.addDomain")} htmlFor={`site-domain-${site.id}`} error={error}>
            <div className="input-row">
              <input
                id={`site-domain-${site.id}`}
                className="input mono"
                placeholder="blog.example.com"
                value={hostname}
                onChange={(event) => setHostname(event.target.value)}
              />
              <Button type="submit" disabled={!hostname.trim()}>
                {t("site.connect")}
              </Button>
            </div>
          </Field>
          <p className="muted small">{t("site.connectNote")}</p>
        </form>
      )}
      {!canAct && domains.length === 0 && <p className="muted card-body">{t("site.noDomain")}</p>}
      <ConfirmDialog
        open={removing !== null}
        title={t("edge.removeConfirm", { hostname: removing?.hostname ?? "" })}
        confirmLabel={t("edge.remove")}
        confirmDisabled={removeBusy}
        danger
        onCancel={() => setRemoving(null)}
        onConfirm={() => void remove()}
      >
        <p className="muted">{t("edge.removeNote")}</p>
      </ConfirmDialog>
    </Card>
  );
}

function deploymentTone(deployment: Deployment, active: boolean): Tone {
  if (active) {
    return "ok";
  }
  switch (deployment.status) {
    case "failed":
      return "err";
    case "building":
      return "info";
    default:
      return "off";
  }
}

function DeploymentsCard({
  site,
  deployments,
  canAct,
  busy,
  onRollback,
  onLog,
}: {
  site: Site;
  deployments: Deployment[];
  canAct: boolean;
  busy: boolean;
  onRollback: (deployment: Deployment) => void;
  onLog: (deployment: Deployment) => void;
}) {
  const { t } = useTranslation();
  const now = useNow(30_000);

  const origin = (deployment: Deployment) => {
    switch (deployment.trigger) {
      case "push":
        return t("site.byPush", {
          commit: shortCommit(deployment.commit_sha),
          branch: deployment.branch,
        });
      case "upload":
        return t("site.byUpload", { who: deployment.requested_by });
      case "create":
        return t("site.byCreate");
      default:
        return deployment.commit_sha
          ? t("site.byHandGit", {
              commit: shortCommit(deployment.commit_sha),
              who: deployment.requested_by,
            })
          : t("site.byHand", { who: deployment.requested_by });
    }
  };

  return (
    <Card
      title={t("site.deployments")}
      note={t(site.type === "php" ? "phpSite.deploymentsNote" : "site.deploymentsNote")}
      flush
    >
      {deployments.length === 0 ? (
        <p className="muted card-body">{t("site.noDeployments")}</p>
      ) : (
        <Table label={t("site.deployments")} compact>
          <thead>
            <tr>
              <th scope="col">#</th>
              <th scope="col">{t("site.state")}</th>
              <th scope="col">{t("site.what")}</th>
              <th scope="col">{t("site.when")}</th>
              <th scope="col">
                <span className="visually-hidden">{t("common.actions")}</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {deployments.map((deployment) => {
              const active = deployment.id === site.active_deployment;
              const canRollback =
                canAct && !active && deployment.status === "succeeded" && deployment.on_server;
              return (
                <tr key={deployment.id}>
                  <td className="mono">#{deployment.number}</td>
                  <td>
                    <StatusPill tone={deploymentTone(deployment, active)}>
                      {active ? t("site.status.active") : t(`site.status.${deployment.status}`)}
                    </StatusPill>
                  </td>
                  <td>
                    <div>{deployment.message || t(`site.trigger.${deployment.trigger}`)}</div>
                    <div className="muted small mono">
                      {origin(deployment)}
                      {deployment.php_version ? ` · PHP ${deployment.php_version}` : ""}
                    </div>
                    {deployment.status === "failed" && deployment.error && (
                      <div className="field-error small">{deployment.error.split("\n")[0]}</div>
                    )}
                  </td>
                  <td>
                    <div>
                      {formatRelative(deployment.finished_at || deployment.created, now, {
                        past: true,
                      })}
                    </div>
                    <div className="muted small">
                      {deployment.status === "succeeded"
                        ? `${formatDuration(deployment.build_ms / 1000)} · ${formatBytes(deployment.size_bytes)}`
                        : ""}
                    </div>
                  </td>
                  <td>
                    <div className="row-actions">
                      {canRollback && (
                        <Button
                          size="small"
                          disabled={busy}
                          onClick={() => onRollback(deployment)}
                          aria-label={t("site.rollbackTo", { number: deployment.number })}
                        >
                          <RotateCcw size={13} aria-hidden="true" />
                          {t("site.rollback")}
                        </Button>
                      )}
                      {deployment.command && (
                        <Button
                          size="small"
                          onClick={() => onLog(deployment)}
                          aria-label={t("site.logOf", { number: deployment.number })}
                        >
                          {t("site.log")}
                        </Button>
                      )}
                    </div>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </Table>
      )}
    </Card>
  );
}

const servingOptions = ["https_redirect", "hsts", "compress", "spa"] as const;

function SourceCard({
  site,
  active,
  canAct,
}: {
  site: Site;
  active?: Deployment;
  canAct: boolean;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const [editing, setEditing] = useState(false);
  const [webhook, setWebhook] = useState(false);
  const [password, setPassword] = useState(false);
  const users = site.basic_auth_users ?? [];

  const toggle = async (field: (typeof servingOptions)[number], value: boolean) => {
    try {
      await pb.collection("sites").update(site.id, { [field]: value });
    } catch (reason) {
      toast.error(reason);
    }
  };

  const php = site.type === "php";
  const image = site.build_image || active?.image;
  // a PHP site's own build is composer, whose command says less than its name
  const command = site.build_cmd || (php ? t("site.composerDefault") : active?.build_cmd);
  // a PHP application routes its own paths (#50)
  const options = servingOptions.filter((field) => !php || field !== "spa");
  return (
    <Card
      title={t("site.sourceAndBuild")}
      side={
        canAct && (
          <Button size="small" onClick={() => setEditing(true)}>
            {t("common.edit")}
          </Button>
        )
      }
    >
      <dl className="facts">
        <dt>{t("site.source")}</dt>
        <dd>{t(`site.sourceKind.${site.source}`)}</dd>
        {site.source === "git" && (
          <>
            <dt>{t("site.repository")}</dt>
            <dd className="mono break">{site.repo_url}</dd>
            <dt>{t("site.branch")}</dt>
            <dd className="mono">
              {site.branch}
              {canAct && (
                <>
                  {" · "}
                  <button type="button" className="link-button" onClick={() => setWebhook(true)}>
                    {t("site.webhook")}
                  </button>
                </>
              )}
            </dd>
          </>
        )}
        {!php && (
          <>
            <dt>{t("site.generatorLabel")}</dt>
            <dd>{t(`site.generator.${site.generator}`)}</dd>
          </>
        )}
        <dt>{t("site.buildImage")}</dt>
        <dd className="mono break">{image || t("site.noBuild")}</dd>
        {command && (
          <>
            <dt>{t("site.command")}</dt>
            <dd className="mono break">{command}</dd>
          </>
        )}
        {!php && (
          <>
            <dt>{t("site.output")}</dt>
            <dd className="mono">{site.output_dir || t("site.outputAuto")}</dd>
          </>
        )}
      </dl>

      <ul className="site-options">
        {options.map((field) => (
          <li key={field}>
            <label className="check-line">
              <input
                type="checkbox"
                checked={site[field]}
                disabled={!canAct}
                onChange={(event) => void toggle(field, event.target.checked)}
              />
              {t(`site.option.${field}`)}
            </label>
          </li>
        ))}
        <li>
          <span className="check-line">
            {users.length > 0
              ? t("site.passwordFor", { users: users.join(", ") })
              : t("site.noPassword")}
          </span>
          {canAct && (
            <Button size="small" onClick={() => setPassword(true)}>
              {t("site.setPassword")}
            </Button>
          )}
        </li>
      </ul>

      {editing && <EditSourceDialog site={site} onClose={() => setEditing(false)} />}
      {webhook && <WebhookDialog site={site} onClose={() => setWebhook(false)} />}
      {password && <PasswordDialog site={site} onClose={() => setPassword(false)} />}
    </Card>
  );
}

/** The fields of how a site is built, shared by the new and the edit dialog. */
export function BuildFields({
  values,
  set,
  errors,
  idPrefix,
  php = false,
}: {
  values: BuildValues;
  set: (change: Partial<BuildValues>) => void;
  errors: Record<string, string>;
  idPrefix: string;
  /** a PHP site builds in its runtime image: only its command may change (#50) */
  php?: boolean;
}) {
  const { t } = useTranslation();
  if (php) {
    return (
      <>
        {values.source === "git" && (
          <GitFields values={values} set={set} errors={errors} idPrefix={idPrefix} />
        )}
        <Field label={t("site.command")} htmlFor={`${idPrefix}-command`} error={errors.build_cmd}>
          <input
            id={`${idPrefix}-command`}
            className="input mono"
            placeholder={t("site.composerDefault")}
            value={values.build_cmd}
            onChange={(event) => set({ build_cmd: event.target.value })}
          />
        </Field>
        <p className="muted small">{t("site.phpBuildNote")}</p>
      </>
    );
  }
  return (
    <>
      {values.source === "git" && (
        <GitFields values={values} set={set} errors={errors} idPrefix={idPrefix} />
      )}
      <Field
        label={t("site.generatorLabel")}
        htmlFor={`${idPrefix}-generator`}
        error={errors.generator}
      >
        <select
          id={`${idPrefix}-generator`}
          className="select"
          value={values.generator}
          onChange={(event) => set({ generator: event.target.value as Generator })}
        >
          {generators.map((generator) => (
            <option key={generator} value={generator}>
              {t(`site.generator.${generator}`)}
            </option>
          ))}
        </select>
      </Field>
      {values.generator !== "none" && (
        <>
          <Field
            label={t("site.buildImage")}
            htmlFor={`${idPrefix}-image`}
            error={errors.build_image}
          >
            <input
              id={`${idPrefix}-image`}
              className="input mono"
              placeholder={values.generator === "custom" ? "node:22-alpine" : t("site.byGenerator")}
              value={values.build_image}
              onChange={(event) => set({ build_image: event.target.value })}
            />
          </Field>
          <Field label={t("site.command")} htmlFor={`${idPrefix}-command`} error={errors.build_cmd}>
            <input
              id={`${idPrefix}-command`}
              className="input mono"
              placeholder={
                values.generator === "custom" ? "npm ci && npm run build" : t("site.byGenerator")
              }
              value={values.build_cmd}
              onChange={(event) => set({ build_cmd: event.target.value })}
            />
          </Field>
        </>
      )}
      <Field label={t("site.output")} htmlFor={`${idPrefix}-output`} error={errors.output_dir}>
        <input
          id={`${idPrefix}-output`}
          className="input mono"
          placeholder={t("site.outputAuto")}
          value={values.output_dir}
          onChange={(event) => set({ output_dir: event.target.value })}
        />
      </Field>
    </>
  );
}

/** Where a site's Git source is, and the token that reads it. */
function GitFields({
  values,
  set,
  errors,
  idPrefix,
}: {
  values: BuildValues;
  set: (change: Partial<BuildValues>) => void;
  errors: Record<string, string>;
  idPrefix: string;
}) {
  const { t } = useTranslation();
  return (
    <>
      <Field label={t("site.repository")} htmlFor={`${idPrefix}-repo`} error={errors.repo_url}>
        <input
          id={`${idPrefix}-repo`}
          className="input mono"
          placeholder="https://github.com/me/blog.git"
          value={values.repo_url}
          onChange={(event) => set({ repo_url: event.target.value })}
        />
      </Field>
      <Field label={t("site.branch")} htmlFor={`${idPrefix}-branch`} error={errors.branch}>
        <input
          id={`${idPrefix}-branch`}
          className="input mono"
          value={values.branch}
          onChange={(event) => set({ branch: event.target.value })}
        />
      </Field>
      <Field label={t("site.token")} htmlFor={`${idPrefix}-token`} error={errors.git_token}>
        <input
          id={`${idPrefix}-token`}
          className="input mono"
          type="password"
          autoComplete="off"
          placeholder={values.tokenSet ? t("site.tokenKept") : t("site.tokenNone")}
          value={values.token}
          onChange={(event) => set({ token: event.target.value })}
        />
      </Field>
    </>
  );
}

export type BuildValues = {
  source: SiteSource;
  repo_url: string;
  branch: string;
  token: string;
  tokenSet: boolean;
  generator: Generator;
  build_image: string;
  build_cmd: string;
  output_dir: string;
};

function EditSourceDialog({ site, onClose }: { site: Site; onClose: () => void }) {
  const { t } = useTranslation();
  const toast = useToast();
  const [values, setValues] = useState<BuildValues>({
    source: site.source,
    repo_url: site.repo_url,
    branch: site.branch,
    token: "",
    tokenSet: site.git_token_set,
    generator: site.generator,
    build_image: site.build_image,
    build_cmd: site.build_cmd,
    output_dir: site.output_dir,
  });
  const [keep, setKeep] = useState(String(site.keep_releases || 5));
  const [notFound, setNotFound] = useState(site.not_found);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const set = (change: Partial<BuildValues>) => setValues((current) => ({ ...current, ...change }));
  const php = site.type === "php";

  const save = async () => {
    setBusy(true);
    setErrors({});
    try {
      await pb.collection("sites").update(site.id, {
        source: values.source,
        repo_url: values.source === "git" ? values.repo_url.trim() : site.repo_url,
        branch: values.source === "git" ? values.branch.trim() : site.branch,
        generator: values.generator,
        build_image: php || values.generator === "none" ? "" : values.build_image.trim(),
        build_cmd: !php && values.generator === "none" ? "" : values.build_cmd.trim(),
        output_dir: php ? "" : values.output_dir.trim(),
        keep_releases: Number(keep),
        not_found: php ? "" : notFound.trim(),
        ...(values.source === "git" && values.token.trim()
          ? { git_token: values.token.trim() }
          : {}),
      });
      toast.success(t("site.saved"));
      onClose();
    } catch (reason) {
      setErrors(fieldErrors(reason));
    } finally {
      setBusy(false);
    }
  };

  return (
    <ConfirmDialog
      open
      title={t("site.editTitle", { name: site.name })}
      confirmLabel={t("common.save")}
      confirmDisabled={busy}
      onCancel={onClose}
      onConfirm={() => void save()}
    >
      <div className="dialog-form">
        <SegmentedControl
          label={t("site.source")}
          value={values.source}
          onChange={(source) => set({ source })}
          options={(["git", "upload", "empty"] as SiteSource[]).map((source) => ({
            value: source,
            label: t(`site.sourceKind.${source}`),
          }))}
        />
        <BuildFields values={values} set={set} errors={errors} idPrefix="edit-site" php={php} />
        {values.source === "git" && values.tokenSet && (
          <Button
            size="small"
            onClick={() =>
              void sitesApi
                .credentials(site.id, { git_token: "" })
                .then(() => {
                  set({ tokenSet: false });
                  toast.success(t("site.tokenRemoved"));
                })
                .catch((reason) => toast.error(reason))
            }
          >
            {t("site.removeToken")}
          </Button>
        )}
        {!php && (
          <Field label={t("site.notFoundPage")} htmlFor="edit-site-404" error={errors.not_found}>
            <input
              id="edit-site-404"
              className="input mono"
              placeholder="/404.html"
              value={notFound}
              onChange={(event) => setNotFound(event.target.value)}
            />
          </Field>
        )}
        <Field label={t("site.keepReleases")} htmlFor="edit-site-keep" error={errors.keep_releases}>
          <input
            id="edit-site-keep"
            className="input"
            type="number"
            min={1}
            max={50}
            value={keep}
            onChange={(event) => setKeep(event.target.value)}
          />
        </Field>
        {(errors.form || errors.headers) && (
          <p className="field-error">{errors.form || errors.headers}</p>
        )}
      </div>
    </ConfirmDialog>
  );
}

function WebhookDialog({ site, onClose }: { site: Site; onClose: () => void }) {
  const { t } = useTranslation();
  const toast = useToast();
  const [webhook, setWebhook] = useState<{ url: string; secret: string } | null>(null);
  const [rotating, setRotating] = useState(false);

  useEffect(() => {
    sitesApi
      .webhook(site.id)
      .then(setWebhook)
      .catch((reason) => toast.error(reason));
  }, [site.id, toast]);

  return (
    <ConfirmDialog
      open
      title={t("site.webhookTitle", { name: site.name })}
      confirmLabel={t("common.close")}
      onCancel={onClose}
      onConfirm={onClose}
    >
      <div className="dialog-form">
        <p className="muted">{t("site.webhookText", { branch: site.branch })}</p>
        {webhook ? (
          <dl className="facts">
            <dt>{t("site.webhookURL")}</dt>
            <dd className="mono break">{webhook.url}</dd>
            <dt>{t("site.webhookSecret")}</dt>
            <dd className="mono break">{webhook.secret}</dd>
            <dt>{t("site.webhookContent")}</dt>
            <dd>application/json</dd>
          </dl>
        ) : (
          <p className="muted">{t("common.loading")}</p>
        )}
        <div>
          <Button
            size="small"
            disabled={rotating}
            onClick={() => {
              setRotating(true);
              sitesApi
                .rotateWebhook(site.id)
                .then((answer) => {
                  setWebhook(answer);
                  toast.success(t("site.webhookRotated"));
                })
                .catch((reason) => toast.error(reason))
                .finally(() => setRotating(false));
            }}
          >
            {t("site.webhookRotate")}
          </Button>
        </div>
      </div>
    </ConfirmDialog>
  );
}

function PasswordDialog({ site, onClose }: { site: Site; onClose: () => void }) {
  const { t } = useTranslation();
  const toast = useToast();
  const [text, setText] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const save = async (value: string) => {
    setBusy(true);
    setError("");
    try {
      await sitesApi.credentials(site.id, { basic_auth: value });
      toast.success(value ? t("site.passwordSet") : t("site.passwordRemoved"));
      onClose();
    } catch (reason) {
      setError(errorMessage(reason));
    } finally {
      setBusy(false);
    }
  };

  return (
    <ConfirmDialog
      open
      title={t("site.passwordTitle", { name: site.name })}
      confirmLabel={t("common.save")}
      confirmDisabled={busy || !text.trim()}
      onCancel={onClose}
      onConfirm={() => void save(text.trim())}
    >
      <div className="dialog-form">
        <p className="muted">{t("site.passwordText")}</p>
        <Field label={t("site.passwordUsers")} htmlFor="site-password" error={error}>
          <textarea
            id="site-password"
            className="input mono"
            rows={4}
            placeholder="bob:$2y$05$…"
            value={text}
            onChange={(event) => setText(event.target.value)}
          />
        </Field>
        {(site.basic_auth_users ?? []).length > 0 && (
          <div>
            <Button size="small" variant="danger" disabled={busy} onClick={() => void save("")}>
              {t("site.removePassword")}
            </Button>
          </div>
        )}
      </div>
    </ConfirmDialog>
  );
}
