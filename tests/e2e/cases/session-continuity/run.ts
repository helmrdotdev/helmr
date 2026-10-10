import { fixtureInput } from "../../support/runtime-mcp"
import { setTimeout as delay } from "node:timers/promises"
import { verify, assert, assertEqual, completedResult, waitOutput, deadline } from "../../support/context"
import { cleanupChildren } from "../helpers/cleanup"

await verify("session-continuity", async ({ client, marker, objects, cleanup, computer, startAgent }) => {
  const target = await computer("helmr-helper-target-smoke", "continuity-target")
  const caller = await computer("helmr-helper-caller-smoke", "continuity-caller")
  const key = `continuity:${marker}`
  const first = await startAgent("helper-continuity", {
    sessionKey: key, computer: caller, input: { marker: `${marker}:first`, childComputerId: target.id },
    idempotencyKey: `continuity:start:${marker}`,
  })
  cleanup(() => cleanupChildren(client, first.session.id, [caller.id, target.id], objects.session_ids))
  // Enqueue before waiting: admitted Turns must run serially in this Session.
  const nextInput = { marker: `${marker}:second`, childComputerId: target.id }
  const second = await first.session.enqueue(fixtureInput(nextInput), { idempotencyKey: `continuity:second:${marker}` })
  objects.turn_ids.push(second.id)
  const state = await first.session.retrieve()
  const matches = await client.sessions.list({ agentId: state.agentId, key })
  assertEqual(matches.items.length, 1, "Session key lookup was not exact")
  assertEqual(matches.items[0]!.id, first.session.id, "Session key changed identity")
  const byKey = client.sessions.get(matches.items[0]!.id)
  const replay = await byKey.enqueue(fixtureInput(nextInput), { idempotencyKey: `continuity:second:${marker}` })
  assertEqual(replay.id, second.id, "Enqueue replay changed Turn identity")
  const outputs = []
  for (const [turn, expectedMarker, expectedCount] of [[first.turn, `${marker}:first`, 1], [second, `${marker}:second`, 2]] as const) {
    const selected = await waitOutput(first.session, turn, value => value !== null && typeof value === "object" && "marker" in value && value.marker === expectedMarker)
    const result = await completedResult(turn)
    assert(result !== null && typeof result === "object" && "count" in result && "childSessionId" in result && "childTurnId" in result)
    assertEqual(result.count, expectedCount, "Session setup state or FIFO order was lost")
    assertEqual(selected, result, "Public selected output differs from the helper result")
    assert(typeof result.childSessionId === "string" && typeof result.childTurnId === "string")
    objects.session_ids.push(result.childSessionId)
    objects.turn_ids.push(result.childTurnId)
    outputs.push(selected)
  }
  const firstState = await first.turn.retrieve(), secondState = await second.retrieve()
  assert(firstState.sequence < secondState.sequence, "Turn sequence did not preserve FIFO admission")
  const readOutputs = async () => {
    const records = []
    let after = 0
    for (;;) {
      const page = await byKey.events.list({ after, limit: 1 }, { signal: deadline(30_000) })
      assert(page.retainedAfter <= after, "Required Session history expired")
      for (const event of page.records) {
        assert(event.sequence > after, "Session events were not strictly ordered")
        if (event.kind === "turn.output") records.push(event)
      }
      if (!page.hasMore) return records
      assert(page.nextAfter > after, "Session cursor did not advance")
      after = page.nextAfter
    }
  }
  const events = await readOutputs()
  assertEqual(events.map(event => event.turnId), [first.turn.id, second.id], "Paginated output did not retain both Turns in order")
  await byKey.close({ idempotencyKey: `continuity:close:${marker}` })
  const signal = deadline(120_000)
  while ((await byKey.retrieve({ signal })).status !== "closed") await delay(500, undefined, { signal })
  assertEqual(await readOutputs(), events, "Closing the Session discarded its output")
  return { sessionId: first.session.id, turns: [first.turn.id, second.id], outputs, outputSequences: events.map(event => event.sequence) }
})
