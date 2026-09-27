import type { childTaskSmokeChild } from "./task"
import { verify, assertEqual, deadline, waitRun } from "../../support/context"

await verify("cancel-peer", async ({ client, marker, objects, computer, cleanup }) => {
  const shared = await computer("helmr-child-task-target-smoke", "cancel-peer")
  const start = async (suffix: string, holdSeconds: number) => {
    const run = await client.tasks.start<typeof childTaskSmokeChild>("child-task-smoke-child", {
      computer: shared,
      payload: { marker: `${marker}:${suffix}`, fail: false, holdSeconds },
      idempotencyKey: `cancel-peer:${marker}:${suffix}`,
    }, { signal: deadline(30_000) })
    objects.run_ids.push(run.id)
    cleanup(async () => {
      const current = await client.runs.retrieve(run.id, { signal: deadline(30_000) })
      if (!["succeeded", "failed", "cancelled", "expired", "system_failed"].includes(current.status)) {
        await client.runs.cancel(run.id, {}, { signal: deadline(30_000) })
        await waitRun(client, run.id, ["cancelled", "succeeded"], 120_000)
      }
    })
    return run
  }
  const target = await start("target", 240)
  await waitRun(client, target.id, ["running"])
  const peer = await start("peer", 45)
  await waitRun(client, peer.id, ["running"])
  await client.runs.cancel(target.id, {}, { signal: deadline(30_000) })
  await waitRun(client, target.id, ["cancelled"], 120_000)
  const result = await client.runs.wait(peer, { signal: deadline(120_000) }).unwrap()
  assertEqual(result, { marker: `${marker}:peer`, childRunId: peer.id, attemptNumber: 1 },
    "Cancelling a member stopped or retried its peer")
  return { cancelledRun: target.id, peerRun: peer.id, peerAttempt: 1, sharedComputer: shared.id }
})
