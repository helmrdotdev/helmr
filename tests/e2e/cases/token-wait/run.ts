import { verify, assert, deadline, waitRun } from "../../support/context"
import type { tokenTask } from "./task"

await verify("token-wait", async ({ client, marker, objects, computer, cleanup }) => {
  const target = await computer("verification-token")
  const run = await client.tasks.start<typeof tokenTask>(
    "verification-token",
    {
      computer: target,
      payload: { marker },
      idempotencyKey: `run:${marker}`,
    },
    { signal: deadline(30_000) },
  )
  objects.run_ids.push(run.id)
  const signal = deadline(5 * 60_000)
  let tokenId: string
  for (;;) {
    signal.throwIfAborted()
    const tokens = await client.tokens.list({ limit: 100 }, { signal })
    const token = tokens.items.find((token) => token.tags.includes(marker))
    if (token) {
      tokenId = token.id
      break
    }
    const current = await client.runs.retrieve(run.id, { signal })
    assert(
      !["failed", "succeeded", "system_failed", "cancelled", "expired"].includes(current.status),
      "Run ended before creating its Token",
    )
    await new Promise((resolve) => setTimeout(resolve, 500))
  }
  objects.token_ids.push(tokenId)
  cleanup(async () => {
    const token = await client.tokens.retrieve(tokenId, { signal: deadline(30_000) })
    if (token.status === "pending")
      await client.tokens.cancel(
        tokenId,
        { idempotencyKey: `cancel:${marker}` },
        { signal: deadline(30_000) },
      )
  })
  await waitRun(client, run.id, ["waiting"])
  await client.tokens.complete(
    tokenId,
    { result: { approved: true, note: marker }, idempotencyKey: `complete:${marker}` },
    { signal: deadline(30_000) },
  )
  const output = await client.runs.wait(run, { signal: deadline(10 * 60_000) }).unwrap()
  assert(output !== null && typeof output === "object" && "token" in output)
  assert(
    output.token !== null &&
      typeof output.token === "object" &&
      "approved" in output.token &&
      output.token.approved === true,
  )
  return { createdInsideRun: true, resumed: true }
})
