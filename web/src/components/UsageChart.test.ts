import { describe, expect, it } from "vitest";

import { cpuAxisMax } from "./UsageChart";

describe("cpuAxisMax", () => {
  it("stays at 100 % for a host and a container below one core", () => {
    expect(cpuAxisMax([])).toBe(100);
    expect(cpuAxisMax([12.5, 99.9])).toBe(100);
  });

  it("grows by whole cores for a container on more than one", () => {
    expect(cpuAxisMax([100.1])).toBe(200);
    expect(cpuAxisMax([40, 250, Number.NaN])).toBe(300);
  });
});
