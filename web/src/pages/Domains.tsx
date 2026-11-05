import { Plus, RefreshCw, Trash2 } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { PageHead } from "../components/Layout";
import { useToast } from "../components/toast";
import {
  Banner,
  Button,
  Card,
  ConfirmDialog,
  EmptyState,
  Field,
  IconButton,
  StatusPill,
  Table,
} from "../components/ui";
import { errorMessage } from "../lib/api";
import { hasRole, useAuth } from "../lib/auth";
import { dnsApi, dnsTone, type DnsKind, type DnsProvider, type DnsZone } from "../lib/dns";
import type { Domain } from "../lib/edge";
import { formatRelative } from "../lib/format";
import { useLiveList } from "../lib/realtime";

/** Domains & TLS: the DNS providers and their zones (#22). */
export function DomainsPage() {
  const { t } = useTranslation();
  const { user } = useAuth();
  const isAdmin = hasRole(user, "admin");
  const providers = useLiveList<DnsProvider>("dns_providers", { sort: "name" });
  const zones = useLiveList<DnsZone>("dns_zones", { sort: "name" });
  const domains = useLiveList<Domain>("domains", { sort: "hostname" });
  const [connecting, setConnecting] = useState(false);

  return (
    <>
      <PageHead
        crumbs={t("nav.web")}
        title={t("nav.domains")}
        description={t("dns.lead")}
        actions={
          isAdmin && (
            <Button variant="primary" onClick={() => setConnecting(true)}>
              <Plus size={15} aria-hidden="true" /> {t("dns.connect")}
            </Button>
          )
        }
      />

      <ProvidersCard providers={providers.items} zones={zones.items} isAdmin={isAdmin} />
      <ZonesCard
        providers={providers.items}
        zones={zones.items}
        domains={domains.items}
        isAdmin={isAdmin}
      />

      {connecting && <ConnectDialog onClose={() => setConnecting(false)} />}
    </>
  );
}

function initials(name: string) {
  return name
    .split(/\s+/)
    .map((word) => word[0] ?? "")
    .join("")
    .slice(0, 2)
    .toUpperCase();
}

function ProvidersCard({
  providers,
  zones,
  isAdmin,
}: {
  providers: DnsProvider[];
  zones: DnsZone[];
  isAdmin: boolean;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const [busy, setBusy] = useState("");
  const [removing, setRemoving] = useState<DnsProvider | null>(null);

  const test = async (provider: DnsProvider) => {
    setBusy(provider.id);
    try {
      const result = await dnsApi.test(provider.id);
      if (result.provider.status === "ok") {
        toast.success(t("dns.tested", { name: provider.name }));
      } else {
        toast.error(result.provider.last_error);
      }
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
      await dnsApi.remove(removing.id);
      toast.success(t("dns.removed", { name: removing.name }));
    } catch (err) {
      toast.error(err);
    } finally {
      setRemoving(null);
    }
  };

  return (
    <Card title={t("dns.providers")}>
      {providers.length === 0 ? (
        <EmptyState title={t("dns.noProviders")}>{t("dns.noProvidersText")}</EmptyState>
      ) : (
        <ul className="list dns-providers">
          {providers.map((provider) => {
            const own = zones.filter((zone) => zone.provider === provider.id);
            return (
              <li key={provider.id} className="dns-provider">
                <span className="avatar" aria-hidden="true">
                  {initials(provider.name)}
                </span>
                <div className="dns-provider-body">
                  <div className="dns-provider-head">
                    <strong>{provider.name}</strong>
                    <StatusPill tone={dnsTone(provider.status)}>
                      {t(`dns.status.${provider.status || "unknown"}`)}
                    </StatusPill>
                  </div>
                  <div className="mono small">
                    {own.length > 0 ? own.map((zone) => zone.name).join(", ") : t("dns.noZones")}
                  </div>
                  {provider.status === "error" && provider.last_error ? (
                    <p className="field-error small">{provider.last_error}</p>
                  ) : (
                    <p className="muted small">
                      {t(`dns.kindName.${provider.kind}`, { defaultValue: provider.kind })}
                      {provider.last_check &&
                        " · " + t("dns.checked", { when: formatRelative(provider.last_check) })}
                    </p>
                  )}
                </div>
                {isAdmin && (
                  <div className="dns-provider-actions">
                    <Button disabled={busy === provider.id} onClick={() => void test(provider)}>
                      <RefreshCw size={14} aria-hidden="true" /> {t("dns.test")}
                    </Button>
                    <IconButton
                      bare
                      aria-label={t("dns.remove", { name: provider.name })}
                      title={t("dns.remove", { name: provider.name })}
                      onClick={() => setRemoving(provider)}
                    >
                      <Trash2 size={15} aria-hidden="true" />
                    </IconButton>
                  </div>
                )}
              </li>
            );
          })}
        </ul>
      )}
      <ConfirmDialog
        open={removing !== null}
        title={t("dns.removeTitle", { name: removing?.name ?? "" })}
        confirmLabel={t("common.remove")}
        danger
        onCancel={() => setRemoving(null)}
        onConfirm={() => void remove()}
      >
        <p>{t("dns.removeText")}</p>
      </ConfirmDialog>
    </Card>
  );
}

function ZonesCard({
  providers,
  zones,
  domains,
  isAdmin,
}: {
  providers: DnsProvider[];
  zones: DnsZone[];
  domains: Domain[];
  isAdmin: boolean;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const names = useMemo(() => new Map(providers.map((p) => [p.id, p.name])), [providers]);

  const toggle = async (zone: DnsZone) => {
    try {
      await dnsApi.setManaged(zone.id, !zone.managed);
    } catch (err) {
      toast.error(err);
    }
  };

  if (zones.length === 0) {
    return null;
  }

  return (
    <Card title={t("dns.zones")} note={t("dns.zonesNote")} flush>
      <Table label={t("dns.zones")}>
        <thead>
          <tr>
            <th>{t("dns.zone")}</th>
            <th>{t("dns.provider")}</th>
            <th>{t("dns.domains")}</th>
            <th>{t("dns.state")}</th>
            <th>{t("dns.managed")}</th>
          </tr>
        </thead>
        <tbody>
          {zones.map((zone) => {
            const used = domains.filter((domain) => domain.zone === zone.id);
            return (
              <tr key={zone.id}>
                <td className="mono">{zone.name}</td>
                <td>{names.get(zone.provider) ?? "–"}</td>
                <td className="muted">
                  {used.length === 0
                    ? t("dns.unused")
                    : used.map((domain) => domain.hostname).join(", ")}
                </td>
                <td>
                  <StatusPill tone={dnsTone(zone.status)}>
                    {t(`dns.status.${zone.status || "unknown"}`)}
                  </StatusPill>
                  {zone.status === "error" && zone.last_error && (
                    <div className="field-error small">{zone.last_error}</div>
                  )}
                </td>
                <td>
                  <label className="checkbox">
                    <input
                      type="checkbox"
                      checked={zone.managed}
                      disabled={!isAdmin}
                      onChange={() => void toggle(zone)}
                      aria-label={t("dns.manageZone", { zone: zone.name })}
                    />
                    <span>{zone.managed ? t("dns.managedYes") : t("dns.managedNo")}</span>
                  </label>
                </td>
              </tr>
            );
          })}
        </tbody>
      </Table>
    </Card>
  );
}

/** Connects a provider: its kind, a name, the credentials it asks for and,
 * for a kind whose API cannot list them, its zones. */
export function ConnectDialog({ onClose }: { onClose: () => void }) {
  const { t } = useTranslation();
  const toast = useToast();
  const [kinds, setKinds] = useState<DnsKind[]>([]);
  const [kind, setKind] = useState("cloudflare");
  const [name, setName] = useState("");
  const [credentials, setCredentials] = useState<Record<string, string>>({});
  const [zones, setZones] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let cancelled = false;
    dnsApi
      .kinds()
      .then((loaded) => {
        if (!cancelled) {
          setKinds(loaded);
        }
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          setError(errorMessage(err));
        }
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const driver = kinds.find((k) => k.kind === kind);

  const connect = async () => {
    if (!driver) {
      return;
    }
    setBusy(true);
    setError("");
    try {
      const result = await dnsApi.connect({
        name: name.trim() || driver.label,
        kind,
        credentials,
        zones: zones
          .split(/[\s,]+/)
          .map((zone) => zone.trim())
          .filter(Boolean),
      });
      if (result.provider.status === "error") {
        // saved, but its API said no: the card shows why too
        toast.error(result.provider.last_error);
      } else {
        toast.success(
          t("dns.connected", { name: result.provider.name, count: result.zones.length }),
        );
      }
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
      title={t("dns.connectTitle")}
      confirmLabel={busy ? t("dns.connecting") : t("dns.connect")}
      onCancel={onClose}
      onConfirm={() => void connect()}
    >
      <div className="dialog-form">
        <Field label={t("dns.kind")} htmlFor="dns-kind">
          <select
            id="dns-kind"
            className="input"
            value={kind}
            onChange={(event) => {
              setKind(event.target.value);
              setCredentials({});
            }}
          >
            {kinds.map((k) => (
              <option key={k.kind} value={k.kind}>
                {t(`dns.kindName.${k.kind}`, { defaultValue: k.label })}
              </option>
            ))}
          </select>
        </Field>
        {driver && (
          <p className="muted small">
            {t(`dns.kindNote.${driver.kind}`, { defaultValue: driver.note ?? "" })}
          </p>
        )}
        <Field label={t("dns.name")} htmlFor="dns-name">
          <input
            id="dns-name"
            className="input"
            placeholder={driver?.label}
            value={name}
            onChange={(event) => setName(event.target.value)}
          />
        </Field>
        {driver?.fields.map((field) => (
          <Field
            key={field.name}
            label={t(`dns.field.${field.name}`, { defaultValue: field.name })}
            htmlFor={`dns-field-${field.name}`}
          >
            <input
              id={`dns-field-${field.name}`}
              className="input mono"
              type={field.secret ? "password" : "text"}
              autoComplete="off"
              value={credentials[field.name] ?? ""}
              onChange={(event) =>
                setCredentials((current) => ({ ...current, [field.name]: event.target.value }))
              }
            />
          </Field>
        ))}
        {driver && !driver.lists_zones && (
          <Field label={t("dns.zonesField")} htmlFor="dns-zones">
            <input
              id="dns-zones"
              className="input mono"
              placeholder="example.com, example.org"
              value={zones}
              onChange={(event) => setZones(event.target.value)}
            />
          </Field>
        )}
        {error && <Banner tone="err">{error}</Banner>}
      </div>
    </ConfirmDialog>
  );
}
