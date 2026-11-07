import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import i18n from "../i18n";
import { pb } from "../lib/pb";
import { describeSchedule, tasksApi, type ScheduledTask } from "../lib/tasks";
import { TaskList } from "./TaskList";
import { ToastProvider } from "./toast";

const lists: Record<string, unknown[]> = {};

vi.mock("../lib/realtime", () => ({
  useLiveList: (collection: string) => {
    const items = lists[collection] ?? [];
    return { items, total: items.length, loading: false, error: null, reload: () => {} };
  },
}));

beforeAll(() => {
  HTMLDialogElement.prototype.showModal = function showModal(this: HTMLDialogElement) {
    this.open = true;
  };
  HTMLDialogElement.prototype.close = function close(this: HTMLDialogElement) {
    this.open = false;
  };
});

afterEach(() => {
  vi.restoreAllMocks();
  for (const key of Object.keys(lists)) {
    delete lists[key];
  }
});

const task = (fields: Partial<ScheduledTask>): ScheduledTask =>
  ({
    id: "t1",
    target_kind: "site",
    site: "s1",
    schedule: "*/5 * * * *",
    command: "wp cron event run --due-now",
    timeout_s: 300,
    enabled: true,
    last_status: "succeeded",
    last_run_at: "2026-11-07 13:45:00.000Z",
    last_duration_ms: 812,
    last_output: "",
    failures: 0,
    ...fields,
  }) as ScheduledTask;

describe("scheduled tasks (#51)", () => {
  it("puts common schedules in words, in both languages", async () => {
    await i18n.changeLanguage("en");
    const t = i18n.t.bind(i18n) as (key: string, options?: Record<string, unknown>) => string;
    expect(describeSchedule("*/5 * * * *", t)).toBe("every 5 min");
    expect(describeSchedule("* * * * *", t)).toBe("every minute");
    expect(describeSchedule("15 * * * *", t)).toBe("hourly at :15");
    expect(describeSchedule("30 3 * * *", t)).toBe("daily 03:30 UTC");
    expect(describeSchedule("@weekly", t)).toBe("Sundays 00:00 UTC");
    expect(describeSchedule("0 4 1 * *", t)).toBe("monthly on the 1. 04:00 UTC");
    expect(describeSchedule("5 4 * 2 1-5", t)).toBe("5 4 * 2 1-5");
    await i18n.changeLanguage("cs");
    expect(describeSchedule("*/5 * * * *", t)).toBe("každých 5 min");
    expect(describeSchedule("*/2 * * * *", t)).toBe("každé 2 min");
    await i18n.changeLanguage("en");
  });

  it("shows how each task ran last, runs one now and adds one", async () => {
    await i18n.changeLanguage("en");
    lists.scheduled_tasks = [
      task({}),
      task({
        id: "t2",
        schedule: "30 3 * * *",
        command: "wp transient delete --expired",
        last_status: "failed",
        last_output: "Error: Error establishing a database connection.",
        failures: 3,
      }),
    ];
    const run = vi.spyOn(tasksApi, "run").mockResolvedValue({ command: "c1" });
    const create = vi
      .spyOn(pb.collection("scheduled_tasks"), "create")
      .mockResolvedValue({} as never);
    render(
      <ToastProvider>
        <TaskList target={{ kind: "site", id: "s1" }} canAct />
      </ToastProvider>,
    );

    const items = screen.getAllByRole("listitem");
    expect(within(items[0]).getByText(/every 5 min/)).toBeInTheDocument();
    expect(within(items[0]).getByText(/^OK \d\d:\d\d$/)).toBeInTheDocument();
    expect(within(items[1]).getByText("3 failures in a row", { exact: false })).toBeInTheDocument();
    // the output of a failed run, at hand
    expect(
      within(items[1]).getByText("Error: Error establishing a database connection."),
    ).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Run wp cron event run --due-now now" }));
    await waitFor(() => expect(run).toHaveBeenCalledWith("t1"));

    fireEvent.click(screen.getByRole("button", { name: "Add a task" }));
    fireEvent.change(screen.getByLabelText("Command"), {
      target: { value: "php artisan schedule:run" },
    });
    fireEvent.change(screen.getByLabelText("Schedule"), { target: { value: "custom" } });
    fireEvent.change(screen.getByLabelText("Cron expression"), { target: { value: "* * * * *" } });
    fireEvent.change(screen.getByLabelText("Timeout (s)"), { target: { value: "120" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(create).toHaveBeenCalledWith({
        command: "php artisan schedule:run",
        schedule: "* * * * *",
        timeout_s: 120,
        enabled: true,
        target_kind: "site",
        site: "s1",
      }),
    );
  });

  it("offers no changes to a viewer", async () => {
    await i18n.changeLanguage("en");
    lists.scheduled_tasks = [task({})];
    render(
      <ToastProvider>
        <TaskList target={{ kind: "stack", id: "st1", services: ["app"] }} canAct={false} />
      </ToastProvider>,
    );
    expect(screen.queryByRole("button", { name: /Run/ })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add a task" })).toBeNull();
  });
});
