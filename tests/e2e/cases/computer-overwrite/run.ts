import { verify, assertEqual, completedResult } from "../../support/context"
await verify("computer-overwrite", async ({ marker, computer, startAgent }) => {
  const target = await computer("helmr-edge-smoke")
  const { turn } = await startAgent(
    "edge-smoke",
    {
      computer: target,
      input: { mode: "computer-overwrite", marker },
      idempotencyKey: `computer-overwrite:${marker}`,
    },
  )
  const output = await completedResult(turn)
  assertEqual(
    output,
    {
      mode: "computer-overwrite",
      marker,
      computer: { path: "edge/overwrite.txt", content: `final:${marker}\n` },
    },
    "Overwrite result mismatch",
  )
  return { verified: true }
})
