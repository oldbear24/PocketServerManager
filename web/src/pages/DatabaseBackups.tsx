// The Databáze page's backups (#53): a database's Backup cell, its card of
// backups and restore tests, a temporary database's card, and a server's
// defaults for its databases' jobs.
import { lazy, Suspense, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { useToast } from "../components/toast";
import { Banner, Button, Card, ConfirmDialog, Field, StatusPill } from "../components/ui";
import { errorMessage } from "../lib/api";
import { formatCheckMoment, type BackupCheck } from "../lib/backupChecks";
import {
  cronShaped,
  describeCron,
  describeRetention,
  fullRetention,
  jobsApi,
  runMoment,
  sourceGone,
  zoneOffset,
  type BackupJob,
  type BackupRun,
  type OwnSetting,
} from "../lib/backupJobs";
import type { BackupTarget } from "../lib/backups";
import {
  autoSchedule,
  backupCell,
  cellTime,
  dailyTime,
  latestTest,
  nextCron,
  temporaryKind,
  temporaryOwner,
} from "../lib/databaseBackups";
import { dbApi, type Database } from "../lib/databases";
import { formatBytes, formatDateTime, formatRelative } from "../lib/format";
import { useMounted } from "../lib/mounted";
import type { Host } from "../lib/types";
import {
  defaultRetention,
  JobDialog,
  keepForm,
  parsedRetention,
  RetentionFields,
  TargetPicker,
  type KeepForm,
  type StackName,
} from "./BackupJobs";

// the restore drawer, with its file browser, loads when it opens
const RestoreDrawer = lazy(() =>
  import("./BackupRestore").then((module) => ({ default: module.RestoreDrawer })),
);

/** A database's Backup cell: how its job's last backup went, linking to the
 * job; a small red line when its newest restore test that ended failed. A
 * dedicated server's database shows its stack's job, by name. Without a
 * job, as a temporary database, a dash. */
export function DatabaseBackupCell({
  job,
  runs,
  checks = [],
  now,
  via = false,
}: {
  job: BackupJob | null;
  runs: BackupRun[];
  checks?: BackupCheck[];
  now: number;
  /** the stack's job of a dedicated server */
  via?: boolean;
}) {
  const { t } = useTranslation();
  if (!job) {
    return <span className="faint">—</span>;
  }
  const cell = backupCell(job, runs);
  const text = t(`db.backupCell.${cell.state}`, { time: cellTime(cell.at, now) });
  const failed = latestTest(checks, job.id, true)?.status === "failed";
  return (
    <span className="db-backup-cell">
      <Link className="db-backup-link" to={`/backups?job=${job.id}`}>
        {cell.tone === "muted" ? (
          <span className="faint">{text}</span>
        ) : (
          <StatusPill tone={cell.tone}>{text}</StatusPill>
        )}
      </Link>
      {failed && <span className="db-backup-test">{t("db.backupCell.testFailed")}</span>}
      {via && (
        <span className="cell-sub" title={t("db.backupCell.viaJobNote")}>
          {t("db.backupCell.viaJob", { name: job.name })}
        </span>
      )}
    </span>
  );
}

/** The temporary database's line in the App column: whose copy it is and
 * when it is dropped, or that it is being dropped. */
export function temporaryLabel(
  database: Pick<Database, "name" | "temporary_until" | "status">,
  owners: { tests: BackupCheck[]; restores: { id: string; database_name?: string }[] | null },
  now: number,
  t: (key: string, options?: Record<string, unknown>) => string,
): string {
  if (database.status === "dropping") {
    return t("db.temporary.dropping");
  }
  const when = formatRelative(database.temporary_until, now);
  const kind = temporaryKind(database.name);
  const owner = temporaryOwner(database.name);
  if (kind === "test") {
    const test = owners.tests.find((item) => item.id === owner);
    return test
      ? t("db.temporary.test", { name: test.database_name || "–", when })
      : t("db.temporary.orphan", { when });
  }
  if (kind === "scratch" || kind === "previous") {
    // a viewer reads no restores: the copy's database is then not known
    const restore = owners.restores?.find((item) => item.id === owner);
    if (owners.restores && !restore) {
      return t("db.temporary.orphan", { when });
    }
    const name = restore?.database_name || "–";
    return kind === "scratch"
      ? t("db.temporary.scratch", { name })
      : t("db.temporary.previous", { name, when });
  }
  return t("db.temporary.orphan", { when });
}

/** A temporary database, picked in the table: what it is, when the panel
 * drops it, and Drop now for operators. */
export function TemporaryDatabaseCard({
  database,
  canAct,
}: {
  database: Database;
  canAct: boolean;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);

  const drop = async () => {
    setAsking(false);
    setBusy(true);
    try {
      await dbApi.drop(database.id);
      toast.success(t("db.temporary.dropQueued", { name: database.name }));
    } catch (err) {
      toast.error(err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card title={t("db.temporary.title", { name: database.name })}>
      <p className="muted">
        {t("db.temporary.text", { at: formatDateTime(database.temporary_until) })}
      </p>
      {canAct && database.status !== "dropping" && (
        <div className="button-row">
          <Button size="small" variant="danger" disabled={busy} onClick={() => setAsking(true)}>
            {t("db.temporary.drop")}
          </Button>
        </div>
      )}
      <ConfirmDialog
        open={asking}
        danger
        title={t("db.temporary.dropConfirm", { name: database.name })}
        confirmLabel={t("db.temporary.drop")}
        onCancel={() => setAsking(false)}
        onConfirm={() => void drop()}
      />
    </Card>
  );
}

/** Whether a setting of an automatic job follows its server, as its card
 * says it. */
function followText(
  job: Pick<BackupJob, "own_settings">,
  field: OwnSetting,
  host: string,
  t: (key: string, options?: Record<string, unknown>) => string,
): string {
  return (job.own_settings ?? []).includes(field)
    ? t("db.backups.own")
    : t("db.backups.follows", { host });
}

/** A database's backups, under its connection (#53): its job, when, where
 * and how long, its last backup and its newest restore test; operators back
 * up, test and restore now, admins change the job. A dedicated server's
 * database says its stack's job backs it up. */
export function DatabaseBackupCard({
  database,
  dedicated,
  job,
  runs,
  checks,
  targets,
  hosts,
  stacks,
  jobs,
  testSchedule,
  now,
  canAct,
  isAdmin,
}: {
  database: Database;
  dedicated: boolean;
  /** its own job; of a dedicated server's database, its stack's */
  job: BackupJob | null;
  runs: BackupRun[];
  checks: BackupCheck[];
  targets: BackupTarget[];
  hosts: Host[];
  stacks: StackName[];
  /** every job the restore drawer may show */
  jobs: BackupJob[];
  /** the restore tests' schedule; undefined on a panel that has none */
  testSchedule: string | undefined;
  now: number;
  canAct: boolean;
  isAdmin: boolean;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const mounted = useMounted();
  const [busy, setBusy] = useState("");
  const [restoring, setRestoring] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);
  const offset = zoneOffset(new Date(now));

  if (dedicated) {
    return (
      <Card title={t("db.backups.title")}>
        {job ? (
          <>
            <p className="muted">{t("db.backups.dedicated")}</p>
            <DatabaseBackupCell job={job} runs={runs} now={now} via />
          </>
        ) : (
          <p className="muted">{t("db.backups.dedicatedNone")}</p>
        )}
      </Card>
    );
  }
  if (!job || database.status !== "ready") {
    return (
      <Card title={t("db.backups.title")}>
        <p className="muted">{t("db.backups.pending")}</p>
      </Card>
    );
  }

  const host = hosts.find((item) => item.id === job.host)?.name ?? "–";
  const names = (job.targets ?? [])
    .map((id) => targets.find((target) => target.id === id)?.name)
    .filter(Boolean);
  const succeeded = runs
    .filter((run) => run.job === job.id && run.status === "succeeded")
    .sort((a, b) => (a.created < b.created ? 1 : -1))[0];
  const test = latestTest(checks, job.id);
  const running = job.enabled && !sourceGone(job);

  let last: string;
  if (succeeded) {
    last = t("db.backups.lastText", {
      when: cellTime(runMoment(succeeded), now),
      target: targets.find((target) => target.id === succeeded.target)?.name ?? "–",
      size: formatBytes(succeeded.total_bytes),
      added: formatBytes(succeeded.bytes_added),
    });
  } else if (job.last_success_at) {
    last = cellTime(job.last_success_at, now);
  } else {
    last = t("db.backups.never");
  }

  let testText: string;
  if (test) {
    const when = formatCheckMoment(test.finished_at || test.started_at || test.created, now);
    switch (test.status) {
      case "ok":
        testText = t("db.backups.testState.ok", { when, count: test.tables_restored ?? 0 });
        break;
      case "failed":
      case "error":
      case "skipped":
        testText = t(`db.backups.testState.${test.status}`, { when, error: test.error || "–" });
        break;
      case "running":
        testText = t("db.backups.testState.running", { name: test.temporary || "–" });
        break;
      case "queued":
        testText = t("db.backups.testState.queued");
        break;
      default:
        testText = t("db.backups.testState.cancelled", { when });
    }
  } else if (testSchedule === undefined || testSchedule.trim() === "") {
    testText = t("db.backups.testOff");
  } else {
    const next = nextCron(testSchedule, now);
    testText = t("db.backups.testNone", {
      when: next === null ? "–" : formatCheckMoment(new Date(next).toISOString(), now),
    });
  }

  const act = async (what: string, action: () => Promise<string>) => {
    setBusy(what);
    try {
      toast.success(await action());
    } catch (err) {
      toast.error(err);
    } finally {
      if (mounted.current) {
        setBusy("");
      }
    }
  };

  return (
    <Card title={t("db.backups.title")}>
      <dl className="facts">
        <dt>{t("db.backups.job")}</dt>
        <dd>
          <Link to={`/backups?job=${job.id}`}>{job.name}</Link>
        </dd>
        <dt>{t("db.backups.schedule")}</dt>
        <dd>
          {describeCron(job.schedule, t, offset)}
          <span className="faint"> · {followText(job, "schedule", host, t)}</span>
        </dd>
        <dt>{t("db.backups.targets")}</dt>
        <dd>
          {names.length > 0 ? (
            names.join(" + ")
          ) : (
            <span className="warn-text">{t("db.backupCell.noTarget")}</span>
          )}
          <span className="faint"> · {followText(job, "targets", host, t)}</span>
        </dd>
        <dt>{t("db.backups.retention")}</dt>
        <dd>
          <span className="mono">{describeRetention(job.retention, t)}</span>
          <span className="faint"> · {followText(job, "retention", host, t)}</span>
        </dd>
        <dt>{t("db.backups.last")}</dt>
        <dd>
          <div>{last}</div>
          <DatabaseBackupCell job={job} runs={runs} checks={checks} now={now} />
        </dd>
        <dt>{t("db.backups.test")}</dt>
        <dd className={test?.status === "failed" ? "err-text" : undefined}>{testText}</dd>
      </dl>
      {/* operators act; an admin, who is one too, also changes the job */}
      {canAct && (
        <div className="button-row">
          {running && (
            <Button
              size="small"
              disabled={busy !== "" || names.length === 0}
              onClick={() =>
                void act("run", async () => {
                  const answer = await jobsApi.run(job.id);
                  return t("backups.job.started", { name: job.name, count: answer.runs.length });
                })
              }
            >
              {t("db.backups.backupNow")}
            </Button>
          )}
          {running && (
            <Button
              size="small"
              disabled={busy !== ""}
              onClick={() =>
                void act("test", async () => {
                  await jobsApi.restoreTest(job.id);
                  return t("db.backups.testQueued");
                })
              }
            >
              {t("db.backups.testNow")}
            </Button>
          )}
          <Button size="small" onClick={() => setRestoring("new")}>
            {t("db.backups.restore")}
          </Button>
          {isAdmin && (
            <Button size="small" onClick={() => setEditing(true)}>
              {t("db.backups.edit")}
            </Button>
          )}
        </div>
      )}
      {canAct && restoring && (
        <Suspense fallback={null}>
          <RestoreDrawer
            jobs={jobs}
            targets={targets}
            hosts={hosts}
            runs={runs}
            stacks={stacks}
            start={{ job: job.id, target: job.targets?.[0], restore: restoring }}
            onRecord={(id) => setRestoring(id ?? "new")}
            onClose={() => setRestoring(null)}
          />
        </Suspense>
      )}
      {isAdmin && editing && (
        <JobDialog
          job={job}
          targets={targets}
          hosts={hosts}
          stacks={stacks}
          onClose={() => setEditing(false)}
        />
      )}
    </Card>
  );
}

/** A server's defaults for its databases' and sites' jobs (#53): when, in
 * cron and UTC (empty is the panel's own, at night), where, and how long;
 * each job takes what it does not set itself. Admins only. */
export function HostBackupsDialog({
  host,
  jobs,
  targets,
  hosts,
  onClose,
}: {
  host: Host;
  /** the automatic jobs of the server */
  jobs: BackupJob[];
  targets: BackupTarget[];
  hosts: Host[];
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const mounted = useMounted();
  const [schedule, setSchedule] = useState(host.auto_backup_schedule ?? "");
  const [chosen, setChosen] = useState<string[]>(host.auto_backup_targets ?? []);
  const [keep, setKeep] = useState<KeepForm>(() =>
    keepForm(
      host.auto_backup_retention ? fullRetention(host.auto_backup_retention) : defaultRetention,
    ),
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const offset = zoneOffset();

  // a local target is a directory of its own server: only that server's jobs
  const fits = (target: BackupTarget) => target.kind !== "local" || target.host === host.id;
  const serverName = (id: string) => hosts.find((item) => item.id === id)?.name ?? "–";
  const retention = parsedRetention(keep);
  const text = schedule.trim();
  const scheduleValid = text === "" || cronShaped(text);
  const follow = jobs.filter(
    (job) => job.host === host.id && !sourceGone(job) && (job.own_settings ?? []).length < 3,
  ).length;
  const ready = scheduleValid && retention !== null && !busy;

  const save = async () => {
    if (!ready || retention === null) {
      return;
    }
    setBusy(true);
    setError("");
    try {
      const answer = await jobsApi.hostDefaults(host.id, {
        schedule: text,
        targets: targets
          .filter((target) => chosen.includes(target.id) && fits(target))
          .map((target) => target.id),
        retention,
      });
      toast.success(t("db.hostBackups.saved", { count: answer.jobs_changed }));
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
      title={t("db.hostBackups.title", { host: host.name })}
      confirmLabel={t("common.save")}
      confirmDisabled={!ready}
      onCancel={onClose}
      onConfirm={() => void save()}
    >
      <div className="dialog-form">
        <p className="muted small">{t("db.hostBackups.note", { host: host.name })}</p>
        <fieldset className="dialog-fieldset" disabled={busy}>
          <Field label={t("db.hostBackups.schedule")} htmlFor="host-backups-schedule">
            <input
              id="host-backups-schedule"
              className="input mono"
              autoComplete="off"
              spellCheck={false}
              placeholder={autoSchedule(host.id)}
              aria-invalid={!scheduleValid}
              value={schedule}
              onChange={(event) => setSchedule(event.target.value)}
            />
            <p className="muted small">
              {text === ""
                ? t("db.hostBackups.scheduleDefault", {
                    time: dailyTime(autoSchedule(host.id), offset),
                  })
                : cronShaped(text)
                  ? t("backups.job.cronReads", { text: describeCron(text, t, offset) })
                  : t("backups.job.cronHint")}
            </p>
          </Field>
          <TargetPicker
            label={t("db.hostBackups.targets")}
            targets={targets}
            chosen={chosen}
            onChange={setChosen}
            fits={fits}
            serverName={serverName}
            none={t("db.hostBackups.targetsNone")}
          />
          <RetentionFields
            id="host-backups-keep"
            label={t("db.hostBackups.retention")}
            keep={keep}
            onChange={setKeep}
          />
          <p className="muted small">{t("db.hostBackups.follow", { count: follow })}</p>
        </fieldset>
        {error && <Banner tone="err">{error}</Banner>}
      </div>
    </ConfirmDialog>
  );
}
