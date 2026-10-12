import { verify, assert, assertEqual, deadline } from "../../support/context"
await verify("secrets", async ({ client, marker, objects, cleanup }) => {
  const secretName = `verification-${marker}`.replace(/[^A-Za-z0-9_.-]/g, "-")
  const secret = await client.secrets.create(
    {
      secret: secretName,
      value: `initial:${marker}`,
      idempotencyKey: `secret:create:${marker}`,
    },
    { signal: AbortSignal.timeout(30_000) },
  )
  objects.secret_ids.push(secret.secret)
  cleanup(() =>
    client.secrets
      .ref(secret.secret)
      .revoke({ idempotencyKey: `secret:revoke:${marker}` }, { signal: deadline(30_000) }),
  )
  const retrievedSecret = await client.secrets.get(secret.secret, {
    signal: AbortSignal.timeout(30_000),
  })
  assertEqual(retrievedSecret.secret, secret.secret, "Secret retrieval changed the ID")
  const secretRef = client.secrets.ref(secret.secret)
  const secretPage = await client.secrets.list(
    { limit: 100 },
    { signal: AbortSignal.timeout(30_000) },
  )
  assert(
    secretPage.items.some((item) => item.secret === secret.secret),
    "Secret list omitted the created Secret",
  )
  const rotated = await secretRef.rotate(
    {
      value: `rotated:${marker}`,
      idempotencyKey: `secret:rotate:${marker}`,
    },
    { signal: AbortSignal.timeout(30_000) },
  )
  assert(rotated.rotatedAt !== undefined, "Secret rotate omitted rotatedAt")
  const revoked = await secretRef.revoke(
    { idempotencyKey: `secret:revoke:${marker}` },
    { signal: AbortSignal.timeout(30_000) },
  )
  assertEqual(revoked.status, "revoked", "Secret was not revoked")

  return { secret: secret.secret, rotated: true, revoked: true }
})
