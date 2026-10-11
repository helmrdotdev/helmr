import { verify, assert, assertEqual, completedResult } from "../../support/context"
await verify("delay", async ({ marker, computer, startAgent }) => {
  const target = await computer("helmr-delay-smoke")
  const { turn } = await startAgent("delay-smoke", {
    computer: target, input: { marker, delayMs: 5000 }, idempotencyKey: `delay:${marker}`,
  })
  const output = await completedResult(turn)
  assert(output !== null && typeof output === "object" && "marker" in output && "elapsedMs" in output && "steps" in output)
  assertEqual(output.marker, marker, "Delay lost marker")
  assert(typeof output.elapsedMs === "number" && output.elapsedMs >= 5000, "Application timer returned too early")
  assertEqual(output.steps, ["before-delay", "after-delay"], "Application timer lost state")
  return { verified: true, output }
})
