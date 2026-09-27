import { verify, assert, deadline } from "../../support/context"
await verify("concurrent-wait", async ({ client, marker, objects, computer }) => {
  const target = await computer("helmr-edge-smoke")
  const run = await client.tasks.start(
    "edge-smoke",
    {
      computer: target,
      payload: { mode: "concurrent-wait", marker, waitTimeout: 30 },
      idempotencyKey: `concurrent-wait:${marker}`,
    },
    { signal: deadline(30_000) },
  )
  objects.run_ids.push(run.id)
  const output = await client.runs.wait(run, { signal: deadline(20 * 60_000) }).unwrap()
  assert(
    output !== null &&
      typeof output === "object" &&
      "concurrentWaitRejected" in output &&
      output.concurrentWaitRejected === true,
    "Concurrent wait was accepted",
  )
  return { verified: true }
})
