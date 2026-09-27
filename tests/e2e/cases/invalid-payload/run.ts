import { verify, assertEqual, deadline, waitRun } from "../../support/context"
await verify("invalid-payload", async ({ client, marker, objects, workspace }) => {
  const target = await workspace("helmr-edge-smoke")
  const run = await client.tasks.start(
    "edge-smoke",
    {
      workspace: target,
      payload: { mode: "sandbox-overwrite", unknown: true },
      idempotencyKey: `invalid-payload:${marker}`,
    },
    { signal: deadline(30_000) },
  )
  objects.run_ids.push(run.id)
  const terminal = await waitRun(client, run.id, ["failed"])
  assertEqual(terminal.failure?.code, "task_payload_invalid", "Wrong failure code")
  return { failureCode: "task_payload_invalid" }
})
