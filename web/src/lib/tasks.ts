import type { RecordModel } from "pocketbase";

import { pb } from "./pb";

/** A command the panel runs on a cron schedule, in UTC, in a PHP site's
 * container or in a stack's service (#51). */
export type ScheduledTask = RecordModel & {
  target_kind: "site" | "stack";
  site: string;
  stack: string;
  service: string;
  host: string;
  schedule: string;
  command: string;
  user: string;
  timeout_s: number;
  enabled: boolean;
  last_run_at: string;
  last_status: "" | "running" | "succeeded" | "failed" | "timed_out" | "skipped" | "cancelled";
  last_exit_code: number;
  last_duration_ms: number;
  last_output: string;
  last_command: string;
  failures: number;
};

/** Schedules offered before one of its own. */
export const schedulePresets = [
  "*/5 * * * *",
  "*/15 * * * *",
  "0 * * * *",
  "30 3 * * *",
  "0 4 * * 0",
] as const;

export const tasksApi = {
  run: (id: string) => pb.send<{ command: string }>(`/api/pc/tasks/${id}/run`, { method: "POST" }),
};

type Translate = (key: string, options?: Record<string, unknown>) => string;

const pad = (value: string) => value.padStart(2, "0");

/** A schedule in words where it is a common one, else as it is written. */
export function describeSchedule(schedule: string, t: Translate): string {
  const macros: Record<string, string> = {
    "@hourly": "0 * * * *",
    "@daily": "0 0 * * *",
    "@midnight": "0 0 * * *",
    "@weekly": "0 0 * * 0",
    "@monthly": "0 0 1 * *",
    "@yearly": "0 0 1 1 *",
    "@annually": "0 0 1 1 *",
  };
  const fields = (macros[schedule.trim()] ?? schedule.trim()).split(/\s+/);
  if (fields.length !== 5) {
    return schedule;
  }
  const [minute, hour, day, month, weekday] = fields;
  const number = /^\d+$/;
  const step = /^\*\/(\d+)$/.exec(minute);
  if (hour === "*" && day === "*" && month === "*" && weekday === "*") {
    if (minute === "*") {
      return t("task.every.minutes", { count: 1 });
    }
    // */7 runs at :00, :07 … :56 and again at :00, which is no "every 7 min"
    if (step && Number(step[1]) >= 1 && Number(step[1]) < 60 && 60 % Number(step[1]) === 0) {
      return t("task.every.minutes", { count: Number(step[1]) });
    }
    if (number.test(minute)) {
      return t("task.every.hour", { minute: pad(minute) });
    }
  }
  if (number.test(minute) && number.test(hour) && month === "*") {
    const time = `${pad(hour)}:${pad(minute)}`;
    if (day === "*" && weekday === "*") {
      return t("task.every.day", { time });
    }
    if (day === "*" && /^[0-7]$/.test(weekday)) {
      return t("task.every.week", { day: t(`task.weekday.${Number(weekday) % 7}`), time });
    }
    if (number.test(day) && weekday === "*") {
      return t("task.every.month", { day: Number(day), time });
    }
  }
  return schedule;
}
