import { expect, test } from "bun:test";
import { bucketLabels, change, channelLabel, formatDuration } from "./usage";

test("formatDuration picks the shortest readable unit", () => {
  expect([formatDuration(40), formatDuration(372), formatDuration(9000), formatDuration(1_123_200)]).toEqual(["40 s", "6 min", "2.5 h", "312 h"]);
});

test("change compares with the previous window", () => {
  expect(change(118, 100)).toBe("+18%");
  expect(change(80, 100)).toBe("-20%");
  expect(change(5, 0)).toBeUndefined();
});

test("bucketLabels names each run-length bucket", () => {
  expect(bucketLabels([60, 600, 3600, 14400])).toEqual(["<1m", "1m–10m", "10m–1h", "1h–4h", "4h+"]);
});

test("channelLabel names channels, groups DMs and falls back to the ID", () => {
  expect(channelLabel({ id: "C1", name: "support" })).toBe("# support");
  expect(channelLabel({ id: "D1" })).toBe("Direct messages");
  expect(channelLabel({ id: "C9" })).toBe("C9");
});
