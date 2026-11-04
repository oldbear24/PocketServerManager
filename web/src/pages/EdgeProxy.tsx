import { Lock, LockOpen, Plus, RefreshCw, Trash2, Upload } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

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
} from "../components/ui";
import { errorMessage } from "../lib/api";
import { hasRole, useAuth } from "../lib/auth";
import {
  edgeApi,
  edgeCondition,
  tlsTone,
  type Certificate,
  type Domain,
  type EdgeRoute,
  type EdgeState,
} from "../lib/edge";
import { formatDate, formatNumber } from "../lib/format";
import { pb } from "../lib/pb";
import { useLiveList } from "../lib/realtime";
import type { Container, Host } from "../lib/types";

const systemRoles = ["agent", "updater", "edge", "job"];

/**
 * The routes and state of a host's edge, read from the panel, which works out
 * the TLS of every route the way the edge's config does. It is read again
 * whenever a domain, a certificate or the edge's state changes.
 */
function useEdgeState(host: Host) {
  const domains = useLiveList<Domain>("domains", {
    filter: `host = "${host.id}"`,
    sort: "hostname",
  });
  const certificates = useLiveList<Certificate>("certificates", { sort: "-not_after" });

  const [state, setState] = useState<EdgeState | null>(null);
  const [error, setError] = useState("");

  const version = [
    host.edge_digest,
    host.edge_applied_digest,
    host.edge_error,
    host.edge_running,
    host.roles.join(),
    domains.items.map((item) => item.updated).join(),
    certificates.items.map((item) => `${item.id}:${item.updated}`).join(),
  ].join("|");

  useEffect(() => {
    let cancelled = false;
    edgeApi
      .state(host.id)
      .then((next) => {
        if (!cancelled) {
          setState(next);
          setError("");
        }
      })
      .catch((reason: unknown) => {
        if (!cancelled) {
          setError(errorMessage(reason));
        }
      });
    return () => {
      cancelled = true;
    };
  }, [host.id, version]);

  return { state, error, certificates: certificates.items };
}

function RouteLock({ tls }: { tls: EdgeRoute["tls"] }) {
  const { t } = useTranslation();
  const tone = tlsTone(tls);
  const Icon = tls === "none" || tls === "off" ? LockOpen : Lock;
  return (
    <Icon
      size={14}
      className={`route-lock ${tone}`}
      aria-label={t(`edge.tlsShort.${tls}`)}
      role="img"
    />
  );
}

function tlsText(t: ReturnType<typeof useTranslation>["t"], route: EdgeRoute) {
  const date = formatDate(route.certificate?.not_after);
  return t(`edge.tls.${route.tls}`, { date });
}

function RouteRow({
  route,
  onRemove,
}: {
  route: EdgeRoute;
  onRemove?: (route: EdgeRoute) => void;
}) {
  const { t } = useTranslation();
  return (
    <li className="route-row">
      <RouteLock tls={route.tls} />
      <span className="mono route-host">{route.hostname}</span>
      <span className="muted" aria-hidden="true">
        →
      </span>
      <span className="mono route-target">{route.target}</span>
      <StatusPill tone={route.target_kind === "container" ? "off" : "info"} dot={false}>
        {t(`edge.kind.${route.target_kind}`)}
      </StatusPill>
      <span className={`route-tls ${tlsTone(route.tls)}`}>{tlsText(t, route)}</span>
      {onRemove && (
        <IconButton
          bare
          aria-label={t("edge.removeDomain", { hostname: route.hostname })}
          title={t("edge.removeDomain", { hostname: route.hostname })}
          onClick={() => onRemove(route)}
        >
          <Trash2 size={15} aria-hidden="true" />
        </IconButton>
      )}
    </li>
  );
}

/** The routes of the edge on the server's overview, as in the design. */
export function EdgeRoutesCard({ host }: { host: Host }) {
  const { t } = useTranslation();
  const { user } = useAuth();
  const isEdge = host.roles.includes("edge");
  const { state } = useEdgeState(host);

  return (
    <Card
      title={t("server.edgeRoutes")}
      note={t("server.edgeRoutesNote")}
      side={
        isEdge &&
        hasRole(user, "operator") && (
          <Link to={`/servers/${host.id}/proxy`}>{t("edge.addDomain")}</Link>
        )
      }
    >
      {!isEdge ? (
        <p className="muted">{t("edge.noRole")}</p>
      ) : state && state.routes.length === 0 ? (
        <p className="muted">{t("edge.noRoutes")}</p>
      ) : (
        <ul className="list routes">
          {state?.routes.map((route) => (
            <RouteRow key={route.id} route={route} />
          ))}
        </ul>
      )}
    </Card>
  );
}

/** The Proxy & sites tab: the edge's state, its routes and their certificates. */
export function ProxyTab({ host, containers }: { host: Host; containers: Container[] }) {
  const { t } = useTranslation();
  const { user } = useAuth();
  const toast = useToast();
  const { state, error, certificates } = useEdgeState(host);
  const [adding, setAdding] = useState(false);
  const [removing, setRemoving] = useState<EdgeRoute | null>(null);
  const [uploading, setUploading] = useState(false);
  const [applying, setApplying] = useState(false);

  const canOperate = hasRole(user, "operator");
  const isAdmin = hasRole(user, "admin");

  if (!host.roles.includes("edge")) {
    return (
      <Card>
        <EmptyState title={t("edge.title")}>{t("edge.noRole")}</EmptyState>
      </Card>
    );
  }

  const removeDomain = async () => {
    if (!removing) {
      return;
    }
    try {
      await pb.collection("domains").delete(removing.id);
      toast.success(t("edge.domainRemoved", { hostname: removing.hostname }));
    } catch (reason) {
      toast.error(reason);
    }
    setRemoving(null);
  };

  const applyAgain = async () => {
    setApplying(true);
    try {
      await edgeApi.apply(host.id);
      toast.success(t("edge.applied"));
    } catch (reason) {
      toast.error(reason);
    }
    setApplying(false);
  };

  return (
    <div className="stack-gap">
      {error && <Banner tone="err">{error}</Banner>}
      {state && (
        <EdgeStatusCard
          state={state}
          canOperate={canOperate}
          applying={applying}
          onApply={() => void applyAgain()}
        />
      )}

      <Card
        title={t("edge.routes")}
        note={t("server.edgeRoutesNote")}
        side={
          canOperate && (
            <Button onClick={() => setAdding(true)}>
              <Plus size={16} aria-hidden="true" />
              {t("edge.addDomain")}
            </Button>
          )
        }
      >
        {state && state.routes.length === 0 ? (
          <p className="muted">{t("edge.noRoutes")}</p>
        ) : (
          <ul className="list routes">
            {state?.routes.map((route) => (
              <RouteRow
                key={route.id}
                route={route}
                onRemove={canOperate ? setRemoving : undefined}
              />
            ))}
          </ul>
        )}
      </Card>

      <CertificatesCard
        host={host}
        routes={state?.routes ?? []}
        certificates={certificates}
        isAdmin={isAdmin}
        onUpload={() => setUploading(true)}
      />

      {adding && (
        <AddDomainDialog host={host} containers={containers} onClose={() => setAdding(false)} />
      )}
      {uploading && <UploadCertificateDialog onClose={() => setUploading(false)} />}
      <ConfirmDialog
        open={removing !== null}
        title={t("edge.removeConfirm", { hostname: removing?.hostname ?? "" })}
        confirmLabel={t("edge.remove")}
        danger
        onCancel={() => setRemoving(null)}
        onConfirm={() => void removeDomain()}
      >
        <p className="muted">{t("edge.removeNote")}</p>
      </ConfirmDialog>
    </div>
  );
}

function EdgeStatusCard({
  state,
  canOperate,
  applying,
  onApply,
}: {
  state: EdgeState;
  canOperate: boolean;
  applying: boolean;
  onApply: () => void;
}) {
  const { t } = useTranslation();
  const { edge } = state;
  const condition = edgeCondition(edge);
  const tone = { synced: "ok", applying: "info", failed: "err", waiting: "warn" } as const;
  const failure = edge.error || (condition === "failed" ? (edge.last_apply?.error ?? "") : "");

  return (
    <Card
      title={t("edge.title")}
      note={
        edge.caddy_version
          ? t("edge.caddyVersion", { version: edge.caddy_version })
          : t("edge.caddyPinned", { version: edge.image_version })
      }
      side={
        canOperate && (
          <Button disabled={applying || edge.applying} onClick={onApply}>
            <RefreshCw size={16} aria-hidden="true" />
            {t("edge.applyAgain")}
          </Button>
        )
      }
    >
      {!edge.agent_has_edge && <Banner tone="warn">{t("edge.agentTooOld")}</Banner>}
      <dl className="stat-row">
        <div>
          <dt>{t("edge.state")}</dt>
          <dd>
            <StatusPill tone={tone[condition]}>
              {t(`edge.condition.${condition}`, { revision: edge.revision })}
            </StatusPill>
          </dd>
        </div>
        <div>
          <dt>{t("server.httpRequests")}</dt>
          <dd>{t("server.perSecond", { value: formatNumber(edge.requests_per_s, 1) })}</dd>
        </div>
        <div>
          <dt>{t("edge.routes")}</dt>
          <dd>{state.routes.length}</dd>
        </div>
      </dl>
      {failure && (
        <div style={{ marginTop: 12 }}>
          <Banner tone="err">{failure}</Banner>
        </div>
      )}
    </Card>
  );
}

function CertificatesCard({
  host,
  routes,
  certificates,
  isAdmin,
  onUpload,
}: {
  host: Host;
  routes: EdgeRoute[];
  certificates: Certificate[];
  isAdmin: boolean;
  onUpload: () => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const [removing, setRemoving] = useState<Certificate | null>(null);

  // the certificates this edge uses, or has installed
  const shown = useMemo(() => {
    const used = new Set(routes.map((route) => route.certificate?.id).filter(Boolean));
    return certificates.filter((cert) => used.has(cert.id) || Boolean(cert.installed?.[host.id]));
  }, [certificates, routes, host.id]);

  const remove = async () => {
    if (!removing) {
      return;
    }
    try {
      await edgeApi.removeCertificate(removing.id);
      toast.success(t("edge.certificateRemoved"));
    } catch (reason) {
      toast.error(reason);
    }
    setRemoving(null);
  };

  return (
    <Card
      title={t("edge.certificates")}
      note={t("edge.certificatesNote")}
      side={
        isAdmin && (
          <Button onClick={onUpload}>
            <Upload size={16} aria-hidden="true" />
            {t("edge.upload")}
          </Button>
        )
      }
    >
      {shown.length === 0 ? (
        <p className="muted">{t("edge.noCertificates")}</p>
      ) : (
        <ul className="list">
          {shown.map((cert) => {
            const expired = cert.status === "expired" || new Date(cert.not_after) < new Date();
            return (
              <li key={cert.id} className="list-row">
                <div>
                  <div className="mono">{cert.names.join(", ")}</div>
                  <div className="muted" style={{ fontSize: 12.5 }}>
                    {t(`edge.issuer.${cert.issuer}`)} ·{" "}
                    {expired
                      ? t("edge.expiredOn", { date: formatDate(cert.not_after) })
                      : t("edge.validUntil", { date: formatDate(cert.not_after) })}
                    {cert.installed?.[host.id] ? ` · ${t("edge.installedHere")}` : ""}
                    {cert.note ? ` · ${cert.note}` : ""}
                  </div>
                  {cert.last_error && <div className="field-error">{cert.last_error}</div>}
                </div>
                {isAdmin && (
                  <IconButton
                    bare
                    aria-label={t("edge.removeCertificate")}
                    title={t("edge.removeCertificate")}
                    onClick={() => setRemoving(cert)}
                  >
                    <Trash2 size={15} aria-hidden="true" />
                  </IconButton>
                )}
              </li>
            );
          })}
        </ul>
      )}
      <ConfirmDialog
        open={removing !== null}
        title={t("edge.removeCertificateConfirm", { names: removing?.names.join(", ") ?? "" })}
        confirmLabel={t("edge.removeCertificate")}
        danger
        onCancel={() => setRemoving(null)}
        onConfirm={() => void remove()}
      >
        <p className="muted">{t("edge.removeCertificateNote")}</p>
      </ConfirmDialog>
    </Card>
  );
}

function AddDomainDialog({
  host,
  containers,
  onClose,
}: {
  host: Host;
  containers: Container[];
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();

  const choices = containers.filter((container) => !systemRoles.includes(container.system_role));

  const [hostname, setHostname] = useState("");
  const [kind, setKind] = useState<"container" | "redirect">("container");
  const [container, setContainer] = useState(choices[0]?.name ?? "");
  const [port, setPort] = useState("80");
  const [redirectTo, setRedirectTo] = useState("");
  const [permanent, setPermanent] = useState(false);
  const [https, setHttps] = useState(true);
  const [httpsRedirect, setHttpsRedirect] = useState(true);
  const [hsts, setHsts] = useState(false);
  const [compress, setCompress] = useState(false);
  const [errors, setErrors] = useState<Record<string, string>>({});

  const save = async () => {
    try {
      await pb.collection("domains").create({
        hostname: hostname.trim(),
        host: host.id,
        target_kind: kind,
        container: kind === "container" ? container : "",
        port: kind === "container" ? Number(port) : 0,
        redirect_to: kind === "redirect" ? redirectTo.trim() : "",
        redirect_permanent: permanent,
        tls: https ? "auto" : "off",
        https_redirect: https && httpsRedirect,
        hsts: https && hsts,
        compress,
      });
      toast.success(t("edge.domainAdded", { hostname: hostname.trim().toLowerCase() }));
      onClose();
    } catch (reason) {
      const data = (reason as { response?: { data?: Record<string, { message?: string }> } })
        .response?.data;
      const fields: Record<string, string> = {};
      for (const [key, value] of Object.entries(data ?? {})) {
        if (value?.message) {
          fields[key] = value.message;
        }
      }
      setErrors(Object.keys(fields).length > 0 ? fields : { form: errorMessage(reason) });
    }
  };

  return (
    <ConfirmDialog
      open
      title={t("edge.addDomainTitle", { server: host.name })}
      confirmLabel={t("edge.add")}
      onCancel={onClose}
      onConfirm={() => void save()}
    >
      <div className="dialog-form">
        <Field label={t("edge.hostname")} htmlFor="domain-hostname" error={errors.hostname}>
          <input
            id="domain-hostname"
            className="input mono"
            value={hostname}
            placeholder="app.example.com"
            autoComplete="off"
            onChange={(event) => setHostname(event.target.value)}
          />
        </Field>
        <SegmentedControl
          label={t("edge.target")}
          value={kind}
          onChange={setKind}
          options={[
            { value: "container", label: t("edge.kind.container") },
            { value: "redirect", label: t("edge.kind.redirect") },
          ]}
        />
        {kind === "container" ? (
          <div className="form-grid">
            <Field label={t("edge.container")} htmlFor="domain-container" error={errors.container}>
              {choices.length === 0 ? (
                <p className="muted">{t("edge.noContainers")}</p>
              ) : (
                <select
                  id="domain-container"
                  className="select"
                  style={{ width: "100%" }}
                  value={container}
                  onChange={(event) => setContainer(event.target.value)}
                >
                  {choices.map((item) => (
                    <option key={item.id} value={item.name}>
                      {item.name}
                    </option>
                  ))}
                </select>
              )}
            </Field>
            <Field label={t("edge.port")} htmlFor="domain-port" error={errors.port}>
              <input
                id="domain-port"
                className="input mono"
                inputMode="numeric"
                value={port}
                onChange={(event) => setPort(event.target.value.replace(/\D/g, ""))}
              />
            </Field>
          </div>
        ) : (
          <>
            <Field
              label={t("edge.redirectTo")}
              htmlFor="domain-redirect"
              error={errors.redirect_to}
            >
              <input
                id="domain-redirect"
                className="input mono"
                value={redirectTo}
                placeholder="https://www.example.com"
                autoComplete="off"
                onChange={(event) => setRedirectTo(event.target.value)}
              />
            </Field>
            <label className="check-line">
              <input
                type="checkbox"
                checked={permanent}
                onChange={(event) => setPermanent(event.target.checked)}
              />
              {t("edge.permanent")}
            </label>
          </>
        )}
        <label className="check-line">
          <input
            type="checkbox"
            checked={https}
            onChange={(event) => setHttps(event.target.checked)}
          />
          {t("edge.https")}
        </label>
        {https && (
          <>
            <label className="check-line">
              <input
                type="checkbox"
                checked={httpsRedirect}
                onChange={(event) => setHttpsRedirect(event.target.checked)}
              />
              {t("edge.httpsRedirect")}
            </label>
            <label className="check-line">
              <input
                type="checkbox"
                checked={hsts}
                onChange={(event) => setHsts(event.target.checked)}
              />
              {t("edge.hsts")}
            </label>
          </>
        )}
        {kind === "container" && (
          <label className="check-line">
            <input
              type="checkbox"
              checked={compress}
              onChange={(event) => setCompress(event.target.checked)}
            />
            {t("edge.compress")}
          </label>
        )}
        {(errors.form || errors.host || errors.target_kind) && (
          <p className="field-error">{errors.form || errors.host || errors.target_kind}</p>
        )}
      </div>
    </ConfirmDialog>
  );
}

function UploadCertificateDialog({ onClose }: { onClose: () => void }) {
  const { t } = useTranslation();
  const toast = useToast();
  const [chain, setChain] = useState("");
  const [key, setKey] = useState("");
  const [note, setNote] = useState("");
  const [error, setError] = useState("");

  const upload = async () => {
    try {
      await edgeApi.upload({ chain_pem: chain, key_pem: key, note });
      toast.success(t("edge.uploaded"));
      onClose();
    } catch (reason) {
      setError(errorMessage(reason));
    }
  };

  return (
    <ConfirmDialog
      open
      title={t("edge.uploadTitle")}
      confirmLabel={t("edge.upload")}
      onCancel={onClose}
      onConfirm={() => void upload()}
    >
      <div className="dialog-form">
        <p className="muted">{t("edge.uploadNote")}</p>
        <Field label={t("edge.chain")} htmlFor="cert-chain">
          <textarea
            id="cert-chain"
            className="input mono"
            rows={5}
            spellCheck={false}
            placeholder="-----BEGIN CERTIFICATE-----"
            value={chain}
            onChange={(event) => setChain(event.target.value)}
          />
        </Field>
        <Field label={t("edge.key")} htmlFor="cert-key">
          <textarea
            id="cert-key"
            className="input mono"
            rows={4}
            spellCheck={false}
            autoComplete="off"
            placeholder="-----BEGIN PRIVATE KEY-----"
            value={key}
            onChange={(event) => setKey(event.target.value)}
          />
        </Field>
        <Field label={t("edge.note")} htmlFor="cert-note">
          <input
            id="cert-note"
            className="input"
            value={note}
            onChange={(event) => setNote(event.target.value)}
          />
        </Field>
        {error && <p className="field-error">{error}</p>}
      </div>
    </ConfirmDialog>
  );
}
