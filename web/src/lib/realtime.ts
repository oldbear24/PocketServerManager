import type { RecordModel } from "pocketbase";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { pb } from "./pb";

const reloadDelay = 250;

export type ListOptions = {
  filter?: string;
  sort?: string;
  perPage?: number;
  expand?: string;
  /** Only these fields, as for a list that needs not a record's long ones. */
  fields?: string;
  /** Set to false to wait, for example until a selection has been made. */
  enabled?: boolean;
  /** Counts no total. PocketBase otherwise runs a COUNT of every record the
   * filter matches with each read, which a long list, as the commands that
   * are never pruned, pays for a page of five. The total is then the number
   * of records loaded. */
  skipTotal?: boolean;
  /** Keeps the list in place: realtime sends only the records the filter
   * matches, an update of a record shown replaces it, an update of a record
   * not shown is ignored, and only a create or a delete loads the list
   * again. Without it any event of a filtered list reloads it, as a
   * command's progress would the queue. Only for a filter and a sort on
   * fields that never change, as a command's type and creation: an update
   * cannot then move a record into the list, out of it or within it. */
  inPlace?: boolean;
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
 * record is applied in place; anything that may change the list itself reloads it,
 * and so does each reconnect of the realtime connection.
 */
export function useLiveList<T extends RecordModel>(
  collection: string,
  options: ListOptions = {},
): ListState<T> {
  const {
    filter,
    sort,
    perPage = 200,
    expand,
    fields,
    enabled = true,
    skipTotal,
    inPlace = false,
  } = options;

  const [items, setItems] = useState<T[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(enabled);
  const [error, setError] = useState<unknown>(null);
  // A list that waited loads once it is enabled: from that render on it is
  // loading, not empty. A list that waits loads nothing.
  const [wasEnabled, setWasEnabled] = useState(enabled);
  if (enabled !== wasEnabled) {
    setWasEnabled(enabled);
    setLoading(enabled);
  }

  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const generation = useRef(0);

  const load = useCallback(async () => {
    const current = ++generation.current;
    try {
      const list = await pb
        .collection(collection)
        .getList<T>(1, perPage, { filter, sort, expand, fields, skipTotal });
      if (current === generation.current) {
        setItems(list.items);
        // without a count PocketBase sends -1
        setTotal(skipTotal ? list.items.length : list.totalItems);
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
  }, [collection, filter, sort, perPage, expand, fields, skipTotal]);

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
    let stopConnect: (() => Promise<void>) | undefined;
    let cancelled = false;
    // once the subscription is in place, a connect is a reconnect
    let subscribed = false;
    const pendingLoads = generation;

    pb.collection(collection)
      .subscribe<T>(
        "*",
        ({ action, record }) => {
          if (inPlace) {
            if (action !== "update") {
              schedule();
              return;
            }
            // a record not shown stays out: its update cannot move it in
            setItems((current) =>
              current.some((item) => item.id === record.id)
                ? current.map((item) => (item.id === record.id ? record : item))
                : current,
            );
            return;
          }
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
        },
        // a list in place hears only of its own records, each as it shows them
        subscribeOptions(inPlace ? { filter, fields, expand } : { fields }),
      )
      .then((stop) => {
        if (cancelled) {
          void stop();
        } else {
          unsubscribe = stop;
          subscribed = true;
        }
      })
      .catch((err: unknown) => setError(err));

    // PocketBase's client reconnects by itself, with a new client id: the
    // events between are lost, so the list is read again, as useLiveRecords
    // does. The connect that makes the subscription comes before it is in
    // place and is no reconnect.
    pb.realtime
      .subscribe("PB_CONNECT", () => {
        if (subscribed && !cancelled) {
          if (timer.current) {
            clearTimeout(timer.current);
          }
          void load();
        }
      })
      .then((stop) => {
        if (cancelled) {
          void stop();
        } else {
          stopConnect = stop;
        }
      })
      .catch(() => {
        // the list's own subscription says why
      });

    return () => {
      cancelled = true;
      pendingLoads.current++; // what is still loading belongs to an older filter
      if (timer.current) {
        clearTimeout(timer.current);
      }
      void unsubscribe?.();
      void stopConnect?.();
    };
  }, [collection, enabled, expand, fields, filter, inPlace, load]);

  // the same state while nothing changed, so that a list shared through a
  // context, as the Layout's servers, renders its readers again only then
  return useMemo(
    () => ({ items, total, loading, error, reload: load }),
    [items, total, loading, error, load],
  );
}

/** The options of a subscription without those not set: each set of options
 * makes a topic of its own. */
function subscribeOptions(options: Record<string, string | undefined>) {
  const set = Object.entries(options).filter(([, value]) => value);
  return set.length > 0 ? Object.fromEntries(set) : undefined;
}

type RecordEvent<T> = { action: string; record: T };

/** An event applied to records by id: a deleted record leaves, any other
 * takes its place unless the copy there is newer, as one loaded after it. */
function applyEvent<T extends RecordModel>(records: Map<string, T>, event: RecordEvent<T>) {
  if (event.action === "delete") {
    records.delete(event.record.id);
    return;
  }
  const known = records.get(event.record.id);
  if (!known || !(known.updated > event.record.updated)) {
    records.set(event.record.id, event.record);
  }
}

/**
 * Loads every record a filter matches, page by page, and keeps them live in
 * place: realtime sends only what the same filter matches, so a new record
 * joins, a changed one replaces its copy and a deleted one leaves, without
 * loading the list again. For lists too long to reload on every change, such
 * as two weeks of backup runs. The records come in no order; `fields` must
 * name `updated`.
 *
 * The list is read once the subscription is in place, so that nothing changes
 * unseen between the two, and again after each reconnect of the realtime
 * connection: what changed while it was down never arrives.
 */
export function useLiveRecords<T extends RecordModel>(
  collection: string,
  options: { filter?: string; fields?: string; enabled?: boolean } = {},
): ListState<T> {
  const { filter, fields, enabled = true } = options;

  const [items, setItems] = useState<T[]>([]);
  const [loading, setLoading] = useState(enabled);
  const [error, setError] = useState<unknown>(null);
  const [version, setVersion] = useState(0);

  useEffect(() => {
    if (!enabled) {
      return;
    }

    let cancelled = false;
    let unsubscribe: (() => Promise<void>) | undefined;
    let stopConnect: (() => Promise<void>) | undefined;
    // once the subscription is in place, a connect is a reconnect
    let subscribed = false;
    // the subscription failed: the list is not live, which a read does not change
    let broken = false;
    // what arrives while the list is read is applied to it once it is there
    let early: RecordEvent<T>[] | null = [];
    // only the newest read counts
    let reads = 0;

    const applyShown = (events: RecordEvent<T>[]) =>
      setItems((shown) => {
        const records = new Map(shown.map((item) => [item.id, item]));
        for (const event of events) {
          applyEvent(records, event);
        }
        return [...records.values()];
      });

    const read = () => {
      const current = ++reads;
      // the first read keeps what arrived before it began; each later one
      // has its own buffer, for what arrives once it has begun
      if (current > 1) {
        if (early) {
          // the read before has not ended and no longer counts. What arrived
          // for it is shown now, so that this read loses none of it should it
          // fail, but it is not replayed onto this read's list: that list is
          // newer, and may lack a record whose delete never arrived.
          applyShown(early);
        }
        early = [];
      }
      pb.collection(collection)
        .getFullList<T>({ filter, fields, batch: 1000 })
        .then((list) => {
          if (cancelled || current !== reads) {
            return;
          }
          const records = new Map(list.map((item) => [item.id, item]));
          for (const event of early ?? []) {
            applyEvent(records, event);
          }
          early = null;
          setItems([...records.values()]);
          if (!broken) {
            setError(null);
          }
        })
        .catch((err: unknown) => {
          if (!cancelled && current === reads) {
            // what is shown stays live, though it may miss what the read had
            const missed = early ?? [];
            early = null;
            applyShown(missed);
            setError(err);
          }
        })
        .finally(() => {
          if (!cancelled && current === reads) {
            setLoading(false);
          }
        });
    };

    pb.collection(collection)
      .subscribe<T>(
        "*",
        (event) => {
          if (cancelled) {
            return;
          }
          if (early) {
            early.push(event);
          } else {
            applyShown([event]);
          }
        },
        { filter, fields },
      )
      .then((stop) => {
        if (cancelled) {
          void stop();
          return;
        }
        unsubscribe = stop;
        subscribed = true;
        read();
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          broken = true;
          setError(err);
          // the records still, though they do not change
          read();
        }
      });

    // PocketBase's client reconnects by itself, with a new client id: the
    // events between are lost. The connect that makes the subscription comes
    // before it is in place and is no reconnect.
    pb.realtime
      .subscribe("PB_CONNECT", () => {
        if (subscribed && !cancelled) {
          read();
        }
      })
      .then((stop) => {
        if (cancelled) {
          void stop();
        } else {
          stopConnect = stop;
        }
      })
      .catch(() => {
        // the records' own subscription says why
      });

    return () => {
      cancelled = true;
      void unsubscribe?.();
      void stopConnect?.();
    };
  }, [collection, enabled, fields, filter, version]);

  const reload = useCallback(() => setVersion((current) => current + 1), []);

  return { items, total: items.length, loading, error, reload };
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
