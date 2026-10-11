import { createHash, randomBytes } from "node:crypto"
import { verify, assertEqual, deadline, completedResult, deleteComputer } from "../../support/context"
await verify("secret-injection", async ({ client, marker, cleanup, objects, startAgent }) => {
  const value = randomBytes(32).toString("hex"),
    name = `verification-${marker}`
  const secret = await client.secrets.create(
    { name, value, idempotencyKey: `secret:${marker}` },
    { signal: deadline(30_000) },
  )
  objects.secret_ids.push(secret.id)
  cleanup(() =>
    client.secrets
      .ref(secret.id)
      .revoke({ idempotencyKey: `revoke:${marker}` }, { signal: deadline(30_000) }),
  )
  const computer = await client.computerDefinitions.createComputer(
    "helmr-secret-smoke",
    {
      key: marker,
      idempotencyKey: `computer:${marker}`,
      secrets: [{ secretId: secret.id, env: { name: "VERIFICATION_SECRET", mode: "raw" } }],
    },
    { signal: deadline(30_000) },
  )
  objects.computer_ids.push(computer.id)
  cleanup(() =>
    deleteComputer(computer, `delete:${marker}`),
  )
  const { turn } = await startAgent(
    "secret-smoke",
    {
      computer,
      input: { sha256: createHash("sha256").update(value).digest("hex") },
      idempotencyKey: `run:${marker}`,
    },
  )
  assertEqual(
    await completedResult(turn, 180_000),
    { matched: true },
    "Secret binding changed",
  )
  return { matched: true }
})
