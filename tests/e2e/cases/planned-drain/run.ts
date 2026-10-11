import { execFile } from "node:child_process"
import { promisify } from "node:util"
import { readFile, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import { verify, assert, deadline } from "../../support/context"
import { hostObservation } from "../../support/host-observation"

// The operator pauses Dispatcher after ready-for-pause.json, acknowledges with
// dispatcher-paused.json, then drains after command-queued.json. It restarts the
// Worker and Dispatcher only after capture-observed.json. These are fresh-case
// coordination files, never recovery or permission to resume interrupted work.
await verify("planned-drain", async ({ client, marker, computer, cleanup }) => {
  const directory = process.env.HELMR_EVIDENCE_DIR!
  const record = (name: string, value: unknown) => writeFile(join(directory, name),
    JSON.stringify(value, null, 2) + "\n", { mode: 0o600, flag: "wx" })
  const shared = await computer("verification-outage", "planned-drain")
  const target = client.computers.ref(shared.id)
  const initial = await target.exec({ command: ["sh", "-ceu", 'printf %s "$MARKER" > /workspace/drain-marker'],
    env: { MARKER: marker }, idempotencyKey: `${marker}:write` })
  const written = await initial.wait({ signal: deadline(300_000) })
  assert.equal(written.kind, "exited")
  assert.equal(written.exitCode, 0)
  const before = await hostObservation("computer-path", { computer_id: shared.id })
  const source = before.leases.filter((lease: any) => lease.status === "active" && lease.fenced_at === null)
  assert.equal(source.length, 1, "expected one active source lease")
  assert.equal(source[0].status, "active")
  await record("ready-for-pause.json", { computerId: shared.id, before })
  const pauseDeadline = deadline(120_000)
  for (;;) {
    pauseDeadline.throwIfAborted()
    try {
      const acknowledgment = JSON.parse(await readFile(join(directory, "dispatcher-paused.json"), "utf8"))
      assert.equal(acknowledgment.computerId, shared.id)
      break
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error
    }
    await delay(500, undefined, { signal: pauseDeadline })
  }
  const dispatcher = await promisify(execFile)("systemctl", ["show",
    "helmr-verification-dispatcher.service", "-p", "ActiveState", "--value"], { timeout: 10_000 })
  assert.equal(dispatcher.stdout.trim(), "inactive", "Dispatcher must be paused before queueing")
  const assertUncaptured = (path: any) => {
    const current = path.leases.find((lease: any) => lease.epoch === source[0].epoch)
    assert.equal(current?.status, "active")
    assert.equal(current?.fenced_at, null)
    assert(!path.checkpoints.some((p: any) => p.source_lease_epoch === source[0].epoch),
      "source was already captured before the drain trigger")
  }
  assertUncaptured(await hostObservation("computer-path", { computer_id: shared.id }))
  const pending = await target.exec({ command: ["cat", "/workspace/drain-marker"],
    timeout: "10m", idempotencyKey: `${marker}:read` })
  cleanup(async () => {
    const state = await pending.retrieve()
    if (["pending", "starting", "running"].includes(state.status)) await pending.cancel()
  })
  const queued = await hostObservation("computer-path", { computer_id: shared.id })
  assertUncaptured(queued)
  const command = queued.commands.find((c: any) => c.id === pending.id)
  assert.equal(command?.status, "pending")
  assert.equal(command?.computer_lease_epoch, null)
  await record("command-queued.json", { computerId: shared.id, commandId: pending.id, queued })
  const captureDeadline = deadline(600_000)
  let captured: any
  let checkpoint: any
  for (;;) {
    captureDeadline.throwIfAborted()
    captured = await hostObservation("computer-path", { computer_id: shared.id })
    checkpoint = captured.checkpoints.find((p: any) => p.source_lease_epoch === source[0].epoch && p.status === "ready")
    const old = captured.leases.find((lease: any) => lease.epoch === source[0].epoch)
    if (checkpoint && old?.status === "released" && old.fenced_at !== null) break
    await delay(1000, undefined, { signal: captureDeadline })
  }
  const stillPending = captured.commands.find((c: any) => c.id === pending.id)
  assert.equal(stillPending?.status, "pending")
  assert.equal(stillPending?.computer_lease_epoch, null)
  await record("capture-observed.json", { computerId: shared.id, captured })
  const outcome = await pending.wait({ signal: deadline(600_000) })
  assert.equal(outcome.kind, "exited")
  assert.equal(outcome.exitCode, 0)
  let stdout = ""
  for await (const event of pending.streamLogs({}, { signal: deadline(30_000) })) {
    assert(event.kind !== "gap", "Command output gap")
    if (event.stream === "stdout") stdout += new TextDecoder().decode(event.content)
  }
  assert.equal(stdout, marker, "restored Command lost the original file")
  const restored = await hostObservation("computer-path", { computer_id: shared.id })
  const finished = restored.commands.find((c: any) => c.id === pending.id)
  const destination = restored.leases.find((lease: any) => lease.epoch === finished?.computer_lease_epoch)
  assert(destination && destination.epoch > source[0].epoch)
  assert.notEqual(destination.computer_instance_id, source[0].computer_instance_id)
  const consumed = restored.checkpoints.find((item: any) => item.id === checkpoint.id)
  assert.equal(consumed?.status, "consumed")
  assert.equal(consumed?.target_lease_epoch, destination.epoch)
  assert.equal(destination.restored_from_save_id, checkpoint.disk_save_id)
  assert.notEqual(destination.worker_host_id, source[0].worker_host_id)
  return { before, queued, captured, restored, stdout }
})
