import { verify, assert, assertEqual, waitTurn } from "../../support/context"
await verify("expected-error", async ({ marker, computer, startAgent }) => {
  const target = await computer("helmr-edge-smoke")
  const { turn } = await startAgent(
    "edge-smoke",
    {
      computer: target,
      input: { mode: "expected-error" },
      idempotencyKey: `expected-error:${marker}`,
    },
  )
  const terminal = await waitTurn(turn, ["failed"])
  assertEqual(terminal.error?.code, "handler_failed", "Wrong failure code")
  assert(terminal.error?.message?.includes("intentional edge-case failure"), "Original application error was lost")
  return { failureCode: "handler_failed" }
})
