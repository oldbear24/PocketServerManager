import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { errorMessage } from "../lib/api";
import { hasRole, useAuth } from "../lib/auth";
import { dnsApi, dnsTone, type DnsStatus, type DomainDns } from "../lib/dns";
import { useToast } from "./toast";
import { Banner, Button, StatusPill, Table } from "./ui";

/** The pill of a domain's DNS state; it opens the domain's records. */
export function DnsPill({ status, onClick }: { status: DnsStatus; onClick?: () => void }) {
  const { t } = useTranslation();
  const label = t(`dns.domainStatus.${status || "none"}`);
  if (!onClick) {
    return (
      <StatusPill tone={dnsTone(status)} dot={false}>
        {label}
      </StatusPill>
    );
  }
  return (
    <button
      type="button"
      className="pill-button"
      onClick={onClick}
      aria-label={t("dns.openRecords", { state: label })}
    >
      <StatusPill tone={dnsTone(status)} dot={false}>
        {label}
      </StatusPill>
    </button>
  );
}

/** What a domain needs in DNS: its records, and what the servers answer. */
export function DomainDnsPanel({ domainId }: { domainId: string }) {
  const { t } = useTranslation();
  const toast = useToast();
  const { user } = useAuth();
  const [state, setState] = useState<DomainDns | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const load = useCallback(
    async (check: boolean) => {
      setBusy(true);
      try {
        setState(await dnsApi.domain(domainId, check));
        setError("");
      } catch (err) {
        setError(errorMessage(err));
      } finally {
        setBusy(false);
      }
    },
    [domainId],
  );

  useEffect(() => {
    void load(true);
  }, [load]);

  const sync = async () => {
    setBusy(true);
    try {
      await dnsApi.sync(domainId);
      toast.success(t("dns.synced"));
    } catch (err) {
      toast.error(err);
    } finally {
      setBusy(false);
    }
    // what the servers answer now
    await load(true);
  };

  if (!state) {
    return error ? (
      <div className="dialog-form">
        <Banner tone="err">{error}</Banner>
        <div className="row-actions">
          <Button disabled={busy} onClick={() => void load(true)}>
            {t("dns.checkNow")}
          </Button>
        </div>
      </div>
    ) : (
      <p className="muted">{t("common.loading")}</p>
    );
  }

  const answers = new Map(
    (state.propagation ?? []).map((p) => [`${p.record.type} ${p.record.value}`, p]),
  );

  return (
    <div className="dialog-form">
      <div>
        <DnsPill status={state.status} />{" "}
        <span className="muted small">
          {state.zone
            ? t("dns.inZone", { zone: state.zone, provider: state.provider?.name ?? "" })
            : t("dns.noZoneFor")}
        </span>
      </div>
      {error && <Banner tone="err">{error}</Banner>}
      {state.problem && <Banner tone="err">{state.problem}</Banner>}
      {!state.problem && state.error && <Banner tone="warn">{state.error}</Banner>}
      {state.manual && !state.problem && <p>{t("dns.manualText")}</p>}
      {state.lan && state.provider && (
        <p className="muted small">{t("dns.lanAnswered", { provider: state.provider.name })}</p>
      )}

      {state.records.length > 0 && (
        <Table label={t("dns.records")} compact>
          <thead>
            <tr>
              <th>{t("dns.recordName")}</th>
              <th>{t("dns.recordType")}</th>
              <th>{t("dns.recordValue")}</th>
              <th>TTL</th>
              <th>{t("dns.resolves")}</th>
            </tr>
          </thead>
          <tbody className="dns-records">
            {state.records.map((record) => {
              const answer = answers.get(`${record.type} ${record.value}`);
              return (
                <tr key={`${record.type} ${record.value}`}>
                  <td className="mono">{record.hostname}</td>
                  <td className="mono">{record.type}</td>
                  <td className="mono">{record.value}</td>
                  <td className="mono">{record.ttl}</td>
                  <td>
                    {answer ? (
                      <StatusPill tone={answer.resolved ? "ok" : "warn"}>
                        {answer.resolved
                          ? t("dns.resolvesYes")
                          : t("dns.resolvesCount", {
                              ok: answer.answers.filter((a) => a.ok).length,
                              count: answer.answers.length,
                            })}
                      </StatusPill>
                    ) : (
                      "–"
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </Table>
      )}

      <div className="row-actions">
        <Button disabled={busy} onClick={() => void load(true)}>
          {t("dns.checkNow")}
        </Button>
        {hasRole(user, "operator") && !state.manual && (
          <Button disabled={busy} onClick={() => void sync()}>
            {t("dns.sync")}
          </Button>
        )}
      </div>
    </div>
  );
}
