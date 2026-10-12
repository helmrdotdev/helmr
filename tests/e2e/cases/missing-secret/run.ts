import { verify, assert, errorCode, deadline, deleteComputer } from "../../support/context"
await verify("missing-secret", async ({ client, marker, cleanup, objects }) => {
  await assert.rejects(
    async () => {
      const ref = await client.computers.create({ template: "helmr-secret-smoke", key: marker,
          idempotencyKey: `missing:${marker}`,
          secrets: [
            { secret: "MISSING_SECRET", env: { name: "VERIFICATION_SECRET", mode: "raw" } },
          ], }, { signal: deadline(30_000) })
      objects.computer_ids.push(ref.id)
      cleanup(() =>
        deleteComputer(ref, `delete:${marker}`),
      )
    },
    (error) => errorCode(error) === "secret_unavailable",
  )
  return { rejectedBeforeExecution: true }
})
