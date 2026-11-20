import { render, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { Layout } from "../components/Layout";
import { ToastProvider } from "../components/toast";
import { AuthProvider } from "../lib/auth";
import * as f from "../lib/overview.fixtures";
import { pb } from "../lib/pb";
import { signInAs } from "./BackupRestore.fixtures";
import { OverviewPage } from "./Overview";

// A cold load of the Overview with its Layout, through the real hooks down
// to the requests the SDK sends, on a panel where every list but the
// servers finds nothing.

/** A request the page sent: its collection, or its route, and its query. */
type Sent = { collection: string; query: URLSearchParams };

const sent: Sent[] = [];

// four servers offline, whose last disconnects the page looks up, and one
// that is not
const servers = [
  f.host("h1", { name: "nas" }),
  ...["h2", "h3", "h4", "h5"].map((id) =>
    f.host(id, { name: id, status: "offline", stream_open: false }),
  ),
];

beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string) => {
      const url = new URL(input);
      const list = url.pathname.match(/^\/api\/collections\/([^/]+)\/records$/);
      sent.push({ collection: list ? list[1] : url.pathname, query: url.searchParams });
      const items = list?.[1] === "hosts" ? servers : [];
      const body = list
        ? {
            page: 1,
            perPage: Number(url.searchParams.get("perPage")),
            totalItems: items.length,
            totalPages: 1,
            items,
          }
        : { gateway_running: true, grpc_address: "panel.lan:9443" };
      return { status: 200, url: input, json: async () => body };
    }),
  );
  // realtime has no connection here: each subscription is in place at once
  vi.spyOn(pb.realtime, "subscribe").mockResolvedValue(async () => {});
  signInAs("viewer");
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  pb.authStore.clear();
  sent.length = 0;
});

async function load() {
  const view = render(
    <MemoryRouter initialEntries={["/"]}>
      <AuthProvider>
        <ToastProvider>
          <Routes>
            <Route element={<Layout />}>
              <Route index element={<OverviewPage />} />
            </Route>
          </Routes>
        </ToastProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
  const root = () => view.container.querySelector("[data-overview-state]");
  await waitFor(() => expect(root()).toHaveAttribute("data-overview-state", "filled"));
  // why the servers went away, which does not hold "filled" up
  await waitFor(() => expect(sent.some((item) => item.collection === "events")).toBe(true));
  return view;
}

describe("a cold load of the Overview", () => {
  // PocketBase holds up (0 to 500 ms at random) the fourth list with a
  // filter of one collection from one address that finds nothing within 3
  // seconds (pocketbase apis/record_crud.go, listTimingRateLimitRule)
  it("reads no collection with a filter more than three times", async () => {
    await load();
    const filtered = new Map<string, number>();
    for (const item of sent) {
      if (item.query.get("filter")) {
        filtered.set(item.collection, (filtered.get(item.collection) ?? 0) + 1);
      }
    }
    for (const [collection, count] of filtered) {
      expect({ collection, count }).toEqual({ collection, count: Math.min(count, 3) });
    }
    // the open alerts once for the sidebar's count and the page, and once
    // for each of the sidebar's two dots, none by the page itself; the last
    // disconnects of the four offline servers once; and each list with a
    // filter, once
    expect(Object.fromEntries(filtered)).toEqual({
      alerts: 3,
      databases: 1,
      commands: 1,
      domains: 1,
      deployments: 1,
      backup_runs: 1,
      events: 1,
    });
  });
});
