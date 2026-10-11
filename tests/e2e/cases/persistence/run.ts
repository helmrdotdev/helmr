import { fixtureInput } from "../../support/runtime-mcp"
import { verify, assert, assertEqual, waitOutput, completedResult } from "../../support/context"
import { observePersistence, assertRestored } from "../../support/persistence"

await verify("persistence", async ({ marker, objects, computer, startAgent }) => {
  const target = await computer("verification", "persistence")
  const started = await startAgent("verification-persistence", {
    computer: target, input: { marker }, idempotencyKey: `persistence:${marker}`,
  })
  const before = await completedResult(started.turn, 180_000)
  assert(before !== null && typeof before === "object" && "nonce" in before && typeof before.nonce === "string")
  assertEqual(before, { marker, nonce: before.nonce, count: 1, turnId: started.turn.id, sessionId: started.session.id, computerId: target.id }, "Initial Turn state changed")
  const parked = await observePersistence("wait-parked", started.session.id)
  const second = await started.session.enqueue(fixtureInput({ marker, holdForObservation: true }), { idempotencyKey: `${marker}:resume` })
  objects.turn_ids.push(second.id)
  await waitOutput(started.session, second, value => value !== null && typeof value === "object" && "phase" in value && value.phase === "restored")
  const restored = await observePersistence("verify-restored", started.session.id)
  assertRestored(parked, restored, started.session.id)
  await second.send(fixtureInput("finish"), { idempotencyKey: `${marker}:finish` })
  const output = await completedResult(second, 180_000)
  assertEqual(output, { marker, nonce: before.nonce, count: 2, turnId: second.id, sessionId: started.session.id, computerId: target.id }, "Restored Session lost memory, file state or identity")
  return { before, parked, restored, output }
})
