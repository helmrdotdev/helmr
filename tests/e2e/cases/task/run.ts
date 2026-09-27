import { deadline } from "../../support/deadline"
import assert from "node:assert/strict"
import { randomUUID } from "node:crypto"
import { mkdir, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { HelmrClient, type WorkspaceRef } from "@helmr/sdk"
import type { verificationTask } from "./task"

// One executable case. Add/remove ordinary case files instead of a case registry.
const apiUrl = process.env["HELMR_API_URL"]
const apiKey = process.env["HELMR_API_KEY"]
const evidenceDir = process.env["HELMR_EVIDENCE_DIR"]
assert(apiUrl && apiKey && evidenceDir, "HELMR_API_URL, HELMR_API_KEY and HELMR_EVIDENCE_DIR are required")
await mkdir(evidenceDir, { recursive: false, mode: 0o700 })
const client = new HelmrClient({ url: apiUrl, apiKey })
const marker = randomUUID()
const evidence: Record<string, unknown> = { case: "task", marker, startedAt: new Date().toISOString(), passed: false, fixtureCleanup: "pending" }
let workspace: WorkspaceRef | undefined
let failure: unknown
try {
  workspace = await client.sandboxes.createWorkspace("verification", {
    key: marker, idempotencyKey: `verification:create:${marker}`,
  }, { signal: deadline(30_000) })
  evidence["workspaceId"] = workspace.id
  const run = await client.tasks.start<typeof verificationTask>("verification-task", {
    workspace, payload: { marker }, idempotencyKey: `verification:task:${marker}`,
  }, { signal: deadline(30_000) })
  evidence["runId"] = run.id
  const output = await client.runs.wait(run, { signal: deadline(180_000) }).unwrap()
  assert.deepEqual(output, { marker, runId: run.id, workspaceId: workspace.id })
  evidence["output"] = output
  evidence["passed"] = true
} catch (error) {
  failure = error
  evidence["failure"] = error instanceof Error ? error.message : String(error)
} finally {
  if (workspace) {
    try {
      await workspace.delete({ idempotencyKey: `verification:delete:${marker}` }, { signal: deadline(30_000) })
      evidence["fixtureCleanup"] = "delete-request-accepted"
    } catch (error) {
      evidence["fixtureCleanup"] = "failed"
      evidence["cleanupFailure"] = error instanceof Error ? error.message : String(error)
      failure ??= error
    }
  } else {
    // An ambiguous create timeout may have committed; retain its idempotency key.
    evidence["fixtureCleanup"] = "creation-unconfirmed"
  }
  evidence["finishedAt"] = new Date().toISOString()
  await writeFile(join(evidenceDir, "task.json"), JSON.stringify(evidence, null, 2) + "\n", { mode: 0o600 })
}
if (failure) throw failure
console.log(`Task assertions passed; evidence: ${join(evidenceDir, "task.json")}`)
