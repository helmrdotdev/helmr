import { verify, assert, deadline } from "../../support/context"
await verify("runtime", async ({ client, marker, objects, workspace }) => {
  const target = await workspace("helmr-runtime-smoke")
  const run = await client.tasks.start(
    "runtime-smoke",
    {
      workspace: target,
      payload: { scenario: "runtime", marker, expectedEnvironment: "unknown" },
      idempotencyKey: `runtime:${marker}`,
    },
    { signal: deadline(30_000) },
  )
  objects.run_ids.push(run.id)
  const output = await client.runs.wait(run, { signal: deadline(20 * 60_000) }).unwrap()
  assert(
    output !== null && typeof output === "object" && "ok" in output && output.ok === true,
    "Runtime fixture failed",
  )
  return { verified: true }
})
