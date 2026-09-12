import { afterEach, beforeEach, expect, test } from "bun:test";

import { formatAbsolute, formatRelative } from "./time";

const NOW = new Date("2026-09-12T12:00:00Z").getTime();
const realNow = Date.now;
beforeEach(() => {
  Date.now = () => NOW;
});
afterEach(() => {
  Date.now = realNow;
});

function at(offsetMs: number): string {
  return new Date(NOW - offsetMs).toISOString();
}

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

test("renders missing or invalid values as a dash", () => {
  expect(formatRelative(undefined)).toBe("—");
  expect(formatRelative(null)).toBe("—");
  expect(formatRelative("")).toBe("—");
  expect(formatRelative("not a date")).toBe("—");
});

test("treats anything within 45 seconds as now", () => {
  expect(formatRelative(at(10_000))).toBe("just now");
  expect(formatRelative(at(-10_000))).toBe("in a moment");
});

test("picks the largest unit for past and future times", () => {
  expect(formatRelative(at(MINUTE))).toBe("1 minute ago");
  expect(formatRelative(at(5 * MINUTE))).toBe("5 minutes ago");
  expect(formatRelative(at(3 * HOUR))).toBe("3 hours ago");
  expect(formatRelative(at(2 * DAY))).toBe("2 days ago");
  expect(formatRelative(at(45 * DAY))).toBe("2 months ago");
  expect(formatRelative(at(400 * DAY))).toBe("1 year ago");
  expect(formatRelative(at(-90 * MINUTE))).toBe("in 2 hours");
  expect(formatRelative(at(-DAY))).toBe("in 1 day");
});

test("formats absolute times and hides missing ones", () => {
  expect(formatAbsolute(undefined)).toBe("");
  expect(formatAbsolute("invalid")).toBe("");
  const label = formatAbsolute("2026-09-12T12:00:00Z");
  expect(label).not.toBe("");
  expect(label).toContain("2026");
});
