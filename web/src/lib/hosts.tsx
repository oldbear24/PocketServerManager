import type { RecordModel } from "pocketbase";
import { createContext, useContext, type ReactNode } from "react";

import type { ListState } from "./realtime";
import type { Host } from "./types";

/** The fields of a server that the sidebar and the Overview show. The Layout
 * reads the servers with these only, so that a heartbeat, every 10 s for
 * each server, does not carry the whole record, its runtime and its edge
 * state, to every page. */
export const hostSummaryFields =
  "id,collectionId,collectionName,updated,name,roles,status,stream_open,ip4,public_ip4,os,cpu_percent,mem_used_bytes,mem_bytes,disk_used_bytes,disk_total_bytes,rtt_ms,last_seen,agent_version";

/** A server as hostSummaryFields reads it. */
export type HostSummary = RecordModel &
  Pick<
    Host,
    | "name"
    | "roles"
    | "status"
    | "stream_open"
    | "ip4"
    | "public_ip4"
    | "os"
    | "cpu_percent"
    | "mem_used_bytes"
    | "mem_bytes"
    | "disk_used_bytes"
    | "disk_total_bytes"
    | "rtt_ms"
    | "last_seen"
    | "agent_version"
  > & { updated: string };

/** Where a server's CPU, RAM or disk turns amber and red, as in the design's
 * Overview: from 75 % and from 90 %. */
export const hostUsageLevels = { warnAt: 75, errAt: 90 } as const;

const HostsContext = createContext<ListState<HostSummary> | null>(null);

/** Gives the pages under the Layout its one live list of the servers, so
 * that a page needs no list of its own, nor a subscription. */
export function HostsProvider({
  value,
  children,
}: {
  value: ListState<HostSummary>;
  children: ReactNode;
}) {
  return <HostsContext.Provider value={value}>{children}</HostsContext.Provider>;
}

/** The Layout's live list of the servers, with hostSummaryFields only. */
export function useHosts(): ListState<HostSummary> {
  const hosts = useContext(HostsContext);
  if (!hosts) {
    throw new Error("useHosts needs a HostsProvider, as the Layout gives its pages");
  }
  return hosts;
}
