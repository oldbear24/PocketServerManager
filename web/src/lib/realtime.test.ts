import { act, renderHook, waitFor } from "@testing-library/react";
import type { RecordModel, RecordSubscription } from "pocketbase";
import { afterEach, describe, expect, it, vi } from "vitest";

import { pb } from "./pb";
import { useLiveRecords } from "./realtime";

type Run = RecordModel & { status: string; updated: string };

afterEach(() => {
  vi.restoreAllMocks();
});

function record(id: string, status: string, updated: string): Run {
  return { id, collectionId: "runs", collectionName: "backup_runs", status, updated };
}

/** A collection whose list arrives when the test says, and whose realtime
 * events the test sends. */
function fakeCollection() {
  let send: (event: RecordSubscription<Run>) => void = () => {};
  let arrive: (list: Run[]) => void = () => {};
  const stop = vi.fn(() => Promise.resolve());
  const subscribe = vi.fn((_topic: string, callback: (event: RecordSubscription<Run>) => void) => {
    send = callback;
    return Promise.resolve(stop);
  });
  const getFullList = vi.fn(
    () =>
      new Promise<Run[]>((resolve) => {
        arrive = resolve;
      }),
  );
  vi.spyOn(pb, "collection").mockReturnValue({ subscribe, getFullList } as never);
  return {
    subscribe,
    getFullList,
    stop,
    send: (action: string, item: Run) => act(() => send({ action, record: item })),
    arrive: (list: Run[]) => act(() => arrive(list)),
  };
}

const sorted = (items: Run[]) =>
  [...items].sort((a, b) => a.id.localeCompare(b.id)).map((item) => `${item.id}:${item.status}`);

describe("live records", () => {
  it("asks for the same records in the list and in realtime", () => {
    const fake = fakeCollection();
    renderHook(() =>
      useLiveRecords<Run>("backup_runs", { filter: "created >= 'x'", fields: "id,updated" }),
    );
    expect(fake.getFullList).toHaveBeenCalledWith({
      filter: "created >= 'x'",
      fields: "id,updated",
      batch: 1000,
    });
    expect(fake.subscribe).toHaveBeenCalledWith("*", expect.any(Function), {
      filter: "created >= 'x'",
      fields: "id,updated",
    });
  });

  it("applies what arrived before the list, unless the list is newer", async () => {
    const fake = fakeCollection();
    const { result } = renderHook(() => useLiveRecords<Run>("backup_runs"));
    expect(result.current.loading).toBe(true);

    fake.send("create", record("r3", "queued", "2026-11-09 12:00:03.000Z"));
    // older than the copy the list brings
    fake.send("update", record("r1", "running", "2026-11-09 12:00:01.000Z"));
    fake.send("delete", record("r2", "failed", "2026-11-09 12:00:00.000Z"));
    expect(result.current.items).toEqual([]);

    fake.arrive([
      record("r1", "succeeded", "2026-11-09 12:00:02.000Z"),
      record("r2", "failed", "2026-11-09 12:00:00.000Z"),
    ]);
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(sorted(result.current.items)).toEqual(["r1:succeeded", "r3:queued"]);
  });

  it("changes the list in place once it is there", async () => {
    const fake = fakeCollection();
    const { result, unmount } = renderHook(() => useLiveRecords<Run>("backup_runs"));
    fake.arrive([record("r1", "queued", "2026-11-09 12:00:00.000Z")]);
    await waitFor(() => expect(result.current.items).toHaveLength(1));

    fake.send("update", record("r1", "running", "2026-11-09 12:00:01.000Z"));
    fake.send("create", record("r2", "queued", "2026-11-09 12:00:02.000Z"));
    expect(sorted(result.current.items)).toEqual(["r1:running", "r2:queued"]);
    fake.send("delete", record("r1", "running", "2026-11-09 12:00:01.000Z"));
    expect(sorted(result.current.items)).toEqual(["r2:queued"]);
    expect(fake.getFullList).toHaveBeenCalledTimes(1);

    unmount();
    await waitFor(() => expect(fake.stop).toHaveBeenCalled());
  });
});
