import { expect, spyOn, test } from "bun:test"
import { HelmrClient } from "../client"
import { waitForTurn } from "./turn-wait"
import type { TurnState } from "../contract"

const sessionId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33"
const turnId = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34"
const otherId="019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35"
const at = "2026-10-07T00:00:00Z"
const pending: TurnState = { id: turnId, sessionId, sequence: 1, status: "running", input: [] }

test("wait reads exact Turn until durable completion and preserves null result and response", async () => {
  const methods: string[] = []
  let count = 0
  const client = new HelmrClient({ apiKey: "test", url: "https://api.example.test", fetch: (async (url, options) => {
    expect(new URL(String(url)).pathname).toBe(`/v1/sessions/${sessionId}/turns/${turnId}`)
    methods.push(options!.method!)
    const base = { id: turnId, session_id: sessionId, sequence: 1, input: [] }
    return Response.json(++count === 1 ? { ...base, status: "running" } : { ...base, status: "completed", terminal_at: at, result: null, response: [{ type: "text", text: "Saved." }] })
  }) as typeof fetch })
  const outcome = await client.sessions.ref(sessionId).turns.ref(turnId).wait()
  expect(outcome).toEqual({ status: "completed", result: null, response: [{ type: "text", text: "Saved." }] })
  expect(methods).toEqual(["GET", "GET"])
  const again = await client.sessions.ref(sessionId).turns.ref(turnId).wait({ timeout: "1s" })
  expect(again).toEqual({ status: "settled", outcome })
})

test("timeout returns a distinct observation and aborts an in-flight read", async () => {
  let observed: AbortSignal | undefined
  const result = await waitForTurn(sessionId, turnId, signal => {
    observed = signal
    // A supplied transport that ignores cancellation cannot extend the wait.
    return new Promise(() => {})
  }, { timeout: 10 })
  expect(result).toEqual({ status: "timeout" })
  expect(observed?.aborted).toBe(true)
})

test("timeout during processing does not synthesize a terminal outcome", async () => {
  let reads = 0
  expect(await waitForTurn(sessionId, turnId, async () => { reads++; return pending }, { timeout: "10ms" })).toEqual({ status: "timeout" })
  expect(reads).toBe(1)
})

test("transport and authorization failures remain errors, and caller abort is not timeout", async () => {
  const failure = Object.assign(new Error("denied"), { code: "forbidden" })
  await expect(waitForTurn(sessionId, turnId, async () => { throw failure }, { timeout: 100 })).rejects.toBe(failure)
  const controller = new AbortController(), reason = new Error("caller stopped observing")
  const observation = waitForTurn(sessionId, turnId, async () => new Promise(() => {}), { timeout: 1000, signal: controller.signal })
  controller.abort(reason)
  await expect(observation).rejects.toBe(reason)
  let reads = 0
  await expect(waitForTurn(sessionId, turnId, async () => { reads++; return pending }, { signal: controller.signal })).rejects.toBe(reason)
  expect(reads).toBe(0)
})

test("failed, interrupted, cancelled and expired outcomes retain their recorded meaning", async () => {
  for (const status of ["failed", "interrupted", "cancelled"] as const) {
    const error = status === "failed" ? { code: "handler_failed", message: "failed" } : undefined
    const state = { ...pending, status, ...(error ? { error } : {}) }
    expect(await waitForTurn(sessionId, turnId, async () => state)).toEqual({ status, ...(error ? { error } : {}) })
  }
  const state: TurnState = { id: turnId, sessionId, sequence: 1, status: "completed", payloadExpiredAt: at }
  expect(await waitForTurn(sessionId, turnId, async () => state)).toEqual({ status: "completed", payloadExpiredAt: at })
})

test("identity mismatch and invalid durations fail without changing work", async () => {
  await expect(waitForTurn(sessionId, turnId, async () => ({ ...pending, sessionId: otherId }))).rejects.toThrow("identity")
  for (const timeout of [0, -1, 0.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1, "0ms", "1.5s", "bad"]) {
    let reads = 0
    await expect(waitForTurn(sessionId, turnId, async () => { reads++; return pending }, { timeout })).rejects.toThrow("timeout")
    expect(reads).toBe(0)
  }
})

test("long observation deadlines do not overflow the native timer", async () => {
  const duration = 30 * 24 * 60 * 60 * 1000
  const maximumDelay = 2147483647
  let now = 0
  const scheduled: { callback: () => void; delay: number; handle: ReturnType<typeof setTimeout> }[] = []
  const clock = spyOn(performance, "now").mockImplementation(() => now)
  const timer = spyOn(globalThis, "setTimeout").mockImplementation(((callback: () => void, delay: number) => {
    const handle = (scheduled.length + 1) as unknown as ReturnType<typeof setTimeout>
    scheduled.push({ callback, delay, handle })
    return handle
  }) as typeof setTimeout)
  const clear = spyOn(globalThis, "clearTimeout").mockImplementation(() => {})
  const cancellation = new AbortController()
  let started!: (signal: AbortSignal) => void
  const reading = new Promise<AbortSignal>(resolve => { started = resolve })
  const observation = waitForTurn(sessionId, turnId, signal => {
    started(signal)
    return new Promise(() => {})
  }, { timeout: "30d", signal: cancellation.signal })
  // Cleanup can abort the observation after an assertion failure.
  void observation.catch(() => {})
  try {
    const signal = await reading
    expect(scheduled.map(item => item.delay)).toEqual([maximumDelay])
    expect(signal.aborted).toBe(false)

    now = maximumDelay
    scheduled[0]!.callback()
    expect(scheduled.map(item => item.delay)).toEqual([maximumDelay, duration - maximumDelay])
    expect(signal.aborted).toBe(false)

    // An early callback must rearm for the remainder rather than expire early.
    now = duration - 1
    scheduled[1]!.callback()
    expect(scheduled.map(item => item.delay)).toEqual([maximumDelay, duration - maximumDelay, 1])
    expect(signal.aborted).toBe(false)

    now = duration
    scheduled[2]!.callback()
    expect(await observation).toEqual({ status: "timeout" })
    expect(signal.aborted).toBe(true)
    expect(clear).toHaveBeenCalledTimes(1)
    expect(clear).toHaveBeenCalledWith(scheduled[2]!.handle)
  } finally {
    cancellation.abort()
    await observation.catch(() => {})
    clear.mockRestore()
    timer.mockRestore()
    clock.mockRestore()
  }
})

test("HTTP authorization failures are not retried or returned as timeout", async () => {
  let requests = 0
  const client = new HelmrClient({ apiKey: "test", url: "https://api.example.test", fetch: (async () => {
    requests++
    return Response.json({ error: { code: "forbidden", message: "read denied" } }, { status: 403 })
  }) as typeof fetch })
  await expect(client.sessions.ref(sessionId).turns.ref(turnId).wait({ timeout: "1s" })).rejects.toMatchObject({ code: "forbidden" })
  expect(requests).toBe(1)
})


test("deadline expiring before the first read returns timeout without an unhandled rejection", async () => {
  let clockReads = 0, reads = 0
  const clock = spyOn(performance, "now").mockImplementation(() => clockReads++ === 0 ? 0 : 2)
  try {
    expect(await waitForTurn(sessionId, turnId, async () => { reads++; return pending }, { timeout: 1 })).toEqual({ status: "timeout" })
    expect(reads).toBe(0)
    // Let the test runner observe any orphaned rejection from synchronous expiry.
    await new Promise(resolve => setTimeout(resolve, 0))
  } finally {
    clock.mockRestore()
  }
})

test("each poll releases its abort observation before the next read", async () => {
  const nativeTimeout = globalThis.setTimeout
  const timer = spyOn(globalThis, "setTimeout").mockImplementation(((callback: TimerHandler, delay?: number, ...args: unknown[]) => nativeTimeout(callback, delay === 500 ? 0 : delay, ...args)) as typeof setTimeout)
  const nativeAdd = AbortSignal.prototype.addEventListener
  const nativeRemove = AbortSignal.prototype.removeEventListener
  const active = new Set<unknown>()
  let removed = 0, reads = 0
  const add = spyOn(AbortSignal.prototype, "addEventListener").mockImplementation(function (this: AbortSignal, ...args: Parameters<typeof nativeAdd>) {
    if (args[0] === "abort") active.add(args[1])
    return nativeAdd.apply(this, args)
  })
  const remove = spyOn(AbortSignal.prototype, "removeEventListener").mockImplementation(function (this: AbortSignal, ...args: Parameters<typeof nativeRemove>) {
    if (args[0] === "abort" && active.delete(args[1])) removed++
    return nativeRemove.apply(this, args)
  })
  try {
    const outcome = await waitForTurn(sessionId, turnId, async () => {
      expect(active.size).toBe(1)
      expect(removed).toBe(reads * 2)
      reads++
      return reads === 50 ? { ...pending, status: "completed", result: true } : pending
    })
    expect(outcome).toEqual({ status: "completed", result: true })
    expect(active.size).toBe(0)
    expect(removed).toBe(99)
  } finally {
    timer.mockRestore()
    add.mockRestore()
    remove.mockRestore()
  }
})

test("known retained-capacity dependency stops observation without changing work", async () => {
  let reads = 0
  const state = { ...pending, status: "queued" as const, waitBlocked: "capacity_wait_blocked" as const }
  await expect(waitForTurn(sessionId, turnId, async () => { reads++; return state }, { timeout: "1s" })).rejects.toMatchObject({ code: "capacity_wait_blocked" })
  expect(reads).toBe(1)
  expect(await waitForTurn(sessionId, turnId, async () => ({ ...state, status: "cancelled" }))).toEqual({ status: "cancelled" })
})
