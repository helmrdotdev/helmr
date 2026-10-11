import { createRuntimeMcpConnection, type RuntimeMcpConnection } from "@helmr/sdk/mcp"
import type { InputContent, Json, TurnOutcome } from "@helmr/sdk"

// Fixture commands serialize their own data into the platform's text contract.
export function fixtureInput(value: Json): InputContent { return [{ type: "text", text: JSON.stringify(value) }] }
export function fixtureValue(input: InputContent): Json { return JSON.parse(input.map(part => part.text).join("")) as Json }

export async function callMcp<T>(signal: AbortSignal, name: string, args: object, connection?: RuntimeMcpConnection): Promise<T> {
  const mcp = connection ?? await createRuntimeMcpConnection()
  const response = await fetch(mcp.url, { method: "POST", signal,
    headers: { ...mcp.headers, "Content-Type": "application/json", Accept: "application/json, text/event-stream", "MCP-Protocol-Version": "2025-11-25" },
    body: JSON.stringify({ jsonrpc: "2.0", id: crypto.randomUUID(), method: "tools/call", params: { name, arguments: args } }) })
  if (!response.ok) throw new Error(`MCP ${name}: HTTP ${response.status}`)
  const rpc = await response.json() as { error?: unknown; result?: { isError?: boolean; content: { type: string; text?: string }[] } }
  if (rpc.error || !rpc.result || rpc.result.isError || rpc.result.content.length !== 1 || rpc.result.content[0]?.type !== "text") throw new Error(`MCP ${name}: ${JSON.stringify(rpc)}`)
  return JSON.parse(rpc.result.content[0].text!) as T
}
export interface ManagedAdmission { sessionId: string; turnId: string; sequence: number; created: boolean }
export async function waitManagedTurn(signal: AbortSignal, sessionId: string, turnId: string): Promise<TurnOutcome> {
  for (;;) {
    const state = await callMcp<{ status: "settled"; outcome: TurnOutcome } | { status: "timeout" }>(signal, "wait_turn", { sessionId, turnId, timeoutMs: 30000 })
    if (state.status === "settled") return state.outcome
  }
}
