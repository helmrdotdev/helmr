import assert from "node:assert/strict"
import { readFile } from "node:fs/promises"
import { setTimeout as delay } from "node:timers/promises"
import type { AgentDefinition, ComputerIdentity, Turn, RuntimeMcpConnectionOptions } from "../../../sdk/typescript/src/agent"
import { callMcp, fixtureInput, waitManagedTurn, type ManagedAdmission } from "../../e2e/support/runtime-mcp"

async function entered(turn: Turn, session: string, active: string) {
  for (;;) {
    try {
      if (JSON.parse(await readFile(`/workspace/control-${session}.json`, "utf8")).turnId === active) return
    } catch (error) {
      if (!(error instanceof Error) || !("code" in error) || error.code !== "ENOENT") throw error
    }
    await delay(100, undefined, { signal: turn.signal })
  }
}
export async function checkSessionControls(mcp: RuntimeMcpConnectionOptions, turn: Turn, computer: ComputerIdentity,
  definition: AgentDefinition, owned: { id: string },
  inspect: (sessionId: string, turnId: string) => Promise<{ status: string }>) {
  const enqueue = (text: string, admission: string) => callMcp<ManagedAdmission>(mcp, turn.signal, "enqueue", { sessionId: owned.id, input: fixtureInput(text), idempotencyKey: `${turn.id}:${admission}` })
  const retrieve = () => callMcp<{ holds: { id: string; session_id: string }[] }>(mcp, turn.signal, "inspect_session", { sessionId: owned.id })
  const exact = await enqueue("native-control-hold", "exact-active")
  await entered(turn, owned.id, exact.turnId)
  const processBefore = JSON.parse(await readFile(`/workspace/control-${owned.id}.json`, "utf8")).processId
  assert(Number.isSafeInteger(processBefore) && processBefore > 0)
  const following = await enqueue("after-exact-interrupt", "exact-following")
  const exactRequest = { sessionId: owned.id, turnId: exact.turnId, idempotencyKey: `${turn.id}:exact-interrupt` }
  const exactReceipt = await callMcp<{ id: string; turnId: string; status: string }>(mcp, turn.signal, "interrupt_turn", exactRequest)
  assert.equal(exactReceipt.turnId, exact.turnId)
  assert.equal(exactReceipt.status, "accepted")
  assert.equal((await waitManagedTurn(mcp, turn.signal, owned.id, exact.turnId)).status, "interrupted")
  const next = await waitManagedTurn(mcp, turn.signal, owned.id, following.turnId)
  assert.equal(next.status, "completed")
  assert.equal(next.result, "after-exact-interrupt")
  assert.deepEqual((await retrieve()).holds, [])
  assert.equal(JSON.parse(await readFile(`/workspace/control-${owned.id}.json`, "utf8")).processId, processBefore)
  assert.deepEqual(await callMcp(mcp, turn.signal, "interrupt_turn", exactRequest), exactReceipt)
  const active = await enqueue("native-control-hold", "session-active")
  await entered(turn, owned.id, active.turnId)
  const pending = await enqueue("after-interrupt", "session-following")
  const interruption = { sessionId: owned.id, idempotencyKey: "native-control-interrupt" }
  const interrupted = await callMcp<{ holdId: string }>(mcp, turn.signal,"interrupt_session",interruption)
  assert.deepEqual(await callMcp(mcp, turn.signal,"interrupt_session",interruption), interrupted)
  assert.equal((await waitManagedTurn(mcp, turn.signal,owned.id,active.turnId)).status, "interrupted")
  const held = await retrieve()
  assert.equal(held.holds.length, 1)
  assert.equal(held.holds[0]!.id, interrupted.holdId)
  assert.equal(held.holds[0]!.session_id, owned.id)
  for (let sample = 0; sample < 200; sample++) {
    assert.equal((await inspect(owned.id, pending.turnId)).status, "queued")
    assert.deepEqual((await retrieve()).holds, held.holds)
    await delay(100, undefined, { signal: turn.signal })
  }
  const resumed = await callMcp<{ holdId: string }>(mcp, turn.signal,"resume_session",{ sessionId: owned.id, holdId: interrupted.holdId, idempotencyKey: "native-control-resume" })
  assert.equal(resumed.holdId, interrupted.holdId)
  const completed = await waitManagedTurn(mcp, turn.signal,owned.id,pending.turnId)
  assert.equal(completed.status, "completed")
  assert.equal(completed.result, "after-interrupt")
  assert.equal(JSON.parse(await readFile(`/workspace/control-${owned.id}.json`, "utf8")).processId, processBefore)
  assert.deepEqual(await callMcp(mcp, turn.signal,"interrupt_session",interruption), interrupted)
  assert.deepEqual((await retrieve()).holds, [])
  assert.equal((await waitManagedTurn(mcp, turn.signal,owned.id,active.turnId)).status, "interrupted")
  const cancelled = await callMcp<ManagedAdmission>(mcp, turn.signal,"spawn",{ agent: definition.id, input: fixtureInput("native-control-hold"), computerId:computer.id, idempotencyKey:`${turn.id}:cancel-target` })
  await entered(turn, cancelled.sessionId, cancelled.turnId)
  const queued = await callMcp<ManagedAdmission>(mcp, turn.signal,"enqueue",{sessionId:cancelled.sessionId,input:fixtureInput("must-not-run"),idempotencyKey:`${turn.id}:cancel-queued`})
  await callMcp(mcp, turn.signal,"cancel_session",{sessionId:cancelled.sessionId,idempotencyKey:"native-control-cancel"})
  assert.equal((await waitManagedTurn(mcp, turn.signal,cancelled.sessionId,cancelled.turnId)).status, "cancelled")
  assert.equal((await waitManagedTurn(mcp, turn.signal,cancelled.sessionId,queued.turnId)).status, "cancelled")
  assert.equal((await callMcp<{status:string}>(mcp, turn.signal,"inspect_session",{sessionId:cancelled.sessionId})).status,"cancelled")
  return { exactInterruptedTurn: exact.turnId, exactFollowingTurn: following.turnId, retainedProcessId: processBefore, interruptedTurn: active.turnId, resumedTurn: pending.turnId, holdId: interrupted.holdId,
    cancelledSession: cancelled.sessionId, cancelledTurn: cancelled.turnId, cancelledQueuedTurn: queued.turnId }
}
