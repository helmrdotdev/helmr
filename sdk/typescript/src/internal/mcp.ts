import type { RuntimeMcpConnection } from "../mcp"
const runtimeMcp = Symbol.for("helmr.agent.v1.mcp_connection")
type McpGlobal = typeof globalThis & { [runtimeMcp]?: () => Promise<RuntimeMcpConnection> }

export function installRuntimeMcp(factory: () => Promise<RuntimeMcpConnection>): () => void {
  const target = globalThis as McpGlobal
  if (target[runtimeMcp]) throw new Error("Runtime MCP is already installed")
  target[runtimeMcp] = factory
  return () => { if (target[runtimeMcp] === factory) delete target[runtimeMcp] }
}
