import { expect, test } from "bun:test"
import type { SessionEvent, SessionEventPage } from "../contract"
import { sessionEvents } from "./session-events"

const event = (sequence: number): SessionEvent => ({
  sessionId: "session", turnId: null, sequence,
  createdAt: "2026-10-07T00:00:00Z", kind: "turn.output", data: [],
})
test("event stream resumes each page cursor without prefetching past consumer demand", async () => {
  const calls: number[] = []
  const pages: SessionEventPage[] = [
    { records: [event(2), event(3)], nextAfter: 3, hasMore: true, retainedAfter: 0 },
    { records: [event(4)], nextAfter: 4, hasMore: false, retainedAfter: 0 },
  ]
  const events = sessionEvents(async (query) => {
    calls.push(query!.after!)
    return pages.shift()!
  })
  const stream = events.stream({ after: 1, limit: 2 })
  expect((await stream.next()).value?.sequence).toBe(2)
  expect(calls).toEqual([1])
  expect((await stream.next()).value?.sequence).toBe(3)
  expect(calls).toEqual([1])
  expect((await stream.next()).value?.sequence).toBe(4)
  expect(calls).toEqual([1, 3])
  await stream.return()
})
test("event stream aborts an idle tail and propagates retention failure", async () => {
  const controller = new AbortController()
  let called!: () => void
  const requested = new Promise<void>((resolve) => { called = resolve })
  const events = sessionEvents(async (_query, options) => {
    expect(options?.signal).toBe(controller.signal)
    called()
    return { records: [], nextAfter: 0, hasMore: false, retainedAfter: 0 }
  })
  const stream = events.stream({}, { signal: controller.signal })
  const pending = stream.next()
  await requested
  const abort = new Error("stop tail")
  controller.abort(abort)
  await expect(pending).rejects.toBe(abort)
  const expired = new Error("cursor_expired")
  const failed = sessionEvents(async () => { throw expired }).stream()
  await expect(failed.next()).rejects.toBe(expired)
})
