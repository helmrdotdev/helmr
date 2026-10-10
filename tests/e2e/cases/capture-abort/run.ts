import { fixtureInput } from "../../support/runtime-mcp"
import { setTimeout as delay } from "node:timers/promises"
import { verify, assert, assertEqual, deadline, waitTurn, waitOutput, completedResult } from "../../support/context"
import { hostObservation } from "../../support/host-observation"
import { observePersistence, assertRestored } from "../../support/persistence"
import { replyFault, assertReplyLosses } from "../../support/reply-fault"

await verify("capture-abort", async ({ marker, objects, computer, cleanup, startAgent }) => {
  assertEqual(process.env.HELMR_API_URL?.replace(/\/$/, ""), "http://127.0.0.1:58080", "Run on the dedicated host")
  const ref = await computer("capture-abort-verification", "capture-abort")
  const members = await Promise.all([false, true].map(cancelMember => startAgent("verification-capture-abort", {
    computer: ref, input: { marker: cancelMember ? `${marker}:cancelled` : marker,
      cancelledMarker: `/workspace/cancelled-capture-${marker}`, cancelMember },
    idempotencyKey: `${marker}:${cancelMember}:start`,
  })))
  const healthy = members[0]!, cancelled = members[1]!
  const ready = await Promise.all(members.map(member => waitOutput(member.session, member.turn,
    value => value !== null && typeof value === "object" && "phase" in value && value.phase === "ready")))
  const before = ready[0]!
  assert(before !== null && typeof before === "object" && "nonce" in before && typeof before.nonce === "string")
  const placements = await hostObservation("session-placements", { session_ids: members.map(m => m.session.id) })
  assert.equal(placements.length, 2)
  const source = placements[0]
  for (const placement of placements) {
    assert.equal(placement.computer_id, ref.id)
    assert.equal(placement.computer_instance_id, source.computer_instance_id)
    assert.equal(placement.computer_lease_epoch, source.computer_lease_epoch)
  }
  await replyFault("POST", { mode: "drop", computer_id: ref.id, instance_id: source.computer_instance_id,
    writer_generation: source.computer_lease_epoch,
    members: placements.map((p: any) => ({ session_id: p.session_id, process_epoch: p.process_epoch })) })
  cleanup(async () => { await replyFault("DELETE") })
  await Promise.all(members.map(member => member.turn.send(fixtureInput("start"), { idempotencyKey: `${marker}:${member.session.id}:start` })))
  const initial = await Promise.all(members.map(member => completedResult(member.turn, 180_000)))
  const cancelledInitial = initial[1]
  assert(cancelledInitial !== null && typeof cancelledInitial === "object" && "count" in cancelledInitial)
  assert.equal(cancelledInitial.count, 1)
  const cancelledCounter = cancelledInitial.count
  assert(typeof cancelledCounter === "number")
  const heldDeadline = deadline(180_000)
  let held
  for (;;) {
    heldDeadline.throwIfAborted()
    held = await replyFault()
    assert.equal(held.failure, "")
    if (held.events?.some(e => e.kind === "guest-capture" && e.dropped)) break
    await delay(200, undefined, { signal: heldDeadline })
  }
  assert.equal(held.released, false)
  const captureEvent = held.events!.find(e => e.kind === "guest-capture" && e.dropped)!
  const captureExpiry = Date.parse(captureEvent.expires_at ?? "")
  assert(Number.isFinite(captureExpiry) && captureExpiry > Date.parse(captureEvent.at), "Capture authority deadline missing or already expired")
  const expiryWait = Math.max(0, captureExpiry + 1_000 - Date.now())
  assert(expiryWait <= 60_000, "Capture deadline exceeds bounded expiry check")
  await delay(expiryWait, undefined, { signal: deadline(65_000) })
  const renewed = await hostObservation("session-placements", { session_ids: members.map(m => m.session.id) })
  assert.equal(renewed.length, 2, "Current CP authority stopped renewing while old capture authority expired")
  for (const placement of renewed) {
    assert.equal(placement.computer_instance_id, source.computer_instance_id)
    assert.equal(placement.computer_lease_epoch, source.computer_lease_epoch)
  }
  const input = (cancelMember: boolean) => ({ marker: cancelMember ? `${marker}:cancelled` : marker,
    cancelledMarker: `/workspace/cancelled-capture-${marker}`, cancelMember, cancelledCounter })
  const cancelledQueued = await cancelled.session.enqueue(fixtureInput(input(true)), { idempotencyKey: `${marker}:cancelled-queued` })
  objects.turn_ids.push(cancelledQueued.id)
  await cancelled.session.cancel({ idempotencyKey: `${marker}:cancel` })
  const cancellationConfirmedAt = Date.now()
  const continued = await healthy.session.enqueue(fixtureInput(input(false)), { idempotencyKey: `${marker}:continue` })
  objects.turn_ids.push(continued.id)
  const continuationAdmittedAt = Date.now()
  const stillHeld = await replyFault()
  assert.equal(stillHeld.released, false)
  assert.equal(stillHeld.failure, "")
  await replyFault("PATCH")
  const inspectionReleasedAt = Date.now()
  const settled = deadline(180_000)
  let replies
  for (;;) {
    settled.throwIfAborted()
    replies = await replyFault()
    assert.equal(replies.failure, "")
    if (replies.relay?.restored) break
    await delay(200, undefined, { signal: settled })
  }
  assertReplyLosses(replies, healthy.session.id, cancelled.session.id)
  const aborted = await observePersistence("wait-aborted", healthy.session.id)
  assert.equal(aborted.checkpoint_id, replies.target?.checkpoint_id)
  assert.equal(aborted.source_instance_id, source.computer_instance_id)
  assert.deepEqual([...(aborted.captured_session_ids as string[])].sort(), members.map(m => m.session.id).sort())
  const cancelledTurn = await waitTurn(cancelledQueued, ["cancelled"], 120_000)
  const resumed = await waitOutput(healthy.session, continued,
    value => value !== null && typeof value === "object" && "phase" in value && value.phase === "resumed")
  assert(resumed !== null && typeof resumed === "object" && "nonce" in resumed)
  assert.equal(resumed.nonce, before.nonce, "Abort lost in-memory state")
  assert.equal((resumed as Record<string, unknown>).count, 2)
  await continued.send(fixtureInput("finish"), { idempotencyKey: `${marker}:finish-abort` })
  const continuedResult = await completedResult(continued, 180_000)
  assert.deepEqual(continuedResult, { marker, nonce: before.nonce, count: 2, turnId: continued.id, sessionId: healthy.session.id, computerId: ref.id })
  const parked = await observePersistence("wait-parked", healthy.session.id)
  assert.equal(parked.prior_runtime_id, aborted.source_instance_id)
  assert.notEqual(parked.checkpoint_id, aborted.checkpoint_id, "Aborted checkpoint became restorable")
  const final = await healthy.session.enqueue(fixtureInput(input(false)), { idempotencyKey: `${marker}:restore` })
  objects.turn_ids.push(final.id)
  await waitOutput(healthy.session, final, value => value !== null && typeof value === "object" && "phase" in value && value.phase === "restored")
  const restored = await observePersistence("verify-restored", healthy.session.id)
  assertRestored(parked, restored, healthy.session.id)
  await final.send(fixtureInput("finish"), { idempotencyKey: `${marker}:finish-restore` })
  const output = await completedResult(final, 180_000)
  assert.deepEqual(output, { marker, nonce: before.nonce, count: 3, turnId: final.id, sessionId: healthy.session.id, computerId: ref.id })
  return { initial, continuedResult, placements, renewed, captureExpiry, held, replies, cancelledCounter, cancellationConfirmedAt, continuationAdmittedAt, inspectionReleasedAt, aborted, cancelledTurn, parked, restored, output }
})
