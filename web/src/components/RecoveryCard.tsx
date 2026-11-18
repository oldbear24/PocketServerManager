import { History } from "lucide-react";
import { useCallback, useEffect, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Link, useLocation } from "react-router-dom";

import { errorMessage } from "../lib/api";
import { formatDateTime, formatRelative, parseDate } from "../lib/format";
import { useMounted } from "../lib/mounted";
import { useNow } from "../lib/realtime";
import {
  generatePassphrase,
  maxKitBytes,
  minPassphrase,
  passphraseLength,
  recoveryApi,
  type ExportedKit,
  type KitCheck,
  type KitSection,
  type RecoveryState,
} from "../lib/recovery";
import { useToast } from "./toast";
import {
  Banner,
  Button,
  Card,
  CodeBlock,
  ConfirmDialog,
  Field,
  IssueBanner,
  StatusPill,
} from "./ui";

type Translate = (key: string, options?: Record<string, unknown>) => string;

/** How long the block of a restore stays once every server is back. */
const restoredShownFor = 7 * 24 * 60 * 60 * 1000;

/** How often the card asks again while servers are still coming back. */
const restoreWatchEvery = 10_000;

/** The sections a stale kit no longer matches, in words. */
export function kitReasons(reasons: KitSection[] | null | undefined, t: Translate): string {
  return (reasons ?? []).map((reason) => t(`settings.recovery.reason.${reason}`)).join(", ");
}

const expectCAPin = "POCKETCLOUD_EXPECT_CA_PIN";

/** The lines of an env file a restored panel starts with: KEY=value, leaving
 * out what is empty, in the order of `recover --env-out`'s file: by name, the
 * CA pin last. The panel's answer keeps no order of its keys. */
export function envLines(env: Record<string, string> | null | undefined): string {
  return Object.entries(env ?? {})
    .filter(([, value]) => value !== "")
    .sort(([a], [b]) =>
      a === expectCAPin || b === expectCAPin
        ? Number(a === expectCAPin) - Number(b === expectCAPin)
        : a.localeCompare(b, "en"),
    )
    .map(([key, value]) => `${key}=${value}`)
    .join("\n");
}

/** Whether the block of a restore shows: while servers are still coming
 * back, and for 7 days after the restore. */
export function restoredShown(restored: RecoveryState["restored"], now: number): boolean {
  if (!restored) {
    return false;
  }
  if (restored.back < restored.total) {
    return true;
  }
  const at = parseDate(restored.at)?.getTime();
  return at !== undefined && now - at < restoredShownFor;
}

/** Settings › Recovery kit (#32), for admins: the master key's id and the
 * CA's pin, whether a current kit was saved and checked, the panel's own
 * backup job, the addresses the servers dial, how a restored panel starts,
 * and a restore the panel came back from. The kit is downloaded and checked
 * in dialogs; their passphrases live only in them. */
export function RecoveryCard() {
  const { t } = useTranslation();
  const { hash } = useLocation();
  const now = useNow(30_000);
  const [state, setState] = useState<RecoveryState | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [dialog, setDialog] = useState<"download" | "check" | null>(null);
  const [asked, setAsked] = useState(0);
  const reload = useCallback(() => setAsked((count) => count + 1), []);

  useEffect(() => {
    let cancelled = false;
    recoveryApi
      .getRecovery()
      .then((loaded) => {
        if (!cancelled) {
          setState(loaded);
          setError(null);
        }
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          setError(err);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [asked]);

  // the router does not scroll to the anchor of a link from another page,
  // and the card grows once it has loaded
  const loaded = state !== null;
  useEffect(() => {
    if (hash === "#recovery-kit") {
      document.getElementById("recovery-kit")?.scrollIntoView?.({ block: "start" });
    }
  }, [hash, loaded]);

  // after a restore, the servers come back one by one
  const watching = state?.restored != null && state.restored.back < state.restored.total;
  useEffect(() => {
    if (!watching) {
      return;
    }
    const timer = setInterval(reload, restoreWatchEvery);
    return () => clearInterval(timer);
  }, [watching, reload]);

  let body: ReactNode;
  if (state) {
    body = (
      <RecoveryDetails
        state={state}
        now={now}
        onDownload={() => setDialog("download")}
        onCheck={() => setDialog("check")}
      />
    );
  } else if (!error) {
    body = <p className="muted">{t("common.loading")}</p>;
  }

  return (
    <>
      <Card
        id="recovery-kit"
        title={t("settings.recovery.title")}
        note={t("settings.recovery.note")}
      >
        <div className="recovery">
          {error !== null && <Banner tone="err">{errorMessage(error)}</Banner>}
          {body}
        </div>
      </Card>
      {dialog === "download" && (
        <DownloadKitDialog
          noJob={state?.panel_job == null}
          onClose={() => setDialog(null)}
          onDownloaded={() => {
            reload();
            // the reminder stays until a check opens the kit
            setDialog("check");
          }}
        />
      )}
      {dialog === "check" && <CheckKitDialog onClose={() => setDialog(null)} onChecked={reload} />}
    </>
  );
}

function RecoveryDetails({
  state,
  now,
  onDownload,
  onCheck,
}: {
  state: RecoveryState;
  now: number;
  onDownload: () => void;
  onCheck: () => void;
}) {
  const { t } = useTranslation();
  const { kit } = state;
  const addresses = state.addresses ?? [];
  const env = envLines(state.start_env);
  const saved = kit.status === "saved";

  let status: string;
  let advice = "";
  switch (kit.status) {
    case "saved":
      status = t("settings.recovery.statusSaved", {
        time: formatRelative(kit.verified_at, now, { past: true }),
        name: kit.verified_by ?? "",
      });
      break;
    case "unverified":
      status = t("settings.recovery.statusUnverified", {
        time: formatRelative(kit.created, now, { past: true }),
      });
      advice = t("settings.recovery.unverifiedText");
      break;
    case "stale":
      status = t("settings.recovery.statusStale", { reasons: kitReasons(kit.reasons, t) });
      advice = t("settings.recovery.staleText");
      break;
    default:
      status = t("settings.recovery.statusNever");
      advice = t("settings.recovery.neverText");
  }

  return (
    <>
      <p className="muted">{t("settings.recovery.lead")}</p>
      {restoredShown(state.restored, now) && state.restored && (
        <IssueBanner
          id="recovery-restored"
          tone="info"
          icon={<History size={18} />}
          title={t("settings.recovery.restoredTitle")}
          meta={
            <>
              <p>
                {t("settings.recovery.restored", {
                  time: formatDateTime(state.restored.at),
                  from: formatDateTime(state.restored.from),
                })}
              </p>
              <p>
                {t("settings.recovery.restoredBack", {
                  back: state.restored.back,
                  total: state.restored.total,
                })}
              </p>
            </>
          }
        />
      )}
      <dl className="facts">
        <dt>{t("settings.recovery.status")}</dt>
        <dd>
          <StatusPill tone={saved ? "ok" : "warn"}>{status}</StatusPill>
        </dd>
        <dt>{t("settings.recovery.keyId")}</dt>
        <dd className="mono small">{state.key_id}</dd>
        <dt>{t("settings.recovery.caPin")}</dt>
        <dd className="mono small">{state.ca_pin}</dd>
        <dt>{t("settings.recovery.panelJob")}</dt>
        <dd>
          <PanelJobLine job={state.panel_job} now={now} />
        </dd>
      </dl>
      {!saved && <Banner tone="warn">{advice}</Banner>}
      <div className="row-actions start">
        <Button variant={saved ? "default" : "primary"} onClick={onDownload}>
          {t("settings.recovery.download")}
        </Button>
        <Button onClick={onCheck}>{t("settings.recovery.check")}</Button>
      </div>
      {(addresses.length > 0 || state.unreported > 0) && (
        <section className="recovery-section" aria-labelledby="recovery-agents">
          <h3 id="recovery-agents" className="drawer-heading">
            {t("settings.recovery.agents")}
          </h3>
          {addresses.length > 0 && (
            <ul className="recovery-addresses">
              {addresses.map((item) => (
                <li key={item.address} className="mono">
                  {t("settings.recovery.agentsAt", { address: item.address, count: item.servers })}
                </li>
              ))}
            </ul>
          )}
          {addresses
            .filter((item) => item.ip)
            .map((item) => (
              <Banner key={item.address} tone="warn">
                {t("settings.recovery.ipWarning", { address: item.address })}
              </Banner>
            ))}
          {state.unreported > 0 && (
            <p className="muted small">
              {t("settings.recovery.unreported", { count: state.unreported })}
            </p>
          )}
        </section>
      )}
      {env && (
        // the code block is the region, by the heading's words
        <div className="recovery-section">
          <h3 className="drawer-heading">{t("settings.recovery.startWith")}</h3>
          <CodeBlock code={env} label={t("settings.recovery.startWith")} />
        </div>
      )}
      <p className="muted small">{t("settings.recovery.guide")}</p>
    </>
  );
}

/** The panel's own backup job: none, with a link to set it up; or its
 * targets and its last success, through which server. */
function PanelJobLine({ job, now }: { job: RecoveryState["panel_job"]; now: number }) {
  const { t } = useTranslation();
  if (!job) {
    return (
      <>
        <span>{t("settings.recovery.panelJobNone")}</span>{" "}
        <Link to="/backups?setup=panel">{t("settings.recovery.addJob")}</Link>
      </>
    );
  }
  const targets = job.targets ?? [];
  return (
    <>
      <div>
        <Link to={`/backups?job=${encodeURIComponent(job.id)}`}>{job.name}</Link>
        {targets.length > 0 && ` → ${targets.join(" + ")}`}
        {!job.enabled && <span className="faint">{` · ${t("backups.job.paused")}`}</span>}
      </div>
      <div className="muted small">
        {job.last_success_at
          ? t("settings.recovery.panelJobLast", {
              time: formatRelative(job.last_success_at, now, { past: true }),
              server: job.carrier_name,
            })
          : t("settings.recovery.panelJobNever")}
      </div>
    </>
  );
}

/** Seals a kit with a passphrase, after the admin's sign-in password; the
 * passphrases and the password live only here, and go with the dialog. */
export function DownloadKitDialog({
  noJob,
  onClose,
  onDownloaded,
}: {
  /** the panel has no backup job: the kit holds the master key alone */
  noJob: boolean;
  onClose: () => void;
  onDownloaded: (kit: ExportedKit) => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const mounted = useMounted();
  const [passphrase, setPassphrase] = useState("");
  const [again, setAgain] = useState("");
  const [generated, setGenerated] = useState("");
  const [password, setPassword] = useState("");
  const [apart, setApart] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const short = passphrase !== "" && passphraseLength(passphrase) < minPassphrase;
  const mismatch = again !== "" && again !== passphrase;
  const ready =
    passphraseLength(passphrase) >= minPassphrase &&
    again === passphrase &&
    password !== "" &&
    apart &&
    !busy;

  const generate = () => {
    const code = generatePassphrase();
    setGenerated(code);
    setPassphrase(code);
    setAgain(code);
  };

  const download = async () => {
    if (!ready) {
      return;
    }
    setBusy(true);
    setError("");
    try {
      const kit = await recoveryApi.exportKit(passphrase, password);
      // the panel counts it as downloaded now, so it is saved even when the
      // dialog was closed meanwhile
      recoveryApi.saveTextFile(kit.file_name, kit.content);
      toast.success(t("settings.recovery.downloaded", { id: kit.kit_id }));
      onDownloaded(kit);
    } catch (err) {
      if (mounted.current) {
        setError(errorMessage(err));
      } else {
        toast.error(err);
      }
    } finally {
      if (mounted.current) {
        setBusy(false);
      }
    }
  };

  return (
    <ConfirmDialog
      open
      title={t("settings.recovery.downloadTitle")}
      confirmLabel={busy ? t("settings.recovery.sealing") : t("settings.recovery.downloadAction")}
      confirmDisabled={!ready}
      onCancel={onClose}
      onConfirm={() => void download()}
    >
      <div className="dialog-form">
        <p className="muted">{t("settings.recovery.downloadText")}</p>
        {noJob && <Banner tone="info">{t("settings.recovery.noJob")}</Banner>}
        <fieldset className="dialog-fieldset" disabled={busy}>
          <Field
            label={t("settings.recovery.passphrase")}
            htmlFor="kit-passphrase"
            error={
              short ? t("settings.recovery.passphraseShort", { count: minPassphrase }) : undefined
            }
          >
            <input
              id="kit-passphrase"
              className="input mono"
              type="password"
              autoComplete="new-password"
              spellCheck={false}
              aria-invalid={short}
              aria-describedby="kit-passphrase-hint"
              value={passphrase}
              onChange={(event) => setPassphrase(event.target.value)}
            />
            <p id="kit-passphrase-hint" className="muted small">
              {t("settings.recovery.passphraseHint")}
            </p>
          </Field>
          <Field
            label={t("settings.recovery.passphraseAgain")}
            htmlFor="kit-passphrase-again"
            error={mismatch ? t("settings.recovery.passphraseMismatch") : undefined}
          >
            <input
              id="kit-passphrase-again"
              className="input mono"
              type="password"
              autoComplete="new-password"
              spellCheck={false}
              aria-invalid={mismatch}
              value={again}
              onChange={(event) => setAgain(event.target.value)}
            />
          </Field>
          <div>
            <Button size="small" onClick={generate}>
              {t("settings.recovery.generate")}
            </Button>
          </div>
          {generated !== "" && passphrase === generated && (
            <div className="recovery-generated">
              <CodeBlock code={generated} label={t("settings.recovery.passphrase")} />
              <p className="warn-text small">{t("settings.recovery.generated")}</p>
            </div>
          )}
          <Field label={t("settings.recovery.password")} htmlFor="kit-password">
            <input
              id="kit-password"
              className="input"
              type="password"
              autoComplete="current-password"
              aria-describedby="kit-password-note"
              value={password}
              onChange={(event) => setPassword(event.target.value)}
            />
            <p id="kit-password-note" className="muted small">
              {t("settings.recovery.passwordNote")}
            </p>
          </Field>
          <label className="check-line">
            <input
              type="checkbox"
              required
              checked={apart}
              onChange={(event) => setApart(event.target.checked)}
            />
            <span>{t("settings.recovery.apart")}</span>
          </label>
        </fieldset>
        {error && <Banner tone="err">{error}</Banner>}
      </div>
    </ConfirmDialog>
  );
}

/** Opens a saved kit as a recovery would: the panel checks the passphrase
 * and its key, and marks the kit saved when it is current. */
export function CheckKitDialog({
  onClose,
  onChecked,
}: {
  onClose: () => void;
  onChecked: (check: KitCheck) => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const mounted = useMounted();
  const [kit, setKit] = useState("");
  const [fileError, setFileError] = useState("");
  const [passphrase, setPassphrase] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<KitCheck | null>(null);

  const pick = async (file: File | undefined) => {
    setKit("");
    setFileError("");
    setError("");
    setResult(null);
    if (!file) {
      return;
    }
    // the panel takes no more: nothing bigger is a kit
    if (file.size > maxKitBytes) {
      setFileError(t("settings.recovery.kitTooBig"));
      return;
    }
    const text = await file.text();
    if (mounted.current) {
      setKit(text);
    }
  };

  const ready = kit !== "" && passphrase !== "" && !busy;
  const check = async () => {
    if (!ready) {
      return;
    }
    setBusy(true);
    setError("");
    setResult(null);
    try {
      const checked = await recoveryApi.checkKit(kit, passphrase);
      if (mounted.current) {
        setResult(checked);
      }
      onChecked(checked);
    } catch (err) {
      if (mounted.current) {
        setError(errorMessage(err));
      } else {
        toast.error(err);
      }
    } finally {
      if (mounted.current) {
        setBusy(false);
      }
    }
  };

  return (
    <ConfirmDialog
      open
      title={t("settings.recovery.checkTitle")}
      confirmLabel={busy ? t("settings.recovery.checking") : t("settings.recovery.checkAction")}
      confirmDisabled={!ready}
      onCancel={onClose}
      onConfirm={() => void check()}
    >
      <div className="dialog-form">
        <p className="muted">{t("settings.recovery.checkText")}</p>
        <fieldset className="dialog-fieldset" disabled={busy}>
          <Field label={t("settings.recovery.kitFile")} htmlFor="kit-file" error={fileError}>
            <input
              id="kit-file"
              className="input"
              type="file"
              accept=".pckit,text/plain"
              aria-invalid={fileError !== ""}
              onChange={(event) => void pick(event.target.files?.[0])}
            />
          </Field>
          <Field label={t("settings.recovery.passphrase")} htmlFor="kit-check-passphrase">
            <input
              id="kit-check-passphrase"
              className="input mono"
              type="password"
              autoComplete="off"
              spellCheck={false}
              value={passphrase}
              onChange={(event) => {
                setPassphrase(event.target.value);
                setResult(null);
              }}
            />
          </Field>
        </fieldset>
        {result &&
          (result.current ? (
            <Banner tone="ok">
              {t("settings.recovery.checkedCurrent", { id: result.kit_id })}
            </Banner>
          ) : (
            <Banner tone="warn">
              {t("settings.recovery.checkedStale", {
                id: result.kit_id,
                reasons: kitReasons(result.reasons, t),
              })}
            </Banner>
          ))}
        {error && <Banner tone="err">{error}</Banner>}
      </div>
    </ConfirmDialog>
  );
}
