import { render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { api } from "../lib/api";
import { AuthProvider } from "../lib/auth";
import { pb, type User } from "../lib/pb";
import { matchesFilter } from "../test-filter";
import type { Alert } from "../lib/types";
import { alertDot, Layout } from "./Layout";

const live = vi.hoisted(() => ({ lists: {} as Record<string, unknown[]> }));
vi.mock("../lib/realtime", () => ({
  useLiveList: (collection: string, options: { filter?: string } = {}) => {
    const items = (live.lists[collection] ?? []).filter(
      (item) => !options.filter || matchesFilter(item as Record<string, unknown>, options.filter),
    );
    return { items, loading: false, error: null, total: items.length, reload: () => {} };
  },
}));

beforeEach(() => {
  // the gateway card waits; it is not what these tests are about
  vi.spyOn(api, "info").mockReturnValue(new Promise(() => {}));
  const payload = btoa(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600, type: "auth" }));
  pb.authStore.save(`header.${payload}.signature`, {
    id: "u1",
    collectionId: "users",
    collectionName: "users",
    email: "viewer@example.com",
    name: "",
    role: "viewer",
  } as unknown as User);
});

afterEach(() => {
  vi.restoreAllMocks();
  pb.authStore.clear();
  live.lists = {};
});

function alert(fields: Partial<Alert>): Alert {
  return {
    id: fields.rule ?? "a1",
    collectionId: "alerts",
    collectionName: "alerts",
    rule: "backup_failed",
    target: "j1:t1",
    host: "h1",
    severity: "warning",
    status: "open",
    first_seen: "",
    last_seen: "",
    message: "",
    ...fields,
  } as Alert;
}

function backups() {
  render(
    <MemoryRouter initialEntries={["/"]}>
      <AuthProvider>
        <Routes>
          <Route path="*" element={<Layout />} />
        </Routes>
      </AuthProvider>
    </MemoryRouter>,
  );
  return screen.getByRole("link", { name: /^Backups/ });
}

describe("the Backups item of the navigation", () => {
  it("has no dot without an open backup alert", () => {
    live.lists = {
      alerts: [
        alert({ rule: "agent_offline", target: "h1", severity: "critical" }),
        alert({ status: "resolved", severity: "critical" }),
      ],
    };
    const link = backups();
    expect(link.querySelector(".nav-dot")).toBeNull();
    expect(link).toHaveAccessibleName("Backups");
  });

  it("is marked warn by a warning, and says how many alerts are open", () => {
    live.lists = {
      alerts: [
        alert({ rule: "backup_failed", severity: "warning" }),
        alert({ rule: "backup_check_unverified", target: "t1", severity: "warning" }),
      ],
    };
    const link = backups();
    expect(link.querySelector(".nav-dot")).toHaveClass("warn");
    expect(within(link).getByText("2 open alerts")).toHaveClass("visually-hidden");
  });

  it("is marked err when any backup alert is critical", () => {
    live.lists = {
      alerts: [
        alert({ rule: "backup_retention", severity: "warning" }),
        alert({ rule: "backup_check_failed", target: "t1", severity: "critical" }),
      ],
    };
    expect(backups().querySelector(".nav-dot")).toHaveClass("err");
  });
});

describe("a section's dot", () => {
  it("is err for a critical alert, warn for any other, none without", () => {
    expect(alertDot([{ severity: "warning" }, { severity: "critical" }], 2)).toBe("err");
    expect(alertDot([{ severity: "warning" }], 1)).toBe("warn");
    // more are open than were loaded
    expect(alertDot([], 60)).toBe("warn");
    expect(alertDot([], 0)).toBeUndefined();
  });
});
