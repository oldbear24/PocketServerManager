import { act, renderHook, waitFor } from "@testing-library/react";
import type { ListResult, RecordModel, RecordSubscription } from "pocketbase";
import { afterEach, describe, expect, it, vi } from "vitest";

import { pb } from "./pb";
import { useLiveList, useLiveRecords } from "./realtime";

type Run = RecordModel & { status: string; updated: string };

afterEach(() => {
  vi.useRealTimers();
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

  it("replays onto a read's list only what arrived after the read began", async () => {
    const fake = fakeCollection();
    const { result } = renderHook(() => useLiveRecords<Run>("backup_runs"));
    await waitFor(() => expect(fake.getFullList).toHaveBeenCalled());
    // r9 comes while the first read is under way
    fake.send("create", record("r9", "queued", "2026-11-09 12:00:01.000Z"));

    // the connection drops, r9 is deleted unseen while it is down, and the
    // reconnect reads the list again before the first read has ended
    fake.connect();
    expect(fake.getFullList).toHaveBeenCalledTimes(2);
    await fake.arrive([record("r1", "queued", "2026-11-09 12:00:00.000Z")], 0);
    await fake.arrive([record("r1", "queued", "2026-11-09 12:00:00.000Z")], 1);
    expect(sorted(result.current.items)).toEqual(["r1:queued"]);
    expect(result.current.loading).toBe(false);

    // live again
    fake.send("update", record("r1", "running", "2026-11-09 12:00:02.000Z"));
    expect(sorted(result.current.items)).toEqual(["r1:running"]);
  });

  it("shows and keeps what arrived for a read that a newer one replaced", async () => {
    const fake = fakeCollection();
    const { result } = renderHook(() => useLiveRecords<Run>("backup_runs"));
    await waitFor(() => expect(fake.getFullList).toHaveBeenCalled());
    await fake.arrive([
      record("r1", "queued", "2026-11-09 12:00:00.000Z"),
      record("r2", "queued", "2026-11-09 12:00:00.000Z"),
    ]);

    fake.connect();
    fake.send("update", record("r1", "running", "2026-11-09 12:00:01.000Z"));
    fake.send("delete", record("r2", "queued", "2026-11-09 12:00:00.000Z"));
    fake.connect();
    expect(fake.getFullList).toHaveBeenCalledTimes(3);
    expect(sorted(result.current.items)).toEqual(["r1:running"]);

    // the replaced read ends late and counts for nothing; the newer one
    // fails, and what is shown loses nothing that arrived meanwhile
    await fake.arrive(
      [
        record("r1", "queued", "2026-11-09 12:00:00.000Z"),
        record("r2", "queued", "2026-11-09 12:00:00.000Z"),
      ],
      1,
    );
    fake.send("create", record("r3", "queued", "2026-11-09 12:00:02.000Z"));
    const err = new Error("Something went wrong.");
    await fake.readFails(err);
    expect(result.current.error).toBe(err);
    expect(sorted(result.current.items)).toEqual(["r1:running", "r3:queued"]);
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

/** A collection for a list: its pages arrive and its subscription is in
 * place when the test says; it sends the realtime events and the connects. */
function fakeList({ held = false } = {}) {
  let send: (event: RecordSubscription<Run>) => void = () => {};
  let connect: () => void = () => {};
  let place: () => void = () => {};
  const pages: { resolve: (page: ListResult<Run>) => void; reject: (err: unknown) => void }[] = [];
  const stop = vi.fn(() => Promise.resolve());
  const stopConnect = vi.fn(() => Promise.resolve());
  const subscribe = vi.fn((_topic: string, callback: (event: RecordSubscription<Run>) => void) => {
    send = callback;
    if (!held) {
      return Promise.resolve(stop);
    }
    return new Promise<typeof stop>((resolve) => {
      place = () => resolve(stop);
    });
  });
  const getList = vi.fn(
    () =>
      new Promise<ListResult<Run>>((resolve, reject) => {
        pages.push({ resolve, reject });
      }),
  );
  vi.spyOn(pb, "collection").mockReturnValue({ subscribe, getList } as never);
  const realtime = vi.spyOn(pb.realtime, "subscribe").mockImplementation((topic, callback) => {
    if (topic === "PB_CONNECT") {
      connect = () => callback({ clientId: "c2" });
    }
    return Promise.resolve(stopConnect);
  });
  return {
    subscribe,
    getList,
    realtime,
    stop,
    stopConnect,
    send: (action: string, item: Run) => act(() => send({ action, record: item })),
    connect: () => act(() => connect()),
    /** the subscription is in place */
    placed: () => act(async () => place()),
    /** the page of a read, by default the newest; without a count PocketBase sends -1 */
    page: (items: Run[], totalItems = items.length, read = pages.length - 1) =>
      act(async () =>
        pages[read].resolve({ page: 1, perPage: 200, totalItems, totalPages: 1, items }),
      ),
    /** the newest read fails */
    pageFails: (err: unknown) => act(async () => pages.at(-1)!.reject(err)),
  };
}

const commandFilter = "parent = '' && type !~ 'logs.%'";

const shown = (items: Run[]) => items.map((item) => `${item.id}:${item.status}`);

describe("a live list", () => {
  it("loads a page with its options, and the total PocketBase counted", async () => {
    const fake = fakeList();
    const { result } = renderHook(() =>
      useLiveList<Run>("alerts", { filter: "status != 'resolved'", perPage: 50, fields: "id" }),
    );
    expect(result.current.loading).toBe(true);
    expect(fake.getList).toHaveBeenCalledWith(1, 50, {
      filter: "status != 'resolved'",
      sort: undefined,
      expand: undefined,
      fields: "id",
      skipTotal: undefined,
    });
    await fake.page([record("a1", "open", "2026-11-09 12:00:00.000Z")], 7);
    expect(result.current.loading).toBe(false);
    expect(result.current.total).toBe(7);
  });

  it("asks for no count with skipTotal, and counts what it loaded", async () => {
    const fake = fakeList();
    const { result } = renderHook(() =>
      useLiveList<Run>("commands", { sort: "-created", perPage: 5, skipTotal: true }),
    );
    expect(fake.getList).toHaveBeenCalledWith(
      1,
      5,
      expect.objectContaining({ sort: "-created", skipTotal: true }),
    );
    await fake.page(
      [
        record("c1", "queued", "2026-11-09 12:00:00.000Z"),
        record("c2", "done", "2026-11-09 12:00:00.000Z"),
      ],
      -1,
    );
    expect(result.current.total).toBe(2);
  });

  it("keeps what it showed when a reload fails", async () => {
    const fake = fakeList();
    const { result } = renderHook(() => useLiveList<Run>("alerts"));
    await fake.page([record("a1", "open", "2026-11-09 12:00:00.000Z")]);

    act(() => void result.current.reload());
    expect(fake.getList).toHaveBeenCalledTimes(2);
    const err = new Error("Something went wrong.");
    await fake.pageFails(err);
    expect(result.current.error).toBe(err);
    expect(shown(result.current.items)).toEqual(["a1:open"]);
    expect(result.current.total).toBe(1);
    expect(result.current.loading).toBe(false);
  });

  it("loads the list again after a reconnect, not at the connect of its subscription", async () => {
    const fake = fakeList({ held: true });
    const { result } = renderHook(() => useLiveList<Run>("hosts", { sort: "name" }));
    expect(fake.realtime).toHaveBeenCalledWith("PB_CONNECT", expect.any(Function));
    await fake.page([record("h1", "online", "2026-11-09 12:00:00.000Z")]);
    // the connect that makes the subscription is no reconnect
    fake.connect();
    await fake.placed();
    expect(fake.getList).toHaveBeenCalledTimes(1);

    // while the connection was down h1 went offline and h2 came, unseen
    fake.connect();
    expect(fake.getList).toHaveBeenCalledTimes(2);
    await fake.page([
      record("h1", "offline", "2026-11-09 12:00:05.000Z"),
      record("h2", "online", "2026-11-09 12:00:05.000Z"),
    ]);
    expect(shown(result.current.items)).toEqual(["h1:offline", "h2:online"]);
    expect(result.current.total).toBe(2);

    fake.connect();
    expect(fake.getList).toHaveBeenCalledTimes(3);
  });

  it("reloads after a reconnect at once, in place of a reload it was waiting for", async () => {
    vi.useFakeTimers();
    const fake = fakeList();
    renderHook(() => useLiveList<Run>("commands", { filter: commandFilter, inPlace: true }));
    await fake.page([record("c1", "queued", "2026-11-09 12:00:00.000Z")]);
    fake.send("create", record("c2", "queued", "2026-11-09 12:00:01.000Z"));
    fake.connect();
    expect(fake.getList).toHaveBeenCalledTimes(2);
    act(() => vi.advanceTimersByTime(1000));
    expect(fake.getList).toHaveBeenCalledTimes(2);
  });

  it("stops listening for connects when it goes", async () => {
    const fake = fakeList();
    const { unmount } = renderHook(() => useLiveList<Run>("hosts"));
    await fake.page([]);
    unmount();
    await waitFor(() => expect(fake.stopConnect).toHaveBeenCalled());
    expect(fake.stop).toHaveBeenCalled();
    fake.connect();
    expect(fake.getList).toHaveBeenCalledTimes(1);
  });

  it("does not wait for a list that is not enabled, nor read it at a reconnect", async () => {
    const fake = fakeList();
    renderHook(() => useLiveList<Run>("dns_providers", { enabled: false }));
    expect(fake.subscribe).not.toHaveBeenCalled();
    expect(fake.realtime).not.toHaveBeenCalled();
    expect(fake.getList).not.toHaveBeenCalled();
  });

  it("is loading from the render that enables it until its page is there", async () => {
    const fake = fakeList();
    const { result, rerender } = renderHook(
      ({ enabled }) => useLiveList<Run>("dns_providers", { enabled }),
      { initialProps: { enabled: false } },
    );
    expect(result.current.loading).toBe(false);

    rerender({ enabled: true });
    // never an empty list that has loaded
    expect(result.current.loading).toBe(true);
    expect(result.current.items).toEqual([]);
    expect(fake.getList).toHaveBeenCalledTimes(1);
    await fake.page([record("p1", "ok", "2026-11-09 12:00:00.000Z")]);
    expect(result.current.loading).toBe(false);
    expect(shown(result.current.items)).toEqual(["p1:ok"]);
  });

  it("is not loading once it waits again, and loads anew when enabled", async () => {
    const fake = fakeList();
    const { result, rerender } = renderHook(
      ({ enabled }) => useLiveList<Run>("dns_providers", { enabled }),
      { initialProps: { enabled: true } },
    );
    expect(result.current.loading).toBe(true);
    rerender({ enabled: false });
    expect(result.current.loading).toBe(false);
    // the page of the read it no longer waits for changes nothing
    await fake.page([record("p1", "ok", "2026-11-09 12:00:00.000Z")], 1, 0);
    expect(result.current.items).toEqual([]);

    rerender({ enabled: true });
    expect(result.current.loading).toBe(true);
    await fake.page([record("p2", "ok", "2026-11-09 12:00:00.000Z")]);
    expect(result.current.loading).toBe(false);
    expect(shown(result.current.items)).toEqual(["p2:ok"]);
  });

  describe("without inPlace", () => {
    it("subscribes to every record of the collection, with the fields only", async () => {
      const fake = fakeList();
      renderHook(() => useLiveList<Run>("commands", { filter: commandFilter, fields: "id" }));
      expect(fake.subscribe).toHaveBeenCalledWith("*", expect.any(Function), { fields: "id" });

      const other = fakeList();
      renderHook(() => useLiveList<Run>("hosts"));
      expect(other.subscribe).toHaveBeenCalledWith("*", expect.any(Function), undefined);
    });

    it("loads a filtered list again on any update", async () => {
      vi.useFakeTimers();
      const fake = fakeList();
      renderHook(() => useLiveList<Run>("commands", { filter: commandFilter }));
      await fake.page([record("c1", "queued", "2026-11-09 12:00:00.000Z")]);
      fake.send("update", record("c1", "running", "2026-11-09 12:00:01.000Z"));
      act(() => vi.advanceTimersByTime(250));
      expect(fake.getList).toHaveBeenCalledTimes(2);
    });

    it("replaces a record of a list without a filter, and reloads for one it lacks", async () => {
      vi.useFakeTimers();
      const fake = fakeList();
      const { result } = renderHook(() => useLiveList<Run>("hosts", { sort: "name" }));
      await fake.page([record("h1", "online", "2026-11-09 12:00:00.000Z")]);
      fake.send("update", record("h1", "offline", "2026-11-09 12:00:01.000Z"));
      expect(shown(result.current.items)).toEqual(["h1:offline"]);
      act(() => vi.advanceTimersByTime(1000));
      expect(fake.getList).toHaveBeenCalledTimes(1);

      fake.send("update", record("h2", "online", "2026-11-09 12:00:01.000Z"));
      act(() => vi.advanceTimersByTime(250));
      expect(fake.getList).toHaveBeenCalledTimes(2);
    });
  });

  describe("in place", () => {
    it("subscribes with the list's filter and fields", () => {
      const fake = fakeList();
      renderHook(() =>
        useLiveList<Run>("commands", {
          filter: commandFilter,
          sort: "-created",
          perPage: 5,
          fields: "id,status,updated",
          inPlace: true,
        }),
      );
      expect(fake.subscribe).toHaveBeenCalledWith("*", expect.any(Function), {
        filter: commandFilter,
        fields: "id,status,updated",
      });
    });

    it("subscribes to every record without a filter", () => {
      const fake = fakeList();
      renderHook(() => useLiveList<Run>("containers", { perPage: 1, inPlace: true }));
      expect(fake.subscribe).toHaveBeenCalledWith("*", expect.any(Function), undefined);
    });

    it("replaces a record shown in place, without a read", async () => {
      vi.useFakeTimers();
      const fake = fakeList();
      const { result } = renderHook(() =>
        useLiveList<Run>("commands", { filter: commandFilter, inPlace: true }),
      );
      await fake.page([
        record("c1", "queued", "2026-11-09 12:00:00.000Z"),
        record("c2", "queued", "2026-11-09 12:00:00.000Z"),
      ]);
      const other = result.current.items[1];

      fake.send("update", record("c1", "running", "2026-11-09 12:00:01.000Z"));
      expect(shown(result.current.items)).toEqual(["c1:running", "c2:queued"]);
      // the other record stays the same, so its row need not render again
      expect(result.current.items[1]).toBe(other);
      act(() => vi.advanceTimersByTime(1000));
      expect(fake.getList).toHaveBeenCalledTimes(1);
    });

    it("ignores an update of a record it does not show", async () => {
      vi.useFakeTimers();
      const fake = fakeList();
      const { result } = renderHook(() =>
        useLiveList<Run>("commands", { filter: commandFilter, perPage: 1, inPlace: true }),
      );
      await fake.page([record("c2", "queued", "2026-11-09 12:00:00.000Z")], 2);
      const before = result.current.items;

      fake.send("update", record("c1", "done", "2026-11-09 12:00:01.000Z"));
      act(() => vi.advanceTimersByTime(1000));
      expect(fake.getList).toHaveBeenCalledTimes(1);
      expect(result.current.items).toBe(before);
      expect(result.current.total).toBe(2);
    });

    it("loads the list again once, 250 ms after a create or a delete", async () => {
      vi.useFakeTimers();
      const fake = fakeList();
      const { result } = renderHook(() =>
        useLiveList<Run>("commands", { filter: commandFilter, inPlace: true }),
      );
      await fake.page([record("c1", "queued", "2026-11-09 12:00:00.000Z")]);

      fake.send("create", record("c3", "queued", "2026-11-09 12:00:02.000Z"));
      fake.send("delete", record("c1", "queued", "2026-11-09 12:00:00.000Z"));
      act(() => vi.advanceTimersByTime(249));
      expect(fake.getList).toHaveBeenCalledTimes(1);
      act(() => vi.advanceTimersByTime(1));
      expect(fake.getList).toHaveBeenCalledTimes(2);
      await fake.page([record("c3", "queued", "2026-11-09 12:00:02.000Z")]);
      expect(shown(result.current.items)).toEqual(["c3:queued"]);
      act(() => vi.advanceTimersByTime(1000));
      expect(fake.getList).toHaveBeenCalledTimes(2);
    });
  });
});
