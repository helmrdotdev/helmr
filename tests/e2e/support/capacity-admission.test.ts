import { expect, spyOn, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import { capacityAwareStartAgent } from "./capacity-admission"
import { deadline, verify, type StartAgent } from "./context"

const request = { computer: { id: "01900000-0000-7000-8000-000000000003" },
  input: { marker: "capacity-test" }, idempotencyKey: "same-admission" }
const capacityError = Object.assign(new Error("capacity unavailable"), { code: "execution_capacity_unavailable" })

test("capacity retries preserve the request identity and wait before retrying", async () => {
  const calls: Parameters<StartAgent>[] = []
  const timestamps: number[] = []
  const stop = new Error("stop after retry")
  const start = capacityAwareStartAgent(async (...args) => {
    calls.push(args)
    timestamps.push(performance.now())
    throw calls.length === 1 ? capacityError : stop
  })
  await expect(start("probe", request)).rejects.toBe(stop)
  expect(calls).toHaveLength(2)
  for (const call of calls) {
    expect(call[0]).toBe("probe")
    expect(call[1]).toBe(request)
    expect(call[1].idempotencyKey).toBe("same-admission")
    expect(call[2]?.signal).toBe(calls[0]![2]?.signal)
  }
  expect(timestamps[1]! - timestamps[0]!).toBeGreaterThanOrEqual(490)
})

for (const code of ["unauthorized", "forbidden", "admission_unavailable", "environment_execution_limit",
  "execution_capacity_exhausted", "execution_capacity_incompatible", "execution_settling", "capacity_unavailable", "idempotency_conflict", undefined]) {
  test(`unrelated error ${code ?? "without code"} is returned immediately`, async () => {
    let calls = 0
    const error = Object.assign(new Error("not retryable here"), { code })
    const start = capacityAwareStartAgent(async () => { calls++; throw error })
    await expect(start("probe", request)).rejects.toBe(error)
    expect(calls).toBe(1)
  })
}

test("an empty idempotency key cannot start retryable work", async () => {
  let calls = 0
  const start = capacityAwareStartAgent(async () => { calls++; throw capacityError })
  await expect(start("probe", { ...request, idempotencyKey: "" })).rejects.toThrow("explicit idempotency key")
  expect(calls).toBe(0)
})

test("cancellation before admission starts no work", async () => {
  let calls = 0
  const reason = new Error("cancelled")
  const start = capacityAwareStartAgent(async () => { calls++; throw capacityError })
  await expect(start("probe", request, { signal: AbortSignal.abort(reason) })).rejects.toBe(reason)
  expect(calls).toBe(0)
})

test("deadline during the retry delay prevents another admission", async () => {
  let calls = 0
  const start = capacityAwareStartAgent(async () => { calls++; throw capacityError })
  await expect(start("probe", request, { signal: deadline(30) })).rejects.toThrow()
  await delay(550)
  expect(calls).toBe(1)
})

test("cancellation concurrent with capacity rejection prevents retry", async () => {
  const controller = new AbortController(), reason = new Error("cancelled")
  let calls = 0
  const start = capacityAwareStartAgent(async () => {
    calls++
    controller.abort(reason)
    throw capacityError
  })
  await expect(start("probe", request, { signal: controller.signal })).rejects.toBe(reason)
  expect(calls).toBe(1)
})

test("native admission tracks only success and aborts an in-flight request at the overall bound", async () => {
  const root = await mkdtemp(join(tmpdir(), "helmr-capacity-")), old = { ...process.env }
  const session = "01900000-0000-7000-8000-000000000001", turn = "01900000-0000-7000-8000-000000000002"
  const bodies: string[] = [], timers: number[] = []
  let inFlightSignal: AbortSignal | undefined
  const transport = spyOn(globalThis, "fetch").mockImplementation((async (url, init) => {
    if (String(url).endsWith("/cancel")) return Response.json({ id: request.computer.id, session_id: session, status: "accepted" })
    bodies.push(String(init?.body))
    if (bodies.length === 1) return Response.json({ error: { code: capacityError.code, message: capacityError.message } }, { status: 503 })
    if (bodies.length === 2) return Response.json({ session_id: session,
      turn: { session_id: session, turn_id: turn, sequence: 1 }, close_after_idle_ms: null })
    inFlightSignal = init?.signal ?? undefined
    await delay(1000, undefined, { signal: inFlightSignal })
    throw new Error("request did not abort")
  }) as typeof fetch)
  try {
    Object.assign(process.env, { HELMR_API_URL: "http://127.0.0.1:1", HELMR_API_KEY: "fixture",
      HELMR_EVIDENCE_DIR: join(root, "evidence"), HELMR_EXPECTED_BUNDLE_DIGEST: "" })
    await verify("capacity-admission", async ({ startAgent, objects }) => {
      const start = capacityAwareStartAgent(async (...args) => {
        expect(objects.session_ids).toEqual([])
        expect(objects.turn_ids).toEqual([])
        return startAgent(...args)
      })
      await start("probe", request)
      expect(bodies).toHaveLength(2)
      expect(bodies[0]).toBe(bodies[1])
      expect(JSON.parse(bodies[0]!)).toEqual({ agent: "probe", computer_id: request.computer.id,
        input: [{ type: "text", text: JSON.stringify(request.input) }], idempotency_key: request.idempotencyKey })
      expect(objects.session_ids).toEqual([session])
      expect(objects.turn_ids).toEqual([turn])

      // Compress only the overall timer; leave the native 30-second request timer intact.
      const nativeTimer = globalThis.setTimeout
      const timer = spyOn(globalThis, "setTimeout").mockImplementation(((...[callback, milliseconds, ...args]: Parameters<typeof setTimeout>) => {
        timers.push(milliseconds ?? 0)
        return nativeTimer(callback, milliseconds === 120_000 ? 30 : milliseconds, ...args)
      }) as typeof setTimeout)
      let pending: ReturnType<StartAgent>
      try {
        pending = capacityAwareStartAgent(startAgent)("probe", { ...request, idempotencyKey: "bounded-admission" })
      } finally { timer.mockRestore() }
      await expect(pending).rejects.toThrow("Verification deadline exceeded")
      expect(timers).toContain(120_000)
      expect(timers).toContain(30_000)
      expect(inFlightSignal?.aborted).toBe(true)
      expect(bodies).toHaveLength(3)
      expect(objects.session_ids).toEqual([session])
      expect(objects.turn_ids).toEqual([turn])
    })
  } finally {
    transport.mockRestore()
    for (const key of ["HELMR_API_URL", "HELMR_API_KEY", "HELMR_EVIDENCE_DIR", "HELMR_EXPECTED_BUNDLE_DIGEST"]) {
      if (old[key] === undefined) delete process.env[key]
      else process.env[key] = old[key]
    }
    await rm(root, { recursive: true, force: true })
  }
})
