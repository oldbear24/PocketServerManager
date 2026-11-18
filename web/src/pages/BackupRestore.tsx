import { ArrowUp, Download, File, Folder, X } from "lucide-react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";

import i18n from "../i18n";
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
  offeredModes,
  openDownloadFrame,
  parentPath,
  pathCrumbs,
  pathLabel,
  previewSentence,
  restoreDraft,
  restoreEnded,
  restoresApi,
  restoreSteps,
  stepsOf,
  restoreTone,
  roomEnough,
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
import { reservedName } from "../lib/databaseBackups";
import { databaseNamePattern, type Database, type DbServer } from "../lib/databases";
import { formatBytes, formatDateTime, formatNumber, parseDate } from "../lib/format";
import { useMounted } from "../lib/mounted";
import { pb } from "../lib/pb";
import { useLiveList, useLiveRecord, useNow } from "../lib/realtime";
import { panelFileName } from "../lib/recovery";
import type { Site } from "../lib/sites";
import type { Command, Host } from "../lib/types";
import { describeSource, type StackName } from "./BackupJobs";
import { ServerField } from "./Backups";
import { PanelRecoverySteps } from "./PanelRecoverySteps";

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
  // a new database's name, and its server when the job's database was dropped (#53)
  const [newName, setNewName] = useState("");
  const [newServer, setNewServer] = useState("");

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
    if (record.mode === "new_database") {
      setNewName(record.database_name ?? "");
    }
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
  const jobHost = hosts.find((item) => item.id === job?.host);
  const modes = job ? modesFor(target, job, user?.role, jobHost) : null;
  // the panel's own backup restores nowhere here: the drawer gives its steps
  const panelJob = job?.source_kind === "panel";
  // a database's job (#53): into its database, a new one, a folder or a download
  const databaseJob = job?.source_kind === "database";
  const offered = job ? offeredModes(job) : [];
  const chosen: RestoreMode | "" =
    mode !== "" && modes && offered.includes(mode) && !modes[mode].disabled
      ? mode
      : modes
        ? (offered.find((item) => !modes[item].disabled) ?? "")
        : "";
  // what a database's restore names: the database, its server and its application
  const dbName = databaseJob ? (job?.source ?? "") : "";
  const dbServerName = databaseJob ? job?.db_server_name || "–" : "";
  const { record: dbRecord } = useLiveRecord<Database>(
    "databases",
    databaseJob && job?.database ? job.database : undefined,
  );
  const { record: dbSite } = useLiveRecord<Site>("sites", dbRecord?.site || undefined);
  const appName =
    (dbRecord?.stack && stacks.find((item) => item.id === dbRecord.stack)?.name) ||
    dbSite?.name ||
    "";
  // a dropped database goes into a new one on a shared server of its engine there
  const dbGone = databaseJob && !job?.database;
  const newServers = useLiveList<DbServer>("db_servers", {
    filter: pb.filter("host = {:host} && kind = 'shared'", { host: job?.host ?? "" }),
    sort: "name",
    enabled: dbGone && chosen === "new_database",
  }).items.filter(
    (item) =>
      item.host === job?.host &&
      item.kind === "shared" &&
      item.engine === (job?.engine === "mysql" ? "mariadb" : "postgres"),
  );
  const newServerValue = newServers.some((item) => item.id === newServer)
    ? newServer
    : (newServers[0]?.id ?? "");
  const newNameProblem =
    newName.trim() === ""
      ? ""
      : reservedName(newName.trim())
        ? t("backups.restore.newNameReserved")
        : databaseNamePattern.test(newName.trim())
          ? ""
          : t("db.databaseNameInvalid");
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
    (chosen !== "download" || picks.length <= 1) &&
    (chosen !== "new_database" ||
      (newName.trim() !== "" && newNameProblem === "" && (!dbGone || newServerValue !== "")));

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
      const created = await restoresApi.create(
        job!.id,
        chosen === "database" || chosen === "new_database"
          ? {
              target: target!.id,
              snapshot: snapshot!.id,
              mode: chosen,
              ...(chosen === "new_database"
                ? { name: newName.trim(), ...(dbGone ? { server: newServerValue } : {}) }
                : {}),
            }
          : {
              target: target!.id,
              snapshot: snapshot!.id,
              mode: chosen as "original" | "path",
              ...(chosen === "path"
                ? { host: hostValue, path: folder.trim(), source: sourceValue }
                : {}),
              ...(picks.length > 0 ? { include: picks.map((pick) => pick.path) } : {}),
              replace: replaceOn,
            },
      );
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

  const begin = async (confirm: boolean, typed?: string) => {
    if (!record || busy) {
      return;
    }
    setBusy(true);
    setError("");
    setConfirmError("");
    try {
      // only a restore back into a database sends the name typed
      const queued =
        typed === undefined
          ? await restoresApi.start(record.id, confirm)
          : await restoresApi.start(record.id, confirm, typed);
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

  const download = async (path: string, kind: "dir" | "file" | "sql") => {
    if (!job || !target || !snapshot || busy) {
      return;
    }
    setBusy(true);
    setError("");
    setFrameError("");
    try {
      // a database's dump as SQL needs no path: the panel knows its file (#53)
      const answer = await restoresApi.download(
        job.id,
        kind === "sql"
          ? { target: target.id, snapshot: snapshot.id, kind }
          : { target: target.id, snapshot: snapshot.id, path, kind },
      );
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
        {panelJob && job && modes && (
          // the panel is restored on a machine of its own (#32): no modes,
          // but the steps, filled from what is picked
          <>
            {target && snapshot && (
              <RestoreFacts
                job={job}
                target={target}
                snapshot={snapshot}
                record={record}
                hosts={hosts}
                now={now}
                brief={!record}
              />
            )}
            <p className="muted">{modeNote("original", modes.original.reason)}</p>
            <PanelRecoverySteps target={target} snapshot={snapshot} hosts={hosts} />
          </>
        )}
        {!panelJob && job && target && snapshot && modes && (
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
              {!databaseJob && (
                // a database's snapshot holds its one dump: nothing to pick
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
                              changed(() =>
                                setPicks(picks.filter((item) => item.path !== pick.path)),
                              )
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
              )}
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
                  options={offered.map((item) => {
                    // a database's download is its dump as plain SQL
                    const sql = databaseJob && item === "download";
                    return {
                      value: item,
                      title: sql
                        ? t("backups.restore.sqlLabel")
                        : t(`backups.restore.mode.${item}.label`),
                      note:
                        sql && !modes[item].reason
                          ? t("backups.restore.sqlNote")
                          : modeNote(item, modes[item].reason),
                      disabled: modes[item].disabled,
                    };
                  })}
                />
                {chosen === "new_database" && (
                  <>
                    <Field
                      label={t("backups.restore.newName")}
                      htmlFor="restore-new-name"
                      error={newNameProblem || undefined}
                    >
                      <input
                        id="restore-new-name"
                        className="input mono"
                        autoComplete="off"
                        spellCheck={false}
                        maxLength={63}
                        placeholder={`${dbName}_restored`}
                        aria-invalid={newNameProblem !== ""}
                        value={newName}
                        onChange={(event) => changed(() => setNewName(event.target.value))}
                      />
                      <p className="muted small">{t("backups.restore.newNameHint")}</p>
                    </Field>
                    {dbGone && (
                      <Field label={t("backups.restore.newServer")} htmlFor="restore-new-server">
                        <select
                          id="restore-new-server"
                          className="input"
                          value={newServerValue}
                          onChange={(event) => changed(() => setNewServer(event.target.value))}
                        >
                          {newServers.map((item) => (
                            <option key={item.id} value={item.id}>
                              {item.name}
                            </option>
                          ))}
                        </select>
                      </Field>
                    )}
                  </>
                )}
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
                {(chosen === "original" || chosen === "path") && (
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
            {previewed &&
              (record.mode === "database" || record.mode === "new_database" ? (
                <DatabasePreview
                  record={record}
                  preview={record.preview!}
                  expired={expired}
                  server={dbServerName}
                />
              ) : (
                <RestorePreview preview={record.preview!} expired={expired} />
              ))}
          </>
        )}
        {error && <Banner tone="err">{error}</Banner>}
      </>
    );
    // what the panel's start refuses from a database's preview: the dump is
    // not in the snapshot, the user's password no longer signs in, the room
    // is short, or the new name is taken
    const refused =
      previewed && (record.mode === "database" || record.mode === "new_database")
        ? databaseRefusal(record.mode, record.preview!)
        : false;
    if (panelJob) {
      // an admin downloads the encrypted copy, a valid input of panel recover
      actions = (
        <>
          {cancel}
          {modes?.download.disabled === false && (
            <Button
              variant="primary"
              disabled={!target || !snapshot || busy}
              onClick={() => void download(`/${panelFileName}`, "file")}
            >
              <Download size={15} aria-hidden="true" />
              {t("backups.restore.panel.download")}
            </Button>
          )}
        </>
      );
    } else if (chosen === "download" && databaseJob) {
      // a database's dump as plain SQL; PostgreSQL's as stored too, for pg_restore
      actions = (
        <>
          {cancel}
          {job?.engine === "postgres" && (
            <Button
              disabled={!ready || busy}
              onClick={() => void download(`/${dbName}.pgdump`, "file")}
            >
              {t("backups.restore.rawLabel")}
            </Button>
          )}
          <Button
            variant="primary"
            disabled={!ready || busy}
            onClick={() => void download("", "sql")}
          >
            <Download size={15} aria-hidden="true" />
            {t("backups.restore.sqlLabel")}
          </Button>
        </>
      );
    } else if (chosen === "download") {
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
            disabled={busy || refused}
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
      {record && record.preview && record.mode === "database" && (
        <ConfirmDatabaseDialog
          open={confirming}
          database={record.database_name || dbName}
          // a whole date reads well in the sentence, as "today" would not in Czech
          time={formatDateTime(record.preview.snapshot.time || record.snapshot_time)}
          stops={record.preview.would_stop?.length ?? 0}
          busy={busy}
          error={confirmError}
          onCancel={() => setConfirming(false)}
          onConfirm={(typed) => void begin(true, typed)}
        />
      )}
      {record && record.preview && record.mode !== "database" && (
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
    if (reason === "panel") {
      return t("backups.restore.panelNoOriginal");
    }
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
    if (reason === "dbGone") {
      return t("backups.restore.dbGone");
    }
    if (reason === "agent") {
      return t("backups.restore.agentNoDatabase", { host: server(job?.host) });
    }
    if (item === "original" && job) {
      return t("backups.restore.mode.original.note", {
        what: describeSource(job, stacks, t),
        host: server(job.host),
      });
    }
    if (item === "database") {
      // "no application" begins a sentence of the note
      const app = appName || capitalized(t("backups.restore.databaseNoApp"));
      return t("backups.restore.mode.database.note", {
        database: dbName,
        server: dbServerName,
        app,
      });
    }
    if (item === "new_database") {
      return t("backups.restore.mode.new_database.note", { server: dbServerName });
    }
    return t(`backups.restore.mode.${item}.note`);
  }
}

/** A text with its first letter in capitals, as the language writes it. */
function capitalized(text: string): string {
  const language = i18n.language || "en";
  return `${text.charAt(0).toLocaleUpperCase(language)}${text.slice(1)}`;
}

/** Whether the panel's start refuses a database's restore from its preview:
 * the dump is not in the snapshot, the panel's password of the user no
 * longer signs in (back into the database), the room is short, or a
 * database or user of the new name exists already. */
export function databaseRefusal(mode: RestoreMode, preview: Preview): boolean {
  const missing = (preview.roots ?? []).some((root) => !root.in_snapshot);
  const database = preview.database;
  if (missing || !roomEnough(preview)) {
    return true;
  }
  if (mode === "database") {
    return database ? !database.sign_in : false;
  }
  return database ? database.exists || database.user_exists : false;
}

/** What a database's preview found (#53): the dump's size, the tables the
 * database has now, the room it needs, what keeps it from starting, what
 * stops, and whether the data as it is now stays. */
export function DatabasePreview({
  record,
  preview,
  expired,
  server,
}: {
  record: Pick<BackupRestore, "mode" | "database_name" | "previous" | "roots" | "stack_name">;
  preview: Preview;
  expired: boolean;
  /** the database's server */
  server: string;
}) {
  const { t } = useTranslation();
  const root = record.roots?.[0];
  const database = record.database_name || root?.database || "";
  const user = root?.user || database;
  const found = preview.database;
  const replace = record.mode === "database";
  const missing = (preview.roots ?? []).some((item) => !item.in_snapshot);
  const stops = preview.would_stop ?? [];
  // the database's own application stops, as its mode says; what else stops
  // is another stack whose settings name the database
  const own = new Set(root?.stop_containers ?? []);
  const others = stops.filter(
    (container) =>
      !own.has(container.name) && (!record.stack_name || container.project !== record.stack_name),
  );
  const previous = record.previous || root?.previous || "";
  // PostgreSQL keeps the data a restore in place replaced; MariaDB does not
  const postgres = (root?.engine ?? "") === "postgres" || previous !== "";
  return (
    <section className="restore-preview" aria-labelledby="restore-preview-title">
      <h3 id="restore-preview-title" className="drawer-heading">
        {t("backups.restore.preview")}
      </h3>
      {expired && <Banner tone="warn">{t("backups.restore.previewExpired")}</Banner>}
      {missing ? (
        <Banner tone="err">{t("backups.restore.previewDumpMissing")}</Banner>
      ) : (
        <p className="preview-sentence">
          {t("backups.restore.previewDump", { size: formatBytes(preview.total_bytes) })}
        </p>
      )}
      {replace && found?.exists && (
        <p>{t("backups.restore.previewTables", { database, count: found.tables })}</p>
      )}
      {!replace && found && (found.exists || found.user_exists) && (
        <Banner tone="err">{t("backups.restore.previewExists", { database, server })}</Banner>
      )}
      {replace && found && !found.sign_in && (
        <Banner tone="err">{t("backups.restore.previewSignIn", { user, server })}</Banner>
      )}
      {preview.free && preview.needed && (
        <p className="muted small">
          {t("backups.restore.previewDbFree", {
            free: formatBytes(preview.free.data),
            server,
            needed: formatBytes(preview.needed.data),
          })}
        </p>
      )}
      {!roomEnough(preview) && (
        <Banner tone="err">{t("backups.restore.previewLowSpace", { server })}</Banner>
      )}
      {stops.length > 0 && (
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
          {others.length > 0 && (
            <p className="muted small">
              {t("backups.restore.alsoStops", {
                names: others.map((container) => container.name).join(", "),
              })}
            </p>
          )}
        </div>
      )}
      {replace &&
        (postgres ? (
          <p className="muted small">
            {t("backups.restore.previous", { server, name: previous || "pc_old_…" })}
          </p>
        ) : (
          <Banner tone="warn">{t("backups.restore.noPrevious")}</Banner>
        ))}
      {(found?.settings ?? []).length > 0 && (
        <p className="muted small mono">{(found?.settings ?? []).join(", ")}</p>
      )}
      {(preview.notes ?? []).map((note) => (
        <p key={note} className="muted small">
          {note}
        </p>
      ))}
    </section>
  );
}

/** Asks to confirm that a restore replaces a database: its button stays
 * disabled until the database's name is typed, and the panel checks the
 * name again. */
export function ConfirmDatabaseDialog({
  open,
  database,
  time,
  stops,
  busy,
  error,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  database: string;
  time: string;
  /** the containers that stop meanwhile */
  stops: number;
  busy: boolean;
  error: string;
  onConfirm: (typed: string) => void;
  onCancel: () => void;
}) {
  const { t } = useTranslation();
  const [typed, setTyped] = useState("");
  // each time it opens, the field starts empty
  const [opened, setOpened] = useState(open);
  if (open !== opened) {
    setOpened(open);
    if (open) {
      setTyped("");
    }
  }
  const matches = database !== "" && typed === database;
  return (
    <ConfirmDialog
      open={open}
      danger
      title={t("backups.restore.confirmDatabase", { database })}
      confirmLabel={t("backups.restore.confirmDatabaseAction", { database })}
      confirmDisabled={!matches || busy}
      onConfirm={() => {
        if (matches) {
          onConfirm(typed);
        }
      }}
      onCancel={onCancel}
    >
      <div className="dialog-form">
        <p className="muted">
          {t("backups.restore.confirmDatabaseText", { database, time, count: stops })}
        </p>
        <Field
          label={t("backups.restore.confirmType", { database })}
          htmlFor="restore-confirm-name"
        >
          <input
            id="restore-confirm-name"
            className="input mono"
            autoComplete="off"
            spellCheck={false}
            value={typed}
            onChange={(event) => setTyped(event.target.value)}
          />
        </Field>
        {error && <Banner tone="err">{error}</Banner>}
      </div>
    </ConfirmDialog>
  );
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
  // PostgreSQL's restore in place puts its restored copy in place (#53)
  const swaps =
    record.mode === "database" && Boolean(record.previous || record.roots?.[0]?.previous);
  const steps = stepsOf(record.mode, record.roots?.[0]?.engine).filter(
    (item) => (stops || (item !== "stop" && item !== "start")) && (swaps || item !== "swap"),
  );
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
      // the agent starts the application again however the restore ended,
      // but after MariaDB's database was emptied it leaves it stopped (#53)
      if (ended && item === "start" && (record.app_stopped?.length ?? 0) > 0) {
        const leftStopped = record.mode === "database" && record.partial;
        state = record.app_problem || leftStopped ? "failed" : "done";
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
  // a database's restore (#53): the tables it restored, and what stays
  // stopped when MariaDB's restore failed after the database was emptied
  const database = record.mode === "database" || record.mode === "new_database";
  const tables =
    record.tables ||
    (typeof command?.result?.tables === "number" ? (command.result.tables as number) : 0);
  const leftStopped = Array.isArray(command?.result?.app_left_stopped)
    ? (command!.result!.app_left_stopped as string[])
    : (record.app_stopped ?? []).map((item) => item.name);

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
          {database
            ? // a sentence of its own, as the containers' after it
              `${t("backups.restore.tablesDone", { count: tables })}.`
            : t("backups.restore.done", {
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
      {ended &&
        record.partial &&
        (database ? (
          <Banner tone="err">
            {t("backups.restore.appLeftStopped", {
              names: leftStopped.join(", ") || "–",
              database: record.database_name || record.roots?.[0]?.database || "",
            })}
          </Banner>
        ) : (
          <Banner tone="warn">{t("backups.restore.partial")}</Banner>
        ))}
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
