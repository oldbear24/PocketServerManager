import { ArrowUp, Download, File, Folder, X } from "lucide-react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { CommandOutput } from "../components/CommandOutput";
import { CommandStatusPill, ContainerStatePill } from "../components/status";
import { useToast } from "../components/toast";
import {
  Banner,
  Button,
  ChoiceGroup,
  ConfirmDialog,
  Drawer,
  Field,
  IconButton,
  ProgressBar,
  StatusPill,
  Table,
  Timeline,
  type TimelineItem,
} from "../components/ui";
import { errorMessage } from "../lib/api";
import { hasRole, useAuth } from "../lib/auth";
import type { BackupJob, BackupRun } from "../lib/backupJobs";
import {
  browseStart,
  existingEntries,
  failedDraft,
  formatSnapshotTime,
  joinPath,
  modesFor,
  needsConfirm,
  openDownloadFrame,
  parentPath,
  pathCrumbs,
  pathLabel,
  previewSentence,
  restoreDraft,
  restoreEnded,
  restoresApi,
  restoreSteps,
  restoreTone,
  snapshotRoots,
  snapshotSources,
  underPath,
  useListing,
  validRestorePath,
  type BackupRestore,
  type BackupSnapshot,
  type Listing,
  type RestoreMode,
  type RestorePreview as Preview,
  type RestoreStep,
  type SnapshotEntry,
} from "../lib/backupRestores";
import type { BackupTarget } from "../lib/backups";
import { formatBytes, formatDateTime, formatNumber, parseDate } from "../lib/format";
import { useMounted } from "../lib/mounted";
import { useLiveRecord, useNow } from "../lib/realtime";
import type { Command, Host } from "../lib/types";
import { describeSource, type StackName } from "./BackupJobs";
import { ServerField } from "./Backups";

/** A page of a snapshot's folder. */
const pageSize = 200;

/** The most items of a snapshot a restore picks. */
const maxPicks = 100;

/** An item of a snapshot picked to restore or download. */
export type PickedItem = { path: string; type: SnapshotEntry["type"] };

/** What opens the drawer: a job, a target and a snapshot picked on the page,
 * or a restore to show again. */
export type RestoreStart = {
  job?: string;
  target?: string;
  snapshot?: string;
  /** a backup_restores record */
  restore?: string;
  /** opens the snapshot's files first */
  browse?: boolean;
};

/** A restore of a job's snapshot, from picking it to seeing how it went: what
 * of it, where to, its preview, the confirmation, its progress; or a
 * download of it. Lazy: the page loads it when it opens. */
export function RestoreDrawer({
  jobs,
  targets,
  hosts,
  runs,
  stacks,
  start,
  onRecord,
  onClose,
}: {
  jobs: BackupJob[];
  targets: BackupTarget[];
  hosts: Host[];
  runs: Pick<BackupRun, "job" | "target">[];
  stacks: StackName[];
  start: RestoreStart;
  /** the record the drawer shows, to keep in the address; null for none */
  onRecord: (id: string | null) => void;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const mounted = useMounted();
  const { user } = useAuth();
  const now = useNow(15_000);

  // undefined until picked here: what the page picked, once it has loaded
  const [pickedJob, setJobId] = useState<string>();
  const [pickedTarget, setTargetId] = useState<string>();
  const [pickedSnapshot, setSnapshotId] = useState<string>();
  const jobId = pickedJob ?? start.job ?? "";
  const targetId = pickedTarget ?? start.target ?? "";
  const snapshotId = pickedSnapshot ?? start.snapshot ?? "";
  const [recordId, setRecordId] = useState<string | null>(
    start.restore && start.restore !== "new" ? start.restore : null,
  );
  const [view, setView] = useState<"plan" | "browse">(start.browse ? "browse" : "plan");
  const [mode, setMode] = useState<RestoreMode | "">("");
  const [host, setHost] = useState("");
  const [folder, setFolder] = useState("");
  const [source, setSource] = useState("");
  const [picks, setPicks] = useState<PickedItem[]>([]);
  const [replace, setReplace] = useState(true);
  // the options changed since the last preview
  const [stale, setStale] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [confirming, setConfirming] = useState(false);
  const [confirmError, setConfirmError] = useState("");
  const [local, setLocal] = useState<BackupRestore | null>(null);
  const [frameError, setFrameError] = useState("");
  const frame = useRef<HTMLIFrameElement | null>(null);

  const live = useLiveRecord<BackupRestore>("backup_restores", recordId ?? undefined);
  const record = newer(live.record, local?.id === recordId ? local : null);

  // a restore opened again brings its own job, target, snapshot and options
  const synced = useRef("");
  useEffect(() => {
    if (!record || synced.current === record.id) {
      return;
    }
    synced.current = record.id;
    setJobId(record.job);
    setTargetId(record.target);
    setSnapshotId(record.snapshot_id);
    setMode(record.mode);
    setReplace(record.replace);
    if (record.mode === "path") {
      setHost(record.host);
      setFolder(record.path);
      setSource(record.source);
    }
    if (record.mode === "download") {
      setPicks(
        record.path
          ? [{ path: record.path, type: record.download_kind === "file" ? "file" : "dir" }]
          : [],
      );
    } else {
      setPicks((record.include ?? []).map((path) => ({ path, type: "dir" as const })));
    }
  }, [record]);

  // a download's frame goes a minute after it ended, when the browser has the
  // file; while it runs it stays, though the drawer closes
  const ending = record?.mode === "download" && restoreEnded(record.status);
  useEffect(() => {
    const done = frame.current;
    if (ending && done) {
      frame.current = null;
      setTimeout(() => done.remove(), 60_000);
    }
  }, [ending]);

  const job = jobs.find((item) => item.id === jobId) ?? null;
  const sources = job ? snapshotSources(job, targets, runs) : [];
  const target =
    sources.find((item) => item.target.id === targetId)?.target ?? sources[0]?.target ?? null;
  const listing = useListing(job && target ? `${job.id}|${target.id}` : null, (refresh) =>
    restoresApi.snapshots(job!.id, target!.id, refresh),
  );
  const snapshot = listing.data?.snapshots.find((item) => item.id === snapshotId) ?? null;
  const modes = job ? modesFor(target, job, user?.role) : null;
  const chosen: RestoreMode | "" =
    mode !== "" && modes && !modes[mode].disabled
      ? mode
      : modes
        ? ((["original", "path", "download"] as RestoreMode[]).find(
            (item) => !modes[item].disabled,
          ) ?? "")
        : "";
  const roots = snapshotRoots(snapshot?.paths ?? record?.preview?.snapshot.paths ?? []);
  // the snapshot's folder a new folder takes: one that holds every pick
  const sourceOptions = ["/", ...roots].filter((root) =>
    picks.every((pick) => underPath(pick.path, root)),
  );
  const sourceValue = sourceOptions.includes(source)
    ? source
    : roots.length === 1 && sourceOptions.includes(roots[0])
      ? roots[0]
      : "/";
  const localTarget = target?.kind === "local";
  const hostValue = localTarget ? target!.host : host;
  const server = (id: string | undefined) => hosts.find((item) => item.id === id)?.name ?? "–";

  const draft = record !== null && (restoreDraft(record.status) || failedDraft(record));
  const started = record !== null && !draft;
  const replaceOn = picks.length === 0 && (mode === "" ? chosen === "original" : replace);

  /** Discards the draft whose options are about to change. */
  const changed = (apply: () => void) => {
    apply();
    setError("");
    if (record && draft) {
      void restoresApi.discard(record.id).catch(() => {
        // a draft left behind is removed after a day
      });
      setRecordId(null);
      setLocal(null);
      onRecord(null);
      setStale(true);
    }
  };

  const folderInvalid =
    chosen === "path" && folder.trim() !== "" && !validRestorePath(folder.trim());
  const ready =
    job !== null &&
    target !== null &&
    snapshot !== null &&
    chosen !== "" &&
    (chosen !== "path" || (hostValue !== "" && folder.trim() !== "" && !folderInvalid)) &&
    (chosen !== "download" || picks.length <= 1);

  const preview = async () => {
    if (!ready || busy) {
      return;
    }
    setBusy(true);
    setError("");
    try {
      if (record && draft) {
        await restoresApi.discard(record.id).catch(() => undefined);
      }
      const created = await restoresApi.create(job!.id, {
        target: target!.id,
        snapshot: snapshot!.id,
        mode: chosen as "original" | "path",
        ...(chosen === "path" ? { host: hostValue, path: folder.trim(), source: sourceValue } : {}),
        ...(picks.length > 0 ? { include: picks.map((pick) => pick.path) } : {}),
        replace: replaceOn,
      });
      if (mounted.current) {
        synced.current = created.id;
        setLocal(created);
        setRecordId(created.id);
        setStale(false);
        onRecord(created.id);
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

  const begin = async (confirm: boolean) => {
    if (!record || busy) {
      return;
    }
    setBusy(true);
    setError("");
    setConfirmError("");
    try {
      const queued = await restoresApi.start(record.id, confirm);
      toast.success(t("backups.restore.started", { job: job?.name ?? "" }));
      if (mounted.current) {
        setLocal(queued);
        setConfirming(false);
      }
    } catch (err) {
      if (!mounted.current) {
        toast.error(err);
      } else if (confirm) {
        // the panel's 409 belongs to what was confirmed
        setConfirmError(errorMessage(err));
      } else {
        setError(errorMessage(err));
      }
    } finally {
      if (mounted.current) {
        setBusy(false);
      }
    }
  };

  const download = async (path: string, kind: "dir" | "file") => {
    if (!job || !target || !snapshot || busy) {
      return;
    }
    setBusy(true);
    setError("");
    setFrameError("");
    try {
      const answer = await restoresApi.download(job.id, {
        target: target.id,
        snapshot: snapshot.id,
        path,
        kind,
      });
      frame.current = openDownloadFrame(
        answer.url,
        t("backups.restore.downloadFrame", { name: answer.restore.download_name || path }),
        (message) => setFrameError(message),
      );
      if (mounted.current) {
        synced.current = answer.restore.id;
        setLocal(answer.restore);
        setRecordId(answer.restore.id);
        setMode("download");
        setView("plan");
        onRecord(answer.restore.id);
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

  const again = () => {
    setRecordId(null);
    setLocal(null);
    setFrameError("");
    setStale(false);
    onRecord(null);
  };

  const close = () => {
    if (record && draft) {
      void restoresApi.discard(record.id).catch(() => undefined);
    }
    onClose();
  };

  const downloadPath =
    picks.length === 1 ? picks[0].path : roots.length === 1 ? roots[0] : browseStart(roots);
  const downloadKind: "dir" | "file" =
    picks.length === 1 && picks[0].type === "file" ? "file" : "dir";

  // what the drawer shows, and its actions
  let body: ReactNode;
  let actions: ReactNode;
  const cancel = (
    <Button onClick={close} disabled={busy && !draft && !started}>
      {started ? t("common.close") : t("common.cancel")}
    </Button>
  );

  if (view === "browse" && job && target && snapshot) {
    body = (
      <SnapshotBrowser
        job={job}
        target={target}
        snapshot={snapshot}
        startPath={chosen === "path" ? sourceValue : browseStart(snapshot.paths)}
        picks={picks}
        onPicks={(next) => changed(() => setPicks(next))}
        canDownload={modes?.download.disabled === false}
        onDownload={(path, kind) => void download(path, kind)}
      />
    );
    actions = (
      <Button variant="primary" onClick={() => setView("plan")}>
        {t("backups.files.done")}
      </Button>
    );
  } else if (started && record.mode === "download") {
    body = (
      <>
        <RestoreFacts
          job={job}
          target={target}
          snapshot={snapshot}
          record={record}
          hosts={hosts}
          now={now}
        />
        <DownloadStatus record={record} host={server(record.host)} frameError={frameError} />
      </>
    );
    const failed = frameError !== "" || record.status === "failed" || record.status === "cancelled";
    actions = (
      <>
        {cancel}
        {(failed || record.status === "queued") && (
          <Button
            variant="primary"
            disabled={busy}
            onClick={() =>
              void download(
                record.path || downloadPath,
                record.download_kind === "file" ? "file" : "dir",
              )
            }
          >
            {t("backups.restore.download")}
          </Button>
        )}
      </>
    );
  } else if (started) {
    body = (
      <>
        <RestoreFacts
          job={job}
          target={target}
          snapshot={snapshot}
          record={record}
          hosts={hosts}
          now={now}
        />
        <RestoreProgress
          record={record}
          host={server(record.host)}
          canStop={hasRole(user, "operator")}
          onChanged={setLocal}
        />
      </>
    );
    actions = (
      <>
        {cancel}
        {(record.status === "failed" || record.status === "cancelled" || record.partial) &&
          restoreEnded(record.status) && (
            <Button variant="primary" onClick={again}>
              {t("backups.restore.again")}
            </Button>
          )}
      </>
    );
  } else {
    const previewed = record?.status === "previewed" && record.preview !== null;
    const expired =
      record !== null &&
      (record.status === "expired" ||
        (record.status === "previewed" &&
          (parseDate(record.expires_at)?.getTime() ?? Infinity) < now));
    const needs = previewed ? needsConfirm(record.preview, record.mode) : true;
    body = (
      <>
        {!record && (
          <RestorePick
            jobs={jobs}
            job={job}
            sources={sources}
            target={target}
            listing={listing}
            snapshotId={snapshotId}
            now={now}
            disabled={busy}
            onJob={(id) =>
              changed(() => {
                setJobId(id);
                setTargetId("");
                setSnapshotId("");
                setPicks([]);
              })
            }
            onTarget={(id) =>
              changed(() => {
                setTargetId(id);
                setSnapshotId("");
                setPicks([]);
              })
            }
            onSnapshot={(id) =>
              changed(() => {
                setSnapshotId(id);
                setPicks([]);
              })
            }
          />
        )}
        {job && target && snapshot && modes && (
          <>
            <RestoreFacts
              job={job}
              target={target}
              snapshot={snapshot}
              record={record}
              hosts={hosts}
              now={now}
              brief={!record}
            />
            <fieldset className="dialog-fieldset" disabled={busy}>
              <section aria-labelledby="restore-what">
                <h3 id="restore-what" className="drawer-heading">
                  {t("backups.restore.what")}
                </h3>
                {picks.length === 0 ? (
                  <p>{t("backups.restore.whole")}</p>
                ) : (
                  <ul className="chip-list restore-picks">
                    {picks.map((pick) => (
                      <li key={pick.path} className="chip">
                        <span className="mono">{pick.path}</span>
                        <IconButton
                          bare
                          aria-label={t("backups.restore.removePick", { path: pick.path })}
                          onClick={() =>
                            changed(() => setPicks(picks.filter((item) => item.path !== pick.path)))
                          }
                        >
                          <X size={13} aria-hidden="true" />
                        </IconButton>
                      </li>
                    ))}
                  </ul>
                )}
                <Button size="small" onClick={() => setView("browse")}>
                  {t("backups.restore.choose")}
                </Button>
              </section>
              <section aria-labelledby="restore-where" className="dialog-form">
                <h3 id="restore-where" className="drawer-heading">
                  {t("backups.restore.where")}
                </h3>
                <ChoiceGroup
                  label={t("backups.restore.where")}
                  value={chosen}
                  onChange={(value) =>
                    changed(() => {
                      setMode(value as RestoreMode);
                      // back to the snapshot's state at the original location; a
                      // new folder keeps what it holds unless asked
                      setReplace(value === "original");
                    })
                  }
                  options={(["original", "path", "download"] as RestoreMode[]).map((item) => ({
                    value: item,
                    title: t(`backups.restore.mode.${item}.label`),
                    note: modeNote(item, modes[item].reason),
                    disabled: modes[item].disabled,
                  }))}
                />
                {chosen === "path" && (
                  <>
                    <ServerField
                      id="restore-host"
                      label={t("backups.restore.server")}
                      hosts={hosts}
                      value={hostValue}
                      onChange={(value) => changed(() => setHost(value))}
                      unavailable={localTarget ? (item) => item.id !== target.host : undefined}
                      hint={
                        localTarget
                          ? t("backups.restore.localOnly", {
                              target: target.name,
                              host: server(target.host),
                            })
                          : t("backups.restore.mode.path.note")
                      }
                    />
                    <Field
                      label={t("backups.restore.folder")}
                      htmlFor="restore-folder"
                      error={folderInvalid ? t("backups.restore.folderInvalid") : undefined}
                    >
                      <input
                        id="restore-folder"
                        className="input mono"
                        autoComplete="off"
                        spellCheck={false}
                        placeholder="/srv/restored"
                        value={folder}
                        onChange={(event) => changed(() => setFolder(event.target.value))}
                      />
                      <p className="muted small">{t("backups.restore.folderHint")}</p>
                    </Field>
                    <Field label={t("backups.restore.source")} htmlFor="restore-source">
                      <select
                        id="restore-source"
                        className="input mono"
                        value={sourceValue}
                        onChange={(event) => changed(() => setSource(event.target.value))}
                      >
                        {sourceOptions.map((root) => (
                          <option key={root} value={root}>
                            {root === "/" ? `/ (${t("backups.restore.whole")})` : root}
                          </option>
                        ))}
                      </select>
                    </Field>
                  </>
                )}
                {chosen === "download" && picks.length > 1 && (
                  <p className="field-error">{t("backups.restore.downloadOne")}</p>
                )}
                {chosen !== "download" && (
                  <div>
                    <label className="check-line">
                      <input
                        type="checkbox"
                        checked={replaceOn}
                        disabled={picks.length > 0}
                        onChange={(event) =>
                          changed(() => {
                            setMode(chosen);
                            setReplace(event.target.checked);
                          })
                        }
                      />
                      {t("backups.restore.replace")}
                    </label>
                    {picks.length > 0 && (
                      <p className="muted small">{t("backups.restore.replacePicked")}</p>
                    )}
                  </div>
                )}
              </section>
            </fieldset>
            {stale && !record && <Banner tone="info">{t("backups.restore.previewAgain")}</Banner>}
            {record?.status === "previewing" && (
              <Banner tone="info">
                {t("backups.restore.previewing", {
                  where: destinationOf(record),
                  host: server(record.host),
                })}
              </Banner>
            )}
            {record && failedDraft(record) && (
              <Banner tone="err">
                {t("backups.restore.previewFailed", { error: record.error })}
              </Banner>
            )}
            {previewed && <RestorePreview preview={record.preview!} expired={expired} />}
          </>
        )}
        {error && <Banner tone="err">{error}</Banner>}
      </>
    );
    if (chosen === "download") {
      actions = (
        <>
          {cancel}
          <Button
            variant="primary"
            disabled={!ready || busy}
            onClick={() => void download(downloadPath, downloadKind)}
          >
            {t("backups.restore.download")}
          </Button>
        </>
      );
    } else if (previewed && !expired) {
      actions = (
        <>
          {cancel}
          <Button
            variant="primary"
            disabled={busy}
            onClick={() => (needs ? (setConfirmError(""), setConfirming(true)) : void begin(false))}
          >
            {needs ? t("backups.restore.start") : t("backups.restore.startNow")}
          </Button>
        </>
      );
    } else {
      actions = (
        <>
          {cancel}
          <Button
            variant="primary"
            disabled={!ready || busy || record?.status === "previewing"}
            onClick={() => void preview()}
          >
            {t("backups.restore.preview")}
          </Button>
        </>
      );
    }
  }

  return (
    <Drawer
      title={job ? t("backups.restore.title", { job: job.name }) : t("backups.restoreFromBackup")}
      onClose={close}
      actions={actions}
    >
      {body}
      {record && record.preview && (
        <ConfirmOverwriteDialog
          open={confirming}
          dest={destinationOf(record)}
          host={server(record.host)}
          // a whole date reads well in the sentence, as "today" would not in Czech
          time={formatDateTime(record.preview.snapshot.time || record.snapshot_time)}
          busy={busy}
          error={confirmError}
          onCancel={() => setConfirming(false)}
          onConfirm={() => void begin(true)}
        />
      )}
    </Drawer>
  );

  function modeNote(item: RestoreMode, reason: string | undefined): string {
    if (reason === "dump") {
      return t("backups.restore.dumpNoOriginal");
    }
    if (reason === "local") {
      return t("backups.restore.localOnly", {
        target: target?.name ?? "",
        host: server(target?.host),
      });
    }
    if (reason === "admin") {
      return t("backups.restore.adminOnly");
    }
    if (item === "original" && job) {
      return t("backups.restore.mode.original.note", {
        what: describeSource(job, stacks, t),
        host: server(job.host),
      });
    }
    return t(`backups.restore.mode.${item}.note`);
  }
}

/** Of two copies of a record, the one saved last; the second, an answer to
 * what was just asked, when both were saved at once. */
function newer<R extends { updated: string }>(a: R | null, b: R | null): R | null {
  if (!a || !b) {
    return a ?? b;
  }
  return b.updated >= a.updated ? b : a;
}

/** Where a restore writes, as people read it: the volumes or folders of the
 * original location, or the new folder. */
function destinationOf(record: BackupRestore): string {
  if (record.mode === "path") {
    return record.path;
  }
  const fromPreview = (record.preview?.roots ?? []).map((root) => root.destination).filter(Boolean);
  if (fromPreview.length > 0) {
    return fromPreview.join(", ");
  }
  return (record.roots ?? [])
    .map((root) => root.volume || root.path || root.source)
    .filter(Boolean)
    .join(", ");
}

/** The job, the target and the snapshot a restore is of, in a header flow
 * of selects; only what is not picked yet asks. */
function RestorePick({
  jobs,
  job,
  sources,
  target,
  listing,
  snapshotId,
  now,
  disabled,
  onJob,
  onTarget,
  onSnapshot,
}: {
  jobs: BackupJob[];
  job: BackupJob | null;
  sources: { target: BackupTarget; old: boolean }[];
  target: BackupTarget | null;
  listing: Listing<{ snapshots: BackupSnapshot[] }>;
  snapshotId: string;
  now: number;
  disabled: boolean;
  onJob: (id: string) => void;
  onTarget: (id: string) => void;
  onSnapshot: (id: string) => void;
}) {
  const { t } = useTranslation();
  const snapshots = listing.data?.snapshots ?? [];
  return (
    <div className="restore-pick">
      <Field label={t("backups.restore.pick.job")} htmlFor="restore-job">
        <select
          id="restore-job"
          className="input"
          disabled={disabled}
          value={job?.id ?? ""}
          onChange={(event) => onJob(event.target.value)}
        >
          {!job && <option value="">{t("backups.restore.pick.choose")}</option>}
          {jobs.map((item) => (
            <option key={item.id} value={item.id}>
              {item.name}
            </option>
          ))}
        </select>
      </Field>
      {job && (
        <Field label={t("backups.restore.pick.target")} htmlFor="restore-target">
          <select
            id="restore-target"
            className="input"
            disabled={disabled || sources.length === 0}
            value={target?.id ?? ""}
            onChange={(event) => onTarget(event.target.value)}
          >
            {sources.length === 0 && <option value="">{t("backups.snapshot.noTarget")}</option>}
            {sources.map((source) => (
              <option key={source.target.id} value={source.target.id}>
                {source.old
                  ? t("backups.snapshot.oldTarget", { target: source.target.name })
                  : source.target.name}
              </option>
            ))}
          </select>
        </Field>
      )}
      {job && target && (
        <Field label={t("backups.restore.pick.snapshot")} htmlFor="restore-snapshot">
          <select
            id="restore-snapshot"
            className="input mono"
            disabled={disabled || snapshots.length === 0}
            value={snapshots.some((item) => item.id === snapshotId) ? snapshotId : ""}
            onChange={(event) => onSnapshot(event.target.value)}
          >
            <option value="">
              {listing.data
                ? snapshots.length === 0
                  ? t("backups.snapshot.empty", { target: target.name })
                  : t("backups.restore.pick.choose")
                : t("common.loading")}
            </option>
            {snapshots.map((item) => (
              <option key={item.id} value={item.id}>
                {`${item.short_id} · ${formatSnapshotTime(item.time, now, undefined, t)} · ${formatBytes(item.size_bytes)}`}
              </option>
            ))}
          </select>
          {listing.error !== null && (
            <p className="field-error">
              {t("backups.snapshot.failed", { error: errorMessage(listing.error) })}
            </p>
          )}
        </Field>
      )}
    </div>
  );
}

/** What a restore is of: the job, the target, the snapshot and its server. */
function RestoreFacts({
  job,
  target,
  snapshot,
  record,
  hosts,
  now,
  brief = false,
}: {
  job: BackupJob | null;
  target: BackupTarget | null;
  snapshot: BackupSnapshot | null;
  record: BackupRestore | null;
  hosts: Host[];
  now: number;
  /** leaves out the job, the target and the snapshot, which are picked above */
  brief?: boolean;
}) {
  const { t } = useTranslation();
  const id = snapshot?.id ?? record?.snapshot_id ?? "";
  const time = snapshot?.time ?? record?.snapshot_time ?? "";
  return (
    <dl className="facts">
      {!brief && (
        <>
          <dt>{t("backups.restore.facts.job")}</dt>
          <dd>{job?.name ?? "–"}</dd>
          <dt>{t("backups.restore.facts.target")}</dt>
          <dd>{target?.name ?? "–"}</dd>
          <dt>{t("backups.restore.facts.snapshot")}</dt>
          <dd className="mono" title={id}>
            {snapshot?.short_id ?? id.slice(0, 8)}
          </dd>
        </>
      )}
      <dt>{t("backups.restore.facts.time")}</dt>
      <dd>{time ? formatSnapshotTime(time, now, undefined, t) : "–"}</dd>
      {snapshot && (
        <>
          <dt>{t("backups.restore.facts.size")}</dt>
          <dd>{formatBytes(snapshot.size_bytes)}</dd>
          <dt>{t("backups.restore.facts.server")}</dt>
          <dd className="mono">
            {hosts.find((host) => host.id === job?.host)?.name ?? snapshot.hostname}
          </dd>
        </>
      )}
    </dl>
  );
}

/** A folder of a snapshot, a page at a time, to pick items from: a
 * breadcrumb, a filter, and its entries. */
function SnapshotBrowser({
  job,
  target,
  snapshot,
  startPath,
  picks,
  onPicks,
  canDownload,
  onDownload,
}: {
  job: BackupJob;
  target: BackupTarget;
  snapshot: BackupSnapshot;
  startPath: string;
  picks: PickedItem[];
  onPicks: (picks: PickedItem[]) => void;
  canDownload: boolean;
  onDownload: (path: string, kind: "dir" | "file") => void;
}) {
  const { t } = useTranslation();
  const [path, setPath] = useState(startPath);
  const [offset, setOffset] = useState(0);
  const [filter, setFilter] = useState("");
  const [match, setMatch] = useState("");

  // the filter asks a moment after the last key
  useEffect(() => {
    const timer = setTimeout(() => {
      setMatch(filter.trim());
      setOffset(0);
    }, 300);
    return () => clearTimeout(timer);
  }, [filter]);

  const listing = useListing(
    `${job.id}|${target.id}|${snapshot.id}|${path}|${offset}|${match}`,
    () =>
      restoresApi.files(job.id, target.id, snapshot.id, { path, offset, limit: pageSize, match }),
  );
  const page = listing.data;
  const full = picks.length >= maxPicks;

  const open = (next: string) => {
    setPath(next);
    setOffset(0);
    setFilter("");
    setMatch("");
  };

  const toggle = (entry: SnapshotEntry, checked: boolean) => {
    const itemPath = joinPath(path, entry.name);
    onPicks(
      checked
        ? [...picks, { path: itemPath, type: entry.type }]
        : picks.filter((pick) => pick.path !== itemPath),
    );
  };

  let body: ReactNode;
  if (listing.error) {
    body = <Banner tone="err">{errorMessage(listing.error)}</Banner>;
  } else if (!page) {
    body = <p className="muted">{t("backups.files.loading", { path: pathLabel(path, t) })}</p>;
  } else if (page.entries.length === 0) {
    body = (
      <p className="muted">
        {match ? t("backups.files.noMatch", { match }) : t("backups.files.empty")}
      </p>
    );
  } else {
    const to = page.offset + page.entries.length;
    const left = Math.max(0, page.total - to);
    body = (
      <>
        <Table label={t("backups.files.title", { id: snapshot.short_id })} compact>
          <thead>
            <tr>
              <th scope="col">{t("backups.files.column.name")}</th>
              <th scope="col">{t("backups.files.column.size")}</th>
              <th scope="col">{t("backups.files.column.modified")}</th>
              {canDownload && (
                <th scope="col">
                  <span className="visually-hidden">{t("common.actions")}</span>
                </th>
              )}
            </tr>
          </thead>
          <tbody>
            {page.entries.map((entry) => {
              const itemPath = joinPath(path, entry.name);
              const picked = picks.some((pick) => pick.path === itemPath);
              const Icon = entry.type === "dir" ? Folder : File;
              // restic shows a name that is not UTF-8 with U+FFFD: no path names it
              const bad = entry.bad_name || entry.name.includes("\uFFFD");
              return (
                <tr key={entry.name}>
                  <td>
                    <div className="file-name">
                      <label
                        className="check-line file-pick"
                        title={bad ? t("backups.files.badName") : undefined}
                      >
                        <input
                          type="checkbox"
                          aria-label={t("backups.files.pick", { name: entry.name })}
                          checked={picked}
                          disabled={bad || (full && !picked)}
                          onChange={(event) => toggle(entry, event.target.checked)}
                        />
                        <Icon size={14} aria-hidden="true" />
                      </label>
                      {entry.type === "dir" ? (
                        <button
                          type="button"
                          className="link-button mono"
                          onClick={() => open(itemPath)}
                        >
                          {entry.name}
                        </button>
                      ) : (
                        <span className="mono">{entry.name}</span>
                      )}
                    </div>
                  </td>
                  <td className="file-size">
                    {entry.type === "dir" ? "–" : formatBytes(entry.size)}
                  </td>
                  <td className="file-time">{formatDateTime(entry.mtime)}</td>
                  {canDownload && (
                    <td>
                      <IconButton
                        bare
                        aria-label={t("backups.files.download", { name: entry.name })}
                        disabled={bad}
                        onClick={() => onDownload(itemPath, entry.type === "dir" ? "dir" : "file")}
                      >
                        <Download size={14} aria-hidden="true" />
                      </IconButton>
                    </td>
                  )}
                </tr>
              );
            })}
          </tbody>
        </Table>
        <div className="browser-pages">
          <span className="muted small">
            {t("backups.files.range", {
              from: formatNumber(page.offset + 1),
              to: formatNumber(to),
              total: formatNumber(page.total),
            })}
          </span>
          {page.offset > 0 && (
            <Button size="small" onClick={() => setOffset(Math.max(0, page.offset - pageSize))}>
              {t("backups.files.previous")}
            </Button>
          )}
          {left > 0 && (
            <Button size="small" onClick={() => setOffset(to)}>
              {t("backups.files.next", { count: Math.min(pageSize, left) })}
            </Button>
          )}
        </div>
      </>
    );
  }

  return (
    <div className="snapshot-browser">
      <nav aria-label={t("backups.files.path")} className="browser-crumbs">
        <IconButton
          bare
          aria-label={t("backups.files.up")}
          disabled={path === "/"}
          onClick={() => open(parentPath(path))}
        >
          <ArrowUp size={15} aria-hidden="true" />
        </IconButton>
        <ol>
          {pathCrumbs(path).map((crumb, index, all) => (
            <li key={crumb.path}>
              {index === all.length - 1 ? (
                <span aria-current="location">{crumbName(crumb)}</span>
              ) : (
                <button type="button" className="link-button" onClick={() => open(crumb.path)}>
                  {crumbName(crumb)}
                </button>
              )}
            </li>
          ))}
        </ol>
      </nav>
      <div className="browser-tools">
        <input
          className="input"
          type="search"
          aria-label={t("backups.files.filter")}
          placeholder={t("backups.files.filter")}
          value={filter}
          onChange={(event) => setFilter(event.target.value)}
        />
        {canDownload && (
          <Button size="small" onClick={() => onDownload(path, "dir")}>
            <Download size={14} aria-hidden="true" /> {t("backups.restore.downloadFolder")}
          </Button>
        )}
      </div>
      {body}
      <p className="muted small" aria-live="polite">
        {t("backups.files.selected", { count: picks.length })}
        {full && ` ${t("backups.files.limit")}`}
      </p>
    </div>
  );

  function crumbName(crumb: { path: string; name: string; volume: boolean }) {
    if (crumb.path === "/") {
      return t("backups.files.root");
    }
    return crumb.volume ? t("backups.files.volume", { name: crumb.name }) : crumb.name;
  }
}

/** What a preview says the restore would change, as a sentence, with what
 * the destination holds, the containers that stop, and samples. */
export function RestorePreview({ preview, expired }: { preview: Preview; expired: boolean }) {
  const { t } = useTranslation();
  const sentence = previewSentence(t, preview);
  const existing = existingEntries(preview);
  const counts = preview.counts ?? { new: 0, updated: 0, deleted: 0, unchanged: 0 };
  const stops = preview.would_stop ?? [];
  const samples = preview.samples ?? { new: [], updated: [], deleted: [] };
  const truncated = preview.truncated ?? { new: 0, updated: 0, deleted: 0 };
  const lines: { mark: string; kind: string; path: string }[] = [
    ...(samples.new ?? []).map((path) => ({ mark: "+", kind: "add", path })),
    ...(samples.updated ?? []).map((path) => ({ mark: "~", kind: "upd", path })),
    ...(samples.deleted ?? []).map((path) => ({ mark: "−", kind: "del", path })),
  ];
  const more = truncated.new + truncated.updated + truncated.deleted;
  const missing = (preview.roots ?? []).filter((root) => !root.in_snapshot);
  const free = (preview.roots ?? []).find((root) => !root.exists && root.free_bytes > 0);
  const owned = (preview.roots ?? []).find((root) => root.exists && (root.uid > 0 || root.gid > 0));

  return (
    <section className="restore-preview" aria-labelledby="restore-preview-title">
      <h3 id="restore-preview-title" className="drawer-heading">
        {t("backups.restore.preview")}
      </h3>
      {expired && <Banner tone="warn">{t("backups.restore.previewExpired")}</Banner>}
      {sentence && <p className="preview-sentence">{sentence}</p>}
      {existing.count > 0 ? (
        <Banner tone="warn">
          {existing.capped
            ? t("backups.restore.previewExistingMany", { count: formatNumber(existing.count) })
            : t("backups.restore.previewExisting", { count: existing.count })}
        </Banner>
      ) : (
        counts.updated === 0 &&
        counts.deleted === 0 && <Banner tone="ok">{t("backups.restore.previewEmpty")}</Banner>
      )}
      {missing.map((root) => (
        <Banner key={root.index} tone="warn">
          {t("backups.restore.previewMissing", {
            path: pathLabel(root.source || root.destination, t),
          })}
        </Banner>
      ))}
      {(preview.not_in_job ?? []).map((path) => (
        <Banner key={path} tone="info">
          {t("backups.restore.previewNotInJob", { path })}
        </Banner>
      ))}
      {stops.length > 0 ? (
        <div>
          <p>{t("backups.restore.previewStops")}</p>
          <ul className="chip-list restore-stops">
            {stops.map((container) => (
              <li key={container.name} className="chip">
                <span className="mono">{container.name}</span>{" "}
                <ContainerStatePill state="running" health="" />
              </li>
            ))}
          </ul>
        </div>
      ) : (
        <p className="muted small">{t("backups.restore.previewNoStops")}</p>
      )}
      {lines.length > 0 && (
        <div
          className="diff restore-diff"
          tabIndex={0}
          role="region"
          aria-label={t("backups.restore.previewChanges")}
        >
          {lines.map((line) => (
            <div key={`${line.kind}:${line.path}`} className={`diff-line ${line.kind}`}>
              <span aria-hidden="true">{line.mark}</span>
              <span>{line.path}</span>
            </div>
          ))}
          {more > 0 && (
            <div className="diff-line more">
              <span />
              <span>{t("backups.restore.previewMore", { count: more })}</span>
            </div>
          )}
        </div>
      )}
      {free && (
        <p className="muted small">
          {t("backups.restore.previewFree", {
            free: formatBytes(free.free_bytes),
            size: formatBytes(preview.total_bytes),
          })}
        </p>
      )}
      {owned && (
        <p className="muted small">
          {t("backups.restore.previewOwner", { owner: `${owned.uid}:${owned.gid}` })}
        </p>
      )}
      {(preview.notes ?? []).map((note) => (
        <p key={note} className="muted small">
          {note}
        </p>
      ))}
    </section>
  );
}

/** Asks to confirm that a restore overwrites what the destination holds: its
 * button stays disabled until the box is ticked. */
export function ConfirmOverwriteDialog({
  open,
  dest,
  host,
  time,
  busy,
  error,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  dest: string;
  host: string;
  time: string;
  busy: boolean;
  error: string;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const { t } = useTranslation();
  const [checked, setChecked] = useState(false);
  // each time it opens, the box starts empty
  const [opened, setOpened] = useState(open);
  if (open !== opened) {
    setOpened(open);
    if (open) {
      setChecked(false);
    }
  }
  return (
    <ConfirmDialog
      open={open}
      danger
      title={t("backups.restore.confirmTitle", { dest, host })}
      confirmLabel={t("backups.restore.confirmButton")}
      confirmDisabled={!checked || busy}
      onConfirm={() => {
        if (checked) {
          onConfirm();
        }
      }}
      onCancel={onCancel}
    >
      <p className="muted">{t("backups.restore.confirmText", { time })}</p>
      <label className="check-line confirm-check">
        <input
          type="checkbox"
          checked={checked}
          onChange={(event) => setChecked(event.target.checked)}
        />
        {t("backups.restore.confirmCheck", { dest })}
      </label>
      {error && <Banner tone="err">{error}</Banner>}
    </ConfirmDialog>
  );
}

/** How a started restore goes: its steps, the bytes restored, its output,
 * Stop, and how it ended. */
function RestoreProgress({
  record,
  host,
  canStop,
  onChanged,
}: {
  record: BackupRestore;
  host: string;
  canStop: boolean;
  onChanged: (record: BackupRestore) => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const mounted = useMounted();
  const { record: command } = useLiveRecord<Command>("commands", record.command || undefined);
  const [asking, setAsking] = useState(false);
  const [stopping, setStopping] = useState(false);
  const [showOutput, setShowOutput] = useState(true);
  const last = useRef<RestoreStep | "">("");

  const ended = restoreEnded(record.status);
  // whether the destination holds what the restore wrote
  const wrote = record.partial || record.files_restored > 0 || record.files_deleted > 0;
  const progress = command?.progress ?? null;
  const step = (restoreSteps as readonly string[]).includes(progress?.step ?? "")
    ? (progress!.step as RestoreStep)
    : "";
  if (step) {
    last.current = step;
  }
  const current = step || last.current;
  const stops =
    (record.preview?.would_stop?.length ?? 0) > 0 || (record.app_stopped?.length ?? 0) > 0;
  const steps = restoreSteps.filter((item) => stops || (item !== "stop" && item !== "start"));
  const at = current ? steps.indexOf(current as (typeof steps)[number]) : -1;
  const cancelling =
    !ended &&
    (stopping ||
      record.stop_requested_label !== "" ||
      Boolean(command?.cancel_requested_at) ||
      progress?.step === "cancelling");

  const items: TimelineItem[] = steps.map((item, index) => {
    let state: TimelineItem["state"] = "pending";
    if (record.status === "succeeded") {
      state = "done";
    } else if (record.status === "running" || ended) {
      if (index < at) {
        state = "done";
      } else if (index === at) {
        state = ended ? "failed" : "active";
      }
      // the agent starts the application again however the restore ended
      if (ended && item === "start" && (record.app_stopped?.length ?? 0) > 0) {
        state = record.app_problem ? "failed" : "done";
      }
    }
    return { id: item, state, title: t(`backups.restore.step.${item}`) };
  });

  const percent =
    record.status === "succeeded" ? 100 : Math.max(0, Math.min(100, progress?.percent ?? 0));
  const amount = (value: number) =>
    progress?.unit === "files" ? formatNumber(value) : formatBytes(value);
  const notRunning = Array.isArray(command?.result?.app_not_running)
    ? (command!.result!.app_not_running as string[])
    : [];

  const stop = async () => {
    setAsking(false);
    try {
      const answer = await restoresApi.cancel(record.id);
      if ("stopping" in answer) {
        if (mounted.current) {
          setStopping(true);
        }
      } else if (mounted.current) {
        onChanged(answer);
      }
    } catch (err) {
      toast.error(err);
    }
  };

  return (
    <section className="restore-progress" aria-labelledby="restore-progress-title">
      <div className="restore-progress-head">
        <h3 id="restore-progress-title" className="drawer-heading">
          {t("backups.restore.progressLabel")}
        </h3>
        <StatusPill tone={restoreTone(record.status)} dot={!ended}>
          {t(`backups.restore.status.${record.status}`)}
        </StatusPill>
      </div>
      {record.status === "queued" && !cancelling && (
        <p className="muted">{t("backups.restore.queued", { host })}</p>
      )}
      {!ended && (
        <>
          <ProgressBar
            value={percent}
            label={t("backups.restore.progressLabel")}
            warnAt={101}
            errAt={101}
          />
          {(progress?.total ?? 0) > 0 && (
            <p className="muted small">
              {t("backups.restore.progress", {
                done: amount(progress?.done ?? 0),
                total: amount(progress?.total ?? 0),
              })}
            </p>
          )}
        </>
      )}
      <Timeline items={items} label={t("backups.restore.steps")} />
      {cancelling && <Banner tone="info">{t("backups.restore.stopping")}</Banner>}
      {record.status === "succeeded" && (
        <Banner tone="ok">
          {t("backups.restore.done", {
            count: record.files_restored,
            size:
              formatBytes(record.bytes_restored) === "–"
                ? "0 B"
                : formatBytes(record.bytes_restored),
          })}
          {(record.app_stopped?.length ?? 0) > 0 && !record.app_problem && (
            <> {t("backups.restore.appRunning")}</>
          )}
        </Banner>
      )}
      {record.status === "failed" && (
        <Banner tone="err">{t("backups.restore.failed", { error: record.error })}</Banner>
      )}
      {record.status === "cancelled" && (
        <Banner tone="warn">
          {wrote ? (
            <div>
              {t("backups.restore.stopped")}
              {record.error && <span className="restore-why">{record.error}</span>}
            </div>
          ) : (
            // stopped in the queue, or before it wrote: the panel says nothing changed
            record.error || t("backups.restore.stoppedUnchanged")
          )}
        </Banner>
      )}
      {ended && record.partial && <Banner tone="warn">{t("backups.restore.partial")}</Banner>}
      {ended && record.app_problem && (
        <Banner tone="warn">
          {notRunning.length > 0
            ? t("backups.restore.appDown", {
                count: notRunning.length,
                names: notRunning.join(", "),
              })
            : record.app_problem}
        </Banner>
      )}
      {!ended && canStop && !cancelling && (
        <div>
          <Button variant="danger" size="small" onClick={() => setAsking(true)}>
            {t("backups.restore.stop")}
          </Button>
        </div>
      )}
      {record.command && showOutput && (
        <CommandOutput
          commandId={record.command}
          title={t("backups.restore.output")}
          status={
            ended || !command ? (
              <StatusPill tone={restoreTone(record.status)} dot={false}>
                {t(`backups.restore.status.${record.status}`)}
              </StatusPill>
            ) : (
              <CommandStatusPill status={command.status} />
            )
          }
          onClose={() => setShowOutput(false)}
        />
      )}
      <ConfirmDialog
        open={asking}
        danger
        title={t("backups.restore.stopTitle")}
        confirmLabel={t("backups.restore.stop")}
        onCancel={() => setAsking(false)}
        onConfirm={() => void stop()}
      >
        <p className="muted">{t("backups.restore.stopText")}</p>
      </ConfirmDialog>
    </section>
  );
}

/** How a download goes: the bytes sent so far, and how it ended. */
function DownloadStatus({
  record,
  host,
  frameError,
}: {
  record: BackupRestore;
  host: string;
  frameError: string;
}) {
  const { t } = useTranslation();
  const { record: command } = useLiveRecord<Command>("commands", record.command || undefined);
  const ended = restoreEnded(record.status);
  const done = command?.progress?.done ?? 0;
  const total = command?.progress?.total ?? 0;

  let state: ReactNode;
  if (frameError) {
    state = (
      <Banner tone="err">{t("backups.restore.downloadFailed", { error: frameError })}</Banner>
    );
  } else if (record.status === "succeeded") {
    state = (
      <Banner tone="ok">
        {t("backups.restore.downloaded", { size: formatBytes(record.bytes || done) })}
      </Banner>
    );
  } else if (ended) {
    state = (
      <Banner tone="err">
        {t("backups.restore.downloadFailed", { error: record.error || t("common.unknown") })}
      </Banner>
    );
  } else if (done > 0) {
    state = (
      <>
        <p>
          {total > 0
            ? t("backups.restore.downloadingOf", {
                size: formatBytes(done),
                total: formatBytes(total),
              })
            : t("backups.restore.downloading", { size: formatBytes(done) })}
        </p>
        {total > 0 && (
          <ProgressBar
            value={Math.min(100, (done / total) * 100)}
            label={t("backups.restore.downloadProgress")}
            warnAt={101}
            errAt={101}
          />
        )}
      </>
    );
  } else {
    state = (
      <>
        <p className="muted">{t("backups.restore.queued", { host })}</p>
        <p className="muted small">{t("backups.restore.downloadNotStarted")}</p>
      </>
    );
  }

  return (
    <section className="restore-download" aria-live="polite">
      <div className="restore-progress-head">
        <span className="mono">{record.download_name || record.path}</span>
        <StatusPill
          tone={frameError ? "err" : restoreTone(record.status)}
          dot={!ended && !frameError}
        >
          {t(`backups.restore.status.${frameError ? "failed" : record.status}`)}
        </StatusPill>
      </div>
      {state}
      <p className="muted small">{t("backups.restore.downloadNote")}</p>
    </section>
  );
}
