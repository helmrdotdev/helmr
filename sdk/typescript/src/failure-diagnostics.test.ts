import { describe, expect, test } from "bun:test"
import { HelmrClient } from "./index"

const id = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36"
const runId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
const timestamp = "2026-07-24T11:00:00Z"

for (const kind of ["schedule", "session"] as const) {
  describe(`${kind} diagnostic read boundary`, () => {
    const read = async (failure: unknown, status?: string) => {
      const response = kind === "schedule" ? {
        id, task_id: "scheduled-task", generation: 1,
        effective_from: timestamp, cron: { pattern: "0 * * * *", timezone: "UTC" },
        status: status ?? "errored", last_failure: failure,
        created_at: timestamp, updated_at: timestamp,
      } : {
        id, actor_id: "actor", deployment_id: runId, workspace_id: runId,
        status: status ?? "failed", failure,
        created_at: timestamp, updated_at: timestamp,
      }
      const client = new HelmrClient({
        url: "https://api.example.test", apiKey: "key",
        fetch: (async () => Response.json(response)) as typeof fetch,
      })
      return kind === "schedule"
        ? (await client.schedules.retrieve(id)).lastFailure
        : (await client.sessions.retrieve(id)).failure
    }

    for (const code of ["future_diagnostic", " ", "future/診断", "x".repeat(129)]) {
      test(`preserves opaque code ${JSON.stringify(code)}`, async () => {
        const details = { run_id: runId, custom: { value: 1 } }
        const failure = await read({ code, message: "diagnosis", details, extra: true })
        expect(failure).toEqual({ code, message: "diagnosis",
          details: kind === "schedule" ? details : { runId } })
      })
    }
    for (const [name, failure] of [
      ["null envelope", null], ["array envelope", []],
      ["null code", { code: null, message: "diagnosis", details: {} }],
      ["number code", { code: 42, message: "diagnosis", details: {} }],
      ["empty code", { code: "", message: "diagnosis", details: {} }],
      ["missing code", { message: "diagnosis", details: {} }],
      ["empty message", { code: "future_diagnostic", message: "", details: {} }],
      ["null details", { code: "future_diagnostic", message: "diagnosis", details: null }],
    ] as const) {
      test(`rejects ${name}`, async () => {
        await expect(read(failure)).rejects.toThrow()
      })
    }
    if (kind === "session") {
      test("keeps reserved cancellation and run identity validation", async () => {
        const failure = { code: "cancelled", message: "cancelled", details: {} }
        expect(await read(failure, "cancelled")).toEqual(failure)
        await expect(read(failure, "failed")).rejects.toThrow("inconsistent")
        await expect(read({ ...failure, code: "future_diagnostic" }, "cancelled")).rejects.toThrow("inconsistent")
        await expect(read({ ...failure, code: "future_diagnostic" }, "open")).rejects.toThrow("inconsistent")
        await expect(read({ ...failure, code: "future_diagnostic", details: { run_id: "invalid" } })).rejects.toThrow()
      })
    }
  })
}
