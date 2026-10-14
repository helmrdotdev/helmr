import type { StandardSchemaV1 } from "../../../sdk/typescript/src/agent"
import type { AgentInputContract, InputSchemaDiscovery } from "../../../sdk/typescript/src/schema/input"
import { canonicalizeJsonValue } from "@helmr/sdk/internal"
import type { Json } from "@helmr/sdk"

// Build-time discovery is advisory. Never execute business validators here and
// never fetch JSON Schema references during discovery or validation.
export function describeInput(schema: StandardSchemaV1 | undefined, agentId: string): AgentInputContract {
  if (!schema) return { inputKind: "text" }
  const unavailable = (reason: Extract<InputSchemaDiscovery, { status: "unavailable" }>["reason"]): AgentInputContract => {
    console.warn(`Agent ${agentId}: input JSON Schema unavailable (${reason}); runtime validation remains enabled`)
    return { inputKind: "json", inputSchema: { status: "unavailable", reason } }
  }
  let document: unknown
  try {
    const standard = schema["~standard"] as StandardSchemaV1["~standard"] & { jsonSchema?: { input?: (options: {target: string}) => unknown } }
    if (typeof standard.jsonSchema?.input !== "function") return unavailable("missing_converter")
    document = standard.jsonSchema.input({ target: "draft-2020-12" })
  } catch { return unavailable("conversion_failed") }
  if (typeof document !== "boolean" && (!document || typeof document !== "object" || Array.isArray(document))) return unavailable("invalid_schema")
  let bytes: Uint8Array
  try { bytes = canonicalizeJsonValue(document as Json) }
  catch { return unavailable("invalid_schema") }
  if (bytes.byteLength > 64 * 1024) return unavailable("schema_too_large")
  return { inputKind: "json", inputSchema: { status: "available", schema: JSON.parse(new TextDecoder().decode(bytes)) as Json } }
}
