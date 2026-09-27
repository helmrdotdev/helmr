import { verify, assertEqual, deadline, waitRun } from "../../support/context"
await verify("expected-error", async ({ client, marker, objects, workspace }) => {
  const target = await workspace("helmr-edge-smoke")
  const run = await client.tasks.start(
    "edge-smoke",
    {
      workspace: target,
      payload: { mode: "expected-error" },
      idempotencyKey: `expected-error:${marker}`,
    },
    { signal: deadline(30_000) },
  )
  objects.run_ids.push(run.id)
  const terminal = await waitRun(client, run.id, ["failed"])
  assertEqual(terminal.failure?.code, "task_failed", "Wrong failure code")
  return { failureCode: "task_failed" }
})
