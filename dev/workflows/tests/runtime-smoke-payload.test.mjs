import { describe, expect, test } from "bun:test"
import { runtimeSmokePayload } from "../tasks/smoke/runtime.ts"

describe("runtime smoke payload", () => {
  test("accepts only canonical UUIDv7 external Token IDs", () => {
    expect(runtimeSmokePayload.safeParse({
      externalTokenId: "01a029e7-3c4a-7395-b578-6bfa2d822a26",
    }).success).toBe(true)
    expect(runtimeSmokePayload.safeParse({
      externalTokenId: "tok_abcdefghijklmnopqrstuvwxyz",
    }).success).toBe(false)
    expect(runtimeSmokePayload.safeParse({
      externalTokenId: "550e8400-e29b-41d4-a716-446655440000",
    }).success).toBe(false)
  })
})
