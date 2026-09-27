import assert from "node:assert/strict"
import { execFile } from "node:child_process"
import { randomUUID } from "node:crypto"
import { mkdir, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { fileURLToPath } from "node:url"
import { promisify } from "node:util"
import { setTimeout as delay } from "node:timers/promises"
import { HelmrClient, type WorkspaceRef } from "@helmr/sdk"
import type { networkTask } from "../tasks/network"
import { deadline } from "./deadline"

const apiUrl = process.env["HELMR_API_URL"], apiKey = process.env["HELMR_API_KEY"]
const evidenceDir = process.env["HELMR_EVIDENCE_DIR"]
assert(apiUrl && apiKey && evidenceDir, "API URL/key and new evidence directory required")
assert.equal(apiUrl.replace(/\/$/, ""), "http://127.0.0.1:58080", "run on the dedicated runtime host")
await mkdir(evidenceDir, { recursive: false, mode: 0o700 })
const client = new HelmrClient({ url: apiUrl, apiKey })
const marker = randomUUID(), tokenIds: string[] = []
const evidence: Record<string, unknown> = { case: "metadata-isolation", marker, startedAt: new Date().toISOString(), passed: false }
let workspace: WorkspaceRef | undefined, runId: string | undefined, failure: unknown
const request = () => ({ signal: deadline(30_000) })
const terminal = new Set(["succeeded", "failed", "system_failed", "cancelled", "expired"])
async function phase(name: string) {
  const signal = deadline(180_000)
  while (!signal.aborted) {
    const logs = await client.runs.logs(runId!, { limit: 100 }, { signal })
    if (logs.items.some(log => log.kind === "structured" && log.attributes["marker"] === marker && log.attributes["phase"] === name)) return
    const run = await client.runs.retrieve(runId!, { signal })
    assert(!terminal.has(run.status), `Run ended with ${run.status} before ${name}`)
    await delay(1000, undefined, { signal })
  }
  signal.throwIfAborted()
}
async function observe() {
  const { stdout } = await promisify(execFile)("sudo", ["-n", "python3",
    fileURLToPath(new URL("./network-observer.py", import.meta.url)), runId!], { timeout: 30_000 })
  return JSON.parse(stdout) as { runtime_id: string; namespace: string; metadata_in_deny_set: boolean; denied_packets: number }
}
try {
  for (const name of ["start", "finish"]) tokenIds.push((await client.tokens.create({ timeout: "10m", idempotencyKey: `${marker}:${name}` }, request())).id)
  evidence["tokenIds"] = tokenIds
  workspace = await client.sandboxes.createWorkspace("verification", { key: marker, idempotencyKey: `${marker}:workspace` }, request())
  evidence["workspaceId"] = workspace.id
  const run = await client.tasks.start<typeof networkTask>("verification-network", { workspace,
    payload: { marker, startToken: tokenIds[0]!, finishToken: tokenIds[1]! }, idempotencyKey: `${marker}:run` }, request())
  runId = run.id
  evidence["runId"] = runId
  await phase("ready")
  const before = await observe()
  evidence["before"] = before
  await client.tokens.complete(tokenIds[0]!, { result: {}, idempotencyKey: `${marker}:start` }, request())
  await phase("observed")
  const after = await observe()
  evidence["after"] = after
  assert.equal(after.runtime_id, before.runtime_id)
  assert.equal(after.namespace, before.namespace)
  assert(before.metadata_in_deny_set && after.metadata_in_deny_set)
  assert(after.denied_packets > before.denied_packets, "no native denied packet observed; timeout alone is insufficient")
  await client.tokens.complete(tokenIds[1]!, { result: {}, idempotencyKey: `${marker}:finish` }, request())
  const output = await client.runs.wait(run, { signal: deadline(180_000) }).unwrap()
  assert.deepEqual(output, { marker, blocked: true, positiveStatus: 200, runId, workspaceId: workspace.id })
  evidence["output"] = output
  evidence["passed"] = true
} catch (error) {
  failure = error
  evidence["failure"] = error instanceof Error ? error.message : String(error)
} finally {
  for (const [name, cleanup] of [
    ["runCleanup", runId && failure ? async () => {
      await client.runs.cancel(runId!, { idempotencyKey: `${marker}:cancel` }, request())
      const signal = deadline(180_000)
      while (!terminal.has((await client.runs.retrieve(runId!, { signal })).status)) await delay(1000, undefined, { signal })
    } : undefined],
    ["tokenCleanup", async () => {
      for (const id of tokenIds) if ((await client.tokens.retrieve(id, request())).status === "pending")
        await client.tokens.cancel(id, { idempotencyKey: `${marker}:cancel:${id}` }, request())
    }],
    ["workspaceCleanup", workspace ? () => workspace!.delete({ idempotencyKey: `${marker}:delete` }, request()) : undefined],
  ] as const) {
    if (!cleanup) continue
    try { await cleanup(); evidence[name] = "request-accepted" }
    catch (error) { evidence[name] = "failed"; evidence[`${name}Failure`] = String(error); failure ??= error }
  }
  evidence["finishedAt"] = new Date().toISOString()
  await writeFile(join(evidenceDir, "network.json"), JSON.stringify(evidence, null, 2) + "\n", { mode: 0o600 })
}
if (failure) throw failure
console.log(`Metadata isolation assertions passed; evidence: ${join(evidenceDir, "network.json")}`)
