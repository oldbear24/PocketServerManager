import { Pencil, Plus, RefreshCw, Trash2 } from "lucide-react";
import type { RecordModel } from "pocketbase";
import { useMemo, useState, type ReactNode } from "react";
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
  ProgressBar,
  StatusPill,
} from "../components/ui";
import { errorMessage } from "../lib/api";
import { hasRole, useAuth } from "../lib/auth";
import { runFields, runsSince, type BackupJob, type BackupRun } from "../lib/backupJobs";
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
import type { Host } from "../lib/types";
import { BackupSummary, JobDialog, JobsCard, type StackName } from "./BackupJobs";

/** Backups: the summary of the last 14 days, the jobs (#29) and the targets
 * they go to (#28). Snapshots and restores come with later issues. */
export function BackupsPage() {
  const { t } = useTranslation();
  const { user } = useAuth();
  const isAdmin = hasRole(user, "admin");
  const canRun = hasRole(user, "operator");
  const now = useNow(30_000);
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
  // "new", or the id of the job being changed
  const [dialog, setDialog] = useState<string | null>(null);
  const edited = jobs.items.find((job) => job.id === dialog);

  return (
    <>
      <PageHead
        title={t("nav.backups")}
        description={t("backups.lead")}
        actions={
          isAdmin && (
            <Button variant="primary" onClick={() => setDialog("new")}>
              <Plus size={16} aria-hidden="true" /> {t("backups.job.new")}
            </Button>
          )
        }
      />
      <BackupSummary
        jobs={jobs.items}
        runs={runs.items}
        targets={targets.items}
        hosts={hosts.items}
        now={now}
      />
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
        onAdd={() => setDialog("new")}
        onEdit={(job) => setDialog(job.id)}
      />
      {targets.error ? (
        <Banner tone="err">{errorMessage(targets.error)}</Banner>
      ) : (
        <TargetsCard
          targets={targets.items}
          hosts={hosts.items}
          loading={targets.loading}
          isAdmin={isAdmin}
        />
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

/** The targets as the design lists them: name and usage, where it is, a bar
 * when the capacity is known, and the notes; with the server that reaches it
 * and how its last check went. */
export function TargetsCard({
  targets,
  hosts,
  loading,
  isAdmin,
}: {
  targets: BackupTarget[];
  hosts: Host[];
  loading: boolean;
  isAdmin: boolean;
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
                server={hosts.find((host) => host.id === target.host)?.name}
                now={now}
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
  server,
  now,
  actions,
}: {
  target: BackupTarget;
  server: string | undefined;
  now: number;
  actions: ReactNode;
}) {
  const { t } = useTranslation();
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
    </li>
  );
}

/** The server whose agent runs restic for a target. */
function ServerField({
  hosts,
  value,
  onChange,
  hint,
}: {
  hosts: Host[];
  value: string;
  onChange: (value: string) => void;
  hint: string;
}) {
  const { t } = useTranslation();
  return (
    <Field label={t("backups.server")} htmlFor="target-host">
      <select
        id="target-host"
        className="input"
        required
        value={value}
        onChange={(event) => onChange(event.target.value)}
      >
        {value === "" && <option value="">{t("backups.chooseServer")}</option>}
        {hosts.map((item) => (
          <option key={item.id} value={item.id}>
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
