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
import { formatBytes, formatDuration, formatNumber, parseDate } from "../lib/format";
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
            />
          )}
        </div>
        <div className="column-side wizard-side">
          <Timeline setup={setup} parent={parent} steps={steps.items} canAct={canAct} />
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
}: {
  edges: Host[];
  loading: boolean;
  accounts: AcmeAccount[];
  canAct: boolean;
  onStarted: (id: string) => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
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
  const name_ = hostname.trim().toLowerCase();
  const withWww = www && !name_.startsWith("www.");
  const preview = useDnsPreview(name_, server, withWww);
  // a name of the LAN gets its certificate from the internal CA (#26)
  const lan = preview.data?.lan === true;
  const primary = accounts.find((account) => account.role === "primary");

  const builds = values.source !== "empty" && values.generator !== "none";
  const start = async () => {
    setBusy(true);
    setErrors({});
    try {
      const site: Record<string, unknown> = {
        name: (name.trim() || name_.split(".")[0] || "").toLowerCase(),
        host: server,
        source: values.source,
        repo_url: values.source === "git" ? values.repo_url.trim() : "",
        branch: values.source === "git" ? values.branch.trim() : "",
        // a placeholder is built by nobody: the hidden fields go back to their
        // defaults, as does what "as it is" does not build
        generator: values.source === "empty" ? "auto" : values.generator,
        build_image: builds ? values.build_image.trim() : "",
        build_cmd: builds ? values.build_cmd.trim() : "",
        output_dir: values.source === "empty" ? "" : values.output_dir.trim(),
        https_redirect: true,
        compress: true,
        ...(values.source === "git" && values.token.trim()
          ? { git_token: values.token.trim() }
          : {}),
      };
      const started = await provisionApi.start(
        { site, hostname: name_, www: withWww, challenge },
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
    for (const field of ["generator", "build_image", "build_cmd", "output_dir"]) {
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
    (values.source !== "upload" || file !== null);

  return (
    <>
      <Card title={t("siteNew.typeTitle")}>
        <ChoiceGroup
          label={t("siteNew.typeTitle")}
          value="static"
          onChange={() => {}}
          options={[
            { value: "static", title: t("siteNew.static"), note: t("siteNew.staticNote") },
            {
              value: "php",
              title: t("siteNew.php"),
              note: t("siteNew.phpLater"),
              disabled: true,
            },
          ]}
        />
      </Card>

      <Card title={t("siteNew.sourceTitle")}>
        <div className="dialog-form">
          <ChoiceGroup
            label={t("site.source")}
            value={values.source}
            onChange={(source) => set({ source: source as SiteSource })}
            options={(["git", "upload", "empty"] as SiteSource[]).map((source) => ({
              value: source,
              title: t(`site.sourceKind.${source}`),
              note: t(`siteNew.sourceNote.${source}`),
            }))}
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
            <BuildFields values={values} set={set} errors={errors} idPrefix="wizard" />
          )}
        </div>
      </Card>

      <Card title={t("siteNew.domainTitle")}>
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

      <Card title={t("siteNew.certificateTitle")}>
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

type SetupPayload = { site: string; site_name: string; hostname: string; www?: string };

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

/** The rows of the timeline: the six steps, the deployment split into its
 * build and its release. */
const rows = [
  "site.create",
  "site.dns",
  "site.propagation",
  "build",
  "release",
  "site.certificate",
  "site.check",
] as const;
type Row = (typeof rows)[number];

type RowState = "done" | "running" | "failed" | "waiting";

function Timeline({
  setup,
  parent,
  steps,
  canAct,
}: {
  setup: string;
  parent: Command | null;
  steps: Command[];
  canAct: boolean;
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

  const states = rows.map((row) => rowState(row, latest));
  const done = states.filter((state) => state === "done").length;
  const current = Math.min(
    states.findIndex((state) => state !== "done"),
    rows.length - 1,
  );
  const payload = (parent?.payload ?? {}) as SetupPayload;
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
                    {step ? stepDetail(row, step, t) : t(`siteNew.plan.${row}`)}
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
function stepDetail(row: Row, step: Command, t: Translate): string {
  const result = (step.result ?? {}) as Record<string, unknown>;
  const progress = step.progress?.message ?? "";
  if (step.status !== "succeeded" && row !== "build" && row !== "release") {
    return progress || t(`siteNew.plan.${row}`);
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
        return stage === "source" || stage === "build" ? progress : t("siteNew.plan.build");
      }
      return [String(result.generator ?? ""), formatDuration(Number(result.build_ms ?? 0) / 1000)]
        .filter(Boolean)
        .join(" · ");
    }
    case "release":
      if (step.status !== "succeeded") {
        return progress && step.progress?.step === "release" ? progress : t("siteNew.plan.release");
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
