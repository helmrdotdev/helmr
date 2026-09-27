import { createHash, randomBytes } from "node:crypto"
import { verify, assertEqual, deadline } from "../../support/context"
await verify("secret-injection", async ({ client, marker, cleanup, objects }) => {
  const value = randomBytes(32).toString("hex"),
    name = `verification-${marker}`
  const secret = await client.secrets.create(
    { name, value, idempotencyKey: `secret:${marker}` },
    { signal: deadline(30_000) },
  )
  cleanup(() =>
    client.secrets
      .ref(secret.id)
      .revoke({ idempotencyKey: `revoke:${marker}` }, { signal: deadline(30_000) }),
  )
  const workspace = await client.sandboxes.createWorkspace(
    "helmr-secret-smoke",
    {
      key: marker,
      idempotencyKey: `workspace:${marker}`,
      secrets: [{ secret: name, env: { name: "HELMR_VERIFICATION_SECRET", mode: "raw" } }],
    },
    { signal: deadline(30_000) },
  )
  objects.workspace_ids.push(workspace.id)
  cleanup(() =>
    workspace.delete({ idempotencyKey: `delete:${marker}` }, { signal: deadline(30_000) }),
  )
  const run = await client.tasks.start(
    "secret-smoke",
    {
      workspace,
      payload: { sha256: createHash("sha256").update(value).digest("hex") },
      idempotencyKey: `run:${marker}`,
    },
    { signal: deadline(30_000) },
  )
  objects.run_ids.push(run.id)
  assertEqual(
    await client.runs.wait(run, { signal: deadline(180_000) }).unwrap(),
    { matched: true },
    "Secret binding changed",
  )
  return { matched: true }
})
