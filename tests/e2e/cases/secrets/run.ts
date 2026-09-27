import { verify, assert, assertEqual, deadline } from "../../support/context"
await verify("secrets", async ({ client, marker, cleanup }) => {
  const secretName = `verification-${marker}`.replace(/[^A-Za-z0-9_.-]/g, "-")
  const secret = await client.secrets.create(
    {
      name: secretName,
      value: `initial:${marker}`,
      idempotencyKey: `secret:create:${marker}`,
    },
    { signal: AbortSignal.timeout(30_000) },
  )
  cleanup(() =>
    client.secrets
      .ref(secret.id)
      .revoke({ idempotencyKey: `secret:revoke:${marker}` }, { signal: deadline(30_000) }),
  )
  const exactSecretPage = await client.secrets.list(
    { name: secretName },
    { signal: AbortSignal.timeout(30_000) },
  )
  assertEqual(exactSecretPage.items.length, 1, "Secret name lookup was not exact")
  assertEqual(exactSecretPage.items[0]!.id, secret.id, "Secret name lookup changed the ID")
  const retrievedSecret = await client.secrets.retrieve(secret.id, {
    signal: AbortSignal.timeout(30_000),
  })
  assertEqual(retrievedSecret.id, secret.id, "Secret retrieval changed the ID")
  const secretRef = client.secrets.ref(secret.id)
  const secretPage = await client.secrets.list(
    { limit: 100 },
    { signal: AbortSignal.timeout(30_000) },
  )
  assert(
    secretPage.items.some((item) => item.id === secret.id),
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

  return { secretId: secret.id, rotated: true, revoked: true }
})
