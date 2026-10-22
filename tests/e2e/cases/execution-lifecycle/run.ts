import { setTimeout as delay } from "node:timers/promises"
import { fixtureInput } from "../../support/runtime-mcp"
import { verify, assert, assertEqual, completedResult, deadline } from "../../support/context"

await verify("execution-lifecycle", async ({ marker, objects, computer, startAgent }) => {
  const target = await computer("verification", "execution-lifecycle")
  const started = await startAgent("verification-execution-lifecycle", {
    computer: target, input: {}, idempotencyKey: `lifecycle:${marker}`,
  })
  const result = (value: unknown) => {
    assert(value && typeof value === "object" && "nonce" in value && "pid" in value && "ticks" in value && "nativeCount" in value)
    assert(typeof value.nonce === "string" && typeof value.pid === "number" && typeof value.ticks === "number" && typeof value.nativeCount === "number")
    return value as { nonce: string; pid: number; ticks: number; nativeCount: number }
  }
  const first = result(await completedResult(started.turn, 180_000))
  await delay(2_000)
  assertEqual((await target.get()).residency, "running", "Manual policy suspended after a Turn")
  const observe = async (key: string) => {
    const turn = await started.session.enqueue(fixtureInput({}), { idempotencyKey: `${marker}:${key}` })
    objects.turn_ids.push(turn.id)
    return result(await completedResult(turn, 180_000))
  }
  const before = await observe("before")
  assert(before.nativeCount > first.nativeCount && before.ticks > first.ticks, "Background progress stopped after the first Turn")
  const sleep = await target.hibernate({ idempotencyKey: `${marker}:sleep` })
  const wait = async (residency: "running" | "parked") => {
    const signal = deadline(180_000)
    for (;;) {
      const state = await target.get({ signal })
      assert(state.executionControl?.status !== "failed", `Execution control failed: ${JSON.stringify(state.executionControl)}`)
      if (state.residency === residency && state.executionControl?.status === "completed") return state
      await delay(250, undefined, { signal })
    }
  }
  const sleeping = await wait("parked")
  const beforeWake = await started.session.turns.list()
  await delay(3_000)
  const wake = await target.wake({ idempotencyKey: `${marker}:wake` })
  const awake = await wait("running")
  const afterWake = await started.session.turns.list()
  assertEqual(afterWake.items.map(turn => turn.id), beforeWake.items.map(turn => turn.id), "Wake fabricated a Turn")
  await delay(1_000)
  const after = await observe("after")
  assertEqual(after.nonce, before.nonce, "Initialization reran across Hibernate/Wake")
  assertEqual(after.pid, before.pid, "Native background process identity changed")
  assert(after.nativeCount > before.nativeCount && after.ticks > before.ticks, "Background work did not continue after Wake")
  return { first, before, sleep, sleeping, wake, awake, after }
})
