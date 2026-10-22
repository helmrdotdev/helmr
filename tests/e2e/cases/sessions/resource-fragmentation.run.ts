import { setTimeout as delay } from "node:timers/promises"
import { fixtureInput } from "../../support/runtime-mcp"
import { verify, assert, assertEqual, deadline, waitOutput, completedResult, deleteComputer } from "../../support/context"
import { hostObservation } from "../../support/host-observation"

// A dedicated three-CPU, two-slot Worker leaves one CPU and one slot after
// admitting the two-CPU Computer. The Environment must fund all three Computers.
await verify("session-resource-fragmentation", async ({ marker, objects, computer, startAgent }) => {
  const resource = process.env.HELMR_WORKER_RESOURCE_ID
  const region = process.env.HELMR_REGION
  assert(resource && region, "Exact Worker resource and region are required")
  const signal = deadline(10 * 60_000)
  const fleet = (await hostObservation("worker-fleet", { region })).filter((row: any) => row.resource_id === resource)
  assertEqual(fleet.length, 1, "Expected one exact Worker")
  const before = fleet[0]
  assertEqual(before.epoch_cpu_millis, 3000, "Case requires three advertised CPUs")
  assertEqual(before.max_vm_slots, 2, "Case requires two physical slots")
  const state = (await hostObservation("worker-state", { resource_ids: [resource] }))[0]
  assertEqual(state.active_leases, 0, "Case requires an idle Worker")
  assertEqual(state.local_optional_vm_slots, 0, "Case requires zero optional VMs")

  const hold = async (template: string, key: string) => {
    const target = await computer(template, key)
    const admission = await startAgent("verification-session", { computer: target,
      input: { marker }, idempotencyKey: `session:${marker}:${key}` })
    const first: any = await completedResult(admission.turn)
    const second = await admission.session.enqueue(fixtureInput({ marker }), { idempotencyKey: `hold:${marker}:${key}` }, { signal })
    objects.turn_ids.push(second.id)
    await waitOutput(admission.session, second, value => value !== null && typeof value === "object" && "phase" in value && value.phase === "restored")
    return { target, admission, first, second }
  }
  const large = await hold("helmr-runtime-smoke", "large")
  const queuedComputer = await computer("helmr-runtime-smoke", "queued")
  const requestedAt = Date.now()
  const queued = await startAgent("verification-session", { computer: queuedComputer,
    input: { marker }, idempotencyKey: `session:${marker}:queued` })
  const waitForFragment = async () => {
    for (;;) {
      signal.throwIfAborted()
      const waiting = await queuedComputer.get({ signal })
      assert(waiting.executionWait?.reason !== "environment_limit", "Environment quota hides host fragmentation")
      const status = (await queued.turn.get({ signal })).status
      assert(!["completed", "failed", "cancelled", "interrupted"].includes(status), "Large request escaped the CPU bound")
      const bins = (await hostObservation("capacity-state", { region, max_age_seconds: 60 }))
        .filter((bin: any) => bin.worker_host_id === before.id)
      if (waiting.executionWait?.reason === "scheduling" && bins.length === 1 &&
          bins[0].available_cpu_millis === 1000 && bins[0].available_vm_slots === 1) {
        const bin = bins[0]
        assertEqual(bin.worker_epoch, before.current_epoch, "Worker ownership changed")
        assert(bin.available_memory_bytes >= bin.per_vm_memory_bytes, "Memory shortage hides CPU fragmentation")
        assert(bin.available_guest_ephemeral_disk_bytes >= bin.per_vm_guest_ephemeral_disk_bytes, "Disk shortage hides CPU fragmentation")
        assert(bin.available_instance_starts > 0 && !bin.admission_paused_reason, "Admission restriction hides CPU fragmentation")
        assert(bin.cpu_shape_vcpu_counts.includes(1) && bin.cpu_shape_vcpu_counts.includes(2), "Worker lacks required CPU shapes")
        return { waiting, bin }
      }
      await delay(250, undefined, { signal })
    }
  }
  const fragmented = await waitForFragment()
  assertEqual((await large.second.get({ signal })).status, "running", "Large busy Computer was preempted")
  const small = await hold("verification", "small")
  assert(small.first.nonce !== large.first.nonce, "Concurrent Computers shared Session memory")
  const occupied = (await hostObservation("worker-state", { resource_ids: [resource] }))[0]
  assertEqual(occupied.active_leases, 2, "Small request did not use the spare slot")
  assertEqual((await queuedComputer.get({ signal })).executionWait?.reason, "scheduling", "Large request did not remain queued")
  await small.second.send(fixtureInput("finish"), { idempotencyKey: `release:${marker}:small` }, { signal })
  const smallResult: any = await completedResult(small.second)
  assertEqual(smallResult.nonce, small.first.nonce, "Small Session lost its memory")
  await small.admission.session.cancel({ idempotencyKey: `cancel:${marker}:small` }, { signal })
  await deleteComputer(small.target, `delete:${marker}:small`)
  const afterSmallRelease = await waitForFragment()
  assertEqual((await large.second.get({ signal })).status, "running", "Busy Computer was preempted to satisfy queued work")
  const releasedAt = Date.now()
  await large.second.send(fixtureInput("finish"), { idempotencyKey: `release:${marker}:large` }, { signal })
  const largeResult: any = await completedResult(large.second)
  assertEqual(largeResult.nonce, large.first.nonce, "Large Session lost its memory")
  assertEqual(largeResult.count, 2, "Pressure restarted authored initialization")
  await large.admission.session.cancel({ idempotencyKey: `cancel:${marker}:large` }, { signal })
  await deleteComputer(large.target, `delete:${marker}:large`)
  const queuedResult = await completedResult(queued.turn)
  const usefulAt = Date.now()
  const paths = []
  for (const target of [large.target, small.target, queuedComputer]) {
    const path = await hostObservation("computer-path", { computer_id: target.id })
    assert(path.leases.length > 0 && path.leases.every((lease: any) => lease.worker_host_id === before.id && lease.worker_epoch === before.current_epoch), "Work ran on another Worker")
    paths.push({ computerId: target.id, path })
  }
  return { before, fragmented, occupied, afterSmallRelease, largeResult, smallResult, queuedResult, paths,
    queuedUsefulOutputMs: usefulAt - requestedAt, releaseToOutputMs: usefulAt - releasedAt }
})
