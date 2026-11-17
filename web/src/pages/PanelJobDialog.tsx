import { useState } from "react";
import { useTranslation } from "react-i18next";

import { useToast } from "../components/toast";
import { Banner, ConfirmDialog, Field } from "../components/ui";
import { errorMessage } from "../lib/api";
import {
  cronPreset,
  cronShaped,
  describeCron,
  describeRetention,
  fullRetention,
  jobsApi,
  panelCapability,
  parseKeep,
  parseTime,
  presetCron,
  retentionKeys,
  sameRetention,
  timeValue,
  zoneOffset,
  type BackupJob,
  type BackupJobChange,
  type NewBackupJob,
  type Retention,
  type RetentionKey,
  type SchedulePreset,
} from "../lib/backupJobs";
import type { BackupTarget } from "../lib/backups";
import { useMounted } from "../lib/mounted";
import type { Host } from "../lib/types";
import { ServerField } from "./Backups";

/** The panel job's name, as the design has it. */
export const panelJobName = "panel (pb_data)";

/** Every day at 04:00 local time, as the design has it. */
export const panelSchedule: SchedulePreset = { kind: "daily", hour: 4, minute: 0 };

/** 14 daily, 8 weekly and 12 monthly snapshots: "14d 8w 12m". */
export const panelRetention: Retention = {
  last: 0,
  hourly: 0,
  daily: 14,
  weekly: 8,
  monthly: 12,
  yearly: 0,
};

/** Whether a server's agent can carry the panel's backup. */
export function canCarry(host: Pick<Host, "capabilities">): boolean {
  return Boolean(host.capabilities?.includes(panelCapability));
}

type ScheduleMode = SchedulePreset["kind"] | "custom";

const scheduleModes: ScheduleMode[] = ["hourly", "daily", "weekly", "custom"];

/** Monday first, as the week starts where the panel is used. */
const weekdays = [1, 2, 3, 4, 5, 6, 0];

type KeepForm = Record<RetentionKey, string>;

function keepForm(retention: Retention): KeepForm {
  const form = {} as KeepForm;
  for (const key of retentionKeys) {
    form[key] = retention[key] > 0 ? String(retention[key]) : "";
  }
  return form;
}

function sameTargets(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((id) => b.includes(id));
}

function zoneName(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
}

/** The panel job's dialog as the Backups page opens it: to change the job,
 * or to set it up while there is none. One that another admin sets up
 * meanwhile does not turn the dialog into its own: it says so instead. */
export function PanelJobSetup({
  panelJob,
  targets,
  hosts,
  onClose,
}: {
  /** the panel's job, as it is now */
  panelJob: BackupJob | null;
  targets: BackupTarget[];
  hosts: Host[];
  onClose: () => void;
}) {
  const [settingUp] = useState(panelJob === null);
  return (
    <PanelJobDialog
      job={settingUp ? null : panelJob}
      exists={settingUp && panelJob !== null}
      targets={targets}
      hosts={hosts}
      onClose={onClose}
    />
  );
}

/** Sets up the panel's own backup (#32), or changes it: the server whose
 * agent uploads the panel's encrypted copy, which only an agent that can
 * carry it may be, the targets, the schedule in local time and the
 * retention. The panel builds the copy; nothing of a server is backed up.
 * The job dialog of the other kinds stays as it is. */
export function PanelJobDialog({
  job,
  targets,
  hosts,
  exists = false,
  onClose,
}: {
  /** null to set it up */
  job: BackupJob | null;
  targets: BackupTarget[];
  hosts: Host[];
  /** the panel has its job already, though this dialog sets one up */
  exists?: boolean;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const mounted = useMounted();
  const [offset] = useState(() => zoneOffset());
  const [initial] = useState<SchedulePreset | null>(() =>
    job ? cronPreset(job.schedule, offset) : panelSchedule,
  );

  const [name, setName] = useState(job?.name ?? panelJobName);
  const [carrier, setCarrier] = useState(job?.host ?? "");
  const [chosen, setChosen] = useState<string[]>(job?.targets ?? []);
  const [mode, setMode] = useState<ScheduleMode>(initial?.kind ?? "custom");
  const [minute, setMinute] = useState(String(initial?.kind === "hourly" ? initial.minute : 0));
  const [time, setTime] = useState(
    initial && initial.kind !== "hourly" ? timeValue(initial.hour, initial.minute) : "04:00",
  );
  const [weekday, setWeekday] = useState(initial?.kind === "weekly" ? initial.weekday : 1);
  const [cron, setCron] = useState(job?.schedule ?? "");
  const [scheduleTouched, setScheduleTouched] = useState(false);
  const [keep, setKeep] = useState<KeepForm>(() =>
    keepForm(job ? fullRetention(job.retention) : panelRetention),
  );
  const [enabled, setEnabled] = useState(job?.enabled ?? true);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const serverName = (id: string) => hosts.find((item) => item.id === id)?.name ?? "–";
  const carrierHost = hosts.find((item) => item.id === carrier);
  const noneCan = !hosts.some(canCarry);
  // the job's own carrier, whose agent was not updated
  const carrierOld = carrierHost !== undefined && !canCarry(carrierHost);

  // a local target is a directory of its own server: only the carrier's
  const fits = (target: BackupTarget) => target.kind !== "local" || target.host === carrier;
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

  const full = (): NewBackupJob => ({
    name: name.trim(),
    host: carrier,
    source_kind: "panel",
    targets: targetIds,
    schedule,
    retention,
    enabled,
  });

  const changes = (): BackupJobChange => {
    if (!job) {
      return {};
    }
    const body = full();
    const change: BackupJobChange = {};
    if (body.name !== job.name) {
      change.name = body.name;
    }
    if (body.host !== job.host) {
      change.host = body.host;
    }
    if (!sameTargets(body.targets, job.targets ?? [])) {
      change.targets = body.targets;
    }
    if (body.schedule !== job.schedule) {
      change.schedule = body.schedule;
    }
    if (!sameRetention(body.retention, job.retention)) {
      change.retention = body.retention;
    }
    if (body.enabled !== job.enabled) {
      change.enabled = body.enabled;
    }
    return change;
  };

  const changed = job ? Object.keys(changes()).length > 0 : true;
  const ready =
    name.trim() !== "" &&
    carrier !== "" &&
    scheduleValid &&
    keepValid &&
    changed &&
    !(exists && !job) &&
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
      title={job ? t("backups.panel.editTitle") : t("backups.panel.dialogTitle")}
      confirmLabel={busy ? t("backups.saving") : job ? t("common.save") : t("backups.job.add")}
      confirmDisabled={!ready}
      onCancel={onClose}
      onConfirm={() => void save()}
    >
      <div className="dialog-form">
        <p className="muted">{t("backups.panel.what")}</p>
        {exists && !job && <Banner tone="warn">{t("backups.job.panelExists")}</Banner>}
        <fieldset className="dialog-fieldset" disabled={busy}>
          <Field label={t("backups.name")} htmlFor="panel-job-name">
            <input
              id="panel-job-name"
              className="input"
              autoComplete="off"
              required
              maxLength={100}
              value={name}
              onChange={(event) => setName(event.target.value)}
            />
          </Field>

          <ServerField
            id="panel-job-carrier"
            label={t("backups.job.carrier")}
            hosts={hosts}
            value={carrier}
            onChange={setCarrier}
            // the job's own carrier stays its value, though it cannot carry it now
            unavailable={(host) => !canCarry(host) && host.id !== job?.host}
            hint={t("backups.job.carrierHint")}
          />
          {noneCan ? (
            <Banner tone="warn">{t("backups.job.carrierNone")}</Banner>
          ) : (
            carrierOld && (
              <Banner tone="warn">
                {t("backups.job.carrierOld", { name: carrierHost?.name ?? "" })}
              </Banner>
            )
          )}

          <fieldset className="job-targets">
            <legend className="field-label">{t("backups.job.targets")}</legend>
            {targets.length === 0 ? (
              <p className="muted small">{t("backups.job.noTargets")}</p>
            ) : (
              targets.map((target) => {
                const fit = fits(target);
                return (
                  <div key={target.id}>
                    <label className="check-line">
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
                    {fit && target.kind === "local" && (
                      <p className="warn-text small">
                        {t("backups.job.panelLocal", {
                          target: target.name,
                          server: serverName(target.host),
                        })}
                      </p>
                    )}
                  </div>
                );
              })
            )}
            {targets.length > 0 && selected.length === 0 && (
              <p className="warn-text small">{t("backups.job.noTargetChosen")}</p>
            )}
          </fieldset>

          <div className="form-grid">
            <Field label={t("backups.job.schedule")} htmlFor="panel-job-schedule">
              <select
                id="panel-job-schedule"
                className="input"
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
              <Field label={t("backups.job.minute")} htmlFor="panel-job-minute">
                <input
                  id="panel-job-minute"
                  className="input"
                  type="number"
                  inputMode="numeric"
                  min={0}
                  max={59}
                  step={1}
                  value={minute}
                  onChange={(event) => {
                    setMinute(event.target.value);
                    touchSchedule();
                  }}
                />
              </Field>
            )}
            {mode === "weekly" && (
              <Field label={t("backups.job.weekday")} htmlFor="panel-job-weekday">
                <select
                  id="panel-job-weekday"
                  className="input"
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
              <Field label={t("backups.job.time")} htmlFor="panel-job-time">
                <input
                  id="panel-job-time"
                  className="input"
                  type="time"
                  required
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
            <Field label={t("backups.job.cron")} htmlFor="panel-job-cron">
              <input
                id="panel-job-cron"
                className="input mono"
                autoComplete="off"
                spellCheck={false}
                placeholder="0 3 * * *"
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
                  cron: presetCron(preset, offset).replace(/ /g, " "),
                })}
              </p>
            )
          )}

          <fieldset className="job-retention-fields">
            <legend className="field-label">{t("backups.job.retention")}</legend>
            <div className="keep-grid">
              {retentionKeys.map((key) => (
                <Field key={key} label={t(`backups.keep.${key}`)} htmlFor={`panel-job-keep-${key}`}>
                  <input
                    id={`panel-job-keep-${key}`}
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
