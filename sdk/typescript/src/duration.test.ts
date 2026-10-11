import { expect, test } from "bun:test"
import { durationMilliseconds } from "./duration"

test("durations share integer milliseconds and single integer units", () => {
  for (const [input, output] of [[1, 1], [30000, 30000], ["1ms", 1], ["30s", 30000], ["2m", 120000], ["1h", 3600000], ["1d", 86400000]] as const) expect(durationMilliseconds(input)).toBe(output)
  for (const input of [0, -1, 1.5, Infinity, NaN, Number.MAX_SAFE_INTEGER + 1, "0s", "01s", "1.5s", "1h30m", "1", " 1s", "1s ", "999999999999999999999d"]) expect(() => durationMilliseconds(input)).toThrow()
})
