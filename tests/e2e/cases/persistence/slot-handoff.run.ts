import { execFile } from "node:child_process"
import { promisify } from "node:util"
import { setTimeout as delay } from "node:timers/promises"
import { assert, assertEqual, completedResult, deadline, verify } from "../../support/context"
import { capacityAwareStartAgent } from "../../support/capacity-admission"
import { hostObservation } from "../../support/host-observation"
import { fixtureInput } from "../../support/runtime-mcp"

// Run with a single one-slot Worker and a policy that permits retained sources.
await verify("checkpoint-slot-handoff", async ({ computer, startAgent, marker, objects }) => {
  const admit = capacityAwareStartAgent(startAgent)
  const hosts = (await hostObservation("worker-fleet", { region: "default" })).filter((h: any) => h.status === "active")
  assert.equal(hosts.length, 1)
  assert.equal(hosts[0].max_vm_slots, 1)
  const path = (id: string) => hostObservation("computer-path", { computer_id: id })
  async function waitRetained(id: string) {
    const signal = deadline(180_000)
    for (;;) {
      signal.throwIfAborted()
      const state = await path(id)
      const checkpoint = state.checkpoints.find((c: any) => c.status === "ready")
      if (checkpoint) {
        const source = state.leases.find((l: any) => l.epoch === checkpoint.source_lease_epoch)
        assert.equal(source.worker_host_id, hosts[0].id)
        assert.equal(source.fenced_at, null, "source already released before competing work arrived")
        const { stdout } = await promisify(execFile)("sudo", ["-n", "journalctl", "-u", "helmr-worker",
          "--no-pager", "-o", "cat", "--since", `@${Math.floor(Date.parse(checkpoint.ready_at) / 1000) - 1}`], { timeout: 15_000, maxBuffer: 16 * 1024 * 1024 })
        if (stdout.split("\n").some(line => ["Computer checkpoint source retained", id, source.computer_instance_id, checkpoint.id].every(value => line.includes(value)))) {
          return { checkpointId: checkpoint.id, saveId: checkpoint.disk_save_id, sourceEpoch: source.epoch, sourceAllocation: source.computer_instance_id }
        }
      }
      await delay(250, undefined, { signal })
    }
  }
  const a = await computer("verification", "handoff-a")
  const first = await admit("verification-persistence", { computer: a, input: { marker }, idempotencyKey: marker + ":a" })
  const before = await completedResult(first.turn, 180_000)
  assert(before !== null && typeof before === "object" && "nonce" in before && typeof before.nonce === "string")
  const parked = await waitRetained(a.id)

  const b = await computer("verification", "handoff-b")
  const second = await admit("verification-persistence", { computer: b, input: { marker: b.id }, idempotencyKey: marker + ":b" })
  const bResult = await completedResult(second.turn, 180_000)
  assert(bResult !== null && typeof bResult === "object" && "computerId" in bResult && bResult.computerId === b.id)
  const parkedB = await waitRetained(b.id)

  const resumed = await first.session.enqueue(fixtureInput({ marker }), { idempotencyKey: `${marker}:resume-a` })
  objects.turn_ids.push(resumed.id)
  const after = await completedResult(resumed, 180_000)
  assertEqual(after, { marker, nonce: before.nonce, count: 2, turnId: resumed.id, sessionId: first.session.id, computerId: a.id }, "A lost state")
  const state = await path(a.id)
  const restored = state.checkpoints.find((c: any) => c.id === parked.checkpointId)
  assert.equal(restored.status, "consumed")
  const target = state.leases.find((l: any) => l.epoch === restored.target_lease_epoch)
  assert.equal(target.worker_host_id, hosts[0].id)
  assert.equal(target.restored_from_save_id, parked.saveId)
  assert(target.epoch > parked.sourceEpoch)
  const bState = await path(b.id)
  const bLease = bState.leases.find((l: any) => l.epoch === parkedB.sourceEpoch)
  const aLease = state.leases.find((l: any) => l.epoch === parked.sourceEpoch)
  assert(aLease.fenced_at && bLease.delivered_at && Date.parse(aLease.fenced_at) <= Date.parse(bLease.delivered_at))
  assert(bLease.fenced_at && target.delivered_at && Date.parse(bLease.fenced_at) <= Date.parse(target.delivered_at))
  return { before, bResult, after, parked, parkedB, aLease, bLease, target, host: hosts[0].id }
})
