import type { StandardSchemaV1 } from "../../../sdk/typescript/src/agent"
import { requireJson } from "../../../sdk/typescript/src/agent"
import type { JsonValue } from "../../../sdk/typescript/src/contract"

export class ValidationFailure extends Error {
  readonly code: "input_invalid" | "result_invalid"
  constructor(code: "input_invalid" | "result_invalid", issues: readonly StandardSchemaV1.Issue[]) {
    // Diagnostics are visible to authorized readers and may contain authored data.
    super(issues.slice(0, 16).map(issue => {
      const path = issue.path?.slice(0, 16).map(part => String(typeof part === "object" ? part.key : part).slice(0, 80)).join(".")
      return `${path ? `${path}: ` : ""}${issue.message.slice(0, 512)}`
    }).join("; ") || code)
    this.code = code
    this.name = "ValidationFailure"
  }
}

export async function validate(schema: StandardSchemaV1 | undefined, value: unknown, code: "input_invalid" | "result_invalid", signal: AbortSignal): Promise<unknown> {
  signal.throwIfAborted()
  if (!schema) return value
  // The caller tracks and joins this Promise on cancellation. Standard Schema
  // validators do not have a cancellation contract; dropping the await is unsafe.
  const result = await schema["~standard"].validate(value)
  signal.throwIfAborted()
  if (result.issues) throw new ValidationFailure(code, result.issues)
  return result.value
}

export function validatedResult(value: unknown): JsonValue {
  try { requireJson(value) }
  catch { throw new ValidationFailure("result_invalid", [{ message: "Result must be finite JSON with ordinary data properties and valid Unicode" }]) }
  return structuredClone(value)
}
