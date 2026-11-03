import { useEffect, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { PageHead } from "../components/Layout";
import { useToast } from "../components/toast";
import {
  Banner,
  Button,
  Card,
  CodeBlock,
  Field,
  StatusPill,
  Tabs,
  Timeline,
  type TimelineItem,
  type TimelineState,
} from "../components/ui";
import { api, errorMessage } from "../lib/api";
import { formatTime, formatVersion, parseDate } from "../lib/format";
import {
  composeSnippet,
  dockerRunCommand,
  scriptCommand,
  type InstallParams,
} from "../lib/install";
import { useLiveList, useLiveRecord, useNow } from "../lib/realtime";
import type { EnrollmentToken, Host, NewEnrollmentToken, PanelInfo } from "../lib/types";

const hostNamePattern = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;
const allRoles = ["docker", "edge", "backup"] as const;

function formatCountdown(ms: number): string {
  if (ms <= 0) {
    return "0:00";
  }
  const seconds = Math.floor(ms / 1000);
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, "0")}`;
}

export function AgentsPage() {
  const { t } = useTranslation();
  const toast = useToast();
  const now = useNow(1000);

  const [name, setName] = useState("");
  const [roles, setRoles] = useState<string[]>(["docker"]);
  const [token, setToken] = useState<NewEnrollmentToken | null>(null);
  const [nameError, setNameError] = useState("");
  const [busy, setBusy] = useState(false);
  const [variant, setVariant] = useState("docker");
  const [info, setInfo] = useState<PanelInfo | null>(null);

  useEffect(() => {
    let cancelled = false;
    api
      .info()
      .then((loaded) => {
        if (!cancelled) {
          setInfo(loaded);
        }
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          toast.error(err);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [toast]);

  const toggleRole = (role: string) => {
    setRoles((current) =>
      current.includes(role) ? current.filter((item) => item !== role) : [...current, role],
    );
  };

  const createToken = async () => {
    if (!hostNamePattern.test(name)) {
      setNameError(t("connect.nameInvalid"));
      return;
    }
    setNameError("");
    setBusy(true);
    try {
      setToken(await api.createEnrollmentToken(name, roles));
    } catch (err) {
      setNameError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const remaining = token ? (parseDate(token.expires_at)?.getTime() ?? 0) - now : 0;
  const expired = token !== null && remaining <= 0;

  const params: InstallParams | null = token && {
    panel: token.panel,
    caPin: token.ca_pin,
    token: token.token,
    name: name || token.name,
    roles: roles.length ? roles : token.roles,
    image: token.agent_image,
    origin: window.location.origin,
  };

  return (
    <>
      <PageHead
        crumbs={t("nav.agents")}
        title={t("connect.title")}
        description={
          <>
            {t("connect.descriptionBefore")} <code>{info?.grpc_address ?? "…"}</code>
            {t("connect.descriptionAfter")}
          </>
        }
      />

      <div className="columns">
        <div className="column-wide">
          <Card title={t("connect.step1")}>
            <div className="form-grid">
              <Field label={t("connect.name")} htmlFor="host-name" error={nameError}>
                <input
                  id="host-name"
                  className="input mono"
                  value={name}
                  placeholder="nas-02"
                  autoComplete="off"
                  spellCheck={false}
                  onChange={(event) => setName(event.target.value)}
                />
              </Field>
              <div className="field">
                <span className="field-label">{t("connect.roles")}</span>
                <div className="form-grid roles">
                  {allRoles.map((role) => (
                    <label key={role} className="check-card">
                      <input
                        type="checkbox"
                        checked={roles.includes(role)}
                        onChange={() => toggleRole(role)}
                      />
                      <span>
                        <div>{t(`host.role.${role}`)}</div>
                        <div className="cell-sub">{t(`connect.roleNote.${role}`)}</div>
                      </span>
                    </label>
                  ))}
                </div>
              </div>
            </div>
          </Card>

          <Card
            title={t("connect.step2")}
            flush
            side={
              token && !expired ? (
                <>
                  <span className="token-chip">
                    <code>{token.token}</code>
                    {t("connect.tokenLeft", { time: formatCountdown(remaining) })}
                  </span>
                  <Button disabled={busy} onClick={() => void createToken()}>
                    {t("connect.newToken")}
                  </Button>
                </>
              ) : undefined
            }
          >
            {!token || expired ? (
              <div className="card-body">
                {expired && <Banner tone="warn">{t("connect.tokenExpired")}</Banner>}
                <div style={{ marginTop: expired ? 12 : 0 }}>
                  <Button
                    variant="primary"
                    disabled={busy || !name}
                    onClick={() => void createToken()}
                  >
                    {t("connect.createToken")}
                  </Button>
                </div>
              </div>
            ) : (
              <>
                <Tabs
                  label={t("connect.step2")}
                  active={variant}
                  onChange={setVariant}
                  items={[
                    { id: "docker", label: "docker run" },
                    { id: "compose", label: "Docker Compose" },
                    { id: "script", label: t("connect.script") },
                  ]}
                />
                <CodeBlock
                  label={t("connect.step2")}
                  code={
                    variant === "docker"
                      ? dockerRunCommand(params!)
                      : variant === "compose"
                        ? composeSnippet(params!)
                        : scriptCommand(params!)
                  }
                />
                <div className="card-body">
                  <p className="muted small">
                    {variant === "compose" ? t("connect.composeNote") : t("connect.mountsNote")}
                  </p>
                </div>
              </>
            )}
          </Card>

          {token && <Checklist tokenId={token.id} />}
        </div>

        <div className="column-side">
          <Card title={t("connect.whatHappens")}>
            <ol style={{ margin: 0, paddingLeft: 18, display: "grid", gap: 8 }}>
              {[1, 2, 3, 4, 5].map((step) => (
                <li key={step}>{t(`connect.happens${step}`)}</li>
              ))}
            </ol>
          </Card>

          <Card title={t("connect.requirements")}>
            <dl className="facts">
              <dt>{t("connect.reqSystem")}</dt>
              <dd>Linux · amd64 / arm64</dd>
              <dt>Docker</dt>
              <dd>{t("connect.reqDocker")}</dd>
              <dt>{t("connect.reqNetwork")}</dt>
              <dd>{t("connect.reqNetworkValue")}</dd>
              <dt>Edge</dt>
              <dd>{t("connect.reqEdge")}</dd>
            </dl>
          </Card>

          <AgentVersions info={info} />
        </div>
      </div>
    </>
  );
}

function Checklist({ tokenId }: { tokenId: string }) {
  const { t } = useTranslation();
  const { record: token } = useLiveRecord<EnrollmentToken>("enrollment_tokens", tokenId);
  const { record: host } = useLiveRecord<Host>("hosts", token?.host || undefined);

  const enrolled = Boolean(token?.used_at);
  const streamOpen = Boolean(host?.stream_open);
  const inventory = Boolean(host?.inventory_at);

  const state = (done: boolean, active: boolean): TimelineState =>
    done ? "done" : active ? "active" : "pending";

  const items: TimelineItem[] = [
    {
      id: "token",
      state: "done",
      title: t("connect.checkToken"),
      note: t("connect.checkTokenNote"),
      side: token ? formatTime(token.created) : undefined,
    },
    {
      id: "connect",
      state: state(enrolled, !enrolled),
      title: enrolled ? t("connect.checkConnected") : t("connect.checkWaiting"),
      note: t("connect.checkWaitingNote"),
    },
    {
      id: "cert",
      state: state(enrolled, false),
      title: t("connect.checkCert"),
      note: t("connect.checkCertNote"),
    },
    {
      id: "stream",
      state: state(streamOpen, enrolled),
      title: t("connect.checkStream"),
      note: t("connect.checkStreamNote"),
    },
    {
      id: "inventory",
      state: state(inventory, streamOpen),
      title: t("connect.checkInventory"),
      note: t("connect.checkInventoryNote"),
    },
  ];

  const done = inventory && host;

  return (
    <Card
      title={t("connect.step3")}
      side={
        done ? (
          <StatusPill tone="ok">{t("connect.done")}</StatusPill>
        ) : (
          <StatusPill tone="info">{t("connect.listening")}</StatusPill>
        )
      }
    >
      <Timeline label={t("connect.step3")} items={items} />
      {done && (
        <div style={{ marginTop: 16 }}>
          <Link to={`/servers/${host.id}`} className="button primary">
            {t("connect.openServer", { name: host.name })}
          </Link>
        </div>
      )}
    </Card>
  );
}

function AgentVersions({ info }: { info: PanelInfo | null }) {
  const { t } = useTranslation();
  const toast = useToast();
  const [busy, setBusy] = useState(false);

  const hosts = useLiveList<Host>("hosts", { sort: "name" });

  const latest = info?.version.replace(/^v/, "");

  const outdated = hosts.items.filter(
    (host) => host.status !== "offline" && latest && host.agent_version !== latest,
  );

  const update = async () => {
    setBusy(true);
    try {
      const result = await api.updateAllHosts();
      toast.success(t("connect.updateStarted", { count: result.hosts }));
    } catch (err) {
      toast.error(err);
    } finally {
      setBusy(false);
    }
  };

  const rows: ReactNode = hosts.items.map((host) => {
    const current = host.agent_version === latest;
    const tone = host.status === "offline" ? "err" : current ? "ok" : "warn";
    return (
      <li key={host.id} className="list-row" style={{ padding: "6px 0", border: 0 }}>
        <span style={{ display: "flex", alignItems: "center", gap: 8, minWidth: 0 }}>
          <span className={`status-dot ${tone}`} aria-hidden="true" />
          <Link to={`/servers/${host.id}/agent`} className="mono" style={{ color: "var(--text)" }}>
            {host.name}
          </Link>
        </span>
        <span className={`mono ${tone}-text`}>
          {formatVersion(host.agent_version)}
          {host.status === "offline" && ` · ${t("host.status.offline")}`}
        </span>
      </li>
    );
  });

  return (
    <Card
      title={t("connect.versions")}
      side={info ? t("connect.latestVersion", { version: info.version }) : undefined}
    >
      {hosts.items.length === 0 ? (
        <p className="muted">{t("connect.noAgents")}</p>
      ) : (
        <ul className="list">{rows}</ul>
      )}
      <p className="muted small" style={{ marginTop: 10 }}>
        {t("connect.updaterNote")}
      </p>
      {info && !info.images_pinned && (
        <p className="warn-text small" style={{ marginTop: 8 }}>
          {t("connect.notPinned")}
        </p>
      )}
      <div style={{ marginTop: 12 }}>
        <Button
          disabled={busy || outdated.length === 0 || !info?.images_pinned}
          onClick={() => void update()}
        >
          {t("connect.updateAgents", { count: outdated.length })}
        </Button>
      </div>
    </Card>
  );
}
