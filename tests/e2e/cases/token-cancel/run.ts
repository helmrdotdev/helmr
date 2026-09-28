import { verify, assertEqual, deadline } from "../../support/context"
await verify("token-cancel", async ({ client, marker, objects, cleanup }) => {
  const token = await client.tokens.create({
    timeout: "10m",
    tags: ["smoke", "computer-basic-exec"],
    metadata: { marker: marker },
    idempotencyKey: `token:create:${marker}`,
  })
  objects.token_ids.push(token.id)
  cleanup(() =>
    client.tokens.cancel(
      token.id,
      { idempotencyKey: `token:cancel:${marker}` },
      { signal: deadline(30_000) },
    ),
  )
  const tokenSnapshot = await client.tokens.retrieve(token.id)
  assertEqual(tokenSnapshot.id, token.id, "external Token retrieve changed the ID")
  const canceled = await client.tokens.cancel(token.id, {
    idempotencyKey: `token:cancel:${marker}`,
  })
  assertEqual(canceled.status, "cancelled", "external Token was not cancelled")

  return { retrieved: true, cancelled: true }
})
