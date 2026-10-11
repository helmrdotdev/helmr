import { verify, assert, assertEqual, waitTurn } from "../../support/context"
await verify("invalid-payload", async ({ marker, computer, startAgent }) => {
  const target = await computer("helmr-edge-smoke")
  const { turn } = await startAgent(
    "edge-smoke",
    {
      computer: target,
      input: { mode: "computer-overwrite", unknown: true },
      idempotencyKey: `invalid-input:${marker}`,
    },
  )
  const terminal = await waitTurn(turn, ["failed"])
  assertEqual(terminal.error?.code, "handler_failed", "Wrong failure code")
  assert(terminal.error?.message?.includes("unknown"), "Application schema did not identify the unknown field")
  return { failureCode: "handler_failed" }
})
