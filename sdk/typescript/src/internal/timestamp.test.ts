import { describe, expect, test } from "bun:test"

import { timestampString } from "./timestamp"

describe("timestampString", () => {
  test("preserves canonical UTC RFC 3339 timestamps", () => {
    for (const value of [
      "2026-08-06T07:15:30Z",
      "2026-08-06T07:15:30.123456789Z",
      "2024-02-29T23:59:59Z",
    ]) {
      expect(timestampString(value, "value")).toBe(value)
    }
  })

  test("rejects normalized, offset, and malformed timestamps", () => {
    for (const value of [
      "2026-02-30T00:00:00Z",
      "2026-08-06T07:15:30+00:00",
      "2026-08-06 07:15:30Z",
      "2026-08-06T24:00:00Z",
      "2026-08-06T07:15:60Z",
    ]) {
      expect(() => timestampString(value, "value")).toThrow(
        "value must be a UTC RFC 3339 timestamp",
      )
    }
  })
})
