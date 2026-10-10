import assert from "node:assert/strict"
import { setTimeout as delay } from "node:timers/promises"
import type { AgentDefinition, ComputerRef, Turn } from "../../../sdk/typescript/src/agent"
import { createRuntimeMcpConnection, type RuntimeMcpConnection } from "../../../sdk/typescript/src/mcp"
import { callMcp, fixtureInput, waitManagedTurn, type ManagedAdmission } from "../../e2e/support/runtime-mcp"
import { checkSessionControls } from "./session-controls"

export interface DiscoveryState {
  owned: { id: string }
  requested: { id: string }
  ownedTurn: string
  requestedTurn: string
  mcp: RuntimeMcpConnection
  checks: number
  controls: Awaited<ReturnType<typeof checkSessionControls>>
}
interface SessionView { id: string; status: string; parent_session_id: string | null; requester_session_id: string | null; initial_turn: { id: string; status: string }; holds: unknown[] }
interface SessionPage { sessions: SessionView[]; next_cursor?: string }

// Only identity and the local MCP descriptor survive in setup state. Every call
// crosses the current Session authority boundary again after restoration.
export async function checkSessionDiscovery(turn: Turn, caller: string, computer: ComputerRef,
  definition: AgentDefinition, prior?: DiscoveryState) {
  let state = prior
  if (!state) {
    const mcp = await createRuntimeMcpConnection()
    const create = (operation: string) => callMcp<ManagedAdmission>(turn.signal, operation, { agentId: definition.id, input: fixtureInput(operation), computerId: computer.id, idempotencyKey: `${turn.id}:${operation}` },mcp)
    const owned = await create("spawn"), requested = await create("start")
    for (const admission of [owned,requested]) assert.equal((await waitManagedTurn(turn.signal,admission.sessionId,admission.turnId)).status,"completed")
    const controls = await checkSessionControls(turn, computer, definition, {id:owned.sessionId},
      (sessionId, turnId) => callMcp(turn.signal, "inspect_turn", { sessionId, turnId }, mcp))
    await callMcp(turn.signal,"close_session",{sessionId:owned.sessionId,idempotencyKey:`${turn.id}:close`},mcp)
    await turn.output.write(JSON.stringify({ discoveryCreated: requested.sessionId }))
    state = { owned:{id:owned.sessionId},requested:{id:requested.sessionId},ownedTurn:owned.turnId,requestedTurn:requested.turnId,mcp,controls,checks:0 }
  }
  const call = <T>(name:string,args:object) => callMcp<T>(turn.signal,name,args,state.mcp)
  for (const [ref, initial, parent] of [[state.owned, state.ownedTurn, caller], [state.requested, state.requestedTurn, null]] as const) {
    let view = await call<SessionView>("inspect_session",{sessionId:ref.id})
    while (view.status !== "closed") {
      assert(view.status === "open" || view.status === "closing", `receipt Session became ${view.status}`)
      await delay(100, undefined, { signal: turn.signal }); view = await call<SessionView>("inspect_session",{sessionId:ref.id})
    }
    assert.equal(view.parent_session_id,parent)
    assert.equal(view.requester_session_id,caller)
    assert.deepEqual(view.initial_turn,{id:initial,status:"completed"})
  }
  const owned = await call<SessionPage>("list_sessions",{relation:"owned",status:["closed"],limit:1})
  assert.deepEqual(owned.sessions.map(item=>item.id),[state.owned.id])
  assert.equal(owned.next_cursor,undefined)
  const requested:string[]=[]
  let cursor:string|undefined
  do {
    const page:SessionPage = await call("list_sessions",{relation:"requested",status:["closed"],limit:1,...(cursor ? {cursor}: {})})
    requested.push(...page.sessions.map(item=>item.id));cursor=page.next_cursor
  } while(cursor)
  assert.deepEqual(requested.sort(),[state.owned.id,state.requested.id].sort())
  for (const [sessionId,turnId,status] of [
    [state.owned.id,state.controls.interruptedTurn,"interrupted"],
    [state.owned.id,state.controls.resumedTurn,"completed"],
    [state.controls.cancelledSession,state.controls.cancelledTurn,"cancelled"],
    [state.controls.cancelledSession,state.controls.cancelledQueuedTurn,"cancelled"],
  ] as const) assert.equal((await call<{status:string}>("inspect_turn",{sessionId,turnId})).status,status)
  assert.deepEqual((await call<SessionView>("inspect_session",{sessionId:state.owned.id})).holds,[])
  state.checks++
  return state
}
