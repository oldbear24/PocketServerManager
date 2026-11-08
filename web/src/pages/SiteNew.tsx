import { Check, Circle, X } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useNavigate, useSearchParams } from "react-router-dom";

import { PageHead } from "../components/Layout";
import { useToast } from "../components/toast";
import { Banner, Button, Card, Field, ProgressBar, StatusPill, Table } from "../components/ui";
import { errorMessage } from "../lib/api";
import { hasRole, useAuth } from "../lib/auth";
import { directoryId, type AcmeAccount, type Challenge } from "../lib/certificates";
import { serverLabel, type DbServer } from "../lib/databases";
import { formatBytes, formatDuration, formatNumber, parseDate } from "../lib/format";
import { phpApi, phpTemplates, type PhpTemplate, type PhpVersions } from "../lib/php";
import { useLiveList, useLiveRecord } from "../lib/realtime";
import { provisionApi, type DnsPreview, type PreviewRecord, type SiteSource } from "../lib/sites";
import type { Command, Host } from "../lib/types";
import { BuildFields, fieldErrors, type BuildValues } from "./Sites";

/** The wizard of a new site (#24): one form for its source, its name on the
 * web and its server, and its certificate; the setup that follows shows each
 * step as it runs. */
export function SiteNewPage() {
  const [params, setParams] = useSearchParams();
  const setup = params.get("setup") ?? "";
  return (
    <SiteWizard setup={setup} onStarted={(id) => setParams({ setup: id }, { replace: true })} />
  );
}

function SiteWizard({ setup, onStarted }: { setup: string; onStarted: (id: string) => void }) {
  const { t } = useTranslation();
  const { user } = useAuth();
  const canAct = hasRole(user, "operator");
  const hosts = useLiveList<Host>("hosts", { sort: "name" });
  const accounts = useLiveList<AcmeAccount>("acme_accounts", {});
  const edges = hosts.items.filter((host) => host.roles.includes("edge"));
  // the setup and its steps, once for both columns
  const { record: parent } = useLiveRecord<Command>("commands", setup || undefined);
  const steps = useLiveList<Command>("commands", {
    filter: setup ? `parent = "${setup}"` : undefined,
    sort: "created,id",
    enabled: Boolean(setup),
  });
  // the form's choice of type shapes the timeline before the setup starts
  const [plan, setPlan] = useState<Plan>({ php: false, database: false });

  return (
    <>
      <PageHead
        crumbs={
          <>
            <Link to="/sites">{t("site.list")}</Link> / {t("siteNew.title")}
          </>
        }
        title={t("siteNew.title")}
        description={t("siteNew.description")}
      />
      <div className="columns">
        <div className="column-wide">
          {setup ? (
            <SetupSummary parent={parent} steps={steps.items} />
          ) : (
            <SiteForm
              edges={edges}
              loading={hosts.loading}
              accounts={accounts.items}
              canAct={canAct}
              onStarted={onStarted}
              onPlan={setPlan}
            />
          )}
        </div>
        <div className="column-side wizard-side">
          <Timeline setup={setup} parent={parent} steps={steps.items} canAct={canAct} plan={plan} />
        </div>
      </div>
    </>
  );
}

function SiteForm({
  edges,
  loading,
  accounts,
  canAct,
  onStarted,
  onPlan,
}: {
  edges: Host[];
  loading: boolean;
  accounts: AcmeAccount[];
  canAct: boolean;
  onStarted: (id: string) => void;
  onPlan: (plan: Plan) => void;
}) {
  const { t, i18n } = useTranslation();
  const toast = useToast();
  // a PHP application (#50) runs in a PHP-FPM container of its own
  const [kind, setKind] = useState<"static" | "php">("static");
  const [phpForm, setPhpForm] = useState({
    version: "",
    template: "custom" as PhpTemplate,
    docroot: "public",
    dbServer: "",
    database: "",
    // a template's worker and a fresh install (#52)
    queueWorker: false,
    title: "",
    adminUser: "",
    adminEmail: "",
    adminPassword: "",
    locale: i18n.language.startsWith("cs") ? "cs_CZ" : "en_US",
  });
  // the template the user chose rather than the one the source looks like
  const [templateChosen, setTemplateChosen] = useState(false);
  const setPhp = (change: Partial<typeof phpForm>) =>
    setPhpForm((current) => ({ ...current, ...change }));
  const versions = usePhpVersions(kind === "php");
  const dbServers = useLiveList<DbServer>("db_servers", { sort: "name", enabled: kind === "php" });
  const php = kind === "php";
  const [name, setName] = useState("");
  const [hostname, setHostname] = useState("");
  const [host, setHost] = useState("");
  const [www, setWww] = useState(true);
  const [challenge, setChallenge] = useState<Challenge>("auto");
  const [file, setFile] = useState<File | null>(null);
  const [values, setValues] = useState<BuildValues>({
    source: "git",
    repo_url: "",
    branch: "main",
    token: "",
    tokenSet: false,
    generator: "auto",
    build_image: "",
    build_cmd: "",
    output_dir: "",
  });
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const set = (change: Partial<BuildValues>) => setValues((current) => ({ ...current, ...change }));

  const server = host || edges[0]?.id || "";
  // the database servers of the chosen server, of an engine the application
  // runs on (#52)
  const engines =
    phpForm.template === "custom" ? undefined : phpTemplates[phpForm.template].engines;
  const serversHere = dbServers.items.filter(
    (item) => item.host === server && (!engines || engines.includes(item.engine)),
  );
  const dbServer = serversHere.some((item) => item.id === phpForm.dbServer) ? phpForm.dbServer : "";
  useEffect(() => {
    onPlan({ php, database: php && dbServer !== "" });
  }, [php, dbServer, onPlan]);
  const name_ = hostname.trim().toLowerCase();
  const withWww = www && !name_.startsWith("www.");
  const preview = useDnsPreview(name_, server, withWww);
  // a name of the LAN gets its certificate from the internal CA (#26)
  const lan = preview.data?.lan === true;
  const primary = accounts.find((account) => account.role === "primary");

  const template = phpForm.template === "custom" ? null : phpTemplates[phpForm.template];
  // a template's application without a source of its own is installed fresh (#52)
  const fresh = php && values.source === "empty" && template?.fresh === true;
  const install = {
    title: phpForm.title.trim(),
    admin_user: phpForm.adminUser.trim(),
    admin_email: phpForm.adminEmail.trim(),
    admin_password: phpForm.adminPassword,
    locale: phpForm.locale,
  };
  const installReady =
    install.title !== "" &&
    install.admin_user !== "" &&
    install.admin_email.includes("@") &&
    install.admin_password.length >= 12;
  const needsDatabase = php && template?.database === true && dbServer === "";
  const sourceless = php && values.source === "empty" && template !== null && !template.fresh;
  const detected = useDetected(php, values, file, server);
  // what the source looks like is the template, until one is chosen
  useEffect(() => {
    if (detected.template !== null && !templateChosen) {
      const found =
        detected.template === ""
          ? null
          : phpTemplates[detected.template as keyof typeof phpTemplates];
      setPhpForm((current) => ({
        ...current,
        template: (detected.template || "custom") as PhpTemplate,
        docroot: found ? found.docroot : current.docroot,
      }));
    }
  }, [detected.template, templateChosen]);
  const chooseTemplate = (value: PhpTemplate) => {
    setTemplateChosen(true);
    const chosen = value === "custom" ? null : phpTemplates[value];
    setPhp({ template: value, docroot: chosen ? chosen.docroot : "public" });
  };

  const builds = values.source !== "empty" && (php || values.generator !== "none");
  const start = async () => {
    setBusy(true);
    setErrors({});
    try {
      const siteName = (name.trim() || name_.split(".")[0] || "").toLowerCase();
      const site: Record<string, unknown> = {
        name: siteName,
        host: server,
        type: kind,
        source: values.source,
        repo_url: values.source === "git" ? values.repo_url.trim() : "",
        branch: values.source === "git" ? values.branch.trim() : "",
        // a placeholder is built by nobody: the hidden fields go back to their
        // defaults, as does what "as it is" does not build
        generator: values.source === "empty" || php ? "auto" : values.generator,
        build_image: builds && !php ? values.build_image.trim() : "",
        build_cmd: builds ? values.build_cmd.trim() : "",
        output_dir: values.source === "empty" || php ? "" : values.output_dir.trim(),
        https_redirect: true,
        compress: true,
        ...(values.source === "git" && values.token.trim()
          ? { git_token: values.token.trim() }
          : {}),
      };
      const started = await provisionApi.start(
        {
          site,
          hostname: name_,
          www: withWww,
          challenge,
          ...(php
            ? {
                php: {
                  version: phpForm.version || versions?.default || "",
                  template: phpForm.template,
                  // a placeholder's page is the release itself
                  docroot:
                    values.source === "empty"
                      ? ""
                      : phpForm.docroot.trim().replace(/^\/+|\/+$/g, ""),
                  ...(dbServer
                    ? {
                        database_server: dbServer,
                        database: phpForm.database.trim() || siteName.replace(/-/g, "_"),
                      }
                    : {}),
                  ...(fresh ? { install } : {}),
                  ...(template?.worker && phpForm.queueWorker ? { queue_worker: true } : {}),
                },
              }
            : {}),
        },
        values.source === "upload" && file ? file : undefined,
      );
      toast.success(t("siteNew.started", { hostname: name_ }));
      onStarted(started.provision);
    } catch (reason) {
      setErrors(fieldErrors(reason));
    } finally {
      setBusy(false);
    }
  };

  // errors of fields the form does not show now still say why
  const shown = new Set(["hostname", "host", "name", "file"]);
  if (values.source !== "empty") {
    for (const field of php
      ? ["build_cmd"]
      : ["generator", "build_image", "build_cmd", "output_dir"]) {
      shown.add(field);
    }
  }
  if (values.source === "git") {
    for (const field of ["repo_url", "branch", "git_token"]) {
      shown.add(field);
    }
  }
  const unshown = Object.entries(errors)
    .filter(([field]) => !shown.has(field))
    .map(([, message]) => message);

  const ready =
    canAct &&
    !busy &&
    name_ !== "" &&
    server !== "" &&
    !preview.data?.problem &&
    (preview.data?.taken.length ?? 0) === 0 &&
    (values.source !== "upload" || file !== null) &&
    (!fresh || installReady) &&
    !needsDatabase &&
    !sourceless;

  return (
    <>
      <Card title={t("siteNew.typeTitle")}>
        <ChoiceGroup
          label={t("siteNew.typeTitle")}
          value={kind}
          onChange={(value) => setKind(value as "static" | "php")}
          options={[
            { value: "static", title: t("siteNew.static"), note: t("siteNew.staticNote") },
            { value: "php", title: t("siteNew.php"), note: t("siteNew.phpNote") },
          ]}
        />
      </Card>

      <Card title={t("siteNew.sourceTitle")}>
        <div className="dialog-form">
          <ChoiceGroup
            label={t("site.source")}
            value={values.source}
            onChange={(source) => set({ source: source as SiteSource })}
            options={(["git", "upload", "empty"] as SiteSource[]).map((source) =>
              source === "empty" && php && template?.fresh
                ? {
                    value: source,
                    title: t("siteNew.freshTitle"),
                    note: t("siteNew.freshNote", {
                      name: t(`phpSite.template.${phpForm.template}`),
                    }),
                  }
                : {
                    value: source,
                    title: t(`site.sourceKind.${source}`),
                    note: t(`siteNew.sourceNote.${source}`),
                  },
            )}
          />
          {values.source === "upload" && (
            <Field label={t("siteNew.zip")} htmlFor="new-site-zip" error={errors.file}>
              <input
                id="new-site-zip"
                type="file"
                accept=".zip,application/zip"
                className="input"
                onChange={(event) => setFile(event.target.files?.[0] ?? null)}
              />
            </Field>
          )}
          {values.source !== "empty" && (
            <BuildFields values={values} set={set} errors={errors} idPrefix="wizard" php={php} />
          )}
          {php && values.source !== "empty" && <DetectedLine detected={detected} />}
        </div>
      </Card>

      {php && (
        <Card title={t("siteNew.phpTitle")}>
          <div className="dialog-form">
            <div className="form-row">
              <Field label={t("siteNew.phpVersion")} htmlFor="new-site-php">
                <select
                  id="new-site-php"
                  className="select"
                  value={phpForm.version || versions?.default || ""}
                  onChange={(event) => setPhp({ version: event.target.value })}
                >
                  {(versions?.versions ?? []).map((item) => (
                    <option
                      key={item.version}
                      value={item.version}
                      disabled={item.state === "ended"}
                    >
                      {`PHP ${item.version} · ${t(`php.state.${item.state}`)}${item.version === versions?.default ? ` · ${t("siteNew.recommended")}` : ""}`}
                    </option>
                  ))}
                </select>
              </Field>
              <Field label={t("siteNew.application")} htmlFor="new-site-template">
                <select
                  id="new-site-template"
                  className="select"
                  value={phpForm.template}
                  onChange={(event) => chooseTemplate(event.target.value as PhpTemplate)}
                >
                  <option value="custom">{t("phpSite.template.custom")}</option>
                  {(["wordpress", "laravel", "nette"] as const).map((name) => (
                    <option key={name} value={name}>
                      {t(`phpSite.template.${name}`)}
                    </option>
                  ))}
                </select>
              </Field>
            </div>
            {template && (
              <p className="muted small">{t(`siteNew.templateNote.${phpForm.template}`)}</p>
            )}
            {sourceless && (
              <p className="field-error small">
                {t("siteNew.templateNeedsSource", {
                  name: t(`phpSite.template.${phpForm.template}`),
                })}
              </p>
            )}
            {fresh && (
              <>
                <div className="form-row">
                  <Field label={t("siteNew.freshSiteTitle")} htmlFor="new-site-title">
                    <input
                      id="new-site-title"
                      className="input"
                      value={phpForm.title}
                      onChange={(event) => setPhp({ title: event.target.value })}
                    />
                  </Field>
                  <Field label={t("siteNew.freshLocale")} htmlFor="new-site-locale">
                    <select
                      id="new-site-locale"
                      className="select"
                      value={phpForm.locale}
                      onChange={(event) => setPhp({ locale: event.target.value })}
                    >
                      {["cs_CZ", "en_US", "en_GB", "sk_SK", "de_DE", "pl_PL"].map((locale) => (
                        <option key={locale} value={locale}>
                          {t(`siteNew.locale.${locale}`)}
                        </option>
                      ))}
                    </select>
                  </Field>
                </div>
                <div className="form-row">
                  <Field label={t("siteNew.freshAdmin")} htmlFor="new-site-admin">
                    <input
                      id="new-site-admin"
                      className="input"
                      autoComplete="off"
                      value={phpForm.adminUser}
                      onChange={(event) => setPhp({ adminUser: event.target.value })}
                    />
                  </Field>
                  <Field label={t("siteNew.freshEmail")} htmlFor="new-site-admin-email">
                    <input
                      id="new-site-admin-email"
                      className="input"
                      type="email"
                      value={phpForm.adminEmail}
                      onChange={(event) => setPhp({ adminEmail: event.target.value })}
                    />
                  </Field>
                </div>
                <Field
                  label={t("siteNew.freshPassword")}
                  htmlFor="new-site-admin-password"
                  error={
                    phpForm.adminPassword !== "" && phpForm.adminPassword.length < 12
                      ? t("siteNew.freshPasswordShort")
                      : undefined
                  }
                >
                  <input
                    id="new-site-admin-password"
                    className="input"
                    type="password"
                    autoComplete="new-password"
                    value={phpForm.adminPassword}
                    onChange={(event) => setPhp({ adminPassword: event.target.value })}
                  />
                </Field>
                <p className="muted small">{t("siteNew.freshPasswordNote")}</p>
              </>
            )}
            {values.source !== "empty" && (
              <>
                <Field label={t("phpSite.webRoot")} htmlFor="new-site-docroot">
                  <input
                    id="new-site-docroot"
                    className="input mono"
                    placeholder={t("siteNew.docrootNone")}
                    value={phpForm.docroot}
                    onChange={(event) => setPhp({ docroot: event.target.value })}
                  />
                </Field>
                <p className="muted small">{t("siteNew.docrootNote")}</p>
              </>
            )}
            <div className="form-row">
              <Field label={t("phpSite.database")} htmlFor="new-site-db-server">
                <select
                  id="new-site-db-server"
                  className="select"
                  value={dbServer}
                  onChange={(event) => setPhp({ dbServer: event.target.value })}
                >
                  <option value="">{t("siteNew.noDatabase")}</option>
                  {serversHere.map((item) => (
                    <option key={item.id} value={item.id}>
                      {`${item.name} · ${serverLabel(item)}`}
                    </option>
                  ))}
                </select>
              </Field>
              {dbServer && (
                <Field label={t("siteNew.databaseName")} htmlFor="new-site-db-name">
                  <input
                    id="new-site-db-name"
                    className="input mono"
                    placeholder={(name.trim() || name_.split(".")[0] || "shop").replace(/-/g, "_")}
                    value={phpForm.database}
                    onChange={(event) => setPhp({ database: event.target.value })}
                  />
                </Field>
              )}
            </div>
            <p className="muted small">
              {serversHere.length === 0 && !dbServers.loading ? (
                <>
                  {t("siteNew.noDatabaseServer")}{" "}
                  <Link to="/databases">{t("phpSite.toDatabases")}</Link>
                </>
              ) : dbServer ? (
                t("siteNew.databaseNote")
              ) : needsDatabase ? (
                <span className="warn-text">
                  {t("siteNew.databaseRequired", {
                    name: t(`phpSite.template.${phpForm.template}`),
                  })}
                </span>
              ) : (
                t("siteNew.noDatabaseNote")
              )}
            </p>
            {template?.worker && (
              <label className="check-line">
                <input
                  type="checkbox"
                  checked={phpForm.queueWorker}
                  onChange={(event) => setPhp({ queueWorker: event.target.checked })}
                />
                <span>
                  {t("siteNew.queueWorker")}
                  <span className="muted small"> · {t("siteNew.queueWorkerNote")}</span>
                </span>
              </label>
            )}
          </div>
        </Card>
      )}

      <Card title={t(php ? "siteNew.domainTitlePhp" : "siteNew.domainTitle")}>
        <div className="dialog-form">
          {!loading && edges.length === 0 && <Banner tone="warn">{t("site.noEdge")}</Banner>}
          <div className="form-row">
            <Field
              label={t("siteNew.hostname")}
              htmlFor="new-site-hostname"
              error={errors.hostname}
            >
              <input
                id="new-site-hostname"
                className="input mono"
                placeholder="docs.example.com"
                autoComplete="off"
                value={hostname}
                onChange={(event) => setHostname(event.target.value)}
              />
            </Field>
            <Field label={t("siteNew.server")} htmlFor="new-site-host" error={errors.host}>
              <select
                id="new-site-host"
                className="select"
                value={server}
                onChange={(event) => setHost(event.target.value)}
              >
                {edges.map((item) => (
                  <option key={item.id} value={item.id}>
                    {item.public_ip4 || item.public_ip6
                      ? `${item.name} · ${item.public_ip4 || item.public_ip6}`
                      : item.name}
                  </option>
                ))}
              </select>
            </Field>
          </div>
          <Field label={t("siteNew.siteName")} htmlFor="new-site-name" error={errors.name}>
            <input
              id="new-site-name"
              className="input mono"
              placeholder={name_.split(".")[0] || "docs"}
              value={name}
              onChange={(event) => setName(event.target.value)}
            />
          </Field>
          {name_ !== "" && <ZoneBanner preview={preview.data} error={preview.error} />}
          {preview.data && preview.data.records.length > 0 && !preview.data.problem && (
            <RecordsTable records={preview.data.records} label={t("siteNew.records")} />
          )}
          {!name_.startsWith("www.") && (
            <label className="check-line">
              <input
                type="checkbox"
                checked={www}
                onChange={(event) => setWww(event.target.checked)}
              />
              <span>
                {t("siteNew.www", { hostname: name_ ? `www.${name_}` : "www.docs.example.com" })}
              </span>
            </label>
          )}
        </div>
      </Card>

      <Card title={t(php ? "siteNew.certificateTitlePhp" : "siteNew.certificateTitle")}>
        <div className="dialog-form">
          <div className="form-row">
            <Field label={t("acme.ca")} htmlFor="new-site-ca">
              <input
                id="new-site-ca"
                className="input"
                readOnly
                value={
                  lan
                    ? t("cert.issuer.internal")
                    : primary
                      ? directoryId(primary.directory_url) === "custom"
                        ? primary.directory_url
                        : t(`cert.ca.${directoryId(primary.directory_url)}`)
                      : t("siteNew.noAccount")
                }
              />
            </Field>
            {!lan && (
              <Field label={t("siteNew.challenge")} htmlFor="new-site-challenge">
                <select
                  id="new-site-challenge"
                  className="select"
                  value={challenge}
                  onChange={(event) => setChallenge(event.target.value as Challenge)}
                >
                  <option value="auto">{t("cert.challengeAuto")}</option>
                  <option value="http-01">HTTP-01</option>
                  <option value="dns-01">DNS-01</option>
                </select>
              </Field>
            )}
          </div>
          <p className="muted small">
            {lan ? t("siteNew.internalNote") : t(`cert.challengeNote.${challenge}`)}
          </p>
          {!primary && !lan && (
            <Banner tone="warn">
              <span>
                {t("siteNew.noAccountText")} <Link to="/settings">{t("siteNew.settings")}</Link>
              </span>
            </Banner>
          )}
        </div>
      </Card>

      {unshown.length > 0 && <Banner tone="err">{unshown.join(" ")}</Banner>}
      <div className="button-row">
        <Button variant="primary" disabled={!ready} onClick={() => void start()}>
          {busy ? t("siteNew.starting") : t("siteNew.start")}
        </Button>
        <Link className="button" to="/sites">
          {t("common.cancel")}
        </Link>
      </div>
    </>
  );
}

type Choice = { value: string; title: string; note: string; disabled?: boolean };

/** A row of cards to pick one from, as radio buttons: the chosen one takes
 * the focus, and the arrow keys move the choice. */
function ChoiceGroup({
  label,
  value,
  options,
  onChange,
}: {
  label: string;
  value: string;
  options: Choice[];
  onChange: (value: string) => void;
}) {
  const refs = useRef<(HTMLButtonElement | null)[]>([]);
  const enabled = options.flatMap((option, index) => (option.disabled ? [] : [index]));
  const move = (from: number, step: number) => {
    const position = enabled.indexOf(from);
    const next = enabled[(position + step + enabled.length) % enabled.length];
    onChange(options[next].value);
    refs.current[next]?.focus();
  };
  return (
    <div
      className={`choice-grid${options.length > 2 ? " three" : ""}`}
      role="radiogroup"
      aria-label={label}
    >
      {options.map((option, index) => {
        const checked = option.value === value;
        return (
          <button
            key={option.value}
            ref={(element) => {
              refs.current[index] = element;
            }}
            type="button"
            role="radio"
            aria-checked={checked}
            tabIndex={checked ? 0 : -1}
            disabled={option.disabled}
            className={`choice${checked ? " active" : ""}`}
            onClick={() => onChange(option.value)}
            onKeyDown={(event) => {
              if (event.key === "ArrowRight" || event.key === "ArrowDown") {
                event.preventDefault();
                move(index, 1);
              } else if (event.key === "ArrowLeft" || event.key === "ArrowUp") {
                event.preventDefault();
                move(index, -1);
              }
            }}
          >
            <span className="choice-title">{option.title}</span>
            <span className="choice-note">{option.note}</span>
          </button>
        );
      })}
    </div>
  );
}

/** Asks which zone holds the name while it is typed, a moment after the last key. */
function useDnsPreview(hostname: string, host: string, www: boolean) {
  const [data, setData] = useState<DnsPreview | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    if (!hostname || !host || !hostname.includes(".")) {
      setData(null);
      setError("");
      return;
    }
    let live = true;
    const timer = setTimeout(() => {
      provisionApi
        .preview(hostname, host, www)
        .then((answer) => {
          if (live) {
            setData(answer);
            setError("");
          }
        })
        .catch((reason: unknown) => {
          if (live && !(reason as { isAbort?: boolean }).isAbort) {
            setData(null);
            setError(errorMessage(reason));
          }
        });
    }, 350);
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [hostname, host, www]);
  return { data, error };
}

function ZoneBanner({ preview, error }: { preview: DnsPreview | null; error: string }) {
  const { t } = useTranslation();
  if (error) {
    return <Banner tone="err">{error}</Banner>;
  }
  if (!preview) {
    return null;
  }
  if (preview.problem) {
    return <Banner tone="err">{preview.problem}</Banner>;
  }
  if (preview.taken.length > 0) {
    return (
      <Banner tone="err">
        {t("siteNew.taken", { count: preview.taken.length, names: preview.taken.join(", ") })}
      </Banner>
    );
  }
  if (preview.mode === "automatic") {
    return (
      <Banner tone="ok">
        <Check size={16} aria-hidden="true" />
        <span>{t("siteNew.automatic", { zone: preview.zone, provider: preview.provider })}</span>
      </Banner>
    );
  }
  return (
    <Banner tone="info">
      <span>
        {preview.zone ? t("siteNew.manualZone", { zone: preview.zone }) : t("siteNew.manual")}
      </span>
    </Banner>
  );
}

function RecordsTable({ records, label }: { records: PreviewRecord[]; label: string }) {
  const { t } = useTranslation();
  return (
    <Table label={label} compact>
      <thead>
        <tr>
          <th scope="col">{t("siteNew.recordType")}</th>
          <th scope="col">{t("siteNew.recordName")}</th>
          <th scope="col">{t("siteNew.recordValue")}</th>
          <th scope="col">TTL</th>
        </tr>
      </thead>
      <tbody>
        {records.map((record) => (
          <tr key={`${record.type} ${record.name} ${record.value}`}>
            <td className="mono record-type">{record.type}</td>
            <td className="mono">{record.name}</td>
            <td className="mono">{record.value}</td>
            <td className="mono">{record.ttl}</td>
          </tr>
        ))}
      </tbody>
    </Table>
  );
}

/** What a new PHP site's source looks like (#52): null until asked, "" for
 * none of the templates'. */
type Detected = { template: string | null; title: string; busy: boolean; error: string };

/** Asks the panel which PHP application the wizard's source is: an upload's
 * files at once, a repository's a moment after its address was typed. */
function useDetected(php: boolean, values: BuildValues, file: File | null, host: string): Detected {
  const [detected, setDetected] = useState<Detected>({
    template: null,
    title: "",
    busy: false,
    error: "",
  });
  const repo = values.source === "git" ? values.repo_url.trim() : "";
  const branch = values.branch.trim();
  const token = values.token.trim();
  useEffect(() => {
    const git = values.source === "git" && /^https:\/\/[^\s/]+\/\S+/.test(repo) && host !== "";
    const upload = values.source === "upload" && file !== null;
    if (!php || (!git && !upload)) {
      setDetected({ template: null, title: "", busy: false, error: "" });
      return;
    }
    const abort = new AbortController();
    const timer = window.setTimeout(
      () => {
        setDetected((current) => ({ ...current, busy: true, error: "" }));
        provisionApi
          .detect(
            upload
              ? { source: "upload" }
              : {
                  source: "git",
                  host,
                  repo_url: repo,
                  branch,
                  ...(token ? { git_token: token } : {}),
                },
            upload ? (file ?? undefined) : undefined,
            abort.signal,
          )
          .then((answer) =>
            setDetected({ template: answer.template, title: answer.title, busy: false, error: "" }),
          )
          .catch((reason: unknown) => {
            if (!abort.signal.aborted) {
              setDetected({ template: null, title: "", busy: false, error: errorMessage(reason) });
            }
          });
      },
      upload ? 0 : 800,
    );
    return () => {
      window.clearTimeout(timer);
      abort.abort();
    };
  }, [php, values.source, repo, branch, token, file, host]);
  return detected;
}

/** "Detected: Laravel", as the static site builders say theirs. */
function DetectedLine({ detected }: { detected: Detected }) {
  const { t } = useTranslation();
  if (detected.busy) {
    return <p className="muted small">{t("siteNew.detecting")}</p>;
  }
  if (detected.error) {
    return <p className="muted small">{t("siteNew.detectFailed", { error: detected.error })}</p>;
  }
  if (detected.template === null) {
    return null;
  }
  if (detected.template === "") {
    return <p className="muted small">{t("siteNew.detectedNone")}</p>;
  }
  return (
    <p className="small ok-text detected-line">
      <Check size={14} aria-hidden="true" />
      {t("siteNew.detected", { name: detected.title })}
    </p>
  );
}

type SetupPayload = {
  site: string;
  site_name: string;
  hostname: string;
  www?: string;
  /** a PHP site's runtime (#50) */
  php?: { version: string; database_server?: string; database?: string };
};

/** What a setup runs: a PHP site's database and PHP-FPM come before its release. */
type Plan = { php: boolean; database: boolean };

/** The PHP versions of the runtime images, once the form asks for them. */
function usePhpVersions(enabled: boolean): PhpVersions | null {
  const [versions, setVersions] = useState<PhpVersions | null>(null);
  useEffect(() => {
    if (!enabled || versions) {
      return;
    }
    let live = true;
    phpApi
      .versions()
      .then((answer) => live && setVersions(answer))
      .catch(() => {
        // the panel picks its default
      });
    return () => {
      live = false;
    };
  }, [enabled, versions]);
  return versions;
}

/** What a setup under way was asked for, in place of the form. */
function SetupSummary({ parent: record, steps }: { parent: Command | null; steps: Command[] }) {
  const { t } = useTranslation();
  const payload = (record?.payload ?? {}) as SetupPayload;
  if (!record) {
    return <p className="muted">{t("common.loading")}</p>;
  }
  // records made by hand: the setup waits for them, so they come first
  const dns = steps.filter((step) => step.type === "site.dns").at(-1);
  const toMake = dns?.status === "succeeded" && isManual(dns);
  return (
    <>
      {toMake && (
        <Card title={t("siteNew.recordsToMake")} note={t("siteNew.recordsToMakeNote")}>
          <RecordsTable
            records={(dns.result?.records as PreviewRecord[]) ?? []}
            label={t("siteNew.recordsToMake")}
          />
        </Card>
      )}
      <SummaryCard record={record} payload={payload} />
    </>
  );
}

function SummaryCard({ record, payload }: { record: Command; payload: SetupPayload }) {
  const { t } = useTranslation();
  return (
    <Card title={t("siteNew.summaryTitle")}>
      <dl className="facts">
        <dt>{t("siteNew.siteName")}</dt>
        <dd className="mono">{payload.site_name}</dd>
        <dt>{t("siteNew.hostname")}</dt>
        <dd className="mono">
          {payload.hostname}
          {payload.www ? ` · ${payload.www}` : ""}
        </dd>
        <dt>{t("siteNew.server")}</dt>
        <dd>{record.host_name}</dd>
        <dt>{t("siteNew.requestedBy")}</dt>
        <dd>{record.requested_label}</dd>
      </dl>
    </Card>
  );
}

/** The rows of the timeline: the steps, the deployment split into its build
 * and its release. A PHP site's database and PHP-FPM come before them. */
type Row =
  | "site.create"
  | "site.dns"
  | "site.propagation"
  | "db.database.create"
  | "site.php.apply"
  | "build"
  | "release"
  | "site.certificate"
  | "site.check";

function rowsOf(plan: Plan): Row[] {
  return [
    "site.create",
    "site.dns",
    "site.propagation",
    ...(plan.php && plan.database ? (["db.database.create"] as const) : []),
    ...(plan.php ? (["site.php.apply"] as const) : []),
    "build",
    "release",
    "site.certificate",
    "site.check",
  ];
}

type RowState = "done" | "running" | "failed" | "waiting";

function Timeline({
  setup,
  parent,
  steps,
  canAct,
  plan,
}: {
  setup: string;
  parent: Command | null;
  steps: Command[];
  canAct: boolean;
  plan: Plan;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const navigate = useNavigate();
  const [busy, setBusy] = useState(false);

  // the newest command of each step: a retried step's last attempt
  const latest = useMemo(() => {
    const byType = new Map<string, Command>();
    for (const step of steps) {
      byType.set(step.type, step);
    }
    return byType;
  }, [steps]);

  const payload = (parent?.payload ?? {}) as SetupPayload;
  const php = setup ? Boolean(payload.php) : plan.php;
  const rows = rowsOf(
    setup ? { php: Boolean(payload.php), database: Boolean(payload.php?.database_server) } : plan,
  );
  const states = rows.map((row) => rowState(row, latest));
  // the DNS step says whether the name is one of the LAN
  const lan = Boolean((latest.get("site.dns")?.result as { lan?: boolean } | null)?.lan);
  const done = states.filter((state) => state === "done").length;
  const current = Math.min(
    states.findIndex((state) => state !== "done"),
    rows.length - 1,
  );
  const finished = parent?.status === "succeeded";
  const ended =
    parent &&
    (parent.status === "failed" || parent.status === "cancelled" || parent.status === "timed_out");
  const startFailed = Boolean(
    parent?.status === "running" &&
    (parent.progress as { paused?: string } | null)?.paused?.startsWith("start:"),
  );
  const paused = Boolean(
    parent?.status === "running" && (parent.progress as { paused?: string } | null)?.paused,
  );

  const act = async (action: () => Promise<void>, success: string) => {
    setBusy(true);
    try {
      await action();
      toast.success(success);
    } catch (reason) {
      toast.error(errorMessage(reason));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card
      title={t("siteNew.progress")}
      side={
        setup ? (
          <StatusPill tone={finished ? "ok" : ended ? "err" : paused ? "warn" : "info"} dot={false}>
            {finished
              ? t("siteNew.done")
              : t("siteNew.stepOf", { step: current + 1, total: rows.length })}
          </StatusPill>
        ) : undefined
      }
    >
      <div className="timeline-wrap">
        <ProgressBar
          value={(done / rows.length) * 100}
          label={t("siteNew.progress")}
          warnAt={101}
          errAt={101}
        />
        <ol className="timeline">
          {rows.map((row, index) => {
            const state = setup ? states[index] : "waiting";
            const step = commandOf(row, latest);
            return (
              <li key={row} className={`timeline-step ${state}`}>
                <span className="timeline-mark" aria-hidden="true">
                  {state === "done" ? (
                    <Check size={14} />
                  ) : state === "failed" ? (
                    <X size={14} />
                  ) : state === "running" ? (
                    <Circle size={8} fill="currentColor" />
                  ) : (
                    index + 1
                  )}
                </span>
                <div className="timeline-body">
                  <div className="timeline-title">
                    <span>{t(`siteNew.step.${row}`)}</span>
                    <span className="visually-hidden">{t(`siteNew.state.${state}`)}</span>
                    {step && state !== "waiting" && <StepTime step={step} state={state} />}
                  </div>
                  <div className="timeline-detail mono">
                    {step
                      ? stepDetail(row, step, t, planOf(row, lan, php, t))
                      : planOf(row, lan, php, t)}
                  </div>
                  {state === "failed" && step?.error && <p className="field-error">{step.error}</p>}
                  {state === "failed" && paused && canAct && (
                    <Button
                      size="small"
                      disabled={busy}
                      onClick={() =>
                        void act(() => provisionApi.retry(setup), t("siteNew.retrying"))
                      }
                    >
                      {t("siteNew.retry")}
                    </Button>
                  )}
                </div>
              </li>
            );
          })}
        </ol>
        {finished && (
          <Banner tone="ok">
            <Check size={16} aria-hidden="true" />
            <span>
              {t("siteNew.live")}{" "}
              <a href={`https://${payload.hostname}`} target="_blank" rel="noreferrer">
                https://{payload.hostname}
              </a>
            </span>
          </Banner>
        )}
        {ended && (
          <Banner tone="err">
            <span>
              {t("siteNew.ended", { error: (parent?.error ?? "").replace(/\.$/, "") })}{" "}
              {t("siteNew.endedCleanup")}
            </span>
          </Banner>
        )}
        {startFailed && (
          <Banner tone="err">
            <span>
              {(parent?.progress as { message?: string } | null)?.message}{" "}
              {canAct && (
                <Button
                  size="small"
                  disabled={busy}
                  onClick={() => void act(() => provisionApi.retry(setup), t("siteNew.retrying"))}
                >
                  {t("siteNew.retry")}
                </Button>
              )}
            </span>
          </Banner>
        )}
        <div className="button-row">
          {payload.site && (finished || ended) && (
            <Button onClick={() => navigate(`/sites/${payload.site}`)}>
              {t("siteNew.openSite")}
            </Button>
          )}
          {parent?.status === "running" && canAct && (
            <Button
              variant="danger"
              disabled={busy}
              onClick={() => void act(() => provisionApi.cancel(setup), t("siteNew.cancelled"))}
            >
              {t("siteNew.cancel")}
            </Button>
          )}
        </div>
        <p className="muted small">{t("siteNew.history")}</p>
      </div>
    </Card>
  );
}

function commandOf(row: Row, latest: Map<string, Command>): Command | undefined {
  return row === "build" || row === "release" ? latest.get("site.deploy") : latest.get(row);
}

function rowState(row: Row, latest: Map<string, Command>): RowState {
  const step = commandOf(row, latest);
  if (!step) {
    return "waiting";
  }
  const final = step.status === "succeeded";
  const failed =
    step.status === "failed" || step.status === "timed_out" || step.status === "cancelled";
  if (row === "build" || row === "release") {
    // the agent says how far its deployment is: source, build, release
    const releasing = step.progress?.step === "release";
    if (row === "build") {
      return final || releasing ? "done" : failed ? "failed" : "running";
    }
    if (final) {
      return "done";
    }
    if (releasing) {
      return failed ? "failed" : "running";
    }
    return "waiting";
  }
  return final ? "done" : failed ? "failed" : "running";
}

function isManual(step: Command): boolean {
  return step.result?.mode === "manual";
}

function StepTime({ step, state }: { step: Command; state: RowState }) {
  const { t } = useTranslation();
  const at = parseDate(state === "running" ? step.started_at : step.finished_at || step.started_at);
  if (state === "running") {
    return <span className="timeline-time">{t("siteNew.now")}</span>;
  }
  return (
    <span className="timeline-time mono">
      {at
        ? at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })
        : ""}
    </span>
  );
}

type Translate = ReturnType<typeof useTranslation>["t"];

/** One line under a step: what it did, or what it waits for. */
/** What a step does before it ran: a name of the LAN skips the world (#26). */
function planOf(row: Row, lan: boolean, php: boolean, t: Translate): string {
  if (lan && (row === "site.propagation" || row === "site.certificate" || row === "site.check")) {
    return t(`siteNew.planLan.${row.slice("site.".length)}`);
  }
  if (php && (row === "build" || row === "release")) {
    return t(`siteNew.planPhp.${row}`);
  }
  return t(`siteNew.plan.${row}`);
}

function stepDetail(row: Row, step: Command, t: Translate, plan: string): string {
  const result = (step.result ?? {}) as Record<string, unknown>;
  const progress = step.progress?.message ?? "";
  if (step.status !== "succeeded" && row !== "build" && row !== "release") {
    return progress || plan;
  }
  switch (row) {
    case "site.create":
      return `${String(result.name ?? "")} → ${String(result.server ?? step.host_name)}`;
    case "site.dns": {
      const types = Array.from(
        new Set(((result.records as PreviewRecord[]) ?? []).map((record) => record.type)),
      ).join(" + ");
      return result.mode === "manual"
        ? t("siteNew.dnsManual", { types })
        : t("siteNew.dnsAutomatic", { types, provider: String(result.provider ?? "") });
    }
    case "db.database.create":
      return t("siteNew.databaseMade", {
        database: String(result.database ?? ""),
        user: String(result.user ?? ""),
      });
    case "site.php.apply":
      return [String(result.image ?? ""), String(result.container ?? "").slice(0, 12)]
        .filter(Boolean)
        .join(" · ");
    case "site.propagation":
      if (result.lan) {
        return t("siteNew.lanAnswered", { provider: String(result.lan) });
      }
      return [
        ...((result.resolvers as string[]) ?? []).map((r) => r.replace(/:53$/, "")),
        t("siteNew.authoritative"),
      ].join(" · ");
    case "build": {
      // the agent's own words while it fetches and builds; the release is
      // the next row's
      const stage = step.progress?.step ?? "";
      if (step.status !== "succeeded") {
        return stage === "source" || stage === "build" ? progress : plan;
      }
      return [String(result.generator ?? ""), formatDuration(Number(result.build_ms ?? 0) / 1000)]
        .filter(Boolean)
        .join(" · ");
    }
    case "release":
      if (step.status !== "succeeded") {
        return progress && step.progress?.step === "release" ? progress : plan;
      }
      return [
        "site.deploy",
        t("siteNew.files", {
          count: Number(result.files ?? 0),
          formatted: formatNumber(Number(result.files ?? 0)),
        }),
        formatBytes(Number(result.size_bytes ?? 0)),
      ].join(" · ");
    case "site.certificate":
      return [String(result.issuer ?? ""), ((result.names as string[]) ?? []).join(", ")]
        .filter(Boolean)
        .join(" · ");
    case "site.check":
      return [
        `${String(result.url ?? "")} → ${String(result.status ?? "")}`,
        String(result.tls ?? ""),
        String(result.protocol ?? ""),
      ]
        .filter(Boolean)
        .join(" · ");
  }
  return "";
}
