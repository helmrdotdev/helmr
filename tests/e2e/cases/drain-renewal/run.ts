import { execFile } from "node:child_process"
import { promisify } from "node:util"
import { writeFile } from "node:fs/promises"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import { verify, assert, deadline, readTelemetry } from "../../support/context"
import { hostObservation } from "../../support/host-observation"
import type { renewalTask } from "./task"

const service = async (property: string, unit = "helmr-worker.service") => (await promisify(execFile)("systemctl",
  ["show", unit, "-p", property, "--value"], { timeout: 10_000 })).stdout.trim()

// After ready-for-drain.json the operator requests native planned drain. Leave
// Worker running through this case; even an expired CLI wait never cancels it.
await verify("drain-renewal", async ({ client, marker, computer, objects }) => {
  const shared = await computer("verification-drain-renewal")
  const run = await client.tasks.start<typeof renewalTask>("verification-drain-renewal", {
    computer: shared, payload: { marker }, idempotencyKey: `${marker}:run`,
  })
  objects.run_ids.push(run.id)
  const startedDeadline = deadline(180_000)
  let nonce: string | undefined
  while (!nonce) {
    startedDeadline.throwIfAborted()
    const logs = await readTelemetry(() => client.runs.logs(run.id, { limit: 100 }, { signal: startedDeadline }))
    const entries = logs.items.filter(item => item.kind === "structured" && item.attributes.marker === marker &&
      item.message === "drain renewal probe started")
    assert(entries.length <= 1, "probe entered more than once")
    const entry = entries[0]
    if (entry?.kind === "structured") {
      assert.equal(typeof entry.attributes.nonce, "string")
      nonce = entry.attributes.nonce as string
    }
    if (!nonce) await delay(500)
  }
  const before = await hostObservation("run-path", { run_id: run.id })
  assert.equal(before.leases.length, 1)
  const source = before.leases[0]
  assert.equal(source.status, "running")
  const fleet = await hostObservation("worker-fleet", { region: "default" })
  const host = fleet.find((h: any) => h.id === source.worker_host_id)
  assert(host, "Run Host missing from fleet")
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
    JSON.stringify({ runId: run.id, computerId: shared.id, nonce, before, host, invocation, dispatcher, controlPlane }), { mode: 0o600, flag: "wx" })
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
    const path = await hostObservation("run-path", { run_id: run.id })
    assert.equal(path.leases.length, 1)
    assert.equal(path.leases[0].id, source.id)
    assert.equal(path.leases[0].status, "running")
    assert.equal(path.leases[0].computer_instance_id, source.computer_instance_id)
    assert.equal(path.leases[0].reclaimed_at, null)
    assert.equal(path.checkpoints.length, 0, "resident Task was captured during drain")
    const elapsed = drainingAt === undefined ? 0 : performance.now() - drainingAt
    samples.push({ elapsed, host: current, lease: path.leases[0] })
    if (elapsed >= 31 * 60_000) break
    await delay(15_000)
  }
  const output = await client.runs.wait(run, { signal: deadline(6 * 60_000) }).unwrap()
  assert.deepEqual(output, { marker, nonce, runId: run.id, attemptNumber: 1 })
  const after = await hostObservation("run-path", { run_id: run.id })
  assert.equal(after.leases.length, 1)
  assert.equal(after.leases[0].id, source.id)
  assert.equal(after.leases[0].computer_instance_id, source.computer_instance_id)
  assert.equal(await service("InvocationID"), invocation)
  assert.equal(await service("ActiveState"), "active")
  await dependencies()
  return { before, samples, after, output, invocation, dispatcher, controlPlane }
})
