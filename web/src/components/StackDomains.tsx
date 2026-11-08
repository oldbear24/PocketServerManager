import { KeyRound } from "lucide-react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { edgeApi, tlsTone, type Certificate, type Domain, type EdgeRoute } from "../lib/edge";
import { useLiveList } from "../lib/realtime";
import type { Stack } from "../lib/types";
import { RouteLock, tlsText } from "../pages/EdgeProxy";
import { useToast } from "./toast";
import { Banner, Button, Card, ConfirmDialog } from "./ui";

/** The routes of a stack's domains, and the certificates that are for them. */
export type DomainRoutes = {
  routes: Map<string, EdgeRoute>;
  certificates: Certificate[];
};

/**
 * The routes of a stack's domains on its server's edge, with their TLS state
 * as the panel works it out. They are read again when a domain, a
 * certificate or the edge changes.
 */
export function useDomainRoutes(
  hostId: string | undefined,
  edgeVersion: string,
  domains: Domain[],
): DomainRoutes {
  const certificates = useLiveList<Certificate>("certificates", {
    sort: "-not_after",
    enabled: domains.length > 0,
  });
  const [routes, setRoutes] = useState<Map<string, EdgeRoute>>(() => new Map());
  const version = [
    edgeVersion,
    domains.map((domain) => `${domain.id}:${domain.updated}`).join(),
    certificates.items.map((cert) => `${cert.id}:${cert.updated}`).join(),
  ].join("|");

  useEffect(() => {
    if (!hostId || domains.length === 0) {
      return;
    }
    let cancelled = false;
    edgeApi
      .state(hostId)
      .then((state) => {
        if (!cancelled) {
          setRoutes(new Map(state.routes.map((route) => [route.id, route])));
        }
      })
      .catch(() => {
        // the domains still show, without their TLS state
      });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [hostId, version]);

  return { routes, certificates: certificates.items };
}

/** Whether a certificate's names cover a hostname, as the edge matches them. */
function covers(cert: Certificate, hostname: string): boolean {
  const parent = hostname.slice(hostname.indexOf(".") + 1);
  return cert.names.some((name) => name === hostname || name === `*.${parent}`);
}

/** The ACME certificate the panel is getting for a name that has none yet. */
function orderFor(certificates: Certificate[], hostname: string): Certificate | undefined {
  return certificates.find(
    // the panel's own never reaches an edge (#63)
    (cert) => !cert.panel && cert.issuer === "acme" && !cert.fingerprint && covers(cert, hostname),
  );
}

/** The domains the labels of one service put it on, with their TLS state
 * (#25), as its card on the stack page shows them. */
export function ServiceDomains({
  domains,
  routes: { routes, certificates },
}: {
  domains: Domain[];
  routes: DomainRoutes;
}) {
  const { t } = useTranslation();
  if (domains.length === 0) {
    return null;
  }
  return (
    <ul className="service-domains" aria-label={t("stack.domains")}>
      {domains.map((domain) => {
        const route = routes.get(domain.id);
        // before the first certificate: on its way, or held up
        const order =
          route?.tls === "none" && domain.tls === "auto"
            ? orderFor(certificates, domain.hostname)
            : undefined;
        const failed = order && (order.last_error !== "" || order.status === "failed");
        const secure = route && route.tls !== "none" && route.tls !== "off";
        return (
          <li key={domain.id} className="service-domain">
            {route && <RouteLock tls={route.tls} />}
            <a
              className="mono"
              href={`${secure ? "https" : "http"}://${domain.hostname}`}
              target="_blank"
              rel="noreferrer"
            >
              {domain.hostname}
            </a>
            {route &&
              (failed ? (
                <Link className="small route-tls warn" to="/domains" title={order.last_error}>
                  {t("stack.certificateFailed")}
                </Link>
              ) : (
                <span className={`small route-tls ${order ? "info" : tlsTone(route.tls)}`}>
                  {order ? t("stack.gettingCertificate") : tlsText(t, route)}
                </span>
              ))}
            {route?.password && (
              <KeyRound size={13} aria-label={t("edge.password")} role="img" className="muted" />
            )}
            <span className="muted small">{t("stack.fromLabel")}</span>
          </li>
        );
      })}
    </ul>
  );
}

/** What was wrong with the labels at the last deploy, and the domains whose
 * label is gone, which wait for someone to remove or keep them. */
export function LabelDomainsCard({
  stack,
  domains,
  canAct,
}: {
  stack: Stack;
  domains: Domain[];
  canAct: boolean;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const [removing, setRemoving] = useState<Domain | null>(null);
  const [removeBusy, setRemoveBusy] = useState(false);
  const [busy, setBusy] = useState("");
  const problems = stack.label_problems ?? [];
  const removed = domains.filter((domain) => domain.label_removed);

  if (problems.length === 0 && removed.length === 0) {
    return null;
  }

  const keep = async (domain: Domain) => {
    setBusy(domain.id);
    try {
      await edgeApi.keepDomain(domain.id);
      toast.success(t("stack.kept", { hostname: domain.hostname }));
    } catch (err) {
      toast.error(err);
    } finally {
      setBusy("");
    }
  };

  const remove = async () => {
    if (!removing) {
      return;
    }
    setRemoveBusy(true);
    try {
      await edgeApi.removeDomain(removing.id);
      toast.success(t("stack.domainRemoved", { hostname: removing.hostname }));
    } catch (err) {
      toast.error(err);
    } finally {
      setRemoveBusy(false);
    }
    setRemoving(null);
  };

  return (
    <Card title={t("stack.labels")} note={t("stack.labelsNote")}>
      <div className="dialog-form">
        {problems.length > 0 && (
          <Banner tone="warn">
            <strong>{t("stack.labelProblems", { count: problems.length })}</strong>
            <ul className="label-problems">
              {problems.map((problem) => (
                <li key={`${problem.line ?? 0}:${problem.message}`}>
                  {problem.line ? (
                    <span className="mono">{t("stack.line", { line: problem.line })} </span>
                  ) : null}
                  {problem.message}
                </li>
              ))}
            </ul>
          </Banner>
        )}
        {removed.length > 0 && (
          <>
            <p className="muted small">{t("stack.removedLabelsText")}</p>
            <ul className="list">
              {removed.map((domain) => (
                <li key={domain.id} className="list-row">
                  <div>
                    <div className="mono">{domain.hostname}</div>
                    <div className="muted small">
                      {t("stack.removedLabel", { service: domain.service })}
                    </div>
                  </div>
                  {canAct && (
                    <div className="row-actions">
                      <Button
                        size="small"
                        disabled={busy === domain.id}
                        onClick={() => void keep(domain)}
                        aria-label={t("stack.keepName", { hostname: domain.hostname })}
                      >
                        {t("stack.keep")}
                      </Button>
                      <Button
                        size="small"
                        variant="danger"
                        onClick={() => setRemoving(domain)}
                        aria-label={t("stack.removeDomainName", { hostname: domain.hostname })}
                      >
                        {t("common.remove")}
                      </Button>
                    </div>
                  )}
                </li>
              ))}
            </ul>
          </>
        )}
      </div>
      <ConfirmDialog
        open={removing !== null}
        title={t("stack.removeDomainTitle", { hostname: removing?.hostname ?? "" })}
        confirmLabel={t("common.remove")}
        confirmDisabled={removeBusy}
        danger
        onCancel={() => setRemoving(null)}
        onConfirm={() => void remove()}
      >
        <p className="muted">{t("stack.removeDomainText")}</p>
      </ConfirmDialog>
    </Card>
  );
}
