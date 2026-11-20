import { createContext, useContext, type ReactNode } from "react";

import type { ListOptions, ListState } from "./realtime";
import type { Alert } from "./types";

/** The alerts that are not resolved, newest first, with the fields the
 * sidebar and the Overview show. The Layout reads them once for every page
 * (#33), and the sidebar's count and the Overview's Needs attention are
 * worked out from that one list, so that a page reads no alerts of its own:
 * PocketBase holds up, by 0 to 500 ms at random, the fourth list with a
 * filter of one collection from one address that finds nothing within 3
 * seconds (apis/record_crud.go), which the Layout's three reads
 * (sectionAlertsQuery) and the Overview's own made of a load on a panel with
 * no open alert. The count is the panel's, of all of them; the rest sees the
 * newest 200, which Needs attention says when there are more. */
export const openAlertsQuery: ListOptions = {
  filter: "status != 'resolved'",
  sort: "-last_seen",
  perPage: 200,
  fields:
    "id,collectionId,collectionName,rule,target,host,severity,status,first_seen,last_seen,message,updated",
};

/** The open alerts of some rules, as a section of the sidebar is marked by
 * them: its dot and its count do not depend on the newest 200 of
 * openAlertsQuery, past which a certificate's alert falls behind many newer
 * alerts of backups. "critical" sorts before "info" and "warning", so the
 * one record read says whether any is critical; the panel's count says how
 * many there are. */
export function sectionAlertsQuery(rules: readonly string[]): ListOptions {
  return {
    filter: `status != 'resolved' && (${rules.map((rule) => `rule = '${rule}'`).join(" || ")})`,
    sort: "severity",
    perPage: 1,
    fields: "id,collectionId,collectionName,severity",
  };
}

const AlertsContext = createContext<ListState<Alert> | null>(null);

/** Gives the pages under the Layout its one live list of the open alerts,
 * so that a page needs no read of them of its own, nor a subscription. */
export function AlertsProvider({
  value,
  children,
}: {
  value: ListState<Alert>;
  children: ReactNode;
}) {
  return <AlertsContext.Provider value={value}>{children}</AlertsContext.Provider>;
}

/** The Layout's live list of the open alerts, as openAlertsQuery reads them. */
export function useOpenAlerts(): ListState<Alert> {
  const alerts = useContext(AlertsContext);
  if (!alerts) {
    throw new Error("useOpenAlerts needs an AlertsProvider, as the Layout gives its pages");
  }
  return alerts;
}
