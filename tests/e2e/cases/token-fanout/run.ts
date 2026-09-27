import { verify, assert, assertEqual, waitRun, deadline } from "../../support/context"
import type { tokenTask } from "../token-wait/task"

await verify("token-fanout", async ({ client, marker, objects, workspace, cleanup }) => {
  const token = await client.tokens.create({ timeout: "10m", idempotencyKey: `token:${marker}` })
  objects.token_ids.push(token.id)
  cleanup(async () => {
    if ((await client.tokens.retrieve(token.id)).status === "pending")
      await client.tokens.cancel(token.id, { idempotencyKey: `cancel:${marker}` })
  })
  async function start(suffix: string) {
    const target = await workspace("verification-token", suffix)
    const run = await client.tasks.start<typeof tokenTask>("verification-token", {
      workspace: target,
      payload: { marker, tokenId: token.id },
      idempotencyKey: `run:${suffix}:${marker}`,
    })
    objects.run_ids.push(run.id)
    return run
  }
  const runs = [await start("first"), await start("second")]
  await Promise.all(runs.map((run) => waitRun(client, run.id, ["waiting"])))
  const completed = await client.tokens.complete(token.id, {
    result: { approved: true },
    idempotencyKey: `complete:${marker}`,
  })
  assertEqual(completed.status, "completed", "Token did not complete")
  for (const run of runs) {
    assertEqual(
      await client.runs.wait(run, { signal: deadline(10 * 60_000) }).unwrap(),
      { marker, token: { approved: true } },
      "Token did not resume every waiting Run",
    )
  }
  const late = await start("after-completion")
  assertEqual(
    await client.runs.wait(late, { signal: deadline(10 * 60_000) }).unwrap(),
    { marker, token: { approved: true } },
    "Completion before wait did not resolve",
  )
  const page = await client.tokens.list({ limit: 100 })
  assert(
    page.items.some((item) => item.id === token.id),
    "Token list omitted completed Token",
  )
  return { fanout: true, completionBeforeWait: true, listed: true }
})
