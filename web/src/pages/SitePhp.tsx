import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { LogViewer } from "../components/LogViewer";
import { TaskList } from "../components/TaskList";
import { useToast } from "../components/toast";
import { Banner, Button, Card, ConfirmDialog, Field, StatusPill } from "../components/ui";
import { errorMessage } from "../lib/api";
import { hasRole, useAuth } from "../lib/auth";
import { serverLabel, type Database, type DbServer } from "../lib/databases";
import { formatBytes, formatDuration, formatNumber, formatTime } from "../lib/format";
import { pb } from "../lib/pb";
import {
  phpApi,
  phpTemplates,
  type FpmSettings,
  type PhpApplied,
  type PhpMail,
  type PhpStatus,
  type PhpVersion,
  type PhpVersions,
  type SitePhp,
  type SitePhpChange,
} from "../lib/php";
import { useLiveList, useLiveRecord } from "../lib/realtime";
import type { Site, SiteRequests } from "../lib/sites";

/** PHP sites (#50): what the page of one shows besides a static site's, from
 * its settings, its PHP-FPM's status and its database. */

/** How often the page asks the site's server how its PHP-FPM is doing. */
const statusEvery = 60_000;

/** The defaults of the panel, for settings stored before they were set. */
const defaultFpm: FpmSettings = {
  pm: "dynamic",
  max_children: 10,
  max_requests: 500,
  request_terminate_timeout: 60,
};

export type PhpSiteData = {
  settings: SitePhp | null;
  status: PhpStatus | null;
  statusError: string;
  database: Database | null;
  dbServer: DbServer | null;
  versions: PhpVersions | null;
};

/** A PHP site's settings, live, and what its server says of its PHP-FPM. */
export function usePhpSite(site: Site, enabled = true): PhpSiteData {
  const list = useLiveList<SitePhp>("site_php", {
    filter: pb.filter("site = {:site}", { site: site.id }),
    perPage: 1,
    enabled,
  });
  const settings = list.items[0] ?? null;
  const { record: database } = useLiveRecord<Database>(
    "databases",
    settings?.database || undefined,
  );
  const { record: dbServer } = useLiveRecord<DbServer>("db_servers", database?.server || undefined);
  const [status, setStatus] = useState<PhpStatus | null>(null);
  const [statusError, setStatusError] = useState("");
  const [versions, setVersions] = useState<PhpVersions | null>(null);
  const running = settings?.state === "running";
  const applied = settings?.applied_at ?? "";

  useEffect(() => {
    if (!enabled) {
      return;
    }
    let live = true;
    phpApi
      .versions()
      .then((answer) => live && setVersions(answer))
      .catch(() => {
        // the version switch offers nothing
      });
    return () => {
      live = false;
    };
  }, [enabled]);

  useEffect(() => {
    if (!running) {
      return;
    }
    let live = true;
    const load = () =>
      phpApi
        .status(site.id)
        .then((answer) => {
          if (live) {
            setStatus(answer);
            setStatusError("");
          }
        })
        .catch((reason: unknown) => {
          if (live && !(reason as { isAbort?: boolean }).isAbort) {
            // what was read before is no longer how it is
            setStatus(null);
            setStatusError(errorMessage(reason));
          }
        });
    void load();
    // each read is a command on the server: none while the page is hidden
    const timer = window.setInterval(() => {
      if (!document.hidden) {
        void load();
      }
    }, statusEvery);
    const visible = () => {
      if (!document.hidden) {
        void load();
      }
    };
    document.addEventListener("visibilitychange", visible);
    return () => {
      live = false;
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", visible);
      setStatus(null);
      setStatusError("");
    };
    // a new apply restarts the container: its status is read again
  }, [site.id, running, applied]);

  return {
    settings,
    status: running ? status : null,
    statusError,
    database: settings?.database ? database : null,
    dbServer: settings?.database ? dbServer : null,
    versions,
  };
}

function fpmOf(settings: SitePhp | null): FpmSettings {
  return { ...defaultFpm, ...(settings?.fpm ?? {}) };
}

/** "repo:8.4-fpm · sha256:94a71dd6" for the image the container runs, as
 * the panel keeps it: its tag pinned by its digest. */
function imageText(image: string): string {
  const [reference, digest] = image.split("@");
  return digest ? `${reference} · ${digest.slice(0, 15)}` : image;
}

function percent(value: number): string {
  return `${formatNumber(value, value > 0 && value < 1 ? 1 : 0)} %`;
}

/** The four figures over a PHP site's page, as the design has them. */
export function PhpKpis({ data, requests }: { data: PhpSiteData; requests: SiteRequests | null }) {
  const { t } = useTranslation();
  const { settings, status, statusError, database, dbServer } = data;
  const fpm = fpmOf(settings);
  const latency = requests?.latency;
  const answered = latency && latency.requests > 0 ? latency : undefined;
  const errorRate = answered ? (answered.errors / answered.requests) * 100 : 0;

  return (
    <div className="kpis site-kpis">
      <div className="card kpi">
        <div className="kpi-label">
          <span>{t("phpSite.workers")}</span>
        </div>
        <div className="kpi-value mono">
          {status?.workers
            ? `${status.workers.active} / ${status.workers.max || fpm.max_children}`
            : "–"}
        </div>
        <div className="kpi-note">
          {status?.workers
            ? t("phpSite.workersNote", { pm: fpm.pm, queue: status.workers.listen_queue })
            : status?.errors?.length
              ? status.errors.join(" ")
              : statusError || t(`phpSite.state.${settings?.state || "pending"}`)}
        </div>
      </div>
      <div className="card kpi">
        <div className="kpi-label">
          <span>{t("phpSite.latency")}</span>
        </div>
        <div className="kpi-value mono">
          {answered
            ? t("phpSite.latencyValue", {
                p50: formatNumber(Math.round(answered.p50_ms)),
                p95: formatNumber(Math.round(answered.p95_ms)),
              })
            : "–"}
        </div>
        <div className="kpi-note">
          {status?.opcache?.enabled
            ? // 99.8 % of hits is not all of them
              t("phpSite.opcacheHits", {
                rate: `${formatNumber(Math.floor(status.opcache.hit_rate))} %`,
              })
            : answered
              ? ""
              : t("phpSite.noRequests")}
        </div>
      </div>
      <div className="card kpi">
        <div className="kpi-label">
          <span>{t("phpSite.errors24h")}</span>
        </div>
        <div className={`kpi-value mono${answered && answered.errors > 0 ? " warn" : ""}`}>
          {answered ? percent(errorRate) : "–"}
        </div>
        <div className="kpi-note">
          {answered
            ? t("phpSite.errorsOf", {
                count: answered.requests,
                errors: formatNumber(answered.errors),
                requests: formatNumber(answered.requests),
              })
            : t("phpSite.noRequests")}
        </div>
      </div>
      <div className="card kpi">
        <div className="kpi-label">
          <span>{t("phpSite.database")}</span>
        </div>
        <div className="kpi-value mono">
          {database ? `${database.name} · ${formatBytes(database.size_bytes)}` : "–"}
        </div>
        <div className="kpi-note">
          {database && dbServer
            ? `${dbServer.name} · ${serverLabel(dbServer)} · ${t(`phpSite.serverKind.${dbServer.kind}`)}`
            : t("phpSite.noDatabase")}
        </div>
      </div>
    </div>
  );
}

/** Watches what a change started: the apply's command, or the deployment of a
 * new version, whose command the deployments card shows. */
export type OnApplied = (applied: PhpApplied, title: string) => void;

export function PhpRuntimeCard({
  site,
  data,
  canAct,
  onApplied,
}: {
  site: Site;
  data: PhpSiteData;
  canAct: boolean;
  onApplied: OnApplied;
}) {
  const { t } = useTranslation();
  const [dialog, setDialog] = useState<"ini" | "version" | "settings" | null>(null);
  const { settings, status, versions } = data;
  if (!settings) {
    return (
      <Card title={t("phpSite.runtime")}>
        <p className="muted">{t("common.loading")}</p>
      </Card>
    );
  }
  const fpm = fpmOf(settings);
  const container = `${site.name}-php`;
  const newer = newerVersion(settings.php_version, versions?.versions ?? []);
  const extensions = [...(status?.extensions ?? [])];
  if (status?.opcache?.enabled && !extensions.includes("opcache")) {
    extensions.unshift("opcache");
  }
  const state = settings.state || "pending";

  return (
    <Card
      title={t("phpSite.runtime")}
      side={
        <span className="runtime-state">
          <StatusPill tone={state === "running" ? "ok" : state === "failed" ? "err" : "info"} dot>
            {status?.workers
              ? t("phpSite.healthyFor", {
                  container,
                  uptime: formatDuration(status.workers.uptime),
                })
              : `${container} · ${t(`phpSite.state.${state}`)}`}
          </StatusPill>
          {canAct && (
            <Button size="small" onClick={() => setDialog("settings")}>
              {t("phpSite.settings")}
            </Button>
          )}
        </span>
      }
    >
      {state === "failed" && settings.last_error && (
        <Banner tone="err">{settings.last_error}</Banner>
      )}
      <dl className="facts">
        <dt>{t("phpSite.image")}</dt>
        <dd className="mono break" title={settings.image}>
          {settings.image ? imageText(settings.image) : "–"}
        </dd>
        <dt>FastCGI</dt>
        <dd className="mono">{t("phpSite.fastcgi", { upstream: `${container}:9000` })}</dd>
        <dt>{t("phpSite.pool")}</dt>
        <dd className="mono">
          {t("phpSite.poolValue", {
            pm: fpm.pm,
            children: fpm.max_children,
            requests: fpm.max_requests,
          })}
        </dd>
        <dt>{t("phpSite.limits")}</dt>
        <dd>
          {t("phpSite.limitsValue", {
            cpu: formatNumber(settings.cpu_limit || 1, 2),
            memory: `${formatNumber(settings.mem_limit_mb || 512)} MB`,
          })}
        </dd>
        <dt>{t("phpSite.webRoot")}</dt>
        <dd className="mono break">
          {`/srv/sites/${site.name}/current${settings.docroot ? `/${settings.docroot}` : ""}`}
        </dd>
        <dt>{t("phpSite.mode")}</dt>
        <dd>{t(`phpSite.modeKind.${settings.mode || "releases"}`)}</dd>
      </dl>

      {extensions.length > 0 && (
        <>
          <h3 className="section-label">{t("phpSite.extensions")}</h3>
          <ul className="chip-list" aria-label={t("phpSite.extensions")}>
            {extensions.map((name) => (
              <li key={name} className="chip mono">
                {name}
              </li>
            ))}
          </ul>
        </>
      )}

      <h3 className="section-label">{t("phpSite.ownIni")}</h3>
      {settings.php_ini.trim() ? (
        <pre className="php-ini mono">{settings.php_ini.trim()}</pre>
      ) : (
        <p className="muted small">{t("phpSite.noOwnIni")}</p>
      )}

      {canAct && (
        <div className="button-row runtime-actions">
          <Button onClick={() => setDialog("ini")}>{t("phpSite.editIni")}</Button>
          <Button
            className={newer ? "accent-soft" : undefined}
            onClick={() => setDialog("version")}
          >
            {newer ? t("phpSite.switchTo", { version: newer.version }) : t("phpSite.switchVersion")}
          </Button>
          <span className="muted small">{t("phpSite.switchNote")}</span>
        </div>
      )}

      {dialog === "ini" && (
        <IniDialog
          site={site}
          settings={settings}
          onClose={() => setDialog(null)}
          onApplied={onApplied}
        />
      )}
      {dialog === "version" && (
        <VersionDialog
          site={site}
          settings={settings}
          versions={versions?.versions ?? []}
          suggested={newer?.version}
          onClose={() => setDialog(null)}
          onApplied={onApplied}
        />
      )}
      {dialog === "settings" && (
        <SettingsDialog
          site={site}
          settings={settings}
          onClose={() => setDialog(null)}
          onApplied={onApplied}
        />
      )}
    </Card>
  );
}

/** The newest version that still gets fixes and is newer than the site's. */
function newerVersion(current: string, versions: PhpVersion[]): PhpVersion | undefined {
  return versions.find(
    (version) =>
      (version.state === "supported" || version.state === "security") &&
      compareVersions(version.version, current) > 0,
  );
}

function compareVersions(a: string, b: string): number {
  const [aMajor = 0, aMinor = 0] = a.split(".").map(Number);
  const [bMajor = 0, bMinor = 0] = b.split(".").map(Number);
  return aMajor - bMajor || aMinor - bMinor;
}

/** Saves a change of the settings and hands on what it started. */
function useSave(site: Site, onApplied: OnApplied, onClose: () => void) {
  const { t } = useTranslation();
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const save = async (change: SitePhpChange) => {
    setBusy(true);
    setError("");
    try {
      const applied = await phpApi.settings(site.id, change);
      toast.success(
        !applied.deployment
          ? t("phpSite.applyStarted")
          : change.php_version !== undefined
            ? t("phpSite.versionStarted")
            : t("phpSite.redeployStarted"),
      );
      onApplied(applied, t("phpSite.applyTitle", { name: site.name }));
      onClose();
    } catch (reason) {
      setError(errorMessage(reason));
    } finally {
      setBusy(false);
    }
  };
  return { busy, error, save };
}

function IniDialog({
  site,
  settings,
  onClose,
  onApplied,
}: {
  site: Site;
  settings: SitePhp;
  onClose: () => void;
  onApplied: OnApplied;
}) {
  const { t } = useTranslation();
  const [ini, setIni] = useState(settings.php_ini);
  const { busy, error, save } = useSave(site, onApplied, onClose);
  return (
    <ConfirmDialog
      open
      title={t("phpSite.iniTitle", { name: site.name })}
      confirmLabel={t("phpSite.saveApply")}
      confirmDisabled={busy}
      onCancel={onClose}
      onConfirm={() => void save({ php_ini: ini })}
    >
      <div className="dialog-form">
        <Field label={t("phpSite.ownIni")} htmlFor="php-ini" error={error}>
          <textarea
            id="php-ini"
            className="input mono"
            rows={9}
            spellCheck={false}
            placeholder={"memory_limit = 256M\nupload_max_filesize = 64M"}
            value={ini}
            onChange={(event) => setIni(event.target.value)}
          />
        </Field>
        <p className="muted small">{t("phpSite.iniNote")}</p>
      </div>
    </ConfirmDialog>
  );
}

function VersionDialog({
  site,
  settings,
  versions,
  suggested,
  onClose,
  onApplied,
}: {
  site: Site;
  settings: SitePhp;
  versions: PhpVersion[];
  suggested?: string;
  onClose: () => void;
  onApplied: OnApplied;
}) {
  const { t } = useTranslation();
  const choices = versions.filter((version) => version.version !== settings.php_version);
  const [picked, setVersion] = useState("");
  // the versions may arrive after the dialog opened
  const version = picked || suggested || choices[0]?.version || "";
  const { busy, error, save } = useSave(site, onApplied, onClose);
  const chosen = versions.find((item) => item.version === version);
  return (
    <ConfirmDialog
      open
      title={t("phpSite.versionTitle", { name: site.name })}
      confirmLabel={t("phpSite.switchConfirm", { version })}
      confirmDisabled={busy || !version}
      onCancel={onClose}
      onConfirm={() => void save({ php_version: version })}
    >
      <div className="dialog-form">
        <Field label={t("phpSite.version")} htmlFor="php-version" error={error}>
          <select
            id="php-version"
            className="select"
            value={version}
            onChange={(event) => setVersion(event.target.value)}
          >
            {choices.map((item) => (
              <option key={item.version} value={item.version} disabled={item.state === "ended"}>
                {`PHP ${item.version} · ${t(`php.state.${item.state}`)}`}
              </option>
            ))}
          </select>
        </Field>
        <p className="muted small">
          {t(
            chosen && compareVersions(chosen.version, settings.php_version) < 0
              ? "phpSite.downgradeNote"
              : "phpSite.upgradeNote",
            { from: settings.php_version, to: version },
          )}
        </p>
      </div>
    </ConfirmDialog>
  );
}

function SettingsDialog({
  site,
  settings,
  onClose,
  onApplied,
}: {
  site: Site;
  settings: SitePhp;
  onClose: () => void;
  onApplied: OnApplied;
}) {
  const { t } = useTranslation();
  const fpm = fpmOf(settings);
  const [values, setValues] = useState({
    docroot: settings.docroot,
    mode: settings.mode || "releases",
    pm: fpm.pm,
    children: String(fpm.max_children),
    requests: String(fpm.max_requests),
    timeout: String(fpm.request_terminate_timeout),
    cpu: String(settings.cpu_limit || 1),
    memory: String(settings.mem_limit_mb || 512),
    shared: (settings.shared_paths ?? []).join("\n"),
    hook: settings.release_hook,
    health: settings.health_path ?? "",
    worker: settings.queue_worker === true,
  });
  // a template's worker, as Laravel's queue worker (#52)
  const template = settings.template === "custom" ? null : phpTemplates[settings.template];
  const set = (change: Partial<typeof values>) =>
    setValues((current) => ({ ...current, ...change }));
  const { busy, error, save } = useSave(site, onApplied, onClose);
  // an empty field is no number, and 0 would mean no limit at all
  const positive = (value: string) => value.trim() !== "" && Number(value) > 0;
  const valid =
    [values.children, values.timeout, values.cpu, values.memory].every(positive) &&
    values.requests.trim() !== "" &&
    Number(values.requests) >= 0;
  const submit = () =>
    void save({
      docroot: values.docroot.trim().replace(/^\/+|\/+$/g, ""),
      mode: values.mode,
      fpm: {
        pm: values.pm,
        max_children: Number(values.children),
        max_requests: Number(values.requests),
        request_terminate_timeout: Number(values.timeout),
      },
      cpu_limit: Number(values.cpu),
      mem_limit_mb: Number(values.memory),
      shared_paths: values.shared
        .split("\n")
        .map((line) => line.trim())
        .filter(Boolean),
      release_hook: values.hook.trim(),
      health_path: values.health.trim(),
      ...(template?.worker ? { queue_worker: values.worker } : {}),
    });
  return (
    <ConfirmDialog
      open
      title={t("phpSite.settingsTitle", { name: site.name })}
      confirmLabel={t("phpSite.saveApply")}
      confirmDisabled={busy || !valid}
      onCancel={onClose}
      onConfirm={submit}
    >
      <div className="dialog-form">
        <div className="form-row">
          <Field label={t("phpSite.webRoot")} htmlFor="php-docroot">
            <input
              id="php-docroot"
              className="input mono"
              placeholder="public"
              value={values.docroot}
              onChange={(event) => set({ docroot: event.target.value })}
            />
          </Field>
          <Field label={t("phpSite.mode")} htmlFor="php-mode">
            <select
              id="php-mode"
              className="select"
              value={values.mode}
              onChange={(event) => set({ mode: event.target.value as SitePhp["mode"] })}
            >
              <option value="releases">{t("phpSite.modeKind.releases")}</option>
              <option value="classic">{t("phpSite.modeKind.classic")}</option>
            </select>
          </Field>
        </div>
        <p className="muted small">{t(`phpSite.modeNote.${values.mode}`)}</p>
        <div className="form-row">
          <Field label={t("phpSite.pm")} htmlFor="php-pm">
            <select
              id="php-pm"
              className="select"
              value={values.pm}
              onChange={(event) => set({ pm: event.target.value as FpmSettings["pm"] })}
            >
              <option value="dynamic">dynamic</option>
              <option value="ondemand">ondemand</option>
              <option value="static">static</option>
            </select>
          </Field>
          <Field label="max_children" htmlFor="php-children">
            <input
              id="php-children"
              className="input"
              type="number"
              min={1}
              value={values.children}
              onChange={(event) => set({ children: event.target.value })}
            />
          </Field>
        </div>
        <div className="form-row">
          <Field label="max_requests" htmlFor="php-requests">
            <input
              id="php-requests"
              className="input"
              type="number"
              min={0}
              value={values.requests}
              onChange={(event) => set({ requests: event.target.value })}
            />
          </Field>
          <Field label={t("phpSite.timeout")} htmlFor="php-timeout">
            <input
              id="php-timeout"
              className="input"
              type="number"
              min={1}
              value={values.timeout}
              onChange={(event) => set({ timeout: event.target.value })}
            />
          </Field>
        </div>
        <div className="form-row">
          <Field label={t("phpSite.cpu")} htmlFor="php-cpu">
            <input
              id="php-cpu"
              className="input"
              type="number"
              min={0.1}
              step={0.1}
              value={values.cpu}
              onChange={(event) => set({ cpu: event.target.value })}
            />
          </Field>
          <Field label={t("phpSite.memory")} htmlFor="php-memory">
            <input
              id="php-memory"
              className="input"
              type="number"
              min={64}
              step={64}
              value={values.memory}
              onChange={(event) => set({ memory: event.target.value })}
            />
          </Field>
        </div>
        <Field label={t("phpSite.sharedPaths")} htmlFor="php-shared">
          <textarea
            id="php-shared"
            className="input mono"
            rows={3}
            spellCheck={false}
            placeholder={"storage\nwp-content/uploads"}
            value={values.shared}
            onChange={(event) => set({ shared: event.target.value })}
          />
        </Field>
        <p className="muted small">{t("phpSite.sharedPathsNote")}</p>
        <Field label={t("phpSite.releaseHook")} htmlFor="php-hook">
          <textarea
            id="php-hook"
            className="input mono"
            rows={3}
            spellCheck={false}
            placeholder="php artisan migrate --force"
            value={values.hook}
            onChange={(event) => set({ hook: event.target.value })}
          />
        </Field>
        <p className="muted small">{t("phpSite.releaseHookNote")}</p>
        <Field label={t("phpSite.healthPath")} htmlFor="php-health">
          <input
            id="php-health"
            className="input mono"
            placeholder="/up"
            value={values.health}
            onChange={(event) => set({ health: event.target.value })}
          />
        </Field>
        <p className="muted small">{t("phpSite.healthPathNote")}</p>
        {template?.worker && (
          <label className="check-line">
            <input
              type="checkbox"
              checked={values.worker}
              onChange={(event) => set({ worker: event.target.checked })}
            />
            <span>
              {t("phpSite.queueWorker")}
              <span className="muted small">
                {" "}
                · {t("phpSite.queueWorkerNote", { name: site.name })}
              </span>
            </span>
          </label>
        )}
        {error && <p className="field-error">{error}</p>}
      </div>
    </ConfirmDialog>
  );
}

export function PhpDatabaseCard({ data }: { data: PhpSiteData }) {
  const { t } = useTranslation();
  const { database, dbServer } = data;
  return (
    <Card
      title={t("phpSite.database")}
      side={
        dbServer && (
          <Link className="small" to="/databases">
            {dbServer.name}
          </Link>
        )
      }
    >
      {database ? (
        <dl className="facts">
          <dt>{t("phpSite.dbServer")}</dt>
          <dd>
            {dbServer
              ? `${dbServer.name} · ${serverLabel(dbServer)} · ${t(`phpSite.serverKind.${dbServer.kind}`)}`
              : "–"}
          </dd>
          <dt>{t("phpSite.dbName")}</dt>
          <dd className="mono">{`${database.name} · ${formatBytes(database.size_bytes)}`}</dd>
          <dt>{t("phpSite.dbUser")}</dt>
          <dd className="mono">{t("phpSite.dbUserValue", { name: database.name })}</dd>
          <dt>{t("phpSite.dbPassword")}</dt>
          <dd className="mono">{t("phpSite.dbPasswordValue")}</dd>
        </dl>
      ) : (
        <p className="muted">
          {t("phpSite.noDatabaseText")} <Link to="/databases">{t("phpSite.toDatabases")}</Link>
        </p>
      )}
    </Card>
  );
}

export function PhpSharedCard({ data }: { data: PhpSiteData }) {
  const { t } = useTranslation();
  const { settings, status } = data;
  const paths = settings?.shared_paths ?? [];
  const sizes = new Map((status?.shared ?? []).map((item) => [item.path, item.bytes]));
  // what backups leave out (#52): a shared path, or another path of the site
  const excluded = settings?.backup_exclude ?? [];
  const elsewhere = excluded.filter((path) => !paths.includes(path));
  return (
    <Card title={t("phpSite.shared")}>
      {paths.length === 0 ? (
        <p className="muted">{t("phpSite.noShared")}</p>
      ) : (
        <ul className="shared-list">
          {paths.map((path) => {
            const size = sizes.get(path);
            return (
              <li key={path}>
                <div className="shared-row">
                  <span className="mono">{path}</span>
                  <span className="mono">
                    {size === undefined || size === null ? "–" : formatBytes(size)}
                  </span>
                </div>
                <div className="muted small">
                  {t(excluded.includes(path) ? "phpSite.sharedNoBackup" : "phpSite.sharedNote")}
                </div>
              </li>
            );
          })}
        </ul>
      )}
      {elsewhere.length > 0 && (
        <p className="muted small">
          {t("phpSite.noBackup")} <span className="mono">{elsewhere.join(", ")}</span>
        </p>
      )}
    </Card>
  );
}

/** The site's environment, and its own secrets by name. */
export function PhpEnvCard({
  site,
  data,
  canAct,
  onApplied,
}: {
  site: Site;
  data: PhpSiteData;
  canAct: boolean;
  onApplied: OnApplied;
}) {
  const { t } = useTranslation();
  const [dialog, setDialog] = useState<"env" | "secrets" | null>(null);
  const { settings } = data;
  if (!settings) {
    return null;
  }
  const names = settings.secret_names ?? [];
  return (
    <Card
      title={t("phpSite.env")}
      side={
        canAct && (
          <span className="runtime-state">
            <Button size="small" onClick={() => setDialog("env")}>
              {t("common.edit")}
            </Button>
            <Button size="small" onClick={() => setDialog("secrets")}>
              {t("phpSite.secrets")}
            </Button>
          </span>
        )
      }
    >
      {settings.env.trim() ? (
        <pre className="php-ini mono">{settings.env.trim()}</pre>
      ) : (
        <p className="muted small">{t("phpSite.noEnv")}</p>
      )}
      <h3 className="section-label">{t("phpSite.secrets")}</h3>
      {names.length > 0 ? (
        <ul className="chip-list" aria-label={t("phpSite.secrets")}>
          {names.map((name) => (
            <li key={name} className="chip mono">
              {name}
            </li>
          ))}
        </ul>
      ) : (
        <p className="muted small">{t("phpSite.noSecrets")}</p>
      )}
      {dialog === "env" && (
        <EnvDialog
          site={site}
          settings={settings}
          onClose={() => setDialog(null)}
          onApplied={onApplied}
        />
      )}
      {dialog === "secrets" && (
        <SecretsDialog
          site={site}
          names={names}
          onClose={() => setDialog(null)}
          onApplied={onApplied}
        />
      )}
    </Card>
  );
}

function EnvDialog({
  site,
  settings,
  onClose,
  onApplied,
}: {
  site: Site;
  settings: SitePhp;
  onClose: () => void;
  onApplied: OnApplied;
}) {
  const { t } = useTranslation();
  const [env, setEnv] = useState(settings.env);
  const { busy, error, save } = useSave(site, onApplied, onClose);
  return (
    <ConfirmDialog
      open
      title={t("phpSite.envTitle", { name: site.name })}
      confirmLabel={t("phpSite.saveApply")}
      confirmDisabled={busy}
      onCancel={onClose}
      onConfirm={() => void save({ env })}
    >
      <div className="dialog-form">
        <Field label={t("phpSite.env")} htmlFor="php-env" error={error}>
          <textarea
            id="php-env"
            className="input mono"
            rows={9}
            spellCheck={false}
            placeholder={"APP_ENV=production\nAPP_KEY=${secret:app-key}"}
            value={env}
            onChange={(event) => setEnv(event.target.value)}
          />
        </Field>
        <p className="muted small">{t("phpSite.envNote")}</p>
      </div>
    </ConfirmDialog>
  );
}

function SecretsDialog({
  site,
  names,
  onClose,
  onApplied,
}: {
  site: Site;
  names: string[];
  onClose: () => void;
  onApplied: OnApplied;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const [name, setName] = useState("");
  const [value, setValue] = useState("");
  const [removed, setRemoved] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const change: Record<string, string | null> = Object.fromEntries(
    removed.map((item) => [item, null]),
  );
  // a name without a value would replace a secret with nothing
  const named = name.trim() !== "";
  if (named && value !== "") {
    change[name.trim()] = value;
  }
  const save = async () => {
    setBusy(true);
    setError("");
    try {
      const applied = await phpApi.secrets(site.id, change);
      toast.success(applied.deployment ? t("phpSite.redeployStarted") : t("phpSite.applyStarted"));
      onApplied(applied, t("phpSite.applyTitle", { name: site.name }));
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
      title={t("phpSite.secretsTitle", { name: site.name })}
      confirmLabel={t("phpSite.saveApply")}
      confirmDisabled={busy || Object.keys(change).length === 0 || (named && value === "")}
      onCancel={onClose}
      onConfirm={() => void save()}
    >
      <div className="dialog-form">
        {names.length > 0 && (
          <ul className="secret-list">
            {names.map((item) => {
              const gone = removed.includes(item);
              return (
                <li key={item}>
                  <span className={`mono${gone ? " struck" : ""}`}>{item}</span>
                  <Button
                    size="small"
                    aria-pressed={gone}
                    aria-label={t(gone ? "phpSite.keepSecretNamed" : "phpSite.removeSecretNamed", {
                      name: item,
                    })}
                    onClick={() =>
                      setRemoved((current) =>
                        gone ? current.filter((other) => other !== item) : [...current, item],
                      )
                    }
                  >
                    {gone ? t("phpSite.keepSecret") : t("phpSite.removeSecret")}
                  </Button>
                </li>
              );
            })}
          </ul>
        )}
        <div className="form-row">
          <Field label={t("phpSite.secretName")} htmlFor="php-secret-name">
            <input
              id="php-secret-name"
              className="input mono"
              placeholder="app-key"
              autoComplete="off"
              value={name}
              onChange={(event) => setName(event.target.value)}
            />
          </Field>
          <Field label={t("phpSite.secretValue")} htmlFor="php-secret-value">
            <input
              id="php-secret-value"
              className="input mono"
              type="password"
              autoComplete="off"
              value={value}
              onChange={(event) => setValue(event.target.value)}
            />
          </Field>
        </div>
        <p className="muted small">{t("phpSite.secretsNote")}</p>
        {error && <p className="field-error">{error}</p>}
      </div>
    </ConfirmDialog>
  );
}

/** PHP's errors and what the site's scripts write, live from its container. */
export function PhpLogCard({ site }: { site: Site }) {
  const { t } = useTranslation();
  return (
    <LogViewer
      path={`/api/pc/sites/${site.id}/php/logs`}
      fileName={`${site.name}-php.log`}
      title={t("phpSite.errorLog")}
    />
  );
}

/** How often the page counts the site's mail. */
const mailEvery = 5 * 60_000;

/** The site's scheduled tasks and its outgoing mail, as the design's "Cron &
 * e-mail" card has them (#51). */
export function PhpCronMailCard({ site, canAct }: { site: Site; canAct: boolean }) {
  const { t } = useTranslation();
  const { user } = useAuth();
  const [mail, setMail] = useState<PhpMail | null>(null);
  const [mailError, setMailError] = useState("");

  useEffect(() => {
    let live = true;
    const load = () => {
      if (document.hidden) {
        return;
      }
      phpApi
        .mail(site.id)
        .then((answer) => {
          if (live) {
            setMail(answer);
            setMailError("");
          }
        })
        .catch((reason: unknown) => {
          if (live && !(reason as { isAbort?: boolean }).isAbort) {
            setMailError(errorMessage(reason));
          }
        });
    };
    load();
    const timer = window.setInterval(load, mailEvery);
    // a tab that comes back reads at once, not at the next interval
    const visible = () => {
      if (!document.hidden) {
        load();
      }
    };
    document.addEventListener("visibilitychange", visible);
    return () => {
      live = false;
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", visible);
    };
  }, [site.id]);
  // PHP-FPM younger than a day: its log, and so the count, starts with it
  const since =
    mail?.since && Date.now() - Date.parse(mail.since) < 23 * 60 * 60 * 1000 ? mail.since : "";

  return (
    <Card title={t("phpSite.cronMail")}>
      <TaskList target={{ kind: "site", id: site.id }} canAct={canAct} />
      <div className="mail-block" aria-label={t("phpSite.mail")}>
        {mailError ? (
          <p className="muted small">{mailError}</p>
        ) : !mail ? (
          <p className="muted small">{t("common.loading")}</p>
        ) : !mail.relay ? (
          <p className="muted small">
            {t("phpSite.noRelay")}{" "}
            {hasRole(user, "admin") && <Link to="/settings">{t("phpSite.toSettings")}</Link>}
          </p>
        ) : (
          <>
            <div className="small">{t("phpSite.relayVia")}</div>
            <div className="mono small">
              {`${mail.relay.host}:${mail.relay.port} · `}
              {since
                ? t("phpSite.mailSince", {
                    count: mail.sent,
                    formatted: formatNumber(mail.sent),
                    time: formatTime(since).slice(0, 5),
                  })
                : t("phpSite.mailToday", { count: mail.sent, formatted: formatNumber(mail.sent) })}
              {mail.failed > 0 && (
                <span className="err-text">
                  {` · ${t("phpSite.mailFailed", { count: mail.failed, formatted: formatNumber(mail.failed) })}`}
                </span>
              )}
            </div>
            {mail.failures.length > 0 && (
              <ul className="mail-failures">
                {mail.failures.map((failure) => (
                  <li key={`${failure.at} ${failure.recipients}`}>
                    <span className="mono">{formatTime(failure.at).slice(0, 5)}</span>{" "}
                    <span className="mono">{failure.recipients}</span>
                    <div className="muted small">{failure.error}</div>
                  </li>
                ))}
              </ul>
            )}
          </>
        )}
      </div>
    </Card>
  );
}

/** "Update WordPress" (#52): a new release, a copy of the live one with the
 * application's updates and the plugins and themes to add, so a broken
 * update is one rollback away. */
export function PhpUpdateButton({
  site,
  settings,
  disabled,
}: {
  site: Site;
  settings: SitePhp;
  disabled?: boolean;
}) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const template = settings.template === "custom" ? null : phpTemplates[settings.template];
  if (!template?.update || settings.mode === "classic") {
    return null;
  }
  const name = t(`phpSite.template.${settings.template}`);
  return (
    <>
      {/* an update is a copy of the live release, which a site may not have yet */}
      <Button disabled={disabled} onClick={() => setOpen(true)}>
        {t("phpSite.update", { name })}
      </Button>
      {open && <UpdateDialog site={site} name={name} onClose={() => setOpen(false)} />}
    </>
  );
}

function UpdateDialog({ site, name, onClose }: { site: Site; name: string; onClose: () => void }) {
  const { t } = useTranslation();
  const toast = useToast();
  const [plugins, setPlugins] = useState("");
  const [themes, setThemes] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const words = (text: string) => text.split(/[\s,]+/).filter(Boolean);
  const submit = async () => {
    setBusy(true);
    setError("");
    try {
      await phpApi.update(site.id, { plugins: words(plugins), themes: words(themes) });
      toast.success(t("phpSite.updateStarted", { name }));
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
      title={t("phpSite.updateTitle", { name })}
      confirmLabel={t("phpSite.updateConfirm")}
      confirmDisabled={busy}
      onCancel={onClose}
      onConfirm={() => void submit()}
    >
      <div className="dialog-form">
        <p>{t("phpSite.updateNote", { name })}</p>
        <Field label={t("phpSite.updatePlugins")} htmlFor="php-update-plugins">
          <input
            id="php-update-plugins"
            className="input mono"
            placeholder="akismet, wordpress-seo"
            value={plugins}
            onChange={(event) => setPlugins(event.target.value)}
          />
        </Field>
        <Field label={t("phpSite.updateThemes")} htmlFor="php-update-themes">
          <input
            id="php-update-themes"
            className="input mono"
            placeholder="twentytwentyfive"
            value={themes}
            onChange={(event) => setThemes(event.target.value)}
          />
        </Field>
        <p className="muted small">{t("phpSite.updateAddNote")}</p>
        {error && <p className="field-error">{error}</p>}
      </div>
    </ConfirmDialog>
  );
}
