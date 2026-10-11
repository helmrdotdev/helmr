import { fixtureInput } from "../../support/runtime-mcp"
import { verify, assert, assertEqual, waitOutput, completedResult } from "../../support/context"
import { observePersistence, assertRestored } from "../../support/persistence"

await verify("shared-persistence", async ({ marker, objects, computer, startAgent }) => {
  const shared = await computer("verification", "shared-persistence")
  const members = await Promise.all(["a", "b"].map(async suffix => {
    const memberMarker = `${marker}:${suffix}`
    const started = await startAgent("verification-persistence", {
      computer: shared, input: { marker: memberMarker, awaitStart: true }, idempotencyKey: `${memberMarker}:start`,
    })
    return { ...started, marker: memberMarker }
  }))
  await Promise.all(members.map(member => waitOutput(member.session, member.turn,
    value => value !== null && typeof value === "object" && "phase" in value && value.phase === "ready")))
  await Promise.all(members.map(member => member.turn.send(fixtureInput("start"), { idempotencyKey: `${member.marker}:ready` })))
  const initial = await Promise.all(members.map(async started => {
    const memberMarker = started.marker
    const before = await completedResult(started.turn, 180_000)
    assert(before !== null && typeof before === "object" && "nonce" in before && typeof before.nonce === "string")
    assertEqual(before, { marker: memberMarker, nonce: before.nonce, count: 1, turnId: started.turn.id, sessionId: started.session.id, computerId: shared.id }, "Initial member state changed")
    return { ...started, marker: memberMarker, nonce: before.nonce, before }
  }))
  const parked = []
  for (const member of members) parked.push(await observePersistence("wait-parked", member.session.id))
  assertEqual(parked[0]!.checkpoint_id, parked[1]!.checkpoint_id, "Members were not captured in one checkpoint")
  assertEqual(parked[0]!.prior_runtime_id, parked[1]!.prior_runtime_id, "Members did not share a source VM")
  const successors = await Promise.all(members.map(member => member.session.enqueue(fixtureInput({ marker: member.marker, holdForObservation: true }), { idempotencyKey: `${member.marker}:resume` })))
  objects.turn_ids.push(...successors.map(turn => turn.id))
  await Promise.all(successors.map((turn, i) => waitOutput(members[i]!.session, turn,
    value => value !== null && typeof value === "object" && "phase" in value && value.phase === "restored")))
  const restored = []
  for (const member of members) restored.push(await observePersistence("verify-restored", member.session.id))
  await Promise.all(successors.map((turn, i) => turn.send(fixtureInput("finish"), { idempotencyKey: `${members[i]!.marker}:finish` })))
  const outputs = await Promise.all(successors.map(turn => completedResult(turn, 180_000)))
  for (const [i, member] of initial.entries()) {
    assertEqual(outputs[i], { marker: member.marker, nonce: member.nonce, count: 2, turnId: successors[i]!.id, sessionId: member.session.id, computerId: shared.id }, "Restored member lost its original memory or files")
    assertRestored(parked[i]!, restored[i]!, member.session.id)
  }
  assertEqual(restored[0]!.target_runtime_id, restored[1]!.target_runtime_id, "Members restored into different VMs")
  assertEqual(restored[0]!.target_lease_epoch, restored[1]!.target_lease_epoch, "Members restored onto different leases")
  return { computerId: shared.id, before: initial.map(m => m.before), parked, outputs, restored }
})
