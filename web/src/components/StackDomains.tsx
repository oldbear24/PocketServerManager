import { KeyRound } from "lucide-react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { edgeApi, tlsTone, type Certificate, type Domain, type EdgeRoute } from "../lib/edge";
import { useLiveList } from "../lib/realtime";
import type { Stack } from "../lib/types";
import { RouteLock, tlsText } from "../pages/EdgeProxy";
import { useToast } from "./toast";
import { Banner, Button, Card, ConfirmDialog } from "./ui";

/**
 * The routes of a stack's domains on its server's edge, with their TLS state
 * as the panel works it out. They are read again when a domain, a
 * certificate or the edge changes.
 */
export function useDomainRoutes(
  hostId: string | undefined,
  edgeVersion: string,
  domains: Domain[],
): Map<string, EdgeRoute> {
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

  return routes;
}

/** The domains the labels of one service put it on, with their TLS state
 * (#25), as its card on the stack page shows them. */
export function ServiceDomains({
  domains,
  routes,
}: {
  domains: Domain[];
  routes: Map<string, EdgeRoute>;
}) {
  const { t } = useTranslation();
  if (domains.length === 0) {
    return null;
  }
  return (
    <ul className="service-domains" aria-label={t("stack.domains")}>
      {domains.map((domain) => {
        const route = routes.get(domain.id);
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
            {route && (
              <span className={`small route-tls ${tlsTone(route.tls)}`}>
                {route.tls === "none" && domain.tls === "auto"
                  ? t("stack.gettingCertificate")
                  : tlsText(t, route)}
              </span>
            )}
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
    try {
      await edgeApi.removeDomain(removing.id);
      toast.success(t("stack.domainRemoved", { hostname: removing.hostname }));
    } catch (err) {
      toast.error(err);
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
        danger
        onCancel={() => setRemoving(null)}
        onConfirm={() => void remove()}
      >
        <p className="muted">{t("stack.removeDomainText")}</p>
      </ConfirmDialog>
    </Card>
  );
}
