import { verify, assert, completedResult } from "../../support/context"
await verify("runtime", async ({ marker, computer, startAgent }) => {
  const target = await computer("helmr-runtime-smoke")
  const { turn } = await startAgent("runtime-smoke", {
    computer: target,
    input: { scenario: "runtime", marker, expectedEnvironment: "unknown" },
    idempotencyKey: `runtime:${marker}`,
  })
  const output = await completedResult(turn)
  assert(output !== null && typeof output === "object" && "ok" in output && output.ok === true,
    "Runtime fixture failed")
  return { verified: true, output }
})
