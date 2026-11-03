import type { RecordModel } from "pocketbase";
import { useCallback, useEffect, useRef, useState } from "react";

import { pb } from "./pb";

const reloadDelay = 250;

export type ListOptions = {
  filter?: string;
  sort?: string;
  perPage?: number;
  expand?: string;
  /** Set to false to wait, for example until a selection has been made. */
  enabled?: boolean;
};

export type ListState<T> = {
  items: T[];
  total: number;
  loading: boolean;
  error: unknown;
  reload: () => void;
};

/**
 * Loads a list and keeps it live over PocketBase realtime. An update of a known
 * record is applied in place; anything that may change the list itself reloads it.
 */
export function useLiveList<T extends RecordModel>(
  collection: string,
  options: ListOptions = {},
): ListState<T> {
  const { filter, sort, perPage = 200, expand, enabled = true } = options;

  const [items, setItems] = useState<T[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(enabled);
  const [error, setError] = useState<unknown>(null);

  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const generation = useRef(0);

  const load = useCallback(async () => {
    const current = ++generation.current;
    try {
      const list = await pb.collection(collection).getList<T>(1, perPage, { filter, sort, expand });
      if (current === generation.current) {
        setItems(list.items);
        setTotal(list.totalItems);
        setError(null);
      }
    } catch (err) {
      if (current === generation.current) {
        setError(err);
      }
    } finally {
      if (current === generation.current) {
        setLoading(false);
      }
    }
  }, [collection, filter, sort, perPage, expand]);

  useEffect(() => {
    if (!enabled) {
      return;
    }

    void load();

    const schedule = () => {
      if (timer.current) {
        clearTimeout(timer.current);
      }
      timer.current = setTimeout(() => void load(), reloadDelay);
    };

    let unsubscribe: (() => Promise<void>) | undefined;
    let cancelled = false;
    const pendingLoads = generation;

    pb.collection(collection)
      .subscribe<T>("*", ({ action, record }) => {
        if (action !== "update" || filter || expand) {
          schedule();
          return;
        }
        setItems((current) => {
          if (!current.some((item) => item.id === record.id)) {
            schedule();
            return current;
          }
          return current.map((item) => (item.id === record.id ? record : item));
        });
      })
      .then((stop) => {
        if (cancelled) {
          void stop();
        } else {
          unsubscribe = stop;
        }
      })
      .catch((err: unknown) => setError(err));

    return () => {
      cancelled = true;
      pendingLoads.current++; // what is still loading belongs to an older filter
      if (timer.current) {
        clearTimeout(timer.current);
      }
      void unsubscribe?.();
    };
  }, [collection, enabled, expand, filter, load]);

  return { items, total, loading, error, reload: load };
}

/** Loads one record and keeps it live. */
export function useLiveRecord<T extends RecordModel>(
  collection: string,
  id: string | undefined,
): { record: T | null; loading: boolean; error: unknown } {
  const [record, setRecord] = useState<T | null>(null);
  const [loading, setLoading] = useState(Boolean(id));
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    if (!id) {
      return;
    }

    let cancelled = false;
    let unsubscribe: (() => Promise<void>) | undefined;

    pb.collection(collection)
      .getOne<T>(id)
      .then((loaded) => {
        if (!cancelled) {
          setRecord(loaded);
          setError(null);
        }
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          setError(err);
        }
      })
      .finally(() => {
        if (!cancelled) {
          setLoading(false);
        }
      });

    pb.collection(collection)
      .subscribe<T>(id, ({ action, record: updated }) => {
        setRecord(action === "delete" ? null : updated);
      })
      .then((stop) => {
        if (cancelled) {
          void stop();
        } else {
          unsubscribe = stop;
        }
      })
      .catch((err: unknown) => setError(err));

    return () => {
      cancelled = true;
      void unsubscribe?.();
    };
  }, [collection, id]);

  return { record, loading, error };
}

/** Re-renders on an interval, for values such as "4 s ago". */
export function useNow(intervalMs = 5000): number {
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(timer);
  }, [intervalMs]);

  return now;
}
