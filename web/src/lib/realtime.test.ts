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

/** A collection whose subscription is in place, and whose lists arrive, when
 * the test says; it sends the realtime events and the connects. */
function fakeCollection({ held = false } = {}) {
  let send: (event: RecordSubscription<Run>) => void = () => {};
  let connect: () => void = () => {};
  let place: () => void = () => {};
  let fail: (err: unknown) => void = () => {};
  const arrivals: { resolve: (list: Run[]) => void; reject: (err: unknown) => void }[] = [];
  const stop = vi.fn(() => Promise.resolve());
  const stopConnect = vi.fn(() => Promise.resolve());
  const subscribe = vi.fn((_topic: string, callback: (event: RecordSubscription<Run>) => void) => {
    send = callback;
    if (!held) {
      return Promise.resolve(stop);
    }
    return new Promise<typeof stop>((resolve, reject) => {
      place = () => resolve(stop);
      fail = reject;
    });
  });
  const getFullList = vi.fn(
    () =>
      new Promise<Run[]>((resolve, reject) => {
        arrivals.push({ resolve, reject });
      }),
  );
  vi.spyOn(pb, "collection").mockReturnValue({ subscribe, getFullList } as never);
  const realtime = vi.spyOn(pb.realtime, "subscribe").mockImplementation((topic, callback) => {
    if (topic === "PB_CONNECT") {
      connect = () => callback({ clientId: "c2" });
    }
    return Promise.resolve(stopConnect);
  });
  return {
    subscribe,
    getFullList,
    realtime,
    stop,
    stopConnect,
    send: (action: string, item: Run) => act(() => send({ action, record: item })),
    connect: () => act(() => connect()),
    /** the subscription is in place */
    placed: () => act(async () => place()),
    failed: (err: unknown) => act(async () => fail(err)),
    /** the list of a read, by default the newest */
    arrive: (list: Run[], read = arrivals.length - 1) =>
      act(async () => arrivals[read].resolve(list)),
    /** the newest read fails */
    readFails: (err: unknown) => act(async () => arrivals.at(-1)!.reject(err)),
  };
}

const sorted = (items: Run[]) =>
  [...items].sort((a, b) => a.id.localeCompare(b.id)).map((item) => `${item.id}:${item.status}`);

describe("live records", () => {
  it("asks for the same records in the list and in realtime", async () => {
    const fake = fakeCollection();
    renderHook(() =>
      useLiveRecords<Run>("backup_runs", { filter: "created >= 'x'", fields: "id,updated" }),
    );
    expect(fake.subscribe).toHaveBeenCalledWith("*", expect.any(Function), {
      filter: "created >= 'x'",
      fields: "id,updated",
    });
    await waitFor(() =>
      expect(fake.getFullList).toHaveBeenCalledWith({
        filter: "created >= 'x'",
        fields: "id,updated",
        batch: 1000,
      }),
    );
  });

  it("reads the list only once its subscription is in place", async () => {
    const fake = fakeCollection({ held: true });
    const { result } = renderHook(() => useLiveRecords<Run>("backup_runs"));
    await act(async () => {});
    // what changed before would arrive as no event
    expect(fake.getFullList).not.toHaveBeenCalled();

    await fake.placed();
    expect(fake.getFullList).toHaveBeenCalledTimes(1);
    fake.send("update", record("r1", "running", "2026-11-09 12:00:02.000Z"));
    await fake.arrive([record("r1", "queued", "2026-11-09 12:00:01.000Z")]);
    expect(result.current.loading).toBe(false);
    expect(sorted(result.current.items)).toEqual(["r1:running"]);
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

    await waitFor(() => expect(fake.getFullList).toHaveBeenCalled());
    await fake.arrive([
      record("r1", "succeeded", "2026-11-09 12:00:02.000Z"),
      record("r2", "failed", "2026-11-09 12:00:00.000Z"),
    ]);
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(sorted(result.current.items)).toEqual(["r1:succeeded", "r3:queued"]);
  });

  it("changes the list in place once it is there", async () => {
    const fake = fakeCollection();
    const { result, unmount } = renderHook(() => useLiveRecords<Run>("backup_runs"));
    await waitFor(() => expect(fake.getFullList).toHaveBeenCalled());
    await fake.arrive([record("r1", "queued", "2026-11-09 12:00:00.000Z")]);
    await waitFor(() => expect(result.current.items).toHaveLength(1));

    fake.send("update", record("r1", "running", "2026-11-09 12:00:01.000Z"));
    fake.send("create", record("r2", "queued", "2026-11-09 12:00:02.000Z"));
    expect(sorted(result.current.items)).toEqual(["r1:running", "r2:queued"]);
    fake.send("delete", record("r1", "running", "2026-11-09 12:00:01.000Z"));
    expect(sorted(result.current.items)).toEqual(["r2:queued"]);
    expect(fake.getFullList).toHaveBeenCalledTimes(1);

    unmount();
    await waitFor(() => expect(fake.stop).toHaveBeenCalled());
    expect(fake.stopConnect).toHaveBeenCalled();
  });

  it("reads the list again after a reconnect, with what arrives meanwhile", async () => {
    const fake = fakeCollection({ held: true });
    const { result } = renderHook(() => useLiveRecords<Run>("backup_runs"));
    expect(fake.realtime).toHaveBeenCalledWith("PB_CONNECT", expect.any(Function));
    // the connect that makes the subscription is no reconnect
    fake.connect();
    await fake.placed();
    await fake.arrive([
      record("r1", "queued", "2026-11-09 12:00:00.000Z"),
      record("r2", "queued", "2026-11-09 12:00:00.000Z"),
    ]);
    expect(fake.getFullList).toHaveBeenCalledTimes(1);

    // while the connection was down r1 succeeded, r2 went and r3 came, unseen
    fake.connect();
    expect(fake.getFullList).toHaveBeenCalledTimes(2);
    expect(sorted(result.current.items)).toEqual(["r1:queued", "r2:queued"]);
    fake.send("create", record("r4", "queued", "2026-11-09 12:00:05.000Z"));
    await fake.arrive([
      record("r1", "succeeded", "2026-11-09 12:00:03.000Z"),
      record("r3", "failed", "2026-11-09 12:00:04.000Z"),
    ]);
    expect(sorted(result.current.items)).toEqual(["r1:succeeded", "r3:failed", "r4:queued"]);
    expect(result.current.loading).toBe(false);

    // live again
    fake.send("update", record("r4", "running", "2026-11-09 12:00:06.000Z"));
    expect(sorted(result.current.items)).toEqual(["r1:succeeded", "r3:failed", "r4:running"]);
  });

  it("keeps the newest read of several reconnects", async () => {
    const fake = fakeCollection();
    const { result } = renderHook(() => useLiveRecords<Run>("backup_runs"));
    await waitFor(() => expect(fake.getFullList).toHaveBeenCalled());
    await fake.arrive([record("r1", "queued", "2026-11-09 12:00:00.000Z")]);

    fake.connect();
    fake.connect();
    expect(fake.getFullList).toHaveBeenCalledTimes(3);
    await fake.arrive([record("r1", "succeeded", "2026-11-09 12:00:02.000Z")], 2);
    // the older read ends last, and counts for nothing
    await fake.arrive([record("r1", "running", "2026-11-09 12:00:01.000Z")], 1);
    expect(sorted(result.current.items)).toEqual(["r1:succeeded"]);
  });

  it("stays live when a read after a reconnect fails", async () => {
    const fake = fakeCollection();
    const { result } = renderHook(() => useLiveRecords<Run>("backup_runs"));
    await waitFor(() => expect(fake.getFullList).toHaveBeenCalled());
    await fake.arrive([record("r1", "queued", "2026-11-09 12:00:00.000Z")]);

    fake.connect();
    fake.send("update", record("r1", "running", "2026-11-09 12:00:01.000Z"));
    const err = new Error("Something went wrong.");
    await fake.readFails(err);
    expect(result.current.error).toBe(err);
    expect(sorted(result.current.items)).toEqual(["r1:running"]);
    fake.send("update", record("r1", "succeeded", "2026-11-09 12:00:02.000Z"));
    expect(sorted(result.current.items)).toEqual(["r1:succeeded"]);
  });

  it("says the subscription failed, and still reads the list", async () => {
    const fake = fakeCollection({ held: true });
    const { result } = renderHook(() => useLiveRecords<Run>("backup_runs"));
    const err = new Error("Failed to establish realtime connection.");
    await fake.failed(err);
    expect(result.current.error).toBe(err);
    await fake.arrive([record("r1", "queued", "2026-11-09 12:00:00.000Z")]);
    expect(sorted(result.current.items)).toEqual(["r1:queued"]);
    expect(result.current.error).toBe(err);
    expect(result.current.loading).toBe(false);
  });
});
