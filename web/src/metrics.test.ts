import { describe, expect, it } from "vitest";
import {
  formatBytes, formatCores, formatCount, formatNumber, formatPercent, formatRate, formatSeconds, nearest, niceTicks, segments,
  seriesName, summarize, total,
} from "./metrics";

describe("formats", () => {
  it.each<[number, string]>([
    [0, "0 B"], [512, "512 B"], [1536, "1.5 KiB"], [842e6, "803 MiB"], [256 * 1024 ** 2, "256 MiB"], [8e9, "7.45 GiB"],
  ])("bytes %d → %s", (v, want) => expect(formatBytes(v)).toBe(want));

  it.each<[number, string]>([[0, "0"], [0.004, "4m"], [0.0005, "0.5m"], [0.25, "0.25"], [1, "1"], [3.5, "3.5"], [12.25, "12.3"]])(
    "cores %d → %s", (v, want) => expect(formatCores(v)).toBe(want));

  it.each<[number, string]>([[0, "0/s"], [0.03, "0.03/s"], [2.5, "2.5/s"], [12, "12/s"], [1234, "1.2k/s"]])(
    "rate %d → %s", (v, want) => expect(formatRate(v)).toBe(want));

  it.each<[number, string]>([[0.18, "180 ms"], [0.0042, "4.2 ms"], [2.5, "2.5 s"], [1, "1 s"]])(
    "seconds %d → %s", (v, want) => expect(formatSeconds(v)).toBe(want));

  it("counts, percentages, plain numbers", () => {
    expect(formatCount(3)).toBe("3");
    expect(formatCount(0.5)).toBe("0.5");
    expect(formatPercent(1, 4)).toBe("25%");
    expect(formatPercent(1, 0)).toBe("—");
    expect(formatPercent(0.05, 1)).toBe("5%");
    expect(formatNumber(1500000)).toBe("1.5M");
    expect(formatNumber(0.00012)).toBe("0.00012");
  });
});

describe("niceTicks", () => {
  it("steps in 1, 2, 2.5, 5 × 10^n from zero to at least the maximum", () => {
    expect(niceTicks(0.9)).toEqual([0, 0.25, 0.5, 0.75, 1]);
    expect(niceTicks(7)).toEqual([0, 2, 4, 6, 8]);
    expect(niceTicks(100)).toEqual([0, 25, 50, 75, 100]);
    expect(niceTicks(0)).toEqual([0, 1]);
  });
  it("bytes step in powers of two", () => {
    expect(niceTicks(900 * 1024 ** 2, 4, true)).toEqual([0, 256, 512, 768, 1024].map((m) => m * 1024 ** 2));
  });
  it("always covers the maximum", () => {
    for (const m of [0.013, 3.3, 47, 999, 12345]) {
      const t = niceTicks(m);
      expect(t[t.length - 1]).toBeGreaterThanOrEqual(m);
      expect(t.length).toBeLessThanOrEqual(6);
    }
  });
});

describe("series helpers", () => {
  const pts = [{ t: 0, v: 1 }, { t: 30, v: 3 }, { t: 60, v: 2 }, { t: 150, v: 5 }, { t: 180, v: 4 }];
  it("splits where samples are missing", () => {
    expect(segments(pts, 30).map((s) => s.map((p) => p.t))).toEqual([[0, 30, 60], [150, 180]]);
    expect(segments([], 30)).toEqual([]);
  });
  it("summarizes", () => {
    expect(summarize(pts)).toEqual({ latest: 4, min: 1, max: 5, mean: 3 });
    expect(summarize(null)).toBeNull();
  });
  it("finds the nearest sample", () => {
    expect(nearest(pts, 44)?.t).toBe(30);
    expect(nearest(pts, 46)?.t).toBe(60);
    expect(nearest(pts, -10)?.t).toBe(0);
    expect(nearest(pts, 999)?.t).toBe(180);
    expect(nearest([], 1)).toBeUndefined();
  });
  it("totals restarts", () => {
    expect(total([{ t: 0, v: 1 }, { t: 60, v: 2.0000001 }])).toBe(3);
    expect(total(null)).toBe(0);
  });
  it("names explorer series", () => {
    expect(seriesName({ __name__: "up", job: "x", instance: "a" })).toBe('up{instance="a", job="x"}');
    expect(seriesName({ namespace: "shop" })).toBe('{namespace="shop"}');
    expect(seriesName({})).toBe("{}");
  });
});
