import { fixtureInput } from "../../support/runtime-mcp"
import { verify, assert, assertEqual, waitTurn, waitOutput } from "../../support/context"
await verify("session-cancel", async ({ client, marker, computer, startAgent, objects }) => {
  const target = await computer("helmr-delay-smoke")
  const { session, turn } = await startAgent("delay-smoke", {
    computer: target, input: { marker, delayMs: 120_000 }, idempotencyKey: `cancel:${marker}`,
  })
  await waitOutput(session, turn, value => value !== null && typeof value === "object" && "phase" in value && value.phase === "before-delay")
  const queued = await session.enqueue(fixtureInput({ marker: `${marker}:queued`, delayMs: 1 }), { idempotencyKey: `queued:${marker}` })
  objects.turn_ids.push(queued.id)
  await session.cancel({ idempotencyKey: `cancel-session:${marker}` })
  await Promise.all([waitTurn(turn, ["cancelled"]), waitTurn(queued, ["cancelled"])])
  assertEqual((await session.retrieve()).status, "cancelled", "Session cancellation did not settle")
  let cursor: string | undefined, found = false
  do {
    const page = await client.sessions.list({ status: "cancelled", limit: 100, cursor })
    found ||= page.items.some(item => item.id === session.id)
    cursor = page.nextCursor
  } while (!found && cursor !== undefined)
  assert(found, "Session list omitted the cancelled Session")
  return { activeCancelled: true, queuedCancelled: true, listed: true }
})
