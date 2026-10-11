/** A stable connection for clients running inside the managed Computer. */
export interface RuntimeMcpConnection {
  readonly url: string
  readonly headers: Readonly<{ Authorization: string }>
}
const runtimeMcp = Symbol.for("helmr.agent.v1.mcp_connection")
type McpGlobal = typeof globalThis & { [runtimeMcp]?: () => Promise<RuntimeMcpConnection> }

/** Obtain this Session process's managed MCP connection during setup. */
export function createRuntimeMcpConnection(): Promise<RuntimeMcpConnection> {
  const factory = (globalThis as McpGlobal)[runtimeMcp]
  if (!factory) return Promise.reject(new Error("Runtime MCP requires the managed Session runtime"))
  return factory()
}
