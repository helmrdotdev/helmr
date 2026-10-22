import { readFile, rename, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import { fixtureInput } from "../../support/runtime-mcp"
import { verify, assert, assertEqual, deadline, waitOutput, completedResult } from "../../support/context"
import { observePersistence, assertRestored } from "../../support/persistence"
import { hostObservation } from "../../support/host-observation"
import { assertSessionTurns } from "./assertions"

// The operator restarts this exact Worker only after ready-for-worker-restart.json.
// The case resumes through public APIs once a fresh later Worker epoch is ready.
await verify("session-worker-restart", async ({ marker, objects, computer, startAgent }) => {
  const resource = process.env.HELMR_WORKER_RESOURCE_ID
  assert(resource && process.env.HELMR_EVIDENCE_DIR, "Worker and evidence directory are required")
  const target = await computer("verification", "restart")
  const first = await startAgent("verification-session", { computer: target,
    input: { marker }, idempotencyKey: `session:${marker}` })
  const firstResult: any = await completedResult(first.turn, 180_000)
  const parked = await observePersistence("wait-parked", first.session.id)
  const readySignal = deadline(5 * 60_000)
  let before: any
  for (;;) {
    readySignal.throwIfAborted()
    const workers = await hostObservation("worker-state", { resource_ids: [resource] })
    assertEqual(workers.length, 1, "Expected one exact Worker")
    before = workers[0]
    if (before.status === "active" && before.active_leases === 0 && before.local_optional_vm_slots > 0 &&
        before.ready_leaf_keys?.length > 0 && Date.now() - Date.parse(before.resource_observed_at) < 60_000) break
    await delay(500, undefined, { signal: readySignal })
  }
  await writeFile(join(process.env.HELMR_EVIDENCE_DIR, "ready-for-worker-restart.json.tmp"),
    JSON.stringify({ before, parked, sessionId: first.session.id, computerId: target.id }) + "\n", { mode: 0o600 })
  await rename(join(process.env.HELMR_EVIDENCE_DIR, "ready-for-worker-restart.json.tmp"),
    join(process.env.HELMR_EVIDENCE_DIR, "ready-for-worker-restart.json"))
  const signal = deadline(5 * 60_000)
  // The native host tool owns an exclusive profile lock during fault injection.
  // Wait for its atomic completion receipt before polling native observations.
  for (;;) {
    signal.throwIfAborted()
    try {
      const receipt = JSON.parse(await readFile(join(process.env.HELMR_EVIDENCE_DIR, "worker-restart-complete.json"), "utf8"))
      assertEqual(receipt.workerHostId, before.id, "Operator restarted another Worker")
      assertEqual(receipt.previousEpoch, before.current_epoch, "Operator used another Worker epoch")
      assertEqual(receipt.passed, true, "Operator crash/recovery failed")
      break
    } catch (error) {
      if (!(error instanceof Error && "code" in error && error.code === "ENOENT")) throw error
    }
    await delay(500, undefined, { signal })
  }
  let after: any
  for (;;) {
    signal.throwIfAborted()
    const workers = await hostObservation("worker-state", { resource_ids: [resource] })
    assertEqual(workers.length, 1, "Restart changed Worker resource identity")
    after = workers[0]
    if (after.id === before.id && after.current_epoch > before.current_epoch && after.status === "active" &&
        Date.now() - Date.parse(after.resource_observed_at) < 60_000) break
    await delay(500, undefined, { signal })
  }
  const second = await first.session.enqueue(fixtureInput({ marker }), { idempotencyKey: `second:${marker}` }, { signal })
  objects.turn_ids.push(second.id)
  await waitOutput(first.session, second, value => value !== null && typeof value === "object" && "phase" in value && value.phase === "restored")
  const restored = await observePersistence("verify-restored", first.session.id)
  assertRestored(parked, restored, first.session.id)
  await second.send(fixtureInput("finish"), { idempotencyKey: `finish:${marker}` }, { signal })
  const secondResult = await completedResult(second, 180_000)
  assertSessionTurns({ marker, nonce: firstResult.nonce, sessionId: first.session.id, computerId: target.id }, firstResult, secondResult)
  return { before, after, parked, restored, turns: [firstResult, secondResult] }
})
