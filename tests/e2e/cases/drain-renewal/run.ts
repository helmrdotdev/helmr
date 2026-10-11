import { execFile } from "node:child_process"
import { promisify } from "node:util"
import { writeFile } from "node:fs/promises"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import { verify, assert, deadline, waitOutput, completedResult } from "../../support/context"
import { hostObservation } from "../../support/host-observation"

const service = async (property: string, unit = "helmr-worker.service") => (await promisify(execFile)("systemctl",
  ["show", unit, "-p", property, "--value"], { timeout: 10_000 })).stdout.trim()

// After ready-for-drain.json the operator requests native planned drain. Leave
// Worker running through this case; even an expired CLI wait never cancels it.
await verify("drain-renewal", async ({ marker, computer, startAgent }) => {
  const shared = await computer("verification-drain-renewal")
  const started = await startAgent("verification-drain-renewal", {
    computer: shared, input: { marker }, idempotencyKey: `${marker}:start`,
  })
  const outputBefore = await waitOutput(started.session, started.turn, value => value !== null && typeof value === "object" && "phase" in value && value.phase === "started")
  assert(outputBefore !== null && typeof outputBefore === "object" && "nonce" in outputBefore && typeof outputBefore.nonce === "string")
  const nonce = outputBefore.nonce
  const before = await hostObservation("computer-path", { computer_id: shared.id })
  assert.equal(before.leases.length, 1)
  const source = before.leases[0]
  assert.equal(source.status, "active")
  const fleet = await hostObservation("worker-fleet", { region: "default" })
  const host = fleet.find((h: any) => h.id === source.worker_host_id)
  assert(host, "Computer Host missing from fleet")
  const invocation = await service("InvocationID")
  assert(invocation)
  const dispatcher = await service("InvocationID", "helmr-verification-dispatcher.service")
  const controlPlane = await service("InvocationID", "helmr-verification-control-plane.service")
  assert(dispatcher && controlPlane)
  const dependencies = async () => {
    for (const [unit, expected] of [["helmr-verification-dispatcher.service", dispatcher],
      ["helmr-verification-control-plane.service", controlPlane]]) {
      assert.equal(await service("ActiveState", unit), "active")
      assert.equal(await service("InvocationID", unit), expected)
    }
  }
  await dependencies()
  await writeFile(join(process.env.HELMR_EVIDENCE_DIR!, "ready-for-drain.json"),
    JSON.stringify({ turnId: started.turn.id, sessionId: started.session.id, computerId: shared.id, nonce, before, host, invocation, dispatcher, controlPlane }), { mode: 0o600, flag: "wx" })
  let drainingAt: number | undefined
  const startDrainDeadline = deadline(60_000)
  const samples: unknown[] = []
  while (true) {
    await dependencies()
    assert.equal(await service("ActiveState"), "active")
    assert.equal(await service("InvocationID"), invocation)
    const values = await hostObservation("worker-state", { resource_ids: [host.resource_id] })
    assert.equal(values.length, 1)
    const current = values[0]
    assert.equal(current.id, host.id)
    assert.equal(current.current_epoch, source.worker_epoch)
    assert(Date.now() / 1000 - current.observed_epoch < 60, "Worker observation is stale")
    if (drainingAt === undefined) {
      startDrainDeadline.throwIfAborted()
      assert(["active", "draining"].includes(current.status))
      if (current.status === "draining") drainingAt = performance.now()
    } else {
      assert.equal(current.status, "draining")
    }
    const path = await hostObservation("computer-path", { computer_id: shared.id })
    assert.equal(path.leases.length, 1)
    assert.equal(path.leases[0].epoch, source.epoch)
    assert.equal(path.leases[0].status, "active")
    assert.equal(path.leases[0].computer_instance_id, source.computer_instance_id)
    assert.equal(path.leases[0].fenced_at, null)
    assert.equal(path.checkpoints.length, 0, "active Turn was captured during drain")
    const elapsed = drainingAt === undefined ? 0 : performance.now() - drainingAt
    samples.push({ elapsed, host: current, lease: path.leases[0] })
    if (elapsed >= 31 * 60_000) break
    await delay(15_000)
  }
  const output = await completedResult(started.turn, 6 * 60_000)
  assert.deepEqual(output, { marker, nonce, turnId: started.turn.id, sessionId: started.session.id })
  const after = await hostObservation("computer-path", { computer_id: shared.id })
  assert.equal(after.leases.length, 1)
  assert.equal(after.leases[0].epoch, source.epoch)
  assert.equal(after.leases[0].computer_instance_id, source.computer_instance_id)
  assert.equal(await service("InvocationID"), invocation)
  assert.equal(await service("ActiveState"), "active")
  await dependencies()
  return { before, samples, after, output, invocation, dispatcher, controlPlane }
})
