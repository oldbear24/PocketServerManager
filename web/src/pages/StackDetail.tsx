import { ArrowUp, RotateCcw, Trash2 } from "lucide-react";
import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useNavigate, useParams } from "react-router-dom";

import { CommandOutput } from "../components/CommandOutput";
import { PageHead } from "../components/Layout";
import { LogViewer } from "../components/LogViewer";
import { TaskList } from "../components/TaskList";
import {
  LabelDomainsCard,
  ServiceDomains,
  useDomainRoutes,
  type DomainRoutes,
} from "../components/StackDomains";
import { CommandStatusPill, ContainerStatePill } from "../components/status";
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
} from "../components/ui";
import { api, errorMessage } from "../lib/api";
import { lineDiff } from "../lib/audit";
import { hasRole, useAuth } from "../lib/auth";
import {
  formatBytes,
  formatDateTime,
  formatDuration,
  formatNumber,
  parseDate,
} from "../lib/format";
import { useContainerValues } from "../lib/metrics";
import { pb } from "../lib/pb";
import { useLiveList, useLiveRecord, useNow } from "../lib/realtime";
import {
  addedLines,
  diffStats,
  envEntries,
  rejectionOf,
  secretNamePattern,
  stackApi,
  type StackProblem,
  type StackValidation,
} from "../lib/stacks";
import type { Domain } from "../lib/edge";
import type { Command, Container, Host, Stack, StackRevision } from "../lib/types";

const CodeEditor = lazy(() => import("../components/CodeEditor"));

const validationDelay = 400;

function RevisionDiff({ before, after }: { before: string; after: string }) {
  const lines = lineDiff(before, after);

  return (
    <div className="diff" tabIndex={0} role="region" style={{ maxHeight: 360, overflow: "auto" }}>
      {lines.map((line, index) => (
        <div key={index} className={`diff-line ${line.kind === "same" ? "" : line.kind}`}>
          <span aria-hidden="true">
            {line.kind === "add" ? "+" : line.kind === "del" ? "−" : " "}
          </span>
          <span>{line.text}</span>
        </div>
      ))}
    </div>
  );
}

function ServiceCards({
  host,
  containers,
  domains,
  routes,
}: {
  host: string;
  containers: Container[];
  domains: Domain[];
  routes: DomainRoutes;
}) {
  const { t } = useTranslation();
  const now = useNow(30000);
  const values = useContainerValues(host).data ?? {};

  if (containers.length === 0) {
    return null;
  }

  return (
    <div className="service-cards">
      {containers.map((container) => {
        const usage = values[container.docker_id];
        const started = parseDate(container.started_at);
        const ports = (container.ports ?? []).filter((port) => port.public);

        return (
          <div key={container.id} className="card service-card">
            <div className="service-head">
              <Link
                className="cell-title"
                to={`/servers/${host}/containers/${container.docker_id}`}
              >
                {container.service || container.name}
              </Link>
              <ContainerStatePill state={container.state} health={container.health} />
              {started && container.state === "running" && (
                <span className="muted small" style={{ marginLeft: "auto" }}>
                  {t("stack.uptime", { value: formatDuration((now - started.getTime()) / 1000) })}
                </span>
              )}
            </div>
            <div className="mono muted small">{container.image}</div>
            <div className="service-stats">
              <span>
                CPU{" "}
                <strong className="num">{usage ? `${formatNumber(usage.cpu, 1)} %` : "–"}</strong>
              </span>
              <span>
                RAM <strong className="num">{usage ? formatBytes(usage.mem) : "–"}</strong>
              </span>
              {ports.length > 0 && (
                <span>
                  {t("stack.ports")}{" "}
                  <strong className="num">{ports.map((port) => port.public).join(", ")}</strong>
                </span>
              )}
            </div>
            <ServiceDomains
              domains={domains.filter(
                (domain) => domain.service === container.service && !domain.label_removed,
              )}
              routes={routes}
            />
          </div>
        );
      })}
    </div>
  );
}

function SecretsCard({ stack, canAct }: { stack: Stack; canAct: boolean }) {
  const { t } = useTranslation();
  const toast = useToast();
  const [name, setName] = useState("");
  const [value, setValue] = useState("");

  const secrets = stack.secret_keys ?? [];
  const plain = envEntries(stack.env);

  const add = async () => {
    try {
      await stackApi.save(stack.id, { secrets: { set: { [name]: value } } });
      setName("");
      setValue("");
      toast.success(t("stack.secretSaved"));
    } catch (err) {
      toast.error(err);
    }
  };

  const remove = async (key: string) => {
    try {
      await stackApi.save(stack.id, { secrets: { remove: [key] } });
    } catch (err) {
      toast.error(err);
    }
  };

  const nameInvalid = name !== "" && !secretNamePattern.test(name);

  return (
    <Card title={t("stack.variables")}>
      <div className="secret-list">
        {secrets.map((key) => (
          <div key={key} className="secret-row">
            <span className="mono">{key}</span>
            <span className="muted" aria-label={t("stack.hiddenValue")}>
              ••••••••••
            </span>
            <StatusPill tone="info" dot={false}>
              {t("stack.secret")}
            </StatusPill>
            {canAct && (
              <IconButton
                bare
                aria-label={t("stack.removeSecret", { name: key })}
                onClick={() => void remove(key)}
              >
                <Trash2 size={14} aria-hidden="true" />
              </IconButton>
            )}
          </div>
        ))}
        {plain.map((entry) => (
          <div key={entry.key} className="secret-row">
            <span className="mono">{entry.key}</span>
            <span className="mono muted secret-value" title={entry.value}>
              {entry.value}
            </span>
          </div>
        ))}
        {secrets.length === 0 && plain.length === 0 && (
          <p className="muted">{t("stack.noVariables")}</p>
        )}
      </div>

      <p className="muted small" style={{ marginTop: 12 }}>
        {t("stack.secretsNote")}
      </p>

      {canAct && (
        <div className="secret-form">
          <Field
            label={t("stack.secretName")}
            htmlFor="secret-name"
            error={nameInvalid ? t("stack.secretNameInvalid") : undefined}
          >
            <input
              id="secret-name"
              className="input mono"
              autoComplete="off"
              value={name}
              onChange={(event) => setName(event.target.value)}
            />
          </Field>
          <Field label={t("stack.secretValue")} htmlFor="secret-value">
            <input
              id="secret-value"
              className="input mono"
              type="password"
              autoComplete="new-password"
              value={value}
              onChange={(event) => setValue(event.target.value)}
            />
          </Field>
          <Button disabled={!name || !value || nameInvalid} onClick={() => void add()}>
            {t("stack.addSecret")}
          </Button>
        </div>
      )}
    </Card>
  );
}

function RevisionsCard({
  stack,
  revisions,
  draftChanged,
  draftStats,
  canAct,
  onRollback,
}: {
  stack: Stack;
  revisions: StackRevision[];
  draftChanged: boolean;
  draftStats: { added: number; removed: number };
  canAct: boolean;
  onRollback: (revision: StackRevision) => void;
}) {
  const { t } = useTranslation();
  const [comparing, setComparing] = useState<StackRevision | null>(null);

  const previousOf = (revision: StackRevision) =>
    revisions.find((other) => other.number === revision.number - 1);

  return (
    <Card title={t("stack.revisions")} flush>
      <ul className="list revision-list">
        {draftChanged && (
          <li>
            <div className="revision-row">
              <span className="mono warn-text">r{stack.current_revision + 1}</span>
              <div className="revision-text">
                <div>{t("stack.draft")}</div>
                <div className="cell-sub">{t("stack.draftNote")}</div>
              </div>
              <span className="num muted small">
                +{draftStats.added} −{draftStats.removed}
              </span>
            </div>
          </li>
        )}
        {revisions.map((revision) => {
          const previous = previousOf(revision);
          const stats = diffStats(previous?.compose_yaml ?? "", revision.compose_yaml);
          const deployed = revision.number === stack.deployed_revision;

          return (
            <li key={revision.id}>
              <div className="revision-row">
                <span className={`mono${deployed ? " ok-text" : ""}`}>r{revision.number}</span>
                <div className="revision-text">
                  <div>{revision.note || t("stack.noNote")}</div>
                  <div className="cell-sub">
                    {deployed
                      ? t("stack.deployedOn", { date: formatDateTime(revision.deployed_at) })
                      : formatDateTime(revision.created)}
                    {revision.expand?.author?.email && ` · ${revision.expand.author.email}`}
                  </div>
                  <div className="revision-actions">
                    <button
                      type="button"
                      className="link-button"
                      aria-expanded={comparing?.id === revision.id}
                      onClick={() => setComparing(comparing?.id === revision.id ? null : revision)}
                    >
                      {t("stack.compare")}
                    </button>
                    {canAct && !deployed && (
                      <button
                        type="button"
                        className="link-button"
                        onClick={() => onRollback(revision)}
                      >
                        <RotateCcw size={12} aria-hidden="true" /> {t("stack.rollback")}
                      </button>
                    )}
                  </div>
                </div>
                <span className="num muted small">
                  +{stats.added} −{stats.removed}
                </span>
              </div>
              {comparing?.id === revision.id && (
                <div style={{ marginTop: 10 }}>
                  <RevisionDiff
                    before={previous?.compose_yaml ?? ""}
                    after={revision.compose_yaml}
                  />
                </div>
              )}
            </li>
          );
        })}
        {revisions.length === 0 && !draftChanged && (
          <li className="muted">{t("stack.noRevisions")}</li>
        )}
      </ul>
    </Card>
  );
}

export function StackDetailPage() {
  const { t } = useTranslation();
  const { id = "" } = useParams();
  const { user } = useAuth();
  const toast = useToast();
  const navigate = useNavigate();

  const { record: stack, loading, error } = useLiveRecord<Stack>("stacks", id);
  const { record: host } = useLiveRecord<Host>("hosts", stack?.host);

  const revisions = useLiveList<StackRevision>("stack_revisions", {
    filter: pb.filter("stack = {:stack}", { stack: id }),
    sort: "-number",
    perPage: 50,
    expand: "author",
  });

  const containers = useLiveList<Container>("containers", {
    filter: stack
      ? pb.filter("host = {:host} && project = {:project}", {
          host: stack.host,
          project: stack.name,
        })
      : undefined,
    sort: "service,name",
    enabled: Boolean(stack),
  });

  const domains = useLiveList<Domain>("domains", {
    filter: pb.filter("stack = {:stack}", { stack: id }),
    sort: "hostname",
  });
  const routes = useDomainRoutes(
    stack?.host,
    [host?.edge_digest, host?.edge_applied_digest].join("|"),
    domains.items,
  );

  const lastCommand = useLiveList<Command>("commands", {
    filter: stack
      ? pb.filter("host = {:host} && resource = {:resource}", {
          host: stack.host,
          resource: `stack:${stack.name}`,
        })
      : undefined,
    sort: "-created",
    perPage: 1,
    enabled: Boolean(stack),
  });

  const canAct = hasRole(user, "operator");

  const [tab, setTab] = useState<"compose" | "env">("compose");
  const [compose, setCompose] = useState<string | null>(null);
  const [env, setEnv] = useState<string | null>(null);
  const [note, setNote] = useState("");
  const [validation, setValidation] = useState<StackValidation | null>(null);
  const [busy, setBusy] = useState(false);
  const [watching, setWatching] = useState<string | null>(null);
  const [rollingBack, setRollingBack] = useState<StackRevision | null>(null);
  const [confirmingDown, setConfirmingDown] = useState(false);
  const [removeVolumes, setRemoveVolumes] = useState(false);
  const [confirmingDelete, setConfirmingDelete] = useState(false);

  const savedCompose = stack?.compose_yaml ?? "";
  const savedEnv = stack?.env ?? "";

  // what the editor shows: the saved files until someone starts typing
  const draftCompose = compose ?? savedCompose;
  const draftEnv = env ?? savedEnv;
  const dirty = Boolean(stack) && (draftCompose !== savedCompose || draftEnv !== savedEnv);

  const name = stack?.name ?? "";
  const secretKeys = useMemo(() => stack?.secret_keys ?? [], [stack?.secret_keys]);
  const secretKeysKey = secretKeys.join(",");

  const generation = useRef(0);

  useEffect(() => {
    if (!stack) {
      return;
    }

    const current = ++generation.current;

    const timer = setTimeout(() => {
      stackApi
        .validate({ name, compose_yaml: draftCompose, env: draftEnv, secret_keys: secretKeys })
        .then((result) => {
          if (current === generation.current) {
            setValidation(result);
          }
        })
        .catch(() => {
          // a viewer may not validate, and a lost answer changes nothing: the deploy checks again
        });
    }, validationDelay);

    return () => clearTimeout(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draftCompose, draftEnv, secretKeysKey, name, Boolean(stack)]);

  useEffect(() => {
    if (!dirty) {
      return;
    }

    const warn = (event: BeforeUnloadEvent) => event.preventDefault();
    window.addEventListener("beforeunload", warn);

    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty]);

  const composeProblems = useMemo(
    () => (validation?.errors ?? []).filter((problem) => problem.file !== ".env"),
    [validation],
  );
  const envProblems = useMemo(
    () => (validation?.errors ?? []).filter((problem) => problem.file === ".env"),
    [validation],
  );

  const added = useMemo(() => addedLines(savedCompose, draftCompose), [savedCompose, draftCompose]);
  const addedEnv = useMemo(() => addedLines(savedEnv, draftEnv), [savedEnv, draftEnv]);
  const stats = useMemo(() => diffStats(savedCompose, draftCompose), [savedCompose, draftCompose]);

  const onComposeChange = useCallback((value: string) => setCompose(value), []);
  const onEnvChange = useCallback((value: string) => setEnv(value), []);

  if (loading) {
    return null;
  }

  if (error || !stack) {
    return <EmptyState title={t("stack.notFound")} />;
  }

  const clearDraft = () => {
    setCompose(null);
    setEnv(null);
    setNote("");
  };

  const edit = () => ({
    ...(draftCompose !== savedCompose ? { compose_yaml: draftCompose } : {}),
    ...(draftEnv !== savedEnv ? { env: draftEnv } : {}),
    ...(note ? { note } : {}),
  });

  const save = async () => {
    setBusy(true);
    try {
      await stackApi.save(stack.id, edit());
      clearDraft();
      toast.success(t("stack.saved"));
    } catch (err) {
      toast.error(err);
    } finally {
      setBusy(false);
    }
  };

  const deploy = async (pull = false) => {
    setBusy(true);
    try {
      const answer = await stackApi.deploy(stack.id, { ...edit(), pull });
      clearDraft();
      setWatching(answer.command.id);
    } catch (err) {
      const rejected = rejectionOf(err);
      if (rejected) {
        setValidation(rejected);
      }
      toast.error(err);
    } finally {
      setBusy(false);
    }
  };

  const rollback = async (revision: StackRevision) => {
    setRollingBack(null);
    setBusy(true);
    try {
      const answer = await stackApi.rollback(stack.id, revision.number);
      clearDraft();
      setWatching(answer.command.id);
    } catch (err) {
      toast.error(err);
    } finally {
      setBusy(false);
    }
  };

  const action = async (type: string, payload: object = {}) => {
    try {
      const command = await api.runCommand(
        stack.host,
        type,
        { name: stack.name, ...payload },
        stack.name,
      );
      setWatching(command.id);
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  const remove = async () => {
    setConfirmingDelete(false);
    try {
      await pb.collection("stacks").delete(stack.id);
      void navigate("/stacks");
    } catch (err) {
      toast.error(err);
    }
  };

  const running = containers.items.filter((container) => container.state === "running").length;

  const nextRevision = stack.current_revision + (dirty ? 1 : 0);
  const invalid = validation !== null && !validation.valid;
  const problems: StackProblem[] = tab === "compose" ? composeProblems : envProblems;

  const services = [
    ...new Set(containers.items.map((container) => container.service).filter(Boolean)),
  ];

  const status = lastCommand.items[0];

  return (
    <>
      <PageHead
        crumbs={
          <>
            <Link to="/stacks">{t("nav.stacks")}</Link> /{" "}
            {host && <Link to={`/servers/${host.id}`}>{host.name}</Link>} / {stack.name}
          </>
        }
        title={<span className="mono">{stack.name}</span>}
        titleExtra={
          <>
            <StatusPill
              tone={
                containers.items.length === 0
                  ? "off"
                  : running === containers.items.length
                    ? "ok"
                    : "warn"
              }
            >
              {t("stacks.running", { running, total: containers.items.length })}
            </StatusPill>
            {dirty && (
              <StatusPill tone="warn" dot={false}>
                {t("stack.unsaved")}
              </StatusPill>
            )}
            {stack.status === "failed" && <StatusPill tone="err">{t("stack.failed")}</StatusPill>}
          </>
        }
        description={[
          "Docker Compose",
          stack.deployed_revision
            ? t("stack.deployedLine", {
                revision: stack.deployed_revision,
                date: formatDateTime(stack.deployed_at),
              })
            : t("stack.notDeployed"),
          t(`stack.source.${stack.source || "editor"}`),
        ].join(" · ")}
        actions={
          canAct && (
            <>
              <Button disabled={!stack.deployed_revision} onClick={() => void action("stack.pull")}>
                {t("stack.pull")}
              </Button>
              <Button disabled={running === 0} onClick={() => void action("stack.stop")}>
                {t("stack.stop")}
              </Button>
              <Button
                disabled={containers.items.length === 0}
                onClick={() => {
                  setRemoveVolumes(false);
                  setConfirmingDown(true);
                }}
              >
                {t("stack.down")}
              </Button>
              <Button
                variant="primary"
                disabled={busy || invalid || (!dirty && stack.current_revision === 0)}
                onClick={() => void deploy()}
              >
                <ArrowUp size={16} aria-hidden="true" />
                {t("stack.deploy", { revision: nextRevision })}
              </Button>
            </>
          )
        }
      />

      <ServiceCards
        host={stack.host}
        containers={containers.items}
        domains={domains.items}
        routes={routes}
      />
      <LabelDomainsCard stack={stack} domains={domains.items} canAct={canAct} />

      {watching && (
        <CommandOutput
          key={watching}
          commandId={watching}
          title={status?.id === watching ? status.type : t("stack.output")}
          status={status?.id === watching ? <CommandStatusPill status={status.status} /> : null}
          progress={status?.id === watching ? status.progress?.message : undefined}
          onClose={() => setWatching(null)}
        />
      )}

      <div className="columns">
        <div className="column-wide">
          <section className="card">
            <div className="editor-head">
              <div className="tabs" role="tablist" aria-label={t("stack.files")}>
                {(["compose", "env"] as const).map((item) => (
                  <button
                    key={item}
                    type="button"
                    role="tab"
                    aria-selected={tab === item}
                    className={`tab mono${tab === item ? " active" : ""}`}
                    onClick={() => setTab(item)}
                  >
                    {item === "compose" ? "compose.yaml" : ".env"}
                  </button>
                ))}
              </div>
              <span className="muted small" role="status">
                {validation === null
                  ? t("stack.checking")
                  : validation.valid
                    ? t("stack.valid", { count: draftCompose.split("\n").length })
                    : t("stack.invalid", { count: validation.errors.length })}
              </span>
            </div>

            <Suspense fallback={<div className="log-empty">{t("stack.editorLoading")}</div>}>
              {tab === "compose" ? (
                <CodeEditor
                  value={draftCompose}
                  onChange={onComposeChange}
                  label="compose.yaml"
                  language="yaml"
                  problems={composeProblems}
                  added={added}
                  readOnly={!canAct}
                />
              ) : (
                <CodeEditor
                  value={draftEnv}
                  onChange={onEnvChange}
                  label=".env"
                  language="env"
                  problems={envProblems}
                  added={addedEnv}
                  readOnly={!canAct}
                />
              )}
            </Suspense>

            {problems.length > 0 && (
              <ul className="problem-list">
                {problems.map((problem, index) => (
                  <li key={index} className="err-text">
                    {problem.line ? (
                      <strong className="mono">{t("stack.line", { line: problem.line })} </strong>
                    ) : null}
                    {problem.message}
                  </li>
                ))}
              </ul>
            )}
            {(validation?.warnings ?? []).length > 0 && (
              <ul className="problem-list">
                {validation?.warnings.map((problem, index) => (
                  <li key={index} className="warn-text">
                    {problem.line ? (
                      <strong className="mono">{t("stack.line", { line: problem.line })} </strong>
                    ) : null}
                    {problem.message}
                  </li>
                ))}
              </ul>
            )}

            {canAct && dirty && (
              <div className="editor-foot">
                <input
                  className="input"
                  aria-label={t("stack.note")}
                  placeholder={t("stack.note")}
                  value={note}
                  maxLength={1024}
                  onChange={(event) => setNote(event.target.value)}
                />
                <Button disabled={busy} onClick={() => void save()}>
                  {t("common.save")}
                </Button>
                <Button disabled={busy} onClick={clearDraft}>
                  {t("stack.discard")}
                </Button>
              </div>
            )}
          </section>
          <Card title={t("task.stackTitle")} note={t("task.stackCardNote")}>
            <TaskList target={{ kind: "stack", id: stack.id, services }} canAct={canAct} />
          </Card>
        </div>

        <div className="column-side">
          <RevisionsCard
            stack={stack}
            revisions={revisions.items}
            draftChanged={dirty}
            draftStats={stats}
            canAct={canAct}
            onRollback={setRollingBack}
          />
          <SecretsCard stack={stack} canAct={canAct} />
          {canAct && (
            <Card title={t("stack.removeTitle")}>
              <p className="muted">{t("stack.removeNote")}</p>
              <div style={{ marginTop: 12 }}>
                <Button
                  variant="danger"
                  disabled={containers.items.length > 0}
                  onClick={() => setConfirmingDelete(true)}
                >
                  {t("stack.remove")}
                </Button>
              </div>
            </Card>
          )}
        </div>
      </div>

      {containers.items.length > 0 && (
        <LogViewer
          path={`/api/pc/stacks/${stack.id}/logs`}
          services={services}
          fileName={`${stack.name}.log`}
        />
      )}

      {host?.status === "offline" && canAct && (
        <Banner tone="warn">{t("stack.hostOffline")}</Banner>
      )}

      <ConfirmDialog
        open={rollingBack !== null}
        title={t("stack.rollbackConfirm", { revision: rollingBack?.number })}
        confirmLabel={t("stack.rollback")}
        onConfirm={() => {
          if (rollingBack) {
            void rollback(rollingBack);
          }
        }}
        onCancel={() => setRollingBack(null)}
      >
        <p className="muted">{t("stack.rollbackNote")}</p>
      </ConfirmDialog>

      <ConfirmDialog
        open={confirmingDown}
        danger
        title={t("stack.downConfirm", { name: stack.name })}
        confirmLabel={t("stack.down")}
        onConfirm={() => {
          setConfirmingDown(false);
          void action("stack.down", { remove_volumes: removeVolumes });
        }}
        onCancel={() => setConfirmingDown(false)}
      >
        <p className="muted">{t("stack.downNote")}</p>
        <label style={{ display: "flex", gap: 8, alignItems: "center", marginTop: 10 }}>
          <input
            type="checkbox"
            checked={removeVolumes}
            onChange={(event) => setRemoveVolumes(event.target.checked)}
          />
          {t("stack.downVolumes")}
        </label>
      </ConfirmDialog>

      <ConfirmDialog
        open={confirmingDelete}
        danger
        title={t("stack.removeConfirm", { name: stack.name })}
        confirmLabel={t("stack.remove")}
        onConfirm={() => void remove()}
        onCancel={() => setConfirmingDelete(false)}
      />
    </>
  );
}
