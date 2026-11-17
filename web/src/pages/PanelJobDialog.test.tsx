import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ClientResponseError } from "pocketbase";
import { useState } from "react";
import { MemoryRouter } from "react-router-dom";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { ToastProvider } from "../components/toast";
import i18n from "../i18n";
import { jobsApi, type BackupJob } from "../lib/backupJobs";
import type { BackupTarget } from "../lib/backups";
import type { Host } from "../lib/types";
import { canCarry, PanelJobDialog, PanelJobSetup } from "./PanelJobDialog";

const zone = process.env.TZ;

beforeAll(() => {
  // presets are in the browser's time, here Prague's: an hour ahead of UTC in winter
  process.env.TZ = "Europe/Prague";
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(Date.parse("2026-11-09T12:00:00Z"));
  HTMLDialogElement.prototype.showModal = function showModal(this: HTMLDialogElement) {
    this.open = true;
  };
  HTMLDialogElement.prototype.close = function close(this: HTMLDialogElement) {
    this.open = false;
  };
});

afterAll(() => {
  vi.useRealTimers();
  if (zone === undefined) {
    delete process.env.TZ;
  } else {
    process.env.TZ = zone;
  }
});

afterEach(async () => {
  vi.restoreAllMocks();
  await i18n.changeLanguage("en");
});

const caps = ["backup.run", "backup.check", "backup.run.panel"];
const hosts = [
  { id: "h1", name: "vm", status: "online", capabilities: caps },
  // an agent from before #32
  { id: "h2", name: "node-c", status: "online", capabilities: ["backup.run", "backup.check"] },
  // too old to say what it can
  { id: "h3", name: "node-old", status: "online", capabilities: null },
  { id: "h4", name: "node-b", status: "offline", capabilities: caps },
] as Host[];

function target(fields: Partial<BackupTarget>): BackupTarget {
  return {
    id: "t1",
    collectionId: "backup_targets",
    collectionName: "backup_targets",
    name: "panel-sftp",
    kind: "sftp",
    host: "h1",
    config: null,
    location: "sftp:backup@192.0.2.2:/restic/panel32",
    status: "ok",
    ...fields,
  } as BackupTarget;
}

const sftp = target({});
const disk = target({ id: "t2", name: "panel-disk", kind: "local", host: "h1" });
const nasDisk = target({ id: "t3", name: "nas-disk", kind: "local", host: "h2" });
const minio = target({ id: "t4", name: "panel-minio", kind: "s3", host: "h4" });
const targets = [sftp, disk, nasDisk, minio];

const panelJob = {
  id: "jp",
  collectionId: "backup_jobs",
  collectionName: "backup_jobs",
  name: "panel (pb_data)",
  host: "h1",
  source_kind: "panel",
  source: "",
  stack: "",
  service: "",
  engine: "",
  targets: ["t1", "t2"],
  // 04:00 in Prague, in winter
  schedule: "0 3 * * *",
  retention: { daily: 14, weekly: 8, monthly: 12 },
  enabled: true,
  origin: "manual",
  label_removed: false,
  problem: "",
  next_run_at: "",
  last_run_at: "",
  last_status: "",
} as BackupJob;

function open(job: BackupJob | null, options: { hosts?: Host[]; exists?: boolean } = {}) {
  const onClose = vi.fn();
  render(
    <MemoryRouter>
      <ToastProvider>
        <PanelJobDialog
          job={job}
          targets={targets}
          hosts={options.hosts ?? hosts}
          exists={options.exists}
          onClose={onClose}
        />
      </ToastProvider>
    </MemoryRouter>,
  );
  return onClose;
}

function form(name = "Back up the panel") {
  return within(screen.getByRole("dialog", { name }));
}

function confirmButton(name = "Back up the panel", label = "Add job") {
  return form(name).getByRole("button", { name: label });
}

describe("setting up the panel's backup", () => {
  it("offers only servers whose agent can carry it", () => {
    expect(canCarry(hosts[0])).toBe(true);
    expect(canCarry(hosts[1])).toBe(false);
    expect(canCarry(hosts[2])).toBe(false);
    open(null);
    const carrier = form().getByLabelText("Server that uploads it");
    const options = within(carrier).getAllByRole("option") as HTMLOptionElement[];
    expect(options.map((option) => [option.textContent, option.disabled])).toEqual([
      ["Choose a server", false],
      ["vm", false],
      ["node-c", true],
      ["node-old", true],
      ["node-b (offline)", false],
    ]);
    expect(
      form().getByText(
        "Its agent fetches the panel's encrypted copy and backs it up with restic in a job container. Choose a server other than the panel's own machine.",
      ),
    ).toBeInTheDocument();
    expect(form().queryByRole("status")).not.toBeInTheDocument();
  });

  it("says when no server's agent can carry it yet", () => {
    open(null, { hosts: [hosts[1], hosts[2]] });
    expect(form().getByRole("status")).toHaveTextContent(
      "No server's agent can back up the panel yet: update the agents.",
    );
    expect(confirmButton()).toBeDisabled();
  });

  it("is daily at 04:00 local time and keeps 14d 8w 12m by default", async () => {
    const add = vi.spyOn(jobsApi, "add").mockResolvedValue(panelJob);
    const onClose = open(null);
    expect(form().getByText(/^The panel's database and stored files, encrypted/)).toBeVisible();
    expect(form().getByLabelText("Name")).toHaveValue("panel (pb_data)");
    expect(form().getByLabelText("Schedule")).toHaveValue("daily");
    expect(form().getByLabelText("Time")).toHaveValue("04:00");
    expect(form().getByText(/The panel keeps it in UTC, as 0 3 \* \* \*/)).toBeInTheDocument();
    expect(form().getByText(/Now: 14d 8w 12m\./)).toBeInTheDocument();
    expect(form().getByLabelText("Daily")).toHaveValue(14);
    expect(form().getByLabelText("Weekly")).toHaveValue(8);
    expect(form().getByLabelText("Monthly")).toHaveValue(12);
    // the carrier is chosen, not guessed: the panel's own machine is a poor one
    expect(form().getByLabelText("Server that uploads it")).toHaveValue("");
    expect(confirmButton()).toBeDisabled();

    fireEvent.change(form().getByLabelText("Server that uploads it"), { target: { value: "h1" } });
    fireEvent.click(form().getByLabelText("panel-sftp"));
    fireEvent.click(form().getByLabelText("panel-minio"));
    fireEvent.click(confirmButton());

    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(add).toHaveBeenCalledWith({
      name: "panel (pb_data)",
      host: "h1",
      source_kind: "panel",
      targets: ["t1", "t4"],
      schedule: "0 3 * * *",
      retention: { last: 0, hourly: 0, daily: 14, weekly: 8, monthly: 12, yearly: 0 },
      enabled: true,
    });
    expect(await screen.findByText("The job panel (pb_data) was added.")).toBeInTheDocument();
  });

  it("warns that a target on the carrier's disk goes down with it, and offers no other's", () => {
    open(null);
    fireEvent.change(form().getByLabelText("Server that uploads it"), { target: { value: "h1" } });
    expect(form().getByLabelText("panel-disk")).toBeEnabled();
    expect(
      form().getByText(
        "panel-disk is a directory of vm: if that is the panel's machine, one failure takes the panel and its backup.",
      ),
    ).toHaveClass("warn-text");
    // a directory of another server cannot take the carrier's upload
    expect(form().getByLabelText(/^nas-disk/)).toBeDisabled();
    expect(form().getByText(/a directory of node-c: only its jobs back up to it/)).toBeVisible();
    expect(form().queryByText(/^nas-disk is a directory of node-c/)).not.toBeInTheDocument();
    // no warning for a remote target
    expect(form().queryByText(/^panel-sftp is a directory/)).not.toBeInTheDocument();
  });

  it("warns in Czech too", async () => {
    await act(() => i18n.changeLanguage("cs"));
    open(null);
    const dialog = within(screen.getByRole("dialog", { name: "Záloha panelu" }));
    fireEvent.change(dialog.getByLabelText("Server, který ji nahrává"), {
      target: { value: "h1" },
    });
    expect(
      dialog.getByText(
        "panel-disk je složka serveru vm: pokud je to stroj panelu, jedna havárie vezme panel i jeho zálohu.",
      ),
    ).toBeInTheDocument();
    expect(dialog.getByText(/Panel plán uloží v UTC jako 0 3 \* \* \*/)).toBeInTheDocument();
    expect(dialog.getByText(/Teď: 14d 8w 12m\./)).toBeInTheDocument();
  });

  it("shows why the panel refused, and stays open", async () => {
    vi.spyOn(jobsApi, "add").mockRejectedValue(
      new ClientResponseError({
        status: 409,
        response: { message: "The panel has its backup job already." },
      }),
    );
    const onClose = open(null);
    fireEvent.change(form().getByLabelText("Server that uploads it"), { target: { value: "h1" } });
    fireEvent.click(form().getByLabelText("panel-sftp"));
    fireEvent.click(confirmButton());
    expect(await form().findByRole("alert")).toHaveTextContent(
      "The panel has its backup job already.",
    );
    expect(onClose).not.toHaveBeenCalled();
  });

  it("says so, and adds none, when another admin set it up meanwhile", () => {
    function Page() {
      const [job, setJob] = useState<BackupJob | null>(null);
      return (
        <ToastProvider>
          <button type="button" onClick={() => setJob(panelJob)}>
            arrives
          </button>
          <PanelJobSetup panelJob={job} targets={targets} hosts={hosts} onClose={() => {}} />
        </ToastProvider>
      );
    }
    render(
      <MemoryRouter>
        <Page />
      </MemoryRouter>,
    );
    fireEvent.change(form().getByLabelText("Server that uploads it"), { target: { value: "h1" } });
    fireEvent.click(form().getByLabelText("panel-sftp"));
    expect(confirmButton()).toBeEnabled();
    fireEvent.click(screen.getByRole("button", { name: "arrives" }));
    expect(form().getByRole("status")).toHaveTextContent("The panel has its backup job already.");
    expect(confirmButton()).toBeDisabled();
  });
});

describe("changing the panel's backup", () => {
  it("shows its schedule in local time, and sends only what changed", async () => {
    const change = vi.spyOn(jobsApi, "change").mockResolvedValue(panelJob);
    open(panelJob);
    const name = "Panel backup";
    expect(form(name).getByLabelText("Server that uploads it")).toHaveValue("h1");
    expect(form(name).getByLabelText("Time")).toHaveValue("04:00");
    expect(form(name).getByLabelText("panel-sftp")).toBeChecked();
    expect(form(name).getByLabelText("panel-disk")).toBeChecked();
    expect(confirmButton(name, "Save")).toBeDisabled();

    fireEvent.change(form(name).getByLabelText("Server that uploads it"), {
      target: { value: "h4" },
    });
    // panel-disk is vm's: it does not go with another carrier
    expect(form(name).getByLabelText(/^panel-disk/)).toBeDisabled();
    fireEvent.click(confirmButton(name, "Save"));
    await waitFor(() => expect(change).toHaveBeenCalledWith("jp", { host: "h4", targets: ["t1"] }));
  });

  it("says when its carrier's agent cannot carry it, and keeps it chosen", () => {
    open({ ...panelJob, host: "h2", targets: ["t1"] });
    const name = "Panel backup";
    expect(form(name).getByLabelText("Server that uploads it")).toHaveValue("h2");
    expect(form(name).getByRole("status")).toHaveTextContent(
      "node-c's agent cannot back up the panel yet: update it.",
    );
    const options = within(form(name).getByLabelText("Server that uploads it")).getAllByRole(
      "option",
    ) as HTMLOptionElement[];
    expect(options.find((option) => option.value === "h2")?.disabled).toBe(false);
    expect(options.find((option) => option.value === "h3")?.disabled).toBe(true);
  });
});
