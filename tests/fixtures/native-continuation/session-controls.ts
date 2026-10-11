import assert from "node:assert/strict"
import { readFile } from "node:fs/promises"
import { setTimeout as delay } from "node:timers/promises"
import type { AgentDefinition, ComputerRef, Turn } from "../../../sdk/typescript/src/agent"
import { callMcp, fixtureInput, waitManagedTurn, type ManagedAdmission } from "../../e2e/support/runtime-mcp"

async function entered(turn: Turn, session: string, active: string) {
  for (;;) {
    try {
      assert.equal(JSON.parse(await readFile(`/workspace/control-${session}.json`, "utf8")).turnId, active)
      return
    } catch (error) {
      if (!(error instanceof Error) || !("code" in error) || error.code !== "ENOENT") throw error
    }
    await delay(100, undefined, { signal: turn.signal })
  }
}
export async function checkSessionControls(turn: Turn, computer: ComputerRef,
  definition: AgentDefinition, owned: { id: string },
  inspect: (sessionId: string, turnId: string) => Promise<{ status: string }>) {
  const enqueue = (text: string) => callMcp<ManagedAdmission>(turn.signal, "enqueue", { sessionId: owned.id, input: fixtureInput(text), idempotencyKey: `${turn.id}:${text}` })
  const retrieve = () => callMcp<{ holds: { id: string; session_id: string }[] }>(turn.signal, "inspect_session", { sessionId: owned.id })
  const active = await enqueue("native-control-hold")
  await entered(turn, owned.id, active.turnId)
  const pending = await enqueue("after-interrupt")
  const interruption = { sessionId: owned.id, idempotencyKey: "native-control-interrupt" }
  const interrupted = await callMcp<{ holdId: string }>(turn.signal,"interrupt_session",interruption)
  assert.deepEqual(await callMcp(turn.signal,"interrupt_session",interruption), interrupted)
  assert.equal((await waitManagedTurn(turn.signal,owned.id,active.turnId)).status, "interrupted")
  const held = await retrieve()
  assert.equal(held.holds.length, 1)
  assert.equal(held.holds[0]!.id, interrupted.holdId)
  assert.equal(held.holds[0]!.session_id, owned.id)
  for (let sample = 0; sample < 200; sample++) {
    assert.equal((await inspect(owned.id, pending.turnId)).status, "queued")
    assert.deepEqual((await retrieve()).holds, held.holds)
    await delay(100, undefined, { signal: turn.signal })
  }
  const resumed = await callMcp<{ holdId: string }>(turn.signal,"resume_session",{ sessionId: owned.id, holdId: interrupted.holdId, idempotencyKey: "native-control-resume" })
  assert.equal(resumed.holdId, interrupted.holdId)
  const completed = await waitManagedTurn(turn.signal,owned.id,pending.turnId)
  assert.equal(completed.status, "completed")
  assert.equal(completed.result, "after-interrupt")
  assert.deepEqual(await callMcp(turn.signal,"interrupt_session",interruption), interrupted)
  assert.deepEqual((await retrieve()).holds, [])
  assert.equal((await waitManagedTurn(turn.signal,owned.id,active.turnId)).status, "interrupted")
  const cancelled = await callMcp<ManagedAdmission>(turn.signal,"spawn",{ agentId: definition.id, input: fixtureInput("native-control-hold"), computerId:computer.id, idempotencyKey:`${turn.id}:cancel-target` })
  await entered(turn, cancelled.sessionId, cancelled.turnId)
  const queued = await callMcp<ManagedAdmission>(turn.signal,"enqueue",{sessionId:cancelled.sessionId,input:fixtureInput("must-not-run"),idempotencyKey:`${turn.id}:cancel-queued`})
  await callMcp(turn.signal,"cancel_session",{sessionId:cancelled.sessionId,idempotencyKey:"native-control-cancel"})
  assert.equal((await waitManagedTurn(turn.signal,cancelled.sessionId,cancelled.turnId)).status, "cancelled")
  assert.equal((await waitManagedTurn(turn.signal,cancelled.sessionId,queued.turnId)).status, "cancelled")
  assert.equal((await callMcp<{status:string}>(turn.signal,"inspect_session",{sessionId:cancelled.sessionId})).status,"cancelled")
  return { interruptedTurn: active.turnId, resumedTurn: pending.turnId, holdId: interrupted.holdId,
    cancelledSession: cancelled.sessionId, cancelledTurn: cancelled.turnId, cancelledQueuedTurn: queued.turnId }
}
