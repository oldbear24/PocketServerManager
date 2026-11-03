import type { DiagConnectionEvent } from "./types";

export type StripCell = "ok" | "warn" | "err" | "off";

/**
 * Turns connection events into the cells of the 24-hour connection strip: "ok" for
 * a stable connection, "warn" when it dropped and came back in that period, "err"
 * when it stayed down, and "off" before the first known event.
 */
export function connectionStrip(
  events: DiagConnectionEvent[],
  now: number,
  cells = 48,
  span = 24 * 3600 * 1000,
): StripCell[] {
  const sorted = [...events]
    .map((event) => ({ ...event, time: new Date(event.at).getTime() }))
    .sort((a, b) => a.time - b.time);

  const width = span / cells;
  const start = now - span;

  const result: StripCell[] = [];
  let index = 0;
  let connected: boolean | null = null;

  for (let cell = 0; cell < cells; cell++) {
    const end = start + width * (cell + 1);
    let changed = false;

    while (index < sorted.length && sorted[index].time < end) {
      const event = sorted[index];
      if (event.time >= start + width * cell && event.kind === "disconnected") {
        changed = true;
      }
      connected = event.kind === "connected";
      index++;
    }

    if (connected === null) {
      result.push("off");
    } else if (!connected) {
      result.push("err");
    } else {
      result.push(changed ? "warn" : "ok");
    }
  }

  return result;
}
