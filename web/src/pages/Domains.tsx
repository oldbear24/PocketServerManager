import { Plus, RefreshCw, Trash2 } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { useSearchParams } from "react-router-dom";

import {
  AcmeSummaryCard,
  CertificatesCard,
  RequestCertificateDialog,
} from "../components/Certificates";
import { InternalCACard } from "../components/InternalCA";
import { PageHead } from "../components/Layout";
import { useToast } from "../components/toast";
import {
  Banner,
  Button,
  Card,
  ConfirmDialog,
  Drawer,
  EmptyState,
  Field,
  IconButton,
  StatusPill,
  Table,
} from "../components/ui";
import { api, errorMessage } from "../lib/api";
import { hasRole, useAuth } from "../lib/auth";
import type { AcmeAccount } from "../lib/certificates";
import {
  dnsApi,
  dnsTone,
  type DnsKind,
  type DnsProvider,
  type DnsStatus,
  type DnsZone,
} from "../lib/dns";
import type { Certificate, Domain } from "../lib/edge";
import { formatRelative } from "../lib/format";
import { useLiveList } from "../lib/realtime";
import type { Host } from "../lib/types";

/** The certificate the panel serves its UI and API with, asked again when the
 * panel orders another (#63). */
function usePanelCertificate(certificates: Certificate[]) {
  const [id, setId] = useState("");
  const ordered = certificates
    .filter((cert) => cert.panel)
    .map((cert) => `${cert.id}:${cert.fingerprint}`)
    .join(",");
  useEffect(() => {
    if (!ordered) {
      return;
    }
    let cancelled = false;
    api
      .info()
      .then((info) => {
        if (!cancelled) {
          setId(info.panel_certificate ?? "");
        }
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, [ordered]);
  return ordered ? id : "";
}

/** Domains & TLS (#27): the DNS providers and their zones (#22), the
 * certificates the edges and the panel serve (#23, #63), the ACME account and
 * the internal CA (#26). A warning elsewhere links here with ?provider= or ?certificate=,
 * which brings its card into view or opens it. */
export function DomainsPage() {
  const { t } = useTranslation();
  const { user } = useAuth();
  const isAdmin = hasRole(user, "admin");
  const providers = useLiveList<DnsProvider>("dns_providers", { sort: "name" });
  const zones = useLiveList<DnsZone>("dns_zones", { sort: "name" });
  const domains = useLiveList<Domain>("domains", { sort: "hostname" });
  const certificates = useLiveList<Certificate>("certificates", { sort: "not_after,created" });
  const accounts = useLiveList<AcmeAccount>("acme_accounts", { sort: "role" });
  const hosts = useLiveList<Host>("hosts", { sort: "name" });
  const panelCertificate = usePanelCertificate(certificates.items);
  const [connecting, setConnecting] = useState(false);
  const [requesting, setRequesting] = useState(false);
  const [managing, setManaging] = useState<string | null>(null);
  const [params, setParams] = useSearchParams();
  const focusProvider = params.get("provider") ?? "";
  const focusCertificate = params.get("certificate") ?? "";

  const forget = useCallback(
    (key: string) =>
      setParams(
        (current) => {
          const next = new URLSearchParams(current);
          next.delete(key);
          return next;
        },
        { replace: true },
      ),
    [setParams],
  );
  const managed = providers.items.find((provider) => provider.id === managing);

  return (
    <>
      <PageHead
        crumbs={t("nav.web")}
        title={t("nav.domains")}
        description={t("dns.lead")}
        actions={
          isAdmin && (
            <>
              <Button onClick={() => setConnecting(true)}>{t("dns.connect")}</Button>
              <Button variant="primary" onClick={() => setRequesting(true)}>
                <Plus size={15} aria-hidden="true" /> {t("cert.request")}
              </Button>
            </>
          )
        }
      />

      {providers.error ? (
        <Banner tone="err">{errorMessage(providers.error)}</Banner>
      ) : (
        <ProvidersSection
          providers={providers.items}
          zones={zones.items}
          hosts={hosts.items}
          isAdmin={isAdmin}
          loading={providers.loading}
          focus={focusProvider}
          onFocusDone={() => forget("provider")}
          onManage={setManaging}
          onConnect={() => setConnecting(true)}
        />
      )}
      {certificates.error ? (
        <Banner tone="err">{errorMessage(certificates.error)}</Banner>
      ) : (
        <CertificatesCard
          certificates={certificates.items}
          hosts={hosts.items}
          zones={zones.items}
          providers={providers.items}
          accounts={accounts.items}
          canRetry={hasRole(user, "operator")}
          isAdmin={isAdmin}
          loading={certificates.loading}
          onRequest={() => setRequesting(true)}
          focus={focusCertificate}
          onFocusDone={() => forget("certificate")}
          panelCertificate={panelCertificate}
        />
      )}
      <div className="columns">
        <div className="column-wide">
          <ZonesCard providers={providers.items} zones={zones.items} domains={domains.items} />
        </div>
        <div className="column-side">
          <AcmeSummaryCard accounts={accounts.items} isAdmin={isAdmin} />
          <InternalCACard />
        </div>
      </div>

      {connecting && <ConnectDialog hosts={hosts.items} onClose={() => setConnecting(false)} />}
      {requesting && (
        <RequestCertificateDialog accounts={accounts.items} onClose={() => setRequesting(false)} />
      )}
      {managed && (
        <ManageProviderDrawer
          provider={managed}
          zones={zones.items.filter((zone) => zone.provider === managed.id)}
          hosts={hosts.items}
          onClose={() => setManaging(null)}
        />
      )}
    </>
  );
}

/** Kinds reached through the agent of a server on their network (#26). */
const lanKinds = new Set(["adguard", "pihole"]);

/** The kinds PocketCloud connects, as the card for more names them. */
const knownKinds = ["cloudflare", "hetzner", "wedos", "adguard", "pihole"];

/** The mark of a provider's card: its kind's, or its name's initials. */
const kindMarks: Record<string, string> = {
  cloudflare: "CF",
  hetzner: "HZ",
  wedos: "WE",
  adguard: "AG",
  pihole: "PH",
  manual: "M",
};

function initials(name: string) {
  return name
    .split(/\s+/)
    .map((word) => word[0] ?? "")
    .join("")
    .slice(0, 2)
    .toUpperCase();
}

/** Whether a provider needs a look: its API failed, for itself or for one of
 * its managed zones. */
export function failingZone(provider: DnsProvider, zones: DnsZone[]): DnsZone | undefined {
  return zones.find(
    (zone) => zone.provider === provider.id && zone.managed && zone.status === "error",
  );
}

function ProvidersSection({
  providers,
  zones,
  hosts,
  isAdmin,
  loading,
  focus,
  onFocusDone,
  onManage,
  onConnect,
}: {
  providers: DnsProvider[];
  zones: DnsZone[];
  hosts: Host[];
  isAdmin: boolean;
  loading: boolean;
  focus: string;
  onFocusDone: () => void;
  onManage: (id: string) => void;
  onConnect: () => void;
}) {
  const { t } = useTranslation();
  const connected = new Set(providers.map((provider) => provider.kind));
  const others = knownKinds
    .filter((kind) => !connected.has(kind))
    .map((kind) => t(`dns.kindName.${kind}`));

  return (
    <section className="providers-section" aria-labelledby="providers-heading">
      <h2 id="providers-heading" className="section-heading">
        {t("dns.providers")}
      </h2>
      {loading && providers.length === 0 ? (
        <p className="muted">{t("common.loading")}</p>
      ) : (
        <div className="provider-grid">
          {providers.map((provider) => (
            <ProviderCard
              key={provider.id}
              provider={provider}
              zones={zones}
              hosts={hosts}
              isAdmin={isAdmin}
              focused={provider.id === focus}
              onFocusDone={onFocusDone}
              onManage={() => onManage(provider.id)}
            />
          ))}
          <article className="provider-card" aria-labelledby="provider-more">
            <div className="provider-card-head">
              <span className="provider-mark" aria-hidden="true">
                <Plus size={14} />
              </span>
              <h3 id="provider-more" className="provider-card-name">
                {providers.length === 0
                  ? t("dns.noProviders")
                  : others.length > 0
                    ? t("dns.kindAndOthers", { kind: others[0] })
                    : t("dns.moreProviders")}
              </h3>
              <StatusPill tone="off">{t("dns.notConnected")}</StatusPill>
            </div>
            <p className="provider-card-zones">{t("dns.manualMode")}</p>
            <p className="provider-card-note muted">
              {providers.length === 0
                ? t("dns.supportedKinds", { kinds: others.join(", ") })
                : others.length > 0
                  ? t("dns.alsoSupported", { kinds: others.join(", ") })
                  : t("dns.anotherAccount")}
            </p>
            {isAdmin && (
              <Button size="small" className="provider-card-action" onClick={onConnect}>
                {t("dns.connectShort")}
              </Button>
            )}
          </article>
        </div>
      )}
    </section>
  );
}

function ProviderCard({
  provider,
  zones,
  hosts,
  isAdmin,
  focused,
  onFocusDone,
  onManage,
}: {
  provider: DnsProvider;
  zones: DnsZone[];
  hosts: Host[];
  isAdmin: boolean;
  focused: boolean;
  onFocusDone: () => void;
  onManage: () => void;
}) {
  const { t } = useTranslation();
  const ref = useRef<HTMLElement>(null);
  const own = zones.filter((zone) => zone.provider === provider.id);
  const zone = failingZone(provider, zones);
  const failing = provider.status === "error" || zone !== undefined;
  const status: DnsStatus = zone ? "error" : provider.status || "unknown";
  const lan = lanKinds.has(provider.kind);
  const titleId = `provider-${provider.id}-name`;

  // a warning's link brings the card into view
  useEffect(() => {
    if (focused && ref.current) {
      ref.current.scrollIntoView?.({ block: "center" });
      ref.current.focus();
      onFocusDone();
    }
  }, [focused, onFocusDone]);

  let note: string;
  if (provider.status === "error" && provider.last_error) {
    note = provider.last_error;
  } else if (zone) {
    note = `${zone.name}: ${zone.last_error}`;
  } else {
    const detail = lan
      ? t("dns.throughAgent", {
          server: hosts.find((host) => host.id === provider.host)?.name ?? "–",
        })
      : t(`dns.kindDetail.${provider.kind}`, { defaultValue: "" });
    note = [
      detail,
      provider.last_check && t("dns.checked", { when: formatRelative(provider.last_check) }),
    ]
      .filter(Boolean)
      .join(" · ");
  }

  return (
    <article
      ref={ref}
      id={`provider-${provider.id}`}
      tabIndex={-1}
      className={`provider-card${failing ? " warn" : ""}`}
      aria-labelledby={titleId}
    >
      <div className="provider-card-head">
        <span className="provider-mark" aria-hidden="true">
          {kindMarks[provider.kind] ?? initials(provider.name)}
        </span>
        <h3 id={titleId} className="provider-card-name">
          {provider.name}
        </h3>
        {/* a card that needs a fix is a warning, as in the design */}
        <StatusPill tone={failing ? "warn" : dnsTone(status)}>
          {t(`dns.status.${status}`)}
        </StatusPill>
      </div>
      <p className="provider-card-zones">
        {own.length > 0 ? own.map((item) => item.name).join(", ") : t("dns.noZones")}
        {lan && own.length > 0 && ` (${t("dns.lanRewrites")})`}
      </p>
      <p className={`provider-card-note${failing ? " warn-text" : " muted"}`}>{note}</p>
      {isAdmin && (
        <Button
          size="small"
          className="provider-card-action"
          aria-label={t(failing ? "dns.fixName" : "dns.manageName", { name: provider.name })}
          onClick={onManage}
        >
          {failing ? t("dns.fix") : t("dns.manage")}
        </Button>
      )}
    </article>
  );
}

/** Everything about one provider: its state and why it failed, its zones,
 * the server that reaches a DNS server of the LAN, new credentials, a test
 * and its removal. */
export function ManageProviderDrawer({
  provider,
  zones,
  hosts,
  onClose,
}: {
  provider: DnsProvider;
  zones: DnsZone[];
  hosts: Host[];
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const [kinds, setKinds] = useState<DnsKind[]>([]);
  const [busy, setBusy] = useState(false);
  const [credentials, setCredentials] = useState<Record<string, string>>({});
  const [zoneName, setZoneName] = useState("");
  const [removing, setRemoving] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    dnsApi
      .kinds()
      .then((loaded) => !cancelled && setKinds(loaded))
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, []);
  const driver = kinds.find((kind) => kind.kind === provider.kind);
  const lan = lanKinds.has(provider.kind);
  const kindName = t(`dns.kindName.${provider.kind}`, { defaultValue: provider.kind });

  const run = async (action: () => Promise<unknown>, done?: string) => {
    setBusy(true);
    setError("");
    try {
      const result = await action();
      const state = (result as { provider?: DnsProvider } | undefined)?.provider;
      if (state?.status === "error") {
        setError(state.last_error);
      } else if (done) {
        toast.success(done);
      }
      return true;
    } catch (err) {
      setError(errorMessage(err));
      return false;
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    setRemoving(false);
    if (await run(() => dnsApi.remove(provider.id), t("dns.removed", { name: provider.name }))) {
      onClose();
    }
  };

  return (
    <Drawer
      title={provider.name}
      onClose={onClose}
      actions={
        <>
          <Button
            variant="danger"
            disabled={busy}
            onClick={() => setRemoving(true)}
            aria-label={t("dns.remove", { name: provider.name })}
          >
            <Trash2 size={14} aria-hidden="true" /> {t("common.remove")}
          </Button>
          <Button
            disabled={busy}
            onClick={() =>
              void run(() => dnsApi.test(provider.id), t("dns.tested", { name: provider.name }))
            }
          >
            <RefreshCw size={14} aria-hidden="true" /> {t("dns.test")}
          </Button>
          <Button variant="primary" onClick={onClose}>
            {t("common.close")}
          </Button>
        </>
      }
    >
      <dl className="facts">
        <dt>{t("dns.kind")}</dt>
        <dd>{kindName}</dd>
        <dt>{t("dns.state")}</dt>
        <dd>
          <StatusPill tone={dnsTone(provider.status || "unknown")}>
            {t(`dns.status.${provider.status || "unknown"}`)}
          </StatusPill>
          {provider.last_check && (
            <span className="muted small">
              {" "}
              {t("dns.checked", { when: formatRelative(provider.last_check) })}
            </span>
          )}
        </dd>
      </dl>
      {provider.status === "error" && provider.last_error && (
        <Banner tone="warn">{provider.last_error}</Banner>
      )}
      {error && error !== provider.last_error && <Banner tone="err">{error}</Banner>}
      {driver?.note && (
        <p className="muted small">
          {t(`dns.kindNote.${provider.kind}`, { defaultValue: driver.note })}
        </p>
      )}

      {lan && (
        <Field label={t("dns.server")} htmlFor="manage-server">
          <select
            id="manage-server"
            className="input"
            value={provider.host}
            disabled={busy}
            onChange={(event) =>
              void run(
                () => dnsApi.change(provider.id, { host: event.target.value }),
                t("dns.serverChanged", {
                  name: provider.name,
                  server: hosts.find((host) => host.id === event.target.value)?.name ?? "",
                }),
              )
            }
          >
            {!provider.host && <option value="">{t("dns.chooseServer")}</option>}
            {hosts.map((host) => (
              <option key={host.id} value={host.id}>
                {host.name}
                {host.status === "online" ? "" : ` (${t("dns.serverOffline")})`}
              </option>
            ))}
          </select>
          {!provider.host && <p className="field-error small">{t("dns.noServer")}</p>}
        </Field>
      )}

      <h3 className="drawer-heading">{t("dns.zones")}</h3>
      {zones.length === 0 ? (
        <p className="muted small">{t("dns.noZones")}</p>
      ) : (
        <ul className="list manage-zones">
          {zones.map((zone) => (
            <li key={zone.id} className="list-row">
              <div>
                <div className="mono">{zone.name}</div>
                {zone.status === "error" && zone.last_error && (
                  <div className="field-error small">{zone.last_error}</div>
                )}
              </div>
              <StatusPill tone={dnsTone(zone.status || "unknown")}>
                {t(`dns.status.${zone.status || "unknown"}`)}
              </StatusPill>
              <label className="checkbox">
                <input
                  type="checkbox"
                  checked={zone.managed}
                  disabled={busy}
                  onChange={() => void run(() => dnsApi.setManaged(zone.id, !zone.managed))}
                  aria-label={t("dns.managedZone", { zone: zone.name })}
                />
                <span>{t("dns.managed")}</span>
              </label>
              {driver && !driver.lists_zones && (
                <IconButton
                  bare
                  aria-label={t("dns.removeZone", { zone: zone.name })}
                  title={t("dns.removeZone", { zone: zone.name })}
                  disabled={busy}
                  onClick={() =>
                    void run(
                      () => dnsApi.removeZone(zone.id),
                      t("dns.zoneRemoved", { zone: zone.name }),
                    )
                  }
                >
                  <Trash2 size={14} aria-hidden="true" />
                </IconButton>
              )}
            </li>
          ))}
        </ul>
      )}
      {driver && !driver.lists_zones && (
        <form
          className="manage-add-zone"
          onSubmit={(event) => {
            event.preventDefault();
            const name = zoneName.trim();
            if (name) {
              void run(
                () => dnsApi.addZone(provider.id, name),
                t("dns.zoneAdded", { zone: name }),
              ).then((ok) => ok && setZoneName(""));
            }
          }}
        >
          <Field label={t("dns.addZone")} htmlFor="manage-zone">
            <input
              id="manage-zone"
              className="input mono"
              value={zoneName}
              placeholder={lan ? "home.arpa" : "example.org"}
              onChange={(event) => setZoneName(event.target.value)}
            />
          </Field>
          <Button type="submit" disabled={busy || !zoneName.trim()}>
            {t("dns.add")}
          </Button>
        </form>
      )}

      {driver && (driver.fields ?? []).length > 0 && (
        <form
          className="manage-credentials"
          onSubmit={(event) => {
            event.preventDefault();
            void run(
              () => dnsApi.change(provider.id, { credentials }),
              t("dns.credentialsSaved", { name: provider.name }),
            ).then((ok) => ok && setCredentials({}));
          }}
        >
          <h3 className="drawer-heading">{t("dns.credentials")}</h3>
          <p className="muted small">{t("dns.credentialsNote")}</p>
          {(driver.fields ?? []).map((field) => (
            <Field
              key={field.name}
              label={t(`dns.fieldOf.${driver.kind}.${field.name}`, {
                defaultValue: t(`dns.field.${field.name}`, { defaultValue: field.name }),
              })}
              htmlFor={`manage-field-${field.name}`}
            >
              <input
                id={`manage-field-${field.name}`}
                className="input mono"
                type={field.secret ? "password" : "text"}
                autoComplete={field.secret ? "new-password" : "off"}
                value={credentials[field.name] ?? ""}
                onChange={(event) =>
                  setCredentials((current) => ({ ...current, [field.name]: event.target.value }))
                }
              />
            </Field>
          ))}
          <Button type="submit" disabled={busy}>
            {t("dns.saveCredentials")}
          </Button>
        </form>
      )}

      <ConfirmDialog
        open={removing}
        title={t("dns.removeTitle", { name: provider.name })}
        confirmLabel={t("common.remove")}
        danger
        onCancel={() => setRemoving(false)}
        onConfirm={() => void remove()}
      >
        <p>{t("dns.removeText")}</p>
      </ConfirmDialog>
    </Drawer>
  );
}

/** The zones as the design lists them: their provider, the names PocketCloud
 * keeps in each, and their state. Whether PocketCloud manages one is set in
 * its provider's panel. */
function ZonesCard({
  providers,
  zones,
  domains,
}: {
  providers: DnsProvider[];
  zones: DnsZone[];
  domains: Domain[];
}) {
  const { t } = useTranslation();
  const names = useMemo(() => new Map(providers.map((p) => [p.id, p.name])), [providers]);

  return (
    <Card title={t("dns.zones")} note={t("dns.zonesNote")} flush>
      {zones.length === 0 ? (
        <EmptyState title={t("dns.noZonesTitle")}>{t("dns.noZonesText")}</EmptyState>
      ) : (
        <Table label={t("dns.zones")}>
          <thead>
            <tr>
              <th>{t("dns.zone")}</th>
              <th>{t("dns.provider")}</th>
              <th>{t("dns.domains")}</th>
              <th>
                <span className="visually-hidden">{t("dns.state")}</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {zones.map((zone) => {
              const used = domains.filter((domain) => domain.zone === zone.id);
              const short = used.map((domain) =>
                domain.hostname === zone.name
                  ? "@"
                  : domain.hostname.slice(0, -zone.name.length - 1),
              );
              return (
                <tr key={zone.id}>
                  <td className="mono">{zone.name}</td>
                  <td>{names.get(zone.provider) ?? "–"}</td>
                  <td className="muted">
                    {used.length === 0
                      ? zone.managed
                        ? t("dns.unused")
                        : t("dns.notManaged")
                      : t("dns.hostsIn", {
                          count: used.length,
                          names: short.slice(0, 4).join(", ") + (short.length > 4 ? "…" : ""),
                        })}
                  </td>
                  <td className="zone-state">
                    <StatusPill tone={dnsTone(zone.status || "unknown")}>
                      {t(`dns.status.${zone.status || "unknown"}`)}
                    </StatusPill>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </Table>
      )}
    </Card>
  );
}

/** Connects a provider: its kind, a name, the credentials it asks for and,
 * for a kind whose API cannot list them, its zones. */
export function ConnectDialog({ hosts, onClose }: { hosts: Host[]; onClose: () => void }) {
  const { t } = useTranslation();
  const toast = useToast();
  const [kinds, setKinds] = useState<DnsKind[]>([]);
  const [kind, setKind] = useState("cloudflare");
  const [name, setName] = useState("");
  // the server whose agent reaches a DNS server of a LAN
  const [host, setHost] = useState("");
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
    if (!driver || busy) {
      return;
    }
    setBusy(true);
    setError("");
    try {
      const result = await dnsApi.connect({
        name: name.trim() || driver.label,
        kind,
        credentials,
        ...(driver.agent ? { host } : {}),
        // a kind whose API lists its zones takes none
        zones: driver.lists_zones
          ? []
          : zones
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
      confirmDisabled={busy || !driver || (driver.agent === true && host === "")}
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
              setZones("");
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
        {(driver?.fields ?? []).map((field) => (
          <Field
            key={field.name}
            label={t(`dns.fieldOf.${driver?.kind ?? ""}.${field.name}`, {
              defaultValue: t(`dns.field.${field.name}`, { defaultValue: field.name }),
            })}
            htmlFor={`dns-field-${field.name}`}
          >
            <input
              id={`dns-field-${field.name}`}
              className="input mono"
              type={field.secret ? "password" : "text"}
              placeholder={field.name === "url" ? "http://192.168.1.2:3000" : undefined}
              autoComplete={field.secret ? "new-password" : "off"}
              value={credentials[field.name] ?? ""}
              onChange={(event) =>
                setCredentials((current) => ({ ...current, [field.name]: event.target.value }))
              }
            />
          </Field>
        ))}
        {driver?.agent && (
          <Field label={t("dns.server")} htmlFor="dns-host">
            <select
              id="dns-host"
              className="input"
              value={host}
              onChange={(event) => setHost(event.target.value)}
            >
              <option value="">{t("dns.chooseServer")}</option>
              {hosts.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.name}
                  {item.status === "online" ? "" : ` (${t("dns.serverOffline")})`}
                </option>
              ))}
            </select>
            <p className="muted small">{t("dns.serverHint")}</p>
          </Field>
        )}
        {driver && !driver.lists_zones && (
          <Field label={t("dns.zonesField")} htmlFor="dns-zones">
            <input
              id="dns-zones"
              className="input mono"
              placeholder={driver.agent ? "lan, home.arpa" : "example.com, example.org"}
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
