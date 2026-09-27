import { expect, test } from "bun:test"
import { createClientCommandRef } from "./client-computer"
import { parseCommandLogPage, streamCommandLogs, type CommandLogRecord } from "./command-logs"

const id = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36"
const record: CommandLogRecord = { kind: "output", cursor: "next", stream: "stdout", content: new Uint8Array([0, 255]), observedAt: "2026-09-26T00:00:00Z" }

test("logs reads one finite page without retrieving or waiting for command state", async () => {
  const requests: string[] = []
  const command = createClientCommandRef(id, { async request(method, path) {
    requests.push(`${method} ${path}`)
    return { output_state: "open", logs: [{ kind: "output", stream: "stdout", cursor: "next", content_base64: "AP8=", observed_at: record.observedAt }], next_cursor: "next" }
  } })
  expect(await command.logs({ cursor: "old", limit: 1 })).toEqual({ items: [record], nextCursor: "next" })
  expect(requests).toEqual([`GET /v1/commands/${id}/logs?cursor=old&limit=1`])
})

test("delayed final delivery retries lag without advancing, then drains after outcome", async () => {
  const cursors: (string | undefined)[] = []
  let calls = 0
  const logs = streamCommandLogs(async query => {
    cursors.push(query.cursor)
    if (++calls === 1) throw Object.assign(new Error("lag"), { code: "telemetry_lagging" })
    if (calls === 2) return { outputState: "open", items: [record], nextCursor: "next" }
    return { outputState: "closed", items: [] }
  })
  expect(await Array.fromAsync(logs)).toEqual([record])
  expect(cursors).toEqual([undefined, undefined, "next"])
})

test("break stops further observation; reconnect starts after the last processed record", async () => {
  let reads = 0
  for await (const item of streamCommandLogs(async () => { reads++; return { outputState: "open", items: [record], nextCursor: "next" } })) {
    expect(item).toEqual(record)
    break
  }
  expect(reads).toBe(1)
  const cursors: (string | undefined)[] = []
  await Array.fromAsync(streamCommandLogs(async q => { cursors.push(q.cursor); return { outputState: "closed", items: [] } }, { after: record.cursor }))
  expect(cursors).toEqual(["next"])
})

test("abort stops polling and unrelated errors propagate", async () => {
  const controller = new AbortController()
  const logs = streamCommandLogs(async () => ({ outputState: "open", items: [] }), {}, { signal: controller.signal })
  const next = logs.next().catch(error => error)
  await new Promise(resolve => setTimeout(resolve, 10))
  const reason = new Error("stop observing")
  controller.abort(reason)
  expect(await next).toBe(reason)
  const unavailable = new Error("store unavailable")
  await expect(streamCommandLogs(async () => { throw unavailable }).next()).rejects.toBe(unavailable)
})

test("closed output drains without depending on result retrieval; unavailable tails fail", async () => {
  expect(await Array.fromAsync(streamCommandLogs(async () => ({ outputState: "closed", items: [] })))).toEqual([])
  await expect(streamCommandLogs(async () => ({ outputState: "unavailable", items: [] })).next()).rejects.toMatchObject({ code: "command_output_unavailable" })
})

test("binary data and explicit gaps survive parsing, invalid page cursors fail", () => {
  expect(parseCommandLogPage({ output_state: "closed", logs: [{ kind: "gap", stream: "stderr", cursor: "gap", from_sequence: "0", through_sequence: "4" }], next_cursor: "gap" }).items[0]).toEqual({ kind: "gap", stream: "stderr", cursor: "gap", fromSequence: "0", throughSequence: "4" })
  expect(() => parseCommandLogPage({ output_state: "closed", logs: [], next_cursor: "skip" })).toThrow("advanced")
})

test("unavailable terminal tail preserves captured output before reporting the loss", async () => {
  let reads = 0
  const logs = streamCommandLogs(async () => ++reads === 1
    ? { outputState: "unavailable", items: [record], nextCursor: record.cursor }
    : { outputState: "unavailable", items: [] })
  expect(await logs.next()).toEqual({ done: false, value: record })
  await expect(logs.next()).rejects.toMatchObject({ code: "command_output_unavailable" })
})
