import { verify, assert, deadline } from "../../support/context"
await verify("timer", async ({ client, marker, objects, computer }) => {
  const target = await computer("helmr-timer-smoke")
  const run = await client.tasks.start(
    "timer-smoke",
    { computer: target, payload: { marker, waitFor: "5s" }, idempotencyKey: `timer:${marker}` },
    { signal: deadline(30_000) },
  )
  objects.run_ids.push(run.id)
  const output = await client.runs.wait(run, { signal: deadline(20 * 60_000) }).unwrap()
  assert(
    output !== null && typeof output === "object" && "marker" in output && output.marker === marker,
    "Timer lost marker",
  )
  return { verified: true }
})
