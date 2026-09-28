import { verify, assertEqual, deadline } from "../../support/context"
await verify("computer-overwrite", async ({ client, marker, objects, computer }) => {
  const target = await computer("helmr-edge-smoke")
  const run = await client.tasks.start(
    "edge-smoke",
    {
      computer: target,
      payload: { mode: "sandbox-overwrite", marker },
      idempotencyKey: `computer-overwrite:${marker}`,
    },
    { signal: deadline(30_000) },
  )
  objects.run_ids.push(run.id)
  const output = await client.runs.wait(run, { signal: deadline(20 * 60_000) }).unwrap()
  assertEqual(
    output,
    {
      mode: "sandbox-overwrite",
      marker,
      computer: { path: "edge/overwrite.txt", content: `final:${marker}\n` },
    },
    "Overwrite result mismatch",
  )
  return { verified: true }
})
