import { describe, expect, it } from "vitest";

import { chartSegments, nearestIndex, timeTicks } from "./charts";

/** Seconds of a moment in the browser's own time zone, which the ticks follow. */
const local = (month: number, date: number, hours: number, minutes = 0) =>
  new Date(2026, month - 1, date, hours, minutes).getTime() / 1000;

describe("chartSegments", () => {
  it("breaks the line where samples are missing", () => {
    expect(chartSegments([0, 10, 20, 50, 60], 10)).toEqual([
      { from: 0, to: 2 },
      { from: 3, to: 4 },
    ]);
  });

  it("allows a late sample, but not two missing ones", () => {
    expect(chartSegments([0, 10, 35], 10)).toEqual([{ from: 0, to: 2 }]);
    expect(chartSegments([0, 10, 36], 10)).toEqual([
      { from: 0, to: 1 },
      { from: 2, to: 2 },
    ]);
  });

  it("has nothing to draw without samples, and a lone sample is a segment", () => {
    expect(chartSegments([], 10)).toEqual([]);
    expect(chartSegments([5], 10)).toEqual([{ from: 0, to: 0 }]);
  });
});

describe("nearestIndex", () => {
  const times = [0, 10, 20, 30];

  it("finds the sample nearest to a moment", () => {
    expect(nearestIndex(times, 4)).toBe(0);
    expect(nearestIndex(times, 6)).toBe(1);
    expect(nearestIndex(times, 21)).toBe(2);
  });

  it("prefers the earlier of two samples equally near", () => {
    expect(nearestIndex(times, 15)).toBe(1);
  });

  it("stops at the ends", () => {
    expect(nearestIndex(times, -50)).toBe(0);
    expect(nearestIndex(times, 500)).toBe(3);
    expect(nearestIndex([7], 100)).toBe(0);
  });
});

describe("timeTicks", () => {
  it("puts an hour on whole quarters, away from now", () => {
    const from = local(10, 16, 10, 7);
    expect(timeTicks(from, from + 3600, 4)).toEqual({
      step: 900,
      ticks: [local(10, 16, 10, 15), local(10, 16, 10, 30), local(10, 16, 10, 45)],
    });
  });

  it("puts a day on every sixth hour, across midnight", () => {
    const from = local(10, 16, 22, 31);
    expect(timeTicks(from, local(10, 17, 22, 31), 4).ticks).toEqual([
      local(10, 17, 0),
      local(10, 17, 6),
      local(10, 17, 12),
      local(10, 17, 18),
    ]);
  });

  it("puts a week on midnights, fewer when there is less room", () => {
    const from = local(10, 10, 22, 31);
    const to = local(10, 17, 22, 31);

    const wide = timeTicks(from, to, 7);
    expect(wide.step).toBe(86400);
    expect(wide.ticks).toEqual([11, 12, 13, 14, 15, 16, 17].map((date) => local(10, date, 0)));

    expect(timeTicks(from, to, 4).ticks).toEqual([12, 14, 16].map((date) => local(10, date, 0)));
  });

  it("has no ticks for an empty range", () => {
    expect(timeTicks(10, 10, 4).ticks).toEqual([]);
  });
});
