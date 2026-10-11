import { verify, assertEqual, completedResult } from "../../support/context"
await verify("network-egress", async ({ marker, computer, startAgent }) => {
  const target = await computer("helmr-network-smoke")
  const { turn } = await startAgent("network-smoke", {
    computer: target, input: null, idempotencyKey: `network:${marker}`,
  })
  assertEqual(await completedResult(turn), { publicIPv4: true, ipv6DefaultRoute: false }, "Network result mismatch")
  return { verified: true }
})
