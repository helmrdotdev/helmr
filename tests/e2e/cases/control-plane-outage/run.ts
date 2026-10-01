import { execFile } from "node:child_process"
import { promisify } from "node:util"
import { writeFile } from "node:fs/promises"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import { verify, assert, deadline, readTelemetry } from "../../support/context"
import { hostObservation } from "../../support/host-observation"
import type { outageTask } from "./task"

async function service(name: string, property: string) {
  return (await promisify(execFile)("systemctl", ["show", `helmr-verification-${name}.service`,
    "-p", property, "--value"], { timeout: 10_000 })).stdout.trim()
}

// The operator stops only CP after ready-for-outage.json, holds it for at least
// 150 seconds, and starts it again. Dispatcher and Worker must remain running.
await verify("control-plane-outage", async ({ client, marker, computer, objects }) => {
  const shared = await computer("verification-outage")
  const run = await client.tasks.start<typeof outageTask>("verification-outage", {
    computer: shared, payload: { marker }, idempotencyKey: `${marker}:run`,
  })
  objects.run_ids.push(run.id)
  const startedDeadline = deadline(180_000)
  let nonce: string | undefined
  while (!nonce) {
    startedDeadline.throwIfAborted()
    const logs = await readTelemetry(() => client.runs.logs(run.id, { limit: 100 }))
    const entries = logs.items.filter(item => item.kind === "structured" && item.attributes.marker === marker)
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
  const fleet = await hostObservation("worker-fleet", { region: "default" })
  const host = fleet.find((h: any) => h.id === source.worker_host_id)
  assert(host, "Run Host missing from fleet")
  const dispatcher = await service("dispatcher", "InvocationID")
  assert(dispatcher)
  await writeFile(join(process.env.HELMR_EVIDENCE_DIR!, "ready-for-outage.json"),
    JSON.stringify({ runId: run.id, computerId: shared.id, nonce, before, host, dispatcher }), { mode: 0o600, flag: "wx" })
  const outageDeadline = deadline(300_000)
  let stoppedAt: number | undefined
  let recovered = false
  const samples: unknown[] = []
  while (!recovered) {
    outageDeadline.throwIfAborted()
    assert.equal(await service("dispatcher", "ActiveState"), "active")
    assert.equal(await service("dispatcher", "InvocationID"), dispatcher)
    const cp = await service("control-plane", "ActiveState")
    if (cp === "inactive") {
      stoppedAt ??= performance.now()
      const observed = await hostObservation("worker-state", { resource_ids: [host.resource_id] })
      assert.equal(observed.length, 1)
      assert.equal(observed[0].id, source.worker_host_id)
      assert.equal(observed[0].status, "active", "API outage fenced a live Host")
      samples.push(observed[0])
    } else if (stoppedAt !== undefined && cp === "active") {
      assert(performance.now() - stoppedAt >= 145_000, "API outage was too short")
      recovered = true
    }
    if (!recovered) await delay(2000)
  }
  assert(samples.length > 0)
  const readinessDeadline = deadline(60_000)
  for (;;) {
    readinessDeadline.throwIfAborted()
    try {
      const response = await fetch("http://127.0.0.1:58080/readyz", { signal: deadline(2000) })
      if (response.ok) break
    } catch (error) {
      if (!(error instanceof Error)) throw error
    }
    await delay(500)
  }
  const readyAt = performance.now()
  const readyWallSeconds = Date.now() / 1000
  assert.equal(await service("dispatcher", "InvocationID"), dispatcher)
  assert.equal(await service("dispatcher", "ActiveState"), "active")
  const output = await client.runs.wait(run, { signal: deadline(300_000) }).unwrap()
  assert.deepEqual(output, { marker, nonce, runId: run.id, attemptNumber: 1 })
  const after = await hostObservation("run-path", { run_id: run.id })
  assert.equal(after.leases.length, 1)
  assert.equal(after.leases[0].id, source.id)
  assert.equal(after.leases[0].computer_instance_id, source.computer_instance_id)
  assert.equal(after.leases[0].worker_host_id, source.worker_host_id)
  assert.equal(await service("dispatcher", "InvocationID"), dispatcher)
  assert.equal(await service("dispatcher", "ActiveState"), "active")
  let recoveredHost: any
  do {
    assert.equal(await service("dispatcher", "InvocationID"), dispatcher)
    assert.equal(await service("dispatcher", "ActiveState"), "active")
    recoveredHost = await hostObservation("worker-state", { resource_ids: [host.resource_id] })
    assert.equal(recoveredHost.length, 1)
    assert.equal(recoveredHost[0].id, source.worker_host_id)
    assert.equal(recoveredHost[0].status, "active")
    assert.equal(recoveredHost[0].current_epoch, source.worker_epoch)
    if (performance.now() - readyAt >= 130_000) break
    await delay(2000)
  } while (true)
  assert(recoveredHost[0].observed_epoch > readyWallSeconds, "no Worker observation after readiness returned")
  assert(Date.now() / 1000 - recoveredHost[0].observed_epoch < 60,
    "Worker did not resume fresh observations after recovery")
  return { before, samples, after, output, dispatcher, recoveredHost }
})
