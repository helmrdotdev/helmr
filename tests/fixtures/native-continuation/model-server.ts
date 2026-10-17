import { fixtureInput } from "../../e2e/support/runtime-mcp"
// Loopback model servers for native qualification.
import { createServer } from "node:http"
import { readFileSync } from "node:fs"
import { claudeModel, codexModel } from "../../../examples/issue-fixer/probes/native-model"
import { nativeStopModelState, nativeStopResponse, type NativeStopModelState } from "./native-stop-model"
import { performanceTool, type PerformanceCommand } from "./performance-model"
export async function startNativeModels(targetSession: () => string, ports?: Record<"codex" | "claude" | "proof", number>, requests: { codex: any[]; claude: any[] } = { codex: [], claude: [] }, performanceMode = () => false, controlState: NativeStopModelState = nativeStopModelState()) {
const command = (provider: string) => JSON.parse(readFileSync(`/workspace/performance-${provider}.json`, "utf8")) as PerformanceCommand
const performanceCodex = performanceTool("codex", () => command("codex"))
const performanceClaude = performanceTool("claude", () => command("claude"))
function enqueueTool(body: any): { name: string; namespace?: string } {
  for (const tool of body.tools ?? []) {
    if (tool.type === "namespace" && tool.tools?.some((nested: any) => nested.name === "enqueue")) return { name: "enqueue", namespace: tool.name }
    if (tool.name?.includes("enqueue")) return { name: tool.name }
  }
  throw new Error("Native client did not expose the managed enqueue tool: " + JSON.stringify(body.tools?.map((tool: any) => ({ type: tool.type, name: tool.name }))))
}

const codex = codexModel(requests.codex, false, false, (body, requestIndex) => {
  if (performanceMode()) return performanceCodex(body, requestIndex)
  const response = nativeStopResponse("codex", controlState)
  if (response.normalIndex === undefined) return response.codex
  const index = response.normalIndex
  return index % 2 === 0 ? undefined : {
  type: "function_call", id: `fc_${index}`, call_id: `call_${index}`, ...enqueueTool(body),
  arguments: JSON.stringify({ sessionId: targetSession(), input: fixtureInput(`codex-${index}`), idempotencyKey: `codex-${index}` }),
} })
const claude = claudeModel(requests.claude, false, (body, requestIndex) => {
  if (performanceMode()) return performanceClaude(body, requestIndex)
  const response = nativeStopResponse("claude", controlState)
  if (response.normalIndex === undefined) return response.claude
  const index = response.normalIndex
  return index % 2 === 0 ? undefined : {
  id: `toolu_${index}`, name: enqueueTool(body).name, input: { sessionId: targetSession(), input: fixtureInput(`claude-${index}`), idempotencyKey: `claude-${index}` },
} })
const proof = createServer((_req, res) => {res.setHeader("content-type", "application/json"); res.end(JSON.stringify(requests))})
await Promise.all(([ ["codex", codex], ["claude", claude], ["proof", proof] ] as const).map(([name, server]) => new Promise<void>((resolve, reject) => {
  server.once("error", reject)
  server.listen(ports?.[name] ?? 0, "127.0.0.1", resolve)
})))
const port = (server: typeof codex) => (server.address() as { port: number }).port
return { endpoints: { codex: port(codex), claude: port(claude), proof: port(proof), host: "127.0.0.1" }, requests, controlState, servers: [codex, claude, proof] }
}
