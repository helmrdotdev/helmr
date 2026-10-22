import { execFile } from "node:child_process"
import { promisify } from "node:util"
import { createHash, randomBytes } from "node:crypto"
import { setTimeout as delay } from "node:timers/promises"
import { verify, assert, assertEqual, deadline, completedResult, deleteComputer } from "../../support/context"
import { hostObservation } from "../../support/host-observation"

// The operator selects policy through Capacity HTTP before running this case.
// Warm requires a fresh ready observation; cold/zero require no prepared adoption.
await verify("prepared-startup", async ({ client, marker, cleanup, objects, startAgent }) => {
  const mode = process.env.HELMR_EXPECT_PREPARED
  const resource = process.env.HELMR_WORKER_RESOURCE_ID
  const environment = process.env.HELMR_ENVIRONMENT_ID
  assert(mode === "warm" || mode === "cold" || mode === "zero", "Select warm, cold or zero")
  assert(resource && environment, "Exact Worker resource and Environment IDs are required")
  const samples: unknown[] = []
  const nonces = new Set<string>()
  let minimumObservation = 0
  for (let ordinal = 0; ordinal < 2; ordinal++) {
    const signal = deadline(15 * 60_000)
    let before: any
    for (;;) {
      signal.throwIfAborted()
      const rows = await hostObservation("worker-state", { resource_ids: [resource] })
      assertEqual(rows.length, 1, "Expected one current Worker")
      before = rows[0]
      const observedAt = Date.parse(before.resource_observed_at)
      const fresh = observedAt > minimumObservation && Date.now() - observedAt < 60_000
      const ready = before.ready_leaf_keys?.some((key: any) => key.environment_id === environment)
      if (fresh && (mode !== "warm" || ready)) break
      await delay(500, undefined, { signal })
    }
    if (mode === "zero") {
      assertEqual(before.local_optional_vm_slots, 0, "Zero policy retained optional VMs")
      assertEqual(before.optional_disk_bytes, 0, "Zero policy retained optional files")
    }
    // Create the secret only after preparation was observed; it cannot have
    // existed in the prepared VM. Different Computers receive different values.
    const value = randomBytes(32).toString("hex")
    const secret = await client.secrets.create({ secret: `prepared-${marker}-${ordinal}`, value,
      idempotencyKey: `secret:${marker}:${ordinal}` }, { signal })
    objects.secret_ids.push(secret.secret)
    cleanup(() => client.secrets.ref(secret.secret).revoke({ idempotencyKey: `revoke:${marker}:${ordinal}` }))
    const requestAt = Date.now()
    const computer = await client.computers.create({ template: "helmr-runtime-smoke", key: `${marker}-${ordinal}`,
      idempotencyKey: `create:${marker}:${ordinal}`,
      secrets: [{ secret: secret.secret, env: { name: "VERIFICATION_SECRET", mode: "raw" } }] }, { signal })
    objects.computer_ids.push(computer.id)
    cleanup(() => deleteComputer(computer, `delete:${marker}:${ordinal}`))
    const admission = await startAgent("prepared-startup", { computer,
      input: createHash("sha256").update(value).digest("hex"), idempotencyKey: `session:${marker}:${ordinal}` })
    const result: any = await completedResult(admission.turn)
    const usefulOutputMs = Date.now() - requestAt
    assertEqual(result.count, 1, "Session initialized with another Session's state")
    assert(typeof result.nonce === "string" && result.nonce.length > 0, "Initialization nonce is missing")
    assert(!nonces.has(result.nonce), "Prepared clones shared authored initialization")
    nonces.add(result.nonce)
    const command = await computer.exec({ command: ["sh", "-ceu",
      'test "$(cat /workspace/prepared-private)" = "$VALUE"'],
      env: { VALUE: result.nonce }, idempotencyKey: `probe:${marker}:${ordinal}` }, { signal })
    const outcome = await command.wait({ signal })
    assert.equal(outcome.kind, "exited", "Computer probe failed")
    assertEqual(outcome.exitCode, 0, "A clone shared another Computer's private writes")
    const path = await hostObservation("computer-path", { computer_id: computer.id })
    const commandPath = path.commands.find((entry: any) => entry.id === command.id)
    assert(commandPath, "Startup Command is missing from the exact Computer")
    const lease = path.leases.find((entry: any) => entry.epoch === commandPath.computer_lease_epoch)
    assert(lease, "Startup Command has no observed lease")
    assertEqual(lease.worker_host_id, before.id, "Computer ran on a different Worker")
    assertEqual(lease.worker_epoch, before.current_epoch, "Worker ownership changed during startup")
    const { stdout } = await promisify(execFile)("sudo", ["-n", "journalctl", "-u", "helmr-worker",
      "--since", `@${Math.floor(requestAt / 1000)}`, "--no-pager", "-o", "cat"], { timeout: 15_000, maxBuffer: 16 * 1024 * 1024 })
    const phases = stdout.split("\n").filter(line => line.includes(computer.id) && line.includes(lease.computer_instance_id) && line.includes("duration_ms"))
    assert(phases.some(line => line.includes("agent_ready")), "Actual startup readiness phase is missing")
    if (mode !== "warm") assert(phases.some(line => line.includes("device_attach")), "Ordinary startup phase is missing")
    const adopted = phases.some(line => line.includes("prepared_adoption"))
    assertEqual(adopted, mode === "warm", "Observed startup path differs from selected case")
    samples.push({ computerId: computer.id, sessionId: admission.session.id, usefulOutputMs, adopted, before, path, phases, nonce: result.nonce })
    await admission.session.cancel({ idempotencyKey: `cancel:${marker}:${ordinal}` }, { signal })
    // Release the slot so the next iteration proves refill after consumption.
    await deleteComputer(computer, `delete:${marker}:${ordinal}`)
    minimumObservation = Date.now()
  }
  return { mode, samples, freshSecrets: true, privateWrites: true, noPreclaimInit: true }
})
