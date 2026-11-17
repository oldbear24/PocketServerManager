import { AlertCircle, AlertTriangle, RefreshCw, ShieldAlert } from "lucide-react";
import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { useToast } from "../components/toast";
import { Banner, Button, ConfirmDialog, IssueBanner } from "../components/ui";
import { errorMessage } from "../lib/api";
import { czechAt, formatCheckMoment } from "../lib/backupChecks";
import {
  failedRuns,
  formatMoment,
  holderState,
  jobsApi,
  lockStaleAt,
  maxAttempts,
  resticLine,
  runIssue,
  runMoment,
  type BackupJob,
  type BackupLock,
  type BackupRun,
  type HolderState,
  type RunIssue,
} from "../lib/backupJobs";
import type { BackupTarget } from "../lib/backups";
import { formatRelative, parseDate } from "../lib/format";
import { useMounted } from "../lib/mounted";
import { useLiveRecord } from "../lib/realtime";
import type { Command, Host } from "../lib/types";
import i18n from "../i18n";

type Translate = (key: string, options?: Record<string, unknown>) => string;

/** The banners shown at once; the rest wait behind "Show N more problems". */
const shownAtOnce = 3;

/** A time of day, local, as the banners say it: "03:00". */
function clock(value: string | Date): string {
  const date = value instanceof Date ? value : parseDate(value);
  if (!date) {
    return "–";
  }
  return new Intl.DateTimeFormat(i18n.language || "en", {
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
  }).format(date);
}

function sameDay(date: Date, now: number): boolean {
  return date.toDateString() === new Date(now).toDateString();
}

/** The problems of the backups, under the summary (#31): first each target
 * whose integrity check found errors, then the runs that failed, newest
 * first, three at a time. A labelled region, not an alert: a live region
 * would read them out again at every realtime update. */
export function FailureBanners({
  jobs,
  runs,
  targets,
  hosts,
  now,
  canRun,
  onShowLog,
  onDetails,
}: {
  jobs: BackupJob[];
  runs: BackupRun[];
  targets: BackupTarget[];
  hosts: Host[];
  now: number;
  /** operators try a run again */
  canRun: boolean;
  /** shows a command's output: a run's log */
  onShowLog: (command: string) => void;
  /** shows a target's integrity checks */
  onDetails: (target: BackupTarget) => void;
}) {
  const { t } = useTranslation();
  const [expanded, setExpanded] = useState(false);

  const banners: ReactNode[] = [
    ...targets
      .filter((target) => target.status !== "pending" && target.integrity_status === "failed")
      .map((target) => (
        <IntegrityBanner key={`check-${target.id}`} target={target} onDetails={onDetails} />
      )),
    ...failedRuns(jobs, runs).flatMap((run) => {
      const job = jobs.find((item) => item.id === run.job);
      if (!job) {
        return [];
      }
      return [
        <RunBanner
          key={run.id}
          run={run}
          previous={run.retry_of ? runs.find((item) => item.id === run.retry_of) : undefined}
          job={job}
          target={targets.find((item) => item.id === run.target)}
          hosts={hosts}
          now={now}
          canRun={canRun}
          onShowLog={onShowLog}
        />,
      ];
    }),
  ];
  if (banners.length === 0) {
    return null;
  }
  const more = banners.length - shownAtOnce;
  return (
    <section className="failure-banners" aria-label={t("backups.failure.label")}>
      {expanded ? banners : banners.slice(0, shownAtOnce)}
      {more > 0 && (
        <div>
          <Button size="small" aria-expanded={expanded} onClick={() => setExpanded(!expanded)}>
            {expanded ? t("backups.failure.fewer") : t("backups.failure.more", { count: more })}
          </Button>
        </div>
      )}
    </section>
  );
}

/** A target whose newest completed check confirmed errors. */
function IntegrityBanner({
  target,
  onDetails,
}: {
  target: BackupTarget;
  onDetails: (target: BackupTarget) => void;
}) {
  const { t } = useTranslation();
  const first = (target.integrity_error ?? "").split(/\r?\n/).find((line) => line.trim() !== "");
  return (
    <IssueBanner
      id={`integrity-${target.id}`}
      tone="err"
      icon={<ShieldAlert size={20} aria-hidden="true" />}
      title={t("backups.failure.integrity", { target: target.name })}
      detail={first}
      actions={
        <Button onClick={() => onDetails(target)}>{t("backups.failure.integrityDetails")}</Button>
      }
    />
  );
}

const toneIcon: Record<RunIssue["tone"], ReactNode> = {
  err: <AlertCircle size={20} aria-hidden="true" />,
  warn: <AlertTriangle size={20} aria-hidden="true" />,
  info: <RefreshCw size={20} aria-hidden="true" />,
};

/** Who holds a run's lock, in a sentence, and when restic counts it as
 * stale while that may still come. */
function holderLines(lock: BackupLock, holder: HolderState, now: number, t: Translate): string[] {
  const lines: string[] = [];
  if (holder === "alive") {
    lines.push(
      t("backups.failure.holderCommand", {
        label: lock.command_label || lock.command,
        server: lock.command_host_name || "–",
      }),
    );
    return lines;
  }
  if (holder === "gone" && lock.command) {
    lines.push(
      t("backups.failure.holderEnded", {
        label: lock.command_label || lock.command,
        server: lock.command_host_name || "–",
      }),
    );
  } else {
    lines.push(
      t("backups.failure.holderOther", {
        host: lock.host || "–",
        pid: lock.pid,
        time: lock.created ? formatMoment(lock.created, now) : "–",
      }),
    );
  }
  const staleAt = lockStaleAt(lock);
  if (staleAt !== null && staleAt > now) {
    lines.push(
      t("backups.failure.staleAt", { time: formatMoment(new Date(staleAt).toISOString(), now) }),
    );
  }
  return lines;
}

/** A run that failed, or the retry of one under way: what restic said, who
 * holds the lock, which attempt it was and what comes next, and what an
 * operator may do. */
function RunBanner({
  run,
  previous,
  job,
  target,
  hosts,
  now,
  canRun,
  onShowLog,
}: {
  run: BackupRun;
  /** the run a retry tries again, when loaded */
  previous: BackupRun | undefined;
  job: BackupJob;
  target: BackupTarget | undefined;
  hosts: Host[];
  now: number;
  canRun: boolean;
  onShowLog: (command: string) => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const mounted = useMounted();
  const [busy, setBusy] = useState(false);
  const [unlocking, setUnlocking] = useState(false);
  // a retry under way speaks of the run it tries again
  const failed = run.status === "failed" ? run : (previous ?? run);
  const lock = failed.failure === "locked" ? (failed.lock ?? null) : null;
  const holderCommand = useLiveRecord<Command>("commands", lock?.command || undefined);
  // until the holder's command is read, it may still run
  const holder: HolderState =
    lock?.command && holderCommand.loading
      ? "alive"
      : holderState(lock, holderCommand.record ?? undefined, hosts);
  const issue = runIssue(run, holder, job);
  const server = hosts.find((host) => host.id === (run.host || job.host))?.name ?? "–";
  const targetName = target?.name ?? "–";

  const moment = runMoment(failed);
  const at = parseDate(moment);
  const title =
    at && sameDay(at, now)
      ? t(`backups.failure.${czechAt(at)}`, { job: job.name, server, time: clock(at) })
      : t("backups.failure.titleDay", {
          job: job.name,
          server,
          when: formatCheckMoment(moment, now),
        });
  const said = resticLine(failed.error);
  const detail = said && lock?.created ? `${said} (${clock(lock.created)})` : said;

  const attempt = run.attempt || 1;
  const state = stateLine(issue.state, run, job, now, t);
  const facts = [
    (job.targets ?? []).length > 1 && t("backups.failure.target", { target: targetName }),
    t("backups.failure.attempt", { attempt, max: Math.max(maxAttempts, attempt) }),
    issue.state !== "invalidLock" && state,
  ].filter(Boolean);
  const meta = (
    <>
      {lock && holderLines(lock, holder, now, t).map((line) => <div key={line}>{line}</div>)}
      <div>{facts.join(" · ")}</div>
      {issue.state === "invalidLock" && <div>{state}</div>}
    </>
  );

  const retry = async () => {
    setBusy(true);
    try {
      const answer = await jobsApi.retry(failed.id);
      toast.success(
        answer.retry_at
          ? t("backups.failure.scheduled", {
              job: job.name,
              time: formatMoment(answer.retry_at, now),
            })
          : t("backups.failure.started", { job: job.name, target: targetName }),
      );
    } catch (err) {
      toast.error(t("backups.failure.retryFailed", { error: errorMessage(err) }));
    } finally {
      if (mounted.current) {
        setBusy(false);
      }
    }
  };

  const variant = issue.tone === "warn" ? "warning" : "critical";
  return (
    <>
      <IssueBanner
        id={`run-${run.id}`}
        tone={issue.tone}
        icon={toneIcon[issue.tone]}
        title={title}
        detail={detail || undefined}
        meta={meta}
        actions={
          <>
            {canRun && issue.action === "unlock" && (
              <Button
                variant={variant}
                disabled={issue.held || busy}
                title={issue.held ? t("backups.failure.heldTitle") : undefined}
                onClick={() => setUnlocking(true)}
              >
                {t("backups.failure.unlockRetry")}
              </Button>
            )}
            {canRun && issue.action === "retry" && (
              <Button variant={variant} disabled={busy} onClick={() => void retry()}>
                {t("backups.failure.retry")}
              </Button>
            )}
            {run.command && (
              <Button onClick={() => onShowLog(run.command ?? "")}>
                {t("backups.failure.showLog")}
              </Button>
            )}
          </>
        }
      />
      {unlocking && (
        <UnlockRetryDialog
          run={failed}
          job={job}
          target={targetName}
          server={server}
          now={now}
          onClose={() => setUnlocking(false)}
        />
      )}
    </>
  );
}

/** What comes next for a run, as its banner's last line says it. */
function stateLine(
  state: RunIssue["state"],
  run: Pick<BackupRun, "retry_at">,
  job: Pick<BackupJob, "next_run_at">,
  now: number,
  t: Translate,
): string {
  switch (state) {
    case "retryAt":
      return t("backups.failure.retryAt", { time: formatMoment(run.retry_at ?? "", now) });
    case "nextRun":
      return t("backups.failure.nextRun", { time: formatMoment(job.next_run_at, now) });
    default:
      return t(`backups.failure.${state}`);
  }
}

/** Unlock and retry: what restic unlock removes and what it leaves, the lock
 * as restic named it, and on which server the retry runs. For a lock restic
 * does not count as stale yet, the retry is scheduled for when it will. */
export function UnlockRetryDialog({
  run,
  job,
  target,
  server,
  now,
  onClose,
}: {
  run: Pick<BackupRun, "id" | "lock">;
  job: Pick<BackupJob, "name">;
  /** the target's name */
  target: string;
  /** the job's server, whose agent runs the retry */
  server: string;
  now: number;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const mounted = useMounted();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const lock = run.lock ?? null;
  const staleAt = lockStaleAt(lock);
  const later = staleAt !== null && now < staleAt;

  const confirm = async () => {
    if (busy) {
      return;
    }
    setBusy(true);
    setError("");
    try {
      const answer = await jobsApi.retry(run.id);
      toast.success(
        answer.retry_at
          ? t("backups.failure.scheduled", {
              job: job.name,
              time: formatMoment(answer.retry_at, now),
            })
          : t("backups.failure.started", { job: job.name, target }),
      );
      if (mounted.current) {
        onClose();
      }
    } catch (err) {
      if (mounted.current) {
        // the panel's refusal, as a clock that is off, keeps the dialog open
        setError(errorMessage(err));
      } else {
        toast.error(t("backups.failure.retryFailed", { error: errorMessage(err) }));
      }
    } finally {
      if (mounted.current) {
        setBusy(false);
      }
    }
  };

  const holder = lock?.command_label
    ? `${lock.command_label}${lock.command_host_name ? ` (${lock.command_host_name})` : ""}`
    : "restic";
  return (
    <ConfirmDialog
      open
      title={t("backups.unlock.title", { target, job: job.name })}
      confirmLabel={
        busy
          ? t("backups.unlock.busy")
          : later
            ? t("backups.unlock.confirmLater", {
                time: formatMoment(new Date(staleAt).toISOString(), now),
              })
            : t("backups.unlock.confirm")
      }
      confirmDisabled={busy}
      onCancel={onClose}
      onConfirm={() => void confirm()}
    >
      <p className="muted">{t("backups.unlock.text", { job: job.name })}</p>
      {lock && (
        <dl className="facts">
          <dt>{t("backups.unlock.fact.holder")}</dt>
          <dd>{holder}</dd>
          <dt>{t("backups.unlock.fact.host")}</dt>
          <dd className="mono">{lock.host || "–"}</dd>
          <dt>{t("backups.unlock.fact.pid")}</dt>
          <dd className="mono">{lock.pid || "–"}</dd>
          <dt>{t("backups.unlock.fact.refreshed")}</dt>
          <dd>
            {lock.created
              ? `${formatMoment(lock.created, now)} (${formatRelative(lock.created, now, { past: true })})`
              : "–"}
          </dd>
          <dt>{t("backups.unlock.fact.kind")}</dt>
          <dd>{lock.exclusive ? t("backups.unlock.exclusive") : t("backups.unlock.shared")}</dd>
        </dl>
      )}
      <p className="muted small">{t("backups.unlock.agentNote", { server })}</p>
      {error && <Banner tone="err">{error}</Banner>}
    </ConfirmDialog>
  );
}
