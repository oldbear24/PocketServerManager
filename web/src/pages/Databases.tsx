import { Database as DatabaseIcon, Download, Plus, RefreshCw } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { PageHead } from "../components/Layout";
import { useToast } from "../components/toast";
import {
  Banner,
  Button,
  Card,
  CodeBlock,
  ConfirmDialog,
  EmptyState,
  Field,
  IconButton,
  Kpi,
  ProgressBar,
  SegmentedControl,
  StatusPill,
  Table,
  type Tone,
} from "../components/ui";
import { errorMessage } from "../lib/api";
import { hasRole, useAuth } from "../lib/auth";
import {
  connectionShare,
  databaseNamePattern,
  dbApi,
  engineLabel,
  engines,
  envSnippet,
  imageTag,
  serverLabel,
  serverNamePattern,
  type Database,
  type DbCandidate,
  type DbDump,
  type DbEngine,
  type DbMove,
  type DbServer,
  type DbUser,
} from "../lib/databases";
import { formatBytes, formatDate, formatNumber, formatRelative } from "../lib/format";
import { pb } from "../lib/pb";
import { useLiveList, useNow } from "../lib/realtime";
import type { Container, Host, Stack } from "../lib/types";

const candidateRefresh = 30000;

/** The containers of the inventory found inside stacks that are database servers. */
function useCandidates(version: string) {
  const [candidates, setCandidates] = useState<DbCandidate[]>([]);

  useEffect(() => {
    let cancelled = false;
    const load = () => {
      dbApi
        .candidates()
        .then((found) => {
          if (!cancelled) {
            setCandidates(found);
          }
        })
        .catch(() => {
          // only the found servers are missing then
        });
    };
    load();
    const timer = setInterval(load, candidateRefresh);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [version]);

  return candidates;
}

const majorOf = (server: DbServer) =>
  Number.parseInt(server.version || imageTag(server.image), 10) || 0;

/**
 * The shared servers a server's databases could move to: on its host, of its
 * engine and not older, as a dump does not restore into an older server.
 */
function moveTargets(server: DbServer, servers: DbServer[]): DbServer[] {
  if ((server.extensions ?? []).length > 0) {
    return [];
  }
  const own = majorOf(server);
  return servers.filter(
    (other) =>
      other.id !== server.id &&
      other.kind === "shared" &&
      other.status === "ready" &&
      other.host === server.host &&
      other.engine === server.engine &&
      own > 0 &&
      majorOf(other) >= own,
  );
}

/** Whether a dedicated server could move to a shared one on its host. */
function moveTarget(server: DbServer, servers: DbServer[]): string {
  return server.kind === "dedicated" ? (moveTargets(server, servers)[0]?.name ?? "") : "";
}

/** The move of a server's databases that is under way, if any. */
function runningMove(server: DbServer, moves: DbMove[]): DbMove | undefined {
  return moves.find(
    (move) =>
      move.status === "moving" &&
      move.host === server.host &&
      move.source_container === server.container_ref,
  );
}

function serverTone(server: DbServer, container: Container | undefined): Tone {
  if (server.status === "failed") {
    return "err";
  }
  if (server.status === "creating") {
    return "info";
  }
  if (!container) {
    return "off";
  }
  if (container.state !== "running") {
    return "warn";
  }
  return container.health === "unhealthy" ? "err" : "ok";
}

export function DatabasesPage() {
  const { t } = useTranslation();
  const { user } = useAuth();
  const canAct = hasRole(user, "operator");
  const isAdmin = hasRole(user, "admin");

  const servers = useLiveList<DbServer>("db_servers", { sort: "name", perPage: 200 });
  const databases = useLiveList<Database>("databases", { sort: "name", perPage: 500 });
  const users = useLiveList<DbUser>("db_users", { perPage: 500 });
  const moves = useLiveList<DbMove>("db_moves", { sort: "-created", perPage: 200 });
  const hosts = useLiveList<Host>("hosts", { sort: "name" });
  const stacks = useLiveList<Stack>("stacks", { sort: "name", perPage: 500 });
  const containers = useLiveList<Container>("containers", {
    filter: "project != ''",
    perPage: 1000,
  });

  // the inventory changes when a stack starts or stops; the list follows it
  const candidates = useCandidates(`${containers.items.length}/${servers.items.length}`);

  const [selected, setSelected] = useState("");
  const [selectedDb, setSelectedDb] = useState("");
  const [form, setForm] = useState<"server" | "database" | null>(null);

  const hostName = (id: string) => hosts.items.find((host) => host.id === id)?.name ?? "–";
  const containerOf = (server: DbServer) =>
    containers.items.find(
      (container) => container.host === server.host && container.name === server.container_ref,
    );

  const current =
    servers.items.find((server) => server.id === selected) ??
    (selected.startsWith("candidate:") ? undefined : servers.items[0]);
  const candidate = selected.startsWith("candidate:")
    ? candidates.find((item) => `candidate:${item.docker_id}` === selected)
    : undefined;

  const serverDatabases = current
    ? databases.items.filter((database) => database.server === current.id)
    : [];
  const database =
    serverDatabases.find((item) => item.id === selectedDb) ?? serverDatabases[0] ?? null;

  // operators refresh the numbers of what they look at; the panel asks at most once a minute
  const currentId = current?.id;
  useEffect(() => {
    if (canAct && currentId) {
      dbApi.stats(currentId).catch(() => undefined);
    }
  }, [canAct, currentId]);

  const shared = servers.items.filter((server) => server.kind === "shared");
  const totalSize = servers.items.reduce((sum, server) => sum + (server.size_bytes || 0), 0);
  const largest = [...databases.items].sort((a, b) => b.size_bytes - a.size_bytes)[0];
  const onShared = databases.items.filter((item) =>
    shared.some((server) => server.id === item.server),
  ).length;

  return (
    <>
      <PageHead
        title={t("db.title")}
        description={t("db.description")}
        actions={
          canAct && (
            <>
              <Button onClick={() => setForm(form === "server" ? null : "server")}>
                {t("db.newServer")}
              </Button>
              <Button
                variant="primary"
                disabled={servers.items.length === 0}
                onClick={() => setForm(form === "database" ? null : "database")}
              >
                <Plus size={16} aria-hidden="true" />
                {t("db.newDatabase")}
              </Button>
            </>
          )
        }
      />

      {form === "server" && (
        <NewServerCard
          hosts={hosts.items}
          onDone={(id) => {
            setForm(null);
            if (id) {
              setSelected(id);
            }
          }}
        />
      )}
      {form === "database" && (
        <NewDatabaseCard
          servers={servers.items}
          stacks={stacks.items}
          initialServer={current?.id ?? ""}
          hostName={hostName}
          onDone={(server, id) => {
            setForm(null);
            if (server) {
              setSelected(server);
              setSelectedDb(id);
            }
          }}
        />
      )}

      <div className="kpis">
        <Kpi
          label={t("db.kpiServers")}
          value={servers.items.length + candidates.length}
          note={t("db.kpiServersNote", {
            shared: shared.length,
            dedicated: servers.items.length - shared.length + candidates.length,
          })}
        />
        <Kpi
          label={t("db.kpiDatabases")}
          value={databases.items.length}
          note={t("db.kpiDatabasesNote", { count: onShared })}
        />
        <Kpi
          label={t("db.kpiData")}
          value={totalSize ? formatBytes(totalSize) : "–"}
          note={
            largest && largest.size_bytes > 0
              ? t("db.kpiLargest", { name: largest.name, size: formatBytes(largest.size_bytes) })
              : t("db.kpiDataNote")
          }
        />
        <Kpi label={t("db.kpiBackups")} value="–" note={t("db.kpiBackupsNote")} />
      </div>

      <h2 className="section-title">{t("db.servers")}</h2>
      {servers.items.length === 0 && candidates.length === 0 ? (
        <Card>
          <EmptyState title={t("db.noServers")}>{t("db.noServersText")}</EmptyState>
        </Card>
      ) : (
        <div className="db-servers">
          {servers.items.map((server) => {
            const count = databases.items.filter((item) => item.server === server.id).length;
            const container = containerOf(server);
            const move = moveTarget(server, servers.items);
            const moving = runningMove(server, moves.items);
            const pressed = current?.id === server.id && !candidate;
            return (
              <button
                key={server.id}
                type="button"
                className="card db-server"
                aria-pressed={pressed}
                onClick={() => {
                  setSelected(server.id);
                  setSelectedDb("");
                }}
              >
                <span className="db-server-head">
                  <span className="mono db-server-name">{server.name}</span>
                  <StatusPill tone={server.kind === "shared" ? "info" : "off"} dot={false}>
                    {t(`db.kind.${server.kind}`)}
                  </StatusPill>
                </span>
                <span className="db-server-line">
                  {serverLabel(server)} · <span className="mono">{hostName(server.host)}</span>
                </span>
                <span className="db-server-line muted">
                  {t("db.databaseCount", { count })}
                  <span className="num">{formatBytes(server.size_bytes)}</span>
                  <span className="num">
                    {t("db.connections", {
                      used: server.connections,
                      max: server.max_connections || "–",
                    })}
                  </span>
                </span>
                <ProgressBar
                  value={connectionShare(server)}
                  label={t("db.connectionsOf", { name: server.name })}
                  warnAt={80}
                  errAt={95}
                />
                <span
                  className={`db-server-hint small${move && !moving ? " warn-text" : " muted"}`}
                >
                  {moving
                    ? t("db.movingHint", { name: moving.target_name })
                    : move
                      ? t("db.canMove", { name: move })
                      : (server.extensions ?? []).length > 0
                        ? t("db.keepDedicated")
                        : t(`db.serverState.${serverTone(server, container)}`)}
                </span>
              </button>
            );
          })}
          {candidates.map((item) => (
            <button
              key={item.docker_id}
              type="button"
              className="card db-server"
              aria-pressed={candidate?.docker_id === item.docker_id}
              onClick={() => setSelected(`candidate:${item.docker_id}`)}
            >
              <span className="db-server-head">
                <span className="mono db-server-name">{item.name}</span>
                <StatusPill tone="off" dot={false}>
                  {t("db.kind.dedicated")}
                </StatusPill>
              </span>
              <span className="db-server-line">
                {item.label} · <span className="mono">{item.host_name}</span>
              </span>
              <span className="db-server-line muted">
                {t("db.inStack", { name: item.project })}
              </span>
              <span className="db-server-hint small warn-text">
                {item.move_to ? t("db.canMove", { name: item.move_to }) : t("db.notLinked")}
              </span>
            </button>
          ))}
        </div>
      )}

      {candidate ? (
        <LinkCard
          key={candidate.docker_id}
          candidate={candidate}
          stacks={stacks.items}
          canAct={canAct}
        />
      ) : (
        current && (
          <div className="columns db-columns">
            <div className="column-wide">
              <DatabasesCard
                server={current}
                databases={serverDatabases}
                selected={database}
                stacks={stacks.items}
                onSelect={setSelectedDb}
              />
              {isAdmin && <DumpsCard server={current} />}
            </div>
            <div className="column-side">
              {database && (
                <ConnectionCard
                  server={current}
                  database={database}
                  user={
                    users.items.find(
                      (item) =>
                        item.server === current.id && (item.databases ?? []).includes(database.id),
                    ) ??
                    users.items.find(
                      (item) => item.server === current.id && item.name === database.name,
                    )
                  }
                  targets={moveTargets(current, servers.items)}
                  moving={moves.items.some(
                    (move) =>
                      move.status === "moving" &&
                      (move.databases ?? []).some((item) => item.name === database.name) &&
                      move.source_name === current.name,
                  )}
                  stacks={stacks.items}
                  canAct={canAct}
                  isAdmin={isAdmin}
                />
              )}
              <ServerCard
                server={current}
                hostName={hostName(current.host)}
                databases={serverDatabases.length}
                targets={current.kind === "dedicated" ? moveTargets(current, servers.items) : []}
                moves={moves.items.filter((move) =>
                  current.kind === "shared"
                    ? move.target === current.id && move.status === "moved"
                    : move.host === current.host && move.source_container === current.container_ref,
                )}
                stacks={stacks.items}
                canAct={canAct}
              />
            </div>
          </div>
        )
      )}
    </>
  );
}

function NewServerCard({ hosts, onDone }: { hosts: Host[]; onDone: (id: string) => void }) {
  const { t } = useTranslation();
  const toast = useToast();
  const [host, setHost] = useState(hosts[0]?.id ?? "");
  const [name, setName] = useState("");
  const [engine, setEngine] = useState<DbEngine>("mariadb");
  const [version, setVersion] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const versions = engines.find((item) => item.value === engine)?.versions ?? [];
  const chosen = versions.includes(version) ? version : versions[0];
  const nameError = name && !serverNamePattern.test(name) ? t("db.serverNameInvalid") : "";

  const create = async () => {
    setBusy(true);
    setError("");
    try {
      const answer = await dbApi.createServer({
        host: host || hosts[0]?.id,
        name,
        engine,
        version: chosen,
      });
      toast.success(t("db.serverQueued", { name }));
      onDone(answer.server.id);
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Card title={t("db.newServer")} note={t("db.newServerNote")}>
      <div className="form-grid">
        <Field label={t("db.serverName")} htmlFor="db-server-name" error={nameError}>
          <input
            id="db-server-name"
            className="input mono"
            value={name}
            placeholder="db-main"
            autoComplete="off"
            onChange={(event) => setName(event.target.value)}
          />
        </Field>
        <Field label={t("db.host")} htmlFor="db-server-host">
          <select
            id="db-server-host"
            className="select"
            style={{ width: "100%" }}
            value={host}
            onChange={(event) => setHost(event.target.value)}
          >
            {hosts.map((item) => (
              <option key={item.id} value={item.id}>
                {item.name}
              </option>
            ))}
          </select>
        </Field>
      </div>
      <div className="form-grid" style={{ marginTop: 12, alignItems: "end" }}>
        <SegmentedControl
          label={t("db.engine")}
          value={engine}
          onChange={setEngine}
          options={engines.map((item) => ({ value: item.value, label: item.label }))}
        />
        <Field label={t("db.version")} htmlFor="db-server-version">
          <select
            id="db-server-version"
            className="select"
            style={{ width: "100%" }}
            value={chosen}
            onChange={(event) => setVersion(event.target.value)}
          >
            {versions.map((item, index) => (
              <option key={item} value={item}>
                {index === 0 ? t("db.versionRecommended", { version: item }) : item}
              </option>
            ))}
          </select>
        </Field>
      </div>
      {error && (
        <p className="field-error" style={{ marginTop: 8 }}>
          {error}
        </p>
      )}
      <div style={{ display: "flex", gap: 8, marginTop: 14 }}>
        <Button
          variant="primary"
          disabled={busy || !name || Boolean(nameError) || hosts.length === 0}
          onClick={() => void create()}
        >
          {t("db.createServer")}
        </Button>
        <Button onClick={() => onDone("")}>{t("common.cancel")}</Button>
      </div>
    </Card>
  );
}

function NewDatabaseCard({
  servers,
  stacks,
  initialServer,
  hostName,
  onDone,
}: {
  servers: DbServer[];
  stacks: Stack[];
  initialServer: string;
  hostName: (id: string) => string;
  onDone: (server: string, id: string) => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const [server, setServer] = useState(initialServer || servers[0]?.id || "");
  const [name, setName] = useState("");
  const [stack, setStack] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const chosen = servers.find((item) => item.id === server);
  const options = stacks.filter(
    (item) => chosen && item.host === chosen.host && item.source !== "db_server",
  );
  const nameError = name && !databaseNamePattern.test(name) ? t("db.databaseNameInvalid") : "";

  const create = async () => {
    setBusy(true);
    setError("");
    try {
      const answer = await dbApi.createDatabase({ server, name, stack });
      toast.success(t("db.databaseQueued", { name }));
      onDone(server, answer.database.id);
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Card title={t("db.newDatabase")} note={t("db.newDatabaseNote")}>
      <div className="form-grid">
        <Field label={t("db.databaseName")} htmlFor="db-name" error={nameError}>
          <input
            id="db-name"
            className="input mono"
            value={name}
            placeholder="shop"
            autoComplete="off"
            onChange={(event) => setName(event.target.value)}
          />
        </Field>
        <Field label={t("db.server")} htmlFor="db-server">
          <select
            id="db-server"
            className="select"
            style={{ width: "100%" }}
            value={server}
            onChange={(event) => {
              setServer(event.target.value);
              setStack("");
            }}
          >
            {servers.map((item) => (
              <option key={item.id} value={item.id}>
                {item.name} · {hostName(item.host)}
              </option>
            ))}
          </select>
        </Field>
        <Field label={t("db.forStack")} htmlFor="db-stack">
          <select
            id="db-stack"
            className="select"
            style={{ width: "100%" }}
            value={stack}
            onChange={(event) => setStack(event.target.value)}
          >
            <option value="">{t("db.noApp")}</option>
            {options.map((item) => (
              <option key={item.id} value={item.id}>
                {item.name}
              </option>
            ))}
          </select>
        </Field>
      </div>
      {error && (
        <p className="field-error" style={{ marginTop: 8 }}>
          {error}
        </p>
      )}
      <div style={{ display: "flex", gap: 8, marginTop: 14 }}>
        <Button
          variant="primary"
          disabled={busy || !name || Boolean(nameError) || !server}
          onClick={() => void create()}
        >
          {t("db.createDatabase")}
        </Button>
        <Button onClick={() => onDone("", "")}>{t("common.cancel")}</Button>
      </div>
    </Card>
  );
}

function databaseTone(status: string): Tone {
  switch (status) {
    case "ready":
      return "ok";
    case "failed":
      return "err";
    case "dropping":
      return "warn";
    default:
      return "info";
  }
}

function DatabasesCard({
  server,
  databases,
  selected,
  stacks,
  onSelect,
}: {
  server: DbServer;
  databases: Database[];
  selected: Database | null;
  stacks: Stack[];
  onSelect: (id: string) => void;
}) {
  const { t } = useTranslation();
  const stackName = (id: string) => stacks.find((stack) => stack.id === id)?.name;

  return (
    <Card
      title={
        <>
          {t("db.databasesOn")} <span className="mono">{server.name}</span>
        </>
      }
      note={`${serverLabel(server)} · ${t(server.kind === "shared" ? "db.privateNetworks" : "db.stackNetwork")}`}
      flush
    >
      {databases.length === 0 ? (
        <EmptyState title={t("db.noDatabases")}>{t("db.noDatabasesText")}</EmptyState>
      ) : (
        <Table label={t("db.databases")}>
          <thead>
            <tr>
              <th>{t("db.database")}</th>
              <th>{t("db.app")}</th>
              <th>{t("db.size")}</th>
              <th>{t("db.connectionsShort")}</th>
              <th>{t("db.backup")}</th>
            </tr>
          </thead>
          <tbody>
            {databases.map((database) => {
              const app = database.stack ? stackName(database.stack) : undefined;
              return (
                <tr key={database.id} className={selected?.id === database.id ? "selected" : ""}>
                  <td>
                    <button
                      type="button"
                      className="link-button mono"
                      aria-pressed={selected?.id === database.id}
                      onClick={() => onSelect(database.id)}
                    >
                      {database.name}
                    </button>
                    {database.status !== "ready" && (
                      <>
                        {" "}
                        <StatusPill tone={databaseTone(database.status)}>
                          {t(`db.status.${database.status}`, database.status)}
                        </StatusPill>
                      </>
                    )}
                  </td>
                  <td className="muted">
                    {app ? (
                      <Link to={`/stacks/${database.stack}`}>
                        {t("db.stackApp", { name: app })}
                      </Link>
                    ) : (
                      "–"
                    )}
                  </td>
                  <td className="num">
                    {database.size_bytes ? formatBytes(database.size_bytes) : "–"}
                  </td>
                  <td className="num">{formatNumber(database.connections || 0)}</td>
                  <td className="muted">–</td>
                </tr>
              );
            })}
          </tbody>
        </Table>
      )}
    </Card>
  );
}

function ConnectionCard({
  server,
  database,
  user,
  targets,
  moving,
  stacks,
  canAct,
  isAdmin,
}: {
  server: DbServer;
  database: Database;
  user: DbUser | undefined;
  /** Shared servers this database could move to. */
  targets: DbServer[];
  moving: boolean;
  stacks: Stack[];
  canAct: boolean;
  isAdmin: boolean;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const now = useNow(30000);
  const [dropping, setDropping] = useState(false);
  const [cloning, setCloning] = useState(false);
  const [cloneName, setCloneName] = useState("");
  const [stack, setStack] = useState("");
  const [busy, setBusy] = useState("");
  const [moveOpen, setMoveOpen] = useState(false);
  const [target, setTarget] = useState("");

  const userName = user?.name ?? database.name;
  // the application keeps this password in its own settings
  const appPassword = Boolean(user?.app_password);
  const moveTo = targets.find((item) => item.id === target) ?? targets[0];
  const options = stacks.filter((item) => item.host === server.host && item.source !== "db_server");
  const ready = database.status === "ready";
  // a dedicated server's databases belong to their stack, with its own users
  const dedicated = server.kind === "dedicated";
  const owner = stacks.find((item) => item.id === database.stack)?.name ?? "–";

  const run = async (what: string, action: () => Promise<unknown>, message: string) => {
    setBusy(what);
    try {
      await action();
      toast.success(message);
    } catch (err) {
      toast.error(err);
    } finally {
      setBusy("");
    }
  };

  const openAdminer = async () => {
    setBusy("adminer");
    try {
      const session = await dbApi.adminer(database.id);
      // noopener: the tool's page gets no handle on the panel's window
      window.open(session.url, "_blank", "noopener");
      toast.success(t("db.adminerStarted"));
    } catch (err) {
      toast.error(err);
    } finally {
      setBusy("");
    }
  };

  return (
    <Card title={t("db.connection", { name: database.name })}>
      {database.last_error && <Banner tone="err">{database.last_error}</Banner>}
      {dedicated && (
        <dl className="facts">
          <dt>{t("db.hostLabel")}</dt>
          <dd className="mono">
            {server.name}:{server.port}
          </dd>
          <dt>{t("db.database")}</dt>
          <dd className="mono">{database.name}</dd>
          <dt>{t("db.credentials")}</dt>
          <dd>{t("db.ownCredentials", { name: owner })}</dd>
        </dl>
      )}
      {dedicated && isAdmin && ready && (
        <div className="button-row">
          <Button size="small" disabled={Boolean(busy)} onClick={() => void openAdminer()}>
            {t("db.adminer")}
          </Button>
          <Button
            size="small"
            disabled={Boolean(busy)}
            onClick={() =>
              void run("export", () => dbApi.export(database.id), t("db.exportQueued"))
            }
          >
            {t("db.export")}
          </Button>
        </div>
      )}
      {!dedicated && (
        <>
          <dl className="facts">
            <dt>{t("db.hostLabel")}</dt>
            <dd className="mono">
              {server.name}:{server.port}
            </dd>
            <dt>{t("db.user")}</dt>
            <dd className="mono">{userName}</dd>
            <dt>{t("db.password")}</dt>
            <dd>
              <span className="mono">••••••••</span> ·{" "}
              {appPassword ? t("db.appPassword") : t("db.appSecret")}
            </dd>
            <dt>{t("db.grants")}</dt>
            <dd>{t("db.grantsText", { name: database.name })}</dd>
            {user?.rotated_at && (
              <>
                <dt>{t("db.rotated")}</dt>
                <dd>{formatRelative(user.rotated_at, now, { past: true })}</dd>
              </>
            )}
          </dl>

          {appPassword ? (
            <p className="muted small" style={{ marginTop: 10 }}>
              {t("db.appPasswordNote")}
            </p>
          ) : (
            <>
              <div style={{ marginTop: 14 }}>
                <CodeBlock
                  code={envSnippet(server, database.name, userName)}
                  label={t("db.envSnippet")}
                />
              </div>
              <p className="muted small" style={{ marginTop: 10 }}>
                {t("db.rotateNote")}
              </p>
            </>
          )}

          {moving && <Banner tone="info">{t("db.movingDatabase")}</Banner>}

          {canAct && ready && !moving && (
            <div className="button-row">
              {!appPassword && (
                <Button
                  size="small"
                  disabled={Boolean(busy)}
                  onClick={() =>
                    void run("rotate", () => dbApi.rotate(database.id), t("db.rotateQueued"))
                  }
                >
                  {t("db.rotate")}
                </Button>
              )}
              {isAdmin && (
                <Button size="small" disabled={Boolean(busy)} onClick={() => void openAdminer()}>
                  {t("db.adminer")}
                </Button>
              )}
              {isAdmin && (
                <Button
                  size="small"
                  disabled={Boolean(busy)}
                  onClick={() =>
                    void run("export", () => dbApi.export(database.id), t("db.exportQueued"))
                  }
                >
                  {t("db.export")}
                </Button>
              )}
              <Button size="small" disabled={Boolean(busy)} onClick={() => setCloning(!cloning)}>
                {t("db.clone")}
              </Button>
              {database.stack && targets.length > 0 && (
                <Button size="small" disabled={Boolean(busy)} onClick={() => setMoveOpen(true)}>
                  {t("db.moveDatabase")}
                </Button>
              )}
            </div>
          )}

          {moveTo && (
            <ConfirmDialog
              open={moveOpen}
              title={t("db.moveDatabaseConfirm", { name: database.name })}
              confirmLabel={t("db.moveDatabase")}
              onCancel={() => setMoveOpen(false)}
              onConfirm={() => {
                setMoveOpen(false);
                void run(
                  "move",
                  () => dbApi.move(server.id, { target: moveTo.id, database: database.name }),
                  t("db.moveQueued", { name: moveTo.name }),
                );
              }}
            >
              <MoveTargetField
                id="db-move-database-target"
                targets={targets}
                value={moveTo.id}
                onChange={setTarget}
              />
              <p className="muted">
                {t("db.moveDatabaseText", {
                  stack: stacks.find((item) => item.id === database.stack)?.name ?? "–",
                  name: moveTo.name,
                })}
              </p>
            </ConfirmDialog>
          )}

          {canAct && ready && cloning && (
            <div className="inline-form">
              <Field
                label={t("db.cloneName")}
                htmlFor="db-clone-name"
                error={
                  cloneName && !databaseNamePattern.test(cloneName)
                    ? t("db.databaseNameInvalid")
                    : ""
                }
              >
                <input
                  id="db-clone-name"
                  className="input mono"
                  value={cloneName}
                  placeholder={`${database.name}_staging`}
                  autoComplete="off"
                  onChange={(event) => setCloneName(event.target.value)}
                />
              </Field>
              <Button
                size="small"
                variant="primary"
                disabled={Boolean(busy) || !databaseNamePattern.test(cloneName)}
                onClick={() =>
                  void run(
                    "clone",
                    () => dbApi.clone(database.id, cloneName),
                    t("db.cloneQueued", { name: cloneName }),
                  ).then(() => setCloning(false))
                }
              >
                {t("db.cloneStart")}
              </Button>
            </div>
          )}

          {canAct && ready && !database.stack && options.length > 0 && (
            <div className="inline-form">
              <Field label={t("db.connectTo")} htmlFor="db-connect-stack">
                <select
                  id="db-connect-stack"
                  className="select"
                  style={{ width: "100%" }}
                  value={stack}
                  onChange={(event) => setStack(event.target.value)}
                >
                  <option value="">{t("db.chooseStack")}</option>
                  {options.map((item) => (
                    <option key={item.id} value={item.id}>
                      {item.name}
                    </option>
                  ))}
                </select>
              </Field>
              <Button
                size="small"
                disabled={Boolean(busy) || !stack}
                onClick={() =>
                  void run(
                    "connect",
                    () => dbApi.connect(database.id, stack),
                    t("db.connectQueued"),
                  )
                }
              >
                {t("db.connect")}
              </Button>
            </div>
          )}

          {canAct && database.status !== "dropping" && (
            <div style={{ marginTop: 14 }}>
              <Button size="small" variant="danger" onClick={() => setDropping(true)}>
                {t("db.drop")}
              </Button>
            </div>
          )}

          <ConfirmDialog
            open={dropping}
            danger
            title={t("db.dropConfirm", { name: database.name })}
            confirmLabel={t("db.drop")}
            onCancel={() => setDropping(false)}
            onConfirm={() => {
              setDropping(false);
              void run(
                "drop",
                () => dbApi.drop(database.id),
                t("db.dropQueued", { name: database.name }),
              );
            }}
          >
            <p className="muted">{t("db.dropNote")}</p>
          </ConfirmDialog>
        </>
      )}
    </Card>
  );
}

/** The shared servers to choose from, when there is more than one. */
function MoveTargetField({
  id,
  targets,
  value,
  onChange,
}: {
  id: string;
  targets: DbServer[];
  value: string;
  onChange: (id: string) => void;
}) {
  const { t } = useTranslation();
  if (targets.length < 2) {
    return null;
  }
  return (
    <Field label={t("db.moveTarget")} htmlFor={id}>
      <select
        id={id}
        className="select"
        style={{ width: "100%" }}
        value={value}
        onChange={(event) => onChange(event.target.value)}
      >
        {targets.map((item) => (
          <option key={item.id} value={item.id}>
            {item.name} · {serverLabel(item)}
          </option>
        ))}
      </select>
    </Field>
  );
}

function ServerCard({
  server,
  hostName,
  databases,
  targets,
  moves,
  stacks,
  canAct,
}: {
  server: DbServer;
  hostName: string;
  databases: number;
  /** Shared servers a dedicated server's databases could move to. */
  targets: DbServer[];
  /** Moves out of a dedicated server, or into a shared one. */
  moves: DbMove[];
  stacks: Stack[];
  canAct: boolean;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const [removing, setRemoving] = useState(false);
  const [removeVolume, setRemoveVolume] = useState(false);
  const [moveOpen, setMoveOpen] = useState(false);
  const [target, setTarget] = useState("");
  const [updating, setUpdating] = useState(false);

  const move = targets.length > 0 ? targets[0].name : "";
  // a newer major version of the engine is a new server to move to
  const newer = (engines.find((item) => item.value === server.engine)?.versions ?? []).filter(
    (version) => Number.parseInt(version, 10) > majorOf(server),
  );
  const moveTo = targets.find((item) => item.id === target) ?? targets[0];
  const latest = server.kind === "dedicated" ? moves[0] : undefined;
  const moving = latest?.status === "moving";
  const stackName = stacks.find((item) => item.id === server.stack)?.name ?? "–";

  const update = async () => {
    setUpdating(false);
    try {
      await dbApi.updateServer(server.id);
      toast.success(t("db.updateQueued", { name: server.name }));
    } catch (err) {
      toast.error(err);
    }
  };

  const startMove = async () => {
    setMoveOpen(false);
    if (!moveTo) {
      return;
    }
    try {
      await dbApi.move(server.id, { target: moveTo.id });
      toast.success(t("db.moveQueued", { name: moveTo.name }));
    } catch (err) {
      toast.error(err);
    }
  };

  const refresh = async () => {
    try {
      await dbApi.stats(server.id);
      toast.success(t("db.statsQueued"));
    } catch (err) {
      toast.error(err);
    }
  };

  const remove = async () => {
    setRemoving(false);
    try {
      await dbApi.removeServer(server.id, removeVolume);
      toast.success(t("db.serverRemoved", { name: server.name }));
    } catch (err) {
      toast.error(err);
    }
  };

  let title = t("db.sharedTitle");
  let text = t("db.sharedText", { host: hostName });
  if (server.kind === "dedicated") {
    title = move ? t("db.moveTitle") : t("db.dedicatedTitle");
    text = move
      ? t("db.moveText", { name: move })
      : (server.extensions ?? []).length > 0
        ? t("db.keepText", { extensions: (server.extensions ?? []).join(", ") })
        : t("db.dedicatedText");
  }

  return (
    <Card
      title={title}
      side={
        canAct && (
          <IconButton aria-label={t("db.refreshStats")} onClick={() => void refresh()}>
            <RefreshCw size={16} aria-hidden="true" />
          </IconButton>
        )
      }
    >
      {server.last_error && <Banner tone="warn">{server.last_error}</Banner>}
      {moving && <Banner tone="info">{t("db.moving", { name: latest.target_name })}</Banner>}
      {latest?.status === "failed" && (
        <Banner tone="warn">
          {t("db.moveFailed", { name: latest.target_name, error: latest.error })}
        </Banner>
      )}
      <p className="muted">{text}</p>
      {server.kind === "dedicated" && move && !moving && (
        <>
          <p className="faint small">{t("db.moveKeep")}</p>
          {canAct && (
            <div style={{ marginTop: 14 }}>
              <Button size="small" variant="primary" onClick={() => setMoveOpen(true)}>
                {t("db.moveStart")}
              </Button>
            </div>
          )}
          {moveTo && (
            <ConfirmDialog
              open={moveOpen}
              title={t("db.moveConfirm", { name: server.name })}
              confirmLabel={t("db.moveStart")}
              onCancel={() => setMoveOpen(false)}
              onConfirm={() => void startMove()}
            >
              <MoveTargetField
                id="db-move-target"
                targets={targets}
                value={moveTo.id}
                onChange={setTarget}
              />
              <p className="muted">
                {t("db.moveConfirmText", {
                  stack: stackName,
                  name: moveTo.name,
                  service: server.service || server.container_ref,
                })}
              </p>
            </ConfirmDialog>
          )}
        </>
      )}
      {server.kind === "shared" && moves.length > 0 && (
        <>
          <h3 className="db-moves-title">{t("db.movedHere")}</h3>
          <ul className="db-moves small">
            {moves.map((item) => {
              // servers add their build to the number: "16.15 (Debian 16.15-1…)"
              const version = item.source_version.match(/^\d+(\.\d+)*/)?.[0];
              const names = (item.databases ?? []).map((entry) => entry.name).join(", ");
              return (
                <li key={item.id}>
                  <span className="mono">{item.source_name}</span>
                  {version && ` · ${engineLabel(server.engine)} ${version}`} · {names}
                  <div className="cell-sub">
                    {item.source_kind !== "dedicated"
                      ? formatDate(item.moved_at)
                      : item.removed_at
                        ? t("db.oldRemoved", { date: formatDate(item.moved_at) })
                        : t("db.oldKept", { date: formatDate(item.remove_after) })}
                  </div>
                </li>
              );
            })}
          </ul>
        </>
      )}
      {server.stack && (
        <p className="small" style={{ marginTop: 10 }}>
          <Link to={`/stacks/${server.stack}`}>{t("db.openStack")}</Link>
        </p>
      )}
      {server.kind === "shared" && server.engine === "postgres" && newer.length > 0 && (
        <p className="faint small" style={{ marginTop: 10 }}>
          {t("db.majorNote", { version: newer[newer.length - 1] })}
        </p>
      )}
      {canAct && server.kind === "shared" && (
        <div className="button-row">
          <Button
            size="small"
            disabled={server.status === "creating"}
            onClick={() => setUpdating(true)}
          >
            {t("db.update")}
          </Button>
          {databases === 0 && (
            <Button size="small" variant="danger" onClick={() => setRemoving(true)}>
              {t("db.removeServer")}
            </Button>
          )}
          <ConfirmDialog
            open={updating}
            title={t("db.updateConfirm", { name: server.name })}
            confirmLabel={t("db.update")}
            onCancel={() => setUpdating(false)}
            onConfirm={() => void update()}
          >
            <p className="muted">
              {t(server.engine === "mariadb" ? "db.updateTextMariadb" : "db.updateText", {
                image: server.image,
              })}
            </p>
          </ConfirmDialog>
          <ConfirmDialog
            open={removing}
            danger
            title={t("db.removeServerConfirm", { name: server.name })}
            confirmLabel={t("db.removeServer")}
            onCancel={() => setRemoving(false)}
            onConfirm={() => void remove()}
          >
            <label style={{ display: "flex", gap: 8, alignItems: "center" }}>
              <input
                type="checkbox"
                checked={removeVolume}
                onChange={(event) => setRemoveVolume(event.target.checked)}
              />
              {t("db.removeVolume", { volume: server.volume })}
            </label>
          </ConfirmDialog>
        </div>
      )}
    </Card>
  );
}

/** A dedicated server found in a stack becomes known with its administrator's credentials. */
function LinkCard({
  candidate,
  stacks,
  canAct,
}: {
  candidate: DbCandidate;
  stacks: Stack[];
  canAct: boolean;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const stack = stacks.find((item) => item.id === candidate.stack);
  // the variable the Compose file takes the password from comes first, secret or not
  const keys = useMemo(() => {
    const list = [...(stack?.secret_keys ?? [])];
    if (candidate.password_key && !list.includes(candidate.password_key)) {
      list.unshift(candidate.password_key);
    }
    return list;
  }, [stack, candidate.password_key]);

  const [adminUser, setAdminUser] = useState(candidate.admin_user);
  const [secretKey, setSecretKey] = useState(candidate.password_key || keys[0] || "");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);

  const link = async () => {
    setBusy(true);
    try {
      await dbApi.linkServer({
        host: candidate.host,
        docker_id: candidate.docker_id,
        admin_user: adminUser,
        secret_key: secretKey || undefined,
        password: secretKey ? undefined : password,
      });
      toast.success(t("db.linked", { name: candidate.name }));
    } catch (err) {
      toast.error(err);
      setBusy(false);
    }
  };

  return (
    <Card title={t("db.linkTitle", { name: candidate.name })} note={candidate.label}>
      <p className="muted">
        {candidate.extensions.length > 0
          ? t("db.keepText", { extensions: candidate.extensions.join(", ") })
          : t("db.linkText", { stack: candidate.project })}
      </p>
      {canAct && (
        <>
          <div className="form-grid" style={{ marginTop: 12 }}>
            <Field label={t("db.adminUser")} htmlFor="db-link-user">
              <input
                id="db-link-user"
                className="input mono"
                value={adminUser}
                autoComplete="off"
                onChange={(event) => setAdminUser(event.target.value)}
              />
            </Field>
            {keys.length > 0 ? (
              <Field label={t("db.passwordFrom")} htmlFor="db-link-secret">
                <select
                  id="db-link-secret"
                  className="select"
                  style={{ width: "100%" }}
                  value={secretKey}
                  onChange={(event) => setSecretKey(event.target.value)}
                >
                  {keys.map((key) => (
                    <option key={key} value={key}>
                      {key}
                    </option>
                  ))}
                  <option value="">{t("db.typePassword")}</option>
                </select>
              </Field>
            ) : null}
            {(!secretKey || keys.length === 0) && (
              <Field label={t("db.adminPassword")} htmlFor="db-link-password">
                <input
                  id="db-link-password"
                  className="input mono"
                  type="password"
                  value={password}
                  autoComplete="off"
                  onChange={(event) => setPassword(event.target.value)}
                />
              </Field>
            )}
          </div>
          <div style={{ marginTop: 14 }}>
            <Button
              variant="primary"
              disabled={busy || !adminUser || (!secretKey && !password)}
              onClick={() => void link()}
            >
              {t("db.link")}
            </Button>
          </div>
        </>
      )}
    </Card>
  );
}

/** SQL exports and final dumps of a server, which hold every row: for admins only. */
function DumpsCard({ server }: { server: DbServer }) {
  const { t } = useTranslation();
  const toast = useToast();
  const now = useNow(60000);
  const dumps = useLiveList<DbDump>("db_dumps", {
    filter: pb.filter("server = {:server}", { server: server.id }),
    sort: "-created",
    perPage: 20,
  });

  if (dumps.items.length === 0) {
    return null;
  }

  const download = async (dump: DbDump) => {
    try {
      const token = await pb.files.getToken();
      window.location.href = pb.files.getURL(dump, dump.file, { token, download: true });
    } catch (err) {
      toast.error(err);
    }
  };

  return (
    <Card title={t("db.dumps")} note={t("db.dumpsNote")} flush>
      <ul className="list">
        {dumps.items.map((dump) => (
          <li key={dump.id} className="list-row">
            <div>
              <div>
                <DatabaseIcon size={14} aria-hidden="true" />{" "}
                <span className="mono">{dump.database_name}</span> · {t(`db.dumpKind.${dump.kind}`)}
              </div>
              <div className="cell-sub">
                {formatBytes(dump.size_bytes)} ·{" "}
                {t("db.expires", { when: formatRelative(dump.expires_at, now) })}
              </div>
            </div>
            <IconButton
              aria-label={t("db.downloadDump", { name: dump.database_name })}
              onClick={() => void download(dump)}
            >
              <Download size={16} aria-hidden="true" />
            </IconButton>
          </li>
        ))}
      </ul>
    </Card>
  );
}
