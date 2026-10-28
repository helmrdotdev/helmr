import { expect, test } from "bun:test"
import { getEventListeners } from "node:events"
import type { Message, Turn } from "@helmr/sdk"
import { fixtureInput } from "../../support/runtime-mcp"
import { waitForProbePermit } from "./permit"

function fixture(controller: AbortController, registration: Promise<void> = Promise.resolve()) {
  let message!: (value: Message) => unknown, ready!: () => void
  const observed = new Promise<void>(resolve => { ready = resolve })
  let readyWrites = 0
  const turn: Pick<Turn, "signal" | "onMessage" | "output"> = {
    signal: controller.signal,
    async onMessage(handler) { message = handler; await registration },
    output: { async pipe() { throw new Error("unexpected pipe") }, async write() { readyWrites++; ready(); return { sequence: 1 } } },
  }
  return { turn, observed, readyWrites: () => readyWrites,
    send: (value: string) => message({ id: "fixture-message", content: fixtureInput(value), source: { kind: "api_key" } }),
  }
}

test("cancelling a ready permit wait drains it and removes its abort listener", async () => {
  const controller = new AbortController(), target = fixture(controller)
  const waiting = waitForProbePermit(target.turn, { phase: "ready" })
  await target.observed
  const reason = new Error("Turn cancelled during operator observation")
  controller.abort(reason)
  await expect(waiting).rejects.toBe(reason)
  expect(getEventListeners(controller.signal, "abort")).toHaveLength(0)
  await target.send("probe") // A retained native callback is inert after cleanup.
  expect(target.readyWrites()).toBe(1)
})
test("an already aborted Turn neither registers messages nor emits readiness", async () => {
  const controller = new AbortController(); controller.abort(new Error("already cancelled"))
  const target = fixture(controller)
  await expect(waitForProbePermit(target.turn, {})).rejects.toThrow("already cancelled")
  expect(target.readyWrites()).toBe(0)
  expect(getEventListeners(controller.signal, "abort")).toHaveLength(0)
})
test("cancellation during native message registration does not strand the handler", async () => {
  const controller = new AbortController(), target = fixture(controller, new Promise(() => {}))
  const waiting = waitForProbePermit(target.turn, {})
  controller.abort(new Error("cancelled while registering"))
  await expect(waiting).rejects.toThrow("cancelled while registering")
  expect(target.readyWrites()).toBe(0)
  expect(getEventListeners(controller.signal, "abort")).toHaveLength(0)
})
test("a probe permit succeeds with cleanup, while a concurrent abort still prevents probing", async () => {
  for (const abort of [false, true]) {
    const controller = new AbortController(), target = fixture(controller)
    const waiting = waitForProbePermit(target.turn, {})
    await target.observed
    await target.send("unrelated")
    target.send("probe")
    if (abort) controller.abort(new Error("cancelled before probing"))
    if (abort) await expect(waiting).rejects.toThrow("cancelled before probing")
    else await waiting
    expect(getEventListeners(controller.signal, "abort")).toHaveLength(0)
  }
})

test("registration completing after cancellation cannot leak an inner rejection", async () => {
  let registered!: () => void
  const registration = new Promise<void>(resolve => { registered = resolve })
  const controller = new AbortController(), target = fixture(controller, registration)
  const unhandled: unknown[] = []
  const capture = (reason: unknown) => { unhandled.push(reason) }
  process.on("unhandledRejection", capture)
  try {
    const waiting = waitForProbePermit(target.turn, {})
    controller.abort(new Error("cancelled before registration completed"))
    await expect(waiting).rejects.toThrow("cancelled before registration completed")
    registered() // The losing setup branch now resumes and throwIfAborted rejects.
    await new Promise(resolve => setImmediate(resolve))
    expect(unhandled).toEqual([])
    expect(target.readyWrites()).toBe(0)
    expect(getEventListeners(controller.signal, "abort")).toHaveLength(0)
  } finally {
    process.off("unhandledRejection", capture)
  }
})
