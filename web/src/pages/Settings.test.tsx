import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ClientResponseError } from "pocketbase";
import { MemoryRouter } from "react-router-dom";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { ToastProvider } from "../components/toast";
import { api } from "../lib/api";
import { AuthProvider } from "../lib/auth";
import { pb, type User } from "../lib/pb";
import type { PanelInfo } from "../lib/types";
import { AboutCard, BackupChecksCard, SettingsPage } from "./Settings";

vi.mock("../lib/realtime", () => ({
  useLiveList: () => ({ items: [], total: 0, loading: false, error: null, reload: () => {} }),
}));

const zone = process.env.TZ;

beforeAll(() => {
  // a zone without summer time: the cron's local reading stays the same
  process.env.TZ = "Asia/Tokyo";
});

afterAll(() => {
  if (zone === undefined) {
    delete process.env.TZ;
  } else {
    process.env.TZ = zone;
  }
});

afterEach(() => {
  vi.restoreAllMocks();
  pb.authStore.clear();
});

const info = {
  version: "v0.1.0",
  public_url: "https://panel.example.com",
  grpc_address: "panel.example.com:9443",
  gateway_running: true,
  ca_pin: "sha256:ab",
  agent_image: "",
  updater_image: "",
  images_pinned: true,
  agents: { connected: 0, total: 0 },
} as PanelInfo;

function about(fields: Partial<PanelInfo>) {
  vi.spyOn(api, "info").mockResolvedValue({ ...info, ...fields });
  return render(
    <MemoryRouter>
      <AboutCard />
    </MemoryRouter>,
  );
}

describe("the panel's HTTPS in Settings (#63)", () => {
  it("links to the certificate the panel serves", async () => {
    about({ https: true, panel_certificate: "p1" });
    const link = await screen.findByRole("link", { name: "The panel's own certificate" });
    expect(link.getAttribute("href")).toBe("/domains?certificate=p1");
  });

  it("says when it is still ordered", async () => {
    about({ https: true, panel_certificate: "" });
    expect(
      await screen.findByText("Served by the panel; its certificate is being ordered"),
    ).toBeTruthy();
  });

  it("claims no plain HTTP without --https-addr, which PocketBase's own HTTPS may serve", async () => {
    about({ https: false });
    expect(
      await screen.findByText("Not managed by the panel (--https-addr is not set)"),
    ).toBeTruthy();
    expect(screen.queryByText(/plain HTTP/)).toBeNull();
  });
});

describe("the integrity checks of backups in Settings (#31)", () => {
  const settings = (fields: Record<string, unknown> = {}) => ({
    id: "s1",
    collectionId: "settings",
    collectionName: "settings",
    backup_check_schedule: "0 5 * * 0",
    backup_check_groups: 20,
    ...fields,
  });

  function loadSettings(record: Record<string, unknown>) {
    const getFirstListItem = vi.fn().mockResolvedValue(record);
    vi.spyOn(pb, "collection").mockReturnValue({ getFirstListItem } as never);
  }

  function card(record = settings()) {
    loadSettings(record);
    return render(
      <ToastProvider>
        <BackupChecksCard />
      </ToastProvider>,
    );
  }

  it("shows the schedule in local time and the groups, and saves them", async () => {
    card();
    const schedule = await screen.findByLabelText("Schedule (cron, UTC)");
    expect(schedule).toHaveValue("0 5 * * 0");
    expect(
      screen.getByText(
        "Empty turns the checks off. At most once a day. In your time: Sundays 14:00.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Groups")).toHaveValue(20);
    expect(
      screen.getByText(/^Each check reads the next of this many groups: 20 reads about 5 %/),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("Without checks, damage in a repository shows only when you restore."),
    ).not.toBeInTheDocument();

    const saved = vi.spyOn(pb, "send").mockResolvedValue({
      schedule: "0 3 * * 6",
      groups: 10,
      next_check_at: "2026-11-21 03:00:00.000Z",
    });
    fireEvent.change(schedule, { target: { value: " 0 3 * * 6 " } });
    fireEvent.change(screen.getByLabelText("Groups"), { target: { value: "10" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(saved).toHaveBeenCalledWith("/api/pc/settings/backup-checks", {
      method: "PUT",
      body: { schedule: "0 3 * * 6", groups: 10 },
    });
    expect(await screen.findByText("The integrity checks were saved.")).toBeInTheDocument();
    expect(schedule).toHaveValue("0 3 * * 6");
  });

  it("warns that checks are off without a schedule, and turns them off", async () => {
    card(settings({ backup_check_schedule: "" }));
    expect(
      await screen.findByText(
        "Without checks, damage in a repository shows only when you restore.",
      ),
    ).toBeInTheDocument();
    const saved = vi
      .spyOn(pb, "send")
      .mockResolvedValue({ schedule: "", groups: 20, next_check_at: "" });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(saved).toHaveBeenCalledWith("/api/pc/settings/backup-checks", {
      method: "PUT",
      body: { schedule: "", groups: 20 },
    });
    await screen.findByText("The integrity checks were saved.");
  });

  it("holds back groups restic does not take", async () => {
    card();
    const groups = await screen.findByLabelText("Groups");
    const send = vi.spyOn(pb, "send");
    for (const value of ["257", "-1", "2.5", ""]) {
      fireEvent.change(groups, { target: { value } });
      expect(screen.getByText("A whole number from 0 to 256.")).toBeInTheDocument();
      expect(groups).toHaveAttribute("aria-invalid", "true");
      expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
    }
    fireEvent.change(groups, { target: { value: "0" } });
    expect(screen.queryByText("A whole number from 0 to 256.")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeEnabled();
    expect(send).not.toHaveBeenCalled();
  });

  it("shows why the panel refused a schedule", async () => {
    card();
    await screen.findByLabelText("Schedule (cron, UTC)");
    vi.spyOn(pb, "send").mockRejectedValue(
      new ClientResponseError({
        status: 400,
        response: { message: "A check runs at most once a day." },
      }),
    );
    fireEvent.change(screen.getByLabelText("Schedule (cron, UTC)"), {
      target: { value: "0 * * * *" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("A check runs at most once a day.")).toBeInTheDocument();
  });

  it("is an admin's", async () => {
    const signInAs = (role: User["role"]) => {
      const payload = btoa(
        JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600, type: "auth" }),
      );
      pb.authStore.save(`header.${payload}.signature`, {
        id: "u1",
        collectionId: "users",
        collectionName: "users",
        email: `${role}@example.com`,
        role,
      } as unknown as User);
    };
    const page = () =>
      render(
        <MemoryRouter>
          <AuthProvider>
            <ToastProvider>
              <SettingsPage />
            </ToastProvider>
          </AuthProvider>
        </MemoryRouter>,
      );
    // the other cards wait for answers that never come
    vi.spyOn(pb, "send").mockReturnValue(new Promise(() => {}));
    loadSettings(settings());

    signInAs("operator");
    const operator = page();
    await waitFor(() => expect(screen.getByText("Language")).toBeInTheDocument());
    expect(screen.queryByText("Integrity checks of backups")).not.toBeInTheDocument();
    operator.unmount();

    signInAs("admin");
    page();
    expect(await screen.findByText("Integrity checks of backups")).toBeInTheDocument();
  });
});
