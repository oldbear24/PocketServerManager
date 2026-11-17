import { ShieldCheck } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { CommandOutput } from "../components/CommandOutput";
import { useToast } from "../components/toast";
import {
  Button,
  ConfirmDialog,
  Drawer,
  ProgressBar,
  StatusPill,
  Timeline,
  type TimelineItem,
  type TimelineState,
} from "../components/ui";
import { api, errorMessage } from "../lib/api";
import {
  checkFields,
  formatCheckMoment,
  nextGroup,
  parseSubset,
  type BackupCheck,
  type CheckStatus,
} from "../lib/backupChecks";
import {
  backupsApi,
  checkBlocker,
  integrityStatus,
  integrityTone,
  type BackupTarget,
} from "../lib/backups";
import { formatDuration } from "../lib/format";
import { useMounted } from "../lib/mounted";
import { pb } from "../lib/pb";
import { useLiveList, useLiveRecord } from "../lib/realtime";
import type { Command, Host } from "../lib/types";
import i18n from "../i18n";

type Translate = (key: string, options?: Record<string, unknown>) => string;

/** The share of a repository one group is, as the check's confirmation says
 * it: about 5 % of 20 groups, with a decimal below one percent. */
function groupShare(groups: number): string {
  const share = 1 / groups;
  return new Intl.NumberFormat(i18n.language || "en", {
    style: "percent",
    maximumFractionDigits: share < 0.01 ? 1 : 0,
  }).format(share);
}

/** The group a check read, as its line says it: "group 3 of 20", or
 * "structure only"; nothing for a subset it cannot read. */
export function describeSubset(subset: string, t: Translate): string {
  if (subset === "") {
    return t("backups.integrity.structure");
  }
  const group = parseSubset(subset);
  return group ? t("backups.integrity.group", { n: group.n, t: group.t }) : "";
}

/** A check's step and how far it is, from its command's progress. */
function checkProgress(command: Command | null, t: Translate) {
  const progress = command?.progress;
  const percent = Math.max(0, Math.min(100, progress?.percent ?? 0));
  const step = progress?.step
    ? t(`backups.integrity.step.${progress.step}`, { defaultValue: progress.step })
    : "";
  const share = new Intl.NumberFormat(i18n.language || "en", {
    style: "percent",
    maximumFractionDigits: 0,
  }).format(percent / 100);
  return {
    percent,
    step,
    text: step ? t("backups.integrity.progress", { step, percent: share }) : "",
  };
}

/** The open check of a target: the newest queued or running of those given. */
export function openCheckOf<C extends Pick<BackupCheck, "target" | "status" | "kind">>(
  target: Pick<BackupTarget, "id">,
  checks: C[],
): C | null {
  return (
    checks.find(
      (check) =>
        check.target === target.id &&
        (check.kind ?? "integrity") === "integrity" &&
        (check.status === "queued" || check.status === "running"),
    ) ?? null
  );
}

/** Check now, for admins: refused with its reason as the title while the
 * panel would refuse it, and behind a confirmation that says what is read
 * and that only errors lock the repository. */
export function CheckNowButton({
  target,
  host,
  groups,
  open,
  size,
}: {
  target: BackupTarget;
  host: Host | undefined;
  groups: number;
  open: Pick<BackupCheck, "status"> | null;
  size?: "small";
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const mounted = useMounted();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const server = host?.name ?? "–";
  const blocker = checkBlocker(target, host, open);

  const start = async () => {
    setBusy(true);
    try {
      await backupsApi.check(target.id);
      toast.success(t("backups.integrity.started", { name: target.name }));
      if (mounted.current) {
        setConfirming(false);
      }
    } catch (err) {
      toast.error(
        t("backups.integrity.startFailed", { name: target.name, error: errorMessage(err) }),
      );
      if (mounted.current) {
        setConfirming(false);
      }
    } finally {
      if (mounted.current) {
        setBusy(false);
      }
    }
  };

  return (
    <>
      <Button
        size={size}
        aria-label={t("backups.integrity.checkNowName", { name: target.name })}
        title={blocker ? t(`backups.integrity.cannot.${blocker}`, { server }) : undefined}
        disabled={blocker !== null || busy}
        onClick={() => setConfirming(true)}
      >
        <ShieldCheck size={14} aria-hidden="true" />
        {busy ? t("backups.integrity.starting") : t("backups.integrity.checkNow")}
      </Button>
      <ConfirmDialog
        open={confirming}
        title={t("backups.integrity.confirmTitle", { name: target.name })}
        confirmLabel={busy ? t("backups.integrity.starting") : t("backups.integrity.confirm")}
        confirmDisabled={busy}
        onCancel={() => setConfirming(false)}
        onConfirm={() => void start()}
      >
        <p className="muted">
          {groups > 0
            ? t("backups.integrity.confirmText", {
                n: nextGroup(target, groups),
                t: groups,
                percent: groupShare(groups),
                server,
              })
            : t("backups.integrity.confirmTextStructure", { server })}
        </p>
      </ConfirmDialog>
    </>
  );
}

/** A target's integrity line: how its checks stand, what the newest that
 * completed read, when the next one is, and why the newest attempt did not
 * complete; Check now for admins and Details for everyone. */
export function IntegrityLine({
  target,
  host,
  open,
  last,
  groups,
  now,
  isAdmin,
  onDetails,
}: {
  target: BackupTarget;
  host: Host | undefined;
  /** its check queued or running, if any */
  open: BackupCheck | null;
  /** the row of its newest check, when loaded */
  last: BackupCheck | null;
  groups: number;
  now: number;
  isAdmin: boolean;
  onDetails: () => void;
}) {
  const { t } = useTranslation();
  const command = useLiveRecord<Command>("commands", open?.command || undefined).record;
  const status = integrityStatus(target, open);
  const progress = status === "running" ? checkProgress(command, t) : null;

  const attempt = target.last_check_status;
  const meta = [
    progress?.text,
    target.integrity_at &&
      t("backups.integrity.checkedAt", { when: formatCheckMoment(target.integrity_at, now) }),
    last && (last.status === "ok" || last.status === "failed") && describeSubset(last.subset, t),
    target.next_check_at &&
      t("backups.integrity.next", { when: formatCheckMoment(target.next_check_at, now) }),
    (attempt === "error" ||
      attempt === "suspect" ||
      attempt === "skipped" ||
      attempt === "cancelled") &&
      t("backups.integrity.lastAttempt", {
        when: formatCheckMoment(last?.finished_at || last?.created || "", now),
        reason: target.last_check_error || t(`backups.integrity.status.${attempt}`),
      }),
  ].filter(Boolean);
  const errors = target.integrity_status === "failed" ? firstLines(target.integrity_error, 3) : "";

  return (
    <>
      <div className="backup-target-integrity">
        <span className="backup-target-integrity-label">{t("backups.integrity.label")}</span>
        <StatusPill tone={integrityTone(target, open)}>
          {t(`backups.integrity.status.${status}`)}
        </StatusPill>
        {meta.length > 0 && <span>{meta.join(" · ")}</span>}
        <div className="row-actions">
          {isAdmin && (
            <CheckNowButton target={target} host={host} groups={groups} open={open} size="small" />
          )}
          <Button
            size="small"
            aria-label={t("backups.integrity.detailsName", { name: target.name })}
            onClick={onDetails}
          >
            {t("backups.integrity.details")}
          </Button>
        </div>
      </div>
      {errors && <pre className="backup-target-error failed">{errors}</pre>}
    </>
  );
}

function firstLines(text: string | undefined, count: number): string {
  return (text ?? "")
    .split(/\r?\n/)
    .filter((line) => line.trim() !== "")
    .slice(0, count)
    .join("\n");
}

const timelineState: Record<CheckStatus, TimelineState> = {
  queued: "active",
  running: "active",
  ok: "done",
  failed: "failed",
  suspect: "failed",
  error: "failed",
  skipped: "pending",
  cancelled: "pending",
};

/** A target's integrity checks (?check=<target>): what comes next, the check
 * under way with Stop for operators, the last ten, and what the one picked
 * found, how to repair it and its output. */
export function IntegrityDrawer({
  target,
  host,
  openChecks,
  groups,
  now,
  isAdmin,
  canRun,
  onClose,
}: {
  target: BackupTarget;
  host: Host | undefined;
  openChecks: BackupCheck[];
  groups: number;
  now: number;
  isAdmin: boolean;
  canRun: boolean;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const mounted = useMounted();
  const history = useLiveList<BackupCheck>("backup_checks", {
    filter: pb.filter("target = {:target} && kind = 'integrity'", { target: target.id }),
    fields: checkFields,
    sort: "-created",
    perPage: 10,
  });
  const rows = history.items
    .filter((row) => row.target === target.id)
    .sort((a, b) => (a.created < b.created ? 1 : a.created > b.created ? -1 : 0))
    .slice(0, 10);
  const open = openCheckOf(target, [...openChecks, ...rows]);
  const command = useLiveRecord<Command>("commands", open?.command || undefined).record;
  const progress = open?.status === "running" ? checkProgress(command, t) : null;
  const [picked, setPicked] = useState<string | null>(null);
  const selectedId = picked ?? rows.find((row) => row.id !== open?.id)?.id ?? rows[0]?.id;
  const selected = useLiveRecord<BackupCheck>("backup_checks", selectedId).record;
  const shown = selected && selected.id === selectedId ? selected : null;
  const [output, setOutput] = useState("");
  const [stopping, setStopping] = useState(false);
  const [stopBusy, setStopBusy] = useState(false);
  const status = integrityStatus(target, open);
  const lastOk =
    target.integrity_status === "ok"
      ? target.integrity_at
      : (rows.find((row) => row.status === "ok")?.finished_at ?? "");

  const stop = async () => {
    if (!open?.command) {
      return;
    }
    setStopBusy(true);
    try {
      await api.cancelCommand(open.command);
      toast.success(t("backups.integrity.stopping", { name: target.name }));
    } catch (err) {
      toast.error(err);
    } finally {
      if (mounted.current) {
        setStopBusy(false);
        setStopping(false);
      }
    }
  };

  const items: TimelineItem[] = rows.map((row) => {
    const when = formatCheckMoment(row.finished_at || row.started_at || row.created, now);
    const note = [
      when,
      row.status !== "queued" && row.status !== "skipped" && describeSubset(row.subset, t),
      row.duration_ms > 0 &&
        t("backups.integrity.took", { duration: formatDuration(row.duration_ms / 1000) }),
    ]
      .filter(Boolean)
      .join(" · ");
    const title = `${t(`backups.integrity.status.${row.status}`)} · ${t(`backups.integrity.trigger.${row.trigger}`)}`;
    return {
      id: row.id,
      state: timelineState[row.status] ?? "pending",
      title: (
        <button
          type="button"
          className={`link-button check-pick${row.id === selectedId ? " picked" : ""}`}
          aria-pressed={row.id === selectedId}
          aria-label={`${title}: ${t("backups.integrity.showCheck", { when })}`}
          onClick={() => {
            setPicked(row.id);
            setOutput("");
          }}
        >
          {title}
        </button>
      ),
      note,
    };
  });

  return (
    <Drawer
      title={t("backups.integrity.drawerTitle", { name: target.name })}
      onClose={onClose}
      actions={
        <>
          {isAdmin && <CheckNowButton target={target} host={host} groups={groups} open={open} />}
          <Button onClick={onClose}>{t("common.close")}</Button>
        </>
      }
    >
      <dl className="facts">
        <dt>{t("backups.integrity.fact.status")}</dt>
        <dd>
          <StatusPill tone={integrityTone(target, open)}>
            {t(`backups.integrity.status.${status}`)}
          </StatusPill>
        </dd>
        <dt>{t("backups.integrity.fact.next")}</dt>
        <dd>{target.next_check_at ? formatCheckMoment(target.next_check_at, now) : "–"}</dd>
        <dt>{t("backups.integrity.fact.group")}</dt>
        <dd>
          {groups > 0
            ? t("backups.integrity.group", { n: nextGroup(target, groups), t: groups })
            : t("backups.integrity.structure")}
        </dd>
        <dt>{t("backups.integrity.lastOk")}</dt>
        <dd>{lastOk ? formatCheckMoment(lastOk, now) : "–"}</dd>
      </dl>

      {open && (
        <section className="check-running" aria-label={t(`backups.integrity.status.${status}`)}>
          {progress && (
            <>
              <ProgressBar
                value={progress.percent}
                label={t("backups.integrity.progressLabel", { name: target.name })}
                warnAt={101}
                errAt={101}
              />
              {progress.text && <p className="muted small">{progress.text}</p>}
            </>
          )}
          {canRun && open.command && (
            <div>
              <Button variant="danger" disabled={stopBusy} onClick={() => setStopping(true)}>
                {t("backups.integrity.stop")}
              </Button>
            </div>
          )}
        </section>
      )}

      <section>
        <h3 className="drawer-heading">{t("backups.integrity.history")}</h3>
        {rows.length === 0 ? (
          <p className="muted">{t("backups.integrity.noHistory")}</p>
        ) : (
          <Timeline items={items} label={t("backups.integrity.history")} />
        )}
      </section>

      {shown && (
        <CheckFindings
          name={target.name}
          check={shown}
          now={now}
          output={output === shown.command}
          onOutput={() => setOutput((current) => (current === shown.command ? "" : shown.command))}
        />
      )}

      <ConfirmDialog
        open={stopping}
        title={t("backups.integrity.stopTitle", { name: target.name })}
        confirmLabel={t("backups.integrity.stop")}
        danger
        confirmDisabled={stopBusy}
        onCancel={() => setStopping(false)}
        onConfirm={() => void stop()}
      >
        <p className="muted">{t("backups.integrity.stopText")}</p>
      </ConfirmDialog>
    </Drawer>
  );
}

/** What one check found: restic's errors, whether the locked pass confirmed
 * them, how to repair the repository, its notes and its output. */
function CheckFindings({
  name,
  check,
  now,
  output,
  onOutput,
}: {
  /** the target's */
  name: string;
  check: BackupCheck;
  now: number;
  output: boolean;
  onOutput: () => void;
}) {
  const { t } = useTranslation();
  const errors = check.errors ?? [];
  const repair = check.repair;
  const packs = repair?.packs ?? [];
  const notes = check.notes ?? [];
  const when = formatCheckMoment(check.finished_at || check.started_at || check.created, now);
  const ended = check.status !== "queued" && check.status !== "running";

  return (
    <section className="check-findings" aria-label={t("backups.integrity.showCheck", { when })}>
      <h3 className="drawer-heading">{t("backups.integrity.found")}</h3>
      {check.packs_total > 0 && (
        <p className="muted small">
          {t("backups.integrity.packsRead", { read: check.packs_read, count: check.packs_total })}
        </p>
      )}
      {errors.length > 0 && (
        <>
          <p className="small">
            {t("backups.integrity.errors", { count: errors.length + (check.errors_more || 0) })}
          </p>
          <ul className="check-errors">
            {errors.map((error, index) => (
              <li key={index} className="mono">
                {error.text}
              </li>
            ))}
          </ul>
          {check.errors_more > 0 && (
            <p className="muted small">
              {t("backups.integrity.moreErrors", { count: check.errors_more })}
            </p>
          )}
        </>
      )}
      {check.status === "ok" && !check.unconfirmed && <p>{t("backups.integrity.noErrors")}</p>}
      {check.status === "ok" && check.unconfirmed && <p>{t("backups.integrity.unconfirmedOk")}</p>}
      {check.status === "failed" && check.confirmed && <p>{t("backups.integrity.confirmed")}</p>}
      {check.status === "suspect" && <p>{t("backups.integrity.suspect")}</p>}
      {check.error && (check.status !== "failed" || errors.length === 0) && (
        <pre className="backup-target-error failed">{check.error}</pre>
      )}

      {repair && (repair.index || packs.length > 0 || repair.snapshots || repair.other) && (
        <>
          <h3 className="drawer-heading">{t("backups.integrity.repair")}</h3>
          <ul className="check-repair">
            {repair.index && <li>{t("backups.integrity.repairIndex")}</li>}
            {packs.length > 0 && (
              <li>{t("backups.integrity.repairPacks", { ids: packs.join(" ") })}</li>
            )}
            {(repair.other || (repair.snapshots && packs.length === 0)) && (
              <li>{t("backups.integrity.repairOther")}</li>
            )}
          </ul>
          <p className="muted small">{t("backups.integrity.repairNote")}</p>
        </>
      )}

      {notes.length > 0 && (
        <>
          <h3 className="drawer-heading">{t("backups.integrity.notes")}</h3>
          <ul className="check-notes">
            {notes.map((note, index) => (
              <li key={index}>{note}</li>
            ))}
          </ul>
        </>
      )}

      {check.command && (
        <div>
          <Button size="small" aria-expanded={output} onClick={onOutput}>
            {t("backups.integrity.showOutput")}
          </Button>
        </div>
      )}
      {output && check.command && (
        <CommandOutput
          commandId={check.command}
          title={t("backups.integrity.logTitle", { name })}
          progress={when}
          status={ended ? undefined : t("backups.integrity.status.running")}
          onClose={onOutput}
        />
      )}
    </section>
  );
}
