import { Pencil, Trash2 } from "lucide-react";
import { useMemo, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";

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
} from "../components/ui";
import { errorMessage } from "../lib/api";
import {
  cronPreset,
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
  nextJob,
  parseKeep,
  parseTime,
  presetCron,
  retentionKeys,
  runMoment,
  runStrip,
  sameRetention,
  shortError,
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
  type Retention,
  type RetentionKey,
  type RunStatus,
  type SchedulePreset,
  type SourceKind,
  type StripDay,
} from "../lib/backupJobs";
import { serverOption, type BackupTarget } from "../lib/backups";
import { formatBytes, formatNumber } from "../lib/format";
import { useMounted } from "../lib/mounted";
import { pb } from "../lib/pb";
import { useLiveList } from "../lib/realtime";
import type { Container, Host, Stack, Volume } from "../lib/types";

/** The stacks as the jobs need them: no compose files. */
export type StackName = Pick<Stack, "id" | "name" | "host">;

/** The tiles above the jobs: how the last 14 days went, what the targets
 * hold, and which job runs next. */
export function BackupSummary({
  jobs,
  runs,
  targets,
  hosts,
  now,
}: {
  jobs: BackupJob[];
  runs: BackupRun[];
  targets: BackupTarget[];
  hosts: Host[];
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
    </section>
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

function daySummary(day: Pick<StripDay, "succeeded" | "failed" | "skipped">, t: Translate) {
  const parts = (["succeeded", "failed", "skipped"] as const)
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
    }),
    { succeeded: 0, failed: 0, skipped: 0 },
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
 * volumes of a stack, or a dump of a service. */
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
    case "stack":
      text = t("backups.source.stack", { name: stack });
      break;
    case "db_dump":
      text = t("backups.source.db_dump", {
        tool: job.engine ? t(`backups.engineTool.${job.engine}`) : "–",
        stack,
        service: job.service,
      });
      break;
    default:
      text = job.source;
  }
  return job.origin === "label" ? `${text} · ${t("backups.job.fromLabel")}` : text;
}

/** How the job's last runs went: the newest per target, the worst first. */
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
      : [target?.name, shortError(run.error)].filter(Boolean).join(": ");
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
    </>
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
  onAdd,
  onEdit,
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
  onAdd: () => void;
  onEdit: (job: BackupJob) => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const [starting, setStarting] = useState<ReadonlySet<string>>(new Set());
  const [removing, setRemoving] = useState<BackupJob | null>(null);
  const [removeBusy, setRemoveBusy] = useState(false);
  const offset = zoneOffset(new Date(now));

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
            {jobs.map((job) => {
              const jobRuns = runsByJob.get(job.id) ?? [];
              const names = (job.targets ?? [])
                .map((id) => targets.find((target) => target.id === id)?.name)
                .filter(Boolean);
              const server = hosts.find((host) => host.id === job.host)?.name ?? "–";
              const source = describeSource(job, stacks, t);
              const runnable = !job.label_removed && names.length > 0;
              const removable = job.origin !== "label" || job.label_removed;
              return (
                <tr key={job.id} className={job.label_removed ? "job-removed" : undefined}>
                  <td className="job-cell">
                    <div className="job-name">{job.name}</div>
                    <div className="job-source" title={source}>
                      {source}
                    </div>
                    {job.problem && !job.label_removed && (
                      <div className="job-problem">{job.problem}</div>
                    )}
                    {job.label_removed && (
                      <div className="job-problem">
                        <span>{t("backups.job.labelRemoved")}</span>
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
                    <span className="mono job-host">{server}</span>
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
                      {!job.enabled && !job.label_removed && (
                        <span className="faint">{` · ${t("backups.job.paused")}`}</span>
                      )}
                    </div>
                    <div className="mono job-retention" title={t("backups.retention.title")}>
                      {describeRetention(job.retention, t)}
                    </div>
                  </td>
                  <td className="job-last">
                    <LastRun job={job} runs={jobRuns} targets={targets} now={now} />
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
                              : job.label_removed
                                ? t("backups.job.labelRemoved")
                                : t("backups.job.cannotRun")
                          }
                          disabled={!runnable || starting.has(job.id)}
                          onClick={() => void run(job)}
                        >
                          {starting.has(job.id) ? t("backups.job.starting") : t("backups.job.run")}
                        </Button>
                      )}
                      {isAdmin && !job.label_removed && (
                        <IconButton
                          bare
                          aria-label={t("backups.job.editName", { name: job.name })}
                          onClick={() => onEdit(job)}
                        >
                          <Pencil size={15} aria-hidden="true" />
                        </IconButton>
                      )}
                      {isAdmin && removable && !job.label_removed && (
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
      <Card title={t("backups.job.title")} side={<StripLegend />} flush>
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

type ScheduleMode = SchedulePreset["kind"] | "custom";

const scheduleModes: ScheduleMode[] = ["hourly", "daily", "weekly", "custom"];

/** Monday first, as the week starts where the panel is used. */
const weekdays = [1, 2, 3, 4, 5, 6, 0];

/** What the retention fields hold, as typed: empty keeps nothing of that kind. */
type KeepForm = Record<RetentionKey, string>;

function keepForm(retention: Retention): KeepForm {
  const form = {} as KeepForm;
  for (const key of retentionKeys) {
    form[key] = retention[key] > 0 ? String(retention[key]) : "";
  }
  return form;
}

/** As the label's default: 7 daily, 4 weekly and 6 monthly snapshots. */
const defaultRetention: Retention = {
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

/** Whether a cron expression has the shape the panel takes: five fields, or
 * a macro such as @daily. The panel checks the rest. */
function cronShaped(cron: string): boolean {
  const text = cron.trim();
  return /^@\w+$/.test(text) || text.split(/\s+/).length === 5;
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
  // what the job's labels set, as they set it
  const labelSets = fromLabel ? (job?.label_sets ?? []) : [];
  const scheduleLocked = labelSets.includes("schedule");
  const targetsLocked = labelSets.includes("targets");
  const retentionLocked = labelSets.includes("retention");
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
  const jobHost = fromLabel ? (job?.host ?? "") : usesStack ? (stackRecord?.host ?? "") : host;
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

  const parsedKeep = {} as Record<RetentionKey, number | null>;
  for (const key of retentionKeys) {
    parsedKeep[key] = parseKeep(keep[key]);
  }
  const keepValid = retentionKeys.every((key) => parsedKeep[key] !== null);
  const retention = fullRetention(parsedKeep as Partial<Retention>);

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

          {fromLabel && job ? (
            <>
              <dl className="facts">
                <dt>{t("backups.job.source")}</dt>
                <dd>{describeSource(job, stacks, t)}</dd>
                <dt>{t("backups.server")}</dt>
                <dd>{serverName(job.host)}</dd>
              </dl>
              <p className="muted small">{t("backups.job.labelNote")}</p>
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

          <fieldset className="job-targets" disabled={targetsLocked}>
            <legend className="field-label">{t("backups.job.targets")}</legend>
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
                        setChosen((current) =>
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
            {targets.length > 0 && selected.length === 0 && (
              <p className="warn-text small">{t("backups.job.noTargetChosen")}</p>
            )}
            {targetsLocked && <p className="muted small">{t("backups.job.labelSets.targets")}</p>}
          </fieldset>

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
          {scheduleLocked && <p className="muted small">{t("backups.job.labelSets.schedule")}</p>}

          <fieldset className="job-retention-fields" disabled={retentionLocked}>
            <legend className="field-label">{t("backups.job.retention")}</legend>
            <div className="keep-grid">
              {retentionKeys.map((key) => (
                <Field key={key} label={t(`backups.keep.${key}`)} htmlFor={`job-keep-${key}`}>
                  <input
                    id={`job-keep-${key}`}
                    className="input"
                    type="number"
                    inputMode="numeric"
                    min={0}
                    max={10000}
                    step={1}
                    placeholder="0"
                    value={keep[key]}
                    onChange={(event) =>
                      setKeep((current) => ({ ...current, [key]: event.target.value }))
                    }
                  />
                </Field>
              ))}
            </div>
            {keepValid ? (
              <p className="muted small">
                {t("backups.job.retentionHint", { keeps: describeRetention(retention, t) })}
              </p>
            ) : (
              <p className="field-error">{t("backups.job.retentionInvalid")}</p>
            )}
            {retentionLocked && (
              <p className="muted small">{t("backups.job.labelSets.retention")}</p>
            )}
          </fieldset>

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
