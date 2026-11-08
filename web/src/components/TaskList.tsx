import { Pencil, Play, Plus, Trash2 } from "lucide-react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { errorMessage } from "../lib/api";
import { formatDuration, formatNumber, formatTime } from "../lib/format";
import { pb } from "../lib/pb";
import { useLiveList } from "../lib/realtime";
import { describeSchedule, schedulePresets, tasksApi, type ScheduledTask } from "../lib/tasks";
import { useToast } from "./toast";
import { Button, ConfirmDialog, Field, IconButton, type Tone } from "./ui";

/** Where a task runs: a PHP site, or a stack whose services it picks from. */
export type TaskTarget =
  { kind: "site"; id: string } | { kind: "stack"; id: string; services: string[] };

/** The scheduled tasks of a site or a stack (#51): what each runs, when, and
 * how its last run went, with Run now. */
export function TaskList({ target, canAct }: { target: TaskTarget; canAct: boolean }) {
  const { t } = useTranslation();
  const toast = useToast();
  const tasks = useLiveList<ScheduledTask>("scheduled_tasks", {
    filter: pb.filter(target.kind === "site" ? "site = {:id}" : "stack = {:id}", { id: target.id }),
    sort: "created",
  });
  const [editing, setEditing] = useState<ScheduledTask | "new" | null>(null);
  const [removing, setRemoving] = useState<ScheduledTask | null>(null);
  const [busy, setBusy] = useState("");

  const run = async (task: ScheduledTask) => {
    setBusy(task.id);
    try {
      await tasksApi.run(task.id);
      toast.success(t("task.started"));
    } catch (reason) {
      toast.error(reason);
    } finally {
      setBusy("");
    }
  };
  const remove = async (task: ScheduledTask) => {
    try {
      await pb.collection("scheduled_tasks").delete(task.id);
      toast.success(t("task.removed"));
    } catch (reason) {
      toast.error(reason);
    } finally {
      setRemoving(null);
    }
  };

  return (
    <div className="task-list">
      {tasks.items.length === 0 ? (
        <p className="muted small">{t("task.none")}</p>
      ) : (
        <ul>
          {tasks.items.map((task) => (
            <li key={task.id} className="task">
              <code className="mono task-command">{task.command}</code>
              <div className="task-meta">
                <TaskNote task={task} />
                {canAct && (
                  <span className="task-actions">
                    <IconButton
                      bare
                      aria-label={t("task.runNow", { command: task.command })}
                      title={t("task.runNowShort")}
                      disabled={busy === task.id || task.last_status === "running"}
                      onClick={() => void run(task)}
                    >
                      <Play size={14} aria-hidden="true" />
                    </IconButton>
                    <IconButton
                      bare
                      aria-label={t("task.edit", { command: task.command })}
                      title={t("common.edit")}
                      onClick={() => setEditing(task)}
                    >
                      <Pencil size={14} aria-hidden="true" />
                    </IconButton>
                    <IconButton
                      bare
                      aria-label={t("task.remove", { command: task.command })}
                      title={t("task.removeShort")}
                      onClick={() => setRemoving(task)}
                    >
                      <Trash2 size={14} aria-hidden="true" />
                    </IconButton>
                  </span>
                )}
              </div>
              {task.last_output && task.last_status !== "succeeded" && (
                <details className="task-output">
                  <summary>{t("task.output")}</summary>
                  <pre className="mono">{task.last_output}</pre>
                </details>
              )}
            </li>
          ))}
        </ul>
      )}
      {canAct && (
        <Button size="small" onClick={() => setEditing("new")}>
          <Plus size={13} aria-hidden="true" />
          {t("task.add")}
        </Button>
      )}
      {editing && (
        <TaskDialog
          target={target}
          task={editing === "new" ? null : editing}
          onClose={() => setEditing(null)}
        />
      )}
      <ConfirmDialog
        open={removing !== null}
        title={t("task.removeTitle")}
        confirmLabel={t("task.removeShort")}
        danger
        onCancel={() => setRemoving(null)}
        onConfirm={() => removing && void remove(removing)}
      >
        <p className="mono break">{removing?.command}</p>
      </ConfirmDialog>
    </div>
  );
}

const statusTone: Record<string, Tone> = {
  running: "info",
  succeeded: "ok",
  failed: "err",
  timed_out: "err",
  skipped: "warn",
};

/** A run's duration: tenths of a second for a short one, as most are. */
function taskDuration(ms: number): string {
  return ms < 60_000 ? `${formatNumber(ms / 1000, 1)} s` : formatDuration(ms / 1000);
}

/** When a task runs, and how its last run went: every 5 min · OK 13:45 · 0.8 s. */
function TaskNote({ task }: { task: ScheduledTask }) {
  const { t } = useTranslation();
  const parts = [describeSchedule(task.schedule, t)];
  if (!task.enabled) {
    parts.push(t("task.paused"));
  }
  return (
    <div className="muted small task-note">
      <span>{parts.join(" · ")}</span>
      {task.last_status && (
        <>
          {" · "}
          <span className={`task-status ${statusTone[task.last_status] ?? "off"}`}>
            {t(`task.status.${task.last_status}`)}
            {task.last_run_at ? ` ${formatTime(task.last_run_at).slice(0, 5)}` : ""}
          </span>
          {task.last_status !== "running" && task.last_duration_ms > 0 && (
            <span>{` · ${taskDuration(task.last_duration_ms)}`}</span>
          )}
          {task.failures > 1 && <span>{` · ${t("task.failures", { count: task.failures })}`}</span>}
        </>
      )}
    </div>
  );
}

function TaskDialog({
  target,
  task,
  onClose,
}: {
  target: TaskTarget;
  task: ScheduledTask | null;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const known = target.kind === "stack" ? target.services : [];
  const [values, setValues] = useState({
    command: task?.command ?? "",
    schedule: task?.schedule ?? schedulePresets[0],
    timeout: String(task?.timeout_s ?? 300),
    enabled: task?.enabled ?? true,
    service: task?.service ?? known[0] ?? "",
    user: task?.user ?? "",
  });
  const preset = (schedulePresets as readonly string[]).includes(values.schedule)
    ? values.schedule
    : "custom";
  const [custom, setCustom] = useState(preset === "custom");
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const set = (change: Partial<typeof values>) =>
    setValues((current) => ({ ...current, ...change }));
  // the task's own service stays a choice when it has no container now
  const services =
    values.service.trim() !== "" &&
    !known.includes(values.service) &&
    task?.service === values.service
      ? [values.service, ...known]
      : known;
  // the stack's services may arrive after the dialog opened
  const first = known[0] ?? "";
  useEffect(() => {
    if (first !== "") {
      setValues((current) => (current.service === "" ? { ...current, service: first } : current));
    }
  }, [first]);
  const timeout = Number(values.timeout);
  const valid =
    values.command.trim() !== "" &&
    values.schedule.trim() !== "" &&
    Number.isInteger(timeout) &&
    timeout >= 1 &&
    (target.kind === "site" || values.service.trim() !== "");

  const save = async () => {
    setBusy(true);
    setErrors({});
    const body: Record<string, unknown> = {
      command: values.command.trim(),
      schedule: values.schedule.trim(),
      timeout_s: timeout,
      enabled: values.enabled,
      ...(target.kind === "stack"
        ? { service: values.service.trim(), user: values.user.trim() }
        : {}),
    };
    try {
      if (task) {
        await pb.collection("scheduled_tasks").update(task.id, body);
      } else {
        await pb.collection("scheduled_tasks").create({
          ...body,
          target_kind: target.kind,
          [target.kind]: target.id,
        });
      }
      toast.success(t("task.saved"));
      onClose();
    } catch (reason) {
      const data = (reason as { response?: { data?: Record<string, { message?: string }> } })
        .response?.data;
      const fields: Record<string, string> = {};
      for (const [key, value] of Object.entries(data ?? {})) {
        if (value?.message) {
          fields[key] = value.message;
        }
      }
      setErrors(Object.keys(fields).length > 0 ? fields : { form: errorMessage(reason) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <ConfirmDialog
      open
      title={task ? t("task.editTitle") : t("task.addTitle")}
      confirmLabel={t("common.save")}
      confirmDisabled={busy || !valid}
      onCancel={onClose}
      onConfirm={() => void save()}
    >
      <div className="dialog-form">
        <Field label={t("task.command")} htmlFor="task-command" error={errors.command}>
          <input
            id="task-command"
            className="input mono"
            placeholder={
              target.kind === "site" ? "wp cron event run --due-now" : "php occ background:cron"
            }
            value={values.command}
            onChange={(event) => set({ command: event.target.value })}
          />
        </Field>
        <p className="muted small">
          {target.kind === "site" ? t("task.siteNote") : t("task.stackNote")}
        </p>
        <div className="form-row">
          <Field label={t("task.schedule")} htmlFor="task-preset">
            <select
              id="task-preset"
              className="select"
              value={custom ? "custom" : preset}
              onChange={(event) => {
                if (event.target.value === "custom") {
                  setCustom(true);
                } else {
                  setCustom(false);
                  set({ schedule: event.target.value });
                }
              }}
            >
              {schedulePresets.map((item) => (
                <option key={item} value={item}>
                  {describeSchedule(item, t)}
                </option>
              ))}
              <option value="custom">{t("task.customSchedule")}</option>
            </select>
          </Field>
          <Field label={t("task.timeout")} htmlFor="task-timeout" error={errors.timeout_s}>
            <input
              id="task-timeout"
              className="input"
              type="number"
              min={1}
              max={86400}
              value={values.timeout}
              onChange={(event) => set({ timeout: event.target.value })}
            />
          </Field>
        </div>
        {custom && (
          <Field label={t("task.cron")} htmlFor="task-cron" error={errors.schedule}>
            <input
              id="task-cron"
              className="input mono"
              placeholder="*/10 * * * *"
              value={values.schedule}
              onChange={(event) => set({ schedule: event.target.value })}
            />
          </Field>
        )}
        {!custom && errors.schedule && <p className="field-error">{errors.schedule}</p>}
        <p className="muted small">{t("task.scheduleNote")}</p>
        {target.kind === "stack" && (
          <div className="form-row">
            <Field label={t("task.service")} htmlFor="task-service" error={errors.service}>
              {services.length > 0 ? (
                <select
                  id="task-service"
                  className="select"
                  value={values.service}
                  onChange={(event) => set({ service: event.target.value })}
                >
                  {services.map((service) => (
                    <option key={service} value={service}>
                      {service}
                    </option>
                  ))}
                </select>
              ) : (
                // a stack without containers yet: its service by name
                <input
                  id="task-service"
                  className="input mono"
                  value={values.service}
                  onChange={(event) => set({ service: event.target.value })}
                />
              )}
            </Field>
            <Field label={t("task.user")} htmlFor="task-user" error={errors.user}>
              <input
                id="task-user"
                className="input mono"
                placeholder={t("task.userDefault")}
                value={values.user}
                onChange={(event) => set({ user: event.target.value })}
              />
            </Field>
          </div>
        )}
        <label className="check-line">
          <input
            type="checkbox"
            checked={values.enabled}
            onChange={(event) => set({ enabled: event.target.checked })}
          />
          {t("task.enabled")}
        </label>
        {errors.form && <p className="field-error">{errors.form}</p>}
      </div>
    </ConfirmDialog>
  );
}
