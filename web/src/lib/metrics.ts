import { useEffect, useState } from "react";

import { pb } from "./pb";

export type MetricRange = "1h" | "24h" | "7d";

export const metricRanges: MetricRange[] = ["1h", "24h", "7d"];

export type MetricPoints = {
  range: MetricRange;
  resolution_s: number;
  from: number;
  to: number;
  t: number[];
  cpu: number[];
  mem: number[];
  mem_total: number[];
  net_in: number[];
  net_out: number[];
  disk?: number[];
  disk_total?: number[];
  load1?: number[];
};

export type MountUsage = {
  mount: string;
  device: string;
  fs_type: string;
  used: number;
  total: number;
  at: number;
};

export type HostMetrics = { points: MetricPoints; mounts: MountUsage[] };

export type ContainerValue = {
  cpu: number;
  mem: number;
  mem_limit: number;
  net_in: number;
  net_out: number;
  at: number;
};

/** How often a chart asks again: about as often as it can have changed. */
const refreshMs: Record<MetricRange, number> = { "1h": 10000, "24h": 60000, "7d": 300000 };

const hostMetrics = (id: string, range: MetricRange) =>
  pb.send<HostMetrics>(`/api/pc/metrics/hosts/${id}?range=${range}`, {});

const containerMetrics = (id: string, docker: string, range: MetricRange) =>
  pb.send<MetricPoints>(`/api/pc/metrics/hosts/${id}/containers/${docker}?range=${range}`, {});

const containerValues = (id: string) =>
  pb.send<Record<string, ContainerValue>>(`/api/pc/metrics/hosts/${id}/containers`, {});

function usePolling<T>(load: (() => Promise<T>) | null, key: string, intervalMs: number) {
  const [state, setState] = useState<{ key: string; data: T | null; error: unknown }>({
    key,
    data: null,
    error: null,
  });

  useEffect(() => {
    if (!load) {
      return;
    }

    let cancelled = false;

    const run = () => {
      load()
        .then((data) => {
          if (!cancelled) {
            setState({ key, data, error: null });
          }
        })
        .catch((error: unknown) => {
          if (!cancelled) {
            setState((current) => ({ key, data: current.data, error }));
          }
        });
    };

    run();
    const timer = setInterval(run, intervalMs);

    return () => {
      cancelled = true;
      clearInterval(timer);
    };
    // `key` stands for what `load` does
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, intervalMs]);

  return {
    data: state.data,
    error: state.error,
    /** A new range holds the old chart, dimmed, until its own data has arrived. */
    stale: state.key !== key,
  };
}

/** A host's history for a range, kept fresh. */
export function useHostMetrics(id: string, range: MetricRange) {
  return usePolling(() => hostMetrics(id, range), `${id}/${range}`, refreshMs[range]);
}

/** A container's history for a range, kept fresh. */
export function useContainerMetrics(id: string, docker: string, range: MetricRange) {
  return usePolling(
    () => containerMetrics(id, docker, range),
    `${id}/${docker}/${range}`,
    refreshMs[range],
  );
}

/** The current CPU and memory of a host's containers, by docker id. */
export function useContainerValues(id: string) {
  return usePolling(() => containerValues(id), id, refreshMs["1h"]);
}
