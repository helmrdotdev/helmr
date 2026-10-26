import { execFile } from "node:child_process"
import { readFile, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import { promisify } from "node:util"
import { z } from "zod"
import { assert, assertEqual, completedResult, deadline, deleteComputer, verify, waitOutput, type ClientTurn } from "../../support/context"
import { hostObservation } from "../../support/host-observation"
import { assertRestored, observePersistence } from "../../support/persistence"
import { fixtureInput } from "../../support/runtime-mcp"

assert(process.env.HELMR_EXECUTION_POLICY_PUBLIC_PROBES === "1", "Set HELMR_EXECUTION_POLICY_PUBLIC_PROBES=1 for the public HTTPS/HTTP/DNS/TLS prerequisites")
const mode = process.env.HELMR_EXECUTION_POLICY_LIFECYCLE
assert(mode === "hibernate" || mode === "drain", "Select HELMR_EXECUTION_POLICY_LIFECYCLE=hibernate or drain")
const preparation = process.env.HELMR_EXPECT_PREPARED
assert(preparation === "warm" || preparation === "cold" || preparation === "zero", "Select HELMR_EXPECT_PREPARED=warm, cold or zero")
const resource = process.env.HELMR_WORKER_RESOURCE_ID, environment = process.env.HELMR_ENVIRONMENT_ID
assert(resource && environment && process.env.HELMR_RUNTIME_HOST_TOOL, "Exact Worker resource, Environment and installed host tool are required")
const finiteBuild = process.env.HELMR_EXPECTED_PREPARATION_BUILD_ID
const blockedBuild = process.env.HELMR_EXPECTED_BLOCKED_PREPARATION_BUILD_ID
assert(preparation !== "warm" || (finiteBuild && blockedBuild), "Warm requires exact finite and blocked preparation build IDs")

const network = z.object({
  expected: z.enum(["ordinary", "finite", "blocked"]), destinationIPv4: z.ipv4(), preparation: z.string().nullable(),
  ordinaryPrerequisites: z.boolean(), deniedHost: z.boolean(), dnsBypassDenied: z.boolean(),
  directIPBypassDenied: z.boolean(), httpBypassDenied: z.boolean(),
}).passthrough()
const restricted = network.extend({ ordinaryPrerequisites: z.literal(false), deniedHost: z.literal(true),
  dnsBypassDenied: z.literal(true), directIPBypassDenied: z.literal(true), httpBypassDenied: z.literal(true) })
const admission = z.object({ sessionId: z.uuid(), turnId: z.uuid(), sequence: z.number().int() })
const report = z.object({
  network: restricted.extend({ expected: z.literal("finite"), preparation: z.literal("prepare-blocked") }),
  nonce: z.uuid(), count: z.number().int(), child: admission,
  childResult: restricted.extend({ expected: z.literal("finite"), preparation: z.literal("prepare-blocked"),
    computerId: z.uuid(), marker: z.string(), emptyMaximum: z.literal(true) }),
  inheritedFiniteMaximum: z.literal(true), allowedScopedRetry: z.literal(true),
  deniedDirectAliasDiscovery: z.literal(true), deniedCurrentReceipt: z.literal(true), emptyScope: z.literal(true),
})
const blockedReport = restricted.extend({ expected: z.literal("blocked"), preparation: z.literal("prepare-allowed") })

// Drain/restart remains operator-owned. Fresh, exclusive marker files identify
// this attempt; they cannot resume an interrupted test or authorize a service kill.
await verify(`execution-policies-${mode}`, async ({ marker, objects, computer, startAgent }) => {
  const directory = process.env.HELMR_EVIDENCE_DIR!
  const record = (name: string, value: unknown) => writeFile(join(directory, name),
    JSON.stringify(value, null, 2) + "\n", { mode: 0o600, flag: "wx" })
  const baselineComputer = await computer("execution-policy-ordinary", "lifecycle-baseline")
  const baselineSession = await startAgent("execution-policy-ordinary-probe", {
    computer: baselineComputer, input: { marker, expected: "ordinary" }, idempotencyKey: `${marker}:baseline`,
  })
  const baseline = network.parse(await completedResult(baselineSession.turn, 180_000))
  assertEqual(baseline.ordinaryPrerequisites, true, "Public endpoints must succeed before denial is policy evidence")
  await baselineSession.session.cancel({ idempotencyKey: `${marker}:baseline-finished` })
  await deleteComputer(baselineComputer, `${marker}:baseline-delete`)

  async function ready(build: string | undefined) {
    const minimum = Date.now(), signal = deadline(15 * 60_000)
    for (;;) {
      const rows = await hostObservation("worker-state", { resource_ids: [resource] })
      assertEqual(rows.length, 1, "Expected one dedicated Worker")
      const worker = rows[0], observed = Date.parse(worker.resource_observed_at)
      const leaf = worker.ready_leaf_keys?.some((key: any) => key.environment_id === environment && key.build_id === build)
      if (worker.status === "active" && observed > minimum && Date.now() - observed < 60_000 && (preparation !== "warm" || leaf)) {
        if (preparation === "zero") {
          assertEqual(worker.local_optional_vm_slots, 0, "Zero retained optional VMs")
          assertEqual(worker.optional_disk_bytes, 0, "Zero retained optional disks")
        }
        return worker
      }
      await delay(500, undefined, { signal })
    }
  }
  async function startup(computerId: string, worker: any, requestedAt: number, build: string | undefined) {
    const path = await hostObservation("computer-path", { computer_id: computerId })
    const leases = path.leases.filter((lease: any) => lease.status === "active" && lease.fenced_at === null)
    assertEqual(leases.length, 1, "Startup must retain one live lease")
    const lease = leases[0]
    assertEqual(path.computer.id, computerId, "Startup observation belongs to another Computer")
    assert(typeof path.computer.initial_root_id === "string" && /^[0-9a-f]{64}$/.test(path.computer.initial_root_digest), "Startup omitted immutable seed identity")
    assertEqual(lease.worker_host_id, worker.id, "Startup selected another Host")
    assertEqual(lease.worker_epoch, worker.current_epoch, "Startup changed Worker incarnation")
    if (preparation === "warm") assertEqual(path.computer.image_build_id, build, "Computer adopted another preparation build")
    const { stdout } = await promisify(execFile)("sudo", ["-n", "journalctl", "-u", "helmr-worker",
      "--since", `@${Math.floor(requestedAt / 1000)}`, "--no-pager", "-o", "cat"], { timeout: 15_000, maxBuffer: 16 * 1024 * 1024 })
    const phases = stdout.split("\n").filter(line => line.includes(computerId) && line.includes(lease.computer_instance_id) && line.includes("duration_ms"))
    assert(phases.some(line => line.includes("agent_ready")), "Startup readiness phase missing")
    assertEqual(phases.some(line => line.includes("prepared_adoption")), preparation === "warm", "Startup path differs from selected preparation mode")
    if (preparation !== "warm") assert(phases.some(line => line.includes("device_attach")), "Cold startup phase missing")
    const sample = { worker, path, phases, lease }
    await record(`${computerId}-startup.json`, sample)
    return sample
  }

  const finiteWorker = await ready(finiteBuild), finiteAt = Date.now()
  const finite = await computer("execution-policy-finite", "lifecycle-finite")
  const input = { marker, expected: "finite", destinationIPv4: baseline.destinationIPv4 }
  const coordinator = await startAgent("execution-policy-coordinator", {
    computer: finite, input, idempotencyKey: `${marker}:finite`,
  })
  async function coordinatorResult(turn: ClientTurn) {
    const output = await waitOutput(coordinator.session, turn,
      value => value !== null && typeof value === "object" && !Array.isArray(value) && "phase" in value && value.phase === "child-admitted", 240_000)
    const child = admission.parse(output)
    objects.session_ids.push(child.sessionId)
    objects.turn_ids.push(child.turnId)
    const result = report.parse(await completedResult(turn, 300_000))
    assertEqual(result.child, child, "Observed child receipt changed")
    assertEqual(result.childResult.computerId, finite.id, "Managed child did not use actual finite Computer")
    assertEqual(result.childResult.marker, marker, "Managed child input changed")
    return result
  }
  const first = await coordinatorResult(coordinator.turn)
  const finiteStartup = await startup(finite.id, finiteWorker, finiteAt, finiteBuild)
  const blockedWorker = await ready(blockedBuild), blockedAt = Date.now()
  assertEqual(blockedWorker.id, finiteWorker.id, "Fixture moved between dedicated Hosts before lifecycle action")
  const blocked = await computer("execution-policy-blocked", "lifecycle-blocked")
  const blockedInput = { marker, expected: "blocked", destinationIPv4: baseline.destinationIPv4 }
  const blockedSession = await startAgent("execution-policy-ordinary-probe", {
    computer: blocked, input: blockedInput, idempotencyKey: `${marker}:blocked`,
  })
  const firstBlocked = blockedReport.parse(await completedResult(blockedSession.turn, 180_000))
  const blockedStartup = await startup(blocked.id, blockedWorker, blockedAt, blockedBuild)
  const targets = [
    { computer: finite, session: coordinator.session, source: finiteStartup.lease, members: [coordinator.session.id, first.child.sessionId] },
    { computer: blocked, session: blockedSession.session, source: blockedStartup.lease, members: [blockedSession.session.id] },
  ]
  await record("before.json", { baseline, first, firstBlocked, finiteStartup, blockedStartup })
  if (mode === "hibernate") {
    for (const target of targets) await target.computer.hibernate({ idempotencyKey: `${marker}:hibernate:${target.computer.id}` })
  } else {
    await record("ready-for-drain.json", { computerIds: targets.map(target => target.computer.id), sourceHostId: finiteWorker.id })
  }

  const captures = []
  for (const target of targets) {
    const signal = deadline((mode === "drain" ? 15 : 10) * 60_000)
    let path: any, checkpoint: any
    for (;;) {
      path = await hostObservation("computer-path", { computer_id: target.computer.id })
      checkpoint = path.checkpoints.find((item: any) => item.source_lease_epoch === target.source.epoch && item.status === "ready")
      const old = path.leases.find((lease: any) => lease.epoch === target.source.epoch)
      if (checkpoint && old?.status === "lost" && old.fenced_at !== null) break
      await delay(500, undefined, { signal })
    }
    assertEqual(checkpoint.target_lease_epoch, null, "Checkpoint restored before operator restart gate")
    assert(checkpoint.disk_save_id, "Checkpoint omitted private disk save")
    assertEqual(checkpoint.members.map((member: any) => member.session_id).sort(), [...target.members].sort(), "Checkpoint changed Session membership")
    const parked = await observePersistence("wait-parked", target.session.id)
    assertEqual(parked.checkpoint_id, checkpoint.id, "Session observer selected another checkpoint")
    captures.push({ target, path, checkpoint, parked })
    await record(`${target.computer.id}-captured.json`, { path, parked })
  }
  let replacement: any
  if (mode === "drain") {
    await record("capture-observed.json", { computerIds: targets.map(target => target.computer.id), sourceHostId: finiteWorker.id })
    const signal = deadline(10 * 60_000)
    for (;;) {
      try {
        const acknowledgment = JSON.parse(await readFile(join(directory, "worker-restarted.json"), "utf8"))
        assertEqual(acknowledgment.computerIds, targets.map(target => target.computer.id), "Restart acknowledgment belongs to another attempt")
        assertEqual(acknowledgment.sourceHostId, finiteWorker.id, "Restart acknowledgment identifies another source Host")
        const rows = await hostObservation("worker-state", { resource_ids: [resource] })
        if (rows.length === 1 && rows[0].status === "active" && rows[0].id !== finiteWorker.id && rows[0].observed_epoch * 1000 > Date.now() - 60_000) {
          replacement = rows[0]
          await record("replacement-observed.json", replacement)
          break
        }
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error
      }
      await delay(500, undefined, { signal })
    }
  }

  const restoredPaths = []
  for (const capture of captures) {
    const { target, checkpoint, parked } = capture
    const turns = (await target.session.turns.list()).items.map(turn => turn.id)
    await target.computer.wake({ idempotencyKey: `${marker}:wake:${target.computer.id}` })
    const restored = await observePersistence("verify-restored", target.session.id)
    assertRestored(parked, restored, target.session.id)
    assertEqual((await target.session.turns.list()).items.map(turn => turn.id), turns, "Wake fabricated a Turn")
    const path = await hostObservation("computer-path", { computer_id: target.computer.id })
    const original = target.computer.id === finite.id ? finiteStartup.path.computer : blockedStartup.path.computer
    for (const key of ["id", "image_build_id", "initial_root_id", "initial_root_digest"]) {
      assertEqual(path.computer[key], original[key], `Restore changed Computer ${key}`)
    }
    const consumed = path.checkpoints.find((item: any) => item.id === checkpoint.id)
    const destination = path.leases.find((lease: any) => lease.epoch === restored.target_lease_epoch)
    assertEqual(consumed?.status, "consumed", "Exact checkpoint was not consumed")
    assertEqual(consumed?.target_lease_epoch, destination?.epoch, "Checkpoint target lease changed")
    assertEqual(consumed?.controls_reconciled_at !== null, true, "Checkpoint omitted current controls")
    assert(destination && destination.status === "active" && destination.fenced_at === null, "Restored lease is not current")
    assertEqual(destination.restored_from_save_id, checkpoint.disk_save_id, "Restore selected another private disk save")
    assert.notEqual(destination.computer_instance_id, target.source.computer_instance_id, "Restore reused source Instance")
    if (mode === "drain") {
      assert.notEqual(destination.worker_host_id, target.source.worker_host_id, "Drain did not replace logical Host")
      assertEqual(destination.worker_host_id, replacement.id, "Restore selected another physical resource")
      assertEqual(destination.worker_epoch, replacement.current_epoch, "Restore selected another Worker incarnation")
    }
    else assertEqual(destination.worker_host_id, target.source.worker_host_id, "Same-host restore moved Hosts")
    restoredPaths.push({ path, restored })
    await record(`${target.computer.id}-restored.json`, { path, restored })
  }
  const second = await coordinator.session.enqueue(fixtureInput(input), { idempotencyKey: `${marker}:finite-restored` })
  objects.turn_ids.push(second.id)
  const continued = await coordinatorResult(second)
  assertEqual(continued.nonce, first.nonce, "Coordinator setup was reconstructed")
  assertEqual([first.count, continued.count], [1, 2], "Coordinator lost retained setup state")
  assertEqual(continued.child, first.child, "Retained narrowed descriptor retry replaced admitted work")
  const blockedTurn = await blockedSession.session.enqueue(fixtureInput(blockedInput), { idempotencyKey: `${marker}:blocked-restored` })
  objects.turn_ids.push(blockedTurn.id)
  const continuedBlocked = blockedReport.parse(await completedResult(blockedTurn, 180_000))
  return { mode, preparation, baseline, first, firstBlocked, finiteStartup, blockedStartup,
    restoredPaths, continued, continuedBlocked, physicalCrossHost: false }
})
