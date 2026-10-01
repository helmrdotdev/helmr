import { execFile } from "node:child_process"
import { promisify } from "node:util"
import { setTimeout as delay } from "node:timers/promises"
import { verify, assert, assertEqual, deadline, readTelemetry, waitRun } from "../../support/context"
import type { captureAbortTask } from "./task"

await verify("capture-abort", async ({ client, marker, objects, computer, cleanup }) => {
  assertEqual(process.env.HELMR_API_URL?.replace(/\/$/, ""), "http://127.0.0.1:58080", "Run on the dedicated host")
  const hostTool = process.env.HELMR_RUNTIME_HOST_TOOL
  assert(hostTool, "HELMR_RUNTIME_HOST_TOOL is required")
  const faultURL = "http://127.0.0.1:58089/__fault"
  const fault = async (method = "GET") => {
    const response = await fetch(faultURL, { method, signal: deadline(10_000) })
    assert(response.ok, `Fault observer returned ${response.status}`)
    return response.json() as Promise<{ delay_ms: number; started: string; failed: string }>
  }
  const observe = async (action: string, runId: string) => {
    const { stdout } = await promisify(execFile)("sudo", ["-n", "python3", hostTool, action, "--run-id", runId], { timeout: 200_000, maxBuffer: 65536 })
    return JSON.parse(stdout)
  }
  const ref = await computer("capture-abort-verification", "capture-abort")
  const tokenIds: string[] = []
  for (const phase of ["first", "second"]) {
    const token = await client.tokens.create({ timeout: "20m", idempotencyKey: `${marker}:${phase}` })
    tokenIds.push(token.id)
    objects.token_ids.push(token.id)
    cleanup(async () => {
      if ((await client.tokens.retrieve(token.id)).status === "pending") await client.tokens.cancel(token.id, { idempotencyKey: `${marker}:${phase}:cancel` })
    })
  }
  const armed = await fault("POST")
  assert(armed.delay_ms >= 360_000, "Fault must leave time to cancel after the five-minute guest grant")
  // Admit both members while the shared Computer starts. The DB assertion below
  // rejects a run where the cancelled member missed the captured member set.
  const runs = await Promise.all([marker, `${marker}:cancelled`].map(async memberMarker => {
    const member = await client.tasks.start<typeof captureAbortTask>("verification-capture-abort", {
      computer: ref, payload: { marker: memberMarker, first: tokenIds[0]!, second: tokenIds[1]!, cancelledMarker: `/root/cancelled-capture-${marker}`, cancelMember: memberMarker !== marker }, idempotencyKey: `${memberMarker}:run`,
    })
    objects.run_ids.push(member.id)
    return member
  }))
  const run = runs[0]!
  const cancelled = runs[1]!
  const waitFault = async (field: "started" | "failed", timeout: number) => {
    const signal = deadline(timeout)
    for (;;) {
      signal.throwIfAborted()
      const state = await fault()
      if (Date.parse(state[field]) > 0) return state
      await delay(500, undefined, { signal })
    }
  }
  const started = await waitFault("started", 180_000)
  // Keep both CP leases renewing while their old guest grants expire. Cancelling
  // earlier would eventually end the cancelled member's lease and interrupt the
  // upload before the intended delayed S3 response.
  await delay(Math.max(0, Date.parse(started.started) + 310_000 - Date.now()), undefined, { signal: deadline(330_000) })
  let cancelledCounter = 0
  let probeCursor: string | undefined
  const probeDeadline = deadline(30_000)
  do {
    const probeLogs = await readTelemetry(() => client.runs.logs(cancelled.id, { limit: 100, cursor: probeCursor }, { signal: probeDeadline }))
    for (const log of probeLogs.items) {
      if (log.kind === "structured" && log.attributes.marker === `${marker}:cancelled` && Number.isSafeInteger(log.attributes.counter)) {
        cancelledCounter = Math.max(cancelledCounter, Number(log.attributes.counter))
      }
    }
    probeCursor = probeLogs.nextCursor
  } while (probeCursor)
  assert(cancelledCounter > 0, "No active cancelled-member counter observed before thaw")
  const stillHeld = await fault()
  assert(Date.parse(stillHeld.started) === Date.parse(started.started) && !(Date.parse(stillHeld.failed) > 0), "Probe baseline was not read while the upload was held")
  await client.runs.cancel(cancelled.id, {})
  const cancellationConfirmedAt = Date.now()
  // Resolve while sealed so acknowledged abort can continue into the second wait.
  await client.tokens.complete(tokenIds[0]!, { result: { cancelledCounter }, idempotencyKey: `${marker}:first:complete` })
  const firstCompletionConfirmedAt = Date.now()
  const failed = await waitFault("failed", 660_000)
  assert(Date.parse(failed.failed) > cancellationConfirmedAt, "Cancellation did not commit before the upload failure")
  assert(Date.parse(failed.failed) > firstCompletionConfirmedAt, "Token did not resolve before the upload failure")
  assert(Date.parse(failed.failed) - Date.parse(started.started) > 300_000, "Capture did not exceed the old guest grant")
  const aborted = await observe("wait-aborted", run.id)
  assertEqual([...aborted.captured_run_ids].sort(), [run.id, cancelled.id].sort(), "Both members must belong to the failed capture")
  const cancelledRun = await waitRun(client, cancelled.id, ["cancelled"], 120_000)
  const signal = deadline(120_000)
  let nonce: string | undefined
  for (;;) {
    signal.throwIfAborted()
    const logs = await readTelemetry(() => client.runs.logs(run.id, { limit: 100 }, { signal }))
    const states = logs.items.filter(log => log.kind === "structured" && log.attributes.marker === marker)
    const before = states.filter(log => log.kind === "structured" && log.attributes.phase === "before")
    const resumed = states.filter(log => log.kind === "structured" && log.attributes.phase === "resumed")
    assert(before.length <= 1 && resumed.length <= 1, "Task entry or continuation replayed")
    if (before[0]?.kind === "structured" && resumed[0]?.kind === "structured") {
      nonce = String(before[0].attributes.nonce)
      assertEqual(resumed[0].attributes.nonce, nonce, "Abort lost in-memory state")
      break
    }
    await delay(500, undefined, { signal })
  }
  const parked = await observe("wait-parked", run.id)
  assertEqual(parked.prior_runtime_id, aborted.source_instance_id, "Second capture did not use the original source")
  assert(parked.checkpoint_id !== aborted.checkpoint_id, "Aborted checkpoint became restorable")
  await client.tokens.complete(tokenIds[1]!, { result: { resume: true }, idempotencyKey: `${marker}:second:complete` })
  const output = await client.runs.wait(run, { signal: deadline(180_000) }).unwrap()
  assertEqual(output, { marker, nonce, runId: run.id, computerId: ref.id }, "Restore lost memory, files or Run identity")
  const restored = await observe("verify-restored", run.id)
  assertEqual(restored.checkpoint_id, parked.checkpoint_id, "Wrong checkpoint restored")
  return { started, failed, cancelledCounter, cancellationConfirmedAt, firstCompletionConfirmedAt, aborted, cancelledRun, parked, restored, output }
})
