import {
  AlertTriangle,
  CheckCircle2,
  ChevronDown,
  ChevronUp,
  Pencil,
  Trash2,
  XCircle,
} from "lucide-react";
import type { RecordModel } from "pocketbase";
import { useMemo, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { useToast } from "../components/toast";
import {
  Banner,
  Button,
  Card,
  ConfirmDialog,
  EmptyState,
  Field,
  IconButton,
  Kpi,
  SegmentedControl,
} from "../components/ui";
import { errorMessage } from "../lib/api";
import {
  formatCheckMoment,
  integritySummary,
  restoreTestTone,
  type BackupCheck,
  type IntegritySummary,
} from "../lib/backupChecks";
import {
  automaticJob,
  cronPreset,
  cronShaped,
  describeCron,
  describeRetention,
  dumpEngines,
  formatMoment,
  formatPercent,
  formatStripDay,
  fullRetention,
  guessEngine,
  jobsApi,
  lastRun,
  maxAttempts,
  nextJob,
  ownSettings,
  parseKeep,
  parseTime,
  presetCron,
  retentionKeys,
  runFields,
  runMoment,
  runStrip,
  runsSince,
  sameRetention,
  shortError,
  sourceGone,
  sourceKinds,
  successPercent,
  successRate,
  timeValue,
  validPath,
  validVolume,
  zoneOffset,
  type BackupJob,
  type BackupJobChange,
  type BackupRun,
  type DumpEngine,
  type NewBackupJob,
  type OwnSetting,
  type Retention,
  type RetentionKey,
  type RunStatus,
  type SchedulePreset,
  type SourceKind,
  type StripDay,
} from "../lib/backupJobs";
import { serverOption, type BackupTarget } from "../lib/backups";
import { backedUpWithin, latestTest } from "../lib/databaseBackups";
import { formatBytes, formatNumber } from "../lib/format";
import { useMounted } from "../lib/mounted";
import { pb } from "../lib/pb";
import { useLiveList, useLiveRecords, useNow } from "../lib/realtime";
import type { Alert, Container, Host, Stack, Volume } from "../lib/types";

/** The stacks as the jobs need them: no compose files. */
export type StackName = Pick<Stack, "id" | "name" | "host">;

/** The tiles above the jobs: how the last 14 days went, what the targets
 * hold, which job runs next, and how the targets' integrity checks stand. */
export function BackupSummary({
  jobs,
  runs,
  targets,
  hosts,
  unverified = [],
  openChecks = [],
  now,
}: {
  jobs: BackupJob[];
  runs: BackupRun[];
  targets: BackupTarget[];
  hosts: Host[];
  /** the open alerts that a target's repository is not verified */
  unverified?: Pick<Alert, "rule" | "target" | "status">[];
  /** the integrity checks queued or running */
  openChecks?: Pick<BackupCheck, "target" | "status">[];
  now: number;
}) {
  const { t } = useTranslation();
  const rate = successRate(runs, now);
  const percent = successPercent(rate.succeeded, rate.total);
  const measured = targets.filter((target) => target.usage_at);
  const stored = measured.reduce((sum, target) => sum + Math.max(0, target.used_bytes || 0), 0);
  const next = nextJob(jobs);
  const server = next ? hosts.find((host) => host.id === next.host)?.name : undefined;

  return (
    <section className="kpis" aria-label={t("backups.summary.label")}>
      <Kpi
        label={t("backups.summary.success")}
        value={percent === null ? "–" : formatPercent(percent)}
        note={
          rate.total > 0
            ? t("backups.summary.successOf", { ok: rate.succeeded, count: rate.total })
            : t("backups.summary.noRuns")
        }
      />
      <Kpi
        label={t("backups.summary.stored")}
        value={
          measured.length === 0 ? "–" : stored > 0 ? formatBytes(stored) : `${formatNumber(0)} B`
        }
        note={
          measured.length === 0
            ? t("backups.summary.notMeasured")
            : t("backups.summary.storedNote", { count: measured.length })
        }
      />
      <Kpi
        label={t("backups.summary.nextRun")}
        value={next ? <span className="mono">{formatMoment(next.next_run_at, now)}</span> : "–"}
        note={
          next
            ? [next.name, server].filter(Boolean).join(" · ")
            : t("backups.summary.nothingScheduled")
        }
      />
      <IntegrityKpi summary={integritySummary(targets, unverified, openChecks)} now={now} />
    </section>
  );
}

/** The integrity tile, as the design has it: a word and its icon, never the
 * colour alone, and the next check below; while a check runs, its target. */
function IntegrityKpi({ summary, now }: { summary: IntegritySummary; now: number }) {
  const { t } = useTranslation();
  let tone: "ok" | "warn" | "err" | undefined;
  let icon: ReactNode = null;
  let value: string;
  let note: string;
  switch (summary.state) {
    case "ok":
      tone = "ok";
      icon = <CheckCircle2 size={24} aria-hidden="true" />;
      value = t("backups.summary.integrityOk");
      note = t("backups.summary.integrityNote", { when: formatCheckMoment(summary.next, now) });
      break;
    case "failed":
      tone = "err";
      icon = <XCircle size={24} aria-hidden="true" />;
      value = t("backups.summary.integrityErrors");
      note = t("backups.summary.integrityErrorsNote", { count: summary.count });
      break;
    case "unverified":
      tone = "warn";
      icon = <AlertTriangle size={24} aria-hidden="true" />;
      value = t("backups.summary.integrityUnverified");
      note = t("backups.summary.integrityUnverifiedNote", { count: summary.count });
      break;
    case "never":
      value = "–";
      note = t("backups.summary.integrityNever");
      break;
    case "off":
      value = "–";
      note = t("backups.summary.integrityOff");
      break;
  }
  if (summary.running) {
    note = t("backups.summary.integrityChecking", { name: summary.running.name });
  }
  return (
    <Kpi label={t("backups.summary.integrity")} value={value} tone={tone} icon={icon} note={note} />
  );
}

/** The colours of the 14-day strip, as its legend names them. */
const stripStates = ["ok", "err", "warn", "off"] as const;

function StripLegend() {
  const { t } = useTranslation();
  return (
    <ul className="strip-legend" aria-label={t("backups.strip.legend")}>
      {stripStates.map((state) => (
        <li key={state}>
          <span className={`strip-swatch ${state}`} aria-hidden="true" />
          {t(`backups.strip.${state}`)}
        </li>
      ))}
    </ul>
  );
}

/** A day's runs in words; of those that succeeded, how many did as a retry
 * of a run that failed, which the strip no longer counts. */
function daySummary(
  day: Pick<StripDay, "succeeded" | "failed" | "skipped" | "retried">,
  t: Translate,
) {
  const parts = (["succeeded", "retried", "failed", "skipped"] as const)
    .filter((status) => day[status] > 0)
    .map((status) => t(`backups.strip.count.${status}`, { count: day[status] }));
  return parts.length > 0 ? parts.join(", ") : t("backups.strip.none");
}

type Translate = (key: string, options?: Record<string, unknown>) => string;

/** A job's last 14 days, oldest first: a day is the colour of its worst run,
 * and says its runs on hover. */
function DayStrip({ days }: { days: StripDay[] }) {
  const { t } = useTranslation();
  const total = days.reduce(
    (sum, day) => ({
      succeeded: sum.succeeded + day.succeeded,
      failed: sum.failed + day.failed,
      skipped: sum.skipped + day.skipped,
      retried: sum.retried + day.retried,
    }),
    { succeeded: 0, failed: 0, skipped: 0, retried: 0 },
  );
  return (
    <div
      className="run-strip"
      role="img"
      aria-label={t("backups.strip.label", { summary: daySummary(total, t) })}
    >
      {days.map((day) => (
        <span
          key={day.day}
          className={day.state}
          title={`${formatStripDay(day.day)}: ${daySummary(day, t)}`}
        />
      ))}
    </div>
  );
}

const statusClass: Record<RunStatus, string> = {
  queued: "info",
  running: "info",
  succeeded: "ok",
  failed: "err",
  skipped: "warn",
  cancelled: "off",
};

/** What a job's source is, in a line: volume vw-data, a directory, the
 * volumes of a stack, those of one service, a dump of a service, or the
 * panel's own encrypted copy (#32). */
export function describeSource(job: BackupJob, stacks: StackName[], t: Translate): string {
  const stack = stacks.find((item) => item.id === job.stack)?.name ?? "–";
  let text: string;
  switch (job.source_kind) {
    case "volume":
      text = t("backups.source.volume", { name: job.source });
      break;
    case "path":
      text = job.source;
      break;
    case "stack": {
      // a service's label names its own volumes; with none named, the job
      // backs up every volume of the stack
      const names = job.source
        .split(",")
        .map((name) => name.trim())
        .filter(Boolean);
      text =
        job.service && names.length > 0
          ? t("backups.source.serviceVolumes", { service: job.service, names: names.join(", ") })
          : t("backups.source.stack", { name: stack });
      break;
    }
    case "db_dump":
      text = t("backups.source.db_dump", {
        tool: job.engine ? t(`backups.engineTool.${job.engine}`) : "–",
        stack,
        service: job.service,
      });
      break;
    case "panel":
      text = t("backups.source.panel");
      break;
    case "database": {
      // a dropped database's job keeps its server's name and the database's
      const server = job.db_server_name || "–";
      text = job.database
        ? t("backups.source.database", {
            tool: job.engine ? t(`backups.engineTool.${job.engine}`) : "–",
            server,
            database: job.source,
          })
        : t("backups.source.databaseGone", { server, database: job.source });
      break;
    }
    case "site": {
      // a classic site's job backs up its directory, as its source says
      const [name, dir = ""] = job.source.split("/");
      text = t(dir === "shared" ? "backups.source.site" : "backups.source.siteClassic", { name });
      break;
    }
    default:
      text = job.source;
  }
  switch (job.origin) {
    case "label":
      return `${text} · ${t("backups.job.fromLabel")}`;
    case "database":
      return `${text} · ${t("backups.job.fromDatabase")}`;
    case "site":
      return `${text} · ${t("backups.job.fromSite")}`;
    default:
      return text;
  }
}

/** Why a job no longer runs, as its row says it: its label, database or
 * site is gone; "" while it is there. */
function goneText(job: BackupJob, t: Translate): string {
  if (!sourceGone(job)) {
    return "";
  }
  switch (job.origin) {
    case "database":
      return t("backups.job.databaseGone");
    case "site":
      return t("backups.job.siteGone");
    default:
      return t("backups.job.labelRemoved");
  }
}

/** How the job's last runs went: the newest per target, the worst first;
 * of a failed run, why in a word (#31), and when it is tried again. */
function LastRun({
  job,
  runs,
  targets,
  now,
}: {
  job: BackupJob;
  runs: BackupRun[];
  targets: BackupTarget[];
  now: number;
}) {
  const { t } = useTranslation();
  const run = lastRun(job, runs);
  if (!run) {
    // older than the runs loaded: what the job says of its newest
    if (job.last_status) {
      return (
        <div className={`run-status ${statusClass[job.last_status]}`}>
          {t("backups.job.lastRunAt", {
            status: t(`backups.runStatus.${job.last_status}`),
            time: formatMoment(job.last_run_at, now),
          })}
        </div>
      );
    }
    return <span className="faint">{t("backups.job.neverRun")}</span>;
  }
  // of several targets, the one a run that did not succeed is about
  const target =
    (job.targets ?? []).length > 1 ? targets.find((item) => item.id === run.target) : undefined;
  const detail =
    run.status === "succeeded"
      ? t("backups.job.bytesAdded", {
          size: run.bytes_added > 0 ? formatBytes(run.bytes_added) : `${formatNumber(0)} B`,
        })
      : [target?.name, failureWord(run, t) || shortError(run.error)].filter(Boolean).join(": ");
  // the panel tries it again by itself, or once restic counts its lock as stale
  const retryAt = run.status === "failed" ? (run.retry_at ?? "") : "";
  const attempt = (run.attempt || 1) + 1;
  // the backup is there, but restic did not forget the old snapshots
  const retention =
    run.status === "succeeded" && (run.retention === "deferred" || run.retention === "failed")
      ? run.retention
      : null;
  return (
    <>
      <div className={`run-status ${statusClass[run.status]}`}>
        {t("backups.job.lastRunAt", {
          status: t(`backups.runStatus.${run.status}`),
          time: formatMoment(runMoment(run), now),
        })}
      </div>
      {detail && (
        <div className="run-detail" title={run.error || undefined}>
          {detail}
        </div>
      )}
      {retryAt && (
        <div className="run-retry" title={run.retry_note || undefined}>
          {t("backups.job.retrying", {
            attempt,
            max: Math.max(maxAttempts, attempt),
            time: formatMoment(retryAt, now),
          })}
        </div>
      )}
      {retention && (
        <div
          className={`run-retention ${retention}`}
          title={[target?.name, t(`backups.runRetention.${retention}`)].filter(Boolean).join(": ")}
        >
          {retention === "deferred"
            ? t("backups.runRetention.deferredShort")
            : t("backups.runRetention.failed")}
        </div>
      )}
    </>
  );
}

/** Why a run failed in a word, as the agent or the panel classed it: "repo
 * zamčené"; "" for a run of an older panel, which says it only in its error. */
function failureWord(run: Pick<BackupRun, "status" | "failure">, t: Translate): string {
  return run.status === "failed" && run.failure ? t(`backups.runFailure.${run.failure}`) : "";
}

/** The group a database's job is listed in: its server's name, kept after
 * the database was dropped (#53). */
function groupKey(job: Pick<BackupJob, "db_server_name">): string {
  return job.db_server_name || "–";
}

/** The database jobs of one server, listed as one row. */
type JobGroup = { kind: "group"; key: string; jobs: BackupJob[] };

type JobEntry = JobGroup | { kind: "job"; job: BackupJob; grouped: boolean };

/** The rows of the jobs' table, in the jobs' order: each job, but the
 * database jobs of a server as one row where the first of them is, followed
 * by each of them while the group is open. */
export function jobEntries(jobs: BackupJob[], open: ReadonlySet<string>): JobEntry[] {
  const groups = new Map<string, JobGroup>();
  for (const job of jobs) {
    if (job.origin !== "database") {
      continue;
    }
    const key = groupKey(job);
    const group = groups.get(key);
    if (group) {
      group.jobs.push(job);
    } else {
      groups.set(key, { kind: "group", key, jobs: [job] });
    }
  }
  const entries: JobEntry[] = [];
  const listed = new Set<string>();
  for (const job of jobs) {
    if (job.origin !== "database") {
      entries.push({ kind: "job", job, grouped: false });
      continue;
    }
    const key = groupKey(job);
    if (listed.has(key)) {
      continue;
    }
    listed.add(key);
    const group = groups.get(key)!;
    entries.push(group);
    if (open.has(key)) {
      entries.push(
        ...group.jobs.map((item) => ({ kind: "job" as const, job: item, grouped: true })),
      );
    }
  }
  return entries;
}

const runRank: Record<RunStatus, number> = {
  failed: 0,
  skipped: 1,
  cancelled: 2,
  running: 3,
  queued: 4,
  succeeded: 5,
};

/** A server's database jobs in one row (#53): how many, how many backed up
 * within a day, the worst of their last runs, and their 14 days summed, a
 * day the colour of its worst run. Its button shows each database. */
function JobGroupRow({
  group,
  runsByJob,
  targets,
  hosts,
  now,
  open,
  onToggle,
}: {
  group: JobGroup;
  runsByJob: Map<string, BackupRun[]>;
  targets: BackupTarget[];
  hosts: Host[];
  now: number;
  open: boolean;
  onToggle: () => void;
}) {
  const { t } = useTranslation();
  const offset = zoneOffset(new Date(now));
  const live = group.jobs.filter((job) => !sourceGone(job));
  const runs = group.jobs.flatMap((job) => runsByJob.get(job.id) ?? []);
  const done = live.filter((job) => backedUpWithin(job, runsByJob.get(job.id) ?? [], now)).length;
  let worst: BackupRun | null = null;
  for (const job of live) {
    const last = lastRun(job, runsByJob.get(job.id) ?? []);
    if (
      last &&
      (!worst ||
        runRank[last.status] < runRank[worst.status] ||
        (runRank[last.status] === runRank[worst.status] && last.created > worst.created))
    ) {
      worst = last;
    }
  }
  const servers = [...new Set(group.jobs.map((job) => job.host))]
    .map((id) => hosts.find((host) => host.id === id)?.name ?? "–")
    .join(", ");
  const names = [...new Set(live.flatMap((job) => job.targets ?? []))]
    .map((id) => targets.find((target) => target.id === id)?.name)
    .filter(Boolean);
  const schedules = [...new Set(live.map((job) => job.schedule.trim()))];
  return (
    <tr className="job-group">
      <td className="job-cell">
        <div className="job-name">
          {t("backups.job.group", { server: group.key, count: group.jobs.length })}
        </div>
        <div className="job-source">
          {t("backups.job.groupState", { ok: done, total: live.length })}
        </div>
      </td>
      <td className="job-where">
        <span className="mono job-host">{servers}</span>
        {" → "}
        {names.length > 0 ? (
          names.join(" + ")
        ) : (
          <span className="warn-text">{t("backups.job.noTarget")}</span>
        )}
      </td>
      <td className="job-schedule">
        {schedules.length === 1 ? describeCron(schedules[0], t, offset) : "–"}
      </td>
      <td className="job-last">
        {worst ? (
          <div className={`run-status ${statusClass[worst.status]}`}>
            {t("backups.job.lastRunAt", {
              status: t(`backups.runStatus.${worst.status}`),
              time: formatMoment(runMoment(worst), now),
            })}
          </div>
        ) : (
          <span className="faint">{t("backups.job.neverRun")}</span>
        )}
      </td>
      <td>
        <DayStrip days={runStrip(runs, now)} />
      </td>
      <td className="job-actions">
        <div className="row-actions">
          <Button size="small" aria-expanded={open} onClick={onToggle}>
            {open ? (
              <ChevronUp size={14} aria-hidden="true" />
            ) : (
              <ChevronDown size={14} aria-hidden="true" />
            )}
            {t("backups.job.groupToggle")}
          </Button>
        </div>
      </td>
    </tr>
  );
}

/** The jobs as the design lists them, with what each backs up, where to,
 * when and how long, its last run and its last 14 days. */
export function JobsCard({
  jobs,
  runs,
  targets,
  hosts,
  stacks,
  loading,
  error,
  now,
  canRun,
  isAdmin,
  selected,
  onSelect,
  onAdd,
  onEdit,
  tests = [],
}: {
  jobs: BackupJob[];
  runs: BackupRun[];
  targets: BackupTarget[];
  hosts: Host[];
  stacks: StackName[];
  loading: boolean;
  error: unknown;
  now: number;
  canRun: boolean;
  isAdmin: boolean;
  /** the job whose snapshots are shown */
  selected?: string;
  /** shows a job's snapshots; its name is then a button */
  onSelect?: (job: BackupJob) => void;
  onAdd: () => void;
  onEdit: (job: BackupJob) => void;
  /** the restore tests of database jobs (#53) */
  tests?: BackupCheck[];
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const [starting, setStarting] = useState<ReadonlySet<string>>(new Set());
  const [removing, setRemoving] = useState<BackupJob | null>(null);
  const [removeBusy, setRemoveBusy] = useState(false);
  const offset = zoneOffset(new Date(now));
  // the database servers whose jobs are shown one by one (#53); the group of
  // the job picked opens, as when an alert links to it
  const groupOf = (id: string | undefined) => {
    const job = jobs.find((item) => item.id === id);
    return job && job.origin === "database" ? groupKey(job) : null;
  };
  const [open, setOpen] = useState<ReadonlySet<string>>(() => {
    const key = groupOf(selected);
    return new Set(key === null ? [] : [key]);
  });
  const [opened, setOpened] = useState(selected);
  if (selected !== opened) {
    setOpened(selected);
    const key = groupOf(selected);
    if (key !== null && !open.has(key)) {
      setOpen(new Set(open).add(key));
    }
  }
  const toggle = (key: string) =>
    setOpen((current) => {
      const next = new Set(current);
      if (!next.delete(key)) {
        next.add(key);
      }
      return next;
    });

  const runsByJob = useMemo(() => {
    const grouped = new Map<string, BackupRun[]>();
    for (const run of runs) {
      const list = grouped.get(run.job);
      if (list) {
        list.push(run);
      } else {
        grouped.set(run.job, [run]);
      }
    }
    return grouped;
  }, [runs]);

  const run = async (job: BackupJob) => {
    setStarting((current) => new Set(current).add(job.id));
    try {
      const result = await jobsApi.run(job.id);
      toast.success(t("backups.job.started", { name: job.name, count: result.runs.length }));
    } catch (err) {
      toast.error(err);
    } finally {
      setStarting((current) => {
        const next = new Set(current);
        next.delete(job.id);
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
      await jobsApi.remove(id);
      toast.success(t("backups.job.removed", { name }));
      setRemoving((current) => (current?.id === id ? null : current));
    } catch (err) {
      toast.error(err);
    } finally {
      setRemoveBusy(false);
    }
  };

  let body: ReactNode;
  if (error) {
    body = (
      <div className="card-pad">
        <Banner tone="err">{errorMessage(error)}</Banner>
      </div>
    );
  } else if (loading && jobs.length === 0) {
    body = <p className="card-pad muted">{t("common.loading")}</p>;
  } else if (jobs.length === 0) {
    body = (
      <EmptyState
        title={t("backups.job.noneTitle")}
        action={
          isAdmin && (
            <Button size="small" onClick={onAdd}>
              {t("backups.job.new")}
            </Button>
          )
        }
      >
        {t("backups.job.noneText")}
      </EmptyState>
    );
  } else {
    body = (
      <div className="table-scroll" tabIndex={0} role="region" aria-label={t("backups.job.title")}>
        <table className="table backup-jobs">
          <thead>
            <tr>
              <th scope="col">{t("backups.job.column.job")}</th>
              <th scope="col">{t("backups.job.column.where")}</th>
              <th scope="col">{t("backups.job.column.schedule")}</th>
              <th scope="col">{t("backups.job.column.lastRun")}</th>
              <th scope="col">{t("backups.job.column.days")}</th>
              <th scope="col" className="job-actions">
                {t("common.actions")}
              </th>
            </tr>
          </thead>
          <tbody>
            {jobEntries(jobs, open).map((entry) => {
              if (entry.kind === "group") {
                return (
                  <JobGroupRow
                    key={`group:${entry.key}`}
                    group={entry}
                    runsByJob={runsByJob}
                    targets={targets}
                    hosts={hosts}
                    now={now}
                    open={open.has(entry.key)}
                    onToggle={() => toggle(entry.key)}
                  />
                );
              }
              const job = entry.job;
              const jobRuns = runsByJob.get(job.id) ?? [];
              const names = (job.targets ?? [])
                .map((id) => targets.find((target) => target.id === id)?.name)
                .filter(Boolean);
              const server = hosts.find((host) => host.id === job.host)?.name ?? "–";
              const source = describeSource(job, stacks, t);
              const gone = sourceGone(job);
              const runnable = !gone && names.length > 0;
              // a job someone added; one that came with a label, a database or
              // a site once that is gone
              const removable = job.origin === "manual" || gone;
              const picked = onSelect !== undefined && selected === job.id;
              const test = job.origin === "database" ? latestTest(tests, job.id) : null;
              const classes = [
                gone && "job-removed",
                picked && "job-selected",
                entry.grouped && "job-grouped",
              ]
                .filter(Boolean)
                .join(" ");
              return (
                <tr key={job.id} className={classes || undefined}>
                  <td className="job-cell">
                    {onSelect ? (
                      <button
                        type="button"
                        className="job-name"
                        aria-pressed={picked}
                        aria-label={t("backups.job.showSnapshots", { name: job.name })}
                        onClick={() => onSelect(job)}
                      >
                        {job.name}
                      </button>
                    ) : (
                      <div className="job-name">{job.name}</div>
                    )}
                    <div className="job-source" title={source}>
                      {source}
                    </div>
                    {job.problem && !gone && <div className="job-problem">{job.problem}</div>}
                    {gone && (
                      <div className="job-problem">
                        <span>{goneText(job, t)}</span>
                        {isAdmin && (
                          <Button
                            size="small"
                            variant="danger"
                            aria-label={t("backups.job.removeName", { name: job.name })}
                            onClick={() => setRemoving(job)}
                          >
                            {t("common.remove")}
                          </Button>
                        )}
                      </div>
                    )}
                  </td>
                  <td className="job-where">
                    {job.source_kind === "panel" ? (
                      // the panel's copy, which the carrier uploads
                      <>
                        <span className="mono job-host">{t("backups.job.panelHost")}</span>
                        <span className="muted">
                          {" · "}
                          {t("backups.job.via", { server })}
                        </span>
                      </>
                    ) : (
                      <span className="mono job-host">{server}</span>
                    )}
                    {" → "}
                    {names.length > 0 ? (
                      names.join(" + ")
                    ) : (
                      <span className="warn-text">{t("backups.job.noTarget")}</span>
                    )}
                  </td>
                  <td className="job-schedule">
                    <div>
                      {describeCron(job.schedule, t, offset)}
                      {!job.enabled && !gone && (
                        <span className="faint">{` · ${t("backups.job.paused")}`}</span>
                      )}
                    </div>
                    <div className="mono job-retention" title={t("backups.retention.title")}>
                      {describeRetention(job.retention, t)}
                    </div>
                  </td>
                  <td className="job-last">
                    <LastRun job={job} runs={jobRuns} targets={targets} now={now} />
                    {test && (
                      <div className={`run-test ${restoreTestTone(test.status)}`}>
                        {t("backups.job.restoreTest", {
                          state: t(`backups.restoreTests.status.${test.status}`),
                        })}
                      </div>
                    )}
                  </td>
                  <td>
                    <DayStrip days={runStrip(jobRuns, now)} />
                  </td>
                  <td className="job-actions">
                    <div className="row-actions">
                      {canRun && (
                        <Button
                          size="small"
                          aria-label={t("backups.job.runName", { name: job.name })}
                          title={
                            runnable
                              ? undefined
                              : gone
                                ? goneText(job, t)
                                : t("backups.job.cannotRun")
                          }
                          disabled={!runnable || starting.has(job.id)}
                          onClick={() => void run(job)}
                        >
                          {starting.has(job.id) ? t("backups.job.starting") : t("backups.job.run")}
                        </Button>
                      )}
                      {isAdmin && !gone && (
                        <IconButton
                          bare
                          aria-label={t("backups.job.editName", { name: job.name })}
                          onClick={() => onEdit(job)}
                        >
                          <Pencil size={15} aria-hidden="true" />
                        </IconButton>
                      )}
                      {isAdmin && removable && !gone && (
                        <IconButton
                          bare
                          aria-label={t("backups.job.removeName", { name: job.name })}
                          onClick={() => setRemoving(job)}
                        >
                          <Trash2 size={15} aria-hidden="true" />
                        </IconButton>
                      )}
                    </div>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    );
  }

  return (
    <>
      <Card
        title={t("backups.job.title")}
        note={onSelect && jobs.length > 0 ? t("backups.job.pickHint") : undefined}
        side={<StripLegend />}
        flush
      >
        {body}
      </Card>
      <ConfirmDialog
        open={removing !== null}
        title={t("backups.job.removeTitle", { name: removing?.name ?? "" })}
        confirmLabel={t("common.remove")}
        danger
        confirmDisabled={removeBusy}
        onCancel={() => setRemoving(null)}
        onConfirm={() => void remove()}
      >
        <p className="muted">{t("backups.job.removeText")}</p>
      </ConfirmDialog>
    </>
  );
}

/** A server's backup jobs, for its page: what each backs up, how its last
 * run went and when it runs next. The Backups page manages them. */
export function ServerJobsCard({ host }: { host: Host }) {
  const { t } = useTranslation();
  const now = useNow(30_000);
  const filter = pb.filter("host = {:host}", { host: host.id });
  const jobs = useLiveList<BackupJob>("backup_jobs", { filter, sort: "name" });
  const stacks = useLiveList<StackName & RecordModel>("stacks", {
    filter,
    fields: "id,collectionId,collectionName,name,host",
    perPage: 500,
  });
  // of several targets, Last run names the one a run is about
  const targets = useLiveList<BackupTarget>("backup_targets", {
    fields: "id,collectionId,collectionName,name",
    perPage: 500,
  });
  const runs = useLiveRecords<BackupRun>("backup_runs", {
    filter: pb.filter("host = {:host} && created >= {:since}", {
      host: host.id,
      since: runsSince(now),
    }),
    fields: runFields,
  });

  let body: ReactNode;
  if (jobs.error || runs.error) {
    body = (
      <div className="card-pad">
        <Banner tone="err">{errorMessage(jobs.error || runs.error)}</Banner>
      </div>
    );
  } else if (jobs.loading && jobs.items.length === 0) {
    body = <p className="card-pad muted">{t("common.loading")}</p>;
  } else if (jobs.items.length === 0) {
    body = (
      <EmptyState title={t("server.backups.noneTitle")}>{t("server.backups.noneText")}</EmptyState>
    );
  } else {
    body = (
      <div
        className="table-scroll"
        tabIndex={0}
        role="region"
        aria-label={t("server.backups.title")}
      >
        <table className="table server-jobs">
          <thead>
            <tr>
              <th scope="col">{t("backups.job.column.job")}</th>
              <th scope="col">{t("backups.job.column.lastRun")}</th>
              <th scope="col">{t("backups.summary.nextRun")}</th>
            </tr>
          </thead>
          <tbody>
            {jobs.items.map((job) => {
              const source = describeSource(job, stacks.items, t);
              // one whose label, database or site is gone says so under its name
              const gone = sourceGone(job);
              let next: ReactNode = "–";
              if (!job.enabled && !gone) {
                next = <span className="faint">{t("backups.job.paused")}</span>;
              } else if (job.enabled && job.next_run_at) {
                next = <span className="mono">{formatMoment(job.next_run_at, now)}</span>;
              }
              return (
                <tr key={job.id} className={gone ? "job-removed" : undefined}>
                  <td className="job-cell">
                    <Link className="job-name" to={`/backups?job=${job.id}`}>
                      {job.name}
                    </Link>
                    <div className="job-source" title={source}>
                      {source}
                    </div>
                    {(gone || job.problem) && (
                      <div className="job-problem">{gone ? goneText(job, t) : job.problem}</div>
                    )}
                  </td>
                  <td className="job-last">
                    <LastRun job={job} runs={runs.items} targets={targets.items} now={now} />
                  </td>
                  <td className="job-next">{next}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    );
  }

  return (
    <Card
      title={t("server.backups.title")}
      side={<Link to="/backups">{t("server.backups.open")}</Link>}
      flush
    >
      {body}
    </Card>
  );
}

type ScheduleMode = SchedulePreset["kind"] | "custom";

const scheduleModes: ScheduleMode[] = ["hourly", "daily", "weekly", "custom"];

/** Monday first, as the week starts where the panel is used. */
const weekdays = [1, 2, 3, 4, 5, 6, 0];

/** What the retention fields hold, as typed: empty keeps nothing of that kind. */
export type KeepForm = Record<RetentionKey, string>;

export function keepForm(retention: Retention): KeepForm {
  const form = {} as KeepForm;
  for (const key of retentionKeys) {
    form[key] = retention[key] > 0 ? String(retention[key]) : "";
  }
  return form;
}

/** The retention the fields hold, or null while one of them is no number
 * the panel takes. */
export function parsedRetention(keep: KeepForm): Retention | null {
  const parsed = {} as Record<RetentionKey, number | null>;
  for (const key of retentionKeys) {
    parsed[key] = parseKeep(keep[key]);
  }
  return retentionKeys.every((key) => parsed[key] !== null)
    ? fullRetention(parsed as Partial<Retention>)
    : null;
}

/** The targets a job backs up to, as boxes to tick; a local target of
 * another server cannot be ticked. */
export function TargetPicker({
  label,
  targets,
  chosen,
  onChange,
  fits,
  serverName,
  disabled,
  none,
  children,
}: {
  label: string;
  targets: BackupTarget[];
  chosen: string[];
  onChange: (update: (current: string[]) => string[]) => void;
  fits: (target: BackupTarget) => boolean;
  serverName: (id: string) => string;
  disabled?: boolean;
  /** what it says while none is ticked */
  none: string;
  children?: ReactNode;
}) {
  const { t } = useTranslation();
  const selected = targets.filter((target) => chosen.includes(target.id) && fits(target));
  return (
    <fieldset className="job-targets" disabled={disabled}>
      <legend className="field-label">{label}</legend>
      {targets.length === 0 ? (
        <p className="muted small">{t("backups.job.noTargets")}</p>
      ) : (
        targets.map((target) => {
          const fit = fits(target);
          return (
            <label key={target.id} className="check-line">
              <input
                type="checkbox"
                disabled={!fit}
                checked={fit && chosen.includes(target.id)}
                onChange={(event) =>
                  onChange((current) =>
                    event.target.checked
                      ? [...current, target.id]
                      : current.filter((id) => id !== target.id),
                  )
                }
              />
              <span>
                {target.name}
                {!fit && (
                  <span className="faint small">
                    {" · "}
                    {t("backups.job.localOnly", { server: serverName(target.host) })}
                  </span>
                )}
              </span>
            </label>
          );
        })
      )}
      {targets.length > 0 && selected.length === 0 && <p className="warn-text small">{none}</p>}
      {children}
    </fieldset>
  );
}

/** How many snapshots of each kind a job keeps, a field per kind. */
export function RetentionFields({
  id,
  label,
  keep,
  onChange,
  disabled,
  children,
}: {
  /** the fields' ids start with it */
  id: string;
  label: string;
  keep: KeepForm;
  onChange: (update: (current: KeepForm) => KeepForm) => void;
  disabled?: boolean;
  children?: ReactNode;
}) {
  const { t } = useTranslation();
  const retention = parsedRetention(keep);
  return (
    <fieldset className="job-retention-fields" disabled={disabled}>
      <legend className="field-label">{label}</legend>
      <div className="keep-grid">
        {retentionKeys.map((key) => (
          <Field key={key} label={t(`backups.keep.${key}`)} htmlFor={`${id}-${key}`}>
            <input
              id={`${id}-${key}`}
              className="input"
              type="number"
              inputMode="numeric"
              min={0}
              max={10000}
              step={1}
              placeholder="0"
              value={keep[key]}
              onChange={(event) =>
                onChange((current) => ({ ...current, [key]: event.target.value }))
              }
            />
          </Field>
        ))}
      </div>
      {retention ? (
        <p className="muted small">
          {t("backups.job.retentionHint", { keeps: describeRetention(retention, t) })}
        </p>
      ) : (
        <p className="field-error">{t("backups.job.retentionInvalid")}</p>
      )}
      {children}
    </fieldset>
  );
}

/** Whether a setting of a database's or a site's job follows its server's
 * defaults or is its own (#53). */
function FollowSwitch({
  label,
  own,
  host,
  onChange,
}: {
  label: string;
  own: boolean;
  host: string;
  onChange: (own: boolean) => void;
}) {
  const { t } = useTranslation();
  const follows = t("backups.job.follows", { host });
  return (
    <div className="follow-switch">
      <SegmentedControl
        // its field has the setting's name: the group says the choice too
        label={`${label}: ${follows} / ${t("backups.job.own")}`}
        value={own ? "own" : "follows"}
        onChange={(value) => onChange(value === "own")}
        options={[
          { value: "follows", label: t("backups.job.follows", { host }) },
          { value: "own", label: t("backups.job.own") },
        ]}
      />
    </div>
  );
}

/** What the panel made a job for, as its note names it: the database on its
 * server, or the site. */
function automaticWhat(job: Pick<BackupJob, "origin" | "source" | "db_server_name">): string {
  return job.origin === "database" && job.db_server_name
    ? `${job.db_server_name}/${job.source}`
    : job.source;
}

/** As the label's default: 7 daily, 4 weekly and 6 monthly snapshots. */
export const defaultRetention: Retention = {
  last: 0,
  hourly: 0,
  daily: 7,
  weekly: 4,
  monthly: 6,
  yearly: 0,
};

function sameTargets(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((id) => b.includes(id));
}

/** The browser's time zone, to say which local time the presets are in. */
function zoneName(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
}

/** Adds a job, or changes one. A label's job keeps its source and server as
 * its labels say; what else they set comes back at the next deploy, and the
 * panel refuses to change it: the dialog shows it read-only and never sends
 * it. */
export function JobDialog({
  job,
  targets,
  hosts,
  stacks,
  onClose,
}: {
  /** null for a new job */
  job: BackupJob | null;
  targets: BackupTarget[];
  hosts: Host[];
  stacks: StackName[];
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const mounted = useMounted();
  const fromLabel = job?.origin === "label";
  // a database's or a site's job (#53): its source and server follow that,
  // and each of when, where and how long follows its server or is its own
  const automatic = job !== null && automaticJob(job);
  const [owned, setOwned] = useState<Record<OwnSetting, boolean>>(() => {
    const own = job?.own_settings ?? [];
    return {
      schedule: own.includes("schedule"),
      targets: own.includes("targets"),
      retention: own.includes("retention"),
    };
  });
  const follows = (field: OwnSetting) => automatic && !owned[field];
  // what the job's labels set, as they set it
  const labelSets = fromLabel ? (job?.label_sets ?? []) : [];
  const scheduleLocked = labelSets.includes("schedule") || follows("schedule");
  const targetsLocked = labelSets.includes("targets") || follows("targets");
  const retentionLocked = labelSets.includes("retention") || follows("retention");
  // presets are in local time now; the panel keeps UTC
  const [offset] = useState(() => zoneOffset());
  const [initial] = useState<SchedulePreset | null>(() =>
    job ? cronPreset(job.schedule, offset) : { kind: "daily", hour: 3, minute: 0 },
  );

  const [name, setName] = useState(job?.name ?? "");
  const [kind, setKind] = useState<SourceKind>(job?.source_kind ?? "volume");
  const [host, setHost] = useState(job?.host ?? "");
  const [source, setSource] = useState(job?.source ?? "");
  const [stack, setStack] = useState(job?.stack ?? "");
  const [service, setService] = useState(job?.service ?? "");
  const [engine, setEngine] = useState<DumpEngine | "">(job?.engine ?? "");
  const [chosen, setChosen] = useState<string[]>(job?.targets ?? []);
  const [mode, setMode] = useState<ScheduleMode>(initial?.kind ?? "custom");
  const [minute, setMinute] = useState(String(initial?.kind === "hourly" ? initial.minute : 0));
  const [time, setTime] = useState(
    initial && initial.kind !== "hourly" ? timeValue(initial.hour, initial.minute) : "03:00",
  );
  const [weekday, setWeekday] = useState(initial?.kind === "weekly" ? initial.weekday : 1);
  const [cron, setCron] = useState(job?.schedule ?? "");
  // an untouched schedule stays as it is, even where the dialog writes it otherwise
  const [scheduleTouched, setScheduleTouched] = useState(false);
  const [keep, setKeep] = useState<KeepForm>(() =>
    keepForm(job ? fullRetention(job.retention) : defaultRetention),
  );
  const [enabled, setEnabled] = useState(job?.enabled ?? true);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const usesStack = kind === "stack" || kind === "db_dump";
  const stackRecord = stacks.find((item) => item.id === stack);
  const jobHost =
    fromLabel || automatic ? (job?.host ?? "") : usesStack ? (stackRecord?.host ?? "") : host;
  const serverName = (id: string) => hosts.find((item) => item.id === id)?.name ?? "–";

  const volumes = useLiveList<Volume>("volumes", {
    filter: pb.filter("host = {:host}", { host }),
    sort: "name",
    fields: "id,host,name",
    enabled: !fromLabel && kind === "volume" && host !== "",
  });
  const containers = useLiveList<Container>("containers", {
    filter: pb.filter("stack = {:stack}", { stack }),
    sort: "service",
    fields: "id,stack,service,image",
    enabled: !fromLabel && kind === "db_dump" && stack !== "",
  });
  const services = [
    ...new Set(containers.items.map((item) => item.service).filter((item) => item !== "")),
  ];
  if (service !== "" && !services.includes(service)) {
    // the job's own service, though it has no container now
    services.unshift(service);
  }

  // a local target is a directory of its own server: only that server's jobs
  const fits = (target: BackupTarget) => target.kind !== "local" || target.host === jobHost;
  const selected = targets.filter((target) => chosen.includes(target.id) && fits(target));
  const targetIds = selected.map((target) => target.id);

  const time24 = parseTime(time);
  const minuteNumber = /^\d{1,2}$/.test(minute.trim()) ? Number(minute) : NaN;
  let preset: SchedulePreset | null = null;
  if (mode === "hourly" && minuteNumber >= 0 && minuteNumber <= 59) {
    preset = { kind: "hourly", minute: minuteNumber };
  } else if (mode === "daily" && time24) {
    preset = { kind: "daily", ...time24 };
  } else if (mode === "weekly" && time24) {
    preset = { kind: "weekly", weekday, ...time24 };
  }
  const schedule =
    !scheduleTouched && job
      ? job.schedule
      : mode === "custom"
        ? cron.trim()
        : preset
          ? presetCron(preset, offset)
          : "";
  const scheduleValid = mode === "custom" ? cronShaped(cron) : preset !== null;

  const parsed = parsedRetention(keep);
  const keepValid = parsed !== null;
  const retention = parsed ?? fullRetention(null);

  const volumeInvalid = kind === "volume" && source.trim() !== "" && !validVolume(source);
  const pathInvalid = kind === "path" && source.trim() !== "" && !validPath(source);
  const sqliteInvalid =
    kind === "db_dump" &&
    engine === "sqlite" &&
    source.trim() !== "" &&
    !source.trim().startsWith("/");

  let sourceReady = true;
  if (!fromLabel) {
    switch (kind) {
      case "volume":
        sourceReady = host !== "" && validVolume(source);
        break;
      case "path":
        sourceReady = host !== "" && validPath(source);
        break;
      case "stack":
        sourceReady = stack !== "";
        break;
      case "db_dump":
        sourceReady =
          stack !== "" &&
          service.trim() !== "" &&
          engine !== "" &&
          (engine !== "sqlite" || source.trim().startsWith("/"));
        break;
    }
  }

  /** What the job is, as the panel takes a new one. */
  const full = (): NewBackupJob => {
    const body: NewBackupJob = {
      name: name.trim(),
      host: jobHost,
      source_kind: kind,
      targets: targetIds,
      schedule,
      retention,
      enabled,
    };
    switch (kind) {
      case "volume":
      case "path":
        body.source = source.trim();
        break;
      case "stack":
        body.stack = stack;
        break;
      case "db_dump":
        body.stack = stack;
        body.service = service.trim();
        body.engine = engine as DumpEngine;
        if (engine === "sqlite") {
          body.source = source.trim();
        }
        break;
    }
    return body;
  };

  /** What differs from the job: a label's job never sends its source, nor
   * what its labels set. */
  const changes = (): BackupJobChange => {
    if (!job) {
      return {};
    }
    const body = full();
    const change: BackupJobChange = {};
    if (body.name !== job.name) {
      change.name = body.name;
    }
    if (!targetsLocked && !sameTargets(body.targets, job.targets ?? [])) {
      change.targets = body.targets;
    }
    if (!scheduleLocked && body.schedule !== job.schedule) {
      change.schedule = body.schedule;
    }
    if (!retentionLocked && !sameRetention(body.retention, job.retention)) {
      change.retention = body.retention;
    }
    if (body.enabled !== job.enabled) {
      change.enabled = body.enabled;
    }
    if (automatic) {
      // a setting made its own is sent though it is the same, so that it
      // stays when the server's defaults change; one that follows again is
      // named in follow, and the panel copies the server's
      const own = job.own_settings ?? [];
      if (owned.targets && !own.includes("targets")) {
        change.targets = body.targets;
      }
      if (owned.schedule && !own.includes("schedule")) {
        change.schedule = body.schedule;
      }
      if (owned.retention && !own.includes("retention")) {
        change.retention = body.retention;
      }
      const follow = ownSettings.filter((field) => own.includes(field) && !owned[field]);
      if (follow.length > 0) {
        change.follow = follow;
      }
      return change;
    }
    if (fromLabel) {
      return change;
    }
    if (body.host !== job.host) {
      change.host = body.host;
    }
    if (body.source_kind !== job.source_kind) {
      // another kind: every field of its source, and none of the old one's
      return {
        ...change,
        host: body.host,
        source_kind: body.source_kind,
        source: body.source ?? "",
        stack: body.stack ?? "",
        service: body.service ?? "",
        engine: body.engine ?? "",
      };
    }
    if ((body.source ?? "") !== job.source) {
      change.source = body.source ?? "";
    }
    if ((body.stack ?? "") !== job.stack) {
      change.stack = body.stack ?? "";
    }
    if ((body.service ?? "") !== job.service) {
      change.service = body.service ?? "";
    }
    if ((body.engine ?? "") !== job.engine) {
      change.engine = body.engine ?? "";
    }
    return change;
  };

  const changed = job ? Object.keys(changes()).length > 0 : true;
  // what the labels set is not sent: it holds nothing back
  const ready =
    name.trim() !== "" &&
    sourceReady &&
    (scheduleValid || scheduleLocked) &&
    (keepValid || retentionLocked) &&
    changed &&
    !busy;

  const save = async () => {
    if (!ready) {
      return;
    }
    setBusy(true);
    setError("");
    try {
      const saved = job ? await jobsApi.change(job.id, changes()) : await jobsApi.add(full());
      toast.success(
        job
          ? t("backups.job.saved", { name: saved.name })
          : t("backups.job.added", { name: saved.name }),
      );
      // closed meanwhile: whatever is open now is another's
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

  const touchSchedule = () => setScheduleTouched(true);

  return (
    <ConfirmDialog
      open
      title={job ? t("backups.job.editTitle", { name: job.name }) : t("backups.job.newTitle")}
      confirmLabel={busy ? t("backups.saving") : job ? t("common.save") : t("backups.job.add")}
      confirmDisabled={!ready}
      onCancel={onClose}
      onConfirm={() => void save()}
    >
      <div className="dialog-form">
        <fieldset className="dialog-fieldset" disabled={busy}>
          <Field label={t("backups.name")} htmlFor="job-name">
            <input
              id="job-name"
              className="input"
              autoComplete="off"
              required
              maxLength={100}
              placeholder="vaultwarden"
              value={name}
              onChange={(event) => setName(event.target.value)}
            />
          </Field>

          {(fromLabel || automatic) && job ? (
            <>
              <dl className="facts">
                <dt>{t("backups.job.source")}</dt>
                <dd>{describeSource(job, stacks, t)}</dd>
                <dt>{t("backups.server")}</dt>
                <dd>{serverName(job.host)}</dd>
              </dl>
              <p className="muted small">
                {automatic
                  ? t("backups.job.automaticNote", {
                      what: automaticWhat(job),
                      host: serverName(job.host),
                    })
                  : t("backups.job.labelNote")}
              </p>
            </>
          ) : (
            <>
              <Field label={t("backups.job.source")} htmlFor="job-kind">
                <select
                  id="job-kind"
                  className="input"
                  value={kind}
                  onChange={(event) => {
                    setKind(event.target.value as SourceKind);
                    setSource("");
                    setError("");
                  }}
                >
                  {sourceKinds.map((item) => (
                    <option key={item} value={item}>
                      {t(`backups.sourceKind.${item}`)}
                    </option>
                  ))}
                </select>
                <p className="muted small">{t(`backups.sourceNote.${kind}`)}</p>
              </Field>

              {!usesStack && (
                <Field label={t("backups.server")} htmlFor="job-host">
                  <select
                    id="job-host"
                    className="input"
                    required
                    value={host}
                    onChange={(event) => setHost(event.target.value)}
                  >
                    {host === "" && <option value="">{t("backups.chooseServer")}</option>}
                    {hosts.map((item) => (
                      <option key={item.id} value={item.id}>
                        {serverOption(item, t("backups.serverOffline"))}
                      </option>
                    ))}
                  </select>
                  <p className="muted small">{t("backups.job.serverHint")}</p>
                </Field>
              )}

              {kind === "volume" && (
                <Field
                  label={t("backups.job.volume")}
                  htmlFor="job-volume"
                  error={volumeInvalid ? t("backups.job.volumeInvalid") : undefined}
                >
                  <input
                    id="job-volume"
                    className="input mono"
                    autoComplete="off"
                    spellCheck={false}
                    required
                    list="job-volumes"
                    placeholder="vw-data"
                    value={source}
                    onChange={(event) => setSource(event.target.value)}
                  />
                  <datalist id="job-volumes">
                    {volumes.items.map((volume) => (
                      <option key={volume.id} value={volume.name} />
                    ))}
                  </datalist>
                </Field>
              )}

              {kind === "path" && (
                <Field
                  label={t("backups.job.path")}
                  htmlFor="job-path"
                  error={pathInvalid ? t("backups.job.pathInvalid") : undefined}
                >
                  <input
                    id="job-path"
                    className="input mono"
                    autoComplete="off"
                    spellCheck={false}
                    required
                    placeholder="/srv/sites"
                    value={source}
                    onChange={(event) => setSource(event.target.value)}
                  />
                </Field>
              )}

              {usesStack && (
                <Field label={t("backups.job.stack")} htmlFor="job-stack">
                  <select
                    id="job-stack"
                    className="input"
                    required
                    value={stack}
                    onChange={(event) => {
                      setStack(event.target.value);
                      setService("");
                    }}
                  >
                    {stack === "" && <option value="">{t("backups.job.chooseStack")}</option>}
                    {stacks.map((item) => (
                      <option key={item.id} value={item.id}>
                        {t("backups.job.stackOption", {
                          name: item.name,
                          server: serverName(item.host),
                        })}
                      </option>
                    ))}
                  </select>
                </Field>
              )}

              {kind === "db_dump" && (
                <>
                  <div className="form-grid">
                    <Field label={t("backups.job.service")} htmlFor="job-service">
                      {services.length > 0 ? (
                        <select
                          id="job-service"
                          className="input"
                          required
                          value={service}
                          onChange={(event) => {
                            const value = event.target.value;
                            setService(value);
                            const image =
                              containers.items.find((item) => item.service === value)?.image ?? "";
                            const guess = guessEngine(image);
                            if (guess) {
                              setEngine(guess);
                            }
                          }}
                        >
                          {service === "" && (
                            <option value="">{t("backups.job.chooseService")}</option>
                          )}
                          {services.map((item) => (
                            <option key={item} value={item}>
                              {item}
                            </option>
                          ))}
                        </select>
                      ) : (
                        // a stack without containers yet: its service by name
                        <input
                          id="job-service"
                          className="input mono"
                          autoComplete="off"
                          spellCheck={false}
                          required
                          placeholder="db"
                          value={service}
                          onChange={(event) => setService(event.target.value)}
                        />
                      )}
                    </Field>
                    <Field label={t("backups.job.engine")} htmlFor="job-engine">
                      <select
                        id="job-engine"
                        className="input"
                        required
                        value={engine}
                        onChange={(event) => setEngine(event.target.value as DumpEngine)}
                      >
                        {engine === "" && <option value="">{t("backups.job.chooseEngine")}</option>}
                        {dumpEngines.map((item) => (
                          <option key={item} value={item}>
                            {t(`backups.engineName.${item}`)}
                          </option>
                        ))}
                      </select>
                    </Field>
                  </div>
                  {engine !== "" && (
                    <p className="muted small">{t(`backups.engineNote.${engine}`)}</p>
                  )}
                  {engine === "sqlite" && (
                    <Field
                      label={t("backups.job.sqlitePath")}
                      htmlFor="job-sqlite"
                      error={sqliteInvalid ? t("backups.job.sqliteInvalid") : undefined}
                    >
                      <input
                        id="job-sqlite"
                        className="input mono"
                        autoComplete="off"
                        spellCheck={false}
                        required
                        placeholder="/data/db.sqlite3"
                        value={source}
                        onChange={(event) => setSource(event.target.value)}
                      />
                    </Field>
                  )}
                </>
              )}
            </>
          )}

          {automatic && (
            <FollowSwitch
              label={t("backups.job.targets")}
              own={owned.targets}
              host={serverName(jobHost)}
              onChange={(own) => setOwned((current) => ({ ...current, targets: own }))}
            />
          )}
          <TargetPicker
            label={t("backups.job.targets")}
            targets={targets}
            chosen={chosen}
            onChange={setChosen}
            fits={fits}
            serverName={serverName}
            disabled={targetsLocked}
            none={t("backups.job.noTargetChosen")}
          >
            {labelSets.includes("targets") && (
              <p className="muted small">{t("backups.job.labelSets.targets")}</p>
            )}
          </TargetPicker>

          {automatic && (
            <FollowSwitch
              label={t("backups.job.schedule")}
              own={owned.schedule}
              host={serverName(jobHost)}
              onChange={(own) => setOwned((current) => ({ ...current, schedule: own }))}
            />
          )}
          <div className="form-grid">
            <Field label={t("backups.job.schedule")} htmlFor="job-schedule">
              <select
                id="job-schedule"
                className="input"
                disabled={scheduleLocked}
                value={mode}
                onChange={(event) => {
                  const next = event.target.value as ScheduleMode;
                  setMode(next);
                  touchSchedule();
                  if (next === "custom" && cron.trim() === "") {
                    setCron(preset ? presetCron(preset, offset) : "");
                  }
                }}
              >
                {scheduleModes.map((item) => (
                  <option key={item} value={item}>
                    {t(`backups.job.mode.${item}`)}
                  </option>
                ))}
              </select>
            </Field>
            {mode === "hourly" && (
              <Field label={t("backups.job.minute")} htmlFor="job-minute">
                <input
                  id="job-minute"
                  className="input"
                  type="number"
                  inputMode="numeric"
                  min={0}
                  max={59}
                  step={1}
                  disabled={scheduleLocked}
                  value={minute}
                  onChange={(event) => {
                    setMinute(event.target.value);
                    touchSchedule();
                  }}
                />
              </Field>
            )}
            {mode === "weekly" && (
              <Field label={t("backups.job.weekday")} htmlFor="job-weekday">
                <select
                  id="job-weekday"
                  className="input"
                  disabled={scheduleLocked}
                  value={weekday}
                  onChange={(event) => {
                    setWeekday(Number(event.target.value));
                    touchSchedule();
                  }}
                >
                  {weekdays.map((day) => (
                    <option key={day} value={day}>
                      {t(`backups.job.day.${day}`)}
                    </option>
                  ))}
                </select>
              </Field>
            )}
            {(mode === "daily" || mode === "weekly") && (
              <Field label={t("backups.job.time")} htmlFor="job-time">
                <input
                  id="job-time"
                  className="input"
                  type="time"
                  required
                  disabled={scheduleLocked}
                  value={time}
                  onChange={(event) => {
                    setTime(event.target.value);
                    touchSchedule();
                  }}
                />
              </Field>
            )}
          </div>
          {mode === "custom" ? (
            <Field label={t("backups.job.cron")} htmlFor="job-cron">
              <input
                id="job-cron"
                className="input mono"
                autoComplete="off"
                spellCheck={false}
                placeholder="0 3 * * *"
                disabled={scheduleLocked}
                value={cron}
                onChange={(event) => {
                  setCron(event.target.value);
                  touchSchedule();
                }}
              />
              <p className="muted small">
                {t("backups.job.cronHint")}
                {cronShaped(cron) &&
                  ` ${t("backups.job.cronReads", { text: describeCron(cron, t, offset) })}`}
              </p>
            </Field>
          ) : (
            preset && (
              <p className="muted small">
                {t("backups.job.utcNote", {
                  zone: zoneName(),
                  // on one line: no-break spaces between its fields
                  cron: presetCron(preset, offset).replace(/ /g, " "),
                })}
              </p>
            )
          )}
          {labelSets.includes("schedule") && (
            <p className="muted small">{t("backups.job.labelSets.schedule")}</p>
          )}

          {automatic && (
            <FollowSwitch
              label={t("backups.job.retention")}
              own={owned.retention}
              host={serverName(jobHost)}
              onChange={(own) => setOwned((current) => ({ ...current, retention: own }))}
            />
          )}
          <RetentionFields
            id="job-keep"
            label={t("backups.job.retention")}
            keep={keep}
            onChange={setKeep}
            disabled={retentionLocked}
          >
            {labelSets.includes("retention") && (
              <p className="muted small">{t("backups.job.labelSets.retention")}</p>
            )}
          </RetentionFields>

          <label className="check-line">
            <input
              type="checkbox"
              checked={enabled}
              onChange={(event) => setEnabled(event.target.checked)}
            />
            {t("backups.job.enabled")}
          </label>
        </fieldset>
        {error && <Banner tone="err">{error}</Banner>}
      </div>
    </ConfirmDialog>
  );
}
