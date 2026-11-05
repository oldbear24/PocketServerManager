import { Lock, LockOpen, RefreshCw, Trash2 } from "lucide-react";
import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { errorMessage } from "../lib/api";
import {
  caName,
  certificatesApi,
  certificateState,
  directoryId,
  lastChallenge,
  needsAction,
  parseNames,
  providerFor,
  stateTone,
  validity,
  type AcmeAccount,
  type CertState,
  type Challenge,
} from "../lib/certificates";
import type { DnsProvider, DnsZone } from "../lib/dns";
import { edgeApi, type Certificate } from "../lib/edge";
import { formatDate, formatDateTime, formatRelative, parseDate } from "../lib/format";
import type { Host } from "../lib/types";
import { useToast } from "./toast";
import {
  Banner,
  Button,
  Card,
  CodeBlock,
  ConfirmDialog,
  EmptyState,
  Field,
  SegmentedControl,
  StatusPill,
  Table,
  Timeline,
  type TimelineItem,
} from "./ui";

type Filter = "all" | "action" | "internal";

/** The name of a CA directory for a person. */
export function useDirectoryLabel() {
  const { t } = useTranslation();
  return (url: string) => {
    const id = directoryId(url);
    if (id !== "custom") {
      return t(`cert.ca.${id}`);
    }
    try {
      return new URL(url).host;
    } catch {
      return url;
    }
  };
}

function useIssuerLabel(accounts: AcmeAccount[]) {
  const { t } = useTranslation();
  const directoryLabel = useDirectoryLabel();
  const primary = accounts.find((account) => account.role === "primary");
  return (cert: Certificate) => {
    if (cert.issuer !== "acme") {
      return t(`cert.issuer.${cert.issuer}`);
    }
    return (
      caName(cert.issuer_name) || (primary ? directoryLabel(primary.directory_url) : t("cert.acme"))
    );
  };
}

/** Days left and until when, with a meter of the lifetime left. */
function ValidityCell({ cert, state, now }: { cert: Certificate; state: CertState; now: number }) {
  const { t } = useTranslation();
  if (!cert.fingerprint) {
    return (
      <span className="muted small">
        {state === "issuing" ? t("cert.issuingNow") : t(`cert.state.${state}`)}
      </span>
    );
  }
  const { days, left } = validity(cert, now);
  const text =
    state === "expired"
      ? t("cert.expiredOn", { date: formatDate(cert.not_after) })
      : t("cert.daysLeft", { count: days, date: formatDate(cert.not_after) });
  const level = state === "expired" ? " err" : state === "valid" ? "" : " warn";
  return (
    <div className="cert-validity">
      <span className="small">{text}</span>
      <div
        className={`progress${level}`}
        role="meter"
        aria-label={t("cert.lifetimeLeft")}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(left * 100)}
        aria-valuetext={text}
      >
        <span style={{ width: `${left * 100}%` }} />
      </div>
    </div>
  );
}

export function CertificatesCard({
  certificates,
  hosts,
  zones,
  providers,
  accounts,
  canRetry,
  isAdmin,
  loading,
  onRequest,
}: {
  certificates: Certificate[];
  hosts: Host[];
  zones: DnsZone[];
  providers: DnsProvider[];
  accounts: AcmeAccount[];
  canRetry: boolean;
  isAdmin: boolean;
  loading: boolean;
  onRequest: () => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const [filter, setFilter] = useState<Filter>("all");
  const [open, setOpen] = useState<string | null>(null);
  const [retrying, setRetrying] = useState("");
  const now = useNow();
  const issuerLabel = useIssuerLabel(accounts);

  const hostNames = useMemo(() => new Map(hosts.map((host) => [host.id, host.name])), [hosts]);
  const counts = useMemo(() => {
    let valid = 0;
    let issuing = 0;
    let action = 0;
    for (const cert of certificates) {
      const state = certificateState(cert, now);
      valid += state === "valid" || state === "expiring" ? 1 : 0;
      issuing += state === "issuing" ? 1 : 0;
      action += needsAction(cert, now) ? 1 : 0;
    }
    return { valid, issuing, action };
  }, [certificates, now]);

  const shown = certificates.filter((cert) => {
    if (filter === "action") {
      return needsAction(cert, now);
    }
    if (filter === "internal") {
      return cert.issuer === "internal";
    }
    return true;
  });

  const retry = async (cert: Certificate) => {
    setRetrying(cert.id);
    try {
      await certificatesApi.retry(cert.id);
      toast.success(t("cert.retrying", { name: cert.names[0] }));
    } catch (err) {
      toast.error(err);
    } finally {
      setRetrying("");
    }
  };

  const validation = (cert: Certificate) => {
    if (cert.issuer !== "acme") {
      return "–";
    }
    const challenge = lastChallenge(cert);
    if (challenge === "dns-01") {
      const provider = providerFor(cert.names[0] ?? "", zones, providers);
      return provider ? `DNS-01 · ${provider.name}` : "DNS-01";
    }
    return challenge === "http-01" ? "HTTP-01" : t("cert.challengeAuto");
  };

  const selected = certificates.find((cert) => cert.id === open);

  return (
    <Card
      title={t("cert.title")}
      note={[
        t("cert.validCount", { count: counts.valid }),
        counts.issuing > 0 ? t("cert.issuingCount", { count: counts.issuing }) : "",
        t("cert.renewalNote"),
      ]
        .filter(Boolean)
        .join(" · ")}
      side={
        <SegmentedControl
          label={t("cert.filter")}
          value={filter}
          onChange={setFilter}
          options={[
            { value: "all", label: t("cert.filterAll") },
            {
              value: "action",
              label:
                counts.action > 0
                  ? `${t("cert.filterAction")} (${counts.action})`
                  : t("cert.filterAction"),
            },
            { value: "internal", label: t("cert.filterInternal") },
          ]}
        />
      }
      flush
    >
      {loading && certificates.length === 0 ? (
        <p className="muted card-pad">{t("common.loading")}</p>
      ) : shown.length === 0 ? (
        <EmptyState
          title={filter === "all" ? t("cert.none") : t("cert.noneHere")}
          action={
            filter === "all" && isAdmin && <Button onClick={onRequest}>{t("cert.request")}</Button>
          }
        >
          {filter === "all" ? t("cert.noneText") : undefined}
        </EmptyState>
      ) : (
        <Table label={t("cert.title")}>
          <thead>
            <tr>
              <th>{t("cert.names")}</th>
              <th>{t("cert.issuerColumn")}</th>
              <th>{t("cert.validation")}</th>
              <th>{t("cert.validity")}</th>
              <th>{t("cert.deployedOn")}</th>
              <th>
                <span className="visually-hidden">{t("cert.actions")}</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {shown.map((cert) => {
              const state = certificateState(cert, now);
              const deployed = Object.keys(cert.installed ?? {})
                .map((id) => hostNames.get(id) ?? id)
                .sort();
              const LockIcon = cert.fingerprint && state !== "expired" ? Lock : LockOpen;
              const showRetry =
                canRetry && cert.issuer === "acme" && state !== "issuing" && needsAction(cert, now);
              return (
                <tr key={cert.id}>
                  <td>
                    <div className="cert-names">
                      <LockIcon
                        size={15}
                        aria-hidden="true"
                        className={`cert-lock ${stateTone(state)}`}
                      />
                      <div>
                        <div className="mono">{cert.names[0]}</div>
                        {cert.names.length > 1 && (
                          <div className="muted small">
                            {cert.names.length > 3
                              ? t("cert.andMore", {
                                  names: cert.names.slice(1, 3).join(", "),
                                  count: cert.names.length - 3,
                                })
                              : cert.names.slice(1).join(", ")}
                          </div>
                        )}
                        {state !== "valid" && (
                          <StatusPill tone={stateTone(state)}>
                            {t(`cert.state.${state}`)}
                          </StatusPill>
                        )}
                        {cert.last_error && state !== "issuing" && (
                          <div className="field-error small cert-error">{cert.last_error}</div>
                        )}
                      </div>
                    </div>
                  </td>
                  <td className="small">{issuerLabel(cert)}</td>
                  <td className="small">{validation(cert)}</td>
                  <td>
                    <ValidityCell cert={cert} state={state} now={now} />
                  </td>
                  <td className="mono small">{deployed.length > 0 ? deployed.join(", ") : "–"}</td>
                  <td>
                    <div className="row-actions">
                      {showRetry && (
                        <Button
                          size="small"
                          disabled={retrying === cert.id}
                          onClick={() => void retry(cert)}
                          aria-label={t("cert.retryName", { name: cert.names[0] })}
                        >
                          <RefreshCw size={14} aria-hidden="true" /> {t("cert.retry")}
                        </Button>
                      )}
                      <Button
                        size="small"
                        onClick={() => setOpen(cert.id)}
                        aria-label={t("cert.detailName", { name: cert.names[0] })}
                      >
                        {t("cert.detail")}
                      </Button>
                    </div>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </Table>
      )}
      {selected && (
        <CertificateDetail
          cert={selected}
          hostNames={hostNames}
          issuer={issuerLabel(selected)}
          validation={validation(selected)}
          canRetry={canRetry}
          isAdmin={isAdmin}
          now={now}
          onClose={() => setOpen(null)}
        />
      )}
    </Card>
  );
}

/** The time, again every 30 seconds, so states and days left move on. */
function useNow() {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(timer);
  }, []);
  return now;
}

/** A side panel on the native dialog element, which traps focus. */
function Drawer({
  title,
  onClose,
  children,
  actions,
}: {
  title: ReactNode;
  onClose: () => void;
  children: ReactNode;
  actions: ReactNode;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const element = ref.current;
    if (element && !element.open) {
      element.showModal?.();
    }
    return () => element?.close?.();
  }, []);
  return (
    <dialog ref={ref} className="dialog drawer" onCancel={onClose}>
      <h2>{title}</h2>
      <div className="drawer-body">{children}</div>
      <div className="dialog-actions">{actions}</div>
    </dialog>
  );
}

function CertificateDetail({
  cert,
  hostNames,
  issuer,
  validation,
  canRetry,
  isAdmin,
  now,
  onClose,
}: {
  cert: Certificate;
  hostNames: Map<string, string>;
  issuer: string;
  validation: string;
  canRetry: boolean;
  isAdmin: boolean;
  now: number;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  const [removing, setRemoving] = useState(false);
  const state = certificateState(cert, now);
  const next = parseDate(cert.next_attempt);

  const retry = async () => {
    setBusy(true);
    try {
      await certificatesApi.retry(cert.id);
      toast.success(t("cert.retrying", { name: cert.names[0] }));
    } catch (err) {
      toast.error(err);
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    try {
      await edgeApi.removeCertificate(cert.id);
      toast.success(t("edge.certificateRemoved"));
      onClose();
    } catch (err) {
      toast.error(err);
    }
    setRemoving(false);
  };

  const history: TimelineItem[] = [...(cert.history ?? [])].reverse().map((attempt, index) => ({
    id: `${attempt.at}-${index}`,
    state: attempt.ok ? "done" : "failed",
    title: attempt.ok
      ? attempt.renewal
        ? t("cert.renewed")
        : t("cert.issued")
      : attempt.renewal
        ? t("cert.renewalFailed")
        : t("cert.issueFailed"),
    note: attempt.ok
      ? t("cert.validUntil", { date: formatDate(attempt.not_after) })
      : attempt.error,
    side: (
      <span className="muted small">
        {formatDateTime(attempt.at)}
        {attempt.challenge ? ` · ${attempt.challenge.toUpperCase()}` : ""}
        {attempt.account === "fallback" ? ` · ${t("cert.fallback")}` : ""}
      </span>
    ),
  }));

  const deployed = Object.keys(cert.installed ?? {}).map((id) => hostNames.get(id) ?? id);

  return (
    <Drawer
      title={cert.names[0]}
      onClose={onClose}
      actions={
        <>
          {isAdmin && (
            <Button variant="danger" onClick={() => setRemoving(true)}>
              <Trash2 size={14} aria-hidden="true" /> {t("common.remove")}
            </Button>
          )}
          {canRetry && cert.issuer === "acme" && (
            <Button disabled={busy || state === "issuing"} onClick={() => void retry()}>
              <RefreshCw size={14} aria-hidden="true" /> {t("cert.retry")}
            </Button>
          )}
          <Button variant="primary" onClick={onClose}>
            {t("common.close")}
          </Button>
        </>
      }
    >
      <div>
        <StatusPill tone={stateTone(state)}>{t(`cert.state.${state}`)}</StatusPill>
      </div>
      {cert.last_error && state !== "issuing" && (
        <Banner tone={cert.attempts > 0 ? "err" : "warn"}>
          <span className="pre-line">{cert.last_error}</span>
        </Banner>
      )}
      {next && next.getTime() > now && state !== "issuing" && (
        <p className="muted small">
          {t("cert.nextAttempt", { when: formatRelative(cert.next_attempt) })}
        </p>
      )}

      <dl className="facts">
        <dt>{t("cert.names")}</dt>
        <dd className="mono">{cert.names.join(", ")}</dd>
        <dt>{t("cert.issuerColumn")}</dt>
        <dd>{issuer}</dd>
        {cert.issuer === "acme" && (
          <>
            <dt>{t("cert.validation")}</dt>
            <dd>{validation}</dd>
          </>
        )}
        {cert.fingerprint && (
          <>
            <dt>{t("cert.validity")}</dt>
            <dd>
              {formatDate(cert.not_before)} – {formatDate(cert.not_after)}
            </dd>
            {cert.issuer === "acme" && cert.renew_after && (
              <>
                <dt>{t("cert.renewsAfter")}</dt>
                <dd>{formatDate(cert.renew_after)}</dd>
              </>
            )}
            <dt>{t("cert.issuerName")}</dt>
            <dd className="small">{cert.issuer_name}</dd>
            <dt>{t("cert.fingerprint")}</dt>
            <dd className="mono small cert-fingerprint">{cert.fingerprint}</dd>
          </>
        )}
        <dt>{t("cert.deployedOn")}</dt>
        <dd className="mono">{deployed.length > 0 ? deployed.join(", ") : "–"}</dd>
        {cert.issuer === "acme" && (
          <>
            <dt>{t("cert.ordered")}</dt>
            <dd>{cert.auto ? t("cert.orderedAuto") : t("cert.orderedByHand")}</dd>
          </>
        )}
        {cert.note && (
          <>
            <dt>{t("cert.note")}</dt>
            <dd>{cert.note}</dd>
          </>
        )}
      </dl>

      {history.length > 0 && (
        <section>
          <h3 className="drawer-heading">{t("cert.history")}</h3>
          <Timeline items={history} label={t("cert.history")} />
        </section>
      )}

      {cert.chain_pem && (
        <details>
          <summary>{t("cert.chain")}</summary>
          <CodeBlock code={cert.chain_pem} label={t("cert.chain")} />
        </details>
      )}

      <ConfirmDialog
        open={removing}
        title={t("edge.removeCertificateConfirm", { names: cert.names.join(", ") })}
        confirmLabel={t("edge.removeCertificate")}
        danger
        onCancel={() => setRemoving(false)}
        onConfirm={() => void remove()}
      >
        <p className="muted">
          {cert.auto ? t("cert.removeAutoNote") : t("edge.removeCertificateNote")}
        </p>
      </ConfirmDialog>
    </Drawer>
  );
}

/** Asks for a certificate for names of the admin's choosing: a wildcard, say. */
export function RequestCertificateDialog({ onClose }: { onClose: () => void }) {
  const { t } = useTranslation();
  const toast = useToast();
  const [names, setNames] = useState("");
  const [challenge, setChallenge] = useState<Challenge>("auto");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const parsed = parseNames(names);

  const request = async () => {
    if (busy || parsed.length === 0) {
      return;
    }
    setBusy(true);
    setError("");
    try {
      await certificatesApi.request(parsed, challenge);
      toast.success(t("cert.requested", { name: parsed[0] }));
      onClose();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <ConfirmDialog
      open
      title={t("cert.requestTitle")}
      confirmLabel={busy ? t("cert.requesting") : t("cert.request")}
      confirmDisabled={busy || parsed.length === 0}
      onCancel={onClose}
      onConfirm={() => void request()}
    >
      <div className="dialog-form">
        <p className="muted small">{t("cert.requestText")}</p>
        <Field label={t("cert.names")} htmlFor="cert-names">
          <textarea
            id="cert-names"
            className="input mono"
            rows={3}
            placeholder={"*.example.com\nexample.com"}
            value={names}
            onChange={(event) => setNames(event.target.value)}
          />
        </Field>
        <Field label={t("cert.validation")} htmlFor="cert-challenge">
          <select
            id="cert-challenge"
            className="input"
            value={challenge}
            onChange={(event) => setChallenge(event.target.value as Challenge)}
          >
            <option value="auto">{t("cert.challengeAuto")}</option>
            <option value="http-01">HTTP-01</option>
            <option value="dns-01">DNS-01</option>
          </select>
        </Field>
        <p className="muted small">{t(`cert.challengeNote.${challenge}`)}</p>
        {error && <Banner tone="err">{error}</Banner>}
      </div>
    </ConfirmDialog>
  );
}

/** The ACME accounts as Domains & TLS shows them; they are set in Settings. */
export function AcmeSummaryCard({
  accounts,
  isAdmin,
}: {
  accounts: AcmeAccount[];
  isAdmin: boolean;
}) {
  const { t } = useTranslation();
  const directoryLabel = useDirectoryLabel();
  const primary = accounts.find((account) => account.role === "primary");
  const fallback = accounts.find((account) => account.role === "fallback");

  return (
    <Card
      title={t("cert.account")}
      side={isAdmin && <Link to="/settings#acme">{t("cert.changeInSettings")}</Link>}
    >
      {!primary && !fallback ? (
        <Banner tone="warn">{isAdmin ? t("cert.noAccountAdmin") : t("cert.noAccount")}</Banner>
      ) : (
        <dl className="facts">
          <dt>{t("cert.contact")}</dt>
          <dd>{primary?.email ?? fallback?.email}</dd>
          <dt>{t("cert.primaryCA")}</dt>
          <dd>{primary ? directoryLabel(primary.directory_url) : "–"}</dd>
          <dt>{t("cert.fallbackCA")}</dt>
          <dd>
            {fallback
              ? directoryLabel(fallback.directory_url) +
                (fallback.eab_kid ? ` · ${t("cert.eabSet")}` : "")
              : t("cert.noFallback")}
          </dd>
          <dt>{t("cert.renewal")}</dt>
          <dd>{t("cert.renewalPolicy", { count: 5 })}</dd>
          <dt>{t("cert.alerts")}</dt>
          <dd>{t("cert.alertsPolicy")}</dd>
        </dl>
      )}
    </Card>
  );
}
