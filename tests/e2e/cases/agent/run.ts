import { verify, assert, completedResult } from "../../support/context"

await verify("agent", async ({ marker, computer, startAgent }) => {
  const target = await computer("verification")
  const started = await startAgent("verification-agent", {
    computer: target, input: { marker }, idempotencyKey: `verification:agent:${marker}`,
  })
  const output = await completedResult(started.turn, 180_000)
  assert.deepEqual(output, { marker, turnId: started.turn.id, sessionId: started.session.id, computerId: target.id })
  return { output }
})
