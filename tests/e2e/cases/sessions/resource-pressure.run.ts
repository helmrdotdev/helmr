import { setTimeout as delay } from "node:timers/promises"
import { fixtureInput } from "../../support/runtime-mcp"
import { verify, assert, assertEqual, deadline, waitOutput, completedResult, deleteComputer } from "../../support/context"
import { hostObservation } from "../../support/host-observation"

// Run on the dedicated two-slot Worker with one funded prepared leaf. The
// Environment must fund all three requests so the third waits on host capacity.
await verify("session-resource-pressure", async ({ marker, objects, computer, startAgent }) => {
  const resource = process.env.HELMR_WORKER_RESOURCE_ID
  assert(resource, "Exact Worker resource is required")
  const signal = deadline(10 * 60_000)
  let before: any
  for (;;) {
    signal.throwIfAborted()
    const workers = await hostObservation("worker-state", { resource_ids: [resource] })
    assertEqual(workers.length, 1, "Expected one Worker")
    before = workers[0]
    if (Date.now() - Date.parse(before.resource_observed_at) < 60_000 && before.local_optional_vm_slots > 0 && before.ready_leaf_keys?.length > 0) break
    await delay(500, undefined, { signal })
  }
  const held = []
  for (let ordinal = 0; ordinal < 2; ordinal++) {
    const target = await computer(ordinal === 0 ? "helmr-runtime-smoke" : "verification", `held-${ordinal}`)
    const admission = await startAgent("verification-session", { computer: target,
      input: { marker }, idempotencyKey: `session:${marker}:${ordinal}` })
    const first: any = await completedResult(admission.turn)
    const second = await admission.session.enqueue(fixtureInput({ marker }), { idempotencyKey: `hold:${marker}:${ordinal}` }, { signal })
    objects.turn_ids.push(second.id)
    await waitOutput(admission.session, second, value => value !== null && typeof value === "object" && "phase" in value && value.phase === "restored")
    held.push({ target, admission, first, second })
  }
  assert(held[0]!.first.nonce !== held[1]!.first.nonce, "Concurrent Computers shared Session state")
  const queuedComputer = await computer("helmr-runtime-smoke", "queued")
  const requestAt = Date.now()
  const queued = await startAgent("verification-session", { computer: queuedComputer,
    input: { marker }, idempotencyKey: `session:${marker}:queued` })
  let waiting
  for (;;) {
    signal.throwIfAborted()
    waiting = await queuedComputer.get({ signal })
    if (waiting.executionWait?.reason === "scheduling") break
    assert(waiting.executionWait?.reason !== "environment_limit", "Environment limit hides host pressure")
    const turn = await queued.turn.get({ signal })
    assert(!["completed", "failed", "cancelled", "interrupted"].includes(turn.status), "Third request escaped full host capacity")
    await delay(200, undefined, { signal })
  }
  // Preserve two busy Turns while waiting for a fresh physical occupancy report.
  let pressure: any
  for (;;) {
    signal.throwIfAborted()
    pressure = (await hostObservation("worker-state", { resource_ids: [resource] }))[0]
    if (Date.parse(pressure.resource_observed_at) > requestAt && pressure.active_leases === 2 && pressure.local_optional_vm_slots === 0) break
    await delay(500, undefined, { signal })
  }
  for (const member of held) assertEqual((await member.second.get({ signal })).status, "running", "Busy work was preempted by pressure")
  const releasedAt = Date.now()
  const first = held[0]!
  await first.second.send(fixtureInput("finish"), { idempotencyKey: `release:${marker}:0` }, { signal })
  const firstResult: any = await completedResult(first.second)
  assertEqual(firstResult.nonce, first.first.nonce, "Pressure changed the first Session's memory")
  await first.admission.session.cancel({ idempotencyKey: `cancel:${marker}:0` }, { signal })
  await deleteComputer(first.target, `delete:${marker}:0`)
  const queuedResult: any = await completedResult(queued.turn)
  const queuedUsefulOutputMs = Date.now() - requestAt
  const releaseToOutputMs = Date.now() - releasedAt
  const survivor = held[1]!
  assertEqual((await survivor.second.get({ signal })).status, "running", "The other busy Computer was preempted")
  await survivor.second.send(fixtureInput("finish"), { idempotencyKey: `release:${marker}:1` }, { signal })
  const survivorResult: any = await completedResult(survivor.second)
  assertEqual(survivorResult.nonce, survivor.first.nonce, "Pressure changed the surviving Session's memory")
  assertEqual(survivorResult.count, 2, "Pressure restarted authored initialization")
  const paths = []
  for (const target of [first.target, survivor.target, queuedComputer]) {
    paths.push(await hostObservation("computer-path", { computer_id: target.id }))
  }
  for (const member of [survivor, { target: queuedComputer, admission: queued }]) {
    await member.admission.session.cancel({ idempotencyKey: `cancel:${marker}:${member.target.id}` }, { signal })
    await deleteComputer(member.target, `delete:${marker}:${member.target.id}`)
  }
  const burst = await Promise.allSettled(Array.from({ length: 3 }, async (_, ordinal) => {
    const startedAt = Date.now()
    const target = await computer("verification", `burst-${ordinal}`)
    const admission = await startAgent("verification-session", { computer: target,
      input: { marker }, idempotencyKey: `burst:${marker}:${ordinal}` })
    const result = await completedResult(admission.turn)
    const usefulOutputMs = Date.now() - startedAt
    await admission.session.cancel({ idempotencyKey: `cancel-burst:${marker}:${ordinal}` }, { signal })
    await deleteComputer(target, `delete-burst:${marker}:${ordinal}`)
    return { result, usefulOutputMs, computerId: target.id }
  }))
  assert(burst.every(result => result.status === "fulfilled"), `Burst did not finish: ${JSON.stringify(burst)}`)
  const burstPaths = []
  for (const result of burst) {
    if (result.status === "fulfilled") burstPaths.push(await hostObservation("computer-path", { computer_id: result.value.computerId }))
  }
  return { before, pressure, waiting, queuedUsefulOutputMs, releaseToOutputMs, firstResult, survivorResult, queuedResult, paths, burst, burstPaths }
})
