import { Pencil, Plus, RefreshCw, Trash2 } from "lucide-react";
import type { RecordModel } from "pocketbase";
import { lazy, Suspense, useMemo, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { useSearchParams } from "react-router-dom";

import { CommandOutput } from "../components/CommandOutput";
import { PageHead } from "../components/Layout";
import { useToast } from "../components/toast";
import {
  Banner,
  Button,
  Card,
  ConfirmDialog,
  EmptyState,
  Field,
  ProgressBar,
  StatusPill,
} from "../components/ui";
import { errorMessage } from "../lib/api";
import { hasRole, useAuth } from "../lib/auth";
import {
  checkFields,
  defaultGroups,
  openChecksFilter,
  unverifiedFilter,
  type BackupCheck,
  type CheckSettingsRecord,
} from "../lib/backupChecks";
import { runFields, runsSince, type BackupJob, type BackupRun } from "../lib/backupJobs";
import {
  draftRecord,
  openRestoreFilter,
  restoreOpen,
  snapshotSources,
  startedRestoresFilter,
  type BackupRestore,
} from "../lib/backupRestores";
import {
  anyCredential,
  backupsApi,
  emptyTargetForm,
  halfCredentials,
  hostKeys,
  keyscanCommand,
  maxImmutableDays,
  parseDays,
  requiredFields,
  sameHostKeys,
  serverOption,
  targetConfig,
  targetCredentials,
  targetKept,
  targetKinds,
  targetTone,
  targetUsage,
  type BackupTarget,
  type BackupTargetChange,
  type BackupTargetKind,
  type TargetForm,
} from "../lib/backups";
import { formatRelative } from "../lib/format";
import { useMounted } from "../lib/mounted";
import { pb } from "../lib/pb";
import { useLiveList, useLiveRecords, useNow } from "../lib/realtime";
import type { Alert, Host } from "../lib/types";
import { FailureBanners } from "./BackupFailures";
import { IntegrityDrawer, IntegrityLine, openCheckOf } from "./BackupIntegrity";
import { BackupSummary, JobDialog, JobsCard, type StackName } from "./BackupJobs";
import { RecentRestores, SnapshotsCard } from "./BackupSnapshots";

// the restore drawer, with its file browser, loads when it opens
const RestoreDrawer = lazy(() =>
  import("./BackupRestore").then((module) => ({ default: module.RestoreDrawer })),
);

/** Backups: the summary of the last 14 days, the jobs (#29), the snapshots
 * of the job picked and their restores (#30), and the targets they go to
 * (#28). Which job and target are picked, and the restore shown, are kept
 * in the address: ?job=&target=&restore= (new, or a restore's id), with the
 * snapshot to restore and whether to browse it first. */
export function BackupsPage() {
  const { t } = useTranslation();
  const { user } = useAuth();
  const isAdmin = hasRole(user, "admin");
  const canRun = hasRole(user, "operator");
  const now = useNow(30_000);
  const [params, setParams] = useSearchParams();
  const targets = useLiveList<BackupTarget>("backup_targets", { sort: "name" });
  const hosts = useLiveList<Host>("hosts", { sort: "name" });
  const jobs = useLiveList<BackupJob>("backup_jobs", { sort: "name" });
  const stacks = useLiveList<StackName & RecordModel>("stacks", {
    sort: "name",
    fields: "id,collectionId,collectionName,name,host",
    perPage: 500,
  });
  // the runs of the last 14 days, and a day more for any time zone
  const since = runsSince(now);
  const runs = useLiveRecords<BackupRun>("backup_runs", {
    filter: pb.filter("created >= {:since}", { since }),
    fields: runFields,
  });
  // the integrity checks under way, and the targets an alert says are not
  // verified (#31)
  const openChecks = useLiveList<BackupCheck>("backup_checks", {
    filter: openChecksFilter,
    fields: checkFields,
    sort: "-created",
    perPage: 100,
  });
  const unverified = useLiveList<Alert>("alerts", { filter: unverifiedFilter, perPage: 200 });
  // each target's newest check, for the group it read and when it ended
  const lastIds = targets.items.map((target) => target.last_check).filter(Boolean);
  const lastChecks = useLiveList<BackupCheck>("backup_checks", {
    filter: lastIds.map((id) => pb.filter("id = {:id}", { id })).join(" || "),
    fields: checkFields,
    perPage: 200,
    enabled: lastIds.length > 0,
  });
  // how many groups a check reads the data packs in
  const settings = useLiveList<CheckSettingsRecord>("settings", {
    fields: "id,collectionId,collectionName,backup_check_schedule,backup_check_groups",
    perPage: 1,
  });
  const groups = settings.items[0]?.backup_check_groups ?? defaultGroups;
  // "new", or the id of the job being changed
  const [dialog, setDialog] = useState<string | null>(null);
  const edited = jobs.items.find((job) => job.id === dialog);

  // the first job is picked until one is
  const job = jobs.items.find((item) => item.id === params.get("job")) ?? jobs.items[0] ?? null;
  const sources = job ? snapshotSources(job, targets.items, runs.items) : [];
  const source =
    sources.find((item) => item.target.id === params.get("target")) ?? sources[0] ?? null;
  // operators read the job's restores: the newest that were started, as a
  // draft is someone's own, in their drawer, and the one under way, which
  // newer records never push out
  const restores = useLiveList<BackupRestore>("backup_restores", {
    filter: job ? startedRestoresFilter(job.id) : undefined,
    sort: "-created",
    perPage: 5,
    enabled: canRun && job !== null,
  });
  const underWay = useLiveList<BackupRestore>("backup_restores", {
    filter: job ? openRestoreFilter(job.id) : undefined,
    sort: "-created",
    perPage: 1,
    enabled: canRun && job !== null,
  });
  const ours = (items: BackupRestore[]) =>
    canRun && job ? items.filter((item) => item.job === job.id && !draftRecord(item)) : [];
  const open =
    ours(underWay.items).find((item) => restoreOpen(item) && item.mode !== "download") ?? null;
  const newest = ours(restores.items);
  const started = open && !newest.some((item) => item.id === open.id) ? [...newest, open] : newest;
  const restoring = params.get("restore") ?? "";
  // the target whose integrity checks are shown: ?check=<target>
  const checked = targets.items.find((target) => target.id === params.get("check")) ?? null;
  // the output shown under the problems: a run's log, ?output=<command>
  const output = params.get("output") ?? "";

  const pick = (values: Record<string, string>) =>
    setParams(
      (current) => {
        const next = new URLSearchParams(current);
        for (const [key, value] of Object.entries(values)) {
          if (value) {
            next.set(key, value);
          } else {
            next.delete(key);
          }
        }
        return next;
      },
      { replace: true },
    );

  return (
    <>
      <PageHead
        title={t("nav.backups")}
        description={t("backups.lead")}
        actions={
          (canRun || isAdmin) && (
            <>
              {canRun && (
                <Button onClick={() => pick({ restore: "new", snapshot: "", browse: "" })}>
                  {t("backups.restoreFromBackup")}
                </Button>
              )}
              {isAdmin && (
                <Button variant="primary" onClick={() => setDialog("new")}>
                  <Plus size={16} aria-hidden="true" /> {t("backups.job.new")}
                </Button>
              )}
            </>
          )
        }
      />
      <BackupSummary
        jobs={jobs.items}
        runs={runs.items}
        targets={targets.items}
        hosts={hosts.items}
        unverified={unverified.items}
        openChecks={openChecks.items}
        now={now}
      />
      <FailureBanners
        jobs={jobs.items}
        runs={runs.items}
        targets={targets.items}
        hosts={hosts.items}
        now={now}
        canRun={canRun}
        onShowLog={(command) => pick({ output: command })}
        onDetails={(target) => pick({ check: target.id })}
      />
      {output && (
        <CommandOutput
          key={output}
          commandId={output}
          title={outputTitle(t, output, runs.items, jobs.items, targets.items, [
            ...openChecks.items,
            ...lastChecks.items,
          ])}
          onClose={() => pick({ output: "" })}
        />
      )}
      <JobsCard
        jobs={jobs.items}
        runs={runs.items}
        targets={targets.items}
        hosts={hosts.items}
        stacks={stacks.items}
        loading={jobs.loading}
        error={jobs.error || runs.error}
        now={now}
        canRun={canRun}
        isAdmin={isAdmin}
        selected={job?.id}
        onSelect={(item) => pick({ job: item.id, target: "" })}
        onAdd={() => setDialog("new")}
        onEdit={(item) => setDialog(item.id)}
      />
      <div className="columns">
        {job && (
          <div className="column-wide">
            <SnapshotsCard
              job={job}
              sources={sources}
              target={source?.target ?? null}
              hosts={hosts.items}
              runs={runs.items}
              now={now}
              canRestore={canRun}
              open={open}
              onTarget={(id) => pick({ target: id })}
              onRestore={(snapshot) => pick({ restore: "new", snapshot: snapshot.id, browse: "" })}
              onFiles={(snapshot) => pick({ restore: "new", snapshot: snapshot.id, browse: "1" })}
              onShowRestore={(id) => pick({ restore: id })}
            />
            <RecentRestores
              restores={started}
              hosts={hosts.items}
              now={now}
              onShow={(id) => pick({ restore: id })}
            />
          </div>
        )}
        <div className="column-side">
          {targets.error ? (
            <Banner tone="err">{errorMessage(targets.error)}</Banner>
          ) : (
            <TargetsCard
              targets={targets.items}
              hosts={hosts.items}
              loading={targets.loading}
              isAdmin={isAdmin}
              openChecks={openChecks.items}
              lastChecks={lastChecks.items}
              groups={groups}
              onDetails={(target) => pick({ check: target.id })}
            />
          )}
        </div>
      </div>
      {checked && (
        <IntegrityDrawer
          key={checked.id}
          target={checked}
          host={hosts.items.find((host) => host.id === checked.host)}
          openChecks={openChecks.items}
          groups={groups}
          now={now}
          isAdmin={isAdmin}
          canRun={canRun}
          onClose={() => pick({ check: "" })}
        />
      )}
      {canRun && restoring && (
        <Suspense fallback={null}>
          <RestoreDrawer
            jobs={jobs.items}
            targets={targets.items}
            hosts={hosts.items}
            runs={runs.items}
            stacks={stacks.items}
            start={{
              job: job?.id,
              target: source?.target.id,
              snapshot: params.get("snapshot") ?? undefined,
              restore: restoring,
              browse: params.get("browse") === "1",
            }}
            onRecord={(id) => pick({ restore: id ?? "new", browse: "" })}
            onClose={() => pick({ restore: "", snapshot: "", browse: "" })}
          />
        </Suspense>
      )}
      {(dialog === "new" || edited) && (
        <JobDialog
          key={edited?.id ?? "new"}
          job={edited ?? null}
          targets={targets.items}
          hosts={hosts.items}
          stacks={stacks.items}
          onClose={() => setDialog(null)}
        />
      )}
    </>
  );
}

/** What a command's output is the log of: a run of a job to a target, or a
 * target's integrity check. */
function outputTitle(
  t: (key: string, options?: Record<string, unknown>) => string,
  command: string,
  runs: BackupRun[],
  jobs: BackupJob[],
  targets: BackupTarget[],
  checks: BackupCheck[],
): string {
  const name = (id: string) => targets.find((target) => target.id === id)?.name ?? "–";
  const run = runs.find((item) => item.command === command);
  if (run) {
    return t("backups.failure.logTitle", {
      job: jobs.find((job) => job.id === run.job)?.name ?? "–",
      target: name(run.target),
    });
  }
  const check = checks.find((item) => item.command === command);
  if (check) {
    return t("backups.integrity.logTitle", { name: name(check.target) });
  }
  return t("command.output");
}

/** The targets as the design lists them: name and usage, where it is, a bar
 * when the capacity is known, and the notes; with the server that reaches it
 * and how its last check went. */
export function TargetsCard({
  targets,
  hosts,
  loading,
  isAdmin,
  openChecks = [],
  lastChecks = [],
  groups = defaultGroups,
  onDetails = () => {},
}: {
  targets: BackupTarget[];
  hosts: Host[];
  loading: boolean;
  isAdmin: boolean;
  /** the integrity checks queued or running (#31) */
  openChecks?: BackupCheck[];
  /** the targets' newest checks */
  lastChecks?: BackupCheck[];
  /** how many groups a check reads the data packs in */
  groups?: number;
  /** shows a target's integrity checks */
  onDetails?: (target: BackupTarget) => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<string | null>(null);
  const [removing, setRemoving] = useState<BackupTarget | null>(null);
  const [removeBusy, setRemoveBusy] = useState(false);
  const [refreshing, setRefreshing] = useState<ReadonlySet<string>>(new Set());
  const now = useNow(30_000);
  const edited = targets.find((target) => target.id === editing);

  const refresh = async (target: BackupTarget) => {
    setRefreshing((current) => new Set(current).add(target.id));
    try {
      const result = await backupsApi.refresh(target.id);
      if (result.status === "failed" || (result.status === "pending" && result.last_error)) {
        toast.error(
          t("backups.refreshFailed", {
            name: result.name,
            error: result.last_error || t("common.unknown"),
          }),
        );
      } else {
        toast.success(t("backups.refreshed", { name: result.name }));
      }
    } catch (err) {
      toast.error(err);
    } finally {
      setRefreshing((current) => {
        const next = new Set(current);
        next.delete(target.id);
        return next;
      });
    }
  };

  const remove = async () => {
    if (!removing) {
      return;
    }
    const { id, name } = removing;
    setRemoveBusy(true);
    try {
      await backupsApi.remove(id);
      toast.success(t("backups.removed", { name }));
      // the admin may have cancelled and asked to remove another meanwhile
      setRemoving((current) => (current?.id === id ? null : current));
    } catch (err) {
      toast.error(err);
    } finally {
      setRemoveBusy(false);
    }
  };

  return (
    <div className="backup-targets">
      <Card
        title={t("backups.targets")}
        side={
          isAdmin && (
            <Button size="small" onClick={() => setAdding(true)}>
              <Plus size={14} aria-hidden="true" /> {t("backups.add")}
            </Button>
          )
        }
        flush
      >
        {loading && targets.length === 0 ? (
          <p className="card-pad muted">{t("common.loading")}</p>
        ) : targets.length === 0 ? (
          <EmptyState title={t("backups.noTargetsTitle")}>{t("backups.noTargetsText")}</EmptyState>
        ) : (
          <ul className="list" aria-label={t("backups.targets")}>
            {targets.map((target) => (
              <TargetItem
                key={target.id}
                target={target}
                host={hosts.find((host) => host.id === target.host)}
                now={now}
                integrity={
                  <IntegrityLine
                    target={target}
                    host={hosts.find((host) => host.id === target.host)}
                    open={openCheckOf(target, openChecks)}
                    last={lastChecks.find((check) => check.id === target.last_check) ?? null}
                    groups={groups}
                    now={now}
                    isAdmin={isAdmin}
                    onDetails={() => onDetails(target)}
                  />
                }
                actions={
                  isAdmin && (
                    <div className="row-actions">
                      <Button
                        size="small"
                        aria-label={t("backups.refreshName", { name: target.name })}
                        disabled={refreshing.has(target.id)}
                        onClick={() => void refresh(target)}
                      >
                        <RefreshCw size={14} aria-hidden="true" />
                        {refreshing.has(target.id) ? t("backups.refreshing") : t("backups.refresh")}
                      </Button>
                      <Button
                        size="small"
                        aria-label={t("backups.editName", { name: target.name })}
                        onClick={() => setEditing(target.id)}
                      >
                        <Pencil size={14} aria-hidden="true" /> {t("common.edit")}
                      </Button>
                      <Button
                        size="small"
                        variant="danger"
                        aria-label={t("backups.removeName", { name: target.name })}
                        onClick={() => setRemoving(target)}
                      >
                        <Trash2 size={14} aria-hidden="true" /> {t("common.remove")}
                      </Button>
                    </div>
                  )
                }
              />
            ))}
          </ul>
        )}
      </Card>

      {adding && <AddTargetDialog hosts={hosts} onClose={() => setAdding(false)} />}
      {edited && (
        <EditTargetDialog
          key={edited.id}
          target={edited}
          hosts={hosts}
          onClose={() => setEditing(null)}
        />
      )}
      <ConfirmDialog
        open={removing !== null}
        title={t("backups.removeTitle", { name: removing?.name ?? "" })}
        confirmLabel={t("common.remove")}
        danger
        confirmDisabled={removeBusy}
        onCancel={() => setRemoving(null)}
        onConfirm={() => void remove()}
      >
        <p className="muted">{t("backups.removeText")}</p>
      </ConfirmDialog>
    </div>
  );
}

function TargetItem({
  target,
  host,
  now,
  integrity,
  actions,
}: {
  target: BackupTarget;
  host: Host | undefined;
  now: number;
  /** its integrity line (#31) */
  integrity: ReactNode;
  actions: ReactNode;
}) {
  const { t } = useTranslation();
  const server = host?.name;
  const usage = targetUsage(target);
  const status = target.status || "pending";
  const titleId = `target-${target.id}-name`;

  let usageText = "";
  if (usage.kind === "capacity") {
    usageText = usage.text;
  } else if (usage.kind === "unlimited") {
    usageText = t("backups.noLimit", { used: usage.used });
  } else if (usage.kind === "used") {
    usageText = usage.used;
  }

  const meta = [
    server &&
      (target.kind === "local"
        ? t("backups.onServer", { server })
        : t("backups.throughAgent", { server })),
    target.checked_at &&
      t("backups.checked", { when: formatRelative(target.checked_at, now, { past: true }) }),
  ].filter(Boolean);

  return (
    <li className="backup-target" aria-labelledby={titleId}>
      <div className="backup-target-head">
        <h3 id={titleId} className="backup-target-name">
          {target.name}
        </h3>
        {usageText && <span className="backup-target-usage">{usageText}</span>}
      </div>
      <div className="backup-target-location mono">{target.location}</div>
      {usage.kind === "capacity" && (
        <ProgressBar value={usage.percent} label={t("backups.usageLabel", { name: target.name })} />
      )}
      {target.immutable_days > 0 && (
        <div className="muted small">
          {t("backups.objectLock", { count: target.immutable_days })}
        </div>
      )}
      {target.note && <div className="muted small backup-target-note">{target.note}</div>}
      <div className="backup-target-meta">
        <StatusPill tone={targetTone(status)}>{t(`backups.status.${status}`)}</StatusPill>
        {meta.length > 0 && <span>{meta.join(" · ")}</span>}
        {actions}
      </div>
      {(status === "failed" || status === "pending") && target.last_error && (
        <pre className={`backup-target-error ${status}`}>{target.last_error}</pre>
      )}
      {integrity}
    </li>
  );
}

/** The server whose agent runs restic for a target, or that a restore
 * writes on. A server that `unavailable` names cannot be chosen. */
export function ServerField({
  id = "target-host",
  label,
  hosts,
  value,
  onChange,
  hint,
  unavailable,
}: {
  id?: string;
  label?: string;
  hosts: Host[];
  value: string;
  onChange: (value: string) => void;
  hint: ReactNode;
  unavailable?: (host: Host) => boolean;
}) {
  const { t } = useTranslation();
  return (
    <Field label={label ?? t("backups.server")} htmlFor={id}>
      <select
        id={id}
        className="input"
        required
        value={value}
        onChange={(event) => onChange(event.target.value)}
      >
        {value === "" && <option value="">{t("backups.chooseServer")}</option>}
        {hosts.map((item) => (
          <option key={item.id} value={item.id} disabled={unavailable?.(item)}>
            {serverOption(item, t("backups.serverOffline"))}
          </option>
        ))}
      </select>
      <p className="muted small">{hint}</p>
    </Field>
  );
}

/** The fields of a kind: its config when adding, and its credentials. */
function KindFields({
  kind,
  form,
  set,
  part,
  required,
}: {
  kind: BackupTargetKind;
  form: TargetForm;
  set: (field: keyof TargetForm, value: string | boolean) => void;
  /** config and credentials to add a target, or new credentials to edit one */
  part: "all" | "credentials";
  required: boolean;
}) {
  const { t } = useTranslation();
  const config = part === "all";

  const text = (
    field: keyof TargetForm,
    label: string,
    options: {
      placeholder?: string;
      secret?: boolean;
      mono?: boolean;
      hint?: ReactNode;
    } = {},
  ) => (
    <Field key={field} label={label} htmlFor={`target-${field}`}>
      <input
        id={`target-${field}`}
        className={`input${options.mono === false ? "" : " mono"}`}
        type={options.secret ? "password" : "text"}
        autoComplete={options.secret ? "new-password" : "off"}
        spellCheck={false}
        placeholder={options.placeholder}
        required={required && requiredFields[kind].includes(field)}
        value={form[field] as string}
        onChange={(event) => set(field, event.target.value)}
      />
      {options.hint && <p className="muted small">{options.hint}</p>}
    </Field>
  );

  switch (kind) {
    case "s3":
      return (
        <>
          {config && (
            <>
              {text("endpoint", t("backups.field.endpoint"), {
                placeholder: "https://s3.eu-central-003.backblazeb2.com",
              })}
              <div className="form-grid">
                {text("bucket", t("backups.field.bucket"), { placeholder: "homelab-backups" })}
                {text("prefix", t("backups.field.prefix"), { placeholder: "pocketcloud" })}
              </div>
              {text("region", t("backups.field.region"), { placeholder: "eu-central-003" })}
              <div>
                <label className="check-line">
                  <input
                    type="checkbox"
                    checked={form.path_style}
                    onChange={(event) => set("path_style", event.target.checked)}
                  />
                  {t("backups.field.pathStyle")}
                </label>
                <p className="muted small">{t("backups.hint.pathStyle")}</p>
              </div>
            </>
          )}
          <div className="form-grid">
            {text("access_key_id", t("backups.field.accessKeyId"))}
            {text("secret_access_key", t("backups.field.secretAccessKey"), { secret: true })}
          </div>
        </>
      );
    case "sftp":
      return (
        <>
          {config && (
            <>
              <div className="form-grid">
                {text("address", t("backups.field.address"), {
                  placeholder: "backup-node:22",
                  hint: t("backups.hint.address"),
                })}
                {text("user", t("backups.field.user"), { placeholder: "backup" })}
              </div>
              {text("path", t("backups.field.path"), { placeholder: "/srv/restic" })}
              <HostKeyField form={form} set={set} required={required} />
            </>
          )}
          <Field label={t("backups.field.privateKey")} htmlFor="target-private_key">
            <textarea
              id="target-private_key"
              className="input mono"
              rows={4}
              spellCheck={false}
              autoComplete="off"
              placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
              required={required}
              value={form.private_key}
              onChange={(event) => set("private_key", event.target.value)}
            />
            <p className="muted small">{t("backups.hint.privateKey")}</p>
          </Field>
        </>
      );
    case "rest":
      return (
        <>
          {config &&
            text("url", t("backups.field.url"), {
              placeholder: "https://backup-node:8000/homelab/",
              hint: t("backups.hint.url"),
            })}
          <div className="form-grid">
            {text("username", t("backups.field.username"))}
            {text("password", t("backups.field.password"), { secret: true })}
          </div>
          {config && <p className="muted small">{t("backups.hint.restCredentials")}</p>}
        </>
      );
    case "local":
      return config
        ? text("path", t("backups.field.localPath"), {
            placeholder: "/srv/backups/restic",
            hint: t("backups.hint.localPath"),
          })
        : null;
  }
}

/** Something typed in the host key field, but no key in it. */
function noHostKey(form: TargetForm) {
  return form.host_key.trim() !== "" && hostKeys(form.host_key) === "";
}

/** The SFTP server's host keys, pasted as ssh-keyscan prints them. */
function HostKeyField({
  form,
  set,
  required = false,
}: {
  form: TargetForm;
  set: (field: keyof TargetForm, value: string) => void;
  required?: boolean;
}) {
  const { t } = useTranslation();
  return (
    <Field
      label={t("backups.field.hostKey")}
      htmlFor="target-host_key"
      error={noHostKey(form) ? t("backups.hostKeyInvalid") : undefined}
    >
      <textarea
        id="target-host_key"
        className="input mono"
        rows={3}
        autoComplete="off"
        spellCheck={false}
        required={required}
        placeholder="ssh-ed25519 AAAA…"
        value={form.host_key}
        onChange={(event) => set("host_key", event.target.value)}
      />
      <p className="muted small">
        {t("backups.hint.hostKey")} <code>{keyscanCommand(form.address)}</code>
      </p>
    </Field>
  );
}

/** A field for the days of a bucket's Object Lock. */
function DaysField({
  value,
  onChange,
  invalid,
}: {
  value: string;
  onChange: (value: string) => void;
  invalid: boolean;
}) {
  const { t } = useTranslation();
  return (
    <Field
      label={t("backups.field.immutableDays")}
      htmlFor="target-days"
      error={invalid ? t("backups.daysInvalid") : undefined}
    >
      <input
        id="target-days"
        className="input"
        type="number"
        inputMode="numeric"
        min={0}
        max={maxImmutableDays}
        step={1}
        value={value}
        onChange={(event) => onChange(event.target.value)}
      />
      <p className="muted small">{t("backups.hint.immutableDays")}</p>
    </Field>
  );
}

/** Adds a target: the panel checks it, then the agent of its server opens the
 * repository with restic, or creates it. */
export function AddTargetDialog({ hosts, onClose }: { hosts: Host[]; onClose: () => void }) {
  const { t } = useTranslation();
  const toast = useToast();
  const mounted = useMounted();
  const [kind, setKind] = useState<BackupTargetKind>("s3");
  const [name, setName] = useState("");
  const [host, setHost] = useState("");
  const [form, setForm] = useState<TargetForm>(emptyTargetForm);
  const [days, setDays] = useState("");
  const [note, setNote] = useState("");
  // the admin's own password, or that of a repository restic already has
  const [repoPassword, setRepoPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const immutableDays = kind === "s3" ? parseDays(days) : 0;
  const half = halfCredentials(kind, form);
  const missing = requiredFields[kind].some((field) => {
    const value = form[field];
    return typeof value === "string" && value.trim() === "";
  });
  const ready =
    name.trim() !== "" &&
    host !== "" &&
    !missing &&
    !half &&
    !(kind === "sftp" && noHostKey(form)) &&
    immutableDays !== null;
  const server = hosts.find((item) => item.id === host)?.name ?? "";

  const set = (field: keyof TargetForm, value: string | boolean) =>
    setForm((current) => ({ ...current, [field]: value }));

  const add = async () => {
    if (!ready || busy) {
      return;
    }
    setBusy(true);
    setError("");
    try {
      const target = await backupsApi.add({
        name: name.trim(),
        kind,
        host,
        config: targetConfig(kind, form),
        credentials: targetCredentials(kind, form),
        immutable_days: immutableDays ?? 0,
        note: note.trim(),
        ...(repoPassword !== "" ? { password: repoPassword } : {}),
      });
      toast.success(t("backups.added", { name: target.name }));
      // closed while restic was at it: whatever is open now is another's
      if (mounted.current) {
        onClose();
      }
    } catch (err) {
      if (targetKept(err)) {
        // the panel keeps the target, pending, and says what to do with it:
        // adding it again could only fail as a duplicate
        toast.error(err);
        if (mounted.current) {
          onClose();
        }
      } else if (mounted.current) {
        setError(errorMessage(err));
      } else {
        // closed while restic was at it: the toast is all that is left
        toast.error(err);
      }
    } finally {
      if (mounted.current) {
        setBusy(false);
      }
    }
  };

  return (
    <ConfirmDialog
      open
      title={t("backups.addTitle")}
      confirmLabel={busy ? t("backups.checking") : t("backups.add")}
      confirmDisabled={busy || !ready}
      onCancel={onClose}
      onConfirm={() => void add()}
    >
      <div className="dialog-form">
        <fieldset className="dialog-fieldset" disabled={busy}>
          <Field label={t("backups.kind")} htmlFor="target-kind">
            <select
              id="target-kind"
              className="input"
              value={kind}
              onChange={(event) => {
                setKind(event.target.value as BackupTargetKind);
                setForm(emptyTargetForm);
                setError("");
              }}
            >
              {targetKinds.map((item) => (
                <option key={item} value={item}>
                  {t(`backups.kindName.${item}`)}
                </option>
              ))}
            </select>
            <p className="muted small">{t(`backups.kindNote.${kind}`)}</p>
          </Field>
          <Field label={t("backups.name")} htmlFor="target-name">
            <input
              id="target-name"
              className="input"
              autoComplete="off"
              required
              placeholder={t(`backups.namePlaceholder.${kind}`)}
              value={name}
              onChange={(event) => setName(event.target.value)}
            />
          </Field>
          <ServerField
            hosts={hosts}
            value={host}
            onChange={setHost}
            hint={kind === "local" ? t("backups.serverHintLocal") : t("backups.serverHint")}
          />
          <KindFields kind={kind} form={form} set={set} part="all" required />
          {half && <p className="field-error">{t("backups.bothOrNeither")}</p>}
          <Field label={t("backups.field.repoPassword")} htmlFor="target-repo-password">
            <input
              id="target-repo-password"
              className="input mono"
              type="password"
              autoComplete="new-password"
              value={repoPassword}
              onChange={(event) => setRepoPassword(event.target.value)}
            />
            <p className="muted small">{t("backups.hint.repoPassword")}</p>
          </Field>
          {kind === "s3" && (
            <DaysField value={days} onChange={setDays} invalid={immutableDays === null} />
          )}
          <NoteField value={note} onChange={setNote} />
        </fieldset>
        {busy && <Banner tone="info">{t("backups.checkingNote", { server })}</Banner>}
        {error && <Banner tone="err">{error}</Banner>}
      </div>
    </ConfirmDialog>
  );
}

function NoteField({ value, onChange }: { value: string; onChange: (value: string) => void }) {
  const { t } = useTranslation();
  return (
    <Field label={t("backups.field.note")} htmlFor="target-note">
      <input
        id="target-note"
        className="input"
        maxLength={500}
        autoComplete="off"
        value={value}
        onChange={(event) => onChange(event.target.value)}
      />
    </Field>
  );
}

/** Changes what may change on a target: its name, the Object Lock note, the
 * note, new credentials, an SFTP server's host key and, but for a local one,
 * the server that reaches it. The kind and the location stay: moving the
 * repository takes a new target. */
export function EditTargetDialog({
  target,
  hosts,
  onClose,
}: {
  target: BackupTarget;
  hosts: Host[];
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const mounted = useMounted();
  const config = useMemo(() => target.config ?? {}, [target.config]);
  const [name, setName] = useState(target.name);
  const [days, setDays] = useState(target.immutable_days > 0 ? String(target.immutable_days) : "");
  const [note, setNote] = useState(target.note);
  const [host, setHost] = useState(target.host);
  // the address only names the server in the hint for its host key
  const [form, setForm] = useState<TargetForm>({
    ...emptyTargetForm,
    address: config.address ?? "",
    host_key: config.host_key ?? "",
  });
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const kind = target.kind;
  const immutableDays = kind === "s3" ? parseDays(days) : 0;
  const credentials = anyCredential(kind, form) ? targetCredentials(kind, form) : undefined;
  const half = halfCredentials(kind, form);
  const hostKey = hostKeys(form.host_key);
  const hostKeyChanged =
    kind === "sftp" && hostKey !== "" && !sameHostKeys(hostKey, config.host_key ?? "");
  // a local target's data is on its server: it cannot move
  const hostChanged = kind !== "local" && host !== "" && host !== target.host;
  // restic checks new settings before they replace the stored ones
  const checks = credentials !== undefined || hostKeyChanged || hostChanged;
  const ready =
    name.trim() !== "" && !half && !(kind === "sftp" && noHostKey(form)) && immutableDays !== null;
  const server = hosts.find((item) => item.id === host)?.name ?? "–";

  const set = (field: keyof TargetForm, value: string | boolean) =>
    setForm((current) => ({ ...current, [field]: value }));

  const save = async () => {
    if (!ready || busy) {
      return;
    }
    const body: BackupTargetChange = { name: name.trim(), note: note.trim() };
    if (kind === "s3") {
      body.immutable_days = immutableDays ?? 0;
    }
    if (hostChanged) {
      body.host = host;
    }
    if (credentials) {
      body.credentials = credentials;
    }
    if (hostKeyChanged) {
      // the host key goes with what it belongs to, which stays as it is
      body.config = {
        address: config.address ?? "",
        user: config.user ?? "",
        path: config.path ?? "",
        host_key: hostKey,
      };
    }
    setBusy(true);
    setError("");
    try {
      const saved = await backupsApi.change(target.id, body);
      toast.success(t("backups.saved", { name: saved.name }));
      if (mounted.current) {
        onClose();
      }
    } catch (err) {
      if (mounted.current) {
        setError(errorMessage(err));
      } else {
        toast.error(err);
      }
    } finally {
      if (mounted.current) {
        setBusy(false);
      }
    }
  };

  return (
    <ConfirmDialog
      open
      title={t("backups.editTitle", { name: target.name })}
      confirmLabel={
        busy ? (checks ? t("backups.checking") : t("backups.saving")) : t("common.save")
      }
      confirmDisabled={busy || !ready}
      onCancel={onClose}
      onConfirm={() => void save()}
    >
      <div className="dialog-form">
        <dl className="facts">
          <dt>{t("backups.kind")}</dt>
          <dd>{t(`backups.kindName.${kind}`)}</dd>
          {kind === "local" && (
            <>
              <dt>{t("backups.server")}</dt>
              <dd>{server}</dd>
            </>
          )}
          <dt>{t("backups.location")}</dt>
          <dd className="mono">{target.location}</dd>
        </dl>
        <p className="muted small">
          {kind === "local" ? t("backups.fixedLocal") : t("backups.fixed")}
        </p>
        <fieldset className="dialog-fieldset" disabled={busy}>
          <Field label={t("backups.name")} htmlFor="target-name">
            <input
              id="target-name"
              className="input"
              autoComplete="off"
              required
              value={name}
              onChange={(event) => setName(event.target.value)}
            />
          </Field>
          {kind === "s3" && (
            <DaysField value={days} onChange={setDays} invalid={immutableDays === null} />
          )}
          <NoteField value={note} onChange={setNote} />
          {kind !== "local" && (
            <ServerField
              hosts={hosts}
              value={host}
              onChange={setHost}
              hint={t("backups.serverChangeHint")}
            />
          )}
          {kind === "sftp" && <HostKeyField form={form} set={set} />}
          {kind !== "local" && (
            <>
              <h3 className="drawer-heading">{t("backups.credentials")}</h3>
              <p className="muted small">{t("backups.keepCredentials")}</p>
              <KindFields kind={kind} form={form} set={set} part="credentials" required={false} />
              {half && <p className="field-error">{t("backups.bothOrNeither")}</p>}
            </>
          )}
        </fieldset>
        {busy && checks && <Banner tone="info">{t("backups.checkingEditNote", { server })}</Banner>}
        {error && <Banner tone="err">{error}</Banner>}
      </div>
    </ConfirmDialog>
  );
}
