import type { JsonValue } from "../contract"
import { canonicalizeJsonValue } from "../internal/jsoncanon"

export type InputSchemaDiscovery =
  | Readonly<{ status: "available"; schema: JsonValue }>
  | Readonly<{ status: "unavailable"; reason: "missing_converter" | "conversion_failed" | "invalid_schema" | "schema_too_large" }>
export type AgentInputContract =
  | Readonly<{ inputKind: "text"; inputSchema?: never }>
  | Readonly<{ inputKind: "json"; inputSchema: InputSchemaDiscovery }>

export function parseInputContract(value: Record<string, unknown>): AgentInputContract {
  if (value["inputKind"] === "text" && !Object.hasOwn(value, "inputSchema")) return { inputKind: "text" }
  const raw = value["inputSchema"]
  if (value["inputKind"] !== "json" || !raw || typeof raw !== "object" || Array.isArray(raw)) throw new Error("Invalid agent input contract")
  const schema = raw as Record<string, unknown>
  if (schema["status"] === "unavailable" && Object.keys(schema).length === 2 && ["missing_converter", "conversion_failed", "invalid_schema", "schema_too_large"].includes(schema["reason"] as string)) {
    return { inputKind: "json", inputSchema: { status: "unavailable", reason: schema["reason"] as Extract<InputSchemaDiscovery, {status: "unavailable"}>["reason"] } }
  }
  const document = schema["schema"]
  if (schema["status"] !== "available" || Object.keys(schema).length !== 2 || (typeof document !== "boolean" && (!document || typeof document !== "object" || Array.isArray(document)))) throw new Error("Invalid input JSON Schema")
  const bytes = canonicalizeJsonValue(document as JsonValue)
  if (bytes.byteLength > 64 * 1024) throw new Error("Input JSON Schema exceeds 64 KiB")
  return { inputKind: "json", inputSchema: { status: "available", schema: JSON.parse(new TextDecoder().decode(bytes)) as JsonValue } }
}
