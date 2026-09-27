import { setTimeout as delay } from "node:timers/promises"
import { deadline } from "../../support/deadline"
import assert from "node:assert/strict"
import { execFile } from "node:child_process"
import { randomUUID } from "node:crypto"
import { mkdir, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { promisify } from "node:util"
import { HelmrClient, type ComputerRef } from "@helmr/sdk"
import type { persistenceTask } from "./task"

const apiUrl = process.env["HELMR_API_URL"]
const apiKey = process.env["HELMR_API_KEY"]
const evidenceDir = process.env["HELMR_EVIDENCE_DIR"]
assert(apiUrl && apiKey && evidenceDir, "HELMR_API_URL, HELMR_API_KEY and HELMR_EVIDENCE_DIR are required")
// The native observer reads this host's private DB, so a remote API cannot match it.
assert.equal(apiUrl.replace(/\/$/, ""), "http://127.0.0.1:58080", "run this case on the dedicated runtime host")
await mkdir(evidenceDir, { recursive: false, mode: 0o700 })
const client = new HelmrClient({ url: apiUrl, apiKey })
const marker = randomUUID()
const evidence: Record<string, unknown> = { case: "persistence", marker, startedAt: new Date().toISOString(), passed: false }
let computer: ComputerRef | undefined
let tokenId: string | undefined
let failure: unknown
const request = () => ({ signal: deadline(30_000) })
async function observe(action: string, runId: string) {
  const { stdout } = await promisify(execFile)("sudo", ["-n", "python3", process.env.HELMR_RUNTIME_HOST_TOOL ?? (() => { throw new Error("HELMR_RUNTIME_HOST_TOOL must identify the installed host profile") })(), action, "--run-id", runId], { timeout: 200_000, maxBuffer: 65536 })
  return JSON.parse(stdout)
}
try {
  const token = await client.tokens.create({ timeout: "10m", idempotencyKey: `token:${marker}` }, request())
  tokenId = token.id
  evidence["tokenId"] = tokenId
  computer = await client.sandboxes.createComputer("verification", { key: marker, idempotencyKey: `computer:${marker}` }, request())
  evidence["computerId"] = computer.id
  const run = await client.tasks.start<typeof persistenceTask>("verification-persistence", {
    computer, payload: { marker, tokenId }, idempotencyKey: `run:${marker}`,
  }, request())
  evidence["runId"] = run.id
  const parked = await observe("wait-parked", run.id)
  evidence["parked"] = parked
  let nonce: string | undefined
  const end = Date.now() + 60_000
  while (!nonce && Date.now() < end) {
    const logs = await client.runs.logs(run.id, { limit: 100 }, request())
    const matches = logs.items.filter(log => log.kind === "structured" && log.attributes["marker"] === marker && typeof log.attributes["nonce"] === "string")
    assert(matches.length <= 1, "Task entered more than once before resume")
    const log = matches[0]
    if (log?.kind === "structured") nonce = String(log.attributes["nonce"])
    if (!nonce) await delay(1000)
  }
  assert(nonce, "pre-checkpoint memory nonce was not observed")
  evidence["nonceBefore"] = nonce
  await client.tokens.complete(tokenId, { result: { resume: true }, idempotencyKey: `resume:${marker}` }, request())
  const output = await client.runs.wait(run, { signal: deadline(180_000) }).unwrap()
  assert.deepEqual(output, { marker, nonce, runId: run.id, computerId: computer.id })
  const restored = await observe("verify-restored", run.id)
  assert.equal(restored.checkpoint_id, parked.checkpoint_id, "resume did not use the observed checkpoint")
  assert.equal(restored.prior_runtime_id, parked.prior_runtime_id)
  evidence["restored"] = restored
  evidence["output"] = output
  evidence["passed"] = true
} catch (error) {
  failure = error
  evidence["failure"] = error instanceof Error ? error.message : String(error)
} finally {
  for (const [name, cleanup] of [
    ["tokenCleanup", tokenId ? async () => {
      const token = await client.tokens.retrieve(tokenId!, request())
      if (token.status === "pending") await client.tokens.cancel(tokenId!, { idempotencyKey: `cancel:${marker}` }, request())
    } : undefined],
    ["computerCleanup", computer ? () => computer!.delete({ idempotencyKey: `delete:${marker}` }, request()) : undefined],
  ] as const) {
    if (!cleanup) { evidence[name] = "creation-unconfirmed"; continue }
    try { await cleanup(); evidence[name] = "request-accepted" }
    catch (error) { evidence[name] = "failed"; evidence[`${name}Failure`] = error instanceof Error ? error.message : String(error); failure ??= error }
  }
  evidence["finishedAt"] = new Date().toISOString()
  await writeFile(join(evidenceDir, "persistence.json"), JSON.stringify(evidence, null, 2) + "\n", { mode: 0o600 })
}
if (failure) throw failure
console.log(`Persistence assertions passed; evidence: ${join(evidenceDir, "persistence.json")}`)
