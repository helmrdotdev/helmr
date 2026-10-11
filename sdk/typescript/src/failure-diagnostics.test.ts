import { describe, expect, test } from "bun:test"
import { HelmrClient } from "./index"

const sessionId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36"
const turnId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
const timestamp = "2026-07-24T11:00:00Z"

describe("Turn error read boundary", () => {
  const read = async (error: unknown, status = "failed", expired = false) => {
    const response = {
      id: turnId, session_id: sessionId, sequence: 1,
      source: { type: "external" }, status, error,
      created_at: timestamp, terminal_at: timestamp,
      interrupt_requested: false, accepts_messages: false,
      ...(expired ? { payload_expired_at: timestamp } : { input: [] }),
    }
    const client = new HelmrClient({
      url: "https://api.example.test", apiKey: "key",
      fetch: (async () => Response.json(response)) as typeof fetch,
    })
    return (await client.sessions.get(sessionId).turn(turnId).retrieve()).error
  }

  for (const code of ["future_diagnostic", " ", "future/診断", "x".repeat(129)]) {
    test(`preserves opaque code ${JSON.stringify(code)}`, async () => {
      expect(await read({ code, message: "diagnosis", extra: true }))
        .toEqual({ code, message: "diagnosis" })
    })
  }
  for (const [name, error] of [
    ["null envelope", null], ["array envelope", []],
    ["null code", { code: null, message: "diagnosis" }],
    ["number code", { code: 42, message: "diagnosis" }],
    ["empty code", { code: "", message: "diagnosis" }],
    ["missing code", { message: "diagnosis" }],
    ["non-string message", { code: "future_diagnostic", message: null }],
  ] as const) {
    test(`rejects ${name}`, async () => {
      await expect(read(error)).rejects.toThrow("Turn.error")
    })
  }
  test("errors belong to failed Turns and expired payloads omit their message", async () => {
    const error = { code: "application_failed", message: "diagnosis" }
    await expect(read(error, "cancelled")).rejects.toThrow("Turn.error")
    expect(await read({ code: error.code }, "failed", true)).toEqual({ code: error.code })
    await expect(read(error, "failed", true)).rejects.toThrow("Turn.error")
  })
})
