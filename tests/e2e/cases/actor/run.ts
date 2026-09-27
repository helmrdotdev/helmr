import { setTimeout as delay } from "node:timers/promises"
import { deadline } from "../../support/deadline"
import assert from "node:assert/strict"
import { execFile } from "node:child_process"
import { randomUUID } from "node:crypto"
import { mkdir, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { promisify } from "node:util"
import { HelmrClient, type WorkspaceRef, type SessionRef, type TurnRef } from "@helmr/sdk"
import { assertActorRestore, assertActorTurns } from "./assertions"

const apiUrl = process.env["HELMR_API_URL"], apiKey = process.env["HELMR_API_KEY"]
const evidenceDir = process.env["HELMR_EVIDENCE_DIR"]
assert(apiUrl && apiKey && evidenceDir, "API URL/key and new evidence directory required")
assert.equal(apiUrl.replace(/\/$/, ""), "http://127.0.0.1:58080", "run on the dedicated runtime host")
await mkdir(evidenceDir, { recursive: false, mode: 0o700 })
const client = new HelmrClient({ url: apiUrl, apiKey })
const marker = randomUUID()
const evidence: Record<string, unknown> = { case: "actor", marker, startedAt: new Date().toISOString(), passed: false }
let workspace: WorkspaceRef | undefined, session: SessionRef | undefined, tokenId: string | undefined
let failure: unknown
let sessionClosed = false
const request = () => ({ signal: deadline(30_000) })
async function observe(action: string, runId: string) {
  assert(process.env["HELMR_RUNTIME_HOST_TOOL"], "HELMR_RUNTIME_HOST_TOOL is required for host observations")
  const { stdout } = await promisify(execFile)("sudo", ["-n", "python3",
    process.env["HELMR_RUNTIME_HOST_TOOL"]!, action, "--run-id", runId],
    { timeout: 200_000, maxBuffer: 65536 })
  return JSON.parse(stdout) as Record<string, unknown>
}
async function completed(turn: TurnRef) {
  const end = Date.now() + 180_000
  while (Date.now() < end) {
    const value = await turn.retrieve(request())
    if (value.status === "completed") return value
    assert(["queued", "running"].includes(value.status), `Actor Turn ended with ${value.status}`)
    await delay(1000)
  }
  throw new Error("Actor Turn did not complete within deadline")
}
try {
  tokenId = (await client.tokens.create({ timeout: "10m", idempotencyKey: `token:${marker}` }, request())).id
  evidence["tokenId"] = tokenId
  workspace = await client.sandboxes.createWorkspace("verification", { key: marker, idempotencyKey: `workspace:${marker}` }, request())
  evidence["workspaceId"] = workspace.id
  const started = await client.actors.start("verification-actor", { workspace, key: marker,
    idempotencyKey: `actor:${marker}`, run: { retry: { enabled: false } } }, request())
  session = started.session
  evidence["sessionId"] = session.id
  evidence["runId"] = started.run.id
  const first = await session.enqueue({ marker, tokenId }, { idempotencyKey: `first:${marker}` }, request())
  evidence["firstTurnId"] = first.id
  const parked = await observe("wait-parked", started.run.id)
  evidence["parked"] = parked
  let nonce: string | undefined
  const end = Date.now() + 60_000
  while (!nonce && Date.now() < end) {
    const logs = await client.runs.logs(started.run.id, { limit: 100 }, request())
    const entries = logs.items.filter(log => log.kind === "structured" && log.attributes["marker"] === marker
      && log.attributes["sessionId"] === session!.id && typeof log.attributes["nonce"] === "string")
    assert(entries.length <= 1, "Actor entered more than once before checkpoint")
    const entry = entries[0]
    if (entry?.kind === "structured") nonce = String(entry.attributes["nonce"])
    if (!nonce) await delay(1000)
  }
  assert(nonce, "pre-checkpoint Actor nonce was not observed")
  evidence["nonceBefore"] = nonce
  await client.tokens.complete(tokenId, { result: { resume: true }, idempotencyKey: `resume:${marker}` }, request())
  const firstState = await completed(first)
  const second = await session.enqueue({ marker }, { idempotencyKey: `second:${marker}` }, request())
  evidence["secondTurnId"] = second.id
  const secondState = await completed(second)
  assert.equal(firstState.sequence, 1)
  assert.equal(secondState.sequence, 2)
  assert.notEqual(first.id, second.id)
  assertActorTurns({ marker, nonce, sessionId: session.id, runId: started.run.id, workspaceId: workspace.id },
    firstState.result, secondState.result)
  evidence["turns"] = [firstState, secondState]
  await session.close({ idempotencyKey: `close:${marker}` }, request())
  await client.runs.wait(started.run, { signal: deadline(180_000) }).unwrap()
  assert.equal((await session.retrieve(request())).status, "closed")
  const restored = await observe("verify-restored", started.run.id)
  assertActorRestore(parked, restored, session.id)
  evidence["restored"] = restored
  evidence["passed"] = true
} catch (error) {
  failure = error
  evidence["failure"] = error instanceof Error ? error.message : String(error)
} finally {
  for (const [name, cleanup] of [
    ["sessionCleanup", session ? async () => {
      const signal = deadline(180_000)
      let current = await session!.retrieve({ signal })
      evidence["sessionCancellation"] = "not-needed"
      if (current.status === "open" || current.status === "closing") {
        evidence["sessionCancellation"] = "requesting"
        await session!.cancel({ idempotencyKey: `cancel-session:${marker}` }, { signal })
        evidence["sessionCancellation"] = "request-accepted"
      }
      evidence["sessionConvergence"] = "waiting"
      while (current.status !== "closed") {
        assert.notEqual(current.status, "failed", "failed Session does not prove Workspace release")
        await delay(1000, undefined, { signal })
        current = await session!.retrieve({ signal })
      }
      sessionClosed = true
      evidence["sessionConvergence"] = "closed"
    } : undefined],
    ["tokenCleanup", tokenId ? async () => {
      if ((await client.tokens.retrieve(tokenId!, request())).status === "pending")
        await client.tokens.cancel(tokenId!, { idempotencyKey: `cancel-token:${marker}` }, request())
    } : undefined],
    ["workspaceCleanup", workspace ? async () => {
      assert(!session || sessionClosed, "Workspace deletion withheld: Session closure not confirmed")
      await workspace!.delete({ idempotencyKey: `delete:${marker}` }, request())
    } : undefined],
  ] as const) {
    if (!cleanup) { evidence[name] = "creation-unconfirmed"; continue }
    try { await cleanup(); evidence[name] = "request-accepted" }
    catch (error) { evidence[name] = "failed"; evidence[`${name}Failure`] = error instanceof Error ? error.message : String(error); failure ??= error }
  }
  evidence["finishedAt"] = new Date().toISOString()
  await writeFile(join(evidenceDir, "actor.json"), JSON.stringify(evidence, null, 2) + "\n", { mode: 0o600 })
}
if (failure) throw failure
console.log(`Actor continuation assertions passed; evidence: ${join(evidenceDir, "actor.json")}`)
