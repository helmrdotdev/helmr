import { execFile } from "node:child_process"
import { promisify } from "node:util"
import { setTimeout as delay } from "node:timers/promises"
import { verify, assert, assertEqual, deadline } from "../../support/context"
import type { persistenceTask } from "./task"

await verify("shared-persistence", async ({ client, marker, objects, computer, cleanup }) => {
  assertEqual(process.env.HELMR_API_URL?.replace(/\/$/, ""), "http://127.0.0.1:58080",
    "Run the physical checkpoint observer on the dedicated host")
  const hostTool = process.env.HELMR_RUNTIME_HOST_TOOL
  assert(hostTool, "HELMR_RUNTIME_HOST_TOOL is required")
  const shared = await computer("verification", "shared-persistence")
  const request = () => ({ signal: deadline(30_000) })
  const members: { token: { id: string }; marker: string }[] = []
  for (const suffix of ["a", "b"]) {
    const token = await client.tokens.create({ timeout: "10m", idempotencyKey: `${marker}:${suffix}:token` }, request())
    objects.token_ids.push(token.id)
    cleanup(async () => {
      if ((await client.tokens.retrieve(token.id, request())).status === "pending")
        await client.tokens.cancel(token.id, { idempotencyKey: `${marker}:${suffix}:cancel` }, request())
    })
    members.push({ token, marker: `${marker}:${suffix}` })
  }
  // Admit both members while the shared Computer is starting.
  const runs = await Promise.all(members.map(async member => {
    const run = await client.tasks.start<typeof persistenceTask>("verification-persistence", {
      computer: shared, payload: { marker: member.marker, tokenId: member.token.id },
      idempotencyKey: `${member.marker}:run`,
    }, request())
    objects.run_ids.push(run.id)
    return run
  }))
  const observe = async (action: string, id: string) => {
    const { stdout } = await promisify(execFile)("sudo", ["-n", "python3", hostTool, action, "--run-id", id],
      { timeout: 200_000, maxBuffer: 65536 })
    return JSON.parse(stdout)
  }
  const parked = await Promise.all(runs.map(run => observe("wait-parked", run.id)))
  assertEqual(parked[0].checkpoint_id, parked[1].checkpoint_id, "Members were not captured in one checkpoint")
  assertEqual(parked[0].prior_runtime_id, parked[1].prior_runtime_id, "Members did not share one source VM")
  const nonces = await Promise.all(runs.map(async (run, i) => {
    const signal = deadline(60_000)
    for (;;) {
      signal.throwIfAborted()
      const logs = await client.runs.logs(run.id, { limit: 100 }, { signal })
      const matches = logs.items.filter(log => log.kind === "structured" && log.attributes.marker === members[i]!.marker && typeof log.attributes.nonce === "string")
      assert(matches.length <= 1, "Member replayed before restore")
      const match = matches[0]
      if (match?.kind === "structured") return String(match.attributes.nonce)
      await delay(500, undefined, { signal })
    }
  }))
  await Promise.all(members.map(member => client.tokens.complete(member.token.id, {
    result: { resume: true }, idempotencyKey: `${member.marker}:resume`,
  }, request())))
  const outputs = await Promise.all(runs.map(run => client.runs.wait(run, { signal: deadline(180_000) }).unwrap()))
  const restored = await Promise.all(runs.map(run => observe("verify-restored", run.id)))
  for (const [i, run] of runs.entries()) {
    assertEqual(outputs[i], { marker: members[i]!.marker, nonce: nonces[i], runId: run.id, computerId: shared.id },
      "Restored member lost its original memory or files")
    assertEqual(restored[i].checkpoint_id, parked[i].checkpoint_id, "Member resumed from a different checkpoint")
  }
  assertEqual(restored[0].restored_runtime_ids.length, 1, "Checkpoint restored more than once")
  return { computerId: shared.id, parked, nonces, outputs, restored }
})
