import { verify, assertEqual, deadline } from "../../support/context"
await verify("network-egress", async ({ client, marker, objects, computer }) => {
  const target = await computer("helmr-network-smoke")
  const run = await client.tasks.start(
    "network-smoke",
    { computer: target, payload: {}, idempotencyKey: `network-egress:${marker}` },
    { signal: deadline(30_000) },
  )
  objects.run_ids.push(run.id)
  const output = await client.runs.wait(run, { signal: deadline(20 * 60_000) }).unwrap()
  assertEqual(output, { publicIPv4: true, ipv6DefaultRoute: false }, "Guest network result")
  return { verified: true }
})
