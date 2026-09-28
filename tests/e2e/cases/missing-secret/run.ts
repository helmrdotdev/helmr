import { verify, assert, errorCode, deadline } from "../../support/context"
await verify("missing-secret", async ({ client, marker, cleanup, objects }) => {
  await assert.rejects(
    async () => {
      const ref = await client.sandboxes.createComputer(
        "helmr-secret-smoke",
        {
          key: marker,
          idempotencyKey: `missing:${marker}`,
          secrets: [
            { secret: `absent-${marker}`, env: { name: "HELMR_VERIFICATION_SECRET", mode: "raw" } },
          ],
        },
        { signal: deadline(30_000) },
      )
      objects.computer_ids.push(ref.id)
      cleanup(() =>
        ref.delete({ idempotencyKey: `delete:${marker}` }, { signal: deadline(30_000) }),
      )
    },
    (error) => errorCode(error) === "secret_unavailable",
  )
  return { rejectedBeforeExecution: true }
})
