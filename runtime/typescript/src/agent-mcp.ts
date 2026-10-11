import type { Json } from "../../../sdk/typescript/src/agent"
import { agentProto } from "@helmr/proto"
import type { RuntimeMcpConnection } from "../../../sdk/typescript/src/mcp"
import { installRuntimeMcp } from "../../../sdk/typescript/src/internal/mcp"
import type { AgentChannel } from "./agent-channel"

export function installAgentMcp(channel: AgentChannel): () => void {
  return installRuntimeMcp(async () => {
    // Repeated factory calls revalidate local admission and obtain the same
    // descriptor. They never mint or renew upstream authority in user code.
    const raw = await channel.operation(agentProto.Operation_Method.GET_MCP_CONNECTION, null)
    if (!raw || typeof raw !== "object" || Array.isArray(raw)) throw new Error("Invalid runtime MCP connection")
    const value = raw as Record<string, Json>
    if (typeof value['url'] !== "string") throw new Error("Invalid runtime MCP connection")
    const rawHeaders = value['headers']
    if (!rawHeaders || typeof rawHeaders !== "object" || Array.isArray(rawHeaders)) throw new Error("Invalid runtime MCP headers")
    const headers = rawHeaders as Record<string, Json>
    if (typeof headers['Authorization'] !== "string" || !headers['Authorization'].startsWith("Bearer ")) throw new Error("Invalid runtime MCP headers")
    const url = new URL(value['url'])
    if (url.protocol !== "http:" || url.hostname !== "127.0.0.1" || !url.port || url.pathname !== "/mcp" || url.username || url.password || url.search || url.hash) throw new Error("Runtime MCP endpoint is not guest-local")
    return Object.freeze({ url: value['url'], headers: Object.freeze({ Authorization: headers['Authorization'] }) }) satisfies RuntimeMcpConnection
  })
}
