import { fixtureInput } from "../../support/runtime-mcp"
import { verify, assert, assertEqual, waitOutput, completedResult } from "../../support/context"
import { observePersistence, assertRestored } from "../../support/persistence"
import { assertSessionTurns } from "./assertions"

await verify("sessions", async ({ marker, objects, computer, startAgent }) => {
  const target = await computer("verification", "sessions")
  const first = await startAgent("verification-session", {
    computer: target, sessionKey: marker, input: { marker }, idempotencyKey: `session:${marker}`,
  })
  const firstResult = await completedResult(first.turn, 180_000)
  assert(firstResult !== null && typeof firstResult === "object" && "nonce" in firstResult && typeof firstResult.nonce === "string")
  const parked = await observePersistence("wait-parked", first.session.id)
  const second = await first.session.enqueue(fixtureInput({ marker }), { idempotencyKey: `second:${marker}` })
  objects.turn_ids.push(second.id)
  await waitOutput(first.session, second, value => value !== null && typeof value === "object" && "phase" in value && value.phase === "restored")
  const restored = await observePersistence("verify-restored", first.session.id)
  assertRestored(parked, restored, first.session.id)
  await second.send(fixtureInput("finish"), { idempotencyKey: `finish:${marker}` })
  const secondResult = await completedResult(second, 180_000)
  assertEqual((await first.turn.retrieve()).sequence, 1, "Initial Turn sequence changed")
  assertEqual((await second.retrieve()).sequence, 2, "Follow-up Turn sequence changed")
  assertSessionTurns({ marker, nonce: firstResult.nonce, sessionId: first.session.id, computerId: target.id }, firstResult, secondResult)
  return { parked, restored, turns: [firstResult, secondResult] }
})
